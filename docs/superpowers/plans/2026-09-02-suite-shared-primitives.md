# Suite Shared Primitives Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the KySecurity suite from carrying divergent copies of the primitives where divergence is a correctness bug rather than a maintenance annoyance — threshold key splitting, the backup capsule format, the hash-chained audit record, and the pairing protocol spec — and cut the scaffold back to the suite's dependency floor so every future product starts lean instead of starting heavy.

**Architecture:** A new dependency-free Go module holds only the primitives whose two implementations must agree byte-for-byte, plus golden test vectors that prove they do. The pairing protocol gets a single authoritative spec. `ky_server_base` keeps being a scaffold, consumes the module like every other product, and sheds the dependencies it does not need — replacing them with implementations the suite already ships and runs in production.

**Tech Stack:** Go 1.26 standard library only in the new module. No frontend.

**Spec:** The evidence below, plus `zero_code_pairing_handoff_spec.md` as it exists in `kyrecovery-server` (see Finding B for why that copy and not the others).

## Global Constraints

- **The new shared module has zero dependencies.** Standard library only, forever. It is importable by `kysignon-server`, whose entire architecture argument rests on having three direct dependencies; a module that drags in anything else is unusable there and the plan fails.
- Every consuming repo keeps its own gates green: `gofmt -l .` empty, `go vet ./...`, `go test -race ./...`, `govulncheck ./...`.
- **No behaviour change to any capsule, kit or audit record that already exists on disk.** Existing backups must still restore. Any change that cannot preserve that is out of scope and must be reported, not worked around.
- Module path and repo naming follow the GitHub org exactly (`github.com/Busness-app/<name>`) — see the amendment under Task 2.

---

## Why this plan is not what was originally proposed

The earlier suite analysis said: *consolidate the duplicated pairing and audit code into `ky_server_base`.* That was based on a wrong reading of what `ky_server_base` is, and following it would make things worse.

**`ky_server_base` is a scaffold, not a library.** It has its own `cmd/server/main.go`, its own store, its own API surface, and `prompt.md` describes it as "a base build with all the things needed to start a new Busness.app product." It is meant to be *copied* to start a product, not imported by finished ones.

**Its dependency philosophy is the opposite of KySignOn's.** `ky_server_base/go.mod` requires `github.com/coreos/go-oidc/v3`, `github.com/elimity-com/scim`, `github.com/jackc/pgx/v5` and `golang.org/x/oauth2`, and its most recent substantive commit is `refactor: delegate SCIM and OAuth handling to upstream libs`. `kysignon-server` advertises three direct dependencies and ~95% standard library as an architectural property. Importing `ky_server_base` into KySignOn would import that entire tree.

So the consolidation target is a **new, small, dependency-free module**, and `ky_server_base` becomes one of its consumers rather than its host.

That leaves the philosophical split itself, which Task 6 closes by moving the scaffold toward KySignOn rather than the other way round. The scaffold's job is to be the shape a new product starts in; 25 modules is the wrong shape to hand someone on day one, and every one of the heavyweight dependencies has a working stdlib counterpart already running in the suite.

---

## Findings that set the scope

Measured, not assumed. Reproduce any of these before starting.

### Finding A: three Shamir implementations, two distinct, identical API

```
kysignon-server/internal/backup/shamir.go   md5 dc8a6da3…   132 lines
gridlock-server/internal/backup/shamir.go   md5 a0d9905b…   130 lines
ky_server_base/internal/backup/shamir.go    md5 a0d9905b…   130 lines
```

Both expose exactly `SplitSecret(secret []byte, k, n int) ([]Share, error)` at line 59 and `CombineShares(shares []Share, k int) ([]byte, error)` at line 95. Same signatures, same line numbers, 32 lines of difference in between — this is a fork, not two designs.

This is the plan's reason to exist. Threshold splitting over GF(2^8) is the mechanism protecting the suite's disaster-recovery keys. Two implementations behind one API means a share set produced by one and reconstructed by the other either fails or, worse, returns a different key — and because the signatures match, no compiler and no type system will ever notice a swap. `kysignon-server`'s recovery kits are its strongest differentiating feature; a silent reconstruction failure discovered during an actual disaster is the worst possible time to learn the two copies drifted.

**Whether they are actually interoperable today is unknown and must be established first.** Task 1 answers it before anything is refactored.

### Finding B: the pairing spec is stale in eight repos and authoritative in one

