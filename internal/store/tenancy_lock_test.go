package store

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// On PostgreSQL the first admin's status is read under a row lock: a status change committed while
// the creation waits is seen, and no organization is created for a disabled admin.
func TestCreateOrganizationWithAdminLocksTheUser(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	if st.driver != "postgres" {
		t.Skip("row locks are PostgreSQL's; SQLite serializes writers")
	}
	ctx := context.Background()
	holder, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	var status string
	if err := holder.QueryRowContext(ctx, st.rebind(`SELECT status FROM users WHERE id=? FOR UPDATE`), "actor").Scan(&status); err != nil || status != "active" {
		t.Fatalf("hold: %s %v", status, err)
	}
	done := make(chan error, 1)
	go func() {
		done <- st.Tenancy().CreateOrganizationWithAdmin(ctx, &Organization{ID: "org_locked", Name: "Locked"}, "actor")
	}()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM users%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the creation never waited on the admin's row")
		}
		runtime.Gosched()
	}
	if _, err := holder.ExecContext(ctx, st.rebind(`UPDATE users SET status='disabled' WHERE id=?`), "actor"); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrInvalid) {
		t.Fatalf("creation for a disabled admin: %v", err)
	}
	var n int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM organizations WHERE id=?`), "org_locked").Scan(&n); err != nil || n != 0 {
		t.Fatalf("organizations: %d %v", n, err)
	}
}
