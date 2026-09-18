package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store/migrations"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
	"strings"
	"testing"
	"time"
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
	if err = st.Tenancy().DiscardApplication(ctx, a, app.ID, 1); err == nil {
		t.Fatal("discard ignored failed audit")
	}
	var apps, revisions, head int
	must(db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM applications`).Scan(&apps))
	must(db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM application_revisions`).Scan(&revisions))
	must(db.db.QueryRowContext(ctx, `SELECT latest_revision FROM applications`).Scan(&head))
	if apps != 1 || revisions != 1 || head != 1 {
		t.Fatalf("partial audit failure: apps=%d revisions=%d head=%d", apps, revisions, head)
	}
}

func TestApplicationRevisionDigestMismatch(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}
	app, err := st.Tenancy().CreateApplication(ctx, a, "shop", spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.ExecContext(ctx, st.rebind(`UPDATE application_revisions SET spec=? WHERE application_id=?`), `{"kind":"compose.v1","services":[{"name":"web","image":"nginx:tampered"}]}`, app.ID); err != nil {
		t.Fatal(err)
	}
	if revision, err := st.Tenancy().ReadApplicationRevision(ctx, a, app.ID, 1); !errors.Is(err, ErrRevisionCorrupt) || revision != nil {
		t.Fatal("tampered spec accepted with old digest")
	}
}
func TestApplicationAuditTargetBounds(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}
	if _, err := st.Tenancy().AppendApplicationRevision(ctx, a, strings.Repeat("a", 300), 1, spec); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid application id: %v", err)
	}
	if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	err := st.Tenancy().(*tenancyStore).withTenantTarget(ctx, a, permissions.ApplicationEdit, strings.Repeat("界", 300), func(*sql.Tx) error { return nil })
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("long denial: %v", err)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range records {
		if r.Action == "application.edit" && r.Result == "denied" {
			count++
			if len(r.Resource) > 255 {
				t.Fatal("unbounded resource")
			}
		}
	}
	if count != 1 {
		t.Fatalf("denials=%d", count)
	}
}

func TestAuditResourceBoundPreservesLegacy(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	drop := `DROP TRIGGER audit_resource_bound`
	if st.Driver() == "postgres" {
		drop = `ALTER TABLE audit_records DROP CONSTRAINT audit_resource_bound`
	}
	if _, err := st.db.ExecContext(ctx, drop); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=19`); err != nil {
		t.Fatal(err)
	}
	resource := strings.Repeat("界", 100)
	insert := st.rebind(`INSERT INTO audit_records(user_id,action,resource,created_at) VALUES('actor',?,?,?)`)
	if _, err := st.db.ExecContext(ctx, insert, "legacy", resource, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, st.db, st.Driver()); err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := st.db.QueryRowContext(ctx, `SELECT resource FROM audit_records WHERE action='legacy'`).Scan(&saved); err != nil || saved != resource {
		t.Fatalf("legacy history changed: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, insert, "new", resource, time.Now().UTC()); err == nil {
		t.Fatal("unbounded raw audit insert")
	}
	if err := st.Audit().LogAudit(ctx, &AuditRecord{UserID: "actor", Action: "bounded", Resource: resource}); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT resource FROM audit_records WHERE action='bounded'`).Scan(&saved); err != nil || len(saved) > 255 {
		t.Fatalf("audit resource not bounded: %v", err)
	}
}
