package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/google/uuid"
)

type pendingInspection struct {
	id, actor, organization string
	agent                   *agentConn
	result                  chan protocol.InspectionResult
	done                    chan struct{}
	once                    sync.Once
	delivered               bool
}
type inspectionRegistry struct {
	mu      sync.Mutex
	pending map[string]*pendingInspection
}

func (r *inspectionRegistry) open(agent *agentConn, actor, org string) *pendingInspection {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = map[string]*pendingInspection{}
	}
	if len(r.pending) >= 64 {
		return nil
	}
	endpointCount, orgCount := 0, 0
	for _, p := range r.pending {
		if p.organization == org {
			orgCount++
		}
		if p.agent.endpointID == agent.endpointID {
			endpointCount++
			if p.actor == actor {
				return nil
			}
		}
	}
	if endpointCount >= protocol.MaxInspectionsPerEndpoint || orgCount >= 16 {
		return nil
	}
	p := &pendingInspection{id: uuid.NewString(), actor: actor, organization: org, agent: agent, result: make(chan protocol.InspectionResult, 1), done: make(chan struct{})}
	r.pending[p.id] = p
	return p
}
func (r *inspectionRegistry) release(p *pendingInspection) {
	r.mu.Lock()
	delete(r.pending, p.id)
	r.mu.Unlock()
}
func (r *inspectionRegistry) closeAgent(c *agentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.pending {
		if c == nil || p.agent == c {
			p.once.Do(func() { close(p.done) })
		}
	}
}
func (r *inspectionRegistry) deliver(c *agentConn, result protocol.InspectionResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pending[result.Request]
	if p == nil || p.agent != c || p.delivered {
		return
	}
	p.delivered = true
	p.result <- result
}
func (s *Server) inspectionAgentCurrent(c *agentConn) bool {
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	if s.agents.conns[c.endpointID] != c {
		return false
	}
	select {
	case <-c.closed:
		return false
	default:
		return true
	}
}
func (s *Server) inspectionAllowed(r *http.Request, a store.TenantAccess, endpoint string) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	if _, _, err := s.sessions.AuthenticateRequest(r.WithContext(ctx)); err != nil {
		return false
	}
	return s.store.Tenancy().StillAllowed(ctx, a, permissions.EndpointRead, endpoint) == nil
}

// Why an inspection ended without a result; handleContainerInspection maps each to one status
// and a plan reads every one as no inspection.
var (
	errInspectionCapacity    = errors.New("inspection capacity reached")
	errInspectionStopping    = errors.New("server shutting down")
	errInspectionForbidden   = errors.New("inspection access changed")
	errInspectionSend        = errors.New("inspection could not be sent")
	errInspectionTimeout     = errors.New("inspection did not complete")
	errInspectionGone        = errors.New("agent disconnected")
	errInspectionBusy        = errors.New("agent inspection capacity reached")
	errInspectionUnavailable = errors.New("runtime inspection unavailable")
	errInspectionInvalid     = errors.New("invalid inspection response")
)

