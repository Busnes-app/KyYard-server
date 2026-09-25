# Kubernetes Enrollment and Inventory (M8 PR 20) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enroll a Kubernetes cluster as a KyYard endpoint by one generated manifest and one `kubectl apply`, show its inventory, health and pod logs read-only, and refuse every Docker-only action for it at the capability, route and UI level.

**Architecture:** The protocol gains a bounded `KubernetesInventory`, a `PodTarget` on log requests and pure runtime rules (`CapabilitiesFit`, `CheckRuntimeShape`, `ClusterHealth`) (Task 1). The agent client stores its identity through an `IdentityStore` and advertises cluster capabilities when `Options.Kubernetes` is set (Task 2). `internal/runtime/kubernetes` reads the cluster with the client-go typed clientset (paged `List`, pod log `Stream`) and keeps the identity in a Secret; no client-go type leaves it (Task 3). `cmd/agent --kubernetes` wires them (Task 4). A client-go-free `manifest` subpackage renders the install YAML the enrollment-token route returns (Task 5). The server refuses mismatched hellos and snapshots, answers `runtime_unsupported` on every Docker route, and derives `cluster_health` on read (Task 6). A pod log route reuses the container log stream (Task 7). The web gets the enrollment runtime switch and a read-only cluster view (Task 8). Documents close (Task 9).

