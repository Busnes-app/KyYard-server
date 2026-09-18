package store

import (
	"context"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
	"testing"
)

func TestApplicationDatabaseScopeAndAtomicAudit(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db := st.(*SQLStore)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"a", "b"} {
		must(st.Tenancy().CreateOrganization(ctx, &Organization{ID: org, Name: org}))
		must(st.Tenancy().CreateEnvironment(ctx, &Environment{ID: "env-" + org, OrganizationID: org, Name: "prod"}))
	}
	must(st.Users().CreateUser(ctx, &User{ID: "actor", Username: "actor", Role: "user", Status: "active"}))
	must(st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: "a", UserID: "actor", Role: RoleOrganizationAdmin, Status: "active"}))
	a := TenantAccess{ActorID: "actor", OrganizationID: "a", EnvironmentID: "env-a"}
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}
	app, err := st.Tenancy().CreateApplication(ctx, a, "shop", spec)
	must(err)
	// The database refuses a foreign environment even when a future caller omits its scope check.
	_, err = db.db.ExecContext(ctx, db.rebind(`INSERT INTO applications(id,organization_id,environment_id,name,latest_revision,created_by,created_at) VALUES(?,?,?,?,1,?,?)`), "bad", "a", "env-b", "bad", "actor", app.CreatedAt)
	if err == nil {
		t.Fatal("cross-tenant application insert accepted")
	}
	_, err = db.db.ExecContext(ctx, db.rebind(`INSERT INTO application_revisions(id,application_id,organization_id,environment_id,number,spec,digest,created_by,created_at) VALUES(?,?,?,?,2,'{}','x',?,?)`), "bad-revision", app.ID, "b", "env-b", "actor", app.CreatedAt)
	if err == nil {
		t.Fatal("cross-tenant revision insert accepted")
	}
	_, err = db.db.ExecContext(ctx, `DROP TABLE audit_records`)
	must(err)
	if _, err = st.Tenancy().CreateApplication(ctx, a, "audit-failure", spec); err == nil {
		t.Fatal("creation ignored failed audit")
	}
	if _, err = st.Tenancy().AppendApplicationRevision(ctx, a, app.ID, 1, spec); err == nil {
		t.Fatal("revision ignored failed audit")
	}
	var apps, revisions, head int
	must(db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM applications`).Scan(&apps))
	must(db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM application_revisions`).Scan(&revisions))
	must(db.db.QueryRowContext(ctx, `SELECT latest_revision FROM applications`).Scan(&head))
	if apps != 1 || revisions != 1 || head != 1 {
		t.Fatalf("partial audit failure: apps=%d revisions=%d head=%d", apps, revisions, head)
	}
}
