**Repo:** Busness-app/kyyard-server
**PR:** #4 — https://github.com/Busness-app/kyyard-server/pull/4
**Worktree:** /home/yoshi/busness.app/kyyard-durable-bootstrap (branch feat/durable-bootstrap)

# KyYard implementation plan

Prepared 2026-09-15 from [KyYard-Engineering-Handoff.md](KyYard-Engineering-Handoff.md). M0 merged in PR #1; password replacement and security corrections merged in PR #3 (base backport #33 also merged). M1 durable secrets are implemented on `feat/durable-bootstrap`; local CI and PostgreSQL race validation pass; PR #4 CI/review is pending. Later slices remain pending.

## 1. Outcome and scope

**Agent decision:** build the narrow KyYard agent with runtime adapters described in the engineering handoff. Portainer integration and its reuse evaluation are out of scope by user decision.

Deliver KyYard as one Go control-plane container with an embedded React UI and one persistent `/data` volume. Docker and Kubernetes remain permanent peers; deliver Docker first. Reuse inherited authentication, MFA, SSO/SCIM, storage, recovery, UI themes, and CI. Tenancy and authorization precede workload APIs; reviewed protocol and security designs precede agent implementation.

**0.1 includes:** generated persistent secrets; organizations and environments; approved Docker enrollment; multiple endpoints; inventory and health; container lifecycle; image pull/removal; network/volume inspection; searchable, downloadable streaming logs; audited browser exec; bounded resource statistics; desired-state Compose discovery/import/deploy/update/removal; manual image update detection/recreate; scoped audit; control-plane backup and restore drill; accessible onboarding and operations.

**After 0.1:** automatic update policies, maintenance windows and health-driven rollback; Kubernetes; migration analysis; registry copying; advanced monitoring. Infrastructure provisioning, image building, CI orchestration, and unrestricted Kubernetes CRD editing remain outside this plan.

Resolve the handoff's milestone-7 scope tension by splitting it: M7a delivers manual updates in 0.1; M7b delivers automated policy and rollback afterward. M6 must already support deliberate recovery to a prior definition. Reapplying a definition does not promise reversal of data changes.

## 2. Baseline and active constraints

- Fresh product history is intentional. The imported baseline derives from `ky_server_base` revision `2a31d5c`; the handoff reviewed `f4ca19a`. Product identity and `/data` packaging landed in PR #1, and forced password replacement plus its security corrections landed in PR #3.
- Preserve untracked recovery notes and local tool state in the original checkout. Implement slices in isolated worktrees from current product master; never push product changes to the base remote or run `ky-init.sh` over a nonempty checkout.
- Durable session/encryption/instance keys are this M1 slice. Production cookies still default to Secure; HTTP onboarding and Compose production defaults require the next slice's coherent transport decision.
- Preserve the existing 20-minute Compose shutdown grace period and detached backup drain.
- `internal/backup` snapshots SQLite only and rejects PostgreSQL snapshots. PostgreSQL recovery needs a separate tested implementation before advertising equivalent coverage.
- `make ci` omits some workflow gates, including frontend build/dist comparison, vulnerability scanning and container/publishing checks. Use the full verification matrix below.

## 3. Delivery mechanics and ownership

Deliver the numbered PR slices in dependency order. Each slice includes its owning DOX updates, relevant backend/frontend tests, user-visible failure states, and backup changes for new durable state. Code owners below are responsibilities, not assigned people.

| Boundary | Responsibility and likely location |
|---|---|
| Bootstrap/platform | `internal/config`, `cmd/server`, packaging, workflows; preserve recovery shutdown ordering |
| Identity/storage | `internal/auth`, `internal/store`, migrations; scoped memberships, permissions, constraints |
| API | `internal/api`; explicit authentication, tenant context, product DTOs, audit and stable errors |
| Fleet/agent | Proposed `internal/fleet`, `internal/agent/protocol`, `cmd/agent`, runtime adapters |
| Applications | Proposed `internal/applications`; revisions, deployments, Compose ownership and reconcile |
| Runtime adapters | Proposed `internal/runtime/docker`, later `internal/runtime/kubernetes`; SDK types stay inside adapters |
| Frontend | `web`; routes, context, fleet and application screens, accessible interaction |
| Recovery | `internal/backup`; payload, restore checks, settings/key glue over recoveryclient |
| Migration | Proposed `internal/migration`, created in M8 only |

