package api_test

import (
	"context"
	stdcrypto "crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/sso"
)

// fakeIdP is a TLS OIDC provider whose token endpoint returns the next ID token.
type fakeIdP struct {
	key     *rsa.PrivateKey
	issuer  string
	idToken string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key}
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": f.issuer, "authorization_endpoint": f.issuer + "/authorize",
				"token_endpoint": f.issuer + "/token", "jwks_uri": f.issuer + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
				"kty": "RSA", "alg": "RS256", "kid": "k",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB",
			}}})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "token_type": "Bearer", "id_token": f.idToken})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.Close)
	f.issuer = idp.URL
	// sso.ProviderContext verifies TLS through the default transport; trust only this fixture.
	saved := http.DefaultTransport
	http.DefaultTransport = idp.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = saved })
	return f
}

func (f *fakeIdP) sign(t *testing.T, typ, aud string, claims map[string]any) string {
	t.Helper()
	claims["iss"], claims["aud"] = f.issuer, aud
	if claims["iat"] == nil {
		claims["iat"] = time.Now().Unix()
	}
	claims["exp"] = time.Now().Add(2 * time.Minute).Unix()
	header := map[string]string{"alg": "RS256", "kid": "k"}
	if typ != "" {
		header["typ"] = typ
	}
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, stdcrypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *fakeIdP) logoutToken(t *testing.T, aud, sub, sid string) string {
	claims := map[string]any{"sub": sub, "jti": sid + sub, "events": map[string]any{"http://schemas.openid.net/event/backchannel-logout": map[string]any{}}}
	if sid != "" {
		claims["sid"] = sid
	}
	return f.sign(t, "logout+jwt", aud, claims)
}

// ssoLogin runs provider login and callback for sub/sid and returns the callback response.
func ssoLogin(t *testing.T, srv *api.Server, f *fakeIdP, provider, aud, sub, sid string) *httptest.ResponseRecorder {
	t.Helper()
	start := do(t, srv, "GET", "/api/sso/"+provider+"/login", nil)
	loc, err := url.Parse(start.Header().Get("Location"))
	if start.Code != http.StatusFound || err != nil {
		t.Fatalf("login redirect: %d %s", start.Code, start.Body.String())
	}
	q := loc.Query()
	f.idToken = f.sign(t, "", aud, map[string]any{"sub": sub, "sid": sid, "nonce": q.Get("nonce"), "preferred_username": sub})
	req := httptest.NewRequest(http.MethodGet, "/api/sso/"+provider+"/callback?code=c&state="+q.Get("state"), nil)
	for _, c := range start.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func sessionCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Value != "" {
			return c
		}
	}
	return nil
}

func signedIn(t *testing.T, srv *api.Server, c *http.Cookie) bool {
	if c == nil {
		return false
	}
	w := do(t, srv, "GET", "/api/auth/me", c)
	return w.Code == http.StatusOK && strings.Contains(w.Body.String(), `"authenticated":true`)
}

