# Scaffold Wires ky-primitives `recoveryclient` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring ky_server_base to the KySignOn backup spec by replacing `internal/backup`'s copied code with the `github.com/Busness-app/ky-primitives/recoveryclient` package, then adding the product work the spec leaves per product: routes, screen, compose, runbook, docs.

**Architecture:** `internal/backup` shrinks to what is scaffold-specific: payload collection (SQLite `VACUUM INTO` snapshot, keys, config manifest), the drill's SQLite checks, a `Settings` adapter over `store.SettingsStore`, and a `Sealer` built from the deployment key. Handlers in `internal/api/backup_handlers.go` call the lib. `cmd/server/main.go` gets a minute-polling `backupLoop` and a ten-line `restore`. The old plaintext `KY_BACKUP_DIR` local backup is replaced by sealed local copies under the same env var.

**Tech Stack:** Go 1.26, `ky-primitives v0.5.0` (recoveryclient package), React/TypeScript web in `web/`, Docker Compose.

**Spec:** `docs/superpowers/plans/2026-09-04-bring-suite-to-kysignon-spec.md` (fourteen rows) and myslop folder `ky-server-base-sealed-token`, post "Full hand-off: bring to KySignOn spec (after the ky-primitives package lands)". The lib is built on ky-primitives branch `feat/recoveryclient`, PR #12 (harrier, 2026-09-04): package `recoveryclient`, with `SQLiteSnapshot` added on Yoshi's instruction. The API this plan names was read from that branch.

## Global Constraints

