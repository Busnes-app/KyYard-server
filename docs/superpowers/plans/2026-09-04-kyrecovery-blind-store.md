# kyrecovery Blind Store Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** kyrecovery-server stores sealed kycap/3 capsules it cannot read, pins the suite recovery public key and hands it to products at pairing, runs the keypair ceremony in the browser, and loses every code path that decrypts.

**Architecture:** Deposit is raw container bytes checked against the pinned key ID through `capsule.ReadUnverifiedManifest` and hashed into an `auditchain` ledger. The ceremony is a `GOOS=js GOARCH=wasm` build of a tiny command over `recoverykey`, embedded and served to an admin tab; only the public key and ID come back. `internal/capsule`, `internal/adapter`, `internal/drill`, the decrypt ceremony and the plaintext push are deleted, not flagged.

**Tech Stack:** Go 1.26.6, `github.com/Busness-app/ky-primitives v0.4.1` (`recoverykey`, `capsule`, `auditchain`, `password`, `keyfile`), SQLite via `modernc.org/sqlite`, embedded static SPA (`internal/server/static`), GitHub Actions.

**Spec:** `/home/yoshi/busness.app/ky-primitives/docs/superpowers/specs/2026-09-04-kyrecovery-blind-store-design.md` (Parts 1–9 and the claims register). Its opening section records what kyrecovery is today with file:line references; the plan below cites the same lines.

## Global Constraints

