
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
- Bootstrap passwords and passwords installed by `init-admin` must be replaced before privileged use. Operator resets atomically revoke sessions, MFA challenges and device pairings. Untouched existing accounts are not retroactively flagged.

When the user requests a durable behavior change, record it here or in the relevant child AGENTS.md

## KyYard product planning

- Build the KyYard agent with runtime adapters; Portainer backend/agent integration and evaluation are out of scope by user decision.

- Before implementing KyYard product work, read [KyYard-Engineering-Handoff.md](KyYard-Engineering-Handoff.md) for product requirements and [KyYard-Implementation-Plan.md](KyYard-Implementation-Plan.md) for delivery order, design decisions, and acceptance gates.
- Repository: `Busness-app/kyyard-server`; Go module: `github.com/Busness-app/kyyard-server`; binary: `kyyard-server`; image: `ghcr.io/busness-app/kyyard`; Compose service/container: `kyyard`; display and default recovery service name: `KyYard`. Container persistence is `/data`, including `/data/backups`.
- Git history starts fresh by user decision. The imported baseline derives from `ky_server_base` revision `2a31d5c`.
- The root owns these planning documents. Proposed domains in the plan become child DOX boundaries when their implementation lands.

- Default Compose is one production container, SQLite, a named `/data` volume and loopback-only HTTP. Existing bind installs must enable `docker-compose.bind.yml`; proxy and PostgreSQL options have separate overlays. README owns setup and transport instructions; `docs/RESTORE.md` restores via the bind overlay while preserving the original named volume. The binary healthcheck probes readiness without loading config or creating keys.
- First boot persists encryption, session and instance keys; config owns key lifecycle and backup owns restore identity/session semantics. Bootstrap credentials print only after the generated account is saved.

## Verification

CI (`.github/workflows/ci.yml`) runs on every push and pull request:
- `make lint` equivalent: gofmt, `go vet`, `go mod tidy`/`verify`
- `go test -race` with coverage on SQLite, and the same suite against PostgreSQL 17
- Frontend vitest suite, then typecheck/build plus a check that committed `web/dist` matches source (it is embedded in the binary)
- `govulncheck` and `npm audit --audit-level=high`
- `scripts/smoke-test.sh`: runs the built binary and asserts CLI, auth, session, and SPA behavior
- Docker image build and container HTTP check
- On a push to `master` that passes every job, `publish` pushes the exact image the Docker check ran against (handed over as an artifact, no rebuild) to `ghcr.io/busness-app/kyyard:<commit sha>`, attests it and verifies the attestation pinned to this workflow on `master`; `promote` then moves `:latest` to that digest, only at the tip of `master`, and asserts the tag resolves to the attested digest. `docker-compose.yml` names the published image and never builds; source installs add `docker-compose.build.yml` to the `COMPOSE_FILE` chain in `.env` (overlay tags `kyyard:local`) so every compose command, recovery docs included, uses the local build.

Run the same checks locally with `make ci` (`tidy-check lint test-race test-web smoke`); add `make test-postgres` when a Postgres instance is available.

## Child DOX Index

- [internal/config/AGENTS.md](internal/config/AGENTS.md): Configuration management and environment loader.
- [internal/store/AGENTS.md](internal/store/AGENTS.md): Pluggable database abstraction layer (SQLite & PostgreSQL).
- [internal/crypto/AGENTS.md](internal/crypto/AGENTS.md): Cryptographic primitives (AES-256-GCM, HMAC, SHA-256, randomness, PKCE).
- [internal/auth/AGENTS.md](internal/auth/AGENTS.md): Authentication, MFA (TOTP), recovery codes, sessions, and CAPTCHA.
- [internal/sso/AGENTS.md](internal/sso/AGENTS.md): Single Sign-On federation (KySignOn, OIDC, SAML 2.0).
- [internal/scim/AGENTS.md](internal/scim/AGENTS.md): SCIM 2.0 user and group provisioning engine.
- [internal/backup/AGENTS.md](internal/backup/AGENTS.md): Product-side adapters over `ky-primitives/recoveryclient`: payload collection, drill checks, settings and sealer glue.
- [internal/devices/AGENTS.md](internal/devices/AGENTS.md): 90-second ephemeral QR device pairing and push registration.
- [internal/testdb/AGENTS.md](internal/testdb/AGENTS.md): Test-only isolated database provisioning (SQLite or PostgreSQL).
- [internal/api/AGENTS.md](internal/api/AGENTS.md): HTTP REST API endpoints, routing, and middleware.
- [web/AGENTS.md](web/AGENTS.md): React 19 + TypeScript + Vite PWA frontend and KySecurity design system.

`cmd/server` owns the scheduler: `backupLoop` builds the `RunConfig` and client once and
returns with `scheduler disabled: ...` if that fails, because a run that never stamps its
attempt would log and audit the same failure every minute forever. It closes its `done` channel
only where it returns, between runs, and `runServer` cancels and waits on that channel after
`httpServer.Shutdown` and before the store closes, then waits on `api.Server.WaitDetached()` for
the pair, pin-key and deposit handlers, which detach from their requests and so outlive
`Shutdown`. Nothing writes into a closed store. Both waits run under one `backupWaitTimeout`
context (17m, the lib's 15m deposit ceiling plus sealing) -- a context, not a timer channel,
which delivers once and would leave the second wait unbounded; the HTTP drain is `shutdownTimeout`
(5s). `docker-compose.yml` grants a `stop_grace_period` above their sum, so the guarantee holds
in the shipped deployment instead of assuming a supervisor grace period;
`TestComposeGracePeriodCoversTheShutdownBudget` keeps the three in step. Past the deadline the
work is abandoned with a log line rather than killed silently.

The KyRecovery wire contract is `kyrecovery-server/zero_code_pairing_handoff_spec.md` (v2.0.0, sealed-capsule deposit); the product half is `ky-primitives/recoveryclient`, wired through `internal/backup` and `internal/api` so every server built on this base inherits it. Operator documents: `README.md` (disaster recovery, every `KY_BACKUP_*` variable, the LAN DNS override) and `docs/RESTORE.md` (the restore runbook, proven against a scratch 2-of-3 kit).
