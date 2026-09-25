package store

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func applyFixture(t *testing.T) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot, *ApplicationMapping, *Deployment, []byte) {
	t.Helper()
	st, a, app, endpoint, snapshot, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	key := imageCheckKey
	// A revision with a secret so apply has something to resolve.
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Restart: "always", Ports: []ApplicationPort{{Target: 80, Published: 8080, Protocol: "tcp"}}, Environment: map[string]ApplicationSecretRef{"TOKEN": {SecretRef: "web-token"}}}}}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, map[string]string{"web-token": "apply-secret-canary"}, key); err != nil {
		t.Fatal(err)
	}
	m2, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m2)); err != nil {
		t.Fatal(err)
	}
	m2, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m2), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	return st, a, app, endpoint, snapshot, m2, d, key
}

func TestApplyDeploymentBuildsTheRequestAndMovesToApplying(t *testing.T) {
	st, a, app, endpoint, snapshot, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	applied, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != "applying" || applied.AppliedBy != a.ActorID || applied.AppliedAt == nil || applied.Deadline == nil || time.Until(*applied.Deadline) > DeploymentApplyDeadline {
		t.Fatalf("row: %+v", applied)
	}
	c := snapshot.Containers[0]
	if req.Deployment != d.ID || req.Endpoint != endpoint || req.Project != "shop" || req.Revision != 2 || len(req.Services) != 1 {
		t.Fatalf("request: %+v", req)
	}
	s := req.Services[0]
	if s.Name != "web" || s.ContainerName != c.Name || s.ImageID != d.Plan.Services[0].ImageID || s.Replaces.ContainerID != c.ID || s.Restart != "always" || len(s.Ports) != 1 || s.Ports[0].Host != 8080 || s.Ports[0].Container != 80 || s.Env["TOKEN"] != "apply-secret-canary" {
		t.Fatalf("service: %+v", s)
	}
	if err := req.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	// The mounts key is always sent: an agent reads its absence as a server older than mounts.
	if frame, err := json.Marshal(req); err != nil || !strings.Contains(string(frame), `"mounts":[]`) {
		t.Fatalf("frame without an empty mounts list: %s %v", frame, err)
	}
	// The row and the audit trail carry no value.
	var stored string
	if err := st.db.QueryRow(st.rebind(`SELECT plan||detail||result FROM deployments WHERE id=?`), d.ID).Scan(&stored); err != nil || strings.Contains(stored, "canary") {
		t.Fatal("secret persisted on the row")
	}
	var leaked int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_records WHERE resource LIKE '%canary%' OR details LIKE '%canary%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("secret in audit: %d %v", leaked, err)
	}
	// Apply twice: the CAS lets one through.
	if _, _, err = ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); !errors.Is(err, ErrDeploymentInProgress) {
		t.Fatalf("second apply: %v", err)
	}
}

