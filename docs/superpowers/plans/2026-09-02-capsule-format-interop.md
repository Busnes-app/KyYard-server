# Capsule Format Interoperability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make one shared capsule package able to open every `.kycap` the suite has already written, before any repo is migrated onto it.

**Architecture:** The suite's three `capsule.go` files write mutually unreadable ciphertext. A shared package therefore cannot pick one encoding — it must detect which encoding a capsule uses and decode accordingly, proven against real capsules generated from each product as it stands today.

**Tech Stack:** Go 1.26+, standard library only.

**Spec:** [The suite shared primitives plan](2026-09-02-suite-shared-primitives.md), Task 2 Step 3, and [the Shamir interop findings](../../shamir-interop-findings.md), whose method this plan repeats for the capsule.

## Global Constraints

- **No behaviour change to any capsule that already exists on disk. Existing backups must still restore.** Copied verbatim from the parent plan; this plan exists because that constraint is at risk.
- The shared module has **zero dependencies**. Standard library only, forever.
- Every consuming repo keeps its gates green: `gofmt -l .` empty, `go vet ./...`, `go test -race ./...`.
- Module path prefix is `github.com/Busness-app/`, matching the GitHub org's casing. See [the module path migration plan](2026-09-02-module-path-migration.md).

---

## Decision and correction, 2026-09-02

**This plan's premise was wrong and Task 1 disproved it.** See
[the capsule interop findings](../../capsule-interop-findings.md) for the measured detail.

Corrected model:

- There are **four** implementations, not three. `kyrecovery-server/internal/capsule` was
  missed by a filename-based survey and is one of only two that persist anything.
- The two persisted formats diverge at the **container** layer, not the encoding layer:
  KySignOn writes JSON (`kycap/1`), KyRecovery writes a tar of `manifest.json`,
  `nonce.bin`, `payload.enc`. Cross-parsing fails in both directions before any key is
  examined.
- `ky_server_base` and `gridlock-server` **persist no capsule at all**, so the base64
  alphabet fork orphans nothing. The tolerant decoder is still worth having; it is not a
  data-loss fix.

**Decision (Yoshi, 2026-09-02): the suite writes KySignOn's `kycap/1` JSON container, and
reads both.** Reading KyRecovery's tar must keep working — those capsules hold real
recovery data on disk today.

Tasks 2 and 3 below are superseded by Tasks 4 and 5.

## Why this plan exists

The parent plan's Task 2 Step 3 says: *"Port the capsule format with a golden capsule fixture generated from the current `kysignon-server` implementation, so a capsule written before this plan opens afterwards."*

**Following that instruction as written would make ky_server_base and gridlock-server unable to open their own existing backups.** The three implementations do not share a format.

### Measured, 2026-09-02

```
gridlock-server/internal/backup/capsule.go   md5 e93fdf6c…   223 lines
ky_server_base/internal/backup/capsule.go    md5 9a17784d…   226 lines
kysignon-server/internal/backup/capsule.go   md5 95bd5e86…   349 lines
```

Three distinct files — unlike `shamir.go`, where gridlock and the scaffold are byte-identical. All three expose the same signature:

```go
func CreateCapsule(serviceName, appVersion string, files []BackupFile, deps, recipe map[string]interface{}, threshold, totalShares int) (*Capsule, []byte, error)
```

The same trap as Shamir: identical API, different implementation, nothing to catch a swap at compile time.

### The divergence is in the ciphertext encoding, and it is real

`capsule.go` delegates encryption to each repo's `internal/crypto`, and those have forked in both signature and output encoding:

| Repo | Signature | Ciphertext encoding |
|---|---|---|
| `ky_server_base` | `EncryptAESGCM(plaintext []byte, key string)` | `base64.RawURLEncoding` |
| `gridlock-server` | `EncryptAESGCM(plaintext []byte, key string)` | `base64.RawURLEncoding` |
| `kysignon-server` | `EncryptAESGCM(key []byte, plaintext []byte)` | `base64.StdEncoding` |

The cryptography is the same — AES-256-GCM, nonce prefixed, `gcm.Seal(nonce, nonce, plaintext, nil)`. Only the encoding of the result differs: unpadded URL alphabet versus padded standard alphabet.

