# Bookkeeping (PR D2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every deployment outcome a closed code with a validated parameter, tie a deployment's audit rows together under the plan's correlation ID, put update plans under the per-application guard, and close three review carry-overs (case-insensitive unique usernames, the first admin's row lock, the two-character volume-name minimum).

**Architecture:** The protocol gains `Code` on steps and results, the closed code sets with per-code detail rules, and `RequestID` on both requests and the result (Task 1). The Docker adapter emits a code and parameter on every refusal (Task 2); the agent deployer answers its own refusals with result codes and echoes and logs the request ID (Task 3). The store records the plan's correlation ID on the deployment row (migration 29), reuses it for every audit row and the frame, refuses a result carrying another request ID, reads a code-less result from an older agent as `legacy`, and drops the free result detail from the wire (Task 4). Migration 30 adds the lowercase username index, `CreateOrganizationWithAdmin` locks the admin row, and volume names need two characters (Task 5). The API guards update plans with the check's in-flight key and SSO auto-provision refuses a case variant (Task 6). The web renders from code tables and shows the correlation ID (Task 7); documents close (Task 8).

**Tech Stack:** Go, Docker Engine API v1.41 (fake Engine in tests), coder/websocket agent protocol, SQLite + PostgreSQL 17, React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-24-bookkeeping-design.md`

## Global Constraints

- Step codes, verbatim from the spec (`protocol.DeploymentStep.Code`, `json:"code"`; `Detail` stays and is the code's parameter, never a sentence):

  | Code | Emitted when | Detail |
  |---|---|---|
  | `runtime_unreadable` | `GET /info` gave no default runtime | none |
  | `container_missing` | the old container is gone (precondition, recheck, removal) | none |
  | `identity_mismatch` | container ID, image ID or creation time differ from `Replaces` | none |
  | `image_identity_mismatch` | the host reported a different image identity than the plan pinned | none |
  | `configuration_unreported` | the daemon omitted `Config`/`HostConfig`/`Mounts` | none |
  | `unsupported` | `undescribed` returned codes | the codes, joined by `,` (each from `UnsupportedCodes`) |
  | `bind_missing` | a bind in the frame is not on the old container | none |
  | `volume_mount_missing` | a per-service kept volume is not on the old container | none |
  | `volume_not_owned` | an existing volume fails the ownership rule | none |
  | `volume_missing` | an external volume does not exist | none |
  | `volume_create_failed` | `POST /volumes/create` failed | none |
  | `image_missing` | the container's image is no longer present | none |
  | `pinned_image_missing` | the pinned image is not on the host after the pull step | none |
  | `configuration_drift` | the recheck found identity or configuration changed | none |
  | `name_reserved` | a container already holds the name reserved for the previous one | none |
  | `name_taken` | a container with the service's name already exists | none |
  | `identity_unusable` | the runtime returned an unusable container identity | none |
  | `identity_unreadable` | the container started but its identity could not be read | the 64-hex container ID |
  | `identity_unverified` | the container started but its identity could not be verified | the 64-hex container ID |
  | `dependents` | removal refused because something depends on the container | none |
  | `deadline` | not enough time left to pull, replace or remove safely | none |
  | `pull_failed` | the pull stream reported an error | none |
  | `pull_unauthorized` | 401/403 from the registry | none |
  | `pull_not_found` | 404 from the registry | none |
  | `pull_digest_mismatch` | the pulled image's digest differs from the pinned one | none |
  | `cancelled` | the run's context ended before the runtime answered | none |
  | `runtime_timeout` | the per-call budget elapsed | none |
  | `runtime_error` | the call failed without a status | none |
  | `runtime_status` | the daemon answered an unexpected status | the 3-digit status |

- Per-code detail rules, verbatim: "`unsupported` requires one to thirty-two distinct `UnsupportedCodes` entries; `identity_unreadable`/`identity_unverified` require a full Docker ID; `runtime_status` requires `[1-5][0-9]{2}`; every other code requires an empty detail. `MaxDeploymentStepDetailBytes` stays the outer bound. A step with outcome `succeeded` or `skipped` carries no code and no detail. A `denied` or `failed` step must carry a code."
- Result codes, verbatim from the spec (`protocol.DeploymentResult.Code`, `json:"code"`):

  | Code | Emitted when |
  |---|---|
  | `step_failed` | a step did not succeed; the steps say which |
  | `clock_skew` | `issued_at` is more than `MaxClockSkew` from the agent's clock |
  | `invalid_request` | the frame failed `Validate` |
  | `wrong_endpoint` | the frame names another endpoint |
  | `busy` | the agent is already applying a deployment |
  | `restarted` | the agent restarted after replacement began |
  | `unreadable` | the runtime returned a result the agent could not validate |

- "A result with outcome `succeeded` carries no code. Any other outcome carries one; the free `Detail` on the result is removed from the wire (`json:"-"` is not enough: the field goes). `storedDeploymentResult` keeps codes, not sentences."
- `legacy`: "The server normalises a missing code on a non-succeeding step or result to `legacy` and drops its detail, so an in-flight deployment applied before the upgrade still settles; `legacy` is in the closed set but no agent emits it. The web shows "the agent did not classify this outcome; upgrade the agent" for it." Plan decision (see Review Focus 5): the marker is the result's `request_id`. A result with an empty `request_id` was produced by a binary built before this change (an old agent, or an old ledger entry a new agent re-sends) and is the only one the server normalises; a result with a `request_id` is taken as sent and a code-less failing step makes it unreadable.
- `request_id`: `DeploymentRequest.RequestID`, `RemovalRequest.RequestID` and `DeploymentResult.RequestID`, all `json:"request_id"`. Required on both requests, grammar `^[A-Za-z0-9_-]{1,64}$` (`protocol.ValidRequestID`; the API's 32 hex characters, the store's UUID fallback). The agent echoes a valid one on every answer and logs it beside the deployment ID at receipt, start and finish.
- Migration 29 adds `deployments.correlation_id TEXT NOT NULL DEFAULT ''`. Migration 30 creates `idx_users_username_lower` as `UNIQUE (LOWER(username))` with one SQL string for both dialects, after refusing with exactly `usernames differ only by case: erin, Erin; rename or delete one of each pair before upgrading` (every group listed, usernames not IDs; within a group in creation order, groups by lowercase name and joined by `; `). Nothing is altered on refusal and the server refuses to start.
- An update plan for an application whose update check or update plan is in flight is 409 `check_in_progress`. Registry slots: 2 per organization, 8 server-wide.
- Volume grammars: declared names `^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}$` (store `applicationVolumeName`, importer message); wire `protocol.deploymentVolume` `^[A-Za-z0-9][A-Za-z0-9_.-]{1,128}$`. `a` is refused and `ab` accepted at all three layers.
- Every store behaviour is tested on SQLite and PostgreSQL. PostgreSQL runs use the local container `kyyard-access-pg`; read the password into a variable, never print it: `PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test ...`. Below this prefix is written `PG=… go test`.
- `web/dist` is rebuilt with `make build-web` and committed in the web task (Task 7).
- `gofmt -w` every edited Go file and check `gofmt -l internal cmd` prints nothing before each commit. Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- The branch ships as one PR. Task 1 makes codes and `request_id` mandatory, so the runtime suite is red until Task 2, the agent-client suite until Task 3, and the store and API suites (whose frames carry no `request_id` yet) until Task 4; each task runs only its own packages' tests. Do not release between them.

## Review Focus

1. An agent result that carries a code with a detail of the wrong shape (`runtime_status` with `abc`, `identity_unverified` with 63 hex, `unsupported` with an unknown or repeated code) must be refused and nothing stored, not settled or shown. Pinned in Task 1 (`TestDeploymentStepCodeDetails`) and Task 4 (`TestSettleRefusesAMalformedCodeParameter`).
2. A result whose `request_id` is another deployment's (a plan re-planned under a new request, the old ID replayed) must be refused, never settle the row. Pinned in Task 4 (`TestSettleRefusesAForeignRequestID`).
3. Migration 30 on a database holding three case variants of one name plus a second pair must list every username exactly once, grouped, and alter nothing; after the operator fixes them it must succeed and close the race. Pinned in Task 5 (`TestMigrationRefusesCaseVariantUsernames`).
4. An SSO sign-in asserting `Erin` while a local `erin` exists must be refused naming the conflict: no second account and, above all, no session as `erin` (a provider's claimed name is never a link to an account it did not create). Pinned in Task 6 (`TestSSOAutoProvisionRefusesACaseVariantUsername`, `TestKySignOnWebhookRefusesACaseVariantUsername`).
5. A step with outcome `denied` and no code from a current agent (the result carries a `request_id`) must be refused as unreadable; only a result with no `request_id` (an older binary) is normalised to `legacy`. Pinned in Task 1 (`TestDeploymentResultCodes`) and Task 4 (`TestSettleRefusesACodelessStepFromACurrentAgent`, `TestSettleReadsALegacyResult`).

---

### Task 1: Protocol — step and result codes, request IDs

**Files:**
- Modify: `internal/agent/protocol/deployment.go` (code sets, `ValidRequestID`, `RequestID` on both requests and the result, `Code` on step and result, `Validate`, required `Mounts`)
- Test: `internal/agent/protocol/deployment_test.go`
- Docs: `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: `knownCodes` (`inspection.go`), `fullDockerID` (`exec.go`).
- Produces:
  ```go
  // package protocol
  const (
  	ResultStepFailed     = "step_failed"
  	ResultClockSkew      = "clock_skew"
  	ResultInvalidRequest = "invalid_request"
  	ResultWrongEndpoint  = "wrong_endpoint"
  	ResultBusy           = "busy"
  	ResultRestarted      = "restarted"
  	ResultUnreadable     = "unreadable"
  	CodeLegacy           = "legacy"
  )
  func ValidRequestID(id string) bool
  // DeploymentRequest.RequestID, RemovalRequest.RequestID, DeploymentResult.RequestID string `json:"request_id"`
  // DeploymentStep.Code, DeploymentResult.Code string `json:"code"`
  ```
  Step codes are string literals checked by the unexported `stepCodes` map. `DeploymentResult.Detail` stays in the struct until Task 4 deletes it; from this task `Validate` refuses a non-empty one. `DeploymentService.Mounts == nil` is now invalid (`invalid mount`): the only sender of a nil list was a server older than mounts, which also sends no `request_id`, so the adapter's `cannotExpress + "mounts"` branch becomes unreachable and Task 2 deletes it.

- [ ] **Step 1: Write the failing tests**

In `internal/agent/protocol/deployment_test.go` replace `goodDeployment` and `goodRemoval`:

```go
func goodDeployment(now time.Time) DeploymentRequest {
	return DeploymentRequest{
		Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", Revision: 2, IssuedAt: now, Deadline: now.Add(5 * time.Minute),
		Services: []DeploymentService{{
			Name: "web", ContainerName: "shop-web-1", ImageID: "sha256:" + strings.Repeat("a", 64),
			Replaces: InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000},
			Restart:  "always", Ports: []Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}}, Env: map[string]string{"TOKEN": "x"}, Mounts: []Mount{},
		}},
	}
}
```

```go
func goodRemoval(now time.Time) RemovalRequest {
	return RemovalRequest{
		Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(5 * time.Minute),
		Containers: []RemovalTarget{
			{Service: "web", Target: InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}},
			{Service: "unmapped-0123456789ab", Target: InspectionTarget{ContainerID: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("d", 64), CreatedUnix: 1700000000}},
		},
	}
}
```

In `TestDeploymentRequestValidation`, add to the map of refused mutations (after `"bad replaces"`):

```go
		"no request id":     func(r *DeploymentRequest) { r.RequestID = "" },
		"bad request id":    func(r *DeploymentRequest) { r.RequestID = "a b" },
		"long request id":   func(r *DeploymentRequest) { r.RequestID = strings.Repeat("a", 65) },
		"mounts key absent": func(r *DeploymentRequest) { r.Services[0].Mounts = nil },
```

In `TestRemovalRequestValidation`, add after `"bad target"`:

```go
		"no request id": func(r *RemovalRequest) { r.RequestID = "" },
```

Replace `TestDeploymentResultValidation` with:

```go
func goodResult() DeploymentResult {
	return DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Outcome: OutcomeSucceeded, Steps: []DeploymentStep{{Service: "web", Step: StepCreate, Outcome: OutcomeSucceeded}}, Services: []DeploymentIdentity{{Service: "web", ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}}}
}

func TestDeploymentResultValidation(t *testing.T) {
	good := goodResult()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	failing := func(r *DeploymentResult, outcome string) {
		r.Outcome, r.Code = outcome, ResultStepFailed
		r.Steps[0].Outcome = outcome
		r.Services = []DeploymentIdentity{}
	}
	for name, mutate := range map[string]func(*DeploymentResult){
		"outcome":          func(r *DeploymentResult) { r.Outcome = "done" },
		"step name":        func(r *DeploymentResult) { r.Steps[0].Step = "fetch" },
		"image digest":     func(r *DeploymentResult) { r.Services[0].ImageDigest = "sha256:" + strings.Repeat("B", 64) },
		"image digest tag": func(r *DeploymentResult) { r.Services[0].ImageDigest = "latest" },
		"step outcome":     func(r *DeploymentResult) { r.Steps[0].Outcome = "ok" },
		"step detail over the bound": func(r *DeploymentResult) {
			failing(r, OutcomeFailed)
			r.Steps[0].Code, r.Steps[0].Detail = "unsupported", strings.Repeat("d", MaxDeploymentStepDetailBytes+1)
		},
		"detail on succeeded step":   func(r *DeploymentResult) { r.Steps[0].Detail = "done" },
		"code on succeeded step":     func(r *DeploymentResult) { r.Steps[0].Code = "runtime_error" },
		"code on skipped step":       func(r *DeploymentResult) { r.Steps[0].Outcome, r.Steps[0].Code = OutcomeSkipped, "runtime_error" },
		"code on succeeded result":   func(r *DeploymentResult) { r.Code = ResultStepFailed },
		"failed result without code": func(r *DeploymentResult) { r.Outcome = OutcomeFailed },
		"unknown result code":        func(r *DeploymentResult) { r.Outcome, r.Code = OutcomeFailed, "exploded" },
		"denied step without code":   func(r *DeploymentResult) { failing(r, OutcomeDenied) },
		"failed step without code":   func(r *DeploymentResult) { failing(r, OutcomeFailed) },
		"sentence as a step code": func(r *DeploymentResult) {
			failing(r, OutcomeDenied)
			r.Steps[0].Code = "the container no longer exists"
		},
		"free result detail": func(r *DeploymentResult) {
			r.Outcome, r.Code, r.Detail = OutcomeFailed, ResultStepFailed, "service web, step create: failed"
		},
		"bad request id": func(r *DeploymentResult) { r.RequestID = "a b" },
		"step service":   func(r *DeploymentResult) { r.Steps[0].Service = "Web" },
		"identity":       func(r *DeploymentResult) { r.Services[0].ImageID = "latest" },
		"too many steps": func(r *DeploymentResult) { r.Steps = make([]DeploymentStep, MaxDeploymentResultSteps+1) },
	} {
		r := good
		r.Steps = append([]DeploymentStep{}, good.Steps...)
		r.Services = append([]DeploymentIdentity{}, good.Services...)
		mutate(&r)
		if r.Validate() == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	for _, step := range []string{StepImage, StepPull} {
		r := good
		r.Steps = []DeploymentStep{{Service: "web", Step: step, Outcome: OutcomeSucceeded}}
		r.Services = []DeploymentIdentity{good.Services[0]}
		r.Services[0].ImageDigest = "sha256:" + strings.Repeat("d", 64)
		if err := r.Validate(); err != nil {
			t.Fatalf("%s step with a pulled identity refused: %v", step, err)
		}
	}
	recheck := good
	recheck.Outcome, recheck.Code = OutcomeDenied, ResultStepFailed
	recheck.Steps = []DeploymentStep{{Service: "web", Step: StepRecheck, Outcome: OutcomeDenied, Code: "configuration_drift"}}
	recheck.Services = []DeploymentIdentity{}
	if err := recheck.Validate(); err != nil {
		t.Fatalf("recheck step refused: %v", err)
	}
	skipped := good
	skipped.Steps = []DeploymentStep{{Service: "web", Step: StepStop, Outcome: OutcomeSkipped}}
	if err := skipped.Validate(); err != nil {
		t.Fatalf("skipped step refused: %v", err)
	}
	// An older agent sends no request_id; a succeeded result needs nothing else.
	older := good
	older.RequestID = ""
	if err := older.Validate(); err != nil {
		t.Fatalf("a result without request_id refused: %v", err)
	}
}

// Every step code takes exactly the parameter its row names; anything else is refused and never
// stored (Review Focus 1).
func TestDeploymentStepCodeDetails(t *testing.T) {
	id := strings.Repeat("c", 64)
	for _, tc := range []struct {
		code, detail string
		ok           bool
	}{
		{"runtime_unreadable", "", true},
		{"container_missing", "", true},
		{"identity_mismatch", "", true},
		{"image_identity_mismatch", "", true},
		{"configuration_unreported", "", true},
		{"unsupported", "privileged", true},
		{"bind_missing", "", true},
		{"volume_mount_missing", "", true},
		{"volume_not_owned", "", true},
		{"volume_missing", "", true},
		{"volume_create_failed", "", true},
		{"image_missing", "", true},
		{"pinned_image_missing", "", true},
		{"configuration_drift", "", true},
		{"name_reserved", "", true},
		{"name_taken", "", true},
		{"identity_unusable", "", true},
		{"identity_unreadable", id, true},
		{"identity_unverified", id, true},
		{"dependents", "", true},
		{"deadline", "", true},
		{"pull_failed", "", true},
		{"pull_unauthorized", "", true},
		{"pull_not_found", "", true},
		{"pull_digest_mismatch", "", true},
		{"cancelled", "", true},
		{"runtime_timeout", "", true},
		{"runtime_error", "", true},
		{"runtime_status", "500", true},
		{CodeLegacy, "", true},
		{"unsupported", "privileged,devices", true},
		{"runtime_status", "404", true},
		{"container_missing", "the container no longer exists", false},
		{"runtime_error", "the runtime call failed", false},
		{"legacy", "old text", false},
		{"unsupported", "", false},
		{"unsupported", "privileged,privileged", false},
		{"unsupported", "privileged, devices", false},
		{"unsupported", "made_up", false},
		{"unsupported", "privileged,", false},
		{"unsupported", strings.Join(UnsupportedCodes, ","), false}, // 311 bytes: past the step bound
		{"identity_unreadable", "", false},
		{"identity_unverified", strings.Repeat("C", 64), false},
		{"identity_unverified", id[:63], false},
		{"runtime_status", "abc", false},
		{"runtime_status", "600", false},
		{"runtime_status", "50", false},
		{"runtime_status", "", false},
		{"", "", false},
	} {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Outcome: OutcomeDenied, Code: ResultStepFailed,
			Steps: []DeploymentStep{{Service: "web", Step: StepPrecondition, Outcome: OutcomeDenied, Code: tc.code, Detail: tc.detail}}, Services: []DeploymentIdentity{}}
		if err := r.Validate(); (err == nil) != tc.ok {
			t.Errorf("code %q detail %q: %v", tc.code, tc.detail, err)
		}
	}
	if len(stepCodes) != 30 || len(resultCodes) != 8 {
		t.Fatalf("closed sets: %d step codes, %d result codes", len(stepCodes), len(resultCodes))
	}
}

// A result that did not succeed names one closed code. legacy is in the set, but reading a result
// as an older agent's is the server's job: the protocol never fills a missing code, with or
// without a request_id (Review Focus 5).
func TestDeploymentResultCodes(t *testing.T) {
	for _, code := range []string{ResultStepFailed, ResultClockSkew, ResultInvalidRequest, ResultWrongEndpoint, ResultBusy, ResultRestarted, ResultUnreadable, CodeLegacy} {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: OutcomeDenied, Code: code, Steps: []DeploymentStep{}, Services: []DeploymentIdentity{}}
		if err := r.Validate(); err != nil {
			t.Fatalf("%s: %v", code, err)
		}
	}
	if ResultStepFailed != "step_failed" || ResultClockSkew != "clock_skew" || ResultInvalidRequest != "invalid_request" || ResultWrongEndpoint != "wrong_endpoint" || ResultBusy != "busy" || ResultRestarted != "restarted" || ResultUnreadable != "unreadable" || CodeLegacy != "legacy" {
		t.Fatal("a result code's spelling changed")
	}
	for _, requestID := range []string{"", "0123456789abcdef0123456789abcdef"} {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: requestID, Outcome: OutcomeDenied, Code: ResultStepFailed,
			Steps: []DeploymentStep{{Service: "web", Step: StepPrecondition, Outcome: OutcomeDenied}}, Services: []DeploymentIdentity{}}
		if r.Validate() == nil {
			t.Fatalf("request_id %q: a denied step without a code validated", requestID)
		}
	}
}

func TestRequestIDs(t *testing.T) {
	for id, ok := range map[string]bool{
		"0123456789abcdef0123456789abcdef":     true,
		"3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b": true,
		"test-request":                         true,
		strings.Repeat("a", 64):                true,
		strings.Repeat("a", 65):                false,
		"":                                     false,
		"a b":                                  false,
		"a\nb":                                 false,
	} {
		if ValidRequestID(id) != ok {
			t.Errorf("%q: want %v", id, ok)
		}
	}
	now := time.Now()
	for _, v := range []any{goodDeployment(now), goodRemoval(now), goodResult()} {
		raw, _ := json.Marshal(v)
		if !strings.Contains(string(raw), `"request_id":"0123456789abcdef0123456789abcdef"`) {
			t.Fatalf("request_id missing on the wire: %s", raw)
		}
	}
}
```

Replace `TestDeploymentResultWorstCaseFitsTheFrame` with:

```go
// The largest result Deploy can produce must fit the frame, or an honest agent could not report.
// Only the step that ended the run carries a parameter, and the longest is as many whole
// unsupported codes as the step bound holds; the frame's every volume adds a step.
func TestDeploymentResultWorstCaseFitsTheFrame(t *testing.T) {
	steps := []string{StepPrecondition, StepPull, StepRecheck, StepRename, StepCreate, StepStop, StepStart, StepRemove}
	detail := UnsupportedCodes[0]
	for _, c := range UnsupportedCodes[1:] {
		if len(detail)+1+len(c) > MaxDeploymentStepDetailBytes {
			break
		}
		detail += "," + c
	}
	build := func(outcome string, stepOutcome func(service, step int) string, identities int) DeploymentResult {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: strings.Repeat("r", 64), Outcome: outcome}
		if outcome != OutcomeSucceeded {
			r.Code = ResultStepFailed
		}
		step := func(name, kind, outcome string) DeploymentStep {
			s := DeploymentStep{Service: name, Step: kind, Outcome: outcome}
			if outcome != OutcomeSucceeded && outcome != OutcomeSkipped {
				s.Code, s.Detail = "unsupported", detail
			}
			return s
		}
		for i := 0; i < MaxDeploymentServices; i++ {
			name := fmt.Sprintf("s%02d", i) + strings.Repeat("x", 60)
			for j, kind := range steps {
				r.Steps = append(r.Steps, step(name, kind, stepOutcome(i, j)))
			}
			if i == 0 {
				for v := 0; v < MaxDeploymentVolumes; v++ {
					r.Steps = append(r.Steps, step(name, StepVolume, stepOutcome(0, 1)))
				}
			}
			if i < identities {
				r.Services = append(r.Services, DeploymentIdentity{Service: name, ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), ImageDigest: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000})
			}
		}
		return r
	}
	for name, r := range map[string]DeploymentResult{
		"all succeeded": build(OutcomeSucceeded, func(int, int) string { return OutcomeSucceeded }, MaxDeploymentServices),
		"failed at last step": build(OutcomeFailed, func(i, j int) string {
			if i == MaxDeploymentServices-1 && j == len(steps)-1 {
				return OutcomeFailed
			}
			return OutcomeSucceeded
		}, MaxDeploymentServices),
		"failed at first step": build(OutcomeTimedOut, func(i, j int) string {
			if i == 0 && j == 0 {
				return OutcomeTimedOut
			}
			return OutcomeSkipped
		}, 0),
	} {
		if err := r.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > MaxDeploymentResultBytes {
			t.Fatalf("%s: %d bytes exceeds %d", name, len(raw), MaxDeploymentResultBytes)
		}
		t.Logf("%s: %d of %d bytes", name, len(raw), MaxDeploymentResultBytes)
	}
}
```

