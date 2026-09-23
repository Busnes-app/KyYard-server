**Repo:** Busnes-app/KyYard-server

# KyYard implementation plan

Updated 2026-09-22. Compose import, explicit snapshot adoption and deployment apply are implemented; section 8 records the remaining M6 work. Milestone acceptance gates remain independent of implementation status; the 24-hour capacity soak has not run.

## 1. Outcome and scope

User corrections: containers/endpoints first, administration under Settings; KyPost/KyDNS theme swatches; configurable OAuth 2/OIDC including KyIdentity; no SCIM or phone pairing; use Docker's existing default bridge. These supersede inherited scaffold feature exposure.

**Agent decision:** build the narrow KyYard agent with runtime adapters described in the engineering handoff. Portainer integration and its reuse evaluation are out of scope by user decision.

Deliver KyYard as one Go control-plane container with an embedded React UI and one persistent `/data` volume. Docker and Kubernetes remain permanent peers; deliver Docker first. Reuse inherited authentication, MFA, SSO, storage, recovery, UI themes, and CI. Tenancy and authorization precede workload APIs; reviewed protocol and security designs precede agent implementation.

**0.1 includes:** generated persistent secrets; organizations and environments; approved Docker enrollment; multiple endpoints; inventory and health; container lifecycle; image pull/removal; network/volume inspection; searchable, downloadable streaming logs; audited browser exec; bounded resource statistics; desired-state Compose discovery/import/deploy/update/removal; manual image update detection/recreate; scoped audit; control-plane backup and restore drill; accessible onboarding and operations.

**After 0.1:** automatic update policies, maintenance windows and health-driven rollback; Kubernetes; migration analysis; registry copying; advanced monitoring. Infrastructure provisioning, image building, CI orchestration, and unrestricted Kubernetes CRD editing remain outside this plan.

Resolve the handoff's milestone-7 scope tension by splitting it: M7a delivers manual updates in 0.1; M7b delivers automated policy and rollback afterward. M6 must already support deliberate recovery to a prior definition. Reapplying a definition does not promise reversal of data changes.

## 2. Baseline and active constraints

- Fresh product history is intentional. The imported baseline derives from `ky_server_base` revision `2a31d5c`; the handoff reviewed `f4ca19a`. Product identity and `/data` packaging landed in PR #1, and forced password replacement plus its security corrections landed in PR #3.
- Preserve untracked recovery notes and local tool state in the original checkout. Implement slices in isolated worktrees from current product master; never push product changes to the base remote or run `ky-init.sh` over a nonempty checkout.
- Durable session/encryption/instance keys landed in PR #4. The onboarding slice uses loopback-only HTTP by default, scheme-derived cookies and an exact advertised browser origin. Remote access requires an HTTPS reverse proxy with explicit peer trust. Agent enrollment is implemented under the M3 protocol.
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
2. Use module `github.com/Busnes-app/kyyard-server`, display name `KyYard`, image `ghcr.io/busnes-app/kyyard`, container/service `kyyard`, and a consistently named product binary.
3. Update imports, Makefile, Dockerfile, Compose overlays, install scripts, CLI help, config defaults, package/lock metadata, manifest/title, README, smoke tests, image publication and attestation repository/workflow identities.
4. Standardize container persistence under `/data`; keep existing startup semantics in this PR. Rename recovery application identity and domain-separated sealer labels coherently for fresh installs; explicitly decide whether preexisting base capsules/data are supported before claiming compatibility.
5. Rebuild and commit `web/dist`. Audit old references in source and generated assets; historical source citations are allowed and listed. Repair stale absolute DOX links as part of the rename.
6. Retain inherited capabilities in documentation and point to this plan instead of reviving obsolete base implementation text.

**Gate:** full inherited CI and container smoke pass under KyYard names. Bootstrap login works using the inherited required configuration. Forced password replacement subsequently landed in PR #3. Zero-config production startup is M1, not a hidden requirement of the rename PR. Published-image smoke follows publication; source smoke uses the build overlay and distinct local tag.

