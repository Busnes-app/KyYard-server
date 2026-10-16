package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "ky.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// A generated bootstrap password lands in an owner-only file, never in the log, which
// docker logs and log shippers keep and copy.
func TestBootstrapWritesThePasswordToAFileNotTheLog(t *testing.T) {
	st, dir := openTestStore(t), t.TempDir()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	if err := bootstrapAdmin(context.Background(), st, dir, ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, config.InitialAdminPasswordFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode %o, want 600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	pass := strings.TrimSpace(string(raw))
	admin, err := st.Users().GetUserByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := password.Verify(pass, admin.PasswordHash); !ok || !admin.MustChangePassword {
		t.Fatal("the file does not hold the admin's temporary password")
	}
	if strings.Contains(logged.String(), pass) || !strings.Contains(logged.String(), path) {
		t.Fatalf("log must name the file and not the password: %q", logged.String())
	}
}

func TestBootstrapEnforcesThePasswordPolicyOnASuppliedPassword(t *testing.T) {
	st := openTestStore(t)
	if err := bootstrapAdmin(context.Background(), st, t.TempDir(), "short"); err == nil {
		t.Fatal("a short KY_ADMIN_PASSWORD was accepted")
	}
	if n, _ := st.Users().CountUsers(context.Background()); n != 0 {
		t.Fatalf("%d users created from a refused password", n)
	}
}

// stdinWith returns a file whose contents are s, standing in for a pipe on stdin.
func stdinWith(t *testing.T, s string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// argv is world-readable and kept in shell history, so init-admin takes the password on stdin.
func TestInitAdminReadsThePasswordFromStdin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KY_DATA_DIR", dir)
	t.Setenv("KY_DB_DRIVER", "sqlite")

	if err := initAdmin([]string{"-password", "OperatorResetPass789!"}, stdinWith(t, "")); err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("-password answered %v, want a refusal pointing at stdin", err)
	}
	if err := initAdmin(nil, stdinWith(t, "short\n")); err == nil {
		t.Fatal("a short password on stdin was accepted")
	}
	if err := initAdmin([]string{"-username", "ops"}, stdinWith(t, "OperatorResetPass789!\n")); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, err := st.Users().GetUserByUsername(context.Background(), "ops")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := password.Verify("OperatorResetPass789!", u.PasswordHash); !ok {
		t.Fatal("the stdin password, without its newline, does not sign in")
	}
}
