# KyYard authorization matrix

**Status:** draft proposal for review (M3 PR 07). Rows marked *implemented* are enforced and tested on master; the rest are the contract each later slice must land with its API. Handlers check named actions, never role strings.

## Roles

| Role | Scope | Summary |
|---|---|---|
| Platform Administrator | instance | recovery, global settings, identity provider configuration, and explicit audited assumption of a tenant context; no implicit tenant access |
| Organization Administrator | organization | everything inside the organization, including members, secrets and destructive actions |
| Environment Administrator | organization (all environments) | environments, endpoints, applications and their lifecycle; no members, no audit, no secrets management |
| Operator | organization | day-to-day operations: restart, logs, stats, host image pulls without credentials; no deploy (so no application updates), no exec, no secrets |
| Developer | organization | deploy applications and read logs; no host-level or destructive actions, no exec |
| Read Only | organization | read everything non-secret |

The first platform administrator comes from first start (the bootstrap `admin`) or `kyyard-server init-admin`, which also resets an existing account's password; neither needs a signed-in administrator. Every later local account and organization comes from Settings → Administration. Disabling or deleting accounts and organizations, and administrator password resets for other accounts, have no UI or API yet.

Per-environment grants (`role_bindings`) are not introduced; the handoff allows them only if fixed organization roles prove insufficient.

## Matrix

Columns: OA organization admin, EA environment admin, Op operator, Dev developer, RO read only. Platform administrators appear only in the platform section. **Secret** says whether the response can contain a credential. **Audit** names the result values recorded; every row records actor, organization, environment, target, correlation ID and result.

### Platform (`platform.*`) — platform administrator only

| Action | Notes | Secret | Audit |
|---|---|---|---|
| `platform.admin` | backup, recovery pairing, theme, SCIM/SSO configuration, user administration | backup pairing shows a one-time token | backup actions audited today; a generic `platform.admin` denial row is *planned* |
| `platform.admin` — create an organization (*implemented*, `POST /api/admin/organizations`, Settings → Administration) | name plus a first administrator chosen from active users; the organization and that user's active `organization_admin` membership commit in one transaction, so no organization exists without an administrator. The creator gets a membership only by naming themself. Renaming is *planned* | no | platform scope `organization.create`: success with `admin=<user id>`; a refused create (name taken, user missing or inactive) is `failure` with `refused=<code>` |
| `platform.admin` — create a local account (*implemented*, `POST /api/admin/users`, Settings → Administration) | platform role `user` or `admin`; the server generates a 24-character temporary password, returns it once in the create response and never again; the account must change it at first sign-in. Organization access comes only from membership, which an organization administrator grants on the Members page | the temporary password, once | platform scope `user.create` with `role=<role>`, never the password; a taken username is `failure` |
| `platform.tenant.assume` | **proposed:** creates a time-boxed (*proposed* 1 hour) organization-administrator membership with `granted_by=platform` and `expires_at`, visible to that organization's administrators in the members list, audited in both platform and organization scope. The time box is enforced at authorization time: the membership lookup inside `withTenant`/`readTenant` and every other `TenantAccess` resolution treats a row whose `expires_at` has passed as absent, and the members list renders it as expired from the same predicate; the cleanup delete is housekeeping only. Platform-granted rows never count toward the active-administrator quorum (the quorum query filters `granted_by`), so the last lasting administrator cannot be demoted or removed behind a temporary grant, and expiry cleanup re-checks the quorum and warns the platform administrator instead of leaving zero lasting administrators. This is the repair route for an organization with no active administrator. It is never implicit and never silent. | no | `platform.tenant.assumed`, `platform.tenant.released` |

### Organization and membership (*implemented*)

| Action | OA | EA | Op | Dev | RO | Secret | Audit |
|---|---|---|---|---|---|---|---|
| `organization.read` | ✓ | ✓ | ✓ | ✓ | ✓ | no | success today; see read audit below |
| `organization.members.manage` | ✓ | – | – | – | – | no | success/denied/failure, target = user |
| `organization.audit.read` | ✓ | – | – | – | – | no | success |
| `organization.settings.manage` (*planned*, first typed setting) | ✓ | – | – | – | – | no | success |

Last-administrator protection: membership writes serialize per organization and refuse to leave zero active administrators (*implemented*). Only lasting memberships count; platform-granted ones do not (*proposed*, lands with `platform.tenant.assume`). External deactivation bypasses this and is repaired by `platform.tenant.assume` (*proposed*).

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
| `endpoint.enroll` (token request, approval, rejection) | ✓ | ✓ | – | – | – | one-time enrollment command, or for a Kubernetes cluster the one-time manifest (the token only inside its enrollment Secret, returned only in this authenticated response); also the cluster's deploy namespaces (`POST .../endpoints/{endpoint}/manifest`, answering an RBAC-only manifest with no link) | success/failure, target = endpoint; the manifest route `<endpoint>/manifest` with the namespace list |
| `endpoint.update` (rename, notes) | ✓ | ✓ | – | – | – | no | success |
| `endpoint.revoke` | ✓ | ✓ | – | – | – | no | success, closes streams |
| `endpoint.rotate` | agent only, authenticated by its current key | | | | | no | success/failure, both fingerprints |

