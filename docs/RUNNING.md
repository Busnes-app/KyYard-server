# Running

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Start KyYard

Published image:

```bash
docker compose up -d
docker compose logs kyyard
```

Open **http://localhost:9273** on the Docker host. Sign in as `admin` with the temporary
password from `docker compose exec kyyard cat /data/initial-admin-password` and replace it;
the file is deleted when you do. **Containers** shows **Local Docker** automatically; no enrollment command or fingerprint approval is needed for this host. The HTTP port is published on loopback only.
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

Maintainers can recover a missing release run with `gh workflow run ci.yml --ref master`. The manual run repeats every CI check, then publishes and attests the tested image through the same release jobs as a push. Dispatches on other branches run checks only.

`AGENTS.md` is the contract for working in this repository.

## First sign-in

Every bootstrap or `init-admin` password must be replaced, including when
`KY_ADMIN_PASSWORD` supplies it. Operator resets revoke existing sessions, MFA challenges and
device pairings immediately, reactivate local admins and require replacement at the next login. Sign in, enter the current password and a different password
of at least 12 characters, then sign in again. Until replacement, the session can only check
its identity, change the password or sign out; privileged APIs remain blocked. Replacement
revokes existing sessions, MFA transactions and device pairings atomically. Existing accounts
are not retroactively flagged, since the server cannot infer whether they still use a bootstrap password.

Five failed sign-ins for one username, from anywhere, delay the next attempt by one minute,
doubling to at most 15; a successful sign-in or a restart clears it. A browser that has
signed in as that username before keeps a device cookie and backs off on its own, so someone
guessing at your account does not delay you there. Unknown and disabled accounts answer
exactly like a wrong password.

## Account security

Local password accounts manage their own sign-in in **Settings → Security**:

- **Two-factor authentication:** *Set up* shows a setup key and an `otpauth://` link for an
  authenticator app (no QR code). Enter the current password and a code to turn it on; ten
  one-time recovery codes are then shown once. Afterwards password sign-in asks for a code or a
  recovery code. *Replace recovery codes* needs a current code and voids the old set. *Turn off*
  needs the password and a code or recovery code. To re-enrol, turn it off first.
- **Change password:** current password (plus a code when MFA is on) and a new password of at
  least 12 characters. Every other session, MFA transaction and device pairing is revoked; this
  browser stays signed in.

Accounts provisioned by an SSO provider have no local password and manage these at the provider.
Keep the recovery codes. An authenticator lost with every recovery code spent needs the host
operator: `init-admin` for that username sets a new password and turns MFA off, and the
account enrols again after signing in.

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

The default SQLite profile uses Docker's existing `bridge` network. The base Compose file creates no KyYard network.
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

A containerized Nginx Proxy Manager is supported with the shipped
`docker-compose.proxy-container.yml` overlay (Compose 2.24 or later). KyPost owns
`kypost-net`; create it through KyPost before starting consumers. Declare the proxy
on that external network with a fixed `ipv4_address` in its own Compose file.

Append `:docker-compose.proxy-container.yml` to
`COMPOSE_FILE`, after the optional PostgreSQL overlay. Set `KY_PROXY_IP` to NPM's
fixed IP address without a prefix; KyYard normalizes it to exactly one address
(`/32` for IPv4 or `/128` for IPv6). Set
`KY_APP_URL` to the public HTTPS origin and point NPM at `http://kyyard:9273` with
WebSocket support. The host HTTP publish is removed; an SSH forward to that port
is unavailable in this profile. The default network remains for the optional
PostgreSQL service, whose DNS name is `postgres`; use that name with a valid
certificate and `sslmode=verify-full` in the configured DSN. Database TLS stays required.

Run `docker compose config --quiet` before starting. Every peer on the shared
suite network can reach the plaintext backend, but only the fixed proxy address
is trusted for forwarded headers. Never trust the whole subnet. Declare network
membership persistently; a one-off `docker network connect` is lost on recreation.


