package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/permissions"

	"github.com/google/uuid"
)

// AgentIdentity is what the connection handshake needs: the endpoint and the key that may
// authenticate it. Pending endpoints authenticate with their key under review so they can
// receive the approval notice; nothing else is granted to them.
type AgentIdentity struct {
	Endpoint    Endpoint
	PublicKey   []byte
	Fingerprint string
}

// Sentinel errors let the handshake tell a retired key (the agent should switch to its
// acknowledged one) from a key still under review (the agent must keep using its old one).
var (
	ErrKeyRetired       = errors.New("key retired")
	ErrKeyPendingReview = errors.New("key pending review")
)

func (t *tenancyStore) AgentIdentity(ctx context.Context, endpointID, fingerprint string) (*AgentIdentity, error) {
	e, err := scanEndpoint(t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT `+endpointColumns+` FROM endpoints e WHERE e.id=?`), pendingCutoff(), endpointID))
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
	return &AgentIdentity{Endpoint: *e, PublicKey: key, Fingerprint: fingerprint}, nil
}

// ReadEndpointRaw is a trusted helper for the handshake's own audit row; it is not authorized.
func (t *tenancyStore) ReadEndpointRaw(ctx context.Context, endpointID string) (*Endpoint, error) {
	e, err := scanEndpoint(t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT `+endpointColumns+` FROM endpoints e WHERE e.id=?`), pendingCutoff(), endpointID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// RecordAgentConnect audits a connection outcome in the endpoint's organization. Unknown
// endpoints have no organization and produce no row, like non-members elsewhere. Details is
// display-safe operator text, empty for the plain outcomes.
func (t *tenancyStore) RecordAgentConnect(ctx context.Context, e *Endpoint, ip, result, details string) error {
	if !displaySafe(details) {
		details = ""
	}
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result,details) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), "agent:"+e.ID, "agent.connect", e.ID, ip, time.Now().UTC(), "organization", e.OrganizationID, e.EnvironmentID, uuid.NewString(), result, details)
	return err
}

// TouchEndpoint records a heartbeat. It never changes state.
func (t *tenancyStore) TouchEndpoint(ctx context.Context, endpointID string) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET last_seen_at=? WHERE id=?`), time.Now().UTC(), endpointID)
	return err
}

// MaxSnapshotBytes is the shared limit from the protocol package.
const MaxSnapshotBytes = protocol.MaxSnapshotBytes

// GenerationSkew is how far ahead of the server clock a generation (a Unix timestamp by
// construction) may run; anything beyond is a broken or hostile clock and is refused so it
// cannot pin the endpoint's inventory forever.
const GenerationSkew = time.Hour

// AcceptInventory applies a snapshot only when its generation is newer than the stored one, so
// reordered or duplicate reports cannot roll state back, and stores the bounded snapshot in the
// same transaction. The first accepted snapshot moves an approved endpoint to active; an
// offline one returns to active. Returns false when rejected.
func (t *tenancyStore) AcceptInventory(ctx context.Context, endpointID string, generation uint64, observedAt time.Time, snapshot []byte) (bool, error) {
	if len(snapshot) > MaxSnapshotBytes {
		return false, ErrInvalid
	}
	now := time.Now().UTC()
	if generation > uint64(now.Add(GenerationSkew).Unix()) {
		return false, nil
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET inventory_generation=?, last_seen_at=?, state=CASE WHEN state IN ('approved','offline') THEN 'active' ELSE state END WHERE id=? AND state IN ('approved','active','offline') AND inventory_generation<?`), generation, now, endpointID, generation)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if len(snapshot) > 0 {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_inventory (endpoint_id,generation,observed_at,received_at,snapshot) VALUES (?,?,?,?,?) ON CONFLICT (endpoint_id) DO UPDATE SET generation=excluded.generation,observed_at=excluded.observed_at,received_at=excluded.received_at,snapshot=excluded.snapshot`), endpointID, generation, observedAt.UTC(), now, string(snapshot)); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// Inventory is the stored snapshot plus the freshness facts the UI needs to tell stale from
// current: when the agent observed it, when the server received it, and the endpoint state.
type Inventory struct {
	EndpointID string          `json:"endpoint_id"`
	State      string          `json:"state"`
	Generation uint64          `json:"generation"`
	ObservedAt time.Time       `json:"observed_at"`
	ReceivedAt time.Time       `json:"received_at"`
	Snapshot   json.RawMessage `json:"snapshot"`
}

func (t *tenancyStore) ReadInventory(ctx context.Context, a TenantAccess, endpointID string) (*Inventory, error) {
	var inv *Inventory
	err := t.readTenant(ctx, a, permissions.EndpointRead, func(tx *sql.Tx) error {
		var i Inventory
		var snap string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.id,e.state,i.generation,i.observed_at,i.received_at,i.snapshot FROM endpoints e JOIN endpoint_inventory i ON i.endpoint_id=e.id WHERE e.organization_id=? AND e.id=? AND (?='' OR e.environment_id=?)`), a.OrganizationID, endpointID, a.EnvironmentID, a.EnvironmentID).Scan(&i.EndpointID, &i.State, &i.Generation, &i.ObservedAt, &i.ReceivedAt, &snap)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if i.State == "pending" && time.Since(i.ReceivedAt) > pendingEndpointLife {
			i.State = "expired"
		}
		i.Snapshot = json.RawMessage(snap)
		inv = &i
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
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
