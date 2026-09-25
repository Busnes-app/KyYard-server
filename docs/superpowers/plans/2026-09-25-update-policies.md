# Update Policies and Maintenance Windows (M7b PR 18) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an organization administrator record a per-application update policy with a weekly maintenance window, and have a scheduler inside the server check, plan and (in `apply` mode) apply available image updates in that window as that administrator, every step audited under one correlation ID.

**Architecture:** Migration 31 adds `update_policies` and `policy_runs`; the store owns validation, occurrence computation (wall clock in an IANA zone, DST gaps skipped, overlaps once), the `UNIQUE (policy_id, occurrence)` idempotency key, failure counting and pausing, and startup reconciliation (Tasks 1–2). The scheduler lives in `internal/api/policies.go` because it reuses unexported `api.Server` pieces (the per-application guard, registry slots, plan-time inspections, `maxFrameBytes`, the agent registry); those pieces gain writer-free cores the HTTP paths now wrap (Task 3). Five routes expose the policy (Task 4); `cmd/server` starts `RunPolicies` beside `backupLoop`, waits for it before the store closes and embeds `time/tzdata` (Task 5). The web gets an Update policy card (Task 6); documents close (Task 7).

**Tech Stack:** Go (`time.LoadLocation`, `time/tzdata`), SQLite + PostgreSQL 17, the coder/websocket agent protocol (fake agent socket in tests), React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-25-update-policies-design.md`

## Global Constraints

- Migration **31** (`update_policies`), both dialects, exactly these shapes (spec, Model):

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

  Tenant FK is the composite `(organization_id, environment_id, application_id) REFERENCES applications(organization_id, environment_id, id) ON DELETE CASCADE`, as `deployments` does. Timestamps are `DATETIME` (SQLite) / `TIMESTAMPTZ` (PostgreSQL), bound as `time.Time` in UTC. Text columns carry length CHECKs.
- Validation (spec): `weekdays` non-empty; `0 ≤ start_minute < end_minute ≤ 1440`; window at least 15 minutes; no window crosses midnight; `timezone` must load with `time.LoadLocation`. One policy per application.
- Occurrences (spec, verbatim): "For a policy at tick time `now`: convert `now` to the policy's zone, take the calendar date, and build `start = time.Date(y, m, d, start_minute/60, start_minute%60, 0, 0, loc)` and `end` the same way. If the zone has a gap at `start` (the built time's wall clock differs from the requested minutes), the occurrence does not exist that day. An overlap yields one occurrence (Go's first mapping). The occurrence is open when `start ≤ now < end` and the weekday of `d` is in the set. The occurrence key stored is `start.UTC()`."
- Outcome vocabulary, exactly: `skipped_missed`, `skipped_busy`, `no_update`, `planned`, `applied`, `blocked`, `failed`, `paused`.
- The scheduler's own sentences, exactly: `the server was not running during this window` (skipped_missed), `another deployment occupied the window` (skipped_busy at the tick), `the policy's creator no longer holds application.deploy` (paused), `three consecutive windows failed` (pause reason), `the server restarted during the run` (reconcile). `detail` is only a blocker list, a code, or one of these sentences; never registry or agent text.
- Scheduler, verbatim from the spec: "`api.Server.RunPolicies(ctx, done chan<- struct{})` is started by `runServer` beside the backup loop and stopped the same way: `done` closes only between runs, and `runServer` waits on it under the existing `backupWaitTimeout` before the store closes. A one-minute ticker; each tick, under a single `policyTick` mutex so ticks never overlap". Runs start in `created_at` order, each in its own goroutine; the tick does not wait for them.
- Concurrency: one automated run per endpoint, at most **2** server-wide; skip (no row) when the application's endpoint has a deployment `planned` or `applying` or a policy run in flight; a run waits at most **30 s** for a registry slot (else `skipped_busy`); a policy still skipped when its window closes gets `skipped_busy` at the next tick. No retry inside a window.
- Failure counting (spec): "`blocked` and `failed` increment `consecutive_failures`; `applied`, `planned` and `no_update` reset it to zero; `skipped_*` leave it. Reaching 3 pauses the policy: status `paused`, reason `three consecutive windows failed`, an audit row `application.policy.paused` (`system`, result `failure`). Resume resets the counter." `ErrForbidden` from the creator's authorization pauses without counting.
- Each run acts as `store.TenantAccess{ActorID: created_by, OrganizationID, EnvironmentID, CorrelationID: uuid}`; the run's check, plan, apply and audit rows carry that correlation ID. Scheduler audit row per run: `user_id = 'system'`, action `application.policy.run`, resource `<app>/policies/<policy>/runs/<run>`, result `applied`/`planned`/`no_update`/`skipped_*` → `success`, `blocked`/`failed` → `failure`, `paused` → `denied`; details the outcome word; correlation the run's correlation ID.
- Authorization: new action `application.policy` (create, edit, delete, resume), `organization_admin` only; reads use `application.read`. Editing sets `created_by` to the editor. Deleting a policy deletes its runs; deployments stay.
- Routes, under `/api/organizations/{organization}/environments/{environment}/applications/{application}`: `GET /update-policy` (200 with `next_occurrence` RFC3339 UTC and the last 20 runs, or 404); `PUT /update-policy` body `{mode, timezone, weekdays, start_minute, end_minute}` → 201 created / 200 edited, 400 codes `invalid_timezone`, `invalid_window`, `invalid_weekdays`, `invalid_mode`, CSRF; `DELETE /update-policy` → 204; `POST /update-policy/resume` → 200, 409 when not paused; `GET /update-policy/runs?limit=` newest first, at most 100. Writes audit through the tenant transaction with the policy as resource.
- `cmd/server` blank-imports `time/tzdata`.
- Every store behaviour is tested on SQLite and PostgreSQL. PostgreSQL runs use the local container `kyyard-access-pg`; read the password into a variable, never print it: `PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test ...`. Below this prefix is written `PG=… go test`.
- `web/dist` is rebuilt with `make build-web` and committed in the web task (Task 6).
- `gofmt -w` every edited Go file and check `gofmt -l internal cmd` prints nothing before each commit. Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

Plan decisions where the code or the spec's silence forced a choice (each is reported to the reviewer; none changes a spec value):

- `policy_runs.outcome` is inserted empty ("outcome empty until finished") but the spec's CHECK list has no `''`: the CHECK is `outcome IN ('','skipped_missed',…,'paused')` with `NOT NULL DEFAULT ''`.
- An expired plan never leaves state `planned` (expiry is computed from `expires_at`; nothing sweeps it). Counting every `planned` row would make one unclicked `plan_only` plan block its endpoint forever, so "busy" is `state='applying' OR (state='planned' AND expires_at > now)`, `now` the wall clock.
- A window is recorded as missed only if it ended after the policy's `updated_at`: otherwise saving a policy (or resuming one) would immediately record a window that predates it as "the server was not running". Saving and resuming set `updated_at`; run bookkeeping does not.
- `guardApplication`, `acquireRegistrySlot` and `planInspections` take an `http.ResponseWriter` (and a `*http.Request`); each gains a writer-free core (`holdApplication`, `takeRegistrySlot` plus a polling `waitRegistrySlot`, `inspectForPlan`) that the HTTP wrapper and the scheduler share.
- A run holds its registry slot from the check through the plan, in the spec's order; a manual plan takes its slot after the inspections.
- `ErrForbidden` from any step of a run (not only `CheckImageUpdateAccess`) means the creator lost authority mid-run and pauses the same way.
- Detail codes for store refusals: `not_adopted` (`ErrNotFound`: the instance was released; distinct from the registry's `not_found`), `mapping_required`, `adoption_changed`, `deployment_in_progress`, `endpoint_offline`, `invalid`, anything else `error`; `not_sent` for an undeliverable frame; `skipped_busy` inside a run uses `check_in_progress` (guard held) or `too_many_checks` (no slot in 30 s).
- The creator-lost pause also writes `application.policy.paused` (result `denied`, details the sentence), so every pause has its own row.
- `next_occurrence` is `null` while the policy is paused. `GET /update-policy` answers 404 for "no policy" without a store error, so opening the card on an application without one writes no failure audit row.
- Audit resource for policy writes is `<app>/policies/<policy>` (the run rows' parent); details on a run row are the bare outcome word.
- Startup reconciliation runs inside `ReconcileAfterStart`'s transaction; its return value still counts commands only.
- There is no deployment page in the web router, so the run list shows the deployment ID's first eight characters (full ID in `title`) instead of a link.
- `Intl.supportedValuesOf('timeZone')` omits `UTC` and aliases such as `US/Eastern` (verified in Node): the zone `<select>` always includes the saved zone and the browser's own.
- A run's worst case (30 s slot wait + 60 s check + 10 s inspections + 60 s plan-time registry + apply) is under 3 minutes, far inside `backupWaitTimeout` (17 min); `docker-compose.yml`'s `stop_grace_period` and `TestComposeGracePeriodCoversTheShutdownBudget` stay as they are.
- There is no `cmd/server/AGENTS.md`; the root `AGENTS.md` owns `cmd/server` and takes that DOX update.

## Review Focus

1. An application released (or deleted) between the tick that found its window open and the run: a released one must record `failed` `not_adopted` and count toward the pause, not crash or plan against nothing; a deleted one (policy cascaded away) must record nothing and log nothing. Pinned in Task 2 (`TestBeginPolicyRunRefusesAGonePolicy`) and Task 3 (`TestPolicyRunOnAReleasedApplicationFails`).
2. A saved zone the server accepts but the browser's zone list lacks (`US/Eastern`, `UTC`): the editor must keep it selected, not silently swap it for the first list entry on the next save. Pinned in Task 6 (`keeps a saved zone this browser does not list…`).
3. Ticks overlapping themselves (a slow tick still running when the next starts) must not start two runs for one occurrence: one row, one plan. Pinned in Task 3 (`TestPolicyTicksNeverOverlap`).
4. A run that starts at 23:59 in a window ending at midnight belongs to that day's occurrence; at 00:00 nothing is open and nothing is recorded as missed. Pinned in Task 2 (`TestPolicyWindowEndingAtMidnight`).
5. The creator's account deleted outright (not just demoted): the next run must pause the policy with its audit rows and make no deployment. Pinned in Task 3 (`TestPolicyPausesWhenTheCreatorIsDeleted`).

---

### Task 1: Permission, migration 31 and the policy CRUD in the store

**Files:**
- Modify: `internal/permissions/permissions.go` (the `ApplicationPolicy` action and the organization-admin grant)
- Test: `internal/permissions/permissions_test.go`
- Modify: `internal/store/migrations/migrations.go` (append migration 31 to `registry`)
- Create: `internal/store/policies.go`
- Modify: `internal/store/store.go` (`TenancyStore` gains five methods)
- Test: `internal/store/policies_test.go`
- Docs: `internal/permissions/AGENTS.md`, `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: `tenancyStore.run`, `readTenant`, `lockApplication`, `displaySafe`, `planFixture` (test), `tenantAtomicStore` (test), `imageCheckKey` (test).
- Produces:
  ```go
  // package permissions
  const ApplicationPolicy Action = "application.policy"

  // package store
  const (
  	PolicyModeApply, PolicyModePlanOnly = "apply", "plan_only"
  	PolicyActive, PolicyPaused          = "active", "paused"
  	MinPolicyWindowMinutes              = 15
  	PolicyFailureLimit                  = 3
  	MaxPolicyRuns                       = 100
  	RunSkippedMissed, RunSkippedBusy, RunNoUpdate, RunPlanned = "skipped_missed", "skipped_busy", "no_update", "planned"
  	RunApplied, RunBlocked, RunFailed, RunPaused              = "applied", "blocked", "failed", "paused"
  	PolicyDetailMissed      = "the server was not running during this window"
  	PolicyDetailBusy        = "another deployment occupied the window"
  	PolicyDetailCreatorLost = "the policy's creator no longer holds application.deploy"
  	PolicyDetailRestarted   = "the server restarted during the run"
  	PolicyReasonFailures    = "three consecutive windows failed"
  )
  var ErrInvalidTimezone, ErrInvalidWindow, ErrInvalidWeekdays, ErrInvalidMode, ErrPolicyNotPaused error
  type PolicyInput struct { Mode, Timezone string; Weekdays []int; StartMinute, EndMinute int } // json: mode, timezone, weekdays, start_minute, end_minute
  type UpdatePolicy struct { ID, ApplicationID, CreatedBy, Mode, Timezone string; Weekdays []int; StartMinute, EndMinute int; Status, PausedReason string; ConsecutiveFailures int; CreatedAt, UpdatedAt time.Time; NextOccurrence *time.Time; OrganizationID, EnvironmentID string /* json:"-" */ }
  type PolicyRun struct { ID, PolicyID string; Occurrence, StartedAt time.Time; FinishedAt *time.Time; Outcome, DeploymentID, Detail, CorrelationID string }
  PutUpdatePolicy(ctx context.Context, access TenantAccess, applicationID string, input PolicyInput) (*UpdatePolicy, bool, error) // bool: created
  ReadUpdatePolicy(ctx context.Context, access TenantAccess, applicationID string) (*UpdatePolicy, []PolicyRun, error) // nil policy, no error: none
  DeleteUpdatePolicy(ctx context.Context, access TenantAccess, applicationID string) error
  ResumeUpdatePolicy(ctx context.Context, access TenantAccess, applicationID string) (*UpdatePolicy, error)
  ListPolicyRuns(ctx context.Context, access TenantAccess, applicationID string, limit int) ([]PolicyRun, error)
  ```
  `NextOccurrence` stays `nil` until Task 2 fills it. Test helpers later tasks reuse: `dailyPolicy()`, `policyFixture`, `policyDraft`, `policyMember`, `instant`.

- [ ] **Step 1: Write the failing permission test**

Append to `internal/permissions/permissions_test.go`:

```go
// Update policies act unattended as the administrator who saved them, so only the organization
// administrator may create, edit, delete or resume one (docs/authorization-matrix.md).
func TestUpdatePolicyIsOrganizationAdminOnly(t *testing.T) {
	for _, role := range []string{"organization_admin", "environment_admin", "operator", "developer", "read_only", "admin", "unknown", ""} {
		if Allows(role, ApplicationPolicy) != (role == "organization_admin") || PlatformAllows(role, ApplicationPolicy) {
			t.Errorf("%q application.policy", role)
		}
	}
	if ApplicationPolicy != "application.policy" {
		t.Fatal("the audit identifier changed")
	}
}
```

- [ ] **Step 2: Write the failing store tests**

Create `internal/store/policies_test.go`:

```go
package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // zone tests must not depend on the host's zoneinfo

	"github.com/google/uuid"
)

func dailyPolicy() PolicyInput {
	return PolicyInput{Mode: PolicyModeApply, Timezone: "UTC", Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, StartMinute: 600, EndMinute: 660}
}

func instant(s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return v.UTC()
}

// policyFixture is planFixture's adopted, mapped application with a policy the actor saved.
func policyFixture(t *testing.T, in PolicyInput) (*SQLStore, TenantAccess, *Application, string, *ApplicationMapping, *UpdatePolicy) {
	t.Helper()
	st, a, app, endpoint, _, m := planFixture(t)
	p, created, err := st.Tenancy().PutUpdatePolicy(context.Background(), a, app.ID, in)
	if err != nil || !created {
		t.Fatalf("policy: created=%v %v", created, err)
	}
	return st, a, app, endpoint, m, p
}

// policyDraft imports an application nobody adopted, which DiscardApplication can delete.
func policyDraft(t *testing.T, st *SQLStore, a TenantAccess, name string) *Application {
	t.Helper()
	app, err := st.Tenancy().ImportApplication(context.Background(), a, name, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}, map[string]string{}, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// policyMember adds an active member of a's organization with role and returns their access.
func policyMember(t *testing.T, st *SQLStore, a TenantAccess, id string, role TenantRole) TenantAccess {
	t.Helper()
	ctx := context.Background()
	if err := st.Users().CreateUser(ctx, &User{ID: id, Username: id, Role: "user", Status: "active", SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: id, Role: role, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	a.ActorID = id
	return a
}

// rawPolicyRun writes a finished run row directly, for tests that precede the scheduler's writers.
func rawPolicyRun(t *testing.T, st *SQLStore, p *UpdatePolicy, occurrence time.Time, outcome string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`INSERT INTO policy_runs(id,policy_id,organization_id,environment_id,application_id,occurrence,started_at,finished_at,outcome,correlation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`), id, p.ID, p.OrganizationID, p.EnvironmentID, p.ApplicationID, occurrence, occurrence, occurrence, outcome, "raw"); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUpdatePolicyTables(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO update_policies(id,organization_id,environment_id,application_id,created_by,mode,timezone,weekdays,start_minute,end_minute,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`), uuid.NewString(), a.OrganizationID, a.EnvironmentID, app.ID, "actor", "apply", "UTC", "1", 600, 660, "active", now, now); err == nil {
		t.Fatal("a second policy for one application")
	}
	for name, stmt := range map[string]string{
		"mode":   `UPDATE update_policies SET mode='yolo' WHERE id=?`,
		"status": `UPDATE update_policies SET status='gone' WHERE id=?`,
		"window": `UPDATE update_policies SET start_minute=600,end_minute=610 WHERE id=?`,
		"reason": `UPDATE update_policies SET paused_reason=` + "'" + strings.Repeat("r", 256) + "'" + ` WHERE id=?`,
	} {
		if _, err := st.db.ExecContext(ctx, st.rebind(stmt), p.ID); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	run := rawPolicyRun(t, st, p, instant("2026-09-24T10:00:00Z"), RunApplied)
	for name, stmt := range map[string]string{
		"outcome": `UPDATE policy_runs SET outcome='exploded' WHERE id=?`,
		"detail":  `UPDATE policy_runs SET detail=` + "'" + strings.Repeat("d", 256) + "'" + ` WHERE id=?`,
	} {
		if _, err := st.db.ExecContext(ctx, st.rebind(stmt), run); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// UNIQUE (policy_id, occurrence) is the idempotency key: one attempt per window.
	if _, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO policy_runs(id,policy_id,organization_id,environment_id,application_id,occurrence,started_at,correlation_id) VALUES(?,?,?,?,?,?,?,?)`), uuid.NewString(), p.ID, a.OrganizationID, a.EnvironmentID, app.ID, instant("2026-09-24T10:00:00Z"), now, "dup"); err == nil {
		t.Fatal("a second run for one occurrence")
	}
}

// A policy and its runs go with their application; deployments are another table's business.
func TestUpdatePolicyGoesWithItsApplication(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app := policyDraft(t, st, a, "draft")
	p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	rawPolicyRun(t, st, p, instant("2026-09-24T10:00:00Z"), RunNoUpdate)
	if err := ts.DiscardApplication(ctx, a, app.ID, 1); err != nil {
		t.Fatal(err)
	}
	var policies, runs int
	if err := st.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM update_policies),(SELECT COUNT(*) FROM policy_runs)`).Scan(&policies, &runs); err != nil || policies != 0 || runs != 0 {
		t.Fatalf("left behind: %d policies, %d runs, %v", policies, runs, err)
	}
}

func TestPutUpdatePolicyValidatesEveryField(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app := policyDraft(t, st, a, "draft")
	for _, tc := range []struct {
		name string
		edit func(*PolicyInput)
		want error
	}{
		{"no mode", func(in *PolicyInput) { in.Mode = "" }, ErrInvalidMode},
		{"unknown mode", func(in *PolicyInput) { in.Mode = "auto" }, ErrInvalidMode},
		{"no zone", func(in *PolicyInput) { in.Timezone = "" }, ErrInvalidTimezone},
		{"the host's zone", func(in *PolicyInput) { in.Timezone = "Local" }, ErrInvalidTimezone},
		{"unknown zone", func(in *PolicyInput) { in.Timezone = "Mars/Olympus_Mons" }, ErrInvalidTimezone},
		{"a path", func(in *PolicyInput) { in.Timezone = "../../etc/passwd" }, ErrInvalidTimezone},
		{"long zone", func(in *PolicyInput) { in.Timezone = "Europe/" + strings.Repeat("a", 60) }, ErrInvalidTimezone},
		{"control character", func(in *PolicyInput) { in.Timezone = "UTC\n" }, ErrInvalidTimezone},
		{"no weekdays", func(in *PolicyInput) { in.Weekdays = nil }, ErrInvalidWeekdays},
		{"empty weekdays", func(in *PolicyInput) { in.Weekdays = []int{} }, ErrInvalidWeekdays},
		{"weekday 7", func(in *PolicyInput) { in.Weekdays = []int{7} }, ErrInvalidWeekdays},
		{"negative weekday", func(in *PolicyInput) { in.Weekdays = []int{-1} }, ErrInvalidWeekdays},
		{"repeated weekday", func(in *PolicyInput) { in.Weekdays = []int{1, 1} }, ErrInvalidWeekdays},
		{"eight weekdays", func(in *PolicyInput) { in.Weekdays = []int{0, 1, 2, 3, 4, 5, 6, 0} }, ErrInvalidWeekdays},
		{"negative start", func(in *PolicyInput) { in.StartMinute = -1 }, ErrInvalidWindow},
		{"past midnight", func(in *PolicyInput) { in.StartMinute, in.EndMinute = 1430, 1441 }, ErrInvalidWindow},
		{"crosses midnight", func(in *PolicyInput) { in.StartMinute, in.EndMinute = 1380, 60 }, ErrInvalidWindow},
		{"empty window", func(in *PolicyInput) { in.EndMinute = in.StartMinute }, ErrInvalidWindow},
		{"fourteen minutes", func(in *PolicyInput) { in.EndMinute = in.StartMinute + 14 }, ErrInvalidWindow},
	} {
		in := dailyPolicy()
		tc.edit(&in)
		if _, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, in); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if p, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || p != nil {
		t.Fatalf("a refused policy was stored: %+v %v", p, err)
	}
	// The edges: an alias the zone database knows, fifteen minutes, both ends of the day.
	for _, in := range []PolicyInput{
		{Mode: PolicyModePlanOnly, Timezone: "US/Eastern", Weekdays: []int{6}, StartMinute: 0, EndMinute: 15},
		{Mode: PolicyModeApply, Timezone: "Pacific/Kiritimati", Weekdays: []int{0}, StartMinute: 1425, EndMinute: 1440},
	} {
		if _, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, in); err != nil {
			t.Fatalf("%+v: %v", in, err)
		}
	}
	if _, _, err := ts.PutUpdatePolicy(ctx, a, uuid.NewString(), dailyPolicy()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a policy for no application: %v", err)
	}
}

func TestPutUpdatePolicyActsAsTheLastEditor(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, PolicyInput{Mode: PolicyModePlanOnly, Timezone: "Europe/Paris", Weekdays: []int{5, 1, 3}, StartMinute: 120, EndMinute: 240})
	ctx := context.Background()
	if p.Mode != PolicyModePlanOnly || p.Timezone != "Europe/Paris" || !slices.Equal(p.Weekdays, []int{1, 3, 5}) || p.StartMinute != 120 || p.EndMinute != 240 || p.Status != PolicyActive || p.CreatedBy != "actor" || p.ConsecutiveFailures != 0 || p.PausedReason != "" || p.ApplicationID != app.ID {
		t.Fatalf("created: %+v", p)
	}
	other := policyMember(t, st, a, "other", RoleOrganizationAdmin)
	edited, created, err := st.Tenancy().PutUpdatePolicy(ctx, other, app.ID, dailyPolicy())
	if err != nil || created || edited.ID != p.ID || edited.CreatedBy != "other" || edited.Timezone != "UTC" || edited.Mode != PolicyModeApply || !edited.CreatedAt.Equal(p.CreatedAt) || edited.UpdatedAt.Before(p.UpdatedAt) {
		t.Fatalf("edited: created=%v %+v %v", created, edited, err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM audit_records WHERE action='application.policy' AND resource=? AND result='success'`), app.ID+"/policies/"+p.ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("save audit rows: %d %v", n, err)
	}
}

