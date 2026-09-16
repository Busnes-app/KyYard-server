package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

const pendingKeyLife = 7 * 24 * time.Hour

// RotateEndpointKey records a key the agent minted as pending_review. It does not authenticate
// until an operator acknowledges it; only one may be pending, and rotation is refused while an
// unacknowledged duplicate connection stands (docs/agent-protocol.md section 3 step 4).
func (t *tenancyStore) RotateEndpointKey(ctx context.Context, endpointID string, newPublicKey, signature []byte, ip string) (string, error) {
	if len(newPublicKey) != 32 {
		return "", ErrInvalid
	}
	now := time.Now().UTC()
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	e, err := scanEndpoint(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+endpointColumns+` FROM endpoints e WHERE e.id=?`), endpointID))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (e.State == "pending" || e.State == "revoked" || e.State == "expired")) {
		return "", ErrForbidden
	}
	if err != nil {
		return "", err
	}
	var currentHex string
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT public_key FROM endpoint_keys WHERE endpoint_id=? AND state='approved'`), endpointID).Scan(&currentHex); err != nil {
		return "", ErrForbidden
	}
	current, _ := hex.DecodeString(currentHex)
	oldFP := sha256.Sum256(current)
	if !protocol.VerifyRotation(current, oldFP[:], newPublicKey, signature) {
		return "", ErrForbidden
	}
	var blocked int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM endpoint_events WHERE endpoint_id=? AND kind='duplicate_connection' AND acknowledged_at IS NULL`), endpointID).Scan(&blocked); err != nil {
		return "", err
	}
	if blocked > 0 {
		return "", ErrRotationBlocked
	}
	var pending int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM endpoint_keys WHERE endpoint_id=? AND state='pending_review' AND created_at>?`), endpointID, now.Add(-pendingKeyLife)).Scan(&pending); err != nil {
		return "", err
	}
	if pending > 0 {
		return "", ErrRotationPending
	}
	// An expired pending key is retired now so the fingerprint can be reused if the agent retries.
	if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_keys SET state='retired',retired_at=? WHERE endpoint_id=? AND state='pending_review'`), now, endpointID); err != nil {
		return "", err
	}
	newFP := protocol.Fingerprint(newPublicKey)
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_keys (endpoint_id,fingerprint,public_key,state,created_at) VALUES (?,?,?,'pending_review',?)`), endpointID, newFP, hex.EncodeToString(newPublicKey), now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return "", ErrAlreadyExists
		}
		return "", err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_events (endpoint_id,organization_id,environment_id,severity,kind,details,created_at) VALUES (?,?,?,'high','rotation_pending',?,?)`), endpointID, e.OrganizationID, e.EnvironmentID, "approved="+hex.EncodeToString(oldFP[:])+" pending="+newFP, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), "agent:"+endpointID, "endpoint.rotate", endpointID, "approved="+hex.EncodeToString(oldFP[:])+" pending="+newFP, ip, now, "organization", e.OrganizationID, e.EnvironmentID, uuid.NewString(), "success"); err != nil {
		return "", err
	}
	return newFP, tx.Commit()
}

// AcknowledgeEndpointKey is the human step: the named pending key becomes the only approved
// key, everything else retires, and the rotation event is acknowledged.
func (t *tenancyStore) AcknowledgeEndpointKey(ctx context.Context, a TenantAccess, endpointID, fingerprint string) error {
	return t.withTenantTarget(ctx, a, permissions.EndpointEnroll, endpointID, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_keys SET state='approved',acknowledged_at=? WHERE endpoint_id=? AND fingerprint=? AND state='pending_review' AND created_at>? AND EXISTS (SELECT 1 FROM endpoints WHERE id=? AND organization_id=? AND state IN ('approved','active','offline'))`), now, endpointID, fingerprint, now.Add(-pendingKeyLife), endpointID, a.OrganizationID)
		if err := tenantChangeResult(result, err); err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrInvalid
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_keys SET state='retired',retired_at=? WHERE endpoint_id=? AND fingerprint<>? AND state<>'retired'`), now, endpointID, fingerprint); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_events SET acknowledged_at=? WHERE endpoint_id=? AND kind='rotation_pending' AND acknowledged_at IS NULL`), now, endpointID)
		return err
	})
}

func (t *tenancyStore) RecordEndpointEvent(ctx context.Context, e *Endpoint, severity, kind, details string) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_events (endpoint_id,organization_id,environment_id,severity,kind,details,created_at) VALUES (?,?,?,?,?,?,?)`), e.ID, e.OrganizationID, e.EnvironmentID, severity, kind, details, time.Now().UTC())
	return err
}

func (t *tenancyStore) AcknowledgeEndpointEvent(ctx context.Context, a TenantAccess, endpointID string, eventID int64) error {
	return t.withTenantTarget(ctx, a, permissions.EndpointEnroll, endpointID, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_events SET acknowledged_at=? WHERE id=? AND endpoint_id=? AND organization_id=? AND acknowledged_at IS NULL`), time.Now().UTC(), eventID, endpointID, a.OrganizationID)
		return tenantChangeResult(result, err)
	})
}

// SetEndpointCapabilities replaces the recorded set from the agent's hello.
func (t *tenancyStore) SetEndpointCapabilities(ctx context.Context, endpointID string, capabilities []string) error {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM endpoint_capabilities WHERE endpoint_id=?`), endpointID); err != nil {
		return err
	}
	for _, c := range capabilities {
		if !displaySafe(c) || c == "" {
			return ErrInvalid
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_capabilities (endpoint_id,capability) VALUES (?,?)`), endpointID, c); err != nil {
			return err
		}
	}
	return tx.Commit()
}
