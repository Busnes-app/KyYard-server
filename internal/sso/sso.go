package sso

import (
	"errors"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
)

var (
	ErrInvalidState       = errors.New("invalid or expired SSO state")
	ErrInvalidIDToken     = errors.New("invalid ID token")
	ErrProviderDisabled   = errors.New("SSO provider is not enabled")
	ErrInvalidLogoutToken = errors.New("invalid logout token")
)

// IdentityClaims holds the unified identity extracted from any SSO provider.
type IdentityClaims struct {
	Issuer            string   `json:"iss"`
	AppRoles          []string `json:"roles,omitempty"`
	Subject           string   `json:"sub"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Role              string   `json:"role,omitempty"`
	Provider          string   `json:"provider"` // "kysignon", "oidc", "saml"
	// SessionID is the ID token `sid`; back-channel logout names the login by it.
	SessionID string `json:"sid,omitempty"`
	// IssuedAt is the ID token's iat: when the provider vouched for this login.
	IssuedAt time.Time `json:"-"`
}

// SSOState represents ephemeral state held during OAuth/OIDC authorization flow.
type SSOState struct {
	State        string    `json:"state"`
	Verifier     string    `json:"verifier"`
	Provider     string    `json:"provider"`
	RedirectBack string    `json:"redirect_back"`
	CreatedAt    time.Time `json:"created_at"`
}

// LocalUsername is the account name for a provider's claimed username: the claim when it passes
// auth.ValidateUsername, otherwise "sso_" and a hash of provider and subject, stable per identity.
func LocalUsername(provider, subject, claimed string) string {
	if auth.ValidateUsername(claimed) == nil {
		return claimed
	}
	return "sso_" + crypto.SHA256Hex([]byte(provider + "\x00" + subject))[:16]
}
