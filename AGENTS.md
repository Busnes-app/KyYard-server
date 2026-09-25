
# DOX framework

- DOX is highly performant AGENTS.md hierarchy installed here
- Agent must follow DOX instructions across any edits

## Core Contract

- AGENTS.md files are binding work contracts for their subtrees
- Work products, source materials, instructions, records, assets, and durable docs must stay understandable from the nearest applicable AGENTS.md plus every parent AGENTS.md above it

## Read Before Editing

1. Read the root AGENTS.md
2. Identify every file or folder you expect to touch
3. Walk from the repository root to each target path
4. Read every AGENTS.md found along each route
5. If a parent AGENTS.md lists a child AGENTS.md whose scope contains the path, read that child and continue from there
6. Use the nearest AGENTS.md as the local contract and parent docs for repo-wide rules
7. If docs conflict, the closer doc controls local work details, but no child doc may weaken DOX

Do not rely on memory. Re-read the applicable DOX chain in the current session before editing.

## Update After Editing

Every meaningful change requires a DOX pass before the task is done.

Update the closest owning AGENTS.md when a change affects:

- purpose, scope, ownership, or responsibilities
- durable structure, contracts, workflows, or operating rules
- required inputs, outputs, permissions, constraints, side effects, or artifacts
- user preferences about behavior, communication, process, organization, or quality
- AGENTS.md creation, deletion, move, rename, or index contents

Update parent docs when parent-level structure, ownership, workflow, or child index changes. Update child docs when parent changes alter local rules. Remove stale or contradictory text immediately. Small edits that do not change behavior or contracts may leave docs unchanged, but the DOX pass still must happen.

## Hierarchy

- Root AGENTS.md is the DOX rail: project-wide instructions, global preferences, durable workflow rules, and the top-level Child DOX Index
- Child AGENTS.md files own domain-specific instructions and their own Child DOX Index
- Each parent explains what its direct children cover and what stays owned by the parent
- The closer a doc is to the work, the more specific and practical it must be

## Child Doc Shape

- Create a child AGENTS.md when a folder becomes a durable boundary with its own purpose, rules, responsibilities, workflow, materials, or quality standards
- Work Guidance must reflect the current standards of the project or user instructions; if there are no specific standards or instructions yet, leave it empty
- Verification must reflect an existing check; if no verification framework exists yet, leave it empty and update it when one exists

Default section order:
- Purpose
- Ownership
- Local Contracts
- Work Guidance
- Verification
- Child DOX Index

## Style

- Keep docs concise, current, and operational
- Document stable contracts, not diary entries
- Put broad rules in parent docs and concrete details in child docs
- Prefer direct bullets with explicit names
- Do not duplicate rules across many files unless each scope needs a local version
- Delete stale notes instead of explaining history
- Trim obvious statements, repeated rules, misplaced detail, and warnings for risks that no longer exist

## Closeout

1. Re-check changed paths against the DOX chain
2. Update nearest owning docs and any affected parents or children
3. Refresh every affected Child DOX Index
4. Remove stale or contradictory text
5. Run existing verification when relevant
6. Report any docs intentionally left unchanged and why

## User Preferences

- Keep container lists paginated and bounded. Normal installation workflows must not require organization management; preserve authorization scope internally and explicit switching for existing multi-membership accounts.
- Keep the interface coherent across the whole site: clear operational navigation, consistent Ky design patterns, readable forms and tables, and reachable controls on mobile. Prioritize interface corrections over adding more UI clutter.
- The local Docker host connects automatically in the standard installation, without an enrollment command, separate agent container or fingerprint approval. Additional hosts retain explicit enrollment.
- Remote-host setup is one image-pulling Docker run command with an enrollment link; do not require inspecting a server container or running a separate enrollment container.
- Containers and endpoints are the primary navigation and home-page content; administration stays in Settings.
- Use the KyPost/KyDNS fifteen-palette swatch picker in Settings and browser-local theme preferences. Remove upper-right theme dropdowns from the header and login.
- Support configurable OAuth 2 / OIDC providers including KyIdentity; preserve local sign-in. SCIM and phone pairing are retired from the product.
- Use Docker's existing default `bridge` network; create no dedicated KyYard network.
- Bootstrap passwords and passwords installed by `init-admin` must be replaced before privileged use. Operator resets atomically revoke sessions, MFA challenges and device pairings. Untouched existing accounts are not retroactively flagged.