// inspect asks agent for one validated observation of target on behalf of actor. It holds an
// admission slot throughout, expires the grant at the earlier of ctx's deadline and
// InspectionLifetime, re-checks the agent and allowed every second and on the answer, and
// cancels the grant on the agent only when it gave up before an answer. health is whether the
// endpoint advertises container.inspect.health, which decides what a valid answer carries.
func (s *Server) inspect(ctx context.Context, agent *agentConn, actor, org string, target protocol.InspectionTarget, health bool, allowed func() bool) (protocol.ContainerInspection, error) {
	var none protocol.ContainerInspection
	p := s.inspections.open(agent, actor, org)
	if p == nil {
		return none, errInspectionCapacity
	}
	defer s.inspections.release(p)
	if s.stopping.Load() {
		return none, errInspectionStopping
	}
	expires := time.Now().UTC().Add(protocol.InspectionLifetime)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expires) {
		expires = deadline.UTC()
	}
	ctx, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	if !s.inspectionAgentCurrent(agent) || !allowed() {
		return none, errInspectionForbidden
	}
	grant := protocol.InspectionOpen{Request: p.id, Endpoint: agent.endpointID, Actor: actor, Connection: agent.nonce, Expires: expires, Target: target}
	select {
	case agent.send <- envelope(protocol.TypeInspectionOpen, grant):
	default:
		return none, errInspectionSend
	}
	answered := false
	defer func() {
		if answered {
			return
		}
		select {
		case agent.send <- envelope(protocol.TypeInspectionCancel, protocol.InspectionCancel{Request: p.id}):
		default:
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return none, errInspectionTimeout
		case <-p.done:
			return none, errInspectionGone
		case <-agent.closed:
			return none, errInspectionGone
		case <-ticker.C:
			if !s.inspectionAgentCurrent(agent) {
				return none, errInspectionGone
			}
			if !allowed() {
				return none, errInspectionForbidden
			}
		case reply := <-p.result:
			answered = true
			if !allowed() {
				return none, errInspectionForbidden
			}
			switch reply.Status {
			case "busy":
				return none, errInspectionBusy
			case "unavailable":
				return none, errInspectionUnavailable
			case "ok":
				if reply.Result != nil && reply.Result.Validate(target, time.Now(), health) == nil {
					return *reply.Result, nil
				}
			}
			return none, errInspectionInvalid
		}
	}
}

func (s *Server) handleContainerInspection(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	endpoint, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.allowAttempt("inspection:"+a.ActorID, 30, time.Minute) {
		s.writeError(w, 429, "Too many inspection requests")
		return
	}
	// Inspection's permission is endpoint.read, which dockerOnly's endpoint read checks.
	if !s.dockerOnly(w, r, a, endpoint) {
		return
	}
	target, err := s.store.Tenancy().ReadInspectionTarget(r.Context(), a, endpoint, r.PathValue("container"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpoint)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspect) {
		s.writeError(w, 501, "Upgrade the agent to enable container inspection")
		return
	}
	s.agents.mu.Lock()
	agent := s.agents.conns[endpoint]
	s.agents.mu.Unlock()
	if agent == nil {
		s.tenantError(w, store.ErrEndpointOffline)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(protocol.InspectionLifetime + 2*time.Second))
	result, err := s.inspect(r.Context(), agent, a.ActorID, a.OrganizationID, target, slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspectHealth), func() bool { return s.inspectionAllowed(r, a, endpoint) })
	switch err {
	case nil, errInspectionBusy, errInspectionUnavailable, errInspectionInvalid:
		// The agent answered: the answer counts only for the target still recorded.
		fresh, err := s.store.Tenancy().ReadInspectionTarget(r.Context(), a, endpoint, target.ContainerID)
		if err != nil {
			s.tenantError(w, err)
			return
		}
		if fresh != target || !s.inspectionAgentCurrent(agent) {
			s.writeError(w, 409, "Inspection target changed; refresh before retrying")
			return
		}
	}
	switch err {
	case nil:
		s.writeJSON(w, 200, result)
	case errInspectionCapacity:
		s.writeError(w, 429, "Inspection capacity reached")
	case errInspectionStopping:
		s.writeError(w, 503, "Server shutting down")
	case errInspectionForbidden:
		s.writeError(w, 403, "Inspection access changed")
	case errInspectionSend:
		s.writeError(w, 503, "Inspection unavailable")
	case errInspectionTimeout:
		s.writeError(w, 504, "Inspection did not complete")
	case errInspectionGone:
		s.writeError(w, 409, "The agent disconnected; refresh before retrying")
	case errInspectionBusy:
		s.writeError(w, 429, "Agent inspection capacity reached")
	case errInspectionUnavailable:
		s.writeError(w, 409, "Runtime inspection unavailable; refresh before retrying")
	default:
		s.writeError(w, 502, "Invalid inspection response")
	}
}
