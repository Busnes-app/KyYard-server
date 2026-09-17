package api_test

import (
	"context"
	"errors"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type failingPing struct {
	store.Store
	timeout time.Duration
}

func (s *failingPing) Ping(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if ok {
		s.timeout = time.Until(deadline)
	}
	return errors.New("postgres://secret:password@private-db")
}

func TestHealthReadinessAndShutdown(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	check := func(s *api.Server, path, method string, status int, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if w.Code != status || w.Body.String() != body {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("probe can be cached")
		}
	}
	check(srv, "/health/ready", "GET", 200, "{\"status\":\"ok\"}\n")
	check(srv, "/health/live", "HEAD", 200, "")
	broken := &failingPing{Store: st}
	unavailable := api.NewServer(cfg, broken)
	check(unavailable, "/health/live", "GET", 200, "{\"status\":\"ok\"}\n")
	check(unavailable, "/health/ready", "GET", 503, "{\"status\":\"unavailable\"}\n")
	if broken.timeout <= 0 || broken.timeout > 2*time.Second {
		t.Fatal("missing bounded ping deadline")
	}
	srv.BeginShutdown()
	check(srv, "/health/ready", "HEAD", 503, "")
	check(srv, "/health/live", "GET", 200, "{\"status\":\"ok\"}\n")
	check(srv, "/health/ready", "POST", 405, "{\"error\":\"Method not allowed\"}\n")
}

func TestLoginRejectsForeignOriginBeforeCredentials(t *testing.T) {
	srv, _, cfg := setupTestServer(t)
	for _, origin := range []string{"https://attacker.example", "null", cfg.Server.AppURL + "/path", "http://evil@localhost:8080"} {
		req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{}`))
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != 403 || !strings.Contains(w.Body.String(), "Origin not allowed") {
			t.Fatalf("origin %q accepted: %d %s", origin, w.Code, w.Body.String())
		}
	}
	for _, origin := range []string{"", cfg.Server.AppURL} {
		req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{}`))
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if strings.Contains(w.Body.String(), "Origin not allowed") {
			t.Fatalf("valid origin %q rejected", origin)
		}
	}
}

func TestOptionalIdentityServicesDisabledByDefault(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	for _, path := range []string{"/api/sso/kysignon/login", "/api/sso/kysignon/callback", "/api/sso/kysignon/sync", "/saml/metadata", "/scim/v2/Users"} {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		want := 404
		if strings.HasPrefix(path, "/scim/") {
			want = 403
		}
		if w.Code != want {
			t.Errorf("%s: expected disabled %d, got %d", path, want, w.Code)
		}
	}
}
