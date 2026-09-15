package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busness-app/ky-primitives/capsule"
	"github.com/Busness-app/ky-primitives/recoveryclient"
	"github.com/Busness-app/ky-primitives/recoverykey"
	"github.com/Busness-app/ky_server_base/internal/api"
	"github.com/Busness-app/ky_server_base/internal/auth"
	"github.com/Busness-app/ky_server_base/internal/store"
)

// adminDo is adminPost with a method and a JSON body.
func adminDo(t *testing.T, srv *api.Server, session *http.Cookie, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(session)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
	req.Header.Set(auth.HeaderCSRF, "test-csrf")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func pinBody(pub recoverykey.PublicKey, k, n int) map[string]any {
	return map[string]any{"public_key": base64.StdEncoding.EncodeToString(pub.Bytes()), "threshold": k, "total_shares": n}
}

func statusOf(t *testing.T, srv *api.Server, session *http.Cookie) map[string]any {
	t.Helper()
	w := adminDo(t, srv, session, "GET", "/api/backup/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func auditRows(t *testing.T, st store.Store, action string) []*store.AuditRecord {
	t.Helper()
	records, _, err := st.Audit().ListAuditRecords(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.AuditRecord
	for _, rec := range records {
		if rec.Action == action {
			out = append(out, rec)
		}
	}
	return out
}

func TestPinKeyIsWriteOnce(t *testing.T) {
	srv, st, _ := setupSQLiteServer(t)
	session := loginAs(t, srv, st, "alice", "admin")
	first, _ := recoverykey.Generate()
	if w := adminDo(t, srv, session, "POST", "/api/backup/pin-key", pinBody(first.Public(), 2, 3)); w.Code != http.StatusOK {
		t.Fatalf("pin: got %d: %s", w.Code, w.Body.String())
	}
	if w := adminDo(t, srv, session, "POST", "/api/backup/pin-key", pinBody(first.Public(), 2, 3)); w.Code != http.StatusOK {
		t.Fatalf("same key again: got %d: %s", w.Code, w.Body.String())
	}
	other, _ := recoverykey.Generate()
	if w := adminDo(t, srv, session, "POST", "/api/backup/pin-key", pinBody(other.Public(), 2, 3)); w.Code != http.StatusConflict {
		t.Fatalf("different key: got %d, want 409: %s", w.Code, w.Body.String())
	}
	var pinned, refused bool
	for _, rec := range auditRows(t, st, "admin.backup_key_pin") {
		switch {
		case rec.Resource == first.Public().ID() && strings.HasPrefix(rec.Details, "threshold=2"):
			pinned = true
		case rec.Resource == other.Public().ID() && strings.HasPrefix(rec.Details, "error="):
			refused = true
		}
	}
	if !pinned || !refused {
		t.Fatalf("pin audit rows: pinned=%v refused=%v", pinned, refused)
	}
	status := statusOf(t, srv, session)
	if status["key_pinned"] != true || status["recovery_key_id"] != first.Public().ID() {
		t.Errorf("status after pin: %v", status)
	}
}

func TestPinKeyBadTopology(t *testing.T) {
	srv, st, _ := setupSQLiteServer(t)
	session := loginAs(t, srv, st, "alice", "admin")
	priv, _ := recoverykey.Generate()
	if w := adminDo(t, srv, session, "POST", "/api/backup/pin-key", pinBody(priv.Public(), 1, 3)); w.Code != http.StatusBadRequest {
		t.Fatalf("1-of-3: got %d, want 400: %s", w.Code, w.Body.String())
	}
	if w := adminDo(t, srv, session, "POST", "/api/backup/pin-key", map[string]any{"public_key": "AAAA", "threshold": 2, "total_shares": 3}); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage key: got %d, want 400: %s", w.Code, w.Body.String())
	}
	if statusOf(t, srv, session)["key_pinned"] != false {
		t.Error("a refused pin left a key behind")
	}
}

func TestRunWithPinnedKeyAndNoDestination(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	cfg.Backup.Dir = ""
	fake := &fakeDepositor{}
	api.SetRecoveryClientForTest(srv, fake)
	session := loginAs(t, srv, st, "alice", "admin")
	priv, _ := recoverykey.Generate()
	if w := adminDo(t, srv, session, "POST", "/api/backup/pin-key", pinBody(priv.Public(), 2, 3)); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w := adminPost(t, srv, session, "/api/backup/deposit")
	if w.Code != http.StatusPreconditionFailed || !strings.Contains(w.Body.String(), "pair with KyRecovery or set KY_BACKUP_DIR") {
		t.Fatalf("no destination: got %d: %s", w.Code, w.Body.String())
	}
	if fake.got != nil {
		t.Error("an unpaired instance sent bytes to the store")
	}
}

func TestRunWritesLocalCopy0600(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	cfg.Backup.Dir = filepath.Join(t.TempDir(), "capsules")
	cfg.Backup.Keep = 3
	fake := &fakeDepositor{}
	api.SetRecoveryClientForTest(srv, fake)
	session := loginAs(t, srv, st, "alice", "admin")
	priv, _ := recoverykey.Generate()
	if w := adminDo(t, srv, session, "POST", "/api/backup/pin-key", pinBody(priv.Public(), 2, 3)); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w := adminPost(t, srv, session, "/api/backup/deposit")
	if w.Code != http.StatusOK {
		t.Fatalf("run: got %d: %s", w.Code, w.Body.String())
	}
	var res recoveryclient.Result
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Receipt != nil || res.LocalPath == "" || fake.got != nil {
		t.Fatalf("unpaired run %+v sent=%v", res, fake.got != nil)
	}
	info, err := os.Stat(res.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("local copy mode %o, want 0600", info.Mode().Perm())
	}
	if filepath.Dir(res.LocalPath) != cfg.Backup.Dir || !strings.HasSuffix(res.LocalPath, "."+res.Manifest.CapsuleID+".kycap") {
		t.Errorf("local copy path %q", res.LocalPath)
	}
	raw, _ := os.ReadFile(res.LocalPath)
	if _, files, err := capsule.Open(raw, priv, t.TempDir()); err != nil || len(files) < 2 {
		t.Fatalf("local copy does not open with the suite key: %v (%d files)", err, len(files))
	}
	var ok bool
	for _, rec := range auditRows(t, st, "admin.backup_run") {
		if rec.UserID == "usr_alice" && rec.Resource == res.Manifest.CapsuleID && strings.Contains(rec.Details, `outcome="success"`) && strings.Contains(rec.Details, "local_path=") {
			ok = true
		}
	}
	if !ok {
		t.Error("no successful admin.backup_run row naming the local copy")
	}
	status := statusOf(t, srv, session)
	copies, _ := status["local_copies"].([]any)
	if status["paired"] != false || status["key_pinned"] != true || len(copies) != 1 || status["local_dir"] != cfg.Backup.Dir {
		t.Errorf("status %v", status)
	}
}

func TestUnpairKeepsPin(t *testing.T) {
	srv, st, _ := setupSQLiteServer(t)
	priv, _ := recoverykey.Generate()
	api.SetRecoveryClientForTest(srv, fakePairer{result: recoveryclient.PairingResult{
		APIToken: "kyrec_live_t",
		Key:      recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3},
	}})
	session := loginAs(t, srv, st, "alice", "admin")
	pair := map[string]string{"recovery_url": "https://recovery.busnes.app", "pairing_code": "123456"}
	if w := adminDo(t, srv, session, "POST", "/api/backup/pair-remote", pair); w.Code != http.StatusOK {
		t.Fatalf("pair: got %d: %s", w.Code, w.Body.String())
	}
	if status := statusOf(t, srv, session); status["paired"] != true || status["recovery_url"] != pair["recovery_url"] {
		t.Fatalf("status after pair: %v", status)
	}
	w := adminDo(t, srv, session, "DELETE", "/api/backup/pairing", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("unpair: got %d: %s", w.Code, w.Body.String())
	}
	status := statusOf(t, srv, session)
	if status["paired"] != false || status["key_pinned"] != true || status["recovery_key_id"] != priv.Public().ID() {
		t.Errorf("unpair changed the pin: %v", status)
	}
	if _, has := status["recovery_url"]; has {
		t.Errorf("unpair left the URL row: %v", status)
	}
	rows := auditRows(t, st, "admin.backup_unpair")
	if len(rows) != 1 || rows[0].Resource != pair["recovery_url"] || rows[0].Details != "success" {
		t.Errorf("unpair audit: %+v", rows)
	}
	if w := adminDo(t, srv, session, "DELETE", "/api/backup/pairing", nil); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("second unpair: got %d, want 412: %s", w.Code, w.Body.String())
	}
}

