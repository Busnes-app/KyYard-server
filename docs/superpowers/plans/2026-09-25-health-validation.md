# Health Validation and Eligible Rollback (M7b PR 19) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Watch every succeeded apply for a bounded time and record a verdict; when an update a policy applied fails, return the instance to the recorded prior revision and images if it can, or stop with the reason, and pause the policy either way until an administrator resumes it.

**Architecture:** Inspections gain `health` and `restart_count` behind the `container.inspect.health` capability (Task 1). Migration 32 adds `deployment_validations`; `SettleDeployment` opens a row for every succeeded apply in the settling transaction, and the store owns the verdict rules as pure functions (`Judge`, `BaselineOf`, `presence`), the lifecycle writes, policy pauses and startup reconciliation (Task 2). The store decides rollback eligibility in one call and lets a plan pin image IDs instead of resolving tags (Task 3). `api.Server.RunValidations` polls every open row, inspects through the plan-time primitive as `system-validation`, finishes verdicts and runs the rollback as the policy's creator through the normal plan/apply/deliver path (Task 4). Policy runs expose their validation, `cmd/server` starts and waits for the loop (Task 5). The web shows verdicts, the rollback line, validation pause reasons and the capability warning (Task 6); documents close (Task 7).

**Tech Stack:** Go, SQLite + PostgreSQL 17, the coder/websocket agent protocol (fake agent socket in tests), Docker Engine API v1.41, React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-25-health-validation-design.md`

## Global Constraints

- Migration **32** (`deployment_validations`), both dialects, the spec's shape exactly:

  ```
  deployment_validations
    deployment_id TEXT PRIMARY KEY (FK deployments ON DELETE CASCADE), organization_id,
    environment_id, application_id, instance_id, endpoint_id, policy_run_id TEXT NULL,
    automated INTEGER NOT NULL (1 when policy_run_id is set), is_rollback INTEGER NOT NULL DEFAULT 0,
    phase TEXT CHECK (phase IN ('grace','observing','done')), started_at, observe_until,
    verdict TEXT CHECK (verdict IN ('', 'healthy','unhealthy','exited','restarting',
    'unverifiable','changed')), detail TEXT NOT NULL DEFAULT '' (≤ 255),
    baseline TEXT NOT NULL DEFAULT '' (JSON: per service {container_id, restart_count}),
    rollback_deployment_id TEXT NULL, rollback_outcome TEXT CHECK (rollback_outcome IN
    ('', 'applied','ineligible','failed')), rollback_detail TEXT NOT NULL DEFAULT '',
    correlation_id TEXT NOT NULL, finished_at NULL
  ```

  Timestamps are `DATETIME` (SQLite) / `TIMESTAMPTZ` (PostgreSQL), bound as `time.Time` in UTC. Text columns carry length CHECKs.
- Constants, exactly: `ValidationGrace = 30 s`, `ValidationWindow = 2 min`, `ValidationPoll = 20 s`. `observe_until = settled_at + 30 s + 2 min`.
- Settle (spec, verbatim): "`SettleDeployment` inserts the row in the same transaction that settles a `succeeded` `apply` deployment: `phase = grace`, `started_at = settled_at`, `observe_until = settled_at + 30 s + 2 min`, `policy_run_id` from the `policy_runs` row whose `deployment_id` is this deployment (none → manual), `is_rollback` when this deployment was itself a rollback (the validation row that dispatched it names it), `correlation_id` = the deployment's. A removal, a failed apply, or a `plan_only` plan gets no row."
- Verdicts (spec, verbatim):
  - `healthy`: at the end of the window every service's container is the settled identity, `State == running`, `Health ∈ {healthy, none}`, and `RestartCount` equals the baseline taken at the first observation after grace.
  - `unhealthy`: any observation shows `Health == unhealthy`.
  - `exited`: any observation shows `State ∉ {running, restarting}` or the container is gone.
  - `restarting`: `State == restarting`, or `RestartCount` above the baseline.
  - `changed`: the container ID differs from the settled identity (someone recreated it) — the deployment is no longer what was applied; no rollback.
  - `unverifiable`: the endpoint is offline for the whole window, the agent lacks `container.inspect.health`, or an inspection fails validation; detail names which.
  - A failing observation ends the window at once. `starting` is neither pass nor fail and keeps observing; a service still `starting` when the window ends is `unhealthy`.
- Unverifiable sentences from the spec, exactly: `the host could not be observed` (a window that ends with no successful observation), `the server was not running during the window` (a row past `observe_until` with no baseline at startup).
- Rollback reasons, exactly: `no_prior_revision`, `prior_definition_invalid`, `service_set_changed`, `prior_images_missing`, `rollback_in_flight`, `already_rolled_back`. Failure details from the spec: `not_sent`, `creator_lost`.
- Pause reasons, exactly: `rolled back after a failed update` (applied), `update failed and could not be rolled back: <rollback_detail>` (ineligible or failed), `update could not be validated: <detail>` (`unverifiable`); one `application.policy.paused` audit row; `Resume` is the acknowledgement.
- Audit action `application.validation`: `system`, resource `<app>/deployments/<id>`, result `success` for `healthy`, `failure` otherwise, correlation the deployment's.
- Capability `container.inspect.health`. `Health` is `none|starting|healthy|unhealthy` (`none` without a healthcheck); `RestartCount` is `0 ≤ n ≤ 1_000_000`; `Validate` requires both exactly when the capability is advertised.
- The loop: `api.Server.RunValidations(ctx, done)` beside `RunPolicies`, ticking every `ValidationPoll` under its own mutex, `done` closing only between ticks; `runServer` waits on it under the existing `backupWaitTimeout` before the store closes. An in-flight rollback plan/apply runs under `context.WithoutCancel`. Inspections count against the per-endpoint admission (2).
- Every store behaviour is tested on SQLite and PostgreSQL. PostgreSQL runs use the local container `kyyard-access-pg`; read the password into a variable, never print it: `PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test ...`. Below this prefix is written `PG=… go test`.
- `web/dist` is rebuilt with `make build-web` and committed in the web task (Task 6).
- `gofmt -w` every edited Go file and check `gofmt -l internal cmd` prints nothing before each commit. Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

Plan decisions where the code or the spec's silence forced a choice (each is reported to the reviewer; none changes a spec value):

1. **Rollback target at an unchanged revision.** `SettleDeployment` sets `previous_revision=current_revision,current_revision=<applied>`, and a policy update re-applies the latest revision with new digests, so after an automated update `previous_revision == current_revision` and "the newest succeeded apply at that revision" is the deployment under validation itself. `RollbackTarget` therefore takes the newest succeeded `apply` of the instance at `previous_revision` that is not this deployment and settled no later than it. Consequence, not changed: the first update after adoption has `previous_revision = 0` and is always `no_prior_revision`.
2. **Wire actor.** `InspectionOpen.Validate` requires the actor to match `^[a-zA-Z0-9_-]{1,128}$`, and an agent treats an invalid grant as a session error, so `system:validation` would drop the agent's connection. The grant's actor is `system-validation`; audit rows stay `system`.
3. **Automated detection has a race in the spec's rule.** `policy_runs.deployment_id` is written by `FinishPolicyRun` after the frame is delivered, so a fast settle would find no run and record an automated update as manual. The run names its deployment first (`AttachPolicyRunDeployment`, after the apply and before `deliver`); settle matches `policy_runs.deployment_id = <deployment> AND outcome IN ('','applied')`, so a `plan_only` run's plan applied by hand (outcome `planned`) is manual, as the spec requires.
4. **`unverifiable` does not roll back.** The spec's step 3 sends every automated verdict outside {`healthy`, `changed`} to the rollback decision, but gives `unverifiable` its own pause reason; nothing is known to be broken, so the policy pauses with `update could not be validated: <detail>` and `rollback_outcome` stays `''`.
5. **Gone vs. recreated.** An agent answers `unavailable` for a missing container, a changed one and an unreachable daemon alike. Presence comes from the store: the service's current `application_resources` binding (rebound by a later deployment → `changed`) and the endpoint's latest inventory, used only when received at or after the settle and not truncated (settled ID listed → present; absent but a container holds its name → `changed`; absent → `exited`; no such inventory → unknown, inspect and wait).
6. **The end of the window.** The table has no column for "a successful observation since the baseline". `healthy` is decided by one complete, passing observation at or after `observe_until`; a failing observation ends the window at any time; the loop keeps trying for `ValidationGrace` past `observe_until` (so one reconnect or a busy endpoint delays rather than fails) and then records `unverifiable` `the host could not be observed`. The baseline observation is judged like any other.
7. **Verdict precedence and detail.** Several failing services in one poll report the strongest: `changed` > `exited` > `restarting` > `unhealthy`. The failure verdicts' detail is the deciding service's name; `unverifiable` and a release use the fixed sentences. The audit row's details are the verdict word.
8. **More unverifiable sentences.** Besides the spec's two: `the agent cannot report container health`, `an inspection failed validation`. An application released mid-window is `changed` with `the application was released` (no rollback, no pause).
9. **A deleted policy.** `policy_run_id` is `REFERENCES policy_runs(id) ON DELETE SET NULL` with `CHECK (policy_run_id IS NULL OR automated=1)`; once the policy (and its runs) is deleted the validation still records its verdict, but there is no policy to act as or pause, so no rollback.
10. **More failure details.** A failed rollback's detail is `creator_lost` (`ErrForbidden`), `not_sent`, `endpoint_offline`, `policy_changed` (the apply's policy guard), `interrupted` (startup found a rollback named but undecided), the joined plan blocker codes, or the policy scheduler's codes (`not_adopted`, `mapping_required`, `adoption_changed`, `deployment_in_progress`, `invalid`, `error`).
11. **Rollback apply.** "Exactly as a policy run does" is taken literally: the rollback applies with `ApplyPolicyDeployment` (on this branch's working tree, not yet committed when this plan was written), so a policy deleted, switched to `plan_only` or saved by someone else during the rollback leaves its plan for a click (`failed`, `policy_changed`).
12. **Prune.** `Prune` deletes a deployment older than 90 days that is not the newest succeeded one at its revision; the rollback target is exactly such a row once an update re-applies its revision. `Prune` skips every deployment of an instance with an automated validation that may still roll back (window open, or failing verdict awaiting the decision); manual validations do not hold history, so `TestPruneKeepsCurrentAndPreviousHistory` is unchanged.
13. **One JSON shape.** Deployments and policy runs carry the same `validation` object: the row minus baseline and tenant IDs, with `rollback: {deployment_id, revision, outcome, detail}` (null before a decision). `revision` (the rollback deployment's, 0 once pruned) is added so the UI can say "Rolled back to revision N".
14. **`Validate` signature.** `ContainerInspection.Validate(target, now, health bool)`; the server passes whether the answering endpoint's stored capabilities include `container.inspect.health`, the agent passes `true` (it advertises the capability whenever it inspects). `s.inspect` gains the same flag; `inspectForPlan`'s agent round trip moves into `s.observe`, which validations share.
15. **Test hooks.** Validation observations go through the plan-time primitive, so `SetPlanInspectorForTest` is the inspector hook that returns health. A tick is synchronous (rows one after another, the rollback inside it), so there is no `WaitValidationsForTest`; `ValidationTickForTest` and `SetValidationClockForTest` suffice.
16. **Reconciliation.** `ReconcileAfterStart` settles a `grace` row past `observe_until` as `unverifiable` `the server was not running during the window`, and marks a row with `rollback_deployment_id` set and no outcome `failed` `interrupted`; neither is dispatched again. A decision still owed (done, failing verdict, no rollback named) is made on the first tick.
17. **Rollback after the tag moved.** A rollback runs the prior image ID; the host's tag still names the new image, so the next window would find the update again. The pause stops that until Resume; the documents say so.
18. **Migration replay test.** `TestTenancyUpgradeAndReopen` in `internal/store/tenancy_test.go` drops and replays the application tables; it must drop `deployment_validations` first and replay migration 32.

## Review Focus

1. A policy update re-applies the same revision (the normal case: `previous_revision == current_revision`): the rollback must pin the older apply's recorded images, never re-apply the deployment under validation. Pinned in Task 3 (`TestRollbackTargetReturnsThePriorImages`) and Task 4 (`TestValidationRollsBackAnUnhealthyUpdate` checks the frame's image).
2. A rollback's own validation fails: its verdict is recorded, no second rollback is dispatched, and the policy keeps its one pause row and reason. Pinned in Task 2 (`TestSettleOfARollbackNeverRollsBackAgain`) and Task 4 (`TestValidationNeverRollsBackARollback`).
3. An administrator applies by hand during an automated update's window: the automated validation is `changed` with no rollback and no pause, and the manual apply gets its own validation. Pinned in Task 4 (`TestValidationOfAnUpdateReplacedByAManualApply`).
4. The agent reconnects mid-window (a new connection, same endpoint): the window continues on the new connection and ends `healthy`, not `unverifiable`. Pinned in Task 4 (`TestValidationSurvivesAnAgentReconnect`).
5. The policy is deleted, or the application released, mid-window: a verdict is still recorded; nothing is rolled back or paused. Pinned in Task 4 (`TestValidationWhenThePolicyOrTheAdoptionGoes`).

---

### Task 1: Inspections carry health and the restart count

**Files:**
- Modify: `internal/agent/protocol/inspection.go` (fields, constants, `Validate`)
- Test: `internal/agent/protocol/inspection_test.go`
- Modify: `internal/runtime/docker/inspection.go` (`inspectedContainer`, `inspectionFacts`)
- Test: `internal/runtime/docker/inspection_test.go`, `internal/runtime/docker/inspection_integration_test.go`
- Modify: `internal/agent/client/connect.go` (advertise), `internal/agent/client/inspection.go` (validate with health)
- Test: `internal/agent/client/inspection_test.go`
- Modify: `internal/api/inspection.go` (`inspect` gains `health`; `handleContainerInspection` passes it), `internal/api/plan_inspection.go` (`observe`; `inspectForPlan` uses it)
- Test: `internal/api/inspection_test.go`
- Docs: `internal/agent/AGENTS.md`, `internal/runtime/AGENTS.md`, root `AGENTS.md` (Verification, real-Docker bullet)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  ```go
  // package protocol
  const CapabilityContainerInspectHealth = "container.inspect.health"
  const MaxRestartCount = 1_000_000
  // ContainerInspection gains:
  Health       string `json:"health"`        // none|starting|healthy|unhealthy
  RestartCount int    `json:"restart_count"` // 0..MaxRestartCount
  func (r ContainerInspection) Validate(target InspectionTarget, now time.Time, health bool) error

  // package api (unexported)
  func (s *Server) inspect(ctx context.Context, agent *agentConn, actor, org string, target protocol.InspectionTarget, health bool, allowed func() bool) (protocol.ContainerInspection, error)
  // observe is one inspection through the plan-time primitive: the planInspector test hook when
  // set, else the endpoint's current agent (store.ErrEndpointOffline without one).
  func (s *Server) observe(ctx context.Context, endpoint, actor, org string, target protocol.InspectionTarget, health bool, allowed func() bool) (protocol.ContainerInspection, error)
  ```

- [ ] **Step 1: Write the failing protocol test**

In `internal/agent/protocol/inspection_test.go` change the three existing calls `Validate(target, time.Now())` to `Validate(target, time.Now(), false)`:

```bash
sed -i 's/Validate(target, time.Now())/Validate(target, time.Now(), false)/' internal/agent/protocol/inspection_test.go
```

Append:

```go
// health and restart_count are present exactly when the answering agent advertises
// container.inspect.health: then an enum and a bounded count, otherwise both empty.
func TestInspectionHealthFields(t *testing.T) {
	target := InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	base := ContainerInspection{Target: target, ObservedAt: time.Now(), State: "running", RestartPolicy: "no", NetworkMode: "bridge", ImagePlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}, ConfigurationVerified: true, Unsupported: []string{}}
	for name, tc := range map[string]struct {
		health     string
		count      int
		advertised bool
		ok         bool
	}{
		"no healthcheck":             {"none", 0, true, true},
		"starting":                   {"starting", 0, true, true},
		"healthy after restarts":     {"healthy", 7, true, true},
		"unhealthy at the bound":     {"unhealthy", MaxRestartCount, true, true},
		"missing when advertised":    {"", 0, true, false},
		"unknown status":             {"secret-canary", 0, true, false},
		"negative count":             {"healthy", -1, true, false},
		"count past the bound":       {"healthy", MaxRestartCount + 1, true, false},
		"older agent":                {"", 0, false, true},
		"health from an older agent": {"healthy", 0, false, false},
		"count from an older agent":  {"", 1, false, false},
	} {
		r := base
		r.Health, r.RestartCount = tc.health, tc.count
		if err := r.Validate(target, time.Now(), tc.advertised); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	if CapabilityContainerInspectHealth != "container.inspect.health" || MaxRestartCount != 1_000_000 {
		t.Fatal("the wire vocabulary changed")
	}
}
```

- [ ] **Step 2: Write the failing Docker adapter tests**

In `internal/runtime/docker/inspection_test.go` change the two calls `out.Validate(target, time.Now())` (in `TestInspectionReportsWhatARecreateWouldDrop`) to `out.Validate(target, time.Now(), true)`:

```bash
sed -i 's/out.Validate(target, time.Now())/out.Validate(target, time.Now(), true)/' internal/runtime/docker/inspection_test.go
```

In `TestInspectionRefusesChangesAndInvalidFacts`, extend the changed-during-read list and its switch. Replace

```go
	for _, field := range []string{"identity", "ports", "restart", "mounts", "state", "config"} {
```

with

```go
	for _, field := range []string{"identity", "ports", "restart", "mounts", "state", "config", "health", "restarts"} {
```

and add two cases to the `switch field` inside the image branch, after `case "config":`'s body:

```go
					case "health":
						container["State"] = map[string]any{"Status": "running", "Health": map[string]any{"Status": "unhealthy"}}
					case "restarts":
						container["RestartCount"] = 1
```

Append:

```go
// Health and the restart count come from State.Health.Status and RestartCount; a container
// without a healthcheck is "none". Anything else, or a count out of bounds, is refused, and the
// healthcheck's log (its command's output) never leaves the adapter.
func TestInspectionReportsHealthAndRestarts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		health   any // State.Health; nil leaves it out
		count    any
		want     string
		restarts int
		err      error
	}{
		{"no healthcheck", nil, 0, "none", 0, nil},
		{"starting", map[string]any{"Status": "starting"}, 0, "starting", 0, nil},
		{"healthy", map[string]any{"Status": "healthy", "FailingStreak": 0, "Log": []any{map[string]any{"Output": "secret-canary"}}}, 3, "healthy", 3, nil},
		{"unhealthy at the bound", map[string]any{"Status": "unhealthy"}, 1_000_000, "unhealthy", 1_000_000, nil},
		{"unknown status", map[string]any{"Status": "secret-canary"}, 0, "", 0, ErrInspectionInvalid},
		{"negative count", nil, -1, "", 0, ErrInspectionInvalid},
		{"count past the bound", nil, 1_000_001, "", 0, ErrInspectionInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, container, image := inspectionFixture()
			state := map[string]any{"Status": "running", "Error": "secret-canary"}
			if tc.health != nil {
				state["Health"] = tc.health
			}
			container["State"], container["RestartCount"] = state, tc.count
			out, err := fakeInspection(t, serve(container, image)).InspectContainer(context.Background(), target)
			if tc.err != nil {
				if !errors.Is(err, tc.err) || out != nil {
					t.Fatalf("got %+v, %v; want %v", out, err, tc.err)
				}
				return
			}
			if err != nil || out.Health != tc.want || out.RestartCount != tc.restarts || out.Validate(target, time.Now(), true) != nil {
				t.Fatalf("got %+v, %v", out, err)
			}
			if raw, _ := json.Marshal(out); strings.Contains(string(raw), "secret-canary") {
				t.Fatal("the healthcheck log leaked")
			}
		})
	}
}
```

In `internal/runtime/docker/inspection_integration_test.go`, `TestInspectionRealDocker`: add `"--no-healthcheck",` to the first fixture's `docker run` arguments right after `"--pull", "never",` (so an image that defines a healthcheck cannot change the expected `none`), and replace

```go
	if out.Validate(target, time.Now()) != nil {
```

with

```go
	if out.Health != "none" || out.RestartCount != 0 {
		t.Fatalf("a container without a healthcheck: health %q, restarts %d", out.Health, out.RestartCount)
	}
	if out.Validate(target, time.Now(), true) != nil {
```

Append to the end of the test function, after the stale-target check:

```go
	// A healthcheck Docker has run reports its status; its command and log never leave the adapter.
	raw, err = exec.CommandContext(ctx, "docker", "run", "-d", "--pull", "never", "--network", "none", "--health-cmd", "true", "--health-interval", "1s", "--health-retries", "1", "--health-start-period", "0s", image, "sh", "-c", "sleep 120").CombinedOutput()
	if err != nil {
		t.Fatalf("healthcheck fixture: %v: %s", err, raw)
	}
	checked := strings.TrimSpace(string(raw))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", "rm", "-fv", checked).CombinedOutput(); err != nil {
			t.Errorf("healthcheck fixture cleanup: %v: %s", err, out)
		}
	})
	for {
		status, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Health.Status}}", checked).Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(status)) == "healthy" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the healthcheck never passed")
		case <-time.After(200 * time.Millisecond):
		}
	}
	raw, err = exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .}}", checked).Output()
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &identity); err != nil {
		t.Fatal("healthcheck fixture identity unreadable")
	}
	target = protocol.InspectionTarget{ContainerID: checked, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()}
	if out, err = c.InspectContainer(ctx, target); err != nil || out.Health != "healthy" || out.RestartCount != 0 || out.Validate(target, time.Now(), true) != nil {
		t.Fatalf("healthcheck inspection: %+v %v", out, err)
	}
```

- [ ] **Step 3: Write the failing agent test**

In `internal/agent/client/inspection_test.go`, in the hello check of the connect test, replace

```go
				advertised = slices.Contains(hello.Capabilities, "container.inspect") && slices.Contains(hello.Capabilities, "container.inspect.verdict")
```

with

```go
				advertised = slices.Contains(hello.Capabilities, "container.inspect") && slices.Contains(hello.Capabilities, "container.inspect.verdict") && slices.Contains(hello.Capabilities, "container.inspect.health")
```

Append:

```go
// An agent that inspects advertises container.inspect.health, so it answers only an observation
// that carries health; one without is withheld like any invalid result.
func TestInspectionTransportAnswersWithHealth(t *testing.T) {
	for name, tc := range map[string]struct {
		health string
		want   string
	}{"with health": {"healthy", "ok"}, "without health": {"", "unavailable"}} {
		t.Run(name, func(t *testing.T) {
			req := inspectionRequest()
			out := make(chan outFrame, 8)
			opts := &Options{inspectionSlots: make(chan struct{}, 2), Inspect: func(context.Context, protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
				return &protocol.ContainerInspection{Target: req.Target, ObservedAt: time.Now(), State: "running", RestartPolicy: "no", NetworkMode: "bridge", ImagePlatform: protocol.ImagePlatform{OS: "linux", Architecture: "amd64"}, Ports: []protocol.Port{}, Unsupported: []string{}, ConfigurationVerified: true, Health: tc.health}, nil
			}}
			s := newInspections(context.Background(), req.Endpoint, req.Connection, opts, out)
			if err := s.handle(execFrame(protocol.TypeInspectionOpen, req), true); err != nil {
				t.Fatal(err)
			}
			if reply := nextExecFrame(t, out, protocol.TypeInspectionResult).Payload.(protocol.InspectionResult); reply.Status != tc.want {
				t.Fatalf("status %q, want %q", reply.Status, tc.want)
			}
		})
	}
}
```

- [ ] **Step 4: Write the failing API test**

In `internal/api/inspection_test.go` replace the function `inspectionFixture` with:

```go
func inspectionFixture(t *testing.T) terminalFixture {
	return inspectionFixtureWith(t, []string{"container.inspect"})
}

