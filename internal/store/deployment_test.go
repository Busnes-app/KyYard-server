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
	if _, err = ts.ReadDeployment(ctx, foreign, app.ID, d.ID); err == nil {
		t.Fatal("foreign read")
	}
	if _, err = ts.PlanDeployment(ctx, foreign, app.ID, planRequest(m)); err == nil {
		t.Fatal("foreign plan")
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
