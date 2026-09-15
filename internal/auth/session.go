package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/Busness-app/ky_server_base/internal/config"
	"github.com/Busness-app/ky_server_base/internal/crypto"
	"github.com/Busness-app/ky_server_base/internal/store"
)

const (
	SessionCookieName = "ky_session"
	CSRFCookieName    = "ky_csrf"
	HeaderCSRF        = "X-CSRF-Token"
)

type SessionManager struct {
	store  store.Store
	config config.SecurityConfig
}

func NewSessionManager(st store.Store, cfg config.SecurityConfig) *SessionManager {
	return &SessionManager{
		store:  st,
		config: cfg,
	}
}

// IssueSession creates an active session in the database and writes HttpOnly cookie + CSRF token.
func (sm *SessionManager) IssueSession(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) (*store.Session, string, error) {
	rawToken := crypto.RandomHex(32)
	tokenHash := crypto.SHA256Hex([]byte(rawToken))

	ttl := sm.config.SessionTTL
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}

	sess := &store.Session{
		TokenHash: tokenHash,
		UserID:    userID,
		UserAgent: r.UserAgent(),
		IPAddress: ClientIP(r, sm.config.TrustedProxies),
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(ttl),
	}

	if err := sm.store.Sessions().CreateSession(ctx, sess); err != nil {
		return nil, "", err
	}

	// Set HttpOnly session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    rawToken,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		HttpOnly: true,
		Secure:   sm.config.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Domain:   sm.config.CookieDomain,
	})

	// Generate CSRF token for state-modifying endpoints
	csrfToken := crypto.RandomHex(24)
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    csrfToken,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		HttpOnly: false, // Read by JS to put into X-CSRF-Token header
		Secure:   sm.config.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Domain:   sm.config.CookieDomain,
	})

	return sess, rawToken, nil
}

// AuthenticateRequest extracts the session token from cookie or Authorization header and returns the user.
func (sm *SessionManager) AuthenticateRequest(r *http.Request) (*store.User, *store.Session, error) {
	var rawToken string

	// 1. Check Bearer token header
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		rawToken = strings.TrimPrefix(authHeader, "Bearer ")
	}

	// 2. Fallback to HttpOnly cookie
	if rawToken == "" {
		if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
			rawToken = cookie.Value
		}
	}

	if rawToken == "" {
		return nil, nil, store.ErrNotFound
	}

	tokenHash := crypto.SHA256Hex([]byte(rawToken))
	sess, err := sm.store.Sessions().GetSession(r.Context(), tokenHash)
	if err != nil {
		return nil, nil, err
	}

	user, err := sm.store.Users().GetUserByID(r.Context(), sess.UserID)
	if err != nil {
		return nil, nil, err
	}
	if user.Status != "active" {
		_ = sm.store.Sessions().DeleteSession(r.Context(), tokenHash)
		return nil, nil, store.ErrNotFound
	}

	return user, sess, nil
}

func ValidateCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(CSRFCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	header := r.Header.Get(HeaderCSRF)
	return len(header) == len(cookie.Value) && subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) == 1
}

// RevokeSession deletes the active session and clears the browser cookies.
func (sm *SessionManager) RevokeSession(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var rawToken string
	if cookie, err := r.Cookie(SessionCookieName); err == nil {
		rawToken = cookie.Value
	}

	if rawToken != "" {
		tokenHash := crypto.SHA256Hex([]byte(rawToken))
		_ = sm.store.Sessions().DeleteSession(ctx, tokenHash)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   sm.config.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})

	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		HttpOnly: false,
		Secure:   sm.config.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})

	return nil
}
