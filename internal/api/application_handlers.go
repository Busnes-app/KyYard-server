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
	imported := s.parseApplicationCompose(w, r, a, input.Compose)
	if imported == nil {
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

func (s *Server) handleAdoptionPreview(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	p, err := s.store.Tenancy().PreviewApplicationAdoption(r.Context(), a, r.PathValue("application"), r.URL.Query().Get("endpoint"), r.URL.Query().Get("project"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, p)
}
func (s *Server) handleAdoption(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input store.AdoptionRequest
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	instance, err := s.store.Tenancy().AdoptApplication(r.Context(), a, r.PathValue("application"), input)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, instance)
}
func (s *Server) handleReleaseApplication(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		InstanceID string `json:"instance_id"`
		Confirm    string `json:"confirm"`
	}
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().ReleaseApplication(r.Context(), a, r.PathValue("application"), input.InstanceID, input.Confirm); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleApplicationInstances(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	endpoint := r.PathValue("endpoint")
	if endpoint != "" {
		e, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpoint)
		if err != nil {
			s.tenantError(w, err)
			return
		}
		a.EnvironmentID = e.EnvironmentID
	}
	rows, err := s.store.Tenancy().ListApplicationInstances(r.Context(), a, endpoint)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleApplicationComparison(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	comparison, err := s.store.Tenancy().CompareApplication(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, comparison)
}

func (s *Server) parseApplicationCompose(w http.ResponseWriter, r *http.Request, a store.TenantAccess, source string) *applications.Import {
	// Check admission before parsing source. Import rechecks transactionally.
	if _, err := s.store.Tenancy().ReadEnvironment(r.Context(), a); err != nil {
		s.tenantError(w, err)
		return nil
	}
	imported, err := applications.ParseCompose(source)
	if err != nil {
		var diagnostic *applications.Diagnostic
		if errors.As(err, &diagnostic) {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Compose import refused", "diagnostic": diagnostic})
			return nil
		}
		s.tenantError(w, err)
		return nil
	}
	return imported
}
func (s *Server) handleReplaceApplicationRevision(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		ExpectedRevision int    `json:"expected_revision"`
		Compose          string `json:"compose"`
	}
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	imported := s.parseApplicationCompose(w, r, a, input.Compose)
	if imported == nil {
		return
	}
	number, err := s.store.Tenancy().ReplaceApplicationRevision(r.Context(), a, r.PathValue("application"), input.ExpectedRevision, imported.Spec, imported.Values, s.config.Security.EncryptionKey)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]int{"revision": number})
}

func (s *Server) handleApplicationMapping(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	result, err := s.store.Tenancy().ReadApplicationMapping(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
func (s *Server) handleSetApplicationMapping(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input store.MappingRequest
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().SetApplicationMapping(r.Context(), a, r.PathValue("application"), input); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleApplicationPreflight(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	result, err := s.store.Tenancy().PreflightApplication(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
