# M8 Hygiene Follow-up (items that depended on #76) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the review minors the M8 hygiene plan deferred until PR 22 (#76) merged: name an unbound claim in a rollout timeout, pin the claim error paths, list the claims a cluster removal kept, name a migration's destination without colliding, refuse a stale destination inventory and an overlong destination name, and check migration state and permission before any inspection is spent. No new feature, no schema change.

**Architecture:** Five tasks by area. Task 1 hardens `internal/runtime/kubernetes` (the `claim=<Phase>` rollout reason, four claim/StorageClass error-path tests, the claim-cap test) and fixes `protocol.truncatable`, which dropped `storage_classes`. Task 2 changes `internal/store` (`Deployment.RetainedClaims` derived on read, destination naming with `(n)`, `ErrDestinationNameTooLong`, `ErrInventoryStale`, `CheckMigrationChoices`, `ReadOpenMigration`) and maps the two new codes in `internal/api/tenant_handlers.go`. Task 3 reorders `internal/api/migration_handlers.go` (state, choices and permission before inspection). Task 4 adds the web sentences, the "Kept on the cluster" list and rebuilds `web/dist`. Task 5 updates the operator documents and runs the gate.

**Tech Stack:** Go 1.26 (module `go 1.26.6`), `k8s.io/client-go` v0.37.1 fake clientset, SQLite + PostgreSQL 17, React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-26-m8-hygiene-followup-design.md`

## Global Constraints

- Work in the worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/hygiene2` (branch `chore/m8-hygiene-followup`, stacked on `chore/m8-review-hygiene` bac5ffb, which sits on `master` 271e30b with PR 76 merged; the spec is 14164b8). Every command below runs from its root. The PR is retargeted to `master` once #77 merges.
- Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Gate each commit on the previous command's exit (`&&`).
- `gofmt -w` every edited Go file and check `gofmt -l cmd internal` prints nothing before each commit. Never put two single quotes or two backticks in a row in a Go comment. `make tidy-check lint` runs after `git add` (it diffs the working tree against the index) and before `git commit`.
- Memory: this machine has been killed by memory pressure. During Tasks 1–4 run only the focused tests each step names (`go test -count=1 -run <Names> ./one/package/`, `npx vitest run --maxWorkers=1 <one file>`), one test process at a time, never `-race` on `./...`, never `make ci`. The controller runs the full gate (Task 5) alone.
- `web/dist` is embedded and committed: Task 4 rebuilds it with `make build-web` and commits it with `web/tsconfig.tsbuildinfo`, with nothing else running.
- DOX: the task that changes a directory's contract updates that directory's `AGENTS.md` in the same commit.
- The server links no client-go: `go list -deps ./cmd/server | grep -c k8s.io` prints `0`.
- Every store change is tested on SQLite and PostgreSQL. PostgreSQL runs use the local container `kyyard-access-pg`; read the password into a variable, never print it: `PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test -count=1 -p 1 -run <Name> ./internal/store/`. Below this prefix is written `PG=… go test -count=1 -p 1 -run <Name> ./internal/store/`.
- No schema migration (the latest registered stays as on the base branch): `git diff --quiet bac5ffb -- internal/store/migrations` must hold at the end.
- Every new closed code has a web sentence in the task that introduces it, and the vocabulary fixtures stay in sync (`web/src/protocol-codes.json` and `web/src/migration-codes.json` do not change: no step, unsupported or analyzer code is added).
- Wire: the only protocol change is that a `rollout_timeout` detail may carry `claim=Pending` or `claim=Lost`. A new server accepts it before any agent sends it; a new agent's detail is refused by an older server, which the documented upgrade order (server first, README "Upgrade in this order") already covers.

Plan decisions where the spec's wording left a choice or disagreed with the code (each is reported to the reviewer; none widens scope):