func TestUpdatePolicyAuthorization(t *testing.T) {
	st, a, app, _, _, _ := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	for id, role := range map[string]TenantRole{"envadmin": RoleEnvironmentAdmin, "operator": RoleOperator, "developer": RoleDeveloper, "viewer": RoleReadOnly} {
		m := policyMember(t, st, a, id, role)
		if _, _, err := ts.PutUpdatePolicy(ctx, m, app.ID, dailyPolicy()); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s put: %v", role, err)
		}
		if err := ts.DeleteUpdatePolicy(ctx, m, app.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s delete: %v", role, err)
		}
		if _, err := ts.ResumeUpdatePolicy(ctx, m, app.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s resume: %v", role, err)
		}
		if p, _, err := ts.ReadUpdatePolicy(ctx, m, app.ID); err != nil || p == nil {
			t.Errorf("%s read: %v", role, err)
		}
		if _, err := ts.ListPolicyRuns(ctx, m, app.ID, 20); err != nil {
			t.Errorf("%s runs: %v", role, err)
		}
	}
	outsider := a
	outsider.ActorID = "nobody"
	if _, _, err := ts.ReadUpdatePolicy(ctx, outsider, app.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-member read the policy: %v", err)
	}
}

func TestDeleteUpdatePolicyTakesItsRuns(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	rawPolicyRun(t, st, p, instant("2026-09-24T10:00:00Z"), RunNoUpdate)
	if err := ts.DeleteUpdatePolicy(ctx, a, app.ID); err != nil {
		t.Fatal(err)
	}
	if got, runs, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || got != nil || len(runs) != 0 {
		t.Fatalf("after delete: %+v %v %v", got, runs, err)
	}
	var runs int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM policy_runs`).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("runs left: %d %v", runs, err)
	}
	if err := ts.DeleteUpdatePolicy(ctx, a, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestResumeUpdatePolicy(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.ResumeUpdatePolicy(ctx, a, app.ID); !errors.Is(err, ErrPolicyNotPaused) {
		t.Fatalf("resume an active policy: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE update_policies SET status='paused',paused_reason=?,consecutive_failures=3 WHERE id=?`), PolicyReasonFailures, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ResumeUpdatePolicy(ctx, a, app.ID)
	if err != nil || got.Status != PolicyActive || got.ConsecutiveFailures != 0 || got.PausedReason != "" || got.UpdatedAt.Before(p.UpdatedAt) {
		t.Fatalf("resumed: %+v %v", got, err)
	}
}

func TestListPolicyRuns(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	for _, limit := range []int{0, -1, MaxPolicyRuns + 1} {
		if _, err := ts.ListPolicyRuns(ctx, a, app.ID, limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	older := rawPolicyRun(t, st, p, instant("2026-09-23T10:00:00Z"), RunNoUpdate)
	newer := rawPolicyRun(t, st, p, instant("2026-09-24T10:00:00Z"), RunSkippedMissed)
	runs, err := ts.ListPolicyRuns(ctx, a, app.ID, MaxPolicyRuns)
	if err != nil || len(runs) != 2 || runs[0].ID != newer || runs[1].ID != older || runs[0].Outcome != RunSkippedMissed || !runs[0].Occurrence.Equal(instant("2026-09-24T10:00:00Z")) || runs[0].FinishedAt == nil || runs[0].DeploymentID != "" {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	if runs, err := ts.ListPolicyRuns(ctx, a, app.ID, 1); err != nil || len(runs) != 1 || runs[0].ID != newer {
		t.Fatalf("limit 1: %+v %v", runs, err)
	}
	draft := policyDraft(t, st, a, "no-policy")
	if runs, err := ts.ListPolicyRuns(ctx, a, draft.ID, 20); err != nil || runs == nil || len(runs) != 0 {
		t.Fatalf("no policy: %+v %v", runs, err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/permissions/ ./internal/store/ -run 'UpdatePolicy|PolicyRuns|ResumeUpdatePolicy'`
Expected: FAIL to compile with `undefined: ApplicationPolicy` and `undefined: PolicyInput`.

- [ ] **Step 4: Add the permission**

In `internal/permissions/permissions.go`, after the `RegistryManage` constant add:

```go
	// ApplicationPolicy creates, edits, deletes and resumes an application's update policy. A
	// policy deploys unattended as whoever saved it last, so the matrix stops at the organization
	// administrator.
	ApplicationPolicy Action = "application.policy"
```

and add `ApplicationPolicy` to the `organization_admin` case list, after `ApplicationDeploy`:

```go
		case ApplicationAdopt, ApplicationRelease, SecretReveal, ApplicationRead, ApplicationImport, ApplicationEdit, ApplicationDestroy, ApplicationDeploy, ApplicationPolicy, ContainerExec, OrganizationRead, MembersManage, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, AuditRead, EndpointRead, EndpointEnroll, EndpointUpdate, EndpointRevoke, ContainerOperate, ContainerDestroy, ImagePull, ImageDestroy, ContainerLogs, RegistryRead, RegistryManage:
```

- [ ] **Step 5: Add migration 31**

In `internal/store/migrations/migrations.go`, append after the version 30 entry (inside `registry`):

```go
	// Update policies and their runs. outcome is empty while a run is in flight; UNIQUE (policy_id,
	// occurrence) makes one attempt per window across restarts and ticks.
	{Version: 31, Name: "update_policies", SQLite: `CREATE TABLE update_policies (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 application_id TEXT NOT NULL UNIQUE,
 created_by TEXT NOT NULL CHECK(length(created_by) BETWEEN 1 AND 255),
 mode TEXT NOT NULL CHECK(mode IN ('apply','plan_only')),
 timezone TEXT NOT NULL CHECK(length(timezone) BETWEEN 1 AND 64),
 weekdays TEXT NOT NULL CHECK(length(weekdays) BETWEEN 1 AND 13),
 start_minute INTEGER NOT NULL CHECK(start_minute BETWEEN 0 AND 1425),
 end_minute INTEGER NOT NULL CHECK(end_minute BETWEEN 15 AND 1440 AND end_minute-start_minute>=15),
 status TEXT NOT NULL CHECK(status IN ('active','paused')),
 paused_reason TEXT NOT NULL DEFAULT '' CHECK(length(paused_reason)<=255),
 consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK(consecutive_failures>=0),
 created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL,
 FOREIGN KEY(organization_id,environment_id,application_id) REFERENCES applications(organization_id,environment_id,id) ON DELETE CASCADE
);
CREATE INDEX idx_update_policies_status ON update_policies(status,created_at);
CREATE TABLE policy_runs (
 id TEXT PRIMARY KEY,
 policy_id TEXT NOT NULL REFERENCES update_policies(id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 application_id TEXT NOT NULL,
 occurrence DATETIME NOT NULL,
 started_at DATETIME NOT NULL,
 finished_at DATETIME,
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','skipped_missed','skipped_busy','no_update','planned','applied','blocked','failed','paused')),
 deployment_id TEXT,
 detail TEXT NOT NULL DEFAULT '' CHECK(length(detail)<=255),
 correlation_id TEXT NOT NULL CHECK(length(correlation_id) BETWEEN 1 AND 64),
 UNIQUE(policy_id,occurrence)
);
CREATE INDEX idx_policy_runs_open ON policy_runs(outcome) WHERE outcome='';
`, Postgres: `CREATE TABLE update_policies (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 application_id TEXT NOT NULL UNIQUE,
 created_by TEXT NOT NULL CHECK(length(created_by) BETWEEN 1 AND 255),
 mode TEXT NOT NULL CHECK(mode IN ('apply','plan_only')),
 timezone TEXT NOT NULL CHECK(length(timezone) BETWEEN 1 AND 64),
 weekdays TEXT NOT NULL CHECK(length(weekdays) BETWEEN 1 AND 13),
 start_minute INTEGER NOT NULL CHECK(start_minute BETWEEN 0 AND 1425),
 end_minute INTEGER NOT NULL CHECK(end_minute BETWEEN 15 AND 1440 AND end_minute-start_minute>=15),
 status TEXT NOT NULL CHECK(status IN ('active','paused')),
 paused_reason TEXT NOT NULL DEFAULT '' CHECK(length(paused_reason)<=255),
 consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK(consecutive_failures>=0),
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL,
 FOREIGN KEY(organization_id,environment_id,application_id) REFERENCES applications(organization_id,environment_id,id) ON DELETE CASCADE
);
CREATE INDEX idx_update_policies_status ON update_policies(status,created_at);
CREATE TABLE policy_runs (
 id TEXT PRIMARY KEY,
 policy_id TEXT NOT NULL REFERENCES update_policies(id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 application_id TEXT NOT NULL,
 occurrence TIMESTAMPTZ NOT NULL,
 started_at TIMESTAMPTZ NOT NULL,
 finished_at TIMESTAMPTZ,
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','skipped_missed','skipped_busy','no_update','planned','applied','blocked','failed','paused')),
 deployment_id TEXT,
 detail TEXT NOT NULL DEFAULT '' CHECK(length(detail)<=255),
 correlation_id TEXT NOT NULL CHECK(length(correlation_id) BETWEEN 1 AND 64),
 UNIQUE(policy_id,occurrence)
);
CREATE INDEX idx_policy_runs_open ON policy_runs(outcome) WHERE outcome='';
`},
```

- [ ] **Step 6: Implement the policy store**

Create `internal/store/policies.go`:

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// Update policies: one per application, saved by an organization administrator and run by the
// scheduler in internal/api as whoever saved it last. See docs/application-schema.md, Update
// policies.
const (
	PolicyModeApply    = "apply"
	PolicyModePlanOnly = "plan_only"
	PolicyActive       = "active"
	PolicyPaused       = "paused"
	// MinPolicyWindowMinutes is the shortest window. A window never crosses midnight.
	MinPolicyWindowMinutes = 15
	// PolicyFailureLimit consecutive failed windows pause a policy.
	PolicyFailureLimit = 3
	// MaxPolicyRuns bounds one page of runs; the policy read carries the latest policyRunsShown.
	MaxPolicyRuns   = 100
	policyRunsShown = 20
)

// A run's outcome. It is empty only while the run is in flight.
const (
	RunSkippedMissed = "skipped_missed"
	RunSkippedBusy   = "skipped_busy"
	RunNoUpdate      = "no_update"
	RunPlanned       = "planned"
	RunApplied       = "applied"
	RunBlocked       = "blocked"
	RunFailed        = "failed"
	RunPaused        = "paused"
)

// The scheduler's own sentences: the only prose a run's detail or a pause reason holds.
const (
	PolicyDetailMissed      = "the server was not running during this window"
	PolicyDetailBusy        = "another deployment occupied the window"
	PolicyDetailCreatorLost = "the policy's creator no longer holds application.deploy"
	PolicyDetailRestarted   = "the server restarted during the run"
	PolicyReasonFailures    = "three consecutive windows failed"
)

var (
	ErrInvalidTimezone = errors.New("invalid update policy time zone")
	ErrInvalidWindow   = errors.New("invalid update policy window")
	ErrInvalidWeekdays = errors.New("invalid update policy weekdays")
	ErrInvalidMode     = errors.New("invalid update policy mode")
	ErrPolicyNotPaused = errors.New("update policy is not paused")
)

// PolicyInput is what an administrator saves: a mode and a weekly window in an IANA zone.
type PolicyInput struct {
	Mode        string `json:"mode"`
	Timezone    string `json:"timezone"`
	Weekdays    []int  `json:"weekdays"`
	StartMinute int    `json:"start_minute"`
	EndMinute   int    `json:"end_minute"`
}

type UpdatePolicy struct {
	ID                  string    `json:"id"`
	ApplicationID       string    `json:"application_id"`
	CreatedBy           string    `json:"created_by"`
	Mode                string    `json:"mode"`
	Timezone            string    `json:"timezone"`
	Weekdays            []int     `json:"weekdays"`
	StartMinute         int       `json:"start_minute"`
	EndMinute           int       `json:"end_minute"`
	Status              string    `json:"status"`
	PausedReason        string    `json:"paused_reason"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	// NextOccurrence is computed on read, UTC; nil while paused.
	NextOccurrence *time.Time `json:"next_occurrence"`
	OrganizationID string     `json:"-"`
	EnvironmentID  string     `json:"-"`
}

type PolicyRun struct {
	ID       string `json:"id"`
	PolicyID string `json:"policy_id"`
	// Occurrence is the window's start, UTC: the run's identity with PolicyID.
	Occurrence    time.Time  `json:"occurrence"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	Outcome       string     `json:"outcome"`
	DeploymentID  string     `json:"deployment_id"`
	Detail        string     `json:"detail"`
	CorrelationID string     `json:"correlation_id"`
}

// validPolicyZone admits an IANA zone the embedded database loads. "" and "Local" load too, but
// they name the server host's zone, not a place.
func validPolicyZone(name string) bool {
	if name == "" || name == "Local" || len(name) > 64 || !displaySafe(name) {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// weekdays validates in and returns its weekday set as stored: ascending, comma-separated.
func (in PolicyInput) weekdays() (string, error) {
	if in.Mode != PolicyModeApply && in.Mode != PolicyModePlanOnly {
		return "", ErrInvalidMode
	}
	if !validPolicyZone(in.Timezone) {
		return "", ErrInvalidTimezone
	}
	if len(in.Weekdays) == 0 || len(in.Weekdays) > 7 {
		return "", ErrInvalidWeekdays
	}
	days := slices.Sorted(slices.Values(in.Weekdays))
	parts := make([]string, len(days))
	for i, d := range days {
		if d < 0 || d > 6 || (i > 0 && days[i-1] == d) {
			return "", ErrInvalidWeekdays
		}
		parts[i] = strconv.Itoa(d)
	}
	// end-start >= 15 also puts start before end: no window crosses midnight.
	if in.StartMinute < 0 || in.EndMinute > 1440 || in.EndMinute-in.StartMinute < MinPolicyWindowMinutes {
		return "", ErrInvalidWindow
	}
	return strings.Join(parts, ","), nil
}

const policyColumns = `p.id,p.organization_id,p.environment_id,p.application_id,p.created_by,p.mode,p.timezone,p.weekdays,p.start_minute,p.end_minute,p.status,p.paused_reason,p.consecutive_failures,p.created_at,p.updated_at`

// scanPolicy reads policyColumns, then extra.
func scanPolicy(row interface{ Scan(...any) error }, extra ...any) (*UpdatePolicy, error) {
	var p UpdatePolicy
	var days string
	if err := row.Scan(append([]any{&p.ID, &p.OrganizationID, &p.EnvironmentID, &p.ApplicationID, &p.CreatedBy, &p.Mode, &p.Timezone, &days, &p.StartMinute, &p.EndMinute, &p.Status, &p.PausedReason, &p.ConsecutiveFailures, &p.CreatedAt, &p.UpdatedAt}, extra...)...); err != nil {
		return nil, err
	}
	p.Weekdays = []int{}
	for _, d := range strings.Split(days, ",") {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 || n > 6 {
			return nil, ErrRevisionCorrupt
		}
		p.Weekdays = append(p.Weekdays, n)
	}
	return &p, nil
}

func (t *tenancyStore) readPolicy(ctx context.Context, tx *sql.Tx, id string) (*UpdatePolicy, error) {
	return scanPolicy(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+policyColumns+` FROM update_policies p WHERE p.id=?`), id))
}

// policyOf returns the ID and status of app's policy in a's scope, locked on PostgreSQL;
// ErrNotFound when it has none.
func (t *tenancyStore) policyOf(ctx context.Context, tx *sql.Tx, a TenantAccess, app string) (string, string, error) {
	lock := ""
	if t.store.driver == "postgres" {
		lock = " FOR UPDATE"
	}
	var id, status string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,status FROM update_policies WHERE organization_id=? AND environment_id=? AND application_id=?`+lock), a.OrganizationID, a.EnvironmentID, app).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return id, status, err
}

const policyRunColumns = `id,policy_id,occurrence,started_at,finished_at,outcome,deployment_id,detail,correlation_id`

// policyRuns lists policy's runs newest window first.
func (t *tenancyStore) policyRuns(ctx context.Context, tx *sql.Tx, policy string, limit int) ([]PolicyRun, error) {
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT `+policyRunColumns+` FROM policy_runs WHERE policy_id=? ORDER BY occurrence DESC,id DESC LIMIT ?`), policy, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PolicyRun{}
	for rows.Next() {
		var r PolicyRun
		var finished sql.NullTime
		var deployment sql.NullString
		if err := rows.Scan(&r.ID, &r.PolicyID, &r.Occurrence, &r.StartedAt, &finished, &r.Outcome, &deployment, &r.Detail, &r.CorrelationID); err != nil {
			return nil, err
		}
		if finished.Valid {
			r.FinishedAt = &finished.Time
		}
		r.DeploymentID = deployment.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// PutUpdatePolicy creates app's policy or replaces its settings, and reports whether it created
// one. The saver becomes created_by: a policy always acts as its last editor. Status and the
// failure count are left alone; ResumeUpdatePolicy reopens a paused policy.
func (t *tenancyStore) PutUpdatePolicy(ctx context.Context, a TenantAccess, app string, in PolicyInput) (*UpdatePolicy, bool, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, false, ErrInvalid
	}
	target := appID.String() + "/policies"
	var out *UpdatePolicy
	created := false
	err = t.run(ctx, a, permissions.ApplicationPolicy, &target, nil, true, func(tx *sql.Tx) error {
		days, err := in.weekdays()
		if err != nil {
			return err
		}
		// The application row first, the lock order plans and releases take.
		if err := t.lockApplication(ctx, tx, a, appID.String()); err != nil {
			if errors.Is(err, ErrAdoptionChanged) {
				return ErrNotFound
			}
			return err
		}
		now := time.Now().UTC()
		id, _, err := t.policyOf(ctx, tx, a, appID.String())
		switch {
		case errors.Is(err, ErrNotFound):
			id, created = uuid.NewString(), true
			_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO update_policies(id,organization_id,environment_id,application_id,created_by,mode,timezone,weekdays,start_minute,end_minute,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`), id, a.OrganizationID, a.EnvironmentID, appID.String(), a.ActorID, in.Mode, in.Timezone, days, in.StartMinute, in.EndMinute, PolicyActive, now, now)
		case err == nil:
			_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET created_by=?,mode=?,timezone=?,weekdays=?,start_minute=?,end_minute=?,updated_at=? WHERE id=?`), a.ActorID, in.Mode, in.Timezone, days, in.StartMinute, in.EndMinute, now, id)
		}
		if err != nil {
			return err
		}
		target = appID.String() + "/policies/" + id
		out, err = t.readPolicy(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// ReadUpdatePolicy returns app's policy and its latest runs, or a nil policy when it has none:
// "no policy" is an answer, not a failure worth an audit row.
func (t *tenancyStore) ReadUpdatePolicy(ctx context.Context, a TenantAccess, app string) (*UpdatePolicy, []PolicyRun, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, nil, ErrInvalid
	}
	var out *UpdatePolicy
	runs := []PolicyRun{}
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		p, err := scanPolicy(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+policyColumns+` FROM update_policies p WHERE p.organization_id=? AND p.environment_id=? AND p.application_id=?`), a.OrganizationID, a.EnvironmentID, appID.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = p
		runs, err = t.policyRuns(ctx, tx, p.ID, policyRunsShown)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return out, runs, nil
}

// DeleteUpdatePolicy deletes app's policy and, by cascade, its runs. Deployments it made stay.
func (t *tenancyStore) DeleteUpdatePolicy(ctx context.Context, a TenantAccess, app string) error {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return ErrInvalid
	}
	target := appID.String() + "/policies"
	return t.run(ctx, a, permissions.ApplicationPolicy, &target, nil, true, func(tx *sql.Tx) error {
		id, _, err := t.policyOf(ctx, tx, a, appID.String())
		if err != nil {
			return err
		}
		target = appID.String() + "/policies/" + id
		_, err = tx.ExecContext(ctx, t.store.rebind(`DELETE FROM update_policies WHERE id=?`), id)
		return err
	})
}

// ResumeUpdatePolicy reopens a paused policy with its failure count at zero. It does not change
// who the policy acts as: a policy paused because its creator lost authority pauses again until
// someone who may deploy saves it.
func (t *tenancyStore) ResumeUpdatePolicy(ctx context.Context, a TenantAccess, app string) (*UpdatePolicy, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	target := appID.String() + "/policies"
	var out *UpdatePolicy
	err = t.run(ctx, a, permissions.ApplicationPolicy, &target, nil, true, func(tx *sql.Tx) error {
		id, status, err := t.policyOf(ctx, tx, a, appID.String())
		if err != nil {
			return err
		}
		target = appID.String() + "/policies/" + id + "/resume"
		if status != PolicyPaused {
			return ErrPolicyNotPaused
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET status=?,paused_reason='',consecutive_failures=0,updated_at=? WHERE id=?`), PolicyActive, time.Now().UTC(), id); err != nil {
			return err
		}
		out, err = t.readPolicy(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPolicyRuns returns up to limit (1..MaxPolicyRuns) of app's runs, newest window first; none
// when app has no policy.
func (t *tenancyStore) ListPolicyRuns(ctx context.Context, a TenantAccess, app string, limit int) ([]PolicyRun, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	out := []PolicyRun{}
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		if limit < 1 || limit > MaxPolicyRuns {
			return ErrInvalid
		}
		var id string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id FROM update_policies WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, appID.String()).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out, err = t.policyRuns(ctx, tx, id, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
```

In `internal/store/store.go`, add to `TenancyStore` directly after the `CheckImageUpdates` method:

```go
	// Update policies (docs/application-schema.md, Update policies): writes under
	// application.policy, reads under application.read. ReadUpdatePolicy returns a nil policy
	// when the application has none.
	PutUpdatePolicy(ctx context.Context, access TenantAccess, applicationID string, input PolicyInput) (*UpdatePolicy, bool, error)
	ReadUpdatePolicy(ctx context.Context, access TenantAccess, applicationID string) (*UpdatePolicy, []PolicyRun, error)
	DeleteUpdatePolicy(ctx context.Context, access TenantAccess, applicationID string) error
	ResumeUpdatePolicy(ctx context.Context, access TenantAccess, applicationID string) (*UpdatePolicy, error)
	ListPolicyRuns(ctx context.Context, access TenantAccess, applicationID string, limit int) ([]PolicyRun, error)
```

- [ ] **Step 7: Run the tests on both drivers**

Run: `gofmt -w internal/store internal/permissions && go vet ./internal/store/ ./internal/permissions/ && go test -race -count=1 ./internal/permissions/ ./internal/store/... && PG=… go test -count=1 ./internal/store/...`
Expected: PASS on SQLite and PostgreSQL (the migration test `TestLatestIsTheLastRegisteredVersion` now sees 31; `internal/backup` drill tests read `migrations.Latest()` and need no edit).

- [ ] **Step 8: DOX and commit**

`internal/permissions/AGENTS.md`, Local Contracts: after the `container.exec` bullet add "- `application.policy` (create, edit, delete, resume an application's update policy) is organization-administrator-only: a policy deploys unattended as whoever saved it last. Reading a policy and its runs is `application.read`."

`internal/store/AGENTS.md`, Local Contracts: append "- Update policies (`policies.go`, migration 31): one `update_policies` row per application (composite FK to `applications`, cascade), `policy_runs` cascade from the policy with `UNIQUE (policy_id, occurrence)` and `outcome` `''` while in flight. `PutUpdatePolicy` validates mode, zone (`time.LoadLocation`; `''` and `Local` refused), weekday set (0–6, distinct) and window (`0 ≤ start < end ≤ 1440`, at least 15 minutes, never across midnight) with one error per field (`ErrInvalidMode`, `ErrInvalidTimezone`, `ErrInvalidWeekdays`, `ErrInvalidWindow`) and sets `created_by` to the saver; writes run under `application.policy` with resource `<app>/policies/<policy>`, reads under `application.read`; `ReadUpdatePolicy` returns a nil policy for none; `ResumeUpdatePolicy` refuses an active policy (`ErrPolicyNotPaused`) and zeroes the count."

```bash
git add internal/permissions internal/store
git commit -m "feat(store): update policies and their runs (migration 31)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Store — occurrences, the schedule, run bookkeeping and reconciliation

**Files:**
- Create: `internal/store/policy_schedule.go`
- Modify: `internal/store/policies.go` (`NextOccurrence` on read, save and resume)
- Modify: `internal/store/reconcile.go` (fail runs a crash left open)
- Modify: `internal/store/store.go` (`TenancyStore` gains four scheduler methods; the `ReconcileAfterStart` comment)
- Test: `internal/store/policy_schedule_test.go`
- Docs: `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: Task 1's types, constants, `scanPolicy`, `policyColumns`; `planRequest`, `imageCheckKey` (tests).
- Produces:
  ```go
  const AuditPolicyRun = "application.policy.run"
  const AuditPolicyPaused = "application.policy.paused"
  // ScheduledPolicy is an active policy as the scheduler sees it at one instant.
  type ScheduledPolicy struct {
  	Policy       UpdatePolicy
  	EndpointID   string    // the adopted instance's endpoint; "" when not adopted
  	EndpointBusy bool      // a live plan or an apply on that endpoint (wall clock)
  	Open         bool      // Occurrence is open now and has no run row
  	Occurrence   time.Time // UTC
  	Missed       bool      // Previous ended after the policy's last change and has no run row
  	Previous     time.Time // UTC
  }
  SchedulePolicies(ctx context.Context, now time.Time) ([]ScheduledPolicy, error) // active only, created_at order, Open or Missed only
  BeginPolicyRun(ctx context.Context, policyID string, occurrence time.Time, correlation string) (string, error) // run ID; ErrNotFound (gone or paused), ErrAlreadyExists (window taken)
  FinishPolicyRun(ctx context.Context, runID, outcome, deploymentID, detail string) error // ErrInvalid outcome, ErrNotFound finished or gone
  SkipPolicyWindow(ctx context.Context, policyID string, occurrence time.Time, outcome, detail string) error // skipped_* only
  func policyOccurrence(p UpdatePolicy, now time.Time) (start, end time.Time, open bool)
  func previousOccurrence(p UpdatePolicy, now time.Time) (start, end time.Time, ok bool)
  func nextOccurrence(p UpdatePolicy, now time.Time) (time.Time, bool)
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/store/policy_schedule_test.go`:

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var everyDay = []int{0, 1, 2, 3, 4, 5, 6}

func windowPolicy(zone string, days []int, start, end int) UpdatePolicy {
	return UpdatePolicy{Timezone: zone, Weekdays: days, StartMinute: start, EndMinute: end, Status: PolicyActive}
}

// backdatePolicy moves the policy's last change, which bounds what the scheduler calls missed.
func backdatePolicy(t *testing.T, st *SQLStore, id string, when time.Time) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE update_policies SET updated_at=? WHERE id=?`), when, id); err != nil {
		t.Fatal(err)
	}
}