Confirm directory names when implementing. Create child AGENTS.md files only as real domains land; update parent indexes then. Keep domain store interfaces narrow, with shared SQLite/PostgreSQL implementation patterns. Add no policy engine, scheduler service, cache, or generic plugin framework without a demonstrated requirement. SQLite is the default; PostgreSQL support alone does not establish multi-replica control-plane safety.

Critical path: **M0 → M1 → M2 → M3 → M4 → M5 → M6 → M7a → release acceptance**. M7b and M8 follow proven Docker deployments. Design documents can be prepared during M1/M2, but remote execution waits for the M3 review gate. No calendar promise is made before baseline validation and protocol/Compose spikes establish effort.

## 4. Milestones and reviewable PR slices

### M0 — Scaffold and preserve the baseline

**PR 01: product identity only.**

1. Record source revision and inspect the scaffold script. Generate into an empty directory; preserve this checkout's local material and establish the intended product remote.
2. Use module `github.com/Busness-app/kyyard-server`, display name `KyYard`, image `ghcr.io/busness-app/kyyard`, container/service `kyyard`, and a consistently named product binary.
3. Update imports, Makefile, Dockerfile, Compose overlays, install scripts, CLI help, config defaults, package/lock metadata, manifest/title, README, smoke tests, image publication and attestation repository/workflow identities.
4. Standardize container persistence under `/data`; keep existing startup semantics in this PR. Rename recovery application identity and domain-separated sealer labels coherently for fresh installs; explicitly decide whether preexisting base capsules/data are supported before claiming compatibility.
5. Rebuild and commit `web/dist`. Audit old references in source and generated assets; historical source citations are allowed and listed. Repair stale absolute DOX links as part of the rename.
6. Retain inherited capabilities in documentation and point to this plan instead of reviving obsolete base implementation text.

**Gate:** full inherited CI and container smoke pass under KyYard names. Bootstrap login works using the inherited required configuration. Forced password replacement subsequently landed in PR #3. Zero-config production startup is M1, not a hidden requirement of the rename PR. Published-image smoke follows publication; source smoke uses the build overlay and distinct local tag.

### M1 — Production-safe first boot

**PR 02: durable bootstrap and key lifecycle.** Reuse the keyfile helper for a persistent session secret and instance identity; inspect helper guarantees before extending it. Define 0700 directory/0600 file handling, exclusive creation, invalid/truncated/symlink failure behavior, restart persistence, and explicit environment override precedence. Keep secret material out of diagnostics and ordinary settings responses. Bootstrap credentials print once; restart preserves keys and does not recreate the administrator. Forced password replacement landed separately in PR #3: restricted identity/replacement/logout access, atomic revocation, operator-reset enforcement and MFA credential snapshots. Durable keys now use private keyfiles; instance identity is an Ed25519 seed reserved for the protocol milestone. Optional encryption/session overrides accept 32 bytes encoded as hex/base64. Snapshots preserve all active keys and remove session/MFA/pairing grants without affecting the live database.

**PR 03: one-container onboarding and health.** Default to SQLite and one named `/data` volume, with local sealed backups beneath that volume when configured. Keep advanced options in overlays/UI. Add secret-free liveness/readiness and a working image healthcheck. Define HTTP/TLS scheme, port, advertised URL, CSRF origins, proxy trust, and cookies together; a port number is not evidence of TLS. Show actionable setup guidance for encrypted remote connectivity. Retain the 20-minute graceful stop budget unless the tested budget changes.

Include generated keys in capsule collection, manifest/member reporting, and restore checks. Specify session invalidation and instance-identity behavior on recovery before agents depend on it.

**Gate:** fresh volume + one Compose command boots production mode; an operator signs in and changes the password; restart preserves access and secrets; invalid key storage fails clearly; no manually authored secret is required. Prove real-browser cookies work on the chosen default transport. Plain HTTP must not enable secret-bearing remote enrollment.

### M2 — Tenancy, authorization, settings, and navigation

**PR 04: schema and bootstrap migration.** Add organizations, memberships, environments, and scoped group membership or scoped extensions. Use fixed role mappings first; add role bindings only where environment grants require them. Define initial-organization migration for existing users; do not automatically grant every existing user access to every organization. Keep platform administration distinct from tenant membership. Migrations are resumable/forward-only and transactional where supported.

