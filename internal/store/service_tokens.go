package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

const (
	serviceTokenBytes = 32
	// lastUsedEvery bounds the write a read costs: last_used_at and last_ip are stamped at
	// most once a minute per token, or when the address changes.
	lastUsedEvery = time.Minute
)

// pairingLife is a variable so a test can mint an already-expired code.
var pairingLife = 15 * time.Minute

// pairingCode draws six digits; a test may pin it.
var pairingCode = func() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

var serviceNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func validServiceName(s string) bool { return serviceNameRe.MatchString(s) }

// maxPairingCodeDraws bounds the redraw loop. pairingLockKey serialises the check-then-insert
// across organizations on PostgreSQL (fix round 2: the check alone is not a critical section
// under concurrent mints — two transactions can both see no live row for the same hash and
// both insert it).
const (
	maxPairingCodeDraws       = 20
	pairingLockKey      int64 = 7345102
)

func (t *tenancyStore) CreateServicePairing(ctx context.Context, a TenantAccess) (*ServicePairing, error) {
	now := time.Now().UTC()
	p := &ServicePairing{ID: uuid.NewString(), ExpiresAt: now.Add(pairingLife)}
	err := t.withTenantTarget(ctx, a, permissions.ServiceTokensManage, p.ID, func(tx *sql.Tx) error {
		if t.store.driver == "postgres" {
			// Transaction-scoped: serialises pairing mints across organizations so the
			// collision check and the insert are one critical section. SQLite needs none
			// because a write transaction holds the database lock.
			if _, err := tx.ExecContext(ctx, t.store.rebind(`SELECT pg_advisory_xact_lock(?)`), pairingLockKey); err != nil {
				return err
			}
		}
		// Expired, unconsumed rows must not keep their codes reserved.
		if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM service_token_pairings WHERE expires_at < ?`), now); err != nil {
			return err
		}
		// The lock (postgres) or the single SQLite writer makes this check-then-insert safe:
		// a collision here means another organization's code is genuinely still live, so
		// redraw rather than letting "oldest wins" hand one organization's claim to
		// another's token (docs/authorization-matrix.md).
		for i := 0; i < maxPairingCodeDraws; i++ {
			code, err := pairingCode()
			if err != nil {
				return err
			}
			hash := crypto.SHA256Hex([]byte(code))
			var exists int
			err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT 1 FROM service_token_pairings WHERE code_hash=? AND consumed_at IS NULL AND expires_at>?`), hash, now).Scan(&exists)
			if err == nil {
				continue // another organization's code is still live: redraw
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO service_token_pairings (id,organization_id,code_hash,created_by,created_at,expires_at) VALUES (?,?,?,?,?,?)`), p.ID, a.OrganizationID, hash, a.ActorID, now, p.ExpiresAt); err != nil {
				return err
			}
			p.Code = code
			return nil
		}
		return ErrPairingCodesExhausted
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ClaimServiceToken consumes exactly one live pairing and mints that organization's token.
// CreateServicePairing redraws on a collision, so at most one organization ever holds a given
// code live at once; the ORDER BY is a tie-breaker, not a selection rule. Every refusal is
// ErrForbidden: the caller is unauthenticated and learns nothing about why. A code that names
// no live pairing writes no tenant row, because there is no organization to attribute it to;
// the API rate-limits and logs the attempt.
func (t *tenancyStore) ClaimServiceToken(ctx context.Context, code, serviceName, ip string) (*ServiceTokenIssue, error) {
	if len(code) != 6 || !validServiceName(serviceName) {
		return nil, ErrForbidden
	}
	now := time.Now().UTC()
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var pairingID, orgID, orgName string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT p.id,p.organization_id,o.name FROM service_token_pairings p JOIN organizations o ON o.id=p.organization_id WHERE p.code_hash=? AND p.consumed_at IS NULL AND p.expires_at>? ORDER BY p.created_at,p.id LIMIT 1`), crypto.SHA256Hex([]byte(code)), now).Scan(&pairingID, &orgID, &orgName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	// The conditional UPDATE is the lock: two claims of one code commit one token.
	res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE service_token_pairings SET consumed_at=? WHERE id=? AND consumed_at IS NULL`), now, pairingID)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, ErrForbidden
	}
	secret := make([]byte, serviceTokenBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	id := "svc_" + crypto.RandomHex(12)
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO service_tokens (id,organization_id,name,token_hash,created_by,created_at) VALUES (?,?,?,?,?,?)`), id, orgID, serviceName, crypto.SHA256Hex(secret), "pairing:"+pairingID, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), "service:"+id, "service_token.claim", id, "service="+serviceName, ip, now, "organization", orgID, "", uuid.NewString(), "success"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ServiceTokenIssue{Token: hex.EncodeToString(secret), Organization: Organization{ID: orgID, Name: orgName}}, nil
}

