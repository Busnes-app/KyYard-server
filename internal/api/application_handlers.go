package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/applications"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/google/uuid"
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
	if endpoint := r.URL.Query().Get("endpoint"); endpoint != "" && !s.runtimeGate(w, r, a, endpoint, dockerRoute) {
		return
	}
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
	if input.EndpointID != "" && !s.runtimeGate(w, r, a, input.EndpointID, dockerRoute) {
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
	// A Kubernetes mapping names its endpoint; a Docker one its adopted instance. One that
	// cannot be read is the store's to refuse, with its own answer.
	endpoint := input.EndpointID
	if instance, err := s.store.Tenancy().ReadApplicationInstance(r.Context(), a, r.PathValue("application"), input.InstanceID); endpoint == "" && err == nil {
		endpoint = instance.EndpointID
	}
	if endpoint != "" && !s.runtimeGate(w, r, a, endpoint, applicationRoute) {
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

// maxFrameBytes is the largest deployment frame an agent with capabilities accepts.
func maxFrameBytes(capabilities []string) int {
	if slices.Contains(capabilities, protocol.CapabilityDeploymentPull) || slices.Contains(capabilities, protocol.CapabilityKubernetesDeploy) {
		return protocol.MaxDeploymentRequestBytes
	}
	return protocol.MaxDeploymentRequestBytesLegacy
}

func (s *Server) handlePlanDeployment(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input store.PlanRequest
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	// A Kubernetes plan resolves every image at the registry, so it runs as an update does.
	instance, err := s.store.Tenancy().ReadApplicationInstance(r.Context(), a, r.PathValue("application"), input.InstanceID)
	kube := err == nil && instance.Namespace != ""
	if err == nil && !s.runtimeGate(w, r, a, instance.EndpointID, applicationRoute) {
		return
	}
	if len(input.Update) > 0 || kube {
		id, err := uuid.Parse(r.PathValue("application"))
		if err != nil {
			s.tenantError(w, store.ErrInvalid)
			return
		}
		// Authorize before the guard and the slot, so a caller who may not deploy can neither see
		// nor hold either.
		if err := s.store.Tenancy().CheckImageUpdateAccess(r.Context(), a, id.String()); err != nil {
			s.tenantError(w, err)
			return
		}
		// The check's guard, before the inspections and the registry slot.
		done, ok := s.guardApplication(w, a, id.String())
		if !ok {
			return
		}
		defer done()
	}
	// The plan measures its frame against what this endpoint's agent accepts.
	pre, err := s.store.Tenancy().PreflightApplication(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, pre.EndpointID)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	input.MaxFrameBytes = maxFrameBytes(ep.Capabilities)
	// Inspect before taking a registry slot, so slow agents cannot hold the organization's slots.
	input.Inspections = s.planInspections(w, r, a, ep, pre)
	if len(input.Update) > 0 || kube {
		release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
		if !ok {
			return
		}
		defer release()
		extendRegistryDeadline(w)
	}
	d, err := s.store.Tenancy().PlanDeployment(r.Context(), a, r.PathValue("application"), input, s.resolver(), s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, d)
}
func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	list, err := s.store.Tenancy().ListDeployments(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}
func (s *Server) handleDeployment(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	d, err := s.store.Tenancy().ReadDeployment(r.Context(), a, r.PathValue("application"), r.PathValue("deployment"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, d)
}

// handleApplyDeployment records the apply before the frame leaves; a frame that cannot be
// queued fails the row rather than leaving it applying with nothing on the wire.
func (s *Server) handleApplyDeployment(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		Confirm string `json:"confirm"`
	}
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	app, id := r.PathValue("application"), r.PathValue("deployment")
	plan, err := s.store.Tenancy().ReadDeployment(r.Context(), a, app, id)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, plan.EndpointID)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	// A plan is for its endpoint's runtime: a Kubernetes plan names a namespace, a Docker plan none.
	kube := ep.Runtime == protocol.RuntimeKubernetes
	if kube != (plan.Plan.Namespace != "") {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return
	}
	deploys := protocol.CapabilityDeploymentApply
	if kube {
		deploys = protocol.CapabilityKubernetesDeploy
	}
	if !slices.Contains(ep.Capabilities, deploys) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable deployments")
		return
	}
	pulls := slices.ContainsFunc(plan.Plan.Services, func(ps store.PlannedService) bool { return ps.PullDigest != "" })
	if pulls && !kube && !slices.Contains(ep.Capabilities, protocol.CapabilityDeploymentPull) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable deployments that pull images")
		return
	}
	if kube && len(plan.Plan.Claims) > 0 && !slices.Contains(ep.Capabilities, protocol.CapabilityKubernetesClaims) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the cluster agent to apply claims")
		return
	}
	if !s.Connected(plan.EndpointID) {
		s.tenantError(w, store.ErrEndpointOffline)
		return
	}
	maxFrame := maxFrameBytes(ep.Capabilities)
	applied, req, err := s.store.Tenancy().ApplyDeployment(r.Context(), a, app, id, input.Confirm, s.config.Security.EncryptionKey, maxFrame)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.agents.deliver(plan.EndpointID, envelope(protocol.TypeDeploymentApply, req)) {
		if err := s.store.Tenancy().FailDeployment(context.WithoutCancel(r.Context()), applied.ID, "the endpoint disconnected before the deployment was sent"); err != nil {
			log.Printf("deployment %s: recording an unsent frame: %v", applied.ID, err)
		}
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The deployment could not be sent to the endpoint", "code": "deployment_not_sent"})
		return
	}
	s.writeJSON(w, http.StatusAccepted, applied)
}

// handleRemoveApplication records the removal before the frame leaves, like an apply. The
// store enforces application.destroy; the reads before it only choose the endpoint.
func (s *Server) handleRemoveApplication(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input store.RemovalBody
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	app := r.PathValue("application")
	instance, err := s.store.Tenancy().ReadApplicationInstance(r.Context(), a, app, input.InstanceID)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, instance.EndpointID)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	kube := ep.Runtime == protocol.RuntimeKubernetes
	if kube != (instance.Namespace != "") {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return
	}
	removes := protocol.CapabilityDeploymentRemove
	if kube {
		removes = protocol.CapabilityKubernetesRemove
	}
	if !slices.Contains(ep.Capabilities, removes) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable application removal")
		return
	}
	if !s.Connected(ep.ID) {
		s.tenantError(w, store.ErrEndpointOffline)
		return
	}
	removing, req, err := s.store.Tenancy().RemoveApplication(r.Context(), a, app, input)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.agents.deliver(removing.EndpointID, envelope(protocol.TypeDeploymentRemove, req)) {
		if err := s.store.Tenancy().FailDeployment(context.WithoutCancel(r.Context()), removing.ID, "the endpoint disconnected before the removal was sent"); err != nil {
			log.Printf("deployment %s: recording an unsent frame: %v", removing.ID, err)
		}
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The removal could not be sent to the endpoint", "code": "deployment_not_sent"})
		return
	}
	s.writeJSON(w, http.StatusAccepted, removing)
}
