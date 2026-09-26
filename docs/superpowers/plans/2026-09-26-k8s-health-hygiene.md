# Hygiene after Kubernetes Health Validation (#79) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the five minors the #79 reviews deferred: `Prune` keeps every succeeded apply of an instance while a rollback of it is undecided, `PendingValidations` skips the binding and inventory reads a cluster row discards, the "no registry call" rollback claim and the registry-row gate on a pinned cluster rollback are asserted, and the web health warning names its runtimes explicitly. No new feature, no schema change.

**Architecture:** Four tasks by area. Task 1 changes one `Prune` clause (`internal/store/samples.go`) and the documents that state the prune rule. Task 2 moves the cluster short-circuit in `PendingValidations` (`internal/store/validations.go`) ahead of the two reads and adds a store test of the registry gate on a pinned cluster plan. Task 3 is API tests only (`internal/api`). Task 4 is `HealthWarning` in `web/src/components/ApplicationPolicy.tsx` and the rebuilt `web/dist`.

**Tech Stack:** Go 1.26 (module `go 1.26.6`), SQLite + PostgreSQL 17, React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-26-k8s-health-hygiene-design.md`

## Global Constraints

- Work only in the worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/k8s-hygiene` (branch `chore/k8s-health-hygiene` off `master` ca8f0ab; the spec is 154b60c). Every command below runs from its root. Never `cd` to the main repository or another worktree.
- Never `git stash`, never `git add -A` or `git add .`: add the exact paths each commit step lists.
- Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Gate each commit on the previous command's exit (`&&`).
- `gofmt -w` every edited Go file and check `gofmt -l cmd internal` prints nothing before each commit. `make tidy-check lint` runs after `git add` (it diffs the working tree against the index) and before `git commit`.
- Memory: this machine has been killed by memory pressure. Run only the focused tests each step names (`go test -count=1 -run <Names> ./one/package/`, `npx vitest run --maxWorkers=1 <one file>`), one test process at a time. Never `go test ./...`, never `-race`, never `make ci` in Tasks 1–4. The controller runs the full gate after Task 4, alone.
- Every store change is tested on SQLite and PostgreSQL. PostgreSQL runs use the local container `kyyard-access-pg`; read the password into a variable, never print it: `PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test -count=1 -p 1 -run <Name> ./internal/store/`. Below this is written `PG=… go test -count=1 -p 1 -run <Name> ./internal/store/`.
- No schema migration: `git diff --quiet ca8f0ab -- internal/store/migrations` must hold at the end.
- `web/dist` and `web/tsconfig.tsbuildinfo` change only in Task 4, rebuilt with `make build-web` with nothing else running.
- DOX: the task that changes a directory's contract updates that directory's `AGENTS.md` (and any document stating the changed rule) in the same commit.
- Root `AGENTS.md` and `docs/agent-protocol.md` stay unchanged: no wire, structure, workflow or child-index change.
- API tests assert the code's existing rollback outcome fields; Task 3 must not change production code.

Plan decisions where the spec's wording left a choice or disagreed with the code (proved in a scratch copy on both dialects; none widens scope):

1. **Prune test, row A.** The spec's test has "A (old, revision 1)" deleted during C's open window, but its own decision keeps *every succeeded apply* of the instance while a rollback is open, so a succeeded A would be kept. A is a **failed** apply (`historyRow(..., "failed", old)`): it shows the prune still runs during the window. B is the fixture's manual prior apply at the same revision as C (`validationFixture`'s `prior`, both at revision 1).
2. **The existing prune test inverts one assertion.** `TestPruneKeepsADeploymentWhoseRollbackIsOpen` asserts the prior apply is pruned while `updated`'s validation is open, which is exactly the bug. It now asserts the prior apply is kept while open and pruned once decided.
3. **No JOIN.** `deployment_validations` carries `instance_id` (indexed, `idx_deployment_validations_instance`), copied from the deployment at settle, so the clause is `EXISTS (SELECT 1 FROM deployment_validations v WHERE v.instance_id=d.instance_id AND rollbackOpen)` and it replaces the old per-deployment clause: the validated deployment is itself a succeeded apply of that instance.
4. **PendingValidations test.** The spec's test ("identical with and without an inventory") passes before the change, so it proves nothing. The test instead renames `endpoint_inventory` and `application_resources` away (each test has its own database or schema) and requires the cluster row to be listed unchanged: it fails today with `no such table: application_resources`. The `Kind == Deployment` placement in the Docker loop stays for a cluster identity whose endpoint row no longer reads `kubernetes` (runtime `''` through `COALESCE`).
5. **Registry gate: there is no row to delete.** Neither `kubernetesPlanFixture` (store) nor `newClusterHost` (API) creates a registry row; both turn anonymous pull on. The arrangement is anonymous pull turned off. The code's outcome: `PlanDeployment` returns `PreflightBlockedError{registry_not_configured}` before `MarkRollbackPlanned`, so **no rollback deployment is created**; the validation records `Rollback{DeploymentID: "", Revision: 0, Outcome: "failed", Detail: "registry_not_configured"}` and the policy pauses with `update failed and could not be rolled back: registry_not_configured`.
6. **Counter name.** `fakeDigests` gains field `heads int` and method `calls() int` (a field named `calls` would collide with the method). The harness keeps whichever resolver it last installed in `v.digests`; `automate` replaces it, so the rollback test reads the counter of the resolver live at rollback time and first requires it to be non-zero (the update check went through it).

