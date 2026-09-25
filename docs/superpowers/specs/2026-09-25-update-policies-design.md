# Update policies and maintenance windows (M7b PR 18)

Manual updates exist: an operator checks a mapped application against its registries, plans
the services with a newer digest and applies inside the plan's deadline. This slice lets an
organization administrator record that intent once as a policy, and a scheduler inside the
server performs it during a maintenance window, as that administrator, with every step
audited under one correlation ID. Health validation and rollback are PR 19.

Decisions recorded 2026-09-24 (Yoshi): per-application policy created by an organization
admin, mode `apply` or `plan_only`; window = weekday set plus local start and end in an IANA
zone evaluated in wall-clock time; a window missed while the server was down is recorded and
skipped; one automated deployment per endpoint at a time and at most two server-wide; no
retry inside a window; after three consecutive failed windows the policy pauses until an
admin resumes it; each run acts as the creating user with `application.deploy` re-checked;
the scheduler runs inside the existing server process.

## Model

Migration 31 adds two tables.

```
update_policies
  id TEXT PRIMARY KEY, organization_id, environment_id, application_id (FK applications ON DELETE CASCADE,
  UNIQUE), created_by TEXT NOT NULL (user id), mode TEXT CHECK (mode IN ('apply','plan_only')),
  timezone TEXT NOT NULL, weekdays TEXT NOT NULL (comma list of 0-6, Sunday 0),
  start_minute INTEGER NOT NULL, end_minute INTEGER NOT NULL,
  status TEXT CHECK (status IN ('active','paused')), paused_reason TEXT NOT NULL DEFAULT '',
  consecutive_failures INTEGER NOT NULL DEFAULT 0, created_at, updated_at

policy_runs
  id TEXT PRIMARY KEY, policy_id (FK update_policies ON DELETE CASCADE), organization_id,
  environment_id, application_id, occurrence TIMESTAMP NOT NULL (the window's start, UTC),
  started_at, finished_at NULL, outcome TEXT CHECK (outcome IN
  ('skipped_missed','skipped_busy','no_update','planned','applied','blocked','failed','paused')),
  deployment_id TEXT NULL, detail TEXT NOT NULL DEFAULT '' (≤ 255), correlation_id TEXT NOT NULL,
  UNIQUE (policy_id, occurrence)
```

- One policy per application. `weekdays` non-empty; `0 ≤ start_minute < end_minute ≤ 1440`;
  window at least 15 minutes; no window crosses midnight in this slice (the editor says so).
  `timezone` must load with `time.LoadLocation`; `cmd/server` blank-imports `time/tzdata` so
  the check does not depend on the host.
- `UNIQUE (policy_id, occurrence)` is the idempotency key: one attempt per window across
  restarts and ticks.
- `detail` follows the fixed-text discipline: blocker codes, a result code, or one of the
  scheduler's own sentences; never registry or agent text.

## Occurrences and DST

