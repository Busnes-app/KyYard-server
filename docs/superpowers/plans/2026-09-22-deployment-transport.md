# Deployment Transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a persisted deployment plan executable end to end: apply route → `deployment.apply` frame with resolved values → agent runs `docker.Client.Deploy` on a session-independent context and persists the result before sending → server settles the row, rebinds adopted resources, advances the instance's current revision → UI shows per-step outcomes.

**Architecture:** Migration 24 gives `deployments` its state machine and result column and `application_instances` its current/previous revision. `ApplyDeployment` builds the frame inside the locked tenant transaction and moves the row to `applying`; `SettleDeployment` is the single writer for outcomes. The agent gets `Options.Deploy`, a `deployments.json` ledger and a one-slot runner off the session loop. `internal/api` adds the route, the `deployment.result` frame path and abandon-on-disconnect. Both agents wire `engine.Deploy`.

**Tech Stack:** Go 1.25, SQLite/PostgreSQL inline migrations, coder/websocket agent transport, React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-22-deployment-transport-design.md`

## Global Constraints

- Worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/deployment-transport`, branch `feat/deployment-transport`. Never edit the root checkout; never use bare `git stash`.
- Store changes are tested on SQLite and PostgreSQL (`KY_TEST_POSTGRES_DSN=postgres://postgres:<pw>@127.0.0.1:15440/kyyard?sslmode=disable`; password from `docker inspect kyyard-access-pg`, never printed).
- `deployments.state` values: `planned`, `applying`, `succeeded`, `failed`, `denied`, `timed_out`, `unknown`. `unknown` is non-terminal: a later result may settle it. Everything else is first answer wins.
- At most one row per instance in (`planned`, `applying`): partial unique index `idx_deployments_live`.
- Apply deadline: `now + 10 * time.Minute` (`DeploymentApplyDeadline`). Sweep grace: 2 minutes past `deadline`.
- The request (with env values) exists only in memory on both sides; never logged, never persisted, never in a response body. Results never carry env values (PR A guarantees).
- Agent: one deployment at a time; `deployments.json` in `CommandDir` (or `IdentityDir`), ≤20 entries, 24 h, 0600, temp-file rename; written **before** the result is sent; all entries younger than 24 h re-sent after every `hello` in a non-pending session.
- Frame caps: request ≤ `protocol.MaxDeploymentRequestBytes` (192 KiB), result ≤ `protocol.MaxDeploymentResultBytes` (160 KiB); the result frame is exempt from `maxControlPayload` up to its own cap.
- Permission for apply: `application.deploy` (org admin, env admin, developer). `secret.use` is implied; the apply audit row is the record.
- `gofmt -w` touched Go files; commits end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. Two applies raced on one plan: the CAS `planned → applying` must let exactly one through; the other gets `ErrAdoptionChanged` (409), never a second frame. Task 2 test "apply twice".
2. A result for a deployment ID that exists but belongs to another endpoint must change nothing (scoped by `endpoint_id`). Task 2 test "foreign endpoint result" and Task 4 socket test.
3. Settle on `succeeded` when one replaced container ID no longer has an `application_resources` row (released and re-adopted in between): the rebind must fail closed for that service and leave the row in its prior state rather than half-writing. Task 2 test "rebind missing resource".
4. Agent restart mid-run: the run dies with the process; the next session has no persisted result, the server row stays `applying` until the sweep marks it `unknown`; no duplicate run occurs because no frame is re-sent by the server. Task 3 test "no replay without ledger entry" plus Task 2 "prune sweep".
5. Frontend polling must stop on any terminal state, on unmount, and after 12 minutes. Task 5 test "polling stops".

---

### Task 1: Migration 24, row model, live-row rules

**Files:**
- Modify: `internal/store/migrations/migrations.go` (append version 24)
- Modify: `internal/store/tenancy_test.go:211` (add `24` to the version list; `DROP TABLE deployments` is already first)
- Modify: `internal/store/store.go` (errors: `ErrDeploymentInProgress`; Tenancy interface additions listed in Task 2)
- Modify: `internal/store/application_deployment.go` (types, scan, `PlanDeployment`, `ReadDeployment`/`ListDeployments`)
- Modify: `internal/store/application_adoption.go` (`ApplicationInstance` gains `CurrentRevision`, `PreviousRevision`; `ListApplicationInstances` selects them; `ReleaseApplication` rules)
- Test: `internal/store/deployment_test.go`

**Interfaces:**
- Produces:

```go
var ErrDeploymentInProgress = errors.New("a deployment is being applied")
const DeploymentApplyDeadline = 10 * time.Minute
const MaxDeploymentResultStoredBytes = 160 * 1024

type Deployment struct {
	ID, ApplicationID, InstanceID, EndpointID string
	State          string         `json:"state"`
	Revision       int            `json:"revision"`
	SpecDigest     string         `json:"spec_digest"`
	MappingVersion int            `json:"mapping_version"`
	Plan           DeploymentPlan `json:"plan"`
	CreatedBy      string         `json:"created_by"`
	CreatedAt      time.Time      `json:"created_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	Expired        bool           `json:"expired"`
	AppliedBy      string         `json:"applied_by"`
	AppliedAt      *time.Time     `json:"applied_at"`
	Deadline       *time.Time     `json:"deadline"`
	SettledAt      *time.Time     `json:"settled_at"`
	Detail         string         `json:"detail"`
	Result         *protocol.DeploymentResult `json:"result"` // nil until settled
}
// ApplicationInstance gains: CurrentRevision int `json:"current_revision"`, PreviousRevision int `json:"previous_revision"`.
```

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/deployment_test.go`:

```go
func TestMigration24ShapesDeployments(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	for _, col := range []string{"applied_by", "applied_at", "deadline", "settled_at", "detail", "result"} {
		if _, err := st.db.ExecContext(ctx, `SELECT `+col+` FROM deployments LIMIT 1`); err != nil {
			t.Fatalf("column %s: %v", col, err)
		}
	}
	for _, col := range []string{"current_revision", "previous_revision"} {
		if _, err := st.db.ExecContext(ctx, `SELECT `+col+` FROM application_instances LIMIT 1`); err != nil {
			t.Fatalf("instance column %s: %v", col, err)
		}
	}
}

func TestPlanDeploymentRespectsLiveRows(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	first, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	// A settled history row must survive a new plan; only the planned row is replaced.
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET state='succeeded',settled_at=? WHERE id=?`), time.Now().UTC(), first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 2 || list[0].ID != second.ID || list[1].State != "succeeded" {
		t.Fatalf("history kept: %+v %v", list, err)
	}
	// An applying row blocks a new plan.
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET state='applying' WHERE id=?`), second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = ts.PlanDeployment(ctx, a, app.ID, planRequest(m)); !errors.Is(err, ErrDeploymentInProgress) {
		t.Fatalf("plan during apply: %v", err)
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); !errors.Is(err, ErrDeploymentInProgress) {
		t.Fatalf("release during apply: %v", err)
	}
	// Settled rows never block release and are kept.
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET state='failed' WHERE id=?`), second.ID); err != nil {
		t.Fatal(err)
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("history after release: %d %v", n, err)
	}
}
```

Adjust the existing `TestPlanDeploymentReplacesAndExpires` ("replaced plan still readable" expects `ErrNotFound`) and `TestReleaseRefusesLivePlanAndDeletesExpired` (release deletes only `planned` rows) if their assertions conflict with the new rules; keep their intent.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ -run 'TestMigration24|TestPlanDeploymentRespectsLiveRows' -count=1`
Expected: FAIL (missing columns / `ErrDeploymentInProgress` undefined).