type policyAuditRow struct{ Action, Resource, Details, Result, User, Correlation string }

func policyAudits(t *testing.T, st *SQLStore) []policyAuditRow {
	t.Helper()
	rows, err := st.db.QueryContext(context.Background(), `SELECT action,resource,details,result,user_id,correlation_id FROM audit_records WHERE action IN ('application.policy.run','application.policy.paused') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []policyAuditRow
	for rows.Next() {
		var r policyAuditRow
		if err := rows.Scan(&r.Action, &r.Resource, &r.Details, &r.Result, &r.User, &r.Correlation); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// 2026-03-08 02:30 does not exist in New York (02:00 EST jumps to 03:00 EDT): that day has no
// occurrence, and the days either side do.
func TestPolicyOccurrenceAcrossADSTGap(t *testing.T) {
	p := windowPolicy("America/New_York", everyDay, 150, 240)
	if _, _, open := policyOccurrence(p, instant("2026-03-08T07:45:00Z")); open { // 03:45 EDT
		t.Fatal("an occurrence opened on a day its start does not exist")
	}
	start, end, ok := previousOccurrence(p, instant("2026-03-08T09:00:00Z"))
	if !ok || !start.Equal(instant("2026-03-07T07:30:00Z")) || !end.Equal(instant("2026-03-07T09:00:00Z")) {
		t.Fatalf("previous across the gap: %s %s %v", start, end, ok)
	}
	next, ok := nextOccurrence(p, instant("2026-03-08T05:00:00Z"))
	if !ok || !next.Equal(instant("2026-03-09T06:30:00Z")) {
		t.Fatalf("next across the gap: %s %v", next, ok)
	}
	start, end, open := policyOccurrence(p, instant("2026-03-09T07:00:00Z")) // 03:00 EDT
	if !open || !start.Equal(instant("2026-03-09T06:30:00Z")) || !end.Equal(instant("2026-03-09T08:00:00Z")) || start.Location() != time.UTC {
		t.Fatalf("the day after: %s %s %v", start, end, open)
	}
}

// 2026-11-01 01:30 happens twice in New York: the window opens once, at the first (EDT), and
// stays the same occurrence through the repeated hour.
func TestPolicyOccurrenceInADSTOverlap(t *testing.T) {
	p := windowPolicy("America/New_York", everyDay, 90, 120)
	for _, now := range []string{"2026-11-01T05:45:00Z", "2026-11-01T06:45:00Z"} { // 01:45 EDT, then 01:45 EST
		start, end, open := policyOccurrence(p, instant(now))
		if !open || !start.Equal(instant("2026-11-01T05:30:00Z")) || !end.Equal(instant("2026-11-01T07:00:00Z")) {
			t.Fatalf("%s: %s %s %v", now, start, end, open)
		}
	}
}

// Kiritimati is UTC+14: Thursday 12:00 UTC is Friday 02:00 there, so the weekday is the zone's.
func TestPolicyOccurrenceEastOfUTC(t *testing.T) {
	now := instant("2026-09-24T12:00:00Z")
	start, _, open := policyOccurrence(windowPolicy("Pacific/Kiritimati", []int{5}, 60, 180), now)
	if !open || !start.Equal(instant("2026-09-24T11:00:00Z")) {
		t.Fatalf("Friday in Kiritimati: %s %v", start, open)
	}
	if _, _, open := policyOccurrence(windowPolicy("Pacific/Kiritimati", []int{4}, 60, 180), now); open {
		t.Fatal("the UTC weekday opened the window")
	}
}

// A run starting at 23:59 in a window ending at midnight belongs to that day's occurrence; at
// 00:00 nothing is open and, with its run recorded, nothing is missed (Review Focus 4).
func TestPolicyWindowEndingAtMidnight(t *testing.T) {
	p := windowPolicy("UTC", []int{4}, 1380, 1440)
	start, end, open := policyOccurrence(p, instant("2026-09-24T23:59:00Z"))
	if !open || !start.Equal(instant("2026-09-24T23:00:00Z")) || !end.Equal(instant("2026-09-25T00:00:00Z")) {
		t.Fatalf("23:59: %s %s %v", start, end, open)
	}
	if _, _, open := policyOccurrence(p, instant("2026-09-25T00:00:00Z")); open {
		t.Fatal("open at midnight")
	}
	st, _, _, _, _, stored := policyFixture(t, PolicyInput{Mode: PolicyModeApply, Timezone: "UTC", Weekdays: []int{4}, StartMinute: 1380, EndMinute: 1440})
	backdatePolicy(t, st, stored.ID, instant("2026-09-01T00:00:00Z"))
	ctx := context.Background()
	ts := st.Tenancy()
	due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T23:59:00Z"))
	if err != nil || len(due) != 1 || !due[0].Open || !due[0].Occurrence.Equal(instant("2026-09-24T23:00:00Z")) {
		t.Fatalf("at 23:59: %+v %v", due, err)
	}
	run, err := ts.BeginPolicyRun(ctx, stored.ID, due[0].Occurrence, "late")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.FinishPolicyRun(ctx, run, RunApplied, "", ""); err != nil {
		t.Fatal(err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-25T00:00:30Z")); err != nil || len(due) != 0 {
		t.Fatalf("after midnight: %+v %v", due, err)
	}
}

func TestNextOccurrenceOnRead(t *testing.T) {
	st, a, app, _, _, _ := policyFixture(t, PolicyInput{Mode: PolicyModeApply, Timezone: "Europe/Paris", Weekdays: []int{1}, StartMinute: 120, EndMinute: 240})
	ctx := context.Background()
	p, _, err := st.Tenancy().ReadUpdatePolicy(ctx, a, app.ID)
	if err != nil || p.NextOccurrence == nil || !p.NextOccurrence.After(time.Now()) || p.NextOccurrence.Location() != time.UTC || p.NextOccurrence.In(mustZone(t, "Europe/Paris")).Weekday() != time.Monday {
		t.Fatalf("next: %+v %v", p, err)
	}
	next, ok := nextOccurrence(windowPolicy("UTC", []int{1}, 600, 660), instant("2026-09-21T10:30:00Z")) // Monday, open
	if !ok || !next.Equal(instant("2026-09-28T10:00:00Z")) {
		t.Fatalf("an open window is not next: %s", next)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE update_policies SET status='paused' WHERE id=?`), p.ID); err != nil {
		t.Fatal(err)
	}
	if p, _, err := st.Tenancy().ReadUpdatePolicy(ctx, a, app.ID); err != nil || p.NextOccurrence != nil {
		t.Fatalf("a paused policy has a next window: %+v %v", p, err)
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestSchedulePolicies(t *testing.T) {
	st, a, app, endpoint, m, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	backdatePolicy(t, st, p.ID, instant("2026-09-01T00:00:00Z"))
	// A second application nobody adopted, saved later: it comes second and has no endpoint.
	draft := policyDraft(t, st, a, "draft")
	second, _, err := ts.PutUpdatePolicy(ctx, a, draft.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	backdatePolicy(t, st, second.ID, instant("2026-09-02T00:00:00Z"))
	due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T10:30:00Z"))
	if err != nil || len(due) != 2 || due[0].Policy.ID != p.ID || due[1].Policy.ID != second.ID || due[1].EndpointID != "" {
		t.Fatalf("order: %+v %v", due, err)
	}
	first := due[0]
	if !first.Open || !first.Occurrence.Equal(instant("2026-09-24T10:00:00Z")) || first.EndpointID != endpoint || first.EndpointBusy || !first.Missed || !first.Previous.Equal(instant("2026-09-23T10:00:00Z")) || first.Policy.ApplicationID != app.ID || first.Policy.CreatedBy != "actor" {
		t.Fatalf("first: %+v", first)
	}
	if err := ts.DeleteUpdatePolicy(ctx, a, draft.ID); err != nil {
		t.Fatal(err)
	}
	// A live plan makes the endpoint busy.
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T10:30:00Z")); err != nil || len(due) != 1 || !due[0].EndpointBusy {
		t.Fatalf("with a live plan: %+v %v", due, err)
	}
	// An expired plan does not: nothing ever moves it out of planned.
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE deployments SET expires_at=?`), time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T10:30:00Z")); err != nil || len(due) != 1 || due[0].EndpointBusy {
		t.Fatalf("with an expired plan: %+v %v", due, err)
	}
	// A run row closes the occurrence; a second one for it is refused.
	if _, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), "second"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("a second run for one window: %v", err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T10:30:00Z")); err != nil || len(due) != 1 || due[0].Open || !due[0].Missed {
		t.Fatalf("after the run started: %+v %v", due, err)
	}
	if err := ts.SkipPolicyWindow(ctx, p.ID, instant("2026-09-23T10:00:00Z"), RunSkippedMissed, PolicyDetailMissed); err != nil {
		t.Fatal(err)
	}
	if err := ts.SkipPolicyWindow(ctx, p.ID, instant("2026-09-23T10:00:00Z"), RunSkippedMissed, PolicyDetailMissed); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("a window skipped twice: %v", err)
	}
	if err := ts.SkipPolicyWindow(ctx, p.ID, instant("2026-09-22T10:00:00Z"), RunFailed, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("skip with a run outcome: %v", err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T10:30:00Z")); err != nil || len(due) != 0 {
		t.Fatalf("nothing left: %+v %v", due, err)
	}
	// A window that ended before the policy's last change is not missed.
	backdatePolicy(t, st, p.ID, instant("2026-09-25T12:00:00Z"))
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-26T09:00:00Z")); err != nil || len(due) != 0 {
		t.Fatalf("a window before the last change: %+v %v", due, err)
	}
	// A paused policy is not scheduled at all.
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE update_policies SET status='paused' WHERE id=?`), p.ID); err != nil {
		t.Fatal(err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-27T10:30:00Z")); err != nil || len(due) != 0 {
		t.Fatalf("a paused policy: %+v %v", due, err)
	}
}

func TestPolicyRunsCountFailuresAndPause(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	var runs []string
	finish := func(day int, outcome, detail string) {
		t.Helper()
		id, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-10T10:00:00Z").AddDate(0, 0, day), fmt.Sprintf("corr-%d", day))
		if err != nil {
			t.Fatal(err)
		}
		if err := ts.FinishPolicyRun(ctx, id, outcome, "", detail); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, id)
	}
	state := func() *UpdatePolicy {
		t.Helper()
		got, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	finish(0, RunFailed, "unauthorized")
	finish(1, RunBlocked, "configuration_unsupported")
	if got := state(); got.ConsecutiveFailures != 2 || got.Status != PolicyActive {
		t.Fatalf("two failures: %+v", got)
	}
	finish(2, RunSkippedBusy, PolicyDetailBusy) // a skip leaves the count
	finish(3, RunSkippedMissed, PolicyDetailMissed)
	if got := state(); got.ConsecutiveFailures != 2 {
		t.Fatalf("after skips: %+v", got)
	}
	finish(4, RunNoUpdate, "") // a success resets it
	if got := state(); got.ConsecutiveFailures != 0 {
		t.Fatalf("after no_update: %+v", got)
	}
	for day := 5; day < 8; day++ {
		finish(day, RunFailed, "unavailable")
	}
	if got := state(); got.ConsecutiveFailures != 3 || got.Status != PolicyPaused || got.PausedReason != PolicyReasonFailures {
		t.Fatalf("three failures: %+v", got)
	}
	if _, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-18T10:00:00Z"), "corr-8"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a paused policy opened a run: %v", err)
	}
	rows := policyAudits(t, st)
	if len(rows) != 9 {
		t.Fatalf("audit rows: %+v", rows)
	}
	for i, r := range rows[:8] {
		if r.Action != AuditPolicyRun || r.User != "system" || r.Resource != app.ID+"/policies/"+p.ID+"/runs/"+runs[i] || r.Correlation != fmt.Sprintf("corr-%d", i) {
			t.Fatalf("run row %d: %+v", i, r)
		}
	}
	if rows[0].Result != "failure" || rows[0].Details != RunFailed || rows[1].Result != "failure" || rows[2].Result != "success" || rows[2].Details != RunSkippedBusy || rows[4].Result != "success" || rows[4].Details != RunNoUpdate {
		t.Fatalf("run results: %+v", rows)
	}
	if last := rows[8]; last.Action != AuditPolicyPaused || last.Result != "failure" || last.User != "system" || last.Details != PolicyReasonFailures || last.Correlation != "corr-7" || last.Resource != app.ID+"/policies/"+p.ID {
		t.Fatalf("pause row: %+v", last)
	}
	resumed, err := ts.ResumeUpdatePolicy(ctx, a, app.ID)
	if err != nil || resumed.Status != PolicyActive || resumed.ConsecutiveFailures != 0 || resumed.PausedReason != "" {
		t.Fatalf("resumed: %+v %v", resumed, err)
	}
	// A run is finished once; the outcome vocabulary is closed.
	if err := ts.FinishPolicyRun(ctx, runs[0], RunApplied, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished twice: %v", err)
	}
	open, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-19T10:00:00Z"), "corr-9")
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"", "exploded"} {
		if err := ts.FinishPolicyRun(ctx, open, outcome, "", ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("outcome %q: %v", outcome, err)
		}
	}
}

func TestPolicyRunPausesWhenItsCreatorLostAccess(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	run, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), "lost")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.FinishPolicyRun(ctx, run, RunPaused, "", PolicyDetailCreatorLost); err != nil {
		t.Fatal(err)
	}
	got, runs, err := ts.ReadUpdatePolicy(ctx, a, app.ID)
	if err != nil || got.Status != PolicyPaused || got.PausedReason != PolicyDetailCreatorLost || got.ConsecutiveFailures != 0 || len(runs) != 1 || runs[0].Outcome != RunPaused || runs[0].Detail != PolicyDetailCreatorLost {
		t.Fatalf("paused: %+v %+v %v", got, runs, err)
	}
	rows := policyAudits(t, st)
	if len(rows) != 2 || rows[0].Result != "denied" || rows[0].Details != RunPaused || rows[1].Action != AuditPolicyPaused || rows[1].Result != "denied" || rows[1].Details != PolicyDetailCreatorLost || rows[1].Correlation != "lost" {
		t.Fatalf("audit: %+v", rows)
	}
}

