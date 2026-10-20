package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func mustAgentToken(t *testing.T, st store.Store, a store.TenantAccess, scope string) *store.AgentTokenIssue {
	t.Helper()
	issue, err := st.Tenancy().CreateAgentToken(context.Background(), a, "homelab agent", scope)
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

// An agent token is shown once, stored as a hash, carries its scope, and its creation is
// audited with the scope and never the secret.
func TestCreateAgentToken(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	issue := mustAgentToken(t, st, a, store.ScopeMCPRead)
	if !strings.HasPrefix(issue.Token, store.AgentTokenPrefix) || len(issue.Token) != len(store.AgentTokenPrefix)+64 || issue.Scope != store.ScopeMCPRead {
		t.Fatalf("issue: %+v", issue)
	}
	for _, bad := range []struct{ name, scope string }{{"", store.ScopeMCPRead}, {"x", "mcp:write"}, {"x", store.ScopePulse}, {strings.Repeat("n", 65), store.ScopeMCPRead}, {"line\nbreak", store.ScopeMCPRead}} {
		if _, err := st.Tenancy().CreateAgentToken(ctx, a, bad.name, bad.scope); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("CreateAgentToken(%q, %q) = %v, want ErrInvalid", bad.name, bad.scope, err)
		}
	}
	rows := auditRows(t, st, string(permissions.AgentTokensManage))
	if len(rows) != 1 || rows[0].Result != "success" || rows[0].Resource != issue.ID || rows[0].Details != "scope=mcp:read" {
		t.Fatalf("create audit: %+v", rows)
	}
	all, _, _ := st.Audit().ListAuditRecords(ctx, 0, 200)
	for _, r := range all {
		if strings.Contains(r.Details+r.Resource, issue.Token[len(store.AgentTokenPrefix):]) {
			t.Fatalf("the secret reached the audit trail: %+v", r)
		}
	}
	list, err := st.Tenancy().ListAgentTokens(ctx, a)
	if err != nil || len(list) != 1 || list[0].ID != issue.ID || list[0].Scope != store.ScopeMCPRead {
		t.Fatalf("list: %+v %v", list, err)
	}
	// Only the organization administrator manages agent tokens.
	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_op", Username: "usr_op", Role: "user", Status: "active", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "org_a", UserID: "usr_op", Role: "operator", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	op := store.TenantAccess{ActorID: "usr_op", OrganizationID: "org_a"}
	if _, err := st.Tenancy().CreateAgentToken(ctx, op, "mine", store.ScopeMCPOperate); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("an operator minted an agent token: %v", err)
	}
	// An agent token cannot mint another, whatever its scope.
	operate := mustAgentToken(t, st, a, store.ScopeMCPOperate)
	agent := store.TenantAccess{ServiceTokenID: operate.ID, OrganizationID: "org_a"}
	if _, err := st.Tenancy().CreateAgentToken(ctx, agent, "child", store.ScopeMCPOperate); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("an agent minted a token: %v", err)
	}
}

func TestAgentTokenLimit(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	var last *store.AgentTokenIssue
	for range 20 {
		last = mustAgentToken(t, st, a, store.ScopeMCPRead)
	}
	if _, err := st.Tenancy().CreateAgentToken(ctx, a, "one more", store.ScopeMCPRead); !errors.Is(err, store.ErrAgentTokenLimit) {
		t.Fatalf("the 21st token: %v", err)
	}
	if err := st.Tenancy().RevokeAgentToken(ctx, a, last.ID); err != nil {
		t.Fatal(err)
	}
	mustAgentToken(t, st, a, store.ScopeMCPRead)
}