When the user requests a durable behavior change, record it here or in the relevant child AGENTS.md

## KyYard product planning

- Build the KyYard agent with runtime adapters; Portainer backend/agent integration and evaluation are out of scope by user decision.

- Before implementing KyYard product work, read [KyYard-Engineering-Handoff.md](KyYard-Engineering-Handoff.md) for product requirements and [KyYard-Implementation-Plan.md](KyYard-Implementation-Plan.md) for delivery order, design decisions, and acceptance gates.
- Repository: `Busnes-app/KyYard-server`; Go module: `github.com/Busnes-app/kyyard-server`; binary: `kyyard-server`; image: `ghcr.io/busnes-app/kyyard`; Compose service/container: `kyyard`; display and default recovery service name: `KyYard`. Container persistence is `/data`, including `/data/backups`.
- Git history starts fresh by user decision. The imported baseline derives from `ky_server_base` revision `2a31d5c`.
- The root owns these planning documents. Proposed domains in the plan become child DOX boundaries when their implementation lands.
- The M3 design package lives in `docs/`: `agent-protocol.md`, `threat-model.md`, `authorization-matrix.md`, `application-schema.md`, `retention-policy.md`. Each records its review status and a decisions table; values marked *proposed* are planning defaults, not settled product decisions, and every numeric retention or capacity value must survive the SQLite soak before it is frozen. Agent, endpoint, application and retention code must cite the document section it implements and update the document when the implementation diverges.

- Standard Compose mounts the local Docker socket and runs the built-in local agent inside the server process, retaining default bridge networking and one container. Local startup creates an approved endpoint in the initial organization once; existing tenant permissions gate actions, and revocation survives restart/deletion. The published `/app/kyyard-agent` remains for additional hosts. Manual/legacy same-host agents still need recreation after server replacement; README owns that runbook. Remote HTTPS enrollment discovers the installed server image’s official repository digest through the local Docker socket and uses one `--link` command. When discovery is unavailable, `KY_AGENT_IMAGE` must explicitly name a verified digest; mutable tags are never generated.
- Default Compose is one container, SQLite, a named `/data` volume and a loopback-only host HTTP publish (bridge peers can reach the container bind). It explicitly acknowledges its container-wide plaintext bind alongside the loopback host publish; the bare image fails closed without that acknowledgement or HTTPS configuration. Existing bind installs must enable `docker-compose.bind.yml`; proxy and PostgreSQL options have separate overlays. README owns setup and transport instructions; `docs/RESTORE.md` restores via the bind overlay while preserving the original named volume. The binary healthcheck probes readiness without loading config or creating keys.
- `cmd/soak` drives the storage path at the capacity targets and fails on any bound the retention policy promises but does not hold (docs/soak.md). Its short run is part of `make ci`, so the harness cannot rot; the 24-hour run is the M4 gate and is started by hand.
- `cmd/server` calls `Tenancy.Initialize` after account bootstrap and before serving HTTP. Store owns the one-time initial-organization migration and its marker. Tenant HTTP routes use named permissions and store-owned transactional authorization/audit; platform administration never implies tenant access.
- First boot persists encryption, session and instance keys; config owns key lifecycle and backup owns restore identity/session semantics. Bootstrap credentials print only after the generated account is saved.

## Verification

