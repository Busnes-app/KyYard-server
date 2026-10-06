package api

import (
	"regexp"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/registry"
)

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func validDigest(s string) bool { return sha256Digest.MatchString(s) }

// trackedReference splits a workload image into the tag it tracks and the digest it is pinned
// at. registry.ParseReference refuses a tag beside a digest, so the pin is cut off first; a
// digest with no tag tracks nothing.
func trackedReference(image string) (name string, ref registry.Reference, pinned string, ok bool) {
	name, pinned, _ = strings.Cut(image, "@")
	if pinned != "" && !strings.Contains(name[strings.LastIndex(name, "/")+1:], ":") {
		return "", registry.Reference{}, pinned, false
	}
	ref, err := registry.ParseReference(name)
	if err != nil {
		return "", registry.Reference{}, pinned, false
	}
	return name, ref, pinned, true
}

// runningDigest reads a pod's reported image ID (containerd "repo@sha256:…", cri-dockerd
// "docker-pullable://repo@sha256:…") as the digest of ref's repository, or "".
func runningDigest(imageID string, ref registry.Reference) string {
	repo, digest, ok := strings.Cut(strings.TrimPrefix(imageID, "docker-pullable://"), "@")
	if !ok || !validDigest(digest) {
		return ""
	}
	parsed, err := registry.ParseReference(repo)
	if err != nil || parsed.Host != ref.Host || parsed.Repository != ref.Repository {
		return ""
	}
	return digest
}
