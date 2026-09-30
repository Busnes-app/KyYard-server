# Kubernetes Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On a Kubernetes endpoint, an operator gets what a Docker host has: a page per workload with Overview, Configuration, Logs, Terminal and Activity tabs; rollout restart, scale and delete on workloads and pods from icon toolbars; an edit of a workload's pod template (image, command, args, env, resources, replicas) saved with a resource-version precondition; and a terminal into a pod container.

**Architecture:** Workload actions travel on the existing `command` frame with a `namespace/kind/name` reference (the same slot image actions use) and are executed by a Kubernetes `Operate` under a `SelfSubjectAccessReview`, so an agent on an older manifest answers `forbidden` and the UI says "regenerate the manifest". The workload configuration read reuses the `configuration.open` grant with its existing `Workload` target and a new `WorkloadConfiguration` result. The edit is a new `workload.apply` frame that does a full `Update` with the read's `resourceVersion` as the precondition and reports through `deployment.result`, so the direct-command store (`endpoint_commands.result`) and the "Last change" panel work unchanged. Pod exec extends `ExecSpec` with a pod target and the agent's `ExecSession` is backed by `client-go/tools/remotecommand`.

**Tech Stack:** Go 1.26, `k8s.io/client-go` v0.37.1 (adds `tools/remotecommand` and its transitive deps), fake clientset tests, React 19 + TypeScript, vitest.

**Spec:** `docs/superpowers/specs/2026-09-29-container-management-design.md`, section 3.

## Global Constraints

- Granted namespaces (`endpoints.deploy_namespaces`) are the boundary for every write and for exec; a workload or pod outside them is refused server-side (422 `namespace_not_granted`) before any frame is sent.
- Permissions: `workload.restart`, `workload.scale` → `container.operate`; `workload.delete`, `pod.delete` → `container.destroy` (typed name, `expects` unused); `workload.configure`/`workload.apply` → `container.configure` (service tokens refused; OA only); pod exec → `container.exec` (OA only). No host-level gate is needed: the editable fields cannot grant host access.
- A KyYard-managed workload (`Workload.application != ""`, i.e. labelled `app.kubernetes.io/managed-by: kyyard`) may be restarted and scaled, but its configuration read and apply are refused 409 `application_managed`.
- No manifest revision mechanism. Every cluster write runs a `SelfSubjectAccessReview` first; `forbidden` is a `denied` step/outcome whose fixed UI text tells the operator to regenerate and apply the manifest (the existing `manifestNote` pattern). The cluster agent advertises `kubernetes.workloads` when `Operate` is wired and `pod.exec` when `Exec` is wired; both are added to `kubernetesCapabilities` and never to a Docker agent.
- Secret-backed env (`valueFrom.secretKeyRef`, `envFrom` a Secret) is shown by reference and preserved verbatim on apply; the agent's role never reads Secrets outside its own identity namespace. Literal env values transit in memory only, exactly as the Docker read.
- The `Expectation` gains `Replicas *int32` (`json:"replicas,omitempty"`) for `workload.scale`; a DaemonSet scale is refused. Every wire change is additive; existing Docker frames are unchanged (golden tests stay green).
- The cluster agent gets a writable `emptyDir` at `/var/lib/kyyard-agent` in the generated Deployment and uses it as its command/deployment ledger directory (today the ledgers resolve to relative paths on a read-only root filesystem).
- Fixed error texts; never render server text; `secureFetch` for writes; every icon control labelled; `web/dist` rebuilt and committed in the last task; DOX docs updated in the same PR.
- Commit messages end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. **An agent whose manifest predates the new RBAC** must answer every workload write with `forbidden` and the UI must say to regenerate the manifest, never hang or show a generic error. Pinned in Task 3 (SSAR test) and Task 7 (fixed text).
2. **A concurrent edit** (resource version changed between read and apply) must be refused with `conflict`, never silently overwrite. Pinned in Task 3 (`TestApplyWorkloadConflict`).
3. **A workload in a namespace the manifest does not grant** must be refused server-side before any frame. Pinned in Task 5.
4. **Pod exec to a pod that restarted between confirm and open** (new pod UID) must be refused, not attach to the replacement. Pinned in Task 4 (UID precondition) and Task 5.
5. **Scale to 0 on a Deployment the operator confused with a DaemonSet** and scaling beyond a sane bound: DaemonSet refused; replicas bounded 0..1000. Pinned in Task 1 (Validate) and Task 3.