1. **Retained claim names.** The spec says the step list carries the claims; it does not. A retained step is `{service, volume, skipped, retained}` with no claim name, and adding one would be a wire change an older server refuses. `Deployment.RetainedClaims` is derived on read from the removal plan's `PlannedService.ClaimMounts` for each service that reported retained steps, and names that service's planned claims only when the agent reported exactly as many retained steps as the plan mounts for it: any other count cannot say which claims exist, so it names none. The page lists the names under "Kept on the cluster" and, when the agent reported more kept claims than are named, adds the count and the `kubectl` label selector that lists them all.
2. **`inventory_stale`.** The spec calls it an existing code; no such code exists (the plan's stale inventory is `adoption_changed`, whose migration sentence names the source host). Task 2 adds `ErrInventoryStale`, 409 `inventory_stale`, with its own sentence. The freshness rule is the plan's (`freshInventory`: received within 3 minutes, observed within 5 minutes either side). It is enforced once, in `migrationDestination`, so start, analysis, choices and destination creation all read the destination under the same rule.
3. **Destination name at analysis.** `migrationDestination` picks the first free name (`<source> on <endpoint>`, then ` (2)` to ` (9)`) whenever it is given the source's name, so the analysis report's destination DNS names and the created destination agree, and it refuses at analysis already: a candidate over 255 bytes (`validTenantName`'s bound) is `ErrDestinationNameTooLong`, all nine held `ErrApplicationNameTaken` (the spec's "tenth attempt"). If another administrator takes exactly that name between analysis and creation, the destination takes the next free name and the report's DNS names are stale until "Analyze again"; accepted, not guarded.
4. **Quota 403 on a claim create.** Already pinned by `TestDeployClaimCreateRefusal` ("admission": the grant allows `create persistentvolumeclaims`, the write answers 403, the step is `admission_denied PersistentVolumeClaim/shop-web-data`). Not duplicated.
5. **`storage_classes` in `Truncated`.** Writing the forbidden-list test found that `protocol.Clamp` drops `storage_classes` from `Truncated` (it is missing from `truncatable`), on the agent and again on the server's copy, so the plan's "a cut list cannot prove absence" exemption (`storageClassBlockers`) never fired. Task 1 adds it; the spec's test would otherwise fail.
6. **Open-migration check on start.** `ReadOpenMigration` (under `application.migrate`) returns `nil, nil` when the application has no open migration, so the start route's pre-check writes no `failure` audit row on every ordinary start; `sourceMigration` turns `nil` into `ErrNotFound`.

## Review Focus

1. Abandon, then migrate again: the second destination must be `<source> on <endpoint> (2)` with its own project, and the first destination must stay untouched (no rename, no delete). Pinned in Task 2 (`TestMigrationDestinationNaming`).
2. A name that fits only without a suffix: a 253-byte `<source> on <endpoint>` whose base is taken must be `destination_name_too_long` (the ` (2)` candidate is 257 bytes), not `application_name_taken`. Pinned in Task 2 (`TestMigrationDestinationNameTooLong`).
3. A stale destination inventory must refuse choices even when they name a class the stale snapshot lists; the refusal must not be `storage_class_unknown`. Pinned in Task 2 (`TestMigrationNeedsAFreshDestinationInventory`).
4. An environment administrator posting choices or analyze on an application that has no migration must get 403 with a `denied` `application.migrate` row, never 404. Pinned in Task 3 (`TestMigrationMutationsCheckPermissionFirst`).
5. A removal whose agent reports fewer kept claims for a service than the plan mounts (a claim the head revision added and nobody applied) must not name any claim of that service. Pinned in Task 2 (`TestRetainedClaimsNameOnlyAnExactCount`, the 1-of-2 case).

## File map

| Path | Task | Change |
|---|---|---|
| `internal/runtime/kubernetes/deploy.go`, `claims_test.go`, `AGENTS.md`; `internal/agent/protocol/{kubernetes_deploy.go,inventory.go}` and their tests; `internal/agent/AGENTS.md` | 1 | `claim=<Phase>` rollout reason, `storage_classes` truncatable, claim/StorageClass error paths, claim cap |
| `internal/store/{application_deployment.go,application_migration.go,store.go}`, `internal/store/{kubernetes_deployment_test.go,application_migration_test.go}`, `internal/api/tenant_handlers.go`, `web/src/components/ApplicationMigration.tsx` + test, `internal/store/AGENTS.md` | 2 | `RetainedClaims`, destination naming, `ErrDestinationNameTooLong`, `ErrInventoryStale`, `CheckMigrationChoices`, `ReadOpenMigration` |
| `internal/api/migration_handlers.go`, `internal/api/{migration_test.go,kubernetes_deploy_test.go}`, `internal/api/AGENTS.md` | 3 | State, choices and permission before inspection; gate-matrix row; budget and canary tests |
| `web/src/components/ApplicationDeploymentPlan.tsx` + test, `web/dist`, `web/tsconfig.tsbuildinfo`, `web/AGENTS.md` | 4 | Claim reason sentence, "Kept on the cluster" |
| `docs/{application-schema,agent-protocol,authorization-matrix}.md`, `README.md` | 5 | Documents; the gate |

---

### Task 1: Runtime — an unbound claim in a rollout timeout, claim error paths, the StorageClass cut

**Files:**
- Modify: `internal/agent/protocol/kubernetes_deploy.go` (new `rolloutReason`, `rolloutDetail`)
- Modify: `internal/agent/protocol/inventory.go` (`truncatable`)
- Modify: `internal/runtime/kubernetes/deploy.go` (`stalled`)
- Test: `internal/agent/protocol/kubernetes_deploy_test.go` (`TestKubernetesStepCodes` rows), `internal/agent/protocol/kubernetes_claims_test.go` (`TestClampKeepsTheStorageClassCut`)
- Test: `internal/runtime/kubernetes/claims_test.go` (helper `removalRequest`, five tests)
- Docs: `internal/runtime/kubernetes/AGENTS.md`, `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: `run.stalled(set render.Set, d *appsv1.Deployment) string`, `render.Set.Claims []*corev1.PersistentVolumeClaim`, test helpers `deployCluster`, `deployClaimRequest`, `owned`, `writes`, `cluster`, constants `testApp`, `testInstance`, `testSpec` (existing).
- Produces: a `rollout_timeout` detail may carry `claim=Pending` or `claim=Lost` (one per planned claim of the service, within the existing cap of three reasons), which `protocol.DeploymentResult.Validate` accepts and Task 4's web renders. `protocol.Clamp` keeps `storage_classes` in `Truncated`. Test helper `func removalRequest(services ...string) protocol.RemovalRequest` (package `kubernetes`, test only).

- [ ] **Step 1: Write the protocol tests**

In `internal/agent/protocol/kubernetes_deploy_test.go`, inside `TestKubernetesStepCodes`, directly below the row `{"rollout_timeout", "replicafailure=exceeded quota", false},` insert:

```go
		{"rollout_timeout", "progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable,claim=Pending", true},
		{"rollout_timeout", "claim=Lost", true},
		{"rollout_timeout", "claim=Bound", false},
		{"rollout_timeout", "claim=shop-data", false},
		{"rollout_timeout", "claim=Pending,claim=Pending,claim=Lost,pod=ContainerCreating", false},
```

Append to the end of `internal/agent/protocol/kubernetes_claims_test.go` (`slices` is already imported):

```go

// Clamp keeps storage_classes in Truncated: the store reads it as "the list cannot prove a class
// absent", on the agent's snapshot and again on the server's copy.
func TestClampKeepsTheStorageClassCut(t *testing.T) {
	s := Snapshot{Truncated: []string{"storage_classes"}, Kubernetes: &KubernetesInventory{}}
	Clamp(&s)
	if !slices.Equal(s.Truncated, []string{"storage_classes"}) {
		t.Fatalf("truncated %v", s.Truncated)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test -count=1 -run 'TestKubernetesStepCodes|TestClampKeepsTheStorageClassCut' ./internal/agent/protocol/`
Expected: FAIL: `rollout_timeout "progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable,claim=Pending"` and `"claim=Lost"` rejected, and `truncated []`.

- [ ] **Step 3: Accept the claim reason and keep the StorageClass cut**

In `internal/agent/protocol/kubernetes_deploy.go`, insert directly above the `var (` that opens with `// kubeObject is a name_taken, ...`:

```go
// rolloutReason is one reason of a rollout_timeout detail.
const rolloutReason = `((progressing|available|replicafailure|pod)=[A-Za-z]{1,64}|claim=(Pending|Lost))`

```

and replace

```go
	// rolloutDetail is a rollout_timeout detail: up to three reasons, each a Kubernetes reason
	// word under the condition or pod it came from.
	rolloutDetail = regexp.MustCompile(`^((progressing|available|replicafailure|pod)=[A-Za-z]{1,64}(,(progressing|available|replicafailure|pod)=[A-Za-z]{1,64}){0,2})?$`)
```

with

```go
	// rolloutDetail is a rollout_timeout detail: up to three reasons, each a Kubernetes reason
	// word under the condition or pod it came from, or a planned claim's phase while unbound.
	rolloutDetail = regexp.MustCompile(`^(` + rolloutReason + `(,` + rolloutReason + `){0,2})?$`)
```

In `internal/agent/protocol/inventory.go`, replace

```go
var truncatable = []string{"containers", "images", "networks", "volumes", "labels", "nodes", "namespaces", "workloads", "pods", "services", "claims"}
```

with

```go
var truncatable = []string{"containers", "images", "networks", "volumes", "labels", "nodes", "namespaces", "workloads", "pods", "services", "claims", "storage_classes"}
```

- [ ] **Step 4: Run the protocol tests**

Run: `go test -count=1 -run 'TestKubernetesStepCodes|TestClampKeepsTheStorageClassCut|Clamp|Shrink|Snapshot|StorageClass' ./internal/agent/protocol/`
Expected: `ok`.

- [ ] **Step 5: Write the runtime tests**

In `internal/runtime/kubernetes/claims_test.go`, replace the import block with:

```go
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)
```

Directly below `func ptr[T any](v T) *T { return &v }` insert:

```go

// removalRequest removes the named services of the instance in namespace shop.
func removalRequest(services ...string) protocol.RemovalRequest {
	now := time.Now()
	return protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: services}
}
```

Append to the end of the file:

```go

// A rollout that times out names each planned claim the cluster has not bound, by its phase: a
// Pending claim (no provisioner, no default class) is why the pod never scheduled. A Bound claim
// adds nothing.
func TestDeployRolloutTimeoutNamesAnUnboundClaim(t *testing.T) {
	for phase, want := range map[corev1.PersistentVolumeClaimPhase]string{
		corev1.ClaimPending: "progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable,claim=Pending",
		corev1.ClaimLost:    "progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable,claim=Lost",
		corev1.ClaimBound:   "progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable",
	} {
		t.Run(string(phase), func(t *testing.T) {
			c, cs := deployCluster(t, false, false)
			cs.PrependReactor("create", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
				action.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim).Status.Phase = phase
				return false, nil, nil
			})
			res := c.Deploy(context.Background(), deployClaimRequest(300*time.Millisecond), func() {})
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			if res.Outcome != protocol.OutcomeTimedOut || i < 0 || res.Validate() != nil {
				t.Fatalf("result %+v %v", res, res.Validate())
			}
			if s := res.Steps[i]; s.Service != "web" || s.Step != protocol.StepStart || s.Code != "rollout_timeout" || s.Detail != want {
				t.Fatalf("step %+v", s)
			}
		})
	}
}

// A claim read the API server answers with an error other than NotFound fails the precondition
// with that status; it is never mistaken for another owner's claim, and nothing is written.
func TestDeployClaimReadErrorIsNotNameTaken(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	cs.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd leader changed"))
	})
	res := c.Deploy(context.Background(), deployClaimRequest(time.Minute), func() {})
	i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
	if res.Outcome != protocol.OutcomeFailed || i < 0 || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	if s := res.Steps[i]; s.Service != "web" || s.Step != protocol.StepPrecondition || s.Code != "runtime_status" || s.Detail != "500" {
		t.Fatalf("step %+v", s)
	}
	if len(writes(cs)) != 0 {
		t.Fatalf("wrote %v", writes(cs))
	}
}

// Past MaxDeploymentVolumes labelled claims a removal reports exactly the cap, logs that the rest
// were left off, and still succeeds: every claim is still in the cluster.
func TestRemoveReportsAtMostTheClaimCap(t *testing.T) {
	objects := []runtime.Object{}
	for i := range protocol.MaxDeploymentVolumes + 1 {
		m := owned("web")
		m.Name = fmt.Sprintf("shop-data-%02d", i)
		objects = append(objects, &corev1.PersistentVolumeClaim{ObjectMeta: m})
	}
	c, _ := deployCluster(t, true, false, objects...)
	var logged bytes.Buffer
	c.log = log.New(&logged, "", 0)
	res := c.Remove(context.Background(), removalRequest("web"), func() {})
	kept := 0
	for _, s := range res.Steps {
		if s.Step == protocol.StepVolume && s.Detail == protocol.DetailRetained {
			kept++
		}
	}
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil || kept != protocol.MaxDeploymentVolumes {
		t.Fatalf("result %+v kept %d %v", res.Outcome, kept, res.Validate())
	}
	if !strings.Contains(logged.String(), "the rest are not reported") {
		t.Fatalf("log %q", logged.String())
	}
}

// A claim list the API server refuses fails the removal's precondition with its status, before
// anything is deleted.
func TestRemoveClaimListFailureDeletesNothing(t *testing.T) {
	web := &appsv1.Deployment{ObjectMeta: owned("web")}
	web.Name = "shop-web"
	c, cs := deployCluster(t, true, false, web)
	cs.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd leader changed"))
	})
	res := c.Remove(context.Background(), removalRequest("web"), func() {})
	i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
	if res.Outcome != protocol.OutcomeFailed || i < 0 || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	if s := res.Steps[i]; s.Step != protocol.StepPrecondition || s.Code != "runtime_status" || s.Detail != "500" {
		t.Fatalf("step %+v", s)
	}
	if len(writes(cs)) != 0 {
		t.Fatalf("deleted %v", writes(cs))
	}
}

// A StorageClass list the ServiceAccount may not read (a manifest older than migrations) is
// reported empty and named storage_classes in Truncated; the rest of the snapshot is whole.
func TestSnapshotSurvivesAForbiddenStorageClassList(t *testing.T) {
	c, cs := cluster(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}})
	cs.PrependReactor("list", "storageclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, "", nil)
	})
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	k := snap.Kubernetes
	if k.StorageClasses == nil || len(k.StorageClasses) != 0 || !slices.Equal(snap.Truncated, []string{"storage_classes"}) {
		t.Fatalf("classes %v truncated %v", k.StorageClasses, snap.Truncated)
	}
	if !slices.Equal(k.Namespaces, []string{"shop"}) || snap.Engine.Version != "v1.36.0" {
		t.Fatalf("the rest of the snapshot went with the forbidden list: %v %+v", k.Namespaces, snap.Engine)
	}
}
```

- [ ] **Step 6: Run them to see the rollout test fail**

Run: `go test -count=1 -run 'TestDeployRolloutTimeoutNamesAnUnboundClaim|TestDeployClaimReadErrorIsNotNameTaken|TestRemoveReportsAtMostTheClaimCap|TestRemoveClaimListFailureDeletesNothing|TestSnapshotSurvivesAForbiddenStorageClassList' ./internal/runtime/kubernetes/`
Expected: FAIL only in `TestDeployRolloutTimeoutNamesAnUnboundClaim/Pending` and `/Lost` (`Detail:progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable`). The other four pass: they pin behaviour that already holds (the StorageClass one passes because Step 3 fixed `truncatable`; before it, it failed with `truncated []`).

- [ ] **Step 7: Name the unbound claim**

In `internal/runtime/kubernetes/deploy.go`, replace the `stalled` doc comment

```go
// stalled says why a rollout did not finish: the Deployment's Progressing reason, its Available
// reason when unavailable, its ReplicaFailure reason when pods could not be created, and the
// newest pod's waiting reason, as far as each can be read in a few seconds past the deadline.
```

with

```go
// stalled says why a rollout did not finish: the Deployment's Progressing reason, its Available
// reason when unavailable, its ReplicaFailure reason when pods could not be created, the phase of
// each planned claim still Pending or Lost (a pod whose claim is unbound never schedules), and the
// newest pod's waiting reason, as far as each can be read in a few seconds past the deadline.
```

and inside `stalled`, replace

```go
	defer cancel()
	pods, err := r.c.cs.CoreV1().Pods(r.namespace).List(
```

with

```go
	defer cancel()
	for _, want := range set.Claims {
		pvc, err := r.c.cs.CoreV1().PersistentVolumeClaims(r.namespace).Get(ctx, want.Name, metav1.GetOptions{})
		if err == nil && (pvc.Status.Phase == corev1.ClaimPending || pvc.Status.Phase == corev1.ClaimLost) {
			parts = append(parts, "claim="+string(pvc.Status.Phase))
		}
	}
	pods, err := r.c.cs.CoreV1().Pods(r.namespace).List(
```

The existing `strings.Join(parts[:min(len(parts), 3)], ",")` keeps the cap: the conditions come first, then the claims, then the pod reason.

- [ ] **Step 8: Run the runtime tests**

Run: `gofmt -l internal && go test -count=1 -run 'TestDeployRolloutTimeoutNamesAnUnboundClaim|TestDeployClaimReadErrorIsNotNameTaken|TestRemoveReportsAtMostTheClaimCap|TestRemoveClaimListFailureDeletesNothing|TestSnapshotSurvivesAForbiddenStorageClassList|TestDeployRolloutTimeout|TestDeployClaim|TestRemoveRetainsClaims' ./internal/runtime/kubernetes/`
Expected: no gofmt output, then `ok`.

- [ ] **Step 9: DOX**

In `internal/runtime/kubernetes/AGENTS.md`, replace

````text
The detail carries the Progressing reason, the Available reason when unavailable, the ReplicaFailure reason and the newest labelled pod's waiting reason (at most three), words only;
````

with

````text
The detail carries the Progressing reason, the Available reason when unavailable, the ReplicaFailure reason, `claim=Pending` or `claim=Lost` for each of the service's planned claims in that phase (read by name past the deadline; a Bound claim or a failed read adds nothing) and the newest labelled pod's waiting reason (at most three, in that order), words only;
````

In `internal/runtime/kubernetes/AGENTS.md`, replace

````text
and a recreated same-name object surviving a guarded delete.
````

with

````text
a recreated same-name object surviving a guarded delete, an unbound claim named in a timeout, a claim read error that is not `name_taken`, a claim list error that deletes nothing, and the claim cap on removal; `TestSnapshotSurvivesAForbiddenStorageClassList` pins `storage_classes` in `Truncated`.
````

In `internal/agent/AGENTS.md`, replace

````text
`rollout_timeout` (detail: at most three `progressing|available|replicafailure|pod=<Reason>`, reasons CamelCase words)
````

with

````text
`rollout_timeout` (detail: at most three `progressing|available|replicafailure|pod=<Reason>` or `claim=Pending|Lost`, reasons CamelCase words)
````

In `internal/agent/AGENTS.md`, replace

````text
cut named `storage_classes`) is never nil after `Clamp`.
````

with

````text
cut named `storage_classes`, a name `Clamp` keeps in `Truncated` like every other list) is never nil after `Clamp`.
````

- [ ] **Step 10: Commit**

```bash
gofmt -l cmd internal && git add internal/agent/protocol/kubernetes_deploy.go internal/agent/protocol/kubernetes_deploy_test.go internal/agent/protocol/inventory.go internal/agent/protocol/kubernetes_claims_test.go internal/runtime/kubernetes/deploy.go internal/runtime/kubernetes/claims_test.go internal/runtime/kubernetes/AGENTS.md internal/agent/AGENTS.md && make tidy-check lint && git commit -m "fix(k8s): name an unbound claim in a rollout timeout; keep the StorageClass cut; claim error-path tests" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Store — retained claims on read, destination naming, the two new refusals

**Files:**
- Modify: `internal/store/application_deployment.go` (`Deployment.RetainedClaims`, `scanDeployment`, new `retainedClaims`)
- Modify: `internal/store/application_migration.go` (constants and errors, `migrationDestination`, new `destinationName`, new `ReadOpenMigration`, `AnalyzeMigration`, `checkChoices` becomes `CheckMigrationChoices`, `CreateMigrationDestination`)
- Modify: `internal/store/store.go` (`TenancyStore.ReadOpenMigration`)
- Modify: `internal/api/tenant_handlers.go` (`tenantError`: two codes, one sentence)
- Modify: `web/src/components/ApplicationMigration.tsx` (`MIGRATION_ERRORS`)
- Test: `internal/store/kubernetes_deployment_test.go` (`TestRemoveKubernetesRetainsClaims`, new `TestRetainedClaimsNameOnlyAnExactCount`)
- Test: `internal/store/application_migration_test.go` (`TestMigrationDestination`; new `readyMigration`, `TestMigrationDestinationNaming`, `TestMigrationDestinationNameTooLong`, `TestMigrationNeedsAFreshDestinationInventory`)
- Test: `web/src/components/ApplicationMigration.test.tsx`
- Docs: `internal/store/AGENTS.md`, `internal/api/AGENTS.md` (the two codes)

**Interfaces:**
- Consumes: `scanDeployment`, `freshInventory(state, raw string, received, observed time.Time) (protocol.Snapshot, map[string]protocol.Container, error)`, `KubernetesProject(name string) string`, `openMigrationOf`, `readTenant`, `migrationApp` (existing); test helpers `migrationFixture`, `kubernetesPlanFixture`, `putStorageClasses`, `analysis`, `dataChoice`, `twoServiceSpec`, `imageCheckKey` (existing).
- Produces (package `store`):
  ```go
  // Deployment gains, after MigrationID:
  RetainedClaims []string `json:"retained_claims,omitempty"`
  func retainedClaims(d *Deployment) []string
  const MaxDestinationSuffix = 9
  var ErrDestinationNameTooLong, ErrInventoryStale error
  func (t *tenancyStore) destinationName(ctx context.Context, tx *sql.Tx, a TenantAccess, base, endpoint string) (string, string, error)
  func CheckMigrationChoices(spec ApplicationSpec, classes []protocol.StorageClass, choices MigrationChoices) error
  // TenancyStore gains:
  ReadOpenMigration(ctx context.Context, access TenantAccess, applicationID string) (*ApplicationMigration, error) // nil, nil when none
  ```
  API codes (409): `destination_name_too_long`, `inventory_stale`. Web: `MIGRATION_ERRORS.destination_name_too_long`, `MIGRATION_ERRORS.inventory_stale`. Task 3 calls `ReadOpenMigration` and `CheckMigrationChoices`; Task 4 renders `retained_claims`.

- [ ] **Step 1: Write the retained-claims tests**

In `internal/store/kubernetes_deployment_test.go`, inside `TestRemoveKubernetesRetainsClaims`, directly below

```go
	if err := ts.SettleDeployment(ctx, cluster, result(steps...)); err != nil {
		t.Fatalf("a claim count and service the plan does not expect: %v", err)
	}
```

insert

```go
	// db reported as many kept claims as it mounts, so they are named; web mounts none and cache
	// is not planned, so theirs cannot be.
	if read, err := ts.ReadDeployment(ctx, a, app.ID, d.ID); err != nil || !slices.Equal(read.RetainedClaims, []string{"shop-front-data", "shop-front-logs"}) {
		t.Fatalf("retained claims %+v %v", read, err)
	}
```

and append to the end of the file:

```go

// A service's planned claims are named only when the agent reported exactly that many kept: fewer
// (a claim the head revision added that nobody applied) or more (one an older revision left)
// cannot say which exist.
func TestRetainedClaimsNameOnlyAnExactCount(t *testing.T) {
	kept := protocol.DeploymentStep{Service: "db", Step: protocol.StepVolume, Outcome: protocol.OutcomeSkipped, Detail: protocol.DetailRetained}
	plan := DeploymentPlan{Namespace: "shop", Services: []PlannedService{{Name: "db", ClaimMounts: []protocol.KubernetesMount{{Claim: "shop-logs"}, {Claim: "shop-data"}}}}}
	for n, want := range map[int][]string{0: nil, 1: nil, 2: {"shop-data", "shop-logs"}, 3: nil} {
		d := Deployment{Kind: "remove", Plan: plan, Result: &protocol.DeploymentResult{Steps: slices.Repeat([]protocol.DeploymentStep{kept}, n)}}
		if got := retainedClaims(&d); !slices.Equal(got, want) {
			t.Errorf("%d kept: %v", n, got)
		}
	}
	docker := Deployment{Kind: "remove", Plan: DeploymentPlan{Services: plan.Services}, Result: &protocol.DeploymentResult{Steps: []protocol.DeploymentStep{kept, kept}}}
	if got := retainedClaims(&docker); got != nil {
		t.Fatalf("a Docker removal: %v", got)
	}
}
```

- [ ] **Step 2: Write the migration tests**

In `internal/store/application_migration_test.go`, add `"fmt"` to the imports (between `"errors"` and `"slices"`). In `TestMigrationDestination`, replace

```go
	squatter, err := ts.ImportApplication(ctx, a, src.Destination.Name, twoServiceSpec(), map[string]string{"web.TOKEN": "x"}, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey); !errors.Is(err, ErrApplicationNameTaken) {
		t.Fatalf("a taken name: %v", err)
	}
	if err := ts.DiscardApplication(ctx, a, squatter.ID, 1); err != nil {
		t.Fatal(err)
	}
```

with

```go
	var squatters []*Application
	for n := 1; n <= MaxDestinationSuffix; n++ {
		name := src.Destination.Name
		if n > 1 {
			name += fmt.Sprintf(" (%d)", n)
		}
		squatter, err := ts.ImportApplication(ctx, a, name, twoServiceSpec(), map[string]string{"web.TOKEN": "x"}, imageCheckKey)
		if err != nil {
			t.Fatal(err)
		}
		squatters = append(squatters, squatter)
	}
	if _, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey); !errors.Is(err, ErrApplicationNameTaken) {
		t.Fatalf("every name taken: %v", err)
	}
	if err := ts.DiscardApplication(ctx, a, squatters[4].ID, 1); err != nil {
		t.Fatal(err)
	}
```

and at its end replace `if err != nil || len(apps) != 1 {` with `if err != nil || len(apps) != MaxDestinationSuffix {` (the source and the eight squatters left: nothing was created). Replace its doc comment's first line with `// The destination refuses when its name and every suffix are taken, and a source whose definition moved on since the` (the second line stays).

Directly after `TestMigrationDestination` insert:

```go

// readyMigration starts app's migration to namespace shop of cluster and makes it ready.
func readyMigration(t *testing.T, ts TenancyStore, a TenantAccess, app, cluster string) {
	t.Helper()
	ctx := context.Background()
	if _, err := ts.CreateMigration(ctx, a, app, MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}, analysis(1, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AnalyzeMigration(ctx, a, app, MigrationChoices{Volumes: dataChoice.Volumes, Acknowledged: MigrationAcknowledgeable}, analysis(1, true)); err != nil {
		t.Fatal(err)
	}
}

// Abandoning a migration keeps its destination; migrating again names the new destination
// "<source> on <cluster> (2)", with its own project, and the analysis already names it so.
func TestMigrationDestinationNaming(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "p"})
	ctx := context.Background()
	ts := st.Tenancy()
	readyMigration(t, ts, a, app.ID, cluster)
	first, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	base := first.DestinationApplicationName
	if _, err := ts.AbandonMigration(ctx, a, app.ID); err != nil {
		t.Fatal(err)
	}
	src, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster)
	if err != nil || src.Destination.Name != base+" (2)" || src.Destination.Project != KubernetesProject(base+" (2)") {
		t.Fatalf("the next analysis %+v %v", src, err)
	}
	readyMigration(t, ts, a, app.ID, cluster)
	second, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey)
	if err != nil || second.DestinationApplicationName != base+" (2)" || second.DestinationApplicationID == first.DestinationApplicationID {
		t.Fatalf("second destination %+v %v", second, err)
	}
	mapped, err := ts.ReadApplicationMapping(ctx, a, second.DestinationApplicationID)
	if err != nil || mapped.Preview.Project != KubernetesProject(base+" (2)") {
		t.Fatalf("second destination mapping %+v %v", mapped, err)
	}
	kept, err := ts.ReadApplicationMapping(ctx, a, first.DestinationApplicationID)
	if err != nil || kept.Preview.ApplicationName != base || kept.Preview.Project != KubernetesProject(base) {
		t.Fatalf("the abandoned destination changed %+v %v", kept, err)
	}
}

// A destination name over 255 bytes is refused at analysis already: the name itself, or the
// " (2)" a taken name needs.
func TestMigrationDestinationNameTooLong(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "p"})
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.RenameEndpoint(ctx, a, cluster, "k"); err != nil {
		t.Fatal(err)
	}
	rename := func(n int) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE applications SET name=? WHERE id=?`), strings.Repeat("s", n), app.ID); err != nil {
			t.Fatal(err)
		}
	}
	rename(251) // "<251> on k" is 256 bytes
	if _, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster); !errors.Is(err, ErrDestinationNameTooLong) {
		t.Fatalf("a 256-byte name: %v", err)
	}
	rename(248) // "<248> on k" is 253 bytes; with " (2)" it is 257
	src, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster)
	if err != nil || len(src.Destination.Name) != 253 {
		t.Fatalf("a 253-byte name: %+v %v", src, err)
	}
	if _, err := ts.ImportApplication(ctx, a, src.Destination.Name, twoServiceSpec(), map[string]string{"web.TOKEN": "x"}, imageCheckKey); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster); !errors.Is(err, ErrDestinationNameTooLong) {
		t.Fatalf("a taken name whose suffix overflows: %v", err)
	}
}

