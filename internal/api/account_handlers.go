package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const recoveryCodeCount = 10

type accountRequest struct {
	Password    string `json:"password"`
	NewPassword string `json:"new_password"`
	Code        string `json:"code"`
}

// accountUser authenticates a signed-in local-password account, charges budget, and
// decodes the body. Federated accounts have no local password to prove, so they are refused.
func (s *Server) accountUser(w http.ResponseWriter, r *http.Request, budget string, limit int, window time.Duration) (*store.User, *accountRequest, bool) {
	user, _, err := s.sessions.AuthenticateRequest(r)
	if errors.Is(err, auth.ErrPasswordChangeRequired) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "Change your password before continuing", "code": "password_change_required"})
		return nil, nil, false
	}
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "Authentication required")
		return nil, nil, false
	}
	if user.SSOProvider != "local" || user.PasswordHash == "" {
		s.writeError(w, http.StatusForbidden, "Only accounts with a local password can change their sign-in security")
		return nil, nil, false
	}
	if !s.allowAccountAttempt(budget+user.ID, limit, window) {
		s.writeError(w, http.StatusTooManyRequests, "Too many attempts; wait a few minutes")
		return nil, nil, false
	}
	var req accountRequest
	if strictJSON(r, &req) != nil || len(req.Password) > 1024 || len(req.NewPassword) > 1024 || len(req.Code) > 64 {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return nil, nil, false
	}
	return user, &req, true
}

// mfaUser is accountUser on the per-account MFA budget the login challenge also spends.
func (s *Server) mfaUser(w http.ResponseWriter, r *http.Request) (*store.User, *accountRequest, bool) {
	return s.accountUser(w, r, "mfa-user:", 5, 5*time.Minute)
}

func (s *Server) checkPassword(w http.ResponseWriter, user *store.User, candidate string) bool {
	if ok, err := password.Verify(candidate, user.PasswordHash); err != nil || !ok {
		s.writeError(w, http.StatusUnauthorized, "Current password is incorrect")
		return false
	}
	return true
}

// spendTOTP validates code against the sealed secret and spends its counter, so a code
// accepted here cannot be replayed at login or on another route.
func (s *Server) spendTOTP(ctx context.Context, userID, secretEnc, code string) bool {
	secret, err := crypto.DecryptAESGCM(secretEnc, s.config.Security.EncryptionKey)
	if err != nil {
		return false
	}
	counter, ok := auth.ValidateTOTP(string(secret), code)
	return ok && s.store.Users().SpendTOTPCounter(ctx, userID, counter) == nil
}

// stepUp proves the signed-in caller again: the current password, then a TOTP code when MFA
// is enrolled, on the per-account MFA budget login also spends. It returns 0 or the refusal's
// status, code and message.
func (s *Server) stepUp(ctx context.Context, user *store.User, candidate, code string) (int, string, string) {
	if ok, err := password.Verify(candidate, user.PasswordHash); err != nil || !ok {
		return http.StatusUnauthorized, "password_incorrect", "Current password is incorrect"
	}
	if !user.TOTPEnabled {
		return 0, "", ""
	}
	if !s.allowAccountAttempt("mfa-user:"+user.ID, 5, 5*time.Minute) {
		return http.StatusTooManyRequests, "too_many_attempts", "Too many attempts; wait a few minutes"
	}
	if !s.spendTOTP(ctx, user.ID, user.TOTPSecretEnc, code) {
		return http.StatusUnauthorized, "code_invalid", "Invalid or already used code"
	}
	return 0, "", ""
}

func (s *Server) writeMFAStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusConflict, "Account changed; reload and try again")
		return
	}
	s.writeError(w, http.StatusInternalServerError, "Failed to update sign-in security")
}

func (s *Server) handleMFAEnrol(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	if user.TOTPEnabled {
		s.writeError(w, http.StatusConflict, "Disable MFA before enrolling again")
		return
	}
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to generate MFA secret")
		return
	}
	sealed, err := crypto.EncryptAESGCM([]byte(secret), s.config.Security.EncryptionKey)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to secure MFA secret")
		return
	}
	from := store.MFAState{SecretEnc: user.TOTPSecretEnc, RecoveryCodesHash: user.RecoveryCodesHash}
	to := store.MFAState{SecretEnc: sealed, RecoveryCodesHash: user.RecoveryCodesHash}
	if err := s.store.Users().UpdateMFA(r.Context(), user.ID, user.PasswordHash, from, to, "auth.mfa.enrol_started", s.requestIP(r)); err != nil {
		s.writeMFAStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "otpauth_uri": auth.GenerateTOTPURL("KyYard", user.Username, secret)})
}