Replace `TestDeploymentResultVolumeStep` with:

```go
func TestDeploymentResultVolumeStep(t *testing.T) {
	r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: OutcomeFailed, Code: ResultStepFailed, Steps: []DeploymentStep{{Service: "web", Step: StepVolume, Outcome: OutcomeFailed, Code: "volume_create_failed"}}}
	if err := r.Validate(); err != nil {
		t.Fatalf("volume step refused: %v", err)
	}
}
```

- [ ] **Step 2: Run the protocol tests to verify they fail**

Run: `go test -count=1 ./internal/agent/protocol/`
Expected: FAIL to compile: `unknown field RequestID`, `unknown field Code`, `undefined: ResultStepFailed`, `undefined: stepCodes`.

- [ ] **Step 3: Implement the protocol**

In `internal/agent/protocol/deployment.go`, add after the `var (...)` block holding `deploymentUUID`:

```go
// Outcome codes: the closed vocabulary a denied or failed step, and a result that did not
// succeed, report in. A step's Detail is its code's parameter, never a sentence.
// See docs/agent-protocol.md, Outcome codes.
const (
	ResultStepFailed     = "step_failed"     // a step did not succeed; the steps say which
	ResultClockSkew      = "clock_skew"      // issued_at is more than MaxClockSkew from the agent's clock
	ResultInvalidRequest = "invalid_request" // the frame failed Validate
	ResultWrongEndpoint  = "wrong_endpoint"  // the frame names another endpoint
	ResultBusy           = "busy"            // the agent is already applying a deployment
	ResultRestarted      = "restarted"       // the agent restarted after replacement began
	ResultUnreadable     = "unreadable"      // the runtime returned a result the agent could not validate
	// CodeLegacy is the server's reading of a result from an agent built before codes, for a
	// step or a result; no agent emits it.
	CodeLegacy = "legacy"
)

type detailRule int

const (
	detailNone        detailRule = iota
	detailUnsupported            // one to MaxUnsupported distinct UnsupportedCodes, joined by ","
	detailContainerID            // a full 64-hex Docker ID
	detailStatus                 // a 3-digit status
)

// stepCodes maps each step code to the parameter its detail carries.
var stepCodes = map[string]detailRule{
	"runtime_unreadable": detailNone, "container_missing": detailNone, "identity_mismatch": detailNone,
	"image_identity_mismatch": detailNone, "configuration_unreported": detailNone, "unsupported": detailUnsupported,
	"bind_missing": detailNone, "volume_mount_missing": detailNone, "volume_not_owned": detailNone,
	"volume_missing": detailNone, "volume_create_failed": detailNone, "image_missing": detailNone,
	"pinned_image_missing": detailNone, "configuration_drift": detailNone, "name_reserved": detailNone,
	"name_taken": detailNone, "identity_unusable": detailNone, "identity_unreadable": detailContainerID,
	"identity_unverified": detailContainerID, "dependents": detailNone, "deadline": detailNone,
	"pull_failed": detailNone, "pull_unauthorized": detailNone, "pull_not_found": detailNone,
	"pull_digest_mismatch": detailNone, "cancelled": detailNone, "runtime_timeout": detailNone,
	"runtime_error": detailNone, "runtime_status": detailStatus, CodeLegacy: detailNone,
}

var resultCodes = map[string]bool{ResultStepFailed: true, ResultClockSkew: true, ResultInvalidRequest: true, ResultWrongEndpoint: true, ResultBusy: true, ResultRestarted: true, ResultUnreadable: true, CodeLegacy: true}

var (
	statusDetail = regexp.MustCompile(`^[1-5][0-9]{2}$`)
	requestID    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// ValidRequestID is the grammar of a frame's request_id, the audit correlation ID: the API's 32
// hex characters or the store's UUID fallback, safe to log.
func ValidRequestID(id string) bool { return requestID.MatchString(id) }

// validStepCode reports a known code whose detail has the shape the code allows.
func validStepCode(code, detail string) bool {
	rule, ok := stepCodes[code]
	switch {
	case !ok:
		return false
	case rule == detailUnsupported:
		return knownCodes(strings.Split(detail, ","))
	case rule == detailContainerID:
		return fullDockerID.MatchString(detail)
	case rule == detailStatus:
		return statusDetail.MatchString(detail)
	}
	return detail == ""
}
```

In `DeploymentRequest`, add after `Deployment`:

```go
	// RequestID is the plan's audit correlation ID; the agent logs and echoes it.
	RequestID string `json:"request_id"`
```

Replace the `Mounts` comment and field in `DeploymentService` with:

```go
	// Mounts are MountVolume or MountBind only; a bind must already be on the replaced container.
	// Always sent: a nil list is invalid.
	Mounts []Mount `json:"mounts"`
```

In `DeploymentRequest.Validate` change the first check to:

```go
	if !deploymentUUID.MatchString(r.Deployment) || !ValidRequestID(r.RequestID) || !execStreamID.MatchString(r.Endpoint) || !deploymentProject.MatchString(r.Project) || r.Revision < 1 || r.Revision > 100 {
		return errors.New("invalid deployment identity")
	}
```

and the mounts check to:

```go
		if s.Mounts == nil || !validMounts(s.Mounts, mounted) {
			return errors.New("invalid mount")
		}
```

Replace the `DeploymentResult` and `DeploymentStep` types and `DeploymentResult.Validate` with:

```go
type DeploymentResult struct {
	Deployment string `json:"deployment"`
	// RequestID echoes the frame's request_id; empty only from a binary built before it.
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	// Code is a result code, set exactly when Outcome is not succeeded.
	Code string `json:"code"`
	// Detail is removed from the wire in the store task; Validate refuses a non-empty one.
	Detail   string               `json:"detail"`
	Steps    []DeploymentStep     `json:"steps"`
	Services []DeploymentIdentity `json:"services"`
}
type DeploymentStep struct {
	Service string `json:"service"`
	Step    string `json:"step"`
	Outcome string `json:"outcome"`
	// Code is a step code, set exactly when Outcome is denied, failed, timed_out or unknown.
	Code string `json:"code"`
	// Detail is Code's parameter: empty, unsupported codes, a container ID or a status.
	Detail string `json:"detail"`
}
```

```go
func (r DeploymentResult) Validate() error {
	if !deploymentUUID.MatchString(r.Deployment) || !resultOutcomes[r.Outcome] || r.Detail != "" || (r.RequestID != "" && !ValidRequestID(r.RequestID)) || len(r.Steps) > MaxDeploymentResultSteps || len(r.Services) > MaxDeploymentServices {
		return errors.New("invalid deployment result")
	}
	if (r.Outcome == OutcomeSucceeded) != (r.Code == "") || (r.Code != "" && !resultCodes[r.Code]) {
		return errors.New("invalid deployment result code")
	}
	for _, s := range r.Steps {
		quiet := s.Outcome == OutcomeSucceeded || s.Outcome == OutcomeSkipped
		if !deploymentService.MatchString(s.Service) || !deploymentSteps[s.Step] || !(resultOutcomes[s.Outcome] || s.Outcome == OutcomeSkipped) || len(s.Detail) > MaxDeploymentStepDetailBytes {
			return errors.New("invalid deployment step")
		}
		if (quiet && (s.Code != "" || s.Detail != "")) || (!quiet && !validStepCode(s.Code, s.Detail)) {
			return errors.New("invalid deployment step code")
		}
	}
	for _, id := range r.Services {
		if !deploymentService.MatchString(id.Service) || (InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() != nil || (id.ImageDigest != "" && !imageID.MatchString(id.ImageDigest)) {
			return errors.New("invalid deployment identity")
		}
	}
	return nil
}
```

In `RemovalRequest`, add after `Deployment`:

```go
	RequestID  string          `json:"request_id"`
```

and in `RemovalRequest.Validate` change the first check to:

```go
	if !deploymentUUID.MatchString(r.Deployment) || !ValidRequestID(r.RequestID) || !execStreamID.MatchString(r.Endpoint) || !deploymentProject.MatchString(r.Project) {
		return errors.New("invalid removal identity")
	}
```

- [ ] **Step 4: Run the protocol tests to verify they pass**

Run: `gofmt -w internal/agent/protocol && go test -race -count=1 ./internal/agent/protocol/ && go build ./... && go vet ./internal/agent/protocol/`
Expected: PASS; the whole tree builds. `./internal/runtime/docker` and `./internal/agent/client` tests now fail on missing codes until Tasks 2 and 3, and `./internal/store` and `./internal/api` on frames without `request_id` (`frame_invalid`) until Task 4.

- [ ] **Step 5: DOX and commit**

`internal/agent/AGENTS.md`, the `protocol.DeploymentRequest`/`DeploymentResult` bullet: after "and 1..`MaxDeploymentServices` (100) services in plan order." insert "Both requests carry a required `RequestID` (`request_id`, `ValidRequestID`: `^[A-Za-z0-9_-]{1,64}$`), the plan's audit correlation ID." Replace "`DeploymentService.Mounts` has no `omitempty`: the server always sends the key, and `nil` after decoding means a server older than mounts, for which the adapter refuses any container with a mount." with "`DeploymentService.Mounts` has no `omitempty` and `nil` is invalid." Replace "and refuses a detail on a succeeded or skipped step." with "and checks outcome codes: a `succeeded` or `skipped` step carries no code and no detail, any other step a step code whose detail has the shape the code allows (`stepCodes`: `unsupported` one to 32 distinct `UnsupportedCodes` joined by `,`, `identity_unreadable`/`identity_unverified` a 64-hex ID, `runtime_status` `[1-5][0-9]{2}`, every other code empty), within `MaxDeploymentStepDetailBytes`; a result's `Code` is set exactly when its outcome is not `succeeded`, from `step_failed`, `clock_skew`, `invalid_request`, `wrong_endpoint`, `busy`, `restarted`, `unreadable`; `legacy` is in both sets and only the server's store assigns it. `RequestID` on a result is optional (empty from a binary built before it) and otherwise must be valid; the protocol never fills a missing code."

```bash
gofmt -l internal cmd
git add internal/agent/protocol internal/agent/AGENTS.md
git commit -m "feat(protocol): closed outcome codes and request IDs on deployment frames" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Runtime — every refusal emits its code and parameter

**Files:**
- Modify: `internal/runtime/docker/deploy.go` (`Deploy`, `refused`, `step`, `succeeded`/`deny`/`fail`, `outcomeFor`, `unsupported`, `prepare`, `replace`; delete `cannotExpress`; drop the `fmt` import)
- Modify: `internal/runtime/docker/deploy_volume.go` (`ensureVolumes`; delete `notOwned`, `notPresent`)
- Modify: `internal/runtime/docker/deploy_pull.go` (`pull`, `tag`)
- Modify: `internal/runtime/docker/remove.go` (`Remove`, `removeTarget`; drop the `errors` import)
- Test: `internal/runtime/docker/deploy_test.go`, `deploy_volume_test.go`, `deploy_pull_test.go`, `remove_test.go`, `deploy_integration_test.go`
- Docs: `internal/runtime/AGENTS.md`

**Interfaces:**
- Consumes (Task 1): `protocol.ValidRequestID`, `protocol.Result*` constants, `DeploymentStep.Code`, `DeploymentResult.Code`, `DeploymentResult.RequestID`, `DeploymentRequest.RequestID`, `RemovalRequest.RequestID`.
- Produces: `Client.Deploy`/`Client.Remove` keep their signatures. Every result echoes the request's `RequestID` when it is valid (else `""`); a refused frame is `denied` `invalid_request` or `failed` `clock_skew` with no step; a run whose step did not succeed is coded `step_failed`; `DeploymentResult.Detail` is never set.

Sentence → code, for every path (the tests below pin each one the fake Engine can reach):

| Where | Old sentence | Outcome | Code | Detail |
|---|---|---|---|---|
| `Deploy`/`Remove` refused frame | `the deployment request is invalid` / `the removal request is invalid` | denied | result `invalid_request` | |
| `Deploy`/`Remove` refused frame | `clock skew exceeds 5 minutes` | failed | result `clock_skew` | |
| `step` result composition | `service %s, step %s: %s` | step's | result `step_failed` | |
| precondition | `the daemon's default runtime could not be read` | failed | `runtime_unreadable` | |
| precondition, recheck | `the container no longer exists` | denied | `container_missing` | |
| precondition (deploy and removal) | `the container is not the one this plan was decided about` | denied | `identity_mismatch` | |
| precondition | `the runtime did not report the container's full configuration` | denied | `configuration_unreported` | |
| precondition | `unsupported: <codes joined by ", ">` | denied | `unsupported` | codes joined by `,`, whole codes within 256 bytes |
| precondition | `the container has configuration the definition cannot express: mounts` | — | deleted: a nil `Mounts` now fails `Validate` (`invalid_request`) | |
| precondition | `bind mount not present on the container` | denied | `bind_missing` | |
| precondition, volume | `volume mount not present on the container` | denied | `volume_mount_missing` | |
| precondition | `the container's image is no longer present` | denied | `image_missing` | |
| precondition | `unsupported: image_config` | denied | `unsupported` | `image_config` |
| volume | `volume is not owned by this project` | denied | `volume_not_owned` | |
| volume | `volume does not exist` | denied | `volume_missing` | |
| volume | `volume create failed` | failed | `volume_create_failed` | |
| image | `the pinned image is not present on this host` | failed | `pinned_image_missing` | |
| image | `the host reported a different image identity` | failed | `image_identity_mismatch` | |
| pull | `not enough time left before the deadline to pull and replace safely` | timed_out | `deadline` | |
| pull | `pull failed` (request build, non-200, stream error line) | failed | `pull_failed` | |
| pull | `unauthorized` | failed | `pull_unauthorized` | |
| pull | `not found` | failed | `pull_not_found` | |
| pull | `pulled image does not match` | failed | `pull_digest_mismatch` | |
| pull tag | `tag failed` | failed | `pull_failed` | |
| recheck | `not enough time left before the deadline to replace this service safely` | timed_out | `deadline` | |
| recheck | `the container changed after the precondition` | denied | `configuration_drift` | |
| rename | `a container already holds the name reserved for the previous one` | failed | `name_reserved` | |
| create | `a container with that name already exists` | failed | `name_taken` | |
| create | `the runtime returned an unusable container identity` | failed | `identity_unusable` | |
| stop, start | `<outcomeFor sentence> (container <id>)` | outcomeFor's | outcomeFor's code | outcomeFor's (the created ID is no longer named) |
| start | `the container started but its identity could not be read: …` | outcomeFor's | `identity_unreadable` | the created container ID |
| start | `the container started but its identity could not be verified (container <id>)` | failed | `identity_unverified` | the created container ID |
| removal stop | `not enough time left before the deadline to remove this container safely` | timed_out | `deadline` | |
| removal remove | `the runtime refused: something still depends on this container` | failed | `dependents` | |
| `outcomeFor` | `the run was cancelled before the runtime answered` | unknown | `cancelled` | |
| `outcomeFor` | `the runtime did not answer in time` | timed_out | `runtime_timeout` | |
| `outcomeFor` | `the runtime call failed` (also a status outside 100..599) | failed | `runtime_error` | |
| `outcomeFor` | `the runtime refused with status %d` | failed | `runtime_status` | the status |

- [ ] **Step 1: Give the fixtures a request ID, a mount list and three identity knobs**

In `internal/runtime/docker/deploy_test.go` add to the `const` block:

```go
	requestID    = "0123456789abcdef0123456789abcdef"
```

Replace `request` and `webService`:

```go
func request(services ...protocol.DeploymentService) protocol.DeploymentRequest {
	return protocol.DeploymentRequest{Deployment: deploymentID, RequestID: requestID, Endpoint: "ep_1", Project: "shop", Revision: 2, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Services: services}
}
func webService() protocol.DeploymentService {
	return protocol.DeploymentService{Name: "web", ContainerName: "shop-web-1", ImageID: newImage, Replaces: protocol.InspectionTarget{ContainerID: oldID, ImageID: oldImage, CreatedUnix: 1700000000}, Restart: "on-failure", Ports: []protocol.Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}}, Env: map[string]string{"TOKEN": "a=b\ncanary-secret", "A": "1"}, Mounts: []protocol.Mount{}}
}

// stepText is a step's code with its parameter, the way the tables below spell them.
func stepText(s protocol.DeploymentStep) string {
	if s.Detail == "" {
		return s.Code
	}
	return s.Code + ": " + s.Detail
}
```

Add three fields to `fakeDeployEngine` (after `inspectNewStatus`):

```go
	imageReportedID  string         // the Id GET /images/{new}/json reports; newImage default
	createdID        string         // the Id POST /containers/create answers; newID default
	startedImage     string         // the Image GET /containers/{new}/json reports; newImage default
```

In `newFakeDeployEngine` add `imageReportedID: newImage, createdID: newID, startedImage: newImage,` to the struct literal, and change three handler cases:

```go
		case r.Method == "GET" && strings.HasSuffix(p, "/images/"+newImage+"/json"):
			w.WriteHeader(f.imageStatus)
			_, _ = w.Write([]byte(`{"Id":"` + f.imageReportedID + `"}`))
```

```go
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/create"):
			w.WriteHeader(f.createStatus)
			_, _ = w.Write([]byte(`{"Id":"` + f.createdID + `","Warnings":[]}`))
```

```go
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+newID+"/json"):
			w.WriteHeader(f.inspectNewStatus)
			_, _ = w.Write([]byte(`{"Id":"` + newID + `","Image":"` + f.startedImage + `","Created":"2024-01-01T00:00:01Z","Name":"/shop-web-1","State":{"Status":"running"}}`))
```

In `internal/runtime/docker/remove_test.go` replace the first line of `removal`:

```go
	req := protocol.RemovalRequest{Deployment: deploymentID, RequestID: requestID, Endpoint: "ep_1", Project: "shop", IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute)}
```

In `internal/runtime/docker/deploy_integration_test.go` add `RequestID: "0123456789abcdef0123456789abcdef",` after `Deployment: …` in the three request literals (lines 116, 291, 392), add `Mounts: []protocol.Mount{},` to the `worker` service literal appended after `docker run` of the worker fixture, and change the recheck assertion (line 402) to:

```go
	if res.Outcome != protocol.OutcomeDenied || len(res.Steps) < 3 || res.Steps[2].Step != protocol.StepRecheck || res.Steps[2].Code != "configuration_drift" || res.Code != protocol.ResultStepFailed {
```

- [ ] **Step 2: Rewrite the assertions to codes and add the refusal-code tests**

`deploy_test.go`:

In `TestDeployReplacesOneServiceInOrder` change the first check to:

```go
	if res.Outcome != protocol.OutcomeSucceeded || res.Deployment != deploymentID || res.RequestID != requestID || res.Code != "" {
```

Replace `TestDeployRefusesAnInvalidRequestWithoutCalling`:

```go
// A frame Validate refuses runs nothing and is answered invalid_request; a request ID that is not
// valid is not echoed.
func TestDeployRefusesAnInvalidRequestWithoutCalling(t *testing.T) {
	f := newFakeDeployEngine(t)
	s := webService()
	s.Env = map[string]string{"1BAD": "x"}
	res := f.client().Deploy(context.Background(), request(s), func() {})
	if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || res.RequestID != requestID || len(f.calls) != 0 || len(res.Steps) != 0 || res.Validate() != nil {
		t.Fatalf("invalid request: %+v calls=%d", res, len(f.calls))
	}
	req := request(webService())
	req.RequestID = "a b\ninjected"
	res = f.client().Deploy(context.Background(), req, func() {})
	if res.Code != protocol.ResultInvalidRequest || res.RequestID != "" || len(f.calls) != 0 || res.Validate() != nil {
		t.Fatalf("bad request id: %+v", res)
	}
}
```

In `TestDeployRuntimeFollowsTheDaemonDefault` change the unreadable-runtime check to:

```go
	if res.Outcome != protocol.OutcomeFailed || res.Steps[0].Step != protocol.StepPrecondition || res.Steps[0].Code != "runtime_unreadable" || res.Steps[len(res.Steps)-1].Outcome != protocol.OutcomeSkipped || len(f.calls) != 1 {
```

In `TestDeployPreconditionsRefuseBeforeTouchingAnything`:
- replace the `const` block with
  ```go
  	const (
  		notTheOne  = "identity_mismatch"
  		unreported = "configuration_unreported"
  		imageGone  = "image_missing"
  	)
  ```
- change the entry `"privileged with devices"`'s expected text from `"unsupported: privileged, devices"` to `"unsupported: privileged,devices"` (every other `"unsupported: <code>"` entry already reads as `stepText`);
- change `res.Steps[0].Detail != tc.detail` to `stepText(res.Steps[0]) != tc.detail`;
- replace the `if !strings.Contains(res.Detail, "service web, step precondition") {…}` block with
  ```go
  			if res.Code != protocol.ResultStepFailed {
  				t.Fatalf("result code: %q", res.Code)
  			}
  ```
- replace the two trailing checks with
  ```go
  	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeDenied || stepText(res.Steps[0]) != "container_missing" || len(f.calls) != 2 {
  		t.Fatalf("missing container: %+v", res)
  	}
  ```
  and
  ```go
  	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); stepText(res.Steps[0]) != "configuration_unreported" {
  		t.Fatalf("absent HostConfig: %+v", res.Steps[0])
  	}
  ```

Replace `TestDeployUnsupportedDetailStaysWithinTheStepBound`:

```go
// Every setting at once still yields a parameter within the wire bound, made of whole codes in
// vocabulary order, and a valid result: an oversized detail would make the agent replace the
// result with unreadable.
func TestDeployUnsupportedDetailStaysWithinTheStepBound(t *testing.T) {
	f := newFakeDeployEngine(t)
	h := f.oldContainer["HostConfig"].(map[string]any)
	for k, v := range map[string]any{"Privileged": true, "AutoRemove": true, "ReadonlyRootfs": true, "Tmpfs": map[string]string{"/run": "rw"}, "CapAdd": []string{"ALL"}, "SecurityOpt": []string{"x"}, "Devices": []any{map[string]any{}}, "PidMode": "host", "IpcMode": "host", "Runtime": "runsc", "Memory": 1, "Ulimits": []any{map[string]any{}}, "Sysctls": map[string]string{"a": "b"}, "DeviceRequests": []any{map[string]any{}}, "Init": true, "UsernsMode": "host", "CgroupParent": "/x", "GroupAdd": []string{"a"}, "ExtraHosts": []string{"a:1.1.1.1"}, "Dns": []string{"1.1.1.1"}, "Links": []string{"a:b"}, "NetworkMode": "host", "VolumesFrom": []string{"x"}, "VolumeDriver": "nfs"} {
		h[k] = v
	}
	f.oldContainer["Config"].(map[string]any)["User"] = "1000"
	f.oldContainer["Mounts"] = []any{map[string]any{"Type": "tmpfs", "Destination": "/run"}, map[string]any{"Type": "volume", "Name": strings.Repeat("ab", 32), "Destination": "/d", "Mode": "nocopy"}}
	res := f.client().Deploy(context.Background(), request(webService()), func() {})
	s := res.Steps[0]
	if res.Outcome != protocol.OutcomeDenied || s.Code != "unsupported" || !strings.HasPrefix(s.Detail, "mount_type,anonymous_volume,") || len(s.Detail) > protocol.MaxDeploymentStepDetailBytes {
		t.Fatalf("detail %d bytes: %q", len(s.Detail), s.Detail)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
}
```

