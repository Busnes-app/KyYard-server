**Repo:** Busnes-app/KyYard-server
**PR:** #33 — https://github.com/Busnes-app/KyYard-server/pull/33
**Worktree:** /home/yoshi/busness.app/KyYard-Server/.worktrees/product-ui (branch fix/product-ui)

# KyYard Engineering Handoff

> **KyYard — The simple control plane for your container fleet.**

**Status:** Product and implementation handoff  
**Starting repository:** `https://github.com/Busnes-app/ky_server_base`  
**Reviewed base revision:** `f4ca19a`  
**Primary implementation language:** Go backend; React 19 + TypeScript frontend  

## 1. Product decision

KyYard is a self-hosted container operations platform for organizations that operate standalone Docker hosts, Docker Compose applications, Kubernetes clusters, or a mixture of them.

Docker and Kubernetes are permanent peers. Docker is not a temporary beginner mode, and Kubernetes is not required to use the product. KyYard provides a common operational view where that is honest and preserves runtime-specific controls where Docker and Kubernetes differ.

KyYard is not intended to reproduce every Portainer feature. The product should cover the routine 80% of container operations with an unusually low setup burden and a credible path for moving suitable workloads between runtimes.

### Product promise

1. Start the KyYard container.
2. Read a one-time administrator credential from its logs.
3. Sign in and change the password.
4. Enroll a Docker host or Kubernetes cluster with one generated command.
5. Operate containers without first configuring a database, cache, reverse proxy, identity provider, or large environment file.

Environment variables remain available for unattended and advanced deployments, but they are overrides—not the primary configuration interface.

## 2. Non-negotiable principles

- Docker remains first-class forever.
- Kubernetes remains optional forever.
- A basic installation requires one container and one persistent data volume.
- Basic production startup requires no hand-authored secrets or configuration.
- Local authentication always remains available; SSO is additive.
- Organizations, tenant isolation, and authorization are designed before workload APIs.
- Adding an endpoint is a one-command, approval-based enrollment flow.
- Common workflows must be discoverable without documentation.
- KyYard stores desired application state rather than acting only as a remote Docker GUI.
- Runtime migration is analyzed, previewed, and validated; it is never advertised as universally automatic.
- KyYard manages container workloads. It does not initially provision servers or Kubernetes clusters, replace Terraform/OpenTofu, build images, become a CI system, or replace a full observability platform.
- Sensitive or destructive actions are authorized, auditable, and scoped to an organization and environment.

## 3. Starting-point assessment

`ky_server_base` is the correct foundation and should be scaffolded into a new product repository rather than rewritten.

### Reuse unchanged or with narrow extensions

- Single Go server and embedded React PWA deployment model.
- SQLite as the zero-configuration default and PostgreSQL as the scale-out database option.
- Migration framework and pluggable store interface.
- Bootstrap administrator creation and forced password change.
- Session authentication, CSRF protection, MFA, rate limiting, and security headers.
- Configurable OAuth 2/OIDC sign-in, including KyIdentity; local recovery login stays available. SCIM and mobile pairing are excluded by user decision.
- Audit logging foundation.
- KyBackup/KyRecovery integration and restore-drill workflow.
- Docker image, Compose packaging, smoke tests, race tests, frontend tests, vulnerability checks, and attested-image publishing pattern.
- KySecurity themes and the embedded-frontend build pipeline.

### Correct before building broad product features

1. **Production secret bootstrap:** The base currently refuses production startup when `KY_SESSION_SECRET` is unset. KyYard must generate and persist this secret under its data directory on first boot, as it already does for the encryption key. An environment-provided value may override it.
2. **Tenant model:** Users currently have a single global role and groups are global. Add organizations and memberships before creating environment or workload APIs.
3. **Authorization:** Replace direct `user.Role == "admin"` decisions for new product endpoints with organization-scoped permission checks. Retain a narrowly defined platform-admin role for instance recovery and global administration.
4. **Configuration:** Move optional product configuration into persistent typed settings exposed through the UI/API. Do not expand the environment-variable surface for ordinary features.
5. **Audit scope:** Add organization, environment, actor, target, request/correlation, and result fields. Container exec sessions and secret-revealing operations require particularly clear audit records.
6. **Frontend structure:** The current tab switcher is suitable for the scaffold but not the product. Introduce route-based navigation and organization/environment context before the UI grows.
7. **Agent enrollment:** Use dedicated endpoint identities and enrollment tokens. Mobile/phone pairing is excluded.

