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

func (s *Server) handleSAMLMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(s.saml.GenerateMetadata()))
}