func TestApplyDeploymentPreconditions(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		mutate func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, endpoint string, d *Deployment)
		want   error
	}{
		"wrong confirm": {func(*testing.T, *SQLStore, TenantAccess, *Application, string, *Deployment) {}, ErrInvalid},
		"mapping version changed": {func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, _ string, _ *Deployment) {
			m, err := st.Tenancy().ReadApplicationMapping(ctx, a, app.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Tenancy().SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"spec digest mismatch": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, d *Deployment) {
			if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET spec_digest=? WHERE id=?`), strings.Repeat("0", 64), d.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"resource missing": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, d *Deployment) {
			if _, err := st.db.Exec(st.rebind(`DELETE FROM application_resources WHERE instance_id=?`), d.InstanceID); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"instance gone": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, d *Deployment) {
			if _, err := st.db.Exec(st.rebind(`DELETE FROM application_instances WHERE id=?`), d.InstanceID); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"instance moved": {func(t *testing.T, st *SQLStore, a TenantAccess, _ *Application, endpoint string, d *Deployment) {
			if _, err := st.db.Exec(st.rebind(`UPDATE endpoints SET name='host-old' WHERE id=?`), endpoint); err != nil {
				t.Fatal(err)
			}
			other := activeEndpointWith(t, st.Tenancy(), a, nil, nil)
			// Resources are keyed by (instance, endpoint), so they go before the move.
			if _, err := st.db.Exec(st.rebind(`DELETE FROM application_resources WHERE instance_id=?`), d.InstanceID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.Exec(st.rebind(`UPDATE application_instances SET endpoint_id=? WHERE id=?`), other, d.InstanceID); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"expired": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, d *Deployment) {
			if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=? WHERE id=?`), time.Now().Add(-time.Minute), d.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"revision past head": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, d *Deployment) {
			if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET revision=3 WHERE id=?`), d.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"endpoint offline": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, endpoint string, _ *Deployment) {
			if _, err := st.db.Exec(st.rebind(`UPDATE endpoints SET state='offline' WHERE id=?`), endpoint); err != nil {
				t.Fatal(err)
			}
		}, ErrEndpointOffline},
	} {
		t.Run(name, func(t *testing.T) {
			st, a, app, endpoint, _, _, d, key := applyFixture(t)
			tc.mutate(t, st, a, app, endpoint, d)
			confirm := "shop"
			if name == "wrong confirm" {
				confirm = "nope"
			}
			if _, _, err := st.Tenancy().ApplyDeployment(ctx, a, app.ID, d.ID, confirm, key, protocol.MaxDeploymentRequestBytes); !errors.Is(err, tc.want) {
				t.Fatalf("%s: %v", name, err)
			}
			var state string
			if err := st.db.QueryRow(st.rebind(`SELECT state FROM deployments WHERE id=?`), d.ID).Scan(&state); err != nil || state != "planned" {
				t.Fatalf("%s left state %s", name, state)
			}
		})
	}
	// Roles: operator is refused, developer allowed.
	st, a, app, _, _, _, d, key := applyFixture(t)
	if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleOperator, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator: %v", err)
	}
	if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleDeveloper, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatalf("developer: %v", err)
	}
}

// One stored value may feed several variables, and JSON escapes '<' as six bytes, so a
// revision inside every value and per-service cap still marshals past the frame bound. The
// plan refuses it before any row exists.
func TestPlanDeploymentRefusesAnOversizedFrame(t *testing.T) {
	st, a, app, _, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	env := map[string]ApplicationSecretRef{}
	for _, n := range []string{"A", "B", "C", "D", "E", "F"} {
		env["V"+n] = ApplicationSecretRef{SecretRef: "web-a"}
	}
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Environment: env}}}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, map[string]string{"web-a": strings.Repeat("<", 10000)}, imageCheckKey); err != nil {
		t.Fatal(err)
	}
	m, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); !isBlocked(err, "frame_too_large") {
		t.Fatalf("oversized frame: %v", err)
	}
	if n := deploymentRows(t, st, app.ID); n != 0 {
		t.Fatalf("a refused plan left %d rows", n)
	}
}

// An agent without deployment.pull closes its session on a frame past the legacy bound, so the
// endpoint's cap refuses it while the row is still planned. JSON escapes '<' as six bytes: four
// 10000-byte values marshal to about 240 KiB.
func TestApplyDeploymentHonoursTheEndpointFrameCap(t *testing.T) {
	st, a, app, _, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	key := imageCheckKey
	env := map[string]ApplicationSecretRef{}
	for _, n := range []string{"A", "B", "C", "D"} {
		env["V"+n] = ApplicationSecretRef{SecretRef: "web-a"}
	}
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Environment: env}}}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, map[string]string{"web-a": strings.Repeat("<", 10000)}, key); err != nil {
		t.Fatal(err)
	}
	m, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytesLegacy); !errors.Is(err, ErrInvalid) {
		t.Fatalf("legacy cap: %v", err)
	}
	var state string
	if err := st.db.QueryRow(st.rebind(`SELECT state FROM deployments WHERE id=?`), d.ID).Scan(&state); err != nil || state != "planned" {
		t.Fatalf("refused apply left state %s (%v)", state, err)
	}
	_, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(req); len(raw) <= protocol.MaxDeploymentRequestBytesLegacy || len(raw) > protocol.MaxDeploymentRequestBytes {
		t.Fatalf("fixture frame is %d bytes, not between the caps", len(raw))
	}
}

// A frame that never left fails its row even when a disconnect abandoned it first.
func TestFailDeploymentCoversApplyingAndUnknown(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := ts.FailDeployment(ctx, d.ID, "never sent"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := st.db.QueryRow(st.rebind(`SELECT state FROM deployments WHERE id=?`), d.ID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("state %s (%v)", state, err)
	}
}

func settledResult(d *Deployment, outcome string, newID string) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: outcome, Steps: []protocol.DeploymentStep{{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}}, Services: []protocol.DeploymentIdentity{}}
	if newID != "" {
		res.Services = append(res.Services, protocol.DeploymentIdentity{Service: "web", ContainerID: newID, ImageID: d.Plan.Services[0].ImageID, CreatedUnix: 1800000000})
	}
	if outcome != protocol.OutcomeSucceeded {
		res.Code = protocol.ResultStepFailed
		res.Steps[0].Outcome, res.Steps[0].Code = outcome, "runtime_error"
	}
	return res
}

func TestSettleDeploymentRebindsAndAdvances(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	newID := strings.Repeat("e", 64)
	// A result from another endpoint changes nothing.
	if err := ts.SettleDeployment(ctx, "other-endpoint", settledResult(d, protocol.OutcomeSucceeded, newID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign endpoint: %v", err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, newID)); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.State != "succeeded" || got.SettledAt == nil || got.Result == nil || len(got.Result.Services) != 1 {
		t.Fatalf("settled: %+v %v", got, err)
	}
	instances, err := ts.ListApplicationInstances(ctx, a, endpoint)
	if err != nil || len(instances) != 1 || instances[0].CurrentRevision != 2 || instances[0].PreviousRevision != 0 {
		t.Fatalf("revision: %+v %v", instances, err)
	}
	var container, service, name string
	if err := st.db.QueryRow(st.rebind(`SELECT container_id,service_name,name FROM application_resources WHERE instance_id=?`), m.InstanceID).Scan(&container, &service, &name); err != nil || container != newID || service != "web" || name != "shop-web" {
		t.Fatalf("rebind: %s %s %s %v", container, service, name, err)
	}
	// First answer wins on a settled row.
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second settle: %v", err)
	}
	var audits int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE action=? AND resource=? AND user_id=? AND result='success' AND details LIKE '%succeeded%'`), "application.deploy", app.ID+"/deployments/"+d.ID, "agent:"+endpoint).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("settle audit: %d %v", audits, err)
	}
}

