package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func setupTenantAccess(t *testing.T) (store.Store, store.TenantAccess) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for _, id := range []string{"a", "b"} {
		mustTenant(t, ts.CreateOrganization(ctx, &store.Organization{ID: id, Name: id}))
		mustTenant(t, ts.CreateEnvironment(ctx, &store.Environment{ID: "env-" + id, OrganizationID: id, Name: "Production"}))
	}
	tenantUser(t, st, "actor", "admin", "local", "active")
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "actor", Role: store.RoleOrganizationAdmin, Status: "active"}))
	return st, store.TenantAccess{ActorID: "actor", OrganizationID: "a", CorrelationID: "server-request", IPAddress: "127.0.0.1"}
}
func TestScopedAuthorizationAndAudit(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	ts := st.Tenancy()
	_, err := ts.ReadOrganization(ctx, a)
	mustTenant(t, err)
	rows, err := ts.ListEnvironments(ctx, a, 0, 20)
	mustTenant(t, err)
	if len(rows) != 1 || rows[0].ID != "env-a" {
		t.Fatal("environment list leaked")
	}
	own := a
	own.EnvironmentID = "env-a"
	_, err = ts.ReadEnvironment(ctx, own)
	mustTenant(t, err)
	foreign := a
	foreign.OrganizationID = "b"
	foreign.EnvironmentID = "env-b"
	if _, err := ts.ReadOrganization(ctx, foreign); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("platform admin bypass: %v", err)
	}
	if _, err := ts.AddEnvironment(ctx, foreign, "Forbidden"); !errors.Is(err, store.ErrForbidden) {
		t.Fatal("cross-tenant create")
	}
	mixed := a
	mixed.EnvironmentID = "env-b"
	if _, err := ts.ReadEnvironment(ctx, mixed); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-tenant environment read")
	}
	if err := ts.UpdateEnvironment(ctx, mixed, "Stolen"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-tenant update")
	}
	if err := ts.RemoveEnvironment(ctx, mixed); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-tenant delete")
	}
	if _, err := ts.ReadAudit(ctx, foreign, 0, 50); !errors.Is(err, store.ErrForbidden) {
		t.Fatal("cross-tenant audit")
	}
	e, err := ts.AddEnvironment(ctx, a, "Staging")
	mustTenant(t, err)
	created := a
	created.EnvironmentID = e.ID
	mustTenant(t, ts.UpdateEnvironment(ctx, created, "Development"))
	mustTenant(t, ts.RemoveEnvironment(ctx, created))
	mustTenant(t, st.Audit().LogAudit(ctx, &store.AuditRecord{Action: "legacy.global", Resource: "global-secret-marker"}))
	records, err := ts.ReadAudit(ctx, a, 0, 200)
	mustTenant(t, err)
	var foundCreate, foundDelete bool
	for _, r := range records {
		if r.Scope != "organization" || r.OrganizationID != "a" || r.Resource == "global-secret-marker" {
			t.Fatalf("audit scope leak %+v", r)
		}
		if r.Action == "environment.create" && r.Result == "success" {
			foundCreate = true
			if r.UserID != "actor" || r.Resource != e.ID || r.EnvironmentID != e.ID || r.CorrelationID != a.CorrelationID || r.CreatedAt.IsZero() {
				t.Fatalf("incomplete mutation audit %+v", r)
			}
		}
		if r.Action == "environment.delete" && r.Result == "success" && r.EnvironmentID == e.ID {
			foundDelete = true
		}
	}
	if !foundCreate || !foundDelete {
		t.Fatal("missing successful mutation audit")
	}
	globalRecords, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
	mustTenant(t, err)
	for _, r := range globalRecords {
		if r.OrganizationID == "b" {
			t.Fatalf("non-member probe wrote into organization b: %+v", r)
		}
		if r.Action == "legacy.global" && (r.Scope != "platform" || r.Result != "unknown") {
			t.Fatal("legacy audit meaning changed")
		}
	}
}
func TestLiveMembershipAndAccountRevocation(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	ts := st.Tenancy()
	a.EnvironmentID = "env-a"
	for _, role := range []store.TenantRole{store.RoleOperator, store.RoleDeveloper, store.RoleReadOnly} {
		mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "actor", Role: role, Status: "active"}))
		_, err := ts.ReadEnvironment(ctx, a)
		mustTenant(t, err)
		if err := ts.UpdateEnvironment(ctx, a, "No"); !errors.Is(err, store.ErrForbidden) {
			t.Fatal("read role mutated environment")
		}
	}
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "actor", Role: store.RoleEnvironmentAdmin, Status: "active"}))
	mustTenant(t, ts.UpdateEnvironment(ctx, a, "Allowed"))
	if _, err := ts.ReadAudit(ctx, a, 0, 50); !errors.Is(err, store.ErrForbidden) {
		t.Fatal("environment admin read organization audit")
	}
	for _, state := range []string{"disabled", "removed", "inactive-user", "restricted-user"} {
		mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "actor", Role: store.RoleOrganizationAdmin, Status: "active"}))
		u, err := st.Users().GetUserByID(ctx, "actor")
		mustTenant(t, err)
		u.Status = "active"
		u.MustChangePassword = false
		mustTenant(t, st.Users().UpdateUser(ctx, u))
		switch state {
		case "disabled":
			mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "actor", Role: store.RoleOrganizationAdmin, Status: "disabled"}))
		case "removed":
			mustTenant(t, ts.DeleteMembership(ctx, "a", "actor"))
		case "inactive-user":
			u.Status = "disabled"
			mustTenant(t, st.Users().UpdateUser(ctx, u))
		case "restricted-user":
			u.MustChangePassword = true
			mustTenant(t, st.Users().UpdateUser(ctx, u))
		}
		if _, err := ts.ReadEnvironment(ctx, a); !errors.Is(err, store.ErrForbidden) {
			t.Fatalf("%s read granted: %v", state, err)
		}
		if err := ts.UpdateEnvironment(ctx, a, "No"); !errors.Is(err, store.ErrForbidden) {
			t.Fatalf("%s write granted: %v", state, err)
		}
	}
}

