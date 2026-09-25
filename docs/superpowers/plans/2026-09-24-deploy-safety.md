# Deploy Safety (PR D1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the six deploy-safety gaps from `KyYard-Implementation-Plan.md` §8: re-check each container right before its replacement, inspect the live host at plan time, refuse clock skew, record a started marker in the agent ledger, measure the frame at plan time, and gate plans on agent capabilities.

**Architecture:** The protocol gains `issued_at`, `MaxClockSkew`, the `recheck` step and a closed `unsupported` vocabulary on `ContainerInspection` (Task 1). The Docker adapter reports configuration as codes (Task 2), gives every inspection a verdict (Task 3), re-inspects before each replacement (Task 4) and calls a `started` hook the agent turns into a durable ledger entry (Task 5). The store builds the real frame at plan time (Task 6), reads endpoint capabilities and inventory clock skew (Task 7), and checks one live inspection per mapped service (Task 9) that the API gathers sequentially under a 10 s budget (Task 8). The web shows the new blockers, the recheck step and the inspection verdict (Task 10); docs close (Task 11).

**Tech Stack:** Go, Docker Engine API v1.41 (`/containers/{id}/json`, `/info`, `/images/{id}/json`), coder/websocket agent protocol, SQLite + PostgreSQL, React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-24-deploy-safety-design.md`

## Global Constraints

- `protocol.MaxClockSkew = 5 * time.Minute`. `DeploymentRequest` and `RemovalRequest` carry `IssuedAt time.Time` (`json:"issued_at"`); `Validate(now)` requires `|now − IssuedAt| ≤ MaxClockSkew` and `Deadline` in `(IssuedAt, IssuedAt + DeploymentLifetime]`, besides the existing checks against `now`. A zero `IssuedAt` fails validation.
- A skewed frame is answered with outcome `failed`, detail exactly `clock skew exceeds 5 minutes`, no step run.
- Preflight and plan top-level blocker `clock_skew` when the inventory row's `observed_at` and `received_at` differ by more than `MaxClockSkew`. `freshInventory` keeps its windows.
- New step `protocol.StepRecheck = "recheck"`: first phase-two step per service; `denied` with detail exactly `the container changed after the precondition` when identity differs from `Replaces` or the fresh `Config`, `HostConfig` or `Mounts` differ (`reflect.DeepEqual` on the decoded `inspectedForDeploy` fields) from phase one's. `State` and `NetworkSettings` are not compared. No rollback of services already replaced.
- `ContainerInspection.Unsupported []string` (`json:"unsupported"`), at most 32 entries (`protocol.MaxUnsupported = 32`), each from the closed vocabulary, in this order: `mount_type`, `anonymous_volume`, `volumes_from`, `volume_driver`, `mount_options`, `tmpfs`, `auto_remove`, `read_only_rootfs`, `privileged`, `capabilities`, `security_opt`, `devices`, `pid_mode`, `ipc_mode`, `user`, `runtime`, `resource_limits`, `ulimits`, `sysctls`, `device_requests`, `init`, `userns_mode`, `cgroup_parent`, `group_add`, `extra_hosts`, `dns`, `links`, `network`, `image_config`. `ConfigurationVerified` is `true` exactly when `Unsupported` is empty; `Validate` refuses a disagreeing pair, an unknown or repeated code, or more than 32.
- The deployment precondition detail for unsupported configuration is `unsupported: ` followed by the codes joined with `, ` (for example `unsupported: privileged, devices`).
- The agent inspection computes `network` with project network `<project>_default` from the container's `com.docker.compose.project` label; without the label `network` is reported only when the container is on a network other than `bridge`. The default runtime comes from `GET /info`, cached per `docker.Client` for one minute. No raw value enters the inspection.
- `Options.Deploy`/`Options.Remove` take a third parameter `started func()`, called once immediately before the first phase-two mutation; a run that fails in phase one never calls it. The deployer writes `{Started: now}` for the deployment ID with `writeDurable` before returning. On load, an entry with `Started` and no result becomes outcome `unknown`, detail exactly `the agent restarted after replacement began; inspect the host`, and is re-sent; a re-sent frame for its ID is answered from the ledger. Pruning never drops an entry with `Started` and no result.
- Frame caps: 320 KiB (`protocol.MaxDeploymentRequestBytes`) with `deployment.pull`, 192 KiB (`protocol.MaxDeploymentRequestBytesLegacy`) otherwise. `buildDeploymentFrame` is shared by plan and apply; at plan the frame is built with the real values and credentials, measured and dropped, never stored.
- Plan top-level blockers, exact strings: `frame_too_large`, `too_many_registry_hosts`, `frame_invalid`, `agent_deploy_unsupported`, `agent_pull_unsupported`, `agent_inspect_unsupported`, `clock_skew`. Per-service: `inspection_unavailable`, `replacement_identity_changed`, `configuration_unsupported` (`PreflightService.Unsupported` carries the codes).
- `planInspectionBudget = 10 * time.Second` for the whole sequential fan-out; each grant's `Expires` is the earlier of the budget and `protocol.InspectionLifetime`; one `inspection:` rate-limit attempt per plan; no fan-out without `container.inspect`; the response write deadline extends by the budget.
- `PlanRequest.MaxFrameBytes` and `PlanRequest.Inspections` are set by the API only (`json:"-"`); a client body naming either is 400.
- Apply keeps its own 501s and its frame check: capabilities can change between plan and apply.
- Every store behaviour is tested on SQLite and PostgreSQL. `web/dist` is rebuilt with `make build-web` and committed in the web task.
- Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. `gofmt -w` every edited Go file and check `gofmt -l internal cmd` is empty before each commit.

## Review Focus

1. A client that posts `inspections` (a forged verified observation) or `max_frame_bytes` in the plan body must be refused with 400, never trusted: the fields are server-set. Pinned in Task 6 (`max_frame_bytes`) and Task 8 (`inspections`).
2. An agent that never answers a plan-time inspection (hung daemon, wedged agent) must not hold the plan open: the plan answers 409 `inspection_unavailable` naming the service within the 10 s budget, and the abandoned grant is cancelled on the agent. Pinned in Task 9 (`TestPlanRefusesWhenTheAgentNeverAnswers`).
3. A container that only restarted between the phases (new `State`, new `NetworkSettings`, same configuration) must be replaced, not denied: a restart-looping service would otherwise be undeployable. Pinned in Task 4 (`TestRecheckIgnoresStateAndNetworkSettings`).
4. A container with many unsupported settings must still produce a result the server accepts: the joined precondition detail is bounded to 256 bytes and the result validates, rather than becoming "the runtime returned an unreadable result". Pinned in Task 2 (`TestDeployUnsupportedDetailStaysWithinTheStepBound`).
5. An agent built before this change answers an inspection with `configuration_verified: false` and no `unsupported` list: the plan must be refused as `inspection_unavailable`, never read as verified. Pinned in Task 9 (`TestPlanRefusesAnOlderAgentsInspection`).

---

### Task 1: Protocol — `issued_at`, `MaxClockSkew`, `recheck`, the unsupported vocabulary

**Files:**
- Modify: `internal/agent/protocol/deployment.go` (constants, `ErrClockSkew`, `IssuedAt` on both requests, `issued`, step cap)
- Modify: `internal/agent/protocol/inspection.go` (`CapabilityContainerInspect`, `Unsupported`, `UnsupportedCodes`, `MaxUnsupported`, `Validate`)
- Modify: `internal/runtime/docker/deploy.go:42-45`, `internal/runtime/docker/remove.go:18-21` (skew answered `failed`)
- Modify: `internal/agent/client/deployments.go:169-172` (skew answered `failed`), `internal/agent/client/connect.go:264` (capability constant)
- Modify: `internal/store/application_apply.go:99` and `:579` (`IssuedAt: now`)
- Modify: `internal/api/inspection.go:123` (capability constant)
- Test: `internal/agent/protocol/deployment_test.go`, `internal/agent/protocol/inspection_test.go`, `internal/runtime/docker/deploy_test.go`, `internal/runtime/docker/remove_test.go`, `internal/runtime/docker/deploy_integration_test.go`, `internal/agent/client/deployments_test.go`, `internal/api/inspection_test.go`
- Docs: `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  ```go
  // package protocol
  const StepRecheck = "recheck"
  const MaxClockSkew = 5 * time.Minute
  const MaxDeploymentResultSteps = 8*MaxDeploymentServices + MaxDeploymentVolumes
  const CapabilityContainerInspect = "container.inspect"
  const MaxUnsupported = 32
  var ErrClockSkew = errors.New("clock skew exceeds 5 minutes")
  var UnsupportedCodes = []string{ /* the 29 codes, spec order */ }
  // DeploymentRequest.IssuedAt, RemovalRequest.IssuedAt  time.Time `json:"issued_at"`
  // ContainerInspection.Unsupported []string `json:"unsupported"`
  ```
  Until Task 3 lands, the Docker adapter still returns `ConfigurationVerified: false` with no codes, which this task's `Validate` refuses: a live inspection answers `unavailable` between Task 1 and Task 3. The branch ships as one PR; do not release between them.

- [ ] **Step 1: Write the failing protocol tests**

In `internal/agent/protocol/deployment_test.go`, give both fixtures an issue time:

```go
func goodDeployment(now time.Time) DeploymentRequest {
	return DeploymentRequest{
		Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: "ep_1", Project: "shop", Revision: 2, IssuedAt: now, Deadline: now.Add(5 * time.Minute),
		Services: []DeploymentService{{
			Name: "web", ContainerName: "shop-web-1", ImageID: "sha256:" + strings.Repeat("a", 64),
			Replaces: InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000},
			Restart:  "always", Ports: []Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}}, Env: map[string]string{"TOKEN": "x"},
		}},
	}
}
```

```go
func goodRemoval(now time.Time) RemovalRequest {
	return RemovalRequest{
		Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(5 * time.Minute),
		Containers: []RemovalTarget{
			{Service: "web", Target: InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}},
			{Service: "unmapped-0123456789ab", Target: InspectionTarget{ContainerID: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("d", 64), CreatedUnix: 1700000000}},
		},
	}
}
```

In `TestDeploymentResultValidation` replace the `"too many steps"` entry with:

```go
		"too many steps":           func(r *DeploymentResult) { r.Steps = make([]DeploymentStep, MaxDeploymentResultSteps+1) },
```

and add, after the `for _, step := range []string{StepImage, StepPull}` loop:

```go
	recheck := good
	recheck.Outcome, recheck.Detail = OutcomeDenied, "service web, step recheck: the container changed after the precondition"
	recheck.Steps = []DeploymentStep{{Service: "web", Step: StepRecheck, Outcome: OutcomeDenied, Detail: "the container changed after the precondition"}}
	recheck.Services = []DeploymentIdentity{}
	if err := recheck.Validate(); err != nil {
		t.Fatalf("recheck step refused: %v", err)
	}
```

In `TestDeploymentResultWorstCaseFitsTheFrame` change the step list to include the recheck (eight per service now, plus the 64 volume steps):

```go
	steps := []string{StepPrecondition, StepPull, StepRecheck, StepRename, StepCreate, StepStop, StepStart, StepRemove}
```

Append two tests to `internal/agent/protocol/deployment_test.go`:

```go
// The issue time bounds skew between the server's clock and the agent's, and the deadline is
// measured from it as well as from the agent's now.
func TestDeploymentRequestIssuedAt(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		mutate func(*DeploymentRequest)
		skew   bool
	}{
		"missing":          {func(r *DeploymentRequest) { r.IssuedAt = time.Time{} }, false},
		"issued behind":    {func(r *DeploymentRequest) { r.IssuedAt = now.Add(-MaxClockSkew - time.Second); r.Deadline = now.Add(time.Minute) }, true},
		"issued ahead":     {func(r *DeploymentRequest) { r.IssuedAt = now.Add(MaxClockSkew + time.Second); r.Deadline = r.IssuedAt.Add(time.Minute) }, true},
		"deadline at issue": {func(r *DeploymentRequest) { r.Deadline = r.IssuedAt }, false},
		"deadline past lifetime from issue": {func(r *DeploymentRequest) {
			r.IssuedAt = now.Add(-time.Minute)
			r.Deadline = r.IssuedAt.Add(DeploymentLifetime + time.Second)
		}, false},
	} {
		r := goodDeployment(now)
		tc.mutate(&r)
		err := r.Validate(now)
		if err == nil || errors.Is(err, ErrClockSkew) != tc.skew {
			t.Fatalf("%s: %v", name, err)
		}
	}
	r := goodDeployment(now)
	r.IssuedAt = now.Add(-MaxClockSkew + time.Second)
	if err := r.Validate(now); err != nil {
		t.Fatalf("skew inside the bound refused: %v", err)
	}
	if ErrClockSkew.Error() != "clock skew exceeds 5 minutes" {
		t.Fatalf("detail: %q", ErrClockSkew.Error())
	}
}

func TestRemovalRequestIssuedAt(t *testing.T) {
	now := time.Now()
	r := goodRemoval(now)
	r.IssuedAt = time.Time{}
	if r.Validate(now) == nil {
		t.Fatal("missing issue time accepted")
	}
	r = goodRemoval(now)
	r.IssuedAt = now.Add(-MaxClockSkew - time.Second)
	r.Deadline = now.Add(time.Minute)
	if err := r.Validate(now); !errors.Is(err, ErrClockSkew) {
		t.Fatalf("skewed removal: %v", err)
	}
}
```

Add `"errors"` to that file's imports.

In `internal/agent/protocol/inspection_test.go` replace the `good` fixture and the first bad mutation, and add a vocabulary test:

```go
	good := ContainerInspection{Target: target, ObservedAt: time.Now(), State: "running", RestartPolicy: "no", NetworkMode: "none", ImagePlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}, ConfigurationVerified: true, Unsupported: []string{}}
```

```go
		func(r *ContainerInspection) { r.ConfigurationVerified = false },
```

```go
// ConfigurationVerified says exactly that no code was reported, and codes come from one closed list.
func TestInspectionUnsupportedVocabulary(t *testing.T) {
	target := InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	base := ContainerInspection{Target: target, ObservedAt: time.Now(), State: "running", RestartPolicy: "no", NetworkMode: "bridge", ImagePlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}}
	for name, tc := range map[string]struct {
		verified bool
		codes    []string
		ok       bool
	}{
		"verified, no codes":     {true, []string{}, true},
		"verified, nil codes":    {true, nil, true},
		"unsupported with codes": {false, []string{"privileged", "devices"}, true},
		"every code":             {false, UnsupportedCodes, true},
		"verified with a code":   {true, []string{"privileged"}, false},
		"unverified, no codes":   {false, []string{}, false},
		"unknown code":           {false, []string{"secret-canary"}, false},
		"repeated code":          {false, []string{"privileged", "privileged"}, false},
		"too many":               {false, make([]string, MaxUnsupported+1), false},
	} {
		r := base
		r.ConfigurationVerified, r.Unsupported = tc.verified, tc.codes
		if err := r.Validate(target, time.Now()); (err == nil) != tc.ok {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(UnsupportedCodes) != 29 || len(UnsupportedCodes) > MaxUnsupported || UnsupportedCodes[0] != "mount_type" || UnsupportedCodes[28] != "image_config" {
		t.Fatalf("vocabulary: %v", UnsupportedCodes)
	}
}
```

- [ ] **Step 2: Run the protocol tests to verify they fail**

Run: `go test -count=1 ./internal/agent/protocol/`
Expected: FAIL to compile (`unknown field IssuedAt`, `undefined: MaxClockSkew`, `undefined: ErrClockSkew`, `undefined: StepRecheck`, `undefined: MaxDeploymentResultSteps`, `unknown field Unsupported`, `undefined: UnsupportedCodes`, `undefined: MaxUnsupported`).

- [ ] **Step 3: Implement the protocol**

In `internal/agent/protocol/deployment.go`, add to the constant block (after `StepRemove`):

```go
	StepRecheck                     = "recheck" // re-inspects the old container at the start of its replacement
	// MaxClockSkew bounds the difference between the server's clock when it built a frame and
	// the agent's when it reads one, and between an inventory's observed and received times.
	MaxClockSkew = 5 * time.Minute
	// MaxDeploymentResultSteps is eight steps per service (precondition, image or pull,
	// recheck, rename, create, stop, start, remove) plus one per volume.
	MaxDeploymentResultSteps = 8*MaxDeploymentServices + MaxDeploymentVolumes
```

Add below the `var (...)` regexp block:

```go
// ErrClockSkew is a frame whose issue time is more than MaxClockSkew from the reader's clock.
// Its text is the detail an agent answers with.
var ErrClockSkew = errors.New("clock skew exceeds 5 minutes")
```

and add `StepRecheck: true` to `deploymentSteps`.

Add the field to `DeploymentRequest` after `Revision`:

```go
	// IssuedAt is the server's clock when it built the frame; Deadline is measured from it too.
	IssuedAt time.Time `json:"issued_at"`
```

and to `RemovalRequest` after `Project`:

```go
	IssuedAt   time.Time       `json:"issued_at"`
```

Add the shared check:

```go
// issued refuses a frame with no issue time, one issued more than MaxClockSkew from now (the
// two clocks disagree), or a deadline outside (issued, issued+DeploymentLifetime].
func issued(at, deadline, now time.Time) error {
	if at.IsZero() {
		return errors.New("missing issue time")
	}
	if d := now.Sub(at); d > MaxClockSkew || d < -MaxClockSkew {
		return ErrClockSkew
	}
	if !deadline.After(at) || deadline.After(at.Add(DeploymentLifetime)) {
		return errors.New("invalid deadline for the issue time")
	}
	return nil
}
```

In `DeploymentRequest.Validate`, directly after the identity check and before the existing deadline check (skew must be reported as skew, not as a past deadline):

```go
	if err := issued(r.IssuedAt, r.Deadline, now); err != nil {
		return err
	}
```

and the same three lines in `RemovalRequest.Validate` after its identity check. In `DeploymentResult.Validate` replace `len(r.Steps) > 8*MaxDeploymentServices` with `len(r.Steps) > MaxDeploymentResultSteps`.

In `internal/agent/protocol/inspection.go`, add `"slices"` to the imports, add to the `const (...)` block that holds `TypeInspectionOpen`:

```go
	CapabilityContainerInspect = "container.inspect"
	MaxUnsupported             = 32
```

replace the `ContainerInspection` comment and last field:

```go
// ContainerInspection is an allowlisted observation, not a recreation spec.
// Counts omit mount paths/network names. Environment, labels, argv, healthcheck
// commands, raw configuration and hashes of those values never enter this type.
// Unsupported names, as codes from UnsupportedCodes, configuration a recreate from the
// definition would drop; ConfigurationVerified is true exactly when it is empty.
```

```go
	Unsupported           []string         `json:"unsupported"`
	ConfigurationVerified bool             `json:"configuration_verified"`
```

add after `MountCounts`:

```go
// UnsupportedCodes is the closed vocabulary of ContainerInspection.Unsupported, in the order the
// Docker adapter reports them. Each is configuration the definition cannot express.
var UnsupportedCodes = []string{"mount_type", "anonymous_volume", "volumes_from", "volume_driver", "mount_options", "tmpfs", "auto_remove", "read_only_rootfs", "privileged", "capabilities", "security_opt", "devices", "pid_mode", "ipc_mode", "user", "runtime", "resource_limits", "ulimits", "sysctls", "device_requests", "init", "userns_mode", "cgroup_parent", "group_add", "extra_hosts", "dns", "links", "network", "image_config"}

// knownCodes accepts at most MaxUnsupported distinct codes from UnsupportedCodes.
func knownCodes(codes []string) bool {
	if len(codes) > MaxUnsupported {
		return false
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] || !slices.Contains(UnsupportedCodes, c) {
			return false
		}
		seen[c] = true
	}
	return true
}
```

and in `ContainerInspection.Validate` replace `r.ConfigurationVerified ||` in the first condition with `r.ConfigurationVerified != (len(r.Unsupported) == 0) || !knownCodes(r.Unsupported) ||`.

- [ ] **Step 4: Run the protocol tests to verify they pass**

Run: `go test -race -count=1 ./internal/agent/protocol/`
Expected: PASS; `TestDeploymentResultWorstCaseFitsTheFrame` logs about 148 KiB for "all succeeded", under 160 KiB.

- [ ] **Step 5: Write the failing consumer tests**

`internal/runtime/docker/deploy_test.go`, helper `request`:

```go
func request(services ...protocol.DeploymentService) protocol.DeploymentRequest {
	return protocol.DeploymentRequest{Deployment: deploymentID, Endpoint: "ep_1", Project: "shop", Revision: 2, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Services: services}
}
```

and append:

```go
// A frame whose issue time is far from the host clock is failed with the skew detail and no call.
func TestDeployReportsClockSkewWithoutCalling(t *testing.T) {
	f := newFakeDeployEngine(t)
	req := request(webService())
	req.IssuedAt = time.Now().Add(-protocol.MaxClockSkew - time.Minute)
	res := f.client().Deploy(context.Background(), req)
	if res.Outcome != protocol.OutcomeFailed || res.Detail != "clock skew exceeds 5 minutes" || len(res.Steps) != 0 || len(f.calls) != 0 {
		t.Fatalf("skewed frame: %+v calls=%v", res, f.steps())
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
}
```

`internal/runtime/docker/remove_test.go`, helper `removal` first line:

```go
	req := protocol.RemovalRequest{Deployment: deploymentID, Endpoint: "ep_1", Project: "shop", IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute)}
```

and append:

```go
func TestRemoveReportsClockSkewWithoutCalling(t *testing.T) {
	f := newFakeRemoveEngine(t)
	req := removal(oldID)
	req.IssuedAt = time.Now().Add(protocol.MaxClockSkew + time.Minute)
	req.Deadline = req.IssuedAt.Add(time.Minute)
	res := f.client().Remove(context.Background(), req)
	if res.Outcome != protocol.OutcomeFailed || res.Detail != "clock skew exceeds 5 minutes" || len(f.calls) != 0 {
		t.Fatalf("skewed removal: %+v", res)
	}
}
```

`internal/runtime/docker/deploy_integration_test.go`: add `IssuedAt: time.Now(),` to the `protocol.DeploymentRequest{...}` literal (line 116) and the `protocol.RemovalRequest{...}` literal (line 291), before `Deadline`.

`internal/agent/client/deployments_test.go`: add `IssuedAt: time.Now(),` before `Deadline` in `testRequest` and `testRemoval`, and append:

```go
// A skewed frame is answered failed with the fixed detail, runs nothing and is not recorded.
func TestDeployerReportsClockSkewAsFailed(t *testing.T) {
	ran := false
	d := newDeployer(context.Background(), t.TempDir(), &Options{Deploy: func(context.Context, protocol.DeploymentRequest) protocol.DeploymentResult {
		ran = true
		return protocol.DeploymentResult{}
	}})
	out := make(chan outFrame, 1)
	req := testRequest("ep_1")
	req.IssuedAt = time.Now().Add(protocol.MaxClockSkew + time.Minute)
	req.Deadline = req.IssuedAt.Add(time.Minute)
	raw, _ := json.Marshal(req)
	d.handleApply(context.Background(), "ep_1", raw, out)
	var res protocol.DeploymentResult
	if decodeResult(<-out, &res) != nil || res.Deployment != req.Deployment || res.Outcome != protocol.OutcomeFailed || res.Detail != "clock skew exceeds 5 minutes" || res.Validate() != nil {
		t.Fatalf("skewed frame: %+v", res)
	}
	d.mu.Lock()
	_, recorded := d.done[req.Deployment]
	d.mu.Unlock()
	if ran || recorded {
		t.Fatalf("ran=%v recorded=%v", ran, recorded)
	}
}
```

`internal/api/inspection_test.go`: the agent now reports a verdict. Replace `inspectionReply`:

```go
func inspectionReply(req protocol.InspectionOpen) protocol.InspectionResult {
	return protocol.InspectionResult{Request: req.Request, Status: "ok", Result: &protocol.ContainerInspection{Target: req.Target, ObservedAt: time.Now(), State: "running", RestartPolicy: "no", NetworkMode: "bridge", ImagePlatform: protocol.ImagePlatform{OS: "linux", Architecture: "amd64"}, Ports: []protocol.Port{}, Unsupported: []string{}, ConfigurationVerified: true}}
}
```

in `TestInspectionAPIReadOnlyAndScoped` replace `got.ConfigurationVerified ||` with `!got.ConfigurationVerified ||`, and in `TestInspectionAPIRefusesUntrustedResult` replace the `"verified"` case body with `reply.Result.ConfigurationVerified = false` (verified false with no codes disagrees).

- [ ] **Step 6: Run the consumer tests to verify they fail**

Run: `go test -count=1 ./internal/runtime/docker/ ./internal/agent/client/ -run 'ClockSkew'`
Expected: FAIL: `TestDeployReportsClockSkewWithoutCalling` gets outcome `denied`, detail `the deployment request is invalid`; the client test gets `denied` `invalid deployment request`.

- [ ] **Step 7: Implement the consumers**

`internal/runtime/docker/deploy.go`, in `Client.Deploy` replace the validation block (add `"errors"` to imports):