// Analysis and choices read the destination's StorageClasses only from a fresh inventory: a stale
// one is ErrInventoryStale, even for a class the stale snapshot lists.
func TestMigrationNeedsAFreshDestinationInventory(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "p"})
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.CreateMigration(ctx, a, app.ID, MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}, analysis(1, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_inventory SET received_at=? WHERE endpoint_id=?`), time.Now().UTC().Add(-10*time.Minute), cluster); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster); !errors.Is(err, ErrInventoryStale) {
		t.Fatalf("analysis on a stale inventory: %v", err)
	}
	if _, err := ts.AnalyzeMigration(ctx, a, app.ID, dataChoice, analysis(1, true)); !errors.Is(err, ErrInventoryStale) {
		t.Fatalf("choices on a stale inventory: %v", err)
	}
	putStorageClasses(t, ts, cluster, []protocol.StorageClass{{Name: "standard", Default: true}, {Name: "fast"}})
	if _, err := ts.AnalyzeMigration(ctx, a, app.ID, dataChoice, analysis(1, true)); err != nil {
		t.Fatalf("choices on a fresh inventory: %v", err)
	}
}
```

- [ ] **Step 3: Run them to see them fail**

Run: `go test -count=1 -run 'TestRemoveKubernetesRetainsClaims|TestRetainedClaimsNameOnlyAnExactCount|TestMigrationDestination|TestMigrationNeedsAFreshDestinationInventory' ./internal/store/`
Expected: build failure, `undefined: retainedClaims`, `read.RetainedClaims undefined`, `undefined: MaxDestinationSuffix`, `undefined: ErrDestinationNameTooLong`, `undefined: ErrInventoryStale`.

- [ ] **Step 4: Derive the retained claims on read**

In `internal/store/application_deployment.go`, in `type Deployment`, replace

```go
	MigrationID string `json:"migration_id,omitempty"`
}
```

with

```go
	MigrationID string `json:"migration_id,omitempty"`
	// RetainedClaims names the claims a settled cluster removal reported kept (retainedClaims).
	RetainedClaims []string `json:"retained_claims,omitempty"`
}
```

In `scanDeployment`, replace

```go
		d.Result = &res
	}
	d.Expired = !time.Now().Before(d.ExpiresAt)
	return &d, nil
}
```

with

```go
		d.Result = &res
	}
	d.RetainedClaims = retainedClaims(&d)
	d.Expired = !time.Now().Before(d.ExpiresAt)
	return &d, nil
}

// retainedClaims names the claims a cluster removal reported kept, sorted. A retained step names
// only its service, so a service's planned claim mounts are named when the agent reported exactly
// that many; any other count cannot say which claims exist, and that service names none.
func retainedClaims(d *Deployment) []string {
	if d.Kind != "remove" || d.Plan.Namespace == "" || d.Result == nil {
		return nil
	}
	kept := map[string]int{}
	for _, s := range d.Result.Steps {
		if s.Step == protocol.StepVolume && s.Outcome == protocol.OutcomeSkipped && s.Detail == protocol.DetailRetained {
			kept[s.Service]++
		}
	}
	var out []string
	for _, ps := range d.Plan.Services {
		if n := kept[ps.Name]; n > 0 && n == len(ps.ClaimMounts) {
			for _, m := range ps.ClaimMounts {
				out = append(out, m.Claim)
			}
		}
	}
	slices.Sort(out)
	return out
}
```

- [ ] **Step 5: Name the destination, gate the inventory, share the choice check**

In `internal/store/application_migration.go`:

Add `"fmt"` to the imports (between `"errors"` and `"slices"`).

Replace

```go
	MaxMigrationReportBytes     = 65536
	MaxMigrationNoteRunes       = 500
)
```

with

```go
	MaxMigrationReportBytes     = 65536
	MaxMigrationNoteRunes       = 500
	// MaxDestinationSuffix is the last " (n)" a destination name takes when the ones before it
	// are held.
	MaxDestinationSuffix = 9
)
```

Replace

```go
	ErrApplicationNameTaken = errors.New("the destination's name is taken")
)
```

with

```go
	ErrApplicationNameTaken = errors.New("the destination's name is taken")
	// ErrDestinationNameTooLong is a destination name over validTenantName's 255 bytes.
	ErrDestinationNameTooLong = errors.New("the destination's name would be too long")
	// ErrInventoryStale is a destination cluster whose inventory is not fresh (freshInventory).
	ErrInventoryStale = errors.New("the destination cluster's inventory is stale")
)
```

Replace the whole `migrationDestination` function (from its doc comment `// migrationDestination reads a Kubernetes endpoint's namespaces and reported StorageClasses and` to its closing `}`) with:

```go
// migrationDestination reads a Kubernetes endpoint's namespaces and, from its fresh inventory
// (freshInventory, else ErrInventoryStale), its StorageClasses; given the source's name, also the
// destination's (destinationName). The endpoint must be connected: a revoked or offline cluster
// must not receive a destination application holding copied secrets.
func (t *tenancyStore) migrationDestination(ctx context.Context, tx *sql.Tx, a TenantAccess, sourceName, endpoint string) (MigrationDestination, error) {
	d := MigrationDestination{EndpointID: endpoint, StorageClasses: []protocol.StorageClass{}}
	var runtime, name, namespaces, state string
	var snapshot sql.NullString
	var received, observed sql.NullTime
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.runtime,e.name,e.deploy_namespaces,e.state,v.snapshot,v.received_at,v.observed_at FROM endpoints e LEFT JOIN endpoint_inventory v ON v.endpoint_id=e.id WHERE e.organization_id=? AND e.environment_id=? AND e.id=?`), a.OrganizationID, a.EnvironmentID, endpoint).Scan(&runtime, &name, &namespaces, &state, &snapshot, &received, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if state != "active" {
		return d, ErrEndpointOffline
	}
	if runtime != protocol.RuntimeKubernetes {
		return d, ErrRuntimeUnsupported
	}
	if !snapshot.Valid {
		return d, ErrInventoryStale
	}
	s, _, err := freshInventory(state, snapshot.String, received.Time, observed.Time)
	if err != nil {
		return d, ErrInventoryStale
	}
	d.Namespaces = decodeNamespaces(namespaces)
	if s.Kubernetes != nil && s.Kubernetes.StorageClasses != nil {
		d.StorageClasses = s.Kubernetes.StorageClasses
	}
	if sourceName == "" {
		return d, nil
	}
	d.Name, d.Project, err = t.destinationName(ctx, tx, a, sourceName+" on "+name, endpoint)
	return d, err
}

