# API

## Purpose
Exposes HTTP REST routes, authentication endpoints, Single Sign-On callbacks, SCIM endpoints, backup restore drill handlers, and static React PWA hosting.

## Ownership
Owns HTTP routing, request parsing, session cookie validation, CORS headers, and error response formatting.

## Local Contracts
- SSO routes (including callbacks, sync and SAML metadata) return 404 while disabled; SCIM retains its disabled middleware.
- Public GET/HEAD `/health/live` reports process availability; `/health/ready` pings the store with a 2s deadline and returns 503 after `BeginShutdown`. Probes are no-store and never return backend errors.
- Unsafe requests with an Origin must match the configured advertised origin, including login/MFA; originless CLI/webhook requests still undergo their existing authentication/CSRF checks.
- POST `/api/auth/change-password` accepts a restricted local session, current password and a different policy-valid new password. Browser CSRF and per-IP/account limits apply. Success revokes all sessions and requires sign-in again; flagged sessions get `password_change_required` on protected routes and public-only settings.
- All JSON API endpoints return structured errors `{"error": "message"}` upon failure.
- Non-API routes fall back to serving `web.Handler()` for client-side SPA routing.
- New routes are unauthenticated only by deliberate choice; platform routes use `s.requireAdmin` (named `platform.admin` policy); tenant routes use `s.tenantRoute` and store-owned authorization.
- Backup routes and theme writes are admin-only: capsules and settings carry site data and secrets. The scaffold has no step-up; admin-only plus `TestPrivilegedEndpointsRequireAdmin` is its equivalent for every destructive backup route. Routes are registered with method patterns, and because the SPA catch-all answers any method, tests pin that a wrong method never reaches a backup handler rather than expecting 405.

| Method | Path | Handler | Response |
|---|---|---|---|
| POST | `/api/backup/drill` | `handleBackupDrill` | `recoveryclient.DrillResult`; 409 when another HTTP/CLI drill holds the data-directory lock |
| POST | `/api/backup/export-capsule` | `handleExportCapsule` | `.kycap` attachment; POST so the CSRF check covers it |
| POST | `/api/backup/pair-remote` | `handlePairRemoteRecovery` | `{recovery_key_id, threshold, total_shares}` |
| POST | `/api/backup/deposit` | `handleRunBackup` | `recoveryclient.Result` (+`receipt_unrecorded`) |
| DELETE | `/api/backup/pairing` | `handleUnpair` | `{paired:false}`; URL and token rows only, key pin stays |
| POST | `/api/backup/pin-key` | `handlePinKey` | write-once; 409 on a different key |
| PUT | `/api/backup/schedule` | `handleSetSchedule` | `{interval_sec}` read back from the store |
| GET | `/api/backup/status` | `handleBackupStatus` | pairing, key, local copies, schedule, members, `database_driver`; never the token |

- `POST /api/backup/deposit` is one `recoveryclient.Run`: seal once, deliver to the local directory and to KyRecovery when paired. 412 no key, key pin missing, no destination, no database snapshot, or a private destination with `KY_BACKUP_ALLOW_PRIVATE_RECOVERY` off; 409 key mismatch or a run in flight; 413 over the capsule caps; 502 when KyRecovery refused (`recoveryclient.ErrRemote`, naming a local copy that was written, so the `ErrPrivateDestination` arm must stay above it: the lib wraps both on the dial path); 500 for a failure before a byte left; 200 with `receipt_unrecorded` when the store holds the capsule but the receipt was not written. It runs on a context detached from the request with a 16-minute write deadline; the acting admin is resolved before the upload and the audit row is written on that same detached context.
- The write-once, irreversible backup handlers (`handlePairRemoteRecovery`, `handlePinKey`, `handleRunBackup`) run on `context.WithoutCancel(r.Context())` so a dropped connection cannot leave a pin, a pairing or a deposit half-written with no audit row; the idempotent ones (`handleSetSchedule`, `handleUnpair`) stay on the request context. Their routes are registered as `s.tracked(s.requireAdmin(s.handleX))`, so the `detached` counter is incremented the moment `ServeHTTP` dispatches -- before `requireAdmin`'s session lookup, which is itself a store round-trip that `ReadTimeout` (15s) lets outlast `shutdownTimeout` (5s). Registering inside the handler was too late: `Shutdown` returns after its timeout with requests still active, and one still in the auth lookup would leave `WaitDetached()` reading zero and the store closing under a request about to pin a key. The header-read window before `ServeHTTP` is entered cannot be covered by any counter, because no handler goroutine exists yet; `Shutdown`'s own drain is what covers it. The counter is a mutex and a `sync.Cond`, not a `sync.WaitGroup`, which panics when an `Add` from zero races an in-progress `Wait` -- two admin requests at SIGTERM do exactly that. `WaitDetached()` is what `cmd/server` blocks on before closing the store, because `http.Server.Shutdown` returns without knowing these goroutines exist.
- Audit actions: `backup.paired`, `backup.pair_failed`, `admin.backup_run` (details start with `outcome="success|failure"`), `admin.backup_unpair`, `admin.backup_key_pin`, `admin.backup_schedule`, `admin.backup_export` (a downloaded capsule, resource the capsule ID). `AuditDetails` flattens the lib's details map into the bounded audit field with locally derived fields first and remote error text last; values are quoted and `=` is escaped before the final `AuditSafe` cut. `cmd/server` uses it for the scheduler and CLI rows. Details carry key or capsule IDs, digests and paths, never the token.
- Rate-limit keys for `login:`, `mfa:` and `pair:` come from `auth.ClientIP`, never from `RemoteAddr` or a raw header, so a limit is neither shared by everyone behind a proxy nor bypassable by forging `X-Forwarded-For`.
- CORS permits only the exact configured `KY_APP_URL` origin and credentialed browser writes require matching CSRF cookie/header tokens.
- API request bodies are capped at 1 MiB and all responses receive baseline CSP, anti-framing, MIME-sniffing, and referrer-policy headers.
- `GET /api/settings` tiers its payload: public fields for the login screen, `db_driver`/`scim_enabled` for any session, and `extra_settings` for admins only; KyRecovery tokens are omitted in both sealed and legacy plaintext forms, dropped by the `kyrecovery_token` key prefix rather than by literal key name.

