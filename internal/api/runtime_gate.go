package api

import (
	"net/http"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// routeKind is which runtimes a route serves.
type routeKind int

const (
	// dockerRoute acts on containers, images, exec, inspection, commands or adoption: Docker only.
	dockerRoute routeKind = iota
	// applicationRoute maps, plans, applies or removes an application on either runtime; the
	// store holds each instance to its endpoint's runtime.
	applicationRoute
)

// runtimeGate refuses, with 409 runtime_unsupported, a Docker route aimed at an endpoint that is
// not Docker. It runs before any capability check, so the answer names the runtime rather than
// asking for an agent upgrade that would not help. It writes the response and reports false on
// refusal.
func (s *Server) runtimeGate(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpointID string, kind routeKind) bool {
	e, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpointID)
	if err != nil {
		s.tenantError(w, err)
		return false
	}
	if kind == dockerRoute && e.Runtime != protocol.RuntimeDocker {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return false
	}
	return true
}
