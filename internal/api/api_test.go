package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Busness-app/ky-primitives/capsule"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busness-app/ky-primitives/keyfile"
	"github.com/Busness-app/ky-primitives/password"
	"github.com/Busness-app/ky-primitives/recoveryclient"
	"github.com/Busness-app/ky-primitives/recoverykey"
	"github.com/Busness-app/ky-primitives/totp"
	"github.com/Busness-app/kyyard-server/internal/api"
	"github.com/Busness-app/kyyard-server/internal/auth"
	"github.com/Busness-app/kyyard-server/internal/backup"
	"github.com/Busness-app/kyyard-server/internal/config"
	"github.com/Busness-app/kyyard-server/internal/crypto"
	"github.com/Busness-app/kyyard-server/internal/store"
	"github.com/Busness-app/kyyard-server/internal/testdb"
)

func setupTestServer(t *testing.T) (*api.Server, store.Store, *config.Config) {
	t.Helper()
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, _ := config.LoadFromEnv()
	db := testdb.Config(t)
	db.DataDir = cfg.Database.DataDir // testdb only picks the backend; keep the temp data dir
	cfg.Database = db
	cfg.Captcha.Provider = "none" // disable captcha for unit test speed

	st, err := store.Open(context.Background(), cfg.Database)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := api.NewServer(cfg, st)
	return srv, st, cfg
}

func TestAuthAndSessionEndpoints(t *testing.T) {
	srv, st, _ := setupTestServer(t)

	// Create test user
	passHash, _ := password.Hash("SuperSecretPass123!")
	user := &store.User{
		ID:           "usr_alice",
		Username:     "alice",
		Email:        "alice@busnes.app",
		DisplayName:  "Alice Admin",
		PasswordHash: passHash,
		Role:         "admin",
		Status:       "active",
		SSOProvider:  "local",
	}
	_ = st.Users().CreateUser(context.Background(), user)

	// 1. Login with correct password
	loginBody, _ := json.Marshal(map[string]string{
		"username": "alice",
		"password": "SuperSecretPass123!",
	})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("login expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	cookies := w.Result().Cookies()
	var sessionCookie *http.Cookie
	var csrfCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == auth.SessionCookieName {
			sessionCookie = c
		} else if c.Name == auth.CSRFCookieName {
			csrfCookie = c
			break
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatal("expected session and CSRF cookies in login response")
	}

	// 2. /api/auth/me
	meReq := httptest.NewRequest("GET", "/api/auth/me", nil)
	meReq.AddCookie(sessionCookie)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, meReq)

	if w.Code != http.StatusOK {
		t.Fatalf("me expected 200 OK, got %d", w.Code)
	}

	var meResp struct {
		Authenticated bool        `json:"authenticated"`
		User          *store.User `json:"user"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &meResp)
	if !meResp.Authenticated || meResp.User.Username != "alice" {
		t.Errorf("unexpected me response: %+v", meResp)
	}

	// 3. /api/settings
	setReq := httptest.NewRequest("GET", "/api/settings", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, setReq)
	if w.Code != http.StatusOK {
		t.Fatalf("settings expected 200 OK, got %d", w.Code)
	}

	// 4. /api/devices/pair/init
	pairReq := httptest.NewRequest("POST", "/api/devices/pair/init", nil)
	pairReq.AddCookie(sessionCookie)
	pairReq.AddCookie(csrfCookie)
	pairReq.Header.Set(auth.HeaderCSRF, csrfCookie.Value)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, pairReq)
	if w.Code != http.StatusOK {
		t.Fatalf("pair init expected 200 OK, got %d", w.Code)
	}

	// 5. /api/backup/drill
	drillReq := httptest.NewRequest("POST", "/api/backup/drill", nil)
	drillReq.AddCookie(sessionCookie)
	drillReq.AddCookie(csrfCookie)
	drillReq.Header.Set(auth.HeaderCSRF, csrfCookie.Value)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, drillReq)
	if w.Code != http.StatusOK {
		t.Fatalf("backup drill expected 200 OK, got %d", w.Code)
	}
}

func TestLoginRejectsUnparseableStoredHash(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	_ = st.Users().CreateUser(context.Background(), &store.User{
		ID: "usr_bad", Username: "bad", PasswordHash: "not-a-phc-string",
		Role: "user", Status: "active", SSOProvider: "local",
	})
	body, _ := json.Marshal(map[string]string{"username": "bad", "password": "whatever-long-enough"})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestInactiveAccountInvalidatesSession(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	hash, _ := password.Hash("SuperSecretPass123!")
	user := &store.User{ID: "usr_inactive", Username: "inactive", PasswordHash: hash, Role: "admin", Status: "active", SSOProvider: "local"}
	if err := st.Users().CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"username": user.Username, "password": "SuperSecretPass123!"})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body)))
	var session *http.Cookie
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == auth.SessionCookieName {
			session = cookie
		}
	}
	user.Status = "inactive"
	if err := st.Users().UpdateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(session)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"authenticated":false`)) {
		t.Fatalf("inactive account retained session: %s", w.Body.String())
	}
}

