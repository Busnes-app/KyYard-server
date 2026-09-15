# kybookmarks: adopt KyRecovery pairing and deposit, move to Argon2id

Hand-off, 2026-09-04. Board folder `kybookmarks-kyrecovery-deposit`. Written before any code; status open.

**Repo:** kybookmarks-server
**PR:** none yet
**Worktree:** none yet (use `.claude/worktrees/deposit`, branch `feat/kyrecovery-deposit`)

## Where it stands (surveyed 2026-09-04, HEAD b975c82)

- ky-primitives `v0.3.0`; imports only `auditchain`. Bump to `v0.4.1`.
- No pairing, capsule, deposit or restore. The only "export" is a plaintext Netscape bookmarks HTML built in the browser (`frontend/src/lib/netscape.ts`).
- Passwords: scrypt, local (`internal/crypto/crypto.go`, call sites `internal/api/auth_handlers.go:258,369,607`). Suite decision 3 moves scrypt products to Argon2id via `ky-primitives/password`. Nothing is in the wild, so no rehash-on-login shim: delete dev data and start clean.
- Paper recovery key: local 24-char code verified with scrypt (`internal/crypto/crypto.go:57`, `internal/api/auth_handlers.go:113`). Replace with `ky-primitives/recoverycode`.
- `audit.key` loaded by hand (`internal/audit/audit.go:23`); use `ky-primitives/keyfile`.
- No CLI subcommand dispatcher at all (`cmd/server/main.go`); add one.
- Frontend: `frontend/dist` is gitignored but 26 files are still tracked and the tracked `index.html` references a JS asset that was never committed, so a bare checkout serves a broken page. `git rm -r --cached frontend/dist`; CI builds the frontend from source and Docker copies a fresh build, so nothing depends on the tracked copy. Dependabot npm entry already exists (PR #14).
- `AGENTS.md:161` claims a pairing client that does not exist; fix it. Stale v1 spec at repo root.

## What to seal

SQLite database, `audit.key` + audit log, the server encryption key if any, `recovery.pub`, config manifest.

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

- Two migrations in one PR is fine here (backup + Argon2id) because nothing is in the wild, but keep them as separate commits so the reviewer can read each.
- `TestNothingInTheServerDecrypts`: copy the kysignon version, not the first kysignon draft; the draft walked nothing.
