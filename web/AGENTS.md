# Web

## Purpose
React 19 + TypeScript + Vite PWA frontend embedding KySecurity color tokens (`Patina Ky`, `Cyber`, `Nord`, `Paper`, `OLED`), 90-second ephemeral QR device pairing modals, client-side WebCrypto PoW CAPTCHA, and administrative management panels.

## Ownership
Owns user interface components, service worker caching, PWA installation manifests, and frontend theme switching.

## Local Contracts
- A signed-in user with `must_change_password` sees only password replacement and sign-out. Replacement uses `secureFetch`, returns to login after session revocation, and never exposes the normal navigation before completion.
- Login shows local setup/SSH/TLS guidance for HTTP installations and offers SSO only when enabled.
- Product title, PWA name, and pre-settings fallback name are `KyYard`.
- Strict TypeScript type safety without unused imports.
- Dynamic theme selection applies `data-theme` attribute to the root HTML document and persists to `localStorage`.
- Authenticated state-changing requests use `secureFetch` so the `ky_csrf` cookie is mirrored into `X-CSRF-Token`.
- Navigation is path-based (`src/router.ts`, no router dependency): `/`, `/scim`, `/backup`, `/settings`, `/organizations/{org}`, `/organizations/{org}/members`, `/organizations/{org}/audit`, `/organizations/{org}/environments/{env}`. Segments are limited to 64 safe characters; anything else is the not-found screen. `Link` renders real anchors with `aria-current` and leaves modified clicks to the browser. Deep links survive sign-in because the path is untouched while `Login` is shown, and the service worker serves the app shell for offline navigations.
- The organization selector lists only the caller's own memberships from `GET /api/organizations` and re-reads when the current organization changes. `<main>` is keyed on the organization ID, so every tenant screen's state is discarded on context change; there is no client-side tenant cache.
- Tenant screens read through `useTenantResource` and show exactly one of loading / denied (401, 403) / not found (404) / offline (network failure) / error, with a retry where retrying can help. Writes go through `tenantWrite` on `secureFetch`; `409 last_administrator`, 403, 404, 409 and network failure map to fixed user-facing messages; any other failure shows only the status code, never server text. Malformed percent-escapes and dot segments in the path land on the not-found screen.
- `Backup.tsx` warns for as long as `database_driver` from `/api/backup/status` is not `sqlite`: only the SQLite path can snapshot a database into a capsule, so a Postgres deployment makes no capsules at all.

## Verification
- `make test-web` or `cd web && npm ci && npm test`, then `npm run build` (vitest with jsdom; `src/pages/Backup.test.tsx` renders the recovery screen against a stubbed status route; `src/pages/Organization.test.tsx` covers denied/offline/retry states, CSRF on tenant writes, the last-administrator message and deep links with browser history). Commit `web/dist` after a build; CI diffs it.

## Child DOX Index
None.
