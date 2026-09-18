package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/coder/websocket"
)

func TestExecAdmissionIsolationAndOverflow(t *testing.T) {
	var registry execRegistry
	c := &agentConn{endpointID: "host"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := registry.open(c, "admin", "org", cancel)
	if stream == nil {
		t.Fatal("no admission")
	}
	defer registry.release(stream)
	if registry.open(&agentConn{endpointID: "host"}, "admin", "org", func() {}) != nil {
		t.Fatal("reconnect bypassed actor limit")
	}
	for i := 1; i < 4; i++ {
		s := registry.open(c, fmt.Sprint(i), "org", func() {})
		if s == nil {
			t.Fatal("endpoint limit too small")
		}
		defer registry.release(s)
	}
	if registry.open(c, "fifth", "org", func() {}) != nil {
		t.Fatal("endpoint limit bypassed")
	}
	// Neither a different endpoint nor the same identity's successor may write it.
	frame := envelope(protocol.TypeExecOutput, protocol.ExecData{Stream: stream.id, Data: []byte("payload")})
	registry.deliver(&agentConn{endpointID: "other"}, stream.id, frame)
	registry.deliver(&agentConn{endpointID: "host"}, stream.id, frame)
	if len(stream.frames) != 0 {
		t.Fatal("foreign connection wrote stream")
	}
	for i := 0; i < 8; i++ {
		registry.deliver(c, stream.id, frame)
	}
	select {
	case <-ctx.Done():
		t.Fatal("queue too small")
	default:
	}
	registry.deliver(c, stream.id, frame)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("overflow did not cancel")
	}
	if len(stream.frames) != 8 {
		t.Fatal("queue grew")
	}
}
func TestExecGlobalAdmissionBound(t *testing.T) {
	var registry execRegistry
	for i := 0; i < 128; i++ {
		if registry.open(&agentConn{endpointID: fmt.Sprint(i)}, "actor", fmt.Sprint(i/32), func() {}) == nil {
			t.Fatal("early refusal")
		}
	}
	if registry.open(&agentConn{endpointID: "extra"}, "actor", "org", func() {}) != nil {
		t.Fatal("unbounded connections")
	}
}

func TestExecGlobalAdmissionBoundOrganizationShare(t *testing.T) {
	var registry execRegistry
	for i := 0; i < 32; i++ {
		if registry.open(&agentConn{endpointID: fmt.Sprint(i)}, "actor", "a", func() {}) == nil {
			t.Fatal("early refusal")
		}
	}
	if registry.open(&agentConn{endpointID: "extra-a"}, "actor", "a", func() {}) != nil {
		t.Fatal("organization exhausted other tenants' capacity")
	}
	if registry.open(&agentConn{endpointID: "b"}, "actor", "b", func() {}) == nil {
		t.Fatal("other organization denied")
	}
}
func TestExecNormalClosePreservesBusyControlSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		accepted <- c
	}))
	defer server.Close()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	conn := <-accepted
	defer conn.CloseNow()
	// Holding the writer models another control-plane write occupying the socket.
	writer, err := conn.Writer(ctx, websocket.MessageText)
	if err != nil {
		t.Fatal(err)
	}
	agent := &agentConn{conn: conn, closed: make(chan struct{}), send: make(chan protocol.Envelope, 1)}
	stream := &browserExec{id: "terminal", agent: agent}
	stream.stopAgent()
	time.Sleep(600 * time.Millisecond)
	select {
	case <-agent.closed:
		t.Fatal("normal terminal close killed management connection")
	default:
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Read(ctx); err != nil {
		t.Fatal(err)
	}
	// A heartbeat still crosses the same socket after congestion clears.
	if err := conn.Write(ctx, websocket.MessageText, []byte("heartbeat")); err != nil {
		t.Fatal(err)
	}
	if _, raw, err := client.Read(ctx); err != nil || string(raw) != "heartbeat" {
		t.Fatalf("heartbeat: %s %v", raw, err)
	}
}
