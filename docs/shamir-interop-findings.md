# Shamir interoperability across the suite

**Task 1 of the [suite shared primitives plan](superpowers/plans/2026-09-02-suite-shared-primitives.md).**
That plan gates every later task on this question, because if the suite's two Shamir
implementations do not agree, recovery kits already in custodians' hands may not
reconstruct and the plan becomes an incident rather than a refactor.

**Verdict: they interoperate. Every case round-trips in both directions, byte for byte.
The gate passes and the plan proceeds.** No issued recovery kit is at risk from the
divergence.

Date: 2026-09-02. Go 1.27.0, linux/amd64.

## What was compared

| File | md5 | Lines |
|---|---|---|
| `kysignon-server/internal/backup/shamir.go` | `dc8a6da39db3a08f8308586b95563bee` | 132 |
| `gridlock-server/internal/backup/shamir.go` | `a0d9905b4080774085ff5f71d304de15` | 130 |
| `ky_server_base/internal/backup/shamir.go`  | `a0d9905b4080774085ff5f71d304de15` | 130 |

Two distinct files, not three: `gridlock-server` and `ky_server_base` are byte-identical,
so testing either covers both. The comparison is therefore KySignOn's copy against the
scaffold's.

## Round-trip results

Split with one implementation, reconstruct with the other, in both directions, asserting
the recovered secret equals the original byte for byte. A recovered secret of the right
length but wrong content is the dangerous outcome and is distinguished from an error
return.

| Case | kysignon → base | base → kysignon |
|---|---|---|
| 32-byte AES-256 key, `k=2 n=3` | pass | pass |
| `k=3 n=5` | pass | pass |
| `k=1 n=1` (degenerate) | rejected by both | rejected by both |
| `k=n=255` (the GF(2^8) ceiling) | pass | pass |
| Secret containing `0x00` bytes | pass | pass |
| All-zero secret | pass | pass |
| 1-byte secret | pass | pass |
| 4 KiB secret | pass | pass |
| Non-prefix subset `{3,5,2}` of `n=5` | pass | pass |

The last row is not in the plan's list and was added because it is the real case: a
restore hands over an arbitrary quorum, not the first `k` shares.

`k=1, n=1` is rejected by *both* with the identical error, `threshold must be <= total
shares and > 1`. The degenerate case is not supported anywhere in the suite. That is
agreement, not divergence, so it does not block the gate — but note that "1 of 1" is not
an option a product can offer.

Shares are freshly randomised per call, so the whole matrix was run **200 times under
`-race`** (9 cases × 4 direction pairs × 200 = 7,200 round trips, 124s, zero failures)
rather than once.

## Share encoding

**Identical, by construction.** The diff between the two files is confined entirely to
`CombineShares`; `SplitSecret`, the `Share` type, its JSON tags and the GF(2^8) tables
are byte-identical. Both serialise as `{"index":7,"data":"3q2+7w=="}` — same field names,
same order, same base64 `[]byte` handling — and both number shares `1..n`.

A custodian can carry a share written by one product into another's restore flow.

## Why they agree

The 32-line difference is a rename with added comments, not a second design:

- `i`/`j` → `j`/`m`, `xi`/`xj` → `xj`/`xm`, `yi` → `yj`, `dataLen` → `secretLen`
- `li := gfDiv(num, den); secretByte ^= gfMul(yi, li)` became
  `lagrange := gfMul(yj, gfDiv(num, den)); secretByte ^= lagrange`

KySignOn's copy is the more legible one: it states the Lagrange identity in a comment and
explains why `0 - x` and `x_j - x_m` are both XOR in GF(2^8).

This was established by reading the diff and then **proved** by the matrix above.
Inspection alone would not have been sufficient — the point of the exercise was that two
functions with identical signatures can silently disagree.

## Two defects found, identical in both implementations

Neither affects interoperability, so neither blocks the plan. Both are follow-up work.

**1. A duplicate share index panics instead of erroring.**

Supplying the same share twice makes the Lagrange denominator zero, and `gfDiv` panics
with `divide by zero in GF(256)`. Confirmed in both implementations.

The only production caller is `kysignon-server/cmd/kyrestore`, whose interface is a
repeated `-shard INDEX:HEX` flag. A custodian who pastes the same shard twice — during a
disaster, under pressure — gets a Go panic and a stack trace instead of "share 3 was
supplied twice". `CombineShares` should reject duplicate indices with an error.

