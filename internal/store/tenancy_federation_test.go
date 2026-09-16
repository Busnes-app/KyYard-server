package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Busness-app/kyyard-server/internal/config"
	"github.com/Busness-app/kyyard-server/internal/crypto"
	"github.com/Busness-app/kyyard-server/internal/scim"
	"github.com/Busness-app/kyyard-server/internal/sso"
	"github.com/Busness-app/kyyard-server/internal/store"
)

// External identity provisions accounts, never tenant membership. Membership is granted by an
// organization administrator; deactivation denies access live and leaves the grant in place.
func TestExternalIdentityNeverGrantsTenantAccess(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := st.Tenancy()
	tenantUser(t, st, "admin", "admin", "local", "active")
	mustTenant(t, ts.Initialize(ctx))

	// SCIM through the real handler, with an IdP-supplied platform role.
	token := "scim-secret"
	scimServer := scim.NewServer(st, config.SCIMConfig{Enabled: true, BearerToken: token}, "http://localhost")
	mux := http.NewServeMux()
	scimServer.RegisterRoutes(mux)
	handler := scimServer.AuthMiddleware(mux)
	scimCall := func(method, path string, payload any) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/scim+json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	w := scimCall("POST", "/scim/v2/Users", map[string]any{"schemas": []string{scim.SchemaUser}, "userName": "scim_admin", "active": true, "roles": []map[string]any{{"value": "admin"}}})
	var created struct{ ID string `json:"id"` }
	mustTenant(t, json.Unmarshal(w.Body.Bytes(), &created))

	// KySignOn directory webhook with a platform admin role.
	secret := "webhook-secret"
	client := sso.NewKySignOnClient(config.SSOConfig{KySignOnHMACSecret: secret}, st)
	sync := func(event string) {
		t.Helper()
		body, _ := json.Marshal(sso.KySignOnSyncPayload{Event: event, ID: "ext-1", Username: "sso_admin", Role: "admin", Status: "active", Timestamp: time.Now().Unix()})
		mustTenant(t, client.HandleSyncWebhook(ctx, body, crypto.ComputeHMACSHA256(body, secret)))
	}
	sync("user.created")
	ssoUser, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-1")
	mustTenant(t, err)

	for _, id := range []string{created.ID, ssoUser.ID} {
		u, err := st.Users().GetUserByID(ctx, id)
		mustTenant(t, err)
		if u.Role != "admin" || u.Status != "active" {
			t.Fatalf("provisioning did not apply the external role: %+v", u)
		}
		orgs, err := ts.ListMemberOrganizations(ctx, id)
		mustTenant(t, err)
		if len(orgs) != 0 {
			t.Fatalf("external identity received organizations: %+v", orgs)
		}
		if _, err := ts.ReadOrganization(ctx, store.TenantAccess{ActorID: id, OrganizationID: store.InitialOrganizationID}); !errors.Is(err, store.ErrForbidden) {
			t.Fatalf("external platform admin read the default organization: %v", err)
		}
	}
	// A repeated bootstrap never picks up the external administrators.
	mustTenant(t, ts.Initialize(ctx))
	if _, err := ts.GetMembership(ctx, store.InitialOrganizationID, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bootstrap granted an external admin: %v", err)
	}

	// An organization administrator grants membership explicitly; the IdP's deactivation then
	// denies access live without touching the grant, and reactivation needs no re-grant.
	mustTenant(t, ts.PutMembership(ctx, store.TenantAccess{ActorID: "admin", OrganizationID: store.InitialOrganizationID}, created.ID, store.RoleOperator, "active"))
	scimAccess := store.TenantAccess{ActorID: created.ID, OrganizationID: store.InitialOrganizationID}
	_, err = ts.ReadOrganization(ctx, scimAccess)
	mustTenant(t, err)
	scimCall("PATCH", "/scim/v2/Users/"+created.ID, map[string]any{"schemas": []string{scim.SchemaPatchOp}, "Operations": []map[string]any{{"op": "replace", "path": "active", "value": false}}})
	if _, err := ts.ReadOrganization(ctx, scimAccess); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("deactivated SCIM user kept tenant access: %v", err)
	}
	m, err := ts.GetMembership(ctx, store.InitialOrganizationID, created.ID)
	mustTenant(t, err)
	if m.Status != "active" || m.Role != store.RoleOperator {
		t.Fatalf("deactivation altered the membership grant: %+v", m)
	}
	scimCall("PATCH", "/scim/v2/Users/"+created.ID, map[string]any{"schemas": []string{scim.SchemaPatchOp}, "Operations": []map[string]any{{"op": "replace", "path": "active", "value": true}}})
	_, err = ts.ReadOrganization(ctx, scimAccess)
	mustTenant(t, err)

	// Deletion removes the account and cascades its grant; re-provisioning is a new identity.
	sync("user.deleted")
	if _, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted SSO user still present: %v", err)
	}
}