// inspectionFixtureWith is inspectionFixture for an agent advertising capabilities.
func inspectionFixtureWith(t *testing.T, capabilities []string) terminalFixture {
	f := newTerminalFixture(t)
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities})
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()) + 2, ObservedAt: time.Now(), Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{{ID: terminalSpec.Container, ImageID: terminalSpec.ImageID, CreatedAt: time.Unix(1700000000, 0), State: "running"}}})
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeHeartbeat, nil)
	readEnvelope(t, f.ctx, f.ag.conn)
	return f
}
```

Append:

```go
// The server validates health against what the answering agent advertised: required and bounded
// from a container.inspect.health agent, absent from an older one (docs/agent-protocol.md).
func TestInspectionAPIChecksHealthAgainstTheCapability(t *testing.T) {
	withHealth := []string{"container.inspect", "container.inspect.health"}
	for _, tc := range []struct {
		name         string
		capabilities []string
		health       string
		restarts     int
		status       int
	}{
		{"health agent with health", withHealth, "unhealthy", 4, 200},
		{"health agent without health", withHealth, "", 0, 502},
		{"health agent with a runaway count", withHealth, "healthy", protocol.MaxRestartCount + 1, 502},
		{"older agent with health", []string{"container.inspect"}, "healthy", 0, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := inspectionFixtureWith(t, tc.capabilities)
			response := beginInspection(f)
			req := inspectionGrant(t, f)
			reply := inspectionReply(req)
			reply.Result.Health, reply.Result.RestartCount = tc.health, tc.restarts
			writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInspectionResult, reply)
			w := <-response
			if w.Code != tc.status {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
			if tc.status == 200 && (!strings.Contains(w.Body.String(), `"health":"unhealthy"`) || !strings.Contains(w.Body.String(), `"restart_count":4`)) {
				t.Fatalf("body: %s", w.Body.String())
			}
		})
	}
}
```

- [ ] **Step 5: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/agent/... ./internal/runtime/docker/ ./internal/api/ -run 'InspectionHealthFields|InspectionReportsHealthAndRestarts|InspectionRefusesChangesAndInvalidFacts|InspectionTransportAnswersWithHealth|InspectionAPIChecksHealthAgainstTheCapability'`
Expected: FAIL to compile with `too many arguments in call to r.Validate`, `undefined: MaxRestartCount` and `unknown field Health in struct literal`.

- [ ] **Step 6: Implement the protocol**

In `internal/agent/protocol/inspection.go`, add to `ContainerInspection` after `State`:

```go
	// Health is Docker's State.Health.Status, "none" without a healthcheck; RestartCount is the
	// runtime's restart count. Both are set exactly when the agent advertises
	// CapabilityContainerInspectHealth. The healthcheck's command and log never enter this type.
	Health       string `json:"health"`
	RestartCount int    `json:"restart_count"`
```

In the `const` block, after `CapabilityContainerInspectVerdict`, add:

```go
	// CapabilityContainerInspectHealth marks an agent whose inspections carry health and
	// restart_count; health validation needs it.
	CapabilityContainerInspectHealth = "container.inspect.health"
	// MaxRestartCount bounds a reported restart count.
	MaxRestartCount = 1_000_000
```

Replace the `Validate` signature and doc comment

```go
// Validate bounds an untrusted agent result before it reaches an HTTP response.
func (r ContainerInspection) Validate(target InspectionTarget, now time.Time) error {
```

with

```go
// Validate bounds an untrusted agent result before it reaches an HTTP response. health says the
// answering agent advertised CapabilityContainerInspectHealth: health and restart_count are then
// required and bounded, and otherwise absent.
func (r ContainerInspection) Validate(target InspectionTarget, now time.Time, health bool) error {
```

and insert after the `switch r.RestartPolicy { ... }` block:

```go
	if health {
		switch r.Health {
		case "none", "starting", "healthy", "unhealthy":
		default:
			return invalid
		}
		if r.RestartCount < 0 || r.RestartCount > MaxRestartCount {
			return invalid
		}
	} else if r.Health != "" || r.RestartCount != 0 {
		return invalid
	}
```

- [ ] **Step 7: Implement the Docker adapter**

In `internal/runtime/docker/inspection.go`, replace the first four fields of `inspectedContainer`

```go
	ID         string `json:"Id"`
	Image      string
	Created    time.Time
	State      *struct{ Status string }
```

with

```go
	ID           string `json:"Id"`
	Image        string
	Created      time.Time
	RestartCount int
	State        *struct {
		Status string
		// Health is absent without a healthcheck. Only its status is read: its log carries the
		// healthcheck command's output.
		Health *struct{ Status string }
	}
```

(the before/after `reflect.DeepEqual` now covers both, so a status or count that changes between the two container reads is `ErrInspectionChanged`). In `inspectionFacts`, after the `switch out.State { ... }` block, insert:

```go
	out.Health = "none"
	if raw.State.Health != nil {
		out.Health = raw.State.Health.Status
	}
	switch out.Health {
	case "none", "starting", "healthy", "unhealthy":
	default:
		return nil, ErrInspectionInvalid
	}
	if raw.RestartCount < 0 || raw.RestartCount > protocol.MaxRestartCount {
		return nil, ErrInspectionInvalid
	}
	out.RestartCount = raw.RestartCount
```

- [ ] **Step 8: Implement the agent**

In `internal/agent/client/connect.go` replace

```go
		capabilities = append(capabilities, protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict)
```

with

```go
		capabilities = append(capabilities, protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict, protocol.CapabilityContainerInspectHealth)
```

In `internal/agent/client/inspection.go` replace `result.Validate(req.Target, time.Now()) == nil` with `result.Validate(req.Target, time.Now(), true) == nil` (this agent advertises health whenever it inspects).

- [ ] **Step 9: Implement the server side**

In `internal/api/inspection.go`, change `inspect`'s signature to

```go
func (s *Server) inspect(ctx context.Context, agent *agentConn, actor, org string, target protocol.InspectionTarget, health bool, allowed func() bool) (protocol.ContainerInspection, error) {
```

extend its doc comment with "health is whether the endpoint advertises container.inspect.health, which decides what a valid answer carries.", replace `reply.Result.Validate(target, time.Now()) == nil` with `reply.Result.Validate(target, time.Now(), health) == nil`, and in `handleContainerInspection` replace

```go
	result, err := s.inspect(r.Context(), agent, a.ActorID, a.OrganizationID, target, func() bool { return s.inspectionAllowed(r, a, endpoint) })
```

with

```go
	result, err := s.inspect(r.Context(), agent, a.ActorID, a.OrganizationID, target, slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspectHealth), func() bool { return s.inspectionAllowed(r, a, endpoint) })
```

In `internal/api/plan_inspection.go`, replace everything from `inspect := s.planInspector` through the end of `inspectForPlan` with:

```go
	health := slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspectHealth)
	for _, svc := range pre.Services {
		if ctx.Err() != nil {
			break // the budget is spent: open no admission, send no expired grant
		}
		if svc.InspectionTarget == nil {
			continue
		}
		if in, err := s.observe(ctx, ep.ID, a.ActorID, a.OrganizationID, *svc.InspectionTarget, health, func() bool { return allowed(ctx) }); err == nil {
			out[svc.InspectionTarget.ContainerID] = in
		}
	}
	return out
}

// observe is one inspection of target on endpoint as actor through the plan-time primitive: the
// test hook when set, else the endpoint's current agent. Plans and health validation share it.
func (s *Server) observe(ctx context.Context, endpoint, actor, org string, target protocol.InspectionTarget, health bool, allowed func() bool) (protocol.ContainerInspection, error) {
	if s.planInspector != nil {
		return s.planInspector(ctx, target)
	}
	s.agents.mu.Lock()
	agent := s.agents.conns[endpoint]
	s.agents.mu.Unlock()
	if agent == nil {
		return protocol.ContainerInspection{}, store.ErrEndpointOffline
	}
	return s.inspect(ctx, agent, actor, org, target, health, allowed)
}
```

- [ ] **Step 10: Run to verify they pass**

Run: `gofmt -w internal/agent internal/runtime internal/api && go vet ./... && go test -race -count=1 ./internal/agent/... ./internal/runtime/... ./internal/api/`
Expected: PASS, including the existing inspection, plan-inspection and connect tests.

Then the real-Docker regression (needs a local Docker daemon): `docker pull alpine:3.24 && KY_TEST_DOCKER_INSPECTION_IMAGE=alpine:3.24 go test -race -count=1 ./internal/runtime/docker -run '^TestInspectionRealDocker$' -v`
Expected: PASS (not SKIP). If no Docker daemon is reachable, record the real-Docker health path as unproven in the commit body; CI runs it.

- [ ] **Step 11: DOX and commit**

`internal/agent/AGENTS.md`, the inspection bullet (it begins "Inspection uses inspection.open/result/cancel"): after "plans require it) when configured." add "They also advertise `container.inspect.health` (`protocol.CapabilityContainerInspectHealth`): answers carry `health` (`none|starting|healthy|unhealthy`) and `restart_count` (0..`MaxRestartCount`, 1,000,000), and `Validate(target, now, health)` requires both exactly when the answering agent advertised it; the server passes the endpoint's stored capability, the agent `true`."

`internal/runtime/AGENTS.md`, the `InspectContainer` bullet: append "It decodes `State.Health.Status` (absent is `none`; the healthcheck log is never decoded) and `RestartCount`, refuses an unknown status or a count outside 0..1,000,000, and compares both across the two container reads like `State`."

Root `AGENTS.md`, `## Verification`, the real-Docker bullet: after "redacted inspection (runtime and authorized HTTP/agent round trip" add ", with `health` `none` for a container without a healthcheck and `healthy` once Docker has run one".

```bash
git add internal/agent internal/runtime internal/api AGENTS.md
git commit -m "feat(agent): report container health and restart count in inspections" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Store — migration 32, the settle-time row, verdicts and the validation lifecycle

**Files:**
- Modify: `internal/store/migrations/migrations.go` (append migration 32 to `registry`)
- Create: `internal/store/validations.go`
- Modify: `internal/store/application_apply.go` (`SettleDeployment` opens the row)
- Modify: `internal/store/application_deployment.go` (`Deployment.Validation`, `selectDeployments`, `scanDeployment`)
- Modify: `internal/store/policy_schedule.go` (`AttachPolicyRunDeployment`)
- Modify: `internal/store/reconcile.go` (`ReconcileAfterStart` calls `reconcileValidations`)
- Modify: `internal/store/store.go` (`TenancyStore` gains six methods)
- Modify: `internal/store/tenancy_test.go` (`TestTenancyUpgradeAndReopen` drops and replays the new table)
- Test: `internal/store/validations_test.go`
- Docs: `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: `tenancyStore.auditPolicy`, `storedDeploymentResult`, `AuditPolicyPaused`, `PolicyPaused`/`PolicyActive`, `protocol.CleanText`; test fixtures `planFixture`, `planRequest`, `applyFixture`, `settledResult`, `dailyPolicy`, `instant`, `policyMember`.
- Produces:
  ```go
  // package store
  const (
  	ValidationGrace, ValidationWindow, ValidationPoll = 30 * time.Second, 2 * time.Minute, 20 * time.Second
  	MaxPendingValidations = 256
  	AuditValidation       = "application.validation"
  	PhaseGrace, PhaseObserving, PhaseDone = "grace", "observing", "done"
  	VerdictHealthy, VerdictUnhealthy, VerdictExited, VerdictRestarting, VerdictUnverifiable, VerdictChanged = "healthy", "unhealthy", "exited", "restarting", "unverifiable", "changed"
  	RollbackApplied, RollbackIneligible, RollbackFailed = "applied", "ineligible", "failed"
  	RollbackNoPriorRevision, RollbackPriorDefinitionInvalid, RollbackServiceSetChanged = "no_prior_revision", "prior_definition_invalid", "service_set_changed"
  	RollbackPriorImagesMissing, RollbackInFlight, RollbackAlreadyRolledBack = "prior_images_missing", "rollback_in_flight", "already_rolled_back"
  	RollbackCreatorLost, RollbackNotSent, RollbackInterrupted = "creator_lost", "not_sent", "interrupted"
  	ValidationDetailUnobserved = "the host could not be observed"
  	ValidationDetailServerDown = "the server was not running during the window"
  	ValidationDetailNoHealth   = "the agent cannot report container health"
  	ValidationDetailInvalid    = "an inspection failed validation"
  	ValidationDetailReleased   = "the application was released"
  	ValidationReasonRolledBack    = "rolled back after a failed update"
  	ValidationReasonNotRolledBack = "update failed and could not be rolled back: "
  	ValidationReasonUnverified    = "update could not be validated: "
  	PresencePresent, PresenceGone, PresenceReplaced, PresenceUnknown = "present", "gone", "replaced", "unknown"
  )
  type ValidationRollback struct { DeploymentID string; Revision int; Outcome, Detail string } // json: deployment_id, revision, outcome, detail
  type Validation struct { DeploymentID, PolicyRunID string; Automated, IsRollback bool; Phase string; StartedAt, ObserveUntil time.Time; Verdict, Detail string; Rollback *ValidationRollback; CorrelationID string; FinishedAt *time.Time }
  type ServiceBaseline struct { ContainerID string `json:"container_id"`; RestartCount int `json:"restart_count"` }
  type ObservedService struct { protocol.DeploymentIdentity; Presence string }
  type PendingValidation struct { Validation; OrganizationID, EnvironmentID, ApplicationID, InstanceID, EndpointID string; Baseline map[string]ServiceBaseline; Services []ObservedService; Health, Released bool; PolicyID, CreatedBy string }
  type Observation struct { Service, Presence string; Inspection *protocol.ContainerInspection }
  func Judge(obs []Observation, baseline map[string]ServiceBaseline, final bool) (verdict, detail string)
  func BaselineOf(obs []Observation) (map[string]ServiceBaseline, bool)
  // Deployment gains: Validation *Validation `json:"validation,omitempty"`
  PendingValidations(ctx context.Context) ([]PendingValidation, error)
  BeginObservation(ctx context.Context, deploymentID string, baseline map[string]ServiceBaseline) error
  FinishValidation(ctx context.Context, deploymentID, verdict, detail string) (bool, error) // true: a rollback decision is due
  MarkRollbackPlanned(ctx context.Context, deploymentID, rollbackDeploymentID string) error
  MarkRollbackOutcome(ctx context.Context, deploymentID, outcome, detail string) error
  AttachPolicyRunDeployment(ctx context.Context, runID, deploymentID string) error
  ```
  Unexported, used by Task 3: `validationColumns`, `validationScan`, `boundServices`, `rowQuerier`. Test helpers later tasks reuse: `imageD`, `imageX`, `priorID`, `updatedID`, `tagged`, `webContainer`, `putInventory`, `deployFixture`, `validationFixture`.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/validations_test.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

var (
	imageD    = "sha256:" + strings.Repeat("d", 64) // planFixture's nginx:1
	imageX    = "sha256:" + strings.Repeat("9", 64) // the update
	priorID   = strings.Repeat("e", 64)
	updatedID = strings.Repeat("f", 64)
)

func tagged(id string, tags ...string) protocol.Image {
	return protocol.Image{ID: id, Tags: tags, Digests: []string{}}
}

// webContainer is the fixture's web container as an inventory reports it.
func webContainer(id, image string, created time.Time) protocol.Container {
	return protocol.Container{ID: id, Name: "shop-web", ImageID: image, State: "running", CreatedAt: created, ComposeProject: "shop", Mounts: []protocol.Mount{}}
}

// putInventory replaces the endpoint's inventory and marks it received and observed now.
func putInventory(t *testing.T, st *SQLStore, endpoint string, containers []protocol.Container, images []protocol.Image) {
	t.Helper()
	raw, _ := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: containers, Images: images, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}})
	now := time.Now().UTC()
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE endpoint_inventory SET snapshot=?,received_at=?,observed_at=? WHERE endpoint_id=?`), string(raw), now, now, endpoint); err != nil {
		t.Fatal(err)
	}
}

// deployFixture plans and applies the latest revision as a with images on the host, runs between
// (when set) after the apply, and settles it succeeded with the web service on container newID
// running the planned image. The inventory then shows newID beside images.
func deployFixture(t *testing.T, st *SQLStore, a TenantAccess, app *Application, endpoint, newID string, images []protocol.Image, between func(*Deployment)) *Deployment {
	t.Helper()
	ctx := context.Background()
	ts := st.Tenancy()
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := m.Preview.Containers[0]
	putInventory(t, st, endpoint, []protocol.Container{webContainer(c.ID, c.ImageID, c.CreatedAt)}, images)
	if m, err = ts.ReadApplicationMapping(ctx, a, app.ID); err != nil {
		t.Fatal(err)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if between != nil {
		between(d)
	}
	created := time.Now().UTC().Truncate(time.Second)
	service := d.Plan.Services[0]
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: service.Name, Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{{Service: service.Name, ContainerID: newID, ImageID: service.ImageID, CreatedUnix: created.Unix()}}}
	if err := ts.SettleDeployment(ctx, endpoint, res); err != nil {
		t.Fatal(err)
	}
	putInventory(t, st, endpoint, []protocol.Container{webContainer(newID, service.ImageID, created)}, images)
	out, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// validationFixture is planFixture with an apply-mode policy and two succeeded applies of
// revision 1: prior, by hand on imageD (its validation finished healthy), then updated, by the
// policy's run on imageX (validation open, in grace). The host keeps imageD, untagged.
func validationFixture(t *testing.T) (*SQLStore, TenantAccess, *Application, string, *UpdatePolicy, string, *Deployment, *Deployment) {
	t.Helper()
	st, a, app, endpoint, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	prior := deployFixture(t, st, a, app, endpoint, priorID, []protocol.Image{tagged(imageD, "nginx:1")}, nil)
	if _, err := ts.FinishValidation(ctx, prior.ID, VerdictHealthy, ""); err != nil {
		t.Fatal(err)
	}
	run, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	updated := deployFixture(t, st, a, app, endpoint, updatedID, []protocol.Image{tagged(imageD), tagged(imageX, "nginx:1")}, func(d *Deployment) {
		if err := ts.AttachPolicyRunDeployment(ctx, run, d.ID); err != nil {
			t.Fatal(err)
		}
	})
	if err := ts.FinishPolicyRun(ctx, run, RunApplied, updated.ID, ""); err != nil {
		t.Fatal(err)
	}
	return st, a, app, endpoint, p, run, prior, updated
}

func TestDeploymentValidationsTable(t *testing.T) {
	st, _, _, _, _, _, prior, updated := validationFixture(t)
	ctx := context.Background()
	for name, stmt := range map[string]string{
		"phase":             `UPDATE deployment_validations SET phase='waiting' WHERE deployment_id=?`,
		"verdict":           `UPDATE deployment_validations SET verdict='fine' WHERE deployment_id=?`,
		"rollback outcome":  `UPDATE deployment_validations SET rollback_outcome='maybe' WHERE deployment_id=?`,
		"detail":            `UPDATE deployment_validations SET detail='` + strings.Repeat("d", 256) + `' WHERE deployment_id=?`,
		"manual with a run": `UPDATE deployment_validations SET automated=0 WHERE deployment_id=?`,
	} {
		if _, err := st.db.ExecContext(ctx, st.rebind(stmt), updated.ID); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	rollback := uuid.NewString()
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE deployment_validations SET rollback_deployment_id=? WHERE deployment_id=?`), rollback, updated.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE deployment_validations SET rollback_deployment_id=? WHERE deployment_id=?`), rollback, prior.ID); err == nil {
		t.Fatal("two validations named one rollback")
	}
	// The run may go first (its policy deleted): the validation stays, still automated.
	if _, err := st.db.ExecContext(ctx, `DELETE FROM policy_runs`); err != nil {
		t.Fatal(err)
	}
	var automated, orphaned int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT automated,(CASE WHEN policy_run_id IS NULL THEN 1 ELSE 0 END) FROM deployment_validations WHERE deployment_id=?`), updated.ID).Scan(&automated, &orphaned); err != nil || automated != 1 || orphaned != 1 {
		t.Fatalf("after the run went: automated=%d orphaned=%d %v", automated, orphaned, err)
	}
	// A validation goes with its deployment.
	if _, err := st.db.ExecContext(ctx, st.rebind(`DELETE FROM deployments WHERE id=?`), updated.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployment_validations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("validations left: %d %v", n, err)
	}
}

func TestSettleOpensAValidation(t *testing.T) {
	st, a, app, _, _, run, prior, updated := validationFixture(t)
	ctx := context.Background()
	v := updated.Validation
	if v == nil || v.DeploymentID != updated.ID || !v.Automated || v.IsRollback || v.PolicyRunID != run || v.Phase != PhaseGrace || v.Verdict != "" || v.Detail != "" || v.Rollback != nil || v.FinishedAt != nil || v.CorrelationID != updated.CorrelationID {
		t.Fatalf("automated: %+v", v)
	}
	if !v.StartedAt.Equal(*updated.SettledAt) || !v.ObserveUntil.Equal(updated.SettledAt.Add(ValidationGrace+ValidationWindow)) {
		t.Fatalf("window %v..%v, settled %v", v.StartedAt, v.ObserveUntil, *updated.SettledAt)
	}
	got, err := st.Tenancy().ReadDeployment(ctx, a, app.ID, prior.ID)
	if err != nil || got.Validation == nil || got.Validation.Automated || got.Validation.PolicyRunID != "" || got.Validation.Phase != PhaseDone || got.Validation.Verdict != VerdictHealthy || got.Validation.FinishedAt == nil {
		t.Fatalf("manual: %+v %v", got.Validation, err)
	}
	raw, _ := json.Marshal(updated)
	if !strings.Contains(string(raw), `"validation":{`) || strings.Contains(string(raw), "baseline") {
		t.Fatalf("json: %s", raw)
	}
	list, err := st.Tenancy().ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 2 || list[0].Validation == nil || list[1].Validation == nil {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestSettleOpensNoValidationForAFailedApply(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployment_validations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("validations: %d %v", n, err)
	}
	if got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID); err != nil || got.Validation != nil {
		t.Fatalf("failed apply: %+v %v", got, err)
	}
}

// A plan_only run's plan applied by hand is a manual apply: validated, never rolled back.
func TestSettleOfAPlanOnlyRunIsManual(t *testing.T) {
	st, a, app, endpoint, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	in := dailyPolicy()
	in.Mode = PolicyModePlanOnly
	p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	run, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	d := deployFixture(t, st, a, app, endpoint, priorID, []protocol.Image{tagged(imageD, "nginx:1")}, func(d *Deployment) {
		if err := ts.FinishPolicyRun(ctx, run, RunPlanned, d.ID, ""); err != nil {
			t.Fatal(err)
		}
	})
	if v := d.Validation; v == nil || v.Automated || v.PolicyRunID != "" {
		t.Fatalf("plan_only applied by hand: %+v", v)
	}
}

// A deployment a validation dispatched is a rollback: validated, never rolled back itself (the
// oscillation guard, Review Focus 2).
func TestSettleOfARollbackNeverRollsBackAgain(t *testing.T) {
	st, a, app, endpoint, _, _, _, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if decide, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil || !decide {
		t.Fatalf("automated failure: decide=%v %v", decide, err)
	}
	back := deployFixture(t, st, a, app, endpoint, strings.Repeat("7", 64), []protocol.Image{tagged(imageD), tagged(imageX, "nginx:1")}, func(d *Deployment) {
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, d.ID); err != nil {
			t.Fatal(err)
		}
	})
	if v := back.Validation; v == nil || !v.IsRollback || v.Automated {
		t.Fatalf("rollback: %+v", v)
	}
	if decide, err := ts.FinishValidation(ctx, back.ID, VerdictUnhealthy, "web"); err != nil || decide {
		t.Fatalf("a rollback's failure asked for another rollback: %v %v", decide, err)
	}
	if err := ts.MarkRollbackOutcome(ctx, back.ID, RollbackIneligible, RollbackNoPriorRevision); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a rollback took a rollback decision: %v", err)
	}
	pending, err := ts.PendingValidations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.DeploymentID == back.ID {
			t.Fatal("a rollback's failure awaits a decision")
		}
	}
}

