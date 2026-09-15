# ky_server_base

The scaffold every Busnes.app server is built from: Go backend, embedded React PWA, SQLite or
PostgreSQL, local and federated sign-in (KySignOn, OIDC, SAML), SCIM provisioning, and
disaster recovery through the suite's KyRecovery.

Published image:

```bash
make ci        # gofmt, vet, race tests, smoke test
make run       # build and start on :8080; first start prints the bootstrap admin password
docker compose up -d
```

Source install (never paste this into a published-image install: the build overlay wins over a
`KY_IMAGE` digest pin, and a source install must set this line before its first `up -d` on a
new checkout; an install from before the published image existed has no such line yet, so run
this block once and confirm with `docker compose config --images`, which must print
`ky_server_base:local` rather than the `ghcr.io` name):

```bash
make ci        # gofmt, vet, race tests, smoke test
make run       # build and start on :8080; first start prints the bootstrap admin password
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

## Disaster recovery

Every backup is one `.kycap` capsule: the database snapshot, the deployment's encryption key,
the settings that describe the deployment, and the pinned suite recovery public key. It is
sealed to the suite recovery key, which only the custodians' cards (k of n, split at the suite
ceremony) can reconstruct. Nothing on this server, and nothing on KyRecovery, can open one.
The mechanics are `github.com/Busness-app/ky-primitives/recoveryclient`; this repository
supplies what it seals and how it checks a drill.

**Capsules are SQLite-only today.** The snapshot is `VACUUM INTO` against the local database
file; on `KY_DB_DRIVER=postgres` there is no snapshot and every backup refuses with "no
consistent database snapshot for this driver". A Postgres deployment must back its database up
itself, with `pg_dump` on its own schedule and its own retention, and must protect that dump:
it is the plaintext of everything a capsule would have sealed. Nothing travels in a capsule
there, because no capsule is made. The recovery key pin, the pairing and the schedule live in
the database and so ride in the `pg_dump`; `data/encryption.key` and `data/recovery.pub` do
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
| `KY_BACKUP_DIR` | empty (off) | Directory for sealed local copies, `<escaped app name>.<capsule-id>.kycap` at mode 0600 (`Busnes_2eapp.cap-Busnes.app-<n>.kycap` by default: bytes outside `[A-Za-z0-9-]` in the app name are hex-escaped). Pruning removes only this application's own prefix. |
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
docker inspect ky_server_base --format '{{.HostConfig.Dns}}'   # must print the resolver you chose
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

### Upgrading from plaintext local backups

Earlier builds wrote unencrypted backups into `KY_BACKUP_DIR`. The variable keeps its name and
now means sealed capsules. Retention deliberately never touches files it did not write, so old
plaintext backups stay where they are: move them out of the directory, keep them until a
restore from a capsule has been proven, then remove them securely. They are the live
directory in the clear.

### Restoring

`docs/RESTORE.md` is the runbook: opening a capsule with the custodians' cards, putting the
result in service, and what to distrust afterwards. Drill it once a quarter with real cards.
