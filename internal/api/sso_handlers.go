package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/sso"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func (s *Server) handleKySignOnSyncWebhook(w http.ResponseWriter, r *http.Request) {
	if _, managed, err := s.store.InstallerConfiguration(r.Context()); err != nil || managed {
		if err != nil {
			s.writeError(w, 503, "Directory unavailable")
			return
		}
		s.handleInstallerDirectory(w, r)
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	sig := r.Header.Get("X-KySignOn-Signature")
	if sig == "" {
		sig = r.Header.Get("X-Signature-SHA256")
	}

	// Read before the signature is checked, so bound it: a directory event is a few hundred bytes.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		s.writeError(w, http.StatusRequestEntityTooLarge, "Request body too large or unreadable")
		return
	}

	switch err := s.kysignon.HandleSyncWebhook(r.Context(), body, sig); {
	case err == nil:
	case errors.Is(err, sso.ErrUsernameTaken):
		s.writeError(w, http.StatusConflict, "username taken")
		return
	case errors.Is(err, store.ErrStaleEvent):
		s.writeError(w, http.StatusConflict, "event already applied or superseded")
		return
	default:
		log.Printf("[SSO] directory webhook refused: %v", err)
		s.writeError(w, http.StatusUnauthorized, "webhook refused")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"synced": true})
}

// ssoLogoutFence outlives the five-minute login and MFA windows, so a logout that lands
// while the browser is still finishing that login also ends it.
const ssoLogoutFence = 10 * time.Minute

// handleBackchannelLogout is the OpenID Connect Back-Channel Logout receiver: the provider
// POSTs a signed logout_token when a login ends, and every local session of it ends here.
// Anonymous by design: the provider's signature is its authority.
func (s *Server) handleBackchannelLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	all, err := s.providers(r.Context())
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "Sign-in providers unavailable")
		return
	}
	// A disabled provider still ends the sessions it issued.
	var p sso.Provider
	for _, candidate := range all {
		if candidate.ID == r.PathValue("provider") {
			p = candidate
		}
	}
	ctx, cancel := context.WithTimeout(sso.ProviderContext(r.Context()), 20*time.Second)
	defer cancel()
	claims, err := p.VerifyLogout(ctx, r.PostFormValue("logout_token"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	fenceUntil := time.Now().Add(ssoLogoutFence)
	if claims.SessionID != "" {
		err = s.store.Sessions().EndSSOSession(ctx, p.ID, claims.SessionID, fenceUntil)
	} else {
		// iat has whole-second precision: round up so a login from that same second ends too.
		err = s.store.Sessions().EndSSOSubject(ctx, p.ID, claims.Subject, claims.IssuedAt.Add(time.Second), fenceUntil)
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "logout failed")
		return
	}
	s.auditPlatform(r, "", "auth.sso.logout", p.ID, "", "success")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleSAMLMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(s.saml.GenerateMetadata()))
}
