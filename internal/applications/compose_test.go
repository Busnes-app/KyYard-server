package applications

import (
	"encoding/json"
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
