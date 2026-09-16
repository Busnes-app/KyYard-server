package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busness-app/kyyard-server/internal/api"
	"github.com/Busness-app/kyyard-server/internal/auth"
	"github.com/Busness-app/kyyard-server/internal/store"
)

func tenantRequest(s *api.Server, cookie *http.Cookie, method, path, body string, csrf bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Request-ID", "untrusted-client-id")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf {
		r.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "token"})
		r.Header.Set(auth.HeaderCSRF, "token")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestTenantRoutesEnforceScopeAndAudit(t *testing.T) {
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
		must(ts.CreateOrganization(ctx, &store.Organization{ID: org, Name: org}))
		must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-" + org, OrganizationID: org, Name: "Secret-" + org}))
	}
	admin := loginAs(t, s, st, "tenant", "user")
	platform := loginAs(t, s, st, "platform", "admin")
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_tenant", Role: store.RoleOrganizationAdmin, Status: "active"}))
	check := func(c *http.Cookie, method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := tenantRequest(s, c, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), status)
		}
		return w
	}
	for _, path := range []string{"/api/organizations/a", "/api/organizations/a/environments", "/api/organizations/a/environments/env-a", "/api/organizations/a/audit"} {
		check(nil, "GET", path, "", 401)
		check(platform, "GET", path, "", 403)
		check(admin, "GET", path, "", 200)
	}
	w := check(admin, "GET", "/api/organizations/a/audit", "", 200)
	var probes []store.AuditRecord
	must(json.Unmarshal(w.Body.Bytes(), &probes))
	for _, r := range probes {
		if r.UserID == "usr_platform" {
			t.Fatalf("non-member denial written into tenant audit: %+v", r)
		}
	}
	check(admin, "GET", "/api/organizations/b/environments", "", 403)
	check(admin, "POST", "/api/organizations/b/environments", `{"name":"Forbidden"}`, 403)
	for _, tc := range []struct{ method, body string }{{"GET", ""}, {"PATCH", `{"name":"Stolen"}`}, {"DELETE", ""}} {
		check(admin, tc.method, "/api/organizations/a/environments/env-b", tc.body, 404)
	}
	check(admin, "GET", "/api/organizations/b/audit", "", 403)
	check(admin, "GET", "/api/organizations/a/environments/env-b/audit", "", 404)
	check(admin, "POST", "/api/organizations/a/environments", `{"name":"Injected","organization_id":"b"}`, 400)
	check(admin, "POST", "/api/organizations/a/environments", `{"name":"Injected","actor_id":"usr_platform"}`, 400)
	check(admin, "POST", "/api/organizations/a/environments", `{"name":"one"}{"name":"two"}`, 400)
	check(admin, "GET", "/api/organizations/a/environments?limit=100000", "", 400)
	w = tenantRequest(s, admin, "POST", "/api/organizations/a/environments", `{"name":"No CSRF"}`, false)
	if w.Code != 403 {
		t.Fatal("tenant mutation bypassed CSRF")
	}
	w = check(admin, "POST", "/api/organizations/a/environments", `{"name":"Created"}`, 201)
	requestID := w.Header().Get("X-Request-ID")
	if len(requestID) != 32 || requestID == "untrusted-client-id" {
		t.Fatal("request correlation not server-generated")
	}
	var created store.Environment
	must(json.Unmarshal(w.Body.Bytes(), &created))
	if created.OrganizationID != "a" {
		t.Fatal("create wrong organization")
	}
	check(admin, "PATCH", "/api/organizations/a/environments/"+created.ID, `{"name":"Renamed"}`, 204)
	check(admin, "GET", "/api/organizations/a/environments/"+created.ID, "", 200)
	check(admin, "DELETE", "/api/organizations/a/environments/"+created.ID, "", 204)
	check(admin, "GET", "/api/organizations/a/environments/"+created.ID, "", 404)
	w = check(admin, "GET", "/api/organizations/a/audit", "", 200)
	var records []store.AuditRecord
	must(json.Unmarshal(w.Body.Bytes(), &records))
	found := false
	for _, r := range records {
		if r.OrganizationID != "a" || r.Scope != "organization" {
			t.Fatal("audit leak")
		}
		if r.CorrelationID == requestID {
			found = true
			if r.UserID != "usr_tenant" || r.EnvironmentID != created.ID || r.Result != "success" {
				t.Fatal("incorrect audit attribution")
			}
		}
	}
	if !found {
		t.Fatal("missing creation audit")
	}
	// A tenant administrator is still not a platform administrator.
	check(admin, "GET", "/api/backup/status", "", 403)
	check(platform, "GET", "/api/backup/status", "", 200)
	// Reuse the already-issued cookie after membership changes: there is no cached grant.
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_tenant", Role: store.RoleReadOnly, Status: "active"}))
	check(admin, "GET", "/api/organizations/a/environments/env-a", "", 200)
	check(admin, "PATCH", "/api/organizations/a/environments/env-a", `{"name":"No"}`, 403)
	check(admin, "GET", "/api/organizations/a/audit", "", 403)
	must(ts.DeleteMembership(ctx, "a", "usr_tenant"))
	check(admin, "GET", "/api/organizations/a/environments/env-a", "", 403)
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_tenant", Role: store.RoleOrganizationAdmin, Status: "disabled"}))
	check(admin, "GET", "/api/organizations/a", "", 403)
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_tenant", Role: store.RoleOrganizationAdmin, Status: "active"}))
	user, err := st.Users().GetUserByID(ctx, "usr_tenant")
	must(err)
	user.MustChangePassword = true
	must(st.Users().UpdateUser(ctx, user))
	w = check(admin, "GET", "/api/organizations/a", "", 403)
	if !strings.Contains(w.Body.String(), "password_change_required") {
		t.Fatal("restricted session passed tenant auth")
	}
}
