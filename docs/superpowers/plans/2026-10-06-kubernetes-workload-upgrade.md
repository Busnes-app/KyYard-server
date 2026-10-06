# Kubernetes Workload Image Updates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give standalone Kubernetes workloads Docker's image-update parity: a digest pin on apply and run, a per-workload update check, and a one-click *Update image*.

**Architecture:** Everything is server-side registry work plus the existing `workload.apply` frame; the agent is unchanged. A shared `headDigest` helper owns registry access for the Docker check, the workload check and the pin. A read-only store precondition check (`CheckWorkloadFrame`) runs before any registry call. The web reuses the Docker checker/badge with a generalised core.

**Tech Stack:** Go 1.26 (`net/http`, `database/sql`), React 19 + TypeScript + vitest.

**Spec:** `docs/superpowers/specs/2026-10-06-kubernetes-workload-upgrade-design.md`

## Global Constraints

- Work only in the worktree `/home/yoshi/git/busnes.app/KyYard-Server/.worktrees/k8s-workload-upgrade` (branch `feat/k8s-workload-upgrade`).
- Images are pinned by digest: the pin writes `host/repository:tag@sha256:<64 hex>`; never a bare tag.
- Registry credentials never leave the server; nothing new goes to the agent or the cluster.
- Registry error text is never returned; only the closed details `unauthorized`, `not_found`, `rate_limited`, `private_destination`, `unavailable`.
- The update check: service tokens refused (audited `bearer`), 12 per minute per principal on the shared `image-check:` key, `ContainerConfigure`, Kubernetes runtime only, kinds `deployment`, `statefulset`, `daemonset`.
- A refused apply/run spends no registry call; a failed pin sends nothing and records no command.
- KyYard-managed workloads: the check answers `managed` with no registry call; apply stays refused (`application_managed`).
- `web/dist` is committed and must match source (`npm run build` in `web/`).
- Comment style: short, present tense, says why; match the surrounding files.

## Review Focus

- A workload scaled to 0 (no pods): the check must answer `unknown` with an empty container list, not 500 or `up_to_date`.
- An image already written as `repo:tag@sha256:…` by an earlier pin: the check must still compare the tag's current digest (not answer `pinned`), and a second *Update image* must work.
- Pods running on cri-dockerd report `docker-pullable://repo@sha256:…`: the running digest must still be read.
- The same reference in two containers: one registry `Head`, both rewritten.
- A run with the pin box left unticked and no registry access configured must still succeed (no regression for runs).

Each line has its test in the owning task (Tasks 1, 1, 1, 4, 8).

---

### Task 1: Image reference helpers

**Files:**
- Create: `internal/api/workload_images.go`
- Modify: `internal/api/export_test.go` (append)
- Test: `internal/api/workload_images_test.go`

**Interfaces:**
- Produces:
  - `func trackedReference(image string) (name string, ref registry.Reference, pinned string, ok bool)`: `name` is the part before `@` (what `ResolveRegistryAccess` takes), `ref` its parse, `pinned` the digest after `@` or `""`. `ok` is false when the name does not parse or the image is digest-only (no tag).
  - `func runningDigest(imageID string, ref registry.Reference) string`: the pod-reported digest when it names `ref`'s host and repository, else `""`.
  - `func validDigest(s string) bool`
  - test exports `api.TrackedReferenceForTest`, `api.RunningDigestForTest`.

- [ ] **Step 1: Write the failing test**

`internal/api/workload_images_test.go`:

```go
package api_test

import (
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/api"
)

var (
	digestA = "sha256:" + strings.Repeat("a", 64)
	digestB = "sha256:" + strings.Repeat("b", 64)
)

func TestTrackedReference(t *testing.T) {
	for _, c := range []struct {
		image, name, repo, tag, pinned string
		ok                             bool
	}{
		{"nginx", "nginx", "library/nginx", "latest", "", true},
		{"ghcr.io/acme/web:2", "ghcr.io/acme/web:2", "acme/web", "2", "", true},
		{"ghcr.io/acme/web:2@" + digestA, "ghcr.io/acme/web:2", "acme/web", "2", digestA, true},
		{"localhost:5000/web:1@" + digestA, "localhost:5000/web:1", "web", "1", digestA, true},
		// Digest-only tracks nothing.
		{"ghcr.io/acme/web@" + digestA, "", "", "", digestA, false},
		{"localhost:5000/web@" + digestA, "", "", "", digestA, false},
		{"sha256:" + strings.Repeat("c", 64), "", "", "", "", false},
		{"", "", "", "", "", false},
	} {
		name, ref, pinned, ok := api.TrackedReferenceForTest(c.image)
		if ok != c.ok || pinned != c.pinned || (ok && (name != c.name || ref.Repository != c.repo || ref.Tag != c.tag || ref.Digest != "")) {
			t.Errorf("%q: %q %+v %q %v", c.image, name, ref, pinned, ok)
		}
	}
}

func TestRunningDigest(t *testing.T) {
	_, ref, _, _ := api.TrackedReferenceForTest("nginx:1.27")
	for _, c := range []struct{ id, want string }{
		{"docker.io/library/nginx@" + digestA, digestA},
		{"docker-pullable://nginx@" + digestB, digestB},
		{"ghcr.io/acme/nginx@" + digestA, ""},
		{"docker.io/library/nginx@sha256:short", ""},
		{"sha256:" + strings.Repeat("c", 64), ""},
		{"", ""},
	} {
		if got := api.RunningDigestForTest(c.id, ref); got != c.want {
			t.Errorf("%q: %q, want %q", c.id, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api -run '^Test(TrackedReference|RunningDigest)$'`
Expected: FAIL, `undefined: api.TrackedReferenceForTest`.

- [ ] **Step 3: Write minimal implementation**

`internal/api/workload_images.go`:

```go
package api

import (
	"regexp"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/registry"
)

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func validDigest(s string) bool { return sha256Digest.MatchString(s) }

// trackedReference splits a workload image into the tag it tracks and the digest it is pinned
// at. registry.ParseReference refuses a tag beside a digest, so the pin is cut off first; a
// digest with no tag tracks nothing.
func trackedReference(image string) (name string, ref registry.Reference, pinned string, ok bool) {
	name, pinned, _ = strings.Cut(image, "@")
	if pinned != "" && !strings.Contains(name[strings.LastIndex(name, "/")+1:], ":") {
		return "", registry.Reference{}, pinned, false
	}
	ref, err := registry.ParseReference(name)
	if err != nil {
		return "", registry.Reference{}, pinned, false
	}
	return name, ref, pinned, true
}

// runningDigest reads a pod's reported image ID (containerd "repo@sha256:…", cri-dockerd
// "docker-pullable://repo@sha256:…") as the digest of ref's repository, or "".
func runningDigest(imageID string, ref registry.Reference) string {
	repo, digest, ok := strings.Cut(strings.TrimPrefix(imageID, "docker-pullable://"), "@")
	if !ok || !validDigest(digest) {
		return ""
	}
	parsed, err := registry.ParseReference(repo)
	if err != nil || parsed.Host != ref.Host || parsed.Repository != ref.Repository {
		return ""
	}
	return digest
}
```

Append to `internal/api/export_test.go`:

```go
var TrackedReferenceForTest = trackedReference
var RunningDigestForTest = runningDigest
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api -run '^Test(TrackedReference|RunningDigest)$'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/workload_images.go internal/api/workload_images_test.go internal/api/export_test.go
git commit -m "api: parse tracked workload image references"
```

---

### Task 2: Shared registry `headDigest`

Extract the credential-and-Head step and the closed-detail mapping out of `handleContainerUpdateCheck` so the workload code reuses them. No behaviour change: `TestContainerUpdateCheckUsesImmutableLocalImageAndRegistryPolicy` is the regression test.

**Files:**
- Modify: `internal/api/workload_images.go`
- Modify: `internal/api/container_update_handlers.go:91-123`

**Interfaces:**
- Produces:
  - `var errRegistryHead = errors.New("registry head failed")`
  - `func (s *Server) headDigest(ctx context.Context, a store.TenantAccess, name string, ref registry.Reference) (string, error)`: a policy refusal comes back as the store error (for `tenantError`); a registry failure wraps both `errRegistryHead` and the registry error. The caller holds the registry slot.
  - `func registryDetail(err error) string`: the closed detail word.

- [ ] **Step 1: Run the regression test first, to see it pass before the refactor**

Run: `go test ./internal/api -run '^TestContainerUpdateCheck'`
Expected: PASS.

- [ ] **Step 2: Add the helpers** to `internal/api/workload_images.go` (extend its imports with `context`, `errors`, `fmt`, `permissions`, `store`):

```go
// errRegistryHead marks a registry failure, as opposed to a registry policy refusal.
var errRegistryHead = errors.New("registry head failed")

// headDigest resolves name's current digest under the organization's registry access: the one
// set of rules every update check and pin uses. The caller holds a registry slot.
func (s *Server) headDigest(ctx context.Context, a store.TenantAccess, name string, ref registry.Reference) (string, error) {
	access, err := s.store.Tenancy().ResolveRegistryAccess(ctx, a, permissions.ImagePull, name, s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, store.ImageCheckDeadline)
	defer cancel()
	digest, err := s.resolver().Head(ctx, ref, access.Credential, access.Registry != nil && access.Registry.AllowPrivate)
	if err == nil && !validDigest(digest) {
		err = registry.ErrUnavailable
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", errRegistryHead, err)
	}
	return digest, nil
}

// registryDetail is the closed word for a registry failure: registry text never reaches a client.
func registryDetail(err error) string {
	switch {
	case errors.Is(err, registry.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, registry.ErrNotFound):
		return "not_found"
	case errors.Is(err, registry.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, registry.ErrPrivateDestination):
		return "private_destination"
	}
	return "unavailable"
}
```

- [ ] **Step 3: Use them in `handleContainerUpdateCheck`.** Replace the block from `access, err := s.store.Tenancy().ResolveRegistryAccess(` through the closing `}` of the `if err != nil { … } else { … }` verdict block with:

