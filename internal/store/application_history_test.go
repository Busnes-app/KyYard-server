package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

func removalSteps(service string, outcomes ...string) []protocol.DeploymentStep {
	out := []protocol.DeploymentStep{}
	for i, step := range []string{protocol.StepPrecondition, protocol.StepStop, protocol.StepRemove} {
		s := protocol.DeploymentStep{Service: service, Step: step, Outcome: outcomes[i]}
		if s.Outcome != protocol.OutcomeSucceeded && s.Outcome != protocol.OutcomeSkipped {
			s.Detail = "fixed text"
		}
		out = append(out, s)
	}
	return out
}

func removalResult(d *Deployment, outcome string, steps ...[]protocol.DeploymentStep) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: d.ID, Outcome: outcome, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	for _, s := range steps {
		res.Steps = append(res.Steps, s...)
	}
	return res
}

func countRows(t *testing.T, st *SQLStore, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(st.rebind(query), args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPlanPriorRevision(t *testing.T) {
	st, a, app, _, _, _, _, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	// Revision 3 adds a service; the mapping is reviewed against it and binds only web.
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Environment: map[string]ApplicationSecretRef{"TOKEN": {SecretRef: "web-token"}}}, {Name: "api", Image: "nginx:1"}}}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 2, spec, map[string]string{"web-token": "revision-three"}, key); err != nil {
		t.Fatal(err)
	}
	m, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	var blocked *PreflightBlockedError
	for _, rev := range []int{3, 0} {
		r := planRequest(m)
		r.Revision = rev
		if _, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, nil, false); !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, "service_unmapped") {
			t.Fatalf("revision %d: %v", rev, err)
		}
	}
	if p, err := ts.PreflightApplication(ctx, a, app.ID); err != nil || p.Revision != 3 {
		t.Fatalf("preflight latest: %+v %v", p, err)
	}
	r := planRequest(m)
	r.Revision = 4
	if _, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, nil, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing revision: %v", err)
	}
	r.Revision = 2
	d, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, nil, false)
	if err != nil || d.Revision != 2 || len(d.Plan.Services) != 1 {
		t.Fatalf("prior revision: %+v %v", d, err)
	}
	rev2, err := ts.ReadApplicationRevision(ctx, a, app.ID, 2)
	if err != nil || d.SpecDigest != rev2.Digest {
		t.Fatalf("digest: %v", err)
	}
	_, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key)
	if err != nil || req.Revision != 2 || req.Services[0].Env["TOKEN"] != "apply-secret-canary" {
		t.Fatalf("apply prior: %+v %v", req, err)
	}
	// A mapped service the chosen revision does not define blocks it.
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET state='failed',settled_at=? WHERE id=?`), time.Now().UTC(), d.ID); err != nil {
		t.Fatal(err)
	}
	mr := mappingRequest(m)
	mr.Bindings = map[string]string{"api": m.Preview.Containers[0].ID}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mr); err != nil {
		t.Fatal(err)
	}
	m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	r = planRequest(m)
	r.Revision = 2
	if _, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, nil, false); !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{"revision_services_differ"}) {
		t.Fatalf("stray binding: %v", err)
	}
	// A prior revision defining a service the mapping lacks is blocked the same way.
	webOnly := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Environment: map[string]ApplicationSecretRef{"TOKEN": {SecretRef: "web-token"}}}}}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 3, webOnly, map[string]string{"web-token": "revision-four"}, key); err != nil {
		t.Fatal(err)
	}
	m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	r = planRequest(m)
	r.Revision = 3
	if _, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, nil, false); !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{"revision_services_differ"}) {
		t.Fatalf("unmapped service in a prior revision: %v", err)
	}
}

func TestRemoveApplicationBuildsARemovalAndSettles(t *testing.T) {
	st, a, app, endpoint, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	d, req, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	c := snapshot.Containers[0]
	if d.Kind != "remove" || d.State != "applying" || d.Revision != 1 || d.AppliedBy != a.ActorID || d.Deadline == nil || len(d.Plan.Services) != 0 || len(d.Plan.Containers) != 1 {
		t.Fatalf("row: %+v", d)
	}
	pc := d.Plan.Containers[0]
	if pc.Service != "web" || pc.Name != "shop-web" || pc.ContainerID != c.ID || pc.ImageID != c.ImageID || pc.CreatedUnix != c.CreatedAt.Unix() {
		t.Fatalf("plan target: %+v", pc)
	}
	if err := req.Validate(time.Now()); err != nil || req.Deployment != d.ID || req.Endpoint != endpoint || req.Project != "shop" || len(req.Containers) != 1 || req.Containers[0].Target.ContainerID != c.ID || req.Containers[0].Service != "web" {
		t.Fatalf("request: %+v %v", req, err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.Kind != "remove" || got.EndpointName != "host" || got.Plan.Services == nil {
		t.Fatalf("read: %+v %v", got, err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM audit_records WHERE action='application.destroy' AND user_id=? AND resource=? AND result='success' AND details=?`, a.ActorID, app.ID+"/deployments/"+d.ID, "project=shop endpoint="+endpoint+" containers=1"); n != 1 {
		t.Fatalf("removal audit: %d", n)
	}
	res := removalResult(d, protocol.OutcomeSucceeded, removalSteps("web", protocol.OutcomeSucceeded, protocol.OutcomeSucceeded, protocol.OutcomeSucceeded))
	if err := ts.SettleDeployment(ctx, endpoint, res); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM application_resources WHERE instance_id=?`, m.InstanceID); n != 0 {
		t.Fatalf("resources left: %d", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM application_instances WHERE application_id=?`, app.ID); n != 0 {
		t.Fatalf("instance left: %d", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM audit_records WHERE action='application.destroy' AND user_id=? AND resource=? AND result='success'`, "agent:"+endpoint, app.ID+"/deployments/"+d.ID); n != 1 {
		t.Fatalf("settle audit: %d", n)
	}
	got, err = ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.State != "succeeded" {
		t.Fatalf("settled: %+v %v", got, err)
	}
	apps, err := ts.ListApplications(ctx, a, 0, 10)
	if err != nil || len(apps) != 1 || apps[0].RemovedAt == nil {
		t.Fatalf("list: %+v %v", apps, err)
	}
	// Removed means released: planning has no instance, discard is allowed.
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, nil, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("plan after removal: %v", err)
	}
	if err := ts.DiscardApplication(ctx, a, app.ID, 1); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM deployments WHERE application_id=?`, app.ID); n != 0 {
		t.Fatalf("history after discard: %d", n)
	}
}