// destinationName is the first of base, "base (2)" to "base (MaxDestinationSuffix)" that no
// application in the environment is named and whose project no instance on endpoint holds, so an
// abandoned migration's destination can stay while a new one is created. A candidate over 255
// bytes is ErrDestinationNameTooLong; all of them held is ErrApplicationNameTaken.
func (t *tenancyStore) destinationName(ctx context.Context, tx *sql.Tx, a TenantAccess, base, endpoint string) (string, string, error) {
	for n := 1; n <= MaxDestinationSuffix; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s (%d)", base, n)
		}
		if len(name) > 255 {
			return "", "", ErrDestinationNameTooLong
		}
		project := KubernetesProject(name)
		var taken int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT (SELECT COUNT(*) FROM applications WHERE organization_id=? AND environment_id=? AND name=?)+(SELECT COUNT(*) FROM application_instances WHERE organization_id=? AND environment_id=? AND endpoint_id=? AND project=?)`), a.OrganizationID, a.EnvironmentID, name, a.OrganizationID, a.EnvironmentID, endpoint, project).Scan(&taken); err != nil {
			return "", "", err
		}
		if taken == 0 {
			return name, project, nil
		}
	}
	return "", "", ErrApplicationNameTaken
}
```

Insert directly above `// changeMigration runs op on app's open migration under application.migrate, locked behind the`:

```go
// ReadOpenMigration returns app's open migration as its source, under application.migrate, and
// nil when it has none: the API reads it before a mutation spends an inspection.
func (t *tenancyStore) ReadOpenMigration(ctx context.Context, a TenantAccess, app string) (*ApplicationMigration, error) {
	app, err := migrationApp(a, app)
	if err != nil {
		return nil, err
	}
	var out *ApplicationMigration
	err = t.readTenant(ctx, a, permissions.ApplicationMigrate, func(tx *sql.Tx) error {
		out, err = t.openMigrationOf(ctx, tx, a, app)
		if errors.Is(err, ErrNotFound) {
			out, err = nil, nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

```

In `AnalyzeMigration`, delete the pre-transaction block

```go
	if len(choices.Volumes) > protocol.MaxKubernetesClaims {
		return nil, ErrInvalid
	}
	for i, code := range choices.Acknowledged {
		if !slices.Contains(MigrationAcknowledgeable, code) || slices.Contains(choices.Acknowledged[:i], code) {
			return nil, ErrInvalid
		}
	}
```

and replace `if err := checkChoices(spec, dest.StorageClasses, choices); err != nil {` with `if err := CheckMigrationChoices(spec, dest.StorageClasses, choices); err != nil {`.

Replace

```go
// checkChoices holds each choice to a named volume of spec and to the destination's classes.
func checkChoices(spec ApplicationSpec, classes []protocol.StorageClass, choices MigrationChoices) error {
	named := spec.namedVolumes()
```

with

```go
// CheckMigrationChoices holds choices to at most MaxKubernetesClaims volumes and distinct
// MigrationAcknowledgeable codes, each volume to a named volume of spec, a valid size, ReadWriteOnce
// and one of the destination's classes. The API checks before it spends an inspection; the store
// checks again against the head revision.
func CheckMigrationChoices(spec ApplicationSpec, classes []protocol.StorageClass, choices MigrationChoices) error {
	if len(choices.Volumes) > protocol.MaxKubernetesClaims {
		return ErrInvalid
	}
	for i, code := range choices.Acknowledged {
		if !slices.Contains(MigrationAcknowledgeable, code) || slices.Contains(choices.Acknowledged[:i], code) {
			return ErrInvalid
		}
	}
	named := spec.namedVolumes()
```

In `CreateMigrationDestination`, replace the doc comment's first line `// CreateMigrationDestination creates the destination application "<name> on <endpoint>": revision` with `// CreateMigrationDestination creates the destination application under destinationName: revision`, and delete the name check that `migrationDestination` now does:

```go
		var taken int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT (SELECT COUNT(*) FROM applications WHERE organization_id=? AND environment_id=? AND name=?)+(SELECT COUNT(*) FROM application_instances WHERE organization_id=? AND environment_id=? AND endpoint_id=? AND project=?)`), a.OrganizationID, a.EnvironmentID, dest.Name, a.OrganizationID, a.EnvironmentID, dest.EndpointID, dest.Project).Scan(&taken); err != nil {
			return err
		}
		if taken > 0 {
			return ErrApplicationNameTaken
		}
```

In `internal/store/store.go`, directly below the `ReadMigration(...)` line of `TenancyStore` insert:

```go
	// ReadOpenMigration reads the open migration of a source under application.migrate; nil when none.
	ReadOpenMigration(ctx context.Context, access TenantAccess, applicationID string) (*ApplicationMigration, error)
```

- [ ] **Step 6: Run the store tests on SQLite**

Run: `gofmt -l internal && go vet ./internal/store/ && go test -count=1 -run 'TestMigration|TestRemoveKubernetesRetainsClaims|TestRetainedClaimsNameOnlyAnExactCount|TestDestinationEditKeepsClaims' ./internal/store/`
Expected: no gofmt output, then `ok` (`TestMigrationLifecycle`, `TestMigrationAuthorization`, `TestMigrationDestination`, `TestMigrationDestinationNaming`, `TestMigrationDestinationNameTooLong`, `TestMigrationNeedsAFreshDestinationInventory`, `TestMigrationKeepsTheSource`, `TestMigrationIDOnDestinationApplies`, `TestMigrationConfirmWithoutDestination`, `TestMigrationDestinationEndpointOffline`, `TestDestinationEditKeepsClaims`, `TestRemoveKubernetesRetainsClaims`, `TestRetainedClaimsNameOnlyAnExactCount` among them).

- [ ] **Step 7: Run them on PostgreSQL**

Run: `PG=… go test -count=1 -p 1 -run 'TestMigration|TestRemoveKubernetesRetainsClaims|TestRetainedClaimsNameOnlyAnExactCount|TestDestinationEditKeepsClaims' ./internal/store/`
Expected: `ok` (each test takes about 0.2 s: it is on PostgreSQL, not skipped).

- [ ] **Step 8: Map the two codes**

In `internal/api/tenant_handlers.go`, replace

```go
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "An application with the destination's name already exists", "code": "application_name_taken"})
```

with

```go
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Other applications hold the destination's name and each suffix up to (9)", "code": "application_name_taken"})
	case errors.Is(err, store.ErrDestinationNameTooLong):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The destination's name would be longer than 255 characters; rename the cluster", "code": "destination_name_too_long"})
	case errors.Is(err, store.ErrInventoryStale):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The cluster's inventory is stale; wait for its next report and try again", "code": "inventory_stale"})
```

Run: `go vet ./internal/api/ && go test -count=1 -run 'TestMigration' ./internal/api/`
Expected: `ok` (the existing migration routes still answer as before).

- [ ] **Step 9: The web sentences**

In `web/src/components/ApplicationMigration.test.tsx`, in the test `has a sentence for every generated code, and no other`, replace `'application_name_taken', 'migration_not_ready',` with `'application_name_taken', 'destination_name_too_long', 'inventory_stale', 'migration_not_ready',`.

Run: `cd web && npx vitest run --maxWorkers=1 src/components/ApplicationMigration.test.tsx; cd ..`
Expected: FAIL (`MIGRATION_ERRORS[code]` undefined for the two codes). If `web/node_modules` is missing, run `cd web && npm ci --no-audit --no-fund; cd ..` first (it is gitignored).

In `web/src/components/ApplicationMigration.tsx`, in `MIGRATION_ERRORS`, replace

```ts
  application_name_taken: 'An application named like the destination already exists. Rename or discard it first.',
```

with

```ts
  application_name_taken: 'Applications already hold the destination name and every suffix up to (9). Discard one of them first.',
  destination_name_too_long: "The destination's name, the source's name plus \" on \" and the cluster's, would be longer than 255 characters. Rename the cluster.",
  inventory_stale: "The cluster's inventory is stale. Wait for the agent's next report, then try again.",
```

Run: `cd web && npx vitest run --maxWorkers=1 src/components/ApplicationMigration.test.tsx; cd ..`
Expected: `Tests  10 passed (10)`.

- [ ] **Step 10: DOX**

In `internal/api/AGENTS.md`, replace

````text
New `tenantError` codes: 409 `migration_open`, `migration_not_ready`, `migration_state`, `migration_stale`, `application_name_taken`;
````

with

````text
New `tenantError` codes: 409 `migration_open`, `migration_not_ready`, `migration_state`, `migration_stale`, `application_name_taken`, `destination_name_too_long`, `inventory_stale`;
````

In `internal/store/AGENTS.md`, replace

````text
and choices name the analyzed revision's named volumes
````

with

````text
and choices (`CheckMigrationChoices`, which the API also calls before it inspects) name the analyzed revision's named volumes
````

In `internal/store/AGENTS.md`, replace

````text
a revoked or offline cluster must not receive a destination application holding copied secrets.
````

with

````text
a revoked or offline cluster must not receive a destination application holding copied secrets. It reads the endpoint's inventory only when fresh (`freshInventory`, else `ErrInventoryStale`), for every step that reads the destination, and given the source's name picks the destination's (`destinationName`): `<name> on <endpoint>`, then ` (2)` to ` (9)` (`MaxDestinationSuffix`), the first no application in the environment is named and whose `KubernetesProject` no instance on the cluster holds, so the analysis and the creation name it alike and an abandoned destination can stay; a candidate over 255 bytes is `ErrDestinationNameTooLong`, all nine held `ErrApplicationNameTaken`. `ReadOpenMigration` reads the open migration under `application.migrate` (nil when none) for the API's checks before it inspects.
````

In `internal/store/AGENTS.md`, replace

````text
creates `<name> on <endpoint>` (`ErrApplicationNameTaken` when the name or its project on the cluster is taken) through
````

with

````text
creates the destination under `destinationName` through
````

In `internal/store/AGENTS.md`, replace

````text
because the cluster, not the revision history, knows which claims exist.
````

with

````text
because the cluster, not the revision history, knows which claims exist. `Deployment.RetainedClaims` (`retained_claims`) is derived on read (`retainedClaims`, no column): for a cluster removal, a service's planned `ClaimMounts` when the agent reported exactly that many retained steps for it, and none of that service's otherwise, since a retained step names no claim.
````

- [ ] **Step 11: Commit**

```bash
gofmt -l cmd internal && git add internal/store/application_deployment.go internal/store/application_migration.go internal/store/store.go internal/store/kubernetes_deployment_test.go internal/store/application_migration_test.go internal/store/AGENTS.md internal/api/tenant_handlers.go internal/api/AGENTS.md web/src/components/ApplicationMigration.tsx web/src/components/ApplicationMigration.test.tsx && make tidy-check lint && git commit -m "fix(migration): suffix the destination name, refuse a stale destination inventory and an overlong name; name retained claims" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: API — permission and state before any inspection

