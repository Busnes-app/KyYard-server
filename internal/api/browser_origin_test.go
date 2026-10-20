package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// Browser-origin hardening, after Joplin's GHSA-9728-v7ww-mxjv (CVE-2026-105783): a pairing
// endpoint that sent wildcard CORS and never checked Origin let any website mint a full-access
// token with one click. Each requirement below has its own test.

const evilOrigin = "https://evil.example"

// originRequest sends method path with body, the given Origin (none when empty), the cookie
// when set, and the CSRF header pair only when csrf is true.
func originRequest(s http.Handler, method, path, body, origin, contentType string, cookie *http.Cookie, csrf bool, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf {
		r.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "token"})
		r.Header.Set(auth.HeaderCSRF, "token")
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func corsHeaders(w *httptest.ResponseRecorder) []string {
	var found []string
	for k := range w.Header() {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "Access-Control-") {
			found = append(found, k)
		}
	}
	return found
}

// Requirement 1: /mcp refuses any request carrying an Origin header with 403, whatever the
// origin (a foreign site, KyYard's own, or the opaque "null" of a sandboxed page), whatever the
// method, and even with a valid operator token.
func TestMCPRefusesEveryOrigin(t *testing.T) {
	f := newTerminalFixture(t)
	tok := agentToken(t, f.st, "usr_execadmin", store.ScopeMCPOperate)
	for _, origin := range []string{evilOrigin, terminalOrigin, "null", "http://localhost:9273"} {
		for _, method := range []string{"POST", "GET", "OPTIONS", "DELETE"} {
			w := originRequest(f.s, method, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, origin, "application/json", nil, false,
				map[string]string{"Authorization": "Bearer " + tok.Token, "Accept": "application/json, text/event-stream"})
			if w.Code != http.StatusForbidden {
				t.Errorf("%s /mcp from %s: %d, want 403", method, origin, w.Code)
			}
			if strings.Contains(w.Body.String(), "restart_container") {
				t.Errorf("%s /mcp from %s answered the tool list", method, origin)
			}
		}
	}
	// The same token without an Origin is admitted: the refusal is the header, not the token.
	if w := rawMCP(f.s, "POST", map[string]string{"Authorization": "Bearer " + tok.Token}, nil); w.Code != 200 {
		t.Fatalf("the token without an Origin: %d", w.Code)
	}
}

// Requirement 2: no response ever carries Access-Control-Allow-Origin: *. /mcp carries no CORS
// header at all, not even on a preflight. Every other route reflects only KyYard's own origin,
// with credentials, and grants a foreign origin nothing.
func TestNoWildcardCORSAndNoneOnMCP(t *testing.T) {
	f := newTerminalFixture(t)
	tok := agentToken(t, f.st, "usr_execadmin", store.ScopeMCPRead)
	for _, origin := range []string{"", evilOrigin, terminalOrigin, "null"} {
		for _, method := range []string{"POST", "OPTIONS", "GET"} {
			w := originRequest(f.s, method, "/mcp", `{}`, origin, "application/json", nil, false, map[string]string{"Authorization": "Bearer " + tok.Token, "Access-Control-Request-Method": "POST"})
			if h := corsHeaders(w); len(h) != 0 {
				t.Errorf("%s /mcp from %q sent CORS headers %v", method, origin, h)
			}
		}
	}
	paths := []string{"/api/organizations/a", "/api/organizations/a/agent-tokens", "/api/auth/me", "/healthz", "/agent/skills/kyyard/SKILL.md", "/"}
	for _, path := range paths {
		for _, method := range []string{"GET", "OPTIONS"} {
			for _, origin := range []string{evilOrigin, "null", terminalOrigin} {
				w := originRequest(f.s, method, path, "", origin, "", f.admin, false, map[string]string{"Access-Control-Request-Method": "POST"})
				got := w.Header().Get("Access-Control-Allow-Origin")
				if got == "*" {
					t.Fatalf("%s %s from %s: wildcard CORS", method, path, origin)
				}
				if origin == terminalOrigin {
					if got != terminalOrigin || w.Header().Get("Access-Control-Allow-Credentials") != "true" {
						t.Errorf("%s %s from the app's own origin: Allow-Origin %q", method, path, got)
					}
				} else if got != "" || w.Header().Get("Access-Control-Allow-Credentials") != "" {
					t.Errorf("%s %s from %s: granted %q", method, path, origin, got)
				}
			}
		}
	}
	// A foreign preflight is refused outright.
	if w := originRequest(f.s, "OPTIONS", "/api/organizations/a/agent-tokens", "", evilOrigin, "", nil, false, map[string]string{"Access-Control-Request-Method": "POST"}); w.Code != http.StatusForbidden {
		t.Fatalf("foreign preflight: %d", w.Code)
	}
}

