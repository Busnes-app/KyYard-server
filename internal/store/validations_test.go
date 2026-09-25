package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

var (
	imageD    = "sha256:" + strings.Repeat("d", 64) // planFixture's nginx:1
	imageX    = "sha256:" + strings.Repeat("9", 64) // the update
	priorID   = strings.Repeat("e", 64)
	updatedID = strings.Repeat("f", 64)
)

func tagged(id string, tags ...string) protocol.Image {
	return protocol.Image{ID: id, Tags: tags, Digests: []string{}}
}

// webContainer is the fixture's web container as an inventory reports it.
func webContainer(id, image string, created time.Time) protocol.Container {
	return protocol.Container{ID: id, Name: "shop-web", ImageID: image, State: "running", CreatedAt: created, ComposeProject: "shop", Mounts: []protocol.Mount{}}
}

// putInventory replaces the endpoint's inventory and marks it received and observed now.
func putInventory(t *testing.T, st *SQLStore, endpoint string, containers []protocol.Container, images []protocol.Image) {
	t.Helper()
	raw, _ := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: containers, Images: images, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}})
	now := time.Now().UTC()
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE endpoint_inventory SET snapshot=?,received_at=?,observed_at=? WHERE endpoint_id=?`), string(raw), now, now, endpoint); err != nil {
		t.Fatal(err)
	}
}

// appliedBy names the policy run that applies a fixture deployment; the zero value applies it by hand.
type appliedBy struct{ policy, run string }

// deployFixture plans and applies the latest revision as a with images on the host (through
// ApplyPolicyDeployment when by names a run), runs between (when set) after the apply, and settles
// it succeeded with the web service on container newID running the planned image. The inventory
// then shows newID beside images.
func deployFixture(t *testing.T, st *SQLStore, a TenantAccess, app *Application, endpoint, newID string, images []protocol.Image, by appliedBy, between func(*Deployment)) *Deployment {
	t.Helper()
	ctx := context.Background()
	ts := st.Tenancy()
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := m.Preview.Containers[0]
	putInventory(t, st, endpoint, []protocol.Container{webContainer(c.ID, c.ImageID, c.CreatedAt)}, images)
	if m, err = ts.ReadApplicationMapping(ctx, a, app.ID); err != nil {
		t.Fatal(err)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if by.run == "" {
		_, _, err = ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", imageCheckKey, protocol.MaxDeploymentRequestBytes)
	} else {
		_, _, err = ts.ApplyPolicyDeployment(ctx, a, by.policy, by.run, app.ID, d.ID, "shop", imageCheckKey, protocol.MaxDeploymentRequestBytes)
	}
	if err != nil {
		t.Fatal(err)
	}
	if between != nil {
		between(d)
	}
	created := time.Now().UTC().Truncate(time.Second)
	service := d.Plan.Services[0]
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: service.Name, Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{{Service: service.Name, ContainerID: newID, ImageID: service.ImageID, CreatedUnix: created.Unix()}}}
	if err := ts.SettleDeployment(ctx, endpoint, res); err != nil {
		t.Fatal(err)
	}
	putInventory(t, st, endpoint, []protocol.Container{webContainer(newID, service.ImageID, created)}, images)
	out, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// validationFixture is planFixture with an apply-mode policy and two succeeded applies of
// revision 1: prior, by hand on imageD (its validation finished healthy), then updated, by the
// policy's run on imageX (validation open, in grace). The host keeps imageD, untagged.
func validationFixture(t *testing.T) (*SQLStore, TenantAccess, *Application, string, *UpdatePolicy, string, *Deployment, *Deployment) {
	t.Helper()
	st, a, app, endpoint, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	prior := deployFixture(t, st, a, app, endpoint, priorID, []protocol.Image{tagged(imageD, "nginx:1")}, appliedBy{}, nil)
	if _, err := ts.FinishValidation(ctx, prior.ID, VerdictHealthy, ""); err != nil {
		t.Fatal(err)
	}
	run, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	updated := deployFixture(t, st, a, app, endpoint, updatedID, []protocol.Image{tagged(imageD), tagged(imageX, "nginx:1")}, appliedBy{p.ID, run}, func(d *Deployment) {
		if err := ts.AttachPolicyRunDeployment(ctx, run, d.ID); err != nil {
			t.Fatal(err)
		}
	})
	if err := ts.FinishPolicyRun(ctx, run, RunApplied, updated.ID, ""); err != nil {
		t.Fatal(err)
	}
	return st, a, app, endpoint, p, run, prior, updated
}

func TestDeploymentValidationsTable(t *testing.T) {
	st, _, _, _, _, _, prior, updated := validationFixture(t)
	ctx := context.Background()
	for name, stmt := range map[string]string{
		"phase":             `UPDATE deployment_validations SET phase='waiting' WHERE deployment_id=?`,
		"verdict":           `UPDATE deployment_validations SET verdict='fine' WHERE deployment_id=?`,
		"rollback outcome":  `UPDATE deployment_validations SET rollback_outcome='maybe' WHERE deployment_id=?`,
		"detail":            `UPDATE deployment_validations SET detail='` + strings.Repeat("d", 256) + `' WHERE deployment_id=?`,
		"manual with a run": `UPDATE deployment_validations SET automated=0 WHERE deployment_id=?`,
	} {
		if _, err := st.db.ExecContext(ctx, st.rebind(stmt), updated.ID); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	rollback := uuid.NewString()
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE deployment_validations SET rollback_deployment_id=? WHERE deployment_id=?`), rollback, updated.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE deployment_validations SET rollback_deployment_id=? WHERE deployment_id=?`), rollback, prior.ID); err == nil {
		t.Fatal("two validations named one rollback")
	}
	// The run may go first (its policy deleted): the validation stays, still automated.
	if _, err := st.db.ExecContext(ctx, `DELETE FROM policy_runs`); err != nil {
		t.Fatal(err)
	}
	var automated, orphaned int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT automated,(CASE WHEN policy_run_id IS NULL THEN 1 ELSE 0 END) FROM deployment_validations WHERE deployment_id=?`), updated.ID).Scan(&automated, &orphaned); err != nil || automated != 1 || orphaned != 1 {
		t.Fatalf("after the run went: automated=%d orphaned=%d %v", automated, orphaned, err)
	}
	// A validation goes with its deployment.
	if _, err := st.db.ExecContext(ctx, st.rebind(`DELETE FROM deployments WHERE id=?`), updated.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployment_validations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("validations left: %d %v", n, err)
	}
}

