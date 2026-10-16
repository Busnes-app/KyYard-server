package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const loginTestPassword = "SuperSecretPass123!"

func loginUser(t *testing.T, st store.Store, username, status string) {
	t.Helper()
	hash, err := password.Hash(loginTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Users().CreateUser(context.Background(), &store.User{
		ID: "usr_" + username, Username: username, PasswordHash: hash, Role: "user", Status: status, SSOProvider: "local",
	}); err != nil {
		t.Fatal(err)
	}
}

// login posts one attempt from peer and returns the status and body.
func login(t *testing.T, srv *api.Server, peer, username, pass string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": pass})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = peer
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// A botnet guessing one account from many addresses never meets the per-IP window, so the
// typed username backs off on its own, whether or not the account exists.
func TestLoginBacksOffPerUsername(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	loginUser(t, st, "alice", "active")
	peer := func(i int) string { return "203.0.113." + strconv.Itoa(i) + ":4000" }

	for _, name := range []string{"alice", "ghost"} {
		for i := 1; i <= 5; i++ {
			if code, _ := login(t, srv, peer(i), name, "WrongPassword123!"); code != http.StatusUnauthorized {
				t.Fatalf("%s failure %d answered %d, want 401", name, i, code)
			}
		}
		if code, _ := login(t, srv, peer(6), name, loginTestPassword); code != http.StatusTooManyRequests {
			t.Fatalf("%s after five failures answered %d, want 429", name, code)
		}
	}
	if code, _ := login(t, srv, peer(7), "ALICE", loginTestPassword); code != http.StatusTooManyRequests {
		t.Fatalf("a re-cased username answered %d, want the same 429", code)
	}

	// Each further failure doubles the delay.
	if d := api.LoginDelayForTest(srv, "alice"); d < 50*time.Second || d > time.Minute {
		t.Fatalf("first delay %s, want about a minute", d)
	}
	login(t, srv, peer(8), "alice", "WrongPassword123!")
	if d := api.LoginDelayForTest(srv, "alice"); d < 110*time.Second || d > 2*time.Minute {
		t.Fatalf("second delay %s, want about two minutes", d)
	}

	// Success clears the count.
	if code, body := login(t, srv, peer(9), "alice", loginTestPassword); code != http.StatusOK {
		t.Fatalf("correct password after the delay answered %d: %s", code, body)
	}
	for i := 1; i <= 5; i++ {
		if code, _ := login(t, srv, peer(10), "alice", "WrongPassword123!"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d after a success answered %d, want 401: success did not reset", i, code)
		}
	}
}

// Strangers guessing at an account must not delay its owner: a browser that has signed in
// as the username backs off on its own. A forged cookie, or one signed for another
// username, shares the stranger's delay.
func TestLoginDeviceCookieEscapesAStrangersBackoff(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	loginUser(t, st, "alice", "active")
	loginUser(t, st, "bob", "active")
	device := func(username string) *http.Cookie {
		body, _ := json.Marshal(map[string]string{"username": username, "password": loginTestPassword})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.10:4000"
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		for _, c := range w.Result().Cookies() {
			if c.Name == "ky_login_device" {
				if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/api/auth/login" {
					t.Fatalf("device cookie attributes: %+v", c)
				}
				return c
			}
		}
		t.Fatalf("%s's sign-in set no device cookie (%d)", username, w.Code)
		return nil
	}
	alices, bobs := device("alice"), device("bob")
	withCookie := func(c *http.Cookie, username string) int {
		body, _ := json.Marshal(map[string]string{"username": username, "password": loginTestPassword})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.10:4000"
		req.AddCookie(c)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code
	}

	for i := 1; i <= 5; i++ {
		login(t, srv, "203.0.113."+strconv.Itoa(i)+":4000", "alice", "WrongPassword123!")
	}
	if code, _ := login(t, srv, "192.0.2.10:4000", "alice", loginTestPassword); code != http.StatusTooManyRequests {
		t.Fatalf("no cookie answered %d, want the strangers' 429", code)
	}
	forged := &http.Cookie{Name: "ky_login_device", Value: strings.Repeat("a", 32) + "." + strings.Repeat("b", 64)}
	for name, c := range map[string]*http.Cookie{"forged": forged, "bob's": bobs} {
		if code := withCookie(c, "alice"); code != http.StatusTooManyRequests {
			t.Fatalf("a %s cookie answered %d, want 429", name, code)
		}
	}
	if code := withCookie(alices, "ALICE"); code != http.StatusOK {
		t.Fatalf("alice's own browser answered %d during the strangers' backoff, want 200", code)
	}
	if code, _ := login(t, srv, "203.0.113.9:4000", "alice", loginTestPassword); code != http.StatusTooManyRequests {
		t.Fatalf("the owner's success lifted the strangers' backoff (%d)", code)
	}
}

// A host is routinely handed a whole /64; each address in it must not be a fresh window.
func TestLoginWindowCoversAnIPv6Slash64(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	for i := 1; i <= 20; i++ {
		peer := "[2001:db8:1:2::" + strconv.FormatInt(int64(i), 16) + "]:4000"
		if code, _ := login(t, srv, peer, "user"+strconv.Itoa(i), "WrongPassword123!"); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d throttled, want no earlier than 21", i)
		}
	}
	if code, _ := login(t, srv, "[2001:db8:1:2:ffff::1]:4000", "user21", "WrongPassword123!"); code != http.StatusTooManyRequests {
		t.Fatalf("attempt 21 from the same /64 answered %d, want 429", code)
	}
	if code, _ := login(t, srv, "[2001:db8:1:3::1]:4000", "user22", "WrongPassword123!"); code == http.StatusTooManyRequests {
		t.Fatal("a different /64 shares the window")
	}
}

