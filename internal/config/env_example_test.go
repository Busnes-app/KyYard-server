package config_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/config"
)

var envNames = regexp.MustCompile(`KY_[A-Z0-9_]+`)

// A listed entry in .env.example, as opposed to a name mentioned in prose.
var serverEntry = regexp.MustCompile(`(?m)^#   (KY_[A-Z0-9_]+)`)

// The example file is the one place an operator looks for what they can set. It is worth only
// as much as its accuracy, and accuracy is exactly what rots: a setting added to the loader
// and not to the file is invisible, and a name left in the file after the code stopped reading
// it sends someone configuring something that does nothing.
func TestEnvExampleMatchesWhatTheCodeReads(t *testing.T) {
	root := filepath.Join("..", "..")
	example := read(t, filepath.Join(root, ".env.example"))
	documented := map[string]bool{}
	for _, name := range envNames.FindAllString(example, -1) {
		documented[name] = true
	}
	if len(documented) == 0 {
		t.Fatal(".env.example names no settings at all")
	}

	// Everything the server reads is documented.
	sources := read(t, filepath.Join(root, "internal", "config", "config.go")) +
		read(t, filepath.Join(root, "cmd", "server", "main.go"))
	for _, name := range envNames.FindAllString(sources, -1) {
		if !documented[name] {
			t.Errorf("%s is read by the server and missing from .env.example", name)
		}
	}

	// And everything documented is read by something: the server, or a compose file.
	compose := ""
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "docker-compose") {
			compose += read(t, filepath.Join(root, e.Name()))
		}
	}
	known := sources + compose + read(t, filepath.Join(root, "Dockerfile"))
	for name := range documented {
		if !strings.Contains(known, name) {
			t.Errorf("%s is in .env.example and nothing reads it", name)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The image, the compose file and the loader have to agree about the port. They are three
// different files, and a default that moves in one of them is a server nobody can reach.
func TestThePackagedPortMatchesTheDefault(t *testing.T) {
	root := filepath.Join("..", "..")
	port := strconv.Itoa(config.DefaultPort)
	// The guide the login page shows is the only place the product itself tells an operator
	// what to keep private, so it must read the port from the origin rather than carry one.
	guide := read(t, filepath.Join(root, "web", "src", "components", "SetupGuide.tsx"))
	if !strings.Contains(guide, "Keep port {port} private") || !strings.Contains(guide, "localhost:{port}") {
		t.Error("SetupGuide.tsx names a port of its own instead of the one it was given")
	}
	for _, want := range []struct{ file, text string }{
		{"Dockerfile", "ENV KY_PORT=" + port},
		{"Dockerfile", "EXPOSE " + port},
		{"docker-compose.yml", "KY_PORT=" + port},
		{"docker-compose.yml", "127.0.0.1:" + port + ":" + port},
		{"docker-compose.yml", "KY_APP_URL=http://localhost:" + port},
		{".env.example", "KY_PORT=" + port},
		{"README.md", "http://localhost:" + port},
		{filepath.Join("web", "vite.config.ts"), "http://localhost:" + port},
	} {
		if !strings.Contains(read(t, filepath.Join(root, want.file)), want.text) {
			t.Errorf("%s does not carry %q, so the packaged port and the default disagree", want.file, want.text)
		}
	}
}

// The product uses Docker's pre-existing bridge; no Compose-owned network is needed.
func TestComposeUsesExistingBridge(t *testing.T) {
	for _, file := range []string{"docker-compose.yml", "docker-compose.proxy-network.yml"} {
		content := read(t, filepath.Join("..", "..", file))
		if !strings.Contains(content, "network_mode: bridge") {
			t.Errorf("%s does not use the existing bridge", file)
		}
		for _, line := range strings.Split(content, "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "#") {
				continue
			}
			if strings.HasPrefix(code, "networks:") || strings.Contains(code, "KY_PROXY_NETWORK") {
				t.Errorf("%s declares a dedicated network: %s", file, code)
			}
		}
	}
}

func TestSharedBridgeCredentialGuidance(t *testing.T) {
	example := read(t, filepath.Join("..", "..", ".env.example"))
	if strings.Contains(example, "sslmode=disable") || strings.Contains(example, "compose-internal network") {
		t.Fatal("shared bridge guidance must not advertise an isolated plaintext database")
	}
	for _, text := range []string{"sslmode=verify-full", "cannot pin", "host-network", "gateway"} {
		if !strings.Contains(example, text) {
			t.Errorf("missing shared bridge guidance: %s", text)
		}
	}
}

// Settings in .env.server reach the server only through the optional env_file. Naming one in a
// compose environment list would shadow an operator's own env_file with an empty value.
func TestComposeForwardsServerSettingsFile(t *testing.T) {
	root := filepath.Join("..", "..")
	if !strings.Contains(read(t, filepath.Join(root, "docker-compose.yml")), "env_file:\n      - path: .env.server\n        required: false\n") {
		t.Fatal("docker-compose.yml does not pass the optional .env.server")
	}
	example := read(t, filepath.Join(root, ".env.example"))
	start := strings.Index(example, "Server settings: put these in .env.server")
	if start < 0 {
		t.Fatal(".env.example has no .env.server section")
	}
	section, _, _ := strings.Cut(example[start:], "\n\n")
	var server []string
	for _, m := range serverEntry.FindAllStringSubmatch(section, -1) {
		server = append(server, m[1])
	}
	if len(server) < 10 {
		t.Fatalf(".env.example .env.server section names only %d settings", len(server))
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "docker-compose") {
			continue
		}
		for _, line := range strings.Split(read(t, filepath.Join(root, e.Name())), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "#") {
				continue
			}
			for _, name := range server {
				if strings.HasPrefix(code, "- "+name+"=") || code == "- "+name || strings.HasPrefix(code, name+":") {
					t.Errorf("%s lists %s, shadowing .env.server and operator env_files", e.Name(), name)
				}
			}
		}
	}
}