`zero_code_pairing_handoff_spec.md` is byte-identical (md5 `24899bae8d11ac740c58dcc5c3581e32`) in `kysignon-server`, `kypassword-server`, `kypost-server`, `kybookmarks-server`, `kynotes-server`, `kydns-server`, `gridlock-server` and `ky_server_base`.

`kyrecovery-server` — the product that *implements the server side of this protocol* — has a different copy, md5 `460f9957…`. The diff is entirely additive: its version documents constraints the other eight omit, including

- `ttl_minutes` defaults to 15 and is rejected above 60
- `429` responses after 10 claim attempts per source or 5 per code in 15 minutes
- 60 pushes per product per 15 minutes, 4 concurrent ingests
- a 64 MiB body cap (`KYRECOVERY_MAX_BACKUP_BYTES`), 4096 files, 32 MiB per file
- `total_shares` may not exceed 255, the GF(2^8) ceiling
- path escapes (`../`, absolute) fail the drill rather than being evaluated
- the API token is returned exactly once and cannot be read back

Eight clients are being written against a spec that omits the server's real limits. A client built to that spec has no reason to expect a `429`, no reason to chunk below 64 MiB, and no reason to persist the token on first receipt.

### Finding C: audit, crypto and devices are duplicated more widely but less dangerously

`internal/audit` exists separately in `kysignon-server`, `kypassword-server`, `kybookmarks-server`, `kyrecovery-server`. `internal/crypto` in five repos. `internal/devices` in four.

These matter less than Finding A because divergence mostly costs maintenance rather than correctness — except the **hash-chained audit record**, where two products that chain differently cannot have their trails verified by one tool, and a verifier written against one chain silently passes garbage from the other. That single piece joins the shared module; the rest stays where it is until there is evidence it needs to move.

---

## Design decisions

**1. A new module, `ky-primitives`, not a package inside an existing repo.** Every candidate host has dependencies that at least one consumer cannot accept. A separate module with an empty `require` block is the only shape that all nine repos can import.

**2. It contains only primitives whose implementations must agree byte-for-byte.** In scope: Shamir over GF(2^8), the capsule container format, the audit hash chain. Explicitly out of scope: HTTP handlers, stores, SSO, config, device pairing UX. Those legitimately differ per product, and pulling them in would recreate `ky_server_base` under a new name.

**3. Compatibility is proven by golden vectors, not by inspection.** The module ships a fixture set — share sets, capsules, audit chains — generated once and checked in. Every implementation, before and after migration, must reproduce them. This is what makes the migration safe and what stops the next fork from going unnoticed.

**4. The spec has exactly one home, and it is the implementer's.** `kyrecovery-server` implements the server side and its copy is a strict superset. That copy becomes authoritative; the other eight are replaced by a pointer to it rather than by a synced duplicate, because a synced duplicate is how eight copies drifted in the first place.

**5. Migration is per-repo and independently revertible.** No repo is required to migrate for another's migration to land. A repo that has not migrated keeps its own copy and stays correct.

**6. If Task 1 finds the two Shamir implementations are already incompatible, the plan stops and escalates.** That result is not a refactoring problem, it is a live disaster-recovery defect affecting kits already issued, and it needs a human decision about existing kits before any code moves.

---

## Task 1: Establish whether the two Shamir implementations interoperate

**Files:**
- Create: `/tmp/shamir-interop/` (throwaway harness, committed nowhere)
- Produces: a written answer, and the golden vectors used by every later task

This task writes no production code. It answers the question the rest of the plan depends on.

- [ ] **Step 1: Build the cross-check harness**

Create a temporary Go module that imports **both** implementations by copying each `shamir.go` into its own package (`impl_kysignon`, `impl_base`) with no other changes. Do not "clean up" either while copying — a whitespace fix that changes behaviour would invalidate the result.

- [ ] **Step 2: Establish round-trip compatibility in both directions**

For each of these cases, split with implementation A and combine with implementation B, then the reverse:

- secret of 32 bytes (the real case — an AES-256 key), `k=2, n=3`
- `k=3, n=5`
- `k=1, n=1` (degenerate)
- `k=n=255` (the GF(2^8) ceiling named in the spec)
- a secret containing `0x00` bytes, and one that is all `0x00` — zero handling is where GF(2^8) implementations most often differ
- a 1-byte secret and a 4 KiB secret