### M1 — Production-safe first boot

**PR 02: durable bootstrap and key lifecycle.** Reuse the keyfile helper for a persistent session secret and instance identity; inspect helper guarantees before extending it. Define 0700 directory/0600 file handling, exclusive creation, invalid/truncated/symlink failure behavior, restart persistence, and explicit environment override precedence. Keep secret material out of diagnostics and ordinary settings responses. Bootstrap credentials print once; restart preserves keys and does not recreate the administrator. Forced password replacement landed separately in PR #3: restricted identity/replacement/logout access, atomic revocation, operator-reset enforcement and MFA credential snapshots. Durable keys now use private keyfiles; instance identity is an Ed25519 seed reserved for the protocol milestone. Optional encryption/session overrides accept 32 bytes encoded as hex/base64. Snapshots preserve all active keys and remove session/MFA/pairing grants without affecting the live database.

**PR 03: one-container onboarding and health.** Default to SQLite and one named `/data` volume, with local sealed backups beneath that volume when configured. Keep advanced options in overlays/UI. Add secret-free liveness/readiness and a working image healthcheck. Define HTTP/TLS scheme, port, advertised URL, CSRF origins, proxy trust, and cookies together; a port number is not evidence of TLS. Show actionable setup guidance for encrypted remote connectivity. Retain the 20-minute graceful stop budget unless the tested budget changes. Implemented: deployment image, named volume, public liveness/database readiness, side-effect-free binary probe, loopback HTTP and strict HTTPS/proxy configuration, cross-origin write rejection, first-login setup guidance, and optional bind/proxy/PostgreSQL overlays. Existing bind installs must opt into their overlay before upgrade.

Include generated keys in capsule collection, manifest/member reporting, and restore checks. Specify session invalidation and instance-identity behavior on recovery before agents depend on it.

**Gate:** fresh volume + one Compose command boots with safe defaults; an operator signs in and changes the password; restart preserves access and secrets; invalid key storage fails clearly; no manually authored secret is required. Prove real-browser cookies work on the chosen default transport. Plain HTTP must not enable secret-bearing remote enrollment.

### M2 — Tenancy, authorization, settings, and navigation

**PR 04: schema and bootstrap migration.** Add organizations, memberships, environments, and scoped group membership or scoped extensions. Use fixed role mappings first; add role bindings only where environment grants require them. Define initial-organization migration for existing users; do not automatically grant every existing user access to every organization. Keep platform administration distinct from tenant membership. Migrations are resumable/forward-only and transactional where supported. Implemented in migration 5: organization-scoped roles/statuses, environments, groups and composite membership references. Startup then transactionally initializes the default organization once, selecting active local `admin` first or the oldest active local administrator; all other users remain unassigned. With no eligible local admin, initialization finishes without a grant. Restarts do not restore removed/disabled memberships. Existing global identity groups confer no organization access. Merged in GitHub PR #6; the planned slice numbers below are delivery order, not GitHub PR numbers.

**PR 05: permission enforcement and audit.** Introduce named actions and a single scoped authorization path. Require organization/environment context in product routes and store methods. Enforce composite ownership constraints so an endpoint, deployment, credential, or environment cannot reference a different tenant. Add actor, target, organization, environment, correlation ID, action, result, and timestamp to audit. Preserve historical/global audit meaning explicitly. Retain instance-wide recovery operations under platform authority. Implemented for organization/environment routes with live user/membership locks, credential snapshot validation and atomic success audit. Migration 6 preserves historical events as platform/unknown. Typed tenant settings join PR 06; credentials arrive with their resource APIs, each using this authorization path.