func TestNonMemberDenialLeavesNoTenantAudit(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTenantAccess(t)
	ts := st.Tenancy()
	tenantUser(t, st, "stranger", "user", "local", "active")
	for _, actor := range []string{"stranger", "actor", "nobody"} {
		foreign := store.TenantAccess{ActorID: actor, OrganizationID: "b", CorrelationID: "probe-" + actor}
		if _, err := ts.ReadOrganization(ctx, foreign); !errors.Is(err, store.ErrForbidden) {
			t.Fatalf("%s read b: %v", actor, err)
		}
		if _, err := ts.AddEnvironment(ctx, foreign, "Planted"); !errors.Is(err, store.ErrForbidden) {
			t.Fatalf("%s wrote b: %v", actor, err)
		}
	}
	// A member's denial is still recorded, so the organization sees its own failed attempts.
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "stranger", Role: store.RoleReadOnly, Status: "active"}))
	if _, err := ts.AddEnvironment(ctx, store.TenantAccess{ActorID: "stranger", OrganizationID: "a", CorrelationID: "member-denied"}, "No"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read-only created environment: %v", err)
	}
	all, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
	mustTenant(t, err)
	var memberDenied bool
	for _, r := range all {
		if r.OrganizationID == "b" || strings.HasPrefix(r.CorrelationID, "probe-") {
			t.Fatalf("non-member wrote tenant audit: %+v", r)
		}
		if r.CorrelationID == "member-denied" && r.Result == "denied" && r.OrganizationID == "a" {
			memberDenied = true
		}
	}
	if !memberDenied {
		t.Fatal("member denial not audited")
	}
}
