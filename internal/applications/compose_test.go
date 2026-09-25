package applications

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestComposeImport(t *testing.T) {
	source := `services:
  web:
    image: nginx:1.27
    restart: unless-stopped
    ports:
      - target: 80
        published: "8080"
        host_ip: 127.0.0.1
    environment:
      PASSWORD: "private-$$value"
      EMPTY: ""
  worker:
    image: busybox:1
    environment: ["TOKEN=one=two"]
`
	imported, err := ParseCompose(source)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(imported.Spec)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "one=two") {
		t.Fatal("plaintext in revision")
	}
	web := imported.Spec.Services[0]
	if web.Name != "web" || web.Ports[0].Published != 8080 || imported.Values[web.Environment["PASSWORD"].SecretRef] != "private-$value" {
		t.Fatal("lost configuration")
	}
	again, err := ParseCompose(source)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := json.Marshal(again.Spec)
	if string(raw) != string(other) {
		t.Fatal("nondeterministic spec")
	}
	raw, _ = json.Marshal(imported)
	if string(raw) != "{}" {
		t.Fatal("transient values serializable")
	}
}
func TestComposeRefusalsDoNotExposeValues(t *testing.T) {
	cases := []string{
		"services: {web: {image: nginx:1, ports: [{target: 0777, published: 80}]}}",
		"services: [secret-canary]", "services: {web: {image: nginx:1, build: secret-canary}}",
		"services: {web: {image: nginx:1, env_file: secret-canary}}",
		"services: {web: {image: nginx:1, volumes: [secret-canary]}}",
		"services: {web: {image: nginx:1, environment: {TOKEN: '${secret-canary}'}}}",
		"services: {web: {image: nginx:1, environment: [secret-canary]}}",
		"services: {web: {image: nginx:1, environment: {TOKEN: null}}}",
		"services: {web: {image: nginx:1, environment: {TOKEN: true}}}",
		"services: {web: {image: nginx:1, image: secret-canary}}",
		"services: {web: &secret-canary {image: nginx:1}}",
		"services: {web: {image: !!str secret-canary}}",
		"services: {web: {image: nginx:1, environment: [TOKEN=first, TOKEN=secret-canary]}}",
		"services: {web: {image: nginx:1, ports: ['80:80']}}",
		"services: {web: {image: nginx:1, ports: [{target: 80, published: 999999999999999999999}]}}",
		"services: {web: {image: nginx:1, ports: [{target: 80, published: 80, host_ip: secret-canary}]}}",
		"services: {web: {image: nginx:1, restart: secret-canary}}",
		"services: {web: {image: nginx:1}}\n---\nsecret-canary",
		"services: {web: {image: 'secret-canary}", strings.Repeat("x", MaxComposeBytes+1),
	}
	for _, source := range cases {
		if result, err := ParseCompose(source); err == nil || result != nil {
			t.Fatalf("accepted %q", source[:min(len(source), 100)])
		} else if strings.Contains(err.Error(), "secret-canary") {
			t.Fatal("diagnostic leaked input")
		}
	}
}

func TestComposeStructureBounds(t *testing.T) {
	for _, source := range []string{
		"services: " + strings.Repeat("[", 18) + "x" + strings.Repeat("]", 18),
		"services: [" + strings.Repeat("x,", 8200) + "]",
		"services: {web: {image: nginx:1, environment: {TOKEN: &x value, COPY: *x}}}",
	} {
		if _, err := ParseCompose(source); err == nil {
			t.Fatal("unbounded or aliased document accepted")
		}
	}
}

// volumeDoc puts mounts at line 5 onward (entries at column 7) and top-level volumes after them.
func volumeDoc(mounts, top string) string {
	return "services:\n  app:\n    image: nginx:1\n    volumes:\n" + mounts + top
}