**2. A corrupted share reconstructs a wrong key silently.**

Flipping one bit in one share returns a different 32-byte secret with `err == nil`. This
is inherent to Shamir without share authentication, and it is *mitigated downstream*:
`ExtractCapsule` verifies the payload's SHA-256 before writing anything, so a wrong
quorum fails cleanly rather than producing a plausible-looking restore. Worth stating
explicitly because that checksum is the only thing standing between a typo and a wrong
key, and any future caller of `CombineShares` that skips it inherits the hazard.

## Corrections to the plan

**The module paths do not match the org the code lives in.**

> **Superseded, 2026-09-02.** This section originally said the parent plan's
> `github.com/Busness-app/<name>` convention "is wrong" and that the real prefix is
> `github.com/Yoshiofthewire/`. **That was wrong.** It described `go.mod` accurately, but
> `go.mod` was the stale artefact: the suite has moved to the `Busness-app` organisation
> and the module paths never followed. The parent plan named the destination, not the
> current state. The canonical prefix is `github.com/Busness-app/`, matching the org — see
> [the module path migration plan](superpowers/plans/2026-09-02-module-path-migration.md).
> The measurements below stand; only the conclusion drawn from them was wrong.

Every module in the suite declares a path from before the move:

```
github.com/Yoshiofthewire/ky_server_base       ky_server_base
github.com/Yoshiofthewire/kysignon-server      kysignon-server
github.com/yoshiofthewire/kydns-server         kydns-server
github.com/yoshiofthewire/kynotes-server       kynotes-server
kyrecovery-server                              kyrecovery-server
kypassword-server                              kypassword-server
kybookmarks-server                             kybookmarks-server
```

Four conventions — the old org capitalised, the old org lowercased, and three with no
domain at all. A bare path still consumes dependencies fine, which is why it went
unnoticed, but it cannot be imported by anything.

Task 2 creates a new module that five repos will import, so its path has to be settled
before it is tagged.

**`gridlock-server` still declares the scaffold's module path.** Its `go.mod` reads
`module github.com/Yoshiofthewire/ky_server_base`, and every internal import inside it
resolves under that same prefix — it is a copy of the scaffold whose module was never
renamed. Two repositories currently claim one module identity, and it is the only repo in
the suite with no `origin` remote.

This lands directly on Task 4, which migrates both together: adding `ky-primitives` to
each is fine, but `gridlock-server` needs its module renamed first, and that rename
touches every import in the repo. Size it as its own change.

## Reproducing this

The harness was throwaway, as the plan specifies, and is committed nowhere. It is two
verbatim copies of the implementations plus a table-driven cross-check:

```bash
mkdir -p /tmp/shamir-interop/implkysignon /tmp/shamir-interop/implbase
sed '1s/^package backup$/package implkysignon/' \
  kysignon-server/internal/backup/shamir.go > /tmp/shamir-interop/implkysignon/shamir.go
sed '1s/^package backup$/package implbase/' \
  ky_server_base/internal/backup/shamir.go > /tmp/shamir-interop/implbase/shamir.go
```

Each copy differs from its source by exactly that one package line — verified with
`diff`, because a whitespace "cleanup" that changed behaviour would have invalidated the
whole result. The cross-check then splits with each package and combines with the other.

**The durable guard is not the harness.** It is `testdata/shamir-vectors.json` and
`internal/backup/shamir_vectors_test.go`, which run in this repo's normal test suite:

```bash
go test -race -count=1 -run TestShamirGoldenVectors ./internal/backup/...
```

## The golden vectors

`testdata/shamir-vectors.json` holds eight frozen cases. Each was generated from
KySignOn's implementation and verified to reconstruct under the scaffold's before being
written.

**Deviation from the plan, deliberate:** the plan asked for "a fixed seed" per vector.
There is no seed to fix. `SplitSecret` reads `crypto/rand` directly and is not
injectable, so split output is unreproducible by construction and no vector file can
pin it.

The vectors therefore fix the **combine** direction: any implementation claiming
compatibility must reconstruct `secret_hex` from these exact shares. That is the
property that actually matters — it is what a recovery kit does — and it is the stronger
test, since it validates against shares produced by code as it stood today rather than
against a re-derivation. A change that breaks a vector is a breaking change to every kit
already issued.