**Tech Stack:** Go 1.26 (module `go 1.26.6`), `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` v0.37.1, SQLite + PostgreSQL 17, the coder/websocket agent protocol (fake agent sockets in tests), React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-25-kubernetes-enrollment-design.md`

## Global Constraints

- Work in the worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/k8s` (branch `feat/k8s-enrollment`). Every command below runs from its root.
- Dependencies, exactly: `k8s.io/client-go v0.37.1`, `k8s.io/api v0.37.1`, `k8s.io/apimachinery v0.37.1` (client-go v0.37.1 declares `go 1.26.0`, below the module's `go 1.26.6`; the `go` directive must not change). Added with `go get` in Task 3; `go mod tidy` after every task that adds an import.
- No Kubernetes SDK type leaves `internal/runtime/kubernetes`. `cmd/agent` calls only `kubernetes.InCluster`, `OwnNamespace`, `NewSecretIdentityStore`, `ErrIdentityConflict` and the `Client` methods; the server imports only `internal/runtime/kubernetes/manifest`, which imports no `k8s.io` package. Check: `go list -deps ./cmd/server | grep -c k8s.io` prints `0`.
- Real-cluster tests skip unless `KY_TEST_KUBECONFIG` is set (kind in CI is out of scope; run locally against a disposable cluster).
- Capabilities, exactly: `kubernetes.inventory`, `pod.logs`. Error codes, exactly: `capability_mismatch` (hello, then close with `protocol.CloseProtocol`), `snapshot_rejected` (session continues), `runtime_unsupported` (HTTP 409), `agent_image_unpinned` (HTTP 409), `unknown_container` (agent log refusal).
- Caps, exactly: `MaxNodes 500`, `MaxNamespaces 500`, `MaxWorkloads 2000`, `MaxPods 2000`, `MaxServices 2000`, `MaxClaims 1000`; names 253 bytes, images 1 KiB; `MaxSnapshotBytes` stays 1 MiB; List `Limit` 500.
- Manifest objects, names and settings are the spec's verbatim (namespace `kyyard-agent`; labels `app.kubernetes.io/name: kyyard-agent`, `app.kubernetes.io/managed-by: kyyard`; ClusterRole `kyyard-agent-read` get/list only on core `namespaces, nodes, pods, pods/log, events, services, persistentvolumeclaims` and apps `deployments, statefulsets, daemonsets`; Role `kyyard-agent-identity`; Secret `kyyard-agent-enrollment` key `link`; Deployment one replica, `Recreate`, args `--kubernetes --link-file /etc/kyyard/link --identity-secret kyyard-agent-identity --name <name> --docker-socket=`, requests 50m/64Mi, limits 500m/256Mi).
- Pod log route: `GET /api/organizations/{organization}/endpoints/{endpoint}/pods/{namespace}/{pod}/logs?container=&tail=&search=&follow=&download=&timestamps=`, `container.logs` permission, `pod.logs` capability, `log_streams.go` unchanged.
- Every store change is tested on SQLite and PostgreSQL. PostgreSQL runs use the local container `kyyard-access-pg`; read the password into a variable, never print it: `PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test ...`. Below this prefix is written `PG=… go test`.
- `gofmt -w` every edited Go file and check `gofmt -l cmd internal` prints nothing before each commit. `make tidy-check lint` runs after `git add` (it diffs the working tree against the index) and before `git commit`. `make ci` must pass before the branch is pushed (Task 9). Never run `make build-web` while `make ci` runs: both run `npm ci` in `web/`.
- `web/dist` is embedded and committed: Task 8 rebuilds it with `make build-web` and commits it with `web/tsconfig.tsbuildinfo`.
- DOX: the task that changes a directory's contract updates that directory's `AGENTS.md` (and a parent's Child DOX Index when a child is added) in the same commit.
- Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Gate each commit on the previous command's exit (`&&`).

Plan decisions where the code or the spec's silence forced a choice (each is reported to the reviewer; none changes a spec value):

1. **`agent_image_unpinned` "as today".** No such refusal exists today: an unpinned Docker enrollment answers 201 with a note. The Kubernetes path refuses before minting a token: 409 `agent_image_unpinned` without a pinned image, and 409 `https_required` when `KY_APP_URL` is not HTTPS (the enrollment link must be HTTPS), both after `CheckEnrollmentAccess` so a member who may not enroll learns neither. The Docker path is unchanged.
2. **Owner resolution without ReplicaSet access.** The spec's ClusterRole grants no `replicasets`, so the adapter never reads one: a pod whose controller is a ReplicaSet named `<deployment>-<pod-template-hash>` (the pod carries the same hash label) is reported as owned by that Deployment; any other controller is reported as is.
3. **Non-root with this image.** The published image runs as root, so `runAsNonRoot: true` alone would stop the pod. The pod sets `runAsUser`/`runAsGroup`/`fsGroup: 65532` and mounts the enrollment Secret with `defaultMode: 0440` and `optional: true`, so a pod restarted after the operator deletes the spent Secret starts; `--link-file` pointing at a missing file means "no link".
4. **Engine `name`.** `protocol.Engine` has no `name` field; the cluster snapshot sets `Engine.Runtime = "kubernetes"`, `Version` = the API server's `GitVersion`, `APIVersion` = `Major.Minor`, `OS`/`Arch` from `Platform`.
5. **Facts.** The store keeps only whitelisted fact keys; `runtime`, `server_version`, `node_count` and `platform` are added. A fact the agent cannot read is `"unknown"`.
6. **`--docker-socket` default.** Its default is non-empty, so `--kubernetes` requires `--docker-socket=` literally (the manifest passes it); the refusal reads `kubernetes and docker are exclusive: pass --docker-socket= with --kubernetes`.
7. **Identity Secret namespace.** `--identity-secret` names the Secret; the namespace is the pod's own, read from `/var/run/secrets/kubernetes.io/serviceaccount/namespace`.
8. **"Then fatal".** `SecretIdentityStore.Save` returns `ErrIdentityConflict` after the one retry, and also when the Secret already holds another endpoint's identity (never overwritten); `cmd/agent` wraps the store and exits on it, so the library never exits the process.
9. **Download filename.** The token response adds `manifest_file` (`kyyard-agent-<slug>.yaml`, slug = lower-case letters, digits, single hyphens; `endpoint` when empty); `command` is `kubectl apply -f <manifest_file>`.
10. **Nested caps.** Not in the spec: `MaxPodContainers 32`, `MaxWorkloadImages 32`, `MaxNodeRoles 16`, `MaxServicePorts 32`, short strings (phase, state, reason, kind, type, cluster IP, kubelet version, os, arch, capacity) 64 bytes. A pod's containers are bounded while decoding (a struct list is the one nested list a small document expands into a large value); cutting them marks `pods` truncated.
11. **Health with gaps.** `cluster_health` is `unknown` also when no node is reported (a forbidden node list must not read as healthy) and when the node list is truncated with every reported node Ready; any reported node not Ready is `degraded`.
12. **Where `runtime_unsupported` is decided.** `dockerOnly` reads the endpoint under `endpoint.read` (every role holds it) right after the path is parsed: commands, removal preview, inspection, container logs, exec (before the WebSocket upgrade), adoption preview and adoption (by the named endpoint). Plan and mapping resolve the instance's endpoint through `ReadApplicationInstance` before the store runs (the preflight reports a non-Docker inventory as `adoption_changed`); apply and application removal check the endpoint they already read, before the capability check. Container samples, rollups and the command list are reads and stay open.
13. **Pod log gaps.** A Docker endpoint on the pod route is 409 `runtime_unsupported`; a cluster agent without `pod.logs` is 501, like every other missing capability. The audited resource is `<endpoint>/pods/<namespace>/<pod>[/<container>]`.
14. **No client-go in the server.** Manifest rendering is `internal/runtime/kubernetes/manifest` (`text/template`, JSON-quoted scalars) with no `k8s.io` import; its tests decode every document strictly into client-go types.
15. **Pending hellos.** The capability rule applies to a pending endpoint's hello too (the capability set is still not stored while pending).
16. **Error payloads.** `capability_mismatch` and `snapshot_rejected` frames carry `{"code", "runtime"}`.
17. **Adapter errors.** The cluster `Snapshot` never returns an error (a failed list is reported empty and named); `Options.Kubernetes` makes the facts-only snapshot carry an empty cluster inventory so it still has the Kubernetes shape.
18. **UI tabs.** A Kubernetes endpoint shows `Cluster` and `Details` tabs; Containers, Projects, Images, Networks, Volumes and Activity are not rendered. The namespace filter falls back to all namespaces when the chosen one leaves the cluster.

## Review Focus

1. The operator deletes the spent enrollment Secret, as the manifest tells them, and the pod later restarts: the agent must come back from its identity Secret, not crash on the missing link file or enroll again. Pinned in Task 4 (`TestReadLinkFile`: a missing file is no link) and Task 5 (`TestManifestCarriesTheTokenOnceAndALockedDownAgent`: the Secret volume is optional).
2. An administrator tightens RBAC or a list call fails: the rest of the cluster must still be reported, and a missing node list must not read as a healthy cluster. Pinned in Task 3 (`TestSnapshotCutsAndSurvivesAForbiddenList`) and Task 1 (`TestClusterHealth`, "no node reported").
3. Someone scales the agent Deployment to two replicas, or re-applies a manifest over another endpoint's identity: the store must never overwrite another endpoint's identity, and two writers racing must stop rather than alternate. Pinned in Task 3 (`TestSecretIdentityStoreConflicts`).
4. A cluster named with YAML- or shell-significant characters (`prod "east": #1`, `$(rm -rf /)`): the manifest must stay valid with the name intact in `--name`, and the filename in the kubectl command must be inert. Pinned in Task 5 (`TestManifestCarriesTheTokenOnceAndALockedDownAgent`, `TestFileName`).
5. A pod runs a sidecar and the operator clicks Logs without naming a container on the API: the agent must refuse with `unknown_container` and the reader must see that reason, not an empty log. Pinned in Task 3 (`TestLogsRefuseAnUnnamedOrUnknownContainer`) and Task 7 (`TestPodLogsStreamFromTheClusterToTheReader`).

## File map

| Path | Task | Responsibility |
|---|---|---|
| `internal/agent/protocol/kubernetes.go` | 1 | Cluster inventory types, caps, bounded decode, clamp/shrink helpers, `PodTarget`, runtime rules, `ClusterHealth` |
| `internal/agent/protocol/inventory.go`, `logs.go` | 1 | `Snapshot.Kubernetes`, decode/Clamp/Shrink hooks, `LogRequest.Pod` |
| `internal/agent/client/identity.go`, `enroll.go`, `connect.go` | 2 | `IdentityStore`, `DirStore`, `DecodeIdentity`; `Enroll` takes a store and facts; `Options.Identities`, `Options.Kubernetes` |
| `internal/runtime/kubernetes/{kubernetes,logs,identity}.go` | 3 | client-go adapter: `Snapshot`, `Facts`, `Logs`, `SecretIdentityStore` |
| `cmd/agent/main.go` | 4 | `--kubernetes`, `--link-file`, `--identity-secret` |
| `internal/runtime/kubernetes/manifest/manifest.go` | 5 | Install manifest rendering, no `k8s.io` import |
| `internal/api/endpoint_handlers.go` | 5 | Kubernetes enrollment tokens |
| `internal/api/runtime_gate.go`, `agent_connect.go`, handlers | 6 | `dockerOnly`, hello and snapshot rules |
| `internal/store/endpoints.go`, `models.go` | 2, 5, 6 | fact keys, `ValidEndpointName`, `cluster_health` |
| `internal/api/log_handlers.go`, `internal/store/logs.go` | 7 | Pod log route over the shared stream |
| `web/src/components/{KubernetesCluster,ResourceTable}.tsx`, `Endpoints.tsx`, `pages/EndpointPage.tsx` | 8 | Enrollment switch, cluster view |

---

### Task 1: Protocol — cluster inventory, pod targets and the runtime rules

**Files:**
- Create: `internal/agent/protocol/kubernetes.go`
- Test: `internal/agent/protocol/kubernetes_test.go`
- Modify: `internal/agent/protocol/inventory.go` (`Snapshot`, `UnmarshalSnapshotBounded`, `Clamp`, `Shrink`)
- Modify: `internal/agent/protocol/logs.go` (`LogRequest.Pod`)
- Docs: `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: `decodeBounded`, `CleanText`, `cleanList`, `ValidContainerID` (existing, package `protocol`).
- Produces (package `protocol`):
  ```go
  const RuntimeDocker, RuntimeKubernetes = "docker", "kubernetes"
  const CapabilityKubernetesInventory = "kubernetes.inventory"
  const CapabilityPodLogs = "pod.logs"
  const MaxNodes, MaxNamespaces, MaxWorkloads, MaxPods, MaxServices, MaxClaims = 500, 500, 2000, 2000, 2000, 1000
  const MaxPodContainers, MaxWorkloadImages, MaxNodeRoles, MaxServicePorts = 32, 32, 16, 32
  const MaxKubeNameBytes, MaxKubeImageBytes, MaxKubeShortBytes = 253, 1 << 10, 64
  const HealthHealthy, HealthDegraded, HealthUnknown = "healthy", "degraded", "unknown"
  type KubernetesInventory struct{ Nodes []Node; Namespaces []string; Workloads []Workload; Pods []Pod; Services []Service; Claims []Claim }
  type Node, Workload, Pod, PodContainer, Service, Claim // fields as in the spec, snake_case JSON
  type PodTarget struct{ Namespace, Name, Container string } // json namespace, name, container,omitempty
  func (p PodTarget) Validate() error
  func (r LogRequest) ValidateFor(runtime string) error
  var ErrSnapshotShape error
  func CheckRuntimeShape(runtime string, s *Snapshot) error
  func CapabilitiesFit(runtime string, capabilities []string) bool
  func ClusterHealth(active bool, s *Snapshot) string
  func ValidDNSLabel(s string) bool
  func ValidDNSSubdomain(s string) bool
  // Snapshot gains:  Kubernetes *KubernetesInventory `json:"kubernetes,omitempty"`
  // LogRequest gains: Pod *PodTarget `json:"pod,omitempty"`
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/protocol/kubernetes_test.go`:

```go
package protocol

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Cluster lists decode one element at a time up to their caps, a pod's containers too, and a
// cut list is named; an absent or null inventory stays nil.
func TestKubernetesSnapshotDecodesBounded(t *testing.T) {
	pods := make([]string, 0, MaxPods+3)
	many := strings.TrimSuffix(strings.Repeat(`{"name":"c"},`, MaxPodContainers+5), ",")
	pods = append(pods, `{"namespace":"shop","name":"web","containers":[`+many+`]}`)
	for i := 1; i < MaxPods+3; i++ {
		pods = append(pods, fmt.Sprintf(`{"namespace":"shop","name":"p%d"}`, i))
	}
	raw := []byte(`{"generation":1,"kubernetes":{"nodes":[{"name":"n1","ready":true}],"pods":[` + strings.Join(pods, ",") + `]}}`)
	var s Snapshot
	if err := UnmarshalSnapshotBounded(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Kubernetes == nil || len(s.Kubernetes.Pods) != MaxPods || len(s.Kubernetes.Pods[0].Containers) != MaxPodContainers || !slices.Contains(s.Truncated, "pods") {
		t.Fatalf("decoded %+v truncated %v", s.Kubernetes != nil, s.Truncated)
	}
	if len(s.Kubernetes.Nodes) != 1 || !s.Kubernetes.Nodes[0].Ready || s.Kubernetes.Nodes[0].Name != "n1" {
		t.Fatalf("nodes %+v", s.Kubernetes.Nodes)
	}
	for _, doc := range []string{`{"generation":1}`, `{"generation":1,"kubernetes":null}`} {
		var docker Snapshot
		if err := UnmarshalSnapshotBounded([]byte(doc), &docker); err != nil || docker.Kubernetes != nil {
			t.Fatalf("%s: %+v %v", doc, docker.Kubernetes, err)
		}
	}
	var bad Snapshot
	if err := UnmarshalSnapshotBounded([]byte(`{"kubernetes":{"pods":{}}}`), &bad); err == nil {
		t.Fatal("a pods object where a list belongs decoded")
	}
}

// A cluster inventory many times the shared byte limit fits it after Clamp and Shrink, text is
// cleaned and cut, the adapter's own truncation names survive, and no list is left nil.
func TestClampAndShrinkKubernetes(t *testing.T) {
	image := strings.Repeat("i", MaxKubeImageBytes+100)
	k := &KubernetesInventory{}
	for i := 0; i < MaxPods; i++ {
		p := Pod{Namespace: "shop", Name: fmt.Sprintf("web-%d", i), Phase: "Running"}
		for range 4 {
			p.Containers = append(p.Containers, PodContainer{Name: "c", Image: image, ImageID: image, State: "running"})
		}
		k.Pods = append(k.Pods, p)
	}
	k.Workloads = []Workload{{Kind: "Deployment", Namespace: "shop", Name: "web‮evil\nline", Images: []string{image}}}
	s := &Snapshot{Kubernetes: k, Truncated: []string{"services"}}
	Clamp(s)
	if k.Workloads[0].Name != "webevilline" || len(k.Workloads[0].Images[0]) != MaxKubeImageBytes || k.Nodes == nil || k.Services == nil || k.Claims == nil || k.Namespaces == nil {
		t.Fatalf("clamp: %+v", k.Workloads[0])
	}
	if !slices.Contains(s.Truncated, "services") {
		t.Fatalf("clamp dropped the adapter's truncation: %v", s.Truncated)
	}
	raw := Shrink(s)
	if len(raw) > MaxSnapshotBytes || !slices.Contains(s.Truncated, "pods") || !slices.Contains(s.Truncated, "services") {
		t.Fatalf("shrunk to %d bytes, truncated %v", len(raw), s.Truncated)
	}
	var back Snapshot
	if err := UnmarshalSnapshotBounded(raw, &back); err != nil || back.Kubernetes == nil || len(back.Kubernetes.Pods) == 0 {
		t.Fatalf("round trip: %v", err)
	}
	// A pod with more containers than the cap is cut and marks the pod list incomplete.
	wide := &Snapshot{Kubernetes: &KubernetesInventory{Pods: []Pod{{Name: "p", Containers: make([]PodContainer, MaxPodContainers+1)}}}}
	Clamp(wide)
	if len(wide.Kubernetes.Pods[0].Containers) != MaxPodContainers || !slices.Contains(wide.Truncated, "pods") {
		t.Fatalf("wide pod: %d %v", len(wide.Kubernetes.Pods[0].Containers), wide.Truncated)
	}
}

func TestPodTargetValidation(t *testing.T) {
	for target, ok := range map[PodTarget]bool{
		{Namespace: "shop", Name: "web-7c9d-x2"}:                        true,
		{Namespace: "shop", Name: "web.v2", Container: "nginx"}:         true,
		{Namespace: "shop", Name: strings.Repeat("a", 253)}:             true,
		{Namespace: "shop", Name: strings.Repeat("a", 254)}:             false,
		{Namespace: strings.Repeat("a", 64), Name: "web"}:               false,
		{Namespace: "Shop", Name: "web"}:                                false,
		{Namespace: "shop", Name: "../secrets"}:                         false,
		{Namespace: "shop", Name: "web", Container: "a.b"}:              false,
		{Namespace: "", Name: "web"}:                                    false,
		{Namespace: "shop", Name: "web", Container: "-x"}:               false,
		{Namespace: "kube-system", Name: "coredns-1", Container: "dns"}: true,
	} {
		if (target.Validate() == nil) != ok {
			t.Errorf("%+v: want ok=%v", target, ok)
		}
	}
}

func TestLogRequestValidateFor(t *testing.T) {
	pod := &PodTarget{Namespace: "shop", Name: "web"}
	id := strings.Repeat("a", 64)
	for _, tc := range []struct {
		runtime string
		req     LogRequest
		ok      bool
	}{
		{RuntimeKubernetes, LogRequest{Pod: pod}, true},
		{RuntimeKubernetes, LogRequest{Pod: pod, Container: id}, false},
		{RuntimeKubernetes, LogRequest{Container: id}, false},
		{RuntimeKubernetes, LogRequest{Pod: &PodTarget{Namespace: "shop", Name: "Web"}}, false},
		{RuntimeDocker, LogRequest{Container: id}, true},
		{RuntimeDocker, LogRequest{Container: id, Pod: pod}, false},
		{RuntimeDocker, LogRequest{Pod: pod}, false},
	} {
		if (tc.req.ValidateFor(tc.runtime) == nil) != tc.ok {
			t.Errorf("%s %+v: want ok=%v", tc.runtime, tc.req, tc.ok)
		}
	}
	// A Docker request's wire form is unchanged: no pod key.
	raw, _ := json.Marshal(LogRequest{Stream: "s", Container: id})
	if strings.Contains(string(raw), "pod") {
		t.Fatalf("docker request grew a pod key: %s", raw)
	}
}

func TestCapabilitiesFitTheRuntime(t *testing.T) {
	for _, tc := range []struct {
		runtime string
		caps    []string
		ok      bool
	}{
		{RuntimeKubernetes, []string{CapabilityKubernetesInventory, CapabilityPodLogs}, true},
		{RuntimeKubernetes, []string{}, true},
		{RuntimeKubernetes, []string{CapabilityKubernetesInventory, CapabilityContainerInspect}, false},
		{RuntimeKubernetes, []string{"container.exec"}, false},
		{RuntimeKubernetes, []string{"kubernetes.write"}, false},
		{RuntimeDocker, []string{CapabilityContainerInspect, CapabilityDeploymentApply, "container.exec"}, true},
		{RuntimeDocker, []string{CapabilityPodLogs}, false},
		{RuntimeDocker, []string{CapabilityDeploymentApply, CapabilityKubernetesInventory}, false},
	} {
		if CapabilitiesFit(tc.runtime, tc.caps) != tc.ok {
			t.Errorf("%s %v: want %v", tc.runtime, tc.caps, tc.ok)
		}
	}
	if CapabilityKubernetesInventory != "kubernetes.inventory" || CapabilityPodLogs != "pod.logs" {
		t.Fatal("the wire vocabulary changed")
	}
}

func TestCheckRuntimeShape(t *testing.T) {
	cluster := &KubernetesInventory{}
	for _, tc := range []struct {
		runtime string
		s       Snapshot
		ok      bool
	}{
		{RuntimeKubernetes, Snapshot{Kubernetes: cluster, Containers: []Container{}, Images: []Image{}}, true},
		{RuntimeKubernetes, Snapshot{}, false},
		{RuntimeKubernetes, Snapshot{Kubernetes: cluster, Containers: []Container{{ID: "c"}}}, false},
		{RuntimeKubernetes, Snapshot{Kubernetes: cluster, Volumes: []Volume{{Name: "v"}}}, false},
		{RuntimeDocker, Snapshot{Containers: []Container{{ID: "c"}}}, true},
		{RuntimeDocker, Snapshot{Kubernetes: cluster}, false},
	} {
		if (CheckRuntimeShape(tc.runtime, &tc.s) == nil) != tc.ok {
			t.Errorf("%s %+v: want ok=%v", tc.runtime, tc.s, tc.ok)
		}
	}
}

func TestClusterHealth(t *testing.T) {
	ready, notReady := Node{Name: "a", Ready: true}, Node{Name: "b"}
	snap := func(truncated []string, nodes ...Node) *Snapshot {
		return &Snapshot{ObservedAt: time.Now(), Kubernetes: &KubernetesInventory{Nodes: nodes}, Truncated: truncated}
	}
	for _, tc := range []struct {
		name   string
		active bool
		s      *Snapshot
		want   string
	}{
		{"every node ready", true, snap(nil, ready, ready), HealthHealthy},
		{"one node not ready", true, snap(nil, ready, notReady), HealthDegraded},
		{"a not-ready node in a partial list", true, snap([]string{"nodes"}, notReady), HealthDegraded},
		{"a partial list of ready nodes", true, snap([]string{"nodes"}, ready), HealthUnknown},
		{"no node reported", true, snap([]string{"nodes"}), HealthUnknown},
		{"offline", false, snap(nil, ready), HealthUnknown},
		{"no inventory", true, nil, HealthUnknown},
		{"docker snapshot", true, &Snapshot{}, HealthUnknown},
	} {
		if got := ClusterHealth(tc.active, tc.s); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/agent/protocol/`
Expected: FAIL to compile (`undefined: KubernetesInventory`, `undefined: PodTarget`, `undefined: CapabilitiesFit`, ...).

- [ ] **Step 3: Implement the cluster types and rules**

Create `internal/agent/protocol/kubernetes.go`:

```go
package protocol

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"
)

// Runtimes an endpoint can have. The endpoint row fixes one at enrollment; nothing changes it.
const (
	RuntimeDocker     = "docker"
	RuntimeKubernetes = "kubernetes"
)

// Kubernetes capabilities. A cluster agent advertises these and nothing else; a Docker agent
// never advertises them (CapabilitiesFit).
const (
	CapabilityKubernetesInventory = "kubernetes.inventory"
	CapabilityPodLogs             = "pod.logs"
)

// Cluster inventory bounds. Lists are cut at their cap and named in Truncated; nested lists are
// cut silently by Clamp, except a pod's containers, which also mark "pods".
const (
	MaxNodes          = 500
	MaxNamespaces     = 500
	MaxWorkloads      = 2000
	MaxPods           = 2000
	MaxServices       = 2000
	MaxClaims         = 1000
	MaxPodContainers  = 32
	MaxWorkloadImages = 32
	MaxNodeRoles      = 16
	MaxServicePorts   = 32
	MaxKubeNameBytes  = 253
	MaxKubeImageBytes = 1 << 10
	MaxKubeShortBytes = 64
)

// KubernetesInventory is what a cluster agent reports in place of the Docker lists.
type KubernetesInventory struct {
	Nodes      []Node     `json:"nodes"`
	Namespaces []string   `json:"namespaces"`
	Workloads  []Workload `json:"workloads"`
	Pods       []Pod      `json:"pods"`
	Services   []Service  `json:"services"`
	Claims     []Claim    `json:"claims"`
}

type Node struct {
	Name           string   `json:"name"`
	KubeletVersion string   `json:"kubelet_version"`
	OS             string   `json:"os"`
	Arch           string   `json:"arch"`
	Ready          bool     `json:"ready"`
	Roles          []string `json:"roles"`
	Unschedulable  bool     `json:"unschedulable"`
}

// Workload is a Deployment, StatefulSet or DaemonSet.
type Workload struct {
	Kind      string   `json:"kind"`
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Desired   int32    `json:"desired"`
	Ready     int32    `json:"ready"`
	Updated   int32    `json:"updated"`
	Images    []string `json:"images"`
	Paused    bool     `json:"paused"`
}

// Pod carries its Kubernetes phase and its controller: a Deployment's pod names the
// Deployment, not the ReplicaSet between them.
type Pod struct {
	Namespace  string         `json:"namespace"`
	Name       string         `json:"name"`
	Phase      string         `json:"phase"`
	Node       string         `json:"node"`
	OwnerKind  string         `json:"owner_kind"`
	OwnerName  string         `json:"owner_name"`
	StartedAt  time.Time      `json:"started_at"`
	Containers []PodContainer `json:"containers"`
}

// PodContainer State is running, waiting or terminated; Reason is the runtime's word for why.
type PodContainer struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	ImageID      string `json:"image_id"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restart_count"`
}

type Service struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	ClusterIP string   `json:"cluster_ip"`
	Ports     []string `json:"ports"`
}

// Claim is a PersistentVolumeClaim.
type Claim struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Phase        string `json:"phase"`
	StorageClass string `json:"storage_class"`
	Capacity     string `json:"capacity"`
}

// UnmarshalJSON caps a pod's containers while decoding: of the nested lists it is the one a
// small document can expand into a large value.
func (p *Pod) UnmarshalJSON(data []byte) error {
	type plain Pod
	var aux struct {
		plain
		Containers json.RawMessage `json:"containers"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*p = Pod(aux.plain)
	if len(aux.Containers) == 0 {
		return nil
	}
	containers, _, err := decodeBounded[PodContainer](aux.Containers, MaxPodContainers)
	p.Containers = containers
	return err
}

func decodeKubernetes(raw []byte) (*KubernetesInventory, []string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	k := &KubernetesInventory{}
	var cut []string
	err := errors.Join(
		decodeField(fields, "nodes", MaxNodes, &k.Nodes, &cut),
		decodeField(fields, "namespaces", MaxNamespaces, &k.Namespaces, &cut),
		decodeField(fields, "workloads", MaxWorkloads, &k.Workloads, &cut),
		decodeField(fields, "pods", MaxPods, &k.Pods, &cut),
		decodeField(fields, "services", MaxServices, &k.Services, &cut),
		decodeField(fields, "claims", MaxClaims, &k.Claims, &cut),
	)
	if err != nil {
		return nil, nil, err
	}
	return k, cut, nil
}

func decodeField[T any](fields map[string]json.RawMessage, name string, max int, dst *[]T, cut *[]string) error {
	raw, ok := fields[name]
	if !ok {
		return nil
	}
	items, over, err := decodeBounded[T](raw, max)
	if err != nil {
		return err
	}
	*dst = items
	if over {
		*cut = append(*cut, name)
	}
	return nil
}

// clampKubernetes is Clamp for the cluster lists: caps, text safety, no nil lists.
func clampKubernetes(k *KubernetesInventory, truncated map[string]bool) {
	short := func(s string) string { return CleanText(s, MaxKubeShortBytes) }
	name := func(s string) string { return CleanText(s, MaxKubeNameBytes) }
	if len(k.Nodes) > MaxNodes {
		k.Nodes, truncated["nodes"] = k.Nodes[:MaxNodes], true
	}
	for i := range k.Nodes {
		n := &k.Nodes[i]
		n.Name, n.KubeletVersion, n.OS, n.Arch = name(n.Name), short(n.KubeletVersion), short(n.OS), short(n.Arch)
		n.Roles = cleanList(n.Roles, MaxNodeRoles, MaxKubeShortBytes)
	}
	if len(k.Namespaces) > MaxNamespaces {
		k.Namespaces, truncated["namespaces"] = k.Namespaces[:MaxNamespaces], true
	}
	k.Namespaces = cleanList(k.Namespaces, MaxNamespaces, MaxKubeNameBytes)
	if len(k.Workloads) > MaxWorkloads {
		k.Workloads, truncated["workloads"] = k.Workloads[:MaxWorkloads], true
	}
	for i := range k.Workloads {
		w := &k.Workloads[i]
		w.Kind, w.Namespace, w.Name = short(w.Kind), name(w.Namespace), name(w.Name)
		w.Images = cleanList(w.Images, MaxWorkloadImages, MaxKubeImageBytes)
	}
	if len(k.Pods) > MaxPods {
		k.Pods, truncated["pods"] = k.Pods[:MaxPods], true
	}
	for i := range k.Pods {
		p := &k.Pods[i]
		p.Namespace, p.Name, p.Phase, p.Node = name(p.Namespace), name(p.Name), short(p.Phase), name(p.Node)
		p.OwnerKind, p.OwnerName = short(p.OwnerKind), name(p.OwnerName)
		if len(p.Containers) > MaxPodContainers {
			p.Containers, truncated["pods"] = p.Containers[:MaxPodContainers], true
		}
		for j := range p.Containers {
			c := &p.Containers[j]
			c.Name, c.Image, c.ImageID = name(c.Name), CleanText(c.Image, MaxKubeImageBytes), CleanText(c.ImageID, MaxKubeImageBytes)
			c.State, c.Reason = short(c.State), short(c.Reason)
		}
		if p.Containers == nil {
			p.Containers = []PodContainer{}
		}
	}
	if len(k.Services) > MaxServices {
		k.Services, truncated["services"] = k.Services[:MaxServices], true
	}
	for i := range k.Services {
		s := &k.Services[i]
		s.Namespace, s.Name, s.Type, s.ClusterIP = name(s.Namespace), name(s.Name), short(s.Type), short(s.ClusterIP)
		s.Ports = cleanList(s.Ports, MaxServicePorts, MaxKubeShortBytes)
	}
	if len(k.Claims) > MaxClaims {
		k.Claims, truncated["claims"] = k.Claims[:MaxClaims], true
	}
	for i := range k.Claims {
		c := &k.Claims[i]
		c.Namespace, c.Name, c.Phase, c.StorageClass, c.Capacity = name(c.Namespace), name(c.Name), short(c.Phase), name(c.StorageClass), short(c.Capacity)
	}
	if k.Nodes == nil {
		k.Nodes = []Node{}
	}
	if k.Workloads == nil {
		k.Workloads = []Workload{}
	}
	if k.Pods == nil {
		k.Pods = []Pod{}
	}
	if k.Services == nil {
		k.Services = []Service{}
	}
	if k.Claims == nil {
		k.Claims = []Claim{}
	}
}

// shrinkKubernetes drops the tail of the longest cluster list and names it; false when every
// list is already empty.
func shrinkKubernetes(k *KubernetesInventory, truncated map[string]bool) bool {
	lists := []struct {
		name string
		n    int
		cut  func()
	}{
		{"pods", len(k.Pods), func() { k.Pods = k.Pods[:len(k.Pods)*3/4] }},
		{"workloads", len(k.Workloads), func() { k.Workloads = k.Workloads[:len(k.Workloads)*3/4] }},
		{"services", len(k.Services), func() { k.Services = k.Services[:len(k.Services)*3/4] }},
		{"claims", len(k.Claims), func() { k.Claims = k.Claims[:len(k.Claims)*3/4] }},
		{"namespaces", len(k.Namespaces), func() { k.Namespaces = k.Namespaces[:len(k.Namespaces)*3/4] }},
		{"nodes", len(k.Nodes), func() { k.Nodes = k.Nodes[:len(k.Nodes)*3/4] }},
	}
	best := -1
	for i, l := range lists {
		if l.n > 0 && (best < 0 || l.n > lists[best].n) {
			best = i
		}
	}
	if best < 0 {
		return false
	}
	lists[best].cut()
	truncated[lists[best].name] = true
	return true
}

// ErrSnapshotShape is a snapshot whose lists do not match the endpoint's runtime.
var ErrSnapshotShape = errors.New("the snapshot does not match the endpoint's runtime")

// CheckRuntimeShape holds a Kubernetes endpoint to a cluster inventory and no Docker lists,
// and a Docker endpoint to no cluster inventory.
func CheckRuntimeShape(runtime string, s *Snapshot) error {
	if runtime == RuntimeKubernetes {
		if s.Kubernetes == nil || len(s.Containers)+len(s.Images)+len(s.Networks)+len(s.Volumes) > 0 {
			return ErrSnapshotShape
		}
		return nil
	}
	if s.Kubernetes != nil {
		return ErrSnapshotShape
	}
	return nil
}

// kubernetesCapabilities is everything a cluster agent may advertise.
var kubernetesCapabilities = map[string]bool{CapabilityKubernetesInventory: true, CapabilityPodLogs: true}

// CapabilitiesFit reports whether a hello's capabilities belong to the endpoint's runtime:
// a cluster agent names only cluster capabilities, a Docker agent names none of them.
func CapabilitiesFit(runtime string, capabilities []string) bool {
	for _, c := range capabilities {
		if (runtime == RuntimeKubernetes) != kubernetesCapabilities[c] {
			return false
		}
	}
	return true
}

// Cluster health, derived on read from the stored inventory.
const (
	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
	HealthUnknown  = "unknown"
)

// ClusterHealth is degraded when a reported node is not Ready, healthy when every node is
// reported and Ready, and unknown when the endpoint is not active, has no cluster inventory,
// reports no node, or reports only part of its nodes.
func ClusterHealth(active bool, s *Snapshot) string {
	if !active || s == nil || s.Kubernetes == nil || len(s.Kubernetes.Nodes) == 0 {
		return HealthUnknown
	}
	for _, n := range s.Kubernetes.Nodes {
		if !n.Ready {
			return HealthDegraded
		}
	}
	if slices.Contains(s.Truncated, "nodes") {
		return HealthUnknown
	}
	return HealthHealthy
}

var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// ValidDNSLabel is RFC 1123 label syntax as Kubernetes applies it: namespaces, containers.
func ValidDNSLabel(s string) bool { return dnsLabel.MatchString(s) }

// ValidDNSSubdomain is RFC 1123 subdomain syntax as Kubernetes applies it: pod names.
func ValidDNSSubdomain(s string) bool { return len(s) <= 253 && dnsSubdomain.MatchString(s) }

// PodTarget names one pod and, optionally, one of its containers. An empty Container asks the
// agent to take the pod's only container; it refuses when there are several.
type PodTarget struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Container string `json:"container,omitempty"`
}

func (p PodTarget) Validate() error {
	if !ValidDNSLabel(p.Namespace) || !ValidDNSSubdomain(p.Name) || (p.Container != "" && !ValidDNSLabel(p.Container)) {
		return errors.New("a pod target names a namespace, a pod and optionally a container, in Kubernetes name syntax")
	}
	return nil
}

// ValidateFor holds a log request to the one target the runtime reads: a container ID for
// Docker, a pod for Kubernetes, never both.
func (r LogRequest) ValidateFor(runtime string) error {
	if runtime == RuntimeKubernetes {
		if r.Container != "" || r.Pod == nil {
			return errors.New("a Kubernetes log request names a pod and no container ID")
		}
		return r.Pod.Validate()
	}
	if r.Pod != nil || !ValidContainerID(r.Container) {
		return errors.New("a Docker log request names a container ID and no pod")
	}
	return nil
}
```

- [ ] **Step 4: Hook the cluster inventory into the snapshot**

In `internal/agent/protocol/inventory.go`, in `type Snapshot`, replace

```go
	Volumes    []Volume    `json:"volumes"`
	// Truncated names the lists that hit their cap; the UI shows the gap.
```

with

```go
	Volumes    []Volume    `json:"volumes"`
	// Kubernetes is a cluster agent's inventory; nil from a Docker agent (CheckRuntimeShape).
	Kubernetes *KubernetesInventory `json:"kubernetes,omitempty"`
	// Truncated names the lists that hit their cap; the UI shows the gap.
```

At the end of `UnmarshalSnapshotBounded`, replace

```go
		if cut {
			s.Truncated = append(s.Truncated, "volumes")
		}
	}
	return nil
}
```

with

```go
		if cut {
			s.Truncated = append(s.Truncated, "volumes")
		}
	}
	if raw, ok := fields["kubernetes"]; ok && string(bytes.TrimSpace(raw)) != "null" {
		k, cut, err := decodeKubernetes(raw)
		if err != nil {
			return err
		}
		s.Kubernetes = k
		s.Truncated = append(s.Truncated, cut...)
	}
	return nil
}
```

At the end of `Clamp`, replace

```go
	s.Truncated = nil
	for _, name := range []string{"containers", "images", "networks", "volumes", "labels"} {
		if truncated[name] {
			s.Truncated = append(s.Truncated, name)
		}
	}
}
```

with

```go
	if s.Kubernetes != nil {
		clampKubernetes(s.Kubernetes, truncated)
	}
	s.Truncated = nil
	for _, name := range truncatable {
		if truncated[name] {
			s.Truncated = append(s.Truncated, name)
		}
	}
}

// truncatable is every name Truncated may carry, in the order it is reported.
var truncatable = []string{"containers", "images", "networks", "volumes", "labels", "nodes", "namespaces", "workloads", "pods", "services", "claims"}
```

In `Shrink`, replace

```go
		default:
			raw, _ := json.Marshal(s)
			return raw
		}
		s.Truncated = nil
		for _, name := range []string{"containers", "images", "networks", "volumes", "labels"} {
```

with

```go
		// A cluster snapshot has no Docker lists; its own longest list is cut instead.
		case s.Kubernetes != nil && shrinkKubernetes(s.Kubernetes, truncated):
		default:
			raw, _ := json.Marshal(s)
			return raw
		}
		s.Truncated = nil
		for _, name := range truncatable {
```

In `internal/agent/protocol/logs.go`, replace

```go
// LogRequest opens one stream. Container is the ID the control plane resolved, never a name:
// a name is a label the runtime reassigns, and a stream must not follow it to another
// container mid-read.
type LogRequest struct {
	Stream     string    `json:"stream"`
	Container  string    `json:"container"`
```

with

```go
// LogRequest opens one stream. Container is the ID the control plane resolved, never a name:
// a name is a label the runtime reassigns, and a stream must not follow it to another
// container mid-read. A Kubernetes endpoint is asked for a Pod instead (ValidateFor).
type LogRequest struct {
	Stream     string     `json:"stream"`
	Container  string     `json:"container"`
	Pod        *PodTarget `json:"pod,omitempty"`
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `gofmt -w internal/agent/protocol && go vet ./internal/agent/... && go test -race -count=1 ./internal/agent/protocol/`
Expected: PASS, including the existing inventory and bounded-decode tests.

- [ ] **Step 6: DOX and commit**

`internal/agent/AGENTS.md`, `## Local Contracts`, append:

```markdown
- Runtimes are `protocol.RuntimeDocker` and `protocol.RuntimeKubernetes`, fixed per endpoint at enrollment. A cluster agent advertises only `kubernetes.inventory` and `pod.logs` (`CapabilitiesFit`), and its snapshot carries `Snapshot.Kubernetes` (`KubernetesInventory`: nodes, namespaces, workloads, pods, services, claims, capped 500/500/2000/2000/2000/1000, names 253 bytes, images 1 KiB, a pod's containers 32 and bounded while decoding) with every Docker list empty (`CheckRuntimeShape`); a Docker snapshot carries none. `Clamp` and `Shrink` treat the cluster lists like the Docker ones and name cuts in `Truncated`. `ClusterHealth` is `degraded` when a reported node is not Ready, `healthy` when every node is reported and Ready, else `unknown`. `LogRequest.Pod` (`PodTarget`: namespace DNS-1123 label, pod DNS-1123 subdomain, optional container label) and `Container` are exclusive (`ValidateFor`).
```

```bash
git add internal/agent/protocol internal/agent/AGENTS.md && make tidy-check lint && git commit -m "feat(protocol): Kubernetes inventory, pod log targets and runtime rules" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Agent client — identity stores, cluster facts and cluster capabilities

**Files:**
- Modify: `internal/agent/client/identity.go` (`IdentityStore`, `DirStore`, `DecodeIdentity`)
- Modify: `internal/agent/client/enroll.go` (`Enroll` takes a store and facts)
- Modify: `internal/agent/client/connect.go` (`Options.Identities`, `Options.Kubernetes`, capabilities, facts-only snapshot, `save`)
- Modify: `internal/agent/client/client_test.go` (six `Enroll` call sites)
- Modify: `cmd/agent/main.go` (the one `Enroll` call, so the build stays green until Task 4)
- Modify: `internal/store/endpoints.go` (`factKeys`)
- Test: `internal/agent/client/kubernetes_test.go`
- Docs: `internal/agent/AGENTS.md`, `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.CapabilityKubernetesInventory`, `protocol.CapabilityPodLogs`, `protocol.KubernetesInventory`, `protocol.RuntimeKubernetes` (Task 1).
- Produces (package `client`):
  ```go
  type IdentityStore interface {
  	Load() (*Identity, error) // nil, nil when never enrolled
  	Save(*Identity) error
  }
  type DirStore string // Load/Save = LoadIdentity/SaveIdentity on the directory
  func DecodeIdentity(raw []byte) (*Identity, error)
  func Enroll(ctx context.Context, httpClient *http.Client, server string, store IdentityStore, name, tokenB64 string, facts map[string]string) (*Identity, error)
  // Options gains:
  Identities IdentityStore // nil falls back to IdentityDir
  Kubernetes bool          // advertise kubernetes.inventory (+ pod.logs with Logs); facts-only snapshot carries an empty cluster inventory
  ```
  Store: enrollment facts keep `runtime`, `server_version`, `node_count`, `platform` besides the Docker keys.

- [ ] **Step 1: Write the failing test**

Create `internal/agent/client/kubernetes_test.go`:

```go
package client_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

// memIdentities is an IdentityStore in memory, standing in for the cluster's Secret.
type memIdentities struct {
	mu    sync.Mutex
	id    *client.Identity
	saves int
}

func (m *memIdentities) Load() (*client.Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.id == nil {
		return nil, nil
	}
	c := *m.id
	return &c, nil
}

func (m *memIdentities) Save(id *client.Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *id
	m.id, m.saves = &c, m.saves+1
	return nil
}

// A cluster agent enrolls through its identity store with cluster facts, advertises only the
// cluster capabilities, and its inventory arrives with the cluster lists.
func TestClusterAgentEnrollsAndReportsTheCluster(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dbCfg := testdb.Config(t)
	dbCfg.DataDir = cfg.Database.DataDir
	cfg.Database = dbCfg
	cfg.Captcha.Provider = "none"
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	httpSrv := httptest.NewServer(api.NewServer(cfg, st))
	defer httpSrv.Close()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	hash, _ := password.Hash("SuperSecretPass123!")
	_ = st.Users().CreateUser(ctx, &store.User{ID: "usr_admin", Username: "admin", PasswordHash: hash, Role: "user", Status: "active", SSOProvider: "local"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_admin", Role: store.RoleOrganizationAdmin, Status: "active"})
	jar := login(t, httpSrv.URL, "admin", "SuperSecretPass123!")
	access := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a", EnvironmentID: "env-a"}
	// Minted in the store: the route renders a manifest, which needs an HTTPS address and a
	// pinned image this test server does not have.
	tok, err := ts.CreateEnrollmentToken(ctx, access, protocol.RuntimeKubernetes, "")
	if err != nil {
		t.Fatal(err)
	}

	identities := &memIdentities{}
	facts := map[string]string{"runtime": "kubernetes", "server_version": "v1.36.0", "node_count": "1", "platform": "linux/amd64"}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	id, err := client.Enroll(ctx, httpClient, httpSrv.URL, identities, "cluster-1", base64.RawURLEncoding.EncodeToString(tok.Secret), facts)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if identities.saves != 1 {
		t.Fatalf("enrollment saved %d times", identities.saves)
	}
	e, _ := ts.ReadEndpointRaw(ctx, id.EndpointID)
	if e.Runtime != protocol.RuntimeKubernetes || e.Facts["server_version"] != "v1.36.0" || e.Facts["node_count"] != "1" || e.Facts["platform"] != "linux/amd64" {
		t.Fatalf("endpoint %+v", e)
	}
	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/approve", jar, `{"fingerprint":"`+e.Fingerprint+`"}`)

	snapshot := func(context.Context) (*protocol.Snapshot, error) {
		return &protocol.Snapshot{ObservedAt: time.Now().UTC(), Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.36.0"}, Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
			Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "kind-control-plane", Ready: true}}}}, nil
	}
	logs := func(context.Context, protocol.LogRequest, func([]byte) error) error { return nil }
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx, id, client.Options{HTTPClient: httpClient, Identities: identities, Kubernetes: true, Snapshot: snapshot, Logs: logs, InventoryEvery: time.Second})
	}()
	waitState(t, ctx, ts, id.EndpointID, "active")
	deadline := time.Now().Add(8 * time.Second)
	for {
		ep, err := ts.ReadEndpoint(ctx, access, id.EndpointID)
		if err == nil && slices.Equal(ep.Capabilities, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("capabilities %+v %v", ep, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	inv, err := ts.ReadInventory(ctx, access, id.EndpointID)
	if err != nil || !strings.Contains(string(inv.Snapshot), `"kind-control-plane"`) {
		t.Fatalf("inventory %v: %s", err, inv.Snapshot)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	saved, _ := identities.Load()
	if saved == nil || saved.Generation == 0 {
		t.Fatal("the rising generation was not written back through the store")
	}

	// With no runtime answer at all the facts-only report still has the cluster shape.
	before := saved.Generation
	runCtx, stop = context.WithCancel(ctx)
	defer stop()
	go func() {
		done <- client.Run(runCtx, saved, client.Options{HTTPClient: httpClient, Identities: identities, Kubernetes: true, InventoryEvery: time.Second})
	}()
	deadline = time.Now().Add(8 * time.Second)
	for {
		inv, err := ts.ReadInventory(ctx, access, id.EndpointID)
		if err == nil && inv.Generation > before {
			if !strings.Contains(string(inv.Snapshot), `"kubernetes":{`) || strings.Contains(string(inv.Snapshot), "kind-control-plane") {
				t.Fatalf("facts-only report: %s", inv.Snapshot)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no facts-only report: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("second run: %v", err)
	}
}
```

Update the existing `Enroll` call sites in `internal/agent/client/client_test.go` to the new signature (directory → `client.DirStore(...)`, runtime version → `client.Facts("")`):

```bash
perl -0pi -e 's/client\.Enroll\(ctx, (httpClient|stub\.Client\(\)|&http\.Client\{Timeout: 10 \* time\.Second\}), (httpSrv\.URL|bad|stub\.URL), (dir|filepath\.Join\(t\.TempDir\(\), "again"\)|t\.TempDir\(\)), ("[^"]*"), (tok\.Token|strings\.Repeat\("A", 43\)), ""\)/client.Enroll(ctx, $1, $2, client.DirStore($3), $4, $5, client.Facts(""))/g' internal/agent/client/client_test.go && test "$(grep -c 'client.Enroll(ctx, .*client.DirStore(.*client.Facts("")' internal/agent/client/client_test.go)" = 6
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 -run TestClusterAgentEnrollsAndReportsTheCluster ./internal/agent/client/`
Expected: FAIL to compile (`cannot use identities ... as string value`, `unknown field Identities in struct literal`, `unknown field Kubernetes`).

- [ ] **Step 3: Add the identity store**

In `internal/agent/client/identity.go`, replace the whole `LoadIdentity` function

```go
// LoadIdentity returns nil, nil when the agent has never enrolled.
func LoadIdentity(dir string) (*Identity, error) {
	raw, err := os.ReadFile(identityPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("identity file: %w", err)
	}
	if len(id.PrivateKey) != ed25519.PrivateKeySize || id.EndpointID == "" {
		return nil, errors.New("identity file: incomplete")
	}
	return &id, nil
}
```

with

```go
// IdentityStore keeps the identity between runs: a directory on a Docker host, a Secret in a
// cluster (internal/runtime/kubernetes).
type IdentityStore interface {
	// Load returns nil, nil when the agent has never enrolled.
	Load() (*Identity, error)
	Save(*Identity) error
}

// DirStore is the identity file in a directory, owner-only.
type DirStore string

func (d DirStore) Load() (*Identity, error) { return LoadIdentity(string(d)) }
func (d DirStore) Save(id *Identity) error  { return SaveIdentity(string(d), id) }

// LoadIdentity returns nil, nil when the agent has never enrolled.
func LoadIdentity(dir string) (*Identity, error) {
	raw, err := os.ReadFile(identityPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return DecodeIdentity(raw)
}

// DecodeIdentity is the one reading of a stored identity, whichever store held it.
func DecodeIdentity(raw []byte) (*Identity, error) {
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("identity file: %w", err)
	}
	if len(id.PrivateKey) != ed25519.PrivateKeySize || id.EndpointID == "" {
		return nil, errors.New("identity file: incomplete")
	}
	return &id, nil
}
```

- [ ] **Step 4: Enroll through a store with the caller's facts**

In `internal/agent/client/enroll.go`, replace

```go
// Enroll redeems a token for an identity and pins the server's instance fingerprint. The token
// is used once and dropped; only the resulting identity is persisted.
func Enroll(ctx context.Context, httpClient *http.Client, server, dir, name, tokenB64 string, runtimeVersion string) (*Identity, error) {
```

with

```go
// Enroll redeems a token for an identity and pins the server's instance fingerprint. The token
// is used once and dropped; only the resulting identity is persisted, to store. Facts are the
// bounded report the approver reads beside the fingerprint (Facts for a Docker host).
func Enroll(ctx context.Context, httpClient *http.Client, server string, store IdentityStore, name, tokenB64 string, facts map[string]string) (*Identity, error) {
```

and in the same function replace `"name":  name, "facts": Facts(runtimeVersion),` with `"name":  name, "facts": facts,` and `if err := SaveIdentity(dir, id); err != nil {` with `if err := store.Save(id); err != nil {`.

In `cmd/agent/main.go`, replace

```go
		id, err = client.Enroll(ctx, httpClient, *server, *dir, *name, token, runtimeVersion)
```

with

```go
		id, err = client.Enroll(ctx, httpClient, *server, client.DirStore(*dir), *name, token, client.Facts(runtimeVersion))
```

- [ ] **Step 5: Cluster options on the connection**

In `internal/agent/client/connect.go`, in `type Options`, replace

```go
	// IdentityDir is where the rising inventory generation and rotation state are written back.
	IdentityDir string
```

with

```go
	// Identities is where the rising inventory generation and rotation state are written back.
	// Nil falls back to IdentityDir.
	Identities IdentityStore
	// IdentityDir is the identity directory when Identities is nil, and the default CommandDir.
	IdentityDir string
	// Kubernetes marks a cluster agent: it advertises kubernetes.inventory, and pod.logs when
	// Logs is set, in place of every Docker capability, and its facts-only snapshot carries an
	// empty cluster inventory so it still has the shape a Kubernetes endpoint requires.
	Kubernetes bool
```

In `session`, replace

```go
	capabilities := []string{}
	if opts.Inspect != nil {
```

with

```go
	capabilities := []string{}
	if opts.Kubernetes {
		capabilities = append(capabilities, protocol.CapabilityKubernetesInventory)
		if opts.Logs != nil {
			capabilities = append(capabilities, protocol.CapabilityPodLogs)
		}
	}
	if opts.Inspect != nil {
```

Replace `save`'s head

```go
func (o *Options) save(id *Identity) error {
	if o.IdentityDir == "" {
		return nil
	}
	if err := SaveIdentity(o.IdentityDir, id); err != nil {
```

with

```go
func (o *Options) save(id *Identity) error {
	store := o.Identities
	if store == nil {
		if o.IdentityDir == "" {
			return nil
		}
		store = DirStore(o.IdentityDir)
	}
	if err := store.Save(id); err != nil {
```

In `sendInventory`, replace

```go
		snap = &protocol.Snapshot{ObservedAt: time.Now().UTC(), Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}
	}
```

with

```go
		snap = &protocol.Snapshot{ObservedAt: time.Now().UTC(), Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}
		if opts.Kubernetes {
			snap.Kubernetes = &protocol.KubernetesInventory{}
		}
	}
```

- [ ] **Step 6: Keep the cluster facts**

In `internal/store/endpoints.go`, replace

```go
var factKeys = map[string]bool{"hostname": true, "os": true, "runtime_version": true, "cpus": true, "memory_bytes": true}
```

with

```go
// factKeys are the enrollment facts kept: a Docker host's, then a cluster's.
var factKeys = map[string]bool{"hostname": true, "os": true, "runtime_version": true, "cpus": true, "memory_bytes": true, "runtime": true, "server_version": true, "node_count": true, "platform": true}
```

- [ ] **Step 7: Run the tests to verify they pass, on both drivers**

Run: `gofmt -w internal/agent cmd/agent internal/store && go vet ./... && go test -race -count=1 ./internal/agent/... ./cmd/... && go test -count=1 ./internal/store/`
Expected: PASS, including every existing client test.

Run: `PG=… go test -count=1 -run 'TestClusterAgent|TestAgentEnrolls' ./internal/agent/client/ && PG=… go test -count=1 ./internal/store/`
Expected: PASS.

- [ ] **Step 8: DOX and commit**

`internal/agent/AGENTS.md`, `## Local Contracts`, the bullet beginning "The server’s built-in local agent reuses `Run`": replace the sentence "`Options.CommandDir` separates its durable command ledger from identity-file persistence; ordinary agents continue using `IdentityDir` for both." with "`Options.CommandDir` separates its durable command ledger from identity persistence; ordinary Docker agents use `IdentityDir` for both. `Options.Identities` (`client.IdentityStore`: `DirStore` for a directory, `kubernetes.SecretIdentityStore` in a cluster) takes precedence for identity writes; `Enroll` saves through the store it is given and sends the caller's facts (`client.Facts` for a Docker host)." Then append to `## Local Contracts`:

```markdown
- `Options.Kubernetes` marks a cluster agent: it advertises `kubernetes.inventory`, plus `pod.logs` when `Options.Logs` is set, and no Docker capability; when its snapshot fails, the facts-only report carries an empty `KubernetesInventory` so it keeps the Kubernetes shape.
```

`internal/store/AGENTS.md`, `## Local Contracts`, append:

```markdown
- Enrollment keeps only whitelisted facts (`factKeys`): a Docker host's `hostname`, `os`, `runtime_version`, `cpus`, `memory_bytes`, and a cluster's `runtime`, `server_version`, `node_count`, `platform`; every value is `displaySafe`.
```

```bash
git add internal/agent cmd/agent internal/store && make tidy-check lint && git commit -m "feat(agent): identity stores and cluster capabilities" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: The Kubernetes adapter — snapshot, pod logs and the identity Secret

**Files:**
- Modify: `go.mod`, `go.sum` (client-go v0.37.1)
- Create: `internal/runtime/kubernetes/kubernetes.go` (`Client`, `New`, `InCluster`, `NewFromClientset`, `OwnNamespace`, `Facts`, `Snapshot`)
- Create: `internal/runtime/kubernetes/logs.go` (`Logs`)
- Create: `internal/runtime/kubernetes/identity.go` (`SecretIdentityStore`)
- Test: `internal/runtime/kubernetes/kubernetes_test.go`, `internal/runtime/kubernetes/identity_test.go`
- Docs: create `internal/runtime/kubernetes/AGENTS.md`; modify `internal/runtime/AGENTS.md`, root `AGENTS.md` (Child DOX Index line)

**Interfaces:**
- Consumes: Task 1's protocol types and rules; Task 2's `client.Identity`, `client.DecodeIdentity`, `client.IdentityStore`.
- Produces (package `kubernetes`, import `github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes`):
  ```go
  type Client struct{ /* unexported: cs k8s.Interface; openLog; log */ }
  func New(cfg *rest.Config) (*Client, error)
  func InCluster() (*Client, error)
  func NewFromClientset(cs k8s.Interface) *Client // tests
  func OwnNamespace() (string, error)
  func (c *Client) Facts(ctx context.Context) map[string]string // runtime, server_version, node_count, platform
  func (c *Client) Snapshot(ctx context.Context) (*protocol.Snapshot, error) // error always nil
  func (c *Client) Logs(ctx context.Context, req protocol.LogRequest, sink func([]byte) error) error
  var ErrIdentityConflict error
  type SecretIdentityStore struct{ /* unexported */ }
  func NewSecretIdentityStore(c *Client, namespace, name string) (*SecretIdentityStore, error)
  func (s *SecretIdentityStore) Load() (*client.Identity, error)
  func (s *SecretIdentityStore) Save(id *client.Identity) error
  ```

- [ ] **Step 1: Add the dependency**

Run: `go get k8s.io/client-go@v0.37.1 k8s.io/api@v0.37.1 k8s.io/apimachinery@v0.37.1 && grep -n '^go ' go.mod`
Expected: `go 1.26.6` unchanged; the three modules listed (as `// indirect` until Step 5's `go mod tidy`).

- [ ] **Step 2: Write the failing tests**

Create `internal/runtime/kubernetes/kubernetes_test.go`:

```go
package kubernetes

import (
	"context"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// cluster is a fake API server holding objects, with a fixed server version and a quiet log.
func cluster(t *testing.T, objects ...runtime.Object) (*Client, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(objects...)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: "36", GitVersion: "v1.36.0", Platform: "linux/amd64"}
	c := NewFromClientset(cs)
	c.log = log.New(io.Discard, "", 0)
	return c, cs
}

func meta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name}
}

// Every field the inventory carries is read from the object that holds it, and a Deployment's
// pod names the Deployment through its ReplicaSet's name.
func TestSnapshotMapsTheCluster(t *testing.T) {
	started := metav1.NewTime(time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC))
	controller := true
	three, storage := int32(3), "fast"
	objects := []runtime.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Labels: map[string]string{"node-role.kubernetes.io/worker": "", "kubernetes.io/os": "linux"}},
			Spec:   corev1.NodeSpec{Unschedulable: true},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.36.0", OperatingSystem: "linux", Architecture: "arm64"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "control-1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.36.0", OperatingSystem: "linux", Architecture: "amd64"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&appsv1.Deployment{ObjectMeta: meta("shop", "web"), Spec: appsv1.DeploymentSpec{Replicas: &three, Paused: true, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx:1.29"}, {Name: "log", Image: "busybox:1"}}}}},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 2, UpdatedReplicas: 1}},
		&appsv1.StatefulSet{ObjectMeta: meta("shop", "db"), Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "postgres:17"}}}}},
			Status: appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1}},
		&appsv1.DaemonSet{ObjectMeta: meta("kube-system", "proxy"), Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "proxy", Image: "kube-proxy:1"}}}}},
			Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 1, UpdatedNumberScheduled: 2}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-7c9d-x2", Labels: map[string]string{"pod-template-hash": "7c9d"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-7c9d", Controller: &controller}}},
			Spec: corev1.PodSpec{NodeName: "worker-1", Containers: []corev1.Container{{Name: "web", Image: "nginx:1.29"}, {Name: "log", Image: "busybox:1"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &started, ContainerStatuses: []corev1.ContainerStatus{
				{Name: "web", ImageID: "docker.io/library/nginx@sha256:aa", Ready: true, RestartCount: 4, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "log", ImageID: "docker.io/library/busybox@sha256:bb", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
			}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-0", OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: &controller}}},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "postgres:17"}}},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: "db", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}}},
		&corev1.Pod{ObjectMeta: meta("default", "bare"), Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sh", Image: "alpine:3"}}}, Status: corev1.PodStatus{Phase: corev1.PodPending}},
		&corev1.Service{ObjectMeta: meta("shop", "web"), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, ClusterIP: "10.96.0.10", Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP}, {Port: 53, Protocol: corev1.ProtocolUDP}}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: meta("shop", "data"), Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &storage},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}},
	}
	c, _ := cluster(t, objects...)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.CheckRuntimeShape(protocol.RuntimeKubernetes, snap); err != nil || len(snap.Truncated) != 0 {
		t.Fatalf("shape %v truncated %v", err, snap.Truncated)
	}
	if e := snap.Engine; e.Runtime != "kubernetes" || e.Version != "v1.36.0" || e.APIVersion != "1.36" || e.OS != "linux" || e.Arch != "amd64" {
		t.Fatalf("engine %+v", e)
	}
	k := snap.Kubernetes
	want := []protocol.Node{
		{Name: "control-1", KubeletVersion: "v1.36.0", OS: "linux", Arch: "amd64", Ready: true, Roles: []string{"control-plane"}},
		{Name: "worker-1", KubeletVersion: "v1.36.0", OS: "linux", Arch: "arm64", Ready: false, Roles: []string{"worker"}, Unschedulable: true},
	}
	if fmt.Sprint(k.Nodes) != fmt.Sprint(want) {
		t.Fatalf("nodes %+v", k.Nodes)
	}
	if !slices.Equal(k.Namespaces, []string{"default", "shop"}) {
		t.Fatalf("namespaces %v", k.Namespaces)
	}
	wantWorkloads := []protocol.Workload{
		{Kind: "DaemonSet", Namespace: "kube-system", Name: "proxy", Desired: 2, Ready: 1, Updated: 2, Images: []string{"kube-proxy:1"}},
		{Kind: "StatefulSet", Namespace: "shop", Name: "db", Desired: 1, Ready: 1, Updated: 1, Images: []string{"postgres:17"}},
		{Kind: "Deployment", Namespace: "shop", Name: "web", Desired: 3, Ready: 2, Updated: 1, Images: []string{"nginx:1.29", "busybox:1"}, Paused: true},
	}
	if fmt.Sprint(k.Workloads) != fmt.Sprint(wantWorkloads) {
		t.Fatalf("workloads %+v", k.Workloads)
	}
	if len(k.Pods) != 3 {
		t.Fatalf("pods %+v", k.Pods)
	}
	bare, db, web := k.Pods[0], k.Pods[1], k.Pods[2]
	if bare.Name != "bare" || bare.Phase != "Pending" || bare.OwnerKind != "" || bare.Containers[0].State != "waiting" || !bare.StartedAt.IsZero() {
		t.Fatalf("bare pod %+v", bare)
	}
	if db.OwnerKind != "StatefulSet" || db.OwnerName != "db" || db.Containers[0].State != "terminated" || db.Containers[0].Reason != "Completed" {
		t.Fatalf("stateful pod %+v", db)
	}
	wantWeb := protocol.Pod{Namespace: "shop", Name: "web-7c9d-x2", Phase: "Running", Node: "worker-1", OwnerKind: "Deployment", OwnerName: "web", StartedAt: started.UTC(), Containers: []protocol.PodContainer{
		{Name: "web", Image: "nginx:1.29", ImageID: "docker.io/library/nginx@sha256:aa", State: "running", Ready: true, RestartCount: 4},
		{Name: "log", Image: "busybox:1", ImageID: "docker.io/library/busybox@sha256:bb", State: "waiting", Reason: "CrashLoopBackOff"},
	}}
	if fmt.Sprint(web) != fmt.Sprint(wantWeb) {
		t.Fatalf("deployment pod %+v", web)
	}
	if s := k.Services; len(s) != 1 || s[0].Type != "NodePort" || s[0].ClusterIP != "10.96.0.10" || !slices.Equal(s[0].Ports, []string{"80:30080/TCP", "53/UDP"}) {
		t.Fatalf("services %+v", s)
	}
	if cl := k.Claims; len(cl) != 1 || cl[0] != (protocol.Claim{Namespace: "shop", Name: "data", Phase: "Bound", StorageClass: "fast", Capacity: "10Gi"}) {
		t.Fatalf("claims %+v", cl)
	}
}

// A list over its cap is cut, in namespace/name order, and named; a list the ServiceAccount
// may not read is reported empty and named, and the rest of the cluster is still reported.
func TestSnapshotCutsAndSurvivesAForbiddenList(t *testing.T) {
	var objects []runtime.Object
	for i := range protocol.MaxClaims + 1 {
		objects = append(objects, &corev1.PersistentVolumeClaim{ObjectMeta: meta("shop", fmt.Sprintf("data-%04d", i))})
	}
	objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}, &corev1.Service{ObjectMeta: meta("shop", "web")})
	c, cs := cluster(t, objects...)
	cs.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", nil)
	})
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	k := snap.Kubernetes
	if len(k.Claims) != protocol.MaxClaims || k.Claims[0].Name != "data-0000" || k.Claims[protocol.MaxClaims-1].Name != fmt.Sprintf("data-%04d", protocol.MaxClaims-1) {
		t.Fatalf("claims %d, first %q", len(k.Claims), k.Claims[0].Name)
	}
	if len(k.Services) != 0 || !slices.Equal(snap.Truncated, []string{"services", "claims"}) {
		t.Fatalf("services %v truncated %v", k.Services, snap.Truncated)
	}
	if !slices.Equal(k.Namespaces, []string{"shop"}) {
		t.Fatalf("the rest of the cluster went with the forbidden list: %v", k.Namespaces)
	}
}

// Lists are read a page of 500 at a time and every page is joined.
func TestSnapshotPagesWithLimitAndContinue(t *testing.T) {
	c, cs := cluster(t)
	var asked []metav1.ListOptions
	cs.PrependReactor("list", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		opts := a.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		asked = append(asked, opts)
		if opts.Continue == "" {
			return true, &corev1.NamespaceList{ListMeta: metav1.ListMeta{Continue: "page-2"}, Items: []corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}}}, nil
		}
		return true, &corev1.NamespaceList{Items: []corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "b"}}}}, nil
	})
	snap, _ := c.Snapshot(context.Background())
	if !slices.Equal(snap.Kubernetes.Namespaces, []string{"a", "b"}) || len(asked) != 2 || asked[0].Limit != 500 || asked[1].Continue != "page-2" {
		t.Fatalf("namespaces %v after %+v", snap.Kubernetes.Namespaces, asked)
	}
}

func TestFactsNameTheCluster(t *testing.T) {
	c, _ := cluster(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}})
	facts := c.Facts(context.Background())
	if facts["runtime"] != "kubernetes" || facts["server_version"] != "v1.36.0" || facts["node_count"] != "2" || facts["platform"] != "linux/amd64" || len(facts) != 4 {
		t.Fatalf("facts %v", facts)
	}
}

// logPod is a pod with the given containers in namespace shop.
func logPod(name string, containers ...string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: meta("shop", name)}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	return p
}

// History asks the kubelet for the tail, the timestamps, the window and the byte ceiling, of
// the pod's only container when none is named.
func TestLogsAskForTheRequestedWindow(t *testing.T) {
	c, _ := cluster(t, logPod("web", "nginx"))
	var got *corev1.PodLogOptions
	c.openLog = func(_ context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		if namespace != "shop" || pod != "web" {
			t.Errorf("opened %s/%s", namespace, pod)
		}
		got = opts
		return io.NopCloser(strings.NewReader("one\ntwo\n")), nil
	}
	since := time.Date(2026, 9, 25, 7, 0, 0, 0, time.UTC)
	var out strings.Builder
	err := c.Logs(context.Background(), protocol.LogRequest{Stream: "s", Pod: &protocol.PodTarget{Namespace: "shop", Name: "web"}, Tail: 5, Since: since, Timestamps: true}, func(b []byte) error { out.Write(b); return nil })
	if err != nil || out.String() != "one\ntwo\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
	if got.Container != "nginx" || *got.TailLines != 5 || !got.Timestamps || got.Follow || !got.SinceTime.Time.Equal(since) || *got.LimitBytes != protocol.MaxLogBytes {
		t.Fatalf("options %+v", got)
	}
}

// A pod with several containers is refused unless one is named, and a name the pod does not
// run is refused, both before any log is opened; so is a request that is not for a pod.
func TestLogsRefuseAnUnnamedOrUnknownContainer(t *testing.T) {
	c, _ := cluster(t, logPod("multi", "web", "sidecar"))
	c.openLog = func(context.Context, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
		t.Error("a log was opened")
		return io.NopCloser(strings.NewReader("")), nil
	}
	sink := func([]byte) error { return nil }
	for _, target := range []protocol.PodTarget{{Namespace: "shop", Name: "multi"}, {Namespace: "shop", Name: "multi", Container: "db"}} {
		err := c.Logs(context.Background(), protocol.LogRequest{Pod: &target}, sink)
		if err == nil || !strings.HasPrefix(err.Error(), "unknown_container") {
			t.Fatalf("%+v: %v", target, err)
		}
	}
	if err := c.Logs(context.Background(), protocol.LogRequest{Pod: &protocol.PodTarget{Namespace: "shop", Name: "gone"}}, sink); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("missing pod: %v", err)
	}
	if err := c.Logs(context.Background(), protocol.LogRequest{Container: strings.Repeat("a", 64)}, sink); err == nil {
		t.Fatal("a container ID reached the cluster adapter")
	}
}

// A follow ends when the reader goes, even with the kubelet silent.
func TestLogsFollowEndsWhenCancelled(t *testing.T) {
	c, _ := cluster(t, logPod("web", "nginx"))
	reader, writer := io.Pipe()
	c.openLog = func(_ context.Context, _, _ string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		if !opts.Follow {
			t.Error("not following")
		}
		return reader, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Logs(ctx, protocol.LogRequest{Pod: &protocol.PodTarget{Namespace: "shop", Name: "web"}, Follow: true}, func(b []byte) error { got <- string(b); return nil })
	}()
	go func() { _, _ = writer.Write([]byte("line\n")) }()
	if s := <-got; s != "line\n" {
		t.Fatalf("got %q", s)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled follow: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follow outlived its reader")
	}
}

// The adapter stops at 10,000 lines, however much more the kubelet sends.
func TestLogsStopAtTheLineCeiling(t *testing.T) {
	c, _ := cluster(t, logPod("web", "nginx"))
	c.openLog = func(context.Context, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(strings.Repeat("x\n", protocol.MaxLogLines*2))), nil
	}
	lines := 0
	err := c.Logs(context.Background(), protocol.LogRequest{Pod: &protocol.PodTarget{Namespace: "shop", Name: "web"}}, func(b []byte) error {
		lines += strings.Count(string(b), "\n")
		return nil
	})
	if err != nil || lines != protocol.MaxLogLines {
		t.Fatalf("%d lines, %v", lines, err)
	}
}
```

Create `internal/runtime/kubernetes/identity_test.go`:

```go
package kubernetes

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func testIdentity(endpoint string, generation uint64) *client.Identity {
	_, priv, _ := ed25519.GenerateKey(nil)
	return &client.Identity{EndpointID: endpoint, PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: "https://yard.example", Generation: generation}
}

// The first save creates the Secret, later saves update it in place, and a load returns what
// was saved; an absent Secret is a never-enrolled agent.
func TestSecretIdentityStoreCreatesThenUpdates(t *testing.T) {
	c, cs := cluster(t)
	store, err := NewSecretIdentityStore(c, "kyyard-agent", "kyyard-agent-identity")
	if err != nil {
		t.Fatal(err)
	}
	if id, err := store.Load(); id != nil || err != nil {
		t.Fatalf("fresh: %+v %v", id, err)
	}
	if err := store.Save(testIdentity("ep_1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testIdentity("ep_1", 2)); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got.EndpointID != "ep_1" || got.Generation != 2 {
		t.Fatalf("load %+v %v", got, err)
	}
	sec, _ := cs.CoreV1().Secrets("kyyard-agent").Get(t.Context(), "kyyard-agent-identity", metav1.GetOptions{})
	if sec.Type != corev1.SecretTypeOpaque || sec.Labels["app.kubernetes.io/managed-by"] != "kyyard" || len(sec.Data) != 1 {
		t.Fatalf("secret %+v", sec)
	}
	var creates, updates int
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "secrets" {
			switch a.GetVerb() {
			case "create":
				creates++
			case "update":
				updates++
			}
		}
	}
	if creates != 1 || updates != 1 {
		t.Fatalf("%d creates, %d updates", creates, updates)
	}
	if _, err := NewSecretIdentityStore(c, "Kyyard", "x"); err == nil {
		t.Fatal("an invalid namespace was accepted")
	}
}

// One conflicting write is retried; a second is ErrIdentityConflict, and so is a Secret that
// already holds another endpoint's identity, which is never overwritten.
func TestSecretIdentityStoreConflicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		conflicts int
		want      error
		updates   int
	}{
		{"one conflict is retried", 1, nil, 2},
		{"a second conflict stops", 2, ErrIdentityConflict, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cs := cluster(t)
			store, _ := NewSecretIdentityStore(c, "kyyard-agent", "kyyard-agent-identity")
			if err := store.Save(testIdentity("ep_1", 1)); err != nil {
				t.Fatal(err)
			}
			updates := 0
			cs.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				if updates++; updates <= tc.conflicts {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "kyyard-agent-identity", errors.New("changed"))
				}
				return false, nil, nil
			})
			if err := store.Save(testIdentity("ep_1", 2)); !errors.Is(err, tc.want) || updates != tc.updates {
				t.Fatalf("save: %v after %d updates", err, updates)
			}
		})
	}
	c, _ := cluster(t)
	store, _ := NewSecretIdentityStore(c, "kyyard-agent", "kyyard-agent-identity")
	if err := store.Save(testIdentity("ep_other", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testIdentity("ep_1", 1)); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("overwrote another endpoint's identity: %v", err)
	}
	if got, _ := store.Load(); got.EndpointID != "ep_other" {
		t.Fatalf("stored %s", got.EndpointID)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go mod tidy && go test -count=1 ./internal/runtime/kubernetes/`
Expected: `go mod tidy` adds the transitive `go.sum` entries the test imports need; the test run FAILS to compile (`undefined: NewFromClientset`, `undefined: NewSecretIdentityStore`, ...).

- [ ] **Step 4: Implement the adapter**

Create `internal/runtime/kubernetes/kubernetes.go`:

```go
// Package kubernetes is the Kubernetes adapter: it reads the cluster the agent runs in through
// the typed clientset, with the agent's ServiceAccount, and returns product types only. No
// client-go type leaves this package.
package kubernetes

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// pageSize is the Limit of every List call; Continue pages the rest.
	pageSize = 500
	// callBudget bounds one API call whose caller named no deadline.
	callBudget = 20 * time.Second
	// serviceAccountNamespace is where the kubelet writes the pod's own namespace.
	serviceAccountNamespace = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	// nodeRolePrefix labels a node with a role; the role is the rest of the key.
	nodeRolePrefix = "node-role.kubernetes.io/"
)

type Client struct {
	cs k8s.Interface
	// openLog streams one container's log. Tests replace it: the fake clientset answers every
	// log request with the same text and ignores its options.
	openLog func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)
	log     *log.Logger
}

// New reads the cluster cfg names.
func New(cfg *rest.Config) (*Client, error) {
	cs, err := k8s.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return NewFromClientset(cs), nil
}

// InCluster reads the cluster the agent's pod runs in, as its ServiceAccount.
func InCluster() (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

// NewFromClientset is for tests: cs is usually k8s.io/client-go/kubernetes/fake.
func NewFromClientset(cs k8s.Interface) *Client {
	return &Client{cs: cs, log: log.Default(), openLog: func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		return cs.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
	}}
}

// OwnNamespace is the namespace the agent's pod runs in.
func OwnNamespace() (string, error) {
	raw, err := os.ReadFile(serviceAccountNamespace)
	if err != nil {
		return "", err
	}
	ns := strings.TrimSpace(string(raw))
	if !protocol.ValidDNSLabel(ns) {
		return "", errors.New("the service account namespace file does not hold a namespace name")
	}
	return ns, nil
}

// engine is the API server's version. Discovery takes no context; the clientset's own
// timeouts bound it.
func (c *Client) engine() (protocol.Engine, error) {
	v, err := c.cs.Discovery().ServerVersion()
	if err != nil {
		return protocol.Engine{Runtime: protocol.RuntimeKubernetes}, err
	}
	os, arch, _ := strings.Cut(v.Platform, "/")
	return protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: v.GitVersion, APIVersion: v.Major + "." + v.Minor, OS: os, Arch: arch}, nil
}

// Facts is the enrollment report for a cluster: what an approver compares with the cluster
// they meant to enroll. A fact the agent cannot read is "unknown", never left out.
func (c *Client) Facts(ctx context.Context) map[string]string {
	facts := map[string]string{"runtime": protocol.RuntimeKubernetes, "server_version": "unknown", "node_count": "unknown", "platform": "unknown"}
	if v, err := c.cs.Discovery().ServerVersion(); err == nil {
		facts["server_version"], facts["platform"] = v.GitVersion, v.Platform
	}
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	if nodes, err := listAll(ctx, protocol.MaxNodes, c.nodes); err == nil {
		facts["node_count"] = strconv.Itoa(len(nodes))
	}
	return facts
}

// Snapshot reads the cluster's inventory. A list the ServiceAccount cannot read is reported
// empty and named in Truncated, so one forbidden verb does not blank the whole endpoint; the
// error return is only ever nil.
func (c *Client) Snapshot(ctx context.Context) (*protocol.Snapshot, error) {
	k := &protocol.KubernetesInventory{Nodes: []protocol.Node{}, Namespaces: []string{}, Workloads: []protocol.Workload{}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}}
	snap := &protocol.Snapshot{ObservedAt: time.Now().UTC(), Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}, Kubernetes: k}
	engine, err := c.engine()
	if err != nil {
		c.log.Printf("kubernetes: reading the server version: %v", err)
	}
	snap.Engine = engine
	truncated := map[string]bool{}
	failed := func(list string, err error) {
		c.log.Printf("kubernetes: listing %s: %v; reported empty", list, err)
		truncated[list] = true
	}
	if nodes, err := listAll(ctx, protocol.MaxNodes, c.nodes); err != nil {
		failed("nodes", err)
	} else {
		for _, n := range nodes {
			k.Nodes = append(k.Nodes, node(n))
		}
	}
	if namespaces, err := listAll(ctx, protocol.MaxNamespaces, c.namespaces); err != nil {
		failed("namespaces", err)
	} else {
		for _, ns := range namespaces {
			k.Namespaces = append(k.Namespaces, ns.Name)
		}
	}
	if deployments, err := listAll(ctx, protocol.MaxWorkloads, c.deployments); err != nil {
		failed("workloads", err)
	} else {
		for _, d := range deployments {
			k.Workloads = append(k.Workloads, protocol.Workload{Kind: "Deployment", Namespace: d.Namespace, Name: d.Name, Desired: replicas(d.Spec.Replicas), Ready: d.Status.ReadyReplicas, Updated: d.Status.UpdatedReplicas, Images: images(d.Spec.Template.Spec), Paused: d.Spec.Paused})
		}
	}
	if sets, err := listAll(ctx, protocol.MaxWorkloads, c.statefulSets); err != nil {
		failed("workloads", err)
	} else {
		for _, s := range sets {
			k.Workloads = append(k.Workloads, protocol.Workload{Kind: "StatefulSet", Namespace: s.Namespace, Name: s.Name, Desired: replicas(s.Spec.Replicas), Ready: s.Status.ReadyReplicas, Updated: s.Status.UpdatedReplicas, Images: images(s.Spec.Template.Spec)})
		}
	}
	if sets, err := listAll(ctx, protocol.MaxWorkloads, c.daemonSets); err != nil {
		failed("workloads", err)
	} else {
		for _, s := range sets {
			k.Workloads = append(k.Workloads, protocol.Workload{Kind: "DaemonSet", Namespace: s.Namespace, Name: s.Name, Desired: s.Status.DesiredNumberScheduled, Ready: s.Status.NumberReady, Updated: s.Status.UpdatedNumberScheduled, Images: images(s.Spec.Template.Spec)})
		}
	}
	if pods, err := listAll(ctx, protocol.MaxPods, c.pods); err != nil {
		failed("pods", err)
	} else {
		for _, p := range pods {
			k.Pods = append(k.Pods, pod(p))
		}
	}
	if services, err := listAll(ctx, protocol.MaxServices, c.services); err != nil {
		failed("services", err)
	} else {
		for _, s := range services {
			k.Services = append(k.Services, service(s))
		}
	}
	if claims, err := listAll(ctx, protocol.MaxClaims, c.claims); err != nil {
		failed("claims", err)
	} else {
		for _, pvc := range claims {
			k.Claims = append(k.Claims, claim(pvc))
		}
	}
	slices.SortFunc(k.Nodes, func(a, b protocol.Node) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(k.Namespaces)
	slices.SortFunc(k.Workloads, func(a, b protocol.Workload) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name), strings.Compare(a.Kind, b.Kind))
	})
	slices.SortFunc(k.Pods, func(a, b protocol.Pod) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	slices.SortFunc(k.Services, func(a, b protocol.Service) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	slices.SortFunc(k.Claims, func(a, b protocol.Claim) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	for list := range truncated {
		snap.Truncated = append(snap.Truncated, list)
	}
	// Clamp cuts every list at its cap after the sort and names the cut; Shrink fits the
	// shared byte limit.
	protocol.Clamp(snap)
	_ = protocol.Shrink(snap)
	return snap, nil
}

// listAll pages through one kind with Limit and Continue. It stops once it holds more than
// max, which is enough to know the list is cut without reading the rest of a large cluster;
// the API server returns items in namespace/name order, so the pages read are the head of it.
func listAll[T any](ctx context.Context, max int, page func(context.Context, metav1.ListOptions) ([]T, string, error)) ([]T, error) {
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	var out []T
	opts := metav1.ListOptions{Limit: pageSize}
	for {
		items, next, err := page(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" || len(out) > max {
			return out, nil
		}
		opts.Continue = next
	}
}

func (c *Client) nodes(ctx context.Context, o metav1.ListOptions) ([]corev1.Node, string, error) {
	l, err := c.cs.CoreV1().Nodes().List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) namespaces(ctx context.Context, o metav1.ListOptions) ([]corev1.Namespace, string, error) {
	l, err := c.cs.CoreV1().Namespaces().List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) deployments(ctx context.Context, o metav1.ListOptions) ([]appsv1.Deployment, string, error) {
	l, err := c.cs.AppsV1().Deployments("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) statefulSets(ctx context.Context, o metav1.ListOptions) ([]appsv1.StatefulSet, string, error) {
	l, err := c.cs.AppsV1().StatefulSets("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) daemonSets(ctx context.Context, o metav1.ListOptions) ([]appsv1.DaemonSet, string, error) {
	l, err := c.cs.AppsV1().DaemonSets("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) pods(ctx context.Context, o metav1.ListOptions) ([]corev1.Pod, string, error) {
	l, err := c.cs.CoreV1().Pods("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) services(ctx context.Context, o metav1.ListOptions) ([]corev1.Service, string, error) {
	l, err := c.cs.CoreV1().Services("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) claims(ctx context.Context, o metav1.ListOptions) ([]corev1.PersistentVolumeClaim, string, error) {
	l, err := c.cs.CoreV1().PersistentVolumeClaims("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func node(n corev1.Node) protocol.Node {
	out := protocol.Node{Name: n.Name, KubeletVersion: n.Status.NodeInfo.KubeletVersion, OS: n.Status.NodeInfo.OperatingSystem, Arch: n.Status.NodeInfo.Architecture, Unschedulable: n.Spec.Unschedulable, Roles: []string{}}
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			out.Ready = cond.Status == corev1.ConditionTrue
		}
	}
	for key := range n.Labels {
		if role, ok := strings.CutPrefix(key, nodeRolePrefix); ok && role != "" {
			out.Roles = append(out.Roles, role)
		}
	}
	slices.Sort(out.Roles)
	return out
}

// replicas is a workload's desired count; Kubernetes defaults an unset one to 1.
func replicas(n *int32) int32 {
	if n == nil {
		return 1
	}
	return *n
}

func images(spec corev1.PodSpec) []string {
	out := []string{}
	for _, c := range spec.Containers {
		out = append(out, c.Image)
	}
	return out
}

func pod(p corev1.Pod) protocol.Pod {
	out := protocol.Pod{Namespace: p.Namespace, Name: p.Name, Phase: string(p.Status.Phase), Node: p.Spec.NodeName, Containers: []protocol.PodContainer{}}
	if p.Status.StartTime != nil {
		out.StartedAt = p.Status.StartTime.UTC()
	}
	out.OwnerKind, out.OwnerName = owner(p)
	statuses := map[string]corev1.ContainerStatus{}
	for _, s := range p.Status.ContainerStatuses {
		statuses[s.Name] = s
	}
	for _, c := range p.Spec.Containers {
		pc := protocol.PodContainer{Name: c.Name, Image: c.Image, State: "waiting"}
		if s, ok := statuses[c.Name]; ok {
			pc.ImageID, pc.Ready, pc.RestartCount = s.ImageID, s.Ready, s.RestartCount
			switch {
			case s.State.Running != nil:
				pc.State = "running"
			case s.State.Terminated != nil:
				pc.State, pc.Reason = "terminated", s.State.Terminated.Reason
			case s.State.Waiting != nil:
				pc.Reason = s.State.Waiting.Reason
			}
		}
		out.Containers = append(out.Containers, pc)
	}
	return out
}

// owner is the pod's controller. A ReplicaSet a Deployment made is named after the Deployment
// plus the pod-template-hash the pod also carries, so the Deployment is read off the name
// rather than from the ReplicaSet, which the agent's role cannot list.
func owner(p corev1.Pod) (kind, name string) {
	ref := metav1.GetControllerOf(&p)
	if ref == nil {
		return "", ""
	}
	if hash := p.Labels[appsv1.DefaultDeploymentUniqueLabelKey]; ref.Kind == "ReplicaSet" && hash != "" {
		if deployment, ok := strings.CutSuffix(ref.Name, "-"+hash); ok && deployment != "" {
			return "Deployment", deployment
		}
	}
	return ref.Kind, ref.Name
}

func service(s corev1.Service) protocol.Service {
	out := protocol.Service{Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type), ClusterIP: s.Spec.ClusterIP, Ports: []string{}}
	for _, p := range s.Spec.Ports {
		port := strconv.Itoa(int(p.Port))
		if p.NodePort != 0 {
			port += ":" + strconv.Itoa(int(p.NodePort))
		}
		out.Ports = append(out.Ports, port+"/"+string(p.Protocol))
	}
	return out
}

func claim(pvc corev1.PersistentVolumeClaim) protocol.Claim {
	out := protocol.Claim{Namespace: pvc.Namespace, Name: pvc.Name, Phase: string(pvc.Status.Phase)}
	if pvc.Spec.StorageClassName != nil {
		out.StorageClass = *pvc.Spec.StorageClassName
	}
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		out.Capacity = q.String()
	}
	return out
}
```

Create `internal/runtime/kubernetes/logs.go`:

```go
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// logFetchBudget and logFollowBudget match the Docker adapter's: history is a read the
	// kubelet already has; a follow ends after an hour and the operator reopens it.
	logFetchBudget  = 60 * time.Second
	logFollowBudget = time.Hour
	// unknownContainer starts every refusal that names no container the pod runs.
	unknownContainer = "unknown_container"
)

// Logs streams one pod container's log to sink, at most protocol.MaxLogLines lines or
// protocol.MaxLogBytes bytes, whichever comes first. The container comes from the pod spec: an
// unnamed one is the pod's only container, and a pod with several is refused rather than
// guessed at.
func (c *Client) Logs(ctx context.Context, req protocol.LogRequest, sink func([]byte) error) error {
	if err := req.ValidateFor(protocol.RuntimeKubernetes); err != nil {
		return err
	}
	budget := logFetchBudget
	if req.Follow {
		budget = logFollowBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	target := *req.Pod
	p, err := c.cs.CoreV1().Pods(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return errors.New("the pod no longer exists")
	}
	if err != nil {
		return errors.New("the cluster did not answer for the pod")
	}
	container, err := podContainer(p.Spec.Containers, target.Container)
	if err != nil {
		return err
	}
	opts := &corev1.PodLogOptions{Container: container, Follow: req.Follow, Timestamps: req.Timestamps, LimitBytes: new(int64(protocol.MaxLogBytes))}
	if req.Tail > 0 {
		opts.TailLines = new(int64(req.Tail))
	}
	if !req.Since.IsZero() {
		opts.SinceTime = new(metav1.NewTime(req.Since))
	}
	body, err := c.openLog(ctx, target.Namespace, target.Name, opts)
	if err != nil {
		return errors.New("the cluster refused the log request")
	}
	defer body.Close()
	// A read blocked on a quiet follow ends when the reader goes, not at the next line.
	stop := context.AfterFunc(ctx, func() { body.Close() })
	defer stop()
	return copyBounded(ctx, body, sink)
}

func podContainer(containers []corev1.Container, want string) (string, error) {
	if want == "" {
		if len(containers) == 1 {
			return containers[0].Name, nil
		}
		return "", fmt.Errorf("%s: this pod runs %d containers; name one", unknownContainer, len(containers))
	}
	for _, c := range containers {
		if c.Name == want {
			return want, nil
		}
	}
	return "", fmt.Errorf("%s: the pod runs no container named %s", unknownContainer, want)
}

// copyBounded forwards the log in reads of at most one chunk and stops at the request's
// ceiling. A stream that ends, or is ended by the reader, is not a failure.
func copyBounded(ctx context.Context, body io.Reader, sink func([]byte) error) error {
	buf := make([]byte, protocol.MaxLogChunkBytes)
	lines, total := 0, 0
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := buf[:min(n, protocol.MaxLogBytes-total)]
			for i, b := range chunk {
				if b == '\n' {
					if lines++; lines == protocol.MaxLogLines {
						chunk = chunk[:i+1]
						break
					}
				}
			}
			total += len(chunk)
			if serr := sink(chunk); serr != nil {
				return serr
			}
			if total >= protocol.MaxLogBytes || lines >= protocol.MaxLogLines {
				return nil
			}
		}
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return errors.New("the log stream broke")
		}
	}
}
```

Create `internal/runtime/kubernetes/identity.go`:

```go
package kubernetes

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// identityKey is the Secret's one data key.
const identityKey = "identity.json"

// ErrIdentityConflict is a Secret that changed under this agent twice running, or that holds
// another endpoint's identity: the sign of two agents sharing one identity, which this store
// exists to prevent. The caller stops.
var ErrIdentityConflict = errors.New("the identity Secret changed under this agent; another agent may share this identity")

// SecretIdentityStore keeps the agent identity in a Secret in the agent's own namespace, so a
// restarted pod comes back as the same endpoint instead of enrolling again.
type SecretIdentityStore struct {
	secrets typedcorev1.SecretInterface
	name    string
}

func NewSecretIdentityStore(c *Client, namespace, name string) (*SecretIdentityStore, error) {
	if !protocol.ValidDNSLabel(namespace) || !protocol.ValidDNSSubdomain(name) {
		return nil, errors.New("the identity Secret needs a valid namespace and name")
	}
	return &SecretIdentityStore{secrets: c.cs.CoreV1().Secrets(namespace), name: name}, nil
}

func (s *SecretIdentityStore) Load() (*client.Identity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callBudget)
	defer cancel()
	sec, err := s.secrets.Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return client.DecodeIdentity(sec.Data[identityKey])
}

// Save writes with Update after Get, creating the Secret only when it is absent. A conflict
// (or a create that lost a race) is retried once against a fresh read, then is
// ErrIdentityConflict.
func (s *SecretIdentityStore) Save(id *client.Identity) error {
	raw, err := json.Marshal(id)
	if err != nil {
		return err
	}
	for range 2 {
		err = s.write(raw, id.EndpointID)
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return ErrIdentityConflict
}

func (s *SecretIdentityStore) write(raw []byte, endpointID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), callBudget)
	defer cancel()
	sec, err := s.secrets.Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = s.secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: s.name, Labels: map[string]string{"app.kubernetes.io/name": "kyyard-agent", "app.kubernetes.io/managed-by": "kyyard"}},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{identityKey: raw},
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if prior, err := client.DecodeIdentity(sec.Data[identityKey]); err == nil && prior.EndpointID != endpointID {
		return ErrIdentityConflict
	}
	sec.Data = map[string][]byte{identityKey: raw}
	_, err = s.secrets.Update(ctx, sec, metav1.UpdateOptions{})
	return err
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `gofmt -w internal/runtime/kubernetes && go mod tidy && go vet ./... && go test -race -count=1 ./internal/runtime/... && test "$(go list -deps ./cmd/server | grep -c k8s.io)" = 0`
Expected: PASS; `go.mod` lists the three modules as direct requirements; the server still links no `k8s.io` package.

- [ ] **Step 6: DOX and commit**

Create `internal/runtime/kubernetes/AGENTS.md`:

```markdown
# Kubernetes runtime adapter

## Purpose
Reads the cluster a KyYard agent runs in, as its ServiceAccount, and returns product types only. Read-only in M8 PR 20: inventory, health input and pod logs.

## Ownership
Owns the client-go typed clientset (`New`, `InCluster`, `NewFromClientset` for the fake clientset), the mapping into `protocol.KubernetesInventory`, pod log streaming, and the agent identity Secret (`SecretIdentityStore`). No client-go type leaves this package; `cmd/agent` sees only `Client`, `SecretIdentityStore`, `OwnNamespace` and `ErrIdentityConflict`.

## Local Contracts
- `Snapshot` lists nodes, namespaces, deployments, statefulsets, daemonsets, pods, services and persistentvolumeclaims with `List` (`Limit` 500, `Continue` paging, stopping once past the cap), sorts each list by namespace then name, and leaves the cut and the byte fit to `protocol.Clamp`/`Shrink`. A list that fails is reported empty and named in `Truncated` with a log line; `Snapshot` never returns an error. `Engine` is `Runtime: kubernetes`, the API server's `GitVersion`, `Major.Minor`, and `Platform` split into OS and arch.
- Node `Ready` is the `Ready` condition; roles are the `node-role.kubernetes.io/<role>` label keys. A workload's `Desired` is `spec.replicas` (unset is 1) or, for a DaemonSet, `status.desiredNumberScheduled`. A pod's owner is its controller, except that a ReplicaSet named `<deployment>-<pod-template-hash>` is reported as that Deployment: the role cannot read ReplicaSets. Container `State` is `running`, `terminated` or `waiting` (also when no status exists yet), `Reason` the waiting or terminated reason. Service ports read `port[:nodePort]/PROTOCOL`.
- `Logs` validates `LogRequest.ValidateFor(kubernetes)`, reads the pod (`get pods`), takes the only container when none is named and refuses otherwise with `unknown_container: ...`, and streams `GetLogs(...).Stream` with `TailLines`, `SinceTime`, `Timestamps`, `Follow` and `LimitBytes` 4 MiB; it stops at `protocol.MaxLogLines` or `MaxLogBytes`, closes the stream when the context ends, and reports fixed error texts, never API error bodies. History has 60 seconds, a follow an hour.
- `SecretIdentityStore` keeps the identity JSON under key `identity.json` in an Opaque Secret in the agent's own namespace, labelled like the manifest's objects. `Save` reads, then updates, creating only when absent; a conflict (or a lost create race) is retried once against a fresh read, then is `ErrIdentityConflict`, as is a Secret holding another endpoint's identity, which is never overwritten.

## Verification
- `go test -race ./internal/runtime/kubernetes/...` runs against `k8s.io/client-go/kubernetes/fake` (mapping of every field, owner resolution, caps, a forbidden list, paging, facts, log options, container refusal, follow cancellation, the line ceiling, Secret create/update/conflict).

## Child DOX Index
None.
```

`internal/runtime/AGENTS.md`: in `## Purpose` replace "`docker` is the first; `kubernetes` follows in M8." with "`docker` for Docker hosts and `kubernetes` for clusters (M8, read-only in PR 20)."; replace the `## Child DOX Index` body `None.` with:

```markdown
- [kubernetes/AGENTS.md](kubernetes/AGENTS.md): client-go adapter for a cluster agent: inventory, pod logs and the identity Secret.
```

Root `AGENTS.md`, `## Child DOX Index`: replace the line

```markdown
- [internal/runtime/AGENTS.md](internal/runtime/AGENTS.md): Docker runtime adapters, bounded streams and exec session primitives.
```

with

```markdown
- [internal/runtime/AGENTS.md](internal/runtime/AGENTS.md): Docker and Kubernetes runtime adapters, bounded streams and exec session primitives.
```

```bash
git add go.mod go.sum internal/runtime AGENTS.md && make tidy-check lint && git commit -m "feat(runtime): Kubernetes adapter with paged inventory, pod logs and identity Secret" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: `cmd/agent --kubernetes`, `--link-file` and `--identity-secret`

**Files:**
- Modify: `cmd/agent/main.go` (whole file below)
- Test: `cmd/agent/main_test.go`
- Modify: `scripts/smoke-test.sh` (the built agent refuses `--kubernetes` with a Docker socket)
- Docs: `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: `client.IdentityStore`, `client.DirStore`, `client.Enroll(..., store, name, token, facts)`, `client.Options.Identities`, `client.Options.Kubernetes` (Task 2); `kubernetes.InCluster`, `kubernetes.OwnNamespace`, `kubernetes.NewSecretIdentityStore`, `kubernetes.ErrIdentityConflict`, `(*kubernetes.Client).Snapshot/Logs/Facts` (Task 3).
- Produces (package `main`, unexported, tested):
  ```go
  func checkFlags(kube bool, socket, link, linkFile string) error
  func readLinkFile(path string) (string, error) // "" , nil when the file does not exist
  type fatalOnConflict struct{ *kubernetes.SecretIdentityStore }
  ```
  Flags: `--kubernetes` (bool), `--link-file` (path), `--identity-secret` (default `kyyard-agent-identity`).

- [ ] **Step 1: Write the failing tests**

Create `cmd/agent/main_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckFlags(t *testing.T) {
	for _, tc := range []struct {
		kube                   bool
		socket, link, linkFile string
		want                   string
	}{
		{false, "/var/run/docker.sock", "https://y/#kyyard=x", "", ""},
		{true, "", "", "/etc/kyyard/link", ""},
		{true, "/var/run/docker.sock", "", "/etc/kyyard/link", "kubernetes and docker are exclusive"},
		{false, "/var/run/docker.sock", "https://y/#kyyard=x", "/etc/kyyard/link", "--link or --link-file"},
	} {
		err := checkFlags(tc.kube, tc.socket, tc.link, tc.linkFile)
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

// The link file is read trimmed; a missing one means no link, because the manifest mounts it
// optional and the operator deletes it once spent; anything longer than a link is refused.
func TestReadLinkFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "link")
	if link, err := readLinkFile(path); link != "" || err != nil {
		t.Fatalf("missing file: %q %v", link, err)
	}
	if err := os.WriteFile(path, []byte("https://yard.example/#kyyard=abc\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if link, err := readLinkFile(path); link != "https://yard.example/#kyyard=abc" || err != nil {
		t.Fatalf("link %q %v", link, err)
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 4097)), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := readLinkFile(big); err == nil {
		t.Fatal("an oversized link file was read")
	}
}
```

In `scripts/smoke-test.sh`, after the line

```sh
  check "refused enrollment creates no second endpoint" "$ENDPOINT_COUNT" "1"
```

insert

```sh
  KUBE_EXIT=0
  "$AGENT" --kubernetes --server "$BASE" >"$WORK/kube.log" 2>&1 || KUBE_EXIT=$?
  check "cluster agent refuses a Docker socket" "$(test "$KUBE_EXIT" -ne 0 && echo refused || echo accepted)" "refused"
  contains "refusal names the exclusive runtimes" "$(cat "$WORK/kube.log")" "kubernetes and docker are exclusive"
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./cmd/agent/`
Expected: FAIL to compile (`undefined: checkFlags`, `undefined: readLinkFile`).

- [ ] **Step 3: Wire the cluster agent**

Replace `cmd/agent/main.go` with:

```go
// kyyard-agent enrolls a Docker host or a Kubernetes cluster with a KyYard control plane and
// keeps it connected.
//
//	printf '%s\n' "$TOKEN" | kyyard-agent --server https://kyyard.example --identity-dir /var/lib/kyyard-agent
//	kyyard-agent --kubernetes --link-file /etc/kyyard/link --docker-socket=   (the generated manifest)
//
// The token is used on the first run only; afterwards the stored identity is used.
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes"
)

var version = "dev"

func main() {
	link := flag.String("link", "", "enrollment link copied from KyYard (first start only)")
	linkFile := flag.String("link-file", "", "file holding the enrollment link (first start only; a missing file means none)")
	enrollOnly := flag.Bool("enroll-only", false, "enroll, print the key fingerprint and exit before connecting")
	server := flag.String("server", "", "control plane origin, e.g. https://kyyard.example")
	dir := flag.String("identity-dir", "/var/lib/kyyard-agent", "directory holding the agent identity (0700)")
	name := flag.String("name", "", "endpoint name to propose at enrollment (default: hostname)")
	rotate := flag.Duration("rotate-every", 30*24*time.Hour, "offer a new identity key this often (0 disables)")
	socket := flag.String("docker-socket", "/var/run/docker.sock", "Docker Engine socket to inventory (empty disables)")
	inventoryEvery := flag.Duration("inventory-every", time.Minute, "how often to report a fresh inventory snapshot")
	kube := flag.Bool("kubernetes", false, "inventory the cluster this agent runs in, as its ServiceAccount (needs --docker-socket=)")
	identitySecret := flag.String("identity-secret", "kyyard-agent-identity", "with --kubernetes: the Secret in the agent's namespace that holds its identity")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := checkFlags(*kube, *socket, *link, *linkFile); err != nil {
		log.Fatal(err)
	}
	if *linkFile != "" {
		var err error
		if *link, err = readLinkFile(*linkFile); err != nil {
			log.Fatalf("enrollment link: %v", err)
		}
	}
	var linkToken string
	if *link != "" {
		if *server != "" {
			log.Fatal("use --link or --server, not both")
		}
		origin, token, err := client.ParseLink(*link)
		if err != nil {
			log.Fatal(err)
		}
		*server, linkToken = origin, token
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpClient := &http.Client{Timeout: 30 * time.Second}
	// The Docker adapter is optional at start: a host whose daemon is down still enrolls and
	// reports facts, and the snapshot call keeps retrying the socket on its own schedule.
	var snapshot func(context.Context) (*protocol.Snapshot, error)
	var metrics func(context.Context, []string) protocol.Metrics
	var operate func(context.Context, protocol.Command) (string, string)
	var logs func(context.Context, protocol.LogRequest, func([]byte) error) error
	var inspect func(context.Context, protocol.InspectionTarget) (*protocol.ContainerInspection, error)
	var exec func(context.Context, protocol.ExecSpec) (client.ExecSession, error)
	var deploy func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult
	var remove func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult
	runtimeVersion := ""
	var identities client.IdentityStore = client.DirStore(*dir)
	where := *dir
	var facts map[string]string
	if *kube {
		cluster, err := kubernetes.InCluster()
		if err != nil {
			log.Fatalf("kubernetes: %v", err)
		}
		namespace, err := kubernetes.OwnNamespace()
		if err != nil {
			log.Fatalf("kubernetes: %v", err)
		}
		secret, err := kubernetes.NewSecretIdentityStore(cluster, namespace, *identitySecret)
		if err != nil {
			log.Fatalf("identity: %v", err)
		}
		identities, where = fatalOnConflict{secret}, "Secret "+namespace+"/"+*identitySecret
		snapshot, logs = cluster.Snapshot, cluster.Logs
		facts = cluster.Facts(ctx)
	} else if *socket != "" {
		engine := docker.New(*socket)
		if facts, err := engine.Engine(ctx); err != nil {
			log.Printf("docker at %s not reachable (%v); reporting facts only until it is", *socket, err)
		} else {
			runtimeVersion = facts.Version
		}
		snapshot = engine.Snapshot
		metrics = engine.Stats
		operate = func(cctx context.Context, cmd protocol.Command) (string, string) { return engine.Operate(cctx, cmd) }
		logs = engine.Logs
		inspect = engine.InspectContainer
		deploy = engine.Deploy
		remove = engine.Remove
		exec = func(ctx context.Context, spec protocol.ExecSpec) (client.ExecSession, error) {
			return engine.OpenExec(ctx, spec)
		}
	}

	if facts == nil {
		facts = client.Facts(runtimeVersion)
	}

	id, err := identities.Load()
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	if id != nil && *enrollOnly {
		log.Fatalf("already enrolled as %s with %s; refusing a new enrollment in %s; restart without --enroll-only to reuse this identity", id.EndpointID, id.Server, where)
	}
	if id == nil {
		if *server == "" {
			log.Fatal("--server is required for enrollment")
		}
		token := linkToken
		if token == "" {
			token, err = client.ReadToken(os.Stdin)
			if err != nil {
				log.Fatalf("enrollment: %v", err)
			}
		}
		if *name == "" {
			*name, _ = os.Hostname()
		}
		id, err = client.Enroll(ctx, httpClient, *server, identities, *name, token, facts)
		if err != nil {
			log.Fatalf("enrollment: %v", err)
		}
		log.Printf("enrolled as %s; waiting for approval", id.EndpointID)
	} else if linkToken != "" && !id.MatchesEnrollment(*server, linkToken) {
		log.Fatalf("the identity in %s belongs to another enrollment; reuse the original link or clear that identity", where)
	} else if *server != "" && *server != id.Server {
		log.Fatalf("identity is enrolled with %s, not %s; remove %s to re-enroll", id.Server, *server, where)
	}
	log.Printf("agent key fingerprint: %s", protocol.Fingerprint(ed25519.PrivateKey(id.PrivateKey).Public().(ed25519.PublicKey)))
	if *enrollOnly {
		return
	}
	opts := client.Options{HTTPClient: httpClient, Version: version, Identities: identities, Kubernetes: *kube, RotateEvery: *rotate, Snapshot: snapshot, Metrics: metrics, Operate: operate, Logs: logs, Exec: exec, Inspect: inspect, Deploy: deploy, Remove: remove, InventoryEvery: *inventoryEvery}
	if !*kube {
		// The command ledger lives beside a host's identity; a cluster agent runs no commands.
		opts.IdentityDir = *dir
	}
	if err := client.Run(ctx, id, opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

// checkFlags refuses combinations that cannot mean anything.
func checkFlags(kube bool, socket, link, linkFile string) error {
	if link != "" && linkFile != "" {
		return errors.New("use --link or --link-file, not both")
	}
	if kube && socket != "" {
		return errors.New("kubernetes and docker are exclusive: pass --docker-socket= with --kubernetes")
	}
	return nil
}

// readLinkFile returns the link a mounted Secret holds, or "" when the file is absent: the
// manifest mounts that Secret optional, so a pod restarted after the operator deleted the
// spent Secret starts from its stored identity.
func readLinkFile(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	if len(raw) > 4096 {
		return "", errors.New("the link file holds more than a link")
	}
	return strings.TrimSpace(string(raw)), nil
}

// fatalOnConflict stops the agent when its identity Secret was written by someone else: two
// agents sharing one identity is the failure the Secret store exists to prevent, and the pod
// restart that follows reads the Secret afresh.
type fatalOnConflict struct {
	*kubernetes.SecretIdentityStore
}

func (f fatalOnConflict) Save(id *client.Identity) error {
	err := f.SecretIdentityStore.Save(id)
	if errors.Is(err, kubernetes.ErrIdentityConflict) {
		log.Fatalf("identity: %v", err)
	}
	return err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -w cmd/agent && go vet ./cmd/... && go test -race -count=1 ./cmd/agent/ ./internal/agent/... && make smoke`
Expected: PASS; the smoke run prints `[ok]   cluster agent refuses a Docker socket` and `[ok]   refusal names the exclusive runtimes`, then `smoke test: all checks passed`.

- [ ] **Step 5: DOX and commit**

`internal/agent/AGENTS.md`, `## Local Contracts`, append:

```markdown
- `cmd/agent --kubernetes` runs as a cluster agent: it requires `--docker-socket=` (`kubernetes and docker are exclusive`), loads the in-cluster configuration, keeps its identity in the Secret `--identity-secret` (default `kyyard-agent-identity`) in its own namespace through `kubernetes.SecretIdentityStore`, and exits on `ErrIdentityConflict` so the restarted pod reads the Secret afresh; `--identity-dir` and the command ledger are unused. `--link-file` reads the enrollment link from a file (the manifest mounts the enrollment Secret there) so the token never appears in a command line or pod spec; a missing file means no link, because the operator deletes the spent Secret. `--link` and `--link-file` are exclusive.
```

```bash
git add cmd/agent scripts/smoke-test.sh internal/agent/AGENTS.md && make tidy-check lint && git commit -m "feat(agent): run as a Kubernetes cluster agent" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: The install manifest and Kubernetes enrollment tokens

**Files:**
- Create: `internal/runtime/kubernetes/manifest/manifest.go`
- Test: `internal/runtime/kubernetes/manifest/manifest_test.go`
- Test: `internal/runtime/kubernetes/cluster_test.go` (real cluster; skips without `KY_TEST_KUBECONFIG`)
- Modify: `internal/api/endpoint_handlers.go` (`handleCreateEnrollmentToken`, two constants, import)
- Modify: `internal/store/endpoints.go` (`ValidEndpointName`)
- Test: `internal/api/enrollment_kubernetes_test.go`
- Modify: `scripts/smoke-test.sh` (an HTTP-only install refuses a cluster enrollment)
- Modify: `go.mod`, `go.sum` (`go mod tidy`: the cluster test's `clientcmd`)
- Docs: `internal/api/AGENTS.md`, `internal/runtime/kubernetes/AGENTS.md`, `internal/runtime/AGENTS.md`, root `AGENTS.md` (`## Verification`)

**Interfaces:**
- Consumes: `config.IsPinnedAgentImage`; `kubernetes.New`, `NewSecretIdentityStore`, `(*Client).Snapshot` (Task 3); `protocol.RuntimeKubernetes` (Task 1).
- Produces:
  ```go
  // package manifest (github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/manifest)
  type Input struct{ Image, Link, Name string }
  func Render(in Input) (string, error) // refuses an unpinned image, an empty name, a non-HTTPS link
  func FileName(name string) string     // kyyard-agent-<slug>.yaml
  // package store
  func ValidEndpointName(name string) bool // the rule Enroll and RenameEndpoint apply
  ```
  HTTP: `POST /api/organizations/{organization}/environments/{environment}/enrollment-tokens` accepts `{runtime: "kubernetes", name}` (name required for Kubernetes, refused for Docker, 400) and answers 201 with the existing fields plus `manifest`, `manifest_file`, `command` (`kubectl apply -f <manifest_file>`), the cluster `disclosure` and `note`; 409 `https_required` or `agent_image_unpinned` before any token is minted.

- [ ] **Step 1: Write the failing tests**

Create `internal/runtime/kubernetes/manifest/manifest_test.go`:

```go
package manifest_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/manifest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
)

const (
	image = "ghcr.io/busnes-app/kyyard@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	token = "AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKK"
	link  = "https://yard.example/#kyyard=" + token
	name  = `prod "east": #1`
)

// decode reads every document strictly into its typed object, so a misspelt field fails here
// rather than being dropped by the API server.
func decode(t *testing.T, doc string) []runtime.Object {
	t.Helper()
	d := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer()
	var out []runtime.Object
	for _, part := range strings.Split(doc, "\n---\n") {
		obj, _, err := d.Decode([]byte(part), nil, nil)
		if err != nil {
			t.Fatalf("document does not decode: %v\n%s", err, part)
		}
		out = append(out, obj)
	}
	return out
}

func render(t *testing.T) (string, []runtime.Object) {
	t.Helper()
	doc, err := manifest.Render(manifest.Input{Image: image, Link: link, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return doc, decode(t, doc)
}

// The RBAC is exactly the read subset: get and list only, no Secrets or ConfigMaps, no watch
// and no wildcard; the only Secret access is the namespaced identity Role.
func TestManifestRBACIsReadOnlyWithoutSecrets(t *testing.T) {
	_, objects := render(t)
	var kinds []string
	var clusterRole *rbacv1.ClusterRole
	var role *rbacv1.Role
	for _, obj := range objects {
		kinds = append(kinds, obj.GetObjectKind().GroupVersionKind().Kind)
		switch o := obj.(type) {
		case *rbacv1.ClusterRole:
			clusterRole = o
		case *rbacv1.Role:
			role = o
		case *rbacv1.ClusterRoleBinding:
			if o.RoleRef.Name != "kyyard-agent-read" || len(o.Subjects) != 1 || o.Subjects[0].Kind != "ServiceAccount" || o.Subjects[0].Name != "kyyard-agent" || o.Subjects[0].Namespace != "kyyard-agent" {
				t.Fatalf("cluster role binding %+v", o)
			}
		case *rbacv1.RoleBinding:
			if o.Namespace != "kyyard-agent" || o.RoleRef.Name != "kyyard-agent-identity" || len(o.Subjects) != 1 || o.Subjects[0].Name != "kyyard-agent" {
				t.Fatalf("role binding %+v", o)
			}
		}
	}
	if !slices.Equal(kinds, []string{"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "Secret", "Deployment"}) {
		t.Fatalf("kinds %v", kinds)
	}
	var granted []string
	for _, rule := range clusterRole.Rules {
		if !slices.Equal(rule.Verbs, []string{"get", "list"}) || len(rule.ResourceNames) != 0 || len(rule.NonResourceURLs) != 0 {
			t.Fatalf("cluster rule %+v", rule)
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				granted = append(granted, group+"/"+resource)
			}
		}
	}
	slices.Sort(granted)
	want := []string{"/events", "/namespaces", "/nodes", "/persistentvolumeclaims", "/pods", "/pods/log", "/services", "apps/daemonsets", "apps/deployments", "apps/statefulsets"}
	if !slices.Equal(granted, want) {
		t.Fatalf("cluster role grants %v", granted)
	}
	for _, g := range granted {
		if strings.Contains(g, "*") || strings.HasSuffix(g, "/secrets") || strings.HasSuffix(g, "/configmaps") {
			t.Fatalf("cluster role grants %s", g)
		}
	}
	if role.Namespace != "kyyard-agent" || len(role.Rules) != 2 {
		t.Fatalf("role %+v", role)
	}
	open, named := role.Rules[0], role.Rules[1]
	if !slices.Equal(open.Resources, []string{"secrets"}) || !slices.Equal(open.Verbs, []string{"get", "create"}) || len(open.ResourceNames) != 0 {
		t.Fatalf("identity rule %+v", open)
	}
	if !slices.Equal(named.Resources, []string{"secrets"}) || !slices.Equal(named.Verbs, []string{"update"}) || !slices.Equal(named.ResourceNames, []string{"kyyard-agent-identity"}) {
		t.Fatalf("identity update rule %+v", named)
	}
}

// The token appears exactly once, inside the enrollment Secret; the image is the pinned digest;
// the Deployment runs one locked-down replica with the Secret mounted read-only.
func TestManifestCarriesTheTokenOnceAndALockedDownAgent(t *testing.T) {
	doc, objects := render(t)
	if n := strings.Count(doc, token); n != 1 {
		t.Fatalf("the token appears %d times", n)
	}
	for _, obj := range objects {
		switch o := obj.(type) {
		case *corev1.Secret:
			if o.Name != "kyyard-agent-enrollment" || o.Namespace != "kyyard-agent" || o.StringData["link"] != link {
				t.Fatalf("secret %+v", o)
			}
		case *appsv1.Deployment:
			spec := o.Spec.Template.Spec
			c := spec.Containers[0]
			if *o.Spec.Replicas != 1 || o.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || spec.ServiceAccountName != "kyyard-agent" || !*spec.AutomountServiceAccountToken {
				t.Fatalf("deployment %+v", o.Spec)
			}
			if c.Image != image || !config.IsPinnedAgentImage(c.Image) || !slices.Equal(c.Command, []string{"/app/kyyard-agent"}) {
				t.Fatalf("container %+v", c)
			}
			if !slices.Equal(c.Args, []string{"--kubernetes", "--link-file", "/etc/kyyard/link", "--identity-secret", "kyyard-agent-identity", "--name", name, "--docker-socket="}) {
				t.Fatalf("args %q", c.Args)
			}
			sc, psc := c.SecurityContext, spec.SecurityContext
			if !*psc.RunAsNonRoot || *psc.RunAsUser != 65532 || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
				t.Fatalf("security context %+v %+v", psc, sc)
			}
			if c.Resources.Requests.Cpu().String() != "50m" || c.Resources.Requests.Memory().String() != "64Mi" || c.Resources.Limits.Cpu().String() != "500m" || c.Resources.Limits.Memory().String() != "256Mi" {
				t.Fatalf("resources %+v", c.Resources)
			}
			if m := c.VolumeMounts[0]; m.MountPath != "/etc/kyyard" || !m.ReadOnly || spec.Volumes[0].Secret.SecretName != "kyyard-agent-enrollment" || !*spec.Volumes[0].Secret.Optional {
				t.Fatalf("mount %+v %+v", m, spec.Volumes[0])
			}
		}
		labels := obj.(interface{ GetLabels() map[string]string }).GetLabels()
		if labels["app.kubernetes.io/name"] != "kyyard-agent" || labels["app.kubernetes.io/managed-by"] != "kyyard" {
			t.Fatalf("%T labels %v", obj, labels)
		}
	}
}

func TestManifestRefusesAnUnpinnedImage(t *testing.T) {
	for _, in := range []manifest.Input{
		{Image: "ghcr.io/busnes-app/kyyard:latest", Link: link, Name: "c"},
		{Image: image, Link: "http://yard.example/#kyyard=" + token, Name: "c"},
		{Image: image, Link: link},
	} {
		if _, err := manifest.Render(in); err == nil {
			t.Fatalf("rendered %+v", in)
		}
	}
}

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"prod-cluster":  "kyyard-agent-prod-cluster.yaml",
		`prod "east" 1`: "kyyard-agent-prod-east-1.yaml",
		"Ünïcode/../x":  "kyyard-agent-n-code-x.yaml",
		"$(rm -rf /)":   "kyyard-agent-rm-rf.yaml",
		"***":           "kyyard-agent-endpoint.yaml",
	} {
		if got := manifest.FileName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
```

Create `internal/runtime/kubernetes/cluster_test.go`:

```go
package kubernetes_test

import (
	"context"
	"crypto/ed25519"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/manifest"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// TestManifestOnARealCluster applies the rendered manifest to the cluster KY_TEST_KUBECONFIG
// names (a disposable kind cluster: it creates and deletes cluster-scoped RBAC), then acts as
// the agent's ServiceAccount: Secrets are denied cluster-wide, pods are listed, the identity
// Secret round-trips, and a snapshot names the cluster's nodes with nothing forbidden.
func TestManifestOnARealCluster(t *testing.T) {
	path := os.Getenv("KY_TEST_KUBECONFIG")
	if path == "" {
		t.Skip("KY_TEST_KUBECONFIG is not set")
	}
	admin, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal(err)
	}
	cs := k8s.NewForConfigOrDie(admin)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	removeAgent(t, ctx, cs)
	t.Cleanup(func() { removeAgent(t, context.Background(), cs) })

	doc, err := manifest.Render(manifest.Input{Image: "ghcr.io/busnes-app/kyyard@sha256:" + strings.Repeat("0", 64), Link: "https://kyyard.invalid/#kyyard=" + strings.Repeat("A", 43), Name: "kind"})
	if err != nil {
		t.Fatal(err)
	}
	decoder := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer()
	for _, part := range strings.Split(doc, "\n---\n") {
		obj, _, err := decoder.Decode([]byte(part), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		switch o := obj.(type) {
		case *corev1.Namespace:
			_, err = cs.CoreV1().Namespaces().Create(ctx, o, metav1.CreateOptions{})
		case *corev1.ServiceAccount:
			_, err = cs.CoreV1().ServiceAccounts(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.ClusterRole:
			_, err = cs.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.ClusterRoleBinding:
			_, err = cs.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.Role:
			_, err = cs.RbacV1().Roles(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.RoleBinding:
			_, err = cs.RbacV1().RoleBindings(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
		case *corev1.Secret:
			_, err = cs.CoreV1().Secrets(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
		case *appsv1.Deployment:
			_, err = cs.AppsV1().Deployments(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
		default:
			t.Fatalf("unexpected %T", obj)
		}
		if err != nil {
			t.Fatalf("apply %T: %v", obj, err)
		}
	}

	agent := rest.CopyConfig(admin)
	agent.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:kyyard-agent:kyyard-agent"}
	as := k8s.NewForConfigOrDie(agent)
	for _, tc := range []struct {
		verb, resource, namespace, name string
		allowed                         bool
	}{
		{"get", "secrets", "", "", false},
		{"get", "secrets", "default", "", false},
		{"list", "secrets", "kube-system", "", false},
		{"list", "configmaps", "", "", false},
		{"watch", "pods", "", "", false},
		{"delete", "pods", "default", "", false},
		{"list", "pods", "", "", true},
		{"get", "pods/log", "default", "", true},
		{"update", "secrets", "kyyard-agent", "kyyard-agent-identity", true},
		{"update", "secrets", "kyyard-agent", "kyyard-agent-enrollment", false},
	} {
		attrs := &authorizationv1.ResourceAttributes{Verb: tc.verb, Namespace: tc.namespace, Name: tc.name}
		attrs.Resource, attrs.Subresource, _ = strings.Cut(tc.resource, "/")
		review, err := as.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attrs}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if review.Status.Allowed != tc.allowed {
			t.Errorf("%s %s in %q (%s): allowed=%v", tc.verb, tc.resource, tc.namespace, tc.name, review.Status.Allowed)
		}
	}

	c, err := kubernetes.New(agent)
	if err != nil {
		t.Fatal(err)
	}
	store, err := kubernetes.NewSecretIdentityStore(c, "kyyard-agent", "kyyard-agent-identity")
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	if err := store.Save(&client.Identity{EndpointID: "ep_kind", PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: "https://kyyard.invalid", Generation: 1}); err != nil {
		t.Fatalf("identity create: %v", err)
	}
	if err := store.Save(&client.Identity{EndpointID: "ep_kind", PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: "https://kyyard.invalid", Generation: 2}); err != nil {
		t.Fatalf("identity update: %v", err)
	}
	if got, err := store.Load(); err != nil || got.Generation != 2 {
		t.Fatalf("identity load: %+v %v", got, err)
	}
	snap, err := c.Snapshot(ctx)
	if err != nil || len(snap.Truncated) != 0 || len(snap.Kubernetes.Nodes) == 0 || snap.Kubernetes.Nodes[0].Name == "" {
		t.Fatalf("snapshot as the agent: %v truncated %v nodes %+v", err, snap.Truncated, snap.Kubernetes.Nodes)
	}
}

// removeAgent deletes what the manifest creates and waits for the namespace to go, so a run
// that died halfway does not break the next one.
func removeAgent(t *testing.T, ctx context.Context, cs k8s.Interface) {
	t.Helper()
	ignore := func(err error) {
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("cleanup: %v", err)
		}
	}
	ignore(cs.RbacV1().ClusterRoleBindings().Delete(ctx, "kyyard-agent-read", metav1.DeleteOptions{}))
	ignore(cs.RbacV1().ClusterRoles().Delete(ctx, "kyyard-agent-read", metav1.DeleteOptions{}))
	ignore(cs.CoreV1().Namespaces().Delete(ctx, "kyyard-agent", metav1.DeleteOptions{}))
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := cs.CoreV1().Namespaces().Get(ctx, "kyyard-agent", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("namespace kyyard-agent still present: %v", err)
		}
		time.Sleep(time.Second)
	}
}
```

Create `internal/api/enrollment_kubernetes_test.go`:

```go
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// A Kubernetes token comes with the manifest and its kubectl command, the token inside the
// manifest once, and the RBAC disclosure; a cluster must be named, a Docker host must not be.
func TestKubernetesEnrollmentReturnsTheManifest(t *testing.T) {
	t.Setenv("KY_AGENT_IMAGE", agentImage)
	s, st, cfg := setupTestServer(t)
	cfg.Server.AppURL = "https://yard.example"
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}); err != nil {
		t.Fatal(err)
	}
	admin := loginAs(t, s, st, "envadmin", "user")
	viewer := loginAs(t, s, st, "viewer", "user")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	tokens := "/api/organizations/a/environments/env-a/enrollment-tokens"
	mint := func(c *http.Cookie, body string, status int) string {
		t.Helper()
		w := tenantRequest(s, c, "POST", tokens, body, true)
		if w.Code != status {
			t.Fatalf("%s: %d %s, want %d", body, w.Code, w.Body.String(), status)
		}
		return w.Body.String()
	}
	mint(admin, `{"runtime":"kubernetes"}`, 400)
	mint(admin, `{"runtime":"kubernetes","name":"bad\nname"}`, 400)
	mint(admin, `{"runtime":"docker","name":"host"}`, 400)
	mint(viewer, `{"runtime":"kubernetes","name":"prod"}`, 403)

	var out struct {
		Token, Command, Disclosure, Image, Note, Manifest string
		File                                              string `json:"manifest_file"`
	}
	if err := json.Unmarshal([]byte(mint(admin, `{"runtime":"kubernetes","name":"prod \"east\""}`, 201)), &out); err != nil {
		t.Fatal(err)
	}
	if out.File != "kyyard-agent-prod-east.yaml" || out.Command != "kubectl apply -f kyyard-agent-prod-east.yaml" || out.Image != agentImage {
		t.Fatalf("command %q file %q image %q", out.Command, out.File, out.Image)
	}
	if strings.Count(out.Manifest, out.Token) != 1 || !strings.Contains(out.Manifest, "image: \""+agentImage+"\"") || !strings.Contains(out.Manifest, `"--name", "prod \"east\""`) {
		t.Fatalf("manifest:\n%s", out.Manifest)
	}
	if !strings.Contains(out.Disclosure, "cannot read Secrets") || !strings.Contains(out.Disclosure, "cluster-admin") || !strings.Contains(out.Note, "delete secret kyyard-agent-enrollment") {
		t.Fatalf("disclosure %q note %q", out.Disclosure, out.Note)
	}

	// No manifest without a pinned image or an HTTPS address; a member who may not enroll
	// learns neither.
	cfg.Server.AgentImage, cfg.Server.DockerSocket = "", ""
	if body := mint(admin, `{"runtime":"kubernetes","name":"prod"}`, 409); !strings.Contains(body, "agent_image_unpinned") {
		t.Fatalf("unpinned: %s", body)
	}
	mint(viewer, `{"runtime":"kubernetes","name":"prod"}`, 403)
	cfg.Server.AgentImage, cfg.Server.AppURL = agentImage, "http://yard.example"
	if body := mint(admin, `{"runtime":"kubernetes","name":"prod"}`, 409); !strings.Contains(body, "https_required") {
		t.Fatalf("http: %s", body)
	}
}
```

In `scripts/smoke-test.sh`, after the line

```sh
  contains "refusal names the exclusive runtimes" "$(cat "$WORK/kube.log")" "kubernetes and docker are exclusive"
```

insert

```sh
  check "cluster enrollment needs HTTPS" \
    "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{"runtime":"kubernetes","name":"smoke-cluster"}' "$BASE/api/organizations/org_initial/environments/$ENV_ID/enrollment-tokens")" "409"
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/runtime/kubernetes/... ./internal/api/ -run 'TestManifest|TestFileName|TestKubernetesEnrollment'`
Expected: FAIL to compile: the `manifest` package has no non-test Go files yet (and `go.sum` lacks the modules `clientcmd` needs until Step 5's `go mod tidy`). Once Step 3 exists but not Step 4, `TestKubernetesEnrollmentReturnsTheManifest` fails with `{"runtime":"kubernetes"}: 201 ..., want 400`.

- [ ] **Step 3: Implement the manifest**

Create `internal/runtime/kubernetes/manifest/manifest.go`:

```go
// Package manifest renders the one file an administrator applies to enroll a Kubernetes
// cluster. It imports no Kubernetes library: the server links this package, and the adapter's
// client-go stays in the agent.
package manifest

import (
	"encoding/json"
	"errors"
	"strings"
	"text/template"

	"github.com/Busnes-app/kyyard-server/internal/config"
)

// Input is everything the manifest varies on. Link carries the single-use token; it appears
// once, in the enrollment Secret.
type Input struct {
	Image string
	Link  string
	Name  string
}

// Render returns the multi-document YAML. The image must be digest-pinned: the same rule the
// Docker command follows, because a tag can move after the administrator reviewed it.
func Render(in Input) (string, error) {
	if !config.IsPinnedAgentImage(in.Image) {
		return "", errors.New("the agent image is not pinned to a digest")
	}
	if in.Name == "" || !strings.HasPrefix(in.Link, "https://") {
		return "", errors.New("a manifest needs an endpoint name and an HTTPS enrollment link")
	}
	var b strings.Builder
	err := manifestTemplate.Execute(&b, map[string]string{"Image": in.Image, "Link": in.Link, "Name": in.Name, "File": FileName(in.Name)})
	return b.String(), err
}

// FileName is kyyard-agent-<name>.yaml with the name reduced to lower-case letters, digits and
// single hyphens, so the kubectl command is safe to paste into any shell.
func FileName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	slug := strings.TrimSuffix(b.String(), "-")
	if slug == "" {
		slug = "endpoint"
	}
	return "kyyard-agent-" + slug + ".yaml"
}

// quote makes any string one YAML double-quoted scalar: JSON string escapes are a subset of
// YAML's, so a name holding quotes, colons or a hash cannot become structure.
func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

var manifestTemplate = template.Must(template.New("manifest").Funcs(template.FuncMap{"q": quote}).Parse(`# KyYard agent for endpoint {{q .Name}}.
# Apply as cluster-admin: kubectl apply -f {{.File}}
# The agent reads get/list on namespaces, nodes, pods, pod logs, events, services,
# persistentvolumeclaims, deployments, statefulsets and daemonsets in every namespace. It cannot
# read Secrets or ConfigMaps; in its own namespace it may read and write its identity Secret.
# The Secret kyyard-agent-enrollment holds a single-use enrollment link. Once the endpoint is
# approved, delete it: kubectl -n kyyard-agent delete secret kyyard-agent-enrollment
# Uninstall: kubectl delete -f {{.File}}
apiVersion: v1
kind: Namespace
metadata:
  name: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kyyard-agent
  namespace: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kyyard-agent-read
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
rules:
  - apiGroups: [""]
    resources: [namespaces, nodes, pods, pods/log, events, services, persistentvolumeclaims]
    verbs: [get, list]
  - apiGroups: [apps]
    resources: [deployments, statefulsets, daemonsets]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kyyard-agent-read
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kyyard-agent-read
subjects:
  - kind: ServiceAccount
    name: kyyard-agent
    namespace: kyyard-agent
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kyyard-agent-identity
  namespace: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
rules:
  # create cannot be limited by name; the namespace holds nothing but the agent's own Secrets.
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, create]
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [kyyard-agent-identity]
    verbs: [update]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kyyard-agent-identity
  namespace: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kyyard-agent-identity
subjects:
  - kind: ServiceAccount
    name: kyyard-agent
    namespace: kyyard-agent
---
apiVersion: v1
kind: Secret
metadata:
  name: kyyard-agent-enrollment
  namespace: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
type: Opaque
stringData:
  link: {{q .Link}}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kyyard-agent
  namespace: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
spec:
  # One replica, replaced rather than rolled: two agents would share one identity.
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: kyyard-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: kyyard-agent
        app.kubernetes.io/managed-by: kyyard
    spec:
      serviceAccountName: kyyard-agent
      automountServiceAccountToken: true
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: agent
          image: {{q .Image}}
          command: ["/app/kyyard-agent"]
          args: ["--kubernetes", "--link-file", "/etc/kyyard/link", "--identity-secret", "kyyard-agent-identity", "--name", {{q .Name}}, "--docker-socket="]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 256Mi
          volumeMounts:
            - name: enrollment
              mountPath: /etc/kyyard
              readOnly: true
      volumes:
        - name: enrollment
          secret:
            secretName: kyyard-agent-enrollment
            # Optional: the Secret is deleted once spent, and a restart must not wait for it.
            optional: true
            defaultMode: 0440
`))
```

- [ ] **Step 4: Mint Kubernetes tokens with the manifest**

In `internal/store/endpoints.go`, after `func validRuntime(r string) bool { return r == "docker" || r == "kubernetes" }`, add:

```go

// ValidEndpointName is the rule Enroll and RenameEndpoint apply to an endpoint's name.
func ValidEndpointName(name string) bool { return validTenantName(name) }
```

In `internal/api/endpoint_handlers.go`, add the import `"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/manifest"` after `"github.com/Busnes-app/kyyard-server/internal/runtime/docker"`; after the `socketDisclosure` constant add:

```go
// clusterDisclosure says what the manifest grants, in the same response as the token.
const clusterDisclosure = "The KyYard agent's ServiceAccount can get and list namespaces, nodes, pods, pod logs, events, services, persistent volume claims, deployments, statefulsets and daemonsets in every namespace. It cannot read Secrets or ConfigMaps; in its own namespace kyyard-agent it reads and writes only its identity Secret. Applying the manifest needs cluster-admin, because it creates a ClusterRole and a ClusterRoleBinding."

const clusterNote = "Save the manifest and apply it with a cluster-admin kubeconfig. Run kubectl -n kyyard-agent logs deploy/kyyard-agent and compare the agent key fingerprint before approving. Once approved, delete the spent enrollment Secret: kubectl -n kyyard-agent delete secret kyyard-agent-enrollment. Uninstall with kubectl delete -f on the same file."
```

and replace the whole `handleCreateEnrollmentToken` function with:

```go
func (s *Server) handleCreateEnrollmentToken(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		Runtime string `json:"runtime"`
		Name    string `json:"name"`
	}
	if err := strictJSON(r, &input); err != nil {
		s.tenantError(w, err)
		return
	}
	// A pod has no useful hostname, so a cluster is named here; a Docker host names itself.
	kube := input.Runtime == protocol.RuntimeKubernetes
	if kube != (input.Name != "") || (kube && !store.ValidEndpointName(input.Name)) {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	https := strings.HasPrefix(s.config.Server.AppURL, "https://")
	image := s.config.Server.AgentImage
	discover := image == "" && https && s.config.Server.DockerSocket != ""
	// Authorize before touching Docker or saying anything about this server's configuration.
	if kube || discover {
		if err := s.store.Tenancy().CheckEnrollmentAccess(r.Context(), a); err != nil {
			s.tenantError(w, err)
			return
		}
	}
	if discover {
		// Use bytes already installed by the operator, not a registry tag that can move.
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		host, _ := os.Hostname()
		digests, _ := docker.New(s.config.Server.DockerSocket).ContainerImageDigests(ctx, host)
		cancel()
		for _, digest := range digests {
			if strings.HasPrefix(digest, "ghcr.io/busnes-app/kyyard@sha256:") && config.IsPinnedAgentImage(digest) {
				image = digest
				break
			}
		}
	}
	// A manifest is the only thing a cluster enrollment hands out, so one that cannot be
	// rendered is refused before a token is minted.
	if kube && !https {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Kubernetes enrollment needs KY_APP_URL on HTTPS", "code": "https_required"})
		return
	}
	if kube && image == "" {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Set KY_AGENT_IMAGE to a digest-pinned ghcr.io/busnes-app/kyyard@sha256:<digest> reference", "code": "agent_image_unpinned"})
		return
	}
	tok, err := s.store.Tenancy().CreateEnrollmentToken(r.Context(), a, input.Runtime, image)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	secret := base64.RawURLEncoding.EncodeToString(tok.Secret)
	out := map[string]any{
		"id": tok.ID, "environment_id": tok.EnvironmentID, "runtime": tok.Runtime, "expires_at": tok.ExpiresAt,
		"token": secret, "disclosure": socketDisclosure,
	}
	out["image"] = image
	if kube {
		doc, err := manifest.Render(manifest.Input{Image: image, Link: strings.TrimRight(s.config.Server.AppURL, "/") + "/#kyyard=" + secret, Name: input.Name})
		if err != nil {
			s.tenantError(w, err)
			return
		}
		file := manifest.FileName(input.Name)
		out["manifest"], out["manifest_file"], out["command"] = doc, file, "kubectl apply -f "+file
		out["disclosure"], out["note"] = clusterDisclosure, clusterNote
		s.writeJSON(w, http.StatusCreated, out)
		return
	}
	if image != "" && https {
		link := strings.TrimRight(s.config.Server.AppURL, "/") + "/#kyyard=" + secret
		out["command"] = fmt.Sprintf("sudo docker run -d --name kyyard-agent --restart unless-stopped --pull always --no-healthcheck --entrypoint /app/kyyard-agent -v /var/run/docker.sock:/var/run/docker.sock -v kyyard-agent-identity:/var/lib/kyyard-agent %s --link %s --name \"$(hostname)\"", shellQuote(image), shellQuote(link))
		out["note"] = "Run on the remote Docker host. This pulls the image, enrolls and keeps the agent running. Omit sudo if your account already has Docker access. Run sudo docker logs kyyard-agent and compare the agent key fingerprint before approving. Keep the identity volume for restarts."
	} else if https {
		out["note"] = "Could not identify a published digest for this server image. Set KY_AGENT_IMAGE to a verified ghcr.io/busnes-app/kyyard@sha256:<digest> reference, then generate a new command. Source builds and custom container hostnames need this explicit image setting."
	} else {
		out["note"] = "Remote setup needs a reachable HTTPS address. Configure KY_APP_URL and your trusted reverse proxy, then generate a new command. Local Docker connects automatically; no local enrollment command is needed."
	}

	s.writeJSON(w, http.StatusCreated, out)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `gofmt -w internal && go mod tidy && go vet ./... && go test -race -count=1 ./internal/runtime/... && go test -count=1 -run 'TestKubernetesEnrollment|TestEnrollment' ./internal/api/ && test "$(go list -deps ./cmd/server | grep -c k8s.io)" = 0 && make smoke`
Expected: PASS; `TestManifestOnARealCluster` reports SKIP; the smoke run prints `[ok]   cluster enrollment needs HTTPS (409)`; the server links no `k8s.io` package.

With a disposable cluster (for example `kind create cluster --name kyyard-it`), also run: `KY_TEST_KUBECONFIG=$HOME/.kube/config go test -count=1 -run TestManifestOnARealCluster -v ./internal/runtime/kubernetes/`
Expected: PASS (not SKIP). If no cluster is available, record in the commit body that the real-cluster RBAC test is unproven; it is not in CI.

- [ ] **Step 6: DOX and commit**

`internal/api/AGENTS.md`, `## Local Contracts`, append:

```markdown
- `POST .../enrollment-tokens` with `{runtime: "kubernetes", name}` (name required and `store.ValidEndpointName`; a Docker request with a name is 400) authorizes enrollment first, then refuses before minting with 409 `https_required` (no HTTPS `KY_APP_URL`) or 409 `agent_image_unpinned` (no digest-pinned image, configured or discovered), and otherwise answers 201 with `manifest` (`manifest.Render`: the enrollment link only inside the Secret `kyyard-agent-enrollment`), `manifest_file`, `command` (`kubectl apply -f <manifest_file>`), the cluster disclosure and note. No unauthenticated manifest route exists: the token is a bearer and its only copy leaves in this authenticated response.
```

`internal/runtime/kubernetes/AGENTS.md`, `## Local Contracts`, append:

```markdown
- `manifest/` renders the install manifest with `text/template` and JSON-quoted scalars and imports no `k8s.io` package, so the server that renders it links no client-go. `Render` refuses an unpinned image (`config.IsPinnedAgentImage`), an empty name and a non-HTTPS link; `FileName` reduces the name to `kyyard-agent-<slug>.yaml`. The objects, RBAC and pod settings are the spec's; the pod also runs as UID/GID/fsGroup 65532 (the image's default user is root) and mounts the enrollment Secret read-only with mode 0440, `optional: true`.
```

and to its `## Verification`:

```markdown
- `go test ./internal/runtime/kubernetes/manifest/` decodes every rendered document strictly into client-go types and asserts the RBAC (get/list only, no secrets, configmaps, watch or wildcard; the only Secret access is the namespaced identity Role), the pinned image, the token appearing once inside the Secret, and the pod's settings.
- `KY_TEST_KUBECONFIG=<kubeconfig of a disposable cluster> go test -run TestManifestOnARealCluster ./internal/runtime/kubernetes/` applies the manifest, impersonates the ServiceAccount (SelfSubjectAccessReview: `get secrets` denied cluster-wide, `list pods` allowed, identity Secret update allowed only by name), round-trips the identity Secret and takes a snapshot naming the nodes. It creates and deletes cluster-scoped RBAC and the `kyyard-agent` namespace before and after; it skips without the variable and is not in CI.
```

`internal/runtime/AGENTS.md`, `## Child DOX Index`: replace "inventory, pod logs and the identity Secret." with "inventory, pod logs, the identity Secret and the install manifest.".

Root `AGENTS.md`, `## Verification`, append to the bullet list above "Run the same checks locally":

```markdown
- Kubernetes: `go test ./internal/runtime/kubernetes/...` runs against the fake clientset in CI; the real-cluster manifest and RBAC test (`TestManifestOnARealCluster`) runs only with `KY_TEST_KUBECONFIG` set, locally, against a disposable cluster.
```

```bash
git add go.mod go.sum internal scripts/smoke-test.sh AGENTS.md && make tidy-check lint && git commit -m "feat(api): Kubernetes enrollment manifest" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Server — capability and snapshot rules, `runtime_unsupported`, `cluster_health`

**Files:**
- Modify: `internal/api/agent_connect.go` (`agentConn.runtime`, hello rule, snapshot rule)
- Modify: `internal/store/store.go` (`ErrRuntimeUnsupported`), `internal/api/tenant_handlers.go` (409 mapping)
- Create: `internal/api/runtime_gate.go` (`dockerOnly`)
- Modify: `internal/api/endpoint_handlers.go`, `inspection.go`, `exec_handlers.go`, `log_handlers.go`, `application_handlers.go` (the gates)
- Modify: `internal/store/models.go` (`Endpoint.ClusterHealth`), `internal/store/endpoints.go` (`decorate`)
- Modify: `internal/api/agent_connect_test.go` (`redeemToken`, `enrollClusterAgent`), `internal/api/deployment_plan_test.go` (`planHost.cfg`)
- Test: `internal/api/runtime_rules_test.go`
- Docs: `internal/api/AGENTS.md`, `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.CapabilitiesFit`, `protocol.CheckRuntimeShape`, `protocol.ClusterHealth`, `protocol.Health*`, `protocol.Runtime*` (Task 1).
- Produces:
  ```go
  // package store
  var ErrRuntimeUnsupported error // tenantError: 409 {"code":"runtime_unsupported"}
  // Endpoint gains: ClusterHealth string `json:"cluster_health,omitempty"` (Kubernetes endpoints only)
  // package api (unexported)
  func (s *Server) dockerOnly(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpointID string) bool
  // package api_test helpers
  func redeemToken(t *testing.T, s *api.Server, secret, name string) enrolledAgent
  func enrollClusterAgent(t *testing.T, s *api.Server, st store.Store, actor, name string) enrolledAgent
  type runtimeFleet struct{ s *api.Server; st store.Store; admin *http.Cookie; url string; cluster, host enrolledAgent }
  func newRuntimeFleet(t *testing.T) runtimeFleet
  // planHost gains: cfg *config.Config
  ```

- [ ] **Step 1: Prepare the test harness**

In `internal/api/agent_connect_test.go`, inside `enrollAgent`, replace

```go
	var minted struct{ Token string }
	_ = json.Unmarshal(w.Body.Bytes(), &minted)
	token, _ := base64.RawURLEncoding.DecodeString(minted.Token)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body, _ := json.Marshal(map[string]any{"token": minted.Token,
```

with

```go
	var minted struct{ Token string }
	_ = json.Unmarshal(w.Body.Bytes(), &minted)
	return redeemToken(t, s, minted.Token, name)
}

// enrollClusterAgent mints a Kubernetes token in the store as actor (the route renders a
// manifest, which needs an HTTPS address and a pinned image the test server lacks) and redeems
// it through the real route.
func enrollClusterAgent(t *testing.T, s *api.Server, st store.Store, actor, name string) enrolledAgent {
	t.Helper()
	tok, err := st.Tenancy().CreateEnrollmentToken(context.Background(), store.TenantAccess{ActorID: actor, OrganizationID: "a", EnvironmentID: "env-a"}, protocol.RuntimeKubernetes, "")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return redeemToken(t, s, base64.RawURLEncoding.EncodeToString(tok.Secret), name)
}

// redeemToken enrolls a fresh key with a minted token through the real route.
func redeemToken(t *testing.T, s *api.Server, secret, name string) enrolledAgent {
	t.Helper()
	token, _ := base64.RawURLEncoding.DecodeString(secret)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body, _ := json.Marshal(map[string]any{"token": secret,
```

(the rest of the old function body, from `r := httptest.NewRequest("POST", "/api/agent/v1/enroll", ...` to its `return enrolledAgent{...}`, is now `redeemToken`'s).

In `internal/api/deployment_plan_test.go`: add `"github.com/Busnes-app/kyyard-server/internal/config"` to the imports; in `type planHost` add the field `cfg         *config.Config` after `st          store.Store`; in `newPlanHost` replace `s, st, _ := setupTestServer(t)` with `s, st, cfg := setupTestServer(t)` and `h := planHost{s: s, st: st, admin: loginAs(t, s, st, "planner", "user")}` with `h := planHost{s: s, st: st, cfg: cfg, admin: loginAs(t, s, st, "planner", "user")}`.

- [ ] **Step 2: Write the failing tests**

Create `internal/api/runtime_rules_test.go`:

```go
package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

// runtimeFleet is organization a with one approved Kubernetes endpoint and one approved Docker
// endpoint, served over a real socket.
type runtimeFleet struct {
	s       *api.Server
	st      store.Store
	admin   *http.Cookie
	url     string
	cluster enrolledAgent
	host    enrolledAgent
}

func newRuntimeFleet(t *testing.T) runtimeFleet {
	t.Helper()
	s, st, _ := setupTestServer(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}); err != nil {
		t.Fatal(err)
	}
	admin := loginAs(t, s, st, "envadmin", "user")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(s)
	t.Cleanup(httpSrv.Close)
	f := runtimeFleet{s: s, st: st, admin: admin, url: httpSrv.URL}
	f.cluster = enrollClusterAgent(t, s, st, "usr_envadmin", "cluster-1")
	f.host = enrollAgent(t, s, st, admin, "host-1")
	for _, ag := range []enrolledAgent{f.cluster, f.host} {
		if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
			t.Fatalf("approve: %d %s", w.Code, w.Body.String())
		}
	}
	return f
}

func (f runtimeFleet) endpoint(t *testing.T, id string) map[string]any {
	t.Helper()
	w := tenantRequest(f.s, f.admin, "GET", "/api/organizations/a/endpoints/"+id, "", false)
	var out map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("endpoint: %d %s", w.Code, w.Body.String())
	}
	return out
}

// expectClose reads until the server closes the socket and returns the close reason.
func expectClose(t *testing.T, ctx context.Context, c *websocket.Conn) string {
	t.Helper()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if errorsAs(err, &ce) {
				return ce.Reason
			}
			t.Fatalf("read: %v", err)
		}
	}
}

// A hello naming a capability outside the endpoint's runtime is answered capability_mismatch
// and closed; a hello that fits is stored.
func TestHelloCapabilitiesMustFitTheRuntime(t *testing.T) {
	f := newRuntimeFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name  string
		agent enrolledAgent
		caps  []string
		fits  bool
	}{
		{"docker capability from a cluster", f.cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityContainerInspect}, false},
		{"cluster capability from a host", f.host, []string{protocol.CapabilityDeploymentApply, protocol.CapabilityPodLogs}, false},
		{"cluster capabilities from a cluster", f.cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs}, true},
		{"docker capabilities from a host", f.host, []string{protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock, reason := connect(t, ctx, f.url, tc.agent, tc.agent.priv, protocol.Version)
			if reason != "" {
				t.Fatalf("connect: %s", reason)
			}
			defer sock.conn.CloseNow()
			writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: tc.caps})
			if !tc.fits {
				if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeError || !strings.Contains(string(e.Payload), "capability_mismatch") {
					t.Fatalf("answer: %+v", e)
				}
				if reason := expectClose(t, ctx, sock.conn); reason != protocol.CloseProtocol {
					t.Fatalf("closed with %q", reason)
				}
			} else {
				writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
				if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
					t.Fatalf("session did not continue: %+v", e)
				}
				var stored []string
				for _, c := range f.endpoint(t, tc.agent.id)["capabilities"].([]any) {
					stored = append(stored, c.(string))
				}
				slices.Sort(tc.caps)
				if !slices.Equal(stored, tc.caps) {
					t.Fatalf("stored %v", stored)
				}
				sock.conn.Close(websocket.StatusNormalClosure, "done")
			}
			waitFor(t, func() bool { return !f.s.Connected(tc.agent.id) })
		})
	}
}

// A Kubernetes endpoint's snapshot must carry the cluster inventory and no Docker lists, a
// Docker endpoint's no cluster inventory; either violation is snapshot_rejected and the
// session goes on. The stored snapshot drives cluster_health, present only for Kubernetes.
func TestSnapshotShapeAndClusterHealth(t *testing.T) {
	f := newRuntimeFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cluster, _ := connect(t, ctx, f.url, f.cluster, f.cluster.priv, protocol.Version)
	defer cluster.conn.CloseNow()
	host, _ := connect(t, ctx, f.url, f.host, f.host.priv, protocol.Version)
	defer host.conn.CloseNow()
	gen := uint64(time.Now().Unix()) - 100
	send := func(sock *agentSocket, s protocol.Snapshot) protocol.Envelope {
		t.Helper()
		gen++
		s.Generation = gen
		writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, s)
		writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
		return readEnvelope(t, ctx, sock.conn)
	}
	rejected := func(e protocol.Envelope) bool {
		return e.Type == protocol.TypeError && strings.Contains(string(e.Payload), "snapshot_rejected")
	}
	nodes := func(ready ...bool) *protocol.KubernetesInventory {
		k := &protocol.KubernetesInventory{}
		for i, r := range ready {
			k.Nodes = append(k.Nodes, protocol.Node{Name: "node-" + string(rune('a'+i)), Ready: r})
		}
		return k
	}
	for name, s := range map[string]protocol.Snapshot{
		"docker lists from a cluster":   {Containers: []protocol.Container{{ID: "c1", Name: "web"}}},
		"both shapes from a cluster":    {Kubernetes: nodes(true), Containers: []protocol.Container{{ID: "c1", Name: "web"}}},
		"no inventory from a cluster":   {},
		"cluster inventory from a host": {Kubernetes: nodes(true)},
	} {
		sock := cluster
		if strings.HasSuffix(name, "host") {
			sock = host
		}
		if e := send(sock, s); !rejected(e) {
			t.Fatalf("%s: %+v", name, e)
		}
		// The session continues: the heartbeat sent after the snapshot is answered.
		if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
			t.Fatalf("%s: session did not continue: %+v", name, e)
		}
	}
	if _, ok := f.endpoint(t, f.cluster.id)["cluster_health"]; !ok {
		t.Fatal("an approved cluster without inventory has no cluster_health")
	}
	if got := f.endpoint(t, f.cluster.id)["cluster_health"]; got != protocol.HealthUnknown {
		t.Fatalf("no inventory yet: %v", got)
	}
	if e := send(cluster, protocol.Snapshot{Kubernetes: nodes(true, false)}); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("a valid cluster snapshot was refused: %+v", e)
	}
	waitFor(t, func() bool { return f.endpoint(t, f.cluster.id)["cluster_health"] == protocol.HealthDegraded })
	w := tenantRequest(f.s, f.admin, "GET", "/api/organizations/a/endpoints", "", false)
	var list []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	for _, e := range list {
		if _, ok := e["cluster_health"]; ok != (e["id"] == f.cluster.id) {
			t.Fatalf("list entry %v", e)
		}
	}
	if e := send(cluster, protocol.Snapshot{Kubernetes: nodes(true, true)}); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("refused: %+v", e)
	}
	waitFor(t, func() bool { return f.endpoint(t, f.cluster.id)["cluster_health"] == protocol.HealthHealthy })
	inv := tenantRequest(f.s, f.admin, "GET", "/api/organizations/a/endpoints/"+f.cluster.id+"/inventory", "", false)
	if inv.Code != 200 || !strings.Contains(inv.Body.String(), `"node-b"`) {
		t.Fatalf("inventory: %d %s", inv.Code, inv.Body.String())
	}
	if e := send(host, protocol.Snapshot{Containers: []protocol.Container{{ID: "c1", Name: "web"}}}); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("a valid docker snapshot was refused: %+v", e)
	}
	if _, ok := f.endpoint(t, f.host.id)["cluster_health"]; ok {
		t.Fatal("a Docker endpoint reports cluster_health")
	}
	cluster.conn.Close(websocket.StatusNormalClosure, "bye")
	waitFor(t, func() bool { return f.endpoint(t, f.cluster.id)["cluster_health"] == protocol.HealthUnknown })
}

// setEndpointRuntime rewrites an endpoint's runtime in storage. No route changes a runtime;
// this reaches the DB the same way expireDeployments does, to put a Docker-adopted
// application on a Kubernetes endpoint.
func setEndpointRuntime(t *testing.T, cfg *config.Config, id, runtime string) {
	t.Helper()
	driver, q := "pgx", "UPDATE endpoints SET runtime=$1 WHERE id=$2"
	if cfg.Database.Driver == "sqlite" {
		driver, q = "sqlite", "UPDATE endpoints SET runtime=? WHERE id=?"
	}
	db, err := sql.Open(driver, cfg.Database.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, runtime, id); err != nil {
		t.Fatal(err)
	}
}

// Every route that acts on containers, images, exec, inspection, deployments, adoption or the
// service mapping refuses a Kubernetes endpoint with runtime_unsupported, before any
// capability or state check.
func TestDockerRoutesRefuseAKubernetesEndpoint(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	var planned store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", h.deployments, h.planBody, 201)), &planned); err != nil {
		t.Fatal(err)
	}
	var plan store.PlanRequest
	_ = json.Unmarshal([]byte(h.planBody), &plan)
	app := strings.TrimSuffix(h.deployments, "/deployments")
	var mapped store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped); err != nil {
		t.Fatal(err)
	}
	mappingBody, _ := json.Marshal(store.MappingRequest{InstanceID: plan.InstanceID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: map[string]string{"web": h.targets[0].ContainerID}})
	setEndpointRuntime(t, h.cfg, h.ag.id, protocol.RuntimeKubernetes)

	ep := "/api/organizations/a/endpoints/" + h.ag.id
	for _, route := range []struct{ method, path, body string }{
		{"POST", ep + "/commands", `{"action":"container.restart","container":"shop-web"}`},
		{"GET", ep + "/containers/shop-web/removal", ""},
		{"GET", ep + "/containers/shop-web/inspection", ""},
		{"GET", ep + "/containers/shop-web/logs", ""},
		{"POST", h.deployments, h.planBody},
		{"POST", h.deployments + "/" + planned.ID + "/apply", `{"confirm":"shop"}`},
		{"POST", app + "/removal", `{"instance_id":"` + plan.InstanceID + `","confirm":"shop"}`},
		{"PUT", app + "/mapping", string(mappingBody)},
		{"GET", app + "/adoption?endpoint=" + h.ag.id + "&project=shop", ""},
		{"POST", app + "/adoption", `{"endpoint_id":"` + h.ag.id + `","project":"shop","digest":"x","confirm":"shop"}`},
	} {
		w := tenantRequest(h.s, h.admin, route.method, route.path, route.body, true)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
			t.Errorf("%s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	// The terminal is a WebSocket upgrade; the refusal comes before it.
	r := httptest.NewRequest("GET", ep+"/containers/shop-web/exec", nil)
	r.Header.Set("Origin", h.cfg.Server.AppURL)
	r.AddCookie(h.admin)
	w := httptest.NewRecorder()
	h.s.ServeHTTP(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
		t.Errorf("exec: %d %s", w.Code, w.Body.String())
	}
	// Reads stay open: the endpoint and its inventory are still visible.
	h.do(t, "GET", ep, "", 200)
	h.do(t, "GET", ep+"/inventory", "", 200)
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestHelloCapabilities|TestSnapshotShape|TestDockerRoutesRefuse' ./internal/api/`
Expected: compiles, then FAIL: the mismatched hellos are not answered with `capability_mismatch` (the test reads the heartbeat-less socket and times out or sees no error frame), `snapshot_rejected` never arrives, `cluster_health` is missing, and every Docker route on the flipped endpoint answers something other than 409 `runtime_unsupported`.

- [ ] **Step 4: Refuse mismatched hellos and snapshots**

In `internal/api/agent_connect.go`, in `type agentConn`, after `fingerprint    string // the key that authenticated this session` add:

```go
	runtime        string // docker or kubernetes, fixed at enrollment
```

In `agentConnect`, in the `c := &agentConn{...}` literal, replace `fingerprint: identity.Fingerprint, ip: s.requestIP(r),` with `fingerprint: identity.Fingerprint, runtime: identity.Endpoint.Runtime, ip: s.requestIP(r),`.

In `handleAgentFrame`, `case protocol.TypeHello:`, replace

```go
		var hello protocol.Hello
		if protocol.UnmarshalHelloBounded(f.Payload, &hello) == nil && !pending {
			if err := ts.SetEndpointCapabilities(fctx, c.endpointID, hello.Capabilities); err != nil {
				log.Printf("agent %s: capabilities: %v", c.endpointID, err)
			}
		}
```

with

```go
		var hello protocol.Hello
		if protocol.UnmarshalHelloBounded(f.Payload, &hello) != nil {
			return false
		}
		// Stored capabilities gate every handler, so an agent claiming another runtime's
		// capabilities is refused outright rather than recorded.
		if !protocol.CapabilitiesFit(c.runtime, hello.Capabilities) {
			_ = s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]string{"code": "capability_mismatch", "runtime": c.runtime}))
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		if !pending {
			if err := ts.SetEndpointCapabilities(fctx, c.endpointID, hello.Capabilities); err != nil {
				log.Printf("agent %s: capabilities: %v", c.endpointID, err)
			}
		}
```

In `case protocol.TypeInventory:`, replace

```go
		if err := protocol.UnmarshalSnapshotBounded(f.Payload, &inv); err != nil {
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
```

with

```go
		if err := protocol.UnmarshalSnapshotBounded(f.Payload, &inv); err != nil {
			c.conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
			return true
		}
		// A snapshot of the wrong shape is a data problem like snapshot_too_large: say so and
		// keep the session.
		if err := protocol.CheckRuntimeShape(c.runtime, &inv); err != nil {
			if err := s.writeFrame(ctx, c.conn, envelope(protocol.TypeError, map[string]string{"code": "snapshot_rejected", "runtime": c.runtime})); err != nil {
				return true
			}
			return false
		}
```

- [ ] **Step 5: `runtime_unsupported` on every Docker route**

In `internal/store/store.go`, in the error `var` block, after `ErrMappingRequired           = errors.New("application has no adopted mapping")` add:

```go
	// ErrRuntimeUnsupported is an action the endpoint's runtime cannot take: a Docker action on
	// a Kubernetes endpoint, a pod log on a Docker one.
	ErrRuntimeUnsupported = errors.New("the endpoint's runtime does not support this action")
```

In `internal/api/tenant_handlers.go`, in `tenantError`, before `case errors.Is(err, store.ErrMappingRequired):` add:

```go
	case errors.Is(err, store.ErrRuntimeUnsupported):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This endpoint's runtime does not support this action", "code": "runtime_unsupported"})
```

Create `internal/api/runtime_gate.go`:

```go
package api

import (
	"net/http"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// dockerOnly refuses, with 409 runtime_unsupported, an action on containers, images,
// networks, volumes, exec, inspection or deployments aimed at an endpoint that is not Docker.
// It runs before any capability check, so the answer names the runtime rather than asking for
// an agent upgrade that would not help. It writes the response and reports false on refusal.
func (s *Server) dockerOnly(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpointID string) bool {
	e, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpointID)
	if err != nil {
		s.tenantError(w, err)
		return false
	}
	if e.Runtime != protocol.RuntimeDocker {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return false
	}
	return true
}
```

Gate the handlers. In `internal/api/endpoint_handlers.go`, in both `handleRemovalPreview` and `handleDispatchCommand`, directly after

```go
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
```

insert

```go
	if !s.dockerOnly(w, r, a, id) {
		return
	}
```

In `internal/api/log_handlers.go`, `handleContainerLogs`, insert the same three lines directly after its `endpointID` block (Task 7 rewrites this function and keeps the gate).

In `internal/api/inspection.go`, `handleContainerInspection`, and in `internal/api/exec_handlers.go`, `handleContainerExec`, directly after their `endpoint, err := endpointID(r)` block insert:

```go
	if !s.dockerOnly(w, r, a, endpoint) {
		return
	}
```

In `internal/api/application_handlers.go`:

- `handleAdoptionPreview`: make its first statement
  ```go
  	if endpoint := r.URL.Query().Get("endpoint"); endpoint != "" && !s.dockerOnly(w, r, a, endpoint) {
  		return
  	}
  ```
- `handleAdoption`: after the `strictJSON` block insert
  ```go
  	if input.EndpointID != "" && !s.dockerOnly(w, r, a, input.EndpointID) {
  		return
  	}
  ```
- `handleSetApplicationMapping`: after the `strictJSON` block insert
  ```go
  	// An instance that cannot be read is the store's to refuse, with its own answer.
  	if instance, err := s.store.Tenancy().ReadApplicationInstance(r.Context(), a, r.PathValue("application"), input.InstanceID); err == nil && !s.dockerOnly(w, r, a, instance.EndpointID) {
  		return
  	}
  ```
- `handlePlanDeployment`: directly before `// The plan measures its frame against what this endpoint's agent accepts.` insert
  ```go
  	// Before the preflight, which refuses a non-Docker inventory as a changed adoption.
  	if instance, err := s.store.Tenancy().ReadApplicationInstance(r.Context(), a, r.PathValue("application"), input.InstanceID); err == nil && !s.dockerOnly(w, r, a, instance.EndpointID) {
  		return
  	}
  ```
- `handleApplyDeployment` and `handleRemoveApplication`: directly after the `ep, err := s.store.Tenancy().ReadEndpoint(...)` error block, before the capability check, insert
  ```go
  	if ep.Runtime != protocol.RuntimeDocker {
  		s.tenantError(w, store.ErrRuntimeUnsupported)
  		return
  	}
  ```

- [ ] **Step 6: Derive `cluster_health` on read**

In `internal/store/models.go`, in `type Endpoint`, after the `Alerts` field add:

```go
	// ClusterHealth is derived on read from the stored inventory; Kubernetes endpoints only.
	ClusterHealth string `json:"cluster_health,omitempty"`
```

In `internal/store/endpoints.go`, in `decorate`, insert directly before its second query (the `rows, err = tx.QueryContext(...)` line that reads `endpoint_events`):

```go
	if e.Runtime == protocol.RuntimeKubernetes {
		var raw string
		var snap *protocol.Snapshot
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT snapshot FROM endpoint_inventory WHERE endpoint_id=?`), e.ID).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var s protocol.Snapshot
			if json.Unmarshal([]byte(raw), &s) == nil {
				snap = &s
			}
		}
		e.ClusterHealth = protocol.ClusterHealth(e.State == "active", snap)
	}
```

- [ ] **Step 7: Run the tests to verify they pass, on both drivers**

Run: `gofmt -w internal && go vet ./... && go test -race -count=1 ./internal/api/ ./internal/store/ ./internal/agent/...`
Expected: PASS, the whole API and store suites included (existing Docker snapshots carry no `kubernetes` key, so they still fit).

Run: `PG=… go test -count=1 ./internal/api/ ./internal/store/`
Expected: PASS.

- [ ] **Step 8: DOX and commit**

`internal/api/AGENTS.md`, `## Local Contracts`, append:

```markdown
- The agent socket knows the endpoint's runtime (`agentConn.runtime`). A `hello` whose capabilities do not fit it (`protocol.CapabilitiesFit`: a Kubernetes endpoint names only `kubernetes.inventory`/`pod.logs`, a Docker endpoint neither) is answered `error {code: capability_mismatch, runtime}` and closed with `protocol.CloseProtocol`, pending endpoints included; nothing is stored. A snapshot of the wrong shape (`protocol.CheckRuntimeShape`) is answered `error {code: snapshot_rejected, runtime}` and the session continues, like `snapshot_too_large`.
- `dockerOnly` (`runtime_gate.go`) answers 409 `runtime_unsupported` for a non-Docker endpoint before any capability check: commands, removal preview, inspection, container logs, exec (before the upgrade), adoption preview and adoption by the named endpoint, and service mapping and plans by the instance's endpoint (`ReadApplicationInstance`, before the store, whose preflight reports a non-Docker inventory as `adoption_changed`); apply and application removal check the endpoint they read. Endpoint, inventory, samples, rollups and the command list stay readable. The web never renders these actions for a Kubernetes endpoint.
```

`internal/store/AGENTS.md`, `## Local Contracts`, append:

```markdown
- `ErrRuntimeUnsupported` is an action the endpoint's runtime cannot take (API: 409 `runtime_unsupported`). `decorate` sets `Endpoint.ClusterHealth` for Kubernetes endpoints only, derived on every read from the stored snapshot with `protocol.ClusterHealth` (`unknown` unless the endpoint is `active` with a cluster inventory); nothing is stored.
```

```bash
git add internal && make tidy-check lint && git commit -m "feat(api): runtime rules for Kubernetes endpoints and cluster health" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: The pod log route

**Files:**
- Modify: `internal/api/log_handlers.go` (`handleContainerLogs` split into `parseLogQuery` + `streamLog`; `handlePodLogs`)
- Modify: `internal/api/server.go` (route)
- Modify: `internal/store/logs.go` (`OpenPodLogTarget`), `internal/store/store.go` (`TenancyStore` method)
- Test: `internal/api/pod_logs_test.go`
- Docs: `internal/api/AGENTS.md`, `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.PodTarget`, `protocol.CapabilityPodLogs`, `protocol.RuntimeKubernetes` (Task 1); `store.ErrRuntimeUnsupported`, `dockerOnly`, `newRuntimeFleet`, `enrollClusterAgent` (Task 6); `awaitOpen`, `connect`, `writeEnvelope`, `readEnvelope`, `waitFor`, `tenantRequest`, `loginAs` (existing test helpers).
- Produces:
  ```go
  // package store, TenancyStore gains:
  OpenPodLogTarget(ctx context.Context, access TenantAccess, endpointID string, pod protocol.PodTarget) error
  // package api (unexported)
  type logQuery struct{ search string; since time.Time; tail int; timestamps, follow, download bool }
  func parseLogQuery(r *http.Request) (logQuery, error)
  func (s *Server) streamLog(w http.ResponseWriter, r *http.Request, a store.TenantAccess, id string, q logQuery, name string, request protocol.LogRequest)
  func (s *Server) handlePodLogs(w http.ResponseWriter, r *http.Request, a store.TenantAccess)
  ```
  Route: `GET /api/organizations/{organization}/endpoints/{endpoint}/pods/{namespace}/{pod}/logs`.

- [ ] **Step 1: Write the failing tests**

Create `internal/api/pod_logs_test.go`:

```go
package api_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

// reports makes every connectCluster snapshot newer than the last, within one second too.
var reports atomic.Uint64

// connectCluster connects the fleet's cluster agent with the given capabilities and makes it
// active with a one-pod inventory.
func connectCluster(t *testing.T, ctx context.Context, f runtimeFleet, caps ...string) *websocket.Conn {
	t.Helper()
	sock, reason := connect(t, ctx, f.url, f.cluster, f.cluster.priv, protocol.Version)
	if reason != "" {
		t.Fatalf("connect: %s", reason)
	}
	t.Cleanup(func() { sock.conn.CloseNow() })
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: caps})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()) - 1000 + reports.Add(1), Kubernetes: &protocol.KubernetesInventory{
		Pods: []protocol.Pod{{Namespace: "shop", Name: "web-7c9", Containers: []protocol.PodContainer{{Name: "web"}, {Name: "sidecar"}}}},
	}})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("setup answered %+v", e)
	}
	waitFor(t, func() bool {
		e, _ := f.st.Tenancy().ReadEndpointRaw(ctx, f.cluster.id)
		return e != nil && e.State == "active"
	})
	return sock.conn
}

