# Restoring KyYard from a capsule

This runbook covers KyYard capsules from fresh KyYard installations. Base-project capsules
and encrypted pairing tokens require their original product; they are not an in-place migration path.

This is the procedure for bringing a server built on this scaffold back from a `.kycap`
backup after the original is gone. It needs three things, held by three different parties by
design:

| Thing | Who has it |
|---|---|
| The capsule (`.kycap`) | KyRecovery, or the local backup directory, or a downloaded copy |
| k custodian cards | The custodians from the suite ceremony (k is usually 2 of 3) |
| A machine to restore on | You |

Nobody can do this alone. KyRecovery cannot open a capsule. One custodian cannot. The server
that made the backup never could. That is the point, and it is also why you should run this
procedure once as a drill before you ever need it.

The `docker compose` commands below use the base file alone, which runs the published
image. Source install: confirm the `COMPOSE_FILE` line in `.env` contains `docker-compose.build.yml`
before the first command; extra overlays beside it, such as `docker-compose.lan-dns.yml`, are
fine (the quickstart in `README.md` adds it). Check: `grep '^COMPOSE_FILE=' .env | grep -q
docker-compose.build.yml && echo ok`. Otherwise a restore silently pulls a different binary than
the one you built and are running.
Published install: never restore onto a floating `:latest`; the step before the restore
command pins and verifies a digest.

## What a capsule holds

Everything a fresh server needs to be the old one:

| Path in the capsule | What it is |
|---|---|
| `data/ky_server.db` | Users, MFA enrolments, devices, SCIM groups, organizations, environments, endpoints and their keys, commands, applications, deployments, registries, audit log, settings and sealed KyRecovery token; sessions, pending MFA challenges and device pairings are removed from new snapshots |
| `data/encryption.key` | 32 bytes. Every TOTP secret, registry credential, application environment value, SSO provider secret and the KyRecovery pairing token are encrypted under it |
| `data/session.key` | 32-byte proof-of-work challenge key, including the active environment override |
| `data/instance.key` | 32-byte Ed25519 identity seed; restore preserves the control-plane identity |
| `data/recovery.pub` | The suite recovery public key, so the restored server comes back pinned (present when the backup had a key) |
| `config/settings.json` | App name, URL, port, database driver. For your reference when re-deploying; nothing reads it |

The restored directory is the live directory in the clear. Treat it like the running server's
`data/`.

**A capsule is the control plane only.** It holds no workload volumes, no images, no container
data and nothing from a remote host. Those live on the hosts and are the hosts' own backup
problem; KyYard's application removal keeps named volumes for the same reason. A restore brings
back what KyYard knows about the hosts, not what runs on them.

**The capsule records its schema, not its commit.** The recipe carries `schema_version`, the
latest migration of the binary that sealed it. No commit is recorded: the manifest prints
`v1.0.0` for every build. `restore` prints `capsule schema version N; this binary migrates to
M`. Compare the two numbers:

- Equal: this binary runs the capsule's schema as it is.
- M is greater: the first start migrates the database forward, and that is one way. The drill
  compares a capsule with its own snapshot, so a drill on the newer binary passes either way
  and cannot tell you a migration happened; only this line does. Accept it knowingly, or pick a
  commit whose `internal/store/migrations` ends at N.
- M is smaller: the server refuses to start with `database schema version N is newer than this
  binary (M)`. Use a binary whose migrations reach N.

The drill check `Schema Version: data/ky_server.db` fails with `database schema is version N,
capsule expects M` when a snapshot and its own recipe differ, typically a data directory last
run by a newer binary than the one sealing.

**This procedure is for SQLite deployments.** A capsule carries `data/ky_server.db` because the
collector snapshots SQLite with `VACUUM INTO`; on `KY_DB_DRIVER=postgres` no snapshot is
possible, so no capsule is made at all and there is nothing here to restore from. Back a
Postgres deployment up with `pg_dump` on its own schedule, guard that dump as the plaintext of
everything above, and copy `data/encryption.key`, `data/session.key`, `data/instance.key` and `data/recovery.pub` separately — the
recovery key pin, the pairing and the schedule are rows in the database and come back with the
dump, but nothing in it can be decrypted without `encryption.key`.

## Before you start

- **Pick the capsule.** In the KyRecovery dashboard, open Capsules, find the newest one for
  this service (the app name, `KyYard` unless `KY_APP_NAME` was set) that is not flagged
  corrupt, and note its `capsule_id`, `created_at` and `digest`. You will compare these after
  the restore. From a local backup directory the file is `<escaped app name>.<capsule-id>.kycap`
  (`KyYard.cap-KyYard-<n>.kycap` by default); the newest is the one to use unless
  you have a reason.