**PR 06: routed shell and administration.** Replace tabs with routes and organization/environment selectors; support deep links, browser navigation, loading/empty/offline/denied states, and keyboard/narrow-screen use. Keep authentication routes available and use secureFetch for writes. Clear cached tenant data and subscriptions on context changes. Add membership/environment management and persistent typed product settings with documented precedence over defaults and below explicit environment overrides. Delivered in two GitHub PRs: membership management APIs first (`GET /api/organizations` own memberships; `/members` list, put, delete under organization administration, with a last-active-administrator guard and target-user audit), then the routed shell, selectors and administration screens (`feat/routed-shell`: path-based routes without a router dependency, organization selector from own memberships, environment/member/audit screens with loading/denied/not-found/offline states, service-worker shell fallback for offline deep links). Typed persisted settings wait for the first optional product setting; today no product feature reads configuration outside startup environment, and a settings framework without a setting is speculative.

Review inherited SSO/SCIM/global groups: external identity never implicitly selects or grants a tenant. Define provider/group-to-membership mappings and revocation behavior; preserve local recovery access. Feature completion of federation remapping can follow the initial local path, but no global route may leak new tenant data. Reviewed on `feat/federation-tenancy`: no SSO, SCIM or webhook path writes tenant tables and no authorization decision reads global groups; deactivation denies live and keeps the grant, deletion cascades it; there is no provider/group-to-membership mapping and none is planned until a product need appears; local login has no disable switch and `ResetAdminPassword` is local-only, so recovery stays local. Pinned by `TestExternalIdentityNeverGrantsTenantAccess` and recorded in the sso/scim/api DOX. Blocker for the authorization matrix (PR 07): no product route lets a platform administrator repair an organization left without an active administrator, and IdP deactivation of a sole administrator bypasses the membership guard (`TestExternalDeactivationCanLockOutAnOrganization`).

**Gate:** every new store and API has cross-organization read/write/reference tests on both databases, including disabled/removed memberships and stale browser context. All six role names in the handoff have an explicit permission mapping; new handlers do not branch on role strings. Forced-password-change and inherited privileged-route protections still hold.

### M3 — Protocol, identity, and approved enrollment

**PR 07: required design package, before agent code.** Create the five documents in section 5 and record reviewed decisions and executable test cases. Resolve transport and TLS bootstrap with a small disposable compatibility spike where needed.

**PR 08: enrollment persistence/API/UI.** Add endpoints, agent identities, hashed expiring single-use tokens, and capability records. Token requests bind organization, environment, and runtime type. Consume atomically and bind a pending request to an agent-generated public key/proof of possession. Approval binds the reviewed fingerprint to the issued identity. Pending agents may report bounded enrollment facts but receive no workload commands; rejection, expiry, and revocation terminate access. Add approval UI and one generated Docker command with a persistent agent identity volume and clear Docker-socket authority disclosure. Backend delivered on `feat/agent-enrollment`: migration 7, `internal/agent/protocol` preimages, token/enroll/approve/reject/revoke/rename store methods, agent-facing enroll route with uniform refusals and a per-IP limit, environment delete guarded by live endpoints; approval UI on `feat/enrollment-ui` (environment screen: enroll a host, one-time command with disclosure, approve by fingerprint, reject, revoke); capability records follow with the agent binary.

**PR 09: KyYard agent binary and connection lifecycle.** Implement outbound authenticated encrypted transport, protocol/capability negotiation, heartbeat, reconnect/backoff, identity rotation, independent revocation, and graceful shutdown. Use lifecycle `pending → approved → active → offline → revoked`, allowing authenticated `offline → active`; revoked is terminal. Separate transport reachability from inventory freshness. Define durable identity and bounded result storage on the agent; enrollment tokens are removed after exchange. Keep the PWA device-pairing subsystem separate. Delivered on `feat/agent-connect`: `cmd/agent`, `internal/agent/client`, migration 8, the `/api/agent/v1/connect` handshake with state-gated channels, one socket per endpoint, heartbeat/offline, inventory generations, approval notice, revocation and shutdown closing sockets. Identity rotation (`feat/agent-rotation`: pending key inert until an operator acknowledges, one pending at a time, 7-day expiry, blocked after a duplicate connection, agent switches on `identity.rotated` or `key_retired`) and capability records are delivered; the published agent image waits on the registry namespace decision.

