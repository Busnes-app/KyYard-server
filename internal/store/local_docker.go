package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

const LocalDockerEndpointID = "ep_local_docker"

// InitializeLocalDocker is a trusted startup helper, never an HTTP enrollment
// path. Mounting the local socket opts this installation into managing that host.
// The durable marker prevents restart (or deletion of a revoked endpoint's
// environment) from silently restoring authority an administrator removed.
func (t *tenancyStore) InitializeLocalDocker(ctx context.Context, publicKey []byte) (uint64, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return 0, ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO server_settings (key,value,updated_at) VALUES ('local_docker_initialized','1',?) ON CONFLICT (key) DO NOTHING`), now)
	if err != nil {
		return 0, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	fingerprint := protocol.Fingerprint(publicKey)
	if inserted == 1 {
		env := uuid.NewString()
		if _, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO environments (id,organization_id,name) VALUES (?,?,'Local') ON CONFLICT (organization_id,name) DO NOTHING`), env, InitialOrganizationID); err != nil {
			return 0, err
		}
		if err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT id FROM environments WHERE organization_id=? AND name='Local'`), InitialOrganizationID).Scan(&env); err != nil {
			return 0, err
		}
		name := "Local Docker"
		var exists int
		if err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM endpoints WHERE organization_id=? AND name=?`), InitialOrganizationID, name).Scan(&exists); err != nil {
			return 0, err
		}
		if exists > 0 {
			name += " (" + uuid.NewString()[:8] + ")"
		}
		if _, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoints (id,organization_id,environment_id,name,runtime,state,facts,created_at,approved_at,approved_by) VALUES (?,?,?,?,'docker','approved','{"connection":"local"}',?,?,'system:local-docker')`), LocalDockerEndpointID, InitialOrganizationID, env, name, now, now); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_keys (endpoint_id,fingerprint,public_key,state,created_at,acknowledged_at) VALUES (?,?,?,'approved',?,?)`), LocalDockerEndpointID, fingerprint, hex.EncodeToString(publicKey), now, now); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES ('system:local-docker','endpoint.local_connected',?,'Built-in local Docker connection initialized',?,'organization',?,?,?,'success')`), LocalDockerEndpointID, now, InitialOrganizationID, env, uuid.NewString()); err != nil {
			return 0, err
		}
	}
	var generation uint64
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.inventory_generation FROM endpoints e JOIN endpoint_keys k ON k.endpoint_id=e.id WHERE e.id=? AND e.organization_id=? AND e.state IN ('approved','active','offline') AND k.state='approved' AND k.fingerprint=? AND k.public_key=?`), LocalDockerEndpointID, InitialOrganizationID, fingerprint, hex.EncodeToString(publicKey)).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrForbidden
	}
	if err != nil {
		return 0, err
	}
	return generation, tx.Commit()
}
