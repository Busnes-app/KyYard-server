# kyyard-server

The simple control plane for your container fleet.

This first slice establishes the KyYard scaffold: Go backend, embedded React PWA,
SQLite/PostgreSQL, local authentication, MFA, configurable OAuth 2/OIDC (including KyIdentity) and KyRecovery.
Container inventory, lifecycle controls, image operations and logs are available after [enrolling a Docker host](#show-containers-after-installation). Remaining milestones are tracked in [KyYard-Implementation-Plan.md](KyYard-Implementation-Plan.md).
SAML metadata is inherited; SAML login is not yet implemented.

Fresh installations only: KyYard uses its own recovery service identity and token-sealing
label. Existing base-project databases, pairing tokens and capsules are not a supported
in-place migration. The SQLite filename `ky_server.db` remains the inherited storage format.
Container data lives under `/data`, including local capsules under `/data/backups` in Compose.
KyYard generates persistent secrets without manual configuration. Compose runs one
container with SQLite and a named `kyyard-data` volume; SSO is optional and configured in Settings.

## Start KyYard

Published image:

```bash
docker compose up -d
docker compose logs kyyard
```

Open **http://localhost:9273** on the Docker host. Sign in as `admin` with the temporary
password printed in the logs and replace it. The HTTP port is published on loopback only.
`docker compose ps` reports health; `docker compose exec kyyard /app/kyyard-server healthcheck`
checks readiness. Restarting preserves accounts, sessions and keys. Do not use `down -v`
unless you intend to delete the data volume.

**Existing `./data` installs:** before updating, append `:docker-compose.bind.yml` to your
existing `COMPOSE_FILE` in `.env` (or set `COMPOSE_FILE=docker-compose.yml:docker-compose.bind.yml`).
Check `docker compose config` still mounts your original directory at `/data`. Otherwise
the new named-volume default would start a separate, empty instance.

Source install (never paste this into a published-image install: the build overlay wins over a
`KY_IMAGE` digest pin, and a source install must set this line before its first `up -d` on a
new checkout; an install from before the published image existed has no such line yet, so run
this block once and confirm with `docker compose config --images`, which must print
`kyyard:local` rather than the `ghcr.io` name):

```bash
make ci        # gofmt, vet, race tests, smoke test
(umask 077; t=$(mktemp ./.env.XXXXXX) && touch .env \
  && cf=$({ grep '^COMPOSE_FILE=' .env || [ $? -eq 1 ]; } | tail -n1 | cut -d= -f2-) && cf=${cf:-docker-compose.yml} \
  && case ":$cf:" in *:docker-compose.build.yml:*) ;; *) cf="$cf:docker-compose.build.yml";; esac \
  && { grep -v -e '^COMPOSE_FILE=' .env || [ $? -eq 1 ]; } > "$t" \
  && printf 'COMPOSE_FILE=%s\n' "$cf" >> "$t" && mv "$t" .env)
docker compose up -d
```

Update a published-image install on the rolling tag:

```bash
docker compose pull && docker compose up -d
```

A digest-pinned install (`KY_IMAGE` in `.env`) gets nothing from `pull`: re-run the pin recipe in
`docker-compose.yml` with the commit sha you want first, or delete that line to follow `:latest` again.

`AGENTS.md` is the contract for working in this repository.

## First sign-in

Every bootstrap or `init-admin` password must be replaced, including when
`KY_ADMIN_PASSWORD` supplies it. Operator resets revoke existing sessions, MFA challenges and
device pairings immediately, reactivate local admins and require replacement at the next login. Sign in, enter the current password and a different password
of at least 12 characters, then sign in again. Until replacement, the session can only check
its identity, change the password or sign out; privileged APIs remain blocked. Replacement
revokes existing sessions, MFA transactions and device pairings atomically. Existing accounts
are not retroactively flagged, since the server cannot infer whether they still use a bootstrap password.

## Transport and advanced deployment

The bare binary defaults to `127.0.0.1:9273`; the image listens on `0.0.0.0:9273` inside
its container, while Compose publishes only `127.0.0.1:9273` on the host. `KY_APP_URL`
defaults to `http://localhost:9273` (or the configured port). Use that exact origin in your
browser: writes from a different origin are refused, including login. Cookies are HttpOnly
for sessions and SameSite, and their Secure flag follows the advertised URL.
`KY_COOKIE_SECURE`, if supplied, must agree with the URL scheme. HTTP advertised URLs are
accepted only for localhost/loopback. A non-loopback HTTP listen address (or a hostname whose
resolution cannot be assumed safe) additionally requires `KY_ALLOW_PLAINTEXT_BIND=true`.
Compose sets this beside its loopback-only host publish; the image deliberately does not.
A bare `docker run` must either configure an HTTPS reverse proxy or explicitly set this
acknowledgement and publish with `-p 127.0.0.1:9273:9273`. It does not add encryption or
restrict the socket: do not expose that HTTP backend port to a LAN or the internet.
`KY_ENV` has been removed; there is no separate development/production security mode.

For access from another machine during setup, forward the local port over SSH:
`ssh -N -L 9273:127.0.0.1:9273 user@docker-host`, then open http://localhost:9273 locally.
For shared access, terminate TLS at your reverse proxy with a valid certificate, route to
this private backend, and preserve the browser Host and Origin headers. Append
`:docker-compose.proxy.yml` to `COMPOSE_FILE` in `.env` and set:

```dotenv
KY_APP_URL=https://yard.example.com
# The proxy's actual peer address as seen by KyYard, not an entire shared Docker network.
KY_TRUSTED_PROXIES=172.18.0.2/32
```

Replace the example with your proxy's address. A host proxy — one running as a service on
this machine — arrives on loopback or through Docker's bridge gateway, and the published port
stays where it is.

KyYard uses Docker's existing default `bridge` network. Compose creates no KyYard network.
The backend is reachable by other containers on that bridge; the host publish stays
loopback-only. This deployment trusts co-resident containers and the Docker host. A loopback
host publish does not isolate the container's own port from bridge peers. Do not use this
packaging for mutually untrusted workloads without host-enforced isolation or a secured backend.

Docker cannot pin container addresses on the default bridge; addresses are recycled. Do not
put a dynamically allocated proxy address in `KY_TRUSTED_PROXIES`. Use a host-service proxy,
or run your containerized proxy in the host network namespace, targeting `127.0.0.1:9273`
with WebSocket support. Trust only the observed host/bridge-gateway peer and keep that gateway
fixed in Docker daemon configuration; never trust the entire bridge subnet. A remote proxy
needs a stable host address and a separately secured backend connection. Do not change the
KyYard container bind to `127.0.0.1`: Docker port forwarding targets its bridge interface.
`docker-compose.proxy-network.yml` remains a no-op compatibility overlay for existing
`COMPOSE_FILE` chains; `KY_PROXY_NETWORK` is no longer used.

Optional overlays are appended to the existing `COMPOSE_FILE` chain, preserving the build,
bind and DNS overlays already in use:

- `docker-compose.bind.yml`: existing host directory at `./data` instead of the named volume.
- `docker-compose.postgres.yml`: PostgreSQL 17 on the existing Docker bridge, without a
  published database port. Set `KY_POSTGRES_PASSWORD`, `KY_POSTGRES_TLS_DIR` and `KY_DB_DSN`.
  The TLS directory contains `server.crt`, `server.key` and `ca.crt`; the overlay enables
  server TLS and mounts only the public CA file into KyYard at `/etc/kyyard-db-ca.crt`.
  Use `sslmode=verify-full&sslrootcert=/etc/kyyard-db-ca.crt` in the DSN. Its host must match
  the certificate SAN and resolve to the database (the bridge has no `postgres` service DNS).
  A recycled IP then fails certificate verification before credentials are sent.
  The PostgreSQL image's `postgres` user must own/read the key with mode 0600; alternatively
  use root ownership, mode 0640 and that user's group. Prepare the files before starting;
  missing or invalid certificates fail startup. See [PostgreSQL TLS setup](https://www.postgresql.org/docs/17/ssl-tcp.html).
  Existing database volumes need the same TLS setup before enabling this overlay. Capsule
  backups support SQLite only. A separately managed PostgreSQL server with a stable address
  and verified TLS is also supported by setting the server's database environment directly.
- SSO: configure providers in **Settings → Single sign-on**. OIDC discovery supports KyIdentity;
  OAuth 2 providers can supply explicit endpoints and JSON profile field mappings. Register the
  displayed callback URL with the provider. New identities require an organization membership
  grant. SCIM and phone pairing are not part of KyYard. Local login remains available.

`GET`/`HEAD /health/live` reports process availability; `/health/ready` additionally probes
the database with a two-second deadline and returns 503 during shutdown or database failure.
They expose only `status`, never connection strings, keys or backend errors. The image
healthcheck calls readiness using the binary, without loading keys or creating data.

## Initial organization

Startup creates the default organization and assigns one active local administrator once:
`admin` if present, otherwise the oldest active local admin. Existing ordinary, disabled and
federated accounts are not automatically enrolled. If no eligible local admin exists, the
migration completes without assigning a member; adding a local admin later does not silently
claim it. Platform administration remains separate from organization membership.

Membership removal or disabling survives restart and administrator password resets. SSO and directory sync provision accounts only: no external role, group or attribute grants
organization access, an IdP deactivation denies access live and leaves the grant in place for
reactivation, and a deleted account's grants are gone for good. Legacy SCIM groups do not
grant organization access. Tenant APIs enforce live membership on every
operation; platform administrators need explicit membership too. Membership administration is available through the tenant API and the web UI: pick an
organization in the header, then manage environments, members and audit history under
`/organizations/{organization}`. Links are deep-linkable and survive sign-in. Provider settings are available under Settings → Single sign-on.

### Tenant API

Use `/api/organizations/{organization}` (`org_initial` for the default organization):

| Method | Suffix | Operation |
|---|---|---|
| GET | empty | Organization details |
| GET / POST | `/environments` | List / create environment |
| GET / PATCH / DELETE | `/environments/{environment}` | Read / rename / delete |
| GET | `/audit` | Organization audit history |
| GET | `/environments/{environment}/audit` | Environment audit history |
| GET | `/members` | List memberships (user, username, role, status) |
| POST | `/environments/{environment}/enrollment-tokens` | Mint a one-time agent enrollment token and command |
| GET | `/endpoints`, `/environments/{environment}/endpoints` | List endpoints |
| GET / PATCH | `/endpoints/{endpoint}` | Read / rename an endpoint |
| POST | `/endpoints/{endpoint}/approve`, `/reject`, `/revoke` | Review or terminate an endpoint |
| PUT / DELETE | `/members/{user}` | Set role/status for an existing user / remove membership |

`GET /api/organizations` (no suffix) lists the signed-in user's own active organizations and
roles; it never reveals anyone else's. Membership writes take `{"role":"operator","status":"active"}`
(status optional). An organization must keep one active administrator, so demoting, disabling or
removing the last one returns `409` with code `last_administrator`, even for that administrator.

Create/rename takes `{"name":"Production"}`. An enrollment token lives 15 minutes, is single use,
and is shown once alongside a Docker command and the host-access disclosure. A host enrolls as
`pending` until an administrator approves its exact key fingerprint.

### Show containers after installation

The server does not automatically inspect the host running it. Enroll an agent on each Docker
host, including the server's own host:

1. On **Containers**, choose **Manage environments & enroll a host**.
2. Create an environment (for example, Production), open it, then choose **Enroll a host**.
3. Run the displayed command on the Docker host running KyYard, using an account allowed to
   run Docker. It first enrolls through an attached, short-lived process, then starts the
   persistent agent with its saved identity. The published image includes `/app/kyyard-agent`; no Go installation or separate
   agent image is needed. For a renamed server container, change `server=kyyard` in the command.
4. Run `docker logs kyyard-agent`. In the site, choose **Refresh hosts**, compare the full
   **agent key fingerprint** with the pending host, then choose **Approve**.
5. Open the host or return to **Containers**. The first inventory arrives after approval;
   refresh the page if necessary. Subsequent reports arrive every minute.

The default command uses the installed server's immutable local image ID (`--pull never`). It
mounts the Docker socket only into the agent, giving it root-equivalent host access, and keeps
its identity in the named `kyyard-agent-identity` volume. It shares the server container's
existing network namespace so HTTP remains on loopback; the server stays on the default Docker
bridge and no dedicated network is created. Do not use this command on a different host.

**Server replacement or upgrade:** the same-host agent shares the old server container's network
namespace. Stop/remove the agent with `docker rm -fv kyyard-agent`, replace the server, then run
this command on that host to reconnect the existing identity using the new installed image:

```sh
server=kyyard
server_id=$(docker inspect --type container --format '{{.Id}}' "$server") &&
image=$(docker inspect --type container --format '{{.Image}}' "$server_id") &&
docker run -d --name kyyard-agent --restart unless-stopped --pull never --no-healthcheck \
  --network "container:$server_id" --entrypoint /app/kyyard-agent \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v kyyard-agent-identity:/var/lib/kyyard-agent "$image"
```

A fresh enrollment command refuses an existing identity and names its endpoint/server; use the
restart command above to reconnect it. To enroll into another environment, revoke the old
endpoint and explicitly choose a new identity volume for the new enrollment.

Keep the named identity volume; `docker rm -fv` removes anonymous volumes, not named ones.
No new token or approval is needed for that existing identity. A revoked endpoint cannot be
revived this way. Never share an identity volume between hosts or control planes. Use distinct
agent container/volume names if running more than one installation on a host.

**Another Docker host:** configure KyYard's advertised `KY_APP_URL` as reachable HTTPS and
set `KY_AGENT_IMAGE` to the published **KyYard** image pinned by digest, for example
`ghcr.io/busnes-app/kyyard@sha256:<verified-digest>`. Follow the image-verification instructions
in [RESTORE.md](docs/RESTORE.md); this is the same image and attestation as the server. Add
`KY_AGENT_IMAGE` to the Compose service's environment (putting it in `.env` alone does not pass
it into the container). The next enrollment command uses that image's `/app/kyyard-agent`
entrypoint on Docker's default bridge and connects over HTTPS. Tags are refused. With a
loopback HTTP advertised URL the command remains same-host, even if an image is configured.

**Source installation:** `make build` produces `./kyyard-agent`. Expand **Source installation
or another Docker host** in the enrollment panel to obtain the one-time token, and pipe it to
`./kyyard-agent --server <server-origin> --identity-dir <private-directory>` on the Docker host.
The process needs access to `/var/run/docker.sock`; keep it running with your service manager.
HTTP is accepted only for a host binary connecting to loopback; remote connections require HTTPS.

### Agent

`kyyard-agent` (built by `make build`) reads the enrollment token from stdin on first start, keeps its
Ed25519 identity in `--identity-dir` (default `/var/lib/kyyard-agent`, 0700), pins the control
plane's instance fingerprint, and holds one outbound WebSocket to `/api/agent/v1/connect` with a
30-second heartbeat, reconnecting with backoff. Plaintext `http://` is refused except to loopback.
It stays `pending` until approved, becomes `active` on its first inventory report, is shown
`offline` after three missed heartbeats, and exits when revoked. It reports a bounded inventory snapshot (engine facts, containers, images, networks, volumes;
never container environment) from `--docker-socket` at connect and every `--inventory-every`
(default one minute); the endpoint screen shows it with its age, and running containers carry the latest CPU, memory
and network sample (kept six hours, pruned every minute). Every 30 days (`--rotate-every`)
it offers a new key; the old key keeps working until an administrator acknowledges the new
fingerprint on the environment screen, and a second live connection for the same endpoint is
refused, flagged, and blocks rotation until cleared. Behind nginx add the standard
upgrade block from `scripts/spikes/websocket-proxy/nginx.conf` (`proxy_http_version 1.1`,
`Upgrade $http_upgrade`, `Connection $connection_upgrade` via the `map`); Caddy needs nothing. Lists accept `offset` (default 0) and
`limit` (default 50, maximum 200). Browser writes require the existing CSRF token.
Responses include a server-generated `X-Request-ID` for audit correlation.

| Organization membership | Read organization/environments/endpoints | Manage environments and endpoints | Manage members | Read tenant audit |
|---|---|---|---|---|
| Organization administrator | Yes | Yes | Yes | Yes |
| Environment administrator | Yes | Yes | No | No |
| Operator / developer / read-only | Yes | No | No | No |

These memberships apply throughout their organization; individual environment grants and
workload permissions arrive with their corresponding APIs. Successful operations and audit
records commit together. Audit failures prevent mutations. Historical platform events keep
their original meaning and are excluded from tenant history. Existing recovery and instance
settings retain platform authorization; tenant settings and credentials are not exposed yet.

## Persistent keys

First boot creates `KY_DATA_DIR` (default `./data`) privately and creates `encryption.key`,
`session.key` and `instance.key` as private 0600 files. Each contains 32 random bytes encoded
as hex. Restarts reuse them; invalid, truncated, symlink or overly permissive files stop
startup without replacing the key. Keep the directory's ancestors trusted and writable only
by the deployment owner. A final symlink for the data directory is refused.
Startup leaves owner-only directory permissions unchanged. If group/other access is present,
it tightens the directory to 0700 and logs the path and old/new modes; a failure reports
ownership details. This also changes host bind-mount permissions. Host jobs copying sealed
backups must run with access as the directory owner, or use `KY_BACKUP_DIR` outside the
private data directory.

`KY_ENCRYPTION_KEY` and `KY_SESSION_SECRET` are optional overrides: exactly 32 bytes encoded
as hex or base64. They take precedence without overwriting files. Empty values use the files.
Older arbitrary-text session-secret overrides must be replaced with this encoded form;
changing it invalidates pending proof-of-work challenges, not database sessions.
`instance.key` is the persistent Ed25519 seed reserved for control-plane identity.
It has no environment override. Never run a restored copy alongside the original instance.

The bootstrap password is printed only after the administrator is saved and only when it
was generated. An ordinary restart preserves both the account and active sessions.

## Disaster recovery

Every backup is one `.kycap` capsule: the database snapshot, the deployment's encryption, session and instance keys,
the settings that describe the deployment, and the pinned suite recovery public key. It is
sealed to the suite recovery key, which only the custodians' cards (k of n, split at the suite
ceremony) can reconstruct. Nothing on this server, and nothing on KyRecovery, can open one.
The mechanics are `github.com/Busnes-app/ky-primitives/recoveryclient`; this repository
supplies what it seals and how it checks a drill. New snapshots exclude sessions, pending
MFA challenges and device pairings so restore requires fresh sign-in; live sessions remain
untouched. The capsule preserves the instance identity and the active keys, including overrides.

