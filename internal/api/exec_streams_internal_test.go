package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
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
		if registry.open(&agentConn{endpointID: fmt.Sprint(i)}, "actor", "org", func() {}) == nil {
			t.Fatal("early refusal")
		}
	}
	if registry.open(&agentConn{endpointID: "extra"}, "actor", "org", func() {}) != nil {
		t.Fatal("unbounded connections")
	}
}
