package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

const (
	claimPerIP     = 5
	claimGlobal    = 30
	claimWindow    = time.Minute
	claimBodyBytes = 1 << 10
)

// handleClaimServiceToken is unauthenticated: a six-digit code inside its 15 minutes is the
// whole proof, so the limits are what keep guessing impractical. Both are checked before the
// body is read, and every refusal is the same 403.
func (s *Server) handleClaimServiceToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip := s.requestIP(r)
	if !s.allowAttempt("service-claim:"+ip, claimPerIP, claimWindow) || !s.allowAttempt("service-claim", claimGlobal, claimWindow) {
		s.writeError(w, http.StatusTooManyRequests, "Too many pairing attempts; wait a minute")
		return
	}
	var req struct {
		PairingCode string `json:"pairing_code"`
		ServiceName string `json:"service_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, claimBodyBytes)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}
	issue, err := s.store.Tenancy().ClaimServiceToken(r.Context(), req.PairingCode, req.ServiceName, ip)
	if err != nil {
		s.writeError(w, http.StatusForbidden, "Pairing refused")
		return
	}
	s.writeJSON(w, http.StatusOK, issue)
}

func (s *Server) handleCreateServicePairing(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	p, err := s.store.Tenancy().CreateServicePairing(r.Context(), a)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": p.ID, "code": p.Code, "expires_at": p.ExpiresAt,
		"disclosure": "Shown once. Enter it in kyPulse within 15 minutes; it pairs one kyPulse to this organization."})
}

func (s *Server) handleServiceTokens(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	list, err := s.store.Tenancy().ListServiceTokens(r.Context(), a)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleRevokeServiceToken(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id := r.PathValue("token")
	if id == "" || len(id) > 64 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().RevokeServiceToken(r.Context(), a, id); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