// A run a crash left open fails at the next start and counts; a second start changes nothing.
func TestReconcileAfterStartFailsInterruptedPolicyRuns(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	done, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-23T10:00:00Z"), "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.FinishPolicyRun(ctx, done, RunNoUpdate, "", ""); err != nil {
		t.Fatal(err)
	}
	open, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), "interrupted")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if n, err := ts.ReconcileAfterStart(ctx); err != nil || n != 0 {
			t.Fatalf("reconcile: %d %v", n, err)
		}
	}
	got, runs, err := ts.ReadUpdatePolicy(ctx, a, app.ID)
	if err != nil || got.ConsecutiveFailures != 1 || len(runs) != 2 {
		t.Fatalf("after reconcile: %+v %+v %v", got, runs, err)
	}
	if r := runs[0]; r.ID != open || r.Outcome != RunFailed || r.Detail != PolicyDetailRestarted || r.FinishedAt == nil {
		t.Fatalf("interrupted run: %+v", r)
	}
	if r := runs[1]; r.ID != done || r.Outcome != RunNoUpdate {
		t.Fatalf("finished run changed: %+v", r)
	}
	rows := policyAudits(t, st)
	if len(rows) != 2 || rows[1].Correlation != "interrupted" || rows[1].Result != "failure" || !strings.HasSuffix(rows[1].Resource, "/runs/"+open) {
		t.Fatalf("audit: %+v", rows)
	}
}

// A policy deleted (here with its application) between the tick and the run opens nothing and
// leaves nothing behind (Review Focus 1).
func TestBeginPolicyRunRefusesAGonePolicy(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app := policyDraft(t, st, a, "draft")
	p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.DiscardApplication(ctx, a, app.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("begin: %v", err)
	}
	if err := ts.SkipPolicyWindow(ctx, p.ID, instant("2026-09-23T10:00:00Z"), RunSkippedMissed, PolicyDetailMissed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("skip: %v", err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM policy_runs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("runs: %d %v", n, err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/store/ -run 'PolicyOccurrence|PolicyWindow|NextOccurrence|SchedulePolicies|PolicyRun|InterruptedPolicyRuns|GonePolicy'`
Expected: FAIL to compile with `undefined: policyOccurrence` and `ts.SchedulePolicies undefined`.

- [ ] **Step 3: Implement occurrences, the schedule and run bookkeeping**

Create `internal/store/policy_schedule.go`:

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

// The scheduler's audit actions. They are not permissions: the scheduler writes them as
// "system", and each run's own steps are audited under the policy's creator.
const (
	AuditPolicyRun    = "application.policy.run"
	AuditPolicyPaused = "application.policy.paused"
)

// policyResults maps a run outcome to the audit result; its keys are the closed vocabulary a
// finished run may carry.
var policyResults = map[string]string{
	RunSkippedMissed: "success", RunSkippedBusy: "success", RunNoUpdate: "success", RunPlanned: "success", RunApplied: "success",
	RunBlocked: "failure", RunFailed: "failure", RunPaused: "denied",
}

// occurrenceOn is p's window on calendar day y-m-d in loc (d may overflow; time.Date normalises
// it). ok is false when the zone skips the window's start that day (a DST gap: the built wall
// clock differs from the one asked for) or the day's weekday is not in the set. An overlap
// resolves to Go's first mapping, so it is one occurrence.
func occurrenceOn(p UpdatePolicy, loc *time.Location, y int, m time.Month, d int) (start, end time.Time, ok bool) {
	start = time.Date(y, m, d, p.StartMinute/60, p.StartMinute%60, 0, 0, loc)
	if start.Hour()*60+start.Minute() != p.StartMinute || !slices.Contains(p.Weekdays, int(start.Weekday())) {
		return time.Time{}, time.Time{}, false
	}
	return start, time.Date(y, m, d, p.EndMinute/60, p.EndMinute%60, 0, 0, loc), true
}

// policyOccurrence is p's occurrence on now's calendar date in p's zone, in UTC, and whether it
// is open: start ≤ now < end. Windows never cross midnight, so no other day's can be open. A
// zone that no longer loads has no occurrences.
func policyOccurrence(p UpdatePolicy, now time.Time) (start, end time.Time, open bool) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	y, m, d := now.In(loc).Date()
	start, end, ok := occurrenceOn(p, loc, y, m, d)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	return start.UTC(), end.UTC(), !now.Before(start) && now.Before(end)
}

// policyHorizon is how many days either side of now the scheduler looks for an occurrence: two
// weeks covers a single weekday whose nearest occurrence fell in a DST gap.
const policyHorizon = 14

// previousOccurrence is p's most recent occurrence that has ended by now, in UTC.
func previousOccurrence(p UpdatePolicy, now time.Time) (start, end time.Time, ok bool) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	y, m, d := now.In(loc).Date()
	for i := 0; i <= policyHorizon; i++ {
		if s, e, ok := occurrenceOn(p, loc, y, m, d-i); ok && !e.After(now) {
			return s.UTC(), e.UTC(), true
		}
	}
	return time.Time{}, time.Time{}, false
}

// nextOccurrence is the start of p's first occurrence after now, in UTC.
func nextOccurrence(p UpdatePolicy, now time.Time) (time.Time, bool) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.Time{}, false
	}
	y, m, d := now.In(loc).Date()
	for i := 0; i <= policyHorizon; i++ {
		if s, _, ok := occurrenceOn(p, loc, y, m, d+i); ok && s.After(now) {
			return s.UTC(), true
		}
	}
	return time.Time{}, false
}

// nextOccurrenceOf is what the API shows as next_occurrence: nil while p is paused.
func nextOccurrenceOf(p UpdatePolicy, now time.Time) *time.Time {
	if p.Status != PolicyActive {
		return nil
	}
	next, ok := nextOccurrence(p, now)
	if !ok {
		return nil
	}
	return &next
}

// ScheduledPolicy is an active policy as the scheduler sees it at one instant.
type ScheduledPolicy struct {
	Policy UpdatePolicy
	// EndpointID is the adopted instance's endpoint; "" when the application is not adopted.
	EndpointID string
	// EndpointBusy: a deployment on that endpoint is applying, or planned and unexpired.
	EndpointBusy bool
	// Open: Occurrence is open now and has no run row.
	Open       bool
	Occurrence time.Time
	// Missed: Previous ended after the policy's last change and has no run row.
	Missed   bool
	Previous time.Time
}

// SchedulePolicies lists the active policies with an open window or a missed one at now, in
// created_at order. Occurrences use now; plan expiry uses the wall clock, since a deployment's
// expiry is a fact of this server's time, not the tick's.
func (t *tenancyStore) SchedulePolicies(ctx context.Context, now time.Time) ([]ScheduledPolicy, error) {
	rows, err := t.store.db.QueryContext(ctx, `SELECT `+policyColumns+`,COALESCE(i.endpoint_id,'') FROM update_policies p LEFT JOIN application_instances i ON i.application_id=p.application_id WHERE p.status='active' ORDER BY p.created_at,p.id`)
	if err != nil {
		return nil, err
	}
	var active []ScheduledPolicy
	for rows.Next() {
		var endpoint string
		p, err := scanPolicy(rows, &endpoint)
		if err != nil {
			rows.Close()
			return nil, err
		}
		active = append(active, ScheduledPolicy{Policy: *p, EndpointID: endpoint})
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	wall := time.Now().UTC()
	out := []ScheduledPolicy{}
	for _, sp := range active {
		if start, _, open := policyOccurrence(sp.Policy, now); open {
			ran, err := t.policyRan(ctx, sp.Policy.ID, start)
			if err != nil {
				return nil, err
			}
			if !ran {
				sp.Open, sp.Occurrence = true, start
			}
		}
		if start, end, ok := previousOccurrence(sp.Policy, now); ok && end.After(sp.Policy.UpdatedAt) {
			ran, err := t.policyRan(ctx, sp.Policy.ID, start)
			if err != nil {
				return nil, err
			}
			if !ran {
				sp.Missed, sp.Previous = true, start
			}
		}
		if sp.Open && sp.EndpointID != "" {
			var live int
			if err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM deployments WHERE endpoint_id=? AND (state='applying' OR (state='planned' AND expires_at>?))`), sp.EndpointID, wall).Scan(&live); err != nil {
				return nil, err
			}
			sp.EndpointBusy = live > 0
		}
		if sp.Open || sp.Missed {
			out = append(out, sp)
		}
	}
	return out, nil
}

func (t *tenancyStore) policyRan(ctx context.Context, policy string, occurrence time.Time) (bool, error) {
	var n int
	err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM policy_runs WHERE policy_id=? AND occurrence=?`), policy, occurrence.UTC()).Scan(&n)
	return n > 0, err
}

// insertPolicyRun opens the run row for policy's occurrence. The policy must still exist and be
// active (ErrNotFound); a second row for one occurrence is ErrAlreadyExists.
func (t *tenancyStore) insertPolicyRun(ctx context.Context, tx *sql.Tx, policy string, occurrence time.Time, correlation string, now time.Time) (string, error) {
	lock := ""
	if t.store.driver == "postgres" {
		lock = " FOR UPDATE"
	}
	var org, env, app string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT organization_id,environment_id,application_id FROM update_policies WHERE id=? AND status='active'`+lock), policy).Scan(&org, &env, &app)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO policy_runs(id,policy_id,organization_id,environment_id,application_id,occurrence,started_at,correlation_id) VALUES(?,?,?,?,?,?,?,?)`), id, policy, org, env, app, occurrence.UTC(), now, correlation)
	if err != nil && (strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key")) {
		return "", ErrAlreadyExists
	}
	return id, err
}

// BeginPolicyRun records that a run for policy's occurrence started, before any work: the row is
// what makes a window run once across ticks and restarts.
func (t *tenancyStore) BeginPolicyRun(ctx context.Context, policy string, occurrence time.Time, correlation string) (string, error) {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := t.insertPolicyRun(ctx, tx, policy, occurrence, correlation, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// FinishPolicyRun records a run's outcome, counts it against the policy and audits it.
func (t *tenancyStore) FinishPolicyRun(ctx context.Context, run, outcome, deployment, detail string) error {
	if _, ok := policyResults[outcome]; !ok {
		return ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := t.finishPolicyRun(ctx, tx, run, outcome, deployment, detail, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// SkipPolicyWindow records a window the scheduler did not run, as one finished row.
func (t *tenancyStore) SkipPolicyWindow(ctx context.Context, policy string, occurrence time.Time, outcome, detail string) error {
	if outcome != RunSkippedMissed && outcome != RunSkippedBusy {
		return ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	id, err := t.insertPolicyRun(ctx, tx, policy, occurrence, uuid.NewString(), now)
	if err != nil {
		return err
	}
	if err := t.finishPolicyRun(ctx, tx, id, outcome, "", detail, now); err != nil {
		return err
	}
	return tx.Commit()
}

// finishPolicyRun settles an open run: blocked and failed count, applied, planned and no_update
// reset the count, skips leave it; PolicyFailureLimit failures or a paused outcome pause the
// policy. One audit row for the run and one for a pause, under the run's correlation ID.
func (t *tenancyStore) finishPolicyRun(ctx context.Context, tx *sql.Tx, run, outcome, deployment, detail string, now time.Time) error {
	var policy, org, env, app, correlation string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT policy_id,organization_id,environment_id,application_id,correlation_id FROM policy_runs WHERE id=? AND outcome=''`), run).Scan(&policy, &org, &env, &app, &correlation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var dep any
	if deployment != "" {
		dep = deployment
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE policy_runs SET outcome=?,deployment_id=?,detail=?,finished_at=? WHERE id=?`), outcome, dep, protocol.CleanText(detail, 255), now, run); err != nil {
		return err
	}
	pause := ""
	switch outcome {
	case RunBlocked, RunFailed:
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET consecutive_failures=consecutive_failures+1 WHERE id=?`), policy); err != nil {
			return err
		}
		var failures int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT consecutive_failures FROM update_policies WHERE id=?`), policy).Scan(&failures); err != nil {
			return err
		}
		if failures >= PolicyFailureLimit {
			pause = PolicyReasonFailures
		}
	case RunApplied, RunPlanned, RunNoUpdate:
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET consecutive_failures=0 WHERE id=?`), policy); err != nil {
			return err
		}
	case RunPaused:
		pause = PolicyDetailCreatorLost
	}
	resource := app + "/policies/" + policy
	if err := t.auditPolicy(ctx, tx, org, env, resource+"/runs/"+run, AuditPolicyRun, outcome, policyResults[outcome], correlation, now); err != nil {
		return err
	}
	if pause == "" {
		return nil
	}
	res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET status=?,paused_reason=?,updated_at=? WHERE id=? AND status=?`), PolicyPaused, pause, now, policy, PolicyActive)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return err
	}
	result := "failure"
	if outcome == RunPaused {
		result = "denied"
	}
	return t.auditPolicy(ctx, tx, org, env, resource, AuditPolicyPaused, pause, result, correlation, now)
}

// auditPolicy writes one scheduler row as "system" inside tx.
func (t *tenancyStore) auditPolicy(ctx context.Context, tx *sql.Tx, org, env, resource, action, details, result, correlation string, at time.Time) error {
	_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), "system", action, protocol.CleanText(resource, 255), protocol.CleanText(details, 255), at, "organization", org, env, correlation, result)
	return err
}

// reconcilePolicyRuns fails every run the previous process left open, counting each.
func (t *tenancyStore) reconcilePolicyRuns(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM policy_runs WHERE outcome='' ORDER BY started_at,id`)
	if err != nil {
		return err
	}
	var open []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		open = append(open, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, id := range open {
		if err := t.finishPolicyRun(ctx, tx, id, RunFailed, "", PolicyDetailRestarted, now); err != nil {
			return err
		}
	}
	return nil
}
```

In `internal/store/policies.go` fill `NextOccurrence` on every policy returned:
- in `PutUpdatePolicy`, replace the final `return out, created, nil` with:
  ```go
  	out.NextOccurrence = nextOccurrenceOf(*out, time.Now())
  	return out, created, nil
  ```
- in `ReadUpdatePolicy`, replace the final `return out, runs, nil` with:
  ```go
  	if out != nil {
  		out.NextOccurrence = nextOccurrenceOf(*out, time.Now())
  	}
  	return out, runs, nil
  ```
- in `ResumeUpdatePolicy`, replace the final `return out, nil` with:
  ```go
  	out.NextOccurrence = nextOccurrenceOf(*out, time.Now())
  	return out, nil
  ```

In `internal/store/reconcile.go`, replace the doc comment's last sentence "Deployments are left to their own sweep." with "Deployments are left to their own sweep. Policy runs the previous process left open fail with `the server restarted during the run` and count against their policy, in the same transaction; the count returned is commands only." and replace the function's final line

```go
	return total, tx.Commit()
```

with

```go
	if err := t.reconcilePolicyRuns(ctx, tx); err != nil {
		return 0, err
	}
	return total, tx.Commit()
```

In `internal/store/store.go`, add after `ListPolicyRuns` (Task 1):

```go
	// The scheduler's side of update policies: trusted, not tenant-scoped. Each run re-authorizes
	// its own steps as the policy's creator.
	SchedulePolicies(ctx context.Context, now time.Time) ([]ScheduledPolicy, error)
	BeginPolicyRun(ctx context.Context, policyID string, occurrence time.Time, correlation string) (string, error)
	FinishPolicyRun(ctx context.Context, runID, outcome, deploymentID, detail string) error
	SkipPolicyWindow(ctx context.Context, policyID string, occurrence time.Time, outcome, detail string) error
```

and change the `ReconcileAfterStart` comment to `// ReconcileAfterStart settles every in-flight command as unknown and fails every open policy run; startup only.`

- [ ] **Step 4: Run to verify they pass, on both drivers**

Run: `gofmt -w internal/store && go vet ./internal/store/ && go test -race -count=1 ./internal/store/... && PG=… go test -count=1 ./internal/store/...`
Expected: PASS, including the existing `TestReconcileAfterStart*` (they count commands only).

- [ ] **Step 5: DOX and commit**

`internal/store/AGENTS.md`, Local Contracts: append "- Policy schedule (`policy_schedule.go`): an occurrence is the policy's window on the zone's calendar date, `time.Date` of the wall-clock minutes; a start the zone skips (DST gap) has no occurrence that day, an overlap is Go's first mapping, and the key is `start.UTC()`. `SchedulePolicies(now)` returns active policies (created_at order) whose occurrence is open with no run row, or whose most recent ended occurrence ended after `updated_at` with no row (missed), plus the instance's endpoint and whether it holds an `applying` or unexpired `planned` deployment (wall clock). `BeginPolicyRun` inserts the row first (`ErrAlreadyExists` for a taken window, `ErrNotFound` for a paused or deleted policy); `FinishPolicyRun` and `SkipPolicyWindow` settle it: `blocked`/`failed` count, `applied`/`planned`/`no_update` reset, skips leave it; 3 failures pause with `three consecutive windows failed`, a `paused` outcome pauses with the creator sentence; each run writes `application.policy.run` and each pause `application.policy.paused`, user `system`, under the run's correlation ID. `ReconcileAfterStart` fails open runs (`the server restarted during the run`) in its transaction."

```bash
git add internal/store
git commit -m "feat(store): policy occurrences, schedule and run bookkeeping" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: API — the scheduler (`RunPolicies`)

**Files:**
- Create: `internal/api/policies.go`
- Modify: `internal/api/server.go` (the `policies policyScheduler` field on `Server`)
- Modify: `internal/api/image_check_handlers.go` (`holdApplication`, `takeRegistrySlot`, `waitRegistrySlot`; the two HTTP functions wrap them)
- Modify: `internal/api/plan_inspection.go` (`inspectForPlan`; `planInspections` wraps it)
- Modify: `internal/api/export_test.go` (tick, wait and clock hooks)
- Test: `internal/api/policies_test.go`, `internal/api/policies_internal_test.go`
- Docs: `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: Task 1–2 store API; `CheckImageUpdateAccess`, `CheckImageUpdates`, `PreflightApplication`, `ReadEndpoint`, `ReadApplicationInstance`, `PlanDeployment`, `ApplyDeployment`, `FailDeployment`, `StillAllowed`; `s.agents.deliver`, `s.Connected`, `maxFrameBytes`, `s.resolver`, `s.inspect`; test fixtures `newPlanHost`, `planHost.do`, `planHost.online`, `verifiedInspector`, `verifiedObservation`, `fakeDigests`, `loginAs`, `tenantRequest`, `readEnvelope`.
- Produces:
  ```go
  func (s *Server) RunPolicies(ctx context.Context, done chan<- struct{})
  func (s *Server) holdApplication(a store.TenantAccess, app string) (release func(), ok bool)
  func (s *Server) takeRegistrySlot(org string) (release func(), ok bool)
  func (s *Server) waitRegistrySlot(org string, wait time.Duration) (release func(), ok bool)
  func (s *Server) inspectForPlan(ctx context.Context, a store.TenantAccess, ep *store.Endpoint, pre *store.DeploymentPreflight, started func(), allowed func(context.Context) bool) map[string]protocol.ContainerInspection
  // export_test.go
  func PolicyTickForTest(s *Server, now time.Time)
  func WaitPolicyRunsForTest(s *Server)
  func SetPolicyClockForTest(s *Server, interval time.Duration, clock func() time.Time)
  ```
  Test helpers Task 4 reuses: `policyCaps`, `tomorrow()`, `planHost.appID`, `planHost.as`, `planHost.policy`, `planHost.updatable`, `planHost.tick`, `planHost.runs`, `policyAuditRows`, `planHost.secondAdmin`.

- [ ] **Step 1: Write the failing internal tests**

Create `internal/api/policies_internal_test.go`:

```go
package api

import (
	"strings"
	"testing"
	"time"
)

// One automated run per endpoint and two server-wide; a policy never runs twice at once. An
// application nobody adopted has no endpoint to hold.
func TestPolicyAdmission(t *testing.T) {
	var p policyScheduler
	if !p.admit("p1", "e1") {
		t.Fatal("first run refused")
	}
	if p.admit("p2", "e1") {
		t.Fatal("a second run on one endpoint")
	}
	if p.admit("p1", "e2") {
		t.Fatal("one policy admitted twice")
	}
	if !p.admit("p3", "e2") {
		t.Fatal("second endpoint refused")
	}
	if p.admit("p4", "e3") {
		t.Fatal("a third run server-wide")
	}
	p.release("p1")
	if !p.admit("p4", "e3") || !p.running("p4") || p.running("p1") {
		t.Fatal("a released slot was not reusable")
	}
	p.release("p3")
	p.release("p4")
	if !p.admit("p5", "") || !p.admit("p6", "") {
		t.Fatal("unadopted applications held each other")
	}
}

// A run waits for a registry slot, up to its bound.
func TestWaitRegistrySlot(t *testing.T) {
	s := &Server{registrySlots: make(chan struct{}, registrySlotsTotal)}
	first, _ := s.takeRegistrySlot("org")
	second, _ := s.takeRegistrySlot("org")
	start := time.Now()
	if _, ok := s.waitRegistrySlot("org", 50*time.Millisecond); ok || time.Since(start) < 50*time.Millisecond {
		t.Fatal("a slot past the organization's quota, or no wait")
	}
	go func() { time.Sleep(20 * time.Millisecond); first() }()
	release, ok := s.waitRegistrySlot("org", 5*time.Second)
	if !ok {
		t.Fatal("a freed slot was not taken")
	}
	release()
	second()
	if len(s.registryHeld) != 0 || len(s.registrySlots) != 0 {
		t.Fatalf("slots left held: %v %d", s.registryHeld, len(s.registrySlots))
	}
}

// A blocked run's detail holds whole codes within the column.
func TestJoinCodes(t *testing.T) {
	if got := joinCodes([]string{"a", "b"}, 255); got != "a,b" {
		t.Fatal(got)
	}
	long := []string{strings.Repeat("x", 200), strings.Repeat("y", 60), "z"}
	if got := joinCodes(long, 255); got != strings.Repeat("x", 200) {
		t.Fatalf("%d bytes", len(got))
	}
}
```

