package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Busness-app/ky_server_base/internal/config"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Open initializes and returns the configured database store backend.
func Open(ctx context.Context, cfg config.DatabaseConfig) (Store, error) {
	driver := strings.ToLower(cfg.Driver)
	switch driver {
	case "sqlite", "sqlite3", "":
		return openSQLite(ctx, cfg.DSN)
	case "postgres", "postgresql", "pgx":
		return openPostgres(ctx, cfg.DSN, cfg.MaxOpenConns, cfg.MaxIdleConns, cfg.ConnMaxLifetime)
	default:
		return nil, fmt.Errorf("unsupported database driver: %q (supported: sqlite, postgres)", cfg.Driver)
	}
}

func openSQLite(ctx context.Context, dsn string) (Store, error) {
	filePath := dsn
	if idx := strings.Index(dsn, "?"); idx != -1 {
		filePath = dsn[:idx]
	}

	dir := filepath.Dir(filePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create sqlite directory %s: %w", dir, err)
		}
	}

	if !strings.Contains(dsn, "_pragma") {
		delim := "?"
		if strings.Contains(dsn, "?") {
			delim = "&"
		}
		dsn = fmt.Sprintf("%s%s_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)", dsn, delim)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}

	return newSQLStore(ctx, db, "sqlite")
}

func openPostgres(ctx context.Context, dsn string, maxOpen, maxIdle int, maxLifetime time.Duration) (Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres database: %w", err)
	}

	if maxOpen <= 0 {
		maxOpen = 25
	}
	if maxIdle <= 0 {
		maxIdle = 5
	}
	if maxLifetime <= 0 {
		maxLifetime = 15 * time.Minute
	}

	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(maxLifetime)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping postgres database: %w", err)
	}

	return newSQLStore(ctx, db, "postgres")
}