- Repo: `/home/yoshi/busness.app/kyrecovery-server`, module `github.com/Busness-app/kyrecovery-server`, branch from `main` at `744dd37`.
- **Prerequisite:** ky-primitives PR #11 merged and tagged `v0.4.1` (exports `capsule.MaxContainerBytes`, `MaxFiles`, `MaxFileBytes`, `MaxExpandedBytes`). If the tag does not exist when Task 1 starts, stop and say so; do not pin a commit hash and do not mirror the constants.
- `go 1.26.6` in `go.mod` (the library's floor). `crypto/hpke` is stdlib in 1.26; nothing else needs a KEM.
- **kyrecovery never holds a recovery private key, seed, share or plaintext payload.** Only `cmd/ceremony-wasm` (compiled to WASM, never linked into the server) and `_test.go` files may call `recoverykey.Generate`, `recoverykey.Split`, `recoverykey.Combine`, `recoverykey.FromSeed`, `capsule.Open`, `capsule.Seal` or `hpke.NewRecipient`. Task 8 pins this with a test.
- **Nothing is in the wild.** Schema changes rewrite the `CREATE TABLE` literals; existing dev databases are deleted, not migrated. Existing audit events are not carried.
- Authorization stays the `apiPolicy` table in `internal/server/server.go:216-262`; every new route is listed there explicitly, and the default for an unlisted route stays `auth.RoleAdmin`.
- Body caps: `/api/backup/deposit` is capped at `capsule.MaxContainerBytes` (384 MiB); every other API body at the existing `maxAPIBodyBytes` (1 MiB).
- Manifest fields are displayed, never decided on, except `RecoveryKeyID` (compared to the pin) and `ServiceName` (compared to the paired app).
- Commit after every task, message `type(scope): imperative sentence`. Tests are `package server_test` etc., matching the file they extend.
- Gate for every task: `gofmt -l $(git ls-files '*.go')` silent, `go mod tidy` a no-op, `go vet ./...`, `go test -race -count=1 ./...`.

## Decisions taken with Yoshi, 2026-09-04

| Decision | Choice |
|---|---|
| Share relay (spec option C) | Deferred to its own plan. |
| Ceremony host | Browser tab running a WASM build of `recoverykey`, served by kyrecovery. |
| Deletion scope | Everything that needs a key. |

---

## File structure

**Delete (Task 1):** `internal/capsule/`, `internal/adapter/`, `internal/drill/`, `internal/ceremony/`, `internal/export/`, `internal/crypto/shamir.go`, `internal/crypto/envelope.go` (and their tests), `internal/server/spec_ingest_test.go`, `internal/tui/` restore path (the whole package if nothing else remains), `data/kyrecovery.db`, the committed `kyrecovery` binary, `main.go` at the repo root if `cmd/kyrecovery/main.go` is the Dockerfile's entry.

**Create:**
- `.github/workflows/ci.yml`, `.github/workflows/ky-primitives-compat.yml`, `.gitignore` additions.
- `internal/server/recoverykey.go` — import endpoint + `GET /api/recovery-key`.
- `internal/server/deposit.go` — the deposit handler.
- `internal/server/verify.go` — per-capsule verify + the daily sweep.
- `internal/audit/chain.go` — `auditchain`-backed ledger (replaces `ledger.go`'s hashing).
- `cmd/ceremony-wasm/main.go` — the WASM entry.
- `internal/server/static/ceremony.html`, `internal/server/static/js/ceremony.js`, `internal/server/static/wasm/ceremony.wasm` (built artefact, committed), `internal/server/static/wasm/wasm_exec.js` (copied from `$(go env GOROOT)/lib/wasm/wasm_exec.js`).
- `scripts/build-wasm.sh`, `scripts/test-wasm.mjs`.
- Tests: `internal/server/deposit_test.go`, `internal/server/recoverykey_test.go`, `internal/server/verify_test.go`, `internal/server/nodecrypt_test.go`, `internal/audit/chain_test.go`.

**Modify:** `go.mod`, `go.sum`, `Dockerfile`, `internal/db/db.go` (schema, records, accessors), `internal/server/server.go` (struct, routes, policy, claim handler, capsule detail), `internal/server/limits.go` (body limit), `internal/pairing/pairing.go` (delete the payload types), `pkg/client/client.go`, `cmd/kyrecovery/app/app.go`, `internal/diff/diff.go`, `internal/auth/auth.go` (password), `internal/secrets/secrets.go` (keyfile), `internal/server/static/index.html`, `internal/server/static/js/app.js`, `README.md`, `AGENTS.md`.

---

### Task 1: Delete every decrypting path, adopt the library, add CI

**Files:**
- Delete: the packages and files listed under "Delete" above.
- Modify: `go.mod`, `go.sum`, `Dockerfile`, `.gitignore`, `cmd/kyrecovery/app/app.go:29-63,87-179`, `internal/server/server.go:32-114,116-200,216-262,909-1070,525-599`, `internal/server/limits.go:20-24,54-60`, `internal/pairing/pairing.go:137-230`, `pkg/client/client.go:80-160`, `internal/diff/diff.go:161-230`, `internal/server/static/index.html`, `internal/server/static/js/app.js`
- Create: `.github/workflows/ci.yml`, `.github/workflows/ky-primitives-compat.yml`

**Interfaces:**
- Produces: a tree that builds and passes with no `internal/capsule`, `internal/adapter`, `internal/drill`, `internal/ceremony`, `internal/export`; `Server` struct without `runner`, `ceremonies`, `adapters`; `db.CapsuleRecord` unchanged for now (Task 3 extends it); `go.mod` on `go 1.26.6` with `github.com/Busness-app/ky-primitives v0.4.1`.

- [ ] **Step 1: Bump Go and add the library**

```bash
cd /home/yoshi/busness.app/kyrecovery-server
go mod edit -go=1.26.6
go get github.com/Busness-app/ky-primitives@v0.4.1
```

If `go get` fails to find `v0.4.1`, stop and report; the tag is a prerequisite.

- [ ] **Step 2: Delete the decrypting packages and artefacts**

```bash
git rm -r internal/capsule internal/adapter internal/drill internal/ceremony internal/export
git rm internal/crypto/shamir.go internal/crypto/shamir_test.go internal/crypto/envelope.go internal/crypto/envelope_test.go 2>/dev/null || true
git rm internal/server/spec_ingest_test.go
git rm data/kyrecovery.db kyrecovery
ls internal/crypto/   # whatever remains must not reference AES envelopes or Shamir
```

If `internal/crypto` is now empty, `git rm -r internal/crypto`. If `internal/tui` only exists to restore capsules (read `internal/tui/tui.go`; it imports `internal/capsule`), `git rm -r internal/tui` and drop the `tui` subcommand. Add to `.gitignore`:

```
/kyrecovery
/data/
```

- [ ] **Step 3: Remove the dead subcommands**

In `cmd/kyrecovery/app/app.go`, the switch at lines 29–63 becomes:

```go
	switch command {
	case "serve":
		cmdServe(args[2:])
	case "audit":
		cmdAudit(args[2:])
	case "pair":
		cmdPair(args[2:])
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command %q. Run 'kyrecovery help' for usage.\n", command)
		os.Exit(1)
	}
```

Delete `cmdCapture`, `cmdRestore`, `cmdDrill`, `cmdSplitKey`, `cmdCombineShares`, `cmdExportKit`, `cmdTUI` and their helpers. In `cmdPair`'s inner switch (`app.go:518-640`) delete the `"push"` case (it sent plaintext). Update `printUsage` to list only `serve`, `audit`, `pair` (`generate`, `list`, `claim`).

- [ ] **Step 4: Cut the server down**

`internal/server/server.go`:

- Struct (lines 47–63): remove `runner`, `ceremonies`, `adapters`. Keep `cfg, db, ledger, authMgr, replication, inspector, claimLimit, loginLimit, pushLimit, pushSlots, mux`.
- `New` (66–114): delete every `adapter.New*`, `drill.NewRunner`, `ceremony.NewManager`, and the `adapters` map; the struct literal loses those three fields.
- `routes()` (116–200): delete the registrations for `/api/capsules/capture`, `/api/drills`, `/api/drills/run`, `/api/ceremonies*`, `/api/backup/push`, `/api/v1/backup/push`. Keep `/api/capsules`, `/api/capsules/`, `/api/pairing/*`, `/api/custodians*`, `/api/replication/*`, `/api/audit*`, `/api/auth/*`, `/api/readiness`, static.
- `apiPolicy` (216–262): delete the rows for the routes above (`capture`, `drills`, `ceremonies`, both `backup/push`).
- `requiredRole` (274–293): the capsule sub-resource switch keeps `"download"` as `auth.RoleOperator` and drops `"export-kit"`.
- Delete `handleCapsuleCapture`, `handleBackupPush`, every `handleDrill*`, every `handleCeremony*`, and the `export-kit` case inside `handleCapsuleDetail` (lines 546–594), leaving `download` and `default`.
- `Close()` (1839): remove the `s.ceremonies.Close()` call.

`internal/server/limits.go`: delete `defaultMaxBackupPushBytes`, `EnvMaxBackupPushBytes`, `maxSharesPerCapsule`, `maxBackupPushBytes()`; `bodyLimit` becomes `return maxAPIBodyBytes` for now (Task 3 adds the deposit path). Delete `newCapsuleID` if nothing calls it after this step.

`internal/pairing/pairing.go`: delete `SelfDeclaredBackupPayload`, `BackupFiles`, `BackupDependencies`, `IngestLimits`, `DefaultIngestLimits`, `IngestSelfDeclaredBackup`, `safeRelPath` and their tests; keep `GeneratePairingCode`.

`pkg/client/client.go`: delete `BackupPushPayload`, `PushResponse`, `PushBackup`, `PushDirectory`; keep `Client`, `NewClient`, `ClaimResponse`, `ClaimPairing`. Change `ClaimPairing` to use a `&http.Client{Timeout: 60 * time.Second}` instead of `http.DefaultClient`.

`internal/diff/diff.go`: `DiffByCapsuleIDs` and `GetServiceTimeline` currently read manifests from files via `capsule.ReadManifestFromFile`. Replace the manifest type with a local struct built from `db.CapsuleRecord`:

```go
// manifestView is what the inspector compares: the deposited record, not the container.
type manifestView struct {
	CapsuleID   string
	ServiceName string
	PayloadHash string
	CreatedAt   time.Time
	Threshold   int
	TotalShares int
}

func viewOf(rec *db.CapsuleRecord) *manifestView {
	return &manifestView{CapsuleID: rec.ID, ServiceName: rec.ServiceName, PayloadHash: rec.PayloadHash,
		CreatedAt: rec.CreatedAt, Threshold: rec.Threshold, TotalShares: rec.TotalShares}
}
```

`CompareManifests(base, target *manifestView)` compares the six fields; file and dependency diffs are removed (the store cannot see inside a capsule). `GetServiceTimeline` no longer counts files; drop `FilesCount` from `HistoricalTimelineEntry`. Update `diff_test.go` to build `db.CapsuleRecord`s.

- [ ] **Step 5: Cut the UI down**

`internal/server/static/index.html`: remove the nav buttons and panels for `tab-drills` (298–323) and the ceremony panel inside `tab-custodians` (212–239), the `#ceremony-create-modal` and `#ceremony-submit-modal` (497–560), the capture form (the "Capture Capsule" modal and its button), and the shares dialog markup (391–406). `js/app.js`: delete `loadDrills`, `loadCeremonies`, `openCeremonyCreateModal`, `submitCreateCeremony`, `showSharesDialog`, `openDrillModal`, the capture submit function, and their calls in `DOMContentLoaded` (lines 14–17). In the capsule row template (83–115) drop the "Run Drill" and "Export Kit" buttons; keep "Download".

- [ ] **Step 6: CI**

`.github/workflows/ci.yml`:

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
  workflow_dispatch:
permissions:
  contents: read
# Every checkout uses persist-credentials: false. Nothing after a checkout runs git
# against a remote, so the job token has no reason to sit in .git/config.
jobs:
  go:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          persist-credentials: false
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
      - run: test -z "$(gofmt -l $(git ls-files '*.go'))"
      - run: go mod tidy && git diff --exit-code go.mod go.sum && go mod verify
      - run: go vet ./...
      - run: go build ./...
      - run: go test -race -count=1 ./...
  security:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          persist-credentials: false
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
      - run: go run golang.org/x/vuln/cmd/govulncheck@latest ./...
  docker:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          persist-credentials: false
      - run: docker build -t kyrecovery:ci .
```

`.github/workflows/ky-primitives-compat.yml`: copy from `/home/yoshi/busness.app/ky_server_base/.github/workflows/ky-primitives-compat.yml`, replacing `ky_server_base` in the failure message with `kyrecovery-server`.

`Dockerfile`: `FROM golang:1.26.6-alpine AS builder`; confirm `cmd/kyrecovery/main.go` exists (`ls cmd/kyrecovery/`); if the repo root `main.go` is a duplicate entry point, `git rm main.go`.

- [ ] **Step 7: Build until green**

```bash
go build ./... 2>&1 | head -30
```

Fix every dangling reference the deletions expose (they will be in `server.go`, `app.go`, `diff.go`, `pkg/client`). Do not re-add any deleted symbol; if a caller needed it, the caller goes too.

```bash
go mod tidy && gofmt -l $(git ls-files '*.go'); go vet ./... && go test -race -count=1 ./...
```

Expected: every remaining package `ok`. Existing tests that exercised deleted handlers (in `server_test.go`, `hostile_test.go`, `regression_test.go`, `authz_test.go`) are deleted at the test-function level, not commented out.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "refactor!: delete every decrypting path; kyrecovery becomes a blind store on ky-primitives v0.4.1"
```

---

### Task 2: Recovery key import and pin

**Files:**
- Modify: `internal/db/db.go` (schema literal at 190–312; add record + accessors)
- Create: `internal/server/recoverykey.go`, `internal/server/recoverykey_test.go`
- Modify: `internal/server/server.go` (`routes`, `apiPolicy`)

**Interfaces:**
- Consumes: `recoverykey.ParsePublicKey([]byte) (recoverykey.PublicKey, error)`, `PublicKey.ID() string`, `PublicKey.Bytes() []byte`, `recoverykey.PublicKeyBytes = 1216`.
- Produces:

```go
// internal/db
type RecoveryKeyRecord struct {
	KeyID       string    `json:"key_id"`
	PublicKey   []byte    `json:"-"`
	Threshold   int       `json:"threshold"`
	TotalShares int       `json:"total_shares"`
	ImportedBy  string    `json:"imported_by"`
	ImportedAt  time.Time `json:"imported_at"`
}
var ErrRecoveryKeyExists = errors.New("a recovery key is already imported")
func (d *DB) InsertRecoveryKey(ctx context.Context, k RecoveryKeyRecord) error   // ErrRecoveryKeyExists on a second row
func (d *DB) GetRecoveryKey(ctx context.Context) (*RecoveryKeyRecord, error)     // (nil, nil) when none
```

  Routes: `POST /api/recovery-key` (`auth.RoleAdmin`), `GET /api/recovery-key` (`auth.RoleViewer`; returns the record plus `public_key` as standard base64).

- [ ] **Step 1: Failing test**

`internal/server/recoverykey_test.go`:

```go
package server_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busness-app/ky-primitives/recoverykey"
	"github.com/Busness-app/kyrecovery-server/internal/audit"
	"github.com/Busness-app/kyrecovery-server/internal/auth"
	"github.com/Busness-app/kyrecovery-server/internal/db"
	"github.com/Busness-app/kyrecovery-server/internal/server"
)

// newAdminServer mirrors server_test.go's inline setup: in-memory DB, local admin login,
// and the session cookie every admin request needs.
func newAdminServer(t *testing.T) (*server.Server, *http.Cookie, *db.DB) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ledger := audit.NewLedger(database)
	authMgr := auth.NewManager(auth.OIDCConfig{}, database)
	adminPass, _, err := authMgr.EnsureAdminUser(t.Context(), "TestAdminPassword123!")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{Port: 8095, DataDir: t.TempDir()}, database, ledger)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": adminPass})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login/local", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	return srv, rec.Result().Cookies()[0], database
}

func importKey(t *testing.T, srv *server.Server, cookie *http.Cookie, pub recoverykey.PublicKey, k, n int) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"public_key": base64.StdEncoding.EncodeToString(pub.Bytes()), "threshold": k, "total_shares": n,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/recovery-key", bytes.NewReader(body))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestRecoveryKeyImportIsSingleShot(t *testing.T) {
	srv, cookie, database := newAdminServer(t)
	a, _ := recoverykey.Generate() // test-only: the private half never leaves this test
	b, _ := recoverykey.Generate()

	if rec := importKey(t, srv, cookie, a.Public(), 3, 5); rec.Code != http.StatusCreated {
		t.Fatalf("first import: %d %s", rec.Code, rec.Body.String())
	}
	if rec := importKey(t, srv, cookie, b.Public(), 3, 5); rec.Code != http.StatusConflict {
		t.Fatalf("second import: %d, want 409", rec.Code)
	}
	stored, err := database.GetRecoveryKey(t.Context())
	if err != nil || stored == nil || stored.KeyID != a.Public().ID() {
		t.Fatalf("pin changed or missing: %+v %v", stored, err)
	}
	if stored.Threshold != 3 || stored.TotalShares != 5 {
		t.Fatalf("topology %d/%d", stored.Threshold, stored.TotalShares)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/recovery-key", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var got struct {
		KeyID     string `json:"key_id"`
		PublicKey string `json:"public_key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.KeyID != a.Public().ID() || got.PublicKey != base64.StdEncoding.EncodeToString(a.Public().Bytes()) {
		t.Fatalf("GET returned %+v", got)
	}
}

