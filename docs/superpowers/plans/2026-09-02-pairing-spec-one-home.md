# Pairing Spec: One Home Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Leave exactly one copy of the zero-code pairing spec — the implementer's — and replace the eight stale copies with pointers, then list what the three real clients are missing because they were written against the stale text.

**Architecture:** `kyrecovery-server` implements the server side of this protocol, so its copy is authoritative by construction. The other copies become short stubs naming it. A pointer, not a synced duplicate: syncing is what produced eight stale copies.

**Tech Stack:** Markdown. No code changes in this plan.

**Spec:** [The suite shared primitives plan](2026-09-02-suite-shared-primitives.md), Finding B and Task 5.

## Global Constraints

- **One authoritative copy, and it is the implementer's.** No repo gets a synced duplicate.
- No client behaviour changes here. This plan produces a gap list; fixing each gap is its own change with its own tests.

---

## Findings that set the scope

Measured 2026-09-02.

### Nine stale copies, not eight

`zero_code_pairing_handoff_spec.md` is byte-identical, md5 `24899bae8d11ac740c58dcc5c3581e32`, in:

```
gridlock-server/     kybookmarks-server/  kydns-server/
kynotes-server/      kypassword-server/   kypost-server/
ky_server_base/      kysignon-server/
```

plus a **ninth copy at the workspace root**, `/home/yoshi/busness.app/zero_code_pairing_handoff_spec.md`, which the parent plan does not mention. That path is not inside any git repository — `git rev-parse` there fails — so it is an untracked loose file that no repo's history will ever record a change to. It is the copy most likely to be read by accident and least likely to be maintained.

`kyrecovery-server`'s copy is md5 `460f9957…`.

### The drift is not purely additive, and the parent plan's gate would stop here

Task 5 Step 1 says: *"Every hunk must be an addition on the `kyrecovery-server` side. If any line differs rather than being added, the two have genuinely diverged and this task needs a human decision about which is correct — stop and report."*

**Ten lines differ rather than being added.** They fall into two groups.

**Group 1, elaborations.** Same claim, more detail on the `kyrecovery-server` side — the `expected_env` and `expected_ports` schema rows, and the single-use-PIN invariant which gains the concurrency and rate-limit detail. No conflict.

**Group 2, the drill check names, which are client-visible.** The stale copies document one set of names in the drill response; `kyrecovery-server`'s copy documents another:

| Stale copies | `kyrecovery-server` copy |
|---|---|
| `"Directory Unpack"` | *(absent)* |
| `"Required Files"` | `"required_file:data/notes.db"` |
| `"SQLite Integrity: data/notes.db"` | `"sqlite_check:data/notes.db"` |
| `"Environment Variables"` | `"expected_env"` |
| `"Network Ports"` | `"expected_ports"` |
| *(absent)* | `"dependencies"` |

**This resolves without a human decision, because the server settles it.** The names the code actually emits are `required_file`, `sqlite_check`, `expected_env`, `expected_ports` and `dependencies`:

```bash
grep -rhno '"\(required_file\|sqlite_check\|expected_env\|expected_ports\|dependencies\)"' \
  kyrecovery-server --include='*.go'
```

The stale copies' human-readable names appear nowhere in the server as check names — the only matches for "Required Files" and "SQLite Integrity" are headings in generated recovery-kit documents. **Eight repos document a response shape the server does not produce.** A client parsing check names from the stale spec looks for `"SQLite Integrity: data/notes.db"` and finds `"sqlite_check:data/notes.db"`.

That is worse than a stale document. It is a documented API that never existed.

### Only three repos are clients; the other six carry the file for no reason

The parent plan's Task 5 Step 4 says to check *"each client repo"* against the omitted limits. Most of the repos carrying the spec are not clients:

| Repo | Talks to KyRecovery? |
|---|---|
| `ky_server_base` | yes — `internal/backup/client.go` |
| `gridlock-server` | yes — `internal/backup/client.go` |
| `kysignon-server` | yes — `internal/backup/recovery_token.go` |
| `kypassword-server`, `kybookmarks-server`, `kynotes-server`, `kydns-server`, `kypost-server` | no |

So the gap audit covers three repos, not eight, and the other five just get the stub.

---

## Task 1: Make kyrecovery-server's copy authoritative

**Files:**
- Modify: `kyrecovery-server/zero_code_pairing_handoff_spec.md`

- [ ] **Step 1: Add a header naming it the single source of truth**

At the very top of the file, above the existing title:

```markdown
> **This is the single authoritative copy of this specification.**
>
> It lives in `kyrecovery-server` because this repository implements the server
> side of the protocol, so a claim made here can be checked against the code
> beside it. Eight other repositories carried a copy that fell behind: they
> omitted the TTL ceiling, the rate limits, the size caps and the fact that the
> API token is returned exactly once — and they documented drill check names the
> server does not emit.
>
> Do not copy this file. Link to it.
```

- [ ] **Step 2: Commit**

```bash
cd /home/yoshi/busness.app/kyrecovery-server
git add zero_code_pairing_handoff_spec.md
git commit -m "docs: name this the authoritative pairing spec"
```

---

## Task 2: Replace the eight stale copies with pointers

**Files:**
- Modify: `zero_code_pairing_handoff_spec.md` in `gridlock-server`, `kybookmarks-server`, `kydns-server`, `kynotes-server`, `kypassword-server`, `kypost-server`, `ky_server_base`, `kysignon-server`

Each repo gets its own commit — they are separate repositories — but all eight get the identical stub.