**Capsules are SQLite-only today.** The snapshot is `VACUUM INTO` against the local database
file; on `KY_DB_DRIVER=postgres` there is no snapshot and every backup refuses with "no
consistent database snapshot for this driver". A Postgres deployment must back its database up
itself, with `pg_dump` on its own schedule and its own retention, and must protect that dump:
it is the plaintext of everything a capsule would have sealed. Nothing travels in a capsule
there, because no capsule is made. The recovery key pin, the pairing and the schedule live in
the database and so ride in the `pg_dump`; `data/encryption.key`, `data/session.key`,
`data/instance.key` and `data/recovery.pub` do
not, and you must copy them separately. Without `encryption.key` no TOTP secret and no
KyRecovery token in that dump can be decrypted.

The admin screen **Backup & recovery** shows four facts (recovery key, KyRecovery, local
copies, schedule) and the actions: Back up now, Download capsule, Run restore drill, the
schedule, pairing with Unpair, and pinning the key by hand.

### Two ways to get a key

- **Pair with KyRecovery.** Its dashboard issues a six-digit code; entering it here hands this
  server the suite public key and a deposit credential. The key is pinned once and never
  replaced: a later pairing that returns a different key is refused.
- **Pin the key by hand.** For a server with no KyRecovery: paste the base64 public key the
  ceremony page shows, with its k-of-n. Capsules then go only to the local directory.

