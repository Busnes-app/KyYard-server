# kynotes: adopt ky-primitives and KyRecovery deposit

Hand-off, 2026-09-04. Board folder `kynotes-kyrecovery-deposit`. Written before any code; status open.

**Repo:** kynotes-server
**PR:** none yet
**Worktree:** none yet (use `.claude/worktrees/deposit`, branch `feat/kyrecovery-deposit`)

## Where it stands (surveyed 2026-09-04)

- `go 1.26.6`, no ky-primitives at all. Not a scaffold fork: own layout (`internal/httpapi`, `blobstore`, `ids`), three commits of history.
- Passwords: scrypt N=2^17 (`internal/auth/hash.go`). Move to Argon2id via `ky-primitives/password` (decision 3). Nothing in the wild.
- Audit: flat table with inline SQL (`internal/httpapi/admin_routes.go:26,269`, `auth_routes.go:319`, `device_routes.go:252`). Per decision 6 it stays flat; do not add auditchain here.
- Single-use recovery code, no TOTP, no keyfile: recovery via `ky-primitives/recoverycode`; keys via `keyfile`.
- No backup of any kind. `AGENTS.md:128` claims a pairing client that does not exist; fix it. Stale v1 spec at repo root.
- Blob storage: notes may hold attachments in `blobstore`. Decide whether blobs go in the capsule (per-file cap 64 MiB, expanded cap 256 MiB, container cap 384 MiB) or only the database plus a manifest of blob hashes. If blobs can exceed the caps, the capsule carries the database and the operator is told what is not covered, in the status endpoint and the manifest.

## What to seal

SQLite database (snapshot through the live handle), encryption/secret keys, `recovery.pub`, config manifest, blobs if they fit.

## Reference implementation

- Product half: `ky_server_base` master `internal/backup/{client,deposit,capsule,drill,recoverykey}.go`, `internal/api/backup_handlers.go`, `cmd/server/main.go` (`deposit`, `export-capsule`, `restore`, `depositLoop`), `internal/config` (`KY_BACKUP_DEPOSIT_INTERVAL`, 15m floor). gridlock is the same code with import paths swapped.
- Adaptation pattern for a repo that is not a scaffold fork: `kysignon-server` PR #16 (plan `2026-09-04-kysignon-deposit-port.md`). It shows how to bind a concrete store with ctx-less `GetSetting`/`SetSetting` (add `store.ErrNotFound`), a `Snapshotter` for the live database, a flat config, an audit logger with outcomes, and step-up gating.
- Wire contract: `kyrecovery-server/zero_code_pairing_handoff_spec.md` v2.0.0. Product-side steps and invariants: `/home/yoshi/busness.app/AGENTS.md`, "KyRecovery integration".

## Steps common to every port

1. `go get github.com/Busness-app/ky-primitives@v0.4.1`; `go 1.26.6`.
2. Bring in the backup package. Keep from kysignon rather than the scaffold: the KyRecovery token sealed under a key derived from the deployment encryption key (never a plaintext settings row); `ErrKeyPinMissing` so a paired instance whose `recovery.pub` vanished is audited, not skipped; redirects refused outright; reserved ranges (CGNAT, 192.0.0.0/24, 198.18.0.0/15, 240.0.0.0/4, 64:ff9b::/96) refused; query/fragment refused on the recovery URL.
3. Collector: snapshot SQLite through the live handle (`VACUUM INTO`), include every key a restore needs, refuse to seal without them. Non-SQLite drivers refuse with `ErrNoDatabaseSnapshot`.
4. Routes (admin, CSRF, step-up where the product has it): `POST drill`, `GET export-capsule`, `POST pair-remote`, `POST deposit`, `GET status`. Deposit on `context.WithoutCancel` with a 16-minute write deadline; the acting admin resolved before the upload; every deposit result audited through `Outcome`; an export that cannot be audited refused.
5. CLI: `backup-drill`, `export-capsule`, `deposit`, `restore` (shares on stdin, never argv). Scheduler every deposit interval (24h default, 15m floor, 0 disables) on a context that survives SIGTERM; silent skip only when never paired.
6. Frontend: pair, deposit now, download sealed capsule, drill, status with the pinned key and last receipt. Rebuild and commit `dist` if the repo commits it.
7. Tests: WAL snapshot carries an uncheckpointed row; deposit opens with a private key held only by the test; token never in the clear; re-pair to a different key refused; unpaired export/deposit 412; the decrypt guard walks from an absolute root, asserts a file-count floor, and exempts only `restore` and the drill by function.
8. Docs: AGENTS.md chain, README; delete the stale v1 `zero_code_pairing_handoff_spec.md` copy in the repo root.
9. Gate: gofmt, tidy, vet, `go test -race ./...`, frontend build (+ dist diff), smoke test if present. Open a PR; the pr-reviewer bot posts findings (public repos: report under `~/.local/state/pr-reviewer/`). Expect two to three rounds.

## Careful

- This is a from-scratch adoption, not a port: budget for the config/store/audit adapters before the backup code.
- Argon2id is not stdlib, so `password` in ky-primitives carries only PHC encoding and policy; the hash itself comes from `golang.org/x/crypto/argon2`, already a dependency.