Replace `TestDeployStepFailuresStopTheRun`:

```go
func TestDeployStepFailuresStopTheRun(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*fakeDeployEngine)
		outcome string
		step    string
		want    string // stepText of the failing step
		calls   int
	}{
		"image missing":   {func(f *fakeDeployEngine) { f.imageStatus = 404 }, protocol.OutcomeFailed, protocol.StepImage, "pinned_image_missing", 4},
		"rename conflict": {func(f *fakeDeployEngine) { f.renameStatus = 409 }, protocol.OutcomeFailed, protocol.StepRename, "name_reserved", 6},
		"create conflict": {func(f *fakeDeployEngine) { f.createStatus = 409 }, protocol.OutcomeFailed, protocol.StepCreate, "name_taken", 7},
		"stop refused":    {func(f *fakeDeployEngine) { f.stopStatus = 500 }, protocol.OutcomeFailed, protocol.StepStop, "runtime_status: 500", 8},
		"start fails":     {func(f *fakeDeployEngine) { f.startStatus = 500 }, protocol.OutcomeFailed, protocol.StepStart, "runtime_status: 500", 9},
		"remove fails":    {func(f *fakeDeployEngine) { f.removeStatus = 409 }, protocol.OutcomeFailed, protocol.StepRemove, "runtime_status: 409", 11},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			res := f.client().Deploy(context.Background(), request(webService()), func() {})
			if res.Outcome != tc.outcome || res.Code != protocol.ResultStepFailed || len(f.calls) != tc.calls {
				t.Fatalf("%s: %+v calls=%v", name, res, f.steps())
			}
			var failing protocol.DeploymentStep
			for _, s := range res.Steps {
				if s.Outcome != protocol.OutcomeSucceeded && s.Outcome != protocol.OutcomeSkipped {
					failing = s
					break
				}
			}
			if failing.Step != tc.step || stepText(failing) != tc.want {
				t.Fatalf("%s failed at %+v", name, failing)
			}
			if tc.step == protocol.StepRemove {
				if len(res.Services) != 1 {
					t.Fatal("started service must keep its identity when only removal failed")
				}
			} else if len(res.Services) != 0 {
				t.Fatalf("%s recorded an identity it did not start: %+v", name, res.Services)
			}
			if raw, _ := json.Marshal(res); strings.Contains(string(raw), "canary") {
				t.Fatal("an environment value reached the result")
			}
			if err := res.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	f := newFakeDeployEngine(t)
	f.stopStatus = 304
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("already stopped must count: %+v", res)
	}
}

// The identity refusals the fixture can reach name their code, and the two that concern a
// started container name it by its full ID.
func TestDeployIdentityRefusalCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*fakeDeployEngine)
		step   string
		want   string
	}{
		"image identity":      {func(f *fakeDeployEngine) { f.imageReportedID = oldImage }, protocol.StepImage, "image_identity_mismatch"},
		"unusable identity":   {func(f *fakeDeployEngine) { f.createdID = "not-an-id" }, protocol.StepCreate, "identity_unusable"},
		"unverified identity": {func(f *fakeDeployEngine) { f.startedImage = oldImage }, protocol.StepStart, "identity_unverified: " + newID},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			res := f.client().Deploy(context.Background(), request(webService()), func() {})
			var failing protocol.DeploymentStep
			for _, s := range res.Steps {
				if s.Outcome != protocol.OutcomeSucceeded && s.Outcome != protocol.OutcomeSkipped {
					failing = s
					break
				}
			}
			if res.Outcome != protocol.OutcomeFailed || failing.Step != tc.step || stepText(failing) != tc.want || len(res.Services) != 0 || res.Validate() != nil {
				t.Fatalf("%+v", res)
			}
		})
	}
}
```

In `TestDeployTimeAndCancellation` change the two mid-stop checks to:

```go
	if res.Outcome != protocol.OutcomeTimedOut || res.Steps[5].Step != protocol.StepStop || res.Steps[5].Outcome != protocol.OutcomeTimedOut || res.Steps[5].Code != "runtime_timeout" || len(res.Services) != 0 {
```

```go
	if res.Outcome != protocol.OutcomeUnknown || res.Steps[5].Step != protocol.StepStop || res.Steps[5].Outcome != protocol.OutcomeUnknown || res.Steps[5].Code != "cancelled" {
```

and the past-deadline check to `res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || len(f.calls) != 0`.

In `TestDeployRefusesToStartWithoutTimeToFinish` replace `res.Steps[2].Detail != "not enough time left before the deadline to replace this service safely"` with `res.Steps[2].Code != "deadline"`.

In `TestDeployIdentityReadFailureNamesTheContainer` replace the first check with:

```go
	if res.Outcome != protocol.OutcomeFailed || res.Steps[6].Step != protocol.StepStart || stepText(res.Steps[6]) != "identity_unreadable: "+newID || len(res.Services) != 0 || res.Validate() != nil {
```

In `TestDeployReportsClockSkewWithoutCalling` replace the check with:

```go
	if res.Outcome != protocol.OutcomeFailed || res.Code != protocol.ResultClockSkew || res.RequestID != requestID || len(res.Steps) != 0 || len(f.calls) != 0 {
```

In `TestRecheckDeniesDriftBetweenThePhases` replace the outcome check with:

```go
			if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultStepFailed || res.Steps[10].Step != protocol.StepRecheck || res.Steps[10].Service != "db" || res.Steps[10].Code != "configuration_drift" {
```

In `TestRecheckDeniesAContainerGoneBetweenThePhases` replace `res.Steps[2].Detail != "the container no longer exists"` with `res.Steps[2].Code != "container_missing"`.

`deploy_volume_test.go`:
- `TestDeployVolumeFailureTouchesNoContainer`: replace `res.Steps[1].Detail != "volume create failed" || res.Detail != "service web, step volume: volume create failed"` with `res.Steps[1].Code != "volume_create_failed" || res.Code != protocol.ResultStepFailed`.
- `TestDeployVolumeOwnership`: change the `const` line to `const notOwned, notPresent = "volume_not_owned", "volume_mount_missing"`, the two `detail: "volume does not exist"` entries to `detail: "volume_missing"`, and `res.Steps[1].Detail != tc.detail` to `stepText(res.Steps[1]) != tc.detail`.
- `TestDeployVolumeRefusalDetails`: replace `res.Steps[0].Detail != "unsupported: "+detail` with `res.Steps[0].Code != "unsupported" || res.Steps[0].Detail != detail`.
- `TestDeployBindPrecondition`: replace every `"bind mount not present on the container"` in its table with `"bind_missing"` (the `"unsupported: mount_type"` entry stays) and `res.Steps[0].Detail != tc.detail` with `stepText(res.Steps[0]) != tc.detail`.
- `TestDeployKeepOnlyVolumePerService`: change `const notPresent = "volume mount not present on the container"` to `const notPresent = "volume_mount_missing"`, both `"volume is not owned by this project"` to `"volume_not_owned"`, and `s.Detail != tc.detail` to `stepText(s) != tc.detail`.
- Replace `TestDeployFrameWithoutMountsKey`:

```go
// A frame without the mounts key comes from a server older than mounts (which also sends no
// request_id): Validate refuses it and nothing is called. "mounts":[] is a definition that drops
// them, already shown as dropped.
func TestDeployFrameWithoutMountsKey(t *testing.T) {
	for name, tc := range map[string]struct {
		absent, mounted bool
		outcome, code   string
	}{
		"absent, container has mounts": {true, true, protocol.OutcomeDenied, protocol.ResultInvalidRequest},
		"absent, container has none":   {true, false, protocol.OutcomeDenied, protocol.ResultInvalidRequest},
		"empty, container has mounts":  {false, true, protocol.OutcomeSucceeded, ""},
		"empty, container has none":    {false, false, protocol.OutcomeSucceeded, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			if tc.mounted {
				withBind(f)
			}
			s := decoded(t, webService(), tc.absent)
			if (s.Mounts == nil) != tc.absent {
				t.Fatalf("decoded mounts %#v", s.Mounts)
			}
			res := f.client().Deploy(context.Background(), request(s), func() {})
			if res.Outcome != tc.outcome || res.Code != tc.code {
				t.Fatalf("%+v", res)
			}
			if tc.absent && len(f.calls) != 0 {
				t.Fatalf("a refused frame called the Engine: %v", f.steps())
			}
		})
	}
}
```

`deploy_pull_test.go`:
- `TestDeployPullVerifiesDockerHubSpelling`: replace `res.Steps[1].Detail != "pulled image does not match"` with `res.Steps[1].Code != "pull_digest_mismatch"`.
- `TestDeployPullFailuresTouchNothing`: change the expected texts to codes: `"401"`/`"403"` → `"pull_unauthorized"`, `"404"` → `"pull_not_found"`, `"500"` and `"stream error"` → `"pull_failed"`, `"mismatch"` and `"id shape"` → `"pull_digest_mismatch"`, `"inspect 404"` → `"runtime_status: 404"`, `"inspect 500"` → `"runtime_status: 500"`, `"tag 500"`/`"tag 404"` → `"pull_failed"`, `"stream cut"` → `"runtime_error"`; and `res.Steps[1].Detail != tc.detail` → `stepText(res.Steps[1]) != tc.detail`.
- `TestDeploySecondPullFailureTouchesNothing`: replace `res.Detail != "service db, step pull: unauthorized"` with `res.Code != protocol.ResultStepFailed || res.Steps[3].Code != "pull_unauthorized"`.
- `TestDeployPullRefusedWithoutTimeToReplace`: add `|| res.Steps[1].Code != "deadline"` to its outcome check.

`remove_test.go`:
- `TestRemoveStopsThenDeletesEachTarget`: `res.Detail != ""` → `res.Code != "" || res.RequestID != requestID`.
- `TestRemoveRefusesAContainerThatIsNotTheDecidedOne`: `res.Steps[0].Detail != "the container is not the one this plan was decided about"` → `res.Steps[0].Code != "identity_mismatch"`.
- `TestRemoveStepFailuresStopTheRun`: after the stop-500 check add `if res.Steps[1].Code != "runtime_status" || res.Steps[1].Detail != "500" { t.Fatalf("stop 500 step: %+v", res.Steps[1]) }`; replace `res.Steps[2].Detail != "the runtime refused: something still depends on this container"` with `res.Steps[2].Code != "dependents"`.
- `TestRemoveRefusesToStartWithoutTimeToFinish`: `res.Steps[1].Detail != "not enough time left before the deadline to remove this container safely"` → `res.Steps[1].Code != "deadline"`.
- `TestRemoveRefusesAnInvalidRequestWithoutCalling`: add `|| res.Code != protocol.ResultInvalidRequest` to the check.
- `TestRemoveTwoTargetsSecondMismatched`: `!strings.HasPrefix(res.Detail, "service db, step precondition:")` → `res.Code != protocol.ResultStepFailed || res.Steps[3].Code != "identity_mismatch"`.
- `TestRemoveReportsClockSkewWithoutCalling`: `res.Detail != "clock skew exceeds 5 minutes"` → `res.Code != protocol.ResultClockSkew || res.RequestID != requestID`.

- [ ] **Step 3: Run to verify they fail**

Run: `go test -count=1 ./internal/runtime/docker/`
Expected: FAIL: the tests compile (Task 1 added `Code` and `RequestID`), but the adapter still writes sentences and no codes, so the code assertions fail and `res.Validate()` refuses its failing results.

- [ ] **Step 4: Implement the adapter**

`internal/runtime/docker/deploy.go`: remove `"fmt"` from the imports. Replace the start of `Deploy` up to `ctx, cancel := context.WithDeadline(parent, req.Deadline)` with:

```go
func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.Validate(time.Now()); err != nil {
		return refused(res, err)
	}
```

Replace `step` and `outcomeFor` with:

```go
// refused answers a frame Validate refused, with no step run: invalid_request, or clock_skew
// failed. A request ID that is not valid is not echoed.
func refused(res protocol.DeploymentResult, err error) protocol.DeploymentResult {
	if !protocol.ValidRequestID(res.RequestID) {
		res.RequestID = ""
	}
	res.Outcome, res.Code = protocol.OutcomeDenied, protocol.ResultInvalidRequest
	if errors.Is(err, protocol.ErrClockSkew) {
		res.Outcome, res.Code = protocol.OutcomeFailed, protocol.ResultClockSkew
	}
	return res
}

// step records one outcome with its code and the code's parameter. The first non-success fixes
// the run's outcome, coded step_failed: the steps say which.
func (r *deployRun) step(service, step string, run func() (outcome, code, detail string)) {
	if r.res.Outcome != "" {
		r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: step, Outcome: protocol.OutcomeSkipped})
		return
	}
	outcome, code, detail := run()
	s := protocol.DeploymentStep{Service: service, Step: step, Outcome: outcome}
	if outcome != protocol.OutcomeSucceeded {
		s.Code, s.Detail = code, detail
		r.res.Outcome, r.res.Code = outcome, protocol.ResultStepFailed
	}
	r.res.Steps = append(r.res.Steps, s)
}

// succeeded, deny and fail are a step's plain answers.
func succeeded() (string, string, string)      { return protocol.OutcomeSucceeded, "", "" }
func deny(code string) (string, string, string) { return protocol.OutcomeDenied, code, "" }
func fail(code string) (string, string, string) { return protocol.OutcomeFailed, code, "" }

// outcomeFor classifies a call that did not succeed. A status is an answer: failed,
// runtime_status with the status. With no answer, a cancelled parent means the run was cancelled
// (agent shutdown under the detached-context contract) before the runtime answered, and the
// Engine may have acted: unknown, never retried by itself.
func (r *deployRun) outcomeFor(ctx context.Context, err error, status int) (string, string, string) {
	if statusOf(err) != 0 {
		err = nil
	}
	switch {
	case err != nil && r.parent.Err() == context.Canceled:
		return protocol.OutcomeUnknown, "cancelled", ""
	case err != nil && ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "runtime_timeout", ""
	case err != nil, status < 100, status > 599:
		return fail("runtime_error")
	}
	return protocol.OutcomeFailed, "runtime_status", strconv.Itoa(status)
}
```

Delete `const cannotExpress = …`. Replace `unsupported` with:

```go
// unsupported is the precondition's refusal for configuration a recreate would drop: the codes
// joined by ",", as many whole ones as the step bound holds. The plan's live inspection already
// listed every one.
func unsupported(codes []string) (string, string, string) {
	detail := codes[0]
	for _, c := range codes[1:] {
		if len(detail)+1+len(c) > protocol.MaxDeploymentStepDetailBytes {
			break
		}
		detail += "," + c
	}
	return protocol.OutcomeDenied, "unsupported", detail
}
```

Replace `prepare` from `r.step(s.Name, protocol.StepPrecondition, …` to its `return prepared{…}` with:

```go
	r.step(s.Name, protocol.StepPrecondition, func() (string, string, string) {
		if r.defaultRuntime == "" {
			return fail("runtime_unreadable")
		}
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		if err := r.c.get(cctx, "/containers/"+old+"/json", &before); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return deny("container_missing")
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		if before.ID != s.Replaces.ContainerID || before.Image != s.Replaces.ImageID || before.Created.Unix() != s.Replaces.CreatedUnix {
			return deny("identity_mismatch")
		}
		if !reported(before) {
			return deny("configuration_unreported")
		}
		if codes := undescribed(before, r.req.Project+"_default", r.defaultRuntime); len(codes) > 0 {
			return unsupported(codes)
		}
		// Binds are preserve-only: a deploy never introduces a host path.
		for _, m := range s.Mounts {
			if m.Kind == protocol.MountBind && !slices.ContainsFunc(*before.Mounts, func(o inspectedMount) bool {
				return o.Type == "bind" && o.Source == m.Source && o.Destination == m.Target && o.RW == !m.ReadOnly
			}) {
				return deny("bind_missing")
			}
			if m.Kind == protocol.MountVolume && r.keepOnly[m.Source] && !keeps(*before.Mounts, m) {
				return deny("volume_mount_missing")
			}
		}
		ictx, icancel := context.WithTimeout(ctx, callBudget)
		defer icancel()
		var im struct{ Config imageDefaults }
		if err := r.c.get(ictx, "/images/"+url.PathEscape(s.Replaces.ImageID)+"/json", &im); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return deny("image_missing")
			}
			return r.outcomeFor(ictx, err, statusOf(err))
		}
		if before.Config.differs(im.Config) {
			return unsupported([]string{"image_config"})
		}
		networkMode, oldMounts = before.HostConfig.NetworkMode, *before.Mounts
		return succeeded()
	})
	r.ensureVolumes(ctx, s, oldMounts)
	if s.Pull != nil {
		r.step(s.Name, protocol.StepPull, func() (string, string, string) {
			outcome, code, detail, id := r.pull(ctx, s)
			s.ImageID = id // the replacement is created from, and verified against, the pulled ID
			return outcome, code, detail
		})
	} else {
		r.step(s.Name, protocol.StepImage, func() (string, string, string) {
			cctx, cancel := context.WithTimeout(ctx, callBudget)
			defer cancel()
			var im struct {
				ID string `json:"Id"`
			}
			if err := r.c.get(cctx, "/images/"+url.PathEscape(s.ImageID)+"/json", &im); err != nil {
				if statusOf(err) == http.StatusNotFound {
					return fail("pinned_image_missing")
				}
				return r.outcomeFor(cctx, err, statusOf(err))
			}
			if im.ID != s.ImageID {
				return fail("image_identity_mismatch")
			}
			return succeeded()
		})
	}
	return prepared{s: s, name: strings.TrimPrefix(before.Name, "/"), networkMode: networkMode, before: before}
```

Replace the body of `replace` with:

```go
	s, old := p.s, url.PathEscape(p.s.Replaces.ContainerID)
	// The pull window can be minutes: re-read the container right before touching it.
	r.step(s.Name, protocol.StepRecheck, func() (string, string, string) {
		if time.Until(r.req.Deadline) < replaceBudget {
			return protocol.OutcomeTimedOut, "deadline", ""
		}
		r.begin()
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var now inspectedForDeploy
		if err := r.c.get(cctx, "/containers/"+old+"/json", &now); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return deny("container_missing")
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		if now.ID != s.Replaces.ContainerID || now.Image != s.Replaces.ImageID || now.Created.Unix() != s.Replaces.CreatedUnix || !sameConfiguration(p.before, now) {
			return deny("configuration_drift")
		}
		return succeeded()
	})
	r.step(s.Name, protocol.StepRename, func() (string, string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		name := p.name + ".kyyard-prev-" + r.req.Deployment[:8]
		status, err := r.c.post(cctx, "/containers/"+old+"/rename?name="+url.QueryEscape(name))
		if err != nil || status >= 400 {
			if status == http.StatusConflict {
				return fail("name_reserved")
			}
			return r.outcomeFor(cctx, err, status)
		}
		return succeeded()
	})
	var created string
	r.step(s.Name, protocol.StepCreate, func() (string, string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var out struct {
			ID string `json:"Id"`
		}
		status, err := r.c.postJSON(cctx, "/containers/create?name="+url.QueryEscape(s.ContainerName), createBody(r.req, s, p.networkMode), &out)
		if err != nil || status != http.StatusCreated {
			if status == http.StatusConflict {
				return fail("name_taken")
			}
			return r.outcomeFor(cctx, err, status)
		}
		if !protocol.ValidExecID(out.ID) {
			return fail("identity_unusable")
		}
		created = out.ID
		return succeeded()
	})
	r.step(s.Name, protocol.StepStop, func() (string, string, string) {
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+old+"/stop?t="+strconv.Itoa(stopGrace))
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		return succeeded()
	})
	r.step(s.Name, protocol.StepStart, func() (string, string, string) {
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+url.PathEscape(created)+"/start")
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		ictx, icancel := context.WithTimeout(ctx, callBudget)
		defer icancel()
		var after inspectedForDeploy
		if err := r.c.get(ictx, "/containers/"+url.PathEscape(created)+"/json", &after); err != nil {
			outcome, _, _ := r.outcomeFor(ictx, err, statusOf(err))
			return outcome, "identity_unreadable", created
		}
		id := protocol.DeploymentIdentity{Service: s.Name, ContainerID: after.ID, ImageID: after.Image, CreatedUnix: after.Created.Unix()}
		if s.Pull != nil {
			id.ImageDigest = s.Pull.Digest
		}
		if after.ID != created || after.Image != s.ImageID || (protocol.InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() != nil {
			return protocol.OutcomeFailed, "identity_unverified", created
		}
		r.res.Services = append(r.res.Services, id)
		return succeeded()
	})
	r.step(s.Name, protocol.StepRemove, func() (string, string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		status, err := r.c.del(cctx, "/containers/"+old)
		if err != nil || status >= 400 {
			return r.outcomeFor(cctx, err, status)
		}
		return succeeded()
	})
```

(`created` is a full 64-hex ID here: `ValidExecID` accepted it at create.)

`internal/runtime/docker/deploy_volume.go`: delete `const notOwned …` and `const notPresent …`, and replace the `r.step(s.Name, protocol.StepVolume, …)` call in `ensureVolumes` with:

```go
		r.step(s.Name, protocol.StepVolume, func() (string, string, string) {
			cctx, cancel := context.WithTimeout(ctx, callBudget)
			defer cancel()
			var v dockerVolume
			err := r.c.get(cctx, "/volumes/"+url.PathEscape(m.Source), &v)
			switch {
			case err == nil && v.owned(r.req.Project):
				return succeeded()
			case err == nil && !mountsVolume(old, m.Source):
				return deny("volume_not_owned")
			case err == nil:
				for _, sm := range s.Mounts {
					if sm.Kind == protocol.MountVolume && sm.Source == m.Source && !keeps(old, sm) {
						return deny("volume_mount_missing")
					}
				}
				r.keepOnly[m.Source] = true
				return succeeded()
			case statusOf(err) != http.StatusNotFound:
				return r.outcomeFor(cctx, err, statusOf(err))
			}
			// Only the project's own volumes are created; an external one must already exist.
			short, ok := strings.CutPrefix(m.Source, r.req.Project+"_")
			if !ok || short == "" {
				return deny("volume_missing")
			}
			body := struct {
				Name   string            `json:"Name"`
				Labels map[string]string `json:"Labels"`
			}{m.Source, map[string]string{"com.docker.compose.project": r.req.Project, "com.docker.compose.volume": short}}
			status, err := r.c.postJSON(cctx, "/volumes/create", body, &v)
			switch {
			case err != nil:
				return r.outcomeFor(cctx, err, status)
			case status != http.StatusCreated:
				return fail("volume_create_failed")
			case !v.owned(r.req.Project):
				// Docker answers 201 with the existing volume when the name was taken meanwhile.
				return deny("volume_not_owned")
			}
			return succeeded()
		})
```

`internal/runtime/docker/deploy_pull.go`: replace `pull` and `tag` with:

