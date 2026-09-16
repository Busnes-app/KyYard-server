package store_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/Busness-app/kyyard-server/internal/store"
	"github.com/Busness-app/kyyard-server/internal/testdb"
)

func tenantUser(t *testing.T, st store.Store, id, role, provider, status string) {
	t.Helper()
	if err := st.Users().CreateUser(context.Background(), &store.User{ID: id, Username: id, Role: role, SSOProvider: provider, Status: status}); err != nil {
		t.Fatal(err)
	}
}
func mustTenant(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestTenancyBootstrapOnce(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	if err := ts.Initialize(ctx); err == nil {
		t.Fatal("initialized without an account")
	}
	tenantUser(t, st, "ordinary", "user", "local", "active")
	tenantUser(t, st, "federated", "admin", "kysignon", "active")
	tenantUser(t, st, "disabled", "admin", "local", "disabled")
	tenantUser(t, st, "other-admin", "admin", "local", "active")
	tenantUser(t, st, "admin", "admin", "local", "active")
	mustTenant(t, st.Groups().CreateGroup(ctx, &store.Group{ID: "global", DisplayName: "Administrators"}))
	mustTenant(t, st.Groups().AddGroupMember(ctx, "global", "ordinary"))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ts.Initialize(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	m, err := ts.GetMembership(ctx, store.InitialOrganizationID, "admin")
	mustTenant(t, err)
	if m.Role != store.RoleOrganizationAdmin || m.Status != "active" {
		t.Fatalf("unexpected membership %+v", m)
	}
	for _, id := range []string{"ordinary", "federated", "disabled", "other-admin"} {
		if _, err := ts.GetMembership(ctx, store.InitialOrganizationID, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unexpected grant to %s: %v", id, err)
		}
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 100)
	mustTenant(t, err)
	count := 0
	for _, r := range records {
		if r.Action == "organization.bootstrap" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("bootstrap audit count %d", count)
	}
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: store.InitialOrganizationID, UserID: "admin", Role: store.RoleReadOnly, Status: "disabled"}))
	mustTenant(t, ts.Initialize(ctx))
	m, err = ts.GetMembership(ctx, store.InitialOrganizationID, "admin")
	mustTenant(t, err)
	if m.Status != "disabled" || m.Role != store.RoleReadOnly {
		t.Fatal("restart restored a disabled or demoted membership")
	}
	mustTenant(t, ts.DeleteMembership(ctx, store.InitialOrganizationID, "admin"))
	mustTenant(t, ts.Initialize(ctx))
	if _, err := ts.GetMembership(ctx, store.InitialOrganizationID, "admin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("restart restored removed membership")
	}
}

func TestTenancyDoesNotPromoteExternalUsers(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tenantUser(t, st, "external", "admin", "oidc", "active")
	mustTenant(t, st.Tenancy().Initialize(ctx))
	tenantUser(t, st, "admin", "admin", "local", "active")
	mustTenant(t, st.Tenancy().Initialize(ctx))
	for _, id := range []string{"external", "admin"} {
		if _, err := st.Tenancy().GetMembership(ctx, store.InitialOrganizationID, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("late or external account granted automatic membership")
		}
	}
}

