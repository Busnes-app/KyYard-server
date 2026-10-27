# Auth

## Purpose
Manages user authentication, password policies, Multi-Factor Authentication (RFC 6238 TOTP), recovery codes, session token lifecycle, and anti-abuse CAPTCHA (Proof-of-Work default).

## Ownership
Owns session issuance and verification, TOTP token generation/validation, recovery code redemption, and client-side PoW verification.

## Local Contracts
- Shared TOTP verification requires sixteen decoded key bytes; generation remains twenty random bytes. Keep original sealed weak records readable and preserve recovery access while refusing short-key authentication.
- Every authenticated session rechecks installer ownership against the immutable setup record; an altered or unassigned `kyidentity` binding fails closed. `TrustedPeer` shares `ClientIP`'s transport-peer allowlist for the public installer scheme self-check.
- Linked local SSO accounts retain local MFA: verified provider authentication can create the same five-minute password-hash-bound transaction; only successful local TOTP/recovery completion issues a session.
- `IssueSSOSession` stores the provider ID and ID token `sid` on the session so back-channel logout can end it, and fails with `store.ErrSSOSessionEnded` when that login was already ended. `IssueSession` is the local form (both empty).
- Sessions for `must_change_password` users authenticate only GET `/api/auth/me` and POST `/api/auth/change-password` or `/api/auth/logout`; private settings and product actions fail closed. Session issuance is bound to the verified password hash and serialized with replacement.
- Passwords require at least 12 characters (`ValidatePassword`).
- Active sessions are stored with SHA-256 hashed tokens and verified against secure HttpOnly / SameSite cookies or Bearer headers.
- Session authentication rejects inactive accounts and deletes their presented session.
- PoW solutions must carry unexpired server-signed challenge metadata. `PoWVerifier` accepts each signature once, remembering it until expiry (10 000 entries; at the cap expired entries go, then a random one).
- Password verification creates a five-minute opaque MFA transaction; MFA endpoints never accept a user ID and consume the transaction once.
- Single-use recovery codes are invalidated immediately upon redemption.
- `GenerateTOTPSecret`, `GenerateTOTPURL` and `GenerateRecoveryCodes` serve self-service enrolment in `internal/api`; the secret is sealed with the instance encryption key exactly as login decrypts it, and recovery codes are stored only as the digests `RedeemRecoveryCode` matches.
- `ClientIP` is the one client-address helper: the rate limiter and session binding both use it. `X-Forwarded-For` is honoured only when the peer is in `KY_TRUSTED_PROXIES`, walking the chain from the right past trusted hops; `X-Real-IP` is ignored. Each entry may be a bare IP or `ip:port` (Azure App Service/Application Gateway append the port); anything else ends the walk and falls back to the peer. `RateKey` groups a `ClientIP` result for limiter keys only: IPv6 by /64, IPv4 unchanged.

## Verification
- `go test -v ./internal/auth/...`

## Child DOX Index
None.