**This was tested, not inferred.** Both `internal/crypto` packages were copied verbatim into one module and cross-decoded:

```
base     ciphertext: 31tIUDaMmp700X0EuEUzKkuBXM7KBgnHuyEwiE_cgq4tGQgfCgNBgpqfS7OQ04badhrt-R650FR2T23eSWMAEg-CYuI
kysignon ciphertext: hziBpmn8IxZ2B3tqcydu079C8Xp2wfKNjjvjUSAwvu91CrzsNDM3B3YZiDKksMEx6jzHHmFEzaiNcocFTJpRduekdLs=

base -> kysignon: illegal base64 data at input byte 38
kysignon -> base: illegal base64 data at input byte 91
```

Two camps, and they cannot read each other. The good news is that it fails cleanly at the decoder rather than silently producing wrong plaintext — but a migration that standardises on either encoding orphans the other camp's backups.

**This is the answer to the gate the parent plan never ran.** Unlike Shamir, which passed, the capsule gate fails.

---

## Design decisions

**1. The shared package detects the encoding; it does not pick one.** Both alphabets are unambiguous in practice — `RawURLEncoding` output has no padding and may contain `-` or `_`; `StdEncoding` output is padded to a multiple of four and may contain `+` or `/`. Detection is a few lines and it is the only option that keeps both camps' backups readable.

**2. New capsules are written in one encoding.** Reading must accept both forever; writing should converge. `base64.StdEncoding` is the choice, because it is what the product with the most valuable recovery kits already writes and because padded standard base64 is what an operator pasting a blob into a tool will expect.

**3. Fixtures come from all three products, not one.** The parent plan's single-fixture instruction is what hid this problem. Every product contributes a real capsule, and the shared package must open all of them.

**4. Encoding detection lives behind the capsule API, not in `crypto`.** The `internal/crypto` signature fork is a separate, larger cleanup. This plan takes the narrowest change that protects existing backups.

---

## Task 1: Prove the divergence with real capsules, not just ciphertext

**Files:**
- Create: `/tmp/capsule-interop/` (throwaway harness, committed nowhere)
- Create: `ky_server_base/testdata/capsules/` (three real capsules, committed)
- Create: `ky_server_base/docs/capsule-interop-findings.md`

The ciphertext-level result above is established. What is not yet established is whether the **manifest** also diverges — field names, ordering, or the container's outer JSON. A capsule is more than its ciphertext, and a shared `Open` must parse the whole thing.

- [ ] **Step 1: Generate one real capsule from each of the three implementations**

Write a small generator per repo that calls that repo's own `CreateCapsule` with a fixed input, and save the result. Run it from inside each repo so it links that repo's code:

```bash
cd /home/yoshi/busness.app/kysignon-server
cat > /tmp/gen_capsule_test.go <<'GOEOF'
package backup

import (
	"encoding/hex"
	"os"
	"testing"
)

func TestGenerateFixtureCapsule(t *testing.T) {
	files := []BackupFile{{Path: "config.json", Content: []byte(`{"a":1}`), Mode: 0o600}}
	capsule, key, err := CreateCapsule("fixture", "0.0.0", files, nil, nil, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := SerializeCapsule(capsule)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/tmp/capsule-kysignon.kycap", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/tmp/capsule-kysignon.key", []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
}
GOEOF
cp /tmp/gen_capsule_test.go internal/backup/
go test -run TestGenerateFixtureCapsule ./internal/backup/
rm internal/backup/gen_capsule_test.go
```

**Read the repo's own `capsule.go` first.** The names above (`SerializeCapsule`, `BackupFile{Path, Content, Mode}`) are the shape the scaffold uses; the three files differ and yours may not match. Adapt the generator to the actual API rather than adapting the API to the generator, and if there is no serialisation helper, marshal the `*Capsule` with `encoding/json` exactly as that repo's caller does.

Repeat for `gridlock-server` and `ky_server_base`.

**Delete the generator from each repo when done.** It is a fixture-making tool, not a test — leaving it behind means every future run rewrites the fixtures, which defeats their purpose.

- [ ] **Step 2: Commit the three capsules and their keys as fixtures**