func (s *Server) handleMFAConfirm(w http.ResponseWriter, r *http.Request) {
	user, req, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	if user.TOTPEnabled || user.TOTPSecretEnc == "" {
		s.writeError(w, http.StatusConflict, "Start MFA enrolment first")
		return
	}
	if !s.checkPassword(w, user, req.Password) {
		return
	}
	if !s.spendTOTP(r.Context(), user.ID, user.TOTPSecretEnc, req.Code) {
		s.writeError(w, http.StatusUnauthorized, "Invalid or already used code")
		return
	}
	s.issueRecoveryCodes(w, r, user, false, "auth.mfa.enabled")
}

func (s *Server) handleMFARecoveryCodes(w http.ResponseWriter, r *http.Request) {
	user, req, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	if !user.TOTPEnabled {
		s.writeError(w, http.StatusConflict, "MFA is not enabled")
		return
	}
	if !s.spendTOTP(r.Context(), user.ID, user.TOTPSecretEnc, req.Code) {
		s.writeError(w, http.StatusUnauthorized, "Invalid or already used code")
		return
	}
	s.issueRecoveryCodes(w, r, user, true, "auth.mfa.recovery_codes_regenerated")
}

// issueRecoveryCodes enables MFA (or keeps it enabled) with a fresh set of codes, shown once.
func (s *Server) issueRecoveryCodes(w http.ResponseWriter, r *http.Request, user *store.User, enabled bool, action string) {
	codes, hashed, err := auth.GenerateRecoveryCodes(recoveryCodeCount)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to generate recovery codes")
		return
	}
	from := store.MFAState{SecretEnc: user.TOTPSecretEnc, Enabled: enabled, RecoveryCodesHash: user.RecoveryCodesHash}
	to := store.MFAState{SecretEnc: user.TOTPSecretEnc, Enabled: true, RecoveryCodesHash: hashed}
	if err := s.store.Users().UpdateMFA(r.Context(), user.ID, user.PasswordHash, from, to, action, s.requestIP(r)); err != nil {
		s.writeMFAStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"totp_enabled": true, "recovery_codes": codes})
}

// handleMFADisable takes the password and either a TOTP or an unused recovery code. The
// swap expects the recovery hashes the code matched, so a concurrent redemption wins once.
func (s *Server) handleMFADisable(w http.ResponseWriter, r *http.Request) {
	user, req, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	if !user.TOTPEnabled {
		s.writeError(w, http.StatusConflict, "MFA is not enabled")
		return
	}
	if !s.checkPassword(w, user, req.Password) {
		return
	}
	if _, recovery := auth.RedeemRecoveryCode(req.Code, user.RecoveryCodesHash); !recovery && !s.spendTOTP(r.Context(), user.ID, user.TOTPSecretEnc, req.Code) {
		s.writeError(w, http.StatusUnauthorized, "Invalid or already used code")
		return
	}
	from := store.MFAState{SecretEnc: user.TOTPSecretEnc, Enabled: true, RecoveryCodesHash: user.RecoveryCodesHash}
	if err := s.store.Users().UpdateMFA(r.Context(), user.ID, user.PasswordHash, from, store.MFAState{RecoveryCodesHash: "[]"}, "auth.mfa.disabled", s.requestIP(r)); err != nil {
		s.writeMFAStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"totp_enabled": false})
}

// handleAccountPassword is the self-service change. Every session and MFA challenge is
// revoked with it, then this browser gets a fresh session bound to the new hash.
func (s *Server) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	user, req, ok := s.accountUser(w, r, "password-change-user:", 5, time.Minute)
	if !ok {
		return
	}
	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Password == req.NewPassword {
		s.writeError(w, http.StatusBadRequest, "Choose a different password")
		return
	}
	if status, _, msg := s.stepUp(r.Context(), user, req.Password, req.Code); status != 0 {
		s.writeError(w, status, msg)
		return
	}
	hash, err := password.Hash(req.NewPassword)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to secure password")
		return
	}
	if err := s.store.Users().ChangePassword(r.Context(), user.ID, user.PasswordHash, hash, s.requestIP(r)); err != nil {
		s.writeMFAStoreError(w, err)
		return
	}
	user.PasswordHash = hash
	if _, _, err := s.sessions.IssueSession(r.Context(), w, r, user); err != nil {
		_ = s.sessions.RevokeSession(r.Context(), w, r)
		s.writeJSON(w, http.StatusOK, map[string]bool{"password_changed": true, "login_required": true})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"password_changed": true, "login_required": false})
}