- [ ] **Step 1: Write the stub once**

```bash
cat > /tmp/pairing-stub.md <<'STUB'
# Zero-Code Product Pairing & Self-Declaring Backup Ingest

This protocol is specified once, in `kyrecovery-server`, which implements its
server side.

**Canonical copy:** `kyrecovery-server/zero_code_pairing_handoff_spec.md`

This repository previously carried its own copy. That copy had fallen behind and
was wrong in ways that mattered to anyone writing a client against it:

- it omitted the `ttl_minutes` ceiling of 60 (default 15)
- it omitted the `429` rate limits — 10 claim attempts per source address and 5
  per code per 15 minutes, 60 pushes per product per 15 minutes, 4 concurrent
  ingests
- it omitted the 64 MiB body cap (`KYRECOVERY_MAX_BACKUP_BYTES`), the 4096-file
  limit and the 32 MiB per-file limit
- it omitted that `total_shares` may not exceed 255, the GF(2^8) ceiling
- it omitted that path escapes (`../`, absolute paths) fail the drill rather
  than being evaluated
- it omitted that **the API token is returned exactly once and cannot be read
  back** — a client that does not persist it on first receipt has lost it
- it documented drill check names the server does not emit: `"Required Files"`
  and `"SQLite Integrity: <path>"` rather than the actual `required_file:<path>`
  and `sqlite_check:<path>`

A pointer, not a synced duplicate. Syncing is what produced eight stale copies.
STUB
```

- [ ] **Step 2: Install it in all eight and commit each**

```bash
cd /home/yoshi/busness.app
for r in gridlock-server kybookmarks-server kydns-server kynotes-server \
         kypassword-server kypost-server ky_server_base kysignon-server; do
  cp /tmp/pairing-stub.md "$r/zero_code_pairing_handoff_spec.md"
  git -C "$r" add zero_code_pairing_handoff_spec.md
  git -C "$r" commit -m "docs: point at the authoritative pairing spec

This copy had fallen behind on the TTL ceiling, the rate limits, the size
caps and the single-issue API token, and it documented drill check names
the server does not emit."
done
```

- [ ] **Step 3: Verify all eight are now identical and none was missed**

```bash
cd /home/yoshi/busness.app
md5sum */zero_code_pairing_handoff_spec.md
```

Expected: eight identical hashes, plus `kyrecovery-server`'s distinct one. Any repo still showing `24899bae8d11ac740c58dcc5c3581e32` was missed.

- [ ] **Step 4: Deal with the ninth copy at the workspace root**

```bash
ls -l /home/yoshi/busness.app/zero_code_pairing_handoff_spec.md
```

This one is not in any repository, so no commit can record its fate.

**Delete it rather than stubbing it.** A loose untracked file next to nine repos has no owner, no history and no review — it is the copy that will go stale again first, and a stub there is a file nobody will ever be asked to update. The eight in-repo stubs already point anyone who goes looking to the right place.

**Confirm with the human before deleting.** It is outside every repository, so it cannot be recovered from git if it turns out something depended on it.

---

## Task 3: List what the three clients are missing

**Files:**
- Create: `ky_server_base/docs/kyrecovery-client-gaps.md`

**This task produces a list, not fixes.** Each gap is its own change with its own tests. Recording them together is what makes them visible; fixing them together is what makes them unreviewable.

- [ ] **Step 1: Audit each client against the four omitted constraints**

For `ky_server_base`, `gridlock-server` and `kysignon-server`, answer four questions with a file and line reference each:

1. **Does it handle `429` with backoff?** Look for `http.StatusTooManyRequests` and `Retry-After`.
2. **Does it check the 64 MiB body cap before pushing?**
3. **Does it check the 4096-file limit before pushing?**
4. **Does it persist the API token on first receipt?**

- [ ] **Step 2: Record what is already known**

Two answers are established and can go straight in:

**`ky_server_base/internal/backup/client.go` does not handle `429`.** `PushBackup` collapses every non-200 into one generic error:

```go
if resp.StatusCode != http.StatusOK {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, fmt.Errorf("backup push rejected (%d): %s", resp.StatusCode, string(b))
}
```

A rate-limited push is indistinguishable from a malformed one, there is no `Retry-After` parse and no backoff. `gridlock-server/internal/backup/client.go` has the same `resp.StatusCode != http.StatusOK` shape at lines 53 and 122 — it was copied from the same source.

**`kysignon-server` does persist the token.** `internal/backup/recovery_token.go` stores it encrypted under the `kyrecovery_token_enc` setting, with a `legacyRecoveryTokenSetting` migration path. That is the one client that got the single-issue-token constraint right, despite the spec it had.

- [ ] **Step 3: Write the gap document**

A table — repo down the side, the four constraints across, each cell either a file:line reference or `MISSING`. Then one paragraph per `MISSING` cell saying what breaks in practice. Close with the follow-up list, one line per gap, so each can become its own change.

- [ ] **Step 4: Commit**

```bash
cd /home/yoshi/busness.app/ky_server_base
git add docs/kyrecovery-client-gaps.md
git commit -m "docs: list what the KyRecovery clients missed from the stale spec"
```

---

## What this plan deliberately does not do

- **It does not fix any client.** Task 3 produces the list; each gap is a separate change. Bundling four behaviour fixes across three repos into a documentation plan is how documentation plans stop landing.
- **It does not sync the spec anywhere.** Eight copies drifted because copying felt cheaper than linking.
- **It does not change the protocol.** Every constraint named here is one the server already enforces and eight documents already omitted.
