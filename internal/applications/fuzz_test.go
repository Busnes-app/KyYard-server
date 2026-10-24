package applications

import (
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// Compose source is pasted by a user. A refusal is a Diagnostic with a fixed reason and no
// result; an accepted import is a spec the store accepts, every environment value is behind
// exactly one reference, and parsing again gives the same spec and values.
func FuzzParseCompose(f *testing.F) {
	for _, source := range []string{
		"services:\n  web:\n    image: nginx:1.27\n    restart: unless-stopped\n    ports:\n      - target: 80\n        published: \"8080\"\n        host_ip: 127.0.0.1\n    environment:\n      PASSWORD: \"private-$$value\"\n      EMPTY: \"\"\n  worker:\n    image: busybox:1\n    environment: [\"TOKEN=one=two\"]\n",
		volumeDoc("      - db:/x:ro\n      - db:/y:rw\n", "volumes:\n  db: {}\n"),
		volumeDoc("      - /srv/cfg:/etc/app:ro\n", ""),
		volumeDoc("      - type: volume\n        source: db\n        target: /x\n      - type: bind\n        source: /srv\n        target: /y\n        read_only: true\n", "volumes:\n  db:\n"),
		volumeDoc("      - shared:/x\n      - db:/y\n", "volumes:\n  shared:\n    external: true\n  db:\n    external: false\n"),
		volumeDoc("      - ./data:/x\n", ""),
		volumeDoc("      - C:/data:/x\n", ""),
		volumeDoc("      - /srv/:/x\n", ""),
		"services: {web: {image: nginx:1, ports: [{target: 0777, published: 80}]}}",
		"services: {web: {image: nginx:1, environment: {TOKEN: '${secret-canary}'}}}",
		"services: {web: {image: nginx:1, environment: [TOKEN=first, TOKEN=second]}}",
		"services: {web: {image: nginx:1, ports: [{target: 80, published: 999999999999999999999}]}}",
		"services: {web: &a {image: nginx:1}}",
		"services: {web: {image: !!str nginx}}",
		"services: {web: {image: nginx:1}}\n---\nx",
		"services: " + strings.Repeat("[", 18) + "x" + strings.Repeat("]", 18),
		"services: {Web: {image: nginx:1}}",
		"services: {web: {image: nginx:1, environment: {'bad key': x}}}",
	} {
		f.Add(source)
	}
	f.Fuzz(func(t *testing.T, source string) {
		imported, err := ParseCompose(source)
		if err != nil {
			var d *Diagnostic
			if imported != nil || !errors.As(err, &d) || d.Reason == "" || strings.Contains(strings.ToLower(d.Reason), "yaml:") {
				t.Fatalf("refusal %T %v with result %v; want a bare Diagnostic", err, err, imported != nil)
			}
			return
		}
		if err := store.ValidateApplicationSpec(imported.Spec); err != nil {
			t.Fatalf("accepted a spec the store refuses: %v", err)
		}
		refs := map[string]bool{}
		for i, s := range imported.Spec.Services {
			if i > 0 && imported.Spec.Services[i-1].Name >= s.Name {
				t.Fatalf("services not in strict name order: %q then %q", imported.Spec.Services[i-1].Name, s.Name)
			}
			for key, ref := range s.Environment {
				if _, ok := imported.Values[ref.SecretRef]; !ok || refs[ref.SecretRef] {
					t.Fatalf("service %q key %q: reference missing from values or shared", s.Name, key)
				}
				refs[ref.SecretRef] = true
			}
		}
		if len(refs) != len(imported.Values) {
			t.Fatalf("%d values for %d references", len(imported.Values), len(refs))
		}
		again, err := ParseCompose(source)
		if err != nil {
			t.Fatalf("second parse refused: %v", err)
		}
		first, _ := json.Marshal(imported.Spec)
		second, _ := json.Marshal(again.Spec)
		if string(first) != string(second) || !maps.Equal(imported.Values, again.Values) {
			t.Fatal("nondeterministic import")
		}
	})
}
