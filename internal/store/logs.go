package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
)

// LogTarget is the container a log request resolved to, as the endpoint last reported it.
type LogTarget struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
}

// OpenLogTarget authorizes reading one container's log and resolves what to read.
//
// The identifier the operator used is resolved against the last inventory and the stream is
// opened on the container ID it yielded, for the same reason a destructive command travels as
// an ID: a name is a label the runtime reassigns, and a stream that followed the name would
// hand over another container's output after a recreate.
//
// The session is audited. Log bodies are never stored -- what is recorded is that this actor
// read this container's log, which is the point of the row.
func (t *tenancyStore) OpenLogTarget(ctx context.Context, a TenantAccess, endpointID, identifier string) (*LogTarget, error) {
	if !protocol.ValidContainerID(identifier) {
		return nil, fmt.Errorf("%w: container", ErrInvalid)
	}
	target := &LogTarget{}
	err := t.run(ctx, a, permissions.ContainerLogs, endpointID+"/"+identifier, false, func(tx *sql.Tx) error {
		var state string
		row := tx.QueryRowContext(ctx, t.store.rebind(`SELECT state FROM endpoints WHERE id=? AND organization_id=? AND (?='' OR environment_id=?)`),
			endpointID, a.OrganizationID, a.EnvironmentID, a.EnvironmentID)
		if err := row.Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state != "active" {
			// Nothing to read from an endpoint that is not connected, and queueing the
			// request would mean holding it open against a host nobody is talking to.
			return fmt.Errorf("%w: it is %s", ErrEndpointOffline, state)
		}
		name, id, err := t.confirmable(ctx, tx, endpointID, identifier)
		if err != nil {
			return err
		}
		target.ContainerID, target.Name = id, name
		return nil
	})
	if err != nil {
		return nil, err
	}
	return target, nil
}
