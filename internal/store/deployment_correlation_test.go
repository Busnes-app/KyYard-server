package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

// correlationRows counts the audit rows carrying one correlation ID.
func correlationRows(t *testing.T, st *SQLStore, correlation string) (n int) {
	t.Helper()
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE correlation_id=?`), correlation).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func deploymentRow(t *testing.T, st *SQLStore, id string) (state, result string) {
	t.Helper()
	if err := st.db.QueryRow(st.rebind(`SELECT state,result FROM deployments WHERE id=?`), id).Scan(&state, &result); err != nil {
		t.Fatal(err)
	}
	return state, result
}

// The plan's request ID is the deployment's correlation ID: stored on the row, sent in the frame,
// and on the plan's, the apply's and the settle's audit rows, never the apply request's own.
func TestDeploymentCorrelationFollowsThePlan(t *testing.T) {
	st, a, app, endpoint, _, m, _, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	plan := a
	plan.CorrelationID = "plan-request"
	d, err := ts.PlanDeployment(ctx, plan, app.ID, planRequest(m), nil, key, false)
	if err != nil || d.CorrelationID != "plan-request" {
		t.Fatalf("plan: %+v %v", d, err)
	}
	var stored string
	if err := st.db.QueryRow(st.rebind(`SELECT correlation_id FROM deployments WHERE id=?`), d.ID).Scan(&stored); err != nil || stored != "plan-request" {
		t.Fatalf("row: %q %v", stored, err)
	}
	apply := a
	apply.CorrelationID = "apply-request"
	applied, req, err := ts.ApplyDeployment(ctx, apply, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes)
	if err != nil || req.RequestID != "plan-request" || applied.CorrelationID != "plan-request" {
		t.Fatalf("apply: %+v %+v %v", applied, req, err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(applied, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); err != nil {
		t.Fatal(err)
	}
	if n := correlationRows(t, st, "plan-request"); n != 3 {
		t.Fatalf("plan, apply and settle audit rows under the plan's ID: %d", n)
	}
	if n := correlationRows(t, st, "apply-request"); n != 0 {
		t.Fatalf("rows under the apply request's own ID: %d", n)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.CorrelationID != "plan-request" {
		t.Fatalf("read: %+v %v", got, err)
	}
}

// Abandoning, refusing and failing a deployment audit under its plan's correlation ID too.
func TestSystemTransitionsCarryThePlanCorrelation(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if d.CorrelationID != "test-request" {
		t.Fatalf("the fixture plans under %q", d.CorrelationID)
	}
	apply := a
	apply.CorrelationID = "apply-request"
	if _, _, err := ts.ApplyDeployment(ctx, apply, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := ts.RefuseDeploymentResult(ctx, endpoint, d.ID, "the host's result did not match the plan; inspect the host"); err != nil {
		t.Fatal(err)
	}
	if err := ts.FailDeployment(ctx, d.ID, "the endpoint disconnected before the deployment was sent"); err != nil {
		t.Fatal(err)
	}
	for _, details := range []string{"outcome=abandoned", "outcome=refused", "outcome=not_sent"} {
		var n int
		if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE correlation_id=? AND details=?`), "test-request", details).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: %d %v", details, n, err)
		}
	}
}

// Without a request ID the plan mints one, and its audit row carries the same.
func TestPlanMintsACorrelationIDWhenNoneIsGiven(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	a.CorrelationID = ""
	d, err := st.Tenancy().PlanDeployment(context.Background(), a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := uuid.Parse(d.CorrelationID); perr != nil {
		t.Fatalf("correlation %q: %v", d.CorrelationID, perr)
	}
	if n := correlationRows(t, st, d.CorrelationID); n != 1 {
		t.Fatalf("plan audit rows: %d", n)
	}
}

// A removal is its own plan: the frame, the row and the settle's audit row share its request ID.
func TestRemovalCarriesTheCorrelationID(t *testing.T) {
	st, a, app, endpoint, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	a.CorrelationID = "remove-request"
	d, req, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"})
	if err != nil || d.CorrelationID != "remove-request" || req.RequestID != "remove-request" {
		t.Fatalf("removal: %+v %+v %v", d, req, err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, removalResult(d, protocol.OutcomeSucceeded, removalSteps("web", protocol.OutcomeSucceeded, protocol.OutcomeSucceeded, protocol.OutcomeSucceeded))); err != nil {
		t.Fatal(err)
	}
	if n := correlationRows(t, st, "remove-request"); n != 2 {
		t.Fatalf("removal and settle audit rows: %d", n)
	}
}

// A result carrying another request ID never settles the row: here the ID of the plan this one
// replaced (Review Focus 2).
func TestSettleRefusesAForeignRequestID(t *testing.T) {
	st, a, app, endpoint, _, m, _, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	first := a
	first.CorrelationID = "req-one"
	if _, err := ts.PlanDeployment(ctx, first, app.ID, planRequest(m), nil, key, false); err != nil {
		t.Fatal(err)
	}
	second := a
	second.CorrelationID = "req-two"
	d, err := ts.PlanDeployment(ctx, second, app.ID, planRequest(m), nil, key, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	res := settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))
	res.RequestID = "req-one"
	if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign request id: %v", err)
	}
	if state, result := deploymentRow(t, st, d.ID); state != "applying" || result != "" {
		t.Fatalf("a foreign result moved the row: %s %q", state, result)
	}
	res.RequestID = "req-two"
	if err := ts.SettleDeployment(ctx, endpoint, res); err != nil {
		t.Fatal(err)
	}
}

