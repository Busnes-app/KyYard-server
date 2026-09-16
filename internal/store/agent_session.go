package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
)

// AgentIdentity is what the connection handshake needs: the endpoint and the key that may
// authenticate it. Pending endpoints authenticate with their key under review so they can
// receive the approval notice; nothing else is granted to them.
type AgentIdentity struct {
	Endpoint  Endpoint
	PublicKey []byte
}

// Sentinel errors let the handshake tell a retired key (the agent should switch to its
// acknowledged one) from a key still under review (the agent must keep using its old one).
var (
	ErrKeyRetired       = errors.New("key retired")
	ErrKeyPendingReview = errors.New("key pending review")
)

func (t *tenancyStore) AgentIdentity(ctx context.Context, endpointID, fingerprint string) (*AgentIdentity, error) {
	e, err := scanEndpoint(t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT `+endpointColumns+` FROM endpoints e WHERE e.id=?`), endpointID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if e.State == "revoked" || e.State == "expired" {
		return nil, ErrForbidden
	}
	var keyHex, state string
	var created time.Time
	err = t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT public_key,state,created_at FROM endpoint_keys WHERE endpoint_id=? AND fingerprint=?`), endpointID, fingerprint).Scan(&keyHex, &state, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	switch {
	case e.State == "pending" && state == "pending_review":
		// The enrolled key authenticates the restricted channel until approval.
	case state == "approved":
	case state == "pending_review" && time.Since(created) <= pendingKeyLife:
		return nil, ErrKeyPendingReview
	case state == "retired" || state == "pending_review":
		return nil, ErrKeyRetired
	default:
		return nil, ErrForbidden
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}
	return &AgentIdentity{Endpoint: *e, PublicKey: key}, nil
}

// ReadEndpointRaw is a trusted helper for the handshake's own audit row; it is not authorized.
func (t *tenancyStore) ReadEndpointRaw(ctx context.Context, endpointID string) (*Endpoint, error) {
	e, err := scanEndpoint(t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT `+endpointColumns+` FROM endpoints e WHERE e.id=?`), endpointID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// RecordAgentConnect audits a connection outcome in the endpoint's organization. Unknown
// endpoints have no organization and produce no row, like non-members elsewhere.
func (t *tenancyStore) RecordAgentConnect(ctx context.Context, e *Endpoint, ip, result string) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), "agent:"+e.ID, "agent.connect", e.ID, ip, time.Now().UTC(), "organization", e.OrganizationID, e.EnvironmentID, uuid.NewString(), result)
	return err
}

// TouchEndpoint records a heartbeat. It never changes state.
func (t *tenancyStore) TouchEndpoint(ctx context.Context, endpointID string) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET last_seen_at=? WHERE id=?`), time.Now().UTC(), endpointID)
	return err
}

// AcceptInventory applies a snapshot only when its generation is newer than the stored one, so
// reordered or duplicate reports cannot roll state back. The first accepted snapshot moves an
// approved endpoint to active; an offline one returns to active. Returns false when rejected.
func (t *tenancyStore) AcceptInventory(ctx context.Context, endpointID string, generation uint64) (bool, error) {
	result, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET inventory_generation=?, last_seen_at=?, state=CASE WHEN state IN ('approved','offline') THEN 'active' ELSE state END WHERE id=? AND state IN ('approved','active','offline') AND inventory_generation<?`), generation, time.Now().UTC(), endpointID, generation)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// MarkEndpointOffline is the transport telling the truth about reachability; freshness stays
// in inventory_generation and last_seen_at.
func (t *tenancyStore) MarkEndpointOffline(ctx context.Context, endpointID string) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET state='offline' WHERE id=? AND state='active'`), endpointID)
	return err
}

// EndpointState is the live check a connection makes before trusting what it cached.
func (t *tenancyStore) EndpointState(ctx context.Context, endpointID string) (string, error) {
	var state string
	err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT state FROM endpoints WHERE id=?`), endpointID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return state, err
}