Assert the recovered secret equals the original **byte for byte**. A recovered secret of the right length but wrong content is the dangerous outcome and must be distinguished from an error return.

- [ ] **Step 3: Compare the share encoding**

Split the same secret with the same `k`/`n` under a fixed random source in both implementations and compare the `Share` values directly. If the wire encodings differ, shares written to a recovery kit by one product cannot be typed into another's restore flow by a custodian, even when the maths agrees. Report the encodings side by side.

- [ ] **Step 4: Write the answer down**

Record, in `docs/shamir-interop-findings.md` in this repo:
- the verdict per case, as a table
- the exact command to reproduce
- whether share encodings match
- if they diverge: which implementation is correct against the GF(2^8) definition, and how many recovery kits are known to exist under each

**If any case fails to round-trip, stop here and escalate.** Do not proceed to Task 2. A wrong-key reconstruction is a live defect in issued recovery kits, and which implementation becomes canonical is a decision with data-loss consequences that belongs to a human.

- [ ] **Step 5: Freeze the golden vectors**

From whichever implementation the previous step establishes as correct, generate and check into this repo a fixture file `testdata/shamir-vectors.json`: for each case, the secret, `k`, `n`, a fixed seed, and the resulting shares, all hex-encoded. Every later task validates against this file.

Commit the findings document and the vectors:

```bash
git add docs/shamir-interop-findings.md testdata/shamir-vectors.json
git commit -m "docs: establish Shamir interoperability across suite implementations"
```

---

## Task 2: Create the `ky-primitives` module

> **Amended 2026-09-02:** the prefix here is correct and confirmed against GitHub —
> `gh api orgs/Busness-app --jq .login` returns `Busness-app`. Go module paths are
> case-sensitive, so every plan in this directory now uses this exact casing; a lowercase
> variant would be a different module to the toolchain. See
> [the module path migration plan](2026-09-02-module-path-migration.md), which brings all
> eight existing repos onto it — none of them followed the org move.

**Files:**
- Create: a new repository `github.com/Busness-app/ky-primitives` with `go.mod`, `shamir/`, `capsule/`, `auditchain/`, `testdata/`
- Depends on: Task 1's verdict and vectors

**Interfaces produced:**
- `shamir.Split(secret []byte, k, n int) ([]Share, error)`, `shamir.Combine(shares []Share, k int) ([]byte, error)`, `shamir.Share` with a documented, versioned encoding
- `capsule.Seal(...)`, `capsule.Open(...)` matching the existing on-disk `.kycap` format exactly
- `auditchain.Append(prev Record, e Event) Record`, `auditchain.Verify(records []Record) error`

- [ ] **Step 1: Initialise with an empty require block**

```
module github.com/Busness-app/ky-primitives

go 1.26
```

Add a test that fails if the module ever gains a dependency — this constraint is the whole reason the module exists and it should be enforced mechanically, not by discipline:

```go
func TestModuleHasNoDependencies(t *testing.T) {
	// This module is imported by kysignon-server, whose architecture rests on having
	// three direct dependencies. A require line here would make it unusable there.
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if strings.Contains(string(data), "require") {
		t.Fatalf("ky-primitives must have no dependencies; go.mod contains a require block:\n%s", data)
	}
}
```

- [ ] **Step 2: Port Shamir, vectors first**

Copy Task 1's canonical implementation. Before adapting anything, add a test that loads `testdata/shamir-vectors.json` and asserts the port reproduces every vector exactly. Run it, watch it pass, and only then improve naming or comments — with the vector test re-run after each change. The vectors are the contract; the code is replaceable.

Carry over the property tests from whichever repo has the better ones, and add the `k=n=255` and all-zero-secret cases if they are missing.

- [ ] **Step 3: Port the capsule format**

The `.kycap` container is AES-256-GCM with a SHA-256 integrity checksum (`kysignon-server/internal/backup/AGENTS.md`). Port it with a golden capsule fixture generated from the *current* `kysignon-server` implementation, so a capsule written before this plan opens afterwards. That fixture is the regression test for the global constraint "existing backups must still restore."

- [ ] **Step 4: Port the audit hash chain**

Take the chain construction from `kyrecovery-server` or `kysignon-server`, whichever verifier is more complete, and add fixtures: a valid chain that verifies, a chain with one mutated record that fails, and a chain with a removed record that fails. Both failure modes must be detected — a chain that only catches mutation is not tamper-evident against deletion.

- [ ] **Step 5: Tag v0.1.0 and publish**

