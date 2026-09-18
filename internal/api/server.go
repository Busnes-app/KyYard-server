package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/sso"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/web"
)

// recoveryClient is the KyRecovery client as the handlers use it, narrowed so tests can stand
// in a fake without reaching the network.
type recoveryClient interface {
	ClaimPairing(ctx context.Context, serverURL, pairingCode, serviceName, appName string) (recoveryclient.PairingResult, error)
	recoveryclient.Depositor
}

type Server struct {
	providersMu sync.Mutex
	loginMu     sync.Mutex
	logins      map[string]loginAttempt
	config      *config.Config
	store       store.Store
	sessions    *auth.SessionManager
	kysignon    *sso.KySignOnClient
	saml        *sso.SAMLServiceProvider
	recovery    recoveryClient
	mux         *http.ServeMux
	attemptsMu  sync.Mutex
	attempts    map[string]attemptWindow
	// detached counts the requests running on a context deliberately separated from their
	// connection. http.Server.Shutdown does not know about them, so runServer waits on this
	// before the store closes.
	detached detachedCounter
	stopping atomic.Bool
	agents   agentRegistry
	logs     *logRegistry
	execs    execRegistry
}

// detachedCounter is a WaitGroup that tolerates a registration arriving while the wait is
// already running. sync.WaitGroup panics on an Add from zero concurrent with Wait, and there is
// no barrier that rules that out here: Shutdown returns when its own timeout expires, with
// requests still in flight, so a second admin request can register just as the first finishes
// and drops the count to zero. A counter under a condition variable has no such rule.
type detachedCounter struct {
	once sync.Once
	mu   sync.Mutex
	cond *sync.Cond
	n    int
}

// signal builds the condition variable on first use, so the zero value of Server works.
func (d *detachedCounter) signal() *sync.Cond {
	d.once.Do(func() { d.cond = sync.NewCond(&d.mu) })
	return d.cond
}

func (d *detachedCounter) add() {
	c := d.signal()
	c.L.Lock()
	d.n++
	c.L.Unlock()
}

func (d *detachedCounter) done() {
	c := d.signal()
	c.L.Lock()
	d.n--
	c.L.Unlock()
	c.Broadcast()
}

// tracked counts a request as detached for as long as h runs. It wraps the auth middleware
// rather than the handler: requireAdmin authenticates against the store before the handler is
// reached, ReadTimeout (15s) outlasts cmd/server's shutdownTimeout (5s), and a SIGTERM landing
// during that lookup would otherwise leave the counter at zero, WaitDetached returning and the
// store closing under a request about to pin a key.
//
// The window before ServeHTTP is entered -- while net/http is still reading the request line
// and headers -- cannot be covered by any counter: there is no handler goroutine to register
// yet. Shutdown's own drain is all that covers it, which is why shutdownTimeout is spent first.
func (s *Server) tracked(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.detached.add()
		defer s.detached.done()
		h(w, r)
	}
}

// WaitDetached blocks until every request that detached from its connection has finished. It is
// called after http.Server.Shutdown and before the store is closed: pairing, the key pin and a
// deposit all keep writing after their connection is gone, and a closed store under them leaves
// a key pinned on disk with no row recording it.
func (s *Server) WaitDetached() {
	c := s.detached.signal()
	c.L.Lock()
	defer c.L.Unlock()
	for s.detached.n > 0 {
		c.Wait()
	}
}

type attemptWindow struct {
	count int
	reset time.Time
}

// attemptsCap bounds the limiter map. Unauthenticated callers influence the keys, so the map
// is itself attack surface. At the cap we evict, never refuse: refusing every unknown key
// would let one caller fill the map and lock every new client out of login.
//
// The trade-off: memory is bounded, but an attacker who fills the map shortens other clients'
// windows, since an evicted counter starts again from zero. That weakens throttling while the
// attack runs; it never locks anyone out, which is the failure mode worth avoiding.
//
// Eviction is deliberately blind to how much of a window is left. Picking the entry nearest to
// expiry would always sacrifice the shortest windows first, so a caller minting keys with a
// long window could keep the one-minute login counter from ever reaching its limit. Every key
// is therefore equally likely to go. The real defence is that no key carries caller-supplied
// bytes, so filling the map costs an attacker one slot per IP.
const attemptsCap = 10000