func TestScheduleBounds(t *testing.T) {
	srv, st, _ := setupSQLiteServer(t)
	session := loginAs(t, srv, st, "alice", "admin")
	for _, tc := range []struct {
		sec  int64
		want int
	}{{0, 200}, {899, 400}, {1 << 55, 400}, {900, 200}} {
		w := adminDo(t, srv, session, "PUT", "/api/backup/schedule", map[string]int64{"interval_sec": tc.sec})
		if w.Code != tc.want {
			t.Errorf("interval %d: got %d, want %d: %s", tc.sec, w.Code, tc.want, w.Body.String())
		}
	}
	if status := statusOf(t, srv, session); status["interval_sec"] != float64(900) || status["min_interval_sec"] != float64(900) {
		t.Errorf("status schedule: %v", status)
	}
	rows := auditRows(t, st, "admin.backup_schedule")
	if len(rows) != 2 || rows[0].Details != "interval_sec=900" || rows[1].Details != "interval_sec=0" {
		t.Errorf("schedule audit: %+v", rows)
	}
}

func TestStatusNeverCarriesToken(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	priv, _ := recoverykey.Generate()
	ctx := context.Background()
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, backupSettings(ctx, st), recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	if err := storePairing(t, cfg, st, "https://recovery.busnes.app", "kyrec_live_secret"); err != nil {
		t.Fatal(err)
	}
	session := loginAs(t, srv, st, "alice", "admin")
	w := adminDo(t, srv, session, "GET", "/api/backup/status", nil)
	body := strings.ToLower(w.Body.String())
	if w.Code != http.StatusOK || strings.Contains(body, "kyrec_live_secret") || strings.Contains(body, "token") {
		t.Fatalf("status carries the credential: %d %s", w.Code, w.Body.String())
	}
	var status map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	if status["paired"] != true || status["key_pinned"] != true {
		t.Errorf("status %v", status)
	}
}