func TestRecoveryKeyImportRefusesBadInput(t *testing.T) {
	srv, cookie, _ := newAdminServer(t)
	a, _ := recoverykey.Generate()
	short := a.Public().Bytes()[:100]
	body, _ := json.Marshal(map[string]any{"public_key": base64.StdEncoding.EncodeToString(short), "threshold": 2, "total_shares": 3})
	req := httptest.NewRequest(http.MethodPost, "/api/recovery-key", bytes.NewReader(body))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short key: %d", rec.Code)
	}
	for _, kn := range [][2]int{{0, 0}, {1, 3}, {4, 3}, {2, 256}} {
		if rec := importKey(t, srv, cookie, a.Public(), kn[0], kn[1]); rec.Code != http.StatusBadRequest {
			t.Fatalf("topology %v: %d", kn, rec.Code)
		}
	}
	// A body carrying shares is refused outright: the server must never see one.
	body, _ = json.Marshal(map[string]any{"public_key": base64.StdEncoding.EncodeToString(a.Public().Bytes()), "threshold": 2, "total_shares": 3, "shares": []string{"ky2-x"}})
	req = httptest.NewRequest(http.MethodPost, "/api/recovery-key", bytes.NewReader(body))
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("body with shares: %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run to see it fail**

```bash
go test ./internal/server/ -run TestRecoveryKey 2>&1 | head -5
```

Expected: `database.GetRecoveryKey undefined` and 404s.

- [ ] **Step 3: Schema and accessors**

In `internal/db/db.go`, inside the `schema` literal after `paired_apps`:

```sql
	CREATE TABLE IF NOT EXISTS recovery_key (
		singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
		key_id TEXT NOT NULL,
		public_key BLOB NOT NULL,
		threshold INTEGER NOT NULL,
		total_shares INTEGER NOT NULL,
		imported_by TEXT NOT NULL,
		imported_at DATETIME NOT NULL
	);
```

Records and accessors (next to `PairedAppRecord`):

```go
// RecoveryKeyRecord is the suite recovery public key this store hands to products and pins
// deposits against. There is exactly one row; the private half never existed here.
type RecoveryKeyRecord struct {
	KeyID       string    `json:"key_id"`
	PublicKey   []byte    `json:"-"`
	Threshold   int       `json:"threshold"`
	TotalShares int       `json:"total_shares"`
	ImportedBy  string    `json:"imported_by"`
	ImportedAt  time.Time `json:"imported_at"`
}

var ErrRecoveryKeyExists = errors.New("a recovery key is already imported")

func (d *DB) InsertRecoveryKey(ctx context.Context, k RecoveryKeyRecord) error {
	q := `INSERT INTO recovery_key (singleton, key_id, public_key, threshold, total_shares, imported_by, imported_at)
	      VALUES (1, ?, ?, ?, ?, ?, ?)`
	_, err := d.conn.ExecContext(ctx, q, k.KeyID, k.PublicKey, k.Threshold, k.TotalShares, k.ImportedBy, k.ImportedAt.UTC())
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrRecoveryKeyExists
	}
	return err
}

func (d *DB) GetRecoveryKey(ctx context.Context) (*RecoveryKeyRecord, error) {
	q := `SELECT key_id, public_key, threshold, total_shares, imported_by, imported_at FROM recovery_key WHERE singleton = 1`
	var k RecoveryKeyRecord
	err := d.conn.QueryRowContext(ctx, q).Scan(&k.KeyID, &k.PublicKey, &k.Threshold, &k.TotalShares, &k.ImportedBy, &k.ImportedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}
```

Confirm how modernc's sqlite spells the primary-key violation (`constraint failed: UNIQUE constraint failed: recovery_key.singleton`); adjust the `Contains` string to what a test observes, or check `sqlite.Error` codes if the package exposes them.

- [ ] **Step 4: Handler**

`internal/server/recoverykey.go`:

```go
package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Busness-app/ky-primitives/recoverykey"
	"github.com/Busness-app/kyrecovery-server/internal/db"
)

// recoveryKeyImport is the whole body the ceremony page may send. It has no field for
// shares on purpose; DisallowUnknownFields makes a body carrying one a 400.
type recoveryKeyImport struct {
	PublicKey   string `json:"public_key"`
	Threshold   int    `json:"threshold"`
	TotalShares int    `json:"total_shares"`
}

func validTopology(k, n int) bool { return k >= 2 && n >= k && n <= 255 }

func (s *Server) handleRecoveryKey(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rec, err := s.db.GetRecoveryKey(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed reading recovery key")
			return
		}
		if rec == nil {
			writeError(w, http.StatusNotFound, "No recovery key imported; run the ceremony")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"key_id": rec.KeyID, "public_key": base64.StdEncoding.EncodeToString(rec.PublicKey),
			"threshold": rec.Threshold, "total_shares": rec.TotalShares,
			"imported_by": rec.ImportedBy, "imported_at": rec.ImportedAt,
		})
	case http.MethodPost:
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var req recoveryKeyImport
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Body must be exactly {public_key, threshold, total_shares}")
			return
		}
		raw, err := base64.StdEncoding.DecodeString(req.PublicKey)
		if err != nil {
			writeError(w, http.StatusBadRequest, "public_key is not standard base64")
			return
		}
		pk, err := recoverykey.ParsePublicKey(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("public_key: %v", err))
			return
		}
		if !validTopology(req.Threshold, req.TotalShares) {
			writeError(w, http.StatusBadRequest, "threshold and total_shares must satisfy 2 <= threshold <= total_shares <= 255")
			return
		}
		rec := db.RecoveryKeyRecord{
			KeyID: pk.ID(), PublicKey: pk.Bytes(), Threshold: req.Threshold, TotalShares: req.TotalShares,
			ImportedBy: s.actor(r), ImportedAt: time.Now().UTC(),
		}
		if err := s.db.InsertRecoveryKey(r.Context(), rec); err != nil {
			if errors.Is(err, db.ErrRecoveryKeyExists) {
				existing, _ := s.db.GetRecoveryKey(r.Context())
				id := ""
				if existing != nil {
					id = existing.KeyID
				}
				writeError(w, http.StatusConflict, fmt.Sprintf("recovery key %s is already imported; rotation is a separate procedure", id))
				return
			}
			writeError(w, http.StatusInternalServerError, "Failed storing recovery key")
			return
		}
		_, _ = s.ledger.Record(r.Context(), "recovery_key_imported", s.actor(r), rec.KeyID, map[string]interface{}{
			"threshold": rec.Threshold, "total_shares": rec.TotalShares,
		})
		writeJSON(w, http.StatusCreated, map[string]any{"key_id": rec.KeyID, "threshold": rec.Threshold, "total_shares": rec.TotalShares})
	default:
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}
```

`server.go`: in `routes()` add `s.mux.HandleFunc("/api/recovery-key", s.handleRecoveryKey)`; in `apiPolicy` add `"GET /api/recovery-key": auth.RoleViewer,` and `"POST /api/recovery-key": auth.RoleAdmin,`.

- [ ] **Step 5: Run, gate, commit**

```bash
go test -race -count=1 ./internal/server/ -run TestRecoveryKey && gofmt -l $(git ls-files '*.go'); go vet ./... && go test -race -count=1 ./...
git add -A && git commit -m "feat(recoverykey): import and pin the suite recovery public key, single-shot"
```

---

### Task 3: Pairing hands out the key; deposit replaces push

**Files:**
- Modify: `internal/db/db.go` (`paired_apps` + `capsules` DDL, `PairedAppRecord`, `CapsuleRecord`, `InsertCapsule`, `GetCapsule`, `ListCapsules` column lists, `ClaimPairingCode`)
- Modify: `internal/server/server.go:842-906` (`handlePairingClaim`), `routes`, `apiPolicy`
- Modify: `internal/server/limits.go` (`bodyLimit`)
- Create: `internal/server/deposit.go`, `internal/server/deposit_test.go`
- Modify: `pkg/client/client.go` (`ClaimResponse`, new `Deposit`)

**Interfaces:**
- Consumes: `capsule.ReadUnverifiedManifest(raw []byte) (capsule.UnverifiedManifest, error)` with fields `CapsuleID, ServiceName, AppVersion, CreatedAt, PayloadHash, Threshold, TotalShares, RecoveryKeyID, EncapsulatedKey`; `capsule.MaxContainerBytes`; `db.GetRecoveryKey` from Task 2.
- Produces:

```go
// internal/db
type CapsuleRecord struct {
	ID              string    `json:"id"`
	ServiceName     string    `json:"service_name"`
	AppName         string    `json:"app_name"`
	AppVersion      string    `json:"app_version"`
	FilePath        string    `json:"-"`
	SizeBytes       int64     `json:"size_bytes"`
	Digest          string    `json:"digest"`        // SHA-256 hex of the container as deposited
	PayloadHash     string    `json:"payload_hash"`  // from the manifest, sealer-attested
	Threshold       int       `json:"threshold"`
	TotalShares     int       `json:"total_shares"`
	RecoveryKeyID   string    `json:"recovery_key_id"`
	EncapsulatedKey string    `json:"-"`
	CreatedAt       time.Time `json:"created_at"`    // from the manifest
	DepositedAt     time.Time `json:"deposited_at"`  // kyrecovery's clock
	PairedAppID     string    `json:"paired_app_id"`
	Status          string    `json:"status"`        // "active" | "corrupt"
}
// PairedAppRecord gains RecoveryKeyID string `json:"recovery_key_id,omitempty"`
func (d *DB) ClaimPairingCode(ctx, code, serviceName, appName, recoveryKeyID string) (*PairedAppRecord, error)
// pkg/client
type ClaimResponse struct { ...; RecoveryPublicKey string `json:"recovery_public_key"`; Threshold int `json:"threshold"`; TotalShares int `json:"total_shares"` }
type DepositResponse struct { CapsuleID string `json:"capsule_id"`; Digest string `json:"digest"`; SizeBytes int64 `json:"size_bytes"`; DepositedAt time.Time `json:"deposited_at"` }
func (c *Client) Deposit(ctx context.Context, container []byte) (*DepositResponse, error)
```

  Route: `POST /api/backup/deposit`, `rolePublic` (bearer product token), body cap `capsule.MaxContainerBytes`.

- [ ] **Step 1: Failing tests**

`internal/server/deposit_test.go`:

```go
package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busness-app/ky-primitives/capsule"
	"github.com/Busness-app/ky-primitives/recoverykey"
	"github.com/Busness-app/kyrecovery-server/internal/pairing"
	"github.com/Busness-app/kyrecovery-server/internal/server"
)

// pairProduct generates a code as admin and claims it as the product, returning the token
// and the claim body.
func pairProduct(t *testing.T, srv *server.Server, cookie *http.Cookie, service string) (string, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"ttl_minutes": 10, "service_name": service, "app_name": service})
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/generate", bytes.NewReader(body))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body.String())
	}
	var gen struct {
		PairingCode string `json:"pairing_code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &gen)
	body, _ = json.Marshal(map[string]string{"pairing_code": gen.PairingCode, "service_name": service, "app_name": service})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/pairing/claim", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	var claim map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &claim)
	return claim["api_token"].(string), claim
}

