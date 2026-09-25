# Recovery and Operator Acceptance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a restored KyYard truthful and complete: in-flight commands from the capsule moment settle as `unknown` at boot, a capsule pins its schema version and the drill checks it, every control-plane table added since the base is proven to survive restore, the operator documents say what a capsule holds, and the section 7 acceptance script exists as a runbook.

**Architecture:** A store method `ReconcileAfterStart` (Task 1) is called from `cmd/server` after tenancy initialisation and before anything serves (Task 1). The capsule recipe gains `schema_version` from a new `migrations.Latest()` and the drill checks it against `schema_migrations` (Task 2). A restore-verification test builds one of everything and round-trips it (Task 3). Docs and the acceptance runbook close the slice (Task 4).

**Tech Stack:** Go (`database/sql`, SQLite + PostgreSQL), `ky-primitives/recoveryclient` and `capsule` (unchanged), Markdown.

**Spec:** `docs/superpowers/specs/2026-09-24-recovery-acceptance-design.md`

## Global Constraints

- Reconcile detail text exactly `the server restarted before a result arrived`; outcome `unknown`; every `endpoint_commands` row with `outcome=''` regardless of `dispatched_at`; one audit row per affected endpoint with `action='endpoint.commands.reconciled'`, `user_id='system'`, `scope='organization'` with the command's organization and environment (so tenant audit views list it, like `auditDeployment`), `resource=<endpoint id>`, `details='commands=<n>'`, `result='unknown'`; idempotent; failure at startup is fatal.
- `applying` deployments are untouched by reconcile.
- Recipe key `schema_version` (int) = `migrations.Latest()`; drill check name `Schema Version: data/ky_server.db`; failure message `database schema is version N, capsule expects M`; a recipe without the key fails with `schema_version must be the latest migration number`.
- Every new store behaviour tested on SQLite and PostgreSQL; the restore-verification and drill tests are SQLite-only like the existing ones.
- Docs must contain no stale statement about `instance.key` ("no agents use this key yet" is false: it signs the endpoint fingerprint and the local Docker binding).
- Commit trailer: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. A restored database whose `endpoint_commands` row has `outcome=''` but `settled_at` already set (a partially written row from a crash) must still be settled as unknown, not skipped (Task 1 test).
2. A reconcile that runs while an agent session is already open must not race a real result: the first answer wins because `SettleCommand` only updates `outcome=''` rows; reconcile must use the same guard (Task 1 test: reconcile after a settle changes nothing).
3. A capsule taken on a newer schema than the binary reading it (downgrade) must fail the drill with the same message, not pass because `MAX(version)` is higher (Task 2 test with version M+1).
4. A database with no `schema_migrations` table at all (foreign file) must fail the schema check with a message, not panic (Task 2 test).
5. A second `ReconcileAfterStart` on the restored database must write no audit row (Task 3 asserts the audit count is unchanged).

---

### Task 1: `ReconcileAfterStart` and the boot hook

**Files:**
- Create: `internal/store/reconcile.go`
- Modify: `internal/store/store.go` (TenancyStore interface: `ReconcileAfterStart(ctx context.Context) (int64, error)`)
- Modify: `cmd/server/main.go:127-131` (call after `Initialize`, before `api.NewServer`)
- Test: `internal/store/reconcile_test.go`, `cmd/server/main_test.go` (or the existing server test file) for the ordering
- Modify: `internal/store/AGENTS.md`, root `AGENTS.md` (the `cmd/server` startup paragraph)

**Interfaces (produces):**
```go
// ReconcileAfterStart settles every command still in flight as unknown: the process that
// dispatched it is gone, so no answer can arrive. Returns the number of commands settled.
func (t *tenancyStore) ReconcileAfterStart(ctx context.Context) (int64, error)
const reconcileDetail = "the server restarted before a result arrived"
```

