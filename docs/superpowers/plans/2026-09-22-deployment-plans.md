# Deployment Plans Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist an executable deployment preview ("plan") for an adopted application instance, bound to every identity a later apply needs, expiring after ten minutes, one per instance, without sending any agent command.

**Architecture:** A new `deployments` table (migration 23) holds one `planned` row per instance. `PlanDeployment` reuses the preflight computation inside a locked tenant transaction, refuses anything that is not a clean preflight, and stores a JSON plan. Three tenant API routes expose plan, list and read. A small React panel mints and shows the plan. Release refuses while a live plan exists.

**Tech Stack:** Go 1.25 (`internal/store`, `internal/api`, `internal/permissions`), SQLite/PostgreSQL via inline migrations, React 19 + Vitest (`web/`).

**Spec:** `docs/superpowers/specs/2026-09-22-deployment-plans-design.md`

## Global Constraints

- Worktree: `/home/yoshi/busness.app/KyYard-Server/.worktrees/deployment-plan`, branch `feat/deployment-plan`. Never edit the root checkout.
- Every store change is tested on SQLite (`go test ./internal/store/`) and PostgreSQL (`KY_TEST_POSTGRES_DSN=postgres://postgres:<pw>@127.0.0.1:15440/kyyard?sslmode=disable go test -count=1 ./internal/store/ ./internal/api/`; the password is in `docker inspect kyyard-access-pg`; never print it).
- `gofmt -w` every edited Go file before `make ci`.
- No agent frame, no image pull, no container mutation, no secret value read in this slice.
- Plan JSON at most 64 KiB (`MaxApplicationValuesBytes` is the same size; reuse the constant name `MaxDeploymentPlanBytes = 64 * 1024`).
- Preview validity: 10 minutes (`DeploymentPlanTTL = 10 * time.Minute`).
- Permission `application.deploy`: organization_admin, environment_admin, developer.
- Commit messages end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. A plan minted, then the definition re-saved (head advances): GET must report the plan unchanged but a POST with the old revision must refuse with `adoption_changed`; nothing marks the old plan stale on read. Pinned to Task 2 ("stale revision refuses").
2. Two administrators plan the same instance concurrently: exactly one row survives and it is the later transaction's; no unique-constraint error surfaces as 500. Pinned to Task 2 ("replacement") plus the `UNIQUE(instance_id)` delete-then-insert inside one locked transaction.
3. Release with an expired plan present: must succeed and delete the row; release with a live plan: 409 `deployment_planned`. Pinned to Task 3.
4. A developer can plan but cannot map or adopt; an operator can neither. Pinned to Task 2 (roles) and Task 4 (403s).
5. The UI receives a 409 body containing a secret canary or an unknown blocker name: it must render fixed text only. Pinned to Task 5.

---

### Task 1: Permission, errors, migration

**Files:**
- Modify: `internal/permissions/permissions.go`
- Modify: `internal/store/store.go` (error list)
- Modify: `internal/store/migrations/migrations.go` (append version 23)
- Modify: `internal/store/tenancy_test.go:211` (add `DROP TABLE deployments` first and `23` to the version list)
- Test: `internal/permissions/permissions_test.go` (create if absent), `internal/store/deployment_test.go` (create)

**Interfaces:**
- Produces: `permissions.ApplicationDeploy Action = "application.deploy"`; `store.ErrDeploymentPlanned`; `store.PreflightBlockedError{Blockers []string}` implementing `error` and matched by `errors.As`; table `deployments`.

- [ ] **Step 1: Write the failing permission test**

```go
// internal/permissions/permissions_test.go
package permissions

import "testing"

func TestApplicationDeployFollowsTheMatrix(t *testing.T) {
	for role, want := range map[string]bool{"organization_admin": true, "environment_admin": true, "developer": true, "operator": false, "read_only": false, "": false} {
		if got := Allows(role, ApplicationDeploy); got != want {
			t.Fatalf("%q application.deploy = %v, want %v", role, got, want)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/permissions/ -run TestApplicationDeployFollowsTheMatrix`
Expected: compile error, `undefined: ApplicationDeploy`.

- [ ] **Step 3: Add the permission**

In `internal/permissions/permissions.go` add to the const block, after `ApplicationDestroy`:

```go
	// ApplicationDeploy mints and, in a later slice, applies a deployment plan. The matrix gives
	// it to developers because a plan is desired state made concrete, not host authority.
	ApplicationDeploy Action = "application.deploy"
```

Add `ApplicationDeploy` to the `organization_admin`, `environment_admin` and `developer` case lists in `Allows`.

- [ ] **Step 4: Run the permission test**

Run: `go test ./internal/permissions/`
Expected: PASS.

- [ ] **Step 5: Add the errors**

In `internal/store/store.go`, in the `var (...)` error block add:

```go
	ErrDeploymentPlanned = errors.New("a live deployment plan exists")
```

Create `internal/store/application_deployment.go` with the error type (the rest of the file is filled in Task 2):

```go
package store

import "strings"

// PreflightBlockedError names the findings that stopped a plan. A plan never guesses past them.
type PreflightBlockedError struct{ Blockers []string }

func (e *PreflightBlockedError) Error() string {
	return "deployment preflight blocked: " + strings.Join(e.Blockers, ",")
}
```

- [ ] **Step 6: Write the failing migration test**

```go
// internal/store/deployment_test.go
package store

import (
	"context"
	"testing"
)

func TestDeploymentsTableExists(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	var n int
	if err := st.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deployments table: %d %v", n, err)
	}
}
```

