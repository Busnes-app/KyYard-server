# kysignon adopts the scaffold's KyRecovery deposit path

Plan, 2026-09-04. Repo `kysignon-server` (main `d3c781b`), branch `feat/kyrecovery-deposit`,
worktree `.claude/worktrees/deposit`. Source of truth for the product half: `ky_server_base`
master `aa125cf` (`internal/backup`, `internal/api/backup_handlers.go`, `cmd/server/main.go`).

## Decisions applied

- Suite decision 9 (one pinned recovery keypair; per-capsule shares rejected): kysignon's
  per-capsule AES key, hand-rolled Shamir, local custodian kit and split-custody quorum are
  deleted. Custodian cards come from the kyrecovery ceremony. An unpaired instance cannot
  export a capsule (412), as in the scaffold.
- kysignon's encrypted-at-rest KyRecovery token stays. `StorePairing`/`LoadPairing` seal the
  token under `crypto.DeriveKey(EncryptionKey, "kysignon:setting:kyrecovery_token")` as
  `recovery_token.go` does today. Scaffold follow-up: do the same there.
- Step-up gating (`adminStepUpM`) stays on pair-remote, deposit and export-capsule.
- The three deferred hardening notes land here: audit the pin if `StorePairing` fails after
  `StoreRecoveryKey`; `depositLoop` on `context.WithoutCancel`; `Outcome` bounds the digest.
- `SnapshotTo` (live-handle `VACUUM INTO`) is the collector's snapshot; the scaffold's
  re-open path is not ported.
- Extra sealed members kept: `data/jwt_rs256.key`, `data/encryption.key`, `data/secret.key`,
  `config/kysignon.json`.

## Tasks

1. `go get github.com/Busness-app/ky-primitives@v0.4.1`; go 1.26.6.
2. `internal/store`: add `ErrNotFound`; `GetSetting` returns it on no row (audit callers:
   every existing caller treats "" as unset, so map `ErrNotFound`→"" at those sites or leave
   them on a new `GetSettingOr` helper).
3. `internal/backup`: delete shamir.go, capsule.go, client.go, drill.go, payload.go, kit.go,
   recovery_kit.go, recovery_token.go and their tests. Copy scaffold capsule.go, client.go,
   deposit.go, drill.go, recoverykey.go; adapt: flat config (`cfg.DataDir`,
   `cfg.EncryptionKey`, `cfg.AppName`), `Snapshotter` for the DB, sealed members above,
   encrypted token, `SettingsStore` = kysignon's `{GetSetting, SetSetting}` with ctx dropped.
4. `internal/config`: `AppName` (env `KYSIGNON_APP_NAME`, default "KySignOn"),
   `BackupDepositInterval` (`KYSIGNON_BACKUP_DEPOSIT_INTERVAL`, 24h, 15m floor, 0 off),
   `getEnvDuration`.
5. `internal/api/backup_handlers.go`: rewrite to drill / export-capsule / pair-remote /
   deposit / status; keep `writeJSON`, `actor()`, `recordCritical` (audit-or-503) semantics;
   audit through `audit.Logger.Record` with outcome success|failure.
6. `cmd/kysignon/main.go`: `backup-drill`, `export-capsule`, `deposit`, `restore` (shares on
   stdin); delete `export-recovery-kit`, `push-backup`, `cmd/kyrestore`; `depositLoop`.
7. `web/src/components/AdminBackup.tsx`: drill, pair, deposit-now, download capsule, status
   (last receipt). Drop kit UI and its parsers/tests. Rebuild and commit `web/dist`.
8. Tests: port scaffold's backup/api tests to kysignon's harness; nodecrypt-style check that
   nothing outside `_test.go` calls `recoverykey.Combine` except the restore command.
9. Docs: AGENTS.md chain, README, delete stale `zero_code_pairing_handoff_spec.md`.
10. Gate: gofmt, tidy, vet, `go test -race ./...`, web build + dist diff, smoke if present.
