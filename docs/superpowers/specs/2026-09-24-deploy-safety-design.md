# Deploy safety (PR D1)

A deployment today is decided from a stored inventory up to three minutes old, checked once
against the live container before a pull that can take minutes, and sent to an agent whose
clock nobody compared with the server's. Six gaps carried in `KyYard-Implementation-Plan.md`
§8 close here. The bookkeeping items (step-detail vocabulary, audit correlation IDs, update
plans under the per-application guard, username index, admin row lock, volume-name length)
are PR D2.

Decisions recorded 2026-09-24 (Yoshi): the plan inspects the live host through the existing
inspection command; clock skew is refused on both sides with a five-minute bound.

## 1. Precondition re-check at each replacement

`internal/runtime/docker` phase one inspects each service's old container once
(`StepPrecondition`), then pulls. Phase two renames and recreates without looking again, so a
`docker update`, a restart with a new image, or a manual recreate in the pull window is not
caught.

- `deployRun.prepare` keeps the phase-one inspection (`inspectedForDeploy`) on the prepared
  service.
- `deployRun.replace` opens with a new step `StepRecheck = "recheck"`: a fresh
  `GET /containers/{id}/json`. The step is `denied` when the identity differs from
  `Replaces` (same rule as the precondition) or when the fresh `Config`, `HostConfig` or
  `Mounts` differ from the phase-one copy (`reflect.DeepEqual` on the decoded structs; `State`
  and `NetworkSettings` are not compared). Detail: `the container changed after the
  precondition`. The service is untouched; later services are recorded `skipped`, as for any
  phase-two failure. Services already replaced stay replaced.
- `Client.Remove` already inspects per target immediately before its stop and needs nothing.

## 2. Plan-time live inspection

Preflight keeps reading the stored inventory for mapping, images, ports and mounts. Planning
adds one live inspection per mapped service so the operator learns at plan time what the
agent would otherwise deny at apply.

### Agent

`ContainerInspection` gains `Unsupported []string` (closed vocabulary, at most 32 entries)
and `ConfigurationVerified` becomes meaningful: `true` exactly when `Unsupported` is empty.
`Validate` requires the pair to agree and refuses any value outside the vocabulary. The
vocabulary is what `undescribed` checks today, as codes without values:

`mount_type`, `anonymous_volume`, `volumes_from`, `volume_driver`, `mount_options`, `tmpfs`,
`auto_remove`, `read_only_rootfs`, `privileged`, `capabilities`, `security_opt`, `devices`,
`pid_mode`, `ipc_mode`, `user`, `runtime`, `resource_limits`, `ulimits`, `sysctls`,
`device_requests`, `init`, `userns_mode`, `cgroup_parent`, `group_add`, `extra_hosts`, `dns`,
`links`, `network`, `image_config`.

`undescribed` returns `[]string` of these codes instead of one sentence; the deployment
precondition detail joins them (`unsupported: privileged, devices`). `InspectContainer` decodes
the same `/containers/{id}/json` body into `inspectedForDeploy` alongside its redacted struct
and runs `undescribed` with the project network `<project>_default` derived from the
container's `com.docker.compose.project` label (no label: `network` is reported only if the
container is on a network other than `bridge`) and the default runtime from `GET /info`
(cached per client for one minute). No raw value enters the inspection.

### Server

`store.PlanRequest` gains `Inspections map[string]protocol.ContainerInspection`, keyed by
container ID, supplied by the API layer. `PlanDeployment` (not `PreflightApplication`)
requires one entry per mapped service and adds per-service blockers:

- `inspection_unavailable`: no entry for the service's container.
- `replacement_identity_changed`: the entry's `Target` differs from the preflight's
  `InspectionTarget`.
- `configuration_unsupported`: `ConfigurationVerified` is false; `PreflightService.Unsupported`
  carries the codes.

`handlePlanDeployment` first calls `PreflightApplication` for the targets (a read). When the
endpoint lacks `container.inspect` it skips the fan-out and lets §6 block the plan. Otherwise it
inspects the targets sequentially through a function extracted from `handleContainerInspection`
(`s.inspect(ctx, agent, actor, org, target) (protocol.ContainerInspection, error)`; the
handler keeps its admission, HTTP mapping and re-read of the target). The whole fan-out has a
10 s budget (`planInspectionBudget`); each request's `Expires` is the earlier of the budget
and `InspectionLifetime`. A failure of any kind (offline agent, `busy`, `unavailable`,
validation, budget) leaves that service without an entry, so the plan is refused with
`inspection_unavailable` and the response names the service. The fan-out counts one
`inspection:` rate-limit attempt per plan, not per target. The response write deadline is
extended by the budget, as `extendRegistryDeadline` does for updates.

The plan row is not changed; the inspection is consumed and dropped, matching the inspection
contract (nothing persisted, no new authority).

## 3. Clock skew