// The pod route asks the agent for the named pod and container, never a container ID, streams
// what it answers, and audits the session with the pod it named.
func TestPodLogsStreamFromTheClusterToTheReader(t *testing.T) {
	f := newRuntimeFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn := connectCluster(t, ctx, f, protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs)
	go func() {
		req := awaitOpen(t, ctx, conn)
		if req.Container != "" || req.Pod == nil || *req.Pod != (protocol.PodTarget{Namespace: "shop", Name: "web-7c9", Container: "web"}) || req.Tail != 10 {
			t.Errorf("the agent was asked for %+v", req)
		}
		writeEnvelope(t, ctx, conn, protocol.TypeLogChunk, protocol.LogChunk{Stream: req.Stream, Data: "ready\n"})
		writeEnvelope(t, ctx, conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "the log ended"})
	}()
	base := "/api/organizations/a/endpoints/" + f.cluster.id + "/pods/shop/web-7c9/logs"
	w := tenantRequest(f.s, f.admin, "GET", base+"?container=web&tail=10", "", false)
	if w.Code != 200 || w.Body.String() != "ready\n" {
		t.Fatalf("logs: %d %q", w.Code, w.Body.String())
	}
	rows, _, err := f.st.Audit().ListAuditRecords(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var session *store.AuditRecord
	for _, row := range rows {
		if row.Action == "container.logs" {
			session = row
		}
	}
	if session == nil || session.Result != "success" || session.Resource != f.cluster.id+"/pods/shop/web-7c9/web" {
		t.Fatalf("audit %+v", session)
	}

	// The agent's refusal of an unnamed container in a two-container pod reaches the reader.
	go func() {
		req := awaitOpen(t, ctx, conn)
		if req.Pod == nil || req.Pod.Container != "" {
			t.Errorf("the agent was asked for %+v", req)
		}
		writeEnvelope(t, ctx, conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "unknown_container: this pod runs 2 containers; name one", Failed: true})
	}()
	w = tenantRequest(f.s, f.admin, "GET", base, "", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "the stream ended: unknown_container") {
		t.Fatalf("unnamed container: %d %q", w.Code, w.Body.String())
	}
}

