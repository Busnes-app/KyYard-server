package api

import (
	"net/http/httptest"
	"testing"
)

// One organization stops at its quota; the pool holds four busy organizations and refuses a
// fifth until a slot comes back.
func TestRegistrySlots(t *testing.T) {
	s := &Server{registrySlots: make(chan struct{}, registrySlotsTotal)}
	take := func(org string) func() {
		t.Helper()
		release, ok := s.acquireRegistrySlot(httptest.NewRecorder(), org)
		if !ok {
			t.Fatalf("%s refused under its quota", org)
		}
		return release
	}
	refused := func(org string) {
		t.Helper()
		w := httptest.NewRecorder()
		if _, ok := s.acquireRegistrySlot(w, org); ok || w.Code != 429 || w.Header().Get("Retry-After") != "5" {
			t.Fatalf("%s admitted past a limit: %d", org, w.Code)
		}
	}
	var releases []func()
	for _, org := range []string{"a", "b", "c", "d"} {
		releases = append(releases, take(org), take(org))
		refused(org)
	}
	refused("e") // the pool is full
	releases[0]()
	refused("b")            // b is still at its quota
	releases[0] = take("e") // a's slot went back to the pool
	for _, release := range releases {
		release()
	}
	if len(s.registryHeld) != 0 || len(s.registrySlots) != 0 {
		t.Fatalf("slots left held: %v %d", s.registryHeld, len(s.registrySlots))
	}
}
