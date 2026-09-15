package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Busness-app/ky-primitives/password"
	"github.com/Busness-app/ky_server_base/internal/auth"
	"github.com/Busness-app/ky_server_base/internal/crypto"
	"github.com/Busness-app/ky_server_base/internal/store"
)

type LoginRequest struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	CaptchaToken string `json:"captcha_token,omitempty"`
}

type MFARequest struct {
	MFAToken string `json:"mfa_token"`
	Code     string `json:"code"`
}

func (s *Server) handlePoWChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	challenge, err := auth.GeneratePoWChallenge(s.config.Captcha.DifficultyPoW, s.config.Security.SessionSecret)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to generate PoW challenge")
		return
	}

	s.writeJSON(w, http.StatusOK, challenge)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if !s.allowAttempt("login:"+s.requestIP(r), 20, time.Minute) {
		s.writeError(w, http.StatusTooManyRequests, "Too many login attempts")
		return
	}

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		s.writeError(w, http.StatusBadRequest, "Username and password are required")
		return
	}

	// Verify PoW CAPTCHA if enabled
	if s.config.Captcha.Provider == "pow" {
		if req.CaptchaToken == "" || !auth.VerifyPoWSolution(req.CaptchaToken, s.config.Security.SessionSecret) {
			s.writeError(w, http.StatusForbidden, "Security check failed. Please complete the CAPTCHA puzzle.")
			return
		}
	}

	user, err := s.store.Users().GetUserByUsername(r.Context(), req.Username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, http.StatusUnauthorized, "Invalid credentials")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "Authentication error")
		return
	}

	if user.Status != "active" {
		s.writeError(w, http.StatusForbidden, "Account is disabled or inactive")
		return
	}

	ok, err := password.Verify(req.Password, user.PasswordHash)
	if err != nil || !ok {
		s.writeError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}

	// Check if MFA is required
	if user.TOTPEnabled {
		rawChallenge := crypto.RandomHex(32)
		if err := s.store.Sessions().CreateMFAChallenge(r.Context(), &store.MFAChallenge{
			TokenHash: crypto.SHA256Hex([]byte(rawChallenge)),
			UserID:    user.ID,
			ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
		}); err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to start MFA verification")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"mfa_required": true,
			"mfa_token":    rawChallenge,
			"methods":      []string{"totp", "recovery_code"},
		})
		return
	}

	// Issue active session
	_, _, err = s.sessions.IssueSession(r.Context(), w, r, user.ID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to issue session")
		return
	}

	_ = s.store.Audit().LogAudit(r.Context(), &store.AuditRecord{
		UserID:   user.ID,
		Action:   "auth.login",
		Resource: "session",
	})

	s.writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          user,
	})
}

func (s *Server) handleMFATOTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req MFARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	// Key on the client alone. Nothing caller-supplied may reach the key: a token in the key
	// lets one client mint unbounded slots and starve every other window. Same shape as the
	// login limit above, so a client occupies one MFA slot no matter how it spends it.
	if !s.allowAttempt("mfa:"+s.requestIP(r), 20, time.Minute) {
		s.writeError(w, http.StatusTooManyRequests, "Too many MFA attempts")
		return
	}

	userID, err := s.store.Sessions().ConsumeMFAChallenge(r.Context(), crypto.SHA256Hex([]byte(req.MFAToken)))
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "Invalid or expired MFA transaction")
		return
	}
	// Per-account, so a botnet spread across many IPs cannot outrun the per-IP window. Each
	// guess already costs a fresh password login, since the challenge is consumed either way.
	if !s.allowAttempt("mfa-user:"+userID, 5, 5*time.Minute) {
		s.writeError(w, http.StatusTooManyRequests, "Too many MFA attempts for this account")
		return
	}
	user, err := s.store.Users().GetUserByID(r.Context(), userID)
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "User not found")
		return
	}
	if user.Status != "active" || !user.TOTPEnabled {
		s.writeError(w, http.StatusForbidden, "Account is not eligible for MFA login")
		return
	}

	secretBytes, err := crypto.DecryptAESGCM(user.TOTPSecretEnc, s.config.Security.EncryptionKey)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to decrypt MFA secret")
		return
	}

	counter, ok := auth.ValidateTOTP(string(secretBytes), req.Code)
	if !ok {
		s.writeError(w, http.StatusUnauthorized, "Invalid TOTP verification code")
		return
	}
	if err := s.store.Users().SpendTOTPCounter(r.Context(), user.ID, counter); err != nil {
		s.writeError(w, http.StatusUnauthorized, "TOTP code already used")
		return
	}

	_, _, err = s.sessions.IssueSession(r.Context(), w, r, user.ID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to issue session")
		return
	}

	_ = s.store.Audit().LogAudit(r.Context(), &store.AuditRecord{
		UserID:   user.ID,
		Action:   "auth.mfa_totp",
		Resource: "session",
	})

	s.writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          user,
	})
}

func (s *Server) handleMFARecovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req MFARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	// Key on the client alone. Nothing caller-supplied may reach the key: a token in the key
	// lets one client mint unbounded slots and starve every other window. Same shape as the
	// login limit above, so a client occupies one MFA slot no matter how it spends it.
	if !s.allowAttempt("mfa:"+s.requestIP(r), 20, time.Minute) {
		s.writeError(w, http.StatusTooManyRequests, "Too many MFA attempts")
		return
	}

	userID, err := s.store.Sessions().ConsumeMFAChallenge(r.Context(), crypto.SHA256Hex([]byte(req.MFAToken)))
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "Invalid or expired MFA transaction")
		return
	}
	// Per-account, so a botnet spread across many IPs cannot outrun the per-IP window. Each
	// guess already costs a fresh password login, since the challenge is consumed either way.
	if !s.allowAttempt("mfa-user:"+userID, 5, 5*time.Minute) {
		s.writeError(w, http.StatusTooManyRequests, "Too many MFA attempts for this account")
		return
	}
	user, err := s.store.Users().GetUserByID(r.Context(), userID)
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "User not found")
		return
	}
	if user.Status != "active" || !user.TOTPEnabled {
		s.writeError(w, http.StatusForbidden, "Account is not eligible for MFA login")
		return
	}

	updatedHashes, ok := auth.RedeemRecoveryCode(req.Code, user.RecoveryCodesHash)
	if !ok {
		s.writeError(w, http.StatusUnauthorized, "Invalid or already used recovery code")
		return
	}

	if err := s.store.Users().UpdateRecoveryCodes(r.Context(), user.ID, user.RecoveryCodesHash, updatedHashes); err != nil {
		s.writeError(w, http.StatusUnauthorized, "Recovery code was already used")
		return
	}

	_, _, err = s.sessions.IssueSession(r.Context(), w, r, user.ID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to issue session")
		return
	}

	_ = s.store.Audit().LogAudit(r.Context(), &store.AuditRecord{
		UserID:   user.ID,
		Action:   "auth.mfa_recovery_code",
		Resource: "session",
	})

	s.writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          user,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	_ = s.sessions.RevokeSession(r.Context(), w, r)
	s.writeJSON(w, http.StatusOK, map[string]bool{"logged_out": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, _, err := s.sessions.AuthenticateRequest(r)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          user,
	})
}