func TestRemovalPreconditions(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		mutate func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, endpoint string, m *ApplicationMapping) RemovalBody
		want   error
	}{
		"wrong confirm": {func(_ *testing.T, _ *SQLStore, _ TenantAccess, _ *Application, _ string, m *ApplicationMapping) RemovalBody {
			return RemovalBody{InstanceID: m.InstanceID, Confirm: "nope"}
		}, ErrInvalid},
		"wrong instance": {func(*testing.T, *SQLStore, TenantAccess, *Application, string, *ApplicationMapping) RemovalBody {
			return RemovalBody{InstanceID: uuid.NewString(), Confirm: "shop"}
		}, ErrAdoptionChanged},
		"offline": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, endpoint string, m *ApplicationMapping) RemovalBody {
			if _, err := st.db.Exec(st.rebind(`UPDATE endpoints SET state='offline' WHERE id=?`), endpoint); err != nil {
				t.Fatal(err)
			}
			return RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"}
		}, ErrEndpointOffline},
		"live plan": {func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, _ string, m *ApplicationMapping) RemovalBody {
			if _, err := st.Tenancy().PlanDeployment(ctx, a, app.ID, planRequest(m), nil, nil, false); err != nil {
				t.Fatal(err)
			}
			return RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"}
		}, ErrDeploymentPlanned},
		"applying": {func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, _ string, m *ApplicationMapping) RemovalBody {
			if _, err := st.Tenancy().PlanDeployment(ctx, a, app.ID, planRequest(m), nil, nil, false); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.Exec(`UPDATE deployments SET state='applying'`); err != nil {
				t.Fatal(err)
			}
			return RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"}
		}, ErrDeploymentInProgress},
		"too many resources": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, endpoint string, m *ApplicationMapping) RemovalBody {
			for i := 1; i <= protocol.MaxRemovalTargets; i++ {
				if _, err := st.db.Exec(st.rebind(`INSERT INTO application_resources(instance_id,endpoint_id,container_id,name,image_id,created_at) VALUES(?,?,?,?,?,?)`), m.InstanceID, endpoint, fmt.Sprintf("%064x", i), fmt.Sprintf("shop-%d", i), "sha256:"+strings.Repeat("b", 64), time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			return RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"}
		}, ErrRemovalTooLarge},
		"stale inventory": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, endpoint string, m *ApplicationMapping) RemovalBody {
			if _, err := st.db.Exec(st.rebind(`UPDATE endpoint_inventory SET received_at=? WHERE endpoint_id=?`), time.Now().UTC().Add(-4*time.Minute), endpoint); err != nil {
				t.Fatal(err)
			}
			return RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"}
		}, ErrAdoptionChanged},
		"operator": {func(t *testing.T, st *SQLStore, a TenantAccess, _ *Application, _ string, m *ApplicationMapping) RemovalBody {
			if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleOperator, Status: "active"}); err != nil {
				t.Fatal(err)
			}
			return RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"}
		}, ErrForbidden},
		"developer": {func(t *testing.T, st *SQLStore, a TenantAccess, _ *Application, _ string, m *ApplicationMapping) RemovalBody {
			if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleDeveloper, Status: "active"}); err != nil {
				t.Fatal(err)
			}
			return RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"}
		}, ErrForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			st, a, app, endpoint, _, m := planFixture(t)
			body := tc.mutate(t, st, a, app, endpoint, m)
			before := countRows(t, st, `SELECT COUNT(*) FROM deployments`)
			if _, _, err := st.Tenancy().RemoveApplication(ctx, a, app.ID, body); !errors.Is(err, tc.want) {
				t.Fatalf("%s: %v", name, err)
			}
			if n := countRows(t, st, `SELECT COUNT(*) FROM deployments WHERE kind='remove'`); n != 0 || countRows(t, st, `SELECT COUNT(*) FROM deployments`) != before {
				t.Fatalf("%s left a row", name)
			}
		})
	}
	// An expired plan does not block, and goes.
	st, a, app, _, _, m := planFixture(t)
	ts := st.Tenancy()
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=?`), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	d, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM deployments WHERE id<>?`, d.ID); n != 0 {
		t.Fatalf("expired plan kept: %d", n)
	}
}

