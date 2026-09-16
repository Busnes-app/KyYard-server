package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/store"
)

// Docker-socket access is host-equivalent; every enrollment command says so, in the same
// response as the one-time token, so it cannot be missed.
const socketDisclosure = "Mounting /var/run/docker.sock gives the KyYard agent, and therefore this control plane, root-equivalent access to that host. Enroll only hosts whose operators accept that."

func (s *Server) instanceFingerprint() string {
	if len(s.config.Security.InstanceKey) != ed25519.SeedSize {
		return ""
	}
	return protocol.Fingerprint(ed25519.NewKeyFromSeed(s.config.Security.InstanceKey).Public().(ed25519.PublicKey))
}

func (s *Server) handleCreateEnrollmentToken(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		Runtime string `json:"runtime"`
	}
	if err := strictJSON(r, &input); err != nil {
		s.tenantError(w, err)
		return
	}
	image := s.config.Server.AgentImage
	tok, err := s.store.Tenancy().CreateEnrollmentToken(r.Context(), a, input.Runtime, image)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	secret := base64.RawURLEncoding.EncodeToString(tok.Secret)
	out := map[string]any{
		"id": tok.ID, "environment_id": tok.EnvironmentID, "runtime": tok.Runtime, "expires_at": tok.ExpiresAt,
		"token": secret, "disclosure": socketDisclosure,
	}
	// No configured image, no command: the control plane never points operators at an image it
	// has not been told to trust by digest.
	if image != "" {
		out["image"] = image
		out["command"] = fmt.Sprintf("printf '%%s\\n' '%s' | docker run -d -i --name kyyard-agent --restart unless-stopped -v /var/run/docker.sock:/var/run/docker.sock -v kyyard-agent-identity:/var/lib/kyyard-agent %s --server %s", secret, image, s.config.Server.AppURL)
	} else {
		out["note"] = "Set KY_AGENT_IMAGE to a digest-pinned agent image to receive a ready-to-run command."
	}
	s.writeJSON(w, http.StatusCreated, out)
}

// handleAgentEnroll is the only agent-facing route in this slice. It has no session: the
// token selects the tenant, the proof binds the key, and every refusal is the same 401.
func (s *Server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.allowAttempt("enroll:"+s.requestIP(r), 10, time.Minute) {
		s.writeError(w, http.StatusTooManyRequests, "Too many enrollment attempts")
		return
	}
	var input struct {
		Token     string            `json:"token"`
		PublicKey string            `json:"public_key"`
		Proof     string            `json:"proof"`
		Name      string            `json:"name"`
		Facts     map[string]string `json:"facts"`
	}
	if err := strictJSON(r, &input); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid enrollment request")
		return
	}
	token, err1 := base64.RawURLEncoding.DecodeString(input.Token)
	key, err2 := base64.RawURLEncoding.DecodeString(input.PublicKey)
	proof, err3 := base64.RawURLEncoding.DecodeString(input.Proof)
	if err1 != nil || err2 != nil || err3 != nil {
		s.writeError(w, http.StatusUnauthorized, "Enrollment refused")
		return
	}
	e, err := s.store.Tenancy().Enroll(r.Context(), store.EnrollmentRequest{Token: token, PublicKey: key, Proof: proof, Name: input.Name, Facts: input.Facts, IPAddress: s.requestIP(r)})
	switch {
	case errors.Is(err, store.ErrAlreadyExists):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Endpoint name already used in this organization", "code": "name_taken"})
		return
	case err != nil:
		s.writeError(w, http.StatusUnauthorized, "Enrollment refused")
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"endpoint_id": e.ID, "state": e.State, "fingerprint": e.Fingerprint, "instance_fingerprint": s.instanceFingerprint()})
}

func endpointID(r *http.Request) (string, error) {
	id := r.PathValue("endpoint")
	if id == "" || len(id) > 64 {
		return "", store.ErrInvalid
	}
	return id, nil
}

func (s *Server) handleTenantEndpoints(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	offset, limit, err := tenantPage(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	rows, err := s.store.Tenancy().ListEndpoints(r.Context(), a, offset, limit)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleTenantEndpoint(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	e, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, id)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleApproveEndpoint(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	var input struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := strictJSON(r, &input); err != nil {
		s.tenantError(w, err)
		return
	}
	if _, err := hex.DecodeString(input.Fingerprint); err != nil || len(input.Fingerprint) != 64 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().ApproveEndpoint(r.Context(), a, id, input.Fingerprint); err != nil {
		s.tenantError(w, err)
		return
	}
	s.agents.notify(id, envelope(protocol.TypeApproved, nil))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) endpointTransition(op func(context.Context, store.TenantAccess, string) error) func(http.ResponseWriter, *http.Request, store.TenantAccess) {
	return func(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
		id, err := endpointID(r)
		if err != nil {
			s.tenantError(w, err)
			return
		}
		if err := op(r.Context(), a, id); err != nil {
			s.tenantError(w, err)
			return
		}
		// Reject and revoke are terminal: a live socket ends in the same request.
		s.agents.closeEndpoint(id, protocol.CloseRevoked)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleRenameEndpoint(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	name, err := tenantName(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if err := s.store.Tenancy().RenameEndpoint(r.Context(), a, id, name); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAcknowledgeKey is the human half of rotation: the named pending key becomes the only
// approved key, and a live agent is told so it switches without waiting for a reconnect.
func (s *Server) handleAcknowledgeKey(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	fp := r.PathValue("fingerprint")
	if _, err := hex.DecodeString(fp); err != nil || len(fp) != 64 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().AcknowledgeEndpointKey(r.Context(), a, id, fp); err != nil {
		s.tenantError(w, err)
		return
	}
	s.agents.notify(id, envelope(protocol.TypeRotated, protocol.Rotated{Fingerprint: fp}))
	// The acknowledged key is now the only one; a session on the retired key ends here, in
	// the same request, whether or not its holder honours the notice.
	s.agents.closeEndpoint(id, protocol.CloseKeyRetired)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAcknowledgeEvent(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	eventID, err := strconv.ParseInt(r.PathValue("event"), 10, 64)
	if err != nil || eventID <= 0 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().AcknowledgeEndpointEvent(r.Context(), a, id, eventID); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEndpointInventory(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	inv, err := s.store.Tenancy().ReadInventory(r.Context(), a, id)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, inv)
}

func (s *Server) handleLatestSamples(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	rows, err := s.store.Tenancy().LatestSamples(r.Context(), a, id)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleContainerSamples(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	container := r.PathValue("container")
	if container == "" || len(container) > 128 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	minutes, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
	rows, err := s.store.Tenancy().ReadSamples(r.Context(), a, id, container, time.Duration(minutes)*time.Minute)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}