## 4. Product vocabulary

Use these terms consistently in code, API responses, UI, and documentation:

| Term | Meaning |
|---|---|
| Organization | Tenant and primary authorization boundary |
| Environment | An operator-defined grouping such as Production, Development, or Customer A |
| Endpoint | One connected Docker host or Kubernetes cluster |
| Agent | The small process that inventories an endpoint and executes authorized operations |
| Application | KyYard's desired-state definition for an operator-managed workload |
| Instance | A runtime realization of an application on an endpoint |
| Deployment | A versioned attempt to reconcile an application instance to desired state |
| Resource | A runtime-native container, image, volume, network, pod, deployment, service, and similar object |
| Registry | An image registry configuration and its scoped credentials |
| Update policy | Rules governing image detection, approval, scheduling, health checks, and rollback |

Do not call endpoints “devices”; that term already belongs to user/PWA pairing in the base.

## 5. Target architecture

### Control plane

The KyYard server owns:

- Web UI and public API.
- Authentication, SSO, organizations, membership, and RBAC.
- Endpoint inventory and health.
- Desired application state and deployment history.
- Registry metadata and encrypted credentials.
- Update policies and maintenance windows.
- Migration analysis and generated runtime plans.
- Monitoring summaries, events, and audit records.
- Agent enrollment, identity, revocation, and upgrade policy.

### Endpoint agent

The agent should remain intentionally narrow. It:

- Initiates an outbound encrypted connection to the control plane.
- Authenticates with a unique endpoint identity created during enrollment.
- Reports capabilities, runtime version, inventory, events, health, and bounded metrics.
- Executes authorized, versioned commands and returns structured results.
- Streams logs and terminal traffic without making authorization decisions.
- Does not store tenant policy, users, global desired state, registry passwords, or long-lived enrollment tokens.

Use one agent binary with runtime adapters unless a concrete platform constraint proves that separate Docker and Kubernetes agents are necessary.

### Runtime adapters

Define narrow interfaces rather than allowing Docker or Kubernetes SDK types to leak into the control-plane domain.

- `runtime/docker`: Docker Engine inventory and actions; Compose discovery and deployment.
- `runtime/kubernetes`: cluster inventory and the supported workload subset.
- `agent/protocol`: transport envelopes, capability negotiation, commands, results, and stream setup.
- `applications`: desired-state model, validation, revisions, and reconciliation requests.
- `migration`: source inspection, compatibility findings, target mapping, and preview.

Runtime-native resources remain accessible. The common application model must not erase capabilities or pretend unlike constructs are identical.

## 6. Initial data model

Create dedicated records and atomic database constraints. Do not overload user groups, settings, or device-pairing tables.

### Identity and tenancy

- `organizations`
- `organization_memberships` with role and status
- `organization_groups` or scoped extensions to the existing group model
- `organization_group_members`
- `role_bindings` if fixed membership roles prove insufficient

Initial roles:

- Platform Administrator
- Organization Administrator
- Environment Administrator
- Operator
- Developer
- Read Only

Permissions should be named actions such as `endpoint.read`, `container.exec`, `application.deploy`, `secret.manage`, and `organization.members.manage`. Roles map to permissions; handlers check permissions, not role strings.

### Fleet and workload

- `environments`
- `endpoints`
- `endpoint_agents`
- `agent_enrollment_tokens`
- `endpoint_capabilities`
- `applications`
- `application_revisions`
- `application_instances`
- `deployments`
- `deployment_events`
- `registries`
- `registry_credentials`
- `update_policies`
- `maintenance_windows`

Every tenant-owned row carries `organization_id`. Environment-owned rows also carry `environment_id`. Uniqueness must be enforced atomically at the database layer, including endpoint names within an organization, application names within an environment, and one-time token consumption.

### Agent lifecycle

Use explicit states:

`pending -> approved -> active -> offline -> revoked`

Enrollment tokens are short-lived, single-use, stored as hashes, scoped to one organization and intended endpoint type, and consumed atomically. The agent exchanges the token for a permanent cryptographic identity. A token must never become the permanent credential or remain necessary after enrollment.

## 7. First-run experience

The default deployment should resemble:

