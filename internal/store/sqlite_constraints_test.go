package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/config"
)

func TestSQLiteCustomPragmaPreservesTenantConstraints(t *testing.T) {
	for _, query := range []string{"_pragma=journal_mode(WAL)", "_pragma=foreign_keys(OFF)", "_pragma=foreign_keys=0", "_foreign_keys=off"} {
		t.Run(query, func(t *testing.T) {
			ctx := context.Background()
			st, err := Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "test.db") + "?" + query})
			if err != nil {
				// Explicitly contradictory options may fail closed instead of being overridden.
				if query != "_pragma=journal_mode(WAL)" && strings.Contains(err.Error(), "foreign keys must be enabled") {
					return
				}
				t.Fatal(err)
			}
			defer st.Close()
			sqlStore := st.(*SQLStore)
			var enabled int
			if err := sqlStore.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
				t.Fatal(err)
			}
			if enabled != 1 {
				t.Errorf("foreign_keys=%d, want 1", enabled)
			}
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(st.Users().CreateUser(ctx, &User{ID: "user", Username: "user", Role: "user", Status: "active", SSOProvider: "local"}))
			ts := st.Tenancy()
			must(ts.CreateOrganizationGroup(ctx, &OrganizationGroup{ID: "group", OrganizationID: InitialOrganizationID, Name: "Operators"}))
			if err := ts.AddOrganizationGroupMember(ctx, InitialOrganizationID, "group", "user"); err == nil {
				t.Fatal("group member without tenant membership accepted")
			}
			must(ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: InitialOrganizationID, UserID: "user", Role: RoleReadOnly, Status: "active"}))
			must(ts.AddOrganizationGroupMember(ctx, InitialOrganizationID, "group", "user"))
			must(ts.DeleteMembership(ctx, InitialOrganizationID, "user"))
			ids, err := ts.ListOrganizationGroupMembers(ctx, InitialOrganizationID, "group")
			must(err)
			if len(ids) != 0 {
				t.Fatal("membership deletion failed to cascade")
			}
			// The DSN must also protect a replacement physical connection.
			sqlStore.db.SetMaxIdleConns(0)
			if err := sqlStore.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
				t.Fatal(err)
			}
			if enabled != 1 {
				t.Fatal("replacement connection lost foreign-key enforcement")
			}
		})
	}
}