- [ ] **Step 2: Write the failing scheduler tests**

Create `internal/api/policies_test.go`:

```go
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// policyCaps is an agent that inspects with verdicts, applies and pulls.
var policyCaps = []string{protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull, protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict}

// tomorrow is the next UTC midnight: the fixture policy's windows (10:00-11:00 UTC) from there on
// lie after the policy was saved, so a window the test clock has passed counts as missed.
func tomorrow() time.Time { return time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour) }

func (h planHost) appID() string {
	return strings.TrimSuffix(strings.TrimPrefix(h.deployments, "/api/organizations/a/environments/env-a/applications/"), "/deployments")
}

func (h planHost) as(user string) store.TenantAccess {
	return store.TenantAccess{ActorID: user, OrganizationID: "a", EnvironmentID: "env-a", CorrelationID: "policy-test"}
}

// policy saves a 10:00-11:00 UTC policy as the planner for every day but today (UTC): the
// windows from tomorrow on lie after the save, and today's, which may not have ended yet, can
// never be recorded as missed whatever the hour the test runs.
func (h planHost) policy(t *testing.T, mode string) *store.UpdatePolicy {
	t.Helper()
	var days []int
	for d := range 7 {
		if d != int(time.Now().UTC().Weekday()) {
			days = append(days, d)
		}
	}
	p, _, err := h.st.Tenancy().PutUpdatePolicy(context.Background(), h.as("usr_planner"), h.appID(), store.PolicyInput{Mode: mode, Timezone: "UTC", Weekdays: days, StartMinute: 600, EndMinute: 660})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// updatable gives the host's image a registry digest, turns anonymous pulls on and answers every
// registry lookup with a newer digest, so a check finds each service update_available.
func (h planHost) updatable(t *testing.T) *fakeDigests {
	t.Helper()
	snap := h.snapshot
	snap.Images = []protocol.Image{{ID: h.snapshot.Images[0].ID, Tags: []string{"nginx:1"}, Digests: []string{"nginx@sha256:" + strings.Repeat("c", 64)}}}
	raw, _ := json.Marshal(snap)
	if _, err := h.st.Tenancy().AcceptInventory(context.Background(), h.ag.id, uint64(time.Now().Unix())+1, time.Now(), raw); err != nil {
		t.Fatal(err)
	}
	h.do(t, "PUT", "/api/organizations/a/registry-policy", `{"anonymous_pull_enabled":true}`, 204)
	fake := &fakeDigests{digest: "sha256:" + strings.Repeat("d", 64)}
	api.SetDigestResolverForTest(h.s, fake)
	return fake
}

// tick runs one scheduler tick at now and waits for the runs it started.
func (h planHost) tick(now time.Time) {
	api.PolicyTickForTest(h.s, now)
	api.WaitPolicyRunsForTest(h.s)
}

func (h planHost) runs(t *testing.T, user string) []store.PolicyRun {
	t.Helper()
	runs, err := h.st.Tenancy().ListPolicyRuns(context.Background(), h.as(user), h.appID(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func policyAuditRows(t *testing.T, h planHost, cookie *http.Cookie) []store.AuditRecord {
	t.Helper()
	w := tenantRequest(h.s, cookie, "GET", "/api/organizations/a/audit?limit=200", "", false)
	var rows []store.AuditRecord
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
		t.Fatalf("audit: %d %s", w.Code, w.Body.String())
	}
	return rows
}

// secondAdmin is another organization administrator, who can still read once the planner cannot.
func (h planHost) secondAdmin(t *testing.T) *http.Cookie {
	t.Helper()
	cookie := loginAs(t, h.s, h.st, "other", "user")
	if err := h.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_other", Role: store.RoleOrganizationAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	return cookie
}

// Inside the window with an update available, the run checks, plans and applies as the policy's
// creator, sends the frame, and every row of it carries one correlation ID.
func TestPolicyRunPlansAndAppliesAsItsCreator(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.updatable(t)
	sock, ctx := h.online(t, policyCaps)
	p := h.policy(t, store.PolicyModeApply)
	day := tomorrow()
	h.tick(day.Add(10*time.Hour + 30*time.Minute))

	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunApplied || runs[0].DeploymentID == "" || runs[0].Detail != "" || !runs[0].Occurrence.Equal(day.Add(10*time.Hour)) || runs[0].FinishedAt == nil {
		t.Fatalf("runs: %+v", runs)
	}
	run := runs[0]
	frame := readEnvelope(t, ctx, sock.conn)
	var req protocol.DeploymentRequest
	if frame.Type != protocol.TypeDeploymentApply || json.Unmarshal(frame.Payload, &req) != nil || req.Deployment != run.DeploymentID || req.RequestID != run.CorrelationID || len(req.Services) != 1 || req.Services[0].Pull == nil || req.Services[0].Pull.Digest != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("frame: %s %+v", frame.Type, req)
	}
	var d store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", h.deployments+"/"+run.DeploymentID, "", 200)), &d); err != nil {
		t.Fatal(err)
	}
	if d.State != "applying" || d.CreatedBy != "usr_planner" || d.AppliedBy != "usr_planner" || d.CorrelationID != run.CorrelationID {
		t.Fatalf("deployment: %+v", d)
	}
	want := map[string]string{
		"application.deploy " + h.appID() + "/updates":                                 "usr_planner",
		"application.deploy " + h.appID() + "/deployments/" + d.ID:                     "usr_planner",
		"application.deploy " + h.appID() + "/deployments/" + d.ID + "/apply":          "usr_planner",
		"application.policy.run " + h.appID() + "/policies/" + p.ID + "/runs/" + run.ID: "system",
	}
	for _, r := range policyAuditRows(t, h, h.admin) {
		if r.CorrelationID != run.CorrelationID {
			continue
		}
		key := r.Action + " " + r.Resource
		if want[key] != r.UserID || r.Result != "success" {
			t.Fatalf("unexpected row under the run's correlation: %+v", r)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing rows: %v", want)
	}
	// The window has its run: another tick inside it starts nothing.
	h.tick(day.Add(10*time.Hour + 45*time.Minute))
	if runs := h.runs(t, "usr_planner"); len(runs) != 1 {
		t.Fatalf("a second run in one window: %+v", runs)
	}
}

// plan_only stops at a plan that waits for a click like any manual plan.
func TestPolicyPlanOnlyStopsAtPlanned(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.updatable(t)
	h.policy(t, store.PolicyModePlanOnly)
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunPlanned || runs[0].DeploymentID == "" {
		t.Fatalf("runs: %+v", runs)
	}
	var d store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", h.deployments+"/"+runs[0].DeploymentID, "", 200)), &d); err != nil {
		t.Fatal(err)
	}
	if d.State != "planned" || d.CreatedBy != "usr_planner" || d.CorrelationID != runs[0].CorrelationID || d.Plan.Services[0].PullDigest != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("plan: %+v", d)
	}
}

// Nothing newer (the host image has no registry digest: unknown_local) is no_update, no plan.
func TestPolicyRunWithNoUpdate(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	h.policy(t, store.PolicyModeApply)
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunNoUpdate || runs[0].DeploymentID != "" || runs[0].Detail != "" {
		t.Fatalf("runs: %+v", runs)
	}
	if body := h.do(t, "GET", h.deployments, "", 200); !strings.HasPrefix(body, "[]") {
		t.Fatalf("deployments: %s", body)
	}
}

// A plan-time blocker is the run's outcome, the blocker codes its detail.
func TestPolicyRunBlocked(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
		in := verifiedObservation(target)
		in.ConfigurationVerified, in.Unsupported = false, []string{"privileged"}
		return in, nil
	})
	h.updatable(t)
	h.policy(t, store.PolicyModeApply)
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunBlocked || runs[0].Detail != "configuration_unsupported" || runs[0].DeploymentID != "" {
		t.Fatalf("runs: %+v", runs)
	}
}

// A creator who lost application.deploy pauses the policy without counting, with its audit rows.
func TestPolicyPausesWhenTheCreatorLosesTheRole(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	other := h.secondAdmin(t)
	p := h.policy(t, store.PolicyModeApply)
	if err := h.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_planner", Role: store.RoleOperator, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	day := tomorrow()
	h.tick(day.Add(10*time.Hour + 30*time.Minute))
	assertPausedByCreator(t, h, other, p)
	// Paused: the next window runs nothing.
	h.tick(day.Add(34*time.Hour + 30*time.Minute))
	if runs := h.runs(t, "usr_other"); len(runs) != 1 {
		t.Fatalf("a paused policy ran: %+v", runs)
	}
}

// The creator's account deleted outright pauses the same way (Review Focus 5).
func TestPolicyPausesWhenTheCreatorIsDeleted(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	other := h.secondAdmin(t)
	p := h.policy(t, store.PolicyModeApply)
	if err := h.st.Users().DeleteUser(context.Background(), "usr_planner"); err != nil {
		t.Fatal(err)
	}
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	assertPausedByCreator(t, h, other, p)
}

func assertPausedByCreator(t *testing.T, h planHost, other *http.Cookie, p *store.UpdatePolicy) {
	t.Helper()
	runs := h.runs(t, "usr_other")
	if len(runs) != 1 || runs[0].Outcome != store.RunPaused || runs[0].Detail != store.PolicyDetailCreatorLost || runs[0].DeploymentID != "" {
		t.Fatalf("runs: %+v", runs)
	}
	got, _, err := h.st.Tenancy().ReadUpdatePolicy(context.Background(), h.as("usr_other"), h.appID())
	if err != nil || got.Status != store.PolicyPaused || got.PausedReason != store.PolicyDetailCreatorLost || got.ConsecutiveFailures != 0 {
		t.Fatalf("policy: %+v %v", got, err)
	}
	var run, paused bool
	for _, r := range policyAuditRows(t, h, other) {
		if r.CorrelationID != runs[0].CorrelationID || r.UserID != "system" {
			continue
		}
		switch {
		case r.Action == store.AuditPolicyRun && r.Result == "denied" && r.Details == store.RunPaused:
			run = true
		case r.Action == store.AuditPolicyPaused && r.Result == "denied" && r.Resource == h.appID()+"/policies/"+p.ID:
			paused = true
		}
	}
	if !run || !paused {
		t.Fatalf("audit rows: run=%v paused=%v", run, paused)
	}
	if body := tenantRequest(h.s, other, "GET", h.deployments, "", false).Body.String(); !strings.HasPrefix(body, "[]") {
		t.Fatalf("a deployment was made: %s", body)
	}
}

// A window that ended while nothing ran is recorded once as missed; it is not a failure.
func TestPolicyRecordsAMissedWindow(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	h.policy(t, store.PolicyModeApply)
	day := tomorrow()
	for range 2 {
		h.tick(day.Add(11*time.Hour + 5*time.Minute))
	}
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunSkippedMissed || runs[0].Detail != store.PolicyDetailMissed || !runs[0].Occurrence.Equal(day.Add(10*time.Hour)) || runs[0].FinishedAt == nil {
		t.Fatalf("runs: %+v", runs)
	}
	if got, _, _ := h.st.Tenancy().ReadUpdatePolicy(context.Background(), h.as("usr_planner"), h.appID()); got.ConsecutiveFailures != 0 {
		t.Fatalf("a missed window counted: %+v", got)
	}
}

// A live plan holds the endpoint: no run while the window is open, skipped_busy once it closes.
func TestPolicySkipsABusyEndpoint(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.updatable(t)
	h.policy(t, store.PolicyModeApply)
	h.do(t, "POST", h.deployments, h.planBody, 201)
	day := tomorrow()
	h.tick(day.Add(10*time.Hour + 30*time.Minute))
	h.tick(day.Add(10*time.Hour + 40*time.Minute))
	if runs := h.runs(t, "usr_planner"); len(runs) != 0 {
		t.Fatalf("ran beside a live plan: %+v", runs)
	}
	h.tick(day.Add(11*time.Hour + time.Minute))
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunSkippedBusy || runs[0].Detail != store.PolicyDetailBusy {
		t.Fatalf("runs: %+v", runs)
	}
}

// Three failed windows in a row pause the policy, with its audit row; the fourth window runs nothing.
func TestPolicyPausesAfterThreeFailedWindows(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	fake := h.updatable(t)
	fake.err = registry.ErrUnauthorized
	p := h.policy(t, store.PolicyModeApply)
	day := tomorrow()
	for i := range 4 {
		h.tick(day.AddDate(0, 0, i).Add(10*time.Hour + 30*time.Minute))
	}
	runs := h.runs(t, "usr_planner")
	if len(runs) != 3 {
		t.Fatalf("runs: %+v", runs)
	}
	for _, r := range runs {
		if r.Outcome != store.RunFailed || r.Detail != "unauthorized" {
			t.Fatalf("run: %+v", r)
		}
	}
	got, _, err := h.st.Tenancy().ReadUpdatePolicy(context.Background(), h.as("usr_planner"), h.appID())
	if err != nil || got.Status != store.PolicyPaused || got.PausedReason != store.PolicyReasonFailures || got.ConsecutiveFailures != 3 {
		t.Fatalf("policy: %+v %v", got, err)
	}
	var paused bool
	for _, r := range policyAuditRows(t, h, h.admin) {
		if r.Action == store.AuditPolicyPaused && r.Result == "failure" && r.UserID == "system" && r.Resource == h.appID()+"/policies/"+p.ID && r.CorrelationID == runs[0].CorrelationID {
			paused = true
		}
	}
	if !paused {
		t.Fatal("no application.policy.paused row under the third run's correlation")
	}
}

// An application released between the tick and the run fails the window and counts; it does
// not plan against nothing (Review Focus 1).
func TestPolicyRunOnAReleasedApplicationFails(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	h.policy(t, store.PolicyModeApply)
	base := strings.TrimSuffix(h.deployments, "/deployments")
	var m store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", base+"/mapping", "", 200)), &m); err != nil {
		t.Fatal(err)
	}
	h.do(t, "DELETE", base+"/adoption", `{"instance_id":"`+m.InstanceID+`","confirm":"shop"}`, 204)
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunFailed || runs[0].Detail != "not_adopted" || runs[0].DeploymentID != "" {
		t.Fatalf("runs: %+v", runs)
	}
	if got, _, _ := h.st.Tenancy().ReadUpdatePolicy(context.Background(), h.as("usr_planner"), h.appID()); got.ConsecutiveFailures != 1 || got.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", got)
	}
}

// Ticks never overlap: four at once on one open window make one run and one plan (Review Focus 3).
func TestPolicyTicksNeverOverlap(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.updatable(t)
	h.policy(t, store.PolicyModePlanOnly)
	now := tomorrow().Add(10*time.Hour + 30*time.Minute)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); api.PolicyTickForTest(h.s, now) }()
	}
	wg.Wait()
	api.WaitPolicyRunsForTest(h.s)
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunPlanned {
		t.Fatalf("runs: %+v", runs)
	}
	var list []store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", h.deployments, "", 200)), &list); err != nil || len(list) != 1 {
		t.Fatalf("deployments: %+v %v", list, err)
	}
}

// done closes only once the run in flight has finished and been recorded.
func TestRunPoliciesClosesDoneOnlyAfterTheRunInFlight(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	fake := h.updatable(t)
	fake.gate, fake.entered = make(chan struct{}), make(chan struct{}, 4)
	h.policy(t, store.PolicyModePlanOnly)
	now := tomorrow().Add(10*time.Hour + 30*time.Minute)
	api.SetPolicyClockForTest(h.s, 10*time.Millisecond, func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go h.s.RunPolicies(ctx, done)
	select {
	case <-fake.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no run reached the registry")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("done closed with a run in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(fake.gate)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("done never closed")
	}
	if runs := h.runs(t, "usr_planner"); len(runs) != 1 || runs[0].Outcome != store.RunPlanned {
		t.Fatalf("the run was not recorded before done: %+v", runs)
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test -count=1 ./internal/api/ -run 'Policy|WaitRegistrySlot|JoinCodes|RunPolicies'`
Expected: FAIL to compile with `undefined: policyScheduler` and `undefined: api.PolicyTickForTest`.

- [ ] **Step 4: Give the guard, the slot and the inspections writer-free cores**

In `internal/api/image_check_handlers.go` replace `guardApplication` and `acquireRegistrySlot` with:

```go
// guardApplication admits one registry operation per application at a time, an update check or
// an update plan, keyed by the canonical application ID, or answers 409 check_in_progress.
func (s *Server) guardApplication(w http.ResponseWriter, a store.TenantAccess, app string) (release func(), ok bool) {
	release, ok = s.holdApplication(a, app)
	if !ok {
		s.tenantError(w, errCheckInProgress)
	}
	return release, ok
}

// holdApplication is guardApplication without a response, for the policy scheduler.
func (s *Server) holdApplication(a store.TenantAccess, app string) (release func(), ok bool) {
	key := a.OrganizationID + "/" + a.EnvironmentID + "/" + app
	if _, busy := s.imageChecks.LoadOrStore(key, struct{}{}); busy {
		return nil, false
	}
	return func() { s.imageChecks.Delete(key) }, true
}

// acquireRegistrySlot takes one of org's registry slots and one of the server's, or answers 429.
func (s *Server) acquireRegistrySlot(w http.ResponseWriter, org string) (release func(), ok bool) {
	release, ok = s.takeRegistrySlot(org)
	if !ok {
		w.Header().Set("Retry-After", "5")
		s.tenantError(w, errTooManyChecks)
	}
	return release, ok
}

// takeRegistrySlot is acquireRegistrySlot without a response.
func (s *Server) takeRegistrySlot(org string) (release func(), ok bool) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	if s.registryHeld == nil {
		s.registryHeld = map[string]int{}
	}
	if s.registryHeld[org] >= registrySlotsPerOrganization {
		return nil, false
	}
	select {
	case s.registrySlots <- struct{}{}:
	default:
		return nil, false
	}
	s.registryHeld[org]++
	return func() {
		s.registryMu.Lock()
		defer s.registryMu.Unlock()
		if s.registryHeld[org]--; s.registryHeld[org] == 0 {
			delete(s.registryHeld, org)
		}
		<-s.registrySlots
	}, true
}

// waitRegistrySlot polls takeRegistrySlot about once a second until wait has passed.
func (s *Server) waitRegistrySlot(org string, wait time.Duration) (release func(), ok bool) {
	deadline := time.Now().Add(wait)
	for {
		if release, ok := s.takeRegistrySlot(org); ok {
			return release, true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, false
		}
		time.Sleep(min(time.Second, left))
	}
}
```

In `internal/api/plan_inspection.go` replace `planInspections` with:

```go
// planInspections inspects each mapped service's container, in plan order and one at a time,
// under one budget and one inspection attempt, so the store can refuse at plan time what the
// agent would deny at apply. A failure of any kind leaves that container without an entry; an
// agent without container.inspect and container.inspect.verdict gets no request. The preflight
// here is the latest revision's, so it does not gate the fan-out: the store judges the revision
// being planned. The observations are consumed by the plan and never stored.
func (s *Server) planInspections(w http.ResponseWriter, r *http.Request, a store.TenantAccess, ep *store.Endpoint, pre *store.DeploymentPreflight) map[string]protocol.ContainerInspection {
	return s.inspectForPlan(r.Context(), a, ep, pre, func() {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(planInspectionBudget + 5*time.Second))
	}, func(ctx context.Context) bool { return s.inspectionAllowed(r.WithContext(ctx), a, ep.ID) })
}

// inspectForPlan is planInspections without a request: started runs once the fan-out is
// admitted, and allowed re-checks the caller's authority during each inspection.
func (s *Server) inspectForPlan(ctx context.Context, a store.TenantAccess, ep *store.Endpoint, pre *store.DeploymentPreflight, started func(), allowed func(context.Context) bool) map[string]protocol.ContainerInspection {
	out := map[string]protocol.ContainerInspection{}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspect) || !slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspectVerdict) || !s.allowAttempt("inspection:"+a.ActorID, 30, time.Minute) {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, planInspectionBudget)
	defer cancel()
	started()
	inspect := s.planInspector
	if inspect == nil {
		inspect = func(ctx context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
			s.agents.mu.Lock()
			agent := s.agents.conns[ep.ID]
			s.agents.mu.Unlock()
			if agent == nil {
				return protocol.ContainerInspection{}, store.ErrEndpointOffline
			}
			return s.inspect(ctx, agent, a.ActorID, a.OrganizationID, target, func() bool { return allowed(ctx) })
		}
	}
	for _, svc := range pre.Services {
		if ctx.Err() != nil {
			break // the budget is spent: open no admission, send no expired grant
		}
		if svc.InspectionTarget == nil {
			continue
		}
		if in, err := inspect(ctx, *svc.InspectionTarget); err == nil {
			out[svc.InspectionTarget.ContainerID] = in
		}
	}
	return out
}
```

