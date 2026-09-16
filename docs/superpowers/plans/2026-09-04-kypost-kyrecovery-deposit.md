# kypost: fix the module path, adopt ky-primitives and KyRecovery deposit

Hand-off, 2026-09-04. Board folder `kypost-kyrecovery-deposit`. Written before any code; status open.

**Repo:** kypost-server
**PR:** none yet
**Worktree:** none yet (use `.claude/worktrees/deposit`, branch `feat/kyrecovery-deposit`)

## Where it stands (surveyed 2026-09-04)

- Go module lives at `backend/`, module path `kypost-server/backend` — not importable and not a GitHub path. Fix first: `module github.com/Busness-app/kypost-server/backend` (or move the module to the repo root), update every import, then everything else.
- `go 1.26.6`, no ky-primitives. Layout `backend/ + frontend/ + worker/ + worker-apns/`. Not a scaffold fork.
- Passwords: scrypt (`backend/internal/users/users.go:2019,2035,2100`; cost notes in `internal/users/kdf.go`). Move to Argon2id via `ky-primitives/password`. Nothing in the wild.
- TOTP, push MFA with number match, recovery codes: all local (`internal/totp`, `internal/mfa`, `internal/api/auth_stepup.go`). Adopt `ky-primitives/totp` and `recoverycode`; keep the push MFA, the library has no equivalent.
- Audit: flat "classification audit log" (`internal/state/schema.go:77`, retention at `:101`). Stays flat (decision 6).
- Export today: contacts vCard and PGP client keys only. No backup.
- Mail data: kypost stores mail. Size will exceed the capsule caps on any real deployment. The capsule carries the database, keys and config; mail bodies need the streaming container (`ky-primitives docs/superpowers/specs/2026-09-02-capsule-streaming-design.md`), which does not exist yet. Say so plainly in status and the manifest rather than shipping a capsule that looks complete.
- Stale v1 spec at repo root.

## What to seal

SQLite state database, PGP server keys if any, deployment encryption/secret keys, `recovery.pub`, config manifest. Not mail bodies until streaming capsules exist.

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

- Do the module path fix as its own PR; it touches every file and the reviewer should not read it mixed with crypto changes.
- The APNs and FCM workers are separate npm packages with their own CI job; the backup work does not touch them.