```go
// pull fetches s.Pull by digest with the frame's credential for its host, proves the image the
// daemon now holds is that repository at that digest, and tags it with s.Pull.Tag so the
// host's tag follows the update. It returns the step's answer and the image ID the replacement
// is created from. Codes are fixed: daemon text can echo the registry's answer.
func (r *deployRun) pull(ctx context.Context, s protocol.DeploymentService) (outcome, code, detail, imageID string) {
	if time.Until(r.pullDeadline) < callBudget {
		return protocol.OutcomeTimedOut, "deadline", "", ""
	}
	pctx, cancel := context.WithDeadline(ctx, r.pullDeadline)
	defer cancel()
	name, digest, _ := strings.Cut(s.Pull.Reference, "@")
	q := url.Values{"fromImage": {name}, "tag": {digest}}
	req, err := http.NewRequestWithContext(pctx, http.MethodPost, r.c.base+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return protocol.OutcomeFailed, "pull_failed", "", ""
	}
	if auth, ok := r.req.Registries[s.Pull.Host()]; ok {
		raw, _ := json.Marshal(struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Server   string `json:"serveraddress"`
		}{auth.Username, auth.Secret, s.Pull.Host()})
		// URL-safe: the daemon decodes with base64.URLEncoding and pulls anonymously on failure.
		req.Header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(raw))
	}
	resp, err := r.c.http.Do(req)
	if err != nil {
		o, c, d := r.outcomeFor(pctx, err, 0)
		return o, c, d, ""
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return protocol.OutcomeFailed, "pull_unauthorized", "", ""
	case resp.StatusCode == http.StatusNotFound:
		return protocol.OutcomeFailed, "pull_not_found", "", ""
	case resp.StatusCode != http.StatusOK:
		return protocol.OutcomeFailed, "pull_failed", "", ""
	}
	// A pull reports late failures as an error line inside the 200.
	failure, err := scanPullStream(resp.Body)
	if err != nil {
		o, c, d := r.outcomeFor(pctx, err, 0)
		return o, c, d, ""
	}
	if failure != "" {
		return protocol.OutcomeFailed, "pull_failed", "", ""
	}
	var im struct {
		ID          string `json:"Id"`
		RepoDigests []string
	}
	if err := r.c.get(pctx, "/images/"+url.PathEscape(s.Pull.Reference)+"/json", &im); err != nil {
		o, c, d := r.outcomeFor(pctx, err, statusOf(err))
		return o, c, d, ""
	}
	if len(im.ID) == 71 && strings.HasPrefix(im.ID, "sha256:") && protocol.ValidImageReference(im.ID) {
		if slices.ContainsFunc(im.RepoDigests, func(rd string) bool {
			n, d, ok := strings.Cut(rd, "@")
			return ok && d == digest && canonicalRepository(n) == canonicalRepository(name)
		}) {
			return r.tag(pctx, s.Pull.Tag, im.ID)
		}
	}
	return protocol.OutcomeFailed, "pull_digest_mismatch", "", ""
}

// tag points tagRef at the verified pulled image; the Engine answers 201. A refused tag is part
// of the pull step: pull_failed.
func (r *deployRun) tag(ctx context.Context, tagRef, id string) (outcome, code, detail, imageID string) {
	if tagRef == "" {
		return protocol.OutcomeSucceeded, "", "", id
	}
	repo, tag := protocol.SplitImageReference(tagRef)
	status, err := r.c.post(ctx, "/images/"+url.PathEscape(id)+"/tag?repo="+url.QueryEscape(repo)+"&tag="+url.QueryEscape(tag))
	if err != nil {
		o, c, d := r.outcomeFor(ctx, err, 0)
		return o, c, d, ""
	}
	if status != http.StatusCreated {
		return protocol.OutcomeFailed, "pull_failed", "", ""
	}
	return protocol.OutcomeSucceeded, "", "", id
}
```

`internal/runtime/docker/remove.go`: remove `"errors"` from the imports; replace the start of `Remove` up to `ctx, cancel := …` with:

```go
func (c *Client) Remove(parent context.Context, req protocol.RemovalRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.Validate(time.Now()); err != nil {
		return refused(res, err)
	}
```

and the three `r.step` calls in `removeTarget` with:

```go
	r.step(t.Service, protocol.StepPrecondition, func() (string, string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var in struct {
			ID      string `json:"Id"`
			Image   string
			Created time.Time
		}
		if err := r.c.get(cctx, "/containers/"+id+"/json", &in); err != nil {
			if statusOf(err) == http.StatusNotFound {
				gone = true
				return succeeded()
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		if in.ID != t.Target.ContainerID || in.Image != t.Target.ImageID || in.Created.Unix() != t.Target.CreatedUnix {
			return deny("identity_mismatch")
		}
		return succeeded()
	})
```

```go
	r.step(t.Service, protocol.StepStop, func() (string, string, string) {
		if time.Until(deadline) < operationBudget+2*callBudget {
			return protocol.OutcomeTimedOut, "deadline", ""
		}
		r.begin()
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+id+"/stop?t="+strconv.Itoa(stopGrace))
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		return succeeded()
	})
	r.step(t.Service, protocol.StepRemove, func() (string, string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		status, err := r.c.del(cctx, "/containers/"+id)
		switch {
		case status == http.StatusConflict:
			return fail("dependents")
		case err != nil || (status >= 400 && status != http.StatusNotFound):
			return r.outcomeFor(cctx, err, status)
		}
		return succeeded()
	})
```

- [ ] **Step 5: Run to verify they pass**

Run: `gofmt -w internal/runtime/docker && go vet ./internal/runtime/docker/ && go test -race -count=1 ./internal/runtime/docker/`
Expected: PASS. With Docker available also `KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 KY_TEST_DOCKER_INSPECTION_IMAGE=alpine:3.24 go test -race -count=1 ./internal/runtime/docker -run '^Test(Inspection|Deploy|Recheck|Remove)RealDocker$'` PASS.

- [ ] **Step 6: DOX and commit**

`internal/runtime/AGENTS.md`, the `Client.Deploy` bullet: replace "with detail `unsupported: <codes joined by ", ">`, bounded to 256 bytes; an absent pointer-decoded field is still `the runtime did not report the container's full configuration` (`reported`)" with "as step code `unsupported` whose detail is the codes joined by `,`, as many whole codes as 256 bytes hold; an absent pointer-decoded field is `configuration_unreported` (`reported`)"; delete "on a frame service without the `mounts` key (nil after decoding: a server older than mounts) against a container with any mount (`cannotExpress + "mounts"`, the pre-mounts rule; `[]` is a deliberate drop and allowed),"; replace "(`bind mount not present on the container`:" with "(`bind_missing`:"; replace "`denied` `the container changed after the precondition`" with "`denied` `configuration_drift`" and "404 is `denied` `the container no longer exists`" with "404 is `denied` `container_missing`"; replace "A failed stop, start or identity read names the created container ID in the step detail; no identity is recorded and the old container is not removed." with "A failed stop or start carries `outcomeFor`'s code; an identity that could not be read is `identity_unreadable` and one that does not match `identity_unverified`, each with the created container's 64-hex ID as detail; no identity is recorded and the old container is not removed."; replace "With no Engine answer, a cancelled run (agent shutdown under the detached-context contract) is `unknown` and a deadline `timed_out`; a status is `failed`. Engine text never enters a result." with "Every non-success step carries a step code (docs/agent-protocol.md, Outcome codes) and the run's result code is `step_failed`; a frame `Validate` refuses is `denied` `invalid_request` (or `failed` `clock_skew`) with no step, echoing the request ID only when valid. `outcomeFor`: with no Engine answer, a cancelled run (agent shutdown under the detached-context contract) is `unknown` `cancelled` and a deadline `timed_out` `runtime_timeout`; a status is `failed` `runtime_status` with the 3-digit status; anything else `runtime_error`. Engine text never enters a result." The volume bullet: replace "(else `denied` `volume is not owned by this project`)" with "(else `denied` `volume_not_owned`)", "(else `denied` `volume mount not present on the container`)" with "(else `denied` `volume_mount_missing`)", "is `denied` `volume does not exist`" with "is `denied` `volume_missing`", "is `failed` `volume create failed`" with "is `failed` `volume_create_failed`". The pull bullet: replace "Fixed `failed` details: 401/403 `unauthorized`, 404 `not found` (Docker Hub answers a private repository without a credential this way), other status or a stream `error` line `pull failed`, a mismatch `pulled image does not match`;" with "Fixed `failed` codes: 401/403 `pull_unauthorized`, 404 `pull_not_found` (Docker Hub answers a private repository without a credential this way), other status or a stream `error` line `pull_failed`, a mismatch `pull_digest_mismatch`; a pull with too little of its window is `timed_out` `deadline`;", and "(201, anything else `tag failed`)" with "(201, anything else `pull_failed`)". The `Client.Remove` bullet: replace "else `denied`;" with "else `denied` `identity_mismatch`;", "409 is `failed` naming a dependency" with "409 is `failed` `dependents`", and "is `timed_out` at that target's stop" with "is `timed_out` `deadline` at that target's stop".

```bash
gofmt -l internal cmd
git add internal/runtime
git commit -m "feat(runtime): report every refusal as a closed code and parameter" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Agent client — result codes, request ID echoed and logged

**Files:**
- Modify: `internal/agent/client/deployments.go` (`restartedDetail` goes; `deploymentEntry.RequestID`; `settleStarted`, `begin`, `finish`, `denied`, `handleApply`, `handleRemoval`, `run`; new `logf`)
- Test: `internal/agent/client/deployments_test.go`
- Docs: `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes (Task 1): `protocol.ValidRequestID`, `protocol.ResultClockSkew`, `ResultInvalidRequest`, `ResultWrongEndpoint`, `ResultBusy`, `ResultRestarted`, `ResultUnreadable`, `DeploymentRequest.RequestID`, `RemovalRequest.RequestID`, `DeploymentResult.RequestID`/`Code`.
- Produces: every answer the deployer sends carries `RequestID` (the frame's, when valid, else `""`) and a result code instead of a sentence: clock skew `failed` `clock_skew`; a frame that does not decode or fails `Validate`, or arrives at an agent with no runtime for it, `denied` `invalid_request`; another endpoint's frame `denied` `wrong_endpoint`; a second run `denied` `busy`; an unreadable runtime result `unknown` `unreadable`; a restart after replacement began `unknown` `restarted`. `deployer.begin(id, requestID string)`. The deployer logs `deployment <id> (request <request id>): received|started|finished <outcome>`, only after `Validate` accepted the frame.

- [ ] **Step 1: Write the failing tests**

In `internal/agent/client/deployments_test.go` replace `testRequest` and `testRemoval`:

```go
func testRequest(endpoint string) protocol.DeploymentRequest {
	return protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: endpoint, Project: "shop", Revision: 1, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Services: []protocol.DeploymentService{{Name: "web", ContainerName: "shop-web-1", ImageID: "sha256:" + strings.Repeat("a", 64), Replaces: protocol.InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000}, Env: map[string]string{"TOKEN": "agent-secret-canary"}, Mounts: []protocol.Mount{}}}}
}

func testRemoval(endpoint string) protocol.RemovalRequest {
	return protocol.RemovalRequest{Deployment: "6c5e4f3a-1b0d-4e9f-8a7b-4f5a6b7c8d9e", RequestID: "fedcba9876543210fedcba9876543210", Endpoint: endpoint, Project: "shop", IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Containers: []protocol.RemovalTarget{{Service: "web", Target: protocol.InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000}}}}
}
```

Edit the existing tests:
- `TestDeployerWaitsForTheRunToRecord`: in the fake result replace `Detail: "cancelled"` with `Code: protocol.ResultStepFailed`.
- `TestDeployerReplacesAnUnreadableResult`: replace `res.Detail != "the runtime returned an unreadable result"` with `res.Code != protocol.ResultUnreadable || res.RequestID != req.RequestID`, and `!strings.Contains(string(stored), "unreadable result")` with `!strings.Contains(string(stored), "\"code\":\"unreadable\"")`.
- `TestDeployerRefusals`: replace the four reads with
  ```go
  	if res := read(); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultWrongEndpoint || res.RequestID != "0123456789abcdef0123456789abcdef" || calls != 0 {
  		t.Fatalf("foreign: %+v", res)
  	}
  ```
  ```go
  	if res := read(); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || calls != 0 {
  		t.Fatalf("invalid: %+v", res)
  	}
  ```
  ```go
  	first, next := read(), read()
  	busy := first
  	if next.Outcome == protocol.OutcomeDenied {
  		busy = next
  	}
  	if busy.Outcome != protocol.OutcomeDenied || busy.Code != protocol.ResultBusy {
  		t.Fatalf("one of two concurrent applies must be denied busy: %+v %+v", first, next)
  	}
  ```
  ```go
  	if res := read(); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest {
  		t.Fatalf("no runtime: %+v", res)
  	}
  ```
- `TestDeployerResendsPersistedResults` and the ledger literal near `req.Deployment: {Result: …Outcome: protocol.OutcomeFailed, Detail: "x"…}` (in the test that writes a ledger for `req.Deployment`): replace `Detail: "x"` with `Code: protocol.ResultStepFailed`.
- `sessionCarriesDeployments`: in the ledger literal add `Code: protocol.ResultStepFailed,` after `Outcome: protocol.OutcomeFailed,`.
- `TestDeployerSharesTheSlotWithRemoval`: replace `res.Detail != "this agent is already applying a deployment"` with `res.Code != protocol.ResultBusy || res.RequestID != removal.RequestID`.
- `TestDeployerRemovalRefusals`: replace `res.Detail != "this agent has no runtime to remove"` with `res.Code != protocol.ResultInvalidRequest`, `res.Detail != "this deployment is addressed to another endpoint"` with `res.Code != protocol.ResultWrongEndpoint`, `res.Detail != "invalid deployment request"` with `res.Code != protocol.ResultInvalidRequest`, and the last check (`[]byte("{")`) with `res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || res.RequestID != ""`.
- `TestDeployerReportsClockSkewAsFailed`: replace `res.Detail != "clock skew exceeds 5 minutes"` with `res.Code != protocol.ResultClockSkew || res.RequestID != req.RequestID`.
- `TestDeployerStartedMarkerSurvivesARestart`: in the started-entry check add `|| !strings.Contains(string(stored), "\"request_id\":\"0123456789abcdef0123456789abcdef\"")`; replace `res.Detail != "the agent restarted after replacement began; inspect the host"` with `res.Code != protocol.ResultRestarted || res.RequestID != req.RequestID`.
- `TestDeployerPruneKeepsAStartedRun`: `d.begin(running)` → `d.begin(running, "")`.
- `TestDeployerReplaysTheLedgerPastTheDeadline`: `newDeployer(context.Background(), dir, &Options{}).begin(req.Deployment)` → `newDeployer(context.Background(), dir, &Options{}).begin(req.Deployment, req.RequestID)`; replace `res.Detail != "the agent restarted after replacement began; inspect the host"` with `res.Code != protocol.ResultRestarted || res.RequestID != req.RequestID`.

Append:

```go
// The request ID is echoed on every answer the deployer sends, run or refused, and logged beside
// the deployment ID at receipt, start and finish; a malformed one is neither echoed nor logged.
func TestDeployerEchoesAndLogsTheRequestID(t *testing.T) {
	var logs strings.Builder
	d := newDeployer(context.Background(), t.TempDir(), &Options{Log: log.New(&logs, "", 0), Deploy: func(_ context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult {
		started()
		// The runtime's result need not carry it: the deployer echoes the frame's.
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}})
	out := make(chan outFrame, 4)
	defer d.attach(context.Background(), out)()
	req := testRequest("ep_1")
	raw, _ := json.Marshal(req)
	d.handleApply(context.Background(), "ep_1", raw, out)
	if res := readResult(t, out); res.Outcome != protocol.OutcomeSucceeded || res.RequestID != req.RequestID || res.Validate() != nil {
		t.Fatalf("result: %+v", res)
	}
	d.wait()
	for _, event := range []string{"received", "started", "finished succeeded"} {
		if want := "deployment " + req.Deployment + " (request " + req.RequestID + "): " + event; !strings.Contains(logs.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, logs.String())
		}
	}
	bad := testRequest("ep_1")
	bad.Deployment, bad.RequestID = "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c", "a b\ninjected"
	raw, _ = json.Marshal(bad)
	d.handleApply(context.Background(), "ep_1", raw, out)
	if res := readResult(t, out); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || res.RequestID != "" {
		t.Fatalf("malformed request id: %+v", res)
	}
	if strings.Contains(logs.String(), "injected") || strings.Contains(logs.String(), bad.Deployment) {
		t.Fatalf("a refused frame was logged:\n%s", logs.String())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/agent/client/ -run 'Deployer|Session'`
Expected: FAIL to compile (`too many arguments in call to d.begin`), then at run the answers still carry sentences and no codes.

- [ ] **Step 3: Implement the deployer**

In `internal/agent/client/deployments.go` replace the `const` block and `deploymentEntry` with:

```go
const (
	deploymentLedgerLife = 24 * time.Hour
	deploymentLedgerMax  = 20
)

type deploymentEntry struct {
	Result   protocol.DeploymentResult `json:"result"`
	Finished time.Time                 `json:"finished"`
	// Started is set, with no result yet, while a run may be changing the host.
	Started time.Time `json:"started,omitzero"`
	// RequestID is the started run's request_id, echoed if a restart settles it.
	RequestID string `json:"request_id,omitempty"`
}
```

Replace `settleStarted`, `begin` and `finish`:

```go
// settleStarted turns every run a restart interrupted after it began changing the host into an
// unknown result coded restarted, re-sent like any other. It reports whether it found one.
func (d *deployer) settleStarted() bool {
	now, found := time.Now().UTC(), false
	for id, e := range d.done {
		if e.pending() {
			d.done[id] = deploymentEntry{Result: protocol.DeploymentResult{Deployment: id, RequestID: e.RequestID, Outcome: protocol.OutcomeUnknown, Code: protocol.ResultRestarted, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: now}
			found = true
		}
	}
	return found
}
```

```go
// begin records durably, before it returns, that run id is about to change the host.
func (d *deployer) begin(id, requestID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done[id] = deploymentEntry{Started: time.Now().UTC(), RequestID: requestID}
	d.logf("deployment %s (request %s): started", id, requestID)
	if err := d.save(); err != nil {
		// A restart before the result would then run the frame again; the recheck still guards it.
		d.logf("deployment %s: start not recorded: %v", id, err)
	}
}

// logf writes one line to the agent's log when it has one. Only validated IDs and closed
// outcomes are passed: the frame itself is never logged.
func (d *deployer) logf(format string, args ...any) {
	if d.opts.Log != nil {
		d.opts.Log.Printf(format, args...)
	}
}
```

```go
// finish records a run's result, frees the slot and returns the session to deliver it to.
// The ledger is written before the slot is released, so no second run can race the file.
func (d *deployer) finish(res protocol.DeploymentResult) *sessionLink {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done[res.Deployment] = deploymentEntry{Result: res, Finished: time.Now().UTC()}
	d.prune()
	if err := d.save(); err != nil {
		// Still delivered and re-sent from memory; only a restart loses it.
		d.logf("deployment %s: result not recorded: %v", res.Deployment, err)
	}
	d.logf("deployment %s (request %s): finished %s", res.Deployment, res.RequestID, res.Outcome)
	d.running = ""
	return d.current
}
```

In `newDeployer`, replace `if err := d.save(); err != nil && d.opts.Log != nil {` / `d.opts.Log.Printf("deployment ledger: interrupted runs not recorded: %v", err)` with:

```go
				if err := d.save(); err != nil {
					d.logf("deployment ledger: interrupted runs not recorded: %v", err)
				}
```

Replace `denied`, `handleApply`, `handleRemoval` and `run` with:

```go
func denied(id, requestID, code string) protocol.DeploymentResult {
	return protocol.DeploymentResult{Deployment: id, RequestID: requestID, Outcome: protocol.OutcomeDenied, Code: code, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
}

// handleApply answers one deployment.apply payload; see run.
func (d *deployer) handleApply(sessionCtx context.Context, endpointID string, payload []byte, out chan<- outFrame) {
	var req protocol.DeploymentRequest
	if json.Unmarshal(payload, &req) != nil {
		go send(sessionCtx, out, resultFrame(denied(req.Deployment, "", protocol.ResultInvalidRequest)))
		return
	}
	var exec func(context.Context) protocol.DeploymentResult
	if deploy := d.opts.Deploy; deploy != nil {
		exec = func(ctx context.Context) protocol.DeploymentResult {
			res := deploy(ctx, req, func() { d.begin(req.Deployment, req.RequestID) })
			for i := range req.Services {
				clear(req.Services[i].Env)
			}
			clear(req.Registries)
			return res
		}
	}
	d.run(sessionCtx, out, req.Deployment, req.RequestID, req.Endpoint, endpointID, req.Validate, exec)
}

// handleRemoval answers one deployment.remove payload; see run.
func (d *deployer) handleRemoval(sessionCtx context.Context, endpointID string, payload []byte, out chan<- outFrame) {
	var req protocol.RemovalRequest
	if json.Unmarshal(payload, &req) != nil {
		go send(sessionCtx, out, resultFrame(denied(req.Deployment, "", protocol.ResultInvalidRequest)))
		return
	}
	var exec func(context.Context) protocol.DeploymentResult
	if remove := d.opts.Remove; remove != nil {
		exec = func(ctx context.Context) protocol.DeploymentResult {
			return remove(ctx, req, func() { d.begin(req.Deployment, req.RequestID) })
		}
	}
	d.run(sessionCtx, out, req.Deployment, req.RequestID, req.Endpoint, endpointID, req.Validate, exec)
}

// run gates and runs one decoded request in the agent's single deployment slot. Refusals are
// sent and not remembered; a run's result is remembered and then sent to the current session.
// Every answer carries requestID when it is valid. run is called on the session loop, which is
// out's only reader, so every send happens off it.
func (d *deployer) run(sessionCtx context.Context, out chan<- outFrame, id, requestID, endpoint, endpointID string, validate func(time.Time) error, exec func(context.Context) protocol.DeploymentResult) {
	// A re-sent frame for the live run: a refusal would settle the row it is still applying,
	// so say nothing and let the real result answer. A remembered result is replayed. Both
	// hold whatever the frame carries, even once its deadline has passed.
	d.mu.Lock()
	live := id != "" && d.running == id
	prior, replay := d.done[id]
	d.mu.Unlock()
	if live {
		return
	}
	if replay {
		go send(sessionCtx, out, resultFrame(prior.Result))
		return
	}
	if err := validate(time.Now()); err != nil {
		echoed := requestID
		if !protocol.ValidRequestID(echoed) {
			echoed = ""
		}
		res := denied(id, echoed, protocol.ResultInvalidRequest)
		if errors.Is(err, protocol.ErrClockSkew) {
			res.Outcome, res.Code = protocol.OutcomeFailed, protocol.ResultClockSkew
		}
		go send(sessionCtx, out, resultFrame(res))
		return
	}
	// Validate accepted requestID: from here it is echoed and safe to log.
	if endpoint != endpointID {
		go send(sessionCtx, out, resultFrame(denied(id, requestID, protocol.ResultWrongEndpoint)))
		return
	}
	d.mu.Lock()
	prior, replay = d.done[id]
	running := d.running
	if !replay && exec != nil && running == "" {
		d.running = id
	}
	d.mu.Unlock()
	switch {
	case replay:
		go send(sessionCtx, out, resultFrame(prior.Result))
		return
	case exec == nil:
		// The server sends no frame this agent did not advertise a runtime for.
		go send(sessionCtx, out, resultFrame(denied(id, requestID, protocol.ResultInvalidRequest)))
		return
	case running == id:
		return
	case running != "":
		go send(sessionCtx, out, resultFrame(denied(id, requestID, protocol.ResultBusy)))
		return
	}
	d.logf("deployment %s (request %s): received", id, requestID)
	d.runs.Add(1)
	go func() {
		res := exec(d.root)
		if res.Deployment == "" {
			res.Deployment = id
		}
		res.RequestID = requestID
		// Never record or send what the server would refuse to read: the host may have acted.
		if res.Deployment != id || res.Validate() != nil {
			res = protocol.DeploymentResult{Deployment: id, RequestID: requestID, Outcome: protocol.OutcomeUnknown, Code: protocol.ResultUnreadable, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
		}
		link := d.finish(res)
		d.runs.Done()
		if link != nil {
			send(link.ctx, link.out, resultFrame(res))
		}
	}()
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `gofmt -w internal/agent/client && go vet ./internal/agent/client/ && go test -race -count=1 ./internal/agent/... ./cmd/agent/`
Expected: PASS.

- [ ] **Step 5: DOX and commit**

`internal/agent/AGENTS.md`, the `deployer` bullet: replace "Everything below applies to both kinds; a busy refusal reads "already applying a deployment" for either, and the no-runtime refusal names deploy or remove." with "Everything below applies to both kinds. Every answer carries the frame's `request_id` when `ValidRequestID` accepts it (else empty) and a result code, never a sentence: a frame that does not decode or fails `Validate`, and a frame for a kind this agent has no runtime for, `denied` `invalid_request`; clock skew `failed` `clock_skew`; another endpoint's `denied` `wrong_endpoint`; a second run `denied` `busy`. After `Validate` accepts a frame the deployer logs `deployment <id> (request <request_id>): received`, then `started` from the `started` hook and `finished <outcome>` when the result is recorded; the request ID is echoed on the run's result whatever the runtime returned." Replace "is replaced by `unknown` "the runtime returned an unreadable result" before it is recorded" with "is replaced by `unknown` `unreadable` before it is recorded"; replace "The runtime's `started` hook makes the deployer write `{started}` for the ID durably" with "The runtime's `started` hook makes the deployer write `{started, request_id}` for the ID durably"; replace "becomes `unknown` `the agent restarted after replacement began; inspect the host` and is re-sent" with "becomes `unknown` `restarted`, carrying the recorded request ID, and is re-sent".

```bash
gofmt -l internal cmd
git add internal/agent
git commit -m "feat(agent): answer deployments with result codes and echo the request ID" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Store — correlation IDs, request-ID check, legacy results, no free result detail

**Files:**
- Modify: `internal/store/migrations/migrations.go` (migration 29)
- Modify: `internal/store/store.go` (`ErrUnreadableResult`)
- Modify: `internal/store/application_deployment.go` (`Deployment.CorrelationID`, `storedDeploymentResult.Code`, `legacyResult`, `PlanDeployment`, `draftPlan`, `insertPlan`, `selectDeployments`, `scanDeployment`)
- Modify: `internal/store/application_apply.go` (`ApplyDeployment`, `buildDeploymentFrame`, `systemTransition`, `auditDeployment`, `SettleDeployment`, `RefuseDeploymentResult`, `RemoveApplication`)
- Modify: `internal/agent/protocol/deployment.go` (delete `DeploymentResult.Detail`)
- Modify: `internal/api/agent_connect.go` (the `deployment.result` case)
- Create: `internal/store/deployment_correlation_test.go`
- Test: `internal/store/application_apply_test.go`, `internal/store/application_history_test.go`, `internal/store/tenancy_test.go`, `internal/agent/protocol/deployment_test.go`, `internal/api/deployment_apply_test.go`, `internal/api/deployment_removal_test.go`
- Docs: `internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes (Tasks 1–3): `protocol.CodeLegacy`, `ResultStepFailed`, `DeploymentResult.RequestID`/`Code`, `DeploymentStep.Code`, `DeploymentRequest.RequestID`, `RemovalRequest.RequestID`.
- Produces:
  ```go
  // package store
  var ErrUnreadableResult = errors.New("unreadable deployment result")
  // Deployment.CorrelationID string `json:"correlation_id"`
  func legacyResult(res protocol.DeploymentResult) protocol.DeploymentResult
  func (t *tenancyStore) auditDeployment(ctx context.Context, tx *sql.Tx, user string, action permissions.Action, org, env, appID, id, correlation, outcome, result string, at time.Time) error
  ```
  `SettleDeployment` returns `ErrUnreadableResult` for a result that fails `Validate` after `legacyResult` (nothing written), `ErrInvalid` for a foreign `RequestID`. `protocol.DeploymentResult` has no `Detail` field any more.

- [ ] **Step 1: Move the fixtures to codes and the plan's request ID**

`internal/store/application_apply_test.go`, replace `settledResult`:

```go
func settledResult(d *Deployment, outcome string, newID string) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: outcome, Steps: []protocol.DeploymentStep{{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}}, Services: []protocol.DeploymentIdentity{}}
	if newID != "" {
		res.Services = append(res.Services, protocol.DeploymentIdentity{Service: "web", ContainerID: newID, ImageID: d.Plan.Services[0].ImageID, CreatedUnix: 1800000000})
	}
	if outcome != protocol.OutcomeSucceeded {
		res.Code = protocol.ResultStepFailed
		res.Steps[0].Outcome, res.Steps[0].Code = outcome, "runtime_error"
	}
	return res
}
```

and in `TestSettleDeploymentRefusesOversizedResult` replace the comment and loop with:

```go
	// Each parameter is twelve unsupported codes (148 bytes), under the per-step cap, so the result
	// validates and only the stored byte cap refuses it.
	detail := strings.Join(protocol.UnsupportedCodes[:12], ",")
	for len(res.Steps) < 8*protocol.MaxDeploymentServices {
		res.Steps = append(res.Steps, protocol.DeploymentStep{Service: "web", Step: protocol.StepStart, Outcome: protocol.OutcomeFailed, Code: "unsupported", Detail: detail})
	}
