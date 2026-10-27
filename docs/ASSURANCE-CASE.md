# Assurance case

The supported [key-strength/TLS configuration](STRONG-CRYPTO.md) covers imported HMAC/RSA/TOTP floors, verifier-matched JWK parsing and Go certificate-chain policy. Preserve original data/instance identity, endpoint authority and custody; independent proxy/daemon/cluster rollout remains operational verification.

[Product overview](../README.md) · [Threat model](threat-model.md) · [Authorization matrix](authorization-matrix.md) · [Verifying releases](VERIFYING-RELEASES.md)

This is the argument that KyYard meets its security requirements: what it protects, from whom,
where the trust boundaries are, and why the design and the code hold at each one. The detailed
threats, each with the test that proves its mitigation, live in [threat-model.md](threat-model.md),
and who may do what lives in [authorization-matrix.md](authorization-matrix.md). This page ties
them together and points at the evidence. Where a claim names code, the path is relative to the
repository root. Vulnerability reports go through the Busnes-app
[security policy](https://github.com/Busnes-app/.github/blob/main/SECURITY.md) (the repository's
**Security** tab, **Report a vulnerability**).

## What KyYard must protect

KyYard is the control plane for a fleet of Docker hosts and Kubernetes clusters. An enrolled
Docker endpoint gives it the Docker socket, which is root on that host. Its security requirements:

1. **Only the right person signs in**, and a stolen session cannot take over the account.
2. **Every action is authorized for its organization.** A member reaches only their own
   organizations, with only the named permissions their role grants; platform administration
   never implies tenant access.
3. **Only an approved agent speaks for an endpoint**, and a revoked one stops at once.
4. **Host-equivalent power stays narrow.** Exec, configuration reads (environment values), recreate
   and deploy each need their own permission. A deploy never adds a host bind mount
   (`bind_mount_new`); a direct container edit adds one only with a per-path acknowledgement from an
   organization administrator, audited per path (`bind_unacknowledged`).
5. **Secrets stay secret:** registry credentials, application secrets, keys and tokens are sealed
   or hashed at rest and never reach a response, a log line or an audit row.
6. **The server cannot be turned against its network** through registry hosts or other
   operator-supplied URLs.
7. **What happened is recorded**, in bounded audit rows per organization.

## Threat model

The full table is in [threat-model.md](threat-model.md). In short:

| Attacker | Can | Wants |
|---|---|---|
| Anonymous network client | Send any request to the published port | Take over an account, enroll a rogue agent, exhaust resources |
| Member of another organization | Use their own session | Read or change this organization's endpoints, containers or secrets |
| Signed-in member with a narrow role | Use their session | Exec into a container, read environment values, deploy |
| Holder of a leaked enrollment token or a copied agent identity | Dial the agent endpoint | Enroll a rogue host or impersonate a real one |
| Compromised agent or its host | Send any frame | Reach other hosts, other organizations, or the control plane's secrets |
| AI agent with an MCP token | Call the MCP tools | Do anything beyond read or restart |
| Someone with a copy of `/data` or a backup | Read the database | Recover registry credentials, application secrets, sessions |

Out of scope: a compromised control plane (it is the root of authority; the mitigations are blast
radius and detection, per the threat model), an attacker who already holds a host's Docker
socket, and a malicious platform administrator.

## Trust boundaries

1. **Browser → control plane.** Session cookie, CSRF token, an Origin check on every unsafe request
   and on every exec WebSocket, and a CSP (`internal/api/server.go`, `internal/api/exec_handlers.go`).
   Agent routes never accept a session cookie.
2. **Agent → control plane.** TLS, then a per-connection challenge-response: the agent signs the
   server's 32-byte nonce, its endpoint ID, the host it dialed and the protocol version with its
   Ed25519 key (`protocol.VerifyAuth`, `internal/agent/protocol/messages.go`). Enrollment tokens are
   single use, expire after 15 minutes and are stored as SHA-256 hashes (`internal/store/endpoints.go`).
3. **Control plane → runtime.** The agent holds the socket; the control plane holds authorization.
   The agent keeps no tenant policy, users or long-lived credentials.