- [ ] **Step 5: Implement the scheduler**

In `internal/api/server.go`, add to the `Server` struct after `registryHeld`:

```go
	// policies is the update-policy scheduler's in-memory state (policies.go).
	policies policyScheduler
```

Create `internal/api/policies.go`:

```go
package api

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/google/uuid"
)

const (
	// policyRunsServerWide bounds automated runs in flight; each also holds its endpoint alone.
	policyRunsServerWide = 2
	// policySlotWait is how long a run waits for a registry slot before skipping its window.
	policySlotWait = 30 * time.Second
)

// policyScheduler is the scheduler's in-memory half: which runs are in flight and which open
// windows a tick passed over as busy. Everything that must survive a restart is in the database.
type policyScheduler struct {
	tick     sync.Mutex // policyTick: one tick at a time
	deferred map[string]time.Time // policy ID -> the open occurrence passed over as busy; under tick
	runs     sync.WaitGroup
	mu       sync.Mutex
	inFlight map[string]string // policy ID -> endpoint ID; under mu
	// Tests only: the tick interval (zero is one minute) and the clock (nil is time.Now).
	interval time.Duration
	now      func() time.Time
}

// admit reserves a run for policy on endpoint, or refuses: the policy already running, another
// run on the endpoint, or policyRunsServerWide in flight. "" (not adopted) holds no endpoint.
func (p *policyScheduler) admit(policy, endpoint string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight == nil {
		p.inFlight = map[string]string{}
	}
	if _, running := p.inFlight[policy]; running || len(p.inFlight) >= policyRunsServerWide {
		return false
	}
	for _, e := range p.inFlight {
		if endpoint != "" && e == endpoint {
			return false
		}
	}
	p.inFlight[policy] = endpoint
	return true
}

func (p *policyScheduler) release(policy string) {
	p.mu.Lock()
	delete(p.inFlight, policy)
	p.mu.Unlock()
}

func (p *policyScheduler) running(policy string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.inFlight[policy]
	return ok
}

// RunPolicies runs the update-policy scheduler until ctx ends: a tick a minute, each run in its
// own goroutine. done closes only once no run is in flight, so runServer can wait on it before
// the store closes.
func (s *Server) RunPolicies(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	defer s.policies.runs.Wait()
	interval := s.policies.interval
	if interval == 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			if s.policies.now != nil {
				now = s.policies.now()
			}
			s.policyTick(ctx, now)
		}
	}
}

// policyTick records the windows that closed without a run, then starts the runs that are due,
// in created_at order, without waiting for them. Ticks never overlap.
func (s *Server) policyTick(ctx context.Context, now time.Time) {
	p := &s.policies
	p.tick.Lock()
	defer p.tick.Unlock()
	if s.stopping.Load() {
		return
	}
	ts := s.store.Tenancy()
	due, err := ts.SchedulePolicies(ctx, now)
	if err != nil {
		log.Printf("[POLICY] schedule unreadable: %v", err)
		return
	}
	listed := map[string]bool{}
	for _, sp := range due {
		listed[sp.Policy.ID] = true
	}
	for id := range p.deferred {
		if !listed[id] {
			delete(p.deferred, id)
		}
	}
	for _, sp := range due {
		if !sp.Missed || p.running(sp.Policy.ID) {
			continue
		}
		outcome, detail := store.RunSkippedMissed, store.PolicyDetailMissed
		if at, ok := p.deferred[sp.Policy.ID]; ok && at.Equal(sp.Previous) {
			outcome, detail = store.RunSkippedBusy, store.PolicyDetailBusy
		}
		delete(p.deferred, sp.Policy.ID)
		if err := ts.SkipPolicyWindow(ctx, sp.Policy.ID, sp.Previous, outcome, detail); err != nil && !errors.Is(err, store.ErrAlreadyExists) && !errors.Is(err, store.ErrNotFound) {
			log.Printf("[POLICY] policy %s: recording %s: %v", sp.Policy.ID, outcome, err)
		}
	}
	for _, sp := range due {
		if !sp.Open || p.running(sp.Policy.ID) {
			continue
		}
		if sp.EndpointBusy || !p.admit(sp.Policy.ID, sp.EndpointID) {
			if p.deferred == nil {
				p.deferred = map[string]time.Time{}
			}
			p.deferred[sp.Policy.ID] = sp.Occurrence
			continue
		}
		delete(p.deferred, sp.Policy.ID)
		p.runs.Add(1)
		go func(sp store.ScheduledPolicy) {
			defer p.runs.Done()
			defer p.release(sp.Policy.ID)
			// A run in flight finishes: shutdown waits for it rather than cut an apply in half.
			s.runPolicy(context.WithoutCancel(ctx), sp)
		}(sp)
	}
}

// runPolicy opens the window's run row, does the work as the policy's last editor under one
// correlation ID, and records the outcome.
func (s *Server) runPolicy(ctx context.Context, sp store.ScheduledPolicy) {
	pol := sp.Policy
	a := store.TenantAccess{ActorID: pol.CreatedBy, OrganizationID: pol.OrganizationID, EnvironmentID: pol.EnvironmentID, CorrelationID: uuid.NewString()}
	ts := s.store.Tenancy()
	run, err := ts.BeginPolicyRun(ctx, pol.ID, sp.Occurrence, a.CorrelationID)
	if errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrNotFound) {
		return // the window has its run, or the policy was paused or deleted since the tick
	}
	if err != nil {
		log.Printf("[POLICY] policy %s: opening a run: %v", pol.ID, err)
		return
	}
	outcome, deployment, detail := s.performPolicyRun(ctx, a, pol)
	if err := ts.FinishPolicyRun(ctx, run, outcome, deployment, detail); err != nil {
		log.Printf("[POLICY] policy %s run %s: recording %s: %v", pol.ID, run, outcome, err)
		return
	}
	log.Printf("[POLICY] policy %s window %s: %s", pol.ID, sp.Occurrence.Format(time.RFC3339), outcome)
}

// performPolicyRun is one run's work as a: authorize, check, plan and, in apply mode, apply and
// send, each step re-authorized by the store. It returns the outcome, the deployment it made and
// a fixed detail.
func (s *Server) performPolicyRun(ctx context.Context, a store.TenantAccess, pol store.UpdatePolicy) (outcome, deployment, detail string) {
	ts := s.store.Tenancy()
	app := pol.ApplicationID
	if err := ts.CheckImageUpdateAccess(ctx, a, app); err != nil {
		return policyFailure(err)
	}
	done, ok := s.holdApplication(a, app)
	if !ok {
		return store.RunSkippedBusy, "", "check_in_progress"
	}
	defer done()
	release, ok := s.waitRegistrySlot(a.OrganizationID, policySlotWait)
	if !ok {
		return store.RunSkippedBusy, "", "too_many_checks"
	}
	defer release()
	key, private := s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate
	check, err := ts.CheckImageUpdates(ctx, a, app, s.resolver(), key, private)
	if err != nil {
		return policyFailure(err)
	}
	var update []string
	registryDetail := ""
	for _, c := range check.Services {
		switch {
		case c.Verdict == "update_available":
			update = append(update, c.Service)
		case c.Verdict == "registry_error" && registryDetail == "":
			registryDetail = c.Detail
		}
	}
	switch {
	case len(update) == 0 && registryDetail != "":
		return store.RunFailed, "", registryDetail
	case len(update) == 0:
		return store.RunNoUpdate, "", ""
	}
	pre, err := ts.PreflightApplication(ctx, a, app)
	if err != nil {
		return policyFailure(err)
	}
	ep, err := ts.ReadEndpoint(ctx, a, pre.EndpointID)
	if err != nil {
		return policyFailure(err)
	}
	instance, err := ts.ReadApplicationInstance(ctx, a, app, pre.InstanceID)
	if err != nil {
		return policyFailure(err)
	}
	maxFrame := maxFrameBytes(ep.Capabilities)
	req := store.PlanRequest{InstanceID: pre.InstanceID, MappingVersion: pre.MappingVersion, Revision: pre.Revision, Confirm: instance.Project, Update: update, MaxFrameBytes: maxFrame,
		Inspections: s.inspectForPlan(ctx, a, ep, pre, func() {}, s.policyInspectionAllowed(a, ep.ID))}
	d, err := ts.PlanDeployment(ctx, a, app, req, s.resolver(), key, private)
	var blocked *store.PreflightBlockedError
	if errors.As(err, &blocked) {
		return store.RunBlocked, "", joinCodes(blocked.Blockers, 255)
	}
	if err != nil {
		return policyFailure(err)
	}
	if pol.Mode == store.PolicyModePlanOnly {
		return store.RunPlanned, d.ID, ""
	}
	// From here as handleApplyDeployment: the row is applying before the frame leaves, and a
	// frame that cannot be queued fails the row.
	if !s.Connected(d.EndpointID) {
		return store.RunFailed, d.ID, "endpoint_offline"
	}
	applied, frame, err := ts.ApplyDeployment(ctx, a, app, d.ID, d.Plan.Project, key, maxFrame)
	if err != nil {
		failed, _, why := policyFailure(err)
		return failed, d.ID, why
	}
	if !s.agents.deliver(applied.EndpointID, envelope(protocol.TypeDeploymentApply, frame)) {
		if err := ts.FailDeployment(ctx, applied.ID, "the endpoint disconnected before the deployment was sent"); err != nil {
			log.Printf("[POLICY] deployment %s: recording an unsent frame: %v", applied.ID, err)
		}
		return store.RunFailed, applied.ID, "not_sent"
	}
	return store.RunApplied, applied.ID, ""
}

// policyInspectionAllowed re-checks during each inspection that the policy's actor may still read
// the endpoint: inspectionAllowed without a session to re-authenticate.
func (s *Server) policyInspectionAllowed(a store.TenantAccess, endpoint string) func(context.Context) bool {
	return func(ctx context.Context) bool {
		ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		return s.store.Tenancy().StillAllowed(ctx, a, permissions.EndpointRead, endpoint) == nil
	}
}

// policyErrorCodes name a store refusal in a run's detail; anything else is "error".
var policyErrorCodes = []struct {
	err  error
	code string
}{
	{store.ErrNotFound, "not_adopted"},
	{store.ErrMappingRequired, "mapping_required"},
	{store.ErrAdoptionChanged, "adoption_changed"},
	{store.ErrDeploymentInProgress, "deployment_in_progress"},
	{store.ErrEndpointOffline, "endpoint_offline"},
	{store.ErrInvalid, "invalid"},
}

// policyFailure maps a store refusal to the run's outcome: lost authority pauses the policy,
// anything else fails the window with a fixed code.
func policyFailure(err error) (outcome, deployment, detail string) {
	if errors.Is(err, store.ErrForbidden) {
		return store.RunPaused, "", store.PolicyDetailCreatorLost
	}
	for _, c := range policyErrorCodes {
		if errors.Is(err, c.err) {
			return store.RunFailed, "", c.code
		}
	}
	return store.RunFailed, "", "error"
}

// joinCodes joins whole codes with "," within limit bytes, dropping those that do not fit.
func joinCodes(codes []string, limit int) string {
	out := ""
	for _, c := range codes {
		next := c
		if out != "" {
			next = out + "," + c
		}
		if len(next) > limit {
			break
		}
		out = next
	}
	return out
}
```

Append to `internal/api/export_test.go`:

```go
// PolicyTickForTest runs one scheduler tick at now; the runs it starts are not waited for.
func PolicyTickForTest(s *Server, now time.Time) { s.policyTick(context.Background(), now) }

// WaitPolicyRunsForTest blocks until every run a tick started has finished.
func WaitPolicyRunsForTest(s *Server) { s.policies.runs.Wait() }

// SetPolicyClockForTest makes RunPolicies tick every interval and read the time from clock.
func SetPolicyClockForTest(s *Server, interval time.Duration, clock func() time.Time) {
	s.policies.interval, s.policies.now = interval, clock
}
```

- [ ] **Step 6: Run to verify they pass, on both drivers**

Run: `gofmt -w internal/api && go vet ./internal/api/ && go test -race -count=1 ./internal/api/ && PG=… go test -count=1 ./internal/api/`
Expected: PASS, including the existing `TestRegistrySlots`, `TestPlanUpdateThroughTheRegistry`, `TestImageCheckRoutes` and `TestPlanInspectsEachMappedServiceInTurn` (the wrappers keep their responses and budgets).

- [ ] **Step 7: DOX and commit**

`internal/api/AGENTS.md`, last bullet of `## Local Contracts` (directly above `## Verification`): add "- `policies.go` is the update-policy scheduler, `RunPolicies(ctx, done)`: a one-minute tick under the `policyTick` mutex (no tick while stopping) reads `SchedulePolicies`, records each missed window (`skipped_missed`, or `skipped_busy` when this process passed the open window over as busy), then starts each due policy in its own goroutine when its endpoint holds no `applying` or unexpired `planned` deployment and `admit` allows (one run per endpoint, two server-wide). A run opens its row first (`BeginPolicyRun`), acts as `created_by` under a fresh correlation ID with every step re-authorized in the store (`CheckImageUpdateAccess`; `holdApplication`, else `skipped_busy` `check_in_progress`; `waitRegistrySlot` up to 30 s, else `skipped_busy` `too_many_checks`; `CheckImageUpdates`; `inspectForPlan`; `PlanDeployment` with the `update_available` services; in `apply` mode `ApplyDeployment` and `deliver`, `FailDeployment` with `not_sent` when the frame cannot be queued) and ends in `FinishPolicyRun`. `ErrForbidden` anywhere pauses (`paused`); other refusals are `failed` with `not_adopted`, `mapping_required`, `adoption_changed`, `deployment_in_progress`, `endpoint_offline`, `invalid` or `error`; blockers are joined whole within 255 bytes. Runs use `context.WithoutCancel`; `done` closes only after the loop stops and every run has finished. `guardApplication`, `acquireRegistrySlot` and `planInspections` are the HTTP wrappers of `holdApplication`, `takeRegistrySlot` and `inspectForPlan`." In `## Verification` extend the first bullet's parenthesis with "; `policies_test.go` the scheduler against a fake agent socket through `newPlanHost` with the test clock and tick hooks in `export_test.go`".

```bash
git add internal/api
git commit -m "feat(api): update-policy scheduler" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: API — the update-policy routes

**Files:**
- Create: `internal/api/policy_handlers.go`
- Modify: `internal/api/server.go` (`routes`: five routes after the `updates/check` route)
- Modify: `internal/api/tenant_handlers.go` (`tenantError`: four 400 codes and `policy_not_paused`)
- Test: `internal/api/policy_handlers_test.go`
- Docs: `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: Task 1–2 store API; Task 3's test helpers (`policyCaps`, `tomorrow`, `planHost.as`, `planHost.tick`, `planHost.runs`, `planHost.appID`, `policyAuditRows`, `planHost.secondAdmin`).
- Produces: `GET|PUT|DELETE .../applications/{application}/update-policy`, `POST .../update-policy/resume`, `GET .../update-policy/runs?limit=`; error codes `invalid_timezone`, `invalid_window`, `invalid_weekdays`, `invalid_mode` (400), `policy_not_paused` (409). `GET` returns the policy's fields plus `runs`.

- [ ] **Step 1: Write the failing test**

Create `internal/api/policy_handlers_test.go`:

```go
package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestUpdatePolicyRoutes(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	ctx := context.Background()
	ts := h.st.Tenancy()
	envAdmin := loginAs(t, h.s, h.st, "envadmin", "user")
	viewer := loginAs(t, h.s, h.st, "viewer", "user")
	for user, role := range map[string]store.TenantRole{"usr_envadmin": store.RoleEnvironmentAdmin, "usr_viewer": store.RoleReadOnly} {
		if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: user, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	other := h.secondAdmin(t)
	url := strings.TrimSuffix(h.deployments, "deployments") + "update-policy"
	send := func(cookie *http.Cookie, method, path, body string, csrf bool, status int) string {
		t.Helper()
		w := tenantRequest(h.s, cookie, method, path, body, csrf)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), status)
		}
		return w.Body.String()
	}
	code := func(body, want string) {
		t.Helper()
		var e struct{ Code string }
		if json.Unmarshal([]byte(body), &e) != nil || e.Code != want {
			t.Fatalf("want code %s: %s", want, body)
		}
	}
	send(h.admin, "GET", url, "", false, 404)
	body := `{"mode":"apply","timezone":"Europe/Paris","weekdays":[5,1,3],"start_minute":120,"end_minute":240}`
	send(envAdmin, "PUT", url, body, true, 403)
	send(h.admin, "PUT", url, body, false, 403) // no CSRF token
	raw := send(h.admin, "PUT", url, body, true, 201)
	var created store.UpdatePolicy
	if err := json.Unmarshal([]byte(raw), &created); err != nil {
		t.Fatal(err)
	}
	// next_occurrence is RFC3339 in UTC: it parses back into time.UTC, not a fixed offset.
	if created.Mode != "apply" || created.Timezone != "Europe/Paris" || len(created.Weekdays) != 3 || created.Weekdays[0] != 1 || created.StartMinute != 120 || created.EndMinute != 240 || created.Status != "active" || created.CreatedBy != "usr_planner" || created.NextOccurrence == nil || created.NextOccurrence.Location() != time.UTC {
		t.Fatalf("created: %s", raw)
	}
	for _, tc := range []struct{ code, body string }{
		{"invalid_mode", `{"mode":"auto","timezone":"UTC","weekdays":[1],"start_minute":0,"end_minute":60}`},
		{"invalid_timezone", `{"mode":"apply","timezone":"Mars/Olympus_Mons","weekdays":[1],"start_minute":0,"end_minute":60}`},
		{"invalid_timezone", `{"mode":"apply","timezone":"Local","weekdays":[1],"start_minute":0,"end_minute":60}`},
		{"invalid_weekdays", `{"mode":"apply","timezone":"UTC","weekdays":[],"start_minute":0,"end_minute":60}`},
		{"invalid_weekdays", `{"mode":"apply","timezone":"UTC","weekdays":[7],"start_minute":0,"end_minute":60}`},
		{"invalid_window", `{"mode":"apply","timezone":"UTC","weekdays":[1],"start_minute":600,"end_minute":610}`},
		{"invalid_window", `{"mode":"apply","timezone":"UTC","weekdays":[1],"start_minute":1380,"end_minute":60}`},
		{"invalid_window", `{"mode":"apply","timezone":"UTC","weekdays":[1],"start_minute":0,"end_minute":1441}`},
	} {
		code(send(h.admin, "PUT", url, tc.body, true, 400), tc.code)
	}
	send(h.admin, "PUT", url, `{"mode":"apply","timezone":"UTC","weekdays":[1],"start_minute":0,"end_minute":60,"created_by":"usr_viewer"}`, true, 400)
	// Another administrator edits: the policy now acts as them. Tomorrow's weekday only, so no
	// window before the edit can be recorded as missed at the tick below.
	daily := fmt.Sprintf(`{"mode":"plan_only","timezone":"UTC","weekdays":[%d],"start_minute":600,"end_minute":660}`, int(tomorrow().Weekday()))
	var edited store.UpdatePolicy
	if err := json.Unmarshal([]byte(send(other, "PUT", url, daily, true, 200)), &edited); err != nil || edited.ID != created.ID || edited.CreatedBy != "usr_other" || edited.Mode != "plan_only" {
		t.Fatalf("edited: %+v %v", edited, err)
	}
	// Every member reads; runs are bounded.
	var view struct {
		store.UpdatePolicy
		Runs []store.PolicyRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(send(viewer, "GET", url, "", false, 200)), &view); err != nil || view.ID != created.ID || view.Runs == nil || len(view.Runs) != 0 {
		t.Fatalf("read: %+v %v", view, err)
	}
	if got := send(viewer, "GET", url+"/runs?limit=100", "", false, 200); got != "[]\n" && got != "[]" {
		t.Fatalf("runs: %q", got)
	}
	for _, q := range []string{"0", "101", "x"} {
		send(viewer, "GET", url+"/runs?limit="+q, "", false, 400)
	}
	// Resume: 409 while active; a pause (the editor lost application.deploy) then resumes.
	code(send(other, "POST", url+"/resume", "", true, 409), "policy_not_paused")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_other", Role: store.RoleOperator, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	if runs := h.runs(t, "usr_planner"); len(runs) != 1 || runs[0].Outcome != store.RunPaused {
		t.Fatalf("runs: %+v", runs)
	}
	var runs []store.PolicyRun
	if err := json.Unmarshal([]byte(send(viewer, "GET", url+"/runs", "", false, 200)), &runs); err != nil || len(runs) != 1 {
		t.Fatalf("runs route: %+v %v", runs, err)
	}
	send(envAdmin, "POST", url+"/resume", "", true, 403)
	var resumed store.UpdatePolicy
	if err := json.Unmarshal([]byte(send(h.admin, "POST", url+"/resume", "", true, 200)), &resumed); err != nil || resumed.Status != "active" || resumed.ConsecutiveFailures != 0 || resumed.PausedReason != "" {
		t.Fatalf("resumed: %+v %v", resumed, err)
	}
	// Delete: administrators only; it takes the runs; a second delete is 404.
	send(viewer, "DELETE", url, "", true, 403)
	send(h.admin, "DELETE", url, "", true, 204)
	send(h.admin, "GET", url, "", false, 404)
	send(h.admin, "DELETE", url, "", true, 404)
	if runs := h.runs(t, "usr_planner"); len(runs) != 0 {
		t.Fatalf("runs outlived their policy: %+v", runs)
	}
	// Every write is audited on the policy.
	saves := 0
	for _, r := range policyAuditRows(t, h, h.admin) {
		if r.Action == "application.policy" && r.Result == "success" && strings.HasPrefix(r.Resource, h.appID()+"/policies/"+created.ID) {
			saves++
		}
	}
	if saves != 4 { // create, edit, resume, delete
		t.Fatalf("policy write rows: %d", saves)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -count=1 ./internal/api/ -run TestUpdatePolicyRoutes`
