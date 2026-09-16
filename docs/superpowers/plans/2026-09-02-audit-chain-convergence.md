# Audit Chain Convergence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the suite one hash-chained audit record that a single verifier can check, and close the gap where one product's audit trail can be forged undetectably.

**Architecture:** Three products chain audit records three different ways and a fourth has no chain at all. `kyrecovery-server` has the only complete design — keyed, with a legacy-migration path and tamper tests — so it becomes the donor. The chain arithmetic moves into `ky-primitives/auditchain`, storage stays per-product, and each product migrates behind the same rekey path `kyrecovery-server` already ships.

**Tech Stack:** Go 1.26+, standard library only.

**Spec:** [The suite shared primitives plan](2026-09-02-suite-shared-primitives.md), Finding C and Task 2 Step 4.

## Global Constraints

- **No existing audit record may become unverifiable.** A migration that invalidates a trail destroys the evidence the trail exists to preserve.
- The shared module has **zero dependencies**. Standard library only.
- Gates stay green in every repo touched: `gofmt -l .` empty, `go vet ./...`, `go test -race ./...`.
- Module path prefix is `github.com/Busness-app/`.

---

## Findings that set the scope

Measured 2026-09-02. Four `internal/audit` packages, four distinct implementations, no two alike.

| Repo | Storage | Keying | Hashed tuple | Verify API |
|---|---|---|---|---|
| `kypassword-server` | JSONL file, `Store` | **unkeyed SHA-256** | `Index｜Timestamp｜Action｜UserID｜DeviceID｜IPAddress｜Details｜PrevHash` | `VerifyIntegrity() (bool, error)` |
| `kybookmarks-server` | JSONL file, `Logger` | HMAC-SHA256, `secret` | `ID｜Timestamp｜Action｜UserID｜DeviceID｜IP｜Details｜PrevHash` | `VerifyChain() (bool, int, error)` |
| `kyrecovery-server` | database, `Ledger` | HMAC-SHA256, ledger key, with unkeyed legacy fallback | `seq｜prevHash｜action｜actor｜targetID｜sha256(detailsJSON)｜RFC3339Nano` | `VerifyChain(ctx) (ChainStatus, error)` |
| `kysignon-server` | store + stdout | **no chain** | — | none |

Three things follow.

### The parent plan offers a choice that does not exist

Task 2 Step 4 says to take the chain *"from `kyrecovery-server` or `kysignon-server`, whichever verifier is more complete."* `kysignon-server` has no chain. `grep -rn "PrevHash\|prev_hash" kysignon-server --include='*.go'` returns nothing; `internal/audit/audit.go` is a structured JSON logger with a `LogEntry` struct and no hashing at all. There is one candidate, not two.

### `kypassword-server`'s audit trail can be forged undetectably

Its chain is plain `sha256.Sum256` over the tuple, with no secret:

```go
raw := fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s|%s",
    e.Index, e.Timestamp.Format(time.RFC3339Nano), e.Action, e.UserID,
    e.DeviceID, e.IPAddress, e.Details, e.PrevHash)
h := sha256.Sum256([]byte(raw))
```

Anyone who can write the audit file can also recompute every hash after the record they altered, and `VerifyIntegrity` will pass. The chain detects *corruption*; it does not detect *tampering*, which is the property an audit trail is kept for. `kybookmarks-server` and `kyrecovery-server` both use HMAC and do not have this problem.

**This is the reason to do this work.** The consolidation is a tidy-up; closing this is not.

### `kyrecovery-server` has already solved the migration

`rekeyLegacyChain` walks a chain written unkeyed, recomputes each record under the ledger key, and marks the ledger keyed — so an existing trail survives the transition to HMAC instead of being invalidated. `kypassword-server` needs exactly this, and it should be taken rather than reinvented.

Note also that `kybookmarks-server`'s doc comment says *"persists and verifies audit entries with SHA-256 hash chaining"* while the code uses `hmac.New(sha256.New, l.secret)`. The comment is stale; the code is right.

---

## Design decisions

**1. The shared package holds chain arithmetic, not storage.** One product writes JSONL files and another writes to a database, and both are legitimate. `auditchain` computes and verifies; it never touches a file or a `*sql.DB`.

**2. `kyrecovery-server`'s tuple becomes the canonical one.** It is the only design that hashes `detailsJSON` before including it — which bounds the tuple's length regardless of how large a details blob gets — and the only one that puts `prevHash` in a fixed early position rather than trailing. It is also the only one with tamper tests.

**3. Keyed by default; unkeyed only as a legacy read path.** `Append` requires a key. Verification accepts an unkeyed chain **only** when reading records written before a rekey, exactly as `kyrecovery-server` does today.

