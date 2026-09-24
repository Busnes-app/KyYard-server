package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

var adminRoutes = []struct{ method, path string }{
	{"GET", "/api/admin/organizations"},
	{"POST", "/api/admin/organizations"},
	{"GET", "/api/admin/users"},
	{"POST", "/api/admin/users"},
}

func userCount(t *testing.T, st store.Store) int {
	t.Helper()
	n, err := st.Users().CountUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

const (
	carolID = "usr_ca401000000000000000000c"
	daveID  = "usr_da7e0000000000000000000d"
)

// seedUser stores a local account with a production-shaped ID and a known password.
func seedUser(t *testing.T, st store.Store, id, username, role, status string) {
	t.Helper()
	hash, err := password.Hash("SuperSecretPass123!")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Users().CreateUser(context.Background(), &store.User{ID: id, Username: username, PasswordHash: hash, Role: role, Status: status, SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
}

// captureLog sends the standard logger to a buffer until the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func decodeCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	code, _ := out["code"].(string)
	return code
}

func TestAdminRoutesRequirePlatformAdmin(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	member := loginAs(t, srv, st, "bob", "user")
	before := userCount(t, st)
	seeded, err := st.Tenancy().ListOrganizations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"username": "mallory", "display_name": "Mallory", "role": "admin", "name": "Evil", "admin_user_id": "usr_bob"}
	for _, rt := range adminRoutes {
		if w := adminDo(t, srv, member, rt.method, rt.path, body); w.Code != http.StatusForbidden {
			t.Errorf("user %s %s: got %d", rt.method, rt.path, w.Code)
		}
	}
	if after := userCount(t, st); after != before {
		t.Fatalf("a user session changed the user table: %d -> %d", before, after)
	}
	orgs, err := st.Tenancy().ListOrganizations(context.Background())
	if err != nil || len(orgs) != len(seeded) {
		t.Fatalf("a user session created an organization: %v %v", orgs, err)
	}
}

func TestAdminWritesRequireCSRF(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	admin := loginAs(t, srv, st, "alice", "admin")
	before := userCount(t, st)
	for _, path := range []string{"/api/admin/users", "/api/admin/organizations"} {
		raw, _ := json.Marshal(map[string]any{"username": "eve", "display_name": "Eve", "role": "user", "name": "Org", "admin_user_id": "usr_alice"})
		req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(admin)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF: got %d", path, w.Code)
		}
	}
	if after := userCount(t, st); after != before {
		t.Fatalf("a request without CSRF created a user")
	}
}

