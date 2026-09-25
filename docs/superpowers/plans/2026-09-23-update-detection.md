# Update Detection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Tell an operator, per mapped service of an adopted application, whether the registry holds a newer image than the host runs, on demand, cached, without deploying.

**Architecture:** Migration 27 adds `image_checks`. The store's `CheckImageUpdates` runs in three phases (read transaction → registry `Head` calls through a `DigestResolver` → audited write transaction) so no network call holds a database transaction. Two routes expose the cache and the check; an Updates panel renders it.

**Tech Stack:** Go (`database/sql`, SQLite + PostgreSQL), `internal/registry` client (`Head`), React 19 + TypeScript + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-23-update-detection-design.md`

## Global Constraints

- Verdicts are exactly `current`, `update_available`, `pinned`, `unknown_local`, `registry_error`; details exactly `not_configured`, `unauthorized`, `not_found`, `rate_limited`, `private_destination`, `unavailable`, else `''`.
- No network call inside a database transaction. At most 4 concurrent `Head` calls, 60 s overall deadline per check.
- Credentials exist only in phase 2 memory; never stored, returned, logged or put in an audit row or error string.
- `CheckImageUpdates` is gated by `application.deploy`; `ReadImageChecks` by `application.read`.
- Audit details on a successful check: `services=N updates=N errors=N`.
- The local digest is taken only when the mapped container's image has exactly one `RepoDigests` entry whose repository equals the reference's repository (`registry.CanonicalHost(host)` + `/` + repository, with `library/` for a bare Docker Hub name); otherwise `unknown_local`.
- `pinned` (reference carries `@sha256:`) and `registry_error/not_configured` make no registry request.
- Every new store behaviour is tested on SQLite and PostgreSQL; `image_checks` joins the SQLite recovery drill.
- Commit trailer on every commit: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. A mapping changed between phase 1 and phase 3 (another admin re-mapped or released): the write must refuse with `ErrAdoptionChanged` and store nothing (Task 2 test `TestCheckImageUpdatesRefusesChangedMapping`).
2. An inventory image whose `RepoDigests` lists the same repository twice (re-tagged pulls) must be `unknown_local`, not a guess (Task 2 table case).
3. A resolver that hangs must not hold the request past the 60 s deadline or leak a goroutine (Task 2 `TestCheckImageUpdatesDeadline` with a blocking fake and a short deadline override).
4. Two concurrent POSTs for the same application must not both run: the second gets 409 `check_in_progress` and the first completes normally (Task 3 test).
5. A `registry_error` row must show the fixed text for its detail and never any server string (Task 4 test per detail).

---

### Task 1: Migration 27, models, errors, `ReadImageChecks`, cache clearing

**Files:**
- Modify: `internal/store/migrations/migrations.go` (append after Version 26)
- Modify: `internal/store/store.go` (errors + `TenancyStore` interface)
- Create: `internal/store/image_checks.go`
- Modify: `internal/store/application_apply.go:307-345` (`settleApply`: delete the instance's `image_checks` rows on success)
- Modify: `internal/store/application_backup_test.go` (drill: an `image_checks` row survives restore)
- Test: `internal/store/image_checks_test.go`
- Modify: `internal/store/AGENTS.md`

**Interfaces:**
- Produces:
  ```go
  var ErrMappingRequired = errors.New("application has no adopted mapping")
  type ImageCheck struct {
      Service string `json:"service"`; Reference string `json:"reference"`
      LocalDigest string `json:"local_digest"`; RemoteDigest string `json:"remote_digest"`
      Verdict string `json:"verdict"`; Detail string `json:"detail"`; CheckedAt time.Time `json:"checked_at"`
  }
  type UpdateCheck struct { InstanceID string `json:"instance_id"`; MappingVersion int `json:"mapping_version"`; Services []ImageCheck `json:"services"` }
  // TenancyStore:
  ReadImageChecks(ctx context.Context, access TenantAccess, app string) (*UpdateCheck, error)
  ```
  `ReadImageChecks` returns `Services: []` (never nil) and, with no instance, `InstanceID: ""`, `MappingVersion: 0`.
  Internal helper `func (t *tenancyStore) clearImageChecks(ctx, tx, instance string) error` (`DELETE FROM image_checks WHERE instance_id=?`).

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/image_checks_test.go
func TestReadImageChecksEmptyWithoutInstance(t *testing.T) {
	st, a, _ := registryFixture(t) // reuse: org admin access with env scope; add env if the fixture lacks it
	app := createDraftApplication(t, st, a) // reuse the helper application tests use to create a draft with one service "web" image "ghcr.io/org/web:1.2"
	out, err := st.Tenancy().ReadImageChecks(context.Background(), a, app)
	if err != nil || out.InstanceID != "" || len(out.Services) != 0 || out.Services == nil {
		t.Fatalf("empty read: %+v %v", out, err)
	}
}

func TestImageChecksClearedBySuccessfulApply(t *testing.T) {
	// Use the adopted+mapped fixture from application_apply_test.go (the one that drives ApplyDeployment then SettleDeployment with OutcomeSucceeded).
	// Insert a row directly: INSERT INTO image_checks(instance_id,service_name,reference,verdict,checked_at) VALUES(instance,'web','ghcr.io/org/web:1.2','update_available',now)
	// Settle a succeeded apply, then assert SELECT COUNT(*) FROM image_checks WHERE instance_id=? == 0.
}

func TestImageChecksCascadeOnRelease(t *testing.T) {
	// Adopt, insert a row, ReleaseApplication, assert zero rows.
}
```
Read `internal/store/application_apply_test.go` and `application_adoption_test.go` for the exact fixture helpers (names differ; use what exists, do not invent).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestReadImageChecks|TestImageChecks' ./internal/store/`
Expected: FAIL (undefined `ReadImageChecks`, no table).

