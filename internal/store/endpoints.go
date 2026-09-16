package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/crypto"
	"github.com/Busness-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

const (
	enrollmentTokenLife = 15 * time.Minute
	pendingEndpointLife = 24 * time.Hour
	maxFactsBytes       = 4096
)

var factKeys = map[string]bool{"hostname": true, "os": true, "runtime_version": true, "cpus": true, "memory_bytes": true}

func validRuntime(r string) bool { return r == "docker" || r == "kubernetes" }

// CreateEnrollmentToken records the image reference the operator is handed with the token, so
// the audit trail names the bytes that were authorized to run as root on the host.
func (t *tenancyStore) CreateEnrollmentToken(ctx context.Context, a TenantAccess, runtime, agentImage string) (*EnrollmentToken, error) {
	if a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	secret := make([]byte, protocol.TokenSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tok := &EnrollmentToken{ID: uuid.NewString(), EnvironmentID: a.EnvironmentID, Runtime: runtime, ExpiresAt: now.Add(enrollmentTokenLife), Secret: secret, AgentImage: agentImage}
	err := t.withTenantTarget(ctx, a, permissions.EndpointEnroll, tok.ID, func(tx *sql.Tx) error {
		if !validRuntime(runtime) {
			return ErrInvalid
		}
		_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO agent_enrollment_tokens (id,organization_id,environment_id,runtime,token_hash,created_by,created_at,expires_at,agent_image) VALUES (?,?,?,?,?,?,?,?,?)`), tok.ID, a.OrganizationID, a.EnvironmentID, runtime, crypto.SHA256Hex(secret), a.ActorID, now, tok.ExpiresAt, agentImage)
		return err
	})
	if err != nil {
		return nil, err
	}
	return tok, nil
}

// Enroll consumes exactly one token per call under concurrency: the conditional UPDATE is the
// lock. Every refusal is ErrForbidden so the agent learns nothing about why.
func (t *tenancyStore) Enroll(ctx context.Context, req EnrollmentRequest) (*Endpoint, error) {
	if !protocol.VerifyEnrollment(req.PublicKey, req.Token, req.Proof) || !validTenantName(req.Name) {
		return nil, ErrForbidden
	}
	// Facts share the review surface with the fingerprint, so values meet the same rule as names.
	facts := map[string]string{}
	for k, v := range req.Facts {
		if !factKeys[k] {
			continue
		}
		if !displaySafe(v) {
			return nil, ErrForbidden
		}
		facts[k] = v
	}
	factsJSON, err := json.Marshal(facts)
	if err != nil || len(factsJSON) > maxFactsBytes {
		return nil, ErrForbidden
	}
	now := time.Now().UTC()
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE agent_enrollment_tokens SET consumed_at=? WHERE token_hash=? AND consumed_at IS NULL AND expires_at>?`), now, crypto.SHA256Hex(req.Token), now)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return nil, ErrForbidden
	}
	e := &Endpoint{ID: "ep_" + crypto.RandomHex(12), Name: strings.TrimSpace(req.Name), State: "pending", Facts: facts, Fingerprint: protocol.Fingerprint(req.PublicKey), CreatedAt: now}
	var tokenID string
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,organization_id,environment_id,runtime FROM agent_enrollment_tokens WHERE token_hash=?`), crypto.SHA256Hex(req.Token)).Scan(&tokenID, &e.OrganizationID, &e.EnvironmentID, &e.Runtime); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoints (id,organization_id,environment_id,name,runtime,state,facts,created_at) VALUES (?,?,?,?,?,?,?,?)`), e.ID, e.OrganizationID, e.EnvironmentID, e.Name, e.Runtime, e.State, string(factsJSON), now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return nil, ErrAlreadyExists
		}
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_keys (endpoint_id,fingerprint,public_key,state,created_at) VALUES (?,?,?,'pending_review',?)`), e.ID, e.Fingerprint, hex.EncodeToString(req.PublicKey), now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE agent_enrollment_tokens SET endpoint_id=? WHERE id=?`), e.ID, tokenID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), "agent:"+e.ID, "agent.enroll", e.ID, req.IPAddress, now, "organization", e.OrganizationID, e.EnvironmentID, uuid.NewString(), "success"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return e, nil
}

const endpointColumns = `e.id,e.organization_id,e.environment_id,e.name,e.runtime,e.state,e.facts,e.created_at,e.approved_at,e.approved_by,e.revoked_at,e.last_seen_at,COALESCE((SELECT k.fingerprint FROM endpoint_keys k WHERE k.endpoint_id=e.id AND k.state IN ('approved','pending_review') ORDER BY k.state LIMIT 1),''),COALESCE((SELECT k.fingerprint FROM endpoint_keys k WHERE k.endpoint_id=e.id AND k.state='pending_review' AND e.state<>'pending' AND k.created_at>? ORDER BY k.created_at DESC LIMIT 1),'')`

// pendingCutoff is the oldest creation time a key may have and still count as pending.
func pendingCutoff() time.Time { return time.Now().UTC().Add(-pendingKeyLife) }

