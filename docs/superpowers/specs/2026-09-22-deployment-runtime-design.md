# Deployment runtime primitives (apply, PR A)

Second sub-slice of plan PR 15. PR A gives the agent the ability to execute a deployment plan
natively against the Docker Engine API and defines the wire types the apply command will carry.
Nothing in PR A is reachable from the server, the agent loop, or the UI: no frame is sent, no
capability is advertised, no route exists. PR B wires it.

Decisions recorded 2026-09-22 (Yoshi): two PRs, A then B; images must already be present on
the host (no pull, no registry credentials until M7a); replacement is stop, rename, create,
start, remove, with no automatic rollback; secrets travel only inside the apply frame and become
container environment. Ruling 2026-09-22: Compose project networks (`<project>_default`) are
accepted and preserved with the service alias; other network modes are refused. Final review
ruling 2026-09-22: rename and create run before stop so avoidable conflicts happen while the old
container still runs; the precondition refuses every configuration recreation would drop and
fails closed on fields the Engine did not report.

## Wire types (`internal/agent/protocol/deployment.go`)

Constants, defined now so PR B cannot drift: `TypeDeploymentApply = "deployment.apply"`,
`TypeDeploymentResult = "deployment.result"`, `CapabilityDeploymentApply = "deployment.apply"`,
`MaxDeploymentRequestBytes = 192 << 10`, `MaxDeploymentResultBytes = 160 << 10`,
`DeploymentLifetime = 15 * time.Minute`, `MaxDeploymentServices = 100`.

```go
type DeploymentRequest struct {
	Deployment string              // UUID of the deployments row
	Endpoint   string              // endpoint the agent must be
	Project    string              // Compose project name, [A-Za-z0-9][A-Za-z0-9_.-]{0,63}
	Revision   int                 // 1..100
	Deadline   time.Time           // at most DeploymentLifetime from now
	Services   []DeploymentService // 1..MaxDeploymentServices, plan order
}
type DeploymentService struct {
	Name          string            // service name, ^[a-z0-9][a-z0-9_-]{0,62}$
	ContainerName string            // name for the new container, ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$
	ImageID       string            // full sha256: image ID, must be present on the host
	Replaces      InspectionTarget  // the mapped container's pinned identity; Validate() must pass
	Restart       string            // "", no, always, unless-stopped, on-failure
	Ports         []Port            // at most 64; Container 1..65535, Host 1..65535, Protocol tcp|udp
	Env           map[string]string // at most 128; names ^[A-Za-z_][A-Za-z0-9_]{0,127}$, values <= 16 KiB, no NUL, total <= 64 KiB
}
```

`(DeploymentRequest) Validate(now time.Time) error` enforces every bound above and refuses a
deadline in the past or beyond `DeploymentLifetime`. Duplicate service names, duplicate
container names, duplicate `Replaces.ContainerID` and a `(HostIP, Host, Protocol)` binding
repeated within or across services are refused; `""`, `0.0.0.0` and `::` are one wildcard
host IP for that check.

```go
type DeploymentResult struct {
	Deployment string
	Outcome    string            // succeeded | failed | denied | timed_out | unknown
	Detail     string            // <= MaxResultDetailBytes, displaySafe
	Steps      []DeploymentStep  // every step attempted, in order
	Services   []DeploymentIdentity // one per service whose new container started
}
type DeploymentStep struct {
	Service string
	Step    string // precondition | image | stop | rename | create | start | remove | skipped
	Outcome string // succeeded | failed | denied | timed_out | unknown | skipped
	Detail  string // <= 256 bytes, displaySafe, fixed text plus a status code at most
}
type DeploymentIdentity struct {
	Service     string
	ContainerID string
	ImageID     string
	CreatedUnix int64
}
```

`(DeploymentResult) Validate() error` bounds steps at 8 × MaxDeploymentServices, identities at
MaxDeploymentServices, and each identity as an `InspectionTarget`; a succeeded or skipped step
must have an empty detail, so the largest valid result fits `MaxDeploymentResultBytes`. Environment values never
appear in a result; the tests prove it with a canary.

## Adapter (`internal/runtime/docker/deploy.go`)

`func (c *Client) Deploy(ctx context.Context, req protocol.DeploymentRequest) protocol.DeploymentResult`

Runs services in order under `ctx` bounded by `req.Deadline`. Every Engine call has the existing
`callBudget`; stop and start have `operationBudget`. Steps per service:

1. **precondition**: `GET /containers/{replaces.id}/json`, decoded with pointers. Refuse
   (`denied`) unless `Id`, `Image` and `Created.Unix()` equal `Replaces`. Refuse "the runtime did
   not report the container's full configuration" when `HostConfig`, `Config`,
   `NetworkSettings`, `Mounts` or `HostConfig.Privileged`/`AutoRemove`/`ReadonlyRootfs` are
   absent. Refuse "the container has configuration the definition cannot express: <reason>" for
   mounts, `HostConfig.Tmpfs`, auto-remove, read-only root, privileged, `CapAdd`/`CapDrop`,
   `SecurityOpt`, `Devices`, a `PidMode` other than `""`/`private`, an `IpcMode` other than the
   daemon defaults `""`/`private`/`shareable`, `Config.User`,
   a `NetworkMode` other than `default`, `bridge` or `<project>_default`, and
   `NetworkSettings.Networks` other than exactly the accepted network (`bridge`, or
   `<project>_default` for the project network mode). Last, `GET /images/{replaces.image_id}/json`
   (404 is `denied` "the container's image is no longer present") and refuse (`command`) when
   the container's `Cmd` or `Entrypoint` differs from the image's (nil equals empty): a run-time
   override would be replaced by the image default. A container 404 is `denied` "the container no
   longer exists". Refusing is the safe default and the message names the reason. When
   the old container is on the project network, the new one is created on it
   (`HostConfig.NetworkMode`) with `NetworkingConfig.EndpointsConfig[<project>_default].Aliases =
   [service]` so Compose service DNS survives. The old container's `Name` is recorded for the
   rename.