// Names outside Kubernetes syntax are refused at the boundary; the pod route refuses a Docker
// endpoint and a cluster agent without pod.logs; a read-only member may not read it.
func TestPodLogsRefusals(t *testing.T) {
	f := newRuntimeFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	viewer := loginAs(t, f.s, f.st, "viewer", "user")
	if err := f.st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	pods := "/api/organizations/a/endpoints/" + f.cluster.id + "/pods/"
	conn := connectCluster(t, ctx, f, protocol.CapabilityKubernetesInventory)
	for path, want := range map[string]int{
		pods + "Shop/web/logs":                                                400,
		pods + "shop/web/logs?container=a.b":                                  400,
		pods + "shop/web/logs?tail=0":                                         400,
		pods + "shop/web/logs?follow=1&download=1":                            400,
		pods + "shop/web/logs":                                                501,
		"/api/organizations/a/endpoints/" + f.host.id + "/pods/shop/web/logs": 409,
	} {
		if w := tenantRequest(f.s, f.admin, "GET", path, "", false); w.Code != want {
			t.Errorf("%s: %d %s, want %d", path, w.Code, w.Body.String(), want)
		}
	}
	conn.Close(websocket.StatusNormalClosure, "reconnect")
	waitFor(t, func() bool { return !f.s.Connected(f.cluster.id) })
	connectCluster(t, ctx, f, protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs)
	if w := tenantRequest(f.s, viewer, "GET", pods+"shop/web/logs", "", false); w.Code != 403 {
		t.Fatalf("read-only member: %d %s", w.Code, w.Body.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run TestPodLogs ./internal/api/`
Expected: FAIL: the pod route is not registered, so `/pods/...` falls through to the organizations catch-all (404), and the refusal table reads 404 for every row.

- [ ] **Step 3: Authorize and audit a pod log session**

In `internal/store/logs.go`, insert before `// StillAllowed re-checks a live authorization`:

```go
// OpenPodLogTarget authorizes reading one pod container's log under container.logs and
// records the session, naming the pod: the endpoint must be an active Kubernetes endpoint. The
// pod is not resolved against the inventory; the agent reads its spec.
func (t *tenancyStore) OpenPodLogTarget(ctx context.Context, a TenantAccess, endpointID string, pod protocol.PodTarget) error {
	if pod.Validate() != nil {
		return fmt.Errorf("%w: pod", ErrInvalid)
	}
	resource := endpointID + "/pods/" + pod.Namespace + "/" + pod.Name
	if pod.Container != "" {
		resource += "/" + pod.Container
	}
	return t.run(ctx, a, permissions.ContainerLogs, &resource, nil, false, func(tx *sql.Tx) error {
		var state, runtime string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT state,runtime FROM endpoints WHERE id=? AND organization_id=? AND (?='' OR environment_id=?)`),
			endpointID, a.OrganizationID, a.EnvironmentID, a.EnvironmentID).Scan(&state, &runtime)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if runtime != protocol.RuntimeKubernetes {
			return ErrRuntimeUnsupported
		}
		if state != "active" {
			return fmt.Errorf("%w: it is %s", ErrEndpointOffline, state)
		}
		return nil
	})
}

