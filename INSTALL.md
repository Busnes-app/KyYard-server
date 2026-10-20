# KyYard standalone installation

## Before you start

These steps are for a **new, independent installation** from this repository. Use the
README’s upgrade/restore procedure for an existing instance. Keep its volumes, keys,
configuration and Compose overlays; do not replace its environment files with examples.

Use Linux with Docker Engine and Docker Compose 2.24 or newer, Git, and permission to use Docker.
Choose a release/revision and use its matching Compose files and documentation. Image tags
can move: follow the README/Compose attestation and digest-pinning procedure before relying
on a published image. Source builds use the separate build overlay where provided.

Clone this repository, enter its directory, then run the product steps below:

```sh
git clone https://github.com/Busnes-app/KyYard-server.git
cd KyYard-server
```

Each service needs its own directory and persistent storage. Several products use host
port 8080 by default; install them on separate hosts or configure distinct host ports and
matching public origins. For remote users, provide a TLS reverse proxy and a hostname you
control. Trust only the proxy’s actual peer address, and forward the original scheme/client
address according to the product’s proxy guide.

## Install the fleet console

The default installation needs a local Docker Engine socket. Mounting it gives KyYard
host-level authority; local administrator accounts do not require KyIdentity. The default
local connection is automatic, while remote hosts/clusters need enrollment.

```sh
[ -e .env ] || (umask 077; cp .env.example .env)
chmod 600 .env
docker compose config --quiet
docker compose pull
docker compose up -d
docker compose exec kyyard cat /data/initial-admin-password
docker compose exec kyyard /app/kyyard-server healthcheck
```

Open exactly `http://localhost:9273`, sign in as `admin`, replace the temporary password
and enable TOTP under account security. Keep the recovery codes. **Containers** should
show **Local Docker**. Review inventory/logs before operating real workloads.

Use `KY_DOCKER_SOCKET_PATH` for a rootless/nonstandard host socket. Setting
`KY_DOCKER_SOCKET` empty disables the built-in connection but leaves the default socket
mount and its authority; remove that mount in your deployment if it should have none.
The host HTTP publish is loopback-only; co-resident Docker bridge peers can still reach
the container. This deployment trusts the host and those peers.

For remote HTTPS access, append `docker-compose.proxy.yml` to the `COMPOSE_FILE` chain in
`.env`, set `KY_APP_URL=https://yard.example.com` and the exact `KY_TRUSTED_PROXIES`.
`.env` configures Compose; extra runtime settings such as `KY_ADMIN_PASSWORD` and SSO go
in a separate mode-0600 `.env.server`. Compose’s explicit environment settings take
precedence. See the [transport and deployment guide](docs/RUNNING.md#transport-and-advanced-deployment).

The named `kyyard-data` volume at `/data` keeps accounts, keys and capsules. Existing bind
mount installations must retain `docker-compose.bind.yml`; do not start an empty named
volume over them. Source builds retain `docker-compose.build.yml` in `COMPOSE_FILE` and
use `docker compose up -d --build`. SAML metadata does not provide SAML login. Additional
host enrollment, suite runner handoff and recovery are separate README procedures.

## Keep it recoverable

Preserve the database **and deployment keys**, not just the image. Configure recovery using
the linked restore runbook, retain an independent copy, and complete a scratch restore with
custodian cards before treating recovery as verified. A synthetic drill uses a throwaway
key and does not prove that the real cards are available or correct.

For upgrades, back up first, read the chosen release’s migration notes, and reuse the same
Compose project, volume mounts and overlay selection for every command. `docker compose
pull` followed by `docker compose up -d` updates a published-image installation; a digest pin
must be deliberately changed after verification. A source install must keep its build
overlay and rebuild. `docker compose down -v` deletes managed volumes: it is not an upgrade.

## What verification means

The startup checks above prove only the checks they perform. Use the README’s development
section and the repository CI workflow for source/build tests; a passing health response or
screenshot is not a substitute. Record the revision, commands, environment and skips when
reporting results. Design plans and dated evidence remain records of their stated scope.