- **Gather k custodians.** Each card carries one share, a single line beginning `ky2-`. They
  type or paste it themselves; do not collect the shares in a file, a chat, or an email. Two
  shares in one place is the suite key in one place.
- **Prepare an empty directory** on a machine you trust, ideally the one that will run the
  restored server. The restore refuses a directory that is not empty.

## Step 1: open the capsule

With the binary (from a release, or `go build ./cmd/server`):

```bash
kyyard-server restore -capsule KyYard.cap-XXXXXXXX.kycap -to ./restored
```

`-service` defaults to `KY_APP_NAME`, then `KyYard`. Pass it only when the backup was made
under a different app name; the capsule's service name must match or the restore stops before
reading a share.

For a published-image install, and always on a fresh recovery machine, pin the commit you
intend to run (normally the current tip; see the schema note above) to a digest you have
verified before it reads a single share (`gh` must be logged in). Name the commit yourself.
Tags are movable, `:<commit sha>` included, so the chain also checks that the attestation records
your commit as its source: the guarantee is the commit you named, not whatever the tag points at. The
chain stops at the first failure and renames a same-directory staging file over `.env` only
if the filtered copy was written in full, so your secrets are never truncated. The pin persists
in `.env` after the drill: see the README's upgrade note for moving off it. Images built before 2026-09-16 can no longer be verified by name: the owner they were attested under is not held by this project, so do not point `--repo` or `--cert-identity` at it. Pin a commit built after that date, or build that commit from source with `docker-compose.build.yml`.

```bash
sha=<full commit sha you intend to run, e.g. $(git rev-parse origin/master)>
d=$(docker buildx imagetools inspect ghcr.io/busnes-app/kyyard:$sha --format '{{.Manifest.Digest}}') \
  && gh attestation verify "oci://ghcr.io/busnes-app/kyyard@$d" --repo Busnes-app/KyYard-server \
       --cert-identity https://github.com/Busnes-app/KyYard-server/.github/workflows/ci.yml@refs/heads/master \
  && [ "$(gh attestation verify "oci://ghcr.io/busnes-app/kyyard@$d" --repo Busnes-app/KyYard-server \
       --cert-identity https://github.com/Busnes-app/KyYard-server/.github/workflows/ci.yml@refs/heads/master \
       --format json --jq '.[0].verificationResult.statement.predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit')" = "$sha" ] \
  && (umask 077; t=$(mktemp ./.env.XXXXXX) && touch .env && { grep -v '^KY_IMAGE=' .env || [ $? -eq 1 ]; } > "$t" \
      && echo "KY_IMAGE=ghcr.io/busnes-app/kyyard@$d" >> "$t" && mv "$t" .env) \
  && grep -qxF "KY_IMAGE=ghcr.io/busnes-app/kyyard@$d" .env
```

Then, in the same shell (the check compares against `$d`), refuse to go on unless the image in
effect is exactly that digest. A source install passes on its `kyyard:local` build instead,
since `docker-compose.build.yml` wins over the pin, which is what a source install wants. The
two refusal messages are distinct on purpose: a broken invocation is not an unpinned image.

```bash
imgs=$(docker compose config --images) || { echo 'refusing: compose could not resolve the image'; false; }
printf '%s\n' "$imgs" | grep -qxF "ghcr.io/busnes-app/kyyard@$d" || printf '%s\n' "$imgs" | grep -qxF 'kyyard:local' \
  || { echo "refusing: image in effect is '$imgs', not the digest verified above"; false; }
```

With Docker Compose, from the repository directory, mount the capsule and an empty target
directory into a one-off container. Create the target yourself at mode 700 and run the
container as your own user, so what comes out is owned by you and not by root. The image's
entrypoint is the binary, so the subcommand goes straight after the service name; `--no-deps`
keeps the real server down:

```bash
mkdir -m 700 restored
docker compose run --rm --no-deps --user "$(id -u):$(id -g)" \
  -v "$PWD/KyYard.cap-XXXXXXXX.kycap:/in.kycap:ro" \
  -v "$PWD/restored:/restored" \
  kyyard restore -capsule /in.kycap -to /restored
```

The bare binary needs none of this: it creates a missing target itself at mode 700.

The command prompts:

```
Paste custodian shares, one per line, then Ctrl-D:
```

Each custodian enters their share on its own line. After the k-th, press Ctrl-D. Shares are
read from stdin only, never from the command line, because argv is world-readable and lands
in shell history.

Only for a rehearsal with synthetic test shares, never with real cards, stdin can be a file.
Delete it afterwards; a file holding k shares is the suite key in a file.

On success it prints the authenticated manifest:

```
Restored 4 files from capsule cap-KyYard-1788605720094118543
  service:      KyYard (v1.0.0)
  created:      2026-09-05T12:15:20Z
  recovery key: 886ff52c...
  payload hash: 8a053985...
  capsule schema version 27; this binary migrates to 27
```