**Files:**
- Modify: `internal/api/migration_handlers.go` (`analyzeMigration`, `sourceMigration`, `handleStartMigration`, `reanalyze`)
- Test: `internal/api/migration_test.go` (canary on audit rows in `TestMigrationOverTheAPI`; new `countInspections`, `TestMigrationRefusesBeforeInspecting`, `TestMigrationMutationsCheckPermissionFirst`, `TestMigrationAnalysisRespectsTheInspectionBudget`)
- Test: `internal/api/kubernetes_deploy_test.go` (`TestRuntimeGateMatrix` migration row)
- Docs: `internal/api/AGENTS.md`

**Interfaces:**
- Consumes (Task 2): `store.TenancyStore.ReadOpenMigration(ctx, access, applicationID) (*store.ApplicationMigration, error)` (nil, nil when none), `store.CheckMigrationChoices(spec store.ApplicationSpec, classes []protocol.StorageClass, choices store.MigrationChoices) error`, `store.MigrationAnalyzed`, `store.ErrMigrationOpen`, `store.ErrMigrationState`. Existing test helpers: `migrationFleet`, `readReport`, `verifiedObservation`, `loginAs`, `tenantRequest`, `api.SetPlanInspectorForTest`, `api.AllowAttemptForTest`, `clusterHost.do`.
- Produces: no new exported name. Order of refusals on the migration routes: permission (403), then state (`migration_open`, `migration_state`, 409), then namespace and choices (400), then inspections.

- [ ] **Step 1: Write the tests**

In `internal/api/migration_test.go`, add `"sync"` to the imports (between `"strings"` and `"testing"`). In `TestMigrationOverTheAPI`, directly below

```go
	if strings.Contains(h.do(t, "GET", app+"/migration", "", 200), migrationCanary) || strings.Contains(h.do(t, "GET", dest+"/deployments", "", 200), migrationCanary) {
		t.Fatal("a secret value reached a response")
	}
```

insert

```go
	records, _, err := h.st.Audit().ListAuditRecords(context.Background(), 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range records {
		if strings.Contains(row.Resource, migrationCanary) || strings.Contains(row.Details, migrationCanary) {
			t.Fatalf("a secret value reached the audit trail: %+v", row)
		}
	}
```

Append to the end of the file:

```go

// countInspections replaces the fleet's plan-time inspector with one that counts its calls.
func countInspections(h clusterHost) func() int {
	var mu sync.Mutex
	calls := 0
	api.SetPlanInspectorForTest(h.s, func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		o := verifiedObservation(target)
		o.Health = "none"
		return o, nil
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// A refusal the migration's state or the request itself decides costs no inspection: a second
// start while one is open, invalid choices, and analyze or choices once the destination exists.
func TestMigrationRefusesBeforeInspecting(t *testing.T) {
	h, app, _ := migrationFleet(t)
	inspected := countInspections(h)
	body, _ := json.Marshal(store.MigrationStart{DestinationEndpointID: h.ag.id, Namespace: "shop"})
	h.do(t, "POST", app+"/migration", string(body), 201)
	before := inspected()
	if before != 2 {
		t.Fatalf("the analysis inspected %d containers", before)
	}
	if b := h.do(t, "POST", app+"/migration", string(body), 409); !strings.Contains(b, "migration_open") {
		t.Fatalf("a second start: %s", b)
	}
	for choice, code := range map[string]string{
		`{"volumes":{"data":{"storage_class":"gold","size":"1Gi","access_mode":"ReadWriteOnce"}}}`:     "storage_class_unknown",
		`{"volumes":{"data":{"storage_class":"standard","size":"1G","access_mode":"ReadWriteOnce"}}}`:  "size_invalid",
		`{"volumes":{"logs":{"storage_class":"standard","size":"1Gi","access_mode":"ReadWriteOnce"}}}`: "volume_unknown",
	} {
		if b := h.do(t, "PUT", app+"/migration/choices", choice, 400); !strings.Contains(b, code) {
			t.Errorf("%s: %s", code, b)
		}
	}
	h.do(t, "PUT", app+"/migration/choices", `{"volumes":{},"acknowledged":["volume_named"]}`, 400)
	if inspected() != before {
		t.Fatalf("a refused request inspected: %d", inspected()-before)
	}
	h.do(t, "PUT", app+"/migration/choices", `{"volumes":{"data":{"storage_class":"","size":"1Gi","access_mode":"ReadWriteOnce"}},"acknowledged":["network_references","port_unpublished"]}`, 200)
	h.do(t, "POST", app+"/migration/destination", "", 201)
	before = inspected()
	if b := h.do(t, "POST", app+"/migration/analyze", "", 409); !strings.Contains(b, "migration_state") {
		t.Fatalf("analyze after the destination: %s", b)
	}
	if b := h.do(t, "PUT", app+"/migration/choices", `{"volumes":{}}`, 409); !strings.Contains(b, "migration_state") {
		t.Fatalf("choices after the destination: %s", b)
	}
	if inspected() != before {
		t.Fatal("a refused state inspected")
	}
}

// A member who may not migrate is refused 403, with a denied application.migrate row, on choices
// and analyze whether or not the application has a migration: never told 404 first.
func TestMigrationMutationsCheckPermissionFirst(t *testing.T) {
	h, app, _ := migrationFleet(t)
	envAdmin := loginAs(t, h.s, h.st, "envadmin", "user")
	if err := h.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path, body string }{
		{"PUT", app + "/migration/choices", `{"volumes":{}}`},
		{"POST", app + "/migration/analyze", ""},
	} {
		if w := tenantRequest(h.s, envAdmin, route.method, route.path, route.body, true); w.Code != 403 {
			t.Errorf("%s %s with no migration: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	records, _, err := h.st.Audit().ListAuditRecords(context.Background(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	denied := 0
	for _, row := range records {
		if row.UserID == "usr_envadmin" && row.Action == "application.migrate" && row.Result == "denied" {
			denied++
		}
	}
	if denied != 2 {
		t.Fatalf("denied application.migrate rows: %d", denied)
	}
}

// An analysis past the actor's inspection budget inspects nothing and reports the probes and
// flags axes unknown (inspection_unavailable); it is never an extra inspection.
func TestMigrationAnalysisRespectsTheInspectionBudget(t *testing.T) {
	h, app, _ := migrationFleet(t)
	inspected := countInspections(h)
	for range 30 {
		api.AllowAttemptForTest(h.s, "inspection:usr_deployer", 30, time.Minute)
	}
	body, _ := json.Marshal(store.MigrationStart{DestinationEndpointID: h.ag.id, Namespace: "shop"})
	var m store.ApplicationMigration
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/migration", string(body), 201)), &m); err != nil {
		t.Fatal(err)
	}
	if inspected() != 0 {
		t.Fatalf("an analysis over the budget inspected %d containers", inspected())
	}
	if !slices.ContainsFunc(readReport(t, m).Services[0].Findings, func(f migration.Finding) bool { return f.Code == "inspection_unavailable" }) {
		t.Fatalf("report %+v", readReport(t, m))
	}
}
```

In `internal/api/kubernetes_deploy_test.go`, in `TestRuntimeGateMatrix`, directly below the first `h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)` insert:

```go
	// A migration's source must be on Docker: an application mapped to the cluster cannot start one.
	if w := tenantRequest(h.s, h.admin, "POST", app+"/migration", `{"destination_endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, true); w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
		t.Errorf("migration of a cluster application: %d %s", w.Code, w.Body.String())
	}
