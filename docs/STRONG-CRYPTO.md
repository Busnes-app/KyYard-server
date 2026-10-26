# Supported key-strength and TLS configuration

[README](../README.md) · [Restore](RESTORE.md) · [Assurance case](ASSURANCE-CASE.md)

Use this configuration with the matching source/image revision. It covers the
server and each KyYard agent. Check the actual endpoints and independent HTTPS
edge before rollout. It is not FIPS certification or a verified live deployment.

## Preserve identity, access and recovery

Retain the original database, `/data` volume or bind mount, `session.key`,
`encryption.key`, `instance.key`, protected environment and installer credential
files, registry credentials, provider records, organization grants, endpoint
identities, recovery pin/token, sealed capsules, receipts and custodian cards.
The instance key is the fingerprint agents pin; replacing it is not a TLS fix.
Keep each Docker agent's identity directory and command/deployment ledgers; keep
each Kubernetes agent's identity Secret, service account/RBAC, scratch volume and
original enrollment binding. Restarting in this mode must not re-enroll, approve
or resurrect revoked agents, replace credentials or reset tenant authority.

Pin merged primitives source `bae5c14dba4c`
(`v0.10.2-0.20261026050236-bae5c14dba4c`). It adds the TOTP floor and carries
existing capsule-budget, Shamir-sampling and setup-refusal repairs. Key, share
and `kycap/3` formats are unchanged. Keep historical capsules and original custody;
test a known restore before upgrading. A normal restart preserves sessions; a
capsule restore deliberately strips authentication grants under the existing
restore contract. This profile does not change that distinction.

## Keys in every mode

| Boundary | Required configuration and behavior |
| --- | --- |
| Session/encryption/instance | Retain the original three 32-byte keyfiles. Existing `KY_SESSION_SECRET`/`KY_ENCRYPTION_KEY` overrides must decode to exactly 32 bytes in hex or base64. The loader already refuses malformed keys without replacing them. Session signing normalizes to hex; instance identity is Ed25519. |
| Legacy directory webhook | Nonempty `KY_KYSIGNON_HMAC_SECRET` needs at least 16 literal bytes at startup and signature use. Prefer independent random bytes, e.g. `openssl rand -hex 32`; length alone does not establish entropy. Empty leaves this webhook unconfigured. Coordinate any replacement with its sender. |
| Managed installer directory | Supply an independent protected directory signing secret of at least 16 literal bytes. Shared `syncauth.Verify` enforces this at use; protected credential-file parsing alone does not establish its strength. Preserve the existing setup record, identity assignments and client/directory references. |
| OIDC | Every fetched RSA JWKS member needs at least 2048 actual modulus bits. The private input guard uses the maintained verifier's envelope/member parsing, bounds input and refuses redirects. Legacy, generic and UI/installer OIDC paths share this guard; signature, issuer/audience/expiry, nonce, role claims and upstream key handling remain maintained-library behavior. Strong EC/Ed25519 signing keys remain available. |
| TOTP | Generate 20 random decoded bytes; verification requires at least 16 under the shared RFC 4226/6238 implementation. Short legacy secrets remain readable in their original sealed records but cannot authenticate. Use an independently enrolled recovery method or an authorized administrator reset, then re-enroll with the issuer; never edit the saved key to pad it. |
| Agent/source credentials | Agent identity signatures use Ed25519 and enrollment credentials use existing random material, one-use semantics, fingerprint approval and revocation. Registry passwords/OAuth client secrets are peer-issued credentials, not locally generated encryption keys. Do not replace them to satisfy a superficial length check. |
| Recovery | Seal only to the pinned suite public key with the shared fixed cryptographic suite. The product server does not receive custodian shares. This is control-plane recovery; Docker volumes and Kubernetes workload data retain their separate backup obligations. |

Weak signing keys now fail rather than verify. Rotate weak IdP keys at the
provider; restart to discard an old in-memory verifier/cache. Preserve independent
local administrator and recovery access throughout credential/MFA migration.

## Enable the Go TLS profile on every process

Use the toolchain in `go.mod` and the pinned image. Set `GODEBUG=fips140=on`
before starting the server, every remote agent and every remote Go CLI process.
Preserve other needed GODEBUG entries with `fips140=on`, then restart. Use `on`,
not the diagnostic `only` mode: Argon2id, HMAC-SHA1 TOTP and fixed-suite recovery
remain available. Go's verified TLS-chain policy rejects undersized keys,
including explicitly trusted RSA roots below 2048 bits; custom CA files do not
waive the floor. Normal chain and hostname verification remains enabled.