// Requirement 3: creating, re-scoping and revoking an agent token need the administrator's
// session and its CSRF token, and a cross-origin Origin is refused even with both. A forged
// cross-site POST (the classic auto-submitting form, text/plain, cookie attached by the
// browser, no CSRF header) mints nothing, raises nothing to operator and revokes nothing.
func TestForgedCrossSiteAgentTokenWritesAreRefused(t *testing.T) {
	f := newTerminalFixture(t)
	admin := store.TenantAccess{ActorID: "usr_execadmin", OrganizationID: "a"}
	existing := agentToken(t, f.st, "usr_execadmin", store.ScopeMCPRead)
	base := "/api/organizations/a/agent-tokens"
	one := base + "/" + existing.ID
	forgeries := []struct {
		name, method, path, body, origin, contentType string
		csrf                                          bool
	}{
		{"cross-site form create", "POST", base, `{"name":"evil","scope":"mcp:operate"}`, evilOrigin, "text/plain", false},
		{"cross-site fetch create without CSRF", "POST", base, `{"name":"evil","scope":"mcp:operate"}`, evilOrigin, "application/json", false},
		{"create with a stolen CSRF pair from a foreign origin", "POST", base, `{"name":"evil","scope":"mcp:operate"}`, evilOrigin, "application/json", true},
		{"create from an opaque origin", "POST", base, `{"name":"evil","scope":"mcp:operate"}`, "null", "application/json", true},
		{"same-site create without CSRF", "POST", base, `{"name":"evil","scope":"mcp:operate"}`, "", "application/json", false},
		{"cross-site raise to operator", "PATCH", one, `{"scope":"mcp:operate"}`, evilOrigin, "application/json", true},
		{"raise to operator without CSRF", "PATCH", one, `{"scope":"mcp:operate"}`, "", "application/json", false},
		{"cross-site revoke", "DELETE", one, "", evilOrigin, "", true},
		{"revoke without CSRF", "DELETE", one, "", "", "", false},
	}
	for _, fg := range forgeries {
		if w := originRequest(f.s, fg.method, fg.path, fg.body, fg.origin, fg.contentType, f.admin, fg.csrf, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", fg.name, w.Code, w.Body)
		}
	}
	// No session at all is not a write either, CSRF pair or not.
	if w := originRequest(f.s, "POST", base, `{"name":"evil","scope":"mcp:operate"}`, "", "application/json", nil, true, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d, want 401", w.Code)
	}
	list, err := f.st.Tenancy().ListAgentTokens(f.ctx, admin)
	if err != nil || len(list) != 1 || list[0].ID != existing.ID || list[0].Scope != store.ScopeMCPRead {
		t.Fatalf("a forged request changed the tokens: %+v %v", list, err)
	}
	if rows := auditByActor(t, f.st, "usr_execadmin", "organization.agent_tokens.manage"); len(rows) != 1 {
		t.Fatalf("forged requests reached the store: %+v", rows)
	}
	// The control: the administrator's own page, same origin, session and CSRF, succeeds.
	if w := originRequest(f.s, "PATCH", one, `{"scope":"mcp:operate"}`, terminalOrigin, "application/json", f.admin, true, nil); w.Code != http.StatusNoContent {
		t.Fatalf("same-origin raise: %d %s", w.Code, w.Body)
	}
	if w := originRequest(f.s, "POST", base, `{"name":"mine","scope":"mcp:read"}`, terminalOrigin, "application/json", f.admin, true, nil); w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"name":"mine"`) || !strings.Contains(w.Body.String(), `"scope":"mcp:read"`) {
		t.Fatalf("same-origin create: %d %s", w.Code, w.Body)
	}
	if w := originRequest(f.s, "DELETE", one, "", terminalOrigin, "", f.admin, true, nil); w.Code != http.StatusNoContent {
		t.Fatalf("same-origin revoke: %d %s", w.Code, w.Body)
	}
}
