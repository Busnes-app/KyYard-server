package permissions

// Action names are stable audit identifiers, not caller-supplied role checks.
type Action string

const (
	SecretReveal       Action = "secret.reveal"
	ApplicationRead    Action = "application.read"
	ApplicationImport  Action = "application.import"
	ApplicationEdit    Action = "application.edit"
	ApplicationDestroy Action = "application.destroy"
	PlatformAdmin      Action = "platform.admin"
	OrganizationRead   Action = "organization.read"
	MembersManage      Action = "organization.members.manage"
	EnvironmentRead    Action = "environment.read"
	EnvironmentCreate  Action = "environment.create"
	EnvironmentUpdate  Action = "environment.update"
	EnvironmentDelete  Action = "environment.delete"
	AuditRead          Action = "organization.audit.read"
	EndpointRead       Action = "endpoint.read"
	EndpointEnroll     Action = "endpoint.enroll"
	EndpointUpdate     Action = "endpoint.update"
	EndpointRevoke     Action = "endpoint.revoke"
	// ContainerOperate is start, stop and restart: reversible lifecycle actions on a
	// container that already exists. Destroying one is a separate action, because undoing it
	// is not a matter of running the opposite command.
	ContainerOperate Action = "container.operate"
	// ContainerDestroy removes a container. It is separate from operating one because the
	// opposite command does not undo it, and the matrix gives it to administrators only.
	ContainerDestroy Action = "container.destroy"
	// ImagePull fetches an image onto an endpoint. It is not destructive, but it spends the
	// host's disk and bandwidth and will later spend a registry credential, so the matrix
	// stops at the operator.
	ImagePull Action = "image.pull"
	// ImageDestroy removes an image from an endpoint.
	ImageDestroy Action = "image.destroy"
	// ContainerLogs reads what a container has written. It is separate from reading a
	// container because a log is the application's own output: it carries whatever the
	// workload prints, which is where credentials and customer data turn up, so the matrix
	// stops it at the developer and audits every session.
	ContainerLogs Action = "container.logs"
	ContainerExec Action = "container.exec"
)

func PlatformAllows(role string, action Action) bool {
	return role == "admin" && action == PlatformAdmin
}

func Allows(role string, action Action) bool {
	switch role {
	case "organization_admin":
		switch action {
		case SecretReveal, ApplicationRead, ApplicationImport, ApplicationEdit, ApplicationDestroy, ContainerExec, OrganizationRead, MembersManage, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, AuditRead, EndpointRead, EndpointEnroll, EndpointUpdate, EndpointRevoke, ContainerOperate, ContainerDestroy, ImagePull, ImageDestroy, ContainerLogs:
			return true
		}
	case "environment_admin":
		switch action {
		case ApplicationRead, ApplicationImport, ApplicationEdit, ApplicationDestroy, OrganizationRead, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, EndpointRead, EndpointEnroll, EndpointUpdate, EndpointRevoke, ContainerOperate, ContainerDestroy, ImagePull, ImageDestroy, ContainerLogs:
			return true
		}
	case "operator":
		// Day-to-day operations, per docs/authorization-matrix.md: an operator restarts a
		// container but does not destroy one.
		switch action {
		case ApplicationRead, OrganizationRead, EnvironmentRead, EndpointRead, ContainerOperate, ImagePull, ContainerLogs:
			return true
		}
	case "developer":
		// A developer reads logs and edits saved desired state, but has no runtime
		// operation, destruction or exec authority.
		switch action {
		case ApplicationRead, ApplicationEdit, OrganizationRead, EnvironmentRead, EndpointRead, ContainerLogs:
			return true
		}
	case "read_only":
		return action == ApplicationRead || action == OrganizationRead || action == EnvironmentRead || action == EndpointRead
	}
	return false
}
