package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/sso"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// directorySync delivers a signed KySignOn directory webhook event for one external account.
func directorySync(t *testing.T, st store.Store) func(event, id, username, status string) {
	secret := "independent-directory-fixture-secret"
	client := sso.NewKySignOnClient(config.SSOConfig{KySignOnHMACSecret: secret}, st)
	return func(event, id, username, status string) {
		t.Helper()
		body, _ := json.Marshal(sso.KySignOnSyncPayload{EventID: crypto.RandomHex(8), Event: event, ID: id, Username: username, Role: "admin", Status: status, Timestamp: time.Now().Unix()})
		mustTenant(t, client.HandleSyncWebhook(context.Background(), body, crypto.ComputeHMACSHA256(body, secret)))
	}
}

// External identity provisions accounts, never tenant membership. Membership is granted by an
// organization administrator; deactivation denies access live and leaves the grant in place.
func TestExternalIdentityNeverGrantsTenantAccess(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	tenantUser(t, st, "admin", "admin", "local", "active")
	mustTenant(t, ts.Initialize(ctx))

	syncAs := directorySync(t, st)
	sync := func(event string) { syncAs(event, "ext-1", "sso_admin", "active") }
	sync("user.created")
	ssoUser, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-1")
	mustTenant(t, err)
	if ssoUser.Role != "admin" || ssoUser.Status != "active" {
		t.Fatalf("provisioning did not apply the external role: %+v", ssoUser)
	}
	if orgs, err := ts.ListMemberOrganizations(ctx, ssoUser.ID); err != nil || len(orgs) != 0 {
		t.Fatalf("external identity received organizations: %v %+v", err, orgs)
	}
	if _, err := ts.ReadOrganization(ctx, store.TenantAccess{ActorID: ssoUser.ID, OrganizationID: store.InitialOrganizationID}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("external platform admin read the default organization: %v", err)
	}
	// A repeated bootstrap never picks up the external administrator.
	mustTenant(t, ts.Initialize(ctx))
	if _, err := ts.GetMembership(ctx, store.InitialOrganizationID, ssoUser.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bootstrap granted an external admin: %v", err)
	}

	// An organization administrator grants membership explicitly; the directory's deactivation
	// then denies access live without touching the grant, and reactivation needs no re-grant.
	localAdmin := store.TenantAccess{ActorID: "admin", OrganizationID: store.InitialOrganizationID}
	mustTenant(t, ts.PutMembership(ctx, localAdmin, ssoUser.ID, store.RoleDeveloper, "active"))
	ssoAccess := store.TenantAccess{ActorID: ssoUser.ID, OrganizationID: store.InitialOrganizationID}
	_, err = ts.ReadOrganization(ctx, ssoAccess)
	mustTenant(t, err)
	sync("user.deactivated")
	if _, err := ts.ReadOrganization(ctx, ssoAccess); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("deactivated SSO user kept tenant access: %v", err)
	}
	if m, err := ts.GetMembership(ctx, store.InitialOrganizationID, ssoUser.ID); err != nil || m.Status != "active" || m.Role != store.RoleDeveloper {
		t.Fatalf("webhook deactivation altered the grant: %v %+v", err, m)
	}
	sync("user.updated")
	_, err = ts.ReadOrganization(ctx, ssoAccess)
	mustTenant(t, err)

	// Deletion removes the account and cascades its grant; re-provisioning is a new identity.
	sync("user.deleted")
	if _, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted SSO user still present: %v", err)
	}
	if _, err := ts.GetMembership(ctx, store.InitialOrganizationID, ssoUser.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted SSO user's grant survived: %v", err)
	}
	sync("user.created")
	recreated, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-1")
	mustTenant(t, err)
	if recreated.ID == ssoUser.ID {
		t.Fatal("re-provisioned account reused the old identity")
	}
	if orgs, err := ts.ListMemberOrganizations(ctx, recreated.ID); err != nil || len(orgs) != 0 {
		t.Fatalf("re-provisioned account inherited organizations: %v %+v", err, orgs)
	}
}

// External deactivation bypasses the last-administrator guard: the guard only governs
// membership writes, and user status is the IdP's. This pins the resulting lockout so the
// platform repair route in docs/authorization-matrix.md is a tracked requirement, not a note.
func TestExternalDeactivationCanLockOutAnOrganization(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	tenantUser(t, st, "admin", "admin", "local", "active")
	mustTenant(t, ts.CreateOrganization(ctx, &store.Organization{ID: "b", Name: "B"}))

	sync := directorySync(t, st)
	sync("user.created", "ext-sole", "sole_admin", "active")
	sole, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-sole")
	mustTenant(t, err)
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: sole.ID, Role: store.RoleOrganizationAdmin, Status: "active"}))
	tenantUser(t, st, "colleague", "user", "local", "active")
	soleAccess := store.TenantAccess{ActorID: sole.ID, OrganizationID: "b"}
	mustTenant(t, ts.PutMembership(ctx, soleAccess, "colleague", store.RoleReadOnly, "active"))

	sync("user.deactivated", "ext-sole", "sole_admin", "")

	// Nobody can write memberships in b any more: not the deactivated administrator, not the
	// remaining member, not the platform administrator.
	for _, actor := range []string{sole.ID, "colleague", "admin"} {
		err := ts.PutMembership(ctx, store.TenantAccess{ActorID: actor, OrganizationID: "b"}, "colleague", store.RoleOrganizationAdmin, "active")
		if !errors.Is(err, store.ErrForbidden) {
			t.Fatalf("%s repaired the organization without a repair route: %v", actor, err)
		}
	}
	if m, err := ts.GetMembership(ctx, "b", sole.ID); err != nil || m.Status != "active" {
		t.Fatalf("grant should be intact for reactivation: %v %+v", err, m)
	}
}
