# Backup

## Purpose
Adapts the scaffold to `github.com/Busnes-app/ky-primitives/recoveryclient`, which owns the
KyRecovery pairing, sealing, deposit, restore and drill contract. This package supplies only
what differs per product: a `Settings` adapter over `store.SettingsStore`, a `Sealer` under the
deployment key, the payload the scaffold seals (`Collect`), and the drill's verification
checks (`Checks`).

## Ownership
Owns the settings adapter (`settings.go`), payload collection (`payload.go`), and restore-drill
checks (`drill.go`) and serialized drill entry point (`run_drill.go`). It holds no private key, no share, and no pairing state of its own — those
live in `recoveryclient` and in the settings rows it reads and writes through the adapter.

## Local Contracts
- `Settings` maps `store.ErrNotFound` to `recoveryclient.ErrNotFound`; every other error passes
  through unchanged.
- `NewSealer` seals the KyRecovery token under the deployment key with label
  `kyyard-server:setting:kyrecovery_token`, domain-separated so a row copied from another
  setting will not open. KyYard supports fresh installations; base-project pairing tokens
  are deliberately rejected. The historical v0.5.0 fixture is checked with its original
  base sealer only in tests.
- `Collect` snapshots SQLite with the lib's `SQLiteSnapshot` (`VACUUM INTO`; the store runs in
  WAL mode, so a plain file read misses uncheckpointed commits) and returns
  `ErrNoDatabaseSnapshot` for any other driver, so a capsule without a consistent database is
  never sealed. Only the snapshot is scrubbed of sessions, MFA challenges and device pairings,
  then vacuumed before sealing; the live grants remain usable. It carries the active encryption,
  session and instance keys (`data/encryption.key`, `data/session.key`, `data/instance.key`),
  including active environment overrides, plus `data/recovery.pub` when pinned. Keys are
  required 32-byte values stored as hex with mode 0600. Restore preserves instance identity;
  never run a restored clone alongside the source.
- `Checks(dir, opened)` reads the opened capsule's manifest, normalizes JSON lists and
  fails malformed or incomplete recipes. Required files include all capsule members and
  the database, settings and all three private keys; key encoding/permissions, SQLite integrity and required environment
  checks cannot be disabled. File checks accept only clean relative manifest members;
  SQLite opens read-only and missing/empty databases fail.
- HTTP and CLI call `RunDrill`, which holds an OS advisory lock on `<data dir>/drill.lock`
  across scratch preparation and the library drill. Contention returns `ErrDrillBusy`;
  closing the descriptor or process exit releases ownership. Keep the lock file in place.
  The Unix lock matches the Linux container deployment.
- `DrillRoot` is under the data directory and forced to 0700; opened payloads stay in the
  library's private, disposable subdirectories. Drills use throwaway keys, not custodian shares.
- `Members` names what `Collect` would seal now, for the status route and the screen; keep
  the two in step.
- `GET /api/settings`'s `extra_settings` never carries `kyrecovery_token_enc` or a legacy
  plaintext `kyrecovery_token`; the filter drops every key with the `kyrecovery_token`
  prefix, so a rename in the lib cannot leak. An admin sees the paired `kyrecovery_url`,
  never the credential.
- Pairing, the write-once key pin, `Run` (one seal, every destination), the schedule, local
  copies and their pruning, drill mechanics, restore and the decrypt guard are the lib's;
  their contracts are in the `recoveryclient` README. `client_test.go` pins only what this
  package's wiring buys: `KY_BACKUP_ALLOW_PRIVATE_RECOVERY` admits RFC1918 and CGNAT and
  nothing else.

## Verification
- `go test -v ./internal/backup/...` covers decoded seal/open checks, malformed recipes,
  subprocess lock contention/exit, scratch cleanup and the synthetic v0.5.0 pairing fixture
  in `testdata/pairing-v050.json`. The fixture uses a 32-byte 0x01 deployment key and retains
  no recovery private key.

## Child DOX Index
None.