func TestJudge(t *testing.T) {
	running := func(health string, restarts int) *protocol.ContainerInspection {
		return &protocol.ContainerInspection{State: "running", Health: health, RestartCount: restarts}
	}
	in := func(state string) *protocol.ContainerInspection {
		return &protocol.ContainerInspection{State: state, Health: "none"}
	}
	web := func(presence string, i *protocol.ContainerInspection) Observation {
		return Observation{Service: "web", Presence: presence, Inspection: i}
	}
	db := func(presence string, i *protocol.ContainerInspection) Observation {
		return Observation{Service: "db", Presence: presence, Inspection: i}
	}
	fine := db(PresencePresent, running("healthy", 0))
	base := map[string]ServiceBaseline{"web": {ContainerID: updatedID, RestartCount: 2}, "db": {ContainerID: priorID}}
	for _, tc := range []struct {
		name            string
		obs             []Observation
		final           bool
		verdict, detail string
	}{
		{"healthy before the end goes on", []Observation{web(PresencePresent, running("healthy", 2)), fine}, false, "", ""},
		{"healthy at the end", []Observation{web(PresencePresent, running("healthy", 2)), fine}, true, VerdictHealthy, ""},
		{"no healthcheck at the end", []Observation{web(PresencePresent, running("none", 2)), fine}, true, VerdictHealthy, ""},
		{"unknown presence inspected fine", []Observation{web(PresenceUnknown, running("healthy", 2)), fine}, true, VerdictHealthy, ""},
		{"unhealthy at once", []Observation{web(PresencePresent, running("unhealthy", 2)), fine}, false, VerdictUnhealthy, "web"},
		{"starting goes on", []Observation{web(PresencePresent, running("starting", 2)), fine}, false, "", ""},
		{"starting at the end", []Observation{web(PresencePresent, running("starting", 2)), fine}, true, VerdictUnhealthy, "web"},
		{"exited", []Observation{web(PresencePresent, in("exited")), fine}, false, VerdictExited, "web"},
		{"dead", []Observation{web(PresencePresent, in("dead")), fine}, false, VerdictExited, "web"},
		{"paused", []Observation{web(PresencePresent, in("paused")), fine}, false, VerdictExited, "web"},
		{"gone", []Observation{web(PresenceGone, nil), fine}, false, VerdictExited, "web"},
		{"restarting", []Observation{web(PresencePresent, in("restarting")), fine}, false, VerdictRestarting, "web"},
		{"restarted since the baseline", []Observation{web(PresencePresent, running("healthy", 3)), fine}, false, VerdictRestarting, "web"},
		{"recreated", []Observation{web(PresenceReplaced, nil), fine}, false, VerdictChanged, "web"},
		{"changed outranks a failure", []Observation{web(PresenceReplaced, nil), db(PresencePresent, running("unhealthy", 0))}, false, VerdictChanged, "web"},
		{"exited outranks unhealthy", []Observation{web(PresencePresent, running("unhealthy", 2)), db(PresenceGone, nil)}, false, VerdictExited, "db"},
		{"a failure needs no complete poll", []Observation{web(PresenceUnknown, nil), db(PresencePresent, running("unhealthy", 0))}, true, VerdictUnhealthy, "db"},
		{"an unobserved service holds the end open", []Observation{web(PresencePresent, nil), fine}, true, "", ""},
		{"nothing observed goes on", []Observation{web(PresencePresent, nil), db(PresencePresent, nil)}, false, "", ""},
	} {
		if v, d := Judge(tc.obs, base, tc.final); v != tc.verdict || d != tc.detail {
			t.Errorf("%s: %q %q, want %q %q", tc.name, v, d, tc.verdict, tc.detail)
		}
	}
}

func TestBaselineOf(t *testing.T) {
	in := &protocol.ContainerInspection{Target: protocol.InspectionTarget{ContainerID: updatedID}, State: "running", Health: "healthy", RestartCount: 4}
	got, ok := BaselineOf([]Observation{{Service: "web", Presence: PresencePresent, Inspection: in}, {Service: "db", Presence: PresenceGone}})
	if !ok || len(got) != 1 || got["web"] != (ServiceBaseline{ContainerID: updatedID, RestartCount: 4}) {
		t.Fatalf("baseline: %+v %v", got, ok)
	}
	if _, ok := BaselineOf([]Observation{{Service: "web", Presence: PresenceUnknown}}); ok {
		t.Fatal("a baseline without an observation")
	}
}

func TestPresence(t *testing.T) {
	id := protocol.DeploymentIdentity{Service: "web", ContainerID: updatedID, ImageID: imageX}
	bound := map[string]boundResource{"web": {containerID: updatedID, name: "shop-web"}}
	now := time.Now()
	snap := func(cs ...protocol.Container) *protocol.Snapshot { return &protocol.Snapshot{Containers: cs} }
	for _, tc := range []struct {
		name  string
		bound map[string]boundResource
		snap  *protocol.Snapshot
		want  string
	}{
		{"reported", bound, snap(webContainer(updatedID, imageX, now)), PresencePresent},
		{"no inventory since the settle", bound, nil, PresenceUnknown},
		{"gone", bound, snap(), PresenceGone},
		{"recreated under its name", bound, snap(webContainer(priorID, imageX, now)), PresenceReplaced},
		{"rebound by a later deployment", map[string]boundResource{"web": {containerID: priorID, name: "shop-web"}}, snap(webContainer(updatedID, imageX, now)), PresenceReplaced},
		{"unbound", map[string]boundResource{}, snap(webContainer(updatedID, imageX, now)), PresenceReplaced},
	} {
		if got := presence(id, tc.bound, tc.snap); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestPendingValidations(t *testing.T) {
	st, a, app, endpoint, p, _, _, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	one := func() PendingValidation {
		t.Helper()
		pending, err := ts.PendingValidations(ctx)
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending: %+v %v", pending, err)
		}
		return pending[0]
	}
	got := one()
	if got.DeploymentID != updated.ID || got.OrganizationID != a.OrganizationID || got.EnvironmentID != a.EnvironmentID || got.ApplicationID != app.ID || got.InstanceID != updated.InstanceID || got.EndpointID != endpoint || got.PolicyID != p.ID || got.CreatedBy != "actor" || got.Health || got.Released || got.Baseline != nil || len(got.Services) != 1 {
		t.Fatalf("pending: %+v", got)
	}
	if s := got.Services[0]; s.Service != "web" || s.ContainerID != updatedID || s.ImageID != imageX || s.CreatedUnix != updated.Result.Services[0].CreatedUnix || s.Presence != PresencePresent {
		t.Fatalf("service: %+v", s)
	}
	if err := ts.SetEndpointCapabilities(ctx, endpoint, []string{protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict, protocol.CapabilityContainerInspectHealth, protocol.CapabilityDeploymentApply}); err != nil {
		t.Fatal(err)
	}
	if !one().Health {
		t.Fatal("the health capability was not read")
	}
	images := []protocol.Image{tagged(imageD), tagged(imageX, "nginx:1")}
	for _, tc := range []struct {
		name       string
		containers []protocol.Container
		received   time.Time
		want       string
	}{
		{"inventory from before the settle", []protocol.Container{}, updated.SettledAt.Add(-time.Minute), PresenceUnknown},
		{"gone", []protocol.Container{}, time.Now().UTC(), PresenceGone},
		{"recreated outside KyYard", []protocol.Container{webContainer(strings.Repeat("7", 64), imageX, time.Now().UTC())}, time.Now().UTC(), PresenceReplaced},
	} {
		putInventory(t, st, endpoint, tc.containers, images)
		if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_inventory SET received_at=? WHERE endpoint_id=?`), tc.received, endpoint); err != nil {
			t.Fatal(err)
		}
		if got := one().Services[0].Presence; got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	// Awaiting its rollback decision it stays listed; decided, it leaves.
	if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
		t.Fatal(err)
	}
	if got := one(); got.Phase != PhaseDone || got.Verdict != VerdictUnhealthy {
		t.Fatalf("awaiting a decision: %+v", got)
	}
	if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackIneligible, RollbackPriorImagesMissing); err != nil {
		t.Fatal(err)
	}
	if pending, err := ts.PendingValidations(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("after the decision: %+v %v", pending, err)
	}
}

func TestPendingValidationOfAReleasedApplication(t *testing.T) {
	st, a, app, _, _, _, _, updated := validationFixture(t)
	ctx := context.Background()
	if err := st.Tenancy().ReleaseApplication(ctx, a, app.ID, updated.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	pending, err := st.Tenancy().PendingValidations(ctx)
	if err != nil || len(pending) != 1 || !pending[0].Released {
		t.Fatalf("released: %+v %v", pending, err)
	}
}

func TestBeginObservation(t *testing.T) {
	st, a, app, _, _, _, _, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	baseline := map[string]ServiceBaseline{"web": {ContainerID: updatedID, RestartCount: 2}}
	if err := ts.BeginObservation(ctx, updated.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if err := ts.BeginObservation(ctx, updated.ID, baseline); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second baseline: %v", err)
	}
	if got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID); err != nil || got.Validation.Phase != PhaseObserving {
		t.Fatalf("phase: %+v %v", got.Validation, err)
	}
	pending, err := ts.PendingValidations(ctx)
	if err != nil || len(pending) != 1 || pending[0].Baseline["web"] != baseline["web"] {
		t.Fatalf("baseline: %+v %v", pending, err)
	}
}

func TestFinishValidation(t *testing.T) {
	t.Run("vocabulary and audit", func(t *testing.T) {
		st, _, app, _, _, _, prior, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		for _, bad := range []string{"", "fine"} {
			if _, err := ts.FinishValidation(ctx, updated.ID, bad, ""); !errors.Is(err, ErrInvalid) {
				t.Fatalf("verdict %q: %v", bad, err)
			}
		}
		if _, err := ts.FinishValidation(ctx, prior.ID, VerdictHealthy, ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("finished twice: %v", err)
		}
		var user, result, details, correlation string
		if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT user_id,result,details,correlation_id FROM audit_records WHERE action=? AND resource=?`), AuditValidation, app.ID+"/deployments/"+prior.ID).Scan(&user, &result, &details, &correlation); err != nil || user != "system" || result != "success" || details != VerdictHealthy || correlation != prior.CorrelationID {
			t.Fatalf("audit: %s %s %s %s %v", user, result, details, correlation, err)
		}
	})
	for _, tc := range []struct {
		verdict, detail string
		decide          bool
		reason          string
	}{
		{VerdictHealthy, "", false, ""},
		{VerdictChanged, "web", false, ""},
		{VerdictUnhealthy, "web", true, ""},
		{VerdictExited, "web", true, ""},
		{VerdictRestarting, "web", true, ""},
		{VerdictUnverifiable, ValidationDetailUnobserved, false, ValidationReasonUnverified + ValidationDetailUnobserved},
	} {
		t.Run(tc.verdict, func(t *testing.T) {
			st, a, app, _, p, _, _, updated := validationFixture(t)
			ctx := context.Background()
			ts := st.Tenancy()
			decide, err := ts.FinishValidation(ctx, updated.ID, tc.verdict, tc.detail)
			if err != nil || decide != tc.decide {
				t.Fatalf("decide=%v %v", decide, err)
			}
			got, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID)
			paused := tc.reason != ""
			if err != nil || (got.Status == PolicyPaused) != paused || got.PausedReason != tc.reason {
				t.Fatalf("policy: %+v %v", got, err)
			}
			var rows int
			if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM audit_records WHERE action=? AND resource=? AND user_id='system' AND result='failure' AND correlation_id=?`), AuditPolicyPaused, app.ID+"/policies/"+p.ID, updated.CorrelationID).Scan(&rows); err != nil || (rows == 1) != paused {
				t.Fatalf("pause rows: %d %v", rows, err)
			}
		})
	}
}

func TestMarkRollback(t *testing.T) {
	t.Run("applied", func(t *testing.T) {
		st, a, app, _, _, _, prior, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, uuid.NewString()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("planned before the verdict: %v", err)
		}
		if _, err := ts.FinishValidation(ctx, updated.ID, VerdictExited, "web"); err != nil {
			t.Fatal(err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackApplied, ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("applied with no rollback named: %v", err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, "maybe", ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unknown outcome: %v", err)
		}
		if err := ts.MarkRollbackPlanned(ctx, prior.ID, uuid.NewString()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a manual apply was rolled back: %v", err)
		}
		rollback := uuid.NewString()
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, rollback); err != nil {
			t.Fatal(err)
		}
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, uuid.NewString()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a second rollback: %v", err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackApplied, ""); err != nil {
			t.Fatal(err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackFailed, RollbackNotSent); !errors.Is(err, ErrNotFound) {
			t.Fatalf("decided twice: %v", err)
		}
		got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID)
		if err != nil || got.Validation.Rollback == nil || *got.Validation.Rollback != (ValidationRollback{DeploymentID: rollback, Outcome: RollbackApplied}) {
			t.Fatalf("rollback: %+v %v", got.Validation, err)
		}
		if pol, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || pol.Status != PolicyPaused || pol.PausedReason != ValidationReasonRolledBack {
			t.Fatalf("policy: %+v %v", pol, err)
		}
	})
	t.Run("ineligible", func(t *testing.T) {
		st, a, app, _, _, _, _, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
			t.Fatal(err)
		}
		if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackIneligible, RollbackPriorImagesMissing); err != nil {
			t.Fatal(err)
		}
		got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID)
		if err != nil || got.Validation.Rollback == nil || *got.Validation.Rollback != (ValidationRollback{Outcome: RollbackIneligible, Detail: RollbackPriorImagesMissing}) {
			t.Fatalf("rollback: %+v %v", got.Validation, err)
		}
		if pol, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || pol.PausedReason != ValidationReasonNotRolledBack+RollbackPriorImagesMissing {
			t.Fatalf("policy: %+v %v", pol, err)
		}
	})
}

func TestReconcileAfterStartSettlesValidations(t *testing.T) {
	t.Run("a window the server missed", func(t *testing.T) {
		st, a, app, _, _, _, _, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if _, err := ts.ReconcileAfterStart(ctx); err != nil {
			t.Fatal(err)
		}
		if got, _ := ts.ReadDeployment(ctx, a, app.ID, updated.ID); got.Validation.Phase != PhaseGrace {
			t.Fatalf("an open window was settled: %+v", got.Validation)
		}
		past := time.Now().UTC().Add(-time.Hour)
		if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE deployment_validations SET started_at=?,observe_until=? WHERE deployment_id=?`), past, past.Add(ValidationGrace+ValidationWindow), updated.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.ReconcileAfterStart(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID)
		if err != nil || got.Validation.Verdict != VerdictUnverifiable || got.Validation.Detail != ValidationDetailServerDown || got.Validation.Phase != PhaseDone {
			t.Fatalf("missed window: %+v %v", got.Validation, err)
		}
		if pol, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || pol.PausedReason != ValidationReasonUnverified+ValidationDetailServerDown {
			t.Fatalf("policy: %+v %v", pol, err)
		}
	})
	t.Run("an interrupted rollback", func(t *testing.T) {
		st, a, app, _, _, _, _, updated := validationFixture(t)
		ctx := context.Background()
		ts := st.Tenancy()
		if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
			t.Fatal(err)
		}
		rollback := uuid.NewString()
		if err := ts.MarkRollbackPlanned(ctx, updated.ID, rollback); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.ReconcileAfterStart(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := ts.ReadDeployment(ctx, a, app.ID, updated.ID)
		if err != nil || got.Validation.Rollback == nil || *got.Validation.Rollback != (ValidationRollback{DeploymentID: rollback, Outcome: RollbackFailed, Detail: RollbackInterrupted}) {
			t.Fatalf("interrupted: %+v %v", got.Validation, err)
		}
		if pol, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || pol.PausedReason != ValidationReasonNotRolledBack+RollbackInterrupted {
			t.Fatalf("policy: %+v %v", pol, err)
		}
		if pending, err := ts.PendingValidations(ctx); err != nil || len(pending) != 0 {
			t.Fatalf("dispatched again: %+v %v", pending, err)
		}
	})
}

// A finished run takes no deployment: the name is written only while the run is open.
func TestAttachPolicyRunDeployment(t *testing.T) {
	st, _, _, _, _, run, _, updated := validationFixture(t)
	if err := st.Tenancy().AttachPolicyRunDeployment(context.Background(), run, updated.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attached to a finished run: %v", err)
	}
}
```

In `internal/store/tenancy_test.go`, `TestTenancyUpgradeAndReopen`: prepend `"DROP TABLE deployment_validations", ` to the drop list (before `"DROP TABLE policy_runs"`) and change `...,29,31)` at the end of the `DELETE FROM schema_migrations` entry to `...,29,31,32)`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/store/ -run 'Validation|Judge|BaselineOf|Presence|Settle|BeginObservation|MarkRollback|ReconcileAfterStart|AttachPolicyRun'`
Expected: FAIL to compile with `undefined: VerdictHealthy`, `undefined: Judge` and `ts.AttachPolicyRunDeployment undefined`.

- [ ] **Step 3: Add migration 32**

In `internal/store/migrations/migrations.go`, append after the version 31 entry (inside `registry`):

```go
	// Health validation of every succeeded apply. A validation goes with its deployment; its policy
	// run may go first (the policy deleted), which leaves it automated with no run to act for.
	{Version: 32, Name: "deployment_validations", SQLite: `CREATE TABLE deployment_validations (
 deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 application_id TEXT NOT NULL,
 instance_id TEXT NOT NULL,
 endpoint_id TEXT NOT NULL,
 policy_run_id TEXT REFERENCES policy_runs(id) ON DELETE SET NULL,
 automated INTEGER NOT NULL CHECK(automated IN (0,1)),
 is_rollback INTEGER NOT NULL DEFAULT 0 CHECK(is_rollback IN (0,1)),
 phase TEXT NOT NULL CHECK(phase IN ('grace','observing','done')),
 started_at DATETIME NOT NULL,
 observe_until DATETIME NOT NULL,
 verdict TEXT NOT NULL DEFAULT '' CHECK(verdict IN ('','healthy','unhealthy','exited','restarting','unverifiable','changed')),
 detail TEXT NOT NULL DEFAULT '' CHECK(length(detail)<=255),
 baseline TEXT NOT NULL DEFAULT '' CHECK(length(baseline)<=32768),
 rollback_deployment_id TEXT,
 rollback_outcome TEXT NOT NULL DEFAULT '' CHECK(rollback_outcome IN ('','applied','ineligible','failed')),
 rollback_detail TEXT NOT NULL DEFAULT '' CHECK(length(rollback_detail)<=255),
 correlation_id TEXT NOT NULL CHECK(length(correlation_id) BETWEEN 1 AND 64),
 finished_at DATETIME,
 CHECK(policy_run_id IS NULL OR automated=1)
);
CREATE INDEX idx_deployment_validations_phase ON deployment_validations(phase,started_at);
CREATE INDEX idx_deployment_validations_instance ON deployment_validations(instance_id);
CREATE UNIQUE INDEX idx_deployment_validations_rollback ON deployment_validations(rollback_deployment_id) WHERE rollback_deployment_id IS NOT NULL;
`, Postgres: `CREATE TABLE deployment_validations (
 deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 application_id TEXT NOT NULL,
 instance_id TEXT NOT NULL,
 endpoint_id TEXT NOT NULL,
 policy_run_id TEXT REFERENCES policy_runs(id) ON DELETE SET NULL,
 automated INTEGER NOT NULL CHECK(automated IN (0,1)),
 is_rollback INTEGER NOT NULL DEFAULT 0 CHECK(is_rollback IN (0,1)),
 phase TEXT NOT NULL CHECK(phase IN ('grace','observing','done')),
 started_at TIMESTAMPTZ NOT NULL,
 observe_until TIMESTAMPTZ NOT NULL,
 verdict TEXT NOT NULL DEFAULT '' CHECK(verdict IN ('','healthy','unhealthy','exited','restarting','unverifiable','changed')),
 detail TEXT NOT NULL DEFAULT '' CHECK(length(detail)<=255),
 baseline TEXT NOT NULL DEFAULT '' CHECK(length(baseline)<=32768),
 rollback_deployment_id TEXT,
 rollback_outcome TEXT NOT NULL DEFAULT '' CHECK(rollback_outcome IN ('','applied','ineligible','failed')),
 rollback_detail TEXT NOT NULL DEFAULT '' CHECK(length(rollback_detail)<=255),
 correlation_id TEXT NOT NULL CHECK(length(correlation_id) BETWEEN 1 AND 64),
 finished_at TIMESTAMPTZ,
 CHECK(policy_run_id IS NULL OR automated=1)
);
CREATE INDEX idx_deployment_validations_phase ON deployment_validations(phase,started_at);
CREATE INDEX idx_deployment_validations_instance ON deployment_validations(instance_id);
CREATE UNIQUE INDEX idx_deployment_validations_rollback ON deployment_validations(rollback_deployment_id) WHERE rollback_deployment_id IS NOT NULL;
`},
```

- [ ] **Step 4: Implement the validation store**

Create `internal/store/validations.go`:

```go
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

// Health validation: every succeeded apply is watched for a bounded time and given a verdict; an
// automated one that fails is rolled back when it can be (rollback.go). The loop is
// api.Server.RunValidations. See docs/application-schema.md, Health validation.
const (
	ValidationGrace  = 30 * time.Second
	ValidationWindow = 2 * time.Minute
	ValidationPoll   = 20 * time.Second
	// MaxPendingValidations bounds one tick's work; the rest wait for the next tick.
	MaxPendingValidations = 256
	// AuditValidation is the loop's audit action, written as "system".
	AuditValidation = "application.validation"
	// maxBaselineBytes matches the baseline column's CHECK.
	maxBaselineBytes = 32768
)

const (
	PhaseGrace     = "grace"
	PhaseObserving = "observing"
	PhaseDone      = "done"
)

const (
	VerdictHealthy      = "healthy"
	VerdictUnhealthy    = "unhealthy"
	VerdictExited       = "exited"
	VerdictRestarting   = "restarting"
	VerdictUnverifiable = "unverifiable"
	VerdictChanged      = "changed"
)

// A rollback's outcome, and the codes its detail holds besides plan blockers and the scheduler's
// codes (policyErrorCodes in internal/api).
const (
	RollbackApplied    = "applied"
	RollbackIneligible = "ineligible"
	RollbackFailed     = "failed"

	RollbackNoPriorRevision        = "no_prior_revision"
	RollbackPriorDefinitionInvalid = "prior_definition_invalid"
	RollbackServiceSetChanged      = "service_set_changed"
	RollbackPriorImagesMissing     = "prior_images_missing"
	RollbackInFlight               = "rollback_in_flight"
	RollbackAlreadyRolledBack      = "already_rolled_back"
	RollbackCreatorLost            = "creator_lost"
	RollbackNotSent                = "not_sent"
	RollbackInterrupted            = "interrupted"
)

// The loop's own sentences: the only prose a validation detail or its pause reason holds. A
// failing verdict's detail is the deciding service's name.
const (
	ValidationDetailUnobserved = "the host could not be observed"
	ValidationDetailServerDown = "the server was not running during the window"
	ValidationDetailNoHealth   = "the agent cannot report container health"
	ValidationDetailInvalid    = "an inspection failed validation"
	ValidationDetailReleased   = "the application was released"

	ValidationReasonRolledBack    = "rolled back after a failed update"
	ValidationReasonNotRolledBack = "update failed and could not be rolled back: "
	ValidationReasonUnverified    = "update could not be validated: "
)

// Where a settled container stands against the instance's resources and the latest inventory.
const (
	PresencePresent  = "present"
	PresenceGone     = "gone"
	PresenceReplaced = "replaced"
	PresenceUnknown  = "unknown" // no inventory received since the settle: inspect and wait
)

// validationResults maps a verdict to its audit result; its keys are the closed vocabulary.
var validationResults = map[string]string{VerdictHealthy: "success", VerdictUnhealthy: "failure", VerdictExited: "failure", VerdictRestarting: "failure", VerdictUnverifiable: "failure", VerdictChanged: "failure"}

// rollbackVerdicts are the failures an automated update is rolled back for.
var rollbackVerdicts = map[string]bool{VerdictUnhealthy: true, VerdictExited: true, VerdictRestarting: true}

// awaitingRollback selects, as v, a finished automated validation whose rollback decision is owed.
const awaitingRollback = `(v.phase='done' AND v.automated=1 AND v.is_rollback=0 AND v.policy_run_id IS NOT NULL AND v.verdict IN ('unhealthy','exited','restarting') AND v.rollback_outcome='' AND v.rollback_deployment_id IS NULL)`

// validationColumns reads a validation aliased v and its rollback deployment aliased rd. Every
// column is NULL-safe, so a LEFT JOIN that found none scans as none.
const validationColumns = `COALESCE(v.deployment_id,''),COALESCE(v.policy_run_id,''),COALESCE(v.automated,0),COALESCE(v.is_rollback,0),COALESCE(v.phase,''),v.started_at,v.observe_until,COALESCE(v.verdict,''),COALESCE(v.detail,''),COALESCE(v.rollback_deployment_id,''),COALESCE(v.rollback_outcome,''),COALESCE(v.rollback_detail,''),COALESCE(rd.revision,0),COALESCE(v.correlation_id,''),v.finished_at`

type ValidationRollback struct {
	DeploymentID string `json:"deployment_id"`
	// Revision is the rollback deployment's; 0 once that row is gone.
	Revision int    `json:"revision"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail"`
}