---

### Task 1: Protocol additions

**Files:** `internal/agent/protocol/kubernetes.go`, `kubernetes_workloads.go` (new), `exec.go`, `messages.go`, `configuration.go`, tests beside each.

**Interfaces:**
```go
// messages.go
ActionWorkloadRestart = "workload.restart"; ActionWorkloadScale = "workload.scale"; ActionWorkloadDelete = "workload.delete"; ActionPodDelete = "pod.delete"
type Expectation struct { ImageDigest string; State string; Replicas *int32 `json:"replicas,omitempty"` }
// kubernetes_workloads.go
CapabilityKubernetesWorkloads = "kubernetes.workloads"; CapabilityPodExec = "pod.exec"
type WorkloadRef struct { Namespace, Kind, Name string }             // Kind deployment|statefulset|daemonset|pod
func ParseWorkloadRef(reference string) (WorkloadRef, error)          // "<ns>/<kind>/<name>", DNS-label grammar, kind lower-case
func (r WorkloadRef) String() string
type WorkloadContainer struct { Name, Image, ImageID string; Command, Args []string; Env []WorkloadEnv; Resources WorkloadResources }
type WorkloadEnv struct { Name, Value string; SecretRef string `json:"secret_ref,omitempty"`; ConfigMapRef string `json:"config_map_ref,omitempty"` } // one of Value|SecretRef|ConfigMapRef; refs are "<name>/<key>"
type WorkloadResources struct { CPURequest, CPULimit, MemoryRequest, MemoryLimit string } // Kubernetes quantity strings, "" when unset
type WorkloadConfiguration struct { Target WorkloadRef; ObservedAt time.Time; ResourceVersion string; Replicas *int32; Paused bool; Strategy string; Containers []WorkloadContainer; InitContainers []WorkloadContainer; EnvFrom []string; Managed bool; Unsupported []string }
func (c *WorkloadConfiguration) Validate(target WorkloadRef, now time.Time) error   // bounds: 16 containers, MaxArgv, env per container 128, quantity strings ≤ 32 bytes and parseable by resource.ParseQuantity
// ConfigurationResult gains: Workload *WorkloadConfiguration `json:"workload,omitempty"` (exactly one of Result|Workload when Status ok)
// InspectionTarget.Workload already exists; ConfigurationOpen with a Workload target and empty ContainerID is valid on a kubernetes runtime (ValidateFor(runtime))
TypeWorkloadApply = "workload.apply"; MaxWorkloadApplyBytes = 128 << 10
type WorkloadApply struct { Request, Endpoint string; IssuedAt, Deadline time.Time; Target WorkloadRef; ResourceVersion string; Spec WorkloadConfiguration }
func (a WorkloadApply) Validate(now time.Time) error
// results: DeploymentResult with one service named "workload"; step names: precondition, apply, rollout; codes: forbidden, conflict, application_managed, rollout_timeout (existing), namespace_not_granted
// exec.go
type PodTarget struct { Namespace, Pod, Container, UID string }
// ExecSpec gains Pod *PodTarget `json:"pod,omitempty"`; Validate: when Pod != nil, Container/ImageID/User must be empty, Pod fields DNS-label + UID grammar, Argv as today
```
`kubernetesCapabilities` gains both capabilities. `CommandPermission` mapping is Task 5.

- [ ] **Step 1: Failing tests** for every rule above (`ParseWorkloadRef` accepts `shop/deployment/web`, rejects `Shop/Deployment/web`, `shop/web`, `../x`; `Expectation` round-trip without `replicas` is byte-identical to before; `ExecSpec` with a pod and a container ID together fails; `WorkloadApply` bounds; `ConfigurationResult` with both results fails; `WorkloadConfiguration` env with both value and secret_ref fails; quantity `1.5Gi` ok, `banana` fails).
- [ ] **Step 2: Run** `go test ./internal/agent/protocol` → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS; `TestDeploymentRequestWireUnchanged` and `TestWebVocabularyFixture` untouched (add the new step codes to `stepCodes`; regenerate the web fixture with `KY_UPDATE_FIXTURES=1` and commit it).
- [ ] **Step 5: Commit** `protocol: workload actions, configuration, apply frame and pod exec targets`.