func sealFor(t *testing.T, pub recoverykey.PublicKey, service string) []byte {
	t.Helper()
	raw, _, err := capsule.Seal(service, "1.0.0", []capsule.File{{Path: "data/x.db", Content: []byte("payload"), Mode: 0600}}, nil, nil, 3, 5, pub)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func deposit(srv *server.Server, token string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/backup/deposit", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestClaimIsRefusedUntilAKeyIsImported(t *testing.T) {
	srv, cookie, _ := newAdminServer(t)
	body, _ := json.Marshal(map[string]any{"ttl_minutes": 10, "service_name": "kynotes", "app_name": "kynotes"})
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/generate", bytes.NewReader(body))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var gen struct {
		PairingCode string `json:"pairing_code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &gen)
	body, _ = json.Marshal(map[string]string{"pairing_code": gen.PairingCode, "service_name": "kynotes", "app_name": "kynotes"})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/pairing/claim", bytes.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("claim without key: %d, want 409", rec.Code)
	}
	// The code was not consumed: the same claim succeeds after import.
	k, _ := recoverykey.Generate()
	importKey(t, srv, cookie, k.Public(), 3, 5)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/pairing/claim", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("claim after import: %d %s", rec.Code, rec.Body.String())
	}
}

func TestClaimHandsOutThePinnedKey(t *testing.T) {
	srv, cookie, database := newAdminServer(t)
	k, _ := recoverykey.Generate()
	importKey(t, srv, cookie, k.Public(), 3, 5)
	_, claim := pairProduct(t, srv, cookie, "kynotes")
	if claim["recovery_public_key"] != base64.StdEncoding.EncodeToString(k.Public().Bytes()) {
		t.Fatal("claim did not carry the pinned public key")
	}
	if int(claim["threshold"].(float64)) != 3 || int(claim["total_shares"].(float64)) != 5 {
		t.Fatalf("topology %v/%v", claim["threshold"], claim["total_shares"])
	}
	apps, _ := database.ListPairedApps(t.Context())
	if len(apps) != 1 || apps[0].RecoveryKeyID != k.Public().ID() {
		t.Fatalf("paired app did not record the key ID: %+v", apps)
	}
}

func TestDepositAcceptsACapsuleSealedToThePinnedKey(t *testing.T) {
	srv, cookie, database := newAdminServer(t)
	k, _ := recoverykey.Generate()
	importKey(t, srv, cookie, k.Public(), 3, 5)
	token, _ := pairProduct(t, srv, cookie, "kynotes")
	raw := sealFor(t, k.Public(), "kynotes")

	rec := deposit(srv, token, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deposit: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		CapsuleID string `json:"capsule_id"`
		Digest    string `json:"digest"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	sum := sha256.Sum256(raw)
	if resp.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s", resp.Digest)
	}
	stored, _ := database.GetCapsule(t.Context(), resp.CapsuleID)
	m, _ := capsule.ReadUnverifiedManifest(raw)
	if stored == nil || stored.RecoveryKeyID != k.Public().ID() || stored.PayloadHash != m.PayloadHash || stored.ServiceName != "kynotes" || stored.SizeBytes != int64(len(raw)) {
		t.Fatalf("record %+v", stored)
	}
	// Re-sending the same bytes is idempotent.
	if rec := deposit(srv, token, raw); rec.Code != http.StatusOK {
		t.Fatalf("duplicate deposit: %d %s", rec.Code, rec.Body.String())
	}
	// Download returns the exact bytes with the digest.
	req := httptest.NewRequest(http.MethodGet, "/api/capsules/"+resp.CapsuleID+"/download", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), raw) || rec.Header().Get("X-Capsule-Digest") != resp.Digest {
		t.Fatalf("download: %d digest=%q", rec.Code, rec.Header().Get("X-Capsule-Digest"))
	}
}

func TestDepositRefusals(t *testing.T) {
	srv, cookie, _ := newAdminServer(t)
	k, _ := recoverykey.Generate()
	other, _ := recoverykey.Generate()
	importKey(t, srv, cookie, k.Public(), 3, 5)
	token, _ := pairProduct(t, srv, cookie, "kynotes")

	cases := []struct {
		name string
		body []byte
		want int
	}{
		{"not a container", []byte("hello"), http.StatusBadRequest},
		{"sealed to another key", sealFor(t, other.Public(), "kynotes"), http.StatusConflict},
		{"another service", sealFor(t, k.Public(), "kypost"), http.StatusForbidden},
		{"over the cap", bytes.Repeat([]byte{'x'}, int(capsule.MaxContainerBytes)+1), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		if rec := deposit(srv, token, tc.body); rec.Code != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
	if rec := deposit(srv, "kyrec_live_bogus", sealFor(t, k.Public(), "kynotes")); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", rec.Code)
	}
}

var _ = pairing.GeneratePairingCode // keep the import honest if the helper above changes
```

Add `"encoding/base64"` to the imports. The over-the-cap case allocates 384 MiB once; keep it in this test (the CI runner has the memory) unless `-short` is set, in which case skip that one row with `testing.Short()`.

- [ ] **Step 2: Run to see it fail**

```bash
go test ./internal/server/ -run 'TestClaim|TestDeposit' 2>&1 | head -5
```

- [ ] **Step 3: Schema and records**

`internal/db/db.go`: rewrite the `capsules` DDL (nothing is in the wild):

```sql
	CREATE TABLE IF NOT EXISTS capsules (
		id TEXT PRIMARY KEY,
		service_name TEXT NOT NULL,
		app_name TEXT NOT NULL DEFAULT '',
		app_version TEXT NOT NULL DEFAULT '',
		file_path TEXT NOT NULL,
		size_bytes INTEGER NOT NULL,
		digest TEXT NOT NULL,
		payload_hash TEXT NOT NULL,
		threshold INTEGER NOT NULL,
		total_shares INTEGER NOT NULL,
		recovery_key_id TEXT NOT NULL,
		encapsulated_key TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		deposited_at DATETIME NOT NULL,
		paired_app_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active'
	);
```

`paired_apps` gains `recovery_key_id TEXT NOT NULL DEFAULT ''` after `status`. Update `CapsuleRecord` and `PairedAppRecord` per the Interfaces block; update `InsertCapsule`, `GetCapsule`, `ListCapsules`, `InsertPairedApp`, `GetPairedAppByCode`, `GetPairedAppByToken`, `ListPairedApps` column lists and scans to match (every SELECT lists the same columns in the same order as its Scan).

`ClaimPairingCode` gains `recoveryKeyID string` and sets it in the UPDATE:

```go
	q := `UPDATE paired_apps SET status = 'paired', service_name = ?, app_name = ?, paired_at = ?, recovery_key_id = ?
	      WHERE id = ? AND status = 'pending' AND expires_at > ?`
	res, err := d.conn.ExecContext(ctx, q, serviceName, appName, now, recoveryKeyID, app.ID, now)
```

and `app.RecoveryKeyID = recoveryKeyID` before returning.

- [ ] **Step 4: Claim handler**

In `handlePairingClaim` (`server.go:842-906`), after the request is decoded and before `ClaimPairingCode`:

```go
	key, err := s.db.GetRecoveryKey(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed reading recovery key")
		return
	}
	if key == nil {
		// Not a failed attempt by the product: the store is not ready. The code is not consumed
		// and the limiter is not charged.
		writeError(w, http.StatusConflict, "No recovery key imported; run the ceremony before pairing products")
		return
	}
```

Pass `key.KeyID` to `ClaimPairingCode`, and extend the response:

```go
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":                  app.ID,
		"status":              "paired",
		"api_token":           app.APIToken,
		"service_name":        app.ServiceName,
		"app_name":            app.AppName,
		"paired_at":           app.PairedAt,
		"server_url":          r.Host,
		"recovery_public_key": base64.StdEncoding.EncodeToString(key.PublicKey),
		"threshold":           key.Threshold,
		"total_shares":        key.TotalShares,
	})
```

Add `"recovery_key_id": key.KeyID` to the `product_paired` audit details.

- [ ] **Step 5: Deposit handler**

`internal/server/deposit.go`:

```go
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Busness-app/ky-primitives/capsule"
	"github.com/Busness-app/kyrecovery-server/internal/db"
)