// Validation is a deployment's health validation as the API shows it: the row without its
// baseline and tenant columns. Rollback is nil until a rollback is named or decided.
type Validation struct {
	DeploymentID  string              `json:"deployment_id"`
	PolicyRunID   string              `json:"policy_run_id"`
	Automated     bool                `json:"automated"`
	IsRollback    bool                `json:"is_rollback"`
	Phase         string              `json:"phase"`
	StartedAt     time.Time           `json:"started_at"`
	ObserveUntil  time.Time           `json:"observe_until"`
	Verdict       string              `json:"verdict"`
	Detail        string              `json:"detail"`
	Rollback      *ValidationRollback `json:"rollback"`
	CorrelationID string              `json:"correlation_id"`
	FinishedAt    *time.Time          `json:"finished_at"`
}

// ServiceBaseline is one service at the first observation after grace.
type ServiceBaseline struct {
	ContainerID  string `json:"container_id"`
	RestartCount int    `json:"restart_count"`
}

// ObservedService is a settled identity and where it stands now.
type ObservedService struct {
	protocol.DeploymentIdentity
	Presence string
}

// PendingValidation is a validation the loop still has work on, with what it needs to do it.
type PendingValidation struct {
	Validation
	OrganizationID, EnvironmentID, ApplicationID, InstanceID, EndpointID string
	Baseline                                                             map[string]ServiceBaseline
	Services                                                             []ObservedService
	// Health: the endpoint's agent advertises container.inspect.health.
	Health bool
	// Released: the instance is gone.
	Released bool
	// PolicyID and CreatedBy name the automated run's policy; empty once it is deleted.
	PolicyID, CreatedBy string
}

// Observation is one service at one poll: its presence and, when one succeeded, its inspection.
type Observation struct {
	Service    string
	Presence   string
	Inspection *protocol.ContainerInspection
}

// unobserved marks a service one poll could not see.
const unobserved = "unobserved"

// verdictRank orders the failing verdicts: a poll with several reports the strongest.
var verdictRank = map[string]int{VerdictUnhealthy: 1, VerdictRestarting: 2, VerdictExited: 3, VerdictChanged: 4}

// Judge applies the verdict rules to one poll against baseline. It returns the terminal verdict and
// the service that decided it, or "" while the window goes on. final: observe_until has passed, so
// a service still starting is unhealthy and a complete, passing poll is healthy.
func Judge(obs []Observation, baseline map[string]ServiceBaseline, final bool) (verdict, detail string) {
	complete := true
	for _, o := range obs {
		v := judgeService(o, baseline[o.Service], final)
		if v == unobserved {
			complete = false
			continue
		}
		if verdictRank[v] > verdictRank[verdict] {
			verdict, detail = v, o.Service
		}
	}
	if verdict == "" && final && complete {
		return VerdictHealthy, ""
	}
	return verdict, detail
}

func judgeService(o Observation, b ServiceBaseline, final bool) string {
	switch o.Presence {
	case PresenceReplaced:
		return VerdictChanged
	case PresenceGone:
		return VerdictExited
	}
	in := o.Inspection
	switch {
	case in == nil:
		return unobserved
	case in.State != "running" && in.State != "restarting":
		return VerdictExited
	case in.State == "restarting" || in.RestartCount > b.RestartCount:
		return VerdictRestarting
	case in.Health == "unhealthy", in.Health == "starting" && final:
		return VerdictUnhealthy
	}
	return ""
}

// BaselineOf is the baseline a first observation gives, false unless every service was either
// inspected or is known gone or replaced.
func BaselineOf(obs []Observation) (map[string]ServiceBaseline, bool) {
	out := map[string]ServiceBaseline{}
	for _, o := range obs {
		switch {
		case o.Inspection != nil:
			out[o.Service] = ServiceBaseline{ContainerID: o.Inspection.Target.ContainerID, RestartCount: o.Inspection.RestartCount}
		case o.Presence != PresenceGone && o.Presence != PresenceReplaced:
			return nil, false
		}
	}
	return out, true
}

type boundResource struct{ containerID, name string }

// presence places a settled identity: rebound or unbound means a later deployment or a release
// took the service; otherwise the inventory decides, when one arrived since the settle (snap
// non-nil). A container holding the resource's name under another ID was recreated.
func presence(id protocol.DeploymentIdentity, bound map[string]boundResource, snap *protocol.Snapshot) string {
	r, ok := bound[id.Service]
	if !ok || r.containerID != id.ContainerID {
		return PresenceReplaced
	}
	if snap == nil {
		return PresenceUnknown
	}
	if slices.ContainsFunc(snap.Containers, func(c protocol.Container) bool { return c.ID == id.ContainerID }) {
		return PresencePresent
	}
	if slices.ContainsFunc(snap.Containers, func(c protocol.Container) bool { return c.Name == r.name }) {
		return PresenceReplaced
	}
	return PresenceGone
}

// validationScan receives validationColumns.
type validationScan struct {
	id, run, phase, verdict, detail, rbID, rbOutcome, rbDetail, correlation string
	automated, isRollback, rbRevision                                       int
	started, until, finished                                                sql.NullTime
}

func (s *validationScan) dest() []any {
	return []any{&s.id, &s.run, &s.automated, &s.isRollback, &s.phase, &s.started, &s.until, &s.verdict, &s.detail, &s.rbID, &s.rbOutcome, &s.rbDetail, &s.rbRevision, &s.correlation, &s.finished}
}

// validation is the scanned row, nil when the LEFT JOIN found none.
func (s *validationScan) validation() *Validation {
	if s.id == "" {
		return nil
	}
	v := &Validation{DeploymentID: s.id, PolicyRunID: s.run, Automated: s.automated == 1, IsRollback: s.isRollback == 1, Phase: s.phase, StartedAt: s.started.Time.UTC(), ObserveUntil: s.until.Time.UTC(), Verdict: s.verdict, Detail: s.detail, CorrelationID: s.correlation}
	if s.finished.Valid {
		f := s.finished.Time.UTC()
		v.FinishedAt = &f
	}
	if s.rbID != "" || s.rbOutcome != "" {
		v.Rollback = &ValidationRollback{DeploymentID: s.rbID, Revision: s.rbRevision, Outcome: s.rbOutcome, Detail: s.rbDetail}
	}
	return v
}

// insertValidation opens the validation of a succeeded apply inside the settling transaction. It is
// automated when an open or applied policy run names the deployment (a plan_only run's plan applied
// by hand is manual), and a rollback when a validation named it as its rollback.
func (t *tenancyStore) insertValidation(ctx context.Context, tx *sql.Tx, org, env, app, instance, endpoint, deployment, correlation string, settled time.Time) error {
	var run any
	var runID string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id FROM policy_runs WHERE deployment_id=? AND outcome IN ('','applied') ORDER BY started_at DESC,id LIMIT 1`), deployment).Scan(&runID)
	switch {
	case err == nil:
		run = runID
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var dispatched int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM deployment_validations WHERE rollback_deployment_id=?`), deployment).Scan(&dispatched); err != nil {
		return err
	}
	automated, rollback := 0, 0
	if run != nil {
		automated = 1
	}
	if dispatched > 0 {
		rollback = 1
	}
	if correlation == "" {
		correlation = uuid.NewString()
	}
	_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployment_validations(deployment_id,organization_id,environment_id,application_id,instance_id,endpoint_id,policy_run_id,automated,is_rollback,phase,started_at,observe_until,correlation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`), deployment, org, env, app, instance, endpoint, run, automated, rollback, PhaseGrace, settled, settled.Add(ValidationGrace+ValidationWindow), correlation)
	return err
}

// rowQuerier is *sql.DB or *sql.Tx.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// boundServices is what the instance's resources bind each service to now.
func (t *tenancyStore) boundServices(ctx context.Context, q rowQuerier, instance string) (map[string]boundResource, error) {
	rows, err := q.QueryContext(ctx, t.store.rebind(`SELECT service_name,container_id,name FROM application_resources WHERE instance_id=? AND service_name<>''`), instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]boundResource{}
	for rows.Next() {
		var service string
		var r boundResource
		if err := rows.Scan(&service, &r.containerID, &r.name); err != nil {
			return nil, err
		}
		out[service] = r
	}
	return out, rows.Err()
}

type inventoryView struct {
	snapshot protocol.Snapshot
	received time.Time
}

// latestInventory is the endpoint's stored snapshot; nil when there is none or its container list
// is truncated, since a container missing from it would prove nothing.
func (t *tenancyStore) latestInventory(ctx context.Context, endpoint string) (*inventoryView, error) {
	var raw string
	var v inventoryView
	err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT snapshot,received_at FROM endpoint_inventory WHERE endpoint_id=?`), endpoint).Scan(&raw, &v.received)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal([]byte(raw), &v.snapshot) != nil || len(v.snapshot.Containers) > protocol.MaxContainers || slices.Contains(v.snapshot.Truncated, "containers") {
		return nil, nil
	}
	return &v, nil
}

// PendingValidations lists, oldest first, every validation not done and every automated one whose
// rollback decision is owed, each with its settled services placed against the instance's
// resources and the endpoint's latest inventory.
func (t *tenancyStore) PendingValidations(ctx context.Context) ([]PendingValidation, error) {
	rows, err := t.store.db.QueryContext(ctx, t.store.rebind(`SELECT `+validationColumns+`,v.organization_id,v.environment_id,v.application_id,v.instance_id,v.endpoint_id,v.baseline,d.result,COALESCE(p.id,''),COALESCE(p.created_by,''),(SELECT COUNT(*) FROM application_instances i WHERE i.id=v.instance_id),(SELECT COUNT(*) FROM endpoint_capabilities c WHERE c.endpoint_id=v.endpoint_id AND c.capability=?) FROM deployment_validations v JOIN deployments d ON d.id=v.deployment_id LEFT JOIN deployments rd ON rd.id=v.rollback_deployment_id LEFT JOIN policy_runs r ON r.id=v.policy_run_id LEFT JOIN update_policies p ON p.id=r.policy_id WHERE v.phase<>'done' OR `+awaitingRollback+` ORDER BY v.started_at,v.deployment_id LIMIT ?`), protocol.CapabilityContainerInspectHealth, MaxPendingValidations)
	if err != nil {
		return nil, err
	}
	out := []PendingValidation{}
	for rows.Next() {
		var s validationScan
		var p PendingValidation
		var baseline, result string
		var instances, health int
		if err := rows.Scan(append(s.dest(), &p.OrganizationID, &p.EnvironmentID, &p.ApplicationID, &p.InstanceID, &p.EndpointID, &baseline, &result, &p.PolicyID, &p.CreatedBy, &instances, &health)...); err != nil {
			rows.Close()
			return nil, err
		}
		p.Validation = *s.validation()
		p.Released, p.Health = instances == 0, health > 0
		var stored storedDeploymentResult
		if json.Unmarshal([]byte(result), &stored) != nil || (baseline != "" && json.Unmarshal([]byte(baseline), &p.Baseline) != nil) {
			rows.Close()
			return nil, ErrRevisionCorrupt
		}
		for _, id := range stored.Services {
			p.Services = append(p.Services, ObservedService{DeploymentIdentity: id})
		}
		out = append(out, p)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	inventories := map[string]*inventoryView{}
	for i := range out {
		p := &out[i]
		bound, err := t.boundServices(ctx, t.store.db, p.InstanceID)
		if err != nil {
			return nil, err
		}
		inv, seen := inventories[p.EndpointID]
		if !seen {
			if inv, err = t.latestInventory(ctx, p.EndpointID); err != nil {
				return nil, err
			}
			inventories[p.EndpointID] = inv
		}
		var snap *protocol.Snapshot
		if inv != nil && !inv.received.Before(p.StartedAt) {
			snap = &inv.snapshot
		}
		for j := range p.Services {
			p.Services[j].Presence = presence(p.Services[j].DeploymentIdentity, bound, snap)
		}
	}
	return out, nil
}

// BeginObservation stores the baseline taken at the first observation after grace and moves the
// validation to observing. A row no longer in grace is ErrNotFound.
func (t *tenancyStore) BeginObservation(ctx context.Context, deployment string, baseline map[string]ServiceBaseline) error {
	raw, err := json.Marshal(baseline)
	if err != nil || len(raw) > maxBaselineBytes {
		return ErrInvalid
	}
	res, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE deployment_validations SET phase=?,baseline=? WHERE deployment_id=? AND phase=?`), PhaseObserving, string(raw), deployment, PhaseGrace)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// FinishValidation records a verdict and its audit row. For an automated update that is not itself
// a rollback, unverifiable pauses the policy, and a failing verdict reports that a rollback
// decision is due (true). A validation already done is ErrNotFound.
func (t *tenancyStore) FinishValidation(ctx context.Context, deployment, verdict, detail string) (bool, error) {
	if _, ok := validationResults[verdict]; !ok {
		return false, ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	decide, err := t.finishValidation(ctx, tx, deployment, verdict, detail, time.Now().UTC())
	if err != nil {
		return false, err
	}
	return decide, tx.Commit()
}

func (t *tenancyStore) finishValidation(ctx context.Context, tx *sql.Tx, deployment, verdict, detail string, now time.Time) (bool, error) {
	var org, env, app, correlation string
	var run sql.NullString
	var automated, rollback int
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT organization_id,environment_id,application_id,correlation_id,policy_run_id,automated,is_rollback FROM deployment_validations WHERE deployment_id=? AND phase<>'done'`), deployment).Scan(&org, &env, &app, &correlation, &run, &automated, &rollback)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	detail = protocol.CleanText(detail, 255)
	if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployment_validations SET phase=?,verdict=?,detail=?,finished_at=? WHERE deployment_id=?`), PhaseDone, verdict, detail, now, deployment); err != nil {
		return false, err
	}
	if err := t.auditPolicy(ctx, tx, org, env, app+"/deployments/"+deployment, AuditValidation, verdict, validationResults[verdict], correlation, now); err != nil {
		return false, err
	}
	if automated == 0 || rollback == 1 || !run.Valid {
		return false, nil
	}
	if verdict == VerdictUnverifiable {
		return false, t.pauseForValidation(ctx, tx, run.String, ValidationReasonUnverified+detail, correlation, now)
	}
	return rollbackVerdicts[verdict], nil
}

// MarkRollbackPlanned names the rollback deployment on a validation awaiting its decision, before
// that deployment is applied: its settle then knows it is a rollback, and nothing dispatches
// another. A validation not awaiting a decision is ErrNotFound.
func (t *tenancyStore) MarkRollbackPlanned(ctx context.Context, deployment, rollback string) error {
	res, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE deployment_validations SET rollback_deployment_id=? WHERE deployment_id IN (SELECT v.deployment_id FROM deployment_validations v WHERE v.deployment_id=? AND `+awaitingRollback+`)`), rollback, deployment)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// MarkRollbackOutcome records the rollback decision on an automated validation and pauses its
// policy with the matching reason. applied needs the rollback already named.
func (t *tenancyStore) MarkRollbackOutcome(ctx context.Context, deployment, outcome, detail string) error {
	switch outcome {
	case RollbackApplied, RollbackIneligible, RollbackFailed:
	default:
		return ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := t.markRollbackOutcome(ctx, tx, deployment, outcome, detail, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (t *tenancyStore) markRollbackOutcome(ctx context.Context, tx *sql.Tx, deployment, outcome, detail string, now time.Time) error {
	var run sql.NullString
	var correlation, rollback string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT policy_run_id,correlation_id,COALESCE(rollback_deployment_id,'') FROM deployment_validations WHERE deployment_id=? AND phase='done' AND automated=1 AND is_rollback=0 AND rollback_outcome=''`), deployment).Scan(&run, &correlation, &rollback)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if outcome == RollbackApplied && rollback == "" {
		return ErrInvalid
	}
	detail = protocol.CleanText(detail, 255)
	if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployment_validations SET rollback_outcome=?,rollback_detail=? WHERE deployment_id=?`), outcome, detail, deployment); err != nil {
		return err
	}
	if !run.Valid {
		return nil
	}
	reason := ValidationReasonRolledBack
	if outcome != RollbackApplied {
		reason = ValidationReasonNotRolledBack + detail
	}
	return t.pauseForValidation(ctx, tx, run.String, reason, correlation, now)
}

// pauseForValidation pauses the policy whose run made the deployment, with its audit row under the
// deployment's correlation ID. A deleted or already paused policy is left as it is.
func (t *tenancyStore) pauseForValidation(ctx context.Context, tx *sql.Tx, run, reason, correlation string, now time.Time) error {
	var policy, org, env, app string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT p.id,p.organization_id,p.environment_id,p.application_id FROM policy_runs r JOIN update_policies p ON p.id=r.policy_id WHERE r.id=?`), run).Scan(&policy, &org, &env, &app)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	reason = protocol.CleanText(reason, 255)
	res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET status=?,paused_reason=?,updated_at=? WHERE id=? AND status=?`), PolicyPaused, reason, now, policy, PolicyActive)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	return t.auditPolicy(ctx, tx, org, env, app+"/policies/"+policy, AuditPolicyPaused, reason, "failure", correlation, now)
}

// reconcileValidations settles what the previous process left: a validation still in grace whose
// window has ended never had its baseline taken, and a rollback named but never decided was
// interrupted. Neither is dispatched again.
func (t *tenancyStore) reconcileValidations(ctx context.Context, tx *sql.Tx) error {
	now := time.Now().UTC()
	missed, err := t.validationIDs(ctx, tx, `SELECT deployment_id FROM deployment_validations WHERE phase='grace' AND observe_until<? ORDER BY started_at,deployment_id`, now)
	if err != nil {
		return err
	}
	for _, id := range missed {
		if _, err := t.finishValidation(ctx, tx, id, VerdictUnverifiable, ValidationDetailServerDown, now); err != nil {
			return err
		}
	}
	interrupted, err := t.validationIDs(ctx, tx, `SELECT deployment_id FROM deployment_validations WHERE rollback_deployment_id IS NOT NULL AND rollback_outcome='' ORDER BY started_at,deployment_id`)
	if err != nil {
		return err
	}
	for _, id := range interrupted {
		if err := t.markRollbackOutcome(ctx, tx, id, RollbackFailed, RollbackInterrupted, now); err != nil {
			return err
		}
	}
	return nil
}

func (t *tenancyStore) validationIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, t.store.rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: Wire settle, deployments, runs and reconciliation**

In `internal/store/application_apply.go`, `SettleDeployment`, replace

```go
	if kind == "apply" && res.Outcome == protocol.OutcomeSucceeded {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_instances SET previous_revision=current_revision,current_revision=? WHERE id=?`), revision, instance); err != nil {
			return err
		}
	}
```

with

```go
	if kind == "apply" && res.Outcome == protocol.OutcomeSucceeded {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_instances SET previous_revision=current_revision,current_revision=? WHERE id=?`), revision, instance); err != nil {
			return err
		}
		// Every succeeded apply is validated, opened in the transaction that settles it.
		if err := t.insertValidation(ctx, tx, org, env, appID, instance, endpointID, res.Deployment, correlation, now); err != nil {
			return err
		}
	}
```

In `internal/store/application_deployment.go`, add to `Deployment` after `Result`:

```go
	// Validation is the deployment's health validation: nil for a plan, a removal or an apply
	// that did not succeed.
	Validation *Validation `json:"validation,omitempty"`
```

Replace `selectDeployments` with

```go
// selectDeployments reads rows aliased d with the endpoint's current name (empty once the
// endpoint is gone) and the deployment's validation; the caller appends the WHERE clause.
const selectDeployments = `SELECT d.id,d.application_id,d.instance_id,d.endpoint_id,COALESCE(e.name,''),d.kind,d.state,d.revision,d.spec_digest,d.mapping_version,d.plan,d.created_by,d.created_at,d.expires_at,d.applied_by,d.applied_at,d.deadline,d.settled_at,d.detail,d.result,d.correlation_id,` + validationColumns + ` FROM deployments d LEFT JOIN endpoints e ON e.id=d.endpoint_id LEFT JOIN deployment_validations v ON v.deployment_id=d.id LEFT JOIN deployments rd ON rd.id=v.rollback_deployment_id `
```

and in `scanDeployment` replace the `rows.Scan(...)` call with

```go
	var vs validationScan
	if err := rows.Scan(append([]any{&d.ID, &d.ApplicationID, &d.InstanceID, &d.EndpointID, &d.EndpointName, &d.Kind, &d.State, &d.Revision, &d.SpecDigest, &d.MappingVersion, &raw, &d.CreatedBy, &d.CreatedAt, &d.ExpiresAt, &d.AppliedBy, &appliedAt, &deadline, &settledAt, &d.Detail, &result, &d.CorrelationID}, vs.dest()...)...); err != nil {
		return nil, err
	}
	d.Validation = vs.validation()
```

In `internal/store/policy_schedule.go`, append:

```go
// AttachPolicyRunDeployment names the deployment an open run is about to send, before the frame
// leaves: a settle that beats FinishPolicyRun still finds the run and validates the deployment as
// automated. A finished or missing run is ErrNotFound.
func (t *tenancyStore) AttachPolicyRunDeployment(ctx context.Context, run, deployment string) error {
	res, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE policy_runs SET deployment_id=? WHERE id=? AND outcome=''`), deployment, run)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}
```

In `internal/store/reconcile.go`, `ReconcileAfterStart`, replace

```go
	if err := t.reconcilePolicyRuns(ctx, tx); err != nil {
		return 0, err
	}
```

with

```go
	if err := t.reconcilePolicyRuns(ctx, tx); err != nil {
		return 0, err
	}
	if err := t.reconcileValidations(ctx, tx); err != nil {
		return 0, err
	}
```

and append to its doc comment: "Validations still in grace past their window become `unverifiable` (`the server was not running during the window`), and a rollback named but undecided becomes `failed` `interrupted`, pausing its policy, in the same transaction."

In `internal/store/store.go`, `TenancyStore`, after the `SkipPolicyWindow` line add:

```go
	// AttachPolicyRunDeployment names the deployment an open run is about to send.
	AttachPolicyRunDeployment(ctx context.Context, runID, deploymentID string) error
	// Health validation (docs/application-schema.md, Health validation): the loop's side, trusted
	// and not tenant-scoped.
	PendingValidations(ctx context.Context) ([]PendingValidation, error)
	BeginObservation(ctx context.Context, deploymentID string, baseline map[string]ServiceBaseline) error
	// FinishValidation reports true when a rollback decision is due.
	FinishValidation(ctx context.Context, deploymentID, verdict, detail string) (bool, error)
	MarkRollbackPlanned(ctx context.Context, deploymentID, rollbackDeploymentID string) error
	MarkRollbackOutcome(ctx context.Context, deploymentID, outcome, detail string) error
```

and update the `ReconcileAfterStart` comment line there to "ReconcileAfterStart settles every in-flight command as unknown, fails every open policy run and settles validations the previous process left".

- [ ] **Step 6: Run the tests on both drivers**

Run: `gofmt -w internal/store && go vet ./internal/store/ && go test -race -count=1 ./internal/store/... && PG=… go test -count=1 ./internal/store/...`
Expected: PASS on SQLite and PostgreSQL, including `TestTenancyUpgradeAndReopen` (the new table dropped and replayed), `TestLatestIsTheLastRegisteredVersion` (now 32) and every existing settle, apply and policy test.

- [ ] **Step 7: DOX and commit**

`internal/store/AGENTS.md`, Local Contracts, after the policy-schedule bullet add: "- Health validation (`validations.go`, migration 32): `SettleDeployment` opens a `deployment_validations` row for every succeeded apply in the settling transaction (`grace`, `observe_until` = settle + `ValidationGrace` + `ValidationWindow`), automated when an open or `applied` policy run names the deployment (`AttachPolicyRunDeployment` names it before the frame leaves), a rollback when a validation named it (`MarkRollbackPlanned`). `Judge` (pure) applies the verdict rules to `Observation`s: `changed` > `exited` > `restarting` > `unhealthy`, detail the deciding service; `healthy` only on a complete passing poll at or after `observe_until`. `PendingValidations` places each settled identity (`presence`): rebound or unbound is replaced, an inventory received since the settle decides present/gone/replaced by ID and name, none is unknown. `FinishValidation` audits `application.validation` as `system` and, for an automated non-rollback row with its policy run, pauses on `unverifiable` or reports a rollback decision due; `MarkRollbackOutcome` pauses with `rolled back after a failed update` or `update failed and could not be rolled back: <detail>`. `ReconcileAfterStart` settles grace rows past their window and interrupted rollbacks. `Deployment.Validation` is joined into every deployment read."

```bash
git add internal/store
git commit -m "feat(store): deployment validations, verdicts and their lifecycle (migration 32)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Store — rollback eligibility, pinned images and the prune guard

