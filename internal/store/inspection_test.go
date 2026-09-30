package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInspectionTargetScopeIdentityAndFreshness(t *testing.T) {
	st, a, _, endpoint, snapshot := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	id := snapshot.Containers[0].ID
	target, err := ts.ReadInspectionTarget(ctx, a, endpoint, id)
	if err != nil || target.ContainerID != id || target.ImageID != snapshot.Containers[0].ImageID || target.CreatedUnix != snapshot.Containers[0].CreatedAt.Unix() {
		t.Fatalf("target: %+v %v", target, err)
	}
	for _, role := range []TenantRole{RoleReadOnly, RoleOperator, RoleDeveloper, RoleEnvironmentAdmin, RoleOrganizationAdmin} {
		if err = ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if _, err = ts.ReadInspectionTarget(ctx, a, endpoint, id); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
	}
	foreign := a
	foreign.EnvironmentID = "foreign"
	if _, err = ts.ReadInspectionTarget(ctx, foreign, endpoint, id); err == nil {
		t.Fatal("foreign environment allowed")
	}
	foreign = a
	foreign.OrganizationID = "foreign"
	if _, err = ts.ReadInspectionTarget(ctx, foreign, endpoint, id); err == nil {
		t.Fatal("foreign organization allowed")
	}
	snapshot.Truncated = []string{"containers"}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if _, err = ts.ReadInspectionTarget(ctx, a, endpoint, id); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("partial inventory allowed")
	}
	snapshot.Truncated = nil
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if _, err = st.db.Exec(st.rebind(`UPDATE endpoint_inventory SET received_at=? WHERE endpoint_id=?`), time.Now().Add(-4*time.Minute), endpoint); err != nil {
		t.Fatal(err)
	}
	if _, err = ts.ReadInspectionTarget(ctx, a, endpoint, id); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("stale inventory allowed")
	}
}

// A container adopted while its configuration was being read is refused when the read is
// recorded, in the same transaction.
func TestConfigurationReadRefusesAContainerAdoptedMeanwhile(t *testing.T) {
	st, a, app, endpoint, snapshot := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	target, err := ts.ReadInspectionTarget(ctx, a, endpoint, snapshot.Containers[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.RecordConfigurationRead(ctx, a, endpoint, target, 0); err != nil {
		t.Fatalf("unmanaged: %v", err)
	}
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.RecordConfigurationRead(ctx, a, endpoint, target, 0); !errors.Is(err, ErrContainerManaged) {
		t.Fatalf("adopted meanwhile: %v", err)
	}
}