- [ ] **Step 3: Migration and model**

Append to `migrations.go`:
```go
	{Version: 27, Name: "image_checks", SQLite: `CREATE TABLE image_checks (
 instance_id TEXT NOT NULL REFERENCES application_instances(id) ON DELETE CASCADE,
 service_name TEXT NOT NULL,
 reference TEXT NOT NULL,
 local_digest TEXT NOT NULL DEFAULT '',
 remote_digest TEXT NOT NULL DEFAULT '',
 verdict TEXT NOT NULL CHECK(verdict IN ('current','update_available','pinned','unknown_local','registry_error')),
 detail TEXT NOT NULL DEFAULT '',
 checked_at DATETIME NOT NULL,
 PRIMARY KEY (instance_id, service_name)
);
`, Postgres: `CREATE TABLE image_checks (
 instance_id TEXT NOT NULL REFERENCES application_instances(id) ON DELETE CASCADE,
 service_name TEXT NOT NULL,
 reference TEXT NOT NULL,
 local_digest TEXT NOT NULL DEFAULT '',
 remote_digest TEXT NOT NULL DEFAULT '',
 verdict TEXT NOT NULL CHECK(verdict IN ('current','update_available','pinned','unknown_local','registry_error')),
 detail TEXT NOT NULL DEFAULT '',
 checked_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY (instance_id, service_name)
);
`},
```
Check `application_instances.id` is the primary key (it is; `applicationMapping` selects by it). If the upgrade-replay fixture (`TestTenancyUpgradeAndReopen`) lists migrations explicitly, add 27.

`image_checks.go`: the types above, `ReadImageChecks` under `readTenant(ApplicationRead)`: `SELECT id, mapping_version FROM application_instances WHERE organization_id=? AND environment_id=? AND application_id=?` (no rows → empty result), then `SELECT service_name,reference,local_digest,remote_digest,verdict,detail,checked_at FROM image_checks WHERE instance_id=? ORDER BY service_name`. `a.EnvironmentID == ""` or a bad UUID → `ErrInvalid`.

`settleApply`: after the loop, when `res.Outcome == protocol.OutcomeSucceeded`, call `t.clearImageChecks(ctx, tx, instance)`.

Backup drill: in `application_backup_test.go`, insert one `image_checks` row for the adopted instance before the backup and assert it is present after restore.

- [ ] **Step 4: Run the tests on both drivers**