func TestAdminCreateOrganization(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	ctx := context.Background()
	admin := loginAs(t, srv, st, "alice", "admin")
	seedUser(t, st, carolID, "carol", "user", "active")
	carol := loginWith(t, srv, "carol", "SuperSecretPass123!")
	seeded, err := st.Tenancy().ListOrganizations(ctx)
	if err != nil {
		t.Fatal(err)
	}

	w := adminDo(t, srv, admin, "POST", "/api/admin/organizations", map[string]any{"name": "Acme", "admin_user_id": carolID})
	if w.Code != http.StatusCreated || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %v %s", w.Code, w.Header(), w.Body)
	}
	var created struct{ ID, Name, CreatedAt string }
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if !regexp.MustCompile(`^org_[0-9a-f]{24}$`).MatchString(created.ID) || created.Name != "Acme" {
		t.Fatalf("created %s", w.Body)
	}

	mine := do(t, srv, "GET", "/api/organizations", carol)
	var memberships []map[string]any
	_ = json.Unmarshal(mine.Body.Bytes(), &memberships)
	if len(memberships) != 1 || memberships[0]["id"] != created.ID || memberships[0]["role"] != "organization_admin" {
		t.Fatalf("carol's organizations: %s", mine.Body)
	}

	list := adminDo(t, srv, admin, "GET", "/api/admin/organizations", nil)
	var orgs []map[string]any
	_ = json.Unmarshal(list.Body.Bytes(), &orgs)
	if list.Code != 200 || list.Header().Get("Cache-Control") != "no-store" || len(orgs) != len(seeded)+1 || orgs[0]["members"] != float64(1) || orgs[0]["name"] != "Acme" || orgs[0]["id"] != created.ID {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}

	rows := auditRows(t, st, "organization.create")
	if len(rows) != 1 || rows[0].Scope != "platform" || rows[0].UserID != "usr_alice" || rows[0].Resource != created.ID || rows[0].Details != "admin="+carolID || rows[0].Result != "success" {
		t.Fatalf("audit: %+v", rows)
	}

	seedUser(t, st, daveID, "dave", "user", "suspended")
	const nobodyID = "usr_000000000000000000000000"
	for _, tc := range []struct {
		name, adminID string
		status        int
		code          string
	}{
		{"Acme", carolID, http.StatusConflict, "organization_exists"},
		{"Other", nobodyID, http.StatusNotFound, "user_not_found"},
		{"Other", daveID, http.StatusConflict, "user_inactive"},
	} {
		w := adminDo(t, srv, admin, "POST", "/api/admin/organizations", map[string]any{"name": tc.name, "admin_user_id": tc.adminID})
		if w.Code != tc.status || decodeCode(t, w) != tc.code {
			t.Errorf("%s/%s: got %d %s", tc.name, tc.adminID, w.Code, w.Body)
		}
	}
	want := map[string]string{
		"Acme":  "admin=" + carolID + " refused=organization_exists",
		"Other": "admin=" + nobodyID + " refused=user_not_found|admin=" + daveID + " refused=user_inactive",
	}
	failures := 0
	for _, row := range auditRows(t, st, "organization.create") {
		if row.Result != "failure" {
			continue
		}
		failures++
		if row.Scope != "platform" || row.UserID != "usr_alice" || !strings.Contains("|"+want[row.Resource]+"|", "|"+row.Details+"|") {
			t.Errorf("failure row: %+v", row)
		}
	}
	if failures != 3 {
		t.Errorf("want 3 failure audit rows, got %d", failures)
	}

	for _, body := range []map[string]any{
		{"name": "", "admin_user_id": carolID},
		{"name": strings.Repeat("a", 65), "admin_user_id": carolID},
		{"name": "Bad\nName", "admin_user_id": carolID},
		{"name": "Bad\x00Name", "admin_user_id": carolID},
		{"name": "Other", "admin_user_id": ""},
		{"name": "Other", "admin_user_id": "usr_carol"},
		{"name": "Other", "admin_user_id": carolID + " refused=organization_exists"},
		{"name": "Other", "admin_user_id": carolID, "members": 5},
	} {
		if w := adminDo(t, srv, admin, "POST", "/api/admin/organizations", body); w.Code != http.StatusBadRequest {
			t.Errorf("%v: got %d", body, w.Code)
		}
	}
	if n := len(auditRows(t, st, "organization.create")); n != 4 {
		t.Errorf("a malformed request wrote an audit row: %d rows", n)
	}
	if orgs, _ := st.Tenancy().ListOrganizations(ctx); len(orgs) != len(seeded)+1 {
		t.Fatalf("refused creates left organizations behind: %+v", orgs)
	}
}