## Review Focus

1. A failed apply of an instance whose rollback is open still ages out: protection covers succeeded applies only. Pinned in Task 1 (`TestPruneKeepsTheInstanceAppliesWhileARollbackIsOpen`, row `failed`).
2. Protection lifts the moment the rollback is decided: the prior apply, no longer its revision's newest, is pruned by the next pass. Pinned in Task 1 (same test, and the amended `TestPruneKeepsADeploymentWhoseRollbackIsOpen`).
3. A pinned cluster plan with anonymous pull on succeeds with zero registry calls; the gate refuses only when neither a row nor anonymous pull exists. Pinned in Task 2 (`TestKubernetesPinnedPlanNeedsARegistryRow`).
4. A rollback refused at plan leaves no deployment row behind (the endpoint is not occupied by a dead plan). Pinned in Task 3 (`TestClusterValidationRollbackNeedsARegistryRow`, the list stays at two rows).
5. A Docker host without `container.inspect.health` still warns and a cluster with `kubernetes.inspect` does not, after the branch is rewritten. Pinned by the existing tests in `ApplicationPolicy.test.tsx`, which Task 4 runs unchanged beside the new unknown-runtime test.

## File map

| Path | Task | Change |
|---|---|---|
| `internal/store/samples.go`, `internal/store/rollback_test.go`, `internal/store/AGENTS.md`, `docs/retention-policy.md`, `docs/application-schema.md` | 1 | Prune keeps an instance's succeeded applies while a rollback is open |
| `internal/store/validations.go`, `internal/store/validations_cluster_test.go`, `internal/store/kubernetes_deployment_test.go`, `internal/store/AGENTS.md` | 2 | Cluster rows skip both reads; pinned cluster plan registry gate test |
| `internal/api/image_check_handlers_test.go`, `internal/api/kubernetes_validation_test.go`, `internal/api/AGENTS.md` | 3 | Resolver call count; rollback registry gate test |
| `web/src/components/ApplicationPolicy.tsx` + `.test.tsx`, `web/dist`, `web/tsconfig.tsbuildinfo`, `web/AGENTS.md` | 4 | Explicit runtimes in `HealthWarning` |

---

### Task 1: Store — Prune keeps an instance's succeeded applies while a rollback is open

**Files:**
- Modify: `internal/store/samples.go:31-34` (`DeploymentHistoryRetention` comment), `:241-242` (the `deployments` clause)
- Test: `internal/store/rollback_test.go` (amend `TestPruneKeepsADeploymentWhoseRollbackIsOpen`, add `TestPruneKeepsTheInstanceAppliesWhileARollbackIsOpen`)
- Docs: `internal/store/AGENTS.md`, `docs/retention-policy.md` (Deployment history row), `docs/application-schema.md:132`

**Interfaces:**
- Consumes: `validationFixture(t) (*SQLStore, TenantAccess, *Application, string, *UpdatePolicy, string, *Deployment, *Deployment)` (prior manual apply, done healthy; updated automated apply, open, both revision 1), `historyRow(t, st, from string, revision int, state string, settled time.Time) string` (`application_history_test.go`), `mustExec(t, st, query, args...)`, constants `rollbackOpen` (`rollback.go`), `RollbackFailed`, `RollbackNotSent`, `VerdictUnhealthy`.
- Produces: the retention rule "while any validation of an instance is `rollbackOpen`, no succeeded apply of that instance is pruned". Nothing later tasks call.

