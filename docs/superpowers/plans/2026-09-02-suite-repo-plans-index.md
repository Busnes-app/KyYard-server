# Suite Repo Plans: Index and Sequencing

The [suite shared primitives plan](2026-09-02-suite-shared-primitives.md) covers nine
repositories in one document. This index splits the work that remains into plans that
can each be executed, reviewed and reverted on their own.

`ky_server_base`'s own work — Task 6, cutting the scaffold to three direct dependencies —
stays in the parent plan. Everything here is the other repos.

## The plans

| Plan | Repos | Blocked by |
|---|---|---|
| [Module path migration](2026-09-02-module-path-migration.md) — **DONE 2026-09-02** | all eight Go repos | — |
| [Capsule format interop](2026-09-02-capsule-format-interop.md) | shared gate | `ky-primitives` exists |
| [kysignon-server migration](2026-09-02-kysignon-server-migration.md) | `kysignon-server` | capsule gate, for its Task 3 only |
| ~~[gridlock-server migration](2026-09-02-gridlock-server-migration.md)~~ — **superseded 2026-09-03** by the two plans below | `gridlock-server` | — |
| [Scaffold adopts ky-primitives v0.4.0](2026-09-03-scaffold-adopts-ky-primitives.md) — **DONE 2026-09-03** (PR #5) | `ky_server_base` | — |
| [gridlock re-fork](2026-09-03-gridlock-refork.md) — **DONE 2026-09-03** (gridlock PR #5) | `gridlock-server` | — |
| [kyrecovery blind store](2026-09-04-kyrecovery-blind-store.md) | `kyrecovery-server` | ky-primitives v0.4.1 tagged (PR #11) |
| [Audit chain convergence](2026-09-02-audit-chain-convergence.md) | `kyrecovery-server`, `kypassword-server`, `kybookmarks-server` | `ky-primitives` exists |
| [Pairing spec, one home](2026-09-02-pairing-spec-one-home.md) | all nine, plus a loose copy | nothing |

## What changed on 2026-09-03

The library shipped `v0.4.0`: capsules are `kycap/3`, sealed to one suite-wide recovery
public key with HPKE (X-Wing), and `Seal`/`Open` take `recoverykey` types instead of a raw
key. That decision lives in `ky-primitives/docs/superpowers/specs/2026-09-03-recovery-keypair-design.md`
and its consequences for products in the suite migration design's Phase 3. Two things here
stopped being true:

- The capsule-format-interop plan's premise — a per-capsule symmetric key returned by
  `Seal`, dispatch across two containers — is retired. Its `kycap/1` reader and the tar
  reader are gone from the library; nothing had been persisted in either by this scaffold or
  gridlock, so nothing is orphaned. The plan stays as history.
- The gridlock migration plan migrated gridlock independently. Phase 3 says the scaffold
  goes first and gridlock is **re-forked** from it. The two 2026-09-03 plans above replace it;
  gridlock's compat job is red on purpose until the re-fork lands.

## Why the audit work is one plan and not three

Every other plan here is per-repo. The audit chain is not, because its entire purpose is
that three products end up chaining records the same way. Three separate plans would let
three executors pick three targets, which is how there came to be four implementations.
Inside it, each repo's tasks are still independently executable and revertible.

## Sequencing

**~~First, and blocking almost everything:~~ Done.** The module path migration ran on
2026-09-02: all eight repos renamed onto `github.com/Busness-app/`, each on a
`refactor/module-path` branch, none pushed or merged. See that plan's Outcome section for
the five things it turned up that were not in the plan.

**Next, no dependencies:** the pairing spec plan. It is documentation only and touches no
module path.

**Then, once `ky-primitives` exists** (parent plan Task 2): the capsule gate, and the
Shamir halves of the two migration plans, which can run in parallel — they are separate
repositories with no shared state.

**Last:** the capsule halves of both migrations, gated on the shared package opening all
three products' capsules.

## What measurement changed

Three of these plans exist because the parent plan's assumptions did not survive being
checked. The corrections, with what proved each:

- **The capsule gate fails where the Shamir gate passed.** `ky_server_base` and
  `gridlock-server` write ciphertext in `base64.RawURLEncoding`; `kysignon-server` writes
  `base64.StdEncoding`. Cross-decoding fails in both directions with `illegal base64
  data`. The parent plan's instruction to port the capsule from `kysignon-server` alone
  would have left two products unable to open their own backups.
- **`gridlock-server` and `ky_server_base` do not "already share an implementation."**
  True of `shamir.go`, false of `capsule.go` — 49 lines apart, and three distinct md5s
  across the three repos.
- **`kysignon-server` has no audit hash chain**, so the parent plan's choice of donor
  "`kyrecovery-server` or `kysignon-server`" has one candidate.
- **`kypassword-server`'s audit chain is unkeyed** and can therefore be forged by anyone
  who can write the audit file. This is the only genuine security defect the split turned
  up.
- **The pairing spec drift is not purely additive**, which trips the parent plan's
  escalation gate — but the server's own code resolves it without a human decision.
- **No `go.mod` followed the move to `github.com/Busness-app/`.** Four conventions are in
  use, including one repo claiming another's identity. See below.

## The module path question is now answered

The suite has moved to `github.com/Busness-app/`. The remotes went; the module paths did
not:

```
go.mod says                                    origin says
github.com/Yoshiofthewire/ky_server_base       Busness-app/ky_server_base
github.com/Yoshiofthewire/ky_server_base       (no origin)          <- gridlock-server
github.com/Yoshiofthewire/kysignon-server      Busness-app/kysignon-server
github.com/yoshiofthewire/kydns-server         Busness-app/kydns-server
github.com/yoshiofthewire/kynotes-server       Busness-app/kynotes-server
kybookmarks-server                             Busness-app/kybookmarks-server
kypassword-server                              Busness-app/kypassword-server
kyrecovery-server                              Busness-app/kyrecovery-server
```

[The module path migration plan](2026-09-02-module-path-migration.md) fixes all eight.

**The casing is settled: `Busness-app`, capital B, matching the GitHub org.** Taken from
the API rather than a remote URL, because remote URLs are case-insensitive and prove
nothing:

```bash
gh api orgs/Busness-app --jq .login          # -> Busness-app
gh api orgs/Busness-app/repos --jq '.[].name'
```

Go module paths are case-sensitive — an uppercase letter is escaped as `!b` in the module
cache and on the proxy — so this has to be exact everywhere. It is normal and works fine;
it just cannot be half-applied, which is the state the suite is in today with
`yoshiofthewire` and `Yoshiofthewire` both live.

**`gridlock-server` is not in the org.** The repo list above returns every other repo in
the suite and no `gridlock-server`; that repo has no `origin` and exists only on this
machine. Its module should still declare `github.com/Busness-app/gridlock-server` — a main
module's own path is never resolved over the network, so nothing breaks locally — but
nothing can import it until the repository exists. Creating it is a decision for a human,
raised in the migration plan rather than acted on.