**4. `kysignon-server` is out of scope.** Adding a chain to a product that has none is a feature, not a consolidation, and it needs a product decision about what its audit trail is for. Raised at the end of this plan, not built in it.

**5. Every product migrates behind a rekey, never a rewrite.** No existing record is discarded, and `VerifyChain` reports whether a chain is legacy or keyed rather than silently accepting both.

---

## Task 1: Build `auditchain` in ky-primitives

**Files:**
- Create: `ky-primitives/auditchain/chain.go`
- Test: `ky-primitives/auditchain/chain_test.go`
- Create: `ky-primitives/testdata/auditchain/` (fixtures)

**Depends on:** the `ky-primitives` module existing (parent plan Task 2 Step 1).

**Interfaces produced:**
- `auditchain.Event{Seq int64, Action, Actor, TargetID, DetailsJSON string, CreatedAt time.Time}`
- `auditchain.Hash(key []byte, prevHash string, e Event) string` — HMAC-SHA256 when `key` is non-empty, plain SHA-256 when it is empty (legacy)
- `auditchain.Verify(key []byte, records []Record) error`
- `auditchain.Record{Event, PrevHash, Hash string}`
- `auditchain.GenesisHash` — the 64-zero string

- [ ] **Step 1: Write the failing tests**

Three properties matter, and a chain that only has the first is not tamper-evident:

```go
package auditchain

import (
	"strings"
	"testing"
	"time"
)

func chainOf(t *testing.T, key []byte, n int) []Record {
	t.Helper()
	var out []Record
	prev := GenesisHash
	for i := 0; i < n; i++ {
		e := Event{
			Seq:         int64(i),
			Action:      "test.action",
			Actor:       "alice@example.com",
			TargetID:    "target-1",
			DetailsJSON: `{"k":"v"}`,
			CreatedAt:   time.Unix(1700000000+int64(i), 0).UTC(),
		}
		h := Hash(key, prev, e)
		out = append(out, Record{Event: e, PrevHash: prev, Hash: h})
		prev = h
	}
	return out
}

func TestValidChainVerifies(t *testing.T) {
	key := []byte("ledger-key")
	if err := Verify(key, chainOf(t, key, 5)); err != nil {
		t.Fatalf("a chain we just built should verify: %v", err)
	}
}

func TestMutatedRecordFails(t *testing.T) {
	key := []byte("ledger-key")
	c := chainOf(t, key, 5)
	c[2].Event.Actor = "mallory@example.com"
	if err := Verify(key, c); err == nil {
		t.Fatal("a mutated record must not verify")
	}
}

// A chain that only catches mutation is not tamper-evident against deletion.
func TestRemovedRecordFails(t *testing.T) {
	key := []byte("ledger-key")
	c := chainOf(t, key, 5)
	c = append(c[:2], c[3:]...)
	if err := Verify(key, c); err == nil {
		t.Fatal("a chain with a record removed must not verify")
	}
}

func TestKeyedAndUnkeyedDiffer(t *testing.T) {
	c := chainOf(t, nil, 1)
	if Hash([]byte("ledger-key"), GenesisHash, c[0].Event) == c[0].Hash {
		t.Fatal("a keyed hash must not equal the unkeyed hash of the same event")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	c := chainOf(t, []byte("ledger-key"), 3)
	if err := Verify([]byte("other-key"), c); err == nil {
		t.Fatal("a chain must not verify under the wrong key")
	}
}

func TestGenesisIsSixtyFourZeros(t *testing.T) {
	if GenesisHash != strings.Repeat("0", 64) {
		t.Fatalf("genesis changed: %q", GenesisHash)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
cd ky-primitives && go test ./auditchain/
```

Expected: `undefined: GenesisHash`, `undefined: Hash`, `undefined: Verify`.

- [ ] **Step 3: Implement, porting from kyrecovery-server**

Take `eventTuple`, `CalculateEventHash` and the keyed `eventHash` from `kyrecovery-server/internal/audit/ledger.go` lines 181–204. Keep the tuple byte-for-byte identical — this is what lets `kyrecovery-server`'s existing ledger verify against the shared package.

