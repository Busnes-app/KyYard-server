# Deployment History, Reapply and Removal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator plan any saved revision the mapping covers (reapply), see deployment history, and remove a managed application through the agent while keeping its data; carry the audit rows for abandon/sweep/not-sent and the endpoint name on the deployment row.

**Architecture:** Migration 25 adds `deployments.kind` and `applications.removed_at`. `preflight` takes a revision; `PlanDeployment`/`ApplyDeployment` accept any saved revision. `RemoveApplication` inserts an `applying` row of kind `remove` and builds a `RemovalRequest`; the agent's deployer handles `deployment.remove` with the same runner; `docker.Client.Remove` stops and removes each adopted container (never volumes or networks); `SettleDeployment` branches on kind to release the instance and mark the application removed. The UI adds a revision selector, a history section and a removal action.

**Tech Stack:** Go 1.25, SQLite/PostgreSQL inline migrations, coder/websocket agent transport, Docker Engine API v1.41, React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-23-deployment-history-design.md`

## Global Constraints

- Worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/deployment-history`, branch `feat/deployment-history`. Never edit the root checkout; never use bare `git stash`.
- Store changes are tested on SQLite and PostgreSQL (`KY_TEST_POSTGRES_DSN=postgres://postgres:<pw>@127.0.0.1:15440/kyyard?sslmode=disable`; password from `docker inspect kyyard-access-pg`, never printed).
- Constants: `TypeDeploymentRemove = "deployment.remove"`, `CapabilityDeploymentRemove = "deployment.remove"`, `MaxRemovalTargets = 1000`, `DeploymentHistoryRetention = 90 * 24 * time.Hour`, `ApplicationRemovedRetention = 90 * 24 * time.Hour`. Removal request frames share `MaxDeploymentRequestBytes`; results share `MaxDeploymentResultBytes`.
- Removal never sends `v=1` or touches networks. A removal result carries no identities; a settle refuses one with `ErrInvalid`.
- `application.destroy` (org admin, env admin) authorises removal; `application.deploy` authorises planning any revision.
- Reapply: a plan targets `1 <= revision <= latest_revision`; apply resolves that revision's own bundle; missing images are the existing `image_not_reported` blocker.
- `gofmt -w` touched Go files; every commit ends with exactly `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. A prior revision whose services differ from the mapped set: planning must refuse with `service_unmapped`/`unassigned_adopted_containers`, never bind a container to the wrong service. Task 3 test "prior revision with a renamed service".
2. Removal settle when a container's `remove` step succeeded but a later target failed: only the removed container's resource row goes; the instance stays; the application is not marked removed. Task 3 test "partial removal".
3. A `deployment.remove` frame arriving while an apply is running: refused `denied` "already applying", never executed. Task 4 test.
4. A removal result naming identities: `ErrInvalid`, row untouched. Task 3 test.
5. Prune must not delete a settled row whose revision is the instance's current or previous revision, and must not delete a removed application before 90 days. Task 3 tests.

---

### Task 1: Protocol removal types

**Files:**
- Modify: `internal/agent/protocol/deployment.go`
- Test: `internal/agent/protocol/deployment_test.go`

**Interfaces (produces, verbatim):**

```go
const (
	TypeDeploymentRemove       = "deployment.remove"
	CapabilityDeploymentRemove = "deployment.remove"
	MaxRemovalTargets          = 1000
)
type RemovalRequest struct {
	Deployment string          `json:"deployment"`
	Endpoint   string          `json:"endpoint"`
	Project    string          `json:"project"`
	Deadline   time.Time       `json:"deadline"`
	Containers []RemovalTarget `json:"containers"`
}
type RemovalTarget struct {
	Service string           `json:"service"`
	Target  InspectionTarget `json:"target"`
}
func (r RemovalRequest) Validate(now time.Time) error
```

`Validate`: deployment UUID, endpoint `execStreamID`, project `deploymentProject`, deadline after now and at most `DeploymentLifetime` ahead, 1..`MaxRemovalTargets` containers, each `Service` matching `deploymentService`, each `Target.Validate()` nil, no duplicate service or container ID.

- [ ] **Step 1: Failing test** — `TestRemovalRequestValidation`: a good request with two targets (`web`, `unmapped-0123456789ab`) passes; mutations (bad uuid, past deadline, no containers, bad service, bad target, duplicate service, duplicate container) fail.
- [ ] **Step 2: Run** `go test ./internal/agent/protocol/ -run TestRemovalRequest -count=1` → compile failure.
- [ ] **Step 3: Implement** in `deployment.go` following `DeploymentRequest.Validate`'s style.
- [ ] **Step 4: Run** the package; PASS.
- [ ] **Step 5: Commit** `feat: deployment removal wire types`.

---

### Task 2: Adapter `Remove`

**Files:**
- Create: `internal/runtime/docker/remove.go`
- Test: `internal/runtime/docker/remove_test.go` (own fake Engine, modelled on `deploy_test.go`'s)
- Modify: `internal/runtime/docker/deploy_integration_test.go` (add `TestRemoveRealDocker` next to the deploy one, same fixture, gated by the same env var; CI's `-run` pattern already matches `^Test(Exec|Inspection|Deploy)RealDocker$` — extend it to `^Test(Exec|Inspection|Deploy|Remove)RealDocker$`)

**Interfaces:**
- Consumes: `deployRun.step`/`outcomeFor` (reuse by giving `deployRun` a `project` field or by a small `removeRun` with the same two methods; prefer reuse: `Remove` builds a `deployRun` with `req` zeroed and uses `step`/`outcomeFor`).
- Produces: `func (c *Client) Remove(parent context.Context, req protocol.RemovalRequest) protocol.DeploymentResult`.

Behaviour per target, in order, under `ctx` bounded by `req.Deadline`, with a guard before the first mutating step (`operationBudget + 2*callBudget` remaining, else `timed_out` "not enough time left before the deadline to remove this container safely"):

1. `precondition`: `GET /containers/{id}/json` under `callBudget`; 404 → `succeeded` with detail "the container was already gone" and the target's `stop`/`remove` recorded `skipped` (use a per-target `gone` flag); Id/Image/Created mismatch → `denied` "the container is not the one this plan was decided about".
2. `stop`: `POST /containers/{id}/stop?t=10` under `operationBudget`; 304 ok.
3. `remove`: `DELETE /containers/{id}` (no query) under `callBudget`; 404 ok; 409 → `failed` "the runtime refused: something still depends on this container".

`Services` stays empty. Overall `Outcome` as `Deploy`.

- [ ] **Step 1: Failing tests** — happy path (call order `GET, POST stop?t=10, DELETE` per target, no query on DELETE, result has 3 succeeded steps per target and no identities); already-gone target (1 call, `precondition` succeeded with the fixed detail, two skipped steps, overall succeeded); identity mismatch (`denied`, later targets skipped); stop 500 (`failed` at stop, no DELETE); remove 409 (`failed`); deadline guard (60 s deadline → `timed_out`, only the GET made); invalid request (no call); two targets second mismatched (first fully removed, overall `denied`).
- [ ] **Step 2: Run** → compile failure.
- [ ] **Step 3: Implement** `remove.go`.
- [ ] **Step 4: Run** `go test -race ./internal/runtime/docker/ -run 'TestRemove' -count=1` then the package.
- [ ] **Step 5: Real Docker** — `TestRemoveRealDocker`: build the fixture as `TestDeployRealDocker` does (committed image, project network, one container), call `Remove` with its identity, assert the container is gone, the network still exists, no `v=1` (a named volume attached with `-v kyyardremovefixture_data:/data` must still exist afterwards; remove it in cleanup), and no leftovers. Run it: `KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 go test -race ./internal/runtime/docker -run '^TestRemoveRealDocker$' -count=1 -v`.
- [ ] **Step 6: Commit** `feat: Docker adapter removes adopted containers without touching data`.

---

### Task 3: Store: revision-aware planning, removal, settle branch, retention, audit

**Files:**
- Modify: `internal/store/migrations/migrations.go` (version 25), `internal/store/tenancy_test.go:211` (add `25`)
- Modify: `internal/store/application_preflight.go` (`preflight(ctx, tx, a, app, lock, revision)`; `PreflightApplication` passes 0)
- Modify: `internal/store/application_deployment.go` (`Deployment` gains `Kind string json:"kind"` and `EndpointName string json:"endpoint_name"`; `deploymentColumns`/scan; `ListDeployments`/`ReadDeployment` join `endpoints e ON e.id=d.endpoint_id` for the name; `PlanDeployment` uses `r.Revision` (0/omitted → latest) and passes it to `preflight`; `PlannedService` unchanged)
- Modify: `internal/store/application_apply.go` (`ApplyDeployment`: replace `head != d.Revision` with `d.Revision < 1 || d.Revision > head`; `SettleDeployment` kind branch; `AbandonDeployments`/`FailDeployment`/sweep write audit rows; new `RemoveApplication`)
- Modify: `internal/store/applications.go` (`Application.RemovedAt *time.Time json:"removed_at"`, selected in `ListApplications`; `CreateApplication`/import unchanged)
- Modify: `internal/store/samples.go` (`Prune`: history retention and removed-application retention)
- Modify: `internal/store/store.go` (interface: `RemoveApplication`)
- Test: `internal/store/application_history_test.go` (new), existing apply tests adjusted

**Migration 25** (`deployment_history`):

```
SQLite:
ALTER TABLE deployments ADD COLUMN kind TEXT NOT NULL DEFAULT 'apply' CHECK(kind IN ('apply','remove'));
ALTER TABLE applications ADD COLUMN removed_at DATETIME;
Postgres:
ALTER TABLE deployments ADD COLUMN kind TEXT NOT NULL DEFAULT 'apply' CHECK(kind IN ('apply','remove'));
ALTER TABLE applications ADD COLUMN removed_at TIMESTAMPTZ;
```

**`preflight(ctx, tx, a, app, lock bool, revision int)`**: when `revision == 0` use `a.latest_revision` (current behaviour); otherwise join `application_revisions r ON r.number=?` for that revision (must exist, else `ErrNotFound`) while still reading `a.latest_revision` into `head` for the `head != m.Preview.Revision` check. `buildDeploymentPreflight` is unchanged: it already derives `service_unmapped` / `unassigned_adopted_containers` from the spec it is given versus `m.Bindings`. The returned `DeploymentPreflight.Revision` becomes the chosen revision.

**`RemoveApplication(ctx, a, app string, r RemovalBody) (*Deployment, *protocol.RemovalRequest, error)`** with `type RemovalBody struct { InstanceID string json:"instance_id"; Confirm string json:"confirm" }`:

```go
	return under withTenantTarget(ApplicationDestroy, app+"/removal/"+id):
	  lock application row (FOR UPDATE on postgres); read latest_revision, name;
	  read instance by id+application (org/env scoped) → project, endpoint_id, mapping_version; missing → ErrAdoptionChanged; confirm != project → ErrInvalid;
	  endpoint state must be active → ErrEndpointOffline;
	  any row for the instance in ('planned' unexpired, 'applying') → ErrDeploymentPlanned / ErrDeploymentInProgress;
	  DELETE expired planned rows for the instance;
	  read application_resources (container_id,name,image_id,created_at,service_name) ORDER BY container_id → at least one, else ErrAdoptionChanged;
	  build plan JSON {project, containers:[{service, container_id, image_id, created_unix, name}]} where service = service_name or "unmapped-"+container_id[:12];
	  build RemovalRequest{Deployment: id, Endpoint, Project, Deadline: now+DeploymentApplyDeadline, Containers: [{Service, Target{ContainerID, ImageID, CreatedUnix}}]}; Validate; size check;
	  INSERT deployments (kind 'remove', state 'applying', revision latest, spec_digest = revision digest, mapping_version, plan, created_by, created_at, expires_at = deadline, applied_by, applied_at, deadline);
	  return the scanned row and the request.