// handleDeposit stores a sealed container. It reads the manifest without a key, decides on
// exactly two of its fields — the recovery key ID against the pin and the service name
// against the paired app — and records the rest as the sealer attested it.
func (s *Server) handleDeposit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	authHeader := r.Header.Get("Authorization")
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" || authHeader == token {
		writeError(w, http.StatusUnauthorized, "Missing or invalid Bearer authorization token")
		return
	}
	ctx := r.Context()
	app, err := s.db.GetPairedAppByToken(ctx, token)
	if err != nil || app == nil {
		writeError(w, http.StatusUnauthorized, "Invalid or revoked API token")
		return
	}
	now := time.Now()
	pushKey := "push:" + app.ID
	if s.pushLimit.exceeded(pushKey, pushesPerToken, now) {
		writeError(w, http.StatusTooManyRequests, "Deposit rate limit exceeded for this paired product")
		return
	}
	s.pushLimit.record(pushKey, now)
	select {
	case s.pushSlots <- struct{}{}:
		defer func() { <-s.pushSlots }()
	case <-ctx.Done():
		writeError(w, http.StatusServiceUnavailable, "Server busy; retry shortly")
		return
	}

	raw, err := io.ReadAll(r.Body) // MaxBytesReader in ServeHTTP caps this at capsule.MaxContainerBytes
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("Container exceeds %d bytes", capsule.MaxContainerBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "Failed reading request body")
		return
	}
	m, err := capsule.ReadUnverifiedManifest(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Body is not a kycap/3 container")
		return
	}
	key, err := s.db.GetRecoveryKey(ctx)
	if err != nil || key == nil {
		writeError(w, http.StatusConflict, "No recovery key imported")
		return
	}
	if m.RecoveryKeyID != key.KeyID {
		writeError(w, http.StatusConflict, fmt.Sprintf("capsule is sealed to recovery key %s; this store pins %s", m.RecoveryKeyID, key.KeyID))
		return
	}
	if m.ServiceName != app.ServiceName {
		writeError(w, http.StatusForbidden, fmt.Sprintf("capsule names service %q; this token is paired for %q", m.ServiceName, app.ServiceName))
		return
	}
	if m.CapsuleID == "" || !validServiceName(m.ServiceName) || strings.ContainsAny(m.CapsuleID, "/\\\x00") {
		writeError(w, http.StatusBadRequest, "Manifest capsule_id or service_name is not a usable name")
		return
	}

	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	if existing, _ := s.db.GetCapsule(ctx, m.CapsuleID); existing != nil {
		if existing.Digest == digest {
			writeJSON(w, http.StatusOK, depositResponse(existing))
			return
		}
		writeError(w, http.StatusConflict, "A different capsule with this ID is already stored")
		return
	}

	rec := db.CapsuleRecord{
		ID: m.CapsuleID, ServiceName: m.ServiceName, AppName: app.AppName, AppVersion: m.AppVersion,
		FilePath: s.capsulePath(m.CapsuleID), SizeBytes: int64(len(raw)), Digest: digest,
		PayloadHash: m.PayloadHash, Threshold: m.Threshold, TotalShares: m.TotalShares,
		RecoveryKeyID: m.RecoveryKeyID, EncapsulatedKey: m.EncapsulatedKey,
		CreatedAt: m.CreatedAt, DepositedAt: now.UTC(), PairedAppID: app.ID, Status: "active",
	}
	if err := s.publishCapsule(ctx, rec, raw); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.db.UpdateAppLastBackup(ctx, app.ID)
	go s.replication.SyncAllAutoTargets(context.Background(), rec.ID)
	_, _ = s.ledger.Record(ctx, "capsule_deposited", "paired-app:"+app.ID, rec.ID, map[string]interface{}{
		"service_name": rec.ServiceName, "digest": digest, "size_bytes": rec.SizeBytes, "recovery_key_id": rec.RecoveryKeyID,
	})
	writeJSON(w, http.StatusCreated, depositResponse(&rec))
}

func depositResponse(rec *db.CapsuleRecord) map[string]any {
	return map[string]any{"capsule_id": rec.ID, "digest": rec.Digest, "size_bytes": rec.SizeBytes, "deposited_at": rec.DepositedAt}
}
```

Add `"context"` to the imports. `server.go`: register `s.mux.HandleFunc("/api/backup/deposit", s.handleDeposit)`; `apiPolicy` gains `"* /api/backup/deposit": rolePublic, // product API token`. `limits.go`:

```go
func bodyLimit(path string) int64 {
	if path == "/api/backup/deposit" {
		return capsule.MaxContainerBytes
	}
	return maxAPIBodyBytes
}
```

`handleCapsuleDetail`'s `download` case sets `w.Header().Set("X-Capsule-Digest", capRec.Digest)` before writing.

- [ ] **Step 6: Client**

`pkg/client/client.go`: extend `ClaimResponse` with `RecoveryPublicKey string \`json:"recovery_public_key"\``, `Threshold int \`json:"threshold"\``, `TotalShares int \`json:"total_shares"\``; in `ClaimPairing`, after decoding, return an error if `RecoveryPublicKey == ""` ("server returned no recovery public key; the ceremony has not run"). Add:

```go
type DepositResponse struct {
	CapsuleID   string    `json:"capsule_id"`
	Digest      string    `json:"digest"`
	SizeBytes   int64     `json:"size_bytes"`
	DepositedAt time.Time `json:"deposited_at"`
}

// Deposit stores a sealed container. The bytes are opaque to the server; it can only check
// that they are sealed to the key it handed out at pairing.
func (c *Client) Deposit(ctx context.Context, container []byte) (*DepositResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ServerURL+"/api/backup/deposit", bytes.NewReader(container))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deposit: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out DepositResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
```

- [ ] **Step 7: Gate and commit**

```bash
gofmt -l $(git ls-files '*.go'); go mod tidy && git diff --exit-code go.mod go.sum && go vet ./... && go test -race -count=1 ./...
git add -A && git commit -m "feat(deposit): store sealed capsules checked against the pinned key; claim hands out the public key"
```

---

### Task 4: Integrity attestation and the inspector

**Files:**
- Create: `internal/server/verify.go`, `internal/server/verify_test.go`
- Modify: `internal/db/db.go` (`SetCapsuleStatus`), `internal/server/server.go` (`routes`, `apiPolicy`, `Start` for the daily sweep), `internal/diff/diff.go` (already rewritten in Task 1; confirm it reads `Digest`/`DepositedAt` too)

**Interfaces:**
- Produces: `GET /api/capsules/{id}/verify` (`auth.RoleViewer`) → `{"capsule_id","digest","valid":bool,"checked_at"}`; `func (s *Server) verifyCapsule(ctx, rec *db.CapsuleRecord) (bool, error)`; `func (s *Server) verifyAll(ctx) (checked, corrupt int)`; `func (d *DB) SetCapsuleStatus(ctx, id, status string) error`.

- [ ] **Step 1: Failing test**

`internal/server/verify_test.go`:

```go
package server_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Busness-app/ky-primitives/recoverykey"
)

func TestVerifyDetectsAFlippedByte(t *testing.T) {
	srv, cookie, database := newAdminServer(t)
	k, _ := recoverykey.Generate()
	importKey(t, srv, cookie, k.Public(), 3, 5)
	token, _ := pairProduct(t, srv, cookie, "kynotes")
	raw := sealFor(t, k.Public(), "kynotes")
	rec := deposit(srv, token, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deposit: %d", rec.Code)
	}
	caps, _ := database.ListCapsules(t.Context())
	id := caps[0].ID

	verify := func() (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/capsules/"+id+"/verify", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr.Code, rr.Body.String()
	}
	if code, body := verify(); code != http.StatusOK || !contains(body, `"valid":true`) {
		t.Fatalf("intact: %d %s", code, body)
	}

	data, _ := os.ReadFile(caps[0].FilePath)
	data[len(data)/2] ^= 0x01
	if err := os.WriteFile(caps[0].FilePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if code, body := verify(); code != http.StatusOK || !contains(body, `"valid":false`) {
		t.Fatalf("flipped: %d %s", code, body)
	}
	after, _ := database.GetCapsule(t.Context(), id)
	if after.Status != "corrupt" {
		t.Fatalf("status %q", after.Status)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || len(s) > 0 && indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

(Use `strings.Contains` instead of the two helpers if `strings` is already imported in the file.) `caps[0].FilePath` is `json:"-"` but exported, so the test package can read it.

- [ ] **Step 2: Implement**

`internal/db/db.go`:

```go
func (d *DB) SetCapsuleStatus(ctx context.Context, id, status string) error {
	_, err := d.conn.ExecContext(ctx, `UPDATE capsules SET status = ? WHERE id = ?`, status, id)
	return err
}
```

`internal/server/verify.go`:

```go
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/Busness-app/kyrecovery-server/internal/db"
)

// verifyCapsule re-hashes the stored container against the digest recorded at deposit and
// writes the outcome to the audit chain. It is the only attestation this store can make:
// the bytes are what arrived. It says nothing about what is inside them.
func (s *Server) verifyCapsule(ctx context.Context, rec *db.CapsuleRecord) (bool, error) {
	f, err := os.Open(rec.FilePath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	valid := hex.EncodeToString(h.Sum(nil)) == rec.Digest
	action, status := "capsule_verified", "active"
	if !valid {
		action, status = "capsule_corrupt", "corrupt"
	}
	if err := s.db.SetCapsuleStatus(ctx, rec.ID, status); err != nil {
		return valid, err
	}
	_, _ = s.ledger.Record(ctx, action, "integrity-sweep", rec.ID, map[string]interface{}{"digest": rec.Digest})
	return valid, nil
}

func (s *Server) handleCapsuleVerify(w http.ResponseWriter, r *http.Request, rec *db.CapsuleRecord) {
	valid, err := s.verifyCapsule(r.Context(), rec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Capsule file unreadable on disk")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capsule_id": rec.ID, "digest": rec.Digest, "valid": valid, "checked_at": time.Now().UTC()})
}

