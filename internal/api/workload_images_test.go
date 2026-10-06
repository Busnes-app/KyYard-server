package api_test

import (
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/api"
)

var (
	digestA = "sha256:" + strings.Repeat("a", 64)
	digestB = "sha256:" + strings.Repeat("b", 64)
)

func TestTrackedReference(t *testing.T) {
	for _, c := range []struct {
		image, name, repo, tag, pinned string
		ok                             bool
	}{
		{"nginx", "nginx", "library/nginx", "latest", "", true},
		{"ghcr.io/acme/web:2", "ghcr.io/acme/web:2", "acme/web", "2", "", true},
		{"ghcr.io/acme/web:2@" + digestA, "ghcr.io/acme/web:2", "acme/web", "2", digestA, true},
		{"localhost:5000/web:1@" + digestA, "localhost:5000/web:1", "web", "1", digestA, true},
		// Digest-only tracks nothing.
		{"ghcr.io/acme/web@" + digestA, "", "", "", digestA, false},
		{"localhost:5000/web@" + digestA, "", "", "", digestA, false},
		{"sha256:" + strings.Repeat("c", 64), "", "", "", "", false},
		{"", "", "", "", "", false},
	} {
		name, ref, pinned, ok := api.TrackedReferenceForTest(c.image)
		if ok != c.ok || pinned != c.pinned || (ok && (name != c.name || ref.Repository != c.repo || ref.Tag != c.tag || ref.Digest != "")) {
			t.Errorf("%q: %q %+v %q %v", c.image, name, ref, pinned, ok)
		}
	}
}

func TestRunningDigest(t *testing.T) {
	_, ref, _, _ := api.TrackedReferenceForTest("nginx:1.27")
	for _, c := range []struct{ id, want string }{
		{"docker.io/library/nginx@" + digestA, digestA},
		{"docker-pullable://nginx@" + digestB, digestB},
		{"ghcr.io/acme/nginx@" + digestA, ""},
		{"docker.io/library/nginx@sha256:short", ""},
		{"sha256:" + strings.Repeat("c", 64), ""},
		{"", ""},
	} {
		if got := api.RunningDigestForTest(c.id, ref); got != c.want {
			t.Errorf("%q: %q, want %q", c.id, got, c.want)
		}
	}
}