func TestSettleOpensAValidation(t *testing.T) {
	st, a, app, _, _, run, prior, updated := validationFixture(t)
	ctx := context.Background()
	v := updated.Validation
	if v == nil || v.DeploymentID != updated.ID || !v.Automated || v.IsRollback || v.PolicyRunID != run || v.Phase != PhaseGrace || v.Verdict != "" || v.Detail != "" || v.Rollback != nil || v.FinishedAt != nil || v.CorrelationID != updated.CorrelationID {
		t.Fatalf("automated: %+v", v)
	}
	if !v.StartedAt.Equal(*updated.SettledAt) || !v.ObserveUntil.Equal(updated.SettledAt.Add(ValidationGrace+ValidationWindow)) {
		t.Fatalf("window %v..%v, settled %v", v.StartedAt, v.ObserveUntil, *updated.SettledAt)
	}
	got, err := st.Tenancy().ReadDeployment(ctx, a, app.ID, prior.ID)
	if err != nil || got.Validation == nil || got.Validation.Automated || got.Validation.PolicyRunID != "" || got.Validation.Phase != PhaseDone || got.Validation.Verdict != VerdictHealthy || got.Validation.FinishedAt == nil {
		t.Fatalf("manual: %+v %v", got.Validation, err)
	}
	raw, _ := json.Marshal(updated)
	if !strings.Contains(string(raw), `"validation":{`) || strings.Contains(string(raw), "baseline") {
		t.Fatalf("json: %s", raw)
	}
	list, err := st.Tenancy().ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 2 || list[0].Validation == nil || list[1].Validation == nil {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestSettleOpensNoValidationForAFailedApply(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployment_validations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("validations: %d %v", n, err)
	}
	if got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID); err != nil || got.Validation != nil {
		t.Fatalf("failed apply: %+v %v", got, err)
	}
}

// A plan_only run's plan applied by hand is a manual apply: validated, never rolled back.
func TestSettleOfAPlanOnlyRunIsManual(t *testing.T) {
	st, a, app, endpoint, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	in := dailyPolicy()
	in.Mode = PolicyModePlanOnly
	p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	run, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	d := deployFixture(t, st, a, app, endpoint, priorID, []protocol.Image{tagged(imageD, "nginx:1")}, appliedBy{}, func(d *Deployment) {
		if err := ts.FinishPolicyRun(ctx, run, RunPlanned, d.ID, ""); err != nil {
			t.Fatal(err)
		}
	})
	if v := d.Validation; v == nil || v.Automated || v.PolicyRunID != "" {
		t.Fatalf("plan_only applied by hand: %+v", v)
	}
}

