# M8 Review Hygiene (deferred minors of PRs 20–21) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the review minors that PRs 20 (#73) and 21 (#75) deferred and that touch code already on `master`: a sturdier cluster rollout and write path, plan-time Kubernetes name checks, permission-before-runtime ordering on every application route, and the matching web, docs and test gaps. No new feature, no schema change.

**Architecture:** Six batched tasks by area. Task 1 hardens `internal/runtime/kubernetes` (rollout wait, update fallback, write-refusal cause, one version read, missing tests). Task 2 adds two plan-time blockers to `internal/store` and narrows two reads. Task 3 reorders the `internal/api` gates and dedupes the runtime selection. Task 4 fixes the web (shared badge, `aria-expanded`, enrollment namespace check, per-case sentences) and rebuilds `web/dist`. Task 5 pins the disclosure wording against the threat model and completes the README. Task 6 runs the full gate. Items that exist only in #76 (feat/migration-analysis) are listed at the end and not planned.

**Tech Stack:** Go 1.26 (module `go 1.26.6`), `k8s.io/client-go` v0.37.1 fake clientset, SQLite + PostgreSQL 17, React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-26-m8-review-hygiene-design.md`

## Global Constraints

- Work in the worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/hygiene` (branch `chore/m8-review-hygiene`, on top of `master` 50095a8 and the committed spec 3ef9c4c). Every command below runs from its root. `master` has PRs 20 and 21, not PR 22 (#76).
- Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Gate each commit on the previous command's exit (`&&`).
- `gofmt -w` every edited Go file and check `gofmt -l cmd internal` prints nothing before each commit. Never put two single quotes or two backticks in a row in a Go comment. `make tidy-check lint` runs after `git add` (it diffs the working tree against the index) and before `git commit`. `make ci` must pass at the end (Task 6). Never run `make build-web` while `make ci` runs: both run `npm ci` in `web/`.
- `web/dist` is embedded and committed: Task 4 rebuilds it with `make build-web` and commits it with `web/tsconfig.tsbuildinfo`.
- DOX: the task that changes a directory's contract updates that directory's `AGENTS.md` in the same commit.
- The server links no client-go: `go list -deps ./cmd/server | grep -c k8s.io` prints `0`.
- Every store change is tested on SQLite and PostgreSQL. PostgreSQL runs use the local container `kyyard-access-pg`; read the password into a variable, never print it: `PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test ...`. Below this prefix is written `PG=… go test`.
- Real-cluster tests skip unless `KY_TEST_KUBECONFIG` is set.
- No schema migration in this slice (the latest registered stays as on `master`).
- Every new closed code has a web sentence in the same task that introduces it, and the vocabulary fixtures (Go vocabulary lists and the web tables' tests) stay in sync. Task 2 edits `web/src` for its codes; Task 4 rebuilds and commits `web/dist` for every web change.
- No wire change for the agent: step codes and their detail shapes are unchanged (`forbidden` keeps an empty detail), so a new server and an old agent, or the reverse, stay compatible. The only `protocol` change is two plan-only `UnsupportedCodes` (Task 2), which no agent sends.

Plan decisions where the spec's wording left a choice (each is reported to the reviewer; none widens scope):

1. **Write 403.** The spec offers "SelfSubjectAccessReview for the failing verb, or name both causes". The agent asks the review for the failing verb, resource and name; not granted is `forbidden` (existing code, existing sentence, empty detail), granted is `admission_denied` as today; a review that cannot be read keeps `admission_denied`.
2. **Rollout read.** A failed `Get` during the wait that is transient (no answer from the API server, 429, 5xx) is polled through to the deadline, keeping the last good read, whose reasons a timeout reports. A read the API server answers otherwise (403, 404) still ends the wait at once with that code: waiting ten minutes would not change it. A failed read neither counts toward nor resets the two-poll failure rule.
3. **Update NotFound.** Only an `Update` that answers NotFound falls back to `Create` (one re-read, within the existing two-attempt budget); a `Create` that answers NotFound (the namespace is gone) still fails as before.
4. **ServerVersion once.** Read as one call site: `Facts` and `engine` share `serverVersion`, and each issues exactly one version read. The version is not cached across calls: an in-place cluster upgrade must show in the next snapshot.
5. **Plan-time name checks.** Two new closed codes, per-service `unsupported` under the existing `kubernetes_unsupported` blocker (so the web table, `PreflightBlockedError` and `MaxUnsupported` 40 absorb them with no new shape): `k8s_name_taken` (the cluster inventory shows a Deployment in the namespace under the service's planned name that is not this instance's: another application's, as in `shop`+`web-api` beside `shop-web`+`api`, or another tool's) and `k8s_service_renamed` (a service of the last-applied revision whose computed name changes because a new service shares its slug, as `a-b` beside `a_b`). The inventory is the source, not other applications' definitions: it shows what apply would hit, and the agent's `name_taken` still guards the gap.
6. **Viewer ordering.** Permission first, then the runtime gate, on commands, removal preview, adoption and mapping, exactly as exec and inspection do since PR 20: a viewer on any endpoint gets 403 with a denial audit row, never 409.
7. **Enrollment namespace check.** The web applies the same rule the server does (`KubernetesManifest.tsx` invalid sentence): DNS-1123 labels, at most 32, not `kyyard-agent`, not `kube-*`, and disables submit with that sentence; the server check stays the authority.

## Review Focus

1. The rollout read keeps failing with an answer that will not change (the agent's grant removed mid-rollout: 403; the Deployment deleted by someone else: 404): the wait must end at once with that code, not sit out the ten-minute deadline. Only transient reads (no answer, 429, 5xx) are polled through. Pinned in Task 1 (`TestDeployRolloutEndsOnAnAnswer`).
2. A service renamed to a slug-equal name in a new revision (`a_b` becomes `a-b`): the new service would take over the old one's objects and the API server would refuse the Deployment's changed selector at apply (`runtime_status 422`). The plan must refuse it with `k8s_service_renamed` instead. Pinned in Task 2 (`TestKubernetesPlanRefusesNameCollisions`, third phase).
3. The web namespace check at its edges: a 63-character label and 32 names are what the server accepts and must not be blocked; a 64-character label must be. Pinned in Task 4 (`checks the namespace list before enrolling a cluster...`).
4. The new permission pre-checks must not double the audit trail: an allowed check writes nothing (the operation audits itself), a denied one writes exactly one `denied` row. Pinned in Task 3 (`TestAccessChecksAuditOnlyDenials`).
5. The plan-time held-name check must not refuse the instance's own Deployment or a same-named Deployment in another namespace. Pinned in Task 2 (`TestKubernetesPlanRefusesNameCollisions`, first phase inventory).

## File map

| Path | Task | Change |
|---|---|---|
| `internal/runtime/kubernetes/deploy.go`, `kubernetes.go`, their tests, `AGENTS.md` | 1 | Transient rollout reads, update→create fallback, write 403 cause by access review, one version read, missing tests |
| `internal/store/application_preflight.go`, `application_apply.go`, `endpoints.go`, `internal/agent/protocol/inspection.go`, store/protocol tests, `internal/store/AGENTS.md`, `internal/agent/AGENTS.md`, `web/src/components/ApplicationInspection.tsx` | 2 | `k8s_name_taken`, `k8s_service_renamed`, narrow health decode, `serviceEnv`, `revisionServices` |
| `internal/store/{exec,image_checks,commands,store}.go`, `internal/api/{runtime_gate,application_handlers,endpoint_handlers,exec_handlers}.go`, api/store tests, `internal/api/AGENTS.md`, `internal/store/AGENTS.md` | 3 | Permission before runtime gate, `runtimeCapability`, cluster-plan inspection guard, mapping read only when needed |
| `web/src/tenant.ts`, `web/src/components/*.tsx` (listed in the task), web tests, `web/dist`, `web/AGENTS.md` | 4 | Shared badge, `aria-expanded`, enrollment namespace check, per-case and cluster sentences, edge-state tests |
| `internal/api/disclosure_internal_test.go`, `README.md`, `docs/{threat-model,agent-protocol,application-schema}.md`, `internal/api/AGENTS.md` | 5 | Disclosure pin, addressing, upgrade order, new codes |
| — | 6 | Full gate |

---

### Task 1: Runtime — rollout, write path and version read on a cluster

**Files:**
- Modify: `internal/runtime/kubernetes/deploy.go` (`Deploy` doc comment, `allowed`, new `review`, `kindResource`, `refusedWrite`, `drop`, `upsert`, `rollout`, new `transient`)
- Modify: `internal/runtime/kubernetes/kubernetes.go` (new `serverVersion`; `engine`, `Facts`)
- Test: `internal/runtime/kubernetes/deploy_test.go` (the fake access review grants the deploy Role; eight new tests)
- Test: `internal/runtime/kubernetes/kubernetes_test.go` (`TestFactsNameTheCluster` counts version reads)
- Docs: `internal/runtime/kubernetes/AGENTS.md`

**Interfaces:**
- Consumes: `run`, `upsert`, `drop`, `failure`, `stalled`, `render.LabelInstance`, test helpers `deployCluster`, `deployClusterWith`, `owned`, `writes`, `deploymentsResource`, `cluster` (existing).
- Produces (package `kubernetes`, unexported):
  ```go
  func (r *run) review(ctx context.Context, attrs authorizationv1.ResourceAttributes) (bool, error)
  var kindResource map[string][2]string // kind -> {group, resource}
  func (r *run) refusedWrite(ctx context.Context, err error, verb, kind, name string) (string, string, string)
  func transient(err error) bool
  func (c *Client) serverVersion(ctx context.Context) (*version.Info, error)
  // test helpers: func deployRoleGrants(a *authorizationv1.ResourceAttributes) bool; func failDeploymentGets(cs *fake.Clientset, n int)
  ```
  No exported name, protocol code or detail shape changes.

- [ ] **Step 1: Make the fake access review grant the deploy Role**

The fake API server's access review allowed only `create apps/deployments`. The new write path asks a review for the failing write, so the fake must answer like the manifest's `kyyard-agent-deploy` Role. In `internal/runtime/kubernetes/deploy_test.go`, inside `deployClusterWith`, replace

```go
		review.Status.Allowed = !denied && a.Namespace == "shop" && a.Verb == "create" && a.Group == "apps" && a.Resource == "deployments"
```

with

```go
		review.Status.Allowed = !denied && a.Namespace == "shop" && deployRoleGrants(a)
```

and insert directly above `// writes lists the verbs that changed the cluster, the access review aside.`:

```go
// deployRoleGrants is the kyyard-agent-deploy Role the manifest grants in a listed namespace.
func deployRoleGrants(a *authorizationv1.ResourceAttributes) bool {
	switch {
	case a.Group == "apps" && a.Resource == "deployments", a.Group == "" && (a.Resource == "services" || a.Resource == "configmaps"):
		return slices.Contains([]string{"get", "list", "create", "update", "patch", "delete"}, a.Verb)
	case a.Group == "" && a.Resource == "secrets":
		return slices.Contains([]string{"get", "create", "update", "patch", "delete"}, a.Verb)
	}
	return false
}
```

- [ ] **Step 2: Write the tests**

Append to the end of `internal/runtime/kubernetes/deploy_test.go` (every import is already there):

```go

// failDeploymentGets makes the next n reads of a Deployment after its first create answer 500,
// standing in for an API server that drops a request mid-rollout.
func failDeploymentGets(cs *fake.Clientset, n int) {
	created, left := false, n
	cs.PrependReactor("create", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		created = true
		return false, nil, nil
	})
	cs.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !created || left == 0 {
			return false, nil, nil
		}
		left--
		return true, nil, apierrors.NewInternalError(errors.New("etcd leader changed"))
	})
}

// One failed read during the rollout wait does not end it: the wait polls on and the rollout
// still succeeds.
func TestDeployRolloutSurvivesATransientRead(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	failDeploymentGets(cs, 1)
	if res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("one failed read ended the rollout: %+v", res)
	}
}

// Reads that keep failing run the wait to the deadline, which names the reasons from the last
// read that succeeded.
func TestDeployRolloutTimeoutKeepsTheLastGoodRead(t *testing.T) {
	c, cs := deployCluster(t, false, false)
	reads := 0
	cs.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if _, err := cs.Tracker().Get(deploymentsResource, "shop", action.(k8stesting.GetAction).GetName()); err != nil {
			return false, nil, nil // the precondition's read of an absent Deployment
		}
		if reads++; reads > 1 {
			return true, nil, apierrors.NewInternalError(errors.New("etcd leader changed"))
		}
		return false, nil, nil
	})
	res := c.Deploy(context.Background(), deployRequest(300*time.Millisecond), func() {})
	i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
	if res.Outcome != protocol.OutcomeTimedOut || i < 0 || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	if s := res.Steps[i]; s.Step != protocol.StepStart || s.Code != "rollout_timeout" || s.Detail != "progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable" {
		t.Fatalf("step %+v", s)
	}
}

// The agent stopping mid-rollout ends the wait as cancelled: the API server may already have
// acted, so the outcome is unknown, not failed.
func TestDeployParentCancelledDuringRollout(t *testing.T) {
	c, _ := deployCluster(t, false, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := c.Deploy(ctx, deployRequest(time.Minute), func() { time.AfterFunc(50*time.Millisecond, cancel) })
	i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
	if res.Outcome != protocol.OutcomeUnknown || i < 0 || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	if s := res.Steps[i]; s.Service != "web" || s.Step != protocol.StepStart || s.Code != "cancelled" {
		t.Fatalf("step %+v", s)
	}
}

// Re-sending the same frame updates every owned object in place to the same content: no create,
// no delete, the same Secret and ConfigMap data.
func TestDeployReapplyIsIdempotent(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	if res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("first apply %+v", res)
	}
	cs.ClearActions()
	res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || len(res.Services) != 2 || res.Services[0].UID != testUID {
		t.Fatalf("re-apply %+v", res)
	}
	if !slices.Equal(writes(cs), []string{"update configmaps", "update secrets", "update deployments", "update services", "update configmaps", "update deployments"}) {
		t.Fatalf("writes %v", writes(cs))
	}
	cm, _ := cs.CoreV1().ConfigMaps("shop").Get(context.Background(), "shop-web-env", metav1.GetOptions{})
	secret, _ := cs.CoreV1().Secrets("shop").Get(context.Background(), "shop-web-secret", metav1.GetOptions{})
	if len(cm.Data) != 1 || cm.Data["MODE"] != "prod" || len(secret.Data) != 1 || string(secret.Data["TOKEN"]) != "s3cret" {
		t.Fatalf("re-apply changed content: %v %v", cm.Data, secret.Data)
	}
}

// An owned object deleted between the precondition read and its update is created instead.
func TestDeployUpdateOfAVanishedObjectCreates(t *testing.T) {
	existing := &corev1.ConfigMap{ObjectMeta: owned("web")}
	existing.Name = "shop-web-env"
	c, cs := deployCluster(t, true, false, existing)
	cs.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if err := cs.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "shop", "shop-web-env"); err != nil {
			return false, nil, nil // already deleted: let the tracker answer NotFound again
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "shop-web-env")
	})
	res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("result %+v", res)
	}
	if cm, err := cs.CoreV1().ConfigMaps("shop").Get(context.Background(), "shop-web-env", metav1.GetOptions{}); err != nil || cm.Labels[render.LabelInstance] != testInstance {
		t.Fatalf("configmap %+v %v", cm, err)
	}
}

// Losing a race on a write re-reads the object: one another tool now holds under the name is
// name_taken (never overwritten), and one this instance holds is written again.
func TestDeployRaceRereadsTheObject(t *testing.T) {
	foreign := func() *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-web-env", Labels: map[string]string{"app": "other"}}}
	}
	ours := func() *corev1.ConfigMap {
		m := owned("web")
		m.Name = "shop-web-env"
		return &corev1.ConfigMap{ObjectMeta: m}
	}
	cmResource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	for name, tc := range map[string]struct {
		existing runtime.Object
		verb     string
		winner   *corev1.ConfigMap
		lost     error
		code     string
	}{
		"conflict, now foreign":    {ours(), "update", foreign(), apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "shop-web-env", nil), "name_taken"},
		"exists race, now foreign": {nil, "create", foreign(), apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "shop-web-env"), "name_taken"},
		"exists race, ours":        {nil, "create", ours(), apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "shop-web-env"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			var objects []runtime.Object
			if tc.existing != nil {
				objects = append(objects, tc.existing)
			}
			c, cs := deployCluster(t, true, false, objects...)
			raced := false
			cs.PrependReactor(tc.verb, "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				if raced {
					return false, nil, nil
				}
				raced = true
				if err := cs.Tracker().Delete(cmResource, "shop", "shop-web-env"); err != nil && !apierrors.IsNotFound(err) {
					return true, nil, err
				}
				if err := cs.Tracker().Add(tc.winner); err != nil {
					return true, nil, err
				}
				return true, nil, tc.lost
			})
			res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
			if res.Validate() != nil {
				t.Fatalf("result %+v %v", res, res.Validate())
			}
			if tc.code == "" {
				if res.Outcome != protocol.OutcomeSucceeded {
					t.Fatalf("result %+v", res)
				}
				return
			}
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			if res.Outcome != protocol.OutcomeDenied || i < 0 || res.Steps[i].Step != protocol.StepCreate || res.Steps[i].Code != tc.code || res.Steps[i].Detail != "ConfigMap/shop-web-env" {
				t.Fatalf("result %+v", res)
			}
			if cm, _ := cs.CoreV1().ConfigMaps("shop").Get(context.Background(), "shop-web-env", metav1.GetOptions{}); cm.Labels["app"] != "other" {
				t.Fatalf("the foreign ConfigMap was overwritten: %+v", cm)
			}
		})
	}
}

// A 403 on a write the agent's Role does not grant (an older or edited manifest) is forbidden:
// re-applying the manifest fixes it. A 403 on a write the Role grants is the cluster refusing
// the object, admission_denied, which a manifest does not fix.
func TestDeployWriteRefusalNamesItsCause(t *testing.T) {
	for name, tc := range map[string]struct {
		granted      bool
		outcome      string
		code, detail string
	}{
		"verb not granted": {false, protocol.OutcomeDenied, "forbidden", ""},
		"admission":        {true, protocol.OutcomeDenied, "admission_denied", "ConfigMap/shop-web-env"},
	} {
		t.Run(name, func(t *testing.T) {
			c, cs := deployCluster(t, true, false)
			cs.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "shop-web-env", errors.New("secret-canary"))
			})
			var asked []authorizationv1.ResourceAttributes
			cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
				a := *review.Spec.ResourceAttributes
				asked = append(asked, a)
				if a.Resource != "configmaps" {
					return false, nil, nil
				}
				review.Status.Allowed = tc.granted
				return true, review, nil
			})
			res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			if res.Outcome != tc.outcome || i < 0 || res.Validate() != nil {
				t.Fatalf("result %+v %v", res, res.Validate())
			}
			if s := res.Steps[i]; s.Step != protocol.StepCreate || s.Code != tc.code || s.Detail != tc.detail {
				t.Fatalf("step %+v", s)
			}
			want := authorizationv1.ResourceAttributes{Namespace: "shop", Verb: "create", Resource: "configmaps", Name: "shop-web-env"}
			if !slices.Contains(asked, want) {
				t.Fatalf("no access review for the failing write: %+v", asked)
			}
		})
	}
}

// A read the API server answers with something waiting will not change (the grant removed: 403;
// the Deployment deleted by someone else: 404) ends the wait at once with that code.
func TestDeployRolloutEndsOnAnAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		err          error
		outcome      string
		code, detail string
	}{
		"forbidden": {apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "shop-web", errors.New("secret-canary")), protocol.OutcomeDenied, "forbidden", ""},
		"not found": {apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, "shop-web"), protocol.OutcomeFailed, "runtime_status", "404"},
	} {
		t.Run(name, func(t *testing.T) {
			c, cs := deployCluster(t, false, false)
			created := false
			cs.PrependReactor("create", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
				created = true
				return false, nil, nil
			})
			cs.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
				if !created {
					return false, nil, nil
				}
				return true, nil, tc.err
			})
			began := time.Now()
			res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
			if took := time.Since(began); took > 5*time.Second {
				t.Fatalf("the wait ran %v on an answer that will not change", took)
			}
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			if res.Outcome != tc.outcome || i < 0 || res.Validate() != nil {
				t.Fatalf("result %+v %v", res, res.Validate())
			}
			if s := res.Steps[i]; s.Step != protocol.StepStart || s.Code != tc.code || s.Detail != tc.detail {
				t.Fatalf("step %+v", s)
			}
		})
	}
}
```

In `internal/runtime/kubernetes/kubernetes_test.go`, replace `TestFactsNameTheCluster` whole with:

```go
// Facts and a snapshot each read the server version once, through the same call.
func TestFactsNameTheCluster(t *testing.T) {
	c, cs := cluster(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}})
	versionReads := func() int {
		n := 0
		for _, a := range cs.Discovery().(*fakediscovery.FakeDiscovery).Actions() {
			if a.GetResource().Resource == "version" {
				n++
			}
		}
		return n
	}
	facts := c.Facts(context.Background())
	if facts["runtime"] != "kubernetes" || facts["server_version"] != "v1.36.0" || facts["node_count"] != "2" || facts["platform"] != "linux/amd64" || len(facts) != 4 {
		t.Fatalf("facts %v", facts)
	}
	if n := versionReads(); n != 1 {
		t.Fatalf("Facts read the version %d times", n)
	}
	if snap, _ := c.Snapshot(context.Background()); snap.Engine.Version != "v1.36.0" || versionReads() != 2 {
		t.Fatalf("engine %+v after %d version reads", snap.Engine, versionReads())
	}
}
```

- [ ] **Step 3: Run the tests to see which fail**

Run: `go test -count=1 -run 'TestDeployRolloutSurvives|TestDeployRolloutTimeoutKeeps|TestDeployRolloutEndsOnAnAnswer|TestDeployParentCancelled|TestDeployReapply|TestDeployUpdateOfAVanished|TestDeployRaceRereads|TestDeployWriteRefusal|TestFactsNameTheCluster' ./internal/runtime/kubernetes/`
Expected: FAIL in exactly four tests:
- `TestDeployRolloutSurvivesATransientRead` and `TestDeployRolloutTimeoutKeepsTheLastGoodRead`: `web start failed runtime_status 500`.
- `TestDeployUpdateOfAVanishedObjectCreates`: `web create failed runtime_status 404`.
- `TestDeployWriteRefusalNamesItsCause/verb_not_granted`: `admission_denied` and `no access review for the failing write`.

`TestDeployParentCancelledDuringRollout`, `TestDeployReapplyIsIdempotent`, `TestDeployRaceRereadsTheObject`, `TestDeployRolloutEndsOnAnAnswer` and `TestFactsNameTheCluster` pass already: they pin behaviour the review found untested (parent cancel, idempotent re-apply, conflict→foreign `name_taken`, the `AlreadyExists` race) or that Step 4 must keep (a 403 or 404 read still ends the wait at once: Review Focus 1), and `TestFactsNameTheCluster` guards the Step 5 refactor.

- [ ] **Step 4: Rollout wait, update fallback and write refusal**

In `internal/runtime/kubernetes/deploy.go`:

Replace the `Deploy` doc comment lines

```go
// Deployment, Service, each updated when it exists and is owned, created otherwise; one conflict
// is re-read and retried; a write the cluster refuses is admission_denied) and start (the
// rollout, polled until available, a failure the cluster reports, or the deadline:
// rollout_timeout). The first step that is not a success ends the run and every later step is
// skipped. Nothing is rolled back: a failed rollout leaves the objects as applied. started is
// called once, before the first write.
```

with

```go
// Deployment, Service, each updated when it exists and is owned, created otherwise; one conflict
// is re-read and retried; a refused write is forbidden without the verb, admission_denied with
// it) and start (the rollout, polled until available, a failure the cluster reports, or the
// deadline: rollout_timeout). The first step that is not a success ends the run and every later
// step is skipped. Nothing is rolled back: a failed rollout leaves the objects as applied.
// started is called once, before the first write.
```

Replace the body of `allowed` and add `review` after it:

```go
func (r *run) allowed(ctx context.Context) (string, string, string) {
	ok, err := r.review(ctx, authorizationv1.ResourceAttributes{Namespace: r.namespace, Verb: "create", Group: "apps", Resource: "deployments"})
	if err != nil {
		return r.failure(ctx, err)
	}
	if !ok {
		return protocol.OutcomeDenied, "forbidden", ""
	}
	return succeeded()
}

// review asks the API server whether the agent may act as attrs describe.
func (r *run) review(ctx context.Context, attrs authorizationv1.ResourceAttributes) (bool, error) {
	review, err := r.c.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs}}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return review.Status.Allowed, nil
}
```

Replace `refusedWrite` and its comment whole with:

```go
// kindResource is the API group and resource each rendered kind is written through.
var kindResource = map[string][2]string{"ConfigMap": {"", "configmaps"}, "Secret": {"", "secrets"}, "Deployment": {"apps", "deployments"}, "Service": {"", "services"}}

// refusedWrite classifies a write that did not succeed. A 403 is the agent's Role lacking the
// verb (an older or edited manifest: forbidden, fixed by applying the manifest) or the cluster's
// admission refusing the object (a quota, a policy engine: admission_denied, named by kind and
// name); an access review for the failing write tells them apart, and an unreadable review
// leaves admission_denied.
func (r *run) refusedWrite(ctx context.Context, err error, verb, kind, name string) (string, string, string) {
	if !apierrors.IsForbidden(err) {
		return r.failure(ctx, err)
	}
	gr := kindResource[kind]
	if ok, rerr := r.review(ctx, authorizationv1.ResourceAttributes{Namespace: r.namespace, Verb: verb, Group: gr[0], Resource: gr[1], Name: name}); rerr == nil && !ok {
		return protocol.OutcomeDenied, "forbidden", ""
	}
	return protocol.OutcomeDenied, "admission_denied", kind + "/" + name
}
```

In `drop`, replace `return r.refusedWrite(ctx, err, kind, o.GetName())` with `return r.refusedWrite(ctx, err, "delete", kind, o.GetName())`.

In `upsert`, replace the doc comment's last two lines

```go
// keeping have's resourceVersion. A conflict, or an object that appeared since, is re-read once:
// still owned, it is written again; a second conflict fails with conflict.
```

with

```go
// keeping have's resourceVersion. A conflict, an object that appeared since, or one that vanished
// before its update is re-read once: still owned it is written again, gone it is created, and a
// second conflict fails with conflict.
```

then in its loop replace

```go
		var got T
		var err error
		if have == zero {
```

with

```go
		var got T
		var err error
		verb := "update"
		if have == zero {
			verb = "create"
```

and replace

```go
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			o, code, detail := r.refusedWrite(ctx, err, kind, want.GetName())
```

with

```go
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) && (verb == "create" || !apierrors.IsNotFound(err)) {
			o, code, detail := r.refusedWrite(ctx, err, verb, kind, want.GetName())
```

(The existing re-read then finds nothing, sets `have` to zero, and the second attempt creates.)

In `rollout`, replace the doc comment's last line

```go
// A failure the controller reports for the current generation on two consecutive polls ends the wait.
```

with

```go
// A failure the controller reports for the current generation on two consecutive polls ends the
// wait. A transient read failure is not an answer: the wait keeps the last good read and polls on.
```

and in its loop replace

```go
		case ctx.Err() == nil:
			return r.failure(ctx, err)
```

with

```go
		case ctx.Err() == nil && !transient(err):
			return r.failure(ctx, err)
```

Add `"net/http"` to the imports (after `"errors"`), and insert above `func ready(`:

```go
// transient is a read error worth another poll: no answer from the API server, or one that says
// try again (429, 5xx). Any other answer (403, 404) will not change by waiting.
func transient(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return true
	}
	code := status.Status().Code
	return code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}
```

A transient error falls through to the `select`, which polls again; `last` keeps the last good read, so the deadline still answers `timed_out rollout_timeout` with `r.stalled(set, last)`, and a cancelled parent `unknown cancelled`.

- [ ] **Step 5: One server version read**

In `internal/runtime/kubernetes/kubernetes.go`, add `"k8s.io/apimachinery/pkg/version"` to the imports after `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`, and replace

```go
// engine is the API server's version, read within ctx.
func (c *Client) engine(ctx context.Context) (protocol.Engine, error) {
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	v, err := c.cs.Discovery().ServerVersionWithContext(ctx)
	if err != nil {
```

with

```go
// serverVersion reads the API server's version within callBudget: the one read Facts and every
// snapshot's Engine share.
func (c *Client) serverVersion(ctx context.Context) (*version.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	return c.cs.Discovery().ServerVersionWithContext(ctx)
}

// engine is the API server's version as a snapshot reports it.
func (c *Client) engine(ctx context.Context) (protocol.Engine, error) {
	v, err := c.serverVersion(ctx)
	if err != nil {
```

and in `Facts` replace `if v, err := c.cs.Discovery().ServerVersionWithContext(ctx); err == nil {` with `if v, err := c.serverVersion(ctx); err == nil {`.

- [ ] **Step 6: Run the package**

Run: `gofmt -w internal/runtime/kubernetes && gofmt -l cmd internal && go test -count=3 -race ./internal/runtime/kubernetes/...`
Expected: `gofmt -l` prints nothing; three `ok` lines (`kubernetes`, `manifest`, `render`), every test passing three times.

- [ ] **Step 7: DOX**

Apply these replacements to `internal/runtime/kubernetes/AGENTS.md` (each old text occurs once):

```text
OLD: alongside the server version (`ServerVersionWithContext`, within the caller's context)
NEW: alongside the server version (`serverVersion`, the one `ServerVersionWithContext` call `Facts` also uses)

OLD: a conflict or a lost create race re-reads once (still owned: written again; now foreign: `name_taken`), and a second conflict fails `conflict` with `Kind/name`; any other 403 on a write is the cluster refusing the object (quota, admission policy): `denied admission_denied` with `Kind/name`.
NEW: a conflict, a lost create race or an update whose object vanished re-reads once (still owned: written again; gone: created; now foreign: `name_taken`), and a second conflict fails `conflict` with `Kind/name`; any other 403 on a write asks a `SelfSubjectAccessReview` for that verb, resource and name: not granted (an older or edited manifest) is `denied forbidden`, granted is the cluster refusing the object (quota, admission policy): `denied admission_denied` with `Kind/name`.

OLD: For the current generation `ReplicaFailure=True` or Progressing `ProgressDeadlineExceeded` ends the wait at once as `failed rollout_timeout`; past the deadline it is `timed_out rollout_timeout`.
NEW: For the current generation `ReplicaFailure=True` or Progressing `ProgressDeadlineExceeded` on two consecutive polls ends the wait as `failed rollout_timeout`; a transient read failure (no answer, 429, 5xx) is not an answer (the wait keeps the last good read and polls on), while any other read error ends the wait with its code; past the deadline it is `timed_out rollout_timeout` with the last good read's reasons.

OLD: rollout status, conflicts, a UID-precondition conflict on delete): create then update in place,
NEW: rollout status, conflicts, a UID-precondition conflict on delete): create then update in place, an identical re-apply, a transient read, a 403 or 404 read and a parent cancel during the rollout, a lost race re-read (foreign: `name_taken`), an update of a vanished object, a write 403 told apart by access review,
```

The third replacement also corrects text that predates the two-poll rule.

- [ ] **Step 8: Commit**

```bash
git add internal/runtime/kubernetes && make tidy-check lint && git commit -m "fix(k8s): poll through a failed rollout read, create a vanished object, tell an RBAC gap from admission" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Store — plan-time name checks, a narrow health decode, one env loop

**Files:**
- Modify: `internal/store/application_preflight.go` (`preflight` reads `i.current_revision`; `buildKubernetesPreflight` takes the inventory and the applied services)
- Modify: `internal/store/application_apply.go` (`removalServices` becomes `revisionServices`; new `serviceEnv` used by both frame builders)
- Modify: `internal/store/endpoints.go` (`decorate` decodes nodes and `truncated` only)
- Modify: `internal/agent/protocol/inspection.go` (`UnsupportedCodes` gains `k8s_name_taken`, `k8s_service_renamed`)
- Modify: `web/src/components/ApplicationInspection.tsx` (`unsupportedNames` sentences for both)
- Test: `internal/store/kubernetes_deployment_test.go`, `internal/store/kubernetes_mapping_test.go`, `internal/agent/protocol/inspection_test.go`, `internal/agent/protocol/deployment_test.go` (a stale byte count in a comment), `web/src/components/ApplicationDeploymentPlan.test.tsx`
- Docs: `internal/store/AGENTS.md`, `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: `activeCluster`, `putClusterInventory`, `kubernetesPlanFixture`, `kubePlanRequest`, `fakeResolver`, `tenantAtomicStore`, `imageCheckKey` (store test helpers); `protocol.KubernetesNames`, `protocol.KindDeployment`, `protocol.Workload.Instance`, `protocol.ClusterHealth`.
- Produces:
  ```go
  // package store (unexported)
  func buildKubernetesPreflight(m *ApplicationMapping, spec ApplicationSpec, snapshot protocol.Snapshot, applied []string) *DeploymentPreflight
  func (t *tenancyStore) revisionServices(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, numbers ...int) ([]string, error)
  func serviceEnv(s ApplicationService, values map[string]string) map[string]string
  // package protocol: UnsupportedCodes = [...34 existing..., "k8s_name_taken", "k8s_service_renamed"] (36, MaxUnsupported stays 40)
  // web: unsupportedNames.k8s_name_taken, unsupportedNames.k8s_service_renamed
  ```
  Both codes ride the existing shape: per-service `unsupported` under the `kubernetes_unsupported` blocker, in `PreflightBlockedError.Services` and in `DeploymentPreflight.Services`.

`k8s_service_renamed` covers both directions of a slug collision with the last-applied revision: an applied service whose name would change (`a-b` added beside `a_b`: the old Deployment would keep running beside the new one), and a service whose name an applied service of another name holds (`a_b` renamed `a-b`: the new service would take over the old objects, and the API server would refuse the Deployment's changed selector at apply as a bare `runtime_status 422`).

Why the inventory for `k8s_name_taken`: the cluster snapshot already lists every Deployment with KyYard's `instance` label, so one pass finds both another application's objects (the `shop`+`web-api` / `shop-web`+`api` case) and another tool's, without parsing other applications' definitions. It sees what apply would hit; a name another plan holds but has not applied yet is still refused by the agent's `name_taken` at apply, as today.

- [ ] **Step 1: Write the failing store tests**

Append to `internal/store/kubernetes_deployment_test.go`:

```go
// A plan stops, before the registry is asked, on a Deployment name another instance or tool
// already holds in the namespace (k8s_name_taken; the agent would refuse it at apply), and on a
// service slug collision with the applied revision (k8s_service_renamed): a new service beside
// a_b renames a_b's objects, and a_b renamed to a-b would take them over.
func TestKubernetesPlanRefusesNameCollisions(t *testing.T) {
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "a_b", Image: "ghcr.io/org/ab:1"}, {Name: "web", Image: "ghcr.io/org/web:1"}}}
	st, a, app, cluster, m := kubernetesPlanFixture(t, spec, nil)
	ctx := context.Background()
	ts := st.Tenancy()
	putClusterInventory(t, ts, cluster, []protocol.Workload{
		{Kind: "Deployment", Namespace: "shop", Name: "shop-front-web", Instance: "99999999-7777-4888-9999-aaaaaaaaaaaa"},
		{Kind: "Deployment", Namespace: "other", Name: "shop-front-a-b"},
		{Kind: "Deployment", Namespace: "shop", Name: "shop-front-a-b", Instance: m.InstanceID},
	})
	resolver := &fakeResolver{reply: map[string]fakeReply{}}
	_, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	var blocked *PreflightBlockedError
	if !errors.As(err, &blocked) || len(blocked.Services) != 1 || blocked.Services[0].Name != "web" || !slices.Equal(blocked.Services[0].Unsupported, []string{"k8s_name_taken"}) {
		t.Fatalf("a held name: %v %+v", err, blocked)
	}

	// The instance applied revision 1 ({a_b, web}); revision 2 adds a-b, which shares a_b's slug.
	putClusterInventory(t, ts, cluster, nil)
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE application_instances SET current_revision=1 WHERE id=?`), m.InstanceID); err != nil {
		t.Fatal(err)
	}
	spec.Services = append(spec.Services, ApplicationService{Name: "a-b", Image: "ghcr.io/org/ab:1"})
	if _, err := ts.AppendApplicationRevision(ctx, a, app.ID, 1, spec); err != nil {
		t.Fatal(err)
	}
	m, err = ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if !errors.As(err, &blocked) || len(blocked.Services) != 1 || blocked.Services[0].Name != "a_b" || !slices.Equal(blocked.Services[0].Unsupported, []string{"k8s_service_renamed"}) {
		t.Fatalf("a renamed service: %v %+v", err, blocked)
	}
	spec.Services = []ApplicationService{{Name: "a-b", Image: "ghcr.io/org/ab:1"}, {Name: "web", Image: "ghcr.io/org/web:1"}}
	if _, err := ts.AppendApplicationRevision(ctx, a, app.ID, 2, spec); err != nil {
		t.Fatal(err)
	}
	if m, err = ts.ReadApplicationMapping(ctx, a, app.ID); err != nil {
		t.Fatal(err)
	}
	_, err = ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if !errors.As(err, &blocked) || len(blocked.Services) != 1 || blocked.Services[0].Name != "a-b" || !slices.Equal(blocked.Services[0].Unsupported, []string{"k8s_service_renamed"}) {
		t.Fatalf("a service taking over another's objects: %v %+v", err, blocked)
	}
	if len(resolver.called()) != 0 {
		t.Fatal("a blocked plan asked the registry")
	}
}
```

Append to `internal/store/kubernetes_mapping_test.go`:

```go
// cluster_health reads only the nodes and the cut lists of the stored snapshot: a list it does
// not need, even one that would not decode, does not blank the health of every endpoint read.
func TestClusterHealthReadsOnlyNodes(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeCluster(t, ts, a, nil, nil)
	for raw, want := range map[string]string{
		`{"kubernetes":{"nodes":[{"name":"n1","ready":true}],"pods":"not a list"},"containers":7}`: protocol.HealthHealthy,
		`{"kubernetes":{"nodes":[{"name":"n1","ready":false}]}}`:                                   protocol.HealthDegraded,
		`{"kubernetes":{"nodes":[{"name":"n1","ready":true}]},"truncated":["nodes"]}`:              protocol.HealthUnknown,
		`{"containers":[]}`: protocol.HealthUnknown,
		`not json`:          protocol.HealthUnknown,
	} {
		if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_inventory SET snapshot=? WHERE endpoint_id=?`), raw, cluster); err != nil {
			t.Fatal(err)
		}
		e, err := ts.ReadEndpoint(ctx, a, cluster)
		if err != nil || e.ClusterHealth != want {
			t.Errorf("%s: %q %v", raw, e.ClusterHealth, err)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestKubernetesPlanRefusesNameCollisions|TestClusterHealthReadsOnlyNodes' ./internal/store/`
Expected: FAIL: `a held name: deployment preflight blocked: registry_unavailable` (the plan reached the registry), and `{"kubernetes":{"nodes":[...]},"pods":"not a list"...}: "unknown"` (the whole snapshot was decoded).

- [ ] **Step 3: Plan-time checks**

In `internal/store/application_preflight.go`, `preflight`: replace

```go
	var version, head, number int
```

with

```go
	var version, head, number, applied int
```

In the `SELECT` on the next lines replace `SELECT i.id,i.mapping_version,a.latest_revision,` with `SELECT i.id,i.mapping_version,i.current_revision,a.latest_revision,` and in its `Scan` replace `.Scan(&instance, &version, &head, &number,` with `.Scan(&instance, &version, &applied, &head, &number,`. Then replace

```go
	if m.Runtime == protocol.RuntimeKubernetes {
		out = buildKubernetesPreflight(m, spec)
	} else {
```

with

```go
	if m.Runtime == protocol.RuntimeKubernetes {
		was, err := t.revisionServices(ctx, tx, a, app, applied)
		if err != nil {
			return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", err
		}
		out = buildKubernetesPreflight(m, spec, snapshot, was)
	} else {
```

Replace the doc comment and signature of `buildKubernetesPreflight`:

```go
// buildKubernetesPreflight checks spec against a Kubernetes mapping: the namespace is still one
// the manifest grants (k8s_namespace), and each service is stateless and expressible, else it
// carries kubernetes_unsupported with the k8s_ codes that say why. applied are the services of
// the instance's last-applied revision, snapshot the cluster's inventory. Images need a tag or
// digest; the plan resolves them at the registry.
func buildKubernetesPreflight(m *ApplicationMapping, spec ApplicationSpec, snapshot protocol.Snapshot, applied []string) *DeploymentPreflight {
```

Directly after the loop that fills `named` (and before `blocked := false`) insert:

```go
	// A Deployment in the namespace that is not this instance's (another application's, another
	// tool's) holds its name; the agent would refuse it at apply with name_taken.
	held := map[string]bool{}
	if snapshot.Kubernetes != nil {
		for _, w := range snapshot.Kubernetes.Workloads {
			if w.Kind == protocol.KindDeployment && w.Namespace == m.Namespace && w.Instance != m.InstanceID {
				held[w.Name] = true
			}
		}
	}
	// A service slug collision moves an applied service's objects: renamed, the old Deployment
	// would keep running beside the new one; taken over by another service, the API server would
	// refuse the Deployment's changed selector at apply.
	was := protocol.KubernetesNames(m.Preview.Project, applied)
	appliedBy := map[string]string{}
	for service, name := range was {
		appliedBy[name] = service
	}
```

and directly after the `k8s_name` check (`codes = append(codes, "k8s_name")` and its closing `}`) insert:

```go
		if held[objects[s.Name]] {
			codes = append(codes, "k8s_name_taken")
		}
		if old, ok := was[s.Name]; ok && old != objects[s.Name] || appliedBy[objects[s.Name]] != "" && appliedBy[objects[s.Name]] != s.Name {
			codes = append(codes, "k8s_service_renamed")
		}
```

In `internal/store/application_apply.go` replace `removalServices` and its comment:

```go
// removalServices names what a cluster removal deletes: the services of the latest revision and
// of the one last applied (current, 0 when none). The agent finds each service's Deployment,
// Service and ConfigMap by the instance label, and its Secret by the name the service gives it.
func (t *tenancyStore) removalServices(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, head, current int) ([]string, error) {
	services := map[string]bool{}
	for _, number := range []int{head, current} {
```

with

```go
// revisionServices is the sorted union of the services of the application's revisions numbers,
// skipping 0 (none). A cluster removal names the latest and the last-applied revisions' services
// (the agent finds each service's Deployment, Service and ConfigMap by the instance label, and its
// Secret by the name the service gives it); a cluster plan compares the last-applied names.
func (t *tenancyStore) revisionServices(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, numbers ...int) ([]string, error) {
	services := map[string]bool{}
	for _, number := range numbers {
```

and its caller in `RemoveApplication`: `t.removalServices(ctx, tx, a, appID.String(), head, current)` becomes `t.revisionServices(ctx, tx, a, appID.String(), head, current)`.

In `internal/agent/protocol/inspection.go` replace the end of `UnsupportedCodes`, `"k8s_restart", "k8s_name", "k8s_namespace"}`, with `"k8s_restart", "k8s_name", "k8s_namespace", "k8s_name_taken", "k8s_service_renamed"}`. In `internal/agent/protocol/inspection_test.go` replace

```go
	if len(UnsupportedCodes) != 34 || len(UnsupportedCodes) > MaxUnsupported || UnsupportedCodes[0] != "mount_type" || UnsupportedCodes[28] != "image_config" || UnsupportedCodes[33] != "k8s_namespace" {
```

with

```go
	if len(UnsupportedCodes) != 36 || len(UnsupportedCodes) > MaxUnsupported || UnsupportedCodes[0] != "mount_type" || UnsupportedCodes[28] != "image_config" || UnsupportedCodes[33] != "k8s_namespace" || UnsupportedCodes[35] != "k8s_service_renamed" {
```

and in `internal/agent/protocol/deployment_test.go` the comment `// 311 bytes: past the step bound` becomes `// 404 bytes: past the step bound` (the joined vocabulary's length now; the old count was already stale).

- [ ] **Step 4: Narrow health decode and one env loop**

In `internal/store/endpoints.go`, `decorate`, replace

```go
		if err == nil {
			var s protocol.Snapshot
			if json.Unmarshal([]byte(raw), &s) == nil {
				snap = &s
			}
		}
```

with

```go
		// Health needs the nodes and the cut lists only: decode nothing else of the snapshot.
		var s struct {
			Kubernetes *struct {
				Nodes []protocol.Node `json:"nodes"`
			} `json:"kubernetes"`
			Truncated []string `json:"truncated"`
		}
		if err == nil && json.Unmarshal([]byte(raw), &s) == nil && s.Kubernetes != nil {
			snap = &protocol.Snapshot{Truncated: s.Truncated, Kubernetes: &protocol.KubernetesInventory{Nodes: s.Kubernetes.Nodes}}
		}
```

In `internal/store/application_apply.go`, insert above `// frameBlocker names what stops req`:

```go
// serviceEnv is a service's environment with each reference resolved to its value.
func serviceEnv(s ApplicationService, values map[string]string) map[string]string {
	env := make(map[string]string, len(s.Environment))
	for name, ref := range s.Environment {
		env[name] = values[ref.SecretRef]
	}
	return env
}
```

In `buildDeploymentFrame` and in `kubernetesFrame`, replace `Env: map[string]string{},` in the `protocol.DeploymentService{...}` literal with `Env: serviceEnv(spec.Services[i], values),`, and delete in each the loop

```go
		for envName, ref := range spec.Services[i].Environment {
			svc.Env[envName] = values[ref.SecretRef]
		}
```

(In `kubernetesFrame` keep the comment `// Every value is secret-backed today: the definition holds references only.` above `svc.SecretKeys = ...`.)

- [ ] **Step 5: Web sentences for the two codes**

In `web/src/components/ApplicationInspection.tsx`, `unsupportedNames`, after the `k8s_namespace` entry add:

```ts
  k8s_name_taken: 'plans a Deployment name another application or tool already uses in the namespace; rename the service or map the application to another namespace',
  k8s_service_renamed: "shares its Kubernetes object name with another service once '_' and '.' read as '-', which would move a running service's objects; give the new service a distinct name",
```

In `web/src/components/ApplicationDeploymentPlan.test.tsx`, test `names each service a Kubernetes plan refuses, with the fix`: in the 409 body replace `{ name: 'web', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_host_ip', 'k8s_restart'] }] }), { status: 409 });` with

```ts
{ name: 'web', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_host_ip', 'k8s_restart'] }, { name: 'api', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_name_taken'] }, { name: 'a_b', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_service_renamed'] }] }), { status: 409 });
```

and after `expect(alert.textContent).toContain('web: publishes a port on a host address');` add

```ts
  expect(alert.textContent).toContain('api: plans a Deployment name another application or tool already uses in the namespace');
  expect(alert.textContent).toContain('a_b: shares its Kubernetes object name with another service');
```

`web/dist` is rebuilt once in Task 4, which commits every web source change of Tasks 2 and 4.

- [ ] **Step 6: Run everything this touched, on both databases**

Run: `gofmt -w internal/store internal/agent/protocol && gofmt -l cmd internal && go test -count=1 ./internal/store/ ./internal/agent/... ./internal/api/ && PG=… go test -count=1 ./internal/store/ && (cd web && npm ci && npx vitest run src/components/ApplicationDeploymentPlan.test.tsx)`
Expected: `gofmt -l` prints nothing; every Go package `ok` on SQLite and the store `ok` on PostgreSQL; vitest `Tests 36 passed (36)`. (`npm ci` installs `web/node_modules`, which the worktree lacks; later tasks reuse it.)

- [ ] **Step 7: DOX**

Apply to `internal/store/AGENTS.md` (each old text occurs once):

```text
OLD: derived on every read from the stored snapshot with `protocol.ClusterHealth` (`unknown` unless the endpoint is `active` with a cluster inventory); nothing is stored.
NEW: derived on every read from the stored snapshot with `protocol.ClusterHealth` (`unknown` unless the endpoint is `active` with a cluster inventory), decoding only the snapshot's `kubernetes.nodes` and `truncated`; nothing is stored.

OLD: and `k8s_name` (a project or service that is not a label value, or a name collision), plus
NEW: `k8s_name` (a project or service that is not a label value, or a name collision), `k8s_name_taken` (a Deployment in the namespace's inventory under the service's planned name without this instance's label: another application's or another tool's) and `k8s_service_renamed` (a slug collision with the last-applied revision: a service whose `KubernetesNames` name changes, as `a_b` beside a new `a-b`, or a service whose name another applied service holds, as `a_b` renamed `a-b`), plus

OLD: names the services of the latest and the last-applied revisions (`removalServices`,
NEW: names the services of the latest and the last-applied revisions (`revisionServices`,
```

Apply to `internal/agent/AGENTS.md`:

```text
OLD: UnsupportedCodes` ends with `k8s_volume`, `k8s_host_ip`, `k8s_restart`, `k8s_name`, `k8s_namespace` (`MaxUnsupported` 40).
NEW: UnsupportedCodes` ends with `k8s_volume`, `k8s_host_ip`, `k8s_restart`, `k8s_name`, `k8s_namespace`, `k8s_name_taken`, `k8s_service_renamed` (`MaxUnsupported` 40), all plan refusals, never an agent's.
```

`docs/application-schema.md` and `docs/agent-protocol.md` name the codes too; Task 5 updates them with the rest of the operator documents.

- [ ] **Step 8: Commit**

```bash
git add internal/store internal/agent web/src/components/ApplicationInspection.tsx web/src/components/ApplicationDeploymentPlan.test.tsx && make tidy-check lint && git commit -m "fix(store): refuse a held or renamed Kubernetes object name at plan; decode only nodes for cluster health" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: API — permission before runtime, one runtime selection, cluster route tests

**Files:**
- Modify: `internal/store/exec.go` (`CheckExecAccess` becomes `CheckEndpointAccess(action, endpoint)`)
- Modify: `internal/store/image_checks.go` (`CheckApplicationAccess`; `CheckImageUpdateAccess` and it share `checkApplication`)
- Modify: `internal/store/commands.go` (`CommandPermission`)
- Modify: `internal/store/store.go` (the `TenancyStore` interface)
- Modify: `internal/api/runtime_gate.go` (doc comment; `adoptionGate`, `runtimeCapability`)
- Modify: `internal/api/application_handlers.go` (adoption preview, adoption, mapping PUT, plan, apply, removal)
- Modify: `internal/api/endpoint_handlers.go` (commands, removal preview comment), `internal/api/exec_handlers.go`
- Test: `internal/api/kubernetes_deploy_test.go` (three new tests), `internal/store/kubernetes_mapping_test.go` (`TestAccessChecksAuditOnlyDenials`)
- Docs: `internal/api/AGENTS.md`, `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: `tenancyStore.readTenant`, `tenancyStore.run`, `tenancyStore.endpointInScope`, `commandActions` (store); `runtimeGate`, `routeKind`, `dockerRoute`, `applicationRoute`, `planInspections` (api); test helpers `newClusterHost`, `clusterHost.{do,sync,importApp}`, `clusterCapabilities`, `loginAs`, `tenantRequest`, `waitFor`, `readEnvelope`, `writeEnvelope`.
- Produces:
  ```go
  // package store; TenancyStore gains the first two (CheckExecAccess is removed; its one caller moves)
  CheckEndpointAccess(ctx context.Context, access TenantAccess, action permissions.Action, endpointID string) error
  CheckApplicationAccess(ctx context.Context, access TenantAccess, action permissions.Action, applicationID string) error
  func CommandPermission(action string) (permissions.Action, bool)
  // package api (unexported)
  func (s *Server) adoptionGate(w http.ResponseWriter, r *http.Request, a store.TenantAccess, app, endpointID string, kind routeKind) bool
  func runtimeCapability(ep *store.Endpoint, namespace, docker, cluster string) (string, error)
  ```

- [ ] **Step 1: Write the tests**

Append to `internal/api/kubernetes_deploy_test.go` (every import is already there):

```go
// A member without the action's own permission is refused 403 with a denied audit row on every
// route aimed at a cluster, never told the runtime first. The removal preview's and inspection's
// permission is endpoint.read, which a viewer holds: they learn the runtime there.
func TestViewerOnClusterRoutes(t *testing.T) {
	h := newClusterHost(t, append(slices.Clone(clusterCapabilities), protocol.CapabilityPodLogs)...)
	app := h.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1}}")
	viewer := loginAs(t, h.s, h.st, "viewer", "user")
	if err := h.st.Tenancy().SetMembership(h.ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	ep := "/api/organizations/a/endpoints/" + h.ag.id
	for _, route := range []struct{ method, path, body, action string }{
		{"POST", ep + "/commands", `{"action":"container.restart","container":"shop-web"}`, "container.operate"},
		{"POST", ep + "/commands", `{"action":"container.remove","container":"shop-web","confirm":"shop-web"}`, "container.destroy"},
		{"GET", app + "/adoption?endpoint=" + h.ag.id + "&project=shop", "", "application.adopt"},
		{"POST", app + "/adoption", `{"endpoint_id":"` + h.ag.id + `","project":"shop","digest":"x","confirm":"shop"}`, "application.adopt"},
		{"PUT", app + "/mapping", `{"endpoint_id":"` + h.ag.id + `","namespace":"shop"}`, "application.adopt"},
		{"GET", ep + "/pods/shop/web-1/logs", "", "container.logs"},
		{"POST", ep + "/manifest", `{"namespaces":["shop"]}`, "endpoint.enroll"},
	} {
		w := tenantRequest(h.s, viewer, route.method, route.path, route.body, true)
		if w.Code != 403 {
			t.Errorf("%s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
			continue
		}
		rows, _, err := h.st.Audit().ListAuditRecords(h.ctx, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(rows, func(row *store.AuditRecord) bool {
			return row.UserID == "usr_viewer" && row.Action == route.action && row.Result == "denied"
		}) {
			t.Errorf("%s %s: no denied %s audit row", route.method, route.path, route.action)
		}
	}
	for _, path := range []string{ep + "/containers/shop-web/removal", ep + "/containers/shop-web/inspection"} {
		if w := tenantRequest(h.s, viewer, "GET", path, "", true); w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
			t.Errorf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
	}
}

// An apply to a cluster whose agent is not connected is refused before anything is recorded:
// the plan stays planned and can be applied once the agent is back.
func TestKubernetesApplyToAnOfflineCluster(t *testing.T) {
	h := newClusterHost(t, clusterCapabilities...)
	app := h.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1}}")
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	_ = json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped)
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop"})
	var planned store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/deployments", string(planBody), 201)), &planned); err != nil {
		t.Fatal(err)
	}
	h.sock.conn.CloseNow()
	waitFor(t, func() bool { return !h.s.Connected(h.ag.id) })
	if body := h.do(t, "POST", app+"/deployments/"+planned.ID+"/apply", `{"confirm":"shop"}`, 409); !strings.Contains(body, "endpoint_offline") {
		t.Fatalf("apply: %s", body)
	}
	var after store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/deployments/"+planned.ID, "", 200)), &after); err != nil || after.State != "planned" {
		t.Fatalf("after %+v %v", after, err)
	}
}

// Removing an application mapped to a cluster but never applied names the latest revision's
// services, and a result that removed nothing releases the instance.
func TestKubernetesRemovalBeforeAnyApply(t *testing.T) {
	h := newClusterHost(t, clusterCapabilities...)
	app := h.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1}}")
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	_ = json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped)
	var removing store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/removal", `{"instance_id":"`+mapped.InstanceID+`","confirm":"shop"}`, 202)), &removing); err != nil {
		t.Fatal(err)
	}
	frame := readEnvelope(t, h.ctx, h.sock.conn)
	var removal protocol.RemovalRequest
	if frame.Type != protocol.TypeDeploymentRemove || json.Unmarshal(frame.Payload, &removal) != nil || removal.Kubernetes == nil || !slices.Equal(removal.Services, []string{"web"}) {
		t.Fatalf("removal frame %s %+v", frame.Type, removal)
	}
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: removing.ID, RequestID: removing.CorrelationID, Outcome: protocol.OutcomeSucceeded, Services: []protocol.DeploymentIdentity{},
		Steps: []protocol.DeploymentStep{{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded}, {Service: "web", Step: protocol.StepRemove, Outcome: protocol.OutcomeSkipped}}})
	h.sync(t)
	var instances []store.ApplicationInstance
	if err := json.Unmarshal([]byte(h.do(t, "GET", h.base+"/instances", "", 200)), &instances); err != nil || len(instances) != 0 {
		t.Fatalf("instances after removal %+v %v", instances, err)
	}
}
```

Append to `internal/store/kubernetes_mapping_test.go`, adding `"github.com/Busnes-app/kyyard-server/internal/permissions"` to its imports:

```go
// The pre-checks the API runs before its runtime gate write nothing when allowed (the operation
// audits itself) and exactly one denied row when refused.
func TestAccessChecksAuditOnlyDenials(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeCluster(t, ts, a, nil, nil)
	app := kubernetesApp(t, st, a, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1"}}}, nil)
	rows := func(action string) []string {
		t.Helper()
		records, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range records {
			if r.Action == action {
				out = append(out, r.Result)
			}
		}
		return out
	}
	if err := ts.CheckEndpointAccess(ctx, a, permissions.ContainerOperate, cluster); err != nil {
		t.Fatal(err)
	}
	if err := ts.CheckApplicationAccess(ctx, a, permissions.ApplicationAdopt, app.ID); err != nil {
		t.Fatal(err)
	}
	if len(rows("container.operate")) != 0 || len(rows("application.adopt")) != 0 {
		t.Fatalf("an allowed check wrote a row: %v %v", rows("container.operate"), rows("application.adopt"))
	}
	if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CheckEndpointAccess(ctx, a, permissions.ContainerOperate, cluster); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer endpoint check: %v", err)
	}
	if err := ts.CheckApplicationAccess(ctx, a, permissions.ApplicationAdopt, app.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer application check: %v", err)
	}
	if !slices.Equal(rows("container.operate"), []string{"denied"}) || !slices.Equal(rows("application.adopt"), []string{"denied"}) {
		t.Fatalf("denials: %v %v", rows("container.operate"), rows("application.adopt"))
	}
}
```

- [ ] **Step 2: Run them to see which fail**

Run: `go vet ./internal/store/`
Expected: FAIL to compile: `ts.CheckEndpointAccess undefined` and `ts.CheckApplicationAccess undefined`.

Run: `go test -count=1 -run 'TestViewerOnClusterRoutes|TestKubernetesApplyToAnOfflineCluster|TestKubernetesRemovalBeforeAnyApply' ./internal/api/`
Expected: FAIL in `TestViewerOnClusterRoutes` only, four lines `409 {"code":"runtime_unsupported",...}`: both commands, the adoption preview and the adoption. The mapping, pod-log and manifest routes already answer 403 with a denied row; `TestKubernetesApplyToAnOfflineCluster` (409 `endpoint_offline`, the plan still `planned`) and `TestKubernetesRemovalBeforeAnyApply` (latest services named, the instance released) pass: they pin the review's untested paths.

- [ ] **Step 3: Store permission checks**

In `internal/store/exec.go` replace

```go
// CheckExecAccess gates WebSocket admission without granting runtime authority.
func (t *tenancyStore) CheckExecAccess(ctx context.Context, a TenantAccess, endpointID string) error {
	return t.readTenant(ctx, a, permissions.ContainerExec, func(tx *sql.Tx) error { return t.endpointInScope(ctx, tx, a, endpointID) })
}
```

with

```go
// CheckEndpointAccess authorizes action on an endpoint in scope, with no success row and a
// denied one on refusal. The API runs it before its runtime gate, so a member without the
// permission is told so rather than told the endpoint's runtime; it grants no runtime authority.
func (t *tenancyStore) CheckEndpointAccess(ctx context.Context, a TenantAccess, action permissions.Action, endpointID string) error {
	return t.readTenant(ctx, a, action, func(tx *sql.Tx) error { return t.endpointInScope(ctx, tx, a, endpointID) })
}
```

In `internal/store/image_checks.go` replace the head of `CheckImageUpdateAccess`

```go
func (t *tenancyStore) CheckImageUpdateAccess(ctx context.Context, a TenantAccess, app string) error {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return ErrInvalid
	}
	target := id.String() + "/updates"
	return t.run(ctx, a, permissions.ApplicationDeploy, &target, nil, false, func(tx *sql.Tx) error {
```

with

```go
func (t *tenancyStore) CheckImageUpdateAccess(ctx context.Context, a TenantAccess, app string) error {
	return t.checkApplication(ctx, a, permissions.ApplicationDeploy, app, "/updates")
}

// CheckApplicationAccess authorizes action on an application in scope, audited on the
// application only when denied.
func (t *tenancyStore) CheckApplicationAccess(ctx context.Context, a TenantAccess, action permissions.Action, app string) error {
	return t.checkApplication(ctx, a, action, app, "")
}

// checkApplication authorizes action on app, auditing a denial on app+suffix.
func (t *tenancyStore) checkApplication(ctx context.Context, a TenantAccess, action permissions.Action, app, suffix string) error {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return ErrInvalid
	}
	target := id.String() + suffix
	return t.run(ctx, a, action, &target, nil, false, func(tx *sql.Tx) error {
```

(the body of the closure is unchanged).

In `internal/store/commands.go` insert above `// destructivePermissions are`:

```go
// CommandPermission is the permission a command action needs; false for an unknown action.
func CommandPermission(action string) (permissions.Action, bool) {
	p, ok := commandActions[action]
	return p, ok
}
```

In `internal/store/store.go`, `TenancyStore`: replace `CheckExecAccess(ctx context.Context, access TenantAccess, endpointID string) error` with `CheckEndpointAccess(ctx context.Context, access TenantAccess, action permissions.Action, endpointID string) error`, and after `CheckImageUpdateAccess(ctx context.Context, access TenantAccess, applicationID string) error` add:

```go
	// CheckApplicationAccess authorizes action on an application in scope the same way, audited
	// on the application when denied; the API runs it before its runtime gate.
	CheckApplicationAccess(ctx context.Context, access TenantAccess, action permissions.Action, applicationID string) error
```

- [ ] **Step 4: API ordering and one runtime selection**

In `internal/api/runtime_gate.go` add `"github.com/Busnes-app/kyyard-server/internal/permissions"` to the imports, replace the `runtimeGate` doc comment with

```go
// runtimeGate refuses, with 409 runtime_unsupported, a Docker route aimed at an endpoint that is
// not Docker. It runs after the action's own permission (adoptionGate, CheckEndpointAccess) and
// before any capability check, so a member without the permission is told that, and one with it
// is told the runtime rather than asked for an agent upgrade that would not help. It writes the
// response and reports false on refusal.
```

and append:

```go
// adoptionGate authorizes application.adopt, the permission of adoption and service mapping, on
// the application, then runs the runtime gate for the endpoint the request aims at.
func (s *Server) adoptionGate(w http.ResponseWriter, r *http.Request, a store.TenantAccess, app, endpointID string, kind routeKind) bool {
	if err := s.store.Tenancy().CheckApplicationAccess(r.Context(), a, permissions.ApplicationAdopt, app); err != nil {
		s.tenantError(w, err)
		return false
	}
	return s.runtimeGate(w, r, a, endpointID, kind)
}

// runtimeCapability holds a plan or an instance to its endpoint's runtime (a namespace on a
// cluster, none on Docker) and names the capability the action needs there.
func runtimeCapability(ep *store.Endpoint, namespace, docker, cluster string) (string, error) {
	kube := ep.Runtime == protocol.RuntimeKubernetes
	if kube != (namespace != "") {
		return "", store.ErrRuntimeUnsupported
	}
	if kube {
		return cluster, nil
	}
	return docker, nil
}
```

In `internal/api/application_handlers.go`:
- `handleAdoptionPreview`: `!s.runtimeGate(w, r, a, endpoint, dockerRoute)` becomes `!s.adoptionGate(w, r, a, r.PathValue("application"), endpoint, dockerRoute)`.
- `handleAdoption`: `!s.runtimeGate(w, r, a, input.EndpointID, dockerRoute)` becomes `!s.adoptionGate(w, r, a, r.PathValue("application"), input.EndpointID, dockerRoute)`.
- `handleSetApplicationMapping`: replace

```go
	// A Kubernetes mapping names its endpoint; a Docker one its adopted instance. One that
	// cannot be read is the store's to refuse, with its own answer.
	endpoint := input.EndpointID
	if instance, err := s.store.Tenancy().ReadApplicationInstance(r.Context(), a, r.PathValue("application"), input.InstanceID); endpoint == "" && err == nil {
		endpoint = instance.EndpointID
	}
	if endpoint != "" && !s.runtimeGate(w, r, a, endpoint, applicationRoute) {
		return
	}
```

with

```go
	// A Kubernetes mapping names its endpoint; a Docker one its adopted instance, read only
	// then. One that cannot be read is the store's to refuse, with its own answer.
	endpoint := input.EndpointID
	if endpoint == "" {
		if instance, err := s.store.Tenancy().ReadApplicationInstance(r.Context(), a, r.PathValue("application"), input.InstanceID); err == nil {
			endpoint = instance.EndpointID
		}
	}
	if endpoint != "" && !s.adoptionGate(w, r, a, r.PathValue("application"), endpoint, applicationRoute) {
		return
	}
```

- `handlePlanDeployment`: replace

```go
	// Inspect before taking a registry slot, so slow agents cannot hold the organization's slots.
	input.Inspections = s.planInspections(w, r, a, ep, pre)
```

with

```go
	// Inspect before taking a registry slot, so slow agents cannot hold the organization's slots.
	// A cluster has no containers to inspect.
	if !kube {
		input.Inspections = s.planInspections(w, r, a, ep, pre)
	}
```

- `handleApplyDeployment`: replace

```go
	kube := ep.Runtime == protocol.RuntimeKubernetes
	if kube != (plan.Plan.Namespace != "") {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return
	}
	deploys := protocol.CapabilityDeploymentApply
	if kube {
		deploys = protocol.CapabilityKubernetesDeploy
	}
```

with

```go
	deploys, err := runtimeCapability(ep, plan.Plan.Namespace, protocol.CapabilityDeploymentApply, protocol.CapabilityKubernetesDeploy)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	kube := plan.Plan.Namespace != ""
```

(keep the comment line above it; `kube` still guards the pull capability check below).
- `handleRemoveApplication`: replace

```go
	kube := ep.Runtime == protocol.RuntimeKubernetes
	if kube != (instance.Namespace != "") {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return
	}
	removes := protocol.CapabilityDeploymentRemove
	if kube {
		removes = protocol.CapabilityKubernetesRemove
	}
```

with

```go
	removes, err := runtimeCapability(ep, instance.Namespace, protocol.CapabilityDeploymentRemove, protocol.CapabilityKubernetesRemove)
	if err != nil {
		s.tenantError(w, err)
		return
	}
```

In `internal/api/exec_handlers.go` replace `s.store.Tenancy().CheckExecAccess(r.Context(), a, endpoint)` with `s.store.Tenancy().CheckEndpointAccess(r.Context(), a, permissions.ContainerExec, endpoint)`.

In `internal/api/endpoint_handlers.go`, `handleDispatchCommand`: delete the `if !s.runtimeGate(w, r, a, id, dockerRoute) { return }` block that follows `endpointID(r)`, and insert, directly above `target := body.Container`:

```go
	// The action's own permission before the runtime; an unknown action is the store's to refuse.
	if needs, ok := store.CommandPermission(body.Action); ok {
		if err := s.store.Tenancy().CheckEndpointAccess(r.Context(), a, needs, id); err != nil {
			s.tenantError(w, err)
			return
		}
	}
	if !s.runtimeGate(w, r, a, id, dockerRoute) {
		return
	}
```

In `handleRemovalPreview` put `// The preview's permission is endpoint.read, which runtimeGate's endpoint read checks.` on the line above its `runtimeGate` call.

- [ ] **Step 5: Run the API and store suites, both databases**

Run: `gofmt -w internal/api internal/store && gofmt -l cmd internal && go vet ./internal/api/ ./internal/store/ && go test -count=1 ./internal/api/ ./internal/store/ && PG=… go test -count=1 ./internal/api/ ./internal/store/`
Expected: `gofmt -l` prints nothing; `ok` for both packages on SQLite and on PostgreSQL. `TestDockerRoutesRefuseAKubernetesEndpoint` and `TestRuntimeGateMatrix` still pass: an administrator holds every permission, so the order is invisible to them.

- [ ] **Step 6: DOX**

Apply to `internal/api/AGENTS.md`:

```text
OLD: Exec and inspection run the gate after their per-actor rate limit and permission check (`CheckExecAccess`; inspection's `endpoint.read` is the gate's own endpoint read), so a member without the permission gets 403 and a `denied` audit row, not the runtime; the other routes run it before their action's own permission check.
NEW: Every gated route checks the action's own permission first, so a member without it gets 403 and a `denied` audit row, never the runtime: exec after its per-actor rate limit (`CheckEndpointAccess(container.exec)`), commands after parsing the body (`CheckEndpointAccess` with `store.CommandPermission(action)`; an unknown action is left to the store), adoption preview, adoption and service mapping through `adoptionGate` (`CheckApplicationAccess(application.adopt)`); inspection's and the removal preview's permission is `endpoint.read`, the gate's own endpoint read. The mapping PUT reads the instance only when the body names no endpoint. Apply and application removal pick the capability with `runtimeCapability` (the plan's or instance's shape against the endpoint's runtime, 409 on a mismatch), and a cluster plan runs no container inspection.
```

Apply to `internal/store/AGENTS.md`:

```text
OLD: - `CheckExecAccess` authorizes socket admission through the tenant read path.
NEW: - `CheckEndpointAccess(action, endpoint)` and `CheckApplicationAccess(action, application)` authorize an action through the tenant read path (no success row, a `denied` row on refusal) and grant nothing: the API runs them before its runtime gate, `CheckEndpointAccess(container.exec)` also for socket admission. `CommandPermission` names the permission a command action needs.
```

- [ ] **Step 7: Commit**

```bash
git add internal/api internal/store && make tidy-check lint && git commit -m "fix(api): check the action's permission before the runtime gate; one runtime capability selection" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Web — shared badge, aria-expanded, enrollment namespace check, cluster sentences, edge states

**Files:**
- Modify: `web/src/tenant.ts` (`validNamespaces`, `NAMESPACE_RULE`)
- Modify: `web/src/components/Endpoints.tsx` (export `healthBadge`; enrollment namespace check)
- Modify: `web/src/components/KubernetesCluster.tsx` (import `healthBadge`)
- Modify: `web/src/components/KubernetesManifest.tsx` (`NAMESPACE_RULE`, `aria-expanded`)
- Modify: `web/src/components/ApplicationDeploymentPlan.tsx` (`POD_SECURITY`, `CLUSTER_STOPPED`, `CLUSTER_FAILED`, cluster explanations, `aria-expanded` on both toggles)
- Modify (`aria-expanded` only): `web/src/components/{ApplicationUpdates,ApplicationPreflight,ApplicationMapping,ApplicationRevisionEditor,ApplicationPolicy,ApplicationComparison,ContainerControls}.tsx`
- Create: `web/src/components/KubernetesCluster.test.tsx`
- Test: `web/src/components/Endpoints.test.tsx`, `web/src/components/KubernetesManifest.test.tsx`, `web/src/components/ApplicationDeploymentPlan.test.tsx`
- Build: `web/dist`, `web/tsconfig.tsbuildinfo` (every web source change of Tasks 2 and 4)
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes: `parseNamespaces`, `stepText`, `STEP_CODES`, `explanationFor`, test helpers `plan`, `props`, `stubFetch` (`ApplicationDeploymentPlan.test.tsx`), `json`, `pending` (`Endpoints.test.tsx`), `endpoint` (`KubernetesManifest.test.tsx`).
- Produces:
  ```ts
  // web/src/tenant.ts
  export const validNamespaces: (names: string[]) => boolean;
  export const NAMESPACE_RULE: string;
  // web/src/components/Endpoints.tsx
  export const healthBadge: Record<string, string>;
  // web/src/components/ApplicationDeploymentPlan.tsx
  export const CLUSTER_STOPPED: string;
  export const CLUSTER_FAILED: string;
  ```

- [ ] **Step 1: Write the tests**

In `web/src/components/Endpoints.test.tsx`, add `import { NAMESPACE_RULE } from '../tenant';` below the `./Endpoints` import and append:

```tsx
it('checks the namespace list before enrolling a cluster, by the rule the server applies', async () => {
  const fetcher = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => json([]));
  vi.stubGlobal('fetch', fetcher);
  render(<Endpoints org="a" env="env-a" />);
  fireEvent.change(screen.getByRole('combobox', { name: 'Runtime' }), { target: { value: 'kubernetes' } });
  fireEvent.change(screen.getByRole('textbox', { name: 'Cluster name' }), { target: { value: 'prod' } });
  const namespaces = screen.getByRole('textbox', { name: 'Namespaces to deploy to' });
  const enroll = screen.getByRole('button', { name: 'Enroll a cluster' });
  for (const bad of ['Shop', 'kube-system', 'kyyard-agent', 'shop shop', 'shop_', 'a'.repeat(64), Array.from({ length: 33 }, (_, i) => `ns${i}`).join(' ')]) {
    fireEvent.change(namespaces, { target: { value: bad } });
    expect(enroll).toHaveProperty('disabled', true);
    expect(screen.getByRole('alert').textContent).toBe(NAMESPACE_RULE);
    expect(namespaces.getAttribute('aria-invalid')).toBe('true');
  }
  for (const good of ['', 'shop billing', 'shop, kube', 'a'.repeat(63), Array.from({ length: 32 }, (_, i) => `ns${i}`).join(' ')]) {
    fireEvent.change(namespaces, { target: { value: good } });
    expect(enroll).toHaveProperty('disabled', false);
    expect(screen.queryByRole('alert')).toBeNull();
  }
  expect(fetcher.mock.calls.every(([, init]) => init?.method !== 'POST')).toBe(true);
});
```

Append to `web/src/components/KubernetesManifest.test.tsx`:

```tsx
it('says whether the namespace form is open', () => {
  render(<ManifestRegeneration org="a" endpoint={endpoint} onSaved={vi.fn()} />);
  const toggle = screen.getByRole('button', { name: 'Regenerate manifest' });
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
});
```

In `web/src/components/ApplicationDeploymentPlan.test.tsx`, change the component import to `import { ApplicationDeploymentPlan, CLUSTER_FAILED, CLUSTER_STOPPED, STEP_CODES, stepText } from './ApplicationDeploymentPlan';`; in `renders a Kubernetes plan, its step codes and Deployment identities` replace the assertion

```tsx
  expect(screen.getByText('The namespace does not enforce Pod Security baseline; label it pod-security.kubernetes.io/enforce=baseline (or restricted) and apply again.')).toBeTruthy();
```

with (its row's detail is `missing`)

```tsx
  expect(screen.getByText('The namespace has no Pod Security enforce label; label it pod-security.kubernetes.io/enforce=baseline (or restricted) and apply again.')).toBeTruthy();
```

and append:

```tsx
it('says whether the plan is open', () => {
  vi.stubGlobal('fetch', stubFetch([]));
  render(<ApplicationDeploymentPlan {...props} />);
  const toggle = screen.getByRole('button', { name: 'Deployment plan' });
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
});
it('names which Pod Security case refused a namespace', () => {
  expect(stepText({ code: 'pod_security', detail: 'missing' })).toContain('has no Pod Security enforce label');
  expect(stepText({ code: 'pod_security', detail: 'privileged' })).toContain('enforces Pod Security privileged');
  expect(stepText({ code: 'pod_security', detail: 'invalid' })).toContain('is not a level Kubernetes knows');
  expect(stepText({ code: 'pod_security', detail: '' })).toBe(STEP_CODES.pod_security);
  expect(stepText({ code: 'pod_security', detail: 'constructor' })).toBe(STEP_CODES.pod_security);
});
it('explains a failed cluster row in cluster terms, never the Docker rename', async () => {
  const cluster = { ...plan, plan: { project: 'shop', namespace: 'shop', services: [{ ...plan.plan.services[0], object: { namespace: 'shop', name: 'shop-web' } }] } };
  const rows = [
    { ...cluster, id: 'd1', state: 'failed', result: { code: 'step_failed', steps: [{ service: 'web', step: 'create', outcome: 'failed', code: 'conflict', detail: 'ConfigMap/shop-web-env' }], services: [] } },
  ];
  vi.stubGlobal('fetch', stubFetch(rows));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect(await screen.findByText(CLUSTER_FAILED)).toBeTruthy();
  expect(document.body.textContent).not.toContain('.kyyard-prev');
  cleanup();
  vi.stubGlobal('fetch', stubFetch([{ ...cluster, id: 'd2', state: 'failed', result: { code: 'step_failed', steps: [{ service: 'web', step: 'precondition', outcome: 'denied', code: 'pod_security', detail: 'privileged' }], services: [] } }]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect(await screen.findByText(CLUSTER_STOPPED)).toBeTruthy();
  expect(document.body.textContent).not.toContain('mapped container');
});
```

Create `web/src/components/KubernetesCluster.test.tsx`:

```tsx
import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { KubernetesCluster } from './KubernetesCluster';
import type { Endpoint, KubernetesInventory } from '../tenant';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const endpoint = { id: 'ep_k', environment_id: 'env-a', name: 'prod', runtime: 'kubernetes', state: 'active', facts: {}, fingerprint: '', capabilities: [], alerts: [], created_at: '', deploy_namespaces: [] } as Endpoint;
const empty: KubernetesInventory = { nodes: [], namespaces: [], workloads: [], pods: [], services: [], claims: [] };
const view = (inventory: KubernetesInventory, e: Endpoint = endpoint) => <KubernetesCluster org="a" base="/api/organizations/a/endpoints/ep_k" endpoint={e} inventory={inventory} instances={[]} admin={false} onChanged={vi.fn()} />;

it('shows a cluster that reports no nodes as unknown, not healthy', () => {
  render(view(empty));
  const health = screen.getByRole('region', { name: 'Cluster health' });
  expect(health.textContent).toContain('unknown');
  expect(health.textContent).toContain('0 of 0 nodes ready.');
  expect(screen.getByText('No nodes reported.')).toBeTruthy();
});

it('lists a pod with no containers reported yet, with nothing to open', () => {
  render(view({ ...empty, namespaces: ['shop'], pods: [{ namespace: 'shop', name: 'web-1', phase: 'Pending', node: '', owner_kind: '', owner_name: '', started_at: '', containers: [] }] }));
  expect(screen.getByText('shop/web-1')).toBeTruthy();
  expect(screen.getByText('Pending')).toBeTruthy();
  expect(screen.queryByRole('button', { name: /^Logs for/ })).toBeNull();
});

it('drops a namespace filter whose namespace left the cluster', () => {
  const pod = (namespace: string, name: string) => ({ namespace, name, phase: 'Running', node: 'n1', owner_kind: '', owner_name: '', started_at: '', containers: [] });
  const both = { ...empty, namespaces: ['billing', 'shop'], pods: [pod('billing', 'pay-1'), pod('shop', 'web-1')] };
  const { rerender } = render(view(both));
  fireEvent.change(screen.getByRole('combobox', { name: 'Namespace' }), { target: { value: 'shop' } });
  expect(screen.queryByText('billing/pay-1')).toBeNull();
  rerender(view({ ...both, namespaces: ['billing'], pods: [pod('billing', 'pay-1')] }));
  expect(screen.getByRole('combobox', { name: 'Namespace' })).toHaveProperty('value', '');
  expect(screen.getByText('billing/pay-1')).toBeTruthy();
});
```

- [ ] **Step 2: Run them to see which fail**

Run: `cd web && npx vitest run src/components/KubernetesCluster.test.tsx src/components/Endpoints.test.tsx src/components/KubernetesManifest.test.tsx src/components/ApplicationDeploymentPlan.test.tsx; cd ..`
Expected: FAIL in five tests: `says whether the namespace form is open` and `says whether the plan is open` (`expected null to be 'false'`), `checks the namespace list before enrolling a cluster...` (the button stays enabled), `names which Pod Security case refused a namespace` (one sentence for every case), `explains a failed cluster row in cluster terms...` (`CLUSTER_FAILED` is undefined). The three `KubernetesCluster.test.tsx` edge-state tests pass: they pin the zero-node, empty-container and vanished-namespace behaviour the review found untested.

- [ ] **Step 3: Implement**

In `web/src/tenant.ts`, directly below `export const parseNamespaces = ...`, add:

```ts
// validNamespaces is the server's rule for a deploy namespace list (store.NormalizeNamespaces):
// at most 32 distinct DNS-1123 labels, none kyyard-agent or kube-*. The server stays the authority.
export const validNamespaces = (names: string[]) => names.length <= 32 && new Set(names).size === names.length
  && names.every((n) => /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/.test(n) && n !== 'kyyard-agent' && !n.startsWith('kube-'));
export const NAMESPACE_RULE = 'List at most 32 namespaces by name: lower-case letters, digits and hyphens; not kyyard-agent or a kube- namespace.';
```

In `web/src/components/Endpoints.tsx`:
- The `../tenant` import becomes `import { NAMESPACE_RULE, parseNamespaces, tenantWrite, useTenantResource, validNamespaces, type Endpoint, type EnrollmentToken } from '../tenant';`.
- `const healthBadge: Record<string, string> = { healthy: 'badge-success', degraded: 'badge-danger' };` becomes

```tsx
// healthBadge colours a cluster_health; anything else (unknown) is secondary.
export const healthBadge: Record<string, string> = { healthy: 'badge-success', degraded: 'badge-danger' };
```

- Below `const [busy, setBusy] = useState(false);` add `const namespacesValid = validNamespaces(parseNamespaces(namespaces));`.
- Replace the namespaces input, the enroll button and the closing `</div>` of the panel header

```tsx
        {runtime === 'kubernetes' && <input aria-label="Namespaces to deploy to" placeholder="Namespaces to deploy to (optional)" value={namespaces} onChange={(e) => setNamespaces(e.target.value)} />}
        <button disabled={busy || (runtime === 'kubernetes' && clusterName.trim() === '')} onClick={() => void mint()}>{runtime === 'kubernetes' ? 'Enroll a cluster' : 'Enroll a host'}</button>
      </div>
```

with

```tsx
        {runtime === 'kubernetes' && <input aria-label="Namespaces to deploy to" placeholder="Namespaces to deploy to (optional)" aria-invalid={!namespacesValid} value={namespaces} onChange={(e) => setNamespaces(e.target.value)} />}
        <button disabled={busy || (runtime === 'kubernetes' && (clusterName.trim() === '' || !namespacesValid))} onClick={() => void mint()}>{runtime === 'kubernetes' ? 'Enroll a cluster' : 'Enroll a host'}</button>
      </div>
      {runtime === 'kubernetes' && !namespacesValid && <p role="alert">{NAMESPACE_RULE}</p>}
```

In `web/src/components/KubernetesCluster.tsx`: `import { displayName } from './Endpoints';` becomes `import { displayName, healthBadge } from './Endpoints';` and delete its local `const healthBadge ...` line.

In `web/src/components/KubernetesManifest.tsx`: the `../tenant` import becomes `import { NAMESPACE_RULE, offlineWrite, parseNamespaces, refusal, type Endpoint } from '../tenant';`, the refusal's `invalid: 'List at most 32 namespaces by name: lower-case letters, digits and hyphens; not kyyard-agent or a kube- namespace.' }));` becomes `invalid: NAMESPACE_RULE }));`, and the toggle gains `aria-expanded={open}` before its `onClick`.

`aria-expanded`: add `aria-expanded={open}` directly before `onClick={() => setOpen(!open)}` on every toggle: `ApplicationDeploymentPlan.tsx` (both, "Deployment plan" and "Deployment history"), `ApplicationUpdates.tsx`, `ApplicationPreflight.tsx`, `ApplicationMapping.tsx`, `ApplicationRevisionEditor.tsx`, `ApplicationPolicy.tsx`, `ApplicationComparison.tsx`, `KubernetesManifest.tsx`; in `ContainerControls.tsx` add `aria-expanded={showTerminal}` before `onClick={() => setShowTerminal(!showTerminal)}` and `aria-expanded={showLogs}` before `onClick={() => setShowLogs(!showLogs)}`. Check: `grep -rn 'onClick={() => set[A-Za-z]*(!' web/src --include='*.tsx' | grep -v test | grep -v aria-expanded` prints only `pages/Login.tsx` (the recovery-code switch, which swaps an input rather than showing a panel).

In `web/src/components/ApplicationDeploymentPlan.tsx`, `stepText`: before `case 'rollout_timeout': {` add

```tsx
    case 'pod_security':
      return Object.hasOwn(POD_SECURITY, detail) ? POD_SECURITY[detail] ?? text : text;
```

above `// The closed detail shapes of the Kubernetes codes: Kind/name, and condition=Reason words.` add

```tsx
// A pod_security refusal by the namespace's enforce label: absent, privileged, or not a level.
const POD_SECURITY: Record<string, string> = {
  missing: 'The namespace has no Pod Security enforce label; label it pod-security.kubernetes.io/enforce=baseline (or restricted) and apply again.',
  privileged: 'The namespace enforces Pod Security privileged, which lets a pod run privileged; set pod-security.kubernetes.io/enforce=baseline (or restricted) and apply again.',
  invalid: "The namespace's Pod Security enforce label is not a level Kubernetes knows; set pod-security.kubernetes.io/enforce=baseline (or restricted) and apply again.",
};
```

above `// The row's state decides first:` add

```tsx
export const CLUSTER_STOPPED = 'The agent stopped before changing the cluster; the step below says why.';
export const CLUSTER_FAILED = 'A step failed on the cluster; objects written before it stay as applied, and nothing was rolled back.';
const CLUSTER_REMOVE_FAILED = 'A step failed on the cluster; objects removed before it are gone and the rest remain.';
```

and in `explanationFor`, directly after `if (!failing) return '';`, add

```tsx
  if (current.plan.namespace) {
    if (failing.outcome === 'denied' && failing.step === 'precondition') return CLUSTER_STOPPED;
    if (failing.outcome === 'failed') return current.kind === 'remove' ? CLUSTER_REMOVE_FAILED : CLUSTER_FAILED;
  }
```

(A cluster precondition never writes: `Deploy` and `Remove` run every precondition before the first write, so "stopped before changing the cluster" is exact.)

- [ ] **Step 4: Run the web suite and type check**

Run: `cd web && npx vitest run && npx tsc -b --noEmit; cd ..`
Expected: `Test Files 30 passed (30)`, `Tests 247 passed (247)`; `tsc` prints nothing.

- [ ] **Step 5: DOX**

Apply to `web/AGENTS.md`:

```text
OLD: A cluster needs a name before "Enroll a cluster" enables; it posts
NEW: A cluster needs a name, and a namespace list that passes `validNamespaces` (the server's rule: at most 32 distinct DNS-1123 labels, not `kyyard-agent`, not `kube-*`; otherwise the field is `aria-invalid` and `NAMESPACE_RULE` shows as an alert), before "Enroll a cluster" enables; it posts

OLD: Endpoint rows show a Kubernetes endpoint's `cluster_health` badge and node count.
NEW: Endpoint rows show a Kubernetes endpoint's `cluster_health` badge (`healthBadge`, shared with `KubernetesCluster.tsx`) and node count.

OLD: step codes `forbidden`, `pod_security` (label the namespace baseline or restricted),
NEW: step codes `forbidden`, `pod_security` (one sentence per detail: `missing`, `privileged`, `invalid`; label the namespace baseline or restricted),

OLD: history lists Deployment identities;
NEW: history lists Deployment identities; a cluster row's explanation reads `CLUSTER_STOPPED` for a precondition refusal and `CLUSTER_FAILED` (or the removal sentence) for a failed step, never the Docker `.kyyard-prev` text;
```

and under `## Shared browser UI`, above the bullet beginning `- Products own layout, routes,`, add:

```text
- Every show/hide toggle button (the `setOpen(!open)` panels, the container Logs and Terminal buttons) sets `aria-expanded` to its open state.
```

- [ ] **Step 6: Build `web/dist` and commit**

`make ci` must not be running (both run `npm ci` in `web/`).

```bash
make build-web && git add web && make tidy-check lint && git commit -m "fix(web): name each cluster refusal, check namespaces at enrollment, aria-expanded on toggles" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

`git add web` stages the removed and added hashed bundles under `web/dist/assets`, `web/dist/index.html` and `web/tsconfig.tsbuildinfo` together with the sources.

---

### Task 5: Docs — a test pinning the blast radius, README addressing and upgrade rule, protocol and schema

**Files:**
- Create: `internal/api/disclosure_internal_test.go`
- Modify: `README.md` (`### Kubernetes endpoints`)
- Modify: `docs/threat-model.md` (the Kubernetes agent row's test column)
- Modify: `docs/agent-protocol.md` (`## Kubernetes runtime`), `docs/application-schema.md` (`## Kubernetes (M8)`)
- Docs: `internal/api/AGENTS.md` (`## Verification`)

**Interfaces:**
- Consumes: `clusterDisclosure`, `namespaceDisclosure` (package `api`, `endpoint_handlers.go`); `manifest.RenderRBAC(name string, namespaces []string) (string, error)`; the codes and behaviour of Tasks 1–3.
- Produces: `TestClusterDisclosureMatchesTheManifestAndThreatModel`; documents only.

- [ ] **Step 1: Write the pinning test**

Create `internal/api/disclosure_internal_test.go`:

```go
package api

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/manifest"
)

// clusterReads names each resource the manifest's ClusterRole reads the way the enrollment
// disclosure and the threat model do. A resource added to the ClusterRole fails this test until
// both documents name it.
var clusterReads = map[string][2]string{ // resource: {disclosure, threat model}
	"namespaces": {"namespaces", "namespaces"}, "nodes": {"nodes", "nodes"}, "pods": {"pods", "pods"},
	"pods/log": {"pod logs", "pod logs"}, "events": {"events", "events"}, "services": {"services", "services"},
	"persistentvolumeclaims": {"persistent volume claims", "persistentvolumeclaims"},
	"deployments":            {"deployments", "deployments"}, "statefulsets": {"statefulsets", "statefulsets"}, "daemonsets": {"daemonsets", "daemonsets"},
}

// The enrollment disclosure and the threat model state the blast radius the manifest grants:
// every read of the ClusterRole, cluster-wide, and nothing it does not grant.
func TestClusterDisclosureMatchesTheManifestAndThreatModel(t *testing.T) {
	rendered, err := manifest.RenderRBAC("prod", []string{"shop"})
	if err != nil {
		t.Fatal(err)
	}
	var role string
	for _, doc := range strings.Split(rendered, "\n---\n") {
		if strings.Contains(doc, "\nkind: ClusterRole\n") {
			role = doc
		}
	}
	reads := regexp.MustCompile(`resources: \[([^\]]*)\]\n\s*verbs: \[get, list\]`).FindAllStringSubmatch(role, -1)
	if len(reads) == 0 {
		t.Fatal("no get/list rule in the ClusterRole")
	}
	raw, err := os.ReadFile("../../docs/threat-model.md")
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "| Compromised Kubernetes agent") {
			row = line
		}
	}
	first, _, _ := strings.Cut(clusterDisclosure, ". ")
	seen := 0
	for _, rule := range reads {
		for _, resource := range strings.Split(rule[1], ", ") {
			words, ok := clusterReads[resource]
			if !ok {
				t.Errorf("the ClusterRole reads %s, which the disclosure and the threat model do not name", resource)
				continue
			}
			seen++
			if !strings.Contains(first, words[0]) || !strings.Contains(row, words[1]) {
				t.Errorf("%s: disclosure %t, threat model %t", resource, strings.Contains(first, words[0]), strings.Contains(row, words[1]))
			}
		}
	}
	if seen != len(clusterReads) {
		t.Errorf("the ClusterRole reads %d resources, the wording names %d", seen, len(clusterReads))
	}
	for _, want := range []string{"in every namespace", "cannot read Secrets or ConfigMaps", "cluster-admin"} {
		if !strings.Contains(clusterDisclosure, want) {
			t.Errorf("the disclosure lost %q", want)
		}
	}
	for _, want := range []string{"no Secrets, no ConfigMaps, no `watch`, no wildcard", "readable cluster-wide by design", "can create any pod"} {
		if !strings.Contains(row, want) {
			t.Errorf("the threat model lost %q", want)
		}
	}
	if !strings.Contains(namespaceDisclosure, "run any pod in those namespaces") {
		t.Error("the namespace disclosure lost its blast radius")
	}
}
```

- [ ] **Step 2: Prove it passes now and fails on drift**

Run: `go test -count=1 -run TestClusterDisclosureMatchesTheManifestAndThreatModel ./internal/api/`
Expected: `ok` (the documents agree today; this test guards the future, as the spec asks).

Then prove each side is guarded, restoring the file after each:

Run: `sed -i 's|persistentvolumeclaims\]|persistentvolumeclaims, endpoints]|' internal/runtime/kubernetes/manifest/manifest.go && go test -count=1 -run TestClusterDisclosureMatchesTheManifestAndThreatModel ./internal/api/; git checkout internal/runtime/kubernetes/manifest/manifest.go`
Expected: FAIL `the ClusterRole reads endpoints, which the disclosure and the threat model do not name`.

Run: `sed -i 's|readable cluster-wide by design|readable everywhere by design|' docs/threat-model.md && go test -count=1 -run TestClusterDisclosureMatchesTheManifestAndThreatModel ./internal/api/; git checkout docs/threat-model.md`
Expected: FAIL `the threat model lost "readable cluster-wide by design"`.

Run: `git status --short internal/runtime docs/threat-model.md`
Expected: nothing (both restored).

- [ ] **Step 3: README**

In `README.md`, `### Kubernetes endpoints`, apply (each old text occurs once):

```text
OLD: (discovered from the installed server image, or `KY_AGENT_IMAGE`), and a cluster-admin
kubeconfig for `kubectl apply`.
NEW: (discovered from the installed server image, or `KY_AGENT_IMAGE`), and a cluster-admin
kubeconfig for `kubectl apply`. Upgrade the KyYard server before any cluster agent, never the
reverse: an older server refuses a newer agent at hello (the full order closes this section).

OLD: touches an object with those names that it did not label (`name_taken`), and the apply waits
NEW: touches an object with those names that it did not label (`name_taken`); the plan already stops
when the cluster's inventory shows another application's or tool's Deployment under a planned
name (`k8s_name_taken`), or when a service's name collides with a running one's once `_` and
`.` read as `-` (`a-b` beside or in place of `a_b`), which would move its objects
(`k8s_service_renamed`). The apply waits

OLD: with `admission_denied` and the object's name. The rendered pod has no resource requests or
NEW: with `admission_denied` and the object's name; a write the agent's Role does not grant (a manifest
older than the agent) stops with `forbidden`: apply the regenerated manifest. The rendered pod has no resource requests or
```

and insert, after the paragraph ending `deletes the objects labelled as its own and nothing else.` and before the paragraph beginning `Container, image, network, volume, terminal,`:

```markdown
Services of one application reach each other through their Kubernetes Service, by its object
name (`<application>-<service>`, for example `shop-web`, or `shop-web.shop.svc.cluster.local`
from another namespace) on the service's published port, which the Service forwards to the
target port. The Compose service name (`web`) does not resolve on a cluster, and a service with
no published port gets no Service, so nothing can reach it; publish every port another service
calls. A name longer than 63 characters, or two services whose names collide, carry a six-hex
suffix: the plan shows each service's object name.
```

- [ ] **Step 4: Protocol, schema and threat model**

In `docs/agent-protocol.md`, `## Kubernetes runtime`, apply:

```text
OLD: update when owned, create otherwise; a second conflict is `conflict` with `Kind/name`; any other 403 on a write is `admission_denied` with `Kind/name`;
NEW: update when owned, create otherwise, and create when an owned object vanished before its update; a second conflict is `conflict` with `Kind/name`; any other 403 on a write runs a `SelfSubjectAccessReview` for that verb, resource and name: not granted is `forbidden`, granted is `admission_denied` with `Kind/name`;

OLD: `ReplicaFailure=True` or `ProgressDeadlineExceeded` for the current generation is `rollout_timeout` at once with outcome `failed`, and past the deadline `rollout_timeout` with outcome `timed_out`;
NEW: `ReplicaFailure=True` or `ProgressDeadlineExceeded` for the current generation on two consecutive reads is `rollout_timeout` with outcome `failed`; a transient read failure (no answer, 429, 5xx) is retried until the deadline, any other ends the step with its code; past the deadline `rollout_timeout` with outcome `timed_out` and the last good read's reasons;

OLD: - Unsupported codes add `k8s_volume`, `k8s_host_ip`, `k8s_restart`, `k8s_name`, `k8s_namespace` (a plan's refusal; never an agent's).
NEW: - Unsupported codes add `k8s_volume`, `k8s_host_ip`, `k8s_restart`, `k8s_name`, `k8s_namespace`, `k8s_name_taken`, `k8s_service_renamed` (a plan's refusal; never an agent's).
```

In `docs/application-schema.md`, `## Kubernetes (M8)`, apply:

```text
OLD: `k8s_restart` (neither `always`, `unless-stopped` nor default) and `k8s_name` (a project or service that is not a label value);
NEW: `k8s_restart` (neither `always`, `unless-stopped` nor default), `k8s_name` (a project or service that is not a label value), `k8s_name_taken` (the cluster inventory shows a Deployment under the service's planned name that is not this instance's: another application's or another tool's) and `k8s_service_renamed` (a slug collision with the last-applied revision, as `a-b` beside or in place of `a_b`, which would move a running service's objects);

OLD: (a write the cluster refuses: `admission_denied`)
NEW: (a write the cluster refuses: `admission_denied`; one the agent's Role does not grant: `forbidden`)

OLD: at once when the cluster reports `ReplicaFailure` or `ProgressDeadlineExceeded`
NEW: early when the cluster reports `ReplicaFailure` or `ProgressDeadlineExceeded` on two consecutive reads
```

In `docs/threat-model.md`, the row beginning `| Compromised Kubernetes agent or its ServiceAccount token |`, apply:

```text
OLD: `TestHelloCapabilitiesMustFitTheRuntime`, `TestDockerRoutesRefuseAKubernetesEndpoint`, `TestRuntimeGateMatrix` |
NEW: `TestHelloCapabilitiesMustFitTheRuntime`, `TestDockerRoutesRefuseAKubernetesEndpoint`, `TestRuntimeGateMatrix`, `TestViewerOnClusterRoutes`, `TestClusterDisclosureMatchesTheManifestAndThreatModel` (the enrollment disclosure and this row name every ClusterRole read) |
```

Run: `go test -count=1 -run TestClusterDisclosureMatchesTheManifestAndThreatModel ./internal/api/`
Expected: `ok` (the edited row still carries every pinned phrase).

- [ ] **Step 5: DOX**

In `internal/api/AGENTS.md`, `## Verification`, insert above the bullet beginning `` - `scripts/smoke-test.sh` ``:

```text
- `go test -run TestClusterDisclosureMatchesTheManifestAndThreatModel ./internal/api/` fails when the manifest's ClusterRole reads a resource that `clusterDisclosure` and the `docs/threat-model.md` Kubernetes row do not both name, or when either loses its blast-radius wording: change the three together.
```

`README.md` and `docs/` are the root's operator documents (the root `AGENTS.md` names them); the root `AGENTS.md` itself needs no change.

- [ ] **Step 6: Commit**

```bash
gofmt -l cmd internal && git add internal/api/disclosure_internal_test.go internal/api/AGENTS.md README.md docs/threat-model.md docs/agent-protocol.md docs/application-schema.md && make tidy-check lint && git commit -m "docs(k8s): pin the disclosure to the manifest and threat model; addressing, upgrade order, new codes" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: The full gate

**Files:** none changed; this task only runs checks. A failure goes back to the task that owns the file.

**Interfaces:**
- Consumes: every commit of Tasks 1–5.
- Produces: the evidence the PR description quotes.

- [ ] **Step 1: Local CI**

No `make build-web` may be running. Run: `make ci`
Expected: `==> Local CI checks passed` (tidy, lint, race suite with coverage on SQLite, web tests, smoke).

If `ApplicationDeploymentPlan.test.tsx > keeps the panel mounted across polls instead of flashing loading` fails: it failed once in the planning dry run under the parallel gate and passed every isolated and loaded rerun; it predates this slice (one read counter shared by two GETs whose order varies). Rerun `make test-web`; if it fails again, stop and report it rather than patching it here.

- [ ] **Step 2: PostgreSQL**

Run: `PG=… go test -count=1 ./...`
Expected: every package `ok`.

- [ ] **Step 3: Boundaries**

Run: `go list -deps ./cmd/server | grep -c k8s.io`
Expected: `0`.

Run: `git diff --quiet master -- internal/store/migrations && echo no-migration`
Expected: `no-migration`.

Run: `git status --short web/dist web/tsconfig.tsbuildinfo`
Expected: nothing (the committed bundle is the one Task 4 built; if `make ci`'s `npm ci` rewrote `tsconfig.tsbuildinfo`, rerun `make build-web`, then `git status` again, and amend Task 4's content in a follow-up commit rather than leaving it dirty).

- [ ] **Step 4: Real cluster (optional, local only)**

If a disposable cluster is at hand: `KY_TEST_KUBECONFIG=$HOME/.kube/config KY_TEST_DEPLOY_IMAGE=registry.k8s.io/pause@sha256:<digest> go test -count=1 -run TestManifestOnARealCluster -v ./internal/runtime/kubernetes/`
Expected: PASS. Without one it skips; the PR description then says the real-cluster path is unproven for this slice.

- [ ] **Step 5: DOX closeout**

Re-read the chain root `AGENTS.md` → `KyYard-Server/AGENTS.md` → `internal/runtime/kubernetes/AGENTS.md`, `internal/store/AGENTS.md`, `internal/agent/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md`, and confirm each names what Tasks 1–5 landed and nothing stale remains (`grep -rn 'CheckExecAccess\|removalServices' --include='*.md' . | grep -v docs/superpowers` prints nothing). `KyYard-Server/AGENTS.md` needs no change: no child was added, moved or renamed.

---

## Deferred to after #76

These spec items exist only in PR 22 (`feat/migration-analysis`, open as #76): `master` 50095a8 has no claim writes in `deploy.go`/`remove.go`, no StorageClass list, no migration analyzer, no `application_migrations` and no `kubernetes.claims`. Plan them on a branch off `master` once #76 merges.

- Runtime: a claim stuck `Pending` with no provisioner: add the claim's `Pending` phase or events reason to the `rollout_timeout` detail vocabulary.
- Runtime: `Remove`'s claim `List` cap counts emitted steps; add a test for the cap.
- Runtime tests: claim `Get` non-NotFound error; quota 403 on claim create; claim `List` failure in `Remove`; a forbidden StorageClass list landing in `Truncated`.
- Store: cluster `settleRemoval` retained-claim steps are informational; consider recording the retained claim names on the deployment row for the UI.
- Store: migration abandon then recreate hits `application_name_taken` until the old destination is removed; consider suffixing the destination name.
- Store: migration destination name overflow (over 255) returns a generic `ErrInvalid`; name a code (and its web sentence).
- Store: migration StorageClasses come from a snapshot of any age; add a freshness gate.
- API: the migration inspection budget is spent before cheap state refusals (reanalyze on a non-analyzed row, a second POST while open, invalid choices); check state first.
- API: `sourceMigration` reads under `application.read`, so environment admins get 404 before 403 on choices and analyze; read under `application.migrate` for mutations.
- API tests: a migration row in the runtime gate matrix; the inspection budget on the migration route; the canary check on migration audit rows.

Already done, no work: the pending-endpoint `capability_mismatch` test (added in the PR 20 fix wave; the spec says keep).
