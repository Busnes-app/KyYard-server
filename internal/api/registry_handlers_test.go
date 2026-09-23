package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestRegistryRoutes(t *testing.T) {
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
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_tenant", Role: store.RoleOrganizationAdmin, Status: "active"}))
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}))
	check := func(c *http.Cookie, method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := tenantRequest(s, c, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), status)
		}
		return w
	}
	const secret = "s3cr3t-registry-token"
	noSecret := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if bytes.Contains(w.Body.Bytes(), []byte(secret)) || bytes.Contains(w.Body.Bytes(), []byte("credential_enc")) {
			t.Fatalf("response leaks the credential: %s", w.Body.String())
		}
	}

	list, policy := "/api/organizations/a/registries", "/api/organizations/a/registry-policy"
	put := `{"host":"GHCR.io","name":"GitHub","username":"bot","credential":"` + secret + `","allow_private":false}`

	check(nil, "GET", list, "", 401)
	check(nil, "GET", policy, "", 401)
	if w := check(admin, "GET", list, "", 200); w.Body.String() != "[]\n" && w.Body.String() != "[]" {
		t.Fatalf("empty list: %q", w.Body.String())
	}
	if w := tenantRequest(s, admin, "PUT", list, put, false); w.Code != 403 {
		t.Fatalf("registry write bypassed CSRF: %d", w.Code)
	}
	if w := tenantRequest(s, admin, "PUT", policy, `{"anonymous_pull_enabled":true}`, false); w.Code != 403 {
		t.Fatalf("policy write bypassed CSRF: %d", w.Code)
	}
	check(envadmin, "PUT", list, put, 403)
	check(admin, "PUT", list, `{"host":"bad host/x","name":"x","username":"","allow_private":false}`, 400)
	check(admin, "PUT", list, `{"host":"ghcr.io","name":"x","organization_id":"b"}`, 400)

	w := check(admin, "PUT", list, put, 200)
	noSecret(w)
	var row store.Registry
	must(json.Unmarshal(w.Body.Bytes(), &row))
	if row.Host != "ghcr.io" || !row.HasCredential || row.OrganizationID != "a" || row.ID == "" {
		t.Fatalf("put row: %+v", row)
	}

	for _, c := range []*http.Cookie{admin, envadmin} {
		w := check(c, "GET", list, "", 200)
		noSecret(w)
		var rows []store.Registry
		must(json.Unmarshal(w.Body.Bytes(), &rows))
		if len(rows) != 1 || rows[0].ID != row.ID || !rows[0].HasCredential {
			t.Fatalf("list: %+v", rows)
		}
	}

	// Policy round trip; environment admins read it but cannot change it.
	var p store.RegistryPolicy
	must(json.Unmarshal(check(envadmin, "GET", policy, "", 200).Body.Bytes(), &p))
	if p.AnonymousPullEnabled {
		t.Fatal("anonymous pull on by default")
	}
	check(envadmin, "PUT", policy, `{"anonymous_pull_enabled":true}`, 403)
	check(admin, "PUT", policy, `{"anonymous_pull_enabled":true}`, 204)
	must(json.Unmarshal(check(admin, "GET", policy, "", 200).Body.Bytes(), &p))
	if !p.AnonymousPullEnabled {
		t.Fatal("policy did not round trip")
	}
	check(admin, "PUT", policy, `{"anonymous_pull_enabled":false}`, 204)
	must(json.Unmarshal(check(admin, "GET", policy, "", 200).Body.Bytes(), &p))
	if p.AnonymousPullEnabled {
		t.Fatal("policy did not turn off")
	}

	// A member of organization a has no access to organization b.
	other := "/api/organizations/b/registries"
	check(admin, "GET", other, "", 403)
	check(admin, "PUT", other, put, 403)
	check(admin, "DELETE", other+"/"+row.ID, "", 403)
	check(admin, "GET", "/api/organizations/b/registry-policy", "", 403)
	check(admin, "PUT", "/api/organizations/b/registry-policy", `{"anonymous_pull_enabled":true}`, 403)

	check(envadmin, "DELETE", list+"/"+row.ID, "", 403)
	check(admin, "DELETE", list+"/00000000-0000-4000-8000-000000000000", "", 404)
	check(admin, "DELETE", list+"/"+row.ID, "", 204)
	check(admin, "DELETE", list+"/"+row.ID, "", 404)
}
