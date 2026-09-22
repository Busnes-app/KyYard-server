# Deployment plans

First sub-slice of plan PR 15 (preview and reconcile). A deployment plan is the persisted,
executable preview that `docs/application-schema.md` "Deploy" step 1 describes: it binds every
identity a later apply needs, expires, and serializes per instance. This slice mints and reads
plans. Nothing here sends an agent command, pulls an image, or changes a container.

Decisions recorded 2026-09-22 (Yoshi): plans land before apply; the agent will execute the
supported subset natively against the Engine API, not by packaging `docker compose`.

## What a plan binds

A plan is minted only for an adopted instance whose deployment preflight is clean. "Clean" means
the preflight blockers are exactly `runtime_verification_required` and every service row has no
blocker. That implies: mapping reviewed against the latest revision, every adopted container
assigned, every service mapped, every image resolved to one full image ID from the endpoint's
own inventory, and no published-port overlap. Anything else refuses the plan with the blockers
in the response body. Loosening (creating unmapped services, pulling missing images) is a later
slice; this one never guesses.

Recorded on the row, all checked again at apply time:

| Field | Source | Why |
|---|---|---|
| `instance_id`, `endpoint_id`, `project` | instance | scope and the Compose project name |
| `revision` | `applications.latest_revision` | the encrypted bundle is bound to revision number plus spec digest, so both are part of the approval |
| `spec_digest` | `application_revisions.digest` | |
| `mapping_version` | `application_instances.mapping_version` | a re-mapped instance invalidates the plan |
| `plan` (JSON) | built from preflight | per service: name, image reference, pinned `image_id` (full sha256), `image_digest` (repo digest when inventory reports one, else empty), `container_id` it replaces, that container's `image_id` and `created_unix`, ports, restart, environment secret reference names |
| `expires_at` | `created_at` + 10 min | schema doc proposed default |
| `created_by`, `created_at` | actor | audit |

Secret values are not read, resolved or stored. Only reference names appear in the plan.

## Data

Migration 23, `deployments`:

```
id TEXT PRIMARY KEY
organization_id, environment_id, application_id, instance_id, endpoint_id TEXT NOT NULL
project TEXT NOT NULL
state TEXT NOT NULL CHECK(state IN ('planned'))
revision INTEGER NOT NULL
spec_digest TEXT NOT NULL
mapping_version INTEGER NOT NULL
plan TEXT NOT NULL              -- JSON, at most 64 KiB
created_by TEXT NOT NULL
created_at, expires_at DATETIME NOT NULL
FOREIGN KEY(organization_id,environment_id,application_id) REFERENCES applications(...) ON DELETE CASCADE
UNIQUE(instance_id)
```

- `UNIQUE(instance_id)` is the per-instance serialization row. Minting a new plan deletes the
  previous planned row for that instance in the same transaction. A plan is intent, not history,
  so replacing it loses nothing. The apply slice will add non-terminal states and turn the unique
  constraint into a partial index over live states.
- No foreign key to `application_instances`. Release checks explicitly: a `planned` row whose
  `expires_at` is in the future refuses release with `ErrDeploymentPlanned` (409
  `deployment_planned`). An expired plan does not block release; release deletes the instance's
  plan rows.
- `ON DELETE CASCADE` from `applications`: discarding a draft already requires release first,
  and a plan is not deployed history.
- SQLite backup drill: the table is covered by the store's snapshot recovery test alongside
  `application_instances` and `application_resources`.

## Store

`internal/store/application_deployment.go`:

- `PlanDeployment(ctx, a, app, PlanRequest{InstanceID, MappingVersion, Revision, Confirm}) (*Deployment, error)`
  under `withTenantTarget(ApplicationDeploy, app+"/deployments")`. It locks the mapping
  (`applicationMapping(..., lock=true)`), runs the preflight computation inside the same
  transaction, and refuses with `ErrAdoptionChanged` when the request's instance, mapping
  version, revision or typed project differ from what it read, or with `ErrPreflightBlocked`
  (new, carries the blockers) when the preflight is not clean. It then builds the plan JSON,
  deletes the instance's existing row, inserts the new one, and returns it.
- `ReadDeployment(ctx, a, app, id)` and `ListDeployments(ctx, a, app)` under
  `application.read`; list is newest first, bounded to 100. Responses carry a computed
  `expired` boolean; nothing rewrites state on read.
- `PreflightApplication` is refactored so the transaction body becomes
  `preflight(ctx, tx, a, id, lock)` returning the preflight, mapping, parsed spec, parsed
  snapshot and revision digest, and both callers share it. The public function keeps its contract unchanged.
- `ReleaseApplication` gains the live-plan check and the delete.

`internal/permissions`: `ApplicationDeploy = "application.deploy"` for organization admin,
environment admin and developer, per `docs/authorization-matrix.md`.

## API

- `POST /api/organizations/{o}/environments/{e}/applications/{a}/deployments` body
  `{instance_id, mapping_version, revision, confirm}`; 201 with the plan. 409 for
  `ErrAdoptionChanged`; 409 with `{"error":"preflight_blocked","blockers":[...]}` for
  `ErrPreflightBlocked`; 403 without `application.deploy`. Same CSRF and strict JSON as the
  mapping PUT.
- `GET .../deployments` and `GET .../deployments/{deployment}` under `application.read`.
- No apply route. No agent frame. `tenantError` maps the new error.

## UI

`web/src/components/ApplicationDeploymentPlan.tsx`, rendered below the preflight panel for an
adopted instance:

- On open, loads the instance's current plan (list filtered by instance).
- "Plan deployment" posts the observed instance ID, mapping version, revision and typed project
  name; the confirmation names the application and revision and states that nothing runs.
- Shows the plan: revision, mapping version, expiry countdown from `expires_at`, and a 25-row
  table of service, image reference, pinned image ID, replaced container ID. States that the plan
  is not executed, that it expires, and that runtime configuration remains unverified.
- Blockers from a 409 render as the same fixed messages the preflight uses. Error bodies are
  never rendered. No apply, retry or polling.

## Testing

Store (SQLite and PostgreSQL): a clean preflight mints a plan binding the identities above;
stale instance, mapping version, revision and wrong project refuse; each preflight blocker
refuses with that blocker listed; a second plan replaces the first; a live plan refuses release
and an expired one does not; operator and read-only are forbidden, developer is allowed;
cross-tenant read and write fail closed; the snapshot recovery drill restores the table.

API: the three routes with 201/200/403/409 and CSRF. Web: vitest for load, plan, blockers,
expiry rendering, and redaction of error bodies.

## Out of scope

Agent command, image pull, container create, deployment events, `current_revision`, history,
reapply, removal. Those are the next two slices.

## Documents to update

`docs/application-schema.md` (Deployment plans section; decision table row "Preview validity"
becomes implemented), `docs/authorization-matrix.md` (`application.deploy` status),
`internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md`,
`KyYard-Implementation-Plan.md` section 8.