- [ ] **Step 1: Write the failing test**

Append to `internal/store/rollback_test.go`:

```go
// While a validation of the instance may still roll back, no succeeded apply of the instance is
// pruned: a cluster rollback returns to the previous one, which a later apply at the same revision
// no longer keeps as its revision's newest. A failed apply still goes.
func TestPruneKeepsTheInstanceAppliesWhileARollbackIsOpen(t *testing.T) {
	st, _, _, _, _, _, prior, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	old := time.Now().UTC().Add(-DeploymentHistoryRetention - time.Hour)
	mustExec(t, st, `UPDATE deployments SET settled_at=? WHERE id=?`, old, prior.ID)
	failed := historyRow(t, st, prior.ID, 1, "failed", old)
	exists := func(id string) bool {
		t.Helper()
		var n int
		if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM deployments WHERE id=?`), id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(failed) || !exists(prior.ID) || !exists(updated.ID) {
		t.Fatalf("while open: failed %t prior %t updated %t", exists(failed), exists(prior.ID), exists(updated.ID))
	}
	if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
		t.Fatal(err)
	}
	if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackFailed, RollbackNotSent); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(prior.ID) || !exists(updated.ID) {
		t.Fatalf("once decided: prior %t updated %t", exists(prior.ID), exists(updated.ID))
	}
}
```

In `TestPruneKeepsADeploymentWhoseRollbackIsOpen` (same file), replace

```go
	if kept(prior.ID) {
		t.Fatal("a manual, validated deployment outlived its retention")
	}
```

with

```go
	if !kept(prior.ID) {
		t.Fatal("pruned the instance's earlier apply while a rollback was open")
	}
```

and replace its last check

```go
	if kept(updated.ID) {
		t.Fatal("an old, superseded deployment outlived its decided validation")
	}
```

with

```go
	if kept(updated.ID) || kept(prior.ID) {
		t.Fatal("an old, superseded deployment outlived its decided validation")
	}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestPruneKeepsTheInstanceAppliesWhileARollbackIsOpen|TestPruneKeepsADeploymentWhoseRollbackIsOpen' ./internal/store/`
Expected: FAIL, `while open: failed false prior false updated true` and `pruned the instance's earlier apply while a rollback was open`.

- [ ] **Step 3: Implement**

In `internal/store/samples.go`, in `Prune`, replace the comment and the tail of the `deployments` clause. The comment

```go
		// A deployment whose rollback is undecided stays: its validation row cascades with it.
```

becomes

```go
		// While a validation of the instance may still roll back, none of its succeeded applies goes:
		// the rollback returns to one of them, and the validation row cascades with its deployment.
```

and in the SQL string, the clause

```
AND NOT EXISTS (SELECT 1 FROM deployment_validations v WHERE v.deployment_id=d.id AND ` + rollbackOpen + `) LIMIT ?)`
```

becomes

```
AND NOT (d.kind='apply' AND d.state='succeeded' AND EXISTS (SELECT 1 FROM deployment_validations v WHERE v.instance_id=d.instance_id AND ` + rollbackOpen + `)) LIMIT ?)`
```

(everything before it in that string is unchanged). Replace the `DeploymentHistoryRetention` comment (`samples.go:31-33`)

```go
	// DeploymentHistoryRetention keeps settled deployments; the latest succeeded row of an
	// instance's current and previous revision, and every row of a removed application (its
	// own retention prunes them), stay past it.
```

with

```go
	// DeploymentHistoryRetention keeps settled deployments; the latest succeeded row of an
	// instance's current and previous revision, every succeeded apply of an instance while a
	// rollback of it is undecided, and every row of a removed application (its own retention
	// prunes them), stay past it.
```

- [ ] **Step 4: Run the tests on both dialects**

Run: `go test -count=1 -run 'TestPrune' ./internal/store/`
Expected: `ok`.
Run: `PG=… go test -count=1 -p 1 -run 'TestPrune' ./internal/store/`
Expected: `ok`.

- [ ] **Step 5: Documents**