- [ ] **Step 7: Run it to verify it fails**

Run: `go test ./internal/store/ -run TestDeploymentsTableExists`
Expected: FAIL, `no such table: deployments`.

- [ ] **Step 8: Append migration 23**

In `internal/store/migrations/migrations.go`, after the version 22 entry:

```go
	{Version: 23, Name: "deployment_plans", SQLite: `CREATE TABLE deployments (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 application_id TEXT NOT NULL,
 instance_id TEXT NOT NULL UNIQUE,
 endpoint_id TEXT NOT NULL,
 project TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('planned')),
 revision INTEGER NOT NULL CHECK(revision BETWEEN 1 AND 100),
 spec_digest TEXT NOT NULL,
 mapping_version INTEGER NOT NULL CHECK(mapping_version BETWEEN 1 AND 1000000000),
 plan TEXT NOT NULL CHECK(length(plan)<=65536),
 created_by TEXT NOT NULL,
 created_at DATETIME NOT NULL,
 expires_at DATETIME NOT NULL,
 FOREIGN KEY(organization_id,environment_id,application_id) REFERENCES applications(organization_id,environment_id,id) ON DELETE CASCADE
);
CREATE INDEX idx_deployments_application ON deployments(application_id,created_at);
`, Postgres: `CREATE TABLE deployments (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 application_id TEXT NOT NULL,
 instance_id TEXT NOT NULL UNIQUE,
 endpoint_id TEXT NOT NULL,
 project TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('planned')),
 revision INTEGER NOT NULL CHECK(revision BETWEEN 1 AND 100),
 spec_digest TEXT NOT NULL,
 mapping_version INTEGER NOT NULL CHECK(mapping_version BETWEEN 1 AND 1000000000),
 plan TEXT NOT NULL CHECK(length(plan)<=65536),
 created_by TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 FOREIGN KEY(organization_id,environment_id,application_id) REFERENCES applications(organization_id,environment_id,id) ON DELETE CASCADE
);
CREATE INDEX idx_deployments_application ON deployments(application_id,created_at);
`},
```

In `internal/store/tenancy_test.go:211` prepend `"DROP TABLE deployments"` to the list and add `23` to the `DELETE FROM schema_migrations WHERE version IN (...)` list.

- [ ] **Step 9: Run store tests on both drivers**

Run: `go test ./internal/store/ -run 'TestDeploymentsTableExists|TestTenancy' -count=1` and the same with `KY_TEST_POSTGRES_DSN` set.
Expected: PASS on both.

- [ ] **Step 10: Commit**

```bash
gofmt -l internal/ && git add internal/permissions internal/store && git commit -m "feat: application.deploy permission and deployments table

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Store: PlanDeployment, ReadDeployment, ListDeployments

**Files:**
- Modify: `internal/store/application_preflight.go:41-100` (extract `preflight`)
- Modify: `internal/store/application_deployment.go`
- Modify: `internal/store/store.go` (Tenancy interface: find `PreflightApplication(` and add the three methods beside it)
- Test: `internal/store/deployment_test.go`

**Interfaces:**
- Consumes: `applicationMapping(ctx, tx, a, app, lock)`, `buildDeploymentPreflight`, `withTenantTarget`, `readTenant`, `permissions.ApplicationDeploy`, `PreflightBlockedError`.
- Produces:

```go
const DeploymentPlanTTL = 10 * time.Minute
const MaxDeploymentPlanBytes = 64 * 1024

type PlanRequest struct {
	InstanceID     string `json:"instance_id"`
	MappingVersion int    `json:"mapping_version"`
	Revision       int    `json:"revision"`
	Confirm        string `json:"confirm"`
}
type PlannedService struct {
	Name         string             `json:"name"`
	Reference    string             `json:"reference"`
	ImageID      string             `json:"image_id"`
	ImageDigest  string             `json:"image_digest"`
	ContainerID  string             `json:"container_id"`
	Replaces     protocol.InspectionTarget `json:"replaces"`
	Restart      string             `json:"restart"`
	Ports        []ApplicationPort  `json:"ports"`
	SecretRefs   []string           `json:"secret_refs"`
}
type DeploymentPlan struct {
	Project  string           `json:"project"`
	Services []PlannedService `json:"services"`
}
type Deployment struct {
	ID             string         `json:"id"`
	ApplicationID  string         `json:"application_id"`
	InstanceID     string         `json:"instance_id"`
	EndpointID     string         `json:"endpoint_id"`
	State          string         `json:"state"`
	Revision       int            `json:"revision"`
	SpecDigest     string         `json:"spec_digest"`
	MappingVersion int            `json:"mapping_version"`
	Plan           DeploymentPlan `json:"plan"`
	CreatedBy      string         `json:"created_by"`
	CreatedAt      time.Time      `json:"created_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	Expired        bool           `json:"expired"`
}
// Tenancy interface additions:
PlanDeployment(ctx context.Context, a TenantAccess, app string, r PlanRequest) (*Deployment, error)
ReadDeployment(ctx context.Context, a TenantAccess, app, id string) (*Deployment, error)
ListDeployments(ctx context.Context, a TenantAccess, app string) ([]Deployment, error)
```

- [ ] **Step 1: Write the failing store tests**

Append to `internal/store/deployment_test.go` (add imports `errors`, `slices`, `strings`, `time`, `protocol`):

```go
func planFixture(t *testing.T) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot, *ApplicationMapping) {
	t.Helper()
	st, a, app, endpoint, snapshot, m := mappingFixture(t)
	snapshot.Images = []protocol.Image{{ID: "sha256:" + strings.Repeat("d", 64), Tags: []string{"nginx:1"}, Digests: []string{"nginx@sha256:" + strings.Repeat("e", 64)}}}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if err := st.Tenancy().SetApplicationMapping(context.Background(), a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, err := st.Tenancy().ReadApplicationMapping(context.Background(), a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, a, app, endpoint, snapshot, m
}
func planRequest(m *ApplicationMapping) PlanRequest {
	return PlanRequest{InstanceID: m.InstanceID, MappingVersion: m.Version, Revision: m.Preview.Revision, Confirm: m.Preview.Project}
}

func TestPlanDeploymentBindsIdentities(t *testing.T) {
	st, a, app, _, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	c := snapshot.Containers[0]
	if d.State != "planned" || d.InstanceID != m.InstanceID || d.Revision != 1 || d.MappingVersion != 1 || d.Expired || time.Until(d.ExpiresAt) > DeploymentPlanTTL || time.Until(d.ExpiresAt) < 9*time.Minute {
		t.Fatalf("plan: %+v", d)
	}
	s := d.Plan.Services[0]
	if d.Plan.Project != "shop" || s.Name != "web" || s.ImageID != snapshot.Images[0].ID || s.ImageDigest != snapshot.Images[0].Digests[0] || s.ContainerID != c.ID || s.Replaces.ImageID != c.ImageID || s.Replaces.CreatedUnix != c.CreatedAt.Unix() {
		t.Fatalf("service: %+v", s)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.ID != d.ID || got.SpecDigest != d.SpecDigest {
		t.Fatalf("read: %+v %v", got, err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 1 || list[0].ID != d.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	var commands int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM endpoint_commands`).Scan(&commands); err != nil || commands != 0 {
		t.Fatal("plan dispatched a command")
	}
	foreign := a
	foreign.EnvironmentID = "foreign"
	if _, err = ts.ReadDeployment(ctx, foreign, app.ID, d.ID); err == nil {
		t.Fatal("foreign read")
	}
	if _, err = ts.PlanDeployment(ctx, foreign, app.ID, planRequest(m)); err == nil {
		t.Fatal("foreign plan")
	}
}