func TestSettleDeploymentFromUnknownAndAbandon(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	n, err := ts.AbandonDeployments(ctx, endpoint)
	if err != nil || n != 1 {
		t.Fatalf("abandon: %d %v", n, err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "unknown" || got.Detail == "" {
		t.Fatalf("abandoned: %+v", got)
	}
	// A late result still settles an unknown row, but a failed run does not advance the revision.
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); err != nil {
		t.Fatal(err)
	}
	got, _ = ts.ReadDeployment(ctx, a, app.ID, d.ID)
	instances, _ := ts.ListApplicationInstances(ctx, a, endpoint)
	if got.State != "failed" || instances[0].CurrentRevision != 0 {
		t.Fatalf("late settle: %+v %+v", got, instances)
	}
}

func TestPruneSweepsStaleApplying(t *testing.T) {
	st, a, app, _, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET deadline=? WHERE id=?`), time.Now().Add(-3*time.Minute), d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "unknown" {
		t.Fatalf("sweep: %+v", got)
	}
}

func TestSettleRebindRefusesMissingResource(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`DELETE FROM application_resources WHERE instance_id=?`), m.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatalf("rebind without resource: %v", err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "applying" {
		t.Fatalf("half-written: %+v", got)
	}
}

// A newer plan was built against the containers the late answer replaces: the answer settles
// and the plan expires.
func TestLateResultSettlesPastANewerPlan(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	newer, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	newID := strings.Repeat("e", 64)
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, newID)); err != nil {
		t.Fatalf("late settle after a newer plan: %v", err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	instances, _ := ts.ListApplicationInstances(ctx, a, endpoint)
	var container string
	if err := st.db.QueryRow(st.rebind(`SELECT container_id FROM application_resources WHERE instance_id=?`), m.InstanceID).Scan(&container); err != nil || container != newID {
		t.Fatalf("resources not rebound: %s %v", container, err)
	}
	if got.State != "succeeded" || instances[0].CurrentRevision != 2 {
		t.Fatalf("late settle: %+v %+v", got, instances)
	}
	plan, _ := ts.ReadDeployment(ctx, a, app.ID, newer.ID)
	if plan.State != "planned" || !plan.Expired {
		t.Fatalf("newer plan still live: %+v", plan)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, newer.ID, "shop", key, protocol.MaxDeploymentRequestBytes); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatalf("apply of the expired plan: %v", err)
	}
}

// A newer row that has been applied is state someone acted on: the late answer changes nothing.
func TestLateResultAfterNewerApplyIsIgnored(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	newer, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, newer.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late settle after a newer apply: %v", err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	current, _ := ts.ReadDeployment(ctx, a, app.ID, newer.ID)
	instances, _ := ts.ListApplicationInstances(ctx, a, endpoint)
	var container string
	if err := st.db.QueryRow(st.rebind(`SELECT container_id FROM application_resources WHERE instance_id=?`), m.InstanceID).Scan(&container); err != nil || container != d.Plan.Services[0].ContainerID {
		t.Fatalf("resources moved: %s %v", container, err)
	}
	if got.State != "unknown" || current.State != "applying" || instances[0].CurrentRevision != 0 {
		t.Fatalf("late settle applied: %+v %+v %+v", got, current, instances)
	}
}

// A result that does not fit the plan leaves the row unknown with the caller's detail and an
// audit row; a settled row or another endpoint's row is not touched.
func TestRefuseDeploymentResult(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if err := ts.RefuseDeploymentResult(ctx, "other-endpoint", d.ID, "refused"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign endpoint: %v", err)
	}
	if err := ts.RefuseDeploymentResult(ctx, endpoint, d.ID, "the host's result did not match the plan; inspect the host"); err != nil {
		t.Fatal(err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "unknown" || got.Detail != "the host's result did not match the plan; inspect the host" {
		t.Fatalf("refused: %+v", got)
	}
	// A re-sent mismatch changes nothing and writes no second audit row.
	if err := ts.RefuseDeploymentResult(ctx, endpoint, d.ID, "the host's result did not match the plan; inspect the host"); err != nil {
		t.Fatal(err)
	}
	var audits int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE action=? AND resource=? AND user_id=? AND result='unknown' AND details='outcome=refused'`), "application.deploy", app.ID+"/deployments/"+d.ID, "agent:"+endpoint).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("refuse audit: %d %v", audits, err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); err != nil {
		t.Fatal(err)
	}
	if err := ts.RefuseDeploymentResult(ctx, endpoint, d.ID, "refused"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("settled row: %v", err)
	}
	if got, _ = ts.ReadDeployment(ctx, a, app.ID, d.ID); got.State != "failed" {
		t.Fatalf("settled row rewritten: %+v", got)
	}
}