`internal/store/AGENTS.md`, in the migration 25 bullet, replace

```
except the latest succeeded apply row at each of their instance's current and previous revisions and any row of a removed application,
```

with

```
except the latest succeeded apply row at each of their instance's current and previous revisions, every succeeded apply of an instance while one of its validations is `rollbackOpen`, and any row of a removed application,
```

and at the end of the Rollback (`rollback.go`) bullet replace

```
`Prune` keeps a deployment whose automated validation's rollback is undecided (`rollbackOpen`): its validation row cascades with it.
```

with

```
`Prune` keeps every succeeded apply of an instance while any validation of that instance is `rollbackOpen` (window open, decision owed or rollback in flight): a cluster rollback returns to the previous succeeded apply, which a later apply at the same revision no longer keeps as its revision's newest. The validation row cascades with its deployment.
```

`docs/retention-policy.md`, Deployment history row, replace

```
except the latest succeeded row at the instance's current and previous revision, a row whose automated validation's rollback is undecided, and any row of a removed application;
```

with

```
except the latest succeeded row at the instance's current and previous revision, every succeeded apply of an instance while any of its automated validations has an undecided rollback (the rollback returns to one of them), and any row of a removed application;
```

`docs/application-schema.md:132`, replace

```
a row whose automated validation's rollback is undecided (its validation cascades with it),
```

with

```
every succeeded apply of an instance while any of its automated validations has an undecided rollback (its validation cascades with its deployment),
```

Check each replacement landed once: `grep -c "while one of its validations is \`rollbackOpen\`" internal/store/AGENTS.md` prints `1`; `grep -c "undecided rollback" docs/retention-policy.md docs/application-schema.md` prints `1` for each.

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/store/samples.go internal/store/rollback_test.go && test -z "$(gofmt -l cmd internal)" && git add internal/store/samples.go internal/store/rollback_test.go internal/store/AGENTS.md docs/retention-policy.md docs/application-schema.md && make tidy-check lint && git commit -m "fix(store): keep an instance's succeeded applies while its rollback is open

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

### Task 2: Store — cluster rows skip the reads they discard; the registry gate on a pinned cluster plan

**Files:**
- Modify: `internal/store/validations.go:435-439` (`PendingValidations` doc comment), `:474-500` (the placement loop)
- Test: `internal/store/validations_cluster_test.go` (add `TestPendingValidationsOfAClusterReadsNoInventory`), `internal/store/kubernetes_deployment_test.go` (add `TestKubernetesPinnedPlanNeedsARegistryRow`)
- Docs: `internal/store/AGENTS.md` (health validation bullet)

**Interfaces:**
- Consumes: `kubernetesPlanFixture(t, spec, values) (*SQLStore, TenantAccess, *Application, string, *ApplicationMapping)` (anonymous pull on, no registry row), `twoServiceSpec()` (web `ghcr.io/org/web:1`, api `ghcr.io/org/api@<digestOf("a")>`), `clusterApply(t, st, a, app, cluster, digest) *Deployment`, `kubePlanRequest(m) PlanRequest`, `digestOf(string) string`, `setAnonymousPull(t, st, a, on bool)`, `isBlocked(err, blocker string) bool`, `fakeResolver{reply map[string]fakeReply}` with `called()`, `mustExec`, `imageCheckKey`.
- Produces: no new API. `PendingValidations` output is unchanged for every row; a cluster row (`p.Kubernetes`) no longer queries `application_resources` or `endpoint_inventory`.

- [ ] **Step 1: Write the failing test and the gate test**

Append to `internal/store/validations_cluster_test.go`:

```go
// A cluster row reads neither the instance's resource bindings nor the endpoint inventory: its
// Deployments are placed unknown for the status read to decide. With both tables renamed away the
// row is still listed, unchanged.
func TestPendingValidationsOfAClusterReadsNoInventory(t *testing.T) {
	st, a, app, cluster, _ := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	d := clusterApply(t, st, a, app, cluster, digestOf("b"))
	before, err := ts.PendingValidations(ctx)
	if err != nil || len(before) != 1 || before[0].DeploymentID != d.ID {
		t.Fatalf("pending: %+v %v", before, err)
	}
	mustExec(t, st, `ALTER TABLE endpoint_inventory RENAME TO endpoint_inventory_hidden`)
	mustExec(t, st, `ALTER TABLE application_resources RENAME TO application_resources_hidden`)
	after, err := ts.PendingValidations(ctx)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("without the tables: %+v %v, want %+v", after, err, before)
	}
	for _, s := range after[0].Services {
		if s.Presence != PresenceUnknown {
			t.Fatalf("service %+v", s)
		}
	}
}
```

