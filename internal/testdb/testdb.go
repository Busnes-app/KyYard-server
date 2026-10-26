// Package testdb hands tests an isolated database configuration.
//
// Default backend is a throwaway SQLite file. Set KY_TEST_POSTGRES_DSN to run
// the same tests against a real Postgres instance; each caller gets its own
// schema, dropped on cleanup.
package testdb

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/google/uuid"
)

// Config returns a database config pointing at an isolated, empty database.
func Config(t *testing.T) config.DatabaseConfig {
	t.Helper()

	dsn := os.Getenv("KY_TEST_POSTGRES_DSN")
	if dsn == "" {
		return config.DatabaseConfig{
			Driver: "sqlite",
			DSN:    filepath.Join(t.TempDir(), "test.db"),
		}
	}
	return postgresConfig(t, dsn)
}

func postgresConfig(t *testing.T, dsn string) config.DatabaseConfig {
	t.Helper()

	// The "pgx" driver is registered by the store package, which every caller imports.
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("testdb: open postgres: %v", err)
	}

	schema := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("testdb: create schema %s: %v", schema, err)
	}

	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	return config.DatabaseConfig{
		Driver: "postgres",
		DSN:    dsn + sep + "search_path=" + schema,
	}
}

// DatabaseConfig owns the whole database when a test measures database-wide
// disk usage. A schema cannot isolate pg_database_size from other fixtures.
// PostgreSQL test credentials need CREATEDB; production never calls this helper.
func DatabaseConfig(t *testing.T) config.DatabaseConfig {
	t.Helper()
	dsn := os.Getenv("KY_TEST_POSTGRES_DSN")
	if dsn == "" {
		return Config(t)
	}
	endpoint, err := url.Parse(dsn)
	if err != nil || (endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql") {
		t.Fatal("testdb: dedicated database requires a PostgreSQL URI")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("testdb: open PostgreSQL: %v", err)
	}
	name := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("testdb: create dedicated database (requires CREATEDB): %v", err)
	}
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		if err != nil {
			t.Errorf("testdb: drop owned database %s: %v", name, err)
		}
		admin.Close()
	})
	endpoint.Path = "/" + name
	endpoint.RawPath = ""
	query := endpoint.Query()
	query.Del("search_path")
	endpoint.RawQuery = query.Encode()
	return config.DatabaseConfig{Driver: "postgres", DSN: endpoint.String()}
}