```yaml
services:
  kyyard:
    image: ghcr.io/busnes-app/kyyard:latest
    restart: unless-stopped
    ports:
      - "9443:9443"
    volumes:
      - kyyard-data:/data

volumes:
  kyyard-data:
```

On first boot, KyYard:

1. Creates its SQLite database.
2. Creates or loads encryption, session-signing, and instance-identity keys in `/data` with restrictive permissions.
3. Applies migrations.
4. Creates the initial organization and local platform administrator.
5. Prints the URL, username, and one-time password once to the logs.
6. Requires password replacement at first login.
7. Starts with SSO, external database, TLS customization, and remote agents unconfigured but available through guided UI flows.

The shipped container must have a useful health check. If automatic locally trusted TLS cannot be delivered honestly, bind HTTP by default and make the UI explicitly guide TLS/reverse-proxy configuration; do not silently ship a misleading self-signed “secure” experience.

## 8. Enrollment flow

### Docker endpoint

1. User selects **Endpoints -> Add endpoint -> Docker**.
2. KyYard creates a short-lived, single-use enrollment token.
3. The UI renders one `docker run` command containing the control-plane URL and enrollment token.
4. The agent connects outbound, presents host facts, and becomes `pending`.
5. An authorized user reviews hostname, OS, Docker version, CPU, memory, and agent fingerprint.
6. Approval issues the permanent agent identity; rejection invalidates the request.
7. Initial inventory begins and the endpoint becomes `active`.

The Docker socket grants host-equivalent power. The UI and installation text must state this plainly. Support a socket proxy or other reduced-capability connector later, but do not imply that mounting `/var/run/docker.sock` is narrowly sandboxed.

### Kubernetes endpoint

Follow the same control-plane workflow, but generate a manifest or Helm installation only after the Docker slice proves the enrollment protocol. Kubernetes credentials and RBAC should be bounded to the capabilities KyYard claims to support.

## 9. Version 0.1 scope

Version 0.1 replaces everyday single-host Portainer use for KyYard's own Docker environments and proves the fleet/control-plane architecture.

### Included

- Zero-config first boot with persistent generated secrets.
- Organizations, memberships, initial roles, and scoped authorization.
- One-command Docker agent enrollment and approval.
- Multiple Docker endpoints grouped into environments.
- Endpoint health and inventory.
- Containers: list, inspect, start, stop, restart, remove with confirmation.
- Images: list, pull, remove with dependency checks.
- Networks and volumes: list and inspect.
- Container logs with follow, timestamps, search/filter, and download.
- Browser terminal/exec with explicit authorization, idle timeout, disconnect handling, and audit trail.
- Basic CPU, memory, network, and restart statistics with bounded retention.
- Compose applications: discover, view configuration, deploy, update, and remove with previews.
- Manual image update detection and controlled recreate.
- Audit log for control-plane and endpoint actions.
- KyBackup coverage for every new control-plane table and encrypted secret.

### Deferred

- Kubernetes adapter.
- Automated update policies and maintenance windows.
- Automated rollback based on health checks.
- Registry-to-registry image copying.
- Docker-to-Kubernetes migration analyzer.
- Advanced metrics retention and alerting.
- Infrastructure provisioning.
- Arbitrary Kubernetes CRD editing.

## 10. Delivery sequence

### Milestone 0 — Scaffold and preserve the baseline

- Run `scripts/ky-init.sh kyyard-server <target>` from the reviewed base.
- Rename module paths, package metadata, binary, images, container names, UI title, defaults, and recovery application identity.
- Replace base implementation-plan text with KyYard's plan while retaining completed inherited capabilities in project documentation.
- Update the DOX/`AGENTS.md` hierarchy for the new product boundaries.
- Run `make ci` before feature work. The scaffolded baseline must pass unchanged.

**Exit:** The renamed KyYard image builds, starts, prints a bootstrap credential, serves the embedded UI, and passes the inherited test suite.

### Milestone 1 — Production-safe zero configuration

- Persistently generate the session secret and instance identity at first boot.
- Define permissions and protect the data directory against unsafe modes.
- Reduce the default Compose file to the one-container experience.
- Keep PostgreSQL, SSO, proxy trust, and recovery customization optional.
- Add a clean readiness/health endpoint that does not disclose secrets.