- **Blocked until** ky-primitives PR #12 merges and `v0.5.0` is tagged. Do not start a new copy of `internal/backup`.
- Lib facts that shape this plan: `Drill(ctx, scratchRoot, payload, checks)` refuses an empty scratch root and wants it inside the data directory (`ErrNoScratchRoot`); `WriteLocalCopy` refuses `Keep < 1` (`ErrBadKeep`); `SQLiteSnapshot(ctx, db, destPath)` runs `VACUUM INTO` on a `*sql.DB` and wants a non-existent dest; `guardtest.MinFiles` is 10.
- **Claim** myslop folder `ky-server-base-sealed-token` before starting; post there when done.
- Env vars: `KY_BACKUP_DIR` (sealed copies, default empty = off), `KY_BACKUP_KEEP` (default 7), `KY_BACKUP_DEPOSIT_INTERVAL` (default `24h`, default only), `KY_BACKUP_ALLOW_PRIVATE_RECOVERY` (default false; admits RFC1918 and CGNAT only, while loopback, link-local, unspecified, multicast, and all other reserved ranges remain refused), `KY_DNS` (compose override only). Retention may delete only the exact capsule filename prefix this application writes and must leave every foreign or legacy file untouched.
- Routes keep the scaffold's prefix `/api/backup/...` (no `/admin`); register with Go 1.22 method patterns so only the declared method can reach each handler. The unscoped SPA catch-all may serve a wrong-method request, so tests assert the backup handler did not run rather than assuming a 405.
- Step-up: the scaffold has none. Spec row 10 says "or the product's equivalent": every backup route is admin-only and listed in `TestPrivilegedEndpointsRequireAdmin`. Say so in `internal/api/AGENTS.md`. (Assumption; flag in the PR.)
- Sealer label: `ky_server_base:setting:kyrecovery_token` (unchanged from #19). Nothing is in the wild; no migration of stored ciphertext. Delete dev data dirs before the live check.
- Audit `Details` is a string bounded to 255 bytes (Postgres column). Convert the lib's `details map[string]any` with `auditDetails` (Task 4), never `fmt.Sprint`.
- Never HTML-escape a value inside an inline `on*=` handler; React props are fine, `dangerouslySetInnerHTML` is not.
- `make ci` green before every commit that touches Go; `npm test` in `web/` before every commit that touches the screen.
- Postgres CI job: keep deposit tests on `setupSQLiteServer`.

---

## File map

```
internal/backup/
  settings.go     settingsAdapter: store.SettingsStore(+ctx) -> recoveryclient.Settings; NewSealer(cfg)
  payload.go      Collect(cfg, appVersion) (recoveryclient.Payload, error)   [from client.go BuildLocalPayload+AppendSealedOnlyFiles+CollectSealable]
  drill.go        Checks(cfg) func(dir string) []recoveryclient.Check           [SQLite + required files + env checks, from RunRestoreDrill]
  nodecrypt_test.go  one call to guardtest.NoDecryptOutside
  AGENTS.md       local contracts rewritten
  DELETE: client.go, deposit.go, recoverykey.go, capsule.go, and their tests
internal/store/store.go, sqlstore.go   DeleteSetting added to SettingsStore
internal/config/config.go              BackupConfig{Dir, Keep, DepositInterval, AllowPrivateRecovery}; StorageDir/AutoDrillDay removed
internal/api/backup_handlers.go        drill, export-capsule, pair-remote, deposit(run), pairing DELETE, pin-key, schedule, status
internal/api/server.go                 route table with methods
internal/api/settings_handlers.go      unchanged (already redacts the token)
internal/api/authz_test.go             new routes in TestPrivilegedEndpointsRequireAdmin
cmd/server/main.go                     backupLoop, runDeposit -> recoveryclient.Run, restore -> recoveryclient.Restore
web/src/pages/Backup.tsx               rewritten from kysignon AdminBackup.tsx
web/src/styles/...                     .dr-* classes
docker-compose.yml, docker-compose.lan-dns.yml, Dockerfile, scripts/smoke-test.sh
docs/RESTORE.md, README.md (new), AGENTS.md
```

---

### Task 1: Bump the lib, add `DeleteSetting`, config

**Files:**
- Modify: `go.mod`, `internal/store/store.go:85-90`, `internal/store/sqlstore.go` (settingsStore methods near line 784), `internal/config/config.go:82-89,143-149,199-203`
- Test: `internal/store/sqlstore_test.go` (existing settings tests), `internal/config/config_test.go`

**Interfaces:**
- Produces: `SettingsStore.DeleteSetting(ctx, key) error` (a never-written key is not an error); `config.BackupConfig{ Dir string; Keep int; DepositInterval time.Duration; AllowPrivateRecovery bool }`.

- [ ] **Step 1: Failing tests**

```go
// internal/store/sqlstore_test.go
func TestDeleteSettingIsIdempotent(t *testing.T) {
	st := openTestStore(t) // whatever helper the file already uses
	ctx := context.Background()
	if err := st.Settings().DeleteSetting(ctx, "never"); err != nil {
		t.Fatal(err)
	}
	_ = st.Settings().SetSetting(ctx, "k", "v")
	_ = st.Settings().DeleteSetting(ctx, "k")
	if _, err := st.Settings().GetSetting(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// internal/config/config_test.go
func TestBackupConfigFromEnv(t *testing.T) {
	t.Setenv("KY_BACKUP_DIR", "/tmp/x")
	t.Setenv("KY_BACKUP_KEEP", "3")
	t.Setenv("KY_BACKUP_ALLOW_PRIVATE_RECOVERY", "true")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Dir != "/tmp/x" || cfg.Backup.Keep != 3 || !cfg.Backup.AllowPrivateRecovery {
		t.Fatalf("%+v", cfg.Backup)
	}
}

func TestBackupKeepBelowOneIsRefused(t *testing.T) {
	t.Setenv("KY_BACKUP_KEEP", "0")
	if _, err := LoadFromEnv(); err == nil || !strings.Contains(err.Error(), "KY_BACKUP_KEEP") {
		t.Fatalf("want KY_BACKUP_KEEP error, got %v", err)
	}
}
```

- [ ] **Step 2: Run, see them fail**

Run: `go test ./internal/store/ -run DeleteSetting; go test ./internal/config/ -run BackupConfig`
Expected: FAIL (undefined method / field)

- [ ] **Step 3: Implement**

```bash
go get github.com/Busness-app/ky-primitives@v0.5.0 && go mod tidy
```

`store.go`: add `DeleteSetting(ctx context.Context, key string) error` to `SettingsStore`. `sqlstore.go`, beside `SetSetting`:

```go
func (s *settingsStore) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.store.db.ExecContext(ctx, s.store.rebind(`DELETE FROM server_settings WHERE key = ?`), key)
	return err
}
```

(Same table and `rebind` style as `SetSetting` at `sqlstore.go:797`; check how it reaches the `*sql.DB`.)

`config.go`: replace `BackupConfig` with the four fields above. In `LoadFromEnv`: `Dir: getEnv("KY_BACKUP_DIR", "")`, `Keep: getEnvInt("KY_BACKUP_KEEP", 7)`, `AllowPrivateRecovery: getEnvBool("KY_BACKUP_ALLOW_PRIVATE_RECOVERY", false)`; after parsing, `if keep < 1 { return nil, fmt.Errorf("KY_BACKUP_KEEP: must be at least 1, got %d", keep) }` (the lib refuses `Keep < 1` at write time; fail at startup instead of at the first backup); keep the `DepositInterval` parsing and the `MinDepositInterval` check; delete `StorageDir` and `AutoDrillDay`. Log at startup in `main.go` when `AllowPrivateRecovery` is set: `log.Printf("[BACKUP] KY_BACKUP_ALLOW_PRIVATE_RECOVERY is on: RFC1918 and CGNAT destinations admitted; loopback, link-local and other reserved addresses remain refused (HTTPS still required)")`.

- [ ] **Step 4: Build, test, commit**

Run: `go build ./... ; go test ./internal/store/ ./internal/config/`
Expected: `go build` fails in `internal/backup` (old code references `StorageDir`); that is Task 2. The two package tests PASS.

```bash
git add go.mod go.sum internal/store internal/config
git commit -m "config+store: backup dir/keep/private-recovery settings, DeleteSetting"
```

---

### Task 2: Replace `internal/backup` with adapters over the lib

**Files:**
- Create: `internal/backup/settings.go`, `internal/backup/settings_test.go`
- Rewrite: `internal/backup/payload.go` (from `client.go:294-417`), `internal/backup/drill.go`
- Delete: `internal/backup/client.go`, `client_test.go`, `deposit.go`, `deposit_test.go`, `recoverykey.go`, `recoverykey_test.go`, `capsule.go`, `capsule_test.go`, `export_test.go`
- Keep and adjust: `payload_test.go` (rename `BuildLocalPayload` cases to `Collect`), `nodecrypt_test.go` (Task 6)

**Interfaces:**
- Produces:

```go
// settings.go
func Settings(ctx context.Context, s store.SettingsStore) recoveryclient.Settings
func NewSealer(cfg *config.Config) (recoveryclient.Sealer, error)   // label ky_server_base:setting:kyrecovery_token
func RunConfig(cfg *config.Config, appVersion string) (recoveryclient.RunConfig, error)
// payload.go
var ErrNoDatabaseSnapshot error
func Collect(ctx context.Context, cfg *config.Config, appVersion string) (recoveryclient.Payload, error)
// drill.go
func Checks(cfg *config.Config, payload recoveryclient.Payload) func(dir string) []recoveryclient.Check
func DrillRoot(cfg *config.Config) string   // filepath.Join(cfg.Database.DataDir, "drill")
```

- [ ] **Step 1: Failing adapter test**

```go
func TestSettingsAdapterMapsNotFound(t *testing.T) {
	st := setupSQLiteStore(t) // helper used by deposit_test.go today; move it to settings_test.go
	s := Settings(context.Background(), st.Settings())
	if _, err := s.Get("nope"); !errors.Is(err, recoveryclient.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	_ = s.Set("a", "1")
	if v, _ := s.Get("a"); v != "1" {
		t.Fatal("set/get")
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("a"); !errors.Is(err, recoveryclient.ErrNotFound) {
		t.Fatal("delete")
	}
}

func TestSealerRoundTripUnderDeploymentKey(t *testing.T) {
	cfg := &config.Config{}
	cfg.Security.EncryptionKey = bytes.Repeat([]byte{1}, 32)
	s, err := NewSealer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Seal([]byte("tok"))
	p, err := s.Open(c)
	if err != nil || string(p) != "tok" {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run to see failure**

Run: `go test ./internal/backup/ -run 'Adapter|Sealer'`
Expected: FAIL

- [ ] **Step 3: Write settings.go**

```go
package backup

import (
	"context"
	"errors"

	"github.com/Busness-app/ky-primitives/recoveryclient"
	"github.com/Busness-app/ky_server_base/internal/config"
	"github.com/Busness-app/ky_server_base/internal/store"
)

const recoveryTokenLabel = "ky_server_base:setting:kyrecovery_token"

type settingsAdapter struct {
	ctx context.Context
	s   store.SettingsStore
}

// Settings binds the request context to the settings store for one call into the lib.
func Settings(ctx context.Context, s store.SettingsStore) recoveryclient.Settings {
	return settingsAdapter{ctx: ctx, s: s}
}

func (a settingsAdapter) Get(key string) (string, error) {
	v, err := a.s.GetSetting(a.ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", recoveryclient.ErrNotFound
	}
	return v, err
}
func (a settingsAdapter) Set(key, val string) error { return a.s.SetSetting(a.ctx, key, val) }
func (a settingsAdapter) Delete(key string) error   { return a.s.DeleteSetting(a.ctx, key) }

// NewSealer seals the KyRecovery token under the deployment key, domain-separated so a row
// copied from another setting will not open.
func NewSealer(cfg *config.Config) (recoveryclient.Sealer, error) {
	return recoveryclient.NewAESGCMSealer(cfg.Security.EncryptionKey, recoveryTokenLabel)
}

func RunConfig(cfg *config.Config, appVersion string) (recoveryclient.RunConfig, error) {
	sealer, err := NewSealer(cfg)
	if err != nil {
		return recoveryclient.RunConfig{}, err
	}
	return recoveryclient.RunConfig{
		DataDir: cfg.Database.DataDir, AppName: cfg.Server.AppName, AppVersion: appVersion,
		BackupDir: cfg.Backup.Dir, Keep: cfg.Backup.Keep, Sealer: sealer,
	}, nil
}
```

- [ ] **Step 4: Write payload.go from client.go**

Move `ErrNoDatabaseSnapshot`, `BuildLocalPayload`, `requiredFiles`, `AppendSealedOnlyFiles`, `CollectSealable` into `payload.go`. Collapse them: `Collect(ctx, cfg, appVersion) (recoveryclient.Payload, error)` = `BuildLocalPayload` then `AppendSealedOnlyFiles`, returning a value not a pointer; `BackupFile` → `recoveryclient.File`; drop the `Payload` type (use `recoveryclient.Payload`); `ServiceName` must be `cfg.Server.AppName`. Delete `BuildLocalPayload` as a name (the plaintext local backup is gone; sealed copies replace it).

Replace `snapshotSQLite` with the lib's snapshot; the scaffold keeps opening its own handle from the DSN because `store.Store` exposes no `*sql.DB`:

```go
func snapshotSQLite(ctx context.Context, dsn, dataDir string) ([]byte, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	dir, err := os.MkdirTemp(dataDir, "snapshot-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "ky_server.db")
	if err := recoveryclient.SQLiteSnapshot(ctx, db, path); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoDatabaseSnapshot, err)
	}
	return os.ReadFile(path)
}
```

Keep the existing `payload_test.go` case that commits a row and snapshots without checkpointing (the lib's README says the row-in-the-WAL proof lives in the product's tests); if there is none, add `TestSnapshotSeesUncheckpointedCommit`: open the store, insert a setting, call `Collect`, open the snapshot bytes from a temp file with `sql.Open("sqlite", ...)`, and read the setting back.

Update `payload_test.go` for the new names.

- [ ] **Step 5: Write drill.go**

Keep the check code from `RunRestoreDrill` lines 100–195 (required files, SQLite integrity, environment) and `drillPath`; wrap as:

```go
// Checks are the scaffold's assertions on an opened capsule: every required member is
// present, each SQLite member passes integrity_check, and the recipe's environment names
// are set. They see only the scratch directory the lib opened the capsule into.
func Checks(cfg *config.Config, payload recoveryclient.Payload) func(dir string) []recoveryclient.Check {
	return func(dir string) []recoveryclient.Check { /* the moved code, appending recoveryclient.Check */ }
}

