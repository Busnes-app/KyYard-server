package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/sso"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"golang.org/x/oauth2"
)

const providerSetting = "sso_providers_enc"

type loginAttempt struct {
	Provider                     sso.Provider
	Verifier, Nonce, BrowserHash string
	Expires                      time.Time
}

func (s *Server) providers(ctx context.Context) ([]sso.Provider, error) {
	providers := []sso.Provider{}
	blob, err := s.store.Settings().GetSetting(ctx, providerSetting)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if blob != "" {
		plain, e := crypto.DecryptAESGCM(blob, crypto.DeriveKey(s.config.Security.EncryptionKey, "kyyard/sso/providers/v1"))
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(plain, &providers); e != nil {
			return nil, e
		}
	}
	if s.config.SSO.Enabled {
		c := s.config.SSO
		if c.KySignOnIssuer != "" && c.KySignOnClientID != "" {
			providers = append(providers, sso.Provider{ID: "kysignon", Name: "KyIdentity / KySignOn", Kind: "oidc", Issuer: c.KySignOnIssuer, ClientID: c.KySignOnClientID, ClientSecret: c.KySignOnSecret, Enabled: true, AutoProvision: c.AutoProvision})
		}
		if c.GenericOIDCIssuer != "" && c.GenericOIDCClientID != "" {
			providers = append(providers, sso.Provider{ID: "oidc", Name: "OpenID Connect", Kind: "oidc", Issuer: c.GenericOIDCIssuer, ClientID: c.GenericOIDCClientID, ClientSecret: c.GenericOIDCSecret, Enabled: true, AutoProvision: c.AutoProvision})
		}
	}
	return providers, nil
}
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := s.providers(r.Context())
	if err != nil {
		s.writeError(w, 500, "Could not load sign-in providers")
		return
	}
	for i := range providers {
		providers[i].CallbackURL = strings.TrimRight(s.config.Server.AppURL, "/") + "/api/sso/" + providers[i].ID + "/callback"
		providers[i].SecretSet = providers[i].ClientSecret != ""
		providers[i].ClientSecret = ""
	}
	s.writeJSON(w, 200, providers)
}
func (s *Server) handleSaveProvider(w http.ResponseWriter, r *http.Request) {
	user, _, authErr := s.sessions.AuthenticateRequest(r)
	if authErr != nil {
		s.writeError(w, 401, "Authentication required")
		return
	}
	var p sso.Provider
	if strictJSON(r, &p) != nil {
		s.writeError(w, 400, "Invalid provider")
		return
	}
	if err := p.Validate(); err != nil {
		s.writeError(w, 400, err.Error())
		return
	}
	// Serialize read-modify-write so simultaneous admin edits cannot drop a provider.
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	all, err := s.providers(r.Context())
	if err != nil {
		s.writeError(w, 500, "Could not load sign-in providers")
		return
	}
	saved := []sso.Provider{}
	for _, old := range all {
		if old.ID != "kysignon" && old.ID != "oidc" {
			saved = append(saved, old)
		}
	}
	if len(saved) >= 16 {
		s.writeError(w, 409, "At most 16 providers can be configured")
		return
	}
	// Creation-only: a provider ID cannot be reassigned to another issuer's identities.
	p.ID = "idp_" + crypto.RandomHex(12)
	p.SecretSet = false
	p.CallbackURL = ""
	saved = append(saved, p)
	plain, _ := json.Marshal(saved)
	blob, err := crypto.EncryptAESGCM(plain, crypto.DeriveKey(s.config.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err == nil {
		err = s.store.Settings().SetSetting(r.Context(), providerSetting, blob)
	}
	if err != nil {
		s.writeError(w, 500, "Could not save sign-in provider")
		return
	}
	_ = s.store.Audit().LogAudit(r.Context(), &store.AuditRecord{UserID: user.ID, Action: "sso.provider.create", Resource: p.ID})
	p.SecretSet = p.ClientSecret != ""
	p.ClientSecret = ""
	s.writeJSON(w, 201, p)
}
func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	user, _, authErr := s.sessions.AuthenticateRequest(r)
	if authErr != nil {
		s.writeError(w, 401, "Authentication required")
		return
	}
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	id := r.PathValue("provider")
	if !strings.HasPrefix(id, "idp_") {
		s.writeError(w, 400, "Environment providers are managed in server configuration")
		return
	}
	all, err := s.providers(r.Context())
	if err != nil {
		s.writeError(w, 500, "Could not load providers")
		return
	}
	saved := []sso.Provider{}
	found := false
	for _, p := range all {
		if p.ID == id {
			found = true
			continue
		}
		if strings.HasPrefix(p.ID, "idp_") {
			saved = append(saved, p)
		}
	}
	if !found {
		s.writeError(w, 404, "Provider not found")
		return
	}
	plain, _ := json.Marshal(saved)
	blob, err := crypto.EncryptAESGCM(plain, crypto.DeriveKey(s.config.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err == nil {
		err = s.store.Settings().SetSetting(r.Context(), providerSetting, blob)
	}
	if err != nil {
		s.writeError(w, 500, "Could not remove provider")
		return
	}
	_ = s.store.Audit().LogAudit(r.Context(), &store.AuditRecord{UserID: user.ID, Action: "sso.provider.delete", Resource: id})
	s.writeJSON(w, 200, map[string]bool{"removed": true})
}
func (s *Server) provider(r *http.Request) (sso.Provider, bool) {
	all, err := s.providers(r.Context())
	if err != nil {
		return sso.Provider{}, false
	}
	for _, p := range all {
		if p.ID == r.PathValue("provider") && p.Enabled {
			return p, true
		}
	}
	return sso.Provider{}, false
}
func (s *Server) handleProviderLogin(w http.ResponseWriter, r *http.Request) {
	p, ok := s.provider(r)
	if !ok {
		s.writeError(w, 404, "Sign-in provider unavailable")
		return
	}
	if !s.allowAttempt("sso:"+s.requestIP(r), 10, time.Minute) {
		s.writeError(w, 429, "Too many sign-in attempts")
		return
	}
	state, nonce, browser := crypto.RandomHex(24), crypto.RandomHex(24), crypto.RandomHex(24)
	verifier := oauth2.GenerateVerifier()
	redirect := strings.TrimRight(s.config.Server.AppURL, "/") + "/api/sso/" + p.ID + "/callback"
	ctx, cancel := context.WithTimeout(sso.ProviderContext(r.Context()), 20*time.Second)
	defer cancel()
	dest, err := p.AuthURL(ctx, redirect, state, verifier, nonce)
	if err != nil {
		s.writeError(w, 502, "Provider unavailable; check its configuration")
		return
	}
	s.loginMu.Lock()
	if s.logins == nil {
		s.logins = map[string]loginAttempt{}
	}
	for k, v := range s.logins {
		if time.Now().After(v.Expires) {
			delete(s.logins, k)
		}
	}
	if len(s.logins) >= 256 {
		s.loginMu.Unlock()
		s.writeError(w, 503, "Sign-in busy; try again shortly")
		return
	}
	s.logins[state] = loginAttempt{p, verifier, nonce, crypto.SHA256Hex([]byte(browser)), time.Now().Add(5 * time.Minute)}
	s.loginMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "ky_sso_" + state, Value: browser, Path: "/api/sso/" + p.ID + "/callback", MaxAge: 300, HttpOnly: true, Secure: s.config.Security.CookieSecure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, dest, http.StatusFound)
}
func (s *Server) handleProviderCallback(w http.ResponseWriter, r *http.Request) {
	p, ok := s.provider(r)
	if !ok {
		s.writeError(w, 404, "Sign-in provider unavailable")
		return
	}
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie("ky_sso_" + state)
	if err != nil || len(state) != 48 {
		s.writeError(w, 400, "Invalid or expired sign-in state")
		return
	}
	s.loginMu.Lock()
	attempt, ok := s.logins[state]
	valid := ok && attempt.Provider.ID == p.ID && time.Now().Before(attempt.Expires) && crypto.SHA256Hex([]byte(cookie.Value)) == attempt.BrowserHash
	if valid {
		delete(s.logins, state)
	}
	s.loginMu.Unlock()
	if !valid {
		s.writeError(w, 400, "Invalid or expired sign-in state")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookie.Name, Value: "", Path: "/api/sso/" + p.ID + "/callback", MaxAge: -1, HttpOnly: true, Secure: s.config.Security.CookieSecure, SameSite: http.SameSiteLaxMode})
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		s.writeError(w, 400, "Sign-in was cancelled or refused")
		return
	}
	redirect := strings.TrimRight(s.config.Server.AppURL, "/") + "/api/sso/" + p.ID + "/callback"
	ctx, cancel := context.WithTimeout(sso.ProviderContext(r.Context()), 30*time.Second)
	defer cancel()
	claims, err := attempt.Provider.Exchange(ctx, r.URL.Query().Get("code"), attempt.Verifier, redirect, attempt.Nonce)
	if err != nil {
		s.writeError(w, 401, "Provider could not verify your identity")
		return
	}
	user, err := s.store.Users().GetUserBySSO(ctx, p.ID, claims.Subject)
	if errors.Is(err, store.ErrNotFound) && p.AutoProvision {
		user = &store.User{ID: "usr_" + crypto.RandomHex(12), Username: claims.PreferredUsername, Email: claims.Email, DisplayName: claims.Name, Role: "user", Status: "active", SSOProvider: p.ID, SSOSubject: claims.Subject}
		err = s.store.Users().CreateUser(ctx, user)
	}
	if err != nil || user == nil || user.Status != "active" {
		s.writeError(w, 403, "Account unavailable; contact your administrator")
		return
	}
	if _, _, err = s.sessions.IssueSession(ctx, w, r, user); err != nil {
		s.writeError(w, 403, "Account unavailable")
		return
	}
	_ = s.store.Audit().LogAudit(ctx, &store.AuditRecord{UserID: user.ID, Action: "auth.sso.login", Resource: p.ID})
	http.Redirect(w, r, "/", http.StatusFound)
}