```bash
mkdir -p /home/yoshi/busness.app/ky_server_base/testdata/capsules
cp /tmp/capsule-*.kycap /tmp/capsule-*.key \
   /home/yoshi/busness.app/ky_server_base/testdata/capsules/
```

These are fixture keys protecting fixture data. They are not secrets and they belong in the repo — a golden capsule with its key withheld cannot be a regression test. Add a `README.md` in that directory saying exactly that, so nobody "fixes" it later:

```markdown
# Capsule fixtures

One real `.kycap` from each of the suite's three capsule implementations, with the
key that opens it. These keys protect fixture data and nothing else. They are
committed on purpose: a golden capsule whose key is withheld cannot prove that a
capsule written before a migration still opens after it.

Regenerate these only if the capsule format changes deliberately. A change that
stops one of these opening is a breaking change to every backup already on disk.
```

- [ ] **Step 3: Diff the three manifests**

```bash
cd /home/yoshi/busness.app/ky_server_base/testdata/capsules
for f in *.kycap; do echo "--- $f"; python3 -m json.tool "$f" | head -40; done
```

Record which fields each manifest carries, in which order, and whether the outer container shape matches. Field *ordering* in JSON does not affect parsing, but a field present in one and absent in another does.

- [ ] **Step 4: Write the findings down**

Create `docs/capsule-interop-findings.md` in the same shape as `docs/shamir-interop-findings.md`: what was compared, the encoding table above, the manifest diff from Step 3, the verdict, and the exact commands to reproduce.

State the verdict plainly at the top. As of this writing the expected verdict is **"the three implementations are not interchangeable; a shared package must read both encodings"** — but if Step 3 turns up a manifest divergence too, say so, because that widens the shared package's job beyond encoding detection.

- [ ] **Step 5: Commit**

```bash
cd /home/yoshi/busness.app/ky_server_base
gofmt -l . && go vet ./... && go test -race -count=1 ./...
git add docs/capsule-interop-findings.md testdata/capsules
git commit -m "docs: establish capsule format divergence across the suite"
```

---

## Task 2: Give the shared capsule package encoding detection

**Files:**
- Create: `ky-primitives/capsule/encoding.go`
- Test: `ky-primitives/capsule/encoding_test.go`

**Depends on:** the `ky-primitives` module existing (parent plan Task 2 Step 1).

**Interfaces produced:**
- `capsule.DecodeCiphertext(s string) ([]byte, error)` — accepts either encoding
- `capsule.EncodeCiphertext(b []byte) string` — always writes `base64.StdEncoding`

- [ ] **Step 1: Write the failing test**

```go
package capsule

import (
	"bytes"
	"encoding/base64"
	"testing"
)

// The suite wrote capsules in two base64 alphabets before these were unified.
// Both must decode forever; anything else orphans backups already on disk.
func TestDecodeCiphertextAcceptsBothEncodings(t *testing.T) {
	// Chosen so the raw bytes encode to a string containing a character that
	// differs between the two alphabets: 0xFB 0xFF -> "+/" std, "-_" url.
	raw := []byte{0xFB, 0xFF, 0x00, 0x11, 0x22}

	std := base64.StdEncoding.EncodeToString(raw)
	url := base64.RawURLEncoding.EncodeToString(raw)
	if std == url {
		t.Fatal("test input does not distinguish the alphabets; pick different bytes")
	}

	for name, encoded := range map[string]string{"std": std, "rawurl": url} {
		got, err := DecodeCiphertext(encoded)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, raw) {
			t.Errorf("%s: got %x want %x", name, got, raw)
		}
	}
}

func TestDecodeCiphertextRejectsGarbage(t *testing.T) {
	if _, err := DecodeCiphertext("not valid base64 !!!"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestEncodeCiphertextIsStandard(t *testing.T) {
	raw := []byte{0xFB, 0xFF, 0x00}
	if got, want := EncodeCiphertext(raw), base64.StdEncoding.EncodeToString(raw); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
cd ky-primitives && go test ./capsule/
```

Expected: `undefined: DecodeCiphertext`.

- [ ] **Step 3: Implement**

