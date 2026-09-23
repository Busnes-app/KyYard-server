package api

import (
	"net/http"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// registryBodyLimit covers the largest input: a credential of at most 4 KiB plus the other fields.
const registryBodyLimit = 8 << 10

func (s *Server) handleRegistries(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	rows, err := s.store.Tenancy().ListRegistries(r.Context(), a)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handlePutRegistry(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	r.Body = http.MaxBytesReader(w, r.Body, registryBodyLimit)
	var in store.RegistryInput
	if err := strictJSON(r, &in); err != nil {
		s.tenantError(w, err)
		return
	}
	row, err := s.store.Tenancy().PutRegistry(r.Context(), a, in, s.config.Security.EncryptionKey)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if err := s.store.Tenancy().DeleteRegistry(r.Context(), a, r.PathValue("registry")); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRegistryPolicy(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	p, err := s.store.Tenancy().ReadRegistryPolicy(r.Context(), a)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleSetRegistryPolicy(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	r.Body = http.MaxBytesReader(w, r.Body, registryBodyLimit)
	var p store.RegistryPolicy
	if err := strictJSON(r, &p); err != nil {
		s.tenantError(w, err)
		return
	}
	if err := s.store.Tenancy().SetAnonymousPull(r.Context(), a, p.AnonymousPullEnabled); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