Run: `go test -race -count=1 -run 'TestReadImageChecks|TestImageChecks|TestApplicationRevisionsSurviveBackup|TestTenancyUpgradeAndReopen' ./internal/store/` and again with `KY_TEST_POSTGRES_DSN` set.
Expected: PASS.

- [ ] **Step 5: DOX and commit**

`internal/store/AGENTS.md`: one bullet for migration 27 and the clearing rules. Commit: `feat(store): image_checks table, cached reads, cleared by apply and release`.

---

### Task 2: `CheckImageUpdates`

**Files:**
- Modify: `internal/store/image_checks.go`
- Modify: `internal/store/store.go` (interface)
- Test: `internal/store/image_checks_test.go`
- Modify: `internal/store/AGENTS.md`, `internal/registry/AGENTS.md` (the store consumes `Head` through `DigestResolver`)

**Interfaces:**
- Consumes: `registry.ParseReference`, `registry.CanonicalHost`, `registry.Credential`, `registry.ErrUnauthorized/ErrNotFound/ErrRateLimited/ErrPrivateDestination`; `t.applicationMapping`, `t.preflight` (or its query, for spec + snapshot), `t.registryFor`, `t.anonymousPull`, `freshInventory`.
- Produces:
  ```go
  // DigestResolver is what the check needs from a registry client. The API satisfies it with
  // internal/registry; tests use a fake.
  type DigestResolver interface {
      Head(ctx context.Context, ref registry.Reference, cred *registry.Credential, allowPrivate bool) (string, error) // digest
  }
  // TenancyStore:
  CheckImageUpdates(ctx context.Context, access TenantAccess, app string, resolver DigestResolver, key []byte, privateAllowed bool) (*UpdateCheck, error)
  ```
  Unexported, test-overridable: `var imageCheckDeadline = 60 * time.Second`, `const imageCheckConcurrency = 4`.

- [ ] **Step 1: Write the failing tests**