// Client-keyed windows are attacker-minted; filling them must not evict an account's.
func TestAccountLimitsSurviveAClientKeyFlood(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	loginUser(t, st, "alice", "active")
	for i := 1; i <= 5; i++ {
		login(t, srv, "203.0.113."+strconv.Itoa(i)+":4000", "alice", "WrongPassword123!")
	}
	api.AllowAccountAttemptForTest(srv, "mfa-user:usr_alice", 5, 5*time.Minute)

	for i := 0; i < api.AttemptsCapForTest+50; i++ {
		api.AllowAttemptForTest(srv, "login:198.51.100."+strconv.Itoa(i), 20, time.Minute)
	}
	if !slices.Contains(api.AccountKeysForTest(srv), "mfa-user:usr_alice") {
		t.Fatal("flooding client keys evicted an account window")
	}
	for i := 2; i <= 5; i++ {
		api.AllowAccountAttemptForTest(srv, "mfa-user:usr_alice", 5, 5*time.Minute)
	}
	if api.AllowAccountAttemptForTest(srv, "mfa-user:usr_alice", 5, 5*time.Minute) {
		t.Fatal("the account window restarted after the flood")
	}
	if code, _ := login(t, srv, "192.0.2.1:4000", "alice", loginTestPassword); code != http.StatusTooManyRequests {
		t.Fatalf("username backoff answered %d after the flood, want 429", code)
	}
}

// Unknown, disabled and wrong-password logins must be indistinguishable by status, body and
// cost; the disabled case is still recorded for the operator.
func TestLoginDoesNotEnumerateAccounts(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	loginUser(t, st, "alice", "active")
	loginUser(t, st, "dora", "disabled")

	type outcome struct {
		code int
		body string
		took time.Duration
	}
	attempt := func(username, pass string) outcome {
		start := time.Now()
		code, body := login(t, srv, "203.0.113.9:4000", username, pass)
		return outcome{code, body, time.Since(start)}
	}
	wrong := attempt("alice", "WrongPassword123!")
	unknown := attempt("nobody", loginTestPassword)
	disabled := attempt("dora", loginTestPassword)
	for name, o := range map[string]outcome{"unknown": unknown, "disabled": disabled} {
		if o.code != wrong.code || o.body != wrong.body {
			t.Errorf("%s answered %d %s, wrong password %d %s", name, o.code, o.body, wrong.code, wrong.body)
		}
	}
	// Argon2 dominates a wrong-password login; skipping it is orders of magnitude faster.
	if again := attempt("nobody2", loginTestPassword); min(unknown.took, again.took) < wrong.took/4 {
		t.Errorf("unknown user took %s, wrong password %s: the unknown path skips the hash", min(unknown.took, again.took), wrong.took)
	}

	records, _, err := st.Audit().ListAuditRecords(context.Background(), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.UserID == "usr_dora" && r.Action == "auth.login" && r.Result == "denied" {
			return
		}
	}
	t.Fatal("a disabled account's correct-password login left no audit record")
}
