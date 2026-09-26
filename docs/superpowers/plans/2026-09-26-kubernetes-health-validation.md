# Kubernetes Health Validation and Rollback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A succeeded Kubernetes apply is watched for the same bounded window a Docker apply is, gets a verdict from a per-Deployment status read, and an automated update that fails is rolled back to the digests the previous succeeded apply pinned (or the policy pauses with the reason).

**Architecture:** The inspection frames learn a second target shape: `InspectionTarget.Workload` (a Deployment's namespace, name and UID) answered by `ContainerInspection.Workload` (`WorkloadStatus`), advertised as `kubernetes.inspect` by a cluster agent whose `Options.Inspect` is the new `kubernetes.Client.Inspect`. The store places cluster services for inspection, judges a cluster observation with its own rules inside the existing `Judge`, reads the capability per runtime, and `RollbackTarget` returns a cluster deployment to the prior succeeded apply's recorded pull references; `PlanDeployment` accepts those as `PinImages` for a cluster and skips the registry. The loop in `internal/api/validations.go` needs only the target shape and the missing-capability sentence. The web drops `KUBERNETES_UNVERIFIED`, adds the new sentences and makes the policy warning runtime-aware.

**Tech Stack:** Go 1.26 (module `go 1.26.6`), `k8s.io/client-go` v0.37.1 fake clientset, SQLite + PostgreSQL 17, React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-26-kubernetes-health-validation-design.md` (commit 0be8e34)

## Global Constraints

- Work in the worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/k8s-health` (branch `feat/k8s-health-validation`, `master` fc83316 plus the spec commit 0be8e34). Every command below runs from its root. Never `cd` to the main repository.
- Never `git stash`; never `git add -A` or `git add .`: stage the exact paths each commit step names.
- Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Gate each commit on the previous command's exit (`&&`).
- `gofmt -w` every edited Go file and check `gofmt -l cmd internal` prints nothing before each commit. Never put two single quotes or two backticks in a row in a Go comment. `make tidy-check lint` runs after `git add` (it diffs the working tree against the index) and before `git commit`.
- Memory: this machine has been killed by memory pressure. During Tasks 1–6 run only the focused tests each step names (`go test -count=1 -run <Names> ./one/package/`, `npx vitest run --maxWorkers=1 <one file>`), one test process at a time; never `go test ./...`, never `-race` on more than one package, never vitest without a file filter, never `make ci`. The controller runs the full gate (Task 7) alone.
- `web/dist` is embedded and committed: only Task 6 rebuilds it (`make build-web`) and commits it with `web/tsconfig.tsbuildinfo`, with nothing else running.
- The server links no client-go: `go list -deps ./cmd/server | grep -c k8s.io` prints `0`. The cluster adapter (`internal/runtime/kubernetes`) is imported only by `cmd/agent`; `internal/agent/protocol` and `internal/agent/client` (which `cmd/server` links for the built-in local agent) must not import any `k8s.io` package. New wire types are plain Go.
- DOX: the task that changes a directory's contract updates that directory's `AGENTS.md` in the same commit.
- Every store change is tested on SQLite and PostgreSQL. PostgreSQL runs use the local container `kyyard-access-pg`; read the password into a variable, never print it: `PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test -count=1 -p 1 -run <Name> ./internal/store/`. Below this prefix is written `PG=… go test -count=1 -p 1 -run <Name> ./internal/store/`.
- No schema migration: `git diff --quiet fc83316 -- internal/store/migrations` must hold at the end. The baseline's new `pod_uids` key lives in the existing JSON `baseline` column.
- Wire compatibility (spec, Compatibility): server first, then agents. The Docker wire form is unchanged: a Docker target has no `workload` key (`omitzero`) and a Docker answer none (`omitempty`). A cluster agent without `kubernetes.inspect` gets no grant; a Docker hello naming it is refused `capability_mismatch` by `CapabilitiesFit`.
- The ClusterRole, the enrollment manifest, the disclosure and the threat model do not change: `get deployments` and `list pods` are already granted cluster-wide. `TestClusterDisclosureMatchesTheManifestAndThreatModel` must pass untouched.
- Every new closed code or sentence has a web sentence in the task that introduces it: the store's `ValidationDetailNoInspect`, the waiting-reason details and the rollback codes `namespace_changed` and `claims_changed` get their web text in Task 6, and Task 6's vocabulary tests list them.

Plan decisions where the spec's wording left a choice or disagreed with the code (each is reported to the reviewer; none widens scope):

1. **`Workload` is a value, not a pointer.** The spec writes `Workload *WorkloadRef`. `InspectionTarget` is compared with `!=` in five places (`ContainerInspection.Validate`, `handleContainerInspection`, `inspectionBlockers`, `kubernetes_deploy.go`'s `Replaces != (InspectionTarget{})`, the plan tests); a pointer would compare addresses and every JSON round trip would differ. `Workload WorkloadRef` with `json:"workload,omitzero"` keeps the struct comparable and the Docker wire form byte-identical.
2. **Runtime-aware validation dispatches on the target.** `InspectionTarget.ValidateFor(runtime)` holds a target to one shape; `InspectionOpen.Validate(now)` becomes `ValidateFor(now, runtime)` (the agent passes its own runtime). `ContainerInspection.Validate(target, now, health)` keeps its signature and takes the runtime from the target's shape: every caller has already held that target to the endpoint's runtime, so a second argument could only disagree with it.
3. **The answer must fit the frame.** `MaxWorkloadPods` (128) pods of `MaxPodContainers` (32) containers cannot fit `MaxInspectionFrameBytes` (32 KiB), and the server closes the socket on an oversized `inspection.result`. So a cluster answer's containers carry no `image`/`image_id` (validation requires them empty; the judge reads neither), the adapter answers an error (sent `unavailable`) for more than `MaxWorkloadPods` pods rather than a cut list, and the agent client sends `unavailable` for any answer whose JSON exceeds `MaxInspectionFrameBytes`. KyYard renders `replicas: 1`, so a real answer is one or two pods.
4. **The target comes from the settled identity.** The spec builds `WorkloadRef{plan.Namespace, PlannedService.Object.Name, identity.UID}`. `settleKubernetesApply` already refuses an identity whose namespace or name differs from the plan's object, so the loop uses `DeploymentIdentity{Namespace, Name, UID}` directly and reads no plan.
5. **Cluster services are placed `unknown`.** `presence` reads `application_resources`, which a cluster settle never writes, so today every cluster service would be `replaced`. `PendingValidations` places a `Kind: Deployment` identity `PresenceUnknown` ("inspect and wait"); the status read decides the rest.
6. **Which capability.** `PendingValidations` counts `c.capability IN ('container.inspect.health','kubernetes.inspect')`: `CapabilitiesFit` keeps each to its runtime, so the count is the runtime's own capability. It also reads the endpoint's runtime into `PendingValidation.Kubernetes`, which picks the unverifiable detail (`ValidationDetailNoInspect` for a cluster).
7. **The settled generation.** `changed` compares the live `Generation` with the settled identity's, carried on `Observation.Generation` (0 for Docker, unused there).
8. **Waiting reasons are immediate, availability is final.** The spec's `unhealthy` line is ambiguous about whether a failing waiting reason waits for `observe_until`. Docker's `unhealthy` health is immediate and `starting` is final-only; the cluster judge follows that: a container waiting with `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError` or `CreateContainerError` is `unhealthy` at once; `Available < Desired`, `Ready < Desired`, `ObservedGeneration < Generation` or any other waiting container is `unhealthy` only at or after `observe_until`.
9. **Detail sentences.** The spec asks for "fixed constants naming the first failing service and the reason word". A detail is one string: a cluster failing verdict's detail is the service name, or `<service>:<reason>` when a waiting reason from the list in decision 8 (`failingWaits`) decided it. The web renders the reason through a closed table and drops any other suffix. `ValidationDetailNoInspect` is the one new loop sentence.
10. **The prior deployment and the service-set check.** The prior deployment is the instance's latest `succeeded` `apply` other than the validated one, by `settled_at`. The Docker rule also refuses when a later deployment replaced what the failed one created; the cluster equivalent is that the validated deployment must still be the instance's latest succeeded apply, else `service_set_changed`. With that, the prior row's `revision` equals `previous_revision`, and `Rollback.Revision` is read from it.
11. **"with detail namespace/claims".** A rollback detail is a comma-joined code list (`reasonText` renders each code). The namespace and claims refusals are `service_set_changed,namespace_changed` and `service_set_changed,claims_changed`; the two new codes get web sentences.
12. **Pins on a cluster plan.** The stored plan field is `PlannedService.PullReference`/`PullDigest` (the spec says `Pull.Reference`/`Pull.Digest`). A cluster `PinImages` maps every service to a `host/repository@sha256:<digest>` reference; a pin that is not a digest reference, or pins that do not cover every service, is `ErrInvalid` (so a nil resolver is never called). The pinned reference replaces the service's tag in the existing update loop, whose digest branch already pins without a registry call; the anonymous-pull/registry gate still applies, as `kubernetesFrame` re-checks it anyway.
13. **The real-cluster read is developer-run.** `TestManifestOnARealCluster` needs `KY_TEST_KUBECONFIG` and `KY_TEST_DEPLOY_IMAGE`; CI has no cluster. Task 2 extends it; its pass is reported as unproven unless a kind cluster was available.

## Review Focus

1. A cluster whose pods were all recreated by someone else during the window (baseline pod gone, a new UID, restarts back to 0) must be `changed`, not `restarting` and not `healthy`. Pinned in Task 3 (`TestJudgeCluster`, the "recreated by someone else" row).
2. A cluster agent answering an inspection too large for the 32 KiB frame must not get its socket closed: the answer is sent `unavailable`, and a poll of it counts as unobserved. Pinned in Task 1 (`TestClusterInspectionTargetsAndFrameBound`).
3. An automated cluster rollback must never call the registry and must plan with the prior digests even when the tag now resolves elsewhere: the rollback plan's `PullDigest` is the prior one although the fake registry answers another digest. Pinned in Task 5 (`TestClusterValidationRollsBackToThePriorDigests`) and Task 4 (`TestPlanDeploymentPinsAClusterDigest`: a counting resolver that answers another digest is never called, and a nil resolver plans the same).
4. A manual apply made after the failed automated one must not be reverted by the rollback: the rollback is ineligible `service_set_changed`. Pinned in Task 4 (`TestClusterRollbackTarget`, the "a later apply" row).
5. A Deployment deleted during the window (`Missing`) and one deleted and recreated under the same name (new UID) must both be `changed`, and a status read of a recreated Deployment must never become the baseline of the old one. Pinned in Task 3 (`TestJudgeCluster` rows "missing" and "recreated under the name") and Task 3 (`TestBaselineOfACluster`).

## File map

| Path | Task | Change |
|---|---|---|
| `internal/agent/protocol/{workload.go,workload_test.go,inspection.go,kubernetes.go}`, `internal/agent/client/{inspection.go,connect.go,capabilities_test.go,kubernetes_inspection_test.go}`, `internal/api/{inspection_test.go,deployment_plan_test.go,validations_test.go,runtime_rules_test.go}`, `internal/agent/AGENTS.md`, `internal/api/AGENTS.md` | 1 | Wire types, runtime-aware validation, `kubernetes.inspect`, the frame bound |
| `internal/runtime/kubernetes/{inspect.go,inspect_test.go,cluster_test.go,AGENTS.md}`, `cmd/agent/main.go`, `internal/agent/AGENTS.md` | 2 | `Client.Inspect`, wiring, the real-cluster read |
| `internal/store/{validations.go,validations_cluster_test.go,validations_test.go}`, `internal/api/{validations.go,validations_test.go,kubernetes_deploy_test.go}`, `internal/store/AGENTS.md`, `internal/api/AGENTS.md` | 3 | Cluster observation, `Judge`, baseline, capability per runtime, the target in the loop |
| `internal/store/{rollback.go,validations.go,application_deployment.go,kubernetes_rollback_test.go}`, `internal/store/AGENTS.md` | 4 | Cluster `RollbackTarget`, `PinImages` on a cluster plan |
| `internal/api/kubernetes_validation_test.go`, `internal/api/AGENTS.md` | 5 | The loop over a fake cluster agent, end to end |
| `web/src/components/{ApplicationValidation.tsx,ApplicationValidation.test.tsx,ApplicationPolicy.tsx,ApplicationPolicy.test.tsx,ApplicationDeploymentPlan.tsx,ApplicationDeploymentPlan.test.tsx}`, `web/dist`, `web/tsconfig.tsbuildinfo`, `web/AGENTS.md` | 6 | Sentences, runtime-aware warning, rebuild |
| `docs/{application-schema,agent-protocol,authorization-matrix}.md`, `README.md`, `AGENTS.md`, `KyYard-Implementation-Plan.md` | 7 | Documents; the gate |

---

### Task 1: Protocol — the Workload target and answer, `kubernetes.inspect`, the agent's grant check and frame bound

**Files:**
- Create: `internal/agent/protocol/workload.go`
- Modify: `internal/agent/protocol/inspection.go` (`InspectionTarget`, `ValidateFor`, `InspectionOpen.ValidateFor`, `ContainerInspection.Workload`, `Validate`)
- Modify: `internal/agent/protocol/kubernetes.go` (`kubernetesCapabilities`)
- Modify: `internal/agent/client/inspection.go` (`runtime`, grant check, frame bound), `internal/agent/client/connect.go` (`helloCapabilities`)
- Test: `internal/agent/protocol/workload_test.go`, `internal/agent/client/kubernetes_inspection_test.go`, `internal/agent/client/capabilities_test.go`
- Test (call sites of the renamed grant check): `internal/api/inspection_test.go:39`, `internal/api/deployment_plan_test.go:247`, `internal/api/validations_test.go:462`; rows in `internal/api/runtime_rules_test.go` (`TestHelloCapabilitiesMustFitTheRuntime`)
- Docs: `internal/agent/AGENTS.md`, `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.ValidDNSLabel`, `ValidDNSSubdomain`, `deploymentUUID`, `PodContainer`, `MaxPodContainers`, `MaxRestartCount`, `MaxInspectionFrameBytes`, `CapabilitiesFit` (existing).
- Produces:
  - `const protocol.CapabilityKubernetesInspect = "kubernetes.inspect"`, `protocol.MaxWorkloadConditions = 8`, `protocol.MaxWorkloadPods = MaxPodContainers * 4` (128).
  - `type protocol.WorkloadRef struct{ Namespace, Name, UID string }` (JSON `namespace`, `name`, `uid`).
  - `type protocol.WorkloadStatus struct{ UID string; Generation, ObservedGeneration int64; Desired, Updated, Ready, Available int32; Conditions []WorkloadCondition; Pods []PodStatus; Missing bool }`, `type protocol.WorkloadCondition struct{ Type, Status, Reason string }`, `type protocol.PodStatus struct{ Name, UID, Phase string; Containers []PodContainer }`.
  - `InspectionTarget.Workload WorkloadRef` (`json:"workload,omitzero"`); `func (InspectionTarget) ValidateFor(runtime string) error`; `InspectionTarget.Validate()` now also refuses a set `Workload`.
  - `func (InspectionOpen) ValidateFor(now time.Time, runtime string) error` replaces `Validate(now)`.
  - `ContainerInspection.Workload *WorkloadStatus` (`json:"workload,omitempty"`); `ContainerInspection.Validate(target, now, health)` accepts exactly a `WorkloadStatus` answer with every Docker field empty for a Workload target.
  - The agent client sends `unavailable` for an answer whose JSON exceeds `MaxInspectionFrameBytes`, and a cluster agent (`Options.Kubernetes`) with `Options.Inspect` advertises `kubernetes.inspect`.

- [ ] **Step 1: Write the protocol tests**

Create `internal/agent/protocol/workload_test.go`:

```go
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	testWorkloadUID = "0f1e2d3c-4b5a-4968-8776-655443322110"
	testPodUID      = "11111111-2222-4333-8444-555555555555"
)

func workloadTarget() InspectionTarget {
	return InspectionTarget{Workload: WorkloadRef{Namespace: "shop", Name: "shop-web", UID: testWorkloadUID}}
}

func workloadAnswer() ContainerInspection {
	return ContainerInspection{Target: workloadTarget(), ObservedAt: time.Now(), Workload: &WorkloadStatus{UID: testWorkloadUID, Generation: 2, ObservedGeneration: 2, Desired: 1, Updated: 1, Ready: 1, Available: 1,
		Conditions: []WorkloadCondition{{Type: "Available", Status: "True", Reason: "MinimumReplicasAvailable"}},
		Pods:       []PodStatus{{Name: "shop-web-7d9f8b6c5-x2x4z", UID: testPodUID, Phase: "Running", Containers: []PodContainer{{Name: "web", State: "running", Ready: true, RestartCount: 1}}}}}}
}

// A target has exactly one shape per runtime, and a grant is checked against the agent's.
func TestInspectionTargetShapePerRuntime(t *testing.T) {
	docker := InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	both := docker
	both.Workload = workloadTarget().Workload
	for _, tc := range []struct {
		name    string
		target  InspectionTarget
		runtime string
		ok      bool
	}{
		{"docker target on docker", docker, RuntimeDocker, true},
		{"workload target on kubernetes", workloadTarget(), RuntimeKubernetes, true},
		{"docker target on kubernetes", docker, RuntimeKubernetes, false},
		{"workload target on docker", workloadTarget(), RuntimeDocker, false},
		{"both on kubernetes", both, RuntimeKubernetes, false},
		{"both on docker", both, RuntimeDocker, false},
		{"namespace not a label", InspectionTarget{Workload: WorkloadRef{Namespace: "Shop", Name: "shop-web", UID: testWorkloadUID}}, RuntimeKubernetes, false},
		{"name not a label", InspectionTarget{Workload: WorkloadRef{Namespace: "shop", Name: "shop.web", UID: testWorkloadUID}}, RuntimeKubernetes, false},
		{"uid not a uuid", InspectionTarget{Workload: WorkloadRef{Namespace: "shop", Name: "shop-web", UID: "secret-canary"}}, RuntimeKubernetes, false},
		{"empty", InspectionTarget{}, RuntimeKubernetes, false},
	} {
		if err := tc.target.ValidateFor(tc.runtime); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
		grant := InspectionOpen{Request: "r", Endpoint: "e", Actor: "a", Connection: make([]byte, 32), Expires: time.Now().Add(10 * time.Second), Target: tc.target}
		if err := grant.ValidateFor(time.Now(), tc.runtime); (err == nil) != tc.ok {
			t.Errorf("%s grant: %v", tc.name, err)
		}
	}
	// A Docker target's wire form is unchanged: no workload key.
	raw, _ := json.Marshal(docker)
	if strings.Contains(string(raw), "workload") {
		t.Fatalf("docker target grew a workload key: %s", raw)
	}
	raw, _ = json.Marshal(ContainerInspection{Target: docker})
	if strings.Contains(string(raw), "workload") {
		t.Fatalf("docker answer grew a workload key: %s", raw)
	}
}

// A cluster answer carries a bounded WorkloadStatus and no Docker field; a Docker answer never
// carries a WorkloadStatus.
func TestWorkloadAnswerValidation(t *testing.T) {
	target := workloadTarget()
	if err := workloadAnswer().Validate(target, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	missing := ContainerInspection{Target: target, ObservedAt: time.Now(), Workload: &WorkloadStatus{Missing: true}}
	if err := missing.Validate(target, time.Now(), false); err != nil {
		t.Fatalf("missing: %v", err)
	}
	recreated := workloadAnswer()
	recreated.Workload.UID = testPodUID
	if err := recreated.Validate(target, time.Now(), false); err != nil {
		t.Fatalf("a live UID other than the target's is a valid answer: %v", err)
	}
	pods := func(n, containers int) []PodStatus {
		out := make([]PodStatus, n)
		for i := range out {
			out[i] = PodStatus{Name: "p", UID: testPodUID, Phase: "Running", Containers: make([]PodContainer, containers)}
			for j := range out[i].Containers {
				out[i].Containers[j] = PodContainer{Name: "c", State: "running"}
			}
		}
		return out
	}
	full := workloadAnswer()
	full.Workload.Pods = pods(MaxWorkloadPods, 1)
	full.Workload.Conditions = make([]WorkloadCondition, MaxWorkloadConditions)
	for i := range full.Workload.Conditions {
		full.Workload.Conditions[i] = WorkloadCondition{Type: "Progressing", Status: "Unknown"}
	}
	if err := full.Validate(target, time.Now(), false); err != nil {
		t.Fatalf("at the caps: %v", err)
	}
	for name, mutate := range map[string]func(*ContainerInspection){
		"no status":          func(r *ContainerInspection) { r.Workload = nil },
		"docker state":       func(r *ContainerInspection) { r.State = "running" },
		"docker health":      func(r *ContainerInspection) { r.Health = "healthy" },
		"verified":           func(r *ContainerInspection) { r.ConfigurationVerified = true },
		"other target":       func(r *ContainerInspection) { r.Target.Workload.Name = "shop-api" },
		"missing with a uid": func(r *ContainerInspection) { r.Workload.Missing = true },
		"uid not a uuid":     func(r *ContainerInspection) { r.Workload.UID = "secret-canary" },
		"generation zero":    func(r *ContainerInspection) { r.Workload.Generation = 0 },
		"negative count":     func(r *ContainerInspection) { r.Workload.Available = -1 },
		"condition status":   func(r *ContainerInspection) { r.Workload.Conditions[0].Status = "Maybe" },
		"condition reason":   func(r *ContainerInspection) { r.Workload.Conditions[0].Reason = "secret canary" },
		"too many conditions": func(r *ContainerInspection) {
			r.Workload.Conditions = make([]WorkloadCondition, MaxWorkloadConditions+1)
		},
		"too many pods":           func(r *ContainerInspection) { r.Workload.Pods = pods(MaxWorkloadPods+1, 1) },
		"too many containers":     func(r *ContainerInspection) { r.Workload.Pods = pods(1, MaxPodContainers+1) },
		"pod phase":               func(r *ContainerInspection) { r.Workload.Pods[0].Phase = "Exploded" },
		"pod name":                func(r *ContainerInspection) { r.Workload.Pods[0].Name = "Shop_Web" },
		"container image":         func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].Image = "nginx:1" },
		"container image id":      func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].ImageID = "sha256:x" },
		"container state":         func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].State = "paused" },
		"container reason":        func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].Reason = "back-off 5m0s" },
		"negative restarts":       func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].RestartCount = -1 },
		"restarts over the bound": func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].RestartCount = MaxRestartCount + 1 },
	} {
		bad := workloadAnswer()
		mutate(&bad)
		if bad.Validate(target, time.Now(), false) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	docker := InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	answer := ContainerInspection{Target: docker, ObservedAt: time.Now(), State: "running", RestartPolicy: "no", NetworkMode: "none", ImagePlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}, ConfigurationVerified: true, Workload: workloadAnswer().Workload}
	if answer.Validate(docker, time.Now(), false) == nil {
		t.Fatal("a Docker answer carried a workload status")
	}
}

// kubernetes.inspect is a cluster capability; a Docker hello naming it is refused.
func TestCapabilitiesFitKubernetesInspect(t *testing.T) {
	if CapabilityKubernetesInspect != "kubernetes.inspect" {
		t.Fatal("the wire vocabulary changed")
	}
	if !CapabilitiesFit(RuntimeKubernetes, []string{CapabilityKubernetesInventory, CapabilityKubernetesInspect}) {
		t.Fatal("refused on a cluster")
	}
	if CapabilitiesFit(RuntimeDocker, []string{CapabilityContainerInspect, CapabilityKubernetesInspect}) {
		t.Fatal("fits a Docker host")
	}
	if CapabilitiesFit(RuntimeKubernetes, []string{CapabilityKubernetesInspect, CapabilityContainerInspectHealth}) {
		t.Fatal("a cluster named the Docker health capability")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestInspectionTargetShapePerRuntime|TestWorkloadAnswerValidation|TestCapabilitiesFitKubernetesInspect' ./internal/agent/protocol/`
Expected: FAIL to build: `undefined: WorkloadRef`, `undefined: CapabilityKubernetesInspect`.

- [ ] **Step 3: Add the wire types**

Create `internal/agent/protocol/workload.go`:

```go
package protocol

import (
	"errors"
	"regexp"
)

// Kubernetes inspection: the inspection frames carry a Deployment target and its status. See
// agent-protocol.md, Inspection frames.
const (
	// CapabilityKubernetesInspect marks a cluster agent that answers a WorkloadRef inspection;
	// health validation of a cluster needs it.
	CapabilityKubernetesInspect = "kubernetes.inspect"
	// MaxWorkloadConditions bounds a status's conditions; MaxWorkloadPods its pods.
	MaxWorkloadConditions = 8
	MaxWorkloadPods       = MaxPodContainers * 4
)

// WorkloadRef names the Deployment a cluster inspection reads.
type WorkloadRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// WorkloadStatus is a Deployment's rollout state and its pods. Missing: the Deployment is gone,
// and nothing else is set. A UID other than the target's is a Deployment recreated under the name.
type WorkloadStatus struct {
	UID                string              `json:"uid"`
	Generation         int64               `json:"generation"`
	ObservedGeneration int64               `json:"observed_generation"`
	Desired            int32               `json:"desired"`
	Updated            int32               `json:"updated"`
	Ready              int32               `json:"ready"`
	Available          int32               `json:"available"`
	Conditions         []WorkloadCondition `json:"conditions"`
	Pods               []PodStatus         `json:"pods"`
	Missing            bool                `json:"missing"`
}

type WorkloadCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// PodStatus is one pod of the Deployment. Its containers carry no image: the judge reads state,
// reason, readiness and restarts only, and the answer must fit MaxInspectionFrameBytes.
type PodStatus struct {
	Name       string         `json:"name"`
	UID        string         `json:"uid"`
	Phase      string         `json:"phase"`
	Containers []PodContainer `json:"containers"`
}

// reasonWord is a Kubernetes reason: one CamelCase word, as a rollout_timeout detail carries it.
var reasonWord = regexp.MustCompile(`^[A-Za-z]{1,64}$`)

var (
	conditionStatus = map[string]bool{"True": true, "False": true, "Unknown": true}
	podPhase        = map[string]bool{"Pending": true, "Running": true, "Succeeded": true, "Failed": true, "Unknown": true}
	containerState  = map[string]bool{"running": true, "waiting": true, "terminated": true}
)

func (w WorkloadRef) valid() bool {
	return ValidDNSLabel(w.Namespace) && ValidDNSLabel(w.Name) && deploymentUUID.MatchString(w.UID)
}

// validate bounds an untrusted status: counts non-negative, closed vocabularies, reason words,
// Kubernetes names and the caps.
func (s WorkloadStatus) validate() error {
	invalid := errors.New("invalid workload status")
	if s.Missing {
		if s.UID != "" || s.Generation != 0 || s.ObservedGeneration != 0 || s.Desired != 0 || s.Updated != 0 || s.Ready != 0 || s.Available != 0 || len(s.Conditions) > 0 || len(s.Pods) > 0 {
			return invalid
		}
		return nil
	}
	if !deploymentUUID.MatchString(s.UID) || s.Generation < 1 || s.ObservedGeneration < 0 || s.Desired < 0 || s.Updated < 0 || s.Ready < 0 || s.Available < 0 || len(s.Conditions) > MaxWorkloadConditions || len(s.Pods) > MaxWorkloadPods {
		return invalid
	}
	for _, c := range s.Conditions {
		if !reasonWord.MatchString(c.Type) || !conditionStatus[c.Status] || (c.Reason != "" && !reasonWord.MatchString(c.Reason)) {
			return invalid
		}
	}
	for _, p := range s.Pods {
		if !ValidDNSSubdomain(p.Name) || !deploymentUUID.MatchString(p.UID) || !podPhase[p.Phase] || len(p.Containers) > MaxPodContainers {
			return invalid
		}
		for _, c := range p.Containers {
			if !ValidDNSLabel(c.Name) || c.Image != "" || c.ImageID != "" || !containerState[c.State] || (c.Reason != "" && !reasonWord.MatchString(c.Reason)) || c.RestartCount < 0 || c.RestartCount > MaxRestartCount {
				return invalid
			}
		}
	}
	return nil
}

// dockerEmpty is an answer carrying none of the Docker fields: what a cluster answer must be.
func (r ContainerInspection) dockerEmpty() bool {
	return r.State == "" && r.Health == "" && r.RestartCount == 0 && r.ImagePlatform == (ImagePlatform{}) && r.RestartPolicy == "" && r.RestartRetries == 0 && len(r.Ports) == 0 && r.Mounts == (MountCounts{}) && r.NetworkMode == "" && r.NetworkCount == 0 && !r.Privileged && !r.ReadOnlyRootFS && !r.AutoRemove && len(r.Unsupported) == 0 && !r.ConfigurationVerified
}
```

- [ ] **Step 4: Make the target and the answer runtime-aware**

In `internal/agent/protocol/inspection.go`, replace the `InspectionTarget` type and its `Validate` with:

```go
// InspectionTarget pins the identity already authorized by the caller. Docker
// inventory exposes creation time in whole seconds. See application-schema.md,
// Runtime inspection foundation. This is not a wire grant or deployment approval.
// A Kubernetes target names a Deployment (Workload) and no container.
type InspectionTarget struct {
	ContainerID string      `json:"container_id"`
	ImageID     string      `json:"image_id"`
	CreatedUnix int64       `json:"created_unix"`
	Workload    WorkloadRef `json:"workload,omitzero"`
}

// Validate holds a target to the Docker shape.
func (t InspectionTarget) Validate() error {
	if !fullDockerID.MatchString(t.ContainerID) || !strings.HasPrefix(t.ImageID, "sha256:") || !fullDockerID.MatchString(strings.TrimPrefix(t.ImageID, "sha256:")) || t.CreatedUnix <= 0 || t.Workload != (WorkloadRef{}) {
		return errors.New("inspection requires full container/image IDs and creation time")
	}
	return nil
}

// ValidateFor holds a target to the one shape the runtime reads: a container for Docker, a
// Deployment for Kubernetes.
func (t InspectionTarget) ValidateFor(runtime string) error {
	if runtime != RuntimeKubernetes {
		return t.Validate()
	}
	if t.ContainerID != "" || t.ImageID != "" || t.CreatedUnix != 0 || !t.Workload.valid() {
		return errors.New("a Kubernetes inspection names a Deployment's namespace, name and UID and no container")
	}
	return nil
}
```

Add the last field of `ContainerInspection`, directly below `ConfigurationVerified`:

```go
	// Workload is a cluster agent's answer, set exactly for a Kubernetes target; every Docker
	// field is then empty.
	Workload *WorkloadStatus `json:"workload,omitempty"`
```

Replace `func (r InspectionOpen) Validate(now time.Time) error { ... }` with:

```go
// ValidateFor checks a grant for an agent of the runtime.
func (r InspectionOpen) ValidateFor(now time.Time, runtime string) error {
	if !execStreamID.MatchString(r.Request) || !execStreamID.MatchString(r.Actor) || !execStreamID.MatchString(r.Endpoint) || len(r.Connection) != 32 || !r.Expires.After(now) || r.Expires.After(now.Add(InspectionLifetime)) {
		return errors.New("invalid inspection grant")
	}
	return r.Target.ValidateFor(runtime)
}
```

Replace the doc comment and the first `if` of `ContainerInspection.Validate` (everything above `switch r.State {`) with:

```go
// Validate bounds an untrusted agent result before it reaches an HTTP response. The target's
// shape decides the runtime: a Workload target takes a WorkloadStatus answer and nothing else. For
// a container, health says the answering agent advertised CapabilityContainerInspectHealth:
// health and restart_count are then required and bounded, and otherwise absent.
func (r ContainerInspection) Validate(target InspectionTarget, now time.Time, health bool) error {
	invalid := errors.New("invalid inspection result")
	if r.Target != target || r.ObservedAt.Before(now.Add(-InspectionLifetime)) || r.ObservedAt.After(now.Add(5*time.Second)) {
		return invalid
	}
	if target.Workload != (WorkloadRef{}) {
		if target.ValidateFor(RuntimeKubernetes) != nil || !r.dockerEmpty() || r.Workload == nil || r.Workload.validate() != nil {
			return invalid
		}
		return nil
	}
	if r.Workload != nil || target.Validate() != nil || r.ConfigurationVerified != (len(r.Unsupported) == 0) || !knownCodes(r.Unsupported) {
		return invalid
	}
```

In `internal/agent/protocol/kubernetes.go`, add `CapabilityKubernetesInspect: true` to `kubernetesCapabilities`:

```go
var kubernetesCapabilities = map[string]bool{CapabilityKubernetesInventory: true, CapabilityPodLogs: true, CapabilityKubernetesDeploy: true, CapabilityKubernetesClaims: true, CapabilityKubernetesRemove: true, CapabilityKubernetesInspect: true}
```

- [ ] **Step 5: Run the protocol package**

Run: `gofmt -w internal/agent/protocol && go test -count=1 ./internal/agent/protocol/`
Expected: PASS (the whole package: `TestInspectionResultValidation`, `TestInspectionHealthFields` and the deployment tests that compare `Replaces != (InspectionTarget{})` still pass).

- [ ] **Step 6: Write the agent client tests**

Create `internal/agent/client/kubernetes_inspection_test.go`:

```go
package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const testWorkloadUID = "0f1e2d3c-4b5a-4968-8776-655443322110"

func workloadRequest() protocol.InspectionOpen {
	req := inspectionRequest()
	req.Target = protocol.InspectionTarget{Workload: protocol.WorkloadRef{Namespace: "shop", Name: "shop-web", UID: testWorkloadUID}}
	return req
}

// A cluster agent takes a Deployment target and refuses a container one; a Docker agent the
// reverse. An answer too large for the frame is sent unavailable, never oversized.
func TestClusterInspectionTargetsAndFrameBound(t *testing.T) {
	var pods []protocol.PodStatus
	answer := func(ctx context.Context, target protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
		return &protocol.ContainerInspection{Target: target, ObservedAt: time.Now(), Workload: &protocol.WorkloadStatus{UID: testWorkloadUID, Generation: 1, ObservedGeneration: 1, Desired: 1, Updated: 1, Ready: 1, Available: 1, Conditions: []protocol.WorkloadCondition{}, Pods: pods}}, nil
	}
	out := make(chan outFrame, 8)
	cluster := &Options{Kubernetes: true, inspectionSlots: make(chan struct{}, 2), Inspect: answer}
	req := workloadRequest()
	s := newInspections(context.Background(), req.Endpoint, req.Connection, cluster, out)
	if err := s.handle(execFrame(protocol.TypeInspectionOpen, req), true); err != nil {
		t.Fatal(err)
	}
	if reply := nextExecFrame(t, out, protocol.TypeInspectionResult).Payload.(protocol.InspectionResult); reply.Status != "ok" || reply.Result.Workload == nil {
		t.Fatalf("workload answer: %+v", reply)
	}
	docker := inspectionRequest()
	docker.Request = "docker"
	if err := s.handle(execFrame(protocol.TypeInspectionOpen, docker), true); err == nil {
		t.Fatal("a cluster agent took a container target")
	}
	host := newInspections(context.Background(), req.Endpoint, req.Connection, &Options{inspectionSlots: make(chan struct{}, 2), Inspect: answer}, out)
	if err := host.handle(execFrame(protocol.TypeInspectionOpen, workloadRequest()), true); err == nil {
		t.Fatal("a Docker agent took a Deployment target")
	}
	// MaxWorkloadPods pods with long names pass validation but do not fit the frame.
	for range protocol.MaxWorkloadPods {
		pods = append(pods, protocol.PodStatus{Name: strings.Repeat("p", 250), UID: testWorkloadUID, Phase: "Running", Containers: []protocol.PodContainer{{Name: "web", State: "running"}}})
	}
	big := workloadRequest()
	big.Request = "big"
	if in, _ := answer(context.Background(), big.Target); in.Validate(big.Target, time.Now(), true) != nil {
		t.Fatal("the oversized answer must be valid, or the bound is not what refuses it")
	}
	if err := s.handle(execFrame(protocol.TypeInspectionOpen, big), true); err != nil {
		t.Fatal(err)
	}
	if reply := nextExecFrame(t, out, protocol.TypeInspectionResult).Payload.(protocol.InspectionResult); reply.Status != "unavailable" || reply.Result != nil {
		t.Fatalf("oversized answer: %s", reply.Status)
	}
}
```

In `internal/agent/client/capabilities_test.go`, `TestHelloCapabilitiesPerRuntime`, directly below the `readOnly` check insert:

```go
	inspect := func(context.Context, protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
		return nil, nil
	}
	if inspecting := helloCapabilities(&Options{Kubernetes: true, Inspect: inspect}); !slices.Equal(inspecting, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesInspect}) || !protocol.CapabilitiesFit(protocol.RuntimeKubernetes, inspecting) {
		t.Fatalf("inspecting cluster %v", inspecting)
	}
```

- [ ] **Step 7: Run them to verify they fail**

Run: `go test -count=1 -run 'TestClusterInspectionTargetsAndFrameBound|TestHelloCapabilitiesPerRuntime' ./internal/agent/client/`
Expected: FAIL to build: `req.Validate undefined` in `inspection.go` (the grant check was renamed in Step 4).

- [ ] **Step 8: Check grants against the agent's runtime, bound the answer, advertise the capability**

In `internal/agent/client/inspection.go`, add below `newInspections`:

```go

// runtime is the target shape this agent answers: a cluster agent reads Deployments.
func (s *inspections) runtime() string {
	if s.opts.Kubernetes {
		return protocol.RuntimeKubernetes
	}
	return protocol.RuntimeDocker
}
```

In `handle`, replace `req.Validate(time.Now()) != nil` with `req.ValidateFor(time.Now(), s.runtime()) != nil`. In `run`, directly below the `if err == nil && result != nil && result.Validate(...) == nil { ... }` block insert:

```go
	// The server closes the socket on an oversized answer: one that does not fit is unavailable.
	if raw, err := json.Marshal(reply); err != nil || len(raw) > protocol.MaxInspectionFrameBytes {
		reply = protocol.InspectionResult{Request: req.Request, Status: "unavailable"}
	}
```

In `internal/agent/client/connect.go`, `helloCapabilities`, inside the `if opts.Kubernetes {` branch directly below the `opts.Remove` append insert:

```go
		if opts.Inspect != nil {
			capabilities = append(capabilities, protocol.CapabilityKubernetesInspect)
		}
```

and extend its doc comment's last sentence to: `A cluster agent that deploys applies claims too (kubernetes.claims), and one that inspects reads Deployments (kubernetes.inspect).`

- [ ] **Step 9: Run the client tests**

Run: `gofmt -w internal/agent/client && go test -count=1 -run 'TestClusterInspectionTargetsAndFrameBound|TestHelloCapabilitiesPerRuntime|TestInspection' ./internal/agent/client/`
Expected: PASS.

- [ ] **Step 10: Move the API tests to the renamed grant check and pin the hello refusal**

Replace the three calls of the old grant check with the Docker runtime:

- `internal/api/inspection_test.go:39`: `req.Validate(time.Now()) != nil` → `req.ValidateFor(time.Now(), protocol.RuntimeDocker) != nil`
- `internal/api/deployment_plan_test.go:247`: `grant.Validate(time.Now()) != nil` → `grant.ValidateFor(time.Now(), protocol.RuntimeDocker) != nil`
- `internal/api/validations_test.go:462`: `g.Validate(time.Now()) != nil` → `g.ValidateFor(time.Now(), protocol.RuntimeDocker) != nil`

In `internal/api/runtime_rules_test.go`, `TestHelloCapabilitiesMustFitTheRuntime`, directly below the row `{"claims from a host", ...}` add:

```go
		{"workload inspection from a host", f.host, []string{protocol.CapabilityContainerInspect, protocol.CapabilityKubernetesInspect}, false},
		{"workload inspection from a cluster", f.cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesInspect}, true},
```

Run: `go vet ./internal/api/ && go test -count=1 -run 'TestHelloCapabilitiesMustFitTheRuntime|TestValidationInspectsOverTheAgentSocketAsTheSystem|TestInspectionAPI|TestPlanInspectsOverTheAgentSocket' ./internal/api/`
Expected: PASS (the host row is refused `capability_mismatch`, the cluster row fits).

- [ ] **Step 11: DOX**

In `internal/agent/AGENTS.md`:
- In the `Runtimes are ...` bullet, replace `` `kubernetes.claims` (with `kubernetes.deploy`: it applies claims and service volumes; the server refuses a plan with claims without it) and `kubernetes.remove` `` with `` `kubernetes.claims` (with `kubernetes.deploy`: it applies claims and service volumes; the server refuses a plan with claims without it), `kubernetes.remove` and `kubernetes.inspect` ``.
- At the end of the `Inspection uses inspection.open/result/cancel ...` bullet append: `A target has one shape per runtime (`InspectionTarget.ValidateFor`; the agent checks grants with `InspectionOpen.ValidateFor(now, runtime)` for its own runtime): a Docker container, or for a cluster `Workload` (`WorkloadRef`: namespace and Deployment name as DNS-1123 labels, the Deployment's UID), answered by `ContainerInspection.Workload` (`WorkloadStatus`: UID, generation and observed generation, desired/updated/ready/available counts, at most `MaxWorkloadConditions` (8) conditions of reason words, at most `MaxWorkloadPods` (128) pods each of at most 32 containers carrying no image or image ID, `Missing` when the Deployment is gone) with every Docker field empty; a Docker answer never carries it and neither Docker form changes on the wire (`omitzero`, `omitempty`). The client sends `unavailable` for any answer whose JSON exceeds `MaxInspectionFrameBytes`, since the server closes the socket on an oversized result.`
- In the `Options.Kubernetes marks a cluster agent` bullet, replace `` and `kubernetes.remove` with `Options.Remove`, `` with `` `kubernetes.remove` with `Options.Remove` and `kubernetes.inspect` with `Options.Inspect`, ``.

In `internal/api/AGENTS.md`, in the `The agent socket knows the endpoint's runtime` bullet, replace `` `kubernetes.claims` and `kubernetes.remove` (so `` with `` `kubernetes.claims`, `kubernetes.remove` and `kubernetes.inspect` (so ``.

- [ ] **Step 12: Commit**

```bash
gofmt -l cmd internal && git add internal/agent/protocol/workload.go internal/agent/protocol/workload_test.go internal/agent/protocol/inspection.go internal/agent/protocol/kubernetes.go internal/agent/client/inspection.go internal/agent/client/connect.go internal/agent/client/capabilities_test.go internal/agent/client/kubernetes_inspection_test.go internal/api/inspection_test.go internal/api/deployment_plan_test.go internal/api/validations_test.go internal/api/runtime_rules_test.go internal/agent/AGENTS.md internal/api/AGENTS.md && make tidy-check lint && git commit -m "feat(protocol): inspect a Kubernetes Deployment over the inspection frames

InspectionTarget gains a Workload shape and ContainerInspection a
WorkloadStatus answer, validated per runtime; kubernetes.inspect is a
cluster capability. The agent checks grants against its own runtime
and sends unavailable for an answer too large for the frame.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```


### Task 2: Runtime — `Client.Inspect` on the cluster, wired into the agent

**Files:**
- Create: `internal/runtime/kubernetes/inspect.go`
- Modify: `cmd/agent/main.go:98` (the `--kubernetes` branch)
- Test: `internal/runtime/kubernetes/inspect_test.go`, `internal/runtime/kubernetes/cluster_test.go` (`TestManifestOnARealCluster`)
- Docs: `internal/runtime/kubernetes/AGENTS.md`, `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes (Task 1): `protocol.WorkloadRef`, `WorkloadStatus`, `WorkloadCondition`, `PodStatus`, `MaxWorkloadConditions`, `MaxWorkloadPods`, `InspectionTarget.ValidateFor`, `ContainerInspection.Validate`. Existing in the package: `callBudget`, `replicas`, `pod`, `reasonWord` (deploy.go), `render.Selector`, `render.LabelInstance`, `render.LabelService`; test helpers `cluster`, `owned`, constants `testUID`, `testDigest`.
- Produces: `func (c *kubernetes.Client) Inspect(ctx context.Context, target protocol.InspectionTarget) (*protocol.ContainerInspection, error)`, the signature of `client.Options.Inspect`; `cmd/agent --kubernetes` sets `Options.Inspect` to it, so the agent advertises `kubernetes.inspect`.

- [ ] **Step 1: Write the adapter tests**

Create `internal/runtime/kubernetes/inspect_test.go`:

```go
package kubernetes

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

const testPodUID = "22222222-3333-4444-8555-666666666666"

// inspectTarget is shop-web, the web service's Deployment, as the settle recorded it.
func inspectTarget() protocol.InspectionTarget {
	return protocol.InspectionTarget{Workload: protocol.WorkloadRef{Namespace: "shop", Name: "shop-web", UID: testUID}}
}

// rolledOut is shop-web at generation 2, its one replica updated, ready and available.
func rolledOut() *appsv1.Deployment {
	m := owned("web")
	m.Name, m.UID, m.Generation = "shop-web", types.UID(testUID), 2
	return &appsv1.Deployment{ObjectMeta: m, Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable", Message: "secret-canary"}}}}
}

// webPod is a pod of the web service whose one container is in state.
func webPod(name, uid string, state corev1.ContainerState, restarts int32) *corev1.Pod {
	m := owned("web")
	m.Name, m.UID = name, types.UID(uid)
	return &corev1.Pod{ObjectMeta: m, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "ghcr.io/org/web@" + testDigest}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "web", State: state, Ready: state.Running != nil, RestartCount: restarts, ImageID: "secret-canary-image-id"}}}}
}

func running() corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
}