```

`internal/store/application_history_test.go`, replace `removalSteps` and `removalResult`:

```go
func removalSteps(service string, outcomes ...string) []protocol.DeploymentStep {
	out := []protocol.DeploymentStep{}
	for i, step := range []string{protocol.StepPrecondition, protocol.StepStop, protocol.StepRemove} {
		s := protocol.DeploymentStep{Service: service, Step: step, Outcome: outcomes[i]}
		switch s.Outcome {
		case protocol.OutcomeDenied:
			s.Code = "identity_mismatch"
		case protocol.OutcomeFailed, protocol.OutcomeTimedOut, protocol.OutcomeUnknown:
			s.Code = "runtime_error"
		}
		out = append(out, s)
	}
	return out
}

func removalResult(d *Deployment, outcome string, steps ...[]protocol.DeploymentStep) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: outcome, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if outcome != protocol.OutcomeSucceeded {
		res.Code = protocol.ResultStepFailed
	}
	for _, s := range steps {
		res.Steps = append(res.Steps, s...)
	}
	return res
}
```

and in `TestRemovalPartialSettle` delete the line `res.Detail = "fixed text"`.

`internal/store/deployment_test.go`, `TestFrameBlocker`: add `RequestID: "0123456789abcdef0123456789abcdef",` after `Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b",` in the `good` literal (a frame without one is now `frame_invalid`).

`internal/store/tenancy_test.go`, `TestTenancyUpgradeAndReopen`: the replayed migrations now include 29 (it alters `deployments`, which the test drops): change `DELETE FROM schema_migrations WHERE version IN (5,7,8,9,10,11,12,13,14,15,16,17,18,20,21,22,23,24,25,26,27,28)` to `DELETE FROM schema_migrations WHERE version IN (5,7,8,9,10,11,12,13,14,15,16,17,18,20,21,22,23,24,25,26,27,28,29)`.

`internal/agent/protocol/deployment_test.go`: delete the `"free result detail"` entry from `TestDeploymentResultValidation` and append:

```go
// The result's free detail is gone from the wire: no sentence travels beside the code, and an
// older agent's is dropped on decode.
func TestDeploymentResultCarriesNoFreeDetail(t *testing.T) {
	r := goodResult()
	r.Steps = []DeploymentStep{}
	if raw, _ := json.Marshal(r); strings.Contains(string(raw), `"detail"`) {
		t.Fatalf("detail on the wire: %s", raw)
	}
	var older DeploymentResult
	if err := json.Unmarshal([]byte(`{"deployment":"3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b","outcome":"failed","detail":"service web, step create: failed","steps":[],"services":[]}`), &older); err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(older); strings.Contains(string(raw), "service web") {
		t.Fatalf("an older agent's sentence survived: %s", raw)
	}
}
```

- [ ] **Step 2: Write the failing store tests**

Create `internal/store/deployment_correlation_test.go`:

```go
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

// correlationRows counts the audit rows carrying one correlation ID.
func correlationRows(t *testing.T, st *SQLStore, correlation string) (n int) {
	t.Helper()
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE correlation_id=?`), correlation).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func deploymentRow(t *testing.T, st *SQLStore, id string) (state, result string) {
	t.Helper()
	if err := st.db.QueryRow(st.rebind(`SELECT state,result FROM deployments WHERE id=?`), id).Scan(&state, &result); err != nil {
		t.Fatal(err)
	}
	return state, result
}

// The plan's request ID is the deployment's correlation ID: stored on the row, sent in the frame,
// and on the plan's, the apply's and the settle's audit rows, never the apply request's own.
func TestDeploymentCorrelationFollowsThePlan(t *testing.T) {
	st, a, app, endpoint, _, m, _, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	plan := a
	plan.CorrelationID = "plan-request"
	d, err := ts.PlanDeployment(ctx, plan, app.ID, planRequest(m), nil, key, false)
	if err != nil || d.CorrelationID != "plan-request" {
		t.Fatalf("plan: %+v %v", d, err)
	}
	var stored string
	if err := st.db.QueryRow(st.rebind(`SELECT correlation_id FROM deployments WHERE id=?`), d.ID).Scan(&stored); err != nil || stored != "plan-request" {
		t.Fatalf("row: %q %v", stored, err)
	}
	apply := a
	apply.CorrelationID = "apply-request"
	applied, req, err := ts.ApplyDeployment(ctx, apply, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes)
	if err != nil || req.RequestID != "plan-request" || applied.CorrelationID != "plan-request" {
		t.Fatalf("apply: %+v %+v %v", applied, req, err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(applied, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); err != nil {
		t.Fatal(err)
	}
	if n := correlationRows(t, st, "plan-request"); n != 3 {
		t.Fatalf("plan, apply and settle audit rows under the plan's ID: %d", n)
	}
	if n := correlationRows(t, st, "apply-request"); n != 0 {
		t.Fatalf("rows under the apply request's own ID: %d", n)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.CorrelationID != "plan-request" {
		t.Fatalf("read: %+v %v", got, err)
	}
}

// Abandoning, refusing and failing a deployment audit under its plan's correlation ID too.
func TestSystemTransitionsCarryThePlanCorrelation(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if d.CorrelationID != "test-request" {
		t.Fatalf("the fixture plans under %q", d.CorrelationID)
	}
	apply := a
	apply.CorrelationID = "apply-request"
	if _, _, err := ts.ApplyDeployment(ctx, apply, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := ts.RefuseDeploymentResult(ctx, endpoint, d.ID, "the host's result did not match the plan; inspect the host"); err != nil {
		t.Fatal(err)
	}
	if err := ts.FailDeployment(ctx, d.ID, "the endpoint disconnected before the deployment was sent"); err != nil {
		t.Fatal(err)
	}
	for _, details := range []string{"outcome=abandoned", "outcome=refused", "outcome=not_sent"} {
		var n int
		if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE correlation_id=? AND details=?`), "test-request", details).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: %d %v", details, n, err)
		}
	}
}

// Without a request ID the plan mints one, and its audit row carries the same.
func TestPlanMintsACorrelationIDWhenNoneIsGiven(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	a.CorrelationID = ""
	d, err := st.Tenancy().PlanDeployment(context.Background(), a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := uuid.Parse(d.CorrelationID); perr != nil {
		t.Fatalf("correlation %q: %v", d.CorrelationID, perr)
	}
	if n := correlationRows(t, st, d.CorrelationID); n != 1 {
		t.Fatalf("plan audit rows: %d", n)
	}
}

// A removal is its own plan: the frame, the row and the settle's audit row share its request ID.
func TestRemovalCarriesTheCorrelationID(t *testing.T) {
	st, a, app, endpoint, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	a.CorrelationID = "remove-request"
	d, req, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop"})
	if err != nil || d.CorrelationID != "remove-request" || req.RequestID != "remove-request" {
		t.Fatalf("removal: %+v %+v %v", d, req, err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, removalResult(d, protocol.OutcomeSucceeded, removalSteps("web", protocol.OutcomeSucceeded, protocol.OutcomeSucceeded, protocol.OutcomeSucceeded))); err != nil {
		t.Fatal(err)
	}
	if n := correlationRows(t, st, "remove-request"); n != 2 {
		t.Fatalf("removal and settle audit rows: %d", n)
	}
}

// A result carrying another request ID never settles the row: here the ID of the plan this one
// replaced (Review Focus 2).
func TestSettleRefusesAForeignRequestID(t *testing.T) {
	st, a, app, endpoint, _, m, _, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	first := a
	first.CorrelationID = "req-one"
	if _, err := ts.PlanDeployment(ctx, first, app.ID, planRequest(m), nil, key, false); err != nil {
		t.Fatal(err)
	}
	second := a
	second.CorrelationID = "req-two"
	d, err := ts.PlanDeployment(ctx, second, app.ID, planRequest(m), nil, key, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	res := settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))
	res.RequestID = "req-one"
	if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign request id: %v", err)
	}
	if state, result := deploymentRow(t, st, d.ID); state != "applying" || result != "" {
		t.Fatalf("a foreign result moved the row: %s %q", state, result)
	}
	res.RequestID = "req-two"
	if err := ts.SettleDeployment(ctx, endpoint, res); err != nil {
		t.Fatal(err)
	}
}

// An older binary's result has no request ID and no codes: each missing code reads as legacy and
// its sentence is dropped, so the in-flight deployment still settles (Review Focus 5).
func TestSettleReadsALegacyResult(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: d.ID, Outcome: protocol.OutcomeDenied, Steps: []protocol.DeploymentStep{
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeDenied, Detail: "the container no longer exists"},
		{Service: "web", Step: protocol.StepImage, Outcome: protocol.OutcomeSkipped},
	}, Services: []protocol.DeploymentIdentity{}}
	if err := ts.SettleDeployment(ctx, endpoint, res); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.State != "denied" || got.Detail != "" || got.Result == nil || got.Result.Code != protocol.CodeLegacy {
		t.Fatalf("settled: %+v %v", got, err)
	}
	if s := got.Result.Steps[0]; s.Code != protocol.CodeLegacy || s.Detail != "" || got.Result.Steps[1].Code != "" {
		t.Fatalf("steps: %+v", got.Result.Steps)
	}
	if _, result := deploymentRow(t, st, d.ID); strings.Contains(result, "no longer exists") {
		t.Fatalf("a sentence was stored: %s", result)
	}
}

// A current agent (the result carries a request ID) must code a failing step: a denied step
// without one is unreadable and nothing is stored (Review Focus 5).
func TestSettleRefusesACodelessStepFromACurrentAgent(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeDenied, Code: protocol.ResultStepFailed, Steps: []protocol.DeploymentStep{
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeDenied},
	}, Services: []protocol.DeploymentIdentity{}}
	if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrUnreadableResult) {
		t.Fatalf("codeless step: %v", err)
	}
	if state, result := deploymentRow(t, st, d.ID); state != "applying" || result != "" {
		t.Fatalf("stored: %s %q", state, result)
	}
}

// A code whose parameter has the wrong shape is refused before anything is stored (Review Focus 1).
func TestSettleRefusesAMalformedCodeParameter(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	for _, step := range []protocol.DeploymentStep{
		{Service: "web", Step: protocol.StepStop, Outcome: protocol.OutcomeFailed, Code: "runtime_status", Detail: "abc"},
		{Service: "web", Step: protocol.StepStart, Outcome: protocol.OutcomeFailed, Code: "identity_unverified", Detail: strings.Repeat("e", 63)},
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeDenied, Code: "unsupported", Detail: "privileged,privileged"},
	} {
		for _, requestID := range []string{d.CorrelationID, ""} {
			res := protocol.DeploymentResult{Deployment: d.ID, RequestID: requestID, Outcome: step.Outcome, Code: protocol.ResultStepFailed, Steps: []protocol.DeploymentStep{step}, Services: []protocol.DeploymentIdentity{}}
			if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrUnreadableResult) {
				t.Fatalf("%s %q (request %q): %v", step.Code, step.Detail, requestID, err)
			}
		}
	}
	if state, result := deploymentRow(t, st, d.ID); state != "applying" || result != "" {
		t.Fatalf("stored: %s %q", state, result)
	}
}

// A result stored before codes reads back with legacy codes and no sentence.
func TestReadDeploymentNormalisesAStoredLegacyResult(t *testing.T) {
	st, a, app, _, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	old := `{"steps":[{"service":"web","step":"create","outcome":"failed","detail":"the runtime refused with status 500"}],"services":[]}`
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET state='failed',detail=?,result=?,settled_at=? WHERE id=?`), "service web, step create: the runtime refused with status 500", old, time.Now().UTC(), d.ID); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.Result == nil || got.Result.Code != protocol.CodeLegacy || len(got.Result.Steps) != 1 || got.Result.Steps[0].Code != protocol.CodeLegacy || got.Result.Steps[0].Detail != "" {
		t.Fatalf("read: %+v %v", got, err)
	}
}
```

- [ ] **Step 3: Run the store tests to verify they fail**

Run: `go test -count=1 ./internal/store/ -run 'Correlation|Settle|Legacy|Removal|Oversized'`
Expected: FAIL to compile (`d.CorrelationID undefined`, `undefined: ErrUnreadableResult`).

- [ ] **Step 4: Implement the store**

`internal/store/migrations/migrations.go`, append to `registry` after version 28:

```go
	{Version: 29, Name: "deployment_correlation", SQLite: `ALTER TABLE deployments ADD COLUMN correlation_id TEXT NOT NULL DEFAULT '';`, Postgres: `ALTER TABLE deployments ADD COLUMN correlation_id TEXT NOT NULL DEFAULT '';`},
```

`internal/store/store.go`, add to the error `var` block after `ErrMappingRequired`:

```go
	// ErrUnreadableResult is a deployment result that fails validation even read as an older
	// agent's: nothing is stored, and the row waits for another answer or the sweep.
	ErrUnreadableResult = errors.New("unreadable deployment result")
```

`internal/store/application_deployment.go`:

In `Deployment`, add after `Detail`:

```go
	// CorrelationID is the plan's request ID: the frame and every audit row of the deployment carry it.
	CorrelationID string `json:"correlation_id"`
```

Replace `storedDeploymentResult` with:

```go
// storedDeploymentResult is the shape kept in the result column: the result code and the
// step-by-step record, codes and parameters only. The outcome lives in the row's state.
type storedDeploymentResult struct {
	Code     string                        `json:"code,omitempty"`
	Steps    []protocol.DeploymentStep     `json:"steps"`
	Services []protocol.DeploymentIdentity `json:"services"`
}

// legacyResult reads a result produced by a binary built before outcome codes: it carries no
// request ID, and a step or result that did not succeed carries no code. Each missing code becomes
// protocol.CodeLegacy and its free text is dropped. A result with a request ID is a current
// agent's and is taken as sent.
func legacyResult(res protocol.DeploymentResult) protocol.DeploymentResult {
	if res.RequestID != "" {
		return res
	}
	res.Steps = slices.Clone(res.Steps)
	for i, s := range res.Steps {
		if s.Code == "" && s.Outcome != protocol.OutcomeSucceeded && s.Outcome != protocol.OutcomeSkipped {
			res.Steps[i].Code, res.Steps[i].Detail = protocol.CodeLegacy, ""
		}
	}
	if res.Code == "" && res.Outcome != protocol.OutcomeSucceeded {
		res.Code = protocol.CodeLegacy
	}
	return res
}
```

In `PlanDeployment`, insert before `planID := uuid.NewString()`:

```go
	// The plan's request ID is the deployment's correlation ID from here on.
	if a.CorrelationID == "" {
		a.CorrelationID = uuid.NewString()
	}
```

In `draftPlan`, add `CorrelationID: a.CorrelationID` to the `&Deployment{…}` literal.

In `insertPlan`, replace the `INSERT` with:

```go
	_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at,correlation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), d.ID, a.OrganizationID, a.EnvironmentID, d.ApplicationID, d.InstanceID, d.EndpointID, d.Plan.Project, d.State, d.Revision, d.SpecDigest, d.MappingVersion, string(raw), d.CreatedBy, d.CreatedAt, d.ExpiresAt, d.CorrelationID)
```

Replace `selectDeployments` and the scan/result part of `scanDeployment`:

```go
const selectDeployments = `SELECT d.id,d.application_id,d.instance_id,d.endpoint_id,COALESCE(e.name,''),d.kind,d.state,d.revision,d.spec_digest,d.mapping_version,d.plan,d.created_by,d.created_at,d.expires_at,d.applied_by,d.applied_at,d.deadline,d.settled_at,d.detail,d.result,d.correlation_id FROM deployments d LEFT JOIN endpoints e ON e.id=d.endpoint_id `
```

```go
	if err := rows.Scan(&d.ID, &d.ApplicationID, &d.InstanceID, &d.EndpointID, &d.EndpointName, &d.Kind, &d.State, &d.Revision, &d.SpecDigest, &d.MappingVersion, &raw, &d.CreatedBy, &d.CreatedAt, &d.ExpiresAt, &d.AppliedBy, &appliedAt, &deadline, &settledAt, &d.Detail, &result, &d.CorrelationID); err != nil {
		return nil, err
	}
```

```go
	if result != "" {
		var stored storedDeploymentResult
		if json.Unmarshal([]byte(result), &stored) != nil {
			return nil, ErrRevisionCorrupt
		}
		// A result stored before codes reads as legacy.
		res := legacyResult(protocol.DeploymentResult{Deployment: d.ID, Outcome: d.State, Code: stored.Code, Steps: stored.Steps, Services: stored.Services})
		d.Result = &res
	}
```

`internal/store/application_apply.go`:

In `ApplyDeployment`, insert after the `planID` parse:

```go
	// Every audit row of a deployment carries its plan's correlation ID, the apply's included.
	// A row's ID never changes, so the transaction need not re-read it.
	var correlation string
	if t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT correlation_id FROM deployments WHERE organization_id=? AND environment_id=? AND application_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), planID.String()).Scan(&correlation) == nil && correlation != "" {
		a.CorrelationID = correlation
	}
```

In `buildDeploymentFrame`, insert before `names := map[string]string{}`:

```go
	// A plan saved before correlation IDs has no request ID to send: plan again.
	if d.CorrelationID == "" {
		return protocol.DeploymentRequest{}, ErrAdoptionChanged
	}
```

and set `RequestID: d.CorrelationID` in the `req := protocol.DeploymentRequest{…}` literal.

In `systemTransition`, change the row type, select and scan:

```go
	type row struct{ id, org, env, app, kind, correlation string }
	var affected []row
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,organization_id,environment_id,application_id,kind,correlation_id FROM deployments WHERE `+filter), arg)
```

```go
		if err := rows.Scan(&r.id, &r.org, &r.env, &r.app, &r.kind, &r.correlation); err != nil {
```

```go
		if err := t.auditDeployment(ctx, tx, "system", action, r.org, r.env, r.app, r.id, r.correlation, outcome, result, now); err != nil {
```

Replace `auditDeployment`:

```go
// auditDeployment writes the audit row for a deployment transition inside its transaction, under
// the deployment's correlation ID (a fresh one for a row planned before correlation IDs).
func (t *tenancyStore) auditDeployment(ctx context.Context, tx *sql.Tx, user string, action permissions.Action, org, env, appID, id, correlation, outcome, result string, at time.Time) error {
	if correlation == "" {
		correlation = uuid.NewString()
	}
	_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), user, string(action), protocol.CleanText(appID+"/deployments/"+id, 255), "outcome="+outcome, at, "organization", org, env, correlation, result)
	return err
}
```

In `SettleDeployment`, replace the doc comment's last sentence ("Anything else is ErrNotFound.") with "Anything else is ErrNotFound. A result from a binary built before codes is read through legacyResult; one that still fails validation is ErrUnreadableResult with nothing written, and one echoing another deployment's request ID is ErrInvalid.", and replace its opening through the plan decode with:

```go
func (t *tenancyStore) SettleDeployment(ctx context.Context, endpointID string, res protocol.DeploymentResult) error {
	res = legacyResult(res)
	if res.Validate() != nil {
		return ErrUnreadableResult
	}
	raw, err := json.Marshal(storedDeploymentResult{Code: res.Code, Steps: res.Steps, Services: res.Services})
	if err != nil || len(raw) > MaxDeploymentResultStoredBytes {
		return ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lock, err := t.lockDeploymentApplication(ctx, tx, endpointID, res.Deployment)
	if err != nil {
		return err
	}
	var org, env, appID, instance, kind, planRaw, correlation string
	var revision int
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT d.organization_id,d.environment_id,d.application_id,d.instance_id,d.kind,d.revision,d.plan,d.correlation_id FROM deployments d WHERE d.id=? AND d.endpoint_id=? AND `+settleable+lock), res.Deployment, endpointID).Scan(&org, &env, &appID, &instance, &kind, &revision, &planRaw, &correlation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// A current agent echoes the frame's request ID; another deployment's is a replay or a bug.
	if res.RequestID != "" && res.RequestID != correlation {
		return ErrInvalid
	}
	var plan DeploymentPlan
	if json.Unmarshal([]byte(planRaw), &plan) != nil {
		return ErrRevisionCorrupt
	}
```

Further down in `SettleDeployment`, replace the state update and the audit call:

```go
	updated, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state=?,detail='',result=?,settled_at=? WHERE id=? AND state IN ('applying','unknown')`), res.Outcome, string(raw), now, res.Deployment)
```

```go
	if err := t.auditDeployment(ctx, tx, "agent:"+endpointID, action, org, env, appID, res.Deployment, correlation, res.Outcome, auditResults[res.Outcome], now); err != nil {
```

In `RefuseDeploymentResult`, select and pass the correlation:

```go
	var org, env, appID, kind, correlation string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT d.organization_id,d.environment_id,d.application_id,d.kind,d.correlation_id FROM deployments d WHERE d.id=? AND d.endpoint_id=? AND `+settleable+lock), id, endpointID).Scan(&org, &env, &appID, &kind, &correlation)
```

```go
	if err := t.auditDeployment(ctx, tx, "agent:"+endpointID, action, org, env, appID, id, correlation, "refused", auditResults[protocol.OutcomeUnknown], now); err != nil {
```

In `RemoveApplication`, insert before `id := uuid.NewString()`:

```go
	// The removal's request ID is its correlation ID, as a plan's is.
	if a.CorrelationID == "" {
		a.CorrelationID = uuid.NewString()
	}
```

set `RequestID: a.CorrelationID` in the `req = &protocol.RemovalRequest{…}` literal, add `CorrelationID: a.CorrelationID` to the `out = &Deployment{…}` literal, and replace its `INSERT` with:

```go
			_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,kind,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at,applied_by,applied_at,deadline,correlation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), out.ID, a.OrganizationID, a.EnvironmentID, out.ApplicationID, out.InstanceID, out.EndpointID, project, out.Kind, out.State, out.Revision, out.SpecDigest, out.MappingVersion, string(raw), out.CreatedBy, now, out.ExpiresAt, out.AppliedBy, now, req.Deadline, out.CorrelationID)
```

`internal/agent/protocol/deployment.go`: delete the `Detail` field and its comment from `DeploymentResult`, and remove `r.Detail != "" ||` from the first check of `DeploymentResult.Validate`.

`internal/api/agent_connect.go`, in the `protocol.TypeDeploymentResult` case replace from the `// The size is already bounded.` comment through the `SettleDeployment` switch's first two cases with:

```go
		switch err := ts.SettleDeployment(fctx, c.endpointID, res); {
		case err == nil:
		case errors.Is(err, store.ErrUnreadableResult):
			// The size is already bounded. Closing would only make the agent re-send the same
			// result on reconnect, so drop it; the row stays until the deadline sweep.
			log.Printf("agent %s: dropped an unreadable deployment result", c.endpointID)
		case errors.Is(err, store.ErrNotFound):
```

(the `ErrNotFound`, `ErrInvalid`/`ErrAdoptionChanged` and default cases stay as they are; the store now normalises an older agent's result before validating it, so the handler no longer calls `res.Validate()` itself).

- [ ] **Step 5: Run the store suite on both drivers**

Run: `gofmt -w internal/store internal/agent/protocol internal/api && go build ./... && go test -race -count=1 ./internal/store/... ./internal/agent/... ./internal/runtime/docker/ && PG=… go test -count=1 ./internal/store/...`
Expected: PASS on SQLite and PostgreSQL.

- [ ] **Step 6: Write the API assertions**

`internal/api/deployment_apply_test.go`, `TestApplyDeploymentOverTheAgentSocket`: replace `planned := plan()` (the first one, before `apply := deployments + "/" + planned.ID + "/apply"`) with:

```go
	// The plan's request ID becomes the deployment's correlation ID.
	planResp := tenantRequest(s, admin, "POST", deployments, string(planBody), true)
	var planned store.Deployment
	if planResp.Code != 201 || json.Unmarshal(planResp.Body.Bytes(), &planned) != nil {
		t.Fatalf("plan: %d %s", planResp.Code, planResp.Body.String())
	}
	if planned.CorrelationID == "" || planned.CorrelationID != planResp.Header().Get("X-Request-ID") {
		t.Fatalf("correlation %q, request %q", planned.CorrelationID, planResp.Header().Get("X-Request-ID"))
	}
```

After the line that decodes the 202 apply response into `applying`, extend its check to `if applying.State != "applying" || applying.ID != planned.ID || applying.CorrelationID != planned.CorrelationID {`; after `must(json.Unmarshal(frame.Payload, &sent))` extend the frame check with `|| sent.RequestID != planned.CorrelationID`.

Replace the `big` fixture with a result that validates under codes:

```go
	big := protocol.DeploymentResult{Deployment: planned.ID, RequestID: planned.CorrelationID, Outcome: protocol.OutcomeFailed, Code: protocol.ResultStepFailed, Steps: make([]protocol.DeploymentStep, 800), Services: []protocol.DeploymentIdentity{}}
	for i := range big.Steps {
		big.Steps[i] = protocol.DeploymentStep{Service: strings.Repeat("w", 63), Step: protocol.StepCreate, Outcome: protocol.OutcomeFailed, Code: "identity_unverified", Detail: strings.Repeat("d", 64)}
	}
```

After the `if refused != 1 {…}` block add:

```go
	// The apply's audit row carries the deployment's correlation ID, not the apply request's.
	applied := 0
	for _, rec := range records {
		if rec.Resource == app.ID+"/deployments/"+planned.ID+"/apply" && rec.Result == "success" && rec.CorrelationID == planned.CorrelationID {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("apply audit rows under the plan's correlation ID: %d", applied)
	}
```

`internal/api/deployment_removal_test.go`, `TestRemovalOverTheAgentSocket`: replace `removing, _ := remove()` and the result written after it with:

```go
	removing, removingFrame := remove()
	if removingFrame.RequestID == "" || removingFrame.RequestID != removing.CorrelationID {
		t.Fatalf("the removal frame's request id %q, row %q", removingFrame.RequestID, removing.CorrelationID)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{
		Deployment: removing.ID, RequestID: removingFrame.RequestID, Outcome: protocol.OutcomeSucceeded, Steps: steps, Services: []protocol.DeploymentIdentity{},
	})
```

- [ ] **Step 7: Run the API and store suites on both drivers**

Run: `gofmt -w internal/api && go test -race -count=1 ./internal/api/ ./internal/store/... && PG=… go test -count=1 ./internal/api/ ./internal/store/...`
Expected: PASS. With Docker available also `KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 go test -race -count=1 ./internal/api -run '^TestApplyRealDocker$'`: the real agent echoes the request ID and the apply and removal settle.

- [ ] **Step 8: DOX and commit**

`internal/store/AGENTS.md`: add a bullet after the migration 25 bullet: "Migration 29 adds `deployments.correlation_id` (`TEXT NOT NULL DEFAULT ''`). `PlanDeployment` and `RemoveApplication` store the request's `CorrelationID` (the API's `X-Request-ID`; minted as a UUID when empty, before the transaction, so the plan's audit row and the row agree) and send it as the frame's `request_id`; `buildDeploymentFrame` refuses a row without one (`ErrAdoptionChanged`: planned before the upgrade, plan again). `ApplyDeployment` reads the row's ID (scoped to organization, environment and application) and audits under it, so the apply request's own `X-Request-ID` names no row; `auditDeployment` takes the correlation ID, and `SettleDeployment`, `RefuseDeploymentResult` and every `systemTransition` (abandon, sweep, not sent) pass the row's (a fresh UUID for a pre-29 row). `Deployment.CorrelationID` (`correlation_id`) is on every deployment read. `SettleDeployment` first reads the result through `legacyResult` (only a result with no `request_id`, from a binary built before codes: each missing step or result code becomes `legacy`, its text dropped), refuses one that still fails `Validate` with `ErrUnreadableResult` (nothing written), and refuses a non-empty `request_id` other than the row's with `ErrInvalid`. The result column holds `{code, steps, services}` (codes and parameters only) and settling clears the row's `detail`; `scanDeployment` reads a pre-29 stored result through `legacyResult`." In the `SettleDeployment` sentence of the `ApplyDeployment` bullet, replace "stores the steps (byte cap" with "stores the result code and steps (byte cap".

`internal/api/AGENTS.md`, the `deployment.result` bullet: replace "One that decodes but fails `Validate` is dropped with a log line naming only the endpoint; the socket stays. A valid frame is passed to `SettleDeployment`, scoped to the sending endpoint:" with "Every decoded frame is passed to `SettleDeployment`, scoped to the sending endpoint, which reads an older agent's code-less result as `legacy` and validates it: `ErrUnreadableResult` is dropped with a log line naming only the endpoint and the socket stays;"; append to the bullet "A result echoing another deployment's `request_id` is `ErrInvalid` and takes the refusal path." In the `POST /applications/{application}/deployments` bullet add after "mints a `store.Deployment` (201)": " whose `correlation_id` is the request's `X-Request-ID`".

`internal/agent/AGENTS.md`: in the protocol bullet replace "a result's `Code` is set exactly" with "`DeploymentResult` has no free `Detail`; a result's `Code` is set exactly".

```bash
gofmt -l internal cmd
git add internal/store internal/agent/protocol internal/api
git commit -m "feat(store): correlate a deployment's audit rows and read older results as legacy" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Store — lowercase unique usernames, the first admin's row lock, two-character volume names

**Files:**
- Modify: `internal/store/migrations/migrations.go` (`Migration.Check`, the check in `Run`, migration 30, `refuseCaseVariantUsernames`)
- Modify: `internal/store/tenancy.go` (`CreateOrganizationWithAdmin`)
- Modify: `internal/api/admin_handlers.go:150` (comment only)
- Modify: `internal/store/application_spec.go:76` and `:142` (`applicationVolumeName`, `ValidVolumeName` comment)
- Modify: `internal/applications/compose.go:86` (refusal text)
- Modify: `internal/agent/protocol/deployment.go` (`deploymentVolume`)
- Create: `internal/store/username_index_test.go`, `internal/store/tenancy_lock_test.go`
- Test: `internal/store/application_spec_test.go`, `internal/applications/compose_test.go`, `internal/agent/protocol/deployment_test.go`
- Docs: `internal/store/AGENTS.md`, `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  ```go
  // package migrations
  type Migration struct {
  	Version  int
  	Name     string
  	SQLite   string
  	Postgres string
  	Check    func(context.Context, *sql.Tx) error // runs before the SQL; an error refuses the migration
  }
  ```
  `users` gains `idx_users_username_lower`; `CreateUser` keeps mapping a unique violation (now of either index) to `ErrAlreadyExists`, which Task 6's SSO paths rely on.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/username_index_test.go`:

```go
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store/migrations"
)

// Migration 30 refuses a database whose usernames differ only by case: it names every one exactly
// once, grouped, and alters nothing. Once they are resolved it creates the index, which closes the
// race the admin pre-check leaves (Review Focus 3).
func TestMigrationRefusesCaseVariantUsernames(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	// Back to a database migrated to 29.
	for _, q := range []string{`DROP INDEX idx_users_username_lower`, `DELETE FROM schema_migrations WHERE version=30`} {
		if _, err := st.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Add(-time.Hour)
	add := func(i int, id, name string) {
		t.Helper()
		if err := st.Users().CreateUser(ctx, &User{ID: id, Username: name, Role: "user", Status: "active", SSOProvider: "local", CreatedAt: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(want string, users int) {
		t.Helper()
		err := migrations.Run(ctx, st.db, st.Driver())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q, got %v", want, err)
		}
		var recorded, n int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=30`).Scan(&recorded); err != nil || recorded != 0 {
			t.Fatalf("a refused migration was recorded: %d %v", recorded, err)
		}
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil || n != users {
			t.Fatalf("users after the refusal: %d %v", n, err)
		}
	}
	add(1, "u1", "erin")
	add(2, "u2", "Erin")
	refused("usernames differ only by case: erin, Erin; rename or delete one of each pair before upgrading", 3)
	add(3, "u3", "ERIN")
	add(4, "u4", "bob")
	add(5, "u5", "Bob")
	refused("usernames differ only by case: bob, Bob; erin, Erin, ERIN; rename or delete one of each pair before upgrading", 6)
	for _, id := range []string{"u2", "u3", "u5"} {
		if err := st.Users().DeleteUser(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrations.Run(ctx, st.db, st.Driver()); err != nil {
		t.Fatalf("a clean database: %v", err)
	}
	if err := st.Users().CreateUser(ctx, &User{ID: "u6", Username: "ERIN", Role: "user", Status: "active", SSOProvider: "local"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("a case variant beside the index: %v", err)
	}
	if u, err := st.Users().GetUserByUsername(ctx, "ERIN"); err != nil || u.ID != "u1" {
		t.Fatalf("lookup: %+v %v", u, err)
	}
}
```

Create `internal/store/tenancy_lock_test.go`:

```go
package store

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// On PostgreSQL the first admin's status is read under a row lock: a status change committed while
// the creation waits is seen, and no organization is created for a disabled admin.
func TestCreateOrganizationWithAdminLocksTheUser(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	if st.driver != "postgres" {
		t.Skip("row locks are PostgreSQL's; SQLite serializes writers")
	}
	ctx := context.Background()
	holder, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	var status string
	if err := holder.QueryRowContext(ctx, st.rebind(`SELECT status FROM users WHERE id=? FOR UPDATE`), "actor").Scan(&status); err != nil || status != "active" {
		t.Fatalf("hold: %s %v", status, err)
	}
	done := make(chan error, 1)
	go func() {
		done <- st.Tenancy().CreateOrganizationWithAdmin(ctx, &Organization{ID: "org_locked", Name: "Locked"}, "actor")
	}()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM users%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the creation never waited on the admin's row")
		}
		runtime.Gosched()
	}
	if _, err := holder.ExecContext(ctx, st.rebind(`UPDATE users SET status='disabled' WHERE id=?`), "actor"); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrInvalid) {
		t.Fatalf("creation for a disabled admin: %v", err)
	}
	var n int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM organizations WHERE id=?`), "org_locked").Scan(&n); err != nil || n != 0 {
		t.Fatalf("organizations: %d %v", n, err)
	}
}
```

`internal/store/application_spec_test.go`, `TestApplicationSpecVolumeBounds`: add to `cases`:

```go
		"one-character name": func(s *store.ApplicationSpec) { s.Volumes[0].Name = "a"; s.Services[0].Volumes[0].Source = "a" },
```

and at the end of the test:

```go
	two := withVolumes()
	two.Volumes[0].Name, two.Services[0].Volumes[0].Source = "ab", "ab"
	if err := store.ValidateApplicationSpec(two); err != nil {
		t.Fatalf("a two-character name refused: %v", err)
	}
```

`internal/applications/compose_test.go`: in `TestComposeVolumes` add the case

```go
		{"two-character name", volumeDoc("      - ab:/x\n", "volumes:\n  ab:\n"),
			`[{"kind":"named","source":"ab","target":"/x"}]`, `[{"name":"ab"}]`},
```

In `TestComposeVolumeRefusals` replace the `"drive letter declared"` case (a one-letter name can no longer be declared, so the top-level refusal comes first) with

```go
		{"one-character declared name", volumeDoc("      - a:/x\n", "volumes:\n  a:\n"), 7, 3, "Volume names must match [a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}"},
```

and change the `"bad declared name"` reason to `"Volume names must match [a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}"`.

`internal/agent/protocol/deployment_test.go`, `TestDeploymentRequestMounts`: add to the accepted map

```go
		"two-character volume name": func(r *DeploymentRequest) {
			r.Services[0].Mounts, r.Volumes = []Mount{{Kind: MountVolume, Source: "ab", Target: "/data"}}, []string{"ab"}
		},
```

and to the refused map

```go
		"volume name one character": func(r *DeploymentRequest) {
			r.Services[0].Mounts, r.Volumes = []Mount{{Kind: MountVolume, Source: "a", Target: "/data"}}, []string{"a"}
		},
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/store/ -run 'CaseVariant|VolumeBounds'; go test -count=1 ./internal/applications/ ./internal/agent/protocol/; PG=… go test -count=1 ./internal/store/ -run 'LocksTheUser'`
Expected: FAIL: `DROP INDEX` finds no `idx_users_username_lower`; one-character names are accepted; on PostgreSQL "the creation never waited on the admin's row".

- [ ] **Step 3: Implement**

`internal/store/migrations/migrations.go`: add `"maps"` and `"slices"` to the imports; replace `Migration` with:

```go
// Migration represents an incremental schema change step.
type Migration struct {
	Version  int
	Name     string
	SQLite   string
	Postgres string
	// Check runs in the migration's transaction before its SQL; an error refuses the migration
	// with nothing altered, and the server does not start.
	Check func(context.Context, *sql.Tx) error
}

// usernameLowerIndex is one statement for both dialects.
const usernameLowerIndex = `CREATE UNIQUE INDEX idx_users_username_lower ON users (LOWER(username));`

// refuseCaseVariantUsernames names every username that differs from another only by case: sign-in
// already matches LOWER(username), so each such group is one login for several accounts, and an
// operator must choose which stays. Groups by lowercase name, each in creation order.
func refuseCaseVariantUsernames(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT LOWER(username), username FROM users WHERE LOWER(username) IN (SELECT LOWER(username) FROM users GROUP BY LOWER(username) HAVING COUNT(*) > 1) ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	groups := map[string][]string{}
	for rows.Next() {
		var lower, name string
		if err := rows.Scan(&lower, &name); err != nil {
			return err
		}
		groups[lower] = append(groups[lower], name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}
	listed := []string{}
	for _, lower := range slices.Sorted(maps.Keys(groups)) {
		listed = append(listed, strings.Join(groups[lower], ", "))
	}
	return fmt.Errorf("usernames differ only by case: %s; rename or delete one of each pair before upgrading", strings.Join(listed, "; "))
}
```

Append to `registry` after version 29:

```go
	{Version: 30, Name: "users_username_lower_unique", Check: refuseCaseVariantUsernames, SQLite: usernameLowerIndex, Postgres: usernameLowerIndex},
```

In `Run`, insert right after the `tx, err := db.BeginTx(ctx, nil)` error check:

```go
		if m.Check != nil {
			if err := m.Check(ctx, tx); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migration v%d (%s) refused: %w", m.Version, m.Name, err)
			}
		}
```

`internal/store/tenancy.go`, `CreateOrganizationWithAdmin`: replace the status read with:

```go
	var status string
	// The admin's row is locked on PostgreSQL, as withTenant locks the actor's: a concurrent status
	// change waits behind the membership insert or is seen by it. SQLite serializes writers.
	query := `SELECT status FROM users WHERE id=?`
	if t.store.driver == "postgres" {
		query += " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx, t.store.rebind(query), adminUserID).Scan(&status)
```

`internal/api/admin_handlers.go:150`: replace the comment with `// Login matches LOWER(username) and idx_users_username_lower enforces it; this pre-check keeps the friendlier refusal.`

`internal/store/application_spec.go`: 

```go
var applicationVolumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}$`)
```

and the `ValidVolumeName` comment: `// ValidVolumeName is Docker's volume name grammar (at least two characters), at most 64.`

`internal/applications/compose.go:86`:

```go
				return nil, refusal(key, "Volume names must match [a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}")
```

`internal/agent/protocol/deployment.go`:

```go
	// Docker's volume name characters, at least two, long enough for a resolved "<project>_<name>" (64+1+64).
	deploymentVolume  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{1,128}$`)
```

- [ ] **Step 4: Run to verify they pass, on both drivers**

Run: `gofmt -w internal/store internal/applications internal/agent/protocol internal/api && go test -race -count=1 ./internal/store/... ./internal/applications/ ./internal/agent/protocol/ ./internal/api/ && PG=… go test -count=1 ./internal/store/... ./internal/api/`
Expected: PASS; `TestCreateOrganizationWithAdminLocksTheUser` skips on SQLite and passes on PostgreSQL.

- [ ] **Step 5: DOX and commit**

`internal/store/AGENTS.md`: in the `CreateOrganizationWithAdmin` bullet, after "in one transaction:" insert "the admin's `status` is read `FOR UPDATE` on PostgreSQL (so a concurrent status change waits behind the membership insert or is seen by it; `TestCreateOrganizationWithAdminLocksTheUser` holds the row, flips the status and asserts `ErrInvalid`);". Add a bullet: "Migration 30 creates `idx_users_username_lower` (`UNIQUE (LOWER(username))`, one statement for both dialects), which `GetUserByUsername`'s `LOWER(username)=LOWER(?)` sign-in lookup relies on. Its `Check` (`migrations.Migration.Check`, run in the migration's transaction before the SQL) refuses a database holding usernames that differ only by case with `usernames differ only by case: <group>; <group>; rename or delete one of each pair before upgrading` (each group's usernames in creation order joined by `, `, groups by lowercase name), altering nothing, so the server does not start until an operator resolves them (README, Upgrading). `CreateUser` maps a violation of either unique index to `ErrAlreadyExists`." In the migration 18 bullet replace "`[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}`" with "`[a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}` (two characters at least; a revision saved earlier with a one-character name no longer validates on read and must be edited)".

`internal/applications/AGENTS.md` does not name the grammar (the store owns it): no change.

`internal/agent/AGENTS.md`: in the protocol bullet replace "`[A-Za-z0-9][A-Za-z0-9_.-]{0,128}`" with "`[A-Za-z0-9][A-Za-z0-9_.-]{1,128}`".

```bash
gofmt -l internal cmd
git add internal/store internal/applications internal/agent internal/api/admin_handlers.go
git commit -m "feat(store): unique lowercase usernames, locked first admin, two-character volume names" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: API — update plans under the per-application guard; SSO refuses case variants

**Files:**
- Modify: `internal/api/image_check_handlers.go` (`guardApplication`; `handleCheckImageUpdates` uses it)
- Modify: `internal/api/application_handlers.go` (`handlePlanDeployment`; import `github.com/google/uuid`)
- Modify: `internal/api/tenant_handlers.go:79` (the `check_in_progress` message)
- Modify: `internal/api/provider_handlers.go` (`handleProviderCallback`; import `internal/agent/protocol`)
- Modify: `internal/sso/kysignon.go` (`ErrUsernameTaken`, `HandleSyncWebhook`)
- Test: `internal/api/deployment_update_test.go`, `internal/api/tenant_error_internal_test.go`, `internal/api/provider_test.go`, `internal/sso/sso_test.go`
- Docs: `internal/api/AGENTS.md`, `internal/sso/AGENTS.md`