**Exit:** A fresh named volume and one `docker compose up -d` are sufficient for a production-mode local installation.

### Milestone 2 — Tenant foundation

- Add organization, membership, environment, and permission models and migrations.
- Place the bootstrap administrator in the initial organization.
- Make organization/environment context explicit in API routes.
- Scope audit and settings access appropriately.
- Add isolation tests that attempt cross-organization reads and writes for every new store.

**Exit:** No endpoint or application can be created outside an organization, and cross-tenant access tests fail closed under SQLite and PostgreSQL.

### Milestone 3 — Agent protocol and enrollment

- Specify the versioned protocol before implementing transport.
- Implement enrollment-token creation, expiration, atomic consumption, approval, identity issuance, reconnect, rotation, and revocation.
- Add agent capability negotiation and heartbeat.
- Scaffold the single agent binary and Docker adapter.
- Threat-model replay, impersonation, control-plane compromise, agent compromise, stale commands, and log/terminal streams.

**Exit:** Two Docker hosts can enroll, reconnect using permanent identities, report health, and be independently revoked without restarting the control plane.

### Milestone 4 — Read-only Docker fleet

- Inventory engine facts, containers, images, networks, and volumes.
- Add endpoint and resource APIs and UI.
- Store current summaries while treating the Docker Engine as authoritative for runtime-native state.
- Bound event and metric retention.

**Exit:** An operator can inspect multiple hosts from one UI, and loss/reconnection of an agent produces correct endpoint status without corrupting inventory.

### Milestone 5 — Docker operations

- Add typed commands with idempotency keys, deadlines, expected resource versions, and structured results.
- Implement lifecycle actions, pulls, logs, and audited exec sessions.
- Separate read, operate, deploy, destructive, exec, and secret permissions.
- Require previews and clear target identity for destructive operations.

**Exit:** KyYard safely replaces routine Portainer operations on the project's own Docker hosts.

### Milestone 6 — Applications and Compose desired state

- Add versioned application definitions and deployment records.
- Import/discover Compose projects without silently taking ownership.
- Preview configuration diffs before reconcile.
- Record the exact prior definition and runtime result for rollback/manual recovery.

**Exit:** A Compose application can be imported, changed, previewed, deployed, observed, and returned to its prior definition.

### Milestone 7 — Updates

- Detect available image changes without deploying them.
- Add manual approval and recreate first.
- Add policies, maintenance windows, health checks, and rollback only after manual deployment semantics are proven.

**Exit:** A failed eligible update returns to the recorded prior image/configuration and leaves an intelligible audit/deployment history.

### Milestone 8 — Kubernetes and migration

- Add the Kubernetes agent adapter for a deliberately bounded resource set.
- Map the common application model to Deployments/StatefulSets, Services, ConfigMaps, Secrets, and PVCs.
- Build a migration analyzer that reports blockers and required operator choices for storage, networking, secrets, health probes, resources, and scheduling.

**Exit:** A stateless reference application can be previewed and deployed on Docker or Kubernetes from the same application definition. Unsupported stateful cases are blocked with actionable explanations.

## 11. First implementation slice

The first pull request should be a scaffold-only change. Do not combine renaming with tenancy or agent work.

Expected contents:

- New `kyyard-server` repository scaffolded from `ky_server_base`.
- Go module `github.com/Busnes-app/kyyard-server`.
- Product display name `KyYard`.
- Image `ghcr.io/busnes-app/kyyard`.
- Container name `kyyard`.
- Persistent paths standardized under `/data` inside the product container.
- Existing behavior and test coverage preserved.
- Root `AGENTS.md`, README, Compose files, workflow publication names, package metadata, UI title, recovery identity, and smoke-test expectations updated consistently.
- A deliberate audit for lingering `ky_server_base`, `Busnes.app Base`, and old image/module references.

Suggested verification:

```bash
rg -n 'ky_server_base|Busnes\.app Base|ghcr\.io/busnes-app/ky_server_base' . \
  --glob '!web/dist/**' --glob '!.git/**'
make ci
docker compose config
docker compose up -d
docker compose logs kyyard
```

Any remaining old-name matches must be justified as historical references. Generated `web/dist` must be rebuilt and committed according to the base repository contract.

## 12. Required design documents before agent code

Produce and review these short documents before implementing remote execution:

1. **Agent protocol specification:** message envelope, version negotiation, commands, results, errors, deadlines, deduplication, heartbeats, streams, and compatibility policy.
2. **Threat model:** assets, trust boundaries, enrollment, agent identity, Docker-socket consequences, terminal access, registry secrets, tenant isolation, revocation, and control-plane recovery.
3. **Authorization matrix:** roles against every API action, including logs, exec, secrets, destructive operations, enrollment, and audit access.
4. **Desired-state schema:** common fields, Docker-only extensions, Kubernetes-only extensions, revisioning, ownership, import behavior, and rollback guarantees.
5. **Retention policy:** metrics, events, logs, deployment history, terminal metadata, and audit records.

## 13. Engineering rules

- Preserve the base repository's DOX workflow: read the applicable `AGENTS.md` chain before editing and update the closest owning documentation after behavioral or structural changes.
- Every new domain gets a narrow store interface and tests against SQLite and PostgreSQL.
- Database migrations must be forward-only, transactional where supported, and safe against partially initialized installations.
- API input and output types are product types, never raw Docker/Kubernetes SDK structures.
- All mutations use explicit HTTP methods, CSRF protection for browser sessions, organization-scoped authorization, structured audit results, and stable error codes.
- Agent commands include organization, endpoint, request ID, command ID, deadline, and expected capability/protocol version.
- Retryable commands must be idempotent. Destructive commands are not automatically retried without a proven idempotency contract.
- Secrets are encrypted at rest and redacted from API responses, logs, events, diffs, and audit details.
- Do not retain unbounded container logs, event streams, or high-cardinality metrics in the control-plane database.
- Frontend actions must always show the selected organization, environment, endpoint, and resource before a destructive confirmation.
- Accessibility, keyboard navigation, narrow-screen use, and readable operational states are acceptance criteria—not later polish.

## 14. Definition of a credible 0.1

KyYard 0.1 is ready for internal use when a new operator can, without documentation:

1. Install the control plane from one Compose file.
2. Retrieve and replace the bootstrap password.
3. Create an environment.
4. Enroll and approve two Docker hosts.
5. Find a container, inspect its configuration and resource use, view and search its logs, restart it, and open a terminal when authorized.
6. Deploy or import a Compose application, preview a change, apply it, and understand the resulting deployment record.
7. Detect an available image update and deliberately apply it.
8. Review who performed every privileged action.
9. Back up the control-plane state and complete the inherited restore drill.

The acceptance test is not merely that every API works. A person familiar with containers but unfamiliar with KyYard must complete this path from the UI without consulting setup documentation. Failure to do so is a product defect.

## 15. Immediate decisions and open questions

### Decided

- Product name: **KyYard**.
- Tagline: **The simple control plane for your container fleet.**
- Base: `Busnes-app/ky_server_base` at or after reviewed revision `f4ca19a`.
- Docker-first delivery; Kubernetes adapter follows a proven Docker control plane.
- One agent implementation with runtime adapters unless evidence requires separation.
- Multi-tenancy and scoped authorization precede workload management.
- Desired state is central to managed applications.

### Decide during the relevant design milestone

- Agent transport: outbound WebSocket, gRPC stream, or another transport supported cleanly by the one-binary deployment and ordinary reverse proxies.
- Public API versioning convention.
- Whether external/unmanaged containers can be edited directly or must first be adopted into an application definition.
- Minimum historical retention that remains practical on SQLite.
- How Compose secrets and environment variables are represented without leaking them through previews or backups.
- Exact TLS default and onboarding guidance.
- Whether platform administrators implicitly access all organizations or must explicitly assume an organization-scoped role.

None of these questions should block the scaffold-only first pull request.

## 16. Handoff summary

Begin with a clean KyYard scaffold from `ky_server_base` and prove that all inherited behavior survives the rename. Then fix persistent production-secret generation and build tenancy/authorization before touching Docker. Specify and threat-model agent enrollment before implementing it. Deliver read-only Docker fleet visibility before remote mutation, and deliver controlled Docker actions before desired-state Compose applications. Add automated updates only after deployment/rollback semantics are dependable. Add Kubernetes only after the control plane and runtime boundary have been proven through real Docker use.

The differentiator is not the number of resource types KyYard exposes. It is that an operator can start simply, manage a real Docker fleet well, add Kubernetes only where it helps, and move suitable workloads without changing control planes.