---

### Task 2: Manifest RBAC and agent scratch volume

**Files:** `internal/runtime/kubernetes/manifest/manifest.go`, `manifest_test.go`, `cmd/agent/main.go` (CommandDir for the cluster agent), `internal/runtime/kubernetes/AGENTS.md`.

- Per-namespace Role `kyyard-agent-deploy` gains `apps: statefulsets, daemonsets` with `get, list, patch, update, delete`; `deployments` keeps its verbs; `"" : pods` gains `delete`; `"" : pods/exec` `create`. The ClusterRole is unchanged (reads stay cluster-wide).
- The agent Deployment mounts `emptyDir` `scratch` at `/var/lib/kyyard-agent`; `cmd/agent/main.go` sets the cluster agent's `CommandDir`/`IdentityDir` to that path (identity stays in the Secret store; only ledgers use the directory). `--scratch-dir` flag defaults to it.
- [ ] Tests: golden manifest test updated; a test that a granted namespace's Role contains the four new rules and the ClusterRole is unchanged; `TestManifestOnARealCluster` extended (local only) to assert `SelfSubjectAccessReview` allows `patch deployments`, `delete pods` and `create pods/exec` in the granted namespace and denies them elsewhere.
- [ ] **Commit** `kubernetes: workload RBAC and an agent scratch volume`.

---

### Task 3: Kubernetes runtime: operate, read, apply

**Files:** `internal/runtime/kubernetes/workloads.go` (new), `workloads_test.go`, `AGENTS.md`.

**Interfaces:**
```go
func (c *Client) Operate(ctx context.Context, cmd protocol.Command) protocol.CommandResult   // matches Options.Operate; only workload.* / pod.delete; anything else denied "unsupported action"
func (c *Client) ReadWorkload(ctx context.Context, target protocol.InspectionTarget) (*protocol.WorkloadConfiguration, error)
func (c *Client) ApplyWorkload(ctx context.Context, req protocol.WorkloadApply, started func()) protocol.DeploymentResult
```
Rules: every action first checks the namespace is in `granted` (the agent learns granted namespaces from the hello/state; if the agent does not carry them, the server check in Task 5 is the boundary and the agent relies on the SSAR); then `SelfSubjectAccessReview` for the verb → `denied forbidden`; restart patches the pod-template annotation `kyyard.busnes.app/restarted-at` (Deployment/StatefulSet/DaemonSet); scale sets `spec.replicas` from `Expects.Replicas` (DaemonSet → `denied unsupported`); delete uses `Background` propagation with `Preconditions{UID}` from a fresh Get; pod delete the same; `ReadWorkload` maps the pod template (containers, init containers, env literal/secret/configmap refs, `envFrom` names, resources, replicas, paused, strategy), sets `Managed` from the managed-by label, `ResourceVersion`, and `Unsupported` for anything the apply cannot carry (volumes are preserved untouched, so not unsupported; `unsupported` covers only fields the form would drop: e.g. `lifecycle`, `securityContext` differences are preserved as-is because apply mutates only the listed fields); `ApplyWorkload`: Get → `Managed` → `application_managed`; `resourceVersion` mismatch → `conflict`; mutate only image/command/args/env/resources/replicas/paused on the fetched object and `Update` (a 409 from the API is `conflict` too); then wait for rollout with the existing `rollout` helper (bounded by the frame deadline) → `rollout_timeout`.
- [ ] Tests with the fake clientset (reactors for SSAR allow/deny): each action's happy path, `forbidden`, DaemonSet scale denied, delete UID precondition, read maps refs without values for secret env, apply conflict, apply managed refusal, apply preserves untouched fields (volumes, probes, securityContext) byte-for-byte, replicas bound.
- [ ] **Commit** `kubernetes: workload operate, read and apply`.

---

### Task 4: Kubernetes runtime: pod exec

**Files:** `internal/runtime/kubernetes/exec.go` (new), `exec_test.go`, `go.mod`/`go.sum` (`go get k8s.io/client-go/tools/remotecommand@v0.37.1`, then `go mod tidy`), `internal/runtime/kubernetes/AGENTS.md`, `docs/agent-protocol.md` §7 sentence about streaming dependencies.