// twoContainerRemoval adopts two containers without any mapping and starts their removal.
func twoContainerRemoval(t *testing.T) (*SQLStore, TenantAccess, *Application, string, string, *Deployment) {
	t.Helper()
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app, err := ts.CreateApplication(ctx, a, "shop", ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-time.Hour)
	snapshot := protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{
		{ID: strings.Repeat("a", 64), Name: "shop-web", ImageID: "sha256:" + strings.Repeat("b", 64), CreatedAt: created, ComposeProject: "shop"},
		{ID: strings.Repeat("c", 64), Name: "shop-worker", ImageID: "sha256:" + strings.Repeat("b", 64), CreatedAt: created, ComposeProject: "shop"},
	}}
	endpoint := activeEndpointWith(t, ts, a, snapshot.Containers, nil)
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	d, req, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: instance.ID, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Containers) != 2 || req.Containers[0].Service != "unmapped-aaaaaaaaaaaa" || req.Containers[1].Service != "unmapped-cccccccccccc" || d.MappingVersion != 0 {
		t.Fatalf("targets: %+v %+v", req.Containers, d)
	}
	return st, a, app, endpoint, instance.ID, d
}

func TestRemovalPartialSettle(t *testing.T) {
	st, a, app, endpoint, instance, d := twoContainerRemoval(t)
	ctx := context.Background()
	ts := st.Tenancy()
	// The second target's remove "succeeded" after a denied precondition: never trusted.
	res := removalResult(d, protocol.OutcomeDenied,
		removalSteps("unmapped-aaaaaaaaaaaa", protocol.OutcomeSucceeded, protocol.OutcomeSucceeded, protocol.OutcomeSucceeded),
		removalSteps("unmapped-cccccccccccc", protocol.OutcomeDenied, protocol.OutcomeSkipped, protocol.OutcomeSucceeded))
	res.Detail = "fixed text"
	if err := ts.SettleDeployment(ctx, endpoint, res); err != nil {
		t.Fatal(err)
	}
	var left string
	if err := st.db.QueryRow(st.rebind(`SELECT container_id FROM application_resources WHERE instance_id=?`), instance).Scan(&left); err != nil || left != strings.Repeat("c", 64) {
		t.Fatalf("partial resources: %s %v", left, err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM application_instances WHERE id=?`, instance) != 1 || countRows(t, st, `SELECT COUNT(*) FROM applications WHERE id=? AND removed_at IS NULL`, app.ID) != 1 {
		t.Fatal("partial removal released the instance")
	}
	if got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID); err != nil || got.State != "denied" || got.Result == nil || len(got.Result.Steps) != 6 {
		t.Fatalf("partial result not recorded: %+v %v", got, err)
	}
	// Again: the remaining container is already gone, which counts as removed. The first one
	// left the inventory when it was removed.
	putAdoptionSnapshot(t, st, endpoint, protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{}})
	d, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: instance, Confirm: "shop"})
	if err != nil || len(d.Plan.Containers) != 1 {
		t.Fatalf("second removal: %+v %v", d, err)
	}
	res = removalResult(d, protocol.OutcomeSucceeded, removalSteps("unmapped-cccccccccccc", protocol.OutcomeSucceeded, protocol.OutcomeSkipped, protocol.OutcomeSkipped))
	if err := ts.SettleDeployment(ctx, endpoint, res); err != nil {
		t.Fatal(err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM application_instances WHERE id=?`, instance) != 0 || countRows(t, st, `SELECT COUNT(*) FROM applications WHERE id=? AND removed_at IS NOT NULL`, app.ID) != 1 {
		t.Fatal("already-gone target did not complete the removal")
	}
}

func TestRemovalResultRefusesIdentities(t *testing.T) {
	st, a, app, endpoint, _, d := twoContainerRemoval(t)
	ctx := context.Background()
	ts := st.Tenancy()
	all := func() protocol.DeploymentResult {
		return removalResult(d, protocol.OutcomeSucceeded,
			removalSteps("unmapped-aaaaaaaaaaaa", protocol.OutcomeSucceeded, protocol.OutcomeSucceeded, protocol.OutcomeSucceeded),
			removalSteps("unmapped-cccccccccccc", protocol.OutcomeSucceeded, protocol.OutcomeSucceeded, protocol.OutcomeSucceeded))
	}
	for name, mutate := range map[string]func(*protocol.DeploymentResult){
		"identity": func(r *protocol.DeploymentResult) {
			r.Services = []protocol.DeploymentIdentity{{Service: "unmapped-aaaaaaaaaaaa", ContainerID: strings.Repeat("e", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1800000000}}
		},
		"success without every target": func(r *protocol.DeploymentResult) { r.Steps = r.Steps[:3] },
		"unknown target":               func(r *protocol.DeploymentResult) { r.Steps[0].Service = "other" },
		"apply step":                   func(r *protocol.DeploymentResult) { r.Steps[0].Step = protocol.StepCreate },
		"duplicate step":               func(r *protocol.DeploymentResult) { r.Steps[1].Step = protocol.StepPrecondition },
	} {
		res := all()
		mutate(&res)
		if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.State != "applying" || countRows(t, st, `SELECT COUNT(*) FROM application_resources`) != 2 {
		t.Fatalf("refused result wrote: %+v %v", got, err)
	}
}

// historyRow copies a deployment row under a new ID with the given revision, state and settle time.
func historyRow(t *testing.T, st *SQLStore, from string, revision int, state string, settled time.Time) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := st.db.Exec(st.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at,kind) SELECT ?,organization_id,environment_id,application_id,instance_id,endpoint_id,project,'failed',revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at,kind FROM deployments WHERE id=?`), id, from); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET state=?,revision=?,settled_at=? WHERE id=?`), state, revision, settled, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPruneKeepsCurrentAndPreviousHistory(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); err != nil {
		t.Fatal(err)
	}
	// current 2, previous 1.
	if _, err := st.db.Exec(st.rebind(`UPDATE application_instances SET previous_revision=1 WHERE id=?`), m.InstanceID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-DeploymentHistoryRetention - time.Hour)
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET settled_at=? WHERE id=?`), old, d.ID); err != nil {
		t.Fatal(err)
	}
	previous := historyRow(t, st, d.ID, 1, "succeeded", old)
	olderPrevious := historyRow(t, st, d.ID, 1, "succeeded", old.Add(-time.Hour))
	failedCurrent := historyRow(t, st, d.ID, 2, "failed", old.Add(time.Hour))
	stale := historyRow(t, st, d.ID, 3, "failed", old)
	recent := historyRow(t, st, d.ID, 3, "failed", time.Now().UTC())
	unsettled := historyRow(t, st, d.ID, 3, "unknown", old)
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET settled_at=NULL WHERE id=?`), unsettled); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, row := range list {
		kept[row.ID] = true
	}
	// Kept: the latest success at current and previous, anything recent or unsettled.
	if !kept[d.ID] || !kept[previous] || kept[olderPrevious] || kept[failedCurrent] || kept[stale] || !kept[recent] || !kept[unsettled] {
		t.Fatalf("retention: %v (current %s previous %s older previous %s failed current %s stale %s recent %s unsettled %s)", kept, d.ID, previous, olderPrevious, failedCurrent, stale, recent, unsettled)
	}
}

