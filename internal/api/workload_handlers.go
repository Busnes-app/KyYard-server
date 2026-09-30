package api

import (
	"context"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// workloadRef reads a Deployment, StatefulSet or DaemonSet from the route: a pod has no
// configuration to read or apply.
func workloadRef(r *http.Request) (protocol.WorkloadRef, error) {
	ref, err := protocol.ParseWorkloadRef(r.PathValue("namespace") + "/" + r.PathValue("kind") + "/" + r.PathValue("name"))
	if err != nil || ref.Kind == protocol.WorkloadPod {
		return ref, store.ErrInvalid
	}
	return ref, nil
}

// workloadGate is what the configuration read, the apply and the run share, in order: no service
// token (all carry values), the shared configure budget, container.configure, a cluster endpoint
// and the route's capability (kubernetes.workloads, a run kubernetes.workloads.run). A route
// naming a workload has it parsed after the service token. It writes the response and reports
// false on refusal.
func (s *Server) workloadGate(w http.ResponseWriter, r *http.Request, a store.TenantAccess, named bool) (string, protocol.WorkloadRef, bool) {
	var ref protocol.WorkloadRef
	if a.ServiceTokenID != "" {
		_ = s.store.Tenancy().DenyService(r.Context(), a, permissions.ContainerConfigure, "bearer")
		s.tenantError(w, store.ErrForbidden)
		return "", ref, false
	}
	endpoint, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return "", ref, false
	}
	if named {
		if ref, err = workloadRef(r); err != nil {
			s.tenantError(w, err)
			return "", ref, false
		}
	}
	if !s.allowAttempt("configure:"+a.Principal(), 12, time.Minute) {
		s.writeError(w, 429, "Too many configuration requests")
		return "", ref, false
	}
	if err := s.store.Tenancy().CheckEndpointAccess(r.Context(), a, permissions.ContainerConfigure, endpoint); err != nil {
		s.tenantError(w, err)
		return "", ref, false
	}
	ep, ok := s.runtimeEndpoint(w, r, a, endpoint, kubernetesRoute)
	if !ok {
		return "", ref, false
	}
	capability, refusal := protocol.CapabilityKubernetesWorkloads, "Upgrade the cluster agent to configure workloads"
	if !named {
		capability, refusal = protocol.CapabilityKubernetesWorkloadsRun, "Upgrade the cluster agent to run workloads"
	}
	if !slices.Contains(ep.Capabilities, capability) {
		s.writeError(w, http.StatusNotImplemented, refusal)
		return "", ref, false
	}
	return endpoint, ref, true
}

// handleWorkloadConfiguration returns a workload's pod template to an organization
// administrator: literal environment values included, Secret and ConfigMap values only by
// reference. Nothing from the answer reaches audit or logs beyond the count of unsupported
// settings.
func (s *Server) handleWorkloadConfiguration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	endpoint, ref, ok := s.workloadGate(w, r, a, true)
	if !ok {
		return
	}
	if err := s.store.Tenancy().CheckWorkloadTarget(r.Context(), a, permissions.ContainerConfigure, endpoint, ref); err != nil {
		s.tenantError(w, err)
		return
	}
	s.agents.mu.Lock()
	agent := s.agents.conns[endpoint]
	s.agents.mu.Unlock()
	if agent == nil {
		s.tenantError(w, store.ErrEndpointOffline)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(protocol.ConfigurationLifetime + 2*time.Second))
	target := protocol.InspectionTarget{Workload: ref}
	answer, err := s.ask(r.Context(), agent, a.Principal(), a.OrganizationID, target, askFrames{&s.configurations, protocol.TypeConfigurationOpen, protocol.TypeConfigurationCancel, protocol.ConfigurationLifetime - 2*time.Second}, func() bool { return s.configurationAllowed(r, a, endpoint) })
	var result *protocol.WorkloadConfiguration
	if err == nil {
		err = errInspectionInvalid
		if reply, ok := answer.(protocol.ConfigurationResult); ok && reply.Workload != nil && reply.Workload.Validate(ref, time.Now()) == nil {
			result, err = reply.Workload, nil
		}
	}
	// An answer counts only from the agent still connected (askSettled's rule for containers).
	if (err == nil || err == errInspectionBusy || err == errInspectionUnavailable || err == errInspectionInvalid) && !s.inspectionAgentCurrent(agent) {
		s.writeError(w, 409, "Configuration read target changed; refresh before retrying")
		return
	}
	if !s.askStatus(w, err, "configuration read") {
		return
	}
	if result.Managed {
		s.tenantError(w, store.ErrWorkloadManaged)
		return
	}
	if err := s.store.Tenancy().RecordWorkloadConfigurationRead(r.Context(), a, endpoint, ref, len(result.Unsupported)); err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, 200, result)
}

