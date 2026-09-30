# Container Edit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An organization administrator opens any Docker container's Configuration tab, sees its full configuration, edits it and saves; the container is recreated in place with a rollback if the new one fails to start. The same form runs a new container.

**Architecture:** A new `container.configure` permission (organization administrators, like exec) gates three things: a configuration read frame that the Docker agent answers from `ContainerInspect` with an allowlisted-but-complete field set, held only in the HTTP request; a recreate that reuses the deployment engine with one service marked `explicit` (the operator saw and kept every setting, so the "unsupported configuration" refusals are skipped and a `rollback` step is added); and a run that is the same frame without a container to replace. Both writes are `endpoint_commands` rows settled from the agent's `deployment.result`, so the Activity tab, audit and polling already work.

**Tech Stack:** Go 1.26 (`internal/permissions`, `internal/agent/protocol`, `internal/agent/client`, `internal/runtime/docker`, `internal/store`, `internal/api`), React 19 + TypeScript (`web/`), vitest.

**Spec:** `docs/superpowers/specs/2026-09-29-container-management-design.md`, section 2 (Edit Docker).

## Global Constraints

- Permission name is exactly `container.configure`; only `organization_admin` holds it. Service tokens are refused on every route it gates.
- The configuration read never persists values: no store row holds env values, no log line, no audit `details` value. Audit rows carry container ID, image ID and field names only.
- Every string in `ContainerConfiguration` and `ExplicitService` is bounded: env `MaxDeploymentEnvEntries` (128) / `MaxDeploymentEnvValueBytes` (16 KiB) / `MaxDeploymentEnvBytes` (64 KiB); ports `MaxDeploymentPorts` (64); mounts `MaxMounts` (32); labels `MaxLabels` (32) × `MaxLabelBytes` (256); new: `MaxArgv = 64` entries of 4 KiB, `MaxListEntries = 32` (cap_add, cap_drop, security_opt, devices, extra_hosts, dns, network aliases), `MaxNetworks = 16` attachments, `MaxLogOptions = 16`. The read result is delivered on the agent's control channel under `MaxConfigurationFrameBytes = 320 << 10` (equal to `MaxDeploymentRequestBytes`), within `ConfigurationLifetime = 20 * time.Second`.
- A truncated list is never silent: the agent records `env_truncated`, `labels_truncated`, `argv_truncated`, `list_truncated:<field>` in `unsupported`, and the server refuses a recreate whose read had a non-empty `unsupported` (422 `configuration_incomplete`).
- Recreate and run are `DeploymentRequest` frames with one service and `Explicit: true`; existing application frames (`Explicit` false, no `ExplicitService`) marshal byte-for-byte as before (golden test) and the agent's precondition behaviour for them is unchanged.
- A recreate never introduces a bind mount the old container did not already have unless the request lists the exact host path in `acknowledge_binds`; a run requires every bind path in `acknowledge_binds`. Each acknowledged path is audited.
- Compose-adopted containers (an `application_instances` row owns the container ID) are refused with 409 `application_managed` on the configuration read and on recreate.
- Rate: configuration read 12 per actor per minute (`allowAttempt("configure:"+principal, 12, time.Minute)`); one recreate or run in flight per endpoint (the agent's `busy` result plus a server check that no `container.recreate`/`container.run` command on the endpoint is unsettled).
- Confirmations: the recreate form requires the typed container name; the run form requires the typed new name. Fixed error texts; never render server text. Every write uses `secureFetch`/`tenantWrite`.
- `web/dist` is rebuilt and committed in the last task. Every DOX doc and design doc named in Task 12 is updated in the same PR.
- Commit messages end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. **A container with a setting the form cannot express** (a `volumes_from`, a device request, a runtime other than the default) must be shown as read-only with the setting named, and Save must be refused server-side, not silently dropped. Pinned in Task 3 (`unsupported` codes) and Task 8 (422 `configuration_incomplete`).
2. **Recreate whose new container exits immediately** (bad command) must leave the old container running under its original name and report a `rollback` step, never two dead containers. Pinned in Task 7 (`TestRecreateRollbackRealDocker`).
3. **An environment value with `=` or a newline** must round-trip unchanged through read, form and recreate. Pinned in Task 3 (adapter split on first `=`), Task 6 (`validEnv` allows newlines in values), Task 10 (form test).
4. **An operator who is demoted while the form is open** must have Save refused with 403 and the audit row written; the read they already hold is not revoked. Pinned in Task 8 (permission re-checked in the write transaction).
5. **A run that names an image not present on the host** must pull by digest through the registry rules (`image.pull` authority, credentials only from the Registries panel) or fail with a named code, never hang. Pinned in Task 8 (plan-time resolution) and Task 7 (`pull` step reuse).

---

### Task 1: Permission and web mirror

**Files:**
- Modify: `internal/permissions/permissions.go`, `internal/permissions/permissions_test.go`, `internal/permissions/AGENTS.md`, `web/src/tenant.ts`
- Test: `internal/permissions/permissions_test.go`, `web/src/tenant.test.ts` (create if absent)

**Interfaces:**
- Produces: `permissions.ContainerConfigure Action = "container.configure"`; `Allows(organization_admin, ContainerConfigure) == true`, every other role false; `web/src/tenant.ts` `export const canConfigure = (role: string | undefined) => role === 'organization_admin'` with the "Mirrors permissions.Allows(role, ContainerConfigure)" comment.

- [ ] **Step 1: Failing tests.** Add `TestConfigureIsOrganizationAdminOnly` beside `TestExecIsOrganizationAdminOnly`, same loop shape, asserting only `organization_admin` is allowed and that the action string is `container.configure`. Add `ContainerConfigure` to the action list `TestFixedRoleMatrix` enumerates (read that test to see how it pins the matrix) and to `TestPulseReaderIsReadOnly`. Web: `tenant.test.ts` asserts `canConfigure('organization_admin') === true` and false for `environment_admin`, `operator`, `developer`, `read_only`, `undefined`.
- [ ] **Step 2: Run** `go test ./internal/permissions` (FAIL: undefined) and `cd web && npx vitest run src/tenant.test.ts` (FAIL).
- [ ] **Step 3: Implement.** Add the constant next to `ContainerExec` with the comment `// ContainerConfigure reads a container's full configuration and recreates or runs one; it exposes environment values, so it sits with exec.` Add it to the `organization_admin` case of `Allows` only. Add `canConfigure` to `tenant.ts`. Update `internal/permissions/AGENTS.md`'s action list.
- [ ] **Step 4: Run** both suites; PASS. Also `go test ./internal/api -run TestPrivilegedEndpointsRequireAdmin` still passes (no route yet).
- [ ] **Step 5: Commit** `permissions: container.configure for organization administrators`.

---

### Task 2: Protocol: configuration type and frames

**Files:**
- Create: `internal/agent/protocol/configuration.go`, `internal/agent/protocol/configuration_test.go`

**Interfaces:**
- Produces:
  ```go
  const (
      TypeConfigurationOpen   = "configuration.open"
      TypeConfigurationResult = "configuration.result"
      TypeConfigurationCancel = "configuration.cancel"
      CapabilityContainerConfigure = "container.configure"
      ConfigurationLifetime        = 20 * time.Second
      MaxConfigurationFrameBytes   = 320 << 10
      MaxArgv, MaxArgvEntryBytes   = 64, 4096
      MaxListEntries, MaxListEntryBytes = 32, 1024
      MaxNetworks                  = 16
      MaxLogOptions                = 16
  )
  // ConfigurationOpen and ConfigurationCancel have exactly InspectionOpen's and InspectionCancel's fields; define them as
  type ConfigurationOpen = InspectionOpen   // same grant shape: request, endpoint, actor, connection nonce, expires, target
  type ConfigurationCancel = InspectionCancel
  type ConfigurationResult struct { Request string; Status string /* ok|unavailable|busy */; Result *ContainerConfiguration }
  type EnvEntry struct { Name, Value string }
  type NetworkAttachmentSpec struct { Name string; Aliases []string; IP string }   // IP is the static address, "" when dynamic
  type Resources struct { NanoCPUs int64; MemoryBytes int64; MemorySwapBytes int64; PidsLimit int64 }
  type Healthcheck struct { Test []string; IntervalSeconds, TimeoutSeconds, StartPeriodSeconds float64; Retries int }
  type Device struct { Host, Container, Permissions string }
  type LogConfig struct { Driver string; Options map[string]string }
  type ContainerConfiguration struct {
      Target      InspectionTarget   `json:"target"`
      ObservedAt  time.Time          `json:"observed_at"`
      Name        string             `json:"name"`
      Image       ImagePull          `json:"image"`      // Reference (as created), Digest when known, Tag
      ImageID     string             `json:"image_id"`
      Command     []string           `json:"command"`
      Entrypoint  []string           `json:"entrypoint"`
      User, WorkingDir, Hostname string
      Env         []EnvEntry         `json:"env"`
      Labels      map[string]string  `json:"labels"`
      Restart     string             `json:"restart"`     // no|always|unless-stopped|on-failure
      RestartRetries int             `json:"restart_retries"`
      Ports       []Port             `json:"ports"`       // Host 0 = exposed, unpublished
      Mounts      []Mount            `json:"mounts"`      // + Kind tmpfs allowed here
      NetworkMode string             `json:"network_mode"`
      Networks    []NetworkAttachmentSpec `json:"networks"`
      Resources   Resources          `json:"resources"`
      Healthcheck *Healthcheck       `json:"healthcheck,omitempty"`
      Privileged, ReadOnlyRootfs, Init, TTY, StdinOpen bool
      CapAdd, CapDrop, SecurityOpt, ExtraHosts, DNS []string
      Devices     []Device           `json:"devices"`
      Log         LogConfig          `json:"log"`
      StopSignal  string             `json:"stop_signal"`
      StopTimeout *int               `json:"stop_timeout,omitempty"`
      Unsupported []string           `json:"unsupported"` // env_truncated, labels_truncated, argv_truncated, list_truncated:<field>, plus any UnsupportedCodes the agent cannot express in ExplicitService (volumes_from, volume_driver, mount_options, ulimits, sysctls, device_requests, pid_mode, ipc_mode, userns_mode, cgroup_parent, group_add, links, runtime)
  }
  func (c *ContainerConfiguration) Validate(target InspectionTarget, now time.Time) error
  func (r ConfigurationResult) Validate() error
  ```
  JSON names are snake_case as shown (`cap_add`, `read_only_rootfs`, `stdin_open`, `working_dir`, `network_mode`).

- [ ] **Step 1: Failing tests** in `configuration_test.go`: (a) a full valid configuration validates; (b) each bound violated fails with a named error (`env` 129 entries, one value 16 KiB+1, argv 65, a 33-entry `cap_add`, 17 networks, 33 labels); (c) `Target` mismatch fails; (d) `ObservedAt` more than `MaxClockSkew` from `now` fails; (e) `Restart` outside the vocabulary fails; (f) an `Unsupported` code outside `UnsupportedCodes ∪ {env_truncated, labels_truncated, argv_truncated}` or not matching `list_truncated:[a-z_]+` fails; (g) NUL bytes in any string fail, but `\n` and `=` inside an env value pass; (h) `ConfigurationResult{Status: "ok"}` without `Result` fails, `busy`/`unavailable` without it pass; (i) marshalled size of the maximal valid configuration is under `MaxConfigurationFrameBytes`.
- [ ] **Step 2: Run** `go test ./internal/agent/protocol -run 'Configuration'`; FAIL (undefined).
- [ ] **Step 3: Implement** the file with the types above and `Validate` reusing `validEnv`'s key regex (exported-internal helper already in `deployment.go`; extract `validEnvKey`/`validEnvValue` if needed without changing their callers), `Mount` validation from `validMounts` (allow `MountKind == "tmpfs"` here only), `Port` validation with `Host` 0 permitted, `CleanText` on every free string. Add the three truncation codes to a `configurationCodes` set (do not add them to `UnsupportedCodes`, whose fixture `web/src/protocol-codes.json` is pinned by `TestWebVocabularyFixture`).
- [ ] **Step 4: Run** the protocol suite; PASS. Run `go test ./internal/agent/protocol -run TestWebVocabularyFixture` to confirm the fixture is untouched.
- [ ] **Step 5: Commit** `protocol: container configuration read frames`.

---

### Task 3: Docker adapter: read a container's configuration

**Files:**
- Modify: `internal/runtime/docker/inspection.go` (new file `configuration.go` preferred), `internal/runtime/docker/inspection_integration_test.go`
- Test: `internal/runtime/docker/configuration_test.go`, integration test

**Interfaces:**
- Produces: `func (c *Client) ReadConfiguration(parent context.Context, target protocol.InspectionTarget) (*protocol.ContainerConfiguration, error)`; same `callBudget`, `inspectionGet` and identity check as `InspectContainer` (read container, image, container again; `ErrInspectionChanged` on drift). Errors never wrap daemon text (same rule as `inspectionGet`).

- [ ] **Step 1: Failing tests** with a fake engine (`httptest`) serving a `/containers/{id}/json` body that has: `Config.Env` `["A=1","B=x=y","C=line1\nline2","NOEQ"]`, `Cmd`, `Entrypoint`, `User`, `WorkingDir`, `Hostname`, `Labels` (incl. a Compose label), `ExposedPorts` with one unpublished port, `HostConfig` with `PortBindings`, `Mounts` (bind, volume, tmpfs), `RestartPolicy{on-failure,3}`, `NetworkMode: "bridge"`, `Memory`, `NanoCpus`, `PidsLimit`, `CapAdd`, `Devices`, `LogConfig`, `Privileged`, `Init`, `StopSignal`, `StopTimeout`, `Healthcheck`, plus `VolumesFrom: ["x"]` and `Sysctls`; `NetworkSettings.Networks` with one static IP and aliases; and `/images/{id}/json` with `RepoTags`/`RepoDigests`. Assert: env is split on the first `=` (`B` → `x=y`, `C` keeps the newline, `NOEQ` → `{NOEQ, ""}`); ports include the unpublished one with `Host 0`; mounts include tmpfs; `Unsupported` equals `["volumes_from","sysctls"]` (sorted); every other field maps as given; `Image.Reference` is `Config.Image`, `Image.Digest` from `RepoDigests` when it matches the reference's repository. A second test: 129 env entries → 128 returned and `env_truncated` in `Unsupported`; 65 argv entries → `argv_truncated`. A third: the container changes between the two reads → `ErrInspectionChanged`.
- [ ] **Step 2: Run**; FAIL.
- [ ] **Step 3: Implement** in `configuration.go` with its own decode struct (`inspectedConfiguration`) that decodes `Config` fully (this is the one place environment is read on purpose; say so in a comment referencing spec section 2.1), maps to `protocol.ContainerConfiguration`, emits the truncation and unsupported codes, then calls `Validate`. Do not touch `inspectedContainer` or `inspectedForDeploy`.
- [ ] **Step 4: Integration.** In `TestInspectionRealDocker`, add a fourth fixture created with `-e A=1 -e B=x=y --memory 64m --cap-add NET_ADMIN -p 127.0.0.1::80 --label t=1 --restart on-failure:2 --stop-timeout 7` and assert `ReadConfiguration` returns those values (host port from the fixture's published binding, memory `67108864`, `CapAdd` contains `NET_ADMIN`, restart `on-failure`/2, stop timeout 7) with empty `Unsupported`. Run `KY_TEST_DOCKER_INSPECTION_IMAGE=alpine:3.24 go test -race ./internal/runtime/docker -run TestInspectionRealDocker -v` if Docker is available.
- [ ] **Step 5: Run** `go test -race ./internal/runtime/docker`; PASS. **Commit** `docker: read a container's full configuration`.

---

### Task 4: Agent client: configuration frames and capability

**Files:**
- Modify: `internal/agent/client/inspection.go`, `internal/agent/client/connect.go` (`Options`, `helloCapabilities`, session routing), `cmd/agent/main.go`, `internal/api/local_docker.go`
- Test: `internal/agent/client/inspection_test.go` (or a new `configuration_test.go` in that package)

**Interfaces:**
- Produces: `Options.Configure func(context.Context, protocol.InspectionTarget) (*protocol.ContainerConfiguration, error)`; `helloCapabilities` adds `container.configure` when `Configure != nil` (Docker only); session loop routes `configuration.open`/`configuration.cancel` to a second `inspections`-style handler that replies `configuration.result`.

- [ ] **Step 1: Refactor for reuse.** Generalise `inspections` minimally: give `newInspections` a `frames struct{ open, cancel, result string; maxBytes int; lifetime time.Duration }` and a `run func(ctx, target) (json.RawMessage-able result, status)`; the existing inspection path keeps its constants; the configuration path uses `TypeConfiguration*`, `MaxConfigurationFrameBytes`, `ConfigurationLifetime`, and a run that calls `opts.Configure`, validates with `ContainerConfiguration.Validate(target, now)`, and marshals `ConfigurationResult`. Slots: a separate `configurationSlots` channel of size 1 (one configuration read per agent at a time), created in `Run` beside `inspectionSlots`.
- [ ] **Step 2: Failing tests** mirroring the existing inspection client tests: a `configuration.open` with a valid grant and nonce yields a `configuration.result` with `Status: "ok"` and the configuration; a second concurrent open yields `busy`; an open with a wrong nonce is ignored; `opts.Configure == nil` yields `unavailable`; `helloCapabilities` contains `container.configure` only when `Configure` is set and never for a Kubernetes agent (`CapabilitiesFit` still holds: add the capability to the Docker side of that check if it enumerates them).
- [ ] **Step 3: Run**; FAIL. **Step 4: Implement** the refactor and wiring; `cmd/agent/main.go` sets `Configure: engine.ReadConfiguration` where `inspect` is set; `internal/api/local_docker.go` the same for the built-in agent.
- [ ] **Step 5: Run** `go test -race ./internal/agent/... ./internal/api -run 'Local|Hello|Capabilit|Inspection|Configuration'`; PASS. **Commit** `agent: answer configuration reads`.

---

### Task 5: Server: configuration read route

**Files:**
- Modify: `internal/api/inspection.go` (registry generalisation), `internal/api/agent_connect.go` (route `configuration.result`, size cap), `internal/api/server.go` (route), `internal/store/inspection.go` (`ReadInspectionTarget` reuse), `internal/store/application_adoption.go` or a new store helper for "container is managed"
- Create: `internal/api/configuration_handlers.go`, `internal/api/configuration_test.go`

**Interfaces:**
- Produces: `GET /api/organizations/{organization}/endpoints/{endpoint}/containers/{container}/configuration` → 200 `protocol.ContainerConfiguration` with `Cache-Control: no-store`; 403 (permission, audited `denied`), 409 `runtime_unsupported` (Kubernetes), 409 `application_managed`, 429 (rate), 501 (no capability), 504 (timeout), 409 (target changed). Store: `func (t *tenancyStore) ContainerManaged(ctx, a, endpointID, containerID string) (bool, error)` under `EndpointRead` — true when an `application_instances`/`application_resources` row binds the container ID on that endpoint (find the actual table used by adoption; the adoption code at `store/application_adoption.go` knows). Audit: `container.configuration.read` row via `withTenantTargetDetails` with target `endpoint/container` and details `image=<image_id> unsupported=<n>`; the handler wraps the read so denial is audited before any agent traffic.
- Consumes: `s.inspect` pattern; `inspectionRegistry`.

- [ ] **Step 1: Failing tests** in `configuration_test.go` using the existing api test harness (`api_test.go` helpers, a fake agent from `exec_test.go`/`inspection_test.go` that answers `configuration.open`): organization admin gets 200 with `no-store` and the body's env values; environment admin gets 403 and an audit row `container.configuration.read` `denied`; a service token gets 403; a container owned by an adopted instance gets 409 `application_managed`; an endpoint without the capability gets 501; a Kubernetes endpoint gets 409 `runtime_unsupported`; the 13th read in a minute by one actor gets 429; the audit row for a success carries no env value (grep the audit `details` for a sentinel value the fake agent returns).
- [ ] **Step 2: Run**; FAIL. **Step 3: Implement**: generalise `inspectionRegistry`/`s.inspect` into a frame-parameterised helper (`s.ask(ctx, agent, actor, org, target, frames, allowed)` returning raw result JSON, with `s.inspect` and the new `s.readConfiguration` as thin wrappers), route `TypeConfigurationResult` in `handleAgentFrame` under `MaxConfigurationFrameBytes`, write the handler in the order: rate limit → `runtimeGate(dockerRoute)` → `CheckEndpointAccess(ContainerConfigure)` (403 audited; refuse service tokens with `DenyService`) → `ReadInspectionTarget` → `ContainerManaged` → capability → ask → re-read target → 200.
- [ ] **Step 4: Run** `go test -race ./internal/api ./internal/store`; PASS. Add the route to `TestPrivilegedEndpointsRequireAdmin`. **Commit** `api: configuration read for organization administrators`.

---

### Task 6: Protocol: explicit service in deployment frames

**Files:**
- Modify: `internal/agent/protocol/deployment.go`, `internal/agent/protocol/deployment_test.go`

**Interfaces:**
- Produces:
  ```go
  // DeploymentRequest gains:
  Explicit bool `json:"explicit,omitempty"` // the operator saw and kept every setting; see spec 2.4
  // DeploymentService gains:
  Explicit *ExplicitService `json:"explicit,omitempty"`
  type ExplicitService struct {
      Command, Entrypoint []string; User, WorkingDir, Hostname string
      Labels map[string]string                       // sent as-is; no Compose labels added
      NetworkMode string; Networks []NetworkAttachmentSpec
      Resources Resources; Healthcheck *Healthcheck
      Privileged, ReadOnlyRootfs, Init, TTY, StdinOpen bool
      CapAdd, CapDrop, SecurityOpt, ExtraHosts, DNS []string; Devices []Device
      Log LogConfig; StopSignal string; StopTimeout *int
      RestartRetries int
      AcknowledgedBinds []string                     // host paths the operator confirmed
  }
  const StepRollback = "rollback"
  ```
  `Validate` rules when `req.Explicit`: exactly one service; `Replaces` may be zero (a run) — when zero, `ContainerName` is required and `Volumes` may be empty; `Explicit != nil`; ports may have `Host == 0` (exposed only); `Mounts` may include `tmpfs` (source empty); bounds from Task 2; `Project` is the fixed string `direct` (regex-valid) and `Revision` is 1. When `req.Explicit` is false, a service with `Explicit != nil` is invalid, and every existing rule stands. Add `StepRollback` and the codes `rollback_failed` (detailNone) and `start_failed_rolled_back` (detailNone) to `stepCodes`; `MaxDeploymentResultSteps` grows by one per service.

- [ ] **Step 1: Failing tests**: golden test `TestDeploymentRequestWireUnchanged` marshals the existing test fixture request (find the fullest one in `deployment_test.go`) and compares to a committed JSON string that must not contain `explicit`; explicit-valid run (no `Replaces`); explicit-valid recreate; non-explicit with `Explicit` set fails; explicit with two services fails; port `Host 0` fails non-explicit and passes explicit; tmpfs mount fails non-explicit and passes explicit; step code `rollback` with `start_failed_rolled_back` validates in a result.
- [ ] **Step 2: Run**; FAIL. **Step 3: Implement.** **Step 4: Run** `go test ./internal/agent/protocol`; PASS; `TestWebVocabularyFixture` unchanged (step codes are not in that fixture; if they are, regenerate with `KY_UPDATE_FIXTURES=1` and commit the JSON). **Commit** `protocol: explicit services and the rollback step`.

---

### Task 7: Docker deploy engine: explicit create, run and rollback

**Files:**
- Modify: `internal/runtime/docker/deploy.go`, `internal/runtime/docker/deploy_test.go`, `internal/runtime/docker/deploy_integration_test.go`

**Interfaces:**
- Consumes: Task 6 types. Produces: `Client.Deploy` handles `req.Explicit`:
  - **precondition** (recreate): identity and `reported()` as today; `undescribed` is **not** applied; `bind_missing` applies only to binds not in `AcknowledgedBinds`; `image_config` not applied. For a run (`Replaces` zero): skip precondition, recheck, rename, stop and remove; the service's steps are `image`/`pull`, `create`, `start`.
  - **createBody** with `Explicit`: `Cmd`, `Entrypoint`, `User`, `WorkingDir`, `Hostname`, `Labels` (exactly the given map, no Compose/kyyard labels), `ExposedPorts` for every port and `PortBindings` only for `Host > 0`, `HostConfig.Mounts` incl. `tmpfs` (`Type: tmpfs`, `Target`), `RestartPolicy{Name, MaximumRetryCount: RestartRetries}`, `NetworkMode`, `NetworkingConfig.EndpointsConfig[name] = {Aliases, IPAMConfig{IPv4Address: IP} when IP != ""}` for the first attachment (Docker create accepts one; extra attachments are connected after create with `POST /networks/{name}/connect` before `start`), `Memory`, `MemorySwap`, `NanoCpus`, `PidsLimit`, `Healthcheck{Test, Interval, Timeout, StartPeriod, Retries}` (seconds → nanoseconds), `Privileged`, `ReadonlyRootfs`, `Init`, `Tty`, `OpenStdin`, `CapAdd`, `CapDrop`, `SecurityOpt`, `ExtraHosts`, `Dns`, `Devices[{PathOnHost, PathInContainer, CgroupPermissions}]`, `LogConfig{Type, Config}`, `StopSignal`, `StopTimeout`.
  - **rollback**: when `req.Explicit`, `Replaces` non-zero and the `start` step fails: record `StepRollback`: `DELETE /containers/{created}?force=1`, rename old back to its name, and if the old container was running before `stop`, `POST /containers/{old}/start`; outcome `succeeded` with code `start_failed_rolled_back`, or `failed` with `rollback_failed`. The `remove` step is then `skipped`.

- [ ] **Step 1: Failing unit tests** with the fake engine: (a) explicit create body contains every field above (assert on the captured JSON); (b) no Compose labels are added when explicit; (c) a run performs exactly `image, create, start`; (d) rollback on start failure: fake returns 500 on `/start` for the new ID, assert the sequence `rename → create → stop → start(fail) → rollback: delete new, rename old back, start old` and the step outcomes; (e) an explicit recreate with a `volumes_from` container is not refused by `undescribed`; (f) a non-explicit request still gets `unsupported` for the same container (regression guard); (g) `bind_missing` for a new bind not acknowledged, none when acknowledged.
- [ ] **Step 2: Run**; FAIL. **Step 3: Implement**, keeping the existing step order and `r.step` helper; add `connectNetworks` after create for attachments beyond the first.
- [ ] **Step 4: Real-Docker tests** (skip without Docker; env var `KY_TEST_DOCKER_INSPECTION_IMAGE`): `TestRecreateRealDocker` changes an env value and memory limit on a fixture and asserts the new container runs with them (`docker inspect` through the client), the old is gone, the name is kept; `TestRunRealDocker` creates a container from the image already present and removes it; `TestRecreateRollbackRealDocker` recreates with `Command: ["/bin/sh","-c","exit 1"]` and asserts the old container is running under its name afterwards, the new one is gone, and the result has a `rollback` step with `start_failed_rolled_back`. Wire them into the CI real-Docker step list (`.github/workflows/ci.yml` `-run` regex) and the root `AGENTS.md` Verification sentence.
- [ ] **Step 5: Run** `go test -race ./internal/runtime/docker` and the real ones locally; PASS. **Commit** `docker: explicit recreate, run and rollback`.

---

### Task 8: Store and API: recreate and run commands

**Files:**
- Modify: `internal/store/migrations/migrations.go` (Version 38: `ALTER TABLE endpoint_commands ADD COLUMN result TEXT NOT NULL DEFAULT ''` SQLite and Postgres), `internal/store/commands.go`, `internal/store/store.go` (interface), `internal/store/application_apply.go` (`SettleDeployment` fallback), `internal/api/agent_connect.go` (result routing), `internal/api/server.go` (routes), `internal/api/AGENTS.md` later
- Create: `internal/api/recreate_handlers.go`, `internal/api/recreate_test.go`, `internal/store/commands_recreate.go`

**Interfaces:**
- Produces:
  - Commands: actions `container.recreate` and `container.run` in `CommandPermission` → `ContainerConfigure`; `Command` gains `Result *storedDeploymentResult` (`json:"result,omitempty"`, the same shape deployments store: code, steps, services) read from the new column.
  - Store: `func (t *tenancyStore) CreateDirectCommand(ctx, a, endpointID, action, containerID string, req protocol.DeploymentRequest, acknowledged []string) (*Command, error)` under `withTenantTargetDetails(ContainerConfigure, endpoint+"/"+container, details "image=<digest> fields=<comma list> binds=<n>")`, which refuses when another `container.recreate|run` command on the endpoint has no outcome (`ErrConflict` → 409 `command_in_progress`), refuses a managed container (`application_managed`), and stores the request ID as `request_id`. `func (t *tenancyStore) SettleDirectCommand(ctx, endpointID string, res protocol.DeploymentResult) error` matches `request_id = res.RequestID`, writes `outcome` (`succeeded|failed|denied|timed_out|unknown` from the result's state mapping used by `SettleDeployment`), `detail` (result code), `result` JSON, `settled_at`. `SettleDeployment` returns `ErrNotFound` when no deployment row matches; the agent-connect handler then calls `SettleDirectCommand`, and only if both are not found logs the unmatched result as today.
  - HTTP: `POST .../containers/{container}/recreate` body `{expects: {image_id, created_unix, state}, spec: <ContainerConfiguration minus target/observed_at/unsupported>, acknowledge_binds: []string, confirm: <container name>}`; `POST .../containers` body `{spec, acknowledge_binds, confirm: <spec.name>}`. Both: `container.configure` (403 audited, service tokens denied), `runtimeGate(dockerRoute)`, endpoint active and connected (409 `endpoint_offline`), capability `deployment.apply` plus `container.configure` on the endpoint (501), rate 12/min shared with the read. Plan-time checks returning 422 `{code: "invalid_spec", blockers: [...]}` with codes: `configuration_incomplete` (recreate only, when the last read for this container by this actor reported `unsupported`; keep a 10-minute in-memory note keyed actor+container from the read handler), `name_taken` (run: name present in inventory), `port_conflict` (a published host port/IP already bound in the inventory by another container), `image_unresolved`, `bind_unacknowledged`, `spec_invalid:<field>`. Image: if `spec.image.digest` is set and the inventory lists that image ID, use it; otherwise resolve `spec.image.reference` through the registry rules the plan uses (`PinImages`, `registryFor`/`anonymousPull`) under `image.pull` authority and add a `Pull` with `Registries` auth as `buildDeploymentFrame` does. Build the frame: `Project: "direct"`, `Revision: 1`, `Explicit: true`, `Deadline: now + DeploymentApplyDeadline`, one service with `Name: "direct"`, `ContainerName: spec.name`, `Replaces` from `expects` (recreate) or zero (run), `Env` from spec, `Explicit` from spec, `Mounts`, `Ports`, `Restart`, `Volumes` = named volumes mounted. `frameBlocker` check. Dispatch with `s.agents.deliver(endpoint, envelope(TypeDeploymentApply, req))`; on failure settle the command `failed` `deployment_not_sent` and return 409. Success: 202 with the command row.
  - `GET .../commands/{id}` now includes `result` (already served by `scanCommand` once the column is added).

- [ ] **Step 1: Failing tests** (`recreate_test.go`, harness as in `deployment_apply_test.go` with a fake agent that records the frame and answers `deployment.result`): OA recreate → 202, the frame has `Explicit: true`, one service, no Compose labels, `Replaces` from `expects`; the command settles from the result with `outcome succeeded` and `result.steps`; the Activity list shows it; env values appear in the frame and in no audit `details`; a second recreate while the first is unsettled → 409 `command_in_progress`; environment admin → 403 audited; managed container → 409; `unsupported` on the last read → 422 `configuration_incomplete`; run with a taken name → 422 `name_taken`; run with a new bind not acknowledged → 422 `bind_unacknowledged`; acknowledged → 202 and the audit details carry `binds=1`; an offline endpoint → 409; a result for an unknown request id is still logged, not an error; migration 38 applies on SQLite and Postgres (the store migration test enumerates versions).
- [ ] **Step 2: Run**; FAIL. **Step 3: Implement**. **Step 4: Run** `go test -race ./internal/store ./internal/api` (and `make test-postgres` if available); PASS. Add both routes to `TestPrivilegedEndpointsRequireAdmin`. **Commit** `api: recreate and run containers as direct commands`.

---

### Task 9: Web: types, read and result plumbing

**Files:**
- Modify: `web/src/tenant.ts` (types `ContainerConfiguration`, `ExplicitSpec`, `DirectCommand`), `web/src/components/ContainerControls.tsx` (export `Command` type; poll accepts a `result`), `web/src/components/ApplicationDeploymentPlan.tsx` (export a `StepTable` component built from `ResultSection`'s table so the container page can render steps)
- Create: `web/src/components/containerConfiguration.ts` (pure helpers: `toSpec(config)`, `diff(a, b): string[]` field names, `parseConfiguration(payload, target)` validating shape like `parseInspection`)
- Test: `web/src/components/containerConfiguration.test.ts`

**Interfaces:**
- Produces: `parseConfiguration(payload: unknown, target: InspectionTarget): ContainerConfiguration | null`; `toSpec(c): Spec` strips `target`, `observed_at`, `unsupported`; `diff(before: Spec, after: Spec): string[]` returns changed top-level field names (`env` counts as changed when any name or value differs, without exposing values); `StepTable({ steps })` exported from `ApplicationDeploymentPlan.tsx` using `stepText` and adding `rollback` to `STEP_CODES` (`start_failed_rolled_back`: "The new container did not start; the previous one was restored.", `rollback_failed`: "The new container did not start and the previous one could not be restored; check the host.").

- [ ] **Step 1: Failing tests**: `parseConfiguration` rejects a wrong target, unknown `unsupported` codes and missing arrays; `toSpec` round-trips; `diff` reports `env` for a value change and nothing for identical input; `StepTable` renders the two new codes' texts.
- [ ] **Step 2: Run**; FAIL. **Step 3: Implement.** **Step 4: Run** `cd web && npx vitest run && npx tsc -b`; PASS. **Commit** `web: configuration types, diff and step table`.

---

### Task 10: Web: Configuration edit form and Run form

**Files:**
- Create: `web/src/components/ContainerConfigurationForm.tsx`, `web/src/components/ContainerConfigurationForm.test.tsx`
- Modify: `web/src/pages/ContainerPage.tsx` (Configuration tab), `web/src/pages/ContainerPage.test.tsx`, `web/src/router.ts` + `web/src/router.test.ts` (route `{ name: 'container-new'; org; endpoint }` at `/organizations/{org}/endpoints/{endpoint}/containers/new`), `web/src/App.tsx`, `web/src/pages/EndpointPage.tsx` ("Run a container" link on the Containers toolbar, `canConfigure` only), `web/src/styles/theme.css` (form group styles)

**Interfaces:**
- Produces: `ContainerConfigurationForm({ base, org, endpoint, mode: 'edit' | 'run', initial?: ContainerConfiguration, container?: Container, managed?: { application: string; link: string }, onSubmitted(command) })`.
  - Groups: Image (reference, digest shown read-only, "Pull the reference's current digest" checkbox that clears `digest`); Command (entrypoint, command as one line per arg, working dir, user, hostname); Environment (name/value rows, value `type="password"` with a per-row Reveal toggle and a "Reveal all" button that posts nothing but is recorded client-side; add/remove rows); Ports (container, host IP, host port, protocol; host port empty = exposed only); Volumes (kind, source, target, read-only; a new bind row shows the acknowledgement checkbox "This container will see host path X"); Network (mode; attachments name/aliases/static IP); Restart (policy, retries); Resources (CPUs as a decimal converted to nano, memory and swap in MiB, pids); Health check (test, interval, timeout, start period, retries); Security (privileged, read-only root, init, capabilities add/drop, security options, devices) with the existing warning style; Logging (driver, options rows); Misc (DNS, extra hosts, stop signal, stop timeout, TTY, stdin).
  - `unsupported` non-empty → a fixed notice listing `unsupportedNames` (plus the three truncation texts) and Save disabled.
  - `managed` → read-only with "Managed by application X. Edit it there." link.
  - Save (edit): shows the diff summary (`diff()` field names; env as "changed" only) and requires the typed container name; posts `recreate` with `expects` from the page's container row; Run: requires the typed new name; posts `containers`. Errors through `tenantWrite` conflict table: `command_in_progress` ("A recreate or run is already in progress on this host."), `application_managed`, `endpoint_offline`, `deployment_not_sent`; 422 `invalid_spec` blockers mapped to fixed texts per code; 501 "Upgrade the host agent to enable editing."
  - After 202 the page polls the command (reuse `ContainerControls`' poll logic extracted into a `useCommand(base, id)` hook in `ContainerControls.tsx`) and renders `StepTable` from `result.steps`, then reloads inventory; a recreate result changes the container ID, so the page navigates to the new container's page when `result.services[0].container_id` is present.
  - Non-administrators keep today's redacted view; the page passes `canConfigure(role)`.

- [ ] **Step 1: Failing tests**: form round-trip (a configuration → spec with no diff → Save disabled); an env value edit → diff lists `env`, values masked in the DOM until Reveal; typed-name gate; bind acknowledgement required for a new bind row; managed read-only; `unsupported` disables Save and names the setting; submit posts the expected body (CSRF header, `expects`) and shows the step table after the poll returns a result with a `rollback` step; run route renders the empty form and requires image and name; `Run a container` link only for `organization_admin`.
- [ ] **Step 2: Run**; FAIL. **Step 3: Implement.** **Step 4: Run** `cd web && npx vitest run && npx tsc -b`; PASS. **Commit** `web: edit any container's configuration and run a new one`.

---

### Task 11: Smoke and acceptance

**Files:**
- Modify: `scripts/smoke-test.sh`, `docs/ACCEPTANCE.md`

- [ ] **Step 1:** In the smoke script, after the local agent's inventory is read: `POST .../containers` runs `alpine:3.24` (already pulled by CI for the real-Docker step; skip the block when `docker image inspect alpine:3.24` fails) named `ky-smoke-run` with `sleep 60`, polls the command to `succeeded`, reads its configuration (asserts `"name":"ky-smoke-run"` and the env it was given), recreates it with a changed env value, polls to `succeeded`, asserts the new container's configuration has the new value, then removes it through the existing `container.remove` path. Every step uses the admin session and the CSRF token as the script already does.
- [ ] **Step 2:** `docs/ACCEPTANCE.md`: add steps for edit and run and remove any Known gap they close.
- [ ] **Step 3:** Run the smoke script locally (`make smoke` or as the Makefile does); PASS. **Commit** `test: smoke edit and run of a container`.

---

### Task 12: Docs, DOX pass, embedded build

**Files:**
- Modify: `docs/threat-model.md` (new row "Configuration read and recreate of any container": mitigations = OA-only permission, no persistence, audit of names only, agent-side bounds, explicit-only precondition relaxation, rollback; proofs = the Task 5, 7, 8 tests), `docs/authorization-matrix.md` (row `container.configure` OA-only, secret column "environment values transit in memory, never stored"; delete the "configuration editing requires adoption" sentence and update the Decisions row), `docs/application-schema.md` (lines about inspection not exposing env and the "Unmanaged edits" decision → direct, `container.configure`), `docs/agent-protocol.md` (configuration frames section; deployment apply: `explicit`, rollback step, outcome codes), `KyYard-Engineering-Handoff.md` line 434 (answered: direct edit), root `AGENTS.md` (Verification real-Docker list; User Preferences unchanged), `internal/api/AGENTS.md`, `internal/agent/AGENTS.md`, `internal/runtime/AGENTS.md`, `internal/store/AGENTS.md`, `internal/permissions/AGENTS.md`, `web/AGENTS.md`, `web/dist`

- [ ] **Step 1:** Write every doc change, verifying each sentence against the code. Delete stale text rather than annotating it.
- [ ] **Step 2:** `cd web && npm run build`; revert `web/tsconfig.tsbuildinfo`; `make ci`; paste the tail in the report.
- [ ] **Step 3: Commit** `docs: direct container edit contracts; rebuild web dist`.

---

## Self-review notes

- Spec 2.1 trust-model change → Task 12 docs plus Task 5's no-persistence tests. 2.2 permission/audit → Tasks 1, 5, 8. 2.3 read frame and bounds → Tasks 2, 3, 4, 5. 2.4 recreate/run/rollback/explicit → Tasks 6, 7, 8. 2.5 HTTP → Task 8. 2.6 UI → Tasks 9, 10. 2.7 tests → Tasks 3, 5, 7, 8, 10, 11.
- The spec's "rate: one configuration read per container per 5 seconds" became 12 per actor per minute using the existing `allowAttempt`; Task 5 records that as the constraint.
- The spec says values transit "encrypted"; the transport is TLS and the frame is in memory on both sides, as application deploys already do. No new encryption is added; Task 12's threat-model row states this.
- `Command` gains `result`; the Observe page's Activity tab keeps working unchanged (the field is optional).