// DrillRoot is where Drill opens capsules: under the data directory, never the system temp
// dir, because the opened payload is the whole instance in the clear. The lib creates and
// wipes a 0700 subdirectory per drill and sweeps stale ones.
func DrillRoot(cfg *config.Config) string { return filepath.Join(cfg.Database.DataDir, "drill") }
```

Delete `RunRestoreDrill`, `CheckItem`, `DrillResult` (use `recoveryclient.Check`, `recoveryclient.DrillResult`). Add a test that `Checks` fails on a scratch dir missing the database member, and one that the drill root is under the data dir.

- [ ] **Step 6: Delete the lifted files, fix tests**

```bash
git rm internal/backup/client.go internal/backup/client_test.go internal/backup/deposit.go internal/backup/deposit_test.go internal/backup/recoverykey.go internal/backup/recoverykey_test.go internal/backup/capsule.go internal/backup/capsule_test.go internal/backup/export_test.go
```

Before this deletion, add adapter-level regression tests for the library client: reject DNS answers in private, loopback, CGNAT and other reserved ranges at dial time when the opt-in is off; continue rejecting loopback and link-local when it is on; refuse HTTP 307/308 instead of following them; and reject non-HTTPS or credentialed URLs. If the selected library version cannot pass, keep a thin transport here and inject it into `NewClient` until the library owns equivalent checks.

Move `setupSQLiteStore` (or whatever `deposit_test.go` calls it) into `settings_test.go` first. The behaviour those deleted tests pinned now lives in the lib's tests.

- [ ] **Step 7: Build the package, commit**

Run: `go build ./internal/backup/ && go test ./internal/backup/`
Expected: PASS (api and cmd still fail to build until Tasks 3–5)

```bash
git add internal/backup
git commit -m "backup: adapters over ky-primitives/recoveryclient; drop copied client, deposit, pin, capsule"
```

---

### Task 3: Handlers and routes

**Files:**
- Rewrite: `internal/api/backup_handlers.go`
- Modify: `internal/api/server.go:25-26,71,158-161`
- Test: `internal/api/backup_test.go` (new; port the shape of kysignon `backup_api_test.go`), `internal/api/authz_test.go:127-140`

**Interfaces:**
- Consumes: Task 2 functions, `recoveryclient.{NewClient,Options,Client,ParsePinRequest,StoreRecoveryKey,LoadRecoveryKey,StorePairing,ClearPairing,HasPairing,LastDeposit,ListLocalCopies,Interval,SetInterval,NextRun,Run,Outcome,Drill,Seal,FilenameSafe,AuditSafe,TooLargeMessage,ErrNotPaired,ErrKeyMismatch,ErrKeyPinMissing,ErrNoDestination,ErrInProgress,ErrRemote,ErrReceiptUnrecorded,ErrBadInterval,MinInterval}`.
- Produces routes:

| Method | Path | Handler | Response |
|---|---|---|---|
| POST | /api/backup/drill | handleBackupDrill | `recoveryclient.DrillResult` |
| POST | /api/backup/export-capsule | handleExportCapsule | CSRF-protected `.kycap` attachment |
| POST | /api/backup/pair-remote | handlePairRemoteRecovery | `{recovery_key_id, threshold, total_shares}` |
| POST | /api/backup/deposit | handleRunBackup | `recoveryclient.Result` |
| DELETE | /api/backup/pairing | handleUnpair | `{paired:false}` |
| POST | /api/backup/pin-key | handlePinKey | `{recovery_key_id, threshold, total_shares}` |
| PUT | /api/backup/schedule | handleSetSchedule | `{interval_sec}` |
| GET | /api/backup/status | handleBackupStatus | status object below |

- [ ] **Step 1: Failing handler tests**

Port kysignon `backup_api_test.go` cases against `setupSQLiteServer` (SQLite only, per the Postgres CI note). Required names:

```go
func TestPinKeyIsWriteOnce(t *testing.T)                 // second pin with a different key: 409; same key: 200
func TestPinKeyBadTopology(t *testing.T)                 // 1-of-3: 400
func TestRunWithPinnedKeyAndNoDestination(t *testing.T)  // 412, body error names "pair with KyRecovery or set KY_BACKUP_DIR"
func TestRunWritesLocalCopy0600(t *testing.T)            // KY_BACKUP_DIR set in cfg, pin key, POST deposit → 200, file <AppName>-<id>.kycap mode 0600, audit row admin.backup_run success
func TestUnpairKeepsPin(t *testing.T)                    // pair (fake client), DELETE pairing → 200; status key_pinned true, paired false; DELETE again → 412
func TestScheduleBounds(t *testing.T)                    // 0 ok, 899 → 400, 1<<55 → 400, 900 ok; audit row reads back interval_sec=900
func TestStatusNeverCarriesToken(t *testing.T)           // body has no kyrecovery_token_enc / token substring
func TestExportCapsuleRequiresCSRF(t *testing.T)         // session without CSRF is 403 and no snapshot is made
func TestExportCapsuleOnlyPOST(t *testing.T)             // wrong methods never produce a capsule attachment or export audit row
```

Add to `TestPrivilegedEndpointsRequireAdmin` cases: `{"DELETE","/api/backup/pairing"}`, `{"POST","/api/backup/pin-key"}`, `{"PUT","/api/backup/schedule"}`, `{"GET","/api/backup/status"}`.

- [ ] **Step 2: Run to see failure**

Run: `go test ./internal/api/ -run 'PinKey|RunWith|LocalCopy|Unpair|Schedule|Status|ExportCapsule|Privileged'`
Expected: FAIL (build errors)

- [ ] **Step 3: Rewrite backup_handlers.go**

Shape (every handler follows kysignon's; the mapping is mechanical):

```go
type recoveryClient interface {
	ClaimPairing(ctx context.Context, serverURL, pairingCode, serviceName, appName string) (recoveryclient.PairingResult, error)
	recoveryclient.Depositor
}