**PR 05: permission enforcement and audit.** Introduce named actions and a single scoped authorization path. Require organization/environment context in product routes and store methods. Enforce composite ownership constraints so an endpoint, deployment, credential, or environment cannot reference a different tenant. Add actor, target, organization, environment, correlation ID, action, result, and timestamp to audit. Preserve historical/global audit meaning explicitly. Scope settings and credential access; retain instance-wide recovery operations under platform authority.

**PR 06: routed shell and administration.** Replace tabs with routes and organization/environment selectors; support deep links, browser navigation, loading/empty/offline/denied states, and keyboard/narrow-screen use. Keep authentication routes available and use secureFetch for writes. Clear cached tenant data and subscriptions on context changes. Add membership/environment management and persistent typed product settings with documented precedence over defaults and below explicit environment overrides.

Review inherited SSO/SCIM/global groups: external identity never implicitly selects or grants a tenant. Define provider/group-to-membership mappings and revocation behavior; preserve local recovery access. Feature completion of federation remapping can follow the initial local path, but no global route may leak new tenant data.

**Gate:** every new store and API has cross-organization read/write/reference tests on both databases, including disabled/removed memberships and stale browser context. All six role names in the handoff have an explicit permission mapping; new handlers do not branch on role strings. Forced-password-change and inherited privileged-route protections still hold.

### M3 — Protocol, identity, and approved enrollment

**PR 07: required design package, before agent code.** Create the five documents in section 5 and record reviewed decisions and executable test cases. Resolve transport and TLS bootstrap with a small disposable compatibility spike where needed.

**PR 08: enrollment persistence/API/UI.** Add endpoints, agent identities, hashed expiring single-use tokens, and capability records. Token requests bind organization, environment, and runtime type. Consume atomically and bind a pending request to an agent-generated public key/proof of possession. Approval binds the reviewed fingerprint to the issued identity. Pending agents may report bounded enrollment facts but receive no workload commands; rejection, expiry, and revocation terminate access. Add approval UI and one generated Docker command with a persistent agent identity volume and clear Docker-socket authority disclosure.

**PR 09: KyYard agent binary and connection lifecycle.** Implement outbound authenticated encrypted transport, protocol/capability negotiation, heartbeat, reconnect/backoff, identity rotation, independent revocation, and graceful shutdown. Use lifecycle `pending → approved → active → offline → revoked`, allowing authenticated `offline → active`; revoked is terminal. Separate transport reachability from inventory freshness. Define durable identity and bounded result storage on the agent; enrollment tokens are removed after exchange. Keep the PWA device-pairing subsystem separate.

**Gate:** two isolated Docker hosts enroll/approve/reconnect and can be revoked independently without server restart. Concurrency tests prove one token consumer. Test expiry/replay, wrong tenant/runtime/key, unapproved commands, stale identities, incompatible versions, rotation interruption, restart, and revocation of open streams. Privileged runtime integration runs on disposable hosts.

### M4 — Read-only Docker fleet

**PR 10: inventory and observations.** Docker adapter reports engine facts, containers, images, networks, volumes, capabilities and health through product types. Use snapshot generations or equivalent atomic refresh semantics; old/reordered reports cannot overwrite newer state, and partial reports cannot erase resources. Store bounded summaries; Docker remains authoritative. Include pagination, observed-at timestamps, stale/offline states and reconnect resync.

**PR 11: fleet UI, logs foundation, and statistics.** Add endpoint lists/details and resource inspection, plus bounded CPU, memory, network and restart observations. Define retention/pruning, reconnect gaps, query limits and endpoint clock handling. Wire readonly log transport only after authorization/stream bounds are in place; finish user-facing log behavior in M5.

**Gate:** multiple hosts are inspectable; cross-tenant subscriptions fail closed; agent loss, stale messages and resync preserve correct inventory. A sustained fixture run proves configured database/storage bounds and cleanup. UI distinguishes missing data from zero resource usage.

### M5 — Controlled Docker actions, logs, and exec

**PR 12: command dispatch and container/image operations.** Add durable command intent/results with tenant/endpoint, request/command IDs, actor, capability/protocol version, deadline, expected resource identity/state, and idempotency key. Authorize at dispatch and enforce endpoint binding. Persist enough deduplication evidence across agent restarts; define an explicit unknown outcome for connection loss. Reconcile actual runtime state before any retry. Docker has no universal optimistic resource version: specify operation-specific identity and preconditions instead of inventing one.