func TestCookieAuthenticatedWriteRequiresCSRF(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	session := loginAs(t, srv, st, "csrf-admin", "admin")
	req := httptest.NewRequest(http.MethodPost, "/api/settings/theme", bytes.NewBufferString(`{"theme":"oled"}`))
	req.AddCookie(session)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("write without CSRF got %d, want 403", w.Code)
	}
}

func TestMFATOTPRefusesReplay(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	ctx := context.Background()
	secret, _ := totp.GenerateSecret()
	enc, _ := crypto.EncryptAESGCM([]byte(secret), cfg.Security.EncryptionKey)
	_ = st.Users().CreateUser(ctx, &store.User{
		ID: "usr_mfa", Username: "mfa", Role: "user", Status: "active", SSOProvider: "local",
		TOTPEnabled: true, TOTPSecretEnc: enc,
	})
	code, _ := totp.Code(secret, time.Now())

	post := func() int {
		raw := crypto.RandomHex(32)
		_ = st.Sessions().CreateMFAChallenge(ctx, &store.MFAChallenge{
			TokenHash: crypto.SHA256Hex([]byte(raw)), UserID: "usr_mfa", ExpiresAt: time.Now().Add(time.Minute),
		}, "")
		body, _ := json.Marshal(map[string]string{"mfa_token": raw, "code": code})
		req := httptest.NewRequest("POST", "/api/auth/mfa/totp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code
	}
	if got := post(); got != http.StatusOK {
		t.Fatalf("first use: %d", got)
	}
	if got := post(); got != http.StatusUnauthorized {
		t.Fatalf("replay: got %d, want 401", got)
	}
}

// fakePairer stands in for KyRecovery: the real client refuses non-HTTPS and private hosts,
// so there is no way to exercise the pairing handler against a test server.
type fakePairer struct {
	result recoveryclient.PairingResult
}

func (f fakePairer) ClaimPairing(ctx context.Context, serverURL, pairingCode, serviceName, appName string) (recoveryclient.PairingResult, error) {
	return f.result, nil
}

func (f fakePairer) Deposit(context.Context, string, string, []byte) (recoveryclient.Receipt, error) {
	return recoveryclient.Receipt{}, errors.New("fakePairer does not deposit")
}

func TestPairRemoteStoresTheRecoveryKey(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	priv, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public()
	api.SetRecoveryClientForTest(srv, fakePairer{result: recoveryclient.PairingResult{
		APIToken: "tok_pair",
		Key:      recoveryclient.RecoveryKey{Public: pub, Threshold: 2, TotalShares: 3},
	}})

	// A URL longer than the Postgres audit column: the pairing must still be recorded.
	longURL := "https://recovery.busnes.app/" + strings.Repeat("a", 300)
	body, _ := json.Marshal(map[string]string{
		"recovery_url": longURL,
		"pairing_code": "123456",
	})
	req := httptest.NewRequest("POST", "/api/backup/pair-remote", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	cookie := loginAs(t, srv, st, "alice", "admin")
	req.AddCookie(cookie)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
	req.Header.Set(auth.HeaderCSRF, "test-csrf")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("pair: got %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["recovery_key_id"] != pub.ID() {
		t.Errorf("recovery_key_id: got %v, want %s", out["recovery_key_id"], pub.ID())
	}

	got, err := recoveryclient.LoadRecoveryKey(cfg.Database.DataDir, backup.Settings(context.Background(), st.Settings()))
	if err != nil {
		t.Fatalf("load pinned key: %v", err)
	}
	if got.Public.ID() != pub.ID() || got.Threshold != 2 || got.TotalShares != 3 {
		t.Errorf("pinned key: got %+v", got)
	}
	records, _, err := st.Audit().ListAuditRecords(context.Background(), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var paired *store.AuditRecord
	for _, rec := range records {
		if rec.Action == "backup.paired" {
			paired = rec
		}
	}
	if paired == nil {
		t.Fatal("no backup.paired audit row for a long recovery URL")
	}
	if len(paired.Resource) > 255 || !strings.HasPrefix(paired.Resource, "https://recovery.busnes.app/") {
		t.Errorf("resource: %d bytes, %.40q", len(paired.Resource), paired.Resource)
	}
	if !strings.Contains(paired.Details, "recovery_key_id="+pub.ID()) {
		t.Errorf("details do not name the key: %s", paired.Details)
	}

	// The sealed token must not reach the admin settings screen under any spelling: the
	// filter drops every kyrecovery_token* row the lib may write.
	settingsBody := adminDo(t, srv, cookie, "GET", "/api/settings", nil)
	if settingsBody.Code != http.StatusOK {
		t.Fatalf("settings: got %d: %s", settingsBody.Code, settingsBody.Body.String())
	}
	if strings.Contains(settingsBody.Body.String(), "tok_pair") || strings.Contains(settingsBody.Body.String(), "kyrecovery_token") {
		t.Errorf("/api/settings carries the KyRecovery token: %s", settingsBody.Body.String())
	}

	// Pairing again to a different key must not silently re-point the product.
	other, _ := recoverykey.Generate()
	api.SetRecoveryClientForTest(srv, fakePairer{result: recoveryclient.PairingResult{
		APIToken: "tok_pair2",
		Key:      recoveryclient.RecoveryKey{Public: other.Public(), Threshold: 2, TotalShares: 3},
	}})
	req2 := httptest.NewRequest("POST", "/api/backup/pair-remote", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.AddCookie(cookie)
	req2.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
	req2.Header.Set(auth.HeaderCSRF, "test-csrf")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("re-pair to a different key: got %d, want 409: %s", w2.Code, w2.Body.String())
	}
}

// A recovery.pub swapped under a live instance must stop the export, not produce a capsule
// sealed to a key the suite's custodians cannot open.
func TestExportCapsuleRefusesAMismatchedRecoveryKey(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	ctx := context.Background()

	pinned, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, backup.Settings(ctx, st.Settings()),
		recoveryclient.RecoveryKey{Public: pinned.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}

	// Swap the file for a different key, leaving the settings pin on the first one.
	other, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := recoveryclient.RecoveryKeyPath(cfg.Database.DataDir)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := keyfile.Store(path, other.Public().Bytes(), keyfile.Raw); err != nil {
		t.Fatal(err)
	}

	w := adminPost(t, srv, loginAs(t, srv, st, "alice", "admin"), "/api/backup/export-capsule")

	if w.Code != http.StatusConflict {
		t.Fatalf("export with a swapped recovery.pub: got %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pinned key ID") {
		t.Errorf("body does not name the condition: %s", w.Body.String())
	}
}

// A database that has outgrown the capsule's per-member cap must say so: an operator whose
// backups have silently stopped working needs the limit, not a bare 500.
func TestExportCapsuleRejectsAnOversizedPayload(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	ctx := context.Background()

	key, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, backup.Settings(ctx, st.Settings()),
		recoveryclient.RecoveryKey{Public: key.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}

	// A real database one blob past the per-member cap: the collector snapshots with VACUUM
	// INTO, so the file has to be a database, and zeroblob makes a large one instantly.
	big := filepath.Join(t.TempDir(), "oversized.db")
	bigStore, err := store.Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: big})
	if err != nil {
		t.Fatal(err)
	}
	if err := bigStore.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", big)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("CREATE TABLE big(b BLOB); INSERT INTO big VALUES (zeroblob(%d))", recoveryclient.MaxCapsuleFileBytes+1)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	cfg.Database.Driver = "sqlite"
	cfg.Database.DSN = big

	w := adminPost(t, srv, loginAs(t, srv, st, "alice", "admin"), "/api/backup/export-capsule")

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized export: got %d, want 413: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "64 MiB per file") {
		t.Errorf("body does not name the limit: %s", w.Body.String())
	}
}

// The MFA routes are unauthenticated and the token is caller-supplied up to the 1 MiB body
// cap. Keying the limiter on it let a caller pin arbitrary memory for the whole window.
func TestMFALimiterKeyIsBounded(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	for i := 0; i < 500; i++ {
		token := strings.Repeat("a", 700_000) + strconv.Itoa(i)
		body, _ := json.Marshal(map[string]string{"mfa_token": token, "code": "000000"})
		req := httptest.NewRequest("POST", "/api/auth/mfa/totp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.9:41000"
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}

	keys := api.AttemptKeysForTest(srv)
	if len(keys) > api.AttemptsCapForTest {
		t.Errorf("limiter holds %d keys, want at most %d", len(keys), api.AttemptsCapForTest)
	}
	mfaKeys := 0
	for _, k := range keys {
		if len(k) >= 200 {
			t.Fatalf("limiter key is %d bytes; keys must not carry the request body", len(k))
		}
		if strings.HasPrefix(k, "mfa:") {
			mfaKeys++
		}
	}
	// One client, 500 distinct oversized tokens, one slot: nothing caller-supplied is in the key.
	if mfaKeys > 1 {
		t.Errorf("500 tokens from one IP took %d MFA slots, want at most 1: %v", mfaKeys, keys)
	}

	// Fill the limiter past the cap with live windows. A full map must evict, not refuse:
	// refusing would let one caller lock every new client out of the auth routes.
	for i := 0; i < api.AttemptsCapForTest+50; i++ {
		api.AllowAttemptForTest(srv, "filler:"+strconv.Itoa(i), 3, 5*time.Minute)
	}
	if n := len(api.AttemptKeysForTest(srv)); n > api.AttemptsCapForTest {
		t.Fatalf("limiter grew to %d keys, want at most %d", n, api.AttemptsCapForTest)
	}

	body, _ := json.Marshal(map[string]string{"mfa_token": "a-fresh-token", "code": "000000"})
	req := httptest.NewRequest("POST", "/api/auth/mfa/totp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.7:2000"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Fatal("a full limiter locked out a new client; it must evict, not refuse")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("fresh client expected 401 from MFA validation, got %d: %s", w.Code, w.Body.String())
	}
	if n := len(api.AttemptKeysForTest(srv)); n > api.AttemptsCapForTest {
		t.Errorf("limiter holds %d keys after admitting a new client, want at most %d", n, api.AttemptsCapForTest)
	}
}

// The poll route is unauthenticated: anyone holding a secret must not learn the code, the
// user behind it, or the device's push token.
func TestPairPollProjectsTheRecord(t *testing.T) {
	srv, st, _ := setupTestServer(t)

	pairing := &store.DevicePairing{
		Code:       "424242",
		Secret:     "s3cr3t-pairing-secret",
		UserID:     "usr_alice",
		DeviceName: "Alice Phone",
		Platform:   "android",
		PushToken:  "push-token-value",
		Status:     "pending",
		CreatedAt:  time.Now().UTC(),
		ExpiresAt:  time.Now().UTC().Add(90 * time.Second),
	}
	if err := st.Devices().CreatePairing(context.Background(), pairing); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/devices/pair/poll?secret="+pairing.Secret, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("poll expected 200, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	for _, leak := range []string{"secret", "push_token", "code", "user_id", pairing.Secret, pairing.Code, pairing.PushToken, pairing.UserID} {
		if strings.Contains(body, leak) {
			t.Errorf("poll response leaks %q: %s", leak, body)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "pending" || got["device_name"] != "Alice Phone" {
		t.Errorf("poll response lost the fields the client needs: %v", got)
	}
	if _, ok := got["expires_at"]; !ok {
		t.Errorf("poll response has no expires_at: %v", got)
	}
}

// Eviction must not favour long windows. Login windows are a minute and MFA windows a minute,
// but any caller that can mint keys at all would starve whichever window is shortest.
func TestFullLimiterStillThrottlesLogin(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	// Fill the map with the long windows an attacker minting MFA keys would leave behind.
	// Login's window is a minute, so "evict the entry nearest expiry" would always pick it.
	for i := 0; i < api.AttemptsCapForTest; i++ {
		api.AllowAttemptForTest(srv, "mfa:"+strconv.Itoa(i), 3, 5*time.Minute)
	}

	loginBody, _ := json.Marshal(map[string]string{"username": "nobody", "password": "WrongPass123!"})
	throttled := false
	for i := 1; i <= 25; i++ {
		// A fresh MFA token before each login: under the old design this minted a new key and
		// evicted login's window before it could ever count to its limit.
		mfaBody, _ := json.Marshal(map[string]string{"mfa_token": "tok-" + strconv.Itoa(i), "code": "000000"})
		mreq := httptest.NewRequest("POST", "/api/auth/mfa/totp", bytes.NewReader(mfaBody))
		mreq.Header.Set("Content-Type", "application/json")
		mreq.RemoteAddr = "192.0.2.9:41000"
		srv.ServeHTTP(httptest.NewRecorder(), mreq)

		req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(loginBody))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.9:41000"
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if w.Code == http.StatusTooManyRequests {
			if i < 21 {
				t.Fatalf("login throttled at attempt %d, want no earlier than 21", i)
			}
			throttled = true
			break
		}
		if i >= 21 {
			t.Fatalf("login attempt %d answered %d, not 429: the login window is being evicted", i, w.Code)
		}
	}
	if !throttled {
		t.Fatal("25 logins from one IP were never throttled")
	}
}

// A botnet spread across many IPs must not outrun the per-IP window when guessing one account.
func TestMFAPerAccountWindow(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	ctx := context.Background()

	passHash, _ := password.Hash("SuperSecretPass123!")
	if err := st.Users().CreateUser(ctx, &store.User{
		ID: "usr_carol", Username: "carol", Email: "carol@busnes.app",
		PasswordHash: passHash, Role: "user", Status: "active", SSOProvider: "local",
	}); err != nil {
		t.Fatal(err)
	}

	var codes int
	for i := 1; i <= 6; i++ {
		raw := "challenge-token-" + strconv.Itoa(i)
		if err := st.Sessions().CreateMFAChallenge(ctx, &store.MFAChallenge{
			TokenHash: crypto.SHA256Hex([]byte(raw)),
			UserID:    "usr_carol",
			ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
		}, passHash); err != nil {
			t.Fatal(err)
		}

		body, _ := json.Marshal(map[string]string{"mfa_token": raw, "code": "000000"})
		req := httptest.NewRequest("POST", "/api/auth/mfa/totp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113." + strconv.Itoa(i) + ":40000" // a different client every time
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if i < 6 {
			if w.Code == http.StatusTooManyRequests {
				t.Fatalf("attempt %d for the account was throttled, want no earlier than 6", i)
			}
			codes++
			continue
		}
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt 6 for the account answered %d, want 429", w.Code)
		}
	}
	if codes != 5 {
		t.Fatalf("only %d attempts got through before the account window closed, want 5", codes)
	}
}

// mfaStatus fires one MFA attempt from peer, optionally claiming to be forwarded for xff.
func mfaStatus(t *testing.T, srv *api.Server, peer, xff string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"mfa_token": "tok", "code": "000000"})
	req := httptest.NewRequest("POST", "/api/auth/mfa/totp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = peer
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w.Code
}

// With no trusted proxy configured, X-Forwarded-For is caller-supplied and must not open a
// fresh limiter bucket: one peer gets one window however many clients it claims to be.
func TestLimiterIgnoresForgedForwardedFor(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	for i := 1; i <= 20; i++ {
		xff := "198.51.100.1"
		if i%2 == 0 {
			xff = "198.51.100.2"
		}
		if code := mfaStatus(t, srv, "203.0.113.5:41000", xff); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d throttled, want no earlier than 21", i)
		}
	}
	if code := mfaStatus(t, srv, "203.0.113.5:41000", "198.51.100.3"); code != http.StatusTooManyRequests {
		t.Fatalf("attempt 21 answered %d, want 429: a forged header bought a second window", code)
	}
}

// Behind a configured trusted proxy the peer is the proxy for every request. Without the
// forwarded client in the key, one bucket would cover the whole instance.
func TestLimiterSplitsPerForwardedClientBehindTrustedProxy(t *testing.T) {
	t.Setenv("KY_TRUSTED_PROXIES", "192.0.2.0/24")
	srv, _, _ := setupTestServer(t)

	for i := 1; i <= 20; i++ {
		for _, client := range []string{"198.51.100.1", "198.51.100.2"} {
			if code := mfaStatus(t, srv, "192.0.2.7:41000", client); code == http.StatusTooManyRequests {
				t.Fatalf("client %s throttled at attempt %d; the two clients share a window", client, i)
			}
		}
	}
	if code := mfaStatus(t, srv, "192.0.2.7:41000", "198.51.100.1"); code != http.StatusTooManyRequests {
		t.Fatalf("attempt 21 for one forwarded client answered %d, want 429", code)
	}
}

// A chain that ends in another trusted proxy names no client we can attribute to, so the
// requests fall back to the peer and share the proxy's own window.
func TestLimiterFallsBackWhenTheChainIsAllTrusted(t *testing.T) {
	t.Setenv("KY_TRUSTED_PROXIES", "192.0.2.0/24")
	srv, _, _ := setupTestServer(t)

	for i := 1; i <= 20; i++ {
		if code := mfaStatus(t, srv, "192.0.2.7:41000", "192.0.2.8, 192.0.2.9"); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d throttled, want no earlier than 21", i)
		}
	}
	if code := mfaStatus(t, srv, "192.0.2.7:41000", "192.0.2.8, 192.0.2.9"); code != http.StatusTooManyRequests {
		t.Fatalf("attempt 21 answered %d, want 429", code)
	}
	for _, k := range api.AttemptKeysForTest(srv) {
		if k == "mfa:192.0.2.7" {
			return
		}
	}
	t.Fatalf("no window keyed on the peer; keys: %v", api.AttemptKeysForTest(srv))
}