### Why TLS matters here

The capsule is sealed, so a copy of it is worthless to an eavesdropper. What the wire does
carry is the suite public key at pairing (trust on first use), the deposit credential, and
each receipt. A man in the middle at pairing could substitute a key whose shares they hold,
so pairing over plain HTTP is refused outright, and a pairing across your own network should
be checked: compare the key ID on the screen with the ceremony card, or pin the key by hand
and skip the question.

### One run, every destination

Back up now, the schedule and the `deposit` command all do the same thing: seal one capsule
and deliver it to every configured destination. A pinned key with no destination is refused
with a message that says so. A local write that fails does not stop the deposit, and a
refused deposit does not remove the local copy.

### Environment

| Variable | Default | Meaning |
|---|---|---|
| `KY_RETENTION_DISK_BUDGET` | `2147483648` (2 GiB) | Bytes of database the telemetry may occupy. At 95 % metrics are refused, at 100 % inventory too, and the raw sample window closes to one hour so pruning reclaims. Heartbeats, endpoint state and audit are never refused. `0` disables the check. |
| `KY_BACKUP_DIR` | empty (off) | Directory for sealed local copies, `<escaped app name>.<capsule-id>.kycap` at mode 0600 (`KyYard.cap-KyYard-<n>.kycap` by default: bytes outside `[A-Za-z0-9-]` in the app name are hex-escaped). Pruning removes only this application's own prefix. |
| `KY_BACKUP_KEEP` | `7` | Local copies to retain; below 1 refuses startup. |
| `KY_BACKUP_DEPOSIT_INTERVAL` | `24h` | Default schedule only. The admin screen's setting wins; `0` is off; 15 minutes to 366 days otherwise. |
| `KY_BACKUP_ALLOW_PRIVATE_RECOVERY` | `false` | Admit a KyRecovery on an RFC1918 or CGNAT address behind your own TLS proxy. Loopback, link-local and other reserved ranges stay refused; HTTPS stays required. Logged at startup and on the pairing audit row. |
| `KY_DNS` | unset | Only in `docker-compose.lan-dns.yml`: the container's resolver, for names that exist only on your LAN. |

