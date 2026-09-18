package config_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/config"
)

func TestConfigLoadDefaults(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Server.Host != "127.0.0.1" || cfg.Security.CookieSecure || cfg.SSO.Enabled || cfg.SCIM.Enabled {
		t.Fatal("unsafe or non-minimal defaults")
	}

	if cfg.Server.DockerSocket != "/var/run/docker.sock" {
		t.Fatalf("local Docker default: %q", cfg.Server.DockerSocket)
	}
	if cfg.Server.AppName != "KyYard" {
		t.Errorf("expected default product name KyYard, got %s", cfg.Server.AppName)
	}
	if cfg.Server.Port != config.DefaultPort {
		t.Errorf("expected default port %d, got %d", config.DefaultPort, cfg.Server.Port)
	}
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("expected default driver sqlite, got %s", cfg.Database.Driver)
	}
	if cfg.Captcha.Provider != "pow" {
		t.Errorf("expected default captcha provider pow, got %s", cfg.Captcha.Provider)
	}
	if cfg.Backup.Dir != "" {
		t.Errorf("expected empty default backup dir (sealed local copies off), got %q", cfg.Backup.Dir)
	}
}

func TestConfigLoadFromEnvOverrides(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	t.Setenv("KY_PORT", "9090")
	t.Setenv("KY_DOCKER_SOCKET", "")
	t.Setenv("KY_DB_DRIVER", "postgres")
	t.Setenv("KY_DB_DSN", "postgres://user:pass@localhost:5432/testdb")
	t.Setenv("KY_APP_NAME", "CustomKyYard")
	t.Setenv("KY_CAPTCHA_PROVIDER", "turnstile")

	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Server.DockerSocket != "" {
		t.Fatal("empty socket did not disable local Docker")
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Server.Port)
	}
	if cfg.Database.Driver != "postgres" {
		t.Errorf("expected driver postgres, got %s", cfg.Database.Driver)
	}
	if cfg.Database.DSN != "postgres://user:pass@localhost:5432/testdb" {
		t.Errorf("expected custom DSN, got %s", cfg.Database.DSN)
	}
	if cfg.Server.AppName != "CustomKyYard" {
		t.Errorf("expected custom app name, got %s", cfg.Server.AppName)
	}
	if cfg.Captcha.Provider != "turnstile" {
		t.Errorf("expected captcha provider turnstile, got %s", cfg.Captcha.Provider)
	}
}

func TestEncryptionKeyPersistsAcrossLoads(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	t.Setenv("KY_ENCRYPTION_KEY", "")
	a, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	b, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Security.EncryptionKey) != 32 || !bytes.Equal(a.Security.EncryptionKey, b.Security.EncryptionKey) {
		t.Fatal("encryption key was not persisted between loads")
	}
}

func TestEncryptionKeyFromEnvMustBe32Bytes(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	t.Setenv("KY_ENCRYPTION_KEY", "deadbeef")
	if _, err := config.LoadFromEnv(); err == nil {
		t.Fatal("8-byte key accepted")
	}
}

func TestDepositIntervalFromEnv(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 24 * time.Hour, true},
		{"90m", 90 * time.Minute, true},
		{"15m", 15 * time.Minute, true},
		{"0", 0, true},
		{"1s", 0, false},
		{"14m", 0, false},
		{"-1h", 0, false},
		{"daily", 0, false},
	} {
		t.Setenv("KY_BACKUP_DEPOSIT_INTERVAL", tc.in)
		cfg, err := config.LoadFromEnv()
		if (err == nil) != tc.ok {
			t.Errorf("%q: err=%v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && cfg.Backup.DepositInterval != tc.want {
			t.Errorf("%q: got %v, want %v", tc.in, cfg.Backup.DepositInterval, tc.want)
		}
	}
}

func TestBackupConfigFromEnv(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	t.Setenv("KY_BACKUP_DIR", "/tmp/x")
	t.Setenv("KY_BACKUP_KEEP", "3")
	t.Setenv("KY_BACKUP_ALLOW_PRIVATE_RECOVERY", "true")
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Dir != "/tmp/x" || cfg.Backup.Keep != 3 || !cfg.Backup.AllowPrivateRecovery {
		t.Fatalf("%+v", cfg.Backup)
	}
}

func TestBackupKeepBelowOneIsRefused(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	t.Setenv("KY_BACKUP_KEEP", "0")
	if _, err := config.LoadFromEnv(); err == nil || !strings.Contains(err.Error(), "KY_BACKUP_KEEP") {
		t.Fatalf("want KY_BACKUP_KEEP error, got %v", err)
	}
}

func TestAgentImageMustBeDigestPinned(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	for _, bad := range []string{"ghcr.io/busnes-app/kyyard-agent:latest", "ghcr.io/busnes-app/kyyard-agent", "kyyard-agent@sha256:abc", "ghcr.io/busnes-app/kyyard-agent@sha256:" + strings.Repeat("g", 64)} {
		t.Setenv("KY_AGENT_IMAGE", bad)
		if _, err := config.LoadFromEnv(); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	good := "ghcr.io/busnes-app/kyyard-agent@sha256:" + strings.Repeat("a", 64)
	t.Setenv("KY_AGENT_IMAGE", good)
	cfg, err := config.LoadFromEnv()
	if err != nil || cfg.Server.AgentImage != good {
		t.Fatalf("digest reference refused: %v", err)
	}
	t.Setenv("KY_AGENT_IMAGE", "")
	if cfg, err := config.LoadFromEnv(); err != nil || cfg.Server.AgentImage != "" {
		t.Fatalf("empty image must be allowed: %v", err)
	}
}

// A control that is silently off is worse than one that refuses to start: zero is the only
// documented way to disable the budget, so every other unusable value must be refused.
func TestDiskBudgetRejectsNonsense(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        int64
		wantErr     bool
	}{
		{name: "default when unset", value: "", want: 2 << 30},
		{name: "zero disables", value: "0", want: 0},
		{name: "documented default", value: "2147483648", want: 2 << 30},
		{name: "negative is refused", value: "-1", wantErr: true},
		{name: "absurd is refused", value: "9223372036854775807", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KY_DATA_DIR", t.TempDir())
			t.Setenv("KY_APP_URL", "http://localhost:8080")
			if tc.value != "" {
				t.Setenv("KY_RETENTION_DISK_BUDGET", tc.value)
			}
			cfg, err := config.LoadFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q was accepted, arming nothing", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Database.DiskBudget != tc.want {
				t.Fatalf("budget %d, want %d", cfg.Database.DiskBudget, tc.want)
			}
			// The 95 % threshold must stay positive, or every pass reads as over budget.
			if cfg.Database.DiskBudget > 0 && cfg.Database.DiskBudget/100*95 <= 0 {
				t.Fatalf("threshold underflowed for budget %d", cfg.Database.DiskBudget)
			}
		})
	}
}
