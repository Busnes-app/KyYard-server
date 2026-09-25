# Registries and Registry Client Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Per-organization registries with write-only encrypted credentials and an audited anonymous-pull opt-in, plus a server-side registry client that resolves an image reference to its current manifest digest safely (HTTPS only, token flows, no redirects, egress guard). Nothing pulls yet.

**Architecture:** Migration 26 adds `registries` and `organizations.anonymous_pull_enabled`. The store owns sealing (AES-GCM bound to org and row), CRUD under `registry.manage`, reads under `registry.read`, and `ResolveRegistryAccess` for later slices. `internal/registry` is a dependency-free client with typed errors and an address-pinned dialer. `internal/api` exposes four routes; the web gets a Registries panel on the organization page.

**Tech Stack:** Go 1.25 (`net/http`, `net/netip`, `crypto/tls` for the test registry), SQLite/PostgreSQL inline migrations, React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-23-registries-design.md`

## Global Constraints

- Worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/registries`, branch `feat/registries` (from master after #57). Never edit the root checkout; never use bare `git stash`.
- Store changes tested on SQLite and PostgreSQL (`KY_TEST_POSTGRES_DSN=postgres://postgres:<pw>@127.0.0.1:15440/kyyard?sslmode=disable`; password from `docker inspect kyyard-access-pg`, never printed).
- Credentials: sealed with `crypto.EncryptAESGCM` under `crypto.DeriveKey(key, string(json.Marshal([]any{"kyyard/registry-credential/v1", org, registryID})))`; never returned by any route, list or audit; the row reports `has_credential`. Secret ≤ 4 KiB, UTF-8, no NUL. Username ≤ 255.
- Host normalisation (as landed, `store.NormalizeRegistryHost`): lowercase; `index.docker.io` and `registry-1.docker.io` → `docker.io`; empty is refused. Like `registry.ParseReference`, only `localhost` or a name with a `.` or a port is a registry host. The name is DNS labels (`[a-z0-9]`, inner `-`, no empty label or leading/trailing `-`), at most 253 bytes; the port is 1..65535 in canonical decimal (no leading zero). Name 1..64 display characters (`protocol.CleanText`).
- Permissions: `registry.read` all five roles; `registry.manage` organization admin only.
- Client: HTTPS only; 15 s per request; ≤ 4 MiB bodies; no redirects (`http.ErrUseLastResponse`); one token request per resolve; credentials only to the configured host and to the realm that host advertised, never on a redirect; loopback never allowed; private/link-local/CGNAT only with `AllowPrivate`; the dialer connects to the address that passed the check.
- Typed errors: `ErrUnauthorized`, `ErrNotFound`, `ErrRateLimited`, `ErrUnavailable`, `ErrDigestMismatch`, `ErrPrivateDestination`, `ErrInvalidReference`. No response body text in any error.
- Audit details: registry writes `host=<h> allow_private=<bool> credential=set|kept|cleared`; policy change `old=<bool> new=<bool>`.
- `gofmt -w` touched Go files; every commit ends with exactly `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. A registry host updated from public to private: an existing credential must not travel to the new address until `allow_private` is explicitly set (the client refuses by default). Task 1 stores `allow_private`; Task 2 refuses; Task 3 test "private host without opt-in".
2. Token realm on a different host than the registry (Docker Hub): the credential goes to the realm exactly once, over HTTPS, and never to a third host named by a redirect from the realm. Task 2 tests "bearer with credential" and "realm redirect refused".
3. `docker.io` reference forms (`nginx:1`, `library/nginx:1`, `docker.io/library/nginx:1`, `index.docker.io/nginx`) all map to host `docker.io`, repository `library/nginx`. Task 2 `TestParseReference`.
4. A `Credential` value must not outlive the call: the client keeps no copy; the store decrypts inside the transaction and returns it only from `ResolveRegistryAccess`. Task 1 test "list never carries a secret"; Task 2 reviewer check.
5. Moving ciphertext between rows or organizations must fail to decrypt. Task 1 test "sealing binding".

---

### Task 1: Migration 26, permissions, store

**Files:**
- Modify: `internal/store/migrations/migrations.go` (version 26), `internal/store/tenancy_test.go:211` (drop `registries`, add `26`)
- Modify: `internal/permissions/permissions.go` (`RegistryRead`, `RegistryManage`), `internal/permissions/permissions_test.go`
- Create: `internal/store/registries.go`, `internal/store/registries_test.go`
- Modify: `internal/store/store.go` (errors `ErrRegistryNotConfigured`; Tenancy interface additions), `internal/store/models.go` (types), `internal/store/application_backup_test.go` (drill)

**Interfaces (produces):**

```go
type Registry struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Host           string    `json:"host"`
	Name           string    `json:"name"`
	Username       string    `json:"username"`
	HasCredential  bool      `json:"has_credential"`
	AllowPrivate   bool      `json:"allow_private"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
type RegistryInput struct {
	Host         string  `json:"host"`
	Name         string  `json:"name"`
	Username     string  `json:"username"`
	Credential   *string `json:"credential"` // nil keep, "" clear, else set
	AllowPrivate bool    `json:"allow_private"`
}
type RegistryPolicy struct{ AnonymousPullEnabled bool `json:"anonymous_pull_enabled"` }
type RegistryAccess struct {
	Registry   *Registry
	Credential *registry.Credential // nil when anonymous or none stored
	Anonymous  bool                 // true when the host has no row and the opt-in allowed it
}
func NormalizeRegistryHost(host string) (string, error)
// Tenancy:
ListRegistries(ctx, a) ([]Registry, error)
PutRegistry(ctx, a, in RegistryInput, key []byte) (*Registry, error)
DeleteRegistry(ctx, a, id string) error
ReadRegistryPolicy(ctx, a) (RegistryPolicy, error)
SetAnonymousPull(ctx, a, enabled bool) error
ResolveRegistryAccess(ctx, a, ref string, key []byte) (*RegistryAccess, error)
```

`ResolveRegistryAccess` uses `registry.ParseReference` (Task 2) for the host; to avoid a cycle, the store imports `internal/registry` (which imports nothing from the store). Implement Task 2's `ParseReference` first if the compile order matters; the plan orders Task 1 first because Task 2's client tests do not need the store, but the store's `ResolveRegistryAccess` needs `ParseReference` and `registry.Credential` — so create the `internal/registry` package with only `ParseReference`, `Reference` and `Credential` in Task 1, and add the client in Task 2.

- [ ] **Step 1: Failing tests** in `registries_test.go` (fixture: `tenantAtomicStore`, an org admin `a`, a 32-byte key): put creates and lists with `has_credential true` and no secret field in JSON (`json.Marshal` of the list must not contain the secret); put again with `Credential nil` keeps it, `""` clears; host normalisation and grammar; duplicate host updates in place; delete; roles (env admin/operator/developer/read-only can list, cannot put/delete/set policy); policy read/set with audit details `old=false new=true`; `ResolveRegistryAccess` for a configured host returns the decrypted credential, for `docker.io` shorthand refs matches a `docker.io` row, for an unconfigured host returns `ErrRegistryNotConfigured` with the opt-in off and `Anonymous: true` with it on; sealing binding (copy `credential_enc` to another row → decrypt fails → `ErrRevisionCorrupt`-style error `ErrInvalid`); cross-org isolation. Add `registries` to the backup drill: put a registry with a credential before `Collect`, resolve it after restore with the recovered key.
- [ ] **Step 2: Run** → compile failures.
- [ ] **Step 3: Implement** migration 26:

```
SQLite:
CREATE TABLE registries (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 host TEXT NOT NULL,
 name TEXT NOT NULL,
 username TEXT NOT NULL DEFAULT '',
 credential_enc TEXT NOT NULL DEFAULT '',
 allow_private INTEGER NOT NULL DEFAULT 0 CHECK(allow_private IN (0,1)),
 created_by TEXT NOT NULL,
 created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL,
 UNIQUE(organization_id, host)
);
ALTER TABLE organizations ADD COLUMN anonymous_pull_enabled INTEGER NOT NULL DEFAULT 0 CHECK(anonymous_pull_enabled IN (0,1));
Postgres: same with BOOLEAN NOT NULL DEFAULT false and TIMESTAMPTZ.
```

Store functions per the interfaces; `PutRegistry` under `withTenantTargetDetails(RegistryManage, "registries/"+id, &details, ...)` with `lockOrganization`; `DeleteRegistry` audited likewise; `SetAnonymousPull` under `withTenantTargetDetails(RegistryManage, "registry-policy", ...)`; reads under `readTenant(RegistryRead)`. The seal helper mirrors `applicationValuesKey`.

- [ ] **Step 4: Run** both drivers; commit `feat: per-organization registries with sealed credentials and the anonymous-pull policy`.

---

### Task 2: `internal/registry` client

**Files:**
- Create/extend: `internal/registry/reference.go` (from Task 1), `internal/registry/client.go`, `internal/registry/egress.go`, `internal/registry/client_test.go`, `internal/registry/reference_test.go`, `internal/registry/online_test.go`, `internal/registry/AGENTS.md`
- Modify: `.github/workflows/ci.yml` (add `KY_TEST_REGISTRY_ONLINE: "1"` to the real-Docker step env and a line `go test -race ./internal/registry -run '^TestResolveDockerHubAlpine$' -count=1`)

**Interfaces (produces):** as the spec's `Options`, `Resolved`, `New`, `Resolve`, plus `Options.RootCAs *x509.CertPool` and `Options.DialAddr func(host string) (netip.Addr, error)` hooks used only by tests (default: real DNS + guard).

Implementation notes:
- `egress.go`: `checkAddr(a netip.Addr, allowPrivate bool) error` refusing loopback (always), unspecified, multicast, link-local unicast, private (`IsPrivate`), and CGNAT `100.64.0.0/10` unless allowed; `resolve(ctx, host)` picks the first address of the lookup that passes; the transport's `DialContext` dials that address with the original host for SNI/Host header.
- `client.go`: manifest GET with `Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json`; on 401 parse `WWW-Authenticate` (`Bearer realm="…",service="…",scope="…"` or `Basic`); realm must be HTTPS; token GET with basic auth when a credential exists; retry once with `Authorization: Bearer`; `Docker-Content-Digest` required, verified against `sha256(body)` when the body is non-empty; platforms parsed from an index's `manifests[].platform`.
- Tests: `httptest.NewTLSServer` fake registry with routes for `/v2/`, `/v2/<repo>/manifests/<ref>`, `/token`; cases per the spec list; the "credential never sent elsewhere" test records every `Authorization` header per path and host.
- `online_test.go`: gated by `KY_TEST_REGISTRY_ONLINE`, resolves `alpine:3.24` anonymously on `docker.io` and asserts a `sha256:` digest and a non-empty platform list.

- [ ] Steps: failing tests → implement → `go test -race ./internal/registry/ -count=1` → online test locally (`KY_TEST_REGISTRY_ONLINE=1`) → `AGENTS.md` → commit `feat: registry client resolves manifest digests with guarded egress`.

---

### Task 3: API routes

**Files:**
- Modify: `internal/api/server.go`, `internal/api/registry_handlers.go` (new), `internal/api/tenant_handlers.go` (`ErrRegistryNotConfigured` → 409 `registry_not_configured`), `internal/api/AGENTS.md`
- Test: `internal/api/registry_handlers_test.go`

Routes per the spec (list, put, delete, policy get/put) through `tenantRoute`; `PutRegistry` gets `s.config.Security.EncryptionKey`. Test: 401, 403 without CSRF, env admin 403 on writes and 200 on reads, org admin 200/204, the list body never contains the credential, policy round trip, cross-organization 403.

- [ ] Steps: failing test → implement → `go test -race ./internal/api/ -run 'TestRegistr' -count=2` → both drivers → commit `feat: registry and registry-policy routes`.

---

### Task 4: Web Registries panel

**Files:**
- Create: `web/src/components/Registries.tsx`, `web/src/components/Registries.test.tsx`
- Modify: `web/src/pages/Members.tsx` or the organization page that renders members (render `<Registries org={org} />` below the members panel; read `web/src/App.tsx` to find the organization route), `web/AGENTS.md`

Behaviour per the spec: list (host, name, username, credential set/none, private allowed), add/update form (credential input `type="password"`, never prefilled, cleared after submit; "Leave empty to keep the stored credential; use Clear to remove it"), delete with confirmation, policy switch with the explanation, fixed refusal texts (403 "Only an organization administrator can manage registries."), never render bodies.

- [ ] Steps: failing tests → implement → `npx vitest run` → `npm run build` → `make build-web` → commit `feat: organization registries panel`.

---

### Task 5: Docs, DOX, CI

`docs/application-schema.md` (Registry paragraph and decision rows → implemented for PR A), `docs/authorization-matrix.md` (`registry.read`, `registry.manage`, opt-in rows → implemented), `docs/threat-model.md` (registry credential row: implemented parts; the token-realm exception stated), `internal/registry/AGENTS.md` (Task 2), `internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md`, root `AGENTS.md` Child DOX Index (add `internal/registry/AGENTS.md`) and verification bullet (online registry test), `KyYard-Implementation-Plan.md` section 8 (M7a PR A implemented; next PR B). Then `make ci` and the PostgreSQL store/api suites. Commit `docs: registries contract`.
