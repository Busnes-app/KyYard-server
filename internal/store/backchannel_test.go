package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store/migrations"
)

// A back-channel logout removes the pending MFA transactions of the logins it ends, not
// just the sessions, so no second factor can complete them.
func TestSSOLogoutEndsPendingMFAChallenges(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	if err := st.Users().CreateUser(ctx, &User{ID: "ext", Username: "ext", Role: "user", Status: "active", SSOProvider: "idp_a", SSOSubject: "sub-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO user_sso_identities (provider, subject, user_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`), "idp_a", "sub-1", "ext"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, c := range []MFAChallenge{
		{TokenHash: "by-sid", SSOProvider: "idp_a", SSOSID: "sid-1", SSOIssuedAt: now.Add(-time.Minute)},
		{TokenHash: "by-subject", SSOProvider: "idp_a", SSOSID: "sid-2", SSOIssuedAt: now.Add(-time.Minute)},
		{TokenHash: "later", SSOProvider: "idp_a", SSOSID: "sid-3", SSOIssuedAt: now.Add(time.Minute)},
		{TokenHash: "other-provider", SSOProvider: "idp_b", SSOSID: "sid-1", SSOIssuedAt: now.Add(-time.Minute)},
	} {
		c.UserID, c.ExpiresAt = "ext", now.Add(5*time.Minute)
		if err := st.Sessions().CreateMFAChallenge(ctx, &c, ""); err != nil {
			t.Fatal(err)
		}
	}
	pending := func(token string) bool {
		var n int
		if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(1) FROM mfa_challenges WHERE token_hash = ?`), token).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if err := st.Sessions().EndSSOSession(ctx, "idp_a", "sid-1", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if pending("by-sid") || !pending("by-subject") || !pending("other-provider") {
		t.Fatal("sid logout ended the wrong MFA transactions")
	}
	if err := st.Sessions().EndSSOSubject(ctx, "idp_a", "sub-1", now, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if pending("by-subject") || !pending("later") || !pending("other-provider") {
		t.Fatal("subject-wide logout ended the wrong MFA transactions")
	}
	if _, err := st.Sessions().ConsumeMFAChallenge(ctx, "by-subject"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ended MFA transaction still consumable: %v", err)
	}
}

// Sessions from before migration 46 carry no provider login, so no logout could reach them:
// the upgrade ends those that may have come from a provider, and only once.
func TestBackchannelMigrationEndsUnboundSSOSessions(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	for _, u := range []*User{
		{ID: "loc", Username: "loc", Role: "user", Status: "active", SSOProvider: "local"},
		{ID: "lnk", Username: "lnk", Role: "user", Status: "active", SSOProvider: "local"},
		{ID: "ext", Username: "ext", Role: "user", Status: "active", SSOProvider: "idp_a", SSOSubject: "sub-1"},
		{ID: "old", Username: "old", Role: "user", Status: "active", SSOProvider: "idp_a", SSOSubject: "sub-old"},
	} {
		if err := st.Users().CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, st.rebind(q), args...); err != nil {
			t.Fatal(q, err)
		}
	}
	exec(`INSERT INTO user_sso_identities (provider, subject, user_id) VALUES (?, ?, ?)`, "idp_a", "lnk-sub", "lnk")
	// An external account from before identities were split out has only users.sso_provider.
	exec(`DELETE FROM user_sso_identities WHERE user_id = ?`, "old")
	// Back to the version 45 schema.
	for _, q := range []string{
		`DROP TABLE sso_subject_logouts`, `DROP TABLE sso_logouts`, `DROP INDEX idx_sessions_sso_sid`,
		`ALTER TABLE mfa_challenges DROP COLUMN sso_issued_at`, `ALTER TABLE mfa_challenges DROP COLUMN sso_sid`, `ALTER TABLE mfa_challenges DROP COLUMN sso_provider`,
		`ALTER TABLE sessions DROP COLUMN sso_sid`, `ALTER TABLE sessions DROP COLUMN sso_provider`,
		`DELETE FROM schema_migrations WHERE version = 46`,
	} {
		exec(q)
	}
	now := time.Now().UTC()
	for _, id := range []string{"loc", "lnk", "ext", "old"} {
		exec(`INSERT INTO sessions (token_hash, user_id, user_agent, ip_address, created_at, expires_at) VALUES (?, ?, '', '', ?, ?)`, "s-"+id, id, now, now.Add(time.Hour))
		exec(`INSERT INTO mfa_challenges (token_hash, user_id, expires_at, password_hash) VALUES (?, ?, ?, '')`, "c-"+id, id, now.Add(time.Minute))
	}
	count := func(table, key string) int {
		var n int
		if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(1) FROM `+table+` WHERE token_hash = ?`), key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := migrations.Run(ctx, st.db, st.Driver()); err != nil {
		t.Fatal(err)
	}
	if count("sessions", "s-loc") != 1 {
		t.Fatal("upgrade ended a local account's session")
	}
	if count("sessions", "s-ext") != 0 || count("sessions", "s-lnk") != 0 || count("sessions", "s-old") != 0 {
		t.Fatal("upgrade kept a session no back-channel logout can reach")
	}
	for _, id := range []string{"loc", "lnk", "ext", "old"} {
		if count("mfa_challenges", "c-"+id) != 0 {
			t.Fatalf("upgrade kept %s's unbound MFA transaction", id)
		}
	}
	// A later start migrates nothing, so a bound session from after the upgrade survives it.
	if err := st.Sessions().CreateSession(ctx, &Session{TokenHash: "bound", UserID: "ext", CreatedAt: now, ExpiresAt: now.Add(time.Hour), SSOProvider: "idp_a", SSOSID: "sid-1"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, st.db, st.Driver()); err != nil {
		t.Fatal(err)
	}
	if count("sessions", "bound") != 1 || count("sessions", "s-loc") != 1 {
		t.Fatal("a later start ended sessions again")
	}
}
