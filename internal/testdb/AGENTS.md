# TestDB

## Purpose
Supplies tests with an isolated, empty database configuration so the same suite runs on either supported backend.

## Ownership
Owns test-only database provisioning and teardown. Never imported by production code.

## Local Contracts
- `Config(t *testing.T) config.DatabaseConfig` returns a SQLite temp file by default.
- With `KY_TEST_POSTGRES_DSN` set, it returns a Postgres config scoped to a per-caller schema, dropped on cleanup.
- `DatabaseConfig(t)` owns a separate PostgreSQL database for database-wide disk-pressure measurements; test credentials require CREATEDB, and cleanup drops only that generated database. It uses the existing SQLite temp-file path on SQLite. Schema-only fixtures still use `Config`.
- Test files must obtain database config here, never by hardcoding a driver, so CI can switch backends.

## Verification
- `go test ./...` (SQLite)
- `KY_TEST_POSTGRES_DSN=... go test -count=1 ./...` (PostgreSQL)

## Child DOX Index
None.
