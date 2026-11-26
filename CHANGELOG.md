# Changelog

## Unreleased

- **PostgreSQL 17 to 18: a return to 18 after a rollback is refused, not silent.** The
  vendored move script is the Ky kit v4. After a rollback that started 17 again, the next `up`
  with the PostgreSQL overlay no longer starts the older 18 cluster, which lacks what 17 wrote.
  `postgres-17-dump` exits with an error that names two choices: move 17 again or keep 18
  ([Back to 18 after a rollback](docs/RUNNING.md#postgresql-17-to-18)). Removing
  `upgrade-from-17` stays safe. **Upgrade impact:** none for installs that never rolled back.

- Keep SQLite `v1.62.0` with its upstream-required libc `v1.80.1` and memory `v1.12.1`; defer independent updates of those runtime dependencies.

- Prepare application version `0.6.1` (from `0.6.0`) for the compatible dependency refresh. Rebuild or install the updated application; this version bump requires no additional data, account, key or configuration migration.

- Refresh compatible dependency versions and lockfiles. Rebuild or install the updated application; no database, configuration, account or key migration is required.

- The short soak regression checks functional bounds under race instrumentation
  without treating shared-runner scheduling delays as a capacity result. Its report
  explicitly marks latency unqualified. The soak command retains the 500 ms p95
  gate, now also refusing a run with no completed reads; deterministic tests cover
  its boundary. **Upgrade impact:** no server behavior or resource limit changes.

### Changed on upgrade

- **The PostgreSQL overlay moves to PostgreSQL 18.** The next `docker compose up` dumps the 17
  database once and restores it into a new `postgres` volume; the 17 `pgdata` volume is kept for
  rollback and removed only by you. It needs about twice the data size free. See
  [PostgreSQL 17 to 18](docs/RUNNING.md#postgresql-17-to-18). SQLite installs are unaffected.

- **Stopping no longer waits up to 17 minutes for a backup.** On SIGTERM a backup in flight is
  interrupted: the snapshot and the upload to KyRecovery are cancelled, the interruption is
  audited, and the scheduled backup runs again about a minute after the next start. An
  interrupted `POST /api/backup/deposit` answers 503. An update-policy run in flight still
  finishes, so shutdown takes seconds without one and up to 3 minutes with one.
  `docker-compose.yml` lowers `stop_grace_period` from 20m to 4m; a Kubernetes
  `terminationGracePeriodSeconds` of 240 is enough.

- Agent exec, inspection and configuration grants refused only for expiry now return a refusal without losing the authenticated connection. Malformed grants, wrong scope/nonce/runtime and all replays still end it. Consumed IDs remain for the socket lifetime, with a reconnect at the existing 1,024-ID bound instead of evicting replay evidence.

- **SQLite retention budget is now 192 MiB (was 2 GiB).** One backup capsule holds 256 MiB, so a
  database kept to the old budget could grow past what any backup can carry. Existing SQLite
  installs that did not set `KY_RETENTION_DISK_BUDGET` prune older history on upgrade: metrics
  fall back to the shorter window the budget allows. PostgreSQL keeps 2 GiB.
- **A SQLite `KY_RETENTION_DISK_BUDGET` above 256 MiB refuses startup while backups are on.**
  Lower it, or turn backups off (the schedule off on the Backup page or by
  `KY_BACKUP_DEPOSIT_INTERVAL=0`, and no `KY_BACKUP_DIR`). The
  Backup page warns from 80 % of a capsule and says when backups fail; a failed backup's audit
  row names the size limit.
- **Upgrade the server before cluster agents.** A cluster agent from this release advertises
  `kubernetes.names.subdomain`, which an older server refuses at hello.

### Fixed

- Kubernetes workloads and pods with dotted or long (up to 253 characters) names have their
  pages and controls. An older cluster agent refuses them until it is upgraded.