// Inspect answers a validated status for the target Deployment and its pods, with no image and
// no message; a Deployment that is gone answers Missing, one recreated under the name its own
// UID, and more pods than an answer reports is an error.
func TestInspectReadsTheDeploymentAndItsPods(t *testing.T) {
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "secret-canary back-off"}}
	other := webPod("shop-web-x", testPodUID, running(), 0)
	other.Labels = map[string]string{"app": "other"}
	c, _ := cluster(t, rolledOut(), webPod("shop-web-a", testPodUID, waiting, 3), other)
	in, err := c.Inspect(context.Background(), inspectTarget())
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Validate(inspectTarget(), time.Now(), false); err != nil {
		t.Fatalf("answer invalid: %v %+v", err, in.Workload)
	}
	w := in.Workload
	if w.Missing || w.UID != testUID || w.Generation != 2 || w.ObservedGeneration != 2 || w.Desired != 1 || w.Available != 1 || w.Ready != 1 || w.Updated != 1 || len(w.Conditions) != 1 || w.Conditions[0] != (protocol.WorkloadCondition{Type: "Available", Status: "True", Reason: "MinimumReplicasAvailable"}) {
		t.Fatalf("status %+v", w)
	}
	if len(w.Pods) != 1 || w.Pods[0].UID != testPodUID || w.Pods[0].Phase != "Running" || w.Pods[0].Containers[0] != (protocol.PodContainer{Name: "web", State: "waiting", Reason: "CrashLoopBackOff", RestartCount: 3}) {
		t.Fatalf("pods %+v", w.Pods)
	}

	gone, _ := cluster(t)
	if in, err := gone.Inspect(context.Background(), inspectTarget()); err != nil || !in.Workload.Missing || in.Validate(inspectTarget(), time.Now(), false) != nil {
		t.Fatalf("missing: %+v %v", in, err)
	}

	recreated := rolledOut()
	recreated.UID = types.UID(testPodUID)
	again, _ := cluster(t, recreated)
	if in, err := again.Inspect(context.Background(), inspectTarget()); err != nil || in.Workload.UID != testPodUID || in.Validate(inspectTarget(), time.Now(), false) != nil {
		t.Fatalf("recreated: %+v %v", in, err)
	}

	crowd := []runtime.Object{rolledOut()}
	for i := range protocol.MaxWorkloadPods + 1 {
		crowd = append(crowd, webPod("shop-web-"+strconv.Itoa(i), testPodUID, running(), 0))
	}
	many, _ := cluster(t, crowd...)
	if _, err := many.Inspect(context.Background(), inspectTarget()); err == nil {
		t.Fatal("more pods than an answer reports was answered")
	}
	if _, err := c.Inspect(context.Background(), protocol.InspectionTarget{ContainerID: "x"}); err == nil {
		t.Fatal("a container target was read")
	}
}