```go
	// Policy before budget: a refused organization spends no registry slot.
	if _, err := s.store.Tenancy().ResolveRegistryAccess(r.Context(), a, permissions.ImagePull, reference, s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate); err != nil {
		s.tenantError(w, err)
		return
	}
	release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
	if !ok {
		return
	}
	defer release()
	extendRegistryDeadline(w)
	remote, err := s.headDigest(r.Context(), a, reference, ref)
	switch {
	case errors.Is(err, errRegistryHead):
		out["verdict"], out["detail"] = "registry_error", registryDetail(err)
	case err != nil:
		s.tenantError(w, err)
		return
	default:
		out["remote_digest"] = remote
		if out["local_digest"] != "" {
			if out["local_digest"] == remote {
				out["verdict"] = "up_to_date"
			} else {
				out["verdict"] = "update_available"
			}
		}
	}
```

Drop the now-unused `context` import if the compiler reports it.

- [ ] **Step 4: Run the regression test**

Run: `go test ./internal/api -run '^TestContainerUpdateCheck' && go vet ./internal/api`
Expected: PASS, no vet output.

- [ ] **Step 5: Commit**

```bash
git add internal/api/workload_images.go internal/api/container_update_handlers.go
git commit -m "api: share registry head between update checks"
```

---

### Task 3: Store `CheckWorkloadFrame`

**Files:**
- Modify: `internal/store/commands_workload.go:240-310` (`createWorkloadFrame`)
- Modify: `internal/store/store.go` (the `TenancyStore` interface, beside `CreateWorkloadRun`)
- Test: `internal/store/commands_workload_test.go`

**Interfaces:**
- Produces: `CheckWorkloadFrame(ctx context.Context, a TenantAccess, endpointID string, w WorkloadApply, create bool) error`: `CreateWorkloadApply` (create false) or `CreateWorkloadRun` (create true) preconditions, read-only, no audit row, no command.

- [ ] **Step 1: Write the failing test** (append to `commands_workload_test.go`):

```go
// CheckWorkloadFrame refuses what CreateWorkloadApply and CreateWorkloadRun refuse, and writes
// nothing: no command, so a later create is not busy.
func TestCheckWorkloadFrame(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeClusterWith(t, ts, a, testCluster(), "shop")
	for name, c := range map[string]struct {
		w      func() WorkloadApply
		create bool
		want   error
	}{
		"confirm":   {func() WorkloadApply { w := testApply(); w.Confirm = "api"; return w }, false, ErrInvalid},
		"managed":   {func() WorkloadApply { w := testApply(); w.Target.Name, w.Confirm = "api", "api"; return w }, false, ErrWorkloadManaged},
		"namespace": {func() WorkloadApply { w := testApply(); w.Target.Namespace = "other"; return w }, false, ErrNamespaceNotGranted},
		"absent":    {func() WorkloadApply { w := testApply(); w.Target.Name, w.Confirm = "ghost", "ghost"; return w }, false, ErrNotFound},
		"taken":     {func() WorkloadApply { w := testRun(); w.Target.Name, w.Confirm = "web", "web"; return w }, true, nil},
	} {
		err := ts.CheckWorkloadFrame(ctx, a, cluster, c.w(), c.create)
		if name == "taken" {
			var spec *InvalidSpecError
			if !errors.As(err, &spec) || spec.Blockers[0] != "name_taken" {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if err := ts.CheckWorkloadFrame(ctx, a, cluster, testApply(), false); err != nil {
		t.Fatalf("valid apply: %v", err)
	}
	if err := ts.CheckWorkloadFrame(ctx, a, cluster, testRun(), true); err != nil {
		t.Fatalf("valid run: %v", err)
	}
	// Nothing was written: the create is not busy.
	if _, _, err := ts.CreateWorkloadApply(ctx, a, cluster, testApply()); err != nil {
		t.Fatalf("apply after checks: %v", err)
	}
	if err := ts.CheckWorkloadFrame(ctx, a, cluster, testApply(), false); !errors.Is(err, ErrCommandInProgress) {
		t.Fatalf("check while busy: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store -run '^TestCheckWorkloadFrame$'`
Expected: FAIL, `ts.CheckWorkloadFrame undefined`.

- [ ] **Step 3: Refactor `createWorkloadFrame`** into a pure frame builder, a shared precondition function, and the two callers. Replace the whole function with:

```go
// newWorkloadFrame checks the request's shape and builds the command, frame and intent details.
func newWorkloadFrame(a TenantAccess, endpointID string, w WorkloadApply, create bool) (*Command, *protocol.WorkloadApply, string, error) {
	if parsed, err := protocol.ParseWorkloadRef(w.Target.String()); err != nil || parsed != w.Target || w.Target.Kind == protocol.WorkloadPod {
		return nil, nil, "", ErrInvalid
	}
	if w.Confirm != w.Target.Name {
		return nil, nil, "", fmt.Errorf("%w: confirm must be %q", ErrInvalid, w.Target.Name)
	}
	if create && w.Target.Kind != protocol.WorkloadDeployment {
		return nil, nil, "", invalidSpec("spec_invalid:target.kind")
	}
	now := time.Now().UTC()
	action := ActionWorkloadApply
	if create {
		action = ActionWorkloadRun
	}
	cmd := &Command{ID: uuid.NewString(), EndpointID: endpointID, ActorID: a.ActorID, RequestID: a.CorrelationID, Action: action, Reference: w.Target.String(), Expects: protocol.Expectation{}, CreatedAt: now, Deadline: now.Add(DeploymentApplyDeadline)}
	spec := w.Spec
	spec.Target, spec.ObservedAt = w.Target, time.Time{}
	frame := &protocol.WorkloadApply{Request: cmd.ID, Endpoint: endpointID, IssuedAt: now, Deadline: cmd.Deadline, Target: w.Target, ResourceVersion: spec.ResourceVersion, Spec: spec, Create: create}
	details := fmt.Sprintf("resource_version=%s containers=%d", protocol.CleanText(spec.ResourceVersion, protocol.MaxResourceVersionBytes), len(spec.Containers))
	if create {
		replicas := "-"
		if spec.Replicas != nil {
			replicas = fmt.Sprint(*spec.Replicas)
		}
		details = fmt.Sprintf("containers=%d replicas=%s", len(spec.Containers), replicas)
	}
	return cmd, frame, details, nil
}

// workloadPreconditions is everything an apply or run checks inside its transaction: an active
// cluster in scope, no live direct command, the target (free for a run, unmanaged for an apply),
// a complete spec and a frame the protocol accepts. sweep settles expired direct commands
// first, which only the writing path may do.
func (t *tenancyStore) workloadPreconditions(ctx context.Context, tx *sql.Tx, a TenantAccess, endpointID string, frame *protocol.WorkloadApply, create, sweep bool, now time.Time) (org, env string, err error) {
	var state, runtime, namespaces string
	org, env, state, runtime, namespaces, err = t.clusterEndpoint(ctx, tx, a, endpointID)
	if err != nil {
		return "", "", err
	}
	if state != "active" {
		return "", "", fmt.Errorf("%w: it is %s", ErrEndpointOffline, state)
	}
	if sweep {
		if err := t.sweepDirect(ctx, tx, endpointID, now); err != nil {
			return "", "", err
		}
	}
	if err := t.directBusy(ctx, tx, endpointID, now); err != nil {
		return "", "", err
	}
	if create {
		if err := t.workloadNameFree(ctx, tx, endpointID, runtime, namespaces, frame.Target); err != nil {
			return "", "", err
		}
	} else if managed, _, err := t.clusterTarget(ctx, tx, endpointID, runtime, namespaces, frame.Target); err != nil {
		return "", "", err
	} else if managed || frame.Spec.Managed {
		return "", "", ErrWorkloadManaged
	}
	if len(frame.Spec.Unsupported) > 0 {
		return "", "", invalidSpec("configuration_incomplete")
	}
	if err := frame.Validate(now); err != nil {
		field := "frame"
		if fe := new(protocol.FieldError); errors.As(err, &fe) {
			field = fe.Field
		}
		return "", "", invalidSpec("spec_invalid:" + strings.NewReplacer(", ", "_", " ", "_").Replace(field))
	}
	return org, env, nil
}

// CheckWorkloadFrame runs CreateWorkloadApply's (create false) or CreateWorkloadRun's (create
// true) preconditions without writing, so the API refuses before spending registry budget.
func (t *tenancyStore) CheckWorkloadFrame(ctx context.Context, a TenantAccess, endpointID string, w WorkloadApply, create bool) error {
	_, frame, _, err := newWorkloadFrame(a, endpointID, w, create)
	if err != nil {
		return err
	}
	return t.readTenant(ctx, a, permissions.ContainerConfigure, func(tx *sql.Tx) error {
		_, _, err := t.workloadPreconditions(ctx, tx, a, endpointID, frame, create, false, frame.IssuedAt)
		return err
	})
}

func (t *tenancyStore) createWorkloadFrame(ctx context.Context, a TenantAccess, endpointID string, w WorkloadApply, create bool) (*Command, *protocol.WorkloadApply, error) {
	if a.CorrelationID == "" {
		a.CorrelationID = uuid.NewString()
	}
	cmd, frame, details, err := newWorkloadFrame(a, endpointID, w, create)
	if err != nil {
		return nil, nil, err
	}
	target := endpointID + "/" + cmd.Reference
	err = t.runAs(ctx, a, permissions.ContainerConfigure, cmd.Action, &target, &details, true, func(tx *sql.Tx) error {
		var err error
		if cmd.OrganizationID, cmd.EnvironmentID, err = t.workloadPreconditions(ctx, tx, a, endpointID, frame, create, true, cmd.CreatedAt); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_commands (id,endpoint_id,organization_id,environment_id,actor_id,request_id,action,container_id,reference,expects,deadline,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`),
			cmd.ID, cmd.EndpointID, cmd.OrganizationID, cmd.EnvironmentID, cmd.ActorID, cmd.RequestID, cmd.Action, "", cmd.Reference, "{}", cmd.Deadline, cmd.CreatedAt)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return cmd, frame, nil
}
```

Check before replacing: the original `runAs` call passed `action` (now `cmd.Action`, the same value), and `cmd.OrganizationID`/`EnvironmentID` were set from `clusterEndpoint`. Keep the original doc comments on `CreateWorkloadApply`/`CreateWorkloadRun`.

Add to the `TenancyStore` interface in `internal/store/store.go`, beside `CreateWorkloadRun`:

```go
	CheckWorkloadFrame(ctx context.Context, access TenantAccess, endpointID string, w WorkloadApply, create bool) error
```

If an API test fake implements `TenancyStore` (e.g. `faultyTenancy` in `internal/api`), it embeds the real store, so no change is needed; confirm with `go vet ./...`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/store -run 'Workload' && go vet ./...`
Expected: PASS (the new test and the existing `TestWorkloadApplyCommand`, `TestWorkloadRunCommand`).