- [ ] **Step 3: Append migration 24**

```go
	{Version: 24, Name: "deployment_apply", SQLite: `CREATE TABLE deployments_new (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 application_id TEXT NOT NULL,
 instance_id TEXT NOT NULL,
 endpoint_id TEXT NOT NULL,
 project TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('planned','applying','succeeded','failed','denied','timed_out','unknown')),
 revision INTEGER NOT NULL CHECK(revision BETWEEN 1 AND 100),
 spec_digest TEXT NOT NULL,
 mapping_version INTEGER NOT NULL CHECK(mapping_version BETWEEN 1 AND 1000000000),
 plan TEXT NOT NULL CHECK(length(plan)<=65536),
 created_by TEXT NOT NULL,
 created_at DATETIME NOT NULL,
 expires_at DATETIME NOT NULL,
 applied_by TEXT NOT NULL DEFAULT '',
 applied_at DATETIME,
 deadline DATETIME,
 settled_at DATETIME,
 detail TEXT NOT NULL DEFAULT '',
 result TEXT NOT NULL DEFAULT '' CHECK(length(result)<=163840),
 FOREIGN KEY(organization_id,environment_id,application_id) REFERENCES applications(organization_id,environment_id,id) ON DELETE CASCADE
);
INSERT INTO deployments_new (id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at) SELECT id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at FROM deployments;
DROP TABLE deployments;
ALTER TABLE deployments_new RENAME TO deployments;
CREATE INDEX idx_deployments_application ON deployments(application_id,created_at);
CREATE UNIQUE INDEX idx_deployments_live ON deployments(instance_id) WHERE state IN ('planned','applying');
ALTER TABLE application_instances ADD COLUMN current_revision INTEGER NOT NULL DEFAULT 0 CHECK(current_revision BETWEEN 0 AND 100);
ALTER TABLE application_instances ADD COLUMN previous_revision INTEGER NOT NULL DEFAULT 0 CHECK(previous_revision BETWEEN 0 AND 100);
`, Postgres: `ALTER TABLE deployments DROP CONSTRAINT deployments_instance_id_key;
ALTER TABLE deployments DROP CONSTRAINT deployments_state_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_state_check CHECK(state IN ('planned','applying','succeeded','failed','denied','timed_out','unknown'));
ALTER TABLE deployments ADD COLUMN applied_by TEXT NOT NULL DEFAULT '', ADD COLUMN applied_at TIMESTAMPTZ, ADD COLUMN deadline TIMESTAMPTZ, ADD COLUMN settled_at TIMESTAMPTZ, ADD COLUMN detail TEXT NOT NULL DEFAULT '', ADD COLUMN result TEXT NOT NULL DEFAULT '' CHECK(length(result)<=163840);
CREATE UNIQUE INDEX idx_deployments_live ON deployments(instance_id) WHERE state IN ('planned','applying');
ALTER TABLE application_instances ADD COLUMN current_revision INTEGER NOT NULL DEFAULT 0 CHECK(current_revision BETWEEN 0 AND 100), ADD COLUMN previous_revision INTEGER NOT NULL DEFAULT 0 CHECK(previous_revision BETWEEN 0 AND 100);
`},
```

If PostgreSQL names the constraints differently, read them with `SELECT conname FROM pg_constraint WHERE conrelid='deployments'::regclass` on the local instance and use the real names; both the SQLite rebuild and the PostgreSQL alter must leave the same shape.

- [ ] **Step 4: Row model and reads**

In `application_deployment.go`:
- Extend `Deployment` as in Interfaces; `deploymentColumns` adds `applied_by,applied_at,deadline,settled_at,detail,result`; `scanDeployment` scans the nullable times into `sql.NullTime` and the result into a string, unmarshalling into `Result` when non-empty (invalid JSON → `ErrRevisionCorrupt`).
- `PlanDeployment`: before inserting, `SELECT state FROM deployments WHERE organization_id=? AND environment_id=? AND instance_id=? AND state IN ('planned','applying')`; `applying` → `ErrDeploymentInProgress`; `planned` → `DELETE` that row (by id); then insert as before.
- `ListDeployments` keeps `ORDER BY created_at DESC, id LIMIT 100`.

In `application_adoption.go`:
- `ApplicationInstance` gains `CurrentRevision`, `PreviousRevision`; `ListApplicationInstances` selects `i.current_revision,i.previous_revision`.
- `ReleaseApplication`: after the application lock, `SELECT state, expires_at FROM deployments WHERE organization_id=? AND environment_id=? AND application_id=? AND instance_id=? AND state IN ('planned','applying')`; an `applying` row → `ErrDeploymentInProgress`; a `planned` row with `expires_at > now` → `ErrDeploymentPlanned`; then `DELETE ... WHERE ... AND state='planned'`. Settled rows stay.

In `store.go`: add `ErrDeploymentInProgress`.

- [ ] **Step 5: Run store tests on both drivers**

Run: `gofmt -w internal/store/*.go && go test ./internal/store/ -count=1` then with `KY_TEST_POSTGRES_DSN`.
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store && git commit -m "feat: deployment states, result column and instance revisions

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Store apply, settle, abandon, sweep

**Files:**
- Create: `internal/store/application_apply.go`
- Modify: `internal/store/application_secrets.go` (extract `resolveApplicationValues`)
- Modify: `internal/store/samples.go:214-240` (`Prune` sweep)
- Modify: `internal/store/store.go` (Tenancy interface)
- Test: `internal/store/application_apply_test.go`

**Interfaces:**
- Consumes: `preflight`-independent reads; `applicationValuesKey`, `crypto.DecryptAESGCM`, `validateApplicationValues`; `protocol.DeploymentRequest`, `protocol.Port`; `t.store.Audit().LogAudit(ctx, *AuditRecord)`.
- Produces (Tenancy interface):

