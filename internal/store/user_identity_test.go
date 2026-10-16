package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestIdentityUpdateKeepsConcurrentCredentialWrites(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.Users().CreateUser(ctx, &store.User{ID: "u", Username: "u", PasswordHash: "old", Role: "user", Status: "active", SSOProvider: "local", RecoveryCodesHash: "codes-1"}); err != nil {
		t.Fatal(err)
	}
	stale, err := st.Users().GetUserByID(ctx, "u")
	if err != nil {
		t.Fatal(err)
	}
	// Credential writers land between the directory's read and its write.
	if err := st.Users().ResetAdminPassword(ctx, "u", "new"); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().UpdateRecoveryCodes(ctx, "u", "codes-1", "codes-2"); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().SpendTOTPCounter(ctx, "u", 7); err != nil {
		t.Fatal(err)
	}
	stale.Email = "u@example.com"
	if err := directory(ctx, st, "e1", "u", func(tx store.DirectoryTx) error { return tx.UpdateUserIdentity(stale) }); err != nil {
		t.Fatal(err)
	}
	got, err := st.Users().GetUserByID(ctx, "u")
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "u@example.com" || got.PasswordHash != "new" || got.RecoveryCodesHash != "codes-2" || got.TOTPLastCounter != 7 || !got.MustChangePassword {
		t.Fatalf("identity update reverted a credential write: %+v", got)
	}
}

func TestOnlyLocalAccountChangesKeepAnOrganizationAdministrator(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	mustTenant(t, ts.CreateOrganization(ctx, &store.Organization{ID: "o", Name: "o"}))
	for _, id := range []string{"sole", "member"} {
		tenantUser(t, st, id, "user", "kysignon", "active")
	}
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "o", UserID: "sole", Role: store.RoleOrganizationAdmin, Status: "active"}))
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "o", UserID: "member", Role: store.RoleReadOnly, Status: "active"}))

	// A local deletion cannot remove the last administrator.
	if err := st.Users().DeleteUser(ctx, "sole"); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("local deletion of the sole administrator: %v", err)
	}
	// The identity provider's deactivation always applies, and says what the organization lost.
	sole, err := st.Users().GetUserByID(ctx, "sole")
	mustTenant(t, err)
	sole.Status = "inactive"
	mustTenant(t, directory(ctx, st, "e1", "sole", func(tx store.DirectoryTx) error { return tx.UpdateUserIdentity(sole) }))
	if got, err := st.Users().GetUserByID(ctx, "sole"); err != nil || got.Status != "inactive" {
		t.Fatalf("deactivation not applied: %v %+v", err, got)
	}
	lost := lastAdminLost(t, st)
	if len(lost) != 1 || lost[0].OrganizationID != "o" || lost[0].Resource != "sole" || lost[0].Scope != "organization" {
		t.Fatalf("admin loss audit: %+v", lost)
	}
	// Removing an administrator who no longer counts records nothing more.
	mustTenant(t, directory(ctx, st, "e2", "sole", func(tx store.DirectoryTx) error { return tx.DeleteUser("sole") }))
	if lost := lastAdminLost(t, st); len(lost) != 1 {
		t.Fatalf("audited a loss that already happened: %+v", lost)
	}
	if err := directory(ctx, st, "e3", "ghost", func(tx store.DirectoryTx) error { return tx.DeleteUser("ghost") }); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	// A local deletion of a non-administrator is unaffected.
	mustTenant(t, st.Users().DeleteUser(ctx, "member"))
}

// directory applies fn as one identity provider event.
func directory(ctx context.Context, st store.Store, id, subject string, fn func(store.DirectoryTx) error) error {
	return st.Users().ApplyDirectoryEvent(ctx, store.DirectoryEvent{Provider: "test", ID: id, Subject: subject, At: time.Now()}, fn)
}

func lastAdminLost(t *testing.T, st store.Store) []*store.AuditRecord {
	t.Helper()
	records, _, err := st.Audit().ListAuditRecords(context.Background(), 0, 100)
	mustTenant(t, err)
	var lost []*store.AuditRecord
	for _, r := range records {
		if r.Action == "organization.last_admin_lost" {
			lost = append(lost, r)
		}
	}
	return lost
}