// A run that failed after planning names its plan; applied later by hand, the plan is manual:
// automation is a fact of the apply, not of the run.
func TestSettleOfAFailedRunsPlanAppliedByHandIsManual(t *testing.T) {
	st, a, app, endpoint, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	run, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	d := deployFixture(t, st, a, app, endpoint, priorID, []protocol.Image{tagged(imageD, "nginx:1")}, appliedBy{}, func(d *Deployment) {
		if err := ts.FinishPolicyRun(ctx, run, RunFailed, d.ID, "endpoint_offline"); err != nil {
			t.Fatal(err)
		}
	})
	if v := d.Validation; v == nil || v.Automated || v.PolicyRunID != "" {
		t.Fatalf("a failed run's plan applied by hand: %+v", v)
	}
}

// A deployment a validation dispatched is a rollback: validated, never rolled back itself (the
// oscillation guard, Review Focus 2).
func TestSettleOfARollbackNeverRollsBackAgain(t *testing.T) {
	st, a, app, endpoint, _, _, _, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if decide, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil || !decide {
		t.Fatalf("automated failure: decide=%v %v", decide, err)
	}
	back := deployFixture(t, st, a, app, endpoint, strings.Repeat("7", 64), []protocol.Image{tagged(imageD), tagged(imageX, "nginx:1")}, appliedBy{}, func(d *Deployment) {
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, d.ID); err != nil {
			t.Fatal(err)
		}
	})
	if v := back.Validation; v == nil || !v.IsRollback || v.Automated {
		t.Fatalf("rollback: %+v", v)
	}
	if decide, err := ts.FinishValidation(ctx, back.ID, VerdictUnhealthy, "web"); err != nil || decide {
		t.Fatalf("a rollback's failure asked for another rollback: %v %v", decide, err)
	}
	if err := ts.MarkRollbackOutcome(ctx, back.ID, RollbackIneligible, RollbackNoPriorIdentity); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a rollback took a rollback decision: %v", err)
	}
	pending, err := ts.PendingValidations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.DeploymentID == back.ID {
			t.Fatal("a rollback's failure awaits a decision")
		}
	}
}

func TestJudge(t *testing.T) {
	running := func(health string, restarts int) *protocol.ContainerInspection {
		return &protocol.ContainerInspection{State: "running", Health: health, RestartCount: restarts}
	}
	in := func(state string) *protocol.ContainerInspection {
		return &protocol.ContainerInspection{State: state, Health: "none"}
	}
	web := func(presence string, i *protocol.ContainerInspection) Observation {
		return Observation{Service: "web", Presence: presence, Inspection: i}
	}
	db := func(presence string, i *protocol.ContainerInspection) Observation {
		return Observation{Service: "db", Presence: presence, Inspection: i}
	}
	fine := db(PresencePresent, running("healthy", 0))
	base := map[string]ServiceBaseline{"web": {ContainerID: updatedID, RestartCount: 2}, "db": {ContainerID: priorID}}
	for _, tc := range []struct {
		name            string
		obs             []Observation
		final           bool
		verdict, detail string
	}{
		{"healthy before the end goes on", []Observation{web(PresencePresent, running("healthy", 2)), fine}, false, "", ""},
		{"healthy at the end", []Observation{web(PresencePresent, running("healthy", 2)), fine}, true, VerdictHealthy, ""},
		{"no healthcheck at the end", []Observation{web(PresencePresent, running("none", 2)), fine}, true, VerdictHealthy, ""},
		{"unknown presence inspected fine", []Observation{web(PresenceUnknown, running("healthy", 2)), fine}, true, VerdictHealthy, ""},
		{"unhealthy at once", []Observation{web(PresencePresent, running("unhealthy", 2)), fine}, false, VerdictUnhealthy, "web"},
		{"starting goes on", []Observation{web(PresencePresent, running("starting", 2)), fine}, false, "", ""},
		{"starting at the end", []Observation{web(PresencePresent, running("starting", 2)), fine}, true, VerdictUnhealthy, "web"},
		{"exited", []Observation{web(PresencePresent, in("exited")), fine}, false, VerdictExited, "web"},
		{"dead", []Observation{web(PresencePresent, in("dead")), fine}, false, VerdictExited, "web"},
		{"paused", []Observation{web(PresencePresent, in("paused")), fine}, false, VerdictExited, "web"},
		{"gone", []Observation{web(PresenceGone, nil), fine}, false, VerdictExited, "web"},
		{"restarting", []Observation{web(PresencePresent, in("restarting")), fine}, false, VerdictRestarting, "web"},
		{"restarted since the baseline", []Observation{web(PresencePresent, running("healthy", 3)), fine}, false, VerdictRestarting, "web"},
		{"recreated", []Observation{web(PresenceReplaced, nil), fine}, false, VerdictChanged, "web"},
		{"changed outranks a failure", []Observation{web(PresenceReplaced, nil), db(PresencePresent, running("unhealthy", 0))}, false, VerdictChanged, "web"},
		{"exited outranks unhealthy", []Observation{web(PresencePresent, running("unhealthy", 2)), db(PresenceGone, nil)}, false, VerdictExited, "db"},
		{"a failure needs no complete poll", []Observation{web(PresenceUnknown, nil), db(PresencePresent, running("unhealthy", 0))}, true, VerdictUnhealthy, "db"},
		{"an unobserved service holds the end open", []Observation{web(PresencePresent, nil), fine}, true, "", ""},
		{"nothing observed goes on", []Observation{web(PresencePresent, nil), db(PresencePresent, nil)}, false, "", ""},
	} {
		if v, d := Judge(tc.obs, base, tc.final); v != tc.verdict || d != tc.detail {
			t.Errorf("%s: %q %q, want %q %q", tc.name, v, d, tc.verdict, tc.detail)
		}
	}
}