// An older binary's result has no request ID and no codes: each missing code reads as legacy and
// its sentence is dropped, so the in-flight deployment still settles (Review Focus 5).
func TestSettleReadsALegacyResult(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: d.ID, Outcome: protocol.OutcomeDenied, Steps: []protocol.DeploymentStep{
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeDenied, Detail: "the container no longer exists"},
		{Service: "web", Step: protocol.StepImage, Outcome: protocol.OutcomeSkipped},
	}, Services: []protocol.DeploymentIdentity{}}
	if err := ts.SettleDeployment(ctx, endpoint, res); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.State != "denied" || got.Detail != "" || got.Result == nil || got.Result.Code != protocol.CodeLegacy {
		t.Fatalf("settled: %+v %v", got, err)
	}
	if s := got.Result.Steps[0]; s.Code != protocol.CodeLegacy || s.Detail != "" || got.Result.Steps[1].Code != "" {
		t.Fatalf("steps: %+v", got.Result.Steps)
	}
	if _, result := deploymentRow(t, st, d.ID); strings.Contains(result, "no longer exists") {
		t.Fatalf("a sentence was stored: %s", result)
	}
}

// A current agent (the result carries a request ID) must code a failing step: a denied step
// without one is unreadable and nothing is stored (Review Focus 5).
func TestSettleRefusesACodelessStepFromACurrentAgent(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeDenied, Code: protocol.ResultStepFailed, Steps: []protocol.DeploymentStep{
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeDenied},
	}, Services: []protocol.DeploymentIdentity{}}
	if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrUnreadableResult) {
		t.Fatalf("codeless step: %v", err)
	}
	if state, result := deploymentRow(t, st, d.ID); state != "applying" || result != "" {
		t.Fatalf("stored: %s %q", state, result)
	}
}

// A code whose parameter has the wrong shape is refused before anything is stored (Review Focus 1).
func TestSettleRefusesAMalformedCodeParameter(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	for _, step := range []protocol.DeploymentStep{
		{Service: "web", Step: protocol.StepStop, Outcome: protocol.OutcomeFailed, Code: "runtime_status", Detail: "abc"},
		{Service: "web", Step: protocol.StepStart, Outcome: protocol.OutcomeFailed, Code: "identity_unverified", Detail: strings.Repeat("e", 63)},
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeDenied, Code: "unsupported", Detail: "privileged,privileged"},
	} {
		for _, requestID := range []string{d.CorrelationID, ""} {
			res := protocol.DeploymentResult{Deployment: d.ID, RequestID: requestID, Outcome: step.Outcome, Code: protocol.ResultStepFailed, Steps: []protocol.DeploymentStep{step}, Services: []protocol.DeploymentIdentity{}}
			if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrUnreadableResult) {
				t.Fatalf("%s %q (request %q): %v", step.Code, step.Detail, requestID, err)
			}
		}
	}
	if state, result := deploymentRow(t, st, d.ID); state != "applying" || result != "" {
		t.Fatalf("stored: %s %q", state, result)
	}
}

// A result stored before codes reads back with legacy codes and no sentence.
func TestReadDeploymentNormalisesAStoredLegacyResult(t *testing.T) {
	st, a, app, _, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	old := `{"steps":[{"service":"web","step":"create","outcome":"failed","detail":"the runtime refused with status 500"}],"services":[]}`
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET state='failed',detail=?,result=?,settled_at=? WHERE id=?`), "service web, step create: the runtime refused with status 500", old, time.Now().UTC(), d.ID); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.Result == nil || got.Result.Code != protocol.CodeLegacy || len(got.Result.Steps) != 1 || got.Result.Steps[0].Code != protocol.CodeLegacy || got.Result.Steps[0].Detail != "" {
		t.Fatalf("read: %+v %v", got, err)
	}
}