2. **image**: `GET /images/{image_id}/json`; `Id` must equal `ImageID`. 404 is `failed`
   "the pinned image is not present on this host".
3. **rename**: `POST /containers/{old}/rename?name={oldName}.kyyard-prev-{deployment[:8]}`,
   while the old container still runs. When less than `2*operationBudget + 3*callBudget` remains
   before `req.Deadline`, it is `timed_out` "not enough time left before the deadline to replace
   this service safely" with no call.
4. **create**: `POST /containers/create?name={ContainerName}` (not started) with body
   `{Image: ImageID, Env: ["K=V"...], Labels: {com.docker.compose.project, com.docker.compose.service, com.docker.compose.container-number: "1", com.docker.compose.oneoff: "False", kyyard.deployment, kyyard.revision}, ExposedPorts: {"<container>/<proto>": {}}, HostConfig: {PortBindings: {"<container>/<proto>": [{HostIp, HostPort}]}, RestartPolicy: {Name, MaximumRetryCount: 0}}}`.
   Expect 201 `{Id}`. 409 is `failed` "a container with that name already exists". The Engine's
   error text is never copied into the result; only the status code is.
5. **stop**: `POST /containers/{old}/stop?t=10`; 304 counts as succeeded.
6. **start**: `POST /containers/{new}/start`; then `GET /containers/{new}/json` to record the
   identity (`Id`, `Image`, `Created`) into `Services`. A failed start or identity read names the
   created container in the step detail.
7. **remove**: `DELETE /containers/{old}` without `v=1` (named volumes are never touched).

A step that is `denied`, `failed`, `timed_out` or `unknown` ends the run: remaining steps of
that service and all later services are recorded as `skipped`. A context error during a call
with no Engine answer is `timed_out` when the deadline passed and `unknown` when the parent was
cancelled (the session dropped; the Engine may or may not have acted); a status received after
cancellation is `failed`. Nothing is rolled back: after a failed create the renamed old container
still runs; after a failed stop or start it stays renamed (running or stopped) beside the created
container; the result's steps say which. Overall `Outcome` is
`succeeded` only when every step succeeded; otherwise it is the outcome of the step that ended
the run, with `Detail` naming the service and step.

`Deploy` validates the request first; an invalid request is `denied` with no Engine call.

Exposed ports use `Port.Container` as the container port and `Port.Host` as the published port.
`Validate` requires `Host` in 1..65535: the plan's `published` is required, so an ephemeral
publish is never requested.

## Tests

Fake Engine (`deploy_test.go`, an `httptest.Server` recording method, path, query and JSON
body per call):

- Happy path: call order is exactly precondition (container, old image), image, rename, create,
  stop, start, inspect, remove; the create body carries the image ID, sorted `K=V` env, all six labels, exposed ports,
  port bindings and restart policy; rename target and remove path are right; the result has one
  identity per service and no environment value (canary).
- Precondition mismatch on identity, every refused configuration, every absent required field,
  command/entrypoint override, old image gone, and 404: each `denied`, later services `skipped`,
  only GETs made.
- Image missing: `failed`, no rename.
- Create 409: `failed`, rename already happened, no stop, no start, no remove.
- Less time left than one replacement needs: `timed_out` at rename with only the three GETs made.
- Identity read failure after start: `failed`, detail names the created container, no remove.
- Stop exceeding the budget: `timed_out`; parent cancel mid-run: `unknown`; deadline already
  past: `denied` by `Validate` with no call.
- Invalid request (duplicate names, bad env name, oversize env): `denied`, no call.
- Two services where the second fails: first has an identity and all steps succeeded, second
  shows the failing step, overall `failed`.

Real Docker (`deploy_integration_test.go`, `TestDeployRealDocker`, gated by
`KY_TEST_DOCKER_DEPLOY_IMAGE`, added to the CI "Real Docker runtime regressions" step): commit
the image with `CMD ["sleep","300"]` as `<p>:local` (no pull), start a fixture container from it
with no command override on `<p>_default` with the Compose labels, read its identity, call
`Deploy` with the same image and an env canary, then prove with `docker inspect` that the new
container is running under the old name with the labels, env, restart policy and service alias,
that the old container is gone, and that the serialized result lacks the canary. Cleanup removes
every container labelled with the fixture project, the fixture image tag and the network.

Protocol validation tests cover every bound in `Validate`.

## Documents

- `internal/runtime/AGENTS.md`: `Deploy` bullet (order, preconditions, refusals, no rollback,
  no pull, no volumes).
- `docs/agent-protocol.md`: new "Deployment apply" section describing the two frame types,
  capability, caps and lifetime as the implemented wire contract, marked "adapter implemented;
  transport lands in PR B".
- `docs/application-schema.md` Deploy section: step list updated to the implemented sequence.
- `KyYard-Implementation-Plan.md` section 8.

## Out of scope (PR B)

Agent frame handling and ledger, capability advertisement, server route, deployment state
machine and events, resource rebinding, secret resolution, UI.
