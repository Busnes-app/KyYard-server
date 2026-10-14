package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store/migrations"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

// A database migrated by a newer build must not be served by this one.
func TestOpenRefusesANewerSchema(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	st, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*SQLStore)
	future := migrations.Latest() + 1
	if _, err := s.db.Exec(s.rebind(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, 'future', CURRENT_TIMESTAMP)`), future); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	reopened, err := Open(ctx, cfg)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("a newer schema was opened")
	}
	want := fmt.Sprintf("database schema version %d is newer than this binary (%d)", future, migrations.Latest())
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// Two servers starting on one fresh database both migrate it: neither may fail on a version
// the other is applying or has applied.
func TestConcurrentMigrationsApplyEachVersionOnce(t *testing.T) {
	ctx := context.Background()
	for round := range 3 {
		cfg := testdb.Config(t)
		driver, dsn := "pgx", cfg.DSN
		if cfg.Driver == "sqlite" {
			driver, dsn = "sqlite", cfg.DSN+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
		}
		errs := make(chan error, 2)
		for range 2 {
			db, err := sql.Open(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			// Connect first: switching a new file to WAL is not what this test races.
			if err := db.PingContext(ctx); err != nil {
				t.Fatal(err)
			}
			go func() { errs <- migrations.Run(ctx, db, cfg.Driver) }()
		}
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
}
