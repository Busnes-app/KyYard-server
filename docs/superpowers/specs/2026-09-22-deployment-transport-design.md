# Deployment transport (apply, PR B)

Third sub-slice of plan PR 15. PR B makes a persisted plan executable end to end: the server
resolves the revision's values into a `deployment.apply` frame, the agent runs
`docker.Client.Deploy` (PR A) on a context detached from the session, persists the result before
sending it and re-sends it on reconnect, and the server settles the deployment row, rebinds the
adopted resources to the new containers and advances the instance's current revision. The UI
applies a plan with a typed confirmation and shows per-step outcomes.

Decisions recorded 2026-09-22 (Yoshi): `unknown` is non-terminal, a later result settles it;
step outcomes and identities are stored as JSON on the deployments row, no events table yet;
apply-time refusals are explained in the UI and plan-time live inspection is a later slice.

## Agent (`internal/agent/client`)

- `Options.Deploy func(context.Context, protocol.DeploymentRequest) protocol.DeploymentResult`.
  Nil means no runtime: the capability is not advertised and every apply is refused `denied`
  "this agent has no runtime to deploy". Both agents set it to `engine.Deploy`.
- Capability `deployment.apply` (`protocol.CapabilityDeploymentApply`) is advertised in `hello`
  when `Deploy` is non-nil.
- A `deployment.apply` frame larger than `MaxDeploymentRequestBytes` closes the socket with
  `CloseProtocol`. Otherwise decode, `Validate(now)` (failure → `denied` "invalid deployment
  request", sent, not persisted), refuse a request whose `Endpoint` is not this agent's (`denied`,
  not persisted), replay a persisted result for the same deployment ID, refuse when a deployment
  is already running (`denied` "this agent is already applying a deployment"; one at a time,
  agent-wide), otherwise run.
- The run uses a context derived from the agent's root context (the one `Run` received), not
  the session's, so a dropped socket never stops it; `Deploy` bounds it by `req.Deadline`. The
  request is never logged. After `Deploy` returns, the env maps are cleared.
- Results are persisted to `<CommandDir>/deployments.json` (map of deployment ID to
  `{result, finished}`, 24 h, at most 20 entries, written to a temp file and renamed, mode 0600)
  **before** being sent. Every session sends the persisted results younger than 24 h right after
  its `hello` (the server settles once and ignores the rest), and a result that finishes with no
  session waits for the next one. Env values are never in a result, so the file holds none.

## Wire (`internal/api/agent_connect.go`)

- `deployment.result` frames are accepted up to `MaxDeploymentResultBytes` (exempt from the
  64 KiB control cap); larger closes the socket. Decode, `Validate()`, then
  `SettleDeployment(endpoint, result)`; errors are logged with the deployment ID only when it
  parses as a UUID.
- `markOffline` calls `AbandonDeployments(endpoint)` beside `AbandonCommands`.

## Store

Migration 24, `deployment_apply`:

- `deployments`: `state CHECK IN ('planned','applying','succeeded','failed','denied','timed_out','unknown')`;
  new columns `applied_by TEXT NOT NULL DEFAULT ''`, `applied_at`, `deadline`, `settled_at`
  (nullable timestamps), `detail TEXT NOT NULL DEFAULT ''`, `result TEXT NOT NULL DEFAULT ''
  CHECK(length(result)<=163840)`. The `UNIQUE(instance_id)` constraint is replaced by a partial
  unique index `idx_deployments_live ON deployments(instance_id) WHERE state IN
  ('planned','applying')`. SQLite rebuilds the table (create, copy, drop, rename); PostgreSQL
  alters in place.
- `application_instances`: `current_revision INTEGER NOT NULL DEFAULT 0`,
  `previous_revision INTEGER NOT NULL DEFAULT 0`.

Operations (`internal/store/application_deployment.go` and a new `application_apply.go`):

- `PlanDeployment` now deletes only the instance's `planned` row before inserting; an `applying`
  row refuses with `ErrDeploymentInProgress` (409 `deployment_in_progress`).
- `ApplyDeployment(ctx, a, app, id, confirm, key) (*Deployment, *protocol.DeploymentRequest, error)`
  under `withTenantTarget(ApplicationDeploy, app+"/deployments/"+id+"/apply")`: lock the
  application row; the row must be `planned` and unexpired, `confirm` must equal its project;
  the instance must still exist on the same endpoint with the same `mapping_version`, and the
  application's `latest_revision` must equal the row's revision (otherwise `ErrAdoptionChanged`);
  the endpoint must be `active` (otherwise `ErrEndpointOffline`). Values are resolved by the
  internal `resolveApplicationValues(tx, a, app, revision, key)` (the decrypt path of
  `ResolveApplicationSecrets` without its own permission or audit; the apply audit row is the
  record, and `secret.use` is implied by `application.deploy` per the matrix). The request is
  built from the plan: per service `ContainerName` is the adopted resource's recorded name for
  the replaced container, `ImageID`, `Replaces`, `Restart`, `Ports` (host = published,
  container = target), `Env` from the resolved values keyed by the spec's environment names;
  `Deadline = now + 10 min`. Then CAS `planned → applying` with `applied_by`, `applied_at`,
  `deadline`. The returned request exists only in memory.
