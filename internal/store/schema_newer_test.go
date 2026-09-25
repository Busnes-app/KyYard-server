package store

import (
	"context"
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
