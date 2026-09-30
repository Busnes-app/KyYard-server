package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
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
	result                  chan any
	done                    chan struct{}
	once                    sync.Once
	delivered               bool
}

// inspectionRegistry admits and routes one family of agent reads (inspection, configuration).
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
	p := &pendingInspection{id: uuid.NewString(), actor: actor, organization: org, agent: agent, result: make(chan any, 1), done: make(chan struct{})}
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

// deliver hands a strictly decoded InspectionResult or ConfigurationResult to its waiter.
func (r *inspectionRegistry) deliver(c *agentConn, reply any) {
	request, _ := replyHead(reply)
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pending[request]
	if p == nil || p.agent != c || p.delivered {
		return
	}
	p.delivered = true
	p.result <- reply
}

// replyHead names the request and status of an agent answer.
func replyHead(reply any) (request, status string) {
	switch r := reply.(type) {
	case protocol.InspectionResult:
		return r.Request, r.Status
	case protocol.ConfigurationResult:
		return r.Request, r.Status
	}
	return "", ""
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
	return s.stillAuthenticated(r.WithContext(ctx), a) && s.store.Tenancy().StillAllowed(ctx, a, permissions.EndpointRead, endpoint) == nil
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

// askFrames is one family of agent reads: its registry, frame names and grant lifetime.
type askFrames struct {
	registry     *inspectionRegistry
	open, cancel string
	lifetime     time.Duration
}

// inspect asks agent for one validated observation of target on behalf of actor. health is
// whether the endpoint advertises container.inspect.health, which decides what a valid answer
// carries.
func (s *Server) inspect(ctx context.Context, agent *agentConn, actor, org string, target protocol.InspectionTarget, health bool, allowed func() bool) (protocol.ContainerInspection, error) {
	answer, err := s.ask(ctx, agent, actor, org, target, askFrames{&s.inspections, protocol.TypeInspectionOpen, protocol.TypeInspectionCancel, protocol.InspectionLifetime}, allowed)
	if err != nil {
		return protocol.ContainerInspection{}, err
	}
	if reply, ok := answer.(protocol.InspectionResult); ok && reply.Result != nil && reply.Result.Validate(target, time.Now(), health) == nil {
		return *reply.Result, nil
	}
	return protocol.ContainerInspection{}, errInspectionInvalid
}

// ask sends one grant for target on behalf of actor and returns the agent's ok answer, the
// family's decoded result frame. It holds an admission slot throughout, expires the grant at
// the earlier of ctx's deadline and the family's lifetime, re-checks the agent and allowed every
// second and on the answer, and cancels the grant on the agent only when it gave up before an
// answer.
func (s *Server) ask(ctx context.Context, agent *agentConn, actor, org string, target protocol.InspectionTarget, frames askFrames, allowed func() bool) (any, error) {
	p := frames.registry.open(agent, actor, org)
	if p == nil {
		return nil, errInspectionCapacity
	}
	defer frames.registry.release(p)
	if s.stopping.Load() {
		return nil, errInspectionStopping
	}
	expires := time.Now().UTC().Add(frames.lifetime)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expires) {
		expires = deadline.UTC()
	}
	ctx, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	if !s.inspectionAgentCurrent(agent) || !allowed() {
		return nil, errInspectionForbidden
	}
	// Agents accept [a-zA-Z0-9_-] only; a service principal's "service:" becomes "service-".
	grant := protocol.InspectionOpen{Request: p.id, Endpoint: agent.endpointID, Actor: strings.Replace(actor, ":", "-", 1), Connection: agent.nonce, Expires: expires, Target: target}
	select {
	case agent.send <- envelope(frames.open, grant):
	default:
		return nil, errInspectionSend
	}
	answered := false
	defer func() {
		if answered {
			return
		}
		select {
		case agent.send <- envelope(frames.cancel, protocol.InspectionCancel{Request: p.id}):
		default:
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, errInspectionTimeout
		case <-p.done:
			return nil, errInspectionGone
		case <-agent.closed:
			return nil, errInspectionGone
		case <-ticker.C:
			if !s.inspectionAgentCurrent(agent) {
				return nil, errInspectionGone
			}
			if !allowed() {
				return nil, errInspectionForbidden
			}
		case reply := <-p.result:
			answered = true
			if !allowed() {
				return nil, errInspectionForbidden
			}
			_, status := replyHead(reply)
			switch status {
			case "busy":
				return nil, errInspectionBusy
			case "unavailable":
				return nil, errInspectionUnavailable
			case "ok":
				return reply, nil
			}
			return nil, errInspectionInvalid
		}
	}
}

func (s *Server) handleContainerInspection(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	endpoint, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.allowAttempt("inspection:"+a.Principal(), 30, time.Minute) {
		s.writeError(w, 429, "Too many inspection requests")
		return
	}
	// Inspection's permission is endpoint.read, which runtimeGate's endpoint read checks.
	if !s.runtimeGate(w, r, a, endpoint, dockerRoute) {
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
	result, err := s.inspect(r.Context(), agent, a.Principal(), a.OrganizationID, target, slices.Contains(ep.Capabilities, protocol.CapabilityContainerInspectHealth), func() bool { return s.inspectionAllowed(r, a, endpoint) })
	if s.askSettled(w, r, a, endpoint, target, agent, err, "inspection") {
		s.writeJSON(w, 200, result)
	}
}

// askSettled maps an ask's outcome to a response and reports whether the caller may publish its
// result: only when err is nil and target is still the one recorded, on the same agent. noun
// names the read in messages.
func (s *Server) askSettled(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpoint string, target protocol.InspectionTarget, agent *agentConn, err error, noun string) bool {
	Noun := strings.ToUpper(noun[:1]) + noun[1:]
	switch err {
	case nil, errInspectionBusy, errInspectionUnavailable, errInspectionInvalid:
		// The agent answered: the answer counts only for the target still recorded.
		fresh, err := s.store.Tenancy().ReadInspectionTarget(r.Context(), a, endpoint, target.ContainerID)
		if err != nil {
			s.tenantError(w, err)
			return false
		}
		if fresh != target || !s.inspectionAgentCurrent(agent) {
			s.writeError(w, 409, Noun+" target changed; refresh before retrying")
			return false
		}
	}
	return s.askStatus(w, err, noun)
}

// askStatus maps an ask's error to a response and reports whether there was none.
func (s *Server) askStatus(w http.ResponseWriter, err error, noun string) bool {
	Noun := strings.ToUpper(noun[:1]) + noun[1:]
	switch err {
	case nil:
		return true
	case errInspectionCapacity:
		s.writeError(w, 429, Noun+" capacity reached")
	case errInspectionStopping:
		s.writeError(w, 503, "Server shutting down")
	case errInspectionForbidden:
		s.writeError(w, 403, Noun+" access changed")
	case errInspectionSend:
		s.writeError(w, 503, Noun+" unavailable")
	case errInspectionTimeout:
		s.writeError(w, 504, Noun+" did not complete")
	case errInspectionGone:
		s.writeError(w, 409, "The agent disconnected; refresh before retrying")
	case errInspectionBusy:
		s.writeError(w, 429, "Agent "+noun+" capacity reached")
	case errInspectionUnavailable:
		s.writeError(w, 409, "Runtime "+noun+" unavailable; refresh before retrying")
	default:
		s.writeError(w, 502, "Invalid "+noun+" response")
	}
	return false
}
