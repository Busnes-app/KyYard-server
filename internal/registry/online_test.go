package registry

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"
)

// TestResolveDockerHubAlpine talks to the real Docker Hub; CI's real-Docker step sets the gate.
func TestResolveDockerHubAlpine(t *testing.T) {
	if os.Getenv("KY_TEST_REGISTRY_ONLINE") != "1" {
		t.Skip("set KY_TEST_REGISTRY_ONLINE=1 to resolve against Docker Hub")
	}
	ref, err := ParseReference("alpine:3.24")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	got, err := New(Options{}).Resolve(ctx, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(got.Digest) || len(got.Platforms) == 0 {
		t.Fatalf("got %+v", got)
	}
	t.Logf("alpine:3.24 = %s %v", got.Digest, got.Platforms)
}
