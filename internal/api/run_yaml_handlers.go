package api

import (
	"net/http"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/applications"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// handleRunYAML previews sensitive YAML under the same authority as the run forms.
// The operator submits the reviewed spec to the existing run routes separately.
func (s *Server) handleRunYAML(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	endpoint, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if a.ServiceTokenID != "" {
		_ = s.store.Tenancy().DenyService(r.Context(), a, permissions.ContainerConfigure, "bearer")
		s.tenantError(w, store.ErrForbidden)
		return
	}
	if !s.allowAttempt("yaml-preview:"+a.Principal(), 12, time.Minute) {
		s.writeError(w, 429, "Too many YAML previews")
		return
	}
	if err = s.store.Tenancy().CheckEndpointAccess(r.Context(), a, permissions.ContainerConfigure, endpoint); err != nil {
		s.tenantError(w, err)
		return
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpoint)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	var body struct {
		YAML string `json:"yaml"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if strictJSON(r, &body) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	preview, err := applications.ParseRunYAML(body.YAML, ep.Runtime)
	if err != nil {
		if diagnostic, ok := err.(*applications.Diagnostic); ok {
			s.writeJSON(w, 422, map[string]any{"code": "yaml_invalid", "diagnostic": diagnostic})
			return
		}
		s.tenantError(w, store.ErrInvalid)
		return
	}
	for _, workload := range preview.Workloads {
		granted := false
		for _, ns := range ep.DeployNamespaces {
			if ns == workload.Target.Namespace {
				granted = true
			}
		}
		if !granted {
			s.tenantError(w, store.ErrNamespaceNotGranted)
			return
		}
	}
	s.writeJSON(w, 200, preview)
}