// A terminated container keeps its reason word; a reason that is not one word is dropped.
func TestInspectKeepsOnlyReasonWords(t *testing.T) {
	exited := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}
	pulling := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "image pull: secret-canary"}}
	c, _ := cluster(t, rolledOut(), webPod("shop-web-a", testPodUID, exited, 0), webPod("shop-web-b", testUID, pulling, 0))
	in, err := c.Inspect(context.Background(), inspectTarget())
	if err != nil || in.Validate(inspectTarget(), time.Now(), false) != nil {
		t.Fatalf("%+v %v", in, err)
	}
	reasons := map[string]string{}
	for _, p := range in.Workload.Pods {
		reasons[p.Name] = p.Containers[0].State + "/" + p.Containers[0].Reason
	}
	if reasons["shop-web-a"] != "terminated/Error" || reasons["shop-web-b"] != "waiting/" {
		t.Fatalf("reasons %v", reasons)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestInspect' ./internal/runtime/kubernetes/`
Expected: FAIL to build: `c.Inspect undefined (type *Client has no field or method Inspect)`.

- [ ] **Step 3: Implement `Inspect`**

Create `internal/runtime/kubernetes/inspect.go`:

```go
package kubernetes

import (
	"context"
	"errors"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Inspect reads the Deployment a validation watches and the pods its instance and service labels
// select, within callBudget: a Get and one List, both verbs the manifest already grants. A
// Deployment that is gone answers Missing; one recreated under the name answers with its own UID.
// More than MaxWorkloadPods pods is an error (unavailable), never a cut list: a judge summing
// restarts over part of the pods would read a drop as a recreate.
func (c *Client) Inspect(ctx context.Context, target protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
	if err := target.ValidateFor(protocol.RuntimeKubernetes); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	ref := target.Workload
	out := &protocol.ContainerInspection{Target: target}
	d, err := c.cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		out.ObservedAt, out.Workload = time.Now().UTC(), &protocol.WorkloadStatus{Missing: true}
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	status := workloadStatus(d)
	selector := labels.SelectorFromSet(render.Selector(d.Labels[render.LabelInstance], d.Labels[render.LabelService])).String()
	pods, err := c.cs.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: protocol.MaxWorkloadPods + 1})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) > protocol.MaxWorkloadPods || pods.Continue != "" {
		return nil, errors.New("the Deployment has more pods than an inspection reports")
	}
	for _, p := range pods.Items {
		ps := protocol.PodStatus{Name: p.Name, UID: string(p.UID), Phase: string(p.Status.Phase), Containers: []protocol.PodContainer{}}
		for _, pc := range pod(p).Containers {
			pc.Image, pc.ImageID = "", ""
			if !reasonWord.MatchString(pc.Reason) {
				pc.Reason = ""
			}
			ps.Containers = append(ps.Containers, pc)
		}
		status.Pods = append(status.Pods, ps)
	}
	out.ObservedAt, out.Workload = time.Now().UTC(), status
	return out, nil
}

// workloadStatus is the Deployment's rollout state: its counts and its conditions' reason words.
func workloadStatus(d *appsv1.Deployment) *protocol.WorkloadStatus {
	s := &protocol.WorkloadStatus{UID: string(d.UID), Generation: d.Generation, ObservedGeneration: d.Status.ObservedGeneration, Desired: replicas(d.Spec.Replicas),
		Updated: d.Status.UpdatedReplicas, Ready: d.Status.ReadyReplicas, Available: d.Status.AvailableReplicas, Conditions: []protocol.WorkloadCondition{}, Pods: []protocol.PodStatus{}}
	for _, c := range d.Status.Conditions {
		if len(s.Conditions) == protocol.MaxWorkloadConditions || !reasonWord.MatchString(string(c.Type)) {
			continue
		}
		wc := protocol.WorkloadCondition{Type: string(c.Type), Status: string(c.Status)}
		if reasonWord.MatchString(c.Reason) {
			wc.Reason = c.Reason
		}
		s.Conditions = append(s.Conditions, wc)
	}
	return s
}
```

- [ ] **Step 4: Run the adapter tests**

Run: `gofmt -w internal/runtime/kubernetes && go test -count=1 -run 'TestInspect' ./internal/runtime/kubernetes/`
Expected: PASS. (The fake clientset ignores `Limit`, so the 129-pod case reaches the length check; it does filter by label selector, which the `other` pod proves.)

- [ ] **Step 5: Wire it into the cluster agent**

In `cmd/agent/main.go`, in the `if *kube {` branch, replace

```go
		snapshot, logs, deploy, remove = cluster.Snapshot, cluster.Logs, cluster.Deploy, cluster.Remove
```

with

```go
		snapshot, logs, deploy, remove, inspect = cluster.Snapshot, cluster.Logs, cluster.Deploy, cluster.Remove, cluster.Inspect
```

Run: `go build ./cmd/... && test "$(go list -deps ./cmd/server | grep -c k8s.io)" = 0 && echo server-clean`
Expected: `server-clean`.

- [ ] **Step 6: Read the status on a real cluster**

In `internal/runtime/kubernetes/cluster_test.go`, `TestManifestOnARealCluster`, directly below the second deploy's check (`t.Fatalf("second deploy as the agent: %+v", res)` and its closing brace) insert:

```go
	// What a validation reads, as the agent: the settled Deployment available, its pod running.
	watched := protocol.InspectionTarget{Workload: protocol.WorkloadRef{Namespace: deployNamespace, Name: "kind-idle", UID: res.Services[0].UID}}
	in, err := c.Inspect(ctx, watched)
	if err != nil || in.Validate(watched, time.Now(), false) != nil {
		t.Fatalf("inspect as the agent: %+v %v", in, err)
	}
	if w := in.Workload; w.Missing || w.UID != res.Services[0].UID || w.ObservedGeneration < w.Generation || w.Desired != 1 || w.Available != 1 || w.Ready != 1 || len(w.Pods) != 1 || w.Pods[0].Containers[0].State != "running" {
		t.Fatalf("workload status %+v", in.Workload)
	}
```

Run: `go vet ./internal/runtime/kubernetes/ && go test -count=1 -run TestManifestOnARealCluster ./internal/runtime/kubernetes/`
Expected: vet clean; the test SKIPs without `KY_TEST_KUBECONFIG`. When a disposable kind cluster is available, run it for real: `KY_TEST_KUBECONFIG=<kubeconfig> KY_TEST_DEPLOY_IMAGE=registry.k8s.io/pause@sha256:<digest> go test -count=1 -run TestManifestOnARealCluster ./internal/runtime/kubernetes/` → PASS. If no cluster was available, the report says the real-cluster read is unproven.

- [ ] **Step 7: DOX**

In `internal/runtime/kubernetes/AGENTS.md`:
- Purpose: replace `reads inventory, health input and pod logs (M8 PR 20)` with `reads inventory, health input, pod logs (M8 PR 20) and one Deployment's status for health validation`.
- Local Contracts, append a bullet: `- `Inspect` validates `InspectionTarget.ValidateFor(kubernetes)` and, within `callBudget`, gets the target Deployment (not found answers `WorkloadStatus{Missing: true}`; one recreated under the name answers with its own UID, which the server judges `changed`) and lists its pods by `render.Selector` of the Deployment's instance and service labels (`Limit` `MaxWorkloadPods`+1). More than `MaxWorkloadPods` pods, or a list that continues, is an error the agent sends as `unavailable`, never a cut list: restarts summed over part of the pods would read as a recreate. The status carries the Deployment's UID, generation, observed generation, desired (`spec.replicas`, unset 1), updated, ready and available counts and at most `MaxWorkloadConditions` conditions (type, status, reason word; messages never); each pod its name, UID, phase and containers as `pod` maps them, with image and image ID cleared and a reason that is not one word dropped. It uses only `get deployments` and `list pods`, which the ClusterRole already grants.`
- Verification, in the `KY_TEST_KUBECONFIG=...` bullet, replace `applies twice, and proves the removal keeps the claim.` with `applies twice, reads the settled Deployment with `Inspect` (available, its one pod running), and proves the removal keeps the claim.`

In `internal/agent/AGENTS.md`, in the `Options.Kubernetes marks a cluster agent` bullet, replace `` `cmd/agent --kubernetes` wires the cluster's `Deploy` and `Remove`. `` with `` `cmd/agent --kubernetes` wires the cluster's `Deploy`, `Remove` and `Inspect`. ``.

- [ ] **Step 8: Commit**

```bash
gofmt -l cmd internal && git add internal/runtime/kubernetes/inspect.go internal/runtime/kubernetes/inspect_test.go internal/runtime/kubernetes/cluster_test.go cmd/agent/main.go internal/runtime/kubernetes/AGENTS.md internal/agent/AGENTS.md && make tidy-check lint && git commit -m "feat(kubernetes): read a Deployment's status for health validation

Client.Inspect gets the target Deployment and lists its pods by the
instance and service labels, answering Missing when it is gone and
an error past MaxWorkloadPods pods. The cluster agent wires it, so it
advertises kubernetes.inspect.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```


### Task 3: Store and loop — observe a cluster, judge its status, read the capability per runtime

**Files:**
- Modify: `internal/store/validations.go` (`ValidationDetailNoInspect`, `ServiceBaseline.PodUIDs`, `PendingValidation.Kubernetes`, `Observation.Generation`, `Judge`, `judgeService`, new `failingWaits`, `judgeWorkload`, `workloadPods`, `BaselineOf`, `PendingValidations`)
- Modify: `internal/api/validations.go` (`validate`, `observeServices`)
- Test: `internal/store/validations_cluster_test.go` (new: helpers `clusterApply`, `workload`, `pod`; tests `TestJudgeCluster`, `TestBaselineOfACluster`, `TestPendingValidationsOfACluster`)
- Test: `internal/store/validations_test.go:339,489` and `internal/api/validations_test.go:474` (line numbers on the base; a `ServiceBaseline` with a slice is no longer comparable with `!=`), `internal/api/kubernetes_deploy_test.go` (`TestKubernetesApplicationOverTheClusterAgent`: the new detail)
- Docs: `internal/store/AGENTS.md`, `internal/api/AGENTS.md`

**Interfaces:**
- Consumes (Task 1): `protocol.ContainerInspection.Workload`, `protocol.WorkloadStatus`, `protocol.WorkloadRef`, `protocol.CapabilityKubernetesInspect`. Existing: `protocol.DeploymentIdentity{Kind, Namespace, Name, UID, Generation}`, `protocol.KindDeployment`; store test helpers `kubernetesPlanFixture`, `twoServiceSpec`, `kubePlanRequest`, `kubeIdentity`, `putClusterInventory`, `fakeResolver`, `fakeReply`, `digestOf`, `imageCheckKey`, const `kubeUID`.
- Produces:
  - `store.ValidationDetailNoInspect = "the cluster agent cannot report workload status; upgrade the agent image"` (Task 6 gives it a sentence).
  - `store.ServiceBaseline.PodUIDs []string` (`json:"pod_uids,omitempty"`); `store.PendingValidation.Kubernetes bool`; `store.Observation.Generation int64`.
  - A cluster failing verdict's detail is `<service>` or `<service>:<reason>` with `<reason>` one of `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError`, `CreateContainerError` (Task 6 renders it).
  - Store test helper `func clusterApply(t *testing.T, st *SQLStore, a TenantAccess, app *Application, cluster, digest string) *Deployment` (plans the latest revision with `ghcr.io/org/web:1` resolved to `digest`, applies it by hand, settles it succeeded with `kubeIdentity` identities), used again by Task 4.

- [ ] **Step 1: Write the store tests**

Create `internal/store/validations_cluster_test.go`:

```go
package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const (
	podA = "aaaaaaaa-1111-4111-8111-111111111111"
	podB = "bbbbbbbb-2222-4222-8222-222222222222"
)

// clusterApply plans, applies and settles the latest revision of a kubernetesPlanFixture
// application with web resolved to digest, the identities at generation 1.
func clusterApply(t *testing.T, st *SQLStore, a TenantAccess, app *Application, cluster, digest string) *Deployment {
	t.Helper()
	ctx := context.Background()
	ts := st.Tenancy()
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	putClusterInventory(t, ts, cluster, nil)
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digest}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, m.Preview.Project, imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}}
	for _, ps := range d.Plan.Services {
		res.Services = append(res.Services, kubeIdentity(ps))
	}
	if err := ts.SettleDeployment(ctx, cluster, res); err != nil {
		t.Fatal(err)
	}
	return d
}

// workload is a status read of the web Deployment at generation 1: desired 1, with pods.
func workload(available int32, pods ...protocol.PodStatus) *protocol.ContainerInspection {
	return &protocol.ContainerInspection{Target: protocol.InspectionTarget{Workload: protocol.WorkloadRef{Namespace: "shop", Name: "shop-front-web", UID: kubeUID}},
		Workload: &protocol.WorkloadStatus{UID: kubeUID, Generation: 1, ObservedGeneration: 1, Desired: 1, Updated: 1, Ready: available, Available: available, Pods: pods}}
}

func pod(uid, state, reason string, restarts int32) protocol.PodStatus {
	return protocol.PodStatus{Name: "shop-front-web-" + uid[:4], UID: uid, Phase: "Running", Containers: []protocol.PodContainer{{Name: "web", State: state, Reason: reason, RestartCount: restarts}}}
}

// Every cluster verdict, from a status read against the baseline and the settled generation.
func TestJudgeCluster(t *testing.T) {
	base := map[string]ServiceBaseline{"web": {RestartCount: 2, PodUIDs: []string{podA}}}
	web := func(in *protocol.ContainerInspection) []Observation {
		return []Observation{{Service: "web", Presence: PresenceUnknown, Inspection: in, Generation: 1}}
	}
	with := func(in *protocol.ContainerInspection, f func(*protocol.WorkloadStatus)) *protocol.ContainerInspection {
		f(in.Workload)
		return in
	}
	up := func() *protocol.ContainerInspection { return workload(1, pod(podA, "running", "", 2)) }
	for _, tc := range []struct {
		name            string
		obs             []Observation
		final           bool
		verdict, detail string
	}{
		{"available before the end goes on", web(up()), false, "", ""},
		{"available at the end", web(up()), true, VerdictHealthy, ""},
		{"missing", web(&protocol.ContainerInspection{Target: up().Target, Workload: &protocol.WorkloadStatus{Missing: true}}), false, VerdictChanged, "web"},
		{"recreated under the name", web(with(up(), func(w *protocol.WorkloadStatus) { w.UID = podB })), false, VerdictChanged, "web"},
		{"edited past the settled generation", web(with(up(), func(w *protocol.WorkloadStatus) { w.Generation, w.ObservedGeneration = 2, 2 })), false, VerdictChanged, "web"},
		{"recreated by someone else", web(workload(1, pod(podB, "running", "", 0))), false, VerdictChanged, "web"},
		{"a replacement pod that restarts is restarting", web(workload(1, pod(podB, "running", "", 3))), false, VerdictRestarting, "web"},
		{"restarted since the baseline", web(workload(1, pod(podA, "running", "", 3))), false, VerdictRestarting, "web"},
		{"exited and not replaced", web(workload(0, pod(podA, "terminated", "Error", 2))), false, VerdictExited, "web"},
		{"terminated while a container waits goes on", web(workload(0, pod(podA, "terminated", "Error", 2), pod(podB, "waiting", "ContainerCreating", 0))), false, "", ""},
		{"crash loop at once", web(workload(0, pod(podA, "waiting", "CrashLoopBackOff", 2))), false, VerdictUnhealthy, "web:CrashLoopBackOff"},
		{"image pull back-off at once", web(workload(0, pod(podA, "waiting", "ImagePullBackOff", 2))), false, VerdictUnhealthy, "web:ImagePullBackOff"},
		{"config error at once", web(workload(0, pod(podA, "waiting", "CreateContainerConfigError", 2))), false, VerdictUnhealthy, "web:CreateContainerConfigError"},
		{"creating goes on", web(workload(0, pod(podA, "waiting", "ContainerCreating", 2))), false, "", ""},
		{"creating at the end", web(workload(0, pod(podA, "waiting", "ContainerCreating", 2))), true, VerdictUnhealthy, "web"},
		{"unavailable at the end", web(workload(0, pod(podA, "running", "", 2))), true, VerdictUnhealthy, "web"},
		{"not ready at the end", web(with(up(), func(w *protocol.WorkloadStatus) { w.Ready = 0 })), true, VerdictUnhealthy, "web"},
		{"generation not observed at the end", web(with(up(), func(w *protocol.WorkloadStatus) { w.ObservedGeneration = 0 })), true, VerdictUnhealthy, "web"},
		{"unobserved holds the end open", []Observation{{Service: "web", Presence: PresenceUnknown, Generation: 1}}, true, "", ""},
	} {
		if v, d := Judge(tc.obs, base, tc.final); v != tc.verdict || d != tc.detail {
			t.Errorf("%s: %q %q, want %q %q", tc.name, v, d, tc.verdict, tc.detail)
		}
	}
}

// A cluster baseline sums the restarts over every pod and keeps the pods' UIDs; a Deployment
// already missing, recreated or edited gives none, so its first poll judges it changed.
func TestBaselineOfACluster(t *testing.T) {
	got, ok := BaselineOf([]Observation{{Service: "web", Presence: PresenceUnknown, Generation: 1, Inspection: workload(1, pod(podA, "running", "", 2), pod(podB, "running", "", 3))}})
	if !ok || !reflect.DeepEqual(got["web"], ServiceBaseline{RestartCount: 5, PodUIDs: []string{podA, podB}}) {
		t.Fatalf("baseline: %+v %v", got, ok)
	}
	recreated := workload(1, pod(podB, "running", "", 0))
	recreated.Workload.UID = podB
	for name, in := range map[string]*protocol.ContainerInspection{
		"missing":   {Target: recreated.Target, Workload: &protocol.WorkloadStatus{Missing: true}},
		"recreated": recreated,
		"edited":    func() *protocol.ContainerInspection { in := workload(1); in.Workload.Generation = 2; return in }(),
	} {
		obs := []Observation{{Service: "web", Presence: PresenceUnknown, Generation: 1, Inspection: in}}
		got, ok := BaselineOf(obs)
		if !ok || len(got) != 0 {
			t.Errorf("%s: %+v %v", name, got, ok)
		}
		if v, _ := Judge(obs, got, false); v != VerdictChanged {
			t.Errorf("%s judged %q", name, v)
		}
	}
}

// A settled cluster apply is pending with its Deployment identities placed unknown, marked
// Kubernetes, and Health only once the agent advertises kubernetes.inspect.
func TestPendingValidationsOfACluster(t *testing.T) {
	st, a, app, cluster, _ := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	d := clusterApply(t, st, a, app, cluster, digestOf("b"))
	find := func() PendingValidation {
		t.Helper()
		pending, err := ts.PendingValidations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pending {
			if p.DeploymentID == d.ID {
				return p
			}
		}
		t.Fatal("the cluster validation is not pending")
		return PendingValidation{}
	}
	p := find()
	if !p.Kubernetes || p.Health || len(p.Services) != 2 {
		t.Fatalf("pending: %+v", p)
	}
	for _, s := range p.Services {
		if s.Presence != PresenceUnknown || s.Kind != protocol.KindDeployment || s.UID != kubeUID || s.Generation != 1 {
			t.Fatalf("service %+v", s)
		}
	}
	if err := ts.SetEndpointCapabilities(ctx, cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesInspect}); err != nil {
		t.Fatal(err)
	}
	if p := find(); !p.Kubernetes || !p.Health {
		t.Fatalf("with kubernetes.inspect: %+v", p)
	}
	// The baseline round-trips its pod UIDs through the column.
	baseline := map[string]ServiceBaseline{"web": {RestartCount: 1, PodUIDs: []string{podA}}, "api": {PodUIDs: []string{podB}}}
	if err := ts.BeginObservation(ctx, d.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if p := find(); !reflect.DeepEqual(p.Baseline, baseline) {
		t.Fatalf("baseline read back: %+v", p.Baseline)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestJudgeCluster|TestBaselineOfACluster|TestPendingValidationsOfACluster' ./internal/store/`
Expected: FAIL to build: `unknown field Generation in struct literal of type Observation`, `unknown field PodUIDs`, `p.Kubernetes undefined`.

- [ ] **Step 3: The new fields and the detail**

In `internal/store/validations.go`:

Add to the loop's sentences, below `ValidationDetailNoHealth`:

```go
	ValidationDetailNoInspect  = "the cluster agent cannot report workload status; upgrade the agent image"
```

Replace `ServiceBaseline` with:

```go
// ServiceBaseline is one service at the first observation after grace. For a cluster,
// RestartCount is the sum over its pods' containers and PodUIDs are the pods then running.
type ServiceBaseline struct {
	ContainerID  string   `json:"container_id"`
	RestartCount int      `json:"restart_count"`
	PodUIDs      []string `json:"pod_uids,omitempty"`
}
```

In `PendingValidation`, replace the `Health bool` field and its comment with:

```go
	// Health: the endpoint's agent can report what validation reads, container.inspect.health
	// on Docker and kubernetes.inspect on a cluster (Kubernetes).
	Health, Kubernetes bool
```

Replace `Observation` with:

```go
// Observation is one service at one poll: its presence and, when one succeeded, its inspection.
// Generation is a cluster service's settled Deployment generation.
type Observation struct {
	Service    string
	Presence   string
	Inspection *protocol.ContainerInspection
	Generation int64
}
```

- [ ] **Step 4: Judge a cluster service**

Replace `Judge` and `judgeService` with:

```go
// Judge applies the verdict rules to one poll against baseline. It returns the terminal verdict and
// the service that decided it, or "" while the window goes on. final: observe_until has passed, so
// a service still starting is unhealthy and a complete, passing poll is healthy.
func Judge(obs []Observation, baseline map[string]ServiceBaseline, final bool) (verdict, detail string) {
	complete := true
	for _, o := range obs {
		v, reason := judgeService(o, baseline[o.Service], final)
		if v == unobserved {
			complete = false
			continue
		}
		if verdictRank[v] > verdictRank[verdict] {
			verdict, detail = v, o.Service
			if reason != "" {
				detail += ":" + reason
			}
		}
	}
	if verdict == "" && final && complete {
		return VerdictHealthy, ""
	}
	return verdict, detail
}

// judgeService is one service's verdict at one poll and, for a cluster service failed by a
// waiting container, that container's reason.
func judgeService(o Observation, b ServiceBaseline, final bool) (string, string) {
	if o.Inspection != nil && o.Inspection.Workload != nil {
		return judgeWorkload(o, b, final)
	}
	switch o.Presence {
	case PresenceReplaced:
		return VerdictChanged, ""
	case PresenceGone:
		return VerdictExited, ""
	}
	in := o.Inspection
	switch {
	case in == nil:
		return unobserved, ""
	case in.State != "running" && in.State != "restarting":
		return VerdictExited, ""
	case in.State == "restarting" || in.RestartCount > b.RestartCount:
		return VerdictRestarting, ""
	case in.Health == "unhealthy", in.Health == "starting" && final:
		return VerdictUnhealthy, ""
	}
	return "", ""
}

// failingWaits are the waiting reasons that fail a cluster service at once, naming the reason in
// the detail as <service>:<reason>: the pod cannot start as applied, and waiting will not change it.
var failingWaits = map[string]bool{"CrashLoopBackOff": true, "ImagePullBackOff": true, "ErrImagePull": true, "CreateContainerConfigError": true, "CreateContainerError": true}

// judgeWorkload judges a cluster service from its Deployment's status: changed when the
// Deployment is gone, recreated, edited past the settled generation, or its pods were all
// replaced without a restart; then exited, restarting and unhealthy as for a container, where a
// failing waiting reason is unhealthy at once and any shortfall only at the window's end.
func judgeWorkload(o Observation, b ServiceBaseline, final bool) (string, string) {
	w := o.Inspection.Workload
	if w.Missing || w.UID != o.Inspection.Target.Workload.UID || w.Generation > o.Generation {
		return VerdictChanged, ""
	}
	restarts, pods := workloadPods(w)
	kept := slices.ContainsFunc(pods, func(uid string) bool { return slices.Contains(b.PodUIDs, uid) })
	if len(b.PodUIDs) > 0 && len(pods) > 0 && !kept && restarts <= b.RestartCount {
		return VerdictChanged, ""
	}
	waiting, terminated, reason := false, false, ""
	for _, p := range w.Pods {
		for _, c := range p.Containers {
			switch c.State {
			case "waiting":
				waiting = true
				if reason == "" && failingWaits[c.Reason] {
					reason = c.Reason
				}
			case "terminated":
				terminated = true
			}
		}
	}
	switch {
	case terminated && !waiting && w.Available < w.Desired:
		return VerdictExited, ""
	case restarts > b.RestartCount:
		return VerdictRestarting, ""
	case reason != "":
		return VerdictUnhealthy, reason
	case final && (w.ObservedGeneration < w.Generation || w.Available < w.Desired || w.Ready < w.Desired || waiting):
		return VerdictUnhealthy, ""
	}
	return "", ""
}

// workloadPods is the restart count summed over every pod's containers, and the pods' UIDs.
func workloadPods(w *protocol.WorkloadStatus) (int, []string) {
	restarts, uids := 0, []string{}
	for _, p := range w.Pods {
		uids = append(uids, p.UID)
		for _, c := range p.Containers {
			restarts += int(c.RestartCount)
		}
	}
	return restarts, uids
}
```

In `BaselineOf`, replace its doc comment and add the cluster case as the first case of its `switch`:

```go
// BaselineOf is the baseline a first observation gives, false unless every service was either
// inspected or is known gone or replaced. A cluster service whose Deployment is already missing,
// recreated or edited gives none: its next poll judges it changed.
func BaselineOf(obs []Observation) (map[string]ServiceBaseline, bool) {
	out := map[string]ServiceBaseline{}
	for _, o := range obs {
		switch {
		case o.Inspection != nil && o.Inspection.Workload != nil:
			if w := o.Inspection.Workload; !w.Missing && w.UID == o.Inspection.Target.Workload.UID && w.Generation <= o.Generation {
				restarts, pods := workloadPods(w)
				out[o.Service] = ServiceBaseline{RestartCount: restarts, PodUIDs: pods}
			}
		case o.Inspection != nil:
```

(the two existing cases follow unchanged).

- [ ] **Step 5: Read the capability and the runtime; place cluster services**

In `PendingValidations`, replace the doc comment's last line `// placed against the instance's resources and the endpoint's latest inventory.` with:

```go
// placed against the instance's resources and the endpoint's latest inventory; a cluster service is
// placed unknown, for its status read to decide. Health counts container.inspect.health or
// kubernetes.inspect: CapabilitiesFit keeps each to its own runtime.
```

In the query, replace `(SELECT COUNT(*) FROM endpoint_capabilities c WHERE c.endpoint_id=v.endpoint_id AND c.capability=?) FROM` with

```sql
(SELECT COUNT(*) FROM endpoint_capabilities c WHERE c.endpoint_id=v.endpoint_id AND c.capability IN (?,?)),COALESCE((SELECT e.runtime FROM endpoints e WHERE e.id=v.endpoint_id),'') FROM
```

and its arguments `protocol.CapabilityContainerInspectHealth, PendingValidationsPerOrg, MaxPendingValidations` with `protocol.CapabilityContainerInspectHealth, protocol.CapabilityKubernetesInspect, PendingValidationsPerOrg, MaxPendingValidations`. In the scan loop:

```go
		var baseline, result, runtime string
		var instances, health int
		if err := rows.Scan(append(s.dest(), &p.OrganizationID, &p.EnvironmentID, &p.ApplicationID, &p.InstanceID, &p.EndpointID, &baseline, &result, &p.PolicyID, &p.CreatedBy, &instances, &health, &runtime)...); err != nil {
			rows.Close()
			return nil, err
		}
		p.Validation = *s.validation()
		p.Released, p.Health, p.Kubernetes = instances == 0, health > 0, runtime == protocol.RuntimeKubernetes
```

and the placement loop at the end:

```go
		for j := range p.Services {
			// A cluster settle binds no resource: the Deployment's status read decides.
			if p.Services[j].Kind == protocol.KindDeployment {
				p.Services[j].Presence = PresenceUnknown
				continue
			}
			p.Services[j].Presence = presence(p.Services[j].DeploymentIdentity, bound, snap)
		}
```

- [ ] **Step 6: Keep the existing baseline comparisons compiling**

`ServiceBaseline` now holds a slice, so `!=` no longer compiles on it. Add `"reflect"` to the imports of `internal/store/validations_test.go` and `internal/api/validations_test.go` and replace:

- `internal/store/validations_test.go:339`: `got["web"] != (ServiceBaseline{ContainerID: updatedID, RestartCount: 4})` → `!reflect.DeepEqual(got["web"], ServiceBaseline{ContainerID: updatedID, RestartCount: 4})`
- `internal/store/validations_test.go:489`: `pending[0].Baseline["web"] != baseline["web"]` → `!reflect.DeepEqual(pending[0].Baseline["web"], baseline["web"])`
- `internal/api/validations_test.go:474`: `pending[0].Baseline["web"] != (store.ServiceBaseline{ContainerID: priorContainer, RestartCount: 2})` → `!reflect.DeepEqual(pending[0].Baseline["web"], store.ServiceBaseline{ContainerID: priorContainer, RestartCount: 2})`

- [ ] **Step 7: Run the store tests on SQLite and PostgreSQL**

Run: `gofmt -w internal/store && go test -count=1 -run 'TestJudge|TestBaselineOf|TestPendingValidations|TestBeginObservation|TestPresence' ./internal/store/`
Expected: PASS (the Docker `TestJudge` rows are unchanged).

Run: `PG=… go test -count=1 -p 1 -run 'TestPendingValidations|TestBeginObservation' ./internal/store/`
Expected: PASS (the `IN (?,?)` and the runtime subquery on PostgreSQL).

- [ ] **Step 8: Inspect the Deployment from the loop, and name the missing capability**

In `internal/api/validations.go`, `validate`, replace the first case of the `switch` with two:

```go
	case !p.Health && p.Kubernetes:
		s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailNoInspect)
		return
	case !p.Health:
		s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailNoHealth)
		return
```

In `observeServices`, replace the doc comment's first two lines with

```go
// observeServices inspects every settled container the store has not already placed as gone or
// replaced, or a cluster service's settled Deployment, one at a time under the plan's inspection
// budget. invalid: an answer failed validation.
```

and the observation and target with:

```go
		o := store.Observation{Service: svc.Service, Presence: svc.Presence, Generation: svc.Generation}
		// A spent budget sends no expired grant: the service stays unobserved this poll.
		if (svc.Presence == store.PresencePresent || svc.Presence == store.PresenceUnknown) && ctx.Err() == nil {
			target := protocol.InspectionTarget{ContainerID: svc.ContainerID, ImageID: svc.ImageID, CreatedUnix: svc.CreatedUnix}
			if svc.Kind == protocol.KindDeployment {
				target = protocol.InspectionTarget{Workload: protocol.WorkloadRef{Namespace: svc.Namespace, Name: svc.Name, UID: svc.UID}}
			}
```

In `internal/api/kubernetes_deploy_test.go`, `TestKubernetesApplicationOverTheClusterAgent`, replace the comment `// A cluster agent cannot report container health, so the apply is not validated.` with `// This cluster agent does not advertise kubernetes.inspect, so the apply is not validated.` and add `|| settled.Validation.Detail != store.ValidationDetailNoInspect` to the end of the condition on the next-but-one line (after `settled.Validation.Verdict != store.VerdictUnverifiable`).

- [ ] **Step 9: Run the loop's tests**

Run: `gofmt -w internal/api && go test -count=1 -run 'TestValidation|TestRunValidations|TestKubernetesApplicationOverTheClusterAgent' ./internal/api/`
Expected: PASS.

- [ ] **Step 10: DOX**

In `internal/store/AGENTS.md`, in the `Health validation (validations.go, migration 32)` bullet:
- replace `` `Judge` (pure) applies the verdict rules to `Observation`s: `changed` > `exited` > `restarting` > `unhealthy`, detail the deciding service; `healthy` only on a complete passing poll at or after `observe_until`. `` with `` `Judge` (pure) applies the verdict rules to `Observation`s: `changed` > `exited` > `restarting` > `unhealthy`, detail the deciding service; `healthy` only on a complete passing poll at or after `observe_until`. A cluster observation (`Inspection.Workload`) is judged by `judgeWorkload` against the settled generation (`Observation.Generation`): `changed` when the Deployment is `Missing`, its UID is not the target's, its generation passed the settled one, or none of the baseline's `PodUIDs` remains while restarts did not grow; `exited` when a container is terminated, none waits and `available < desired`; `restarting` when the restarts summed over every pod's containers grew; `unhealthy` at once for a container waiting with `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError` or `CreateContainerError` (detail `<service>:<reason>`), and at or after `observe_until` for `available` or `ready` below `desired`, an unobserved generation or any waiting container. `BaselineOf` gives a cluster service the summed restarts and its pods' UIDs (`ServiceBaseline.PodUIDs`, `pod_uids` in the JSON column), and none when its Deployment is already missing, recreated or edited. ``
- replace `` It places each settled identity (`presence`): `` with `` It reads the endpoint's runtime (`PendingValidation.Kubernetes`) and `Health` as a count of `container.inspect.health` or `kubernetes.inspect` (`CapabilitiesFit` keeps each to its runtime), and places each settled identity: a cluster `Kind: Deployment` identity `unknown` (its status read decides; a cluster settle binds no resource), otherwise by `presence`: ``

In `internal/api/AGENTS.md`, in the `validations.go is the health-validation loop` bullet, append: `A cluster row without `kubernetes.inspect` finishes `unverifiable` with `ValidationDetailNoInspect`; with it, each service is inspected by a `Workload` target built from the settled identity (namespace, name, UID) and observed with the settled generation.`

- [ ] **Step 11: Commit**

```bash
gofmt -l cmd internal && git add internal/store/validations.go internal/store/validations_cluster_test.go internal/store/validations_test.go internal/api/validations.go internal/api/validations_test.go internal/api/kubernetes_deploy_test.go internal/store/AGENTS.md internal/api/AGENTS.md && make tidy-check lint && git commit -m "feat(validation): judge a Kubernetes apply from its Deployment's status

Cluster services are inspected through a Workload target built from
the settled identity; the judge reads the Deployment's generation,
counts and pods against a baseline of summed restarts and pod UIDs.
The capability is read per runtime, and a cluster agent without
kubernetes.inspect is unverifiable with an upgrade detail.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```


### Task 4: Store — a cluster rollback target, and a cluster plan pinned by digest

**Files:**
- Modify: `internal/store/rollback.go` (`Rollback.Images` doc, `RollbackTarget`, new `clusterRollback`)
- Modify: `internal/store/validations.go` (`RollbackNamespaceChanged`, `RollbackClaimsChanged`)
- Modify: `internal/store/application_deployment.go` (`PlanRequest.PinImages` doc, `PlanDeployment`)
- Test: `internal/store/kubernetes_rollback_test.go` (new: `TestPlanDeploymentPinsAClusterDigest`, helper `rewritePlan`, `TestClusterRollbackTarget`)
- Docs: `internal/store/AGENTS.md`

**Interfaces:**
- Consumes (Task 3): store test helper `clusterApply(t, st, a, app, cluster, digest) *Deployment`. Existing: `pinPull`, `registry.ParseReference`, `validSHA256`, `applicationSpecDigest`, `ValidateApplicationSpec`, `DeploymentPlan{Namespace, Services, Claims}`, `PlannedService{PullReference, PullDigest, ClaimMounts}`, `FailDeployment`; test helpers `kubernetesPlanFixture`, `twoServiceSpec`, `kubePlanRequest`, `fakeResolver`, `digestOf`, `imageCheckKey`.
- Produces:
  - `store.RollbackNamespaceChanged = "namespace_changed"`, `store.RollbackClaimsChanged = "claims_changed"`; a cluster refusal's reason is `service_set_changed`, `service_set_changed,namespace_changed` or `service_set_changed,claims_changed` (Task 6 renders both codes).
  - `RollbackTarget` for a cluster deployment returns `Rollback{InstanceID, MappingVersion, Project, Revision: <prior apply's revision>, Images: service -> "<host>/<repository>@sha256:<digest>"}` or `no_prior_identity`, `service_set_changed[,...]`, `prior_definition_invalid`.
  - `PlanDeployment` on a cluster instance accepts `PinImages` covering every service with canonical digest references, needs no resolver then, and plans each with `PullReference` = the pin and `PullDigest` = its digest. `api.Server.performRollback` (unchanged) passes `rb.Images` as `PinImages` and a nil resolver.

- [ ] **Step 1: Write the store tests**

Create `internal/store/kubernetes_rollback_test.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// A cluster plan pinned by digest references pulls exactly those digests and asks no registry;
// pins that are not canonical digest references, or that leave a service unpinned, are invalid.
func TestPlanDeploymentPinsAClusterDigest(t *testing.T) {
	st, a, app, _, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	web, api := "ghcr.io/org/web@"+digestOf("c"), "ghcr.io/org/api@"+digestOf("a")
	for name, pins := range map[string]map[string]string{
		"one service unpinned": {"web": web},
		"a tag":                {"web": "ghcr.io/org/web:2", "api": api},
		"an image ID":          {"web": digestOf("c"), "api": api},
		"a tag and a digest":   {"web": "ghcr.io/org/web:1@" + digestOf("c"), "api": api},
		"not canonical":        {"web": "ghcr.io/org/web@" + digestOf("c"), "api": "GHCR.io/org/api@" + digestOf("a")},
		"an unknown service":   {"web": web, "api": api, "db": web},
	} {
		req := kubePlanRequest(m)
		req.PinImages = pins
		if _, err := ts.PlanDeployment(ctx, a, app.ID, req, nil, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The registry would answer another digest for web: the pin wins, and nothing asks it.
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	req := kubePlanRequest(m)
	req.PinImages = map[string]string{"web": web, "api": api}
	for _, r := range []DigestResolver{resolver, nil} {
		d, err := ts.PlanDeployment(ctx, a, app.ID, req, r, imageCheckKey, false)
		if err != nil {
			t.Fatal(err)
		}
		if ps := d.Plan.Services[0]; ps.PullReference != web || ps.PullDigest != digestOf("c") || ps.Reference != "ghcr.io/org/web:1" || d.Plan.Services[1].PullReference != api {
			t.Fatalf("pinned plan %+v", d.Plan.Services)
		}
		_, frame, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes)
		if err != nil {
			t.Fatal(err)
		}
		if p := frame.Services[0].Pull; p == nil || p.Reference != web || p.Digest != digestOf("c") || p.Tag != "" {
			t.Fatalf("frame pull %+v", p)
		}
		if err := ts.FailDeployment(ctx, d.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if calls := resolver.called(); len(calls) != 0 {
		t.Fatalf("registry asked %v", calls)
	}
}

// rewritePlan edits a settled deployment's stored plan or revision, standing in for history the
// API cannot produce in one test.
func rewritePlan(t *testing.T, st *SQLStore, id string, revision int, edit func(*DeploymentPlan)) {
	t.Helper()
	var raw string
	if err := st.db.QueryRowContext(context.Background(), st.rebind(`SELECT plan FROM deployments WHERE id=?`), id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan DeploymentPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatal(err)
	}
	edit(&plan)
	out, _ := json.Marshal(plan)
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE deployments SET plan=?,revision=? WHERE id=?`), string(out), revision, id); err != nil {
		t.Fatal(err)
	}
}

