# SCIM

## Purpose
Adapts the `elimity-com/scim` RFC 7643/7644 server to the local user and group stores for enterprise provisioning from Okta, Azure AD / Microsoft Entra ID, OneLogin, and KySignOn.

## Ownership
Owns local persistence adapters and bearer authentication; the library owns `/scim/v2/Users`, `/scim/v2/Groups`, discovery endpoints, schema validation, filter/PATCH parsing, pagination, and SCIM error serialization.

## Local Contracts
- This is retained legacy code for storage/backward-compatibility tests. KyYard exposes no SCIM or phone-pairing HTTP routes.
- Content-Type for all SCIM endpoints must be `application/scim+json`.
- Requests must be authenticated with the configured bearer token.
- User de-provisioning via `PATCH` with `active: false` updates user status to `inactive` and revokes sessions; it does not remove organization memberships. Tenant access is denied live by user status and returns when the IdP reactivates the account. `DELETE` removes the account and cascades its memberships. Deactivating an organization's sole active administrator bypasses the last-administrator guard (see `internal/sso/AGENTS.md`).
- SCIM `roles` map to the global `User.Role` only (an IdP may grant platform `admin`); no SCIM user or group attribute grants organization membership. SCIM groups are global identity data that no authorization decision reads. `TestExternalIdentityNeverGrantsTenantAccess` in `internal/store` pins this through the real handler.
- A Replace or Patch that renames a user onto a username another account holds, ignoring case, is SCIM `uniqueness` (409): `UpdateUser` maps the unique-index violation to `ErrAlreadyExists` and `scimStoreError` translates it (`TestSCIMRenameOntoATakenUsernameIsUniqueness`).
- SCIM protocol models and parsing must come from `github.com/elimity-com/scim`; do not add parallel local request/response implementations.

## Verification
- `go test -v ./internal/scim/...`

## Child DOX Index
None.