- [ ] **Step 1: Failing tests** (`reconcile_test.go`, both drivers via the testdb pattern used in `commands_test.go`; reuse its endpoint/command fixture):
```go
func TestReconcileAfterStartSettlesInFlightCommands(t *testing.T) {
	// fixture: endpoint E1 with a dispatched command (MarkCommandDispatched) and an undispatched one;
	// endpoint E2 with a command already settled (SettleCommand ... succeeded).
	n, err := ts.ReconcileAfterStart(ctx)            // n == 2
	// both E1 commands: outcome unknown, detail reconcileDetail, settled_at set
	// E2 command unchanged (succeeded)
	// audit: exactly one row action='endpoint.commands.reconciled' resource=E1.ID details='commands=2' result='unknown' user_id='system' scope='organization' organization_id=E1.org; none for E2
	n, err = ts.ReconcileAfterStart(ctx)             // n == 0, audit count unchanged
}
func TestReconcileAfterStartLeavesDeployments(t *testing.T) { /* applyFixture: a row in applying stays applying */ }
func TestReconcileAfterStartHonoursFirstAnswer(t *testing.T) { /* SettleCommand(succeeded) then reconcile: still succeeded */ }
func TestReconcileAfterStartSettlesRowsWithStrayTimestamps(t *testing.T) { /* raw UPDATE settled_at=now, outcome='' then reconcile: unknown */ }
```
- [ ] **Step 2: Run** `go test -count=1 -run 'TestReconcileAfterStart' ./internal/store/` → FAIL.
- [ ] **Step 3: Implement** in one transaction: `SELECT endpoint_id, COUNT(*) FROM endpoint_commands WHERE outcome='' GROUP BY endpoint_id`; for each endpoint `UPDATE endpoint_commands SET outcome=?, detail=?, settled_at=? WHERE endpoint_id=? AND outcome=''` and insert the audit row with `LogAudit`-style fields the way `auditDeployment` does for the system actor (`user_id='system'`, `scope='system'`, `organization_id` from the endpoint row so the tenant audit view can still find it — read `auditDeployment` and mirror its columns). Commit; return the sum of rows affected.
- [ ] **Step 4: Boot hook.** In `runServer` after `Initialize`: `if n, err := st.Tenancy().ReconcileAfterStart(ctx); err != nil { log.Fatalf("Failed to reconcile in-flight commands: %v", err) } else if n > 0 { log.Printf("[RECOVERY] %d in-flight command(s) settled as unknown after restart", n) }`. Ordering test: if `cmd/server` has a test harness around store setup, assert the reconcile runs before `NewServer`; otherwise add `TestStartupReconcilesBeforeServing` in `internal/api` is out of scope — document the ordering in the root AGENTS.md startup paragraph and rely on the store test (say so in the report).
- [ ] **Step 5: Run** both drivers → PASS; gofmt; commit `feat(store): settle in-flight commands as unknown at startup`.

---

### Task 2: Schema version in the capsule recipe and drill

**Files:**
- Modify: `internal/store/migrations/migrations.go` (`func Latest() int` returning the highest `Version` in `registry`)
- Modify: `internal/backup/payload.go:85-92` (recipe key `schema_version`)
- Modify: `internal/backup/drill.go` (require the key; `schemaVersionCheck(name, path, expected int)`)
- Test: `internal/store/migrations/migrations_test.go` (Latest equals the last entry), `internal/backup/drill_test.go` (recipe rejection; pass on current; fail on behind, ahead, missing table), `internal/backup/payload_test.go` (recipe carries it)
- Modify: `internal/backup/AGENTS.md`

**Interfaces:**
- Produces: `migrations.Latest() int`; drill check `Schema Version: data/ky_server.db`.

- [ ] **Step 1: Failing tests.** In `drill_test.go` extend `TestDrillRejectsMalformedRecipes` with a recipe lacking `schema_version` (message `schema_version must be the latest migration number`) and one where it is a string; add `TestDrillChecksSchemaVersion`: build a scratch dir with a real migrated SQLite (use the testdb helper or `store.Open` on a temp DSN, then copy the file), run `Checks` with `schema_version: migrations.Latest()` → passing check; then `UPDATE schema_migrations SET version=version-1 WHERE version=(SELECT MAX(version) ...)` (or delete the top row) → failing check with `database schema is version N, capsule expects M`; insert a row with `version = Latest()+1` → failing with the same shape; a SQLite file with no `schema_migrations` → failing check whose message mentions `schema_migrations`.
- [ ] **Step 2: Run** `go test -count=1 ./internal/backup/ ./internal/store/migrations/` → FAIL.
- [ ] **Step 3: Implement.** `Latest()` iterates `registry`. `Collect` adds `"schema_version": migrations.Latest()`. `Checks`: `expected, ok := recipe["schema_version"].(float64)` (JSON numbers decode as float64 after a capsule round-trip; accept `int` too for the in-process manifest) else `recipeFailure("schema_version must be the latest migration number")`; after the integrity check for the database path, append `schemaVersionCheck(name, full, int(expected))`, which opens read-only like `sqliteIntegrityCheck`, runs `SELECT COALESCE(MAX(version),0) FROM schema_migrations`, and fails with the exact message on mismatch or with `schema_migrations table missing or unreadable` on query error.
- [ ] **Step 4: Run** → PASS; gofmt; commit `feat(backup): capsules pin the schema version and the drill checks it`.

