package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/ky-primitives/totp"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const accountPassword = "AccountPassword123!"

// accountSession creates u with accountPassword and returns a caller that posts JSON
// with that session's cookies and CSRF header.
func accountSession(t *testing.T, srv *api.Server, st store.Store, cfg *config.Config, u *store.User) func(path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	if u.PasswordHash == "" && u.SSOProvider == "local" {
		u.PasswordHash, _ = password.Hash(accountPassword)
	}
	if err := st.Users().CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	if _, _, err := auth.NewSessionManager(st, cfg.Security).IssueSession(ctx, login, httptest.NewRequest("POST", "/api/auth/login", nil), u); err != nil {
		t.Fatal(err)
	}
	cookies := login.Result().Cookies()
	return func(path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
		for _, c := range cookies {
			req.AddCookie(c)
			if c.Name == auth.CSRFCookieName {
				req.Header.Set(auth.HeaderCSRF, c.Value)
			}
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}
}

// stepCodes returns codes for the current step and the next. Both stay valid while the
// test runs inside two steps, and spending them in order never trips replay protection.
func stepCodes(t *testing.T, secret string) (string, string) {
	t.Helper()
	now := time.Now()
	a, err := totp.Code(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := totp.Code(secret, now.Add(totp.Period*time.Second))
	return a, b
}

func want(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("got %d, want %d: %s", w.Code, status, w.Body)
	}
}

func TestMFAEnrolmentLifecycle(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	ctx := context.Background()
	call := accountSession(t, srv, st, cfg, &store.User{ID: "usr_mfa", Username: "mfa", Role: "user", Status: "active", SSOProvider: "local"})

	if w := call("/api/account/mfa/confirm", map[string]string{"password": accountPassword, "code": "000000"}); w.Code != http.StatusConflict {
		t.Fatal("confirm before enrol:", w.Code)
	}

	w := call("/api/account/mfa/enrol", map[string]string{})
	want(t, w, 200)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("secret response is cacheable")
	}
	var started map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	secret, uri := started["secret"], started["otpauth_uri"]
	if secret == "" || !strings.HasPrefix(uri, "otpauth://totp/") || !strings.Contains(uri, secret) {
		t.Fatal("bad enrolment payload", w.Body)
	}
	pending, _ := st.Users().GetUserByID(ctx, "usr_mfa")
	if pending.TOTPEnabled || pending.TOTPSecretEnc == "" || strings.Contains(pending.TOTPSecretEnc, secret) {
		t.Fatal("pending secret not sealed or already enabled")
	}
	first, next := stepCodes(t, secret)

	want(t, call("/api/account/mfa/confirm", map[string]string{"password": "wrong", "code": first}), 401)
	want(t, call("/api/account/mfa/confirm", map[string]string{"password": accountPassword, "code": "12345"}), 401)
	w = call("/api/account/mfa/confirm", map[string]string{"password": accountPassword, "code": first})
	want(t, w, 200)
	var issued struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &issued)
	if len(issued.RecoveryCodes) != 10 {
		t.Fatal("recovery codes", w.Body)
	}

	api.ResetAttemptsForTest(srv)
	want(t, call("/api/account/mfa/enrol", map[string]string{}), 409)

	// Password sign-in now stops at the second factor.
	body, _ := json.Marshal(map[string]string{"username": "mfa", "password": accountPassword})
	login := httptest.NewRecorder()
	srv.ServeHTTP(login, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(string(body))))
	if !strings.Contains(login.Body.String(), `"mfa_required":true`) {
		t.Fatal("login skipped MFA", login.Body)
	}

	want(t, call("/api/account/mfa/recovery-codes", map[string]string{"code": first}), 401) // replay
	w = call("/api/account/mfa/recovery-codes", map[string]string{"code": next})
	want(t, w, 200)
	var regenerated struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &regenerated)
	if len(regenerated.RecoveryCodes) != 10 || regenerated.RecoveryCodes[0] == issued.RecoveryCodes[0] {
		t.Fatal("codes not regenerated")
	}

	api.ResetAttemptsForTest(srv)
	want(t, call("/api/account/mfa/disable", map[string]string{"password": "wrong", "code": regenerated.RecoveryCodes[0]}), 401)
	want(t, call("/api/account/mfa/disable", map[string]string{"password": accountPassword, "code": issued.RecoveryCodes[0]}), 401) // superseded set
	want(t, call("/api/account/mfa/disable", map[string]string{"password": accountPassword, "code": regenerated.RecoveryCodes[0]}), 200)
	off, _ := st.Users().GetUserByID(ctx, "usr_mfa")
	if off.TOTPEnabled || off.TOTPSecretEnc != "" || off.RecoveryCodesHash != "[]" {
		t.Fatalf("MFA state survived disable: %+v", off)
	}
	want(t, call("/api/account/mfa/disable", map[string]string{"password": accountPassword, "code": regenerated.RecoveryCodes[1]}), 409)
	want(t, call("/api/account/mfa/recovery-codes", map[string]string{"code": next}), 409)

	audits, _, _ := st.Audit().ListAuditRecords(ctx, 0, 100)
	seen := map[string]bool{}
	for _, a := range audits {
		seen[a.Action] = true
	}
	for _, action := range []string{"auth.mfa.enrol_started", "auth.mfa.enabled", "auth.mfa.recovery_codes_regenerated", "auth.mfa.disabled"} {
		if !seen[action] {
			t.Error("missing audit", action)
		}
	}
}