```

`created_unix` in the plan must be the resource's `created_at.Unix()` (adoption stored microseconds; the target uses whole seconds, matching inventory precision as `InspectionTarget` does).

**`SettleDeployment` kind branch**: read `kind` with the row. For `remove`: refuse `len(res.Services) > 0` with `ErrInvalid`; for each target in the plan whose `remove` step (or `precondition` with the already-gone detail) `succeeded`, `DELETE FROM application_resources WHERE instance_id=? AND endpoint_id=? AND container_id=?`; on `succeeded`: delete remaining resources, `DELETE FROM application_instances WHERE id=?`, `UPDATE applications SET removed_at=? WHERE id=?`; the revision advance is skipped; audit as today with action `application.destroy`. For `apply`: unchanged.

**Audit rows** for `AbandonDeployments`, the sweep (in `Prune`) and `FailDeployment`: insert into `audit_records` with `user_id 'system'` (abandon/sweep) or the caller's actor is unavailable in `FailDeployment` (use `'system'`), action `application.deploy`, resource `<app>/deployments/<id>`, `result 'unknown'` / `'failure'`, details `outcome=abandoned|swept|not_sent`, one row per affected deployment (select the affected IDs first, then update, then insert).

**`Prune`** additions: `DELETE FROM deployments WHERE settled_at IS NOT NULL AND settled_at<? AND NOT EXISTS (SELECT 1 FROM application_instances i WHERE i.id=deployments.instance_id AND deployments.revision IN (i.current_revision, i.previous_revision))` with `now-DeploymentHistoryRetention`; and for applications with `removed_at < now-ApplicationRemovedRetention`: delete their deployments, revisions, then the application (bounded by `PruneBatch`).

- [ ] **Step 1: Failing tests** (`application_history_test.go`, reuse `applyFixture`/`planFixture`):
  - `TestPlanPriorRevision`: append revision 3 (renamed service `api`); plan revision 2 → succeeds with `Revision 2`; plan revision 3 → `PreflightBlockedError` containing `service_unmapped`; plan revision 0/omitted → latest; plan revision 4 → `ErrNotFound`; apply the revision-2 plan → request `Revision 2` and env from revision 2's bundle.
  - `TestRemoveApplicationBuildsARemovalAndSettles`: removal → row `kind remove`, state `applying`, plan with one container (`service web`, name), request valid; settle `succeeded` with the three steps → resources gone, instance gone, `applications.removed_at` set, audit row `application.destroy`; `ListApplications` shows `removed_at`; `DiscardApplication` then succeeds.
  - `TestRemovalPreconditions`: wrong confirm `ErrInvalid`; wrong instance `ErrAdoptionChanged`; offline `ErrEndpointOffline`; live plan `ErrDeploymentPlanned`; applying row `ErrDeploymentInProgress`; operator/developer `ErrForbidden`.
  - `TestRemovalPartialSettle`: two adopted containers; result: first target's three steps succeeded, second target `precondition denied`, overall denied → first resource deleted, second kept, instance kept, `removed_at` null.
  - `TestRemovalResultRefusesIdentities`: `ErrInvalid`, row unchanged.
  - `TestPruneKeepsCurrentAndPreviousHistory` and `TestPruneRemovedApplications`.
  - `TestAbandonSweepAndFailAreAudited`: count audit rows with `details LIKE 'outcome=abandoned%'` etc.
  - `TestDeploymentCarriesEndpointName`.
- [ ] **Step 2: Run** → failures.
- [ ] **Step 3: Implement** as specified.
- [ ] **Step 4: Run** `go test ./internal/store/ -count=1` on both drivers.
- [ ] **Step 5: Commit** `feat: plan any saved revision, remove applications, audit every deployment transition`.

---

### Task 4: Agent: `Options.Remove` and the remove frame

**Files:**
- Modify: `internal/agent/client/connect.go` (`Options.Remove func(context.Context, protocol.RemovalRequest) protocol.DeploymentResult`; capability when set; frame case `TypeDeploymentRemove` with the same size/pending rules calling `deployments.handleRemoval`)
- Modify: `internal/agent/client/deployments.go` (generalise: `handle` becomes `handleApply`; new `handleRemoval` sharing the running-ID silence, Validate, endpoint, replay, busy, run, finish path; factor the common tail into `run(sessionCtx, id, exec func() protocol.DeploymentResult, out)`)
- Test: `internal/agent/client/deployments_test.go`

- [ ] **Step 1: Failing tests** — a removal frame runs `Options.Remove`, persists and delivers like apply; a removal while an apply runs → `denied` "already applying"; replay of a removal result; no runtime → `denied` "this agent has no runtime to remove"; capability advertised only with `Remove`.
- [ ] **Step 2: Run** → compile failure.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run** `go test -race ./internal/agent/... -count=1` and `-run 'TestDeployer' -count=10`.
- [ ] **Step 5: Commit** `feat: agent handles deployment.remove through the deployment runner`.

---

### Task 5: Server route, wiring, end-to-end

**Files:**
- Modify: `internal/api/server.go` (`POST .../applications/{application}/removal`)
- Modify: `internal/api/application_handlers.go` (`handleRemoveApplication`: strict JSON `{instance_id, confirm}`; `ReadEndpoint` for the instance's endpoint — read the instance via `ListApplicationInstances` filtered, or add `ReadApplicationInstance(ctx, a, app, id)` to the store if none exists — capability `deployment.remove` (501), connected (409), `RemoveApplication`, deliver `TypeDeploymentRemove` (failure → `FailDeployment` + 409 `deployment_not_sent`), 202)
- Modify: `internal/api/local_docker.go`, `cmd/agent/main.go` (`Remove: engine.Remove`)
- Modify: `internal/api/deployment_integration_test.go` (`TestApplyRealDocker` continues: after `succeeded`, POST removal, wait for `succeeded`, assert the container is gone via `docker inspect`, the network still exists, `GET .../applications/instances` no longer lists the instance, `GET .../applications` shows `removed_at`, then DELETE the application (discard) → 204)
- Test: `internal/api/deployment_apply_test.go` (add a removal section: codes 401/403 (CSRF, read-only, developer), 501 without capability, 202, frame type `deployment.remove` with the target, result settles, instance released, `removed_at` set; a removal result with identities leaves the row `unknown` via the refusal path)
- Modify: `.github/workflows/ci.yml` runtime `-run` pattern per Task 2.

- [ ] **Step 1–5** as the previous slices: failing socket test, implement, run api tests on both drivers with `-race`, run the real-Docker test with leftover checks, commit `feat: application removal route and agent wiring`.

---

### Task 6: Web

**Files:**
- Modify: `web/src/components/ApplicationDeploymentPlan.tsx` (revision selector fed by `GET ${base}/revisions`? there is no list route; use `latest_revision` from the draft prop passed down from `Applications.tsx` and offer `1..latest`; posts `revision`; warning text below latest: "Revision {n} uses its own saved environment values. Data written since is not reversed. The service mapping was reviewed against revision {latest}."; history section listing rows newest first with kind, revision, state, applied_by, applied_at, settled_at and an expandable `ResultSection`; instance current/previous revision shown from the instance prop)
- Modify: `web/src/components/Applications.tsx` (pass `latestRevision={draft.latest_revision}` and `instance={i}` to the plan panel; label removed applications "Removed {date}" and enable Discard for them)
- Modify: `web/src/components/ApplicationAdoption.tsx` (`Remove application` for instances: typed project confirmation naming what is kept and what is stopped; POST `${base}/removal` `{instance_id, confirm}`; 501/409/403 fixed texts; on 202 call `onChanged`)
- Tests: extend the plan panel test (selector posts `revision: 1` with the warning; history renders two rows and expands steps), adoption test (removal POST body, refusal texts), Applications test if present (removed label).
- `make build-web`; commit `feat: reapply a prior revision, show deployment history, remove applications`.

---

### Task 7: Docs, DOX, CI

`docs/agent-protocol.md` (Deployment remove), `docs/application-schema.md` (Reapply; History; Delete semantics implemented), `docs/authorization-matrix.md`, `docs/retention-policy.md` (history 90 d, removed applications 90 d, audit rows per transition), `internal/agent/AGENTS.md`, `internal/runtime/AGENTS.md`, `internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md`, root `AGENTS.md` verification bullet, `KyYard-Implementation-Plan.md` section 8 (M6 complete; next M7a). Then `make ci` and the PostgreSQL store/api suites. Commit `docs: deployment history, reapply and removal contract`.