```go
package capsule

import (
	"encoding/base64"
	"fmt"
)

// DecodeCiphertext reads a capsule's ciphertext field.
//
// The suite wrote capsules in two alphabets before they were unified:
// ky_server_base and gridlock-server used base64.RawURLEncoding, kysignon-server
// used base64.StdEncoding. Both must keep decoding or backups already on disk
// stop opening.
//
// Trying standard first is unambiguous, not a guess. Standard requires padding
// to a multiple of four and rejects the '-' and '_' that raw-url uses, so a
// raw-url string either fails here and falls through, or contains only
// characters both alphabets agree on and decodes to the same bytes. Verified
// against 51,200 random inputs spanning every length class.
func DecodeCiphertext(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("capsule ciphertext is neither standard nor raw-url base64: %w", err)
	}
	return b, nil
}

// EncodeCiphertext writes new capsules in one encoding. Reading stays permissive;
// writing converges.
func EncodeCiphertext(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
```

- [ ] **Step 4: Run the tests and watch them pass**

```bash
cd ky-primitives && go test -race -count=1 ./capsule/
```

- [ ] **Step 5: Commit**

```bash
git add capsule/encoding.go capsule/encoding_test.go
git commit -m "feat(capsule): decode both base64 alphabets the suite has written"
```

---

## Task 3: Prove the shared package opens all three products' capsules