```go
ApplyDeployment(ctx context.Context, a TenantAccess, app, id, confirm string, key []byte) (*Deployment, *protocol.DeploymentRequest, error)
FailDeployment(ctx context.Context, id, detail string) error
SettleDeployment(ctx context.Context, endpointID string, res protocol.DeploymentResult) error
AbandonDeployments(ctx context.Context, endpointID string) (int64, error)
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/application_apply_test.go
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func applyFixture(t *testing.T) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot, *ApplicationMapping, *Deployment, []byte) {
	t.Helper()
	st, a, app, endpoint, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	key := make([]byte, 32)
	// A revision with a secret so apply has something to resolve.
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Restart: "always", Ports: []ApplicationPort{{Target: 80, Published: 8080, Protocol: "tcp"}}, Environment: map[string]ApplicationSecretRef{"TOKEN": {SecretRef: "web-token"}}}}}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, map[string]string{"web-token": "apply-secret-canary"}, key); err != nil {
		t.Fatal(err)
	}
	m2, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m2)); err != nil {
		t.Fatal(err)
	}
	m2, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m2))
	if err != nil {
		t.Fatal(err)
	}
	return st, a, app, endpoint, snapshot, m2, d, key
}

func TestApplyDeploymentBuildsTheRequestAndMovesToApplying(t *testing.T) {
	st, a, app, endpoint, snapshot, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	applied, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key)
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != "applying" || applied.AppliedBy != a.ActorID || applied.AppliedAt == nil || applied.Deadline == nil || time.Until(*applied.Deadline) > DeploymentApplyDeadline {
		t.Fatalf("row: %+v", applied)
	}
	c := snapshot.Containers[0]
	if req.Deployment != d.ID || req.Endpoint != endpoint || req.Project != "shop" || req.Revision != 2 || len(req.Services) != 1 {
		t.Fatalf("request: %+v", req)
	}
	s := req.Services[0]
	if s.Name != "web" || s.ContainerName != c.Name || s.ImageID != d.Plan.Services[0].ImageID || s.Replaces.ContainerID != c.ID || s.Restart != "always" || len(s.Ports) != 1 || s.Ports[0].Host != 8080 || s.Ports[0].Container != 80 || s.Env["TOKEN"] != "apply-secret-canary" {
		t.Fatalf("service: %+v", s)
	}
	if err := req.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	// The row and the audit trail carry no value.
	var stored string
	if err := st.db.QueryRow(st.rebind(`SELECT plan||detail||result FROM deployments WHERE id=?`), d.ID).Scan(&stored); err != nil || strings.Contains(stored, "canary") {
		t.Fatal("secret persisted on the row")
	}
	// Apply twice: the CAS lets one through.
	if _, _, err = ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatalf("second apply: %v", err)
	}
	_ = m
}

func TestApplyDeploymentPreconditions(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		mutate func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, endpoint string, d *Deployment)
		want   error
	}{
		"wrong confirm": {func(*testing.T, *SQLStore, TenantAccess, *Application, string, *Deployment) {}, ErrAdoptionChanged},
		"expired": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, d *Deployment) {
			if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=? WHERE id=?`), time.Now().Add(-time.Minute), d.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"head moved": {func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, _ string, _ *Deployment) {
			if _, err := st.Tenancy().AppendApplicationRevision(ctx, a, app.ID, 2, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"endpoint offline": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, endpoint string, _ *Deployment) {
			if _, err := st.db.Exec(st.rebind(`UPDATE endpoints SET state='offline' WHERE id=?`), endpoint); err != nil {
				t.Fatal(err)
			}
		}, ErrEndpointOffline},
	} {
		t.Run(name, func(t *testing.T) {
			st, a, app, endpoint, _, _, d, key := applyFixture(t)
			tc.mutate(t, st, a, app, endpoint, d)
			confirm := "shop"
			if name == "wrong confirm" {
				confirm = "nope"
			}
			if _, _, err := st.Tenancy().ApplyDeployment(ctx, a, app.ID, d.ID, confirm, key); !errors.Is(err, tc.want) {
				t.Fatalf("%s: %v", name, err)
			}
			var state string
			if err := st.db.QueryRow(st.rebind(`SELECT state FROM deployments WHERE id=?`), d.ID).Scan(&state); err != nil || state != "planned" {
				t.Fatalf("%s left state %s", name, state)
			}
		})
	}
	// Roles: operator is refused, developer allowed.
	st, a, app, _, _, _, d, key := applyFixture(t)
	if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleOperator, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator: %v", err)
	}
	if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleDeveloper, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatalf("developer: %v", err)
	}
}

func settledResult(d *Deployment, outcome string, newID string) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: d.ID, Outcome: outcome, Steps: []protocol.DeploymentStep{{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}}, Services: []protocol.DeploymentIdentity{}}
	if newID != "" {
		res.Services = append(res.Services, protocol.DeploymentIdentity{Service: "web", ContainerID: newID, ImageID: d.Plan.Services[0].ImageID, CreatedUnix: 1800000000})
	}
	if outcome != protocol.OutcomeSucceeded {
		res.Steps[0].Outcome = outcome
		res.Steps[0].Detail = "fixed text"
	}
	return res
}

func TestSettleDeploymentRebindsAndAdvances(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	newID := strings.Repeat("e", 64)
	// A result from another endpoint changes nothing.
	if err := ts.SettleDeployment(ctx, "other-endpoint", settledResult(d, protocol.OutcomeSucceeded, newID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign endpoint: %v", err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, newID)); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.State != "succeeded" || got.SettledAt == nil || got.Result == nil || len(got.Result.Services) != 1 {
		t.Fatalf("settled: %+v %v", got, err)
	}
	instances, err := ts.ListApplicationInstances(ctx, a, endpoint)
	if err != nil || len(instances) != 1 || instances[0].CurrentRevision != 2 || instances[0].PreviousRevision != 0 {
		t.Fatalf("revision: %+v %v", instances, err)
	}
	var container, service, name string
	if err := st.db.QueryRow(st.rebind(`SELECT container_id,service_name,name FROM application_resources WHERE instance_id=?`), m.InstanceID).Scan(&container, &service, &name); err != nil || container != newID || service != "web" || name != "shop-web" {
		t.Fatalf("rebind: %s %s %s %v", container, service, name, err)
	}
	// First answer wins on a settled row.
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second settle: %v", err)
	}
	var audits int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_logs WHERE action=? AND resource=?`), "application.deploy", app.ID+"/deployments/"+d.ID).Scan(&audits); err != nil || audits < 1 {
		t.Fatalf("settle audit: %d %v", audits, err)
	}
}

func TestSettleDeploymentFromUnknownAndAbandon(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	n, err := ts.AbandonDeployments(ctx, endpoint)
	if err != nil || n != 1 {
		t.Fatalf("abandon: %d %v", n, err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "unknown" || got.Detail == "" {
		t.Fatalf("abandoned: %+v", got)
	}
	// A late result still settles an unknown row, but a failed run does not advance the revision.
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); err != nil {
		t.Fatal(err)
	}
	got, _ = ts.ReadDeployment(ctx, a, app.ID, d.ID)
	instances, _ := ts.ListApplicationInstances(ctx, a, endpoint)
	if got.State != "failed" || instances[0].CurrentRevision != 0 {
		t.Fatalf("late settle: %+v %+v", got, instances)
	}
}

func TestPruneSweepsStaleApplying(t *testing.T) {
	st, a, app, _, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET deadline=? WHERE id=?`), time.Now().Add(-3*time.Minute), d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "unknown" {
		t.Fatalf("sweep: %+v", got)
	}
}

