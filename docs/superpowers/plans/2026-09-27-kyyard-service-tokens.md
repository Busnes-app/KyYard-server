# KyYard service tokens and `pulse_reader` (kyPulse step 3a) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let kyPulse read one KyYard organization with a revocable, read-only service token obtained by a 6-digit pairing code, and give KyYard a `/healthz` on the suite health contract.

**Architecture:** A service token is a second kind of tenant principal. `store.TenantAccess` gains `ServiceTokenID`; the existing `run` resolves the principal's role from either the membership row (users) or the token row (services, fixed role `pulse_reader`) and everything downstream — named actions, live authorization inside the transaction, denial audit — stays as it is. Tokens are minted only by claiming a single-use pairing code an organization administrator generated; the claim route is unauthenticated and rate-limited. Service reads write no per-request audit row; the API counts them and writes one `service_token.reads` row per token per hour. `/healthz` comes from `ky-primitives/health` beside the existing `/health/live` and `/health/ready`.

**Tech Stack:** Go 1.26, `ky-primitives` bumped to v0.9.0 (`health`, `logging`), SQLite + PostgreSQL through `rebind`, React 19 + TypeScript (vitest), the repo's smoke test.

**Spec:** kyPulse design `/home/yoshi/git/busnes.app/kyPulse-server/docs/superpowers/specs/2026-09-26-kypulse-design.md` §3 "KyYard changes (KyYard-Server repo)" and §1 (health contract). KyYard authority: `docs/authorization-matrix.md` (handlers check named actions; denials audited; successful reads not audited), `internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `internal/permissions/AGENTS.md`, root `AGENTS.md` (`cmd/server` start order and loop waits).

## Global Constraints

- Handlers check named actions, never role strings; live authorization runs inside the store transaction (`run`). A service principal must go through the same path.
- `pulse_reader` allows exactly: `organization.read`, `environment.read`, `endpoint.read`, `container.logs` (never `follow=1`), `organization.audit.read`. Every other action, every mutating route and every other organization answer 403 and write a `denied` audit row (a foreign organization writes no tenant row, matching non-members).
- Pairing code: six digits from `crypto/rand`, 15-minute TTL, single use, bound to the organization that minted it; only its SHA-256 is stored. Token: 32 random bytes, presented as 64 hex characters, only its SHA-256 stored; never logged, never in an audit row.
- Claim route `POST /api/service-tokens/claim`, body `{"pairing_code","service_name"}`, response `{"token","organization":{"id","name"}}`; limits 5 per minute per IP and 30 per minute globally via `allowAttempt`; every refusal is 403 with no distinguishing detail.
- Service reads write no per-request audit row; the token records `last_used_at` and `last_ip` (stamped at most once a minute); one `service_token.reads` audit row per token per hour with `reads=N`; claim, revoke and every denial are audited individually.
- Rate limits keyed on `s.requestIP(r)`; keys follow the `"<route>:"+ip` convention.
- Every env var is `KY_*` (none added). Both dialects; CI runs Postgres. `make ci` green. DOX pass on every touched AGENTS.md. Commits end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. **Two organizations minting the same six digits** at once: the claim must consume exactly one pairing, the oldest live match, and the token must belong to that pairing's organization. Pinned in Task 2 (`TestClaimConsumesOneLivePairing`).
2. **A revoked token mid-session**: the next request is 401, and a request in flight when the row is revoked is denied by the in-transaction check, not by a cache. Pinned in Task 2 (`TestServicePrincipalDeniedAfterRevoke`) and Task 3 (401 after revoke).
3. **Bearer on a mutating route** (`POST .../environments`): 403 plus a `denied` audit row naming `service:<id>`; nothing created. Pinned in Task 3.
4. **Follow on logs with a bearer**: 403 with a denial row, the stream never opens. Pinned in Task 3.
5. **Claim brute force**: the 6th attempt from one IP inside a minute is 429 before the body is read; the 31st across IPs is 429. Pinned in Task 3.

---

### Task 1: `pulse_reader` role and `organization.service_tokens.manage`

**Files:**
- Modify: `internal/permissions/permissions.go`
- Modify: `internal/permissions/permissions_test.go`
- Modify: `internal/permissions/AGENTS.md`, `docs/authorization-matrix.md`

**Interfaces:**
- Produces: `permissions.ServiceTokensManage Action = "organization.service_tokens.manage"` (organization administrators only); `permissions.RolePulseReader = "pulse_reader"`; `Allows(RolePulseReader, …)` true for exactly `OrganizationRead, EnvironmentRead, EndpointRead, ContainerLogs, AuditRead`.

- [ ] **Step 1: Write the failing test**

Add to `internal/permissions/permissions_test.go` (match the file's existing style for table tests; if it iterates a role→actions map, extend that map instead):

```go
func TestPulseReaderIsReadOnly(t *testing.T) {
	allowed := []Action{OrganizationRead, EnvironmentRead, EndpointRead, ContainerLogs, AuditRead}
	for _, a := range allowed {
		if !Allows(RolePulseReader, a) {
			t.Errorf("pulse_reader must hold %s", a)
		}
	}
	for _, a := range []Action{ApplicationRead, MembersManage, EnvironmentCreate, EndpointEnroll, ContainerOperate, ContainerExec, ImagePull, RegistryRead, ServiceTokensManage, ApplicationDeploy} {
		if Allows(RolePulseReader, a) {
			t.Errorf("pulse_reader must not hold %s", a)
		}
	}
	if !Allows("organization_admin", ServiceTokensManage) || Allows("environment_admin", ServiceTokensManage) || Allows("operator", ServiceTokensManage) {
		t.Fatal("service tokens are managed by organization administrators only")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/permissions/ -run TestPulseReaderIsReadOnly`
Expected: FAIL to compile (`RolePulseReader`, `ServiceTokensManage` undefined).

- [ ] **Step 3: Implement**

In `internal/permissions/permissions.go` add to the const block:

```go
	// ServiceTokensManage mints pairing codes for, lists and revokes the organization's
	// service tokens. A token reads the whole organization, so only its administrator
	// hands one out.
	ServiceTokensManage Action = "organization.service_tokens.manage"
```

and after the block:

```go
// RolePulseReader is the fixed role of a service token: kyPulse reads inventory, samples,
// logs and the audit feed of one organization and can change nothing.
const RolePulseReader = "pulse_reader"
```

In `Allows`, add `ServiceTokensManage` to the `organization_admin` case list, and add before `case "read_only":`:

```go
	case RolePulseReader:
		switch action {
		case OrganizationRead, EnvironmentRead, EndpointRead, ContainerLogs, AuditRead:
			return true
		}
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/permissions/`
Expected: PASS.

- [ ] **Step 5: Docs**

- `internal/permissions/AGENTS.md` Local Contracts: "`organization.service_tokens.manage` (mint a pairing code, list, revoke) is organization-administrator-only. `pulse_reader` is the fixed role of a service token: `organization.read`, `environment.read`, `endpoint.read`, `container.logs` and `organization.audit.read`, nothing else; it is never assignable to a user."
- `docs/authorization-matrix.md`: Roles table gains `| Pulse reader (service token) | organization | read-only: organization, environments, endpoints, inventory, samples, container logs without follow, audit feed; no mutation, no exec, no secrets |`. Add a section `### Service tokens (*implemented*)` after "Endpoints and enrollment" with a table row `| organization.service_tokens.manage | ✓ | – | – | – | – | the six-digit pairing code, once; the token only in the claim response | success/denied, target = pairing or token id |` and the sentence: "A service token authenticates as `service:<id>` with the fixed role `pulse_reader`; its successful reads are not audited (one `service_token.reads` row per token per hour instead), its denials and every claim and revocation are." Decisions table: `| Read-only service tokens | 6-digit pairing code, 15 min, single use, unauthenticated claim rate-limited 5/min/IP and 30/min; fixed pulse_reader role; hourly read summary | implemented |`.

- [ ] **Step 6: Commit**

```bash
git add internal/permissions docs/authorization-matrix.md
git commit -m "permissions: pulse_reader role and organization.service_tokens.manage"
```

---

### Task 2: Service tokens in the store

**Files:**
- Modify: `internal/store/migrations/migrations.go` (migration 36)
- Modify: `internal/store/models.go` (`TenantAccess.ServiceTokenID`, `ServiceToken`, `ServicePairing`, `ServiceTokenIssue`)
- Modify: `internal/store/store.go` (`TenancyStore` methods)
- Modify: `internal/store/tenant_access.go` (`run` resolves a service principal)
- Create: `internal/store/service_tokens.go`
- Test: `internal/store/service_tokens_test.go` (package `store_test` if the package's tests are external; check `tenant_*_test.go` and follow suit)

**Interfaces:**
- Consumes: `permissions.RolePulseReader`, `permissions.ServiceTokensManage` (Task 1).
- Produces on `TenancyStore`:
  - `CreateServicePairing(ctx, a TenantAccess) (*ServicePairing, error)`
  - `ClaimServiceToken(ctx, code, serviceName, ip string) (*ServiceTokenIssue, error)`
  - `AuthenticateServiceToken(ctx, token, ip string) (*ServiceToken, error)`
  - `ListServiceTokens(ctx, a TenantAccess) ([]ServiceToken, error)`
  - `RevokeServiceToken(ctx, a TenantAccess, id string) error`
  - `DenyService(ctx, a TenantAccess, action permissions.Action, detail string) error`
  - `RecordServiceTokenReads(ctx, organizationID, tokenID string, reads int) error`
- `TenantAccess` with `ServiceTokenID` set and `ActorID` empty is a service principal everywhere `run` is used.

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/service_tokens_test.go
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
// and returns the admin's TenantAccess. Copy the fixture the package's tenant tests use
// (CreateUser + CreateOrganizationWithAdmin) rather than inventing one.
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

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
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
	_, err = st.Tenancy().CreateEnvironment(ctx, svc, "prod")
	if !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("service write must be denied: %v", err)
	}
	denials := auditRows(t, st, string(permissions.EnvironmentCreate))
	if len(denials) != 1 || denials[0].Result != "denied" || denials[0].UserID != "service:"+tok.ID {
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
	if rows := auditRows(t, st, "service_token.revoke"); len(rows) != 1 || rows[0].Result != "success" || rows[0].Resource != tok.ID {
		t.Fatalf("revoke audit: %+v", rows)
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
```

Check against the real code before relying on it: `CreateOrganizationWithAdmin` and `PutMembership` signatures (`store.go:262,278`; `PutMembership` takes a `TenantRole` — convert the string as the package does), `CreateEnvironment`'s signature (grep `func (t \*tenancyStore) CreateEnvironment`), `AuditStore.ListAuditRecords`'s return shape, and the `User` fields a create needs. Adjust the fixture to the real names.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/store/ -run 'TestPairing|TestClaim|TestServicePrincipal|TestRecordServiceTokenReads'`
Expected: FAIL to compile.

- [ ] **Step 3: Migration 36**

Append to the registry:

```go
	{Version: 36, Name: "service_tokens", SQLite: serviceTokens, Postgres: strings.ReplaceAll(serviceTokens, "DATETIME", "TIMESTAMPTZ")},
```

and the constant:

```go
// serviceTokens holds another Ky product's read-only credential for one organization, and
// the single-use six-digit pairing codes that mint them. Only hashes are stored.
const serviceTokens = `CREATE TABLE service_tokens (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 64),
 token_hash TEXT NOT NULL UNIQUE,
 created_by TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL,
 last_used_at DATETIME,
 last_ip TEXT NOT NULL DEFAULT '',
 revoked_at DATETIME,
 revoked_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_service_tokens_org ON service_tokens(organization_id);
CREATE TABLE service_token_pairings (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 code_hash TEXT NOT NULL,
 created_by TEXT NOT NULL,
 created_at DATETIME NOT NULL,
 expires_at DATETIME NOT NULL,
 consumed_at DATETIME
);
CREATE INDEX idx_service_token_pairings_code ON service_token_pairings(code_hash);
`
```

(Confirm the organizations table is named `organizations` — `ReadOrganization` selects from it.)

- [ ] **Step 4: Models and interface**

`internal/store/models.go`: `TenantAccess` gains `ServiceTokenID string` with the comment `// set instead of ActorID for a service token; run resolves its fixed role`. Add:

```go
// ServiceToken is another Ky product's read-only credential for one organization. The
// secret is never stored: token_hash is its SHA-256.
type ServiceToken struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organization_id"`
	Name           string     `json:"name"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	LastIP         string     `json:"last_ip,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}

// ServicePairing is a six-digit code shown once to an organization administrator.
type ServicePairing struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ServiceTokenIssue is the claim response: the token, once, and the organization it reads.
type ServiceTokenIssue struct {
	Token        string       `json:"token"`
	Organization Organization `json:"organization"`
}
```

`internal/store/store.go`, `TenancyStore` gains:

```go
	CreateServicePairing(ctx context.Context, access TenantAccess) (*ServicePairing, error)
	ClaimServiceToken(ctx context.Context, code, serviceName, ip string) (*ServiceTokenIssue, error)
	AuthenticateServiceToken(ctx context.Context, token, ip string) (*ServiceToken, error)
	ListServiceTokens(ctx context.Context, access TenantAccess) ([]ServiceToken, error)
	RevokeServiceToken(ctx context.Context, access TenantAccess, id string) error
	// DenyService writes a denied audit row for a refusal the API decides itself (a service
	// token asking to follow a log); the store's own checks audit their denials in run.
	DenyService(ctx context.Context, access TenantAccess, action permissions.Action, detail string) error
	// RecordServiceTokenReads writes the hourly summary row for one token.
	RecordServiceTokenReads(ctx context.Context, organizationID, tokenID string, reads int) error
```

- [ ] **Step 5: `run` resolves a service principal**

In `internal/store/tenant_access.go`, change the start of `run`:

```go
	if (a.ActorID == "" && a.ServiceTokenID == "") || a.OrganizationID == "" {
		return ErrForbidden
	}
```

set `record.UserID` from a helper:

```go
// actor names the principal an audit row is about: a user id, or service:<token id>.
func (a TenantAccess) actor() string {
	if a.ServiceTokenID != "" {
		return "service:" + a.ServiceTokenID
	}
	return a.ActorID
}
```

(`UserID: a.actor()` in the record literal), and replace the membership lookup block (from `query := …` through the `permissions.Allows` check) with:

```go
	role, err := t.principalRole(ctx, tx, a, lock)
	if err == nil && !permissions.Allows(role, action) {
		err = ErrForbidden
	}
```

with:

```go
// principalRole is the live role of the caller inside the transaction: the membership row
// for a user (locked for a mutation), the token row for a service, whose role is fixed. No
// row means the URL scope is unverified: ErrForbidden and no tenant audit row.
func (t *tenancyStore) principalRole(ctx context.Context, tx *sql.Tx, a TenantAccess, lock bool) (string, error) {
	if a.ServiceTokenID != "" {
		var revoked sql.NullTime
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT revoked_at FROM service_tokens WHERE id=? AND organization_id=?`), a.ServiceTokenID, a.OrganizationID).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrForbidden
		}
		if err != nil {
			return "", err
		}
		if revoked.Valid {
			return "", ErrForbidden
		}
		return permissions.RolePulseReader, nil
	}
	query := `SELECT u.status,u.must_change_password,m.status,m.role FROM users u JOIN organization_memberships m ON m.user_id=u.id WHERE u.id=? AND m.organization_id=?`
	if lock && t.store.driver == "postgres" {
		query += " FOR UPDATE"
	} else if lock {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE organization_memberships SET status=status WHERE organization_id=? AND user_id=?`), a.OrganizationID, a.ActorID); err != nil {
			return "", err
		}
	}
	var status, memberStatus, role string
	var restricted bool
	err := tx.QueryRowContext(ctx, t.store.rebind(query), a.ActorID, a.OrganizationID).Scan(&status, &restricted, &memberStatus, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrForbidden
	}
	if err != nil {
		return "", err
	}
	if status != "active" || restricted || memberStatus != "active" {
		return "", ErrForbidden
	}
	return role, nil
}
```

Keep every comment of the original block that still applies (the SQLite reserved-lock explanation moves with the code). The environment check, `op`, the failure path and the success path stay. The success-row rule becomes:

```go
	if !lock && (!auditedReads[action] || a.ServiceTokenID != "") {
		return tx.Commit()
	}
```

with the comment "A service principal's reads are summarised hourly, not recorded one by one (docs/authorization-matrix.md, Service tokens)."

- [ ] **Step 6: `service_tokens.go`**

```go
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

const (
	serviceTokenBytes = 32
	// lastUsedEvery bounds the write a read costs: last_used_at and last_ip are stamped at
	// most once a minute per token, or when the address changes.
	lastUsedEvery = time.Minute
)

// pairingLife is a variable so a test can mint an already-expired code.
var pairingLife = 15 * time.Minute

// pairingCode draws six digits; a test may pin it.
var pairingCode = func() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

var serviceNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func validServiceName(s string) bool { return serviceNameRe.MatchString(s) }

func (t *tenancyStore) CreateServicePairing(ctx context.Context, a TenantAccess) (*ServicePairing, error) {
	code, err := pairingCode()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	p := &ServicePairing{ID: uuid.NewString(), Code: code, ExpiresAt: now.Add(pairingLife)}
	err = t.withTenantTarget(ctx, a, permissions.ServiceTokensManage, p.ID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO service_token_pairings (id,organization_id,code_hash,created_by,created_at,expires_at) VALUES (?,?,?,?,?,?)`), p.ID, a.OrganizationID, crypto.SHA256Hex([]byte(code)), a.ActorID, now, p.ExpiresAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ClaimServiceToken consumes exactly one live pairing, the oldest that matches, and mints
// that organization's token. Every refusal is ErrForbidden: the caller is unauthenticated and
// learns nothing about why. A code that names no live pairing writes no tenant row, because
// there is no organization to attribute it to; the API rate-limits and logs the attempt.
func (t *tenancyStore) ClaimServiceToken(ctx context.Context, code, serviceName, ip string) (*ServiceTokenIssue, error) {
	if len(code) != 6 || !validServiceName(serviceName) {
		return nil, ErrForbidden
	}
	now := time.Now().UTC()
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var pairingID, orgID, orgName string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT p.id,p.organization_id,o.name FROM service_token_pairings p JOIN organizations o ON o.id=p.organization_id WHERE p.code_hash=? AND p.consumed_at IS NULL AND p.expires_at>? ORDER BY p.created_at,p.id LIMIT 1`), crypto.SHA256Hex([]byte(code)), now).Scan(&pairingID, &orgID, &orgName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	// The conditional UPDATE is the lock: two claims of one code commit one token.
	res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE service_token_pairings SET consumed_at=? WHERE id=? AND consumed_at IS NULL`), now, pairingID)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, ErrForbidden
	}
	secret := make([]byte, serviceTokenBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	id := "svc_" + crypto.RandomHex(12)
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO service_tokens (id,organization_id,name,token_hash,created_by,created_at) VALUES (?,?,?,?,?,?)`), id, orgID, serviceName, crypto.SHA256Hex(secret), "pairing:"+pairingID, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), "service:"+id, "service_token.claim", id, "service="+serviceName, ip, now, "organization", orgID, "", uuid.NewString(), "success"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ServiceTokenIssue{Token: hex.EncodeToString(secret), Organization: Organization{ID: orgID, Name: orgName}}, nil
}