**Files:**
- Create: `internal/store/rollback.go`
- Modify: `internal/store/application_deployment.go` (`PlanRequest.PinImages`; `draftPlan` passes it)
- Modify: `internal/store/application_preflight.go` (`preflight` and `buildDeploymentPreflight` take pins; `imagesOnHost`)
- Modify: `internal/store/image_checks.go` (the `preflight` call passes `nil`)
- Modify: `internal/store/application_preflight_test.go` (seven `buildDeploymentPreflight` calls gain `nil`)
- Modify: `internal/store/samples.go` (`Prune` skips instances whose automated update may still roll back)
- Modify: `internal/store/store.go` (`TenancyStore.RollbackTarget`)
- Test: `internal/store/rollback_test.go`
- Docs: `internal/store/AGENTS.md`

**Interfaces:**
- Consumes (Task 2): `validationFixture`, `deployFixture`, `putInventory`, `webContainer`, `tagged`, `imageD`, `imageX`, `priorID`, `updatedID`, `boundServices`, `FinishValidation`, `MarkRollbackPlanned`, `MarkRollbackOutcome`, `storedDeploymentResult`, `applicationSpecDigest`, `ValidateApplicationSpec`.
- Produces:
  ```go
  // Rollback is what an automated rollback re-applies.
  type Rollback struct {
  	InstanceID     string
  	MappingVersion int
  	Project        string
  	Revision       int
  	Images         map[string]string // service -> image ID
  }
  // RollbackTarget returns the target, or "" and nil with an ineligibility reason, or an error
  // (ErrForbidden when access no longer holds application.deploy; ErrNotFound for no such
  // validated deployment or a released instance).
  RollbackTarget(ctx context.Context, access TenantAccess, applicationID, deploymentID string) (*Rollback, string, error)
  // PlanRequest gains:
  PinImages map[string]string `json:"-"` // service -> image ID; set only by the rollback
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/store/rollback_test.go`:

