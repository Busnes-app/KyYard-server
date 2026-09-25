# Health validation and eligible rollback (M7b PR 19)

PR 18 lets a policy apply an update inside a window. Nothing yet says whether the update
worked. This slice watches every applied deployment for a bounded time, records a verdict,
and, for an update a policy applied, returns to the recorded prior revision and images when it
can, or stops with the reason when it cannot. Either way the policy pauses until an admin looks.

Decisions recorded 2026-09-24 (Yoshi): startup grace 30 s, then a 2-minute observation; every
service must be running, and `healthy` where the image defines a healthcheck; any exit,
restart loop or `unhealthy` fails validation. Rollback re-applies the recorded prior revision
and image digests through the normal deploy path, eligible only if those images are still on
the host and the prior definition still validates; otherwise stop with the reason. After a
rollback or an ineligible failure the policy pauses until an admin acknowledges. Manual
applies get the verdict recorded but are never rolled back automatically.

## Health on the wire

`protocol.ContainerInspection` gains `Health string` (`none|starting|healthy|unhealthy`, from
Docker's `State.Health.Status`; `none` when the image defines no healthcheck) and
`RestartCount int` (Docker's `RestartCount`). Both are allowlisted observations, validated as
an enum and a non-negative bound (`≤ 1_000_000`). The Docker adapter decodes them in
`InspectContainer`. Agents built from this branch advertise `container.inspect.health`;
`Validate` requires the pair present when the capability is advertised (the server knows
which agent it asked). No change to the inventory snapshot.

## Model

Migration 32 adds:

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

- `SettleDeployment` inserts the row in the same transaction that settles a `succeeded`
  `apply` deployment: `phase = grace`, `started_at = settled_at`, `observe_until = settled_at
  + 30 s + 2 min`, `policy_run_id` from the `policy_runs` row whose `deployment_id` is this
  deployment (none → manual), `is_rollback` when this deployment was itself a rollback (the
  validation row that dispatched it names it), `correlation_id` = the deployment's. A
  removal, a failed apply, or a `plan_only` plan gets no row.
- Constants: `ValidationGrace = 30 s`, `ValidationWindow = 2 min`, `ValidationPoll = 20 s`.

## Verdicts

- `healthy`: at the end of the window every service's container is the settled identity,
  `State == running`, `Health ∈ {healthy, none}`, and `RestartCount` equals the baseline
  taken at the first observation after grace.
- `unhealthy`: any observation shows `Health == unhealthy`.
- `exited`: any observation shows `State ∉ {running, restarting}` or the container is gone.
- `restarting`: `State == restarting`, or `RestartCount` above the baseline.
- `changed`: the container ID differs from the settled identity (someone recreated it) — the
  deployment is no longer what was applied; no rollback.
- `unverifiable`: the endpoint is offline for the whole window, the agent lacks
  `container.inspect.health`, or an inspection fails validation; detail names which.
- A failing observation ends the window at once. `starting` is neither pass nor fail and
  keeps observing; a service still `starting` when the window ends is `unhealthy`.

## The validation loop

`api.Server.RunValidations(ctx, done)` beside `RunPolicies`, ticking every `ValidationPoll`
under its own mutex and stopping the same way (`done` closes between ticks; `runServer` waits
under the existing budget). Each tick, for every row not `done`:

1. `grace` and `started_at + ValidationGrace` passed → take the baseline (one inspection per
   service via the plan-time inspection primitive, endpoint capability
   `container.inspect.health` required) and move to `observing`; a missing capability or an
   offline endpoint at this point is `unverifiable` immediately.
2. `observing` → inspect every service; apply the verdict rules; on a terminal verdict or
   `observe_until` reached, finish: set `verdict`, `detail`, `finished_at`, `phase = done`,
   audit `application.validation` (`system`, resource `<app>/deployments/<id>`, result
   `success` for `healthy`, `failure` otherwise, correlation the deployment's).
3. Automated, not a rollback, verdict ∉ {`healthy`, `changed`} → **rollback decision** (below).
   Manual, rollback, or `changed` → done.

Inspections reuse `inspect` with a system actor (`system:validation`); they count against
the per-endpoint admission (2) like any other, so a busy endpoint delays a poll rather than
failing it; a window that ends with no successful observation since the baseline is
`unverifiable` (`the host could not be observed`).

## Rollback

Eligibility is decided in one store call, `RollbackTarget(ctx, a, app, deployment)`:

- The instance's `previous_revision` is set, and the newest `succeeded` `apply` deployment
  at that revision exists with `result.services[]` (the identities that actually ran).
- The prior revision's spec still passes `ValidateApplicationSpec`, and its service set equals
  the current mapping's.
- Every prior `image_id` is present on the host now: in the latest inventory's `images[].id`
  or as a running container's `image_id`.
- No rollback is in flight for the instance, and this deployment has not already been rolled
  back (`rollback_deployment_id` empty).

Ineligible → `rollback_outcome = ineligible`, `rollback_detail` one of `no_prior_revision`,
`prior_definition_invalid`, `service_set_changed`, `prior_images_missing`,
`rollback_in_flight`, `already_rolled_back`.

Eligible → the loop plans and applies as the policy's creator under a fresh correlation ID
recorded in `rollback_detail`'s sibling column `rollback_deployment_id` once known:
`PlanDeployment` with `PlanRequest{Revision: prior, PinImages: map[service]image_id}` where
`PinImages` (`json:"-"`, server-set) makes preflight take the given image ID for that service
instead of resolving the tag (the ID must be on the host, checked again at plan time; no
pull, no registry), then `ApplyDeployment` and `deliver` exactly as a policy run does
(`FailDeployment` on delivery failure → `rollback_outcome = failed`, `not_sent`). The
rollback deployment's own row, result and validation carry `is_rollback = 1`, so its
verdict is recorded but never triggers another rollback: no oscillation.

After the decision, whatever the outcome, the policy pauses: reason `rolled back after a
failed update` (applied), `update failed and could not be rolled back: <rollback_detail>`
(ineligible or failed), or `update could not be validated: <detail>` (`unverifiable`); one
`application.policy.paused` audit row; `Resume` is the acknowledgement. The `policy_runs`
row of the original apply gets `outcome = applied` unchanged; the validation row is the
record, exposed through the run's JSON as `validation {verdict, detail, rollback:
{deployment_id, outcome, detail}}`.

## Restart and shutdown

Rows are durable, so a restarted server resumes them on its first tick: a row past
`observe_until` with no baseline is `unverifiable` (`the server was not running during the
window`); a row whose rollback was dispatched (`rollback_deployment_id` set) is never
dispatched again. `done` closes between ticks; an in-flight rollback plan/apply runs under
`context.WithoutCancel` like a policy run.

## Authorization and audit

Validation inspections and audit rows act as `system`; the rollback plan and apply act as
the policy's `created_by` with `application.deploy` re-checked (a lapse is
`rollback_outcome = failed`, `creator_lost`, and the policy pauses with that reason).

## API and UI

- `GET .../deployments/{deployment}` and the deployments list gain `validation` (the row
  minus baseline) when one exists.
- `GET .../update-policy` runs gain `validation` as above.
- Deployment detail and the policy run list show the verdict text (`VALIDATION_VERDICTS`
  table), the rollback line ("Rollback sent to revision N" linking the rollback deployment by
  ID prefix, or the ineligible reason from a fixed table), and the pause reason on the card.
- Endpoint capability `container.inspect.health` missing → the policy card warns that
  automated updates on this host cannot be validated and will pause.

## Documents

`docs/agent-protocol.md` (inspection fields, capability), `docs/application-schema.md`
(validation, rollback eligibility, "Rollback honesty" updated for the automated case),
`docs/threat-model.md` (a validation never rolls back a manual change; `changed` stops),
`README.md`, `KyYard-Implementation-Plan.md` §8 (M7b complete; M8 next), AGENTS.md for
protocol, runtime, agent client, store, api, web, root (cmd/server).

## Tests

Protocol/runtime: `Health`/`RestartCount` decode and validation, capability advertised.
Store (SQLite + PostgreSQL): the settle-time row for automated, manual and rollback applies;
every verdict rule on synthetic observations; `RollbackTarget` eligibility for each reason;
`PinImages` planning bypasses tag resolution and refuses an absent image; oscillation guard.
API/loop (fake agent socket, `newPlanHost`): healthy path → `healthy`, no pause; `unhealthy`
after grace → rollback frame dispatched pinning the prior image IDs, policy paused with the
reason; prior images gone → `ineligible`, paused; manual apply → verdict only; offline agent
→ `unverifiable`, paused; restart mid-window resumes and does not re-dispatch; shutdown waits
for an in-flight rollback. Web: verdict and rollback rendering, capability warning.

## Out of scope

HTTP probes, per-service policies, rolling back manual applies, retention of validations
beyond the deployment row's own (cascade), notifications beyond the UI.