Reach a KyRecovery that only your LAN's DNS knows:

The snippet appends `docker-compose.lan-dns.yml` to whatever `COMPOSE_FILE` chain `.env` already
holds (build overlay, local override) and leaves the rest of the chain alone; the resolver and the private-recovery flag
sit next to it: the resolver comes from an exported
`KY_DNS` (`export KY_DNS=<addr>`; fish: `set -x KY_DNS <addr>`) or, when that is unset, from the `KY_DNS` line
already in `.env`; there is no default, the block refuses to guess. An exported value overrides
`.env`, so re-running is a no-op only while `KY_DNS` is unset in your shell; the flag is set to true. One block for every install type:

```bash
(umask 077; touch .env \
  && cf=$({ grep '^COMPOSE_FILE=' .env || [ $? -eq 1 ]; } | tail -n1 | cut -d= -f2-) && cf=${cf:-docker-compose.yml} \
  && dns=${KY_DNS:-$({ grep '^KY_DNS=' .env || [ $? -eq 1 ]; } | tail -n1 | cut -d= -f2-)} \
  && : "${dns:?no resolver chosen: export KY_DNS=<your LAN resolver> (fish: set -x KY_DNS <addr>), then re-run this block}" \
  && case ":$cf:" in *:docker-compose.lan-dns.yml:*) ;; *) cf="$cf:docker-compose.lan-dns.yml";; esac \
  && t=$(mktemp ./.env.XXXXXX) && { grep -v -e '^COMPOSE_FILE=' -e '^KY_DNS=' -e '^KY_BACKUP_ALLOW_PRIVATE_RECOVERY=' .env || [ $? -eq 1 ]; } > "$t" \
  && printf 'COMPOSE_FILE=%s\nKY_DNS=%s\nKY_BACKUP_ALLOW_PRIVATE_RECOVERY=true\n' "$cf" "$dns" >> "$t" && mv "$t" .env)
docker compose up -d --force-recreate
docker inspect kyyard --format '{{.HostConfig.Dns}}'   # must print the resolver you chose
```

