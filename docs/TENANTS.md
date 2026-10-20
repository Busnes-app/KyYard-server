# Tenants

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Initial organization

Startup creates the default organization and assigns one active local administrator once:
`admin` if present, otherwise the oldest active local admin. Existing ordinary, disabled and
federated accounts are not automatically enrolled. If no eligible local admin exists, the
migration completes without assigning a member; adding a local admin later does not silently
claim it. Platform administration remains separate from organization membership.

Membership removal or disabling survives restart and administrator password resets. SSO and directory sync provision accounts only: no external role, group or attribute grants
organization access, an IdP deactivation denies access live and leaves the grant in place for
reactivation, and a deleted account's grants are gone for good. Tenant APIs enforce live membership on every
operation; platform administrators need explicit membership too. Membership administration is available through the tenant API and the web UI: pick an
organization in the header, then manage environments, members and audit history under
`/organizations/{organization}`. Links are deep-linkable and survive sign-in. Provider settings are available under Settings → Single sign-on.

### Organizations and accounts

The first platform administrator is the bootstrap `admin` from first start, or one made with
`kyyard-server init-admin [-username name]` (which also resets an existing local account's
password, clears its MFA and makes it a platform administrator). It reads the password from stdin, never argv: `docker compose exec -T kyyard
/app/kyyard-server init-admin < password-file`, or type it at the hidden prompt. Everything
after that is in the UI: **Settings → Administration**, shown to platform administrators only.

- **Create user** makes a local account with platform role User or Platform administrator. The
  server generates a 24-character temporary password and shows it once, with Copy; it is not
  shown again, logged or audited. Hand it over out of band. The account must replace it at
  first sign-in.
- **Create organization** takes a name and a first administrator chosen from active users. The
  organization and that administrator's membership are created together; the creator is not
  made a member unless named.
- The organization's administrator adds everyone else on the organization's **Members** page,
  by user ID (`usr_…`), with an organization role. Settings → Administration shows every user's
  and organization's ID. A platform role grants no organization access.

Both creations are recorded in platform audit (`organization.create`, `user.create`), which has
no screen yet. Accounts and organizations cannot be disabled or deleted from the UI yet.

### Tenant API

Use `/api/organizations/{organization}` (`org_initial` for the default organization):

| Method | Suffix | Operation |
|---|---|---|
| GET | empty | Organization details |
| GET / POST | `/environments` | List / create environment |
| GET / PATCH / DELETE | `/environments/{environment}` | Read / rename / delete |
| GET | `/audit` | Organization audit history |
| GET | `/environments/{environment}/audit` | Environment audit history |
| GET | `/members` | List memberships (user, username, role, status) |
| POST | `/environments/{environment}/enrollment-tokens` | Mint a one-time agent enrollment token and command |
| GET | `/endpoints`, `/environments/{environment}/endpoints` | List endpoints |
| GET / PATCH | `/endpoints/{endpoint}` | Read / rename an endpoint |
| POST | `/endpoints/{endpoint}/approve`, `/reject`, `/revoke` | Review or terminate an endpoint |
| PUT / DELETE | `/members/{user}` | Set role/status for an existing user / remove membership |

`GET /api/organizations` (no suffix) lists the signed-in user's own active organizations and
roles; it never reveals anyone else's. Membership writes take `{"role":"operator","status":"active"}`
(status optional). An organization must keep one active administrator, so demoting, disabling or
removing the last one returns `409` with code `last_administrator`, even for that administrator.

Create/rename takes `{"name":"Production"}`. An enrollment token lives 15 minutes, is single use,
and is shown once alongside a Docker command and the host-access disclosure. A host enrolls as
`pending` until an administrator approves its exact key fingerprint.