- [ ] **Step 5: Commit**

```bash
git add internal/store/commands_workload.go internal/store/commands_workload_test.go internal/store/store.go
git commit -m "store: check workload frames without writing"
```

---

### Task 4: Pin on apply and run

**Files:**
- Modify: `internal/api/workload_handlers.go:120-195`
- Modify: `internal/api/workload_images.go`
- Test: `internal/api/workload_pin_test.go`

**Interfaces:**
- Consumes: `trackedReference`, `headDigest`, `errRegistryHead` (Tasks 1, 2); `CheckWorkloadFrame` (Task 3).
- Produces: apply body `{"resource_version","spec","confirm","pull":[names]}`; run body `{"spec","confirm","pull":[names]}`. `func (s *Server) pinWorkloadImages(w http.ResponseWriter, r *http.Request, a store.TenantAccess, spec *protocol.WorkloadConfiguration, pull []string) bool`.

- [ ] **Step 1: Write the failing tests** in `internal/api/workload_pin_test.go`. They use the `workloadFleet` helpers from `workload_test.go` (`newWorkloadFleet`, `workloadConfiguration`, `applyBody`, `workloadRunBody`, `readEnvelope`) and the resolvers from `recreate_test.go` (`countingResolver`, `fixedResolver`) and `fakeDigests` from the Docker check test.

```go
package api_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func withPull(t *testing.T, body string, pull ...string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	m["pull"] = pull
	raw, _ := json.Marshal(m)
	return string(raw)
}

func (f *workloadFleet) anonymousPulls(t *testing.T) {
	t.Helper()
	if err := f.st.Tenancy().SetAnonymousPull(f.ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: "a"}, true); err != nil {
		t.Fatal(err)
	}
}

func (f *workloadFleet) appliedImages(t *testing.T) []string {
	t.Helper()
	e := readEnvelope(t, f.ctx, f.conn)
	var frame protocol.WorkloadApply
	if e.Type != protocol.TypeWorkloadApply || json.Unmarshal(e.Payload, &frame) != nil || frame.Validate(time.Now()) != nil {
		t.Fatalf("frame %s %s", e.Type, e.Payload)
	}
	var out []string
	for _, c := range append(frame.Spec.Containers, frame.Spec.InitContainers...) {
		out = append(out, c.Image)
	}
	return out
}

// Pull pins each named container at its tag's registry digest, keeps the tag, re-resolves an
// earlier pin from its tag, and Heads a repeated reference once.
func TestWorkloadApplyPinsPulledImages(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	f.anonymousPulls(t)
	resolver := &countingResolver{}
	api.SetDigestResolverForTest(f.s, resolver)
	pinned := "sha256:" + strings.Repeat("f", 64)
	cfg := workloadConfiguration(protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "web"})
	cfg.Containers[0].Image = "ghcr.io/acme/web:2@sha256:" + strings.Repeat("1", 64)
	side := cfg.Containers[0]
	side.Name, side.Image, side.Env = "side", "ghcr.io/acme/web:2", []protocol.WorkloadEnv{}
	keep := side
	keep.Name, keep.Image = "keep", "ghcr.io/acme/other:1"
	cfg.Containers = append(cfg.Containers, side, keep)
	w := tenantRequest(f.s, f.org, "POST", f.webWorkload+"/apply", withPull(t, applyBody(t, cfg, "web"), "web", "side"), true)
	if w.Code != 202 {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
	got := f.appliedImages(t)
	want := []string{"ghcr.io/acme/web:2@" + pinned, "ghcr.io/acme/web:2@" + pinned, "ghcr.io/acme/other:1"}
	if strings.Join(got, ",") != strings.Join(want, ",") || resolver.calls.Load() != 1 {
		t.Fatalf("images %v, %d heads", got, resolver.calls.Load())
	}
}

// Refusals spend no registry call and record no command.
func TestWorkloadPinRefusals(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	resolver := &countingResolver{}
	api.SetDigestResolverForTest(f.s, resolver)
	cfg := workloadConfiguration(protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "web"})
	managed := workloadConfiguration(protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "api"})
	for name, c := range map[string]struct {
		path, body string
		code       int
	}{
		"unknown container":  {f.webWorkload + "/apply", withPull(t, applyBody(t, cfg, "web"), "ghost"), 400},
		"duplicate":          {f.webWorkload + "/apply", withPull(t, applyBody(t, cfg, "web"), "web", "web"), 400},
		"managed":            {f.clusterPath + "/workloads/shop/deployment/api/apply", withPull(t, applyBody(t, managed, "api"), "web"), 409},
		"wrong confirm":      {f.webWorkload + "/apply", withPull(t, applyBody(t, cfg, "api"), "web"), 400},
		"no registry access": {f.webWorkload + "/apply", withPull(t, applyBody(t, cfg, "web"), "web"), 409},
	} {
		if w := tenantRequest(f.s, f.org, "POST", c.path, c.body, true); w.Code != c.code {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	f.anonymousPulls(t)
	digestOnly := *cfg
	digestOnly.Containers = []protocol.WorkloadContainer{cfg.Containers[0]}
	digestOnly.Containers[0].Image = "ghcr.io/acme/web@sha256:" + strings.Repeat("1", 64)
	if w := tenantRequest(f.s, f.org, "POST", f.webWorkload+"/apply", withPull(t, applyBody(t, &digestOnly, "web"), "web"), true); w.Code != 422 || !strings.Contains(w.Body.String(), "image_unresolved") {
		t.Fatalf("digest only: %d %s", w.Code, w.Body.String())
	}
	if n := resolver.calls.Load(); n != 0 {
		t.Fatalf("%d registry calls for refused pins", n)
	}
	api.SetDigestResolverForTest(f.s, &fakeDigests{err: errors.Join(errors.New("sensitive-registry-diagnostic"), registry.ErrNotFound)})
	w := tenantRequest(f.s, f.org, "POST", f.webWorkload+"/apply", withPull(t, applyBody(t, cfg, "web"), "web"), true)
	if w.Code != 422 || !strings.Contains(w.Body.String(), "image_unresolved") || strings.Contains(w.Body.String(), "sensitive") {
		t.Fatalf("unresolved: %d %s", w.Code, w.Body.String())
	}
	f.sync(t) // no frame was sent
	commands, err := f.st.Tenancy().ListCommands(f.ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: "a"}, f.cluster.id, "", "", 50)
	if err != nil || len(commands) != 0 {
		t.Fatalf("refusals recorded commands: %v %v", commands, err)
	}
}

// A run pins when asked and, unticked, still runs with no registry access configured.
func TestWorkloadRunPins(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	api.SetDigestResolverForTest(f.s, fixedResolver{"sha256:" + strings.Repeat("e", 64)})
	if w := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/workloads", workloadRunBody(t, "shop", "fresh", "fresh"), true); w.Code != 202 {
		t.Fatalf("unpinned run: %d %s", w.Code, w.Body.String())
	}
	if got := f.appliedImages(t); got[0] != "ghcr.io/acme/web:1" {
		t.Fatalf("unpinned run image %v", got)
	}
	f.settleLast(t)
	f.anonymousPulls(t)
	if w := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/workloads", withPull(t, workloadRunBody(t, "shop", "pinned", "pinned"), "web"), true); w.Code != 202 {
		t.Fatalf("pinned run: %d %s", w.Code, w.Body.String())
	}
	if got := f.appliedImages(t); got[0] != "ghcr.io/acme/web:1@sha256:"+strings.Repeat("e", 64) {
		t.Fatalf("pinned run image %v", got)
	}
}
```

`f.settleLast` does not exist yet; add it in this file. It settles the last sent command so the next one is not `command_in_progress`:

```go
func (f *workloadFleet) settleLast(t *testing.T) {
	t.Helper()
	commands, err := f.st.Tenancy().ListCommands(f.ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: "a"}, f.cluster.id, "", "", 1)
	if err != nil || len(commands) != 1 {
		t.Fatalf("last command: %v %v", commands, err)
	}
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: commands[0].ID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: protocol.WorkloadApplyService, Step: protocol.StepApply, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{}})
	f.sync(t)
}
```

If `fakeDigests` lives in another test file of package `api_test`, it is already visible; check its constructor fields with `grep -n "type fakeDigests" -A8 internal/api/*_test.go` and adapt the literal if the field is not `err`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api -run '^TestWorkload(ApplyPinsPulledImages|PinRefusals|RunPins)$'`
Expected: FAIL. The apply strict-decodes the body, so `pull` is rejected with 400.

- [ ] **Step 3: Implement the pin.** Add to `internal/api/workload_images.go`:

```go
// pinWorkloadImages rewrites each container pull names to its tag at the registry's current
// digest, one Head per reference. It writes the response and reports false on refusal: an
// unknown or repeated name is invalid, a digest-only image or a registry failure is
// image_unresolved, a policy refusal is the store's.
func (s *Server) pinWorkloadImages(w http.ResponseWriter, r *http.Request, a store.TenantAccess, spec *protocol.WorkloadConfiguration, pull []string) bool {
	containers := map[string]*protocol.WorkloadContainer{}
	for _, group := range [][]protocol.WorkloadContainer{spec.Containers, spec.InitContainers} {
		for i := range group {
			containers[group[i].Name] = &group[i]
		}
	}
	targets := make([]*protocol.WorkloadContainer, 0, len(pull))
	for _, name := range pull {
		c, ok := containers[name]
		if !ok {
			s.tenantError(w, store.ErrInvalid)
			return false
		}
		delete(containers, name) // a repeated name is unknown the second time
		targets = append(targets, c)
	}
	unresolved := &store.InvalidSpecError{Blockers: []string{"image_unresolved"}}
	type tracked struct {
		name string
		ref  registry.Reference
	}
	refs := make([]tracked, len(targets))
	for i, c := range targets {
		name, ref, _, ok := trackedReference(c.Image)
		if !ok {
			s.tenantError(w, unresolved)
			return false
		}
		refs[i] = tracked{name, ref}
	}
	release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
	if !ok {
		return false
	}
	defer release()
	extendRegistryDeadline(w)
	digests := map[string]string{}
	for i, c := range targets {
		digest, seen := digests[refs[i].name]
		if !seen {
			var err error
			if digest, err = s.headDigest(r.Context(), a, refs[i].name, refs[i].ref); errors.Is(err, errRegistryHead) {
				s.tenantError(w, unresolved)
				return false
			} else if err != nil {
				s.tenantError(w, err)
				return false
			}
			digests[refs[i].name] = digest
		}
		c.Image = refs[i].name + "@" + digest
	}
	return true
}
```

Add `net/http` and `protocol` to its imports.

In `internal/api/workload_handlers.go`:

1. Both body structs gain `Pull []string \`json:"pull"\``.
2. Change `sendWorkloadFrame`'s signature from taking a `create func(...)` to `create bool, pull []string`, and choose the store call inside it:

