package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

type ThemeUpdateRequest struct {
	Theme string `json:"theme"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	theme, _ := s.store.Settings().GetSetting(r.Context(), "site_theme")
	if theme == "" {
		theme = "patina"
	}

	// Public tier: what the login screen needs before a session exists.
	out := map[string]any{
		"app_name":         s.config.Server.AppName,
		"app_url":          s.config.Server.AppURL,
		"theme":            theme,
		"captcha_provider": s.config.Captcha.Provider,
		"sso_enabled":      s.config.SSO.Enabled,
	}

	user, _, err := s.sessions.AuthenticateRequest(r)
	if err != nil {
		s.writeJSON(w, http.StatusOK, out)
		return
	}

	out["scim_enabled"] = s.config.SCIM.Enabled
	out["db_driver"] = s.config.Database.Driver

	// extra_settings holds secrets such as the SCIM bearer token. The KyRecovery pairing
	// token is sealed at rest and never leaves the process in either form: an admin can see
	// that a recovery URL is set, never the token that authenticates to it.
	if user.Role == "admin" {
		settings, err := s.store.Settings().GetAllSettings(r.Context())
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to load settings")
			return
		}
		// By prefix, not by literal: the lib owns the token's key name, and a future
		// spelling must not leak by default.
		for k := range settings {
			if strings.HasPrefix(k, "kyrecovery_token") {
				delete(settings, k)
			}
		}
		out["extra_settings"] = settings
	}

	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSetTheme(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req ThemeUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Theme == "" {
		s.writeError(w, http.StatusBadRequest, "Invalid theme")
		return
	}

	if err := s.store.Settings().SetSetting(r.Context(), "site_theme", req.Theme); err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to save theme")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"theme": req.Theme})
}