func TestBaselineOf(t *testing.T) {
	in := &protocol.ContainerInspection{Target: protocol.InspectionTarget{ContainerID: updatedID}, State: "running", Health: "healthy", RestartCount: 4}
	got, ok := BaselineOf([]Observation{{Service: "web", Presence: PresencePresent, Inspection: in}, {Service: "db", Presence: PresenceGone}})
	if !ok || len(got) != 1 || got["web"] != (ServiceBaseline{ContainerID: updatedID, RestartCount: 4}) {
		t.Fatalf("baseline: %+v %v", got, ok)
	}
	if _, ok := BaselineOf([]Observation{{Service: "web", Presence: PresenceUnknown}}); ok {
		t.Fatal("a baseline without an observation")
	}
}

func TestPresence(t *testing.T) {
	id := protocol.DeploymentIdentity{Service: "web", ContainerID: updatedID, ImageID: imageX}
	bound := map[string]boundResource{"web": {containerID: updatedID, name: "shop-web"}}
	now := time.Now()
	snap := func(cs ...protocol.Container) *protocol.Snapshot { return &protocol.Snapshot{Containers: cs} }
	for _, tc := range []struct {
		name  string
		bound map[string]boundResource
		snap  *protocol.Snapshot
		want  string
	}{
		{"reported", bound, snap(webContainer(updatedID, imageX, now)), PresencePresent},
		{"no inventory since the settle", bound, nil, PresenceUnknown},
		{"gone", bound, snap(), PresenceGone},
		{"recreated under its name", bound, snap(webContainer(priorID, imageX, now)), PresenceReplaced},
		{"rebound by a later deployment", map[string]boundResource{"web": {containerID: priorID, name: "shop-web"}}, snap(webContainer(updatedID, imageX, now)), PresenceReplaced},
		{"unbound", map[string]boundResource{}, snap(webContainer(updatedID, imageX, now)), PresenceReplaced},
	} {
		if got := presence(id, tc.bound, tc.snap); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestPendingValidations(t *testing.T) {
	st, a, app, endpoint, p, _, _, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	one := func() PendingValidation {
		t.Helper()
		pending, err := ts.PendingValidations(ctx)
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending: %+v %v", pending, err)
		}
		return pending[0]
	}
	got := one()
	if got.DeploymentID != updated.ID || got.OrganizationID != a.OrganizationID || got.EnvironmentID != a.EnvironmentID || got.ApplicationID != app.ID || got.InstanceID != updated.InstanceID || got.EndpointID != endpoint || got.PolicyID != p.ID || got.CreatedBy != "actor" || got.Health || got.Released || got.Baseline != nil || len(got.Services) != 1 {
		t.Fatalf("pending: %+v", got)
	}
	if s := got.Services[0]; s.Service != "web" || s.ContainerID != updatedID || s.ImageID != imageX || s.CreatedUnix != updated.Result.Services[0].CreatedUnix || s.Presence != PresencePresent {
		t.Fatalf("service: %+v", s)
	}
	if err := ts.SetEndpointCapabilities(ctx, endpoint, []string{protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict, protocol.CapabilityContainerInspectHealth, protocol.CapabilityDeploymentApply}); err != nil {
		t.Fatal(err)
	}
	if !one().Health {
		t.Fatal("the health capability was not read")
	}
	images := []protocol.Image{tagged(imageD), tagged(imageX, "nginx:1")}
	for _, tc := range []struct {
		name       string
		containers []protocol.Container
		received   time.Time
		want       string
	}{
		{"inventory from before the settle", []protocol.Container{}, updated.SettledAt.Add(-time.Minute), PresenceUnknown},
		{"gone", []protocol.Container{}, time.Now().UTC(), PresenceGone},
		{"recreated outside KyYard", []protocol.Container{webContainer(strings.Repeat("7", 64), imageX, time.Now().UTC())}, time.Now().UTC(), PresenceReplaced},
	} {
		putInventory(t, st, endpoint, tc.containers, images)
		if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_inventory SET received_at=? WHERE endpoint_id=?`), tc.received, endpoint); err != nil {
			t.Fatal(err)
		}
		if got := one().Services[0].Presence; got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	// Awaiting its rollback decision it stays listed; decided, it leaves.
	if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
		t.Fatal(err)
	}
	if got := one(); got.Phase != PhaseDone || got.Verdict != VerdictUnhealthy {
		t.Fatalf("awaiting a decision: %+v", got)
	}
	if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackIneligible, RollbackPriorImagesMissing); err != nil {
		t.Fatal(err)
	}
	if pending, err := ts.PendingValidations(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("after the decision: %+v %v", pending, err)
	}
}

func TestPendingValidationOfAReleasedApplication(t *testing.T) {
	st, a, app, _, _, _, _, updated := validationFixture(t)
	ctx := context.Background()
	if err := st.Tenancy().ReleaseApplication(ctx, a, app.ID, updated.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	pending, err := st.Tenancy().PendingValidations(ctx)
	if err != nil || len(pending) != 1 || !pending[0].Released {
		t.Fatalf("released: %+v %v", pending, err)
	}
}

func TestBeginObservation(t *testing.T) {
	st, a, app, _, _, _, _, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	baseline := map[string]ServiceBaseline{"web": {ContainerID: updatedID, RestartCount: 2}}
	if err := ts.BeginObservation(ctx, updated.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if err := ts.BeginObservation(ctx, updated.ID, baseline); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second baseline: %v", err)
	}
	if got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID); err != nil || got.Validation.Phase != PhaseObserving {
		t.Fatalf("phase: %+v %v", got.Validation, err)
	}
	pending, err := ts.PendingValidations(ctx)
	if err != nil || len(pending) != 1 || pending[0].Baseline["web"] != baseline["web"] {
		t.Fatalf("baseline: %+v %v", pending, err)
	}
}

func TestFinishValidation(t *testing.T) {
	t.Run("vocabulary and audit", func(t *testing.T) {
		st, _, app, _, _, _, prior, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		for _, bad := range []string{"", "fine"} {
			if _, err := ts.FinishValidation(ctx, updated.ID, bad, ""); !errors.Is(err, ErrInvalid) {
				t.Fatalf("verdict %q: %v", bad, err)
			}
		}
		if _, err := ts.FinishValidation(ctx, prior.ID, VerdictHealthy, ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("finished twice: %v", err)
		}
		var user, result, details, correlation string
		if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT user_id,result,details,correlation_id FROM audit_records WHERE action=? AND resource=?`), AuditValidation, app.ID+"/deployments/"+prior.ID).Scan(&user, &result, &details, &correlation); err != nil || user != "system" || result != "success" || details != VerdictHealthy || correlation != prior.CorrelationID {
			t.Fatalf("audit: %s %s %s %s %v", user, result, details, correlation, err)
		}
	})
	for _, tc := range []struct {
		verdict, detail string
		decide          bool
		reason          string
	}{
		{VerdictHealthy, "", false, ""},
		{VerdictChanged, "web", false, ""},
		{VerdictUnhealthy, "web", true, ""},
		{VerdictExited, "web", true, ""},
		{VerdictRestarting, "web", true, ""},
		{VerdictUnverifiable, ValidationDetailUnobserved, false, ValidationReasonUnverified + ValidationDetailUnobserved},
	} {
		t.Run(tc.verdict, func(t *testing.T) {
			st, a, app, _, p, _, _, updated := validationFixture(t)
			ctx := context.Background()
			ts := st.Tenancy()
			decide, err := ts.FinishValidation(ctx, updated.ID, tc.verdict, tc.detail)
			if err != nil || decide != tc.decide {
				t.Fatalf("decide=%v %v", decide, err)
			}
			got, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID)
			paused := tc.reason != ""
			if err != nil || (got.Status == PolicyPaused) != paused || got.PausedReason != tc.reason {
				t.Fatalf("policy: %+v %v", got, err)
			}
			var rows int
			if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM audit_records WHERE action=? AND resource=? AND user_id='system' AND result='failure' AND correlation_id=?`), AuditPolicyPaused, app.ID+"/policies/"+p.ID, updated.CorrelationID).Scan(&rows); err != nil || (rows == 1) != paused {
				t.Fatalf("pause rows: %d %v", rows, err)
			}
		})
	}
}

func TestMarkRollback(t *testing.T) {
	t.Run("applied", func(t *testing.T) {
		st, a, app, _, _, _, prior, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, uuid.NewString()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("planned before the verdict: %v", err)
		}
		if _, err := ts.FinishValidation(ctx, updated.ID, VerdictExited, "web"); err != nil {
			t.Fatal(err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackApplied, ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("applied with no rollback named: %v", err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, "maybe", ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unknown outcome: %v", err)
		}
		if err := ts.MarkRollbackPlanned(ctx, prior.ID, uuid.NewString()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a manual apply was rolled back: %v", err)
		}
		rollback := uuid.NewString()
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, rollback); err != nil {
			t.Fatal(err)
		}
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, uuid.NewString()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a second rollback: %v", err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackApplied, ""); err != nil {
			t.Fatal(err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackFailed, RollbackNotSent); !errors.Is(err, ErrNotFound) {
			t.Fatalf("decided twice: %v", err)
		}
		got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID)
		if err != nil || got.Validation.Rollback == nil || *got.Validation.Rollback != (ValidationRollback{DeploymentID: rollback, Outcome: RollbackApplied}) {
			t.Fatalf("rollback: %+v %v", got.Validation, err)
		}
		if pol, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || pol.Status != PolicyPaused || pol.PausedReason != ValidationReasonRolledBack {
			t.Fatalf("policy: %+v %v", pol, err)
		}
	})
	t.Run("not after a passing verdict", func(t *testing.T) {
		st, _, _, _, _, _, _, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if _, err := ts.FinishValidation(ctx, updated.ID, VerdictHealthy, ""); err != nil {
			t.Fatal(err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackFailed, RollbackNotSent); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a healthy update took a rollback decision: %v", err)
		}
	})
	t.Run("ineligible", func(t *testing.T) {
		st, a, app, _, _, _, _, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
			t.Fatal(err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackIneligible, RollbackPriorImagesMissing); err != nil {
			t.Fatal(err)
		}
		got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID)
		if err != nil || got.Validation.Rollback == nil || *got.Validation.Rollback != (ValidationRollback{Outcome: RollbackIneligible, Detail: RollbackPriorImagesMissing}) {
			t.Fatalf("rollback: %+v %v", got.Validation, err)
		}
		if pol, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || pol.PausedReason != ValidationReasonNotRolledBack+RollbackPriorImagesMissing {
			t.Fatalf("policy: %+v %v", pol, err)
		}
	})
}

// reconcileAgain runs ReconcileAfterStart a second time and fails if it wrote an audit row.
func reconcileAgain(t *testing.T, st *SQLStore) {
	t.Helper()
	before := countRows(t, st, `SELECT COUNT(*) FROM audit_records`)
	if _, err := st.Tenancy().ReconcileAfterStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := countRows(t, st, `SELECT COUNT(*) FROM audit_records`); after != before {
		t.Fatalf("a second reconcile wrote %d audit rows", after-before)
	}
}

func TestReconcileAfterStartSettlesValidations(t *testing.T) {
	t.Run("a window the server missed", func(t *testing.T) {
		st, a, app, _, _, _, _, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if _, err := ts.ReconcileAfterStart(ctx); err != nil {
			t.Fatal(err)
		}
		if got, _ := ts.ReadDeployment(ctx, a, app.ID, updated.ID); got.Validation.Phase != PhaseGrace {
			t.Fatalf("an open window was settled: %+v", got.Validation)
		}
		past := time.Now().UTC().Add(-time.Hour)
		if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE deployment_validations SET started_at=?,observe_until=? WHERE deployment_id=?`), past, past.Add(ValidationGrace+ValidationWindow), updated.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.ReconcileAfterStart(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID)
		if err != nil || got.Validation.Verdict != VerdictUnverifiable || got.Validation.Detail != ValidationDetailServerDown || got.Validation.Phase != PhaseDone {
			t.Fatalf("missed window: %+v %v", got.Validation, err)
		}
		if pol, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || pol.PausedReason != ValidationReasonUnverified+ValidationDetailServerDown {
			t.Fatalf("policy: %+v %v", pol, err)
		}
		reconcileAgain(t, st)
	})
	t.Run("an interrupted rollback", func(t *testing.T) {
		st, a, app, _, _, _, _, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
			t.Fatal(err)
		}
		rollback := uuid.NewString()
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, rollback); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.ReconcileAfterStart(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID)
		if err != nil || got.Validation.Rollback == nil || *got.Validation.Rollback != (ValidationRollback{DeploymentID: rollback, Outcome: RollbackFailed, Detail: RollbackInterrupted}) {
			t.Fatalf("interrupted: %+v %v", got.Validation, err)
		}
		if pol, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || pol.PausedReason != ValidationReasonNotRolledBack+RollbackInterrupted {
			t.Fatalf("policy: %+v %v", pol, err)
		}
		if pending, err := ts.PendingValidations(ctx); err != nil || len(pending) != 0 {
			t.Fatalf("dispatched again: %+v %v", pending, err)
		}
		reconcileAgain(t, st)
	})
	// Startup shares the live rule: an applying rollback or an unexpired plan waits for its
	// settle or the deadline sweep; an expired plan is interrupted.
	t.Run("a rollback still applying or planned", func(t *testing.T) {
		st, a, app, _, _, _, prior, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
			t.Fatal(err)
		}
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, prior.ID); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			state   string
			expires time.Time
		}{{"applying", time.Now().UTC().Add(-time.Hour)}, {"planned", time.Now().UTC().Add(time.Hour)}} {
			mustExec(t, st, `UPDATE deployments SET state=?,expires_at=? WHERE id=?`, c.state, c.expires, prior.ID)
			if _, err := ts.ReconcileAfterStart(ctx); err != nil {
				t.Fatal(err)
			}
			if got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID); err != nil || got.Validation.Rollback == nil || got.Validation.Rollback.Outcome != "" {
				t.Fatalf("%s decided at startup: %+v %v", c.state, got.Validation, err)
			}
		}
		mustExec(t, st, `UPDATE deployments SET expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), prior.ID)
		if _, err := ts.ReconcileAfterStart(ctx); err != nil {
			t.Fatal(err)
		}
		if got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID); err != nil || got.Validation.Rollback == nil || got.Validation.Rollback.Outcome != RollbackFailed || got.Validation.Rollback.Detail != RollbackInterrupted {
			t.Fatalf("expired plan at startup: %+v %v", got.Validation, err)
		}
		reconcileAgain(t, st)
	})
	// A rollback that settled before the restart was applied, whatever the loop recorded.
	t.Run("a rollback that settled", func(t *testing.T) {
		st, a, app, endpoint, _, _, _, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
			t.Fatal(err)
		}
		back := deployFixture(t, st, a, app, endpoint, strings.Repeat("7", 64), []protocol.Image{tagged(imageD), tagged(imageX, "nginx:1")}, appliedBy{}, func(d *Deployment) {
			if err := ts.MarkRollbackPlanned(ctx, updated.ID, d.ID); err != nil {
				t.Fatal(err)
			}
		})
		if _, err := ts.ReconcileAfterStart(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID)
		if err != nil || got.Validation.Rollback == nil || *got.Validation.Rollback != (ValidationRollback{DeploymentID: back.ID, Revision: back.Revision, Outcome: RollbackApplied}) {
			t.Fatalf("settled rollback: %+v %v", got.Validation, err)
		}
		if pol, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || pol.PausedReason != ValidationReasonRolledBack {
			t.Fatalf("policy: %+v %v", pol, err)
		}
		reconcileAgain(t, st)
	})
}

// A run the restart failed keeps its deployment, so that deployment's late settle is automated.
func TestSettleOfARunFailedByReconcileIsAutomated(t *testing.T) {
	st, a, app, endpoint, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	run, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	d := deployFixture(t, st, a, app, endpoint, priorID, []protocol.Image{tagged(imageD, "nginx:1")}, appliedBy{p.ID, run}, func(d *Deployment) {
		if err := ts.AttachPolicyRunDeployment(ctx, run, d.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.ReconcileAfterStart(ctx); err != nil {
			t.Fatal(err)
		}
		if n := countRows(t, st, `SELECT COUNT(*) FROM policy_runs WHERE id=? AND outcome=? AND deployment_id=?`, run, RunFailed, d.ID); n != 1 {
			t.Fatal("the failed run lost its deployment")
		}
	})
	if v := d.Validation; v == nil || !v.Automated || v.PolicyRunID != run {
		t.Fatalf("settled after the restart: %+v", v)
	}
}

func TestSettleOfARemovalOpensNoValidation(t *testing.T) {
	st, a, app, endpoint, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	d, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, removalResult(d, protocol.OutcomeSucceeded, removalSteps("web", protocol.OutcomeSucceeded, protocol.OutcomeSucceeded, protocol.OutcomeSucceeded))); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM deployment_validations`); n != 0 {
		t.Fatalf("validations: %d", n)
	}
}

