package permissions

import "testing"

func TestFixedRoleMatrix(t *testing.T) {
	actions := []Action{OrganizationRead, MembersManage, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, AuditRead, PlatformAdmin, "unknown"}
	for _, tc := range []struct {
		role    string
		allowed []Action
	}{
		{"organization_admin", []Action{OrganizationRead, MembersManage, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete, AuditRead}},
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
			if got := PlatformAllows(tc.role, a); got != (tc.role == "admin" && a == PlatformAdmin) {
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