func TestPruneRemovedApplications(t *testing.T) {
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
	// Recently removed: kept, history too.
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM applications WHERE id=?`, app.ID) != 1 || countRows(t, st, `SELECT COUNT(*) FROM deployments WHERE application_id=?`, app.ID) != 1 {
		t.Fatal("recent removal pruned")
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE applications SET removed_at=? WHERE id=?`), time.Now().UTC().Add(-ApplicationRemovedRetention-time.Hour), app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM applications WHERE id=?`, `SELECT COUNT(*) FROM application_revisions WHERE application_id=?`, `SELECT COUNT(*) FROM deployments WHERE application_id=?`} {
		if n := countRows(t, st, q, app.ID); n != 0 {
			t.Fatalf("%s: %d", q, n)
		}
	}
}

func TestAbandonSweepAndFailAreAudited(t *testing.T) {
	audits := func(t *testing.T, st *SQLStore, app, id, outcome, result string) int {
		return countRows(t, st, `SELECT COUNT(*) FROM audit_records WHERE user_id='system' AND action='application.deploy' AND resource=? AND details=? AND result=?`, app+"/deployments/"+id, "outcome="+outcome, result)
	}
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := ts.AbandonDeployments(ctx, endpoint); err != nil {
			t.Fatal(err)
		}
	}
	if n := audits(t, st, app.ID, d.ID, "abandoned", "unknown"); n != 1 {
		t.Fatalf("abandon audits: %d", n)
	}
	for range 2 {
		if err := ts.FailDeployment(ctx, d.ID, "never sent"); err != nil {
			t.Fatal(err)
		}
	}
	if n := audits(t, st, app.ID, d.ID, "not_sent", "failure"); n != 1 {
		t.Fatalf("not sent audits: %d", n)
	}

	st, a, app, _, _, _, d, key = applyFixture(t)
	ts = st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET deadline=? WHERE id=?`), time.Now().Add(-3*time.Minute), d.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := ts.Prune(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := audits(t, st, app.ID, d.ID, "swept", "unknown"); n != 1 {
		t.Fatalf("sweep audits: %d", n)
	}

	// A removal's transitions audit as application.destroy.
	st, _, app, endpoint, _, d = twoContainerRemoval(t)
	if _, err := st.Tenancy().AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().FailDeployment(ctx, d.ID, "never sent"); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"abandoned", "not_sent"} {
		if n := countRows(t, st, `SELECT COUNT(*) FROM audit_records WHERE user_id='system' AND action='application.destroy' AND resource=? AND details=?`, app.ID+"/deployments/"+d.ID, "outcome="+outcome); n != 1 {
			t.Fatalf("removal %s audits: %d", outcome, n)
		}
	}
}

