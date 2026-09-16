# KyYard authorization matrix

**Status:** draft proposal for review (M3 PR 07). Rows marked *implemented* are enforced and tested on master; the rest are the contract each later slice must land with its API. Handlers check named actions, never role strings.

## Roles

| Role | Scope | Summary |
|---|---|---|
| Platform Administrator | instance | recovery, global settings, identity provider configuration, and explicit audited assumption of a tenant context; no implicit tenant access |
| Organization Administrator | organization | everything inside the organization, including members, secrets and destructive actions |
| Environment Administrator | organization (all environments) | environments, endpoints, applications and their lifecycle; no members, no audit, no secrets management |
| Operator | organization | day-to-day operations: restart, logs, stats, manual updates; no deploy, no exec, no secrets |
| Developer | organization | deploy applications and read logs; no host-level or destructive actions, no exec |
| Read Only | organization | read everything non-secret |

Per-environment grants (`role_bindings`) are not introduced; the handoff allows them only if fixed organization roles prove insufficient.

## Matrix

Columns: OA organization admin, EA environment admin, Op operator, Dev developer, RO read only. Platform administrators appear only in the platform section. **Secret** says whether the response can contain a credential. **Audit** names the result values recorded; every row records actor, organization, environment, target, correlation ID and result.

### Platform (`platform.*`) — platform administrator only

| Action | Notes | Secret | Audit |
|---|---|---|---|
| `platform.admin` | backup, recovery pairing, theme, SCIM/SSO configuration, user administration | backup pairing shows a one-time token | backup actions audited today; a generic `platform.admin` denial row is *planned* |
| `platform.organization.create` / `rename` (*planned*) | creating and renaming organizations; the creator receives no membership | no | success |
| `platform.tenant.assume` | **proposed:** creates a time-boxed (*proposed* 1 hour) organization-administrator membership with `granted_by=platform` and `expires_at`, visible to that organization's administrators in the members list, audited in both platform and organization scope. Expiry is a platform-scope cleanup that deletes the row directly, not through the guarded `RemoveMembership`; if the platform administrator did not add a lasting administrator meanwhile, the organization returns to the state it was in before. This is the repair route for an organization with no active administrator. It is never implicit and never silent. | no | `platform.tenant.assumed`, `platform.tenant.released` |

### Organization and membership (*implemented*)

| Action | OA | EA | Op | Dev | RO | Secret | Audit |
|---|---|---|---|---|---|---|---|
| `organization.read` | ✓ | ✓ | ✓ | ✓ | ✓ | no | success today; see read audit below |
| `organization.members.manage` | ✓ | – | – | – | – | no | success/denied/failure, target = user |
| `organization.audit.read` | ✓ | – | – | – | – | no | success |
| `organization.settings.manage` (*planned*, first typed setting) | ✓ | – | – | – | – | no | success |

Last-administrator protection: membership writes serialize per organization and refuse to leave zero active administrators (*implemented*). External deactivation bypasses this and is repaired by `platform.tenant.assume` (*proposed*).

### Environments (*implemented*)

| Action | OA | EA | Op | Dev | RO |
|---|---|---|---|---|---|
| `environment.read` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `environment.create` / `update` / `delete` | ✓ | ✓ | – | – | – |

Today `environment.delete` is unconditional. From M3 it refuses (*planned*, 409) while the environment holds endpoints that are not revoked or applications that are not removed.

### Endpoints and enrollment (M3)

| Action | OA | EA | Op | Dev | RO | Secret | Audit |
|---|---|---|---|---|---|---|---|
| `endpoint.read` | ✓ | ✓ | ✓ | ✓ | ✓ | no | – (reads of inventory are not audited; list reads are bounded) |
| `endpoint.enroll` (token request, approval, rejection) | ✓ | ✓ | – | – | – | one-time enrollment command | success/failure, target = endpoint |
| `endpoint.update` (rename, notes) | ✓ | ✓ | – | – | – | no | success |
| `endpoint.revoke` | ✓ | ✓ | – | – | – | no | success, closes streams |
| `endpoint.rotate` | agent only, authenticated by its current key | | | | | no | success/failure, both fingerprints |

Agent-side actions (`agent.enroll`, `agent.connect`, `agent.inventory`, `agent.event`, `agent.metrics`, `agent.result`) are authenticated by the endpoint identity and authorized by endpoint state and tenant binding only; they carry no user role. Enrollment and connection outcomes are audited in organization scope with the endpoint as target; inventory, events, metrics and results are not audited.

### Containers, images, networks, volumes (M4–M5)

