# Bookkeeping (PR D2)

The last three items carried in `KyYard-Implementation-Plan.md` §8 and three review carry-overs.
None changes what a deployment does; each makes what happened legible, atomic, or bounded.

Decisions recorded 2026-09-24 (Yoshi): step outcomes carry a closed code with a per-code
validated detail; the plan's request ID becomes the deployment's correlation ID for apply and
settle; a database holding case-variant duplicate usernames refuses to start and names them.

## 1. Closed step-detail vocabulary

`protocol.DeploymentStep` gains `Code string` (`json:"code"`); `Detail` stays but is a
parameter, not a sentence. `protocol.DeploymentResult` gains `Code` the same way for the
result-level outcome. Both sides validate; the web renders from the code.

### Step codes

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

The per-code detail shape is validated: `unsupported` requires one to thirty-two distinct
`UnsupportedCodes` entries; `identity_unreadable`/`identity_unverified` require a full Docker
ID; `runtime_status` requires `[1-5][0-9]{2}`; every other code requires an empty detail.
`MaxDeploymentStepDetailBytes` stays the outer bound. A step with outcome `succeeded` or
`skipped` carries no code and no detail. A `denied` or `failed` step must carry a code.

### Result codes

| Code | Emitted when |
|---|---|
| `step_failed` | a step did not succeed; the steps say which |
| `clock_skew` | `issued_at` is more than `MaxClockSkew` from the agent's clock |
| `invalid_request` | the frame failed `Validate` |
| `wrong_endpoint` | the frame names another endpoint |
| `busy` | the agent is already applying a deployment |
| `restarted` | the agent restarted after replacement began |
| `unreadable` | the runtime returned a result the agent could not validate |

A result with outcome `succeeded` carries no code. Any other outcome carries one; the free
`Detail` on the result is removed from the wire (`json:"-"` is not enough: the field goes).
`storedDeploymentResult` keeps codes, not sentences.

### Compatibility

A result from an agent built before this change has no codes. The server normalises a
missing code on a non-succeeding step or result to `legacy` and drops its detail, so an
in-flight deployment applied before the upgrade still settles; `legacy` is in the closed set
but no agent emits it. The web shows "the agent did not classify this outcome; upgrade the
agent" for it.

### Web

`ApplicationDeploymentPlan.tsx` renders every step and result outcome from a
`Record<code, string>` table; the `unsupported` parameter is rendered through the existing
`unsupportedNames`, the status and container ID as inert text. `FIXED_DETAIL_PREFIXES`,
`preconditionExplanation`'s substring matches and `explanationFor`'s `includes` calls go.
An unknown code renders as "unrecognised outcome `<code>`" with the code escaped.

## 2. Audit correlation IDs

- Migration 29 adds `deployments.correlation_id TEXT NOT NULL DEFAULT ''`.
- `PlanDeployment` and `RemoveApplication`'s plan path store `a.CorrelationID` (the request's
  `X-Request-ID`) on the row. The plan's audit row already carries it through `t.run`.
- `ApplyDeployment`, `SettleDeployment`, `AbandonDeployments` and the removal settle write
  their audit rows with the row's `correlation_id`, not a fresh UUID: `auditDeployment` takes
  the ID as a parameter. The apply request's own response header still returns its own ID;
  its audit row is the deployment's.
- `protocol.DeploymentRequest` and `RemovalRequest` gain `RequestID string`
  (`json:"request_id"`, required, the same grammar `Command.RequestID` uses). The agent logs
  it beside the deployment ID at receipt, start and finish, and echoes it in
  `DeploymentResult.RequestID`; `SettleDeployment` refuses a result whose `RequestID` differs
  from the row's (`ErrInvalid`, audited `denied`), which also catches a result replayed
  against a re-planned deployment.
- The deployment API responses and the deployment detail panel show `correlation_id` so an
  operator can search the audit log by it.

## 3. Update plans under the per-application guard

`handlePlanDeployment` takes the same `s.imageChecks` in-flight key (`org/env/app`) as
`handleCheckImageUpdates` for a plan with `update` entries, before the inspections and the
registry slot, and answers 409 `check_in_progress` when a check or another update plan for
that application is running. The stale "4 registry slots" wording in
`KyYard-Implementation-Plan.md` and any doc becomes the real topology: 2 per organization,
8 server-wide.

## 4. Lowercase unique index on usernames

- Migration 30 first runs `SELECT LOWER(username) FROM users GROUP BY LOWER(username)
  HAVING COUNT(*) > 1`. If any row comes back the migration fails with
  `usernames differ only by case: erin, Erin; rename or delete one of each pair before
  upgrading` (every group listed, usernames not IDs) and the server refuses to start;
  nothing is altered. Otherwise it creates `idx_users_username_lower` as
  `UNIQUE (LOWER(username))` with one SQL string for both dialects.
- `CreateUser` keeps mapping the unique violation to `ErrAlreadyExists`. The admin-creation
  pre-check stays for its friendlier message; the index closes its race.
- SSO auto-provision (`provider_handlers.go`, KySignOn) resolves an existing user with the
  case-insensitive lookup before creating one; an `ErrAlreadyExists` on create is answered
  as a refused sign-in naming the conflict, never a second account.
- `README.md` upgrade notes and `docs/RESTORE.md` name the refusal and the fix.

## 5. Row lock on the first admin's status

`CreateOrganizationWithAdmin` reads the admin's `status` with `FOR UPDATE` on PostgreSQL
(the driver-conditional idiom `withTenant` uses), so a concurrent status change waits behind
the membership insert or is seen by it. A PostgreSQL-only test holds the user row locked in
one transaction, starts the creation in another, flips the status to disabled, commits, and
asserts `ErrInvalid`.

## 6. Volume-name minimum length

Declared volume names need two characters: `applicationVolumeName` becomes
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}$`, the importer's refusal text quotes the new grammar, and
`protocol.deploymentVolume` becomes `{1,128}` (a resolved `<project>_<name>` is longer; an
external volume is a bare name). Tests pin that `a` is refused and `ab` accepted at all
three layers.

## Documents

`docs/agent-protocol.md` (step and result codes, `request_id`, compatibility), `README.md`
and `docs/RESTORE.md` (username migration refusal), `docs/application-schema.md` (volume
grammar), `KyYard-Implementation-Plan.md` §8 (the D2 list closes; M7b is next), AGENTS.md
for protocol, runtime, agent client, store, api, web.

## Tests

Protocol: every code's detail rule, missing code on failed step refused, legacy
normalisation is the server's not the protocol's. Runtime (fake Engine): each refusal path
emits its code and exact detail; result codes for clock skew and busy from the deployer.
Store (SQLite + PostgreSQL): correlation ID stored at plan, reused by apply/settle/abandon
audit rows, result with a foreign `request_id` refused; migration 30 refuses duplicates and
names them, then succeeds on a clean database; the `FOR UPDATE` test on PostgreSQL; volume
grammar. API: update plan while a check is in flight → 409; SSO auto-provision against a
case-variant existing user. Web: rendering tables, unknown code, legacy code.

## Out of scope

Audit search UI by correlation ID (the value is shown; searching is the existing audit
screen's filter), M7b policies, any change to what a deployment does.