```

- [ ] **Step 2: Run them to see them fail**

Run: `gofmt -l internal && go test -count=1 -run 'TestMigration|TestRuntimeGateMatrix' ./internal/api/`
Expected: FAIL in `TestMigrationRefusesBeforeInspecting` (`a refused request inspected: 10`) and `TestMigrationMutationsCheckPermissionFirst` (`with no migration: 404`, `denied application.migrate rows: 0`). The budget, canary and gate-matrix assertions pass: they pin behaviour that already holds.

- [ ] **Step 3: Check permission, state and choices first**

In `internal/api/migration_handlers.go`, replace the `analyzeMigration` doc comment and its first check:

```go
// analyzeMigration reads app's migration inputs against the cluster endpoint, inspects the
// mapped containers through the plan-time primitive (the plan's per-actor budget, results used
// once and dropped), and runs the analyzer with choices. It writes the refusal and reports false
// when the inputs cannot be read.
func (s *Server) analyzeMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess, app, endpoint, namespace string, choices store.MigrationChoices) (*store.MigrationAnalysis, bool) {
	src, err := s.store.Tenancy().ReadMigrationSource(r.Context(), a, app, endpoint)
	if err == nil && !slices.Contains(src.Destination.Namespaces, namespace) {
		err = store.ErrNamespaceUnknown // before any inspection is spent; the store checks again
	}
	if err != nil {
```

with

```go
// analyzeMigration reads app's migration inputs against the cluster endpoint, refuses a namespace
// the cluster does not grant and invalid choices, then inspects the mapped containers through the
// plan-time primitive (the plan's per-actor budget, results used once and dropped), and runs the
// analyzer with choices. It writes the refusal and reports false when the inputs cannot be read.
func (s *Server) analyzeMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess, app, endpoint, namespace string, choices store.MigrationChoices) (*store.MigrationAnalysis, bool) {
	src, err := s.store.Tenancy().ReadMigrationSource(r.Context(), a, app, endpoint)
	if err == nil && !slices.Contains(src.Destination.Namespaces, namespace) {
		err = store.ErrNamespaceUnknown // before any inspection is spent; the store checks again
	}
	if err == nil {
		err = store.CheckMigrationChoices(src.Spec, src.Destination.StorageClasses, choices)
	}
	if err != nil {
```

Replace

```go
// sourceMigration reads app's open migration as its source; a destination has nothing to change.
func (s *Server) sourceMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) (*store.ApplicationMigration, bool) {
	m, err := s.store.Tenancy().ReadMigration(r.Context(), a, r.PathValue("application"))
	if err == nil && m.Role != "source" {
		err = store.ErrNotFound
	}
```

with

```go
// sourceMigration reads app's open migration as its source under application.migrate, so a
// member who may not migrate is refused before learning whether one exists; a destination has
// nothing to change.
func (s *Server) sourceMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) (*store.ApplicationMigration, bool) {
	m, err := s.store.Tenancy().ReadOpenMigration(r.Context(), a, r.PathValue("application"))
	if err == nil && m == nil {
		err = store.ErrNotFound
	}
```

In `handleStartMigration`, replace

```go
	app := r.PathValue("application")
	an, ok := s.analyzeMigration(w, r, a, app, input.DestinationEndpointID, input.Namespace, store.MigrationChoices{})
```

with

```go
	app := r.PathValue("application")
	// An open migration refuses the start before any inspection is spent; the store checks again.
	open, err := s.store.Tenancy().ReadOpenMigration(r.Context(), a, app)
	if err == nil && open != nil {
		err = store.ErrMigrationOpen
	}
	if err != nil {
		s.tenantError(w, err)
		return
	}
	an, ok := s.analyzeMigration(w, r, a, app, input.DestinationEndpointID, input.Namespace, store.MigrationChoices{})
```

(`m, err := s.store.Tenancy().CreateMigration(...)` below still compiles: `m` is new.)

Replace

```go
// reanalyze analyzes the open migration again with choices, or its stored ones when nil.
func (s *Server) reanalyze(w http.ResponseWriter, r *http.Request, a store.TenantAccess, choices *store.MigrationChoices) {
	m, ok := s.sourceMigration(w, r, a)
	if !ok {
		return
	}
```

with

```go
// reanalyze analyzes the open migration again with choices, or its stored ones when nil. Only an
// analyzed migration takes choices; any other status is refused before an inspection is spent.
func (s *Server) reanalyze(w http.ResponseWriter, r *http.Request, a store.TenantAccess, choices *store.MigrationChoices) {
	m, ok := s.sourceMigration(w, r, a)
	if !ok {
		return
	}
	if m.Status != store.MigrationAnalyzed {
		s.tenantError(w, store.ErrMigrationState)
		return
	}
```

- [ ] **Step 4: Run the tests**

Run: `gofmt -l internal && go vet ./internal/api/ && go test -count=1 -run 'TestMigration|TestRuntimeGateMatrix|TestViewerOnClusterRoutes' ./internal/api/`
Expected: no gofmt output, then `ok` (`TestMigrationOverTheAPI`, `TestMigrationAbandonKeepsTheDestination`, `TestMigrationRolesAndRuntimes`, the three new tests, `TestRuntimeGateMatrix`).

- [ ] **Step 5: DOX**

In `internal/api/AGENTS.md`, replace

````text
Start, choices and analyze read `store.ReadMigrationSource` (under `application.migrate`, so nobody else causes an inspection), refuse a namespace the cluster does not grant (400 `namespace_unknown`) before any inspection,
````

with

````text
Start first reads `store.ReadOpenMigration` (409 `migration_open` when one is open); choices and analyze read the open migration through `sourceMigration` (`ReadOpenMigration`, under `application.migrate`, so a member who may not migrate gets 403 and a denied audit row, never 404) and refuse any status but `analyzed` (409 `migration_state`). All three then read `store.ReadMigrationSource` (under `application.migrate`, so nobody else causes an inspection), refuse a namespace the cluster does not grant (400 `namespace_unknown`) and invalid choices (`store.CheckMigrationChoices`) before any inspection,
````

In `internal/api/AGENTS.md`, replace

````text
`migration_test.go` the whole migration over the API with a Docker source and the fake cluster agent;
````

with

````text
`migration_test.go` the whole migration over the API with a Docker source and the fake cluster agent (no secret in a response or an audit row), the refusals that cost no inspection, permission before 404 on the mutations, and the inspection budget;
````

- [ ] **Step 6: Commit**

```bash
gofmt -l cmd internal && git add internal/api/migration_handlers.go internal/api/migration_test.go internal/api/kubernetes_deploy_test.go internal/api/AGENTS.md && make tidy-check lint && git commit -m "fix(migration): check permission, state and choices before spending an inspection" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Web — the claim reason and "Kept on the cluster"; rebuild `web/dist`

**Files:**
- Modify: `web/src/components/ApplicationDeploymentPlan.tsx` (`Deployment` type, `stepText`, new `CLAIM_PHASES`, new `KeptClaims`, `ResultSection`)
- Test: `web/src/components/ApplicationDeploymentPlan.test.tsx`
- Build: `web/dist`, `web/tsconfig.tsbuildinfo`
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes: the deployment row's `retained_claims?: string[]` (Task 2), a `rollout_timeout` detail carrying `claim=Pending` or `claim=Lost` (Task 1), existing `stepText`, `STEP_CODES`, `CLAIM_RETAINED`, `ResultSection`.
- Produces: `stepText({code: 'rollout_timeout', detail})` appends `; a volume claim is still Pending: no StorageClass provisioned it` or `; a volume claim is Lost: the volume behind it is gone` (once per phase); a settled cluster removal's result shows the heading "Kept on the cluster", one `PersistentVolumeClaim <namespace>/<name>` item per `retained_claims` entry and, when the agent reported more retained steps than names, `<n> more kept claim(s) are labelled for this instance: kubectl -n <namespace> get pvc -l kyyard.busnes.app/instance=<instance_id>`.

- [ ] **Step 1: Write the tests**

In `web/src/components/ApplicationDeploymentPlan.test.tsx`, in the test `names an immutable claim, shows the claims a plan creates, and says a removal kept its claims`, replace

```ts
  const removal = { ...plan, id: 'd2', kind: 'remove', state: 'succeeded', plan: { project: 'shop', namespace: 'shop' },
    result: { steps: [{ service: 'web', step: 'remove', outcome: 'succeeded', detail: '' }, { service: 'web', step: 'volume', outcome: 'skipped', detail: 'retained' }], services: [] } };
```

with

```ts
  const removal = { ...plan, id: 'd2', kind: 'remove', state: 'succeeded', plan: { project: 'shop', namespace: 'shop' }, retained_claims: ['shop-data'],
    result: { steps: [{ service: 'web', step: 'remove', outcome: 'succeeded', detail: '' }, { service: 'web', step: 'volume', outcome: 'skipped', detail: 'retained' }, { service: 'api', step: 'volume', outcome: 'skipped', detail: 'retained' }], services: [] } };
```

and replace its last lines

```ts
  expect(await screen.findByText(CLAIM_RETAINED)).toBeTruthy();
  expect(screen.getByText(/Its PersistentVolumeClaims and their data are kept/)).toBeTruthy();
});
```

with

```ts
  expect((await screen.findAllByText(CLAIM_RETAINED)).length).toBe(2);
  expect(screen.getByText(/Its PersistentVolumeClaims and their data are kept/)).toBeTruthy();
  expect(screen.getByRole('heading', { name: 'Kept on the cluster' })).toBeTruthy();
  expect(screen.getByText('PersistentVolumeClaim shop/shop-data')).toBeTruthy();
  expect(screen.getByText(/1 more kept claim\(s\) are labelled for this instance/)).toBeTruthy();
  expect(screen.getByText('kubectl -n shop get pvc -l kyyard.busnes.app/instance=i')).toBeTruthy();
});
it('names an unbound claim in a rollout timeout and drops anything else', () => {
  const text = STEP_CODES.rollout_timeout;
  expect(stepText({ code: 'rollout_timeout', detail: 'progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable,claim=Pending' }))
    .toBe(`${text} (progressing ReplicaSetUpdated, available MinimumReplicasUnavailable); a volume claim is still Pending: no StorageClass provisioned it.`);
  expect(stepText({ code: 'rollout_timeout', detail: 'claim=Lost' })).toBe(`${text}; a volume claim is Lost: the volume behind it is gone.`);
  expect(stepText({ code: 'rollout_timeout', detail: 'claim=Bound,claim=<b>x</b>' })).toBe(`${text}.`);
});
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd web && npx vitest run --maxWorkers=1 src/components/ApplicationDeploymentPlan.test.tsx; cd ..`
Expected: FAIL in both tests (no "Kept on the cluster" heading; the claim reason is dropped: `... (progressing ReplicaSetUpdated, available MinimumReplicasUnavailable).`).

- [ ] **Step 3: Render the claim reason and the kept claims**

In `web/src/components/ApplicationDeploymentPlan.tsx`:

In the `type Deployment = { ... }` line, replace `validation?: Validation; migration_id?: string };` with `validation?: Validation; migration_id?: string; retained_claims?: string[] };`.

In `stepText`, replace

```ts
    case 'rollout_timeout': {
      const reasons = detail.split(',').filter((r) => ROLLOUT.test(r)).map((r) => r.replace('=', ' '));
      return reasons.length ? `${text} (${reasons.join(', ')}).` : `${text}.`;
    }
```

with

```ts
    case 'rollout_timeout': {
      const parts = detail.split(',');
      const reasons = parts.filter((r) => ROLLOUT.test(r)).map((r) => r.replace('=', ' '));
      const claims = [...new Set(parts.filter((r) => Object.hasOwn(CLAIM_PHASES, r)))].map((r) => CLAIM_PHASES[r]);
      const base = reasons.length ? `${text} (${reasons.join(', ')})` : text;
      return claims.length ? `${base}; ${claims.join('; ')}.` : `${base}.`;
    }
```

Directly above `// The closed detail shapes of the Kubernetes codes: Kind/name, and condition=Reason words.` insert:

```ts
// A rollout_timeout's claim reasons: a planned claim the cluster has not bound.
const CLAIM_PHASES: Record<string, string> = {
  'claim=Pending': 'a volume claim is still Pending: no StorageClass provisioned it',
  'claim=Lost': 'a volume claim is Lost: the volume behind it is gone',
};
```

(`ROLLOUT` stays as it is: `claim` is not one of its prefixes, so a claim reason never renders as a condition.)

In `ResultSection`, directly above `{current.result.services.length > 0 && <ul className="ky-list">` insert `<KeptClaims d={current} />` on its own line (same indentation), and directly above `function PlanDetails({ d }: { d: Deployment }) {` insert:

```tsx
// KeptClaims lists what a cluster removal left on the cluster: the claims the server can name, and
// how many more the agent reported, with the command that lists them all.
function KeptClaims({ d }: { d: Deployment }) {
  if (d.kind !== 'remove' || !d.plan.namespace || !d.result) return null;
  const kept = d.result.steps.filter(s => s.step === 'volume' && s.outcome === 'skipped' && s.detail === 'retained').length;
  const named = d.retained_claims ?? [];
  if (kept === 0) return null;
  return <>
    <h4>Kept on the cluster</h4>
    {named.length > 0 && <ul className="ky-list">{named.map(c => <li key={c}>PersistentVolumeClaim {d.plan.namespace}/{c}</li>)}</ul>}
    {kept > named.length && <p>{kept - named.length} more kept claim(s) are labelled for this instance: <code>kubectl -n {d.plan.namespace} get pvc -l kyyard.busnes.app/instance={d.instance_id}</code></p>}
  </>;
}
```

`ResultSection` also renders each row of the deployment history, so a removed application's history shows the list.

- [ ] **Step 4: Run the tests and the typecheck**

Run: `cd web && npx vitest run --maxWorkers=1 src/components/ApplicationDeploymentPlan.test.tsx && npx vitest run --maxWorkers=1 src/components/ApplicationMigration.test.tsx && npx tsc -b; cd ..`
Expected: `Tests  41 passed (41)`, `Tests  10 passed (10)`, and `tsc` silent.

- [ ] **Step 5: Rebuild the embedded bundle**

Nothing else may be running (no test process, no `make ci`). Run: `make build-web`
Expected: exits 0; `git status --short web/dist web/tsconfig.tsbuildinfo` lists the rebuilt files.

- [ ] **Step 6: DOX**

In `web/AGENTS.md`, replace

````text
a removal's kept claim reads `CLAIM_RETAINED`, a cluster removal says its claims are kept,
````

with

````text
a removal's kept claim reads `CLAIM_RETAINED`, a cluster removal says its claims are kept and its result lists them under "Kept on the cluster" (`KeptClaims`: each `retained_claims` name, and when the agent reported more retained steps than names, their count with `kubectl -n <namespace> get pvc -l kyyard.busnes.app/instance=<instance>`), a `rollout_timeout` detail's `claim=Pending` or `claim=Lost` appends its `CLAIM_PHASES` sentence,
````

- [ ] **Step 7: Commit**

```bash
git add web/src/components/ApplicationDeploymentPlan.tsx web/src/components/ApplicationDeploymentPlan.test.tsx web/dist web/tsconfig.tsbuildinfo web/AGENTS.md && make tidy-check lint && git commit -m "feat(web): name an unbound claim in a rollout timeout; list the claims a removal kept" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Documents, then the gate

**Files:**
- Modify: `docs/agent-protocol.md`, `docs/application-schema.md`, `docs/authorization-matrix.md`, `README.md`
- The gate changes nothing; a failure goes back to the task that owns the file.

**Interfaces:**
- Consumes: every commit of Tasks 1–4.
- Produces: the evidence the PR description quotes.

- [ ] **Step 1: Edit the documents**

Make exactly these ten replacements. Each `old` block occurs once in its file; line breaks inside a block are the file's own.

1. `docs/agent-protocol.md`, replace

````text
detail at most three `progressing|available|replicafailure|pod=<Reason>`).
````

   with

````text
detail at most three `progressing|available|replicafailure|pod=<Reason>` or `claim=Pending|Lost`, a planned claim of the service the cluster has not bound; a server older than this refuses a `claim=` reason, so upgrade the server first).
````

2. `docs/agent-protocol.md`, replace

````text
that is the only step with an outcome of `skipped` that carries a detail.
````

   with

````text
that is the only step with an outcome of `skipped` that carries a detail. The step names no claim; the server names them from the removal plan (docs/application-schema.md, Claims).
````

3. `docs/application-schema.md`, replace

````text
Removal reports each claim as a skipped `volume` step with detail `retained`; deleting the data is the operator's deliberate `kubectl delete pvc`.
````

   with

````text
Removal reports each claim as a skipped `volume` step with detail `retained`; deleting the data is the operator's deliberate `kubectl delete pvc`. A step names no claim, so the removal row's `retained_claims`, derived on read, names a service's planned claims only when the agent reported exactly that many for it, and the page lists them under "Kept on the cluster" with the label selector for any it cannot name. A rollout that times out names each planned claim still `Pending` or `Lost` (`claim=<phase>`).
````

4. `docs/application-schema.md`, replace

````text
- The destination is a new application `<name> on <endpoint>`:
````

   with

````text
- The destination is a new application `<name> on <endpoint>`, or that name with ` (2)` to ` (9)` when it or its project is held, so an abandoned destination can stay (all nine held is `application_name_taken`, any candidate over 255 bytes `destination_name_too_long`; the analysis already names the destination this way and refuses the same):
````

5. `docs/application-schema.md`, replace

````text
- Storage choices are held to the analyzed revision's named volumes and the StorageClasses the destination reports
````

   with

````text
- Every step that reads the destination reads its inventory only while fresh (the plan's rule; otherwise `inventory_stale`). A refusal decided by permission, the migration's status (`migration_open`, `migration_state`) or the request's choices comes before any inspection is spent.
- Storage choices are held to the analyzed revision's named volumes and the StorageClasses the destination reports
````

6. `docs/authorization-matrix.md`, replace

````text
reading a migration is `application.read` |
````

   with

````text
reading a migration is `application.read`, while choices and analyze read the open migration under `application.migrate`, so any other role is refused with a denied row before it learns whether one exists |
````

7. `README.md`, replace

````text
(`FailedCreate`, `ImagePullBackOff`, `CrashLoopBackOff`, ...) and leaves the objects as applied.
````

   with

````text
(`FailedCreate`, `ImagePullBackOff`, `CrashLoopBackOff`, ..., or a volume claim still `Pending`
because no StorageClass provisioned it) and leaves the objects as applied.
````

8. `README.md`, replace

````text
choices when you edit its definition later; a volume it newly declares has none, so remove it or
migrate again.
````

   with

````text
choices when you edit its definition later; a volume it newly declares has none, so remove it or
migrate again. Analysis and choices read the cluster's inventory only while it is fresh (reported
in the last three minutes); otherwise they answer `inventory_stale`.
````

9. `README.md`, replace

````text
is no longer needed. Health probes,
````

   with

````text
is no longer needed; the removal's result lists them under **Kept on the cluster**. Health probes,
````

10. `README.md`, replace

````text
The destination application keeps its name after an abandoned migration: a new migration's
destination creation answers `application_name_taken` until the old destination is removed.
````

   with

````text
An abandoned migration's destination stays under its name; a new migration names its destination
`<name> on <cluster> (2)`, then `(3)`, up to `(9)`, and answers `application_name_taken` past that
(remove or discard an old destination). A destination name longer than 255 characters is refused
(`destination_name_too_long`): rename the cluster.
````

Check each landed once: `grep -c 'claim=Pending|Lost' docs/agent-protocol.md` prints `1`; `grep -c 'retained_claims' docs/application-schema.md` prints `1`; `grep -c 'inventory_stale' README.md docs/application-schema.md` prints `1` for each; `grep -c 'destination_name_too_long' README.md docs/application-schema.md` prints `1` for each; `grep -c 'Kept on the cluster' README.md` prints `1`.

DOX: `README.md` and `docs/` are the root's operator documents (`KyYard-Server/AGENTS.md` names them); the root `AGENTS.md` needs no change (no child added, moved or renamed), and `docs/threat-model.md` is unchanged (no new trust boundary: the new reads are under permissions the matrix already grants).

- [ ] **Step 2: Commit**

```bash
git add docs/agent-protocol.md docs/application-schema.md docs/authorization-matrix.md README.md && git commit -m "docs(migration): destination naming, fresh-inventory and permission-first rules, kept claims" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

- [ ] **Step 3: The gate (controller only, one command at a time, nothing else running)**

Run: `make tidy-check lint`
Expected: exits 0.

Run: `go test -race -p 2 ./...`
Expected: every package `ok`.

Run: `cd web && npx vitest run --maxWorkers=2; cd ..`
Expected: every test file passes. If `ApplicationDeploymentPlan.test.tsx > keeps the panel mounted across polls instead of flashing loading` fails alone, it is the known pre-existing flake (one read counter shared by two GETs whose order varies); rerun that file with `--maxWorkers=1` and report it if it fails again.

Run: `make smoke`
Expected: `smoke test: all checks passed`.

Run PostgreSQL per package, one at a time: `for p in $(go list ./internal/...); do PG=… go test -count=1 -p 1 $p || break; done` (with the `PG=…` prefix expanded as in Global Constraints)
Expected: every package `ok` (packages without a database test print `ok` or `no test files`).

- [ ] **Step 4: Boundaries**

Run: `go list -deps ./cmd/server | grep -c k8s.io`
Expected: `0`.

Run: `git diff --quiet bac5ffb -- internal/store/migrations && echo no-migration`
Expected: `no-migration`.

Run: `git diff --quiet bac5ffb -- web/src/protocol-codes.json web/src/migration-codes.json && echo fixtures-unchanged`
Expected: `fixtures-unchanged` (no step, unsupported or analyzer code was added).

Run: `git status --short web/dist web/tsconfig.tsbuildinfo`
Expected: nothing. If `npm ci` in the gate rewrote `tsconfig.tsbuildinfo`, rerun `make build-web` alone, check `git status` again, and commit the result as a follow-up of Task 4 rather than leaving it dirty.

- [ ] **Step 5: DOX closeout**

Re-read the chain `busness.app/AGENTS.md` → `KyYard-Server/AGENTS.md` → `internal/runtime/kubernetes/AGENTS.md`, `internal/agent/AGENTS.md`, `internal/store/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md`, and confirm each names what Tasks 1–4 landed and nothing stale remains: `grep -rn "until the old destination is removed\|An application with the destination's name already exists\|checkChoices" --include='*.md' --include='*.go' --include='*.tsx' . | grep -v -e docs/superpowers -e node_modules` prints nothing.