Append to `internal/store/kubernetes_deployment_test.go` (behaviour unchanged; this pins it):

```go
// A pinned cluster plan (a rollback's) makes no registry call, but still needs the pulled host's
// registry row or anonymous pull: the kubelet pulls with what the frame carries.
func TestKubernetesPinnedPlanNeedsARegistryRow(t *testing.T) {
	st, a, app, _, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{}}
	pinned := kubePlanRequest(m)
	pinned.PinImages = map[string]string{"web": "ghcr.io/org/web@" + digestOf("b"), "api": "ghcr.io/org/api@" + digestOf("a")}
	setAnonymousPull(t, st, a, false)
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pinned, resolver, imageCheckKey, false); !isBlocked(err, "registry_not_configured") {
		t.Fatalf("pinned without a registry row: %v", err)
	}
	setAnonymousPull(t, st, a, true)
	d, err := ts.PlanDeployment(ctx, a, app.ID, pinned, resolver, imageCheckKey, false)
	if err != nil || d.Plan.Services[0].PullDigest == "" || len(resolver.called()) != 0 {
		t.Fatalf("pinned with anonymous pull: %+v %v calls %+v", d, err, resolver.called())
	}
}
```

- [ ] **Step 2: Run them**

Run: `go test -count=1 -run 'TestPendingValidationsOfAClusterReadsNoInventory|TestKubernetesPinnedPlanNeedsARegistryRow' ./internal/store/`
Expected: `TestPendingValidationsOfAClusterReadsNoInventory` FAILS with `without the tables: [] SQL logic error: no such table: application_resources`; `TestKubernetesPinnedPlanNeedsARegistryRow` PASSES (it pins existing behaviour).

- [ ] **Step 3: Implement**

In `internal/store/validations.go`, `PendingValidations`, at the top of the placement loop replace

```go
	for i := range out {
		p := &out[i]
		bound, err := t.boundServices(ctx, t.store.db, p.InstanceID)
```

with

```go
	for i := range out {
		p := &out[i]
		if p.Kubernetes {
			// A cluster settle binds no resource: the Deployment's status read decides.
			for j := range p.Services {
				p.Services[j].Presence = PresenceUnknown
			}
			continue
		}
		bound, err := t.boundServices(ctx, t.store.db, p.InstanceID)
```

and in the inner loop further down replace the comment

```go
			// A cluster settle binds no resource: the Deployment's status read decides.
			if p.Services[j].Kind == protocol.KindDeployment {
```

with

```go
			// A cluster identity whose endpoint no longer reads as a cluster: still unknown.
			if p.Services[j].Kind == protocol.KindDeployment {
```

Replace the doc comment

```go
// PendingValidations lists each organization's PendingValidationsPerOrg oldest validations that are
// not done or owe a rollback decision, oldest first, each with its settled services
// placed against the instance's resources and the endpoint's latest inventory; a cluster service is
// placed unknown, for its status read to decide. Health counts container.inspect.health or
// kubernetes.inspect: CapabilitiesFit keeps each to its own runtime.
```

with

```go
// PendingValidations lists each organization's PendingValidationsPerOrg oldest validations that are
// not done or owe a rollback decision, oldest first, each with its settled services
// placed against the instance's resources and the endpoint's latest inventory; a cluster row's
// services are placed unknown without either read, for the status read to decide. Health counts
// container.inspect.health or kubernetes.inspect: CapabilitiesFit keeps each to its own runtime.
```

- [ ] **Step 4: Run on both dialects**

Run: `go test -count=1 -run 'TestPendingValidations|TestKubernetesPinnedPlanNeedsARegistryRow' ./internal/store/`
Expected: `ok`.
Run: `PG=… go test -count=1 -p 1 -run 'TestPendingValidations|TestKubernetesPinnedPlanNeedsARegistryRow' ./internal/store/`
Expected: `ok`.

- [ ] **Step 5: Document**

`internal/store/AGENTS.md`, health validation bullet, replace

