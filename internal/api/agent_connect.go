package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	// frameBudget bounds one frame handler and the offline write; a frame this recent also
	// counts as proof of life when a duplicate is probed.
	frameBudget       = 10 * time.Second
	agentHeartbeat    = 30 * time.Second
	agentReadTimeout  = 3 * agentHeartbeat // three missed heartbeats mark the endpoint offline
	agentFrameLimit   = 4 << 20
	preAuthFrameLimit = 4096
	maxControlPayload = 64 << 10
	handshakeTimeout  = 10 * time.Second
)

// agentConn is one live socket. The registry holds at most one per endpoint.
type agentConn struct {
	nonce          []byte
	endpointID     string
	organizationID string
	environmentID  string
	fingerprint    string // the key that authenticated this session
	runtime        string // docker or kubernetes, fixed at enrollment
	ip             string
	conn           *websocket.Conn
	send           chan protocol.Envelope
	closed         chan struct{}
	closeOnce      sync.Once
	reason         string
	lastFrame      atomic.Int64 // unix nanoseconds of the last frame the reader delivered
	lastMetrics    time.Time    // last metrics frame handed to the store; loop goroutine only
	skewRaised     bool         // generation_rejected raised this session; loop goroutine only
}

// alive pings the socket with a short deadline; a peer that cannot answer is not a competitor.
// alive tells a dropped peer from a busy one. A frame delivered inside the handler budget
// proves the socket lived moments ago even though the reader, blocked handing that frame to
// the loop, cannot answer a ping; otherwise the ping decides.
func (c *agentConn) alive(ctx context.Context) bool {
	if c.conn == nil {
		return false
	}
	if time.Since(time.Unix(0, c.lastFrame.Load())) < frameBudget {
		return true
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.conn.Ping(pctx) == nil
}

func (c *agentConn) close(reason string) {
	c.closeOnce.Do(func() {
		c.reason = reason
		close(c.closed)
	})
}

// agentRegistry refuses a second connection for an endpoint and lets tenant handlers reach a
// live socket to close it (revocation) or notify it (approval) in the same request.
type agentRegistry struct {
	mu    sync.Mutex
	conns map[string]*agentConn
}

// add registers c, or returns the incumbent socket that already holds the endpoint.
func (r *agentRegistry) add(c *agentConn) (incumbent *agentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = map[string]*agentConn{}
	}
	if live, ok := r.conns[c.endpointID]; ok {
		return live
	}
	r.conns[c.endpointID] = c
	return nil
}

func (r *agentRegistry) remove(c *agentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[c.endpointID] == c {
		delete(r.conns, c.endpointID)
	}
}

func (r *agentRegistry) closeEndpoint(id, reason string) {
	r.mu.Lock()
	c := r.conns[id]
	r.mu.Unlock()
	if c != nil {
		c.close(reason)
	}
}

func (r *agentRegistry) notify(id string, e protocol.Envelope) {
	r.mu.Lock()
	c := r.conns[id]
	r.mu.Unlock()
	if c != nil {
		select {
		case c.send <- e:
		default:
		}
	}
}

// deliver queues a frame for one endpoint and says whether it was taken. notify may drop a
// frame the agent can do without; a command cannot be dropped quietly, because the record of it
// is already durable and would sit in flight until something abandoned it.
func (r *agentRegistry) deliver(id string, e protocol.Envelope) bool {
	r.mu.Lock()
	c := r.conns[id]
	r.mu.Unlock()
	if c == nil {
		return false
	}
	select {
	case c.send <- e:
		return true
	default:
		return false
	}
}

func (r *agentRegistry) closeAll(reason string) {
	r.mu.Lock()
	conns := make([]*agentConn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()
	for _, c := range conns {
		c.close(reason)
	}
}

// Connected reports whether an endpoint holds a live socket; tests and status screens use it.
func (s *Server) Connected(endpointID string) bool {
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	_, live := s.agents.conns[endpointID]
	return live
}

func envelope(typ string, payload any) protocol.Envelope {
	raw, _ := json.Marshal(payload)
	return protocol.Envelope{V: protocol.Version, Type: typ, Payload: raw}
}

// handleAgentConnect runs the handshake from docs/agent-protocol.md section 2, then serves the
// socket until heartbeat timeout, revocation, shutdown or the agent leaves. It never trusts the
// agent's claim about state: state comes from the store at connect and again on every frame that
// needs it.
func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	s.agentConnect(w, r, "agent-connect:"+s.requestIP(r))
}

