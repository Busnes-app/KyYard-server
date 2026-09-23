package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const registrySecret = "registry-secret-canary"

func registryFixture(t *testing.T) (*SQLStore, TenantAccess, []byte) {
	t.Helper()
	st, a := tenantAtomicStore(t)
	a.EnvironmentID = ""
	return st, a, []byte(strings.Repeat("k", 32))
}

func registryAudit(t *testing.T, st *SQLStore, action, resource string) (details []string) {
	t.Helper()
	rows, err := st.db.QueryContext(context.Background(), st.rebind(`SELECT details FROM audit_records WHERE action=? AND resource=? AND result='success' ORDER BY id`), action, resource)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		details = append(details, d)
	}
	return details
}

func auditCount(t *testing.T, st *SQLStore, action, result string) (n int) {
	t.Helper()
	if err := st.db.QueryRowContext(context.Background(), st.rebind(`SELECT COUNT(*) FROM audit_records WHERE action=? AND result=?`), action, result).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ptr(s string) *string { return &s }

func TestRegistryCredentialIsWriteOnly(t *testing.T) {
	st, a, key := registryFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	r, err := ts.PutRegistry(ctx, a, RegistryInput{Host: "GHCR.io", Name: "GitHub", Username: "bot", Credential: ptr(registrySecret)}, key)
	if err != nil {
		t.Fatal(err)
	}
	if r.Host != "ghcr.io" || !r.HasCredential || r.CreatedBy != "actor" || r.OrganizationID != a.OrganizationID {
		t.Fatalf("put: %+v", r)
	}
	list, err := ts.ListRegistries(ctx, a)
	if err != nil || len(list) != 1 || !list[0].HasCredential || list[0].Username != "bot" || list[0].ID != r.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	raw, _ := json.Marshal(list)
	one, _ := json.Marshal(r)
	var stored string
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT credential_enc FROM registries WHERE id=?`), r.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw)+string(one), registrySecret) || strings.Contains(string(raw)+string(one), stored) || strings.Contains(stored, registrySecret) {
		t.Fatal("secret or ciphertext left the store")
	}

	// nil keeps, "" clears; the same host updates the same row.
	kept, err := ts.PutRegistry(ctx, a, RegistryInput{Host: "ghcr.io", Name: "GitHub 2", Username: "bot", AllowPrivate: true}, key)
	if err != nil || kept.ID != r.ID || !kept.HasCredential || kept.Name != "GitHub 2" || !kept.AllowPrivate || !kept.CreatedAt.Equal(r.CreatedAt) {
		t.Fatalf("keep: %+v %v", kept, err)
	}
	access, err := ts.ResolveRegistryAccess(ctx, a, "ghcr.io/org/app:v1", key)
	if err != nil || access.Credential == nil || access.Credential.Secret != registrySecret || access.Credential.Username != "bot" || access.Anonymous || access.Registry.ID != r.ID {
		t.Fatalf("resolve after keep: %+v %v", access, err)
	}
	cleared, err := ts.PutRegistry(ctx, a, RegistryInput{Host: "ghcr.io", Name: "GitHub", Credential: ptr("")}, key)
	if err != nil || cleared.ID != r.ID || cleared.HasCredential {
		t.Fatalf("clear: %+v %v", cleared, err)
	}
	access, err = ts.ResolveRegistryAccess(ctx, a, "ghcr.io/org/app:v1", key)
	if err != nil || access.Credential != nil || access.Anonymous {
		t.Fatalf("resolve after clear: %+v %v", access, err)
	}
	got := registryAudit(t, st, "registry.manage", "registries/"+r.ID)
	want := []string{"host=ghcr.io allow_private=false credential=set", "host=ghcr.io allow_private=true credential=kept", "host=ghcr.io allow_private=false credential=cleared"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("audit details %q", got)
	}
	var leaked int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM audit_records WHERE details LIKE ? OR resource LIKE ?`), "%"+registrySecret+"%", "%"+registrySecret+"%").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("secret in audit: %d %v", leaked, err)
	}

	if err := ts.DeleteRegistry(ctx, a, r.ID); err != nil {
		t.Fatal(err)
	}
	if got := registryAudit(t, st, "registry.manage", "registries/"+r.ID); len(got) != 4 || got[3] != "host=ghcr.io" {
		t.Fatalf("delete audit %q", got)
	}
	if err := ts.DeleteRegistry(ctx, a, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := ts.DeleteRegistry(ctx, a, "not-a-uuid"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	if list, _ := ts.ListRegistries(ctx, a); len(list) != 0 {
		t.Fatal("delete left the row")
	}
}

func TestRegistryInputValidation(t *testing.T) {
	st, a, key := registryFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for host, want := range map[string]string{"docker.io": "docker.io", "index.docker.io": "docker.io", "Registry-1.Docker.io": "docker.io", "registry.example:5000": "registry.example:5000", "localhost:5000": "localhost:5000", "localhost": "localhost", "10.0.0.5": "10.0.0.5"} {
		if got, err := NormalizeRegistryHost(host); err != nil || got != want {
			t.Errorf("NormalizeRegistryHost(%q) = %q, %v", host, got, err)
		}
	}
	for _, host := range []string{"", "-bad.io", "ghcr.io/org", "https://ghcr.io", "ghcr.io:", "ghcr.io:123456", "gh cr.io", "ghcr.io\n", strings.Repeat("a", 254), "myregistry", "a..b", "-a.b", "a-.b", "a.b.", "a.b:70000", "a.b:0"} {
		if _, err := NormalizeRegistryHost(host); !errors.Is(err, ErrInvalid) {
			t.Errorf("NormalizeRegistryHost(%q) accepted", host)
		}
	}
	for _, in := range []RegistryInput{
		{Host: "ghcr.io/x", Name: "n"},
		{Host: "ghcr.io", Name: ""},
		{Host: "ghcr.io", Name: "   "},
		{Host: "ghcr.io", Name: strings.Repeat("n", 65)},
		{Host: "ghcr.io", Name: "bad‮name"},
		{Host: "ghcr.io", Name: "n", Username: strings.Repeat("u", 256)},
		{Host: "ghcr.io", Name: "n", Username: "bad\nuser"},
		{Host: "ghcr.io", Name: "n", Username: "bad:user"},
		{Host: "ghcr.io", Name: "n", Credential: ptr(strings.Repeat("s", 4097))},
		{Host: "ghcr.io", Name: "n", Credential: ptr("nul\x00secret")},
		{Host: "ghcr.io", Name: "n", Credential: ptr("\xff\xfe")},
	} {
		if _, err := ts.PutRegistry(ctx, a, in, key); !errors.Is(err, ErrInvalid) {
			t.Errorf("PutRegistry(%+v) = %v, want ErrInvalid", in, err)
		}
	}
	if _, err := ts.PutRegistry(ctx, a, RegistryInput{Host: "ghcr.io", Name: "n", Credential: ptr("s")}, key[:16]); !errors.Is(err, ErrInvalid) {
		t.Errorf("short key: %v", err)
	}
	if _, err := ts.PutRegistry(ctx, a, RegistryInput{Host: "ghcr.io", Name: strings.Repeat("n", 64), Username: strings.Repeat("u", 255), Credential: ptr(strings.Repeat("s", 4096))}, key); err != nil {
		t.Errorf("limits refused: %v", err)
	}
}

func TestRegistryRoles(t *testing.T) {
	st, admin, key := registryFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	r, err := ts.PutRegistry(ctx, admin, RegistryInput{Host: "ghcr.io", Name: "GitHub", Credential: ptr(registrySecret)}, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []TenantRole{RoleEnvironmentAdmin, RoleOperator, RoleDeveloper, RoleReadOnly} {
		id := "member-" + string(role)
		if err := st.Users().CreateUser(ctx, &User{ID: id, Username: id, Role: "user", Status: "active", SSOProvider: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: admin.OrganizationID, UserID: id, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		a := TenantAccess{ActorID: id, OrganizationID: admin.OrganizationID}
		if list, err := ts.ListRegistries(ctx, a); err != nil || len(list) != 1 {
			t.Errorf("%s list: %v", role, err)
		}
		if _, err := ts.ReadRegistryPolicy(ctx, a); err != nil {
			t.Errorf("%s policy read: %v", role, err)
		}
		if _, err := ts.PutRegistry(ctx, a, RegistryInput{Host: "quay.io", Name: "Quay"}, key); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s put: %v", role, err)
		}
		if err := ts.DeleteRegistry(ctx, a, r.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s delete: %v", role, err)
		}
		if err := ts.SetAnonymousPull(ctx, a, true); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s policy set: %v", role, err)
		}
	}
	if n := auditCount(t, st, "registry.manage", "denied"); n != 12 {
		t.Fatalf("denied audit rows = %d, want 12", n)
	}
	if list, _ := ts.ListRegistries(ctx, admin); len(list) != 1 {
		t.Fatal("a denied write changed the list")
	}
	if p, _ := ts.ReadRegistryPolicy(ctx, admin); p.AnonymousPullEnabled {
		t.Fatal("a denied write changed the policy")
	}
}

func TestRegistryPolicyAndResolution(t *testing.T) {
	st, a, key := registryFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	hub, err := ts.PutRegistry(ctx, a, RegistryInput{Host: "index.docker.io", Name: "Docker Hub", Username: "hub", Credential: ptr(registrySecret)}, key)
	if err != nil || hub.Host != "docker.io" {
		t.Fatalf("hub: %+v %v", hub, err)
	}
	for _, ref := range []string{"nginx", "nginx:1", "library/nginx:1", "docker.io/library/nginx:1", "registry-1.docker.io/org/app"} {
		access, err := ts.ResolveRegistryAccess(ctx, a, ref, key)
		if err != nil || access.Registry == nil || access.Registry.ID != hub.ID || access.Credential == nil || access.Credential.Secret != registrySecret {
			t.Errorf("resolve %q: %+v %v", ref, access, err)
		}
	}
	if _, err := ts.ResolveRegistryAccess(ctx, a, "../etc", key); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad ref: %v", err)
	}
	if _, err := ts.ResolveRegistryAccess(ctx, a, "quay.io/org/app", key); !errors.Is(err, ErrRegistryNotConfigured) {
		t.Fatalf("unconfigured with opt-in off: %v", err)
	}
	policy, err := ts.ReadRegistryPolicy(ctx, a)
	if err != nil || policy.AnonymousPullEnabled {
		t.Fatalf("default policy: %+v %v", policy, err)
	}
	if err := ts.SetAnonymousPull(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if policy, err := ts.ReadRegistryPolicy(ctx, a); err != nil || !policy.AnonymousPullEnabled {
		t.Fatalf("policy after set: %+v %v", policy, err)
	}
	access, err := ts.ResolveRegistryAccess(ctx, a, "quay.io/org/app", key)
	if err != nil || !access.Anonymous || access.Registry != nil || access.Credential != nil {
		t.Fatalf("unconfigured with opt-in on: %+v %v", access, err)
	}
	if err := ts.SetAnonymousPull(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if got := registryAudit(t, st, "registry.manage", "registry-policy"); strings.Join(got, "|") != "old=false new=true|old=true new=false" {
		t.Fatalf("policy audit %q", got)
	}
	if _, err := ts.ResolveRegistryAccess(ctx, a, "quay.io/org/app", key); !errors.Is(err, ErrRegistryNotConfigured) {
		t.Fatalf("opt-in off again: %v", err)
	}
	// Resolution audits nothing itself, whether configured, anonymous or not configured.
	if n := auditCount(t, st, "registry.read", "failure") + auditCount(t, st, "registry.read", "success"); n != 0 {
		t.Fatalf("resolve wrote %d registry.read audit rows", n)
	}
}

// Ciphertext is bound to its organization and row: moved elsewhere, it does not decrypt.
func TestRegistryCredentialIsBoundToItsRow(t *testing.T) {
	st, a, key := registryFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	first, err := ts.PutRegistry(ctx, a, RegistryInput{Host: "ghcr.io", Name: "one", Credential: ptr(registrySecret)}, key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ts.PutRegistry(ctx, a, RegistryInput{Host: "quay.io", Name: "two", Credential: ptr("other")}, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE registries SET credential_enc=(SELECT credential_enc FROM registries WHERE id=?) WHERE id=?`), first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ResolveRegistryAccess(ctx, a, "quay.io/org/app", key); !errors.Is(err, ErrInvalid) {
		t.Fatalf("moved ciphertext: %v", err)
	}
	if _, err := ts.ResolveRegistryAccess(ctx, a, "ghcr.io/org/app", []byte(strings.Repeat("x", 32))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestRegistriesAreOrganizationScoped(t *testing.T) {
	st, a, key := registryFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &Organization{ID: "other", Name: "Other"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().CreateUser(ctx, &User{ID: "other-admin", Username: "other-admin", Role: "user", Status: "active", SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: "other", UserID: "other-admin", Role: RoleOrganizationAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	b := TenantAccess{ActorID: "other-admin", OrganizationID: "other"}
	mine, err := ts.PutRegistry(ctx, a, RegistryInput{Host: "ghcr.io", Name: "mine", Credential: ptr(registrySecret)}, key)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := ts.PutRegistry(ctx, b, RegistryInput{Host: "ghcr.io", Name: "theirs", Credential: ptr("theirs")}, key)
	if err != nil || theirs.ID == mine.ID {
		t.Fatalf("same host in another organization: %+v %v", theirs, err)
	}
	if err := ts.SetAnonymousPull(ctx, b, true); err != nil {
		t.Fatal(err)
	}
	if list, _ := ts.ListRegistries(ctx, b); len(list) != 1 || list[0].ID != theirs.ID {
		t.Fatalf("list leaked: %+v", list)
	}
	if err := ts.DeleteRegistry(ctx, b, mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-organization delete: %v", err)
	}
	access, err := ts.ResolveRegistryAccess(ctx, b, "ghcr.io/org/app", key)
	if err != nil || access.Credential.Secret != "theirs" {
		t.Fatalf("resolve in b: %+v %v", access, err)
	}
	if p, _ := ts.ReadRegistryPolicy(ctx, a); p.AnonymousPullEnabled {
		t.Fatal("policy leaked across organizations")
	}
	// A non-member naming another organization is refused.
	if _, err := ts.ListRegistries(ctx, TenantAccess{ActorID: "other-admin", OrganizationID: a.OrganizationID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member list: %v", err)
	}
	if _, err := ts.ResolveRegistryAccess(ctx, TenantAccess{ActorID: "other-admin", OrganizationID: a.OrganizationID}, "ghcr.io/org/app", key); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member resolve: %v", err)
	}
}
