// Package registry resolves image references against container registries. It must not
// import internal/store: the store imports it.
package registry

import (
	"errors"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// DockerHub is the canonical host for references that name none.
const DockerHub = "docker.io"

var ErrInvalidReference = errors.New("invalid image reference")

// Reference is an image reference split into what a registry request needs.
type Reference struct {
	Host, Repository, Tag, Digest string
}

// Credential authenticates to one registry. Secret is held only for the call.
type Credential struct {
	Username, Secret string
}

// CanonicalHost lowercases a registry host and folds Docker Hub's aliases into docker.io, so a
// reference and a configured registry name the same host the same way.
func CanonicalHost(host string) string {
	host = strings.ToLower(host)
	if host == "index.docker.io" || host == "registry-1.docker.io" {
		return DockerHub
	}
	return host
}

// ParseReference applies Docker's defaults: no host is docker.io, a one-component Docker Hub
// repository is under library/, and a reference with neither tag nor digest means latest.
func ParseReference(ref string) (Reference, error) {
	// A bare image ID names no registry.
	if !protocol.ValidImageReference(ref) || strings.HasPrefix(ref, "sha256:") {
		return Reference{}, ErrInvalidReference
	}
	name, pin := protocol.SplitImageReference(ref)
	r := Reference{Host: DockerHub, Repository: name}
	if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Host, r.Repository = CanonicalHost(first), rest
	}
	// The protocol grammar reads "nginx:1@sha256:..." as a host port; a tag beside a digest
	// is ambiguous, so refuse it.
	if strings.Contains(r.Repository, ":") {
		return Reference{}, ErrInvalidReference
	}
	if r.Host == DockerHub && !strings.Contains(r.Repository, "/") {
		r.Repository = "library/" + r.Repository
	}
	switch {
	case strings.HasPrefix(pin, "sha256:"):
		r.Digest = pin
	case pin != "":
		r.Tag = pin
	default:
		r.Tag = "latest"
	}
	return r, nil
}
