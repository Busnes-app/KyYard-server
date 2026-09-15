# scaffold + gridlock: seal the KyRecovery token at rest, carry kysignon hardening back

Hand-off, 2026-09-04. Board folder `ky-server-base-sealed-token`. Written before any code; status open.

**Repo:** ky_server_base, then gridlock-server
**PR:** none yet
**Worktree:** none yet (use `.claude/worktrees/sealed-token`, branch `fix/sealed-recovery-token`)

## Why

The scaffold and gridlock store `kyrecovery_token` as a plaintext settings row (`internal/backup/deposit.go` `StorePairing`). kysignon seals it under `DeriveKey(EncryptionKey, "<app>:setting:kyrecovery_token")` (`kysignon-server` `internal/backup/deposit.go`). A single database disclosure hands over the standing credential to the service holding every historical backup. Both scaffold-shaped products should match kysignon.

## What to carry back from kysignon #16

1. Sealed token in `StorePairing`/`LoadPairing`; `HasPairing` that never decrypts; `Status` reports pairing without echoing the credential.
2. `ErrKeyPinMissing`: a pairing record with an unresolvable key pin is audited and answered 412, never silently skipped by the scheduler.
3. Redirects refused outright (`CheckRedirect` returns an error); the test client installs the same policy; test that a 308 is not followed.
4. Reserved ranges refused in `isPublicIP` (CGNAT 100.64.0.0/10, 192.0.0.0/24, 198.18.0.0/15, 240.0.0.0/4, 64:ff9b::/96); query/fragment refused on the recovery URL.
5. The decrypt guard: absolute root, file-count floor, function-scoped exemptions (`restore`, `RunRestoreDrill`). The scaffold has no such test yet; add it.
6. `Outcome` bounding every field including the success branch digest (the scaffold already does this after #17).

## Then

Port to gridlock by per-file `git merge-file` with the scaffold's before/after as base/theirs (see the gridlock #14 hand-off on the board for the recipe). Both PRs get a security review round; expect one.

## Careful

- The scaffold's `internal/api/settings_handlers.go` exposes `extra_settings` (SCIM bearer, recovery token) to admins. Once the token is sealed, that endpoint must show "paired" not the ciphertext, and never the plaintext.
- Nothing is in the wild, so no migration of an existing plaintext row is needed; delete dev data.
