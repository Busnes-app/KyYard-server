# ky_server_base Adopts ky-primitives v0.4.0 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The scaffold stops carrying its own crypto and backup primitives and uses `github.com/Busness-app/ky-primitives v0.4.0` for password hashing, TOTP, recovery codes, key files, and capsules sealed to the suite recovery public key.

**Architecture:** Every product is forked from this scaffold, so it moves first and gridlock is re-forked from it afterwards (companion plan `2026-09-03-gridlock-refork.md`). The product never holds a recovery private key: it receives the suite public key at pairing, stores it with `keyfile.Store`, seals every capsule to it with `capsule.Seal`, and restores with `capsule.Open` only when custodians hand it shares. The restore drill proves the pipeline against a throwaway keypair; the export endpoint hands the operator the sealed `.kycap` itself, not shares.

**Tech Stack:** Go 1.26.6, stdlib, `github.com/Busness-app/ky-primitives v0.4.0` (public module, `golang.org/x/crypto` only), SQLite via `modernc.org/sqlite`, Postgres via `pgx`.

**Spec:** `/home/yoshi/busness.app/ky-primitives/docs/superpowers/specs/2026-09-02-suite-migration-design.md` (Phase 3, lines 275–311; and "One suite-wide recovery keypair", lines 36–53) and `/home/yoshi/busness.app/ky-primitives/docs/superpowers/specs/2026-09-03-recovery-keypair-design.md` (Part 4, "At pairing, per product" and "Every backup, per product").

## Global Constraints

