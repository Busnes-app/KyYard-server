package store

import (
	"context"
	"errors"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"time"
)

var (
	ErrRevisionCorrupt  = errors.New("application revision digest mismatch")
	ErrRevisionConflict = errors.New("application revision changed")
	ErrApplicationLimit = errors.New("application storage limit reached")
	ErrForbidden        = errors.New("tenant access denied")
	ErrInvalid          = errors.New("invalid tenant input")
	ErrNotFound         = errors.New("record not found")
	ErrAlreadyExists    = errors.New("record already exists")
	ErrLastAdmin        = errors.New("organization needs one active administrator")
	// ErrEndpointOffline says the endpoint is not connected, so there is nowhere to send a
	// command. It is distinct from a bad request: the caller asked for something reasonable
	// that cannot happen right now.
	ErrEndpointOffline = errors.New("endpoint is not connected")
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
	// SampleCeiling is the stored metric rows allowed per endpoint.
	SampleCeiling() int
	Ping(ctx context.Context) error
	Close() error

	// Usage reports the bytes the database occupies and Budget the ceiling it is measured
	// against, zero when the check is disabled. Pressure says what that means for telemetry;
	// audit is never refused, whatever the pressure.
	Usage(ctx context.Context) (int64, error)
	Budget() int64
	EvaluatePressure(ctx context.Context) (Pressure, error)
	Pressure() Pressure
}

// Pressure is how close stored data is to its disk budget (docs/retention-policy.md).
type Pressure int32

const (
	// PressureNormal accepts everything.
	PressureNormal Pressure = iota
	// PressureDegraded refuses metrics: they are the cheapest telemetry to lose and the
	// fastest to grow. Inventory, heartbeats, endpoint state and audit continue.
	PressureDegraded
	// PressureStopped refuses inventory as well. Heartbeats, endpoint state and audit
	// continue, so the fleet stays visible and every refusal is still recorded.
	PressureStopped
)

func (p Pressure) String() string {
	switch p {
	case PressureDegraded:
		return "degraded"
	case PressureStopped:
		return "stopped"
	default:
		return "normal"
	}
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
	ImportApplication(ctx context.Context, access TenantAccess, name string, spec ApplicationSpec, values map[string]string, key []byte) (*Application, error)
	ResolveApplicationSecrets(ctx context.Context, access TenantAccess, applicationID string, number int, key []byte) (map[string]string, error)
	DiscardApplication(ctx context.Context, access TenantAccess, applicationID string, expectedRevision int) error
	CreateApplication(ctx context.Context, access TenantAccess, name string, spec ApplicationSpec) (*Application, error)
	AppendApplicationRevision(ctx context.Context, access TenantAccess, applicationID string, expectedRevision int, spec ApplicationSpec) (int, error)
	ListApplications(ctx context.Context, access TenantAccess, offset, limit int) ([]Application, error)
	ReadApplicationRevision(ctx context.Context, access TenantAccess, applicationID string, number int) (*ApplicationRevision, error)
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

	CheckEnrollmentAccess(ctx context.Context, access TenantAccess) error
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
	// RollUp summarises ended hours before the raw window drops them; ReadRollups serves the
	// week of history that summary buys.
	// Commands are durable intent: the row exists before the frame is sent, so an operation
	// the control plane loses track of can still be marked unknown rather than vanish.
	CreateCommand(ctx context.Context, access TenantAccess, endpointID, action, containerID, confirm string, expects protocol.Expectation) (*Command, error)
	CheckExecAccess(ctx context.Context, access TenantAccess, endpointID string) error
	OpenExecTarget(ctx context.Context, access TenantAccess, endpointID, streamID, confirm string, spec protocol.ExecSpec) (string, error)
	OpenLogTarget(ctx context.Context, access TenantAccess, endpointID, identifier string) (*LogTarget, error)
	StillAllowed(ctx context.Context, access TenantAccess, action permissions.Action, endpointID string) error
	MarkCommandDispatched(ctx context.Context, id string) error
	SettleCommand(ctx context.Context, endpointID, id, outcome, detail string) error
	AbandonCommands(ctx context.Context, endpointID string) (int64, error)
	ReadCommand(ctx context.Context, access TenantAccess, endpointID, id string) (*Command, error)
	ListCommands(ctx context.Context, access TenantAccess, endpointID string, limit int) ([]Command, error)
	RollUp(ctx context.Context, since time.Time) (int64, error)
	ReadRollups(ctx context.Context, access TenantAccess, endpointID, containerID string, window time.Duration) ([]RollupRow, error)
	Prune(ctx context.Context) (int64, error)
	MarkEndpointOffline(ctx context.Context, endpointID string) error
	EndpointState(ctx context.Context, endpointID string) (string, error)

	Initialize(ctx context.Context) error
	InitializeLocalDocker(ctx context.Context, publicKey []byte) (uint64, error)
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