```go
	if err := req.Validate(time.Now()); err != nil {
		res.Outcome, res.Detail = protocol.OutcomeDenied, "the deployment request is invalid"
		if errors.Is(err, protocol.ErrClockSkew) {
			res.Outcome, res.Detail = protocol.OutcomeFailed, protocol.ErrClockSkew.Error()
		}
		return res
	}
```

`internal/runtime/docker/remove.go`, same shape (add `"errors"`):

```go
	if err := req.Validate(time.Now()); err != nil {
		res.Outcome, res.Detail = protocol.OutcomeDenied, "the removal request is invalid"
		if errors.Is(err, protocol.ErrClockSkew) {
			res.Outcome, res.Detail = protocol.OutcomeFailed, protocol.ErrClockSkew.Error()
		}
		return res
	}
```

`internal/agent/client/deployments.go`, in `run` replace the validate block (add `"errors"`):

```go
	if err := validate(time.Now()); err != nil {
		res := denied(id, "invalid deployment request")
		if errors.Is(err, protocol.ErrClockSkew) {
			res.Outcome, res.Detail = protocol.OutcomeFailed, err.Error()
		}
		go send(sessionCtx, out, resultFrame(res))
		return
	}
```

`internal/store/application_apply.go`: in `ApplyDeployment` the request literal becomes

```go
		req = &protocol.DeploymentRequest{Deployment: d.ID, Endpoint: d.EndpointID, Project: d.Plan.Project, Revision: d.Revision, IssuedAt: now, Deadline: now.Add(DeploymentApplyDeadline), Services: []protocol.DeploymentService{}, Volumes: d.Plan.Volumes}
```

and in `RemoveApplication`

```go
		req = &protocol.RemovalRequest{Deployment: id, Endpoint: endpoint, Project: project, IssuedAt: now, Deadline: now.Add(DeploymentApplyDeadline), Containers: []protocol.RemovalTarget{}}
```

`internal/agent/client/connect.go:264`: `capabilities = append(capabilities, protocol.CapabilityContainerInspect)`. `internal/api/inspection.go:123`: `if !slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspect) {`.

- [ ] **Step 8: Run everything touched**

Run: `go test -race -count=1 ./internal/agent/... ./internal/runtime/docker/ && go test -count=1 ./internal/store/ ./internal/api/`
Expected: PASS (the store's apply tests now see `issued_at` in every frame; Docker-gated tests skip locally).

- [ ] **Step 9: DOX and commit**

`internal/agent/AGENTS.md`: in the `protocol.DeploymentRequest`/`DeploymentResult` bullet replace "a deadline at most `DeploymentLifetime` (15 minutes) out" with "an `IssuedAt` (`issued_at`, the server's clock at build) within `MaxClockSkew` (5 minutes) of the reader's clock, else `ErrClockSkew`, which the adapter and the deployer answer `failed` `clock skew exceeds 5 minutes` with no step and no ledger entry, and a deadline in `(IssuedAt, IssuedAt + DeploymentLifetime]` and at most `DeploymentLifetime` (15 minutes) past now", apply the same to `RemovalRequest`, and replace "bounds steps at 8×`MaxDeploymentServices` (seven per service plus one per volume fits)" with "bounds steps at `MaxDeploymentResultSteps` (8×`MaxDeploymentServices` + `MaxDeploymentVolumes`: eight per service with `recheck`, plus one per volume)" and "accepts steps `pull` and `volume`" with "accepts steps `pull`, `volume` and `recheck`". In the inspection bullet replace "configuration_verified remains false and inspection grants no deployment authority" with "`unsupported` lists codes from `protocol.UnsupportedCodes` (at most `MaxUnsupported`, 32, distinct) and `configuration_verified` is true exactly when it is empty; inspection grants no deployment authority".

```bash
gofmt -l internal cmd
git add internal/agent internal/runtime/docker internal/store/application_apply.go internal/api/inspection.go internal/api/inspection_test.go
git commit -m "feat(protocol): issue time, clock skew bound, recheck step and unsupported codes" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Runtime — `undescribed` returns codes

**Files:**
- Modify: `internal/runtime/docker/deploy.go:185-299` (`imageDefaults.differs`, `reported`, `undescribed`, `unsupported`), `:313-360` (the precondition)
- Test: `internal/runtime/docker/deploy_test.go` (`TestDeployPreconditionsRefuseBeforeTouchingAnything`), `internal/runtime/docker/deploy_volume_test.go` (`TestDeployVolumeRefusalDetails`, `TestDeployBindPrecondition`)
- Docs: `internal/runtime/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.UnsupportedCodes` (Task 1) as the order and spelling of every code.
- Produces (package `docker`, used by Tasks 3 and 4):
  ```go
  func reported(in inspectedForDeploy) bool
  func undescribed(in inspectedForDeploy, projectNetwork, defaultRuntime string) []string // in must be reported; never nil
  func unsupported(codes []string) string // "unsupported: a, b"
  func (a imageDefaults) differs(b imageDefaults) bool
  ```

- [ ] **Step 1: Write the failing tests**

In `internal/runtime/docker/deploy_test.go` replace the table and loop of `TestDeployPreconditionsRefuseBeforeTouchingAnything` (keep the `host`, `unset` and `networks` helpers above it and the two trailing checks below it) with:

```go
	const (
		notTheOne  = "the container is not the one this plan was decided about"
		unreported = "the runtime did not report the container's full configuration"
		imageGone  = "the container's image is no longer present"
	)
	for name, tc := range map[string]struct {
		mutate func(*fakeDeployEngine)
		calls  int // after GET /info: 1 when decided from the container alone, 2 when the old image was read
		detail string
	}{
		"image":                  {func(f *fakeDeployEngine) { f.oldContainer["Image"] = newImage }, 1, notTheOne},
		"created":                {func(f *fakeDeployEngine) { f.oldContainer["Created"] = "2023-11-14T22:13:21Z" }, 1, notTheOne},
		"absent HostConfig":      {unset("HostConfig"), 1, unreported},
		"absent Config":          {unset("Config"), 1, unreported},
		"absent NetworkSettings": {unset("NetworkSettings"), 1, unreported},
		"absent Mounts":          {unset("Mounts"), 1, unreported},
		"absent Privileged": {func(f *fakeDeployEngine) {
			delete(f.oldContainer["HostConfig"].(map[string]any), "Privileged")
		}, 1, unreported},
		"tmpfs mount": {func(f *fakeDeployEngine) {
			f.oldContainer["Mounts"] = []any{map[string]any{"Type": "tmpfs", "Destination": "/run"}}
		}, 1, "unsupported: mount_type"},
		"npipe mount": {func(f *fakeDeployEngine) { f.oldContainer["Mounts"] = []any{map[string]any{"Type": "npipe"}} }, 1, "unsupported: mount_type"},
		"anonymous volume": {func(f *fakeDeployEngine) {
			f.oldContainer["Mounts"] = []any{map[string]any{"Type": "volume", "Name": strings.Repeat("9f", 32), "Destination": "/var/lib/postgresql/data", "RW": true}}
		}, 1, "unsupported: anonymous_volume"},
		"volumes from":  {host("VolumesFrom", []string{"shop-data-1"}), 1, "unsupported: volumes_from"},
		"volume driver": {host("VolumeDriver", "nfs"), 1, "unsupported: volume_driver"},
		"bind propagation": {func(f *fakeDeployEngine) {
			f.oldContainer["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/srv", "Destination": "/srv", "RW": true, "Propagation": "rshared"}}
		}, 1, "unsupported: mount_options"},
		"volume nocopy mode": {func(f *fakeDeployEngine) {
			f.oldContainer["Mounts"] = []any{map[string]any{"Type": "volume", "Name": "shop_data", "Destination": "/data", "RW": true, "Mode": "nocopy"}}
		}, 1, "unsupported: mount_options"},
		"volume subpath":       {host("Mounts", []any{map[string]any{"Type": "volume", "Source": "shop_data", "Target": "/data", "VolumeOptions": map[string]any{"Subpath": "app"}}}), 1, "unsupported: mount_options"},
		"volume nocopy":        {host("Mounts", []any{map[string]any{"Type": "volume", "Source": "shop_data", "Target": "/data", "VolumeOptions": map[string]any{"NoCopy": true}}}), 1, "unsupported: mount_options"},
		"volume driver config": {host("Mounts", []any{map[string]any{"Type": "volume", "Source": "shop_data", "Target": "/data", "VolumeOptions": map[string]any{"DriverConfig": map[string]any{"Name": "nfs"}}}}), 1, "unsupported: mount_options"},
		"bind non-recursive":   {host("Mounts", []any{map[string]any{"Type": "bind", "Source": "/srv", "Target": "/srv", "BindOptions": map[string]any{"NonRecursive": true}}}), 1, "unsupported: mount_options"},
		"bind api propagation": {host("Mounts", []any{map[string]any{"Type": "bind", "Source": "/srv", "Target": "/srv", "BindOptions": map[string]any{"Propagation": "rslave"}}}), 1, "unsupported: mount_options"},
		"tmpfs":                {host("Tmpfs", map[string]string{"/run": "rw"}), 1, "unsupported: tmpfs"},
		"auto-remove":          {host("AutoRemove", true), 1, "unsupported: auto_remove"},
		"read-only root":       {host("ReadonlyRootfs", true), 1, "unsupported: read_only_rootfs"},
		"privileged":           {host("Privileged", true), 1, "unsupported: privileged"},
		"cap add":              {host("CapAdd", []string{"NET_ADMIN"}), 1, "unsupported: capabilities"},
		"cap drop":             {host("CapDrop", []string{"ALL"}), 1, "unsupported: capabilities"},
		"security opt":         {host("SecurityOpt", []string{"no-new-privileges"}), 1, "unsupported: security_opt"},
		"devices":              {host("Devices", []any{map[string]any{"PathOnHost": "/dev/fuse"}}), 1, "unsupported: devices"},
		"pid mode":             {host("PidMode", "host"), 1, "unsupported: pid_mode"},
		"ipc mode host":        {host("IpcMode", "host"), 1, "unsupported: ipc_mode"},
		"ipc container":        {host("IpcMode", "container:"+oldID), 1, "unsupported: ipc_mode"},
		"user":                 {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["User"] = "1000" }, 1, "unsupported: user"},
		"network mode":         {host("NetworkMode", "host"), 1, "unsupported: network"},
		"two networks":         {networks("bridge", "shop_default"), 1, "unsupported: network"},
		"other network":        {networks("shop_default"), 1, "unsupported: network"},
		"cmd":                  {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["Cmd"] = []string{"sleep", "300"} }, 2, "unsupported: image_config"},
		"entrypoint": {func(f *fakeDeployEngine) {
			f.oldContainer["Config"].(map[string]any)["Entrypoint"] = []string{"/bin/sh", "-c"}
		}, 2, "unsupported: image_config"},
		"old image gone":     {func(f *fakeDeployEngine) { f.oldImageStatus = 404 }, 2, imageGone},
		"runtime":            {host("Runtime", "runsc"), 1, "unsupported: runtime"},
		"memory":             {host("Memory", 1<<30), 1, "unsupported: resource_limits"},
		"memory swap":        {host("MemorySwap", 1<<30), 1, "unsupported: resource_limits"},
		"memory reservation": {host("MemoryReservation", 1<<30), 1, "unsupported: resource_limits"},
		"nano cpus":          {host("NanoCpus", 500000000), 1, "unsupported: resource_limits"},
		"cpu shares":         {host("CpuShares", 512), 1, "unsupported: resource_limits"},
		"cpu quota":          {host("CpuQuota", 50000), 1, "unsupported: resource_limits"},
		"cpuset":             {host("CpusetCpus", "0"), 1, "unsupported: resource_limits"},
		"pids":               {host("PidsLimit", 100), 1, "unsupported: resource_limits"},
		"ulimits":            {host("Ulimits", []any{map[string]any{"Name": "nofile", "Soft": 1024, "Hard": 1024}}), 1, "unsupported: ulimits"},
		"sysctls":            {host("Sysctls", map[string]string{"net.ipv4.ip_forward": "1"}), 1, "unsupported: sysctls"},
		"device requests":    {host("DeviceRequests", []any{map[string]any{"Driver": "nvidia", "Count": -1}}), 1, "unsupported: device_requests"},
		"init":               {host("Init", true), 1, "unsupported: init"},
		"userns":             {host("UsernsMode", "host"), 1, "unsupported: userns_mode"},
		"cgroup parent":      {host("CgroupParent", "/custom"), 1, "unsupported: cgroup_parent"},
		"group add":          {host("GroupAdd", []string{"audio"}), 1, "unsupported: group_add"},
		"extra hosts":        {host("ExtraHosts", []string{"db:10.0.0.2"}), 1, "unsupported: extra_hosts"},
		"dns":                {host("Dns", []string{"1.1.1.1"}), 1, "unsupported: dns"},
		"dns options":        {host("DnsOptions", []string{"ndots:1"}), 1, "unsupported: dns"},
		"dns search":         {host("DnsSearch", []string{"lan"}), 1, "unsupported: dns"},
		"links":              {host("Links", []string{"/shop-db-1:/shop-web-1/db"}), 1, "unsupported: links"},
		"healthcheck differs": {func(f *fakeDeployEngine) {
			f.oldContainer["Config"].(map[string]any)["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}}
		}, 2, "unsupported: image_config"},
		"working dir differs": {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["WorkingDir"] = "/srv" }, 2, "unsupported: image_config"},
		"healthcheck interval differs": {func(f *fakeDeployEngine) {
			f.oldImageConfig["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}, "Interval": 30000000000}
			f.oldContainer["Config"].(map[string]any)["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}, "Interval": 5000000000}
		}, 2, "unsupported: image_config"},
		"runtime not the daemon default": {func(f *fakeDeployEngine) {
			f.defaultRuntime = "nvidia"
			f.oldContainer["HostConfig"].(map[string]any)["Runtime"] = "runsc"
		}, 1, "unsupported: runtime"},
		"stop signal differs": {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["StopSignal"] = "SIGINT" }, 2, "unsupported: image_config"},
		"privileged with devices": {func(f *fakeDeployEngine) {
			host("Privileged", true)(f)
			host("Devices", []any{map[string]any{"PathOnHost": "/dev/fuse"}})(f)
		}, 1, "unsupported: privileged, devices"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			db := webService()
			db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", strings.Repeat("f", 64), nil
			res := f.client().Deploy(context.Background(), request(webService(), db))
			if res.Outcome != protocol.OutcomeDenied || len(f.calls) != 1+tc.calls || res.Steps[0].Detail != tc.detail {
				t.Fatalf("%s: %+v calls=%v", name, res, f.steps())
			}
			for _, c := range f.calls {
				if c.Method != "GET" {
					t.Fatalf("%s: mutating call %+v", name, c)
				}
			}
			if res.Steps[0].Outcome != protocol.OutcomeDenied || res.Steps[1].Outcome != protocol.OutcomeSkipped || res.Steps[len(res.Steps)-1].Service != "db" || res.Steps[len(res.Steps)-1].Outcome != protocol.OutcomeSkipped || len(res.Steps) != 14 {
				t.Fatalf("%s steps: %+v", name, res.Steps)
			}
			if !strings.Contains(res.Detail, "service web, step precondition") {
				t.Fatalf("detail: %q", res.Detail)
			}
		})
	}
```

Append (Review Focus 4):

```go
// Every setting at once still yields a step detail within the wire bound and a valid result:
// an oversized detail would make the agent replace the result with "unreadable".
func TestDeployUnsupportedDetailStaysWithinTheStepBound(t *testing.T) {
	f := newFakeDeployEngine(t)
	h := f.oldContainer["HostConfig"].(map[string]any)
	for k, v := range map[string]any{"Privileged": true, "AutoRemove": true, "ReadonlyRootfs": true, "Tmpfs": map[string]string{"/run": "rw"}, "CapAdd": []string{"ALL"}, "SecurityOpt": []string{"x"}, "Devices": []any{map[string]any{}}, "PidMode": "host", "IpcMode": "host", "Runtime": "runsc", "Memory": 1, "Ulimits": []any{map[string]any{}}, "Sysctls": map[string]string{"a": "b"}, "DeviceRequests": []any{map[string]any{}}, "Init": true, "UsernsMode": "host", "CgroupParent": "/x", "GroupAdd": []string{"a"}, "ExtraHosts": []string{"a:1.1.1.1"}, "Dns": []string{"1.1.1.1"}, "Links": []string{"a:b"}, "NetworkMode": "host", "VolumesFrom": []string{"x"}, "VolumeDriver": "nfs"} {
		h[k] = v
	}
	f.oldContainer["Config"].(map[string]any)["User"] = "1000"
	f.oldContainer["Mounts"] = []any{map[string]any{"Type": "tmpfs", "Destination": "/run"}, map[string]any{"Type": "volume", "Name": strings.Repeat("ab", 32), "Destination": "/d", "Mode": "nocopy"}}
	res := f.client().Deploy(context.Background(), request(webService()))
	if res.Outcome != protocol.OutcomeDenied || !strings.HasPrefix(res.Steps[0].Detail, "unsupported: mount_type, anonymous_volume") || len(res.Steps[0].Detail) > protocol.MaxDeploymentStepDetailBytes {
		t.Fatalf("detail %d bytes: %q", len(res.Steps[0].Detail), res.Steps[0].Detail)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
}
```

In `internal/runtime/docker/deploy_volume_test.go`, `TestDeployVolumeRefusalDetails`: rename the four map keys to the codes (`"anonymous volumes"` → `"anonymous_volume"`, `"volumes-from"` → `"volumes_from"`, `"volume driver"` → `"volume_driver"`, `"mount options"` → `"mount_options"`) and change the check to

```go
		if res.Outcome != protocol.OutcomeDenied || res.Steps[0].Detail != "unsupported: "+detail {
```

In `TestDeployBindPrecondition`, the `"tmpfs beside them"` case expects `"unsupported: mount_type"`. The `"absent, container has mounts"` case (line 404) keeps `the container has configuration the definition cannot express: mounts`: that is the pre-mounts server rule, not a code.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/runtime/docker/ -run 'Precondition|RefusalDetails|StepBound'`
Expected: FAIL with details like `the container has configuration the definition cannot express: privileged` where `unsupported: privileged` is wanted.

- [ ] **Step 3: Implement**

In `internal/runtime/docker/deploy.go` replace `imageDefaults.differs` with:

```go
// differs reports a Cmd, Entrypoint, Healthcheck, WorkingDir or StopSignal the container was
// given at run time: recreation from the image would drop it (code image_config).
func (a imageDefaults) differs(b imageDefaults) bool {
	return !slices.Equal(a.Cmd, b.Cmd) || !slices.Equal(a.Entrypoint, b.Entrypoint) ||
		a.Healthcheck.none() != b.Healthcheck.none() || (!a.Healthcheck.none() && !reflect.DeepEqual(a.Healthcheck, b.Healthcheck)) ||
		a.WorkingDir != b.WorkingDir || a.StopSignal != b.StopSignal
}
```

Replace `undescribed` (keep `const cannotExpress`, still used for a frame without the mounts key) with:

```go
// reported says the Engine returned every field undescribed reads through a pointer; an absent
// one is refused rather than read as its zero value.
func reported(in inspectedForDeploy) bool {
	h := in.HostConfig
	return h != nil && in.Config != nil && in.NetworkSettings != nil && in.Mounts != nil && h.Privileged != nil && h.AutoRemove != nil && h.ReadonlyRootfs != nil
}

// undescribed lists, as codes of protocol.UnsupportedCodes in its order, the configuration
// recreation would drop. The definition expresses image, env, ports, restart, the project
// network and volume and bind mounts only; log configuration and settings outside this list
// are not compared, and image_config is the caller's, compared against the image. in must be
// reported.
func undescribed(in inspectedForDeploy, projectNetwork, defaultRuntime string) []string {
	h, n, mounts := in.HostConfig, in.NetworkSettings, *in.Mounts
	network := projectNetwork
	if h.NetworkMode == "default" || h.NetworkMode == "bridge" {
		network = "bridge"
	}
	_, onNetwork := n.Networks[network]
	checks := []struct {
		code  string
		found bool
	}{
		{"mount_type", slices.ContainsFunc(mounts, func(m inspectedMount) bool { return m.Type != "volume" && m.Type != "bind" })},
		{"anonymous_volume", slices.ContainsFunc(mounts, func(m inspectedMount) bool { return m.Type == "volume" && anonymousVolume.MatchString(m.Name) })},
		{"volumes_from", len(h.VolumesFrom) > 0},
		{"volume_driver", h.VolumeDriver != "" && h.VolumeDriver != "local"},
		{"mount_options", mountOptions(in)},
		{"tmpfs", len(h.Tmpfs) > 0},
		{"auto_remove", *h.AutoRemove},
		{"read_only_rootfs", *h.ReadonlyRootfs},
		{"privileged", *h.Privileged},
		{"capabilities", len(h.CapAdd) > 0 || len(h.CapDrop) > 0},
		{"security_opt", len(h.SecurityOpt) > 0},
		{"devices", len(h.Devices) > 0},
		{"pid_mode", h.PidMode != "" && h.PidMode != "private"},
		{"ipc_mode", h.IpcMode != "" && h.IpcMode != "private" && h.IpcMode != "shareable"}, // daemon defaults recreation reproduces
		{"user", in.Config.User != ""},
		{"runtime", h.Runtime != "" && h.Runtime != defaultRuntime},
		{"resource_limits", h.Memory > 0 || h.MemorySwap > 0 || h.MemoryReservation > 0 || h.NanoCpus > 0 || h.CpuShares > 0 || h.CpuQuota > 0 || h.CpusetCpus != "" || (h.PidsLimit != nil && *h.PidsLimit != 0)},
		{"ulimits", len(h.Ulimits) > 0},
		{"sysctls", len(h.Sysctls) > 0},
		{"device_requests", len(h.DeviceRequests) > 0},
		{"init", h.Init != nil && *h.Init},
		{"userns_mode", h.UsernsMode != ""},
		{"cgroup_parent", h.CgroupParent != ""},
		{"group_add", len(h.GroupAdd) > 0},
		{"extra_hosts", len(h.ExtraHosts) > 0},
		{"dns", len(h.Dns) > 0 || len(h.DnsOptions) > 0 || len(h.DnsSearch) > 0},
		{"links", len(h.Links) > 0},
		{"network", (h.NetworkMode != "default" && h.NetworkMode != "bridge" && h.NetworkMode != projectNetwork) || len(n.Networks) != 1 || !onNetwork},
	}
	codes := []string{}
	for _, c := range checks {
		if c.found {
			codes = append(codes, c.code)
		}
	}
	return codes
}

// unsupported is the precondition's detail for configuration a recreate would drop.
func unsupported(codes []string) string {
	return "unsupported: " + strings.Join(codes, ", ")
}
```

In `prepare`'s precondition closure replace the `undescribed` call with:

```go
		if !reported(before) {
			return protocol.OutcomeDenied, "the runtime did not report the container's full configuration"
		}
		if codes := undescribed(before, r.req.Project+"_default", r.defaultRuntime); len(codes) > 0 {
			return protocol.OutcomeDenied, unsupported(codes)
		}
```

and the image comparison with:

```go
		if before.Config.differs(im.Config) {
			return protocol.OutcomeDenied, unsupported([]string{"image_config"})
		}
```

`step` already bounds the detail with `bound(detail, protocol.MaxDeploymentStepDetailBytes)`, cutting on a rune boundary.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -count=1 ./internal/runtime/docker/`
Expected: PASS.

- [ ] **Step 5: DOX and commit**

`internal/runtime/AGENTS.md`, `Client.Deploy` bullet: after "The precondition decodes with pointers and is `denied`" state that the configuration refusals are reported as codes of `protocol.UnsupportedCodes` (`undescribed` returns every one that applies, in vocabulary order; `image_config` for a differing `Cmd`/`Entrypoint`/`Healthcheck`/`WorkingDir`/`StopSignal`) with detail `unsupported: <codes joined by ", ">`, bounded to 256 bytes; an absent pointer-decoded field is still `the runtime did not report the container's full configuration` (`reported`).

```bash
gofmt -l internal cmd
git add internal/runtime
git commit -m "feat(runtime): report unsupported configuration as codes" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Runtime — every inspection carries a verdict

**Files:**
- Modify: `internal/runtime/docker/docker.go:25-30` (`Client` gains the runtime cache)
- Modify: `internal/runtime/docker/inspection.go` (`InspectContainer`, `inspectionGet`, new `inspectionRuntime`)
- Test: `internal/runtime/docker/inspection_test.go`, `internal/runtime/docker/inspection_integration_test.go`
- Docs: `internal/runtime/AGENTS.md`

**Interfaces:**
- Consumes: `reported`, `undescribed`, `imageDefaults.differs` (Task 2); `protocol.ContainerInspection.Unsupported` (Task 1).
- Produces:
  ```go
  func (c *Client) inspectionRuntime(ctx context.Context) (string, error) // GET /info, cached one minute
  func (c *Client) inspectionGet(ctx context.Context, path string, outs ...any) error
  // InspectContainer now sets Unsupported (never nil) and ConfigurationVerified = len(Unsupported) == 0
  ```

- [ ] **Step 1: Write the failing tests**

In `internal/runtime/docker/inspection_test.go` (add `"sync/atomic"` to its imports), route `/info` for every existing test by replacing `fakeInspection`:

```go
func fakeInspection(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.41/info" {
			json.NewEncoder(w).Encode(map[string]string{"DefaultRuntime": "runc"})
			return
		}
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return NewHTTP(s.Client(), s.URL)
}
```

Add a fixture of a container a recreate fully expresses, beside `inspectionFixture`:

```go
// expressibleFixture is inspectionFixture on its Compose project network, with a writable root
// and the image's own command, so nothing a recreate would drop remains.
func expressibleFixture() (protocol.InspectionTarget, map[string]any, map[string]any) {
	target, container, image := inspectionFixture()
	container["Config"] = map[string]any{"Env": []string{"TOKEN=secret-canary"}, "Cmd": []string{"nginx"}, "Labels": map[string]string{"com.docker.compose.project": "shop", "token": "secret-canary"}}
	h := container["HostConfig"].(map[string]any)
	h["NetworkMode"], h["ReadonlyRootfs"] = "shop_default", false
	container["NetworkSettings"].(map[string]any)["Networks"] = map[string]any{"shop_default": map[string]string{"EndpointID": "secret-canary"}}
	image["Config"] = map[string]any{"Env": []string{"TOKEN=secret-canary"}, "Cmd": []string{"nginx"}}
	return target, container, image
}

func serve(container, image map[string]any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/images/") {
			json.NewEncoder(w).Encode(image)
		} else {
			json.NewEncoder(w).Encode(container)
		}
	}
}
```

Append:

```go
// The verdict is codes only: the canary fixture has a read-only root, a network off the project
// network (no Compose label) and a command its image does not set; no value leaks.
func TestInspectionReportsWhatARecreateWouldDrop(t *testing.T) {
	target, container, image := inspectionFixture()
	out, err := fakeInspection(t, serve(container, image)).InspectContainer(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if out.ConfigurationVerified || fmt.Sprint(out.Unsupported) != "[read_only_rootfs network image_config]" || out.Validate(target, time.Now()) != nil {
		t.Fatalf("verdict: %v %v", out.ConfigurationVerified, out.Unsupported)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret-canary") {
		t.Fatal("inspection leaked configuration")
	}
	target, container, image = expressibleFixture()
	out, err = fakeInspection(t, serve(container, image)).InspectContainer(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !out.ConfigurationVerified || out.Unsupported == nil || len(out.Unsupported) != 0 || out.Validate(target, time.Now()) != nil {
		t.Fatalf("expressible: %v %v", out.ConfigurationVerified, out.Unsupported)
	}
	raw, _ = json.Marshal(out)
	if strings.Contains(string(raw), "secret-canary") || strings.Contains(string(raw), "shop") {
		t.Fatal("inspection leaked a label or network name")
	}
}

// The daemon's default runtime is read once a minute per client, and a container on it is fine.
func TestInspectionCachesTheDefaultRuntime(t *testing.T) {
	target, container, image := expressibleFixture()
	container["HostConfig"].(map[string]any)["Runtime"] = "nvidia"
	var infos atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.41/info" {
			infos.Add(1)
			json.NewEncoder(w).Encode(map[string]string{"DefaultRuntime": "nvidia"})
			return
		}
		serve(container, image)(w, r)
	}))
	t.Cleanup(s.Close)
	c := NewHTTP(s.Client(), s.URL)
	for range 2 {
		out, err := c.InspectContainer(context.Background(), target)
		if err != nil || !out.ConfigurationVerified {
			t.Fatalf("%+v %v", out, err)
		}
	}
	if n := infos.Load(); n != 1 {
		t.Fatalf("GET /info %d times", n)
	}
}