// A session records the address it was issued to. Binding it to an unvalidated header let any
// caller write whatever origin they liked into the audit trail, and made the limiter's key and
// the session's IP disagree about who the caller was.
func TestSessionIPIgnoresForgedForwardedFor(t *testing.T) {
	srv, st, _ := setupTestServer(t)

	hash, _ := password.Hash("SuperSecretPass123!")
	if err := st.Users().CreateUser(context.Background(), &store.User{
		ID: "usr_dave", Username: "dave", PasswordHash: hash,
		Role: "user", Status: "active", SSOProvider: "local",
	}); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"username": "dave", "password": "SuperSecretPass123!"})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "10.66.66.66")
	req.Header.Set("X-Real-IP", "10.77.77.77")
	req.RemoteAddr = "203.0.113.5:41000"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login: got %d: %s", w.Code, w.Body.String())
	}

	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	sess, err := st.Sessions().GetSession(context.Background(), crypto.SHA256Hex([]byte(cookie.Value)))
	if err != nil {
		t.Fatal(err)
	}
	if sess.IPAddress != "203.0.113.5" {
		t.Errorf("session bound to %q, want the peer 203.0.113.5: a header decided the address", sess.IPAddress)
	}
}

// fakeDepositor is kyrecovery's deposit route: it answers with the receipt the real server
// would for the bytes it received.
type fakeDepositor struct {
	fakePairer
	err error
	got []byte
}