**Interfaces:**
- Consumes (Task 5): `idx_users_username_lower`, so a racing `CreateUser` of a case variant is `store.ErrAlreadyExists`; `Users().GetUserByUsername` (case-insensitive).
- Produces: `func (s *Server) guardApplication(w http.ResponseWriter, a store.TenantAccess, app string) (release func(), ok bool)`; `sso.ErrUsernameTaken`. An update plan while a check or another update plan for the application runs is 409 `check_in_progress` "An image update check or update plan for this application is already running". An SSO sign-in whose username matches an existing account ignoring case (and is not that account's linked identity) is 403 "The username <name> is taken by another account; ask your administrator" with no account created and no session.

- [ ] **Step 1: Write the failing tests**

`internal/api/tenant_error_internal_test.go`: change the `errCheckInProgress` row to

```go
		{errCheckInProgress, 409, "check_in_progress", "An image update check or update plan for this application is already running"},
```

`internal/api/deployment_update_test.go`, `TestPlanUpdateThroughTheRegistry`: replace the block from `// Organization a's quota is two registry calls in flight, a check and a plan; a third is 429` through `plan() // every slot came back` with (the per-organization and server-wide slot caps stay pinned by `registry_slots_internal_test.go`):

```go
	// One registry operation per application: while a check runs, an update plan for the same
	// application is 409 check_in_progress before any inspection or registry slot, and a plan
	// with no update is not guarded. Another organization's check still gets through.
	gated := &fakeDigests{digest: remote, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	api.SetDigestResolverForTest(s, routeDigests{"orgb/web": fake, "": gated})
	results := make(chan *httptest.ResponseRecorder, 1)
	var release sync.Once
	open := func() { release.Do(func() { close(gated.gate) }) }
	t.Cleanup(open)
	// The goroutines only request; every check runs here, where t.Fatal is allowed.
	go func() { results <- tenantRequest(s, admin, "POST", check, "", true) }()
	<-gated.entered
	code(request("POST", deployments, string(planBody), 409), "check_in_progress")
	if held := api.RegistrySlotsHeldForTest(s); held != 1 {
		t.Fatalf("a refused update plan took a registry slot: %d held", held)
	}
	var checkedB store.UpdateCheck
	must(json.Unmarshal([]byte(request("POST", baseB+"/"+appB.ID+"/updates/check", "", 200)), &checkedB))
	if len(checkedB.Services) != 1 || checkedB.Services[0].RemoteDigest != remote {
		t.Fatalf("organization b's check: %+v", checkedB)
	}
	// Authorization answers before the guard: a member who may not deploy is 403, audited, not 409.
	w := tenantRequest(s, viewer, "POST", deployments, string(planBody), true)
	if w.Code != 403 {
		t.Fatalf("read-only update plan while a check runs: %d %s", w.Code, w.Body.String())
	}
	code(w.Body.String(), "tenant_access_denied")
	request("POST", deployments, string(plainBody), 201)
	open()
	if w := <-results; w.Code != 200 || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("the guarded check: %d %s", w.Code, w.Body.String())
	}
	// And the other way round: while an update plan runs, a check or a second update plan is 409.
	planGate := &fakeDigests{digest: remote, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	api.SetDigestResolverForTest(s, planGate)
	var releasePlan sync.Once
	openPlan := func() { releasePlan.Do(func() { close(planGate.gate) }) }
	t.Cleanup(openPlan)
	go func() { results <- tenantRequest(s, admin, "POST", deployments, string(planBody), true) }()
	<-planGate.entered
	code(request("POST", check, "", 409), "check_in_progress")
	code(request("POST", deployments, string(planBody), 409), "check_in_progress")
	openPlan()
	if w := <-results; w.Code != 201 || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("the guarded update plan: %d %s", w.Code, w.Body.String())
	}
	records, err := ts.ReadAudit(ctx, store.TenantAccess{ActorID: "usr_updater", OrganizationID: "a"}, 0, 200)
	must(err)
	denied := 0
	for _, rec := range records {
		if rec.UserID == "usr_viewer" && rec.Action == "application.deploy" && rec.Result == "denied" && rec.Resource == app.ID+"/updates" {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("read-only denial audit rows: %d", denied)
	}
	api.SetDigestResolverForTest(s, fake)
	plan() // the guard and every slot came back
```

`internal/api/provider_test.go`: add `"errors"` and `"github.com/Busnes-app/kyyard-server/internal/store"` to the imports and append:

```go
// A provider asserting "Erin" while a local "erin" exists is refused by name: sign-in matches
// LOWER(username), so creating "Erin" would put two accounts behind one login, and signing in as
// "erin" would hand the provider an account it never owned (Review Focus 4).
func TestSSOAutoProvisionRefusesACaseVariantUsername(t *testing.T) {
	var challenge string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "valid-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"profile-token","token_type":"Bearer"}`))
		case "/user":
			_, _ = w.Write([]byte(`{"id":"erin-at-idp","login":"Erin","name":"Erin"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer idp.Close()
	srv, st, cfg := setupTestServer(t)
	ctx := context.Background()
	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_erin", Username: "erin", Role: "admin", Status: "active", SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
	// HTTP is only used by this local provider fixture, bypassing the production HTTPS validator.
	p := sso.Provider{ID: "idp_test", Name: "OAuth fixture", Kind: "oauth2", ClientID: "yard", ClientSecret: "secret", AuthorizationURL: idp.URL + "/authorize", TokenURL: idp.URL + "/token", UserInfoURL: idp.URL + "/user", SubjectField: "id", UsernameField: "login", NameField: "name", Enabled: true, AutoProvision: true}
	plain, _ := json.Marshal([]sso.Provider{p})
	sealed, _ := crypto.EncryptAESGCM(plain, crypto.DeriveKey(cfg.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err := st.Settings().SetSetting(ctx, "sso_providers_enc", sealed); err != nil {
		t.Fatal(err)
	}
	login := do(t, srv, "GET", "/api/sso/idp_test/login", nil)
	if login.Code != 302 {
		t.Fatal(login.Code, login.Body)
	}
	dest, _ := url.Parse(login.Header().Get("Location"))
	challenge = dest.Query().Get("code_challenge")
	r := httptest.NewRequest("GET", "/api/sso/idp_test/callback?state="+dest.Query().Get("state")+"&code=valid-code", nil)
	for _, c := range login.Result().Cookies() {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "The username Erin is taken by another account") {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if !strings.HasPrefix(c.Name, "ky_sso_") && c.Value != "" {
			t.Fatalf("a session cookie was issued: %s", c.Name)
		}
	}
	if _, err := st.Users().GetUserBySSO(ctx, "idp_test", "erin-at-idp"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second account was created: %v", err)
	}
	if u, err := st.Users().GetUserByUsername(ctx, "ERIN"); err != nil || u.ID != "usr_erin" || u.SSOProvider != "local" {
		t.Fatalf("the local account changed: %+v %v", u, err)
	}
}
```

`internal/sso/sso_test.go`: add `"errors"` and `"strings"` to the imports and append:

```go
// A directory user whose username matches an existing account ignoring case is refused: no second
// account, and the existing one is not linked (Review Focus 4).
func TestKySignOnWebhookRefusesACaseVariantUsername(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()
	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_erin", Username: "erin", Role: "admin", Status: "active", SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
	client := sso.NewKySignOnClient(config.SSOConfig{KySignOnHMACSecret: "webhook-secret-999"}, st)
	body, _ := json.Marshal(sso.KySignOnSyncPayload{Event: "user.created", ID: "ext-erin", Username: "Erin", Email: "erin@busnes.app", Role: "user", Status: "active", Timestamp: time.Now().Unix()})
	err = client.HandleSyncWebhook(ctx, body, crypto.ComputeHMACSHA256(body, "webhook-secret-999"))
	if !errors.Is(err, sso.ErrUsernameTaken) || !strings.Contains(err.Error(), "Erin") {
		t.Fatalf("case variant: %v", err)
	}
	if _, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-erin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second account was created: %v", err)
	}
	if u, err := st.Users().GetUserByUsername(ctx, "ERIN"); err != nil || u.ID != "usr_erin" || u.SSOProvider != "local" {
		t.Fatalf("the local account changed: %+v %v", u, err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/api/ -run 'TestPlanUpdateThroughTheRegistry|TenantError|TestSSOAutoProvision' ; go test -count=1 ./internal/sso/ -run CaseVariant`
Expected: FAIL: the update plan during a check is 201 or 429, not 409; the message differs; the SSO callback answers 403 "Account unavailable" (the index refuses the create) and the webhook error is not `ErrUsernameTaken` (`undefined: sso.ErrUsernameTaken` at compile).

- [ ] **Step 3: Implement**

`internal/api/image_check_handlers.go`, add after `acquireRegistrySlot`:

```go
// guardApplication admits one registry operation per application at a time, an update check or
// an update plan, keyed by the canonical application ID, or answers 409 check_in_progress.
func (s *Server) guardApplication(w http.ResponseWriter, a store.TenantAccess, app string) (release func(), ok bool) {
	key := a.OrganizationID + "/" + a.EnvironmentID + "/" + app
	if _, busy := s.imageChecks.LoadOrStore(key, struct{}{}); busy {
		s.tenantError(w, errCheckInProgress)
		return nil, false
	}
	return func() { s.imageChecks.Delete(key) }, true
}
```

and in `handleCheckImageUpdates` replace from `key := a.OrganizationID + …` through `defer release()` with:

```go
	done, ok := s.guardApplication(w, a, app)
	if !ok {
		return
	}
	defer done()
	release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
	if !ok {
		return
	}
	defer release()
```

`internal/api/application_handlers.go`: add `"github.com/google/uuid"` to the imports; in `handlePlanDeployment` replace the first `if len(input.Update) > 0 { … }` block with:

```go
	if len(input.Update) > 0 {
		id, err := uuid.Parse(r.PathValue("application"))
		if err != nil {
			s.tenantError(w, store.ErrInvalid)
			return
		}
		// Authorize before the guard and the slot, so a caller who may not deploy can neither see
		// nor hold either.
		if err := s.store.Tenancy().CheckImageUpdateAccess(r.Context(), a, id.String()); err != nil {
			s.tenantError(w, err)
			return
		}
		// The check's guard, before the inspections and the registry slot.
		done, ok := s.guardApplication(w, a, id.String())
		if !ok {
			return
		}
		defer done()
	}
```

`internal/api/tenant_handlers.go:79`:

```go
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "An image update check or update plan for this application is already running", "code": "check_in_progress"})
```

`internal/api/provider_handlers.go`: add `"github.com/Busnes-app/kyyard-server/internal/agent/protocol"` to the imports; in `handleProviderCallback` replace from `user, err := s.store.Users().GetUserBySSO(ctx, p.ID, claims.Subject)` through the `if err != nil || user == nil || user.Status != "active" {` line with:

```go
	user, err := s.store.Users().GetUserBySSO(ctx, p.ID, claims.Subject)
	if errors.Is(err, store.ErrNotFound) && p.AutoProvision {
		// Sign-in matches LOWER(username): an account by this name in any case is neither linked
		// to the provider's claim nor shadowed by a second one. The index closes the race.
		switch _, taken := s.store.Users().GetUserByUsername(ctx, claims.PreferredUsername); {
		case taken == nil:
			err = store.ErrAlreadyExists
		case errors.Is(taken, store.ErrNotFound):
			user = &store.User{ID: "usr_" + crypto.RandomHex(12), Username: claims.PreferredUsername, Email: claims.Email, DisplayName: claims.Name, Role: "user", Status: "active", SSOProvider: p.ID, SSOSubject: claims.Subject}
			err = s.store.Users().CreateUser(ctx, user)
		default:
			err = taken
		}
	}
	if errors.Is(err, store.ErrAlreadyExists) {
		s.writeError(w, 403, "The username "+protocol.CleanText(claims.PreferredUsername, 64)+" is taken by another account; ask your administrator")
		return
	}
	if err != nil || user == nil || user.Status != "active" {
```

`internal/sso/kysignon.go`: add after the imports

```go
// ErrUsernameTaken refuses a directory user whose username matches an existing account ignoring
// case: sign-in matches LOWER(username), and a directory's claim is never a link to that account.
var ErrUsernameTaken = errors.New("username is taken by another account")
```

and in `HandleSyncWebhook`, in the `user.created`/`user.updated` case, replace the creation (`newUser := &store.User{…}` through `return k.store.Users().CreateUser(ctx, newUser)`) with:

```go
		if _, err := k.store.Users().GetUserByUsername(ctx, payload.Username); err == nil {
			return fmt.Errorf("%w: %s", ErrUsernameTaken, payload.Username)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		newUser := &store.User{
			ID:          fmt.Sprintf("usr_%s", crypto.RandomHex(12)),
			Username:    payload.Username,
			Email:       payload.Email,
			DisplayName: payload.DisplayName,
			Role:        role,
			Status:      status,
			SSOProvider: "kysignon",
			SSOSubject:  payload.ID,
		}
		err = k.store.Users().CreateUser(ctx, newUser)
		if errors.Is(err, store.ErrAlreadyExists) {
			return fmt.Errorf("%w: %s", ErrUsernameTaken, payload.Username)
		}
		return err
```

- [ ] **Step 4: Run to verify they pass, on both drivers**

Run: `gofmt -w internal/api internal/sso && go vet ./internal/api/ ./internal/sso/ && go test -race -count=1 ./internal/api/ ./internal/sso/ && PG=… go test -count=1 ./internal/api/ ./internal/sso/`
Expected: PASS.

- [ ] **Step 5: DOX and commit**

`internal/api/AGENTS.md`: in the updates bullet replace "409 `check_in_progress` while a check for the same application runs: an in-memory `sync.Map` on `Server` (`imageChecks`) keyed `org/env/<canonical uuid>`" with "409 `check_in_progress` while a check or an update plan for the same application runs: `guardApplication`, an in-memory `sync.Map` on `Server` (`imageChecks`) keyed `org/env/<canonical uuid>` shared with update plans"; in the plan bullet replace "then, after the plan-time inspections below (so a slow agent never holds a registry slot), takes a registry slot as the check does" with "then takes the check's per-application guard (`guardApplication`, 409 `check_in_progress` while a check or another update plan for the application runs), then, after the plan-time inspections below (so a slow agent never holds a registry slot), takes a registry slot as the check does". In the SSO bullets add: "SSO auto-provision looks the provider's username up case-insensitively (`GetUserByUsername`) before creating an account; a match, or `ErrAlreadyExists` from a racing create (`idx_users_username_lower`), is 403 "The username <name> is taken by another account; ask your administrator" (`CleanText`, 64 bytes) with no account and no session: a provider's claimed name never links to or shadows an existing account (`TestSSOAutoProvisionRefusesACaseVariantUsername`)."

`internal/sso/AGENTS.md`: add after the webhook-signature bullet: "`HandleSyncWebhook` refuses a `user.created` whose username matches an existing account ignoring case with `ErrUsernameTaken` (named in the error), never linking to or duplicating that account; a racing create is caught by `idx_users_username_lower` the same way."

```bash
gofmt -l internal cmd
git add internal/api internal/sso
git commit -m "feat(api): guard update plans per application; refuse SSO case-variant usernames" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Web — outcomes from code tables, the correlation ID on the panel

**Files:**
- Modify: `web/src/components/ApplicationDeploymentPlan.tsx` (types, `STEP_CODES`, `RESULT_CODES`, `SERVER_DETAILS`, `codeText`, `stepText`, `preconditionExplanation`, `explanationFor`, `ResultSection`, `Correlation`, the plan header; `FIXED_DETAIL_PREFIXES` and `fixedDetail` go)
- Modify: `web/src/components/ApplicationUpdates.tsx` (`check_in_progress` texts)
- Test: `web/src/components/ApplicationDeploymentPlan.test.tsx`, `web/src/components/ApplicationUpdates.test.tsx`
- Build: `web/dist` via `make build-web`
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes (Tasks 4, 6): deployment JSON `correlation_id`; `result: {code, steps: [{service, step, outcome, code, detail}], services}` with no free result detail; the row's `detail` holds only the server's own sentences; 409 `check_in_progress` for an update plan.
- Produces: exported `STEP_CODES`, `RESULT_CODES`, `stepText(step)` (tests import them).

- [ ] **Step 1: Write the failing tests**

`web/src/components/ApplicationDeploymentPlan.test.tsx`:

Replace the `refused` fixture:

```ts
const refused = { ...plan, state: 'denied', detail: '', result: { code: 'step_failed', steps: [{ service: 'web', step: 'precondition', outcome: 'denied', code: 'unsupported', detail: 'privileged' }, { service: 'web', step: 'image', outcome: 'skipped', detail: '' }], services: [] } };
```

Replace the test `'shows a step detail with a recognized adapter prefix and hides an unrecognized one'` with:

```ts
it('renders step codes from the table and anything else as inert text', async () => {
  const mixed = { ...plan, state: 'failed', detail: 'unexpected raw server text', result: { code: 'step_failed', steps: [
    { service: 'web', step: 'precondition', outcome: 'succeeded', detail: '' },
    { service: 'web', step: 'start', outcome: 'failed', code: 'identity_unverified', detail: 'c'.repeat(64) },
    { service: 'web', step: 'stop', outcome: 'failed', code: 'runtime_status', detail: '500' },
    { service: 'web', step: 'remove', outcome: 'failed', code: 'runtime_status', detail: 'abc' },
    { service: 'web', step: 'create', outcome: 'failed', code: '<img src=x onerror=alert(1)>', detail: 'secret-canary' },
  ], services: [] } };
  vi.stubGlobal('fetch', stubFetch([mixed]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(`The container started but its identity could not be verified (container ${'c'.repeat(64)}).`);
  expect(screen.getByText('The runtime refused with status 500.')).toBeTruthy();
  expect(screen.getByText('The runtime refused.')).toBeTruthy();
  expect(screen.getByText('unrecognised outcome `<img src=x onerror=alert(1)>`')).toBeTruthy();
  expect(screen.getByText('A step did not succeed; the steps say which.')).toBeTruthy();
  expect(document.querySelector('img')).toBeNull();
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(document.body.textContent).not.toContain('unexpected raw server text');
});
```

Replace `'keys a refused precondition on its step detail'` with:

```ts
it('keys a refused precondition on its step code', async () => {
  const gone = { ...refused, result: { code: 'step_failed', steps: [{ service: 'web', step: 'precondition', outcome: 'denied', code: 'container_missing', detail: '' }], services: [] } };
  vi.stubGlobal('fetch', stubFetch([gone]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The mapped container no longer exists on the host; refresh the inventory and plan again.');
  expect(screen.getByText('The container no longer exists.')).toBeTruthy();
});
```

In the `removal` fixture replace the step with `{ service: 'web', step: 'precondition', outcome: 'denied', code: 'identity_mismatch', detail: '' }` and add `code: 'step_failed'` to its `result`.

Replace `'explains a recheck denial like a precondition'`, `'explains a deployment refused for clock skew'`, `'shows the restart detail with the inspect-the-host guidance'` and `'shows an unsupported-configuration step detail'` with:

```ts
it('explains a recheck denial like a precondition', async () => {
  const drifted = { ...plan, state: 'denied', detail: '', result: { code: 'step_failed', steps: [
    { service: 'web', step: 'precondition', outcome: 'succeeded', detail: '' },
    { service: 'web', step: 'image', outcome: 'succeeded', detail: '' },
    { service: 'web', step: 'recheck', outcome: 'denied', code: 'configuration_drift', detail: '' },
    { service: 'web', step: 'rename', outcome: 'skipped', detail: '' },
  ], services: [] } };
  vi.stubGlobal('fetch', stubFetch([drifted]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(/changed on the host while the deployment prepared/);
  expect(screen.getByText('The container changed after the precondition.')).toBeTruthy();
});
it('explains a deployment refused for clock skew', async () => {
  vi.stubGlobal('fetch', stubFetch([{ ...plan, state: 'failed', detail: '', result: { code: 'clock_skew', steps: [], services: [] } }]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The host clock differs from the server by more than five minutes; nothing ran. Correct the host clock, then plan again.');
  expect(screen.getByText('The host clock differs from the server by more than five minutes; nothing ran.')).toBeTruthy();
});
it('shows the restart outcome with the inspect-the-host guidance', async () => {
  vi.stubGlobal('fetch', stubFetch([{ ...plan, state: 'unknown', detail: '', result: { code: 'restarted', steps: [], services: [] } }]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The host may or may not have acted. Inspect it before planning again.');
  expect(screen.getByText('The agent restarted after replacement began; inspect the host.')).toBeTruthy();
});
it('names unsupported configuration from its codes and drops unknown ones', async () => {
  const refusedCodes = { ...plan, state: 'denied', detail: '', result: { code: 'step_failed', steps: [{ service: 'web', step: 'precondition', outcome: 'denied', code: 'unsupported', detail: 'privileged,devices,made_up' }], services: [] } };
  vi.stubGlobal('fetch', stubFetch([refusedCodes]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The container has configuration the definition cannot express: runs privileged, maps host devices.');
  expect(screen.getByText(/configuration the definition does not describe/i)).toBeTruthy();
  expect(document.body.textContent).not.toContain('made_up');
});
it('asks for an agent upgrade on a legacy outcome and hides its old sentence', async () => {
  const older = { ...plan, state: 'denied', detail: 'service web, step precondition: the container no longer exists', result: { code: 'legacy', steps: [{ service: 'web', step: 'precondition', outcome: 'denied', code: 'legacy', detail: '' }], services: [] } };
  vi.stubGlobal('fetch', stubFetch([older]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect((await screen.findAllByText('The agent did not classify this outcome; upgrade the agent.')).length).toBe(2);
  expect(document.body.textContent).not.toContain('no longer exists');
  expect(document.body.textContent).not.toContain('configuration the definition does not describe');
});
it.each([
  ['planned', { ...plan, correlation_id: '0123456789abcdef0123456789abcdef' }],
  ['settled', { ...settled, correlation_id: '0123456789abcdef0123456789abcdef' }],
])('shows the %s deployment correlation ID once', async (_state, row) => {
  vi.stubGlobal('fetch', stubFetch([row]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect((await screen.findAllByText('0123456789abcdef0123456789abcdef')).length).toBe(1);
  expect(screen.getByText(/search the audit log for it/)).toBeTruthy();
});
```

`web/src/components/ApplicationUpdates.test.tsx`: change `[409, 'check_in_progress', 'A check is already running.']` to `[409, 'check_in_progress', 'An update check or update plan is already running.']`, and add `['check_in_progress', 'An update check or update plan is already running.'],` to the plan-conflict `it.each` table.

- [ ] **Step 2: Run to verify they fail**

Run: `(cd web && npx vitest run src/components/ApplicationDeploymentPlan.test.tsx src/components/ApplicationUpdates.test.tsx)`
Expected: FAIL: steps still render through `fixedDetail` (codes and parameters show nothing), no correlation ID, and the update texts differ.

- [ ] **Step 3: Implement the plan panel**

`web/src/components/ApplicationDeploymentPlan.tsx`: add `import { unsupportedNames } from './ApplicationInspection';`. Replace the `DeployStep` and `Deployment` types:

```ts
type DeployStep = { service: string; step: string; outcome: string; code?: string; detail: string };
```

```ts
type Deployment = { id: string; instance_id: string; endpoint_id: string; endpoint_name?: string; kind?: string; applied_by?: string; state: string; revision: number; mapping_version: number; created_at: string; expires_at: string; expired: boolean; detail: string; correlation_id?: string; applied_at?: string | null; deadline?: string | null; settled_at?: string | null; result: { code?: string; steps: DeployStep[]; services: DeployedService[] } | null; plan: { project: string; services?: PlannedService[]; containers?: RemovalTarget[]; volumes?: string[] } };
```

Replace everything from the `// Fixed detail prefixes:` comment through the end of `fixedDetail` with:

```ts
const LEGACY_OUTCOME = 'The agent did not classify this outcome; upgrade the agent.';
// Step codes (docs/agent-protocol.md, Outcome codes). A step's detail is its code's parameter and
// renders only in the shape its code allows; an unknown code renders as inert text.
export const STEP_CODES: Record<string, string> = {
  runtime_unreadable: "The daemon's default runtime could not be read.",
  container_missing: 'The container no longer exists.',
  identity_mismatch: 'The container is not the one this plan was decided about.',
  image_identity_mismatch: 'The host reported a different image identity than the plan pinned.',
  configuration_unreported: "The runtime did not report the container's full configuration.",
  unsupported: 'The container has configuration the definition cannot express',
  bind_missing: 'A bind mount in the plan is not on the container.',
  volume_mount_missing: 'A kept volume is not mounted on the container.',
  volume_not_owned: 'The volume is not owned by this project.',
  volume_missing: 'The volume does not exist.',
  volume_create_failed: 'The volume could not be created.',
  image_missing: "The container's image is no longer present.",
  pinned_image_missing: 'The pinned image is not present on the host.',
  configuration_drift: 'The container changed after the precondition.',
  name_reserved: 'A container already holds the name reserved for the previous one.',
  name_taken: 'A container with that name already exists.',
  identity_unusable: 'The runtime returned an unusable container identity.',
  identity_unreadable: 'The container started but its identity could not be read',
  identity_unverified: 'The container started but its identity could not be verified',
  dependents: 'Something still depends on this container.',
  deadline: 'Not enough time was left before the deadline to continue safely.',
  pull_failed: 'The image pull failed.',
  pull_unauthorized: 'The registry refused the credential.',
  pull_not_found: 'The registry has no such image.',
  pull_digest_mismatch: 'The pulled image does not match the pinned digest.',
  cancelled: 'The run was cancelled before the runtime answered.',
  runtime_timeout: 'The runtime did not answer in time.',
  runtime_error: 'The runtime call failed.',
  runtime_status: 'The runtime refused',
  legacy: LEGACY_OUTCOME,
};
export const RESULT_CODES: Record<string, string> = {
  step_failed: 'A step did not succeed; the steps say which.',
  clock_skew: 'The host clock differs from the server by more than five minutes; nothing ran.',
  invalid_request: 'The agent refused the deployment request as invalid.',
  wrong_endpoint: 'The deployment was addressed to another endpoint.',
  busy: 'The agent was already applying a deployment.',
  restarted: 'The agent restarted after replacement began; inspect the host.',
  unreadable: 'The runtime returned a result the agent could not read.',
  legacy: LEGACY_OUTCOME,
};
// Sentences the server itself writes on a row it settles without a result (internal/store,
// internal/api); any other row detail is an older agent's and stays hidden.
const SERVER_DETAILS = new Set([
  'the connection ended before a result arrived',
  'no result arrived before the deadline',
  "the host's result did not match the plan; inspect the host",
  'the endpoint disconnected before the deployment was sent',
  'the endpoint disconnected before the removal was sent',
]);
function codeText(table: Record<string, string>, code: string): string {
  return Object.hasOwn(table, code) ? table[code] ?? '' : `unrecognised outcome \`${code}\``;
}
export function stepText(s: { code?: string; detail?: string }): string {
  const code = s.code ?? '';
  if (!code) return '';
  const text = codeText(STEP_CODES, code);
  const detail = s.detail ?? '';
  switch (code) {
    case 'unsupported': {
      const names = detail.split(',').filter(c => Object.hasOwn(unsupportedNames, c)).map(c => unsupportedNames[c]);
      return names.length ? `${text}: ${names.join(', ')}.` : `${text}.`;
    }
    case 'identity_unreadable':
    case 'identity_unverified':
      return /^[0-9a-f]{64}$/.test(detail) ? `${text} (container ${detail}).` : `${text}.`;
    case 'runtime_status':
      return /^[1-5][0-9]{2}$/.test(detail) ? `${text} with status ${detail}.` : `${text}.`;
  }
  return text;
}
```

Replace `preconditionExplanation` and `explanationFor` with:

```ts
function preconditionExplanation(code: string): string {
  switch (code) {
    case 'configuration_drift': return 'The mapped container changed on the host while the deployment prepared; it and every later service were left untouched, and services before it were replaced. Review the host, then plan again.';
    case 'container_missing': return 'The mapped container no longer exists on the host; refresh the inventory and plan again.';
    case 'identity_mismatch': return 'The mapped container changed on the host; plan again.';
    case 'image_missing': return "The mapped container's image is no longer present on the host. Review it on the host before planning again.";
    case 'legacy': return '';
  }
  return 'A mapped container has configuration the definition does not describe. Review it on the host before planning again.';
}
// The row's state decides first: an unknown or timed-out row may carry no steps, and a failed
// row without a result never reached the host (FailDeployment). Then the result and step codes.
function explanationFor(current: Deployment): string {
  if (current.state === 'unknown') return 'The host may or may not have acted. Inspect it before planning again.';
  if (current.state === 'timed_out') return 'The host did not answer in time.';
  if (current.state === 'failed' && current.result === null) return 'The deployment was not sent to the host.';
  if (current.result?.code === 'clock_skew') return 'The host clock differs from the server by more than five minutes; nothing ran. Correct the host clock, then plan again.';
  const failing = current.result?.steps.find(s => s.outcome !== 'succeeded' && s.outcome !== 'skipped');
  if (!failing) return '';
  if (failing.outcome === 'denied' && (failing.step === 'precondition' || failing.step === 'recheck')) return current.kind === 'remove'
    ? 'A container of this application is not the one recorded; refresh the inventory and, if it was recreated outside KyYard, release and adopt it again.'
    : preconditionExplanation(failing.code ?? '');
  if (failing.code === 'pinned_image_missing') return 'The pinned image is no longer present on the host.';
  if (failing.outcome === 'unknown') return 'The host may or may not have acted. Inspect it before planning again.';
  if (failing.outcome === 'timed_out') return 'The host did not answer in time.';
  if (failing.outcome === 'failed' && current.kind === 'remove') return 'A step failed on the host; containers removed before it are gone and the rest stay adopted.';
  if (failing.outcome === 'failed') return 'A step failed on the host; the previous container may remain renamed with a .kyyard-prev suffix.';
  return '';
}
```

Replace `ResultSection` with:

```tsx
function Correlation({ id }: { id?: string }) {
  return id ? <p>Correlation ID <code>{id}</code>: search the audit log for it.</p> : null;
}
function ResultSection({ current }: { current: Deployment }) {
  const steps = usePagination(current.result?.steps ?? [], `${current.id}-steps`);
  const explanation = explanationFor(current);
  const detail = SERVER_DETAILS.has(current.detail) ? current.detail : '';
  const outcome = current.result?.code ? codeText(RESULT_CODES, current.result.code) : '';
  return <>
    <p>State: {current.state}{current.settled_at ? `, settled ${new Date(current.settled_at).toLocaleString()}` : ''}.</p>
    <Correlation id={current.correlation_id} />
    {detail && <p>{detail}</p>}
    {outcome && <p>{outcome}</p>}
    {explanation && <p role="alert">{explanation}</p>}
    {current.result && <>
      {steps.controls}
      <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Step</th><th>Outcome</th><th>Detail</th></tr></thead><tbody>{steps.rows.map((s, i) => <tr key={`${s.service}-${s.step}-${i}`}>
        <td data-label="Service">{s.service}</td>
        <td data-label="Step">{s.step}</td>
        <td data-label="Outcome">{s.outcome}</td>
        <td data-label="Detail">{stepText(s)}</td>
      </tr>)}</tbody></table>
      {current.result.services.length > 0 && <ul className="ky-list">{current.result.services.map(s => <li key={s.container_id} style={{ overflowWrap: 'anywhere' }}><strong>{s.service}</strong><br /><span>{s.container_id}</span><br /><span>{s.image_id}</span></li>)}</ul>}
    </>}
  </>;
}
```

In `PlanView`, directly after `<PlanDetails d={current} />` add `{current.state === 'planned' && <Correlation id={current.correlation_id} />}` (a settled row shows it in `ResultSection`).

`web/src/components/ApplicationUpdates.tsx`: change `check_in_progress: 'A check is already running.',` to `check_in_progress: 'An update check or update plan is already running.',` and add `check_in_progress: 'An update check or update plan is already running.',` to `planConflicts`.

- [ ] **Step 4: Run the web suite and rebuild**

Run: `(cd web && npx vitest run) && make build-web && git status --short web/dist`
Expected: every test passes, the typecheck in `npm run build` passes, and `web/dist` shows the rebuilt bundle.

- [ ] **Step 5: DOX and commit**

`web/AGENTS.md`, the apply bullet: replace "A settled row shows state, a fixed-text detail (only recognized prefixes render; anything else stays hidden), a 25-row step table (service, step, outcome, fixed detail) and the new container identities." with "A settled row shows state, its correlation ID (\"search the audit log for it\"; a planned row shows it under the plan), the row detail only when it is one of the server's own sentences (`SERVER_DETAILS`: connection ended, no result arrived, the host's result did not match, the endpoint disconnected before the deployment or removal was sent), the result code's text (`RESULT_CODES`), a 25-row step table (service, step, outcome, `stepText`: the step code's text from `STEP_CODES`, with `unsupported` codes named through `unsupportedNames` and unknown ones dropped, a 64-hex container ID for `identity_unreadable`/`identity_unverified` and a 3-digit status for `runtime_status`, each rendered only in that shape) and the new container identities. `legacy` reads \"The agent did not classify this outcome; upgrade the agent.\"; any other unknown code renders as the inert text ``unrecognised outcome `<code>` ``." Replace "then on the failing step: `denied` at `precondition` keyed on its detail (container gone, container changed, its image gone, otherwise configuration the definition cannot describe), the pinned image missing," with "a `clock_skew` result; then on the failing step's code: `denied` at `precondition` or `recheck` (`configuration_drift`, `container_missing`, `identity_mismatch`, `image_missing`, `legacy` says nothing more, otherwise configuration the definition cannot describe), `pinned_image_missing`,". Delete the sentence "Server-fixed details (`the connection ended`, `no result arrived`, `the endpoint disconnected`, `the host's result`) are recognized prefixes." In the Updates bullet, where `check_in_progress` texts are listed, use "An update check or update plan is already running." for both the check and **Plan update**.

```bash
git add web
git commit -m "feat(web): deployment outcomes from code tables and the correlation ID" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Documents, the DOX pass and the full gate

**Files:**
- Modify: `docs/agent-protocol.md` (Deployment apply, Deployment remove, new Outcome codes section)
- Modify: `docs/application-schema.md` (volume grammar, settle)
- Modify: `README.md` (upgrade notes)
- Modify: `docs/RESTORE.md` (Step 3)
- Modify: `KyYard-Implementation-Plan.md` §8
- Docs: every `AGENTS.md` touched in Tasks 1–7 re-read against the code

**Interfaces:**
- Consumes: everything above. Produces: no code.

- [ ] **Step 1: `docs/agent-protocol.md`**

In `## Deployment apply` replace these fragments (each occurs once):
- "carries the outcome, a fixed-text detail, every step attempted with its outcome," → "carries the outcome, a result code, the frame's `request_id`, every step attempted with its outcome, code and parameter (Outcome codes, below),"
- "Nil `Deploy` answers every apply `denied` "this agent has no runtime to deploy"" → "Nil `Deploy` answers every apply `denied` `invalid_request`"
- "(failure is `denied` "invalid deployment request", sent but not persisted)" → "(failure is `denied` `invalid_request`, sent but not persisted)"
- "(`denied`, not persisted)" → "(`denied` `wrong_endpoint`, not persisted)"
- "(`denied` "this agent is already applying a deployment")" → "(`denied` `busy`)"
- "is replaced by `unknown` "the runtime returned an unreadable result"" → "is replaced by `unknown` `unreadable`"
- "A skewed frame is answered `failed`, detail `clock skew exceeds 5 minutes`" → "A skewed frame is answered `failed`, code `clock_skew`"
- "`denied` `the container changed after the precondition`" → "`denied` `configuration_drift`"
- "`unknown`, `the agent restarted after replacement began; inspect the host`" → "`unknown` `restarted`"
- "Its details are fixed (`unauthorized`, `not found`, `pull failed`, `pulled image does not match`, `tag failed`), never the daemon's text" → "Its codes are fixed (`pull_unauthorized`, `pull_not_found`, `pull_failed` for any other status, a stream error or a refused tag, `pull_digest_mismatch`), never the daemon's text"
- "a missing Hub credential reads `not found`" → "a missing Hub credential reads `pull_not_found`"
- "(`denied`, `bind mount not present on the container`)" → "(`denied` `bind_missing`)"
- "(else `denied` `volume is not owned by this project`)" → "(else `denied` `volume_not_owned`)"
- "(else `denied` `volume mount not present on the container`, from the volume step or from a later service's precondition)" → "(else `denied` `volume_mount_missing`, from the volume step or from a later service's precondition)"
- "is `denied` `volume does not exist`" → "is `denied` `volume_missing`"
- "fails the step with `volume create failed`" → "fails the step with `volume_create_failed`"

Replace the paragraph beginning "The server always sends a service's `mounts` key" up to (not including) "A service's `mounts` (at most 32)" with: "Every service carries its `mounts` list (`[]` for none): a frame without it comes from a server older than mounts, which also sends no `request_id`, and fails `Validate` (`invalid_request`). Upgrade the server before its agents: an agent with this rule refuses every deployment from an older server rather than guess." In the same paragraph replace "`[A-Za-z0-9][A-Za-z0-9_.-]{0,128}`" with "`[A-Za-z0-9][A-Za-z0-9_.-]{1,128}`".

At the end of the `Server side:` paragraph append: "A result that fails `Validate` is dropped with a log line and nothing is stored; a result with no `request_id` (from an agent built before outcome codes, or an older ledger entry a newer agent re-sends) is read with each missing step or result code as `legacy` and its text dropped; a result whose `request_id` is not the row's is refused like one that does not fit the plan."

In `## Deployment remove` replace "must match the pinned identity, else `denied`;" with "must match the pinned identity, else `denied` `identity_mismatch`;" and "A time guard before each target's `stop` refuses to continue unless `operationBudget + 2*callBudget` remain before the deadline." with "A time guard before each target's `stop` refuses to continue (`timed_out` `deadline`) unless `operationBudget + 2*callBudget` remain before the deadline; a 409 from the remove is `failed` `dependents`."

Insert before `## Deployment remove`:

```markdown
## Outcome codes

**Implemented (PR D2).** A deployment result names what happened in a closed vocabulary; a step's `detail` is its code's parameter, never a sentence, and the web renders every outcome from the code. Both sides validate (`DeploymentResult.Validate`).

`request_id` (required on `deployment.apply` and `deployment.remove`, `^[A-Za-z0-9_-]{1,64}$`) is the plan's audit correlation ID: the server stores the plan request's `X-Request-ID` on the deployment row as `correlation_id` and writes every audit row of the deployment (plan, apply, settle, refusal, abandon, sweep, not sent) under it. The agent logs it beside the deployment ID at receipt, start and finish, and echoes it in `deployment.result`; the server refuses a result whose `request_id` is another deployment's.

Step codes (`code` on a `denied`, `failed`, `timed_out` or `unknown` step; a `succeeded` or `skipped` step carries no code and no detail):

| Code | Emitted when | Detail |
|---|---|---|
| `runtime_unreadable` | `GET /info` gave no default runtime | none |
| `container_missing` | the old container is gone (precondition, recheck, removal) | none |
| `identity_mismatch` | container ID, image ID or creation time differ from `Replaces` | none |
| `image_identity_mismatch` | the host reported a different image identity than the plan pinned | none |
| `configuration_unreported` | the daemon omitted `Config`/`HostConfig`/`Mounts` | none |
| `unsupported` | `undescribed` returned codes | the codes, joined by `,` (each from `UnsupportedCodes`) |
| `bind_missing` | a bind in the frame is not on the old container | none |
| `volume_mount_missing` | a per-service kept volume is not on the old container | none |
| `volume_not_owned` | an existing volume fails the ownership rule | none |
| `volume_missing` | an external volume does not exist | none |
| `volume_create_failed` | `POST /volumes/create` failed | none |
| `image_missing` | the container's image is no longer present | none |
| `pinned_image_missing` | the pinned image is not on the host after the pull step | none |
| `configuration_drift` | the recheck found identity or configuration changed | none |
| `name_reserved` | a container already holds the name reserved for the previous one | none |
| `name_taken` | a container with the service's name already exists | none |
| `identity_unusable` | the runtime returned an unusable container identity | none |
| `identity_unreadable` | the container started but its identity could not be read | the 64-hex container ID |
| `identity_unverified` | the container started but its identity could not be verified | the 64-hex container ID |
| `dependents` | removal refused because something depends on the container | none |
| `deadline` | not enough time left to pull, replace or remove safely | none |
| `pull_failed` | the pull stream reported an error | none |
| `pull_unauthorized` | 401/403 from the registry | none |
| `pull_not_found` | 404 from the registry | none |
| `pull_digest_mismatch` | the pulled image's digest differs from the pinned one | none |
| `cancelled` | the run's context ended before the runtime answered | none |
| `runtime_timeout` | the per-call budget elapsed | none |
| `runtime_error` | the call failed without a status | none |
| `runtime_status` | the daemon answered an unexpected status | the 3-digit status |

`unsupported` carries one to thirty-two distinct `UnsupportedCodes`, as many whole codes as 256 bytes (`MaxDeploymentStepDetailBytes`) hold; `identity_unreadable`/`identity_unverified` a full Docker ID; `runtime_status` `[1-5][0-9]{2}`; every other code an empty detail. A step whose detail has another shape makes the result unreadable and nothing is stored.

Result codes (`code` on every result whose outcome is not `succeeded`; the result has no free detail):

| Code | Emitted when |
|---|---|
| `step_failed` | a step did not succeed; the steps say which |
| `clock_skew` | `issued_at` is more than `MaxClockSkew` from the agent's clock |
| `invalid_request` | the frame failed `Validate` (or named a kind this agent has no runtime for) |
| `wrong_endpoint` | the frame names another endpoint |
| `busy` | the agent is already applying a deployment |
| `restarted` | the agent restarted after replacement began |
| `unreadable` | the runtime returned a result the agent could not validate |

Compatibility: a result without `request_id` was produced by a binary built before this change. The server reads each missing code on a non-succeeding step or result as `legacy` and drops its detail, so a deployment in flight across the upgrade still settles; `legacy` is in both sets but no agent emits it, and the web shows "The agent did not classify this outcome; upgrade the agent." A result with a `request_id` and a code-less failing step is unreadable. The stop and start steps no longer name the created container; `identity_unreadable` and `identity_unverified` do.
```

- [ ] **Step 2: `docs/application-schema.md`**

In the Volumes **Import** bullet replace "Names match `[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}`." with "Names match `[a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}` (two characters at least, as Docker requires; a revision saved before this rule with a one-character name no longer plans: save a new revision with a longer name)." In `## Deploy` item 4 (Settle) append: "Each settle, refusal and system transition audits under the deployment's `correlation_id` (the plan request's `X-Request-ID`); a result echoing another request ID is refused, and an older agent's code-less result is read as `legacy` (docs/agent-protocol.md, Outcome codes)."

- [ ] **Step 3: `README.md`**

After the paragraph ending "is blocked (`mounts_unreported`)." add:

```markdown
Deployment frames now carry a `request_id`. An agent upgraded ahead of its server refuses every
deployment (`invalid_request`) until the server is upgraded; an older agent still deploys, and
its results show "The agent did not classify this outcome; upgrade the agent."

Usernames are unique ignoring case from this release (migration 30). If two accounts differ only
by case the server refuses to start and names them, for example
`usernames differ only by case: erin, Erin; rename or delete one of each pair before upgrading`;
nothing is changed. Stop the server (`docker compose stop kyyard`), copy the database, then
rename or delete one account of each pair in it and start again. For the default SQLite
volume (find its name with `docker volume ls | grep kyyard-data`):

    docker run --rm -it -v <volume>:/data alpine:3.24 sh -c 'apk add --no-cache sqlite >/dev/null && sqlite3 /data/ky_server.db'
    sqlite> SELECT id, username, sso_provider, created_at FROM users ORDER BY LOWER(username);
    sqlite> UPDATE users SET username = 'erin-old' WHERE id = '<id of the account to rename>';

On PostgreSQL run the same statements with `psql`. A single sign-on login whose username
matches an existing account ignoring case is refused with "The username … is taken by another
account": KyYard never links a provider's claimed name to an account it did not create.
```

- [ ] **Step 4: `docs/RESTORE.md`**

At the end of `## Step 3: put it in service` (after the **Bare binary** paragraph) add: "A capsule taken before migration 30 that holds usernames differing only by case makes the restored server refuse to start with `usernames differ only by case: …; rename or delete one of each pair before upgrading`; nothing is changed. Rename or delete one account of each pair in `data/ky_server.db` as the README's upgrade notes show, then start again."

- [ ] **Step 5: `KyYard-Implementation-Plan.md` §8**

In the PR C paragraph replace "a server-wide cap of 4 registry operations" with "a cap of 2 registry operations per organization and 8 server-wide". Replace the block from "Next: M7a PR D2 (hardening), carried from M6 and PR C review:" through its three bullets with:

```markdown
Implemented M7a PR D2 (`feat/bookkeeping`): deployment step and result outcomes are closed codes with a validated parameter (docs/agent-protocol.md, Outcome codes), and an older agent's code-less result reads as `legacy`; the plan's request ID is the deployment's correlation ID on its row, its frame and every audit row from plan to settle, and a result echoing another request ID is refused; an update plan takes the per-application guard an update check holds (409 `check_in_progress`; registry slots stay 2 per organization and 8 server-wide); migration 30 makes usernames unique ignoring case and refuses to start on existing case variants, naming them; single sign-on refuses a case-variant username instead of creating a second account; the first organization admin's status is read under a row lock on PostgreSQL; declared volume names need two characters. Spec `docs/superpowers/specs/2026-09-24-bookkeeping-design.md`.

Next: M7b (automated update policies, maintenance windows, health validation and eligible rollback).
```

and delete the now-duplicated line "Then M7b (automated update policies, maintenance windows, health validation and eligible rollback)."

- [ ] **Step 6: DOX closeout**

Re-read every `AGENTS.md` on the paths changed in Tasks 1–7 (`internal/agent`, `internal/runtime`, `internal/store`, `internal/api`, `internal/sso`, `web`, root) against the code as merged: each edited bullet names the codes, the correlation ID, the guard and the migration exactly as implemented, and no bullet still quotes a retired sentence (`grep -rn "the container no longer exists\|clock skew exceeds\|this agent is already applying\|unreadable result\|restarted after replacement began" --include=AGENTS.md .` prints nothing). The root `AGENTS.md` and its Child DOX Index are unchanged: no AGENTS.md was created, moved or removed, and root Verification already runs everything added here.

- [ ] **Step 7: Full gate**

Run: `gofmt -l internal cmd && make ci && PG=… make test-postgres`
Expected: `gofmt -l` prints nothing; `make ci` (tidy-check, lint, race tests, web tests, smoke) and the PostgreSQL suite pass. With Docker available also `KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 KY_TEST_DOCKER_INSPECTION_IMAGE=alpine:3.24 go test -race -count=1 ./internal/runtime/docker -run '^Test(Inspection|Deploy|Recheck|Remove)RealDocker$' && KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 go test -race -count=1 ./internal/api -run '^TestApplyRealDocker$'`.

- [ ] **Step 8: Commit**

```bash
git add docs README.md KyYard-Implementation-Plan.md
git commit -m "docs: outcome codes, correlation IDs and the username migration" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```
