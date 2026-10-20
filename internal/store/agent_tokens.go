package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// Scopes of a service_tokens row. A kyPulse token is pulse; an AI agent (MCP) token is one of
// the two mcp scopes, which the organization administrator picks and may change later.
const (
	ScopePulse      = "pulse"
	ScopeMCPRead    = "mcp:read"
	ScopeMCPOperate = "mcp:operate"
)

// IsAgentScope reports whether scope is an AI agent token's.
func IsAgentScope(scope string) bool { return scope == ScopeMCPRead || scope == ScopeMCPOperate }

const (
	// AgentTokenPrefix marks an agent token, so a secret scanner and a reader both know it for
	// one, and so it can never parse as a kyPulse token (64 bare hex characters).
	AgentTokenPrefix = "kyy_mcp_"
	// AgentIdleLimit is how long an agent token may go unused before it is revoked. A constant,
	// not a setting, as in KyCalendar.
	AgentIdleLimit = 90 * 24 * time.Hour
	// maxAgentTokens bounds an organization's live agent tokens.
	maxAgentTokens = 20
)

// ErrAgentTokenLimit refuses a token past maxAgentTokens live ones.
var ErrAgentTokenLimit = errors.New("too many agent tokens")

// AgentTokenIssue is a created agent token: the bearer, once.
type AgentTokenIssue struct {
	ServiceToken
	Token string `json:"token"`
}

// AgentLastActive is when tok was last used, or created if never used: the idle clock.
func AgentLastActive(tok *ServiceToken) time.Time {
	if tok.LastUsedAt != nil {
		return *tok.LastUsedAt
	}
	return tok.CreatedAt
}

func validAgentName(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= 64 && strings.TrimSpace(name) == name && validTenantName(name)
}

// CreateAgentToken mints an agent token of scope for a.OrganizationID. Only its SHA-256 is
// stored; the bearer is in the result and nowhere else.
func (t *tenancyStore) CreateAgentToken(ctx context.Context, a TenantAccess, name, scope string) (*AgentTokenIssue, error) {
	if !validAgentName(name) || !IsAgentScope(scope) {
		return nil, ErrInvalid
	}
	secret := make([]byte, serviceTokenBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	issue := &AgentTokenIssue{ServiceToken: ServiceToken{ID: "agt_" + crypto.RandomHex(12), OrganizationID: a.OrganizationID, Name: name, CreatedBy: a.Principal(), CreatedAt: now, Scope: scope}, Token: AgentTokenPrefix + hex.EncodeToString(secret)}
	details := "scope=" + scope
	err := t.withTenantTargetDetails(ctx, a, permissions.AgentTokensManage, issue.ID, &details, func(tx *sql.Tx) error {
		var live int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM service_tokens WHERE organization_id=? AND scope<>? AND revoked_at IS NULL`), a.OrganizationID, ScopePulse).Scan(&live); err != nil {
			return err
		}
		if live >= maxAgentTokens {
			return ErrAgentTokenLimit
		}
		_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO service_tokens (id,organization_id,name,token_hash,created_by,created_at,scope) VALUES (?,?,?,?,?,?,?)`), issue.ID, a.OrganizationID, name, crypto.SHA256Hex(secret), issue.CreatedBy, now, scope)
		return err
	})
	if err != nil {
		return nil, err
	}
	return issue, nil
}

// ListAgentTokens lists the organization's live agent tokens, newest first. A revoked token
// leaves the list; its creation and revocation stay in the audit trail.
func (t *tenancyStore) ListAgentTokens(ctx context.Context, a TenantAccess) ([]ServiceToken, error) {
	out := []ServiceToken{}
	err := t.readTenant(ctx, a, permissions.AgentTokensManage, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,organization_id,name,created_by,created_at,last_used_at,last_ip,scope FROM service_tokens WHERE organization_id=? AND scope<>? AND revoked_at IS NULL ORDER BY created_at DESC, id LIMIT 200`), a.OrganizationID, ScopePulse)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tok ServiceToken
			var lastUsed sql.NullTime
			if err := rows.Scan(&tok.ID, &tok.OrganizationID, &tok.Name, &tok.CreatedBy, &tok.CreatedAt, &lastUsed, &tok.LastIP, &tok.Scope); err != nil {
				return err
			}
			if lastUsed.Valid {
				tok.LastUsedAt = &lastUsed.Time
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

// SetAgentTokenScope moves a live agent token between the two agent scopes. A kyPulse token is
// not an agent token and answers ErrNotFound, as does another organization's.
func (t *tenancyStore) SetAgentTokenScope(ctx context.Context, a TenantAccess, id, scope string) error {
	if !IsAgentScope(scope) {
		return ErrInvalid
	}
	details := "scope=" + scope
	return t.withTenantTargetDetails(ctx, a, permissions.AgentTokensManage, id, &details, func(tx *sql.Tx) error {
		var from string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT scope FROM service_tokens WHERE id=? AND organization_id=? AND scope<>? AND revoked_at IS NULL`), id, a.OrganizationID, ScopePulse).Scan(&from)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		details = "from=" + from + " to=" + scope
		_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE service_tokens SET scope=? WHERE id=? AND organization_id=? AND revoked_at IS NULL`), scope, id, a.OrganizationID)
		return err
	})
}

// RevokeAgentToken revokes a live agent token of a.OrganizationID; a kyPulse token is
// ErrNotFound here and is revoked through RevokeServiceToken.
func (t *tenancyStore) RevokeAgentToken(ctx context.Context, a TenantAccess, id string) error {
	now := time.Now().UTC()
	return t.withTenantTarget(ctx, a, permissions.AgentTokensManage, id, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE service_tokens SET revoked_at=?, revoked_by=? WHERE id=? AND organization_id=? AND scope<>? AND revoked_at IS NULL`), now, a.Principal(), id, a.OrganizationID, ScopePulse)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// LookupAgentToken resolves an agent bearer to its live token without writing. A malformed,