Expected: FAIL: `GET .../update-policy: 404` is met, then `PUT ... : 404 ... want 403` (the route does not exist).

- [ ] **Step 3: Implement the routes**

Create `internal/api/policy_handlers.go`:

```go
package api

import (
	"net/http"
	"strconv"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// policyView is what GET update-policy returns: the policy and its latest runs.
type policyView struct {
	*store.UpdatePolicy
	Runs []store.PolicyRun `json:"runs"`
}

func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	p, runs, err := s.store.Tenancy().ReadUpdatePolicy(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if p == nil {
		s.writeError(w, http.StatusNotFound, "This application has no update policy")
		return
	}
	s.writeJSON(w, http.StatusOK, policyView{UpdatePolicy: p, Runs: runs})
}

func (s *Server) handlePutUpdatePolicy(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input store.PolicyInput
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	p, created, err := s.store.Tenancy().PutUpdatePolicy(r.Context(), a, r.PathValue("application"), input)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, p)
}

func (s *Server) handleDeleteUpdatePolicy(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if err := s.store.Tenancy().DeleteUpdatePolicy(r.Context(), a, r.PathValue("application")); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResumeUpdatePolicy(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	p, err := s.store.Tenancy().ResumeUpdatePolicy(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, p)
}

// handlePolicyRuns lists runs newest first, 20 unless ?limit= names 1..100.
func (s *Server) handlePolicyRuns(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.tenantError(w, store.ErrInvalid)
			return
		}
		limit = n
	}
	runs, err := s.store.Tenancy().ListPolicyRuns(r.Context(), a, r.PathValue("application"), limit)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, runs)
}
```

In `internal/api/server.go` `routes()`, after the `POST .../updates/check` line add:

```go
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/{application}/update-policy", s.tenantRoute(s.handleUpdatePolicy))
	s.mux.HandleFunc("PUT /api/organizations/{organization}/environments/{environment}/applications/{application}/update-policy", s.tenantRoute(s.handlePutUpdatePolicy))
	s.mux.HandleFunc("DELETE /api/organizations/{organization}/environments/{environment}/applications/{application}/update-policy", s.tenantRoute(s.handleDeleteUpdatePolicy))
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications/{application}/update-policy/resume", s.tenantRoute(s.handleResumeUpdatePolicy))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/{application}/update-policy/runs", s.tenantRoute(s.handlePolicyRuns))
```

In `internal/api/tenant_handlers.go` `tenantError`, add before `case errors.Is(err, store.ErrInvalid):`:

```go
	case errors.Is(err, store.ErrInvalidTimezone):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "This server does not recognise that time zone", "code": "invalid_timezone"})
	case errors.Is(err, store.ErrInvalidWindow):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "A window lasts at least 15 minutes and ends by midnight", "code": "invalid_window"})
	case errors.Is(err, store.ErrInvalidWeekdays):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Choose one to seven distinct days, 0 (Sunday) to 6", "code": "invalid_weekdays"})
	case errors.Is(err, store.ErrInvalidMode):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Mode is apply or plan_only", "code": "invalid_mode"})
	case errors.Is(err, store.ErrPolicyNotPaused):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The update policy is not paused", "code": "policy_not_paused"})
```

- [ ] **Step 4: Run to verify it passes, on both drivers**

Run: `gofmt -w internal/api && go vet ./internal/api/ && go test -race -count=1 ./internal/api/ && PG=… go test -count=1 ./internal/api/`
Expected: PASS.

- [ ] **Step 5: DOX and commit**

`internal/api/AGENTS.md`, after the scheduler bullet (Task 3) add "- `policy_handlers.go`: `GET|PUT|DELETE .../applications/{application}/update-policy`, `POST .../update-policy/resume`, `GET .../update-policy/runs?limit=` (default 20, 1..100). `GET` answers 404 with no store error when there is no policy and otherwise the policy's fields plus `runs` (latest 20) and `next_occurrence` (UTC, null while paused). `PUT` is strict JSON `{mode, timezone, weekdays, start_minute, end_minute}`, 201 created / 200 edited; `tenantError` maps the four field errors to 400 `invalid_mode`, `invalid_timezone`, `invalid_weekdays`, `invalid_window` and `ErrPolicyNotPaused` to 409 `policy_not_paused`."

```bash
git add internal/api
git commit -m "feat(api): update-policy routes" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: `cmd/server` — start and wait for the scheduler; embed the zone database

**Files:**
- Modify: `cmd/server/main.go` (import `time/tzdata`; start `RunPolicies`; wait on it)
- Test: `cmd/server/tzdata_test.go`
- Docs: `AGENTS.md` (root; it owns `cmd/server`)

**Interfaces:**
- Consumes: `api.Server.RunPolicies` (Task 3), `waitForBackupWork`.
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Create `cmd/server/tzdata_test.go`:

```go
package main

import (
	"go/parser"
	"go/token"
	"testing"
)