// The listener selects the budget; request headers cannot opt into the private one.
func (s *Server) agentConnect(w http.ResponseWriter, r *http.Request, limitKey string) {
	if !s.allowAttempt(limitKey, 20, time.Minute) {
		s.writeError(w, http.StatusTooManyRequests, "Too many connection attempts")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	// Only a small auth frame is legitimate before the handshake; the full limit waits for it.
	conn.SetReadLimit(preAuthFrameLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		_ = conn.CloseNow()
		return
	}
	identity, closeReason := s.agentHandshake(ctx, conn, r, nonce)
	if identity == nil {
		conn.Close(websocket.StatusPolicyViolation, closeReason)
		return
	}
	conn.SetReadLimit(agentFrameLimit)
	c := &agentConn{nonce: nonce, endpointID: identity.Endpoint.ID, organizationID: identity.Endpoint.OrganizationID, environmentID: identity.Endpoint.EnvironmentID, fingerprint: identity.Fingerprint, runtime: identity.Endpoint.Runtime, ip: s.requestIP(r), conn: conn, send: make(chan protocol.Envelope, 8), closed: make(chan struct{})}
	if incumbent := s.agents.add(c); incumbent != nil {
		// The incumbent may be a socket the network dropped without a FIN: the agent gives up
		// after two heartbeats and redials before the server's three-heartbeat timeout. Probe
		// it; a dead incumbent is evicted, audited, and the newcomer admitted with no event.
		if !incumbent.alive(ctx) {
			incumbent.close(protocol.CloseTimeout)
			s.agents.remove(incumbent)
			// A stream was opened against the session being displaced, and the successor has
			// never heard of it: nothing will ever arrive on it again. Ending it here is what
			// keeps every socket transition closing the endpoint's readers exactly once --
			// the loser's unwind must not do it, because by then the successor's own readers
			// would go with them.
			s.logs.closeEndpointStreams(c.endpointID, "the endpoint reconnected")
			if again := s.agents.add(c); again == nil {
				_ = s.store.Tenancy().RecordAgentConnect(ctx, &identity.Endpoint, incumbent.ip, "failure", "evicted: unanswering socket displaced by "+s.requestIP(r))
				goto admitted
			}
		}
		// A second live socket is the signature of a copied identity volume: audit it, raise
		// an operator-facing event naming both parties (the refused one is usually the real
		// host, the holder is the one to doubt), and block rotation until someone looks.
		_ = s.store.Tenancy().RecordAgentConnect(ctx, &identity.Endpoint, s.requestIP(r), "denied", "")
		_ = s.store.Tenancy().RecordEndpointEvent(ctx, &identity.Endpoint, "high", "duplicate_connection", "refused "+s.requestIP(r)+"; live socket held from "+incumbent.ip)
		conn.Close(websocket.StatusPolicyViolation, protocol.CloseDuplicate)
		return
	}
admitted:
	defer s.agents.remove(c)
	defer s.execs.closeAgent(c)
	defer s.inspections.closeAgent(c)
	if s.stopping.Load() {
		conn.Close(websocket.StatusGoingAway, protocol.CloseShutdown)
		return
	}
	_ = s.store.Tenancy().RecordAgentConnect(ctx, &identity.Endpoint, s.requestIP(r), "success", "")
	if err := s.writeFrame(ctx, conn, envelope(protocol.TypeHello, protocol.Hello{State: identity.Endpoint.State, HeartbeatSeconds: int(agentHeartbeat / time.Second)})); err != nil {
		return
	}
	s.serveAgent(ctx, c, identity.Endpoint.State == "pending")
}

func (s *Server) agentHandshake(ctx context.Context, conn *websocket.Conn, r *http.Request, nonce []byte) (*store.AgentIdentity, string) {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := s.writeFrame(hctx, conn, envelope(protocol.TypeChallenge, protocol.Challenge{Nonce: nonce, InstanceFingerprint: s.instanceFingerprint(), Versions: []int{protocol.Version}})); err != nil {
		return nil, protocol.CloseProtocol
	}
	frame, err := readFrame(hctx, conn)
	if err != nil || frame.Type != protocol.TypeAuth {
		return nil, protocol.CloseProtocol
	}
	var auth protocol.Auth
	if err := json.Unmarshal(frame.Payload, &auth); err != nil || auth.EndpointID == "" || len(auth.EndpointID) > 64 {
		return nil, protocol.CloseProtocol
	}
	if auth.Version != protocol.Version {
		return nil, protocol.CloseIncompatible
	}
	if len(auth.Fingerprint) != 64 {
		return nil, protocol.CloseProtocol
	}
	identity, err := s.store.Tenancy().AgentIdentity(hctx, auth.EndpointID, auth.Fingerprint)
	if err != nil {
		if errors.Is(err, store.ErrForbidden) {
			if e, readErr := s.store.Tenancy().ReadEndpointRaw(hctx, auth.EndpointID); readErr == nil {
				_ = s.store.Tenancy().RecordAgentConnect(hctx, e, s.requestIP(r), "denied", "")
			}
		}
		return nil, agentStoreCloseReason(err)
	}
	if identity.Endpoint.State == "revoked" || identity.Endpoint.State == "expired" {
		_ = s.store.Tenancy().RecordAgentConnect(hctx, &identity.Endpoint, s.requestIP(r), "denied", "")
		return nil, protocol.CloseRevoked
	}
	if !protocol.VerifyAuth(identity.PublicKey, auth.EndpointID, nonce, r.Host, auth.Version, auth.Signature) {
		_ = s.store.Tenancy().RecordAgentConnect(hctx, &identity.Endpoint, s.requestIP(r), "denied", "")
		return nil, protocol.CloseRevoked
	}
	return identity, ""
}

// Store outages fail closed for this connection, but must not revoke the identity.
func agentStoreCloseReason(err error) string {
	switch {
	case errors.Is(err, store.ErrKeyRetired):
		return protocol.CloseKeyRetired
	case errors.Is(err, store.ErrKeyPendingReview):
		return protocol.CloseKeyPending
	case errors.Is(err, store.ErrForbidden), errors.Is(err, store.ErrNotFound):
		return protocol.CloseRevoked
	default:
		return protocol.CloseProtocol
	}
}

// serveAgent pumps frames both ways. A pending endpoint may only refresh facts and wait for
// enrollment.approved; approved, active and offline endpoints get heartbeats and inventory.
func (s *Server) serveAgent(ctx context.Context, c *agentConn, pending bool) {
	ts := s.store.Tenancy()
	frames := make(chan protocol.Envelope)
	readErr := make(chan error, 1)
	go func() {
		for {
			f, err := readFrame(ctx, c.conn)
			if err != nil {
				readErr <- err
				return
			}
			c.lastFrame.Store(time.Now().UnixNano())
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	timer := time.NewTimer(agentReadTimeout)
	defer timer.Stop()
	helloSeen := false
	for {
		select {
		case <-c.closed:
			code := websocket.StatusPolicyViolation
			if c.reason == protocol.CloseShutdown {
				code = websocket.StatusGoingAway
			}
			c.conn.Close(code, c.reason)
			if c.reason != protocol.CloseShutdown {
				return
			}
			s.markOffline(ctx, c)
			return
		case e := <-c.send:
			if err := s.writeFrame(ctx, c.conn, e); err != nil {
				return
			}
		case <-timer.C:
			s.markOffline(ctx, c)
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseTimeout)
			return
		case <-readErr:
			s.markOffline(ctx, c)
			return
		case f := <-frames:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(agentReadTimeout)
			if s.handleAgentFrame(ctx, ts, c, f, pending, &helloSeen) {
				return
			}
		}
	}
}

// markOffline ends a session: the slot is freed first so a reconnecting agent never waits on
// the writer lock and reads as its own duplicate, and the offline write is skipped when a
// successor already holds the endpoint, so a late write cannot demote a live session. The
// detached context is bounded.
func (s *Server) markOffline(ctx context.Context, c *agentConn) {
	s.agents.remove(c)
	if s.Connected(c.endpointID) {
		// A successor already holds the endpoint -- an evicted socket unwinding, which is an
		// ordinary flow -- so nothing here has been lost, and the readers attached to the
		// live session must not be told the endpoint disconnected.
		return
	}
	// A reader waiting on this endpoint is waiting on a socket that has gone. Telling it so
	// is the honest answer; leaving the request open until its own timeout is not.
	s.logs.closeEndpointStreams(c.endpointID, "the endpoint disconnected")
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), frameBudget)
	defer cancel()
	_ = s.store.Tenancy().MarkEndpointOffline(wctx, c.endpointID)
	// Anything dispatched and unanswered is now unknown. The connection ending is not
	// evidence the work did not happen, and nothing is retried on its own.
	if n, err := s.store.Tenancy().AbandonCommands(wctx, c.endpointID); err == nil && n > 0 {
		log.Printf("agent %s: %d commands left unknown when the connection ended", c.endpointID, n)
	}
	if n, err := s.store.Tenancy().AbandonDeployments(wctx, c.endpointID); err == nil && n > 0 {
		log.Printf("agent %s: %d deployments left unknown when the connection ended", c.endpointID, n)
	}
}

