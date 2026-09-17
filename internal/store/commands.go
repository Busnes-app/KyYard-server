package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// CommandRetention keeps a settled command long enough to answer "what happened to my restart"
// the next morning, and no longer. An unsettled one is never pruned: a command whose outcome
// nobody knows is exactly the record an operator needs.
const CommandRetention = 7 * 24 * time.Hour

// CommandDeadline is how long an agent has to answer before the outcome is timed out.
const CommandDeadline = 30 * time.Second

// Command is one instruction and what became of it. Outcome is empty while the command is in
// flight; afterwards it is one of protocol's five, and the set is closed, so a command whose
// answer never arrived reads as unknown rather than as a row with nothing in it.
type Command struct {
	ID             string               `json:"id"`
	EndpointID     string               `json:"endpoint_id"`
	OrganizationID string               `json:"organization_id"`
	EnvironmentID  string               `json:"environment_id"`
	ActorID        string               `json:"actor_id"`
	RequestID      string               `json:"request_id"`
	Action         string               `json:"action"`
	ContainerID    string               `json:"container_id"`
	Expects        protocol.Expectation `json:"expects"`
	Deadline       time.Time            `json:"deadline"`
	Outcome        string               `json:"outcome"`
	Detail         string               `json:"detail,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	DispatchedAt   *time.Time           `json:"dispatched_at,omitempty"`
	SettledAt      *time.Time           `json:"settled_at,omitempty"`
}

// InFlight reports whether the command is still waiting for an answer.
func (c *Command) InFlight() bool { return c.Outcome == "" }

// containerName is Docker's grammar for a container name or ID, anchored and length-bounded.
// Anything outside it cannot name a container, and several things outside it can name a
// different Engine API route.
var containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// commandActions maps each action to the permission it needs. Removing a container is not a
// stronger form of stopping one: it is a different thing to be allowed to do.
var commandActions = map[string]permissions.Action{
	protocol.ActionStart:   permissions.ContainerOperate,
	protocol.ActionStop:    permissions.ContainerOperate,
	protocol.ActionRestart: permissions.ContainerOperate,
	protocol.ActionRemove:  permissions.ContainerDestroy,
}

// CreateCommand records the intent before anything is sent. The row exists first so that a
// command which is dispatched and then lost still has somewhere to be marked unknown: an
// operation the control plane cannot account for is worse than one that failed.
func (t *tenancyStore) CreateCommand(ctx context.Context, a TenantAccess, endpointID, action, containerID, confirm string, expects protocol.Expectation) (*Command, error) {
	needs, ok := commandActions[action]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported action %q", ErrInvalid, action)
	}
	// Destructive is a property of the permission, not a second list to keep in step with it:
	// a future action given ContainerDestroy inherits these requirements automatically.
	destructive := needs == permissions.ContainerDestroy
	if destructive && expects.State == "" {
		// Without this the actor is asking to destroy whatever is there now, not the thing
		// they looked at.
		return nil, fmt.Errorf("%w: a destructive command must say what state it expects", ErrInvalid)
	}
	if !containerName.MatchString(containerID) {
		// Docker's own grammar for a name or ID. displaySafe is not enough for a value that
		// becomes part of a URL: it permits a slash, a query and a fragment.
		return nil, fmt.Errorf("%w: container", ErrInvalid)
	}
	if !displaySafe(expects.ImageDigest) || !displaySafe(expects.State) {
		return nil, fmt.Errorf("%w: expectation", ErrInvalid)
	}
	raw, err := json.Marshal(expects)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	cmd := &Command{
		ID:          uuid.NewString(),
		EndpointID:  endpointID,
		ActorID:     a.ActorID,
		Action:      action,
		ContainerID: containerID,
		Expects:     expects,
		Deadline:    now.Add(CommandDeadline),
		CreatedAt:   now,
	}
	err = t.withTenantTarget(ctx, a, needs, endpointID, func(tx *sql.Tx) error {
		var state string
		row := tx.QueryRowContext(ctx, t.store.rebind(`SELECT organization_id,environment_id,state FROM endpoints WHERE id=? AND organization_id=? AND (?='' OR environment_id=?)`),
			endpointID, a.OrganizationID, a.EnvironmentID, a.EnvironmentID)
		if err := row.Scan(&cmd.OrganizationID, &cmd.EnvironmentID, &state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if destructive {
			// The confirmation is checked against what the server knows the container is
			// called, not against the request repeating itself, which would attest to
			// nothing. The identifier may be a name or an ID; both resolve here.
			name, resolvedID, err := t.confirmable(ctx, tx, endpointID, containerID)
			if err != nil {
				return err
			}
			if confirm != name {
				return fmt.Errorf("%w: confirm must be %q", ErrInvalid, name)
			}
			// What travels is the container the confirmation was checked against, not the
			// name it answered to. A name is a label the runtime reassigns: a compose
			// recreate puts a different container behind it, and the preview warns about
			// exactly that. Sending the name would confirm one container and destroy
			// whichever holds the label when the agent acts.
			cmd.ContainerID = resolvedID
		}
		if state != "active" {
			// A command for an endpoint that is not connected has nowhere to go, and queueing
			// it would mean an operator's decision executing at an unknown later time against
			// a host whose state they have not seen since.
			return fmt.Errorf("%w: it is %s", ErrEndpointOffline, state)
		}
		cmd.RequestID = a.CorrelationID
		_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_commands (id,endpoint_id,organization_id,environment_id,actor_id,request_id,action,container_id,expects,deadline,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`),
			cmd.ID, cmd.EndpointID, cmd.OrganizationID, cmd.EnvironmentID, cmd.ActorID, cmd.RequestID, cmd.Action, cmd.ContainerID, string(raw), cmd.Deadline, cmd.CreatedAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return cmd, nil
}