// A session without the CSRF header stops at the middleware. With no key pinned, the handler
// itself would answer 412, so 403 proves it never ran.
func TestExportCapsuleRequiresCSRF(t *testing.T) {
	srv, st, _ := setupSQLiteServer(t)
	req := httptest.NewRequest("POST", "/api/backup/export-capsule", nil)
	req.AddCookie(loginAs(t, srv, st, "alice", "admin"))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || w.Header().Get("Content-Disposition") != "" {
		t.Fatalf("export without CSRF: got %d %q", w.Code, w.Header().Get("Content-Disposition"))
	}
}

// The SPA catch-all may answer a wrong-method request, so the property pinned is that no
// method but POST produces a capsule attachment.
func TestExportCapsuleOnlyPOST(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	priv, _ := recoverykey.Generate()
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, backupSettings(context.Background(), st), recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	session := loginAs(t, srv, st, "alice", "admin")
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		w := adminDo(t, srv, session, method, "/api/backup/export-capsule", nil)
		if w.Header().Get("Content-Disposition") != "" || w.Header().Get("X-Recovery-Key-ID") != "" || w.Header().Get("Content-Type") == "application/octet-stream" {
			t.Errorf("%s produced a capsule: %d %v", method, w.Code, w.Header())
		}
	}
	w := adminPost(t, srv, session, "/api/backup/export-capsule")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("POST export: got %d %v", w.Code, w.Header())
	}
	// A capsule that left the server is a copy of everything it holds: the trail names it.
	m, err := capsule.ReadUnverifiedManifest(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	rows := auditRows(t, st, "admin.backup_export")
	if len(rows) != 1 || rows[0].Resource != m.CapsuleID || rows[0].UserID != "usr_alice" ||
		!strings.Contains(rows[0].Details, fmt.Sprintf("size_bytes=%q", fmt.Sprint(w.Body.Len()))) {
		t.Errorf("export audit: %+v", rows)
	}
}