// An unreadable default runtime makes the inspection unavailable rather than reporting every
// container's runtime as unsupported.
func TestInspectionRefusesAnUnreadableRuntime(t *testing.T) {
	target, container, image := expressibleFixture()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.41/info" {
			w.WriteHeader(500)
			return
		}
		serve(container, image)(w, r)
	}))
	t.Cleanup(s.Close)
	if out, err := NewHTTP(s.Client(), s.URL).InspectContainer(context.Background(), target); out != nil || !errors.Is(err, ErrInspectionUnavailable) {
		t.Fatalf("got %v %v", out, err)
	}
}
```

In `TestInspectionRefusesChangesAndInvalidFacts`, add `"config"` to the `changed during read` field list and its case in the switch:

```go
					case "config":
						container["HostConfig"].(map[string]any)["Memory"] = 1 << 30
```

In `internal/runtime/docker/inspection_integration_test.go`, after the existing fact check, add:

```go
	for _, code := range []string{"tmpfs", "read_only_rootfs", "capabilities", "security_opt", "network"} {
		if !slices.Contains(out.Unsupported, code) {
			t.Fatalf("unsupported %v lacks %s", out.Unsupported, code)
		}
	}
	if out.Validate(target, time.Now()) != nil {
		t.Fatalf("real inspection invalid: %+v", out)
	}
```

(add `"slices"` to its imports).

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/runtime/docker/ -run Inspection`
Expected: FAIL: `verdict: false []` (no codes yet); the cache test counts 0 `/info`; the unreadable-runtime test gets a result.

- [ ] **Step 3: Implement**

`internal/runtime/docker/docker.go`, `Client`:

```go
type Client struct {
	http    *http.Client
	base    string
	cpuMu   sync.Mutex
	cpuPrev map[string]cpuPoint
	// The daemon's default runtime for inspections, read at most once a minute.
	runtimeMu   sync.Mutex
	runtimeName string
	runtimeRead time.Time
}
```

`internal/runtime/docker/inspection.go`: `inspectionGet` decodes one body into several shapes:

```go
// A separate small read budget avoids changing the fleet snapshot contract.
// Never wrap daemon, decode or transport errors: they can contain configuration.
// The body is decoded into each of outs.
func (c *Client) inspectionGet(ctx context.Context, path string, outs ...any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return ErrInspectionUnavailable
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrInspectionUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrInspectionNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return ErrInspectionUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxInspectionBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrInspectionUnavailable
	}
	if len(body) > maxInspectionBody {
		return ErrInspectionInvalid
	}
	for _, out := range outs {
		if json.Unmarshal(body, out) != nil {
			return ErrInspectionInvalid
		}
	}
	return nil
}

// inspectionRuntime is the daemon's DefaultRuntime, what a container created without one gets,
// read at most once a minute per client.
func (c *Client) inspectionRuntime(ctx context.Context) (string, error) {
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	if c.runtimeName != "" && time.Since(c.runtimeRead) < time.Minute {
		return c.runtimeName, nil
	}
	var info struct{ DefaultRuntime string }
	if err := c.inspectionGet(ctx, "/info", &info); err != nil {
		return "", err
	}
	if info.DefaultRuntime == "" {
		return "", ErrInspectionUnavailable
	}
	c.runtimeName, c.runtimeRead = info.DefaultRuntime, time.Now()
	return c.runtimeName, nil
}
```

Replace `InspectContainer` with:

```go
// InspectContainer reads a bounded, redacted observation through Engine v1.41.
// It performs GETs only, follows the pinned image ID rather than a tag, and
// rechecks selected container fields after the image read. It also decodes the
// container into inspectedForDeploy and reports, as codes only, what a recreate
// from the definition would drop. Callers own scope, authorization/admission
// through the agent inspection transport.
func (c *Client) InspectContainer(parent context.Context, target protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, callBudget)
	defer cancel()
	daemonRuntime, err := c.inspectionRuntime(ctx)
	if err != nil {
		return nil, err
	}
	var before, after inspectedContainer
	var full, fullAfter inspectedForDeploy
	var labels struct {
		Config *struct{ Labels map[string]string }
	}
	path := "/containers/" + target.ContainerID + "/json"
	if err := c.inspectionGet(ctx, path, &before, &full, &labels); err != nil {
		return nil, err
	}
	if before.ID != target.ContainerID || before.Image != target.ImageID || before.Created.Unix() != target.CreatedUnix {
		return nil, ErrInspectionChanged
	}
	out, err := inspectionFacts(before)
	if err != nil {
		return nil, err
	}
	if !reported(full) {
		return nil, ErrInspectionInvalid
	}
	var im struct {
		ID                    string `json:"Id"`
		OS                    string `json:"Os"`
		Architecture, Variant string
		Config                imageDefaults
	}
	if err = c.inspectionGet(ctx, "/images/"+target.ImageID+"/json", &im); err != nil {
		return nil, err
	}
	if im.ID != target.ImageID {
		return nil, ErrInspectionChanged
	}
	if !platformPart.MatchString(im.OS) || !platformPart.MatchString(im.Architecture) || (im.Variant != "" && !platformPart.MatchString(im.Variant)) {
		return nil, ErrInspectionInvalid
	}
	if err = c.inspectionGet(ctx, path, &after, &fullAfter); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(before, after) || !reported(fullAfter) || !sameConfiguration(full, fullAfter) {
		return nil, ErrInspectionChanged
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// No Compose label: "" matches no network, so network is reported unless the container is on bridge only.
	projectNetwork := ""
	if labels.Config != nil && labels.Config.Labels["com.docker.compose.project"] != "" {
		projectNetwork = labels.Config.Labels["com.docker.compose.project"] + "_default"
	}
	out.Unsupported = undescribed(full, projectNetwork, daemonRuntime)
	if full.Config.differs(im.Config) {
		out.Unsupported = append(out.Unsupported, "image_config")
	}
	out.ConfigurationVerified = len(out.Unsupported) == 0
	out.Target = target
	out.ImagePlatform = protocol.ImagePlatform{OS: im.OS, Architecture: im.Architecture, Variant: im.Variant}
	out.ObservedAt = time.Now().UTC()
	return out, nil
}
```

`sameConfiguration` is also what Task 4's recheck compares; define it here in `deploy.go`, below `reported`:

```go
// sameConfiguration compares what recreation depends on: Config, HostConfig and Mounts. State
// and NetworkSettings change on their own (a restart) and are not compared.
func sameConfiguration(a, b inspectedForDeploy) bool {
	return reflect.DeepEqual(a.Config, b.Config) && reflect.DeepEqual(a.HostConfig, b.HostConfig) && reflect.DeepEqual(a.Mounts, b.Mounts)
}
```

`TestInspectionRedactsAndPins` records calls inside its handler, which `fakeInspection` now reaches only after `/info`, so its expected call list is unchanged. `TestInspectionCapsLongDeadlinesAndRedactsTransportErrors` now fails at the `/info` call with `ErrInspectionUnavailable` under the same `callBudget` context, as it expects.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -count=1 ./internal/runtime/docker/ && go test -race -count=1 ./internal/agent/client/ -run Inspection`
Expected: PASS. With Docker available also `KY_TEST_DOCKER_INSPECTION_IMAGE=alpine:3.24 go test -race -count=1 ./internal/runtime/docker -run TestInspectionRealDocker` PASS.

- [ ] **Step 5: DOX and commit**

`internal/runtime/AGENTS.md`, `InspectContainer` bullet: replace "ConfigurationVerified remains false: the authorized agent/API inspection path returns observations, never a recreation spec or deployment grant." with "It reads the daemon's `DefaultRuntime` from `/info` first (cached per client for one minute; unreadable is `ErrInspectionUnavailable`), decodes the same container bodies into `inspectedForDeploy` and the `com.docker.compose.project` label, and reports `Unsupported`: `undescribed` with project network `<project>_default` (no label: `\"\"`, so `network` unless on `bridge` only) plus `image_config` when the container's command, entrypoint, healthcheck, working directory or stop signal differ from the image's; `ConfigurationVerified` is true exactly when it is empty. A change in `Config`, `HostConfig` or `Mounts` between the two container reads is `ErrInspectionChanged`. Codes only: no value, path, label or name leaves. The path returns observations, never a recreation spec or deployment grant."

```bash
gofmt -l internal cmd
git add internal/runtime
git commit -m "feat(runtime): inspections report what a recreate would drop" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Runtime — `recheck` at the start of each replacement

**Files:**
- Modify: `internal/runtime/docker/deploy.go` (`replaceBudget`, `prepared.before`, `prepare`'s return, `replace`)
- Test: `internal/runtime/docker/deploy_test.go` (fake engine knob, new tests, index updates), `internal/runtime/docker/deploy_pull_test.go`, `internal/runtime/docker/deploy_volume_test.go`
- Docs: `internal/runtime/AGENTS.md`

**Interfaces:**
- Consumes: `sameConfiguration` (Task 3), `protocol.StepRecheck` (Task 1).
- Produces: step `recheck` before `rename` for every service; the deadline guard `not enough time left before the deadline to replace this service safely` now runs at `recheck` (the first phase-two step), so Task 5 can call `started` after it and before any read or mutation of phase two. `replaceBudget = 2*operationBudget + 4*callBudget`.

- [ ] **Step 1: Give the fake engine a drift knob**

In `internal/runtime/docker/deploy_test.go` add to `fakeDeployEngine`:

```go
	reads            map[string]int                                 // GETs of each old container so far
	drift            func(id string, read int, body map[string]any) // edits a copy of what the read-th GET (from 1) of an old container answers
```

initialise `reads: map[string]int{}` in `newFakeDeployEngine`, and replace the two old-container cases of the handler:

```go
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+oldID+"/json"):
			body := f.oldRead(oldID, f.oldContainer)
			w.WriteHeader(f.oldStatus)
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+otherOldID+"/json"):
			other := cloneJSON(f.oldContainer)
			other["Id"], other["Name"] = otherOldID, "/shop-db-1"
			if f.otherMounts != nil {
				other["Mounts"] = f.otherMounts
			}
			_ = json.NewEncoder(w).Encode(f.oldRead(otherOldID, other))
```

with, below `client()`:

```go
// oldRead counts a GET of an old container and lets drift edit a copy of its body. drift runs on
// the handler goroutine before the status is written, so it may also set oldStatus.
func (f *fakeDeployEngine) oldRead(id string, body map[string]any) map[string]any {
	f.mu.Lock()
	f.reads[id]++
	n, drift := f.reads[id], f.drift
	f.mu.Unlock()
	body = cloneJSON(body)
	if drift != nil {
		drift(id, n, body)
	}
	return body
}

