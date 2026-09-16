# kysignon-server Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move `kysignon-server` onto the suite's shared backup primitives without changing what any recovery kit or capsule already in existence can do.

**Architecture:** KySignOn is the donor for both primitives and the most demanding consumer, so it migrates first. Its `internal/backup` keeps its exported API and becomes a thin delegation to `ky-primitives`.

**Tech Stack:** Go 1.26+, standard library plus this repo's three direct dependencies.

**Spec:** [The suite shared primitives plan](2026-09-02-suite-shared-primitives.md) Task 3, plus [the Shamir interop findings](../../shamir-interop-findings.md) and [the capsule interop plan](2026-09-02-capsule-format-interop.md).

## Global Constraints

- **No behaviour change to any capsule or recovery kit that already exists.** KySignOn's kits are the suite's most valuable artefact and the reason the parent plan gates on compatibility.
- `go.mod` direct requires go from **three to four**, with **no new indirect entries**. This repo's architecture rests on a small dependency tree; a migration that grows it has failed.
- Gates stay green: `gofmt -l .` empty, `go vet ./...`, `go test -race ./...`, `govulncheck ./...`.
- Module path prefix is `github.com/Busness-app/`.

---

## Why this repo goes first

It has the most demanding dependency constraint, the most valuable recovery kits, and the best test coverage. If `ky-primitives` cannot serve KySignOn it cannot serve anyone, and that is better learned here than after four migrations.

It is also the **donor** for both primitives:

- **Shamir:** [proven](../../shamir-interop-findings.md) behaviourally identical to the scaffold's copy across 7,200 randomised round trips. Its copy is the more legible of the two — it states the Lagrange identity in a comment and explains why `0 - x` and `x_j - x_m` are both XOR in GF(2^8). The parent plan's Task 2 Step 2 takes this version.
- **Capsule:** at 349 lines against the scaffold's 226, its copy carries hardening the others lack — `safeRelPath`, `prepareTargetDir`, `countingReader`, and `O_EXCL|O_NOFOLLOW` on write.

One correction to the parent plan applies here. Task 2 Step 4 says to take the audit hash chain *"from `kyrecovery-server` or `kysignon-server`, whichever verifier is more complete."* **`kysignon-server` has no hash chain at all** — `internal/audit/audit.go` is a structured JSON logger, and `PrevHash` appears nowhere in the repo. The choice has one option. See [the audit chain plan](2026-09-02-audit-chain-convergence.md).

---

## Task 1: Adopt ky-primitives for Shamir

**Files:**
- Modify: `internal/backup/shamir.go`, `go.mod`, `go.sum`
- Create: `testdata/shamir-vectors.json`, `internal/backup/shamir_vectors_test.go`

**Depends on:** `ky-primitives` published (parent plan Task 2).

**Interfaces consumed:**
- `shamir.Split(secret []byte, k, n int) ([]Share, error)`
- `shamir.Combine(shares []Share, k int) ([]byte, error)`
- `shamir.Share{Index int, Data []byte}`

- [ ] **Step 1: Bring the golden vectors in and watch them pass first**

```bash
cd /home/yoshi/busness.app/kysignon-server
mkdir -p testdata
cp /home/yoshi/busness.app/ky_server_base/testdata/shamir-vectors.json testdata/
cp /home/yoshi/busness.app/ky_server_base/internal/backup/shamir_vectors_test.go internal/backup/
sed -i 's|github.com/Busness-app/ky_server_base/internal/backup|github.com/Busness-app/kysignon-server/internal/backup|' \
  internal/backup/shamir_vectors_test.go
go test -race -count=1 -run TestShamirGoldenVectors ./internal/backup/...
```

Expected: PASS **before** anything is replaced. The vectors were generated from this repo's implementation, so a failure here means the vectors or the copy are wrong, not the migration.

- [ ] **Step 2: Add the dependency and check its cost**

```bash
grep -c "^	" go.mod
go list -m all | wc -l
go get github.com/Busness-app/ky-primitives@v0.1.0
go mod tidy
go list -m all | wc -l
cat go.mod
```