Agent-side actions (`agent.enroll`, `agent.connect`, `agent.inventory`, `agent.event`, `agent.metrics`, `agent.result`) are authenticated by the endpoint identity and authorized by endpoint state and tenant binding only; they carry no user role. Enrollment and connection outcomes are audited in organization scope with the endpoint as target; inventory, events, metrics and results are not audited.

### Containers, images, networks, volumes (M4–M5)

| Action | OA | EA | Op | Dev | RO | Secret | Audit |
|---|---|---|---|---|---|---|---|
| `container.read` (inspect, stats) | ✓ | ✓ | ✓ | ✓ | ✓ | redacted env/labels | – |
| `container.logs` (implemented) | ✓ | ✓ | ✓ | ✓ | – | log bodies not stored; also covers pod logs on a Kubernetes endpoint (read workload logs) | session metadata: one row per session naming the endpoint and the container, or the pod and container, asked for |
| `container.operate` (start, stop, restart, pause) | ✓ | ✓ | ✓ | – | – | no | success/failure/unknown |
| `container.destroy` (remove, prune) | ✓ | ✓ | – | – | – | no | success/failure/unknown, confirmation required |
| `container.exec` (implemented) | ✓ | – | – | – | – | no | session open/close with target and duration; contents never recorded |
| `image.read` | ✓ | ✓ | ✓ | ✓ | ✓ | no | – |
| `image.pull` (endpoint image controls; operators keep it for host-level pulls) | ✓ | ✓ | ✓ | – | – | sends no registry credential: a registry that needs one is refused (`credentialsMissing`); credentialed pulls go only through a deployment under `application.deploy` | success/failure |
| `image.destroy` | ✓ | ✓ | – | – | – | no | success/failure |
| `volume.read`, `network.read` | ✓ | ✓ | ✓ | ✓ | ✓ | no | – |
| `volume.destroy` | ✓ | – | – | – | – | no | success/failure, separate explicit confirmation naming data loss |

Unmanaged containers: lifecycle actions above apply by permission; configuration editing requires explicit adoption into an application (`application.adopt`).

### Applications (M6–M7)

| Action | OA | EA | Op | Dev | RO | Secret | Audit |
|---|---|---|---|---|---|---|---|
| `application.read` (desired state, revisions, previews) | ✓ | ✓ | ✓ | ✓ | ✓ | secret references only, never values | – |
| `application.release` (release recorded ownership only) | ✓ | ✓ | – | – | – | no | success, exact instance |
| `application.adopt` / `import` | ✓ | ✓ | – | – | – | no | success |
| `application.edit` (create a new revision from desired configuration) | ✓ | ✓ | – | ✓ | – | secret references only | success, target = revision |
| `application.deploy` (preview, plan and apply an approved revision; implemented) | ✓ | ✓ | – | ✓ | – | no | audited by `SettleDeployment` in the settle transaction, result mapped to success/denied/failure/unknown |
| `application.deploy` — checks image updates (`CheckImageUpdateAccess`/`CheckImageUpdates`, target `<app>/updates`; implemented) | ✓ | ✓ | – | ✓ | – | registry credential used internally, never returned | denials and failures on `<app>/updates`; one success row per completed check, details `services=N updates=N errors=N` |
| `application.deploy` — manual image update (plan with `update`, then apply; implemented) | ✓ | ✓ | – | ✓ | – | registry credential decrypted at plan (`Head`) and at apply (frame assembly), never returned or stored | plan success row on `<app>/deployments/<id>` with `pulls=N`; apply audited by `SettleDeployment` |
| `application.destroy` (remove application, keep data; implemented, `POST .../applications/{application}/removal`) | ✓ | ✓ | – | – | – | no | one row per removal (`withTenantTarget`); settle audits the same action for the agent's result, `outcome=abandoned/swept/not_sent` for a disconnect, sweep or undeliverable frame |
| `secret.reveal` (internal imported-value resolution) | ✓ | – | – | – | – | plaintext, only after audit commits | success/failure |
| `secret.manage` (create, rotate, delete secret values) | ✓ | – | – | – | – | write-only; values never returned | success, target = secret name |
| `secret.use` (deploy with a secret reference) | implied by `application.deploy` | | | | | no | recorded on the deployment |
| `registry.read` (list registries, read the anonymous-pull policy; implemented) | ✓ | ✓ | ✓ | ✓ | ✓ | no credentials; rows carry `has_credential` | denials only |
| `registry.manage` (create/update by host, delete; implemented) | ✓ | – | – | – | – | write-only credentials | target `registries/<id>`; put details `host=<h> allow_private=<bool> credential=set\|kept\|cleared`, delete details `host=<h>` |
| `registry.manage` — anonymous-pull opt-in (implemented) | ✓ | – | – | – | – | no | target `registry-policy`, details `old=<bool> new=<bool>`; per organization, off by default |
| Registry credential use (implemented as `ResolveRegistryAccess`) | the operation's own permission (`image.pull` or `application.deploy`, an allow-list); no separate permission, and no read permission unlocks it | | | | | internal only, never returned | denials of that operation's action; the operation audits its own result |
| `application.policy` (create, edit, delete, resume an update policy; implemented, M7b) | ✓ | – | – | – | – | no | success and failure on `<app>/policies/<policy>` (resume on `<app>/policies/<policy>/resume`); a refusal before the policy is resolved on `<app>/policies`; reading a policy and its runs is `application.read` |
| `application.migrate` (analyze for a cluster, storage choices, create the destination, confirm validation and cutover, abandon; implemented, M8) | ✓ | – | – | – | – | the destination receives a re-sealed copy of the source's values, never shown | success and failure on `<app>/migration/<id>` (a refusal before the migration is resolved on `<app>/migration`), and a second success row on `<destination>/migration/<id>` when the destination is created; reading a migration is `application.read`, while choices and analyze read the open migration under `application.migrate`, so any other role is refused with a denied row before it learns whether one exists |
| Update-policy runs (implemented, M7b) | the scheduler acts as the policy's `created_by` with `application.deploy` re-checked on every step | | | | | registry credential as for a manual update | the run's check, plan and apply rows under the creator and one correlation ID; `application.policy.run` and `application.policy.paused` as `system` |
| Health validation (implemented, M7b; clusters, M8) | no new action: the loop inspects as the wire identifier `system-validation` (no user, no session) through the plan-time inspection, a Docker container or a cluster's Deployment and pods alike; a rollback acts as the policy's `created_by` with `application.deploy` re-checked | | | | | none read; a cluster status read carries no image, message or value | `application.validation` as `system` on `<app>/deployments/<id>`; a rollback's plan and apply rows under the creator |

