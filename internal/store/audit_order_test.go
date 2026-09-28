package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
	"github.com/google/uuid"
)

// Hold a lower ID uncommitted, observe a later writer actually waiting in
// PostgreSQL's lock graph, and page before and after release. A timer is only a
// failure deadline; it never establishes the overlap or the expected ordering.
func TestAuditCommitOrder(t *testing.T) {
	for _, kind := range []string{"generic", "tenant", "password_change", "password_reset", "service_summary", "agent_connect"} {
		t.Run(kind, func(t *testing.T) { testAuditCommitOrder(t, kind) })
	}
}

func testAuditCommitOrder(t *testing.T, kind string) {
	cfg := testdb.Config(t)
	if cfg.Driver != "postgres" {
		t.Skip("PostgreSQL sequence/commit ordering")
	}
	application := "audit_order_" + uuid.NewString()
	cfg.DSN += "&application_name=" + application
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	opened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	s := opened.(*SQLStore)
	ts := s.Tenancy().(*tenancyStore)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Users().CreateUser(ctx, &User{ID: "actor", Username: "actor", PasswordHash: "old", Role: "admin", Status: "active", SSOProvider: "local"}))
	must(s.Users().CreateUser(ctx, &User{ID: "password", Username: "password", PasswordHash: "old", Role: "user", Status: "active", SSOProvider: "local", MustChangePassword: true}))
	must(ts.CreateOrganization(ctx, &Organization{ID: "org", Name: "org"}))
	must(ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: "org", UserID: "actor", Role: RoleOrganizationAdmin, Status: "active"}))
	a := TenantAccess{ActorID: "actor", OrganizationID: "org"}
	pairing, err := ts.CreateServicePairing(ctx, a)
	must(err)
	issue, err := ts.ClaimServiceToken(ctx, pairing.Code, "kypulse", "")
	must(err)
	tok, err := ts.LookupServiceToken(ctx, issue.Token)
	must(err)
	reader := TenantAccess{ServiceTokenID: tok.ID, OrganizationID: "org"}
	type held struct {
		id  int64
		pid int
	}
	allocated := make(chan held, 1)
	release := make(chan struct{})
	lowerDone := make(chan error, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		if err := <-lowerDone; err != nil {
			t.Error(err)
		}
	}()
	go func() {
		lowerDone <- ts.withTenant(ctx, a, permissions.EnvironmentCreate, func(tx *sql.Tx) error {
			var h held
			if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&h.pid); err != nil {
				return err
			}
			err := tx.QueryRowContext(ctx, `INSERT INTO audit_records(user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES ('actor','lower','','','',CURRENT_TIMESTAMP,'organization','org','','','success') RETURNING id`).Scan(&h.id)
			if err != nil {
				return err
			}
			allocated <- h
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			// The credential writer must not hold this row while waiting for audit: that
			// would invert the lock order and deadlock this already-audited transaction.
			_, err = tx.ExecContext(ctx, `UPDATE users SET display_name='ordered' WHERE id='password'`)
			return err
		})
	}()
	var lower held
	select {
	case lower = <-allocated:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	higherDone := make(chan error, 1)
	expected := map[string]string{"generic": "higher", "tenant": "environment.create", "password_change": "auth.password_changed", "password_reset": "auth.password_changed", "service_summary": "service_token.reads", "agent_connect": "agent.connect"}[kind]
	go func() {
		var err error
		switch kind {
		case "generic":
			err = s.Audit().LogAudit(ctx, &AuditRecord{Action: "higher", Scope: "organization", OrganizationID: "org"})
		case "tenant":
			_, err = ts.AddEnvironment(ctx, a, "later")
		case "password_change":
			err = s.Users().CompletePasswordChange(ctx, "password", "old", "new", "")
		case "password_reset":
			err = s.Users().ResetAdminPassword(ctx, "actor", "new")
		case "service_summary":
			err = ts.RecordServiceTokenReads(ctx, "org", tok.ID, 5)
		case "agent_connect":
			err = ts.RecordAgentConnect(ctx, &Endpoint{ID: "endpoint", OrganizationID: "org"}, "", "success", "")
		}
		higherDone <- err
	}()
	for {
		select {
		case err := <-higherDone:
			must(err)
			t.Fatal("higher audit committed while lower ID remained uncommitted: cursor would skip lower")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
		}
		var waiting bool
		must(s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks w JOIN pg_locks h ON w.locktype=h.locktype AND w.database=h.database AND w.classid=h.classid AND w.objid=h.objid AND w.objsubid=h.objsubid WHERE w.locktype='advisory' AND NOT w.granted AND h.granted AND w.pid<>h.pid AND h.pid=$1 AND w.classid=7345103 AND w.pid IN (SELECT pid FROM pg_stat_activity WHERE application_name=$2))`, lower.pid, application).Scan(&waiting))
		if waiting {
			break
		}
	}
	cursor := lower.id - 1
	page, err := ts.ReadAuditAfter(ctx, reader, cursor, 1)
	must(err)
	if len(page) != 0 {
		t.Fatalf("cursor passed uncommitted ID: %+v", page)
	}
	close(release)
	select {
	case err := <-higherDone:
		must(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	foundLower, foundHigher := false, false
	for {
		page, err = ts.ReadAuditAfter(ctx, reader, cursor, 1)
		must(err)
		if len(page) == 0 {
			break
		}
		row := page[0]
		if row.ID <= cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = row.ID
		foundLower = foundLower || row.Action == "lower"
		foundHigher = foundHigher || (row.Action == expected && row.ID > lower.id+1)
	}
	if !foundLower {
		t.Fatal("cursor skipped late lower commit")
	}
	if kind == "password_change" || kind == "password_reset" {
		// Credential events stay platform-scoped, but share the global ID allocator.
		var id int64
		must(s.db.QueryRowContext(ctx, `SELECT id FROM audit_records WHERE action='auth.password_changed'`).Scan(&id))
		foundHigher = id > lower.id+1
	}
	if !foundHigher {
		t.Fatal("missing later audit event")
	}
}