Implement start/stop/restart/remove, image list/pull/remove and dependency checks. Previews/confirmations show organization, environment, endpoint, resource and consequences. Record intent and outcome, including denied/failed/unknown operations. Destructive actions do not receive blind automatic retries.

**PR 13: bounded logs and browser exec.** Logs support follow, timestamps, search/filter and download with explicit limits and truncation/gap indications. Exec requires its own permission, target/container/user confirmation, short-lived stream authorization, origin checks, idle/absolute timeout, resize, disconnect cleanup and immediate revocation. Use bounded buffers, backpressure and stream counts. Audit session metadata and outcome; do not record terminal contents or log bodies by default. The agent verifies authenticated command scope/expiry but holds no tenant authorization policy.

**Gate:** exercise every action against disposable Docker workloads. Test concurrent commands, changed/deleted targets, failed pulls, image-in-use removal, lost acknowledgments, server/agent restarts, expired deadlines and revoked authorization. Demonstrate bounded memory for slow log/terminal clients and cleanup after browser disconnect. Permission tests distinguish read/logs, operate, deploy, destructive, exec and secrets.

### M6 — Applications and Compose desired state

**PR 14: revisions and ownership.** Add applications, immutable revisions, instances, deployments and deployment events. Scope application name uniqueness to environment. Discover Compose projects as unmanaged; inspect/import/explicit adoption are distinct actions. Define desired configuration, observed runtime, drift and last deployment separately. Redact runtime inspect/configuration responses as well as previews, since environment variables can contain secrets.

**PR 15: preview and reconcile.** Validate the supported Compose subset and use an established Compose implementation where it fits the agent packaging; record exact tool/version requirements. Preview resolved configuration and runtime changes, persist revision/digest/secret references, and deploy that exact approved revision. Serialize conflicting deployments per instance. Resolve mutable image tags to recorded digests, retain prior config/image references, and return structured per-step outcomes on partial failure. Handle bind mounts, local files, secrets, project naming and offline endpoints explicitly; reject unsupported inputs with useful errors. Do not silently add image-building support.

Provide deploy/update/remove, deployment history, drift display, and deliberate reapply of a prior revision. Volume deletion requires separate explicit authorization/confirmation; application removal preserves data by default. Secrets are encrypted and delivered only for an authorized operation, without permanent agent storage of registry passwords.

**Gate:** discover → import/adopt → preview → deploy → inspect history → change → restore prior definition works end to end. Test concurrent revisions, stale approval, partial deploy, engine/agent loss, secret redaction, missing bind paths, incompatible Compose features and durable data preservation. The UI states when external data changes prevent meaningful rollback.

### M7a — Manual updates and 0.1 completion

**PR 16: registry access, detection, deliberate recreate.** Add scoped registries/encrypted credentials with separate read/manage/use permissions. Compare image digests, represent architecture/platform and unavailable/private registry failures, and report updates without deploying. Operator approval produces a previewed M6 deployment; pin exact chosen digests and retain prior references. Short-lived agent delivery handles credentials in memory where possible and removes temporary files on failure/restart.

**PR 17: recovery and operator acceptance.** Complete capsule coverage and restore verification for every new control-plane table and required key/secret. Exercise recovery to a clean installation with endpoint reconnect, revoked identities and pending/unknown commands; stale restored commands must not execute. Clarify that control-plane backup does not back up workload volumes or remote host data. Validate the full operator journey in section 7.

**Gate:** a detected image update changes nothing until approved; applied updates have exact image/configuration history and a deliberate recovery path. SQLite sealed backup/restore/drill passes with tenant, application, registry and agent state. All 0.1 acceptance scenarios pass.

### M7b — Automated update policies, after 0.1

**PR 18: policies and maintenance windows.** Add persistent update policies/windows only now. Define timezone/DST behavior, missed windows, concurrency, approval requirements, retry ceilings, exclusions, and auditable scheduler identity. Reuse proven deployment semantics; keep scheduling inside the existing server lifecycle unless measured needs justify another service.

**PR 19: health validation and eligible rollback.** Define startup grace, health signals, observation window, failures and rollback eligibility. Roll back only to recorded available image/configuration and expose incompatible data changes or absent prior artifacts. Persist attempts and stop oscillating update/rollback loops. Handle restart and server shutdown without duplicate dispatch.

