package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Busness-app/kyyard-server/internal/permissions"
	"github.com/Busness-app/kyyard-server/internal/store/migrations"
	"github.com/Busness-app/kyyard-server/internal/testdb"
)

func tenantAtomicStore(t *testing.T) (*SQLStore, TenantAccess) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.Users().CreateUser(ctx, &User{ID: "actor", Username: "actor", Role: "user", Status: "active", SSOProvider: "local"}))
	must(st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: InitialOrganizationID, UserID: "actor", Role: RoleOrganizationAdmin, Status: "active"}))
	must(st.Tenancy().CreateEnvironment(ctx, &Environment{ID: "environment", OrganizationID: InitialOrganizationID, Name: "Before"}))
	return st.(*SQLStore), TenantAccess{ActorID: "actor", OrganizationID: InitialOrganizationID, EnvironmentID: "environment", CorrelationID: "test-request"}
}
func TestTenantMutationAndAuditAreAtomic(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ddl := `CREATE TRIGGER fail_scoped_audit BEFORE INSERT ON audit_records WHEN NEW.correlation_id='fail-audit' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END;`
	if st.Driver() == "postgres" {
		ddl = `CREATE FUNCTION fail_scoped_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.correlation_id='fail-audit' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_scoped_audit BEFORE INSERT ON audit_records FOR EACH ROW EXECUTE FUNCTION fail_scoped_audit();`
	}
	if _, err := st.db.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	a.CorrelationID = "fail-audit"
	if err := st.Tenancy().UpdateEnvironment(ctx, a, "Must roll back"); err == nil {
		t.Fatal("mutation succeeded without audit")
	}
	e, err := st.Tenancy().GetEnvironment(ctx, a.OrganizationID, a.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "Before" {
		t.Fatal("unaudited mutation persisted")
	}
	a.CorrelationID = "working"
	if err := st.Tenancy().UpdateEnvironment(ctx, a, "After"); err != nil {
		t.Fatal(err)
	}
}
func TestTenantRevocationSerializesWithMutation(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	checked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	go func() {
		done <- (&tenancyStore{store: st}).withTenant(ctx, a, permissions.EnvironmentUpdate, func(tx *sql.Tx) error {
			close(checked)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := tx.ExecContext(ctx, st.rebind(`UPDATE environments SET name=? WHERE organization_id=? AND id=?`), "Authorized", a.OrganizationID, a.EnvironmentID)
			return err
		})
	}()
	select {
	case <-checked:
	case <-ctx.Done():
		t.Fatal("authorization did not start")
	}
	started, revoked := make(chan struct{}), make(chan error, 1)
	go func() { close(started); revoked <- st.Tenancy().DeleteMembership(ctx, a.OrganizationID, a.ActorID) }()
	<-started
	select {
	case err := <-revoked:
		t.Fatalf("revocation bypassed the authorization lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().UpdateEnvironment(ctx, a, "Forbidden"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked actor retained access: %v", err)
	}
}

func TestScopedAuditMigrationPreservesHistory(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	// Reconstruct the v5 audit table, insert a legacy event, then run the real upgrade.
	for _, ddl := range []string{
		"DROP INDEX idx_audit_scope_created",
		"ALTER TABLE audit_records DROP COLUMN scope",
		"ALTER TABLE audit_records DROP COLUMN organization_id",
		"ALTER TABLE audit_records DROP COLUMN environment_id",
		"ALTER TABLE audit_records DROP COLUMN correlation_id",
		"ALTER TABLE audit_records DROP COLUMN result",
		"DELETE FROM schema_migrations WHERE version=6",
	} {
		if _, err := st.db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err := st.db.ExecContext(ctx, st.rebind("INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at) VALUES (?,?,?,?,?,?)"), "actor", "legacy.action", "target", "original details", "127.0.0.1", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, st.db, st.Driver()); err != nil {
		t.Fatal(err)
	}
	var r AuditRecord
	err = st.db.QueryRowContext(ctx, "SELECT user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result FROM audit_records WHERE action='legacy.action'").Scan(&r.UserID, &r.Action, &r.Resource, &r.Details, &r.IPAddress, &r.CreatedAt, &r.Scope, &r.OrganizationID, &r.EnvironmentID, &r.CorrelationID, &r.Result)
	if err != nil {
		t.Fatal(err)
	}
	if r.UserID != "actor" || r.Resource != "target" || r.Details != "original details" || r.IPAddress != "127.0.0.1" || !r.CreatedAt.Equal(at) || r.Scope != "platform" || r.Result != "unknown" || r.OrganizationID != "" || r.EnvironmentID != "" || r.CorrelationID != "" {
		t.Fatalf("legacy audit meaning changed: %+v", r)
	}
}