```bash
git tag v0.1.0 && git push origin v0.1.0
```

---

## Task 3: Migrate `kysignon-server`

**Files:** `kysignon-server/internal/backup/shamir.go`, `capsule.go`, `internal/audit/audit.go`, `go.mod`

Do this repo first: it has the most demanding dependency constraint, the most valuable recovery kits, and the best test coverage. If the module cannot serve KySignOn it cannot serve anyone, and that is better learned here than after four migrations.

- [ ] **Step 1: Add the dependency and check the cost**

```bash
go get github.com/Busness-app/ky-primitives@v0.1.0
go mod tidy
```

`go.mod`'s direct requires must go from three to four, with **no new indirect entries**. If any appear, stop — the module violated its own constraint and Task 2 is not finished.

- [ ] **Step 2: Replace the internals, keep the package boundary**

`internal/backup` keeps its exported API. Its `shamir.go` and the capsule internals become thin delegations to `ky-primitives`. Nothing outside `internal/backup` should need to change; if a caller does, say so in your report, because it means the module's API does not actually match what the suite needs.

- [ ] **Step 3: Prove existing kits still work**

Run the full backup suite, then the restore drill (`go test -v ./internal/backup/...`), then verify a capsule generated *before* this change still opens. If no such capsule exists in `testdata`, generate one from the pre-migration commit first — this is the single most important check in the whole plan.

- [ ] **Step 4: Full gate and commit**

```bash
gofmt -l . && go vet ./... && go test -race -count=1 ./... && govulncheck ./...
git commit -m "refactor(backup): use shared ky-primitives for Shamir and capsule"
```

---

## Task 4: Migrate `ky_server_base` and `gridlock-server`

Same shape as Task 3, in either order. These two already share an implementation (md5 `a0d9905b`), so they migrate together or not at all.

`ky_server_base` has a second job here: because it is the scaffold every future product is copied from, migrating it is what stops the next product from being born with a ninth copy. Update its `IMPLEMENTATION_PLAN.md` and `prompt.md` to say that backup primitives come from `ky-primitives` and are not to be vendored.

- [ ] Add the dependency to each, delegate, run each repo's full gate, verify existing capsules still open, commit separately.

---

## Task 5: Give the pairing spec one home

**Files:** `zero_code_pairing_handoff_spec.md` in nine repos

- [ ] **Step 1: Confirm the direction of the drift**

```bash
diff kysignon-server/zero_code_pairing_handoff_spec.md kyrecovery-server/zero_code_pairing_handoff_spec.md
```

Every hunk must be an addition on the `kyrecovery-server` side. If any line differs rather than being added, the two have genuinely diverged and this task needs a human decision about which is correct — stop and report.

- [ ] **Step 2: Make `kyrecovery-server`'s copy authoritative**

It is the implementer of the server side, and its copy is the superset. Add a header naming it the single source of truth and giving its canonical URL.

- [ ] **Step 3: Replace the eight stale copies with a pointer**

In each of `kysignon-server`, `kypassword-server`, `kypost-server`, `kybookmarks-server`, `kynotes-server`, `kydns-server`, `gridlock-server`, `ky_server_base`, replace the file's body with a short stub naming the canonical location and the reason:

> This protocol is specified once, in `kyrecovery-server`, which implements its server
> side. This repository previously carried a copy that had fallen eight sections behind —
> it omitted the TTL ceiling, the rate limits, the size caps and the fact that the API
> token is returned exactly once. Read the canonical copy.

A pointer, not a synced duplicate: syncing is what produced eight stale copies.

- [ ] **Step 4: Surface the limits the clients never knew about**

For each client repo, check whether its implementation handles what the stale spec omitted — `429` responses with backoff, the 64 MiB body cap, the 4096-file limit, storing the API token on first receipt. Record the answer per repo in the commit message. **Do not fix them here**; each gap is its own change with its own tests. This step produces a list, and that list is the follow-up work.

---

## Task 6: Cut the scaffold back to the suite's dependency floor

**Files:** `ky_server_base/go.mod`, `internal/sso/*.go`, `internal/scim/*.go`, `internal/store/*.go`

**Why this belongs in this plan:** the scaffold is the template every future product is copied from, so its dependency tree is the *default* dependency tree of everything Busness.app ships next. Today that default is 7 direct requires pulling 18 indirect — 25 modules — against `kysignon-server`'s 3 direct and 8 indirect. Every new product inherits the larger number by default and has to fight its way back.

