# kyyard-server

The simple control plane for your container fleet.

This first slice establishes the KyYard scaffold: Go backend, embedded React PWA,
SQLite/PostgreSQL, local authentication, MFA, KySignOn/OIDC, SCIM and KyRecovery.
Container management and agent enrollment are planned in [KyYard-Implementation-Plan.md](KyYard-Implementation-Plan.md).
SAML metadata is inherited; SAML login is not yet implemented.

Fresh installations only: KyYard uses its own recovery service identity and token-sealing
label. Existing base-project databases, pairing tokens and capsules are not a supported
in-place migration. The SQLite filename `ky_server.db` remains the inherited storage format.
Container data lives under `/data`, including local capsules under `/data/backups` in Compose.
KyYard generates persistent secrets without manual configuration. Compose runs one
container with SQLite and a named `kyyard-data` volume; SSO and SCIM are off by default.

## Start KyYard

Published image:

```bash
docker compose up -d
docker compose logs kyyard
```

Open **http://localhost:8080** on the Docker host. Sign in as `admin` with the temporary
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

The bare binary defaults to `127.0.0.1:8080`; the image listens on `0.0.0.0:8080` inside
its container, while Compose publishes only `127.0.0.1:8080` on the host. `KY_APP_URL`
defaults to `http://localhost:8080` (or the configured port). Use that exact origin in your
browser: writes from a different origin are refused, including login. Cookies are HttpOnly
for sessions and SameSite, and their Secure flag follows the advertised URL.
`KY_COOKIE_SECURE`, if supplied, must agree with the URL scheme. HTTP advertised URLs are
accepted only for localhost/loopback. A non-loopback HTTP listen address (or a hostname whose
resolution cannot be assumed safe) additionally requires `KY_ALLOW_PLAINTEXT_BIND=true`.
Compose sets this beside its loopback-only host publish; the image deliberately does not.
A bare `docker run` must either configure an HTTPS reverse proxy or explicitly set this
acknowledgement and publish with `-p 127.0.0.1:8080:8080`. It does not add encryption or
restrict the socket: do not expose that HTTP backend port to a LAN or the internet.
`KY_ENV` has been removed; there is no separate development/production security mode.

For access from another machine during setup, forward the local port over SSH:
`ssh -N -L 8080:127.0.0.1:8080 user@docker-host`, then open http://localhost:8080 locally.
For shared access, terminate TLS at your reverse proxy with a valid certificate, route to
this private backend, and preserve the browser Host and Origin headers. Append
`:docker-compose.proxy.yml` to `COMPOSE_FILE` in `.env` and set:

```dotenv
KY_APP_URL=https://yard.example.com
# The proxy's actual peer address as seen by KyYard, not an entire shared Docker network.
KY_TRUSTED_PROXIES=172.18.0.2/32
```

Replace the example with your proxy's address. A host proxy may arrive through Docker's
bridge gateway; a proxy container needs connectivity to KyYard's Docker network. Keep the
published port on loopback. Only listed peers may supply `X-Forwarded-For`; forwarded
scheme headers never change cookie security. HTTPS startup requires this explicit proxy
allowlist. Recreate with `docker compose up -d`, then sign in at the HTTPS URL.
Agent enrollment is planned; secret-bearing remote enrollment will require HTTPS.

Optional overlays are appended to the existing `COMPOSE_FILE` chain, preserving the build,
bind and DNS overlays already in use:

- `docker-compose.bind.yml`: existing host directory at `./data` instead of the named volume.
- `docker-compose.postgres.yml`: PostgreSQL 17, without a published database port. Set
  `KY_POSTGRES_PASSWORD` and a URL-encoded `KY_DB_DSN` such as
  `postgres://kyyard:<encoded-password>@postgres:5432/kyyard?sslmode=disable` in private `.env`.
  This uses the private Compose network; capsule backups support SQLite only.
- SSO/SCIM: explicitly set `KY_SSO_ENABLED=true` / `KY_SCIM_ENABLED=true` in an environment
  overlay after configuring the provider or stable `KY_SCIM_TOKEN`.

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

Membership removal or disabling survives restart and administrator password resets. Legacy
SCIM groups do not grant organization access. This release adds the storage foundation;
tenant permission enforcement and management screens are the next delivery slices.

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
The mechanics are `github.com/Busness-app/ky-primitives/recoveryclient`; this repository
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
