package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
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

// errRegistryHead marks a registry failure, as opposed to a registry policy refusal.
var errRegistryHead = errors.New("registry head failed")

// headDigest resolves name's current digest under the organization's registry access: the one
// set of rules every update check and pin uses. The caller holds a registry slot.
func (s *Server) headDigest(ctx context.Context, a store.TenantAccess, name string, ref registry.Reference) (string, error) {
	access, err := s.store.Tenancy().ResolveRegistryAccess(ctx, a, permissions.ImagePull, name, s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, store.ImageCheckDeadline)
	defer cancel()
	digest, err := s.resolver().Head(ctx, ref, access.Credential, access.Registry != nil && access.Registry.AllowPrivate)
	if err == nil && !validDigest(digest) {
		err = registry.ErrUnavailable
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", errRegistryHead, err)
	}
	return digest, nil
}

// registryDetail is the closed word for a registry failure: registry text never reaches a client.
func registryDetail(err error) string {
	switch {
	case errors.Is(err, registry.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, registry.ErrNotFound):
		return "not_found"
	case errors.Is(err, registry.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, registry.ErrPrivateDestination):
		return "private_destination"
	}
	return "unavailable"
}

// pinWorkloadImages rewrites the image of each container pull names to its tag at the
// registry's current digest, one Head per reference, all inside one ImageCheckDeadline. It writes the response and reports false on refusal: an
// unknown or repeated name is invalid, a digest-only image or a registry failure is
// image_unresolved, a policy refusal is the store's.
func (s *Server) pinWorkloadImages(w http.ResponseWriter, r *http.Request, a store.TenantAccess, spec *protocol.WorkloadConfiguration, pull []string) bool {
	containers := map[string]*protocol.WorkloadContainer{}
	for _, group := range [][]protocol.WorkloadContainer{spec.Containers, spec.InitContainers} {
		for i := range group {
			containers[group[i].Name] = &group[i]
		}
	}
	targets := make([]*protocol.WorkloadContainer, 0, len(pull))
	for _, name := range pull {
		c, ok := containers[name]
		if !ok {
			s.tenantError(w, store.ErrInvalid)
			return false
		}
		delete(containers, name) // a repeated name is unknown the second time
		targets = append(targets, c)
	}
	unresolved := &store.InvalidSpecError{Blockers: []string{"image_unresolved"}}
	type tracked struct {
		name string
		ref  registry.Reference
	}
	refs := make([]tracked, len(targets))
	for i, c := range targets {
		name, ref, _, ok := trackedReference(c.Image)
		if !ok {
			s.tenantError(w, unresolved)
			return false
		}
		refs[i] = tracked{name, ref}
	}
	release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
	if !ok {
		return false
	}
	defer release()
	extendRegistryDeadline(w)
	ctx, cancel := context.WithTimeout(r.Context(), store.ImageCheckDeadline)
	defer cancel()
	digests := map[string]string{}
	for i, c := range targets {
		digest, seen := digests[refs[i].name]
		if !seen {
			var err error
			if digest, err = s.headDigest(ctx, a, refs[i].name, refs[i].ref); errors.Is(err, errRegistryHead) || (err != nil && ctx.Err() != nil) {
				s.tenantError(w, unresolved)
				return false
			} else if err != nil {
				s.tenantError(w, err)
				return false
			}
			digests[refs[i].name] = digest
		}
		c.Image = refs[i].name + "@" + digest
	}
	return true
}
