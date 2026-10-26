# Contributing to KyYard

KyYard is the simple control plane for your container fleet. Contributions must preserve authenticated, authorized control over hosts and the self-hosting contract — control-plane actions are powerful and must never run without an authorized identity.

## Before contributing

Read [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) and [SECURITY.md](SECURITY.md),
the [README](README.md), and any AGENTS.md in the subtree you are about to
touch. Open an issue before proposing a feature larger than a bug fix or
documentation change.

Keep changes small, reuse existing code, and fix the shared root cause rather
than the reported symptom. New non-trivial logic ships with a runnable test or
self-check.

## Security requirements

- Every fleet action — start, stop, deploy, inspect — is authorized against the authenticated identity before it runs.
- Host connection credentials are stored encrypted at rest, handled with least privilege, and never logged.
- Destructive actions require an explicit user action.
- People and groups come from KyIdentity; a deactivated identity must lose control access.
- Do not add telemetry, phone-home, analytics, or unconfigured third-party
  services.
- Do not add silent security fallbacks; fail closed with a readable error and a
  remediation step.
- Name and document any security trade-off in [SECURITY.md](SECURITY.md) before
  it merges.

## Verification

Run the checks that apply to the files you changed, and the repository's
full required checks before hand-off. Record the exact commands and their
results in the pull request.

CI's smoke job runs the real-binary installer/OIDC regression after `make build`:
`KY_INSTALLER_TEST_BINARY="$PWD/kyyard-server" python3 scripts/installer-oidc-test.py`.
It requires Python 3 and OpenSSL, uses disposable local data and a TLS identity-provider
fixture, and never contacts existing suite services. Keep this check in the job that
gates main publication and release images.

### Browser regressions

Run `make test-browser` from the repository root (Go, Node 26 and Chromium system
libraries required; on a fresh Linux machine, install those with
`cd tests/browser && npm ci && npx playwright install --with-deps chromium`).
The target builds the current server and tests the committed `web/dist`; run
`make build-web` and commit its generated files when changing frontend source.
To select a case: `cd tests/browser && npm test -- --project=desktop --grep 'CSRF'`.

Each test owns a fresh loopback-only SQLite server, data directory and browser
context. Docker auto-connection and CAPTCHA are disabled only in this fixture.
Cases exercise bootstrap password replacement, replayed-session refusal, deep
links, real tenant writes/refusals, appearance persistence, keyboard navigation
and a phone viewport. These checks do not establish whole-product branch coverage,
real Docker/Kubernetes acceptance, HTTPS cookie behavior or the human/soak gates.
CI requires the browser job before main publication and release images; failures
retain traces/screenshots as the `browser-failures` artifact. Private test packages
have their own lockfile and Dependabot entry, outside the shipped frontend.

## AI attribution

AI-assisted contributions are welcome; undisclosed ones are not. Disclose
material AI assistance in the pull request — the tool, what it changed, and what
you personally ran, read, and verified — and add a commit trailer naming any
tool that materially contributed. You are the author of the code you submit for
every purpose, regardless of how it was produced.

## Pull requests

Branch off `main` with a focused name, and use a Conventional Commit subject
(`fix:`, `feat:`, `docs:`, `refactor:`, `test:`, or `chore:`). The pull request
explains why the change exists, the verification performed, the security impact,
and any known limitations.

Every pull request gets an adversarial review pass; see
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md#hostile-reviews-are-about-code-never-people)
for what that is and the boundary it keeps.

## Licence

KyYard is licensed under the MIT License. See [LICENSE.txt](LICENSE.txt).


Repeat the full race suites with `GODEBUG=fips140=on` on SQLite and PostgreSQL; preserve existing fuzz, real-runtime and image gates. Run the synthetic Compose preservation check in `docs/check-strong-crypto-compose.py`. `docs/STRONG-CRYPTO.md` owns key, agent and recovery migration requirements.

PostgreSQL disk-pressure tests create and remove their own disposable databases via `testdb.DatabaseConfig`; the dedicated test role needs CREATEDB. Other fixtures retain schema-only isolation. Never point these tests at a production database.