func TestRemovedApplicationRevivedByAdoption(t *testing.T) {
	st, a, app, endpoint, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	d, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, removalResult(d, protocol.OutcomeSucceeded, removalSteps("web", protocol.OutcomeSucceeded, protocol.OutcomeSucceeded, protocol.OutcomeSucceeded))); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE applications SET removed_at=? WHERE id=?`), time.Now().UTC().Add(-ApplicationRemovedRetention-time.Hour), app.ID); err != nil {
		t.Fatal(err)
	}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	apps, err := ts.ListApplications(ctx, a, 0, 10)
	if err != nil || len(apps) != 1 || apps[0].RemovedAt != nil {
		t.Fatalf("re-adopted still removed: %+v %v", apps, err)
	}
	if err := ts.ReleaseApplication(ctx, a, app.ID, instance.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM applications WHERE id=?`, app.ID) != 1 {
		t.Fatal("a revived application was pruned")
	}
}

func TestMigration25RefusesUnmappedApply(t *testing.T) {
	st, _, app, endpoint, _, m := planFixture(t)
	insert := func(kind string) error {
		_, err := st.db.Exec(st.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,kind,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at) SELECT ?,organization_id,environment_id,id,?,?,'shop',?,'failed',1,'x',0,'{}','actor',created_at,created_at FROM applications WHERE id=?`), uuid.NewString(), m.InstanceID, endpoint, kind, app.ID)
		return err
	}
	if err := insert("apply"); err == nil {
		t.Fatal("apply row with mapping_version 0 accepted")
	}
	if err := insert("remove"); err != nil {
		t.Fatalf("remove row with mapping_version 0: %v", err)
	}
}

func TestDeploymentCarriesEndpointName(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, nil, false)
	if err != nil || d.EndpointName != "host" || d.Kind != "apply" {
		t.Fatalf("plan: %+v %v", d, err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.EndpointName != "host" || got.Kind != "apply" {
		t.Fatalf("read: %+v %v", got, err)
	}
	if _, err := st.db.Exec(`UPDATE endpoints SET name='renamed'`); err != nil {
		t.Fatal(err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 1 || list[0].EndpointName != "renamed" {
		t.Fatalf("list: %+v %v", list, err)
	}
}

// A removal releases the instance, so it refuses while the host may run a container of the
// project KyYard would stop tracking: an unadopted one in the inventory, or one an apply of
// unknown outcome may have created.
func TestRemovalRefusesOrphaningContainers(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, instance, want string) {
		t.Helper()
		before := countRows(t, st, `SELECT COUNT(*) FROM deployments`)
		var blocked *PreflightBlockedError
		if _, _, err := st.Tenancy().RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: instance, Confirm: "shop"}); !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{want}) {
			t.Fatalf("want %s: %v", want, err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM deployments`) != before || countRows(t, st, `SELECT COUNT(*) FROM application_instances WHERE id=?`, instance) != 1 {
			t.Fatal("refused removal changed state")
		}
	}

	// Every project container adopted, another project's beside it: allowed.
	st, a, app, endpoint, snapshot, m := planFixture(t)
	other := protocol.Container{ID: strings.Repeat("f", 64), Name: "blog-web", ImageID: "sha256:" + strings.Repeat("b", 64), CreatedAt: time.Now().UTC().Add(-time.Hour), ComposeProject: "blog"}
	withOther := snapshot
	withOther.Containers = append(append([]protocol.Container{}, snapshot.Containers...), other)
	putAdoptionSnapshot(t, st, endpoint, withOther)
	if _, _, err := st.Tenancy().RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"}); err != nil {
		t.Fatalf("adopted project: %v", err)
	}

	// A leftover of the project that was never adopted.
	st, a, app, endpoint, snapshot, m = planFixture(t)
	leftover := other
	leftover.Name, leftover.ComposeProject = "shop-web-old", "shop"
	withLeftover := snapshot
	withLeftover.Containers = append(append([]protocol.Container{}, snapshot.Containers...), leftover)
	putAdoptionSnapshot(t, st, endpoint, withLeftover)
	refused(t, st, a, app, m.InstanceID, "unadopted_project_containers")

	// The last apply was abandoned; a newer remove row that is still planned does not hide it.
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	if _, _, err := st.Tenancy().ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Tenancy().AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	refused(t, st, a, app, m.InstanceID, "apply_outcome_unknown")
	// Once the result settles it, removal proceeds.
	if err := st.Tenancy().SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"}); err != nil {
		t.Fatalf("after settle: %v", err)
	}
}

