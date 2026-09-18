package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestImportedApplicationSecrets(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	key := make([]byte, 32)
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Environment: map[string]ApplicationSecretRef{"PASSWORD": {SecretRef: "password"}}}}}
	values := map[string]string{"password": "plaintext-canary"}
	app, err := ts.ImportApplication(ctx, a, "imported", spec, values, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ts.ResolveApplicationSecrets(ctx, a, app.ID, 1, key)
	if err != nil || got["password"] != values["password"] {
		t.Fatalf("resolve: %v", err)
	}
	revision, err := ts.ReadApplicationRevision(ctx, a, app.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(revision)
	if strings.Contains(string(raw), values["password"]) {
		t.Fatal("public revision leaked")
	}
	var encrypted, stored string
	if err = st.db.QueryRowContext(ctx, `SELECT spec,secrets_enc FROM application_revisions`).Scan(&stored, &encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored+encrypted, values["password"]) {
		t.Fatal("plaintext at rest")
	}
	wrong := make([]byte, 32)
	wrong[0] = 1
	if v, err := ts.ResolveApplicationSecrets(ctx, a, app.ID, 1, wrong); !errors.Is(err, ErrRevisionCorrupt) || v != nil {
		t.Fatal("wrong key accepted")
	}
	// Ciphertext is not portable between otherwise identical application records.
	second, err := ts.ImportApplication(ctx, a, "second", spec, values, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.ExecContext(ctx, st.rebind(`UPDATE application_revisions SET secrets_enc=? WHERE application_id=?`), encrypted, second.ID); err != nil {
		t.Fatal(err)
	}
	if v, err := ts.ResolveApplicationSecrets(ctx, a, second.ID, 1, key); !errors.Is(err, ErrRevisionCorrupt) || v != nil {
		t.Fatal("swapped ciphertext accepted")
	}
	for _, role := range []TenantRole{RoleEnvironmentAdmin, RoleDeveloper, RoleOperator, RoleReadOnly} {
		if err = ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if v, err := ts.ResolveApplicationSecrets(ctx, a, app.ID, 1, key); !errors.Is(err, ErrForbidden) || v != nil {
			t.Fatalf("role %s revealed values", role)
		}
	}
}
func TestImportAtomicSecretsAndAudit(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	key := make([]byte, 32)
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Environment: map[string]ApplicationSecretRef{"PASSWORD": {SecretRef: "password"}}}}}
	for _, values := range []map[string]string{nil, {"other": "value"}, {"password": strings.Repeat("x", 16385)}, {"password": "has\x00nul"}} {
		if _, err := ts.ImportApplication(ctx, a, "bad", spec, values, key); err == nil {
			t.Fatal("invalid values accepted")
		}
	}
	if _, err := ts.ImportApplication(ctx, a, "bad-key", spec, map[string]string{"password": "x"}, nil); err == nil {
		t.Fatal("invalid key accepted")
	}
	app, err := ts.ImportApplication(ctx, a, "good", spec, map[string]string{"password": "x"}, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.ExecContext(ctx, `DROP TABLE audit_records`); err != nil {
		t.Fatal(err)
	}
	if v, err := ts.ResolveApplicationSecrets(ctx, a, app.ID, 1, key); err == nil || v != nil {
		t.Fatal("values returned without audit")
	}
	if _, err = ts.ImportApplication(ctx, a, "failed-audit", spec, map[string]string{"password": "x"}, key); err == nil {
		t.Fatal("import without audit")
	}
	var count int
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM applications`).Scan(&count); err != nil || count != 1 {
		t.Fatal("partial import persisted")
	}
}
