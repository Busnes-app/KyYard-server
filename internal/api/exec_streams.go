package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/coder/websocket"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

var errExecEnded = errors.New("terminal attachment ended; process state may be unknown")

// The registry owns admission across agent reconnects. Terminal data is never
// dropped and continued: overflow ends the attachment instead of corrupting it.
type browserExec struct {
	id, actor, organization string
	agent                   *agentConn
	frames                  chan protocol.Envelope
	cancel                  context.CancelFunc
}
type execRegistry struct {
	mu      sync.Mutex
	streams map[string]*browserExec
}

func (r *execRegistry) open(c *agentConn, actor, organization string, cancel context.CancelFunc) *browserExec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streams == nil {
		r.streams = make(map[string]*browserExec)
	}
	if len(r.streams) >= 128 {
		return nil
	}
	count := 0
	for _, s := range r.streams {
		if s.agent.endpointID == c.endpointID {
			count++
			if s.actor == actor {
				return nil
			}
		}
	}
	if count >= protocol.MaxExecStreamsPerEndpoint {
		return nil
	}
	s := &browserExec{id: uuid.NewString(), actor: actor, organization: organization, agent: c, frames: make(chan protocol.Envelope, 8), cancel: cancel}
	r.streams[s.id] = s
	return s
}
func (r *execRegistry) release(s *browserExec) {
	r.mu.Lock()
	delete(r.streams, s.id)
	r.mu.Unlock()
	s.cancel()
}
func (r *execRegistry) closeActor(actor string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.streams {
		if s.actor == actor {
			s.cancel()
		}
	}
}
func (r *execRegistry) closeAgent(c *agentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.streams {
		if s.agent == c {
			s.cancel()
		}
	}
}
func (r *execRegistry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.streams {
		s.cancel()
	}
}
func (r *execRegistry) deliver(c *agentConn, id string, f protocol.Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.streams[id]
	if s == nil || s.agent != c {
		return
	}
	select {
	case s.frames <- f:
	default:
		s.cancel()
	}
}

// Never redirect an old stream onto a newly authenticated socket. If cancellation
// cannot be queued, close that exact connection so its runtime attachments end.
func (s *browserExec) send(f protocol.Envelope) bool {
	select {
	case <-s.agent.closed:
		return false
	default:
	}
	// Send on this exact socket, in handler order. The general notification queue
	// can wait behind a store operation; revocation must bypass that queue. A
	// cancelled write closes the socket, so a stalled peer cannot retain access.
	if s.agent.conn == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	raw, err := json.Marshal(f)
	if err != nil {
		return false
	}
	return s.agent.conn.Write(ctx, websocket.MessageText, raw) == nil
}
func (s *browserExec) stopAgent() {
	if !s.send(envelope(protocol.TypeExecCancel, protocol.ExecStream{Stream: s.id})) {
		s.agent.close(protocol.CloseProtocol)
		if s.agent.conn != nil {
			_ = s.agent.conn.CloseNow()
		}
	}
}
