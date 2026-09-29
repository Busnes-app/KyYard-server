package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// Migration 38 gives endpoint_commands a result column bounded like deployments.result.
func TestCommandResultColumnMigration(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	var name string
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT name FROM schema_migrations WHERE version=?`), 38).Scan(&name); err != nil || name != "endpoint_command_result" {
		t.Fatalf("migration 38: %q %v", name, err)
	}
	endpointID := activeEndpointWith(t, st.Tenancy(), a, nil, nil)
	insert := func(id, result string) error {
		_, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO endpoint_commands (id,endpoint_id,organization_id,environment_id,actor_id,request_id,action,container_id,expects,deadline,created_at,result) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`),
			id, endpointID, a.OrganizationID, a.EnvironmentID, a.ActorID, "r", ActionRun, "", "{}", time.Now().UTC(), time.Now().UTC(), result)
		return err
	}
	if err := insert("fits", strings.Repeat("x", MaxDeploymentResultStoredBytes)); err != nil {
		t.Fatalf("a result at the bound: %v", err)
	}
	if err := insert("oversize", strings.Repeat("x", MaxDeploymentResultStoredBytes+1)); err == nil {
		t.Fatal("an oversize result was stored")
	}
	// A row with no result reads as having none.
	if err := insert("empty", ""); err != nil {
		t.Fatal(err)
	}
	if cmd, err := st.Tenancy().ReadCommand(ctx, a, endpointID, "empty"); err != nil || cmd.Result != nil {
		t.Fatalf("empty result: %+v %v", cmd, err)
	}
}

// Only the endpoint that ran a direct command settles it, by its deployment and request IDs, and
// a real answer replaces the unknown a dropped socket left.
func TestSettleDirectCommand(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	endpointID := activeEndpointWith(t, st.Tenancy(), a, nil, nil)
	id, request := "0b7f4c1e-8a57-4c1c-9d0e-3b8f0a1c2d3e", "request-1"
	_, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO endpoint_commands (id,endpoint_id,organization_id,environment_id,actor_id,request_id,action,container_id,expects,deadline,created_at,dispatched_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`),
		id, endpointID, a.OrganizationID, a.EnvironmentID, a.ActorID, request, ActionRun, "", "{}", time.Now().UTC(), time.Now().UTC(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: id, RequestID: request, Outcome: protocol.OutcomeFailed, Code: protocol.ResultStepFailed,
		Steps: []protocol.DeploymentStep{{Service: "direct", Step: protocol.StepCreate, Outcome: protocol.OutcomeFailed, Code: "runtime_error"}}, Services: []protocol.DeploymentIdentity{}}
	other := res
	other.RequestID = "request-2"
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, other); err != ErrNotFound {
		t.Fatalf("another request's result: %v", err)
	}
	if err := st.Tenancy().SettleDirectCommand(ctx, "ep_other", res); err != ErrNotFound {
		t.Fatalf("another endpoint's result: %v", err)
	}
	if _, err := st.Tenancy().AbandonCommands(ctx, endpointID); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != nil {
		t.Fatal(err)
	}
	cmd, err := st.Tenancy().ReadCommand(ctx, a, endpointID, id)
	if err != nil || cmd.Outcome != protocol.OutcomeFailed || cmd.Detail != protocol.ResultStepFailed || cmd.Result == nil || len(cmd.Result.Steps) != 1 {
		t.Fatalf("settled: %+v %v", cmd, err)
	}
	// The first real answer wins.
	res.Outcome, res.Code = protocol.OutcomeSucceeded, ""
	res.Steps = []protocol.DeploymentStep{}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != ErrNotFound {
		t.Fatalf("a second answer: %v", err)
	}
}

// directRun is a minimal valid run frame for image on a host whose inventory lists it.
func directRun(name, image string) DirectCommand {
	return DirectCommand{Action: ActionRun, Confirm: name, MaxFrameBytes: protocol.MaxDeploymentRequestBytes, Frame: protocol.DeploymentRequest{Services: []protocol.DeploymentService{{
		Name: "direct", ContainerName: name, ImageID: image, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: []protocol.Mount{}, Explicit: &protocol.ExplicitService{},
	}}}}
}

// An unsettled direct command past its deadline no longer blocks the endpoint: creating the next
// one settles it unknown, and a late real answer may still replace that.
func TestCreateDirectCommandSweepsAnExpiredOne(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	image := "sha256:" + strings.Repeat("b", 64)
	endpointID := activeEndpointWith(t, st.Tenancy(), a, nil, []protocol.Image{{ID: image, Tags: []string{}, Digests: []string{}}})
	first, _, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("one", image))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("two", image)); err != ErrCommandInProgress {
		t.Fatalf("while the first is live: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_commands SET deadline=? WHERE id=?`), time.Now().UTC().Add(-time.Minute), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("two", image)); err != nil {
		t.Fatalf("after the deadline: %v", err)
	}
	if cmd, err := st.Tenancy().ReadCommand(ctx, a, endpointID, first.ID); err != nil || cmd.Outcome != protocol.OutcomeUnknown {
		t.Fatalf("expired command: %+v %v", cmd, err)
	}
}

// One writer settles a direct command: a plain command.result cannot, a dispatch failure goes
// through FailDirectCommand, and a result must speak only of the direct service.
func TestDirectCommandsHaveOneWriter(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	image := "sha256:" + strings.Repeat("b", 64)
	endpointID := activeEndpointWith(t, st.Tenancy(), a, nil, []protocol.Image{{ID: image, Tags: []string{}, Digests: []string{}}})
	cmd, req, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("one", image))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().SettleCommand(ctx, endpointID, cmd.ID, protocol.OutcomeSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	read := func() *Command {
		t.Helper()
		c, err := st.Tenancy().ReadCommand(ctx, a, endpointID, cmd.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := read(); c.Outcome != "" {
		t.Fatalf("a command.result settled a direct command: %+v", c)
	}
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: "other", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{}}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != ErrUnreadableResult {
		t.Fatalf("a step for another service: %v", err)
	}
	identity := protocol.DeploymentIdentity{Service: "direct", ContainerID: strings.Repeat("e", 64), ImageID: image, CreatedUnix: time.Now().Unix()}
	res.Steps = []protocol.DeploymentStep{}
	res.Services = []protocol.DeploymentIdentity{identity, identity}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != ErrUnreadableResult {
		t.Fatalf("two identities: %v", err)
	}
	res.Services = []protocol.DeploymentIdentity{identity}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != nil {
		t.Fatal(err)
	}
	if c := read(); c.Outcome != protocol.OutcomeSucceeded || c.ResultContainerID != identity.ContainerID || c.ContainerID != "" {
		t.Fatalf("settled: %+v", c)
	}
	// The new container's Activity lists the run that made it.
	list, err := st.Tenancy().ListCommands(ctx, a, endpointID, identity.ContainerID, 0)
	if err != nil || len(list) != 1 || list[0].ID != cmd.ID {
		t.Fatalf("by new container: %+v %v", list, err)
	}
	next, _, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("two", image))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().FailDirectCommand(ctx, endpointID, next.ID, "deployment_not_sent"); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.Tenancy().ReadCommand(ctx, a, endpointID, next.ID); c == nil || c.Outcome != protocol.OutcomeFailed || c.Detail != "deployment_not_sent" {
		t.Fatalf("unsent: %+v", c)
	}
}