**Gate:** an eligible failed update automatically returns to its recorded prior image/configuration with understandable history; an ineligible case stops with an actionable reason. Test scheduler restart, timezone transitions, endpoint outage and failed rollback.

### M8 — Kubernetes and migration, after Docker proof

**PR 20: Kubernetes enrollment and inventory.** Reuse the agent/protocol with explicit Kubernetes capabilities. Deliver generated installation manifests or Helm packaging with bounded namespace/cluster RBAC, inventory and health. Start with the resource subset in the handoff; prove denied capabilities stay unavailable in UI/API.

**PR 21: common application reconciliation.** Map supported definitions to Deployments/StatefulSets, Services, ConfigMaps, Secrets and PVCs. Preserve runtime-specific extensions and adoption/ownership rules. Track native rollout results without leaking SDK types. Test least-privilege credentials, partial rollout, immutable fields and storage constraints.

**PR 22: migration analysis and preview.** Inspect source definitions/resources; classify supported, operator-choice-required and blocked mappings for storage, networking, ports/ingress, secrets, probes, resources, scheduling and Docker-specific flags. Generate a versioned plan with assumptions and validation. Make destination deploy and traffic/data cutover explicit separate steps; preserve source until the operator completes validation.

**Gate:** the same stateless reference definition previews and deploys on Docker and Kubernetes. Unsupported stateful migrations stop with specific blockers and required operator actions. Native Docker operations remain usable after Kubernetes is added.

## 5. Design decisions and required artifacts

Create these as short documents before M3 agent implementation; review status is recorded in each document. Proposed defaults below are planning recommendations, not settled product decisions.

| Artifact | Required content and evidence |
|---|---|
| `docs/agent-protocol.md` | Versioned envelopes, endpoint/tenant binding, proof-of-possession enrollment, transport authentication, identity storage/rotation/revocation, deadlines, dedupe persistence, unknown outcomes, heartbeat, stream limits, compatibility and upgrade policy. Demonstrate reverse-proxy reconnect and failure behavior. Prefer outbound WebSocket if the spike meets these needs; choose gRPC only with evidence that deployment remains simple. |
| `docs/threat-model.md` | Assets and trust boundaries; Docker-socket host authority, replay/impersonation, compromised agent/control plane, registry credentials, exec streams, tenant escape, certificate trust, backup rollback and cloned identities. Map each mitigation to a test/operating constraint. |
| `docs/authorization-matrix.md` | Every public/product/agent action against the six roles, scope, secret exposure and audit behavior. Proposed platform-admin model: explicit audited assumption of tenant context rather than invisible blanket tenant access. Define stream revocation and last-admin protections. |
| `docs/application-schema.md` | Desired/observed state, revisions, Compose subset, runtime extensions, adoption, secret references, image digests, preview validity, concurrency, partial outcomes, delete semantics and honest rollback limits. Proposed default: unmanaged lifecycle actions are allowed by permission; configuration editing requires explicit adoption. |
| `docs/retention-policy.md` | Numeric time/byte/cardinality limits, query bounds, cleanup cadence, overload behavior and UI indications for metrics, events, logs, deployments, audit and exec metadata. Defaults require a SQLite fixture/soak measurement before freeze; do not ship unlimited retention. |

Additional decisions: settle new product API namespace/versioning in M2 (proposed `/api/v1/orgs/{organization}/...`, retaining inherited auth routes); choose safe default transport/cookies in M1; settle Compose secret resolution in M6. Choose capacity targets—endpoints, resources, concurrent streams and retention disk budget—before the M4 soak gate. Record supported Docker/Compose/Kubernetes and agent/control-plane version ranges when adapters are implemented against current upstream documentation.

No unresolved design decision blocks PR 01. Security-dependent decisions block their dependent implementation, not unrelated milestones. PostgreSQL backup support is a separate delivery decision; keep its unsupported snapshot status visible until a consistent snapshot and restore path is tested.

## 6. Verification and release discipline