func TestComposeVolumes(t *testing.T) {
	declared := "volumes:\n  db:\n"
	cases := []struct {
		name, source, mounts, declared string
	}{
		{"named", volumeDoc("      - db:/var/lib/postgresql/data\n", declared),
			`[{"kind":"named","source":"db","target":"/var/lib/postgresql/data"}]`, `[{"name":"db"}]`},
		{"read only", volumeDoc("      - db:/x:ro\n      - db:/y:rw\n", "volumes:\n  db: {}\n"),
			`[{"kind":"named","source":"db","target":"/x","read_only":true},{"kind":"named","source":"db","target":"/y"}]`, `[{"name":"db"}]`},
		{"bind", volumeDoc("      - /srv/cfg:/etc/app:ro\n", ""),
			`[{"kind":"bind","source":"/srv/cfg","target":"/etc/app","read_only":true}]`, `null`},
		{"long", volumeDoc("      - type: volume\n        source: db\n        target: /x\n      - type: bind\n        source: /srv\n        target: /y\n        read_only: true\n", declared),
			`[{"kind":"named","source":"db","target":"/x"},{"kind":"bind","source":"/srv","target":"/y","read_only":true}]`, `[{"name":"db"}]`},
		{"external", volumeDoc("      - shared:/x\n      - db:/y\n", "volumes:\n  shared:\n    external: true\n  db:\n    external: false\n"),
			`[{"kind":"named","source":"shared","target":"/x"},{"kind":"named","source":"db","target":"/y"}]`, `[{"name":"db"},{"name":"shared","external":true}]`},
		{"two-character name", volumeDoc("      - ab:/x\n", "volumes:\n  ab:\n"),
			`[{"kind":"named","source":"ab","target":"/x"}]`, `[{"name":"ab"}]`},
	}
	for _, c := range cases {
		imported, err := ParseCompose(c.source)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		mounts, _ := json.Marshal(imported.Spec.Services[0].Volumes)
		declared, _ := json.Marshal(imported.Spec.Volumes)
		if string(mounts) != c.mounts || string(declared) != c.declared {
			t.Errorf("%s: got %s %s", c.name, mounts, declared)
		}
	}
}

