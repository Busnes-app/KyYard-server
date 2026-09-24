# Acceptance Readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a platform admin create organizations (each with a first admin) and local accounts from the UI, stop the preflight screen claiming deployment is disabled, and show the Terminal button only to roles that may exec, so the section 7 acceptance run can be done through the product.

**Architecture:** Store gains an atomic organization-plus-admin create and an organization list (Task 1). The API gains four `requireAdmin` routes with audit rows (Task 2). The web Settings page gains an Administration section for platform admins (Task 3). The always-on preflight blocker is removed end to end and the Terminal button is gated by role (Task 4). Docs and the acceptance runbook close (Task 5).

**Tech Stack:** Go, SQLite + PostgreSQL, React 19 + TypeScript + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-24-acceptance-readiness-design.md`

## Global Constraints

- Admin routes: `GET/POST /api/admin/organizations`, `GET/POST /api/admin/users`, all behind `requireAdmin` (platform role `admin`), CSRF on POST like the other admin POSTs.
- Organization create: `name` 1..64, `protocol.CleanText`-stable, unique → 409 `organization_exists`; `admin_user_id` existing active user → 404 `user_not_found` / 409 `user_inactive`; organization id `"org_" + 12 random hex`; membership `organization_admin`/`active` in the same transaction; audit `organization.create`, platform scope, resource the new org id, details `admin=<user id>`.
- User create: `username` 3..64 `[a-z0-9._-]`, unique → 409 `username_exists`; `display_name` 1..128 `CleanText`-stable; `role` `user`|`admin`; 24-character generated temporary password, hashed with `password.Hash`, `MustChangePassword: true`, `Status: active`, `SSOProvider: local`; the plaintext appears only in the 201 body, never in logs, audit rows or later reads; audit `user.create`, platform scope, resource the new user id, details `role=<role>`.
- Preflight: `runtime_verification_required` leaves the store, the plan filter, the API type list and the web union; `executable` is true exactly when no blocker remains; heading "Deployment preflight"; ready text "Ready to plan: every service maps to an adopted container and its image is known on the host."
- Terminal: `ContainerControls` prop `canExec bool`; rendered only when true; pages compute it as `role === 'organization_admin'` from `GET /api/organizations`.
- Every store behaviour tested on SQLite and PostgreSQL. Commit trailer: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. A username that differs only in case from an existing one (`Alice` vs `alice`): the grammar is lowercase-only, so `Alice` is a 400, never a second account (Task 2 test).
2. Creating an organization whose `admin_user_id` is a platform admin who is already an org admin elsewhere must work; membership is per organization (Task 1 test).
3. The temporary password must not survive a `GET /api/admin/users` or `/api/auth/me` for that user, and the server log captured during the test must not contain it (Task 2 test).
4. A non-admin with a valid session hitting `POST /api/admin/users` must get 403 and leave no user behind (Task 2 test asserts the user count).
5. With the blocker gone, a fully mapped application with every image known must report `executable: true` and a plan must be mintable without the old filter (Task 4 store test on both drivers).

---

### Task 1: Store — organization with first admin, organization list

**Files:**
- Modify: `internal/store/tenancy.go` (after `CreateOrganization`), `internal/store/store.go` (TenancyStore interface), `internal/store/models.go` (`OrganizationSummary`)
- Test: `internal/store/tenancy_admin_test.go`
- Modify: `internal/store/AGENTS.md`

**Interfaces (produces):**
```go
type OrganizationSummary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Members   int       `json:"members"` // active memberships
}
// TenancyStore:
CreateOrganizationWithAdmin(ctx context.Context, o *Organization, adminUserID string) error // ErrNotFound (no user), ErrInvalid (user not active), ErrAlreadyExists (name taken)
ListOrganizations(ctx context.Context) ([]OrganizationSummary, error) // ordered by name, then id
```

- [ ] **Step 1: Failing tests** (both drivers via the testdb pattern; create users with `Users().CreateUser`):
  - create with an active user → organization row, one membership `organization_admin`/`active`, `ListMemberOrganizations(user)` contains it with role `organization_admin`.
  - missing user → `ErrNotFound`, no organization row; inactive (`Status: "suspended"`) → `ErrInvalid`, no row.
  - duplicate name (case-sensitive exact) → `ErrAlreadyExists`.
  - a user who is already an admin of another organization can be the first admin of a second one.
  - `ListOrganizations` returns both organizations with member counts (add a second `active` and one `disabled` membership; count only active).
- [ ] **Step 2: Run** `go test -count=1 -run 'TestCreateOrganizationWithAdmin|TestListOrganizations' ./internal/store/` → FAIL.
- [ ] **Step 3: Implement.** One transaction: `SELECT status FROM users WHERE id=?` (no rows → `ErrNotFound`; not `active` → `ErrInvalid`); `SELECT COUNT(*) FROM organizations WHERE name=?` → `ErrAlreadyExists`; `INSERT INTO organizations`; `INSERT INTO organization_memberships (organization_id,user_id,role,status) VALUES (?,?,'organization_admin','active')`; commit. `ListOrganizations`: `SELECT o.id,o.name,o.created_at,(SELECT COUNT(*) FROM organization_memberships m WHERE m.organization_id=o.id AND m.status='active') FROM organizations o ORDER BY o.name,o.id`.
- [ ] **Step 4: Run** both drivers → PASS; gofmt; `internal/store/AGENTS.md` bullet; commit `feat(store): create an organization with its first admin; list organizations`.

---

### Task 2: Platform administration API

**Files:**
- Create: `internal/api/admin_handlers.go`
- Modify: `internal/api/server.go` (four routes beside the backup admin routes; CSRF middleware already covers non-GET), `internal/api/AGENTS.md`
- Test: `internal/api/admin_handlers_test.go`

**Interfaces:**
- Consumes: Task 1 store methods; `Users().CreateUser`, `Users().GetUserByUsername`, `Users().ListUsers(ctx, offset, limit, search)`; `password.Hash` and `password.Verify` from `github.com/Busnes-app/ky-primitives/password` (what `init-admin` uses); `crypto.RandomHex`; the audit store (`s.store.Audit().LogAudit(ctx, &store.AuditRecord{Scope: "platform", UserID, Action, Resource, Details, Result, IPAddress})`, the shape `internal/api/auth_handlers.go:119` uses).
- Produces: the JSON shapes in the spec.

- [ ] **Step 1: Failing tests** using `loginAs(t, s, st, name, role)` from the existing api tests (`"admin"` role for the platform admin): 401 unauthenticated on all four; 403 for a `user` session on all four and the user table unchanged; POST without CSRF → 403; organization create 201 with membership visible via `GET /api/organizations` as that user, audit row `organization.create` with details `admin=<id>`; 409 `organization_exists`; 404 `user_not_found`; 409 `user_inactive`; 400 on a 65-character name and on control characters; user create 201 with a 24-character `temporary_password` that `password.Verify` (or the existing verify helper) accepts against the stored hash, `must_change_password` true, `sso_provider` `local`; the password absent from `GET /api/admin/users`, from `/api/auth/me` as that user, and from the server log (capture `log` output with `log.SetOutput` around the request); 409 `username_exists`; 400 for `Alice`, `ab`, a 65-character username, role `manager`; audit row `user.create` details `role=user` without the password.
- [ ] **Step 2: Run** `go test -count=1 -run 'TestAdmin' ./internal/api/` → FAIL.
- [ ] **Step 3: Implement.** `usernameRE = ^[a-z0-9._-]{3,64}$`; temporary password: 24 characters from `[A-Za-z0-9]` via `crypto/rand` (write `randomPassword(n int) string` in admin_handlers.go using `rand.Int`); error bodies `{"error": <fixed text>, "code": <code>}` via `s.writeJSON`; audit rows through the audit store with `scope: "platform"`, `user_id` the admin's id, `result: "success"`, and a failure row with `result: "failure"` when the store refuses (duplicate, missing user).
- [ ] **Step 4: Run** the api package on SQLite and PostgreSQL → PASS; gofmt; `internal/api/AGENTS.md`; commit `feat(api): platform administration of organizations and local accounts`.

---

### Task 3: Settings → Administration

**Files:**
- Create: `web/src/components/Administration.tsx`, `web/src/components/Administration.test.tsx`
- Modify: `web/src/pages/Settings.tsx` (a fifth tab `administration` shown only when the current user's role is `admin`; read how `App.tsx` stores `/api/auth/me` and pass the role down or fetch it in the page the way `SSOSettings` decides its visibility), `web/src/tenant.ts` (types `AdminOrganization`, `AdminUser`, `CreatedUser`), `web/AGENTS.md`

- [ ] **Step 1: Failing tests:** section absent for a `user`, present for an `admin`; organizations table renders rows with member counts; create form posts `{name, admin_user_id}` with the CSRF header and shows "Organization created. Its admin adds members on the organization page."; 409 shows "An organization with that name already exists."; users table renders; create form posts `{username, display_name, role}`; the 201 shows the temporary password in a read-only input with a Copy button and the sentence "Give this password to the person out of band. They must change it at first sign-in. It is not shown again."; the form clears; 409 shows "That username is already taken."; 403 shows "Administrator role required."
- [ ] **Step 2: Run** `cd web && npx vitest run src/components/Administration.test.tsx` → FAIL.
- [ ] **Step 3: Implement** following the Registries panel (tenantWrite with `texts`, fixed strings, `useTenantResource` for both lists; the first-admin picker is a `<select>` of active users).
- [ ] **Step 4: Run** `cd web && npx vitest run && npm run build`, then `make build-web`; commit `feat(web): platform administration section`.

---

### Task 4: Preflight blocker removal and Terminal gate

**Files:**
- Modify: `internal/store/application_preflight.go:134` (no seeded blocker), `internal/store/application_deployment.go:281` (drop the filter), store tests that expect the blocker (grep `runtime_verification_required` in internal/store and internal/api tests), `docs/application-schema.md`, `internal/store/AGENTS.md`
- Modify: `web/src/components/ApplicationPreflight.tsx` (+test): union, heading, ready text
- Modify: `web/src/components/ContainerControls.tsx` (+test): `canExec` prop; `web/src/pages/EndpointPage.tsx` and `web/src/pages/Dashboard.tsx`: compute `canExec` from the organization role (`MemberOrganization.role === 'organization_admin'`; the Dashboard already loads `/api/organizations`; the endpoint page loads what it needs the same way)

- [ ] **Step 1: Failing tests:** store: a fully mapped application with known images reports `Executable: true` and zero blockers on both drivers; API preflight route shows `executable: true`; web preflight: heading "Deployment preflight", ready text when `executable`; ContainerControls: no Terminal button when `canExec` false, present when true; pages pass `canExec` per role (table test over the five roles).
- [ ] **Step 2: Run** the named Go tests and vitest files → FAIL.
- [ ] **Step 3: Implement**; grep the repo for the blocker name and remove every remaining mention (Go, TS, docs).
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/store/ ./internal/api/` (SQLite + PostgreSQL) and `cd web && npx vitest run && npm run build`, `make build-web`; commit `fix: preflight reports real readiness; terminal only for roles with exec`.

---

### Task 5: Docs and the runbook

**Files:** `docs/authorization-matrix.md` (platform admin: create organizations with a first admin, create local accounts), `README.md` ("Organizations and accounts": `init-admin` once, then Settings → Administration), `docs/ACCEPTANCE.md` (prerequisites through the UI; remove the SQL and its "unproven" marks; step 4 uses a created `user` account added as `read_only`; step 3 notes the Terminal button appears only for organization admins), `KyYard-Implementation-Plan.md` §8 (acceptance readiness delivered; volumes next), `internal/api/AGENTS.md`, `internal/store/AGENTS.md`, `web/AGENTS.md` verified.

- [ ] **Step 1: Update the documents.**
- [ ] **Step 2: `make ci`** and the store + api suites on PostgreSQL. Expected: exit 0.
- [ ] **Step 3: Commit** `docs: organizations and accounts; acceptance runbook through the UI`.
