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