```go
package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

func mustExec(t *testing.T, st *SQLStore, query string, args ...any) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(), st.rebind(query), args...); err != nil {
		t.Fatal(err)
	}
}

// updatedContainer is the fixture's updated web container as the inventory reports it.
func updatedContainer(updated *Deployment) protocol.Container {
	return webContainer(updatedID, imageX, time.Unix(updated.Result.Services[0].CreatedUnix, 0).UTC())
}

// A policy update re-applies revision 1, so previous_revision is 1 too: the target is the older
// apply's recorded image, never the deployment under validation (Review Focus 1).
func TestRollbackTargetReturnsThePriorImages(t *testing.T) {
	st, a, app, endpoint, _, _, _, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	rb, reason, err := ts.RollbackTarget(ctx, a, app.ID, updated.ID)
	if err != nil || reason != "" || rb == nil || rb.Revision != 1 || len(rb.Images) != 1 || rb.Images["web"] != imageD || rb.Project != "shop" || rb.InstanceID != updated.InstanceID || rb.MappingVersion != updated.MappingVersion {
		t.Fatalf("target: %+v %q %v", rb, reason, err)
	}
	// A running container's image is on the host even when the image list omits it.
	putInventory(t, st, endpoint, []protocol.Container{updatedContainer(updated), {ID: strings.Repeat("5", 64), Name: "old", ImageID: imageD, State: "running", CreatedAt: time.Now().UTC(), Mounts: []protocol.Mount{}}}, []protocol.Image{tagged(imageX, "nginx:1")})
	if rb, reason, err := ts.RollbackTarget(ctx, a, app.ID, updated.ID); err != nil || reason != "" || rb.Images["web"] != imageD {
		t.Fatalf("image under a running container: %+v %q %v", rb, reason, err)
	}
}

func TestRollbackTargetIneligibility(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, st *SQLStore, app *Application, endpoint string, prior, updated *Deployment)
		want  string
	}{
		{"already rolled back", func(t *testing.T, st *SQLStore, _ *Application, _ string, _, updated *Deployment) {
			if _, err := st.Tenancy().FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
				t.Fatal(err)
			}
			if err := st.Tenancy().MarkRollbackPlanned(ctx, updated.ID, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
		}, RollbackAlreadyRolledBack},
		{"another rollback still validating", func(t *testing.T, st *SQLStore, _ *Application, _ string, prior, _ *Deployment) {
			mustExec(t, st, `UPDATE deployment_validations SET is_rollback=1,phase='observing' WHERE deployment_id=?`, prior.ID)
		}, RollbackInFlight},
		{"no prior apply recorded", func(t *testing.T, st *SQLStore, _ *Application, _ string, prior, _ *Deployment) {
			mustExec(t, st, `DELETE FROM deployments WHERE id=?`, prior.ID)
		}, RollbackNoPriorRevision},
		{"prior definition tampered", func(t *testing.T, st *SQLStore, app *Application, _ string, _, _ *Deployment) {
			mustExec(t, st, `UPDATE application_revisions SET spec=? WHERE application_id=? AND number=1`, `{"kind":"compose.v1","services":[{"name":"web","image":"nginx:tampered"}]}`, app.ID)
		}, RollbackPriorDefinitionInvalid},
		{"services renamed since", func(t *testing.T, st *SQLStore, _ *Application, _ string, _, updated *Deployment) {
			mustExec(t, st, `UPDATE application_resources SET service_name='api' WHERE instance_id=?`, updated.InstanceID)
		}, RollbackServiceSetChanged},
		{"prior image removed", func(t *testing.T, st *SQLStore, _ *Application, endpoint string, _, updated *Deployment) {
			putInventory(t, st, endpoint, []protocol.Container{updatedContainer(updated)}, []protocol.Image{tagged(imageX, "nginx:1")})
		}, RollbackPriorImagesMissing},
		{"prior image only under a stopped container", func(t *testing.T, st *SQLStore, _ *Application, endpoint string, _, updated *Deployment) {
			putInventory(t, st, endpoint, []protocol.Container{updatedContainer(updated), {ID: strings.Repeat("5", 64), Name: "old", ImageID: imageD, State: "exited", CreatedAt: time.Now().UTC(), Mounts: []protocol.Mount{}}}, []protocol.Image{tagged(imageX, "nginx:1")})
		}, RollbackPriorImagesMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, a, app, endpoint, _, _, prior, updated := validationFixture(t)
			tc.setup(t, st, app, endpoint, prior, updated)
			rb, reason, err := st.Tenancy().RollbackTarget(ctx, a, app.ID, updated.ID)
			if err != nil || rb != nil || reason != tc.want {
				t.Fatalf("%+v %q %v, want %q", rb, reason, err, tc.want)
			}
		})
	}
	t.Run("first apply after adoption", func(t *testing.T) {
		st, a, app, endpoint, _, _ := planFixture(t)
		d := deployFixture(t, st, a, app, endpoint, priorID, []protocol.Image{tagged(imageD, "nginx:1")}, nil)
		if rb, reason, err := st.Tenancy().RollbackTarget(ctx, a, app.ID, d.ID); err != nil || rb != nil || reason != RollbackNoPriorRevision {
			t.Fatalf("%+v %q %v", rb, reason, err)
		}
	})
}

// The rollback acts as the policy's creator: application.deploy is checked again.
func TestRollbackTargetReauthorizesTheCreator(t *testing.T) {
	st, a, app, _, _, _, _, updated := validationFixture(t)
	viewer := policyMember(t, st, a, "viewer", RoleReadOnly)
	if _, _, err := st.Tenancy().RollbackTarget(context.Background(), viewer, app.ID, updated.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a read-only member decided a rollback: %v", err)
	}
	if _, _, err := st.Tenancy().RollbackTarget(context.Background(), a, app.ID, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such deployment: %v", err)
	}
}

// PinImages takes the given image instead of resolving the tag: no registry, no pull, and the
// image must be on the host now.
func TestPlanDeploymentPinsImages(t *testing.T) {
	st, a, app, endpoint, _, _, _, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	plan := func(pin string) (*Deployment, error) {
		t.Helper()
		m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
		if err != nil {
			t.Fatal(err)
		}
		r := planRequest(m)
		r.PinImages = map[string]string{"web": pin}
		return ts.PlanDeployment(ctx, a, app.ID, r, nil, imageCheckKey, false)
	}
	// nginx:1 resolves to imageX; the pin wins.
	d, err := plan(imageD)
	if err != nil || d.Plan.Services[0].ImageID != imageD || d.Plan.Services[0].PullDigest != "" || d.Plan.Services[0].Reference != "nginx:1" {
		t.Fatalf("pinned plan: %+v %v", d, err)
	}
	current := updatedContainer(updated)
	for _, tc := range []struct {
		name       string
		containers []protocol.Container
		images     []protocol.Image
		pin, want  string
	}{
		{"image gone", []protocol.Container{current}, []protocol.Image{tagged(imageX, "nginx:1")}, imageD, "image_not_reported"},
		{"only a stopped container has it", []protocol.Container{current, {ID: strings.Repeat("5", 64), Name: "old", ImageID: imageD, State: "exited", CreatedAt: time.Now().UTC(), Mounts: []protocol.Mount{}}}, []protocol.Image{tagged(imageX, "nginx:1")}, imageD, "image_not_reported"},
		{"not an image ID", []protocol.Container{current}, []protocol.Image{tagged(imageD), tagged(imageX, "nginx:1")}, "nginx:1", "image_identity_invalid"},
	} {
		putInventory(t, st, endpoint, tc.containers, tc.images)
		_, err := plan(tc.pin)
		var blocked *PreflightBlockedError
		if !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, tc.want) {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.want)
		}
	}
	putInventory(t, st, endpoint, []protocol.Container{current, {ID: strings.Repeat("5", 64), Name: "old", ImageID: imageD, State: "running", CreatedAt: time.Now().UTC(), Mounts: []protocol.Mount{}}}, []protocol.Image{tagged(imageX, "nginx:1")})
	if d, err := plan(imageD); err != nil || d.Plan.Services[0].ImageID != imageD {
		t.Fatalf("pin under a running container: %+v %v", d, err)
	}
}

// Prune keeps every deployment of an instance whose automated update may still roll back: the
// rollback needs the prior apply's recorded images (plan decision 12).
func TestPruneKeepsTheRollbackTargetDuringAValidation(t *testing.T) {
	st, _, _, _, _, _, prior, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	mustExec(t, st, `UPDATE deployments SET settled_at=? WHERE id=?`, time.Now().UTC().Add(-DeploymentHistoryRetention-time.Hour), prior.ID)
	kept := func() bool {
		t.Helper()
		if _, err := ts.Prune(ctx); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM deployments WHERE id=?`), prior.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if !kept() {
		t.Fatal("pruned the rollback target while its validation was open")
	}
	if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
		t.Fatal(err)
	}
	if !kept() {
		t.Fatal("pruned the rollback target while its decision was owed")
	}
	if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackIneligible, RollbackPriorImagesMissing); err != nil {
		t.Fatal(err)
	}
	if kept() {
		t.Fatal("an old, superseded deployment outlived its validation")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/store/ -run 'RollbackTarget|PlanDeploymentPinsImages|PruneKeepsTheRollbackTarget'`
Expected: FAIL to compile with `ts.RollbackTarget undefined` and `r.PinImages undefined`.

- [ ] **Step 3: Pin images in the preflight**

In `internal/store/application_deployment.go`, add to `PlanRequest` after `Inspections`:

```go
	// PinImages is set only by a validation's rollback: service to image ID, taken instead of
	// resolving the service's tag. The ID must be on the host; nothing is pulled.
	PinImages map[string]string `json:"-"`
```

and in `draftPlan` replace `t.preflight(ctx, tx, a, app, lock, r.Revision)` with `t.preflight(ctx, tx, a, app, lock, r.Revision, r.PinImages)`.

In `internal/store/application_preflight.go`: in `PreflightApplication` replace `t.preflight(ctx, tx, a, id.String(), false, 0)` with `t.preflight(ctx, tx, a, id.String(), false, 0, nil)`; change `preflight`'s signature to

```go
func (t *tenancyStore) preflight(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, lock bool, revision int, pins map[string]string) (*DeploymentPreflight, *ApplicationMapping, ApplicationSpec, protocol.Snapshot, string, error) {
```

adding to its doc comment "pins, from PlanRequest.PinImages, take an image ID for a service instead of its tag."; replace `out := buildDeploymentPreflight(m, spec, snapshot, number == head)` with `out := buildDeploymentPreflight(m, spec, snapshot, number == head, pins)`; change `buildDeploymentPreflight`'s signature to

```go
func buildDeploymentPreflight(m *ApplicationMapping, spec ApplicationSpec, snapshot protocol.Snapshot, latest bool, pins map[string]string) *DeploymentPreflight {
```

insert before `for _, s := range spec.Services {` (the loop that builds rows):

```go
	onHost := imagesOnHost(snapshot)
```

and replace

```go
		_, tag := protocol.SplitImageReference(s.Image)
		switch {
		case tag == "":
```

with

```go
		_, tag := protocol.SplitImageReference(s.Image)
		pin, pinned := pins[s.Name]
		switch {
		case pinned && !validSHA256(pin):
			row.Blockers = append(row.Blockers, "image_identity_invalid")
		case pinned && !onHost[pin]:
			row.Blockers = append(row.Blockers, "image_not_reported")
		case pinned:
			row.ImageID = pin
		case tag == "":
```

Append to the file:

```go
// imagesOnHost is every image ID the inventory shows present: listed images and the images of
// running containers.
func imagesOnHost(snapshot protocol.Snapshot) map[string]bool {
	out := map[string]bool{}
	for _, im := range snapshot.Images {
		out[im.ID] = true
	}
	for _, c := range snapshot.Containers {
		if c.State == "running" {
			out[c.ImageID] = true
		}
	}
	return out
}
```

In `internal/store/image_checks.go` replace `t.preflight(ctx, tx, a, id.String(), false, 0)` with `t.preflight(ctx, tx, a, id.String(), false, 0, nil)`. In `internal/store/application_preflight_test.go` give each of the seven `buildDeploymentPreflight(..., true)` calls a trailing `nil`:

```bash
sed -i 's/buildDeploymentPreflight(\(.*\), true)/buildDeploymentPreflight(\1, true, nil)/' internal/store/application_preflight_test.go
grep -c 'true, nil)' internal/store/application_preflight_test.go
```

Expected: `7`.

- [ ] **Step 4: Implement the eligibility decision**

Create `internal/store/rollback.go`:

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

// mayRollBack selects, as v, an automated validation that may still roll back: its window open, or
// its failing verdict awaiting the decision. Manual and rollback validations never roll back.
const mayRollBack = `(v.automated=1 AND v.is_rollback=0 AND v.policy_run_id IS NOT NULL AND v.rollback_outcome='' AND v.rollback_deployment_id IS NULL AND (v.phase<>'done' OR v.verdict IN ('unhealthy','exited','restarting')))`

// Rollback is what an automated rollback re-applies: the prior revision, each service pinned to
// the image it ran in the prior apply.
type Rollback struct {
	InstanceID     string
	MappingVersion int
	Project        string
	Revision       int
	// Images maps each service to the image ID it ran.
	Images map[string]string
}

// RollbackTarget decides, as a with application.deploy re-checked, whether the failed deployment
// can be rolled back, and to what. An ineligible one returns a reason from the fixed vocabulary.
// The prior apply is the newest succeeded one of the instance at its previous_revision that is not
// this deployment and settled no later: an update a policy applies keeps its revision, so
// previous_revision is usually the current one. See docs/application-schema.md, Rollback.
func (t *tenancyStore) RollbackTarget(ctx context.Context, a TenantAccess, app, deployment string) (*Rollback, string, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, "", ErrInvalid
	}
	depID, err := uuid.Parse(deployment)
	if err != nil {
		return nil, "", ErrNotFound
	}
	var out *Rollback
	reason := ""
	err = t.readTenant(ctx, a, permissions.ApplicationDeploy, func(tx *sql.Tx) error {
		out, reason = nil, ""
		var instance, endpoint, rolledBack string
		var settled time.Time
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT d.instance_id,d.endpoint_id,d.settled_at,COALESCE(v.rollback_deployment_id,'') FROM deployments d JOIN deployment_validations v ON v.deployment_id=d.id WHERE d.organization_id=? AND d.environment_id=? AND d.application_id=? AND d.id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), depID.String()).Scan(&instance, &endpoint, &settled, &rolledBack)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if rolledBack != "" {
			reason = RollbackAlreadyRolledBack
			return nil
		}
		var inFlight int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM deployment_validations v WHERE v.instance_id=? AND v.deployment_id<>? AND ((v.is_rollback=1 AND v.phase<>'done') OR EXISTS (SELECT 1 FROM deployments r WHERE r.id=v.rollback_deployment_id AND r.state IN ('planned','applying')))`), instance, depID.String()).Scan(&inFlight); err != nil {
			return err
		}
		if inFlight > 0 {
			reason = RollbackInFlight
			return nil
		}
		var previous, version int
		var project string
		err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT previous_revision,mapping_version,project FROM application_instances WHERE id=?`), instance).Scan(&previous, &version, &project)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var result string
		if previous > 0 {
			err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT result FROM deployments WHERE instance_id=? AND kind='apply' AND state='succeeded' AND revision=? AND id<>? AND settled_at<=? ORDER BY settled_at DESC,id DESC LIMIT 1`), instance, previous, depID.String(), settled).Scan(&result)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var prior storedDeploymentResult
		if result == "" || json.Unmarshal([]byte(result), &prior) != nil || len(prior.Services) == 0 {
			reason = RollbackNoPriorRevision
			return nil
		}
		var specRaw, digest string
		err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), a.OrganizationID, a.EnvironmentID, appID.String(), previous).Scan(&specRaw, &digest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var spec ApplicationSpec
		if err != nil || applicationSpecDigest([]byte(specRaw)) != digest || json.Unmarshal([]byte(specRaw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
			reason = RollbackPriorDefinitionInvalid
			return nil
		}
		bound, err := t.boundServices(ctx, tx, instance)
		if err != nil {
			return err
		}
		ran := map[string]string{}
		for _, s := range prior.Services {
			ran[s.Service] = s.ImageID
		}
		images := map[string]string{}
		for _, s := range spec.Services {
			if _, ok := bound[s.Name]; !ok || ran[s.Name] == "" {
				break
			}
			images[s.Name] = ran[s.Name]
		}
		if len(images) != len(spec.Services) || len(bound) != len(spec.Services) {
			reason = RollbackServiceSetChanged
			return nil
		}
		var raw string
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT snapshot FROM endpoint_inventory WHERE endpoint_id=?`), endpoint).Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var snapshot protocol.Snapshot
		_ = json.Unmarshal([]byte(raw), &snapshot) // unreadable reads as empty: nothing is on the host
		onHost := imagesOnHost(snapshot)
		for _, id := range images {
			if !onHost[id] {
				reason = RollbackPriorImagesMissing
				return nil
			}
		}
		out = &Rollback{InstanceID: instance, MappingVersion: version, Project: project, Revision: previous, Images: images}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, reason, nil
}
```

In `internal/store/store.go`, `TenancyStore`, after `MarkRollbackOutcome` add:

```go
	// RollbackTarget authorizes application.deploy as the policy's creator and returns the prior
	// revision and images, or an ineligibility reason (docs/application-schema.md, Rollback).
	RollbackTarget(ctx context.Context, access TenantAccess, applicationID, deploymentID string) (*Rollback, string, error)
```

- [ ] **Step 5: Guard the prune**

In `internal/store/samples.go`, `Prune`, replace the `deployments` entry of the statement list with:

```go
		// A deployment of an instance whose automated update may still roll back stays: the
		// rollback needs the prior apply's recorded images.
		{`DELETE FROM deployments WHERE id IN (SELECT d.id FROM deployments d WHERE d.settled_at IS NOT NULL AND d.settled_at<? AND NOT (d.kind='apply' AND d.state='succeeded' AND EXISTS (SELECT 1 FROM application_instances i WHERE i.id=d.instance_id AND d.revision IN (i.current_revision,i.previous_revision)) AND NOT EXISTS (SELECT 1 FROM deployments n WHERE n.instance_id=d.instance_id AND n.revision=d.revision AND n.kind='apply' AND n.state='succeeded' AND (n.settled_at>d.settled_at OR (n.settled_at=d.settled_at AND n.id>d.id)))) AND NOT EXISTS (SELECT 1 FROM applications a WHERE a.id=d.application_id AND a.removed_at IS NOT NULL) AND NOT EXISTS (SELECT 1 FROM deployment_validations v WHERE v.instance_id=d.instance_id AND ` + mayRollBack + `) LIMIT ?)`, now.Add(-DeploymentHistoryRetention)},
```

- [ ] **Step 6: Run the tests on both drivers**

Run: `gofmt -w internal/store && go vet ./internal/store/ && go test -race -count=1 ./internal/store/... && PG=… go test -count=1 ./internal/store/...`
Expected: PASS on SQLite and PostgreSQL, including the existing preflight, plan and prune tests (`TestPlanDeploymentBindsIdentities`, `TestPruneKeepsCurrentAndPreviousHistory`, whose manual validation holds nothing, `TestPruneKeepsRemovedApplicationHistory`).

- [ ] **Step 7: DOX and commit**

`internal/store/AGENTS.md`, Local Contracts, after the health-validation bullet add: "- Rollback (`rollback.go`): `RollbackTarget` authorizes `application.deploy` as the caller and decides in one read transaction, in this order: `already_rolled_back` (the validation named one), `rollback_in_flight` (another rollback of the instance still validating or planned/applying), `no_prior_revision` (no succeeded apply at `previous_revision` other than this one and settled no later), `prior_definition_invalid`, `service_set_changed` (prior spec services, the bound services and the prior result must match), `prior_images_missing` (every prior image ID listed in the latest inventory or run by a running container). `PlanRequest.PinImages` (`json:\"-\"`) makes the preflight take a service's image ID instead of its tag (`image_identity_invalid` for a malformed ID, `image_not_reported` when not on the host). `Prune` keeps every deployment of an instance with an automated validation that may still roll back (`mayRollBack`)."

```bash
git add internal/store
git commit -m "feat(store): rollback eligibility, pinned plan images and the prune guard" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: API — the validation loop (`RunValidations`) and the rollback

**Files:**
- Create: `internal/api/validations.go`
- Modify: `internal/api/server.go` (`Server.validations`)
- Modify: `internal/api/policies.go` (`performPolicyRun` names its deployment before sending)
- Modify: `internal/api/export_test.go` (test hooks)
- Test: `internal/api/validations_test.go`
- Docs: `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: Task 1 `s.observe`; Task 2 `PendingValidations`, `BeginObservation`, `FinishValidation`, `MarkRollbackPlanned`, `MarkRollbackOutcome`, `AttachPolicyRunDeployment`, `Judge`, `BaselineOf`, the constants; Task 3 `RollbackTarget`, `PlanRequest.PinImages`; existing `inspectForPlan`, `policyInspectionAllowed`, `maxFrameBytes`, `policyFailure`, `joinCodes`, `agents.deliver`, `Connected`, `ApplyPolicyDeployment`, `ErrPolicyChanged`; test fixtures `newPlanHost`, `planHost.online`, `planHost.policy`, `planHost.tick`, `planHost.runs`, `planHost.as`, `planHost.appID`, `tomorrow`, `policyAuditRows`, `fakeDigests`, `verifiedObservation`, `grant`, `waitFor`, `readEnvelope`, `writeEnvelope`.
- Produces:
  ```go
  // package api
  func (s *Server) RunValidations(ctx context.Context, done chan<- struct{})
  // export_test.go
  func ValidationTickForTest(s *Server, now time.Time)
  func SetValidationClockForTest(s *Server, interval time.Duration, clock func() time.Time)
  var ErrInspectionInvalidForTest = errInspectionInvalid
  ```
  Test helpers Task 5 reuses: `validationCaps`, `validationHost`, `newValidationHost`, `(*validationHost).automate`, `.frame`, `.settle`, `.inventory`, `.validation`, `.at`, `observations.on`, `health`, `afterGrace`, `afterWindow`, `hostImage`, `newContainer`, `newImage`.

- [ ] **Step 1: Write the failing tests**

Create `internal/api/validations_test.go`:

```go
package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// validationCaps is an agent that inspects with verdicts and health, applies and pulls.
var validationCaps = []string{protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull, protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict, protocol.CapabilityContainerInspectHealth}

const (
	afterGrace  = store.ValidationGrace + time.Second
	afterWindow = store.ValidationGrace + store.ValidationWindow + time.Second
)

var (
	hostImage      = "sha256:" + strings.Repeat("b", 64) // newPlanHost's image, tagged nginx:1
	hostDigest     = "nginx@sha256:" + strings.Repeat("c", 64)
	priorContainer = strings.Repeat("e", 64)
	newContainer   = strings.Repeat("f", 64)
	newImage       = "sha256:" + strings.Repeat("9", 64)
	newDigest      = "sha256:" + strings.Repeat("d", 64)
	generationSeq  atomic.Uint64
)

// nextGeneration is an inventory generation above every one sent before, inside the store's skew.
func nextGeneration() uint64 { return uint64(time.Now().Unix()) + 10 + generationSeq.Add(1) }

// observations answers the plan-time inspector, which validation shares: every container running,
// verified and healthy with no restarts unless on changes it.
type observations struct {
	mu sync.Mutex
	by map[string]func(*protocol.ContainerInspection) error
}

func (o *observations) on(container string, f func(*protocol.ContainerInspection) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.by == nil {
		o.by = map[string]func(*protocol.ContainerInspection) error{}
	}
	o.by[container] = f
}

func (o *observations) inspect(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
	in := verifiedObservation(target)
	in.Health = "healthy"
	o.mu.Lock()
	f := o.by[target.ContainerID]
	o.mu.Unlock()
	if f != nil {
		if err := f(&in); err != nil {
			return protocol.ContainerInspection{}, err
		}
	}
	return in, nil
}

func health(status string) func(*protocol.ContainerInspection) error {
	return func(in *protocol.ContainerInspection) error { in.Health = status; return nil }
}

// validationHost is a planHost whose web service already ran one succeeded manual apply of
// revision 1 on the host image (prior): the deployment a rollback returns to. Its agent is online
// with health, and every inspection goes through obs.
type validationHost struct {
	planHost
	sock    *agentSocket
	ctx     context.Context
	obs     *observations
	prior   string
	running protocol.Container // the web container the last settle reported
}

func newValidationHost(t *testing.T) *validationHost {
	t.Helper()
	v := &validationHost{planHost: newPlanHost(t, validationCaps, "web"), obs: &observations{}}
	api.SetPlanInspectorForTest(v.s, v.obs.inspect)
	v.sock, v.ctx = v.online(t, validationCaps)
	v.prior = v.applyByHand(t)
	v.settle(t, v.prior, priorContainer, hostImage, "", []protocol.Image{{ID: hostImage, Tags: []string{"nginx:1"}, Digests: []string{hostDigest}}})
	return v
}

// applyByHand plans and applies the latest revision as the planner and reads the frame.
func (v *validationHost) applyByHand(t *testing.T) string {
	t.Helper()
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "POST", v.deployments, v.planBody, 201)), &d); err != nil {
		t.Fatal(err)
	}
	v.do(t, "POST", v.deployments+"/"+d.ID+"/apply", `{"confirm":"shop"}`, 202)
	if req := v.frame(t); req.Deployment != d.ID {
		t.Fatalf("frame for %s, want %s", req.Deployment, d.ID)
	}
	return d.ID
}

// frame reads the next frame as a deployment request.
func (v *validationHost) frame(t *testing.T) protocol.DeploymentRequest {
	t.Helper()
	f := readEnvelope(t, v.ctx, v.sock.conn)
	var req protocol.DeploymentRequest
	if f.Type != protocol.TypeDeploymentApply || json.Unmarshal(f.Payload, &req) != nil {
		t.Fatalf("expected a deployment frame, got %s", f.Type)
	}
	return req
}

// settle answers deployment id as the agent would: web replaced by container on image (digest for
// a pulled service), then an inventory showing it beside images.
func (v *validationHost) settle(t *testing.T, id, container, image, digest string, images []protocol.Image) {
	t.Helper()
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments+"/"+id, "", 200)), &d); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Truncate(time.Second)
	res := protocol.DeploymentResult{Deployment: id, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{{Service: "web", ContainerID: container, ImageID: image, CreatedUnix: created.Unix(), ImageDigest: digest}}}
	if err := v.st.Tenancy().SettleDeployment(context.Background(), v.ag.id, res); err != nil {
		t.Fatal(err)
	}
	v.running = protocol.Container{ID: container, Name: "shop-web", ImageID: image, State: "running", ComposeProject: "shop", CreatedAt: created, Mounts: []protocol.Mount{}}
	v.inventory(t, images)
}

// inventory sends the running web container beside images.
func (v *validationHost) inventory(t *testing.T, images []protocol.Image) {
	t.Helper()
	raw, _ := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{v.running}, Images: images})
	if _, err := v.st.Tenancy().AcceptInventory(context.Background(), v.ag.id, nextGeneration(), time.Now(), raw); err != nil {
		t.Fatal(err)
	}
}

// automate saves an apply-mode policy, offers a newer registry digest and runs its window: the
// policy applies the update and the agent settles it on newContainer running newImage. The host
// keeps the old image, untagged.
func (v *validationHost) automate(t *testing.T) (string, *store.UpdatePolicy) {
	t.Helper()
	v.do(t, "PUT", "/api/organizations/a/registry-policy", `{"anonymous_pull_enabled":true}`, 204)
	api.SetDigestResolverForTest(v.s, &fakeDigests{digest: newDigest})
	p := v.policy(t, store.PolicyModeApply)
	v.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := v.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunApplied {
		t.Fatalf("runs: %+v", runs)
	}
	if req := v.frame(t); req.Deployment != runs[0].DeploymentID {
		t.Fatalf("frame for %s, want %s", req.Deployment, runs[0].DeploymentID)
	}
	v.settle(t, runs[0].DeploymentID, newContainer, newImage, newDigest, []protocol.Image{{ID: hostImage, Digests: []string{hostDigest}}, {ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{"nginx@" + newDigest}}})
	return runs[0].DeploymentID, p
}

func (v *validationHost) validation(t *testing.T, deployment string) *store.Validation {
	t.Helper()
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments+"/"+deployment, "", 200)), &d); err != nil {
		t.Fatal(err)
	}
	if d.Validation == nil {
		t.Fatalf("deployment %s has no validation", deployment)
	}
	return d.Validation
}

// at runs one validation tick at the validation's start plus offset.
func (v *validationHost) at(t *testing.T, deployment string, offset time.Duration) {
	t.Helper()
	api.ValidationTickForTest(v.s, v.validation(t, deployment).StartedAt.Add(offset))
}

func (v *validationHost) policyNow(t *testing.T) *store.UpdatePolicy {
	t.Helper()
	p, _, err := v.st.Tenancy().ReadUpdatePolicy(context.Background(), v.as("usr_planner"), v.appID())
	if err != nil || p == nil {
		t.Fatalf("policy: %+v %v", p, err)
	}
	return p
}

func (v *validationHost) deploymentCount(t *testing.T) int {
	t.Helper()
	var list []store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments, "", 200)), &list); err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func (v *validationHost) pauseRows(t *testing.T) int {
	t.Helper()
	n := 0
	for _, r := range policyAuditRows(t, v.planHost, v.admin) {
		if r.Action == store.AuditPolicyPaused {
			n++
		}
	}
	return n
}

func TestValidationRecordsAHealthyUpdate(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	got := v.validation(t, id)
	if !got.Automated || got.IsRollback || got.PolicyRunID == "" || got.Phase != store.PhaseGrace || !got.ObserveUntil.Equal(got.StartedAt.Add(store.ValidationGrace+store.ValidationWindow)) {
		t.Fatalf("opened: %+v", got)
	}
	api.ValidationTickForTest(v.s, got.StartedAt.Add(store.ValidationGrace-time.Second))
	if got := v.validation(t, id); got.Phase != store.PhaseGrace {
		t.Fatalf("observed during grace: %+v", got)
	}
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Phase != store.PhaseObserving || got.Verdict != "" {
		t.Fatalf("after grace: %+v", got)
	}
	v.at(t, id, afterWindow)
	got = v.validation(t, id)
	if got.Phase != store.PhaseDone || got.Verdict != store.VerdictHealthy || got.Rollback != nil || got.FinishedAt == nil {
		t.Fatalf("finished: %+v", got)
	}
	if p := v.policyNow(t); p.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", p)
	}
	audited := false
	for _, r := range policyAuditRows(t, v.planHost, v.admin) {
		if r.Action == store.AuditValidation && r.Resource == v.appID()+"/deployments/"+id && r.UserID == "system" && r.Result == "success" && r.CorrelationID == got.CorrelationID {
			audited = true
		}
	}
	if !audited {
		t.Fatal("no application.validation row")
	}
}

// An unhealthy update is rolled back to the prior apply's image, pinned by ID, as the policy's
// creator; the policy pauses with one audit row (Review Focus 1).
func TestValidationRollsBackAnUnhealthyUpdate(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	req := v.frame(t)
	if req.Revision != 1 || len(req.Services) != 1 || req.Services[0].ImageID != hostImage || req.Services[0].Pull != nil || req.Services[0].Replaces.ContainerID != newContainer {
		t.Fatalf("rollback frame: %+v", req)
	}
	got := v.validation(t, id)
	if got.Verdict != store.VerdictUnhealthy || got.Detail != "web" || got.Rollback == nil || *got.Rollback != (store.ValidationRollback{DeploymentID: req.Deployment, Revision: 1, Outcome: store.RollbackApplied}) {
		t.Fatalf("validation: %+v %+v", got, got.Rollback)
	}
	var rb store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments+"/"+req.Deployment, "", 200)), &rb); err != nil {
		t.Fatal(err)
	}
	if rb.State != "applying" || rb.CreatedBy != "usr_planner" || rb.AppliedBy != "usr_planner" || rb.CorrelationID == got.CorrelationID || rb.Plan.Services[0].ImageID != hostImage {
		t.Fatalf("rollback deployment: %+v", rb)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyPaused || pol.PausedReason != store.ValidationReasonRolledBack {
		t.Fatalf("policy: %+v", pol)
	}
	if n := v.pauseRows(t); n != 1 {
		t.Fatalf("pause rows: %d", n)
	}
	if runs := v.runs(t, "usr_planner"); runs[0].Outcome != store.RunApplied || runs[0].DeploymentID != id {
		t.Fatalf("the run changed: %+v", runs[0])
	}
}

func TestValidationStopsWhenThePriorImagesAreGone(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	v.inventory(t, []protocol.Image{{ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{"nginx@" + newDigest}}})
	v.at(t, id, afterGrace)
	got := v.validation(t, id)
	if got.Verdict != store.VerdictUnhealthy || got.Rollback == nil || *got.Rollback != (store.ValidationRollback{Outcome: store.RollbackIneligible, Detail: store.RollbackPriorImagesMissing}) {
		t.Fatalf("validation: %+v %+v", got, got.Rollback)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyPaused || pol.PausedReason != store.ValidationReasonNotRolledBack+store.RollbackPriorImagesMissing {
		t.Fatalf("policy: %+v", pol)
	}
	if n := v.deploymentCount(t); n != 2 {
		t.Fatalf("deployments: %d", n)
	}
}

// A manual apply gets its verdict and nothing else.
func TestValidationOfAManualApplyOnlyRecords(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(priorContainer, health("unhealthy"))
	v.at(t, v.prior, afterGrace)
	got := v.validation(t, v.prior)
	if got.Automated || got.Verdict != store.VerdictUnhealthy || got.Detail != "web" || got.Rollback != nil {
		t.Fatalf("manual: %+v", got)
	}
	if n := v.deploymentCount(t); n != 1 {
		t.Fatalf("deployments: %d", n)
	}
}

func TestValidationOfAnOfflineHostIsUnverifiable(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	v.sock.conn.CloseNow()
	waitFor(t, func() bool { return !v.s.Connected(v.ag.id) })
	v.at(t, id, afterGrace)
	got := v.validation(t, id)
	if got.Verdict != store.VerdictUnverifiable || got.Detail != store.ValidationDetailUnobserved || got.Rollback != nil {
		t.Fatalf("offline: %+v", got)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyPaused || pol.PausedReason != store.ValidationReasonUnverified+store.ValidationDetailUnobserved {
		t.Fatalf("policy: %+v", pol)
	}
}

func TestValidationWithoutTheHealthCapabilityIsUnverifiable(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	if err := v.st.Tenancy().SetEndpointCapabilities(context.Background(), v.ag.id, policyCaps); err != nil {
		t.Fatal(err)
	}
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Verdict != store.VerdictUnverifiable || got.Detail != store.ValidationDetailNoHealth {
		t.Fatalf("no health: %+v", got)
	}
}

func TestValidationOfAnInvalidInspectionIsUnverifiable(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, func(*protocol.ContainerInspection) error { return api.ErrInspectionInvalidForTest })
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Verdict != store.VerdictUnverifiable || got.Detail != store.ValidationDetailInvalid {
		t.Fatalf("invalid inspection: %+v", got)
	}
	if pol := v.policyNow(t); pol.PausedReason != store.ValidationReasonUnverified+store.ValidationDetailInvalid {
		t.Fatalf("policy: %+v", pol)
	}
}

// Over the real socket the loop's grant names the system actor in the wire's identifier form, and
// a health agent's answer becomes the baseline.
func TestValidationInspectsOverTheAgentSocketAsTheSystem(t *testing.T) {
	v := newValidationHost(t)
	api.SetPlanInspectorForTest(v.s, nil)
	start := v.validation(t, v.prior).StartedAt
	done := make(chan struct{})
	go func() { defer close(done); api.ValidationTickForTest(v.s, start.Add(afterGrace)) }()
	g := grant(t, v.ctx, v.sock)
	if g.Actor != "system-validation" || g.Validate(time.Now()) != nil || g.Target.ContainerID != priorContainer {
		t.Fatalf("grant: %+v", g)
	}
	in := verifiedObservation(g.Target)
	in.Health, in.RestartCount = "healthy", 2
	writeEnvelope(t, v.ctx, v.sock.conn, protocol.TypeInspectionResult, protocol.InspectionResult{Request: g.Request, Status: "ok", Result: &in})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick never finished")
	}
	pending, err := v.st.Tenancy().PendingValidations(context.Background())
	if err != nil || len(pending) != 1 || pending[0].Phase != store.PhaseObserving || pending[0].Baseline["web"] != (store.ServiceBaseline{ContainerID: priorContainer, RestartCount: 2}) {
		t.Fatalf("pending: %+v %v", pending, err)
	}
}

// A restart mid-window resumes the window; a rollback already dispatched is never sent again.
func TestValidationResumesAfterARestart(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	if _, err := v.st.Tenancy().ReconcileAfterStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := v.validation(t, id); got.Phase != store.PhaseObserving {
		t.Fatalf("the open window did not survive the restart: %+v", got)
	}
	v.at(t, id, afterWindow)
	if got := v.validation(t, id); got.Verdict != store.VerdictHealthy {
		t.Fatalf("resumed: %+v", got)
	}
}

func TestValidationNeverDispatchesARollbackTwice(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	first := v.frame(t)
	if _, err := v.st.Tenancy().ReconcileAfterStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	v.at(t, id, afterWindow)
	api.ValidationTickForTest(v.s, time.Now().Add(time.Hour))
	got := v.validation(t, id)
	if got.Rollback == nil || got.Rollback.DeploymentID != first.Deployment || got.Rollback.Outcome != store.RollbackApplied {
		t.Fatalf("rollback: %+v", got.Rollback)
	}
	if n := v.deploymentCount(t); n != 3 {
		t.Fatalf("deployments: %d", n)
	}
}

// Shutdown waits for a rollback in flight: done closes only once its frame is sent and recorded.
func TestRunValidationsWaitsForAnInFlightRollback(t *testing.T) {
	v := newValidationHost(t)
	entered, gate := make(chan struct{}, 1), make(chan struct{})
	var calls atomic.Int32
	v.obs.on(newContainer, func(in *protocol.ContainerInspection) error {
		in.Health = "unhealthy"
		if calls.Add(1) == 2 { // the first is the validation's poll, the second the rollback plan's
			entered <- struct{}{}
			<-gate
		}
		return nil
	})
	id, _ := v.automate(t)
	start := v.validation(t, id).StartedAt
	api.SetValidationClockForTest(v.s, 10*time.Millisecond, func() time.Time { return start.Add(afterGrace) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go v.s.RunValidations(ctx, done)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no rollback reached its plan")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("done closed with a rollback in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("done never closed")
	}
	req := v.frame(t)
	if got := v.validation(t, id); got.Rollback == nil || got.Rollback.Outcome != store.RollbackApplied || got.Rollback.DeploymentID != req.Deployment {
		t.Fatalf("rollback: %+v", got.Rollback)
	}
}

// Review Focus 2: a rollback's own failed validation is recorded and changes nothing else.
func TestValidationNeverRollsBackARollback(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	req := v.frame(t)
	back := strings.Repeat("7", 64)
	v.obs.on(back, health("unhealthy"))
	v.settle(t, req.Deployment, back, hostImage, "", []protocol.Image{{ID: hostImage, Digests: []string{hostDigest}}, {ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{"nginx@" + newDigest}}})
	if got := v.validation(t, req.Deployment); !got.IsRollback || got.Automated {
		t.Fatalf("rollback's validation: %+v", got)
	}
	v.at(t, req.Deployment, afterGrace)
	if got := v.validation(t, req.Deployment); got.Verdict != store.VerdictUnhealthy || got.Rollback != nil {
		t.Fatalf("rollback's verdict: %+v", got)
	}
	if n := v.deploymentCount(t); n != 3 {
		t.Fatalf("deployments: %d", n)
	}
	if pol := v.policyNow(t); pol.PausedReason != store.ValidationReasonRolledBack || v.pauseRows(t) != 1 {
		t.Fatalf("policy: %+v, pause rows %d", pol, v.pauseRows(t))
	}
}

// Review Focus 3: a manual apply inside an automated window makes the automated one changed.
func TestValidationOfAnUpdateReplacedByAManualApply(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	manual := v.applyByHand(t)
	v.settle(t, manual, strings.Repeat("6", 64), newImage, "", []protocol.Image{{ID: hostImage, Digests: []string{hostDigest}}, {ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{"nginx@" + newDigest}}})
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Verdict != store.VerdictChanged || got.Detail != "web" || got.Rollback != nil {
		t.Fatalf("automated: %+v", got)
	}
	if got := v.validation(t, manual); got.Automated || got.Verdict == store.VerdictChanged {
		t.Fatalf("manual: %+v", got)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", pol)
	}
	if n := v.deploymentCount(t); n != 3 {
		t.Fatalf("deployments: %d", n)
	}
}

// Review Focus 4: a new connection mid-window carries on the same window.
func TestValidationSurvivesAnAgentReconnect(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	v.sock.conn.CloseNow()
	waitFor(t, func() bool { return !v.s.Connected(v.ag.id) })
	v.at(t, id, afterGrace+store.ValidationPoll)
	if got := v.validation(t, id); got.Phase != store.PhaseObserving {
		t.Fatalf("offline mid-window ended it: %+v", got)
	}
	v.sock, v.ctx = v.online(t, validationCaps)
	v.at(t, id, afterWindow)
	if got := v.validation(t, id); got.Verdict != store.VerdictHealthy {
		t.Fatalf("after the reconnect: %+v", got)
	}
}

// Review Focus 5: with the policy or the adoption gone mid-window, a verdict and nothing else.
func TestValidationWhenThePolicyOrTheAdoptionGoes(t *testing.T) {
	t.Run("policy deleted", func(t *testing.T) {
		v := newValidationHost(t)
		v.obs.on(newContainer, health("unhealthy"))
		id, _ := v.automate(t)
		if err := v.st.Tenancy().DeleteUpdatePolicy(context.Background(), v.as("usr_planner"), v.appID()); err != nil {
			t.Fatal(err)
		}
		v.at(t, id, afterGrace)
		if got := v.validation(t, id); got.Verdict != store.VerdictUnhealthy || got.PolicyRunID != "" || got.Rollback != nil {
			t.Fatalf("policy deleted: %+v", got)
		}
		if n := v.deploymentCount(t); n != 2 {
			t.Fatalf("deployments: %d", n)
		}
	})
	t.Run("application released", func(t *testing.T) {
		v := newValidationHost(t)
		v.obs.on(newContainer, health("unhealthy"))
		id, _ := v.automate(t)
		var d store.Deployment
		if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments+"/"+id, "", 200)), &d); err != nil {
			t.Fatal(err)
		}
		v.do(t, "DELETE", strings.TrimSuffix(v.deployments, "/deployments")+"/adoption", `{"instance_id":"`+d.InstanceID+`","confirm":"shop"}`, 204)
		v.at(t, id, afterGrace)
		if got := v.validation(t, id); got.Verdict != store.VerdictChanged || got.Detail != store.ValidationDetailReleased || got.Rollback != nil {
			t.Fatalf("released: %+v", got)
		}
		if pol := v.policyNow(t); pol.Status != store.PolicyActive {
			t.Fatalf("policy: %+v", pol)
		}
	})
}
```

- [ ] **Step 2: Add the test hooks**

Append to `internal/api/export_test.go`:

```go
// ValidationTickForTest runs one validation tick at now; it returns when the tick, rollback
// included, is done.
func ValidationTickForTest(s *Server, now time.Time) { s.validationTick(context.Background(), now) }

// SetValidationClockForTest makes RunValidations tick every interval and read the time from clock.
func SetValidationClockForTest(s *Server, interval time.Duration, clock func() time.Time) {
	s.validations.interval, s.validations.now = interval, clock
}

// ErrInspectionInvalidForTest is what an inspection that failed validation returns.
var ErrInspectionInvalidForTest = errInspectionInvalid
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/api/ -run 'Validation'`
Expected: FAIL to compile with `s.validationTick undefined` and `s.validations undefined`.

- [ ] **Step 4: Name the policy run's deployment before sending**

In `internal/api/policies.go`, change `performPolicyRun`'s signature to

```go
func (s *Server) performPolicyRun(ctx context.Context, a store.TenantAccess, pol store.UpdatePolicy, run string) (outcome, deployment, detail string) {
```

its call in `runPolicy` to `s.performPolicyRun(ctx, a, pol, run)`, and insert directly above the line `if !s.agents.deliver(applied.EndpointID, envelope(protocol.TypeDeploymentApply, frame)) {`:

```go
	// Named before the frame leaves: a settle that beats FinishPolicyRun still finds this run and
	// validates the deployment as automated. Unnamed, it is validated as a manual apply.
	if err := ts.AttachPolicyRunDeployment(ctx, run, applied.ID); err != nil {
		log.Printf("[POLICY] run %s: naming deployment %s: %v", run, applied.ID, err)
	}
```

- [ ] **Step 5: Implement the loop**

In `internal/api/server.go`, add to `Server` after `policies policyScheduler`:

```go
	// validations is the health-validation loop's in-memory state (validations.go).
	validations validationLoop
```

Create `internal/api/validations.go`:

```go
package api

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/google/uuid"
)

// validationActor is who a validation's inspections act as on the wire. A grant's actor must be an
// identifier ([A-Za-z0-9_-]), so it is not the audit rows' "system".
const validationActor = "system-validation"

// validationLoop is the validation loop's in-memory half; every row is durable.
type validationLoop struct {
	tick sync.Mutex // one tick at a time
	// Tests only: the tick interval (zero is store.ValidationPoll) and the clock (nil is time.Now).
	interval time.Duration
	now      func() time.Time
}

// RunValidations watches every settled apply until ctx ends (docs/application-schema.md, Health
// validation). A tick runs to its end, a rollback included, so done closes only between ticks and
// runServer can wait on it before the store closes.
func (s *Server) RunValidations(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	interval := s.validations.interval
	if interval == 0 {
		interval = store.ValidationPoll
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			now := time.Now()
			if s.validations.now != nil {
				now = s.validations.now()
			}
			s.validationTick(ctx, now)
		}
	}
}

// validationTick works through the pending validations oldest first. Shutdown stops it between
// rows; the rows resume on the next start.
func (s *Server) validationTick(ctx context.Context, now time.Time) {
	s.validations.tick.Lock()
	defer s.validations.tick.Unlock()
	if s.stopping.Load() {
		return
	}
	pending, err := s.store.Tenancy().PendingValidations(ctx)
	if err != nil {
		log.Printf("[VALIDATION] pending validations unreadable: %v", err)
		return
	}
	for _, p := range pending {
		if ctx.Err() != nil || s.stopping.Load() {
			return
		}
		func() {
			// One row's panic must not end the loop for every other deployment.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[VALIDATION] deployment %s panicked: %v", p.DeploymentID, r)
				}
			}()
			s.validate(ctx, p, now)
		}()
	}
}

// validate advances one validation at now: take the baseline once grace has passed, judge each
// poll, finish on a terminal verdict, and decide a rollback when one is due.
func (s *Server) validate(ctx context.Context, p store.PendingValidation, now time.Time) {
	if p.Phase == store.PhaseDone { // a rollback decision still owed, from before a restart
		s.rollback(context.WithoutCancel(ctx), p)
		return
	}
	if p.Released {
		s.finishValidation(ctx, p, store.VerdictChanged, store.ValidationDetailReleased)
		return
	}
	if p.Phase == store.PhaseGrace && now.Before(p.StartedAt.Add(store.ValidationGrace)) {
		return
	}
	// Past the window by a further grace with no complete observation: give up.
	late := !now.Before(p.ObserveUntil.Add(store.ValidationGrace))
	switch {
	case !p.Health:
		s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailNoHealth)
		return
	case !s.Connected(p.EndpointID):
		if p.Phase == store.PhaseGrace || late {
			s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailUnobserved)
		}
		return
	}
	obs, invalid := s.observeServices(ctx, p)
	if ctx.Err() != nil || s.stopping.Load() {
		return // a poll cut short by shutdown says nothing about the host
	}
	if invalid {
		s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailInvalid)
		return
	}
	baseline := p.Baseline
	if p.Phase == store.PhaseGrace {
		var ok bool
		if baseline, ok = store.BaselineOf(obs); !ok {
			if late {
				s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailUnobserved)
			}
			return
		}
		if err := s.store.Tenancy().BeginObservation(ctx, p.DeploymentID, baseline); err != nil {
			log.Printf("[VALIDATION] deployment %s: recording the baseline: %v", p.DeploymentID, err)
			return
		}
	}
	verdict, detail := store.Judge(obs, baseline, !now.Before(p.ObserveUntil))
	switch {
	case verdict != "":
		s.finishValidation(ctx, p, verdict, detail)
	case late:
		s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailUnobserved)
	}
}

// observeServices inspects every settled container the store has not already placed as gone or
// replaced, one at a time under the plan's inspection budget. invalid: an answer failed validation.
func (s *Server) observeServices(ctx context.Context, p store.PendingValidation) (obs []store.Observation, invalid bool) {
	ctx, cancel := context.WithTimeout(ctx, planInspectionBudget)
	defer cancel()
	for _, svc := range p.Services {
		o := store.Observation{Service: svc.Service, Presence: svc.Presence}
		if svc.Presence == store.PresencePresent || svc.Presence == store.PresenceUnknown {
			target := protocol.InspectionTarget{ContainerID: svc.ContainerID, ImageID: svc.ImageID, CreatedUnix: svc.CreatedUnix}
			in, err := s.observe(ctx, p.EndpointID, validationActor, p.OrganizationID, target, true, func() bool { return true })
			if errors.Is(err, errInspectionInvalid) {
				return nil, true
			}
			if err == nil {
				o.Inspection = &in
			}
		}
		obs = append(obs, o)
	}
	return obs, false
}

// finishValidation records the verdict and, when the store says a decision is due, decides the
// rollback, uncancelled, so a shutdown waits for it.
func (s *Server) finishValidation(ctx context.Context, p store.PendingValidation, verdict, detail string) {
	decide, err := s.store.Tenancy().FinishValidation(ctx, p.DeploymentID, verdict, detail)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("[VALIDATION] deployment %s: recording %s: %v", p.DeploymentID, verdict, err)
		}
		return
	}
	log.Printf("[VALIDATION] deployment %s: %s", p.DeploymentID, verdict)
	if decide {
		s.rollback(context.WithoutCancel(ctx), p)
	}
}

// rollback decides and, when eligible, performs the rollback of a failed automated update, then
// records the outcome, which pauses the policy.
func (s *Server) rollback(ctx context.Context, p store.PendingValidation) {
	outcome, detail := s.performRollback(ctx, p)
	if err := s.store.Tenancy().MarkRollbackOutcome(ctx, p.DeploymentID, outcome, detail); err != nil {
		log.Printf("[VALIDATION] deployment %s: recording rollback %s: %v", p.DeploymentID, outcome, err)
		return
	}
	log.Printf("[VALIDATION] deployment %s: rollback %s %s", p.DeploymentID, outcome, detail)
}

// performRollback is the rollback as the policy's creator under a fresh correlation ID: eligibility
// in the store, then a plan pinned to the prior images, the apply and the frame, exactly as a
// policy run sends one. The rollback is named on the validation before it is applied.
func (s *Server) performRollback(ctx context.Context, p store.PendingValidation) (outcome, detail string) {
	ts := s.store.Tenancy()
	a := store.TenantAccess{ActorID: p.CreatedBy, OrganizationID: p.OrganizationID, EnvironmentID: p.EnvironmentID, CorrelationID: uuid.NewString()}
	app := p.ApplicationID
	rb, reason, err := ts.RollbackTarget(ctx, a, app, p.DeploymentID)
	if err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	if reason != "" {
		return store.RollbackIneligible, reason
	}
	ep, err := ts.ReadEndpoint(ctx, a, p.EndpointID)
	if err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	pre, err := ts.PreflightApplication(ctx, a, app)
	if err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	key, private := s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate
	maxFrame := maxFrameBytes(ep.Capabilities)
	req := store.PlanRequest{InstanceID: rb.InstanceID, MappingVersion: rb.MappingVersion, Revision: rb.Revision, Confirm: rb.Project, PinImages: rb.Images, MaxFrameBytes: maxFrame,
		Inspections: s.inspectForPlan(ctx, a, ep, pre, func() {}, s.policyInspectionAllowed(a, ep.ID))}
	d, err := ts.PlanDeployment(ctx, a, app, req, nil, key, private)
	var blocked *store.PreflightBlockedError
	if errors.As(err, &blocked) {
		return store.RollbackFailed, joinCodes(blocked.Blockers, 255)
	}
	if err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	if err := ts.MarkRollbackPlanned(ctx, p.DeploymentID, d.ID); err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	if !s.Connected(d.EndpointID) {
		return store.RollbackFailed, "endpoint_offline"
	}
	applied, frame, err := ts.ApplyPolicyDeployment(ctx, a, p.PolicyID, app, d.ID, d.Plan.Project, key, maxFrame)
	if errors.Is(err, store.ErrPolicyChanged) {
		return store.RollbackFailed, "policy_changed"
	}
	if err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	if !s.agents.deliver(applied.EndpointID, envelope(protocol.TypeDeploymentApply, frame)) {
		if err := ts.FailDeployment(ctx, applied.ID, "the endpoint disconnected before the deployment was sent"); err != nil {
			log.Printf("[VALIDATION] deployment %s: recording an unsent rollback: %v", applied.ID, err)
		}
		return store.RollbackFailed, store.RollbackNotSent
	}
	return store.RollbackApplied, ""
}

// rollbackCode names a refusal in a rollback's detail: lost authority is creator_lost, anything
// else the policy scheduler's code.
func rollbackCode(err error) string {
	if errors.Is(err, store.ErrForbidden) {
		return store.RollbackCreatorLost
	}
	_, _, code := policyFailure(err)
	return code
}
```

- [ ] **Step 6: Run to verify they pass, on both drivers**

Run: `gofmt -w internal/api && go vet ./internal/api/ && go test -race -count=1 ./internal/api/ && PG=… go test -count=1 ./internal/api/`
Expected: PASS, including the scheduler tests `TestPolicyRunPlansAndAppliesAsItsCreator` and `TestRunPoliciesClosesDoneOnlyAfterTheRunInFlight` (the run now names its deployment before sending) and `TestPlanInspectsEachMappedServiceInTurn` (the plan's inspections now go through `observe`).

- [ ] **Step 7: DOX and commit**

`internal/api/AGENTS.md`, Local Contracts, after the `policies.go` bullet add: "- `validations.go` is the health-validation loop, `RunValidations(ctx, done)`: a `store.ValidationPoll` tick under its own mutex (no tick while stopping) reads `PendingValidations` and advances each row in turn: after `ValidationGrace` it needs `container.inspect.health` (else `unverifiable`) and a connected agent (offline at the baseline is `unverifiable`; later it waits), inspects each settled container the store has not placed as gone or replaced through `observe` as `system-validation` under the plan's 10 s budget, takes the baseline (`BeginObservation`), judges every poll with `store.Judge`, and past `observe_until` + `ValidationGrace` without a complete observation records `the host could not be observed`. A released instance is `changed`. When `FinishValidation` reports a decision due (or the row was left awaiting one), the rollback runs under `context.WithoutCancel` as the policy's `created_by` with a fresh correlation ID: `RollbackTarget`, `inspectForPlan`, `PlanDeployment` with `PinImages`, `MarkRollbackPlanned`, `ApplyPolicyDeployment` and `deliver` (`FailDeployment` and `not_sent` when unsent), then `MarkRollbackOutcome` pauses the policy. `done` closes only between ticks. `performPolicyRun` names its deployment on the run (`AttachPolicyRunDeployment`) before the frame leaves." In `## Verification` extend the first bullet's parenthesis with "; `validations_test.go` the loop against a fake agent socket through `newPlanHost` with `SetPlanInspectorForTest` answering health and `ValidationTickForTest`/`SetValidationClockForTest`".

```bash
git add internal/api
git commit -m "feat(api): health validation loop and eligible rollback" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Validation on policy runs, the API's JSON, and `cmd/server` wiring

**Files:**
- Modify: `internal/store/policies.go` (`PolicyRun.Validation`; `policyRunColumns` and `policyRuns` join the validation)
- Test: `internal/store/policies_test.go`
- Test: `internal/api/validations_test.go` (JSON), `internal/api/deployment_plan_test.go` (pins are the server's)
- Modify: `cmd/server/main.go` (start `RunValidations`; wait on it)
- Test: `cmd/server/validations_test.go`
- Docs: root `AGENTS.md` (the `cmd/server` paragraph), `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: Task 2 `validationColumns`, `validationScan`, `validationFixture`; Task 4 `api.Server.RunValidations`, `newValidationHost`, `automate`, `frame`, `at`, `afterGrace`, `health`, `newContainer`.
- Produces: `PolicyRun.Validation *Validation` (`json:"validation,omitempty"`), carried by `GET .../update-policy` and `GET .../update-policy/runs`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/policies_test.go`:

```go
// A run's validation, rollback included, comes with the run; the run's own outcome is untouched.
func TestPolicyRunsCarryTheirValidation(t *testing.T) {
	st, a, app, _, _, run, _, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
		t.Fatal(err)
	}
	if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackIneligible, RollbackPriorImagesMissing); err != nil {
		t.Fatal(err)
	}
	runs, err := ts.ListPolicyRuns(ctx, a, app.ID, 20)
	if err != nil || len(runs) != 1 || runs[0].ID != run || runs[0].Outcome != RunApplied || runs[0].Validation == nil || runs[0].Validation.DeploymentID != updated.ID || runs[0].Validation.Verdict != VerdictUnhealthy || runs[0].Validation.Rollback == nil || runs[0].Validation.Rollback.Detail != RollbackPriorImagesMissing {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	if _, viaPolicy, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || len(viaPolicy) != 1 || viaPolicy[0].Validation == nil || viaPolicy[0].Validation.Verdict != VerdictUnhealthy {
		t.Fatalf("policy runs: %+v %v", viaPolicy, err)
	}
	// A run with no deployment has no validation.
	skipped := rawPolicyRun(t, st, &UpdatePolicy{ID: runs[0].PolicyID, OrganizationID: a.OrganizationID, EnvironmentID: a.EnvironmentID, ApplicationID: app.ID}, instant("2026-09-25T10:00:00Z"), RunSkippedMissed)
	runs, err = ts.ListPolicyRuns(ctx, a, app.ID, 20)
	if err != nil || len(runs) != 2 || runs[0].ID != skipped || runs[0].Validation != nil {
		t.Fatalf("skipped run: %+v %v", runs, err)
	}
}
```

Append to `internal/api/validations_test.go`:

```go
// Deployment detail, the deployment list and the policy's runs carry the validation without its
// baseline; a deployment still applying carries none.
func TestValidationJSON(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	rollback := v.frame(t)
	policyURL := strings.TrimSuffix(v.deployments, "deployments") + "update-policy"
	for name, body := range map[string]string{
		"detail": v.do(t, "GET", v.deployments+"/"+id, "", 200),
		"list":   v.do(t, "GET", v.deployments, "", 200),
		"policy": v.do(t, "GET", policyURL, "", 200),
		"runs":   v.do(t, "GET", policyURL+"/runs?limit=20", "", 200),
	} {
		if !strings.Contains(body, `"validation":{`) || strings.Contains(body, "baseline") {
			t.Fatalf("%s: %s", name, body)
		}
	}
	var pol struct {
		PausedReason string            `json:"paused_reason"`
		Runs         []store.PolicyRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(v.do(t, "GET", policyURL, "", 200)), &pol); err != nil || len(pol.Runs) != 1 {
		t.Fatalf("policy: %+v %v", pol, err)
	}
	r := pol.Runs[0].Validation
	if r == nil || r.Verdict != store.VerdictUnhealthy || r.Rollback == nil || *r.Rollback != (store.ValidationRollback{DeploymentID: rollback.Deployment, Revision: 1, Outcome: store.RollbackApplied}) || pol.PausedReason != store.ValidationReasonRolledBack {
		t.Fatalf("run validation: %+v, paused %q", r, pol.PausedReason)
	}
	if body := v.do(t, "GET", v.deployments+"/"+rollback.Deployment, "", 200); strings.Contains(body, `"validation"`) {
		t.Fatalf("an applying deployment carries a validation: %s", body)
	}
}
```

Append to `internal/api/deployment_plan_test.go`:

```go
// Pinned images are the rollback's to set: a client cannot hand them in.
func TestPlanRefusesClientSuppliedPins(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	for _, key := range []string{"pin_images", "PinImages"} {
		forged := `,"` + key + `":{"web":"` + h.targets[0].ImageID + `"}}`
		h.do(t, "POST", h.deployments, strings.TrimSuffix(h.planBody, "}")+forged, 400)
	}
}
```

Create `cmd/server/validations_test.go`:

```go
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// runServer starts the validation loop and waits for it before the store closes, so a rollback in
// flight is sent and recorded. Removing either half fails here.
func TestServerRunsAndAwaitsTheValidationLoop(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var started, awaited string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			if sel, ok := n.Call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RunValidations" && len(n.Call.Args) == 2 {
				if id, ok := n.Call.Args[1].(*ast.Ident); ok {
					started = id.Name
				}
			}
		case *ast.UnaryExpr:
			if id, ok := n.X.(*ast.Ident); ok && n.Op == token.ARROW && started != "" && id.Name == started {
				awaited = id.Name
			}
		}
		return true
	})
	if started == "" || awaited != started {
		t.Fatalf("RunValidations started with done %q, awaited %q", started, awaited)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/store/ -run TestPolicyRunsCarryTheirValidation; go test -count=1 ./internal/api/ -run 'TestValidationJSON|TestPlanRefusesClientSuppliedPins'; go test -count=1 ./cmd/server/ -run TestServerRunsAndAwaitsTheValidationLoop`
Expected: the store test fails to compile (`runs[0].Validation undefined`); `TestValidationJSON` fails on the `policy` body lacking `"validation":{`; `TestPlanRefusesClientSuppliedPins` passes already (strict decoding, `json:"-"`; kept as the guard); the `cmd/server` test fails with `RunValidations started with done "", awaited ""`.

- [ ] **Step 3: Join the validation into policy runs**

In `internal/store/policies.go`, add to `PolicyRun` after `CorrelationID`:

```go
	// Validation is the run's deployment's health validation, rollback included; nil without one.
	Validation *Validation `json:"validation,omitempty"`
```

replace `policyRunColumns` with

```go
const policyRunColumns = `r.id,r.policy_id,r.occurrence,r.started_at,r.finished_at,r.outcome,r.deployment_id,r.detail,r.correlation_id,` + validationColumns
```

and in `policyRuns` replace the query and the scan:

```go
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT `+policyRunColumns+` FROM policy_runs r LEFT JOIN deployment_validations v ON v.deployment_id=r.deployment_id LEFT JOIN deployments rd ON rd.id=v.rollback_deployment_id WHERE r.policy_id=? ORDER BY r.occurrence DESC,r.id DESC LIMIT ?`), policy, limit)
```

```go
		var r PolicyRun
		var finished sql.NullTime
		var deployment sql.NullString
		var vs validationScan
		if err := rows.Scan(append([]any{&r.ID, &r.PolicyID, &r.Occurrence, &r.StartedAt, &finished, &r.Outcome, &deployment, &r.Detail, &r.CorrelationID}, vs.dest()...)...); err != nil {
			return nil, err
		}
		r.Validation = vs.validation()
```

- [ ] **Step 4: Start and wait for the loop**

In `cmd/server/main.go` replace

```go
	policiesDone := make(chan struct{})
	go srv.RunPolicies(ctx, policiesDone)
```

with

```go
	policiesDone := make(chan struct{})
	go srv.RunPolicies(ctx, policiesDone)
	validationsDone := make(chan struct{})
	go srv.RunValidations(ctx, validationsDone)
```

and replace

```go
	// A policy run in flight finishes before the store closes: its apply is recorded before its
	// frame leaves. A run's worst case (under 3 minutes) fits the same budget.
	waitForBackupWork(waitCtx, backupDone, func() { <-localDone; <-policiesDone; srv.WaitDetached() })
```

with

```go
	// A policy run and a validation tick in flight finish before the store closes: each records
	// its apply before the frame leaves. A run's worst case (under 3 minutes) and a tick's (its
	// rollback's inspections, plan and apply: seconds) fit the same budget.
	waitForBackupWork(waitCtx, backupDone, func() { <-localDone; <-policiesDone; <-validationsDone; srv.WaitDetached() })
```

- [ ] **Step 5: Run to verify they pass, on both drivers**

Run: `gofmt -w internal/store internal/api cmd/server && go vet ./... && go test -race -count=1 ./internal/store/ ./internal/api/ ./cmd/server/ && PG=… go test -count=1 ./internal/store/ ./internal/api/`
Expected: PASS, including `TestComposeGracePeriodCoversTheShutdownBudget` (budget unchanged) and the existing `TestUpdatePolicyRoutes` and `TestListPolicyRuns`.

- [ ] **Step 6: DOX and commit**

Root `AGENTS.md`, the `cmd/server` scheduler paragraph: after the sentence ending "`runServer` waits on it inside the same handler wait (a run's worst case is under 3 minutes, so the budget is unchanged)." add "`api.Server.RunValidations`, the health-validation loop, starts beside it and closes its `done` only between ticks, a rollback in flight included (under `context.WithoutCancel`); `runServer` waits on it in the same handler wait (`TestServerRunsAndAwaitsTheValidationLoop`)."

`internal/store/AGENTS.md`, the update-policies bullet: append "A run carries its deployment's validation (`PolicyRun.Validation`, joined on `deployment_id`)."

```bash
git add internal/store internal/api cmd/server AGENTS.md
git commit -m "feat(server): run the validation loop; runs and deployments carry their validation" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Web — verdicts, the rollback line, validation pauses and the capability warning

**Files:**
- Modify: `web/src/tenant.ts` (`Validation`, `ValidationRollback`; `PolicyRun.validation`)
- Create: `web/src/components/ApplicationValidation.tsx`
- Test: `web/src/components/ApplicationValidation.test.tsx`
- Modify: `web/src/components/ApplicationPolicy.tsx` (Validation column, validation pause text, `HealthWarning`, props `org`/`endpointID`)
- Test: `web/src/components/ApplicationPolicy.test.tsx`
- Modify: `web/src/components/ApplicationDeploymentPlan.tsx` (`Deployment.validation`; `ResultSection` shows it)
- Test: `web/src/components/ApplicationDeploymentPlan.test.tsx`
- Modify: `web/src/components/Applications.tsx` (pass `org` and the instance's `endpoint_id` to the policy card)
- Build: `web/dist` via `make build-web`
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes: Task 5's JSON (`validation` on deployments and runs; `rollback.revision`); `planBlockers` from `ApplicationUpdates.tsx`; `useTenantResource`, `Endpoint` from `../tenant`.
- Produces: `VALIDATION_VERDICTS`, `ROLLBACK_REASONS`, `verdictText`, `rollbackText`, `reasonText`, `pauseText`, `ValidationLine({ v })`; `ApplicationPolicy({ base, admin, org?, endpointID? })`.

- [ ] **Step 1: Write the failing tests**

Create `web/src/components/ApplicationValidation.test.tsx`:

```tsx
import { afterEach, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { ROLLBACK_REASONS, VALIDATION_VERDICTS, ValidationLine, pauseText, reasonText } from './ApplicationValidation';
import type { Validation } from '../tenant';
afterEach(cleanup);

const validation = (over: Partial<Validation> = {}): Validation => ({ deployment_id: 'd', policy_run_id: 'r', automated: true, is_rollback: false, phase: 'done', started_at: '2026-09-25T10:00:00Z', observe_until: '2026-09-25T10:02:30Z', verdict: 'healthy', detail: '', rollback: null, correlation_id: 'c', finished_at: '2026-09-25T10:02:31Z', ...over });
const line = (over: Partial<Validation>) => {
  cleanup();
  return render(<ValidationLine v={validation(over)} />).container.textContent ?? '';
};

it('renders every verdict from the fixed table', () => {
  for (const [verdict, text] of Object.entries(VALIDATION_VERDICTS)) expect(line({ verdict })).toContain(text);
  expect(line({ verdict: 'exploded' })).toContain('Unrecognised verdict.');
});

it('names the deciding service and the loop sentences, and nothing else the server sent', () => {
  expect(line({ verdict: 'unhealthy', detail: 'web' })).toContain('Service web.');
  expect(line({ verdict: 'unverifiable', detail: 'the host could not be observed' })).toContain('The host could not be observed.');
  expect(line({ verdict: 'changed', detail: 'the application was released' })).toContain('The application was released.');
  expect(line({ verdict: 'unverifiable', detail: 'secret-canary <b>' })).not.toContain('secret-canary');
});

it('shows the rollback revision and deployment prefix, or why there was none', () => {
  const id = '3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b';
  render(<ValidationLine v={validation({ verdict: 'unhealthy', detail: 'web', rollback: { deployment_id: id, revision: 4, outcome: 'applied', detail: '' } })} />);
  expect(screen.getByText(/Rolled back to revision 4\./)).toBeTruthy();
  expect(screen.getByText('3f2b1c9e').getAttribute('title')).toBe(id);
  for (const [code, text] of Object.entries(ROLLBACK_REASONS)) expect(line({ verdict: 'exited', rollback: { deployment_id: '', revision: 0, outcome: 'ineligible', detail: code } })).toContain(text);
  expect(line({ verdict: 'exited', rollback: { deployment_id: '', revision: 0, outcome: 'failed', detail: 'image_not_reported,made_up' } })).not.toContain('made_up');
  expect(reasonText('made_up')).toBe('The reason was not recognised.');
});

it('explains each validation pause and nothing else', () => {
  expect(pauseText('rolled back after a failed update')).toContain('was rolled back');
  expect(pauseText('update failed and could not be rolled back: prior_images_missing')).toContain(ROLLBACK_REASONS.prior_images_missing);
  expect(pauseText('update could not be validated: the host could not be observed')).toContain('The host could not be observed.');
  expect(pauseText('update could not be validated: secret-canary')).not.toContain('secret-canary');
  expect(pauseText('three consecutive windows failed')).toBe('');
});
```

Append to `web/src/components/ApplicationPolicy.test.tsx`:

```tsx
const endpoint = (capabilities: string[]) => ({ id: 'host', environment_id: 'env', name: 'Docker', runtime: 'docker', state: 'active', facts: {}, fingerprint: 'f', capabilities, alerts: [], created_at: '2026-09-24T10:00:00Z' });
function stubWithEndpoint(p: unknown, capabilities: string[]) {
  const fetcher = vi.fn(async (url: string) => String(url).includes('/endpoints/') ? json(endpoint(capabilities)) : json(p));
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}
const isEndpoint = (c: unknown[]) => c[0] === '/api/organizations/a/endpoints/host';

it('warns that a host without health reporting cannot validate automated updates', async () => {
  const fetcher = stubWithEndpoint(policy(), ['container.inspect', 'container.inspect.verdict']);
  render(<ApplicationPolicy base="/app" admin={false} org="a" endpointID="host" />);
  open();
  await screen.findByText(/cannot report container health/);
  expect(fetcher.mock.calls.some(isEndpoint)).toBe(true);
});

it('does not warn for a host that reports health, and asks nothing for a plan-only policy', async () => {
  const fetcher = stubWithEndpoint(policy(), ['container.inspect', 'container.inspect.health']);
  render(<ApplicationPolicy base="/app" admin={false} org="a" endpointID="host" />);
  open();
  await vi.waitFor(() => expect(fetcher.mock.calls.some(isEndpoint)).toBe(true));
  await screen.findByText(/Plan and apply ·/);
  expect(screen.queryByText(/cannot report container health/)).toBeNull();
  cleanup();
  const planOnly = stubWithEndpoint(policy({ mode: 'plan_only' }), ['container.inspect']);
  render(<ApplicationPolicy base="/app" admin={false} org="a" endpointID="host" />);
  open();
  await screen.findByText(/Plan only; apply by hand ·/);
  expect(planOnly.mock.calls.some(isEndpoint)).toBe(false);
});

it('shows each run validation and its rollback, and explains a validation pause', async () => {
  const rolledBack = '3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b';
  const v = (over: Record<string, unknown>) => ({ deployment_id: 'd', policy_run_id: 'r', automated: true, is_rollback: false, phase: 'done', started_at: '2026-09-24T10:00:00Z', observe_until: '2026-09-24T10:02:30Z', verdict: 'unhealthy', detail: 'web', rollback: null, correlation_id: 'c', finished_at: '2026-09-24T10:00:31Z', ...over });
  stub(() => json(policy({ status: 'paused', next_occurrence: null, paused_reason: 'update failed and could not be rolled back: prior_images_missing', runs: [
    run({ id: 'a', validation: v({ rollback: { deployment_id: rolledBack, revision: 3, outcome: 'applied', detail: '' } }) }),
    run({ id: 'b', validation: v({ verdict: 'exited', detail: 'db', rollback: { deployment_id: '', revision: 0, outcome: 'ineligible', detail: 'prior_images_missing' } }) }),
    run({ id: 'c', outcome: 'no_update' }),
  ] })));
  render(<ApplicationPolicy base="/app" admin={false} />);
  open();
  await screen.findByText(/Rolled back to revision 3\./);
  expect(screen.getByText('3f2b1c9e').getAttribute('title')).toBe(rolledBack);
  expect(screen.getAllByText(/The earlier images are no longer on the host\./).length).toBe(2);
  expect(screen.getByText(/failed validation and was not rolled back/)).toBeTruthy();
});
```

Append to `web/src/components/ApplicationDeploymentPlan.test.tsx`:

```tsx
it('shows the validation verdict and the rollback of a settled deployment', async () => {
  const rolledBack = '3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b';
  const validated = { ...settled, validation: { deployment_id: 'd1', policy_run_id: 'r', automated: true, is_rollback: false, phase: 'done', started_at: '2026-09-22T12:01:00Z', observe_until: '2026-09-22T12:03:30Z', verdict: 'unhealthy', detail: 'web', rollback: { deployment_id: rolledBack, revision: 1, outcome: 'applied', detail: '' }, correlation_id: 'c', finished_at: '2026-09-22T12:01:31Z' } };
  vi.stubGlobal('fetch', stubFetch([validated]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect((await screen.findAllByText(/Unhealthy: a healthcheck failed or never passed\./)).length).toBeGreaterThan(0);
  expect(screen.getAllByText(/Rolled back to revision 1\./).length).toBeGreaterThan(0);
});
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd web && npx vitest run src/components/ApplicationValidation.test.tsx src/components/ApplicationPolicy.test.tsx src/components/ApplicationDeploymentPlan.test.tsx`
Expected: FAIL: `ApplicationValidation` cannot be resolved; the new policy and plan cases find no validation text.

- [ ] **Step 3: Implement**

In `web/src/tenant.ts`, before `export interface PolicyRun`, add:

```ts
export interface ValidationRollback { deployment_id: string; revision: number; outcome: string; detail: string }
// A deployment's health validation (docs/application-schema.md, Health validation).
export interface Validation { deployment_id: string; policy_run_id: string; automated: boolean; is_rollback: boolean; phase: string; started_at: string; observe_until: string; verdict: string; detail: string; rollback: ValidationRollback | null; correlation_id: string; finished_at: string | null }
```

and add `validation?: Validation` as the last field of `PolicyRun`.

Create `web/src/components/ApplicationValidation.tsx`:

```tsx
import type { Validation } from '../tenant';
import { planBlockers } from './ApplicationUpdates';

// Verdicts (docs/application-schema.md, Health validation); '' is a validation still running.
export const VALIDATION_VERDICTS: Record<string, string> = {
  '': 'Validating: watching the containers after the deployment.',
  healthy: 'Healthy: every service kept running and passed its healthcheck.',
  unhealthy: 'Unhealthy: a healthcheck failed or never passed.',
  exited: 'Exited: a container stopped or is gone.',
  restarting: 'Restarting: a container restarted.',
  unverifiable: 'Not validated.',
  changed: 'Changed: the containers were replaced after this deployment.',
};
// The loop's own sentences, as an unverifiable or changed detail or a pause reason carries them.
const VALIDATION_DETAILS: Record<string, string> = {
  'the host could not be observed': 'The host could not be observed.',
  'the server was not running during the window': 'The server was not running during the window.',
  'the agent cannot report container health': "The host's agent cannot report container health; upgrade it.",
  'an inspection failed validation': "An inspection answer from the host's agent failed validation.",
  'the application was released': 'The application was released.',
};
// Why an automated update was not rolled back: eligibility first, then what stopped an attempt.
export const ROLLBACK_REASONS: Record<string, string> = {
  no_prior_revision: 'No earlier successful deployment of this instance is recorded.',
  prior_definition_invalid: 'The earlier revision no longer validates.',
  service_set_changed: "The application's services changed since the earlier revision.",
  prior_images_missing: 'The earlier images are no longer on the host.',
  rollback_in_flight: 'Another rollback was still in progress.',
  already_rolled_back: 'This deployment was already rolled back.',
  creator_lost: 'The administrator who last saved the policy can no longer deploy.',
  policy_changed: 'The policy changed while the rollback was prepared.',
  not_sent: 'The host disconnected before the rollback was sent.',
  endpoint_offline: 'The host was not connected.',
  interrupted: 'The server restarted during the rollback.',
  not_adopted: 'The application is no longer adopted.',
  mapping_required: 'The services are not mapped.',
  adoption_changed: 'Adoption or inventory changed during the rollback.',
  deployment_in_progress: 'Another deployment was being applied.',
  invalid: 'The rollback was refused as invalid.',
  error: 'The rollback failed on the server; check the server log.',
};
const SERVICE = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;
const DEPLOYMENT = /^[0-9a-f-]{36}$/;
const fixed = (table: Record<string, string>, key: string) => Object.hasOwn(table, key) ? table[key] : '';

// verdictText is the verdict and why, through the fixed tables and the service-name shape only.
export function verdictText(v: Validation): string {
  const head = fixed(VALIDATION_VERDICTS, v.verdict) || 'Unrecognised verdict.';
  const why = fixed(VALIDATION_DETAILS, v.detail) || (SERVICE.test(v.detail) ? `Service ${v.detail}.` : '');
  return why ? `${head} ${why}` : head;
}
// reasonText renders comma-separated codes; unknown codes are dropped.
export function reasonText(codes: string): string {
  const known = codes.split(',').map((c) => fixed(ROLLBACK_REASONS, c) || fixed(planBlockers, c)).filter(Boolean);
  return known.length ? known.join(' ') : 'The reason was not recognised.';
}
export function rollbackText(v: Validation): string {
  const r = v.rollback;
  if (!r) return '';
  if (r.outcome === 'applied') return `Rolled back to revision ${r.revision}.`;
  if (r.outcome === 'ineligible' || r.outcome === 'failed') return `Not rolled back. ${reasonText(r.detail)}`;
  return 'Rolling back.';
}
const ROLLED_BACK = 'rolled back after a failed update';
const NOT_ROLLED_BACK = 'update failed and could not be rolled back: ';
const UNVERIFIED = 'update could not be validated: ';
// pauseText explains a pause the validation loop caused; '' for any other reason.
export function pauseText(reason: string): string {
  if (reason === ROLLED_BACK) return 'An automated update failed validation and was rolled back. Check the application, then resume.';
  if (reason.startsWith(NOT_ROLLED_BACK)) return `An automated update failed validation and was not rolled back. ${reasonText(reason.slice(NOT_ROLLED_BACK.length))} Fix the application, then resume.`;
  if (reason.startsWith(UNVERIFIED)) {
    const why = fixed(VALIDATION_DETAILS, reason.slice(UNVERIFIED.length));
    return `An automated update could not be validated.${why ? ` ${why}` : ''} Check the application, then resume.`;
  }
  return '';
}
// ValidationLine is one validation: verdict, rollback and the rollback deployment's ID prefix.
export function ValidationLine({ v }: { v: Validation }) {
  const id = v.rollback?.outcome === 'applied' ? v.rollback.deployment_id : '';
  return <span>{verdictText(v)}{v.rollback && <> {rollbackText(v)}</>}{DEPLOYMENT.test(id) && <> Deployment <code title={id}>{id.slice(0, 8)}</code>.</>}</span>;
}
```

In `web/src/components/ApplicationPolicy.tsx`:

- replace the import line `import { useTenantResource, type PolicyRun, type UpdatePolicy } from '../tenant';` with `import { useTenantResource, type Endpoint, type PolicyRun, type UpdatePolicy } from '../tenant';` and add `import { ValidationLine, pauseText } from './ApplicationValidation';`
- replace `type Props = { base: string; admin: boolean };` with

```tsx
// org and endpointID, when known, let the card check the host for container.inspect.health.
type Props = { base: string; admin: boolean; org?: string; endpointID?: string };
```

- replace `function PolicyView({ base, admin }: Props) {` with `function PolicyView({ base, admin, org, endpointID }: Props) {`, and directly after the `PolicyStatus`/`No update policy.` line insert

```tsx
    {p?.mode === 'apply' && org && endpointID && <HealthWarning org={org} endpointID={endpointID} />}
```

- in `PolicyStatus` replace `<p role="status">Paused. {fixed(SENTENCES, policy.paused_reason)}</p>` with `<p role="status">Paused. {fixed(SENTENCES, policy.paused_reason) || pauseText(policy.paused_reason)}</p>`
- in `RunList` replace the header row with `<thead><tr><th>Window</th><th>Outcome</th><th>Detail</th><th>Deployment</th><th>Validation</th></tr></thead>` and add as the last cell of each row `<td>{r.validation ? <ValidationLine v={r.validation} /> : '—'}</td>`
- append:

```tsx
// HealthWarning names a host whose agent cannot report container health: every automated update
// there ends unverifiable and pauses the policy.
function HealthWarning({ org, endpointID }: { org: string; endpointID: string }) {
  const endpoint = useTenantResource<Endpoint>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpointID)}`);
  if (endpoint.state !== 'ready' || !endpoint.data || endpoint.data.capabilities.includes('container.inspect.health')) return null;
  return <p role="alert">This host's agent cannot report container health, so automated updates here cannot be validated and pause the policy after each one. Upgrade the agent.</p>;
}
```

In `web/src/components/ApplicationDeploymentPlan.tsx`: replace `import { useTenantResource } from '../tenant';` with `import { useTenantResource, type Validation } from '../tenant';` and add `import { ValidationLine } from './ApplicationValidation';`; add `validation?: Validation` as the last field of the `Deployment` type; in `ResultSection`, after `{explanation && <p role="alert">{explanation}</p>}` insert `{current.validation && <p><ValidationLine v={current.validation} /></p>}`.

In `web/src/components/Applications.tsx`, replace

```tsx
        {instances.state === 'ready' && <ApplicationPolicy key={`policy/${draft.id}`} base={`${base}/${encodeURIComponent(draft.id)}`} admin={policyAdmin} />}</>}
```

with

```tsx
        {instances.state === 'ready' && <ApplicationPolicy key={`policy/${draft.id}`} base={`${base}/${encodeURIComponent(draft.id)}`} admin={policyAdmin} org={org} endpointID={instances.data?.find((i) => i.application_id === draft.id)?.endpoint_id} />}</>}
```

- [ ] **Step 4: Run the web suite and rebuild**

Run: `(cd web && npx vitest run) && make build-web && git status --short web/dist`
Expected: every test passes (the existing `ApplicationPolicy` cases pass no `org`, so no endpoint request is made), the typecheck in `npm run build` passes, and `web/dist` shows the rebuilt bundle.

- [ ] **Step 5: DOX and commit**

`web/AGENTS.md`, Local Contracts, the Update policy bullet: append "Each run shows its deployment's validation (`ApplicationValidation.tsx`: `VALIDATION_VERDICTS`, the failing service's name when it has the service shape, the loop's sentences, \"Rolled back to revision N\" with the rollback deployment's first 8 characters in `<code title>`, or \"Not rolled back\" with `ROLLBACK_REASONS`/`planBlockers` text; unknown codes dropped). A pause the validation loop caused is explained by `pauseText`. With the adopted instance's endpoint known and the policy in `apply` mode, the card reads `GET /api/organizations/{org}/endpoints/{endpoint}` and warns when `container.inspect.health` is missing. The deployment plan's result shows the same `ValidationLine`." In `## Verification` extend the parenthesis with "; `src/components/ApplicationValidation.test.tsx` covers every verdict, the service and sentence details, the rollback line and every reason, and the validation pause texts".

```bash
git add web
git commit -m "feat(web): validation verdicts, rollback line and the health capability warning" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Documents, the DOX pass and the full gate

**Files:**
- Modify: `docs/application-schema.md` (new Health validation section; Rollback honesty; Tables)
- Modify: `docs/agent-protocol.md` (Container inspection: the health fields and capability)
- Modify: `docs/threat-model.md` (one threat row)
- Modify: `README.md` (Update policies section)
- Modify: `KyYard-Implementation-Plan.md` §8
- Docs: every `AGENTS.md` touched in Tasks 1–6 re-read against the code

**Interfaces:**
- Consumes: everything above. Produces: no code.

- [ ] **Step 1: `docs/application-schema.md`**

In `## Tables` add `deployment_validations` after `policy_runs`. Replace the third bullet of `## Rollback honesty` ("M7b automated rollback (health-based) only targets a recorded available revision and reports the same limits.") with:

```markdown
- M7b automated rollback (Health validation, below) re-applies the prior revision pinned to the image IDs its last succeeded apply recorded, only when every one is still on the host and the prior definition still validates; otherwise it stops and says why. It pulls nothing and reverses no data. It never touches a manual apply, and a `changed` deployment (someone replaced the containers) is left alone. After a rollback the host's tag still names the newer image, so the next window would find the update again: the policy stays paused until an administrator resumes it.
```

Insert before `## Kubernetes (M8)`:

```markdown
## Health validation (M7b PR 19, implemented)

- Migration 32 adds `deployment_validations`, one row per succeeded `apply` (primary key and cascade from the deployment; `policy_run_id` set null when the policy and its runs are deleted). `SettleDeployment` opens it in the settling transaction: `grace`, `started_at` the settle time, `observe_until` 30 s + 2 min later, automated when an open or `applied` policy run names the deployment (the run names it before its frame is sent; a `plan_only` run's plan applied by hand is manual), a rollback when a validation named it. Removals, failed applies and plans get none.
- The loop (`api.Server.RunValidations`) polls every 20 s. After the 30 s grace it needs the endpoint's agent to advertise `container.inspect.health` and be connected (either missing then is `unverifiable`), inspects each settled container as `system-validation` through the plan-time inspection (counting against the endpoint's two admissions), records the restart counts as the baseline and judges every poll. A settled container the store knows to be rebound (a later deployment, a release) or recreated under its name (the latest inventory received since the settle) is `changed`; one absent from that inventory is `exited`.
- Verdicts: `healthy` (a complete poll at or after `observe_until` finds every container the settled identity, running, `health` `healthy` or `none`, restart count at the baseline), `unhealthy` (`health` `unhealthy`, or still `starting` at the end), `exited` (not running or restarting, or gone), `restarting` (restarting, or restarted since the baseline), `changed`, `unverifiable` (`the agent cannot report container health`, `the host could not be observed`, `an inspection failed validation`, `the server was not running during the window`). A failing poll ends the window at once; several failures report the strongest (`changed`, `exited`, `restarting`, `unhealthy`) with the deciding service as the detail. Without a complete observation by `observe_until` + 30 s the verdict is `unverifiable`. Each verdict is audited `application.validation` as `system` on `<app>/deployments/<id>` under the deployment's correlation ID.
- Rollback, for an automated update that is not itself a rollback and fails with `unhealthy`, `exited` or `restarting`: `RollbackTarget` decides as the policy's creator (`application.deploy` re-checked) and refuses with `already_rolled_back`, `rollback_in_flight`, `no_prior_revision` (the newest succeeded apply of the instance at `previous_revision` other than this one; a policy update keeps its revision, so this is usually an older apply of the same revision, and the first update after adoption has none), `prior_definition_invalid`, `service_set_changed` or `prior_images_missing` (listed in the latest inventory or run by a running container). Eligible, the loop plans with every service pinned to its prior image ID (no tag, no registry, no pull), names the rollback on the validation, applies it with the policy re-checked and sends it; a rollback that cannot be sent is `failed` (`not_sent`, `endpoint_offline`, `policy_changed`, `creator_lost`, a plan blocker, or a scheduler code). A rollback's own verdict never triggers another.
- After the decision the policy pauses (`rolled back after a failed update`, or `update failed and could not be rolled back: <detail>`); `unverifiable` pauses with `update could not be validated: <detail>` and does not roll back. Resume is the acknowledgement. The run keeps `applied`. A manual apply only gets its verdict. A restart settles a grace row whose window passed and marks a rollback named but undecided `failed` `interrupted`; nothing is dispatched twice. `Prune` keeps an instance's history while its automated update may still roll back.
- `GET .../deployments`, `GET .../deployments/{deployment}` and the policy's runs carry `validation` (the row without its baseline, `rollback: {deployment_id, revision, outcome, detail}` once decided).
```

- [ ] **Step 2: `docs/agent-protocol.md`**

In `## Container inspection`, at the end of the paragraph beginning "`inspection.result` carries request ID", add: "Agents built from M7b PR 19 also advertise `container.inspect.health`: their answers carry `health` (`none` without a healthcheck, else Docker's `State.Health.Status`: `starting`, `healthy`, `unhealthy`) and `restart_count` (Docker's `RestartCount`, 0..1,000,000); the healthcheck command and log are never read. Both sides validate the pair exactly when the answering agent advertised the capability, so an older agent answers without it and a health agent must answer with it. The server's health validation needs the capability; without it every automated update on that host is `unverifiable`."

- [ ] **Step 3: `docs/threat-model.md`**

Add a row to the threats table after "Unattended deployment outliving its author's authority":

```markdown
| Automated rollback acting on a false or stale signal | Implemented (M7b PR 19): only an update a policy applied is ever rolled back, never a manual apply; a deployment someone replaced since (`changed`) or one that could not be observed (`unverifiable`) is not rolled back; the rollback re-checks `application.deploy` for the policy's creator and the policy itself at apply, pins the prior apply's recorded image IDs (no tag resolution, no registry, no pull), requires them on the host and the prior definition valid, and a rollback's own validation never triggers another; every outcome pauses the policy until an administrator resumes it. Health comes only from allowlisted, bounded fields (`health`, `restart_count`); healthcheck commands and logs never leave the agent. Residual: `healthy` means the containers ran and passed their own healthchecks for two minutes, not that the application works; data changes are never reversed | `TestValidationRollsBackAnUnhealthyUpdate`, `TestValidationOfAManualApplyOnlyRecords`, `TestValidationOfAnUpdateReplacedByAManualApply`, `TestValidationNeverRollsBackARollback`, `TestValidationNeverDispatchesARollbackTwice`, `TestRollbackTargetIneligibility`, `TestRollbackTargetReauthorizesTheCreator`, `TestPlanDeploymentPinsImages`, `TestPlanRefusesClientSuppliedPins`, `TestInspectionHealthFields`, `TestInspectionReportsHealthAndRestarts` |
```

- [ ] **Step 4: `README.md`**

In `## Update policies`, after the paragraph ending "is in the audit log under one correlation ID.", add:

```markdown
After every deployment the server watches the containers for two and a half minutes: each must
keep running, pass its own healthcheck if the image has one, and not restart. The verdict is
shown on the deployment and on the policy's run list. When an update the policy applied fails,
the server returns the application to the previous deployment's exact images if they are still
on the host, or says why it cannot; either way the policy pauses until you resume it. Updates you
apply by hand are judged the same way but never rolled back automatically. This needs the
host's agent to report container health: the policy card warns when it cannot.
```

- [ ] **Step 5: `KyYard-Implementation-Plan.md` §8**

Replace the line "Next: M7b PR 19 (health validation and eligible rollback)." with:

```markdown
Implemented M7b PR 19 (`feat/health-validation`): every succeeded apply is validated for 30 s grace plus a 2-minute window through `container.inspect.health` inspections (health and restart count), with a recorded verdict; an update a policy applied that fails is rolled back to the prior apply's pinned images when they are on the host and the prior definition validates, or stops with the reason, and the policy pauses either way until resumed; manual applies are judged, never rolled back. Spec `docs/superpowers/specs/2026-09-25-health-validation-design.md`. M7b is complete.

Next: M8 (Kubernetes and migration).
```

- [ ] **Step 6: DOX closeout**

Re-read every `AGENTS.md` on the paths changed in Tasks 1–6 (`internal/agent`, `internal/runtime`, `internal/store`, `internal/api`, `web`, root for `cmd/server` and Verification) against the code as merged: each bullet names the capability, the fields and bounds, the table, the verdicts and their precedence, the sentences, the rollback reasons and failure codes, the pause reasons and the hooks exactly as implemented. `grep -rn "system:validation" --include=*.md --include=*.go . | grep -v docs/superpowers` prints nothing. No `AGENTS.md` was created, moved or removed, so every Child DOX Index is unchanged; root Verification already runs everything added here (`make ci` covers the Go and web tests; `make test-postgres` the PostgreSQL store and API suites; CI's real-Docker step runs `TestInspectionRealDocker`, which now covers health).

- [ ] **Step 7: Full gate**

Run: `gofmt -l internal cmd && make ci && PG=… make test-postgres`
Expected: `gofmt -l` prints nothing; `make ci` (tidy-check, lint, race tests, web tests, smoke) and the PostgreSQL suite pass.

- [ ] **Step 8: Commit**

```bash
git add docs README.md KyYard-Implementation-Plan.md AGENTS.md internal web
git commit -m "docs: health validation and eligible rollback" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```
