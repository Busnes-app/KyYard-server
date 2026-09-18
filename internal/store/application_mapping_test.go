package store

import (
	"context"
	"errors"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"strings"
	"testing"
)

func mappingFixture(t *testing.T) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot, *ApplicationMapping) {
	t.Helper()
	st, a, app, endpoint, snapshot := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
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
	return st, a, app, endpoint, snapshot, m
}
func mappingRequest(m *ApplicationMapping) MappingRequest {
	return MappingRequest{InstanceID: m.InstanceID, Version: m.Version, Digest: m.Preview.Digest, Confirm: m.Preview.Project, Bindings: map[string]string{"web": m.Preview.Containers[0].ID}}
}
func TestApplicationMappingPreconditions(t *testing.T) {
	st, a, app, endpoint, snapshot, m := mappingFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	r := mappingRequest(m)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, r); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil || got.Version != 1 || got.MappedRevision != 1 || got.Bindings["web"] != r.Bindings["web"] {
		t.Fatalf("saved: %+v %v", got, err)
	}
	if err = ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("replayed mapping accepted")
	}
	r = mappingRequest(got)
	// New project containers are not offered as owned choices.
	extra := snapshot.Containers[0]
	extra.ID = strings.Repeat("c", 64)
	snapshot.Containers = append(snapshot.Containers, extra)
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if err = ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("changed preview accepted")
	}
	got, err = ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil || len(got.Preview.Containers) != 1 {
		t.Fatal("unowned choice exposed")
	}
	r = mappingRequest(got)
	r.Bindings["web"] = extra.ID
	if err = ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrInvalid) {
		t.Fatal("unowned ID assigned")
	}
	r = mappingRequest(got)
	r.Bindings = map[string]string{"unknown": snapshot.Containers[0].ID}
	if err = ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrInvalid) {
		t.Fatal("unknown service assigned")
	}
	r = mappingRequest(got)
	if _, err = ts.AppendApplicationRevision(ctx, a, app.ID, 1, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:2"}, {Name: "db", Image: "postgres:17"}}}); err != nil {
		t.Fatal(err)
	}
	if err = ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("old revision accepted")
	}
	got, err = ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil || got.MappedRevision != 1 || got.Preview.Revision != 2 {
		t.Fatal("stale mapping not identified")
	}
	r = mappingRequest(got)
	r.Bindings["db"] = r.Bindings["web"]
	if err = ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate container assigned")
	}
	r = mappingRequest(got)
	r.Bindings = map[string]string{}
	if err = ts.SetApplicationMapping(ctx, a, app.ID, r); err != nil {
		t.Fatal(err)
	}
	got, err = ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil || len(got.Bindings) != 0 || got.MappedRevision != 2 {
		t.Fatal("clear failed")
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, got.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
		t.Fatal(err)
	}
	if err = ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(got)); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("old instance accepted")
	}
	var commands int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM endpoint_commands`).Scan(&commands); err != nil || commands != 0 {
		t.Fatal("mapping dispatched command")
	}
}
func TestApplicationMappingRefusalsAndAtomicity(t *testing.T) {
	st, a, app, endpoint, snapshot, m := mappingFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	r := mappingRequest(m)
	foreign := a
	foreign.EnvironmentID = "foreign"
	if err := ts.SetApplicationMapping(ctx, foreign, app.ID, r); err == nil {
		t.Fatal("foreign environment accepted")
	}
	for _, role := range []TenantRole{RoleDeveloper, RoleOperator, RoleReadOnly} {
		if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if err := ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrForbidden) {
			t.Fatalf("role %s accepted", role)
		}
	}
	if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleOrganizationAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	original := snapshot.Containers[0]
	snapshot.Containers[0].ImageID = "changed"
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("changed identity accepted")
	}
	snapshot.Containers[0] = original
	snapshot.Truncated = []string{"containers"}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("partial inventory accepted")
	}
	snapshot.Truncated = nil
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if _, err := st.db.Exec(`DROP TABLE audit_records`); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, r); err == nil {
		t.Fatal("mapping without audit")
	}
	var version int
	var service string
	if err := st.db.QueryRow(`SELECT mapping_version FROM application_instances`).Scan(&version); err != nil || version != 0 {
		t.Fatal("partial version persisted")
	}
	if err := st.db.QueryRow(`SELECT service_name FROM application_resources`).Scan(&service); err != nil || service != "" {
		t.Fatal("partial mapping persisted")
	}
}

func TestApplicationMappingConcurrentAdministrators(t *testing.T) {
	st, a, app, _, _, m := mappingFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if err := st.Users().CreateUser(ctx, &User{ID: "second", Username: "second", Role: "user", SSOProvider: "local", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: "second", Role: RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, actor := range []string{a.ActorID, "second"} {
		go func(actor string) {
			access := a
			access.ActorID = actor
			<-start
			results <- ts.SetApplicationMapping(ctx, access, app.ID, mappingRequest(m))
		}(actor)
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrAdoptionChanged) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success %d conflict %d", success, conflict)
	}
}