```
and places each settled identity: a cluster `Kind: Deployment` identity `unknown` (its status read decides; a cluster settle binds no resource), otherwise by `presence`:
```

with

```
and places each settled identity: every identity of a cluster row `unknown` without reading bindings or inventory (its status read decides; a cluster settle binds no resource; `TestPendingValidationsOfAClusterReadsNoInventory`), a `Kind: Deployment` identity on an endpoint no longer read as a cluster also `unknown`, otherwise by `presence`:
```

In the Rollback bullet, after the sentence ending `with no registry call; a pin naming no service, or pins beside \`Update\`, is \`ErrInvalid\`.` insert:

```
 The pinned host still needs its registry row or anonymous pull: without either a pinned cluster plan is blocked `registry_not_configured` (`TestKubernetesPinnedPlanNeedsARegistryRow`), because `registryFor` runs before the digest shortcut and the frame carries the pull credential.
```

Check: `grep -c "TestKubernetesPinnedPlanNeedsARegistryRow\|TestPendingValidationsOfAClusterReadsNoInventory" internal/store/AGENTS.md` prints `2`.

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/store/validations.go internal/store/validations_cluster_test.go internal/store/kubernetes_deployment_test.go && test -z "$(gofmt -l cmd internal)" && git add internal/store/validations.go internal/store/validations_cluster_test.go internal/store/kubernetes_deployment_test.go internal/store/AGENTS.md && make tidy-check lint && git commit -m "fix(store): skip binding and inventory reads for a cluster validation

Pins the registry-row gate on a pinned cluster plan.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

### Task 3: API — the rollback's registry claims asserted

Tests only; no production code changes.

**Files:**
- Test: `internal/api/image_check_handlers_test.go:24-35` (`fakeDigests`)
- Test: `internal/api/kubernetes_validation_test.go` (`clusterValidation`, `newClusterValidation`, `automate`, `TestClusterValidationRollsBackToThePriorDigests`, add `TestClusterValidationRollbackNeedsARegistryRow`)
- Docs: `internal/api/AGENTS.md` (the `kubernetes_validation_test.go` clause)

**Interfaces:**
- Consumes: `api.SetDigestResolverForTest(s *Server, r store.DigestResolver)`, the harness helpers `v.automate(t) string`, `v.at(t, deployment, offset)`, `v.restart(n)`, `v.frame(t)`, `v.validation(t, id) *store.Validation`, `v.policy(t) *store.UpdatePolicy`, `v.do(t, method, path, body, status) string`, `inspectingCluster`, `afterGrace`, `store.ValidationPoll`, `store.RollbackFailed`, `store.ValidationRollback`, `store.PolicyPaused`, `store.ValidationReasonNotRolledBack` (`"update failed and could not be rolled back: "`).
- Produces: test-only `func (f *fakeDigests) calls() int` and `clusterValidation.digests *fakeDigests` (the resolver the server holds now).

- [ ] **Step 1: Count resolver calls**

In `internal/api/image_check_handlers_test.go` replace

```go
// fakeDigests answers every Head with digest, or err when set. With gate set, it reports entry
// on entered and holds until gate closes or ctx ends.
type fakeDigests struct {
	digest  string
	err     error
	gate    chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	secrets []string
}

func (f *fakeDigests) Head(ctx context.Context, _ registry.Reference, cred *registry.Credential, _ bool) (string, error) {
	f.mu.Lock()
	if cred != nil {
```

with

```go
// fakeDigests answers every Head with digest, or err when set, and counts the calls. With gate
// set, it reports entry on entered and holds until gate closes or ctx ends.
type fakeDigests struct {
	digest  string
	err     error
	gate    chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	secrets []string
	heads   int
}

func (f *fakeDigests) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heads
}

func (f *fakeDigests) Head(ctx context.Context, _ registry.Reference, cred *registry.Credential, _ bool) (string, error) {
	f.mu.Lock()
	f.heads++
	if cred != nil {
```

- [ ] **Step 2: Keep the resolver on the harness**

In `internal/api/kubernetes_validation_test.go`:

- The `clusterValidation` doc comment's last line `// one pod running with restarts.` becomes `// one pod running with restarts. digests is the registry resolver the server holds now.`
- In the struct, below `prior                string` add `digests              *fakeDigests`.
- In `newClusterValidation`, `api.SetDigestResolverForTest(v.s, &fakeDigests{digest: priorDigest})` becomes

