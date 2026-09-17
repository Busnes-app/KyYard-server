package protocol

import "testing"

func TestSplitImageReference(t *testing.T) {
	for _, c := range []struct{ in, name, tag string }{
		{"nginx", "nginx", ""},
		{"nginx:1.2.3", "nginx", "1.2.3"},
		{"registry.example.com:5000/team/app", "registry.example.com:5000/team/app", ""},
		{"registry.example.com:5000/team/app:v1", "registry.example.com:5000/team/app", "v1"},
		{"ghcr.io/a/b@sha256:abc", "ghcr.io/a/b", "sha256:abc"},
	} {
		name, tag := SplitImageReference(c.in)
		if name != c.name || tag != c.tag {
			t.Fatalf("%q split to %q %q, want %q %q", c.in, name, tag, c.name, c.tag)
		}
	}
}

func TestValidImageReference(t *testing.T) {
	for _, good := range []string{
		"nginx",
		"nginx:1.2.3",
		"library/nginx:latest",
		"ghcr.io/busnes-app/kyyard:1.2.3",
		"registry.example.com:5000/team/app:v1",
		"ghcr.io/busnes-app/kyyard@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		if !ValidImageReference(good) {
			t.Fatalf("%q was rejected", good)
		}
	}
	for _, bad := range []string{
		"", "a/../../etc", "a/./b", "..", ".", "a//b", "/leading", "trailing/",
		"-leading-dash", "a/b:", "a b", "a/b?all=1", "a/b#frag",
		"nginx:" + string(make([]byte, 600)),
	} {
		if ValidImageReference(bad) {
			t.Fatalf("%q was accepted as an image reference", bad)
		}
	}
}