// AuthenticateServiceToken resolves a bearer to its live token. A revoked or unknown token is
// ErrForbidden; the API answers 401 either way.
func (t *tenancyStore) AuthenticateServiceToken(ctx context.Context, token, ip string) (*ServiceToken, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(token))
	if err != nil || len(raw) != serviceTokenBytes {
		return nil, ErrForbidden
	}
	var tok ServiceToken
	var lastUsed, revoked sql.NullTime
	err = t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT id,organization_id,name,created_by,created_at,last_used_at,last_ip,revoked_at FROM service_tokens WHERE token_hash=?`), crypto.SHA256Hex(raw)).Scan(&tok.ID, &tok.OrganizationID, &tok.Name, &tok.CreatedBy, &tok.CreatedAt, &lastUsed, &tok.LastIP, &revoked)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked.Valid) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if !lastUsed.Valid || now.Sub(lastUsed.Time) > lastUsedEvery || tok.LastIP != ip {
		if _, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE service_tokens SET last_used_at=?, last_ip=? WHERE id=? AND revoked_at IS NULL`), now, ip, tok.ID); err != nil {
			return nil, err
		}
		lastUsed = sql.NullTime{Time: now, Valid: true}
		tok.LastIP = ip
	}
	tok.LastUsedAt = &lastUsed.Time
	return &tok, nil
}

func (t *tenancyStore) ListServiceTokens(ctx context.Context, a TenantAccess) ([]ServiceToken, error) {
	out := []ServiceToken{}
	err := t.readTenant(ctx, a, permissions.ServiceTokensManage, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,organization_id,name,created_by,created_at,last_used_at,last_ip,revoked_at FROM service_tokens WHERE organization_id=? ORDER BY created_at DESC LIMIT 200`), a.OrganizationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tok ServiceToken
			var lastUsed, revoked sql.NullTime
			if err := rows.Scan(&tok.ID, &tok.OrganizationID, &tok.Name, &tok.CreatedBy, &tok.CreatedAt, &lastUsed, &tok.LastIP, &revoked); err != nil {
				return err
			}
			if lastUsed.Valid {
				tok.LastUsedAt = &lastUsed.Time
			}
			if revoked.Valid {
				tok.RevokedAt = &revoked.Time
			}
			out = append(out, tok)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (t *tenancyStore) RevokeServiceToken(ctx context.Context, a TenantAccess, id string) error {
	now := time.Now().UTC()
	return t.withTenantTarget(ctx, a, permissions.ServiceTokensManage, id, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE service_tokens SET revoked_at=?, revoked_by=? WHERE id=? AND organization_id=? AND revoked_at IS NULL`), now, a.ActorID, id, a.OrganizationID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// tokenInOrganization reports whether tokenID names a service_tokens row of organizationID,
// so a caller cannot plant an audit row in an organization its token does not belong to.
func (t *tenancyStore) tokenInOrganization(ctx context.Context, tokenID, organizationID string) (bool, error) {
	var exists int
	err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT 1 FROM service_tokens WHERE id=? AND organization_id=?`), tokenID, organizationID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// DenyService writes a denied audit row for an API-level refusal. a.ServiceTokenID must name
// a token of a.OrganizationID, or the caller could plant a row in an organization it holds no
// token for; that case is ErrForbidden and writes nothing.
func (t *tenancyStore) DenyService(ctx context.Context, a TenantAccess, action permissions.Action, detail string) error {
	if a.ServiceTokenID == "" {
		return ErrForbidden
	}
	ok, err := t.tokenInOrganization(ctx, a.ServiceTokenID, a.OrganizationID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return t.store.Audit().LogAudit(ctx, &AuditRecord{UserID: a.actor(), Action: string(action), Resource: a.OrganizationID, Details: detail, IPAddress: a.IPAddress, Scope: "organization", OrganizationID: a.OrganizationID, EnvironmentID: a.EnvironmentID, CorrelationID: a.CorrelationID, Result: "denied", CreatedAt: time.Now().UTC()})
}

// RecordServiceTokenReads writes the hourly summary row for one token, only when tokenID
// names a token of organizationID. A stale counter for a token that has moved on (revoked,
// or never that organization's) is not an error: it writes nothing.
func (t *tenancyStore) RecordServiceTokenReads(ctx context.Context, organizationID, tokenID string, reads int) error {
	if tokenID == "" {
		return nil
	}
	ok, err := t.tokenInOrganization(ctx, tokenID, organizationID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return t.store.Audit().LogAudit(ctx, &AuditRecord{UserID: "service:" + tokenID, Action: "service_token.reads", Resource: tokenID, Details: fmt.Sprintf("reads=%d", reads), Scope: "organization", OrganizationID: organizationID, CorrelationID: uuid.NewString(), Result: "success", CreatedAt: time.Now().UTC()})
}