// verifyAll is the daily sweep. It never stops on one bad capsule.
func (s *Server) verifyAll(ctx context.Context) (checked, corrupt int) {
	caps, err := s.db.ListCapsules(ctx)
	if err != nil {
		return 0, 0
	}
	for i := range caps {
		valid, err := s.verifyCapsule(ctx, &caps[i])
		if err != nil {
			continue
		}
		checked++
		if !valid {
			corrupt++
		}
	}
	return checked, corrupt
}

func (s *Server) runIntegritySweep(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.verifyAll(ctx)
		}
	}
}
```

In `handleCapsuleDetail` add `case "verify": s.handleCapsuleVerify(w, r, capRec)`; in `requiredRole`'s capsule switch, `"verify"` falls into the viewer default (it reads); in `Start` (`server.go:1810`) add `go s.runIntegritySweep(ctx)` before the listener starts.

- [ ] **Step 3: Gate and commit**

```bash
gofmt -l $(git ls-files '*.go'); go vet ./... && go test -race -count=1 ./...
git add -A && git commit -m "feat(verify): re-hash stored capsules on demand and daily, recording the outcome"
```

---

### Task 5: Audit ledger on `auditchain`

**Files:**
- Create: `internal/audit/chain.go`, `internal/audit/chain_test.go`
- Delete: `internal/audit/ledger.go`'s hashing (`CalculateEventHash`, `eventTuple`, `eventHash`, `rekeyLegacyChain`, `VerifyChain`, `ChainStatus`), `internal/audit/ledger_test.go` cases for them
- Modify: `internal/db/db.go` (`audit_events` DDL, `AuditRecord`, `InsertAuditEvent`, `GetLastAuditEvent`, new `audit_anchor` table + `GetAuditAnchor`/`SetAuditAnchor`, `IterAuditEvents`), `internal/server/server.go:737-752` (`handleAuditVerify`), `cmd/kyrecovery/app/app.go` (`cmdAudit` verify path)

**Interfaces:**
- Consumes: `auditchain.New(key []byte) (*auditchain.Chain, error)`, `auditchain.Resume(key, last auditchain.Record, anchor auditchain.Anchor)`, `(*Chain).Append(ctx, persist func(auditchain.Record, auditchain.Anchor) error, fields ...string) (auditchain.Record, error)`, `(*Chain).Anchor()`, `auditchain.VerifyStream(key, iter.Seq2[auditchain.Record, error], anchor) error`; `auditchain.Record{Seq uint64; Prev, Hash string; Fields []string}`; `auditchain.Anchor{Count uint64; Hash string}`.
- Produces: `audit.NewLedger(database *db.DB) *Ledger` (same constructor; resumes from the stored anchor), `(*Ledger).Record(ctx, action, actor, targetID string, details map[string]interface{}) (*db.AuditRecord, error)` (same signature), `(*Ledger).Verify(ctx) (auditchain.Anchor, error)`.

- [ ] **Step 1: Failing test**

`internal/audit/chain_test.go`:

```go
package audit_test

import (
	"testing"

	"github.com/Busness-app/kyrecovery-server/internal/audit"
	"github.com/Busness-app/kyrecovery-server/internal/db"
)

