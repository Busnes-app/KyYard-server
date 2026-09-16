package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

const (
	agentHeartbeat    = 30 * time.Second
	agentReadTimeout  = 3 * agentHeartbeat // three missed heartbeats mark the endpoint offline
	agentFrameLimit   = 4 << 20
	preAuthFrameLimit = 4096
	handshakeTimeout  = 10 * time.Second
)

// agentConn is one live socket. The registry holds at most one per endpoint.
type agentConn struct {
	endpointID string
	conn       *websocket.Conn
	send       chan protocol.Envelope
	closed     chan struct{}
	closeOnce  sync.Once
	reason     string
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

func (r *agentRegistry) add(c *agentConn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = map[string]*agentConn{}
	}
	if _, live := r.conns[c.endpointID]; live {
		return false
	}
	r.conns[c.endpointID] = c
	return true
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
	if !s.allowAttempt("agent-connect:"+s.requestIP(r), 20, time.Minute) {
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

	identity, closeReason := s.agentHandshake(ctx, conn, r)
	if identity == nil {
		conn.Close(websocket.StatusPolicyViolation, closeReason)
		return
	}
	conn.SetReadLimit(agentFrameLimit)
	c := &agentConn{endpointID: identity.Endpoint.ID, conn: conn, send: make(chan protocol.Envelope, 8), closed: make(chan struct{})}
	if !s.agents.add(c) {
		_ = s.store.Tenancy().RecordAgentConnect(ctx, &identity.Endpoint, s.requestIP(r), "denied")
		conn.Close(websocket.StatusPolicyViolation, protocol.CloseDuplicate)
		return
	}
	defer s.agents.remove(c)
	if s.stopping.Load() {
		conn.Close(websocket.StatusGoingAway, protocol.CloseShutdown)
		return
	}
	_ = s.store.Tenancy().RecordAgentConnect(ctx, &identity.Endpoint, s.requestIP(r), "success")
	if err := s.writeFrame(ctx, conn, envelope(protocol.TypeHello, protocol.Hello{State: identity.Endpoint.State, HeartbeatSeconds: int(agentHeartbeat / time.Second)})); err != nil {
		return
	}
	s.serveAgent(ctx, c, identity.Endpoint.State == "pending")
}

func (s *Server) agentHandshake(ctx context.Context, conn *websocket.Conn, r *http.Request) (*store.AgentIdentity, string) {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, protocol.CloseProtocol
	}
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
	identity, err := s.store.Tenancy().AgentIdentity(hctx, auth.EndpointID)
	if err != nil {
		// Unknown and revoked look the same to the caller; a revoked endpoint is audited.
		if errors.Is(err, store.ErrForbidden) {
			if e, readErr := s.store.Tenancy().ReadEndpointRaw(hctx, auth.EndpointID); readErr == nil {
				_ = s.store.Tenancy().RecordAgentConnect(hctx, e, s.requestIP(r), "denied")
			}
		}
		return nil, protocol.CloseRevoked
	}
	if identity.Endpoint.State == "revoked" || identity.Endpoint.State == "expired" {
		_ = s.store.Tenancy().RecordAgentConnect(hctx, &identity.Endpoint, s.requestIP(r), "denied")
		return nil, protocol.CloseRevoked
	}
	if !protocol.VerifyAuth(identity.PublicKey, auth.EndpointID, nonce, r.Host, auth.Version, auth.Signature) {
		_ = s.store.Tenancy().RecordAgentConnect(hctx, &identity.Endpoint, s.requestIP(r), "denied")
		return nil, protocol.CloseRevoked
	}
	return identity, ""
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
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	timer := time.NewTimer(agentReadTimeout)
	defer timer.Stop()
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
			_ = ts.MarkEndpointOffline(context.WithoutCancel(ctx), c.endpointID)
			return
		case e := <-c.send:
			if err := s.writeFrame(ctx, c.conn, e); err != nil {
				return
			}
		case <-timer.C:
			_ = ts.MarkEndpointOffline(context.WithoutCancel(ctx), c.endpointID)
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseTimeout)
			return
		case <-readErr:
			_ = ts.MarkEndpointOffline(context.WithoutCancel(ctx), c.endpointID)
			return
		case f := <-frames:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(agentReadTimeout)
			// Revocation between frames must not be outrun by a cached state.
			state, err := ts.EndpointState(ctx, c.endpointID)
			if err != nil || state == "revoked" {
				c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseRevoked)
				return
			}
			switch f.Type {
			case protocol.TypeHello:
				// Capabilities are recorded with M4; the frame is accepted and ignored for now.
			case protocol.TypeHeartbeat:
				if !pending {
					_ = ts.TouchEndpoint(ctx, c.endpointID)
				}
				if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeHeartbeat, nil)); err != nil {
					return
				}
			case protocol.TypeInventory:
				if pending {
					continue
				}
				var inv protocol.Inventory
				if err := json.Unmarshal(f.Payload, &inv); err != nil {
					c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
					return
				}
				accepted, err := ts.AcceptInventory(ctx, c.endpointID, inv.Generation)
				if err != nil {
					log.Printf("agent %s: inventory: %v", c.endpointID, err)
				} else if !accepted {
					log.Printf("agent %s: inventory generation %d not newer than the stored one; snapshot ignored", c.endpointID, inv.Generation)
				}
			default:
				if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]string{"code": "unsupported_type", "type": f.Type})); err != nil {
					return
				}
			}
		}
	}
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