Optional overlays are appended to the existing `COMPOSE_FILE` chain, preserving the build,
bind and DNS overlays already in use:

- `docker-compose.bind.yml`: existing host directory at `./data` instead of the named volume.
- `docker-compose.postgres.yml` (Compose 2.24 or later): the pinned PostgreSQL 18
  image and KyYard on the private default Compose network, without a published database port. Set `KY_POSTGRES_PASSWORD`, `KY_POSTGRES_TLS_DIR` and `KY_DB_DSN`.
  The TLS directory contains `server.crt`, `server.key` and `ca.crt`; the overlay enables
  server TLS and mounts only the public CA file into KyYard at `/etc/kyyard-db-ca.crt`.
  Use `sslmode=verify-full&sslrootcert=/etc/kyyard-db-ca.crt` in the DSN. Its host must match
  the certificate SAN; use `postgres` for the overlay’s service DNS name.
  A recycled IP then fails certificate verification before credentials are sent.
  The PostgreSQL image's `postgres` user must own/read the key with mode 0600; alternatively
  use root ownership, mode 0640 and that user's group. Prepare the files before starting;
  missing or invalid certificates fail startup. See [PostgreSQL TLS setup](https://www.postgresql.org/docs/18/ssl-tcp.html).
  Existing database volumes need the same TLS setup before enabling this overlay. Capsule
  backups support SQLite only. A separately managed PostgreSQL server with a stable address
  and verified TLS is also supported by setting the server's database environment directly.
  Run exactly one server per database: every start settles all in-flight commands as
  `unknown`, including another live server's, so a second server (a rolling deploy or a
  stray compose project) corrupts the first one's command results.
- SSO: configure providers in **Settings → Sign-in**. OIDC discovery supports KyIdentity;
  OAuth 2 providers can supply explicit endpoints and JSON profile field mappings. Register the
  displayed callback URL with the provider. For an OIDC provider that supports back-channel
  logout (KyIdentity does), also register `<KY_APP_URL>/api/sso/<provider ID>/backchannel-logout`
  (the callback URL with `callback` replaced) so signing out there ends the KyYard session.
  Without it a KyYard session lasts until it expires or the user signs out here. The upgrade
  that adds back-channel logout signs out every account with an SSO identity, linked local
  accounts included, and drops pending MFA sign-ins: their sessions name no provider login a
  logout could end. New
  identities require an organization membership grant. SCIM and phone pairing are not part
  of KyYard. Local login remains available.
  To use SSO with an existing local account, sign in locally and open **Settings → Sign-in →
  Connect single sign-on to your account**. Supply the current KyYard password, then sign in
  to the provider as the identity to connect. The same account ID and permissions are retained;
  local sign-in and operator password recovery remain available. Each provider identity can
  belong to one account, and an account can connect one identity per provider. Linked local accounts retain their configured TOTP requirement: complete the KyYard authenticator or recovery-code challenge after provider sign-in before receiving a session.

`GET`/`HEAD /health/live` reports process availability; `/health/ready` additionally probes
the database with a two-second deadline and returns 503 during shutdown or database failure.
They expose only `status`, never connection strings, keys or backend errors. The image
healthcheck calls readiness using the binary, without loading keys or creating data.

### Server settings

Settings the compose files do not list themselves (`KY_ADMIN_PASSWORD`, `KY_OIDC_*`,
`KY_AGENT_IMAGE` and the rest of the last section of `.env.example`) go in `.env.server`
beside `docker-compose.yml`, never in `.env`, which only feeds the compose files. Compose
passes that file into the container as-is when it exists. Git ignores it; it holds secrets:

```bash
(umask 077; printf 'KY_ADMIN_PASSWORD=%s\n' '<at least 12 characters>' >> .env.server)
docker compose up -d
```

Write a literal `$` as `$$`, or single-quote the value. A setting a compose file sets itself
(`KY_APP_URL`, `KY_HOST`, `KY_TRUSTED_PROXIES` through the proxy overlay) wins over this file,
and an `env_file` in your own override wins over it.
