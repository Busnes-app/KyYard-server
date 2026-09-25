package api

import (
	"net/http"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// dockerOnly refuses, with 409 runtime_unsupported, an action on containers, images,
// networks, volumes, exec, inspection or deployments aimed at an endpoint that is not Docker.
// It runs before any capability check, so the answer names the runtime rather than asking for
// an agent upgrade that would not help. It writes the response and reports false on refusal.
func (s *Server) dockerOnly(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpointID string) bool {
	e, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpointID)
	if err != nil {
		s.tenantError(w, err)
		return false
	}
	if e.Runtime != protocol.RuntimeDocker {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return false
	}
	return true
}
