package store_test

import (
	"context"
	"errors"
	"testing"

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
	if err := st.Users().UpdateUserIdentity(ctx, stale); err != nil {
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

func TestDirectoryAccountChangesKeepAnOrganizationAdministrator(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	mustTenant(t, ts.CreateOrganization(ctx, &store.Organization{ID: "o", Name: "o"}))
	for _, id := range []string{"sole", "member", "idle"} {
		tenantUser(t, st, id, "user", "kysignon", "active")
	}
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "o", UserID: "sole", Role: store.RoleOrganizationAdmin, Status: "active"}))
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "o", UserID: "member", Role: store.RoleReadOnly, Status: "active"}))

	sole, err := st.Users().GetUserByID(ctx, "sole")
	mustTenant(t, err)
	sole.Status = "inactive"
	if err := st.Users().UpdateUserIdentity(ctx, sole); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("sole administrator deactivated: %v", err)
	}
	if err := st.Users().DeleteUser(ctx, "sole"); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("sole administrator deleted: %v", err)
	}
	if got, err := st.Users().GetUserByID(ctx, "sole"); err != nil || got.Status != "active" {
		t.Fatalf("refusal changed the account: %v %+v", err, got)
	}
	// Accounts that administer nothing are unaffected.
	if err := st.Users().DeleteUser(ctx, "member"); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().DeleteUser(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	// With a second active administrator both changes go through.
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "o", UserID: "idle", Role: store.RoleOrganizationAdmin, Status: "active"}))
	if err := st.Users().UpdateUserIdentity(ctx, sole); err != nil {
		t.Fatal(err)
	}
	// An inactive administrator was not counted, so removing them changes nothing.
	if err := st.Users().DeleteUser(ctx, "sole"); err != nil {
		t.Fatal(err)
	}
}
