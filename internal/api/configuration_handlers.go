package api

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func (s *Server) configurationAllowed(r *http.Request, a store.TenantAccess, endpoint string) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	return s.stillAuthenticated(r.WithContext(ctx), a) && s.store.Tenancy().StillAllowed(ctx, a, permissions.ContainerConfigure, endpoint) == nil
}

// readConfiguration asks agent for target's full configuration, validated against target.
func (s *Server) readConfiguration(ctx context.Context, agent *agentConn, actor, org string, target protocol.InspectionTarget, allowed func() bool) (protocol.ContainerConfiguration, error) {
	// The agent refuses a grant past its own lifetime; leave room for clock skew.
	answer, err := s.ask(ctx, agent, actor, org, target, askFrames{&s.configurations, protocol.TypeConfigurationOpen, protocol.TypeConfigurationCancel, protocol.ConfigurationLifetime - 2*time.Second}, allowed)
	if err != nil {
		return protocol.ContainerConfiguration{}, err
	}
	// The frame handler has already checked the reply's shape (ConfigurationResult.Validate).
	if reply, ok := answer.(protocol.ConfigurationResult); ok && reply.Result != nil && reply.Result.Validate(target, time.Now()) == nil {
		return *reply.Result, nil
	}
	return protocol.ContainerConfiguration{}, errInspectionInvalid
}

// handleContainerConfiguration returns a container's full configuration, environment values
// included, to an organization administrator. Nothing from the answer reaches audit or logs
// beyond the image ID and the count of unsupported fields.
func (s *Server) handleContainerConfiguration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	// Configuration carries secrets; a service token never reads it.
	if a.ServiceTokenID != "" {
		_ = s.store.Tenancy().DenyService(r.Context(), a, permissions.ContainerConfigure, "bearer")
		s.tenantError(w, store.ErrForbidden)
		return
	}
	endpoint, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.allowAttempt("configure:"+a.Principal(), 12, time.Minute) {
		s.writeError(w, 429, "Too many configuration requests")
		return
	}
	if err := s.store.Tenancy().CheckEndpointAccess(r.Context(), a, permissions.ContainerConfigure, endpoint); err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.runtimeGate(w, r, a, endpoint, dockerRoute) {
		return
	}
	target, err := s.store.Tenancy().ReadInspectionTarget(r.Context(), a, endpoint, r.PathValue("container"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	managed, err := s.store.Tenancy().ContainerManaged(r.Context(), a, endpoint, target.ContainerID)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if managed {
		s.writeJSON(w, 409, map[string]string{"error": "This container belongs to an adopted application; change it through the application", "code": "application_managed"})
		return
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpoint)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityContainerConfigure) {
		s.writeError(w, 501, "Upgrade the agent to enable container configuration")
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
	result, err := s.readConfiguration(r.Context(), agent, a.Principal(), a.OrganizationID, target, func() bool { return s.configurationAllowed(r, a, endpoint) })
	if !s.askSettled(w, r, a, endpoint, target, agent, err, "configuration read") {
		return
	}
	if err := s.store.Tenancy().RecordConfigurationRead(r.Context(), a, endpoint, target, len(result.Unsupported)); err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, 200, result)
}