func (f *fakeDepositor) Deposit(_ context.Context, _, _ string, container []byte) (recoveryclient.Receipt, error) {
	f.got = container
	if f.err != nil {
		return recoveryclient.Receipt{}, f.err
	}
	m, err := capsule.ReadUnverifiedManifest(container)
	if err != nil {
		return recoveryclient.Receipt{}, err
	}
	sum := sha256.Sum256(container)
	return recoveryclient.Receipt{CapsuleID: m.CapsuleID, Digest: hex.EncodeToString(sum[:]), SizeBytes: int64(len(container)), DepositedAt: time.Now().UTC()}, nil
}

func adminPost(t *testing.T, srv *api.Server, session *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, nil)
	req.AddCookie(session)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
	req.Header.Set(auth.HeaderCSRF, "test-csrf")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func backupSettings(ctx context.Context, st store.Store) recoveryclient.Settings {
	return backup.Settings(ctx, st.Settings())
}

// storePairing seals a KyRecovery pairing the way the handler does.
func storePairing(t *testing.T, cfg *config.Config, st store.Store, url, token string) error {
	t.Helper()
	sealer, err := backup.NewSealer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return recoveryclient.StorePairing(backup.Settings(context.Background(), st.Settings()), sealer, url, token)
}

// setupSQLiteServer is setupTestServer pinned to SQLite: deposits snapshot the database, and
// only the SQLite path can. The Postgres CI job would otherwise (correctly) refuse them.
func setupSQLiteServer(t *testing.T) (*api.Server, store.Store, *config.Config) {
	t.Helper()
	t.Setenv("KY_TEST_POSTGRES_DSN", "")
	return setupTestServer(t)
}