func TestAdminCreateUser(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	ctx := context.Background()
	admin := loginAs(t, srv, st, "alice", "admin")

	logs := captureLog(t)
	w := adminDo(t, srv, admin, "POST", "/api/admin/users", map[string]any{"username": "erin", "display_name": "Erin Example", "role": "user"})
	if w.Code != http.StatusCreated || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %v %s", w.Code, w.Header(), w.Body)
	}
	var created struct {
		ID                string `json:"id"`
		Username          string `json:"username"`
		DisplayName       string `json:"display_name"`
		Role              string `json:"role"`
		TemporaryPassword string `json:"temporary_password"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	temp := created.TemporaryPassword
	if !regexp.MustCompile(`^[A-Za-z0-9]{24}$`).MatchString(temp) {
		t.Fatalf("temporary password shape: %q", temp)
	}
	if created.Username != "erin" || created.DisplayName != "Erin Example" || created.Role != "user" || !regexp.MustCompile(`^usr_[0-9a-f]{24}$`).MatchString(created.ID) {
		t.Fatalf("created %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "password_hash") || strings.Contains(w.Body.String(), "$argon2") {
		t.Fatalf("response leaked a hash: %s", w.Body)
	}

	stored, err := st.Users().GetUserByUsername(ctx, "erin")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := password.Verify(temp, stored.PasswordHash); err != nil || !ok {
		t.Fatalf("stored hash does not verify the temporary password: %v %v", ok, err)
	}
	if !stored.MustChangePassword || stored.SSOProvider != "local" || stored.Status != "active" || stored.Role != "user" {
		t.Fatalf("stored user: %+v", stored)
	}
	if strings.Contains(logs.String(), temp) {
		t.Fatal("server log carried the temporary password")
	}

	list := adminDo(t, srv, admin, "GET", "/api/admin/users", nil)
	if list.Code != 200 || list.Header().Get("Cache-Control") != "no-store" || strings.Contains(list.Body.String(), temp) || strings.Contains(list.Body.String(), stored.PasswordHash) {
		t.Fatalf("user list: %d %s", list.Code, list.Body)
	}
	var users []map[string]any
	_ = json.Unmarshal(list.Body.Bytes(), &users)
	if len(users) != 2 || users[0]["username"] != "alice" || users[1]["username"] != "erin" || users[1]["sso_provider"] != "local" || users[1]["status"] != "active" {
		t.Fatalf("user list: %s", list.Body)
	}
	for _, u := range users {
		for _, secret := range []string{"password_hash", "totp_enabled", "totp_secret_enc", "recovery_codes_hash", "must_change_password"} {
			if _, found := u[secret]; found {
				t.Errorf("user list carries %q", secret)
			}
		}
	}

	erin := loginWith(t, srv, "erin", temp)
	me := do(t, srv, "GET", "/api/auth/me", erin)
	if !strings.Contains(me.Body.String(), `"must_change_password":true`) || strings.Contains(me.Body.String(), temp) {
		t.Fatalf("me: %s", me.Body)
	}

	rows := auditRows(t, st, "user.create")
	if len(rows) != 1 || rows[0].Scope != "platform" || rows[0].UserID != "usr_alice" || rows[0].Resource != created.ID || rows[0].Details != "role=user" || rows[0].Result != "success" {
		t.Fatalf("audit: %+v", rows)
	}
	all, _, _ := st.Audit().ListAuditRecords(ctx, 0, 100)
	for _, row := range all {
		if strings.Contains(row.Details+row.Resource, temp) {
			t.Fatalf("audit row carries the password: %+v", row)
		}
	}

	if w := adminDo(t, srv, admin, "POST", "/api/admin/users", map[string]any{"username": "erin", "display_name": "Erin Two", "role": "user"}); w.Code != http.StatusConflict || decodeCode(t, w) != "username_exists" {
		t.Errorf("duplicate: %d %s", w.Code, w.Body)
	}
	rows = auditRows(t, st, "user.create")
	if len(rows) != 2 {
		t.Fatalf("duplicate wrote no failure row: %+v", rows)
	}
	for _, row := range rows {
		if row.Result == "failure" && (row.Scope != "platform" || row.UserID != "usr_alice" || row.Resource != "erin" || row.Details != "role=user refused=username_exists") {
			t.Errorf("failure row: %+v", row)
		}
	}

	w = adminDo(t, srv, admin, "POST", "/api/admin/users", map[string]any{"username": "grace", "display_name": "Grace", "role": "admin"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create admin: %d %s", w.Code, w.Body)
	}
	if grace, err := st.Users().GetUserByUsername(ctx, "grace"); err != nil || grace.Role != "admin" {
		t.Fatalf("stored admin: %+v %v", grace, err)
	}
	found := false
	for _, row := range auditRows(t, st, "user.create") {
		found = found || (row.Result == "success" && row.Details == "role=admin")
	}
	if !found {
		t.Error("no role=admin audit row")
	}

	before := userCount(t, st)
	for _, body := range []map[string]any{
		{"username": "Alice", "display_name": "A", "role": "user"},
		{"username": "ab", "display_name": "A", "role": "user"},
		{"username": strings.Repeat("a", 65), "display_name": "A", "role": "user"},
		{"username": "frank", "display_name": "Frank", "role": "manager"},
		{"username": "frank", "display_name": "", "role": "user"},
		{"username": "frank", "display_name": "Bad\x1bName", "role": "user"},
		{"username": "frank", "display_name": strings.Repeat("a", 129), "role": "user"},
		{"username": "frank", "display_name": "Frank", "role": "user", "must_change_password": false},
	} {
		if w := adminDo(t, srv, admin, "POST", "/api/admin/users", body); w.Code != http.StatusBadRequest {
			t.Errorf("%v: got %d", body, w.Code)
		}
	}
	if after := userCount(t, st); after != before {
		t.Fatal("an invalid request created a user")
	}
}

func loginWith(t *testing.T, srv *api.Server, username, pass string) *http.Cookie {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"username": username, "password": pass})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	t.Fatalf("login %s: %d %s", username, w.Code, w.Body)
	return nil
}

// The users column is unique by exact case but login matches LOWER(username), so "erin"
// beside "Erin" would make one login name resolve to two accounts.
func TestAdminCreateUserRefusesCaseVariant(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	admin := loginAs(t, srv, st, "alice", "admin")
	seedUser(t, st, "usr_e0000000000000000000000e", "Erin", "user", "active")
	if u, err := st.Users().GetUserByUsername(context.Background(), "erin"); err != nil || u.Username != "Erin" {
		t.Fatalf("lookup is not case-insensitive: %+v %v", u, err)
	}
	before := userCount(t, st)
	w := adminDo(t, srv, admin, "POST", "/api/admin/users", map[string]any{"username": "erin", "display_name": "Erin", "role": "user"})
	if w.Code != http.StatusConflict || decodeCode(t, w) != "username_exists" {
		t.Fatalf("case variant: %d %s", w.Code, w.Body)
	}
	if after := userCount(t, st); after != before {
		t.Fatal("a case variant of an existing username was created")
	}
}
