package sso

import (
	"errors"
	"time"
)

var (
	ErrInvalidState     = errors.New("invalid or expired SSO state")
	ErrInvalidIDToken   = errors.New("invalid ID token")
	ErrProviderDisabled = errors.New("SSO provider is not enabled")
)

// IdentityClaims holds the unified identity extracted from any SSO provider.
type IdentityClaims struct {
	Subject           string `json:"sub"`
	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Role              string `json:"role,omitempty"`
	Provider          string `json:"provider"` // "kysignon", "oidc", "saml"
}

// SSOState represents ephemeral state held during OAuth/OIDC authorization flow.
type SSOState struct {
	State        string    `json:"state"`
	Verifier     string    `json:"verifier"`
	Provider     string    `json:"provider"`
	RedirectBack string    `json:"redirect_back"`
	CreatedAt    time.Time `json:"created_at"`
}
