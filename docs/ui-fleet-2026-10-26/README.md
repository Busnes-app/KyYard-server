# KyYard fleet Appearance — 2026-10-26

Applies the approved Busnes look to KyYard's web shell and gives it the shared ky-ui 0.4.0 Appearance theme-preview cards (native radios) over the suite catalog: Match system plus every catalog theme, read dynamically from `web/src/ky-ui/themes.js`. Shared files come from ky-ui `4f83f34` and pass `sync-consumers --check`; they are generated, never hand-edited. No score is claimed here: the council rates these captures.

## Stages

- **00 before**: base commit, current theme control and main view.
- **01 after**: suite shell scale (240px sidebar, 24px title, 40px controls and nav rows, shared field border and type) and the Appearance cards.
- **02 after**: owner correction — shared description class and copy, consistent wrapping, preview hairline, no theme selector in the shell navigation. Only affected Appearance/shell views were recaptured.
- **02 confirm**: one confirmation recapture after the single stage 02 fix batch, where a fix was needed.

Surfaces: Home (main view) and Settings → Appearance. The automatic local Docker endpoint was disabled (`KY_DOCKER_SOCKET=`), so no host container data appears; an earlier baseline that showed this host's container counts was deleted before commit.

## Catalog and saved choice

- Storage key `kyyard-theme`; theme names saved by earlier versions (the 17 suite names) still select their theme; new choices are saved by catalog name, and Match system as `system`.
- Match system follows the device until a choice is saved; that default is unchanged.

## Product notes

- Polished Ky keeps KyYard's shipped button text `#021716` (`LOCAL_PALETTE` in `web/src/theme.ts`). The suite catalog carries `#042f2e`, which is 3.9:1 on the Polished Ky accent and fails the AA test KyYard already enforced. Upstream should decide whether the catalog adopts `#021716`.
- Appearance stays a Settings section (KyYard's documented Home/Endpoints/Settings navigation); the page h1 is Settings and the section heading is Appearance.

## Capture batches

Every manifest in `screenshots/` lists the source commit, a hash of any uncommitted diff, OS, driver, viewport, theme source, fixture and the SHA-256 of every PNG. `results.csv` pairs each after image with its stage 00 before; `checks.csv` holds the in-app checks. `source.patch` is the source-only diff (no vendored or built files) for independent review.

| Batch | Source commit | Uncommitted diff | Images | Surfaces | Captured |
| --- | --- | --- | --- | --- | --- |
| `kyyard-00-before` | `996c59a1c4` | none | 8 | appearance, home | 2026-10-26T11:02Z |
| `kyyard-01-after` | `038fb4ad67` | `b424025ed9a3…` | 8 | appearance, home | 2026-10-26T11:21Z |
| `kyyard-02-after` | `59dd637e7f` | none | 4 | appearance | 2026-10-26T11:37Z |
| `kyyard-02-confirm` | `b5b1e62085` | none | 4 | appearance | 2026-10-26T11:39Z |

Conditions: linux 7.2.10-1-cachyos, playwright chromium 156.0.8078.4 (headless), device scale 1, 1280×900 and 390×844, light/dark by OS emulation with no saved choice, America/Chicago, en-US. Fixture: web built from this tree; fresh SQLite data dir; captcha none; KY_DOCKER_SOCKET empty (no local Docker endpoint), no enrolled endpoints or organizations beyond first-run defaults; init-admin "admin" (generated password on stdin, replaced through /api/auth/change-password when required).

## In-app theme check

`node docs/ui-fleet-2026-10-26/capture.mjs --verify <prefix>`: Sky and Sun (light named themes), Busnes Dark, Busnes Light and Match system, asserting the page background, the hovered idle navigation row and the selected row at 4.5:1 or better from computed colours in the running product, then arrow-key selection with a visible focus ring.

| Run | Source commit | Passed |
| --- | --- | --- |
| `kyyard-01-after` | `038fb4ad67` | 16/16 |
| `kyyard-02-after` | `59dd637e7f` | 16/16 |

## Checks

| Check | Result |
| --- | --- |
| Web unit tests (vitest) | 563/563 pass at low load; under heavy shared CPU load, timing tests timed out (5–7 s) in two earlier runs and passed in isolation |
| Web build and committed `web/dist` equals a fresh build | pass |
| `npm audit` | 0 vulnerabilities |
| Vendored ky-ui check | pass at 4f83f34 |
| Fuzz, govulncheck, shellcheck, smoke, Docker and agent-install tests | not run locally: CI runs them |

Go: `go build ./...`, `go vet ./...`, gofmt and the full `go test -race -count=1 ./...` passed locally on this machine (CachyOS, Go 1.27.1); Go code is unchanged by this work. Docker image, publish and attestation jobs were not run locally: CI runs them on this PR.

## Reproduce

```sh
cd web && npm ci && npm run build   # then any product-specific bundle step in AGENTS.md
cd .. && KY_PLAYWRIGHT=<path to @playwright/test/index.mjs, if web/ has none> \
  node docs/ui-fleet-2026-10-26/capture.mjs <prefix> [surface,...]
node docs/ui-fleet-2026-10-26/capture.mjs --verify <prefix>
node docs/ui-fleet-2026-10-26/results.mjs
```

Each run starts a disposable loopback server on its own port with generated secrets and removes its data directory afterwards. No live service, personal data or authentication bypass is used.
