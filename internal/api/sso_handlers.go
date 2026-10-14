package api

import (
	"errors"
	"io"
	"log"
	"net/http"

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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Failed to read request body")
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
	case errors.Is(err, store.ErrLastAdmin):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The account is an organization's last active administrator", "code": "last_administrator"})
		return
	default:
		log.Printf("[SSO] directory webhook refused: %v", err)
		s.writeError(w, http.StatusUnauthorized, "webhook refused")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"synced": true})
}

func (s *Server) handleSAMLMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(s.saml.GenerateMetadata()))
}