For a policy at tick time `now`: convert `now` to the policy's zone, take the calendar date,
and build `start = time.Date(y, m, d, start_minute/60, start_minute%60, 0, 0, loc)` and `end`
the same way. If the zone has a gap at `start` (the built time's wall clock differs from the
requested minutes), the occurrence does not exist that day. An overlap yields one occurrence
(Go's first mapping). The occurrence is open when `start ≤ now < end` and the weekday of `d`
is in the set. The occurrence key stored is `start.UTC()`.

## Scheduler

`api.Server.RunPolicies(ctx, done chan<- struct{})` is started by `runServer` beside the
backup loop and stopped the same way: `done` closes only between runs, and `runServer` waits
on it under the existing `backupWaitTimeout` before the store closes. A one-minute ticker;
each tick, under a single `policyTick` mutex so ticks never overlap:

1. **Missed windows.** For every active policy whose most recent occurrence has ended with no
   `policy_runs` row, insert `skipped_missed` with the sentence `the server was not running
   during this window`. This is bookkeeping, not a failure.
2. **Due policies.** For every active policy whose occurrence is open and has no row, in
   `created_at` order:
   - Concurrency: skip (no row yet) when the application's endpoint has a deployment in
     `planned` or `applying` state or a policy run in flight, or when two policy runs are in
     flight server-wide. A policy still skipped when its window closes gets a `skipped_busy`
     row at the next tick (`another deployment occupied the window`); `skipped_busy` is not a
     failure.
   - Otherwise start the run in its own goroutine (the tick does not wait for it).
3. **Run.** With `a := store.TenantAccess{ActorID: created_by, OrganizationID, EnvironmentID,
   CorrelationID: uuid}`:
   - Insert the `policy_runs` row (`started_at`, outcome empty until finished; the UNIQUE key
     refuses a duplicate).
   - Authorization: `CheckImageUpdateAccess(ctx, a, app)`. `ErrForbidden` (the creator lost
     `application.deploy`, was disabled, or left the organization) → outcome `paused`,
     detail `the policy's creator no longer holds application.deploy`, policy status `paused`
     with the same reason. This is the only path that pauses without counting.
   - Take `guardApplication` (a manual check or update plan in flight → `skipped_busy`) and a
     registry slot (none free within 30 s → `skipped_busy`).
   - `CheckImageUpdates`. Services with verdict `update_available` are the update set; empty →
     `no_update`. Any `registry_error` with no `update_available` → `failed` with the verdict's
     detail code.
   - Live inspections (`planInspections`) and `PlanDeployment` with `Update` = the update set,
     `MaxFrameBytes` from the endpoint's capabilities. `PreflightBlockedError` → `blocked`,
     detail the blockers joined by `,` (≤ 255); other errors → `failed` with the store error
     code.
   - `plan_only` → outcome `planned`, `deployment_id` set; the plan waits for a click like any
     manual plan.
   - `apply` → `ApplyDeployment` and `deliver` exactly as `handleApplyDeployment` does
     (including `FailDeployment` when delivery fails → `failed`, detail `not_sent`); outcome
     `applied`, `deployment_id` set. Settlement is the deployment's own affair; the run does
     not wait for it (PR 19 observes it).
   - The run's plan, apply and audit rows carry `a.CorrelationID`; the scheduler writes one
     audit row per run, `user_id = 'system'`, action `application.policy.run`, resource
     `<app>/policies/<policy>/runs/<run>`, result from the outcome (`applied`/`planned`/
     `no_update`/`skipped_*` → `success`; `blocked`/`failed` → `failure`; `paused` → `denied`),
     details the outcome word, correlation the run's ID.
4. **Failure counting.** `blocked` and `failed` increment `consecutive_failures`; `applied`,
   `planned` and `no_update` reset it to zero; `skipped_*` leave it. Reaching 3 pauses the
   policy: status `paused`, reason `three consecutive windows failed`, an audit row
   `application.policy.paused` (`system`, result `failure`). Resume resets the counter.
5. **Shutdown.** A run in flight finishes (its `ApplyDeployment` uses `context.WithoutCancel`
   like the backup deposit); `done` closes when no run is in flight. A run interrupted by a
   crash leaves a row with empty outcome; `startStore`'s reconcile marks such rows `failed`,
   detail `the server restarted during the run`, and counts them.

## Authorization

New action `application.policy` (create, edit, delete, resume), granted to `organization_admin`
only; reading a policy and its runs uses `application.read`. Runs act as `created_by` with
`application.deploy` re-checked as above. Editing a policy sets `created_by` to the editor
(the policy always acts as its last editor). Deleting a policy deletes its runs (cascade);
deployments it created stay.

## API

Under `/api/organizations/{organization}/environments/{environment}/applications/{application}`:

- `GET /update-policy` → 200 the policy with `next_occurrence` (RFC3339 in UTC, computed) and
  the last 20 runs, or 404.
- `PUT /update-policy` body `{mode, timezone, weekdays: [0..6], start_minute, end_minute}` →
  200/201; validation errors 400 with a code (`invalid_timezone`, `invalid_window`,
  `invalid_weekdays`, `invalid_mode`); CSRF; `application.policy`.
- `DELETE /update-policy` → 204.
- `POST /update-policy/resume` → 200 the policy (status `active`, counter 0); 409 when not
  paused.
- `GET /update-policy/runs?limit=` → runs newest first (≤ 100).

All writes audited through `withTenantTarget` with the policy ID as resource.

## UI

An **Update policy** card on the application page beside Updates: mode, weekday toggles, start
and end time, timezone (a `<select>` from `Intl.supportedValuesOf('timeZone')`, defaulting to
the browser's zone), Save/Delete; status line with the next occurrence in the policy's zone and
the browser's; pause reason with a Resume button; the run list (occurrence, outcome text from a
`RUN_OUTCOMES` table, detail rendered through the existing blocker/code tables, link to the
deployment). Only `organization_admin` sees the editor; others see the read-only card.

## Documents

`docs/application-schema.md` (policies section), `docs/authorization-matrix.md`
(`application.policy`), `docs/threat-model.md` (scheduler acts as a user; lapse pauses),
`README.md` (the scheduler and the `time/tzdata` note), `KyYard-Implementation-Plan.md` §8
(PR 18 done; PR 19 next), AGENTS.md for store, api, web, cmd/server.

## Tests

Store (SQLite + PostgreSQL): migration; validation of every field; occurrence computation
across a DST gap (2026-03-08 02:30 America/New_York does not exist), an overlap (2026-11-01
01:30) and a zone east of UTC crossing the date line; `UNIQUE (policy_id, occurrence)`;
failure counting and pause; resume; reconcile of an interrupted run. API/scheduler (fake
agent socket, `newPlanHost`): a tick inside the window with an update available plans and
applies as the creator under one correlation ID and delivers the frame; `plan_only` stops at
`planned`; no update → `no_update`; blocker → `blocked`; creator loses the role → `paused`
with the audit row; a missed window → `skipped_missed`; endpoint busy → `skipped_busy`; two
runs in flight block a third; three failures pause; shutdown waits for a run in flight
(`done` closes after). Web: editor validation, timezone list, run list rendering, admin-only
editor.

## Out of scope

Cross-midnight windows, interval mode, approval flows, health validation and rollback (PR
19), per-service policies, notifications beyond the UI.