func enabledMFAUser(t *testing.T, cfg *config.Config, id string) (*store.User, string) {
	t.Helper()
	secret, _ := totp.GenerateSecret()
	enc, _ := crypto.EncryptAESGCM([]byte(secret), cfg.Security.EncryptionKey)
	_, hashed, _ := auth.GenerateRecoveryCodes(2)
	return &store.User{ID: id, Username: id, Role: "user", Status: "active", SSOProvider: "local", TOTPEnabled: true, TOTPSecretEnc: enc, RecoveryCodesHash: hashed}, secret
}

func TestMFADisableWithTOTP(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	u, secret := enabledMFAUser(t, cfg, "usr_totp")
	call := accountSession(t, srv, st, cfg, u)
	code, _ := stepCodes(t, secret)
	want(t, call("/api/account/mfa/disable", map[string]string{"password": accountPassword}), 401)
	want(t, call("/api/account/mfa/disable", map[string]string{"password": accountPassword, "code": code}), 200)
	if got, _ := st.Users().GetUserByID(context.Background(), u.ID); got.TOTPEnabled {
		t.Fatal("still enabled")
	}
}

func TestAccountRoutesRefuse(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	routes := []string{"/api/account/password", "/api/account/mfa/enrol", "/api/account/mfa/confirm", "/api/account/mfa/disable", "/api/account/mfa/recovery-codes"}
	for _, path := range routes {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader("{}")))
		want(t, w, 401)
	}

	federated := accountSession(t, srv, st, cfg, &store.User{ID: "usr_fed", Username: "fed", Role: "user", Status: "active", SSOProvider: "oidc", SSOSubject: "sub"})
	flagged := accountSession(t, srv, st, cfg, &store.User{ID: "usr_flag", Username: "flag", Role: "user", Status: "active", SSOProvider: "local", MustChangePassword: true})
	for _, path := range routes {
		want(t, federated(path, map[string]string{}), 403)
		if w := flagged(path, map[string]string{}); w.Code != 403 || !strings.Contains(w.Body.String(), "password_change_required") {
			t.Fatal(path, w.Code, w.Body)
		}
	}

	call := accountSession(t, srv, st, cfg, &store.User{ID: "usr_limit", Username: "limit", Role: "user", Status: "active", SSOProvider: "local"})
	want(t, call("/api/account/mfa/enrol", map[string]string{"extra": "x"}), 400)

	// A session cookie without the CSRF header: these routes must stay outside the exempt /api/auth/mfa/ prefix.
	sess := httptest.NewRecorder()
	u, _ := st.Users().GetUserByID(context.Background(), "usr_limit")
	_, _, _ = auth.NewSessionManager(st, cfg.Security).IssueSession(context.Background(), sess, httptest.NewRequest("POST", "/", nil), u)
	req := httptest.NewRequest("POST", "/api/account/mfa/enrol", strings.NewReader("{}"))
	for _, c := range sess.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	want(t, w, 403)

	// The enrol request above spent one of five; the per-account MFA budget covers every MFA route.
	for range 4 {
		call("/api/account/mfa/disable", map[string]string{"password": "guess", "code": "000000"})
	}
	want(t, call("/api/account/mfa/confirm", map[string]string{"password": accountPassword, "code": "000000"}), 429)
}

func TestSelfServicePasswordChange(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	ctx := context.Background()
	call := accountSession(t, srv, st, cfg, &store.User{ID: "usr_pw", Username: "pw", Role: "user", Status: "active", SSOProvider: "local"})
	u, _ := st.Users().GetUserByID(ctx, "usr_pw")
	_, other, err := auth.NewSessionManager(st, cfg.Security).IssueSession(ctx, httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), u)
	if err != nil {
		t.Fatal(err)
	}
	const replacement = "ReplacementPassword456!"
	want(t, call("/api/account/password", map[string]string{"password": accountPassword, "new_password": "short"}), 400)
	want(t, call("/api/account/password", map[string]string{"password": accountPassword, "new_password": accountPassword}), 400)
	want(t, call("/api/account/password", map[string]string{"password": "wrong", "new_password": replacement}), 401)
	w := call("/api/account/password", map[string]string{"password": accountPassword, "new_password": replacement})
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), `"login_required":false`) {
		t.Fatal(w.Body)
	}

	me := func(req *http.Request) string {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Body.String()
	}
	stale := httptest.NewRequest("GET", "/api/auth/me", nil)
	stale.Header.Set("Authorization", "Bearer "+other)
	if strings.Contains(me(stale), `"authenticated":true`) {
		t.Fatal("other session survived")
	}
	fresh := httptest.NewRequest("GET", "/api/auth/me", nil)
	for _, c := range w.Result().Cookies() {
		fresh.AddCookie(c)
	}
	if !strings.Contains(me(fresh), `"authenticated":true`) {
		t.Fatal("this browser lost its session")
	}
	got, _ := st.Users().GetUserByID(ctx, "usr_pw")
	if ok, _ := password.Verify(replacement, got.PasswordHash); !ok {
		t.Fatal("new password not stored")
	}
}

func TestSelfServicePasswordChangeRequiresTOTP(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	u, secret := enabledMFAUser(t, cfg, "usr_pwmfa")
	call := accountSession(t, srv, st, cfg, u)
	code, _ := stepCodes(t, secret)
	const replacement = "ReplacementPassword456!"
	want(t, call("/api/account/password", map[string]string{"password": accountPassword, "new_password": replacement}), 401)
	want(t, call("/api/account/password", map[string]string{"password": accountPassword, "new_password": replacement, "code": code}), 200)
}
