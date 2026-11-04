package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
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
	Link                         *ssoLinkAttempt
}

type ssoLinkAttempt struct{ UserID, SessionHash, PasswordHash string }

func (s *Server) providers(ctx context.Context) ([]sso.Provider, error) {
	if p, managed, err := s.installerProvider(ctx); err != nil || managed {
		if err != nil {
			return nil, err
		}
		return []sso.Provider{p}, nil
	}
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
	for _, p := range all {
		if p.InstallerManaged {
			s.writeError(w, 409, "Sign-in is managed by KyQuickStart")
			return
		}
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
	p.InstallerManaged = false
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
	// Its sign-ins would otherwise outlive it to their full TTL, beyond its back-channel logout.
	// Ended first, so a failure leaves the provider in place to remove again.
	err = s.store.Sessions().EndSSOProvider(r.Context(), id)
	plain, _ := json.Marshal(saved)
	var blob string
	if err == nil {
		blob, err = crypto.EncryptAESGCM(plain, crypto.DeriveKey(s.config.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	}
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
	return s.enabledProvider(r.Context(), r.PathValue("provider"))
}

func (s *Server) enabledProvider(ctx context.Context, id string) (sso.Provider, bool) {
	all, err := s.providers(ctx)
	if err != nil {
		return sso.Provider{}, false
	}
	for _, p := range all {
		if p.ID == id && p.Enabled {
			return p, true
		}
	}
	return sso.Provider{}, false
}

// lockEnabledProvider takes providersMu and reports whether provider id is still configured and
// enabled; the caller unlocks. A sign-in issues its session or MFA challenge under it, so one
// whose token exchange straddled the provider's removal or disabling cannot outlive the
// EndSSOProvider purge, which runs under the same lock.
func (s *Server) lockEnabledProvider(ctx context.Context, id string) bool {
	s.providersMu.Lock()
	_, ok := s.enabledProvider(ctx, id)
	return ok
}
func (s *Server) handleProviderLogin(w http.ResponseWriter, r *http.Request) {
	s.startProviderLogin(w, r, nil)
}

func (s *Server) handleSSOLinkOptions(w http.ResponseWriter, r *http.Request) {
	user, _, err := s.sessions.AuthenticateRequest(r)
	if err != nil {
		s.writeError(w, 401, "Authentication required")
		return
	}
	if user.SSOProvider != "local" || user.PasswordHash == "" {
		s.writeError(w, 403, "Linking requires a local account")
		return
	}
	providers, err := s.providers(r.Context())
	if err != nil {
		s.writeError(w, 500, "Could not load providers")
		return
	}
	out := []map[string]string{}
	for _, p := range providers {
		if p.Enabled && !p.InstallerManaged {
			out = append(out, map[string]string{"id": p.ID, "name": p.Name})
		}
	}
	s.writeJSON(w, 200, out)
}

func (s *Server) handleLinkProvider(w http.ResponseWriter, r *http.Request) {
	user, session, err := s.sessions.AuthenticateRequest(r)
	if err != nil {
		s.writeError(w, 401, "Authentication required")
		return
	}
	if user.SSOProvider != "local" || user.PasswordHash == "" {
		s.writeError(w, 403, "Linking requires a local account")
		return
	}
	if !s.allowAccountAttempt("sso-link:"+user.ID, 5, time.Minute) {
		s.writeError(w, 429, "Too many linking attempts")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if strictJSON(r, &req) != nil || req.Password == "" || len(req.Password) > 4096 {
		s.writeError(w, 400, "Supply your current password")
		return
	}
	valid, err := password.Verify(req.Password, user.PasswordHash)
	if err != nil || !valid {
		s.writeError(w, 401, "Invalid credentials")
		return
	}
	s.startProviderLogin(w, r, &ssoLinkAttempt{user.ID, session.TokenHash, user.PasswordHash})
}

func (s *Server) startProviderLogin(w http.ResponseWriter, r *http.Request, link *ssoLinkAttempt) {
	p, ok := s.provider(r)
	if !ok {
		s.writeError(w, 404, "Sign-in provider unavailable")
		return
	}
	if link != nil && p.InstallerManaged {
		s.writeError(w, 403, "Managed identities require explicit installer assignment")
		return
	}
	if !s.allowAttempt("sso:"+s.limitIP(r), 10, time.Minute) {
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
		// Keep memory bounded without letting abandoned attempts deny every newcomer.
		// All attempts have the same lifetime, so the earliest expiry is the oldest.
		oldest := ""
		var expiry time.Time
		for key, attempt := range s.logins {
			if oldest == "" || attempt.Expires.Before(expiry) {
				oldest, expiry = key, attempt.Expires
			}
		}
		delete(s.logins, oldest)
	}
	s.logins[state] = loginAttempt{Provider: p, Verifier: verifier, Nonce: nonce, BrowserHash: crypto.SHA256Hex([]byte(browser)), Expires: time.Now().Add(5 * time.Minute), Link: link}
	s.loginMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "ky_sso_" + state, Value: browser, Path: "/api/sso/" + p.ID + "/callback", MaxAge: 300, HttpOnly: true, Secure: s.config.Security.CookieSecure, SameSite: http.SameSiteLaxMode})
	if link != nil {
		s.writeJSON(w, 200, map[string]string{"authorization_url": dest})
		return
	}
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
	if p.InstallerManaged && (attempt.Link != nil || !reflect.DeepEqual(p, attempt.Provider)) {
		s.writeError(w, 403, "Sign-in configuration changed; start again")
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
	if attempt.Link != nil {
		link := attempt.Link
		user, session, err := s.sessions.AuthenticateRequest(r)
		if err != nil || user.ID != link.UserID || session.TokenHash != link.SessionHash {
			s.writeError(w, 403, "The original account session is required; start linking again")
			return
		}
		err = s.store.Users().LinkSSO(ctx, link.UserID, link.SessionHash, link.PasswordHash, p.ID, claims.Subject, s.requestIP(r))
		if errors.Is(err, store.ErrAlreadyExists) {
			s.writeError(w, 409, "This provider identity or account already has a different link")
			return
		}
		if err != nil {
			s.writeError(w, 403, "Account changed or linking was refused; start again")
			return
		}
		http.Redirect(w, r, "/settings?sso=linked", http.StatusFound)
		return
	}
	user, err := s.store.Users().GetUserBySSO(ctx, p.ID, claims.Subject)
	username := sso.LocalUsername(p.ID, claims.Subject, claims.PreferredUsername)
	if errors.Is(err, store.ErrNotFound) && p.AutoProvision {
		// Sign-in matches LOWER(username): an account by this name in any case is neither linked
		// to the provider's claim nor shadowed by a second one. The index closes the race.
		switch _, taken := s.store.Users().GetUserByUsername(ctx, username); {
		case taken == nil:
			err = store.ErrAlreadyExists
		case errors.Is(taken, store.ErrNotFound):
			user = &store.User{ID: "usr_" + crypto.RandomHex(12), Username: username, Email: claims.Email, DisplayName: claims.Name, Role: "user", Status: "active", SSOProvider: p.ID, SSOSubject: claims.Subject}
			err = s.store.Users().CreateUser(ctx, user)
		default:
			err = taken
		}
	}
	if errors.Is(err, store.ErrAlreadyExists) {
		name := protocol.CleanText(username, 64)
		s.auditPlatform(r, "", "auth.sso.refused", p.ID, "username="+name, "denied")
		s.writeError(w, 403, "The username "+name+" is taken by another account; ask your administrator")
		return
	}
	if err != nil || user == nil || user.Status != "active" {
		s.writeError(w, 403, "Account unavailable; contact your administrator")
		return
	}
	if p.InstallerManaged {
		current, managed, e := s.installerProvider(ctx)
		if e != nil || !managed || !reflect.DeepEqual(current, attempt.Provider) || claims.Issuer != current.Issuer {
			s.writeError(w, 403, "Managed sign-in configuration changed or identity mismatched")
			return
		}
		allowed, e := s.store.InstallerUserAllowed(ctx, user)
		if e != nil || !allowed {
			s.writeError(w, 403, "Identity is not assigned to this installation")
			return
		}
		admin := false
		for _, role := range claims.AppRoles {
			if role == "kyyard.admin" {
				admin = true
			}
		}
		if admin != (user.Role == "admin") {
			if e := s.store.RevokeInstallerIdentity(ctx, claims.Issuer, claims.Subject); e != nil {
				s.writeError(w, 503, "Could not record assignment change")
				return
			}
			s.writeError(w, 403, "Application role no longer matches the assigned identity")
			return
		}
	}
	ok = s.lockEnabledProvider(ctx, p.ID)
	defer s.providersMu.Unlock()
	if !ok {
		s.writeError(w, 404, "Sign-in provider unavailable")
		return
	}
	if user.SSOProvider == "local" && user.TOTPEnabled {
		token, err := s.beginMFA(ctx, user, p.ID, claims.Subject, claims.SessionID, claims.IssuedAt)
		if err != nil {
			s.writeError(w, 403, "Account unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		// The fragment is never sent in HTTP requests or referrers. Login removes
		// it from browser history and completes the existing TOTP/recovery flow.
		http.Redirect(w, r, "/#sso-mfa="+token, http.StatusFound)
		return
	}
	if _, _, err = s.sessions.IssueSSOSession(ctx, w, r, user, p.ID, claims.Subject, claims.SessionID, claims.IssuedAt); errors.Is(err, store.ErrSSOSessionEnded) {
		s.writeError(w, 401, "Sign-in already ended at the provider; sign in again")
		return
	} else if err != nil {
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
	if update.Enabled != nil && !*update.Enabled {
		if err := s.store.Sessions().EndSSOProvider(r.Context(), id); err != nil {
			s.writeError(w, 500, "Provider disabled, but its sessions could not be ended; disable it again")
			return
		}
	}
	_ = s.store.Audit().LogAudit(r.Context(), &store.AuditRecord{UserID: user.ID, Action: "sso.provider.update", Resource: id})
	s.writeJSON(w, 200, map[string]bool{"updated": true})
}

// handleAccountSSO lists the provider identities that sign in to the caller's local account,
// so its owner sees a link they did not make. A removed provider's identity is listed by ID.
func (s *Server) handleAccountSSO(w http.ResponseWriter, r *http.Request) {
	user, _, err := s.sessions.AuthenticateRequest(r)
	if err != nil {
		s.writeError(w, 401, "Authentication required")
		return
	}
	if user.SSOProvider != "local" || user.PasswordHash == "" {
		s.writeError(w, 403, "Linked sign-in applies to local accounts")
		return
	}
	ids, err := s.store.Users().ListSSOIdentities(r.Context(), user.ID)
	if err != nil {
		s.writeError(w, 500, "Could not load linked sign-in")
		return
	}
	providers, err := s.providers(r.Context())
	if err != nil {
		s.writeError(w, 500, "Could not load providers")
		return
	}
	names := map[string]string{}
	for _, p := range providers {
		names[p.ID] = p.Name
	}
	type identity struct {
		store.SSOIdentity
		Name string `json:"name"`
	}
	out := make([]identity, 0, len(ids))
	for _, id := range ids {
		name := names[id.Provider]
		if name == "" {
			name = id.Provider
		}
		out = append(out, identity{id, name})
	}
	s.writeJSON(w, 200, out)
}

// handleUnlinkSSO removes one linked identity after the same step-up as a password change,
// and ends the sessions it signed in.
func (s *Server) handleUnlinkSSO(w http.ResponseWriter, r *http.Request) {
	user, req, ok := s.accountUser(w, r, "sso-unlink:", 5, time.Minute)
	if !ok {
		return
	}
	if status, code, msg := s.stepUp(r.Context(), user, req.Password, req.Code); status != 0 {
		s.writeJSON(w, status, map[string]string{"error": msg, "code": code})
		return
	}
	err := s.store.Users().UnlinkSSO(r.Context(), user.ID, user.PasswordHash, r.PathValue("provider"), s.requestIP(r))
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, 404, "That sign-in is not linked to your account")
		return
	}
	if err != nil {
		s.writeError(w, 500, "Could not unlink sign-in")
		return
	}
	s.writeJSON(w, 200, map[string]bool{"unlinked": true})
}