**Interfaces:** `func (c *Client) OpenExec(ctx context.Context, spec protocol.ExecSpec) (client.ExecSession, error)` — requires `spec.Pod`; Gets the pod, checks `UID == spec.Pod.UID` and phase Running and the container exists, then `remotecommand.NewSPDYExecutor` on `pods/{name}/exec` with `Stdin/Stdout/TTY` and `Command = spec.Argv`; the session is an `io.ReadWriteCloser` over pipes with a `TerminalSizeQueue` fed by `Resize`, idle (15 min) and absolute (8 h) deadlines like Docker's, and `Inspect` returning the exit code from `exec.CodeExitError` once `StreamWithContext` returns. `Close` cancels the context and closes the pipes (unblocks Read/Write).
- [ ] Tests: unit tests with a fake round tripper are impractical for SPDY; test the wrapper's contract with an injected `executor` interface (start/stream func): resize reaches the queue, Close unblocks Read, exit code mapping, idle timeout closes. A real-cluster test `TestPodExecOnARealCluster` (gated on `KY_TEST_KUBECONFIG`) runs `sh -c 'echo hi; exit 3'` in a fixture pod and asserts output and exit code 3.
- [ ] **Commit** `kubernetes: pod exec session over remotecommand`.

---

### Task 5: Agent wiring, server routes and store

**Files:** `internal/agent/client/connect.go` (hello caps for cluster: `kubernetes.workloads` when `Operate`, `pod.exec` when `Exec`; route configuration frames for cluster agents to `Options.ConfigureWorkload`), `client/exec.go` (nothing if `Options.Exec` is generic), `cmd/agent/main.go` (wire `cluster.Operate`, `cluster.ReadWorkload`, `cluster.ApplyWorkload`, `cluster.OpenExec`), `internal/agent/client/deployments.go` (handle `workload.apply` like `deployment.apply`), `internal/store/commands.go` (`CommandPermission` for the four actions; `CreateCommand` accepts a `reference` that parses as a `WorkloadRef` for them and requires `expects.replicas` for scale and `confirm == name` for the deletes), `internal/store/commands_recreate.go` (a `CreateWorkloadApply` direct command: action `workload.apply`, reference, `result`, settled by `SettleDirectCommand` with service `workload`), `internal/store/commands.go` `ListCommands` gains a `reference` filter, `internal/api/runtime_gate.go` (`kubernetesRoute`), `internal/api/endpoint_handlers.go` (`handleDispatchCommand` accepts workload actions on a kubernetes endpoint: gate by runtime, granted namespace, capability `kubernetes.workloads`, managed check for none of these), `internal/api/workload_handlers.go` (new: `GET .../workloads/{namespace}/{kind}/{name}/configuration` under `container.configure` via `s.ask` with a Workload target; `POST .../workloads/{namespace}/{kind}/{name}/apply` body `{resource_version, spec, confirm}` → direct command + `workload.apply` frame; both refuse managed 409, non-granted 422, missing capability 501, service tokens 403), `internal/api/exec_handlers.go` (`GET .../pods/{namespace}/{pod}/exec` WebSocket: `container.exec`, kubernetes runtime, `pod.exec` capability, first message `{csrf, spec:{pod:{namespace,pod,container,uid}, argv}, confirm: <pod name>, size}`; `OpenExecTarget` for pods matches the inventory pod by namespace/name, container present, phase Running, and the inventory-known UID — add `UID` to `protocol.Pod` (additive) in Task 1 if absent), `internal/api/server.go` routes, `authz_test.go` route lists, audit rows (`workload.<action>`, `pod.exec.open/close`, `workload.configuration.read`, `workload.apply` outcome) modelled on the Docker ones.
- [ ] Tests (api harness with a fake cluster agent): permission matrix per route; Kubernetes-only (a Docker endpoint gets 409 on workload routes; a cluster endpoint still gets 409 on Docker routes); granted-namespace refusal; capability 501; managed refusal on read/apply but not on restart/scale; scale without replicas → 400; delete without typed name → 400; the apply command settles from `deployment.result` and `?reference=` lists it; no secret value in audit/logs (sentinel); pod exec open/close audit and the UID mismatch refusal.
- [ ] **Commit** `api: workload commands, configuration, apply and pod exec routes`.

