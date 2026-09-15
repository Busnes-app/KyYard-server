package api

import (
	"fmt"
	"io"
	"net/http"

	"github.com/Busness-app/ky_server_base/internal/crypto"
	"github.com/Busness-app/ky_server_base/internal/store"
	"golang.org/x/oauth2"
)

func (s *Server) handleKySignOnLogin(w http.ResponseWriter, r *http.Request) {
	state := crypto.RandomHex(16)
	nonce := crypto.RandomHex(16)
	verifier := oauth2.GenerateVerifier()

	redirectURI := fmt.Sprintf("%s/api/sso/kysignon/callback", s.config.Server.AppURL)
	authURL, err := s.kysignon.BuildAuthURL(r.Context(), redirectURI, state, verifier, nonce)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Store verifier in short-lived cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "ky_pkce_" + state,
		Value:    verifier,
		Path:     "/",
		MaxAge:   300,
		HttpOnly: true,
		Secure:   s.config.Security.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "ky_nonce_" + state,
		Value:    nonce,
		Path:     "/api/sso/kysignon/callback",
		MaxAge:   300,
		HttpOnly: true,
		Secure:   s.config.Security.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, authURL, http.StatusFound)
}

func (s *Server) handleKySignOnCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")

	cookie, err := r.Cookie("ky_pkce_" + state)
	if err != nil || cookie.Value == "" {
		s.writeError(w, http.StatusBadRequest, "Invalid or expired SSO state")
		return
	}
	verifier := cookie.Value
	nonceCookie, err := r.Cookie("ky_nonce_" + state)
	if err != nil || nonceCookie.Value == "" {
		s.writeError(w, http.StatusBadRequest, "Invalid or expired SSO nonce")
		return
	}

	redirectURI := fmt.Sprintf("%s/api/sso/kysignon/callback", s.config.Server.AppURL)
	claims, err := s.kysignon.ExchangeCode(r.Context(), code, verifier, redirectURI, nonceCookie.Value)
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, fmt.Sprintf("SSO exchange failed: %v", err))
		return
	}

	// Upsert user
	user, err := s.store.Users().GetUserBySSO(r.Context(), "kysignon", claims.Subject)
	if err != nil {
		if s.config.SSO.AutoProvision {
			user = &store.User{
				ID:          fmt.Sprintf("usr_%s", crypto.RandomHex(12)),
				Username:    claims.PreferredUsername,
				Email:       claims.Email,
				DisplayName: claims.Name,
				Role:        "user",
				Status:      "active",
				SSOProvider: "kysignon",
				SSOSubject:  claims.Subject,
			}
			if err := s.store.Users().CreateUser(r.Context(), user); err != nil {
				s.writeError(w, http.StatusInternalServerError, "Failed to provision SSO user")
				return
			}
		} else {
			s.writeError(w, http.StatusForbidden, "User account not provisioned")
			return
		}
	}

	_, _, err = s.sessions.IssueSession(r.Context(), w, r, user.ID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Session creation failed")
		return
	}

	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleKySignOnSyncWebhook(w http.ResponseWriter, r *http.Request) {
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

	if err := s.kysignon.HandleSyncWebhook(r.Context(), body, sig); err != nil {
		s.writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"synced": true})
}

func (s *Server) handleSAMLMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(s.saml.GenerateMetadata()))
}