| Action | OA | EA | Op | Dev | RO | Secret | Audit |
|---|---|---|---|---|---|---|---|
| `container.read` (inspect, stats) | ✓ | ✓ | ✓ | ✓ | ✓ | redacted env/labels | – |
| `container.logs` | ✓ | ✓ | ✓ | ✓ | – | log bodies not stored | session metadata |
| `container.operate` (start, stop, restart, pause) | ✓ | ✓ | ✓ | – | – | no | success/failure/unknown |
| `container.destroy` (remove, prune) | ✓ | ✓ | – | – | – | no | success/failure/unknown, confirmation required |
| `container.exec` | ✓ | – | – | – | – | no | session open/close with target and duration; contents never recorded |
| `image.read` | ✓ | ✓ | ✓ | ✓ | ✓ | no | – |
| `image.pull` | ✓ | ✓ | ✓ | – | – | uses registry credential without revealing it | success/failure |
| `image.destroy` | ✓ | ✓ | – | – | – | no | success/failure |
| `volume.read`, `network.read` | ✓ | ✓ | ✓ | ✓ | ✓ | no | – |
| `volume.destroy` | ✓ | – | – | – | – | no | success/failure, separate explicit confirmation naming data loss |

Unmanaged containers: lifecycle actions above apply by permission; configuration editing requires explicit adoption into an application (`application.adopt`).

### Applications (M6–M7)

| Action | OA | EA | Op | Dev | RO | Secret | Audit |
|---|---|---|---|---|---|---|---|
| `application.read` (desired state, revisions, previews) | ✓ | ✓ | ✓ | ✓ | ✓ | secret references only, never values | – |
| `application.adopt` / `import` | ✓ | ✓ | – | – | – | no | success |
| `application.edit` (create a new revision from desired configuration) | ✓ | ✓ | – | ✓ | – | secret references only | success, target = revision |
| `application.deploy` (preview, deploy approved revision) | ✓ | ✓ | – | ✓ | – | no | success/partial/failure/unknown per step |
| `application.update` (manual image update, 0.1) | ✓ | ✓ | ✓ | – | – | no | success/failure |
| `application.destroy` (remove application, keep data) | ✓ | ✓ | – | – | – | no | success |
| `secret.manage` (create, rotate, delete secret values) | ✓ | – | – | – | – | write-only; values never returned | success, target = secret name |
| `secret.use` (deploy with a secret reference) | implied by `application.deploy` | | | | | no | recorded on the deployment |
| `registry.read` | ✓ | ✓ | ✓ | ✓ | ✓ | no credentials | – |
| `registry.manage` | ✓ | – | – | – | – | write-only credentials | success |
| `registry.use` | implied by `image.pull` and `application.deploy` | | | | | no | on the operation |
| `update_policy.manage` and `maintenance_window.manage` (M7b) | ✓ | ✓ | – | – | – | no | success; scheduler acts as `system:scheduler` with the policy's organization |

## Streams and revocation

- A stream grant is issued at authorization time and lives *proposed* 60 seconds until the stream opens; then the stream's own timeouts apply.
- Any of these closes the stream immediately: membership removal or disable, user disable, session revocation, endpoint revocation, password replacement. The check runs on every heartbeat (*proposed* 30 s) and on every server-side revocation event.
- Cross-organization subscriptions fail closed: an event stream is scoped to one organization and carries no rows from another.

## Audit

**Read audit (proposed change):** master records a `success` row for every successful tenant read, which at fleet scale (inventory lists every few seconds) is the audit-growth threat itself. From M4, successful reads are not audited except `container.logs` and `container.exec` session metadata and any read that reveals a one-time secret; denied reads by members remain audited. The change lands with the first read-heavy API and updates `internal/store/AGENTS.md`.

Every mutating action and every denied attempt by a member is recorded in organization scope with result `success`, `denied`, `failure` or `unknown`. Non-members produce no tenant audit row. Exec sessions and secret-revealing operations (enrollment command, backup pairing token) carry explicit `disclosure=one_time` details. Details never include secrets, request bodies or SQL text.

## Decisions

| Decision | Proposed | Status |
|---|---|---|
| Platform access to tenants | explicit, time-boxed, audited assumption; no blanket access | proposed (plan preference) |
| Repair of an organization with no active administrator | `platform.tenant.assume` | proposed, blocker from PR #10 |
| Successful reads audited | stop from M4, except logs, exec and one-time disclosures | proposed |
| Organization creation | platform administrators, no membership for the creator | proposed |
| Unmanaged containers | lifecycle by permission, configuration edit needs adoption | proposed (plan default) |
| Developer scope | deploy plus logs, no exec, no destructive | proposed |
| Exec | organization administrators only in 0.1 | proposed |
| Per-environment grants | not in 0.1 | proposed |
