# KyYard threat model

**Status:** draft proposal for review (M3 PR 07). Each mitigation names the test or operating constraint that proves it; items marked *planned* have no test yet and block the milestone that introduces the asset.

## Assets

| Asset | Where | Why it matters |
|---|---|---|
| Host authority through the Docker socket | every enrolled endpoint | root-equivalent on that host |
| Agent identities and enrollment tokens | `endpoint_agents`, `agent_enrollment_tokens`, agent identity volume | impersonating an endpoint or enrolling a rogue one |
| Control-plane keys | `/data` (encryption, session, instance identity) | decrypting secrets, forging sessions, cloning the instance |
| Registry credentials and Compose secrets | `registries.credential_enc`, the organization secret store (revisions hold references only), agent memory during a command | supply-chain and data access |
| Tenant data and desired state | organization-scoped tables | cross-tenant read or write |
| Exec and log streams | live sockets | interactive host access, data exfiltration |
| Audit records | `audit_records` | accountability; tampering or displacement hides abuse |
| Control-plane backups | sealed capsules | full instance clone |

## Trust boundaries

1. Browser ↔ control plane: session cookie, CSRF token, Origin check, CSP. Never reused for agents.
2. Agent ↔ control plane: TLS plus per-connection challenge-response on an Ed25519 identity. Never reused for browsers.
3. Control plane ↔ runtime: the agent holds the socket; the control plane holds authorization. Neither trusts the other's claims about tenancy.
4. Control plane ↔ registries and KyRecovery: outbound only, credentials scoped per use.
5. Organization ↔ organization: every product row carries `organization_id`; every product query is scoped in the same transaction that authorizes it.

## Threats and mitigations

