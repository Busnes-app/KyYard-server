package api

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/migration"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// analyzeMigration reads app's migration inputs against the cluster endpoint, inspects the
// mapped containers through the plan-time primitive (the plan's per-actor budget, results used
// once and dropped), and runs the analyzer with choices. It writes the refusal and reports false
// when the inputs cannot be read.
func (s *Server) analyzeMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess, app, endpoint, namespace string, choices store.KubernetesExtension) (*store.MigrationAnalysis, bool) {
	src, err := s.store.Tenancy().ReadMigrationSource(r.Context(), a, app, endpoint)
	if err == nil && !slices.Contains(src.Destination.Namespaces, namespace) {
		err = store.ErrNamespaceUnknown // before any inspection is spent; the store checks again
	}
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, src.EndpointID)
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	pre, err := s.store.Tenancy().PreflightApplication(r.Context(), a, app)
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	observed := s.planInspections(w, r, a, ep, pre)
	inspections := map[string]protocol.ContainerInspection{}
	for service, c := range src.Containers {
		if in, ok := observed[c.ID]; ok {
			inspections[service] = in
		}
	}
	report := migration.Analyze(migration.Input{Spec: src.Spec, Project: src.Project, Containers: src.Containers, Inspections: inspections, Volumes: src.Volumes, Choices: choices,
		Destination: migration.Destination{Namespace: namespace, Project: src.Destination.Project, StorageClasses: src.Destination.StorageClasses}})
	raw, err := json.Marshal(report)
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	return &store.MigrationAnalysis{Revision: src.Revision, Report: raw, Ready: report.Ready}, true
}

// sourceMigration reads app's open migration as its source; a destination has nothing to change.
func (s *Server) sourceMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) (*store.ApplicationMigration, bool) {
	m, err := s.store.Tenancy().ReadMigration(r.Context(), a, r.PathValue("application"))
	if err == nil && m.Role != "source" {
		err = store.ErrNotFound
	}
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	return m, true
}

func (s *Server) handleStartMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input store.MigrationStart
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	app := r.PathValue("application")
	an, ok := s.analyzeMigration(w, r, a, app, input.DestinationEndpointID, input.Namespace, store.KubernetesExtension{})
	if !ok {
		return
	}
	m, err := s.store.Tenancy().CreateMigration(r.Context(), a, app, input, *an)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, m)
}

func (s *Server) handleMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	m, err := s.store.Tenancy().ReadMigration(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, m)
}

// handleMigrationChoices stores the choices and the analysis they produce together.
func (s *Server) handleMigrationChoices(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var choices store.KubernetesExtension
	if strictJSON(r, &choices) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	s.reanalyze(w, r, a, &choices)
}

func (s *Server) handleAnalyzeMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	s.reanalyze(w, r, a, nil)
}

// reanalyze analyzes the open migration again with choices, or its stored ones when nil.
func (s *Server) reanalyze(w http.ResponseWriter, r *http.Request, a store.TenantAccess, choices *store.KubernetesExtension) {
	m, ok := s.sourceMigration(w, r, a)
	if !ok {
		return
	}
	if choices == nil {
		choices = &m.Choices
	}
	an, ok := s.analyzeMigration(w, r, a, m.ApplicationID, m.DestinationEndpointID, m.Namespace, *choices)
	if !ok {
		return
	}
	m, err := s.store.Tenancy().AnalyzeMigration(r.Context(), a, m.ApplicationID, *choices, *an)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleMigrationDestination(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	m, err := s.store.Tenancy().CreateMigrationDestination(r.Context(), a, r.PathValue("application"), s.config.Security.EncryptionKey)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, m)
}

// handleConfirmMigration records the operator's confirmation step with its note.
func (s *Server) handleConfirmMigration(step string) func(http.ResponseWriter, *http.Request, store.TenantAccess) {
	return func(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
		var input struct {
			Note string `json:"note"`
		}
		if strictJSON(r, &input) != nil {
			s.tenantError(w, store.ErrInvalid)
			return
		}
		m, err := s.store.Tenancy().ConfirmMigration(r.Context(), a, r.PathValue("application"), step, input.Note)
		if err != nil {
			s.tenantError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, m)
	}
}

// handleAbandonMigration closes the open migration and says whether a destination it created
// stays: it does, with its deployments, until someone removes it.
func (s *Server) handleAbandonMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	m, err := s.store.Tenancy().AbandonMigration(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, struct {
		*store.ApplicationMigration
		DestinationKept bool `json:"destination_kept"`
	}{m, m.DestinationApplicationID != ""})
}