// unknown, revoked, kyPulse or idle token is ErrForbidden alike; an idle one is revoked on the
// spot, so the 90-day limit holds whether or not the sweep has run.
func (t *tenancyStore) LookupAgentToken(ctx context.Context, token string, now time.Time) (*ServiceToken, error) {
	hexPart, ok := strings.CutPrefix(token, AgentTokenPrefix)
	raw, err := hex.DecodeString(hexPart)
	if !ok || err != nil || len(raw) != serviceTokenBytes {
		return nil, ErrForbidden
	}
	var tok ServiceToken
	var lastUsed sql.NullTime
	err = t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT id,organization_id,name,created_by,created_at,last_used_at,last_ip,scope FROM service_tokens WHERE token_hash=? AND scope<>? AND revoked_at IS NULL`), crypto.SHA256Hex(raw), ScopePulse).Scan(&tok.ID, &tok.OrganizationID, &tok.Name, &tok.CreatedBy, &tok.CreatedAt, &lastUsed, &tok.LastIP, &tok.Scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	if lastUsed.Valid {
		tok.LastUsedAt = &lastUsed.Time
	}
	if now.Sub(AgentLastActive(&tok)) > AgentIdleLimit {
		if _, err := t.ExpireIdleAgentTokens(ctx, now, tok.ID); err != nil {
			return nil, err
		}
		return nil, ErrForbidden
	}
	return &tok, nil
}

// AuthenticateAgentToken is LookupAgentToken for a request: it also stamps last_used_at and
// last_ip, at most once per lastUsedEvery, which restarts the idle clock.
func (t *tenancyStore) AuthenticateAgentToken(ctx context.Context, token, ip string, now time.Time) (*ServiceToken, error) {
	tok, err := t.LookupAgentToken(ctx, token, now)
	if err != nil {
		return nil, err
	}
	if tok.LastUsedAt == nil || now.Sub(*tok.LastUsedAt) > lastUsedEvery {
		if _, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE service_tokens SET last_used_at=?, last_ip=? WHERE id=? AND revoked_at IS NULL`), now.UTC(), ip, tok.ID); err != nil {
			return nil, err
		}
		at := now.UTC()
		tok.LastUsedAt, tok.LastIP = &at, ip
	}
	return tok, nil
}

// ExpireIdleAgentTokens revokes agent tokens unused for AgentIdleLimit before now, only id when
// it is set, and writes one agent_token.expired row per token in its organization. The API's
// hourly loop sweeps the ones nobody presents.
func (t *tenancyStore) ExpireIdleAgentTokens(ctx context.Context, now time.Time, id string) ([]ServiceToken, error) {
	cutoff := now.UTC().Add(-AgentIdleLimit)
	tx, err := t.store.beginTx(ctx, true)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,organization_id FROM service_tokens WHERE scope<>? AND revoked_at IS NULL AND COALESCE(last_used_at,created_at)<? AND (?='' OR id=?) LIMIT 500`), ScopePulse, cutoff, id, id)
	if err != nil {
		return nil, err
	}
	var gone []ServiceToken
	for rows.Next() {
		var tok ServiceToken
		if err := rows.Scan(&tok.ID, &tok.OrganizationID); err != nil {
			rows.Close()
			return nil, err
		}
		gone = append(gone, tok)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, tok := range gone {
		// The conditional update makes a concurrent expiry of the same token write one row.
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE service_tokens SET revoked_at=?, revoked_by=? WHERE id=? AND revoked_at IS NULL`), now.UTC(), "idle", tok.ID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), "service:"+tok.ID, "agent_token.expired", tok.ID, "unused for 90 days", "", now.UTC(), "organization", tok.OrganizationID, "", uuid.NewString(), "success"); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return gone, nil
}

// RecordAgentAction writes the success row for one agent action, beside the row the operation's
// own permission wrote, so the trail names the tool. details never carries log text or values.
func (t *tenancyStore) RecordAgentAction(ctx context.Context, a TenantAccess, action, resource, details string) error {
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
	return t.store.Audit().LogAudit(ctx, &AuditRecord{UserID: a.Principal(), Action: action, Resource: resource, Details: details, IPAddress: a.IPAddress, Scope: "organization", OrganizationID: a.OrganizationID, EnvironmentID: a.EnvironmentID, CorrelationID: a.CorrelationID, Result: "success", CreatedAt: time.Now().UTC()})
}