```go
// sendWorkloadFrame records an apply or a run (create) and sends its frame. With pull it first
// runs the store's preconditions, so a refused request spends no registry call, then pins the
// named containers. Offline 409, unsent 409 deployment_not_sent (the row settled failed), else
// 202 with the command.
func (s *Server) sendWorkloadFrame(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpoint string, create bool, pull []string, wa store.WorkloadApply) {
	if !s.Connected(endpoint) {
		s.tenantError(w, store.ErrEndpointOffline)
		return
	}
	if len(pull) > 0 {
		if err := s.store.Tenancy().CheckWorkloadFrame(r.Context(), a, endpoint, wa, create); err != nil {
			s.tenantError(w, err)
			return
		}
		if !s.pinWorkloadImages(w, r, a, &wa.Spec, pull) {
			return
		}
	}
	record := s.store.Tenancy().CreateWorkloadApply
	if create {
		record = s.store.Tenancy().CreateWorkloadRun
	}
	cmd, frame, err := record(r.Context(), a, endpoint, wa)
	// … the rest of the function body is unchanged from `if err != nil {` onwards.
```

3. Callers: `handleApplyWorkload` calls `s.sendWorkloadFrame(w, r, a, endpoint, false, body.Pull, store.WorkloadApply{…})`; `handleRunWorkload` calls it with `true, body.Pull`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api -run 'Workload' && go vet ./internal/api`
Expected: PASS, including the existing `TestWorkloadRoutesPermissionMatrix`, `TestWorkloadRefusals` and `TestWorkloadConfigurationAndApplySettle`.

- [ ] **Step 5: Commit**

```bash
git add internal/api/workload_images.go internal/api/workload_handlers.go internal/api/workload_pin_test.go
git commit -m "api: pin workload images at the registry digest"
```

---

### Task 5: Workload update check

**Files:**
- Create: `internal/api/workload_update_handlers.go`
- Modify: `internal/api/server.go` (route, after the `/apply` route at line 333)
- Test: `internal/api/workload_update_test.go`

**Interfaces:**
- Consumes: `trackedReference`, `runningDigest`, `headDigest`, `registryDetail`, `errRegistryHead`.
- Produces: `POST /api/organizations/{organization}/endpoints/{endpoint}/workloads/{namespace}/{kind}/{name}/updates/check`, which returns

```json
{"workload":"shop/deployment/web","verdict":"…","detail":"","checked_at":"RFC3339",
 "containers":[{"name":"web","reference":"…","local_digest":"…","remote_digest":"…","verdict":"…","detail":""}]}
```

- [ ] **Step 1: Write the failing tests** in `internal/api/workload_update_test.go`:

```go
package api_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/registry"
)

type workloadCheck struct {
	Workload, Verdict, Detail string
	Containers                []struct{ Name, Reference, LocalDigest, RemoteDigest, Verdict, Detail string } `json:"containers"`
}

// report replaces the cluster inventory with web's pods running images and image IDs.
func (f *workloadFleet) report(t *testing.T, pods ...protocol.Pod) {
	t.Helper()
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()) - 1000 + reports.Add(1), Kubernetes: &protocol.KubernetesInventory{
		Namespaces: []string{"shop", "other"},
		Workloads: []protocol.Workload{
			{Kind: "Deployment", Namespace: "shop", Name: "web"},
			{Kind: "Deployment", Namespace: "shop", Name: "api", Application: "shop", Instance: "shop"},
		},
		Pods: pods,
	}})
	f.sync(t)
}

func webPod(name string, containers ...protocol.PodContainer) protocol.Pod {
	return protocol.Pod{Namespace: "shop", Name: name, Phase: "Running", OwnerKind: "Deployment", OwnerName: "web", Containers: containers}
}

func (f *workloadFleet) check(t *testing.T, path string) workloadCheck {
	t.Helper()
	w := tenantRequest(f.s, f.org, "POST", path+"/updates/check", "", true)
	var out workloadCheck
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("check %s: %d %s", path, w.Code, w.Body.String())
	}
	return out
}