- Go floor is `go 1.26.6` in `go.mod`; do not raise it. `crypto/hpke` is stdlib in 1.26, which is why the library needs no KEM dependency.
- Pin `github.com/Busness-app/ky-primitives v0.4.0` exactly. The module is **public**, so no `GOPRIVATE` is needed (the spec's line 300 assumed otherwise; this plan corrects it).
- **Nothing is in the wild.** No installed base of password hashes, TOTP secrets, recovery codes, or capsules exists for this scaffold. Formats change without migration paths. If that is ever found false for a deployment, stop and design one.
- The product **never** holds `recoverykey.PrivateKey` except transiently in the restore CLI, from shares typed by custodians, and in the drill, from a throwaway `Generate()`.
- `capsule.Open` proves integrity and binding to this key, not origin or freshness. Anything that acts on a manifest compares `ServiceName` to the configured app name first.
- CI (`.github/workflows/ci.yml`, job `go`) runs `go mod tidy && git diff --exit-code go.mod go.sum`, gofmt, vet, `go test -race`. The `web` job runs `npm run build && git diff --exit-code -- web/dist`, so any change under `web/src` must be accompanied by a rebuilt, committed `web/dist`.
- Commit after every task. Commit messages: `type(scope): imperative sentence`, no trailer other than the co-author line the harness adds.
- Tests are `package xxx_test` where the existing file is; keep the existing files' package clauses.

## Decisions this plan takes beyond the spec

These are recorded so the executor does not re-decide them and the reviewer can check them.

| Decision | Choice | Why |
|---|---|---|
| What the restore drill opens with | A throwaway `recoverykey.Generate()` keypair, sealed and opened in the same call | The product has no private key. The drill proves tar/seal/open/extract/recipe end to end; a separate check proves the suite public key is present and its ID matches the pinned ID. |
| What `/api/backup/export-kit` becomes | `/api/backup/export-capsule`, a `.kycap` download sealed to the suite key | Shares belong to the ceremony (Plan 5), not to a product. An HTML page of shares that never unlocked a shipped payload is deleted. |
| Where the public key lives | `<DataDir>/recovery.pub`, raw 1216 bytes, via `keyfile.Store`; key ID and k/n in `server_settings` | `Store` refuses overwrite, which is the anti-substitution property. Re-pairing to a *different* key is an explicit operator action (delete the file), not an API call. |
| Pairing response shape | `/api/pairing/claim` must return `recovery_public_key` (std base64, 1216 bytes), `threshold`, `total_shares` alongside `api_token` | The spec says the key arrives at pairing over the authenticated channel. A claim without it is not a completed pairing, so it fails closed. kyrecovery does not send this yet; pairing is red until Plan 5 lands. Recorded, expected. |
| `Threshold`/`TotalShares` a product passes to `Seal` | The values received at pairing | The manifest fields describe the suite seed's split (spec line 146). The product cannot invent them. |
| `KY_ENCRYPTION_KEY` | Still honoured, via `keyfile.FromEnv`; otherwise `keyfile.LoadOrCreate(<DataDir>/encryption.key, 32)` in every environment | Fixes the live bug where a dev instance mints a fresh key per restart and orphans `users.totp_secret_enc`. The production-only "required" error goes: a persistent file is a valid production configuration. |
| `crypto.EncryptAESGCM` key type | `[]byte`, exactly 32 bytes, or an error | `parseKey`'s SHA-256 fallback silently accepted any string as a key. It is deleted. |
| `KY_SESSION_SECRET` | Unchanged | Out of Phase 3 scope; a rotating session secret logs users out, it does not lose data. Noted for later. |
| Recovery-code digest | `hex(sha256(recoverycode.Normalize(code)))`, redeemed slot blanked to `""` | The library leaves the hash to the product. Same primitive as today, applied to the normalised form so both sides agree, stored as before in `users.recovery_codes_hash`. |
| TOTP replay | New column `users.totp_last_counter`, compare-and-swap on spend | The reason for adopting `totp.Validate`'s counter at all. |

---

## File structure

**Delete**
- `internal/backup/shamir.go`, `internal/backup/shamir_vectors_test.go`, `testdata/shamir-vectors.json` — the vectors are 0x11d and the library's `ParseShare` accepts only `ky2-` strings; nothing to keep.
- `internal/backup/recovery_kit.go` — replaced by the capsule download.
- `internal/auth/totp.go`, `internal/auth/recovery.go` — bodies become one-line delegations; the files stay but shrink.

**Create**
- `internal/backup/recoverykey.go` — load/store the suite public key and the pinned metadata (`RecoveryKey`, `LoadRecoveryKey`, `StoreRecoveryKey`).
- `internal/backup/recoverykey_test.go`
- `internal/backup/capsule_test.go` — the drill and seal tests, replacing the Shamir-shaped ones in `backup_test.go`.
- `.github/workflows/ky-primitives-compat.yml` — early-warning build against the library's default branch, copied from gridlock.

**Modify**
- `go.mod`, `go.sum`
- `internal/config/config.go` — `EncryptionKey []byte`, keyfile-backed.
- `internal/crypto/crypto.go` — delete `HashPassword`, `VerifyPassword`, `parseKey`; `EncryptAESGCM`/`DecryptAESGCM` take `[]byte`.
- `internal/crypto/crypto_test.go`
- `internal/api/auth_handlers.go` — password verify, TOTP counter spend, recovery-code redeem.
- `internal/api/api_test.go`, `internal/api/authz_test.go` — `crypto.HashPassword` → `password.Hash`.
- `cmd/server/main.go` — `init-admin` hashing, `backup-drill`, `export-capsule`, new `restore`.
- `internal/auth/totp.go`, `internal/auth/recovery.go`, `internal/auth/auth_test.go`
- `internal/store/store.go`, `internal/store/sqlstore.go`, `internal/store/models.go`, `internal/store/migrations/migrations.go` — `totp_last_counter`, `SpendTOTPCounter`.
- `internal/backup/capsule.go` — becomes a thin layer over `capsule.Seal`/`capsule.Open`.
- `internal/backup/drill.go` — signature change, pinned-key check.
- `internal/backup/client.go` — `ClaimPairing` returns the pairing result struct.
- `internal/api/backup_handlers.go`, `internal/api/server.go`
- `web/src/pages/Backup.tsx`, `web/dist/**`
- `scripts/ky-init.sh`
- `internal/backup/AGENTS.md`, `IMPLEMENTATION_PLAN.md`

---

### Task 1: Encryption key from a key file, and AES-GCM takes bytes

**Files:**
- Modify: `go.mod`, `go.sum`
- Modify: `internal/config/config.go:44-50` (SecurityConfig), `:115-121` (key mint)
- Modify: `internal/crypto/crypto.go:100-178`
- Modify: `internal/crypto/crypto_test.go:26-50`
- Modify: `internal/config/config_test.go`
- Modify: `internal/api/auth_handlers.go:160`
- Modify: `internal/backup/capsule.go:120,143` (temporary; Task 6 deletes these calls)

**Interfaces:**
- Produces: `config.SecurityConfig.EncryptionKey []byte` (32 bytes); `crypto.EncryptAESGCM(plaintext, key []byte) (string, error)`; `crypto.DecryptAESGCM(encoded string, key []byte) ([]byte, error)`; `crypto.ErrKeyLength`.

- [ ] **Step 1: Add the dependency**

```bash
cd /home/yoshi/busness.app/ky_server_base
go get github.com/Busness-app/ky-primitives@v0.4.0
```

Expected: `go.mod` gains `github.com/Busness-app/ky-primitives v0.4.0` and `golang.org/x/crypto` moves to `v0.55.0`. Do not run `go mod tidy` yet; nothing imports it and tidy would drop it.

- [ ] **Step 2: Write the failing crypto test**

Replace `TestAESGCMEncryption` in `internal/crypto/crypto_test.go` with:

```go
func TestAESGCMEncryption(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	plaintext := []byte("totp secret")

	enc, err := crypto.EncryptAESGCM(plaintext, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	dec, err := crypto.DecryptAESGCM(enc, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(dec, plaintext) {
		t.Fatalf("round trip: got %q", dec)
	}

	other := bytes.Repeat([]byte{0x43}, 32)
	if _, err := crypto.DecryptAESGCM(enc, other); err == nil {
		t.Fatal("wrong key decrypted")
	}
}

func TestAESGCMRefusesShortKey(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := crypto.EncryptAESGCM([]byte("x"), make([]byte, n))
		if !errors.Is(err, crypto.ErrKeyLength) {
			t.Errorf("len %d: got %v, want ErrKeyLength", n, err)
		}
	}
}
```

Add `"bytes"` and `"errors"` to the imports.

- [ ] **Step 3: Run it to see it fail**

```bash
go test ./internal/crypto/ -run 'TestAESGCM' 2>&1 | head
```

Expected: compile error, `cannot use key (variable of type []byte) as string value`.

- [ ] **Step 4: Change the AES-GCM helpers**

In `internal/crypto/crypto.go`, add the sentinel next to the parameter constants:

```go
// ErrKeyLength reports an AES-256-GCM key that is not exactly 32 bytes.
var ErrKeyLength = errors.New("crypto: AES-256-GCM key must be exactly 32 bytes")
```

Replace `EncryptAESGCM`, `DecryptAESGCM` and `parseKey`:

```go
// EncryptAESGCM encrypts plaintext with AES-256-GCM under a 32-byte key and a random
// 12-byte nonce, returning nonce||ciphertext as raw base64url.
func EncryptAESGCM(plaintext, key []byte) (string, error) {
	aesGCM, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(aesGCM.Seal(nonce, nonce, plaintext, nil)), nil
}

// DecryptAESGCM reverses EncryptAESGCM.
func DecryptAESGCM(encoded string, key []byte) ([]byte, error) {
	aesGCM, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(data) < aesGCM.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ciphertext := data[:aesGCM.NonceSize()], data[aesGCM.NonceSize():]
	return aesGCM.Open(nil, nonce, ciphertext, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrKeyLength
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
```

Delete `parseKey` entirely. Remove `"encoding/hex"` from the imports if nothing else in the file uses it (`RandomHex` does — check before removing). Add `"errors"` if absent.

- [ ] **Step 5: Change the config**

In `internal/config/config.go`, `SecurityConfig`:

```go
type SecurityConfig struct {
	SessionSecret string `json:"session_secret"`
	EncryptionKey []byte `json:"-"` // 32 bytes for AES-256-GCM; never serialised
	CookieSecure  bool   `json:"cookie_secure"`
	CookieDomain  string `json:"cookie_domain"`
	SessionTTL    time.Duration
}
```

Replace lines 115–121 (the `encryptionKey` block) with:

```go
	encryptionKey, ok, err := keyfile.FromEnv("KY_ENCRYPTION_KEY", 32)
	if err != nil {
		return nil, fmt.Errorf("KY_ENCRYPTION_KEY: %w", err)
	}
	if !ok {
		encryptionKey, err = keyfile.LoadOrCreate(filepath.Join(dataDir, "encryption.key"), 32)
		if err != nil {
			return nil, fmt.Errorf("encryption key: %w", err)
		}
	}
```

Add imports `"path/filepath"` and `"github.com/Busness-app/ky-primitives/keyfile"`. `EncryptionKey: encryptionKey,` in the struct literal is unchanged.

- [ ] **Step 6: Fix the two callers**

`internal/api/auth_handlers.go:160` needs no change in shape (`s.config.Security.EncryptionKey` is now `[]byte`, which is what `DecryptAESGCM` wants).

`internal/backup/capsule.go:120`: `crypto.EncryptAESGCM(tarBytes, ephemeralKey)`. `:143`: pass `key` directly instead of `hex.EncodeToString(key)`; read the surrounding lines and drop the now-unused `hex` import if that was its only use. Task 6 deletes this code; keep it compiling for now.

- [ ] **Step 7: Config tests**

In `internal/config/config_test.go`, both tests call `config.LoadFromEnv()` and will now create `./data/encryption.key` relative to the test's cwd. Add `t.Setenv("KY_DATA_DIR", t.TempDir())` as the first line of each test, and add:

```go
func TestEncryptionKeyPersistsAcrossLoads(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	t.Setenv("KY_ENCRYPTION_KEY", "")
	a, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	b, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Security.EncryptionKey) != 32 || !bytes.Equal(a.Security.EncryptionKey, b.Security.EncryptionKey) {
		t.Fatal("encryption key was not persisted between loads")
	}
}

func TestEncryptionKeyFromEnvMustBe32Bytes(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	t.Setenv("KY_ENCRYPTION_KEY", "deadbeef")
	if _, err := config.LoadFromEnv(); err == nil {
		t.Fatal("8-byte key accepted")
	}
}
```

- [ ] **Step 8: Build and test**

```bash
gofmt -l . ; go vet ./... && go test -race -count=1 ./internal/crypto/ ./internal/config/ ./internal/backup/ ./internal/api/
```

Expected: gofmt prints nothing; all pass. `git status` must show no stray `data/` directory in the repo root; if one appeared, a test is missing `KY_DATA_DIR`.

- [ ] **Step 9: Commit**

```bash
git add go.mod go.sum internal/config internal/crypto internal/api/auth_handlers.go internal/backup/capsule.go
git commit -m "feat(config,crypto): persist the encryption key with keyfile and refuse non-32-byte AES keys"
```

---

### Task 2: Password hashing through `password`

**Files:**
- Modify: `internal/crypto/crypto.go:24-97` (delete `HashPassword`, `VerifyPassword`, `splitHash`, the Argon constants, the `argon2` import)
- Modify: `internal/crypto/crypto_test.go:9-25` (delete `TestPasswordHashingAndVerification`)
- Modify: `internal/api/auth_handlers.go:86`
- Modify: `cmd/server/main.go:65,133`
- Modify: `internal/api/api_test.go:39,133`, `internal/api/authz_test.go:22`

**Interfaces:**
- Consumes: `password.Hash(plaintext string) (string, error)`, `password.Verify(plaintext, encoded string) (bool, error)`.

- [ ] **Step 1: Write the failing handler test**

Add to `internal/api/api_test.go`:

```go
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
```

- [ ] **Step 2: Switch every hashing call**

- `internal/api/auth_handlers.go:86`:

```go
	ok, err := password.Verify(req.Password, user.PasswordHash)
	if err != nil || !ok {
		s.writeError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}
```

  Import `"github.com/Busness-app/ky-primitives/password"`. Keep the `crypto` import; `RandomHex`/`SHA256Hex` are still used.

- `cmd/server/main.go:65` and `:133`: `crypto.HashPassword(x)` → `password.Hash(x)`; add the import; remove the `crypto` import if it becomes unused.
- `internal/api/api_test.go:39,133` and `internal/api/authz_test.go:22`: `crypto.HashPassword(...)` → `password.Hash(...)`; fix imports.
- `internal/crypto/crypto.go`: delete `HashPassword`, `VerifyPassword`, `splitHash`, the five `Argon*`/`SaltLen` constants, and the `golang.org/x/crypto/argon2` import.
- `internal/crypto/crypto_test.go`: delete `TestPasswordHashingAndVerification`.

- [ ] **Step 3: Build, test, tidy**

```bash
go mod tidy && git diff --stat go.mod go.sum
gofmt -l . ; go vet ./... && go test -race -count=1 ./...
```

Expected: `golang.org/x/crypto` stays a direct require (the library needs it; tidy may mark it `// indirect`, which is fine). All tests pass, including the new one.

- [ ] **Step 4: Commit**

```bash
git add -A internal/crypto internal/api cmd/server go.mod go.sum
git commit -m "refactor(auth): hash and verify passwords with ky-primitives/password"
```

---

### Task 3: TOTP with a spent counter

**Files:**
- Modify: `internal/store/migrations/migrations.go` (append version 3)
- Modify: `internal/store/models.go:18-19`
- Modify: `internal/store/store.go:31-42` (UserStore)
- Modify: `internal/store/sqlstore.go` (SELECT column lists at `:108,161,172,183,194,283,294`, scan at `:142`, new `SpendTOTPCounter`)
- Modify: `internal/store/store_test.go`
- Modify: `internal/auth/totp.go` (replace whole file)
- Modify: `internal/auth/auth_test.go:11-27`
- Modify: `internal/api/auth_handlers.go:160-169`

**Interfaces:**
- Consumes: `totp.Validate(secret, code string, t time.Time) (int64, bool)`, `totp.GenerateSecret() (string, error)`, `totp.ProvisioningURI(issuer, account, secret string) string`.
- Produces: `store.User.TOTPLastCounter int64`; `UserStore.SpendTOTPCounter(ctx, userID string, counter int64) error` returning `store.ErrAlreadyExists` when `counter <= totp_last_counter`.

- [ ] **Step 1: Failing store test**

Add to `internal/store/store_test.go` (follow the file's existing store-opening helper; it uses `testdb`):

```go
func TestSpendTOTPCounterRefusesReplay(t *testing.T) {
	st := openTestStore(t) // whatever helper the file already uses to get a store.Store
	ctx := context.Background()
	u := &store.User{ID: "usr_t", Username: "t", Role: "user", Status: "active", SSOProvider: "local"}
	if err := st.Users().CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().SpendTOTPCounter(ctx, u.ID, 100); err != nil {
		t.Fatalf("first spend: %v", err)
	}
	if err := st.Users().SpendTOTPCounter(ctx, u.ID, 100); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("replay: got %v, want ErrAlreadyExists", err)
	}
	if err := st.Users().SpendTOTPCounter(ctx, u.ID, 99); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("older counter: got %v, want ErrAlreadyExists", err)
	}
	if err := st.Users().SpendTOTPCounter(ctx, u.ID, 101); err != nil {
		t.Fatalf("next counter: %v", err)
	}
	got, _ := st.Users().GetUserByID(ctx, u.ID)
	if got.TOTPLastCounter != 101 {
		t.Fatalf("stored counter %d, want 101", got.TOTPLastCounter)
	}
}
```

If the file has no shared helper, open the store the same way `TestUserCRUD` (or the first test in the file) does.

- [ ] **Step 2: Run it to see it fail**

```bash
go test ./internal/store/ -run TestSpendTOTPCounterRefusesReplay 2>&1 | head -5
```

Expected: `st.Users().SpendTOTPCounter undefined`.

- [ ] **Step 3: Migration, model, store**

Append to `registry` in `migrations.go`:

```go
	{
		Version:  3,
		Name:     "totp_last_counter",
		SQLite:   `ALTER TABLE users ADD COLUMN totp_last_counter INTEGER NOT NULL DEFAULT 0;`,
		Postgres: `ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_last_counter BIGINT NOT NULL DEFAULT 0;`,
	},
```

`models.go`, after `TOTPEnabled`:

```go
	TOTPLastCounter    int64      `json:"-"` // last RFC 6238 counter accepted; refuses replay inside the skew window
```

`store.go`, in `UserStore` after `UpdateRecoveryCodes`:

```go
	// SpendTOTPCounter records counter as used. It returns ErrAlreadyExists when counter is
	// not greater than the stored one, which is how a replayed code inside the skew window fails.
	SpendTOTPCounter(ctx context.Context, userID string, counter int64) error
```

`sqlstore.go`: add `totp_last_counter` to every user SELECT column list and to the `Scan` at `:142` (`&user.TOTPLastCounter` right after `&user.TOTPEnabled`). Do **not** add it to `CreateUser`'s INSERT or `UpdateUser`'s UPDATE; the only writer is:

```go
func (u *userStore) SpendTOTPCounter(ctx context.Context, userID string, counter int64) error {
	q := u.store.rebind("UPDATE users SET totp_last_counter = ?, updated_at = ? WHERE id = ? AND totp_last_counter < ?")
	res, err := u.store.db.ExecContext(ctx, q, counter, time.Now().UTC(), userID, counter)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrAlreadyExists
	}
	return nil
}
```

- [ ] **Step 4: Run the store tests**

```bash
go test -race -count=1 ./internal/store/...
```

Expected: pass. If `KY_TEST_POSTGRES_DSN` is set locally, run again with it to exercise the Postgres `ALTER`.

- [ ] **Step 5: Replace `internal/auth/totp.go`**

```go
package auth

import (
	"time"

	"github.com/Busness-app/ky-primitives/totp"
)

// GenerateTOTPSecret returns a fresh base32 RFC 6238 secret.
func GenerateTOTPSecret() (string, error) { return totp.GenerateSecret() }

// GenerateTOTPURL returns the otpauth:// URI an authenticator app enrols from.
func GenerateTOTPURL(issuer, accountName, secret string) string {
	return totp.ProvisioningURI(issuer, accountName, secret)
}

// ValidateTOTP reports whether code is valid for secret now, and the counter it matched.
// The caller must spend the counter with UserStore.SpendTOTPCounter before trusting it.
func ValidateTOTP(secret, code string) (int64, bool) {
	return totp.Validate(secret, code, time.Now())
}
```

- [ ] **Step 6: Update the auth test**

Replace `TestTOTPGenerationAndValidation` in `internal/auth/auth_test.go`:

```go
func TestTOTPValidateReturnsCounter(t *testing.T) {
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if uri := auth.GenerateTOTPURL("BusnesApp", "alice", secret); !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("uri %q", uri)
	}
	now := time.Now()
	code, err := totp.Code(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	counter, ok := auth.ValidateTOTP(secret, code)
	if !ok {
		t.Fatal("fresh code rejected")
	}
	if want := now.Unix() / totp.Period; counter < want-1 || counter > want+1 {
		t.Fatalf("counter %d not within one step of %d", counter, want)
	}
	if _, ok := auth.ValidateTOTP(secret, "000000"); ok {
		if _, ok2 := auth.ValidateTOTP(secret, "999999"); ok2 {
			t.Fatal("two arbitrary codes both valid")
		}
	}
}
```

Imports: add `"strings"`, `"time"`, `"github.com/Busness-app/ky-primitives/totp"`.

- [ ] **Step 7: Spend the counter in the handler**

`internal/api/auth_handlers.go:166-169` becomes:

```go
	counter, ok := auth.ValidateTOTP(string(secretBytes), req.Code)
	if !ok {
		s.writeError(w, http.StatusUnauthorized, "Invalid TOTP verification code")
		return
	}
	if err := s.store.Users().SpendTOTPCounter(r.Context(), user.ID, counter); err != nil {
		s.writeError(w, http.StatusUnauthorized, "TOTP code already used")
		return
	}
```

- [ ] **Step 8: Handler test for replay**

Add to `internal/api/api_test.go`. Build the user with an encrypted secret the way the existing MFA test does (look for `TOTPSecretEnc` in the file; if no test enrols TOTP, use `crypto.EncryptAESGCM([]byte(secret), cfg.Security.EncryptionKey)` with the `cfg` returned by `setupTestServer`). Then:

```go
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
		})
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
```

Check `MFARequest`'s JSON tags at the top of `auth_handlers.go` and `MFAChallenge`'s fields in `models.go` before relying on the names above; adjust to the real ones.

- [ ] **Step 9: Run everything**

```bash
gofmt -l . ; go vet ./... && go test -race -count=1 ./...
```

- [ ] **Step 10: Commit**

```bash
git add -A internal/store internal/auth internal/api
git commit -m "feat(auth): validate TOTP with ky-primitives/totp and spend the counter to refuse replay"
```

---

### Task 4: Recovery codes: normalised, blanked in place

**Files:**
- Modify: `internal/auth/recovery.go` (replace whole file)
- Modify: `internal/auth/auth_test.go:29-50`

**Interfaces:**
- Consumes: `recoverycode.Generate(n int) ([]string, error)`, `recoverycode.Normalize(code string) string`, `recoverycode.MatchCode(code string, digests []string, hash func(string) string) (int, bool)`.
- Produces: unchanged names `auth.GenerateRecoveryCodes(count int) ([]string, string, error)` and `auth.RedeemRecoveryCode(candidate, hashedJSON string) (string, bool)`. Handler at `auth_handlers.go:220` is untouched.

- [ ] **Step 1: Failing test**

Replace `TestRecoveryCodes` in `internal/auth/auth_test.go`:

```go
func TestRecoveryCodes(t *testing.T) {
	codes, hashedJSON, err := auth.GenerateRecoveryCodes(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 8 || len(codes[0]) != 14 { // xxxx-xxxx-xxxx
		t.Fatalf("codes %v", codes)
	}

	// Typed in upper case without dashes: still redeems.
	typed := strings.ToUpper(strings.ReplaceAll(codes[3], "-", ""))
	updated, ok := auth.RedeemRecoveryCode(typed, hashedJSON)
	if !ok {
		t.Fatalf("normalised form of %q rejected", codes[3])
	}

	// The slot is blanked, not removed: still 8 entries, one empty.
	var digests []string
	if err := json.Unmarshal([]byte(updated), &digests); err != nil {
		t.Fatal(err)
	}
	if len(digests) != 8 || digests[3] != "" {
		t.Fatalf("slot 3 not blanked in place: %v", digests)
	}

	if _, again := auth.RedeemRecoveryCode(codes[3], updated); again {
		t.Fatal("redeemed code accepted twice")
	}
	if _, other := auth.RedeemRecoveryCode(codes[4], updated); !other {
		t.Fatal("unrelated code rejected after a redemption")
	}
}
```

Import `"strings"` (already added in Task 3) and `"encoding/json"` (already present).

- [ ] **Step 2: Run to see it fail**

```bash
go test ./internal/auth/ -run TestRecoveryCodes
```

Expected: FAIL on length 14 (today's codes are `XXXX-XXXX`, 9 chars).

- [ ] **Step 3: Replace `internal/auth/recovery.go`**

```go
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/Busness-app/ky-primitives/recoverycode"
)

// digest is the product's recovery-code hash: SHA-256 of the normalised code, hex.
// recoverycode leaves the hash to the product; what it owns is the normalisation, which is
// why both GenerateRecoveryCodes and RedeemRecoveryCode go through it.
func digest(normalised string) string {
	sum := sha256.Sum256([]byte(normalised))
	return hex.EncodeToString(sum[:])
}

// GenerateRecoveryCodes returns count one-time codes and the JSON array of digests to store
// in users.recovery_codes_hash.
func GenerateRecoveryCodes(count int) (plainCodes []string, hashedJSON string, err error) {
	if count <= 0 {
		count = 8
	}
	codes, err := recoverycode.Generate(count)
	if err != nil {
		return nil, "", err
	}
	digests := make([]string, len(codes))
	for i, c := range codes {
		digests[i] = digest(recoverycode.Normalize(c))
	}
	out, err := json.Marshal(digests)
	if err != nil {
		return nil, "", err
	}
	return codes, string(out), nil
}

// RedeemRecoveryCode matches what the user typed against the stored digests and blanks the
// matching slot in place. Removing the entry instead renumbers the list, which is how two
// concurrent redemptions lose one another's write under the store's compare-and-swap.
func RedeemRecoveryCode(candidateCode, hashedJSON string) (string, bool) {
	var digests []string
	if err := json.Unmarshal([]byte(hashedJSON), &digests); err != nil {
		return "", false
	}
	i, ok := recoverycode.MatchCode(candidateCode, digests, digest)
	if !ok {
		return "", false
	}
	digests[i] = ""
	out, err := json.Marshal(digests)
	if err != nil {
		return "", false
	}
	return string(out), true
}
```

Confirm `recoverycode.MatchCode` applies `Normalize` before calling `hash` by reading its source in the module cache (`go doc -src github.com/Busness-app/ky-primitives/recoverycode MatchCode`). If it does not, call `digest(recoverycode.Normalize(x))` in the closure instead. The test above catches either way.

- [ ] **Step 4: Run**

```bash
gofmt -l . ; go vet ./internal/auth/ && go test -race -count=1 ./internal/auth/ ./internal/api/
```

- [ ] **Step 5: Commit**

```bash
git add internal/auth
git commit -m "refactor(auth): issue 12-symbol recovery codes and blank redeemed slots in place"
```

---

### Task 5: Receive and pin the suite recovery public key at pairing

**Files:**
- Create: `internal/backup/recoverykey.go`, `internal/backup/recoverykey_test.go`
- Modify: `internal/backup/client.go:71-114` (`ClaimPairing`)
- Modify: `internal/api/backup_handlers.go:88-131` (`handlePairRemoteRecovery`)
- Modify: `internal/api/api_test.go` (or `internal/api/backup_test.go` if the pairing test lives there; `grep -n pair-remote internal/api/*_test.go`)

**Interfaces:**
- Consumes: `recoverykey.ParsePublicKey([]byte) (recoverykey.PublicKey, error)`, `PublicKey.Bytes()`, `PublicKey.ID()`, `keyfile.Store(path, key, keyfile.Raw)`, `keyfile.LoadEncoded(path, size, keyfile.Raw)`, `recoverykey.PublicKeyBytes` (1216).
- Produces:

```go
// backup
type RecoveryKey struct {
	Public      recoverykey.PublicKey
	Threshold   int
	TotalShares int
}
func RecoveryKeyPath(dataDir string) string                       // <dataDir>/recovery.pub
func LoadRecoveryKey(ctx context.Context, dataDir string, settings store.SettingsStore) (RecoveryKey, error)
func StoreRecoveryKey(ctx context.Context, dataDir string, settings store.SettingsStore, k RecoveryKey) error
var ErrNotPaired = errors.New("backup: no recovery public key; pair with KyRecovery first")
var ErrRecoveryKeyMismatch = errors.New("backup: stored recovery public key does not match the pinned key ID")

type PairingResult struct {
	APIToken string
	Key      RecoveryKey
}
func (c *KyRecoveryClient) ClaimPairing(ctx context.Context, serverURL, pairingCode, appName string) (PairingResult, error)
```

  Settings keys: `kyrecovery_key_id`, `kyrecovery_threshold`, `kyrecovery_total_shares` (alongside the existing `kyrecovery_url`, `kyrecovery_token`).

- [ ] **Step 1: Failing tests for the key store**

`internal/backup/recoverykey_test.go`:

```go
package backup_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/Busness-app/ky-primitives/recoverykey"
	"github.com/Busness-app/ky_server_base/internal/backup"
	"github.com/Busness-app/ky_server_base/internal/store"
	"github.com/Busness-app/ky_server_base/internal/testdb"
)

func openSettings(t *testing.T) store.SettingsStore {
	t.Helper()
	st, err := store.Open(context.Background(), testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st.Settings()
}

func TestRecoveryKeyRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	settings := openSettings(t)

	priv, _ := recoverykey.Generate()
	want := backup.RecoveryKey{Public: priv.Public(), Threshold: 3, TotalShares: 5}
	if err := backup.StoreRecoveryKey(ctx, dir, settings, want); err != nil {
		t.Fatal(err)
	}
	got, err := backup.LoadRecoveryKey(ctx, dir, settings)
	if err != nil {
		t.Fatal(err)
	}
	if got.Public.ID() != want.Public.ID() || got.Threshold != 3 || got.TotalShares != 5 {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadRecoveryKeyUnpaired(t *testing.T) {
	_, err := backup.LoadRecoveryKey(context.Background(), t.TempDir(), openSettings(t))
	if !errors.Is(err, backup.ErrNotPaired) {
		t.Fatalf("got %v, want ErrNotPaired", err)
	}
}

func TestStoreRecoveryKeyRefusesADifferentKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	settings := openSettings(t)
	a, _ := recoverykey.Generate()
	b, _ := recoverykey.Generate()
	if err := backup.StoreRecoveryKey(ctx, dir, settings, backup.RecoveryKey{Public: a.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	err := backup.StoreRecoveryKey(ctx, dir, settings, backup.RecoveryKey{Public: b.Public(), Threshold: 2, TotalShares: 3})
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second key: got %v, want fs.ErrExist", err)
	}
	// Storing the same key again is idempotent.
	if err := backup.StoreRecoveryKey(ctx, dir, settings, backup.RecoveryKey{Public: a.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatalf("same key again: %v", err)
	}
	got, _ := backup.LoadRecoveryKey(ctx, dir, settings)
	if got.Public.ID() != a.Public().ID() {
		t.Fatal("pinned key changed")
	}
}

func TestLoadRecoveryKeyDetectsSwappedFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	settings := openSettings(t)
	a, _ := recoverykey.Generate()
	b, _ := recoverykey.Generate()
	if err := backup.StoreRecoveryKey(ctx, dir, settings, backup.RecoveryKey{Public: a.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	// Someone with filesystem access swaps the public key file.
	if err := os.Remove(backup.RecoveryKeyPath(dir)); err != nil {
		t.Fatal(err)
	}
	if err := keyfile.Store(backup.RecoveryKeyPath(dir), b.Public().Bytes(), keyfile.Raw); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.LoadRecoveryKey(ctx, dir, settings); !errors.Is(err, backup.ErrRecoveryKeyMismatch) {
		t.Fatalf("got %v, want ErrRecoveryKeyMismatch", err)
	}
}
```

Add `"os"` and `"github.com/Busness-app/ky-primitives/keyfile"` to the imports.

- [ ] **Step 2: Run to see them fail**

```bash
go test ./internal/backup/ -run 'RecoveryKey' 2>&1 | head -5
```

Expected: `undefined: backup.RecoveryKey`.

- [ ] **Step 3: Write `internal/backup/recoverykey.go`**

```go
package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"

	"github.com/Busness-app/ky-primitives/keyfile"
	"github.com/Busness-app/ky-primitives/recoverykey"
	"github.com/Busness-app/ky_server_base/internal/store"
)

var (
	ErrNotPaired           = errors.New("backup: no recovery public key; pair with KyRecovery first")
	ErrRecoveryKeyMismatch = errors.New("backup: stored recovery public key does not match the pinned key ID")
)

const (
	settingRecoveryKeyID   = "kyrecovery_key_id"
	settingThreshold       = "kyrecovery_threshold"
	settingTotalShares     = "kyrecovery_total_shares"
	recoveryPublicKeyFile  = "recovery.pub"
)

// RecoveryKey is what a product holds after pairing: the suite recovery public key and the
// custodian topology kyrecovery reported for it. There is no private half here, ever.
type RecoveryKey struct {
	Public      recoverykey.PublicKey
	Threshold   int
	TotalShares int
}

// RecoveryKeyPath is where the raw 1216-byte public key lives.
func RecoveryKeyPath(dataDir string) string {
	return filepath.Join(dataDir, recoveryPublicKeyFile)
}

// StoreRecoveryKey persists k. keyfile.Store refuses to replace an existing file, so a
// second pairing to a different key fails with fs.ErrExist; the same key again is a no-op.
func StoreRecoveryKey(ctx context.Context, dataDir string, settings store.SettingsStore, k RecoveryKey) error {
	if k.Public.IsZero() {
		return errors.New("backup: refusing to store a zero recovery public key")
	}
	if k.Threshold < 2 || k.TotalShares < k.Threshold || k.TotalShares > 255 {
		return fmt.Errorf("backup: %d-of-%d is not a custodian topology", k.Threshold, k.TotalShares)
	}
	path := RecoveryKeyPath(dataDir)
	err := keyfile.Store(path, k.Public.Bytes(), keyfile.Raw)
	if errors.Is(err, fs.ErrExist) {
		existing, lerr := keyfile.LoadEncoded(path, recoverykey.PublicKeyBytes, keyfile.Raw)
		if lerr != nil {
			return lerr
		}
		if pk, perr := recoverykey.ParsePublicKey(existing); perr != nil || pk.ID() != k.Public.ID() {
			return fmt.Errorf("%w: already paired to recovery key %s; remove %s to re-pair", err, pinnedID(existing), path)
		}
		// Same key: fall through and refresh the settings.
	} else if err != nil {
		return err
	}
	if err := settings.SetSetting(ctx, settingRecoveryKeyID, k.Public.ID()); err != nil {
		return err
	}
	if err := settings.SetSetting(ctx, settingThreshold, strconv.Itoa(k.Threshold)); err != nil {
		return err
	}
	return settings.SetSetting(ctx, settingTotalShares, strconv.Itoa(k.TotalShares))
}

func pinnedID(raw []byte) string {
	pk, err := recoverykey.ParsePublicKey(raw)
	if err != nil {
		return "(unparseable)"
	}
	return pk.ID()
}

// LoadRecoveryKey reads the public key file and checks it against the pinned key ID in the
// settings table, so a swapped file is detected before anything is sealed to it.
func LoadRecoveryKey(ctx context.Context, dataDir string, settings store.SettingsStore) (RecoveryKey, error) {
	raw, err := keyfile.LoadEncoded(RecoveryKeyPath(dataDir), recoverykey.PublicKeyBytes, keyfile.Raw)
	if errors.Is(err, fs.ErrNotExist) {
		return RecoveryKey{}, ErrNotPaired
	}
	if err != nil {
		return RecoveryKey{}, err
	}
	pk, err := recoverykey.ParsePublicKey(raw)
	if err != nil {
		return RecoveryKey{}, err
	}
	id, err := settings.GetSetting(ctx, settingRecoveryKeyID)
	if errors.Is(err, store.ErrNotFound) {
		return RecoveryKey{}, ErrNotPaired
	}
	if err != nil {
		return RecoveryKey{}, err
	}
	if id != pk.ID() {
		return RecoveryKey{}, ErrRecoveryKeyMismatch
	}
	k := RecoveryKey{Public: pk}
	if k.Threshold, err = intSetting(ctx, settings, settingThreshold); err != nil {
		return RecoveryKey{}, err
	}
	if k.TotalShares, err = intSetting(ctx, settings, settingTotalShares); err != nil {
		return RecoveryKey{}, err
	}
	return k, nil
}

func intSetting(ctx context.Context, settings store.SettingsStore, key string) (int, error) {
	v, err := settings.GetSetting(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return strconv.Atoi(v)
}
```

Check how `keyfile.LoadEncoded` reports a missing file (`go doc github.com/Busness-app/ky-primitives/keyfile LoadEncoded`; its `Load` sibling documents the behaviour). If it wraps `fs.ErrNotExist`, the `errors.Is` above works; if it returns its own sentinel for "missing", match that instead. The `TestLoadRecoveryKeyUnpaired` test decides.

- [ ] **Step 4: Run the key tests**

```bash
go test -race -count=1 ./internal/backup/ -run RecoveryKey
```

Expected: 4 pass.

- [ ] **Step 5: `ClaimPairing` returns the key**

In `internal/backup/client.go`, add above `ClaimPairing`:

```go
// PairingResult is what a completed pairing yields: the bearer token for deposits and the
// suite recovery public key with its custodian topology. A claim that returns no key is not a
// completed pairing.
type PairingResult struct {
	APIToken string
	Key      RecoveryKey
}
```

Change the signature to `func (c *KyRecoveryClient) ClaimPairing(ctx context.Context, serverURL, pairingCode, appName string) (PairingResult, error)`; every early `return "", err` becomes `return PairingResult{}, err`. Replace the response decoding (lines 102–114):

```go
	var claimResp struct {
		APIToken          string `json:"api_token"`
		Status            string `json:"status"`
		RecoveryPublicKey string `json:"recovery_public_key"` // std base64 of 1216 bytes
		Threshold         int    `json:"threshold"`
		TotalShares       int    `json:"total_shares"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&claimResp); err != nil {
		return PairingResult{}, err
	}
	if claimResp.APIToken == "" {
		return PairingResult{}, errors.New("empty api_token in claim response")
	}
	pkBytes, err := base64.StdEncoding.DecodeString(claimResp.RecoveryPublicKey)
	if err != nil {
		return PairingResult{}, fmt.Errorf("recovery_public_key: %w", err)
	}
	pk, err := recoverykey.ParsePublicKey(pkBytes)
	if err != nil {
		return PairingResult{}, fmt.Errorf("recovery_public_key: %w", err)
	}
	return PairingResult{
		APIToken: claimResp.APIToken,
		Key:      RecoveryKey{Public: pk, Threshold: claimResp.Threshold, TotalShares: claimResp.TotalShares},
	}, nil
```

Imports: `"encoding/base64"` is already there; add `"fmt"` if missing and `"github.com/Busness-app/ky-primitives/recoverykey"`.

- [ ] **Step 6: The handler stores it**

`internal/api/backup_handlers.go`, `handlePairRemoteRecovery`, replace from `token, err := s.recovery.ClaimPairing(...)` to the end:

```go
	result, err := s.recovery.ClaimPairing(r.Context(), req.RecoveryURL, req.PairingCode, s.config.Server.AppName)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Recovery pairing failed")
		return
	}

	if err := backup.StoreRecoveryKey(r.Context(), s.config.Database.DataDir, s.store.Settings(), result.Key); err != nil {
		if errors.Is(err, fs.ErrExist) {
			s.writeError(w, http.StatusConflict, "Already paired to a different recovery key")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "Failed to save recovery key")
		return
	}
	if err := s.store.Settings().SetSetting(r.Context(), "kyrecovery_url", req.RecoveryURL); err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to save recovery pairing")
		return
	}
	if err := s.store.Settings().SetSetting(r.Context(), "kyrecovery_token", result.APIToken); err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to save recovery pairing")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"paired":          true,
		"recovery_url":    req.RecoveryURL,
		"recovery_key_id": result.Key.Public.ID(),
		"threshold":       result.Key.Threshold,
		"total_shares":    result.Key.TotalShares,
	})
