package api

import (
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"testing"
)

func TestInspectionRegistryAdmissionAndConnectionIsolation(t *testing.T) {
	var r inspectionRegistry
	c := &agentConn{endpointID: "host"}
	first := r.open(c, "actor", "org")
	if first == nil {
		t.Fatal("not admitted")
	}
	if r.open(&agentConn{endpointID: "host"}, "actor", "org") != nil {
		t.Fatal("reconnect bypassed actor bound")
	}
	second := r.open(c, "other", "org")
	if second == nil || r.open(c, "third", "org") != nil {
		t.Fatal("endpoint bound")
	}
	reply := protocol.InspectionResult{Request: first.id, Status: "unavailable"}
	r.deliver(&agentConn{endpointID: "host"}, reply)
	if len(first.result) != 0 {
		t.Fatal("successor supplied old response")
	}
	r.deliver(c, reply)
	r.deliver(c, reply)
	if len(first.result) != 1 {
		t.Fatal("duplicate result accepted")
	}
	r.closeAgent(&agentConn{endpointID: "host"})
	select {
	case <-first.done:
		t.Fatal("successor closed old request")
	default:
	}
	r.closeAgent(c)
	select {
	case <-first.done:
	default:
		t.Fatal("disconnect did not close request")
	}
	r.release(first)
	r.release(second)
	if r.open(c, "actor", "org") == nil {
		t.Fatal("admission not released")
	}
}