// Update-policy windows are evaluated in IANA zones, and PUT refuses a zone that does not load.
// A bare binary on a host without zoneinfo must behave the same, so the server embeds the zone
// database. Deleting the import fails here.
func TestServerEmbedsTheTimeZoneDatabase(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range f.Imports {
		if im.Path.Value == `"time/tzdata"` && im.Name != nil && im.Name.Name == "_" {
			return
		}
	}
	t.Fatal(`cmd/server does not blank-import "time/tzdata"`)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -count=1 ./cmd/server/ -run TestServerEmbedsTheTimeZoneDatabase`
Expected: FAIL with `cmd/server does not blank-import "time/tzdata"`.

- [ ] **Step 3: Wire the scheduler and the import**

In `cmd/server/main.go`, add to the import block after `"time"`:

```go
	_ "time/tzdata" // update-policy zones load the same on every host
```

Replace

```go
	backupDone := make(chan struct{})
	go backupLoop(ctx, cfg, st, backupDone)
```

with

```go
	backupDone := make(chan struct{})
	go backupLoop(ctx, cfg, st, backupDone)
	policiesDone := make(chan struct{})
	go srv.RunPolicies(ctx, policiesDone)
```

and replace

```go
	waitForBackupWork(waitCtx, backupDone, func() { <-localDone; srv.WaitDetached() })
```

with

```go
	// A policy run in flight finishes before the store closes: its apply is recorded before its
	// frame leaves. A run's worst case (under 3 minutes) fits the same budget.
	waitForBackupWork(waitCtx, backupDone, func() { <-localDone; <-policiesDone; srv.WaitDetached() })
```

- [ ] **Step 4: Run to verify it passes**

Run: `gofmt -w cmd/server && go vet ./cmd/server/ && go test -race -count=1 ./cmd/server/ && go build ./...`
Expected: PASS, including `TestComposeGracePeriodCoversTheShutdownBudget` (budget unchanged: `docker-compose.yml`'s `stop_grace_period: 20m` still exceeds `shutdownTimeout + backupWaitTimeout`).

- [ ] **Step 5: DOX and commit**

Root `AGENTS.md`, the `cmd/server` paragraph: replace "`Shutdown`. Nothing writes into a closed store." with "`Shutdown`. `api.Server.RunPolicies`, the update-policy scheduler, starts beside `backupLoop` and closes its own `done` only when its loop has stopped and no policy run is in flight; `runServer` waits on it inside the same handler wait (a run's worst case is under 3 minutes, so the budget is unchanged). `main.go` blank-imports `time/tzdata` so policy zones load the same on every host (`TestServerEmbedsTheTimeZoneDatabase`). Nothing writes into a closed store."

```bash
git add cmd/server AGENTS.md
git commit -m "feat(server): run the update-policy scheduler and embed tzdata" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Web — the Update policy card

**Files:**
- Modify: `web/src/tenant.ts` (`UpdatePolicy`, `PolicyRun`, `canManagePolicies`)
- Modify: `web/src/components/ApplicationUpdates.tsx` (export `detailText` and `planBlockers`)
- Create: `web/src/components/ApplicationPolicy.tsx`
- Modify: `web/src/components/Applications.tsx` (role lookup; render the card for a selected application)
- Test: `web/src/components/ApplicationPolicy.test.tsx`, `web/src/components/Applications.test.tsx`
- Build: `web/dist` via `make build-web`
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes: Task 4's routes and JSON; `secureFetch` (`../api`), `useTenantResource`, `StateNotice`, `planBlockers`/`detailText` from `ApplicationUpdates.tsx`.
- Produces: `ApplicationPolicy({ base, admin })`, `RUN_OUTCOMES`, `runDetail`, `zoneChoices`, `browserZone`, `formatIn`.

- [ ] **Step 1: Write the failing tests**

Create `web/src/components/ApplicationPolicy.test.tsx`:

```tsx
import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ApplicationPolicy, RUN_OUTCOMES } from './ApplicationPolicy';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); document.cookie = 'ky_csrf=; Max-Age=0'; });

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
const policy = (over: Record<string, unknown> = {}) => ({ id: 'p', application_id: 'app', created_by: 'usr_a', mode: 'apply', timezone: 'Europe/Paris', weekdays: [1, 3], start_minute: 120, end_minute: 240, status: 'active', paused_reason: '', consecutive_failures: 0, created_at: '2026-09-24T10:00:00Z', updated_at: '2026-09-24T10:00:00Z', next_occurrence: '2026-09-28T00:00:00Z', runs: [], ...over });
const run = (over: Record<string, unknown>) => ({ id: 'r', policy_id: 'p', occurrence: '2026-09-24T00:00:00Z', started_at: '2026-09-24T00:00:05Z', finished_at: '2026-09-24T00:00:09Z', outcome: 'applied', deployment_id: '', detail: '', correlation_id: 'c', ...over });
const windowText = 'The window must end after it starts, last at least 15 minutes and end by midnight; windows cannot cross midnight.';
function stub(get: () => Response, write?: (url: string, init: RequestInit) => Response) {
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => init?.method ? (write ? write(url, init) : json({})) : get());
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}
const open = () => fireEvent.click(screen.getByRole('button', { name: 'Update policy' }));
const save = () => fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));

it('requests nothing until opened, then offers an administrator a new policy', async () => {
  const fetcher = stub(() => json({ error: 'none' }, 404));
  render(<ApplicationPolicy base="/app" admin />);
  expect(fetcher).not.toHaveBeenCalled();
  open();
  await screen.findByText('No update policy.');
  expect(fetcher.mock.calls[0][0]).toBe('/app/update-policy');
  expect(screen.getByRole('button', { name: 'Save policy' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Delete policy' })).toBeNull();
});

it('shows other members the policy without the editor', async () => {
  stub(() => json(policy()));
  render(<ApplicationPolicy base="/app" admin={false} />);
  open();
  await screen.findByText(/Mon, Wed 02:00–04:00 \(Europe\/Paris\)/);
  expect(screen.queryByRole('button', { name: 'Save policy' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Delete policy' })).toBeNull();
});

it('refuses an empty day set and a short or reversed window before sending', async () => {
  const fetcher = stub(() => json({}, 404));
  render(<ApplicationPolicy base="/app" admin />);
  open();
  await screen.findByText('No update policy.');
  for (const day of ['Mon', 'Tue', 'Wed', 'Thu', 'Fri']) fireEvent.click(screen.getByLabelText(day));
  save();
  expect(await screen.findByText('Choose at least one day.')).toBeTruthy();
  fireEvent.click(screen.getByLabelText('Sat'));
  fireEvent.change(screen.getByLabelText('Window start'), { target: { value: '10:00' } });
  fireEvent.change(screen.getByLabelText('Window end'), { target: { value: '10:10' } });
  save();
  expect(await screen.findByText(windowText)).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Window end'), { target: { value: '09:00' } });
  save();
  expect(await screen.findByText(windowText)).toBeTruthy();
  expect(fetcher.mock.calls.every((c) => !c[1]?.method)).toBe(true);
});

it('saves with the CSRF header in the shape the server expects; 00:00 ends at midnight', async () => {
  document.cookie = 'ky_csrf=csrf';
  let saved: unknown = null;
  vi.spyOn(Intl, 'supportedValuesOf').mockReturnValue(['Europe/Paris', 'Asia/Tokyo']);
  const fetcher = stub(() => saved ? json(policy()) : json({}, 404), (_url, init) => { saved = JSON.parse(String(init.body)); return json(policy(), 201); });
  render(<ApplicationPolicy base="/app" admin />);
  open();
  await screen.findByText('No update policy.');
  fireEvent.change(screen.getByLabelText('Mode'), { target: { value: 'apply' } });
  fireEvent.change(screen.getByLabelText('Window start'), { target: { value: '23:00' } });
  fireEvent.change(screen.getByLabelText('Window end'), { target: { value: '00:00' } });
  fireEvent.change(screen.getByLabelText('Time zone'), { target: { value: 'Asia/Tokyo' } });
  save();
  await screen.findByText(/Plan and apply ·/);
  const put = fetcher.mock.calls.find((c) => c[1]?.method === 'PUT');
  expect(put?.[0]).toBe('/app/update-policy');
  expect(new Headers(put?.[1]?.headers).get('X-CSRF-Token')).toBe('csrf');
  expect(saved).toEqual({ mode: 'apply', timezone: 'Asia/Tokyo', weekdays: [1, 2, 3, 4, 5], start_minute: 1380, end_minute: 1440 });
});

it('maps each refusal code to fixed text and never shows server text', async () => {
  const texts: Record<string, string> = {
    invalid_timezone: 'The server does not recognise this time zone. Choose another.',
    invalid_window: windowText,
    invalid_weekdays: 'Choose at least one day.',
    invalid_mode: 'Choose a mode.',
  };
  let code = '';
  stub(() => json({}, 404), () => json({ error: 'secret-canary', code }, 400));
  render(<ApplicationPolicy base="/app" admin />);
  open();
  await screen.findByText('No update policy.');
  for (const [c, text] of Object.entries(texts)) {
    code = c;
    save();
    expect(await screen.findByText(text)).toBeTruthy();
  }
  code = 'made_up';
  save();
  expect(await screen.findByText('The policy was refused as invalid.')).toBeTruthy();
  expect(document.body.textContent).not.toContain('secret-canary');
});

// Review Focus 2: the browser's list lacks aliases and UTC; a saved zone must survive the editor.
it('keeps a saved zone this browser does not list, and defaults a new policy to the browser zone', async () => {
  vi.spyOn(Intl, 'supportedValuesOf').mockReturnValue(['Europe/Paris']);
  stub(() => json(policy({ timezone: 'US/Eastern' })));
  render(<ApplicationPolicy base="/app" admin />);
  open();
  const saved = await screen.findByLabelText('Time zone') as HTMLSelectElement;
  expect(saved.value).toBe('US/Eastern');
  expect(Array.from(saved.options).map((o) => o.value)).toContain('Europe/Paris');
  cleanup();
  const real = new Intl.DateTimeFormat().resolvedOptions();
  vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions').mockReturnValue({ ...real, timeZone: 'Asia/Tokyo' });
  stub(() => json({}, 404));
  render(<ApplicationPolicy base="/app" admin />);
  open();
  expect(((await screen.findByLabelText('Time zone')) as HTMLSelectElement).value).toBe('Asia/Tokyo');
});

it('renders runs through fixed tables only', async () => {
  const deployment = '3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b';
  stub(() => json(policy({ runs: [
    run({ id: 'a', outcome: 'applied', deployment_id: deployment }),
    run({ id: 'b', outcome: 'blocked', detail: 'configuration_unsupported,made_up_blocker' }),
    run({ id: 'c', outcome: 'failed', detail: 'unauthorized' }),
    run({ id: 'd', outcome: 'skipped_missed', detail: 'the server was not running during this window' }),
    run({ id: 'e', outcome: 'failed', detail: 'secret-canary' }),
    run({ id: 'f', outcome: 'exploded' }),
  ] })));
  render(<ApplicationPolicy base="/app" admin={false} />);
  open();
  await screen.findByText(RUN_OUTCOMES.applied);
  expect(screen.getByText('3f2b1c9e').getAttribute('title')).toBe(deployment);
  expect(screen.getByText(/configuration the definition cannot express/)).toBeTruthy();
  expect(screen.getByText('The registry refused the credentials.')).toBeTruthy();
  expect(screen.getByText('The server was not running during this window.')).toBeTruthy();
  expect(screen.getByText('Unrecognised outcome')).toBeTruthy();
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(document.body.textContent).not.toContain('made_up_blocker');
});

it('shows why a policy paused and lets only an administrator resume it', async () => {
  document.cookie = 'ky_csrf=csrf';
  let paused = true;
  const get = () => json(policy(paused ? { status: 'paused', paused_reason: 'three consecutive windows failed', next_occurrence: null, consecutive_failures: 3 } : {}));
  const fetcher = stub(get, () => { paused = false; return json(policy()); });
  render(<ApplicationPolicy base="/app" admin />);
  open();
  await screen.findByText(/Three windows in a row failed/);
  fireEvent.click(screen.getByRole('button', { name: 'Resume policy' }));
  await screen.findByText(/Next window/);
  expect(fetcher.mock.calls.find((c) => c[1]?.method === 'POST')?.[0]).toBe('/app/update-policy/resume');
  cleanup();
  paused = true;
  stub(get);
  render(<ApplicationPolicy base="/app" admin={false} />);
  open();
  await screen.findByText(/Three windows in a row failed/);
  expect(screen.queryByRole('button', { name: 'Resume policy' })).toBeNull();
});

it('shows the next window in the policy zone and the browser zone', async () => {
  stub(() => json(policy({ timezone: 'Asia/Tokyo', next_occurrence: '2026-09-28T00:00:00Z' })));
  render(<ApplicationPolicy base="/app" admin={false} />);
  open();
  const status = await screen.findByText(/Next window/);
  expect(status.textContent).toContain('09:00'); // 00:00 UTC is 09:00 in Tokyo
  expect(status.textContent).toContain('your time');
});
```

Append to `web/src/components/Applications.test.tsx`:

```tsx
it('offers the update-policy editor to organization administrators only', async () => {
  for (const [role, editor] of [['organization_admin', true], ['environment_admin', false]] as const) {
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      if (url === '/api/organizations') return json([{ id: 'a', name: 'A', role }]);
      if (url.endsWith('/update-policy')) return json({ error: 'none' }, 404);
      if (url.includes('/revisions/')) return json({ digest: 'digest', spec: { services: [] } });
      if (url.endsWith('/instances')) return json([]);
      return json([{ id: 'app', name: 'shop', latest_revision: 1 }]);
    }));
    render(<Applications org="a" env="env" />);
    fireEvent.click(await screen.findByRole('button', { name: 'View configuration for shop' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Update policy' }));
    await screen.findByText('No update policy.');
    if (editor) expect(await screen.findByRole('button', { name: 'Save policy' })).toBeTruthy();
    else expect(screen.queryByRole('button', { name: 'Save policy' })).toBeNull();
    cleanup();
  }
});
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd web && npx vitest run src/components/ApplicationPolicy.test.tsx src/components/Applications.test.tsx`
Expected: FAIL: `Failed to resolve import "./ApplicationPolicy"`.

- [ ] **Step 3: Implement**

In `web/src/tenant.ts`, after the `UpdateCheck` interface add:

```ts
export interface PolicyRun { id: string; policy_id: string; occurrence: string; started_at: string; finished_at: string | null; outcome: string; deployment_id: string; detail: string; correlation_id: string }
export interface UpdatePolicy { id: string; application_id: string; created_by: string; mode: string; timezone: string; weekdays: number[]; start_minute: number; end_minute: number; status: string; paused_reason: string; consecutive_failures: number; created_at: string; updated_at: string; next_occurrence: string | null; runs?: PolicyRun[] }
```

and after `canExec` add:

```ts
// Mirrors permissions.Allows(role, ApplicationPolicy): only organization admins edit update policies.
export const canManagePolicies = (role: string | undefined) => role === 'organization_admin';
```

In `web/src/components/ApplicationUpdates.tsx`, change `const detailText: Record<string, string> = {` to `export const detailText: Record<string, string> = {` and `const planBlockers: Record<string, string> = {` to `export const planBlockers: Record<string, string> = {`.

Create `web/src/components/ApplicationPolicy.tsx`:

```tsx
import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource, type PolicyRun, type UpdatePolicy } from '../tenant';
import { StateNotice } from './StateNotice';
import { detailText, planBlockers } from './ApplicationUpdates';

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MODES: Record<string, string> = { apply: 'Plan and apply', plan_only: 'Plan only; apply by hand' };
export const RUN_OUTCOMES: Record<string, string> = {
  '': 'Running',
  skipped_missed: 'Skipped: server not running',
  skipped_busy: 'Skipped: busy',
  no_update: 'No update',
  planned: 'Planned; waiting to be applied',
  applied: 'Applied',
  blocked: 'Blocked',
  failed: 'Failed',
  paused: 'Paused the policy',
};
// The scheduler's own sentences, as a run's detail or a pause reason carries them.
const SENTENCES: Record<string, string> = {
  'the server was not running during this window': 'The server was not running during this window.',
  'another deployment occupied the window': 'Another deployment or plan occupied the window.',
  "the policy's creator no longer holds application.deploy": 'The administrator who last saved this policy can no longer deploy. Save the policy to make it act as you, then resume it.',
  'the server restarted during the run': 'The server restarted during the run.',
  'three consecutive windows failed': 'Three windows in a row failed. Fix the cause, then resume.',
};
const CODES: Record<string, string> = {
  ...detailText,
  not_adopted: 'The application is no longer adopted.',
  mapping_required: 'Map the services first.',
  adoption_changed: 'Adoption or inventory changed during the run.',
  deployment_in_progress: 'A deployment was being applied.',
  endpoint_offline: 'The endpoint was not connected.',
  not_sent: 'The endpoint disconnected before the deployment was sent.',
  check_in_progress: 'An update check or update plan was already running.',
  too_many_checks: 'Too many registry checks were running.',
  invalid: 'The run was refused as invalid.',
  error: 'The run failed on the server; check the server log.',
};
const INVALID: Record<string, string> = {
  invalid_mode: 'Choose a mode.',
  invalid_timezone: 'The server does not recognise this time zone. Choose another.',
  invalid_weekdays: 'Choose at least one day.',
  invalid_window: 'The window must end after it starts, last at least 15 minutes and end by midnight; windows cannot cross midnight.',
};
// Server strings never render raw: unknown keys show nothing.
const fixed = (table: Record<string, string>, key: string) => Object.hasOwn(table, key) ? table[key] : '';
const hhmm = (m: number) => `${String(Math.floor(m / 60) % 24).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`;
const minutes = (v: string) => { const [h, m] = v.split(':').map(Number); return h * 60 + m; };

export const browserZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone;
// zoneChoices is the browser's zone list plus the zones in keep: a zone the server accepts but
// this browser does not list (UTC, aliases) must never be swapped silently.
export function zoneChoices(keep: string[]): string[] {
  return Array.from(new Set([...keep, browserZone(), ...Intl.supportedValuesOf('timeZone')].filter(Boolean)));
}
// formatIn shows an instant in zone, or in UTC when this browser does not know the zone.
export function formatIn(iso: string, zone: string): string {
  try {
    return new Intl.DateTimeFormat(undefined, { timeZone: zone, weekday: 'short', year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' }).format(new Date(iso));
  } catch {
    return `${iso} (UTC)`;
  }
}
// runDetail renders a stored detail through the fixed tables only.
export function runDetail(run: PolicyRun): string {
  if (run.outcome === 'blocked') return run.detail.split(',').map((b) => fixed(planBlockers, b)).filter(Boolean).join(' ');
  return fixed(SENTENCES, run.detail) || fixed(CODES, run.detail);
}
function problem(days: number[], start: number, end: number): string {
  if (days.length === 0) return INVALID.invalid_weekdays;
  if (!(end - start >= 15)) return INVALID.invalid_window;
  return '';
}

type Props = { base: string; admin: boolean };

export function ApplicationPolicy(props: Props) {
  const [open, setOpen] = useState(false);
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" onClick={() => setOpen(!open)}>{open ? 'Close update policy' : 'Update policy'}</button>
    {open && <PolicyView key={props.base} {...props} />}
  </section>;
}

function PolicyView({ base, admin }: Props) {
  const url = `${base}/update-policy`;
  const policy = useTenantResource<UpdatePolicy>(url);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const write = async (method: string, path: string, body?: unknown) => {
    setBusy(true); setMessage('');
    try {
      const r = await secureFetch(path, { method, headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
      if (r.ok) { policy.reload(); return; }
      const payload: unknown = await r.json().catch(() => null);
      const code = payload && typeof payload === 'object' ? (payload as { code?: unknown }).code : undefined;
      if (r.status === 400) setMessage((typeof code === 'string' && fixed(INVALID, code)) || 'The policy was refused as invalid.');
      else if (r.status === 403) setMessage('Only organization administrators can change update policies.');
      else if (r.status === 409 && code === 'policy_not_paused') setMessage('The policy is not paused.');
      else if (r.status === 404) setMessage('The policy or the application no longer exists. Refresh.');
      else setMessage('The change was refused or its outcome is unknown. Refresh before trying again.');
    } catch { setMessage('Offline: the server could not be reached.'); }
    finally { setBusy(false); }
  };
  if (policy.state !== 'ready' && policy.state !== 'notfound') return <StateNotice state={policy.state} onRetry={policy.reload} />;
  const p = policy.state === 'ready' ? policy.data : null;
  return <>
    <p>A policy checks this application's images in a weekly maintenance window and plans, or plans and applies, what changed, acting as the administrator who last saved it. Every run is audited.</p>
    {p ? <PolicyStatus policy={p} admin={admin} busy={busy} onResume={() => void write('POST', `${url}/resume`)} /> : <p>No update policy.</p>}
    {admin && <PolicyEditor key={p?.updated_at ?? 'new'} policy={p} busy={busy} onSave={(body) => void write('PUT', url, body)} onDelete={() => { if (window.confirm('Delete this update policy and its run history? Deployments it made are kept.')) void write('DELETE', url); }} />}
    {message && <p role="alert">{message}</p>}
    {p && <RunList runs={p.runs ?? []} zone={p.timezone} />}
  </>;
}

function PolicyStatus({ policy, admin, busy, onResume }: { policy: UpdatePolicy; admin: boolean; busy: boolean; onResume: () => void }) {
  const schedule = `${policy.weekdays.map((d) => DAYS[d] ?? '').join(', ')} ${hhmm(policy.start_minute)}–${hhmm(policy.end_minute)} (${policy.timezone})`;
  return <div className="dr-stack">
    <p>{fixed(MODES, policy.mode)} · {schedule}</p>
    {policy.status === 'paused' ? <>
      <p role="status">Paused. {fixed(SENTENCES, policy.paused_reason)}</p>
      {admin && <button type="button" disabled={busy} onClick={onResume}>Resume policy</button>}
    </> : <p role="status">Active. {policy.next_occurrence ? `Next window: ${formatIn(policy.next_occurrence, policy.timezone)} (your time: ${formatIn(policy.next_occurrence, browserZone())}).` : 'No upcoming window.'}{policy.consecutive_failures > 0 ? ` ${policy.consecutive_failures} failed window(s) in a row; the policy pauses at 3.` : ''}</p>}
  </div>;
}

function PolicyEditor({ policy, busy, onSave, onDelete }: { policy: UpdatePolicy | null; busy: boolean; onSave: (body: unknown) => void; onDelete: () => void }) {
  const [mode, setMode] = useState(policy?.mode ?? 'plan_only');
  const [days, setDays] = useState<number[]>(policy?.weekdays ?? [1, 2, 3, 4, 5]);
  const [start, setStart] = useState(hhmm(policy?.start_minute ?? 120));
  const [end, setEnd] = useState(hhmm(policy?.end_minute ?? 240));
  const [zone, setZone] = useState(policy?.timezone ?? browserZone());
  const [error, setError] = useState('');
  const toggle = (d: number) => setDays(days.includes(d) ? days.filter((x) => x !== d) : [...days, d].sort((a, b) => a - b));
  const submit = () => {
    const s = minutes(start), e = minutes(end) || 1440; // 00:00 as the end is midnight
    const p = problem(days, s, e);
    setError(p);
    if (!p) onSave({ mode, timezone: zone, weekdays: days, start_minute: s, end_minute: e });
  };
  return <form className="dr-stack" onSubmit={(ev) => { ev.preventDefault(); submit(); }}>
    <label>Mode<select value={mode} onChange={(e) => setMode(e.target.value)} disabled={busy}>{Object.entries(MODES).map(([k, v]) => <option key={k} value={k}>{v}</option>)}</select></label>
    <fieldset><legend>Days</legend>{DAYS.map((name, d) => <label key={name}><input type="checkbox" checked={days.includes(d)} onChange={() => toggle(d)} disabled={busy} />{name}</label>)}</fieldset>
    <label>Window start<input type="time" value={start} onChange={(e) => setStart(e.target.value)} required disabled={busy} /></label>
    <label>Window end<input type="time" value={end} onChange={(e) => setEnd(e.target.value)} required disabled={busy} /></label>
    <p>Times are wall-clock times in the chosen zone. A window lasts at least 15 minutes and cannot cross midnight; an end of 00:00 means midnight.</p>
    <label>Time zone<select value={zone} onChange={(e) => setZone(e.target.value)} disabled={busy}>{zoneChoices([policy?.timezone ?? '', zone]).map((z) => <option key={z} value={z}>{z}</option>)}</select></label>
    {error && <p role="alert">{error}</p>}
    <button disabled={busy}>Save policy</button>
    {policy && <button type="button" className="btn-danger" disabled={busy} onClick={onDelete}>Delete policy</button>}
  </form>;
}

function RunList({ runs, zone }: { runs: PolicyRun[]; zone: string }) {
  if (runs.length === 0) return <p>No runs yet.</p>;
  return <table>
    <thead><tr><th>Window</th><th>Outcome</th><th>Detail</th><th>Deployment</th></tr></thead>
    <tbody>{runs.map((r) => <tr key={r.id}>
      <td>{formatIn(r.occurrence, zone)}</td>
      <td><span className="badge">{Object.hasOwn(RUN_OUTCOMES, r.outcome) ? RUN_OUTCOMES[r.outcome] : 'Unrecognised outcome'}</span></td>
      <td>{runDetail(r)}</td>
      <td>{/^[0-9a-f-]{36}$/.test(r.deployment_id) ? <code title={r.deployment_id}>{r.deployment_id.slice(0, 8)}</code> : '—'}</td>
    </tr>)}</tbody>
  </table>;
}
```

In `web/src/components/Applications.tsx`:
- add `import { ApplicationPolicy } from './ApplicationPolicy';` after the `ApplicationUpdates` import, and change `import { useTenantResource } from '../tenant';` to `import { canManagePolicies, useTenantResource, type MemberOrganization } from '../tenant';`
- after `const instances = useTenantResource<ApplicationInstance[]>(`${base}/instances`);` add:
  ```tsx
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const policyAdmin = canManagePolicies((Array.isArray(organizations.data) ? organizations.data : []).find((o) => o.id === org)?.role);
  ```
- in the selected-draft fragment, directly before its closing `</>}` (after the `instances.data?.filter(...).map(...)` block), add:
  ```tsx
  {instances.state === 'ready' && <ApplicationPolicy key={`policy/${draft.id}`} base={`${base}/${encodeURIComponent(draft.id)}`} admin={policyAdmin} />}
  ```

- [ ] **Step 4: Run the web suite and rebuild**

Run: `(cd web && npx vitest run) && make build-web && git status --short web/dist`
Expected: every test passes, the typecheck in `npm run build` passes, and `web/dist` shows the rebuilt bundle.

- [ ] **Step 5: DOX and commit**

`web/AGENTS.md`, Local Contracts, after the Updates bullet add: "- An adopted or draft application's configuration shows an \"Update policy\" toggle (`ApplicationPolicy.tsx`) that requests nothing until opened, then reads `GET .../update-policy` (404 is \"No update policy.\"). The status line shows mode, days, window and zone, then \"Active. Next window: …\" in the policy's zone and the browser's (`formatIn`, falling back to UTC for a zone the browser cannot format), or \"Paused.\" with the reason's fixed text; the run list shows window, a `RUN_OUTCOMES` badge (unknown: \"Unrecognised outcome\"), a detail rendered only through fixed tables (`planBlockers` per comma-separated blocker, `detailText` and the store codes, the scheduler's sentences; anything else nothing) and the deployment's first 8 characters (full ID in `title`). Only `organization_admin` (`canManagePolicies`, from `/api/organizations` in `Applications.tsx`) sees the editor, Delete (confirmed) and Resume: mode, weekday checkboxes, start and end `<input type=\"time\">` (end 00:00 is 1440), and a zone `<select>` from `Intl.supportedValuesOf('timeZone')` that always keeps the saved and the browser's zone (`zoneChoices`). The editor refuses no day and a window under 15 minutes or reversed before sending; 400 codes and 403/404/409 `policy_not_paused` map to fixed texts." In `## Verification`, extend the parenthesis with "; `src/components/ApplicationPolicy.test.tsx` covers the unopened card, the admin-only editor, client validation, the PUT body with CSRF and 00:00 as 1440, every 400 code as fixed text, a saved zone the browser does not list, runs through fixed tables, pause and resume, and the next window in both zones".

```bash
git add web
git commit -m "feat(web): update policy card, editor and run list" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Documents, the DOX pass and the full gate

**Files:**
- Modify: `docs/application-schema.md` (Tables; new Update policies section; Update detection's "no timer" line)
- Modify: `docs/authorization-matrix.md` (the M7b placeholder row)
- Modify: `docs/threat-model.md` (one threat row, one operating constraint)
- Modify: `README.md` (Update policies section)
- Modify: `KyYard-Implementation-Plan.md` §8
- Docs: every `AGENTS.md` touched in Tasks 1–6 re-read against the code

**Interfaces:**
- Consumes: everything above. Produces: no code.

- [ ] **Step 1: `docs/application-schema.md`**

In `## Tables` replace "`update_policies`, `maintenance_windows`." with "`update_policies` (the maintenance window is on the policy row), `policy_runs`." In `## Update detection` replace "On demand, no timer (periodic checks are M7b policies)," with "On demand, or by an update policy's run (Update policies, below),". Insert before `## Kubernetes (M8)`:

```markdown
## Update policies (M7b PR 18, implemented)

- Migration 31 adds `update_policies` (one per application, composite FK to `applications`, cascade) and `policy_runs` (cascade from the policy; `UNIQUE (policy_id, occurrence)` is the idempotency key: one attempt per window across restarts and ticks). A policy has a `mode` (`apply` or `plan_only`), an IANA `timezone` (loaded with `time.LoadLocation`; the server embeds `time/tzdata`), a weekday set (0-6, Sunday 0) and a window `start_minute`..`end_minute` of at least 15 minutes that never crosses midnight. Deleting a policy deletes its runs; deployments it made stay.
- Occurrences are wall clock: on the zone's calendar date the window starts at `time.Date(y, m, d, start/60, start%60, 0, 0, loc)`; a start the zone skips (a DST gap, for example 02:30 on 2026-03-08 in New York) has no occurrence that day, an overlap (01:30 on 2026-11-01) is one occurrence at its first mapping, and the key stored is the start in UTC.
- The scheduler runs inside the server (`api.Server.RunPolicies`), one tick a minute. A window that ended with no run, after the policy was last saved or resumed, is recorded `skipped_missed` ("the server was not running during this window"). An open window runs when the application's endpoint has no `applying` or unexpired `planned` deployment and no policy run, and fewer than two runs are in flight server-wide; otherwise it waits, and a window that closes still waiting is `skipped_busy` ("another deployment occupied the window"). No retry inside a window.
- A run acts as the policy's `created_by` (whoever saved it last) under one correlation ID: `application.deploy` is re-checked, then the per-application guard and a registry slot (30 s at most, else `skipped_busy`), the update check (no `update_available` is `no_update`; only registry errors is `failed` with the error code), live inspections and a plan pinning every `update_available` service (a blocker is `blocked` with the blocker codes). `plan_only` stops at `planned`, waiting for a click; `apply` applies and sends the frame like **Apply deployment** (`applied`, or `failed` `not_sent`). Settlement is the deployment's own. Every run has an `application.policy.run` audit row as `system`, and its check, plan and apply rows carry the run's correlation ID.
- `blocked` and `failed` count; `applied`, `planned` and `no_update` reset the count; skips leave it. Three consecutive failed windows pause the policy ("three consecutive windows failed", audit `application.policy.paused`). A creator who lost `application.deploy` (role change, disabled, removed or deleted) pauses it at once without counting. **Resume** reopens it with the count at zero; saving the policy makes it act as the saver. A run interrupted by a crash is failed at the next start ("the server restarted during the run") and counts.
- Routes, under `.../applications/{application}`: `GET /update-policy` (`application.read`; the policy, `next_occurrence` in UTC, null while paused, and the last 20 runs; 404 without one), `PUT /update-policy` (`application.policy`; 201/200; 400 `invalid_mode`, `invalid_timezone`, `invalid_weekdays`, `invalid_window`), `DELETE /update-policy` (204), `POST /update-policy/resume` (200; 409 `policy_not_paused`), `GET /update-policy/runs?limit=` (newest first, 1..100). Writes are audited as `application.policy` on `<app>/policies/<policy>`.
```

- [ ] **Step 2: `docs/authorization-matrix.md`**

Replace the row beginning "| `update_policy.manage` and `maintenance_window.manage` (M7b)" with:

```markdown
| `application.policy` (create, edit, delete, resume an update policy; implemented, M7b) | ✓ | – | – | – | – | no | success and failure on `<app>/policies/<policy>`; reading a policy and its runs is `application.read` |
| Update-policy runs (implemented, M7b) | the scheduler acts as the policy's `created_by` with `application.deploy` re-checked on every step | | | | | registry credential as for a manual update | the run's check, plan and apply rows under the creator and one correlation ID; `application.policy.run` and `application.policy.paused` as `system` |
```

- [ ] **Step 3: `docs/threat-model.md`**

Add a row to the threats table after "A container reconfigured between plan and replacement":

```markdown
| Unattended deployment outliving its author's authority | Implemented (M7b PR 18): a policy acts as the organization administrator who last saved it and nobody else; every run re-checks `application.deploy` for that user in the store before and during the run, and a lapse (role change, disabled account, removed membership, deleted account) pauses the policy at once, audited, with nothing checked, planned or applied. Only `organization_admin` holds `application.policy`. Each window runs at most once (`UNIQUE (policy_id, occurrence)`), three failed windows in a row pause the policy, and a run interrupted by a crash is failed rather than resumed. Residual: between one run's authorization and its apply (seconds) a revocation is caught only by the apply's own re-authorization | `TestPolicyPausesWhenTheCreatorLosesTheRole`, `TestPolicyPausesWhenTheCreatorIsDeleted`, `TestPolicyRunPausesWhenItsCreatorLostAccess`, `TestPolicyPausesAfterThreeFailedWindows`, `TestSchedulePolicies`, `TestReconcileAfterStartFailsInterruptedPolicyRuns`, `TestUpdatePolicyIsOrganizationAdminOnly` |
```

Under `## Operating constraints` add: "- Update policies run inside one server process; running two servers against one database is not supported, and the per-window key keeps a second process from repeating a window but not from overlapping another's checks."

- [ ] **Step 4: `README.md`**

Append at the end:

```markdown
## Update policies

An organization administrator can give an adopted, mapped application an **Update policy**:
a weekly maintenance window (days, start and end in a time zone you choose) and a mode. In
each window the server checks the application's images and, when a registry has a newer
image, plans the update (**Plan only**, which waits for someone to apply it) or plans and
applies it (**Plan and apply**). The policy acts as the administrator who last saved it: if
that person can no longer deploy, the policy pauses itself instead. Three failed windows in a
row also pause it; **Resume** starts it again. Windows are wall-clock times: a start that a
daylight-saving jump skips does not run that day, and a window cannot cross midnight. A window
missed while the server was down is recorded, not run late. Every run, and every step it takes,
is in the audit log under one correlation ID.

The scheduler runs inside the server; there is nothing extra to deploy. The binary embeds the
time-zone database (`time/tzdata`), so a bare binary on a host without `/usr/share/zoneinfo`
evaluates zones the same way as the container.
```

- [ ] **Step 5: `KyYard-Implementation-Plan.md` §8**

Replace the line "Next: M7b (automated update policies, maintenance windows, health validation and eligible rollback)." with:

```markdown
Implemented M7b PR 18 (`feat/update-policies`): per-application update policies with a weekly maintenance window in an IANA zone (wall clock; a DST-skipped start does not run, an overlap runs once), modes `apply` and `plan_only`, created and edited by organization administrators (`application.policy`). A scheduler inside the server runs each window once (`UNIQUE (policy_id, occurrence)`), as the policy's last editor with `application.deploy` re-checked, one run per endpoint and two server-wide, recording missed and busy windows; three failed windows or a lapsed creator pause the policy until resumed; a crash mid-run fails the run at the next start. Spec `docs/superpowers/specs/2026-09-25-update-policies-design.md`.

Next: M7b PR 19 (health validation and eligible rollback).
```

- [ ] **Step 6: DOX closeout**

Re-read every `AGENTS.md` on the paths changed in Tasks 1–6 (`internal/permissions`, `internal/store`, `internal/api`, `web`, root for `cmd/server`) against the code as merged: each bullet names the tables, the outcome vocabulary, the sentences, the concurrency numbers, the routes and the codes exactly as implemented. `grep -rn "maintenance_window" --include=AGENTS.md --include=*.md docs AGENTS.md internal web` prints nothing outside `docs/superpowers`. No `AGENTS.md` was created, moved or removed, so every Child DOX Index is unchanged; root Verification already runs everything added here (`make ci` covers the new Go and web tests; `make test-postgres` the PostgreSQL store and API suites).

- [ ] **Step 7: Full gate**

Run: `gofmt -l internal cmd && make ci && PG=… make test-postgres`
Expected: `gofmt -l` prints nothing; `make ci` (tidy-check, lint, race tests, web tests, smoke) and the PostgreSQL suite pass.

- [ ] **Step 8: Commit**

```bash
git add docs README.md KyYard-Implementation-Plan.md AGENTS.md internal web
git commit -m "docs: update policies, the scheduler and its authorization" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```
