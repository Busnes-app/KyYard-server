package store

import (
	"context"
	"errors"
)

var (
	ErrForbidden      = errors.New("tenant access denied")
	ErrInvalid        = errors.New("invalid tenant input")
	ErrNotFound       = errors.New("record not found")
	ErrAlreadyExists  = errors.New("record already exists")
	ErrSessionExpired = errors.New("session expired")
	ErrPairingExpired = errors.New("pairing session expired")
)

// Store defines the unified storage contract implemented across SQLite, PostgreSQL, and MySQL.
type Store interface {
	Tenancy() TenancyStore
	Users() UserStore
	Sessions() SessionStore
	Devices() DeviceStore
	Groups() GroupStore
	Audit() AuditStore
	Settings() SettingsStore

	Driver() string
	Ping(ctx context.Context) error
	Close() error
}

// UserStore defines repository operations for accounts.
type UserStore interface {
	CreateUser(ctx context.Context, u *User) error
	GetUserByID(ctx context.Context, id string) (*User, error)
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	GetUserBySSO(ctx context.Context, provider, subject string) (*User, error)
	UpdateUser(ctx context.Context, u *User) error
	ResetAdminPassword(ctx context.Context, userID, newHash string) error
	CompletePasswordChange(ctx context.Context, userID, oldHash, newHash, ip string) error
	UpdateRecoveryCodes(ctx context.Context, userID, oldHashes, newHashes string) error
	// SpendTOTPCounter records counter as used. It returns ErrAlreadyExists when counter is
	// not greater than the stored one, which is how a replayed code inside the skew window fails.
	SpendTOTPCounter(ctx context.Context, userID string, counter int64) error
	DeleteUser(ctx context.Context, id string) error
	ListUsers(ctx context.Context, offset, limit int, search string) ([]*User, int, error)
	CountUsers(ctx context.Context) (int, error)
}

// SessionStore defines repository operations for active login sessions.
type SessionStore interface {
	CreateSession(ctx context.Context, s *Session, expectedPasswordHash string) error
	GetSession(ctx context.Context, tokenHash string) (*Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteUserSessions(ctx context.Context, userID string) error
	CleanExpiredSessions(ctx context.Context) error
	CreateMFAChallenge(ctx context.Context, challenge *MFAChallenge, expectedPasswordHash string) error
	ConsumeMFAChallenge(ctx context.Context, tokenHash string) (userID, passwordHash string, err error)
}

// DeviceStore handles 90s ephemeral QR pairing sessions and paired push clients.
type DeviceStore interface {
	CreatePairing(ctx context.Context, p *DevicePairing) error
	GetPairingByCode(ctx context.Context, code string) (*DevicePairing, error)
	GetPairingBySecret(ctx context.Context, secret string) (*DevicePairing, error)
	ConsumePairing(ctx context.Context, secret, deviceName, platform, pushToken string) error
	CleanExpiredPairings(ctx context.Context) error
}

// GroupStore defines repository operations for SCIM and RBAC groups.
type GroupStore interface {
	CreateGroup(ctx context.Context, g *Group) error
	GetGroupByID(ctx context.Context, id string) (*Group, error)
	GetGroupByName(ctx context.Context, name string) (*Group, error)
	UpdateGroup(ctx context.Context, g *Group) error
	DeleteGroup(ctx context.Context, id string) error
	ListGroups(ctx context.Context, offset, limit int) ([]*Group, int, error)
	AddGroupMember(ctx context.Context, groupID, userID string) error
	RemoveGroupMember(ctx context.Context, groupID, userID string) error
	GetUserGroups(ctx context.Context, userID string) ([]*Group, error)
}

// AuditStore logs security events.
type AuditStore interface {
	LogAudit(ctx context.Context, r *AuditRecord) error
	ListAuditRecords(ctx context.Context, offset, limit int) ([]*AuditRecord, int, error)
}

// SettingsStore handles persistent key-value configuration.
type SettingsStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, val string) error
	DeleteSetting(ctx context.Context, key string) error
	GetAllSettings(ctx context.Context) (map[string]string, error)
}

// TenancyStore provides authorized operations through TenantAccess. The raw methods
// below Initialize are trusted persistence helpers for bootstrap and management.
type TenancyStore interface {
	ReadOrganization(ctx context.Context, access TenantAccess) (*Organization, error)
	ReadEnvironment(ctx context.Context, access TenantAccess) (*Environment, error)
	ListEnvironments(ctx context.Context, access TenantAccess, offset, limit int) ([]Environment, error)
	AddEnvironment(ctx context.Context, access TenantAccess, name string) (*Environment, error)
	UpdateEnvironment(ctx context.Context, access TenantAccess, name string) error
	RemoveEnvironment(ctx context.Context, access TenantAccess) error
	ReadAudit(ctx context.Context, access TenantAccess, offset, limit int) ([]AuditRecord, error)

	Initialize(ctx context.Context) error
	CreateOrganization(ctx context.Context, organization *Organization) error
	GetOrganization(ctx context.Context, organizationID string) (*Organization, error)
	SetMembership(ctx context.Context, membership *OrganizationMembership) error
	GetMembership(ctx context.Context, organizationID, userID string) (*OrganizationMembership, error)
	DeleteMembership(ctx context.Context, organizationID, userID string) error
	CreateEnvironment(ctx context.Context, environment *Environment) error
	GetEnvironment(ctx context.Context, organizationID, environmentID string) (*Environment, error)
	RenameEnvironment(ctx context.Context, organizationID, environmentID, name string) error
	DeleteEnvironment(ctx context.Context, organizationID, environmentID string) error
	CreateOrganizationGroup(ctx context.Context, group *OrganizationGroup) error
	AddOrganizationGroupMember(ctx context.Context, organizationID, groupID, userID string) error
	ListOrganizationGroupMembers(ctx context.Context, organizationID, groupID string) ([]string, error)
	RemoveOrganizationGroupMember(ctx context.Context, organizationID, groupID, userID string) error
}