func TestWorkloadUpdateCheckVerdicts(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	f.anonymousPulls(t)
	remote := "sha256:" + strings.Repeat("b", 64)
	old := "sha256:" + strings.Repeat("a", 64)
	fake := &fakeDigests{digest: remote}
	api.SetDigestResolverForTest(f.s, fake)
	running := func(image, id string) protocol.PodContainer {
		return protocol.PodContainer{Name: "web", Image: image, ImageID: id, State: "running"}
	}
	for name, c := range map[string]struct {
		pods    []protocol.Pod
		verdict string
	}{
		"update":       {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2", "ghcr.io/acme/web@"+old))}, "update_available"},
		"current":      {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2", "ghcr.io/acme/web@"+remote))}, "up_to_date"},
		"earlier pin":  {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2@"+old, "ghcr.io/acme/web@"+old))}, "update_available"},
		"digest only":  {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web@"+old, "ghcr.io/acme/web@"+old))}, "pinned"},
		"cri-dockerd":  {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2", "docker-pullable://ghcr.io/acme/web@"+remote))}, "up_to_date"},
		"mid-rollout":  {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2", "ghcr.io/acme/web@"+old)), webPod("web-2", running("ghcr.io/acme/web:2", "ghcr.io/acme/web@"+remote))}, "unknown"},
		"no pods":      {nil, "unknown"},
		"not reported": {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2", ""))}, "unknown"},
	} {
		f.report(t, c.pods...)
		if got := f.check(t, f.webWorkload); got.Verdict != c.verdict || got.Workload != "shop/deployment/web" {
			t.Errorf("%s: %+v", name, got)
		}
	}
	f.report(t, webPod("web-1", running("ghcr.io/acme/web:2", "ghcr.io/acme/web@"+old)))
	calls := fake.calls()
	if got := f.check(t, f.clusterPath+"/workloads/shop/deployment/api"); got.Verdict != "managed" || len(got.Containers) != 0 || fake.calls() != calls {
		t.Fatalf("managed: %+v, %d heads", got, fake.calls()-calls)
	}
	for _, c := range []struct {
		err    error
		detail string
	}{{registry.ErrUnauthorized, "unauthorized"}, {registry.ErrNotFound, "not_found"}, {registry.ErrRateLimited, "rate_limited"}, {registry.ErrPrivateDestination, "private_destination"}, {registry.ErrUnavailable, "unavailable"}} {
		api.SetDigestResolverForTest(f.s, &fakeDigests{err: fmt.Errorf("sensitive-registry-diagnostic: %w", c.err)})
		w := tenantRequest(f.s, f.org, "POST", f.webWorkload+"/updates/check", "", true)
		var got workloadCheck
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Verdict != "registry_error" || got.Detail != c.detail || got.Containers[0].Detail != c.detail || strings.Contains(w.Body.String(), "sensitive") {
			t.Errorf("%s: %d %s", c.detail, w.Code, w.Body.String())
		}
	}
}

func TestWorkloadUpdateCheckRefusals(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	api.SetDigestResolverForTest(f.s, &fakeDigests{digest: "sha256:" + strings.Repeat("b", 64)})
	f.report(t, webPod("web-1", protocol.PodContainer{Name: "web", Image: "ghcr.io/acme/web:2", ImageID: "ghcr.io/acme/web@sha256:" + strings.Repeat("a", 64)}))
	path := f.webWorkload + "/updates/check"
	if w := tenantRequest(f.s, f.org, "POST", path, "", false); w.Code != 403 {
		t.Fatalf("CSRF %d", w.Code)
	}
	if w := tenantRequest(f.s, f.org, "POST", path, "", true); w.Code != 409 || !strings.Contains(w.Body.String(), "anonymous_pull_disabled") && !strings.Contains(w.Body.String(), "registry_not_configured") {
		t.Fatalf("registry policy %d %s", w.Code, w.Body.String())
	}
	if w := tenantRequest(f.s, f.viewer, "POST", path, "", true); w.Code != 403 {
		t.Fatalf("viewer %d", w.Code)
	}
	if w := tenantRequest(f.s, f.org, "POST", f.hostPath+"/workloads/shop/deployment/web/updates/check", "", true); w.Code != 409 {
		t.Fatalf("docker endpoint %d %s", w.Code, w.Body.String())
	}
	if w := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/workloads/shop/pod/web-1/updates/check", "", true); w.Code != 400 {
		t.Fatalf("pod kind %d", w.Code)
	}
	if w := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/workloads/shop/deployment/ghost/updates/check", "", true); w.Code != 404 {
		t.Fatalf("absent %d", w.Code)
	}
	token, _ := f.service(t)
	if w := bearerRequest(f.s, token, "POST", path, ""); w.Code != 403 {
		t.Fatalf("service token %d", w.Code)
	}
	f.anonymousPulls(t)
	limited := false
	for range 13 {
		if w := tenantRequest(f.s, f.org, "POST", path, "", true); w.Code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("no rate limit after 13 checks")
	}
}
```

`f.service(t)` exists in `workload_test.go` (line ~250) and returns a service token. Check how the existing tests send a bearer request (`grep -n "Bearer" internal/api/workload_test.go`) and use that helper in place of `bearerRequest` if its name differs. Viewer: `CheckEndpointAccess(ContainerConfigure)` refuses a read-only member with 403; confirm against `TestWorkloadRoutesPermissionMatrix`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api -run '^TestWorkloadUpdateCheck'`
Expected: FAIL, 404/405 on the unregistered route.

- [ ] **Step 3: Implement.** `internal/api/workload_update_handlers.go`:

```go
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

type containerCheck struct {
	Name         string `json:"name"`
	Reference    string `json:"reference"`
	LocalDigest  string `json:"local_digest"`
	RemoteDigest string `json:"remote_digest"`
	Verdict      string `json:"verdict"`
	Detail       string `json:"detail"`
}

// podImage is one container as the workload's pods agree on it; agreed is false when they don't.
type podImage struct {
	name, image, imageID string
	agreed              bool
}

// workloadPodImages finds ref in the snapshot and reads its containers from its pods, in first
// seen order. Pods that disagree on a container's image or image ID mark it not agreed.
func workloadPodImages(snap protocol.Snapshot, ref protocol.WorkloadRef) (found, managed bool, images []podImage) {
	if snap.Kubernetes == nil {
		return false, false, nil
	}
	i := slices.IndexFunc(snap.Kubernetes.Workloads, func(w protocol.Workload) bool {
		return w.Namespace == ref.Namespace && w.Name == ref.Name && strings.EqualFold(w.Kind, ref.Kind)
	})
	if i < 0 {
		return false, false, nil
	}
	w := snap.Kubernetes.Workloads[i]
	if w.Application != "" || w.Instance != "" {
		return true, true, nil
	}
	index := map[string]int{}
	for _, p := range snap.Kubernetes.Pods {
		if p.Namespace != w.Namespace || p.OwnerKind != w.Kind || p.OwnerName != w.Name {
			continue
		}
		for _, c := range p.Containers {
			j, seen := index[c.Name]
			if !seen {
				index[c.Name] = len(images)
				images = append(images, podImage{c.Name, c.Image, c.ImageID, true})
				continue
			}
			if images[j].image != c.Image || images[j].imageID != c.ImageID {
				images[j].agreed = false
			}
		}
	}
	return true, false, images
}

// handleWorkloadUpdateCheck is the workload twin of handleContainerUpdateCheck: an explicit
// registry observation under the same gates, never an update approval. Containers are read from
// the stored inventory's pods; each tracked tag is Headed once.
func (s *Server) handleWorkloadUpdateCheck(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if a.ServiceTokenID != "" {
		_ = s.store.Tenancy().DenyService(r.Context(), a, permissions.ContainerConfigure, "bearer")
		s.tenantError(w, store.ErrForbidden)
		return
	}
	endpoint, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	ref, err := workloadRef(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.allowAttempt("image-check:"+a.Principal(), 12, time.Minute) {
		s.writeError(w, 429, "Too many update checks")
		return
	}
	if err := s.store.Tenancy().CheckEndpointAccess(r.Context(), a, permissions.ContainerConfigure, endpoint); err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.runtimeGate(w, r, a, endpoint, kubernetesRoute) {
		return
	}
	read := func() (bool, bool, []podImage, error) {
		inv, err := s.store.Tenancy().ReadInventory(r.Context(), a, endpoint)
		if err != nil {
			return false, false, nil, err
		}
		var snap protocol.Snapshot
		if err := json.Unmarshal(inv.Snapshot, &snap); err != nil {
			return false, false, nil, err
		}
		found, managed, images := workloadPodImages(snap, ref)
		return found, managed, images, nil
	}
	found, managed, images, err := read()
	if err == nil && !found {
		err = store.ErrNotFound
	}
	if err != nil {
		s.tenantError(w, err)
		return
	}
	out := struct {
		Workload   string           `json:"workload"`
		Verdict    string           `json:"verdict"`
		Detail     string           `json:"detail"`
		CheckedAt  string           `json:"checked_at"`
		Containers []containerCheck `json:"containers"`
	}{Workload: ref.String(), CheckedAt: time.Now().UTC().Format(time.RFC3339), Containers: []containerCheck{}}
	if managed {
		out.Verdict = "managed"
		s.writeJSON(w, 200, out)
		return
	}
	heads := map[string]error{}
	remotes := map[string]string{}
	acquired := false
	for _, im := range images {
		c := containerCheck{Name: im.name, Reference: im.image, Verdict: "unknown"}
		name, tracked, pinned, ok := trackedReference(im.image)
		switch {
		case !im.agreed:
		case !ok && pinned != "":
			c.Verdict = "pinned"
		case !ok:
		default:
			c.LocalDigest = runningDigest(im.imageID, tracked)
			if _, done := heads[name]; !done {
				if !acquired {
					// Policy before budget, as the container check does.
					if _, err := s.store.Tenancy().ResolveRegistryAccess(r.Context(), a, permissions.ImagePull, name, s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate); err != nil {
						s.tenantError(w, err)
						return
					}
					release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
					if !ok {
						return
					}
					defer release()
					extendRegistryDeadline(w)
					acquired = true
				}
				remote, err := s.headDigest(r.Context(), a, name, tracked)
				if err != nil && !errors.Is(err, errRegistryHead) {
					s.tenantError(w, err)
					return
				}
				heads[name], remotes[name] = err, remote
			}
			if err := heads[name]; err != nil {
				c.Verdict, c.Detail = "registry_error", registryDetail(err)
				break
			}
			c.RemoteDigest = remotes[name]
			switch {
			case c.LocalDigest == "":
			case c.LocalDigest == c.RemoteDigest:
				c.Verdict = "up_to_date"
			default:
				c.Verdict = "update_available"
			}
		}
		out.Containers = append(out.Containers, c)
	}
	out.Verdict, out.Detail = workloadVerdict(out.Containers)
	// The pods may have rolled while the registry answered.
	_, _, again, err := read()
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !slices.Equal(again, images) || !s.configurationAllowed(r, a, endpoint) {
		s.tenantError(w, store.ErrAdoptionChanged)
		return
	}
	s.writeJSON(w, 200, out)
}

// workloadVerdict folds container verdicts: any update wins, then any registry failure (with
// its detail), then all current, then all pinned or current; anything else is unknown.
func workloadVerdict(cs []containerCheck) (string, string) {
	if len(cs) == 0 {
		return "unknown", ""
	}
	for _, c := range cs {
		if c.Verdict == "update_available" {
			return c.Verdict, ""
		}
	}
	for _, c := range cs {
		if c.Verdict == "registry_error" {
			return c.Verdict, c.Detail
		}
	}
	if !slices.ContainsFunc(cs, func(c containerCheck) bool { return c.Verdict != "up_to_date" }) {
		return "up_to_date", ""
	}
	if !slices.ContainsFunc(cs, func(c containerCheck) bool { return c.Verdict != "up_to_date" && c.Verdict != "pinned" }) {
		return "pinned", ""
	}
	return "unknown", ""
}
```

Registry policy is resolved for the first tracked name only, which matches the Docker check (one reference). Later names that need other registries go through `headDigest`, which returns their policy error, and the handler reports it through `tenantError`. Both paths produce the same refusal.

Route, in `internal/api/server.go` after the `/apply` workload route:

```go
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/workloads/{namespace}/{kind}/{name}/updates/check", s.tenantRoute(s.handleWorkloadUpdateCheck))
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api -run 'Workload|ContainerUpdateCheck' && go vet ./internal/api`
Expected: PASS. `slices.Equal` on `[]podImage` compiles because every field of `podImage` is comparable.

- [ ] **Step 5: Commit**

```bash
git add internal/api/workload_update_handlers.go internal/api/server.go internal/api/workload_update_test.go
git commit -m "api: check standalone workload images for updates"
```

---

### Task 6: Web: shared update checker and the workload badge

Generalise the Docker checker so the workload badge reuses its queue, pacing, cache and verdict texts.

**Files:**
- Modify: `web/src/components/ContainerUpdate.tsx`
- Create: `web/src/components/WorkloadUpdate.tsx`
- Test: `web/src/components/WorkloadUpdate.test.tsx`; the existing `web/src/components/ContainerUpdate.test.tsx` is the regression test.

**Interfaces:**
- Produces from `ContainerUpdate.tsx`: `export const VERDICTS`, `export type Verdict`, `export function readVerdict(response: Response, matches: (body: object) => boolean): Promise<Verdict>`, `export function useUpdateQueue(): (key: string, url: string, matches: (body: object) => boolean, signal: AbortSignal, fresh: boolean) => Promise<Verdict | null>`, `export function UpdateBadge(props: { label: string; identity: string; active: boolean; org: string; check: (signal: AbortSignal, fresh: boolean) => Promise<Verdict | null>; onUpdate: () => void; updateDisabled?: boolean })`.
- Produces from `WorkloadUpdate.tsx`: `useWorkloadUpdateChecker(base: string): CheckWorkload`, `WorkloadUpdate({ workload, pods, active, org, checkUpdate, onUpdate, updateDisabled })`.
- `VERDICTS` gains `managed: 'Managed by an application'`.

- [ ] **Step 1: Write the failing workload test** `web/src/components/WorkloadUpdate.test.tsx`:

```tsx
import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { useWorkloadUpdateChecker, WorkloadUpdate } from './WorkloadUpdate';
import type { Pod, Workload } from '../tenant';
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });
const workload: Workload = { kind: 'Deployment', namespace: 'shop', name: 'web', desired: 1, ready: 1, updated: 1, images: ['ghcr.io/acme/web:2'], paused: false };
const pod: Pod = { namespace: 'shop', name: 'web-1', phase: 'Running', node: 'n', owner_kind: 'Deployment', owner_name: 'web', started_at: '', containers: [{ name: 'web', image: 'ghcr.io/acme/web:2', image_id: 'ghcr.io/acme/web@sha256:' + 'a'.repeat(64), state: 'running', reason: '', ready: true, restart_count: 0 }] };
function Row({ pods = [pod], onUpdate = () => {} }: { pods?: Pod[]; onUpdate?: () => void }) {
  const check = useWorkloadUpdateChecker('/api/c');
  return <WorkloadUpdate workload={workload} pods={pods} active org="a" checkUpdate={check} onUpdate={onUpdate} />;
}
const reply = (verdict: string, workloadName = 'shop/deployment/web') => new Response(JSON.stringify({ workload: workloadName, verdict, detail: '', containers: [] }));
it('checks the workload route and shows the verdict and the update button', async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn(async (_input: RequestInfo | URL) => reply('update_available'));
  vi.stubGlobal('fetch', fetcher);
  const onUpdate = vi.fn();
  render(<Row onUpdate={onUpdate} />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(String(fetcher.mock.calls[0]?.[0])).toBe('/api/c/workloads/shop/deployment/web/updates/check');
  expect(screen.getByText('Update available')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Update image' }));
  expect(onUpdate).toHaveBeenCalledTimes(1);
});
it('shows managed without an update button, and refuses an answer for another workload', async () => {
  vi.useFakeTimers();
  vi.stubGlobal('fetch', vi.fn(async () => reply('managed')));
  const view = render(<Row />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(screen.getByText('Managed by an application')).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Update image' })).toBeNull();
  view.unmount();
  vi.stubGlobal('fetch', vi.fn(async () => reply('up_to_date', 'shop/deployment/api')));
  render(<Row pods={[{ ...pod, name: 'web-2' }]} />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(screen.getByText('Update check failed')).toBeTruthy();
});
it('re-checks when the pods change images', async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn(async () => reply('up_to_date'));
  vi.stubGlobal('fetch', fetcher);
  const view = render(<Row />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  view.rerender(<Row pods={[{ ...pod, containers: [{ ...pod.containers[0]!, image_id: 'ghcr.io/acme/web@sha256:' + 'b'.repeat(64) }] }]} />);
  await act(() => vi.advanceTimersByTimeAsync(5100));
  expect(fetcher).toHaveBeenCalledTimes(2);
});
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd web && npx vitest run src/components/WorkloadUpdate.test.tsx`
Expected: FAIL, module `./WorkloadUpdate` not found.

- [ ] **Step 3: Refactor `ContainerUpdate.tsx`.** Keep `pause`, `CHECK_INTERVAL` and `CACHE_LIFETIME`, and replace the rest of the file with:

```tsx
export const VERDICTS = {
  up_to_date: 'Up to date', update_available: 'Update available', pinned: 'Digest pinned', unknown: 'Image version unknown',
  registry_error: 'Registry check failed', check_failed: 'Update check failed', forbidden: 'Update check not permitted',
  rate_limited: 'Update check limit reached', registry_policy: 'Registry access not configured',
  registry_unauthorized: 'Registry authentication required', registry_not_found: 'Image not found in registry',
  registry_rate_limited: 'Registry rate limit reached', registry_private: 'Registry address blocked', registry_unavailable: 'Registry unavailable',
  managed: 'Managed by an application',
};
export type Verdict = keyof typeof VERDICTS;
type CheckUpdate = (container: Container, signal: AbortSignal, fresh: boolean) => Promise<Verdict | null>;

const DETAILS: Record<string, Verdict> = { unauthorized: 'registry_unauthorized', not_found: 'registry_not_found', rate_limited: 'registry_rate_limited', private_destination: 'registry_private', unavailable: 'registry_unavailable' };
// readVerdict maps a check answer to a fixed verdict; matches proves the answer is for this row.
export async function readVerdict(response: Response, matches: (body: object) => boolean): Promise<Verdict> {
  if (response.status === 401 || response.status === 403) return 'forbidden';
  if (response.status === 429) return 'rate_limited';
  const body: unknown = await response.json();
  if (!body || typeof body !== 'object') return 'check_failed';
  if (response.ok && matches(body) && 'verdict' in body) {
    switch (body.verdict) {
      case 'up_to_date': case 'update_available': case 'pinned': case 'unknown': case 'managed': return body.verdict;
      case 'registry_error': return 'detail' in body && typeof body.detail === 'string' && Object.hasOwn(DETAILS, body.detail) ? DETAILS[body.detail] ?? 'registry_error' : 'registry_error';
    }
    return 'check_failed';
  }
  if ('code' in body && (body.code === 'registry_not_configured' || body.code === 'anonymous_pull_disabled')) return 'registry_policy';
  return 'check_failed';
}

// One queue per page, shared by its visible rows. Cached answers survive pagination, filter and
// tab changes; identity changes (part of key) and leaving the page invalidate them.
export function useUpdateQueue() {
  const queue = useRef(Promise.resolve());
  const next = useRef(0);
  const cache = useRef(new Map<string, { verdict: Verdict; expires: number }>());
  return useCallback((key: string, url: string, matches: (body: object) => boolean, signal: AbortSignal, fresh: boolean) => {
    const task = queue.current.then(async (): Promise<Verdict | null> => {
      if (signal.aborted) return null;
      const saved = cache.current.get(key);
      if (!fresh && saved && saved.expires > Date.now()) return saved.verdict;
      await pause(Math.max(0, next.current - Date.now()), signal);
      if (signal.aborted) return null;
      next.current = Date.now() + CHECK_INTERVAL;
      let verdict: Verdict = 'check_failed';
      try {
        verdict = await readVerdict(await secureFetch(url, { method: 'POST', signal: AbortSignal.any([signal, AbortSignal.timeout(30_000)]) }), matches);
      } catch { /* Fixed failure text only; an aborted request publishes nothing. */ }
      if (signal.aborted) return null;
      if (cache.current.size >= 1000) cache.current.clear();
      cache.current.set(key, { verdict, expires: Date.now() + CACHE_LIFETIME });
      return verdict;
    });
    queue.current = task.then(() => {});
    return task;
  }, []);
}

export function useContainerUpdateChecker(base: string): CheckUpdate {
  const run = useUpdateQueue();
  return useCallback((container, signal, fresh) => run(
    JSON.stringify([base, container.id, container.image_id, container.image, container.created_at]),
    `${base}/containers/${encodeURIComponent(container.id)}/updates/check`,
    (body) => 'image_id' in body && body.image_id === container.image_id, signal, fresh,
  ), [base, run]);
}

// UpdateBadge checks when identity changes or on demand and offers the update; a managed answer
// offers none, because the application's own flow owns those images.
export function UpdateBadge({ label, identity, active, org, check: checkUpdate, onUpdate, updateDisabled = false }: { label: string; identity: string; active: boolean; org: string; check: (signal: AbortSignal, fresh: boolean) => Promise<Verdict | null>; onUpdate: () => void; updateDisabled?: boolean }) {
  const [verdict, setVerdict] = useState<Verdict | null>(null);
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  const latest = useRef(checkUpdate);
  latest.current = checkUpdate;
  const check = useCallback(async (fresh: boolean) => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    const result = await latest.current(controller.signal, fresh);
    if (!controller.signal.aborted) { setVerdict(result); setBusy(false); }
  }, [identity]);
  useEffect(() => {
    setVerdict(null);
    if (active) void check(false);
    return () => request.current?.abort();
  }, [active, check]);
  const text = busy ? 'Checking…' : verdict ? VERDICTS[verdict] : 'Endpoint offline';
  return <span className="ky-image-update"><span role="status" className={`badge ${verdict === 'update_available' ? 'badge-warning' : 'badge-secondary'}`}>{text}</span>{verdict === 'registry_policy' && <Link to={`/organizations/${encodeURIComponent(org)}/members#registries-heading`} title="Configure this registry in Members → Registries.">Configure registry access</Link>}<button type="button" className="btn-secondary ky-icon-button" aria-label={`Check image update for ${label}`} title="Check image update" disabled={!active || busy} onClick={() => void check(true)}><RefreshCw size={15} /></button>{active && verdict !== 'managed' && <button type="button" className="btn-secondary" disabled={updateDisabled} onClick={onUpdate} title="Pull the latest image and update using current settings">Update image</button>}</span>;
}

export function ContainerUpdate({ container, active, org, checkUpdate, onUpdate, updateDisabled = false }: { container: Container; active: boolean; org: string; endpoint: string; checkUpdate: CheckUpdate; onUpdate: () => void; updateDisabled?: boolean }) {
  return <UpdateBadge label={container.name} identity={JSON.stringify([container.id, container.image_id, container.image, container.created_at])} active={active} org={org}
    check={(signal, fresh) => checkUpdate(container, signal, fresh)} onUpdate={onUpdate} updateDisabled={updateDisabled} />;
}
```

The button's `title` changes from "…recreate using current settings" to "…update using current settings". If `ContainerUpdate.test.tsx` asserts that title, keep the old text for containers by passing `title` through as a prop instead.

`WorkloadUpdate.tsx`:

```tsx
import { useCallback } from 'react';
import type { Pod, Workload } from '../tenant';
import { UpdateBadge, useUpdateQueue, type Verdict } from './ContainerUpdate';

type CheckWorkload = (workload: Workload, pods: Pod[], signal: AbortSignal, fresh: boolean) => Promise<Verdict | null>;
const ref = (w: Workload) => `${w.namespace}/${w.kind.toLowerCase()}/${w.name}`;
// The images the pods run are the workload's identity: a rollout invalidates the cached answer.
const identity = (w: Workload, pods: Pod[]) => JSON.stringify([ref(w), w.images, pods.map((p) => [p.name, p.containers.map((c) => [c.image, c.image_id])])]);

export function useWorkloadUpdateChecker(base: string): CheckWorkload {
  const run = useUpdateQueue();
  return useCallback((w, pods, signal, fresh) => run(
    JSON.stringify([base, identity(w, pods)]),
    `${base}/workloads/${encodeURIComponent(w.namespace)}/${encodeURIComponent(w.kind.toLowerCase())}/${encodeURIComponent(w.name)}/updates/check`,
    (body) => 'workload' in body && body.workload === ref(w), signal, fresh,
  ), [base, run]);
}

export function WorkloadUpdate({ workload, pods, active, org, checkUpdate, onUpdate, updateDisabled = false }: { workload: Workload; pods: Pod[]; active: boolean; org: string; checkUpdate: CheckWorkload; onUpdate: () => void; updateDisabled?: boolean }) {
  return <UpdateBadge label={`${workload.namespace}/${workload.name}`} identity={identity(workload, pods)} active={active} org={org}
    check={(signal, fresh) => checkUpdate(workload, pods, signal, fresh)} onUpdate={onUpdate} updateDisabled={updateDisabled} />;
}
```

- [ ] **Step 4: Run tests**

Run: `cd web && npx vitest run src/components/WorkloadUpdate.test.tsx src/components/ContainerUpdate.test.tsx src/pages/EndpointPage.test.tsx src/pages/ContainerPage.test.tsx && npx tsc -b`
Expected: PASS, no type errors.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/ContainerUpdate.tsx web/src/components/WorkloadUpdate.tsx web/src/components/WorkloadUpdate.test.tsx
git commit -m "web: share the update checker and add the workload badge"
```

---

### Task 7: Web: one-click *Update image* for workloads

**Files:**
- Modify: `web/src/components/workloadConfiguration.ts` (add `tracksTag`)
- Create: `web/src/components/useWorkloadImageUpdate.ts`
- Modify: `web/src/components/KubernetesCluster.tsx` (Workloads table)
- Modify: `web/src/pages/WorkloadPage.tsx` (heading)
- Test: `web/src/components/useWorkloadImageUpdate.test.tsx`, `web/src/components/workloadConfiguration.test.ts` (append)

**Interfaces:**
- Consumes: `useWorkloadUpdateChecker`, `WorkloadUpdate` (Task 6); `workloadURL` from `WorkloadConfigurationForm`; `parseWorkloadConfiguration`, `toWorkloadSpec`.
- Produces:
  - `export function tracksTag(image: string): boolean`
  - `export function useWorkloadImageUpdate(base: string, onSent: (command: DirectCommand, workload: Workload) => void): { update: (w: Workload) => Promise<void>; busy: boolean; error: string; lost: boolean }`

- [ ] **Step 1: Write the failing tests.** Append to `workloadConfiguration.test.ts`:

```ts
import { tracksTag } from './workloadConfiguration';
it('knows which images track a tag', () => {
  const d = 'sha256:' + 'a'.repeat(64);
  expect(tracksTag('nginx')).toBe(true);
  expect(tracksTag('ghcr.io/acme/web:2')).toBe(true);
  expect(tracksTag(`ghcr.io/acme/web:2@${d}`)).toBe(true);
  expect(tracksTag(`localhost:5000/web@${d}`)).toBe(false);
  expect(tracksTag(`ghcr.io/acme/web@${d}`)).toBe(false);
});
```

Create `useWorkloadImageUpdate.test.tsx`:

```tsx
import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, renderHook } from '@testing-library/react';
import { useWorkloadImageUpdate } from './useWorkloadImageUpdate';
import type { Workload } from '../tenant';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const workload: Workload = { kind: 'Deployment', namespace: 'shop', name: 'web', desired: 1, ready: 1, updated: 1, images: [], paused: false };
const d = 'sha256:' + 'a'.repeat(64);
const container = (name: string, image: string) => ({ name, image, image_id: '', command: [], args: [], env: [], resources: { cpu_request: '', cpu_limit: '', memory_request: '', memory_limit: '' } });
const read = (unsupported: string[] = []) => ({ target: { namespace: 'shop', kind: 'deployment', name: 'web' }, observed_at: new Date().toISOString(), resource_version: '42', replicas: 1, paused: false, strategy: 'RollingUpdate',
  containers: [container('web', 'ghcr.io/acme/web:2'), container('pinned', `ghcr.io/acme/side@${d}`)], init_containers: [container('init', 'busybox:1')], env_from: [], managed: false, unsupported });
it('applies the read configuration with every tag-tracking container pulled', async () => {
  const calls: [string, RequestInit | undefined][] = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    calls.push([url, init]);
    return url.endsWith('/configuration') ? new Response(JSON.stringify(read())) : new Response(JSON.stringify({ id: 'cmd-1', action: 'workload.apply', outcome: '' }), { status: 202 });
  }));
  const onSent = vi.fn();
  const { result } = renderHook(() => useWorkloadImageUpdate('/api/c', onSent));
  await act(() => result.current.update(workload));
  const body = JSON.parse(String(calls[1]?.[1]?.body));
  expect(calls[1]?.[0]).toBe('/api/c/workloads/shop/deployment/web/apply');
  expect(body.pull).toEqual(['web', 'init']);
  expect(body.resource_version).toBe('42');
  expect(body.confirm).toBe('web');
  expect(body.spec.containers[0].image).toBe('ghcr.io/acme/web:2');
  expect(onSent).toHaveBeenCalledWith(expect.objectContaining({ id: 'cmd-1' }), workload);
});
it('refuses unsupported settings and locks after a lost response', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(read(['env_field_ref'])))));
  const { result } = renderHook(() => useWorkloadImageUpdate('/api/c', () => {}));
  await act(() => result.current.update(workload));
  expect(result.current.error).toContain('Cannot update without losing settings');
  vi.stubGlobal('fetch', vi.fn(async (url: string) => { if (url.endsWith('/configuration')) return new Response(JSON.stringify(read())); throw new TypeError('lost'); }));
  await act(() => result.current.update(workload));
  expect(result.current.lost).toBe(true);
  const fetcher = vi.fn();
  vi.stubGlobal('fetch', fetcher);
  await act(() => result.current.update(workload));
  expect(fetcher).not.toHaveBeenCalled();
});
```

`secureFetch` adds CSRF headers and calls `fetch`; confirm in `web/src/api.ts` that the stub sees `(url, init)`.

- [ ] **Step 2: Run to verify they fail**

Run: `cd web && npx vitest run src/components/useWorkloadImageUpdate.test.tsx src/components/workloadConfiguration.test.ts`
Expected: FAIL, missing exports.

- [ ] **Step 3: Implement.** Append to `workloadConfiguration.ts`:

```ts
// tracksTag is false for a digest-only image: there is no tag to resolve again (server trackedReference).
export function tracksTag(image: string): boolean {
  const [name = '', pinned] = image.split('@', 2);
  return pinned === undefined || name.slice(name.lastIndexOf('/') + 1).includes(':');
}
```

`useWorkloadImageUpdate.ts`, modelled on `useContainerImageUpdate.ts`:

```ts
import { useEffect, useRef, useState } from 'react';
import { secureFetch } from '../api';
import type { DirectCommand, Workload } from '../tenant';
import { parseWorkloadConfiguration, toWorkloadSpec, tracksTag, workloadUnsupportedLabel } from './workloadConfiguration';
import { workloadURL } from './WorkloadConfigurationForm';
import { workloadRefusal } from './workloadTexts';

// Owned by the page, so filtering a row or changing tabs cannot unlock a pending update.
export function useWorkloadImageUpdate(base: string, onSent: (command: DirectCommand, workload: Workload) => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [lost, setLost] = useState(false);
  const locked = useRef(false);
  const read = useRef<AbortController | null>(null);
  const generation = useRef(0);
  useEffect(() => {
    generation.current++;
    locked.current = false; setBusy(false); setLost(false); setError('');
    return () => { generation.current++; read.current?.abort(); };
  }, [base]);

  const update = async (w: Workload) => {
    if (locked.current) return;
    locked.current = true;
    setBusy(true); setError('');
    const scope = generation.current;
    const current = () => scope === generation.current;
    let submitted = false, uncertain = false;
    const controller = new AbortController();
    read.current = controller;
    const target = { namespace: w.namespace, kind: w.kind.toLowerCase(), name: w.name };
    try {
      const url = workloadURL(base, target);
      const response = await fetch(`${url}/configuration`, { cache: 'no-store', signal: controller.signal });
      if (controller.signal.aborted || !current()) return;
      if (!response.ok) { setError(await workloadRefusal(response)); return; }
      const config = parseWorkloadConfiguration(await response.json(), target);
      if (controller.signal.aborted || !current()) return;
      if (!config) { setError('The configuration did not match this workload. Refresh the inventory.'); return; }
      if (config.unsupported.length) { setError(`Cannot update without losing settings: ${config.unsupported.map(workloadUnsupportedLabel).join('; ')}.`); return; }
      const spec = toWorkloadSpec(config);
      const pull = [...spec.containers, ...spec.init_containers].filter((c) => tracksTag(c.image)).map((c) => c.name);
      if (!pull.length) { setError('Every image is pinned by digest only; there is no tag to update from.'); return; }
      read.current = null;
      submitted = true;
      const result = await secureFetch(`${url}/apply`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ resource_version: spec.resource_version, spec, confirm: w.name, pull }) });
      if (!result.ok) { if (current()) setError(await workloadRefusal(result)); return; }
      const command: unknown = await result.json();
      if (!command || typeof command !== 'object' || !('id' in command) || typeof command.id !== 'string' || !command.id) throw new Error('Invalid command receipt');
      onSent({ id: command.id, action: 'workload.apply', outcome: '' }, w);
    } catch {
      uncertain = submitted;
      if (current() && !controller.signal.aborted) {
        setLost(uncertain);
        setError(uncertain ? "Connection lost. The update may have been sent. Check the workload's Activity tab before trying again." : 'Could not read the current settings. No update was sent. Try again.');
      }
    } finally {
      if (current()) { locked.current = uncertain; setBusy(false); }
    }
  };
  return { update, busy, error, lost };
}
```

`workloadRefusal` must map `image_unresolved` and the registry policy codes. In `workloadTexts.ts`:
- add to `blockerText`: `if (code === 'image_unresolved') return 'The registry did not resolve the image. Check the reference and the registry access.';`
- add to `CONFLICTS`: `registry_not_configured: 'Registry access is not configured for this image. Configure it in Members → Registries.'` and `anonymous_pull_disabled: 'Anonymous pulls are off and no registry is configured for this image. Configure it in Members → Registries.'`

Wiring:

`KubernetesCluster.tsx`: add the props `checkUpdate?: CheckWorkload` and `imageUpdate?: ReturnType<typeof useWorkloadImageUpdate>`, which the page passes in. In the Workloads table, add an `'Image'` column after `Images` when `checkUpdate && canConfigure(role) && endpoint.capabilities.includes('kubernetes.workloads')`:

```tsx
Object.hasOwn(PAGE_KINDS, w.kind) && <WorkloadUpdate key={`${w.kind}/${w.namespace}/${w.name}`} workload={w} pods={inventory.pods.filter((p) => p.namespace === w.namespace && p.owner_kind === w.kind && p.owner_name === w.name)} active={active} org={org} checkUpdate={checkUpdate} onUpdate={() => void imageUpdate?.update(w)} updateDisabled={!imageUpdate || imageUpdate.busy || imageUpdate.lost} />
```

Render `imageUpdate.error` under the table as `<p role="alert" className="dr-alert dr-alert-error">`.

In the page that renders `KubernetesCluster` (`web/src/pages/EndpointPage.tsx`): create `const checkWorkload = useWorkloadUpdateChecker(base);` and `const workloadUpdate = useWorkloadImageUpdate(base, (cmd, w) => onStatus(...))`. Follow how `EndpointPage.tsx` wires `useContainerImageUpdate`'s `onSent` for containers (`grep -n "useContainerImageUpdate" -A6 web/src/pages/EndpointPage.tsx`) and mirror it, reporting through the same status line used for workload controls.

`WorkloadPage.tsx`: in the heading, beside `WorkloadControls`, render for `canConfigure(role) && caps.includes('kubernetes.workloads') && !w.application`:

```tsx
<WorkloadUpdate workload={w} pods={pods} active={active} org={org} checkUpdate={checkWorkload} onUpdate={() => void imageUpdate.update(w)} updateDisabled={imageUpdate.busy || imageUpdate.lost || (!!command && !command.outcome)} />
```

with `const checkWorkload = useWorkloadUpdateChecker(base);` and `const imageUpdate = useWorkloadImageUpdate(base, (cmd) => setSent(cmd));`. The page already polls `sent` and shows the result in its "Last change" panel. Show `imageUpdate.error` beside `status`.

- [ ] **Step 4: Run tests**

Run: `cd web && npx vitest run && npx tsc -b`
Expected: PASS. Fix any existing page tests that count fetch calls, because the badge now issues a check POST on render. Stub the `/updates/check` URL in those tests the same way `EndpointPage.test.tsx` stubs container checks.

- [ ] **Step 5: Commit**

```bash
git add web/src
git commit -m "web: update standalone workload images in one click"
```

---

### Task 8: Web: pin checkbox and running digest in the workload form

**Files:**
- Modify: `web/src/components/WorkloadConfigurationForm.tsx`
- Test: `web/src/components/WorkloadConfigurationForm.test.tsx` (append)

**Interfaces:**
- Consumes: `tracksTag` (Task 7).
- Produces: apply/run bodies carry `pull` (the ticked container names).

- [ ] **Step 1: Write the failing tests.** Append, reusing the file's existing render helpers and fixtures (open the file and use its `initial` fixture and its fetch-capture pattern; the names below are the ones to look for):

```tsx
it('pins a changed image by default and shows the running digest', async () => {
  const sent = captureApply(); // existing helper or: vi.stubGlobal('fetch', vi.fn(async (_u, init) => { bodies.push(JSON.parse(init.body)); return new Response(JSON.stringify({ id: 'c', action: 'workload.apply', outcome: '' }), { status: 202 }); }))
  const config = { ...initial, containers: [{ ...initial.containers[0]!, image_id: 'ghcr.io/acme/web@sha256:' + 'a'.repeat(64) }] };
  render(<WorkloadConfigurationForm base="/api/c" initial={config} onSent={() => {}} />);
  expect(screen.getByText(/sha256:a{64}/)).toBeTruthy();
  const box = screen.getByRole('checkbox', { name: `Pin the current digest of ${config.containers[0]!.name}` }) as HTMLInputElement;
  expect(box.checked).toBe(false);
  fireEvent.change(screen.getByLabelText(`Image of ${config.containers[0]!.name}`), { target: { value: 'ghcr.io/acme/web:3' } });
  expect(box.checked).toBe(true);
  fireEvent.change(screen.getByLabelText(`Type the workload name ${config.target.name} to confirm`), { target: { value: config.target.name } });
  fireEvent.click(screen.getByRole('button', { name: 'Save and apply' }));
  await waitFor(() => expect(sent()[0]?.pull).toEqual([config.containers[0]!.name]));
});
it('starts unticked in run mode and sends no pull', async () => {
  // Render in run mode with one granted namespace, fill name and image, submit, assert body.pull is [].
});
it('hides the box for a digest-only image', () => {
  const config = { ...initial, containers: [{ ...initial.containers[0]!, image: 'ghcr.io/acme/web@sha256:' + 'a'.repeat(64) }] };
  render(<WorkloadConfigurationForm base="/api/c" initial={config} onSent={() => {}} />);
  expect(screen.queryByRole('checkbox', { name: /Pin the current digest/ })).toBeNull();
});
```

Write the run-mode test in full, using the file's existing run-mode test as the template: copy its render and fill steps, then assert `body.pull` equals `[]`. If `captureApply` does not exist, inline the `bodies` array shown in the comment.

- [ ] **Step 2: Run to verify they fail**

Run: `cd web && npx vitest run src/components/WorkloadConfigurationForm.test.tsx`
Expected: FAIL, no checkbox.

- [ ] **Step 3: Implement** in `WorkloadConfigurationForm`:

1. State: `const [pins, setPins] = useState<ReadonlySet<string>>(new Set());`. It is keyed by container index in run mode and by name when editing; use the same `who` key the render uses (`run ? String(i) : c.name`).
2. In `setContainer`, when the patch contains `image` and not in run mode, tick the box iff the new image differs from the read image:

```tsx
const setContainer = (i: number, patch: Partial<WorkloadContainer>) => {
  setDraft((d) => ({ ...d, containers: d.containers.map((c, j) => j === i ? { ...c, ...patch } : c) }));
  if (!run && patch.image !== undefined) {
    const name = draft.containers[i]?.name ?? '';
    const was = start.containers.find((c) => c.name === name)?.image;
    setPins((p) => { const n = new Set(p); if (patch.image !== was) n.add(name); else n.delete(name); return n; });
  }
};
```

3. Under each container's image input:

```tsx
{!run && c.image_id && <p>Running <code style={{ overflowWrap: 'anywhere' }}>{c.image_id}</code></p>}
{tracksTag(c.image) && <label><input type="checkbox" aria-label={`Pin the current digest of ${who}`} checked={pins.has(run ? String(i) : c.name)} onChange={(e) => { const key = run ? String(i) : c.name; setPins((p) => { const n = new Set(p); if (e.target.checked) n.add(key); else n.delete(key); return n; }); }} /> Pin the reference's current digest</label>}
```

4. In `submit`, compute `pull` from `pins`: names in edit mode; in run mode map indexes to `spec.containers[i].name`. Keep only containers whose image still `tracksTag`. Add `pull` to both bodies.
5. The save note stays, with one sentence added: `A ticked image is resolved at the registry and pinned by digest; the tag stays visible.`
6. `ready` already requires changes in edit mode. A pin with an unchanged image is a change too: change the check to `(run ? … : changes.length > 0 || pull.length > 0)`.

- [ ] **Step 4: Run tests**

Run: `cd web && npx vitest run && npx tsc -b`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/WorkloadConfigurationForm.tsx web/src/components/WorkloadConfigurationForm.test.tsx
git commit -m "web: pin workload images from the configuration form"
```

---

### Task 9: Real-cluster proof, docs, build, CI

**Files:**
- Modify: `internal/runtime/kubernetes/cluster_test.go` (end of `TestManifestOnARealCluster`)
- Modify: `internal/api/AGENTS.md`, `internal/store/AGENTS.md`, `web/AGENTS.md`, `README.md` (update checks paragraph), `UI-VERIFICATION.md`
- Rebuild: `web/dist`

- [ ] **Step 1: Real-cluster test.** Append to the end of `TestManifestOnARealCluster`, before its closing brace:

```go
	// A standalone run written as tag@digest, as the API's pin writes it, pulls by digest: the
	// pod reports that digest.
	name, digest, _ := strings.Cut(image, "@")
	pinnedImage := name + ":kyyard-pin@" + digest
	one := int32(1)
	run := protocol.WorkloadApply{Request: "6c5d4e3f-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: "ep_kind", IssuedAt: time.Now(), Deadline: time.Now().Add(2 * time.Minute),
		Target: protocol.WorkloadRef{Namespace: deployNamespace, Kind: protocol.WorkloadDeployment, Name: "kind-pinned"}, Create: true}
	run.Spec = protocol.WorkloadConfiguration{Target: run.Target, Replicas: &one, Strategy: "RollingUpdate",
		Containers: []protocol.WorkloadContainer{{Name: "pinned", Image: pinnedImage, Command: []string{}, Args: []string{}, Env: []protocol.WorkloadEnv{}}}, InitContainers: []protocol.WorkloadContainer{}, EnvFrom: []string{}}
	if err := run.Validate(time.Now()); err != nil {
		t.Fatalf("run frame: %v", err)
	}
	if res := c.ApplyWorkload(ctx, run, func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("pinned run as the agent: %+v", res)
	}
	t.Cleanup(func() { _ = cs.AppsV1().Deployments(deployNamespace).Delete(context.Background(), "kind-pinned", metav1.DeleteOptions{}) })
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
		pods, err := cs.CoreV1().Pods(deployNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		found := slices.ContainsFunc(pods.Items, func(p corev1.Pod) bool {
			return strings.HasPrefix(p.Name, "kind-pinned-") && len(p.Status.ContainerStatuses) == 1 && p.Status.ContainerStatuses[0].Ready && strings.HasSuffix(p.Status.ContainerStatuses[0].ImageID, "@"+digest)
		})
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ready kind-pinned pod reporting %s", digest)
		}
	}
```

Check the `WorkloadConfiguration` fields the create path needs against `TestWorkloadRunCommand`'s `testRun()` in the store; match its field set if `Validate` refuses.

Run: `go vet ./internal/runtime/kubernetes && go test ./internal/runtime/kubernetes/...`
Expected: PASS. The real-cluster test skips without `KY_TEST_KUBECONFIG`. If a disposable kind cluster is available locally, also run `KY_TEST_KUBECONFIG=<path> KY_TEST_DEPLOY_IMAGE=registry.k8s.io/pause@sha256:<digest> go test ./internal/runtime/kubernetes -run TestManifestOnARealCluster -v` and record the result in the PR. If no cluster is available, say in the PR that this test was not run.

- [ ] **Step 2: DOX pass.** Update each owning doc in the existing style:
  - `internal/api/AGENTS.md`: the workload apply/run `pull` field and `pinWorkloadImages` (preconditions via `CheckWorkloadFrame` first, one `Head` per reference, `image_unresolved`); `handleWorkloadUpdateCheck` (gates, verdicts, `managed`, `trackedReference`/`runningDigest`, the snapshot re-read); `headDigest`/`registryDetail` as the shared registry rule for both checks and the pin.
  - `internal/store/AGENTS.md`: `CheckWorkloadFrame` beside the `CreateWorkloadApply`/`CreateWorkloadRun` bullet.
  - `web/AGENTS.md`: `UpdateBadge`/`useUpdateQueue` shared by container and workload badges; `useWorkloadImageUpdate`; the form's pin box (edit: ticked on reference change; run: unticked).
  - `README.md`: in the paragraph that describes container update checks (`grep -n "update" README.md`), add that cluster workloads outside applications get the same check and one-click update, and that private images still need the namespace `imagePullSecret`.
  - `UI-VERIFICATION.md`: add the workload badge, *Update image*, and the form checkbox in the same format as the container entries added by PR 99.

- [ ] **Step 3: Rebuild the embedded web bundle**

Run: `cd web && npm run build`
Expected: success; `git status web/dist` shows new hashed assets.

- [ ] **Step 4: Full verification**

Run: `make ci`
Expected: every target passes (`tidy-check lint test-race test-web smoke`). Paste the tail of the output into the PR.

- [ ] **Step 5: Commit**

```bash
git add -A internal docs web README.md UI-VERIFICATION.md
git commit -m "docs: record workload image updates; rebuild web"
```
