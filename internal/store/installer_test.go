package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/applysetup"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

func yardInstallerBundle() applysetup.Common {
	return applysetup.Common{Schema: applysetup.Schema, Service: "kyyard", SetupID: "12345678-1234-1234-1234-123456789abc", SSO: &applysetup.SSO{IssuerURL: "https://identity.example", ClientID: "yard", ClientSecretFile: "/data/installer/client.secret", DirectorySecretFile: "/data/installer/directory.secret", RedirectURI: "https://yard.example/api/sso/kyidentity/callback"}, Admins: []applysetup.Identity{{Issuer: "https://identity.example", Subject: "owner", Username: "owner-admin"}}, Everyday: []applysetup.Identity{{Issuer: "https://identity.example", Subject: "daily", Username: "everyday"}}}
}

func TestInstallerDirectoryFenceAndAtomicRevocation(t *testing.T) {
	s := installerTestStore(t)
	ctx := context.Background()
	b := yardInstallerBundle()
	if _, err := s.ApplyInstaller(ctx, b, false); err != nil {
		t.Fatal(err)
	}
	id := InstallerUserID(b.SSO.IssuerURL, "owner")
	now := time.Now().UTC()
	if err := s.Sessions().CreateSession(ctx, &Session{TokenHash: "session", UserID: id, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Sessions().CreateMFAChallenge(ctx, &MFAChallenge{TokenHash: "mfa", UserID: id, ExpiresAt: now.Add(time.Minute)}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Devices().CreatePairing(ctx, &DevicePairing{Secret: "pairing", UserID: id, Status: "approved"}); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	apply := func(issuer, event, revision, digest string, assigned, admin bool) error {
		return s.ApplyInstallerDirectory(ctx, issuer, "owner", event, revision, digest, assigned, admin)
	}
	for _, revision := range []string{"0", "01", "+1", " 1", "18446744073709551616"} {
		if err := apply(b.SSO.IssuerURL, "user.deactivated", revision, digest, false, false); err == nil {
			t.Fatal("noncanonical revision accepted", revision)
		}
	}
	if err := apply("https://foreign.example", "user.deactivated", "2", digest, false, false); err == nil {
		t.Fatal("foreign issuer accepted")
	}
	if err := apply(b.SSO.IssuerURL, "user.deactivated", "2", digest, false, false); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "mfa_challenges", "device_pairings"} {
		var n int
		if err := s.db.QueryRowContext(ctx, s.rebind("SELECT COUNT(*) FROM "+table+" WHERE user_id=?"), id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("surviving %s %d %v", table, n, err)
		}
	}
	if err := apply(b.SSO.IssuerURL, "user.deactivated", "2", digest, false, false); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"1", "2"} {
		if err := apply(b.SSO.IssuerURL, "user.updated", revision, strings.Repeat("b", 64), true, true); err == nil {
			t.Fatal("stale or conflicting event accepted", revision)
		}
	}
	if _, err := s.ApplyInstaller(ctx, b, false); err == nil {
		t.Fatal("setup restored offboarded account")
	}
	// A failed native audit cannot commit availability or the new revision fence.
	ddl := `CREATE TRIGGER fail_directory_audit BEFORE INSERT ON audit_records WHEN NEW.action='installer.directory_applied' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END;`
	if s.Driver() == "postgres" {
		ddl = `CREATE FUNCTION fail_directory_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='installer.directory_applied' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_directory_audit BEFORE INSERT ON audit_records FOR EACH ROW EXECUTE FUNCTION fail_directory_audit();`
	}
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	if err := apply(b.SSO.IssuerURL, "user.updated", "3", strings.Repeat("c", 64), true, true); err == nil {
		t.Fatal("reactivated without audit")
	}
	if u, err := s.Users().GetUserByID(ctx, id); err != nil || u.Status != "inactive" {
		t.Fatalf("partial availability %+v %v", u, err)
	}
	raw, err := s.Settings().GetSetting(ctx, "installer.directory."+id)
	var fence installerDirectoryFence
	if err != nil || applysetup.DecodeRaw([]byte(raw), &fence) != nil || fence.Revision != 2 {
		t.Fatal("partial revision", raw, err)
	}
}

func installerTestStore(t *testing.T) *SQLStore {
	t.Helper()
	s, err := Open(context.Background(), testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.(*SQLStore)
}

func TestInstallerOwnershipAndNativeWrites(t *testing.T) {
	s := installerTestStore(t)
	ctx := context.Background()
	b := yardInstallerBundle()
	if _, err := s.ApplyInstaller(ctx, b, true); err == nil {
		t.Fatal("absent check succeeded")
	}
	if n, _ := s.Users().CountUsers(ctx); n != 0 {
		t.Fatal("check created identities")
	}
	r, err := s.ApplyInstaller(ctx, b, false)
	if err != nil || len(r.Created) != 3 {
		t.Fatalf("apply %+v %v", r, err)
	}
	for _, check := range []bool{false, true} {
		r, err = s.ApplyInstaller(ctx, b, check)
		if err != nil || len(r.Present) != 3 || len(r.Created) != 0 {
			t.Fatalf("resume %+v %v", r, err)
		}
	}
	if _, n, err := s.Audit().ListAuditRecords(ctx, 0, 100); err != nil || n != 3 {
		t.Fatalf("audit %d %v", n, err)
	}
	u, err := s.Users().GetUserBySSO(ctx, InstallerProvider, "owner")
	if err != nil || u.Role != "admin" || u.PasswordHash != "" {
		t.Fatalf("owner %+v %v", u, err)
	}
	if allowed, err := s.InstallerUserAllowed(ctx, u); err != nil || !allowed {
		t.Fatalf("owner denied %v %v", allowed, err)
	}
	if memberships, err := s.Tenancy().ListMemberOrganizations(ctx, u.ID); err != nil || len(memberships) != 0 {
		t.Fatalf("platform role granted tenant access %+v %v", memberships, err)
	}
	for _, mutate := range []func(*User){func(u *User) { u.Role = "user" }, func(u *User) { u.Status = "inactive" }, func(u *User) { u.PasswordHash = "foreign" }, func(u *User) { u.Username = "renamed" }, func(u *User) { u.MustChangePassword = true }} {
		copy := *u
		mutate(&copy)
		if err := s.Users().UpdateUser(ctx, &copy); !errors.Is(err, ErrInstallerOwned) {
			t.Fatalf("authority changed %v", err)
		}
	}
	now := time.Now().UTC()
	u.LastLoginAt = &now
	u.TOTPEnabled = true
	u.TOTPSecretEnc = "sealed-mfa"
	if err := s.Users().UpdateUser(ctx, u); err != nil {
		t.Fatalf("MFA and login update refused %v", err)
	}
	if err := s.Users().DeleteUser(ctx, u.ID); !errors.Is(err, ErrInstallerOwned) {
		t.Fatalf("deleted owner %v", err)
	}
	for _, key := range []string{InstallerConfigKey, "installer.user." + u.ID, "sso_providers_enc"} {
		if err := s.Settings().SetSetting(ctx, key, "foreign"); !errors.Is(err, ErrInstallerOwned) {
			t.Fatalf("setting changed %s %v", key, err)
		}
		if err := s.Settings().DeleteSetting(ctx, key); !errors.Is(err, ErrInstallerOwned) {
			t.Fatalf("setting removed %s %v", key, err)
		}
	}
	if err := s.Users().CreateUser(ctx, &User{ID: "foreign", Username: "foreign", Role: "admin", Status: "active", SSOProvider: InstallerProvider, SSOSubject: "other"}); !errors.Is(err, ErrInstallerOwned) {
		t.Fatalf("created unassigned identity %v", err)
	}
	if err := s.Users().LinkSSO(ctx, "local", "session", "hash", InstallerProvider, "other", "test"); !errors.Is(err, ErrInstallerOwned) {
		t.Fatalf("linked reserved provider %v", err)
	}
	local := &User{ID: "local", Username: "local", PasswordHash: "hash", Role: "admin", Status: "active", SSOProvider: "local"}
	if err := s.Users().CreateUser(ctx, local); err != nil {
		t.Fatal(err)
	}
	local.DisplayName = "retained"
	if err := s.Users().UpdateUser(ctx, local); err != nil {
		t.Fatal(err)
	}
	if err := s.Users().DeleteUser(ctx, local.ID); err != nil {
		t.Fatal(err)
	}
}

func TestInstallerRefusesAdoptionAndRollsBack(t *testing.T) {
	for _, collision := range []string{"username", "provider", "audit"} {
		t.Run(collision, func(t *testing.T) {
			s := installerTestStore(t)
			ctx := context.Background()
			b := yardInstallerBundle()
			wantUsers := 0
			switch collision {
			case "username":
				wantUsers = 1
				if err := s.Users().CreateUser(ctx, &User{ID: "foreign", Username: "EVERYDAY", PasswordHash: "retained-hash", Role: "admin", Status: "active", SSOProvider: "local"}); err != nil {
					t.Fatal(err)
				}
			case "provider":
				if err := s.Settings().SetSetting(ctx, "sso_providers_enc", "retained"); err != nil {
					t.Fatal(err)
				}
			case "audit":
				ddl := `CREATE TRIGGER fail_installer_audit BEFORE INSERT ON audit_records WHEN NEW.user_id='installer' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END;`
				if s.Driver() == "postgres" {
					ddl = `CREATE FUNCTION fail_installer_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.user_id='installer' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_installer_audit BEFORE INSERT ON audit_records FOR EACH ROW EXECUTE FUNCTION fail_installer_audit();`
				}
				if _, err := s.db.ExecContext(ctx, ddl); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ApplyInstaller(ctx, b, false); err == nil {
				t.Fatal("adopted or committed without audit")
			}
			if n, _ := s.Users().CountUsers(ctx); n != wantUsers {
				t.Fatalf("partial identities %d", n)
			}
			if _, managed, err := s.InstallerConfiguration(ctx); err != nil || managed {
				t.Fatalf("partial configuration %v %v", managed, err)
			}
			if collision == "username" {
				if u, err := s.Users().GetUserByID(ctx, "foreign"); err != nil || u.PasswordHash != "retained-hash" {
					t.Fatalf("foreign credential changed %+v %v", u, err)
				}
			}
		})
	}
}