```go
// Package auditchain computes and verifies the suite's hash-chained audit record.
//
// It holds arithmetic only. Storage is each product's own business: one writes
// JSONL files, another writes a database table, and both are fine.
package auditchain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// GenesisHash is the PrevHash of the first record in any chain.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

type Event struct {
	Seq         int64
	Action      string
	Actor       string
	TargetID    string
	DetailsJSON string
	CreatedAt   time.Time
}

type Record struct {
	Event    Event
	PrevHash string
	Hash     string
}

// tuple is the exact byte sequence kyrecovery-server has always hashed. Details
// are hashed before inclusion so the tuple stays bounded however large a details
// blob gets.
func tuple(prevHash string, e Event) []byte {
	detailsHash := sha256.Sum256([]byte(e.DetailsJSON))
	return []byte(fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s",
		e.Seq, prevHash, e.Action, e.Actor, e.TargetID,
		hex.EncodeToString(detailsHash[:]),
		e.CreatedAt.UTC().Format(time.RFC3339Nano)))
}

// Hash authenticates one record. An empty key produces the unkeyed hash a
// legacy chain was written with; it is a read path, not a choice new callers
// should make. An unkeyed chain can be recomputed wholesale by anyone who can
// write the store, so it detects corruption but not tampering.
func Hash(key []byte, prevHash string, e Event) string {
	if len(key) == 0 {
		h := sha256.New()
		h.Write(tuple(prevHash, e))
		return hex.EncodeToString(h.Sum(nil))
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(tuple(prevHash, e))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks that every record links to its predecessor and authenticates
// under key. Both mutation and deletion must fail: the PrevHash link catches a
// removed record, the recomputation catches an altered one.
func Verify(key []byte, records []Record) error {
	prev := GenesisHash
	for i, r := range records {
		if r.PrevHash != prev {
			return fmt.Errorf("audit chain broken at record %d (seq %d): prevHash mismatch", i, r.Event.Seq)
		}
		want := Hash(key, r.PrevHash, r.Event)
		if !hmac.Equal([]byte(want), []byte(r.Hash)) {
			return fmt.Errorf("audit record %d (seq %d) does not authenticate", i, r.Event.Seq)
		}
		prev = r.Hash
	}
	return nil
}

// IsGenesis reports whether s is the genesis hash, without callers hard-coding
// sixty-four zeros in four repositories.
func IsGenesis(s string) bool { return strings.EqualFold(s, GenesisHash) }
```

- [ ] **Step 4: Run the tests and watch them pass**

```bash
cd ky-primitives && go test -race -count=1 ./auditchain/
```

- [ ] **Step 5: Freeze fixtures**

Write `testdata/auditchain/valid-keyed.json`, `mutated.json` and `removed.json` — a five-record chain under a fixed key, plus the two tampered variants — and add a test that loads them. The fixtures are what stop a future refactor silently changing the tuple.

- [ ] **Step 6: Commit**

```bash
git add auditchain/ testdata/auditchain/
git commit -m "feat(auditchain): shared hash-chained audit record"
```

---

## Task 2: Migrate kyrecovery-server onto the shared package

**Files:**
- Modify: `kyrecovery-server/internal/audit/ledger.go`
- Test: `kyrecovery-server/internal/audit/ledger_test.go`, `tamper_test.go` (both unchanged — they are the contract)

**Depends on:** Task 1.

The donor migrates first. Its tuple is the canonical one, so if anything here needs to change, `auditchain` got the port wrong.

- [ ] **Step 1: Prove the existing tests pass before touching anything**

```bash
cd /home/yoshi/busness.app/kyrecovery-server
go test -race -count=1 ./internal/audit/... 2>&1 | tee /tmp/kyrecovery-audit-before.txt
```

- [ ] **Step 2: Add the dependency**

```bash
go get github.com/Busness-app/ky-primitives@v0.2.0
go mod tidy
go list -m all | wc -l
```

No new indirect entries. If any appear, stop.

- [ ] **Step 3: Delegate the arithmetic, keep the ledger**

`eventTuple`, `CalculateEventHash` and `eventHash` become calls into `auditchain`. `Ledger`, its database access, `rekeyLegacyChain`, `sanitizeActor` and `maxActorLen` all stay exactly where they are — none of that is chain arithmetic.

`CalculateEventHash` is exported and may have callers outside the package. Check before changing its signature:

```bash
grep -rn "CalculateEventHash" --include='*.go' .
```

Keep it as a wrapper if anything outside `internal/audit` uses it.

- [ ] **Step 4: The tamper tests must pass unedited**

```bash
go test -race -count=1 -v ./internal/audit/...
diff /tmp/kyrecovery-audit-before.txt <(go test -race -count=1 ./internal/audit/...)
```