func scanEndpoint(row interface{ Scan(...any) error }) (*Endpoint, error) {
	var e Endpoint
	var facts string
	if err := row.Scan(&e.ID, &e.OrganizationID, &e.EnvironmentID, &e.Name, &e.Runtime, &e.State, &facts, &e.CreatedAt, &e.ApprovedAt, &e.ApprovedBy, &e.RevokedAt, &e.LastSeenAt, &e.Fingerprint, &e.PendingFingerprint); err != nil {
		return nil, err
	}
	e.Capabilities = []string{}
	e.Alerts = []EndpointEvent{}
	e.Facts = map[string]string{}
	_ = json.Unmarshal([]byte(facts), &e.Facts)
	// A pending enrollment nobody reviewed in time is shown as expired and cannot be approved.
	if e.State == "pending" && time.Since(e.CreatedAt) > pendingEndpointLife {
		e.State = "expired"
	}
	return &e, nil
}

// decorate loads capabilities and unacknowledged high-severity events (bounded) for the UI.
func (t *tenancyStore) decorate(ctx context.Context, tx *sql.Tx, e *Endpoint) error {
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT capability FROM endpoint_capabilities WHERE endpoint_id=? ORDER BY capability LIMIT ?`), e.ID, MaxCapabilities)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return err
		}
		e.Capabilities = append(e.Capabilities, c)
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, t.store.rebind(`SELECT id,severity,kind,details,created_at FROM endpoint_events WHERE endpoint_id=? AND severity='high' AND acknowledged_at IS NULL ORDER BY created_at DESC LIMIT 10`), e.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ev EndpointEvent
		if err := rows.Scan(&ev.ID, &ev.Severity, &ev.Kind, &ev.Details, &ev.CreatedAt); err != nil {
			return err
		}
		e.Alerts = append(e.Alerts, ev)
	}
	return rows.Err()
}

func (t *tenancyStore) ListEndpoints(ctx context.Context, a TenantAccess, offset, limit int) ([]Endpoint, error) {
	result := []Endpoint{}
	err := t.readTenant(ctx, a, permissions.EndpointRead, func(tx *sql.Tx) error {
		if offset < 0 || limit < 1 || limit > 200 {
			return ErrInvalid
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT `+endpointColumns+` FROM endpoints e WHERE e.organization_id=? AND (?='' OR e.environment_id=?) ORDER BY e.name,e.id LIMIT ? OFFSET ?`), pendingCutoff(), a.OrganizationID, a.EnvironmentID, a.EnvironmentID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEndpoint(rows)
			if err != nil {
				return err
			}
			result = append(result, *e)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for i := range result {
			if err := t.decorate(ctx, tx, &result[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (t *tenancyStore) ReadEndpoint(ctx context.Context, a TenantAccess, id string) (*Endpoint, error) {
	var e *Endpoint
	err := t.readTenant(ctx, a, permissions.EndpointRead, func(tx *sql.Tx) error {
		var err error
		e, err = scanEndpoint(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+endpointColumns+` FROM endpoints e WHERE e.organization_id=? AND e.id=?`), pendingCutoff(), a.OrganizationID, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return t.decorate(ctx, tx, e)
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// ApproveEndpoint binds the fingerprint the administrator reviewed: a different one, or a
// pending enrollment older than its life, is refused rather than silently approved.
func (t *tenancyStore) ApproveEndpoint(ctx context.Context, a TenantAccess, id, fingerprint string) error {
	return t.withTenantTarget(ctx, a, permissions.EndpointEnroll, id, func(tx *sql.Tx) error {
		var created time.Time
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT created_at FROM endpoints WHERE organization_id=? AND id=? AND state='pending'`), a.OrganizationID, id).Scan(&created)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if time.Since(created) > pendingEndpointLife {
			return ErrInvalid
		}
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_keys SET state='approved',acknowledged_at=? WHERE endpoint_id=? AND fingerprint=? AND state='pending_review'`), now, id, fingerprint)
		if err := tenantChangeResult(result, err); err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrInvalid
			}
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET state='approved',approved_at=?,approved_by=? WHERE id=?`), now, a.ActorID, id)
		return err
	})
}

func (t *tenancyStore) RejectEndpoint(ctx context.Context, a TenantAccess, id string) error {
	return t.withTenantTarget(ctx, a, permissions.EndpointEnroll, id, func(tx *sql.Tx) error {
		return t.terminate(ctx, tx, a.OrganizationID, id, "state='pending'")
	})
}

func (t *tenancyStore) RevokeEndpoint(ctx context.Context, a TenantAccess, id string) error {
	return t.withTenantTarget(ctx, a, permissions.EndpointRevoke, id, func(tx *sql.Tx) error {
		return t.terminate(ctx, tx, a.OrganizationID, id, "state<>'revoked'")
	})
}

// terminate is the one transition into the terminal state; keys retire with it.
func (t *tenancyStore) terminate(ctx context.Context, tx *sql.Tx, org, id, condition string) error {
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET state='revoked',revoked_at=? WHERE organization_id=? AND id=? AND `+condition), now, org, id)
	if err := tenantChangeResult(result, err); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_keys SET state='retired',retired_at=? WHERE endpoint_id=? AND state<>'retired'`), now, id)
	return err
}

func (t *tenancyStore) RenameEndpoint(ctx context.Context, a TenantAccess, id, name string) error {
	return t.withTenantTarget(ctx, a, permissions.EndpointUpdate, id, func(tx *sql.Tx) error {
		if !validTenantName(name) {
			return ErrInvalid
		}
		result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET name=? WHERE organization_id=? AND id=?`), strings.TrimSpace(name), a.OrganizationID, id)
		return tenantChangeResult(result, err)
	})
}