// AuthenticateServiceToken resolves a bearer to its live token. A revoked or unknown token is
// ErrForbidden; the API answers 401 either way.
func (t *tenancyStore) AuthenticateServiceToken(ctx context.Context, token, ip string) (*ServiceToken, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(token))
	if err != nil || len(raw) != serviceTokenBytes {
		return nil, ErrForbidden
	}
	var tok ServiceToken
	var lastUsed, revoked sql.NullTime
	err = t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT id,organization_id,name,created_by,created_at,last_used_at,last_ip,revoked_at FROM service_tokens WHERE token_hash=?`), crypto.SHA256Hex(raw)).Scan(&tok.ID, &tok.OrganizationID, &tok.Name, &tok.CreatedBy, &tok.CreatedAt, &lastUsed, &tok.LastIP, &revoked)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked.Valid) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if !lastUsed.Valid || now.Sub(lastUsed.Time) > lastUsedEvery || tok.LastIP != ip {
		if _, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE service_tokens SET last_used_at=?, last_ip=? WHERE id=? AND revoked_at IS NULL`), now, ip, tok.ID); err != nil {
			return nil, err
		}
		lastUsed = sql.NullTime{Time: now, Valid: true}
		tok.LastIP = ip
	}
	tok.LastUsedAt = &lastUsed.Time
	return &tok, nil
}

