# Recovery

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

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
`instance.key` is the persistent Ed25519 seed of the control-plane identity: agents pin its
fingerprint at enrollment, so replacing it disconnects every agent. It has no environment override. Never run a restored copy alongside the original instance.

A generated bootstrap password is written to `initial-admin-password` in the data directory
(owner-only, 0600) and never logged; the log names the file once the administrator is saved.
The file is deleted when `admin` replaces the password. A supplied `KY_ADMIN_PASSWORD` must meet
the 12-character policy or startup refuses. With Compose, put it in `.env.server`, not `.env`
(see [Server settings](RUNNING.md#server-settings)). An ordinary restart preserves both the account and
active sessions.

## Disaster recovery

Every backup is one `.kycap` capsule: the database snapshot, the deployment's encryption, session and instance keys,
the settings that describe the deployment, and the pinned suite recovery public key. It is
sealed to the suite recovery key, which only the custodians' cards (k of n, split at the suite
ceremony) can reconstruct. Nothing on this server, and nothing on KyRecovery, can open one.
The mechanics are `github.com/Busnes-app/ky-primitives/recoveryclient`; this repository
supplies what it seals and how it checks a drill. New snapshots exclude sessions, pending
MFA challenges and device pairings so restore requires fresh sign-in; live sessions remain
untouched. The capsule preserves the instance identity and the active keys, including overrides.
It is the control plane only: no workload volumes, images, container data or anything from a
remote host, which stay the hosts' own backup problem ([docs/RESTORE.md](RESTORE.md)).

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
| `KY_REGISTRY_ALLOW_PRIVATE` | `false` | Let organization administrators mark a registry `allow_private`, admitting RFC1918 and CGNAT registry addresses. Off, the server refuses the flag, so a tenant cannot aim the server at your network. Loopback and link-local stay refused. Logged at startup. |
| `KY_CONTAINER_ALLOW_PRIVILEGED` | `false` | Let a container edit or run set host-level settings: privileged, devices, security options other than `no-new-privileges`, capabilities beyond Docker's defaults, the host's or another container's network (as the mode or an attachment), or a bind of a system path: `/`, `/etc`, `/run`, `/var/run`, `/var/spool`, `/var/lib`, `/usr`, `/lib`, `/lib64`, `/bin`, `/sbin`, `/proc`, `/sys`, `/dev`, `/boot` or `/root`, a parent of one, or anything beneath one other than `/`, except a read-only bind of `/etc/localtime`, `/etc/timezone` or `/etc/ssl/certs` (or beneath it); and any path with a `.ssh` segment. Off blocks these as a guard against mistakes and the obvious escalations. It is not a boundary against an organization administrator holding `container.configure`, who can still acknowledge a bind of any other host path; each acknowledged bind is audited per path. On makes that administrator root on every Docker host. Logged at startup. |
| `KY_SUITE_RUNNER_SOCKET` | empty (off) | The suite runner's Unix socket, set by KyQuickStart's hand-off. Absolute and canonical, or startup is refused; its directory must be private to KyYard's user and is rechecked on every connection. See [Suite management](INTEGRATIONS.md#suite-management). |
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