// MarkCommandDispatched records that the frame reached the socket. Until this is set the
// command was never sent, so nothing needs reconciling if the server stops here.
func (t *tenancyStore) MarkCommandDispatched(ctx context.Context, id string) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_commands SET dispatched_at=? WHERE id=? AND dispatched_at IS NULL`), time.Now().UTC(), id)
	return err
}

// SettleCommand records an outcome once. The first answer wins: a late duplicate from an agent
// that retried, or an unknown written when the socket dropped, must not overwrite a real one.
func (t *tenancyStore) SettleCommand(ctx context.Context, endpointID, id, outcome, detail string) error {
	switch outcome {
	case protocol.OutcomeSucceeded, protocol.OutcomeFailed, protocol.OutcomeDenied, protocol.OutcomeTimedOut, protocol.OutcomeUnknown:
	default:
		return fmt.Errorf("%w: outcome %q", ErrInvalid, outcome)
	}
	if len(detail) > protocol.MaxResultDetailBytes {
		detail = detail[:protocol.MaxResultDetailBytes]
	}
	if !displaySafe(detail) {
		detail = ""
	}
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_commands SET outcome=?, detail=?, settled_at=? WHERE id=? AND endpoint_id=? AND outcome=''`),
		outcome, detail, time.Now().UTC(), id, endpointID)
	return err
}

// AbandonCommands settles everything still in flight for an endpoint as unknown. The socket
// going is not evidence that the work did not happen, so the outcome says exactly that rather
// than guessing at failure, and nothing is retried on its own.
func (t *tenancyStore) AbandonCommands(ctx context.Context, endpointID string) (int64, error) {
	result, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_commands SET outcome=?, detail=?, settled_at=? WHERE endpoint_id=? AND outcome='' AND dispatched_at IS NOT NULL`),
		protocol.OutcomeUnknown, "the connection ended before a result arrived", time.Now().UTC(), endpointID)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// ReadCommand returns one command in the caller's scope.
func (t *tenancyStore) ReadCommand(ctx context.Context, a TenantAccess, endpointID, id string) (*Command, error) {
	var cmd *Command
	err := t.readTenant(ctx, a, permissions.EndpointRead, func(tx *sql.Tx) error {
		if err := t.endpointInScope(ctx, tx, a, endpointID); err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx, t.store.rebind(commandColumns+` WHERE id=? AND endpoint_id=?`), id, endpointID)
		var err error
		cmd, err = scanCommand(row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return cmd, nil
}

// ListCommands returns an endpoint's recent commands, newest first.
func (t *tenancyStore) ListCommands(ctx context.Context, a TenantAccess, endpointID string, limit int) ([]Command, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := []Command{}
	err := t.readTenant(ctx, a, permissions.EndpointRead, func(tx *sql.Tx) error {
		if err := t.endpointInScope(ctx, tx, a, endpointID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(commandColumns+` WHERE endpoint_id=? ORDER BY created_at DESC LIMIT ?`), endpointID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			cmd, err := scanCommand(rows)
			if err != nil {
				return err
			}
			out = append(out, *cmd)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const commandColumns = `SELECT id,endpoint_id,organization_id,environment_id,actor_id,request_id,action,container_id,expects,deadline,outcome,detail,created_at,dispatched_at,settled_at FROM endpoint_commands`

type scanner interface{ Scan(dest ...any) error }

func scanCommand(row scanner) (*Command, error) {
	var c Command
	var expects string
	var deadline, created any
	var dispatched, settled sql.NullTime
	if err := row.Scan(&c.ID, &c.EndpointID, &c.OrganizationID, &c.EnvironmentID, &c.ActorID, &c.RequestID, &c.Action, &c.ContainerID, &expects, &deadline, &c.Outcome, &c.Detail, &created, &dispatched, &settled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var err error
	if c.Deadline, err = scanTime(deadline); err != nil {
		return nil, err
	}
	if c.CreatedAt, err = scanTime(created); err != nil {
		return nil, err
	}
	if dispatched.Valid {
		at := dispatched.Time.UTC()
		c.DispatchedAt = &at
	}
	if settled.Valid {
		at := settled.Time.UTC()
		c.SettledAt = &at
	}
	if err := json.Unmarshal([]byte(expects), &c.Expects); err != nil {
		return nil, err
	}
	return &c, nil
}

// confirmable resolves a container identifier against the last inventory and returns both the
// name a destructive confirmation must repeat and the ID the command will carry. Without an
// inventory there is nothing to confirm against, and guessing would make the confirmation
// ceremonial.
func (t *tenancyStore) confirmable(ctx context.Context, tx *sql.Tx, endpointID, identifier string) (name, id string, err error) {
	var raw string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT snapshot FROM endpoint_inventory WHERE endpoint_id=?`), endpointID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w: this endpoint has reported no inventory to confirm against", ErrInvalid)
	}
	if err != nil {
		return "", "", err
	}
	var snap protocol.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return "", "", err
	}
	for _, c := range snap.Containers {
		if c.ID == identifier || c.Name == identifier {
			if !containerName.MatchString(c.ID) {
				return "", "", fmt.Errorf("%w: the recorded container ID is not usable", ErrInvalid)
			}
			return c.Name, c.ID, nil
		}
	}
	return "", "", fmt.Errorf("%w: no container %q in the last inventory", ErrNotFound, identifier)
}