**Check it against KyRecovery's record.** The capsule ID and `created` must match the
deposit record you noted. Opening has already proved the bytes are intact and were sealed to
the suite key; matching the ID and time against the blind store's record is what proves this
is the capsule you meant, not an older one someone substituted.

Failures you may see, and what they mean:

| Message | Meaning |
|---|---|
| `capsule is for service "KyYard", this instance is "X"` | `-service` or `KY_APP_NAME` names something else. Override `-service` only if the backup was made under a different app name |
| `shamir: fewer shares than the threshold requires` | Fewer than k valid lines were read. Check for a missed line or a truncated paste |
| `restore target directory is not empty` | Use an empty directory. The restore never overwrites |
| a decrypt or integrity error | Wrong shares (from a different ceremony), a share mistyped, or a damaged file. Re-download and retry with the custodians |

## Step 2: check what came out

```bash
find restored -type f -printf '%m %p\n'
```

Expect three or four files, all mode `600`, under `restored/data` and `restored/config`.
`cat restored/config/settings.json` shows the app URL and port the old server ran with.

## Step 3: put it in service

**Docker Compose.** The default uses a named volume. This restore procedure deliberately
switches to a host directory using `docker-compose.bind.yml`; retain the old named volume
until recovery is verified. Stop the original first with `docker compose down` (never `-v`).
Append `:docker-compose.bind.yml` to the existing `COMPOSE_FILE` in `.env`, or set
`COMPOSE_FILE=docker-compose.yml:docker-compose.bind.yml` if none exists. Preserve any build,
proxy and DNS overlays. Run `mkdir -p data` and verify `docker compose config` mounts that
host directory at `/data` before continuing.

The destination bind mount `./data` must be empty before the copy, for the same reason Step 1 demands
an empty directory: a capsule carries `ky_server.db` but never its `-wal` and `-shm`
sidecars, and a write-ahead log left over from the old database would be replayed into the
restored one at first open, mixing two databases.

```bash
docker compose down
ls -A data | wc -l
```

That must print `0`. If it does not, the old directory still holds data, and you keep a copy
of it before anything else: it holds every change made after the capsule was sealed, and it
is the only record Step 5 can walk. The container runs as root, so the files are root-owned;
copy as root into a directory you create at mode 700:

```bash
mkdir -m 700 old-data
sudo cp -a data/. old-data/ && sudo ls -A old-data | wc -l
```

The count must equal the count above and the command must exit 0. `old-data/` is now the
old live directory in the clear, with the same key the capsule holds; it is removed in
"Afterwards", not before Step 5 is done.

Only with the copy confirmed, empty the directory. This is irreversible:

```bash
sudo rm -rf data/* data/.[!.]*
ls -A data | wc -l
```

With `0` confirmed, copy the restored files in and start:

```bash
sudo cp -a restored/data/. data/ && sudo chmod 600 data/*
docker compose up -d
```

Keep `KY_APP_URL` and `KY_APP_NAME` identical to the old deployment, from
`config/settings.json`: the app name is what every capsule is sealed under and what
KyRecovery pinned for the pairing token.

The restored key files contain the active keys used at backup time. If the old deployment
supplied `KY_ENCRYPTION_KEY` or `KY_SESSION_SECRET`, the environment wins when both are
present: remove those overrides to use the restored files or supply the same values.
Restore preserves `instance.key`; stop the original before starting the restored instance.
Older capsules lack session/instance keys, so startup generates those keys for them; they
are not a way to recover an identity that was established later. Never print a key to a terminal or type
one on a command line: it lands in scrollback, session recordings and shell history. If you
must produce the hex form, write it straight into the compose project's `.env` with
`umask 077` and nothing else on stdout.

**Bare binary.** Point `KY_DATA_DIR` at `restored/data`, set `KY_APP_URL` and `KY_APP_NAME`
as before, and start.

A capsule taken before migration 30 that holds usernames differing only by case makes the
restored server refuse to start with `usernames differ only by case: …; rename or delete one
of each pair before upgrading`; nothing is changed. Rename or delete one account of each pair
in `data/ky_server.db` as the README's upgrade notes show, then start again.

## Step 4: prove it

1. Open the app URL and sign in with an existing admin account and its second factor. TOTP
   working proves `encryption.key` is right.
2. Open Backup & recovery. If the backup had a key, the recovery key shows as pinned with the
   same key ID as before; compare it with the ceremony card. If the backup was paired, the
   sealed token came across in the database, so the restored server can deposit again
   without re-pairing: click Back up now to prove it. If the screen says the key is missing,
   `data/recovery.pub` did not come across; re-pair, which is refused unless KyRecovery hands
   back the same key.