`tamper_test.go` is the reason to trust this port. **If it needed editing, the tuple changed** — which means every audit record in the product just became unverifiable. Stop and report.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./... && go test -race -count=1 ./...
git add go.mod go.sum internal/audit
git commit -m "refactor(audit): use shared ky-primitives/auditchain"
```

---

## Task 3: Give kypassword-server a keyed chain

**Files:**
- Modify: `kypassword-server/internal/audit/audit.go`
- Test: `kypassword-server/internal/audit/audit_test.go`
- Modify: whatever config supplies secrets to `audit.NewStore`

**Depends on:** Tasks 1 and 2.

This is the task that closes the forgeable-trail gap. It is also the only one that changes an existing chain's hashes, so it carries the rekey.

- [ ] **Step 1: Write the failing test for the property that is missing**

```go
package audit

import (
	"os"
	"path/filepath"
	"testing"
)

// An unkeyed chain can be recomputed wholesale by anyone who can write the audit
// file, so it detects corruption but not tampering. This test is the difference.
func TestForgedChainDoesNotVerifyUnderKey(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, []byte("ledger-key"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Append("login", "alice", "device-1", "10.0.0.1", "{}"); err != nil {
			t.Fatal(err)
		}
	}

	// An attacker rewrites the trail and recomputes the chain the only way they
	// can without the key: unkeyed.
	path := filepath.Join(dir, auditFileName)
	forged, err := forgeUnkeyedChain(path, "login", "mallory")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, forged, 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewStore(dir, []byte("ledger-key"))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := reopened.VerifyIntegrity()
	if err == nil && ok {
		t.Fatal("a forged, unkeyed-recomputed chain must not verify under the ledger key")
	}
}
```

`forgeUnkeyedChain` is a test helper you write in the same file: read the JSONL, replace the actor on every entry, then recompute each entry's `PrevHash` and `Hash` with `auditchain.Hash(nil, prev, event)`. Write it explicitly rather than reaching for a mock — the point is to reproduce exactly what an attacker without the key can do.

- [ ] **Step 2: Run it and watch it fail**

```bash
cd /home/yoshi/busness.app/kypassword-server
go test ./internal/audit/ -run TestForgedChain -v
```

Expected: a compile error, because `NewStore` currently takes only `dir`. That is the point — the key does not exist yet.

- [ ] **Step 3: Port the tuple and take the key**

Change `NewStore(dir string)` to `NewStore(dir string, key []byte)`, and replace `computeHash` with `auditchain.Hash(s.key, prevHash, event)`. Map the existing `Entry` fields onto `auditchain.Event`:

| `Entry` field | `auditchain.Event` field |
|---|---|
| `Index` | `Seq` |
| `Action` | `Action` |
| `UserID` | `Actor` |
| `DeviceID` | `TargetID` |
| `Timestamp` | `CreatedAt` |
| `Details` | `DetailsJSON` |

`IPAddress` has no home in the canonical tuple. **Do not silently drop it** — fold it into `DetailsJSON` so it stays in the record and stays covered by the hash:

```go
details := fmt.Sprintf(`{"ip":%q,"details":%s}`, e.IPAddress, detailsOrNull(e.Details))
```

Keep `IPAddress` as a JSON field on `Entry` so the on-disk record still reads the same way; it is the *hashed tuple* that converges, not the stored shape.

- [ ] **Step 4: Port kyrecovery-server's rekey path**

Take `rekeyLegacyChain` from `kyrecovery-server/internal/audit/ledger.go` lines 125–175 and adapt it to the JSONL store: on open, if the chain verifies unkeyed but not keyed, recompute every record under the key, rewrite the file atomically, and record that the rekey happened.

**Write the rekeyed file to a temporary path in the same directory and `os.Rename` it into place.** A rekey interrupted halfway through an in-place rewrite leaves an audit trail that verifies under neither scheme, and rename is atomic on POSIX. This is also what makes the operation safe to run twice: a second open finds a chain that already verifies keyed and does nothing.

- [ ] **Step 5: Write the rekey test**

```go
// A trail written before the key existed must survive the transition. Losing it
// would destroy exactly the evidence the trail is kept for.
func TestLegacyChainSurvivesRekey(t *testing.T) {
	dir := t.TempDir()
	legacy, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Append("login", "alice", "device-1", "10.0.0.1", "{}"); err != nil {
		t.Fatal(err)
	}
	entriesBefore, err := legacy.Entries()
	if err != nil {
		t.Fatal(err)
	}

	rekeyed, err := NewStore(dir, []byte("ledger-key"))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := rekeyed.VerifyIntegrity()
	if err != nil || !ok {
		t.Fatalf("rekeyed chain must verify: ok=%v err=%v", ok, err)
	}

	entriesAfter, err := rekeyed.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatalf("rekey lost records: %d -> %d", len(entriesBefore), len(entriesAfter))
	}
	if entriesAfter[0].Action != entriesBefore[0].Action || entriesAfter[0].UserID != entriesBefore[0].UserID {
		t.Fatal("rekey altered a record's content")
	}

	// Running it again must be a no-op, not a second rewrite.
	again, err := NewStore(dir, []byte("ledger-key"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := again.VerifyIntegrity(); err != nil || !ok {
		t.Fatalf("second open must be a no-op: ok=%v err=%v", ok, err)
	}
}
```

- [ ] **Step 6: Run everything and commit**

```bash
gofmt -l . && go vet ./... && go test -race -count=1 ./...
git add go.mod go.sum internal/audit
git commit -m "feat(audit): key the audit chain and adopt the shared record

The chain was plain SHA-256 with no secret, so anyone who could write
the audit file could recompute every hash after a record they altered
and still pass verification. Existing trails are rekeyed in place
rather than discarded."
```

---

## Task 4: Migrate kybookmarks-server onto the shared package

**Files:**
- Modify: `kybookmarks-server/internal/audit/audit.go`
- Test: `kybookmarks-server/internal/audit/audit_test.go`

**Depends on:** Tasks 1 and 2.

This repo is already keyed, so there is no security gap to close — only the tuple to converge. It still needs the rekey path, because its tuple changes even though its keying does not.

- [ ] **Step 1: Prove the existing tests pass first**

```bash
cd /home/yoshi/busness.app/kybookmarks-server
go test -race -count=1 ./internal/audit/... 2>&1 | tee /tmp/kybookmarks-audit-before.txt
```

- [ ] **Step 2: Map the fields**

| `Entry` field | `auditchain.Event` field |
|---|---|
| `ID` | — see below |
| `Action` | `Action` |
| `UserID` | `Actor` |
| `DeviceID` | `TargetID` |
| `Timestamp` | `CreatedAt` |
| `Details` | `DetailsJSON` |
| `IP` | folded into `DetailsJSON` |

`Entry.ID` is a string; `auditchain.Event.Seq` is an `int64` position. They are not the same thing. Add a sequence number to `Entry` rather than parsing the ID — the chain needs a position, and the ID stays as the record's own identifier. Keep `ID` as a JSON field so existing readers are unaffected.

- [ ] **Step 3: Delegate and rekey**

Replace `computeHash` with `auditchain.Hash(l.secret, prevHash, event)` and port the same atomic rekey-on-open from Task 3 Step 4. Same reasoning: rename into place, idempotent on a second open.

- [ ] **Step 4: Fix the stale comment**

`Logger`'s doc comment says *"persists and verifies audit entries with SHA-256 hash chaining."* It is HMAC-SHA256 and has been for some time. Say what it does:

```go
// Logger persists and verifies audit entries chained with HMAC-SHA256 under a
// server secret, so a trail cannot be recomputed by someone who can only write
// the file.
```

- [ ] **Step 5: Run everything and commit**

```bash
gofmt -l . && go vet ./... && go test -race -count=1 ./...
git add go.mod go.sum internal/audit
git commit -m "refactor(audit): use shared ky-primitives/auditchain"
```

---

## Task 5: One verifier for the suite

**Files:**
- Create: `ky-primitives/cmd/kyauditverify/main.go`

**Depends on:** Tasks 2, 3 and 4.

The stated reason for consolidating the chain was that *"two products that chain differently cannot have their trails verified by one tool."* Until that tool exists, the consolidation has not delivered anything.

- [ ] **Step 1: Write it**

A CLI taking a JSONL audit file and a key, calling `auditchain.Verify`, and reporting the first record that fails and why. Roughly forty lines.

- [ ] **Step 2: Prove it against all three products**

Run it against a real trail from `kypassword-server`, `kybookmarks-server` and `kyrecovery-server`. All three must verify. **If one does not, the convergence is incomplete** — say which and why rather than special-casing it in the tool.

- [ ] **Step 3: Commit**

```bash
git add cmd/kyauditverify
git commit -m "feat: one audit verifier for the suite"
```

---

## What this plan deliberately does not do

- **It does not give `kysignon-server` an audit chain.** It has none. Whether the suite's identity provider should have a tamper-evident trail is a genuine product question and probably a yes — but it is a feature with its own threat model, storage decision and retention policy, and bolting it onto a consolidation plan would get it built without any of those being decided. **Raise it separately.**
- **It does not unify the audit APIs.** `VerifyIntegrity`, `VerifyChain() (bool, int, error)` and `VerifyChain(ctx) (ChainStatus, error)` stay as they are. The chain converges; the per-product surface does not need to.
- **It does not move `internal/audit` wholesale.** Only the chain arithmetic. Storage, retention and redaction are each product's own.