```

In `internal/store/store.go`, in `TenancyStore`, after `OpenLogTarget(ctx context.Context, access TenantAccess, endpointID, identifier string) (*LogTarget, error)` add:

```go
	OpenPodLogTarget(ctx context.Context, access TenantAccess, endpointID string, pod protocol.PodTarget) error
```

- [ ] **Step 4: Share the stream between container and pod logs**

In `internal/api/log_handlers.go`, add `"slices"` to the imports, and replace everything from the comment `// handleContainerLogs streams one container's log to the caller: history, or history followed` down to (not including) `// parseSince reads either an instant or an age.` with:

```go
// handleContainerLogs streams one container's log, named as the operator sees it and resolved
// to the ID the endpoint last reported.
func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.dockerOnly(w, r, a, id) {
		return
	}
	q, err := parseLogQuery(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	// Authorization, the audit row, and the resolution of the name the operator used into the
	// container the endpoint last reported, all in one place.
	target, err := s.store.Tenancy().OpenLogTarget(r.Context(), a, id, r.PathValue("container"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.streamLog(w, r, a, id, q, target.Name, protocol.LogRequest{Container: target.ContainerID})
}

// handlePodLogs streams one pod container's log under the same authorization, bounds and
// shapes as a container's. The pod is named, not resolved: the agent reads the pod spec and
// refuses an unnamed container in a pod that runs several.
func (s *Server) handlePodLogs(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	pod := protocol.PodTarget{Namespace: r.PathValue("namespace"), Name: r.PathValue("pod"), Container: r.URL.Query().Get("container")}
	if pod.Validate() != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	q, err := parseLogQuery(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, id)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if ep.Runtime != protocol.RuntimeKubernetes {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return
	}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityPodLogs) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the cluster agent to read pod logs")
		return
	}
	if err := s.store.Tenancy().OpenPodLogTarget(r.Context(), a, id, pod); err != nil {
		s.tenantError(w, err)
		return
	}
	name := pod.Namespace + "/" + pod.Name
	if pod.Container != "" {
		name += "/" + pod.Container
	}
	s.streamLog(w, r, a, id, q, name, protocol.LogRequest{Pod: &pod})
}

// logQuery is a log request's options, bounded at the boundary.
type logQuery struct {
	search                       string
	since                        time.Time
	tail                         int
	timestamps, follow, download bool
}

func parseLogQuery(r *http.Request) (logQuery, error) {
	v := r.URL.Query()
	q := logQuery{search: v.Get("search"), tail: defaultLogTail, timestamps: v.Get("timestamps") == "1", follow: v.Get("follow") == "1", download: v.Get("download") == "1"}
	if len(q.search) > maxLogSearchBytes {
		return q, store.ErrInvalid
	}
	since, err := parseSince(v.Get("since"))
	if err != nil {
		return q, store.ErrInvalid
	}
	q.since = since
	if raw := v.Get("tail"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 || n > protocol.MaxLogTail {
			return q, store.ErrInvalid
		}
		q.tail = n
	}
	// A download is a file, and a file has an end. Asking for both says nothing coherent.
	if q.follow && q.download {
		return q, store.ErrInvalid
	}
	return q, nil
}

// streamLog opens the stream on the endpoint and serves it: history, or history followed by
// whatever arrives next. The body is never stored and never interpreted -- it is the
// application's own output, forwarded as text.
//
// Every bound is explicit and reported when it bites: the request ends at
// protocol.MaxLogLines or protocol.MaxLogBytes with a line saying so, a reader that cannot
// keep up gets a gap marker naming the bytes dropped rather than a log that looks continuous,
// and an endpoint serves only so many streams at once.
func (s *Server) streamLog(w http.ResponseWriter, r *http.Request, a store.TenantAccess, id string, q logQuery, name string, request protocol.LogRequest) {
	stream, refusal := s.logs.open(id, a.ActorID)
	if refusal != "" {
		s.writeError(w, http.StatusTooManyRequests, refusal)
		return
	}
	defer s.logs.release(stream)
	request.Stream, request.Tail, request.Since, request.Timestamps, request.Follow = stream.id, q.tail, q.since, q.timestamps, q.follow
	if !s.agents.deliver(id, envelope(protocol.TypeLogOpen, request)) {
		s.writeError(w, http.StatusConflict, "The endpoint is not connected")
		return
	}
	// The agent stops reading the host when the reader goes, however the reader goes.
	defer s.agents.deliver(id, envelope(protocol.TypeLogCancel, protocol.LogCancel{Stream: stream.id}))

	out := newLogWriter(w, q.follow, q.download, name)
	out.head(budgetFor(q.follow))
	ticker := time.NewTicker(accessRecheck)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// Authorization is not a thing that was true once. A membership taken away, a
			// role narrowed, an account disabled or a session ended between one chunk and the
			// next must end the stream; the event-driven closes are the fast path and this is
			// the backstop for anything not wired to one.
			if !s.stillAllowed(r, a, id) {
				out.notice("your access to this log was withdrawn")
				return
			}
			if out.expired() {
				out.notice("this stream reached its time limit; open it again to carry on watching")
				return
			}
			out.keepalive()
		case chunk := <-stream.chunks:
			out.gap(stream.gap())
			if done := out.write(chunk.Data, q.search); done {
				return
			}
		case <-stream.done:
			// Drain what arrived before the end: a short log finishes before the reader has
			// looked, and losing it would make a container that printed once look silent.
			for {
				select {
				case chunk := <-stream.chunks:
					out.gap(stream.gap())
					if done := out.write(chunk.Data, q.search); done {
						return
					}
					continue
				default:
				}
				break
			}
			out.gap(stream.gap())
			out.end(stream.end.Load())
			return
		}
	}
}
```

