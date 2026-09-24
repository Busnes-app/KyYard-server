package store

import (
	"context"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// reconcileEndpoint is activeEndpointWith under a unique name, since enrollment names are
// unique per organization.
func reconcileEndpoint(t *testing.T, st *SQLStore, a TenantAccess, name string) string {
	t.Helper()
	id := activeEndpointWith(t, st.Tenancy(), a, nil, nil)
	if _, err := st.db.Exec(st.rebind(`UPDATE endpoints SET name=? WHERE id=?`), name, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func reconcileCommand(t *testing.T, st *SQLStore, a TenantAccess, endpointID string) *Command {
	t.Helper()
	cmd, err := st.Tenancy().CreateCommand(context.Background(), a, endpointID, protocol.ActionRestart, "web", "", protocol.Expectation{})
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func reconciledAudits(t *testing.T, st *SQLStore) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_records WHERE action='endpoint.commands.reconciled'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReconcileAfterStartSettlesInFlightCommands(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	e1 := reconcileEndpoint(t, st, a, "host-1")
	e2 := reconcileEndpoint(t, st, a, "host-2")
	dispatched := reconcileCommand(t, st, a, e1)
	if err := ts.MarkCommandDispatched(ctx, dispatched.ID); err != nil {
		t.Fatal(err)
	}
	pending := reconcileCommand(t, st, a, e1)
	done := reconcileCommand(t, st, a, e2)
	if err := ts.SettleCommand(ctx, e2, done.ID, protocol.OutcomeSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}

	n, err := ts.ReconcileAfterStart(ctx)
	if err != nil || n != 2 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	for _, id := range []string{dispatched.ID, pending.ID} {
		cmd, err := ts.ReadCommand(ctx, a, e1, id)
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Outcome != protocol.OutcomeUnknown || cmd.Detail != reconcileDetail || cmd.SettledAt == nil {
			t.Fatalf("command %s after reconcile: %+v", id, cmd)
		}
	}
	if cmd, err := ts.ReadCommand(ctx, a, e2, done.ID); err != nil || cmd.Outcome != protocol.OutcomeSucceeded || cmd.Detail != "ok" {
		t.Fatalf("settled command changed: %+v %v", cmd, err)
	}

	var user, resource, details, scope, org, env, correlation, result string
	if err := st.db.QueryRow(`SELECT user_id,resource,details,scope,organization_id,environment_id,correlation_id,result FROM audit_records WHERE action='endpoint.commands.reconciled'`).
		Scan(&user, &resource, &details, &scope, &org, &env, &correlation, &result); err != nil {
		t.Fatal(err)
	}
	if user != "system" || resource != e1 || details != "commands=2" || scope != "organization" || org != a.OrganizationID || env != a.EnvironmentID || correlation == "" || result != "unknown" {
		t.Fatalf("audit row: %q %q %q %q %q %q %q %q", user, resource, details, scope, org, env, correlation, result)
	}
	if got := reconciledAudits(t, st); got != 1 {
		t.Fatalf("%d reconcile audit rows, want 1 (none for %s)", got, e2)
	}

	n, err = ts.ReconcileAfterStart(ctx)
	if err != nil || n != 0 {
		t.Fatalf("second reconcile: %d %v", n, err)
	}
	if got := reconciledAudits(t, st); got != 1 {
		t.Fatalf("second reconcile wrote audit rows: %d", got)
	}
}

// Deployments keep their own recovery path: the sweep times them out and an agent re-sending
// its ledgered result settles them with what happened.
func TestReconcileAfterStartLeavesDeployments(t *testing.T) {
	st, a, app, _, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ReconcileAfterStart(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.State != "applying" {
		t.Fatalf("deployment after reconcile: %+v %v", got, err)
	}
}

func TestReconcileAfterStartHonoursFirstAnswer(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	e := reconcileEndpoint(t, st, a, "host-1")
	cmd := reconcileCommand(t, st, a, e)
	if err := ts.MarkCommandDispatched(ctx, cmd.ID); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleCommand(ctx, e, cmd.ID, protocol.OutcomeSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}
	if n, err := ts.ReconcileAfterStart(ctx); err != nil || n != 0 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	if got, err := ts.ReadCommand(ctx, a, e, cmd.ID); err != nil || got.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("first answer overwritten: %+v %v", got, err)
	}
}

// Outcome is the only in-flight marker: a row with settled_at stamped but no outcome (a
// partial write) still settles.
func TestReconcileAfterStartSettlesRowsWithStrayTimestamps(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	e := reconcileEndpoint(t, st, a, "host-1")
	cmd := reconcileCommand(t, st, a, e)
	if _, err := st.db.Exec(st.rebind(`UPDATE endpoint_commands SET settled_at=?, outcome='' WHERE id=?`), cmd.CreatedAt, cmd.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := ts.ReconcileAfterStart(ctx); err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	if got, err := ts.ReadCommand(ctx, a, e, cmd.ID); err != nil || got.Outcome != protocol.OutcomeUnknown || got.Detail != reconcileDetail {
		t.Fatalf("stray-timestamp row: %+v %v", got, err)
	}
}