// A cluster deployment rolls back to the digests the previous succeeded apply pinned, at its
// revision, only while it is still the latest apply and the prior plan names the same namespace,
// services and claims with a digest for every service and a revision that still validates.
func TestClusterRollbackTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*DeploymentPlan)
		rev    int
		reason string
	}{
		{"eligible", func(*DeploymentPlan) {}, 1, ""},
		{"another namespace", func(p *DeploymentPlan) { p.Namespace = "billing" }, 1, RollbackServiceSetChanged + "," + RollbackNamespaceChanged},
		{"another service set", func(p *DeploymentPlan) { p.Services = p.Services[:1] }, 1, RollbackServiceSetChanged},
		{"a claim the failed plan lacks", func(p *DeploymentPlan) {
			p.Claims = []protocol.KubernetesClaim{{Name: "shop-front-data", Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce}}
		}, 1, RollbackServiceSetChanged + "," + RollbackClaimsChanged},
		{"a mount the failed plan lacks", func(p *DeploymentPlan) {
			p.Services[0].ClaimMounts = []protocol.KubernetesMount{{Claim: "shop-front-data", MountPath: "/data"}}
		}, 1, RollbackServiceSetChanged + "," + RollbackClaimsChanged},
		{"a service without a digest", func(p *DeploymentPlan) { p.Services[1].PullDigest = "" }, 1, RollbackNoPriorIdentity},
		{"a revision that is gone", func(*DeploymentPlan) {}, 99, RollbackPriorDefinitionInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
			ctx := context.Background()
			ts := st.Tenancy()
			prior := clusterApply(t, st, a, app, cluster, digestOf("b"))
			failed := clusterApply(t, st, a, app, cluster, digestOf("c"))
			rewritePlan(t, st, prior.ID, tc.rev, tc.edit)
			rb, reason, err := ts.RollbackTarget(ctx, a, app.ID, failed.ID)
			if err != nil || reason != tc.reason {
				t.Fatalf("reason %q %v, want %q", reason, err, tc.reason)
			}
			if tc.reason != "" {
				return
			}
			want := &Rollback{InstanceID: m.InstanceID, MappingVersion: m.Version, Project: "shop-front", Revision: 1, Images: map[string]string{"web": "ghcr.io/org/web@" + digestOf("b"), "api": "ghcr.io/org/api@" + digestOf("a")}}
			if !reflect.DeepEqual(rb, want) {
				t.Fatalf("rollback %+v, want %+v", rb, want)
			}
		})
	}
	t.Run("a later apply", func(t *testing.T) {
		st, a, app, cluster, _ := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
		clusterApply(t, st, a, app, cluster, digestOf("b"))
		failed := clusterApply(t, st, a, app, cluster, digestOf("c"))
		clusterApply(t, st, a, app, cluster, digestOf("d"))
		if _, reason, err := st.Tenancy().RollbackTarget(context.Background(), a, app.ID, failed.ID); err != nil || reason != RollbackServiceSetChanged {
			t.Fatalf("reason %q %v", reason, err)
		}
	})
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestPlanDeploymentPinsAClusterDigest|TestClusterRollbackTarget' ./internal/store/`
Expected: FAIL to build: `undefined: RollbackNamespaceChanged`, `undefined: RollbackClaimsChanged`.