**Gate:** two isolated Docker hosts enroll/approve/reconnect and can be revoked independently without server restart. Concurrency tests prove one token consumer. Test expiry/replay, wrong tenant/runtime/key, unapproved commands, stale identities, incompatible versions, rotation interruption, restart, and revocation of open streams. Privileged runtime integration runs on disposable hosts.

### M4 — Read-only Docker fleet

**PR 10: inventory and observations.** Docker adapter reports engine facts, containers, images, networks, volumes, capabilities and health through product types. Use snapshot generations or equivalent atomic refresh semantics; old/reordered reports cannot overwrite newer state, and partial reports cannot erase resources. Store bounded summaries; Docker remains authoritative. Include pagination, observed-at timestamps, stale/offline states and reconnect resync. Delivered on `feat/docker-inventory`: `internal/runtime/docker` adapter with product types and bounds, snapshots on the existing generation channel stored per endpoint (migration 10), inventory API with observed/received timestamps, endpoint screen with staleness, skew and truncation indications; successful reads stop being audited. Pagination within a snapshot, stats and the retention soak remain.

**PR 11: fleet UI, logs foundation, and statistics.** Add endpoint lists/details and resource inspection, plus bounded CPU, memory, network and restart observations. Define retention/pruning, reconnect gaps, query limits and endpoint clock handling. Wire readonly log transport only after authorization/stream bounds are in place; finish user-facing log behavior in M5. Partly delivered on `feat/container-stats`: bounded CPU/memory/network/pid samples via one-shot Engine stats with agent-side deltas, six-hour retention with a minute-cadence pruner, latest-per-container and per-container windows, usage column on the endpoint screen that tells no data from zero. Restart counts, roll-ups, the soak fixture and the log transport remain.

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

**PR 16: registry access, detection, deliberate recreate.** Add scoped registries/encrypted credentials with read/manage permissions; credential use is gated by the using operation's own permission. Compare image digests, represent architecture/platform and unavailable/private registry failures, and report updates without deploying. Operator approval produces a previewed M6 deployment; pin exact chosen digests and retain prior references. Short-lived agent delivery handles credentials in memory where possible and removes temporary files on failure/restart.

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

Additional decisions: new product API namespace is `/api/organizations/{organization}/...`, retaining inherited auth routes; choose safe default transport/cookies in M1; settle Compose secret resolution in M6. Choose capacity targets—endpoints, resources, concurrent streams and retention disk budget—before the M4 soak gate. Record supported Docker/Compose/Kubernetes and agent/control-plane version ranges when adapters are implemented against current upstream documentation.

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

## 8. Current implementation and next delivery

