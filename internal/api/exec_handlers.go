package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

// A GET only upgrades the connection. Within ten seconds the browser must send
// a CSRF-protected start message confirming the immutable inventory target. No
// runtime action occurs until live store authorization and its audit commit.
type browserExecStart struct {
	CSRF    string                `json:"csrf"`
	Spec    protocol.ExecSpec     `json:"spec"`
	Size    protocol.TerminalSize `json:"size"`
	Confirm string                `json:"confirm"`
}

func execJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return store.ErrInvalid
	}
	return nil
}
func (s *Server) execAllowed(r *http.Request, a store.TenantAccess, endpoint string) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	r = r.WithContext(ctx)
	if _, _, err := s.sessions.AuthenticateRequest(r); err != nil {
		return false
	}
	return s.store.Tenancy().StillAllowed(ctx, a, permissions.ContainerExec, endpoint) == nil
}
func (s *Server) handleContainerExec(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	// Unlike ordinary API requests, a browser WebSocket must always carry Origin.
	if r.Header.Get("Origin") == "" || !sameOrigin(r.Header.Get("Origin"), s.config.Server.AppURL) {
		s.writeError(w, http.StatusForbidden, "Terminal origin refused")
		return
	}
	endpoint, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.allowAttempt("exec:"+a.ActorID, 10, time.Minute) {
		s.writeError(w, 429, "Too many terminal attempts")
		return
	}
	if err := s.store.Tenancy().CheckExecAccess(r.Context(), a, endpoint); err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.runtimeGate(w, r, a, endpoint, dockerRoute) {
		return
	}
	s.agents.mu.Lock()
	agent := s.agents.conns[endpoint]
	s.agents.mu.Unlock()
	if agent == nil {
		s.tenantError(w, store.ErrEndpointOffline)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), protocol.ExecAbsoluteTimeout)
	defer cancel()
	stream := s.execs.open(agent, a.ActorID, a.OrganizationID, cancel)
	if stream == nil {
		s.writeError(w, 429, "Terminal capacity reached")
		return
	}
	defer s.execs.release(stream)
	if s.stopping.Load() {
		s.writeError(w, 503, "Server shutting down")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled}) // exact configured Origin checked above
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxExecFrameBytes)
	r = r.WithContext(ctx)
	firstCtx, firstCancel := context.WithTimeout(ctx, 10*time.Second)
	typ, raw, err := conn.Read(firstCtx)
	firstCancel()
	var start browserExecStart
	if err != nil || typ != websocket.MessageText || execJSON(raw, &start) != nil || start.Spec.Validate() != nil || start.Size.Validate() != nil || start.Spec.Container != r.PathValue("container") {
		return
	}
	csrfRequest := r.Clone(ctx)
	csrfRequest.Header.Set(auth.HeaderCSRF, start.CSRF)
	if !auth.ValidateCSRF(csrfRequest) || !s.execAllowed(r, a, endpoint) {
		return
	}
	a.CorrelationID = stream.id
	environment, err := s.store.Tenancy().OpenExecTarget(ctx, a, endpoint, stream.id, start.Confirm, start.Spec)
	if err != nil {
		_ = writeExecBrowser(ctx, conn, envelope(protocol.TypeExecClose, protocol.ExecClose{Stream: stream.id, Reason: "Terminal authorization or target confirmation refused; not started"}))
		return
	}
	a.EnvironmentID = environment
	began := time.Now()
	outcome := "unknown"
	var exit *int
	defer func() {
		auditCtx, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		details := fmt.Sprintf("stream=%q user=%q duration_ms=%d", stream.id, start.Spec.User, time.Since(began).Milliseconds())
		if exit != nil {
			details += fmt.Sprintf(" exit_code=%d", *exit)
		}
		if err := s.store.Audit().LogAudit(auditCtx, &store.AuditRecord{Scope: "organization", OrganizationID: a.OrganizationID, EnvironmentID: environment, CorrelationID: stream.id, UserID: a.ActorID, IPAddress: a.IPAddress, Resource: endpoint + "/" + start.Spec.Container, Action: "container.exec.close", Result: outcome, Details: details, CreatedAt: time.Now().UTC()}); err != nil {
			log.Printf("exec close audit failed for stream %s", stream.id)
		}
	}()
	// Revalidate after registering and auditing: revocation racing startup cannot
	// leave an unregistered stream that missed the event-driven cancellation.
	if ctx.Err() != nil || !s.execAllowed(r, a, endpoint) {
		return
	}
	grant := protocol.ExecOpen{Stream: stream.id, Endpoint: endpoint, Actor: a.ActorID, Connection: agent.nonce, Expires: time.Now().UTC().Add(protocol.ExecGrantLifetime), Spec: start.Spec, Size: start.Size}
	if !stream.send(envelope(protocol.TypeExecOpen, grant)) {
		return
	}
	agentEnded := false
	defer func() {
		if !agentEnded {
			stream.stopAgent()
		}
	}()
	// Revocation checks run independently of a slow browser writer. Store failures
	// fail closed within the same bounded check, including external account changes.
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-agent.closed:
				cancel()
				return
			case <-ticker.C:
				if !s.execAllowed(r, a, endpoint) {
					// A browser disconnect also cancels an in-flight store read.
					// Only a live check's refusal is an authority revocation.
					if ctx.Err() == nil {
						stream.revoke()
					}
					return
				}
			}
		}
	}()
	incoming := make(chan protocol.Envelope, 1)
	go func() {
		defer cancel()
		for {
			typ, raw, err := conn.Read(ctx)
			if err != nil || typ != websocket.MessageText {
				return
			}
			var f protocol.Envelope
			if execJSON(raw, &f) != nil || f.V != protocol.Version {
				return
			}
			select {
			case incoming <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	ready := time.NewTimer(10 * time.Second)
	defer ready.Stop()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	idle := time.NewTimer(protocol.ExecIdleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			pingCtx, done := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Ping(pingCtx)
			done()
			if err != nil {
				return
			}
		case <-ready.C:
			return
		case <-idle.C:
			return
		case f := <-incoming:
			if ctx.Err() != nil {
				return
			}
			switch f.Type {
			case protocol.TypeExecInput:
				var data protocol.ExecData
				if execJSON(f.Payload, &data) != nil || data.Stream != stream.id || len(data.Data) == 0 || len(data.Data) > protocol.MaxExecChunkBytes {
					return
				}
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(protocol.ExecIdleTimeout)
			case protocol.TypeExecResize:
				var resize protocol.ExecResize
				if execJSON(f.Payload, &resize) != nil || resize.Stream != stream.id || resize.Size.Validate() != nil {
					return
				}
			case protocol.TypeExecCancel:
				var end protocol.ExecStream
				if execJSON(f.Payload, &end) != nil || end.Stream != stream.id {
					return
				}
				return
			default:
				return
			}
			if !stream.send(f) {
				return
			}
		case f := <-stream.frames:
			agentEnded = f.Type == protocol.TypeExecClose
			if f.Type == protocol.TypeExecReady {
				ready.Stop()
			}
			if err := writeExecBrowser(ctx, conn, f); err != nil {
				return
			}
			if f.Type == protocol.TypeExecClose {
				var close protocol.ExecClose
				_ = json.Unmarshal(f.Payload, &close)
				exit = close.ExitCode
				if exit != nil {
					outcome = "success"
					if *exit != 0 {
						outcome = "failure"
					}
				}
				return
			}
		}
	}
}
func writeExecBrowser(ctx context.Context, c *websocket.Conn, f protocol.Envelope) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, raw)
}

// Only well-formed, connection-scoped frames enter browser queues. Agent error
// text is untrusted and can contain secrets; replace it with a fixed notice.
func (s *Server) handleExecFrame(c *agentConn, f protocol.Envelope) {
	var id string
	switch f.Type {
	case protocol.TypeExecReady:
		var v protocol.ExecStream
		if json.Unmarshal(f.Payload, &v) != nil {
			return
		}
		id = v.Stream
		f = envelope(f.Type, v)
	case protocol.TypeExecOutput:
		var v protocol.ExecData
		if json.Unmarshal(f.Payload, &v) != nil || len(v.Data) > protocol.MaxExecChunkBytes {
			s.execs.closeAgent(c)
			return
		}
		id = v.Stream
		f = envelope(f.Type, v)
	case protocol.TypeExecClose:
		var v protocol.ExecClose
		if json.Unmarshal(f.Payload, &v) != nil {
			return
		}
		id = v.Stream
		v.Reason = errExecEnded.Error()
		f = envelope(f.Type, v)
	}
	s.execs.deliver(c, id, f)
}
