package api

import (
	"context"
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
	p := s.inspections.open(agent, a.ActorID, a.OrganizationID)
	if p == nil {
		s.writeError(w, 429, "Inspection capacity reached")
		return
	}
	defer s.inspections.release(p)
	if s.stopping.Load() {
		s.writeError(w, 503, "Server shutting down")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), protocol.InspectionLifetime)
	defer cancel()
	r = r.WithContext(ctx)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(protocol.InspectionLifetime + 2*time.Second))
	if !s.inspectionAgentCurrent(agent) || !s.inspectionAllowed(r, a, endpoint) {
		s.writeError(w, 403, "Inspection access changed")
		return
	}
	grant := protocol.InspectionOpen{Request: p.id, Endpoint: endpoint, Actor: a.ActorID, Connection: agent.nonce, Expires: time.Now().UTC().Add(protocol.InspectionLifetime), Target: target}
	select {
	case agent.send <- envelope(protocol.TypeInspectionOpen, grant):
	default:
		s.writeError(w, 503, "Inspection unavailable")
		return
	}
	defer func() {
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
			s.writeError(w, 504, "Inspection did not complete")
			return
		case <-p.done:
			s.writeError(w, 409, "The agent disconnected; refresh before retrying")
			return
		case <-agent.closed:
			s.writeError(w, 409, "The agent disconnected; refresh before retrying")
			return
		case <-ticker.C:
			if !s.inspectionAgentCurrent(agent) {
				s.writeError(w, 409, "The agent disconnected; refresh before retrying")
				return
			}
			if !s.inspectionAllowed(r, a, endpoint) {
				s.writeError(w, 403, "Inspection access changed")
				return
			}
		case reply := <-p.result:
			if !s.inspectionAllowed(r, a, endpoint) {
				s.writeError(w, 403, "Inspection access changed")
				return
			}
			fresh, err := s.store.Tenancy().ReadInspectionTarget(ctx, a, endpoint, target.ContainerID)
			if err != nil {
				s.tenantError(w, err)
				return
			}
			if fresh != target || !s.inspectionAgentCurrent(agent) {
				s.writeError(w, 409, "Inspection target changed; refresh before retrying")
				return
			}
			switch reply.Status {
			case "busy":
				s.writeError(w, 429, "Agent inspection capacity reached")
			case "unavailable":
				s.writeError(w, 409, "Runtime inspection unavailable; refresh before retrying")
			case "ok":
				if reply.Result == nil || reply.Result.Validate(target, time.Now()) != nil {
					s.writeError(w, 502, "Invalid inspection response")
					return
				}
				s.writeJSON(w, 200, reply.Result)
			default:
				s.writeError(w, 502, "Invalid inspection response")
			}
			return
		}
	}
}