func TestLedgerVerifiesAndDetectsTruncation(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	l := audit.NewLedger(database)
	for i := 0; i < 3; i++ {
		if _, err := l.Record(t.Context(), "capsule_deposited", "paired-app:x", "cap-1", map[string]interface{}{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	anchor, err := l.Verify(t.Context())
	if err != nil || anchor.Count != 3 {
		t.Fatalf("verify: %v %+v", err, anchor)
	}
	// Remove the tail: the remaining chain still links, only the anchor knows.
	if err := database.DeleteAuditEventForTest(t.Context(), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verify(t.Context()); err == nil {
		t.Fatal("truncated log verified")
	}
	// A fresh ledger over the same store resumes from the anchor and refuses to append onto
	// a log that no longer matches it.
	l2 := audit.NewLedger(database)
	if _, err := l2.Record(t.Context(), "x", "y", "z", nil); err == nil {
		t.Fatal("append succeeded on a chain that fails its anchor")
	}
}
```

`DeleteAuditEventForTest` lives in `internal/db/export_test.go`? No — it must be callable from `audit_test`, a different package, so add it to `db.go` as an exported method with a doc line saying it exists for tests only, or put the truncation in the test through `database.Conn()` if one exists. Prefer a `func (d *DB) DeleteAuditEventForTest(ctx, seq uint64) error` in `db.go`; production code never calls it (Task 8's grep can assert that too).

- [ ] **Step 2: Schema**

Replace the `audit_events` DDL and add the anchor:

```sql
	CREATE TABLE IF NOT EXISTS audit_events (
		seq INTEGER PRIMARY KEY,
		prev_hash TEXT NOT NULL,
		event_hash TEXT NOT NULL,
		action TEXT NOT NULL,
		actor TEXT NOT NULL,
		target_id TEXT NOT NULL,
		details_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS audit_anchor (
		singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
		count INTEGER NOT NULL,
		hash TEXT NOT NULL
	);
```

`AuditRecord` becomes `{Seq uint64; PrevHash, EventHash, Action, Actor, TargetID, DetailsJSON, CreatedAt string}` with `created_at` stored as the RFC3339Nano string that is also the hashed field (so the hashed bytes and the stored bytes are the same bytes). Accessors:

```go
func (d *DB) InsertAuditEventAndAnchor(ctx context.Context, ar AuditRecord, count uint64, hash string) error // one transaction
func (d *DB) GetLastAuditEvent(ctx context.Context) (*AuditRecord, error)
func (d *DB) GetAuditAnchor(ctx context.Context) (count uint64, hash string, found bool, err error)
func (d *DB) IterAuditEvents(ctx context.Context) iter.Seq2[AuditRecord, error] // ascending by seq, no limit
func (d *DB) ListAuditEvents(ctx context.Context, limit int) ([]AuditRecord, error) // kept for the UI, descending
```

`InsertAuditEventAndAnchor` runs `INSERT INTO audit_events ...` and `INSERT INTO audit_anchor (singleton, count, hash) VALUES (1, ?, ?) ON CONFLICT(singleton) DO UPDATE SET count = excluded.count, hash = excluded.hash` inside one `BeginTx`/`Commit`.

- [ ] **Step 3: The ledger**

`internal/audit/chain.go` (and delete the hashing code from `ledger.go`, keeping `Logger`, `sanitizeActor`, and the constructor's shape):

```go
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/Busness-app/ky-primitives/auditchain"
	"github.com/Busness-app/kyrecovery-server/internal/db"
)

// fields is the order every record is hashed in. Changing it changes every digest.
const (
	fAction = iota
	fActor
	fTarget
	fDetails
	fCreated
	fieldCount
)

type Ledger struct {
	mu    sync.Mutex
	db    *db.DB
	key   []byte
	chain *auditchain.Chain
	err   error // set when the stored log does not match its anchor; every Record fails
}

func NewLedger(database *db.DB) *Ledger {
	l := &Ledger{db: database}
	if database == nil {
		return l
	}
	l.key = database.Keyring().LedgerKey()
	if err := l.resume(context.Background()); err != nil {
		l.err = err
	}
	return l
}

func (l *Ledger) resume(ctx context.Context) error {
	count, hash, found, err := l.db.GetAuditAnchor(ctx)
	if err != nil {
		return err
	}
	if !found {
		l.chain, err = auditchain.New(l.key)
		return err
	}
	last, err := l.db.GetLastAuditEvent(ctx)
	if err != nil {
		return err
	}
	if last == nil || last.Seq != count || last.EventHash != hash {
		return errors.New("audit log does not match its anchor; refusing to append")
	}
	l.chain, err = auditchain.Resume(l.key, toRecord(*last), auditchain.Anchor{Count: count, Hash: hash})
	return err
}

func toRecord(ar db.AuditRecord) auditchain.Record {
	return auditchain.Record{Seq: ar.Seq, Prev: ar.PrevHash, Hash: ar.EventHash,
		Fields: []string{ar.Action, ar.Actor, ar.TargetID, ar.DetailsJSON, ar.CreatedAt}}
}

func (l *Ledger) Record(ctx context.Context, action, actor, targetID string, details map[string]interface{}) (*db.AuditRecord, error) {
	if l.db == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	if details == nil {
		details = map[string]interface{}{}
	}
	dj, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	var out db.AuditRecord
	_, err = l.chain.Append(ctx, func(r auditchain.Record, a auditchain.Anchor) error {
		out = db.AuditRecord{Seq: r.Seq, PrevHash: r.Prev, EventHash: r.Hash, Action: action, Actor: sanitizeActor(actor),
			TargetID: targetID, DetailsJSON: string(dj), CreatedAt: created}
		return l.db.InsertAuditEventAndAnchor(ctx, out, a.Count, a.Hash)
	}, action, sanitizeActor(actor), targetID, string(dj), created)
	if err != nil {
		return nil, fmt.Errorf("audit append: %w", err)
	}
	return &out, nil
}

// Verify streams the whole log against the anchor. It does not page: kyrecovery's previous
// verifier read a fixed 100000 rows and reported a gap on a healthy chain past that.
func (l *Ledger) Verify(ctx context.Context) (auditchain.Anchor, error) {
	count, hash, found, err := l.db.GetAuditAnchor(ctx)
	if err != nil {
		return auditchain.Anchor{}, err
	}
	anchor := auditchain.Anchor{Count: count, Hash: hash}
	if !found {
		anchor = auditchain.Anchor{Count: 0, Hash: auditchain.Genesis()}
	}
	records := func(yield func(auditchain.Record, error) bool) {
		for ar, err := range l.db.IterAuditEvents(ctx) {
			if !yield(toRecord(ar), err) {
				return
			}
		}
	}
	return anchor, auditchain.VerifyStream(l.key, iter.Seq2[auditchain.Record, error](records), anchor)
}
```

Check `go doc github.com/Busness-app/ky-primitives/auditchain` for the genesis hash's exported name; if there is no `Genesis()` function, use the exported constant the package provides, or `auditchain.New(key).Anchor()` on a fresh chain to obtain the empty anchor. Do not hard-code the zero string.

`handleAuditVerify` becomes:

```go
	anchor, err := s.ledger.Verify(r.Context())
	resp := map[string]interface{}{"valid": err == nil, "count": anchor.Count, "last_hash": anchor.Hash}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
```

`cmdAudit`'s verify path in `app.go` calls `ledger.Verify` the same way. Delete `secrets.KeyedMarkerName`, `LedgerKeyed`, `MarkLedgerKeyed` and the `ledger.keyed` marker logic; there is no legacy chain to convert.

- [ ] **Step 4: Gate and commit**

```bash
gofmt -l $(git ls-files '*.go'); go vet ./... && go test -race -count=1 ./...
git add -A && git commit -m "refactor(audit): chain events with ky-primitives/auditchain and verify against a stored anchor"
```

---

### Task 6: Passwords and the keyring master key through the library

**Files:**
- Modify: `internal/auth/auth.go` (local password hash + verify), `internal/db/db.go` (users table hash columns), `internal/secrets/secrets.go:26-60` (`Load`)
- Tests: `internal/auth/auth_test.go`, `internal/db/secrets_test.go`

**Interfaces:**
- Consumes: `password.Hash(plaintext string) (string, error)`, `password.Verify(plaintext, encoded string) (bool, error)`, `keyfile.FromEnv(name string, size int) ([]byte, bool, error)`, `keyfile.LoadOrCreate(path string, size int) ([]byte, error)`.

- [ ] **Step 1: Read the current password storage**

```bash
grep -n "password_hash\|password_salt\|PasswordHash\|PasswordSalt\|argon2\|scrypt" internal/auth/auth.go internal/db/db.go | head -30
```

The spec says hashes are hex hash + hex salt in two columns. Rewrite the users DDL to a single `password_hash TEXT NOT NULL DEFAULT ''` (PHC string), drop the salt column, and route every hash/verify through `password.Hash`/`password.Verify`. `EnsureAdminUser` hashes with `password.Hash`. Delete the local Argon2 call and its parameter constants. Update the auth tests to assert a login round trip, not a hash format.

- [ ] **Step 2: Keyring via keyfile**

In `secrets.Load(dataDir)`:

```go
	if key, ok, err := keyfile.FromEnv(EnvKey, 32); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvKey, err)
	} else if ok {
		return &Keyring{master: key, dir: dataDir}, nil
	}
	if dataDir == "" {
		return Ephemeral()
	}
	key, err := keyfile.LoadOrCreate(filepath.Join(dataDir, KeyFileName), 32)
	if err != nil {
		return nil, err
	}
	return &Keyring{master: key, dir: dataDir}, nil
```

`keyfile.LoadOrCreate` writes hex with owner-only permissions and refuses an unreadable existing file; delete the hand-rolled file read/write and permission check that `Load` had. If the existing `secret.key` was raw bytes rather than hex, that is a format change; nothing is in the wild, so document "delete `secret.key` and let it regenerate" in the README, and use `keyfile.LoadOrCreateEncoded(path, 32, keyfile.Hex)` explicitly so the spelling is visible.

- [ ] **Step 3: Gate and commit**

```bash
gofmt -l $(git ls-files '*.go'); go vet ./... && go test -race -count=1 ./...
git add -A && git commit -m "refactor(auth,secrets): PHC passwords via ky-primitives/password; keyring master key via keyfile"
```

---

### Task 7: The browser ceremony

**Files:**
- Create: `cmd/ceremony-wasm/main.go`, `scripts/build-wasm.sh`, `scripts/test-wasm.mjs`, `internal/server/static/ceremony.html`, `internal/server/static/js/ceremony.js`, `internal/server/static/wasm/ceremony.wasm`, `internal/server/static/wasm/wasm_exec.js`
- Modify: `internal/server/server.go` (`routes` for `/admin/ceremony`, MIME for `.wasm`), `internal/server/static/index.html` (a link from the Custodians tab), `.github/workflows/ci.yml` (build + node test + `git diff --exit-code` on the artefact)

**Interfaces:**
- Consumes: `recoverykey.Generate() (PrivateKey, error)`, `recoverykey.Split(k PrivateKey, threshold, total int) ([]shamir.Share, error)`, `shamir.Share.String()`, `PrivateKey.Public()`, `PublicKey.Bytes()`, `PublicKey.ID()`.
- Produces: a JS global `kyCeremony(threshold, total)` returning `{key_id, public_key_b64, shares: [..]}` or `{error}`; page at `/admin/ceremony` (admin session required — it is not under `/api/`, so add the check in the handler with `s.authMgr.GetSession` and `auth.RoleRank`).

- [ ] **Step 1: The WASM command**

`cmd/ceremony-wasm/main.go`:

```go
//go:build js && wasm

// ceremony-wasm is the only code in this repository that ever holds the suite recovery
// private key, and it runs in an operator's browser tab, never in the server. It exposes one
// function to the page: generate a keypair, split the seed, return the cards' contents and
// the public half. Nothing here can write to the network or to disk.
package main

import (
	"encoding/base64"
	"syscall/js"

	"github.com/Busness-app/ky-primitives/recoverykey"
)

func ceremony(_ js.Value, args []js.Value) any {
	if len(args) != 2 {
		return map[string]any{"error": "kyCeremony(threshold, total)"}
	}
	k, n := args[0].Int(), args[1].Int()
	priv, err := recoverykey.Generate()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	shares, err := recoverykey.Split(priv, k, n)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	out := make([]any, len(shares))
	for i, s := range shares {
		out[i] = s.String()
	}
	pub := priv.Public()
	return map[string]any{
		"key_id":         pub.ID(),
		"public_key_b64": base64.StdEncoding.EncodeToString(pub.Bytes()),
		"threshold":      k,
		"total_shares":   n,
		"shares":         out,
	}
}

func main() {
	js.Global().Set("kyCeremony", js.FuncOf(ceremony))
	select {}
}
```

`scripts/build-wasm.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o internal/server/static/wasm/ceremony.wasm ./cmd/ceremony-wasm
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" internal/server/static/wasm/wasm_exec.js
ls -la internal/server/static/wasm/
```

`scripts/test-wasm.mjs` (run with `node scripts/test-wasm.mjs`):

```js
import fs from 'node:fs';
import { webcrypto } from 'node:crypto';
globalThis.crypto ??= webcrypto;
globalThis.fs = fs;
await import('../internal/server/static/wasm/wasm_exec.js');
const go = new Go();
const { instance } = await WebAssembly.instantiate(fs.readFileSync('internal/server/static/wasm/ceremony.wasm'), go.importObject);
go.run(instance); // returns when main blocks on select{}; kyCeremony is now registered
const r = globalThis.kyCeremony(3, 5);
if (r.error) throw new Error(r.error);
if (!/^[0-9a-f]{64}$/.test(r.key_id)) throw new Error('key_id ' + r.key_id);
if (Buffer.from(r.public_key_b64, 'base64').length !== 1216) throw new Error('public key length');
if (r.shares.length !== 5 || !r.shares.every(s => s.startsWith('ky2-'))) throw new Error('shares ' + JSON.stringify(r.shares));
console.log('ceremony.wasm OK', r.key_id);
process.exit(0);
```

If `go.run` does not return because `main` blocks, call `go.run(instance)` without awaiting and read `globalThis.kyCeremony` on the next tick (`await new Promise(r => setTimeout(r, 0))`). The spike that proved this used exactly that shape.

- [ ] **Step 2: A Go test that the shares reconstruct the key**

The Node test proves shape; this proves the shares are real. `cmd/ceremony-wasm/ceremony_test.go` cannot run under the `js && wasm` build tag on the host, so put the property test in `internal/server/ceremony_test.go` as a normal test that exercises the same library calls:

```go
package server_test

import (
	"testing"

	"github.com/Busness-app/ky-primitives/recoverykey"
	"github.com/Busness-app/ky-primitives/shamir"
)

// The WASM module calls exactly Generate and Split. This test pins that any k of the n
// share strings it prints reconstruct the key whose public half it posts. It is the only
// place outside a product where the recovery private key is combined, and it is a test.
func TestCeremonySharesReconstructTheKey(t *testing.T) {
	priv, _ := recoverykey.Generate()
	shares, err := recoverykey.Split(priv, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	var cards []shamir.Share
	for _, i := range []int{0, 2, 4} { // non-consecutive on purpose
		s, err := shamir.ParseShare(shares[i].String())
		if err != nil {
			t.Fatal(err)
		}
		cards = append(cards, s)
	}
	got, err := recoverykey.Combine(cards)
	if err != nil {
		t.Fatal(err)
	}
	if got.Public().ID() != priv.Public().ID() {
		t.Fatal("combined key does not match the published public key")
	}
}
```

- [ ] **Step 3: The page**

`internal/server/static/ceremony.html`:

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Recovery key ceremony</title>
  <link rel="stylesheet" href="/static/css/styles.css">
  <meta http-equiv="Cache-Control" content="no-store">
</head>
<body>
<main class="container" style="max-width: 900px; margin: 40px auto;">
  <h1>Recovery key ceremony</h1>
  <div class="panel">
    <p><strong>Read before you begin.</strong> This tab will generate the suite's recovery private key and split it into custodian cards. The key exists only in this tab's memory and cannot be erased before the tab closes. Run this in a <strong>fresh private window</strong>, with <strong>no browser extensions</strong>, on a machine that will <strong>not hibernate</strong> during the ceremony. Print the cards, confirm the import, then <strong>close the tab</strong>. Nothing but the public key leaves this page.</p>
  </div>
  <form id="ceremony-form" class="panel">
    <label>Custodians needed to recover (k) <input id="k" type="number" min="2" max="255" value="3" required></label>
    <label>Cards to print (n) <input id="n" type="number" min="2" max="255" value="5" required></label>
    <button class="btn btn-primary" type="submit">Generate and split</button>
  </form>
  <section id="cards" hidden>
    <div class="panel">
      <p>Key ID <code id="key-id"></code></p>
      <button class="btn btn-secondary" onclick="window.print()">Print cards</button>
      <button class="btn btn-primary" id="import-btn">Import public key into kyrecovery</button>
      <p id="import-status"></p>
    </div>
    <div id="card-list"></div>
  </section>
  <p id="error" class="status-pill error" hidden></p>
</main>
<script src="/static/wasm/wasm_exec.js"></script>
<script src="/static/js/ceremony.js"></script>
</body>
</html>
```

`internal/server/static/js/ceremony.js`:

```js
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

let ready = (async () => {
  const go = new Go();
  const { instance } = await WebAssembly.instantiateStreaming(fetch('/static/wasm/ceremony.wasm'), go.importObject);
  go.run(instance);
})();

let result = null;

document.getElementById('ceremony-form').addEventListener('submit', async ev => {
  ev.preventDefault();
  await ready;
  const k = Number(document.getElementById('k').value), n = Number(document.getElementById('n').value);
  const r = globalThis.kyCeremony(k, n);
  const err = document.getElementById('error');
  if (r.error) { err.textContent = r.error; err.hidden = false; return; }
  err.hidden = true;
  result = r;
  document.getElementById('key-id').textContent = r.key_id;
  document.getElementById('card-list').innerHTML = r.shares.map((s, i) => `
    <div class="panel card">
      <h3>Custodian card ${i + 1} of ${n} &mdash; ${k} needed</h3>
      <p>Recovery key <code>${esc(r.key_id)}</code></p>
      <p>Share <code style="font-size:1.1em">${esc(s)}</code></p>
      <p>Custodian: ____________________ &nbsp; Date: ${new Date().toISOString().slice(0, 10)}</p>
    </div>`).join('');
  document.getElementById('cards').hidden = false;
  document.getElementById('ceremony-form').hidden = true;
});

document.getElementById('import-btn').addEventListener('click', async () => {
  if (!result) return;
  const status = document.getElementById('import-status');
  // Only these three fields are sent. The shares are never in this request.
  const res = await fetch('/api/recovery-key', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ public_key: result.public_key_b64, threshold: result.threshold, total_shares: result.total_shares }),
  });
  const body = await res.json().catch(() => ({}));
  status.textContent = res.ok ? `Imported key ${body.key_id}. Print the cards, then close this tab.` : `Import failed: ${body.error || res.status}`;
});
```

`server.go` `routes()`:

```go
	s.mux.HandleFunc("/admin/ceremony", func(w http.ResponseWriter, r *http.Request) {
		session, err := s.authMgr.GetSession(r.Context(), r)
		if err != nil || session == nil || auth.RoleRank(session.Role) < auth.RoleRank(auth.RoleAdmin) {
			http.Error(w, "admin session required", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		data, err := staticFS.ReadFile("static/ceremony.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
```

The `/static/` file server must serve `.wasm` as `application/wasm`; Go's `mime` package knows the extension since 1.13, so `http.FileServer` sets it. Confirm with a test request. The CSP at `server.go` (`contentSecurityPolicy`) must allow `wasm-unsafe-eval` for this page: set `Content-Security-Policy: default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' data:` on the ceremony response only, overriding the global header for that path.

`index.html`, Custodians tab: add `<a class="btn btn-primary" href="/admin/ceremony" target="_blank" rel="noopener">Run recovery key ceremony</a>` in the panel header, and show the imported key ID via `GET /api/recovery-key` in `app.js` (`loadRecoveryKey()`), with "No recovery key — run the ceremony" when it 404s.

- [ ] **Step 4: Build, test, CI**

```bash
scripts/build-wasm.sh && node scripts/test-wasm.mjs
go test -race -count=1 ./internal/server/ -run 'Ceremony|RecoveryKey'
```

Add to `ci.yml`'s `go` job, after `go test`:

```yaml
      - uses: actions/setup-node@v7
        with:
          node-version: 22
      - run: scripts/build-wasm.sh && git diff --exit-code -- internal/server/static/wasm
      - run: node scripts/test-wasm.mjs
```

`-trimpath` and `-ldflags="-s -w"` make the build reproducible enough for the diff check; if CI's `wasm_exec.js` differs from the committed one because of a Go patch version, pin the CI Go version to the exact `go.mod` version (`go-version-file` already does) and note it in the report if it still drifts.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(ceremony): generate and split the suite recovery key in the operator's browser; import only the public half"
```

---

### Task 8: Docs, the no-decrypt guard, README

**Files:**
- Create: `internal/server/nodecrypt_test.go`
- Modify: `README.md`, `AGENTS.md`, `zero_code_pairing_handoff_spec.md` (claim response), `docker-compose.yml`

- [ ] **Step 1: The guard**

`internal/server/nodecrypt_test.go`:

```go
package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kyrecovery is a blind store. Nothing linked into the server may be able to open a capsule
// or hold the recovery private key. The WASM command is excluded because it never links
// into the server binary; test files are excluded because tests are where keys are allowed.
func TestNothingInTheServerDecrypts(t *testing.T) {
	forbidden := []string{"recoverykey.Generate(", "recoverykey.Split(", "recoverykey.Combine(", "recoverykey.FromSeed(", "capsule.Open(", "capsule.Seal(", "hpke.NewRecipient("}
	root, _ := filepath.Abs("../..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" || path == filepath.Join(root, "cmd", "ceremony-wasm") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, f := range forbidden {
			if strings.Contains(string(src), f) {
				t.Errorf("%s calls %s; the server must not be able to decrypt", path, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Docs**

`README.md`: rewrite the overview to the blind-store model; document `POST /api/backup/deposit` (octet-stream, bearer token, 413/400/409/403 meanings), the claim response's three new fields, `GET/POST /api/recovery-key`, `/admin/ceremony` and its tab rules, `GET /api/capsules/{id}/verify`, the daily sweep, the CLI (`serve`, `audit`, `pair generate|list|claim`), env vars (`KYRECOVERY_SECRET_KEY` hex/base64 via keyfile, `KY_*` SSO, `KY_ADMIN_INITIAL_PASSWORD`, `KYRECOVERY_COOKIE_SECURE`), and "Upgrading from the previous kyrecovery: delete `data/`; nothing was in the wild". Remove every mention of push, capture, drills, export kits and share responses. `AGENTS.md`: package index matches `internal/*`. `zero_code_pairing_handoff_spec.md`: the claim response section gains the three fields and the 409-before-ceremony rule. `docker-compose.yml`: remove `KYRECOVERY_MAX_BACKUP_BYTES`; comment that `data/` holds `secret.key`, the DB and `capsules/`.

- [ ] **Step 3: Final gate**

```bash
gofmt -l $(git ls-files '*.go'); go mod tidy && git diff --exit-code go.mod go.sum && go mod verify
go vet ./... && go test -race -count=1 ./...
scripts/build-wasm.sh && git diff --exit-code -- internal/server/static/wasm && node scripts/test-wasm.mjs
docker build -t kyrecovery:local . && docker run --rm kyrecovery:local help
grep -rn "backup/push\|export-kit\|SelfDeclaredBackupPayload\|MasterKey\|value_hex" --include='*.go' --include='*.html' --include='*.js' --include='*.md' . | grep -v node_modules
```

Expected: everything clean; the grep prints nothing.

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "docs: kyrecovery as a blind store; pin that nothing in the server decrypts"
```

---

## What is deliberately not in this plan

- The share relay (spec non-goal). The recovering instance gets shares from custodians directly.
- Key rotation. Both the product's `keyfile.Store` and the single-row import refuse a second key; a rotation procedure is a later document.
- Streaming deposits. A capsule over `capsule.MaxContainerBytes` is refused; the streaming container is Plan 2.
- Migrating existing audit events, capsules, passwords or the `secret.key` format. Nothing is in the wild; `data/` is deleted on upgrade.

## Self-review

- Spec coverage: Part 1 (Task 1, 6); Part 2 ceremony (Task 7, import in Task 2); Part 3 pairing (Task 3); Part 4 deposit (Task 3); Part 5 attestation + auditchain + inspector (Tasks 4, 5, 1); Part 6 deletion (Task 1); Part 7 restore is product-side, nothing to build; Part 8 errors are each a named refusal in Tasks 2–4; Part 9 tests 1–9 map to Tasks 3, 3, 3, 2, 3+7 (round trip is `TestDepositAcceptsACapsuleSealedToThePinnedKey` plus `TestCeremonySharesReconstructTheKey`), 4, 7, 5, 8.
- Names used across tasks: `newAdminServer`, `importKey`, `pairProduct`, `sealFor`, `deposit` (Task 2/3 test helpers, defined once in `recoverykey_test.go` and `deposit_test.go`, same package); `db.RecoveryKeyRecord`, `GetRecoveryKey`, `InsertRecoveryKey`, `ErrRecoveryKeyExists`; `db.CapsuleRecord` fields as listed in Task 3; `ClaimPairingCode` five-arg form; `s.verifyCapsule`, `s.verifyAll`, `SetCapsuleStatus`; `Ledger.Record` unchanged, `Ledger.Verify` new; `kyCeremony` JS global; `capsule.MaxContainerBytes`.
- Plan defects to watch during execution: the exact sqlite unique-violation string (Task 2 Step 3); the `auditchain` genesis accessor name (Task 5 Step 3); `go.run` blocking behaviour under Node (Task 7 Step 1). Each is called out where it occurs with the check to make.