---

### Task 3: Restore-verification test

**Files:**
- Create: `internal/store/recovery_test.go` (SQLite only; skip on other drivers like `application_backup_test.go`)
- Reuse: fixtures from `application_backup_test.go` (adopt, map, plan, image_checks, registry), `applyFixture` (apply + settle for a succeeded deployment and a second one left `applying`), the endpoint fixture used in `commands_test.go` / `endpoints_test.go` (enrollment token, keys, capabilities via `AcceptInventory`/capability recording, `RevokeEndpoint`), `backup.Collect`.
- Modify: `internal/store/AGENTS.md` (Verification: name the test)

- [ ] **Step 1: Write the test** `TestRestoreCarriesEveryControlPlaneTable`: build the state listed in the spec; `backup.Collect`; write `data/ky_server.db` and the keys to a temp dir; `store.Open`; `ReconcileAfterStart`; assert each item (registry credential via `ResolveRegistryAccess(..., ApplicationDeploy, "ghcr.io/org/app:v1", key, true)` returns the canary; `ResolveApplicationSecrets` returns the values; `ReadImageChecks` returns the row; `ReadDeployment` of the succeeded one has plan and result; the `applying` one is still `applying`; endpoint list shows the revoked endpoint as revoked and the active one with its capabilities; both commands `unknown` with the reconcile detail; exactly one reconcile audit row; a second `ReconcileAfterStart` returns 0 and the audit count is unchanged).
- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestRestoreCarriesEveryControlPlaneTable' ./internal/store/` → PASS (this task adds coverage; if any assertion fails, that is a defect to fix in the owning code, report it).
- [ ] **Step 3: Commit** `test(store): prove every control-plane table survives a sealed restore`.

---

### Task 4: Documents and the acceptance runbook

**Files:**
- Modify: `docs/RESTORE.md` ("What a capsule holds"; Step 5 "re-check endpoints"; the `instance.key` paragraph), `README.md` (Disaster recovery: control plane only, link), `KyYard-Implementation-Plan.md` §8 (PR 17 engineering delivered; the gate waits on the acceptance record), root `AGENTS.md` (startup order sentence if Task 1 did not add it), `internal/backup/AGENTS.md`, `internal/store/AGENTS.md`
- Create: `docs/ACCEPTANCE.md`

- [ ] **Step 1: RESTORE.md and README** per the spec's Documents section, verbatim facts: the capsule holds the control plane only (database snapshot, encryption/session/instance keys, settings, recovery public key), no workload volumes, images, container data or remote host data; in-flight commands show `unknown` with `the server restarted before a result arrived`; an applying deployment shows `unknown` until the agent reconnects and re-sends within 24 h; re-revoke agents revoked after `created_at`; `instance.key` signs the endpoint fingerprint and the local Docker binding, do not rotate during a restore.
- [ ] **Step 2: docs/ACCEPTANCE.md**: prerequisites; the nine steps from `KyYard-Implementation-Plan.md` §7 expanded against the current UI (read `web/src/pages/*.tsx` and `web/src/components/*.tsx` for screen and button names: Dashboard, Organization, Environment, Endpoint page, Applications with Adoption/Mapping/Comparison/Preflight/Deployment plan/Updates panels, Backup, Settings); for each step: where to click, what to type, what "pass" looks like, what to record; a results table template; the release-defect rule copied from §7.
- [ ] **Step 3: `make ci`** from the worktree root and the store suite on PostgreSQL. Expected: exit 0.
- [ ] **Step 4: Commit** `docs: recovery runbook fixes and the 0.1 acceptance script`.
