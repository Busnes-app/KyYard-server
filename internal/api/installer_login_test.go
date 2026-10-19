package api_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/applysetup"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/config"
)

// A KyIdentity-shaped ID token carries app roles in `roles`; the managed callback admits by it.
func TestManagedLoginReadsKyIdentityRolesClaim(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	enc := func(v any) string { raw, _ := json.Marshal(v); return b64(raw) }
	var issuer, nonce, sub string
	var extra map[string]any
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k", "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes())}}})
		case "/token":
			body := map[string]any{"iss": issuer, "aud": "yard", "sub": sub, "nonce": nonce, "role": "admin", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
			for k, v := range extra {
				body[k] = v
			}
			signed := enc(map[string]string{"alg": "RS256", "kid": "k", "typ": "JWT"}) + "." + enc(body)
			sum := sha256.Sum256([]byte(signed))
			sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "a", "token_type": "Bearer", "id_token": signed + "." + b64(sig)})
		default:
			w.WriteHeader(404)
		}
	}))
	defer idp.Close()
	issuer = idp.URL
	// sso.ProviderContext verifies TLS through the default transport; trust only this fixture.
	saved := http.DefaultTransport
	http.DefaultTransport = idp.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = saved })

	ctx := context.Background()
	srv, _, cfg := setupTestServerWith(t, func(c *config.Config) { c.Server.AppURL = "https://yard.example" })
	secret := filepath.Join(t.TempDir(), "client")
	if err := os.WriteFile(secret, []byte("private-fixture-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	bundle, _ := json.Marshal(applysetup.Common{Schema: applysetup.Schema, Service: "kyyard", SetupID: "271333e2-c221-4601-b249-563b3d828530", SSO: &applysetup.SSO{IssuerURL: issuer, ClientID: "yard", ClientSecretFile: secret, DirectorySecretFile: secret, RedirectURI: cfg.Server.AppURL + "/api/sso/kyidentity/callback"}, Admins: []applysetup.Identity{{Issuer: issuer, Subject: "owner", Username: "owner-admin"}}, Everyday: []applysetup.Identity{{Issuer: issuer, Subject: "daily", Username: "daily-user"}}})
	socket := filepath.Join(cfg.Database.DataDir, ".installer", "setup.sock")
	closeSetup, err := applysetup.StartLocal(ctx, socket, srv.ApplyInstaller, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSetup()
	if _, err := applysetup.CallLocal(ctx, socket, bundle, false); err != nil {
		t.Fatal(err)
	}

	login := func(who string, claims map[string]any, want int) *http.Cookie {
		t.Helper()
		sub, extra = who, claims
		start := do(t, srv, "GET", "/api/sso/kyidentity/login", nil)
		dest, err := url.Parse(start.Header().Get("Location"))
		if start.Code != 302 || err != nil {
			t.Fatal("login", start.Code, start.Body)
		}
		nonce = dest.Query().Get("nonce")
		r := httptest.NewRequest("GET", "/api/sso/kyidentity/callback?code=c&state="+dest.Query().Get("state"), nil)
		for _, c := range start.Result().Cookies() {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatal(who, claims, w.Code, w.Body)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == auth.SessionCookieName && c.Value != "" {
				if want != 302 {
					t.Fatal("refused callback issued a session", who)
				}
				return c
			}
		}
		if want == 302 {
			t.Fatal("no session", who)
		}
		return nil
	}
	admin := []string{"kyyard.admin"}
	owner := login("owner", map[string]any{"roles": admin}, 302)
	if w := do(t, srv, "GET", "/api/backup/status", owner); w.Code != 200 {
		t.Fatal("owner lacks admin", w.Code, w.Body)
	}
	daily := login("daily", map[string]any{"roles": []string{}}, 302)
	if w := do(t, srv, "GET", "/api/backup/status", daily); w.Code != 403 {
		t.Fatal("everyday account gained admin", w.Code)
	}
	// A malformed claim fails the token closed without revoking the assignment.
	login("owner", map[string]any{"roles": "kyyard.admin"}, 401)
	login("owner", map[string]any{"roles": admin}, 302)
	// An everyday assignment cannot claim admin, and app_roles is not read: the owner is revoked.
	login("daily", map[string]any{"roles": admin}, 403)
	login("owner", map[string]any{"app_roles": admin}, 403)
	if w := do(t, srv, "GET", "/api/backup/status", owner); w.Code != 401 {
		t.Fatal("session survived role loss", w.Code)
	}
}