func (t *tenancyStore) ListServiceTokens(ctx context.Context, a TenantAccess) ([]ServiceToken, error) {
	out := []ServiceToken{}
	err := t.readTenant(ctx, a, permissions.ServiceTokensManage, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,organization_id,name,created_by,created_at,last_used_at,last_ip,revoked_at FROM service_tokens WHERE organization_id=? ORDER BY created_at DESC LIMIT 200`), a.OrganizationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tok ServiceToken
			var lastUsed, revoked sql.NullTime
			if err := rows.Scan(&tok.ID, &tok.OrganizationID, &tok.Name, &tok.CreatedBy, &tok.CreatedAt, &lastUsed, &tok.LastIP, &revoked); err != nil {
				return err
			}
			if lastUsed.Valid {
				tok.LastUsedAt = &lastUsed.Time
			}
			if revoked.Valid {
				tok.RevokedAt = &revoked.Time
			}
			out = append(out, tok)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (t *tenancyStore) RevokeServiceToken(ctx context.Context, a TenantAccess, id string) error {
	now := time.Now().UTC()
	return t.withTenantTarget(ctx, a, permissions.ServiceTokensManage, id, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE service_tokens SET revoked_at=?, revoked_by=? WHERE id=? AND organization_id=? AND revoked_at IS NULL`), now, a.ActorID, id, a.OrganizationID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return ErrNotFound
		}
		return nil
	})
}

