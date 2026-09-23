# Deployment history, reapply and removal (PR C)

Last sub-slice of plan PR 15. It closes milestone 6: an operator can see what was deployed,
deliberately return to a prior saved revision, and remove a managed application while keeping
its data.

Decisions recorded 2026-09-23 (Yoshi): a plan may target any saved revision the current
mapping covers; removal travels as a `deployment.remove` frame through the same agent runner;
a removed application is marked `removed` and its revisions and history are kept 90 days.

## Reapply: plan any saved revision

- `PlanRequest.Revision` may be any saved revision `1..latest_revision`. The preflight runs
  against that revision's spec (`preflight` gains a `revision` argument; `0` means latest). The
  adoption preview and head checks are unchanged (the mapping must still have been reviewed
  against the latest revision: `mapping_requires_review` stays). Every service of the chosen
  revision must be mapped and every mapped container must belong to a service of that revision;
  otherwise the existing `service_unmapped` / `unassigned_adopted_containers` blockers apply.
- The plan row records the chosen revision and its digest. `ApplyDeployment` requires the
  row's revision to exist with that digest and to be `<= latest_revision`; it no longer requires
  it to equal the head. Values come from that revision's own encrypted bundle.
- Honesty: an image of the prior revision that is no longer on the host is the existing
  `image_not_reported` blocker, so the plan is refused rather than warned. The UI states, before
  planning a revision below the latest, that the revision's own environment values are used,
  that data written since is not reversed, and that mapping was reviewed against the latest
  definition.

## History

- `ListDeployments` already returns every row newest first. The UI's plan panel gains a
  "Deployment history" section: revision, kind, state, who applied it and when, settled time,
  and an expandable per-step view reusing the result section. The instance's current and
  previous revision are shown at the top.
- Retention (`Prune`): settled rows older than 90 days are deleted unless their revision is the
  instance's current or previous revision; `DeploymentHistoryRetention = 90 * 24h`.

## Removal

Wire (`internal/agent/protocol/deployment.go`):

```go
const TypeDeploymentRemove = "deployment.remove"
const CapabilityDeploymentRemove = "deployment.remove"
type RemovalRequest struct {
	Deployment string            // UUID of the deployments row (kind remove)
	Endpoint   string
	Project    string
	Deadline   time.Time         // at most DeploymentLifetime out
	Containers []RemovalTarget   // 1..1000, the instance's adopted resources
}
type RemovalTarget struct {
	Service string            // service name, or "unmapped-<12 hex>" for an unmapped resource
	Target  InspectionTarget  // pinned identity, Validate() must pass
}
func (r RemovalRequest) Validate(now time.Time) error
```

