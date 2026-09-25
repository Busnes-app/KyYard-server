package api

import (
	"net/http"
	"strconv"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// policyView is what GET update-policy returns: the policy and its latest runs.
type policyView struct {
	*store.UpdatePolicy
	Runs []store.PolicyRun `json:"runs"`
}

func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	p, runs, err := s.store.Tenancy().ReadUpdatePolicy(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if p == nil {
		s.writeError(w, http.StatusNotFound, "This application has no update policy")
		return
	}
	s.writeJSON(w, http.StatusOK, policyView{UpdatePolicy: p, Runs: runs})
}

func (s *Server) handlePutUpdatePolicy(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input store.PolicyInput
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	p, created, err := s.store.Tenancy().PutUpdatePolicy(r.Context(), a, r.PathValue("application"), input)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, p)
}

func (s *Server) handleDeleteUpdatePolicy(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if err := s.store.Tenancy().DeleteUpdatePolicy(r.Context(), a, r.PathValue("application")); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResumeUpdatePolicy(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	p, err := s.store.Tenancy().ResumeUpdatePolicy(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, p)
}

// handlePolicyRuns lists runs newest first, 20 unless ?limit= names 1..100.
func (s *Server) handlePolicyRuns(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.tenantError(w, store.ErrInvalid)
			return
		}
		limit = n
	}
	runs, err := s.store.Tenancy().ListPolicyRuns(r.Context(), a, r.PathValue("application"), limit)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, runs)
}
