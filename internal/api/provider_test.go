package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/backup"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/sso"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestProviderConfigurationSecretsAndPermissions(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	admin := loginAs(t, srv, st, "provideradmin", "admin")
	user := loginAs(t, srv, st, "provideruser", "user")
	payload := `{"name":"KyIdentity","kind":"oidc","issuer":"https://identity.example.test","client_id":"yard","client_secret":"do-not-leak","enabled":true,"auto_provision":true}`
	save := func(cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/settings/sso", strings.NewReader(payload))
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf {
			r.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "x"})
			r.Header.Set(auth.HeaderCSRF, "x")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	for _, cookie := range []*http.Cookie{nil, user} {
		if w := save(cookie, true); w.Code == 201 {
			t.Fatal("unprivileged provider creation")
		}
	}
	if w := save(admin, false); w.Code != 403 {
		t.Fatal("missing CSRF accepted", w.Code)
	}
	w := save(admin, true)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	for _, path := range []string{"/api/settings", "/api/settings/sso"} {
		for _, cookie := range []*http.Cookie{nil, user, admin} {
			w = do(t, srv, "GET", path, cookie)
			if strings.Contains(w.Body.String(), "do-not-leak") || strings.Contains(w.Body.String(), "sso_providers_enc") {
				t.Fatal("secret leak", path)
			}
		}
	}
	blob, err := st.Settings().GetSetting(context.Background(), "sso_providers_enc")
	if err != nil || strings.Contains(blob, "do-not-leak") {
		t.Fatal("provider secret not encrypted")
	}
	public := do(t, srv, "GET", "/api/settings", nil)
	if !strings.Contains(public.Body.String(), "KyIdentity") || strings.Contains(public.Body.String(), "identity.example.test") {
		t.Fatal("bad public provider projection", public.Body)
	}
	var configured []sso.Provider
	w = do(t, srv, "GET", "/api/settings/sso", admin)
	if err := json.Unmarshal(w.Body.Bytes(), &configured); err != nil || len(configured) != 1 {
		t.Fatal("missing configuration")
	}
	id := configured[0].ID
	r := httptest.NewRequest("PUT", "/api/settings/sso/"+id, strings.NewReader(`{"client_secret":"rotated-secret","enabled":false}`))
	r.AddCookie(admin)
	r.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "x"})
	r.Header.Set(auth.HeaderCSRF, "x")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w = do(t, srv, "GET", "/api/sso/"+id+"/login", nil); w.Code != 404 {
		t.Fatal("disabled provider accepted")
	}
	blob, err = st.Settings().GetSetting(context.Background(), "sso_providers_enc")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := crypto.DecryptAESGCM(blob, crypto.DeriveKey(cfg.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err != nil || !bytes.Contains(plain, []byte("rotated-secret")) || !bytes.Contains(plain, []byte(id)) {
		t.Fatal("rotation changed identity or lost secret", err)
	}
	if cfg.Database.Driver == "sqlite" {
		payload, err := backup.Collect(context.Background(), cfg, "test")
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		var key []byte
		for _, f := range payload.Files {
			if f.Path == "data/ky_server.db" {
				if err := os.WriteFile(filepath.Join(dir, "snapshot.db"), f.Data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if f.Path == "data/encryption.key" {
				key, err = hex.DecodeString(strings.TrimSpace(string(f.Data)))
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		db, err := sql.Open("sqlite", filepath.Join(dir, "snapshot.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var restored string
		if err := db.QueryRow("SELECT value FROM server_settings WHERE key = ?", "sso_providers_enc").Scan(&restored); err != nil {
			t.Fatal(err)
		}
		opened, err := crypto.DecryptAESGCM(restored, crypto.DeriveKey(key, "kyyard/sso/providers/v1"))
		if err != nil || !bytes.Equal(opened, plain) {
			t.Fatal("backup lost provider credentials", err)
		}
	}

}

func TestOAuthProviderLoginBindsBrowserProviderAndConsumesState(t *testing.T) {
	var challenge string
	var exchanges int
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "valid-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				w.WriteHeader(400)
				return
			}
			exchanges++
			_, _ = w.Write([]byte(`{"access_token":"profile-token","token_type":"Bearer"}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer profile-token" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"id":987654321012345678,"login":"oauth-person","name":"Person"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer idp.Close()
	srv, st, cfg := setupTestServer(t)
	// HTTP is only used by this local provider fixture, bypassing the production HTTPS validator.
	p := sso.Provider{ID: "idp_test", Name: "OAuth fixture", Kind: "oauth2", ClientID: "yard", ClientSecret: "secret", AuthorizationURL: idp.URL + "/authorize", TokenURL: idp.URL + "/token", UserInfoURL: idp.URL + "/user", SubjectField: "id", UsernameField: "login", NameField: "name", Enabled: true, AutoProvision: true}
	other := p
	other.ID = "idp_other"
	plain, _ := json.Marshal([]sso.Provider{p, other})
	sealed, _ := crypto.EncryptAESGCM(plain, crypto.DeriveKey(cfg.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err := st.Settings().SetSetting(context.Background(), "sso_providers_enc", sealed); err != nil {
		t.Fatal(err)
	}
	w := do(t, srv, "GET", "/api/sso/idp_test/login", nil)
	login := w
	if w.Code != 302 {
		t.Fatal(w.Code, w.Body)
	}
	dest, _ := url.Parse(w.Header().Get("Location"))
	challenge = dest.Query().Get("code_challenge")
	if challenge == "" || dest.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing PKCE")
	}
	path := "/api/sso/idp_test/callback?state=" + dest.Query().Get("state") + "&code=valid-code"
	if w = do(t, srv, "GET", path, nil); w.Code != 400 {
		t.Fatal("unbound callback accepted")
	}
	// Wrong browser doesn't consume the legitimate browser's attempt.
	dest, _ = url.Parse(login.Header().Get("Location"))
	challenge = dest.Query().Get("code_challenge")
	path = "/api/sso/idp_test/callback?state=" + dest.Query().Get("state") + "&code=valid-code"
	r := httptest.NewRequest("GET", path, nil)
	for _, c := range login.Result().Cookies() {
		r.AddCookie(c)
	}
	wrong := httptest.NewRequest("GET", strings.Replace(path, "idp_test", "idp_other", 1), nil)
	for _, c := range login.Result().Cookies() {
		wrong.AddCookie(c)
	}
	refused := httptest.NewRecorder()
	srv.ServeHTTP(refused, wrong)
	if refused.Code != 400 {
		t.Fatal("provider mix-up accepted", refused.Code)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != 302 {
		t.Fatal(w.Code, w.Body)
	}
	if exchanges != 1 {
		t.Fatal("wrong number of exchanges", exchanges)
	}
	u, err := st.Users().GetUserBySSO(context.Background(), p.ID, "987654321012345678")
	if err != nil || u.Role != "user" {
		t.Fatal("identity mapping", u, err)
	}
	// Replaying the same state/cookie cannot mint another session or exchange another code.
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != 400 || exchanges != 1 {
		t.Fatal("callback replay accepted")
	}
	rows, err := st.Tenancy().ListMemberOrganizations(context.Background(), u.ID)
	if err != nil || len(rows) != 0 {
		t.Fatal("SSO granted tenant access", err)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("secret")) {
		t.Fatal("callback leaked credential")
	}
}

func TestSSOAdmissionRemainsAvailableWhenAttemptMapIsFull(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	cfg.Security.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}
	p := sso.Provider{ID: "idp_capacity", Kind: "oauth2", Enabled: true, ClientID: "yard", AuthorizationURL: "https://identity.example.test/authorize"}
	plain, _ := json.Marshal([]sso.Provider{p})
	sealed, err := crypto.EncryptAESGCM(plain, crypto.DeriveKey(cfg.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Settings().SetSetting(context.Background(), "sso_providers_enc", sealed); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		r := httptest.NewRequest("GET", "/api/sso/idp_capacity/login", nil)
		r.RemoteAddr = "192.0.2.1:12345"
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("2001:db8::%x", i+1))
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != http.StatusFound {
			t.Fatalf("fresh client %d: got %d, want redirect: %s", i, w.Code, w.Body)
		}
	}
}

// A provider asserting "Erin" while a local "erin" exists is refused by name: sign-in matches
// LOWER(username), so creating "Erin" would put two accounts behind one login, and signing in as
// "erin" would hand the provider an account it never owned (Review Focus 4).
func TestSSOAutoProvisionRefusesACaseVariantUsername(t *testing.T) {
	var challenge string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "valid-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"profile-token","token_type":"Bearer"}`))
		case "/user":
			_, _ = w.Write([]byte(`{"id":"erin-at-idp","login":"Erin","name":"Erin"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer idp.Close()
	srv, st, cfg := setupTestServer(t)
	ctx := context.Background()
	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_erin", Username: "erin", Role: "admin", Status: "active", SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
	// HTTP is only used by this local provider fixture, bypassing the production HTTPS validator.
	p := sso.Provider{ID: "idp_test", Name: "OAuth fixture", Kind: "oauth2", ClientID: "yard", ClientSecret: "secret", AuthorizationURL: idp.URL + "/authorize", TokenURL: idp.URL + "/token", UserInfoURL: idp.URL + "/user", SubjectField: "id", UsernameField: "login", NameField: "name", Enabled: true, AutoProvision: true}
	plain, _ := json.Marshal([]sso.Provider{p})
	sealed, _ := crypto.EncryptAESGCM(plain, crypto.DeriveKey(cfg.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err := st.Settings().SetSetting(ctx, "sso_providers_enc", sealed); err != nil {
		t.Fatal(err)
	}
	login := do(t, srv, "GET", "/api/sso/idp_test/login", nil)
	if login.Code != 302 {
		t.Fatal(login.Code, login.Body)
	}
	dest, _ := url.Parse(login.Header().Get("Location"))
	challenge = dest.Query().Get("code_challenge")
	r := httptest.NewRequest("GET", "/api/sso/idp_test/callback?state="+dest.Query().Get("state")+"&code=valid-code", nil)
	for _, c := range login.Result().Cookies() {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "The username Erin is taken by another account") {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if !strings.HasPrefix(c.Name, "ky_sso_") && c.Value != "" {
			t.Fatalf("a session cookie was issued: %s", c.Name)
		}
	}
	if _, err := st.Users().GetUserBySSO(ctx, "idp_test", "erin-at-idp"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second account was created: %v", err)
	}
	if u, err := st.Users().GetUserByUsername(ctx, "ERIN"); err != nil || u.ID != "usr_erin" || u.SSOProvider != "local" {
		t.Fatalf("the local account changed: %+v %v", u, err)
	}
	rows := auditRows(t, st, "auth.sso.refused")
	if len(rows) != 1 || rows[0].Result != "denied" || rows[0].Resource != "idp_test" || rows[0].Details != "username=Erin" || rows[0].UserID != "" {
		t.Fatalf("refusal audit rows: %+v", rows)
	}
}
