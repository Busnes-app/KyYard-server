package config_test

import (
	"github.com/Busness-app/kyyard-server/internal/config"
	"testing"
)

func TestTransportConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, url, proxy, cookie string
		secure, invalid          bool
	}{
		{name: "production loopback", url: "http://localhost:8080"},
		{name: "IPv4 loopback", url: "http://127.0.0.1:8080"},
		{name: "IPv6 loopback", url: "http://[::1]:8080"},
		{name: "HTTPS", url: "https://yard.example.com", proxy: "127.0.0.1", secure: true},
		{name: "remote HTTP", url: "http://yard.example.com", invalid: true},
		{name: "LAN HTTP", url: "http://192.168.1.2", invalid: true},
		{name: "HTTPS without proxy", url: "https://yard.example.com", invalid: true},
		{name: "insecure HTTPS cookie", url: "https://yard.example.com", proxy: "127.0.0.1", cookie: "false", invalid: true},
		{name: "secure HTTP cookie", url: "http://localhost:8080", cookie: "true", invalid: true},
		{name: "credentials", url: "http://user:password@localhost", invalid: true},
		{name: "path", url: "http://localhost/path", invalid: true},
		{name: "query", url: "http://localhost/?x=1", invalid: true},
		{name: "fragment", url: "http://localhost/#x", invalid: true},
		{name: "port overflow", url: "http://localhost:65536", invalid: true},
		{name: "empty port", url: "http://localhost:", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KY_DATA_DIR", t.TempDir())
			t.Setenv("KY_ENV", "production")
			t.Setenv("KY_APP_URL", tc.url)
			t.Setenv("KY_TRUSTED_PROXIES", tc.proxy)
			t.Setenv("KY_COOKIE_SECURE", tc.cookie)
			cfg, err := config.LoadFromEnv()
			if tc.invalid {
				if err == nil {
					t.Fatal("accepted unsafe transport")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Security.CookieSecure != tc.secure {
				t.Fatal("cookie security does not match transport")
			}
		})
	}
}

func TestAdvertisedOriginNormalization(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	t.Setenv("KY_APP_URL", "https://YARD.example.com:443/")
	t.Setenv("KY_TRUSTED_PROXIES", "127.0.0.1")
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AppURL != "https://yard.example.com" {
		t.Fatalf("non-browser origin: %s", cfg.Server.AppURL)
	}
}