- SSO callbacks, the directory webhook and SCIM only touch global accounts; none of them, and no global group, can select or grant a tenant. Tenant membership is granted only through the authorized `/members` routes.
- Tenant namespace is `/api/organizations/{organization}`: GET organization; GET/POST `/environments`; GET/PATCH/DELETE `/environments/{environment}`; GET `/audit` or `/environments/{environment}/audit`; GET `/members`; PUT/DELETE `/members/{user}`. Unknown tenant paths/methods return JSON 404, never SPA content.
- Endpoints: `POST /environments/{environment}/enrollment-tokens` (`{runtime}`; 201 with the one-time token and the Docker-socket disclosure; the generated `docker run -i` command that pipes the token on stdin is included only when `KY_AGENT_IMAGE` names a digest-pinned image, and names exactly that image, otherwise a `note` says so); `GET /endpoints`, `GET /environments/{environment}/endpoints`, `GET|PATCH /endpoints/{endpoint}`, `POST /endpoints/{endpoint}/approve` (`{fingerprint}`, 64 hex, must be the enrolled key), `/reject`, `/revoke`. Deleting an environment with a non-revoked endpoint returns `409 environment_in_use`.
- `POST /api/agent/v1/enroll` is the only agent-facing route: no session, no CSRF (no cookie), strict JSON `{token, public_key, proof, name, facts}` in base64url, per-IP limit 10/minute, `201 {endpoint_id, state, fingerprint, instance_fingerprint}`, `409 name_taken`, and every other refusal a uniform `401 Enrollment refused`. Unknown `/api/agent/` paths return JSON 404.
- `GET /api/organizations` lists the session user's own active memberships (`id`, `name`, `role`) for the organization selector. It is identity-scoped, not tenant-scoped: it never lists another user's organizations and platform administrators without membership get an empty list.
- Membership writes take `{role, status?}` (status defaults to `active`); the user ID comes from the path and must already exist (404 otherwise). `409 {code: last_administrator}` means the change would leave the organization without an active administrator, including an administrator demoting, disabling or removing themself.
- `tenantRoute` authenticates unrestricted sessions, derives actor and correlation ID server-side, and sends `X-Request-ID` plus `Cache-Control: no-store`. Handlers pass explicit URL scope to authorized store methods; clients cannot supply actor, organization or arbitrary update fields in JSON. Existing Origin/CSRF/body limits apply.
- Every tenant JSON body goes through `strictJSON` (unknown fields and trailing documents rejected). Environment create/update accepts only `{name}`; lists accept nonnegative `offset` and `limit` 1–200 (default 50). Store errors map to generic 400/403/404/409/500 responses. Request parsing/authentication/CSRF failures occur before the transactional tenant audit path.

## Verification
- `go test -v ./internal/api/...` (`authz_test.go` pins the per-role exposure of every privileged route; `backup_test.go` the backup routes, on SQLite only because a run snapshots the database)
- `scripts/smoke-test.sh` asserts the same boundaries against a running binary

## Child DOX Index
None.