M0–M4 infrastructure, M5 container/image commands and bounded logs, plus the product corrections and browser logs are merged through master `f1ce4ab` (PR #37). The 24-hour capacity soak remains an unproven M4 gate.

Merged PR #34 (`feat/image-controls`) adds explicit-tag/digest pulls and full-ID image removal with a fresh server-inventory preview, dependency checks, confirmation and visible command outcomes. It reuses the existing server/agent authorization and commands without adding routes. Browser tests against a local agent proved image pull/removal; frontend tests cover stale/incomplete/dependent previews, cancellation, uncertain submissions and polling. Generated bundle diffs no longer consume review input; CI still verifies exact source/build correspondence.

Merged PR #35: package `/app/kyyard-agent` in the existing published image, generate same-host enrollment from the exact installed image ID, use attached `--enroll-only` before detached startup, print the approval fingerprint, and expose a host refresh action. The same-host agent shares the server network namespace on the existing default bridge; after server replacement recreate the agent with its retained identity volume. Remote HTTPS enrollment uses the same image pinned by digest. CI now builds both binaries for smoke coverage and executes the generated command against real Docker, proving approval, inventory, restart and server replacement.

Merged PR #36 (`feat/exec-runtime`): Docker exec PTY with immutable target checks, explicit user, bounded argv and I/O, resize, cancellation, input-idle/absolute timeouts and independent exit inspection. Fake Engine tests cover refusal, uncertainty and blocked I/O; the real Docker regression proves non-root input/resize and exit code 7 in an isolated fixture, and runs in CI. Closing the attachment does not promise process termination. No browser/API exec entrypoint is enabled yet.

Merged PR #37 (`feat/exec-streams`): binary-safe agent exec frames, nonce-bound 60-second grants, replay refusal, four endpoint / one actor admission across reconnects, bounded input/output queues, cancellation and inspected exit reporting. Runtime work stays off the heartbeat loop. Race tests cover grant validation, binary I/O, queue pressure, cancellation, reconnect admission and live WebSocket responsiveness. That slice left the production runtime hook disabled pending the server authorization/revocation path described below.

Merged PR #38 (`fix/local-docker`): automatic in-process local Docker connection, default Compose socket mounting, trusted one-time endpoint bootstrap, tenant-gated operations, durable revocation, restart/replacement inventory, and sudo-aware manual enrollment for additional hosts. Local connection requires no shell enrollment command or separate agent container, by user decision. Theme palettes remain in Settings; header/login dropdowns are removed.

Implemented remote enrollment: single-container remote enrollment via HTTPS link, installed-image digest discovery by default, explicit digest-pinned override when discovery is unavailable, persistent identity and explicit approval; no same-host inspection or two-container setup chain.

Merged PR #40 (`feat/browser-exec`): browser terminal and server authorization, inventory confirmation, nonce-bound grants, scoped metadata audit, bounded queues, independent revocation checks, resize and inspected exit status. Both Docker agent adapters enable exec. Real Docker regression covers the entire browser-protocol/server/agent/PTY path, including first connection after approval.

Merged PR #41 (`feat/applications`): read-only Compose project discovery on the endpoint screen, grouped reported containers, explicit unmanaged state and navigation into existing container controls. Reuses authorized inventory without a new API, ownership record or runtime mutation.

Implemented M6 revision storage: application and immutable revision persistence, tenant/environment constraints, named permissions, expected-head concurrency, digest consistency checks, admission bounds, explicit draft discard and SQLite backup coverage. Typed specs hold the supported Compose fields and environment secret references; the importer below is the public admission path.

Implemented M6 import: bounded Compose draft import with encrypted environment bundles, audited internal resolution, reference-only inspection and explicit discard. Import itself performs no runtime mutations or adoption.

Implemented M6 ownership: explicit adoption/release of an immutable Compose container snapshot into one application instance, with preview confirmation, scoped constraints, audit and restore coverage. No runtime mutations or inferred configuration parity.

Implemented M6 comparison: read-only comparison of the latest definition with adopted identities and current project inventory. Explicitly distinguishes missing, changed and unowned containers; service labels and image-reference comparisons are advisory and configuration parity remains unverified. No deployment or runtime dispatch.

Implemented M6 revision editing: full-definition Compose replacements, revision-bound encrypted bundles, expected-head concurrency and immutable historical inspection. Saves never deploy; prior encrypted revisions survive recovery.

Implemented M6 service mapping: explicit service-to-adopted-ID assignments, version/revision/inventory preconditions, audit and restore coverage. No runtime mutations; labels remain advisory.

Implemented M6 deployment preflight: read-only local image identity resolution and mapping/published-port findings, gated on fresh complete container observations. Always non-executable; no secrets, persisted image pins or runtime commands.

Implemented M6 runtime inspection foundation: bounded redacted Docker container/image reads, immutable identity checks, selected-fact recheck and real Docker secret/tmpfs coverage. The transport below exposes its bounded redacted observation without deployment authority.

Implemented M6 inspection transport: nonce-bound agent requests, bounded cross-reconnect admission, a scoped read-only inspection API, strict result validation and live authority/target checks. Both local and remote adapters are wired; the real Docker regression exercises HTTP through agent to runtime. The live preflight UI below consumes the read-only API; executable planning does not.

Current M6 live preflight UI: mapped services expose an on-demand, cancellable native inspection dialog with full identity comparison, redacted facts and explicit non-executable boundaries. Preflight supplies owned inspection targets separately from desired-image resolution. No automatic inspection fanout, retry or deployment.

Implemented M6 deployment plans: persisted executable previews minted from a clean preflight under `application.deploy`, binding instance, mapping version, revision, spec digest, pinned image IDs and replaced-container identities, expiring after 10 minutes, one per instance, refusing release while live. No agent command.

Implemented M6 deployment runtime: `deployment.apply`/`deployment.result` wire types with bounds, and `docker.Client.Deploy` replacing mapped containers natively through the Engine API with preconditions, per-step outcomes, no pull, no volume access and no rollback, proven against a fake Engine and real Docker in CI.

Implemented M6 apply: the agent runs `Deploy` one at a time on its root context, persists results to a durable ledger before sending and re-sends them across reconnects; the wire is capped both directions and a protocol violation closes the socket. The apply route rechecks every plan precondition, resolves secret values internally, and moves `planned → applying` by compare-and-set; `SettleDeployment` rebinds `application_resources` to the new containers, advances `current_revision`/`previous_revision` on success, and writes the apply audit row in the same transaction, with `unknown` non-terminal until a later result or the deadline sweep resolves it. The UI applies a plan with a typed confirmation, polls per-step status while applying and shows fixed-text outcomes. Proven against a fake agent socket and a real Docker regression (`TestApplyRealDocker`).

Implemented M6 history, reapply and removal: a plan may target any saved revision, applied against that revision's own spec and encrypted values, still gated on the mapping review against the latest definition; the plan panel shows the instance's current and previous revision and a per-instance deployment history with expandable steps. `deployment.remove` travels through the same agent runner and one-slot ledger as apply; `docker.Client.Remove` stops and deletes each adopted container in order (precondition, stop, remove) without ever touching networks or volumes. `RemoveApplication` (`application.destroy`) inserts a `kind=remove` row already applying; settling it releases the instance and marks `removed_at` on full success, or forgets only the containers actually removed on a partial one. A removed application keeps its revisions and history for 90 days, can be re-adopted (clearing `removed_at`) or discarded, and `Prune` deletes both settled deployment history and removed applications past their 90-day windows. Every abandon, sweep and delivery-failure transition audits as `system`. Proven against a fake agent socket, SQLite and PostgreSQL, and the real Docker regression (`TestApplyRealDocker`, extended to remove the application it just deployed). **M6 is complete.**

Implemented M7a PR A (`feat/registries`): per-organization registries with write-only credentials sealed to their row, the audited anonymous-pull opt-in, `registry.read`/`registry.manage`, the Registries panel, and `internal/registry`, a server-side client that resolves a reference to its manifest digest behind an egress guard (`Head` for digest-only checks that Docker Hub does not count as pulls); private registry addresses need the operator's `KY_REGISTRY_ALLOW_PRIVATE`. Nothing pulls yet. Spec `docs/superpowers/specs/2026-09-23-registries-design.md`.

Next, M7a PR B: update detection (per-service digest comparison for an adopted instance, reported without deploying). Then PR C: a plan pins a repository digest, the agent gains a `pull` step with the credential in the frame, carrying forward from M6 a closed step-detail vocabulary (currently fixed text), a started-marker in the agent's deployment ledger, refusing a request whose deadline implies host/server clock skew, and plan-time inspection of live host configuration beyond the stored precondition identity.

The engineering gates still apply, including the unrun 24-hour soak. No UI or completed unit suite establishes that capacity gate.

The repository is the durable record; `kyyard-engineering-plan` on myslop mirrors handoffs and expires seven days after its last post.
