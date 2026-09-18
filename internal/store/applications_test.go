package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func desired(image string) store.ApplicationSpec {
	return store.ApplicationSpec{Kind: "compose.v1", Services: []store.ApplicationService{{Name: "web", Image: image, Environment: map[string]store.ApplicationSecretRef{"DATABASE_PASSWORD": {SecretRef: "database-password"}}}}}
}
func TestApplicationRevisionsScopeAndHistory(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	a.EnvironmentID = "env-a"
	ts := st.Tenancy()
	app, err := ts.CreateApplication(ctx, a, " shop ", desired("nginx:1"))
	mustTenant(t, err)
	if app.Name != "shop" || app.LatestRevision != 1 || app.CreatedBy != a.ActorID {
		t.Fatalf("bad application %+v", app)
	}
	if _, err = ts.CreateApplication(ctx, a, "shop", desired("nginx:1")); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate: %v", err)
	}
	next, err := ts.AppendApplicationRevision(ctx, a, app.ID, 1, desired("nginx:2"))
	mustTenant(t, err)
	if next != 2 {
		t.Fatal(next)
	}
	if _, err = ts.AppendApplicationRevision(ctx, a, app.ID, 1, desired("nginx:3")); !errors.Is(err, store.ErrRevisionConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	first, err := ts.ReadApplicationRevision(ctx, a, app.ID, 1)
	mustTenant(t, err)
	second, err := ts.ReadApplicationRevision(ctx, a, app.ID, 2)
	mustTenant(t, err)
	if first.Spec.Services[0].Image != "nginx:1" || second.Spec.Services[0].Image != "nginx:2" || first.Digest == second.Digest || len(first.Digest) != 71 {
		t.Fatal("history or digest changed")
	}
	first.Spec.Services[0].Image = "corrupt:1"
	again, err := ts.ReadApplicationRevision(ctx, a, app.ID, 1)
	mustTenant(t, err)
	if again.Spec.Services[0].Image != "nginx:1" {
		t.Fatal("mutable revision")
	}
	rows, err := ts.ListApplications(ctx, a, 0, 20)
	mustTenant(t, err)
	if len(rows) != 1 || rows[0].LatestRevision != 2 {
		t.Fatal(rows)
	}
	for _, scope := range []store.TenantAccess{{ActorID: a.ActorID, OrganizationID: "b", EnvironmentID: "env-b"}, {ActorID: a.ActorID, OrganizationID: "a", EnvironmentID: "env-b"}, {ActorID: a.ActorID, OrganizationID: "a"}} {
		if _, err = ts.ReadApplicationRevision(ctx, scope, app.ID, 1); err == nil {
			t.Fatal("scope read escaped")
		}
		if _, err = ts.AppendApplicationRevision(ctx, scope, app.ID, 2, desired("nginx:3")); err == nil {
			t.Fatal("scope write escaped")
		}
	}
	mustTenant(t, ts.CreateEnvironment(ctx, &store.Environment{ID: "second-env", OrganizationID: "a", Name: "staging"}))
	secondEnv := a
	secondEnv.EnvironmentID = "second-env"
	_, err = ts.CreateApplication(ctx, secondEnv, "shop", desired("nginx:1"))
	mustTenant(t, err)
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: a.ActorID, Role: store.RoleOrganizationAdmin, Status: "active"}))
	otherOrg := a
	otherOrg.OrganizationID = "b"
	otherOrg.EnvironmentID = "env-b"
	if _, err = ts.ReadApplicationRevision(ctx, otherOrg, app.ID, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("member cross-scope read: %v", err)
	}
	if _, err = ts.AppendApplicationRevision(ctx, otherOrg, app.ID, 2, desired("nginx:3")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("member cross-scope edit: %v", err)
	}
	if err = ts.RemoveEnvironment(ctx, a); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("application environment deletion: %v", err)
	}
	audits, err := ts.ReadAudit(ctx, a, 0, 200)
	mustTenant(t, err)
	success := 0
	for _, row := range audits {
		if row.Action == "application.edit" && row.Result == "success" {
			success++
		}
		if row.Details != "" {
			t.Fatal("revision content entered audit")
		}
	}
	if success != 1 {
		t.Fatalf("successful edit audits %d", success)
	}
}
func TestApplicationRevisionRoles(t *testing.T) {
	for _, role := range []store.TenantRole{store.RoleOrganizationAdmin, store.RoleEnvironmentAdmin, store.RoleOperator, store.RoleDeveloper, store.RoleReadOnly} {
		t.Run(string(role), func(t *testing.T) {
			ctx := context.Background()
			st, a := setupTenantAccess(t)
			a.EnvironmentID = "env-a"
			ts := st.Tenancy()
			app, err := ts.CreateApplication(ctx, a, "shop", desired("nginx:1"))
			mustTenant(t, err)
			tenantUser(t, st, "reader", "admin", "local", "active")
			mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "reader", Role: role, Status: "active"}))
			a.ActorID = "reader"
			_, err = ts.ReadApplicationRevision(ctx, a, app.ID, 1)
			mustTenant(t, err)
			_, err = ts.CreateApplication(ctx, a, "other", desired("nginx:1"))
			wantCreate := role == store.RoleOrganizationAdmin || role == store.RoleEnvironmentAdmin
			if (err == nil) != wantCreate {
				t.Fatalf("create %s: %v", role, err)
			}
			_, err = ts.AppendApplicationRevision(ctx, a, app.ID, 1, desired("nginx:2"))
			wantEdit := wantCreate || role == store.RoleDeveloper
			if (err == nil) != wantEdit {
				t.Fatalf("edit %s: %v", role, err)
			}
			mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "reader", Role: role, Status: "disabled"}))
			if _, err = ts.ReadApplicationRevision(ctx, a, app.ID, 1); !errors.Is(err, store.ErrForbidden) {
				t.Fatalf("disabled read: %v", err)
			}
			if _, err = ts.AppendApplicationRevision(ctx, a, app.ID, 2, desired("nginx:3")); !errors.Is(err, store.ErrForbidden) {
				t.Fatalf("disabled edit: %v", err)
			}
		})
	}
}
func TestApplicationRevisionConcurrentEditors(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	a.EnvironmentID = "env-a"
	ts := st.Tenancy()
	app, err := ts.CreateApplication(ctx, a, "shop", desired("nginx:1"))
	mustTenant(t, err)
	tenantUser(t, st, "second", "user", "local", "active")
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "second", Role: store.RoleDeveloper, Status: "active"}))
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, actor := range []string{a.ActorID, "second"} {
		wg.Add(1)
		go func(actor string) {
			defer wg.Done()
			access := a
			access.ActorID = actor
			<-start
			_, err := ts.AppendApplicationRevision(ctx, access, app.ID, 1, desired("nginx:2"))
			results <- err
		}(actor)
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrRevisionConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}
func TestApplicationRevisionValidationAndLimit(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	a.EnvironmentID = "env-a"
	ts := st.Tenancy()
	invalid := []store.ApplicationSpec{{}, {Kind: "compose.v1"}, desired("https://user:password@registry/image")}
	duplicate := desired("nginx:1")
	duplicate.Services = append(duplicate.Services, duplicate.Services[0])
	invalid = append(invalid, duplicate)
	literal := desired("nginx:1")
	literal.Services[0].Environment["PASSWORD"] = store.ApplicationSecretRef{SecretRef: "literal secret value"}
	invalid = append(invalid, literal)
	oversized := store.ApplicationSpec{Kind: "compose.v1"}
	for i := 0; i < 4; i++ {
		service := store.ApplicationService{Name: fmt.Sprint("service", i), Image: "nginx:1", Environment: map[string]store.ApplicationSecretRef{}}
		for j := 0; j < 128; j++ {
			service.Environment[fmt.Sprint("VAR_", j)] = store.ApplicationSecretRef{SecretRef: strings.Repeat("s", 128)}
		}
		oversized.Services = append(oversized.Services, service)
	}
	invalid = append(invalid, oversized)
	for _, spec := range invalid {
		if _, err := ts.CreateApplication(ctx, a, "invalid", spec); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("invalid accepted: %v", err)
		}
	}
	apps, err := ts.ListApplications(ctx, a, 0, 20)
	mustTenant(t, err)
	if len(apps) != 0 {
		t.Fatal("partial creation")
	}
	app, err := ts.CreateApplication(ctx, a, "shop", desired("nginx:1"))
	mustTenant(t, err)
	for n := 1; n < store.MaxApplicationRevisions; n++ {
		_, err = ts.AppendApplicationRevision(ctx, a, app.ID, n, desired("nginx:2"))
		mustTenant(t, err)
	}
	if _, err = ts.AppendApplicationRevision(ctx, a, app.ID, store.MaxApplicationRevisions, desired("nginx:3")); !errors.Is(err, store.ErrApplicationLimit) {
		t.Fatalf("unbounded revisions: %v", err)
	}
}

func TestApplicationOrganizationQuotaAcrossEditors(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	a.EnvironmentID = "env-a"
	ts := st.Tenancy()
	for i := 0; i < store.MaxApplicationsPerOrganization-1; i++ {
		_, err := ts.CreateApplication(ctx, a, fmt.Sprint("app-", i), desired("nginx:1"))
		mustTenant(t, err)
	}
	tenantUser(t, st, "second", "user", "local", "active")
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "second", Role: store.RoleOrganizationAdmin, Status: "active"}))
	mustTenant(t, ts.CreateEnvironment(ctx, &store.Environment{ID: "other-env", OrganizationID: "a", Name: "other"}))
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, actor := range []string{a.ActorID, "second"} {
		go func(actor string) {
			access := a
			access.ActorID = actor
			if actor == "second" {
				access.EnvironmentID = "other-env"
			}
			<-start
			_, err := ts.CreateApplication(ctx, access, actor, desired("nginx:1"))
			results <- err
		}(actor)
	}
	close(start)
	success, limited := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrApplicationLimit) {
			limited++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || limited != 1 {
		t.Fatalf("success=%d limited=%d", success, limited)
	}
}