4. **Control plane → registries and KyRecovery.** Outbound only. The registry client pins the
   address it checked and dials nothing else, refuses loopback, link-local, multicast and
   unspecified addresses always and private or CGNAT addresses unless the operator and the
   organization both allow them, ignores proxy variables, and never follows a redirect
   (`internal/registry/client.go`, `internal/registry/egress.go`).
5. **Organization → organization.** Every product row carries `organization_id`; store methods take
   a `TenantAccess` and authorize and query in the same transaction.
6. **MCP agents → control plane.** `/mcp` accepts only an agent token (stored as a SHA-256 hash,
   expired after 90 days unused) and refuses any request that carries an `Origin`
   (`internal/api/mcp.go`, `internal/store/agent_tokens.go`).

## Secure design principles applied

- **Least privilege.** Named permissions per action instead of role checks; exec, configuration and
  destroy are separate permissions; MCP tokens are read-only unless an administrator chooses
  Operator. The Kubernetes ClusterRole reads only listed resource kinds, with writes only in named
  namespaces ([threat-model.md](threat-model.md)). The server runs as uid 10001 after the entrypoint
  hands it `/data` and the socket's group.
- **Fail-safe defaults.** The bare image refuses to serve plaintext HTTP on a non-loopback bind
  without an explicit acknowledgement; Compose publishes on `127.0.0.1`. A new enrollment lands in
  `pending` until a human approves its fingerprint. Private registry addresses are refused until
  allowed. A rotated agent key is inert until acknowledged.
- **Complete mediation.** Authorization runs in the same store transaction as the read or write it
  guards; a policy run re-checks its author's authority before and during the run.
- **Economy of mechanism.** One registry client, one CSRF/Origin gate, shared primitives
  (`ky-primitives`) for password hashing, key files and backup sealing.
- **Separation of privilege.** Platform administration and tenant access are distinct; the last
  active organization administrator cannot be removed; turning MFA off or changing the password of
  an MFA account needs a current code.
- **Open design.** Only published algorithms: Argon2id, AES-256-GCM, Ed25519, HMAC and SHA-256. The
  code, the threat model and this argument are public.

## Common weaknesses and how they are countered

| Weakness | Counter | Where |
|---|---|---|
| Credential guessing | Argon2id from `ky-primitives/password` (64 MiB, 3 passes, 4 lanes), 12-character minimum, a proof-of-work CAPTCHA by default, 20 logins per minute per client address, and `DummyVerify` so an unknown user costs the same | `internal/auth/policy.go`, `internal/api/auth_handlers.go`, `internal/config/config.go` |
| Session theft or fixation | 32 random bytes per session, only the SHA-256 stored; `HttpOnly` and `SameSite` cookies, `Secure` tied to the `KY_APP_URL` scheme; sessions, MFA challenges and device pairings deleted on a password change | `internal/auth/session.go`, `internal/store/sqlstore.go` (`revokePasswordGrants`) |
| CSRF and cross-site WebSockets | Origin must equal `KY_APP_URL` on unsafe requests; the CSRF cookie must be echoed in `X-CSRF-Token`; an exec WebSocket must carry that Origin | `internal/api/server.go`, `internal/api/exec_handlers.go`, `TestBrowserExecRefusesOriginAndRoles` |
| Broken access control and tenant escape | Scope taken from the URL only; every store method takes `TenantAccess`; non-members get no audit row | `internal/store`, `TestTenantRoutesEnforceScopeAndAudit`, `TestNonMemberDenialLeavesNoTenantAudit`, `TestPrivilegedEndpointsRequireAdmin` |
| Federation granting access | SSO and directory sync write global accounts only, never memberships | `TestExternalIdentityNeverGrantsTenantAccess` |
| Session outliving its identity-provider login | Back-channel logout token verified (signature, issuer, audience, expiry, logout event, no nonce) before ending every session of that provider's `sid`; the `sid` is fenced so a callback or MFA completion in flight cannot revive it | `internal/sso/backchannel.go`, `internal/store/sqlstore.go`, `TestVerifyLogoutToken`, `TestBackchannelLogoutEndsTheSSOLogin` |
| Rogue or impersonated agent | Hashed single-use 15-minute enrollment token, human fingerprint approval, Ed25519 challenge bound to endpoint, nonce, host and version | `internal/store/endpoints.go`, `internal/agent/protocol` |
| SQL injection | `database/sql` with `?` parameters for every value; assembled statements join only string constants from the code (column lists, `FOR UPDATE`, placeholder lists, fixed table names) | `internal/store` |
| Cross-site scripting | React escaping, no `dangerouslySetInnerHTML` in `web/src`; a CSP with `script-src 'self'` and `frame-ancestors 'none'`; workload logs are labelled untrusted in MCP output | `web/src`, `internal/api/server.go` |
| Server-side request forgery | The registry client's checked-and-pinned dial, no proxies, no redirects, HTTPS only | `internal/registry` |
| Secrets at rest | Registry credentials sealed with AES-256-GCM under a key bound to organization and row, write-only through the API; enrollment, service and MCP tokens stored as SHA-256; key files must be `0600` | `internal/store/registries.go`, `internal/store/agent_tokens.go`, `ky-primitives/keyfile` |
| Parser bugs on hostile input | Bounded decoding of agent inventory, Compose import and image references, each with size caps | `internal/agent/protocol`, `internal/applications`, `internal/registry` |
| Vulnerable dependencies | Dependabot, `govulncheck` and `npm audit --audit-level=high` in CI; a daily dependency release ships the updates | `.github/dependabot.yml`, `.github/workflows/` |
| Supply-chain tampering | Every GitHub Action pinned by commit; base images pinned by digest; release images carry a build-provenance attestation checked before `:stable` moves; both binaries rebuild to the same bytes | [VERIFYING-RELEASES.md](VERIFYING-RELEASES.md) |