A fake resolver:
```go
type fakeResolver struct {
	mu    sync.Mutex
	calls []fakeCall // {Ref registry.Reference, Cred *registry.Credential, AllowPrivate bool}
	reply map[string]struct{ digest string; err error } // keyed by ref.Host+"/"+ref.Repository+":"+ref.Tag
	block chan struct{} // when non-nil, Head waits on it (deadline test)
}
```
Tests (each on both drivers via the existing testdb pattern):
- `TestCheckImageUpdatesVerdicts`: fixture with a mapped instance whose revision has services `web` (`ghcr.io/org/web:1.2`, host image RepoDigests `["ghcr.io/org/web@sha256:aaa…"]`), `db` (`postgres:16`, RepoDigests `["postgres@sha256:bbb…"]`), `pinned` (`ghcr.io/org/x@sha256:ccc…`), `built` (`local/thing:1`, RepoDigests `[]`), `dup` (`quay.io/a/b:1`, two RepoDigests for `quay.io/a/b`). Registry rows: `ghcr.io` with credential `secret`, anonymous pull off. Fake replies: web → different digest (`update_available`), postgres → `registry_error/not_configured` expected with no call (docker.io has no row and the opt-in is off), then turn the opt-in on and re-check: docker.io call has `Cred == nil` and reply equal to local → `current`. Assert: `pinned` and `built`/`dup` made no call; the ghcr call carried `Cred.Secret == "secret"`; rows persisted match; the audit row details equal `services=5 updates=1 errors=1` (first run) and never contain `secret`.
- `TestCheckImageUpdatesRegistryErrors`: table over `registry.ErrUnauthorized`, `ErrNotFound`, `ErrRateLimited`, `ErrPrivateDestination`, `errors.New("boom")` → details `unauthorized`, `not_found`, `rate_limited`, `private_destination`, `unavailable`; `remote_digest` empty; `Detail` never contains "boom".
- `TestCheckImageUpdatesRefusesChangedMapping`: fake resolver's `Head` bumps the mapping (call `SetApplicationMapping` with a new binding set, or release the instance) before returning; expect `ErrAdoptionChanged` and zero rows.
- `TestCheckImageUpdatesReplacesRows`: first check stores 2 rows; edit the revision to drop one service and re-map; second check leaves exactly the remaining service's row.
- `TestCheckImageUpdatesDeadline`: set `imageCheckDeadline = 200 * time.Millisecond` (restore with `t.Cleanup`), blocking fake; expect the call to return within 1 s with every unresolved service as `registry_error/unavailable`, and no goroutine left (use `runtime.NumGoroutine` before/after with a small tolerance or close the block channel in cleanup and check the resolver's call count).
- `TestCheckImageUpdatesPermissions`: developer allowed, operator and read_only `ErrForbidden` with a denied audit row of `application.deploy`; `ErrMappingRequired` for an adopted instance with `mapping_version == 0`; `ErrNotFound` for an unknown application.
- `TestCheckImageUpdatesPrivateFlag`: registry row `allow_private=true`; with `privateAllowed=false` the fake sees `AllowPrivate == false`, with `true` it sees `true`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestCheckImageUpdates' ./internal/store/`
Expected: FAIL (undefined `CheckImageUpdates`).

- [ ] **Step 3: Implement**

```go
type imageCheckWork struct {
	Service, Reference, LocalDigest string
	Ref          registry.Reference
	Cred         *registry.Credential
	AllowPrivate bool
	Row          ImageCheck // pre-filled for pinned / unknown_local / not_configured
	Skip         bool       // no registry call
}

func (t *tenancyStore) CheckImageUpdates(ctx context.Context, a TenantAccess, app string, resolver DigestResolver, key []byte, privateAllowed bool) (*UpdateCheck, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" || len(key) != 32 || resolver == nil {
		return nil, ErrInvalid
	}
	// Phase 1: read.
	var work []imageCheckWork
	var instance string
	var version int
	err = t.readTenant(ctx, a, permissions.ApplicationDeploy, func(tx *sql.Tx) error {
		m, err := t.applicationMapping(ctx, tx, a, id.String(), false)
		if err != nil { return err }
		if m.Version < 1 { return ErrMappingRequired }
		_, m, spec, snapshot, _, err := t.preflight(ctx, tx, a, id.String(), false, 0)
		if err != nil { return err }
		instance, version = m.InstanceID, m.Version
		images := map[string]protocol.Image{}
		for _, im := range snapshot.Images { images[im.ID] = im }
		containers := map[string]protocol.Container{}
		for _, c := range snapshot.Containers { containers[c.ID] = c }
		anonymous, err := t.anonymousPull(ctx, tx, a.OrganizationID)
		if err != nil { return err }
		for _, s := range spec.Services {
			w := imageCheckWork{Service: s.Name, Reference: s.Image, Row: ImageCheck{Service: s.Name, Reference: s.Image}}
			w.Ref, err = registry.ParseReference(s.Image)
			switch {
			case err != nil:
				w.Skip, w.Row.Verdict, w.Row.Detail = true, "registry_error", "unavailable"
			case w.Ref.Digest != "":
				w.Skip, w.Row.Verdict = true, "pinned"
			default:
				w.LocalDigest = localRepoDigest(images[containers[m.Bindings[s.Name]].ImageID], w.Ref)
				if w.LocalDigest == "" {
					w.Skip, w.Row.Verdict = true, "unknown_local"
					break
				}
				r, cred, err := t.registryFor(ctx, tx, a.OrganizationID, w.Ref.Host, key)
				switch {
				case errors.Is(err, ErrNotFound) && !anonymous:
					w.Skip, w.Row.Verdict, w.Row.Detail = true, "registry_error", "not_configured"
				case errors.Is(err, ErrNotFound):
				case err != nil:
					return err
				default:
					w.Cred, w.AllowPrivate = cred, r.AllowPrivate && privateAllowed
				}
			}
			w.Row.LocalDigest = w.LocalDigest
			work = append(work, w)
		}
		return nil
	})
	if err != nil { return nil, err }
	// Phase 2: resolve, outside any transaction. Credentials die with this scope.
	resolveImageChecks(ctx, resolver, work)
	// Phase 3: write.
	now := time.Now().UTC().Truncate(time.Microsecond)
	updates, failures := 0, 0
	for i := range work {
		work[i].Row.CheckedAt = now
		switch work[i].Row.Verdict { case "update_available": updates++; case "registry_error": failures++ }
	}
	details := fmt.Sprintf("services=%d updates=%d errors=%d", len(work), updates, failures)
	out := &UpdateCheck{InstanceID: instance, MappingVersion: version, Services: []ImageCheck{}}
	err = t.withTenantTargetDetails(ctx, a, permissions.ApplicationDeploy, id.String()+"/updates", &details, func(tx *sql.Tx) error {
		var v int
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT mapping_version FROM application_instances WHERE id=? AND organization_id=? AND environment_id=? AND application_id=?`), instance, a.OrganizationID, a.EnvironmentID, id.String()).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && v != version) { return ErrAdoptionChanged }
		if err != nil { return err }
		if err := t.clearImageChecks(ctx, tx, instance); err != nil { return err }
		for _, w := range work {
			if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO image_checks(instance_id,service_name,reference,local_digest,remote_digest,verdict,detail,checked_at) VALUES(?,?,?,?,?,?,?,?)`), instance, w.Service, w.Reference, w.Row.LocalDigest, w.Row.RemoteDigest, w.Row.Verdict, w.Row.Detail, now); err != nil { return err }
			out.Services = append(out.Services, w.Row)
		}
		return nil
	})
	if err != nil { return nil, err }
	return out, nil
}

// localRepoDigest returns the host image's digest for exactly the reference's repository, or "".
func localRepoDigest(im protocol.Image, ref registry.Reference) string {
	want := ref.Host + "/" + ref.Repository
	found := ""
	for _, d := range im.Digests {
		name, digest, ok := strings.Cut(d, "@")
		if !ok { continue }
		parsed, err := registry.ParseReference(name)
		if err != nil || parsed.Host+"/"+parsed.Repository != want { continue }
		if found != "" { return "" } // two entries for one repository: ambiguous
		found = digest
	}
	return found
}

func resolveImageChecks(ctx context.Context, resolver DigestResolver, work []imageCheckWork) {
	ctx, cancel := context.WithTimeout(ctx, imageCheckDeadline)
	defer cancel()
	sem := make(chan struct{}, imageCheckConcurrency)
	var wg sync.WaitGroup
	for i := range work {
		if work[i].Skip { continue }
		wg.Add(1)
		go func(w *imageCheckWork) {
			defer wg.Done()
			select { case sem <- struct{}{}: case <-ctx.Done(): w.Row.Verdict, w.Row.Detail = "registry_error", "unavailable"; return }
			defer func() { <-sem }()
			digest, err := resolver.Head(ctx, w.Ref, w.Cred, w.AllowPrivate)
			w.Cred = nil
			switch {
			case err == nil && digest == w.LocalDigest: w.Row.Verdict, w.Row.RemoteDigest = "current", digest
			case err == nil: w.Row.Verdict, w.Row.RemoteDigest = "update_available", digest
			default: w.Row.Verdict, w.Row.Detail = "registry_error", registryErrorDetail(err)
			}
		}(&work[i])
	}
	wg.Wait()
}

func registryErrorDetail(err error) string {
	switch {
	case errors.Is(err, registry.ErrUnauthorized): return "unauthorized"
	case errors.Is(err, registry.ErrNotFound): return "not_found"
	case errors.Is(err, registry.ErrRateLimited): return "rate_limited"
	case errors.Is(err, registry.ErrPrivateDestination): return "private_destination"
	}
	return "unavailable"
}
```
Note the deadline: `resolver.Head` must honour `ctx`; the fake in the deadline test must select on `ctx.Done()`. A `Head` that ignores ctx would hang `wg.Wait()`; document in `DigestResolver` that implementations must return when ctx ends. `preflight` already validates the mapping against the live snapshot (returns `ErrAdoptionChanged` when containers moved); the `m.Version < 1` check must come first so an unmapped instance reports `ErrMappingRequired`, not a preflight error.

- [ ] **Step 4: Run tests on both drivers**

Run: `go test -race -count=1 -run 'TestCheckImageUpdates|TestReadImageChecks|TestImageChecks' ./internal/store/` and with `KY_TEST_POSTGRES_DSN`.
Expected: PASS.

- [ ] **Step 5: DOX and commit**

`internal/store/AGENTS.md`: the three phases, verdict rules, audit details, `DigestResolver` contract. `internal/registry/AGENTS.md`: one line that the store calls `Head` through `store.DigestResolver`, one client per host setting. Commit: `feat(store): check image updates per mapped service through the registry client`.

---

### Task 3: API routes

**Files:**
- Create: `internal/api/image_check_handlers.go`
- Modify: `internal/api/server.go` (two routes after line 216; a `checks sync.Map` or `map[string]*struct{}` + mutex on `Server` for in-progress applications)
- Modify: `internal/api/tenant_handlers.go` (`ErrMappingRequired` → 409 `mapping_required`; `ErrCheckInProgress` → 409 `check_in_progress`, an API-level error `var errCheckInProgress`)
- Modify: `internal/api/tenant_error_internal_test.go` (rows for both codes)
- Test: `internal/api/image_check_handlers_test.go`
- Modify: `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: `store.CheckImageUpdates`, `store.ReadImageChecks`, `registry.New(registry.Options{AllowPrivate: bool}).Head(ctx, ref, cred) (registry.Resolved, error)`, `s.config.Security.EncryptionKey`, `s.config.Registry.AllowPrivate`.
- Produces: `GET .../applications/{application}/updates` → 200 `UpdateCheck` JSON; `POST .../applications/{application}/updates/check` → 200 `UpdateCheck` JSON.

- [ ] **Step 1: Write the failing tests**

Reuse the API application fixture that adopts and maps an instance (see `application_handlers_test.go` / the plan-route tests) and inject a fake `DigestResolver` through a server field `s.digestResolver store.DigestResolver` (nil → the real client adapter). Cases: 401; GET as read_only member 200 with `services: []`; POST without CSRF 403; POST as operator 403 with fixed text; POST as developer 200 with one `update_available` row and the GET then returns it; POST for an adopted but unmapped app 409 `mapping_required`; concurrent POSTs (fake resolver blocks on a channel; second POST gets 409 `check_in_progress`; release the channel; first returns 200); response bodies never contain the credential string; cross-organization 403.

- [ ] **Step 2: Run to verify failure**

Run: `go test -count=1 -run 'TestImageCheckRoutes' ./internal/api/` → FAIL (404 routes).

- [ ] **Step 3: Implement**

```go
// registryResolver adapts internal/registry to store.DigestResolver: one client per call so a
// private-address allowance never leaks between hosts.
type registryResolver struct{}

func (registryResolver) Head(ctx context.Context, ref registry.Reference, cred *registry.Credential, allowPrivate bool) (string, error) {
	r, err := registry.New(registry.Options{AllowPrivate: allowPrivate}).Head(ctx, ref, cred)
	if err != nil { return "", err }
	return r.Digest, nil
}

func (s *Server) handleImageChecks(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	out, err := s.store.Tenancy().ReadImageChecks(r.Context(), a, r.PathValue("application"))
	if err != nil { s.tenantError(w, err); return }
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCheckImageUpdates(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	key := a.OrganizationID + "/" + a.EnvironmentID + "/" + r.PathValue("application")
	if _, busy := s.imageChecks.LoadOrStore(key, struct{}{}); busy {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "An update check for this application is already running", "code": "check_in_progress"})
		return
	}
	defer s.imageChecks.Delete(key)
	resolver := s.digestResolver
	if resolver == nil { resolver = registryResolver{} }
	out, err := s.store.Tenancy().CheckImageUpdates(r.Context(), a, r.PathValue("application"), resolver, s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate)
	if err != nil { s.tenantError(w, err); return }
	s.writeJSON(w, http.StatusOK, out)
}
```
`s.imageChecks` is a `sync.Map`. `tenantError`: `store.ErrMappingRequired` → 409 `{"error": "Map the application's services to adopted containers first", "code": "mapping_required"}`.

- [ ] **Step 4: Run tests on both drivers**

Run: `go test -race -count=1 -run 'TestImageCheckRoutes|TestTenantErrorCodes' ./internal/api/` and with `KY_TEST_POSTGRES_DSN`. Expected: PASS.

- [ ] **Step 5: DOX and commit**

`internal/api/AGENTS.md`: the two routes, permissions, 409 codes, the per-application in-memory mutex. Commit: `feat(api): image update check routes`.

---

### Task 4: Updates panel

**Files:**
- Create: `web/src/components/ApplicationUpdates.tsx`, `web/src/components/ApplicationUpdates.test.tsx`
- Modify: `web/src/components/Applications.tsx:84` (mount `<ApplicationUpdates base={…} instanceID={i.id} />` after `<ApplicationPreflight …/>`)
- Modify: `web/src/tenant.ts` (types `ImageCheck`, `UpdateCheck`)
- Modify: `web/AGENTS.md`

**Interfaces:**
- Consumes: `useTenantResource<UpdateCheck>(`${base}/updates`)`, `tenantWrite(`${base}/updates/check`, 'POST', undefined, { forbidden: 'Only administrators and developers can check for updates.' })`.

- [ ] **Step 1: Write the failing tests**

Vitest with the fetch mock pattern of `ApplicationDeploymentPlan.test.tsx`: renders nothing (or "No update check yet") with `services: []` and a Check button; a `current` row shows "Up to date"; `update_available` shows "Update available" with both short digests (first 12 hex chars); `pinned` → "Pinned"; `unknown_local` → "Unknown on host"; each `registry_error` detail shows its fixed text and never the raw detail token; clicking Check POSTs to `/updates/check` with the CSRF header and re-renders the returned rows; 403 shows the fixed forbidden text; 409 `check_in_progress` shows "A check is already running."; 409 `mapping_required` shows "Map the services first."

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npx vitest run src/components/ApplicationUpdates.test.tsx` → FAIL (module missing).

