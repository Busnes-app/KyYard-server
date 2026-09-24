# Recovery and operator acceptance (PR 17)

The last engineering slice before the M7a gate. It closes the recovery promises the
implementation plan makes for 0.1: every control-plane table added since the base survives a
sealed backup and restore with its secrets usable, a restored server never executes work that
was in flight when the capsule was taken, the operator documents say what a capsule does and
does not hold, and the section 7 human acceptance script exists as a runbook Yoshi can run.

Decisions recorded 2026-09-24 (Yoshi): restored in-flight commands settle as `unknown` at boot;
`applying` deployments are left to the existing sweep and to the agent's re-sent result; the
capsule recipe pins the schema version; the acceptance run itself stays with Yoshi.

## Startup reconciliation

`Tenancy().ReconcileAfterStart(ctx) (int64, error)` runs in `cmd/server` `runServer` after
`Initialize` and before the listener, the backup loop and the prune loop start:

- Every `endpoint_commands` row with `outcome=''` becomes `outcome='unknown'`,
  `detail='the server restarted before a result arrived'`, `settled_at=now`, whether or not
  `dispatched_at` is set. A never-dispatched row is never sent: dispatch happens only inside
  the creating request, so a restored pending command cannot execute; it just stops looking
  pending. A dispatched row's real outcome, if the agent still has it, is not modelled for
  container commands: the agent keeps a 24-hour dedupe ledger, but it only answers a
  re-dispatch and never re-sends command results on reconnect, so `unknown` is the truthful
  state.
- One audit row per affected endpoint: `action='endpoint.commands.reconciled'`,
  `user_id='system'`, `scope='organization'` with the command's organization and environment (so tenant audit views list it, like `auditDeployment`), `resource=<endpoint id>`, `details='commands=<n>'`, `result='unknown'`,
  written through the existing system-audit path (`systemTransition`'s audit shape).
- `deployments` in `applying` are untouched: the periodic sweep marks them `unknown` two
  minutes past their deadline, and an agent that re-sends the result (its ledger keeps results
  for 24 h) settles them with what actually happened. `planned` rows past `expires_at` are
  already treated as expired by every reader.
- Idempotent: a second run affects no rows and writes no audit row.
- A failure to reconcile is fatal at startup (`log.Fatalf`), like a failed migration: serving
  with stale in-flight rows is the defect this slice removes.

## Capsule schema version

- `backup.Collect` adds `"schema_version": <latest migration number>` to the verification
  recipe (`migrations.Latest()` — a new exported accessor returning the highest `Version` in
  the list).
- `backup.Checks` requires the field and adds check `Schema Version: data/ky_server.db`: it
  opens the restored SQLite read-only and requires `MAX(version)` from `schema_migrations` to
  equal the recipe's value. A capsule from an older schema fails the drill with
  `database schema is version N, capsule expects M` instead of migrating silently on first
  start; an operator restoring an old capsule sees the message and decides.
- The lib's manifest/recipe shape does not change; the recipe is an opaque map to it.

## Restore verification test

`internal/store/recovery_test.go` (SQLite only, like the existing drill test): builds a
database with one of everything the base did not have — an organization with a registry (with
credential), an application with revisions and encrypted environment values, an adopted and
mapped instance with `application_resources`, an `image_checks` row, a `deployments` row in
`succeeded` with plan and result and one in `applying`, an endpoint with keys, capabilities,
an enrollment token, a dispatched and an undispatched command, and a revoked identity; runs
`backup.Collect` → capsule seal/open (the existing scratch-kit helpers) → restore into a new
data directory → `store.Open` → `ReconcileAfterStart`; then asserts: the registry credential
decrypts under the restored key; application values resolve; the image check reads back; the
succeeded deployment's plan and result read back; the endpoint's keys and capabilities read
back and the revoked one is still revoked; both commands are `unknown` with the reconcile
detail and one audit row names the endpoint; the `applying` deployment is still `applying`
(the sweep owns it). The test proves the same on a second `ReconcileAfterStart` (no change).

## Documents

- `docs/RESTORE.md`:
  - "What a capsule holds": add that it holds the control plane only — no workload volumes,
    no images, no container data, nothing from a remote host; those live on the hosts and are
    the host's backup problem; KyYard's application removal keeps volumes for the same reason.
  - Step 5: add "re-check endpoints": a capsule from before a revocation brings the agent
    identity back; re-revoke from the environment screen (the audit walk lists them); every
    in-flight command from the capsule moment shows as `unknown` with the restart detail; a
    deployment that was applying shows `unknown` until the agent reconnects and re-sends its
    result (within 24 h) — read the deployment history before planning again.
  - Fix the stale sentence about `instance.key`: it signs the control plane's endpoint
    fingerprint and the local Docker binding; rotating it changes the fingerprint every agent
    pinned, so do not rotate it during a restore.
- `README.md` Disaster recovery: one sentence on control plane only, linking RESTORE.md.
- `docs/ACCEPTANCE.md` (new): the section 7 script as a runbook: prerequisites (clean install,
  two disposable Docker hosts, a sample Compose app with a volume, two organizations, an
  operator unfamiliar with KyYard), then the nine steps expanded to the current UI (screen
  names, the exact actions, what "pass" looks like), including for step 6 the Updates panel,
  Plan update and apply, and for step 9 the Backup screen's drill and the restore command;
  a results template (step, pass/fail, confusing moments, recovery outcome) and the release
  defect rule copied from the plan. The run is Yoshi's; the runbook is the deliverable.
- `KyYard-Implementation-Plan.md` section 8: PR 17 engineering delivered; the M7a gate waits
  on the acceptance run's record.
- `internal/store/AGENTS.md`, `internal/backup/AGENTS.md`, root `AGENTS.md` (the `cmd/server`
  paragraph mentions the reconcile step in the startup order).

## Tests

Store (SQLite + PostgreSQL): reconciliation settles dispatched and undispatched rows, leaves
settled rows alone, audits once per endpoint, is idempotent, leaves `applying` deployments.
Backup: recipe carries `schema_version`; the drill passes on a current database and fails on
one whose `schema_migrations` is behind, with the exact message; the recipe-shape tests reject
a missing field. Restore verification as above. `cmd/server`: a startup test (the existing
smoke or a unit test around `runServer`'s store setup) proves reconcile runs before the
listener accepts.

## Out of scope

PostgreSQL capsules (capsules are SQLite snapshots by design; PostgreSQL operators back up
their database themselves, as the README says), agent-side command ledgers, PR D hardening.
