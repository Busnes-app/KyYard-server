package api_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// Replaying every migration per test under -race cost this package most of its run time and
// pushed it past go test's 10-minute timeout. store.Open on a fresh file only migrates, so
// each test copies one database migrated once per process instead.
var sqliteTemplate struct {
	once sync.Once
	dir  string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sqliteTemplate.dir != "" {
		_ = os.RemoveAll(sqliteTemplate.dir)
	}
	os.Exit(code)
}

// copyMigratedSQLite writes a fully migrated database to dst, which must not exist yet.
func copyMigratedSQLite(t *testing.T, dst string) {
	t.Helper()
	sqliteTemplate.once.Do(func() {
		dir, err := os.MkdirTemp("", "kyyard-api-sqlite-template-")
		if err != nil {
			sqliteTemplate.err = err
			return
		}
		sqliteTemplate.dir = dir
		st, err := store.Open(context.Background(), config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(dir, "template.db")})
		if err == nil {
			err = st.Close() // checkpoints the WAL into the main file
		}
		sqliteTemplate.err = err
	})
	if sqliteTemplate.err != nil {
		t.Fatalf("migrated sqlite template: %v", sqliteTemplate.err)
	}
	src := filepath.Join(sqliteTemplate.dir, "template.db")
	if fi, err := os.Stat(src + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("migrated sqlite template: WAL not checkpointed on close")
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read sqlite template: %v", err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatalf("copy sqlite template: %v", err)
	}
}
