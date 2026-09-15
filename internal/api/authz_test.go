package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busness-app/ky-primitives/password"
	"github.com/Busness-app/ky_server_base/internal/api"
	"github.com/Busness-app/ky_server_base/internal/auth"
	"github.com/Busness-app/ky_server_base/internal/store"
)

// loginAs creates a user with the given role and returns its session cookie.
func loginAs(t *testing.T, srv *api.Server, st store.Store, username, role string) *http.Cookie {
	t.Helper()

	const testPassword = "SuperSecretPass123!"
	hash, err := password.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	err = st.Users().CreateUser(context.Background(), &store.User{
		ID:           "usr_" + username,
		Username:     username,
		PasswordHash: hash,
		Role:         role,
		Status:       "active",
		SSOProvider:  "local",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": username, "password": testPassword})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login as %s: got %d: %s", username, w.Code, w.Body.String())
	}

	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	t.Fatalf("no session cookie for %s", username)
	return nil
}

func do(t *testing.T, srv *api.Server, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
		req.Header.Set(auth.HeaderCSRF, "test-csrf")
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// Settings drive the login screen, so part of the payload is public. Secrets
// (SCIM bearer token, recovery token) live in extra_settings and must not be.
func TestSettingsExposureByRole(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	if err := st.Settings().SetSetting(context.Background(), "scim_token", "super-secret-bearer"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	if err := st.Settings().SetSetting(context.Background(), "kyrecovery_token_enc", "sealed-ciphertext-blob"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}

	decode := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("settings: got %d: %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode settings: %v", err)
		}
		return out
	}

	anon := decode(do(t, srv, "GET", "/api/settings", nil))
	if anon["app_name"] == nil || anon["captcha_provider"] == nil {
		t.Errorf("anonymous settings must keep the login screen working, got %v", anon)
	}
	for _, secret := range []string{"extra_settings", "db_driver"} {
		if _, found := anon[secret]; found {
			t.Errorf("anonymous settings leaked %q: %v", secret, anon)
		}
	}
	if bytes.Contains(do(t, srv, "GET", "/api/settings", nil).Body.Bytes(), []byte("super-secret-bearer")) {
		t.Error("anonymous settings leaked the SCIM bearer token")
	}

	member := decode(do(t, srv, "GET", "/api/settings", loginAs(t, srv, st, "bob", "user")))
	if member["db_driver"] == nil {
		t.Errorf("authenticated settings should include db_driver, got %v", member)
	}
	if _, found := member["extra_settings"]; found {
		t.Errorf("non-admin settings leaked extra_settings: %v", member)
	}

	admin := decode(do(t, srv, "GET", "/api/settings", loginAs(t, srv, st, "alice", "admin")))
	extra, ok := admin["extra_settings"].(map[string]any)
	if !ok || extra["scim_token"] != "super-secret-bearer" {
		t.Errorf("admin should still see extra_settings, got %v", admin)
	}
	if _, found := extra["kyrecovery_token_enc"]; found {
		t.Errorf("admin settings leaked the sealed KyRecovery token: %v", admin)
	}
	if _, found := extra["kyrecovery_token"]; found {
		t.Errorf("admin settings leaked a legacy plaintext KyRecovery token: %v", admin)
	}
}

func TestPrivilegedEndpointsRequireAdmin(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{"POST", "/api/backup/drill"},
		{"POST", "/api/backup/export-capsule"},
		{"POST", "/api/backup/pair-remote"},
		{"POST", "/api/backup/deposit"},
		{"DELETE", "/api/backup/pairing"},
		{"POST", "/api/backup/pin-key"},
		{"PUT", "/api/backup/schedule"},
		{"GET", "/api/backup/status"},
		{"POST", "/api/settings/theme"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			srv, st, _ := setupTestServer(t)

			if got := do(t, srv, tc.method, tc.path, nil).Code; got != http.StatusUnauthorized {
				t.Errorf("anonymous: got %d, want 401", got)
			}

			member := loginAs(t, srv, st, "bob", "user")
			if got := do(t, srv, tc.method, tc.path, member).Code; got != http.StatusForbidden {
				t.Errorf("non-admin: got %d, want 403", got)
			}

			// Admins get through: any status other than 401/403 proves authorization passed.
			admin := loginAs(t, srv, st, "alice", "admin")
			if got := do(t, srv, tc.method, tc.path, admin).Code; got == http.StatusUnauthorized || got == http.StatusForbidden {
				t.Errorf("admin was blocked: got %d", got)
			}
		})
	}
}