- [ ] **Step 3: The two codes**

In `internal/store/validations.go`, at the end of the `const` block that holds `RollbackInterrupted`, add:

```go
	// A cluster rollback's service_set_changed names what changed beside it: the namespace, or
	// the claims or their mounts.
	RollbackNamespaceChanged = "namespace_changed"
	RollbackClaimsChanged    = "claims_changed"
```

- [ ] **Step 4: Decide a cluster rollback**

In `internal/store/rollback.go`, add `"slices"` and `"strings"` to the imports. Replace the `Images` comment of `Rollback` with:

```go
	// Images maps each service to its plan's Replaces image ID on Docker, and on a cluster to the
	// prior succeeded apply's pull reference (host/repository@sha256:...).
```

In `RollbackTarget`'s doc comment, replace `Replaces identity is the container it recreated. An ineligible one returns a reason from the` with:

```go
// Replaces identity is the container it recreated; a cluster deployment's comes from the prior
// succeeded apply's pulled digests (clusterRollback). An ineligible one returns a reason from the
```

Replace

```go
		// A Kubernetes plan replaces no recorded container, so it has nothing to roll back to.
		if len(plan.Services) == 0 || plan.Namespace != "" {
			reason = RollbackNoPriorIdentity
			return nil
		}
```

with

```go
		if plan.Namespace != "" {
			out, reason, err = t.clusterRollback(ctx, tx, a, appID.String(), depID.String(), instance, version, project, plan)
			return err
		}
		if len(plan.Services) == 0 {
			reason = RollbackNoPriorIdentity
			return nil
		}
```