// kyPulse tokens and agent tokens are separate credentials: neither authenticates, is listed,
// re-scoped or revoked as the other.
func TestAgentAndPulseTokensDoNotCross(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	now := time.Now()
	pulse := mustClaim(t, st, mustPairing(t, st, a).Code, "kypulse", "10.0.0.2")
	agent := mustAgentToken(t, st, a, store.ScopeMCPRead)
	if _, err := st.Tenancy().LookupServiceToken(ctx, agent.Token); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("an agent token authenticated as a service token: %v", err)
	}
	if _, err := st.Tenancy().LookupServiceToken(ctx, strings.TrimPrefix(agent.Token, store.AgentTokenPrefix)); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("an agent token without its prefix authenticated as a service token: %v", err)
	}
	if _, err := st.Tenancy().LookupAgentToken(ctx, pulse.Token, now); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("a pulse token authenticated as an agent token: %v", err)
	}
	if _, err := st.Tenancy().LookupAgentToken(ctx, store.AgentTokenPrefix+pulse.Token, now); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("a prefixed pulse token authenticated as an agent token: %v", err)
	}
	pulseTok := mustAuthenticate(t, st, pulse.Token, "10.0.0.2")
	if err := st.Tenancy().SetAgentTokenScope(ctx, a, pulseTok.ID, store.ScopeMCPOperate); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a pulse token was re-scoped: %v", err)
	}
	if err := st.Tenancy().RevokeAgentToken(ctx, a, pulseTok.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a pulse token was revoked as an agent token: %v", err)
	}
	if err := st.Tenancy().RevokeServiceToken(ctx, a, agent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an agent token was revoked as a service token: %v", err)
	}
	services, err := st.Tenancy().ListServiceTokens(ctx, a)
	if err != nil || len(services) != 1 || services[0].ID != pulseTok.ID {
		t.Fatalf("service list: %+v %v", services, err)
	}
	agents, err := st.Tenancy().ListAgentTokens(ctx, a)
	if err != nil || len(agents) != 1 || agents[0].ID != agent.ID {
		t.Fatalf("agent list: %+v %v", agents, err)
	}
}

// The scope is the role, read live inside each transaction: a read token is denied an
// operation and the denial is audited; the same token raised to operate passes the role check;
// lowered again it is denied again; revoked it is denied everything.
func TestAgentScopeIsTheRole(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	issue := mustAgentToken(t, st, a, store.ScopeMCPRead)
	agent := store.TenantAccess{ServiceTokenID: issue.ID, OrganizationID: "org_a", IPAddress: "10.0.0.9"}
	operate := func() error {
		return st.Tenancy().CheckEndpointAccess(ctx, agent, permissions.ContainerOperate, "missing-endpoint")
	}
	if _, err := st.Tenancy().ReadOrganization(ctx, agent); err != nil {
		t.Fatalf("read token read: %v", err)
	}
	if err := operate(); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read token operated: %v", err)
	}
	var denied int
	for _, r := range auditRows(t, st, string(permissions.ContainerOperate)) {
		if r.UserID == "service:"+issue.ID && r.Result == "denied" {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("the read token's denial was not audited once: %d", denied)
	}
	// A pulse_reader-only action stays refused: agent tokens never read the audit trail.
	if _, err := st.Tenancy().ReadAudit(ctx, agent, 0, 10); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("an agent token read the audit trail: %v", err)
	}
	if err := st.Tenancy().SetAgentTokenScope(ctx, a, issue.ID, store.ScopeMCPOperate); err != nil {
		t.Fatal(err)
	}
	if rows := auditRows(t, st, string(permissions.AgentTokensManage)); rows[0].Details != "from=mcp:read to=mcp:operate" {
		t.Fatalf("scope change audit: %+v", rows[0])
	}
	if err := operate(); errors.Is(err, store.ErrForbidden) {
		t.Fatalf("operate token refused by role: %v", err)
	}
	if err := st.Tenancy().SetAgentTokenScope(ctx, a, issue.ID, store.ScopeMCPRead); err != nil {
		t.Fatal(err)
	}
	if err := operate(); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("lowered token operated: %v", err)
	}
	if err := st.Tenancy().SetAgentTokenScope(ctx, a, issue.ID, store.ScopePulse); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("an agent token became a pulse token: %v", err)
	}
	if err := st.Tenancy().RevokeAgentToken(ctx, a, issue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Tenancy().ReadOrganization(ctx, agent); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("revoked token read: %v", err)
	}
	if _, err := st.Tenancy().LookupAgentToken(ctx, issue.Token, time.Now()); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("revoked token authenticated: %v", err)
	}
}