---

### Task 6: Web types, router and helpers

**Files:** `web/src/tenant.ts` (`WorkloadConfiguration`, `WorkloadRef`, `Pod.uid`, `Expectation.replicas`; `canOperate(role)` = OA/EA/operator, `canDestroy(role)` = OA/EA mirrors), `web/src/router.ts` (route `workload` at `/organizations/{org}/endpoints/{endpoint}/workloads/{namespace}/{kind}/{name}` with DNS-label grammar; `workloadPath(org, endpoint, ns, kind, name, tab?)`), `web/src/components/workloadConfiguration.ts` (`parseWorkloadConfiguration`, `toWorkloadSpec`, `diffWorkload`), tests.
- [ ] **Commit** `web: workload route, types and helpers`.

---

### Task 7: Web: workload page, toolbars, edit form, pod terminal

**Files:** `web/src/pages/WorkloadPage.tsx` (new; tabs Overview (kind, replicas desired/ready/updated, paused, images, pods table with uptime/node/restarts/containers), Configuration (`WorkloadConfigurationForm` for `canConfigure`, read-only summary otherwise), Logs (pod + container picker over `ContainerLogs` on the pod logs route), Terminal (pod + container picker → `PodTerminal`), Activity (`?reference=`)), `web/src/components/WorkloadControls.tsx` (icon group: Restart, Scale (prompt for replicas, bounded), Delete (typed name), Open), `web/src/components/PodControls.tsx` (Delete typed name, Logs link, Terminal link), `web/src/components/WorkloadConfigurationForm.tsx` (replicas, per-container image/command/args/env with masked literal values and reference rows read-only, resources; Save posts `apply` with `resource_version` and typed name; `unsupported` disables Save; managed read-only), `web/src/components/PodTerminal.tsx` (reuses `LiveTerminal`'s socket logic with the pod URL and spec; form asks container (select) and shell; typed pod name), `web/src/components/KubernetesCluster.tsx` (names link to the page; toolbars on workload and pod rows; pod logs stay a dialog there), `App.tsx`, `EndpointPage.tsx` (status line for cluster commands), `web/AGENTS.md` sentences, tests for each.
- Fixed texts: `forbidden` → "The agent's role does not allow this. Regenerate and apply the cluster manifest, then retry."; `conflict` → "The workload changed since you read it. Read again."; `namespace_not_granted` → "This namespace is not granted to KyYard on this cluster."; `application_managed` → "Managed by application X. Edit it there."
- [ ] **Commit** `web: workload page, actions, edit and pod terminal`.

---

### Task 8: Docs, DOX, dist

**Files:** `docs/agent-protocol.md` (Kubernetes runtime: workload commands, configuration read, apply frame, pod exec; §7 dependency sentence), `docs/authorization-matrix.md` (workload rows), `docs/threat-model.md` (row: workload writes and pod exec bounded by granted namespaces and SSAR; secrets by reference), `docs/application-schema.md` (managed workloads), `docs/ACCEPTANCE.md` (cluster steps; Known gaps: no manifest revision, DaemonSet scale, StatefulSet rollout wait), root `AGENTS.md` Verification (real-cluster tests), `internal/*/AGENTS.md`, `web/AGENTS.md`, README (the cluster section), `web/dist`; `make ci`.
- [ ] **Commit** `docs: Kubernetes parity contracts; rebuild web dist`.

## Self-review notes

- Spec 3.1 RBAC → Task 2 (no manifest revision; runtime SSAR + fixed text instead, recorded as a ruling). 3.2 actions → Tasks 1, 3, 5. 3.3 pod terminal → Tasks 1, 4, 5, 7. 3.4 UI → Tasks 6, 7. 3.5 tests → each task; real-cluster tests stay local (`KY_TEST_KUBECONFIG`).
- The spec's strategic merge patch became a full `Update` with the read's `resourceVersion` (the existing `upsert` pattern); the conflict semantics are the same.
- The spec's Overview "conditions" and pod IPs are added to the inventory only if cheap in Task 3's read (`WorkloadConfiguration` carries none); the page shows what the inventory has.
