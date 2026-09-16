package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busness-app/kyyard-server/internal/store"
)

func TestMembershipRoutesEnforceScope(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx := context.Background()
	ts := st.Tenancy()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"a", "b"} {
		must(ts.CreateOrganization(ctx, &store.Organization{ID: org, Name: "Org " + org}))
	}
	admin := loginAs(t, s, st, "tenant", "user")
	envadmin := loginAs(t, s, st, "envadmin", "user")
	platform := loginAs(t, s, st, "platform", "admin")
	newcomer := loginAs(t, s, st, "newcomer", "user")
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_tenant", Role: store.RoleOrganizationAdmin, Status: "active"}))
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}))
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "usr_newcomer", Role: store.RoleReadOnly, Status: "disabled"}))
	check := func(c *http.Cookie, method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := tenantRequest(s, c, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), status)
		}
		return w
	}
	decode := func(w *httptest.ResponseRecorder, v any) {
		t.Helper()
		if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
			t.Fatal(err)
		}
	}

	// Own organizations: active memberships only, never someone else's.
	check(nil, "GET", "/api/organizations", "", 401)
	var orgs []store.MemberOrganization
	decode(check(admin, "GET", "/api/organizations", "", 200), &orgs)
	if len(orgs) != 1 || orgs[0].ID != "a" || orgs[0].Role != store.RoleOrganizationAdmin || orgs[0].Name != "Org a" {
		t.Fatalf("own organizations: %+v", orgs)
	}
	decode(check(newcomer, "GET", "/api/organizations", "", 200), &orgs)
	if len(orgs) != 0 {
		t.Fatalf("disabled membership listed: %+v", orgs)
	}
	decode(check(platform, "GET", "/api/organizations", "", 200), &orgs)
	if len(orgs) != 0 {
		t.Fatalf("platform admin got implicit organizations: %+v", orgs)
	}

	members := "/api/organizations/a/members"
	check(nil, "GET", members, "", 401)
	check(platform, "GET", members, "", 403)
	check(envadmin, "GET", members, "", 403)
	check(newcomer, "GET", members, "", 403)
	check(admin, "GET", "/api/organizations/b/members", "", 403)
	check(envadmin, "PUT", members+"/usr_envadmin", `{"role":"organization_admin"}`, 403)
	check(admin, "PUT", "/api/organizations/b/members/usr_newcomer", `{"role":"organization_admin"}`, 403)

	check(admin, "PUT", members+"/usr_newcomer", `{"role":"operator","organization_id":"b"}`, 400)
	check(admin, "PUT", members+"/usr_newcomer", `{"role":"owner"}`, 400)
	check(admin, "PUT", members+"/usr_missing", `{"role":"operator"}`, 404)
	if w := tenantRequest(s, admin, "PUT", members+"/usr_newcomer", `{"role":"operator"}`, false); w.Code != 403 {
		t.Fatal("membership write bypassed CSRF")
	}
	check(admin, "PUT", members+"/usr_newcomer", `{"role":"operator"}`, 204)
	var list []store.OrganizationMember
	decode(check(admin, "GET", members, "", 200), &list)
	if len(list) != 3 || list[0].Username != "envadmin" || list[1].Username != "newcomer" || list[1].Role != store.RoleOperator || list[1].Status != "active" {
		t.Fatalf("member list: %+v", list)
	}
	decode(check(newcomer, "GET", "/api/organizations", "", 200), &orgs)
	if len(orgs) != 1 || orgs[0].ID != "a" {
		t.Fatalf("new membership not visible: %+v", orgs)
	}

	w := check(admin, "PUT", members+"/usr_tenant", `{"role":"read_only"}`, 409)
	var conflict map[string]string
	decode(w, &conflict)
	if conflict["code"] != "last_administrator" {
		t.Fatalf("last admin code: %v", conflict)
	}
	check(admin, "DELETE", members+"/usr_tenant", "", 409)
	check(admin, "PUT", members+"/usr_newcomer", `{"role":"organization_admin","status":"active"}`, 204)
	check(admin, "DELETE", members+"/usr_tenant", "", 204)
	check(admin, "GET", members, "", 403)
	check(admin, "DELETE", members+"/usr_tenant", "", 403)
	check(newcomer, "DELETE", members+"/usr_ghost", "", 404)
}
