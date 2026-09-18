package api

import (
	"context"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"net/http"
	"time"
)

// BeginShutdown makes readiness fail before the listener drains.
func (s *Server) BeginShutdown() {
	s.stopping.Store(true)
	s.execs.closeAll()
	s.inspections.closeAgent(nil)
	// Agent sockets are hijacked connections http.Server.Shutdown does not know about.
	s.agents.closeAll(protocol.CloseShutdown)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	status, state := http.StatusOK, "ok"
	if r.URL.Path == "/health/ready" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.stopping.Load() || s.store.Ping(ctx) != nil {
			status, state = http.StatusServiceUnavailable, "unavailable"
		}
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	s.writeJSON(w, status, map[string]string{"status": state})
}
