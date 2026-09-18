package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
)

// ReadInspectionTarget derives the immutable target from scoped, fresh
// inventory. The caller never supplies an image or creation identity.
func (t *tenancyStore) ReadInspectionTarget(ctx context.Context, a TenantAccess, endpoint, container string) (protocol.InspectionTarget, error) {
	var out protocol.InspectionTarget
	if !protocol.ValidContainerID(container) {
		return out, ErrInvalid
	}
	err := t.readTenant(ctx, a, permissions.EndpointRead, func(tx *sql.Tx) error {
		var state, raw string
		var received, observed time.Time
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.state,i.snapshot,i.received_at,i.observed_at FROM endpoints e JOIN endpoint_inventory i ON i.endpoint_id=e.id WHERE e.id=? AND e.organization_id=? AND (?='' OR e.environment_id=?)`), endpoint, a.OrganizationID, a.EnvironmentID, a.EnvironmentID).Scan(&state, &raw, &received, &observed)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if state != "active" {
			return ErrEndpointOffline
		}
		if time.Since(received) > 3*time.Minute || time.Until(received) > time.Minute || time.Since(observed) > 5*time.Minute || time.Until(observed) > 5*time.Minute {
			return ErrAdoptionChanged
		}
		var snapshot protocol.Snapshot
		if json.Unmarshal([]byte(raw), &snapshot) != nil || snapshot.Engine.Version == "" || len(snapshot.Containers) > protocol.MaxContainers {
			return ErrAdoptionChanged
		}
		for _, part := range snapshot.Truncated {
			if part == "containers" {
				return ErrAdoptionChanged
			}
		}
		found := false
		for _, c := range snapshot.Containers {
			if c.ID == container {
				if found {
					return ErrAdoptionChanged
				}
				found = true
				out = protocol.InspectionTarget{ContainerID: c.ID, ImageID: c.ImageID, CreatedUnix: c.CreatedAt.Unix()}
				if out.Validate() != nil {
					return ErrAdoptionChanged
				}
			}
		}
		if !found {
			return ErrNotFound
		}
		return nil
	})
	return out, err
}