CI (`.github/workflows/ci.yml`) runs on every push and pull request:
- `make lint` equivalent: gofmt, `go vet`, `go mod tidy`/`verify`
- `go test -race` with coverage on SQLite, and the same suite against PostgreSQL 17
- Frontend vitest suite, then typecheck/build plus a check that committed `web/dist` matches source (it is embedded in the binary). Generated bundles and TypeScript build metadata use non-text diffs so review input includes source; the build comparison still fails on any artifact change.
- `govulncheck` and `npm audit --audit-level=high`
- `scripts/smoke-test.sh`: runs the built binaries and asserts CLI, auth, session, SPA behavior and the agent enroll/approve/connect/revoke path
- `scripts/spikes/websocket-proxy/run.sh`: developer-run, not part of CI. The compatibility spike behind `docs/agent-protocol.md` section 2 (own Go module, needs Docker, binds loopback only); prints `RESULT <proxy> PASS` for Caddy and nginx. Re-run it when the transport decision or proxy guidance changes.
- Real Docker runtime regressions (`go test -race ./internal/runtime/docker -run '^Test(Exec|Inspection|Deploy|Recheck|Remove)RealDocker$'` and `./internal/api -run '^Test(BrowserExec|Apply)RealDocker$'`): redacted inspection (runtime and authorized HTTP/agent round trip, with `health` `none` for a container without a healthcheck and `healthy` once Docker has run one), exec PTY, deployment replacement and removal (`TestRemoveRealDocker`), the recheck (`TestRecheckRealDocker`: a fixture container whose memory limit is changed between the phases is `denied` at `recheck` and left running under its name), the deploy pull step (`TestDeployRealDocker` with `KY_TEST_DOCKER_PULL_DIGEST`, which CI derives from `alpine:3.24`'s repository digest after `docker pull`: a second fixture service is replaced from `docker.io/library/alpine@<digest>` pulled anonymously by digest; the image is already present, so the download itself is not exercised), the full apply-then-remove regression (`TestApplyRealDocker`, `KY_TEST_DOCKER_DEPLOY_IMAGE`: server plus a real agent plan/apply a fixture container to `succeeded`, resources rebound, `current_revision` advanced, then removes the application and asserts the containers are gone, the network remains, the instance is released and the application is marked removed) and browser-protocol/server/agent regressions in the Go job (isolated fixtures, explicit non-root user, first approved connection, resize and exit status). The same step sets `KY_TEST_REGISTRY_ONLINE=1` and runs `TestResolveDockerHubAlpine`, which resolves `alpine:3.24` on Docker Hub through `internal/registry`; locally it skips unless that variable is set.
- Docker image build and container HTTP check; `scripts/agent-install-test.py` proves automatic local inventory, restart/replacement and durable revocation, then executes the generated additional-agent command with fingerprint approval and identity-preserving reconnect.
- On a push to `master` that passes every job, `publish` pushes the exact image the Docker check ran against (handed over as an artifact, no rebuild) to `ghcr.io/busnes-app/kyyard:<commit sha>`, attests it and verifies the attestation pinned to this workflow on `master`; `promote` then moves `:latest` to that digest, only at the tip of `master`, and asserts the tag resolves to the attested digest. `docker-compose.yml` names the published image and never builds; source installs add `docker-compose.build.yml` to the `COMPOSE_FILE` chain in `.env` (overlay tags `kyyard:local`) so every compose command, recovery docs included, uses the local build.
- Kubernetes: `go test ./internal/runtime/kubernetes/...` runs against the fake clientset in CI; the real-cluster manifest and RBAC test (`TestManifestOnARealCluster`) runs only with `KY_TEST_KUBECONFIG` set, locally, against a disposable cluster.

Run the same checks locally with `make ci` (`tidy-check lint test-race test-web smoke`); add `make test-postgres` when a Postgres instance is available.

## Child DOX Index

- [internal/applications/AGENTS.md](internal/applications/AGENTS.md): Bounded Compose import and transient secret separation.

- [internal/agent/AGENTS.md](internal/agent/AGENTS.md): Agent identity, enrollment, connection lifecycle and shared protocol.
- [internal/runtime/AGENTS.md](internal/runtime/AGENTS.md): Docker and Kubernetes runtime adapters, bounded streams and exec session primitives.
- [internal/config/AGENTS.md](internal/config/AGENTS.md): Configuration management and environment loader.
- [internal/permissions/AGENTS.md](internal/permissions/AGENTS.md): Named actions and fixed platform/tenant permission mappings.
- [internal/store/AGENTS.md](internal/store/AGENTS.md): Pluggable database abstraction layer (SQLite & PostgreSQL).
- [internal/crypto/AGENTS.md](internal/crypto/AGENTS.md): Cryptographic primitives (AES-256-GCM, HMAC, SHA-256, randomness, PKCE).
- [internal/auth/AGENTS.md](internal/auth/AGENTS.md): Authentication, MFA (TOTP), recovery codes, sessions, and CAPTCHA.
- [internal/sso/AGENTS.md](internal/sso/AGENTS.md): Single Sign-On federation (KySignOn, OIDC, SAML 2.0).
- [internal/scim/AGENTS.md](internal/scim/AGENTS.md): Retained legacy SCIM library tests; no product HTTP routes.
- [internal/backup/AGENTS.md](internal/backup/AGENTS.md): Product-side adapters over `ky-primitives/recoveryclient`: payload collection, drill checks, settings and sealer glue.
- [internal/registry/AGENTS.md](internal/registry/AGENTS.md): Image reference parsing and the guarded registry client that resolves manifest digests.
- [internal/devices/AGENTS.md](internal/devices/AGENTS.md): Retained legacy pairing library/storage compatibility; no product HTTP routes.
- [internal/testdb/AGENTS.md](internal/testdb/AGENTS.md): Test-only isolated database provisioning (SQLite or PostgreSQL).
- [internal/api/AGENTS.md](internal/api/AGENTS.md): HTTP REST API endpoints, routing, and middleware.
- [web/AGENTS.md](web/AGENTS.md): React 19 + TypeScript + Vite PWA frontend and KySecurity design system.

`cmd/server` `runServer` starts in this order: open the store (migrations; a schema newer than the binary is refused), bootstrap the admin, `startStore` (`Tenancy().Initialize`, then `Tenancy().ReconcileAfterStart`), and only then `api.NewServer`, the local-agent, backup and prune loops and the listener. A `startStore` failure is fatal like a failed migration. `TestStartStoreSettlesInFlightCommands` proves `startStore` settles an in-flight command; that `runServer` calls it before the listener is not tested, so keep the order.

`cmd/server` supervises the built-in local agent with jittered exponential retry (1s to 60s base delay); disabled configuration and persisted authority refusal stop the loop. Transient socket/store failures retry. Shutdown cancels and waits for this loop before the detached-handler wait and store close. Its private authenticated WebSocket handler joins the existing detached counter.

`cmd/server` owns the scheduler: `backupLoop` builds the `RunConfig` and client once and
returns with `scheduler disabled: ...` if that fails, because a run that never stamps its
attempt would log and audit the same failure every minute forever. It closes its `done` channel
only where it returns, between runs, and `runServer` cancels and waits on that channel after
`httpServer.Shutdown` and before the store closes, then waits on `api.Server.WaitDetached()` for
the pair, pin-key and deposit handlers, which detach from their requests and so outlive
`Shutdown`. `api.Server.RunPolicies`, the update-policy scheduler, starts beside `backupLoop` and
closes its own `done` only when its loop has stopped and no policy run is in flight; `runServer`
waits on it inside the same handler wait (a run's worst case is under 3 minutes, so the budget is
unchanged). `api.Server.RunValidations`, the health-validation loop, starts beside it and closes
its `done` only between ticks, a rollback in flight included (under `context.WithoutCancel`);
`runServer` waits on it in the same handler wait (`TestServerRunsAndAwaitsTheValidationLoop`).
`main.go` blank-imports `time/tzdata` so policy zones load the same on every host
(`TestServerEmbedsTheTimeZoneDatabase`). Nothing writes into a closed store. Both waits run under
one `backupWaitTimeout`
context (17m, the lib's 15m deposit ceiling plus sealing) -- a context, not a timer channel,
which delivers once and would leave the second wait unbounded; the HTTP drain is `shutdownTimeout`
(5s). `docker-compose.yml` grants a `stop_grace_period` above their sum, so the guarantee holds
in the shipped deployment instead of assuming a supervisor grace period;
`TestComposeGracePeriodCoversTheShutdownBudget` keeps the three in step. Past the deadline the
work is abandoned with a log line rather than killed silently.

The KyRecovery wire contract is `kyrecovery-server/zero_code_pairing_handoff_spec.md` (v2.0.0, sealed-capsule deposit); the product half is `ky-primitives/recoveryclient`, wired through `internal/backup` and `internal/api` so every server built on this base inherits it. Operator documents: `README.md` (disaster recovery, every `KY_BACKUP_*` variable, the LAN DNS override), `docs/RESTORE.md` (the restore runbook, proven against a scratch 2-of-3 kit) and `docs/ACCEPTANCE.md` (the plan's section 7 acceptance script as a runbook against the current UI; its Known gaps list must track UI changes).