```go
	v.digests = &fakeDigests{digest: priorDigest}
	api.SetDigestResolverForTest(v.s, v.digests)
```

- In `automate`, `api.SetDigestResolverForTest(v.s, &fakeDigests{digest: updateDigest})` becomes

```go
	v.digests = &fakeDigests{digest: updateDigest}
	api.SetDigestResolverForTest(v.s, v.digests)
```

- In `TestClusterValidationRollsBackToThePriorDigests`, replace

```go
	v.restart(2)
	v.at(t, id, afterGrace+store.ValidationPoll)
	req := v.frame(t)
	if req.Revision != 1
```

with

```go
	v.restart(2)
	heads := v.digests.calls()
	if heads == 0 {
		t.Fatal("the update check did not go through the harness resolver")
	}
	v.at(t, id, afterGrace+store.ValidationPoll)
	req := v.frame(t)
	if n := v.digests.calls(); n != heads {
		t.Fatalf("the rollback called the registry: %d calls, %d before", n, heads)
	}
	if req.Revision != 1
```

(the rest of that `if` line is unchanged).

- [ ] **Step 3: Add the registry gate test**

Append to `internal/api/kubernetes_validation_test.go`:

```go
// A cluster rollback pins digests and calls no registry, but the pulled host still needs a
// registry row or anonymous pull: with anonymous pull turned off after the update, the rollback
// plan is refused, nothing is planned and the policy pauses naming the blocker.
func TestClusterValidationRollbackNeedsARegistryRow(t *testing.T) {
	v := newClusterValidation(t, inspectingCluster...)
	id := v.automate(t)
	if err := v.st.Tenancy().SetAnonymousPull(context.Background(), store.TenantAccess{ActorID: "usr_deployer", OrganizationID: "a"}, false); err != nil {
		t.Fatal(err)
	}
	v.at(t, id, afterGrace)
	v.restart(2)
	v.at(t, id, afterGrace+store.ValidationPoll)
	got := v.validation(t, id)
	if got.Verdict != store.VerdictRestarting || got.Rollback == nil || *got.Rollback != (store.ValidationRollback{Outcome: store.RollbackFailed, Detail: "registry_not_configured"}) {
		t.Fatalf("validation %+v %+v", got, got.Rollback)
	}
	if p := v.policy(t); p.Status != store.PolicyPaused || p.PausedReason != store.ValidationReasonNotRolledBack+"registry_not_configured" {
		t.Fatalf("policy %+v", p)
	}
	var list []store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.app+"/deployments", "", 200)), &list); err != nil || len(list) != 2 {
		t.Fatalf("deployments: %d %v", len(list), err)
	}
}
```

- [ ] **Step 4: Run**

Run: `go test -count=1 -run 'TestClusterValidation|TestImageCheck' ./internal/api/`
Expected: `ok`. (These assert existing behaviour, so there is no red step; to see the counter bite, temporarily change `heads == 0` to `heads != 0` and watch the first check fail, then revert.)

- [ ] **Step 5: Document**

`internal/api/AGENTS.md`, replace

```
a restarting one rolled back to the prior apply's digest with no registry call and validated as a rollback,
```

with

```
a restarting one rolled back to the prior apply's digest with no registry call (`fakeDigests.calls` unchanged across the rollback) and validated as a rollback, the same rollback refused `registry_not_configured` with anonymous pull turned off (no rollback deployment, validation `failed` with that detail, policy paused naming it),
```

Check: `grep -c "fakeDigests.calls" internal/api/AGENTS.md` prints `1`.

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/api/image_check_handlers_test.go internal/api/kubernetes_validation_test.go && test -z "$(gofmt -l cmd internal)" && git add internal/api/image_check_handlers_test.go internal/api/kubernetes_validation_test.go internal/api/AGENTS.md && make tidy-check lint && git commit -m "test(api): assert a cluster rollback's registry use and its registry gate

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

### Task 4: Web — the health warning names its runtimes

