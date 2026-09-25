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
	resource := endpointID + "/" + identifier
	err := t.run(ctx, a, permissions.ContainerLogs, &resource, nil, false, func(tx *sql.Tx) error {
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

// OpenPodLogTarget authorizes reading one pod container's log under container.logs and
// records the session, naming the pod: the endpoint must be an active Kubernetes endpoint. The
// pod is not resolved against the inventory; the agent reads its spec.
func (t *tenancyStore) OpenPodLogTarget(ctx context.Context, a TenantAccess, endpointID string, pod protocol.PodTarget) error {
	if pod.Validate() != nil {
		return fmt.Errorf("%w: pod", ErrInvalid)
	}
	resource := endpointID + "/pods/" + pod.Namespace + "/" + pod.Name
	if pod.Container != "" {
		resource += "/" + pod.Container
	}
	return t.run(ctx, a, permissions.ContainerLogs, &resource, nil, false, func(tx *sql.Tx) error {
		var state, runtime string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT state,runtime FROM endpoints WHERE id=? AND organization_id=? AND (?='' OR environment_id=?)`),
			endpointID, a.OrganizationID, a.EnvironmentID, a.EnvironmentID).Scan(&state, &runtime)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if runtime != protocol.RuntimeKubernetes {
			return ErrRuntimeUnsupported
		}
		if state != "active" {
			return fmt.Errorf("%w: it is %s", ErrEndpointOffline, state)
		}
		return nil
	})
}

// StillAllowed re-checks a live authorization without writing an audit row. It exists for work
// that outlives the request that started it: a stream is authorized when it opens, and a
// membership removed, a role narrowed or an account disabled while it runs must end it.
//
// No audit row, because this asks the same question every few seconds; the session that
// answered it the first time is what the trail records.
func (t *tenancyStore) StillAllowed(ctx context.Context, a TenantAccess, action permissions.Action, endpointID string) error {
	if a.ActorID == "" || a.OrganizationID == "" {
		return ErrForbidden
	}
	var status, memberStatus, role string
	var restricted bool
	err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT u.status,u.must_change_password,m.status,m.role FROM users u JOIN organization_memberships m ON m.user_id=u.id WHERE u.id=? AND m.organization_id=?`), a.ActorID, a.OrganizationID).
		Scan(&status, &restricted, &memberStatus, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if status != "active" || restricted || memberStatus != "active" || !permissions.Allows(role, action) {
		return ErrForbidden
	}
	if endpointID != "" {
		var state string
		err = t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT state FROM endpoints WHERE id=? AND organization_id=? AND (?='' OR environment_id=?)`),
			endpointID, a.OrganizationID, a.EnvironmentID, a.EnvironmentID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if action == permissions.ContainerExec && state != "active" {
			return ErrEndpointOffline
		}
	}
	return nil
}
