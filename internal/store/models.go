package store

import (
	"time"
)

// User represents an identity within the system.
type User struct {
	ID                 string     `json:"id"`
	Username           string     `json:"username"`
	Email              string     `json:"email"`
	DisplayName        string     `json:"display_name"`
	PasswordHash       string     `json:"-"`            // Never serialized to JSON
	Role               string     `json:"role"`         // "admin", "user", "manager"
	Status             string     `json:"status"`       // "active", "suspended", "inactive"
	SSOProvider        string     `json:"sso_provider"` // "local", "kysignon", "oidc", "saml", "scim"
	SSOSubject         string     `json:"sso_subject,omitempty"`
	TOTPSecretEnc      string     `json:"-"` // AES-256-GCM encrypted
	TOTPEnabled        bool       `json:"totp_enabled"`
	TOTPLastCounter    int64      `json:"-"` // last RFC 6238 counter accepted; refuses replay inside the skew window
	RecoveryCodesHash  string     `json:"-"` // JSON array of sha256 hashes
	PushDeviceID       string     `json:"push_device_id,omitempty"`
	MustChangePassword bool       `json:"must_change_password"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
}

// Session represents an active authenticated user session.
type Session struct {
	TokenHash string    `json:"token_hash"`
	UserID    string    `json:"user_id"`
	UserAgent string    `json:"user_agent"`
	IPAddress string    `json:"ip_address"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type MFAChallenge struct {
	TokenHash string
	UserID    string
	ExpiresAt time.Time
}

// DevicePairing represents a 90-second ephemeral session to link mobile/PWA wrappers.
type DevicePairing struct {
	// Code, Secret and PushToken never serialise: this record is reached by unauthenticated
	// pair/verify and pair/poll callers. A handler that must return one needs its own type.
	Code       string    `json:"-"` // 6-digit verification code
	Secret     string    `json:"-"` // Ephemeral secret for exchange
	UserID     string    `json:"user_id,omitempty"`
	DeviceName string    `json:"device_name,omitempty"`
	Platform   string    `json:"platform,omitempty"` // "android", "ios", "pwa", "desktop"
	PushToken  string    `json:"-"`
	Status     string    `json:"status"` // "pending", "approved", "consumed", "expired"
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Group represents a SCIM/RBAC user group.
type Group struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	ExternalID  string    `json:"external_id,omitempty"`
	Members     []string  `json:"members,omitempty"` // User IDs
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AuditRecord logs security and operational events with tamper-evident structure.
type AuditRecord struct {
	ID        int64     `json:"id"`
	UserID    string    `json:"user_id"`
	Action    string    `json:"action"` // e.g. "auth.login", "scim.user_created"
	Resource  string    `json:"resource"`
	Details   string    `json:"details,omitempty"`
	IPAddress string    `json:"ip_address"`
	CreatedAt time.Time `json:"created_at"`
}

// Setting represents a durable server-wide key-value configuration entry.
type Setting struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}