and append to the file:

```go
// clusterRollback decides a cluster deployment's rollback from the instance's previous succeeded
// apply. The validated deployment must still be the latest succeeded apply (nothing applied
// since), and the prior plan must name the same namespace, services and claims (claims are
// immutable and never deleted, so a rollback must not try to change them), a pulled digest for
// every service, and a revision that still validates. The kubelet pulls by digest: nothing is
// checked on the cluster.
func (t *tenancyStore) clusterRollback(ctx context.Context, tx *sql.Tx, a TenantAccess, app, deployment, instance string, version int, project string, plan DeploymentPlan) (*Rollback, string, error) {
	// The instance's two latest succeeded applies: the validated deployment, then the prior one.
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,plan,revision FROM deployments WHERE instance_id=? AND kind='apply' AND state='succeeded' ORDER BY settled_at DESC,id DESC LIMIT 2`), instance)
	if err != nil {
		return nil, "", err
	}
	var ids, plans []string
	var revisions []int
	for rows.Next() {
		var id, raw string
		var revision int
		if err := rows.Scan(&id, &raw, &revision); err != nil {
			rows.Close()
			return nil, "", err
		}
		ids, plans, revisions = append(ids, id), append(plans, raw), append(revisions, revision)
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	switch {
	case len(ids) == 0 || ids[0] != deployment:
		return nil, RollbackServiceSetChanged, nil
	case len(ids) == 1:
		return nil, RollbackNoPriorIdentity, nil
	}
	var prior DeploymentPlan
	if json.Unmarshal([]byte(plans[1]), &prior) != nil {
		return nil, "", ErrRevisionCorrupt
	}
	if prior.Namespace != plan.Namespace {
		return nil, RollbackServiceSetChanged + "," + RollbackNamespaceChanged, nil
	}
	mounts := map[string][]protocol.KubernetesMount{}
	for _, ps := range plan.Services {
		mounts[ps.Name] = ps.ClaimMounts
	}
	if len(prior.Services) != len(plan.Services) || slices.ContainsFunc(prior.Services, func(ps PlannedService) bool { _, ok := mounts[ps.Name]; return !ok }) {
		return nil, RollbackServiceSetChanged, nil
	}
	if !slices.Equal(prior.Claims, plan.Claims) || slices.ContainsFunc(prior.Services, func(ps PlannedService) bool { return !slices.Equal(ps.ClaimMounts, mounts[ps.Name]) }) {
		return nil, RollbackServiceSetChanged + "," + RollbackClaimsChanged, nil
	}
	images := map[string]string{}
	for _, ps := range prior.Services {
		if ps.PullDigest == "" || !strings.HasSuffix(ps.PullReference, "@"+ps.PullDigest) {
			return nil, RollbackNoPriorIdentity, nil
		}
		images[ps.Name] = ps.PullReference
	}
	var specRaw, digest string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), a.OrganizationID, a.EnvironmentID, app, revisions[1]).Scan(&specRaw, &digest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}
	var spec ApplicationSpec
	if err != nil || applicationSpecDigest([]byte(specRaw)) != digest || json.Unmarshal([]byte(specRaw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
		return nil, RollbackPriorDefinitionInvalid, nil
	}
	return &Rollback{InstanceID: instance, MappingVersion: version, Project: project, Revision: revisions[1], Images: images}, "", nil
}
```

- [ ] **Step 5: Plan a cluster pinned by digest**

In `internal/store/application_deployment.go`, replace the `PinImages` comment in `PlanRequest` with:

```go
	// PinImages is set only by a validation's rollback: service to image ID, taken instead of
	// resolving the service's tag. The ID must be on the host; nothing is pulled. On a cluster it
	// maps every service to a digest reference (host/repository@sha256:...), which the plan pulls
	// with no registry call.
```

In `PlanDeployment`, replace the comment above `var namespace string` with:

```go
	// A Kubernetes plan resolves every image at the registry, or takes every one pinned, so it
	// takes the update path. This read only picks the path: both paths re-read the instance under
	// authorization and refuse one whose runtime changed since.
```

and

```go
	kube := namespace != ""
	if kube && (len(r.PinImages) > 0 || resolver == nil) {
		return nil, ErrInvalid
	}
```

with

```go
	kube := namespace != ""
	if kube && len(r.PinImages) == 0 && resolver == nil {
		return nil, ErrInvalid
	}
	if kube {
		// A cluster pin is a canonical digest reference, as pinPull wrote it into the prior plan.
		for _, pin := range r.PinImages {
			if ref, err := registry.ParseReference(pin); err != nil || !validSHA256(ref.Digest) || pin != ref.Host+"/"+ref.Repository+"@"+ref.Digest {
				return nil, ErrInvalid
			}
		}
	}
```

Inside the update path's transaction, replace

```go
		updates := r.Update
		if kube {
			updates = nil
```

with

```go
		updates := r.Update
		if kube {
			// Pins cover every service or none: an unpinned one would need the registry.
			if len(r.PinImages) > 0 && len(r.PinImages) != len(d.Plan.Services) {
				return ErrInvalid
			}
			updates = nil
```

and, in the loop over `updates`,

```go
			ref, err := registry.ParseReference(d.Plan.Services[i].Reference)
```

with

```go
			reference := d.Plan.Services[i].Reference
			if pin, ok := r.PinImages[name]; ok {
				reference = pin // a digest reference: pinned below with no registry call
			}
			ref, err := registry.ParseReference(reference)
```

(The existing `if ref.Digest != "" { pinPull(&d.Plan.Services[i], ref, ref.Digest); continue }` then pins it without queuing registry work; only a cluster reaches this loop with pins, since a Docker plan with pins and no `Update` takes the first path and one with both is already `ErrInvalid`. The anonymous-pull/registry-row check before it still applies, as `kubernetesFrame` re-checks it at apply.)

- [ ] **Step 6: Run the store tests on SQLite and PostgreSQL**

Run: `gofmt -w internal/store && go test -count=1 -run 'TestPlanDeploymentPinsAClusterDigest|TestClusterRollbackTarget|Rollback|TestKubernetes|TestSettleKubernetesDeployment|TestPlanDeployment' ./internal/store/`
Expected: PASS. `TestKubernetesPlanAndFrame` still refuses its `PinImages: {"web": sha256:...}` (an image ID, one service) with `ErrInvalid`, and `TestSettleKubernetesDeployment` still gets `no_prior_identity` for the first cluster apply.

Run: `PG=… go test -count=1 -p 1 -run 'TestPlanDeploymentPinsAClusterDigest|TestClusterRollbackTarget|TestRollbackTarget' ./internal/store/`
Expected: PASS.

- [ ] **Step 7: DOX**

In `internal/store/AGENTS.md`:
- In the `Rollback (rollback.go)` bullet, after `` `prior_images_missing` (a target image neither in the latest inventory's images nor run by a running container). `` insert: `` A cluster deployment (`plan.Namespace` set, `clusterRollback`) returns instead to the instance's previous succeeded apply, read with the validated one as the instance's two latest succeeded `apply` rows by `settled_at`: `service_set_changed` when the validated deployment is no longer the latest (something applied since), `no_prior_identity` when there is no prior one, `service_set_changed,namespace_changed` for another namespace, `service_set_changed` for another service set, `service_set_changed,claims_changed` when its `Claims` or any service's `ClaimMounts` differ (claims are immutable and never deleted), `no_prior_identity` for a service without a pulled digest, `prior_definition_invalid` when its revision no longer validates; eligible, `Images` maps each service to the prior plan's `PullReference` (`host/repository@sha256:...`) at the prior row's revision (equal to `previous_revision`). No image-presence check: the kubelet pulls by digest. ``
- In the same bullet, replace `` `PlanRequest.PinImages` (`json:"-"`, server-set) makes the preflight take a service's image ID instead of its tag `` with `` `PlanRequest.PinImages` (`json:"-"`, server-set) makes a Docker preflight take a service's image ID instead of its tag, and a cluster plan take a canonical digest reference for every service (anything else is `ErrInvalid`) with no registry call ``.
- In the `Kubernetes plans` bullet, replace `refuses `PinImages` and a nil resolver, and needs` with `takes `PinImages` only as described under Rollback (then no resolver is needed; without pins a nil resolver is `ErrInvalid`), and needs`, and replace `` `RollbackTarget` answers `no_prior_identity` for a Kubernetes plan. `` with `` `RollbackTarget` decides a Kubernetes plan by `clusterRollback` (see Rollback). ``

- [ ] **Step 8: Commit**

```bash
gofmt -l cmd internal && git add internal/store/rollback.go internal/store/validations.go internal/store/application_deployment.go internal/store/kubernetes_rollback_test.go internal/store/AGENTS.md && make tidy-check lint && git commit -m "feat(rollback): roll a Kubernetes apply back to the prior digests

RollbackTarget returns a cluster deployment to the pull references
the instance's previous succeeded apply pinned, while nothing was
applied since and the namespace, services and claims are unchanged.
PlanDeployment takes those as PinImages for a cluster and asks no
registry.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```


### Task 5: API — the validation loop and the rollback over a fake cluster agent

**Files:**
- Create: `internal/api/kubernetes_validation_test.go`
- Docs: `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: everything Tasks 1–4 produce; existing test helpers `newClusterHost`, `clusterHost{s, st, ag, sock, ctx, base, do, importApp}`, `clusterCapabilities`, `fakeDigests`, `readEnvelope`, `writeEnvelope`, `grant`, `nextGeneration`, `tomorrow`, `afterGrace`, `afterWindow`; `api.SetPlanInspectorForTest`, `SetDigestResolverForTest`, `PolicyTickForTest`, `WaitPolicyRunsForTest`, `ValidationTickForTest`; `store.PutUpdatePolicy`, `ListPolicyRuns`, `ReadUpdatePolicy`, `AcceptInventory`, `SettleDeployment`, `PendingValidations`.
- Produces: test harness `clusterValidation` (`newClusterValidation(t, capabilities...)`, `inspect`, `restart`, `frame`, `settle`, `automate`, `validation`, `at`, `policy`) and `inspectingCluster`, used only here. No production code changes: a failure here is a defect in the task that owns the behavior.

- [ ] **Step 1: Write the end-to-end tests**

Create `internal/api/kubernetes_validation_test.go`:

```go
package api_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const (
	clusterUID = "0f1e2d3c-4b5a-4968-8776-655443322110"
	clusterPod = "aaaaaaaa-1111-4111-8111-111111111111"
)

var (
	priorDigest  = "sha256:" + strings.Repeat("b", 64)
	updateDigest = "sha256:" + strings.Repeat("c", 64)
)

// clusterValidation is a clusterHost whose application shop (web: ghcr.io/org/web:1) already ran
// one succeeded manual apply at priorDigest: the deployment a rollback returns to. Every status
// read goes through inspect, which reports the web Deployment at the last settled generation, its
// one pod running with restarts.
type clusterValidation struct {
	clusterHost
	app, appID, instance string
	prior                string
	mu                   sync.Mutex
	generation           int64
	restarts             int32
}

func newClusterValidation(t *testing.T, capabilities ...string) *clusterValidation {
	t.Helper()
	v := &clusterValidation{clusterHost: newClusterHost(t, capabilities...)}
	v.app = v.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1}}")
	v.appID = strings.TrimPrefix(v.app, v.base+"/")
	v.do(t, "PUT", v.app+"/mapping", `{"endpoint_id":"`+v.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.app+"/mapping", "", 200)), &mapped); err != nil {
		t.Fatal(err)
	}
	v.instance = mapped.InstanceID
	api.SetPlanInspectorForTest(v.s, v.inspect)
	api.SetDigestResolverForTest(v.s, &fakeDigests{digest: priorDigest})
	body, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop"})
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "POST", v.app+"/deployments", string(body), 201)), &d); err != nil {
		t.Fatal(err)
	}
	v.do(t, "POST", v.app+"/deployments/"+d.ID+"/apply", `{"confirm":"shop"}`, 202)
	v.prior = v.settle(t, v.frame(t))
	return v
}