Direct requires must be exactly four. The module count must rise by exactly one. **If any new indirect entry appears, stop** — `ky-primitives` broke its zero-dependency contract, and this repo is the one that cannot absorb it.

- [ ] **Step 3: Delegate, keeping the package boundary**

```go
package backup

import (
	"github.com/Busness-app/ky-primitives/shamir"
)

// Share is one custodian's key shard in a (k, n) threshold scheme.
type Share = shamir.Share

// SplitSecret splits a secret into n shares with a reconstruction threshold of k.
func SplitSecret(secret []byte, k, n int) ([]Share, error) {
	return shamir.Split(secret, k, n)
}

// CombineShares reconstructs the secret from any k valid shares.
func CombineShares(shares []Share, k int) ([]byte, error) {
	return shamir.Combine(shares, k)
}
```

`Share` is a type alias, not a defined type. `cmd/kyrestore/main.go` builds `backup.Share` values from its `-shard INDEX:HEX` flag, and an alias keeps that compiling untouched.

Check whether the error sentinels are referenced before dropping them:

```bash
grep -rn "ErrInvalidThreshold\|ErrNotEnoughShares" --include='*.go' .
```

If they are, re-export: `var ErrInvalidThreshold = shamir.ErrInvalidThreshold`, likewise `ErrNotEnoughShares`.

- [ ] **Step 4: Nothing outside internal/backup may need to change**

```bash
go build ./...
```

If a caller outside `internal/backup` needs editing, **say so in your report** — it means `ky-primitives`' API does not match what the suite actually needs, which is a finding about the module, not about this repo.

- [ ] **Step 5: Run the restore drill and the full suite**

```bash
go test -race -count=1 -v ./internal/backup/...
go test -race -count=1 ./...
```

`backup_test.go`, `hardening_test.go` and the vector test must all pass unedited.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./... && go test -race -count=1 ./... && govulncheck ./...
git add go.mod go.sum internal/backup testdata
git commit -m "refactor(backup): use shared ky-primitives for Shamir"
```

---

## Task 2: Fix the duplicate-shard panic in kyrestore

**Files:**
- Modify: `cmd/kyrestore/main.go`
- Test: `cmd/kyrestore/main_test.go`

**Depends on:** nothing. Can be done before or after Task 1.

[The interop findings](../../shamir-interop-findings.md) turned this up: supplying the same shard twice zeroes the Lagrange denominator and `gfDiv` panics with `divide by zero in GF(256)`. `cmd/kyrestore` is the only production caller, and its interface is a repeated `-shard INDEX:HEX` flag — so a custodian who pastes the same shard twice, during a disaster, gets a Go stack trace instead of a sentence telling them what they did.

The fix belongs here rather than in `ky-primitives` because `Combine` rejecting duplicates is a change to the shared contract, and this is the caller that can validate its own input cheaply. Raise the shared-package change separately if you want both.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"strings"
	"testing"

	"github.com/Busness-app/kysignon-server/internal/backup"
)

// A custodian pasting the same shard twice must be told so, not handed a panic
// from inside GF(256) arithmetic in the middle of a disaster recovery.
func TestRunRejectsDuplicateShardIndex(t *testing.T) {
	shards := []backup.Share{
		{Index: 1, Data: []byte{0x01, 0x02}},
		{Index: 1, Data: []byte{0x01, 0x02}},
		{Index: 2, Data: []byte{0x03, 0x04}},
	}
	err := run("/nonexistent.kycap", t.TempDir(), shards)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "twice") && !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error should name the duplicate shard, got: %v", err)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
go test ./cmd/kyrestore/ -run TestRunRejectsDuplicateShardIndex -v
```

Expected: a panic, `divide by zero in GF(256)`, or a file-not-found error from `os.ReadFile` — whichever comes first. Either way it is not the error the test wants.

- [ ] **Step 3: Validate before anything else in `run`**

Add this as the **first** thing `run` does, before `os.ReadFile`, so the message does not depend on the capsule path being valid:

