package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store/migrations"
)

// Migration 30 refuses a database whose usernames differ only by case: it names every one exactly
// once, grouped, and alters nothing. Once they are resolved it creates the index, which closes the
// race the admin pre-check leaves (Review Focus 3).
func TestMigrationRefusesCaseVariantUsernames(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	// Back to a database migrated to 29.
	for _, q := range []string{`DROP INDEX idx_users_username_lower`, `DELETE FROM schema_migrations WHERE version=30`} {
		if _, err := st.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Add(-time.Hour)
	add := func(i int, id, name string) {
		t.Helper()
		if err := st.Users().CreateUser(ctx, &User{ID: id, Username: name, Role: "user", Status: "active", SSOProvider: "local", CreatedAt: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(want string, users int) {
		t.Helper()
		err := migrations.Run(ctx, st.db, st.Driver())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q, got %v", want, err)
		}
		var recorded, n int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=30`).Scan(&recorded); err != nil || recorded != 0 {
			t.Fatalf("a refused migration was recorded: %d %v", recorded, err)
		}
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil || n != users {
			t.Fatalf("users after the refusal: %d %v", n, err)
		}
	}
	add(1, "u1", "erin")
	add(2, "u2", "Erin")
	refused("usernames differ only by case: erin, Erin; rename or delete one of each pair before upgrading", 3)
	add(3, "u3", "ERIN")
	add(4, "u4", "bob")
	add(5, "u5", "Bob")
	refused("usernames differ only by case: bob, Bob; erin, Erin, ERIN; rename or delete one of each pair before upgrading", 6)
	for _, id := range []string{"u2", "u3", "u5"} {
		if err := st.Users().DeleteUser(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrations.Run(ctx, st.db, st.Driver()); err != nil {
		t.Fatalf("a clean database: %v", err)
	}
	if err := st.Users().CreateUser(ctx, &User{ID: "u6", Username: "ERIN", Role: "user", Status: "active", SSOProvider: "local"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("a case variant beside the index: %v", err)
	}
	if u, err := st.Users().GetUserByUsername(ctx, "ERIN"); err != nil || u.ID != "u1" {
		t.Fatalf("lookup: %+v %v", u, err)
	}
}