// inspect is the fake cluster's status read of the web Deployment.
func (v *clusterValidation) inspect(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return protocol.ContainerInspection{Target: target, ObservedAt: time.Now().UTC(), Workload: &protocol.WorkloadStatus{UID: clusterUID, Generation: v.generation, ObservedGeneration: v.generation, Desired: 1, Updated: 1, Ready: 1, Available: 1, Conditions: []protocol.WorkloadCondition{},
		Pods: []protocol.PodStatus{{Name: "shop-web-7d9f8b6c5-x2x4z", UID: clusterPod, Phase: "Running", Containers: []protocol.PodContainer{{Name: "web", State: "running", Ready: true, RestartCount: v.restarts}}}}}}, nil
}

func (v *clusterValidation) restart(n int32) {
	v.mu.Lock()
	v.restarts = n
	v.mu.Unlock()
}

// frame reads the next frame as a cluster deployment request.
func (v *clusterValidation) frame(t *testing.T) protocol.DeploymentRequest {
	t.Helper()
	f := readEnvelope(t, v.ctx, v.sock.conn)
	var req protocol.DeploymentRequest
	if f.Type != protocol.TypeDeploymentApply || json.Unmarshal(f.Payload, &req) != nil || req.Kubernetes == nil {
		t.Fatalf("expected a cluster deployment frame, got %s", f.Type)
	}
	return req
}

// settle answers req as the cluster agent would, the Deployment's generation one higher, and
// reports the Deployment running req's digest in the inventory.
func (v *clusterValidation) settle(t *testing.T, req protocol.DeploymentRequest) string {
	t.Helper()
	v.mu.Lock()
	v.generation++
	generation := v.generation
	v.mu.Unlock()
	digest := req.Services[0].Pull.Digest
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{},
		Services: []protocol.DeploymentIdentity{{Service: "web", Kind: protocol.KindDeployment, Namespace: "shop", Name: "shop-web", UID: clusterUID, Generation: generation, ImageDigest: digest}}}
	if err := v.st.Tenancy().SettleDeployment(context.Background(), v.ag.id, res); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.36.0"}, Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "n1", Ready: true}}, Namespaces: []string{"shop"}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{},
			Workloads: []protocol.Workload{{Kind: protocol.KindDeployment, Namespace: "shop", Name: "shop-web", Desired: 1, Ready: 1, Updated: 1, Images: []string{"ghcr.io/org/web@" + digest}, Application: v.appID, Instance: v.instance}}}})
	if _, err := v.st.Tenancy().AcceptInventory(context.Background(), v.ag.id, nextGeneration(), time.Now(), raw); err != nil {
		t.Fatal(err)
	}
	return req.Deployment
}

func (v *clusterValidation) access() store.TenantAccess {
	return store.TenantAccess{ActorID: "usr_deployer", OrganizationID: "a", EnvironmentID: "env-a", CorrelationID: "cluster-validation-test"}
}

// automate saves an apply-mode policy as the deployer, offers updateDigest and runs its window:
// the policy applies the update and the agent settles it.
func (v *clusterValidation) automate(t *testing.T) string {
	t.Helper()
	var days []int
	for d := range 7 {
		if d != int(time.Now().UTC().Weekday()) {
			days = append(days, d)
		}
	}
	if _, _, err := v.st.Tenancy().PutUpdatePolicy(context.Background(), v.access(), v.appID, store.PolicyInput{Mode: store.PolicyModeApply, Timezone: "UTC", Weekdays: days, StartMinute: 600, EndMinute: 660}); err != nil {
		t.Fatal(err)
	}
	api.SetDigestResolverForTest(v.s, &fakeDigests{digest: updateDigest})
	api.PolicyTickForTest(v.s, tomorrow().Add(10*time.Hour+30*time.Minute))
	api.WaitPolicyRunsForTest(v.s)
	runs, err := v.st.Tenancy().ListPolicyRuns(context.Background(), v.access(), v.appID, 10)
	if err != nil || len(runs) != 1 || runs[0].Outcome != store.RunApplied {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	req := v.frame(t)
	if req.Deployment != runs[0].DeploymentID || req.Services[0].Pull.Digest != updateDigest {
		t.Fatalf("update frame %+v", req)
	}
	return v.settle(t, req)
}

func (v *clusterValidation) validation(t *testing.T, deployment string) *store.Validation {
	t.Helper()
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.app+"/deployments/"+deployment, "", 200)), &d); err != nil || d.Validation == nil {
		t.Fatalf("deployment %s: %+v %v", deployment, d, err)
	}
	return d.Validation
}

func (v *clusterValidation) at(t *testing.T, deployment string, offset time.Duration) {
	t.Helper()
	api.ValidationTickForTest(v.s, v.validation(t, deployment).StartedAt.Add(offset))
}

func (v *clusterValidation) policy(t *testing.T) *store.UpdatePolicy {
	t.Helper()
	p, _, err := v.st.Tenancy().ReadUpdatePolicy(context.Background(), v.access(), v.appID)
	if err != nil || p == nil {
		t.Fatalf("policy: %+v %v", p, err)
	}
	return p
}

var inspectingCluster = append(slices.Clone(clusterCapabilities), protocol.CapabilityKubernetesInspect)

// An automated cluster update whose Deployment stays available with no restart is healthy, and
// its policy stays active.
func TestClusterValidationRecordsAHealthyUpdate(t *testing.T) {
	v := newClusterValidation(t, inspectingCluster...)
	id := v.automate(t)
	if got := v.validation(t, id); !got.Automated || got.Phase != store.PhaseGrace {
		t.Fatalf("opened: %+v", got)
	}
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Phase != store.PhaseObserving {
		t.Fatalf("after grace: %+v", got)
	}
	v.at(t, id, afterWindow)
	if got := v.validation(t, id); got.Verdict != store.VerdictHealthy || got.Rollback != nil {
		t.Fatalf("finished: %+v", got)
	}
	if p := v.policy(t); p.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", p)
	}
}

// A restarting cluster update is rolled back to the prior apply's digest although the registry
// now answers another one; the rollback applies, settles and is validated as a rollback, and the
// policy pauses (Review Focus 3).
func TestClusterValidationRollsBackToThePriorDigests(t *testing.T) {
	v := newClusterValidation(t, inspectingCluster...)
	id := v.automate(t)
	v.at(t, id, afterGrace)
	v.restart(2)
	v.at(t, id, afterGrace+store.ValidationPoll)
	req := v.frame(t)
	if req.Revision != 1 || len(req.Services) != 1 || req.Services[0].Pull.Digest != priorDigest || req.Services[0].Pull.Reference != "ghcr.io/org/web@"+priorDigest || req.Kubernetes.Namespace != "shop" {
		t.Fatalf("rollback frame %+v", req)
	}
	got := v.validation(t, id)
	if got.Verdict != store.VerdictRestarting || got.Detail != "web" || got.Rollback == nil || *got.Rollback != (store.ValidationRollback{DeploymentID: req.Deployment, Revision: 1, Outcome: store.RollbackApplied}) {
		t.Fatalf("validation %+v %+v", got, got.Rollback)
	}
	if p := v.policy(t); p.Status != store.PolicyPaused || p.PausedReason != store.ValidationReasonRolledBack {
		t.Fatalf("policy %+v", p)
	}
	v.restart(0)
	v.settle(t, req)
	if rb := v.validation(t, req.Deployment); !rb.IsRollback || rb.Phase != store.PhaseGrace {
		t.Fatalf("the rollback's validation %+v", rb)
	}
}

// A cluster agent without kubernetes.inspect cannot validate an automated update: it is
// unverifiable with the upgrade detail, and the policy pauses with that reason.
func TestClusterValidationWithoutTheInspectCapability(t *testing.T) {
	v := newClusterValidation(t, clusterCapabilities...)
	id := v.automate(t)
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Verdict != store.VerdictUnverifiable || got.Detail != store.ValidationDetailNoInspect {
		t.Fatalf("validation %+v", got)
	}
	if p := v.policy(t); p.Status != store.PolicyPaused || p.PausedReason != store.ValidationReasonUnverified+store.ValidationDetailNoInspect {
		t.Fatalf("policy %+v", p)
	}
}

// Over the real socket the loop's grant names the settled Deployment and no container, and the
// agent's WorkloadStatus answer, validated, becomes the baseline.
func TestClusterValidationInspectsOverTheAgentSocket(t *testing.T) {
	v := newClusterValidation(t, inspectingCluster...)
	api.SetPlanInspectorForTest(v.s, nil)
	start := v.validation(t, v.prior).StartedAt
	done := make(chan struct{})
	go func() { defer close(done); api.ValidationTickForTest(v.s, start.Add(afterGrace)) }()
	g := grant(t, v.ctx, v.sock)
	if g.Actor != "system-validation" || g.ValidateFor(time.Now(), protocol.RuntimeKubernetes) != nil || g.Target.Workload != (protocol.WorkloadRef{Namespace: "shop", Name: "shop-web", UID: clusterUID}) || g.Target.ContainerID != "" {
		t.Fatalf("grant %+v", g)
	}
	v.restart(1)
	in, _ := v.inspect(context.Background(), g.Target)
	writeEnvelope(t, v.ctx, v.sock.conn, protocol.TypeInspectionResult, protocol.InspectionResult{Request: g.Request, Status: "ok", Result: &in})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick never finished")
	}
	pending, err := v.st.Tenancy().PendingValidations(context.Background())
	if err != nil || len(pending) != 1 || pending[0].Phase != store.PhaseObserving || pending[0].Baseline["web"].RestartCount != 1 || !slices.Equal(pending[0].Baseline["web"].PodUIDs, []string{clusterPod}) {
		t.Fatalf("pending %+v %v", pending, err)
	}
}
```

- [ ] **Step 2: Run them**

Run: `go vet ./internal/api/ && go test -count=1 -run 'TestClusterValidation' ./internal/api/`
Expected: PASS: `TestClusterValidationRecordsAHealthyUpdate`, `TestClusterValidationRollsBackToThePriorDigests` (the rollback frame pulls `priorDigest` while the registry answers `updateDigest`), `TestClusterValidationWithoutTheInspectCapability`, `TestClusterValidationInspectsOverTheAgentSocket` (the answer passes the server's `Validate` on the wire path). A failure names the owning task: the grant or answer shape is Task 1, the verdict or baseline Task 3, the rollback plan or frame Task 4.

Run: `go test -race -count=1 -run 'TestClusterValidation' ./internal/api/`
Expected: PASS, no `DATA RACE` (the harness's generation and restarts are read by the loop's workers under its mutex).

- [ ] **Step 3: DOX**

In `internal/api/AGENTS.md`, in the Verification bullet, directly after `` `validations_test.go` the loop against a fake agent socket through `newPlanHost` with `SetPlanInspectorForTest` answering health and `ValidationTickForTest`/`SetValidationClockForTest` `` insert `` ; `kubernetes_validation_test.go` the loop over the fake cluster agent (`newClusterValidation`): a healthy automated update, a restarting one rolled back to the prior apply's digest with no registry call and validated as a rollback, an agent without `kubernetes.inspect` pausing the policy with the upgrade detail, and the `Workload` grant and answer over the real socket ``.

- [ ] **Step 4: Commit**

```bash
gofmt -l cmd internal && git add internal/api/kubernetes_validation_test.go internal/api/AGENTS.md && make tidy-check lint && git commit -m "test(validation): validate and roll back a Kubernetes apply end to end

