package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

// orgAdmin opens a store with one organization and one active organization administrator,
// and returns the admin's TenantAccess.
func orgAdmin(t *testing.T) (store.Store, store.TenantAccess) {
	t.Helper()
	st, err := store.Open(context.Background(), testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_admin", Username: "admin", Role: "user", Status: "active", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	org := &store.Organization{ID: "org_a", Name: "A"}
	if err := st.Tenancy().CreateOrganizationWithAdmin(ctx, org, "usr_admin"); err != nil {
		t.Fatal(err)
	}
	return st, store.TenantAccess{ActorID: "usr_admin", OrganizationID: "org_a", IPAddress: "10.0.0.1"}
}

func auditRows(t *testing.T, st store.Store, action string) []store.AuditRecord {
	t.Helper()
	rows, _, err := st.Audit().ListAuditRecords(context.Background(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AuditRecord
	for _, r := range rows {
		if r.Action == action {
			out = append(out, *r)
		}
	}
	return out
}

func TestPairingIsSingleUseAndBoundToItsOrganization(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	p, err := st.Tenancy().CreateServicePairing(ctx, a)
	if err != nil || len(p.Code) != 6 || time.Until(p.ExpiresAt) > 15*time.Minute {
		t.Fatalf("pairing: %+v %v", p, err)
	}
	issue, err := st.Tenancy().ClaimServiceToken(ctx, p.Code, "kypulse", "10.0.0.2")
	if err != nil || len(issue.Token) != 64 || issue.Organization.ID != "org_a" || issue.Organization.Name != "A" {
		t.Fatalf("claim: %+v %v", issue, err)
	}
	if _, err := st.Tenancy().ClaimServiceToken(ctx, p.Code, "kypulse", "10.0.0.2"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("second claim must be refused: %v", err)
	}
	if _, err := st.Tenancy().ClaimServiceToken(ctx, "000000", "kypulse", "10.0.0.2"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("unknown code must be refused: %v", err)
	}
	if _, err := st.Tenancy().ClaimServiceToken(ctx, p.Code, "bad name!", "10.0.0.2"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("bad service name must be refused: %v", err)
	}
	claims := auditRows(t, st, "service_token.claim")
	if len(claims) != 1 || claims[0].Result != "success" || claims[0].OrganizationID != "org_a" || claims[0].UserID[:8] != "service:" {
		t.Fatalf("claim audit: %+v", claims)
	}
	for _, r := range claims {
		if len(r.Details) > 0 && (contains(r.Details, issue.Token) || contains(r.Details, p.Code)) {
			t.Fatal("audit details must carry neither the token nor the code")
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestClaimConsumesOneLivePairing(t *testing.T) {
	// Two organizations can mint the same six digits; a claim must take exactly one, the
	// oldest live one, and hand out that organization's token.
	st, a := orgAdmin(t)
	ctx := context.Background()
	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_b", Username: "b", Role: "user", Status: "active", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().CreateOrganizationWithAdmin(ctx, &store.Organization{ID: "org_b", Name: "B"}, "usr_b"); err != nil {
		t.Fatal(err)
	}
	b := store.TenantAccess{ActorID: "usr_b", OrganizationID: "org_b", IPAddress: "10.0.0.1"}
	// Force the same code twice through the exported test hook.
	restore := store.SetPairingCodeForTest("424242")
	defer restore()
	if _, err := st.Tenancy().CreateServicePairing(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Tenancy().CreateServicePairing(ctx, b); err != nil {
		t.Fatal(err)
	}
	first, err := st.Tenancy().ClaimServiceToken(ctx, "424242", "kypulse", "10.0.0.2")
	if err != nil || first.Organization.ID != "org_a" {
		t.Fatalf("first claim: %+v %v", first, err)
	}
	second, err := st.Tenancy().ClaimServiceToken(ctx, "424242", "kypulse", "10.0.0.2")
	if err != nil || second.Organization.ID != "org_b" {
		t.Fatalf("second claim: %+v %v", second, err)
	}
	if _, err := st.Tenancy().ClaimServiceToken(ctx, "424242", "kypulse", "10.0.0.2"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("third claim: %v", err)
	}
}

func TestPairingExpires(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	restore := store.SetPairingLifeForTest(-time.Second)
	defer restore()
	p, err := st.Tenancy().CreateServicePairing(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Tenancy().ClaimServiceToken(ctx, p.Code, "kypulse", "10.0.0.2"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("expired code must be refused: %v", err)
	}
}

func TestServicePrincipalReadsWithoutAuditAndIsDeniedWrites(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	p, _ := st.Tenancy().CreateServicePairing(ctx, a)
	issue, _ := st.Tenancy().ClaimServiceToken(ctx, p.Code, "kypulse", "10.0.0.2")
	tok, err := st.Tenancy().AuthenticateServiceToken(ctx, issue.Token, "10.0.0.3")
	if err != nil || tok.OrganizationID != "org_a" || tok.LastIP != "10.0.0.3" || tok.LastUsedAt == nil {
		t.Fatalf("authenticate: %+v %v", tok, err)
	}
	svc := store.TenantAccess{ServiceTokenID: tok.ID, OrganizationID: "org_a", IPAddress: "10.0.0.3"}
	before := len(auditRows(t, st, "organization.read")) + len(auditRows(t, st, "organization.audit.read"))
	if _, err := st.Tenancy().ReadOrganization(ctx, svc); err != nil {
		t.Fatalf("service read: %v", err)
	}
	if _, err := st.Tenancy().ReadAudit(ctx, svc, 0, 10); err != nil {
		t.Fatalf("service audit read: %v", err)
	}
	if after := len(auditRows(t, st, "organization.read")) + len(auditRows(t, st, "organization.audit.read")); after != before {
		t.Fatalf("service reads must write no success row: %d -> %d", before, after)
	}
	// A write: denied and audited as service:<id>.
	_, err = st.Tenancy().CreateServicePairing(ctx, svc)
	if !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("service write must be denied: %v", err)
	}
	// Two rows share this action (the admin's earlier pairing and this denial); only the
	// service principal's row matters here.
	var denials []store.AuditRecord
	for _, r := range auditRows(t, st, string(permissions.ServiceTokensManage)) {
		if r.UserID == "service:"+tok.ID {
			denials = append(denials, r)
		}
	}
	if len(denials) != 1 || denials[0].Result != "denied" {
		t.Fatalf("denial audit: %+v", denials)
	}
	// Another organization: no access, no tenant row.
	foreign := store.TenantAccess{ServiceTokenID: tok.ID, OrganizationID: "org_zzz", IPAddress: "10.0.0.3"}
	if _, err := st.Tenancy().ReadOrganization(ctx, foreign); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("foreign organization: %v", err)
	}
	// DenyService writes a denial row for an API-level refusal (follow on logs).
	if err := st.Tenancy().DenyService(ctx, svc, permissions.ContainerLogs, "follow"); err != nil {
		t.Fatal(err)
	}
	if rows := auditRows(t, st, string(permissions.ContainerLogs)); len(rows) != 1 || rows[0].Result != "denied" || rows[0].Details != "follow" {
		t.Fatalf("DenyService row: %+v", rows)
	}
}

func TestServicePrincipalDeniedAfterRevoke(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	p, _ := st.Tenancy().CreateServicePairing(ctx, a)
	issue, _ := st.Tenancy().ClaimServiceToken(ctx, p.Code, "kypulse", "10.0.0.2")
	tok, _ := st.Tenancy().AuthenticateServiceToken(ctx, issue.Token, "10.0.0.3")
	list, err := st.Tenancy().ListServiceTokens(ctx, a)
	if err != nil || len(list) != 1 || list[0].ID != tok.ID || list[0].Name != "kypulse" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := st.Tenancy().RevokeServiceToken(ctx, a, tok.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().RevokeServiceToken(ctx, a, tok.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	if _, err := st.Tenancy().AuthenticateServiceToken(ctx, issue.Token, "10.0.0.3"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("revoked token must not authenticate: %v", err)
	}
	svc := store.TenantAccess{ServiceTokenID: tok.ID, OrganizationID: "org_a", IPAddress: "10.0.0.3"}
	if _, err := st.Tenancy().ReadOrganization(ctx, svc); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("revoked principal must be denied in the transaction: %v", err)
	}
	// Several rows share this action and resource (the successful revoke and the second,
	// failed one); exactly one is the successful revoke.
	var revokeSuccesses []store.AuditRecord
	for _, r := range auditRows(t, st, string(permissions.ServiceTokensManage)) {
		if r.Resource == tok.ID && r.Result == "success" {
			revokeSuccesses = append(revokeSuccesses, r)
		}
	}
	if len(revokeSuccesses) != 1 {
		t.Fatalf("revoke audit: %+v", revokeSuccesses)
	}
	// A viewer-level user may not manage tokens.
	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_ro", Username: "ro", Role: "user", Status: "active", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().PutMembership(ctx, a, "usr_ro", "read_only", "active"); err != nil {
		t.Fatal(err)
	}
	ro := store.TenantAccess{ActorID: "usr_ro", OrganizationID: "org_a"}
	if _, err := st.Tenancy().CreateServicePairing(ctx, ro); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read_only must not mint: %v", err)
	}
}

func TestRecordServiceTokenReads(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	p, _ := st.Tenancy().CreateServicePairing(ctx, a)
	issue, _ := st.Tenancy().ClaimServiceToken(ctx, p.Code, "kypulse", "10.0.0.2")
	tok, _ := st.Tenancy().AuthenticateServiceToken(ctx, issue.Token, "10.0.0.3")
	if err := st.Tenancy().RecordServiceTokenReads(ctx, "org_a", tok.ID, 42); err != nil {
		t.Fatal(err)
	}
	rows := auditRows(t, st, "service_token.reads")
	if len(rows) != 1 || rows[0].Details != "reads=42" || rows[0].UserID != "service:"+tok.ID || rows[0].OrganizationID != "org_a" {
		t.Fatalf("summary row: %+v", rows)
	}
}