It also closes the gap named at the top of this plan: once the scaffold is stdlib-lean, it stops contradicting the architecture of the suite's own identity provider.

**Measured starting point:**

| Dependency | Files importing it | Indirect deps it drags in |
|---|---|---|
| `github.com/elimity-com/scim` | 2 | `q-uint/parser`, `q-uint/xsd-datetime`, `scim2/filter-parser/v2`, `golang.org/x/exp` |
| `github.com/coreos/go-oidc/v3` | 1 | `go-jose/go-jose/v4` |
| `github.com/jackc/pgx/v5` | 1 | `pgpassfile`, `pgservicefile`, `puddle/v2`, `x/sync`, `x/text` |
| `golang.org/x/oauth2` | 2 | — |
| `github.com/google/uuid` | 2 | — |
| `golang.org/x/crypto` | 1 | — |
| `modernc.org/sqlite` | 3 | its own `libc`/`mathutil`/`memory` tree |

**The replacements already exist inside the suite, and are already in production.** This task is not "reimplement OIDC" — it is "adopt the implementation the suite already ships and trusts":

- **OIDC relying party** (discovery, PKCE, code exchange, claim parsing): `kypassword-server/internal/sso/sso.go`, ~280 lines, standard library only. That is exactly what `go-oidc` and `x/oauth2` are doing here.
- **ID token verification against JWKS, RS256**: `kysignon-server/internal/oauth/oauth.go` and `internal/crypto`, standard library only.
- **SCIM 2.0 User resource shaping**: `kysignon-server/internal/sync/sync.go` builds and parses SCIM User resources with `encoding/json` alone.
- **SAML**: already hand-rolled here — `internal/sso/saml.go` exists with no SAML dependency in `go.mod`, which is the proof that this direction works.

**Target: three direct dependencies, matching `kysignon-server` exactly** — `golang.org/x/crypto` (Argon2id, genuinely not in stdlib), `modernc.org/sqlite` (pure-Go SQLite, keeps `CGO_ENABLED=0`), and `github.com/google/uuid`. The `uuid` one is replaceable with `crypto/rand` plus hex in a few lines, but keeping it matches the identity provider and is not worth a separate argument.

Postgres is deferred, so `pgx` goes with it — see Step 3.

- [ ] **Step 1: Replace `go-oidc` and `x/oauth2` with the in-suite RP implementation**

Port `kypassword-server/internal/sso/sso.go`'s discovery, PKCE, `ExchangeCode` and `ParseClaims` into `internal/sso`. Keep this package's existing exported API so `internal/api/sso_handlers.go` does not change.

The one thing to port carefully rather than copy: `go-oidc` verifies the ID token signature against the provider's JWKS, and a hand-rolled RP that skips that check is a critical vulnerability, not a simplification. Take the JWKS fetch, key selection and RS256 verification from `kysignon-server/internal/crypto`, and write a test that a token signed by the wrong key is rejected. **If you cannot make that test pass, stop and report** — shipping an RP that accepts unverified tokens is far worse than carrying `go-oidc`.

- [ ] **Step 2: Replace `elimity-com/scim` with stdlib handlers**

`internal/scim` currently delegates to the library for `/scim/v2/Users` and `/scim/v2/Groups`. The resource marshalling is `encoding/json` against the structs in `kysignon-server/internal/sync/sync.go` (`SCIMUserResource`, `SCIMName`, `SCIMEmail`, `SCIMRole`, `SCIMMeta`), and the routing is `net/http` patterns, which this repo already uses everywhere else.

The library's genuine contribution is SCIM **filter parsing** (`filter-parser`) — `filter=userName eq "alice"`. Do not reimplement a filter grammar. Support the single equality form the suite actually uses, and return `501 Not Implemented` with a clear message for anything else. A documented 501 is honest; a half-parsed filter that silently matches the wrong users is not.

- [ ] **Step 3: Remove Postgres and `pgx`**

**Decided: Postgres is deferred.** `prompt.md` requires "swapping of DBs to a heavier DB on demand" and `pgx` is imported by exactly one file, but no product in the suite has asked for Postgres and every shipped one runs SQLite. Carrying a driver plus 5 indirect dependencies for a capability nothing uses is exactly the weight this task exists to remove.

