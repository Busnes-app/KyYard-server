package registry

import (
	"errors"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// A reference comes from a request body. An accepted one is in the protocol grammar, names a
// canonical host and exactly one of tag or digest, and written back out in full it parses to
// itself; a refused one is ErrInvalidReference with nothing filled in.
func FuzzParseReference(f *testing.F) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, ref := range []string{
		"nginx", "nginx:1", "library/nginx:1", "docker.io/library/nginx:1", "docker.io/nginx",
		"index.docker.io/nginx", "registry-1.docker.io/org/app", "bitnami/redis:7",
		"ghcr.io/org/app@" + digest, "GHCR.io/org/app:v2", "registry.example:5000/app:v1",
		"localhost:5000/app", "localhost/app",
		"", "nginx:", "nginx@sha256:short", "../etc", "a//b", "a/../b", "https://ghcr.io/app",
		"nginx:1@" + digest, strings.Repeat("a", 513), digest,
		"localhost:5000", "docker.io:443/nginx", "a:1/b:c", "INDEX.DOCKER.IO/x",
	} {
		f.Add(ref)
	}
	f.Fuzz(func(t *testing.T, ref string) {
		r, err := ParseReference(ref)
		if err != nil {
			if !errors.Is(err, ErrInvalidReference) || r != (Reference{}) {
				t.Fatalf("ParseReference(%q) = %+v, %v; want zero and ErrInvalidReference", ref, r, err)
			}
			return
		}
		if !protocol.ValidImageReference(ref) || strings.HasPrefix(ref, "sha256:") {
			t.Fatalf("ParseReference(%q) accepted what the grammar refuses", ref)
		}
		if r.Host == "" || r.Host != CanonicalHost(r.Host) || r.Repository == "" || strings.Contains(r.Repository, ":") {
			t.Fatalf("ParseReference(%q) = %+v: host not canonical or repository malformed", ref, r)
		}
		if (r.Tag == "") == (r.Digest == "") {
			t.Fatalf("ParseReference(%q) = %+v: want exactly one of tag and digest", ref, r)
		}
		if r.Host == DockerHub && !strings.Contains(r.Repository, "/") {
			t.Fatalf("ParseReference(%q) = %+v: Docker Hub repository without a namespace", ref, r)
		}
		full := r.Host + "/" + r.Repository + ":" + r.Tag
		if r.Digest != "" {
			full = r.Host + "/" + r.Repository + "@" + r.Digest
		}
		// Defaults can push a reference near the cap past it; that is the cap, not a bug.
		if len(full) > protocol.MaxImageReferenceBytes {
			return
		}
		if again, err := ParseReference(full); err != nil || again != r {
			t.Fatalf("ParseReference(%q) = %+v, but its full form %q parses to %+v, %v", ref, r, full, again, err)
		}
	})
}