func NewServer(cfg *config.Config, st store.Store) *Server {
	sessions := auth.NewSessionManager(st, cfg.Security)
	kysignon := sso.NewKySignOnClient(cfg.SSO, st)
	saml := sso.NewSAMLServiceProvider(cfg.SSO.SAMLEntityID, cfg.Server.AppURL+"/saml/acs")
	recovery := recoveryclient.NewClient(recoveryclient.Options{AllowPrivate: cfg.Backup.AllowPrivateRecovery})

	s := &Server{
		config:   cfg,
		store:    st,
		sessions: sessions,
		kysignon: kysignon,
		saml:     saml,
		recovery: recovery,
		mux:      http.NewServeMux(),
		logs:     newLogRegistry(),
		attempts: make(map[string]attemptWindow),
	}

	s.routes()
	return s
}

func (s *Server) allowAttempt(key string, limit int, window time.Duration) bool {
	now := time.Now()
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()
	if _, known := s.attempts[key]; !known && len(s.attempts) >= attemptsCap {
		s.makeRoom(now)
	}
	entry := bumpWindow(s.attempts[key], now, window)
	s.attempts[key] = entry
	return entry.count <= limit
}

// makeRoom frees a slot for a new key: it drops every expired window, and if the map is still
// full it drops one live entry chosen at random, never the one nearest expiry. Caller holds
// attemptsMu. The scan is O(attemptsCap) and only runs for a new key while the map is full;
// 10 000 entries is microseconds.
func (s *Server) makeRoom(now time.Time) {
	for candidate, w := range s.attempts {
		if now.After(w.reset) {
			delete(s.attempts, candidate)
		}
	}
	if len(s.attempts) >= attemptsCap {
		// Go randomises map iteration, so the first entry is an unbiased victim.
		for candidate := range s.attempts {
			delete(s.attempts, candidate)
			break
		}
	}
}

func bumpWindow(entry attemptWindow, now time.Time, window time.Duration) attemptWindow {
	if now.After(entry.reset) {
		entry = attemptWindow{reset: now.Add(window)}
	}
	entry.count++
	return entry
}

