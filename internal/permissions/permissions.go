package permissions

// Action names are stable audit identifiers, not caller-supplied role checks.
type Action string

const (
	PlatformAdmin     Action = "platform.admin"
	OrganizationRead  Action = "organization.read"
	MembersManage     Action = "organization.members.manage"
	EnvironmentRead   Action = "environment.read"
	EnvironmentCreate Action = "environment.create"
	EnvironmentUpdate Action = "environment.update"
	EnvironmentDelete Action = "environment.delete"
	AuditRead         Action = "organization.audit.read"
)

func PlatformAllows(role string, action Action) bool {
	return role == "admin" && action == PlatformAdmin
}

func Allows(role string, action Action) bool {
	switch role {
	case "organization_admin":
		switch action {
		case OrganizationRead, MembersManage, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, AuditRead:
			return true
		}
	case "environment_admin":
		switch action {
		case OrganizationRead, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete:
			return true
		}
	case "operator", "developer", "read_only":
		return action == OrganizationRead || action == EnvironmentRead
	}
	return false
}