- **Each PR:** focused tests for changed behavior; all new stores/migrations exercised on SQLite and PostgreSQL; permission/tenant isolation tests for new routes and streams; relevant DOX pass. Each new durable table/secret must update backup coverage in the same slice.
- **Baseline and integration gates:** `make ci`, plus `make test-postgres` with an isolated configured database; frontend build/typecheck and committed-dist comparison; workflow vulnerability checks; Docker build, health and HTTP smoke. For rename PRs, run clean-tree diff checks after intentionally recording module/dist changes, so expected edits are not confused with drift.
- **Packaging:** `docker compose config`, fresh-volume source-build smoke via build overlay, then published-image smoke and workflow attestation/digest verification. Use isolated Compose project names and disposable hosts. Verify graceful stop under active backup and agent work.
- **Faults:** token consumption races; cross-tenant references; membership removal; proxy reconnect; stale inventory; command timeout/unknown outcome; duplicate dispatch; deployment partial failure; revoked active streams; restart during rotation and restore.
- **Security/redaction:** verify credentials never appear in normal API output, inspect views, previews, telemetry, audit details or failure logs. Bootstrap password and generated enrollment command are intentional one-time disclosures. Keep browser CSRF/session and agent transport trust models distinct.
- **Recovery:** verify all required new records and keys after decrypting a scratch capsule; demonstrate tenant isolation and secret decryption after restore, and invalidate/reconcile stale authority and command state. Preserve inherited 17-minute backup wait plus HTTP drain guarantees.
- **Performance/accessibility:** use agreed M4 capacity fixtures; demonstrate bounded disk/memory and pruning under streaming load. Test keyboard paths, focus/confirmation behavior, screen-reader labels, contrast and narrow screens across operational states.
- **Release rollback:** retain previous image digest and backup before schema-changing upgrades. Forward-only migrations mean older binaries may not read the new schema; recover into a separate compatible installation from a verified backup rather than promising binary downgrade.

Record actual commands/results and failures in each implementation PR. Planning-time inspection does not substitute for these checks.

## 7. 0.1 human acceptance script

Use a clean installation, two disposable Docker hosts, a sample Compose application with persistent data, and two organizations. Ask an operator familiar with containers but unfamiliar with KyYard to complete this through the UI without setup documentation:

1. Start from the shipped Compose file, retrieve the one-time credential and replace it.
2. Create an environment and enroll/approve both endpoints; inspect identity and status.
3. Find a container, inspect configuration/statistics, search/follow/download logs, restart it, and open an authorized terminal.
4. Verify a read-only account cannot mutate, exec or reveal secrets; verify another organization cannot access the resources through direct URLs/API requests.
5. Discover/import a Compose project, preview a change, deploy, read deployment results, and deliberately restore a prior definition without losing its data.
6. Detect an image update, see that nothing changes automatically, approve and apply it.
7. Disconnect/reconnect one agent and revoke the other; understand stale state and denied actions.
8. Review attributable audit records for privileged operations and failures.
9. Create a sealed control-plane backup and complete the inherited scratch restore drill; perform the separate clean-install restore exercise with the recovery operator and verify new product state.

Record success/failure, confusing steps and recovery outcomes. Any required documentation lookup, cross-tenant exposure, unexplained unknown action, lost secret, or unusable recovery is a release defect. Fix and repeat affected paths before calling 0.1 ready for internal use.

## 8. Handoff and immediate next action

**Done:** M0 product identity and fresh history merged in PR #1. Password replacement and security corrections merged in PR #3, and base backport #33 is merged. PR #4 implements persistent private encryption/session/instance keys, production startup without manual secrets, environment override rules, post-save bootstrap credential logging, capsule key coverage and snapshot grant invalidation. Local `make ci` passes. PostgreSQL race validation passes after updating the oversized-backup fixture to use the real schema. Tests prove concurrent creation, unsafe-key rejection, restart continuity and restored-key continuity with snapshot-only grant revocation.

**Next:** finish PR #4 CI and security review. Then implement the next M1 slice: one-container onboarding, named volume, health and explicit transport/cookie defaults. M1's whole-installation acceptance gate is not complete until that slice lands. Instance identity is persisted now but agent protocol use and rotation are reserved for M3.

**Easy to get wrong:** pushing to the base remote; overwriting this nonempty checkout with ky-init; assuming HTTP port 9443 supplies TLS; breaking Secure cookies during onboarding; issuing identity before approval; retrying an unknown destructive operation; leaking Compose environment secrets; claiming rollback reverses volume writes; claiming PostgreSQL backup coverage that does not exist; closing the store while backup/agent work is still active.

The complete file is mirrored to myslop under `kyyard-engineering-plan`. The board expires seven days after its last post; this repository file is the durable copy. M0 and password replacement are merged; the M1 durable-secret slice is claimed.