// requestIP is the limiter's key for unauthenticated routes. It resolves to the same address
// a session is bound to, and honours X-Forwarded-For only from a configured trusted proxy:
// keying on a caller-supplied header would make every limit here bypassable.
func (s *Server) requestIP(r *http.Request) string {
	return auth.ClientIP(r, s.config.Security.TrustedProxies)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/organizations/{organization}", s.tenantRoute(s.handleTenantOrganization))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/instances", s.tenantRoute(s.handleApplicationInstances))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/applications", s.tenantRoute(s.handleApplicationInstances))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/{application}/preflight", s.tenantRoute(s.handleApplicationPreflight))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/{application}/mapping", s.tenantRoute(s.handleApplicationMapping))
	s.mux.HandleFunc("PUT /api/organizations/{organization}/environments/{environment}/applications/{application}/mapping", s.tenantRoute(s.handleSetApplicationMapping))
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications/{application}/revisions", s.tenantRoute(s.handleReplaceApplicationRevision))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/{application}/comparison", s.tenantRoute(s.handleApplicationComparison))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/{application}/adoption", s.tenantRoute(s.handleAdoptionPreview))
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications/{application}/adoption", s.tenantRoute(s.handleAdoption))
	s.mux.HandleFunc("DELETE /api/organizations/{organization}/environments/{environment}/applications/{application}/adoption", s.tenantRoute(s.handleReleaseApplication))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications", s.tenantRoute(s.handleApplications))
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications", s.tenantRoute(s.handleImportApplication))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/{application}/revisions/{revision}", s.tenantRoute(s.handleApplicationRevision))
	s.mux.HandleFunc("DELETE /api/organizations/{organization}/environments/{environment}/applications/{application}", s.tenantRoute(s.handleDiscardApplication))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments", s.tenantRoute(s.handleTenantEnvironments))
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments", s.tenantRoute(s.handleCreateEnvironment))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}", s.tenantRoute(s.handleTenantEnvironment))
	s.mux.HandleFunc("PATCH /api/organizations/{organization}/environments/{environment}", s.tenantRoute(s.handleUpdateEnvironment))
	s.mux.HandleFunc("DELETE /api/organizations/{organization}/environments/{environment}", s.tenantRoute(s.handleRemoveEnvironment))
	s.mux.HandleFunc("GET /api/organizations/{organization}/audit", s.tenantRoute(s.handleTenantAudit))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/audit", s.tenantRoute(s.handleTenantAudit))
	s.mux.HandleFunc("GET /api/organizations/{organization}/members", s.tenantRoute(s.handleTenantMembers))
	s.mux.HandleFunc("PUT /api/organizations/{organization}/members/{user}", s.tenantRoute(s.handlePutMembership))
	s.mux.HandleFunc("DELETE /api/organizations/{organization}/members/{user}", s.tenantRoute(s.handleRemoveMembership))
	s.mux.HandleFunc("GET /api/organizations", s.handleMyOrganizations)
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/enrollment-tokens", s.tenantRoute(s.handleCreateEnrollmentToken))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints", s.tenantRoute(s.handleTenantEndpoints))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/endpoints", s.tenantRoute(s.handleTenantEndpoints))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}", s.tenantRoute(s.handleTenantEndpoint))
	s.mux.HandleFunc("PATCH /api/organizations/{organization}/endpoints/{endpoint}", s.tenantRoute(s.handleRenameEndpoint))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/inventory", s.tenantRoute(s.handleEndpointInventory))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/samples", s.tenantRoute(s.handleLatestSamples))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/containers/{container}/samples", s.tenantRoute(s.handleContainerSamples))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/containers/{container}/rollups", s.tenantRoute(s.handleContainerRollups))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/containers/{container}/removal", s.tenantRoute(s.handleRemovalPreview))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/commands", s.tenantRoute(s.handleDispatchCommand))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/commands", s.tenantRoute(s.handleListCommands))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/commands/{command}", s.tenantRoute(s.handleReadCommand))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/containers/{container}/logs", s.tenantRoute(s.handleContainerLogs))
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/containers/{container}/exec", s.tracked(s.tenantRoute(s.handleContainerExec)))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/approve", s.tenantRoute(s.handleApproveEndpoint))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/reject", s.tenantRoute(s.endpointTransition(s.store.Tenancy().RejectEndpoint)))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/revoke", s.tenantRoute(s.endpointTransition(s.store.Tenancy().RevokeEndpoint)))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/keys/{fingerprint}/acknowledge", s.tenantRoute(s.handleAcknowledgeKey))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/events/{event}/acknowledge", s.tenantRoute(s.handleAcknowledgeEvent))
	s.mux.HandleFunc("POST /api/agent/v1/enroll", s.handleAgentEnroll)
	s.mux.HandleFunc("GET /api/agent/v1/connect", s.handleAgentConnect)
	s.mux.HandleFunc("/api/agent/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, http.StatusNotFound, "Agent route not found")
	})
	s.mux.HandleFunc("/api/organizations/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, http.StatusNotFound, "Tenant route not found")
	})

	// Public, secret-free probes; readiness includes the database.
	s.mux.HandleFunc("/health/live", s.handleHealth)
	s.mux.HandleFunc("/health/ready", s.handleHealth)

	// Auth
	s.mux.HandleFunc("/api/auth/pow-challenge", s.handlePoWChallenge)
	s.mux.HandleFunc("/api/auth/login", s.handleLogin)
	s.mux.HandleFunc("/api/auth/mfa/totp", s.handleMFATOTP)
	s.mux.HandleFunc("/api/auth/mfa/recovery-code", s.handleMFARecovery)
	s.mux.HandleFunc("/api/auth/logout", s.handleLogout)
	s.mux.HandleFunc("/api/auth/me", s.handleMe)
	s.mux.HandleFunc("/api/auth/change-password", s.handleChangePassword)

	// SSO
	s.mux.HandleFunc("GET /api/sso/{provider}/login", s.handleProviderLogin)
	s.mux.HandleFunc("GET /api/sso/{provider}/callback", s.handleProviderCallback)
	s.mux.HandleFunc("/api/sso/kysignon/sync", s.handleKySignOnSyncWebhook)
	s.mux.HandleFunc("/saml/metadata", s.handleSAMLMetadata)

	// Retired mobile pairing namespace.
	s.mux.HandleFunc("/api/devices/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, http.StatusNotFound, "Mobile pairing is not available in KyYard")
	})

	// Feature 0 KyBackup & Restore Drills. Capsules carry site data and keys: admins only.
	// Method patterns: only the declared method reaches a handler. Export is a POST so the
	// CSRF check covers a download that carries the whole instance.
	s.mux.HandleFunc("POST /api/backup/drill", s.requireAdmin(s.handleBackupDrill))
	s.mux.HandleFunc("POST /api/backup/export-capsule", s.requireAdmin(s.handleExportCapsule))
	s.mux.HandleFunc("POST /api/backup/pair-remote", s.tracked(s.requireAdmin(s.handlePairRemoteRecovery)))
	s.mux.HandleFunc("POST /api/backup/deposit", s.tracked(s.requireAdmin(s.handleRunBackup)))
	s.mux.HandleFunc("DELETE /api/backup/pairing", s.requireAdmin(s.handleUnpair))
	s.mux.HandleFunc("POST /api/backup/pin-key", s.tracked(s.requireAdmin(s.handlePinKey)))
	s.mux.HandleFunc("PUT /api/backup/schedule", s.requireAdmin(s.handleSetSchedule))
	s.mux.HandleFunc("GET /api/backup/status", s.requireAdmin(s.handleBackupStatus))

	s.mux.HandleFunc("GET /api/settings/sso", s.requireAdmin(s.handleProviders))
	s.mux.HandleFunc("POST /api/settings/sso", s.requireAdmin(s.handleSaveProvider))
	s.mux.HandleFunc("PUT /api/settings/sso/{provider}", s.requireAdmin(s.handleUpdateProvider))
	s.mux.HandleFunc("DELETE /api/settings/sso/{provider}", s.requireAdmin(s.handleDeleteProvider))
	// Settings & Theme. The read endpoint tiers its own payload by role.
	s.mux.HandleFunc("/api/settings", s.handleGetSettings)
	s.mux.HandleFunc("/api/settings/theme", s.requireAdmin(s.handleSetTheme))

	// Embedded React PWA Frontend
	s.mux.Handle("/", web.Handler())
}

