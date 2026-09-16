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