func TestComposeVolumeRefusals(t *testing.T) {
	const (
		relative   = "Relative and home-relative bind mounts are unsupported; write the absolute path"
		anonymous  = "Anonymous volumes are unsupported; declare a named volume"
		undeclared = "Named volumes must be declared under top-level volumes"
		mode       = "Volume mode must be ro or rw"
		kind       = "Volume type must be volume or bind"
		boolean    = "An explicit true or false is required"
		list       = "volumes must be a list of at most 32 entries"
		unsupport  = "Unsupported field; see the supported import fields"
		normalized = "Paths must be absolute and normalized (no trailing slash, surrounding spaces, . or ..)"
		required   = "Volume source and target are required"
		longKeys   = "Long-syntax volumes require type and target"
	)
	declared := "volumes:\n  db:\n"
	var many strings.Builder
	for i := 0; i < 33; i++ {
		fmt.Fprintf(&many, "      - db:/v%d\n", i)
	}
	var manyDeclared strings.Builder
	manyDeclared.WriteString("volumes:\n")
	for i := 0; i < 65; i++ {
		fmt.Fprintf(&manyDeclared, "  v%d:\n", i)
	}
	cases := []struct {
		name, source string
		line, column int
		reason       string
	}{
		{"dot bind", volumeDoc("      - ./data:/x\n", ""), 5, 9, relative},
		{"home bind", volumeDoc("      - ~/data:/x\n", ""), 5, 9, relative},
		{"windows bind", volumeDoc("      - C:\\data:/x\n", ""), 5, 9, relative},
		{"one-character declared name", volumeDoc("      - a:/x\n", "volumes:\n  a:\n"), 7, 3, "Volume names must match [a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}"},
		{"bind trailing slash", volumeDoc("      - /srv/cfg/:/etc/app\n", ""), 5, 9, normalized},
		{"bind double slash", volumeDoc("      - //srv:/x\n", ""), 5, 9, normalized},
		{"bind dot dot", volumeDoc("      - /a/../b:/x\n", ""), 5, 9, normalized},
		{"relative target", volumeDoc("      - db:data\n", declared), 5, 9, normalized},
		{"unclean target", volumeDoc("      - db:/x/\n", declared), 5, 9, normalized},
		{"trailing space target", volumeDoc("      - \"db:/x \"\n", declared), 5, 9, normalized},
		{"root target", volumeDoc("      - db:/\n", declared), 5, 9, "A volume cannot be mounted at /"},
		{"duplicate target", volumeDoc("      - db:/x\n      - /srv:/x\n", declared), 6, 9, "Duplicate volume target"},
		{"empty source", volumeDoc("      - :/x\n", declared), 5, 9, required},
		{"empty target", volumeDoc("      - \"db:\"\n", declared), 5, 9, required},
		{"long no type", volumeDoc("      - source: db\n        target: /x\n", declared), 5, 9, longKeys},
		{"long no target", volumeDoc("      - type: volume\n        source: db\n", declared), 5, 9, longKeys},
		{"long relative target", volumeDoc("      - type: volume\n        source: db\n        target: rel\n", declared), 7, 17, normalized},
		{"long duplicate target", volumeDoc("      - db:/x\n      - type: bind\n        source: /srv\n        target: /x\n", declared), 8, 17, "Duplicate volume target"},
		{"long bind unclean", volumeDoc("      - type: bind\n        source: /srv/\n        target: /x\n", ""), 6, 17, normalized},
		{"bad declared name", volumeDoc("      - db:/x\n", "volumes:\n  db:\n  _cache:\n"), 8, 3, "Volume names must match [a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}"},
		{"anonymous", volumeDoc("      - /x\n", ""), 5, 9, anonymous},
		{"undeclared", volumeDoc("      - cache:/x\n", declared), 5, 9, undeclared},
		{"mode", volumeDoc("      - db:/x:z\n", declared), 5, 9, mode},
		{"four parts", volumeDoc("      - db:/x:ro:z\n", declared), 5, 9, mode},
		{"not a list", volumeDoc("", "")[:len(volumeDoc("", ""))-1] + " db:/x\n" + declared, 4, 14, list},
		{"too many", volumeDoc(many.String(), declared), 5, 7, list},
		{"long tmpfs", volumeDoc("      - type: tmpfs\n        target: /x\n", ""), 5, 15, kind},
		{"long consistency", volumeDoc("      - type: volume\n        source: db\n        target: /x\n        consistency: cached\n", declared), 8, 22, unsupport},
		{"long anonymous", volumeDoc("      - type: volume\n        target: /x\n", ""), 5, 9, anonymous},
		{"long relative", volumeDoc("      - type: bind\n        source: ./data\n        target: /x\n", ""), 6, 17, relative},
		{"long undeclared", volumeDoc("      - type: volume\n        source: cache\n        target: /x\n", declared), 6, 17, undeclared},
		{"long read_only string", volumeDoc("      - type: volume\n        source: db\n        target: /x\n        read_only: \"true\"\n", declared), 8, 20, boolean},
		{"top driver", volumeDoc("      - db:/x\n", "volumes:\n  db:\n    driver: local\n"), 8, 13, unsupport},
		{"top name", volumeDoc("      - db:/x\n", "volumes:\n  db:\n    name: other\n"), 8, 11, unsupport},
		{"top external string", volumeDoc("      - db:/x\n", "volumes:\n  db:\n    external: \"yes\"\n"), 8, 15, boolean},
		{"top list", volumeDoc("      - db:/x\n", "volumes: [db]\n"), 6, 10, "Expected a mapping"},
		{"top too many", volumeDoc("      - v0:/x\n", manyDeclared.String()), 7, 3, "At most 64 volumes may be declared"},
	}
	for _, c := range cases {
		result, err := ParseCompose(c.source)
		var d *Diagnostic
		if result != nil || !errors.As(err, &d) {
			t.Errorf("%s: accepted or untyped: %v", c.name, err)
			continue
		}
		if d.Line != c.line || d.Column != c.column || d.Reason != c.reason {
			t.Errorf("%s: got %d:%d %q", c.name, d.Line, d.Column, d.Reason)
		}
	}
}
