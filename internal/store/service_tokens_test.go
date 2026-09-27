package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

// orgAdmin opens a store with one organization, "org_a", and one active organization
// administrator, and returns the admin's TenantAccess.
func orgAdmin(t *testing.T) (store.Store, store.TenantAccess) {
	t.Helper()
	st, err := store.Open(context.Background(), testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, newOrgAdmin(t, st, "org_a", "A", "usr_admin")
}

// newOrgAdmin creates an organization and its one active administrator, and returns the
// admin's TenantAccess.
func newOrgAdmin(t *testing.T, st store.Store, orgID, orgName, userID string) store.TenantAccess {
	t.Helper()
	ctx := context.Background()
	if err := st.Users().CreateUser(ctx, &store.User{ID: userID, Username: userID, Role: "user", Status: "active", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().CreateOrganizationWithAdmin(ctx, &store.Organization{ID: orgID, Name: orgName}, userID); err != nil {
		t.Fatal(err)
	}
	return store.TenantAccess{ActorID: userID, OrganizationID: orgID, IPAddress: "10.0.0.1"}
}

func mustPairing(t *testing.T, st store.Store, a store.TenantAccess) *store.ServicePairing {
	t.Helper()
	p, err := st.Tenancy().CreateServicePairing(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustClaim(t *testing.T, st store.Store, code, name, ip string) *store.ServiceTokenIssue {
	t.Helper()
	issue, err := st.Tenancy().ClaimServiceToken(context.Background(), code, name, ip)
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func mustAuthenticate(t *testing.T, st store.Store, token, ip string) *store.ServiceToken {
	t.Helper()
	tok, err := st.Tenancy().AuthenticateServiceToken(context.Background(), token, ip)
	if err != nil {
		t.Fatal(err)
	}
	return tok
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

// auditRowsForOrg counts every audit row naming organizationID, whatever its action.
func auditRowsForOrg(t *testing.T, st store.Store, organizationID string) int {
	t.Helper()
	rows, _, err := st.Audit().ListAuditRecords(context.Background(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.OrganizationID == organizationID {
			n++
		}
	}
	return n
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
	if len(claims) != 1 || claims[0].Result != "success" || claims[0].OrganizationID != "org_a" || !strings.HasPrefix(claims[0].UserID, "service:") {
		t.Fatalf("claim audit: %+v", claims)
	}
	for _, r := range claims {
		if r.Details != "" && (strings.Contains(r.Details, issue.Token) || strings.Contains(r.Details, p.Code)) {
			t.Fatal("audit details must carry neither the token nor the code")
		}
	}
}

func TestClaimConsumesOneLivePairing(t *testing.T) {
	// CreateServicePairing redraws on a collision (fix round 1: "oldest wins" is withdrawn),
	// so two organizations never hold the same live code at once. A pinned draw sequence
	// proves the redraw happened rather than the two organizations coincidentally differing.
	st, a := orgAdmin(t)
	ctx := context.Background()
	b := newOrgAdmin(t, st, "org_b", "B", "usr_b")
	restore := store.SetPairingCodesForTest("424242", "424242", "777777")
	defer restore()
	pa, err := st.Tenancy().CreateServicePairing(ctx, a)
	if err != nil || pa.Code != "424242" {
		t.Fatalf("org a pairing: %+v %v", pa, err)
	}
	pb, err := st.Tenancy().CreateServicePairing(ctx, b)
	if err != nil || pb.Code != "777777" {
		t.Fatalf("org b pairing did not redraw past the collision: %+v %v", pb, err)
	}
	first, err := st.Tenancy().ClaimServiceToken(ctx, "424242", "kypulse", "10.0.0.2")
	if err != nil || first.Organization.ID != "org_a" {
		t.Fatalf("first claim: %+v %v", first, err)
	}
	second, err := st.Tenancy().ClaimServiceToken(ctx, "777777", "kypulse", "10.0.0.2")
	if err != nil || second.Organization.ID != "org_b" {
		t.Fatalf("second claim: %+v %v", second, err)
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
	p := mustPairing(t, st, a)
	issue := mustClaim(t, st, p.Code, "kypulse", "10.0.0.2")
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
	if _, err := st.Tenancy().CreateServicePairing(ctx, svc); !errors.Is(err, store.ErrForbidden) {
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
	// A real second organization: no access, no tenant row planted in it.
	newOrgAdmin(t, st, "org_b", "B", "usr_b")
	foreign := store.TenantAccess{ServiceTokenID: tok.ID, OrganizationID: "org_b", IPAddress: "10.0.0.3"}
	orgBBefore := auditRowsForOrg(t, st, "org_b")
	if _, err := st.Tenancy().ReadOrganization(ctx, foreign); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("foreign organization: %v", err)
	}
	if orgBAfter := auditRowsForOrg(t, st, "org_b"); orgBAfter != orgBBefore {
		t.Fatalf("non-member probe wrote into org_b: %d -> %d", orgBBefore, orgBAfter)
	}
	// DenyService writes a denial row for an API-level refusal (follow on logs).
	if err := st.Tenancy().DenyService(ctx, svc, permissions.ContainerLogs, "follow"); err != nil {
		t.Fatal(err)
	}
	if rows := auditRows(t, st, string(permissions.ContainerLogs)); len(rows) != 1 || rows[0].Result != "denied" || rows[0].Details != "follow" {
		t.Fatalf("DenyService row: %+v", rows)
	}
	// DenyService for a token that does not belong to the named organization writes nothing.
	if err := st.Tenancy().DenyService(ctx, foreign, permissions.ContainerLogs, "follow"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("DenyService for a foreign organization: %v", err)
	}
	if rows := auditRows(t, st, string(permissions.ContainerLogs)); len(rows) != 1 {
		t.Fatalf("DenyService for a foreign organization wrote a row: %+v", rows)
	}
	// RecordServiceTokenReads for a token that does not belong to the named organization
	// writes nothing, and is not an error (a stale counter, not a caller mistake).
	if err := st.Tenancy().RecordServiceTokenReads(ctx, "org_b", tok.ID, 7); err != nil {
		t.Fatalf("RecordServiceTokenReads for a foreign organization: %v", err)
	}
	if rows := auditRows(t, st, "service_token.reads"); len(rows) != 0 {
		t.Fatalf("RecordServiceTokenReads for a foreign organization wrote a row: %+v", rows)
	}
}

func TestServicePrincipalDeniedAfterRevoke(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	p := mustPairing(t, st, a)
	issue := mustClaim(t, st, p.Code, "kypulse", "10.0.0.2")
	tok := mustAuthenticate(t, st, issue.Token, "10.0.0.3")
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
	// The revoked principal's denied read is itself audited.
	var revokedReadDenials []store.AuditRecord
	for _, r := range auditRows(t, st, string(permissions.OrganizationRead)) {
		if r.UserID == "service:"+tok.ID && r.Result == "denied" {
			revokedReadDenials = append(revokedReadDenials, r)
		}
	}
	if len(revokedReadDenials) != 1 {
		t.Fatalf("revoked principal's denied read not audited: %+v", revokedReadDenials)
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
	p := mustPairing(t, st, a)
	issue := mustClaim(t, st, p.Code, "kypulse", "10.0.0.2")
	tok := mustAuthenticate(t, st, issue.Token, "10.0.0.3")
	if err := st.Tenancy().RecordServiceTokenReads(ctx, "org_a", tok.ID, 42); err != nil {
		t.Fatal(err)
	}
	rows := auditRows(t, st, "service_token.reads")
	if len(rows) != 1 || rows[0].Details != "reads=42" || rows[0].UserID != "service:"+tok.ID || rows[0].OrganizationID != "org_a" {
		t.Fatalf("summary row: %+v", rows)
	}
}

// TestAuthenticateStampRules covers the write AuthenticateServiceToken spends on a read: at
// most once a minute, always on an IP change, and never for a revoked token.
func TestAuthenticateStampRules(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	p := mustPairing(t, st, a)
	issue := mustClaim(t, st, p.Code, "kypulse", "10.0.0.2")
	first := mustAuthenticate(t, st, issue.Token, "10.0.0.3")
	if first.LastUsedAt == nil {
		t.Fatal("first authenticate did not stamp last_used_at")
	}
	// Read back the stored stamp: comparing against a value that went through the same
	// storage round trip avoids a false mismatch from timestamp precision truncation.
	list, err := st.Tenancy().ListServiceTokens(ctx, a)
	if err != nil || len(list) != 1 || list[0].LastUsedAt == nil {
		t.Fatalf("list after first authenticate: %+v %v", list, err)
	}
	stored := *list[0].LastUsedAt

	// Same IP, immediately again: no restamp.
	second := mustAuthenticate(t, st, issue.Token, "10.0.0.3")
	if second.LastUsedAt == nil || !second.LastUsedAt.Equal(stored) {
		t.Fatalf("same IP within a minute restamped: stored=%v second=%v", stored, second.LastUsedAt)
	}

	// A different IP restamps even within the minute.
	third := mustAuthenticate(t, st, issue.Token, "10.0.0.4")
	if third.LastUsedAt == nil || third.LastUsedAt.Equal(stored) || third.LastIP != "10.0.0.4" {
		t.Fatalf("IP change did not restamp: stored=%v third=%+v", stored, third)
	}
	// Read back the post-restamp stamp too, for the same precision reason as `stored`.
	afterThird, err := st.Tenancy().ListServiceTokens(ctx, a)
	if err != nil || len(afterThird) != 1 || afterThird[0].LastUsedAt == nil {
		t.Fatalf("list after third authenticate: %+v %v", afterThird, err)
	}
	thirdStored := *afterThird[0].LastUsedAt

	// A revoked token is never re-stamped: authentication itself is refused, and the stored
	// stamp is unchanged.
	if err := st.Tenancy().RevokeServiceToken(ctx, a, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Tenancy().AuthenticateServiceToken(ctx, issue.Token, "10.0.0.5"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("revoked token authenticated: %v", err)
	}
	finalList, err := st.Tenancy().ListServiceTokens(ctx, a)
	if err != nil || len(finalList) != 1 || finalList[0].LastUsedAt == nil {
		t.Fatalf("list after revoke: %+v %v", finalList, err)
	}
	if finalList[0].LastIP != "10.0.0.4" || !finalList[0].LastUsedAt.Equal(thirdStored) {
		t.Fatalf("revoked token was re-stamped: %+v", finalList[0])
	}
}

// TestConcurrentPairingMintsNeverCollide proves the fix round 2 ruling: on PostgreSQL, the
// check-then-insert in CreateServicePairing is only safe because pg_advisory_xact_lock
// serialises it across organizations. SQLite serialises writers on its own, so this needs a
// real Postgres to mean anything.
func TestConcurrentPairingMintsNeverCollide(t *testing.T) {
	if os.Getenv("KY_TEST_POSTGRES_DSN") == "" {
		t.Skip("advisory-lock serialization is PostgreSQL's; SQLite serializes writers")
	}
	st, a := orgAdmin(t)
	ctx := context.Background()
	b := newOrgAdmin(t, st, "org_b", "B", "usr_b")

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		access := a
		if i%2 == 1 {
			access = b
		}
		wg.Add(1)
		go func(acc store.TenantAccess) {
			defer wg.Done()
			if _, err := st.Tenancy().CreateServicePairing(ctx, acc); err != nil {
				errs <- err
			}
		}(access)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	hashes, err := store.LivePairingCodeHashes(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != n {
		t.Fatalf("expected %d live pairings, got %d: %v", n, len(hashes), hashes)
	}
	seen := make(map[string]bool, n)
	for _, h := range hashes {
		if seen[h] {
			t.Fatalf("two live pairings share a code_hash: %v", hashes)
		}
		seen[h] = true
	}
}

func TestExpiredPairingIsDeletedByTheNextMint(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	restoreLife := store.SetPairingLifeForTest(-time.Second)
	p := mustPairing(t, st, a)
	restoreLife()

	exists, err := store.PairingRowExists(ctx, st, p.ID)
	if err != nil || !exists {
		t.Fatalf("expired pairing row missing before the sweep: exists=%v err=%v", exists, err)
	}

	// The next mint's DELETE sweeps expired, unconsumed rows.
	mustPairing(t, st, a)

	exists, err = store.PairingRowExists(ctx, st, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expired pairing row survived the next mint")
	}
}