func (t *tenancyStore) DenyService(ctx context.Context, a TenantAccess, action permissions.Action, detail string) error {
	return t.store.Audit().LogAudit(ctx, &AuditRecord{UserID: a.actor(), Action: string(action), Resource: a.OrganizationID, Details: detail, IPAddress: a.IPAddress, Scope: "organization", OrganizationID: a.OrganizationID, EnvironmentID: a.EnvironmentID, CorrelationID: a.CorrelationID, Result: "denied", CreatedAt: time.Now().UTC()})
}

func (t *tenancyStore) RecordServiceTokenReads(ctx context.Context, organizationID, tokenID string, reads int) error {
	return t.store.Audit().LogAudit(ctx, &AuditRecord{UserID: "service:" + tokenID, Action: "service_token.reads", Resource: tokenID, Details: fmt.Sprintf("reads=%d", reads), Scope: "organization", OrganizationID: organizationID, CorrelationID: uuid.NewString(), Result: "success", CreatedAt: time.Now().UTC()})
}
```

The revoke audit action: `withTenantTarget` audits `organization.service_tokens.manage`; the test expects `service_token.revoke`. Ruling for the implementer: keep the named action as the audit `Action` (that is the matrix's rule) and change the test's expectation to `string(permissions.ServiceTokensManage)` with `Resource == tok.ID`; do the same for the pairing (`Resource == p.ID`).

Export the two test hooks in `internal/store/export_test.go` (create it if absent; package `store`):

```go
package store

import "time"

func SetPairingCodeForTest(code string) (restore func()) {
	old := pairingCode
	pairingCode = func() (string, error) { return code, nil }
	return func() { pairingCode = old }
}

func SetPairingLifeForTest(d time.Duration) (restore func()) {
	old := pairingLife
	pairingLife = d
	return func() { pairingLife = old }
}
```

Check whether `LogAudit` in this repo's `auditStore` inserts the scope/organization columns (kyPulse's does not; KyYard's `run` inserts them directly). If `LogAudit` ignores `Scope`/`OrganizationID`, write `DenyService` and `RecordServiceTokenReads` with the same direct INSERT `run` uses (extract a small `insertAudit(ctx, exec, record)` helper in `tenant_access.go` and call it from `run`, `ClaimServiceToken`, `DenyService` and `RecordServiceTokenReads`).

- [ ] **Step 7: Run the tests on both dialects**

Run: `go test -race ./internal/store/ -run 'TestPairing|TestClaim|TestServicePrincipal|TestRecordServiceTokenReads'` then the whole package `go test -race ./internal/store/`, then Postgres: `docker run -d --rm --name kyyard-pg -e POSTGRES_USER=ci -e POSTGRES_PASSWORD=ci -e POSTGRES_DB=ci_test -p 55432:5432 postgres:17-alpine`, `KY_TEST_POSTGRES_DSN='postgres://ci:ci@127.0.0.1:55432/ci_test?sslmode=disable' go test -race -count=1 ./internal/store/`, `docker stop kyyard-pg` (check `internal/testdb` for the exact env var name).
Expected: PASS.

- [ ] **Step 8: Docs**

`internal/store/AGENTS.md` Local Contracts, one bullet: "Service tokens (migration 36): `TenantAccess.ServiceTokenID` is a second principal; `principalRole` resolves it to the fixed role `pulse_reader` from a live, unrevoked token row inside the same transaction, so revocation takes effect on the next statement. Service reads write no success row (`RecordServiceTokenReads` writes the hourly summary); denials are audited as `service:<id>`. `ClaimServiceToken` consumes the oldest live pairing for a code with a conditional UPDATE and audits `service_token.claim`; the code and token exist only as SHA-256. `DenyService` records an API-level refusal."

- [ ] **Step 9: Commit**

```bash
git add internal/store
git commit -m "store: service tokens, pairing codes and the pulse_reader principal"
```

---

### Task 3: API routes, bearer principal, claim limits, read summaries, `/healthz`

**Files:**
- Modify: `internal/api/tenant_handlers.go` (`tenantRoute`)
- Create: `internal/api/service_token_handlers.go`
- Modify: `internal/api/server.go` (routes, `Server` fields, `/healthz`)
- Modify: `internal/api/log_handlers.go` (`follow` refused for a service principal)
- Create: `internal/api/service_reads.go` (counter + `RunServiceReadSummaries`)
- Modify: `cmd/server/main.go` (start and await the summary loop), `go.mod`/`go.sum` (ky-primitives v0.9.0)
- Test: `internal/api/service_token_test.go`

**Interfaces:**
- Consumes: Task 2's store methods; `permissions.ServiceTokensManage`.
- Produces: routes `POST /api/service-tokens/claim`, `POST /api/organizations/{organization}/service-tokens/pairings`, `GET /api/organizations/{organization}/service-tokens`, `DELETE /api/organizations/{organization}/service-tokens/{token}`, `GET /healthz`; `Server.RunServiceReadSummaries(ctx, done chan<- struct{})`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/api/service_token_test.go
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// pairAndClaim mints a pairing as the organization administrator and claims it, returning
// the bearer. srv/st/cookie come from the file's usual fixture (newTestServer + loginAs +
// an organization the admin administers; copy the fixture authz_test.go uses).
func pairAndClaim(t *testing.T, srv *api.Server, cookie *http.Cookie, csrf string, org string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/organizations/"+org+"/service-tokens/pairings", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("pairing: %d %s", w.Code, w.Body)
	}
	var p struct{ Code string `json:"code"` }
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("POST", "/api/service-tokens/claim", strings.NewReader(`{"pairing_code":"`+p.Code+`","service_name":"kypulse"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	var issue struct{ Token string `json:"token"`; Organization struct{ ID string `json:"id"` } `json:"organization"` }
	_ = json.Unmarshal(w.Body.Bytes(), &issue)
	if issue.Organization.ID != org || len(issue.Token) != 64 {
		t.Fatalf("issue: %+v", issue)
	}
	return issue.Token
}