**Files:**
- Create: `ky-primitives/capsule/capsule.go`
- Create: `ky-primitives/capsule/fixtures_test.go`
- Create: `ky-primitives/testdata/capsules/` (copies of Task 1's fixtures)

**Interfaces produced:**
- `capsule.File{Path string, Content []byte, Mode os.FileMode}`
- `capsule.Open(raw []byte, key []byte, targetDir string) ([]File, error)` — parses the
  container, decrypts with either ciphertext encoding, verifies the payload SHA-256, and
  extracts into `targetDir` under the hardening described in Step 4
- `capsule.Seal(serviceName, appVersion string, files []File, deps, recipe map[string]interface{}, threshold, totalShares int) (raw []byte, key []byte, err error)`

`Open` takes the key as raw bytes, not a hex string: the two repos disagree on that too
(`ky_server_base` passes hex, `kysignon-server` passes `[]byte`), and bytes is the one
that cannot be got wrong silently.

This is the task that makes the global constraint true rather than aspirational.

- [ ] **Step 1: Copy the fixtures into the module**

```bash
cp /home/yoshi/busness.app/ky_server_base/testdata/capsules/*.kycap \
   /home/yoshi/busness.app/ky_server_base/testdata/capsules/*.key \
   ky-primitives/testdata/capsules/
```

They live in both places on purpose: `ky-primitives` needs them to prove its own contract, and `ky_server_base` needs them to prove its migration did not break anything.

- [ ] **Step 2: Write the failing test**

```go
package capsule_test

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busness-app/ky-primitives/capsule"
)

// One real capsule from each implementation the suite has shipped. If any of
// these stops opening, a backup already on disk has stopped opening.
func TestOpensEveryShippedCapsule(t *testing.T) {
	paths, err := filepath.Glob("../testdata/capsules/*.kycap")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 3 {
		t.Fatalf("expected a fixture from each of the three implementations, found %d", len(paths))
	}

	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			keyHex, err := os.ReadFile(strings.TrimSuffix(p, ".kycap") + ".key")
			if err != nil {
				t.Fatal(err)
			}
			key, err := hex.DecodeString(strings.TrimSpace(string(keyHex)))
			if err != nil {
				t.Fatalf("fixture key is not hex: %v", err)
			}
			files, err := capsule.Open(raw, key, t.TempDir())
			if err != nil {
				t.Fatalf("capsule from a shipped implementation failed to open: %v", err)
			}
			if len(files) == 0 {
				t.Fatal("opened but produced no files")
			}
		})
	}
}
```

- [ ] **Step 3: Run it and watch it fail**

```bash
cd ky-primitives && go test ./capsule/
```

Expected: `undefined: capsule.Open`, or a decode failure on at least one fixture.

- [ ] **Step 4: Implement `capsule.Open` on top of `DecodeCiphertext`**

Port the parse-and-decrypt path from `kysignon-server/internal/backup/capsule.go` — it is the longest of the three (349 lines against 226 and 223) because it carries the hardening the others lack: `safeRelPath`, `prepareTargetDir`, `countingReader`, and `O_EXCL|O_NOFOLLOW` on write. Take that version, and replace only its base64 call with `DecodeCiphertext`.

**Do not port the extraction hardening loosely.** `ExtractCapsule` rejecting absolute paths, `../` traversal, symlinks and oversized archives is a security contract named in `ky_server_base/internal/backup/AGENTS.md`. Carry the tests that cover it across at the same time, and if the scaffold's copy has hardening KySignOn's lacks, keep both.

- [ ] **Step 5: Run the tests and watch them pass**

```bash
cd ky-primitives && go test -race -count=1 ./capsule/
```

All three fixtures must open. If one does not, **stop** — that is a manifest divergence Task 1 Step 3 missed, and it needs to go into the findings document before any repo migrates.

- [ ] **Step 6: Commit**

```bash
git add capsule/ testdata/capsules/
git commit -m "feat(capsule): open capsules from every implementation the suite shipped"
```

---

## What this plan deliberately does not do

- **It does not unify `internal/crypto`.** The `EncryptAESGCM` signature fork — `(plaintext, keyString)` against `(key, plaintext)` — is real and worth fixing, but it is a five-repo change with no data-loss risk, and bundling it here would put the backup-compatibility work behind it.
- **It does not rewrite existing capsules.** Detection on read means no migration pass over anyone's backups, which is the whole point.
- **It does not change what any product writes today.** Products converge on `StdEncoding` when they migrate onto the shared package, not before.

---

## Outcome, 2026-09-02

**Done.** `ky-primitives/capsule` exists at `/home/yoshi/busness.app/ky-primitives`,
commit `908ddf0`, on disk only — the GitHub repo has not been created and nothing has been
pushed.

Built against the corrected model, not the original one:

- `Open(raw, key, targetDir)` reads **both** persisted containers.
- `Seal(...)` writes `kycap/1` only.
- `DecodeCiphertext` / `EncodeCiphertext` as specified in Task 2, with an added
  exhaustive test that trying standard before raw-url is unambiguous across every length
  class.
- The extraction hardening is ported from `kysignon-server` and applied to **both**
  containers, so a KyRecovery capsule opened through this package gets path, type, size
  and mode checks its own `Unpack` never applied.

### Compatibility, proven in both directions

| Check | Result |
|---|---|
| `ky-primitives` opens `kysignon.kycap` | PASS |
| `ky-primitives` opens `kyrecovery.kycap` | PASS |
| `kysignon-server` `ParseCapsule`+`ExtractCapsule` open a capsule sealed by `ky-primitives`, unmodified | PASS |

The third is the one that matters for migration: a product moved onto the shared package
writes capsules the unmigrated products can still read.

### Deviations from this plan

1. **Fixtures were not copied into `ky_server_base/testdata/capsules/`.** The plan wanted
   them in both places so the scaffold could prove its migration broke nothing. The
   scaffold persists no capsule, so it has no on-disk compatibility to prove and the copy
   would be decoration. The fixtures live in `ky-primitives/testdata/capsules/` only.
2. **Only two fixtures, not three.** The third and fourth implementations persist nothing.
3. **`Seal` does not split the key into Shamir shares.** `kycap/1` has never carried
   shares; they belong to the recovery kit beside the capsule. Callers split the returned
   key.

### Left open

- **The GitHub repo does not exist.** `github.com/Busness-app/ky-primitives` must be
  created before any repo can depend on this.
- **No product has been migrated onto it.** That is the next plan, and it needs the
  repo published first.
- **`kyrecovery-server`'s own `Unpack` still has no path hardening** — it puts
  `hdr.Name` straight into a map with no `safeRelPath` and no entry-type check. Opening
  through `ky-primitives` fixes that for callers who migrate; it does not fix KyRecovery
  itself.
- **The recovery kit question.** `ky_server_base` and `gridlock-server` hand an operator
  a kit containing the shares but not the payload. Whether that is intended was not
  established here.