| Threat | Mitigation | Proof |
|---|---|---|
| Rogue host enrolls with a leaked token | Token is single use, 15 min, hashed at rest, bound to one organization/environment/runtime; enrollment lands in `pending` and needs a human to approve the reviewed fingerprint | protocol test 1–3; UI approval step |
| Replayed connection handshake | Server nonce per connection signed by the agent key under a distinct context string; nonce never reused | protocol test 12 |
| Impersonated endpoint | Identity bound to the reviewed public key; a rotated key cannot authenticate until an operator acknowledges its fingerprint and the approved key keeps working meanwhile, so a copied identity volume can neither take the endpoint over nor lock the real agent out; wrong key fails the challenge; tenant binding checked on every command; a second live connection for one endpoint is refused, recorded, and blocks rotation | protocol tests 2, 4, 7, 13 |
| Compromised agent | Agent holds no tenant policy, users, desired state or long-lived credentials; commands are scoped and deadlined; the agent can only affect its own host and the resources the control plane names; revocation is immediate and needs no restart | protocol tests 8, 11; agent code review rule "no secret persisted" |
| Compromised control plane | Out of scope for containment: it is the root of authority. Mitigations are reducing blast radius (per-use registry credentials, no stored plaintext secrets, keys in `/data` with 0600) and detection (audit of every privileged action, instance identity in backups) | key-permission tests in `internal/config`; audit tests |
| Stale or replayed commands after restore or reconnect | Deadlines on every command; dedupe store on the agent; restore marks in-flight commands `unknown` and never re-dispatches them | protocol tests 5, 6, 11 |
| Docker socket presented as sandboxed | Enrollment command, UI and README state that the socket is host-equivalent; a socket proxy is a later hardening, not implied now | copy review; README |
| Registry credential leakage | Implemented (M7a PR A): write-only (no route, list or audit row carries it; rows report `has_credential`); sealed with AES-GCM under a key bound to organization and row, so moved ciphertext does not decrypt; managed only under `registry.manage` (organization administrators); a reference on an unconfigured host is refused unless the audited per-organization anonymous-pull opt-in is on, so an operator who may pull cannot choose the host that receives a credential. The server-side client is HTTPS only, follows no redirect, ignores proxy variables, caps bodies at 4 MiB and headers, and sends the credential only to the configured host or the HTTPS token realm that host advertised (the one exception to exact-host matching, which Docker Hub's separate auth host needs); the bearer token only to the configured host. Egress guard: each host resolved once and the dial pinned to the checked address; loopback, unspecified, multicast, link-local and local-use NAT64 always refused; private and CGNAT refused unless the row sets `allow_private`, which an organization administrator can set only when the operator has set `KY_REGISTRY_ALLOW_PRIVATE` (off by default) and which the store re-checks against that switch on every use, so a tenant cannot aim the server at the server's own network and turning the switch off takes effect at once. The credential is decrypted only under the permission of the operation that uses it (`image.pull`, `application.deploy`), never `registry.read`. The bearer token is capped at 8 KiB. The update check (PR B) decrypts it under `application.deploy` and holds it in memory only for its registry calls, with no transaction open; it stores and returns only digests, the verdict and a fixed detail code, never error text or response bodies, and its per-application in-progress guard answers only after `application.deploy` is authorized, so a caller without it gets 403, never 409. Planned (PR C): delivery to the agent only inside the authorizing pull frame, scrubbed from results, events, inspect, previews, diffs and logs, never persisted by the agent. Residual: `0.0.0.0/8`, `240.0.0.0/4`, `fec0::/10` and `2002::/16` are not yet refused | `TestRegistryCredentialIsWriteOnly`, `TestRegistryCredentialIsBoundToItsRow`, `TestRegistryRoles`, `TestPrivateRegistriesNeedTheOperatorOptIn`, `TestRegistryPolicyAndResolution`, `TestRegistryRoutes`, `TestRegistryPrivateOptIn`, `TestTokenIsCapped`, `TestCredentialNeverLeavesTheConfiguredHosts`, `TestResolveRefusesRealmThatIsNotHTTPS`, `TestResolveGuardsTheRealmHost`, `TestResolveRefusesPrivateDestination`, `TestCheckAddr`, `TestTransportRefusesAnUncheckedHost`, `TestCheckImageUpdatesVerdicts` (no secret in rows or audit), `TestCheckImageUpdatesPermissions`, `TestCheckImageUpdateAccess`, `TestImageCheckRoutes` (credential reaches the resolver, never a response; guard after authorization); agent delivery redaction tests (*planned*, PR C); protocol section 8 |
| Exec stream abuse | Own permission (`container.exec`), target confirmation, short-lived stream grant, Origin check, idle and absolute timeouts, immediate close on revocation, session metadata audited, contents not recorded | `TestBrowserExecRefusesOriginAndRoles`, `TestBrowserExecRefusesCSRFAndChangedTarget`, `TestBrowserExecRevocationAndDisconnect`, `TestExecAdmissionIsolationAndOverflow`, `TestBrowserExecRealDocker` (implemented, M5) |
| Tenant escape through IDs in URLs or bodies | Scope from URL only; strict JSON; every store method takes `TenantAccess`; composite foreign keys; non-members get no audit row; cross-tenant tests run on SQLite locally and on PostgreSQL in the CI workflow's PostgreSQL job (`make ci` alone is SQLite) | `TestTenantRoutesEnforceScopeAndAudit`, `TestScopedAuthorizationAndAudit`, `TestNonMemberDenialLeavesNoTenantAudit` |
| Tenant escape through federation | SSO/SCIM/webhook write global accounts only; no group grants membership | `TestExternalIdentityNeverGrantsTenantAccess` (PR #10) |
| Audit displacement or growth | Audit rows only for members; bounded pagination; successful reads stop being audited when the read APIs land (`authorization-matrix.md`); repeated identical denials collapse into counted rows and each organization has a row/byte ceiling whose exhaustion refuses that organization's mutations only (`retention-policy.md`) | `TestNonMemberDenialLeavesNoTenantAudit`; retention soak with sustained denials (*planned*) |
| Certificate trust | Agent verifies the server chain against the system store or a pinned CA supplied at enrollment; no insecure flag exists; the control plane never claims TLS it does not provide | agent config tests (*planned*); M1 transport contract |
| Cloned agent identity volume | Duplicate live connections refused and recorded; rotation blocked after a duplicate; a rotated key is inert until acknowledged and only one may be pending; revocation is the remedy | protocol tests 7, 13 |
| Cloned instance from a restored backup | Instance identity key travels with the capsule; two live instances with one identity would both be accepted by agents. Mitigation: restore runbook requires the source to be stopped, restore marks non-revoked identities `offline`, and the operator may rotate the instance key; agents pin the instance key fingerprint at enrollment and stop with `instance_changed` when it differs, so a rotated clone cannot silently take the fleet | `docs/RESTORE.md` step (*planned*); protocol test 11 |
| Backup rollback reviving revoked authority | Snapshots exclude sessions, MFA challenges and pairings; restore revokes password grants; revoked agent identities remain revoked because revocation is a row state that the capsule carries; a capsule older than a revocation is a known risk stated in the runbook | `TestRestorePreservesKeysAndRevokesOnlySnapshotGrants`; runbook text |
| Last administrator lockout | Membership changes serialize per organization and refuse to leave zero active administrators; platform repair path is a design decision in `authorization-matrix.md` | `TestLastAdminGuardSurvivesConcurrentDemotion` |
| Browser CSRF/session used to drive agents | Agent routes never accept session cookies; browser routes never accept agent signatures; distinct middleware and namespaces (`/api/agent/v1` vs `/api/organizations`) | route tests (*planned* with PR 08) |
| Denial of service through streams or inventory | Per-endpoint and per-user stream caps, bounded buffers with explicit gaps, inventory generations, metric sampling limits | retention soak (*planned*) |

## Operating constraints

- Run the agent only on hosts whose operators accept host-equivalent access from the control plane.
- Keep `/data` on a filesystem that honours 0700/0600; the server tightens loose modes, refuses symlinked key files, and logs what it changed.
- Stop the original instance before restoring its capsule elsewhere.
- Rotate registry credentials after any suspected agent compromise; revoke the agent first.
- Set `KY_REGISTRY_ALLOW_PRIVATE` only when an organization needs a registry on the operator's own network; organization administrators can then set a registry's `allow_private`, and without it they cannot.
- Forward-only migrations: recover into a separate compatible installation from a verified backup rather than downgrading a binary.

## Decisions

| Decision | Proposed | Status |
|---|---|---|
| Docker socket proxy | Not in 0.1; disclosure instead | proposed |
| Cloned-identity handling | Restore marks identities offline; operator-confirmed instance key rotation forces re-approval | proposed |
| Compromised control plane | Blast-radius reduction and audit only; no containment claim | proposed |