// A refusal follows the settle rule: a row superseded by a newer applied one is left alone.
func TestRefuseSupersededDeploymentResult(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	newer, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, newer.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	before, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err := ts.RefuseDeploymentResult(ctx, endpoint, d.ID, "refused"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded row: %v", err)
	}
	after, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	var audits int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE details='outcome=refused'`)).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("refuse audit: %d %v", audits, err)
	}
	if after.State != "unknown" || after.Detail != before.Detail {
		t.Fatalf("superseded row rewritten: %+v", after)
	}
}

func TestLateResultAfterReleaseIsIgnored(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late settle after release: %v", err)
	}
	var resources int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM application_resources WHERE instance_id=?`), m.InstanceID).Scan(&resources); err != nil || resources != 0 {
		t.Fatalf("late settle resurrected resources: %d %v", resources, err)
	}
}

func TestSettleDeploymentRefusesIncompleteOrWrongIdentities(t *testing.T) {
	for name, mutate := range map[string]func(*protocol.DeploymentResult){
		"succeeded without every service": func(r *protocol.DeploymentResult) { r.Services = r.Services[:0] },
		"unpinned image":                  func(r *protocol.DeploymentResult) { r.Services[0].ImageID = "sha256:" + strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			st, a, app, endpoint, _, m, d, key := applyFixture(t)
			ctx := context.Background()
			ts := st.Tenancy()
			if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
				t.Fatal(err)
			}
			res := settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))
			mutate(&res)
			if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s: %v", name, err)
			}
			got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
			var container string
			if err := st.db.QueryRow(st.rebind(`SELECT container_id FROM application_resources WHERE instance_id=?`), m.InstanceID).Scan(&container); err != nil || container != d.Plan.Services[0].ContainerID || got.State != "applying" {
				t.Fatalf("%s wrote: %s %+v %v", name, container, got, err)
			}
		})
	}
}