// handleAgentFrame applies one received frame and reports whether the session must end. A
// frame that was received is applied even if the socket drops while we work: the request
// context dies with the connection, and an inventory report or rotation offer must not be
// lost to that race, so store calls run on a detached, bounded context.
func (s *Server) handleAgentFrame(ctx context.Context, ts store.TenancyStore, c *agentConn, f protocol.Envelope, pending bool, helloSeen *bool) bool {
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), frameBudget)
	defer fcancel()
	// Revocation between frames must not be outrun by a cached state, and neither must the
	// retirement of the key that authenticated this session.
	state, err := ts.EndpointState(fctx, c.endpointID)
	if err != nil {
		c.conn.Close(websocket.StatusPolicyViolation, agentStoreCloseReason(err))
		return true
	}
	if state == "revoked" {
		c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseRevoked)
		return true
	}
	if _, err := ts.AgentIdentity(fctx, c.endpointID, c.fingerprint); err != nil {
		c.conn.Close(websocket.StatusPolicyViolation, agentStoreCloseReason(err))
		return true
	}
	if f.Type == protocol.TypeInspectionResult && len(f.Payload) > protocol.MaxInspectionFrameBytes {
		s.inspections.closeAgent(c)
		c.close(protocol.CloseProtocol)
		_ = c.conn.CloseNow()
		return true
	}
	if f.Type == protocol.TypeMetrics && len(f.Payload) > protocol.MaxMetricsBytes {
		if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]any{"code": "metrics_too_large", "limit_bytes": protocol.MaxMetricsBytes})); err != nil {
			return true
		}
		return false
	}
	if f.Type == protocol.TypeLogChunk && len(f.Payload) > protocol.MaxLogFrameBytes {
		c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
		return true
	}
	if f.Type == protocol.TypeDeploymentResult && len(f.Payload) > protocol.MaxDeploymentResultBytes {
		c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
		return true
	}
	if f.Type != protocol.TypeInventory && f.Type != protocol.TypeLogChunk && f.Type != protocol.TypeDeploymentResult && len(f.Payload) > maxControlPayload {
		c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
		return true
	}
	switch f.Type {
	case protocol.TypeHello:
		// One hello per session: a second carries nothing new and must not rewrite the set.
		if *helloSeen {
			return false
		}
		*helloSeen = true
		var hello protocol.Hello
		if protocol.UnmarshalHelloBounded(f.Payload, &hello) != nil {
			return false
		}
		// Stored capabilities gate every handler, so an agent claiming another runtime's
		// capabilities is refused outright rather than recorded.
		if !protocol.CapabilitiesFit(c.runtime, hello.Capabilities) {
			_ = s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]string{"code": "capability_mismatch", "runtime": c.runtime}))
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		if !pending {
			if err := ts.SetEndpointCapabilities(fctx, c.endpointID, hello.Capabilities); err != nil {
				log.Printf("agent %s: capabilities: %v", c.endpointID, err)
			}
		}
	case protocol.TypeRotate:
		if pending {
			return false
		}
		var rot protocol.Rotate
		if err := json.Unmarshal(f.Payload, &rot); err != nil {
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		fp, err := ts.RotateEndpointKey(fctx, c.endpointID, rot.PublicKey, rot.Signature, c.ip)
		reply := protocol.Rotated{Fingerprint: fp}
		switch {
		case errors.Is(err, store.ErrRotationPending):
			reply.Code = "rotation_pending"
		case errors.Is(err, store.ErrRotationBlocked):
			reply.Code = "rotation_blocked"
		case err != nil:
			reply.Code = "rotation_refused"
		}
		if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeRotate, reply)); err != nil {
			return true
		}
	case protocol.TypeHeartbeat:
		if !pending {
			_ = ts.TouchEndpoint(fctx, c.endpointID)
		}
		if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeHeartbeat, nil)); err != nil {
			return true
		}
	case protocol.TypeResult:
		if pending {
			return false
		}
		var res protocol.Result
		if err := json.Unmarshal(f.Payload, &res); err != nil || res.ID == "" {
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		// The first answer wins, and an answer for a command this endpoint was never given
		// changes nothing: the update is scoped to both.
		if err := ts.SettleCommand(fctx, c.endpointID, res.ID, res.Outcome, res.Detail); err != nil {
			// The identifier is server-minted, so anything else is the agent's invention and
			// none of it reaches the line: an agent does not get to write the operator's log.
			id := "an unrecognised id"
			if _, uErr := uuid.Parse(res.ID); uErr == nil {
				id = res.ID
			}
			log.Printf("agent %s: settling command %s: %v", c.endpointID, id, err)
		}
	case protocol.TypeDeploymentResult:
		if pending {
			return false
		}
		var res protocol.DeploymentResult
		if err := json.Unmarshal(f.Payload, &res); err != nil {
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		switch err := ts.SettleDeployment(fctx, c.endpointID, res); {
		case err == nil:
		case errors.Is(err, store.ErrUnreadableResult):
			// The size is already bounded. Closing would only make the agent re-send the same
			// result on reconnect, so drop it; the row stays until the deadline sweep.
			log.Printf("agent %s: dropped an unreadable deployment result", c.endpointID)
		case errors.Is(err, store.ErrNotFound):
			// A row this endpoint may not settle: nothing to report to the agent.
			log.Printf("agent %s: late deployment result ignored: %s", c.endpointID, res.Deployment)
		case errors.Is(err, store.ErrInvalid), errors.Is(err, store.ErrAdoptionChanged):
			// The host may have acted, but not as planned: record that rather than guess.
			if err := ts.RefuseDeploymentResult(fctx, c.endpointID, res.Deployment, "the host's result did not match the plan; inspect the host"); err != nil && !errors.Is(err, store.ErrNotFound) {
				log.Printf("agent %s: refusing deployment %s: store error", c.endpointID, res.Deployment)
			}
		default:
			log.Printf("agent %s: settling deployment %s: store error", c.endpointID, res.Deployment)
		}
	case protocol.TypeInspectionResult:
		var result protocol.InspectionResult
		if pending || len(f.Payload) > protocol.MaxInspectionFrameBytes || execJSON(f.Payload, &result) != nil || result.Request == "" {
			s.inspections.closeAgent(c)
			c.close(protocol.CloseProtocol)
			_ = c.conn.CloseNow()
			return true
		}
		s.inspections.deliver(c, result)
	case protocol.TypeExecReady, protocol.TypeExecOutput, protocol.TypeExecClose:
		if !pending {
			s.handleExecFrame(c, f)
		}
	case protocol.TypeLogChunk:
		if pending {
			return false
		}
		var chunk protocol.LogChunk
		if json.Unmarshal(f.Payload, &chunk) != nil {
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		// A stream this endpoint was never given addresses nothing: the lookup is scoped to
		// the endpoint, so an agent cannot write into another endpoint's reader.
		stream := s.logs.find(c.endpointID, chunk.Stream)
		if stream == nil {
			return false
		}
		if len(chunk.Data) > protocol.MaxLogChunkBytes {
			// One bad data frame ends one stream. Closing the session instead would turn an
			// agent-side framing slip into an endpoint nobody can manage: this socket carries
			// the heartbeats, the inventory and every command as well.
			stream.finish(&protocol.LogClose{Stream: chunk.Stream, Reason: "the endpoint sent an oversized chunk", Failed: true})
			return false
		}
		stream.deliver(chunk)
	case protocol.TypeLogClose:
		if pending {
			return false
		}
		var end protocol.LogClose
		if json.Unmarshal(f.Payload, &end) != nil {
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		if stream := s.logs.find(c.endpointID, end.Stream); stream != nil {
			end.Reason = protocol.CleanText(end.Reason, protocol.MaxResultDetailBytes)
			stream.finish(&end)
		}
	case protocol.TypeMetrics:
		if pending {
			return false
		}
		var m protocol.Metrics
		if err := json.Unmarshal(f.Payload, &m); err != nil || store.ValidateSamples(m) != nil {
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		// Metrics are the first telemetry dropped when storage runs short: cheapest to lose,
		// fastest to grow. The session, its heartbeats and the audit trail continue.
		if p := s.store.Pressure(); p != store.PressureNormal {
			if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]any{"code": "retention_pressure", "pressure": p.String(), "dropped": "metrics"})); err != nil {
				return true
			}
			return false
		}
		if !c.lastMetrics.IsZero() && time.Since(c.lastMetrics) < store.SampleCadence {
			return false
		}
		c.lastMetrics = time.Now()
		if err := ts.RecordSamples(fctx, c.endpointID, m); err != nil {
			if errors.Is(err, store.ErrSampleBudget) {
				if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]any{"code": "samples_budget_exhausted", "limit_rows": s.store.SampleCeiling()})); err != nil {
					return true
				}
				return false
			}
			if errors.Is(err, store.ErrInvalid) {
				// A frame the store refuses is a protocol violation: close so the socket cannot
				// become an unbounded write channel of rejected frames.
				c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
				return true
			}
			log.Printf("agent %s: metrics: %v", c.endpointID, err)
		}
	case protocol.TypeInventory:
		if pending {
			return false
		}
		// Inventory goes only when dropping metrics was not enough. Heartbeats and endpoint
		// state still flow, so the fleet stays visible while storage is recovered.
		if s.store.Pressure() == store.PressureStopped {
			if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]any{"code": "retention_pressure", "pressure": store.PressureStopped.String(), "dropped": "inventory"})); err != nil {
				return true
			}
			return false
		}
		var inv protocol.Snapshot
		if len(f.Payload) > protocol.MaxSnapshotBytes {
			// A data problem, not an availability one: say so and keep the session so
			// heartbeats continue and the endpoint stays visible.
			if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]any{"code": "snapshot_too_large", "limit_bytes": protocol.MaxSnapshotBytes})); err != nil {
				return true
			}
			return false
		}
		if err := protocol.UnmarshalSnapshotBounded(f.Payload, &inv); err != nil {
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		// A snapshot of the wrong shape is a data problem like snapshot_too_large: say so and
		// keep the session.
		if err := protocol.CheckRuntimeShape(c.runtime, &inv); err != nil {
			if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]string{"code": "snapshot_rejected", "runtime": c.runtime})); err != nil {
				return true
			}
			return false
		}
		if inv.ObservedAt.IsZero() {
			inv.ObservedAt = time.Now().UTC()
		}
		// Store the schema-conforming re-encoding, never the agent's bytes.
		protocol.Clamp(&inv)
		body := protocol.Shrink(&inv)
		accepted, err := ts.AcceptInventory(fctx, c.endpointID, inv.Generation, inv.ObservedAt, body)
		if err != nil {
			log.Printf("agent %s: inventory: %v", c.endpointID, err)
		} else if !accepted {
			if inv.Generation > uint64(time.Now().UTC().Add(store.GenerationSkew).Unix()) && !c.skewRaised {
				// Once per session; the store also keeps one unacknowledged event per kind.
				c.skewRaised = true
				if err := ts.RecordEndpointEvent(fctx, &store.Endpoint{ID: c.endpointID, OrganizationID: c.organizationID, EnvironmentID: c.environmentID}, "high", "generation_rejected", "generation is ahead of the server clock"); err != nil {
					log.Printf("agent %s: generation rejection audit: %v", c.endpointID, err)
				}
			}
			log.Printf("agent %s: inventory generation %d not accepted (not newer, or implausible); snapshot ignored", c.endpointID, inv.Generation)
			if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]any{"code": "generation_rejected", "generation": inv.Generation})); err != nil {
				return true
			}
		}
	default:
		if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]string{"code": "unsupported_type", "type": f.Type})); err != nil {
			return true
		}
	}
	return false
}

func (s *Server) writeFrame(ctx context.Context, conn *websocket.Conn, e protocol.Envelope) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, raw)
}

func readFrame(ctx context.Context, conn *websocket.Conn) (protocol.Envelope, error) {
	var e protocol.Envelope
	_, raw, err := conn.Read(ctx)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, err
	}
	if e.V != protocol.Version {
		return e, errors.New("unsupported envelope version")
	}
	return e, nil
}