// requireAdmin rejects requests without a valid session, or with a non-admin one.
func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, _, err := s.sessions.AuthenticateRequest(r)
		if err != nil {
			if errors.Is(err, auth.ErrPasswordChangeRequired) {
				s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "Change your password before continuing", "code": "password_change_required"})
			} else {
				s.writeError(w, http.StatusUnauthorized, "Authentication required")
			}
			return
		}
		if !permissions.PlatformAllows(user.Role, permissions.PlatformAdmin) {
			s.writeError(w, http.StatusForbidden, "Administrator role required")
			return
		}
		h(w, r)
	}
}

func (s *Server) requireAuthenticated(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := s.sessions.AuthenticateRequest(r); err != nil {
			if errors.Is(err, auth.ErrPasswordChangeRequired) {
				s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "Change your password before continuing", "code": "password_change_required"})
			} else {
				s.writeError(w, http.StatusUnauthorized, "Authentication required")
			}
			return
		}
		h(w, r)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
	if s.config.Security.CookieSecure {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}

	origin := r.Header.Get("Origin")
	if origin != "" && sameOrigin(origin, s.config.Server.AppURL) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-CSRF-Token, X-KySignOn-Signature")

	if r.Method == http.MethodOptions {
		if origin != "" && !sameOrigin(origin, s.config.Server.AppURL) {
			http.Error(w, "Origin not allowed", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	if isUnsafeMethod(r.Method) && origin != "" && !sameOrigin(origin, s.config.Server.AppURL) {
		s.writeError(w, http.StatusForbidden, "Origin not allowed; use the configured KY_APP_URL")
		return
	}
	if isUnsafeMethod(r.Method) && hasSessionCookie(r) && !csrfExempt(r.URL.Path) && !auth.ValidateCSRF(r) {
		s.writeError(w, http.StatusForbidden, "Invalid CSRF token")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	}

	if !s.config.SSO.Enabled && (r.URL.Path == "/api/sso/kysignon/sync" || strings.HasPrefix(r.URL.Path, "/saml/")) {
		s.writeError(w, http.StatusNotFound, "SSO is disabled")
		return
	}

	// Retired SCIM namespace.
	if strings.HasPrefix(r.URL.Path, "/scim/v2") {
		s.writeError(w, http.StatusNotFound, "SCIM is not available in KyYard")
		return
	}

	s.mux.ServeHTTP(w, r)
}

func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func hasSessionCookie(r *http.Request) bool {
	cookie, err := r.Cookie(auth.SessionCookieName)
	return err == nil && cookie.Value != ""
}

func csrfExempt(path string) bool {
	return path == "/api/auth/login" || strings.HasPrefix(path, "/api/auth/mfa/") || path == "/api/sso/kysignon/sync"
}

func sameOrigin(origin, appURL string) bool {
	a, err := url.Parse(appURL)
	if err != nil || a.Scheme == "" || a.Host == "" {
		return false
	}
	o, err := url.Parse(origin)
	return err == nil && o.User == nil && o.Path == "" && o.RawQuery == "" && !o.ForceQuery && o.Fragment == "" && o.Scheme == a.Scheme && o.Host == a.Host
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	s.writeJSON(w, status, map[string]string{"error": message})
}
