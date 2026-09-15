# Bring every product up to the KySignOn backup spec

**Repo:** ky_server_base, gridlock-server, kypassword-server, kydns-server, kypost-server, kybookmarks-server, kynotes-server
**PR:** none yet; one per product
**Worktree:** none

KySignOn is the reference for product-side backup as of 2026-09-04 evening (kysignon-server
master after PRs #16, #17, #18, #19, #20). Everything below was proven against Yoshi's
homelab: a live pairing to the real KyRecovery behind a TLS proxy on the LAN, a live deposit
KyRecovery holds, a rebuilt Disaster recovery screen, and a restore runbook that survived
three security review rounds. Every other product either has an older shape or nothing.
This hand-off is the delta. Claim a product's board folder before starting.

## The spec

Each line names the KySignOn file that is the reference. Copy behaviour, not text; adapt
names, config style and audit API to the product.

| # | Requirement | Reference in kysignon-server |
|---|---|---|
| 1 | Pair with KyRecovery: send `service_name` = app name, pin key write-once, seal token at rest under the deployment key with a domain-separated label | `internal/backup/client.go` `ClaimPairing`, `recoverykey.go` `StoreRecoveryKey`, `deposit.go` `StorePairing`/`LoadPairing` |
| 2 | Pin the suite public key by hand (base64 + k-of-n), same write-once rule, 409 on a different key | `internal/api/backup_handlers.go` `PinKey`, route `POST .../backup/pin-key` |
| 3 | One run seals once and delivers to every configured destination: local directory when set, KyRecovery when paired; pinned key with no destination is a 412 the screen explains | `deposit.go` `RunBackup`, `Result`, `Outcome`, `ErrNoDestination` |
| 4 | Local backup directory `<PRODUCT>_BACKUP_DIR` + `<PRODUCT>_BACKUP_KEEP` (default 7): files named `<AppName>-<capsule-id>.kycap`, temp+rename, 0600; list and prune only that prefix; a local failure never cancels the deposit | `local.go` `WriteLocalCopy`, `ListLocalCopies`, `localPrefix`; tests `TestPruneLeavesForeignCapsulesAlone`, `TestLocalFailureDoesNotCancelTheDeposit` |
| 5 | Schedule is an admin setting (off, or 15 min to 366 days, bounded in seconds before any Duration math), env var only the default, loop polls the setting every minute, next run counts from the last attempt; route reads the stored value back for the audit row | `schedule.go` `Interval`/`SetInterval`/`NextRun`, `cmd/kysignon/main.go` `backupLoop`, handler `SetSchedule`, route `PUT .../backup/schedule` |
| 6 | Unpair: deletes URL and sealed token rows only; key pin, receipts and local directory stay; a half-cleared pairing is still clearable; text claims only what happens (rows removed, not scrubbed; credential dead only when KyRecovery revokes) | `deposit.go` `ClearPairing`, handler `Unpair`, route `DELETE .../backup/pairing` |
| 7 | Private KyRecovery opt-in `<PRODUCT>_BACKUP_ALLOW_PRIVATE_RECOVERY` (off): admits private + CGNAT, still refuses loopback/link-local/multicast/unspecified/reserved, HTTPS mandatory, logged at startup, recorded on the pairing audit row, refusal names the switch | `client.go` `allowedIP`, `cgnatRange`, `ValidateRecoveryURL(raw, allowPrivate)`, `NewKyRecoveryClient(allowPrivate)` |
| 8 | Compose: `<PRODUCT>_DNS` as an override file (`docker-compose.lan-dns.yml`, `${X_DNS:?...}`), never in the base file; backup env vars passed through | `docker-compose.yml`, `docker-compose.lan-dns.yml` |
| 9 | Screen: four fact cards (recovery key, KyRecovery, local copies, schedule), one action row (Back up now, Download capsule, Run drill), what a capsule carries, schedule form, pairing panel with Unpair, key-by-hand panel; warnings for no key, no destination, schedule off | `web/src/components/AdminBackup.tsx`, CSS `.dr-*` in `index.css` |
| 10 | Every destructive route behind step-up (or the product's equivalent) and listed in the hardening test | `internal/api/hardening_test.go` `destructiveAdminRoutes` |
| 11 | Decrypt guard test: absolute repo root, file-count floor, `capsule.Open`/`Combine`/`FromSeed` only inside `restore` and the drill | `internal/backup/nodecrypt_test.go` |
| 12 | Restore runbook `docs/RESTORE.md`, written against the code and proven in a scratch run | `docs/RESTORE.md` (adapt paths, keys, "prove it" checks, rotation section to the product) |
| 13 | Docs: README says why TLS matters when the capsule is sealed (key at pairing, token, receipts), tells the operator to pin by hand or compare fingerprints, documents every env var above; package AGENTS.md lists the local contracts | `README.md` backup paragraphs, `internal/backup/AGENTS.md` |
| 14 | Old v1 `zero_code_pairing_handoff_spec.md` deleted from the product repo | (kysignon: deleted in #16) |

## Where each product stands (surveyed 2026-09-04 late)

| Product | Has | Owes |
|---|---|---|
| ky_server_base (scaffold) | 1 (sealed token via #19), 3-as-deposit-only, 10, 11, drill, restore CLI, plaintext `KY_BACKUP_DIR` local backups from before capsules | 2, 3 (unify into `RunBackup`), 4 (replace or reconcile the old plaintext local backup with sealed copies), 5 (UI), 6, 7, 8, 9, 12, 13 |
| gridlock-server | same as scaffold (ported by merge-file, #14 + #15) | same as scaffold; port from scaffold by `git merge-file` after scaffold lands |
| kypassword-server | own structure (#22): pair, sealed token, deposit, drill, export, status, `Restore`, decrypt guard, env interval | 2, 3, 4, 5 (UI), 6, 7, 8, 9, 12, 13; check 10 and 11 against the spec's shape |
| kydns-server | own structure (#27): pair, sealed token, deposit, drill, export, status, restore, env interval `KYDNS_BACKUP_DEPOSIT_INTERVAL` | 2, 3, 4, 5 (UI), 6, 7, 8, 9, 11, 12, 13, 14 |
| kypost-server | module path fixed (#173) only | everything 1–14; mail bodies exceed capsule caps, seal metadata/config/keys only and say so on the screen |
| kybookmarks-server | nothing | everything 1–14, plus the earlier hand-off items (scrypt→Argon2id, library recovery codes) |
| kynotes-server | nothing | everything 1–14, plus scrypt→Argon2id and library recovery codes; attachments vs caps is an open question |

## Order

1. **ky_server_base first**, to spec, in one PR. It is what the three cold products lift.
2. **gridlock** by per-file `git merge-file` (scaffold before/after as base/theirs). Four
   conflicts last time: tickets service field, DOX index, imports; gofmt sorts imports
   differently because the module path differs.
3. **kypassword** and **kydns** in parallel: their packages are hand-rolled, so this is
   adding features, not porting; keep their names, add the spec items.
4. **kypost, kybookmarks, kynotes** from the finished scaffold, each in its own PR.

## Careful: what the reviewer caught this week, so you do not repeat it

- Prune only files with your own prefix; never `*.kycap` in the whole directory.
- A failed local write must not stop the deposit; carry the error in the result.
- Bound `interval_sec` as an integer before converting; 2^55 seconds wraps to zero and
  reads as "off". Read the stored value back for the audit row.
- Never put a `dns:` entry in the base compose file; it replaces the host's resolvers.
- Give the CGNAT exemption a name, not a slice index.
- Do not claim a deleted row is "gone from the host"; say rows removed, credential dead only
  when KyRecovery revokes.
- In any dashboard HTML: never HTML-escape a value inside an inline `on*=` handler; use data
  attributes and bind after render. Extend the escaping guard test if the product has one.
- Runbook hazards: verify the volume is empty before copying a restore in (a leftover `-wal`
  replays into the restored database); copy the old volume out first, mode 700, as root,
  verified by count, before any `down -v`; never print a key to stdout; the Docker restore
  target must be writable by the container user (run as `$(id -u):$(id -g)`); session
  revocation is per user unless the product has a global one; give rotation exact commands
  and name the key that must never be rotated (the one the database is encrypted under).
- The decrypt guard walked nothing on its first draft (relative root matched the hidden-dir
  skip). Absolute root, count floor, and plant a `capsule.Open` in `main` once to see it fail.
- Fish: quote `--include='*.go'`; `$SHA[0-9a-f]*` is an array subscript in zsh, use `${SHA}`.

## Proving it

- Unit tests as in the spec rows; `go test ./...` and the web tests green.
- Screen rendered live: run the binary against a throwaway data dir with `<PRODUCT>_BACKUP_DIR`
  set, log in, pin a freshly generated key by hand, click Back up now, confirm a 0600 capsule
  in the directory and audit rows for the pin and the run.
- Live pairing in the homelab needs `<PRODUCT>_BACKUP_ALLOW_PRIVATE_RECOVERY=true` and the
  DNS override on the command line (`X_DNS=192.168.1.1 docker compose -f docker-compose.yml -f
  docker-compose.lan-dns.yml up -d`); a value in `.env` alone does nothing and the container
  must be recreated. Verify with `docker inspect <container> --format '{{.HostConfig.Dns}}'`.
- Restore runbook: run Step 1 against a capsule sealed to a freshly split 2-of-3 key with
  shares on stdin, and each failure mode (one share, wrong service, non-empty target).

## Still unproven anywhere

A restore with the real custodian cards. The runbook's drill section is the test.