3. Check the audit log: the last events before the restore are there, followed by your
   sign-in.

## Step 5: decide what to trust

The restore proves the service works. It does not make the restored state current or safe.
Everything comes back as of the capsule's `created_at`: users, passwords, MFA enrolments,
paired devices, SCIM state, organizations, endpoints, applications and registries. Anything
you revoked or changed after that moment is undone. New capsules exclude sessions, pending MFA challenges and device pairings from
the snapshot. Older capsules and external database dumps may still contain them.

1. For an older capsule or external database dump, revoke authentication grants before
   anyone signs in (also safe to repeat for a new capsule):

   ```bash
   docker compose down
   sudo sqlite3 data/ky_server.db 'DELETE FROM sessions; DELETE FROM mfa_challenges; DELETE FROM device_pairings;'
   docker compose up -d
   ```

   Everyone signs in again. After hardware loss that is enough.
2. Walk the old audit log in `old-data/ky_server.db` from `created_at` to the moment the old
   server was lost (the restored server's log stops at `created_at`), and re-apply what
   happened after the capsule: disabled accounts, rotated passwords, removed devices, reset
   MFA, SCIM changes.
3. Re-check endpoints. The audit walk lists every `endpoint.revoke` after `created_at`:
   a capsule from before a revocation brings that agent identity back, so revoke it again on
   the environment screen (Hosts, Revoke). Hosts enrolled after `created_at` are unknown to
   the restored server; enroll them again. Every command in flight at the capsule moment
   shows `unknown` with `the server restarted before a result arrived` (one
   `endpoint.commands.reconciled` audit row per endpoint), and nothing sends them after the
   restart. A deployment that was applying shows `unknown` until its agent reconnects and
   re-sends the result, which it keeps for 24 hours. Read the application's deployment
   history before planning again. A capsule taken between a deployment settling and the
   agent's next inventory report shows the application's mapping as "adoption changed" until
   that agent reconnects and reports; that is drift detection working, not data loss.
4. If the reason for the restore was a suspected compromise rather than hardware loss, treat
   the restored secrets as exposed and rotate the ones that can be rotated. A restore from
   before a compromise brings the attacker's access back with the service unless you do this.

   **Never rotate `encryption.key`.** Every TOTP secret, registry credential, application
   environment value and the KyRecovery pairing token are encrypted under it. Remove it and
   all of them are gone for good, on a server you just recovered.

   What can be rotated, and how:

   - `session.key` signs proof-of-work challenges, not database sessions. With the server
     stopped and `KY_SESSION_SECRET` unset, remove only `data/session.key`; startup securely
     generates a replacement. If an override is used, replace that encoded 32-byte value in
     its secret store instead. Neither action revokes sessions; use the deletion above.
   - `instance.key` is the control plane's identity: its public key is the fingerprint every
     agent pinned at enrollment, and it derives the built-in local Docker binding. Rotating
     it changes that fingerprint, so every agent refuses the server. Never rotate it during a
     restore, and never copy it to another active installation.
   - `KY_SCIM_TOKEN` is the SCIM bearer. Replace it the same way and give the new value to the
     identity provider. If it was never set, the server mints a fresh one at every start.
   - The KyRecovery pairing token: ask the KyRecovery admin to revoke this service and pair
     again from the screen; the same key comes back, so the pairing is accepted.

   Then have every admin re-enrol their second factor, and confirm with a Back up now so the
   recovered server has a capsule that reflects the rotation.

## Afterwards

- Delete the `restored/` directory once the server runs from its own copy, and `old-data/`
  once Step 5 is done. Both are the live directory in the clear, key included. Files in
  `old-data/` are root-owned after the copy, so `sudo rm -rf old-data`.
- The custodians' cards are unchanged; a restore does not consume them. If a card was
  exposed during the restore (read aloud, photographed, pasted anywhere shared), that is a
  key compromise for the whole suite, not for one server: run a new ceremony.
- Make a backup from the restored server so the newest capsule reflects the recovery.

## Drill it

Run Steps 1 and 2 against the latest capsule on a scratch machine once a quarter, with the
real custodians and their real cards, and then delete the output. The in-app drill proves the
capsule format restores; only this proves the cards do.

The in-app drill and `backup-drill` CLI validate the recipe from the capsule actually opened,
including required files, read-only SQLite integrity, the schema version and
environment-variable presence.
A malformed recipe fails the drill. Concurrent drills on one data directory are refused
(HTTP 409 or a CLI error); retry after the active drill finishes. The OS releases the lock
if the process exits. Keep `data/drill.lock` in place; it holds no secret and must not be
removed to bypass a running drill. Opened scratch data stays under `data/drill` with 0700
permissions and is removed when the drill returns.