## Residual risk

- The Docker socket is root on its host. Anyone who can drive KyYard with exec or deploy rights on
  an endpoint has that power; the README and the enrollment UI say so.
- Login throttling is per client address (20 per minute) plus the proof-of-work CAPTCHA. There is
  no per-account limit on password guesses, so an attacker with many addresses is slowed only by
  the CAPTCHA and Argon2id.
- If the entrypoint cannot hand `/data` or the socket to uid 10001, the server keeps running as
  root and logs a warning (`scripts/docker-entrypoint.sh`).
- Two live instances restored from one backup share an instance identity; the restore runbook
  requires the source to be stopped ([threat-model.md](threat-model.md)).
- Rows marked *planned* in the threat model have no test yet.
- No Ky product has had an independent security audit.

## Evidence

- **Tests:** the Go suite runs under the race detector on SQLite and again on PostgreSQL 17 in CI,
  with real-Docker regressions for exec, inspection, deploy, recreate and rollback; Vitest for the
  frontend; and an end-to-end smoke test that enrolls, approves, connects and revokes a real agent.
  The Go statement coverage is printed in each CI run's summary.
- **Installer authentication:** the smoke job also runs `scripts/installer-oidc-test.py`
  against the real server and a disposable certificate-verified TLS identity provider:
  setup, PKCE, signed-token owner/everyday/unassigned admission, global-role rejection,
  app-role loss and session revocation. Main publication and release images require smoke.
  This does not qualify every installer workflow or establish broad branch/input coverage.
- **Real-browser regressions:** desktop and phone Chromium execute committed `web/dist`
  against a disposable real SQLite server: mandatory password replacement, replayed
  session refusal, deep links, CSRF-protected writes, duplicate-name recovery,
  existing non-member tenant denial, appearance persistence and keyboard/viewport
  checks. The browser job gates main publication and release images. Fixture rules
  are in `tests/browser/AGENTS.md`; these bounded cases do not establish broad
  branch coverage, HTTPS cookie behavior or real-runtime/human acceptance.
- **Static analysis:** CodeQL (GitHub default setup: Go, JavaScript/TypeScript, Python, Actions),
  `go vet`, `govulncheck`, and the TypeScript compiler in `strict` mode.
- **Dynamic analysis:** the race detector, real-Docker and smoke tests, and the Docker job, which
  runs the image and checks the server runs as uid 10001.
- **Releases:** attested images and repeatable binaries ([VERIFYING-RELEASES.md](VERIFYING-RELEASES.md)).

This case is updated when a trust boundary or a security requirement changes. A pull request that
moves one must update this page, and [threat-model.md](threat-model.md), in the same change.