A fake cluster agent answers status reads through the plan-time hook
and once over the real socket; an automated update is healthy, or
restarting and rolled back to the prior digest, or unverifiable with
the upgrade detail when the agent cannot inspect.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```


### Task 6: Web — one set of sentences for both runtimes, the runtime-aware warning, `web/dist`

**Files:**
- Modify: `web/src/components/ApplicationValidation.tsx` (`VALIDATION_DETAILS`, `ROLLBACK_REASONS`, new `WAITING_REASONS` and `serviceText`, `verdictText`, `ValidationLine`; `KUBERNETES_UNVERIFIED` removed)
- Modify: `web/src/components/ApplicationDeploymentPlan.tsx:208`, `web/src/components/ApplicationPolicy.tsx` (`HealthWarning`, the `Props` comment)
- Test: `web/src/components/ApplicationValidation.test.tsx`, `web/src/components/ApplicationPolicy.test.tsx`, `web/src/components/ApplicationDeploymentPlan.test.tsx`
- Build: `web/dist`, `web/tsconfig.tsbuildinfo`
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes (Tasks 3, 4): the detail `the cluster agent cannot report workload status; upgrade the agent image`; a cluster failing detail `<service>:<reason>` with `<reason>` in `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError`, `CreateContainerError`; rollback codes `namespace_changed`, `claims_changed`; the endpoint's `runtime` and `capabilities` (`kubernetes.inspect`) from `GET /api/organizations/{org}/endpoints/{endpoint}` (existing `Endpoint` type).
- Produces: `verdictText(v: Validation): string` and `ValidationLine({ v })` without the `kubernetes` flag; `KUBERNETES_UNVERIFIED` no longer exists.

- [ ] **Step 1: Write the failing tests**

In `web/src/components/ApplicationValidation.test.tsx`: drop `KUBERNETES_UNVERIFIED` from the import (`import { ROLLBACK_REASONS, VALIDATION_VERDICTS, ValidationLine, pauseText, reasonText } from './ApplicationValidation';`), add `'namespace_changed', 'claims_changed'` to the end of the `emitted` list in `has a fixed text for every rollback code the server can emit`, and replace the test `gives a Kubernetes apply the one unverifiable sentence instead of the upgrade advice` with:

```tsx
// Mirrors internal/store/validations.go failingWaits: every waiting reason a cluster verdict's
// detail can carry has a fixed text; any other suffix is dropped.
it('names a cluster service and its waiting reason, and the cluster upgrade advice', () => {
  for (const reason of ['CrashLoopBackOff', 'ImagePullBackOff', 'ErrImagePull', 'CreateContainerConfigError', 'CreateContainerError']) {
    const text = line({ verdict: 'unhealthy', detail: `web:${reason}` });
    expect(text.startsWith(`${VALIDATION_VERDICTS.unhealthy} Service web: `)).toBe(true);
    expect(text.endsWith(`(${reason}).`)).toBe(true);
  }
  expect(line({ verdict: 'unhealthy', detail: 'web:CrashLoopBackOff' })).toContain('Service web: its container keeps crashing (CrashLoopBackOff).');
  expect(line({ verdict: 'unhealthy', detail: 'web:secret-canary' })).toBe(`${VALIDATION_VERDICTS.unhealthy} Service web.`);
  expect(line({ verdict: 'unhealthy', detail: 'web:CrashLoopBackOff:secret-canary' })).toBe(VALIDATION_VERDICTS.unhealthy);
  expect(line({ verdict: 'unverifiable', detail: 'the cluster agent cannot report workload status; upgrade the agent image' })).toBe(`${VALIDATION_VERDICTS.unverifiable} The cluster's agent cannot report workload status; upgrade the agent image.`);
  expect(pauseText('update could not be validated: the cluster agent cannot report workload status; upgrade the agent image')).toContain('upgrade the agent image');
  expect(reasonText('service_set_changed,claims_changed')).toBe(`${ROLLBACK_REASONS.service_set_changed} ${ROLLBACK_REASONS.claims_changed}`);
});
```

In `web/src/components/ApplicationPolicy.test.tsx`, give the endpoint fixture a runtime:

```tsx
const endpoint = (capabilities: string[], runtime = 'docker') => ({ id: 'host', environment_id: 'env', name: 'Docker', runtime, state: 'active', facts: {}, fingerprint: 'f', capabilities, alerts: [], created_at: '2026-09-24T10:00:00Z' });
function stubWithEndpoint(p: unknown, capabilities: string[], runtime = 'docker') {
  const fetcher = vi.fn(async (url: string) => String(url).includes('/endpoints/') ? json(endpoint(capabilities, runtime)) : json(p));
```

(the rest of `stubWithEndpoint` unchanged) and add, before `shows each run validation and its rollback, and explains a validation pause`:

```tsx
it('warns for a cluster whose agent cannot report workload status, and not once it can', async () => {
  stubWithEndpoint(policy(), ['kubernetes.inventory', 'kubernetes.deploy'], 'kubernetes');
  render(<ApplicationPolicy base="/app" admin={false} org="a" endpointID="host" />);
  open();
  await screen.findByText(/cluster's agent cannot report workload status.*Upgrade the agent image\./);
  expect(screen.queryByText(/cannot report container health/)).toBeNull();
  cleanup();
  const fetcher = stubWithEndpoint(policy(), ['kubernetes.inventory', 'kubernetes.deploy', 'kubernetes.inspect'], 'kubernetes');
  render(<ApplicationPolicy base="/app" admin={false} org="a" endpointID="host" />);
  open();
  await vi.waitFor(() => expect(fetcher.mock.calls.some(isEndpoint)).toBe(true));
  await screen.findByText(/Plan and apply ·/);
  expect(screen.queryByText(/cannot report/)).toBeNull();
});
```

In `web/src/components/ApplicationDeploymentPlan.test.tsx`: delete `import { KUBERNETES_UNVERIFIED } from './ApplicationValidation';`; in `renders a Kubernetes plan, its step codes and Deployment identities` change the fixture's validation `detail` from `'the agent cannot report container health'` to `'the cluster agent cannot report workload status; upgrade the agent image'` and replace `expect(screen.getByText(KUBERNETES_UNVERIFIED)).toBeTruthy();` with:

```tsx
  expect(screen.getByText("Not validated. The cluster's agent cannot report workload status; upgrade the agent image.")).toBeTruthy();
```

- [ ] **Step 2: Run them to verify they fail**

Run, one at a time: `cd web && npx vitest run --maxWorkers=1 src/components/ApplicationValidation.test.tsx`, then `npx vitest run --maxWorkers=1 src/components/ApplicationPolicy.test.tsx`, then `npx vitest run --maxWorkers=1 src/components/ApplicationDeploymentPlan.test.tsx`
Expected: FAIL: the waiting reasons render `Service web.` only, `claims_changed`/`namespace_changed` have no text, no cluster warning is shown, and the plan's cluster line still reads the Kubernetes-only sentence Step 3 removes.

- [ ] **Step 3: The sentences**

In `web/src/components/ApplicationValidation.tsx`, add to `VALIDATION_DETAILS` below the `'the agent cannot report container health'` entry:

```tsx
  'the cluster agent cannot report workload status; upgrade the agent image': "The cluster's agent cannot report workload status; upgrade the agent image.",
```

add to `ROLLBACK_REASONS` below `service_set_changed`:

```tsx
  namespace_changed: 'The earlier deployment ran in another namespace.',
  claims_changed: "The application's volume claims changed since the earlier deployment.",
```

add above `const SERVICE = ...`:

```tsx
// Why a cluster service failed at once: its container's waiting reason, as <service>:<reason>.
const WAITING_REASONS: Record<string, string> = {
  CrashLoopBackOff: 'its container keeps crashing (CrashLoopBackOff)',
  ImagePullBackOff: 'its image cannot be pulled (ImagePullBackOff)',
  ErrImagePull: 'its image cannot be pulled (ErrImagePull)',
  CreateContainerConfigError: 'its container configuration is invalid (CreateContainerConfigError)',
  CreateContainerError: 'its container cannot be created (CreateContainerError)',
};
```

replace the `KUBERNETES_UNVERIFIED` constant, its comment and `verdictText` with:

```tsx
// serviceText is a failing verdict's deciding service, with a cluster waiting reason when one decided.
function serviceText(detail: string): string {
  const [service, reason, ...rest] = detail.split(':');
  if (!SERVICE.test(service) || rest.length > 0) return '';
  if (reason === undefined) return `Service ${service}.`;
  const why = fixed(WAITING_REASONS, reason);
  return why ? `Service ${service}: ${why}.` : `Service ${service}.`;
}

// verdictText is the verdict and why, through the fixed tables and the service-name shape only.
export function verdictText(v: Validation): string {
  const head = fixed(VALIDATION_VERDICTS, v.verdict) || 'Unrecognised verdict.';
  const why = fixed(VALIDATION_DETAILS, v.detail) || serviceText(v.detail);
  return why ? `${head} ${why}` : head;
}
```

and `ValidationLine` with:

```tsx
export function ValidationLine({ v }: { v: Validation }) {
  const id = v.rollback?.outcome === 'applied' ? v.rollback.deployment_id : '';
  return <span>{verdictText(v)}{v.rollback && <> {rollbackText(v)}</>}{DEPLOYMENT.test(id) && <> Deployment <code title={id}>{id.slice(0, 8)}</code>.</>}</span>;
}
```

In `web/src/components/ApplicationDeploymentPlan.tsx:208`, replace `<ValidationLine v={current.validation} kubernetes={Boolean(current.plan.namespace)} />` with `<ValidationLine v={current.validation} />`.

- [ ] **Step 4: The runtime-aware warning**

In `web/src/components/ApplicationPolicy.tsx`, replace the `Props` comment with:

```tsx
// org and endpointID, when known, let the card check the host for container.inspect.health, or a
// cluster for kubernetes.inspect.
```

and `HealthWarning` with:

```tsx
// HealthWarning names a host whose agent cannot report what validation reads (container health on
// Docker, workload status on a cluster): every automated update there ends unverifiable and pauses
// the policy.
function HealthWarning({ org, endpointID }: { org: string; endpointID: string }) {
  const endpoint = useTenantResource<Endpoint>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpointID)}`);
  if (endpoint.state !== 'ready' || !endpoint.data) return null;
  if (endpoint.data.runtime === 'kubernetes') {
    if (endpoint.data.capabilities.includes('kubernetes.inspect')) return null;
    return <p role="alert">This cluster's agent cannot report workload status, so automated updates here cannot be validated and pause the policy after each one. Upgrade the agent image.</p>;
  }
  if (endpoint.data.capabilities.includes('container.inspect.health')) return null;
  return <p role="alert">This host's agent cannot report container health, so automated updates here cannot be validated and pause the policy after each one. Upgrade the agent.</p>;
}
```

- [ ] **Step 5: Run the tests and the typecheck**

Run, one at a time: `cd web && npx vitest run --maxWorkers=1 src/components/ApplicationValidation.test.tsx`, `npx vitest run --maxWorkers=1 src/components/ApplicationPolicy.test.tsx`, `npx vitest run --maxWorkers=1 src/components/ApplicationDeploymentPlan.test.tsx`, then `npx tsc -b`
Expected: 6, 14 and 41 tests PASS; `tsc -b` exits 0 (nothing else referenced `KUBERNETES_UNVERIFIED` or the `kubernetes` prop: `grep -rn 'KUBERNETES_UNVERIFIED\|kubernetes={' src` prints nothing).

- [ ] **Step 6: Rebuild the embedded bundle, with nothing else running**

Run: `make build-web && git status --short web/dist web/tsconfig.tsbuildinfo`
Expected: the build succeeds and `web/dist` (and `web/tsconfig.tsbuildinfo`) show changes.

- [ ] **Step 7: DOX**

In `web/AGENTS.md`:
- In the `"Update policy" toggle` bullet, replace `the failing service's name when it has the service shape,` with `the failing service's name when it has the service shape (for a cluster, `<service>:<reason>` with the reason's `WAITING_REASONS` text; any other suffix dropped),` and replace `warns when `container.inspect.health` is missing.` with `warns when the endpoint's own capability is missing: `container.inspect.health` on a Docker host, `kubernetes.inspect` on a cluster (naming the agent image upgrade).`
- In the `KubernetesMapping` bullet, replace `never the Docker `.kyyard-prev` text; a Kubernetes apply's unverifiable validation reads `KUBERNETES_UNVERIFIED`, not the upgrade advice.` with `never the Docker `.kyyard-prev` text; a cluster apply's validation renders through the same `ValidationLine` as a Docker one.`

- [ ] **Step 8: Commit**

```bash
git add web/src/components/ApplicationValidation.tsx web/src/components/ApplicationValidation.test.tsx web/src/components/ApplicationPolicy.tsx web/src/components/ApplicationPolicy.test.tsx web/src/components/ApplicationDeploymentPlan.tsx web/src/components/ApplicationDeploymentPlan.test.tsx web/dist web/tsconfig.tsbuildinfo web/AGENTS.md && git commit -m "feat(web): show Kubernetes validation like Docker's

The Kubernetes-only unverifiable sentence is gone: both runtimes use
the verdict table, a cluster verdict names its waiting reason, the
cluster upgrade detail and the two new rollback codes have text, and
the policy card warns per runtime about the missing capability.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```


### Task 7: Documents, then the gate

**Files:**
- Modify: `docs/application-schema.md` (Rollback honesty, Health validation, Kubernetes), `docs/agent-protocol.md` (Container inspection, Kubernetes runtime), `docs/authorization-matrix.md` (Matrix), `README.md` (the Kubernetes deployment paragraph), `KyYard-Implementation-Plan.md` (§8)
- Unchanged by design: `docs/threat-model.md`, `internal/runtime/kubernetes/manifest/`, `internal/api/disclosure*.go`, root `AGENTS.md`

**Interfaces:**
- Consumes: the behavior of Tasks 1–6, exactly as their commits and child `AGENTS.md` edits state it.
- Produces: operator and design documents that match the code; the full gate result.

- [ ] **Step 1: `docs/application-schema.md`**

- Rollback honesty, third bullet: after `the policy stays paused until an administrator resumes it.` append ` On a cluster the rollback re-applies the prior revision pinned to the digests the instance's previous succeeded apply pulled (`host/repository@sha256:...`), only while nothing was applied since and the namespace, services and claims are unchanged; the kubelet pulls them by digest, and no claim is changed or deleted.`
- Health validation, second bullet: replace `After the 30 s grace it needs the endpoint's agent to advertise `container.inspect.health`: a missing capability is `unverifiable` at once.` with `After the 30 s grace it needs the endpoint's agent to advertise `container.inspect.health`, or on a cluster `kubernetes.inspect`: a missing capability is `unverifiable` at once (`the agent cannot report container health`; on a cluster `the cluster agent cannot report workload status; upgrade the agent image`).`
- Health validation: insert a new bullet directly after the second bullet:

```markdown
- On a cluster (M8 follow-on) each service's settled Deployment identity is the target (`workload`: namespace, name, UID) and the agent answers a status read made with `get deployments` and `list pods` (docs/agent-protocol.md, Container inspection); a cluster service is never placed by inventory. The baseline is the restart count summed over the pods' containers and the pods' UIDs. A cluster service is `changed` when its Deployment is gone, recreated under the name (another UID), edited past the settled generation, or every baseline pod was replaced while restarts did not grow; `exited` when a container is terminated, none waits and fewer replicas are available than desired; `restarting` when the summed restarts grew; `unhealthy` at once for a container waiting with `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError` or `CreateContainerError` (detail `<service>:<reason>`), and at or after `observe_until` for fewer available or ready replicas than desired, an unobserved generation or any other waiting container; `healthy` for a complete poll at or after `observe_until` with none of these. Readiness is Kubernetes' own: KyYard renders no probe, so a running container counts as ready.
```

- Health validation, the `Rollback, for an automated update ...` bullet: after `A rollback's own verdict never triggers another.` append ` A cluster deployment's target is the instance's previous succeeded apply instead, decided in this order: `service_set_changed` when anything was applied since, `no_prior_identity` without a prior apply, `service_set_changed,namespace_changed` for another namespace, `service_set_changed` for another service set, `service_set_changed,claims_changed` when the claims or their mounts differ, `no_prior_identity` for a service without a pulled digest, `prior_definition_invalid`; eligible, the plan pins every service to the prior `host/repository@sha256:...` reference with no registry call, at the prior apply's revision, and nothing is checked on the cluster.`
- Kubernetes, the `Implemented (M8 PR 21), stateless.` section's third paragraph: replace `A cluster agent cannot report container health, so every apply is `unverifiable` and an automated policy pauses; rollback is `ineligible` (`no_prior_identity`).` with `Every succeeded apply is validated, and an automated one rolled back, as on Docker (Health validation, above), through an agent that advertises `kubernetes.inspect`; with an older agent every apply is `unverifiable` with the upgrade detail and an automated policy pauses.`

- [ ] **Step 2: `docs/agent-protocol.md`**

- Container inspection: insert a paragraph directly after the one that ends `without it every automated update on that host is `unverifiable`.`:

```markdown
A cluster agent that advertises `kubernetes.inspect` (M8 follow-on) answers the same frames for a Deployment. The target is `workload: {namespace, name, uid}` (DNS-1123 labels and the Deployment's UID) with every container field empty; a Docker target never carries `workload`, and each agent refuses the other runtime's target as an invalid grant. The answer carries `workload` (`uid`, `generation`, `observed_generation`, `desired`, `updated`, `ready`, `available`, at most 8 `conditions` {type, status `True`|`False`|`Unknown`, reason}, at most 128 `pods` {name, uid, phase `Pending`|`Running`|`Succeeded`|`Failed`|`Unknown`, at most 32 `containers` {name, state `running`|`waiting`|`terminated`, reason, ready, restart_count 0..1,000,000, `image` and `image_id` empty}}, or `missing: true` and nothing else when the Deployment is gone) and every Docker field empty; a reason is one CamelCase word or empty, and no message is read. A `uid` other than the target's is a Deployment recreated under the name. The agent answers from one `get` of the Deployment and one `list` of pods by its instance and service labels, verbs the ClusterRole already grants; more than 128 pods is `unavailable`, never a cut list, and so is any answer whose JSON exceeds the 32 KiB frame. Only the validation loop asks: there is no HTTP inspection route for a cluster. A cluster agent with `kubernetes.inspect` is refused at hello by a server older than this rule (`capability_mismatch`): upgrade the server first.
```

- Kubernetes runtime, Capabilities bullet: replace `` `kubernetes.claims` with `kubernetes.deploy` when it applies claims (every agent from M8 PR 22 on), and nothing else `` with `` `kubernetes.claims` with `kubernetes.deploy` when it applies claims (every agent from M8 PR 22 on), `kubernetes.inspect` when it reads a Deployment's status for health validation, and nothing else ``.

- [ ] **Step 3: `docs/authorization-matrix.md`**

In the Matrix table, directly below the `| Update-policy runs (implemented, M7b) | ...` row add:

```markdown
| Health validation (implemented, M7b; clusters, M8) | no new action: the loop inspects as the wire identifier `system-validation` (no user, no session) through the plan-time inspection, a Docker container or a cluster's Deployment and pods alike; a rollback acts as the policy's `created_by` with `application.deploy` re-checked | | | | | none read; a cluster status read carries no image, message or value | `application.validation` as `system` on `<app>/deployments/<id>`; a rollback's plan and apply rows under the creator |
```

- [ ] **Step 4: `README.md`**

In the Kubernetes deployment section, replace

```text
an imagePullSecret. A Kubernetes apply is not health-validated and never rolls back
automatically; plan and apply the earlier revision to go back. Removing the application
deletes the objects labelled as its own and nothing else.
```

with

```text
an imagePullSecret. A Kubernetes apply is health-validated like a Docker one (Update policies,
below): for two and a half minutes the server reads the Deployment and its pods, and an update a
policy applied that fails is returned to the digests the previous apply pulled, unless something
was applied since or the namespace, services or volume claims changed. It sees what Kubernetes
reports (replica counts, whether the rollout reached the latest change, container states and
waiting reasons, restarts); it cannot see whether the application answers requests, since KyYard
adds no readiness probe. A Deployment someone else edits, scales or recreates during the window is
`changed` and left alone. This needs a current agent image: an older one cannot report workload
status, every automated update pauses the policy, and the policy card says so. Removing the
application deletes the objects labelled as its own and nothing else.
```

- [ ] **Step 5: `KyYard-Implementation-Plan.md` §8**

Directly after the paragraph that begins `Implemented M8 PR 22 (`feat/migration-analysis`)`, insert:

```markdown
Implemented M8 follow-on (`feat/k8s-health-validation`): a Kubernetes apply is health-validated and rolled back like a Docker one. The inspection frames carry a Deployment target (`workload`) and a bounded status answer from a cluster agent advertising `kubernetes.inspect` (`get deployments`, `list pods`; ClusterRole unchanged). The store judges the Deployment's generation, replica counts, container states, waiting reasons and summed restarts against the baseline, and a failed automated update returns to the digests the instance's previous succeeded apply pulled, planned with pins and no registry call, while nothing was applied since and the namespace, services and claims are unchanged. An older cluster agent makes every automated cluster update `unverifiable` with an upgrade detail. The real-cluster status read is covered by the developer-run `TestManifestOnARealCluster`.
```

- [ ] **Step 6: The DOX pass**

Run: `grep -rn 'not health-validated\|KUBERNETES_UNVERIFIED\|cannot report container health, so every apply' docs/application-schema.md docs/agent-protocol.md README.md web/AGENTS.md internal/*/AGENTS.md internal/runtime/kubernetes/AGENTS.md AGENTS.md`
Expected: no output. Root `AGENTS.md` needs no change (it names no validation or capability rule this work changes); `docs/threat-model.md` and the manifest are unchanged because the ClusterRole already grants both verbs; say both in the PR description.

- [ ] **Step 7: Commit the documents**

```bash
git add docs/application-schema.md docs/agent-protocol.md docs/authorization-matrix.md README.md KyYard-Implementation-Plan.md && git commit -m "docs: Kubernetes health validation and rollback

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

- [ ] **Step 8: The gate (controller only, nothing else running)**

Run each, one at a time, and stop at the first failure:

```bash
test "$(go list -deps ./cmd/server | grep -c k8s.io)" = 0 && echo server-clean
git diff --quiet fc83316 -- internal/store/migrations docs/threat-model.md internal/runtime/kubernetes/manifest && echo no-schema-or-rbac-change
go test -count=1 -run TestClusterDisclosureMatchesTheManifestAndThreatModel ./internal/api/
KY_TEST_DOCKER_INSPECTION_IMAGE=alpine:3.24 go test -count=1 -run '^TestInspectionRealDocker$' ./internal/runtime/docker/
make ci
```

Expected: `server-clean`, `no-schema-or-rbac-change`, PASS, PASS (a Docker answer still validates with the new `Validate`), and `==> Local CI checks passed`.

Then, alone, the whole suite on PostgreSQL:

```bash
PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test -count=1 -p 1 ./...
```

Expected: every package `ok`. When a disposable kind cluster is available, also run Task 2 Step 6's real-cluster command; otherwise report the real-cluster status read as unproven.

- [ ] **Step 9: Confirm the branch**

Run: `git status --short && git log --oneline fc83316..HEAD`
Expected: a clean tree (untracked files that predate this work aside) and, above the spec commit, the six task commits and the documents commit.