// A removed application's history is kept by its own retention, not the 90-day history rule.
func TestPruneKeepsRemovedApplicationHistory(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET settled_at=? WHERE id=?`), time.Now().UTC().Add(-DeploymentHistoryRetention-time.Hour), d.ID); err != nil {
		t.Fatal(err)
	}
	r, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, removalResult(r, protocol.OutcomeSucceeded, removalSteps("web", protocol.OutcomeSucceeded, protocol.OutcomeSucceeded, protocol.OutcomeSucceeded))); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM deployments WHERE id=?`, d.ID) != 1 {
		t.Fatal("history of a removed application pruned by age")
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE applications SET removed_at=? WHERE id=?`), time.Now().UTC().Add(-ApplicationRemovedRetention-time.Hour), app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM applications WHERE id=?`, `SELECT COUNT(*) FROM application_revisions WHERE application_id=?`, `SELECT COUNT(*) FROM deployments WHERE application_id=?`} {
		if n := countRows(t, st, q, app.ID); n != 0 {
			t.Fatalf("%s: %d", q, n)
		}
	}
}

// A refused removal result audits as application.destroy.
func TestRefuseRemovalResultAuditsDestroy(t *testing.T) {
	st, _, app, endpoint, _, d := twoContainerRemoval(t)
	if err := st.Tenancy().RefuseDeploymentResult(context.Background(), endpoint, d.ID, "refused"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM audit_records WHERE action='application.destroy' AND user_id=? AND resource=? AND details='outcome=refused'`, "agent:"+endpoint, app.ID+"/deployments/"+d.ID); n != 1 {
		t.Fatalf("refuse audit: %d", n)
	}
}
