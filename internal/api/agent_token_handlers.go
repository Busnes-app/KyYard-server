package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// agentTokenView is an agent token as the members page lists it: never its secret, and the
// date it is revoked if nobody uses it.
type agentTokenView struct {
	store.ServiceToken
	RevokeAfter time.Time `json:"revoke_after"`
}

func viewOfAgentToken(tok store.ServiceToken) agentTokenView {
	return agentTokenView{ServiceToken: tok, RevokeAfter: store.AgentLastActive(&tok).Add(store.AgentIdleLimit)}
}

// refuseBearer keeps agent-token management to a person's session: a kyPulse token holds no
// such permission anyway, and an agent token never reaches /api.
func (s *Server) refuseBearer(w http.ResponseWriter, a store.TenantAccess) bool {
	if a.ServiceTokenID == "" {
		return false
	}
	s.tenantError(w, store.ErrForbidden)
	return true
}

func (s *Server) handleAgentTokens(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if s.refuseBearer(w, a) {
		return
	}
	list, err := s.store.Tenancy().ListAgentTokens(r.Context(), a)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	out := make([]agentTokenView, 0, len(list))
	for _, tok := range list {
		out = append(out, viewOfAgentToken(tok))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleCreateAgentToken mints a token and shows it once. scope is required, so no client gets
// operator access by default.
func (s *Server) handleCreateAgentToken(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if s.refuseBearer(w, a) {
		return
	}
	var req struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	issue, err := s.store.Tenancy().CreateAgentToken(r.Context(), a, req.Name, req.Scope)
	if errors.Is(err, store.ErrAgentTokenLimit) {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Revoke an agent token before creating another", "code": "agent_token_limit"})
		return
	}
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"token": issue.Token, "agent_token": viewOfAgentToken(issue.ServiceToken),
		"disclosure": "Shown once. Put it in the agent's environment, not in a config file you share or commit."})
}

// handleSetAgentTokenScope moves a token between read-only and operator access.
func (s *Server) handleSetAgentTokenScope(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if s.refuseBearer(w, a) {
		return
	}
	id := r.PathValue("token")
	var req struct {
		Scope string `json:"scope"`
	}
	if id == "" || len(id) > 64 || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().SetAgentTokenScope(r.Context(), a, id, req.Scope); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeAgentToken(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if s.refuseBearer(w, a) {
		return
	}
	id := r.PathValue("token")
	if id == "" || len(id) > 64 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().RevokeAgentToken(r.Context(), a, id); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
