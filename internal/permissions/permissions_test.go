package permissions

import (
	"slices"
	"testing"
)

func TestFixedRoleMatrix(t *testing.T) {
	actions := []Action{OrganizationRead, MembersManage, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, AuditRead, ContainerConfigure, PlatformAdmin, SuiteRead, SuiteManage, "unknown"}
	for _, tc := range []struct {
		role    string
		allowed []Action
	}{
		{"organization_admin", []Action{OrganizationRead, MembersManage, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, AuditRead, ContainerConfigure}},
		{"environment_admin", []Action{OrganizationRead, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete}},
		{"operator", []Action{OrganizationRead, EnvironmentRead}},
		{"developer", []Action{OrganizationRead, EnvironmentRead}},
		{"read_only", []Action{OrganizationRead, EnvironmentRead}},
		{"admin", nil}, {"unknown", nil}, {"", nil},
	} {
		for _, a := range actions {
			want := false
			for _, v := range tc.allowed {
				if a == v {
					want = true
				}
			}
			if got := Allows(tc.role, a); got != want {
				t.Errorf("%s %s: %v, want %v", tc.role, a, got, want)
			}
			platform := a == PlatformAdmin || a == SuiteRead || a == SuiteManage
			if got := PlatformAllows(tc.role, a); got != (tc.role == "admin" && platform) {
				t.Errorf("platform permission leaked to %s %s", tc.role, a)
			}
		}
	}
}

// Reading a container's log is not reading a container: a log carries whatever the workload
// printed, which is where credentials and customer data turn up. The matrix (row
// `container.logs` in docs/authorization-matrix.md) stops it at the developer.
func TestLogsAreReadableByEveryoneButTheReadOnlyMember(t *testing.T) {
	for _, role := range []string{"organization_admin", "environment_admin", "operator", "developer"} {
		if !Allows(role, ContainerLogs) {
			t.Errorf("%s cannot read logs", role)
		}
	}
	for _, role := range []string{"read_only", "admin", "unknown", ""} {
		if Allows(role, ContainerLogs) {
			t.Errorf("%s can read logs", role)
		}
	}
	// A developer still operates nothing.
	for _, a := range []Action{ContainerOperate, ContainerDestroy, ImagePull, ImageDestroy} {
		if Allows("developer", a) {
			t.Errorf("a developer was granted %s", a)
		}
	}
}

func TestExecIsOrganizationAdminOnly(t *testing.T) {
	for _, role := range []string{"organization_admin", "environment_admin", "operator", "developer", "read_only", "admin", "unknown", ""} {
		if Allows(role, ContainerExec) != (role == "organization_admin") {
			t.Errorf("unexpected exec permission: %s", role)
		}
	}
}

func TestConfigureIsOrganizationAdminOnly(t *testing.T) {
	if ContainerConfigure != "container.configure" {
		t.Fatal("the audit identifier changed")
	}
	for _, role := range []string{"organization_admin", "environment_admin", "operator", "developer", "read_only", RolePulseReader, "admin", "unknown", ""} {
		if Allows(role, ContainerConfigure) != (role == "organization_admin") {
			t.Errorf("unexpected configure permission: %s", role)
		}
	}
}

func TestApplicationRoleMatrix(t *testing.T) {
	for _, role := range []string{"organization_admin", "environment_admin", "operator", "developer", "read_only", "admin", "unknown", ""} {
		member := role == "organization_admin" || role == "environment_admin" || role == "operator" || role == "developer" || role == "read_only"
		admin := role == "organization_admin" || role == "environment_admin"
		for action, want := range map[Action]bool{ApplicationAdopt: admin, ApplicationRelease: admin, SecretReveal: role == "organization_admin", ApplicationRead: member, ApplicationImport: admin, ApplicationDestroy: admin, ApplicationEdit: admin || role == "developer"} {
			if Allows(role, action) != want || PlatformAllows(role, action) {
				t.Errorf("%s %s", role, action)
			}
		}
	}
}

func TestApplicationDeployFollowsTheMatrix(t *testing.T) {
	for role, want := range map[string]bool{"organization_admin": true, "environment_admin": true, "developer": true, "operator": false, "read_only": false, "": false} {
		if got := Allows(role, ApplicationDeploy); got != want {
			t.Fatalf("%q application.deploy = %v, want %v", role, got, want)
		}
	}
}