func postLogout(srv *api.Server, provider, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/sso/"+provider+"/backchannel-logout", strings.NewReader(url.Values{"logout_token": {token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func completeRecovery(t *testing.T, srv *api.Server, callback *httptest.ResponseRecorder, code string) *httptest.ResponseRecorder {
	t.Helper()
	loc, _ := url.Parse(callback.Header().Get("Location"))
	token := strings.TrimPrefix(loc.Fragment, "sso-mfa=")
	if callback.Code != http.StatusFound || len(token) != 64 {
		t.Fatalf("no MFA challenge: %d %s", callback.Code, callback.Header().Get("Location"))
	}
	body, _ := json.Marshal(map[string]string{"mfa_token": token, "code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/mfa/recovery-code", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestBackchannelLogoutEndsTheSSOLogin(t *testing.T) {
	ctx := context.Background()
	f := newFakeIdP(t)
	srv, st, cfg := setupTestServer(t)
	// Two providers at the same issuer: a sid is scoped to the provider that issued the session.
	providers := []sso.Provider{
		{ID: "idp_a", Name: "A", Kind: "oidc", Issuer: f.issuer, ClientID: "yard-a", Enabled: true, AutoProvision: true},
		{ID: "idp_b", Name: "B", Kind: "oidc", Issuer: f.issuer, ClientID: "yard-b", Enabled: true, AutoProvision: true},
	}
	plain, _ := json.Marshal(providers)
	sealed, _ := crypto.EncryptAESGCM(plain, crypto.DeriveKey(cfg.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err := st.Settings().SetSetting(ctx, "sso_providers_enc", sealed); err != nil {
		t.Fatal(err)
	}

	ended := sessionCookie(ssoLogin(t, srv, f, "idp_a", "yard-a", "alice", "sid-1"))
	kept := sessionCookie(ssoLogin(t, srv, f, "idp_a", "yard-a", "alice", "sid-2"))
	otherProvider := sessionCookie(ssoLogin(t, srv, f, "idp_b", "yard-b", "alice-b", "sid-1"))
	if !signedIn(t, srv, ended) || !signedIn(t, srv, kept) || !signedIn(t, srv, otherProvider) {
		t.Fatal("SSO login did not issue working sessions")
	}

	// An ID token is not a logout token, even when signed by the provider.
	if w := postLogout(srv, "idp_a", f.idToken); w.Code != http.StatusBadRequest {
		t.Fatalf("ID token as logout token: got %d", w.Code)
	}
	// A token for one client is not accepted at another provider's route, nor at an unknown one.
	for _, provider := range []string{"idp_b", "idp_missing"} {
		if w := postLogout(srv, provider, f.logoutToken(t, "yard-a", "alice", "sid-1")); w.Code != http.StatusBadRequest {
			t.Fatalf("logout for yard-a at %s: got %d", provider, w.Code)
		}
	}
	w := postLogout(srv, "idp_a", f.logoutToken(t, "yard-a", "alice", "sid-1"))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("logout: got %d %q %s", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
	}
	if signedIn(t, srv, ended) {
		t.Fatal("session survived its back-channel logout")
	}
	if !signedIn(t, srv, kept) {
		t.Fatal("logout of one login ended another login of the same user")
	}
	if !signedIn(t, srv, otherProvider) {
		t.Fatal("one provider's sid ended another provider's session")
	}
	// A replay is harmless and still acknowledged.
	if w := postLogout(srv, "idp_a", f.logoutToken(t, "yard-a", "alice", "sid-1")); w.Code != http.StatusOK {
		t.Fatalf("replayed logout: got %d", w.Code)
	}

	// A logout that lands before the callback finishes fences that login.
	if w := postLogout(srv, "idp_a", f.logoutToken(t, "yard-a", "alice", "sid-late")); w.Code != http.StatusOK {
		t.Fatalf("early logout: got %d", w.Code)
	}
	if w := ssoLogin(t, srv, f, "idp_a", "yard-a", "alice", "sid-late"); w.Code != http.StatusUnauthorized || sessionCookie(w) != nil {
		t.Fatalf("callback revived an ended login: %d", w.Code)
	}

	// A subject-wide token ends the user's logins from that provider issued by then, and nobody else's.
	bob := sessionCookie(ssoLogin(t, srv, f, "idp_a", "yard-a", "bob", "sid-b"))
	stale := f.sign(t, "logout+jwt", "yard-a", map[string]any{"sub": "bob", "iat": time.Now().Add(-time.Minute).Unix(), "events": map[string]any{"http://schemas.openid.net/event/backchannel-logout": map[string]any{}}})
	if w := postLogout(srv, "idp_a", stale); w.Code != http.StatusOK || !signedIn(t, srv, bob) {
		t.Fatalf("subject-wide logout issued before the login ended it: %d", w.Code)
	}
	if w := postLogout(srv, "idp_a", f.logoutToken(t, "yard-a", "alice", "")); w.Code != http.StatusOK {
		t.Fatalf("subject-wide logout: got %d", w.Code)
	}
	if signedIn(t, srv, kept) || !signedIn(t, srv, bob) || !signedIn(t, srv, otherProvider) {
		t.Fatal("subject-wide logout ended the wrong sessions")
	}
	if w := postLogout(srv, "idp_a", f.logoutToken(t, "yard-a", "nobody", "")); w.Code != http.StatusOK {
		t.Fatalf("logout for an unknown subject: got %d", w.Code)
	}

	// A linked local account with MFA gets its session after the second factor; the provider's
	// logout still reaches that session and fences a pending one, and never its password session.
	local := loginAs(t, srv, st, "carol", "user")
	carol, err := st.Users().GetUserByUsername(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Users().LinkSSO(ctx, carol.ID, crypto.SHA256Hex([]byte(local.Value)), carol.PasswordHash, "idp_a", "carol-sub", ""); err != nil {
		t.Fatal(err)
	}
	codes, hashed, err := auth.GenerateRecoveryCodes(2)
	if err != nil {
		t.Fatal(err)
	}
	carol.TOTPEnabled, carol.RecoveryCodesHash = true, hashed
	if err := st.Users().UpdateUser(ctx, carol); err != nil {
		t.Fatal(err)
	}
	mfa := sessionCookie(completeRecovery(t, srv, ssoLogin(t, srv, f, "idp_a", "yard-a", "carol-sub", "sid-c1"), codes[0]))
	if !signedIn(t, srv, mfa) {
		t.Fatal("SSO login with MFA did not issue a session")
	}
	pending := ssoLogin(t, srv, f, "idp_a", "yard-a", "carol-sub", "sid-c2")
	for _, sid := range []string{"sid-c1", "sid-c2"} {
		if w := postLogout(srv, "idp_a", f.logoutToken(t, "yard-a", "carol-sub", sid)); w.Code != http.StatusOK {
			t.Fatalf("logout %s: got %d", sid, w.Code)
		}
	}
	if signedIn(t, srv, mfa) {
		t.Fatal("MFA-completed SSO session survived its logout")
	}
	if w := completeRecovery(t, srv, pending, codes[1]); w.Code != http.StatusUnauthorized || sessionCookie(w) != nil || !strings.Contains(w.Body.String(), "ended at the provider") {
		t.Fatalf("MFA completion revived an ended login: %d %s", w.Code, w.Body.String())
	}
	if w := postLogout(srv, "idp_a", f.logoutToken(t, "yard-a", "carol-sub", "")); w.Code != http.StatusOK {
		t.Fatalf("subject-wide logout for a linked account: got %d", w.Code)
	}
	if !signedIn(t, srv, local) {
		t.Fatal("provider logout ended the linked account's local password session")
	}

	if len(auditRows(t, st, "auth.sso.logout")) == 0 {
		t.Fatal("back-channel logout was not audited")
	}
}