The profile covers project Go HTTPS/WSS: agent enrollment/reconnect, identity
providers, OAuth token/profile calls, registry metadata, KyRecovery, Kubernetes
service-account API/exec TLS and verified PostgreSQL TLS. Server healthcheck and
built-in local-agent traffic are private local transport; Docker and suite-runner
access use Unix sockets. The Docker daemon independently performs image pulls:
this process setting does not configure the daemon, its registry policy or
workload containers. Keep normal verified registry HTTPS there and verify its
independent transport configuration before rollout.

Use direct HTTPS OIDC issuer, authorization, token and JWKS URLs without URL
credentials/fragments. Configured mode also refuses legacy discovery/token
redirects; existing configured-provider redirects remain refused. HTTPS discovery
cannot downgrade its endpoints even outside this mode. All saved UI/installer
providers need inspection; environment defaults do not establish their settings.
Keep PKCE, nonce, local-account linking, app-role and tenant checks intact.

For a native/Docker agent, retain the original `--identity-dir`, server origin,
CA file and ledgers and supply GODEBUG in its process/container environment. A
failed handshake must be repaired at the peer; do not erase its identity or
repeat approval. For Kubernetes agents, add the same environment entry to the
existing Deployment while retaining its Secret/RBAC/scratch mounts and image
pin. Its in-cluster client uses the service-account CA and HTTPS API; do not
replace it with an insecure client. Verify the actual cluster/exec peer too.

For Compose, append `docker-compose.strong-crypto.yml` to the protected `.env`
`COMPOSE_FILE` chain. Preserve all base/build/bind/proxy/PostgreSQL/LAN-DNS files,
the original named volume and operator env_files. Existing settings absent from
the shipped environment list still come from `.env.server` or the operator's
later env_file; do not forward all of `.env` or add per-name overrides. If another
file sets GODEBUG, retain its needed entries with `fips140=on` in the final value.
Inspect resolved configuration privately because it contains secrets.
`python3 docs/check-strong-crypto-compose.py` compares seven complete chains with
synthetic credentials and starts no services.

The SQLite profile keeps its current paths and backup arrangement. Native CLI
processes must explicitly export the existing `KY_PORT` and `KY_DB_DRIVER=sqlite`
for the restore drill environment checks; the shipped Compose base already
supplies both. A bare image invocation must supply the driver too. Container
CLI commands must run as the server UID (`docker exec --user 10001:10001 ...`),
so original owner-only key checks remain in force; a default root exec is refused. Remote
PostgreSQL must use `sslmode=verify-full` with the correct CA/hostname and strong
keys. The optional PostgreSQL overlay requires real TLS files; this profile does
not add PostgreSQL capsule snapshots or change its separate backup procedure.

## Independent HTTPS edge and rollout

Retain the documented HTTPS origin, Secure cookies, exact trusted proxy address
and private backend binding/network. Use TLS 1.2/1.3 with a verified RSA-2048+ or
P-256+ certificate chain. For a host Nginx/OpenSSL proxy, relevant policy is:

```nginx
ssl_protocols TLSv1.2 TLSv1.3;
ssl_ciphers 'DEFAULT@SECLEVEL=2';
```

Set actual certificate/key paths and routes and verify the deployed edge. Go's
setting does not configure the proxy, browsers, Docker daemon or other products.
Preserve the selected host/proxy-container networking contract and original
volume; this overlay does not create a network or expose a port.

Before rollout, run complete ordinary/configured SQLite/PostgreSQL race suites,
frontend checks, required fuzz counts, real Docker regressions/smoke, installer
OIDC regression, module/vulnerability/static checks, image build/agent-install,
Compose preservation, repeatable server/agent binaries and recovery checks.
Then verify real sign-in/MFA/recovery, assigned owner/everyday access and
revocation, registry/agent/cluster peers, container operations, backup receipts
and a custody-correct offline restore. Local fixtures do not prove the deployed
edge, a real cluster or the human acceptance/capacity gates.

References: [RFC 4226](https://www.rfc-editor.org/rfc/rfc4226#section-4),
[RFC 6238](https://www.rfc-editor.org/rfc/rfc6238),
[Go FIPS runtime mode and limits](https://go.dev/doc/security/fips140).