## Streams and revocation

- A stream grant is issued at authorization time and lives *proposed* 60 seconds until the stream opens; then the stream's own timeouts apply.
- Any of these closes the stream immediately: membership removal or disable, user disable, session revocation, endpoint revocation, password replacement. Exec checks every 250 ms under a 500 ms deadline as well as on server-side membership/endpoint/shutdown events; session revocation and external account changes therefore fail closed within the one-second acceptance bound under normal scheduling. Log streams retain their separate recheck cadence.
- Cross-organization subscriptions fail closed: an event stream is scoped to one organization and carries no rows from another.

## Audit

**Read audit (proposed change):** master records a `success` row for every successful tenant read, which at fleet scale (inventory lists every few seconds) is the audit-growth threat itself. From M4, successful reads are not audited except `container.logs` (implemented: `auditedReads` in `internal/store/tenant_access.go`) and `container.exec` session metadata and any read that reveals a one-time secret; denied reads by members remain audited. The change lands with the first read-heavy API and updates `internal/store/AGENTS.md`.

Every mutating action and every denied attempt by a member is recorded in organization scope with result `success`, `denied`, `failure` or `unknown`. Non-members produce no tenant audit row. Exec sessions and secret-revealing operations (enrollment command, backup pairing token) carry explicit `disclosure=one_time` details. Details never include secrets, request bodies or SQL text.

## Decisions

| Decision | Proposed | Status |
|---|---|---|
| Platform access to tenants | explicit, time-boxed, audited assumption; no blanket access | proposed (plan preference) |
| Repair of an organization with no active administrator | `platform.tenant.assume` | proposed, blocker from PR #10 |
| Successful reads audited | stopped with the inventory API (M4) except `organization.audit.read` and `organization.members.manage`, which keep a success row; denials always audited | implemented |
| Platform grant expiry | enforced in the membership lookup, not by cleanup; excluded from the administrator quorum | proposed |
| Organization creation | platform administrators; a first organization administrator is seeded in the same transaction; the creator gets no membership unless named | implemented |
| Unmanaged containers | lifecycle by permission, configuration edit needs adoption | proposed (plan default) |
| Developer scope | deploy plus logs, no exec, no destructive | proposed |
| Exec | organization administrators only in 0.1; the UI offers Terminal only to them | implemented |
| Per-environment grants | not in 0.1 | proposed |

Application persistence implements `application.read`, `application.import`, `application.edit` and `application.destroy` through authorized store operations. Import creates an application and first revision; edit appends a revision using the expected head. `application.destroy` covers two operations: discarding an undeployed draft (and a removed application's history) at the expected head, which releases quota, and `RemoveApplication`, which stops and removes an adopted application's containers through `deployment.remove` and keeps its data. HTTP import/list/read/discard/removal routes are implemented; explicit adoption/release are implemented. Internal `secret.reveal` permits organization administrators only, commits audit before returning values and has no HTTP endpoint. Every operation requires explicit environment scope; successful edits audit the revision target without configuration.

### Live redacted container inspection

The inspection GET uses implemented endpoint.read for every active membership role. It exposes bounded operational facts/counts, not environment values, labels, argv, mount paths or network names. Session and tenant access are rechecked while pending and at publication; runtime identity comes from fresh scoped inventory. No successful-read audit or deployment/secret authority is granted. Bounds and wire lifecycle are in agent-protocol.md, Container inspection.
