package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busness-app/ky-primitives/password"
)

// handleChangePassword completes a mandatory local-password replacement. All sessions
// are revoked atomically with the password update; the user signs in again afterward.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	user, _, err := s.sessions.AuthenticateRequest(r)
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}
	if !user.MustChangePassword || user.SSOProvider != "local" {
		s.writeError(w, http.StatusConflict, "No local password replacement is required")
		return
	}
	if !s.allowAttempt("password-change:"+s.requestIP(r), 10, time.Minute) || !s.allowAttempt("password-change-user:"+user.ID, 5, time.Minute) {
		s.writeError(w, http.StatusTooManyRequests, "Too many password change attempts")
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if len(req.CurrentPassword) > 1024 || len(req.NewPassword) > 1024 {
		s.writeError(w, http.StatusBadRequest, "Password is too long")
		return
	}
	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.CurrentPassword == req.NewPassword {
		s.writeError(w, http.StatusBadRequest, "Choose a different password")
		return
	}
	ok, err := password.Verify(req.CurrentPassword, user.PasswordHash)
	if err != nil || !ok {
		s.writeError(w, http.StatusUnauthorized, "Current password is incorrect")
		return
	}
	hash, err := password.Hash(req.NewPassword)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to secure password")
		return
	}
	if err := s.store.Users().CompletePasswordChange(r.Context(), user.ID, user.PasswordHash, hash, s.requestIP(r)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, http.StatusConflict, "Account changed; sign in again")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "Failed to change password")
		return
	}
	_ = s.sessions.RevokeSession(r.Context(), w, r)
	s.writeJSON(w, http.StatusOK, map[string]bool{"password_changed": true, "login_required": true})
}