- [ ] **Step 3: Implement**

Follow `ApplicationDeploymentPlan.tsx` for markup (`panel-header`, `StateNotice`, table). Verdict labels and error texts as constants:
```ts
const verdictText: Record<string, string> = { current: 'Up to date', update_available: 'Update available', pinned: 'Pinned', unknown_local: 'Unknown on host', registry_error: 'Registry error' };
const detailText: Record<string, string> = {
  not_configured: 'No registry entry for this host and anonymous pulls are off.',
  unauthorized: 'The registry refused the credentials.',
  not_found: 'The image was not found in the registry.',
  rate_limited: 'The registry rate limit was reached; try later.',
  private_destination: 'The registry is on a private address this organization may not reach.',
  unavailable: 'The registry could not be reached.',
};
const short = (d: string) => d.replace(/^sha256:/, '').slice(0, 12);
```
Map 409 codes to fixed texts by reading the `code` the way the existing tenant helpers do (check `tenantWrite`; if it returns only text, extend `texts` with `conflict?: Record<string,string>` minimally).

- [ ] **Step 4: Run the suite, build, dist**

Run: `cd web && npx vitest run && npm run build`, then `make build-web` from the worktree root; `git status --short web/dist` must show the rebuilt assets and nothing else.

- [ ] **Step 5: DOX and commit**

`web/AGENTS.md`: the panel and its routes. Commit: `feat(web): application updates panel`.

---

### Task 5: Docs, DOX, CI

**Files:**
- Modify: `docs/application-schema.md` (Update detection: implemented, table, verdicts; PR C remains), `docs/threat-model.md` (check uses the credential under `application.deploy`; digests only are stored), `docs/authorization-matrix.md` (a line under `application.deploy`: also checks image updates), `KyYard-Implementation-Plan.md` §8 (PR B done; PR C next), `internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md`, `internal/registry/AGENTS.md` (verify all accurate after Tasks 1–4).

- [ ] **Step 1: Update the documents** as listed; remove any sentence that says update detection is planned.
- [ ] **Step 2: Run `make ci`** from the worktree root (foreground) and the store + api suites with `KY_TEST_POSTGRES_DSN`. Expected: exit 0.
- [ ] **Step 3: Commit** `docs: update detection contract`.