func TestTenancyScopedPersistence(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	for _, org := range []string{"a", "b"} {
		mustTenant(t, ts.CreateOrganization(ctx, &store.Organization{ID: org, Name: org}))
		o, err := ts.GetOrganization(ctx, org)
		mustTenant(t, err)
		if o.ID != org {
			t.Fatal("wrong organization")
		}
	}
	tenantUser(t, st, "alice", "user", "local", "active")
	tenantUser(t, st, "bob", "user", "local", "active")
	for _, role := range []store.TenantRole{store.RoleOrganizationAdmin, store.RoleEnvironmentAdmin, store.RoleOperator, store.RoleDeveloper, store.RoleReadOnly} {
		mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "alice", Role: role, Status: "active"}))
	}
	for _, m := range []*store.OrganizationMembership{
		{OrganizationID: "a", UserID: "alice", Role: "admin", Status: "active"},
		{OrganizationID: "a", UserID: "alice", Role: store.RoleReadOnly, Status: "unknown"},
		{OrganizationID: "missing", UserID: "alice", Role: store.RoleReadOnly, Status: "active"},
	} {
		if err := ts.SetMembership(ctx, m); err == nil {
			t.Fatal("invalid membership accepted")
		}
	}
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "bob", Role: store.RoleOperator, Status: "active"}))
	if _, err := ts.GetMembership(ctx, "b", "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("membership crossed organizations")
	}
	if err := ts.DeleteMembership(ctx, "b", "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("membership delete crossed organizations")
	}
	for _, org := range []string{"a", "b"} {
		mustTenant(t, ts.CreateEnvironment(ctx, &store.Environment{ID: "env-" + org, OrganizationID: org, Name: "Production"}))
		mustTenant(t, ts.CreateOrganizationGroup(ctx, &store.OrganizationGroup{ID: "group-" + org, OrganizationID: org, Name: "Operators"}))
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "duplicate", OrganizationID: "a", Name: "Production"}); err == nil {
		t.Fatal("duplicate environment name accepted")
	}
	if err := ts.CreateOrganizationGroup(ctx, &store.OrganizationGroup{ID: "duplicate", OrganizationID: "a", Name: "Operators"}); err == nil {
		t.Fatal("duplicate group name accepted")
	}
	if _, err := ts.GetEnvironment(ctx, "b", "env-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-tenant read")
	}
	if err := ts.RenameEnvironment(ctx, "b", "env-a", "Stolen"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-tenant rename")
	}
	if err := ts.DeleteEnvironment(ctx, "b", "env-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-tenant delete")
	}
	mustTenant(t, ts.RenameEnvironment(ctx, "a", "env-a", "Development"))
	e, err := ts.GetEnvironment(ctx, "a", "env-a")
	mustTenant(t, err)
	if e.Name != "Development" {
		t.Fatal("rename missing")
	}
	for _, args := range [][3]string{{"a", "group-b", "alice"}, {"a", "group-a", "bob"}, {"b", "group-a", "bob"}} {
		if err := ts.AddOrganizationGroupMember(ctx, args[0], args[1], args[2]); err == nil {
			t.Fatal("cross-tenant group reference accepted")
		}
	}
	mustTenant(t, ts.AddOrganizationGroupMember(ctx, "a", "group-a", "alice"))
	ids, err := ts.ListOrganizationGroupMembers(ctx, "b", "group-a")
	mustTenant(t, err)
	if len(ids) != 0 {
		t.Fatal("group members leaked")
	}
	if err := ts.RemoveOrganizationGroupMember(ctx, "b", "group-a", "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-tenant group deletion")
	}
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "alice", Role: store.RoleReadOnly, Status: "disabled"}))
	m, err := ts.GetMembership(ctx, "a", "alice")
	mustTenant(t, err)
	if m.Status != "disabled" {
		t.Fatal("membership re-enabled")
	}
	mustTenant(t, ts.DeleteMembership(ctx, "a", "alice"))
	ids, err = ts.ListOrganizationGroupMembers(ctx, "a", "group-a")
	mustTenant(t, err)
	if len(ids) != 0 {
		t.Fatal("removed member retains group link")
	}
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "alice", Role: store.RoleReadOnly, Status: "active"}))
	ids, err = ts.ListOrganizationGroupMembers(ctx, "a", "group-a")
	mustTenant(t, err)
	if len(ids) != 0 {
		t.Fatal("rejoining restored prior group grants")
	}
	mustTenant(t, ts.DeleteEnvironment(ctx, "a", "env-a"))
}

func TestTenancyUpgradeAndReopen(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	st, err := store.Open(ctx, cfg)
	mustTenant(t, err)
	tenantUser(t, st, "admin", "admin", "local", "active")
	tenantUser(t, st, "ordinary", "user", "local", "active")
	mustTenant(t, st.Close())
	driver := cfg.Driver
	if driver == "postgres" {
		driver = "pgx"
	}
	db, err := sql.Open(driver, cfg.DSN)
	mustTenant(t, err)
	defer db.Close()
	// Reconstruct the v4 schema while preserving existing accounts, then exercise real Open.
	// Migration 7's tables reference environments, so they go first and are replayed too.
	for _, q := range []string{"DROP TABLE endpoint_events", "DROP TABLE endpoint_capabilities", "DROP TABLE agent_enrollment_tokens", "DROP TABLE endpoint_keys", "DROP TABLE endpoints", "DROP TABLE organization_group_members", "DROP TABLE organization_groups", "DROP TABLE environments", "DROP TABLE organization_memberships", "DROP TABLE tenancy_bootstrap", "DROP TABLE organizations", "DELETE FROM schema_migrations WHERE version IN (5,7,8,9)"} {
		_, err := db.ExecContext(ctx, q)
		mustTenant(t, err)
	}
	st, err = store.Open(ctx, cfg)
	mustTenant(t, err)
	mustTenant(t, st.Tenancy().Initialize(ctx))
	mustTenant(t, st.Close())
	st, err = store.Open(ctx, cfg)
	mustTenant(t, err)
	defer st.Close()
	mustTenant(t, st.Tenancy().Initialize(ctx))
	_, err = st.Tenancy().GetMembership(ctx, store.InitialOrganizationID, "admin")
	mustTenant(t, err)
	if _, err := st.Tenancy().GetMembership(ctx, store.InitialOrganizationID, "ordinary"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("upgrade granted ordinary user")
	}
	var count int
	mustTenant(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version=5").Scan(&count))
	if count != 1 {
		t.Fatal("migration not recorded exactly once")
	}
}

func TestTenancyBootstrapRollsBack(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	tenantUser(t, st, "admin", "admin", "local", "active")
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: store.InitialOrganizationID, UserID: "admin", Role: store.RoleReadOnly, Status: "active"}))
	if err := ts.Initialize(ctx); err == nil {
		t.Fatal("bootstrap overwrote an existing membership")
	}
	m, err := ts.GetMembership(ctx, store.InitialOrganizationID, "admin")
	mustTenant(t, err)
	if m.Role != store.RoleReadOnly {
		t.Fatal("failed bootstrap changed existing role")
	}
	mustTenant(t, ts.DeleteMembership(ctx, store.InitialOrganizationID, "admin"))
	mustTenant(t, ts.Initialize(ctx))
	m, err = ts.GetMembership(ctx, store.InitialOrganizationID, "admin")
	mustTenant(t, err)
	if m.Role != store.RoleOrganizationAdmin {
		t.Fatal("failed bootstrap consumed its retry marker")
	}
}