```go
func run(capsulePath, outDir string, shards []backup.Share) error {
	seen := make(map[int]bool, len(shards))
	for _, s := range shards {
		if seen[s.Index] {
			return fmt.Errorf("shard %d was supplied twice; each -shard must come from a different custodian", s.Index)
		}
		seen[s.Index] = true
	}

	if capsulePath == "" || outDir == "" {
		return fmt.Errorf("both -capsule and -out are required")
	}
	// … existing body unchanged
}
```

- [ ] **Step 4: Run the test and watch it pass**

```bash
go test -race -count=1 ./cmd/kyrestore/ -v
```

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./... && go test -race -count=1 ./...
git add cmd/kyrestore
git commit -m "fix(kyrestore): reject duplicate shards instead of panicking

Supplying the same shard twice zeroes the Lagrange denominator and
panics inside GF(256) arithmetic. A custodian doing this during a
recovery deserves a sentence, not a stack trace."
```

---

## Task 3: Adopt ky-primitives for the capsule format

**Files:**
- Modify: `internal/backup/capsule.go`
- Test: `internal/backup/backup_test.go`, `internal/backup/hardening_test.go`

**Depends on:** Task 1, and **[the capsule interop plan](2026-09-02-capsule-format-interop.md) Task 3 complete and passing.**

This repo writes ciphertext in `base64.StdEncoding`; `ky_server_base` and `gridlock-server` write `base64.RawURLEncoding`, and the two are mutually unreadable. The shared package must read both before this repo moves onto it.

- [ ] **Step 1: Prove the pre-migration capsule opens, before changing anything**

```bash
go test -race -count=1 ./internal/backup/...
ls /home/yoshi/busness.app/ky_server_base/testdata/capsules/capsule-kysignon.kycap
```

- [ ] **Step 2: Delegate, keeping the exported signatures**

```go
func CreateCapsule(serviceName, appVersion string, files []BackupFile, deps, recipe map[string]interface{}, threshold, totalShares int) (*Capsule, []byte, error)
func ExtractCapsule(capsule *Capsule, key []byte, targetDir string) ([]BackupFile, error)
```

This repo's extraction hardening is the version `ky-primitives` adopted, so the delegation should be close to a straight substitution. **Verify that rather than assuming it** — `hardening_test.go` is the check, and it must pass unedited.

- [ ] **Step 3: Prove the pre-migration capsule still opens**

Add a test that opens `testdata/capsules/capsule-kysignon.kycap` through the post-migration path.

```bash
go test -race -count=1 -v ./internal/backup/...
```

**This is the most important check in this plan.** A failure here is data loss, not a test failure. Stop and report.

- [ ] **Step 4: Full gate and commit**

```bash
gofmt -l . && go vet ./... && go test -race -count=1 ./... && govulncheck ./...
git add go.mod go.sum internal/backup testdata
git commit -m "refactor(backup): use shared ky-primitives for the capsule format"
```

---

## Task 4: Document the shared primitives

**Files:**
- Modify: `AGENTS.md` (or `internal/backup/AGENTS.md`, matching where this repo keeps package docs)

- [ ] Add a `## Shared primitives` section stating: Shamir and the capsule format come from `github.com/Busness-app/ky-primitives`; the module is dependency-free by contract; **a local reimplementation of either is a defect, not an optimisation**; and the golden vectors in `testdata/` are the compatibility contract, so a change that alters one is a breaking change to every recovery kit already issued.

- [ ] Commit:

```bash
git add AGENTS.md
git commit -m "docs: record where the backup primitives come from"
```

---

## What this plan deliberately does not do

- **It does not give this repo an audit hash chain.** It has none today. Whether it should is a product question, raised in [the audit chain plan](2026-09-02-audit-chain-convergence.md).
- **It does not touch `internal/crypto`.** The `EncryptAESGCM` signature fork is a separate five-repo change.
- **It does not change `internal/sync`'s SCIM shaping**, which the scaffold borrows from. That flows the other way, in the scaffold's own dependency work.