**Files:**
- Modify: `web/src/components/ApplicationPolicy.tsx:180-192` (`HealthWarning`)
- Test: `web/src/components/ApplicationPolicy.test.tsx`
- Build: `web/dist`, `web/tsconfig.tsbuildinfo`
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes: `Endpoint.runtime: string` (`web/src/tenant.ts`; the server stores only `docker` or `kubernetes`), test helpers `stubWithEndpoint(p, capabilities, runtime = 'docker')`, `policy()`, `open()`, `isEndpoint`.
- Produces: nothing other tasks use.

- [ ] **Step 1: Write the failing test**

In `web/src/components/ApplicationPolicy.test.tsx`, directly above `it('shows each run validation and its rollback, and explains a validation pause'`, insert:

```tsx
it('warns nothing for a runtime it does not know', async () => {
  const fetcher = stubWithEndpoint(policy(), [], 'podman');
  render(<ApplicationPolicy base="/app" admin={false} org="a" endpointID="host" />);
  open();
  await vi.waitFor(() => expect(fetcher.mock.calls.some(isEndpoint)).toBe(true));
  await screen.findByText(/Plan and apply ·/);
  expect(screen.queryByText(/cannot report/)).toBeNull();
});

```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd web && npx vitest run --maxWorkers=1 src/components/ApplicationPolicy.test.tsx`
Expected: 1 failed (`warns nothing for a runtime it does not know`: `expected <p role="alert"></p> to be null`), 14 passed.

- [ ] **Step 3: Implement**

In `web/src/components/ApplicationPolicy.tsx` replace the whole `HealthWarning` (comment included) with:

```tsx
// HealthWarning names a host whose agent cannot report what validation reads (container health on
// Docker, workload status on a cluster): every automated update there ends unverifiable and pauses
// the policy. Another runtime gets no warning.
function HealthWarning({ org, endpointID }: { org: string; endpointID: string }) {
  const endpoint = useTenantResource<Endpoint>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpointID)}`);
  if (endpoint.state !== 'ready' || !endpoint.data) return null;
  const { runtime, capabilities } = endpoint.data;
  if (runtime === 'kubernetes' && !capabilities.includes('kubernetes.inspect')) {
    return <p role="alert">This cluster's agent cannot report workload status, so automated updates here cannot be validated and pause the policy after each one. Upgrade the agent image.</p>;
  }
  if (runtime === 'docker' && !capabilities.includes('container.inspect.health')) {
    return <p role="alert">This host's agent cannot report container health, so automated updates here cannot be validated and pause the policy after each one. Upgrade the agent.</p>;
  }
  return null;
}
```

- [ ] **Step 4: Run the file**

Run: `cd web && npx vitest run --maxWorkers=1 src/components/ApplicationPolicy.test.tsx`
Expected: 15 passed (the Docker and cluster warning tests included).

- [ ] **Step 5: Document**

`web/AGENTS.md`, replace

```
and warns when the endpoint's own capability is missing: `container.inspect.health` on a Docker host, `kubernetes.inspect` on a cluster (naming the agent image upgrade).
```

with

```
and warns when the endpoint's own capability is missing: `container.inspect.health` on a Docker host, `kubernetes.inspect` on a cluster (naming the agent image upgrade); any other runtime gets no warning.
```

and in the Verification bullet replace `and the next window in both zones;` with `the next window in both zones, and the capability warning per runtime (none for an unknown one);`.

Check: `grep -c "any other runtime gets no warning" web/AGENTS.md` prints `1`.

- [ ] **Step 6: Rebuild the embedded bundle**

With nothing else running: `make build-web`
Expected: the Vite build succeeds; `git status --short web` lists `web/src/components/ApplicationPolicy.tsx`, `web/src/components/ApplicationPolicy.test.tsx`, `web/AGENTS.md`, files under `web/dist` and possibly `web/tsconfig.tsbuildinfo`, nothing else.

- [ ] **Step 7: Commit**

```bash
git add web/src/components/ApplicationPolicy.tsx web/src/components/ApplicationPolicy.test.tsx web/AGENTS.md web/dist web/tsconfig.tsbuildinfo && make tidy-check lint && git commit -m "fix(web): warn about validation capability only for a known runtime

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Final gate (controller only, after Task 4, nothing else running)

- `git diff --quiet ca8f0ab -- internal/store/migrations` (no migration).
- `make ci`, then `make test-postgres` with `KY_TEST_POSTGRES_DSN` set as in Global Constraints.
- `git status --short` is clean apart from the untracked files present before the branch started.
