package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func TestDeploymentsTableExists(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	var n int
	if err := st.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deployments table: %d %v", n, err)
	}
}

func planFixture(t *testing.T) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot, *ApplicationMapping) {
	t.Helper()
	st, a, app, endpoint, snapshot, m := mappingFixture(t)
	snapshot.Images = []protocol.Image{{ID: "sha256:" + strings.Repeat("d", 64), Tags: []string{"nginx:1"}, Digests: []string{"nginx@sha256:" + strings.Repeat("e", 64)}}}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if err := st.Tenancy().SetApplicationMapping(context.Background(), a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, err := st.Tenancy().ReadApplicationMapping(context.Background(), a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, a, app, endpoint, snapshot, m
}
func planRequest(m *ApplicationMapping) PlanRequest {
	return PlanRequest{InstanceID: m.InstanceID, MappingVersion: m.Version, Revision: m.Preview.Revision, Confirm: m.Preview.Project}
}

func TestPlanDeploymentBindsIdentities(t *testing.T) {
	st, a, app, _, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	c := snapshot.Containers[0]
	if d.State != "planned" || d.InstanceID != m.InstanceID || d.Revision != 1 || d.MappingVersion != 1 || d.Expired || time.Until(d.ExpiresAt) > DeploymentPlanTTL || time.Until(d.ExpiresAt) < 9*time.Minute {
		t.Fatalf("plan: %+v", d)
	}
	s := d.Plan.Services[0]
	if d.Plan.Project != "shop" || s.Name != "web" || s.ImageID != snapshot.Images[0].ID || s.ImageDigest != snapshot.Images[0].Digests[0] || s.ContainerID != c.ID || s.Replaces.ImageID != c.ImageID || s.Replaces.CreatedUnix != c.CreatedAt.Unix() {
		t.Fatalf("service: %+v", s)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.ID != d.ID || got.SpecDigest != d.SpecDigest {
		t.Fatalf("read: %+v %v", got, err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 1 || list[0].ID != d.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	var commands int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM endpoint_commands`).Scan(&commands); err != nil || commands != 0 {
		t.Fatal("plan dispatched a command")
	}
	foreign := a
	foreign.EnvironmentID = "foreign"
	if _, err = ts.ReadDeployment(ctx, foreign, app.ID, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign read: %v", err)
	}
	if _, err = ts.PlanDeployment(ctx, foreign, app.ID, planRequest(m)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign plan: %v", err)
	}
}

func TestPlanDeploymentRefusesStaleAndBlocked(t *testing.T) {
	st, a, app, endpoint, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for name, r := range map[string]PlanRequest{
		"instance": {InstanceID: "other", MappingVersion: 1, Revision: 1, Confirm: "shop"},
		"mapping":  {InstanceID: m.InstanceID, MappingVersion: 2, Revision: 1, Confirm: "shop"},
		"revision": {InstanceID: m.InstanceID, MappingVersion: 1, Revision: 2, Confirm: "shop"},
		"project":  {InstanceID: m.InstanceID, MappingVersion: 1, Revision: 1, Confirm: "nope"},
	} {
		if _, err := ts.PlanDeployment(ctx, a, app.ID, r); !errors.Is(err, ErrAdoptionChanged) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	// Definition advances: the old approval no longer names the head.
	if _, err := ts.AppendApplicationRevision(ctx, a, app.ID, 1, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m)); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("stale revision accepted")
	}
	r := planRequest(m)
	r.Revision = 2
	var blocked *PreflightBlockedError
	if _, err := ts.PlanDeployment(ctx, a, app.ID, r); !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, "mapping_requires_review") {
		t.Fatalf("mapping stale not reported: %v", err)
	}
	// Re-map, then remove the image from inventory: a service blocker refuses too.
	m2, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m2)); err != nil {
		t.Fatal(err)
	}
	snapshot.Images = nil
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	m2, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m2)); !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, "image_not_reported") {
		t.Fatalf("service blocker not reported: %v", err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 0 {
		t.Fatal("refused plan left a row")
	}
}

func TestPlanDeploymentReplacesAndExpires(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	first, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
	if err != nil || second.ID == first.ID {
		t.Fatalf("replacement: %+v %v", second, err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 1 || list[0].ID != second.ID {
		t.Fatalf("one row per instance: %+v %v", list, err)
	}
	if _, err = ts.ReadDeployment(ctx, a, app.ID, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("replaced plan still readable")
	}
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=?`), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, second.ID)
	if err != nil || !got.Expired {
		t.Fatalf("expiry: %+v %v", got, err)
	}
}

func TestPlanDeploymentRoles(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for role, ok := range map[TenantRole]bool{RoleDeveloper: true, RoleEnvironmentAdmin: true, RoleOperator: false, RoleReadOnly: false} {
		if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		_, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
		if ok && err != nil || !ok && !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s: %v", role, err)
		}
		if _, err = ts.ListDeployments(ctx, a, app.ID); err != nil {
			t.Fatalf("%s list: %v", role, err)
		}
	}
}

// An adopted container whose image ID lacks the sha256: prefix passes adoption (which only
// requires a non-empty image ID) but cannot back a real InspectionTarget. PlanDeployment must
// refuse it rather than mint a plan naming a container with no identity to recheck at apply.
func TestPlanDeploymentRefusesInvalidReplacementIdentity(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app, err := ts.CreateApplication(ctx, a, "shop", ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{{ID: strings.Repeat("a", 64), Name: "shop-web", ImageID: strings.Repeat("b", 64), CreatedAt: time.Now().UTC().Add(-time.Hour), ComposeProject: "shop"}}}
	endpoint := activeEndpointWith(t, ts, a, snapshot.Containers, nil)
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
		t.Fatal(err)
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, err = ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Images = []protocol.Image{{ID: "sha256:" + strings.Repeat("d", 64), Tags: []string{"nginx:1"}}}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	var blocked *PreflightBlockedError
	if _, err = ts.PlanDeployment(ctx, a, app.ID, planRequest(m)); !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, "replacement_identity_invalid") {
		t.Fatalf("invalid replacement identity not reported: %v", err)
	}
}

func TestReleaseRefusesLivePlanAndDeletesExpired(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m)); err != nil {
		t.Fatal(err)
	}
	if err := ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); !errors.Is(err, ErrDeploymentPlanned) {
		t.Fatalf("release with live plan: %v", err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=?`), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 0 {
		t.Fatal("release kept plan rows")
	}
}

func TestMigration24ShapesDeployments(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	for _, col := range []string{"applied_by", "applied_at", "deadline", "settled_at", "detail", "result"} {
		if _, err := st.db.ExecContext(ctx, `SELECT `+col+` FROM deployments LIMIT 1`); err != nil {
			t.Fatalf("column %s: %v", col, err)
		}
	}
	for _, col := range []string{"current_revision", "previous_revision"} {
		if _, err := st.db.ExecContext(ctx, `SELECT `+col+` FROM application_instances LIMIT 1`); err != nil {
			t.Fatalf("instance column %s: %v", col, err)
		}
	}
}

func TestPlanDeploymentRespectsLiveRows(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	first, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	// A settled history row must survive a new plan; only the planned row is replaced.
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET state='succeeded',settled_at=? WHERE id=?`), time.Now().UTC(), first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 2 || list[0].ID != second.ID || list[1].State != "succeeded" {
		t.Fatalf("history kept: %+v %v", list, err)
	}
	// An applying row blocks a new plan.
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET state='applying' WHERE id=?`), second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = ts.PlanDeployment(ctx, a, app.ID, planRequest(m)); !errors.Is(err, ErrDeploymentInProgress) {
		t.Fatalf("plan during apply: %v", err)
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); !errors.Is(err, ErrDeploymentInProgress) {
		t.Fatalf("release during apply: %v", err)
	}
	// Settled rows never block release and are kept.
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET state='failed' WHERE id=?`), second.ID); err != nil {
		t.Fatal(err)
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("history after release: %d %v", n, err)
	}
}