- `DeploymentRequest` and `RemovalRequest` gain `IssuedAt time.Time` (`issued_at`), the
  server's clock when the frame was built. `Validate(now)` requires `|now − IssuedAt| ≤
  MaxClockSkew` (5 minutes) and `Deadline` within `(IssuedAt, IssuedAt + DeploymentLifetime]`,
  in addition to the existing checks against `now`. A missing `issued_at` fails validation: an
  agent built with this change refuses a frame from an older server, and the server's
  `Validate` before send catches a build error.
- The agent reports a skewed frame as outcome `failed`, detail `clock skew exceeds 5 minutes`,
  no step run.
- Preflight (both `PreflightApplication` and `PlanDeployment`) adds the top-level blocker
  `clock_skew` when the inventory row's `observed_at` and `received_at` differ by more than
  `MaxClockSkew`. `freshInventory` keeps its existing windows.

## 4. Started marker in the agent ledger

`internal/agent/client` records a deployment only when it finishes, so an agent restarted
between the first rename and the result re-runs the deployment when the frame is re-sent.

- `Options.Deploy` and `Options.Remove` gain a third parameter `started func()`. The runtime
  calls it once, immediately before the first phase-two mutation (`Deploy`: before the first
  `StepRecheck`; `Remove`: before the first stop). A run that fails in phase one never calls
  it and stays re-runnable.
- `deployer.run` passes a closure that writes the ledger entry `{Started: now}` for the
  deployment ID durably (`writeDurable`) before returning. `finish` fills in the result as
  today.
- On load, every entry with `Started` set and no result becomes a result of outcome
  `unknown`, detail `the agent restarted after replacement began; inspect the host`, and is
  re-sent like any other result. A re-sent frame for such an ID is answered from the ledger,
  never re-run.
- Pruning never drops an entry that has `Started` and no result.

`docs/agent-protocol.md` §8 gains the rule: a deployment whose result is `unknown` needs an
operator's inspection before another plan for that application; `SettleDeployment` already
maps the outcome.

## 5. Frame caps at plan time

`ApplyDeployment` builds the wire frame, validates it and measures it against the endpoint's
cap (320 KiB with `deployment.pull`, 192 KiB otherwise). Planning learns nothing of this.

- The frame builder moves to `buildDeploymentFrame(ctx, tx, plan, key, now) (protocol.DeploymentRequest, error)`
  shared by `PlanDeployment` and `ApplyDeployment`. At plan time the frame is built with the
  real environment values and registry credentials, measured, and dropped; nothing is stored.
- `PlanRequest` gains `MaxFrameBytes int`, computed by the handler from the endpoint's
  capabilities exactly as `handleApplyDeployment` does. Top-level blockers:
  `frame_too_large` (marshalled size over `MaxFrameBytes`) and `too_many_registry_hosts`
  (more than `MaxRegistryAuthHosts` distinct credentialed hosts). Every other validation error
  from `Validate` is `frame_invalid`.
- Apply keeps its own check: capabilities can change between plan and apply.

The threat-model residual for registry credentials changes from "refused only at apply" to
"decrypted in memory at plan and apply; never stored in a plan".

## 6. Agent capability gating at plan time

`PlanDeployment` reads the endpoint's capabilities (`endpoint_capabilities`). Top-level
blockers: `agent_deploy_unsupported` when `deployment.apply` is absent;
`agent_pull_unsupported` when any service carries a `PullDigest` and `deployment.pull` is
absent; `agent_inspect_unsupported` when `container.inspect` is absent (the fan-out in §2
cannot run). `handleApplyDeployment` keeps its 501s.

## Panels

- Preflight and plan: texts for `clock_skew`, `inspection_unavailable`,
  `replacement_identity_changed`, `configuration_unsupported` (listing the codes with human
  names, e.g. `privileged` → "runs privileged"), `frame_too_large`,
  `too_many_registry_hosts`, `frame_invalid`, `agent_deploy_unsupported`,
  `agent_pull_unsupported`, `agent_inspect_unsupported`.
- Deployment detail: the `recheck` step renders like `precondition`; an `unknown` outcome with
  the restart detail shows the existing "inspect the host" guidance.
- Container inspection dialog: shows "Configuration: fully expressible" or the unsupported
  list.

## Documents

`docs/agent-protocol.md` (§inspection fields, `issued_at`, `recheck`, started marker,
`MaxClockSkew`), `docs/threat-model.md` (drift in the pull window, skew, credentials at plan),
`docs/ACCEPTANCE.md` (the plan now needs an online agent with `container.inspect`),
`KyYard-Implementation-Plan.md` §8 (drop the six items; D2 list stays), AGENTS.md for
protocol, runtime, agent client, store, api, web.

## Tests

Runtime (fake Engine): recheck denies on identity change and on `HostConfig` drift between
phases, leaves the service untouched and skips the rest; earlier replaced services are not
rolled back; `started` is called once, before the first rename, and not on a phase-one
failure; `undescribed` codes for each refusal; `InspectContainer` reports codes and
`ConfigurationVerified`. Real Docker (CI): `docker update --memory` between phases makes
the recheck deny. Protocol: `Unsupported`/`ConfigurationVerified` consistency and vocabulary;
`IssuedAt` skew and deadline windows. Agent client: started entry survives a reload as an
`unknown` result, is re-sent and not re-run; pruning keeps it. Store (SQLite + PostgreSQL):
every new blocker; frame measured with credentials and dropped; `clock_skew` from inventory
timestamps; capability blockers. API: plan fan-out sequential, budget exhaustion →
`inspection_unavailable`, one rate-limit attempt per plan, offline agent. Web: blocker texts
and the inspection verdict.

## Out of scope

Step-detail vocabulary, audit correlation IDs, update plans under the per-application guard
(PR D2); rollback of replaced services after a mid-plan recheck denial; comparing inspection
observations (ports, restart policy) against the definition, which preflight already does
from inventory.