// handleApplyWorkload applies an edited pod template read at resource_version: a workload.apply
// frame recorded as a direct command before it is sent, settled from the agent's
// deployment.result.
func (s *Server) handleApplyWorkload(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	endpoint, ref, ok := s.workloadGate(w, r, a, true)
	if !ok {
		return
	}
	var body struct {
		ResourceVersion string                         `json:"resource_version"`
		Spec            protocol.WorkloadConfiguration `json:"spec"`
		Confirm         string                         `json:"confirm"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // the frame's own bound applies in the store
	if strictJSON(r, &body) != nil || body.ResourceVersion == "" || (body.Spec.ResourceVersion != "" && body.Spec.ResourceVersion != body.ResourceVersion) {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	body.Spec.ResourceVersion = body.ResourceVersion
	s.sendWorkloadFrame(w, r, a, endpoint, s.store.Tenancy().CreateWorkloadApply, store.WorkloadApply{Target: ref, Confirm: body.Confirm, Spec: body.Spec})
}

// handleRunWorkload creates the Deployment spec.target names from the spec the operator wrote:
// a workload.apply frame with create set, recorded as a workload.run direct command before it is
// sent and settled from the agent's deployment.result.
func (s *Server) handleRunWorkload(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	endpoint, _, ok := s.workloadGate(w, r, a, false)
	if !ok {
		return
	}
	var body struct {
		Spec    protocol.WorkloadConfiguration `json:"spec"`
		Confirm string                         `json:"confirm"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // the frame's own bound applies in the store
	if strictJSON(r, &body) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	s.sendWorkloadFrame(w, r, a, endpoint, s.store.Tenancy().CreateWorkloadRun, store.WorkloadApply{Target: body.Spec.Target, Confirm: body.Confirm, Spec: body.Spec})
}

// sendWorkloadFrame records an apply or run with create and sends its frame: offline 409, unsent
// 409 deployment_not_sent (the row settled failed), else 202 with the command.
func (s *Server) sendWorkloadFrame(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpoint string,
	create func(context.Context, store.TenantAccess, string, store.WorkloadApply) (*store.Command, *protocol.WorkloadApply, error), wa store.WorkloadApply) {
	if !s.Connected(endpoint) {
		s.tenantError(w, store.ErrEndpointOffline)
		return
	}
	cmd, frame, err := create(r.Context(), a, endpoint, wa)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.agents.deliver(endpoint, envelope(protocol.TypeWorkloadApply, frame)) {
		if err := s.store.Tenancy().FailDirectCommand(context.WithoutCancel(r.Context()), endpoint, cmd.ID, "deployment_not_sent"); err != nil {
			log.Printf("command %s: recording an unsent frame: %v", cmd.ID, err)
		}
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The command could not be sent to the endpoint", "code": "deployment_not_sent"})
		return
	}
	if err := s.store.Tenancy().MarkCommandDispatched(r.Context(), cmd.ID); err != nil {
		log.Printf("command %s: marking dispatched: %v", cmd.ID, err)
	}
	s.writeJSON(w, http.StatusAccepted, cmd)
}