func TestConcurrentSettlesCommitOnce(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	if st.driver != "postgres" {
		t.Skip("concurrent settles need real row locks")
	}
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	// A success must name every planned service, so it carries the identity; the failure none.
	results := []protocol.DeploymentResult{settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64)), settledResult(d, protocol.OutcomeFailed, "")}
	// Hold the row so both settles are in flight and queued before either can commit; otherwise
	// the first usually finishes before the second starts and the race never runs. The first
	// waits on the deployment row, the second on the application row the first holds.
	holder, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	var held string
	if err := holder.QueryRowContext(ctx, st.rebind(`SELECT id FROM deployments WHERE id=? FOR UPDATE`), d.ID).Scan(&held); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, res := range results {
		go func() { errs <- ts.SettleDeployment(ctx, endpoint, res) }()
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND (query LIKE '%FROM deployments%' OR query LIKE '%FROM applications%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settles never queued on the row: %d waiting", waiting)
		}
		runtime.Gosched()
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	var ok, notFound int
	for range results {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrNotFound):
			notFound++
		default:
			t.Fatalf("settle: %v", err)
		}
	}
	if ok != 1 || notFound != 1 {
		t.Fatalf("settles: %d committed, %d refused", ok, notFound)
	}
	var audits int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE user_id=? AND resource=?`), "agent:"+endpoint, app.ID+"/deployments/"+d.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit rows: %d %v", audits, err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	instances, _ := ts.ListApplicationInstances(ctx, a, endpoint)
	want := 0
	if got.State == "succeeded" {
		want = 2
	}
	if instances[0].CurrentRevision != want || instances[0].PreviousRevision != 0 {
		t.Fatalf("state %s moved revisions: %+v", got.State, instances[0])
	}
}

func TestSettleDeploymentRefusesOversizedResult(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	res := settledResult(d, protocol.OutcomeFailed, "")
	// Each parameter is twelve unsupported codes (148 bytes), under the per-step cap, so the result
	// validates and only the stored byte cap refuses it.
	detail := strings.Join(protocol.UnsupportedCodes[:12], ",")
	for len(res.Steps) < 8*protocol.MaxDeploymentServices {
		res.Steps = append(res.Steps, protocol.DeploymentStep{Service: "web", Step: protocol.StepStart, Outcome: protocol.OutcomeFailed, Code: "unsupported", Detail: detail})
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized result: %v", err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "applying" {
		t.Fatalf("oversized result settled: %+v", got)
	}
}

func TestPlanAndReleaseNeverLeaveALivePlanForAReleasedInstance(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	if st.driver != "postgres" {
		t.Skip("interleaving needs real row locks")
	}
	ctx := context.Background()
	ts := st.Tenancy()
	var planFirst, releaseFirst int
	for round := 0; round < 40; round++ {
		// Re-adopt if the previous round released.
		if _, err := ts.ReadApplicationMapping(ctx, a, app.ID); errors.Is(err, ErrNotFound) {
			p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, m.Preview.EndpointID, "shop")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: m.Preview.EndpointID, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
				t.Fatal(err)
			}
			m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
			if err = ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
				t.Fatal(err)
			}
			m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
		}
		// Expire the previous round's plan so release can win this one.
		if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=? WHERE instance_id=? AND state='planned'`), time.Now().UTC().Add(-time.Minute), m.InstanceID); err != nil {
			t.Fatal(err)
		}
		var planErr, releaseErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(rand.IntN(2000)) * time.Microsecond)
			_, planErr = ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(rand.IntN(2000)) * time.Microsecond)
			releaseErr = ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop")
		}()
		wg.Wait()
		for _, err := range []error{planErr, releaseErr} {
			if err != nil && !errors.Is(err, ErrDeploymentPlanned) && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrAdoptionChanged) {
				t.Fatalf("round %d: unexpected error: %v", round, err)
			}
		}
		switch {
		case planErr == nil && releaseErr != nil:
			planFirst++
		case releaseErr == nil && planErr != nil:
			releaseFirst++
		default:
			t.Fatalf("round %d: plan %v, release %v", round, planErr, releaseErr)
		}
		var live int
		if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM deployments d WHERE d.state='planned' AND d.expires_at>? AND NOT EXISTS (SELECT 1 FROM application_instances i WHERE i.id=d.instance_id)`), time.Now().UTC()).Scan(&live); err != nil || live != 0 {
			t.Fatalf("round %d: live plan for a released instance: %d %v", round, live, err)
		}
	}
	t.Logf("plan first %d, release first %d", planFirst, releaseFirst)
	if planFirst == 0 || releaseFirst == 0 {
		t.Fatalf("one ordering never ran: plan first %d, release first %d", planFirst, releaseFirst)
	}
}

// pulledApply plans an update of every service and applies it.
func pulledApply(t *testing.T, services []ApplicationService, digests map[string][]string, reply map[string]fakeReply) (*SQLStore, TenantAccess, *Application, string, *Deployment, *protocol.DeploymentRequest) {
	t.Helper()
	st, a, app, endpoint, _ := pullFixture(t, services, digests)
	ctx := context.Background()
	ts := st.Tenancy()
	setAnonymousPull(t, st, a, true)
	names := []string{}
	for _, s := range services {
		names = append(names, s.Name)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, names...), &fakeResolver{reply: reply}, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	applied, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", imageCheckKey, protocol.MaxDeploymentRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	return st, a, app, endpoint, applied, req
}

func TestApplyDeploymentCarriesThePullAndItsCredential(t *testing.T) {
	services := []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}, {Name: "api", Image: "ghcr.io/org/api:1"}, {Name: "db", Image: "postgres:16"}}
	digests := map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}, "api": {"ghcr.io/org/api@" + digestOf("b")}, "db": {"postgres@" + digestOf("c")}}
	reply := map[string]fakeReply{"ghcr.io/org/web:1.2": {digest: digestOf("1")}, "ghcr.io/org/api:1": {digest: digestOf("2")}, "docker.io/library/postgres:16": {digest: digestOf("3")}}
	st, _, _, _, d, req := pulledApply(t, services, digests, reply)
	want := map[string]string{"web": "ghcr.io/org/web@" + digestOf("1"), "api": "ghcr.io/org/api@" + digestOf("2"), "db": "docker.io/library/postgres@" + digestOf("3")}
	// The tag the agent moves: the spec reference, canonical.
	tags := map[string]string{"web": "ghcr.io/org/web:1.2", "api": "ghcr.io/org/api:1", "db": "docker.io/library/postgres:16"}
	for _, s := range req.Services {
		if s.Pull == nil || s.Pull.Reference != want[s.Name] || s.Pull.Tag != tags[s.Name] || !strings.HasSuffix(s.Pull.Reference, "@"+s.Pull.Digest) || s.ImageID != "" {
			t.Fatalf("%s: %+v %+v", s.Name, s, s.Pull)
		}
	}
	// One entry for the host both ghcr.io services pull from; none for the anonymous one.
	if len(req.Registries) != 1 || req.Registries["ghcr.io"] != (protocol.RegistryAuth{Username: "bot", Secret: pullCanary}) {
		t.Fatalf("registries: %+v", req.Registries)
	}
	if err := req.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := st.db.QueryRow(st.rebind(`SELECT plan||detail||result FROM deployments WHERE id=?`), d.ID).Scan(&stored); err != nil || strings.Contains(stored, pullCanary) || strings.Contains(stored, "registries") {
		t.Fatalf("credential persisted on the row: %v", err)
	}
	var leaked int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_records WHERE resource LIKE '%canary%' OR details LIKE '%canary%' OR details LIKE '%registries%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("credential in audit: %d %v", leaked, err)
	}
}

func TestApplyDeploymentSendsNoCredentialForAnAnonymousPull(t *testing.T) {
	_, _, _, _, _, req := pulledApply(t, []ApplicationService{{Name: "db", Image: "postgres:16"}}, map[string][]string{"db": {"postgres@" + digestOf("c")}}, map[string]fakeReply{"docker.io/library/postgres:16": {digest: digestOf("3")}})
	if req.Registries != nil || req.Services[0].Pull == nil {
		t.Fatalf("request: %+v", req)
	}
	if raw, _ := json.Marshal(req); !strings.Contains(string(raw), `"tag":"docker.io/library/postgres:16"`) {
		t.Fatalf("frame carries no tag: %s", raw)
	}
}

// A digest-pinned spec reference names no tag, so the frame asks for none.
func TestApplyDeploymentSendsNoTagForADigestPinnedReference(t *testing.T) {
	image := "ghcr.io/org/web@" + digestOf("5")
	_, _, _, _, _, req := pulledApply(t, []ApplicationService{{Name: "web", Image: image}}, map[string][]string{"web": {image}}, nil)
	if p := req.Services[0].Pull; p == nil || p.Reference != image || p.Tag != "" {
		t.Fatalf("pull: %+v", p)
	}
	if raw, _ := json.Marshal(req); strings.Contains(string(raw), `"tag"`) {
		t.Fatalf("frame carries a tag: %s", raw)
	}
}

// A result mixing a pulled and a pinned service is checked per service: each must carry its
// own identity, and swapping them is refused.
func TestSettleDeploymentVerifiesAMixedResult(t *testing.T) {
	services := []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}, {Name: "db", Image: "postgres:16"}}
	digests := map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}, "db": {"postgres@" + digestOf("c")}}
	st, a, app, endpoint, _ := pullFixture(t, services, digests)
	ctx := context.Background()
	ts := st.Tenancy()
	d, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "web"), &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1.2": {digest: digestOf("1")}}}, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	web, db := d.Plan.Services[0], d.Plan.Services[1]
	if web.Name != "web" || web.PullDigest != digestOf("1") || db.PullDigest != "" {
		t.Fatalf("plan: %+v", d.Plan.Services)
	}
	pulledImage := "sha256:" + strings.Repeat("9", 64)
	result := func(webImage, webDigest, dbImage, dbDigest string) protocol.DeploymentResult {
		return protocol.DeploymentResult{Deployment: d.ID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{
			{Service: "web", ContainerID: strings.Repeat("e", 64), ImageID: webImage, ImageDigest: webDigest, CreatedUnix: 1800000000},
			{Service: "db", ContainerID: strings.Repeat("f", 64), ImageID: dbImage, ImageDigest: dbDigest, CreatedUnix: 1800000000},
		}}
	}
	for name, res := range map[string]protocol.DeploymentResult{
		"db reports web's digest":      result(pulledImage, web.PullDigest, pulledImage, web.PullDigest),
		"web reports db's pinned ID":   result(db.ImageID, "", db.ImageID, ""),
		"db reports a new ID":          result(pulledImage, web.PullDigest, pulledImage, ""),
		"web reports no digest at all": result(pulledImage, "", db.ImageID, ""),
	} {
		if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
		var state string
		if err := st.db.QueryRow(st.rebind(`SELECT state FROM deployments WHERE id=?`), d.ID).Scan(&state); err != nil || state != "applying" {
			t.Fatalf("%s: state %s %v", name, state, err)
		}
	}
	if err := ts.SettleDeployment(ctx, endpoint, result(pulledImage, web.PullDigest, db.ImageID, "")); err != nil {
		t.Fatal(err)
	}
	for service, want := range map[string]string{"web": pulledImage, "db": db.ImageID} {
		var image string
		if err := st.db.QueryRow(st.rebind(`SELECT image_id FROM application_resources WHERE instance_id=? AND service_name=?`), d.InstanceID, service).Scan(&image); err != nil || image != want {
			t.Fatalf("%s image: %s %v", service, image, err)
		}
	}
}

func TestSettleDeploymentVerifiesThePull(t *testing.T) {
	st, _, _, endpoint, d, _ := pulledApply(t, []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}}, map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}}, map[string]fakeReply{"ghcr.io/org/web:1.2": {digest: digestOf("1")}})
	ctx := context.Background()
	ts := st.Tenancy()
	ps := d.Plan.Services[0]
	newImage := "sha256:" + strings.Repeat("9", 64)
	result := func(imageID, digest string) protocol.DeploymentResult {
		res := settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))
		res.Services[0].ImageID, res.Services[0].ImageDigest = imageID, digest
		return res
	}
	for name, res := range map[string]protocol.DeploymentResult{
		"old image, no digest": result(ps.ImageID, ""),
		"other digest":         result(newImage, digestOf("2")),
		"new image, no digest": result(newImage, ""),
	} {
		if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
		var state string
		if err := st.db.QueryRow(st.rebind(`SELECT state FROM deployments WHERE id=?`), d.ID).Scan(&state); err != nil || state != "applying" {
			t.Fatalf("%s: state %s %v", name, state, err)
		}
	}
	if err := ts.SettleDeployment(ctx, endpoint, result(newImage, ps.PullDigest)); err != nil {
		t.Fatal(err)
	}
	var image string
	if err := st.db.QueryRow(st.rebind(`SELECT image_id FROM application_resources WHERE instance_id=? AND service_name='web'`), d.InstanceID).Scan(&image); err != nil || image != newImage {
		t.Fatalf("resource image: %s %v", image, err)
	}
}

// Turning anonymous pull off between plan and apply stops a pull from an unconfigured host.
func TestApplyDeploymentRechecksTheAnonymousPullPolicy(t *testing.T) {
	st, a, app, _, _ := pullFixture(t, []ApplicationService{{Name: "db", Image: "postgres:16"}}, map[string][]string{"db": {"postgres@" + digestOf("c")}})
	ctx := context.Background()
	ts := st.Tenancy()
	setAnonymousPull(t, st, a, true)
	d, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "db"), &fakeResolver{reply: map[string]fakeReply{"docker.io/library/postgres:16": {digest: digestOf("3")}}}, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	setAnonymousPull(t, st, a, false)
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", imageCheckKey, protocol.MaxDeploymentRequestBytes); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatalf("apply with anonymous pull off: %v", err)
	}
	var state string
	if err := st.db.QueryRow(st.rebind(`SELECT state FROM deployments WHERE id=?`), d.ID).Scan(&state); err != nil || state != "planned" {
		t.Fatalf("state %s %v", state, err)
	}
}

// The frame's mounts and volumes are the plan's, unchanged.
func TestApplyDeploymentCarriesMountsAndVolumes(t *testing.T) {
	webBind := protocol.Mount{Kind: protocol.MountBind, Source: "/srv/web", Target: "/srv", ReadOnly: true}
	st, a, app, m := volumesFixture(t, volumesSpec(), map[string][]protocol.Mount{"web": {webBind}})
	ctx := context.Background()
	ts := st.Tenancy()
	key := imageCheckKey
	d := planWithValues(t, st, a, app, m, volumesSpec(), key)
	_, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Services) != 2 || !slices.Equal(req.Services[0].Mounts, d.Plan.Services[0].Mounts) || !slices.Equal(req.Services[1].Mounts, d.Plan.Services[1].Mounts) || !slices.Equal(req.Volumes, []string{"shop_data", "shared"}) {
		t.Fatalf("frame: %+v", req)
	}
	if err := req.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
}

// Spec and plan caps keep binds far below the agent's frame bound, so the frame check stays
// the guard: a frame of long binds past the endpoint's cap is refused while the row is planned.
func TestApplyDeploymentRefusesAFrameOfLongBinds(t *testing.T) {
	var volumes []ApplicationVolume
	var mounts []protocol.Mount
	for i := range protocol.MaxMounts {
		p := "/" + strings.Repeat(string(rune('a'+i%26)), 200) + "/" + string(rune('a'+i/26))
		volumes = append(volumes, ApplicationVolume{Kind: "bind", Source: p, Target: p})
		mounts = append(mounts, protocol.Mount{Kind: protocol.MountBind, Source: p, Target: p})
	}
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Volumes: volumes}}}
	st, a, app, m := volumesFixture(t, spec, map[string][]protocol.Mount{"web": mounts})
	ctx := context.Background()
	ts := st.Tenancy()
	key := imageCheckKey
	d := planWithValues(t, st, a, app, m, spec, key)
	const limit = 12 << 10
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, limit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized frame: %v", err)
	}
	var state string
	if err := st.db.QueryRow(st.rebind(`SELECT state FROM deployments WHERE id=?`), d.ID).Scan(&state); err != nil || state != "planned" {
		t.Fatalf("refused apply left state %s (%v)", state, err)
	}
	_, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(req); len(raw) <= limit {
		t.Fatalf("fixture frame is %d bytes, not past the cap", len(raw))
	}
}

// planWithValues saves spec as a revision with its (empty) value bundle, remaps it to the
// containers m binds and plans it: apply resolves values, which a created revision lacks.
func planWithValues(t *testing.T, st *SQLStore, a TenantAccess, app *Application, m *ApplicationMapping, spec ApplicationSpec, key []byte) *Deployment {
	t.Helper()
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, m.Preview.Revision, spec, nil, key); err != nil {
		t.Fatal(err)
	}
	next, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := mappingRequest(next)
	r.Bindings = m.Bindings
	if err := ts.SetApplicationMapping(ctx, a, app.ID, r); err != nil {
		t.Fatal(err)
	}
	if next, err = ts.ReadApplicationMapping(ctx, a, app.ID); err != nil {
		t.Fatal(err)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(next), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
