package store

import (
	"context"
	"github.com/Busnes-app/kyyard-server/internal/store/migrations"
	"testing"
)

func TestSSOIdentityMigrationPreservesExistingAccounts(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	user := &User{ID: "external", Username: "external", Role: "user", Status: "active", SSOProvider: "idp_old", SSOSubject: "stable-subject"}
	if err := st.Users().CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE user_sso_identities`, `DELETE FROM schema_migrations WHERE version=39`} {
		if _, err := st.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrations.Run(ctx, st.db, st.Driver()); err != nil {
		t.Fatal(err)
	}
	after, err := st.Users().GetUserBySSO(ctx, "idp_old", "stable-subject")
	if err != nil || after.ID != user.ID || after.Username != user.Username {
		t.Fatal("identity migration lost the original account", err)
	}
	if err := migrations.Run(ctx, st.db, st.Driver()); err != nil {
		t.Fatal("repeat migration", err)
	}
}