func cloneJSON(m map[string]any) map[string]any {
	raw, _ := json.Marshal(m)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// dbService is a second service on otherOldID, so both services can succeed against the fake.
func dbService() protocol.DeploymentService {
	db := webService()
	db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", otherOldID, nil
	return db
}
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/runtime/docker/deploy_test.go`:

```go
// The recheck re-reads each old container right before its rename. A changed identity or a
// changed Config, HostConfig or Mounts is denied there: that service is untouched, later
// services are skipped, and a service already replaced stays replaced.
func TestRecheckDeniesDriftBetweenThePhases(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"identity":    func(b map[string]any) { b["Created"] = "2023-11-14T22:13:21Z" },
		"image":       func(b map[string]any) { b["Image"] = newImage },
		"host config": func(b map[string]any) { b["HostConfig"].(map[string]any)["Memory"] = 1 << 30 },
		"config":      func(b map[string]any) { b["Config"].(map[string]any)["User"] = "1000" },
		"mounts": func(b map[string]any) {
			b["Mounts"] = []any{map[string]any{"Type": "volume", "Name": "shop_data", "Destination": "/data", "RW": true}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			f.drift = func(id string, read int, body map[string]any) {
				if id == otherOldID && read > 1 {
					change(body)
				}
			}
			res := f.client().Deploy(context.Background(), request(webService(), dbService()))
			if res.Outcome != protocol.OutcomeDenied || res.Detail != "service db, step recheck: the container changed after the precondition" {
				t.Fatalf("outcome: %+v", res)
			}
			got := []string{}
			for _, s := range res.Steps {
				if s.Step != protocol.StepPrecondition && s.Step != protocol.StepImage {
					got = append(got, s.Service+" "+s.Step+" "+s.Outcome)
				}
			}
			want := "web recheck succeeded,web rename succeeded,web create succeeded,web stop succeeded,web start succeeded,web remove succeeded," +
				"db recheck denied,db rename skipped,db create skipped,db stop skipped,db start skipped,db remove skipped"
			if strings.Join(got, ",") != want {
				t.Fatalf("steps:\n got %v\nwant %v", got, want)
			}
			if len(res.Services) != 1 || res.Services[0].Service != "web" {
				t.Fatalf("the replaced service must keep its identity: %+v", res.Services)
			}
			for _, c := range f.calls {
				if c.Method != "GET" && strings.Contains(c.Path, otherOldID) {
					t.Fatalf("db was touched: %v", f.steps())
				}
			}
			if err := res.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A container removed between the phases is denied at recheck with nothing touched.
func TestRecheckDeniesAContainerGoneBetweenThePhases(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.drift = func(id string, read int, _ map[string]any) {
		if id == oldID && read > 1 {
			f.oldStatus = 404
		}
	}
	res := f.client().Deploy(context.Background(), request(webService()))
	if res.Outcome != protocol.OutcomeDenied || res.Steps[2].Step != protocol.StepRecheck || res.Steps[2].Detail != "the container no longer exists" {
		t.Fatalf("gone at recheck: %+v", res)
	}
	for _, c := range f.calls {
		if c.Method != "GET" {
			t.Fatalf("mutating call: %v", f.steps())
		}
	}
}

// A restart between the phases changes State and NetworkSettings only: that is not drift.
func TestRecheckIgnoresStateAndNetworkSettings(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.drift = func(_ string, read int, body map[string]any) {
		if read > 1 {
			body["State"] = map[string]any{"Status": "running", "StartedAt": "2026-09-24T12:00:00Z", "RestartCount": 3}
			body["NetworkSettings"] = map[string]any{"Networks": map[string]any{"bridge": map[string]any{"IPAddress": "172.17.0.9"}}}
		}
	}
	if res := f.client().Deploy(context.Background(), request(webService())); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("a restart denied the replacement: %+v", res)
	}
}
```

Update the existing assertions for the extra read and step:

- `TestDeployReplacesOneServiceInOrder`: `want` becomes

```go
	want := []string{"GET /info", "GET /containers/" + oldID + "/json", "GET /images/" + oldImage + "/json", "GET /images/" + newImage + "/json", "GET /containers/" + oldID + "/json", "POST /containers/" + oldID + "/rename", "POST /containers/create", "POST /containers/" + oldID + "/stop", "POST /containers/" + newID + "/start", "GET /containers/" + newID + "/json", "DELETE /containers/" + oldID}
```

  the query check becomes `if f.calls[5].Query != "name=shop-web-1.kyyard-prev-3f2b1c9e" || f.calls[6].Query != "name=shop-web-1" || f.calls[7].Query != "t=10" || f.calls[10].Query != "" {`, the body decode reads `f.calls[6].Body`, and `len(res.Steps) != 7` becomes `len(res.Steps) != 8`.
- `TestDeployKeepsTheProjectNetwork`: decode `f.calls[6].Body`.
- `TestDeployPreconditionsRefuseBeforeTouchingAnything`: `len(res.Steps) != 14` becomes `len(res.Steps) != 16`.
- `TestDeployStepFailuresStopTheRun`: call counts `"rename conflict"` 6, `"create conflict"` 7, `"stop refused"` 8, `"start fails"` 9, `"remove fails"` 11 (`"image missing"` stays 4).
- `TestDeployTimeAndCancellation`: both `res.Steps[4]` become `res.Steps[5]`.
- `TestDeployRefusesToStartWithoutTimeToFinish`: the guard is now the recheck:

```go
	if res.Outcome != protocol.OutcomeTimedOut || res.Steps[2].Step != protocol.StepRecheck || res.Steps[2].Outcome != protocol.OutcomeTimedOut || res.Steps[2].Detail != "not enough time left before the deadline to replace this service safely" {
```

  (its `want` call list is unchanged: the guard runs before the read).
- `TestDeployIdentityReadFailureNamesTheContainer`: `res.Steps[5]` becomes `res.Steps[6]` (both uses).
- `TestDeployTwoServicesSecondRefusedTouchesNothing`: `want` becomes

```go
	want := "web precondition succeeded,web image succeeded,db precondition denied,db image skipped," +
		"web recheck skipped,web rename skipped,web create skipped,web stop skipped,web start skipped,web remove skipped," +
		"db recheck skipped,db rename skipped,db create skipped,db stop skipped,db start skipped,db remove skipped"
```

`internal/runtime/docker/deploy_pull_test.go`, `TestDeployPullsThePinnedDigest`: insert `"GET /containers/" + oldID + "/json"` into `want` after `"POST /images/" + newImage + "/tag"`, decode `f.calls[8].Body` for the create body, and expect steps `"precondition,pull,recheck,rename,create,stop,start,remove"`.

`internal/runtime/docker/deploy_volume_test.go`: the two step-order strings (lines 54-56 and 138-140) gain `web recheck succeeded,` / `db recheck succeeded,` (and `skipped` in the failure case) immediately before each service's `rename`:

```go
	want := "web precondition succeeded,web volume succeeded,web volume succeeded,web image succeeded,db precondition succeeded,db image succeeded," +
		"web recheck succeeded,web rename succeeded,web create succeeded,web stop succeeded,web start succeeded,web remove succeeded," +
		"db recheck succeeded,db rename succeeded,db create succeeded,db stop succeeded,db start succeeded,db remove succeeded"
```

```go
		want := "web precondition succeeded,web volume failed,web volume skipped,web image skipped,db precondition skipped,db image skipped," +
			"web recheck skipped,web rename skipped,web create skipped,web stop skipped,web start skipped,web remove skipped," +
			"db recheck skipped,db rename skipped,db create skipped,db stop skipped,db start skipped,db remove skipped"
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test -count=1 ./internal/runtime/docker/ -run 'Recheck|Deploy'`
Expected: FAIL: no `recheck` step in any result; `TestRecheckDeniesDriftBetweenThePhases` sees db replaced.

- [ ] **Step 4: Implement**

In `internal/runtime/docker/deploy.go`:

```go
// replaceBudget is the time one service's phase-two steps may need: stop and start at
// operationBudget, recheck, rename, create and the identity read at callBudget.
const replaceBudget = 2*operationBudget + 4*callBudget
```

`prepared` keeps phase one's inspection:

```go
// prepared is what a service's precondition and image (or pull) steps settled for its replacement.
type prepared struct {
	s           protocol.DeploymentService // ImageID is the pulled ID for a pulled service
	name        string                     // the old container's name, without the leading slash
	networkMode string
	before      inspectedForDeploy // the precondition's read; recheck compares against it
}
```

`prepare` returns `prepared{s: s, name: strings.TrimPrefix(before.Name, "/"), networkMode: networkMode, before: before}`.

In `replace`, insert the recheck as the first step and drop the deadline guard from the rename:

```go
func (r *deployRun) replace(ctx context.Context, p prepared) {
	s, old := p.s, url.PathEscape(p.s.Replaces.ContainerID)
	// The pull window can be minutes: re-read the container right before touching it.
	r.step(s.Name, protocol.StepRecheck, func() (string, string) {
		if time.Until(r.req.Deadline) < replaceBudget {
			return protocol.OutcomeTimedOut, "not enough time left before the deadline to replace this service safely"
		}
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var now inspectedForDeploy
		if err := r.c.get(cctx, "/containers/"+old+"/json", &now); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return protocol.OutcomeDenied, "the container no longer exists"
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		if now.ID != s.Replaces.ContainerID || now.Image != s.Replaces.ImageID || now.Created.Unix() != s.Replaces.CreatedUnix || !sameConfiguration(p.before, now) {
			return protocol.OutcomeDenied, "the container changed after the precondition"
		}
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepRename, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		name := p.name + ".kyyard-prev-" + r.req.Deployment[:8]
		status, err := r.c.post(cctx, "/containers/"+old+"/rename?name="+url.QueryEscape(name))
		if err != nil || status >= 400 {
			if status == http.StatusConflict {
				return protocol.OutcomeFailed, "a container already holds the name reserved for the previous one"
			}
			return r.outcomeFor(cctx, err, status)
		}
		return protocol.OutcomeSucceeded, ""
	})
```

(the create, stop, start and remove steps are unchanged). Update the `Deploy` doc comment's phase-two list to "then per service recheck, rename, create, stop, start, remove".

- [ ] **Step 5: Run to verify they pass**

Run: `go test -race -count=1 ./internal/runtime/docker/`
Expected: PASS.

- [ ] **Step 6: DOX and commit**

`internal/runtime/AGENTS.md`, `Client.Deploy` bullet: phase two per service is now "recheck (a fresh `GET /containers/{id}/json`: `denied` `the container changed after the precondition` when `Id`, `Image` or `Created` second differ from `Replaces` or `Config`, `HostConfig` or `Mounts` differ by `reflect.DeepEqual` from the precondition's read kept on `prepared.before`; `State` and `NetworkSettings` are not compared; 404 is `denied` `the container no longer exists`), rename, …"; replace "Less than `2*operationBudget + 3*callBudget` before the deadline is `timed_out` at rename with no mutating call" with "Less than `replaceBudget` (`2*operationBudget + 4*callBudget`) before the deadline is `timed_out` at recheck with no call"; the pull-phase formula now subtracts that larger budget.

```bash
gofmt -l internal cmd
git add internal/runtime
git commit -m "feat(runtime): recheck each container before its replacement" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Started marker — runtime hook, durable ledger entry, real-Docker recheck

**Files:**
- Modify: `internal/runtime/docker/deploy.go` (`Client.Deploy` signature, `deployRun.started`/`begun`/`begin`, `begin` in the recheck step), `internal/runtime/docker/remove.go` (`Client.Remove` signature, `begin` in the stop step)
- Modify: `internal/agent/client/connect.go:61-68` (`Options.Deploy`/`Options.Remove`), `internal/agent/client/deployments.go` (entry, load, `begin`, `save`, `prune`, `resend`, exec closures)
- Modify: `cmd/agent/main.go:61-62` (variable types); `internal/api/local_docker.go:73` and `internal/api/deployment_integration_test.go:97` compile unchanged (method values)
- Test: every `Deploy(`/`Remove(` call in `internal/runtime/docker/*_test.go`, every `Deploy`/`Remove` literal in `internal/agent/client/deployments_test.go`; new tests in `deploy_test.go`, `remove_test.go`, `deployments_test.go`; new `TestRecheckRealDocker` in `internal/runtime/docker/deploy_integration_test.go`
- Modify: `.github/workflows/ci.yml:100` (run the new real-Docker test)
- Docs: `internal/runtime/AGENTS.md`, `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: the `recheck` step and its deadline guard (Task 4).
- Produces:
  ```go
  func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult
  func (c *Client) Remove(parent context.Context, req protocol.RemovalRequest, started func()) protocol.DeploymentResult
  // client.Options
  Deploy func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult
  Remove func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult
  // client, unexported
  const restartedDetail = "the agent restarted after replacement began; inspect the host"
  func (e deploymentEntry) pending() bool
  func (d *deployer) begin(id string)
  func (d *deployer) save() error
  ```
  `started` is never nil: every caller passes a function.

- [ ] **Step 1: Move every call site to the three-parameter form**

These two commands are the whole mechanical change; run them from the worktree root and read the diff before going on:

```bash
perl -0pi -e 's/\.(Deploy|Remove)(\((?:[^()]++|(?2))*\))/my ($m, $args) = ($1, substr($2, 0, -1)); $args =~ m{, func\(\) \{\}\z} ? ".$m$args)" : ".$m$args, func() {})"/ge' internal/runtime/docker/*_test.go
perl -pi -e 's/(\w+ context\.Context, \w+ protocol\.(?:DeploymentRequest|RemovalRequest))\) protocol\.DeploymentResult/$1, _ func()) protocol.DeploymentResult/g; s/(context\.Context, protocol\.(?:DeploymentRequest|RemovalRequest))\) protocol\.DeploymentResult/$1, func()) protocol.DeploymentResult/g' internal/agent/client/deployments_test.go internal/agent/client/connect.go cmd/agent/main.go
git diff --stat
```

Expected: 54 runtime test calls gain `, func() {}` (48 existing, 6 added by Tasks 1, 2 and 4); 17 function types in `deployments_test.go`, 2 in `connect.go` and 2 in `cmd/agent/main.go` gain the `func()` parameter; `func removed(req protocol.RemovalRequest)` (no context parameter) is untouched. Both commands are idempotent: a second run changes nothing.

- [ ] **Step 2: Write the failing tests**

Append to `internal/runtime/docker/deploy_test.go`:

```go
// started is called once, after phase one and before the first phase-two call, and never when
// phase one fails or the deadline guard refuses.
func TestDeployCallsStartedOnceBeforePhaseTwo(t *testing.T) {
	f := newFakeDeployEngine(t)
	marks := []int{}
	res := f.client().Deploy(context.Background(), request(webService(), dbService()), func() {
		f.mu.Lock()
		marks = append(marks, len(f.calls))
		f.mu.Unlock()
	})
	if res.Outcome != protocol.OutcomeSucceeded || len(marks) != 1 {
		t.Fatalf("started %d times: %+v", len(marks), res)
	}
	f.mu.Lock()
	before, next := f.calls[:marks[0]], f.calls[marks[0]]
	f.mu.Unlock()
	for _, c := range before {
		if c.Method != "GET" {
			t.Fatalf("mutation before started: %+v", c)
		}
	}
	if next.Method != "GET" || !strings.HasSuffix(next.Path, "/containers/"+oldID+"/json") {
		t.Fatalf("the first call after started is not web's recheck: %+v", next)
	}
	for name, mutate := range map[string]func(*fakeDeployEngine, *protocol.DeploymentRequest){
		"precondition denied": func(f *fakeDeployEngine, _ *protocol.DeploymentRequest) {
			f.oldContainer["HostConfig"].(map[string]any)["Privileged"] = true
		},
		"image missing":  func(f *fakeDeployEngine, _ *protocol.DeploymentRequest) { f.imageStatus = 404 },
		"deadline guard": func(_ *fakeDeployEngine, r *protocol.DeploymentRequest) { r.Deadline = time.Now().Add(60 * time.Second) },
	} {
		f := newFakeDeployEngine(t)
		req := request(webService())
		mutate(f, &req)
		called := false
		if res := f.client().Deploy(context.Background(), req, func() { called = true }); res.Outcome == protocol.OutcomeSucceeded || called {
			t.Fatalf("%s: started=%v %+v", name, called, res)
		}
	}
}
```

Append to `internal/runtime/docker/remove_test.go`:

```go
// started is called once, before the first stop; a removal of containers already gone never
// changes the host and never calls it.
func TestRemoveCallsStartedBeforeTheFirstStop(t *testing.T) {
	f := newFakeRemoveEngine(t)
	marks := []int{}
	res := f.client().Remove(context.Background(), removal(oldID, secondID), func() {
		f.mu.Lock()
		marks = append(marks, len(f.calls))
		f.mu.Unlock()
	})
	if res.Outcome != protocol.OutcomeSucceeded || len(marks) != 1 || marks[0] != 1 {
		t.Fatalf("started at %v: %+v", marks, res)
	}
	f = newFakeRemoveEngine(t)
	f.inspect[oldID] = 404
	called := false
	if res := f.client().Remove(context.Background(), removal(oldID), func() { called = true }); res.Outcome != protocol.OutcomeSucceeded || called {
		t.Fatalf("started=%v for a container already gone: %+v", called, res)
	}
}
```

Append to `internal/agent/client/deployments_test.go` (add `"fmt"` and `"sync/atomic"` to its imports):

```go
// A run that began changing the host and never recorded a result (the agent restarted) is
// reported unknown after the restart, re-sent, and answered from the ledger, never run again.
func TestDeployerStartedMarkerSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	marked := make(chan struct{})
	release := make(chan struct{})
	first := newDeployer(context.Background(), dir, &Options{Deploy: func(_ context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult {
		started()
		close(marked)
		<-release
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}})
	req := testRequest("ep_1")
	raw, _ := json.Marshal(req)
	first.handleApply(context.Background(), "ep_1", raw, make(chan outFrame, 1))
	<-marked
	stored, err := os.ReadFile(filepath.Join(dir, "deployments.json"))
	if err != nil || !strings.Contains(string(stored), req.Deployment) || !strings.Contains(string(stored), `"started"`) || strings.Contains(string(stored), "agent-secret-canary") {
		t.Fatalf("started entry: %v %s", err, stored)
	}
	// The agent restarts here: a new deployer reads the ledger the first one left.
	var ran atomic.Bool
	second := newDeployer(context.Background(), dir, &Options{Deploy: func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		ran.Store(true)
		return protocol.DeploymentResult{}
	}})
	out := make(chan outFrame, 4)
	second.resend(context.Background(), out)
	var res protocol.DeploymentResult
	if decodeResult(<-out, &res) != nil || res.Deployment != req.Deployment || res.Outcome != protocol.OutcomeUnknown || res.Detail != "the agent restarted after replacement began; inspect the host" || res.Validate() != nil {
		t.Fatalf("re-sent: %+v", res)
	}
	second.handleApply(context.Background(), "ep_1", raw, out)
	if decodeResult(<-out, &res) != nil || res.Outcome != protocol.OutcomeUnknown {
		t.Fatalf("replayed: %+v", res)
	}
	if ran.Load() {
		t.Fatal("a re-sent frame ran again after a restart")
	}
	close(release)
	first.wait()
}

// Pruning never drops a run that began and has no result, however old or full the ledger, and
// re-sending skips it: it has nothing to send yet.
func TestDeployerPruneKeepsAStartedRun(t *testing.T) {
	d := newDeployer(context.Background(), t.TempDir(), &Options{})
	for i := range deploymentLedgerMax + 5 {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		d.done[id] = deploymentEntry{Result: protocol.DeploymentResult{Deployment: id, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: time.Now().UTC()}
	}
	running := "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b"
	d.begin(running)
	e := d.done[running]
	e.Started = time.Now().UTC().Add(-2 * deploymentLedgerLife)
	d.done[running] = e
	d.prune()
	if e, ok := d.done[running]; !ok || !e.pending() {
		t.Fatal("pruning dropped a started run")
	}
	if len(d.done) != deploymentLedgerMax+1 {
		t.Fatalf("ledger holds %d entries", len(d.done))
	}
	out := make(chan outFrame, 2*deploymentLedgerMax)
	d.resend(context.Background(), out)
	if len(out) != deploymentLedgerMax {
		t.Fatalf("re-sent %d results", len(out))
	}
	for range deploymentLedgerMax {
		var res protocol.DeploymentResult
		if decodeResult(<-out, &res) != nil || res.Deployment == running {
			t.Fatalf("re-sent the started run: %+v", res)
		}
	}
}
```

Append to `internal/runtime/docker/deploy_integration_test.go` (Global Constraints: the recheck's real-Docker proof):

```go
// Opt-in, already-present image only, like TestDeployRealDocker. The started hook runs at the
// last moment before phase two; a memory limit set there must be denied at recheck and the old
// container left running under its name.
func TestRecheckRealDocker(t *testing.T) {
	image := os.Getenv("KY_TEST_DOCKER_DEPLOY_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_DEPLOY_IMAGE to an existing shell image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	project := "kyyardrecheckfixture"
	name := project + "-web-1"
	network := project + "_default"
	fixtureImage := project + ":local"
	builder := project + "-build"
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		ids, _ := exec.CommandContext(cctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).Output()
		for _, id := range strings.Fields(string(ids)) {
			_ = exec.CommandContext(cctx, "docker", "rm", "-fv", id).Run()
		}
		_ = exec.CommandContext(cctx, "docker", "rm", "-fv", builder).Run()
		_ = exec.CommandContext(cctx, "docker", "rmi", "-f", fixtureImage).Run()
		_ = exec.CommandContext(cctx, "docker", "network", "rm", network).Run()
	}
	cleanup() // a prior aborted run may have left any of it behind
	t.Cleanup(cleanup)
	if out, err := docker("create", "--pull", "never", "--name", builder, image); err != nil {
		t.Fatalf("fixture image source: %v: %s", err, out)
	}
	if out, err := docker("commit", "--change", `CMD ["sleep","300"]`, builder, fixtureImage); err != nil {
		t.Fatalf("fixture image: %v: %s", err, out)
	}
	if out, err := docker("rm", "-fv", builder); err != nil {
		t.Fatalf("fixture image source removal: %v: %s", err, out)
	}
	if out, err := docker("network", "create", network); err != nil {
		t.Fatalf("fixture network: %v: %s", err, out)
	}
	oldID, err := docker("run", "-d", "--pull", "never", "--name", name, "--network", network, "--network-alias", "web",
		"--label", "com.docker.compose.project="+project, "--label", "com.docker.compose.service=web", fixtureImage)
	if err != nil {
		t.Fatalf("fixture: %v: %s", err, oldID)
	}
	raw, err := docker("inspect", "--format", "{{json .}}", oldID)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Image   string
		Created time.Time
	}
	if err = json.Unmarshal([]byte(raw), &identity); err != nil {
		t.Fatal(err)
	}
	req := protocol.DeploymentRequest{Deployment: "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c", Endpoint: "ep_1", Project: project, Revision: 2, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Services: []protocol.DeploymentService{{
		Name: "web", ContainerName: name, ImageID: identity.Image, Restart: "no", Mounts: []protocol.Mount{},
		Replaces: protocol.InspectionTarget{ContainerID: oldID, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()},
	}}}
	res := New("/var/run/docker.sock").Deploy(ctx, req, func() {
		if out, err := docker("update", "--memory", "64m", "--memory-swap", "128m", oldID); err != nil {
			t.Errorf("docker update: %v: %s", err, out)
		}
	})
	serialized, _ := json.Marshal(res)
	if res.Outcome != protocol.OutcomeDenied || len(res.Steps) < 3 || res.Steps[2].Step != protocol.StepRecheck || res.Steps[2].Detail != "the container changed after the precondition" {
		t.Fatalf("recheck: %s", serialized)
	}
	if out, err := docker("inspect", "--format", "{{.Id}} {{.State.Running}}", name); err != nil || out != oldID+" true" {
		t.Fatalf("the old container was touched: %v %s", err, out)
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go vet ./internal/runtime/docker/ ./internal/agent/client/ ./cmd/agent/`
Expected: FAIL to compile: `too many arguments in call to f.client().Deploy`, and in the client `cannot use func(...) (value of type func(context.Context, protocol.DeploymentRequest, func()) ...)` against the old `Options.Deploy`.

- [ ] **Step 4: Implement the runtime**

`internal/runtime/docker/deploy.go`: the signature becomes `func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult`, the run is built with `r := &deployRun{c: c, parent: parent, req: req, res: res, ensured: map[string]bool{}, keepOnly: map[string]bool{}, started: started}`, the doc comment gains "started is called once, immediately before the first phase-two call (after the first recheck's deadline guard); a run that ends in phase one never calls it.", and `deployRun` gains:

```go
	started        func() // called once before the run first reads or changes a container in phase two
	begun          bool
```

with

```go
// begin tells the caller, once, that the run is about to change the host.
func (r *deployRun) begin() {
	if !r.begun {
		r.begun = true
		r.started()
	}
}
```

In `replace`'s recheck step, call it between the deadline guard and the read:

```go
		if time.Until(r.req.Deadline) < replaceBudget {
			return protocol.OutcomeTimedOut, "not enough time left before the deadline to replace this service safely"
		}
		r.begin()
		cctx, cancel := context.WithTimeout(ctx, callBudget)
```

`internal/runtime/docker/remove.go`: `func (c *Client) Remove(parent context.Context, req protocol.RemovalRequest, started func()) protocol.DeploymentResult`, `r := &deployRun{c: c, parent: parent, res: res, started: started}`, and in `removeTarget`'s stop step:

```go
		if time.Until(deadline) < operationBudget+2*callBudget {
			return protocol.OutcomeTimedOut, "not enough time left before the deadline to remove this container safely"
		}
		r.begin()
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
```

- [ ] **Step 5: Implement the agent ledger**

`internal/agent/client/connect.go`: after the perl edit the fields read `Deploy func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult` and the same for `Remove`; extend the `Deploy` comment with: "The runtime calls the third argument once, immediately before its first phase-two mutation; it returns once the deployer has recorded durably that the host may change."

`internal/agent/client/deployments.go`:

```go
const (
	deploymentLedgerLife = 24 * time.Hour
	deploymentLedgerMax  = 20
	// restartedDetail settles a run that began changing the host and never recorded a result.
	restartedDetail = "the agent restarted after replacement began; inspect the host"
)

type deploymentEntry struct {
	Result   protocol.DeploymentResult `json:"result"`
	Finished time.Time                 `json:"finished"`
	// Started is set, with no result yet, while a run may be changing the host.
	Started time.Time `json:"started,omitempty"`
}

// pending is a run that began changing the host and has no result yet.
func (e deploymentEntry) pending() bool { return e.Result.Deployment == "" }
```

`newDeployer` settles runs a restart interrupted, then prunes:

```go
func newDeployer(root context.Context, dir string, opts *Options) *deployer {
	d := &deployer{root: root, opts: opts, path: filepath.Join(dir, "deployments.json"), done: map[string]deploymentEntry{}}
	if raw, err := os.ReadFile(d.path); err == nil {
		var saved map[string]deploymentEntry
		if json.Unmarshal(raw, &saved) == nil {
			d.done = saved
			if d.settleStarted() {
				if err := d.save(); err != nil && d.opts.Log != nil {
					d.opts.Log.Printf("deployment ledger: interrupted runs not recorded: %v", err)
				}
			}
			d.prune()
		}
	}
	return d
}

// settleStarted turns every run a restart interrupted after it began changing the host into an
// unknown result, re-sent like any other. It reports whether it found one.
func (d *deployer) settleStarted() bool {
	now, found := time.Now().UTC(), false
	for id, e := range d.done {
		if e.pending() {
			d.done[id] = deploymentEntry{Result: protocol.DeploymentResult{Deployment: id, Outcome: protocol.OutcomeUnknown, Detail: restartedDetail, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: now}
			found = true
		}
	}
	return found
}

// save writes the ledger durably. The caller holds mu, or owns d alone.
func (d *deployer) save() error {
	raw, err := json.Marshal(d.done)
	if err != nil {
		return err
	}
	return writeDurable(d.path, raw)
}

// begin records durably, before it returns, that run id is about to change the host.
func (d *deployer) begin(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done[id] = deploymentEntry{Started: time.Now().UTC()}
	if err := d.save(); err != nil && d.opts.Log != nil {
		// A restart before the result would then run the frame again; the recheck still guards it.
		d.opts.Log.Printf("deployment %s: start not recorded: %v", id, err)
	}
}
```

`prune` never drops a pending run:

```go
func (d *deployer) prune() {
	cutoff := time.Now().UTC().Add(-deploymentLedgerLife)
	ids := make([]string, 0, len(d.done))
	for id, e := range d.done {
		switch {
		case e.pending():
		case e.Finished.Before(cutoff):
			delete(d.done, id)
		default:
			ids = append(ids, id)
		}
	}
	if len(ids) <= deploymentLedgerMax {
		return
	}
	sort.Slice(ids, func(i, j int) bool { return d.done[ids[i]].Finished.Before(d.done[ids[j]].Finished) })
	for _, id := range ids[:len(ids)-deploymentLedgerMax] {
		delete(d.done, id)
	}
}
```

`finish` writes through `save`:

```go
func (d *deployer) finish(res protocol.DeploymentResult) *sessionLink {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done[res.Deployment] = deploymentEntry{Result: res, Finished: time.Now().UTC()}
	d.prune()
	if err := d.save(); err != nil && d.opts.Log != nil {
		// Still delivered and re-sent from memory; only a restart loses it.
		d.opts.Log.Printf("deployment %s: result not recorded: %v", res.Deployment, err)
	}
	d.running = ""
	return d.current
}
```

`resend` skips pending runs:

```go
	for _, e := range d.done {
		if !e.pending() {
			results = append(results, e.Result)
		}
	}
```

The exec closures pass the hook:

```go
			res := deploy(ctx, req, func() { d.begin(req.Deployment) })
```

```go
		exec = func(ctx context.Context) protocol.DeploymentResult {
			return remove(ctx, req, func() { d.begin(req.Deployment) })
		}
```

A pending entry exists only for the running ID, which `run` answers with silence before it looks in the ledger, so `replay` never reads one.

`.github/workflows/ci.yml:100`: `go test -race ./internal/runtime/docker -run '^Test(Exec|Inspection|Deploy|Recheck|Remove)RealDocker$' -count=1`.

- [ ] **Step 6: Run to verify they pass**

Run: `go build ./... && go test -race -count=1 ./internal/runtime/docker/ ./internal/agent/... ./cmd/... && go test -count=1 ./internal/api/`
Expected: PASS. With Docker available: `KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 go test -race -count=1 ./internal/runtime/docker -run '^TestRecheckRealDocker$'` PASS.

- [ ] **Step 7: DOX and commit**

`internal/runtime/AGENTS.md`: `Client.Deploy`/`Client.Remove` take `started func()`, called once immediately before the first phase-two call (Deploy: inside the first `recheck`, after its deadline guard; Remove: inside the first `stop`, after its guard); a run that ends in phase one, or removes only containers already gone, never calls it. Add to Verification: "`KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 go test -race ./internal/runtime/docker -run TestRecheckRealDocker` runs a fixture container and, from the `started` hook, `docker update --memory 64m --memory-swap 128m` on it; the deploy must be `denied` at `recheck` and the old container still running under its name (fixture project `kyyardrecheckfixture`, cleaned by label, image tag and network name before and after)."

`internal/agent/AGENTS.md`, `deployer` bullet: "The runtime's `started` hook makes the deployer write `{started}` for the ID durably before the first phase-two mutation. On load, an entry with `started` and no result becomes `unknown` `the agent restarted after replacement began; inspect the host` and is re-sent; a re-sent frame for it replays that result. Pruning never drops such an entry and re-sending skips it while its run is live."

```bash
gofmt -l internal cmd
git add internal/runtime internal/agent cmd/agent .github/workflows/ci.yml
git commit -m "feat(agent): record a started marker before a deployment changes the host" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Store — the frame is built and measured at plan time

**Files:**
- Modify: `internal/store/application_deployment.go` (`PlanRequest` fields, key always required, `checkFrame` in both plan paths, doc comment)
- Modify: `internal/store/application_apply.go` (`buildDeploymentFrame`, `frameBlocker`, `checkFrame`; `ApplyDeployment` uses them)
- Modify: `internal/api/application_handlers.go` (`maxFrameBytes`; `handlePlanDeployment` reads the preflight's endpoint and sets `MaxFrameBytes`; `handleApplyDeployment` uses `maxFrameBytes`)
- Test: `internal/store/deployment_test.go`, `internal/store/application_apply_test.go`, `internal/store/application_adoption_test.go`, `internal/store/recovery_test.go`, `internal/store/application_backup_test.go`, every `package store` test calling `PlanDeployment`; `internal/api/deployment_apply_test.go`
- Docs: `internal/store/AGENTS.md`, `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.DeploymentRequest.IssuedAt` (Task 1).
- Produces:
  ```go
  // store.PlanRequest, set by the API only
  MaxFrameBytes int                                     `json:"-"`
  Inspections   map[string]protocol.ContainerInspection `json:"-"` // read from Task 9 on
  func (t *tenancyStore) buildDeploymentFrame(ctx context.Context, tx *sql.Tx, a TenantAccess, d *Deployment, key []byte, now time.Time) (protocol.DeploymentRequest, error)
  func frameBlocker(req protocol.DeploymentRequest, now time.Time, maxFrameBytes int) string // "", "too_many_registry_hosts", "frame_invalid", "frame_too_large"
  func (t *tenancyStore) checkFrame(ctx context.Context, tx *sql.Tx, a TenantAccess, d *Deployment, key []byte, maxFrameBytes int) error
  // package api
  func maxFrameBytes(capabilities []string) int
  ```
  `PlanDeployment` now requires a 32-byte key on every plan (it decrypts values to measure). The frame is checked only when no other blocker remains: a service with blockers may have no resource row to name.

- [ ] **Step 1: Move the store tests to one key and a frame cap**

Every successful plan now decrypts the revision's values, so a test revision must be sealed under the key the plan passes. Make `imageCheckKey` the one key of `package store` tests:

```bash
sed -i 's/key := make(\[\]byte, 32)/key := imageCheckKey/' internal/store/application_apply_test.go
grep -l '^package store$' internal/store/*_test.go | xargs perl -pi -e 's/, nil, nil, false\)/, nil, imageCheckKey, false)/g'
git diff --stat internal/store
```

In `internal/store/application_adoption_test.go`, `adoptionFixtureSpec` seals an empty value bundle:

```go
	app, err := st.Tenancy().ImportApplication(ctx, a, "shop", spec, map[string]string{}, imageCheckKey)
```

In `internal/store/deployment_test.go`, `TestPlanDeploymentRefusesInvalidReplacementIdentity` creates its application the same way:

```go
	app, err := ts.ImportApplication(ctx, a, "shop", ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}, map[string]string{}, imageCheckKey)
```

and `planRequest` carries the cap the API sets for an agent with `deployment.pull`:

```go
func planRequest(m *ApplicationMapping) PlanRequest {
	return PlanRequest{InstanceID: m.InstanceID, MappingVersion: m.Version, Revision: m.Preview.Revision, Confirm: m.Preview.Project, MaxFrameBytes: protocol.MaxDeploymentRequestBytes}
}
```

In `internal/store/recovery_test.go` add, below `planAdopted`:

```go
// verifiedPlan adds what the API supplies to a plan request.
func verifiedPlan(r store.PlanRequest, containers []store.AdoptedContainer) store.PlanRequest {
	r.MaxFrameBytes = protocol.MaxDeploymentRequestBytes
	return r
}
```

give `planAdopted` a trailing `key []byte` parameter, plan with

```go
	d, err := ts.PlanDeployment(ctx, a, appID, verifiedPlan(store.PlanRequest{InstanceID: instance.ID, MappingVersion: 1, Revision: revision, Confirm: project}, mapping.Preview.Containers), nil, key, false)
```

and pass `key` at both calls (`planAdopted(t, ts, a, shop.ID, host.ID, "shop", shopContainer, 2, key)` and `planAdopted(t, ts, a, blog.ID, host.ID, "blog", blogContainer, 1, key)`).

In `internal/store/application_backup_test.go` the planned application must have sealed values; replace its creation and second revision with

```go
	app, err := ts.ImportApplication(ctx, a, "shop", desired("nginx:1"), map[string]string{"database-password": "backup-plain-value"}, cfg.Security.EncryptionKey)
	mustTenant(t, err)
	_, err = ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, desired("nginx:2"), map[string]string{"database-password": "backup-plain-value"}, cfg.Security.EncryptionKey)
	mustTenant(t, err)
```

and the plan with

```go
	planned, err := ts.PlanDeployment(ctx, a, app.ID, verifiedPlan(store.PlanRequest{InstanceID: instance.ID, MappingVersion: 1, Revision: 2, Confirm: "shop"}, mapping.Preview.Containers), nil, cfg.Security.EncryptionKey, false)
```

- [ ] **Step 2: Write the failing store tests**

In `internal/store/application_apply_test.go` replace `TestApplyDeploymentRefusesAnOversizedFrame` with:

```go
// One stored value may feed several variables, and JSON escapes '<' as six bytes, so a
// revision inside every value and per-service cap still marshals past the frame bound. The
// plan refuses it before any row exists.
func TestPlanDeploymentRefusesAnOversizedFrame(t *testing.T) {
	st, a, app, _, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	env := map[string]ApplicationSecretRef{}
	for _, n := range []string{"A", "B", "C", "D", "E", "F"} {
		env["V"+n] = ApplicationSecretRef{SecretRef: "web-a"}
	}
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Environment: env}}}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, map[string]string{"web-a": strings.Repeat("<", 10000)}, imageCheckKey); err != nil {
		t.Fatal(err)
	}
	m, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); !isBlocked(err, "frame_too_large") {
		t.Fatalf("oversized frame: %v", err)
	}
	if n := deploymentRows(t, st, app.ID); n != 0 {
		t.Fatalf("a refused plan left %d rows", n)
	}
}
```

(`TestApplyDeploymentHonoursTheEndpointFrameCap` stays: its plan fits 320 KiB and apply at the legacy cap still refuses.)

Append to `internal/store/deployment_test.go`:

```go
// The plan builds the real frame, secret values included, measures it against the endpoint's
// cap and drops it: at the legacy cap it is refused, at 320 KiB the stored plan holds no value.
func TestPlanDeploymentMeasuresTheFrame(t *testing.T) {
	st, a, app, _, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	env := map[string]ApplicationSecretRef{}
	for _, n := range []string{"A", "B", "C", "D"} {
		env["V"+n] = ApplicationSecretRef{SecretRef: "web-a"}
	}
	value := "frame-canary" + strings.Repeat("<", 9988)
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Environment: env}}}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, map[string]string{"web-a": value}, imageCheckKey); err != nil {
		t.Fatal(err)
	}
	m, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	legacy := planRequest(m)
	legacy.MaxFrameBytes = protocol.MaxDeploymentRequestBytesLegacy
	if _, err := ts.PlanDeployment(ctx, a, app.ID, legacy, nil, imageCheckKey, false); !isBlocked(err, "frame_too_large") {
		t.Fatalf("legacy cap: %v", err)
	}
	if n := deploymentRows(t, st, app.ID); n != 0 {
		t.Fatalf("a refused plan left %d rows", n)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, nil, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a plan without a key: %v", err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, make([]byte, 32), false); !errors.Is(err, ErrRevisionCorrupt) {
		t.Fatalf("a plan under the wrong key: %v", err)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := st.db.QueryRow(st.rebind(`SELECT plan FROM deployments WHERE id=?`), d.ID).Scan(&stored); err != nil || strings.Contains(stored, "frame-canary") {
		t.Fatalf("stored plan: %v", err)
	}
}

// An update plan decrypts the registry credential into the frame it measures and stores none of it.
func TestPlanDeploymentDropsTheCredentialItMeasured(t *testing.T) {
	st, a, app, _, _ := pullFixture(t, []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}}, map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}})
	ctx := context.Background()
	ts := st.Tenancy()
	f := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1.2": {digest: digestOf("f")}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "web"), f, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := st.db.QueryRow(st.rebind(`SELECT plan FROM deployments WHERE id=?`), d.ID).Scan(&stored); err != nil || strings.Contains(stored, pullCanary) {
		t.Fatalf("stored plan: %v", err)
	}
}

// frameBlocker names the first thing that stops a frame: credentials, validity, then size.
func TestFrameBlocker(t *testing.T) {
	now := time.Now()
	env := map[string]string{}
	for _, n := range []string{"A", "B", "C", "D"} {
		env["V"+n] = strings.Repeat("<", 10000)
	}
	good := protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: "ep_1", Project: "shop", Revision: 1, IssuedAt: now, Deadline: now.Add(DeploymentApplyDeadline), Services: []protocol.DeploymentService{{
		Name: "web", ContainerName: "shop-web", ImageID: "sha256:" + strings.Repeat("a", 64), Mounts: []protocol.Mount{}, Env: env,
		Replaces: protocol.InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000},
	}}}
	if b := frameBlocker(good, now, protocol.MaxDeploymentRequestBytes); b != "" {
		t.Fatalf("a 240 KiB frame at 320 KiB: %s", b)
	}
	if b := frameBlocker(good, now, protocol.MaxDeploymentRequestBytesLegacy); b != "frame_too_large" {
		t.Fatalf("a 240 KiB frame at 192 KiB: %s", b)
	}
	if b := frameBlocker(good, now, 1<<30); b != "" {
		t.Fatalf("a cap above the wire bound: %s", b)
	}
	invalid := good
	invalid.Deadline = now.Add(-time.Second)
	if b := frameBlocker(invalid, now, protocol.MaxDeploymentRequestBytes); b != "frame_invalid" {
		t.Fatalf("invalid: %s", b)
	}
	crowded := good
	crowded.Registries = map[string]protocol.RegistryAuth{}
	for i := range protocol.MaxRegistryAuthHosts + 1 {
		crowded.Registries[fmt.Sprintf("r%d.example.com", i)] = protocol.RegistryAuth{Username: "u", Secret: "s"}
	}
	if b := frameBlocker(crowded, now, protocol.MaxDeploymentRequestBytes); b != "too_many_registry_hosts" {
		t.Fatalf("17 hosts: %s", b)
	}
}
```

- [ ] **Step 3: Run the store tests to verify they fail**

Run: `go test -count=1 ./internal/store/ -run 'Frame|Plan'`
Expected: FAIL: `undefined: frameBlocker`; `unknown field MaxFrameBytes in struct literal of type PlanRequest`.

- [ ] **Step 4: Implement the store**

`internal/store/application_deployment.go`, `PlanRequest` gains:

```go
	// Set by the API, never read from a client: the largest frame the endpoint's agent accepts,
	// and one live inspection per mapped container, keyed by container ID.
	MaxFrameBytes int                                     `json:"-"`
	Inspections   map[string]protocol.ContainerInspection `json:"-"`
```

`PlanDeployment`'s comment ends "It sends no command. It decrypts the revision's values and credentials to build the frame apply would send, measures it against r.MaxFrameBytes and drops it; nothing secret is stored." Its opening checks become:

```go
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" || len(key) != 32 {
		return nil, ErrInvalid
	}
	if len(r.Update) > 0 {
		sorted := slices.Sorted(slices.Values(r.Update))
		if len(r.Update) > protocol.MaxDeploymentServices || len(slices.Compact(sorted)) != len(r.Update) || resolver == nil {
			return nil, ErrInvalid
		}
	}
```

In the no-update transaction, after `if err := blocked(blockers); err != nil { return err }`:

```go
				if err := t.checkFrame(ctx, tx, a, out, key, r.MaxFrameBytes); err != nil {
					return err
				}
```

In the update path's write transaction, after `out.CreatedAt, out.ExpiresAt = now, now.Add(DeploymentPlanTTL)`:

```go
			if err := t.checkFrame(wctx, tx, a, out, key, r.MaxFrameBytes); err != nil {
				return err
			}
```

`internal/store/application_apply.go`, add below `ApplyDeployment`:

```go
// buildDeploymentFrame resolves plan d into the frame an agent executes, issued at now: the
// revision's environment values and each pulled host's registry credential, decrypted with key
// and held only in the returned request. PlanDeployment measures it and drops it;
// ApplyDeployment sends it. A plan that no longer matches its revision, its adopted resources or
// the registry rows is ErrAdoptionChanged.
func (t *tenancyStore) buildDeploymentFrame(ctx context.Context, tx *sql.Tx, a TenantAccess, d *Deployment, key []byte, now time.Time) (protocol.DeploymentRequest, error) {
	spec, values, digest, err := t.resolveApplicationValues(ctx, tx, a, d.ApplicationID, d.Revision, key)
	if err != nil {
		return protocol.DeploymentRequest{}, err
	}
	if digest != d.SpecDigest || len(spec.Services) != len(d.Plan.Services) {
		return protocol.DeploymentRequest{}, ErrAdoptionChanged
	}
	names := map[string]string{}
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name FROM application_resources WHERE instance_id=? AND endpoint_id=?`), d.InstanceID, d.EndpointID)
	if err != nil {
		return protocol.DeploymentRequest{}, err
	}
	for rows.Next() {
		var cid, name string
		if err := rows.Scan(&cid, &name); err != nil {
			rows.Close()
			return protocol.DeploymentRequest{}, err
		}
		names[cid] = name
	}
	if err := rows.Close(); err != nil {
		return protocol.DeploymentRequest{}, err
	}
	if err := rows.Err(); err != nil {
		return protocol.DeploymentRequest{}, err
	}
	req := protocol.DeploymentRequest{Deployment: d.ID, Endpoint: d.EndpointID, Project: d.Plan.Project, Revision: d.Revision, IssuedAt: now, Deadline: now.Add(DeploymentApplyDeadline), Services: []protocol.DeploymentService{}, Volumes: d.Plan.Volumes}
	hosts := map[string]bool{}
	for i, ps := range d.Plan.Services {
		name, ok := names[ps.ContainerID]
		if !ok || spec.Services[i].Name != ps.Name {
			return protocol.DeploymentRequest{}, ErrAdoptionChanged
		}
		svc := protocol.DeploymentService{Name: ps.Name, ContainerName: name, ImageID: ps.ImageID, Replaces: ps.Replaces, Restart: ps.Restart, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: append([]protocol.Mount{}, ps.Mounts...)}
		if ps.PullDigest != "" {
			svc.ImageID, svc.Pull = "", &protocol.ImagePull{Reference: ps.PullReference, Digest: ps.PullDigest}
			// The agent moves the service's tag to the pulled image, so the next plan (and
			// Compose on the host) resolves the tag to the update rather than reverting it.
			ref, err := registry.ParseReference(ps.Reference)
			if err != nil {
				return protocol.DeploymentRequest{}, ErrAdoptionChanged
			}
			if ref.Digest == "" {
				svc.Pull.Tag = ref.Host + "/" + ref.Repository + ":" + ref.Tag
			}
			hosts[svc.Pull.Host()] = true
		}
		for _, p := range ps.Ports {
			svc.Ports = append(svc.Ports, protocol.Port{Container: p.Target, Host: p.Published, Protocol: p.Protocol, HostIP: p.HostIP})
		}
		for envName, ref := range spec.Services[i].Environment {
			svc.Env[envName] = values[ref.SecretRef]
		}
		req.Services = append(req.Services, svc)
	}
	// A credential travels once per host, decrypted here and never stored. A host whose row is
	// gone pulls anonymously only while the organization still allows it.
	for host := range hosts {
		_, cred, err := t.registryFor(ctx, tx, a.OrganizationID, host, key)
		if errors.Is(err, ErrNotFound) {
			anonymous, err := t.anonymousPull(ctx, tx, a.OrganizationID)
			if err != nil {
				return protocol.DeploymentRequest{}, err
			}
			if !anonymous {
				return protocol.DeploymentRequest{}, ErrAdoptionChanged
			}
			continue
		}
		if err != nil {
			return protocol.DeploymentRequest{}, err
		}
		if cred != nil {
			if req.Registries == nil {
				req.Registries = map[string]protocol.RegistryAuth{}
			}
			req.Registries[host] = protocol.RegistryAuth{Username: cred.Username, Secret: cred.Secret}
		}
	}
	return req, nil
}

// frameBlocker names what stops req reaching an agent that accepts maxFrameBytes, or "" when
// nothing does. Plan and apply both run it: capabilities can change between them.
func frameBlocker(req protocol.DeploymentRequest, now time.Time, maxFrameBytes int) string {
	if len(req.Registries) > protocol.MaxRegistryAuthHosts {
		return "too_many_registry_hosts"
	}
	if req.Validate(now) != nil {
		return "frame_invalid"
	}
	if raw, err := json.Marshal(req); err != nil || len(raw) > min(maxFrameBytes, protocol.MaxDeploymentRequestBytes) {
		return "frame_too_large"
	}
	return ""
}

// checkFrame builds the frame apply would send for d and refuses the plan when the endpoint's
// agent could not accept it. The frame, values and credentials included, is dropped.
func (t *tenancyStore) checkFrame(ctx context.Context, tx *sql.Tx, a TenantAccess, d *Deployment, key []byte, maxFrameBytes int) error {
	now := time.Now().UTC()
	req, err := t.buildDeploymentFrame(ctx, tx, a, d, key, now)
	if err != nil {
		return err
	}
	if b := frameBlocker(req, now, maxFrameBytes); b != "" {
		return blocked([]string{b})
	}
	return nil
}
```

In `ApplyDeployment`, replace everything from `spec, values, digest, err := t.resolveApplicationValues(` down to and including the marshalled-size check with:

```go
		now := time.Now().UTC()
		frame, err := t.buildDeploymentFrame(ctx, tx, a, d, key, now)
		if err != nil {
			return err
		}
		// The agent closes the session on a frame past its bound, so refuse it while the row
		// is still planned rather than send one that can only end unknown.
		if frameBlocker(frame, now, maxFrameBytes) != "" {
			return ErrInvalid
		}
```

and in the rest of that closure use `frame.Deadline` where it used `req.Deadline`, ending with

```go
		d.State, d.AppliedBy, d.AppliedAt, d.Deadline = "applying", a.ActorID, &now, &frame.Deadline
		out, req = d, &frame
		return nil
```

`registry` stays imported by `application_apply.go` (used by `buildDeploymentFrame`).

- [ ] **Step 5: Run the store tests on both drivers**

Run: `go test -race -count=1 ./internal/store/ && KY_TEST_POSTGRES_DSN=postgres://postgres:postgrespassword@127.0.0.1:5432/ky_server?sslmode=disable go test -count=1 ./internal/store/`
Expected: PASS on both. A remaining failure with `ErrRevisionCorrupt` at a plan is a test revision created with `CreateApplication`/`AppendApplicationRevision` (no sealed values) or sealed under another key: seal it with `ImportApplication`/`ReplaceApplicationRevision` under `imageCheckKey`, as Step 1 does. `grep -n 'AppendApplicationRevision\|CreateApplication' internal/store/*_test.go` lists the candidates; only those followed by a plan that must succeed need the change.

- [ ] **Step 6: Write the failing API test**

In `internal/api/deployment_apply_test.go`, after `online`:

```go
	legacy := []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect}
	pulling := []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentPull}
	reconnect := func(capabilities []string) *agentSocket {
		t.Helper()
		sock := online(capabilities)
		inventory(sock, newID, newImage, created.Add(30*time.Minute))
		sync(sock)
		waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })
		return sock
	}
```

(`newID`, `newImage`, `created`, `inventory` and `sync` are declared above `online`), use `online(legacy)` for both `online([]string{protocol.CapabilityDeploymentApply})` calls, add right after `planBody` is first built:

```go
	// The frame cap is the server's: a client cannot name one.
	request(admin, "POST", deployments, strings.TrimSuffix(string(planBody), "}")+`,"max_frame_bytes":1048576}`, 400)
```

and replace the final `for _, capabilities := range [][]string{...}` loop with:

```go
	// A frame past 192 KiB goes only to an agent with deployment.pull: an older one would close
	// its session on it. The plan measures against the connected agent's cap and apply measures
	// again, since the agent can change between them.
	sock.conn.CloseNow()
	sock = reconnect(legacy)
	must(json.Unmarshal([]byte(request(admin, "GET", mapping, "", 200)), &mapped))
	mappingBody, _ = json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: map[string]string{"web": newID}})
	request(admin, "PUT", mapping, string(mappingBody), 204)
	planBody, _ = json.Marshal(store.PlanRequest{InstanceID: instance.ID, MappingVersion: mapped.Version + 1, Revision: 2, Confirm: "shop"})
	if body := request(admin, "POST", deployments, string(planBody), 409); !strings.Contains(body, "frame_too_large") {
		t.Fatalf("a legacy agent's plan: %s", body)
	}
	sock.conn.CloseNow()
	sock = reconnect(pulling)
	wide := plan()
	sock.conn.CloseNow()
	sock = reconnect(legacy)
	request(admin, "POST", deployments+"/"+wide.ID+"/apply", `{"confirm":"shop"}`, 400)
	if got := state(wide.ID); got.State != "planned" {
		t.Fatalf("an oversized frame for a legacy agent moved the row: %+v", got)
	}
	sock.conn.CloseNow()
	sock = reconnect(pulling)
	request(admin, "POST", deployments+"/"+wide.ID+"/apply", `{"confirm":"shop"}`, 202)
	sock.conn.CloseNow()
```

(the `env`/`spec`/`ReplaceApplicationRevision` lines above the old loop stay).

- [ ] **Step 7: Run to verify it fails**

Run: `go test -count=1 ./internal/api/ -run TestApplyDeploymentOverTheAgentSocket`
Expected: FAIL at the first `plan()`: `409 ... "frame_too_large"`. The store now measures every plan, and the handler still passes `MaxFrameBytes` 0.

- [ ] **Step 8: Implement the API**

`internal/api/application_handlers.go`:

```go
// maxFrameBytes is the largest deployment frame an agent with capabilities accepts.
func maxFrameBytes(capabilities []string) int {
	if slices.Contains(capabilities, protocol.CapabilityDeploymentPull) {
		return protocol.MaxDeploymentRequestBytes
	}
	return protocol.MaxDeploymentRequestBytesLegacy
}
```

`handlePlanDeployment`, after the `len(input.Update) > 0` block and before `PlanDeployment`:

```go
	// The plan measures its frame against what this endpoint's agent accepts.
	pre, err := s.store.Tenancy().PreflightApplication(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, pre.EndpointID)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	input.MaxFrameBytes = maxFrameBytes(ep.Capabilities)
```

`handleApplyDeployment`: replace the four `maxFrame` lines with `maxFrame := maxFrameBytes(ep.Capabilities)`.

- [ ] **Step 9: Run the API and store suites on both drivers**

Run: `go test -race -count=1 ./internal/api/ ./internal/store/ && KY_TEST_POSTGRES_DSN=postgres://postgres:postgrespassword@127.0.0.1:5432/ky_server?sslmode=disable go test -count=1 ./internal/store/ ./internal/api/`
Expected: PASS.

- [ ] **Step 10: DOX and commit**

`internal/store/AGENTS.md`, `PlanDeployment` bullet: replace "a plan without `r.Update` is that single locked transaction and needs no resolver or key" with "every plan needs a 32-byte key (`ErrInvalid` otherwise); a plan without `r.Update` is that single locked transaction and needs no resolver", and add: "Once no other blocker remains, `checkFrame` builds the frame apply would send (`buildDeploymentFrame`, shared with apply: values and credentials decrypted in memory), measures it with `frameBlocker` against `PlanRequest.MaxFrameBytes` (set by the API from the endpoint's capabilities, `json:\"-\"`) and drops it: `too_many_registry_hosts` over 16 credentialed hosts, `frame_invalid` for any other `Validate` failure, `frame_too_large` past `min(MaxFrameBytes, 320 KiB)`. Nothing from the frame is stored." In the `ApplyDeployment` bullet: it builds the frame with `buildDeploymentFrame` (`IssuedAt` now) and refuses any `frameBlocker` finding with `ErrInvalid`.

`internal/api/AGENTS.md`, plan bullet: "`handlePlanDeployment` first reads `PreflightApplication` and the endpoint, and sets `MaxFrameBytes` from its capabilities with `maxFrameBytes` (320 KiB with `deployment.pull`, else 192 KiB), the same function apply uses; a body naming `max_frame_bytes` or `inspections` is 400 (strict decoding, the fields are `json:\"-\"`)."

```bash
gofmt -l internal cmd
git add internal/store internal/api
git commit -m "feat(store): build and measure the deployment frame at plan time" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Store — capability gating and clock skew at plan time

**Files:**
- Modify: `internal/store/application_deployment.go` (`draft`, `draftPlan`, `endpointCapabilities`, `capabilityBlockers`, both `PlanDeployment` paths)
- Modify: `internal/store/application_preflight.go:116-118` (`clock_skew`)
- Test: `internal/store/commands_test.go` (`activeEndpointWith`), `internal/store/deployment_test.go`, `internal/store/application_preflight_test.go`, `internal/store/recovery_test.go`, `internal/store/application_backup_test.go`; `internal/api/deployment_apply_test.go`, `internal/api/deployment_update_test.go`, `internal/api/application_handlers_test.go`
- Docs: `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.CapabilityDeploymentApply`, `CapabilityDeploymentPull`, `CapabilityContainerInspect`, `MaxClockSkew` (Task 1); `checkFrame` (Task 6).
- Produces:
  ```go
  type draft struct {
  	d            *Deployment
  	m            *ApplicationMapping
  	blockers     []string
  	capabilities map[string]bool
  }
  func (t *tenancyStore) draftPlan(ctx context.Context, tx *sql.Tx, a TenantAccess, app, planID string, r PlanRequest, lock bool) (*draft, error)
  func (t *tenancyStore) endpointCapabilities(ctx context.Context, tx *sql.Tx, endpoint string) (map[string]bool, error)
  func capabilityBlockers(capabilities map[string]bool, plan DeploymentPlan) []string
  ```
  Task 9 extends `draft` and `draftPlan`.

- [ ] **Step 1: Give the store fixtures an agent that can deploy**

`internal/store/commands_test.go`, `activeEndpointWith`, right after `ApproveEndpoint`:

```go
	if err := ts.SetEndpointCapabilities(ctx, enrolled.ID, []string{protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull, protocol.CapabilityDeploymentRemove}); err != nil {
		t.Fatal(err)
	}
```

`internal/store/recovery_test.go:93` (sorted, as the endpoint read returns them):

```go
	capabilities := []string{"compose.v1", protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply, "logs"}
```

`internal/store/application_backup_test.go`, after its `ApproveEndpoint`:

```go
	mustTenant(t, ts.SetEndpointCapabilities(ctx, endpoint.ID, []string{protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply}))
```

- [ ] **Step 2: Write the failing store tests**

Append to `internal/store/deployment_test.go`:

```go
// A plan the endpoint's agent could not run is refused at plan time, not at apply.
func TestPlanDeploymentRequiresAgentCapabilities(t *testing.T) {
	st, a, app, endpoint, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for name, tc := range map[string]struct {
		capabilities []string
		want         []string
	}{
		"none":       {nil, []string{"agent_deploy_unsupported", "agent_inspect_unsupported"}},
		"no deploy":  {[]string{protocol.CapabilityContainerInspect}, []string{"agent_deploy_unsupported"}},
		"no inspect": {[]string{protocol.CapabilityDeploymentApply}, []string{"agent_inspect_unsupported"}},
	} {
		if err := ts.SetEndpointCapabilities(ctx, endpoint, tc.capabilities); err != nil {
			t.Fatal(err)
		}
		var blocked *PreflightBlockedError
		if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, tc.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := ts.SetEndpointCapabilities(ctx, endpoint, []string{protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
}

// An update that pulls needs deployment.pull; a plan with no pull does not.
func TestPlanDeploymentRefusesAPullTheAgentCannotRun(t *testing.T) {
	st, a, app, endpoint, _ := pullFixture(t, []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}}, map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}})
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.SetEndpointCapabilities(ctx, endpoint, []string{protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply}); err != nil {
		t.Fatal(err)
	}
	f := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1.2": {digest: digestOf("f")}}}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "web"), f, imageCheckKey, false); !isBlocked(err, "agent_pull_unsupported") {
		t.Fatalf("pull without deployment.pull: %v", err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app), nil, imageCheckKey, false); err != nil {
		t.Fatalf("a plan without a pull: %v", err)
	}
}
```

Append to `internal/store/application_preflight_test.go`:

```go
// An inventory whose agent clock disagrees with the server's by more than MaxClockSkew blocks
// preflight and plan; inside the bound it does not.
func TestPreflightBlocksClockSkew(t *testing.T) {
	st, a, app, endpoint, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	skew := func(received, observed time.Duration) {
		t.Helper()
		now := time.Now().UTC()
		if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_inventory SET received_at=?, observed_at=? WHERE endpoint_id=?`), now.Add(received), now.Add(observed), endpoint); err != nil {
			t.Fatal(err)
		}
	}
	skew(-2*time.Minute, 4*time.Minute) // six minutes apart, each inside freshInventory's windows
	p, err := ts.PreflightApplication(ctx, a, app.ID)
	if err != nil || p.Executable || !slices.Contains(p.Blockers, "clock_skew") {
		t.Fatalf("skewed preflight: %+v %v", p, err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); !isBlocked(err, "clock_skew") {
		t.Fatalf("skewed plan: %v", err)
	}
	skew(-time.Minute, 3*time.Minute)
	if p, err = ts.PreflightApplication(ctx, a, app.ID); err != nil || slices.Contains(p.Blockers, "clock_skew") {
		t.Fatalf("four minutes apart: %+v %v", p, err)
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test -count=1 ./internal/store/ -run 'AgentCapabilities|PullTheAgent|ClockSkew'`
Expected: FAIL: the plans succeed with no capability; no `clock_skew` blocker.

- [ ] **Step 4: Implement**

`internal/store/application_preflight.go`, in `preflight` after `out := buildDeploymentPreflight(...)`:

```go
	// observed_at is the agent's clock, received_at the server's.
	if d := observed.Sub(received); d > protocol.MaxClockSkew || d < -protocol.MaxClockSkew {
		out.Blockers = append(out.Blockers, "clock_skew")
		out.Executable = false
	}
```

`internal/store/application_deployment.go`, replace `draftPlan` and add the helpers:

```go
// draft is a plan built in memory from one preflight, with every blocker found for it and the
// endpoint's capabilities, which the update path checks again once its pulls are pinned.
type draft struct {
	d            *Deployment
	m            *ApplicationMapping
	blockers     []string
	capabilities map[string]bool
}

// draftPlan runs the preflight (locking when the plan is written in the same transaction) and
// builds the plan in memory, returning its blockers unrefused so an update can add its own.
func (t *tenancyStore) draftPlan(ctx context.Context, tx *sql.Tx, a TenantAccess, app, planID string, r PlanRequest, lock bool) (*draft, error) {
	p, m, spec, snapshot, digest, err := t.preflight(ctx, tx, a, app, lock, r.Revision)
	if err != nil {
		return nil, err
	}
	if r.InstanceID != m.InstanceID || r.MappingVersion != m.Version || (r.Revision != 0 && r.Revision != p.Revision) || r.Confirm != m.Preview.Project {
		return nil, ErrAdoptionChanged
	}
	capabilities, err := t.endpointCapabilities(ctx, tx, m.Preview.EndpointID)
	if err != nil {
		return nil, err
	}
	blockers := slices.Clone(p.Blockers)
	plan := DeploymentPlan{Project: m.Preview.Project, Services: []PlannedService{}}
	// The preflight resolved each reference to one full image ID, which is the pin. Record
	// the repository digest beside it only when inventory reported exactly one; it is
	// advisory.
	digests := map[string]string{}
	for _, im := range snapshot.Images {
		if len(im.Digests) == 1 {
			digests[im.ID] = im.Digests[0]
		}
	}
	for i, s := range spec.Services {
		row := p.Services[i]
		blockers = append(blockers, row.Blockers...)
		refs := make([]string, 0, len(s.Environment))
		for _, ref := range s.Environment {
			refs = append(refs, ref.SecretRef)
		}
		slices.Sort(refs)
		ps := PlannedService{Name: s.Name, Reference: s.Image, ImageID: row.ImageID, ImageDigest: digests[row.ImageID], ContainerID: row.ContainerID, Restart: s.Restart, Ports: s.Ports, SecretRefs: refs, Mounts: row.Mounts}
		if ps.Ports == nil {
			ps.Ports = []ApplicationPort{}
		}
		if len(row.DroppedMounts) > 0 {
			ps.DroppedMounts = row.DroppedMounts
		}
		for _, mount := range row.Mounts {
			if mount.Kind == protocol.MountVolume && !slices.Contains(plan.Volumes, mount.Source) {
				plan.Volumes = append(plan.Volumes, mount.Source)
			}
		}
		if row.InspectionTarget != nil {
			ps.Replaces = *row.InspectionTarget
		}
		plan.Services = append(plan.Services, ps)
	}
	blockers = append(blockers, capabilityBlockers(capabilities, plan)...)
	now := time.Now().UTC()
	d := &Deployment{ID: planID, ApplicationID: app, InstanceID: m.InstanceID, EndpointID: m.Preview.EndpointID, EndpointName: m.Preview.EndpointName, Kind: "apply", State: "planned", Revision: p.Revision, SpecDigest: digest, MappingVersion: m.Version, Plan: plan, CreatedBy: a.ActorID, CreatedAt: now, ExpiresAt: now.Add(DeploymentPlanTTL)}
	return &draft{d: d, m: m, blockers: blockers, capabilities: capabilities}, nil
}

// endpointCapabilities is what the endpoint's agent advertised at its last connect.
func (t *tenancyStore) endpointCapabilities(ctx context.Context, tx *sql.Tx, endpoint string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT capability FROM endpoint_capabilities WHERE endpoint_id=?`), endpoint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out[c] = true
	}
	return out, rows.Err()
}

// capabilityBlockers refuses a plan the endpoint's agent could not run: no deployments, no live
// inspection for the plan to check, or a pull without deployment.pull. Apply checks again.
func capabilityBlockers(capabilities map[string]bool, plan DeploymentPlan) []string {
	var out []string
	if !capabilities[protocol.CapabilityDeploymentApply] {
		out = append(out, "agent_deploy_unsupported")
	}
	if !capabilities[protocol.CapabilityContainerInspect] {
		out = append(out, "agent_inspect_unsupported")
	}
	if !capabilities[protocol.CapabilityDeploymentPull] && slices.ContainsFunc(plan.Services, func(ps PlannedService) bool { return ps.PullDigest != "" }) {
		out = append(out, "agent_pull_unsupported")
	}
	return out
}
```

In `PlanDeployment`, the no-update closure becomes:

```go
		err = t.withTenantTarget(ctx, a, permissions.ApplicationDeploy, target, func(tx *sql.Tx) error {
			dr, err := t.draftPlan(ctx, tx, a, id.String(), planID, r, true)
			if err != nil {
				return err
			}
			if err := blocked(dr.blockers); err != nil {
				return err
			}
			if err := t.checkFrame(ctx, tx, a, dr.d, key, r.MaxFrameBytes); err != nil {
				return err
			}
			out = dr.d
			return t.insertPlan(ctx, tx, a, out, dr.m.InstanceID)
		})
```

In the update path declare `var capabilities map[string]bool` beside `var state string`, open the read closure with

```go
		dr, err := t.draftPlan(ctx, tx, a, id.String(), planID, r, false)
		if err != nil {
			return err
		}
		d, m, blockers := dr.d, dr.m, dr.blockers
		capabilities = dr.capabilities
```

and, after the loop that turns resolver answers into pins or blockers, add:

```go
	// Only now is it known which services pull.
	blockers = append(blockers, capabilityBlockers(capabilities, out.Plan)...)
```

- [ ] **Step 5: Run the store suite on both drivers**

Run: `go test -race -count=1 ./internal/store/ && KY_TEST_POSTGRES_DSN=postgres://postgres:postgrespassword@127.0.0.1:5432/ky_server?sslmode=disable go test -count=1 ./internal/store/`
Expected: PASS.

- [ ] **Step 6: Update the API tests for capability-gated plans**

`internal/api/application_handlers_test.go`, after `must(ts.ApproveEndpoint(ctx, a, ep.ID, ep.Fingerprint))`:

```go
	must(ts.SetEndpointCapabilities(ctx, ep.ID, []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect}))
```

`internal/api/deployment_apply_test.go`, replace the "An agent that does not advertise the capability is never sent a plan." block with:

```go
	// An agent that does not advertise deployments is never planned for, and a plan made before
	// its capabilities changed is refused at apply with nothing sent.
	sock = reconnect(legacy)
	third := plan()
	sock.conn.CloseNow()
	sock = reconnect(nil)
	if body := request(admin, "POST", deployments, string(planBody), 409); !strings.Contains(body, "agent_deploy_unsupported") || !strings.Contains(body, "agent_inspect_unsupported") {
		t.Fatalf("a plan for an agent without deployments: %s", body)
	}
	request(admin, "POST", deployments+"/"+third.ID+"/apply", `{"confirm":"shop"}`, 501)
	if got := state(third.ID); got.State != "planned" {
		t.Fatalf("a refused apply moved the row: %+v", got)
	}
	sock.conn.CloseNow()
```

`internal/api/deployment_update_test.go`: the first connection advertises everything an update plan needs,

```go
	sock := online(protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull, protocol.CapabilityContainerInspect)
```

the "An agent without deployment.pull is never sent a pull." block becomes

```go
	// An agent without deployment.pull is never planned a pull, and a pull planned before its
	// capabilities changed is refused at apply with nothing sent.
	planned = plan()
	sock.conn.CloseNow()
	sock = online(protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect)
	if body := request("POST", deployments, string(planBody), 409); !strings.Contains(body, "agent_pull_unsupported") {
		t.Fatalf("a pull for an agent without deployment.pull: %s", body)
	}
	apply := deployments + "/" + planned.ID + "/apply"
	if body := request("POST", apply, `{"confirm":"shop"}`, 501); !strings.Contains(body, "Upgrade the host agent to enable deployments that pull images") {
		t.Fatalf("pull without the capability: %s", body)
	}
```

(the `var d store.Deployment` state check and the plain plan/apply that follow are unchanged), and the last reconnect is `sock = online(protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull, protocol.CapabilityContainerInspect)`.

- [ ] **Step 7: Run the API suite on both drivers**

Run: `go test -race -count=1 ./internal/api/ && KY_TEST_POSTGRES_DSN=postgres://postgres:postgrespassword@127.0.0.1:5432/ky_server?sslmode=disable go test -count=1 ./internal/api/`
Expected: PASS.

- [ ] **Step 8: DOX and commit**

`internal/store/AGENTS.md`: `PreflightApplication` bullet gains "`clock_skew` (top level) when the inventory row's `observed_at` (agent clock) and `received_at` (server clock) differ by more than `protocol.MaxClockSkew`; `freshInventory`'s windows are unchanged." `PlanDeployment` bullet gains "`draftPlan` reads `endpoint_capabilities` (`endpointCapabilities`) and `capabilityBlockers` adds `agent_deploy_unsupported` without `deployment.apply`, `agent_inspect_unsupported` without `container.inspect`, and, once pulls are pinned (the update path checks again after resolving), `agent_pull_unsupported` for a pulling plan without `deployment.pull`. Apply keeps its own 501s: capabilities can change between plan and apply."

```bash
gofmt -l internal cmd
git add internal/store internal/api
git commit -m "feat(store): refuse plans the agent cannot run or whose clock is skewed" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: API — one live inspection per mapped service at plan time

**Files:**
- Modify: `internal/api/inspection.go` (sentinel errors, `inspect`, `handleContainerInspection` over it)
- Create: `internal/api/plan_inspection.go` (`planInspectionBudget`, `planInspections`)
- Modify: `internal/api/server.go:55-56` (`planInspector` field), `internal/api/application_handlers.go` (`handlePlanDeployment` fills `Inspections`)
- Modify: `internal/api/export_test.go` (`SetPlanInspectorForTest`, `PlanInspectionBudgetForTest`)
- Create: `internal/api/deployment_plan_test.go` (fixture `newPlanHost`, helpers, fan-out tests)
- Test: `internal/api/deployment_apply_test.go`, `internal/api/deployment_update_test.go`, `internal/api/application_handlers_test.go` (install the verified inspector)
- Docs: `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: `store.PlanRequest.Inspections` (Task 6, `json:"-"`), `protocol.CapabilityContainerInspect` (Task 1), `store.DeploymentPreflight.Services[].InspectionTarget`.
- Produces:
  ```go
  const planInspectionBudget = 10 * time.Second
  func (s *Server) inspect(ctx context.Context, agent *agentConn, actor, org string, target protocol.InspectionTarget, allowed func() bool) (protocol.ContainerInspection, error)
  func (s *Server) planInspections(w http.ResponseWriter, r *http.Request, a store.TenantAccess, ep *store.Endpoint, pre *store.DeploymentPreflight) map[string]protocol.ContainerInspection
  // Server.planInspector func(context.Context, protocol.InspectionTarget) (protocol.ContainerInspection, error) // tests; nil = the agent
  // export_test.go
  func SetPlanInspectorForTest(s *Server, f func(context.Context, protocol.InspectionTarget) (protocol.ContainerInspection, error))
  const PlanInspectionBudgetForTest = planInspectionBudget
  // deployment_plan_test.go (package api_test), used again by Task 9
  type planHost struct { s *api.Server; st store.Store; admin *http.Cookie; ag enrolledAgent; deployments, planBody string; targets []protocol.InspectionTarget }
  func newPlanHost(t *testing.T, capabilities []string, services ...string) planHost
  func (h planHost) do(t *testing.T, method, path, body string, status int) string
  func verifiedObservation(target protocol.InspectionTarget) protocol.ContainerInspection
  func verifiedInspector(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error)
  ```
  This task only gathers inspections; the store starts refusing on them in Task 9. The inspection route sends `inspection.cancel` only when it gave up before an answer (an answered request needs none), which keeps a fake agent socket's frame sequence deterministic.

- [ ] **Step 1: Write the fixture and the failing tests**

Create `internal/api/deployment_plan_test.go`:

```go
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// planHost is an application adopted and mapped on an approved endpoint whose inventory the
// store accepted directly: one container per service, every one on the same local image.
type planHost struct {
	s           *api.Server
	st          store.Store
	admin       *http.Cookie
	ag          enrolledAgent
	deployments string
	planBody    string
	targets     []protocol.InspectionTarget // per service, in the order given
}

func newPlanHost(t *testing.T, capabilities []string, services ...string) planHost {
	t.Helper()
	s, st, _ := setupTestServer(t)
	ctx := context.Background()
	ts := st.Tenancy()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}))
	must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}))
	h := planHost{s: s, st: st, admin: loginAs(t, s, st, "planner", "user")}
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_planner", Role: store.RoleOrganizationAdmin, Status: "active"}))
	h.ag = enrollAgent(t, s, st, h.admin, "plan-host")
	h.do(t, "POST", "/api/organizations/a/endpoints/"+h.ag.id+"/approve", `{"fingerprint":"`+h.ag.fp+`"}`, 204)
	must(ts.SetEndpointCapabilities(ctx, h.ag.id, capabilities))
	image, created := "sha256:"+strings.Repeat("b", 64), time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	snapshot := protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Images: []protocol.Image{{ID: image, Tags: []string{"nginx:1"}}}}
	compose, bindings := []string{}, map[string]string{}
	for i, name := range services {
		id := strings.Repeat(strconv.Itoa(i+1), 64)
		snapshot.Containers = append(snapshot.Containers, protocol.Container{ID: id, Name: "shop-" + name, ImageID: image, ComposeProject: "shop", CreatedAt: created, Mounts: []protocol.Mount{}})
		compose = append(compose, name+": {image: nginx:1}")
		bindings[name] = id
		h.targets = append(h.targets, protocol.InspectionTarget{ContainerID: id, ImageID: image, CreatedUnix: created.Unix()})
	}
	raw, _ := json.Marshal(snapshot)
	_, err := ts.AcceptInventory(ctx, h.ag.id, uint64(time.Now().Unix()), time.Now(), raw)
	must(err)
	base := "/api/organizations/a/environments/env-a/applications"
	importBody, _ := json.Marshal(map[string]string{"name": "shop", "compose": "services: {" + strings.Join(compose, ", ") + "}"})
	var app store.Application
	must(json.Unmarshal([]byte(h.do(t, "POST", base, string(importBody), 201)), &app))
	adoption := base + "/" + app.ID + "/adoption"
	var preview store.AdoptionPreview
	must(json.Unmarshal([]byte(h.do(t, "GET", adoption+"?endpoint="+h.ag.id+"&project=shop", "", 200)), &preview))
	adoptionBody, _ := json.Marshal(store.AdoptionRequest{EndpointID: h.ag.id, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	var instance store.ApplicationInstance
	must(json.Unmarshal([]byte(h.do(t, "POST", adoption, string(adoptionBody), 201)), &instance))
	mapping := base + "/" + app.ID + "/mapping"
	var mapped store.ApplicationMapping
	must(json.Unmarshal([]byte(h.do(t, "GET", mapping, "", 200)), &mapped))
	mappingBody, _ := json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: bindings})
	h.do(t, "PUT", mapping, string(mappingBody), 204)
	h.deployments = base + "/" + app.ID + "/deployments"
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: instance.ID, MappingVersion: mapped.Version + 1, Revision: 1, Confirm: "shop"})
	h.planBody = string(planBody)
	return h
}

func (h planHost) do(t *testing.T, method, path, body string, status int) string {
	t.Helper()
	w := tenantRequest(h.s, h.admin, method, path, body, true)
	if w.Code != status {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// preflightTargets are the mapped containers in plan order, as the preflight lists them.
func (h planHost) preflightTargets(t *testing.T) []protocol.InspectionTarget {
	t.Helper()
	var pre store.DeploymentPreflight
	if err := json.Unmarshal([]byte(h.do(t, "GET", strings.TrimSuffix(h.deployments, "deployments")+"preflight", "", 200)), &pre); err != nil {
		t.Fatal(err)
	}
	out := []protocol.InspectionTarget{}
	for _, svc := range pre.Services {
		out = append(out, *svc.InspectionTarget)
	}
	return out
}

// verifiedObservation is what an agent reports for a container a recreate fully expresses.
func verifiedObservation(target protocol.InspectionTarget) protocol.ContainerInspection {
	return protocol.ContainerInspection{Target: target, ObservedAt: time.Now().UTC(), State: "running", RestartPolicy: "no", NetworkMode: "bridge", ImagePlatform: protocol.ImagePlatform{OS: "linux", Architecture: "amd64"}, Ports: []protocol.Port{}, Unsupported: []string{}, ConfigurationVerified: true}
}

// verifiedInspector answers every plan-time inspection with a verified observation.
func verifiedInspector(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
	return verifiedObservation(target), nil
}

var inspecting = []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect}

// The plan inspects each mapped service's container one at a time, in plan order, all under
// the plan's budget.
func TestPlanInspectsEachMappedServiceInTurn(t *testing.T) {
	h := newPlanHost(t, inspecting, "web", "db")
	var mu sync.Mutex
	var seen []protocol.InspectionTarget
	inFlight, most := 0, 0
	start := time.Now()
	api.SetPlanInspectorForTest(h.s, func(ctx context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
		mu.Lock()
		inFlight++
		most = max(most, inFlight)
		seen = append(seen, target)
		mu.Unlock()
		defer func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()
		if deadline, ok := ctx.Deadline(); !ok || deadline.After(start.Add(api.PlanInspectionBudgetForTest+time.Second)) {
			t.Errorf("inspection deadline %v (set %v) outlives the plan budget", deadline, ok)
		}
		time.Sleep(20 * time.Millisecond)
		return verifiedObservation(target), nil
	})
	want := h.preflightTargets(t)
	h.do(t, "POST", h.deployments, h.planBody, 201)
	mu.Lock()
	defer mu.Unlock()
	if most != 1 || !slices.Equal(seen, want) {
		t.Fatalf("at most %d at once, inspected %v, want %v", most, seen, want)
	}
}

// A plan's inspections share one inspection attempt, however many services it maps.
func TestPlanCountsOneInspectionAttempt(t *testing.T) {
	h := newPlanHost(t, inspecting, "web", "db")
	var mu sync.Mutex
	calls := 0
	api.SetPlanInspectorForTest(h.s, func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return verifiedObservation(target), nil
	})
	for range 29 {
		api.AllowAttemptForTest(h.s, "inspection:usr_planner", 30, time.Minute)
	}
	h.do(t, "POST", h.deployments, h.planBody, 201)
	mu.Lock()
	first := calls
	mu.Unlock()
	if first != 2 {
		t.Fatalf("the 30th attempt inspected %d services", first)
	}
	h.do(t, "POST", h.deployments, h.planBody, 201)
	mu.Lock()
	defer mu.Unlock()
	if calls != first {
		t.Fatal("a plan over the inspection limit inspected")
	}
}

// Without container.inspect nothing is inspected, no attempt is spent, and the store names why.
func TestPlanSkipsInspectionWithoutTheCapability(t *testing.T) {
	h := newPlanHost(t, []string{protocol.CapabilityDeploymentApply}, "web")
	api.SetPlanInspectorForTest(h.s, func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
		t.Error("inspected without container.inspect")
		return verifiedObservation(target), nil
	})
	body := h.do(t, "POST", h.deployments, h.planBody, 409)
	if !strings.Contains(body, "agent_inspect_unsupported") || strings.Contains(body, "inspection_unavailable") {
		t.Fatalf("blockers: %s", body)
	}
	if slices.Contains(api.AttemptKeysForTest(h.s), "inspection:usr_planner") {
		t.Fatal("an inspection attempt was spent")
	}
}

// Inspections are the server's to gather: a client cannot hand one in (Review Focus 1).
func TestPlanRefusesClientSuppliedInspections(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	forged := `,"inspections":{"` + h.targets[0].ContainerID + `":{"configuration_verified":true,"unsupported":[]}}}`
	h.do(t, "POST", h.deployments, strings.TrimSuffix(h.planBody, "}")+forged, 400)
}

// Over a real agent socket the plan sends one grant per mapped container, expiring within the
// plan budget, and plans on the agent's answer.
func TestPlanInspectsOverTheAgentSocket(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	httpSrv := httptest.NewServer(h.s)
	defer httpSrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sock, reason := connect(t, ctx, httpSrv.URL, h.ag, h.ag.priv, protocol.Version)
	if sock == nil {
		t.Fatalf("connect refused: %s", reason)
	}
	defer sock.conn.CloseNow()
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: inspecting})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeHeartbeat {
		t.Fatalf("expected a heartbeat, got %s", f.Type)
	}
	start := time.Now()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- tenantRequest(h.s, h.admin, "POST", h.deployments, h.planBody, true) }()
	frame := readEnvelope(t, ctx, sock.conn)
	var grant protocol.InspectionOpen
	if frame.Type != protocol.TypeInspectionOpen || json.Unmarshal(frame.Payload, &grant) != nil || grant.Validate(time.Now()) != nil {
		t.Fatalf("expected an inspection grant, got %s", frame.Type)
	}
	if grant.Target != h.targets[0] || grant.Actor != "usr_planner" || grant.Expires.After(start.Add(api.PlanInspectionBudgetForTest+time.Second)) {
		t.Fatalf("grant: %+v", grant)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInspectionResult, protocol.InspectionResult{Request: grant.Request, Status: "ok", Result: ptr(verifiedObservation(grant.Target))})
	if w := <-response; w.Code != 201 {
		t.Fatalf("plan: %d %s", w.Code, w.Body.String())
	}
}

func ptr[T any](v T) *T { return &v }
```

If `ptr` already exists in `package api_test` (`grep -n 'func ptr' internal/api/*_test.go`), drop the definition above and use the existing one.

In `internal/api/deployment_apply_test.go`, `internal/api/deployment_update_test.go` and `internal/api/application_handlers_test.go`, right after `setupTestServer(t)`, install the verified inspector (these tests are about apply, updates and routes, not inspection; a fake agent socket would otherwise receive grants it does not answer):

```go
	api.SetPlanInspectorForTest(s, verifiedInspector)
```

adding `"github.com/Busnes-app/kyyard-server/internal/api"` to the imports of the two files that lack it.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/api/ -run 'TestPlan'`
Expected: FAIL to compile: `undefined: api.SetPlanInspectorForTest`, `undefined: api.PlanInspectionBudgetForTest`.

- [ ] **Step 3: Implement the extracted inspection**

`internal/api/inspection.go` (add `"errors"` to the imports):

```go
// Why an inspection ended without a result; handleContainerInspection maps each to one status
// and a plan reads every one as no inspection.
var (
	errInspectionCapacity    = errors.New("inspection capacity reached")
	errInspectionStopping    = errors.New("server shutting down")
	errInspectionForbidden   = errors.New("inspection access changed")
	errInspectionSend        = errors.New("inspection could not be sent")
	errInspectionTimeout     = errors.New("inspection did not complete")
	errInspectionGone        = errors.New("agent disconnected")
	errInspectionBusy        = errors.New("agent inspection capacity reached")
	errInspectionUnavailable = errors.New("runtime inspection unavailable")
	errInspectionInvalid     = errors.New("invalid inspection response")
)

// inspect asks agent for one validated observation of target on behalf of actor. It holds an
// admission slot throughout, expires the grant at the earlier of ctx's deadline and
// InspectionLifetime, re-checks the agent and allowed every second and on the answer, and
// cancels the grant on the agent only when it gave up before an answer.
func (s *Server) inspect(ctx context.Context, agent *agentConn, actor, org string, target protocol.InspectionTarget, allowed func() bool) (protocol.ContainerInspection, error) {
	var none protocol.ContainerInspection
	p := s.inspections.open(agent, actor, org)
	if p == nil {
		return none, errInspectionCapacity
	}
	defer s.inspections.release(p)
	if s.stopping.Load() {
		return none, errInspectionStopping
	}
	expires := time.Now().UTC().Add(protocol.InspectionLifetime)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expires) {
		expires = deadline.UTC()
	}
	ctx, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	if !s.inspectionAgentCurrent(agent) || !allowed() {
		return none, errInspectionForbidden
	}
	grant := protocol.InspectionOpen{Request: p.id, Endpoint: agent.endpointID, Actor: actor, Connection: agent.nonce, Expires: expires, Target: target}
	select {
	case agent.send <- envelope(protocol.TypeInspectionOpen, grant):
	default:
		return none, errInspectionSend
	}
	answered := false
	defer func() {
		if answered {
			return
		}
		select {
		case agent.send <- envelope(protocol.TypeInspectionCancel, protocol.InspectionCancel{Request: p.id}):
		default:
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return none, errInspectionTimeout
		case <-p.done:
			return none, errInspectionGone
		case <-agent.closed:
			return none, errInspectionGone
		case <-ticker.C:
			if !s.inspectionAgentCurrent(agent) {
				return none, errInspectionGone
			}
			if !allowed() {
				return none, errInspectionForbidden
			}
		case reply := <-p.result:
			answered = true
			if !allowed() {
				return none, errInspectionForbidden
			}
			switch reply.Status {
			case "busy":
				return none, errInspectionBusy
			case "unavailable":
				return none, errInspectionUnavailable
			case "ok":
				if reply.Result != nil && reply.Result.Validate(target, time.Now()) == nil {
					return *reply.Result, nil
				}
			}
			return none, errInspectionInvalid
		}
	}
}
```

Replace `handleContainerInspection` from `p := s.inspections.open(...)` to the end with:

```go
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(protocol.InspectionLifetime + 2*time.Second))
	result, err := s.inspect(r.Context(), agent, a.ActorID, a.OrganizationID, target, func() bool { return s.inspectionAllowed(r, a, endpoint) })
	switch err {
	case nil, errInspectionBusy, errInspectionUnavailable, errInspectionInvalid:
		// The agent answered: the answer counts only for the target still recorded.
		fresh, err := s.store.Tenancy().ReadInspectionTarget(r.Context(), a, endpoint, target.ContainerID)
		if err != nil {
			s.tenantError(w, err)
			return
		}
		if fresh != target || !s.inspectionAgentCurrent(agent) {
			s.writeError(w, 409, "Inspection target changed; refresh before retrying")
			return
		}
	}
	switch err {
	case nil:
		s.writeJSON(w, 200, result)
	case errInspectionCapacity:
		s.writeError(w, 429, "Inspection capacity reached")
	case errInspectionStopping:
		s.writeError(w, 503, "Server shutting down")
	case errInspectionForbidden:
		s.writeError(w, 403, "Inspection access changed")
	case errInspectionSend:
		s.writeError(w, 503, "Inspection unavailable")
	case errInspectionTimeout:
		s.writeError(w, 504, "Inspection did not complete")
	case errInspectionGone:
		s.writeError(w, 409, "The agent disconnected; refresh before retrying")
	case errInspectionBusy:
		s.writeError(w, 429, "Agent inspection capacity reached")
	case errInspectionUnavailable:
		s.writeError(w, 409, "Runtime inspection unavailable; refresh before retrying")
	default:
		s.writeError(w, 502, "Invalid inspection response")
	}
}
```

(the handler's endpoint parse, rate limit, target read, endpoint read, capability check and agent lookup above stay as they are).

- [ ] **Step 4: Implement the fan-out**

`internal/api/server.go`, below `digestResolver`:

```go
	// planInspector replaces the agent round trip of one plan-time inspection; nil means the
	// connected agent. Tests only.
	planInspector func(context.Context, protocol.InspectionTarget) (protocol.ContainerInspection, error)
```

(add the `protocol` import to `server.go` if it lacks it).

Create `internal/api/plan_inspection.go`:

```go
package api

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// planInspectionBudget bounds a plan's whole live-inspection fan-out.
const planInspectionBudget = 10 * time.Second

// planInspections inspects each mapped service's container, in plan order and one at a time,
// under one budget and one inspection attempt, so the store can refuse at plan time what the
// agent would deny at apply. A failure of any kind leaves that container without an entry; an
// agent without container.inspect gets no request. The observations are consumed by the plan
// and never stored.
func (s *Server) planInspections(w http.ResponseWriter, r *http.Request, a store.TenantAccess, ep *store.Endpoint, pre *store.DeploymentPreflight) map[string]protocol.ContainerInspection {
	out := map[string]protocol.ContainerInspection{}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspect) || !s.allowAttempt("inspection:"+a.ActorID, 30, time.Minute) {
		return out
	}
	ctx, cancel := context.WithTimeout(r.Context(), planInspectionBudget)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(planInspectionBudget + 5*time.Second))
	inspect := s.planInspector
	if inspect == nil {
		inspect = func(ctx context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
			s.agents.mu.Lock()
			agent := s.agents.conns[ep.ID]
			s.agents.mu.Unlock()
			if agent == nil {
				return protocol.ContainerInspection{}, store.ErrEndpointOffline
			}
			return s.inspect(ctx, agent, a.ActorID, a.OrganizationID, target, func() bool { return s.inspectionAllowed(r.WithContext(ctx), a, ep.ID) })
		}
	}
	for _, svc := range pre.Services {
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

`internal/api/application_handlers.go`, `handlePlanDeployment`, after `input.MaxFrameBytes = maxFrameBytes(ep.Capabilities)`:

```go
	input.Inspections = s.planInspections(w, r, a, ep, pre)
	if len(input.Update) > 0 {
		extendRegistryDeadline(w) // the registry work starts after the inspections
	}
```

`internal/api/export_test.go` (add `"context"` and the `protocol` import):

```go
// SetPlanInspectorForTest replaces the agent round trip of each plan-time inspection. Test-only.
func SetPlanInspectorForTest(s *Server, f func(context.Context, protocol.InspectionTarget) (protocol.ContainerInspection, error)) {
	s.planInspector = f
}

// PlanInspectionBudgetForTest is the plan's inspection budget.
const PlanInspectionBudgetForTest = planInspectionBudget
```

- [ ] **Step 5: Run the API suite on both drivers**

Run: `go test -race -count=1 ./internal/api/ && KY_TEST_POSTGRES_DSN=postgres://postgres:postgrespassword@127.0.0.1:5432/ky_server?sslmode=disable go test -count=1 ./internal/api/`
Expected: PASS, the existing `TestInspectionAPI*` tests included (the route's statuses are unchanged).

- [ ] **Step 6: DOX and commit**

`internal/api/AGENTS.md`: in the inspection bullet replace "Strict result parsing/validation permits only redacted fields and configuration_verified=false." with "Strict result parsing/validation permits only redacted fields and a consistent `unsupported`/`configuration_verified` pair. The route and plan-time inspection share `inspect` (admission, grant, per-second recheck, validation); the route keeps its rate limit, capability check, target re-read and status mapping. A grant is cancelled on the agent only when the server gave up before an answer." In the plan bullet add "Before `PlanDeployment`, `planInspections` inspects each mapped service's container (`PreflightApplication`'s `inspection_target`s) in plan order, one at a time, under `planInspectionBudget` (10 s) and one `inspection:` attempt per plan, each grant expiring at the earlier of the budget and `InspectionLifetime`; nothing is requested without `container.inspect`; a failure of any kind leaves that container out of `PlanRequest.Inspections`. The response write deadline is extended by the budget, and an update plan extends it again for its registry work."

```bash
gofmt -l internal cmd
git add internal/api
git commit -m "feat(api): inspect each mapped container live when planning" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Store and API — refuse the plan on what the live inspection found

**Files:**
- Modify: `internal/store/application_deployment.go` (`PreflightBlockedError.Services`, `BlockedService`, `blocked`, `draft.services`, `draftPlan`, `inspectionBlockers`, both `PlanDeployment` paths)
- Modify: `internal/store/application_preflight.go:31-45` (`PreflightService.Unsupported`)
- Modify: `internal/api/tenant_handlers.go:40-43` (the 409 body names blocked services)
- Test: `internal/store/deployment_test.go` (`planRequest`, new test), `internal/store/recovery_test.go` (`verifiedPlan`), `internal/api/deployment_plan_test.go`
- Docs: `internal/store/AGENTS.md`, `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: `PlanRequest.Inspections` filled by the API (Task 8); `draft` (Task 7).
- Produces:
  ```go
  type BlockedService struct {
  	Name        string   `json:"name"`
  	Blockers    []string `json:"blockers"`
  	Unsupported []string `json:"unsupported,omitempty"`
  }
  type PreflightBlockedError struct {
  	Blockers []string
  	Services []BlockedService
  }
  func blocked(blockers []string, services ...BlockedService) error
  func inspectionBlockers(row *PreflightService, inspections map[string]protocol.ContainerInspection) []string
  // PreflightService.Unsupported []string `json:"unsupported,omitempty"`
  // 409 preflight_blocked body: {"error", "code", "blockers", "services": [{"name","blockers","unsupported"}]} (services only when non-empty)
  ```
  The 409 body keeps its minimal contract: service names, blocker codes and unsupported codes, never a reference, container or image identity.

- [ ] **Step 1: Give the store tests a verified inspection per container**

`internal/store/deployment_test.go`:

```go
// planRequest is what the API sends for m: the cap of an agent with deployment.pull and a
// verified live inspection of every adopted container.
func planRequest(m *ApplicationMapping) PlanRequest {
	r := PlanRequest{InstanceID: m.InstanceID, MappingVersion: m.Version, Revision: m.Preview.Revision, Confirm: m.Preview.Project, MaxFrameBytes: protocol.MaxDeploymentRequestBytes, Inspections: map[string]protocol.ContainerInspection{}}
	for _, c := range m.Preview.Containers {
		target := protocol.InspectionTarget{ContainerID: c.ID, ImageID: c.ImageID, CreatedUnix: c.CreatedAt.Unix()}
		r.Inspections[c.ID] = protocol.ContainerInspection{Target: target, ObservedAt: time.Now().UTC(), ConfigurationVerified: true, Unsupported: []string{}}
	}
	return r
}
```

`internal/store/recovery_test.go`:

```go
// verifiedPlan adds what the API supplies to a plan request: the frame cap of an agent with
// deployment.pull and a verified live inspection of every adopted container.
func verifiedPlan(r store.PlanRequest, containers []store.AdoptedContainer) store.PlanRequest {
	r.MaxFrameBytes = protocol.MaxDeploymentRequestBytes
	r.Inspections = map[string]protocol.ContainerInspection{}
	for _, c := range containers {
		target := protocol.InspectionTarget{ContainerID: c.ID, ImageID: c.ImageID, CreatedUnix: c.CreatedAt.Unix()}
		r.Inspections[c.ID] = protocol.ContainerInspection{Target: target, ObservedAt: time.Now().UTC(), ConfigurationVerified: true, Unsupported: []string{}}
	}
	return r
}
```

- [ ] **Step 2: Write the failing store test**

Append to `internal/store/deployment_test.go`:

```go
// Each mapped service needs a live inspection of the container the preflight names, and that
// inspection must find nothing a recreate would drop. The refusal names the service.
func TestPlanDeploymentChecksTheLiveInspection(t *testing.T) {
	st, a, app, endpoint, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	c := snapshot.Containers[0]
	target := protocol.InspectionTarget{ContainerID: c.ID, ImageID: c.ImageID, CreatedUnix: c.CreatedAt.Unix()}
	moved := target
	moved.CreatedUnix++
	for name, tc := range map[string]struct {
		inspections map[string]protocol.ContainerInspection
		want        string
		unsupported []string
	}{
		"none":             {map[string]protocol.ContainerInspection{}, "inspection_unavailable", nil},
		"identity changed": {map[string]protocol.ContainerInspection{c.ID: {Target: moved, ConfigurationVerified: true, Unsupported: []string{}}}, "replacement_identity_changed", nil},
		"unsupported":      {map[string]protocol.ContainerInspection{c.ID: {Target: target, Unsupported: []string{"privileged", "devices"}}}, "configuration_unsupported", []string{"privileged", "devices"}},
	} {
		r := planRequest(m)
		r.Inspections = tc.inspections
		_, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, imageCheckKey, false)
		var blocked *PreflightBlockedError
		if !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{tc.want}) || len(blocked.Services) != 1 {
			t.Fatalf("%s: %v", name, err)
		}
		if s := blocked.Services[0]; s.Name != "web" || !slices.Equal(s.Blockers, []string{tc.want}) || !slices.Equal(s.Unsupported, tc.unsupported) {
			t.Fatalf("%s: service %+v", name, s)
		}
	}
	if n := deploymentRows(t, st, app.ID); n != 0 {
		t.Fatalf("a refused plan left %d rows", n)
	}
	// Without container.inspect there was nothing to ask: only the capability is named.
	if err := ts.SetEndpointCapabilities(ctx, endpoint, []string{protocol.CapabilityDeploymentApply}); err != nil {
		t.Fatal(err)
	}
	r := planRequest(m)
	r.Inspections = nil
	var blocked *PreflightBlockedError
	if _, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, imageCheckKey, false); !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{"agent_inspect_unsupported"}) || len(blocked.Services) != 0 {
		t.Fatalf("no capability: %v", err)
	}
	if err := ts.SetEndpointCapabilities(ctx, endpoint, []string{protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test -count=1 ./internal/store/ -run TestPlanDeploymentChecksTheLiveInspection`
Expected: FAIL to compile: `blocked.Services undefined`.

- [ ] **Step 4: Implement the store**

`internal/store/application_preflight.go`, `PreflightService` gains:

```go
	// Unsupported are the codes a plan-time live inspection reported (configuration_unsupported).
	Unsupported []string `json:"unsupported,omitempty"`
```

`internal/store/application_deployment.go`:

```go
// PreflightBlockedError names the findings that stopped a plan. A plan never guesses past them.
type PreflightBlockedError struct {
	Blockers []string
	// Services are the planned services that carry blockers of their own.
	Services []BlockedService
}

// BlockedService names a service a refused plan blocked on: its own blockers and the codes a
// live inspection reported, nothing else about it.
type BlockedService struct {
	Name        string   `json:"name"`
	Blockers    []string `json:"blockers"`
	Unsupported []string `json:"unsupported,omitempty"`
}

func blocked(blockers []string, services ...BlockedService) error {
	if len(blockers) == 0 {
		return nil
	}
	slices.Sort(blockers)
	return &PreflightBlockedError{Blockers: slices.Compact(blockers), Services: services}
}

// inspectionBlockers checks a mapped service against the live inspection the API made of its
// container at plan time, recording on row the codes of what a recreate would drop.
func inspectionBlockers(row *PreflightService, inspections map[string]protocol.ContainerInspection) []string {
	in, ok := inspections[row.ContainerID]
	switch {
	case !ok:
		return []string{"inspection_unavailable"}
	case in.Target != *row.InspectionTarget:
		return []string{"replacement_identity_changed"}
	case !in.ConfigurationVerified:
		row.Unsupported = in.Unsupported
		return []string{"configuration_unsupported"}
	}
	return nil
}
```

`draft` gains `services []BlockedService // the services with blockers of their own, for the refusal`. In `draftPlan`, before the service loop:

```go
	inspected := capabilities[protocol.CapabilityContainerInspect]
	var refused []BlockedService
```

and the loop opens with:

```go
	for i, s := range spec.Services {
		row := p.Services[i]
		// Without container.inspect there was nothing to ask; agent_inspect_unsupported says why.
		if inspected && row.InspectionTarget != nil {
			row.Blockers = append(row.Blockers, inspectionBlockers(&row, r.Inspections)...)
		}
		blockers = append(blockers, row.Blockers...)
		if len(row.Blockers) > 0 {
			refused = append(refused, BlockedService{Name: row.Name, Blockers: row.Blockers, Unsupported: row.Unsupported})
		}
```

(the rest of the loop is unchanged), returning `&draft{d: d, m: m, blockers: blockers, capabilities: capabilities, services: refused}`.

In `PlanDeployment`, the no-update path refuses with `blocked(dr.blockers, dr.services...)`; in the update path's read closure, keep `services := dr.services` beside `d, m, blockers` and refuse with `blocked(blockers, services...)`.

- [ ] **Step 5: Run the store suite on both drivers**

Run: `go test -race -count=1 ./internal/store/ && KY_TEST_POSTGRES_DSN=postgres://postgres:postgrespassword@127.0.0.1:5432/ky_server?sslmode=disable go test -count=1 ./internal/store/`
Expected: PASS.

- [ ] **Step 6: Write the failing API tests**

Append to `internal/api/deployment_plan_test.go` (add `"errors"` to its imports):

```go
// planBlocked is a 409 plan refusal as the API writes it.
type planBlocked struct {
	Code     string
	Blockers []string
	Services []struct {
		Name        string
		Blockers    []string
		Unsupported []string
	}
}

// refused posts the plan, expects 409 and checks the body names no container.
func (h planHost) refused(t *testing.T) planBlocked {
	t.Helper()
	body := h.do(t, "POST", h.deployments, h.planBody, 409)
	var b planBlocked
	if err := json.Unmarshal([]byte(body), &b); err != nil || b.Code != "preflight_blocked" {
		t.Fatalf("refusal: %s", body)
	}
	for _, target := range h.targets {
		if strings.Contains(body, target.ContainerID) || strings.Contains(body, target.ImageID) {
			t.Fatalf("the refusal named a container: %s", body)
		}
	}
	return b
}

// online connects the host's agent over a real socket advertising capabilities.
func (h planHost) online(t *testing.T, capabilities []string) (*agentSocket, context.Context) {
	t.Helper()
	srv := httptest.NewServer(h.s)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	sock, reason := connect(t, ctx, srv.URL, h.ag, h.ag.priv, protocol.Version)
	if sock == nil {
		t.Fatalf("connect refused: %s", reason)
	}
	t.Cleanup(func() { sock.conn.CloseNow() })
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeHeartbeat {
		t.Fatalf("expected a heartbeat, got %s", f.Type)
	}
	return sock, ctx
}

// grant reads the next frame as an inspection grant.
func grant(t *testing.T, ctx context.Context, sock *agentSocket) protocol.InspectionOpen {
	t.Helper()
	frame := readEnvelope(t, ctx, sock.conn)
	var g protocol.InspectionOpen
	if frame.Type != protocol.TypeInspectionOpen || json.Unmarshal(frame.Payload, &g) != nil {
		t.Fatalf("expected an inspection grant, got %s", frame.Type)
	}
	return g
}

// What the live inspection reports refuses the plan, and the refusal names the service.
func TestPlanRefusesOnTheLiveInspection(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	for name, tc := range map[string]struct {
		inspect     func(context.Context, protocol.InspectionTarget) (protocol.ContainerInspection, error)
		want        string
		unsupported []string
	}{
		"no answer": {func(context.Context, protocol.InspectionTarget) (protocol.ContainerInspection, error) {
			return protocol.ContainerInspection{}, errors.New("unavailable")
		}, "inspection_unavailable", nil},
		"unsupported": {func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
			in := verifiedObservation(target)
			in.ConfigurationVerified, in.Unsupported = false, []string{"privileged", "devices"}
			return in, nil
		}, "configuration_unsupported", []string{"privileged", "devices"}},
		"another container": {func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
			target.CreatedUnix++
			return verifiedObservation(target), nil
		}, "replacement_identity_changed", nil},
	} {
		api.SetPlanInspectorForTest(h.s, tc.inspect)
		b := h.refused(t)
		if !slices.Equal(b.Blockers, []string{tc.want}) || len(b.Services) != 1 || b.Services[0].Name != "web" || !slices.Equal(b.Services[0].Unsupported, tc.unsupported) {
			t.Fatalf("%s: %+v", name, b)
		}
	}
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.do(t, "POST", h.deployments, h.planBody, 201)
}

// With no agent connected the plan is refused as uninspected, never planned blind.
func TestPlanRefusesWhenTheAgentIsOffline(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	if b := h.refused(t); !slices.Equal(b.Blockers, []string{"inspection_unavailable"}) || len(b.Services) != 1 || b.Services[0].Name != "web" {
		t.Fatalf("offline: %+v", b)
	}
}

// An agent that never answers holds the plan no longer than the budget: the plan is refused and
// the abandoned grant is cancelled on the agent (Review Focus 2).
func TestPlanRefusesWhenTheAgentNeverAnswers(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	sock, ctx := h.online(t, inspecting)
	start := time.Now()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- tenantRequest(h.s, h.admin, "POST", h.deployments, h.planBody, true) }()
	g := grant(t, ctx, sock)
	w := <-response
	if elapsed := time.Since(start); w.Code != 409 || !strings.Contains(w.Body.String(), "inspection_unavailable") || elapsed > api.PlanInspectionBudgetForTest+3*time.Second {
		t.Fatalf("after %v: %d %s", elapsed, w.Code, w.Body.String())
	}
	frame := readEnvelope(t, ctx, sock.conn)
	var stopped protocol.InspectionCancel
	if frame.Type != protocol.TypeInspectionCancel || json.Unmarshal(frame.Payload, &stopped) != nil || stopped.Request != g.Request {
		t.Fatalf("expected the grant's cancel, got %s", frame.Type)
	}
}

// An agent built before verdicts answers with configuration_verified false and no unsupported
// list. The server refuses that answer, so the plan is uninspected, never verified
// (Review Focus 5).
func TestPlanRefusesAnOlderAgentsInspection(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	sock, ctx := h.online(t, inspecting)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- tenantRequest(h.s, h.admin, "POST", h.deployments, h.planBody, true) }()
	g := grant(t, ctx, sock)
	raw, _ := json.Marshal(verifiedObservation(g.Target))
	var older map[string]any
	if err := json.Unmarshal(raw, &older); err != nil {
		t.Fatal(err)
	}
	delete(older, "unsupported")
	older["configuration_verified"] = false
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInspectionResult, map[string]any{"request": g.Request, "status": "ok", "result": older})
	if w := <-response; w.Code != 409 || !strings.Contains(w.Body.String(), "inspection_unavailable") {
		t.Fatalf("an older agent's answer: %d %s", w.Code, w.Body.String())
	}
}
```

In `TestPlanCountsOneInspectionAttempt` the over-limit plan is now refused; replace its second `h.do(t, "POST", h.deployments, h.planBody, 201)` with:

```go
	if b := h.refused(t); !slices.Equal(b.Blockers, []string{"inspection_unavailable"}) || len(b.Services) != 2 {
		t.Fatalf("over the inspection limit: %+v", b)
	}
```

- [ ] **Step 7: Run to verify they fail**

Run: `go test -count=1 ./internal/api/ -run 'TestPlan'`
Expected: FAIL: the 409 bodies carry no `services` (`TestPlanRefusesOnTheLiveInspection`: `len(b.Services) != 1`).

- [ ] **Step 8: Implement the API body**

`internal/api/tenant_handlers.go`:

```go
	var blocked *store.PreflightBlockedError
	if errors.As(err, &blocked) {
		body := map[string]any{"error": "Deployment preflight reported blockers", "code": "preflight_blocked", "blockers": blocked.Blockers}
		if len(blocked.Services) > 0 {
			body["services"] = blocked.Services
		}
		s.writeJSON(w, http.StatusConflict, body)
		return
	}
```

- [ ] **Step 9: Run the API and store suites on both drivers**

Run: `go test -race -count=1 ./internal/api/ ./internal/store/ && KY_TEST_POSTGRES_DSN=postgres://postgres:postgrespassword@127.0.0.1:5432/ky_server?sslmode=disable go test -count=1 ./internal/api/ ./internal/store/`
Expected: PASS (`TestPlanRefusesWhenTheAgentNeverAnswers` takes about 10 s). With Docker available also `KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 go test -race -count=1 ./internal/api -run '^TestApplyRealDocker$'`: the real agent answers the plan's inspection with a verified verdict and the apply succeeds.

- [ ] **Step 10: DOX and commit**

`internal/store/AGENTS.md`, `PlanDeployment` bullet: "With `container.inspect`, every mapped service (one with an `InspectionTarget`) needs `PlanRequest.Inspections[containerID]` (`json:\"-\"`, set by the API): none is `inspection_unavailable`, a `Target` other than the preflight's is `replacement_identity_changed`, `ConfigurationVerified` false is `configuration_unsupported` with the codes on `PreflightService.Unsupported`. `PreflightBlockedError.Services` lists each service with blockers of its own as `BlockedService{name, blockers, unsupported}`. The inspections are consumed and never stored."

`internal/api/AGENTS.md`, plan bullet: replace "any other blocker is 409 `preflight_blocked` with the blocker list only, no reference or container identity beyond it" with "any other blocker is 409 `preflight_blocked` with the blocker list and, when a service has blockers of its own, `services: [{name, blockers, unsupported}]`: service names and codes only, no reference or container identity".

```bash
gofmt -l internal cmd
git add internal/store internal/api
git commit -m "feat(store): refuse a plan on what the live inspection found" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: Web — blocker texts, service findings, the recheck step, the inspection verdict

**Files:**
- Modify: `web/src/components/ApplicationInspection.tsx` (`unsupportedNames`, parse and render the verdict)
- Modify: `web/src/components/ApplicationPreflight.tsx` (blocker union and texts, `serviceFindings`)
- Modify: `web/src/components/ApplicationDeploymentPlan.tsx` (409 service findings, detail prefixes, recheck and clock-skew explanations)
- Test: `web/src/components/ApplicationPreflight.test.tsx`, `web/src/components/ApplicationDeploymentPlan.test.tsx`
- Build: `web/dist` via `make build-web`
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes: the 409 body `services: [{name, blockers, unsupported}]` (Task 9); inspection fields `unsupported`, `configuration_verified` (Tasks 1, 3); step `recheck` (Task 4); details `clock skew exceeds 5 minutes` (Task 1), `the agent restarted after replacement began; inspect the host` (Task 5), `unsupported: …` (Task 2), `the container changed after the precondition` (Task 4).
- Produces:
  ```ts
  export const unsupportedNames: Record<string, string>                 // ApplicationInspection.tsx
  export function serviceFindings(payload: unknown): string[]            // ApplicationPreflight.tsx
  ```

- [ ] **Step 1: Write the failing tests**

`web/src/components/ApplicationPreflight.test.tsx`: import the tables,

```ts
import { ApplicationPreflight, messages } from './ApplicationPreflight';
import { unsupportedNames } from './ApplicationInspection';
```

give the `inspection` fixture a verdict (`configuration_verified: true, unsupported: []` in place of `configuration_verified: false`), and in the `it.each` of malformed observations replace `{ ...inspection, configuration_verified: true }` with

```ts
  { ...inspection, configuration_verified: false },
  { ...inspection, configuration_verified: false, unsupported: ['secret-canary'] },
  { ...inspection, unsupported: undefined },
```

Append:

```ts
it('explains clock skew in fixed text', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ ...data, blockers: ['clock_skew'] }))));
  render(<ApplicationPreflight org="org" base="/app" instanceID="i" />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment preflight' }));
  expect(await screen.findByText(messages.clock_skew)).toBeTruthy();
});
it('shows the configuration verdict by name, never by code', async () => {
  modal();
  vi.stubGlobal('fetch', vi.fn(async (url: string) => new Response(JSON.stringify(url.endsWith('/preflight') ? inspectable : inspection))));
  await openInspection();
  expect(await screen.findByText('Configuration: fully expressible')).toBeTruthy();
  cleanup();
  vi.stubGlobal('fetch', vi.fn(async (url: string) => new Response(JSON.stringify(url.endsWith('/preflight') ? inspectable : { ...inspection, configuration_verified: false, unsupported: ['privileged', 'image_config'] }))));
  await openInspection();
  expect(await screen.findByText('runs privileged')).toBeTruthy();
  expect(screen.getByText(unsupportedNames.image_config!)).toBeTruthy();
  expect(screen.queryByText('Configuration: fully expressible')).toBeNull();
  expect(document.body.textContent).not.toContain('image_config');
});
```

`web/src/components/ApplicationDeploymentPlan.test.tsx`: import `messages` (`import { messages } from './ApplicationPreflight';`) and append:

```ts
it('names the services a live inspection refused and hides anything unrecognized', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    if (init?.method === 'POST') return new Response(JSON.stringify({ code: 'preflight_blocked', blockers: ['configuration_unsupported', 'inspection_unavailable'], services: [
      { name: 'web', blockers: ['configuration_unsupported'], unsupported: ['privileged', 'devices', 'secret-canary'] },
      { name: 'db', blockers: ['inspection_unavailable'] },
      { name: 'Bad Name', blockers: ['configuration_unsupported'], unsupported: ['privileged'] },
    ] }), { status: 409 });
    return new Response('[]');
  }));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  fireEvent.change(screen.getByLabelText('Confirm plan project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan deployment' }));
  await screen.findByRole('alert');
  expect(screen.getByText(messages.configuration_unsupported)).toBeTruthy();
  expect(screen.getByText(messages.inspection_unavailable)).toBeTruthy();
  expect(screen.getByText('web: runs privileged, maps host devices')).toBeTruthy();
  expect(screen.getByText('db: no live inspection answered')).toBeTruthy();
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(document.body.textContent).not.toContain('Bad Name');
});
it('explains a recheck denial like a precondition', async () => {
  const drifted = { ...plan, state: 'denied', detail: 'service web, step recheck: the container changed after the precondition', result: { steps: [
    { service: 'web', step: 'precondition', outcome: 'succeeded', detail: '' },
    { service: 'web', step: 'image', outcome: 'succeeded', detail: '' },
    { service: 'web', step: 'recheck', outcome: 'denied', detail: 'the container changed after the precondition' },
    { service: 'web', step: 'rename', outcome: 'skipped', detail: '' },
  ], services: [] } };
  vi.stubGlobal('fetch', stubFetch([drifted]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(/changed on the host while the deployment prepared/);
  expect(screen.getByText('the container changed after the precondition')).toBeTruthy();
});
it('explains a deployment refused for clock skew', async () => {
  vi.stubGlobal('fetch', stubFetch([{ ...plan, state: 'failed', detail: 'clock skew exceeds 5 minutes', result: { steps: [], services: [] } }]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The host clock differs from the server by more than five minutes; nothing ran. Correct the host clock, then plan again.');
  expect(screen.getByText('clock skew exceeds 5 minutes')).toBeTruthy();
});
it('shows the restart detail with the inspect-the-host guidance', async () => {
  vi.stubGlobal('fetch', stubFetch([{ ...plan, state: 'unknown', detail: 'the agent restarted after replacement began; inspect the host', result: { steps: [], services: [] } }]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The host may or may not have acted. Inspect it before planning again.');
  expect(screen.getByText('the agent restarted after replacement began; inspect the host')).toBeTruthy();
});
it('shows an unsupported-configuration step detail', async () => {
  const refusedCodes = { ...plan, state: 'denied', detail: '', result: { steps: [{ service: 'web', step: 'precondition', outcome: 'denied', detail: 'unsupported: privileged, devices' }], services: [] } };
  vi.stubGlobal('fetch', stubFetch([refusedCodes]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('unsupported: privileged, devices');
  expect(screen.getByText(/configuration the definition does not describe/i)).toBeTruthy();
});
```

- [ ] **Step 2: Run to verify they fail**

Run: `(cd web && npx vitest run src/components/ApplicationPreflight.test.tsx src/components/ApplicationDeploymentPlan.test.tsx)`
Expected: FAIL: `messages.clock_skew` undefined, no `unsupportedNames` export, no verdict text, no service findings.

- [ ] **Step 3: Implement the inspection verdict**

`web/src/components/ApplicationInspection.tsx`, below `InspectionTarget`:

```ts
// Human names of the agent's unsupported-configuration codes (protocol.UnsupportedCodes).
// A code missing here is refused, never shown raw.
export const unsupportedNames: Record<string, string> = {
  mount_type: 'has mounts other than volumes and binds',
  anonymous_volume: 'uses anonymous volumes',
  volumes_from: 'mounts volumes from another container',
  volume_driver: 'uses a volume driver',
  mount_options: 'sets mount options',
  tmpfs: 'mounts tmpfs',
  auto_remove: 'removes itself when stopped',
  read_only_rootfs: 'has a read-only root filesystem',
  privileged: 'runs privileged',
  capabilities: 'adds or drops capabilities',
  security_opt: 'sets security options',
  devices: 'maps host devices',
  pid_mode: 'shares a PID namespace',
  ipc_mode: 'shares an IPC namespace',
  user: 'runs as a set user',
  runtime: 'uses a non-default runtime',
  resource_limits: 'has memory, CPU or process limits',
  ulimits: 'sets ulimits',
  sysctls: 'sets sysctls',
  device_requests: 'requests GPUs or other devices',
  init: 'runs an init process',
  userns_mode: 'sets a user namespace mode',
  cgroup_parent: 'sets a cgroup parent',
  group_add: 'adds supplementary groups',
  extra_hosts: 'adds host entries',
  dns: 'sets DNS options',
  links: 'uses container links',
  network: 'is on a network other than its project network',
  image_config: "overrides its image's command, entrypoint, healthcheck, working directory or stop signal",
};
```

`Inspection` gains `configuration_verified: boolean; unsupported: string[]`. In `parseInspection`, drop `|| value.configuration_verified !== false` from the first condition, add `configuration_verified, unsupported` to the destructured fields, and before the ports loop:

```ts
  if (typeof configuration_verified !== 'boolean' || !Array.isArray(unsupported) || unsupported.length > 32 || new Set(unsupported).size !== unsupported.length
    || !unsupported.every((c): c is string => typeof c === 'string' && Object.hasOwn(unsupportedNames, c)) || configuration_verified !== (unsupported.length === 0)) return null;
```

and return `configuration_verified, unsupported: [...unsupported]` in the parsed object. Replace the dialog's opening paragraph with `<p>Read-only observation of the mapped container. It says whether a recreate from the definition could keep the container's configuration; it is not a replacement specification and approves nothing.</p>` and add to `InspectionFacts`, before the `<dl>`:

```tsx
    {data.configuration_verified
      ? <p>Configuration: fully expressible</p>
      : <><p>Configuration the definition cannot express, which a recreate would drop:</p><ul className="ky-list">{data.unsupported.map(c => <li key={c}>{unsupportedNames[c]}</li>)}</ul></>}
```

- [ ] **Step 4: Implement the blocker texts and service findings**

`web/src/components/ApplicationPreflight.tsx`: import `unsupportedNames` beside `ApplicationInspection`,

```ts
import { ApplicationInspection, unsupportedNames, type InspectionTarget } from './ApplicationInspection';
```

extend the `Blocker` union with `| 'clock_skew' | 'inspection_unavailable' | 'replacement_identity_changed' | 'configuration_unsupported' | 'frame_too_large' | 'too_many_registry_hosts' | 'frame_invalid' | 'agent_deploy_unsupported' | 'agent_pull_unsupported' | 'agent_inspect_unsupported'` and add to `messages`:

```ts
  clock_skew: "The host's clock differs from the server's by more than five minutes. Correct the host clock (NTP), then check again.",
  inspection_unavailable: 'The live container could not be inspected before planning. Check that the host agent is connected, then plan again.',
  replacement_identity_changed: 'The live container is not the one in the stored inventory. Refresh the host inventory and plan again.',
  configuration_unsupported: 'The live container has configuration the definition cannot express, which a recreate would drop. Change it on the host, or recreate the container from the definition by hand, then plan again.',
  frame_too_large: 'This deployment is larger than the host agent accepts. Upgrade the agent, or reduce the environment values, then plan again.',
  too_many_registry_hosts: 'This deployment pulls with credentials from more than 16 registries. Pull from fewer, then plan again.',
  frame_invalid: 'This deployment could not be built from the saved definition and values. Review the definition, then plan again.',
  agent_deploy_unsupported: 'Upgrade the host agent to enable deployments.',
  agent_pull_unsupported: 'Upgrade the host agent to enable deployments that pull images.',
  agent_inspect_unsupported: 'Upgrade the host agent to enable live inspection, which planning requires.',
```

and below `knownBlockers`:

```ts
// serviceFindings reads the services a 409 preflight_blocked body names and states, in fixed
// text, what a live inspection found for each: "web: runs privileged, maps host devices".
// Names outside the service grammar and unknown codes are dropped.
export function serviceFindings(payload: unknown): string[] {
  const services = payload && typeof payload === 'object' ? (payload as { services?: unknown }).services : undefined;
  if (!Array.isArray(services)) return [];
  const lines: string[] = [];
  for (const s of services) {
    if (!s || typeof s !== 'object') continue;
    const { name, blockers, unsupported } = s as { name?: unknown; blockers?: unknown; unsupported?: unknown };
    if (typeof name !== 'string' || !/^[a-z0-9][a-z0-9_-]{0,62}$/.test(name)) continue;
    const codes = Array.isArray(unsupported) ? unsupported.filter((c): c is string => typeof c === 'string' && Object.hasOwn(unsupportedNames, c)) : [];
    if (codes.length) lines.push(`${name}: ${codes.map(c => unsupportedNames[c]).join(', ')}`);
    else if (Array.isArray(blockers) && blockers.includes('inspection_unavailable')) lines.push(`${name}: no live inspection answered`);
  }
  return lines;
}
```

- [ ] **Step 5: Implement the plan panel**

`web/src/components/ApplicationDeploymentPlan.tsx`: import `serviceFindings` beside `knownBlockers`; add `'unsupported: ', 'clock skew', 'the agent restarted'` to `FIXED_DETAIL_PREFIXES`; in `preconditionExplanation` add first:

```ts
  if (detail.includes('changed after the precondition')) return 'The mapped container changed on the host while the deployment prepared; it and every later service were left untouched, and services before it were replaced. Review the host, then plan again.';
```

in `explanationFor`, after the `failed` with no result line:

```ts
  if (current.state === 'failed' && current.detail.startsWith('clock skew')) return 'The host clock differs from the server by more than five minutes; nothing ran. Correct the host clock, then plan again.';
```

and let the recheck share the precondition branch:

```ts
  if (failing.outcome === 'denied' && (failing.step === 'precondition' || failing.step === 'recheck')) return current.kind === 'remove'
```

In `plan`, the 409 branch becomes:

```ts
      if (r.status === 409) {
        const payload: unknown = await r.json().catch(() => null);
        const lines = [...knownBlockers(payload, messages).map(b => messages[b]), ...serviceFindings(payload)];
        setError(lines.length ? lines : ['Ownership, mapping or the definition changed. Refresh applications and review before planning again.']);
        return;
      }
```

- [ ] **Step 6: Run the web suite and rebuild**

Run: `(cd web && npx vitest run) && make build-web && git status --short web/dist`
Expected: every vitest file passes; `web/dist` shows the rebuilt bundle (old hashed assets deleted, new ones added).

- [ ] **Step 7: DOX and commit**

`web/AGENTS.md`: preflight bullet: "`clock_skew` and the plan-time blockers (`inspection_unavailable`, `replacement_identity_changed`, `configuration_unsupported`, `frame_too_large`, `too_many_registry_hosts`, `frame_invalid`, `agent_deploy_unsupported`, `agent_pull_unsupported`, `agent_inspect_unsupported`) are fixed texts in `messages`. The live inspection dialog shows \"Configuration: fully expressible\" or the unsupported configuration by human name from `unsupportedNames` (`ApplicationInspection.tsx`); a code outside that table, a disagreeing `configuration_verified` or a missing `unsupported` list refuses the whole observation." Plan bullet: "A 409 also lists `serviceFindings`: per named service (service grammar only) its unsupported configuration by human name, or \"no live inspection answered\". A `recheck` denial is explained like a precondition keyed on its detail (`the container changed after the precondition`); a `failed` row with detail `clock skew exceeds 5 minutes` explains the host clock; `unsupported: `, `clock skew` and `the agent restarted` are recognized detail prefixes."

```bash
git add web
git commit -m "feat(web): plan-time blockers, inspection verdict and recheck step" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 11: Documents and the DOX pass

**Files:**
- Modify: `docs/agent-protocol.md` (§6 inventory skew; Container inspection; Deployment apply; Deployment remove)
- Modify: `docs/threat-model.md` (two new rows; the registry credential residual)
- Modify: `docs/ACCEPTANCE.md` (the plan needs an online agent with `container.inspect`; clocks)
- Modify: `docs/application-schema.md` (Runtime inspection foundation; Deployment plans) — not named by the spec, but its "`configuration_verified` is always false" and "executable planning does not consume these observations yet" become false with this PR, and DOX forbids leaving them
- Modify: `KyYard-Implementation-Plan.md` §8
- Verify: `internal/agent/AGENTS.md`, `internal/runtime/AGENTS.md`, `internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md` (edited in Tasks 1–10), root `AGENTS.md` (no change: no structure or child index changed)

**Interfaces:** none (documents only).

- [ ] **Step 1: `docs/agent-protocol.md`**

§6: replace "and flags clock skew over *proposed* 5 minutes." with "and flags clock skew over `MaxClockSkew` (5 minutes); a deployment preflight or plan on such an inventory is blocked with `clock_skew`."

Container inspection: replace "observation freshness (past 25 seconds/future five seconds) and configuration_verified=false." with "observation freshness (past 25 seconds/future five seconds) and a consistent verdict: `unsupported` lists codes from the closed vocabulary (`protocol.UnsupportedCodes`: `mount_type`, `anonymous_volume`, `volumes_from`, `volume_driver`, `mount_options`, `tmpfs`, `auto_remove`, `read_only_rootfs`, `privileged`, `capabilities`, `security_opt`, `devices`, `pid_mode`, `ipc_mode`, `user`, `runtime`, `resource_limits`, `ulimits`, `sysctls`, `device_requests`, `init`, `userns_mode`, `cgroup_parent`, `group_add`, `extra_hosts`, `dns`, `links`, `network`, `image_config`; at most 32, none repeated) and `configuration_verified` is true exactly when it is empty. The Docker adapter derives it from the same reads as the deployment precondition, with the project network from the container's `com.docker.compose.project` label and the daemon's default runtime (`/info`, cached a minute); codes only, never a value. An agent older than this rule reports no `unsupported` and is refused." and "No result is stored, used as a deploy approval, or retried automatically." with "No result is stored or retried automatically. A deployment plan inspects each mapped container this way, sequentially under a 10-second budget and one attempt per plan, and is refused on what it finds; the observation approves nothing by itself."

Deployment apply: after the paragraph on the ledger, add:

> Every frame carries `issued_at`, the server's clock when it built the frame. `Validate` requires it within `MaxClockSkew` (5 minutes) of the reader's clock and the deadline within `(issued_at, issued_at + 15 minutes]`, besides the checks against the reader's now. A frame without it is invalid: an agent with this rule refuses a frame from an older server. A skewed frame is answered `failed`, detail `clock skew exceeds 5 minutes`, with no step run and nothing recorded.
>
> Phase two opens each service with `recheck`: a fresh inspect of the old container, `denied` `the container changed after the precondition` when its identity or its `Config`, `HostConfig` or `Mounts` differ from the precondition's read (`State` and `NetworkSettings` are not compared, so a restart is not drift). That service is untouched and every later one skipped; services already replaced stay replaced. The runtime calls the deployer's started hook once, immediately before the first phase-two call (after the first recheck's deadline guard; for a removal, before the first stop). The deployer writes `{started}` for the deployment to the ledger durably before the run continues. After an agent restart, an entry with `started` and no result becomes `unknown`, `the agent restarted after replacement began; inspect the host`, and is re-sent like any result; a re-sent frame for it is answered from the ledger, never run again; pruning never drops it. A deployment whose result is `unknown` needs an operator's inspection of the host before another plan for that application; `SettleDeployment` records the outcome.
>
> The server builds the frame at plan time exactly as at apply, with the real values and credentials, measures it against the endpoint's cap and drops it (`frame_too_large`, `too_many_registry_hosts`, `frame_invalid`); it refuses a plan the agent could not run (`agent_deploy_unsupported`, `agent_pull_unsupported`, `agent_inspect_unsupported`). Apply keeps its own 501s and frame check: capabilities can change between plan and apply.

In the same section, the step sequences gain `recheck`: "A pulled service's steps are precondition, pull, recheck, rename, create, stop, start, remove.", "a service's steps are then precondition, volume (zero or more), image or pull, recheck, rename, create, stop, start, remove. The result's step bound (`MaxDeploymentResultSteps`: eight per service plus one per volume) holds them.", and "then per service recheck, rename, create, stop, start, remove."

Deployment remove: after "deadline (at most `DeploymentLifetime`, 15 minutes)" add ", `issued_at` under the same skew rule as an apply frame".

- [ ] **Step 2: `docs/threat-model.md`**

Add after the "Host path exposure through a revision" row:

```markdown
| A container reconfigured between plan and replacement | Implemented (PR D1): the plan inspects each mapped container live and is refused on configuration a recreate would drop (`configuration_unsupported`) or another container (`replacement_identity_changed`); the agent's precondition re-reads it before any pull, and `recheck` re-reads it immediately before its rename, denying a changed identity, `Config`, `HostConfig` or `Mounts` (a `docker update`, a recreate) with that service untouched. Residual: a change in the milliseconds between the recheck and the rename is not caught; services replaced before a denied recheck are not rolled back | `TestRecheckDeniesDriftBetweenThePhases`, `TestRecheckRealDocker`, `TestPlanRefusesOnTheLiveInspection` |
| Host and server clocks disagree | Implemented (PR D1): every deployment and removal frame carries `issued_at`; an agent more than 5 minutes off refuses it (`failed`, `clock skew exceeds 5 minutes`) before touching anything, and preflight and plan refuse an inventory whose agent clock is more than 5 minutes from the server's (`clock_skew`) | `TestDeploymentRequestIssuedAt`, `TestDeployReportsClockSkewWithoutCalling`, `TestPreflightBlocksClockSkew` |
```

In the "Registry credential leakage" row replace "and a planned frame with more than 16 credentialed hosts or over 320 KiB is refused only at apply" with "credentials are decrypted in memory at plan (to measure the frame) and at apply, and never stored in a plan; a frame with more than 16 credentialed hosts or past the agent's cap is refused at plan and again at apply".

- [ ] **Step 3: `docs/ACCEPTANCE.md`**

In the "Two disposable Docker hosts" paragraph, after the sentence about older agents, add: "Planning inspects each mapped container live through its host's agent, so the agent must be online when you plan: an offline agent gives \"The live container could not be inspected before planning…\", and an agent without live inspection \"Upgrade the host agent to enable live inspection, which planning requires.\" Keep each host's clock within five minutes of the control plane's (NTP); otherwise preflight shows the clock blocker and no plan is made."

- [ ] **Step 4: `docs/application-schema.md`**

Runtime inspection foundation, first paragraph: replace "executable planning does not consume these observations yet" with "a deployment plan inspects every mapped container through the same API and is refused on what it finds (Deployment plans)". Replace the last paragraph's first sentence "`configuration_verified` is always false." with "`unsupported` names, as codes only, the configuration a recreate from the definition would drop, and `configuration_verified` is true exactly when it is empty (agent-protocol.md, Container inspection)." and delete its final two sentences ("The agent transport enforces … The existing deployment preflight remains non-executable.").

Deployment plans, first paragraph, after "any blocker refuses with `409 preflight_blocked` naming the blockers": add " and, for a service with blockers of its own, the service by name. Beyond the preflight, a plan requires the endpoint's agent to advertise `deployment.apply` and `container.inspect` (and `deployment.pull` for a plan that pulls): `agent_deploy_unsupported`, `agent_inspect_unsupported`, `agent_pull_unsupported`; a live inspection of each mapped container, gathered by the API one at a time within 10 seconds, finding the preflight's container (`inspection_unavailable`, `replacement_identity_changed`) with nothing a recreate would drop (`configuration_unsupported`, naming the codes); an inventory whose agent clock is within 5 minutes of the server's (`clock_skew`); and the frame apply would send, built with the real values and credentials, within the agent's cap (`frame_too_large`, `too_many_registry_hosts`, `frame_invalid`); the frame is dropped".

- [ ] **Step 5: `KyYard-Implementation-Plan.md` §8**

Before "M7a's manual update path …" is fine as is; after the "Implemented volumes in the application definition" paragraph add:

```markdown
Implemented M7a PR D1 (`feat/deploy-safety`): the agent re-checks each container immediately before its replacement (`recheck`: identity, `Config`, `HostConfig`, `Mounts`); a plan inspects every mapped container live and is refused on configuration a recreate would drop, measures the real frame against the agent's cap, and requires the capabilities apply needs; frames carry `issued_at` and both sides refuse more than five minutes of clock skew; the agent ledger records a started marker before the first phase-two mutation, so a restart mid-replacement reports `unknown` instead of running the frame again.
```

and replace the "Next: M7a PR D (hardening)" block (the heading line and its eight bullets) with:

```markdown
Next: M7a PR D2 (hardening), carried from M6 and PR C review:
- Closed step-detail vocabulary (details are fixed text today, not a closed set).
- Audit correlation IDs linking plan, apply and settle.
- Put update plans under the per-application guard, so one user cannot fill all 4 registry slots with plans for one application.
```

- [ ] **Step 6: DOX closeout**

Walk the changed paths against the DOX chain: root `AGENTS.md` → `KyYard-Server/AGENTS.md` → `internal/agent/AGENTS.md`, `internal/runtime/AGENTS.md`, `internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md`. Confirm each child reflects its task's contract (issued_at/skew, codes, recheck, started marker, frame at plan, capability and inspection blockers, 409 `services`, web texts) and that none still says "configuration_verified remains false", "always false", "refused only at apply", "8×`MaxDeploymentServices`" or "3*callBudget":

```bash
grep -rn 'configuration_verified remains false\|configuration_verified=false\|always false\|refused only at apply\|8×`MaxDeploymentServices`\|3\*callBudget\|proposed\* 5 minutes' --include=AGENTS.md --include='*.md' . | grep -v '^./docs/superpowers/'
```

Expected: no output. `KyYard-Server/AGENTS.md` and the root `AGENTS.md` are unchanged (no child boundary, structure or index changed); say so in the PR description.

- [ ] **Step 7: Full gate**

Run: `gofmt -l internal cmd && make ci && make test-postgres`
Expected: `gofmt -l` prints nothing; `make ci` (tidy-check, lint, race tests, web tests, smoke) and the PostgreSQL suite pass. With Docker available also `KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 KY_TEST_DOCKER_INSPECTION_IMAGE=alpine:3.24 go test -race -count=1 ./internal/runtime/docker -run '^Test(Inspection|Deploy|Recheck|Remove)RealDocker$' && KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 go test -race -count=1 ./internal/api -run '^TestApplyRealDocker$'`.

- [ ] **Step 8: Commit**

```bash
git add docs KyYard-Implementation-Plan.md
git commit -m "docs: deploy safety in the protocol, threat model and acceptance runbook" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```