In `internal/api/server.go`, after the line registering `.../containers/{container}/logs` add:

```go
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/pods/{namespace}/{pod}/logs", s.tenantRoute(s.handlePodLogs))
```

- [ ] **Step 5: Run the tests to verify they pass, on both drivers**

Run: `gofmt -w internal && go vet ./... && go test -race -count=1 ./internal/api/ ./internal/store/`
Expected: PASS, including every existing log test (`logs_test.go`, `log_writer_internal_test.go`): the container route's behaviour is unchanged.

Run: `PG=… go test -count=1 -run 'TestPodLogs|TestLogs|Log' ./internal/api/ && PG=… go test -count=1 ./internal/store/`
Expected: PASS.

- [ ] **Step 6: DOX and commit**

`internal/api/AGENTS.md`, `## Local Contracts`, append:

```markdown
- `GET /endpoints/{endpoint}/pods/{namespace}/{pod}/logs?container=&tail=&search=&follow=&download=&timestamps=` validates the names as DNS-1123 (`protocol.PodTarget`, 400), requires a Kubernetes endpoint (409 `runtime_unsupported`), the stored `pod.logs` capability (501) and `container.logs` through `store.OpenPodLogTarget`, then shares `streamLog` with the container route: same tail default and ceiling, caps, notice tokens, SSE and download shapes, `log.cancel` and access recheck; `log_streams.go` is unchanged. The frame carries `LogRequest.Pod` and no container ID; an empty `container` lets the agent choose the only one or refuse with `unknown_container`.
```