func TestDrillReportsBusyAndRunsDecodedChecks(t *testing.T) {
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	srv, st, cfg := setupSQLiteServer(t)
	session := loginAs(t, srv, st, "drill-admin", "admin")
	f, err := os.OpenFile(filepath.Join(cfg.Database.DataDir, "drill.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	w := adminPost(t, srv, session, "/api/backup/drill")
	if w.Code != http.StatusConflict {
		t.Fatalf("busy: %d %s", w.Code, w.Body.String())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	w = adminPost(t, srv, session, "/api/backup/drill")
	if w.Code != http.StatusOK {
		t.Fatalf("drill: %d %s", w.Code, w.Body.String())
	}
	var result recoveryclient.DrillResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Passed {
		t.Fatalf("drill failed: %+v", result)
	}
	for _, name := range []string{"Required Files", "SQLite Integrity: data/ky_server.db", "Environment: KY_PORT", "Environment: KY_DB_DRIVER"} {
		found := false
		for _, check := range result.Checks {
			if check.Name == name && check.Passed {
				found = true
			}
		}
		if !found {
			t.Errorf("missing check %s", name)
		}
	}
}

// A stored KyRecovery URL that now resolves private is not a server fault: the run must name
// the switch that admits it, the way pairing does, rather than answer a bare 500.
func TestRunRefusesAPrivateDestination(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	ctx := context.Background()
	priv, _ := recoverykey.Generate()
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, backupSettings(ctx, st),
		recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	if err := storePairing(t, cfg, st, "https://recovery.busnes.app", "kyrec_live_t"); err != nil {
		t.Fatal(err)
	}
	// The lib wraps both sentinels on the dial path, so this pins the case order too: were the
	// 502 ErrRemote arm to come first, a private destination would report a remote refusal.
	api.SetRecoveryClientForTest(srv, &fakeDepositor{err: fmt.Errorf("%w: deposit request failed: %w",
		recoveryclient.ErrRemote,
		fmt.Errorf("recovery host resolves only to private or reserved addresses: %w", recoveryclient.ErrPrivateDestination))})

	w := adminPost(t, srv, loginAs(t, srv, st, "alice", "admin"), "/api/backup/deposit")
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("private destination: got %d, want 412: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "KY_BACKUP_ALLOW_PRIVATE_RECOVERY") {
		t.Errorf("body does not name the switch: %s", w.Body.String())
	}
}

// Only SQLite can be snapshotted into a capsule, so on Postgres "Back up now" can never
// succeed. It must say why -- the driver, in the body -- not answer a bare 500, which is what
// the README and the screen's standing warning promise.
func TestRunRefusesWithoutADatabaseSnapshot(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	ctx := context.Background()
	priv, _ := recoverykey.Generate()
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, backupSettings(ctx, st),
		recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	if err := storePairing(t, cfg, st, "https://recovery.busnes.app", "kyrec_live_t"); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDepositor{}
	api.SetRecoveryClientForTest(srv, fake)
	// The store stays SQLite so the fixture works; the collector reads the driver from the
	// config, which is what decides whether a snapshot is possible.
	cfg.Database.Driver = "postgres"

	w := adminPost(t, srv, loginAs(t, srv, st, "alice", "admin"), "/api/backup/deposit")
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("no database snapshot: got %d, want 412: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "snapshot") || !strings.Contains(w.Body.String(), "postgres") {
		t.Errorf("body does not name the snapshot refusal and the driver: %s", w.Body.String())
	}
	if fake.got != nil {
		t.Error("an instance that cannot snapshot its database sent bytes to the store")
	}
}

// brokenReceiptStore is the store with one write broken, the receipt row: the only way to
// reach the state where KyRecovery holds a capsule this side cannot record.
type brokenReceiptStore struct{ store.Store }

func (b brokenReceiptStore) Settings() store.SettingsStore {
	return brokenReceiptSettings{b.Store.Settings()}
}

type brokenReceiptSettings struct{ store.SettingsStore }

func (b brokenReceiptSettings) SetSetting(ctx context.Context, key, val string) error {
	if key == "kyrecovery_last_deposit" {
		return errors.New("settings write failed")
	}
	return b.SettingsStore.SetSetting(ctx, key, val)
}

// The screen keys its "deposited, but unrecorded" warning off this exact reply: 200, the
// result fields it reads, and receipt_unrecorded. A shape change here silently lies to the
// admin about what KyRecovery is holding.
func TestRunReportsAnUnrecordedReceipt(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	ctx := context.Background()
	priv, _ := recoverykey.Generate()
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, backupSettings(ctx, st),
		recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	if err := storePairing(t, cfg, st, "https://recovery.busnes.app", "kyrec_live_t"); err != nil {
		t.Fatal(err)
	}
	session := loginAs(t, srv, st, "alice", "admin")
	api.SetRecoveryClientForTest(srv, &fakeDepositor{})
	api.SetStoreForTest(srv, brokenReceiptStore{st})

	w := adminPost(t, srv, session, "/api/backup/deposit")
	if w.Code != http.StatusOK {
		t.Fatalf("unrecorded receipt: got %d, want 200: %s", w.Code, w.Body.String())
	}
	var out struct {
		recoveryclient.Result
		ReceiptUnrecorded bool `json:"receipt_unrecorded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.ReceiptUnrecorded {
		t.Errorf("reply does not carry receipt_unrecorded: %s", w.Body.String())
	}
	if out.Manifest.CapsuleID == "" || out.Receipt == nil || out.Receipt.CapsuleID != out.Manifest.CapsuleID {
		t.Errorf("reply lacks the fields the screen reads: %s", w.Body.String())
	}
	// The capsule is at KyRecovery, so the run is audited a success with the cause attached.
	var audited bool
	for _, rec := range auditRows(t, st, "admin.backup_run") {
		if rec.Resource == out.Manifest.CapsuleID && strings.Contains(rec.Details, `outcome="success"`) &&
			strings.Contains(rec.Details, "receipt_unrecorded=") {
			audited = true
		}
	}
	if !audited {
		t.Error("no successful admin.backup_run row naming the unrecorded receipt")
	}
}

// cancellingPairer drops the admin's connection while the pairing is being claimed, the way
// a closed browser tab does.
type cancellingPairer struct {
	fakePairer
	cancel context.CancelFunc
}

func (c *cancellingPairer) ClaimPairing(ctx context.Context, serverURL, pairingCode, serviceName, appName string) (recoveryclient.PairingResult, error) {
	c.cancel()
	return c.fakePairer.ClaimPairing(ctx, serverURL, pairingCode, serviceName, appName)
}

// Pairing is write-once and irreversible: once KyRecovery has handed back the suite key the
// pin, the stored pairing and the audit row must all land even if the admin's connection is
// gone, or the instance is left half-paired with nothing on record.
func TestPairRemoteOutlivesTheRequest(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	ctx := context.Background()
	priv, _ := recoverykey.Generate()
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	api.SetRecoveryClientForTest(srv, &cancellingPairer{
		fakePairer: fakePairer{result: recoveryclient.PairingResult{APIToken: "kyrec_live_t",
			Key: recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}}},
		cancel: cancel,
	})
	session := loginAs(t, srv, st, "alice", "admin")

	body, _ := json.Marshal(map[string]string{"recovery_url": "https://recovery.busnes.app", "pairing_code": "123456"})
	req := httptest.NewRequest("POST", "/api/backup/pair-remote", bytes.NewReader(body)).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(session)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
	req.Header.Set(auth.HeaderCSRF, "test-csrf")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("pair: got %d: %s", w.Code, w.Body.String())
	}
	settings := backupSettings(ctx, st)
	key, err := recoveryclient.LoadRecoveryKey(cfg.Database.DataDir, settings)
	if err != nil {
		t.Fatalf("the key pin did not survive the dropped connection: %v", err)
	}
	if key.Public.ID() != priv.Public().ID() {
		t.Errorf("pinned key: got %s, want %s", key.Public.ID(), priv.Public().ID())
	}
	if !recoveryclient.HasPairing(settings) {
		t.Error("no pairing stored after the request went away")
	}
	var audited bool
	for _, rec := range auditRows(t, st, "backup.paired") {
		if rec.UserID == "usr_alice" && strings.Contains(rec.Details, "recovery_key_id="+priv.Public().ID()) {
			audited = true
		}
	}
	if !audited {
		t.Error("no admin-attributed backup.paired row after the request went away")
	}
}

// blockingPairer holds the pairing open until it is released, standing in for a claim still on
// the wire when SIGTERM arrives.
type blockingPairer struct {
	fakePairer
	entered chan struct{}
	release chan struct{}
}

func (b *blockingPairer) ClaimPairing(ctx context.Context, serverURL, pairingCode, serviceName, appName string) (recoveryclient.PairingResult, error) {
	close(b.entered)
	<-b.release
	return b.fakePairer.ClaimPairing(ctx, serverURL, pairingCode, serviceName, appName)
}

// The detached handlers keep writing after their connection is gone, and http.Server.Shutdown
// knows nothing about them. WaitDetached is what stands between them and the store closing, so
// it must not return while one is still running.
func TestWaitDetachedBlocksUntilAPairingFinishes(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	priv, _ := recoverykey.Generate()
	pairer := &blockingPairer{
		fakePairer: fakePairer{result: recoveryclient.PairingResult{APIToken: "kyrec_live_t",
			Key: recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}}},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	api.SetRecoveryClientForTest(srv, pairer)
	session := loginAs(t, srv, st, "alice", "admin")

	go func() {
		body, _ := json.Marshal(map[string]string{"recovery_url": "https://recovery.busnes.app", "pairing_code": "123456"})
		req := httptest.NewRequest("POST", "/api/backup/pair-remote", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(session)
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
		req.Header.Set(auth.HeaderCSRF, "test-csrf")
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-pairer.entered // the handler has detached and is inside the claim

	waited := make(chan struct{})
	go func() { defer close(waited); srv.WaitDetached() }()
	select {
	case <-waited:
		t.Fatal("WaitDetached returned while a pairing was still in flight; the store would close under it")
	case <-time.After(100 * time.Millisecond):
	}

	close(pairer.release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitDetached did not return after the pairing finished")
	}
	// The wait is only worth anything if the work it waited for actually landed.
	if _, err := recoveryclient.LoadRecoveryKey(cfg.Database.DataDir, backupSettings(context.Background(), st)); err != nil {
		t.Errorf("the pairing did not complete before WaitDetached returned: %v", err)
	}
}

// blockingBody is a request body that stalls mid-read, the way a slow client does.
type blockingBody struct {
	reading chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	<-b.release
	return 0, io.EOF
}

// Shutdown returns after its own timeout with slow requests still active, so a handler that
// registers only after decoding its body is invisible to WaitDetached: the counter reads zero,
// the wait returns and the store closes under a request that is about to pair. Registration has
// to happen before the first byte is read.
func TestDetachedHandlerRegistersBeforeReadingItsBody(t *testing.T) {
	srv, st, _ := setupSQLiteServer(t)
	session := loginAs(t, srv, st, "alice", "admin")
	body := &blockingBody{reading: make(chan struct{}), release: make(chan struct{})}

	go func() {
		req := httptest.NewRequest("POST", "/api/backup/pair-remote", body)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(session)
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
		req.Header.Set(auth.HeaderCSRF, "test-csrf")
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-body.reading // inside the handler, stalled on the body

	waited := make(chan struct{})
	go func() { defer close(waited); srv.WaitDetached() }()
	select {
	case <-waited:
		t.Fatal("WaitDetached returned while a handler was still reading its request; it registered too late")
	case <-time.After(100 * time.Millisecond):
	}
	close(body.release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitDetached did not return after the handler finished")
	}
}

// blockingSessions stalls the session lookup that requireAdmin does before the handler runs.
type blockingSessions struct {
	store.SessionStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSessions) GetSession(ctx context.Context, tokenHash string) (*store.Session, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.SessionStore.GetSession(ctx, tokenHash)
}

// blockingSessionStore is the store with only its session lookup stalled.
type blockingSessionStore struct {
	store.Store
	sessions store.SessionStore
}

func (b *blockingSessionStore) Sessions() store.SessionStore { return b.sessions }

// The routes are s.tracked(s.requireAdmin(s.handleX)), and requireAdmin authenticates against
// the store before the handler is ever called. ReadTimeout (15s) outlasts shutdownTimeout (5s),
// so a SIGTERM landing during that lookup finds a counter registration must already cover:
// otherwise WaitDetached sees zero, returns, and the store closes under a request that is about
// to enter handlePinKey and pin a key. Registration inside the handler is too late.
func TestDetachedRegistrationCoversTheAuthLookup(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	session := loginAs(t, srv, st, "alice", "admin")

	blocked := &blockingSessions{SessionStore: st.Sessions(), entered: make(chan struct{}), release: make(chan struct{})}
	stalled := api.NewServer(cfg, &blockingSessionStore{Store: st, sessions: blocked})

	go func() {
		body, _ := json.Marshal(map[string]string{"public_key": "", "threshold": ""})
		req := httptest.NewRequest("POST", "/api/backup/pin-key", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(session)
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
		req.Header.Set(auth.HeaderCSRF, "test-csrf")
		stalled.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-blocked.entered // inside requireAdmin's store round-trip, before the handler

	waited := make(chan struct{})
	go func() { defer close(waited); stalled.WaitDetached() }()
	select {
	case <-waited:
		t.Fatal("WaitDetached returned during the auth lookup; the store would close under a request about to pin a key")
	case <-time.After(100 * time.Millisecond):
	}

	close(blocked.release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitDetached did not return after the request finished")
	}
}

// A sync.WaitGroup panics with "sync: WaitGroup misuse" when Add from zero races an in-progress
// Wait, and Shutdown returning on its timeout is no barrier: two admin requests arriving at
// SIGTERM can do exactly this. The counter has to tolerate registration concurrent with the wait.
func TestDetachedRegistrationRacesWait(t *testing.T) {
	srv := &api.Server{} // the zero value has to work: this exercises the counter, nothing else
	release := api.RegisterDetachedForTest(srv)

	waited := make(chan struct{})
	go func() { defer close(waited); srv.WaitDetached() }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); release() }()                          // counter falls to zero
	go func() { defer wg.Done(); api.RegisterDetachedForTest(srv)() }() // ...as another registers
	wg.Wait()

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitDetached did not return once every registration had finished")
	}
}

// blockingDepositor stalls inside the upload, where a deposit spends nearly all of its life.
type blockingDepositor struct {
	fakeDepositor
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingDepositor) Deposit(ctx context.Context, url, token string, container []byte) (recoveryclient.Receipt, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.fakeDepositor.Deposit(ctx, url, token, container)
}

// A deposit is the longest-running detached write in the system, and the only thing that keeps
// it visible to WaitDetached is the s.tracked( wrapper on its route: the handler no longer
// registers itself. Without this test that one route line can be deleted and the suite stays
// green, while a SIGTERM mid-upload closes the store under the run and leaves KyRecovery holding
// a capsule this instance has no receipt for.
func TestDetachedTrackingCoversADepositInFlight(t *testing.T) {
	srv, st, cfg := setupSQLiteServer(t)
	ctx := context.Background()
	priv, _ := recoverykey.Generate()
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, backupSettings(ctx, st), recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	if err := storePairing(t, cfg, st, "https://recovery.busnes.app", "kyrec_live_t"); err != nil {
		t.Fatal(err)
	}
	depositor := &blockingDepositor{entered: make(chan struct{}), release: make(chan struct{})}
	api.SetRecoveryClientForTest(srv, depositor)
	session := loginAs(t, srv, st, "alice", "admin")

	go func() {
		req := httptest.NewRequest("POST", "/api/backup/deposit", nil)
		req.AddCookie(session)
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
		req.Header.Set(auth.HeaderCSRF, "test-csrf")
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-depositor.entered // the capsule is on its way to KyRecovery

	waited := make(chan struct{})
	go func() { defer close(waited); srv.WaitDetached() }()
	select {
	case <-waited:
		t.Fatal("WaitDetached returned with a deposit still uploading; the store would close before the receipt was written")
	case <-time.After(100 * time.Millisecond):
	}

	close(depositor.release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitDetached did not return after the deposit finished")
	}
	if _, ok, _ := recoveryclient.LastDeposit(backupSettings(ctx, st)); !ok {
		t.Error("the wait returned but no receipt was recorded: it did not cover the write it exists for")
	}
}
