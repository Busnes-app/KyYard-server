package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
)

// Two administrators demoting themselves at once must leave one in place. Each
// transaction pauses after its own write so both reach the count together; the
// organization lock makes the second wait for the first to commit instead.
func TestLastAdminGuardSurvivesConcurrentDemotion(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.Users().CreateUser(ctx, &User{ID: "second", Username: "second", Role: "user", SSOProvider: "local", Status: "active"}))
	must(st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: "second", Role: RoleOrganizationAdmin, Status: "active"}))
	ts := &tenancyStore{store: st}
	var reached sync.WaitGroup
	reached.Add(2)
	release := make(chan struct{})
	results := make(chan error, 2)
	demote := func(actor string) {
		access := TenantAccess{ActorID: actor, OrganizationID: a.OrganizationID}
		results <- ts.withTenantTarget(ctx, access, permissions.MembersManage, actor, func(tx *sql.Tx) error {
			if err := ts.lockOrganization(ctx, tx, access.OrganizationID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, st.rebind(`UPDATE organization_memberships SET role='read_only' WHERE organization_id=? AND user_id=?`), access.OrganizationID, actor); err != nil {
				return err
			}
			reached.Done()
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return ts.requireActiveAdmin(ctx, tx, access.OrganizationID)
		})
	}
	go demote(a.ActorID)
	go demote("second")
	both := make(chan struct{})
	go func() { reached.Wait(); close(both) }()
	select {
	case <-both:
	case <-time.After(500 * time.Millisecond):
		// One transaction is waiting on the organization lock; let the other finish.
	}
	close(release)
	var ok, last int
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrLastAdmin):
				last++
			default:
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("demotions did not finish")
		}
	}
	var admins int
	must(st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM organization_memberships WHERE organization_id=? AND role='organization_admin' AND status='active'`), a.OrganizationID).Scan(&admins))
	if ok != 1 || last != 1 || admins != 1 {
		t.Fatalf("succeeded=%d lastAdmin=%d admins=%d, want 1/1/1", ok, last, admins)
	}
}
