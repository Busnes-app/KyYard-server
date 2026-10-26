# Browser regressions

## Purpose

Execute the committed KyYard browser bundle against a disposable real server.

## Ownership

Private test tooling, server lifecycle fixtures and desktop/phone Chromium tests.

## Local Contracts

- Build the current server and use only its fresh loopback SQLite/data directory.
- Disable automatic Docker connection and CAPTCHA only in the fixture configuration.
- Exercise real authentication, CSRF and tenant APIs; never attach to an installed service.
- Keep new data and test credentials disposable. Preserve normal caches and remove owned fixtures.
- No automatic retries, real host/container mutations or claims of human acceptance/soak coverage.

## Work Guidance

Use semantic locators and assertions about persisted behavior. Keep production code and
committed assets unchanged unless a reproduced defect requires a separate documented fix.

## Verification

`npm ci && npx playwright install chromium && npm test` builds and tests the current server.

## Child DOX Index

None.