func TestPlanDeploymentRefusesStaleAndBlocked(t *testing.T) {
	st, a, app, endpoint, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for name, r := range map[string]PlanRequest{
		"instance": {InstanceID: "other", MappingVersion: 1, Revision: 1, Confirm: "shop"},
		"mapping":  {InstanceID: m.InstanceID, MappingVersion: 2, Revision: 1, Confirm: "shop"},
		"revision": {InstanceID: m.InstanceID, MappingVersion: 1, Revision: 2, Confirm: "shop"},
		"project":  {InstanceID: m.InstanceID, MappingVersion: 1, Revision: 1, Confirm: "nope"},
	} {
		if _, err := ts.PlanDeployment(ctx, a, app.ID, r); !errors.Is(err, ErrAdoptionChanged) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	// Definition advances: the old approval no longer names the head.
	if _, err := ts.AppendApplicationRevision(ctx, a, app.ID, 1, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m)); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("stale revision accepted")
	}
	r := planRequest(m)
	r.Revision = 2
	var blocked *PreflightBlockedError
	if _, err := ts.PlanDeployment(ctx, a, app.ID, r); !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, "mapping_requires_review") {
		t.Fatalf("mapping stale not reported: %v", err)
	}
	// Re-map, then remove the image from inventory: a service blocker refuses too.
	m2, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m2)); err != nil {
		t.Fatal(err)
	}
	snapshot.Images = nil
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	m2, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m2)); !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, "image_not_reported") {
		t.Fatalf("service blocker not reported: %v", err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 0 {
		t.Fatal("refused plan left a row")
	}
}

func TestPlanDeploymentReplacesAndExpires(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	first, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
	if err != nil || second.ID == first.ID {
		t.Fatalf("replacement: %+v %v", second, err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 1 || list[0].ID != second.ID {
		t.Fatalf("one row per instance: %+v %v", list, err)
	}
	if _, err = ts.ReadDeployment(ctx, a, app.ID, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("replaced plan still readable")
	}
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=?`), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, second.ID)
	if err != nil || !got.Expired {
		t.Fatalf("expiry: %+v %v", got, err)
	}
}

func TestPlanDeploymentRoles(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for role, ok := range map[TenantRole]bool{RoleDeveloper: true, RoleEnvironmentAdmin: true, RoleOperator: false, RoleReadOnly: false} {
		if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		_, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
		if ok && err != nil || !ok && !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s: %v", role, err)
		}
		if _, err = ts.ListDeployments(ctx, a, app.ID); err != nil {
			t.Fatalf("%s list: %v", role, err)
		}
	}
}
```

Check the exact role constant names with `grep -n "Role[A-Za-z]* *TenantRole" internal/store/models.go` and adjust.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ -run 'TestPlanDeployment' -count=1`
Expected: compile error, `PlanDeployment` undefined.

- [ ] **Step 3: Extract the preflight transaction body**

In `internal/store/application_preflight.go` replace the body of `PreflightApplication` so it becomes:

```go
func (t *tenancyStore) PreflightApplication(ctx context.Context, a TenantAccess, app string) (*DeploymentPreflight, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	var out *DeploymentPreflight
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		out, _, _, err = t.preflight(ctx, tx, a, id.String(), false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// preflight is the shared diagnostic. lock=true takes the mapping locks for a writer.
// preflight is the shared diagnostic. lock=true takes the mapping locks for a writer. It also
// returns the mapping, the parsed spec, the parsed snapshot and the revision digest so a writer
// can build a plan from exactly what it checked.
func (t *tenancyStore) preflight(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, lock bool) (*DeploymentPreflight, *ApplicationMapping, ApplicationSpec, protocol.Snapshot, string, error) {
	m, err := t.applicationMapping(ctx, tx, a, app, lock)
	if err != nil {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", err
	}
	... // the existing body from `var raw, specRaw, digest, instance, state string` through the
	    // identity loop, unchanged, returning errors as (nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", err)
	out := buildDeploymentPreflight(m, spec, snapshot)
	out.ReceivedAt = received
	return out, m, spec, snapshot, digest, nil
}
```

In `PreflightApplication` the call becomes `out, _, _, _, _, err = t.preflight(ctx, tx, a, id.String(), false)`. The public JSON is unchanged.

- [ ] **Step 4: Run the existing preflight tests**

Run: `go test ./internal/store/ -run 'Preflight' -count=1`
Expected: PASS (refactor only).

- [ ] **Step 5: Implement the deployment store**

Fill `internal/store/application_deployment.go`:

```go
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

const DeploymentPlanTTL = 10 * time.Minute
const MaxDeploymentPlanBytes = 64 * 1024

// (types from Interfaces above, verbatim)

// PreflightBlockedError ... (from Task 1)

// PlanDeployment persists an executable preview. It is minted only from a clean preflight and
// records every identity apply must recheck. It sends no command and reads no secret value.
// See docs/application-schema.md, Deployment plans.
func (t *tenancyStore) PlanDeployment(ctx context.Context, a TenantAccess, app string, r PlanRequest) (*Deployment, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	var out *Deployment
	err = t.withTenantTarget(ctx, a, permissions.ApplicationDeploy, id.String()+"/deployments", func(tx *sql.Tx) error {
		p, m, spec, snapshot, digest, err := t.preflight(ctx, tx, a, id.String(), true)
		if err != nil {
			return err
		}
		if r.InstanceID != m.InstanceID || r.MappingVersion != m.Version || r.Revision != p.Revision || r.Confirm != m.Preview.Project {
			return ErrAdoptionChanged
		}
		blockers := []string{}
		for _, b := range p.Blockers {
			if b != "runtime_verification_required" {
				blockers = append(blockers, b)
			}
		}
		plan := DeploymentPlan{Project: m.Preview.Project, Services: []PlannedService{}}
		// The preflight resolved each reference to one full image ID, which is the pin. Record
		// the repository digest beside it only when inventory reported exactly one; it is
		// advisory, for a later registry pull.
		digests := map[string]string{}
		for _, im := range snapshot.Images {
			if len(im.Digests) == 1 {
				digests[im.ID] = im.Digests[0]
			}
		}
		for i, s := range spec.Services {
			row := p.Services[i]
			for _, b := range row.Blockers {
				blockers = append(blockers, b)
			}
			refs := make([]string, 0, len(s.Environment))
			for _, ref := range s.Environment {
				refs = append(refs, ref.SecretRef)
			}
			slices.Sort(refs)
			ps := PlannedService{Name: s.Name, Reference: s.Image, ImageID: row.ImageID, ImageDigest: digests[row.ImageID], ContainerID: row.ContainerID, Restart: s.Restart, Ports: s.Ports, SecretRefs: refs}
			if ps.Ports == nil {
				ps.Ports = []ApplicationPort{}
			}
			if row.InspectionTarget != nil {
				ps.Replaces = *row.InspectionTarget
			}
			plan.Services = append(plan.Services, ps)
		}
		if len(blockers) > 0 {
			slices.Sort(blockers)
			return &PreflightBlockedError{Blockers: slices.Compact(blockers)}
		}
		raw, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		if len(raw) > MaxDeploymentPlanBytes {
			return ErrInvalid
		}
		now := time.Now().UTC()
		out = &Deployment{ID: uuid.NewString(), ApplicationID: id.String(), InstanceID: m.InstanceID, EndpointID: m.Preview.EndpointID, State: "planned", Revision: p.Revision, SpecDigest: digest, MappingVersion: m.Version, Plan: plan, CreatedBy: a.ActorID, CreatedAt: now, ExpiresAt: now.Add(DeploymentPlanTTL)}
		if _, err = tx.ExecContext(ctx, t.store.rebind(`DELETE FROM deployments WHERE instance_id=?`), m.InstanceID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), out.ID, a.OrganizationID, a.EnvironmentID, out.ApplicationID, out.InstanceID, out.EndpointID, plan.Project, out.State, out.Revision, out.SpecDigest, out.MappingVersion, string(raw), out.CreatedBy, out.CreatedAt, out.ExpiresAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const deploymentColumns = `id,application_id,instance_id,endpoint_id,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at`

func scanDeployment(rows interface{ Scan(...any) error }) (*Deployment, error) {
	var d Deployment
	var raw string
	if err := rows.Scan(&d.ID, &d.ApplicationID, &d.InstanceID, &d.EndpointID, &d.State, &d.Revision, &d.SpecDigest, &d.MappingVersion, &raw, &d.CreatedBy, &d.CreatedAt, &d.ExpiresAt); err != nil {
		return nil, err
	}
	if json.Unmarshal([]byte(raw), &d.Plan) != nil {
		return nil, ErrRevisionCorrupt
	}
	d.Expired = !time.Now().Before(d.ExpiresAt)
	return &d, nil
}

func (t *tenancyStore) ReadDeployment(ctx context.Context, a TenantAccess, app, id string) (*Deployment, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	if _, err = uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	var out *Deployment
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		out, err = scanDeployment(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+deploymentColumns+` FROM deployments WHERE organization_id=? AND environment_id=? AND application_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (t *tenancyStore) ListDeployments(ctx context.Context, a TenantAccess, app string) ([]Deployment, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	out := []Deployment{}
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT `+deploymentColumns+` FROM deployments WHERE organization_id=? AND environment_id=? AND application_id=? ORDER BY created_at DESC, id LIMIT 100`), a.OrganizationID, a.EnvironmentID, appID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDeployment(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
```

Add the three methods to the `Tenancy` interface in `internal/store/store.go` next to `PreflightApplication`.

- [ ] **Step 6: Run the store tests**

Run: `gofmt -w internal/store/*.go && go test ./internal/store/ -run 'TestPlanDeployment|Preflight|Mapping' -count=1`
Expected: PASS.

- [ ] **Step 7: Run on PostgreSQL**

Run with `KY_TEST_POSTGRES_DSN` set: `go test ./internal/store/ -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/store && git commit -m "feat: persist executable deployment plans from a clean preflight

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Release refuses a live plan; backup coverage

**Files:**
- Modify: `internal/store/application_adoption.go:187-205` (ReleaseApplication)
- Modify: `internal/store/application_backup_test.go:56-60,100-112`
- Test: `internal/store/deployment_test.go`

**Interfaces:**
- Consumes: `ErrDeploymentPlanned`, `DeploymentPlanTTL`.

- [ ] **Step 1: Write the failing release test**

```go
func TestReleaseRefusesLivePlanAndDeletesExpired(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m)); err != nil {
		t.Fatal(err)
	}
	if err := ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); !errors.Is(err, ErrDeploymentPlanned) {
		t.Fatalf("release with live plan: %v", err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=?`), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 0 {
		t.Fatal("release kept plan rows")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/store/ -run TestReleaseRefusesLivePlan -count=1`
Expected: FAIL at "release with live plan".

- [ ] **Step 3: Add the check to ReleaseApplication**

Inside the `withTenantTarget` closure, before the `DELETE FROM application_instances`:

```go
		var live int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM deployments WHERE instance_id=? AND expires_at>?`), id, time.Now().UTC()).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrDeploymentPlanned
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM deployments WHERE instance_id=?`), id); err != nil {
			return err
		}
```

- [ ] **Step 4: Run the release tests**

Run: `go test ./internal/store/ -run 'Release|Adoption' -count=1`
Expected: PASS.

- [ ] **Step 5: Extend the backup drill**

In `internal/store/application_backup_test.go`: give the snapshot an image (`Images: []protocol.Image{{ID: "sha256:" + strings.Repeat("c", 64), Tags: []string{"nginx:2"}}}`) so the mapped `web` service resolves; after `SetApplicationMapping`, add:

```go
	planned, err := ts.PlanDeployment(ctx, a, app.ID, store.PlanRequest{InstanceID: instance.ID, MappingVersion: 1, Revision: 2, Confirm: "shop"})
	mustTenant(t, err)
```

After the restored mapping assertion add:

```go
	restoredPlan, err := restored.Tenancy().ReadDeployment(ctx, a, app.ID, planned.ID)
	mustTenant(t, err)
	if restoredPlan.Plan.Services[0].ImageID != "sha256:"+strings.Repeat("c", 64) || restoredPlan.MappingVersion != 1 {
		t.Fatalf("plan did not survive restore: %+v", restoredPlan)
	}
```

Note `desired("nginx:2")` is revision 2 and the mapping test maps revision 2, so the preflight is clean only if the image tag in inventory is `nginx:2`.

- [ ] **Step 6: Run the backup test and both drivers**

Run: `go test ./internal/store/ -run 'Backup' -count=1` then the full store suite on PostgreSQL.
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/store && git commit -m "feat: release refuses a live deployment plan; plans survive restore

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: API routes

**Files:**
- Modify: `internal/api/server.go:209` (three routes)
- Modify: `internal/api/application_handlers.go` (three handlers)
- Modify: `internal/api/tenant_handlers.go:40-65` (`tenantError`)
- Test: `internal/api/application_handlers_test.go:84-104`

**Interfaces:**
- Consumes: `store.PlanRequest`, `store.Tenancy().PlanDeployment/ListDeployments/ReadDeployment`, `store.ErrDeploymentPlanned`, `*store.PreflightBlockedError`.
- Produces routes:
  - `POST /api/organizations/{organization}/environments/{environment}/applications/{application}/deployments` → 201 `store.Deployment`
  - `GET .../deployments` → 200 `[]store.Deployment`
  - `GET .../deployments/{deployment}` → 200 `store.Deployment`
  - 409 `{"error":"Deployment preflight reported blockers","code":"preflight_blocked","blockers":[...]}`
  - 409 `{"error":"A live deployment plan exists; let it expire or plan again after release","code":"deployment_planned"}`

- [ ] **Step 1: Write the failing API test**

In `TestApplicationImportRoutes` after the preflight requests (line ~88) insert:

```go
	deployments := base + "/" + app.ID + "/deployments"
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: instance.ID, MappingVersion: 1, Revision: 1, Confirm: "shop"})
	request(nil, "POST", deployments, string(planBody), 401)
	if w := tenantRequest(s, admin, "POST", deployments, string(planBody), false); w.Code != 403 {
		t.Fatal("plan without CSRF")
	}
	// No image in inventory yet: blocked, and the body names the blocker only.
	blocked := request(admin, "POST", deployments, string(planBody), 409)
	if !strings.Contains(blocked, `"preflight_blocked"`) || !strings.Contains(blocked, "image_not_reported") {
		t.Fatalf("blocked body: %s", blocked)
	}
	withImage, _ := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Images: []protocol.Image{{ID: "sha256:" + strings.Repeat("c", 64), Tags: []string{"nginx:1"}}}, Containers: []protocol.Container{{ID: strings.Repeat("a", 64), Name: "shop-web", ImageID: "sha256:" + strings.Repeat("b", 64), ComposeProject: "shop", CreatedAt: time.Now().UTC()}}})
```

The container `CreatedAt` must equal the adopted one to the microsecond; capture the first snapshot's `CreatedAt` in a variable at the top of the test and reuse it here. Then:

```go
	_, err = ts.AcceptInventory(ctx, ep.ID, uint64(time.Now().Unix())+1, time.Now(), withImage)
	must(err)
	var planned store.Deployment
	must(json.Unmarshal([]byte(request(admin, "POST", deployments, string(planBody), 201)), &planned))
	request(admin, "GET", deployments, "", 200)
	request(admin, "GET", deployments+"/"+planned.ID, "", 200)
	request(admin, "GET", deployments+"/not-a-uuid", "", 404)
	request(admin, "GET", "/api/organizations/b/environments/env-b/applications/"+app.ID+"/deployments", "", 403)
	request(admin, "DELETE", adoption, string(releaseBody), 409) // live plan
```

Move the `releaseBody` declaration above this block. In the role loop add `request(admin, "GET", deployments, "", 200)` for every role and, for `RoleDeveloper`, `request(admin, "POST", deployments, string(planBody), 201)`; for operator and read-only `403`. Before the existing release `204`, expire the plan directly: `_, err = st.DB().Exec(...)` — check how tests reach the DB (`st.(*store.SQLStore)`? grep `setupTestServer` return types) and run `UPDATE deployments SET expires_at=<past>`; if the test package cannot reach the DB, instead call `ts.ReleaseApplication` expectations via a second instance: simplest is `st.Tenancy()` exposing no expiry, so use the DB handle. Then the existing `request(admin, "DELETE", adoption, string(releaseBody), 204)` passes.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/api/ -run TestApplicationImportRoutes -count=1`
Expected: FAIL with 404 on the POST (route absent).

- [ ] **Step 3: Add routes, handlers and error mapping**

`internal/api/server.go`, after the preflight route:

```go
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications/{application}/deployments", s.tenantRoute(s.handlePlanDeployment))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/{application}/deployments", s.tenantRoute(s.handleDeployments))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/{application}/deployments/{deployment}", s.tenantRoute(s.handleDeployment))
```

`internal/api/application_handlers.go`:

```go
func (s *Server) handlePlanDeployment(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input store.PlanRequest
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	d, err := s.store.Tenancy().PlanDeployment(r.Context(), a, r.PathValue("application"), input)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, d)
}
func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	list, err := s.store.Tenancy().ListDeployments(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}
func (s *Server) handleDeployment(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	d, err := s.store.Tenancy().ReadDeployment(r.Context(), a, r.PathValue("application"), r.PathValue("deployment"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, d)
}
```

`tenantError`, before the `ErrInvalid` case:

```go
	case errors.Is(err, store.ErrDeploymentPlanned):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "A live deployment plan exists; let it expire before releasing", "code": "deployment_planned"})
```

and at the top of the switch (it is a type, not a sentinel):

```go
	var blocked *store.PreflightBlockedError
	if errors.As(err, &blocked) {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "Deployment preflight reported blockers", "code": "preflight_blocked", "blockers": blocked.Blockers})
		return
	}
```

Check the CSRF gate: the mapping PUT test proves `tenantRoute` enforces CSRF on non-GET; the new POST inherits it.

- [ ] **Step 4: Run the API tests**

Run: `gofmt -w internal/api/*.go && go test ./internal/api/ -run 'TestApplication' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api && git commit -m "feat: deployment plan routes

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Web panel

**Files:**
- Create: `web/src/components/ApplicationDeploymentPlan.tsx`
- Create: `web/src/components/ApplicationDeploymentPlan.test.tsx`
- Modify: `web/src/components/Applications.tsx:83` (render the panel after `ApplicationPreflight`)
- Modify: `web/dist/**` (rebuild with `make build-web`)

**Interfaces:**
- Consumes: `useTenantResource`, `secureFetch`, `StateNotice`, `usePagination`, the `messages` map from `ApplicationPreflight.tsx` (export it: `export const messages`), route shapes from Task 4.

- [ ] **Step 1: Write the failing component tests**

```tsx
// web/src/components/ApplicationDeploymentPlan.test.tsx
import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ApplicationDeploymentPlan } from './ApplicationDeploymentPlan';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const plan = { id: 'd1', application_id: 'app', instance_id: 'i', endpoint_id: 'host', state: 'planned', revision: 2, spec_digest: 'x', mapping_version: 1, created_by: 'u', created_at: '2026-09-22T12:00:00Z', expires_at: '2999-01-01T00:00:00Z', expired: false, plan: { project: 'shop', services: [{ name: 'web', reference: 'nginx:1', image_id: `sha256:${'a'.repeat(64)}`, image_digest: '', container_id: 'b'.repeat(64), replaces: { container_id: 'b'.repeat(64), image_id: `sha256:${'c'.repeat(64)}`, created_unix: 1 }, restart: 'always', ports: [], secret_refs: ['TOKEN'] }] } };
const props = { base: '/app', instanceID: 'i', mappingVersion: 1, revision: 2, project: 'shop' };
it('loads the current plan on open and never offers apply', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify([plan]))));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(`sha256:${'a'.repeat(64)}`);
  expect(screen.getByText(/not executed/i)).toBeTruthy();
  expect(screen.queryByRole('button', { name: /apply|deploy now|execute/i })).toBeNull();
});
it('plans on explicit confirmation and posts the observed bindings', async () => {
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => new Response(JSON.stringify(init?.method === 'POST' ? plan : [])));
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  fireEvent.change(screen.getByLabelText('Confirm plan project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan deployment' }));
  await screen.findByText(`sha256:${'a'.repeat(64)}`);
  const post = fetcher.mock.calls.find(c => (c[1] as RequestInit | undefined)?.method === 'POST');
  expect(post?.[0]).toBe('/app/deployments');
  expect(JSON.parse(String((post?.[1] as RequestInit).body))).toEqual({ instance_id: 'i', mapping_version: 1, revision: 2, confirm: 'shop' });
});
it('renders blockers as fixed text and hides error bodies', async () => {
  vi.stubGlobal('fetch', vi.fn(async (_: string, init?: RequestInit) => init?.method === 'POST' ? new Response(JSON.stringify({ error: 'secret-canary', code: 'preflight_blocked', blockers: ['image_not_reported', 'made_up_blocker'] }), { status: 409 }) : new Response('[]')));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  fireEvent.change(screen.getByLabelText('Confirm plan project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan deployment' }));
  await screen.findByRole('alert');
  expect(document.body.textContent).toContain('not reported');
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(document.body.textContent).not.toContain('made_up_blocker');
});
it('shows an expired plan as expired and hides a plan for another instance', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify([{ ...plan, expired: true }, { ...plan, id: 'd2', instance_id: 'other' }]))));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(/expired/i);
  expect(screen.getAllByRole('row').length).toBe(2); // header + one service
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npx vitest run src/components/ApplicationDeploymentPlan.test.tsx`
Expected: FAIL, module not found.

- [ ] **Step 3: Implement the component**

Export `messages` from `ApplicationPreflight.tsx` (`export const messages`). Create:

```tsx
// web/src/components/ApplicationDeploymentPlan.tsx
import { useState } from 'react';
import { useTenantResource } from '../tenant';
import { secureFetch } from '../api';
import { StateNotice } from './StateNotice';
import { usePagination } from './Pagination';
import { messages } from './ApplicationPreflight';

type PlannedService = { name: string; reference: string; image_id: string; image_digest: string; container_id: string; replaces: { container_id: string; image_id: string; created_unix: number }; restart: string; ports: { target: number; published: number; protocol: string; host_ip: string }[]; secret_refs: string[] };
type Deployment = { id: string; instance_id: string; endpoint_id: string; state: string; revision: number; mapping_version: number; created_at: string; expires_at: string; expired: boolean; plan: { project: string; services: PlannedService[] } };
type Props = { base: string; instanceID: string; mappingVersion: number; revision: number; project: string };

export function ApplicationDeploymentPlan(props: Props) {
  const [open, setOpen] = useState(false);
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" onClick={() => setOpen(!open)}>{open ? 'Close deployment plan' : 'Deployment plan'}</button>
    {open && <PlanView key={`${props.instanceID}/${props.mappingVersion}/${props.revision}`} {...props} />}
  </section>;
}
function PlanView({ base, instanceID, mappingVersion, revision, project }: Props) {
  const resource = useTenantResource<Deployment[]>(`${base}/deployments`);
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const [blocked, setBlocked] = useState(false);
  const [error, setError] = useState<string[]>([]);
  const current = resource.data?.find(d => d.instance_id === instanceID) ?? null;
  const page = usePagination(current?.plan.services ?? [], base);
  const plan = async () => {
    setBusy(true); setError([]);
    try {
      const r = await secureFetch(`${base}/deployments`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ instance_id: instanceID, mapping_version: mappingVersion, revision, confirm }) });
      if (r.ok) { setConfirm(''); resource.reload(); return; }
      setBlocked(true);
      if (r.status === 409) {
        const payload: unknown = await r.json().catch(() => null);
        const blockers = payload && typeof payload === 'object' && Array.isArray((payload as { blockers?: unknown }).blockers) ? ((payload as { blockers: unknown[] }).blockers.filter((b): b is keyof typeof messages => typeof b === 'string' && b in messages)) : [];
        setError(blockers.length ? blockers.map(b => messages[b]) : ['Ownership, mapping or the definition changed. Refresh applications and review before planning again.']);
        return;
      }
      setError([r.status === 403 ? 'You do not have permission to plan deployments.' : 'The plan was refused or its outcome is unknown. Refresh before trying again.']);
    } catch { setBlocked(true); setError(['The outcome is unknown. Refresh before trying again.']); }
    finally { setBusy(false); }
  };
  return <>
    <p>A plan records the exact revision, mapping and image identities a deployment would use. It is not executed here: no containers change, no images are pulled and no secret values are read. Runtime configuration remains unverified.</p>
    <StateNotice state={resource.state} onRetry={resource.reload} />
    {resource.state === 'ready' && (current ? <>
      <p>Plan for revision {current.revision}, mapping version {current.mapping_version}, project <bdi>{current.plan.project}</bdi>. {current.expired ? 'This plan has expired; plan again to continue.' : `Expires ${new Date(current.expires_at).toLocaleString()}.`} This plan is not executed and expires on its own.</p>
      {page.controls}
      <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Pinned image</th><th>Replaces container</th><th>Secrets</th></tr></thead><tbody>{page.rows.map(s => <tr key={s.name}>
        <td data-label="Service"><div className="ky-resource-name"><strong>{s.name}</strong><small>{s.reference} · restart {s.restart || 'default'}</small></div></td>
        <td data-label="Pinned image"><div className="ky-resource-name"><span>{s.image_id}</span><small>{s.image_digest || 'No repository digest reported'}</small></div></td>
        <td data-label="Replaces container"><div className="ky-resource-name"><span>{s.container_id}</span><small>image {s.replaces.image_id}</small></div></td>
        <td data-label="Secrets">{s.secret_refs.length ? `${s.secret_refs.length} reference(s), values not shown` : 'None'}</td>
      </tr>)}</tbody></table>
    </> : <p>No plan for this instance.</p>)}
    {resource.state === 'ready' && <form className="dr-stack" onSubmit={e => { e.preventDefault(); void plan(); }}>
      <p>Planning replaces any earlier plan for this instance. Type the project name <bdi>{project}</bdi> to confirm. Nothing runs.</p>
      <label>Confirm plan project<input value={confirm} onChange={e => setConfirm(e.target.value)} disabled={busy || blocked} autoComplete="off" /></label>
      <button disabled={busy || blocked || confirm !== project}>Plan deployment</button>
      <button type="button" className="btn-secondary" disabled={busy} onClick={() => { setBlocked(false); setError([]); resource.reload(); }}>Refresh plan</button>
      {error.length > 0 && <ul role="alert">{error.map(e => <li key={e}>{e}</li>)}</ul>}
    </form>}
  </>;
}
```

- [ ] **Step 4: Wire it into Applications.tsx**

At `web/src/components/Applications.tsx:83`, after `<ApplicationPreflight org={org} base={...} instanceID={i.id} />` add:

```tsx
<ApplicationDeploymentPlan base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} mappingVersion={i.mapping_version ?? 0} revision={draft.latest_revision} project={i.project} />
```

Check `ApplicationInstance` JSON: `ListApplicationInstances` does not return `mapping_version`. Add it: in `internal/store/application_adoption.go` `ApplicationInstance` add `MappingVersion int \`json:"mapping_version"\`` and select `i.mapping_version` in `ListApplicationInstances` (Task 2's Postgres run covers it; re-run `go test ./internal/store/ -run Instances`). Update the `instances` type in `Applications.tsx` accordingly.

- [ ] **Step 5: Run the web tests and typecheck**

Run: `cd web && npx vitest run && npm run typecheck` (check the exact script name in `web/package.json`).
Expected: PASS.

- [ ] **Step 6: Rebuild dist and commit**

```bash
make build-web && git add web internal/store && git commit -m "feat: deployment plan panel

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Docs, DOX pass, full CI

**Files:**
- Modify: `docs/application-schema.md` (new "Deployment plans" section after "Deployment preflight"; decision table row "Preview validity" → `implemented (plans)`)
- Modify: `docs/authorization-matrix.md:87` (`application.deploy` row: note "plan implemented; apply pending")
- Modify: `internal/store/AGENTS.md` (bullet for `PlanDeployment`/migration 23/release rule)
- Modify: `internal/api/AGENTS.md` (bullet for the three routes and the 409 codes)
- Modify: `web/AGENTS.md` (bullet for the plan panel)
- Modify: `KyYard-Implementation-Plan.md:234-236` (add "Implemented M6 deployment plans" paragraph; next slice becomes apply)

- [ ] **Step 1: Write the schema section**

Under a new `## Deployment plans` heading in `docs/application-schema.md`:

> `POST .../deployments` under `application.deploy` mints a plan from a clean preflight: every blocker other than `runtime_verification_required` refuses with `409 preflight_blocked` naming the blockers. The request binds instance ID, mapping version, revision number and typed project; any mismatch is `409 adoption_changed`. The row records endpoint, project, revision, spec digest, mapping version and a JSON plan: per service the reference, the full image ID the preflight resolved (the pin), the repository digest when inventory reported exactly one, the container it replaces with that container's image ID and creation second, restart, ports and secret reference names. Plans expire after 10 minutes; reads report `expired` and never rewrite state. One plan per instance: planning again replaces the earlier row. Release refuses (`409 deployment_planned`) while an unexpired plan exists and deletes expired ones. No command, pull, secret value or runtime change; apply is the next slice.

- [ ] **Step 2: Update AGENTS.md files and the plan document** with one bullet each matching the section above, and in `KyYard-Implementation-Plan.md` add after the live preflight paragraph:

> Implemented M6 deployment plans: persisted executable previews minted from a clean preflight under `application.deploy`, binding instance, mapping version, revision, spec digest, pinned image IDs and replaced-container identities, expiring after 10 minutes, one per instance, refusing release while live. No agent command.
>
> Next M6 slice: apply a plan through a new agent command (native Engine API pull by pinned ID, create, start, per-step outcomes, precondition rechecks), then history/reapply/remove.

- [ ] **Step 3: Full local CI on both drivers**

Run: `gofmt -l internal cmd; make ci` then `KY_TEST_POSTGRES_DSN=... go test -count=1 ./internal/store/ ./internal/api/`.
Expected: `==> Local CI checks passed`; Postgres PASS.

- [ ] **Step 4: Commit and push**

```bash
git add docs internal web KyYard-Implementation-Plan.md && git commit -m "docs: deployment plans contract

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>" && git push -u origin feat/deployment-plan
```

Then open the PR with the pull-request skill and watch both gates.
