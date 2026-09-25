package api

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// planInspectionBudget bounds a plan's whole live-inspection fan-out.
const planInspectionBudget = 10 * time.Second

// planInspections inspects each mapped service's container, in plan order and one at a time,
// under one budget and one inspection attempt, so the store can refuse at plan time what the
// agent would deny at apply. A failure of any kind leaves that container without an entry; an
// agent without container.inspect and container.inspect.verdict gets no request. The preflight
// here is the latest revision's, so it does not gate the fan-out: the store judges the revision
// being planned. The observations are consumed by the plan and never stored.
func (s *Server) planInspections(w http.ResponseWriter, r *http.Request, a store.TenantAccess, ep *store.Endpoint, pre *store.DeploymentPreflight) map[string]protocol.ContainerInspection {
	return s.inspectForPlan(r.Context(), a, ep, pre, func() {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(planInspectionBudget + 5*time.Second))
	}, func(ctx context.Context) bool { return s.inspectionAllowed(r.WithContext(ctx), a, ep.ID) })
}

// inspectForPlan is planInspections without a request: started runs once the fan-out is
// admitted, and allowed re-checks the caller's authority during each inspection.
func (s *Server) inspectForPlan(ctx context.Context, a store.TenantAccess, ep *store.Endpoint, pre *store.DeploymentPreflight, started func(), allowed func(context.Context) bool) map[string]protocol.ContainerInspection {
	out := map[string]protocol.ContainerInspection{}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspect) || !slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspectVerdict) || !s.allowAttempt("inspection:"+a.ActorID, 30, time.Minute) {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, planInspectionBudget)
	defer cancel()
	started()
	health := slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspectHealth)
	for _, svc := range pre.Services {
		if ctx.Err() != nil {
			break // the budget is spent: open no admission, send no expired grant
		}
		if svc.InspectionTarget == nil {
			continue
		}
		if in, err := s.observe(ctx, ep.ID, a.ActorID, a.OrganizationID, *svc.InspectionTarget, health, func() bool { return allowed(ctx) }); err == nil {
			out[svc.InspectionTarget.ContainerID] = in
		}
	}
	return out
}

// observe is one inspection of target on endpoint as actor through the plan-time primitive: the
// test hook when set, else the endpoint's current agent. Plans and health validation share it.
func (s *Server) observe(ctx context.Context, endpoint, actor, org string, target protocol.InspectionTarget, health bool, allowed func() bool) (protocol.ContainerInspection, error) {
	if s.planInspector != nil {
		return s.planInspector(ctx, target)
	}
	s.agents.mu.Lock()
	agent := s.agents.conns[endpoint]
	s.agents.mu.Unlock()
	if agent == nil {
		return protocol.ContainerInspection{}, store.ErrEndpointOffline
	}
	return s.inspect(ctx, agent, actor, org, target, health, allowed)
}