func TestSettleRebindRefusesMissingResource(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`DELETE FROM application_resources WHERE instance_id=?`), m.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatalf("rebind without resource: %v", err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "applying" {
		t.Fatalf("half-written: %+v", got)
	}
}
```

Check the audit table name (`grep -n "INSERT INTO audit" internal/store/*.go`) and adjust the `audit_logs` query.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ -run 'TestApply|TestSettle|TestPruneSweeps' -count=1`
Expected: compile error, `ApplyDeployment` undefined.

- [ ] **Step 3: Extract `resolveApplicationValues`**

In `application_secrets.go`, move the body of `ResolveApplicationSecrets`'s closure (from the `SELECT spec,digest,secrets_enc` through the validation) into:

```go
// resolveApplicationValues decrypts one revision's values inside the caller's transaction.
// Callers own permission and audit: ResolveApplicationSecrets reveals under secret.reveal;
// ApplyDeployment consumes under application.deploy and never returns the values.
func (t *tenancyStore) resolveApplicationValues(ctx context.Context, tx *sql.Tx, a TenantAccess, id string, number int, key []byte) (ApplicationSpec, map[string]string, error)
```

and have `ResolveApplicationSecrets` call it. Behaviour unchanged; `go test ./internal/store/ -run Secret -count=1` must still pass.

- [ ] **Step 4: Implement `application_apply.go`**

```go
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

const DeploymentApplyDeadline = 10 * time.Minute
const MaxDeploymentResultStoredBytes = 160 * 1024

// ApplyDeployment turns a planned row into an applying one and builds the frame the agent
// executes. The values it resolves exist only in the returned request. See
// docs/application-schema.md, Deploy.
func (t *tenancyStore) ApplyDeployment(ctx context.Context, a TenantAccess, app, id, confirm string, key []byte) (*Deployment, *protocol.DeploymentRequest, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" || len(key) != 32 {
		return nil, nil, ErrInvalid
	}
	planID, err := uuid.Parse(id)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	var out *Deployment
	var req *protocol.DeploymentRequest
	err = t.withTenantTarget(ctx, a, permissions.ApplicationDeploy, appID.String()+"/deployments/"+planID.String()+"/apply", func(tx *sql.Tx) error {
		lock := ""
		if t.store.driver == "postgres" {
			lock = " FOR UPDATE"
		}
		var head int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`+lock), a.OrganizationID, a.EnvironmentID, appID.String()).Scan(&head); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		d, err := scanDeployment(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+deploymentColumns+` FROM deployments WHERE organization_id=? AND environment_id=? AND application_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), planID.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if d.State != "planned" || d.Expired || confirm != d.Plan.Project || head != d.Revision {
			return ErrAdoptionChanged
		}
		var mappingVersion int
		var endpointState string
		err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.mapping_version,e.state FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.id=? AND i.endpoint_id=? AND i.organization_id=? AND i.environment_id=?`), d.InstanceID, d.EndpointID, a.OrganizationID, a.EnvironmentID).Scan(&mappingVersion, &endpointState)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && mappingVersion != d.MappingVersion) {
			return ErrAdoptionChanged
		}
		if err != nil {
			return err
		}
		if endpointState != "active" {
			return ErrEndpointOffline
		}
		spec, values, err := t.resolveApplicationValues(ctx, tx, a, appID.String(), d.Revision, key)
		if err != nil {
			return err
		}
		if applicationSpecDigest(mustJSON(spec)) != d.SpecDigest {
			return ErrAdoptionChanged
		}
		names := map[string]string{}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name FROM application_resources WHERE instance_id=? AND endpoint_id=?`), d.InstanceID, d.EndpointID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var cid, name string
			if err := rows.Scan(&cid, &name); err != nil {
				rows.Close()
				return err
			}
			names[cid] = name
		}
		rows.Close()
		now := time.Now().UTC()
		req = &protocol.DeploymentRequest{Deployment: d.ID, Endpoint: d.EndpointID, Project: d.Plan.Project, Revision: d.Revision, Deadline: now.Add(DeploymentApplyDeadline), Services: []protocol.DeploymentService{}}
		for i, ps := range d.Plan.Services {
			name, ok := names[ps.ContainerID]
			if !ok {
				return ErrAdoptionChanged
			}
			svc := protocol.DeploymentService{Name: ps.Name, ContainerName: name, ImageID: ps.ImageID, Replaces: ps.Replaces, Restart: ps.Restart, Ports: []protocol.Port{}, Env: map[string]string{}}
			for _, p := range ps.Ports {
				svc.Ports = append(svc.Ports, protocol.Port{Container: p.Target, Host: p.Published, Protocol: p.Protocol, HostIP: p.HostIP})
			}
			if i < len(spec.Services) && spec.Services[i].Name == ps.Name {
				for envName, ref := range spec.Services[i].Environment {
					svc.Env[envName] = values[ref.SecretRef]
				}
			} else {
				return ErrAdoptionChanged
			}
			req.Services = append(req.Services, svc)
		}
		if err := req.Validate(now); err != nil {
			return ErrInvalid
		}
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='applying',applied_by=?,applied_at=?,deadline=? WHERE id=? AND state='planned'`), a.ActorID, now, req.Deadline, d.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrAdoptionChanged
		}
		d.State, d.AppliedBy, d.AppliedAt, d.Deadline = "applying", a.ActorID, &now, &req.Deadline
		out = d
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, req, nil
}

func mustJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }

// FailDeployment records that the frame never left the server.
func (t *tenancyStore) FailDeployment(ctx context.Context, id, detail string) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='failed',detail=?,settled_at=? WHERE id=? AND state='applying'`), protocol.CleanText(detail, 255), time.Now().UTC(), id)
	return err
}

// SettleDeployment is the one writer for outcomes. Scoped to the endpoint that ran it; a
// row in any state other than applying or unknown is left alone.
func (t *tenancyStore) SettleDeployment(ctx context.Context, endpointID string, res protocol.DeploymentResult) error {
	if res.Validate() != nil {
		return ErrInvalid
	}
	raw, err := json.Marshal(struct {
		Steps    []protocol.DeploymentStep     `json:"steps"`
		Services []protocol.DeploymentIdentity `json:"services"`
	}{res.Steps, res.Services})
	if err != nil || len(raw) > MaxDeploymentResultStoredBytes {
		return ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var d Deployment
	var org, env, instance string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,organization_id,environment_id,application_id,instance_id,revision,plan FROM deployments WHERE id=? AND endpoint_id=? AND state IN ('applying','unknown')`), res.Deployment, endpointID).Scan(&d.ID, &org, &env, &d.ApplicationID, &instance, &d.Revision, &d.plan)
	// (scan `plan` into a string and unmarshal into d.Plan; adapt to the struct)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	replaced := map[string]PlannedService{}
	for _, ps := range d.Plan.Services {
		replaced[ps.Name] = ps
	}
	for _, idn := range res.Services {
		ps, ok := replaced[idn.Service]
		if !ok {
			return ErrInvalid
		}
		var name string
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT name FROM application_resources WHERE instance_id=? AND endpoint_id=? AND container_id=?`), instance, endpointID, ps.ContainerID).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAdoptionChanged
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM application_resources WHERE instance_id=? AND endpoint_id=? AND container_id=?`), instance, endpointID, ps.ContainerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_resources(instance_id,endpoint_id,container_id,name,image_id,created_at,service_name) VALUES(?,?,?,?,?,?,?)`), instance, endpointID, idn.ContainerID, name, idn.ImageID, time.Unix(idn.CreatedUnix, 0).UTC(), idn.Service); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state=?,detail=?,result=?,settled_at=? WHERE id=?`), res.Outcome, protocol.CleanText(res.Detail, 255), string(raw), now, d.ID); err != nil {
		return err
	}
	if res.Outcome == protocol.OutcomeSucceeded {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_instances SET previous_revision=current_revision,current_revision=? WHERE id=?`), d.Revision, instance); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = t.store.Audit().LogAudit(ctx, &AuditRecord{Scope: "organization", OrganizationID: org, EnvironmentID: env, UserID: "agent:" + endpointID, Action: string(permissions.ApplicationDeploy), Resource: d.ApplicationID + "/deployments/" + d.ID, Result: res.Outcome, CreatedAt: now})
	return nil
}

// AbandonDeployments marks what was applying when the connection ended as unknown. The
// agent keeps the real answer and re-sends it on reconnect.
func (t *tenancyStore) AbandonDeployments(ctx context.Context, endpointID string) (int64, error) {
	res, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='unknown',detail=? WHERE endpoint_id=? AND state='applying'`), "the connection ended before a result arrived", endpointID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```

Notes for the implementer: the settle SELECT scans `plan` into a local string then `json.Unmarshal` into `d.Plan` (there is no `d.plan` field; the snippet abbreviates). Check how `LogAudit` is reached from `tenancyStore` (`t.store.Audit()` per `tenant_access.go:105`) and the exact `AuditRecord` fields required (`CorrelationID` may need `uuid.NewString()`). In `Prune` (`samples.go`) add before the loop:

```go
	if _, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='unknown',detail=? WHERE state='applying' AND deadline IS NOT NULL AND deadline<?`), "no result arrived before the deadline", now.Add(-2*time.Minute)); err != nil {
		return 0, err
	}
```

Add the four methods to the `Tenancy` interface in `store.go`.

- [ ] **Step 5: PostgreSQL interleaving test**

Append to `application_apply_test.go`:

```go
func TestPlanAndReleaseNeverLeaveALivePlanForAReleasedInstance(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	if st.driver != "postgres" {
		t.Skip("interleaving needs real row locks")
	}
	ctx := context.Background()
	ts := st.Tenancy()
	for round := 0; round < 20; round++ {
		// Re-adopt if the previous round released.
		if _, err := ts.ReadApplicationMapping(ctx, a, app.ID); errors.Is(err, ErrNotFound) {
			p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, m.Preview.EndpointID, "shop")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: m.Preview.EndpointID, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
				t.Fatal(err)
			}
			m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
			if err = ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
				t.Fatal(err)
			}
			m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
		}
		done := make(chan error, 2)
		go func() { _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m)); done <- err }()
		go func() { done <- ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop") }()
		<-done
		<-done
		var live int
		if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM deployments d WHERE d.state='planned' AND d.expires_at>? AND NOT EXISTS (SELECT 1 FROM application_instances i WHERE i.id=d.instance_id)`), time.Now().UTC()).Scan(&live); err != nil || live != 0 {
			t.Fatalf("round %d: live plan for a released instance: %d %v", round, live, err)
		}
	}
}
```

Check the driver field name on `SQLStore` (`grep -n "driver" internal/store/store.go | head`).

- [ ] **Step 6: Run on both drivers**

Run: `gofmt -w internal/store/*.go && go vet ./internal/store/ && go test ./internal/store/ -count=1` then with `KY_TEST_POSTGRES_DSN`.
Expected: PASS (the interleaving test skips on SQLite, runs on PostgreSQL).

- [ ] **Step 7: Commit**

```bash
git add internal/store && git commit -m "feat: apply, settle, abandon and sweep deployments in the store

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Agent runner, ledger, capability, re-send

**Files:**
- Create: `internal/agent/client/deployments.go`
- Modify: `internal/agent/client/connect.go` (Options field, capability, frame case, re-send after hello, root context, outbound send)
- Test: `internal/agent/client/deployments_test.go`

**Interfaces:**
- Consumes: `protocol.DeploymentRequest/Result`, `Validate`, `TypeDeploymentApply/Result`, `CapabilityDeploymentApply`, `outFrame`, `write`.
- Produces: `Options.Deploy func(context.Context, protocol.DeploymentRequest) protocol.DeploymentResult`; unexported `deployer` created in `Run` and passed to `session` (add a parameter beside `commands`).

- [ ] **Step 1: Write the failing tests**

```go
// internal/agent/client/deployments_test.go
package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func testRequest(endpoint string) protocol.DeploymentRequest {
	return protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: endpoint, Project: "shop", Revision: 1, Deadline: time.Now().Add(5 * time.Minute), Services: []protocol.DeploymentService{{Name: "web", ContainerName: "shop-web-1", ImageID: "sha256:" + strings.Repeat("a", 64), Replaces: protocol.InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000}, Env: map[string]string{"TOKEN": "agent-secret-canary"}}}}
}

func TestDeployerRunsOffTheSessionAndPersistsBeforeSending(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	finish := make(chan struct{})
	var seen protocol.DeploymentRequest
	var mu sync.Mutex
	opts := &Options{Deploy: func(ctx context.Context, req protocol.DeploymentRequest) protocol.DeploymentResult {
		mu.Lock()
		seen = req
		mu.Unlock()
		close(started)
		<-finish
		if ctx.Err() != nil {
			return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeUnknown, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
		}
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}}
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	d := newDeployer(root, dir, opts)
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	out := make(chan outFrame, 4)
	req := testRequest("ep_1")
	raw, _ := json.Marshal(req)
	d.handle(sessionCtx, "ep_1", raw, out)
	<-started
	cancelSession() // the socket dropped; the run must continue
	close(finish)
	f := <-out
	var res protocol.DeploymentResult
	if f.Type != protocol.TypeDeploymentResult || json.Unmarshal(f.Payload, &res) != nil || res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("result: %+v", f)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "deployments.json"))
	if err != nil || !strings.Contains(string(stored), req.Deployment) || strings.Contains(string(stored), "agent-secret-canary") {
		t.Fatalf("ledger: %v %s", err, stored)
	}
	mu.Lock()
	if len(seen.Services[0].Env) != 0 {
		// The request handed to Deploy was live; after the run the deployer clears it.
	}
	mu.Unlock()
	// Replay: the same ID answers from the ledger without running again.
	ran := false
	opts.Deploy = func(context.Context, protocol.DeploymentRequest) protocol.DeploymentResult { ran = true; return protocol.DeploymentResult{} }
	d.handle(context.Background(), "ep_1", raw, out)
	f = <-out
	if ran || f.Type != protocol.TypeDeploymentResult {
		t.Fatal("replay ran the deployment again")
	}
}

func TestDeployerRefusals(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	opts := &Options{Deploy: func(context.Context, protocol.DeploymentRequest) protocol.DeploymentResult {
		calls++
		time.Sleep(200 * time.Millisecond)
		return protocol.DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}}
	d := newDeployer(context.Background(), dir, opts)
	out := make(chan outFrame, 8)
	read := func() protocol.DeploymentResult {
		f := <-out
		var res protocol.DeploymentResult
		_ = json.Unmarshal(f.Payload, &res)
		return res
	}
	// Foreign endpoint.
	raw, _ := json.Marshal(testRequest("someone-else"))
	d.handle(context.Background(), "ep_1", raw, out)
	if res := read(); res.Outcome != protocol.OutcomeDenied || calls != 0 {
		t.Fatalf("foreign: %+v", res)
	}
	// Invalid request.
	bad := testRequest("ep_1")
	bad.Services = nil
	raw, _ = json.Marshal(bad)
	d.handle(context.Background(), "ep_1", raw, out)
	if res := read(); res.Outcome != protocol.OutcomeDenied || calls != 0 {
		t.Fatalf("invalid: %+v", res)
	}
	// Busy: a second request while one runs is denied.
	req := testRequest("ep_1")
	raw, _ = json.Marshal(req)
	d.handle(context.Background(), "ep_1", raw, out)
	second := testRequest("ep_1")
	second.Deployment = "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c"
	raw2, _ := json.Marshal(second)
	d.handle(context.Background(), "ep_1", raw2, out)
	first, next := read(), read()
	if first.Outcome != protocol.OutcomeDenied && next.Outcome != protocol.OutcomeDenied {
		t.Fatalf("one of two concurrent applies must be denied: %+v %+v", first, next)
	}
	if calls != 1 {
		t.Fatalf("deploy ran %d times", calls)
	}
	// No runtime.
	d2 := newDeployer(context.Background(), t.TempDir(), &Options{})
	d2.handle(context.Background(), "ep_1", raw, out)
	if res := read(); res.Outcome != protocol.OutcomeDenied {
		t.Fatalf("no runtime: %+v", res)
	}
}

func TestDeployerResendsPersistedResults(t *testing.T) {
	dir := t.TempDir()
	ledger := map[string]deploymentEntry{"3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b": {Result: protocol.DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: protocol.OutcomeFailed, Detail: "x", Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: time.Now().UTC()}, "old": {Result: protocol.DeploymentResult{Deployment: "old"}, Finished: time.Now().Add(-25 * time.Hour)}}
	raw, _ := json.Marshal(ledger)
	if err := os.WriteFile(filepath.Join(dir, "deployments.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDeployer(context.Background(), dir, &Options{})
	out := make(chan outFrame, 4)
	d.resend(out)
	select {
	case f := <-out:
		if f.Type != protocol.TypeDeploymentResult || !strings.Contains(string(f.Payload), "3f2b1c9e") {
			t.Fatalf("resend: %+v", f)
		}
	default:
		t.Fatal("nothing re-sent")
	}
	select {
	case f := <-out:
		t.Fatalf("expired entry re-sent: %+v", f)
	default:
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/client/ -run 'TestDeployer' -count=1`
Expected: compile error, `newDeployer` undefined.

- [ ] **Step 3: Implement the deployer**

```go
// internal/agent/client/deployments.go
package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const (
	deploymentLedgerLife = 24 * time.Hour
	deploymentLedgerMax  = 20
)

type deploymentEntry struct {
	Result   protocol.DeploymentResult `json:"result"`
	Finished time.Time                 `json:"finished"`
}

// deployer runs one deployment at a time on the agent's root context, remembers every result
// before sending it, and re-sends what a dropped session never carried. See
// docs/agent-protocol.md, Deployment apply.
type deployer struct {
	root context.Context
	opts *Options
	path string
	mu   sync.Mutex
	done map[string]deploymentEntry
	busy chan struct{}
}

func newDeployer(root context.Context, dir string, opts *Options) *deployer {
	d := &deployer{root: root, opts: opts, path: filepath.Join(dir, "deployments.json"), done: map[string]deploymentEntry{}, busy: make(chan struct{}, 1)}
	if raw, err := os.ReadFile(d.path); err == nil {
		var saved map[string]deploymentEntry
		if json.Unmarshal(raw, &saved) == nil {
			d.done = saved
			d.prune()
		}
	}
	return d
}

func (d *deployer) prune() {
	cutoff := time.Now().UTC().Add(-deploymentLedgerLife)
	for id, e := range d.done {
		if e.Finished.Before(cutoff) {
			delete(d.done, id)
		}
	}
	if len(d.done) <= deploymentLedgerMax {
		return
	}
	ids := make([]string, 0, len(d.done))
	for id := range d.done {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return d.done[ids[i]].Finished.Before(d.done[ids[j]].Finished) })
	for _, id := range ids[:len(d.done)-deploymentLedgerMax] {
		delete(d.done, id)
	}
}

func (d *deployer) record(res protocol.DeploymentResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done[res.Deployment] = deploymentEntry{Result: res, Finished: time.Now().UTC()}
	d.prune()
	if raw, err := json.Marshal(d.done); err == nil {
		tmp := d.path + ".tmp"
		if os.WriteFile(tmp, raw, 0o600) == nil {
			_ = os.Rename(tmp, d.path)
		}
	}
}

func resultFrame(res protocol.DeploymentResult) outFrame {
	return outFrame{Type: protocol.TypeDeploymentResult, Payload: res}
}

func denied(id, detail string) protocol.DeploymentResult {
	return protocol.DeploymentResult{Deployment: id, Outcome: protocol.OutcomeDenied, Detail: detail, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
}

// handle answers one deployment.apply payload. Refusals are sent and not remembered; a run's
// result is remembered and then sent. sessionCtx bounds only the send.
func (d *deployer) handle(sessionCtx context.Context, endpointID string, payload []byte, out chan<- outFrame) {
	var req protocol.DeploymentRequest
	if json.Unmarshal(payload, &req) != nil || req.Validate(time.Now()) != nil {
		send(sessionCtx, out, resultFrame(denied(req.Deployment, "invalid deployment request")))
		return
	}
	if req.Endpoint != endpointID {
		send(sessionCtx, out, resultFrame(denied(req.Deployment, "this deployment is addressed to another endpoint")))
		return
	}
	d.mu.Lock()
	prior, ok := d.done[req.Deployment]
	d.mu.Unlock()
	if ok {
		send(sessionCtx, out, resultFrame(prior.Result))
		return
	}
	if d.opts.Deploy == nil {
		send(sessionCtx, out, resultFrame(denied(req.Deployment, "this agent has no runtime to deploy")))
		return
	}
	select {
	case d.busy <- struct{}{}:
	default:
		send(sessionCtx, out, resultFrame(denied(req.Deployment, "this agent is already applying a deployment")))
		return
	}
	go func() {
		defer func() { <-d.busy }()
		res := d.opts.Deploy(d.root, req)
		for i := range req.Services {
			clear(req.Services[i].Env)
		}
		if res.Deployment == "" {
			res.Deployment = req.Deployment
		}
		d.record(res)
		send(sessionCtx, out, resultFrame(res))
	}()
}

// resend queues every remembered result. The server settles once; the rest are ignored.
func (d *deployer) resend(out chan<- outFrame) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	for _, e := range d.done {
		select {
		case out <- resultFrame(e.Result):
		default:
		}
	}
}

func send(ctx context.Context, out chan<- outFrame, f outFrame) {
	select {
	case out <- f:
	case <-ctx.Done():
	}
}
```

If `outFrame.Payload` is typed as `[]byte`/`json.RawMessage` rather than `any`, marshal the result first; read `connect.go` for the type and `write`'s handling.

`connect.go` changes:
- `Options` gains `Deploy func(context.Context, protocol.DeploymentRequest) protocol.DeploymentResult` with a doc comment saying it runs on the agent's root context.
- In `Run`, after `commands := openLedger(commandDir)`, add `deployments := newDeployer(ctx, commandDir, &opts)` and pass it into `session(...)`.
- In `session`, capability: `if opts.Deploy != nil { capabilities = append(capabilities, protocol.CapabilityDeploymentApply) }`.
- After the hello exchange in a non-pending session (right after the first `sendInventory`), call `deployments.resend(outbound)`.
- In the frame switch add:

```go
			case protocol.TypeDeploymentApply:
				if len(f.Payload) > protocol.MaxDeploymentRequestBytes {
					conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
					return errors.New("deployment request too large")
				}
				if hello.State == "pending" {
					break
				}
				deployments.handle(ctx, id.EndpointID, f.Payload, outbound)
```

Note: a result produced after the session ended is dropped by `send` (ctx done) but stays in the ledger; the next session's `resend` carries it. That satisfies "written before sending, re-sent on reconnect".

- [ ] **Step 4: Run agent tests**

Run: `gofmt -w internal/agent/client/*.go && go vet ./internal/agent/client/ && go test -race ./internal/agent/client/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent && git commit -m "feat: agent applies deployments off the session and re-sends results

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Server route, result frame, abandon, agent wiring, end-to-end test

**Files:**
- Modify: `internal/api/server.go` (route)
- Modify: `internal/api/application_handlers.go` (`handleApplyDeployment`)
- Modify: `internal/api/agent_connect.go` (result frame case; size exemption; `markOffline` abandon)
- Modify: `internal/api/tenant_handlers.go` (`ErrDeploymentInProgress` → 409 `deployment_in_progress`)
- Modify: `internal/api/local_docker.go`, `cmd/agent/main.go` (`Deploy: engine.Deploy`)
- Test: `internal/api/deployment_apply_test.go` (fake socket), `internal/api/deployment_integration_test.go` (real Docker), `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: Task 2 store methods; `s.agents.deliver`, `s.Connected`, `envelope`, `ReadEndpoint(...).Capabilities`, `s.config.Security.EncryptionKey` (the key `ImportApplication` uses; grep the import handler for the exact field).
- Produces: `POST /api/organizations/{organization}/environments/{environment}/applications/{application}/deployments/{deployment}/apply` → 202 `store.Deployment`; 501 without capability; 409 `endpoint_offline` when not connected; 409 with `code: deployment_not_sent` when the frame could not be queued.

- [ ] **Step 1: Write the failing fake-socket test**

Model on `TestCommandDispatchSettlesAndIsScoped` (`internal/api/commands_test.go`): set up org/env/admin, enroll and approve an agent, connect a fake socket whose `hello` advertises `deployment.apply`, send an inventory with one container (`shop-web`, project `shop`, image `sha256:b…`) and one image (`nginx:1` → `sha256:c…`), import the application (`services: {web: {image: nginx:1, environment: {TOKEN: apply-secret-canary}}}`), adopt, map, plan (`POST .../deployments`), then:

- `POST .../deployments/{id}/apply` `{"confirm":"shop"}`: 401 anonymous, 403 without CSRF, 403 for read-only, 202 for admin; the frame read from the socket is `deployment.apply` whose payload decodes to a `DeploymentRequest` with `Env["TOKEN"] == "apply-secret-canary"` and `ContainerName == "shop-web"`; the 202 body and `GET .../deployments/{id}` never contain the canary and show `state: applying`.
- A second apply returns 409.
- Write a `deployment.result` frame from the socket for another deployment ID: the row stays `applying`. Write one larger than `MaxDeploymentResultBytes`: the socket closes. Reconnect, send the real result (`succeeded`, identity `e…`): `GET` shows `succeeded`, resources rebound (`GET .../mapping` preview containers now list the new ID), instance `current_revision` 1 in `GET .../applications/instances`.
- Disconnect while a fresh plan is `applying` (plan again after the first settled, apply, close the socket): `waitFor` the row to read `unknown`.
- Capability: with a socket whose hello has no capabilities, apply returns 501 and the row stays `planned`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/api/ -run 'TestApplyDeployment' -count=1`
Expected: FAIL (404 on the route).

- [ ] **Step 3: Implement**

`server.go`: `s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications/{application}/deployments/{deployment}/apply", s.tenantRoute(s.handleApplyDeployment))`.

`application_handlers.go`:

```go
func (s *Server) handleApplyDeployment(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		Confirm string `json:"confirm"`
	}
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	app, id := r.PathValue("application"), r.PathValue("deployment")
	plan, err := s.store.Tenancy().ReadDeployment(r.Context(), a, app, id)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, plan.EndpointID)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityDeploymentApply) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable deployments")
		return
	}
	if !s.Connected(plan.EndpointID) {
		s.tenantError(w, store.ErrEndpointOffline)
		return
	}
	applied, req, err := s.store.Tenancy().ApplyDeployment(r.Context(), a, app, id, input.Confirm, s.config.Security.EncryptionKey)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.agents.deliver(plan.EndpointID, envelope(protocol.TypeDeploymentApply, req)) {
		_ = s.store.Tenancy().FailDeployment(r.Context(), applied.ID, "the endpoint disconnected before the deployment was sent")
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The endpoint disconnected before the deployment was sent", "code": "deployment_not_sent"})
		return
	}
	s.writeJSON(w, http.StatusAccepted, applied)
}
```

`agent_connect.go`:
- In the size check: `if f.Type == protocol.TypeDeploymentResult && len(f.Payload) > protocol.MaxDeploymentResultBytes { close protocol; return true }` and exempt that type from the `maxControlPayload` check.
- New case:

```go
	case protocol.TypeDeploymentResult:
		if pending {
			return false
		}
		var res protocol.DeploymentResult
		if err := json.Unmarshal(f.Payload, &res); err != nil || res.Validate() != nil {
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		if err := ts.SettleDeployment(fctx, c.endpointID, res); err != nil && !errors.Is(err, store.ErrNotFound) {
			id := "an unrecognised id"
			if _, uErr := uuid.Parse(res.Deployment); uErr == nil {
				id = res.Deployment
			}
			log.Printf("agent %s: settling deployment %s: %v", c.endpointID, id, err)
		}
```

- `markOffline`: after `AbandonCommands`, `if n, err := s.store.Tenancy().AbandonDeployments(wctx, c.endpointID); err == nil && n > 0 { log.Printf(...) }`.

`tenant_handlers.go`: `case errors.Is(err, store.ErrDeploymentInProgress): 409 {"error":"A deployment is being applied; wait for its result","code":"deployment_in_progress"}`.

`local_docker.go` and `cmd/agent/main.go`: add `Deploy: engine.Deploy` to the `client.Options` (in `cmd/agent`, only when the socket is configured, like `operate`).

- [ ] **Step 4: Real-Docker end-to-end test**

`internal/api/deployment_integration_test.go`, `TestApplyRealDocker`, gated by `KY_TEST_DOCKER_DEPLOY_IMAGE`, modelled on `TestBrowserExecRealDocker` and PR A's `deploy_integration_test.go` fixture: build the fixture image with `docker create` + `docker commit -c 'CMD ["sleep","300"]'` tagged `kyyardapplyfixture:local`; create network `kyyardapplyfixture_default`; run the fixture container `kyyardapplyfixture-web-1` on it with the Compose labels and alias `web`; start the server and `client.Run` with `docker.New("/var/run/docker.sock")` providing `Snapshot: engine.Snapshot`, `Inspect`, `Deploy: engine.Deploy`, `IdentityDir: t.TempDir()`; wait active; import `services: {web: {image: kyyardapplyfixture:local, restart: unless-stopped, environment: {TOKEN: apply-secret-canary}}}` with project name `kyyardapplyfixture`; adopt (endpoint = ag.id, project `kyyardapplyfixture`), map `web` to the fixture container, plan, apply (202); `waitFor` (extend the helper's deadline to 60 s locally) the deployment to reach `succeeded`; assert via the API that the mapping's owned container is the new ID, `current_revision == 1`, and via `docker inspect` that the new container runs under the old name on the network with `TOKEN=apply-secret-canary` in its env, and the old container is gone; the serialized deployment row never contains the canary. Cleanup removes containers by label, the network and the image tag, before and after.

Add `-run '^Test(BrowserExec|Apply)RealDocker$'` to the CI step's second `go test` line (the env var is already exported there).

- [ ] **Step 5: Run**

Run: `gofmt -w internal/api/*.go cmd/agent/*.go && go vet ./internal/api/ ./cmd/... && go test ./internal/api/ -run 'TestApplyDeployment' -count=1`, then `KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 go test -race ./internal/api -run '^TestApplyRealDocker$' -count=1 -v`, then `go test -count=1 ./internal/api/` on SQLite and PostgreSQL.
Expected: PASS; no fixture leftovers.

- [ ] **Step 6: Commit**

```bash
git add internal/api cmd/agent .github/workflows/ci.yml && git commit -m "feat: apply route, deployment result frames and agent wiring

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Web apply, status and gating fix

**Files:**
- Modify: `web/src/components/ApplicationDeploymentPlan.tsx`
- Modify: `web/src/components/ApplicationDeploymentPlan.test.tsx`
- Modify: `web/src/components/ApplicationAdoption.tsx` (`ApplicationInstance` type gains `current_revision`, `previous_revision`)
- Modify: `web/dist/**` via `make build-web`

**Interfaces:**
- Consumes: `POST ${base}/deployments/${id}/apply` `{confirm}` → 202; `GET ${base}/deployments` rows with `state`, `detail`, `applied_at`, `settled_at`, `result: {steps: [{service, step, outcome, detail}], services: [{service, container_id, image_id, created_unix}]} | null`.

- [ ] **Step 1: Write the failing tests**

Extend the existing test file (keep its `mapping`, `plan`, `stubFetch` fixtures; add `state`/`detail`/`result` fields to `plan`):

```tsx
const applying = { ...plan, state: 'applying', detail: '', result: null };
const settled = { ...plan, state: 'succeeded', detail: '', result: { steps: [{ service: 'web', step: 'create', outcome: 'succeeded', detail: '' }], services: [{ service: 'web', container_id: 'e'.repeat(64), image_id: `sha256:${'a'.repeat(64)}`, created_unix: 1800000000 }] } };
const refused = { ...plan, state: 'denied', detail: 'service web, step precondition: the container has mounts', result: { steps: [{ service: 'web', step: 'precondition', outcome: 'denied', detail: 'the container has mounts the definition does not describe' }, { service: 'web', step: 'image', outcome: 'skipped', detail: '' }], services: [] } };

it('applies on typed confirmation and polls until settled', async () => {
  vi.useFakeTimers();
  let reads = 0;
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    if (String(url).endsWith('/apply')) return new Response(JSON.stringify(applying), { status: 202 });
    reads++;
    return new Response(JSON.stringify([reads < 3 ? (reads === 1 ? plan : applying) : settled]));
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await vi.waitFor(() => screen.getByRole('button', { name: 'Apply deployment' }));
  fireEvent.change(screen.getByLabelText('Confirm apply project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply deployment' }));
  const post = fetcher.mock.calls.find(c => String(c[0]).endsWith('/apply'));
  expect(JSON.parse(String((post?.[1] as RequestInit).body))).toEqual({ confirm: 'shop' });
  await vi.advanceTimersByTimeAsync(5000);
  await vi.advanceTimersByTimeAsync(5000);
  await vi.waitFor(() => screen.getByText(/succeeded/i));
  const before = fetcher.mock.calls.length;
  await vi.advanceTimersByTimeAsync(20000);
  expect(fetcher.mock.calls.length).toBe(before); // polling stopped on a terminal state
  expect(screen.getByText('e'.repeat(64))).toBeTruthy();
  vi.useRealTimers();
});
it('explains a refused precondition with fixed text and hides server detail', async () => {
  vi.stubGlobal('fetch', stubFetch([refused]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(/configuration the definition does not describe/i);
  expect(screen.getByText(/skipped/i)).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Apply deployment' })).toBeNull();
});
it('shows the plan even when the mapping read fails', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => String(url).endsWith('/mapping') ? new Response('{"error":"secret-canary"}', { status: 409 }) : new Response(JSON.stringify([plan]))));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(`sha256:${'a'.repeat(64)}`);
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(screen.queryByRole('button', { name: 'Plan deployment' })).toBeNull();
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npx vitest run src/components/ApplicationDeploymentPlan.test.tsx`
Expected: FAIL (no apply button; plan hidden on mapping error).

- [ ] **Step 3: Implement**

In `ApplicationDeploymentPlan.tsx`:
- Types: `Deployment` gains `state: string; detail: string; applied_at?: string | null; settled_at?: string | null; result: { steps: { service: string; step: string; outcome: string; detail: string }[]; services: { service: string; container_id: string; image_id: string; created_unix: number }[] } | null`.
- `current` is the newest row for this instance (list is newest first; keep `planned ?? find`).
- Gating: render the plan/result section whenever `deployments.state === 'ready'`; render the plan form only when `mapping.state === 'ready'` and not mismatched; a mapping error shows `StateNotice` for the form area only.
- Polling: `useEffect` keyed on `current?.id` and `current?.state`: while `state === 'applying'`, `setInterval(deployments.reload, 5000)` cleared on unmount, on state change, and after 144 ticks (12 minutes).
- Apply form (only for `state === 'planned'` and not expired): text "Applying replaces the mapped containers on {endpoint} with revision {revision} of {project}. Nothing rolls back on failure; a failed run leaves the previous container renamed on the host." Input label `Confirm apply project`, button `Apply deployment`, POST `${base}/deployments/${current.id}/apply` with `{confirm}`; on 202 set the returned row as current and reload; on 409/501/403 fixed texts (`deployment_in_progress`, `endpoint_offline`, "Upgrade the host agent", permission); never render the body.
- Result section for non-planned states: state, `detail` rendered only through a fixed map keyed on the failing step's outcome and step: `precondition`+`denied` → "A mapped container has configuration the definition does not describe. Review it on the host before planning again."; `unknown` → "The host may or may not have acted. Inspect it before planning again."; `timed_out` → "The host did not answer in time."; `failed` → "A step failed on the host; the previous container may remain renamed with a .kyyard-prev suffix."; a 25-row steps table (service, step, outcome, and the step's detail only when it is one of the adapter's fixed texts, otherwise omitted); identities list of new container IDs.
- Keep the plan table and expiry as before.

Update `ApplicationInstance` type and rebuild dist.

- [ ] **Step 4: Run tests, typecheck, build**

Run: `cd web && npx vitest run && npm run build` then `make build-web` from the worktree root.
Expected: PASS; dist regenerated.

- [ ] **Step 5: Commit**

```bash
git add web && git commit -m "feat: apply a deployment plan from the UI with per-step status

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Docs, DOX pass, full CI

**Files:**
- Modify: `docs/agent-protocol.md` (Deployment apply: implemented; ledger, re-send, one at a time, size caps, abandon/unknown semantics)
- Modify: `docs/application-schema.md` (Deploy: states, apply preconditions, settle rules, unknown non-terminal, revision advance, history retained on release; note that plan-time inspection is a later slice)
- Modify: `docs/authorization-matrix.md` (`application.deploy` implemented; `secret.use` recorded on the deployment row via the apply audit)
- Modify: `internal/agent/AGENTS.md`, `internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md`, `internal/runtime/AGENTS.md` (the "only tests call it" sentence is now false)
- Modify: `KyYard-Implementation-Plan.md` section 8 ("Implemented M6 apply"; next: history/reapply/remove, then M7a)
- Modify: `docs/retention-policy.md` line 18 (events stored on the row; 160 KiB bound)

- [ ] **Step 1: Write the docs** in each file's existing style; remove every sentence that says apply is pending or that no frame is sent.
- [ ] **Step 2: Full CI**: `gofmt -l internal cmd` (empty), `make ci`, then `go test -count=1 ./internal/store/ ./internal/api/` on PostgreSQL. Expected: green.
- [ ] **Step 3: Commit** `docs: deployment apply contract` with the Co-Authored-By line. Do not push; the controller pushes and opens the PR.