// Credentials and availability may change without changing the provider's identity namespace.
func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	user, _, err := s.sessions.AuthenticateRequest(r)
	if err != nil {
		s.writeError(w, 401, "Authentication required")
		return
	}
	var update struct {
		ClientSecret *string `json:"client_secret"`
		Enabled      *bool   `json:"enabled"`
	}
	if strictJSON(r, &update) != nil || (update.ClientSecret == nil && update.Enabled == nil) || (update.ClientSecret != nil && len(*update.ClientSecret) > 4096) {
		s.writeError(w, 400, "Supply a client secret or enabled state")
		return
	}
	id := r.PathValue("provider")
	if !strings.HasPrefix(id, "idp_") {
		s.writeError(w, 400, "Environment providers are managed in server configuration")
		return
	}
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	all, err := s.providers(r.Context())
	if err != nil {
		s.writeError(w, 500, "Could not load providers")
		return
	}
	saved := []sso.Provider{}
	found := false
	for _, p := range all {
		if !strings.HasPrefix(p.ID, "idp_") {
			continue
		}
		if p.ID == id {
			found = true
			if update.ClientSecret != nil {
				p.ClientSecret = *update.ClientSecret
			}
			if update.Enabled != nil {
				p.Enabled = *update.Enabled
			}
		}
		saved = append(saved, p)
	}
	if !found {
		s.writeError(w, 404, "Provider not found")
		return
	}
	plain, _ := json.Marshal(saved)
	blob, err := crypto.EncryptAESGCM(plain, crypto.DeriveKey(s.config.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err == nil {
		err = s.store.Settings().SetSetting(r.Context(), providerSetting, blob)
	}
	if err != nil {
		s.writeError(w, 500, "Could not update provider")
		return
	}
	_ = s.store.Audit().LogAudit(r.Context(), &store.AuditRecord{UserID: user.ID, Action: "sso.provider.update", Resource: id})
	s.writeJSON(w, 200, map[string]bool{"updated": true})
}