// Every member reads the registry list (it carries no secret); only the organization
// administrator configures registries, credentials and the anonymous-pull opt-in.
func TestRegistryRoleMatrix(t *testing.T) {
	for _, role := range []string{"organization_admin", "environment_admin", "operator", "developer", "read_only", "admin", "unknown", ""} {
		member := role == "organization_admin" || role == "environment_admin" || role == "operator" || role == "developer" || role == "read_only"
		if Allows(role, RegistryRead) != member || PlatformAllows(role, RegistryRead) {
			t.Errorf("%s registry.read", role)
		}
		if Allows(role, RegistryManage) != (role == "organization_admin") || PlatformAllows(role, RegistryManage) {
			t.Errorf("%s registry.manage", role)
		}
	}
}

// Update policies act unattended as the administrator who saved them, so only the organization
// administrator may create, edit, delete or resume one (docs/authorization-matrix.md).
func TestUpdatePolicyIsOrganizationAdminOnly(t *testing.T) {
	for _, role := range []string{"organization_admin", "environment_admin", "operator", "developer", "read_only", "admin", "unknown", ""} {
		if Allows(role, ApplicationPolicy) != (role == "organization_admin") || PlatformAllows(role, ApplicationPolicy) {
			t.Errorf("%q application.policy", role)
		}
	}
	if ApplicationPolicy != "application.policy" {
		t.Fatal("the audit identifier changed")
	}
}

// A migration copies the source's secret values into the destination it creates, so only the
// organization administrator, who may reveal them, migrates (docs/authorization-matrix.md).
func TestMigrateIsOrganizationAdminOnly(t *testing.T) {
	for _, role := range []string{"organization_admin", "environment_admin", "operator", "developer", "read_only", "admin", "unknown", ""} {
		if Allows(role, ApplicationMigrate) != (role == "organization_admin") || PlatformAllows(role, ApplicationMigrate) {
			t.Errorf("%q application.migrate", role)
		}
	}
	if ApplicationMigrate != "application.migrate" {
		t.Fatal("the audit identifier changed")
	}
}

func TestPulseReaderIsReadOnly(t *testing.T) {
	allowed := []Action{OrganizationRead, EnvironmentRead, EndpointRead, ContainerLogs, AuditRead}
	for _, a := range allowed {
		if !Allows(RolePulseReader, a) {
			t.Errorf("pulse_reader must hold %s", a)
		}
	}
	for _, a := range []Action{ApplicationRead, MembersManage, EnvironmentCreate, EndpointEnroll, ContainerOperate, ContainerExec, ContainerConfigure, ImagePull, RegistryRead, ServiceTokensManage, ApplicationDeploy} {
		if Allows(RolePulseReader, a) {
			t.Errorf("pulse_reader must not hold %s", a)
		}
	}
	if !Allows("organization_admin", ServiceTokensManage) || Allows("environment_admin", ServiceTokensManage) || Allows("operator", ServiceTokensManage) {
		t.Fatal("service tokens are managed by organization administrators only")
	}
}

// An agent token's role is its whole ceiling: every action not listed here is refused, so a new
// action never reaches an AI agent by default.
func TestAgentRolesHoldExactlyTheirActions(t *testing.T) {
	all := []Action{ApplicationAdopt, ApplicationRelease, SecretReveal, ApplicationRead, ApplicationImport, ApplicationEdit, ApplicationDestroy, PlatformAdmin, OrganizationRead, MembersManage,
		EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, AuditRead, EndpointRead, EndpointEnroll, EndpointUpdate, EndpointRevoke, ContainerOperate, ContainerDestroy,
		ImagePull, ImageDestroy, ContainerLogs, ContainerExec, ContainerConfigure, ApplicationDeploy, RegistryRead, RegistryManage, ApplicationPolicy, ApplicationMigrate,
		ServiceTokensManage, AgentTokensManage, SuiteRead, SuiteManage}
	reader := []Action{OrganizationRead, EnvironmentRead, EndpointRead, ContainerLogs, ApplicationRead}
	want := map[string][]Action{
		RoleAgentReader:   reader,
		RoleAgentOperator: append(append([]Action{}, reader...), ContainerOperate, ApplicationDeploy),
	}
	for role, held := range want {
		for _, a := range all {
			if got := Allows(role, a); got != slices.Contains(held, a) {
				t.Errorf("Allows(%s, %s) = %t", role, a, got)
			}
		}
	}
	if !Allows("organization_admin", AgentTokensManage) || Allows("environment_admin", AgentTokensManage) || Allows("operator", AgentTokensManage) || Allows(RolePulseReader, AgentTokensManage) {
		t.Fatal("agent tokens are managed by organization administrators only")
	}
}
