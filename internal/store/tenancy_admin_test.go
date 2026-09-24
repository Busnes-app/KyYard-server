package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func newOrgID() string { return "org_" + crypto.RandomHex(12) }

func TestCreateOrganizationWithAdmin(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	tenantUser(t, st, "alice", "user", "local", "active")
	tenantUser(t, st, "sleepy", "user", "local", "suspended")

	first := &store.Organization{ID: newOrgID(), Name: "Acme"}
	mustTenant(t, ts.CreateOrganizationWithAdmin(ctx, first, "alice"))
	if _, err := ts.GetOrganization(ctx, first.ID); err != nil {
		t.Fatalf("organization row: %v", err)
	}
	m, err := ts.GetMembership(ctx, first.ID, "alice")
	mustTenant(t, err)
	if m.Role != store.RoleOrganizationAdmin || m.Status != "active" {
		t.Fatalf("membership: %+v", m)
	}

	absent := func(id string) {
		t.Helper()
		if _, err := ts.GetOrganization(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("organization %s exists after refusal: %v", id, err)
		}
	}
	missing := &store.Organization{ID: newOrgID(), Name: "Ghost"}
	if err := ts.CreateOrganizationWithAdmin(ctx, missing, "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	absent(missing.ID)
	inactive := &store.Organization{ID: newOrgID(), Name: "Dormant"}
	if err := ts.CreateOrganizationWithAdmin(ctx, inactive, "sleepy"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("inactive user: %v", err)
	}
	absent(inactive.ID)
	dup := &store.Organization{ID: newOrgID(), Name: "Acme"}
	if err := ts.CreateOrganizationWithAdmin(ctx, dup, "alice"); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	absent(dup.ID)

	// Exact match: a name differing only in case is a different organization.
	second := &store.Organization{ID: newOrgID(), Name: "acme"}
	mustTenant(t, ts.CreateOrganizationWithAdmin(ctx, second, "alice"))

	orgs, err := ts.ListMemberOrganizations(ctx, "alice")
	mustTenant(t, err)
	roles := map[string]store.TenantRole{}
	for _, o := range orgs {
		roles[o.ID] = o.Role
	}
	if len(orgs) != 2 || roles[first.ID] != store.RoleOrganizationAdmin || roles[second.ID] != store.RoleOrganizationAdmin {
		t.Fatalf("member organizations: %+v", orgs)
	}
}

func TestListOrganizations(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	for _, id := range []string{"alice", "bob", "carol"} {
		tenantUser(t, st, id, "user", "local", "active")
	}
	zeta := &store.Organization{ID: newOrgID(), Name: "Zeta"}
	beta := &store.Organization{ID: newOrgID(), Name: "Beta"}
	mustTenant(t, ts.CreateOrganizationWithAdmin(ctx, zeta, "alice"))
	mustTenant(t, ts.CreateOrganizationWithAdmin(ctx, beta, "alice"))
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: zeta.ID, UserID: "bob", Role: store.RoleOperator, Status: "active"}))
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: zeta.ID, UserID: "carol", Role: store.RoleReadOnly, Status: "disabled"}))

	got, err := ts.ListOrganizations(ctx)
	mustTenant(t, err)
	// The migration seeds the initial organization with no members on a fresh database.
	want := []struct {
		id, name string
		members  int
	}{{beta.ID, "Beta", 1}, {store.InitialOrganizationID, "Default organization", 0}, {zeta.ID, "Zeta", 2}}
	if len(got) != len(want) {
		t.Fatalf("organizations: %+v", got)
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].Name != w.name || got[i].Members != w.members || got[i].CreatedAt.IsZero() {
			t.Fatalf("organization %d: got %+v, want %+v", i, got[i], w)
		}
	}
}

func TestCreateOrganizationWithAdminConcurrentSameName(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	tenantUser(t, st, "alice", "user", "local", "active")
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = ts.CreateOrganizationWithAdmin(ctx, &store.Organization{ID: newOrgID(), Name: "Race"}, "alice")
		}()
	}
	wg.Wait()
	ok, dup := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrAlreadyExists):
			dup++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || dup != 1 {
		t.Fatalf("want one success and one ErrAlreadyExists, got %v", errs)
	}
	orgs, err := ts.ListOrganizations(ctx)
	mustTenant(t, err)
	rows := 0
	for _, o := range orgs {
		if o.Name == "Race" {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("want one Race row, got %d", rows)
	}
}
