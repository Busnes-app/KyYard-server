package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"strings"
	"sync"
	"testing"
	"time"
)

func adoptionFixture(t *testing.T) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot) {
	t.Helper()
	return adoptionFixtureWith(t, []ApplicationService{{Name: "web", Image: "nginx:1"}}, nil)
}

// adoptionFixtureWith runs one container per service, the i-th on its own image; digests, when
// set, puts those images in the inventory with their repository digests.
func adoptionFixtureWith(t *testing.T, services []ApplicationService, digests map[string][]string) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot) {
	t.Helper()
	return adoptionFixtureSpec(t, ApplicationSpec{Kind: "compose.v1", Services: services}, digests, nil)
}

// adoptionFixtureSpec is adoptionFixtureWith for a whole spec. Each container reports
// mounts[service], or no mounts when the service has no entry; a nil entry is an agent that
// does not report mounts.
func adoptionFixtureSpec(t *testing.T, spec ApplicationSpec, digests map[string][]string, mounts map[string][]protocol.Mount) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot) {
	t.Helper()
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	app, err := st.Tenancy().ImportApplication(ctx, a, "shop", spec, map[string]string{}, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := protocol.Snapshot{Engine: protocol.Engine{Version: "1"}}
	for i, s := range spec.Services {
		c := protocol.Container{ID: strings.Repeat("a", 63) + string("a0123456789"[i]), Name: "shop-" + s.Name, ImageID: "sha256:" + strings.Repeat("b", 63) + string("b0123456789"[i]), CreatedAt: time.Now().UTC().Add(-time.Hour), ComposeProject: "shop", Mounts: []protocol.Mount{}}
		if m, ok := mounts[s.Name]; ok {
			c.Mounts = m
		}
		snapshot.Containers = append(snapshot.Containers, c)
		if digests != nil {
			snapshot.Images = append(snapshot.Images, protocol.Image{ID: c.ImageID, Digests: digests[s.Name]})
		}
	}
	endpoint := activeEndpointWith(t, st.Tenancy(), a, snapshot.Containers, nil)
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	return st, a, app, endpoint, snapshot
}
func putAdoptionSnapshot(t *testing.T, st *SQLStore, endpoint string, snapshot protocol.Snapshot) {
	t.Helper()
	raw, _ := json.Marshal(snapshot)
	_, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE endpoint_inventory SET snapshot=? WHERE endpoint_id=?`), string(raw), endpoint)
	if err != nil {
		t.Fatal(err)
	}
}
func TestAdoptionPinsResourcesAndReleasePreservesRuntime(t *testing.T) {
	st, a, app, endpoint, snapshot := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	r := AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}
	instance, err := ts.AdoptApplication(ctx, a, app.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(instance.Containers) != 1 || instance.Containers[0].ID != snapshot.Containers[0].ID {
		t.Fatal("lost resource identity")
	}
	if err = ts.DiscardApplication(ctx, a, app.ID, 1); !errors.Is(err, ErrApplicationAdopted) {
		t.Fatalf("discard adopted: %v", err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, r); err == nil {
		t.Fatal("duplicate adoption")
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, "wrong-instance", "shop"); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("unconfirmed release")
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, instance.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	next, err := ts.AdoptApplication(ctx, a, app.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, instance.ID, "shop"); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("old release affected replacement")
	}
	rows, err := ts.ListApplicationInstances(ctx, a, endpoint)
	if err != nil || len(rows) != 1 || rows[0].ID != next.ID {
		t.Fatalf("list: %v", err)
	}
	if got, err := ts.ReadApplicationInstance(ctx, a, app.ID, next.ID); err != nil || got.EndpointID != endpoint || got.Project != "shop" || got.ContainerCount != 1 {
		t.Fatalf("read instance: %+v %v", got, err)
	}
	if _, err := ts.ReadApplicationInstance(ctx, a, app.ID, instance.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read a released instance: %v", err)
	}
	if _, err := ts.ReadApplicationInstance(ctx, a, "00000000-0000-0000-0000-000000000000", next.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read an instance through another application: %v", err)
	}
	inv, err := ts.ReadInventory(ctx, a, endpoint)
	if err != nil || !strings.Contains(string(inv.Snapshot), snapshot.Containers[0].ID) {
		t.Fatal("runtime observation changed")
	}
	var commands int
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM endpoint_commands`).Scan(&commands); err != nil || commands != 0 {
		t.Fatal("adoption dispatched a command")
	}
}
func TestAdoptionRefusesStalePartialChangedOrForeignTargets(t *testing.T) {
	st, a, app, endpoint, snapshot := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	request := AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}
	snapshot.Containers[0].ID = strings.Repeat("c", 64)
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if _, err = ts.AdoptApplication(ctx, a, app.ID, request); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatalf("stale accepted: %v", err)
	}
	snapshot.Truncated = []string{"containers"}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if _, err = ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop"); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("partial preview")
	}
	snapshot.Truncated = nil
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	_, err = st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_inventory SET received_at=? WHERE endpoint_id=?`), time.Now().Add(-4*time.Minute), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, request); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("stale inventory accepted")
	}
	foreign := a
	foreign.OrganizationID = "other"
	if _, err = ts.AdoptApplication(ctx, foreign, app.ID, request); !errors.Is(err, ErrForbidden) {
		t.Fatalf("scope bypass: %v", err)
	}
	for _, role := range []TenantRole{RoleDeveloper, RoleOperator, RoleReadOnly} {
		if err = ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if _, err = ts.AdoptApplication(ctx, a, app.ID, request); !errors.Is(err, ErrForbidden) {
			t.Fatalf("role bypass %s: %v", role, err)
		}
	}
}
func TestAdoptionConcurrentOwnershipAndAuditRollback(t *testing.T) {
	st, a, app, endpoint, _ := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	other, err := ts.CreateApplication(ctx, a, "other", ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{app.ID, other.ID} {
		p, err := ts.PreviewApplicationAdoption(ctx, a, id, endpoint, "shop")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(id, digest string) {
			defer wg.Done()
			_, err := ts.AdoptApplication(ctx, a, id, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: digest, Confirm: "shop"})
			results <- err
		}(id, p.Digest)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("owners=%d", success)
	}
	rows, err := ts.ListApplicationInstances(ctx, a, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err = ts.ReleaseApplication(ctx, a, rows[0].ApplicationID, rows[0].ID, "shop"); err != nil {
		t.Fatal(err)
	}
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.ExecContext(ctx, `DROP TABLE audit_records`); err != nil {
		t.Fatal(err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err == nil {
		t.Fatal("adopted without audit")
	}
	var count int
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM application_instances`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial adoption")
	}
}

func TestAdoptionCannotCrossEnvironmentOrClaimReplacement(t *testing.T) {
	st, a, app, endpoint, snapshot := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.CreateEnvironment(ctx, &Environment{ID: "second", OrganizationID: a.OrganizationID, Name: "Second"}); err != nil {
		t.Fatal(err)
	}
	b := a
	b.EnvironmentID = "second"
	other, err := ts.CreateApplication(ctx, b, "other", ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.PreviewApplicationAdoption(ctx, b, other.ID, endpoint, "shop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign environment endpoint: %v", err)
	}
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Containers[0].ID = strings.Repeat("d", 64)
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	rows, err := ts.ListApplicationInstances(ctx, a, endpoint)
	if err != nil || len(rows) != 1 || rows[0].Containers[0].ID != strings.Repeat("a", 64) {
		t.Fatal("replacement silently claimed")
	}
	_, err = st.db.ExecContext(ctx, st.rebind(`INSERT INTO application_instances(id,organization_id,environment_id,application_id,endpoint_id,project,revision,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`), "invalid", b.OrganizationID, b.EnvironmentID, other.ID, endpoint, "other", 1, b.ActorID, time.Now())
	if err == nil {
		t.Fatal("cross-environment foreign key accepted")
	}
}
