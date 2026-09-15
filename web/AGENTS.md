# Web

## Purpose
React 19 + TypeScript + Vite PWA frontend embedding KySecurity color tokens (`Patina Ky`, `Cyber`, `Nord`, `Paper`, `OLED`), 90-second ephemeral QR device pairing modals, client-side WebCrypto PoW CAPTCHA, and administrative management panels.

## Ownership
Owns user interface components, service worker caching, PWA installation manifests, and frontend theme switching.

## Local Contracts
- Strict TypeScript type safety without unused imports.
- Dynamic theme selection applies `data-theme` attribute to the root HTML document and persists to `localStorage`.
- Authenticated state-changing requests use `secureFetch` so the `ky_csrf` cookie is mirrored into `X-CSRF-Token`.
- `Backup.tsx` warns for as long as `database_driver` from `/api/backup/status` is not `sqlite`: only the SQLite path can snapshot a database into a capsule, so a Postgres deployment makes no capsules at all.

## Verification
- `make test-web` or `cd web && npm ci && npm test`, then `npm run build` (vitest with jsdom; `src/pages/Backup.test.tsx` renders the recovery screen against a stubbed status route). Commit `web/dist` after a build; CI diffs it.

## Child DOX Index
None.