Delete the Postgres backend and the `pgx` require. **Keep the `internal/store` interface and its factory intact** — the abstraction is the thing that makes this reversible, and it costs nothing to keep. Re-adding a driver behind an interface that already exists is a contained change on the day a product actually needs it; that day is not today.

Concretely:
- Remove the Postgres branch from `internal/store/factory.go` and whatever dialect handling in `sqlstore.go` exists only for it.
- Leave the dialect seam in place. If the SQL is currently written to be dialect-portable, do not go through and "simplify" it to SQLite-only — that is the change that makes re-adding Postgres expensive later, and it buys nothing now.
- A configuration naming Postgres should fail at startup with a message saying it was deferred and where to look, not fall through to SQLite silently. A server that quietly used a different database than the operator configured is a data-loss shape.

Record the deferral in `IMPLEMENTATION_PLAN.md` directly beside requirement 6, so the next reader sees the requirement and its status together rather than discovering the gap in the code.

- [ ] **Step 4: Prove the tree actually shrank**

```bash
go mod tidy
go list -m all | wc -l
go build ./... && go vet ./... && go test -race -count=1 ./...
```

Report the module count before and after, and the full `go.mod` after. The direct requires should be exactly three — `x/crypto`, `modernc.org/sqlite`, `google/uuid` — and the indirect list should lose the `q-uint`, `scim2`, `go-jose`, `jackc` and `x/sync`/`x/text` entries entirely. Expect roughly 25 modules down to around 8, which is `kysignon-server`'s number.

Behaviour must be unchanged: the existing `internal/api` and `internal/sso` tests are the contract, and they should pass without modification. **A test you had to edit to make pass is a behaviour change** — call it out explicitly in your report rather than letting it ride.

- [ ] **Step 5: Make the constraint stick**

A scaffold's dependency tree regrows unless something holds it down. Add a test in the same shape as `ky-primitives`':

```go
func TestDependencyFloor(t *testing.T) {
	// This is the template every Busness.app product is copied from, so its dependency
	// tree becomes theirs by default. Adding one here adds it to every future product.
	// If you are adding a dependency deliberately, add it to this list and say why.
	allowed := map[string]bool{
		"golang.org/x/crypto":  true, // Argon2id, not in stdlib
		"modernc.org/sqlite":   true, // pure-Go SQLite, keeps CGO_ENABLED=0
		"github.com/google/uuid": true,
	}
	// Parse go.mod's direct requires and fail on anything not in `allowed`.
}
```

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./... && go test -race -count=1 ./...
git add go.mod go.sum internal cmd
git commit -m "refactor: cut the scaffold to the suite's dependency floor"
```

---

## Task 7: Documentation

- [ ] Add a `## Shared primitives` section to each migrated repo's `AGENTS.md`: which primitives come from `ky-primitives`, that the module is dependency-free by contract, and that a local reimplementation of Shamir or the capsule format is a defect rather than an optimisation.
- [ ] In `ky-primitives`' own README, state its one rule — no dependencies, ever — and that its golden vectors are the compatibility contract, so a change that alters a vector is a breaking change requiring a major version.
- [ ] Record in `ky_server_base`'s `IMPLEMENTATION_PLAN.md` that new products consume `ky-primitives` rather than copying `internal/backup`.
- [ ] Record the Postgres deferral beside requirement 6 in `IMPLEMENTATION_PLAN.md` and in `prompt.md`'s "swapping of DBs" line: the store interface still abstracts the backend, no driver ships, and adding one back is a contained change when a product needs it. A requirement quietly unimplemented is worse than one marked deferred — the next reader must not have to discover it from the absence of code.

---

## What this plan deliberately does not do

- **It does not consolidate device pairing.** Four implementations exist, but pairing is entangled with each product's UX and session model, and the divergence there costs maintenance rather than correctness. Revisit only after the module has proven itself.
- **It does not touch `internal/crypto` or the non-chain parts of `internal/audit`.** Wrapping `crypto/aes` differently in five repos is duplication without a failure mode.
- **It does not make `ky_server_base` a library.** It stays a scaffold. That is what it is for.
- **It does not merge any products.** Same reasoning as the KySignOn/KyPassword analysis: shared primitives are not shared processes.

## Sequencing note

Task 1 gates everything. If the two Shamir implementations do not interoperate, this stops being a refactoring plan and becomes an incident — recovery kits already in custodians' hands may not reconstruct against the code that will try to read them. Do not let a subagent skip Task 1 because the refactor looks obvious.