The result is a `DeploymentResult` with steps `precondition`, `stop`, `remove` per target
(`Service` = the target's service label) and no identities. Frame cap
`MaxDeploymentRequestBytes`.

Adapter (`internal/runtime/docker/remove.go`): `func (c *Client) Remove(ctx, req
protocol.RemovalRequest) protocol.DeploymentResult`. Per target in order: precondition
(`GET /containers/{id}/json`: Id, Image, Created second must match; 404 → the container is
already gone: `succeeded` with no detail, since a succeeded step carries none and the server
treats removed and absent alike, and the target's stop and remove `skipped`), stop (`?t=10`, 304 ok, `operationBudget`), remove
(`DELETE /containers/{id}` without `v=1`, 404 counts as succeeded). Any other non-success ends
the run; later steps `skipped`. Networks and volumes are never touched; the project network
(if any) stays. Same outcome classification as `Deploy`. A time guard before each target's stop
refuses to go on unless `operationBudget + 2*callBudget` remain.

Agent: `Options.Remove`; capability `deployment.remove` advertised when set; the deployer
handles `deployment.remove` exactly like `deployment.apply` (size cap, Validate, foreign
endpoint, replay, running-ID silence, one at a time, ledger, delivery). Both agents set
`Remove: engine.Remove`.

Store:

- Migration 25: `deployments.kind TEXT NOT NULL DEFAULT 'apply' CHECK(kind IN ('apply','remove'))`;
  `applications.removed_at` nullable timestamp.
- `RemoveApplication(ctx, a, app, RemovalRequestBody{InstanceID, Confirm}) (*Deployment,
  *protocol.RemovalRequest, error)` under `application.destroy` (org admin, env admin): locks
  the application; the instance must exist with the given ID and `confirm` must equal its
  project; refuses while any live row exists (`ErrDeploymentPlanned` /
  `ErrDeploymentInProgress`); inserts a `kind='remove'` row already in state `applying` with
  `revision = latest_revision`, the removal plan JSON (`{project, containers:[{service,
  container_id, image_id, created_unix, name}]}`), `applied_by`, `applied_at`, `deadline`; builds
  the `RemovalRequest`. One store call, one audit row.
- `SettleDeployment` on a `kind='remove'` row that `succeeded`: delete the instance's
  `application_resources`, delete the instance row (release), set `applications.removed_at`,
  leave revisions and history. Partial or failed: resources for targets whose `remove` step
  succeeded are deleted (they no longer exist on the host); the instance stays adopted with the
  rest. Identities in a removal result are refused (`ErrInvalid`).
- `Application` gains `RemovedAt *time.Time`; `ListApplications` includes removed applications
  (the UI labels them); `DiscardApplication` is allowed for a removed application (already
  released) and deletes its revisions and history; import of the same name after removal is
  refused until discard (name uniqueness unchanged).
- `Prune`: applications with `removed_at` older than 90 days are deleted with their revisions
  and deployments (`ApplicationRemovedRetention = 90 * 24h`).
- `ReleaseApplication` (manual) is unchanged.

API:

- `POST .../applications/{application}/removal` body `{instance_id, confirm}` under
  `application.destroy`, CSRF: capability `deployment.remove` (501), connected (409
  `endpoint_offline`), `RemoveApplication`, deliver `deployment.remove` (failure →
  `FailDeployment`, 409 `deployment_not_sent`), 202 with the row.
- `GET .../deployments` rows carry `kind`.

UI:

- Plan panel: a revision selector (default latest) with the reapply warning text when a lower
  revision is chosen; the plan and apply flow is otherwise unchanged. A "Deployment history"
  section as above.
- Adoption panel (`ApplicationAdoption.tsx`): "Remove application" for administrators with a
  typed project confirmation naming what is kept (named volumes, images, revisions for 90 days)
  and that the containers will be stopped and removed; POST removal; the plan panel's polling
  and result section show its progress. A removed application shows "Removed" in the list with
  its `removed_at`, and offers Discard.

## Tests

- Protocol: `RemovalRequest.Validate` bounds.
- Adapter fake Engine: happy path call order per target; already-gone container; stop failure;
  remove 409; deadline guard; two targets, second fails; no `v=1` ever sent.
- Agent: remove frames go through the same runner (replay, busy, foreign, no runtime).
- Store (SQLite + PostgreSQL): plan a prior revision (blockers when unmapped services or
  missing images; apply uses that revision's values); removal preconditions; settle of a
  removal (full, partial); removed application listing and discard; prune of history and
  removed applications; identities in a removal result refused.
- API: removal route codes and the frame round trip with a fake socket; `TestApplyRealDocker`
  extended to remove the application after applying and assert the containers are gone, the
  network remains, the instance is released and the application is marked removed.
- Web: revision selector posts the chosen revision and shows the warning; history renders rows
  and steps; removal confirmation and POST body; removed label.

## Documents

`docs/agent-protocol.md` (Deployment remove), `docs/application-schema.md` (Reapply, History,
Delete semantics implemented), `docs/authorization-matrix.md` (`application.destroy`
implemented), `docs/retention-policy.md`, `internal/agent/AGENTS.md`, `internal/runtime/AGENTS.md`,
`internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md`,
`KyYard-Implementation-Plan.md` section 8 (M6 complete; next M7a).

## Carried from earlier slices (in scope here)

Audit rows for abandon, sweep and not-sent transitions; the endpoint name on the deployment
row; a closed detail vocabulary is NOT done here (deferred to M7a with the registry work).

## Out of scope

Registry pulls and credentials (M7a), plan-time live inspection of host configuration,
automated rollback (M7b), a started-marker in the agent ledger (M7a).
