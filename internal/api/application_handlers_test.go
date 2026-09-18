package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestApplicationImportRoutes(t *testing.T) {
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
		must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-" + org, OrganizationID: org, Name: "prod"}))
	}
	admin := loginAs(t, s, st, "importer", "user")
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_importer", Role: store.RoleOrganizationAdmin, Status: "active"}))
	base := "/api/organizations/a/environments/env-a/applications"
	body, _ := json.Marshal(map[string]string{"name": "shop", "compose": "services: {web: {image: nginx:1, environment: {TOKEN: secret-canary}}}"})
	request := func(cookie *http.Cookie, method, path, body string, status int) string {
		t.Helper()
		w := tenantRequest(s, cookie, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret-canary") {
			t.Fatal("response leaked secret")
		}
		return w.Body.String()
	}
	request(nil, "POST", base, string(body), 401)
	if w := tenantRequest(s, admin, "POST", base, string(body), false); w.Code != 403 {
		t.Fatal("missing csrf accepted")
	}
	saved := request(admin, "POST", base, string(body), 201)
	var app store.Application
	must(json.Unmarshal([]byte(saved), &app))
	request(admin, "GET", base, "", 200)
	request(admin, "GET", base+"/"+app.ID+"/revisions/1", "", 200)
	request(admin, "POST", base, string(body), 409)
	request(admin, "POST", base, `{"name":"bad","compose":"services: {web: {build: secret-canary}}"}`, 400)
	request(admin, "POST", base, `{"name":"bad","compose":"services: {}","actor_id":"forged"}`, 400)
	request(admin, "GET", "/api/organizations/a/environments/env-b/applications", "", 404)
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "usr_importer", Role: store.RoleOrganizationAdmin, Status: "active"}))
	request(admin, "GET", "/api/organizations/b/environments/env-b/applications/"+app.ID+"/revisions/1", "", 404)
	for _, role := range []store.TenantRole{store.RoleReadOnly, store.RoleDeveloper, store.RoleOperator} {
		must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_importer", Role: role, Status: "active"}))
		request(admin, "POST", base, string(body), 403)
		request(admin, "DELETE", base+"/"+app.ID, `{"expected_revision":1}`, 403)
		request(admin, "GET", base+"/"+app.ID+"/revisions/1", "", 200)
	}
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_importer", Role: store.RoleOrganizationAdmin, Status: "active"}))
	request(admin, "DELETE", base+"/"+app.ID, `{"expected_revision":2}`, 409)
	request(admin, "DELETE", base+"/"+app.ID, `{"expected_revision":1}`, 204)
	request(admin, "GET", base+"/"+app.ID+"/revisions/1", "", 404)
	audits, err := ts.ReadAudit(ctx, store.TenantAccess{ActorID: "usr_importer", OrganizationID: "a"}, 0, 200)
	must(err)
	if strings.Contains(fmt.Sprint(audits), "secret-canary") {
		t.Fatal("audit leaked source")
	}
}