`internal/store/AGENTS.md`, `## Local Contracts`, append:

```markdown
- `OpenPodLogTarget` authorizes `container.logs` with an audited session row naming `<endpoint>/pods/<namespace>/<pod>[/<container>]`, requires a Kubernetes endpoint (`ErrRuntimeUnsupported`) that is active (`ErrEndpointOffline`), and does not resolve the pod against the inventory: the agent reads the pod spec.
```

```bash
git add internal && make tidy-check lint && git commit -m "feat(api): pod log route over the shared log stream" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Web — enrollment runtime switch and the read-only cluster view

**Files:**
- Modify: `web/src/tenant.ts` (types)
- Modify: `web/src/components/ContainerControls.tsx` (`ContainerLogs` gains `query`)
- Create: `web/src/components/ResourceTable.tsx` (the table `EndpointPage` defined privately, now shared)
- Create: `web/src/components/KubernetesCluster.tsx`
- Modify: `web/src/pages/EndpointPage.tsx` (whole file below), `web/src/components/Endpoints.tsx` (whole file below)
- Test: `web/src/components/Endpoints.test.tsx`, `web/src/pages/EndpointPage.test.tsx` (appended)
- Build: `web/dist/**`, `web/tsconfig.tsbuildinfo` (`make build-web`)
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes: the HTTP contracts of Tasks 5–7: token response `manifest`, `manifest_file`, `command`, `disclosure`, `note`, 409 `agent_image_unpinned`/`https_required`; `Endpoint.cluster_health`; `Snapshot.kubernetes`; `GET .../pods/{namespace}/{pod}/logs?container=`.
- Produces (TypeScript):
  ```ts
  export interface KubernetesInventory { nodes: KubeNode[]; namespaces: string[]; workloads: Workload[]; pods: Pod[]; services: KubeService[]; claims: Claim[] }
  // Snapshot gains kubernetes?: KubernetesInventory; Endpoint gains cluster_health?; EnrollmentToken gains manifest?, manifest_file?
  export function ResourceTable<T>(props: { title: string; rows: T[]; empty: string; head: string[]; render: (r: T) => React.ReactNode[] }): JSX.Element
  export function KubernetesCluster(props: { base: string; endpoint: Endpoint; inventory: KubernetesInventory }): JSX.Element
  export function ContainerLogs(props: { url: string; name: string; query?: Record<string, string> }): JSX.Element
  ```

- [ ] **Step 1: Write the failing tests**

Append to `web/src/components/Endpoints.test.tsx`:

```tsx
it('enrolls a named Kubernetes cluster with the manifest, its command and the RBAC disclosure', async () => {
  const manifest = 'apiVersion: v1\nkind: Namespace\n';
  let posted = '';
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'POST') {
      posted = String(init.body);
      return json({ id: 't1', runtime: 'kubernetes', expires_at: '2026-09-16T00:15:00Z', token: 'tok', command: 'kubectl apply -f kyyard-agent-prod.yaml', manifest, manifest_file: 'kyyard-agent-prod.yaml', note: 'delete secret kyyard-agent-enrollment', disclosure: 'It cannot read Secrets or ConfigMaps. Applying the manifest needs cluster-admin.' }, 201);
    }
    return json([]);
  }));
  const writeText = vi.fn(async () => {});
  vi.stubGlobal('navigator', { clipboard: { writeText } });
  const clicked: string[] = [];
  vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: vi.fn(() => 'blob:manifest'), revokeObjectURL: vi.fn() }));
  const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) { clicked.push(this.download); });
  document.cookie = 'ky_csrf=csrf-test';
  render(<Endpoints org="a" env="env-a" />);
  await screen.findByText(/No endpoints yet/);
  fireEvent.change(screen.getByRole('combobox', { name: 'Runtime' }), { target: { value: 'kubernetes' } });
  const enroll = screen.getByRole('button', { name: 'Enroll a cluster' }) as HTMLButtonElement;
  expect(enroll.disabled).toBe(true);
  fireEvent.change(screen.getByRole('textbox', { name: 'Cluster name' }), { target: { value: ' prod ' } });
  fireEvent.click(enroll);
  const region = await screen.findByRole('region', { name: 'Enrollment manifest' });
  expect(JSON.parse(posted)).toEqual({ runtime: 'kubernetes', name: 'prod' });
  expect(region.textContent).toContain('kubectl apply -f kyyard-agent-prod.yaml');
  expect(region.textContent).toContain('cannot read Secrets');
  expect(region.textContent).toContain('cluster-admin');
  expect(region.textContent).not.toContain('docker');
  fireEvent.click(screen.getByRole('button', { name: 'Copy manifest' }));
  expect(writeText).toHaveBeenCalledWith(manifest);
  fireEvent.click(screen.getByRole('button', { name: 'Download manifest' }));
  expect(clicked).toEqual(['kyyard-agent-prod.yaml']);
  click.mockRestore();
});

it('explains a cluster enrollment the server cannot render', async () => {
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => init?.method === 'POST'
    ? json({ error: 'x', code: 'agent_image_unpinned' }, 409)
    : json([])));
  document.cookie = 'ky_csrf=csrf-test';
  render(<Endpoints org="a" env="env-a" />);
  await screen.findByText(/No endpoints yet/);
  fireEvent.change(screen.getByRole('combobox', { name: 'Runtime' }), { target: { value: 'kubernetes' } });
  fireEvent.change(screen.getByRole('textbox', { name: 'Cluster name' }), { target: { value: 'prod' } });
  fireEvent.click(screen.getByRole('button', { name: 'Enroll a cluster' }));
  expect((await screen.findByRole('alert')).textContent).toBe('Set KY_AGENT_IMAGE to a digest-pinned image before enrolling a cluster.');
  expect(screen.queryByRole('region', { name: 'Enrollment manifest' })).toBeNull();
});

it('shows a cluster endpoint with its health and node count', async () => {
  const cluster = { ...pending, id: 'ep_2', name: 'prod', runtime: 'kubernetes', state: 'active', facts: { runtime: 'kubernetes', server_version: 'v1.36.0', node_count: '3' }, cluster_health: 'degraded' };
  vi.stubGlobal('fetch', vi.fn(async () => json([cluster])));
  render(<Endpoints org="a" env="env-a" />);
  expect(await screen.findByText('degraded')).toBeTruthy();
  expect(screen.getByText('3 nodes')).toBeTruthy();
  expect(screen.getByText('v1.36.0')).toBeTruthy();
});
```

Append to `web/src/pages/EndpointPage.test.tsx`:

```tsx
it('renders a Kubernetes cluster read-only, filters by namespace and opens pod logs on the pod route', async () => {
  const now = new Date().toISOString();
  const cluster = { ...endpoint, runtime: 'kubernetes', capabilities: ['kubernetes.inventory', 'pod.logs'], cluster_health: 'degraded' };
  const kubernetes = {
    nodes: [{ name: 'control-1', kubelet_version: 'v1.36.0', os: 'linux', arch: 'amd64', ready: true, roles: ['control-plane'], unschedulable: false }, { name: 'worker-1', kubelet_version: 'v1.36.0', os: 'linux', arch: 'arm64', ready: false, roles: [], unschedulable: true }],
    namespaces: ['kube-system', 'shop'],
    workloads: [{ kind: 'Deployment', namespace: 'shop', name: 'web', desired: 3, ready: 2, updated: 3, images: ['nginx:1.29'], paused: false }, { kind: 'DaemonSet', namespace: 'kube-system', name: 'proxy', desired: 2, ready: 2, updated: 2, images: ['kube-proxy:1'], paused: false }],
    pods: [{ namespace: 'shop', name: 'web-7c9', phase: 'Running', node: 'worker-1', owner_kind: 'Deployment', owner_name: 'web', started_at: now, containers: [{ name: 'web', image: 'nginx:1.29', image_id: '', state: 'running', reason: '', ready: true, restart_count: 2 }, { name: 'log', image: 'busybox:1', image_id: '', state: 'waiting', reason: 'CrashLoopBackOff', ready: false, restart_count: 5 }] },
      { namespace: 'kube-system', name: 'proxy-x', phase: 'Running', node: 'control-1', owner_kind: 'DaemonSet', owner_name: 'proxy', started_at: now, containers: [{ name: 'proxy', image: 'kube-proxy:1', image_id: '', state: 'running', reason: '', ready: true, restart_count: 0 }] }],
    services: [{ namespace: 'shop', name: 'web', type: 'ClusterIP', cluster_ip: '10.96.0.10', ports: ['80/TCP'] }],
    claims: [{ namespace: 'shop', name: 'data', phase: 'Bound', storage_class: 'fast', capacity: '10Gi' }],
  };
  const snapshot = { generation: 3, observed_at: now, engine: { runtime: 'kubernetes', version: 'v1.36.0', api_version: '1.36', os: 'linux', arch: 'amd64', kernel: '', cpus: 0, memory_bytes: 0, hostname: '' }, containers: [], images: [], networks: [], volumes: [], kubernetes, truncated: ['pods'] };
  const requests: string[] = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    requests.push(url);
    if (url.includes('/pods/')) return new Response('ready\n', { status: 200 });
    if (url.endsWith('/inventory')) return json({ endpoint_id: 'ep_1', state: 'active', generation: 3, observed_at: now, received_at: now, snapshot });
    if (url.endsWith('/samples') || url.includes('/commands') || url.endsWith('/applications') || url === '/api/organizations') return json([]);
    return json(cluster);
  }));
  const show = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: show });
  render(<EndpointPage org="a" endpoint="ep_1" />);
  const health = await screen.findByRole('region', { name: 'Cluster health' });
  expect(health.textContent).toContain('degraded');
  expect(health.textContent).toContain('1 of 2 nodes ready');
  expect(screen.getByText('cordoned')).toBeTruthy();
  expect(screen.getByRole('status').textContent).toContain('truncated: pods');
  // No Docker resource or control is rendered for a cluster.
  for (const name of ['Containers', 'Images', 'Networks', 'Volumes', 'Projects', 'Activity']) expect(screen.queryByRole('button', { name })).toBeNull();
  expect(screen.queryByText('Actions')).toBeNull();
  expect(screen.queryByText(/Pull an image/)).toBeNull();
  expect(screen.getByRole('region', { name: 'Applications' }).textContent).toContain('Kubernetes deployment arrives in a later release');
  expect(screen.getByText('2/3')).toBeTruthy();
  expect(screen.getByText('proxy-x', { exact: false })).toBeTruthy();
  fireEvent.change(screen.getByRole('combobox', { name: 'Namespace' }), { target: { value: 'shop' } });
  expect(screen.queryByText('kube-system/proxy-x')).toBeNull();
  expect(screen.getByText('7')).toBeTruthy(); // restarts summed across the pod's containers
  fireEvent.click(screen.getByRole('button', { name: 'Logs for shop/web-7c9/web' }));
  const dialog = screen.getByRole('dialog', { name: 'Logs for shop/web-7c9/web' });
  expect(show).toHaveBeenCalledTimes(1);
  fireEvent.click(within(dialog).getByRole('button', { name: 'Load logs' }));
  await waitFor(() => expect(within(dialog).getByText('ready')).toBeTruthy());
  const logURL = requests.find((u) => u.includes('/pods/'));
  expect(logURL?.startsWith('/api/organizations/a/endpoints/ep_1/pods/shop/web-7c9/logs?container=web&tail=200')).toBe(true);
  Reflect.deleteProperty(HTMLDialogElement.prototype, 'showModal');
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm ci && npx vitest run src/components/Endpoints.test.tsx src/pages/EndpointPage.test.tsx; cd ..`
Expected: FAIL: no `Runtime` combobox, no `Cluster health` region.

- [ ] **Step 3: Types and the shared pieces**

In `web/src/tenant.ts`, in `interface Endpoint`, replace `approved_by?: string }` with `approved_by?: string; cluster_health?: 'healthy' | 'degraded' | 'unknown' }`; in `interface EnrollmentToken` replace `disclosure: string }` with `disclosure: string; manifest?: string; manifest_file?: string }`; in `interface Snapshot` replace

```ts
  truncated?: string[];
}
```

with

```ts
  truncated?: string[];
  kubernetes?: KubernetesInventory;
}
// A cluster agent's inventory (protocol.KubernetesInventory); started_at is year 1 when unknown.
export interface KubeNode { name: string; kubelet_version: string; os: string; arch: string; ready: boolean; roles: string[]; unschedulable: boolean }
export interface Workload { kind: string; namespace: string; name: string; desired: number; ready: number; updated: number; images: string[]; paused: boolean }
export interface PodContainer { name: string; image: string; image_id: string; state: string; reason: string; ready: boolean; restart_count: number }
export interface Pod { namespace: string; name: string; phase: string; node: string; owner_kind: string; owner_name: string; started_at: string; containers: PodContainer[] }
export interface KubeService { namespace: string; name: string; type: string; cluster_ip: string; ports: string[] }
export interface Claim { namespace: string; name: string; phase: string; storage_class: string; capacity: string }
export interface KubernetesInventory { nodes: KubeNode[]; namespaces: string[]; workloads: Workload[]; pods: Pod[]; services: KubeService[]; claims: Claim[] }
```

In `web/src/components/ContainerControls.tsx`, replace

```tsx
export function ContainerLogs({ url, name }: { url: string; name: string }) {
```

with

```tsx
// query carries route-specific parameters, such as a pod log's container.
export function ContainerLogs({ url, name, query }: { url: string; name: string; query?: Record<string, string> }) {
```

and `const params = new URLSearchParams({ tail: '200', timestamps: timestamps ? '1' : '0', search });` with `const params = new URLSearchParams({ ...query, tail: '200', timestamps: timestamps ? '1' : '0', search });`.

Create `web/src/components/ResourceTable.tsx`:

```tsx
import React from 'react';
import { usePagination } from './Pagination';
import { EmptyNotice } from './StateNotice';

// A titled, paged resource table whose rows become labelled cards at mobile widths.
export function ResourceTable<T>({ title, rows, empty, head, render }: { title: string; rows: T[]; empty: string; head: string[]; render: (r: T) => React.ReactNode[] }) {
  const pagination = usePagination(rows, title);
  return (
    <section className="panel">
      <div className="panel-header"><h2 style={{ fontSize: 16 }}>{title} <span style={{ color: 'var(--ink)', fontWeight: 400 }}>({rows.length})</span></h2></div>
      {pagination.controls}
      {rows.length === 0 ? <EmptyNotice>{empty}</EmptyNotice> : (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr>{head.map((h) => <th key={h}>{h}</th>)}</tr></thead>
            <tbody>{pagination.rows.map((r, i) => <tr key={i}>{render(r).map((cell, j) => <td key={j} data-label={head[j]}>{cell}</td>)}</tr>)}</tbody>
          </table>
        </div>
      )}
    </section>
  );
}
```

Create `web/src/components/KubernetesCluster.tsx`:

```tsx
import { useEffect, useRef, useState } from 'react';
import { ContainerLogs } from './ContainerControls';
import { displayName } from './Endpoints';
import { ResourceTable } from './ResourceTable';
import type { Endpoint, KubernetesInventory, Pod, PodContainer } from '../tenant';

const healthBadge: Record<string, string> = { healthy: 'badge-success', degraded: 'badge-danger' };
const stateBadge: Record<string, string> = { running: 'badge-success', terminated: 'badge-danger' };

// A Kubernetes endpoint is read-only in this release: health, nodes, workloads, pods with their
// logs, services and claims. No Docker control is rendered, and none would be accepted.
export function KubernetesCluster({ base, endpoint, inventory }: { base: string; endpoint: Endpoint; inventory: KubernetesInventory }) {
  const [namespace, setNamespace] = useState('');
  const [logs, setLogs] = useState<{ pod: Pod; container: PodContainer } | null>(null);
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => { if (logs) dialog.current?.showModal(); }, [logs]);
  const health = endpoint.cluster_health ?? 'unknown';
  const ready = inventory.nodes.filter((n) => n.ready).length;
  // A namespace that left the cluster must not keep hiding everything.
  const selected = inventory.namespaces.includes(namespace) ? namespace : '';
  const scoped = <T extends { namespace: string }>(rows: T[]) => rows.filter((r) => selected === '' || r.namespace === selected);
  const qualified = (r: { namespace: string; name: string }) => `${displayName(r.namespace)}/${displayName(r.name)}`;
  const active = endpoint.state === 'active';
  return <>
    <section className="panel" aria-label="Cluster health">
      <h2 style={{ fontSize: 16 }}>Cluster health <span className={`badge ${healthBadge[health] ?? 'badge-secondary'}`}>{health}</span></h2>
      <p>{ready} of {inventory.nodes.length} nodes ready.</p>
    </section>
    <ResourceTable title="Nodes" rows={inventory.nodes} empty="No nodes reported." head={['Node', 'Ready', 'Roles', 'Version', 'Platform']} render={(n) => [
      <>{displayName(n.name)}{n.unschedulable && <> <span className="badge badge-secondary">cordoned</span></>}</>,
      <span className={`badge ${n.ready ? 'badge-success' : 'badge-danger'}`}>{n.ready ? 'ready' : 'not ready'}</span>,
      n.roles.map(displayName).join(', ') || '—', displayName(n.kubelet_version), `${displayName(n.os)}/${displayName(n.arch)}`,
    ]} />
    <div className="ky-toolbar">
      <label>Namespace <select aria-label="Namespace" value={selected} onChange={(e) => setNamespace(e.target.value)}>
        <option value="">All namespaces</option>
        {inventory.namespaces.map((ns) => <option key={ns} value={ns}>{displayName(ns)}</option>)}
      </select></label>
    </div>
    <ResourceTable key={`workloads-${selected}`} title="Workloads" rows={scoped(inventory.workloads)} empty="No workloads." head={['Workload', 'Kind', 'Ready', 'Images']} render={(w) => [
      qualified(w), w.kind, `${w.ready}/${w.desired}${w.paused ? ' (paused)' : ''}`, w.images.map(displayName).join(', '),
    ]} />
    <ResourceTable key={`pods-${selected}`} title="Pods" rows={scoped(inventory.pods)} empty="No pods." head={['Pod', 'Phase', 'Node', 'Restarts', 'Containers']} render={(p) => [
      <div className="ky-resource-name"><strong>{qualified(p)}</strong>{p.owner_kind && <small>{displayName(p.owner_kind)} {displayName(p.owner_name)}</small>}</div>,
      displayName(p.phase), displayName(p.node) || '—', p.containers.reduce((sum, c) => sum + c.restart_count, 0),
      <ul className="ky-list">{p.containers.map((c) => <li key={c.name}>
        {displayName(c.name)} <span className={`badge ${stateBadge[c.state] ?? 'badge-secondary'}`} title={c.image}>{displayName(c.state)}</span>{c.reason && ` ${displayName(c.reason)}`}{' '}
        <button className="btn-secondary" disabled={!active} aria-label={`Logs for ${p.namespace}/${p.name}/${c.name}`} onClick={() => setLogs({ pod: p, container: c })}>Logs</button>
      </li>)}</ul>,
    ]} />
    <ResourceTable key={`services-${selected}`} title="Services" rows={scoped(inventory.services)} empty="No services." head={['Service', 'Type', 'Cluster IP', 'Ports']} render={(s) => [
      qualified(s), displayName(s.type), displayName(s.cluster_ip) || '—', s.ports.map(displayName).join(', ') || '—',
    ]} />
    <ResourceTable key={`claims-${selected}`} title="Volume claims" rows={scoped(inventory.claims)} empty="No volume claims." head={['Claim', 'Phase', 'Storage class', 'Capacity']} render={(c) => [
      qualified(c), displayName(c.phase), displayName(c.storage_class) || '—', displayName(c.capacity) || '—',
    ]} />
    <section className="panel" aria-label="Applications"><h2 style={{ fontSize: 16 }}>Applications</h2>
      <p>Kubernetes deployment arrives in a later release. KyYard reads this cluster; it does not change it.</p>
    </section>
    {logs && <dialog ref={dialog} className="modal-window ky-log-dialog" aria-label={`Logs for ${logs.pod.namespace}/${logs.pod.name}/${logs.container.name}`} onCancel={(event) => { event.preventDefault(); setLogs(null); }} onClose={() => setLogs(null)}>
      <button className="btn-secondary" onClick={() => setLogs(null)}>Close logs</button>
      <ContainerLogs key={`${logs.pod.namespace}/${logs.pod.name}/${logs.container.name}`} url={`${base}/pods/${encodeURIComponent(logs.pod.namespace)}/${encodeURIComponent(logs.pod.name)}/logs`} name={`${logs.pod.namespace}/${logs.pod.name}/${logs.container.name}`} query={{ container: logs.container.name }} />
    </dialog>}
  </>;
}
```

- [ ] **Step 4: The cluster view and the enrollment switch**

Replace `web/src/pages/EndpointPage.tsx` with (the private `Table` moves to `ResourceTable`; a Kubernetes endpoint gets the `Cluster` and `Details` tabs only):

```tsx
import { ContainerPorts } from '../components/ContainerPorts';
import type { ApplicationInstance } from '../components/ApplicationAdoption';
import React, { useState } from 'react';
import { ResourceTable } from '../components/ResourceTable';
import { ComposeProjects } from '../components/ComposeProjects';
import { ImageControls } from '../components/ImageControls';
import { ContainerControls } from '../components/ContainerControls';
import { Server } from 'lucide-react';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { envPath } from '../router';
import { canExec, useTenantResource, type Endpoint, type Inventory, type MemberOrganization, type Sample } from '../tenant';
import { displayName } from '../components/Endpoints';
import { KubernetesCluster } from '../components/KubernetesCluster';

const bytes = (n: number) => n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GiB` : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(0)} MiB` : `${n} B`;
const ago = (iso: string) => { const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000)); return s < 90 ? `${s}s ago` : s < 5400 ? `${Math.round(s / 60)}m ago` : `${Math.round(s / 3600)}h ago`; };

// Freshness is shown from received_at (server clock); observed_at is the agent's clock and is
// flagged when it disagrees by more than five minutes.
export const EndpointPage: React.FC<{ org: string; endpoint: string }> = ({ org, endpoint }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}`;
  const [view, setView] = useState('containers');
  const [search, setSearch] = useState('');
  const [projectFilter, setProjectFilter] = useState<{ base: string; name: string } | null>(null);
  const details = useTenantResource<Endpoint>(base);
  const inventory = useTenantResource<Inventory>(`${base}/inventory`);
  const commands = useTenantResource<{ id: string; action: string; outcome: string; detail?: string; container_id?: string; reference?: string }[]>(`${base}/commands?limit=20`);
  const ownership = useTenantResource<ApplicationInstance[]>(`${base}/applications`);
  const samples = useTenantResource<Sample[]>(`${base}/samples`);
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const exec = canExec((Array.isArray(organizations.data) ? organizations.data : []).find((o) => o.id === org)?.role);
  const latest = new Map((Array.isArray(samples.data) ? samples.data : []).map((s) => [s.container_id, s]));
  // -1 is "no interval yet" and a missing row is "no data"; neither is zero usage.
  const usage = (c: { id: string; state: string }) => {
    const s = latest.get(c.id);
    if (!s) return c.state === 'running' ? 'no data' : '—';
    const cpu = s.cpu_percent < 0 ? 'cpu —' : `cpu ${s.cpu_percent.toFixed(1)}%`;
    // A runtime that did not answer reports -1, and a server too old to send the field at all
    // reports nothing; neither is a container that has never restarted, so both stay silent.
    const restarts = typeof s.restart_count === 'number' && s.restart_count >= 0 ? ` · ${s.restart_count} restarts` : '';
    return `${cpu} · mem ${bytes(s.memory_bytes)}${restarts}`;
  };
  const e = details.data;
  const inv = inventory.data;
  // A cluster has its own view; Docker tabs, and the controls under them, are never rendered.
  const cluster = e?.runtime === 'kubernetes';
  const tabs = cluster ? ['cluster', 'details'] : ['containers', 'projects', 'images', 'networks', 'volumes', 'activity', 'details'];
  const shown = tabs.includes(view) ? view : tabs[0];
  // A filter belongs to one endpoint and must not hide a refreshed or different host.
  const selectedProject = projectFilter?.base === base && inv?.snapshot.containers.some((c) => c.compose_project === projectFilter.name) ? projectFilter.name : null;
  const visibleContainers = inv?.snapshot.containers.filter((c) => selectedProject === null || c.compose_project === selectedProject).filter((c) => `${c.name} ${c.image}`.toLowerCase().includes(search.toLowerCase())) ?? [];
  const skew = inv ? Math.abs(new Date(inv.received_at).getTime() - new Date(inv.observed_at).getTime()) > 5 * 60 * 1000 : false;
  const stale = inv ? Date.now() - new Date(inv.received_at).getTime() > 3 * 60 * 1000 : false;

  return (
    <div className="ky-page ky-endpoint-page">
      <nav aria-label="Breadcrumb" className="ky-subnav"><Link to="/endpoints">Endpoints</Link>{e && <><span>/</span><Link to={envPath(org, e.environment_id)}>Environment</Link></>}</nav>
      <div className="ky-page-heading">
      <h1 style={{ fontSize: 24, display: 'flex', alignItems: 'center', gap: 10 }}>
        <Server size={24} style={{ color: 'var(--accent)' }} /><span>{e?.name ?? endpoint}</span>
        {e && <span className={`badge ${e.state === 'active' ? 'badge-success' : e.state === 'pending' ? 'badge-accent' : 'badge-danger'}`}>{e.state}</span>}
      </h1>
      <button className="btn-secondary" onClick={() => { details.reload(); inventory.reload(); samples.reload(); commands.reload(); ownership.reload(); }}>Refresh inventory</button>
      </div>
      <nav aria-label="Host resources" className="ky-resource-tabs">
        {tabs.map((item) => <button type="button" key={item} aria-pressed={shown === item} onClick={() => setView(item)}>{item[0].toUpperCase() + item.slice(1)}</button>)}
      </nav>
      <StateNotice state={details.state} onRetry={() => { details.reload(); inventory.reload(); }} />
      {shown === 'details' && details.state === 'ready' && e && (
        <section className="panel"><h2>Host details & identity</h2>
          <div className="panel-header"><h2 style={{ fontSize: 16 }}>Endpoint</h2></div>
          <dl className="ky-facts">
            <dt>Runtime</dt><dd>{e.runtime} {inv?.snapshot.engine.version ?? e.facts.runtime_version ?? ''}</dd>
            {cluster ? <><dt>Nodes</dt><dd>{inv?.snapshot.kubernetes?.nodes.length ?? e.facts.node_count ?? 'not reported yet'} · {e.facts.platform ?? ''}</dd></> : <>
            <dt>Host</dt><dd>{inv?.snapshot.engine.hostname ?? e.facts.hostname ?? '—'} · {inv?.snapshot.engine.os ?? e.facts.os ?? ''} {inv?.snapshot.engine.arch ?? ''}</dd>
            <dt>Capacity</dt><dd>{inv ? `${inv.snapshot.engine.cpus} CPUs · ${bytes(inv.snapshot.engine.memory_bytes)}` : 'not reported yet'}</dd></>}
            <dt>Key</dt><dd className="font-mono" style={{ fontSize: 11 }}>{e.fingerprint || '—'}</dd>
            <dt>Capabilities</dt><dd>{e.capabilities.length ? e.capabilities.join(', ') : 'none reported'}</dd>
          </dl>
        </section>
      )}
      {shown === 'activity' && <section className="panel"><div className="panel-header"><h2>Recent activity</h2></div><button className="btn-secondary" onClick={commands.reload}>Refresh activity</button><StateNotice state={commands.state} onRetry={commands.reload} />{Array.isArray(commands.data) && <ul className="ky-list">{commands.data.map((c) => <li key={c.id}>{c.action} · {c.container_id || c.reference} · {c.outcome || 'pending'}{c.detail ? ` — ${c.detail}` : ''}</li>)}</ul>}{commands.state === 'ready' && Array.isArray(commands.data) && commands.data.length === 0 && <EmptyNotice>No recent activity on this host.</EmptyNotice>}</section>}
      {inventory.state === 'notfound' && details.state === 'ready' && <EmptyNotice>No inventory yet. It arrives with the agent's first report after approval.</EmptyNotice>}
      {inventory.state !== 'notfound' && <StateNotice state={inventory.state} onRetry={inventory.reload} />}
      {inventory.state === 'ready' && inv && (
        <>
          <p role="status" style={{ color: stale ? 'var(--danger)' : 'var(--ink)', fontSize: 13 }}>
            Inventory generation {inv.generation}, received {ago(inv.received_at)}{stale ? ' (stale: no report for over three minutes)' : ''}{skew ? ' · agent clock differs from the server by more than five minutes' : ''}.
            {inv.snapshot.truncated?.length ? ` Lists truncated: ${inv.snapshot.truncated.join(', ')}.` : ''}
          </p>
          {cluster && shown === 'cluster' && e && (inv.snapshot.kubernetes ? <KubernetesCluster key={base} base={base} endpoint={e} inventory={inv.snapshot.kubernetes} /> : <EmptyNotice>The agent has not reported the cluster yet.</EmptyNotice>)}
          {shown === 'projects' && <><StateNotice state={ownership.state} onRetry={ownership.reload} /><ComposeProjects ownership={ownership.state === 'ready' && Array.isArray(ownership.data) ? ownership.data : null} containers={inv.snapshot.containers} truncated={inv.snapshot.truncated?.includes('containers') ?? false} onSelect={(name) => {
            setProjectFilter({ base, name }); setView('containers');
            requestAnimationFrame(() => document.getElementById('endpoint-containers')?.focus());
          }} /></>}
          {shown === 'containers' && <div id="endpoint-containers" tabIndex={-1}>
          <div className="ky-toolbar"><input type="search" aria-label="Find containers" placeholder="Search containers or images" value={search} onChange={(event) => setSearch(event.target.value)} /><span>{visibleContainers.length} containers</span></div>
          {selectedProject !== null && <p>Showing containers for <strong style={{ overflowWrap: 'anywhere' }}><bdi>{selectedProject}</bdi></strong>. <button className="btn-secondary" onClick={() => setProjectFilter(null)}>Show all containers</button></p>}
          <ResourceTable key={JSON.stringify([base, search, selectedProject])} title="Containers" rows={visibleContainers} empty={search ? "No matching containers." : "No containers on this host."} head={['Container', 'Status', 'Usage', 'Actions']} render={(c) => [<div className="ky-resource-name"><strong>{displayName(c.name)}</strong><span title={c.image}>{displayName(c.image)}</span><ContainerPorts ports={c.ports} />{c.compose_project && <small>{displayName(c.compose_project)}</small>}</div>, <span className={`badge ${c.state === 'running' ? 'badge-success' : c.state === 'exited' || c.state === 'dead' ? 'badge-danger' : 'badge-secondary'}`} title={c.status}>{displayName(c.state)}</span>, usage(c), <ContainerControls key={c.id} base={base} container={c} active={e?.state === 'active'} scope={`Host ${displayName(e?.name ?? endpoint)} · Endpoint ${endpoint}`} onRefresh={commands.reload} canExec={exec} />]} />
          </div>}
          {shown === 'images' && <><section className="panel"><h2>Pull an image</h2><ImageControls key={base} kind="pull" base={base} active={e?.state === 'active'} scope={`Host ${displayName(e?.name ?? endpoint)} · Endpoint ${endpoint}`} onActivity={commands.reload} /><p>Use an explicit tag or digest. A pull downloads an image; it does not update running containers.</p></section>
          <ResourceTable title="Images" rows={inv.snapshot.images} empty="No images on this host." head={['Tags', 'Size', 'ID', 'Actions']} render={(i) => [i.tags.map(displayName).join(', ') || '<untagged>', bytes(i.size_bytes), <span title={i.id}>{i.id.slice(0, 19)}</span>, <ImageControls key={`${base}/${i.id}`} kind="remove" imageID={i.id} base={base} active={e?.state === 'active'} scope={`Host ${displayName(e?.name ?? endpoint)} · Endpoint ${endpoint}`} onActivity={commands.reload} />]} />
          </>}
          {shown === 'networks' && <ResourceTable title="Networks" rows={inv.snapshot.networks} empty="No networks." head={['Name', 'Driver', 'Scope']} render={(n) => [displayName(n.name), displayName(n.driver), displayName(n.scope)]} />}
          {shown === 'volumes' && <ResourceTable title="Volumes" rows={inv.snapshot.volumes} empty="No volumes." head={['Name', 'Driver', 'Mountpoint']} render={(v) => [displayName(v.name), displayName(v.driver), displayName(v.mountpoint)]} />}
        </>
      )}
    </div>
  );
};
```

Replace `web/src/components/Endpoints.tsx` with (runtime selector; a cluster needs a name; the manifest region replaces the Docker one-liner; list rows show cluster health and node count):

```tsx
import React, { useState } from 'react';
import { secureFetch } from '../api';
import { tenantWrite, useTenantResource, type Endpoint, type EnrollmentToken } from '../tenant';
import { EmptyNotice, StateNotice } from './StateNotice';
import { Link } from './Link';
import { endpointPath } from '../router';

const terminal = (s: string) => s === 'revoked' || s === 'expired';
const healthBadge: Record<string, string> = { healthy: 'badge-success', degraded: 'badge-danger' };

// Fixed texts for the refusals a cluster enrollment can meet.
const mintRefusals: Record<string, string> = {
  agent_image_unpinned: 'Set KY_AGENT_IMAGE to a digest-pinned image before enrolling a cluster.',
  https_required: 'Kubernetes enrollment needs KY_APP_URL on HTTPS.',
};

// Names come from whoever redeemed the token. The server refuses control characters and line
// separators; the dialog strips the same classes again (C0, DEL, C1 and Unicode line and
// paragraph separators) and clamps the length before the name shares a prompt with a fingerprint.
export const displayName = (name: string) => {
  const clean = name.replace(/[\p{Cc}\p{Zl}\p{Zp}]/gu, '');
  return clean.length > 64 ? clean.slice(0, 63) + '…' : clean;
};

// Endpoints live under their environment because enrollment tokens are minted per environment.
export const Endpoints: React.FC<{ org: string; env: string }> = ({ org, env }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}`;
  const envBase = `${base}/environments/${encodeURIComponent(env)}`;
  const endpoints = useTenantResource<Endpoint[]>(`${envBase}/endpoints`);
  const [token, setToken] = useState<EnrollmentToken | null>(null);
  const [runtime, setRuntime] = useState<'docker' | 'kubernetes'>('docker');
  const [clusterName, setClusterName] = useState('');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);

  const mint = async () => {
    setBusy(true);
    setMessage('');
    try {
      const body = runtime === 'kubernetes' ? { runtime, name: clusterName.trim() } : { runtime };
      const resp = await secureFetch(`${envBase}/enrollment-tokens`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (!resp.ok) {
        const code = resp.status === 409 ? ((await resp.json().catch(() => ({}))) as { code?: string }).code ?? '' : '';
        setMessage(resp.status === 403 ? 'You do not have permission to enroll hosts here.' : mintRefusals[code] ?? (resp.status === 400 && runtime === 'kubernetes' ? 'Name the cluster in printable text, at most 255 bytes.' : `Could not create an enrollment token (${resp.status}).`));
        return;
      }
      setToken((await resp.json()) as EnrollmentToken);
    } catch {
      setMessage('Offline: the server could not be reached.');
    } finally {
      setBusy(false);
    }
  };
  const download = (t: EnrollmentToken) => {
    const url = URL.createObjectURL(new Blob([t.manifest ?? ''], { type: 'application/yaml' }));
    const link = document.createElement('a');
    link.href = url;
    link.download = t.manifest_file ?? 'kyyard-agent.yaml';
    link.click();
    URL.revokeObjectURL(url);
  };
  const acknowledgeKey = async (e: Endpoint) => {
    if (!e.pending_fingerprint) return;
    if (!window.confirm(`"${displayName(e.name)}" offered a new key.\n\nCurrent:  ${e.fingerprint}\nPending:  ${e.pending_fingerprint}\n\nAcknowledge only if the host printed the pending fingerprint. The current key stops working immediately.`)) return;
    setBusy(true);
    const err = await tenantWrite(`${base}/endpoints/${encodeURIComponent(e.id)}/keys/${encodeURIComponent(e.pending_fingerprint)}/acknowledge`, 'POST');
    setBusy(false);
    setMessage(err);
    if (!err) endpoints.reload();
  };
  const clearAlert = async (e: Endpoint, id: number) => {
    setBusy(true);
    const err = await tenantWrite(`${base}/endpoints/${encodeURIComponent(e.id)}/events/${id}/acknowledge`, 'POST');
    setBusy(false);
    setMessage(err);
    if (!err) endpoints.reload();
  };
  const act = async (e: Endpoint, action: 'approve' | 'reject' | 'revoke') => {
    const name = displayName(e.name);
    const prompts = {
      approve: `Approve "${name}" with key fingerprint\n\n${e.fingerprint}\n\nOnly approve if this matches the fingerprint the host printed.`,
      reject: `Reject the pending enrollment of "${name}"? The host will have to enroll again.`,
      revoke: `Revoke "${name}"? Its identity stops working immediately and cannot be restored.`,
    };
    if (!window.confirm(prompts[action])) return;
    setBusy(true);
    const err = await tenantWrite(`${base}/endpoints/${encodeURIComponent(e.id)}/${action}`, 'POST', action === 'approve' ? { fingerprint: e.fingerprint } : undefined);
    setBusy(false);
    setMessage(err);
    if (!err) endpoints.reload();
  };

  return (
    <section className="panel" aria-labelledby="endpoints-heading">
      <div className="panel-header" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12 }}>
        <h2 id="endpoints-heading" style={{ fontSize: 16 }}>Endpoints</h2>
        <button className="btn-secondary" onClick={endpoints.reload}>Refresh hosts</button>
        <label>Runtime <select aria-label="Runtime" value={runtime} onChange={(e) => setRuntime(e.target.value === 'kubernetes' ? 'kubernetes' : 'docker')}>
          <option value="docker">Docker host</option>
          <option value="kubernetes">Kubernetes cluster</option>
        </select></label>
        {runtime === 'kubernetes' && <input aria-label="Cluster name" placeholder="Cluster name" required maxLength={255} value={clusterName} onChange={(e) => setClusterName(e.target.value)} />}
        <button disabled={busy || (runtime === 'kubernetes' && clusterName.trim() === '')} onClick={() => void mint()}>{runtime === 'kubernetes' ? 'Enroll a cluster' : 'Enroll a host'}</button>
      </div>
      {token?.manifest && (
        <div className="dr-alert dr-alert-warn ky-enrollment" role="region" aria-label="Enrollment manifest">
          <p><strong>Shown once.</strong> Apply this manifest as cluster-admin before {new Date(token.expires_at).toLocaleTimeString()}:</p>
          <pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}>{token.command}</pre>
          <details><summary>Manifest</summary><pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}>{token.manifest}</pre></details>
          {token.note && <p>{token.note}</p>}
          <p>{token.disclosure}</p>
          <button onClick={() => download(token)}>Download manifest</button>{' '}
          <button className="btn-secondary" onClick={() => { void navigator.clipboard?.writeText(token.manifest ?? ''); }}>Copy manifest</button>{' '}
          <button className="btn-secondary" onClick={() => setToken(null)}>Dismiss</button>
        </div>
      )}
      {token && !token.manifest && (
        <div className="dr-alert dr-alert-warn ky-enrollment" role="region" aria-label="Enrollment command">
          <p><strong>Shown once.</strong> {token.command ? 'Run this on the remote host before' : 'Source enrollment token expires at'} {new Date(token.expires_at).toLocaleTimeString()}:</p>
          <pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}>{token.command ?? token.token}</pre>
          {token.note && <p>{token.note}</p>}
          <p>After starting the agent, refresh hosts below and approve the matching fingerprint. Then open the host to see its containers.</p>
          <details><summary>Source installation</summary>
            <p>Source builds can pass this token to kyyard-agent on stdin with --server set to the server address. Remote hosts require HTTPS.</p>
            <pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{token.token}</pre>
          </details>
          <p>{token.disclosure}</p>
          <button className="btn-secondary" onClick={() => { void navigator.clipboard?.writeText(token.command ?? token.token); }}>{token.command ? 'Copy command' : 'Copy token'}</button>
          <button className="btn-secondary" onClick={() => setToken(null)}>Dismiss</button>
        </div>
      )}
      <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
      {endpoints.state === 'ready' && endpoints.data && (endpoints.data.length === 0 ? <EmptyNotice>No endpoints yet. Enroll a host to begin.</EmptyNotice> : (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr><th>Name</th><th>State</th><th>Host</th><th>Fingerprint</th><th><span className="sr-only">Actions</span></th></tr></thead>
            <tbody>
              {endpoints.data.map((e) => (
                <tr key={e.id}>
                  <td data-label="Name"><Link to={endpointPath(org, e.id)}>{displayName(e.name)}</Link> <span className="font-mono" style={{ fontSize: 11, color: 'var(--ink)' }}>{e.runtime}</span>{e.cluster_health && <> <span className={`badge ${healthBadge[e.cluster_health] ?? 'badge-secondary'}`}>{e.cluster_health}</span></>}</td>
                  <td data-label="State"><span className={`badge ${e.state === 'pending' ? 'badge-accent' : e.state === 'active' ? 'badge-success' : 'badge-danger'}`}>{e.state}</span></td>
                  <td data-label="Host">{e.runtime === 'kubernetes' ? `${e.facts.node_count ?? '?'} nodes` : e.facts.hostname ?? ''} <span style={{ fontSize: 11, color: 'var(--ink)' }}>{e.facts.runtime_version ?? e.facts.server_version ?? ''}</span></td>
                  <td data-label="Fingerprint" className="font-mono" style={{ fontSize: 11 }}>
                    <span title={e.fingerprint}>{e.fingerprint ? e.fingerprint.slice(0, 16) + '…' : '—'}</span>
                    {e.pending_fingerprint && <div><span className="badge badge-accent">rotation pending</span> <span title={e.pending_fingerprint}>{e.pending_fingerprint.slice(0, 16)}…</span></div>}
                    {e.alerts.filter((a) => a.kind !== 'rotation_pending').map((a) => (
                      <div key={a.id} role="alert"><span className="badge badge-danger">{a.kind}</span> {a.details}{' '}
                        <button className="btn-secondary" disabled={busy} onClick={() => void clearAlert(e, a.id)}>Clear</button>
                      </div>
                    ))}
                  </td>
                  <td data-label="Actions">
                    {e.state === 'pending' && <>
                      <button disabled={busy} onClick={() => void act(e, 'approve')}>Approve</button>{' '}
                      <button className="btn-secondary" disabled={busy} onClick={() => void act(e, 'reject')}>Reject</button>
                    </>}
                    {e.pending_fingerprint && <><button disabled={busy} onClick={() => void acknowledgeKey(e)}>Acknowledge rotation</button>{' '}</>}
                    {!terminal(e.state) && e.state !== 'pending' && <button className="btn-danger" disabled={busy} onClick={() => void act(e, 'revoke')}>Revoke</button>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
      {message && <p role="alert" className="dr-alert dr-alert-error">{message}</p>}
    </section>
  );
};
```

- [ ] **Step 5: Run the tests to verify they pass, then build**

Run: `cd web && npx tsc -b && npx vitest run; cd ..`
Expected: every test file passes (26 files), including the four new tests.

Run: `make build-web && git status --short web/dist web/tsconfig.tsbuildinfo`
Expected: the build succeeds and `web/dist` shows the new hashed bundles. Do not run `make ci` while this runs.

- [ ] **Step 6: DOX and commit**

`web/AGENTS.md`, `## Local Contracts`, append:

```markdown
- The environment's enrollment has a runtime selector (Docker host / Kubernetes cluster). A cluster needs a name before "Enroll a cluster" enables; it posts `{runtime: "kubernetes", name}` and shows, once, the `kubectl apply -f <manifest_file>` command, the manifest (collapsed), note and RBAC disclosure, with Download manifest (a Blob named `manifest_file`) and Copy manifest; 409 `agent_image_unpinned` and `https_required` map to fixed texts. The Docker one-liner is unchanged. Endpoint rows show a Kubernetes endpoint's `cluster_health` badge and node count.
- `/organizations/{org}/endpoints/{endpoint}` for a `kubernetes` endpoint renders `KubernetesCluster.tsx` under the `Cluster` tab (with `Details`): cluster health and nodes, a namespace filter (falls back to all when the namespace leaves), workloads (ready/desired, images), pods (phase, node, summed restarts, containers with state and a Logs action opening the shared `ContainerLogs` dialog against `/pods/{namespace}/{pod}/logs` with `container`), services and volume claims, and an Applications panel saying Kubernetes deployment arrives in a later release. No container, image, network, volume, exec, inspect, deploy or update control is rendered; the status line's truncation notice covers the cluster lists. `ResourceTable.tsx` is the shared paged table.
```

`web/AGENTS.md`, `## Verification`: after "`src/components/Endpoints.test.tsx` covers the one-time enrollment command and fingerprint-bound approval" insert ", the Kubernetes runtime switch, manifest Download/Copy, the fixed 409 texts and the cluster row badge; `src/pages/EndpointPage.test.tsx` covers the read-only cluster view (no Docker tab or control, namespace filter, pod logs on the pod route)".

```bash
git add web && make tidy-check lint && git commit -m "feat(web): Kubernetes enrollment and read-only cluster view" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Documents, the implementation status and the full gate

**Files:**
- Modify: `README.md` (new `### Kubernetes endpoints` section before `## Persistent keys`)
- Modify: `docs/agent-protocol.md` (§1 agent line; new `## Kubernetes runtime` section at the end)
- Modify: `docs/authorization-matrix.md` (`endpoint.enroll`, `container.logs` rows)
- Modify: `docs/threat-model.md` (assets row, threats row)
- Modify: `KyYard-Implementation-Plan.md` (§8 status)

**Interfaces:**
- Consumes: every contract of Tasks 1–8, stated as built.
- Produces: operator and protocol documents; no code.

- [ ] **Step 1: README**

In `README.md`, insert directly before `## Persistent keys`:

```markdown
### Kubernetes endpoints

A Kubernetes cluster enrolls as an endpoint the way a Docker host does, read-only in this
release: inventory, cluster health and pod logs. Prerequisites: `KY_APP_URL` on HTTPS, a
digest-pinned agent image (discovered from the installed server image, or `KY_AGENT_IMAGE`),
and a cluster-admin kubeconfig for the one `kubectl apply`.

1. On the environment screen choose **Kubernetes cluster**, name the cluster and select
   **Enroll a cluster**. The manifest is shown once: download it (or copy it) and run the
   `kubectl apply -f kyyard-agent-<name>.yaml` command shown beside it.
2. Read the key fingerprint with `kubectl -n kyyard-agent logs deploy/kyyard-agent` and
   approve the matching endpoint.
3. Delete the spent enrollment Secret: `kubectl -n kyyard-agent delete secret kyyard-agent-enrollment`.
   The link inside it is single use, and the agent restarts from its identity Secret.

What the manifest creates, all labelled `app.kubernetes.io/name: kyyard-agent`,
`app.kubernetes.io/managed-by: kyyard`: the namespace `kyyard-agent`; the ServiceAccount
`kyyard-agent`; the ClusterRole and ClusterRoleBinding `kyyard-agent-read` (only `get` and
`list` on namespaces, nodes, pods, pod logs, events, services, persistentvolumeclaims,
deployments, statefulsets and daemonsets: no Secrets, no ConfigMaps, no `watch`, no
wildcard); the Role and RoleBinding `kyyard-agent-identity` (`get`/`create` Secrets in
`kyyard-agent` and `update` only on `kyyard-agent-identity`, where the agent keeps its
identity); the Secret `kyyard-agent-enrollment`; and a one-replica `Recreate` Deployment
running `/app/kyyard-agent --kubernetes` as UID 65532 with a read-only root filesystem, no
privilege escalation, every capability dropped, seccomp `RuntimeDefault`, and 50m/64Mi
requested, 500m/256Mi limited. Applying it needs cluster-admin because it creates cluster
RBAC; the agent itself holds only the read role above.

Container, image, network, volume, terminal, inspection and deployment actions are refused
for a cluster (`409 runtime_unsupported`) and not shown. Uninstall with
`kubectl delete -f kyyard-agent-<name>.yaml`, then revoke the endpoint. To enroll the same
cluster again after a revocation, delete the Secret `kyyard-agent-identity` (or uninstall)
before applying a new manifest: an identity from an old enrollment refuses a new link.
```

- [ ] **Step 2: Agent protocol**

In `docs/agent-protocol.md` §1, replace "(Docker first, Kubernetes later)" with "(Docker, and Kubernetes read-only; see Kubernetes runtime)". Append at the end of the file:

```markdown
## Kubernetes runtime

**Implemented (M8 PR 20), read-only.** An endpoint's runtime (`docker` or `kubernetes`) is fixed by its enrollment token. A cluster agent is `kyyard-agent --kubernetes` in the cluster, as the ServiceAccount the generated manifest creates; its identity lives in the Secret `kyyard-agent-identity` in its own namespace, and the enrollment link arrives by `--link-file`.

- Capabilities: a cluster agent advertises `kubernetes.inventory` and, when it can read logs, `pod.logs`, and nothing else; a Docker agent advertises neither. A `hello` that breaks this is answered `error {code: capability_mismatch, runtime}` and closed with `protocol_error`. Stored capabilities gate every handler, so a cluster can never satisfy a `container.*` or `deployment.*` check, and every Docker route answers 409 `runtime_unsupported` before any capability check.
- Snapshot: a cluster's `inventory` carries `kubernetes` (`KubernetesInventory`: `nodes` {name, kubelet_version, os, arch, ready, roles, unschedulable}, `namespaces`, `workloads` {kind Deployment|StatefulSet|DaemonSet, namespace, name, desired, ready, updated, images, paused}, `pods` {namespace, name, phase, node, owner_kind, owner_name, started_at, containers {name, image, image_id, state running|waiting|terminated, reason, ready, restart_count}}, `services` {namespace, name, type, cluster_ip, ports `port[:nodePort]/PROTOCOL`}, `claims` {namespace, name, phase, storage_class, capacity}) and empty Docker lists; a Docker snapshot carries no `kubernetes`. Either violation is `error {code: snapshot_rejected, runtime}` and the session continues. Caps: 500 nodes, 500 namespaces, 2000 workloads, 2000 pods, 2000 services, 1000 claims, 32 containers per pod, 32 images per workload, 16 roles per node, 32 ports per service; names 253 bytes, images 1 KiB, short fields 64 bytes; each list is decoded one element at a time, cut lists are named in `truncated`, and `MaxSnapshotBytes` stays 1 MiB. `engine` is `{runtime: kubernetes, version: <GitVersion>, api_version: <major.minor>, os, arch}`. The agent lists with `Limit` 500 and `Continue`, sorts by namespace then name, and reports a list it may not read as empty and truncated.
- Cluster health is derived on read, never stored: `cluster_health` on the endpoint and in the endpoint list, Kubernetes only: `degraded` when a reported node is not Ready, `healthy` when every node is reported and Ready, `unknown` otherwise (offline, no inventory, no node reported, or a truncated node list of Ready nodes).
- Pod logs: `log.open` carries `pod: {namespace, name, container?}` (`PodTarget`, DNS-1123 names) and no `container` ID; the two are exclusive. An empty container means the pod's only one; a pod with several is refused `unknown_container`, from the pod spec. Every log bound above applies unchanged; the agent also stops at 10,000 lines or 4 MiB before the sink. The HTTP route is `GET .../endpoints/{endpoint}/pods/{namespace}/{pod}/logs` under `container.logs` and the `pod.logs` capability.
- Enrollment facts from a cluster: `runtime`, `server_version`, `node_count`, `platform`.
```

- [ ] **Step 3: Authorization matrix and threat model**

In `docs/authorization-matrix.md`, in the `endpoint.enroll` row replace `one-time enrollment command` with `one-time enrollment command, or for a Kubernetes cluster the one-time manifest (the token only inside its enrollment Secret, returned only in this authenticated response)`; in the `container.logs` row replace `log bodies not stored` with `log bodies not stored; also covers pod logs on a Kubernetes endpoint (read workload logs)` and `naming the endpoint and the container asked for` with `naming the endpoint and the container, or the pod and container, asked for`.

In `docs/threat-model.md`, `## Assets`, in the row `| Agent identities and enrollment tokens |` replace `agent identity volume` with `agent identity volume, a cluster agent's identity Secret`. In `## Threats and mitigations`, insert after the `| Compromised agent |` row:

```markdown
| Compromised Kubernetes agent or its ServiceAccount token | The ClusterRole grants only `get`/`list` on namespaces, nodes, pods, pod logs, events, services, persistentvolumeclaims, deployments, statefulsets and daemonsets: no Secrets, no ConfigMaps, no `watch`, no wildcard, no write. Its only Secret access is a Role in its own namespace (`get`/`create`, `update` by name on its identity Secret), which holds nothing else. The agent never writes to the cluster in this release; every Docker action is refused for the endpoint at hello, route and UI. Residual: pod logs and the listed metadata (images, node names, service IPs) are readable cluster-wide by design; applying the manifest needs cluster-admin, so the person applying it can grant anything, and the manifest they apply is the one KyYard returned in an authenticated response. Two agents sharing one identity are stopped by the identity Secret's conflict rule | `TestManifestRBACIsReadOnlyWithoutSecrets`, `TestManifestOnARealCluster` (`KY_TEST_KUBECONFIG`, local only), `TestSecretIdentityStoreConflicts`, `TestHelloCapabilitiesMustFitTheRuntime`, `TestDockerRoutesRefuseAKubernetesEndpoint` |
```

- [ ] **Step 4: Implementation status**

In `KyYard-Implementation-Plan.md` §8, insert directly above the line beginning `Next:` (leave that line unchanged unless it names PR 20):

```markdown
Implemented M8 PR 20 (`feat/k8s-enrollment`): a Kubernetes cluster enrolls by a generated manifest applied with one `kubectl apply` (namespace, ServiceAccount, read-only ClusterRole without Secrets or ConfigMaps, a namespaced identity Role, the single-use enrollment Secret and a locked-down one-replica Deployment of the same pinned agent image). `kyyard-agent --kubernetes` reads the cluster with client-go (paged lists, no informers), keeps its identity in a Secret, and advertises only `kubernetes.inventory` and `pod.logs`; the server refuses any other capability at hello, holds snapshots to the runtime's shape, answers `runtime_unsupported` on every Docker route, and derives `cluster_health` on read. The endpoint page shows health, nodes, workloads, pods with their logs, services and claims read-only. Writes to the cluster, application mapping and reconciliation are PR 21; the real-cluster RBAC test runs locally with `KY_TEST_KUBECONFIG`, not in CI.
```

- [ ] **Step 5: The full gate**

Run: `make ci`
Expected: `==> Local CI checks passed` (tidy, lint, race suite with coverage, web tests, smoke).

Run: `PG=… go test -count=1 ./...`
Expected: every package `ok`.

If a disposable cluster is at hand: `KY_TEST_KUBECONFIG=$HOME/.kube/config go test -count=1 -run TestManifestOnARealCluster -v ./internal/runtime/kubernetes/`
Expected: PASS. Otherwise the real-cluster proof stays unproven and the PR description says so.

- [ ] **Step 6: Commit**

DOX pass: `README.md` and `docs/` are the root's operator documents (root `AGENTS.md` names them); no `AGENTS.md` contract changes in this task. Re-read the chain root → `internal/runtime` → `internal/runtime/kubernetes`, `internal/agent`, `internal/api`, `internal/store`, `web` and confirm each names the contracts Tasks 1–8 landed.

```bash
git add README.md docs KyYard-Implementation-Plan.md && make tidy-check lint && git commit -m "docs: Kubernetes endpoints, protocol, authorization and threat model" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```
