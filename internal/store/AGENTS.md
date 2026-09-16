# Storage Layer

## Purpose
Provides the unified Database Abstraction Layer (DAL) supporting pluggable backends (SQLite zero-CGO default and PostgreSQL enterprise) with automated dialect-aware migrations.

## Ownership
Owns data models, store interfaces (`UserStore`, `SessionStore`, `DeviceStore`, `GroupStore`, `AuditStore`, `SettingsStore`, `TenancyStore`), dialect translations, and schema migrations.

## Local Contracts
- Migration 5 creates organizations, fixed-role/status memberships, environments and organization-local groups. Legacy SCIM groups remain global identity data and confer no tenant membership. Composite group/member foreign keys enforce same-organization references; deleting a membership cascades its group links, so rejoining does not restore them.
- Tenant roles are `organization_admin`, `environment_admin`, `operator`, `developer`, `read_only`. Global `User.Role=admin` remains platform authority and is not a tenant role. Role-to-action enforcement follows in the next slice; these persistence methods do not authorize callers or filter disabled records. New HTTP routes must add that authorization before exposing them.
- Every environment/group operation requires its organization ID; names are unique within that organization. `GetMembership` returns status for management; permission checks must additionally require both an active membership and an active user. No global-user/SCIM role implies tenant access.
- `Tenancy.Initialize` runs after account bootstrap, once per database: it grants initial organization administration to the active local admin named `admin`, otherwise the oldest active local admin (creation time then ID). With users but no eligible local admin, it completes without a grant; future administration must explicitly add membership. With zero users it fails and rolls back. Other organizations never receive implicit memberships.
- Bootstrap claims its singleton marker, inserts the membership and records `organization.bootstrap` in one transaction. Concurrent calls serialize on the marker; failed calls retry. Restarts and account resets never regrant removed or disabled memberships. SQLite capsule snapshots include the tenant tables and marker automatically.
- `CompletePasswordChange` atomically compares the old password, updates a flagged local account, clears the flag, deletes sessions/MFA challenges/device pairings and records `auth.password_changed`. Session/MFA issuance locks the same user row against the verified hash; MFA challenges persist the creation-time password hash, and consumption returns that snapshot to reject stale completions. Migration 4 discards preexisting challenges because their credential snapshot is unknown.
- `ResetAdminPassword` reactivates a local administrator with the replacement flag set and shares the atomic grant purge and audit path with `CompletePasswordChange`; it also works for disabled accounts.
- `store.Open(ctx, cfg)` initializes and auto-migrates the configured database backend.
- SQLite defaults to WAL mode. Every connection DSN independently enables foreign keys even when custom tuning pragmas are supplied; open verifies `PRAGMA foreign_keys=1` and fails closed on conflicting options. Tenant reference/cascade guarantees depend on this check.
- PostgreSQL queries are rebound dynamically from standard positional parameters.
- MFA challenges and device pairings are consumed with database state transitions that permit exactly one successful use.
- Recovery-code hash updates use optimistic concurrency so simultaneous redemption cannot reuse a code.

## Verification
- `go test -v ./internal/store/...`

## Child DOX Index
None.
