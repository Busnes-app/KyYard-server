package api

import (
	"strings"
	"testing"
	"time"
)

// One automated run per endpoint and two server-wide; a policy never runs twice at once. An
// application nobody adopted has no endpoint to hold.
func TestPolicyAdmission(t *testing.T) {
	var p policyScheduler
	if !p.admit("p1", "e1") {
		t.Fatal("first run refused")
	}
	if p.admit("p2", "e1") {
		t.Fatal("a second run on one endpoint")
	}
	if p.admit("p1", "e2") {
		t.Fatal("one policy admitted twice")
	}
	if !p.admit("p3", "e2") {
		t.Fatal("second endpoint refused")
	}
	if p.admit("p4", "e3") {
		t.Fatal("a third run server-wide")
	}
	p.release("p1")
	if !p.admit("p4", "e3") || !p.running("p4") || p.running("p1") {
		t.Fatal("a released slot was not reusable")
	}
	p.release("p3")
	p.release("p4")
	if !p.admit("p5", "") || !p.admit("p6", "") {
		t.Fatal("unadopted applications held each other")
	}
}

// A run waits for a registry slot, up to its bound.
func TestWaitRegistrySlot(t *testing.T) {
	s := &Server{registrySlots: make(chan struct{}, registrySlotsTotal)}
	first, _ := s.takeRegistrySlot("org")
	second, _ := s.takeRegistrySlot("org")
	start := time.Now()
	if _, ok := s.waitRegistrySlot("org", 50*time.Millisecond); ok || time.Since(start) < 50*time.Millisecond {
		t.Fatal("a slot past the organization's quota, or no wait")
	}
	go func() { time.Sleep(20 * time.Millisecond); first() }()
	release, ok := s.waitRegistrySlot("org", 5*time.Second)
	if !ok {
		t.Fatal("a freed slot was not taken")
	}
	release()
	second()
	if len(s.registryHeld) != 0 || len(s.registrySlots) != 0 {
		t.Fatalf("slots left held: %v %d", s.registryHeld, len(s.registrySlots))
	}
}

// A blocked run's detail holds whole codes within the column.
func TestJoinCodes(t *testing.T) {
	if got := joinCodes([]string{"a", "b"}, 255); got != "a,b" {
		t.Fatal(got)
	}
	long := []string{strings.Repeat("x", 200), strings.Repeat("y", 60), "z"}
	if got := joinCodes(long, 255); got != strings.Repeat("x", 200) {
		t.Fatalf("%d bytes", len(got))
	}
}
