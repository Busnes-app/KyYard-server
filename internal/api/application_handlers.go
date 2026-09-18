package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Busnes-app/kyyard-server/internal/applications"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func (s *Server) handleImportApplication(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		Name    string `json:"name"`
		Compose string `json:"compose"`
	}
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	// Check admission before parsing source. Import rechecks transactionally.
	if _, err := s.store.Tenancy().ReadEnvironment(r.Context(), a); err != nil {
		s.tenantError(w, err)
		return
	}
	imported, err := applications.ParseCompose(input.Compose)
	if err != nil {
		var diagnostic *applications.Diagnostic
		if errors.As(err, &diagnostic) {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Compose import refused", "diagnostic": diagnostic})
			return
		}
		s.tenantError(w, err)
		return
	}
	app, err := s.store.Tenancy().ImportApplication(r.Context(), a, input.Name, imported.Spec, imported.Values, s.config.Security.EncryptionKey)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, app)
}
func (s *Server) handleApplications(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	offset, limit, err := tenantPage(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	rows, err := s.store.Tenancy().ListApplications(r.Context(), a, offset, limit)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}
func (s *Server) handleApplicationRevision(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	number, err := strconv.Atoi(r.PathValue("revision"))
	if err != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	revision, err := s.store.Tenancy().ReadApplicationRevision(r.Context(), a, r.PathValue("application"), number)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, revision)
}
func (s *Server) handleDiscardApplication(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		ExpectedRevision int `json:"expected_revision"`
	}
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().DiscardApplication(r.Context(), a, r.PathValue("application"), input.ExpectedRevision); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