// auditDetails flattens the lib's map into the 255-byte audit column, stable key order.
func auditDetails(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m { keys = append(keys, k) }
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if b.Len() > 0 { b.WriteByte(' ') }
		fmt.Fprintf(&b, "%s=%v", k, m[k])
	}
	return recoveryclient.AuditSafe(b.String())
}
```

- `handleBackupDrill`: `payload, err := backup.Collect(ctx, s.config, appVersion)`; `ErrNoDatabaseSnapshot` → 200 with a failed `Check` as today; `recoveryclient.Drill(ctx, backup.DrillRoot(s.config), payload, backup.Checks(s.config, payload))`.
- `handleExportCapsule`: `LoadRecoveryKey(dataDir, backup.Settings(ctx, st))`; `ErrNotPaired` → 412 "No recovery key; pair or pin one"; `ErrKeyMismatch` → 409; `recoveryclient.Seal(payload, key)`; `capsule.ErrCapsuleTooLarge` → 413 with `recoveryclient.TooLargeMessage`.
- `handlePairRemoteRecovery`: `recoveryclient.ValidateURL(req.RecoveryURL, s.config.Backup.AllowPrivateRecovery)` first (400 naming `KY_BACKUP_ALLOW_PRIVATE_RECOVERY` when the lib's error says private), with an adapter check if needed so the opt-in admits only RFC1918 and CGNAT and never loopback, link-local, or other reserved ranges; `ClaimPairing(ctx, url, code, cfg.Server.AppName, cfg.Server.AppName)`; `StoreRecoveryKey` (`fs.ErrExist` → 409); `StorePairing(settings, sealer, url, token)`; audit `backup.paired` with `details = "recovery_key_id=<id> allow_private=<bool>"`.
- `handleRunBackup` (route stays `/api/backup/deposit`): `rc, _ := backup.RunConfig(cfg, appVersion)`; `res, err := recoveryclient.Run(ctx, rc, settings, func() (recoveryclient.Payload, error) { return backup.Collect(ctx, cfg, appVersion) }, s.recovery)`; `action, outcome, details := recoveryclient.Outcome(res, err)`; audit `action` with resource `res.Manifest.CapsuleID` and `auditDetails(details)` plus `outcome`; status mapping: `ErrReceiptUnrecorded` → 200 with `receipt_unrecorded` field; `ErrKeyPinMissing` → 412 "Paired, but recovery.pub is missing or does not match the pin"; `ErrNotPaired` → 412 "No recovery key"; `ErrNoDestination` → 412 "Nowhere to put a capsule: pair with KyRecovery or set KY_BACKUP_DIR"; `ErrInProgress` → 409; `ErrKeyMismatch` → 409; too large → 413; `ErrRemote` → 502; else 500. Every remote error text logged through `AuditSafe`.
- `handleUnpair`, `handlePinKey`, `handleSetSchedule`, `handleBackupStatus`: copy kysignon's bodies, `h.store` → `backup.Settings(r.Context(), s.store.Settings())`, `h.cfg.DataDir` → `s.config.Database.DataDir`, `backup.X` → `recoveryclient.X`, `h.record(...)` → `s.auditBackup(...)`, `config.MinBackupDepositInterval` → `recoveryclient.MinInterval`, `backup.Interval(h.cfg, h.store)` → `recoveryclient.Interval(s.config.Backup.DepositInterval, settings)`. `PinKey` uses `recoveryclient.ParsePinRequest(req.PublicKey, req.Threshold, req.TotalShares)` (400 on error) then `StoreRecoveryKey`. `Status` fields: `paired, key_pinned, app_name, app_version, recovery_url, recovery_key_id, threshold, total_shares, recovery_key_error, last_deposit, local_dir, local_keep, local_copies, local_error, interval_sec, min_interval_sec, next_run_at, allow_private_recovery`.

`server.go`: `recovery := recoveryclient.NewClient(recoveryclient.Options{AllowPrivate: cfg.Backup.AllowPrivateRecovery})`; the `recoveryClient` interface above replaces the inline one at line 25. Route table:

```go
s.mux.HandleFunc("POST /api/backup/drill", s.requireAdmin(s.handleBackupDrill))
s.mux.HandleFunc("POST /api/backup/export-capsule", s.requireAdmin(s.handleExportCapsule))
s.mux.HandleFunc("POST /api/backup/pair-remote", s.requireAdmin(s.handlePairRemoteRecovery))
s.mux.HandleFunc("POST /api/backup/deposit", s.requireAdmin(s.handleRunBackup))
s.mux.HandleFunc("DELETE /api/backup/pairing", s.requireAdmin(s.handleUnpair))
s.mux.HandleFunc("POST /api/backup/pin-key", s.requireAdmin(s.handlePinKey))
s.mux.HandleFunc("PUT /api/backup/schedule", s.requireAdmin(s.handleSetSchedule))
s.mux.HandleFunc("GET /api/backup/status", s.requireAdmin(s.handleBackupStatus))
```

The frontend calls export with `secureFetch`, so POST carries the CSRF header. Because `s.mux.Handle("/", web.Handler())` is an any-method catch-all, a wrong method can reach the SPA instead of producing 405. Pin the security property directly: it must not reach `handleExportCapsule`, return a capsule attachment, or write an export audit row.

- [ ] **Step 4: Run, commit**

Run: `go test ./internal/api/`
Expected: PASS

```bash
git add internal/api
git commit -m "api: backup routes to spec: pin-key, schedule, unpair, status; Run replaces deposit"
```

---

### Task 4: `cmd/server/main.go`: loop, deposit CLI, restore

**Files:**
- Modify: `cmd/server/main.go:100-102,130-200,238-330,321-420`
- Test: `cmd/server/restore_test.go` (existing; retarget to `recoveryclient.Restore` via the `restore` wrapper)

- [ ] **Step 1: Replace `depositLoop` with `backupLoop`**

Start it unconditionally (`go backupLoop(ctx, cfg, st)`; the schedule is a setting now):

```go
// backupLoop polls the admin's schedule once a minute; a change in the UI needs no restart
// and a restart never loses its place, the last attempt is in the database. The wait honours
// shutdown; the run does not, so SIGTERM cannot land between KyRecovery storing a capsule
// and the receipt being written.
func backupLoop(ctx context.Context, cfg *config.Config, st store.Store) {
	client := recoveryclient.NewClient(recoveryclient.Options{AllowPrivate: cfg.Backup.AllowPrivateRecovery})
	rc, err := backup.RunConfig(cfg, appVersion)
	if err != nil {
		log.Printf("[BACKUP] scheduler disabled: %v", err)
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		settings := backup.Settings(ctx, st.Settings())
		next, on, err := recoveryclient.NextRun(cfg.Backup.DepositInterval, settings)
		if err != nil {
			log.Printf("[BACKUP] schedule unreadable: %s", recoveryclient.AuditSafe(err.Error()))
			continue
		}
		if !on || time.Now().Before(next) {
			continue
		}
		runCtx := context.WithoutCancel(ctx)
		res, err := recoveryclient.Run(runCtx, rc, backup.Settings(runCtx, st.Settings()),
			func() (recoveryclient.Payload, error) { return backup.Collect(runCtx, cfg, appVersion) }, client)
		if errors.Is(err, recoveryclient.ErrNotPaired) || errors.Is(err, recoveryclient.ErrNoDestination) {
			continue
		}
		recordRun(runCtx, st, "system", res, err)
	}
}

