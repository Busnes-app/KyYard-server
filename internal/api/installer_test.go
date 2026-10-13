package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/applysetup"
	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/sso"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestInstallerLiveSetupAndSignedDirectory(t *testing.T) {
	ctx := context.Background()
	srv, st, cfg := setupTestServerWith(t, func(c *config.Config) { c.Server.AppURL = "https://yard.example" })
	dir := t.TempDir()
	secret, directory := filepath.Join(dir, "client"), filepath.Join(dir, "directory")
	for _, p := range []string{secret, directory} {
		if err := os.WriteFile(p, []byte("private-fixture-secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b := applysetup.Common{Schema: applysetup.Schema, Service: "kyyard", SetupID: "271333e2-c221-4601-b249-563b3d828530", SSO: &applysetup.SSO{IssuerURL: "https://identity.example", ClientID: "yard", ClientSecretFile: secret, DirectorySecretFile: directory, RedirectURI: cfg.Server.AppURL + "/api/sso/kyidentity/callback"}, Admins: []applysetup.Identity{{Issuer: "https://identity.example", Subject: "owner", Username: "owner-admin"}}, Everyday: []applysetup.Identity{{Issuer: "https://identity.example", Subject: "daily", Username: "daily-user"}}}
	raw, _ := json.Marshal(b)
	socket := filepath.Join(cfg.Database.DataDir, ".installer", "setup.sock")
	closeSetup, err := applysetup.StartLocal(ctx, socket, srv.ApplyInstaller, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSetup()
	if _, err := applysetup.CallLocal(ctx, socket, raw, true); err == nil {
		t.Fatal("absent check passed")
	}
	report, err := applysetup.CallLocal(ctx, socket, raw, false)
	if err != nil || len(report.Created) != 3 || len(report.Pending) != 0 {
		t.Fatal(report, err)
	}
	if report, err = applysetup.CallLocal(ctx, socket, raw, true); err != nil || len(report.Present) != 3 {
		t.Fatal(report, err)
	}
	public := do(t, srv, "GET", "/api/settings", nil)
	if public.Code != 200 || !strings.Contains(public.Body.String(), "/api/sso/kyidentity/login") || strings.Contains(public.Body.String(), "private-fixture-secret") {
		t.Fatal("bad provider projection", public.Code, public.Body)
	}
	for _, sub := range []string{"owner", "daily"} {
		u, err := st.Users().GetUserBySSO(ctx, store.InstallerProvider, sub)
		if err != nil {
			t.Fatal(err)
		}
		login := httptest.NewRecorder()
		if _, _, err = auth.NewSessionManager(st, cfg.Security).IssueSession(ctx, login, httptest.NewRequest("GET", "/", nil), u); err != nil {
			t.Fatal(err)
		}
		var cookie *http.Cookie
		for _, c := range login.Result().Cookies() {
			if c.Name == auth.SessionCookieName {
				cookie = c
			}
		}
		want := 200
		if sub == "daily" {
			want = 403
		}
		if w := do(t, srv, "GET", "/api/backup/status", cookie); w.Code != want {
			t.Fatal(sub, w.Code, w.Body)
		}
		if sub == "owner" {
			w := do(t, srv, "GET", "/api/settings", cookie)
			if strings.Contains(w.Body.String(), "installer.config") || strings.Contains(w.Body.String(), directory) {
				t.Fatal("protected path leaked", w.Body)
			}
			w = do(t, srv, "GET", "/api/settings/sso", cookie)
			var providers []sso.Provider
			if json.Unmarshal(w.Body.Bytes(), &providers) != nil || len(providers) != 1 || !providers[0].InstallerManaged || providers[0].ClientSecret != "" || !providers[0].SecretSet {
				t.Fatal("bad managed projection", w.Body)
			}
			w = adminDo(t, srv, cookie, "POST", "/api/settings/sso", map[string]any{"name": "Other", "kind": "oidc", "issuer": "https://foreign.example", "client_id": "other", "enabled": true})
			if w.Code != 409 {
				t.Fatal("managed provider edited", w.Code, w.Body)
			}
		}
		deliver := func(path, event, revision string, active, admin bool) int {
			t.Helper()
			roles := []any{}
			if admin {
				roles = append(roles, map[string]any{"value": "kyyard.admin"})
			}
			body, _ := json.Marshal(map[string]any{"id": sub, "active": active, "roles": roles, "meta": map[string]any{"version": `W/"` + revision + `"`}})
			r := httptest.NewRequest("POST", path, strings.NewReader(string(body)))
			headers, err := syncauth.Sign([]byte("private-fixture-secret"), time.Now(), event, "fixture-"+sub+"-"+revision, body)
			if err != nil {
				t.Fatal(err)
			}
			headers.Apply(r)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			return w.Code
		}
		if code := deliver("/api/installer/directory", "user.deactivated", "2", false, false); code != 204 {
			t.Fatal("offboard", sub, code)
		}
		if w := do(t, srv, "GET", "/api/auth/me", cookie); w.Code != 200 || !strings.Contains(w.Body.String(), `"authenticated":false`) {
			t.Fatal("session survived", w.Code, w.Body)
		}
		if w := do(t, srv, "GET", "/api/backup/status", cookie); w.Code != 401 {
			t.Fatal("revoked session retained protected access", w.Code, w.Body)
		}
		if code := deliver("/api/sso/kysignon/sync", "user.updated", "1", true, sub == "owner"); code != 422 {
			t.Fatal("stale legacy alias accepted", code)
		}
		if code := deliver("/api/installer/directory", "user.deactivated", "2", false, false); code != 204 {
			t.Fatal("idempotent resend", code)
		}
		if code := deliver("/api/installer/directory", "user.updated", "2", true, sub == "owner"); code != 422 {
			t.Fatal("conflicting revision accepted", code)
		}
		if code := deliver("/api/installer/directory", "user.updated", "3", true, sub == "owner"); code != 204 {
			t.Fatal("explicit reassign", code)
		}
	}
	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	if _, err := applysetup.CallLocal(ctx, socket, raw, true); err == nil {
		t.Fatal("missing private credential accepted")
	}
	if w := do(t, srv, "GET", "/api/sso/kyidentity/login", nil); w.Code != 404 {
		t.Fatal("missing credential sign-in remained available", w.Code, w.Body)
	}
}

func TestInstallerNetworkUsesNativeTrust(t *testing.T) {
	srv, _, cfg := setupTestServer(t)
	for _, trusted := range []bool{false, true} {
		cfg.Security.TrustedProxies = nil
		if trusted {
			cfg.Security.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32")}
		}
		r := httptest.NewRequest("GET", "/api/installer/network", nil)
		r.RemoteAddr = "192.0.2.10:1234"
		r.Header.Set("X-Forwarded-For", "198.51.100.8")
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		var got struct {
			IP      string `json:"clientIp"`
			Trusted bool   `json:"proxyHeadersTrusted"`
			Scheme  string `json:"scheme"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		ip, scheme := "192.0.2.10", "http"
		if trusted {
			ip, scheme = "198.51.100.8", "https"
		}
		if got.IP != ip || got.Scheme != scheme || got.Trusted != trusted || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(fmt.Sprintf("network %+v", got))
		}
	}
}