func TestDepositRequiresAPairing(t *testing.T) {
	srv, st, _ := setupSQLiteServer(t)
	fake := &fakeDepositor{}
	api.SetRecoveryClientForTest(srv, fake)
	session := loginAs(t, srv, st, "alice", "admin")
	if w := adminPost(t, srv, session, "/api/backup/deposit"); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("unpaired deposit: got %d, want 412: %s", w.Code, w.Body.String())
	}
	if fake.got != nil {
		t.Error("an unpaired instance sent bytes to the store")
	}
}

func TestDepositSealsToThePinnedKeyAndAudits(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	ctx := context.Background()
	priv, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, backup.Settings(ctx, st.Settings()), recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	if err := storePairing(t, cfg, st, "https://recovery.busnes.app", "kyrec_live_t"); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDepositor{}
	api.SetRecoveryClientForTest(srv, fake)
	session := loginAs(t, srv, st, "alice", "admin")

	w := adminPost(t, srv, session, "/api/backup/deposit")
	if w.Code != http.StatusOK {
		t.Fatalf("deposit: got %d: %s", w.Code, w.Body.String())
	}
	var res recoveryclient.Result
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Receipt == nil {
		t.Fatalf("decode result: %v %+v", err, res)
	}
	rcpt := *res.Receipt
	if _, _, err := capsule.Open(fake.got, priv, t.TempDir()); err != nil {
		t.Fatalf("what the store received does not open with the suite key: %v", err)
	}
	last, ok, err := recoveryclient.LastDeposit(backup.Settings(ctx, st.Settings()))
	if err != nil || !ok || last.CapsuleID != rcpt.CapsuleID {
		t.Errorf("last deposit: %+v %v %v, want %s", last, ok, err, rcpt.CapsuleID)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var audited bool
	for _, rec := range records {
		if rec.Action == "admin.backup_run" && rec.Resource == rcpt.CapsuleID && rec.UserID == "usr_alice" && strings.Contains(rec.Details, `outcome="success"`) {
			audited = true
		}
	}
	if !audited {
		t.Error("no successful admin.backup_run audit record for the acting admin")
	}

	// A refusal by the store is reported, audited with a bounded message, and leaves the
	// receipt untouched.
	fake.err = fmt.Errorf("%w: deposit rejected (502): %s", recoveryclient.ErrRemote, strings.Repeat("<html>", 20000)+"\x00\x1b")
	if w := adminPost(t, srv, session, "/api/backup/deposit"); w.Code != http.StatusBadGateway {
		t.Fatalf("refused deposit: got %d, want 502: %s", w.Code, w.Body.String())
	}
	// A failure this side of the wire is not a refusal by the store, and not a seal failure
	// either: a pinned URL the client refuses is the operator's problem to see plainly.
	fake.err = nil
	if err := storePairing(t, cfg, st, "http://recovery.busnes.app", "kyrec_live_t"); err != nil {
		t.Fatal(err)
	}
	api.SetRecoveryClientForTest(srv, recoveryclient.NewClient(recoveryclient.Options{}))
	w = adminPost(t, srv, session, "/api/backup/deposit")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("local failure: got %d, want 500: %s", w.Code, w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "seal") {
		t.Errorf("a refused recovery URL was reported as a seal failure: %s", w.Body.String())
	}
	if again, _, _ := recoveryclient.LastDeposit(backup.Settings(ctx, st.Settings())); again.CapsuleID != rcpt.CapsuleID {
		t.Error("a refused deposit replaced the last receipt")
	}
	records, _, _ = st.Audit().ListAuditRecords(ctx, 0, 50)
	var failed *store.AuditRecord
	for _, rec := range records {
		if rec.Action == "admin.backup_run" && strings.Contains(rec.Details, `outcome="failure"`) {
			failed = rec
		}
	}
	if failed == nil {
		t.Fatal("no failed admin.backup_run record")
	}
	if len(failed.Details) > 300 || strings.ContainsAny(failed.Details, "\x00\x1b") {
		t.Errorf("audit details are %d bytes with control characters: %.60q", len(failed.Details), failed.Details)
	}
}

func TestAuditDetailsAlwaysCarriesTheOutcome(t *testing.T) {
	got := api.AuditDetails(map[string]any{
		"capsule_id": "cap-Busnes.app-1788605720094118543",
		"error":      "remote said outcome=success " + strings.Repeat("x", 200),
		"local_path": "/backups/a.kycap",
		"outcome":    "failure",
		"size_bytes": 1234,
	})
	if !strings.Contains(got, `outcome="failure"`) {
		t.Fatalf("AuditDetails lost outcome: %q", got)
	}
	if strings.Count(got, "outcome=") != 1 {
		t.Fatalf("AuditDetails allowed a value to forge outcome: %q", got)
	}
	if !strings.Contains(got, `local_path="/backups/a.kycap"`) {
		t.Fatalf("AuditDetails lost local path before remote error: %q", got)
	}
	if len(got) > 210 {
		t.Fatalf("AuditDetails length = %d, want bounded: %q", len(got), got)
	}
}

// cancellingDepositor cancels the request while the upload is in flight, the way a closed
// browser tab does, and reports whether its own context survived.
type cancellingDepositor struct {
	fakeDepositor
	cancel context.CancelFunc
	err    error
}

func (c *cancellingDepositor) Deposit(ctx context.Context, url, token string, container []byte) (recoveryclient.Receipt, error) {
	c.cancel()
	c.err = ctx.Err()
	return c.fakeDepositor.Deposit(ctx, url, token, container)
}

// Once bytes are on their way the deposit must finish and record its receipt even if the
// admin's connection is gone; otherwise KyRecovery holds a capsule this instance has no
// record of, which is the value a restore is checked against.
func TestDepositOutlivesTheRequest(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	ctx := context.Background()
	priv, _ := recoverykey.Generate()
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, backup.Settings(ctx, st.Settings()), recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	if err := storePairing(t, cfg, st, "https://recovery.busnes.app", "kyrec_live_t"); err != nil {
		t.Fatal(err)
	}
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fake := &cancellingDepositor{cancel: cancel}
	api.SetRecoveryClientForTest(srv, fake)

	req := httptest.NewRequest("POST", "/api/backup/deposit", nil).WithContext(reqCtx)
	req.AddCookie(loginAs(t, srv, st, "alice", "admin"))
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
	req.Header.Set(auth.HeaderCSRF, "test-csrf")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if fake.err != nil {
		t.Fatalf("the upload context was cancelled with the request: %v", fake.err)
	}
	rcpt, ok, _ := recoveryclient.LastDeposit(backup.Settings(ctx, st.Settings()))
	if !ok {
		t.Fatal("no receipt recorded after the request went away")
	}
	// The audit row must survive the dropped connection too, and name the admin who acted.
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var audited bool
	for _, rec := range records {
		if rec.Action == "admin.backup_run" && rec.Resource == rcpt.CapsuleID && rec.UserID == "usr_alice" && strings.Contains(rec.Details, `outcome="success"`) {
			audited = true
		}
	}
	if !audited {
		t.Error("no successful admin.backup_run audit record for the acting admin after the request went away")
	}
}

func TestMFAChallengeRejectedAfterPasswordRotation(t *testing.T) {
	srv, st, cfg := setupTestServer(t)
	ctx := context.Background()
	secret, _ := totp.GenerateSecret()
	enc, _ := crypto.EncryptAESGCM([]byte(secret), cfg.Security.EncryptionKey)
	if err := st.Users().CreateUser(ctx, &store.User{
		ID: "usr_mfa", Username: "mfa", Role: "user", Status: "active", SSOProvider: "local",
		PasswordHash: "old", TOTPEnabled: true, TOTPSecretEnc: enc,
	}); err != nil {
		t.Fatal(err)
	}
	code, _ := totp.Code(secret, time.Now())

	post := func() int {
		raw := crypto.RandomHex(32)
		if err := st.Sessions().CreateMFAChallenge(ctx, &store.MFAChallenge{
			TokenHash: crypto.SHA256Hex([]byte(raw)), UserID: "usr_mfa", ExpiresAt: time.Now().Add(time.Minute),
		}, "old"); err != nil {
			t.Fatal(err)
		}
		user, err := st.Users().GetUserByID(ctx, "usr_mfa")
		if err != nil {
			t.Fatal(err)
		}
		user.PasswordHash = "new"
		if err := st.Users().UpdateUser(ctx, user); err != nil {
			t.Fatal(err)
		}

		body, _ := json.Marshal(map[string]string{"mfa_token": raw, "code": code})
		req := httptest.NewRequest("POST", "/api/auth/mfa/totp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code
	}
	if got := post(); got != http.StatusForbidden {
		t.Fatalf("stale challenge: got %d, want 403", got)
	}
}