// The kit's postgres-17-to-18.sh, vendored byte-identical so every Ky repo diffs against it.
const postgresMoveScriptSHA256 = "2992644109716cd34297fbcb76eba8907f4a5e3031a12c05aade010e5ea2b571"

// PostgreSQL 18's image refuses a mount at /var/lib/postgresql/data and cannot open 17 data.
// The overlay moves existing installs: a 17 one-shot dumps the old pgdata volume into the new
// volume, which 18 mounts at /var/lib/postgresql and restores from. Losing any piece strands
// an operator's database, so each one is pinned here.
func TestPostgresOverlayMovesSeventeenToEighteen(t *testing.T) {
	root := filepath.Join("..", "..")
	overlay := read(t, filepath.Join(root, "docker-compose.postgres.yml"))
	for _, want := range []string{
		"  postgres-17-dump:\n    image: postgres:17.12-alpine3.24@sha256:f9cf3e1f0066f0eb0c48707d864f0ac5941e9ff454a072077d97a4d278d07d73\n    network_mode: none\n    restart: \"no\"\n    # Long enough for 17 to stop cleanly; a SIGKILL leaves postmaster.pid, which the dump refuses.\n    stop_grace_period: 2m\n    entrypoint: [\"bash\", \"/ky/postgres-17-to-18.sh\", \"dump\"]\n",
		"      POSTGRES_USER: kyyard\n      OLD: /var/lib/postgresql/data\n      NEW: /pg18\n",
		"      - pgdata:/var/lib/postgresql/data\n      - postgres:/pg18\n      - ./scripts/postgres-17-to-18.sh:/ky/postgres-17-to-18.sh:ro\n",
		"  postgres:\n    image: postgres:18.7-alpine3.24@sha256:0115267ab5a509022df9319c77be365cef335adb71556c75f60f5360ee247e71\n    restart: unless-stopped\n    stop_grace_period: 2m\n    entrypoint: [\"bash\", \"/ky/postgres-17-to-18.sh\", \"restore\"]\n    command: [\"postgres\", ",
		"      - postgres:/var/lib/postgresql\n      - ${KY_POSTGRES_TLS_DIR:?Set the absolute PostgreSQL TLS directory}:/tls:ro\n      - ./scripts/postgres-17-to-18.sh:/ky/postgres-17-to-18.sh:ro\n",
		"      postgres-17-dump:\n        condition: service_completed_successfully\n",
		"      start_period: 1h\n",
		"volumes:\n  # PostgreSQL 17's data, kept for rollback; only postgres-17-dump reads it.\n  pgdata:\n  postgres:\n",
	} {
		if !strings.Contains(overlay, want) {
			t.Errorf("docker-compose.postgres.yml lost:\n%s", want)
		}
	}
	if n := strings.Count(overlay, ":/var/lib/postgresql/data"); n != 1 {
		t.Errorf("%d mounts at /var/lib/postgresql/data; only the 17 dump may have one", n)
	}
	if strings.Contains(overlay, "PGDATA") {
		t.Error("the overlay sets PGDATA; 18 must keep its default under /var/lib/postgresql")
	}
	script, err := os.ReadFile(filepath.Join(root, "scripts", "postgres-17-to-18.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(script)); got != postgresMoveScriptSHA256 {
		t.Errorf("scripts/postgres-17-to-18.sh is %s, not the kit's %s: fix the kit, then vendor it", got, postgresMoveScriptSHA256)
	}
	if info, err := os.Stat(filepath.Join(root, "scripts", "postgres-17-to-18.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("scripts/postgres-17-to-18.sh must be mode 0755: %v %v", info, err)
	}
}