- `FailDeployment(ctx, id, detail)`: `applying → failed` when the frame could not be queued.
- `SettleDeployment(ctx, endpoint, res)`: row with `id = res.Deployment`, `endpoint_id =
  endpoint`, state in (`applying`, `unknown`); set state to `res.Outcome`, `detail`, `result`
  (steps and identities as JSON), `settled_at`. For every identity in `res.Services`: delete the
  `application_resources` row of the container that service replaced (from the plan) and insert
  the new one (ID, name = the plan's container name, image, `created_at` from `CreatedUnix`,
  `service_name`). On `succeeded`: `previous_revision = current_revision`, `current_revision =
  revision`. Writes an audit row (`application.deploy`, resource `app/deployments/id`, result =
  outcome). A row in any other state is left alone (first answer wins).
- `AbandonDeployments(ctx, endpoint) (int64, error)`: `applying → unknown` with detail "the
  connection ended before a result arrived".
- `Prune` adds: `applying` rows whose `deadline` is more than two minutes past → `unknown`
  "no result arrived before the deadline".
- `ReleaseApplication` refuses while a `planned` unexpired or `applying` row exists; it deletes
  `planned` rows and keeps settled history (no instance FK).
- `ListDeployments` returns history newest first (100), each with state, detail and the parsed
  result; `ReadDeployment` likewise.

## API

- `POST .../applications/{application}/deployments/{deployment}/apply` body `{confirm}` under
  `application.deploy`, CSRF, strict JSON. Order: `ReadEndpoint` capability check (501 "Upgrade
  the host agent to enable deployments" when `deployment.apply` is absent), connection check
  (409 `endpoint_offline`), `ApplyDeployment`, `deliver` the frame (on failure
  `FailDeployment` and 409 "the deployment was not sent"), 202 with the deployment row.
- New error mappings: `ErrDeploymentInProgress` → 409 `deployment_in_progress`.

## Agents

`internal/api/local_docker.go` and `cmd/agent/main.go` set `Deploy: engine.Deploy`.

## UI (`web/src/components/ApplicationDeploymentPlan.tsx`)

- The plan table is gated on the deployments read alone; only the plan form is gated on the
  mapping read (the parked fix).
- A `planned` unexpired row offers "Apply deployment" with a typed project confirmation that
  names the application, revision and endpoint and states that containers will be replaced and
  nothing rolls back. POST apply; on 202 poll `GET .../deployments` every 5 s while the state
  is `applying`, for at most 12 minutes, then stop.
- The row shows state, detail, a 25-row table of steps (service, step, outcome, fixed text
  detail) and the new identities. Fixed explanations: `denied` at `precondition` means the
  container has configuration the definition does not describe and must be reviewed on the
  host; `unknown` means the host may or may not have acted and the operator must inspect it;
  a failed run leaves a `<name>.kyyard-prev-<id>` container on the host.
- Error bodies are never rendered. No retry, no rollback control.

## Tests

- Agent: fake `Deploy` records the request and the context's independence (cancel the session,
  the run continues); size cap, invalid request, foreign endpoint, replay from
  `deployments.json`, single slot, re-send after reconnect, env cleared.
- Store (SQLite and PostgreSQL): apply preconditions (state, expiry, confirm, mapping version,
  head, endpoint, offline); request contents including resolved env and container names; CAS;
  settle rebinding and revision advance; settle from `unknown`; first answer wins; abandon;
  prune sweep; release rules; `PlanDeployment` vs `applying`. A PostgreSQL-only interleaving
  test runs `PlanDeployment` and `ReleaseApplication` concurrently many times and asserts no
  live plan ever survives for a released instance (the parked item).
- API: apply route (401/403/404/409/501/202, CSRF), the frame round trip with a fake agent
  socket (result settles; oversize result closes; result for another endpoint ignored),
  abandon on disconnect. `TestApplyRealDocker` (gated by `KY_TEST_DOCKER_DEPLOY_IMAGE`, in
  CI): server + `client.Run` with a real engine; fixture container on a project network with a
  committed image tag; import a Compose definition naming that tag, adopt, map, plan, apply,
  wait for `succeeded`, assert resources rebound, `current_revision` advanced, the old
  container gone and the new one running on the network.
- Web: vitest for apply confirmation, POST body, polling stop, step rendering, fixed texts,
  gating fix.

## Documents

`docs/agent-protocol.md` (Deployment apply: implemented; ledger and re-send),
`docs/application-schema.md` (Deploy: implemented states, settle rules, revision advance),
`docs/authorization-matrix.md` (`application.deploy` implemented; `secret.use` recorded on the
deployment), `internal/agent/AGENTS.md`, `internal/store/AGENTS.md`, `internal/api/AGENTS.md`,
`web/AGENTS.md`, `KyYard-Implementation-Plan.md` section 8.

## Out of scope

Deployment history UI beyond the list, deliberate reapply, application removal, plan-time live
inspection, registry pulls (M7a), preflight blockers for host configuration.
