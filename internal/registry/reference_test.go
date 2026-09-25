package registry

import (
	"strings"
	"testing"
)

func TestParseReference(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for ref, want := range map[string]Reference{
		"nginx":                        {Host: "docker.io", Repository: "library/nginx", Tag: "latest"},
		"nginx:1":                      {Host: "docker.io", Repository: "library/nginx", Tag: "1"},
		"library/nginx:1":              {Host: "docker.io", Repository: "library/nginx", Tag: "1"},
		"docker.io/library/nginx:1":    {Host: "docker.io", Repository: "library/nginx", Tag: "1"},
		"docker.io/nginx":              {Host: "docker.io", Repository: "library/nginx", Tag: "latest"},
		"index.docker.io/nginx":        {Host: "docker.io", Repository: "library/nginx", Tag: "latest"},
		"registry-1.docker.io/org/app": {Host: "docker.io", Repository: "org/app", Tag: "latest"},
		"bitnami/redis:7":              {Host: "docker.io", Repository: "bitnami/redis", Tag: "7"},
		"ghcr.io/org/app@" + digest:    {Host: "ghcr.io", Repository: "org/app", Digest: digest},
		"GHCR.io/org/app:v2":           {Host: "ghcr.io", Repository: "org/app", Tag: "v2"},
		"registry.example:5000/app:v1": {Host: "registry.example:5000", Repository: "app", Tag: "v1"},
		"localhost:5000/app":           {Host: "localhost:5000", Repository: "app", Tag: "latest"},
		"localhost/app":                {Host: "localhost", Repository: "app", Tag: "latest"},
	} {
		got, err := ParseReference(ref)
		if err != nil || got != want {
			t.Errorf("ParseReference(%q) = %+v, %v; want %+v", ref, got, err, want)
		}
	}
}

func TestParseReferenceRefusesWhatTheGrammarRefuses(t *testing.T) {
	for _, ref := range []string{"", "nginx:", "nginx@sha256:short", "../etc", "a//b", "a/../b", "https://ghcr.io/app", "nginx:1@sha256:" + strings.Repeat("a", 64), strings.Repeat("a", 513), "sha256:" + strings.Repeat("a", 64)} {
		if got, err := ParseReference(ref); err == nil {
			t.Errorf("ParseReference(%q) = %+v, want an error", ref, got)
		}
	}
}
