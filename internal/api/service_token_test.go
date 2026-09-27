package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// tenantAdminFixture logs in a fresh organization administrator and returns the server, its
// store, the session cookie, that session's CSRF token (the ky_csrf cookie value from the
// login response) and the organization id.
func tenantAdminFixture(t *testing.T) (*api.Server, store.Store, *http.Cookie, string, string) {
	t.Helper()
	srv, st, _ := setupTestServer(t)

	const testPassword = "SuperSecretPass123!"
	hash, err := password.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if err := st.Users().CreateUser(context.Background(), &store.User{
		ID: "usr_orgadmin", Username: "orgadmin", PasswordHash: hash, Role: "user", Status: "active", SSOProvider: "local",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	orgID := "org_fixture"
	if err := st.Tenancy().CreateOrganizationWithAdmin(context.Background(), &store.Organization{ID: orgID, Name: "Fixture"}, "usr_orgadmin"); err != nil {
		t.Fatalf("create organization: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": "orgadmin", "password": testPassword})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var session, csrf *http.Cookie
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case auth.SessionCookieName:
			session = c
		case auth.CSRFCookieName:
			csrf = c
		}
	}
	if session == nil || csrf == nil {
		t.Fatalf("login response missing session or CSRF cookie")
	}
	return srv, st, session, csrf.Value, orgID
}

// pairAndClaim mints a pairing as the organization administrator and claims it, returning
// the bearer.
func pairAndClaim(t *testing.T, srv *api.Server, cookie *http.Cookie, csrf string, org string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/organizations/"+org+"/service-tokens/pairings", nil)
	req.AddCookie(cookie)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("pairing: %d %s", w.Code, w.Body)
	}
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("POST", "/api/service-tokens/claim", strings.NewReader(`{"pairing_code":"`+p.Code+`","service_name":"kypulse"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	var issue struct {
		Token        string `json:"token"`
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &issue)
	if issue.Organization.ID != org || len(issue.Token) != 64 {
		t.Fatalf("issue: %+v", issue)
	}
	return issue.Token
}

func bearer(srv *api.Server, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestServiceTokenReadsAndIsRefusedWrites(t *testing.T) {
	srv, st, cookie, csrf, org := tenantAdminFixture(t)
	token := pairAndClaim(t, srv, cookie, csrf, org)
	for _, path := range []string{"/api/organizations/" + org, "/api/organizations/" + org + "/environments", "/api/organizations/" + org + "/endpoints", "/api/organizations/" + org + "/audit"} {
		if w := bearer(srv, "GET", path, token); w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
		}
	}
	if w := bearer(srv, "GET", "/api/organizations/"+org+"/members", token); w.Code != http.StatusForbidden {
		t.Fatalf("members must be refused: %d", w.Code)
	}
	if w := bearer(srv, "GET", "/api/organizations/org_other", token); w.Code != http.StatusForbidden {
		t.Fatalf("foreign organization: %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/api/organizations/"+org+"/environments", strings.NewReader(`{"name":"prod"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("service write: %d %s", w.Code, w.Body)
	}
	rows, _, _ := st.Audit().ListAuditRecords(context.Background(), 0, 50)
	var denied bool
	for _, r := range rows {
		denied = denied || (r.Action == "environment.create" && r.Result == "denied" && strings.HasPrefix(r.UserID, "service:"))
	}
	if !denied {
		t.Fatal("no denial row for the service write")
	}
	if w := bearer(srv, "DELETE", "/api/organizations/"+org+"/service-tokens/svc_x", token); w.Code != http.StatusForbidden {
		t.Fatalf("a token must not manage tokens: %d", w.Code)
	}
	if w := bearer(srv, "GET", "/api/organizations/"+org, "deadbeef"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad bearer: %d", w.Code)
	}
}

func TestServiceTokenCannotFollowLogs(t *testing.T) {
	srv, st, cookie, csrf, org := tenantAdminFixture(t)
	token := pairAndClaim(t, srv, cookie, csrf, org)
	w := bearer(srv, "GET", "/api/organizations/"+org+"/endpoints/ep_none/containers/c/logs?follow=1", token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("follow must be refused before any endpoint lookup: %d %s", w.Code, w.Body)
	}
	rows, _, _ := st.Audit().ListAuditRecords(context.Background(), 0, 50)
	var denied bool
	for _, r := range rows {
		denied = denied || (r.Action == "container.logs" && r.Result == "denied" && r.Details == "follow")
	}
	if !denied {
		t.Fatal("follow refusal must be audited")
	}
}

func TestClaimIsRateLimited(t *testing.T) {
	srv, _, _, _, _ := tenantAdminFixture(t)
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest("POST", "/api/service-tokens/claim", strings.NewReader(`{"pairing_code":"000000","service_name":"kypulse"}`)))
		if w.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("POST", "/api/service-tokens/claim", strings.NewReader(`{"pairing_code":"000000","service_name":"kypulse"}`)))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt from one address: %d", w.Code)
	}
}

func TestRevokedTokenIsUnauthorized(t *testing.T) {
	srv, _, cookie, csrf, org := tenantAdminFixture(t)
	token := pairAndClaim(t, srv, cookie, csrf, org)
	req := httptest.NewRequest("GET", "/api/organizations/"+org+"/service-tokens", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	var list []struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != http.StatusOK || len(list) != 1 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	req = httptest.NewRequest("DELETE", "/api/organizations/"+org+"/service-tokens/"+list[0].ID, nil)
	req.AddCookie(cookie)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := bearer(srv, "GET", "/api/organizations/"+org, token); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked bearer: %d", w.Code)
	}
}

func TestHealthzOnTheContract(t *testing.T) {
	srv, _, _, _, _ := tenantAdminFixture(t)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	var body struct {
		Schema, Service, Status string
		Checks                  []struct{ Name, Status string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("healthz: %d %s", w.Code, w.Body)
	}
	if body.Schema != "ky.health/1" || body.Service != "kyyard" || body.Status != "ok" || len(body.Checks) != 1 || body.Checks[0].Name != "database" {
		t.Fatalf("body: %+v", body)
	}
}