// A token unused for 90 days is refused and revoked at lookup even when no sweep has run; a
// never-used one counts from creation; use restarts the clock; the sweep takes the rest and
// leaves kyPulse tokens alone.
func TestAgentTokenIdleRevocation(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	issue := mustAgentToken(t, st, a, store.ScopeMCPRead)
	created := time.Now()
	day := 24 * time.Hour
	if _, err := st.Tenancy().LookupAgentToken(ctx, issue.Token, created.Add(89*day)); err != nil {
		t.Fatalf("89 days unused: %v", err)
	}
	if _, err := st.Tenancy().AuthenticateAgentToken(ctx, issue.Token, "10.0.0.4", created.Add(89*day)); err != nil {
		t.Fatalf("authenticate at 89 days: %v", err)
	}
	// Used at day 89, so day 150 is 61 days idle.
	if _, err := st.Tenancy().LookupAgentToken(ctx, issue.Token, created.Add(150*day)); err != nil {
		t.Fatalf("use did not restart the clock: %v", err)
	}
	if _, err := st.Tenancy().LookupAgentToken(ctx, issue.Token, created.Add(181*day)); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("92 days idle: %v", err)
	}
	if list, _ := st.Tenancy().ListAgentTokens(ctx, a); len(list) != 0 {
		t.Fatalf("an idle token stayed listed: %+v", list)
	}
	if _, err := st.Tenancy().LookupAgentToken(ctx, issue.Token, created); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("an expired token came back: %v", err)
	}
	expired := auditRows(t, st, "agent_token.expired")
	if len(expired) != 1 || expired[0].Resource != issue.ID || expired[0].OrganizationID != "org_a" {
		t.Fatalf("expiry audit: %+v", expired)
	}

	// The sweep: an unused token and never a pulse token.
	pulse := mustClaim(t, st, mustPairing(t, st, a).Code, "kypulse", "10.0.0.2")
	other := mustAgentToken(t, st, a, store.ScopeMCPOperate)
	gone, err := st.Tenancy().ExpireIdleAgentTokens(ctx, time.Now().Add(91*day), "")
	if err != nil || len(gone) != 1 || gone[0].ID != other.ID {
		t.Fatalf("sweep: %+v %v", gone, err)
	}
	if _, err := st.Tenancy().LookupServiceToken(ctx, pulse.Token); err != nil {
		t.Fatalf("the sweep took a pulse token: %v", err)
	}
	if len(auditRows(t, st, "agent_token.expired")) != 2 {
		t.Fatal("the sweep did not audit its expiry")
	}
}

// RecordAgentAction writes only for a token of the named organization.
func TestRecordAgentAction(t *testing.T) {
	st, a := orgAdmin(t)
	ctx := context.Background()
	issue := mustAgentToken(t, st, a, store.ScopeMCPOperate)
	agent := store.TenantAccess{ServiceTokenID: issue.ID, OrganizationID: "org_a"}
	if err := st.Tenancy().RecordAgentAction(ctx, agent, "mcp.container_restart", "ep/web", "agent_token:"+issue.ID); err != nil {
		t.Fatal(err)
	}
	newOrgAdmin(t, st, "org_b", "B", "usr_b")
	if err := st.Tenancy().RecordAgentAction(ctx, store.TenantAccess{ServiceTokenID: issue.ID, OrganizationID: "org_b"}, "mcp.container_restart", "x", ""); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("a row planted in another organization: %v", err)
	}
	rows := auditRows(t, st, "mcp.container_restart")
	if len(rows) != 1 || rows[0].UserID != "service:"+issue.ID || rows[0].OrganizationID != "org_a" {
		t.Fatalf("agent action audit: %+v", rows)
	}
}
