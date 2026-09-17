package store

import (
	"context"
	"errors"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"time"
)

var (
	ErrForbidden       = errors.New("tenant access denied")
	ErrInvalid         = errors.New("invalid tenant input")
	ErrNotFound        = errors.New("record not found")
	ErrAlreadyExists   = errors.New("record already exists")
	ErrLastAdmin       = errors.New("organization needs one active administrator")
	ErrInUse           = errors.New("record is still referenced")
	ErrRotationPending = errors.New("a rotated key is already awaiting review")
	ErrRotationBlocked = errors.New("rotation is blocked until a duplicate connection is cleared")
	ErrSessionExpired  = errors.New("session expired")
	ErrPairingExpired  = errors.New("pairing session expired")
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
	ListMembers(ctx context.Context, access TenantAccess, offset, limit int) ([]OrganizationMember, error)
	PutMembership(ctx context.Context, access TenantAccess, userID string, role TenantRole, status string) error
	RemoveMembership(ctx context.Context, access TenantAccess, userID string) error
	// ListMemberOrganizations returns the caller's own active memberships; it is not tenant-scoped.
	ListMemberOrganizations(ctx context.Context, userID string) ([]MemberOrganization, error)

	CreateEnrollmentToken(ctx context.Context, access TenantAccess, runtime, agentImage string) (*EnrollmentToken, error)
	// Enroll is agent-facing: the token, not a session, selects the tenant.
	Enroll(ctx context.Context, request EnrollmentRequest) (*Endpoint, error)
	ListEndpoints(ctx context.Context, access TenantAccess, offset, limit int) ([]Endpoint, error)
	ReadEndpoint(ctx context.Context, access TenantAccess, endpointID string) (*Endpoint, error)
	ApproveEndpoint(ctx context.Context, access TenantAccess, endpointID, fingerprint string) error
	RejectEndpoint(ctx context.Context, access TenantAccess, endpointID string) error
	RevokeEndpoint(ctx context.Context, access TenantAccess, endpointID string) error
	RenameEndpoint(ctx context.Context, access TenantAccess, endpointID, name string) error

	// Agent-facing lifecycle: authenticated by endpoint identity, never by a session.
	AgentIdentity(ctx context.Context, endpointID, fingerprint string) (*AgentIdentity, error)
	RotateEndpointKey(ctx context.Context, endpointID string, newPublicKey, signature []byte, ip string) (string, error)
	AcknowledgeEndpointKey(ctx context.Context, access TenantAccess, endpointID, fingerprint string) error
	RecordEndpointEvent(ctx context.Context, endpoint *Endpoint, severity, kind, details string) error
	AcknowledgeEndpointEvent(ctx context.Context, access TenantAccess, endpointID string, eventID int64) error
	SetEndpointCapabilities(ctx context.Context, endpointID string, capabilities []string) error
	ReadEndpointRaw(ctx context.Context, endpointID string) (*Endpoint, error)
	RecordAgentConnect(ctx context.Context, endpoint *Endpoint, ip, result, details string) error
	TouchEndpoint(ctx context.Context, endpointID string) error
	AcceptInventory(ctx context.Context, endpointID string, generation uint64, observedAt time.Time, snapshot []byte) (bool, error)
	ReadInventory(ctx context.Context, access TenantAccess, endpointID string) (*Inventory, error)
	RecordSamples(ctx context.Context, endpointID string, metrics protocol.Metrics) error
	LatestSamples(ctx context.Context, access TenantAccess, endpointID string) ([]SampleRow, error)
	ReadSamples(ctx context.Context, access TenantAccess, endpointID, containerID string, window time.Duration) ([]SampleRow, error)
	Prune(ctx context.Context) (int64, error)
	MarkEndpointOffline(ctx context.Context, endpointID string) error
	EndpointState(ctx context.Context, endpointID string) (string, error)

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