func bearer(srv *api.Server, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestServiceTokenReadsAndIsRefusedWrites(t *testing.T) {
	srv, st, cookie, csrf, org := tenantAdminFixture(t) // adapt to the file's fixture
	token := pairAndClaim(t, srv, cookie, csrf, org)
	for _, path := range []string{"/api/organizations/" + org, "/api/organizations/" + org + "/environments", "/api/organizations/" + org + "/endpoints", "/api/organizations/" + org + "/audit"} {
		if w := bearer(srv, "GET", path, token); w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
		}
	}
	if w := bearer(srv, "GET", "/api/organizations/"+org+"/members", token); w.Code != http.StatusForbidden {
		t.Fatalf("members must be refused: %d", w.Code)
	}
	if w := bearer(srv, "GET", "/api/organizations/org_other", token); w.Code != http.StatusForbidden {
		t.Fatalf("foreign organization: %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/api/organizations/"+org+"/environments", strings.NewReader(`{"name":"prod"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("service write: %d %s", w.Code, w.Body)
	}
	rows, _, _ := st.Audit().ListAuditRecords(context.Background(), 0, 50)
	var denied bool
	for _, r := range rows {
		denied = denied || (r.Action == "environment.create" && r.Result == "denied" && strings.HasPrefix(r.UserID, "service:"))
	}
	if !denied {
		t.Fatal("no denial row for the service write")
	}
	if w := bearer(srv, "DELETE", "/api/organizations/"+org+"/service-tokens/svc_x", token); w.Code != http.StatusForbidden {
		t.Fatalf("a token must not manage tokens: %d", w.Code)
	}
	if w := bearer(srv, "GET", "/api/organizations/"+org, "deadbeef"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad bearer: %d", w.Code)
	}
}

func TestServiceTokenCannotFollowLogs(t *testing.T) {
	srv, st, cookie, csrf, org := tenantAdminFixture(t)
	token := pairAndClaim(t, srv, cookie, csrf, org)
	w := bearer(srv, "GET", "/api/organizations/"+org+"/endpoints/ep_none/containers/c/logs?follow=1", token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("follow must be refused before any endpoint lookup: %d %s", w.Code, w.Body)
	}
	rows, _, _ := st.Audit().ListAuditRecords(context.Background(), 0, 50)
	var denied bool
	for _, r := range rows {
		denied = denied || (r.Action == "container.logs" && r.Result == "denied" && r.Details == "follow")
	}
	if !denied {
		t.Fatal("follow refusal must be audited")
	}
}

func TestClaimIsRateLimited(t *testing.T) {
	srv, _, _, _, _ := tenantAdminFixture(t)
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest("POST", "/api/service-tokens/claim", strings.NewReader(`{"pairing_code":"000000","service_name":"kypulse"}`)))
		if w.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("POST", "/api/service-tokens/claim", strings.NewReader(`{"pairing_code":"000000","service_name":"kypulse"}`)))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt from one address: %d", w.Code)
	}
}

func TestRevokedTokenIsUnauthorized(t *testing.T) {
	srv, _, cookie, csrf, org := tenantAdminFixture(t)
	token := pairAndClaim(t, srv, cookie, csrf, org)
	req := httptest.NewRequest("GET", "/api/organizations/"+org+"/service-tokens", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	var list []struct{ ID string `json:"id"` }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != http.StatusOK || len(list) != 1 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	req = httptest.NewRequest("DELETE", "/api/organizations/"+org+"/service-tokens/"+list[0].ID, nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := bearer(srv, "GET", "/api/organizations/"+org, token); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked bearer: %d", w.Code)
	}
}

func TestHealthzOnTheContract(t *testing.T) {
	srv, _, _, _, _ := tenantAdminFixture(t)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	var body struct{ Schema, Service, Status string; Checks []struct{ Name, Status string } }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("healthz: %d %s", w.Code, w.Body)
	}
	if body.Schema != "ky.health/1" || body.Service != "kyyard" || body.Status != "ok" || len(body.Checks) != 1 || body.Checks[0].Name != "database" {
		t.Fatalf("body: %+v", body)
	}
}
```

`tenantAdminFixture` must return a server, its store, a logged-in organization administrator's session cookie, that session's CSRF token (the `ky_csrf` cookie value from the login response) and the organization id. Build it from the file's existing helpers (`loginAs`, `seedUser`, `CreateOrganizationWithAdmin`) — read `authz_test.go` and one tenant handler test to see how they obtain the CSRF value.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/api/ -run 'TestServiceToken|TestClaimIsRateLimited|TestRevokedToken|TestHealthzOnTheContract'`
Expected: FAIL (404s; `/healthz` 404 or SPA fallback).

- [ ] **Step 3: Bearer principal in `tenantRoute`**

In `internal/api/tenant_handlers.go`, after the headers are set and before `AuthenticateRequest`:

```go
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			s.serviceRoute(w, r, correlation, token, h)
			return
		}
```

and add:

```go
// serviceRoute is tenantRoute for a service token: the token names the organization it may
// read, so a URL naming another one is refused here (no tenant row: unverified scope, like a
// non-member). The store's run applies the pulse_reader role to whatever the handler asks.
func (s *Server) serviceRoute(w http.ResponseWriter, r *http.Request, correlation, token string, h func(http.ResponseWriter, *http.Request, store.TenantAccess)) {
	ip := s.requestIP(r)
	tok, err := s.store.Tenancy().AuthenticateServiceToken(r.Context(), token, ip)
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}
	org, env := r.PathValue("organization"), r.PathValue("environment")
	if org == "" || len(org) > 64 || len(env) > 64 {
		s.writeError(w, http.StatusBadRequest, "Invalid tenant scope")
		return
	}
	if org != tok.OrganizationID {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "Tenant access denied", "code": "tenant_access_denied"})
		return
	}
	s.serviceReads.add(tok.OrganizationID, tok.ID)
	h(w, r, store.TenantAccess{ServiceTokenID: tok.ID, OrganizationID: org, EnvironmentID: env, CorrelationID: correlation, IPAddress: ip})
}
```

- [ ] **Step 4: Read counter and summary loop**

```go
// internal/api/service_reads.go
package api

import (
	"context"
	"sync"
	"time"
)

// serviceReadSummaryEvery is how often counted service reads become one audit row per token.
const serviceReadSummaryEvery = time.Hour

// serviceReadCounter counts reads per service token between summaries. In memory: one
// process is the supported deployment, and a lost hour of counts costs nothing an auditor
// acts on.
type serviceReadCounter struct {
	mu     sync.Mutex
	counts map[string]serviceReads
}

type serviceReads struct {
	organizationID string
	reads          int
}

func (c *serviceReadCounter) add(organizationID, tokenID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]serviceReads{}
	}
	e := c.counts[tokenID]
	e.organizationID, e.reads = organizationID, e.reads+1
	c.counts[tokenID] = e
}

func (c *serviceReadCounter) drain() map[string]serviceReads {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.counts
	c.counts = nil
	return out
}

// RunServiceReadSummaries writes one service_token.reads row per token per hour and once
// more at shutdown, on a context detached from the loop so the last flush lands. It closes
// done only when it returns; runServer waits on it before the store closes.
func (s *Server) RunServiceReadSummaries(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(serviceReadSummaryEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.flushServiceReads(ctx)
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			s.flushServiceReads(flushCtx)
			cancel()
			return
		}
	}
}

func (s *Server) flushServiceReads(ctx context.Context) {
	for tokenID, e := range s.serviceReads.drain() {
		if err := s.store.Tenancy().RecordServiceTokenReads(ctx, e.organizationID, tokenID, e.reads); err != nil {
			log.Printf("[SERVICE] read summary for %s not written: %v", tokenID, err)
		}
	}
}
```

(add `"log"` to the imports; the repo logs with `log.Printf`). Add `serviceReads serviceReadCounter` to `Server`.

- [ ] **Step 5: Handlers**

```go
// internal/api/service_token_handlers.go
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

const (
	claimPerIP     = 5
	claimGlobal    = 30
	claimWindow    = time.Minute
	claimBodyBytes = 1 << 10
)

// handleClaimServiceToken is unauthenticated: a six-digit code inside its 15 minutes is the
// whole proof, so the limits are what keep guessing impractical. Both are checked before the
// body is read, and every refusal is the same 403.
func (s *Server) handleClaimServiceToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip := s.requestIP(r)
	if !s.allowAttempt("service-claim:"+ip, claimPerIP, claimWindow) || !s.allowAttempt("service-claim", claimGlobal, claimWindow) {
		s.writeError(w, http.StatusTooManyRequests, "Too many pairing attempts; wait a minute")
		return
	}
	var req struct {
		PairingCode string `json:"pairing_code"`
		ServiceName string `json:"service_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, claimBodyBytes)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}
	issue, err := s.store.Tenancy().ClaimServiceToken(r.Context(), req.PairingCode, req.ServiceName, ip)
	if err != nil {
		s.writeError(w, http.StatusForbidden, "Pairing refused")
		return
	}
	s.writeJSON(w, http.StatusOK, issue)
}

func (s *Server) handleCreateServicePairing(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	p, err := s.store.Tenancy().CreateServicePairing(r.Context(), a)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": p.ID, "code": p.Code, "expires_at": p.ExpiresAt,
		"disclosure": "Shown once. Enter it in kyPulse within 15 minutes; it pairs one kyPulse to this organization."})
}

func (s *Server) handleServiceTokens(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	list, err := s.store.Tenancy().ListServiceTokens(r.Context(), a)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleRevokeServiceToken(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id := r.PathValue("token")
	if id == "" || len(id) > 64 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().RevokeServiceToken(r.Context(), a, id); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Routes in `server.go` `routes()`, next to the members routes:

```go
	s.mux.HandleFunc("POST /api/service-tokens/claim", s.handleClaimServiceToken)
	s.mux.HandleFunc("POST /api/organizations/{organization}/service-tokens/pairings", s.tenantRoute(s.handleCreateServicePairing))
	s.mux.HandleFunc("GET /api/organizations/{organization}/service-tokens", s.tenantRoute(s.handleServiceTokens))
	s.mux.HandleFunc("DELETE /api/organizations/{organization}/service-tokens/{token}", s.tenantRoute(s.handleRevokeServiceToken))
```

CSRF: the claim route carries no session cookie, so the CSRF check does not apply; confirm `csrfExempt` needs no change (it keys on `hasSessionCookie`).

- [ ] **Step 6: `follow` refused for a service principal**

In `handleContainerLogs` and `handlePodLogs`, right after `parseLogQuery` succeeds:

```go
	if a.ServiceTokenID != "" && q.follow {
		// A service token reads a bounded slice of a log; an open stream is a session, and a
		// session belongs to a person.
		_ = s.store.Tenancy().DenyService(r.Context(), a, permissions.ContainerLogs, "follow")
		s.tenantError(w, store.ErrForbidden)
		return
	}
```

(import `permissions`).

- [ ] **Step 7: `/healthz` on the contract**

`go get github.com/Busnes-app/ky-primitives@v0.9.0 && go mod tidy && go build ./...` — if v0.9.0 changed an API this repo uses (`capsule`, `password`, `recoveryclient`), stop and report NEEDS_CONTEXT with the compile error.

`Server` gains `health *logging.Logger`; in `NewServer`: `health, _ := logging.New(logging.Config{App: "kyyard", Out: os.Stderr})` (the handler panics on nil; a construction error here is a programming error, so `if err != nil { panic(err) }`). Register:

```go
	// The suite health contract (ky.health/1) for kyPulse; /health/live and /health/ready
	// stay for compose and the binary healthcheck.
	s.mux.Handle("GET /healthz", health.Handler("kyyard", s.health, health.Check{Name: "database", Run: s.store.Ping}))
```

Check that the SPA fallback / method handling does not shadow `GET /healthz` (the tests will tell).

- [ ] **Step 8: Wire the summary loop**

In `cmd/server/main.go` `runServer`, beside `RunValidations`:

```go
	serviceReadsDone := make(chan struct{})
	go srv.RunServiceReadSummaries(ctx, serviceReadsDone)
```

and add `serviceReadsDone` to the same wait the other loops use before the store closes (read the surrounding code and the root `AGENTS.md` paragraph on loop waits; the wait runs under the existing `backupWaitTimeout` context). If there is a test enumerating awaited loops (`TestServerRunsAndAwaitsTheValidationLoop`), add the same for this loop.

- [ ] **Step 9: Run**

Run: `go test -race ./internal/api/ ./cmd/server/` (SQLite) and the Postgres run for `./internal/api/`.
Expected: PASS.

- [ ] **Step 10: Docs**

`internal/api/AGENTS.md`: a bullet — "`tenantRoute` accepts `Authorization: Bearer <service token>` and dispatches through `serviceRoute`: the token's organization must equal the URL's, the principal is `TenantAccess{ServiceTokenID}` and the store applies `pulse_reader`; reads are counted and `RunServiceReadSummaries` writes one `service_token.reads` row per token per hour (and at shutdown). `POST /api/service-tokens/claim` is unauthenticated, limited 5/min/IP and 30/min (`allowAttempt`), answers 403 for every refusal and 200 `{token, organization}` once. Pairings, list and revoke are `organization.service_tokens.manage` tenant routes. `follow=1` with a service token is 403 with a `container.logs` denial row. `GET /healthz` is `ky-primitives/health` (`ky.health/1`, check `database`); `/health/live` and `/health/ready` are unchanged." Root `AGENTS.md`: the `cmd/server` loop paragraph names `RunServiceReadSummaries` among the awaited loops.

- [ ] **Step 11: Commit**

```bash
git add internal/api cmd/server go.mod go.sum AGENTS.md
git commit -m "api: service-token principal, pairing claim, read summaries and /healthz"
```

---

### Task 4: Organization UI, smoke test, README

**Files:**
- Create: `web/src/components/ServiceTokens.tsx`, `web/src/components/ServiceTokens.test.tsx`
- Modify: `web/src/tenant.ts` (`ServiceToken`, `ServicePairing` types), `web/src/pages/Organization.tsx` or the members page (render the panel for `organization_admin`), `web/dist` (rebuild)
- Modify: `scripts/smoke-test.sh`, `README.md`, `web/AGENTS.md`

- [ ] **Step 1: Types**

`web/src/tenant.ts`:

```ts
export interface ServiceToken { id: string; organization_id: string; name: string; created_by: string; created_at: string; last_used_at?: string; last_ip?: string; revoked_at?: string }
export interface ServicePairing { id: string; code: string; expires_at: string; disclosure: string }
export const canManageServiceTokens = (role: string | undefined) => role === 'organization_admin';
```

- [ ] **Step 2: Write the failing component test**

```tsx
// web/src/components/ServiceTokens.test.tsx
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ServiceTokens } from './ServiceTokens';

const tokens = [{ id: 'svc_1', organization_id: 'org_a', name: 'kypulse', created_by: 'pairing:p1', created_at: '2026-09-27T10:00:00Z', last_used_at: '2026-09-27T11:00:00Z', last_ip: '10.0.0.5' }];

function stub(extra: Array<[RegExp, unknown, number?]> = []) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const key = `${init?.method ?? 'GET'} ${String(input)}`;
    for (const [re, body, status] of extra) if (re.test(key)) return new Response(status === 204 ? null : JSON.stringify(body), { status: status ?? 200 });
    if (key === 'GET /api/organizations/org_a/service-tokens') return new Response(JSON.stringify(tokens));
    throw new Error(key);
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('ServiceTokens', () => {
  it('lists tokens and shows a pairing code once', async () => {
    stub([[/^POST \/api\/organizations\/org_a\/service-tokens\/pairings$/, { id: 'p2', code: '123456', expires_at: '2026-09-27T12:15:00Z', disclosure: 'Shown once.' }, 201]]);
    render(<ServiceTokens org="org_a" />);
    expect(await screen.findByText('kypulse')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Pair kyPulse' }));
    expect(await screen.findByText('123456')).toBeTruthy();
    expect(screen.getByText(/Shown once/)).toBeTruthy();
  });

  it('revokes after confirmation and names the kyPulse side', async () => {
    const fetchMock = stub([[/^DELETE \/api\/organizations\/org_a\/service-tokens\/svc_1$/, null, 204]]);
    vi.stubGlobal('confirm', vi.fn(() => true));
    render(<ServiceTokens org="org_a" />);
    await screen.findByText('kypulse');
    expect(screen.getByText(/Unpair in kyPulse as well/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Revoke' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => (init as RequestInit)?.method === 'DELETE')).toBe(true));
  });
});
```

- [ ] **Step 3: Run it to verify it fails**

Run: `cd web && npx vitest run src/components/ServiceTokens.test.tsx`
Expected: FAIL, cannot resolve.

- [ ] **Step 4: Component**

```tsx
// web/src/components/ServiceTokens.tsx
import { useState } from 'react';
import { tenantWrite, useTenantResource, type ServicePairing, type ServiceToken } from '../tenant';
import { secureFetch } from '../api';
import { StateNotice } from './StateNotice'; // use the file the Endpoints component imports it from

export function ServiceTokens({ org }: { org: string }) {
  const base = `/api/organizations/${encodeURIComponent(org)}`;
  const tokens = useTenantResource<ServiceToken[]>(`${base}/service-tokens`);
  const [pairing, setPairing] = useState<ServicePairing | null>(null);
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);

  const pair = async () => {
    setBusy(true);
    setMessage('');
    try {
      const resp = await secureFetch(`${base}/service-tokens/pairings`, { method: 'POST' });
      if (!resp.ok) { setMessage('Could not create a pairing code.'); return; }
      setPairing(await resp.json());
    } catch { setMessage('Offline: the server could not be reached.'); } finally { setBusy(false); }
  };
  const revoke = async (t: ServiceToken) => {
    if (!window.confirm(`Revoke "${t.name}"? kyPulse stops reading this organization immediately. Unpair in kyPulse as well; revoking here does not.`)) return;
    setBusy(true);
    const err = await tenantWrite(`${base}/service-tokens/${encodeURIComponent(t.id)}`, 'DELETE');
    setBusy(false);
    setMessage(err);
    if (!err) tokens.reload();
  };

  return (
    <section className="panel" aria-labelledby="service-tokens-heading">
      <div className="panel-header" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12 }}>
        <h2 id="service-tokens-heading" style={{ fontSize: 16 }}>Service tokens</h2>
        <button disabled={busy} onClick={() => void pair()}>Pair kyPulse</button>
      </div>
      <p>A service token lets kyPulse read this organization: endpoints, inventory, samples, container logs and the audit feed. It can change nothing. Unpair in kyPulse as well when you revoke one here; each side keeps its own step.</p>
      {pairing && (
        <div className="dr-alert dr-alert-warn" role="region" aria-label="Pairing code">
          <p><strong>Shown once.</strong> Enter this code in kyPulse before {new Date(pairing.expires_at).toLocaleTimeString()}:</p>
          <pre className="font-mono" style={{ fontSize: 24, letterSpacing: 4 }}>{pairing.code}</pre>
          <p>{pairing.disclosure}</p>
          <button className="btn-secondary" onClick={() => setPairing(null)}>Dismiss</button>
        </div>
      )}
      {message && <p role="alert">{message}</p>}
      <StateNotice state={tokens.state} onRetry={tokens.reload} />
      {tokens.state === 'ready' && tokens.data && (tokens.data.length === 0 ? <p>No service tokens. Pair kyPulse to create one.</p> : (
        <table>
          <thead><tr><th>Service</th><th>Created</th><th>Last used</th><th>From</th><th>Status</th><th /></tr></thead>
          <tbody>
            {tokens.data.map((t) => (
              <tr key={t.id}>
                <td>{t.name}</td>
                <td>{new Date(t.created_at).toLocaleString()}</td>
                <td>{t.last_used_at ? new Date(t.last_used_at).toLocaleString() : 'never'}</td>
                <td className="font-mono">{t.last_ip ?? ''}</td>
                <td>{t.revoked_at ? 'revoked' : 'active'}</td>
                <td>{!t.revoked_at && <button className="btn-secondary" disabled={busy} onClick={() => void revoke(t)}>Revoke</button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ))}
    </section>
  );
}
```

Match the repo's existing table and notice components (`StateNotice`, `EmptyNotice`) and CSS classes as `Endpoints.tsx` uses them; the code above names them as that file does — verify the import paths.

Render it on the organization page for administrators: in `web/src/App.tsx`'s `'members'` route (or wherever the members page composes), add `{canManageServiceTokens(role) && <ServiceTokens org={route.org} />}` below `<Members …/>` (find how the members page learns the caller's tenant role — `MemberOrganization.role` from `/api/organizations`).

- [ ] **Step 5: Run web tests, build, rebuild dist**

Run: `cd web && npx vitest run && npm run build`
Expected: PASS; `web/dist` changes staged for the commit.

- [ ] **Step 6: Smoke test**

In `scripts/smoke-test.sh`, in the authenticated section (after the CSRF value is known and an organization exists — the smoke test's admin is the bootstrap platform admin; find how it obtains an organization id and an organization-admin session, e.g. the initial organization `Tenancy.Initialize` creates; if the platform admin holds no membership there, create a membership through the admin routes the script already exercises), add:

```bash
echo "==> Service tokens"
PAIR="$(curl -s -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X POST "$BASE/api/organizations/$ORG/service-tokens/pairings")"
contains "pairing code is six digits" "$(echo "$PAIR" | grep -o '"code":"[0-9]\{6\}"')" '"code":"'
CODE="$(echo "$PAIR" | sed -n 's/.*"code":"\([0-9]\{6\}\)".*/\1/p')"
CLAIM="$(curl -s -X POST -H 'Content-Type: application/json' -d '{"pairing_code":"'"$CODE"'","service_name":"kypulse"}' "$BASE/api/service-tokens/claim")"
contains "claim returns a token" "$CLAIM" '"token":"'
TOKEN="$(echo "$CLAIM" | sed -n 's/.*"token":"\([0-9a-f]\{64\}\)".*/\1/p')"
check "second claim is refused" "$(status -X POST -H 'Content-Type: application/json' -d '{"pairing_code":"'"$CODE"'","service_name":"kypulse"}' "$BASE/api/service-tokens/claim")" "403"
check "bearer reads the organization" "$(status -H "Authorization: Bearer $TOKEN" "$BASE/api/organizations/$ORG")" "200"
check "bearer reads the audit feed" "$(status -H "Authorization: Bearer $TOKEN" "$BASE/api/organizations/$ORG/audit")" "200"
check "bearer cannot list members" "$(status -H "Authorization: Bearer $TOKEN" "$BASE/api/organizations/$ORG/members")" "403"
check "bearer cannot create an environment" "$(status -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"name":"x"}' "$BASE/api/organizations/$ORG/environments")" "403"
check "bearer cannot follow a log" "$(status -H "Authorization: Bearer $TOKEN" "$BASE/api/organizations/$ORG/endpoints/ep_none/containers/c/logs?follow=1")" "403"
TOKENS="$(curl -s -b "$WORK/cookies" "$BASE/api/organizations/$ORG/service-tokens")"
TOKEN_ID="$(echo "$TOKENS" | sed -n 's/.*"id":"\(svc_[0-9a-f]*\)".*/\1/p' | head -1)"
check "admin revokes the token" "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X DELETE "$BASE/api/organizations/$ORG/service-tokens/$TOKEN_ID")" "204"
check "revoked bearer is 401" "$(status -H "Authorization: Bearer $TOKEN" "$BASE/api/organizations/$ORG")" "401"
contains "healthz serves ky.health/1" "$(curl -s "$BASE/healthz")" '"schema":"ky.health/1"'
```

Run: `make ci`
Expected: passes.

- [ ] **Step 7: README and web DOX**

`README.md`: a short "Pair kyPulse" subsection under the organization/administration docs: Members page → Service tokens → Pair kyPulse → enter the six-digit code in kyPulse within 15 minutes; what the token can read; revoke here and unpair in kyPulse (two steps, two admins); `/healthz` for external monitors. `web/AGENTS.md`: the members page renders `ServiceTokens` for organization administrators; the pairing code is shown once.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "web, smoke, docs: pair kyPulse from the organization page"
```
