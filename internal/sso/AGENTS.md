# SSO

## Purpose
Provides unified Single Sign-On federation for KySignOn, Generic OpenID Connect (Google, Microsoft Entra ID, Okta, Keycloak), and SAML 2.0 Service Provider (SP).

## Ownership
Owns the application adapters around OAuth/OIDC login, KySignOn HMAC-SHA256 signed directory sync webhooks, and SAML metadata publication.

## Local Contracts
- `KySignOnClient.HandleSyncWebhook` verifies HMAC-SHA256 signatures before modifying local user state.
- PKCE with `S256` is enforced on all OAuth/OIDC authorization requests.
- ID tokens require provider signature, issuer, audience, expiry, and one-time nonce verification before claims are trusted.
- OAuth discovery, authorization URLs, PKCE parameters, code exchange, and token verification are delegated to `golang.org/x/oauth2` and `coreos/go-oidc`; application code only maps verified claims.
- SAML assertion parsing is not implemented locally; metadata XML uses `encoding/xml` and no ACS route is exposed until a maintained SAML service-provider library is configured.
- Directory webhook timestamps are accepted only within five minutes; status or role changes revoke the user's sessions.
- External identity never selects or grants a tenant. SSO sign-in and the directory webhook create or update the global account (`role`, `status`) only; they never write organization memberships or organization groups, and an externally supplied `admin` role is platform authority with no tenant access. Deactivation denies tenant access live through user status and leaves the membership grant untouched, so reactivation restores what the organization administrator granted; deletion removes the account and its grants, and a re-provisioned account is a new identity that must be granted again. External status changes bypass the last-administrator guard, which only governs membership writes: deactivating an organization's sole active administrator leaves it with no in-product membership write path until the IdP reactivates the account or a platform repair route exists (tracked in `docs/authorization-matrix.md`; pinned by `TestExternalDeactivationCanLockOutAnOrganization`). Provider- or group-to-membership mapping does not exist; if it is ever added it must be an explicit, audited organization-level setting. `TestExternalIdentityNeverGrantsTenantAccess` in `internal/store` pins this through the real webhook client.

## Verification
- `go test -v ./internal/sso/...`

## Child DOX Index
None.