// A finished run takes no deployment: the name is written only while the run is open.
func TestAttachPolicyRunDeployment(t *testing.T) {
	st, _, _, _, _, run, _, updated := validationFixture(t)
	if err := st.Tenancy().AttachPolicyRunDeployment(context.Background(), run, updated.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attached to a finished run: %v", err)
	}
}

// The live loop decides a named rollback from its deployment: waiting while it is applying or an
// unexpired plan, applied once it settled succeeded, interrupted when it failed, expired or is gone.
func TestDecideNamedRollbacks(t *testing.T) {
	undecided := func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, updated *Deployment) {
		t.Helper()
		if err := st.Tenancy().DecideNamedRollbacks(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, err := st.Tenancy().ReadDeployment(context.Background(), a, app.ID, updated.ID)
		if err != nil || got.Validation.Rollback == nil || got.Validation.Rollback.Outcome != "" {
			t.Fatalf("decided early: %+v %v", got.Validation, err)
		}
	}
	decided := func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, updated *Deployment, outcome, detail, reason string) {
		t.Helper()
		ctx := context.Background()
		if err := st.Tenancy().DecideNamedRollbacks(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := st.Tenancy().ReadDeployment(ctx, a, app.ID, updated.ID)
		if err != nil || got.Validation.Rollback == nil || got.Validation.Rollback.Outcome != outcome || got.Validation.Rollback.Detail != detail {
			t.Fatalf("decision: %+v %v", got.Validation, err)
		}
		if pol, _, err := st.Tenancy().ReadUpdatePolicy(ctx, a, app.ID); err != nil || pol.Status != PolicyPaused || pol.PausedReason != reason {
			t.Fatalf("policy: %+v %v", pol, err)
		}
		before := countRows(t, st, `SELECT COUNT(*) FROM audit_records`)
		if err := st.Tenancy().DecideNamedRollbacks(ctx); err != nil {
			t.Fatal(err)
		}
		if after := countRows(t, st, `SELECT COUNT(*) FROM audit_records`); after != before {
			t.Fatalf("a second decision wrote %d audit rows", after-before)
		}
	}
	// named points updated's validation at prior, a real deployment row the test then moves.
	named := func(t *testing.T) (*SQLStore, TenantAccess, *Application, *Deployment, *Deployment) {
		t.Helper()
		st, a, app, _, _, _, prior, updated := validationFixture(t)
		if _, err := st.Tenancy().FinishValidation(context.Background(), updated.ID, VerdictUnhealthy, "web"); err != nil {
			t.Fatal(err)
		}
		if err := st.Tenancy().MarkRollbackPlanned(context.Background(), updated.ID, prior.ID); err != nil {
			t.Fatal(err)
		}
		return st, a, app, prior, updated
	}
	set := func(t *testing.T, st *SQLStore, id, state string, expires time.Time) {
		t.Helper()
		if _, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE deployments SET state=?,expires_at=? WHERE id=?`), state, expires, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("settled", func(t *testing.T) {
		st, a, app, _, updated := named(t)
		decided(t, st, a, app, updated, RollbackApplied, "", ValidationReasonRolledBack)
	})
	t.Run("in flight, then failed", func(t *testing.T) {
		st, a, app, prior, updated := named(t)
		set(t, st, prior.ID, "applying", time.Now().UTC().Add(time.Hour))
		undecided(t, st, a, app, updated)
		set(t, st, prior.ID, "failed", time.Now().UTC().Add(time.Hour))
		decided(t, st, a, app, updated, RollbackFailed, RollbackInterrupted, ValidationReasonNotRolledBack+RollbackInterrupted)
	})
	t.Run("planned, then expired", func(t *testing.T) {
		st, a, app, prior, updated := named(t)
		set(t, st, prior.ID, "planned", time.Now().UTC().Add(time.Hour))
		undecided(t, st, a, app, updated)
		set(t, st, prior.ID, "planned", time.Now().UTC().Add(-time.Minute))
		decided(t, st, a, app, updated, RollbackFailed, RollbackInterrupted, ValidationReasonNotRolledBack+RollbackInterrupted)
	})
	t.Run("gone", func(t *testing.T) {
		st, a, app, _, _, _, _, updated := validationFixture(t)
		if _, err := st.Tenancy().FinishValidation(context.Background(), updated.ID, VerdictUnhealthy, "web"); err != nil {
			t.Fatal(err)
		}
		if err := st.Tenancy().MarkRollbackPlanned(context.Background(), updated.ID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		decided(t, st, a, app, updated, RollbackFailed, RollbackInterrupted, ValidationReasonNotRolledBack+RollbackInterrupted)
	})
}
