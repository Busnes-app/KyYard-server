# Acceptance readiness (platform administration, preflight, terminal)

The section 7 acceptance run (docs/ACCEPTANCE.md) needs two organizations and a non-admin
account, and two screens say things the product no longer means. This slice makes the run
possible through the UI and removes the two lies. Volumes in the application definition, the
remaining gap, are a separate spec.

Decisions recorded 2026-09-24 (Yoshi): platform admins create organizations (with a first
organization admin) and local accounts; the server generates a temporary password and shows
it once; the always-on preflight blocker goes; the Terminal button renders only for roles
with `container.exec`.

## Platform administration API

All routes sit behind the existing `requireAdmin` (platform role `admin`) and the same CSRF
rules as the other admin routes.

- `GET /api/admin/organizations` → `[{id, name, created_at, members}]` ordered by name;
  `members` is the count of active memberships.
- `POST /api/admin/organizations` body `{name, admin_user_id}` → 201 `{id, name, created_at}`.
  `name` 1..64 characters, `protocol.CleanText`-stable, unique (409 `organization_exists`).
  `admin_user_id` must be an existing active user (404 `user_not_found`; 409 `user_inactive`).
  One transaction creates the organization (`id = "org_" + 12 random hex`) and the membership
  (`organization_admin`, `active`), so no organization is ever born without an admin.
  Audit: `organization.create`, platform scope, actor the platform admin, resource the new
  organization ID, details `admin=<user id>`.
- `GET /api/admin/users` → `[{id, username, display_name, role, status, sso_provider, created_at}]`
  ordered by username; never password hashes, TOTP or recovery fields.
- `POST /api/admin/users` body `{username, display_name, role}` → 201
  `{id, username, display_name, role, temporary_password}`. `username` 3..64 characters,
  `[a-z0-9._-]`, unique (409 `username_exists`); `display_name` 1..128 `CleanText`-stable;
  `role` `user` or `admin`. The server generates a 24-character temporary password from
  `crypto.RandomHex`-style alphanumerics, hashes it with `password.Hash` (the hasher `init-admin` uses), sets
  `MustChangePassword: true`, `Status: active`, `SSOProvider: local`. The plaintext appears
  only in this response body and never in a log, audit row or later read. Audit:
  `user.create`, platform scope, resource the new user ID, details `role=<role>`.
- `init-admin` keeps working; it stays the way to create the first admin.

Store additions: `CreateOrganizationWithAdmin(ctx, org *Organization, adminUserID string) error`
(one transaction, membership row included, `ErrNotFound` for a missing user, `ErrInvalid` for
an inactive one, `ErrAlreadyExists` on a duplicate name) and `ListOrganizations(ctx) ([]OrganizationSummary, error)`.
User creation reuses `Users().CreateUser`; the API layer validates and hashes.

## UI: Settings → Administration

Visible only when `/api/auth/me` reports `role == "admin"`. Two panels in the existing
Settings page section style:

- **Organizations**: table (name, members, created) and a create form (name; first admin
  chosen from the users list, active users only). Success text: "Organization created. Its
  admin adds members on the organization page." Fixed texts for 409 `organization_exists`
  and 404/409 on the user.
- **Users**: table (username, display name, role, status, provider) and a create form
  (username, display name, role). On 201 the temporary password is shown once in a
  read-only field with a Copy button and the text "Give this password to the person out of
  band. They must change it at first sign-in. It is not shown again." Fixed text for 409
  `username_exists`. The form clears after submit.

Non-admins never see the section; the routes refuse them with 403 regardless.

## Preflight

- `buildDeploymentPreflight` no longer seeds `runtime_verification_required`;
  `PlanDeployment` no longer filters it; the blocker name leaves the store, the API type list
  and the web `Blocker` union. `executable` is true exactly when no blocker remains.
- `ApplicationPreflight.tsx`: heading "Deployment preflight"; when `executable` is true the
  panel says "Ready to plan: every service maps to an adopted container and its image is
  known on the host."; when blockers remain it lists them as today. The stale sentence about
  execution not being available is removed. Tests updated.
- `docs/application-schema.md` and `internal/store/AGENTS.md` drop the blocker.

## Terminal

- `ContainerControls` gains a `canExec bool` prop; the Terminal button (and the dialog) render
  only when it is true. The pages that render the controls (the endpoint page and any
  container list) compute it from the caller's organization role, which the app already
  loads through `GET /api/organizations` (`MemberOrganization.role`): true only for
  `organization_admin`, matching `permissions.Allows(role, ContainerExec)`. Platform admins
  without a membership see no Terminal either (the server would refuse them).
- The exec route itself is unchanged; the server still refuses every other role.

## Documents

`docs/authorization-matrix.md` (platform admin: create organizations and local accounts;
first admin seeded), `README.md` (a short "Organizations and accounts" subsection: init-admin
once, then Settings → Administration), `docs/ACCEPTANCE.md` (prerequisites use the UI, the
SQL and its "unproven" marks go; step 4 uses a created `user` account added as `read_only`),
`internal/api/AGENTS.md`, `internal/store/AGENTS.md`, `web/AGENTS.md`.

## Tests

Store (SQLite + PostgreSQL): organization with admin in one transaction, rollback when the
user is missing or inactive, duplicate name, list with member counts. API: every admin route
403 for a non-admin session and 401 unauthenticated; create organization 201 + membership +
audit row; create user 201 with a 24-character password that verifies against the stored
hash, `must_change_password` true, audit row without the password, the password absent from
every later read and from logs (the test captures the server log); validation 400s; CSRF on
POSTs. Web: Administration hidden for non-admins and shown for admins; both forms post the
right bodies; the temporary password renders once and the form clears; preflight panel
states; Terminal hidden for `read_only`, `developer`, `operator`, `environment_admin`, shown
for `organization_admin`.

## Out of scope

Disabling or deleting accounts and organizations, password reset by admins, SSO/SCIM account
management, volumes in the application definition (next spec), PR D hardening.
