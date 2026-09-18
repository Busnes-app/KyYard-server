package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/config"
)

var envNames = regexp.MustCompile(`KY_[A-Z0-9_]+`)

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

// The overlay for a containerised reverse proxy exists to remove the host publish; an edit
// that leaves a published port behind would put plain HTTP back on the host while looking
// like it had not.
func TestTheContainerProxyOverlayPublishesNothing(t *testing.T) {
	overlay := read(t, filepath.Join("..", "..", "docker-compose.proxy-network.yml"))
	if !strings.Contains(overlay, "ports: !reset []") {
		t.Error("the overlay does not clear the base file's published port")
	}
	if !strings.Contains(overlay, "external: true") || !strings.Contains(overlay, "KY_PROXY_NETWORK") {
		t.Error("the overlay does not join the proxy's existing network")
	}
	// A publish reintroduced by hand, in any form.
	for _, line := range strings.Split(overlay, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") && strings.Contains(trimmed, ":9273") && !strings.HasPrefix(trimmed, "#") {
			t.Errorf("the overlay publishes a port again: %q", trimmed)
		}
	}
}
