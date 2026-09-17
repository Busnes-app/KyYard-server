package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestMembershipManagementIsScopedAndGuarded(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	ts := st.Tenancy()
	tenantUser(t, st, "newcomer", "user", "local", "active")
	tenantUser(t, st, "envadmin", "user", "local", "active")
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}))
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "newcomer", Role: store.RoleReadOnly, Status: "active"}))

	if err := ts.PutMembership(ctx, a, "newcomer", store.RoleOperator, "active"); err != nil {
		t.Fatal(err)
	}
	members, err := ts.ListMembers(ctx, a, 0, 50)
	mustTenant(t, err)
	if len(members) != 3 || members[0].Username != "actor" || members[2].Role != store.RoleOperator {
		t.Fatalf("member list wrong: %+v", members)
	}
	for _, m := range members {
		if m.UserID == "newcomer" && m.Role != store.RoleOperator {
			t.Fatal("role not applied")
		}
	}

	if err := ts.PutMembership(ctx, a, "ghost", store.RoleOperator, "active"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	if err := ts.PutMembership(ctx, a, "newcomer", "owner", "active"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("unknown role: %v", err)
	}
	if err := ts.PutMembership(ctx, a, "newcomer", store.RoleOperator, "pending"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("unknown status: %v", err)
	}
	if err := ts.RemoveMembership(ctx, a, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("remove unknown: %v", err)
	}

	// Environment administrators manage environments, never members.
	env := store.TenantAccess{ActorID: "envadmin", OrganizationID: "a"}
	if _, err := ts.ListMembers(ctx, env, 0, 50); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("environment admin listed members: %v", err)
	}
	if err := ts.PutMembership(ctx, env, "envadmin", store.RoleOrganizationAdmin, "active"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("self promotion: %v", err)
	}
	// Membership in b does not reach a's members, and a's admin cannot reach b.
	foreign := a
	foreign.OrganizationID = "b"
	if _, err := ts.ListMembers(ctx, foreign, 0, 50); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("cross-tenant list: %v", err)
	}
	if err := ts.RemoveMembership(ctx, foreign, "newcomer"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("cross-tenant remove: %v", err)
	}
	if m, err := ts.GetMembership(ctx, "b", "newcomer"); err != nil || m.Role != store.RoleReadOnly {
		t.Fatalf("b membership disturbed: %v %+v", err, m)
	}

	// The last active administrator cannot demote, disable or remove themself.
	for _, tc := range []struct {
		name string
		op   func() error
	}{
		{"demote", func() error { return ts.PutMembership(ctx, a, "actor", store.RoleReadOnly, "active") }},
		{"disable", func() error { return ts.PutMembership(ctx, a, "actor", store.RoleOrganizationAdmin, "disabled") }},
		{"remove", func() error { return ts.RemoveMembership(ctx, a, "actor") }},
	} {
		if err := tc.op(); !errors.Is(err, store.ErrLastAdmin) {
			t.Fatalf("%s last admin: %v", tc.name, err)
		}
		if m, err := ts.GetMembership(ctx, "a", "actor"); err != nil || m.Role != store.RoleOrganizationAdmin || m.Status != "active" {
			t.Fatalf("%s rolled back badly: %v %+v", tc.name, err, m)
		}
	}
	// With a second administrator the same operations succeed, then the guard moves.
	mustTenant(t, ts.PutMembership(ctx, a, "newcomer", store.RoleOrganizationAdmin, "active"))
	mustTenant(t, ts.RemoveMembership(ctx, a, "actor"))
	if _, err := ts.ListMembers(ctx, a, 0, 50); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("removed admin still acts: %v", err)
	}

	records, err := ts.ReadAudit(ctx, store.TenantAccess{ActorID: "newcomer", OrganizationID: "a"}, 0, 50)
	mustTenant(t, err)
	var targeted, denied int
	for _, r := range records {
		if r.Action == "organization.members.manage" && r.Resource == "actor" && r.Result == "success" {
			targeted++
		}
		if r.Action == "organization.members.manage" && r.Result == "denied" {
			denied++
		}
	}
	if targeted == 0 || denied == 0 {
		t.Fatalf("membership audit missing target or denial: %+v", records)
	}
}

func TestListMemberOrganizationsIsOwnActiveOnly(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTenantAccess(t)
	ts := st.Tenancy()
	tenantUser(t, st, "other", "user", "local", "active")
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "actor", Role: store.RoleReadOnly, Status: "disabled"}))
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "other", Role: store.RoleOperator, Status: "active"}))
	orgs, err := ts.ListMemberOrganizations(ctx, "actor")
	mustTenant(t, err)
	if len(orgs) != 1 || orgs[0].ID != "a" || orgs[0].Role != store.RoleOrganizationAdmin {
		t.Fatalf("own organizations wrong: %+v", orgs)
	}
	orgs, err = ts.ListMemberOrganizations(ctx, "nobody")
	mustTenant(t, err)
	if len(orgs) != 0 {
		t.Fatalf("stranger got organizations: %+v", orgs)
	}
}
