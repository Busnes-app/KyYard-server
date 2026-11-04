# Changelog

## Unreleased

### Changed on upgrade

- **SQLite retention budget is now 192 MiB (was 2 GiB).** One backup capsule holds 256 MiB, so a
  database kept to the old budget could grow past what any backup can carry. Existing SQLite
  installs that did not set `KY_RETENTION_DISK_BUDGET` prune older history on upgrade: metrics
  fall back to the shorter window the budget allows. PostgreSQL keeps 2 GiB.
- **A SQLite `KY_RETENTION_DISK_BUDGET` above 256 MiB refuses startup while backups are on.**
  Lower it, or turn backups off (`KY_BACKUP_DEPOSIT_INTERVAL=0` and no `KY_BACKUP_DIR`). The
  Backup page warns from 80 % of a capsule and says when backups fail; a failed backup's audit
  row names the size limit.
- **Upgrade the server before cluster agents.** A cluster agent from this release advertises
  `kubernetes.names.subdomain`, which an older server refuses at hello.

### Fixed

- An agent whose clock disagrees with the server's no longer drops its session when a container
  page or terminal opens; the terminal says to turn on time sync.
- Kubernetes workloads and pods with dotted or long (up to 253 characters) names have their
  pages and controls. An older cluster agent refuses them until it is upgraded.
