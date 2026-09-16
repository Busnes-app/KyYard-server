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
	EndpointRead      Action = "endpoint.read"
	EndpointEnroll    Action = "endpoint.enroll"
	EndpointUpdate    Action = "endpoint.update"
	EndpointRevoke    Action = "endpoint.revoke"
)

func PlatformAllows(role string, action Action) bool {
	return role == "admin" && action == PlatformAdmin
}

func Allows(role string, action Action) bool {
	switch role {
	case "organization_admin":
		switch action {
		case OrganizationRead, MembersManage, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, AuditRead, EndpointRead, EndpointEnroll, EndpointUpdate, EndpointRevoke:
			return true
		}
	case "environment_admin":
		switch action {
		case OrganizationRead, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, EndpointRead, EndpointEnroll, EndpointUpdate, EndpointRevoke:
			return true
		}
	case "operator", "developer", "read_only":
		return action == OrganizationRead || action == EnvironmentRead || action == EndpointRead
	}
	return false
}