func recordRun(ctx context.Context, st store.Store, actor string, res recoveryclient.Result, err error) {
	action, outcome, details := recoveryclient.Outcome(res, err)
	details["outcome"] = outcome
	_ = st.Audit().LogAudit(ctx, &store.AuditRecord{UserID: actor, Action: action,
		Resource: res.Manifest.CapsuleID, Details: api.AuditDetails(details)})
	if err != nil {
		log.Printf("[BACKUP] %s: %s", actor, recoveryclient.AuditSafe(err.Error()))
		return
	}
	log.Printf("[BACKUP] %s: capsule %s (%d bytes) local=%q deposited=%t", actor, res.Manifest.CapsuleID, res.SizeBytes, res.LocalPath, res.Receipt != nil)
}
```

Export `auditDetails` from Task 3 as `api.AuditDetails` so both callers share it. `runDeposit` (the `deposit` subcommand) calls the same `Run` + `recordRun(ctx, st, "cli", ...)`.

- [ ] **Step 2: Restore becomes ten lines**

```go
func restore(capsulePath, targetDir, expectService string, shares []string, stdout io.Writer) error {
	return recoveryclient.Restore(capsulePath, targetDir, expectService, shares, stdout)
}
```

`runRestore` keeps its flags; shares via `recoveryclient.ReadShares(os.Stdin)`; default service `os.Getenv("KY_APP_NAME")` then `config.DefaultAppName`. `runBackupDrill` uses `recoveryclient.Drill(ctx, backup.DrillRoot(cfg), payload, backup.Checks(cfg, payload))` and prints the checks; drop `loadRecoveryKey` there.

- [ ] **Step 3: Build, run tests, commit**

Run: `go build ./... && go test ./cmd/... && make ci`
Expected: PASS except `nodecrypt_test.go` (Task 6)

```bash
git add cmd/server internal/api
git commit -m "server: minute-polling backup loop, restore via recoveryclient"
```

---

### Task 5: Screen

**Files:**
- Rewrite: `web/src/pages/Backup.tsx` (236 lines today) from `kysignon-server/web/src/components/AdminBackup.tsx` (586 lines)
- Modify: `web/src/styles/` (copy the `.dr-*` rules from kysignon `index.css`)
- Test: `web/src/pages/Backup.test.tsx` (new; whatever runner `web/package.json` uses)

- [ ] **Step 1: Failing render test**

```tsx
it('warns when a key is pinned but there is no destination', async () => {
  mockFetch({ '/api/backup/status': { key_pinned: true, paired: false, interval_sec: 0 } });
  render(<Backup />);
  expect(await screen.findByText(/nowhere to put a capsule/i)).toBeInTheDocument();
  expect(screen.getByText(/schedule is off/i)).toBeInTheDocument();
});
it('never renders the token', ...)  // status includes recovery_url only; assert no "token" text
```

- [ ] **Step 2: Port the component**

Copy `AdminBackup.tsx`, then: `secureFetch` from `../api`, paths `/api/admin/backup/*` → `/api/backup/*`, no step-up prompt (there is none), `KYSIGNON_BACKUP_DIR` → `KY_BACKUP_DIR`. Keep the four fact cards (key, KyRecovery, local copies, schedule), one action row (Back up now, Download capsule, Run drill), "what a capsule carries", schedule form (minutes → `interval_sec`, off = 0, min from `min_interval_sec`), pairing panel with Unpair, key-by-hand panel; warnings for no key, no destination, schedule off. Unpair copy: "Removes the URL and sealed token rows. The key pin, receipts and local copies stay. The credential is dead only when the KyRecovery admin revokes it."

- [ ] **Step 3: Test, build, commit**

Run: `cd web && npm test && npm run build`
Expected: PASS

```bash
git add web
git commit -m "web: disaster recovery screen to the KySignOn spec"
```

---

### Task 6: Decrypt guard, compose, Dockerfile, smoke test

**Files:**
- Rewrite: `internal/backup/nodecrypt_test.go`
- Modify: `docker-compose.yml:17`, `Dockerfile:30`, `scripts/smoke-test.sh:45,68`
- Create: `docker-compose.lan-dns.yml`

- [ ] **Step 1: Guard**

```go
package backup_test

func TestNothingInTheServerDecrypts(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	guardtest.NoDecryptOutside(t, root, map[string][]string{
		filepath.Join("cmd", "server", "main.go"): {"restore"},
	})
}
```

Prove it: add `_ = recoveryclient.Restore` to `handleBackupStatus`, run `go test ./internal/backup/ -run Nothing`, confirm it fails naming `backup_handlers.go`, remove, confirm pass. Paste both outputs in the PR.

- [ ] **Step 2: Compose and Dockerfile**

`docker-compose.yml`: replace `KY_BACKUP_DIR=/app/backups` with pass-through of `KY_BACKUP_DEPOSIT_INTERVAL: ${KY_BACKUP_DEPOSIT_INTERVAL:-24h}`, `KY_BACKUP_DIR: ${KY_BACKUP_DIR:-}`, `KY_BACKUP_KEEP: ${KY_BACKUP_KEEP:-7}`, `KY_BACKUP_ALLOW_PRIVATE_RECOVERY: ${KY_BACKUP_ALLOW_PRIVATE_RECOVERY:-false}`, and a comment that `KY_DNS` lives only in the override file. `docker-compose.lan-dns.yml`, service name as in the base file:

```yaml
# Optional override: send the container's DNS lookups to your LAN's resolver, so names that
# exist only there (a KyRecovery behind your own proxy) resolve inside the container. It
# replaces the host's resolvers for every lookup this container makes, which is why it is
# not in the base file.
#
#   KY_DNS=192.168.1.1 docker compose -f docker-compose.yml -f docker-compose.lan-dns.yml up -d
services:
  ky-server-base:
    dns:
      - ${KY_DNS:?set KY_DNS to your LAN DNS server}
```

`Dockerfile:30`: remove the `backups` directory creation or keep it as the optional mount point; `scripts/smoke-test.sh:45,68`: the plaintext-backup assertions go; if the smoke test wants a backup check, set `KY_BACKUP_DIR`, pin a key via `POST /api/backup/pin-key` with a freshly generated key, `POST /api/backup/deposit`, and assert one `*.kycap` at mode 600.

- [ ] **Step 3: Run everything, commit**

Run: `make ci && docker compose config -q && KY_DNS=1.1.1.1 docker compose -f docker-compose.yml -f docker-compose.lan-dns.yml config -q`
Expected: PASS

```bash
git add internal/backup/nodecrypt_test.go docker-compose.yml docker-compose.lan-dns.yml Dockerfile scripts/smoke-test.sh
git commit -m "guard via guardtest; compose backup env pass-through and LAN DNS override"
```

---

### Task 7: Runbook and docs

**Files:**
- Create: `docs/RESTORE.md` (from `kysignon-server/docs/RESTORE.md`, 259 lines), `README.md`
- Modify: `internal/backup/AGENTS.md`, `internal/api/AGENTS.md`, `AGENTS.md` (DOX index)

- [ ] **Step 1: RESTORE.md**

Adapt kysignon's runbook: binary `ky_server_base restore -capsule <file> -to <dir> [-service <name>]`, env `KY_*`, data dir from `KY_DATA_DIR` (check `config.go` for the real name), the key that is never rotated is `encryption.key` (the database is encrypted under it), session revocation is per user (`internal/store` has no global revoke; say so). Keep every hazard: empty-volume gate (`-wal` replay), copy the old volume out first at mode 700 as root verified by count, no keys on stdout, Docker target writable as `$(id -u):$(id -g)`, exact rotation commands, post-restore trust step (compare `recovery_key_id` in status against the ceremony card).

- [ ] **Step 2: Prove Step 1 of the runbook in a scratch run**

```bash
go run ./cmd/kyrecovery-tools split 2 3   # or: a 10-line Go test that calls recoverykey.Generate + Split, prints shares and base64 public key
# pin the key by hand through the API against a throwaway data dir, POST /api/backup/deposit with KY_BACKUP_DIR set,
# then:
printf '%s\n%s\n' "$SHARE1" "$SHARE2" | ./ky_server_base restore -capsule "$KY_BACKUP_DIR"/*.kycap -to /tmp/restore-test -service Busnes.app
```

Also run each failure mode: one share (error names threshold), `-service wrong` (refused before shares are read), non-empty `-to` (refused). Record the four outputs in the PR.

- [ ] **Step 3: README.md and AGENTS.md**

`README.md` (new) with a "Disaster recovery" section: what a capsule carries; why TLS matters when the capsule is sealed (the public key arrives at pairing, trust on first use; the token; the receipts); pin by hand or compare fingerprints before trusting a pairing; every env var from Global Constraints with default and meaning; the LAN DNS override command; link to `docs/RESTORE.md`. Warn upgrading operators to move old plaintext backups out of `KY_BACKUP_DIR`, retain them until migration is verified, and securely remove them afterward; capsule retention deliberately ignores them.

`internal/backup/AGENTS.md`: rewrite Ownership and Local Contracts to say what is now here (Collect, Checks, Settings adapter, Sealer label) and that pairing, pin, run, schedule, local copies, drill mechanics, restore and the guard are `ky-primitives/recoveryclient`, with the contracts the lib README lists. `internal/api/AGENTS.md`: the route table, the "admin-only stands in for step-up" note, `AuditDetails`. Root `AGENTS.md` DOX index: add `docs/RESTORE.md`, `README.md`, drop any mention of `KY_BACKUP_DRILL_DAY` and plaintext local backups.

- [ ] **Step 4: Commit, PR**

```bash
git add docs/RESTORE.md README.md AGENTS.md internal/backup/AGENTS.md internal/api/AGENTS.md
git commit -m "docs: restore runbook, README disaster recovery, DOX to spec"
```

Open one PR for the whole branch (`pull-request` skill); expect a security review round. PR body lists: the guard proof outputs, the runbook proof outputs, and the step-up assumption.

---

### Task 8: Prove it live, hand off

- [ ] **Step 1: Screen live**

```bash
rm -rf /tmp/kyb && mkdir -p /tmp/kyb/data /tmp/kyb/backups
KY_DATA_DIR=/tmp/kyb/data KY_BACKUP_DIR=/tmp/kyb/backups KY_DB_DRIVER=sqlite ./ky_server_base &
```

Log in, pin a fresh key by hand, Back up now, check `ls -l /tmp/kyb/backups` shows `Busnes.app-<id>.kycap` at `-rw-------`, and the audit page shows `admin.backup_key_pin` and `admin.backup_run` rows. Screenshot for the PR.

- [ ] **Step 2: Live pairing in the homelab**

```bash
KY_BACKUP_ALLOW_PRIVATE_RECOVERY=true KY_DNS=192.168.1.1 docker compose -f docker-compose.yml -f docker-compose.lan-dns.yml up -d --force-recreate
docker inspect <container> --format '{{.HostConfig.Dns}}'   # must print [192.168.1.1]
```

Pair from the screen against Yoshi's KyRecovery, Back up now, confirm the receipt on the screen and the capsule in KyRecovery's dashboard. Unpair, confirm status shows key pinned and not paired.

- [ ] **Step 3: Post to myslop** folder `ky-server-base-sealed-token`: done rows, proof outputs, what is left for gridlock (per-file `git merge-file` with this branch's before/after as base/theirs; last time the conflicts were the tickets service field, DOX index and imports), status `done` for the scaffold half.

---

## Self-review notes

- Spec rows: 1 (T2/T3), 2 (T3 pin-key), 3 (T3/T4 Run), 4 (T1 config + lib), 5 (T3 schedule + T4 loop + T5 form), 6 (T3 unpair), 7 (T1 + T3 ValidateURL + startup log), 8 (T6), 9 (T5), 10 (T3 authz test, assumption noted), 11 (T6), 12 (T7), 13 (T7), 14 (already absent from `git ls-files`; confirm in T7).
- The old plaintext local backup (`BuildLocalPayload` name, `StorageDir`, `AutoDrillDay`, `KY_BACKUP_DRILL_DAY`) is removed in T1/T2/T6/T7; `KY_BACKUP_DIR` keeps its name with a new meaning and an empty default. Say this in the PR and README: an operator who had `KY_BACKUP_DIR=/app/backups` in compose now gets sealed copies there, while old plaintext files are never pruned automatically and must be moved and securely removed after migration.
- `settingsAdapter` binds a context per call site; in `backupLoop` the run uses `context.WithoutCancel(ctx)` so the adapter it is handed does too.
- Updated 2026-09-04 after harrier's posts 204/208/211: package renamed to `recoveryclient`; `Drill` takes a scratch root under the data dir (`DrillRoot`); `SQLiteSnapshot` replaces the hand-rolled `VACUUM INTO`; `Keep` validated at startup because the lib refuses `Keep < 1`.