```

Imports: `"errors"`, `"io/fs"`.

- [ ] **Step 7: Handler test with a fake KyRecovery**

Find the existing pair-remote test (`grep -rn "pair-remote" internal/api/*_test.go`). The client refuses non-HTTPS and private addresses, so the existing test either stubs `s.recovery` or asserts the failure path. If there is an interface or field the test can replace, add a fake whose `ClaimPairing` returns a `PairingResult` with a `recoverykey.Generate().Public()` key and `Threshold: 2, TotalShares: 3`, POST to `/api/backup/pair-remote` as admin, and assert `200`, `recovery_key_id` in the body equals the fake key's `ID()`, and `backup.LoadRecoveryKey` returns the same ID. If `s.recovery` is a concrete `*backup.KyRecoveryClient` with no seam, add one:

```go
// internal/api/server.go
type recoveryPairer interface {
	ClaimPairing(ctx context.Context, serverURL, pairingCode, appName string) (backup.PairingResult, error)
}
```

and type the field as `recoveryPairer`, assigning `backup.NewKyRecoveryClient()` in `NewServer`. Export a test-only setter if the package's tests are external (`package api_test`): `func (s *Server) SetRecoveryClientForTest(p recoveryPairer)` is acceptable; keep it in a `_test.go`-adjacent file named `export_test.go` **only if** the tests are internal. Choose whichever matches the file's package clause.

- [ ] **Step 8: Run**

```bash
gofmt -l . ; go vet ./... && go test -race -count=1 ./...
```

- [ ] **Step 9: Commit**

```bash
git add internal/backup internal/api
git commit -m "feat(backup): receive the suite recovery public key at pairing and pin its ID"
```

---

### Task 6: Seal with `capsule`, drill against a throwaway key, export the `.kycap`

**Files:**
- Delete: `internal/backup/shamir.go`, `internal/backup/shamir_vectors_test.go`, `testdata/shamir-vectors.json`, `internal/backup/recovery_kit.go`
- Modify: `internal/backup/capsule.go` (replace whole file)
- Modify: `internal/backup/drill.go:30-60`
- Modify: `internal/backup/backup_test.go` (delete `TestShamirSecretSharing`, `TestExtractCapsuleRejectsTraversal`, `TestCapsuleLifecycleAndRestoreDrill`)
- Create: `internal/backup/capsule_test.go`
- Modify: `internal/api/backup_handlers.go:11-90`, `internal/api/server.go:118-120`
- Modify: `cmd/server/main.go:27-32,150-235`
- Modify: `web/src/pages/Backup.tsx:142`, rebuild `web/dist`

**Interfaces:**
- Consumes: `capsule.Seal(serviceName, appVersion string, files []capsule.File, deps, recipe map[string]any, threshold, totalShares int, to recoverykey.PublicKey) ([]byte, capsule.Manifest, error)`; `capsule.Open(raw []byte, with recoverykey.PrivateKey, targetDir string) (capsule.Manifest, []capsule.File, error)`; `capsule.File{Path string; Content []byte; Mode os.FileMode}`; `recoverykey.Generate()`.
- Produces:

```go
// backup
type BackupFile struct{ Path string; Data []byte; Mode int64 }   // unchanged
func Seal(serviceName, appVersion string, files []BackupFile, deps, recipe map[string]any, key RecoveryKey) ([]byte, capsule.Manifest, error)
func RunRestoreDrill(ctx context.Context, serviceName, appVersion string, files []BackupFile, deps, recipe map[string]any, pinned RecoveryKey) (*DrillResult, error)
```

  `Capsule`, `CreateCapsule`, `ExtractCapsule`, `Share`, `SplitSecret`, `CombineShares`, `GenerateRecoveryKitHTML`, `ErrUnsafePath`, `ErrCorruptCapsule` are all removed from this package. Callers needing the sentinels use `capsule.ErrPathTraversal`, `capsule.ErrCorruptCapsule` from the library.

- [ ] **Step 1: Delete the Shamir and kit files**

```bash
git rm internal/backup/shamir.go internal/backup/shamir_vectors_test.go testdata/shamir-vectors.json internal/backup/recovery_kit.go
rmdir testdata 2>/dev/null || true
```

- [ ] **Step 2: Failing tests**

Create `internal/backup/capsule_test.go`:

```go
package backup_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Busness-app/ky-primitives/capsule"
	"github.com/Busness-app/ky-primitives/recoverykey"
	"github.com/Busness-app/ky_server_base/internal/backup"
)

func testKey(t *testing.T) (recoverykey.PrivateKey, backup.RecoveryKey) {
	t.Helper()
	priv, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return priv, backup.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}
}

var testFiles = []backup.BackupFile{
	{Path: "data/test.db", Data: []byte("SQLite format 3\x00mock"), Mode: 0600},
	{Path: "config/app.json", Data: []byte(`{"app_name":"BusnesApp"}`), Mode: 0600},
}

func TestSealProducesKycap3ForThePinnedKey(t *testing.T) {
	priv, key := testKey(t)
	raw, m, err := backup.Seal("busnes_app", "1.0.0", testFiles, nil, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	if m.RecoveryKeyID != key.Public.ID() || m.Threshold != 2 || m.TotalShares != 3 {
		t.Fatalf("manifest %+v", m.UnverifiedManifest)
	}
	um, err := capsule.ReadUnverifiedManifest(raw)
	if err != nil || um.CapsuleID != m.CapsuleID {
		t.Fatalf("keyless read: %v %+v", err, um)
	}
	_, files, err := capsule.Open(raw, priv, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || string(files[0].Content) != string(testFiles[0].Data) {
		t.Fatalf("files %+v", files)
	}
}

func TestSealRefusesWhenNotPaired(t *testing.T) {
	_, _, err := backup.Seal("busnes_app", "1.0.0", testFiles, nil, nil, backup.RecoveryKey{})
	if !errors.Is(err, backup.ErrNotPaired) {
		t.Fatalf("got %v, want ErrNotPaired", err)
	}
}

func TestDrillPassesAndReportsThePinnedKey(t *testing.T) {
	_, key := testKey(t)
	recipe := map[string]any{
		"required_files": []any{"data/test.db", "config/app.json"},
		"expected_env":   []any{"KY_PORT"},
		"expected_ports": []any{8080},
	}
	t.Setenv("KY_PORT", "8080")
	res, err := backup.RunRestoreDrill(context.Background(), "busnes_app", "1.0.0", testFiles, map[string]any{"ports": []int{8080}}, recipe, key)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatalf("drill failed: %+v", res)
	}
	var sawKey bool
	for _, c := range res.Checks {
		if c.Name == "Recovery Key" && c.Passed {
			sawKey = true
		}
	}
	if !sawKey {
		t.Fatalf("no passing Recovery Key check in %+v", res.Checks)
	}
}

func TestDrillFailsWhenNotPaired(t *testing.T) {
	res, err := backup.RunRestoreDrill(context.Background(), "busnes_app", "1.0.0", testFiles, nil, nil, backup.RecoveryKey{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatal("drill passed with no recovery key")
	}
}

func TestDrillFailsARequiredFileThatIsMissing(t *testing.T) {
	_, key := testKey(t)
	recipe := map[string]any{"required_files": []any{"data/absent.db"}}
	res, err := backup.RunRestoreDrill(context.Background(), "busnes_app", "1.0.0", testFiles, nil, recipe, key)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatal("drill passed with a missing required file")
	}
}
```

Delete `TestShamirSecretSharing`, `TestExtractCapsuleRejectsTraversal` (the library owns traversal: `capsule.ErrPathTraversal` is tested there) and `TestCapsuleLifecycleAndRestoreDrill` from `backup_test.go`, and prune its imports. If nothing is left in `backup_test.go`, delete the file.

- [ ] **Step 3: Run to see them fail**

```bash
go test ./internal/backup/ 2>&1 | head -5
```

Expected: `undefined: backup.Seal` (and build errors from the deleted Shamir symbols in `capsule.go`).

- [ ] **Step 4: Replace `internal/backup/capsule.go`**

```go
package backup

import (
	"os"

	"github.com/Busness-app/ky-primitives/capsule"
)

// BackupFile is one member of a capsule's payload, as the collectors produce it.
type BackupFile struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
	Mode int64  `json:"mode"`
}

// Seal writes a kycap/3 container sealed to the suite recovery public key. The product
// holds nothing afterwards that opens it; only the custodians' shares do.
func Seal(serviceName, appVersion string, files []BackupFile, deps, recipe map[string]any, key RecoveryKey) ([]byte, capsule.Manifest, error) {
	if key.Public.IsZero() {
		return nil, capsule.Manifest{}, ErrNotPaired
	}
	return capsule.Seal(serviceName, appVersion, toCapsuleFiles(files), deps, recipe, key.Threshold, key.TotalShares, key.Public)
}

func toCapsuleFiles(files []BackupFile) []capsule.File {
	out := make([]capsule.File, 0, len(files))
	for _, f := range files {
		out = append(out, capsule.File{Path: f.Path, Content: f.Data, Mode: os.FileMode(f.Mode)})
	}
	return out
}
```

- [ ] **Step 5: Rewrite the top of `internal/backup/drill.go`**

Replace the `RunRestoreDrill` signature and its first section (through the "Directory Unpack" pass check) with:

```go
// RunRestoreDrill proves the backup pipeline: it seals files exactly as a real backup would,
// but to a throwaway keypair it then opens with, extracts into a 0700 scratch directory, and
// runs the verification recipe. The product has no recovery private key, so this is the only
// end-to-end check it can run alone. A separate check reports whether the suite key is pinned.
func RunRestoreDrill(ctx context.Context, serviceName, appVersion string, files []BackupFile, deps, recipe map[string]any, pinned RecoveryKey) (*DrillResult, error) {
	start := time.Now()
	result := &DrillResult{Passed: true, Checks: make([]CheckItem, 0)}

	// 0. Is this instance paired to the suite recovery key?
	if pinned.Public.IsZero() {
		result.Passed = false
		result.Checks = append(result.Checks, CheckItem{Name: "Recovery Key", Passed: false, Message: ErrNotPaired.Error()})
	} else {
		result.Checks = append(result.Checks, CheckItem{Name: "Recovery Key", Passed: true,
			Message: fmt.Sprintf("Sealing to recovery key %s (%d-of-%d custodians)", pinned.Public.ID()[:16], pinned.Threshold, pinned.TotalShares)})
	}

	// 1. Seal to a throwaway key and open with it. Topology is fixed here: it is display
	// metadata, and the drill key has no custodians.
	drillKey, err := recoverykey.Generate()
	if err != nil {
		return nil, fmt.Errorf("drill key: %w", err)
	}
	raw, _, err := capsule.Seal(serviceName, appVersion, toCapsuleFiles(files), deps, recipe, 2, 3, drillKey.Public())
	if err != nil {
		return nil, fmt.Errorf("drill seal: %w", err)
	}

	scratchDir, err := os.MkdirTemp("", "kyrec-drill-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create drill sandbox: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratchDir) }()
	_ = os.Chmod(scratchDir, 0700)

	m, extracted, err := capsule.Open(raw, drillKey, scratchDir)
	if err != nil {
		result.Passed = false
		result.ErrorMessage = fmt.Sprintf("Open failed: %v", err)
		result.Checks = append(result.Checks, CheckItem{Name: "Directory Unpack", Passed: false, Message: result.ErrorMessage})
		result.DurationMS = time.Since(start).Milliseconds()
		return result, nil
	}

	var totalBytes int64
	for _, f := range extracted {
		totalBytes += int64(len(f.Content))
	}
	result.Checks = append(result.Checks, CheckItem{Name: "Directory Unpack", Passed: true,
		Message: fmt.Sprintf("Extracted %d files (%d bytes)", len(extracted), totalBytes)})

	recipeMap, _ := m.VerificationRecipe.(map[string]any)
	if recipeMap == nil {
		recipeMap = map[string]any{}
	}
```

Then rename the local the rest of the function reads from `recipe` to `recipeMap` (the old code did `recipe := capsule.Manifest.VerificationRecipe`; the sections "Verify Required Files", "SQLite Integrity Checks" and onward keep working against `recipeMap`). Add imports `"github.com/Busness-app/ky-primitives/capsule"` and `"github.com/Busness-app/ky-primitives/recoverykey"`.

Note the manifest's `VerificationRecipe` comes back through JSON as `map[string]any` with `[]any` slices, which is what the existing recipe code already type-asserts.

- [ ] **Step 6: Handlers**

`internal/api/backup_handlers.go`. Add a helper and rewrite the two handlers; `handlePairRemoteRecovery` is unchanged from Task 5.

```go
// collectFiles is what both the drill and the export seal: the same payload the deposit path
// will send, decoded from BuildLocalPayload's transport form.
func (s *Server) collectFiles() (*backup.PushBackupPayload, []backup.BackupFile, error) {
	payload, err := backup.BuildLocalPayload(s.config, "1.0.0")
	if err != nil {
		return nil, nil, err
	}
	files := make([]backup.BackupFile, 0, len(payload.Files))
	for _, f := range payload.Files {
		data, err := base64.StdEncoding.DecodeString(f.DataBase64)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, backup.BackupFile{Path: f.Path, Data: data, Mode: f.Mode})
	}
	return payload, files, nil
}

func (s *Server) handleBackupDrill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	payload, files, err := s.collectFiles()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to collect backup files")
		return
	}
	pinned, err := backup.LoadRecoveryKey(r.Context(), s.config.Database.DataDir, s.store.Settings())
	if err != nil && !errors.Is(err, backup.ErrNotPaired) {
		s.writeError(w, http.StatusInternalServerError, "Failed to load recovery key")
		return
	}
	result, err := backup.RunRestoreDrill(r.Context(), s.config.Server.AppName, "1.0.0", files, payload.Dependencies, payload.VerificationRecipe, pinned)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to execute restore drill")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// handleExportCapsule hands the operator the sealed capsule itself. Only the custodians'
// shares open it, so the download is safe to store anywhere; kyrecovery is where it belongs.
func (s *Server) handleExportCapsule(w http.ResponseWriter, r *http.Request) {
	payload, files, err := s.collectFiles()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to collect backup files")
		return
	}
	key, err := backup.LoadRecoveryKey(r.Context(), s.config.Database.DataDir, s.store.Settings())
	if errors.Is(err, backup.ErrNotPaired) {
		s.writeError(w, http.StatusPreconditionFailed, "Not paired with KyRecovery; no recovery key to seal to")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to load recovery key")
		return
	}
	raw, m, err := backup.Seal(s.config.Server.AppName, "1.0.0", files, payload.Dependencies, payload.VerificationRecipe, key)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to seal capsule")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.kycap"`, m.CapsuleID))
	w.Header().Set("X-Recovery-Key-ID", m.RecoveryKeyID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
```

`m.CapsuleID` is minted by the library as `cap-<service>-<unix>`; confirm it contains no characters needing quoting by reading `go doc -src github.com/Busness-app/ky-primitives/capsule Seal` and, if the service name is user-controlled, sanitise with `strings.Map` to `[A-Za-z0-9._-]`.

`internal/api/server.go:119`: `s.mux.HandleFunc("/api/backup/export-capsule", s.requireAdmin(s.handleExportCapsule))`. Delete the `export-kit` route.

Imports in `backup_handlers.go`: `"errors"`, `"fmt"`, `"io/fs"`, `"encoding/base64"`, `"encoding/json"`, `"net/http"`, `backup`.

- [ ] **Step 7: CLI**

`cmd/server/main.go`: rename the `export-recovery-kit` case to `export-capsule` calling `runExportCapsule`. Rewrite `runBackupDrill`'s tail and replace `runExportRecoveryKit`:

```go
func loadRecoveryKey(ctx context.Context, cfg *config.Config, st store.Store) (backup.RecoveryKey, error) {
	return backup.LoadRecoveryKey(ctx, cfg.Database.DataDir, st.Settings())
}

func collectFiles(cfg *config.Config) (*backup.PushBackupPayload, []backup.BackupFile) {
	payload, err := backup.BuildLocalPayload(cfg, "1.0.0")
	if err != nil {
		log.Fatalf("Failed to collect backup files: %v", err)
	}
	var files []backup.BackupFile
	for _, f := range payload.Files {
		data, err := base64.StdEncoding.DecodeString(f.DataBase64)
		if err != nil {
			log.Fatalf("Invalid backup file encoding: %v", err)
		}
		files = append(files, backup.BackupFile{Path: f.Path, Data: data, Mode: f.Mode})
	}
	return payload, files
}
```

In `runBackupDrill`, after opening the store, replace the `CreateCapsule`/`RunRestoreDrill` pair with:

```go
	payload, files := collectFiles(cfg)
	pinned, err := loadRecoveryKey(ctx, cfg, st)
	if err != nil && !errors.Is(err, backup.ErrNotPaired) {
		log.Fatalf("Recovery key: %v", err)
	}
	result, err := backup.RunRestoreDrill(ctx, cfg.Server.AppName, "1.0.0", files, payload.Dependencies, payload.VerificationRecipe, pinned)
```

Read the existing `runBackupDrill` first: if it does not currently open the store, add `st, err := store.Open(ctx, cfg.Database)` with `defer st.Close()` as `runInitAdmin` does.

```go
func runExportCapsule(args []string) {
	fs := flag.NewFlagSet("export-capsule", flag.ExitOnError)
	out := fs.String("out", "", "output path (default <capsule-id>.kycap in the current directory)")
	_ = fs.Parse(args)

	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		log.Fatalf("DB error: %v", err)
	}
	defer st.Close()

	key, err := loadRecoveryKey(ctx, cfg, st)
	if err != nil {
		log.Fatalf("Recovery key: %v", err)
	}
	payload, files := collectFiles(cfg)
	raw, m, err := backup.Seal(cfg.Server.AppName, "1.0.0", files, payload.Dependencies, payload.VerificationRecipe, key)
	if err != nil {
		log.Fatalf("Seal: %v", err)
	}
	path := *out
	if path == "" {
		path = m.CapsuleID + ".kycap"
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		log.Fatalf("Write: %v", err)
	}
	log.Printf("✓ Capsule %s sealed to recovery key %s, written to %s (%d bytes)", m.CapsuleID, m.RecoveryKeyID, path, len(raw))
}
```

This also fixes the old `runExportRecoveryKit`, which packed the base64 *text* of each file as its content. Imports: `"encoding/base64"`, `"errors"`.

- [ ] **Step 8: Web link and dist**

`web/src/pages/Backup.tsx:142`: `href="/api/backup/export-capsule"`; change the visible label near it from "Recovery Kit" wording to "Download sealed capsule (.kycap)" and add `download`. Then:

```bash
cd web && npm ci && npm run build && cd ..
git status --short web/dist | head
```

Expected: `web/dist` changed; commit it with the source change.

- [ ] **Step 9: Build and test everything**

```bash
go mod tidy && git diff --exit-code go.mod go.sum
gofmt -l . ; go vet ./... && go test -race -count=1 ./...
grep -rn "CreateCapsule\|ExtractCapsule\|SplitSecret\|CombineShares\|GenerateRecoveryKitHTML\|export-kit\|shamir" --include='*.go' --include='*.tsx' --include='*.ts' . | grep -v node_modules
```

Expected: tidy is a no-op (the `x/crypto` direct require may have moved to indirect in Task 2; that state is what must be committed); grep prints nothing.

- [ ] **Step 10: Commit**

```bash
git add -A
git commit -m "feat(backup): seal kycap/3 capsules to the suite recovery key; drill against a throwaway key; export the capsule, not shares"
```

---

### Task 7: `restore` command: custodian shares in, files out

**Files:**
- Modify: `cmd/server/main.go` (new case and function)
- Create: `cmd/server/restore_test.go`

**Interfaces:**
- Consumes: `shamir.ParseShare(string) (shamir.Share, error)`, `recoverykey.Combine([]shamir.Share) (recoverykey.PrivateKey, error)`, `capsule.Open`, `capsule.ErrWrongRecoveryKey`.
- Produces: `func restore(capsulePath, targetDir, expectService string, shareStrings []string, stdout io.Writer) error` (unexported, testable); CLI `restore -capsule X.kycap -to DIR -share ky2-... -share ky2-...`.

- [ ] **Step 1: Failing test**

`cmd/server/restore_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busness-app/ky-primitives/capsule"
	"github.com/Busness-app/ky-primitives/recoverykey"
	"github.com/Busness-app/ky_server_base/internal/backup"
)

func sealFixture(t *testing.T, service string) (string, []string) {
	t.Helper()
	priv, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	shares, err := recoverykey.Split(priv, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	key := backup.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}
	raw, _, err := backup.Seal(service, "1.0.0", []backup.BackupFile{{Path: "data/x.db", Data: []byte("payload"), Mode: 0600}}, nil, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "x.kycap")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Non-consecutive indices: {1,2} would let an XOR-shaped bug pass.
	return path, []string{shares[0].String(), shares[2].String()}
}

func TestRestoreExtractsWithTwoShares(t *testing.T) {
	path, shares := sealFixture(t, "busnes_app")
	target := t.TempDir()
	var out bytes.Buffer
	if err := restore(path, target, "busnes_app", shares, &out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "data", "x.db"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("restored file: %q %v", got, err)
	}
	if !strings.Contains(out.String(), "busnes_app") {
		t.Fatalf("manifest not printed: %s", out.String())
	}
}

func TestRestoreRefusesAnotherService(t *testing.T) {
	path, shares := sealFixture(t, "someone_else")
	err := restore(path, t.TempDir(), "busnes_app", shares, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "someone_else") {
		t.Fatalf("got %v, want a service-name refusal naming the capsule's service", err)
	}
}

func TestRestoreRefusesTheWrongKit(t *testing.T) {
	path, _ := sealFixture(t, "busnes_app")
	_, otherShares := sealFixture(t, "busnes_app")
	err := restore(path, t.TempDir(), "busnes_app", otherShares, &bytes.Buffer{})
	if !errors.Is(err, capsule.ErrWrongRecoveryKey) {
		t.Fatalf("got %v, want ErrWrongRecoveryKey", err)
	}
}

func TestRestoreRefusesOneShare(t *testing.T) {
	path, shares := sealFixture(t, "busnes_app")
	if err := restore(path, t.TempDir(), "busnes_app", shares[:1], &bytes.Buffer{}); err == nil {
		t.Fatal("one share of a 2-of-3 kit was accepted")
	}
}
```

- [ ] **Step 2: Run to see it fail**

```bash
go test ./cmd/server/ -run TestRestore 2>&1 | head -3
```

Expected: `undefined: restore`.

- [ ] **Step 3: Implement**

In `cmd/server/main.go`, add `case "restore": runRestore(os.Args[2:]); return` to the switch, and:

```go
// restore is the product-side half of the ceremony: k custodian shares typed from their cards,
// combined here, used once, and dropped. It refuses a capsule from another service before
// touching the key, and prints the authenticated manifest so the operator can compare
// CapsuleID and CreatedAt against kyrecovery's deposit record — Open proves integrity and
// binding to this key, not which backup this is.
func restore(capsulePath, targetDir, expectService string, shareStrings []string, stdout io.Writer) error {
	raw, err := os.ReadFile(capsulePath)
	if err != nil {
		return err
	}
	peek, err := capsule.ReadUnverifiedManifest(raw)
	if err != nil {
		return err
	}
	if peek.ServiceName != expectService {
		return fmt.Errorf("capsule is for service %q, this instance is %q; pass -service to override", peek.ServiceName, expectService)
	}

	shares := make([]shamir.Share, 0, len(shareStrings))
	for i, s := range shareStrings {
		sh, err := shamir.ParseShare(s)
		if err != nil {
			return fmt.Errorf("share %d: %w", i+1, err)
		}
		shares = append(shares, sh)
	}
	priv, err := recoverykey.Combine(shares)
	if err != nil {
		return err
	}

	m, files, err := capsule.Open(raw, priv, targetDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restored %d files from capsule %s\n  service:      %s (v%s)\n  created:      %s\n  recovery key: %s\n  payload hash: %s\n",
		len(files), m.CapsuleID, m.ServiceName, m.AppVersion, m.CreatedAt.Format(time.RFC3339), m.RecoveryKeyID, m.PayloadHash)
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runRestore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	capsulePath := fs.String("capsule", "", "path to the .kycap file")
	target := fs.String("to", "", "empty directory to restore into")
	service := fs.String("service", "", "expected service name (default: KY_APP_NAME)")
	var shares multiFlag
	fs.Var(&shares, "share", "one custodian share (ky2-...); repeat for each")
	_ = fs.Parse(args)
	if *capsulePath == "" || *target == "" || len(shares) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	if *service == "" {
		cfg, err := config.LoadFromEnv()
		if err != nil {
			log.Fatalf("Failed to load configuration: %v", err)
		}
		*service = cfg.Server.AppName
	}
	if err := restore(*capsulePath, *target, *service, shares, os.Stdout); err != nil {
		log.Fatalf("Restore failed: %v", err)
	}
}
```

Imports: `"io"`, `"strings"`, `"github.com/Busness-app/ky-primitives/capsule"`, `"github.com/Busness-app/ky-primitives/recoverykey"`, `"github.com/Busness-app/ky-primitives/shamir"`.

`capsule.Open` refuses a non-empty target with `ErrTargetNotEmpty`; that is the containment we want, so do not pre-create or clear anything.

- [ ] **Step 4: Run**

```bash
gofmt -l . ; go vet ./cmd/... && go test -race -count=1 ./cmd/...
```

- [ ] **Step 5: Commit**

```bash
git add cmd/server
git commit -m "feat(cli): restore a capsule from custodian shares"
```

---

### Task 8: Scaffold plumbing: `ky-init.sh`, compat workflow, docs

**Files:**
- Modify: `scripts/ky-init.sh:38`
- Create: `.github/workflows/ky-primitives-compat.yml`
- Modify: `internal/backup/AGENTS.md`, `IMPLEMENTATION_PLAN.md:10,38-39`, `AGENTS.md:102`

- [ ] **Step 1: Derive the module path**

`scripts/ky-init.sh:38` becomes:

```bash
MODULE_OLD="$(cd "$BASE_DIR" && go list -m)"
```

Add `go` to whatever prerequisite check the script has (it already needs `go` for `go mod tidy`, so none is needed beyond the existing `set -euo pipefail`). Run `shellcheck scripts/ky-init.sh` (the `smoke` CI job does).

The spec's line 300 says `go mod tidy` "must now resolve a module under the Busness-app org" and needs `GOPRIVATE`. It does not: `github.com/Busness-app/ky-primitives` is public and resolves through the proxy. Do not add `GOPRIVATE`.

- [ ] **Step 2: Compat workflow**

Copy `/home/yoshi/busness.app/gridlock-server/.github/workflows/ky-primitives-compat.yml` verbatim into this repo. Read it once: it checks out the consumer to `consumer/` and the library's default branch to `ky-primitives/`, uses `cache: false`, runs `go mod edit -replace github.com/Busness-app/ky-primitives=../ky-primitives`, then `go build ./...` and `go test -count=1 ./...`, and is scheduled plus dispatch, never a merge gate. Nothing in it names gridlock; if it does, fix the name.

- [ ] **Step 3: Docs**

- `internal/backup/AGENTS.md`: rewrite the purpose and rules paragraphs. Capsules are `kycap/3` from `github.com/Busness-app/ky-primitives/capsule`, sealed to the suite recovery public key received at pairing (`recovery.pub`, key ID pinned in `server_settings`). This package collects the payload, loads and pins the key, runs the drill against a throwaway key, and exposes the sealed capsule for download. It holds no private key and no shares. Delete the Shamir and Recovery Kit rules.
- `IMPLEMENTATION_PLAN.md:10`: "Encrypted backup capsules sealed to the suite recovery key (ky-primitives `capsule`), restore drill verification". `:38-39`: point both items at `ky-primitives`; delete the Shamir line.
- `AGENTS.md:102`: drop "and Shamir secret splitting".

- [ ] **Step 4: Final gate, the same commands CI runs**

```bash
gofmt -l $(git ls-files '*.go'); go mod tidy && git diff --exit-code go.mod go.sum && go mod verify
go vet ./... && go test -race -count=1 ./...
shellcheck scripts/*.sh
go build -o /tmp/ky_server_base ./cmd/server && /tmp/ky_server_base version
```

Expected: everything clean. Then `git status` must show nothing untracked; a stray `data/` or `*.kycap` means a test wrote outside `t.TempDir()`.

- [ ] **Step 5: Commit**

```bash
git add scripts/ky-init.sh .github/workflows/ky-primitives-compat.yml internal/backup/AGENTS.md IMPLEMENTATION_PLAN.md AGENTS.md
git commit -m "chore: derive the scaffold module path, add the ky-primitives compat job, update the backup docs"
```

---

## What is deliberately not in this plan

- **Deposit to kyrecovery.** `PushBackup` still sends plaintext files to `/api/backup/push` and still has no caller. Plan 5 replaces that endpoint with a capsule deposit and the product side follows it; sealing here is what it will send.
- **`KY_SESSION_SECRET`** rotating per restart in development. Same shape as the encryption-key bug, different consequence (logout, not data loss). A one-line follow-up with `keyfile`.
- **Parent plan Task 6** (three-dependency floor). Its allowlist will need `github.com/Busness-app/ky-primitives` added when it runs.
- **`web/` copy for the drill result**'s new "Recovery Key" check row: the page renders whatever checks come back, so no change is required. Verify by loading the page once after Task 6.

## Self-review

- Spec coverage: Phase 3 adopts `shamir` (restore CLI via `ParseShare`/`Combine`), `capsule` (Task 6), `password` (Task 2), `totp` with counter column (Task 3), `recoverycode` with in-place blank (Task 4), `keyfile` fixing the key mint (Task 1), plus `recoverykey` receive path at pairing (Task 5, from the keypair design Part 4). Vectors deleted, not ported (Task 6). `ky-init.sh` `MODULE_OLD` derived (Task 8). `GOPRIVATE` claim corrected rather than followed.
- Names used across tasks: `backup.RecoveryKey{Public, Threshold, TotalShares}`, `backup.LoadRecoveryKey`, `backup.StoreRecoveryKey`, `backup.RecoveryKeyPath`, `backup.ErrNotPaired`, `backup.Seal`, `backup.RunRestoreDrill(ctx, service, version, files, deps, recipe, pinned)`, `backup.PairingResult{APIToken, Key}`, `UserStore.SpendTOTPCounter`, `crypto.ErrKeyLength`, `config.SecurityConfig.EncryptionKey []byte`. Each is defined in the task whose Interfaces block names it under Produces and consumed only by later tasks.