Turning it off: remove the resolver and the flag, strip only `docker-compose.lan-dns.yml` from
`COMPOSE_FILE` (a build overlay or local override in the chain survives), and recreate:

```bash
(umask 077; t=$(mktemp ./.env.XXXXXX) && touch .env \
  && cf=$({ grep '^COMPOSE_FILE=' .env || [ $? -eq 1 ]; } | tail -n1 | cut -d= -f2- | tr ':' '\n' | grep -vx docker-compose.lan-dns.yml | paste -sd: -) \
  && { grep -v -e '^COMPOSE_FILE=' -e '^KY_DNS=' -e '^KY_BACKUP_ALLOW_PRIVATE_RECOVERY=' .env || [ $? -eq 1 ]; } > "$t" \
  && { [ -z "$cf" ] || [ "$cf" = docker-compose.yml ] || printf 'COMPOSE_FILE=%s\n' "$cf" >> "$t"; } && mv "$t" .env)
docker compose up -d --force-recreate
```

`KY_DNS` takes effect only while `docker-compose.lan-dns.yml` is in `COMPOSE_FILE`, but
`KY_BACKUP_ALLOW_PRIVATE_RECOVERY` persists in `.env` on its own and keeps relaxing destination checks until you
remove it.

### Restoring

`docs/RESTORE.md` is the runbook: opening a capsule with the custodians' cards, putting the
result in service, and what to distrust afterwards. Drill it once a quarter with real cards.

## Upgrading after the Busnes-app owner move

The GitHub organisation was renamed on 2026-09-16 and the image now lives at `ghcr.io/busnes-app/kyyard`. The project no longer controls `ghcr.io/busness-app`; GHCR does not redirect it, and anything served under that name must be treated as untrusted. If `KY_IMAGE` in `.env` still names the old namespace, re-pinning is required, not optional: inspect `git remote -v` before any `git pull`, `make ci`, or `docker compose` command, and replace a retired-owner remote with `https://github.com/Busnes-app/KyYard-server.git` (prefer a fresh clone plus a known commit). Then remove `KY_IMAGE` to follow the compose default or verify and pin a digest using `docs/RESTORE.md` before pulling.
