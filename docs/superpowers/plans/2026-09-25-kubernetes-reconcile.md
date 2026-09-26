# Kubernetes Application Reconciliation (M8 PR 21) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deploy the same stateless `compose.v1` definition to a Kubernetes endpoint: map the application to a namespace the cluster's manifest grants, plan with every image pinned at the registry, apply through the cluster agent as KyYard-labelled Deployments, Services, ConfigMaps and Secrets with a rollout wait, remove them by label, and stop every definition a Deployment cannot express at the plan with a named reason.

**Architecture:** The protocol gains a `KubernetesTarget` on deployment and removal frames, `SecretKeys`, Deployment identities, the cluster step codes and the `kubernetes.deploy`/`kubernetes.remove` capabilities, plus `KubernetesNames`, the one naming rule the server and the agent share without the server linking client-go (Task 1). The store adds migration 34 and the Kubernetes mapping (Task 2), then plans, frames, settles, removes, rolls back and checks updates for a namespace instance (Task 3). The agent renders objects in the new `render` package (Task 4) and applies and removes them with the typed clientset (Task 5). The manifest grants a deploy Role per listed namespace and a new route regenerates it (Task 6). The API's `dockerOnly` becomes `runtimeGate`, and mapping, plan, apply and removal take a cluster (Task 7). The web maps, plans, explains and lists (Task 8). Documents and the full gate close (Task 9).

**Tech Stack:** Go 1.26 (module `go 1.26.6`), `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` v0.37.1 (already required; no module is added), SQLite + PostgreSQL 17, the coder/websocket agent protocol (fake agent sockets in tests), React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-25-kubernetes-reconcile-design.md`

## Global Constraints

- Work in the worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/k8s-deploy` (branch `feat/k8s-reconcile`, on top of `master` 094c2ef and the committed spec). Every command below runs from its root.
- Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Gate each commit on the previous command's exit (`&&`).
- `gofmt -w` every edited Go file and check `gofmt -l cmd internal` prints nothing before each commit. Never put two single quotes or two backticks in a row in a Go comment: gofmt turns them into curly quotes. `make tidy-check lint` runs after `git add` (it diffs the working tree against the index) and before `git commit`. `make ci` must pass before the branch is pushed (Task 9). Never run `make build-web` while `make ci` runs: both run `npm ci` in `web/`.
- `web/dist` is embedded and committed: Task 8 rebuilds it with `make build-web` and commits it with `web/tsconfig.tsbuildinfo`.
- DOX: the task that changes a directory's contract updates that directory's `AGENTS.md` in the same commit.
- The server links no client-go: `go list -deps ./cmd/server | grep -c k8s.io` prints `0`. The new `internal/runtime/kubernetes/render` may import `k8s.io/api` and `k8s.io/apimachinery`; only `internal/runtime/kubernetes` imports it. The server imports `protocol` and `manifest`, neither of which imports a `k8s.io` package.
- Every store change is tested on SQLite and PostgreSQL. PostgreSQL runs use the local container `kyyard-access-pg`; read the password into a variable, never print it: `PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test ...`. Below this prefix is written `PG=… go test`.
- Real-cluster tests skip unless `KY_TEST_KUBECONFIG` is set; with it set, `KY_TEST_DEPLOY_IMAGE` must name a digest-pinned image that keeps running (for example `registry.k8s.io/pause@sha256:<digest>`).
- Migration: **34**, `kubernetes_namespaces` (the latest registered is 33, `deployment_policy_run`).
- Spec values, exactly: capabilities `kubernetes.deploy`, `kubernetes.remove` (a cluster agent never advertises `deployment.pull`); step codes `name_taken`, `rollout_timeout`, `forbidden`; unsupported codes `k8s_volume`, `k8s_host_ip`, `k8s_restart`, `k8s_name`, `k8s_namespace`; manifest namespaces DNS-1123 labels, at most 32, sorted, no duplicates; per namespace a `Role` and `RoleBinding` `kyyard-agent-deploy` granting `apps deployments` and core `services`, `configmaps` `get, list, create, update, patch, delete` and core `secrets` `get, create, update, patch, delete` (no `list`); `authorization.k8s.io selfsubjectaccessreviews create` in the ClusterRole; labels `app.kubernetes.io/name: <service>`, `app.kubernetes.io/instance: <project>`, `app.kubernetes.io/managed-by: kyyard`, `kyyard.busnes.app/application`, `kyyard.busnes.app/instance`, `kyyard.busnes.app/service`; annotations `kyyard.busnes.app/revision`, `kyyard.busnes.app/deployment`, `kyyard.busnes.app/spec-digest`; Deployment `<name>` one replica, `Recreate`, one container, image `image@sha256:...`, `envFrom` the ConfigMap `<name>-env` and, when present, the Secret `<name>-secret` (`Opaque`), `restartPolicy: Always`; Service `<name>` `ClusterIP`, `port` = published, `targetPort` = target; rollout poll 2 s; routes `PUT .../applications/{application}/mapping` with `{endpoint_id, namespace}` (400 `namespace_unknown`) and `POST /api/organizations/{organization}/endpoints/{endpoint}/manifest` with `{namespaces}` under `endpoint.enroll`.

Plan decisions where the code or the spec's silence forced a choice (each is reported to the reviewer; none changes a spec value):

1. **Plan refusal shape.** "400 `plan_blocked`, existing shape": the existing shape is 409 `preflight_blocked` with `blockers` and `services [{name, blockers, unsupported}]`. A service a Deployment cannot express carries the blocker `kubernetes_unsupported` with its `k8s_` codes in `unsupported`; `k8s_namespace` is a top-level blocker. The closed shape carries codes, so the volume's target path is not echoed; the UI sentence names the service.
2. **Images on a cluster.** A cluster reports no image inventory, so a Kubernetes plan resolves every service at the registry as an image update does (a tag to its digest; a digest reference pins itself), and every frame service is a `Pull` by digest with no `Tag`. No registry credential travels: the kubelet pulls (a private registry needs an imagePullSecret on the namespace's default ServiceAccount; the README says so). `PinImages` is refused for a cluster plan: rollback is ineligible there.
3. **Secret keys.** Every environment value of a `compose.v1` revision is an `ApplicationSecretRef` today, so `SecretKeys` lists every env key: the ConfigMap `<name>-env` is always rendered and empty until plain values exist, and the Secret is omitted for a service without environment.
4. **Names without client-go.** `protocol.KubernetesNames` computes names for the server (the plan's `Object`, settle, update checks) and for `render`. Every name starts with a letter (`ky-` is prefixed otherwise) because a Service name is a DNS-1035 label; the collision suffix is six hex characters of `sha256(project + "/" + service)`. A service or project that is not a label value, or a slug collision, is `k8s_name`.
5. **Project of a Kubernetes instance.** The mapping body is `{endpoint_id, namespace}`, so the project is the application's name slugged (`store.KubernetesProject`: lower-case letters and digits, other runs as one `-`, at most 63, `app` when empty). A second application slugging alike on the same cluster is 409 (the existing `UNIQUE(endpoint_id, project)`).
6. **Mapping lifecycle.** The first PUT creates the instance (mapping version 1, mapped revision the latest, `previous_revision` 0, no adopted identity); every later PUT is a review (version + 1, mapped revision the latest) and may move the namespace only while `current_revision` is 0. An instance on another endpoint, adopted on Docker, or applied in the namespace it would leave is 409 `application_adopted`. A Kubernetes mapping has no bindings to review, so a new revision does not block its plan with `mapping_requires_review`.
7. **Namespaces at enrollment.** A token's namespaces are stored on the token (migration 34 also adds `agent_enrollment_tokens.deploy_namespaces`) and copied to the endpoint at enrollment. `kyyard-agent` and `kube-*` are refused: the agent's own Secrets are its identity, and system namespaces are not deployment targets.
8. **The ClusterRole.** "The read ClusterRole is unchanged" and "`selfsubjectaccessreviews: create` in the existing ClusterRole" meet as: every read rule is unchanged, one access-review rule is added, and the manifest test exempts exactly that rule from its `get`/`list` assertion.
9. **RBAC-only manifest.** The regenerated file also carries the Namespace and ServiceAccount, so it applies on its own. A namespace dropped from a later list keeps its Role until deleted by hand; the manifest header, the route's note and the README say how.
10. **Manifest audit.** No new permission: the row is action `endpoint.enroll`, resource `<endpoint>/manifest`, details `namespaces=<sorted list>`.
11. **Docker-only steps.** A cluster result records `precondition`, `create` and `start` per service and omits `image`, `pull`, `volume`, `stop`, `rename`, `remove` and `recheck`: a skipped step may carry no detail (`DeploymentResult.Validate`), and ten steps per service would exceed `MaxDeploymentResultSteps`.
12. **Second conflict.** It fails the `create` step with the new step code `conflict`, detail `Kind/name`; `name_taken` takes the same optional detail (Docker's empty detail stays valid).
13. **Rollout detail.** At most three `key=Reason` pairs, keys `progressing`, `available`, `pod`, each reason a CamelCase word; anything else (messages included) is dropped.
14. **Spec digest on the wire.** `KubernetesTarget` also carries `spec_digest`, which the annotation needs and the frame did not have.
15. **Pod template.** It carries `kyyard.busnes.app/deployment`, so every apply rolls the pods and a changed ConfigMap or Secret value reaches the process, as a Docker apply recreates the container; pods set `automountServiceAccountToken: false` (a compose service has no use for API credentials).
16. **Removal frame.** `RemovalRequest` gains `Kubernetes` and `Services` (the latest and the last-applied revisions' services); the agent lists Deployments, Services and ConfigMaps by the instance label, gets each named service's Secret by name, and also removes services it finds labelled but not named.
17. **Runtime shape of an instance.** An instance must have its endpoint's shape: containers on Docker, a namespace on Kubernetes. A mismatch is `runtime_unsupported` from the store (mapping, preflight, plan, removal) and from the apply and removal handlers, which keeps `TestDockerRoutesRefuseAKubernetesEndpoint` meaningful now that those routes accept both runtimes.
18. **Update checks on a cluster.** `CheckImageUpdates` reads a service's local digest from its labelled Deployment's single `host/repo@digest` image in the inventory, so update policies see `update_available`.
19. **`MaxUnsupported`.** Raised from 32 to 40: five `k8s_` codes make the vocabulary 34.
20. **Real-cluster deploy image.** `KY_TEST_DEPLOY_IMAGE` is required once `KY_TEST_KUBECONFIG` is set; a pinned digest in the test would rot.

## Review Focus

1. Another tool (a Helm release, a hand-applied manifest) already owns `shop-web` in the namespace: the apply must stop before any write and name the object, never adopt or overwrite it. Pinned in Task 5 (`TestDeployRefusesBeforeWriting`: an unlabelled Deployment, another instance's Secret).
2. The administrator shrinks the namespace list, or never applies the regenerated manifest: the plan stops with `k8s_namespace`, and the agent refuses with `forbidden` from its own access review before touching anything. Pinned in Task 3 (`TestKubernetesPlanBlockers`, namespace no longer granted) and Task 5 (`TestDeployRefusesBeforeWriting`, "no grant").
3. An image that never pulls or crash-loops: the run ends `timed_out` with the cluster's reason words only, the objects stay as applied, every later step is skipped, and nothing the pod printed reaches the UI. Pinned in Task 5 (`TestDeployRolloutTimeout`, message text dropped) and Task 8 (`renders a Kubernetes plan, its step codes and Deployment identities`, markup dropped).
4. A second apply that changes only an environment value: the pods must roll and the Service must keep its cluster IP. Pinned in Task 5 (`TestDeployCreatesThenUpdatesOwnedObjects`: generation 2, ConfigMap updated, cluster IP kept) and Task 4 (`TestRenderTwoServices`: the pod template carries the deployment annotation).
5. Removal after a service left the definition, with a same-named Secret someone else owns: the dropped service's objects go, the foreign Secret stays, and Secrets are never listed. Pinned in Task 5 (`TestRemoveDeletesOnlyTheInstancesObjects`) and Task 3 (`TestRemoveKubernetesApplication`: the frame names the latest and applied revisions' services).

## File map

| Path | Task | Responsibility |
|---|---|---|
| `internal/agent/protocol/kubernetes_deploy.go` (new), `deployment.go`, `inspection.go`, `kubernetes.go` | 1 | `KubernetesTarget`, names, cluster frame validation, identities, step and unsupported codes, capabilities, workload labels |
| `internal/store/migrations/migrations.go`, `endpoints.go`, `models.go`, `store.go` | 2 | Migration 34, namespace lists, `SetEndpointDeployNamespaces` |
| `internal/store/application_mapping.go`, `application_adoption.go` | 2 | Kubernetes mapping, instance namespace |
| `internal/store/application_deployment.go`, `application_preflight.go`, `application_apply.go`, `application_spec.go`, `rollback.go`, `image_checks.go` | 3 | Kubernetes preflight, plan, frame, settle, removal, rollback eligibility, update checks |
| `internal/runtime/kubernetes/render/render.go` (new) | 4 | One service to Deployment, Service, ConfigMap, Secret |
| `internal/runtime/kubernetes/deploy.go`, `remove.go` (new), `kubernetes.go` | 5 | `Client.Deploy`, `Client.Remove`, workload labels |
| `internal/agent/client/connect.go`, `deployments.go`, `cmd/agent/main.go`, `internal/runtime/docker/{deploy,remove}.go` | 5 | Capabilities per runtime, frames held to the agent's runtime, wiring |
| `internal/runtime/kubernetes/manifest/manifest.go`, `internal/api/endpoint_handlers.go`, `server.go` | 6 | Deploy Roles per namespace, `RenderRBAC`, token namespaces, manifest route |
| `internal/api/runtime_gate.go`, `application_handlers.go`, the Docker route handlers, `tenant_handlers.go` | 7 | `runtimeGate`, application routes on a cluster, `namespace_unknown` |
| `web/src/components/Kubernetes{Mapping,Manifest}.tsx` (new), `KubernetesCluster.tsx`, `ApplicationAdoption.tsx`, `Applications.tsx`, `ApplicationDeploymentPlan.tsx`, `ApplicationPreflight.tsx`, `ApplicationInspection.tsx`, `ApplicationValidation.tsx`, `Endpoints.tsx`, `pages/EndpointPage.tsx`, `tenant.ts` | 8 | Mapping card, manifest regeneration, code tables, cluster Applications section, validation sentence |
| `README.md`, `docs/*.md`, `KyYard-Implementation-Plan.md` | 9 | Operator and protocol documents, status |

---

### Task 1: Protocol — cluster frames, identities, names and codes

**Files:**
- Create: `internal/agent/protocol/kubernetes_deploy.go`
- Test: `internal/agent/protocol/kubernetes_deploy_test.go`
- Modify: `internal/agent/protocol/deployment.go` (request, service, identity and removal fields; port and env validation shared; step codes)
- Modify: `internal/agent/protocol/inspection.go` (`UnsupportedCodes`, `MaxUnsupported`)
- Modify: `internal/agent/protocol/kubernetes.go` (capabilities, `Workload` labels)
- Modify tests: `internal/agent/protocol/deployment_test.go`, `inspection_test.go` (vocabulary counts)
- Docs: `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: `deploymentUUID`, `deploymentService`, `deploymentEnvName`, `imageID`, `ImagePull.valid`, `ValidDNSLabel`, `CleanText`, `knownCodes` (existing, package `protocol`).
- Produces (package `protocol`):
  ```go
  const CapabilityKubernetesDeploy, CapabilityKubernetesRemove = "kubernetes.deploy", "kubernetes.remove"
  type KubernetesTarget struct{ Namespace, ApplicationID, InstanceID, SpecDigest string } // json namespace, application_id, instance_id, spec_digest
  func (k KubernetesTarget) Validate() error
  const KindDeployment = "Deployment"
  const MaxKubeObjectName = 63
  func ValidLabelValue(s string) bool
  func ValidServiceName(s string) bool
  func KubernetesNames(project string, services []string) map[string]string
  func (r DeploymentRequest) ValidateFor(runtime string, now time.Time) error
  func (r RemovalRequest) ValidateFor(runtime string, now time.Time) error
  // DeploymentRequest gains  Kubernetes *KubernetesTarget `json:"kubernetes,omitempty"`
  // DeploymentService gains  SecretKeys []string `json:"secret_keys,omitempty"`
  // DeploymentIdentity gains Kind, Namespace, Name, UID string; Generation int64 (all omitempty)
  // RemovalRequest gains     Kubernetes *KubernetesTarget; Services []string `json:"services,omitempty"`
  // Workload gains           Application, Instance string `json:"application,omitempty"` / `json:"instance,omitempty"`
  // Step codes: forbidden, rollout_timeout (detail rolloutDetail), conflict (detail Kind/name);
  //   name_taken's detail is empty or Kind/name.
  // UnsupportedCodes ends with k8s_volume, k8s_host_ip, k8s_restart, k8s_name, k8s_namespace; MaxUnsupported = 40.
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/protocol/kubernetes_deploy_test.go`:

```go
package protocol

import (
	"strings"
	"testing"
	"time"
)

const (
	testDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testApp      = "11111111-2222-4333-8444-555555555555"
	testInstance = "66666666-7777-4888-9999-aaaaaaaaaaaa"
)

func goodKubernetesDeployment(now time.Time) DeploymentRequest {
	return DeploymentRequest{
		Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", Revision: 2, IssuedAt: now, Deadline: now.Add(5 * time.Minute),
		Kubernetes: &KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testDigest},
		Services: []DeploymentService{
			{Name: "web", Restart: "always", Pull: &ImagePull{Reference: "ghcr.io/org/web@" + testDigest, Digest: testDigest}, Ports: []Port{{Container: 80, Host: 8080, Protocol: "tcp"}}, Env: map[string]string{"MODE": "prod", "TOKEN": "x"}, SecretKeys: []string{"TOKEN"}, Mounts: []Mount{}},
			{Name: "api", Pull: &ImagePull{Reference: "ghcr.io/org/api@" + testDigest, Digest: testDigest}, Ports: []Port{{Container: 80, Host: 8080, Protocol: "tcp"}}, Env: map[string]string{}, Mounts: []Mount{}},
		},
	}
}

// A cluster frame carries its target and every service pulled by digest, with nothing a Docker
// host needs; a published port may repeat across services, each of which gets its own Service.
func TestKubernetesDeploymentRequestValidation(t *testing.T) {
	now := time.Now()
	if err := goodKubernetesDeployment(now).Validate(now); err != nil {
		t.Fatal(err)
	}
	if err := goodKubernetesDeployment(now).ValidateFor(RuntimeKubernetes, now); err != nil {
		t.Fatal(err)
	}
	if goodKubernetesDeployment(now).ValidateFor(RuntimeDocker, now) == nil || goodDeployment(now).ValidateFor(RuntimeKubernetes, now) == nil {
		t.Fatal("a frame crossed runtimes")
	}
	if err := goodDeployment(now).ValidateFor(RuntimeDocker, now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DeploymentRequest){
		"bad namespace":        func(r *DeploymentRequest) { r.Kubernetes.Namespace = "Shop" },
		"bad application":      func(r *DeploymentRequest) { r.Kubernetes.ApplicationID = "app" },
		"bad instance":         func(r *DeploymentRequest) { r.Kubernetes.InstanceID = "" },
		"bad spec digest":      func(r *DeploymentRequest) { r.Kubernetes.SpecDigest = "sha256:abc" },
		"container name":       func(r *DeploymentRequest) { r.Services[0].ContainerName = "shop-web" },
		"local image":          func(r *DeploymentRequest) { r.Services[0].ImageID = testDigest },
		"no pull":              func(r *DeploymentRequest) { r.Services[0].Pull = nil },
		"tag moved":            func(r *DeploymentRequest) { r.Services[0].Pull.Tag = "ghcr.io/org/web:1" },
		"replaces a container": func(r *DeploymentRequest) { r.Services[0].Replaces.ContainerID = strings.Repeat("b", 64) },
		"mounts": func(r *DeploymentRequest) {
			r.Services[0].Mounts = []Mount{{Kind: MountVolume, Source: "shop_data", Target: "/data"}}
		},
		"volumes":              func(r *DeploymentRequest) { r.Volumes = []string{"shop_data"} },
		"registry credential":  func(r *DeploymentRequest) { r.Registries = map[string]RegistryAuth{"ghcr.io": {Secret: "s"}} },
		"on-failure restart":   func(r *DeploymentRequest) { r.Services[0].Restart = "on-failure" },
		"no restart":           func(r *DeploymentRequest) { r.Services[0].Restart = "no" },
		"host address":         func(r *DeploymentRequest) { r.Services[0].Ports[0].HostIP = "127.0.0.1" },
		"port twice":           func(r *DeploymentRequest) { r.Services[0].Ports = append(r.Services[0].Ports, r.Services[0].Ports[0]) },
		"unknown secret key":   func(r *DeploymentRequest) { r.Services[0].SecretKeys = []string{"PASSWORD"} },
		"unsorted secret keys": func(r *DeploymentRequest) { r.Services[0].SecretKeys = []string{"TOKEN", "MODE"} },
		"repeated secret key":  func(r *DeploymentRequest) { r.Services[0].SecretKeys = []string{"TOKEN", "TOKEN"} },
		"duplicate service":    func(r *DeploymentRequest) { r.Services[1].Name = "web" },
		"env nul":              func(r *DeploymentRequest) { r.Services[0].Env["MODE"] = "a\x00b" },
	} {
		r := goodKubernetesDeployment(now)
		mutate(&r)
		if r.Validate(now) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	docker := goodDeployment(now)
	docker.Services[0].SecretKeys = []string{"TOKEN"}
	if docker.Validate(now) == nil {
		t.Error("a Docker frame carried secret keys")
	}
}

func TestKubernetesRemovalRequestValidation(t *testing.T) {
	now := time.Now()
	good := func() RemovalRequest {
		return RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(5 * time.Minute),
			Kubernetes: &KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testDigest}, Services: []string{"web", "api"}}
	}
	if err := good().ValidateFor(RuntimeKubernetes, now); err != nil {
		t.Fatal(err)
	}
	if good().ValidateFor(RuntimeDocker, now) == nil || goodRemoval(now).ValidateFor(RuntimeKubernetes, now) == nil {
		t.Fatal("a removal crossed runtimes")
	}
	for name, mutate := range map[string]func(*RemovalRequest){
		"no services":       func(r *RemovalRequest) { r.Services = nil },
		"bad service":       func(r *RemovalRequest) { r.Services[0] = "Web" },
		"repeated service":  func(r *RemovalRequest) { r.Services[1] = "web" },
		"containers":        func(r *RemovalRequest) { r.Containers = goodRemoval(now).Containers },
		"bad target":        func(r *RemovalRequest) { r.Kubernetes.Namespace = "" },
		"too many services": func(r *RemovalRequest) { r.Services = make([]string, MaxRemovalTargets+1) },
	} {
		r := good()
		mutate(&r)
		if r.Validate(now) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	docker := goodRemoval(now)
	docker.Services = []string{"web"}
	if docker.Validate(now) == nil {
		t.Error("a Docker removal named services")
	}
}

// Names are DNS-1123 labels that start with a letter, cut with a digest suffix when too long
// or shared, and distinct for every service of a definition.
func TestKubernetesNames(t *testing.T) {
	long := strings.Repeat("a", 64)
	got := KubernetesNames("Shop.Front", []string{"web", "my_api", "db-", "a-b", "a_b"})
	if got["web"] != "shop-front-web" || got["my_api"] != "shop-front-my-api" || got["db-"] != "shop-front-db" {
		t.Fatalf("names %v", got)
	}
	if got["a-b"] == got["a_b"] || !strings.HasPrefix(got["a-b"], "shop-front-a-b-") || len(got["a-b"]) != len("shop-front-a-b-")+6 {
		t.Fatalf("colliding slugs %q %q", got["a-b"], got["a_b"])
	}
	if n := KubernetesNames("1shop", []string{"web"})["web"]; n != "ky-1shop-web" {
		t.Fatalf("digit start %q", n)
	}
	cut := KubernetesNames(long, []string{"web", "api"})
	for _, n := range cut {
		if len(n) > MaxKubeObjectName || !ValidDNSLabel(n) || n[0] < 'a' || n[0] > 'z' {
			t.Fatalf("cut name %q", n)
		}
	}
	if cut["web"] == cut["api"] {
		t.Fatal("cut names collide")
	}
	if KubernetesNames(long, []string{"web"})["web"] != cut["web"] {
		t.Fatal("a name depends on its siblings when it does not collide")
	}
	if !ValidServiceName("my_api") || ValidServiceName("Web") || ValidServiceName("") {
		t.Fatal("service name grammar")
	}
	for _, v := range []string{"shop", "a.b_c-d", ""} {
		if !ValidLabelValue(v) {
			t.Errorf("%q refused", v)
		}
	}
	for _, v := range []string{"db-", "_db", long, "a b"} {
		if ValidLabelValue(v) {
			t.Errorf("%q accepted", v)
		}
	}
}

// A Deployment identity carries its namespace, name, UID, generation and pulled digest and no
// container field; a Docker identity carries none of the cluster fields.
func TestKubernetesIdentities(t *testing.T) {
	uid := "0f1e2d3c-4b5a-4968-8776-655443322110"
	good := DeploymentIdentity{Service: "web", Kind: KindDeployment, Namespace: "shop", Name: "shop-web", UID: uid, Generation: 3, ImageDigest: testDigest}
	result := func(ids ...DeploymentIdentity) DeploymentResult {
		return DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: OutcomeSucceeded, Steps: []DeploymentStep{}, Services: ids}
	}
	if err := result(good).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DeploymentIdentity){
		"other kind":     func(i *DeploymentIdentity) { i.Kind = "StatefulSet" },
		"bad namespace":  func(i *DeploymentIdentity) { i.Namespace = "Shop" },
		"bad name":       func(i *DeploymentIdentity) { i.Name = "shop_web" },
		"bad uid":        func(i *DeploymentIdentity) { i.UID = "x" },
		"no generation":  func(i *DeploymentIdentity) { i.Generation = 0 },
		"no digest":      func(i *DeploymentIdentity) { i.ImageDigest = "" },
		"container id":   func(i *DeploymentIdentity) { i.ContainerID = strings.Repeat("a", 64) },
		"docker created": func(i *DeploymentIdentity) { i.CreatedUnix = 1 },
	} {
		id := good
		mutate(&id)
		if result(id).Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	docker := DeploymentIdentity{Service: "web", ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	if err := result(docker).Validate(); err != nil {
		t.Fatal(err)
	}
	docker.Namespace = "shop"
	if result(docker).Validate() == nil {
		t.Error("a Docker identity carried a namespace")
	}
}

// The cluster step codes carry only their closed details.
func TestKubernetesStepCodes(t *testing.T) {
	for _, tc := range []struct {
		code, detail string
		ok           bool
	}{
		{"forbidden", "", true},
		{"forbidden", "create deployments", false},
		{"name_taken", "", true},
		{"name_taken", "Deployment/shop-web", true},
		{"name_taken", "Secret/shop-web-secret", true},
		{"name_taken", "Pod/shop-web", false},
		{"name_taken", "Deployment/Shop Web", false},
		{"conflict", "ConfigMap/shop-web-env", true},
		{"rollout_timeout", "", true},
		{"rollout_timeout", "progressing=ProgressDeadlineExceeded,available=MinimumReplicasUnavailable,pod=ImagePullBackOff", true},
		{"rollout_timeout", "pod=CrashLoopBackOff", true},
		{"rollout_timeout", "pod=back-off pulling image", false},
		{"rollout_timeout", "node=NotReady", false},
		{"unsupported", "k8s_volume,k8s_restart", true},
	} {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: OutcomeFailed, Code: ResultStepFailed, Services: []DeploymentIdentity{},
			Steps: []DeploymentStep{{Service: "web", Step: StepStart, Outcome: OutcomeFailed, Code: tc.code, Detail: tc.detail}}}
		if err := r.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s %q: %v", tc.code, tc.detail, err)
		}
	}
}

// A cluster agent may advertise deploy and remove; deployment.pull and deployment.apply stay
// Docker capabilities, and a Docker agent cannot claim the cluster ones.
func TestCapabilitiesFitClusterDeployment(t *testing.T) {
	if !CapabilitiesFit(RuntimeKubernetes, []string{CapabilityKubernetesInventory, CapabilityPodLogs, CapabilityKubernetesDeploy, CapabilityKubernetesRemove}) {
		t.Fatal("cluster deployment capabilities refused")
	}
	for _, c := range []string{CapabilityDeploymentPull, CapabilityDeploymentApply, CapabilityDeploymentRemove} {
		if CapabilitiesFit(RuntimeKubernetes, []string{CapabilityKubernetesInventory, c}) {
			t.Errorf("%s fits a cluster", c)
		}
	}
	for _, c := range []string{CapabilityKubernetesDeploy, CapabilityKubernetesRemove} {
		if CapabilitiesFit(RuntimeDocker, []string{c}) {
			t.Errorf("%s fits a Docker host", c)
		}
	}
}

// A workload's KyYard labels are cut like every short field.
func TestWorkloadLabelsClamp(t *testing.T) {
	s := &Snapshot{Kubernetes: &KubernetesInventory{Workloads: []Workload{{Kind: "Deployment", Namespace: "shop", Name: "shop-web", Application: strings.Repeat("a", 100), Instance: "i\nx"}}}}
	Clamp(s)
	w := s.Kubernetes.Workloads[0]
	if len(w.Application) != MaxKubeShortBytes || w.Instance != "ix" {
		t.Fatalf("labels %q %q", w.Application, w.Instance)
	}
}
```

In `internal/agent/protocol/deployment_test.go`:

Replace

```go
		}
	}
	if len(stepCodes) != 30 || len(resultCodes) != 8 {
		t.Fatalf("closed sets: %d step codes, %d result codes", len(stepCodes), len(resultCodes))
	}
```

with

```go
		}
	}
	if len(stepCodes) != 33 || len(resultCodes) != 8 {
		t.Fatalf("closed sets: %d step codes, %d result codes", len(stepCodes), len(resultCodes))
	}
```

In `internal/agent/protocol/inspection_test.go`:

Replace

```go
		}
	}
	if len(UnsupportedCodes) != 29 || len(UnsupportedCodes) > MaxUnsupported || UnsupportedCodes[0] != "mount_type" || UnsupportedCodes[28] != "image_config" {
		t.Fatalf("vocabulary: %v", UnsupportedCodes)
	}
```

with

```go
		}
	}
	if len(UnsupportedCodes) != 34 || len(UnsupportedCodes) > MaxUnsupported || UnsupportedCodes[0] != "mount_type" || UnsupportedCodes[28] != "image_config" || UnsupportedCodes[33] != "k8s_namespace" {
		t.Fatalf("vocabulary: %v", UnsupportedCodes)
	}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agent/protocol/`
Expected: FAIL to compile: `undefined: KubernetesTarget`, `undefined: KubernetesNames`, `r.ValidateFor undefined`, `unknown field Kind in struct literal of type DeploymentIdentity`.

- [ ] **Step 3: Implement**

Create `internal/agent/protocol/kubernetes_deploy.go`:

```go
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Cluster deployment capabilities. A cluster agent advertises them when it can apply and remove
// an application's objects; it never advertises deployment.pull, because the kubelet pulls.
const (
	CapabilityKubernetesDeploy = "kubernetes.deploy"
	CapabilityKubernetesRemove = "kubernetes.remove"
)

// KubernetesTarget is where a cluster agent applies or removes an instance's objects, and what
// it labels and annotates them with. A request carries it exactly when the endpoint's runtime is
// kubernetes (ValidateFor).
type KubernetesTarget struct {
	Namespace     string `json:"namespace"`
	ApplicationID string `json:"application_id"`
	InstanceID    string `json:"instance_id"`
	SpecDigest    string `json:"spec_digest"`
}

func (k KubernetesTarget) Validate() error {
	if !ValidDNSLabel(k.Namespace) || !deploymentUUID.MatchString(k.ApplicationID) || !deploymentUUID.MatchString(k.InstanceID) || !imageID.MatchString(k.SpecDigest) {
		return errors.New("a Kubernetes target names a namespace, the application and instance UUIDs and the spec digest")
	}
	return nil
}

// ValidServiceName is the grammar of a service name on the wire: a cluster agent reads it back
// from its objects' labels.
func ValidServiceName(s string) bool { return deploymentService.MatchString(s) }

// KindDeployment is the one object kind a Kubernetes identity names.
const KindDeployment = "Deployment"

// MaxKubeObjectName bounds a Deployment's and a Service's name: a DNS-1123 label.
const MaxKubeObjectName = 63

var labelValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)

// ValidLabelValue is Kubernetes label value syntax: at most 63 characters, alphanumeric at
// both ends, '-', '_' and '.' between.
func ValidLabelValue(s string) bool { return labelValue.MatchString(s) }

// KubernetesNames maps each service to the name of its Deployment and Service:
// <project>-<service> lower-cased, '_' and '.' read as '-', prefixed "ky-" when it would not
// start with a letter (a Service name must). A name longer than 63 characters, or one two
// services share, is cut and suffixed with six hex characters of a digest of project and
// service, so every name is distinct.
func KubernetesNames(project string, services []string) map[string]string {
	base := make(map[string]string, len(services))
	count := map[string]int{}
	for _, s := range services {
		slug := strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				return r
			case r >= 'A' && r <= 'Z':
				return r + 'a' - 'A'
			}
			return '-'
		}, project+"-"+s)
		slug = strings.Trim(slug, "-")
		if slug == "" || slug[0] < 'a' || slug[0] > 'z' {
			slug = "ky-" + slug
		}
		base[s] = slug
		count[slug]++
	}
	out := make(map[string]string, len(services))
	for _, s := range services {
		name := base[s]
		if len(name) > MaxKubeObjectName || count[name] > 1 {
			sum := sha256.Sum256([]byte(project + "/" + s))
			name = strings.TrimRight(name[:min(len(name), MaxKubeObjectName-7)], "-") + "-" + hex.EncodeToString(sum[:])[:6]
		}
		out[s] = name
	}
	return out
}

var kubernetesRestart = map[string]bool{"": true, "always": true, "unless-stopped": true}

// validateKubernetes is Validate for a cluster frame: no Docker field, every image pulled by
// digest (the kubelet pulls, so no tag moves and no credential travels), and secret keys named
// among the environment's.
func (r DeploymentRequest) validateKubernetes() error {
	if r.Kubernetes.Validate() != nil || len(r.Volumes) > 0 || len(r.Registries) > 0 {
		return errors.New("invalid Kubernetes deployment")
	}
	names := map[string]bool{}
	for _, s := range r.Services {
		if !deploymentService.MatchString(s.Name) || names[s.Name] || s.ContainerName != "" || s.ImageID != "" || s.Pull == nil || !s.Pull.valid() || s.Pull.Tag != "" || s.Replaces != (InspectionTarget{}) || len(s.Mounts) > 0 || !kubernetesRestart[s.Restart] {
			return errors.New("invalid Kubernetes service")
		}
		names[s.Name] = true
		if err := validPorts(s.Ports, map[binding]bool{}, false); err != nil {
			return err
		}
		if err := validEnv(s.Env); err != nil {
			return err
		}
		for i, k := range s.SecretKeys {
			if _, ok := s.Env[k]; !ok || (i > 0 && s.SecretKeys[i-1] >= k) {
				return errors.New("invalid secret keys")
			}
		}
	}
	return nil
}

// ValidateFor is Validate holding the frame to the agent's runtime: a Kubernetes target exactly
// when the runtime is kubernetes.
func (r DeploymentRequest) ValidateFor(runtime string, now time.Time) error {
	if (r.Kubernetes != nil) != (runtime == RuntimeKubernetes) {
		return errors.New("the deployment does not match the agent's runtime")
	}
	return r.Validate(now)
}

// ValidateFor is RemovalRequest.Validate holding the frame to the agent's runtime.
func (r RemovalRequest) ValidateFor(runtime string, now time.Time) error {
	if (r.Kubernetes != nil) != (runtime == RuntimeKubernetes) {
		return errors.New("the removal does not match the agent's runtime")
	}
	return r.Validate(now)
}

// valid holds an identity to one of two shapes: a Docker container, or a Kubernetes Deployment
// with its namespace, name, UID, generation and the digest its pod runs.
func (id DeploymentIdentity) valid() bool {
	if id.Kind == "" {
		return id.Namespace == "" && id.Name == "" && id.UID == "" && id.Generation == 0 && (InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() == nil && (id.ImageDigest == "" || imageID.MatchString(id.ImageDigest))
	}
	return id.Kind == KindDeployment && id.ContainerID == "" && id.ImageID == "" && id.CreatedUnix == 0 && ValidDNSLabel(id.Namespace) && ValidDNSLabel(id.Name) && deploymentUUID.MatchString(id.UID) && id.Generation >= 1 && imageID.MatchString(id.ImageDigest)
}

var (
	// kubeObject is a name_taken or conflict detail: the object's kind and name.
	kubeObject = regexp.MustCompile(`^(Deployment|Service|ConfigMap|Secret)/[a-z0-9]([-a-z0-9.]{0,241}[a-z0-9])?$`)
	// rolloutDetail is a rollout_timeout detail: up to three reasons, each a Kubernetes reason
	// word under the condition or pod it came from.
	rolloutDetail = regexp.MustCompile(`^((progressing|available|pod)=[A-Za-z]{1,64}(,(progressing|available|pod)=[A-Za-z]{1,64}){0,2})?$`)
)
```

In `internal/agent/protocol/deployment.go`:

Replace

```go
	detailContainerID            // a full 64-hex Docker ID
	detailStatus                 // a 3-digit status
)
```

with

```go
	detailContainerID            // a full 64-hex Docker ID
	detailStatus                 // a 3-digit status
	detailObject                 // empty, or a Kubernetes object's Kind/name
	detailRollout                // a rollout_timeout's reasons
)
```

Replace

```go
	"volume_missing": detailNone, "volume_create_failed": detailNone, "image_missing": detailNone,
	"pinned_image_missing": detailNone, "configuration_drift": detailNone, "name_reserved": detailNone,
	"name_taken": detailNone, "identity_unusable": detailNone, "identity_unreadable": detailContainerID,
	"identity_unverified": detailContainerID, "dependents": detailNone, "deadline": detailNone,
	"pull_failed": detailNone, "pull_unauthorized": detailNone, "pull_not_found": detailNone,
	"pull_digest_mismatch": detailNone, "cancelled": detailNone, "runtime_timeout": detailNone,
	"runtime_error": detailNone, "runtime_status": detailStatus, CodeLegacy: detailNone,
}
```

with

```go
	"volume_missing": detailNone, "volume_create_failed": detailNone, "image_missing": detailNone,
	"pinned_image_missing": detailNone, "configuration_drift": detailNone, "name_reserved": detailNone,
	"name_taken": detailObject, "identity_unusable": detailNone, "identity_unreadable": detailContainerID,
	"identity_unverified": detailContainerID, "dependents": detailNone, "deadline": detailNone,
	"pull_failed": detailNone, "pull_unauthorized": detailNone, "pull_not_found": detailNone,
	"pull_digest_mismatch": detailNone, "cancelled": detailNone, "runtime_timeout": detailNone,
	"runtime_error": detailNone, "runtime_status": detailStatus, CodeLegacy: detailNone,
	"forbidden": detailNone, "rollout_timeout": detailRollout, "conflict": detailObject,
}
```

Replace

```go
	case rule == detailStatus:
		return statusDetail.MatchString(detail)
	}
	return detail == ""
```

with

```go
	case rule == detailStatus:
		return statusDetail.MatchString(detail)
	case rule == detailObject:
		return detail == "" || kubeObject.MatchString(detail)
	case rule == detailRollout:
		return rolloutDetail.MatchString(detail)
	}
	return detail == ""
```

Replace

```go
	// service mounts.
	Volumes []string `json:"volumes,omitempty"`
}
type DeploymentService struct {
```

with

```go
	// service mounts.
	Volumes []string `json:"volumes,omitempty"`
	// Kubernetes is set exactly for a cluster endpoint: the namespace and labels of its objects.
	Kubernetes *KubernetesTarget `json:"kubernetes,omitempty"`
}
type DeploymentService struct {
```

Replace

```go
	// Always sent: a nil list is invalid.
	Mounts []Mount `json:"mounts"`
}
```

with

```go
	// Always sent: a nil list is invalid.
	Mounts []Mount `json:"mounts"`
	// SecretKeys are the Env keys backed by a secret reference, sorted; Kubernetes only. The
	// agent puts them in the service's Secret and the rest in its ConfigMap.
	SecretKeys []string `json:"secret_keys,omitempty"`
}
```

Replace

```go
}

// Validate refuses anything the adapter would have to guess about. Every bound here is a
// wire bound as well: PR B rejects a frame that fails it before touching the runtime.
```

with

```go
}

// binding is one published port as the runtime binds it.
type binding struct {
	ip       string
	port     int
	protocol string
}

// validPorts checks ports and records their bindings in used, refusing one already there. A host
// address is allowed only where hostIP is: a Kubernetes Service has none.
func validPorts(ports []Port, used map[binding]bool, hostIP bool) error {
	if len(ports) > MaxDeploymentPorts {
		return errors.New("too many ports")
	}
	for _, p := range ports {
		if p.Container < 1 || p.Container > 65535 || p.Host < 1 || p.Host > 65535 || (p.Protocol != "tcp" && p.Protocol != "udp") || (p.HostIP != "" && !hostIP) {
			return errors.New("invalid port")
		}
		// "" is Docker's IPv4 wildcard; 0.0.0.0 and :: are separate binds Docker allows together.
		b := binding{"v4-any", p.Host, p.Protocol}
		if p.HostIP != "" {
			ip, err := netip.ParseAddr(p.HostIP)
			if err != nil || ip.Zone() != "" {
				return errors.New("invalid host address")
			}
			switch {
			case ip.IsUnspecified() && ip.Is4():
			case ip.IsUnspecified():
				b.ip = "v6-any"
			default:
				b.ip = ip.String()
			}
		}
		if used[b] {
			return errors.New("duplicate port binding")
		}
		used[b] = true
	}
	return nil
}

func validEnv(env map[string]string) error {
	if len(env) > MaxDeploymentEnvEntries {
		return errors.New("too many environment entries")
	}
	total := 0
	for k, v := range env {
		total += len(k) + len(v)
		if !deploymentEnvName.MatchString(k) || len(v) > MaxDeploymentEnvValueBytes || !utf8.ValidString(v) || strings.ContainsRune(v, 0) || total > MaxDeploymentEnvBytes {
			return errors.New("invalid environment value")
		}
	}
	return nil
}

// Validate refuses anything the adapter would have to guess about. Every bound here is a
// wire bound as well: PR B rejects a frame that fails it before touching the runtime.
```

Replace

```go
		return errors.New("invalid service count")
	}
	type binding struct {
		ip       string
		port     int
		protocol string
	}
	names, containers, replaces, bindings, pulled, mounted := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[binding]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range r.Services {
		image := (s.Pull == nil && fullImageID(s.ImageID)) || (s.Pull != nil && s.ImageID == "" && s.Pull.valid())
		if !deploymentService.MatchString(s.Name) || names[s.Name] || !ValidContainerID(s.ContainerName) || containers[s.ContainerName] || !image || s.Replaces.Validate() != nil || replaces[s.Replaces.ContainerID] || !deploymentRestart[s.Restart] {
			return errors.New("invalid deployment service")
		}
```

with

```go
		return errors.New("invalid service count")
	}
	if r.Kubernetes != nil {
		return r.validateKubernetes()
	}
	names, containers, replaces, bindings, pulled, mounted := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[binding]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range r.Services {
		image := (s.Pull == nil && fullImageID(s.ImageID)) || (s.Pull != nil && s.ImageID == "" && s.Pull.valid())
		if !deploymentService.MatchString(s.Name) || names[s.Name] || !ValidContainerID(s.ContainerName) || containers[s.ContainerName] || !image || s.Replaces.Validate() != nil || replaces[s.Replaces.ContainerID] || !deploymentRestart[s.Restart] || len(s.SecretKeys) > 0 {
			return errors.New("invalid deployment service")
		}
```

Replace

```go
			return errors.New("invalid mount")
		}
		if len(s.Ports) > MaxDeploymentPorts {
			return errors.New("too many ports")
		}
		for _, p := range s.Ports {
			if p.Container < 1 || p.Container > 65535 || p.Host < 1 || p.Host > 65535 || (p.Protocol != "tcp" && p.Protocol != "udp") {
				return errors.New("invalid port")
			}
			// "" is Docker's IPv4 wildcard; 0.0.0.0 and :: are separate binds Docker allows together.
			b := binding{"v4-any", p.Host, p.Protocol}
			if p.HostIP != "" {
				ip, err := netip.ParseAddr(p.HostIP)
				if err != nil || ip.Zone() != "" {
					return errors.New("invalid host address")
				}
				switch {
				case ip.IsUnspecified() && ip.Is4():
				case ip.IsUnspecified():
					b.ip = "v6-any"
				default:
					b.ip = ip.String()
				}
			}
			if bindings[b] {
				return errors.New("duplicate port binding")
			}
			bindings[b] = true
		}
		if len(s.Env) > MaxDeploymentEnvEntries {
			return errors.New("too many environment entries")
		}
		total := 0
		for k, v := range s.Env {
			total += len(k) + len(v)
			if !deploymentEnvName.MatchString(k) || len(v) > MaxDeploymentEnvValueBytes || !utf8.ValidString(v) || strings.ContainsRune(v, 0) || total > MaxDeploymentEnvBytes {
				return errors.New("invalid environment value")
			}
		}
	}
```

with

```go
			return errors.New("invalid mount")
		}
		if err := validPorts(s.Ports, bindings, true); err != nil {
			return err
		}
		if err := validEnv(s.Env); err != nil {
			return err
		}
	}
```

Replace

```go
	Detail string `json:"detail"`
}
type DeploymentIdentity struct {
	Service     string `json:"service"`
```

with

```go
	Detail string `json:"detail"`
}

// DeploymentIdentity is what a service now runs as: a Docker container, or on a cluster
// (Kind set) the Deployment's namespace, name, UID and generation. ImageDigest is the pulled
// repository digest, always set for a Deployment.
type DeploymentIdentity struct {
	Service     string `json:"service"`
```

Replace

```go
	ImageID     string `json:"image_id"`
	CreatedUnix int64  `json:"created_unix"`
	ImageDigest string `json:"image_digest,omitempty"` // the pulled repository digest
}
```

with

```go
	ImageID     string `json:"image_id"`
	CreatedUnix int64  `json:"created_unix"`
	ImageDigest string `json:"image_digest,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name,omitempty"`
	UID         string `json:"uid,omitempty"`
	Generation  int64  `json:"generation,omitempty"`
}
```

Replace

```go
	}
	for _, id := range r.Services {
		if !deploymentService.MatchString(id.Service) || (InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() != nil || (id.ImageDigest != "" && !imageID.MatchString(id.ImageDigest)) {
			return errors.New("invalid deployment identity")
		}
```

with

```go
	}
	for _, id := range r.Services {
		if !deploymentService.MatchString(id.Service) || !id.valid() {
			return errors.New("invalid deployment identity")
		}
```

Replace

```go
	Deadline   time.Time       `json:"deadline"`
	Containers []RemovalTarget `json:"containers"`
}
type RemovalTarget struct {
```

with

```go
	Deadline   time.Time       `json:"deadline"`
	Containers []RemovalTarget `json:"containers"`
	// Kubernetes is set exactly for a cluster endpoint; Containers is then empty and Services
	// names every service whose objects the agent deletes.
	Kubernetes *KubernetesTarget `json:"kubernetes,omitempty"`
	Services   []string          `json:"services,omitempty"`
}
type RemovalTarget struct {
```

Replace

```go
		return errors.New("invalid removal deadline")
	}
	if len(r.Containers) == 0 || len(r.Containers) > MaxRemovalTargets {
		return errors.New("invalid container count")
	}
```

with

```go
		return errors.New("invalid removal deadline")
	}
	if r.Kubernetes != nil {
		if r.Kubernetes.Validate() != nil || len(r.Containers) > 0 || len(r.Services) == 0 || len(r.Services) > MaxRemovalTargets {
			return errors.New("invalid Kubernetes removal")
		}
		seen := map[string]bool{}
		for _, s := range r.Services {
			if !deploymentService.MatchString(s) || seen[s] {
				return errors.New("invalid Kubernetes removal")
			}
			seen[s] = true
		}
		return nil
	}
	if len(r.Services) > 0 || len(r.Containers) == 0 || len(r.Containers) > MaxRemovalTargets {
		return errors.New("invalid container count")
	}
```

In `internal/agent/protocol/inspection.go`:

Replace

```go
// UnsupportedCodes is the closed vocabulary of ContainerInspection.Unsupported, in the order the
// Docker adapter reports them. Each is configuration the definition cannot express.
var UnsupportedCodes = []string{"mount_type", "anonymous_volume", "volumes_from", "volume_driver", "mount_options", "tmpfs", "auto_remove", "read_only_rootfs", "privileged", "capabilities", "security_opt", "devices", "pid_mode", "ipc_mode", "user", "runtime", "resource_limits", "ulimits", "sysctls", "device_requests", "init", "userns_mode", "cgroup_parent", "group_add", "extra_hosts", "dns", "links", "network", "image_config"}

// knownCodes accepts at most MaxUnsupported distinct codes from UnsupportedCodes.
```

with

```go
// UnsupportedCodes is the closed vocabulary of ContainerInspection.Unsupported, in the order the
// Docker adapter reports them, then the k8s_ codes a plan for a Kubernetes endpoint refuses. Each
// is configuration the target runtime cannot take from the definition.
var UnsupportedCodes = []string{"mount_type", "anonymous_volume", "volumes_from", "volume_driver", "mount_options", "tmpfs", "auto_remove", "read_only_rootfs", "privileged", "capabilities", "security_opt", "devices", "pid_mode", "ipc_mode", "user", "runtime", "resource_limits", "ulimits", "sysctls", "device_requests", "init", "userns_mode", "cgroup_parent", "group_add", "extra_hosts", "dns", "links", "network", "image_config", "k8s_volume", "k8s_host_ip", "k8s_restart", "k8s_name", "k8s_namespace"}

// knownCodes accepts at most MaxUnsupported distinct codes from UnsupportedCodes.
```

Replace

```go
	// MaxRestartCount bounds a reported restart count.
	MaxRestartCount = 1_000_000
	MaxUnsupported  = 32
)
```

with

```go
	// MaxRestartCount bounds a reported restart count.
	MaxRestartCount = 1_000_000
	MaxUnsupported  = 40
)
```

In `internal/agent/protocol/kubernetes.go`:

Replace

```go
	Images    []string `json:"images"`
	Paused    bool     `json:"paused"`
}
```

with

```go
	Images    []string `json:"images"`
	Paused    bool     `json:"paused"`
	// Application and Instance are the KyYard labels of a workload KyYard deployed, else empty.
	Application string `json:"application,omitempty"`
	Instance    string `json:"instance,omitempty"`
}
```

Replace

```go
		w := &k.Workloads[i]
		w.Kind, w.Namespace, w.Name = short(w.Kind), name(w.Namespace), name(w.Name)
		w.Images = cleanList(w.Images, MaxWorkloadImages, MaxKubeImageBytes)
	}
```

with

```go
		w := &k.Workloads[i]
		w.Kind, w.Namespace, w.Name = short(w.Kind), name(w.Namespace), name(w.Name)
		w.Application, w.Instance = short(w.Application), short(w.Instance)
		w.Images = cleanList(w.Images, MaxWorkloadImages, MaxKubeImageBytes)
	}
```

Replace

```go
// kubernetesCapabilities is everything a cluster agent may advertise.
var kubernetesCapabilities = map[string]bool{CapabilityKubernetesInventory: true, CapabilityPodLogs: true}

// CapabilitiesFit reports whether a hello's capabilities belong to the endpoint's runtime:
```

with

```go
// kubernetesCapabilities is everything a cluster agent may advertise.
var kubernetesCapabilities = map[string]bool{CapabilityKubernetesInventory: true, CapabilityPodLogs: true, CapabilityKubernetesDeploy: true, CapabilityKubernetesRemove: true}

// CapabilitiesFit reports whether a hello's capabilities belong to the endpoint's runtime:
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -w internal/agent/protocol && go vet ./... && go test -race -count=1 ./internal/agent/protocol/`
Expected: PASS, the existing deployment, removal, inspection and inventory tests included. `go vet ./...` proves every caller still compiles: nothing outside the package uses the new fields yet.

- [ ] **Step 5: DOX and commit**

In `internal/agent/AGENTS.md`, `## Local Contracts`, in the bullet beginning `- Runtimes are`, replace `A cluster agent advertises only \`kubernetes.inventory\` and \`pod.logs\` (\`CapabilitiesFit\`)` with `A cluster agent advertises only \`kubernetes.inventory\`, \`pod.logs\`, \`kubernetes.deploy\` and \`kubernetes.remove\` (\`CapabilitiesFit\`; never \`deployment.pull\`: the kubelet pulls)`, and append this bullet after it:

```markdown
- Cluster frames: `DeploymentRequest.Kubernetes` and `RemovalRequest.Kubernetes` (`KubernetesTarget`: namespace DNS-1123 label, application and instance UUIDs, spec digest) are set exactly for a Kubernetes endpoint, and `ValidateFor(runtime, now)` holds a frame to its agent's runtime. A cluster `DeploymentService` has no container name, image ID, replaced identity or mounts; it pulls by digest with no tag, restarts `always`, `unless-stopped` or by default, publishes ports without a host address (unique within the service, since each gets its own Service), and names its secret-backed env keys in `SecretKeys` (sorted, each an `Env` key); a cluster frame carries no volumes and no registry credential. A cluster `RemovalRequest` names 1..100 distinct `Services` and no containers. `DeploymentIdentity` is a container, or with `Kind: Deployment` a namespace, name, UID, generation ≥ 1 and the pulled digest. Step codes add `forbidden`, `rollout_timeout` (detail: at most three `progressing|available|pod=<Reason>`, reasons CamelCase words) and `conflict` (detail `Kind/name`), and `name_taken` takes the same optional `Kind/name`. `UnsupportedCodes` ends with `k8s_volume`, `k8s_host_ip`, `k8s_restart`, `k8s_name`, `k8s_namespace` (`MaxUnsupported` 40). `KubernetesNames(project, services)` is the one naming rule, shared by the server and the agent: `<project>-<service>` lower-cased, `_` and `.` as `-`, prefixed `ky-` unless it starts with a letter, cut to 63 with a six-hex digest suffix when too long or shared. `ValidLabelValue` and `ValidServiceName` are the grammars both sides check. `Workload.Application`/`Instance` carry KyYard's labels, 64 bytes each.
```

```bash
git add internal/agent/protocol internal/agent/AGENTS.md && make tidy-check lint && git commit -m "feat(protocol): Kubernetes deployment frames, identities and step codes" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Store — migration 34, deploy namespaces and the Kubernetes mapping

**Files:**
- Modify: `internal/store/migrations/migrations.go` (migration 34)
- Modify: `internal/store/models.go` (`Endpoint.DeployNamespaces`)
- Modify: `internal/store/store.go` (interface, `ErrNamespaceUnknown`)
- Modify: `internal/store/endpoints.go` (`NormalizeNamespaces`, token and enrollment, `SetEndpointDeployNamespaces`)
- Modify: `internal/store/application_adoption.go` (`ApplicationInstance.Namespace`)
- Modify: `internal/store/application_mapping.go` (Kubernetes mapping)
- Test: `internal/store/kubernetes_mapping_test.go` (new); `internal/store/tenancy_test.go` (the v4 replay also forgets migration 34)
- Docs: `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.ValidDNSLabel`, `protocol.RuntimeKubernetes`, `protocol.MaxKubeObjectName` (Task 1); `withTenantTargetDetails`, `scanEndpoint`, `endpointColumns`, `adoptionPreview` (existing).
- Produces (package `store`):
  ```go
  const MaxDeployNamespaces = 32
  func NormalizeNamespaces(in []string) ([]string, error)       // sorted; ErrInvalid past 32, on a repeat, a non-label, kyyard-agent, kube-*
  var ErrNamespaceUnknown error                                  // API: 400 namespace_unknown (Task 7)
  func KubernetesProject(name string) string
  // TenancyStore:
  CreateEnrollmentToken(ctx, access TenantAccess, runtime, agentImage string, namespaces ...string) (*EnrollmentToken, error)
  SetEndpointDeployNamespaces(ctx, access TenantAccess, endpointID string, namespaces []string) (*Endpoint, error)
  // Endpoint.DeployNamespaces []string `json:"deploy_namespaces"` (never nil)
  // ApplicationInstance.Namespace string `json:"namespace,omitempty"`
  // MappingRequest.EndpointID, Namespace string `json:"endpoint_id"`, `json:"namespace"`
  // ApplicationMapping.Runtime string `json:"runtime"`; Namespace string `json:"namespace,omitempty"`; DeployNamespaces []string `json:"deploy_namespaces,omitempty"`
  // test helpers (package store): activeCluster(t, ts, a, namespaces []string, workloads []protocol.Workload) string;
  //   putClusterInventory(t, ts, endpoint string, workloads []protocol.Workload); kubernetesApp(t, st, a, spec, values) *Application
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/store/kubernetes_mapping_test.go`:

```go
package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// activeCluster enrolls, approves and reports a Kubernetes endpoint whose manifest granted
// namespaces, with the cluster deployment capabilities and workloads in its inventory.
func activeCluster(t *testing.T, ts TenancyStore, a TenantAccess, namespaces []string, workloads []protocol.Workload) string {
	t.Helper()
	ctx := context.Background()
	tok, err := ts.CreateEnrollmentToken(ctx, a, protocol.RuntimeKubernetes, "", namespaces...)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	enrolled, err := ts.Enroll(ctx, EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "cluster-" + tok.ID[:8]})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ApproveEndpoint(ctx, a, enrolled.ID, enrolled.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetEndpointCapabilities(ctx, enrolled.ID, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesRemove}); err != nil {
		t.Fatal(err)
	}
	putClusterInventory(t, ts, enrolled.ID, workloads)
	return enrolled.ID
}

var clusterGeneration atomic.Uint64

// putClusterInventory reports a fresh cluster snapshot holding workloads.
func putClusterInventory(t *testing.T, ts TenancyStore, endpoint string, workloads []protocol.Workload) {
	t.Helper()
	if workloads == nil {
		workloads = []protocol.Workload{}
	}
	snap := protocol.Snapshot{Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.31.0"},
		Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "n1", Ready: true}}, Namespaces: []string{"shop"}, Workloads: workloads, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}}}
	raw, _ := json.Marshal(snap)
	// Each report must raise the generation, and none may be ahead of the clock.
	generation := uint64(time.Now().Unix()) - 1000 + clusterGeneration.Add(1)
	if ok, err := ts.AcceptInventory(context.Background(), endpoint, generation, time.Now().UTC(), raw); err != nil || !ok {
		t.Fatalf("cluster inventory: %v %v", ok, err)
	}
}

// Namespaces are normalized once, travel from the token to the endpoint, and are replaced only
// through the audited manifest write; a Docker endpoint has none and takes none.
func TestDeployNamespacesFromTokenToEndpoint(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeCluster(t, ts, a, []string{"shop", "billing"}, nil)
	e, err := ts.ReadEndpoint(ctx, a, cluster)
	if err != nil || !slices.Equal(e.DeployNamespaces, []string{"billing", "shop"}) {
		t.Fatalf("enrolled namespaces: %v %v", e.DeployNamespaces, err)
	}
	host := activeEndpointWith(t, ts, a, nil, nil)
	if e, err := ts.ReadEndpoint(ctx, a, host); err != nil || e.DeployNamespaces == nil || len(e.DeployNamespaces) != 0 {
		t.Fatalf("docker namespaces: %v %v", e.DeployNamespaces, err)
	}
	if _, err := ts.CreateEnrollmentToken(ctx, a, protocol.RuntimeDocker, "", "shop"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("docker token with namespaces: %v", err)
	}
	for name, list := range map[string][]string{
		"repeated":         {"shop", "shop"},
		"not a label":      {"Shop"},
		"agent namespace":  {"kyyard-agent"},
		"system namespace": {"kube-system"},
		"too many":         strings.Split(strings.Repeat("n,", MaxDeployNamespaces)+"x", ","),
	} {
		if _, err := ts.CreateEnrollmentToken(ctx, a, protocol.RuntimeKubernetes, "", list...); !errors.Is(err, ErrInvalid) {
			t.Errorf("token %s: %v", name, err)
		}
		if _, err := ts.SetEndpointDeployNamespaces(ctx, a, cluster, list); !errors.Is(err, ErrInvalid) {
			t.Errorf("manifest %s: %v", name, err)
		}
	}
	updated, err := ts.SetEndpointDeployNamespaces(ctx, a, cluster, []string{"web", "shop"})
	if err != nil || !slices.Equal(updated.DeployNamespaces, []string{"shop", "web"}) {
		t.Fatalf("manifest namespaces: %+v %v", updated, err)
	}
	if e, _ := ts.ReadEndpoint(ctx, a, cluster); !slices.Equal(e.DeployNamespaces, []string{"shop", "web"}) {
		t.Fatalf("stored %v", e.DeployNamespaces)
	}
	if _, err := ts.SetEndpointDeployNamespaces(ctx, a, host, []string{"shop"}); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("docker manifest: %v", err)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(records, func(r *AuditRecord) bool {
		return r.Action == "endpoint.enroll" && r.Resource == cluster+"/manifest" && r.Details == "namespaces=shop,web" && r.Result == "success"
	}) {
		t.Fatal("the manifest write left no audit row naming its namespaces")
	}
}

// kubernetesApp imports an application named "Shop Front" with services and returns it.
func kubernetesApp(t *testing.T, st *SQLStore, a TenantAccess, spec ApplicationSpec, values map[string]string) *Application {
	t.Helper()
	app, err := st.Tenancy().ImportApplication(context.Background(), a, "Shop Front", spec, values, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// Mapping to a cluster creates the instance in a listed namespace with a project taken from
// the application's name; the namespace may move only while nothing was applied there.
func TestKubernetesMapping(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeCluster(t, ts, a, []string{"shop", "staging"}, nil)
	app := kubernetesApp(t, st, a, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1"}}}, nil)
	body := MappingRequest{EndpointID: cluster, Namespace: "prod"}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, body); !errors.Is(err, ErrNamespaceUnknown) {
		t.Fatalf("unlisted namespace: %v", err)
	}
	for name, r := range map[string]MappingRequest{
		"bindings too": {EndpointID: cluster, Namespace: "shop", Bindings: map[string]string{"web": "x"}},
		"version too":  {EndpointID: cluster, Namespace: "shop", Version: 1},
		"no endpoint":  {Namespace: "shop"},
	} {
		if err := ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	host := activeEndpointWith(t, ts, a, nil, nil)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: host, Namespace: "shop"}); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("namespace on a Docker endpoint: %v", err)
	}
	body.Namespace = "shop"
	if err := ts.SetApplicationMapping(ctx, a, app.ID, body); err != nil {
		t.Fatal(err)
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil || m.Runtime != protocol.RuntimeKubernetes || m.Namespace != "shop" || m.Version != 1 || m.MappedRevision != 1 || m.Preview.Project != "shop-front" || m.Preview.EndpointID != cluster || len(m.Preview.Containers) != 0 || len(m.Bindings) != 0 || !slices.Equal(m.Services, []string{"web"}) || !slices.Equal(m.DeployNamespaces, []string{"shop", "staging"}) {
		t.Fatalf("mapping: %+v %v", m, err)
	}
	instance, err := ts.ReadApplicationInstance(ctx, a, app.ID, m.InstanceID)
	if err != nil || instance.Namespace != "shop" || instance.PreviousRevision != 0 || instance.CurrentRevision != 0 || instance.ContainerCount != 0 {
		t.Fatalf("instance: %+v %v", instance, err)
	}
	// A Docker body cannot bind containers to a cluster instance.
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{InstanceID: m.InstanceID, Version: m.Version, Confirm: "shop-front", Bindings: map[string]string{}}); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("docker body on a cluster instance: %v", err)
	}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: cluster, Namespace: "staging"}); err != nil {
		t.Fatalf("move before any apply: %v", err)
	}
	if m, _ := ts.ReadApplicationMapping(ctx, a, app.ID); m.Namespace != "staging" || m.Version != 2 {
		t.Fatalf("moved: %+v", m)
	}
	other := activeCluster(t, ts, a, []string{"shop"}, nil)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: other, Namespace: "shop"}); !errors.Is(err, ErrApplicationAdopted) {
		t.Fatalf("second endpoint: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE application_instances SET current_revision=1 WHERE id=?`), m.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: cluster, Namespace: "shop"}); !errors.Is(err, ErrApplicationAdopted) {
		t.Fatalf("move after an apply: %v", err)
	}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: cluster, Namespace: "staging"}); err != nil {
		t.Fatalf("review in place: %v", err)
	}
}

// A Docker-adopted instance whose endpoint reads as a cluster is refused as the wrong runtime,
// not read as either shape.
func TestMappingRefusesAnInstanceOfTheOtherRuntime(t *testing.T) {
	st, a, app, endpoint, _, _ := mappingFixture(t)
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE endpoints SET runtime='kubernetes' WHERE id=?`), endpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Tenancy().ReadApplicationMapping(context.Background(), a, app.ID); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("read: %v", err)
	}
}

func TestKubernetesProject(t *testing.T) {
	for in, want := range map[string]string{
		"Shop Front":            "shop-front",
		"  web__api!!":          "web-api",
		"Ünï":                   "n",
		"***":                   "app",
		strings.Repeat("a", 70): strings.Repeat("a", 63),
	} {
		if got := KubernetesProject(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
```

In `internal/store/tenancy_test.go`:

Replace

```go
	// Reconstruct the v4 schema while preserving existing accounts, then exercise real Open.
	// Later tables reference environments, so remove them first and replay their migrations too.
	for _, q := range []string{"DROP TABLE deployment_validations", "DROP TABLE deployments", "DROP TABLE policy_runs", "DROP TABLE update_policies", "DROP TABLE image_checks", "DROP TABLE registries", "DROP TABLE application_resources", "DROP TABLE application_instances", "DROP TABLE application_revisions", "DROP TABLE applications", "DROP TABLE endpoint_commands", "DROP TABLE container_rollups", "DROP TABLE container_samples", "DROP TABLE endpoint_inventory", "DROP TABLE endpoint_events", "DROP TABLE endpoint_capabilities", "DROP TABLE agent_enrollment_tokens", "DROP TABLE endpoint_keys", "DROP TABLE endpoints", "DROP TABLE organization_group_members", "DROP TABLE organization_groups", "DROP TABLE environments", "DROP TABLE organization_memberships", "DROP TABLE tenancy_bootstrap", "DROP TABLE organizations", "DELETE FROM schema_migrations WHERE version IN (5,7,8,9,10,11,12,13,14,15,16,17,18,20,21,22,23,24,25,26,27,28,29,31,32,33)"} {
		_, err := db.ExecContext(ctx, q)
		mustTenant(t, err)
```

with

```go
	// Reconstruct the v4 schema while preserving existing accounts, then exercise real Open.
	// Later tables reference environments, so remove them first and replay their migrations too.
	for _, q := range []string{"DROP TABLE deployment_validations", "DROP TABLE deployments", "DROP TABLE policy_runs", "DROP TABLE update_policies", "DROP TABLE image_checks", "DROP TABLE registries", "DROP TABLE application_resources", "DROP TABLE application_instances", "DROP TABLE application_revisions", "DROP TABLE applications", "DROP TABLE endpoint_commands", "DROP TABLE container_rollups", "DROP TABLE container_samples", "DROP TABLE endpoint_inventory", "DROP TABLE endpoint_events", "DROP TABLE endpoint_capabilities", "DROP TABLE agent_enrollment_tokens", "DROP TABLE endpoint_keys", "DROP TABLE endpoints", "DROP TABLE organization_group_members", "DROP TABLE organization_groups", "DROP TABLE environments", "DROP TABLE organization_memberships", "DROP TABLE tenancy_bootstrap", "DROP TABLE organizations", "DELETE FROM schema_migrations WHERE version IN (5,7,8,9,10,11,12,13,14,15,16,17,18,20,21,22,23,24,25,26,27,28,29,31,32,33,34)"} {
		_, err := db.ExecContext(ctx, q)
		mustTenant(t, err)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestDeployNamespaces|TestKubernetesMapping|TestMappingRefuses|TestKubernetesProject' ./internal/store/`
Expected: FAIL to compile: `undefined: MaxDeployNamespaces`, `ts.SetEndpointDeployNamespaces undefined`, `unknown field EndpointID in struct literal of type MappingRequest`.

- [ ] **Step 3: Implement**

In `internal/store/migrations/migrations.go`:

Replace

```go
	// is a fact of the apply, so a run's plan applied by hand stays manual.
	{Version: 33, Name: "deployment_policy_run", SQLite: `ALTER TABLE deployments ADD COLUMN policy_run_id TEXT REFERENCES policy_runs(id) ON DELETE SET NULL;`, Postgres: `ALTER TABLE deployments ADD COLUMN policy_run_id TEXT REFERENCES policy_runs(id) ON DELETE SET NULL;`},
}

// Latest returns the highest registered migration version: the schema this binary runs.
func Latest() int {
```

with

```go
	// is a fact of the apply, so a run's plan applied by hand stays manual.
	{Version: 33, Name: "deployment_policy_run", SQLite: `ALTER TABLE deployments ADD COLUMN policy_run_id TEXT REFERENCES policy_runs(id) ON DELETE SET NULL;`, Postgres: `ALTER TABLE deployments ADD COLUMN policy_run_id TEXT REFERENCES policy_runs(id) ON DELETE SET NULL;`},
	{Version: 34, Name: "kubernetes_namespaces", SQLite: kubernetesNamespaces, Postgres: kubernetesNamespaces},
}

// kubernetesNamespaces stores the namespaces a cluster's manifest grants writes in (a JSON list,
// on the token until enrollment copies it to the endpoint) and the namespace an instance maps to,
// empty for a Docker instance.
const kubernetesNamespaces = `ALTER TABLE agent_enrollment_tokens ADD COLUMN deploy_namespaces TEXT NOT NULL DEFAULT '[]';
ALTER TABLE endpoints ADD COLUMN deploy_namespaces TEXT NOT NULL DEFAULT '[]';
ALTER TABLE application_instances ADD COLUMN namespace TEXT NOT NULL DEFAULT '';
`

// Latest returns the highest registered migration version: the schema this binary runs.
func Latest() int {
```

In `internal/store/models.go`:

Replace

```go
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
}
```

with

```go
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	// DeployNamespaces are the namespaces the cluster's last manifest grants writes in, sorted;
	// empty for a Docker endpoint.
	DeployNamespaces []string `json:"deploy_namespaces"`
}
```

In `internal/store/store.go`:

Replace

```go
	// agent's: nothing is stored, and the row waits for another answer or the sweep.
	ErrUnreadableResult = errors.New("unreadable deployment result")
)
```

with

```go
	// agent's: nothing is stored, and the row waits for another answer or the sweep.
	ErrUnreadableResult = errors.New("unreadable deployment result")
	// ErrNamespaceUnknown is a mapping to a namespace the cluster's manifest does not list.
	ErrNamespaceUnknown = errors.New("the namespace is not one the cluster's manifest grants")
)
```

Replace

```go
	CheckEnrollmentAccess(ctx context.Context, access TenantAccess) error
	CreateEnrollmentToken(ctx context.Context, access TenantAccess, runtime, agentImage string) (*EnrollmentToken, error)
	// Enroll is agent-facing: the token, not a session, selects the tenant.
	Enroll(ctx context.Context, request EnrollmentRequest) (*Endpoint, error)
```

with

```go
	CheckEnrollmentAccess(ctx context.Context, access TenantAccess) error
	// CreateEnrollmentToken records namespaces (Kubernetes only) for the endpoint it enrolls.
	CreateEnrollmentToken(ctx context.Context, access TenantAccess, runtime, agentImage string, namespaces ...string) (*EnrollmentToken, error)
	// SetEndpointDeployNamespaces replaces a Kubernetes endpoint's namespace list, audited as
	// <endpoint>/manifest under endpoint.enroll.
	SetEndpointDeployNamespaces(ctx context.Context, access TenantAccess, endpointID string, namespaces []string) (*Endpoint, error)
	// Enroll is agent-facing: the token, not a session, selects the tenant.
	Enroll(ctx context.Context, request EnrollmentRequest) (*Endpoint, error)
```

In `internal/store/endpoints.go`:

Replace

```go
	"encoding/json"
	"errors"
	"strings"
	"time"
```

with

```go
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
```

Replace

```go
func validRuntime(r string) bool { return r == "docker" || r == "kubernetes" }

// ValidEndpointName is the rule Enroll and RenameEndpoint apply to an endpoint's name.
func ValidEndpointName(name string) bool { return validTenantName(name) }
```

with

```go
func validRuntime(r string) bool { return r == "docker" || r == "kubernetes" }

// MaxDeployNamespaces bounds the namespaces one cluster manifest grants writes in.
const MaxDeployNamespaces = 32

// NormalizeNamespaces sorts a namespace list and refuses one with more than MaxDeployNamespaces
// entries, a repeat, a name that is not a DNS-1123 label, the agent's own namespace (its Secrets
// are its identity) or a kube- system namespace.
func NormalizeNamespaces(in []string) ([]string, error) {
	out := slices.Sorted(slices.Values(in))
	if len(out) > MaxDeployNamespaces {
		return nil, ErrInvalid
	}
	for i, ns := range out {
		if !protocol.ValidDNSLabel(ns) || ns == "kyyard-agent" || strings.HasPrefix(ns, "kube-") || (i > 0 && out[i-1] == ns) {
			return nil, ErrInvalid
		}
	}
	return out, nil
}

// ValidEndpointName is the rule Enroll and RenameEndpoint apply to an endpoint's name.
func ValidEndpointName(name string) bool { return validTenantName(name) }
```

Replace

```go
// CreateEnrollmentToken records the image reference the operator is handed with the token, so
// the audit trail names the bytes that were authorized to run as root on the host.
func (t *tenancyStore) CreateEnrollmentToken(ctx context.Context, a TenantAccess, runtime, agentImage string) (*EnrollmentToken, error) {
	if a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	secret := make([]byte, protocol.TokenSize)
	if _, err := rand.Read(secret); err != nil {
```

with

```go
// CreateEnrollmentToken records the image reference the operator is handed with the token, so
// the audit trail names the bytes that were authorized to run as root on the host.
func (t *tenancyStore) CreateEnrollmentToken(ctx context.Context, a TenantAccess, runtime, agentImage string, namespaces ...string) (*EnrollmentToken, error) {
	if a.EnvironmentID == "" || (runtime != protocol.RuntimeKubernetes && len(namespaces) > 0) {
		return nil, ErrInvalid
	}
	namespaces, err := NormalizeNamespaces(namespaces)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(namespaces)
	secret := make([]byte, protocol.TokenSize)
	if _, err := rand.Read(secret); err != nil {
```

Replace

```go
	now := time.Now().UTC()
	tok := &EnrollmentToken{ID: uuid.NewString(), EnvironmentID: a.EnvironmentID, Runtime: runtime, ExpiresAt: now.Add(enrollmentTokenLife), Secret: secret, AgentImage: agentImage}
	err := t.withTenantTarget(ctx, a, permissions.EndpointEnroll, tok.ID, func(tx *sql.Tx) error {
		if !validRuntime(runtime) {
			return ErrInvalid
		}
		_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO agent_enrollment_tokens (id,organization_id,environment_id,runtime,token_hash,created_by,created_at,expires_at,agent_image) VALUES (?,?,?,?,?,?,?,?,?)`), tok.ID, a.OrganizationID, a.EnvironmentID, runtime, crypto.SHA256Hex(secret), a.ActorID, now, tok.ExpiresAt, agentImage)
		return err
	})
```

with

```go
	now := time.Now().UTC()
	tok := &EnrollmentToken{ID: uuid.NewString(), EnvironmentID: a.EnvironmentID, Runtime: runtime, ExpiresAt: now.Add(enrollmentTokenLife), Secret: secret, AgentImage: agentImage}
	err = t.withTenantTarget(ctx, a, permissions.EndpointEnroll, tok.ID, func(tx *sql.Tx) error {
		if !validRuntime(runtime) {
			return ErrInvalid
		}
		_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO agent_enrollment_tokens (id,organization_id,environment_id,runtime,token_hash,created_by,created_at,expires_at,agent_image,deploy_namespaces) VALUES (?,?,?,?,?,?,?,?,?,?)`), tok.ID, a.OrganizationID, a.EnvironmentID, runtime, crypto.SHA256Hex(secret), a.ActorID, now, tok.ExpiresAt, agentImage, string(encoded))
		return err
	})
```

Replace

```go
	}
	e := &Endpoint{ID: "ep_" + crypto.RandomHex(12), Name: strings.TrimSpace(req.Name), State: "pending", Facts: facts, Fingerprint: protocol.Fingerprint(req.PublicKey), CreatedAt: now}
	var tokenID string
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,organization_id,environment_id,runtime FROM agent_enrollment_tokens WHERE token_hash=?`), crypto.SHA256Hex(req.Token)).Scan(&tokenID, &e.OrganizationID, &e.EnvironmentID, &e.Runtime); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoints (id,organization_id,environment_id,name,runtime,state,facts,created_at) VALUES (?,?,?,?,?,?,?,?)`), e.ID, e.OrganizationID, e.EnvironmentID, e.Name, e.Runtime, e.State, string(factsJSON), now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return nil, ErrAlreadyExists
```

with

```go
	}
	e := &Endpoint{ID: "ep_" + crypto.RandomHex(12), Name: strings.TrimSpace(req.Name), State: "pending", Facts: facts, Fingerprint: protocol.Fingerprint(req.PublicKey), CreatedAt: now}
	var tokenID, namespaces string
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,organization_id,environment_id,runtime,deploy_namespaces FROM agent_enrollment_tokens WHERE token_hash=?`), crypto.SHA256Hex(req.Token)).Scan(&tokenID, &e.OrganizationID, &e.EnvironmentID, &e.Runtime, &namespaces); err != nil {
		return nil, err
	}
	e.DeployNamespaces = decodeNamespaces(namespaces)
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoints (id,organization_id,environment_id,name,runtime,state,facts,created_at,deploy_namespaces) VALUES (?,?,?,?,?,?,?,?,?)`), e.ID, e.OrganizationID, e.EnvironmentID, e.Name, e.Runtime, e.State, string(factsJSON), now, namespaces); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return nil, ErrAlreadyExists
```

Replace

```go
}

const endpointColumns = `e.id,e.organization_id,e.environment_id,e.name,e.runtime,e.state,e.facts,e.created_at,e.approved_at,e.approved_by,e.revoked_at,e.last_seen_at,COALESCE((SELECT k.fingerprint FROM endpoint_keys k WHERE k.endpoint_id=e.id AND k.state IN ('approved','pending_review') ORDER BY k.state LIMIT 1),''),COALESCE((SELECT k.fingerprint FROM endpoint_keys k WHERE k.endpoint_id=e.id AND k.state='pending_review' AND e.state<>'pending' AND k.created_at>? ORDER BY k.created_at DESC LIMIT 1),'')`

// pendingCutoff is the oldest creation time a key may have and still count as pending.
```

with

```go
}

const endpointColumns = `e.id,e.organization_id,e.environment_id,e.name,e.runtime,e.state,e.facts,e.created_at,e.approved_at,e.approved_by,e.revoked_at,e.last_seen_at,e.deploy_namespaces,COALESCE((SELECT k.fingerprint FROM endpoint_keys k WHERE k.endpoint_id=e.id AND k.state IN ('approved','pending_review') ORDER BY k.state LIMIT 1),''),COALESCE((SELECT k.fingerprint FROM endpoint_keys k WHERE k.endpoint_id=e.id AND k.state='pending_review' AND e.state<>'pending' AND k.created_at>? ORDER BY k.created_at DESC LIMIT 1),'')`

// pendingCutoff is the oldest creation time a key may have and still count as pending.
```

Replace

```go
func scanEndpoint(row interface{ Scan(...any) error }) (*Endpoint, error) {
	var e Endpoint
	var facts string
	if err := row.Scan(&e.ID, &e.OrganizationID, &e.EnvironmentID, &e.Name, &e.Runtime, &e.State, &facts, &e.CreatedAt, &e.ApprovedAt, &e.ApprovedBy, &e.RevokedAt, &e.LastSeenAt, &e.Fingerprint, &e.PendingFingerprint); err != nil {
		return nil, err
	}
	e.Capabilities = []string{}
	e.Alerts = []EndpointEvent{}
```

with

```go
func scanEndpoint(row interface{ Scan(...any) error }) (*Endpoint, error) {
	var e Endpoint
	var facts, namespaces string
	if err := row.Scan(&e.ID, &e.OrganizationID, &e.EnvironmentID, &e.Name, &e.Runtime, &e.State, &facts, &e.CreatedAt, &e.ApprovedAt, &e.ApprovedBy, &e.RevokedAt, &e.LastSeenAt, &namespaces, &e.Fingerprint, &e.PendingFingerprint); err != nil {
		return nil, err
	}
	e.DeployNamespaces = decodeNamespaces(namespaces)
	e.Capabilities = []string{}
	e.Alerts = []EndpointEvent{}
```

Replace

```go
}

// ApproveEndpoint binds the fingerprint the administrator reviewed: a different one, or a
// pending enrollment older than its life, is refused rather than silently approved.
```

with

```go
}

// decodeNamespaces reads a stored namespace list; an unreadable one is empty, which grants nothing.
func decodeNamespaces(raw string) []string {
	out := []string{}
	if json.Unmarshal([]byte(raw), &out) != nil || out == nil {
		return []string{}
	}
	return out
}

// SetEndpointDeployNamespaces replaces a Kubernetes endpoint's namespace list with the one the
// administrator is about to apply a regenerated manifest for. It is the list plans and mappings
// check; the agent checks its real grant itself before every apply.
func (t *tenancyStore) SetEndpointDeployNamespaces(ctx context.Context, a TenantAccess, id string, namespaces []string) (*Endpoint, error) {
	namespaces, err := NormalizeNamespaces(namespaces)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(namespaces)
	details := "namespaces=" + strings.Join(namespaces, ",")
	var e *Endpoint
	err = t.withTenantTargetDetails(ctx, a, permissions.EndpointEnroll, id+"/manifest", &details, func(tx *sql.Tx) error {
		var err error
		e, err = scanEndpoint(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+endpointColumns+` FROM endpoints e WHERE e.organization_id=? AND e.id=?`), pendingCutoff(), a.OrganizationID, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if a.EnvironmentID != "" && e.EnvironmentID != a.EnvironmentID {
			return ErrNotFound
		}
		if e.Runtime != protocol.RuntimeKubernetes {
			return ErrRuntimeUnsupported
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET deploy_namespaces=? WHERE id=?`), string(encoded), id); err != nil {
			return err
		}
		e.DeployNamespaces = namespaces
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// ApproveEndpoint binds the fingerprint the administrator reviewed: a different one, or a
// pending enrollment older than its life, is refused rather than silently approved.
```

In `internal/store/application_adoption.go`:

Replace

```go
	CreatedAt        time.Time          `json:"created_at"`
	Containers       []AdoptedContainer `json:"containers"`
}
```

with

```go
	CreatedAt        time.Time          `json:"created_at"`
	Containers       []AdoptedContainer `json:"containers"`
	// Namespace is set exactly for an instance mapped to a Kubernetes endpoint.
	Namespace string `json:"namespace,omitempty"`
}
```

Replace

```go
	i := ApplicationInstance{Containers: []AdoptedContainer{}}
	err := t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.id,i.application_id,i.endpoint_id,i.project,i.revision,i.mapping_version,i.current_revision,i.previous_revision,i.created_by,i.created_at,e.name,(SELECT COUNT(*) FROM application_resources r WHERE r.instance_id=i.id) FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND i.environment_id=? AND i.application_id=? AND i.id=?`), a.OrganizationID, a.EnvironmentID, app, id).Scan(&i.ID, &i.ApplicationID, &i.EndpointID, &i.Project, &i.Revision, &i.MappingVersion, &i.CurrentRevision, &i.PreviousRevision, &i.CreatedBy, &i.CreatedAt, &i.EndpointName, &i.ContainerCount)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
```

with

```go
	i := ApplicationInstance{Containers: []AdoptedContainer{}}
	err := t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.id,i.application_id,i.endpoint_id,i.project,i.revision,i.mapping_version,i.current_revision,i.previous_revision,i.created_by,i.created_at,e.name,(SELECT COUNT(*) FROM application_resources r WHERE r.instance_id=i.id),i.namespace FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND i.environment_id=? AND i.application_id=? AND i.id=?`), a.OrganizationID, a.EnvironmentID, app, id).Scan(&i.ID, &i.ApplicationID, &i.EndpointID, &i.Project, &i.Revision, &i.MappingVersion, &i.CurrentRevision, &i.PreviousRevision, &i.CreatedBy, &i.CreatedAt, &i.EndpointName, &i.ContainerCount, &i.Namespace)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
```

Replace

```go
	out := []ApplicationInstance{}
	err := t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT i.id,i.application_id,i.endpoint_id,i.project,i.revision,i.mapping_version,i.current_revision,i.previous_revision,i.created_by,i.created_at,e.name,(SELECT COUNT(*) FROM application_resources r WHERE r.instance_id=i.id) FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND (?='' OR i.environment_id=?) AND (?='' OR i.endpoint_id=?) ORDER BY i.id LIMIT 100`), a.OrganizationID, a.EnvironmentID, a.EnvironmentID, endpoint, endpoint)
		if err != nil {
			return err
```

with

```go
	out := []ApplicationInstance{}
	err := t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT i.id,i.application_id,i.endpoint_id,i.project,i.revision,i.mapping_version,i.current_revision,i.previous_revision,i.created_by,i.created_at,e.name,(SELECT COUNT(*) FROM application_resources r WHERE r.instance_id=i.id),i.namespace FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND (?='' OR i.environment_id=?) AND (?='' OR i.endpoint_id=?) ORDER BY i.id LIMIT 100`), a.OrganizationID, a.EnvironmentID, a.EnvironmentID, endpoint, endpoint)
		if err != nil {
			return err
```

Replace

```go
			var i ApplicationInstance
			i.Containers = []AdoptedContainer{}
			if err = rows.Scan(&i.ID, &i.ApplicationID, &i.EndpointID, &i.Project, &i.Revision, &i.MappingVersion, &i.CurrentRevision, &i.PreviousRevision, &i.CreatedBy, &i.CreatedAt, &i.EndpointName, &i.ContainerCount); err != nil {
				rows.Close()
				return err
```

with

```go
			var i ApplicationInstance
			i.Containers = []AdoptedContainer{}
			if err = rows.Scan(&i.ID, &i.ApplicationID, &i.EndpointID, &i.Project, &i.Revision, &i.MappingVersion, &i.CurrentRevision, &i.PreviousRevision, &i.CreatedBy, &i.CreatedAt, &i.EndpointName, &i.ContainerCount, &i.Namespace); err != nil {
				rows.Close()
				return err
```

In `internal/store/application_mapping.go`:

Replace

```go
	"encoding/json"
	"errors"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
```

with

```go
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
```

Replace

```go
	Services       []string          `json:"services"`
	Bindings       map[string]string `json:"bindings"`
}
type MappingRequest struct {
	InstanceID string            `json:"instance_id"`
```

with

```go
	Services       []string          `json:"services"`
	Bindings       map[string]string `json:"bindings"`
	// Runtime is the endpoint's. A Kubernetes mapping names its Namespace and the namespaces
	// the cluster's manifest grants; it has no containers and no bindings.
	Runtime          string   `json:"runtime"`
	Namespace        string   `json:"namespace,omitempty"`
	DeployNamespaces []string `json:"deploy_namespaces,omitempty"`
}

// MappingRequest binds services to adopted containers (Docker), or, with EndpointID and
// Namespace and nothing else, maps the application to a namespace of a Kubernetes endpoint.
type MappingRequest struct {
	InstanceID string            `json:"instance_id"`
```

Replace

```go
	Confirm    string            `json:"confirm"`
	Bindings   map[string]string `json:"bindings"`
}
```

with

```go
	Confirm    string            `json:"confirm"`
	Bindings   map[string]string `json:"bindings"`
	EndpointID string            `json:"endpoint_id"`
	Namespace  string            `json:"namespace"`
}
```

Replace

```go
func (t *tenancyStore) applicationMapping(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, lock bool) (*ApplicationMapping, error) {
	out := &ApplicationMapping{Services: []string{}, Bindings: map[string]string{}}
	var endpoint, project string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,endpoint_id,project,mapping_version,mapped_revision FROM application_instances WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, app).Scan(&out.InstanceID, &endpoint, &project, &out.Version, &out.MappedRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
```

with

```go
func (t *tenancyStore) applicationMapping(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, lock bool) (*ApplicationMapping, error) {
	out := &ApplicationMapping{Services: []string{}, Bindings: map[string]string{}}
	var endpoint, project, namespaces string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.id,i.endpoint_id,i.project,i.mapping_version,i.mapped_revision,i.namespace,e.runtime,e.deploy_namespaces FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND i.environment_id=? AND i.application_id=?`), a.OrganizationID, a.EnvironmentID, app).Scan(&out.InstanceID, &endpoint, &project, &out.Version, &out.MappedRevision, &out.Namespace, &out.Runtime, &namespaces)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
```

Replace

```go
		return nil, err
	}
	out.Preview, err = t.adoptionPreview(ctx, tx, a, app, endpoint, project, lock)
	if err != nil {
```

with

```go
		return nil, err
	}
	// An instance's shape must be its endpoint's runtime's: containers on Docker, a namespace
	// on Kubernetes.
	if (out.Runtime == protocol.RuntimeKubernetes) != (out.Namespace != "") {
		return nil, ErrRuntimeUnsupported
	}
	if out.Runtime == protocol.RuntimeKubernetes {
		out.DeployNamespaces = decodeNamespaces(namespaces)
		if out.Preview, err = t.kubernetesPreview(ctx, tx, a, app, endpoint, project, lock); err != nil {
			return nil, err
		}
		return out, t.mappedServices(ctx, tx, a, app, out)
	}
	out.Preview, err = t.adoptionPreview(ctx, tx, a, app, endpoint, project, lock)
	if err != nil {
```

Replace

```go
	// Keep the digest of the complete observed project, but expose only owned choices.
	out.Preview.Containers = owned
	var raw, digest string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest FROM application_revisions WHERE application_id=? AND number=? AND organization_id=? AND environment_id=?`), app, out.Preview.Revision, a.OrganizationID, a.EnvironmentID).Scan(&raw, &digest)
	if err != nil {
		return nil, err
	}
	var spec ApplicationSpec
	if applicationSpecDigest([]byte(raw)) != digest || json.Unmarshal([]byte(raw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
		return nil, ErrRevisionCorrupt
	}
	for _, s := range spec.Services {
		out.Services = append(out.Services, s.Name)
	}
	return out, nil
}
func (t *tenancyStore) ReadApplicationMapping(ctx context.Context, a TenantAccess, app string) (*ApplicationMapping, error) {
```

with

```go
	// Keep the digest of the complete observed project, but expose only owned choices.
	out.Preview.Containers = owned
	return out, t.mappedServices(ctx, tx, a, app, out)
}

// mappedServices lists the services of the revision out's preview names.
func (t *tenancyStore) mappedServices(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, out *ApplicationMapping) error {
	var raw, digest string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest FROM application_revisions WHERE application_id=? AND number=? AND organization_id=? AND environment_id=?`), app, out.Preview.Revision, a.OrganizationID, a.EnvironmentID).Scan(&raw, &digest)
	if err != nil {
		return err
	}
	var spec ApplicationSpec
	if applicationSpecDigest([]byte(raw)) != digest || json.Unmarshal([]byte(raw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
		return ErrRevisionCorrupt
	}
	for _, s := range spec.Services {
		out.Services = append(out.Services, s.Name)
	}
	return nil
}

// kubernetesPreview is adoptionPreview for a Kubernetes instance: the application and endpoint
// it maps, taking the same locks in the same order, with no containers and no digest.
func (t *tenancyStore) kubernetesPreview(ctx context.Context, tx *sql.Tx, a TenantAccess, app, endpoint, project string, lock bool) (*AdoptionPreview, error) {
	p := &AdoptionPreview{ApplicationID: app, EndpointID: endpoint, Project: project, Containers: []AdoptedContainer{}}
	q := `SELECT name,latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`
	if lock && t.store.driver == "postgres" {
		q += " FOR UPDATE"
	}
	if err := tx.QueryRowContext(ctx, t.store.rebind(q), a.OrganizationID, a.EnvironmentID, app).Scan(&p.ApplicationName, &p.Revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if lock {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET name=name WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, endpoint); err != nil {
			return nil, err
		}
	}
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT name FROM endpoints WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, endpoint).Scan(&p.EndpointName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// KubernetesProject is the project a Kubernetes instance takes from its application's name:
// lower-case letters and digits, other runs as one '-', at most 63 characters, "app" when
// nothing is left. It names the instance's objects and labels.
func KubernetesProject(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > protocol.MaxKubeObjectName {
		out = strings.TrimRight(out[:protocol.MaxKubeObjectName], "-")
	}
	if out == "" {
		return "app"
	}
	return out
}

// mapKubernetes creates the application's instance on a Kubernetes endpoint, or moves it to
// another listed namespace while nothing has been applied there. Every call is a review: the
// mapping version rises and the mapped revision becomes the latest.
func (t *tenancyStore) mapKubernetes(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, r MappingRequest) error {
	if r.InstanceID != "" || r.Version != 0 || r.Digest != "" || r.Confirm != "" || len(r.Bindings) > 0 || r.EndpointID == "" {
		return ErrInvalid
	}
	lock := ""
	if t.store.driver == "postgres" {
		lock = " FOR UPDATE"
	}
	var name string
	var head int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT name,latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`+lock), a.OrganizationID, a.EnvironmentID, app).Scan(&name, &head); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var runtime, raw string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT runtime,deploy_namespaces FROM endpoints WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, r.EndpointID).Scan(&runtime, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if runtime != protocol.RuntimeKubernetes {
		return ErrRuntimeUnsupported
	}
	if !slices.Contains(decodeNamespaces(raw), r.Namespace) {
		return ErrNamespaceUnknown
	}
	var instance, endpoint, namespace string
	var current int
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,endpoint_id,namespace,current_revision FROM application_instances WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, app).Scan(&instance, &endpoint, &namespace, &current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_instances(id,organization_id,environment_id,application_id,endpoint_id,project,revision,created_by,created_at,mapping_version,mapped_revision,namespace) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), uuid.NewString(), a.OrganizationID, a.EnvironmentID, app, r.EndpointID, KubernetesProject(name), head, a.ActorID, time.Now().UTC(), 1, head, r.Namespace)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case endpoint != r.EndpointID || namespace == "" || (namespace != r.Namespace && current > 0):
		// Mapped elsewhere, adopted on Docker, or applied in the namespace it would leave.
		return ErrApplicationAdopted
	default:
		var applying int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM deployments WHERE instance_id=? AND state='applying'`), instance).Scan(&applying); err != nil {
			return err
		}
		if applying > 0 {
			return ErrDeploymentInProgress
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_instances SET namespace=?,mapping_version=mapping_version+1,mapped_revision=? WHERE id=?`), r.Namespace, head, instance); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE applications SET removed_at=NULL WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, app)
	return err
}
func (t *tenancyStore) ReadApplicationMapping(ctx context.Context, a TenantAccess, app string) (*ApplicationMapping, error) {
```

Replace

```go
			return ErrInvalid
		}
		p, err := t.applicationMapping(ctx, tx, a, id.String(), true)
		if err != nil {
			return err
		}
		if r.InstanceID != p.InstanceID || r.Version != p.Version || r.Digest != p.Preview.Digest || r.Confirm != p.Preview.Project {
			return ErrAdoptionChanged
```

with

```go
			return ErrInvalid
		}
		if r.EndpointID != "" || r.Namespace != "" {
			return t.mapKubernetes(ctx, tx, a, id.String(), r)
		}
		p, err := t.applicationMapping(ctx, tx, a, id.String(), true)
		if err != nil {
			return err
		}
		if p.Runtime != protocol.RuntimeDocker {
			return ErrRuntimeUnsupported
		}
		if r.InstanceID != p.InstanceID || r.Version != p.Version || r.Digest != p.Preview.Digest || r.Confirm != p.Preview.Project {
			return ErrAdoptionChanged
```

- [ ] **Step 4: Run the tests to verify they pass, on both databases**

Run: `gofmt -w internal/store && go vet ./... && go test -race -count=1 ./internal/store/... && PG=… go test -count=1 ./internal/store/... && go test -count=1 ./internal/api/`
Expected: every package `ok`. The API suite proves the endpoint JSON (`deploy_namespaces`) and the variadic token signature break no caller.

- [ ] **Step 5: DOX and commit**

In `internal/store/AGENTS.md`, `## Local Contracts`, append:

```markdown
- Migration 34 (`kubernetes_namespaces`) adds `deploy_namespaces` (a JSON list, default `'[]'`) to `agent_enrollment_tokens` and `endpoints`, and `namespace` (empty for Docker) to `application_instances`. `NormalizeNamespaces` sorts a list and refuses more than 32, a repeat, a name that is not a DNS-1123 label, `kyyard-agent` and `kube-*`. `CreateEnrollmentToken(…, namespaces...)` stores them on a Kubernetes token (a Docker token takes none) and `Enroll` copies them to the endpoint; `Endpoint.DeployNamespaces` is always a list. `SetEndpointDeployNamespaces` replaces a Kubernetes endpoint's list under `endpoint.enroll` (a Docker endpoint is `ErrRuntimeUnsupported`), audited on `<endpoint>/manifest` with details `namespaces=<list>`.
- A Kubernetes mapping (`MappingRequest{EndpointID, Namespace}` and no other field; `mapKubernetes`, under `application.adopt` like the Docker mapping) creates the application's instance on a Kubernetes endpoint with project `KubernetesProject(<application name>)`, mapping version 1 and mapped revision the latest. A namespace outside the endpoint's list is `ErrNamespaceUnknown`; an instance on another endpoint, adopted on Docker, or applied (`current_revision` > 0) in the namespace it would leave is `ErrApplicationAdopted`; each accepted call raises the mapping version and sets the mapped revision to the latest. `applicationMapping` reports `Runtime`, and for a Kubernetes instance its `Namespace`, the endpoint's `DeployNamespaces` and a preview without containers or digest. An instance whose shape does not fit its endpoint's runtime (adopted containers on a cluster) is `ErrRuntimeUnsupported` on every read, and a Docker mapping body on a Kubernetes instance is refused the same. Every instance read carries `ApplicationInstance.Namespace`.
```

```bash
git add internal/store && make tidy-check lint && git commit -m "feat(store): deploy namespaces and the Kubernetes mapping (migration 34)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Store — plan, frame, settle and remove on a Kubernetes namespace

**Files:**
- Modify: `internal/store/application_spec.go` (`serviceNames`)
- Modify: `internal/store/application_deployment.go` (`KubernetesObject`, plan fields, the Kubernetes path of `PlanDeployment`, `capabilityBlockers`, `draftPlan`)
- Modify: `internal/store/application_preflight.go` (`buildKubernetesPreflight`)
- Modify: `internal/store/application_apply.go` (`kubernetesFrame`, `settleKubernetesApply`, removal)
- Modify: `internal/store/rollback.go`, `internal/store/image_checks.go`
- Test: `internal/store/kubernetes_deployment_test.go` (new)
- Docs: `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: Task 1 (`KubernetesTarget`, `KubernetesNames`, `ValidLabelValue`, `KindDeployment`, identity fields, `RemovalRequest.Services`), Task 2 (`ApplicationMapping.Runtime/Namespace/DeployNamespaces`, the test helpers `activeCluster`, `putClusterInventory`, `kubernetesApp`), existing `fakeResolver`, `digestOf`, `imageCheckKey`.
- Produces (package `store`):
  ```go
  type KubernetesObject struct{ Namespace, Name string }          // json namespace, name
  // PlannedService.Object *KubernetesObject `json:"object,omitempty"`
  // DeploymentPlan.Namespace string `json:"namespace,omitempty"`  (set exactly for a Kubernetes plan or removal)
  func (spec ApplicationSpec) serviceNames() []string              // unexported
  ```
  Behavior later tasks rely on: a Kubernetes `PlanDeployment` needs a resolver and resolves every service; `ApplyDeployment` returns a frame with `Kubernetes` set; `RemoveApplication` returns a `RemovalRequest` with `Kubernetes` and `Services`; `SettleDeployment` takes Deployment identities; every refusal keeps the existing error values.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/kubernetes_deployment_test.go`:

```go
package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
)

const kubeUID = "0f1e2d3c-4b5a-4968-8776-655443322110"

// kubernetesPlanFixture maps a two-service application (web with a secret, api pinned by digest)
// to namespace shop of an active cluster, with anonymous pulls on.
func kubernetesPlanFixture(t *testing.T, spec ApplicationSpec, values map[string]string) (*SQLStore, TenantAccess, *Application, string, *ApplicationMapping) {
	t.Helper()
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeCluster(t, ts, a, []string{"shop"}, nil)
	if err := ts.SetAnonymousPull(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	app := kubernetesApp(t, st, a, spec, values)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: cluster, Namespace: "shop"}); err != nil {
		t.Fatal(err)
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, a, app, cluster, m
}

func twoServiceSpec() ApplicationSpec {
	return ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{
		{Name: "web", Image: "ghcr.io/org/web:1", Restart: "always", Ports: []ApplicationPort{{Target: 80, Published: 8080, Protocol: "tcp"}}, Environment: map[string]ApplicationSecretRef{"TOKEN": {SecretRef: "web.TOKEN"}}},
		{Name: "api", Image: "ghcr.io/org/api@" + digestOf("a")},
	}}
}

func kubePlanRequest(m *ApplicationMapping) PlanRequest {
	return PlanRequest{InstanceID: m.InstanceID, MappingVersion: m.Version, Revision: m.Preview.Revision, Confirm: m.Preview.Project, MaxFrameBytes: protocol.MaxDeploymentRequestBytes}
}

// A Kubernetes plan resolves every service's image at the registry, names each service's
// objects in the instance's namespace, and builds a frame with the target, the pinned pulls and
// the secret keys, and no Docker field.
func TestKubernetesPlanAndFrame(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "secret-canary"})
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), nil, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a cluster plan without a resolver: %v", err)
	}
	pinned := kubePlanRequest(m)
	pinned.PinImages = map[string]string{"web": digestOf("c")}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pinned, resolver, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a cluster plan with pinned image IDs: %v", err)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolver.called()) != 1 {
		t.Fatalf("registry calls %v: the pinned api needs none", resolver.called())
	}
	web, api := d.Plan.Services[0], d.Plan.Services[1]
	if d.Plan.Namespace != "shop" || d.Plan.Project != "shop-front" || web.Object == nil || *web.Object != (KubernetesObject{Namespace: "shop", Name: "shop-front-web"}) || api.Object.Name != "shop-front-api" {
		t.Fatalf("plan %+v", d.Plan)
	}
	if web.PullReference != "ghcr.io/org/web@"+digestOf("b") || web.PullDigest != digestOf("b") || api.PullDigest != digestOf("a") || web.ImageID != "" || web.ContainerID != "" || web.Replaces != (protocol.InspectionTarget{}) || len(web.Mounts) != 0 {
		t.Fatalf("services %+v %+v", web, api)
	}
	applied, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != "applying" || req.Kubernetes == nil || *req.Kubernetes != (protocol.KubernetesTarget{Namespace: "shop", ApplicationID: app.ID, InstanceID: m.InstanceID, SpecDigest: d.SpecDigest}) || len(req.Registries) != 0 || len(req.Volumes) != 0 {
		t.Fatalf("frame %+v", req)
	}
	s := req.Services[0]
	if s.Pull == nil || s.Pull.Tag != "" || s.ContainerName != "" || s.Env["TOKEN"] != "secret-canary" || !slices.Equal(s.SecretKeys, []string{"TOKEN"}) || s.Ports[0] != (protocol.Port{Container: 80, Host: 8080, Protocol: "tcp"}) {
		t.Fatalf("frame service %+v", s)
	}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = cluster
}

// Every definition a Deployment cannot express stops at the plan with its code, all at once,
// and so does a namespace the manifest no longer grants.
func TestKubernetesPlanBlockers(t *testing.T) {
	spec := ApplicationSpec{Kind: "compose.v1", Volumes: []DeclaredVolume{{Name: "data"}}, Services: []ApplicationService{
		{Name: "db", Image: "ghcr.io/org/db:1", Volumes: []ApplicationVolume{{Kind: "named", Source: "data", Target: "/var/lib/db"}}},
		{Name: "web", Image: "ghcr.io/org/web:1", Restart: "on-failure", Ports: []ApplicationPort{{Target: 80, Published: 80, HostIP: "127.0.0.1", Protocol: "tcp"}}},
		{Name: "api-", Image: "ghcr.io/org/api:1"},
		{Name: "ok", Image: "ghcr.io/org/ok:1"},
	}}
	st, a, app, cluster, m := kubernetesPlanFixture(t, spec, nil)
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{}}
	_, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	var blocked *PreflightBlockedError
	if !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{"kubernetes_unsupported"}) {
		t.Fatalf("blocked: %v", err)
	}
	want := map[string][]string{"db": {"k8s_volume"}, "web": {"k8s_host_ip", "k8s_restart"}, "api-": {"k8s_name"}}
	if len(blocked.Services) != len(want) {
		t.Fatalf("services %+v", blocked.Services)
	}
	for _, s := range blocked.Services {
		if !slices.Equal(s.Unsupported, want[s.Name]) || !slices.Equal(s.Blockers, []string{"kubernetes_unsupported"}) {
			t.Errorf("%s: %+v", s.Name, s)
		}
	}
	if len(resolver.called()) != 0 {
		t.Fatal("a blocked plan asked the registry")
	}
	if _, err := ts.SetEndpointDeployNamespaces(ctx, a, cluster, []string{"other"}); err != nil {
		t.Fatal(err)
	}
	pre, err := ts.PreflightApplication(ctx, a, app.ID)
	if err != nil || !slices.Contains(pre.Blockers, "k8s_namespace") || pre.Executable {
		t.Fatalf("namespace no longer granted: %+v %v", pre, err)
	}
}

// A cluster agent needs kubernetes.deploy and nothing else to take a plan.
func TestKubernetesPlanCapability(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.SetEndpointCapabilities(ctx, cluster, []string{protocol.CapabilityKubernetesInventory}); err != nil {
		t.Fatal(err)
	}
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	_, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	var blocked *PreflightBlockedError
	if !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{"agent_deploy_unsupported"}) {
		t.Fatalf("without kubernetes.deploy: %v", err)
	}
}

// kubeIdentity is what a cluster agent reports for a planned service.
func kubeIdentity(ps PlannedService) protocol.DeploymentIdentity {
	return protocol.DeploymentIdentity{Service: ps.Name, Kind: protocol.KindDeployment, Namespace: ps.Object.Namespace, Name: ps.Object.Name, UID: kubeUID, Generation: 1, ImageDigest: ps.PullDigest}
}

// A cluster result settles with Deployment identities in the planned namespace, names and
// digests; anything else is refused, and a success advances the instance's revisions.
func TestSettleKubernetesDeployment(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	result := func(ids ...protocol.DeploymentIdentity) protocol.DeploymentResult {
		return protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: ids}
	}
	web, api := kubeIdentity(d.Plan.Services[0]), kubeIdentity(d.Plan.Services[1])
	for name, bad := range map[string]protocol.DeploymentIdentity{
		"other namespace": func() protocol.DeploymentIdentity { i := web; i.Namespace = "billing"; return i }(),
		"other name":      func() protocol.DeploymentIdentity { i := web; i.Name = "shop-front-api"; return i }(),
		"other digest":    func() protocol.DeploymentIdentity { i := web; i.ImageDigest = digestOf("f"); return i }(),
		"a container":     {Service: "web", ContainerID: strings.Repeat("e", 64), ImageID: digestOf("e"), CreatedUnix: 1700000000, ImageDigest: digestOf("b")},
	} {
		if err := ts.SettleDeployment(ctx, cluster, result(bad, api)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := ts.SettleDeployment(ctx, cluster, result(web)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a success missing a service: %v", err)
	}
	if err := ts.SettleDeployment(ctx, cluster, result(web, api)); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.State != protocol.OutcomeSucceeded || len(got.Result.Services) != 2 || got.Result.Services[0].UID != kubeUID || got.Validation == nil {
		t.Fatalf("settled: %+v %v", got, err)
	}
	instance, err := ts.ReadApplicationInstance(ctx, a, app.ID, m.InstanceID)
	if err != nil || instance.CurrentRevision != 1 || instance.PreviousRevision != 0 {
		t.Fatalf("instance: %+v %v", instance, err)
	}
	// The instance has no recorded container to go back to.
	_, reason, err := ts.RollbackTarget(ctx, a, app.ID, d.ID)
	if err != nil || reason != RollbackNoPriorIdentity {
		t.Fatalf("rollback: %q %v", reason, err)
	}
}

// Removing a cluster instance names its services and target, not containers; a success releases
// the instance and marks the application removed.
func TestRemoveKubernetesApplication(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	d, req, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop-front"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Plan.Namespace != "shop" || len(req.Containers) != 0 || !slices.Equal(req.Services, []string{"api", "web"}) || req.Kubernetes == nil || req.Kubernetes.Namespace != "shop" || req.Kubernetes.InstanceID != m.InstanceID {
		t.Fatalf("removal %+v %+v", d.Plan, req)
	}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded, Services: []protocol.DeploymentIdentity{}, Steps: []protocol.DeploymentStep{
		{Service: "api", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded},
		{Service: "api", Step: protocol.StepRemove, Outcome: protocol.OutcomeSkipped},
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded},
		{Service: "web", Step: protocol.StepRemove, Outcome: protocol.OutcomeSucceeded},
	}}
	bad := res
	bad.Steps = append(slices.Clone(res.Steps), protocol.DeploymentStep{Service: "web", Step: protocol.StepStop, Outcome: protocol.OutcomeSucceeded})
	if err := ts.SettleDeployment(ctx, cluster, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a Docker step in a cluster removal: %v", err)
	}
	if err := ts.SettleDeployment(ctx, cluster, res); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ReadApplicationInstance(ctx, a, app.ID, m.InstanceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("instance kept: %v", err)
	}
	var removed int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM applications WHERE id=? AND removed_at IS NOT NULL`), app.ID).Scan(&removed); err != nil || removed != 1 {
		t.Fatalf("application not marked removed: %d %v", removed, err)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(records, func(r *AuditRecord) bool {
		return r.Action == string(permissions.ApplicationDestroy) && strings.Contains(r.Details, "services=2")
	}) {
		t.Fatal("the removal's audit row does not count its services")
	}
}

// A cluster instance's update check reads the running digest off its Deployment in the
// inventory, only when the Deployment carries the instance's label.
func TestKubernetesImageCheckReadsTheWorkload(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1"}}}, nil)
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("c")}}}
	check := func() ImageCheck {
		t.Helper()
		out, err := ts.CheckImageUpdates(ctx, a, app.ID, resolver, imageCheckKey, false)
		if err != nil || len(out.Services) != 1 {
			t.Fatalf("check: %+v %v", out, err)
		}
		return out.Services[0]
	}
	if c := check(); c.Verdict != "unknown_local" {
		t.Fatalf("no workload yet: %+v", c)
	}
	running := protocol.Workload{Kind: protocol.KindDeployment, Namespace: "shop", Name: "shop-front-web", Images: []string{"ghcr.io/org/web@" + digestOf("b")}, Instance: m.InstanceID, Application: app.ID}
	foreign := running
	foreign.Instance = ""
	putClusterInventory(t, ts, cluster, []protocol.Workload{foreign})
	if c := check(); c.Verdict != "unknown_local" {
		t.Fatalf("an unlabelled Deployment was read: %+v", c)
	}
	putClusterInventory(t, ts, cluster, []protocol.Workload{running})
	if c := check(); c.Verdict != "update_available" || c.LocalDigest != digestOf("b") || c.RemoteDigest != digestOf("c") {
		t.Fatalf("labelled Deployment: %+v", c)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestKubernetesPlan|TestSettleKubernetes|TestRemoveKubernetes|TestKubernetesImageCheck' ./internal/store/`
Expected: FAIL to compile: `undefined: KubernetesObject`, `d.Plan.Namespace undefined`.

- [ ] **Step 3: Implement**

In `internal/store/application_spec.go`:

Replace

```go
}

func ValidateApplicationSpec(spec ApplicationSpec) error {
	_, _, err := encodeApplicationSpec(spec)
```

with

```go
}

// serviceNames lists the spec's services in order.
func (spec ApplicationSpec) serviceNames() []string {
	out := make([]string, 0, len(spec.Services))
	for _, s := range spec.Services {
		out = append(out, s.Name)
	}
	return out
}

func ValidateApplicationSpec(spec ApplicationSpec) error {
	_, _, err := encodeApplicationSpec(spec)
```

In `internal/store/application_deployment.go`:

Replace

```go
	// DroppedMounts are the replaced container's mounts the recreate leaves off, for approval.
	DroppedMounts []protocol.Mount `json:"dropped_mounts,omitempty"`
}
```

with

```go
	// DroppedMounts are the replaced container's mounts the recreate leaves off, for approval.
	DroppedMounts []protocol.Mount `json:"dropped_mounts,omitempty"`
	// Object is the Deployment (and Service) a Kubernetes plan applies for this service; nil on
	// Docker. Replaces, ImageID and Mounts are then empty and the service always pulls.
	Object *KubernetesObject `json:"object,omitempty"`
}

// KubernetesObject names a service's objects: the Deployment and Service <name>, the
// ConfigMap <name>-env and the Secret <name>-secret.
type KubernetesObject struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
```

Replace

```go
	// Volumes are the named volumes the agent ensures, host names in first-use order.
	Volumes []string `json:"volumes,omitempty"`
}
type RemovalPlanTarget struct {
```

with

```go
	// Volumes are the named volumes the agent ensures, host names in first-use order.
	Volumes []string `json:"volumes,omitempty"`
	// Namespace is set exactly for a Kubernetes plan or removal.
	Namespace string `json:"namespace,omitempty"`
}
type RemovalPlanTarget struct {
```

Replace

```go
		return nil, ErrInvalid
	}
	if len(r.Update) > 0 {
		sorted := slices.Sorted(slices.Values(r.Update))
```

with

```go
		return nil, ErrInvalid
	}
	// A Kubernetes plan resolves every image at the registry, so it takes the update path. This
	// read only picks the path: both paths re-read the instance under authorization and refuse
	// one whose runtime changed since.
	var namespace string
	_ = t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT namespace FROM application_instances WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, id.String()).Scan(&namespace)
	kube := namespace != ""
	if kube && (len(r.PinImages) > 0 || resolver == nil) {
		return nil, ErrInvalid
	}
	if len(r.Update) > 0 {
		sorted := slices.Sorted(slices.Values(r.Update))
```

Replace

```go
	target := id.String() + "/deployments/" + planID
	var out *Deployment
	if len(r.Update) == 0 {
		err = t.withTenantTarget(ctx, a, permissions.ApplicationDeploy, target, func(tx *sql.Tx) error {
			dr, err := t.draftPlan(ctx, tx, a, id.String(), planID, r, true)
```

with

```go
	target := id.String() + "/deployments/" + planID
	var out *Deployment
	if len(r.Update) == 0 && !kube {
		err = t.withTenantTarget(ctx, a, permissions.ApplicationDeploy, target, func(tx *sql.Tx) error {
			dr, err := t.draftPlan(ctx, tx, a, id.String(), planID, r, true)
```

Replace

```go
				return err
			}
			if err := blocked(dr.blockers, dr.services...); err != nil {
				return err
```

with

```go
				return err
			}
			if dr.m.Runtime != protocol.RuntimeDocker {
				return ErrAdoptionChanged
			}
			if err := blocked(dr.blockers, dr.services...); err != nil {
				return err
```

Replace

```go
		}
		d, m, blockers := dr.d, dr.m, dr.blockers
		services = dr.services
		capabilities = dr.capabilities
```

with

```go
		}
		d, m, blockers := dr.d, dr.m, dr.blockers
		if (m.Runtime == protocol.RuntimeKubernetes) != kube {
			return ErrAdoptionChanged
		}
		services = dr.services
		capabilities = dr.capabilities
```

Replace

```go
			return err
		}
		for _, name := range r.Update {
			i := slices.IndexFunc(d.Plan.Services, func(ps PlannedService) bool { return ps.Name == name })
			if i < 0 || m.Bindings[name] == "" {
				blockers = append(blockers, "update_not_mapped")
				continue
```

with

```go
			return err
		}
		updates := r.Update
		if kube {
			updates = nil
			for _, ps := range d.Plan.Services {
				updates = append(updates, ps.Name)
			}
		}
		for _, name := range updates {
			i := slices.IndexFunc(d.Plan.Services, func(ps PlannedService) bool { return ps.Name == name })
			if i < 0 || (!kube && m.Bindings[name] == "") {
				blockers = append(blockers, "update_not_mapped")
				continue
```

Replace

```go
	}
	inspected := inspectsVerdicts(capabilities)
	var refused []BlockedService
	for i, s := range spec.Services {
```

with

```go
	}
	inspected := inspectsVerdicts(capabilities)
	var objects map[string]string
	if m.Runtime == protocol.RuntimeKubernetes {
		plan.Namespace = m.Namespace
		objects = protocol.KubernetesNames(m.Preview.Project, spec.serviceNames())
	}
	var refused []BlockedService
	for i, s := range spec.Services {
```

Replace

```go
			ps.Replaces = *row.InspectionTarget
		}
		plan.Services = append(plan.Services, ps)
	}
```

with

```go
			ps.Replaces = *row.InspectionTarget
		}
		if plan.Namespace != "" {
			ps.Object = &KubernetesObject{Namespace: plan.Namespace, Name: objects[s.Name]}
		}
		plan.Services = append(plan.Services, ps)
	}
```

Replace

```go
// capabilityBlockers refuses a plan the endpoint's agent could not run: no deployments, no live
// inspection for the plan to check, or a pull without deployment.pull. Apply checks again.
func capabilityBlockers(capabilities map[string]bool, plan DeploymentPlan) []string {
	var out []string
	if !capabilities[protocol.CapabilityDeploymentApply] {
```

with

```go
// capabilityBlockers refuses a plan the endpoint's agent could not run: no deployments, no live
// inspection for the plan to check, or a pull without deployment.pull. Apply checks again. A
// Kubernetes plan needs kubernetes.deploy only: the kubelet pulls, and nothing is inspected.
func capabilityBlockers(capabilities map[string]bool, plan DeploymentPlan) []string {
	if plan.Namespace != "" {
		if !capabilities[protocol.CapabilityKubernetesDeploy] {
			return []string{"agent_deploy_unsupported"}
		}
		return nil
	}
	var out []string
	if !capabilities[protocol.CapabilityDeploymentApply] {
```

In `internal/store/application_preflight.go`:

Replace

```go
		}
	}
	out := buildDeploymentPreflight(m, spec, snapshot, number == head, pins)
	out.Revision, out.ReceivedAt = number, received
	// observed_at is the agent's clock, received_at the server's.
```

with

```go
		}
	}
	var out *DeploymentPreflight
	if m.Runtime == protocol.RuntimeKubernetes {
		out = buildKubernetesPreflight(m, spec)
	} else {
		out = buildDeploymentPreflight(m, spec, snapshot, number == head, pins)
	}
	out.Revision, out.ReceivedAt = number, received
	// observed_at is the agent's clock, received_at the server's.
```

Replace

```go
}

// resolveMounts turns a service's volumes into runtime mounts: a named volume by its host name
// in project, a bind by its path.
```

with

```go
}

// kubernetesRestart is what a one-replica Deployment's restartPolicy Always expresses.
var kubernetesRestart = map[string]bool{"": true, "always": true, "unless-stopped": true}

// buildKubernetesPreflight checks spec against a Kubernetes mapping: the namespace is still one
// the manifest grants (k8s_namespace), and each service is stateless and expressible, else it
// carries kubernetes_unsupported with the k8s_ codes that say why. Images need a tag or digest;
// the plan resolves them at the registry.
func buildKubernetesPreflight(m *ApplicationMapping, spec ApplicationSpec) *DeploymentPreflight {
	out := &DeploymentPreflight{InstanceID: m.InstanceID, EndpointID: m.Preview.EndpointID, EndpointName: m.Preview.EndpointName, Revision: m.Preview.Revision, MappingVersion: m.Version, Blockers: []string{}, Services: []PreflightService{}}
	if !slices.Contains(m.DeployNamespaces, m.Namespace) {
		out.Blockers = append(out.Blockers, "k8s_namespace")
	}
	objects := protocol.KubernetesNames(m.Preview.Project, spec.serviceNames())
	named := map[string]int{}
	for _, n := range objects {
		named[n]++
	}
	blocked := false
	for _, s := range spec.Services {
		row := PreflightService{Name: s.Name, Reference: s.Image, Blockers: []string{}, Mounts: []protocol.Mount{}, DroppedBinds: []protocol.Mount{}, DroppedMounts: []protocol.Mount{}, UnsupportedMounts: []protocol.Mount{}}
		var codes []string
		if len(s.Volumes) > 0 {
			codes = append(codes, "k8s_volume")
		}
		if slices.ContainsFunc(s.Ports, func(p ApplicationPort) bool { return p.HostIP != "" }) {
			codes = append(codes, "k8s_host_ip")
		}
		if !kubernetesRestart[s.Restart] {
			codes = append(codes, "k8s_restart")
		}
		if named[objects[s.Name]] > 1 || !protocol.ValidLabelValue(s.Name) || !protocol.ValidLabelValue(m.Preview.Project) {
			codes = append(codes, "k8s_name")
		}
		if len(codes) > 0 {
			row.Blockers, row.Unsupported = append(row.Blockers, "kubernetes_unsupported"), codes
		}
		if _, tag := protocol.SplitImageReference(s.Image); tag == "" {
			row.Blockers = append(row.Blockers, "explicit_image_reference_required")
		}
		ports := map[portKey]bool{}
		for _, p := range s.Ports {
			k := portKey{p.Published, p.Protocol}
			if ports[k] {
				row.Blockers = append(row.Blockers, "desired_port_overlap")
				break
			}
			ports[k] = true
		}
		out.Services = append(out.Services, row)
		blocked = blocked || len(row.Blockers) > 0
	}
	out.Executable = !blocked && len(out.Blockers) == 0
	return out
}

// resolveMounts turns a service's volumes into runtime mounts: a named volume by its host name
// in project, a bind by its path.
```

In `internal/store/application_apply.go`:

Replace

```go
	"errors"
	"fmt"
	"time"
```

with

```go
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
```

Replace

```go
		return protocol.DeploymentRequest{}, ErrAdoptionChanged
	}
	names := map[string]string{}
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name FROM application_resources WHERE instance_id=? AND endpoint_id=?`), d.InstanceID, d.EndpointID)
```

with

```go
		return protocol.DeploymentRequest{}, ErrAdoptionChanged
	}
	if d.Plan.Namespace != "" {
		return t.kubernetesFrame(ctx, tx, d, spec, values, now)
	}
	names := map[string]string{}
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name FROM application_resources WHERE instance_id=? AND endpoint_id=?`), d.InstanceID, d.EndpointID)
```

Replace

```go
}

// frameBlocker names what stops req reaching an agent that accepts maxFrameBytes, or "" when
// nothing does. Plan and apply both run it: capabilities can change between them.
```

with

```go
}

// kubernetesFrame is buildDeploymentFrame for a Kubernetes plan: the instance's namespace, which
// must still be the plan's, and per service the pinned pull, the environment values and the keys
// among them that are secret-backed. The kubelet pulls, so no credential travels.
func (t *tenancyStore) kubernetesFrame(ctx context.Context, tx *sql.Tx, d *Deployment, spec ApplicationSpec, values map[string]string, now time.Time) (protocol.DeploymentRequest, error) {
	var namespace string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT namespace FROM application_instances WHERE id=? AND endpoint_id=?`), d.InstanceID, d.EndpointID).Scan(&namespace)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && namespace != d.Plan.Namespace) {
		return protocol.DeploymentRequest{}, ErrAdoptionChanged
	}
	if err != nil {
		return protocol.DeploymentRequest{}, err
	}
	req := protocol.DeploymentRequest{Deployment: d.ID, RequestID: d.CorrelationID, Endpoint: d.EndpointID, Project: d.Plan.Project, Revision: d.Revision, IssuedAt: now, Deadline: now.Add(DeploymentApplyDeadline), Services: []protocol.DeploymentService{},
		Kubernetes: &protocol.KubernetesTarget{Namespace: namespace, ApplicationID: d.ApplicationID, InstanceID: d.InstanceID, SpecDigest: d.SpecDigest}}
	for i, ps := range d.Plan.Services {
		if spec.Services[i].Name != ps.Name || ps.PullDigest == "" || ps.Object == nil {
			return protocol.DeploymentRequest{}, ErrAdoptionChanged
		}
		svc := protocol.DeploymentService{Name: ps.Name, Restart: ps.Restart, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: []protocol.Mount{}, Pull: &protocol.ImagePull{Reference: ps.PullReference, Digest: ps.PullDigest}}
		for _, p := range ps.Ports {
			svc.Ports = append(svc.Ports, protocol.Port{Container: p.Target, Host: p.Published, Protocol: p.Protocol})
		}
		// Every value is secret-backed today: the definition holds references only.
		for envName, ref := range spec.Services[i].Environment {
			svc.Env[envName] = values[ref.SecretRef]
		}
		svc.SecretKeys = slices.Sorted(maps.Keys(spec.Services[i].Environment))
		req.Services = append(req.Services, svc)
	}
	return req, nil
}

// frameBlocker names what stops req reaching an agent that accepts maxFrameBytes, or "" when
// nothing does. Plan and apply both run it: capabilities can change between them.
```

Replace

```go
// in full before any write.
func (t *tenancyStore) settleApply(ctx context.Context, tx *sql.Tx, endpointID, instance string, plan DeploymentPlan, res protocol.DeploymentResult) error {
	replaced := map[string]PlannedService{}
	for _, ps := range plan.Services {
```

with

```go
// in full before any write.
func (t *tenancyStore) settleApply(ctx context.Context, tx *sql.Tx, endpointID, instance string, plan DeploymentPlan, res protocol.DeploymentResult) error {
	if plan.Namespace != "" {
		return t.settleKubernetesApply(ctx, tx, instance, plan, res)
	}
	replaced := map[string]PlannedService{}
	for _, ps := range plan.Services {
```

Replace

```go
	for _, idn := range res.Services {
		ps, ok := replaced[idn.Service]
		if !ok || seen[idn.Service] {
			return ErrInvalid
		}
```

with

```go
	for _, idn := range res.Services {
		ps, ok := replaced[idn.Service]
		if !ok || seen[idn.Service] || idn.Kind != "" {
			return ErrInvalid
		}
```

Replace

```go
}

// settleRemoval forgets each target the steps show is off the host: its precondition matched
// the pinned identity (or found it gone) and then its remove step succeeded, or stop and remove
```

with

```go
}

// settleKubernetesApply checks each identity names a distinct planned service's Deployment, in
// the planned namespace and name, running the planned digest; a success accounts for every
// service. The result row keeps the identities; nothing else is rebound.
func (t *tenancyStore) settleKubernetesApply(ctx context.Context, tx *sql.Tx, instance string, plan DeploymentPlan, res protocol.DeploymentResult) error {
	planned := map[string]PlannedService{}
	for _, ps := range plan.Services {
		planned[ps.Name] = ps
	}
	seen := map[string]bool{}
	for _, idn := range res.Services {
		ps, ok := planned[idn.Service]
		if !ok || seen[idn.Service] || ps.Object == nil || idn.Kind != protocol.KindDeployment || idn.Namespace != ps.Object.Namespace || idn.Name != ps.Object.Name || idn.ImageDigest != ps.PullDigest {
			return ErrInvalid
		}
		seen[idn.Service] = true
	}
	if res.Outcome != protocol.OutcomeSucceeded {
		return nil
	}
	if len(seen) != len(plan.Services) {
		return ErrInvalid
	}
	return t.clearImageChecks(ctx, tx, instance)
}

// settleRemoval forgets each target the steps show is off the host: its precondition matched
// the pinned identity (or found it gone) and then its remove step succeeded, or stop and remove
```

Replace

```go
		return ErrInvalid
	}
	targets := map[string]string{}
	for _, c := range plan.Containers {
```

with

```go
		return ErrInvalid
	}
	// A cluster removal reports precondition and remove steps per service it found labelled;
	// only a success releases the instance.
	if plan.Namespace != "" {
		for _, s := range res.Steps {
			if s.Step != protocol.StepPrecondition && s.Step != protocol.StepRemove {
				return ErrInvalid
			}
		}
		if res.Outcome != protocol.OutcomeSucceeded {
			return nil
		}
		return t.releaseRemoved(ctx, tx, appID, instance)
	}
	targets := map[string]string{}
	for _, c := range plan.Containers {
```

Replace

```go
		return nil
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM application_resources WHERE instance_id=?`), instance); err != nil {
		return err
```

with

```go
		return nil
	}
	return t.releaseRemoved(ctx, tx, appID, instance)
}

// releaseRemoved forgets a removed instance and marks its application removed.
func (t *tenancyStore) releaseRemoved(ctx context.Context, tx *sql.Tx, appID, instance string) error {
	if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM application_resources WHERE instance_id=?`), instance); err != nil {
		return err
```

Replace

```go
			return err
		}
		var project, endpoint, endpointName, endpointState string
		var mappingVersion int
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.project,i.endpoint_id,i.mapping_version,e.name,e.state FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND i.environment_id=? AND i.application_id=? AND i.id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), instanceID.String()).Scan(&project, &endpoint, &mappingVersion, &endpointName, &endpointState)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAdoptionChanged
```

with

```go
			return err
		}
		var project, endpoint, endpointName, endpointState, namespace, runtime string
		var mappingVersion, current int
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.project,i.endpoint_id,i.mapping_version,i.namespace,i.current_revision,e.name,e.state,e.runtime FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND i.environment_id=? AND i.application_id=? AND i.id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), instanceID.String()).Scan(&project, &endpoint, &mappingVersion, &namespace, &current, &endpointName, &endpointState, &runtime)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAdoptionChanged
```

Replace

```go
			return err
		}
		if r.Confirm != project {
			return ErrInvalid
```

with

```go
			return err
		}
		if (runtime == protocol.RuntimeKubernetes) != (namespace != "") {
			return ErrRuntimeUnsupported
		}
		if r.Confirm != project {
			return ErrInvalid
```

Replace

```go
		}
		now := time.Now().UTC()
		plan := DeploymentPlan{Project: project, Services: []PlannedService{}, Containers: []RemovalPlanTarget{}}
		req = &protocol.RemovalRequest{Deployment: id, RequestID: a.CorrelationID, Endpoint: endpoint, Project: project, IssuedAt: now, Deadline: now.Add(DeploymentApplyDeadline), Containers: []protocol.RemovalTarget{}}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name,image_id,created_at,service_name FROM application_resources WHERE instance_id=? AND endpoint_id=? ORDER BY container_id`), instanceID.String(), endpoint)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c RemovalPlanTarget
			var created time.Time
			if err := rows.Scan(&c.ContainerID, &c.Name, &c.ImageID, &created, &c.Service); err != nil {
				rows.Close()
				return err
			}
			// Whole seconds: the precision the runtime reports, as InspectionTarget uses.
			c.CreatedUnix = created.Unix()
			if c.Service == "" {
				c.Service = "unmapped-" + c.ContainerID[:min(12, len(c.ContainerID))]
			}
			plan.Containers = append(plan.Containers, c)
			req.Containers = append(req.Containers, protocol.RemovalTarget{Service: c.Service, Target: protocol.InspectionTarget{ContainerID: c.ContainerID, ImageID: c.ImageID, CreatedUnix: c.CreatedUnix}})
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(plan.Containers) == 0 {
			return ErrAdoptionChanged
		}
		if len(plan.Containers) > protocol.MaxRemovalTargets {
			return ErrRemovalTooLarge
		}
```

with

```go
		}
		now := time.Now().UTC()
		plan := DeploymentPlan{Project: project, Services: []PlannedService{}, Containers: []RemovalPlanTarget{}, Namespace: namespace}
		req = &protocol.RemovalRequest{Deployment: id, RequestID: a.CorrelationID, Endpoint: endpoint, Project: project, IssuedAt: now, Deadline: now.Add(DeploymentApplyDeadline), Containers: []protocol.RemovalTarget{}}
		count, unit := 0, "containers"
		if namespace != "" {
			services, err := t.removalServices(ctx, tx, a, appID.String(), head, current)
			if err != nil {
				return err
			}
			req.Kubernetes = &protocol.KubernetesTarget{Namespace: namespace, ApplicationID: appID.String(), InstanceID: instanceID.String(), SpecDigest: digest}
			req.Services, count, unit = services, len(services), "services"
		} else {
			rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name,image_id,created_at,service_name FROM application_resources WHERE instance_id=? AND endpoint_id=? ORDER BY container_id`), instanceID.String(), endpoint)
			if err != nil {
				return err
			}
			for rows.Next() {
				var c RemovalPlanTarget
				var created time.Time
				if err := rows.Scan(&c.ContainerID, &c.Name, &c.ImageID, &created, &c.Service); err != nil {
					rows.Close()
					return err
				}
				// Whole seconds: the precision the runtime reports, as InspectionTarget uses.
				c.CreatedUnix = created.Unix()
				if c.Service == "" {
					c.Service = "unmapped-" + c.ContainerID[:min(12, len(c.ContainerID))]
				}
				plan.Containers = append(plan.Containers, c)
				req.Containers = append(req.Containers, protocol.RemovalTarget{Service: c.Service, Target: protocol.InspectionTarget{ContainerID: c.ContainerID, ImageID: c.ImageID, CreatedUnix: c.CreatedUnix}})
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if len(plan.Containers) == 0 {
				return ErrAdoptionChanged
			}
			count = len(plan.Containers)
		}
		if count > protocol.MaxRemovalTargets {
			return ErrRemovalTooLarge
		}
```

Replace

```go
		out = &Deployment{ID: id, ApplicationID: appID.String(), InstanceID: instanceID.String(), EndpointID: endpoint, EndpointName: endpointName, Kind: "remove", State: "applying", Revision: head, SpecDigest: digest, MappingVersion: mappingVersion, Plan: plan, CreatedBy: a.ActorID, CreatedAt: now, ExpiresAt: req.Deadline, AppliedBy: a.ActorID, AppliedAt: &now, Deadline: &req.Deadline, CorrelationID: a.CorrelationID}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,kind,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at,applied_by,applied_at,deadline,correlation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), out.ID, a.OrganizationID, a.EnvironmentID, out.ApplicationID, out.InstanceID, out.EndpointID, project, out.Kind, out.State, out.Revision, out.SpecDigest, out.MappingVersion, string(raw), out.CreatedBy, now, out.ExpiresAt, out.AppliedBy, now, req.Deadline, out.CorrelationID)
		details = fmt.Sprintf("project=%s endpoint=%s containers=%d", project, endpoint, len(plan.Containers))
		return err
	})
```

with

```go
		out = &Deployment{ID: id, ApplicationID: appID.String(), InstanceID: instanceID.String(), EndpointID: endpoint, EndpointName: endpointName, Kind: "remove", State: "applying", Revision: head, SpecDigest: digest, MappingVersion: mappingVersion, Plan: plan, CreatedBy: a.ActorID, CreatedAt: now, ExpiresAt: req.Deadline, AppliedBy: a.ActorID, AppliedAt: &now, Deadline: &req.Deadline, CorrelationID: a.CorrelationID}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,kind,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at,applied_by,applied_at,deadline,correlation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), out.ID, a.OrganizationID, a.EnvironmentID, out.ApplicationID, out.InstanceID, out.EndpointID, project, out.Kind, out.State, out.Revision, out.SpecDigest, out.MappingVersion, string(raw), out.CreatedBy, now, out.ExpiresAt, out.AppliedBy, now, req.Deadline, out.CorrelationID)
		details = fmt.Sprintf("project=%s endpoint=%s %s=%d", project, endpoint, unit, count)
		return err
	})
```

Replace

```go
}

// removalBlockers refuses a removal the host may not survive: a fresh inventory must show no
// container of the project outside the adopted ones, and the instance's last acted-on row must
```

with

```go
}

// removalServices names what a cluster removal deletes: the services of the latest revision and
// of the one last applied (current, 0 when none). The agent finds each service's Deployment,
// Service and ConfigMap by the instance label, and its Secret by the name the service gives it.
func (t *tenancyStore) removalServices(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, head, current int) ([]string, error) {
	services := map[string]bool{}
	for _, number := range []int{head, current} {
		if number == 0 {
			continue
		}
		var raw, digest string
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), a.OrganizationID, a.EnvironmentID, app, number).Scan(&raw, &digest); err != nil {
			return nil, err
		}
		var spec ApplicationSpec
		if applicationSpecDigest([]byte(raw)) != digest || json.Unmarshal([]byte(raw), &spec) != nil {
			return nil, ErrRevisionCorrupt
		}
		for _, s := range spec.Services {
			services[s.Name] = true
		}
	}
	return slices.Sorted(maps.Keys(services)), nil
}

// removalBlockers refuses a removal the host may not survive: a fresh inventory must show no
// container of the project outside the adopted ones, and the instance's last acted-on row must
```

In `internal/store/rollback.go`:

Replace

```go
			return ErrRevisionCorrupt
		}
		if len(plan.Services) == 0 {
			reason = RollbackNoPriorIdentity
			return nil
```

with

```go
			return ErrRevisionCorrupt
		}
		// A Kubernetes plan replaces no recorded container, so it has nothing to roll back to.
		if len(plan.Services) == 0 || plan.Namespace != "" {
			reason = RollbackNoPriorIdentity
			return nil
```

In `internal/store/image_checks.go`:

Replace

```go
			return err
		}
		for _, s := range spec.Services {
			container, mapped := m.Bindings[s.Name]
			if !mapped {
				continue
			}
```

with

```go
			return err
		}
		kube := m.Runtime == protocol.RuntimeKubernetes
		objects := protocol.KubernetesNames(m.Preview.Project, spec.serviceNames())
		for _, s := range spec.Services {
			container, mapped := m.Bindings[s.Name]
			if !mapped && !kube {
				continue
			}
```

Replace

```go
				w.Row.Verdict = "pinned"
			default:
				w.Row.LocalDigest = localRepoDigest(images[containers[container].ImageID], w.Ref)
				if w.Row.LocalDigest == "" {
					w.Row.Verdict = "unknown_local"
```

with

```go
				w.Row.Verdict = "pinned"
			default:
				if kube {
					w.Row.LocalDigest = workloadDigest(snapshot, m, objects[s.Name], w.Ref)
				} else {
					w.Row.LocalDigest = localRepoDigest(images[containers[container].ImageID], w.Ref)
				}
				if w.Row.LocalDigest == "" {
					w.Row.Verdict = "unknown_local"
```

Replace

```go
}

// localRepoDigest returns the host image's digest for exactly the reference's repository, or ""
// when there is none, more than one, or it is not a sha256 digest.
```

with

```go
}

// workloadDigest is the digest a Kubernetes instance's Deployment runs for ref: its one image,
// pinned as host/repository@digest to exactly ref's repository. "" when the Deployment is not
// reported, is not the instance's, or runs anything else.
func workloadDigest(snapshot protocol.Snapshot, m *ApplicationMapping, name string, ref registry.Reference) string {
	if snapshot.Kubernetes == nil {
		return ""
	}
	for _, w := range snapshot.Kubernetes.Workloads {
		if w.Kind != protocol.KindDeployment || w.Namespace != m.Namespace || w.Name != name || w.Instance != m.InstanceID || len(w.Images) != 1 {
			continue
		}
		repo, digest, ok := strings.Cut(w.Images[0], "@")
		parsed, err := registry.ParseReference(repo)
		if ok && err == nil && parsed.Host == ref.Host && parsed.Repository == ref.Repository && validSHA256(digest) {
			return digest
		}
	}
	return ""
}

// localRepoDigest returns the host image's digest for exactly the reference's repository, or ""
// when there is none, more than one, or it is not a sha256 digest.
```

- [ ] **Step 4: Run the tests to verify they pass, on both databases**

Run: `gofmt -w internal/store && go vet ./... && go test -race -count=1 ./internal/store/... && PG=… go test -count=1 ./internal/store/... && go test -count=1 ./internal/api/`
Expected: every package `ok`; the Docker plan, apply, settle, removal, rollback and update-check tests are unchanged and pass.

- [ ] **Step 5: DOX and commit**

In `internal/store/AGENTS.md`, `## Local Contracts`, append:

```markdown
- Kubernetes plans (`DeploymentPlan.Namespace` set; each `PlannedService.Object{Namespace, Name}` from `protocol.KubernetesNames`): `buildKubernetesPreflight` blocks `k8s_namespace` when the namespace is no longer in the endpoint's list, and per service `kubernetes_unsupported` with `Unsupported` codes `k8s_volume`, `k8s_host_ip`, `k8s_restart` (neither `always`, `unless-stopped` nor default) and `k8s_name` (a project or service that is not a label value, or a name collision), plus `explicit_image_reference_required` and `desired_port_overlap` within a service; no image inventory, container, inspection or mapping-review check applies. `PlanDeployment` picks its path from an unaudited read of the instance's namespace and re-checks it under authorization (`ErrAdoptionChanged` when it changed): a Kubernetes plan always takes the update path and resolves every service at the registry, refuses `PinImages` and a nil resolver, and needs only `kubernetes.deploy` (`agent_deploy_unsupported`). `kubernetesFrame` builds the frame from the instance's current namespace, which must be the plan's: `KubernetesTarget` (spec digest included), per service the pinned pull with no tag, ports without host address, the values and `SecretKeys` (every env key: the definition holds references only), no volume and no registry credential. `settleKubernetesApply` takes only `Kind: Deployment` identities at the planned namespace, name and digest, keeps them in the result and clears the image checks on success; a Docker settle refuses a cluster identity. `RemoveApplication` on a cluster names the services of the latest and the last-applied revisions (`removalServices`, at most 100, else `ErrRemovalTooLarge`) with the `KubernetesTarget`, audits `services=<n>`, and `settleRemoval` takes only precondition and remove steps and releases the instance on success. `RollbackTarget` answers `no_prior_identity` for a Kubernetes plan. `CheckImageUpdates` reads a Kubernetes service's local digest from its labelled Deployment's single `host/repo@digest` image in the inventory (`workloadDigest`).
```

```bash
git add internal/store && make tidy-check lint && git commit -m "feat(store): plan, apply, settle and remove on a Kubernetes namespace" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: `render` — one service as KyYard-labelled objects

**Files:**
- Create: `internal/runtime/kubernetes/render/render.go`
- Test: `internal/runtime/kubernetes/render/render_test.go`
- Docs: `internal/runtime/kubernetes/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.DeploymentRequest` with `Kubernetes` set and validated by `ValidateFor(protocol.RuntimeKubernetes, now)`, `protocol.KubernetesNames` (Task 1).
- Produces (package `render`; imported by `internal/runtime/kubernetes` only):
  ```go
  const LabelName, LabelInstanceName, LabelManagedBy = "app.kubernetes.io/name", "app.kubernetes.io/instance", "app.kubernetes.io/managed-by"
  const LabelApplication, LabelInstance, LabelService = "kyyard.busnes.app/application", "kyyard.busnes.app/instance", "kyyard.busnes.app/service"
  const AnnotationRevision, AnnotationDeployment, AnnotationSpecDigest = "kyyard.busnes.app/revision", "kyyard.busnes.app/deployment", "kyyard.busnes.app/spec-digest"
  const ManagedBy = "kyyard"
  type Set struct {
      Service, Name string          // the service, and its objects' base name
      ConfigMap  *corev1.ConfigMap  // <name>-env
      Secret     *corev1.Secret     // <name>-secret, nil without secret keys
      Deployment *appsv1.Deployment // <name>
      Endpoint   *corev1.Service    // <name>, nil without ports
  }
  func Request(req protocol.DeploymentRequest) []Set                 // one Set per service, in order
  func Labels(req protocol.DeploymentRequest, service string) map[string]string
  func Selector(instance, service string) map[string]string          // kyyard.busnes.app/instance and /service
  ```

- [ ] **Step 1: Write the failing test**

Create `internal/runtime/kubernetes/render/render_test.go`:

```go
package render_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
)

const (
	digest     = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	specDigest = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	app        = "11111111-2222-4333-8444-555555555555"
	instance   = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	deployment = "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b"
)

func request(project string, services ...protocol.DeploymentService) protocol.DeploymentRequest {
	now := time.Now()
	return protocol.DeploymentRequest{Deployment: deployment, RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: project, Revision: 3, IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: app, InstanceID: instance, SpecDigest: specDigest}, Services: services}
}

func web() protocol.DeploymentService {
	return protocol.DeploymentService{Name: "web", Restart: "always", Pull: &protocol.ImagePull{Reference: "ghcr.io/org/web@" + digest, Digest: digest},
		Ports: []protocol.Port{{Container: 80, Host: 8080, Protocol: "tcp"}, {Container: 80, Host: 8081, Protocol: "tcp"}, {Container: 53, Host: 5353, Protocol: "udp"}},
		Env:   map[string]string{"MODE": "prod", "TOKEN": "s3cret"}, SecretKeys: []string{"TOKEN"}, Mounts: []protocol.Mount{}}
}

func api() protocol.DeploymentService {
	return protocol.DeploymentService{Name: "api", Pull: &protocol.ImagePull{Reference: "ghcr.io/org/api@" + digest, Digest: digest}, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: []protocol.Mount{}}
}

// The two-service definition renders exactly these objects: every label and annotation, the
// ConfigMap and Secret split, the Service's port mapping, and no Secret or Service where there
// is nothing to put in one.
func TestRenderTwoServices(t *testing.T) {
	req := request("shop", web(), api())
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
	sets := render.Request(req)
	labels := func(s string) map[string]string {
		return map[string]string{"app.kubernetes.io/name": s, "app.kubernetes.io/instance": "shop", "app.kubernetes.io/managed-by": "kyyard", "kyyard.busnes.app/application": app, "kyyard.busnes.app/instance": instance, "kyyard.busnes.app/service": s}
	}
	meta := func(name, s string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: labels(s), Annotations: map[string]string{"kyyard.busnes.app/revision": "3", "kyyard.busnes.app/deployment": deployment, "kyyard.busnes.app/spec-digest": specDigest}}
	}
	one, automount := int32(1), false
	deploymentOf := func(name, s, image string, ports []corev1.ContainerPort, envFrom []corev1.EnvFromSource) *appsv1.Deployment {
		return &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta(name, s), Spec: appsv1.DeploymentSpec{
			Replicas: &one, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"kyyard.busnes.app/instance": instance, "kyyard.busnes.app/service": s}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels(s), Annotations: map[string]string{"kyyard.busnes.app/deployment": deployment}}, Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyAlways, AutomountServiceAccountToken: &automount,
				Containers: []corev1.Container{{Name: s, Image: image, Ports: ports, EnvFrom: envFrom}},
			}},
		}}
	}
	want := []render.Set{
		{
			Service: "web", Name: "shop-web",
			ConfigMap: &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta("shop-web-env", "web"), Data: map[string]string{"MODE": "prod"}},
			Secret:    &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: meta("shop-web-secret", "web"), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"TOKEN": []byte("s3cret")}},
			Deployment: deploymentOf("shop-web", "web", "ghcr.io/org/web@"+digest,
				[]corev1.ContainerPort{{ContainerPort: 80, Protocol: corev1.ProtocolTCP}, {ContainerPort: 53, Protocol: corev1.ProtocolUDP}},
				[]corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "shop-web-env"}}}, {SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "shop-web-secret"}}}}),
			Endpoint: &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta("shop-web", "web"), Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"kyyard.busnes.app/instance": instance, "kyyard.busnes.app/service": "web"},
				Ports: []corev1.ServicePort{
					{Name: "tcp-8080", Port: 8080, TargetPort: intstr.FromInt32(80), Protocol: corev1.ProtocolTCP},
					{Name: "tcp-8081", Port: 8081, TargetPort: intstr.FromInt32(80), Protocol: corev1.ProtocolTCP},
					{Name: "udp-5353", Port: 5353, TargetPort: intstr.FromInt32(53), Protocol: corev1.ProtocolUDP},
				},
			}},
		},
		{
			Service: "api", Name: "shop-api",
			ConfigMap:  &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta("shop-api-env", "api"), Data: map[string]string{}},
			Deployment: deploymentOf("shop-api", "api", "ghcr.io/org/api@"+digest, nil, []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "shop-api-env"}}}}),
		},
	}
	if !reflect.DeepEqual(sets, want) {
		got, _ := json.MarshalIndent(sets, "", "  ")
		t.Fatalf("rendered:\n%s", got)
	}
	// Each object survives the API's strict decoding: no field is misspelt or dropped.
	decoder := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer()
	for _, set := range sets {
		for _, obj := range []any{set.ConfigMap, set.Secret, set.Deployment, set.Endpoint} {
			if reflect.ValueOf(obj).IsNil() {
				continue
			}
			raw, _ := json.Marshal(obj)
			if _, _, err := decoder.Decode(raw, nil, nil); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
		}
	}
}

// Whatever the project and service names the definition allows, every rendered name and label
// is one Kubernetes accepts, and colliding slugs get distinct names.
func TestRenderNamesAreValid(t *testing.T) {
	svc := func(name string) protocol.DeploymentService {
		s := api()
		s.Name = name
		s.Ports = []protocol.Port{{Container: 80, Host: 80, Protocol: "tcp"}}
		return s
	}
	for _, project := range []string{"shop", "1shop", "Shop.Front_2", strings.Repeat("p", 64)} {
		sets := render.Request(request(project, svc("my_api"), svc("my-api"), svc(strings.Repeat("s", 63))))
		seen := map[string]bool{}
		for _, set := range sets {
			if seen[set.Name] {
				t.Fatalf("%s: %s rendered twice", project, set.Name)
			}
			seen[set.Name] = true
			for _, errs := range [][]string{
				validation.IsDNS1035Label(set.Endpoint.Name),
				validation.IsDNS1123Label(set.Deployment.Name),
				validation.IsDNS1123Subdomain(set.ConfigMap.Name),
				validation.IsDNS1123Label(set.Deployment.Spec.Template.Spec.Containers[0].Name),
				validation.IsValidPortName(set.Endpoint.Spec.Ports[0].Name),
			} {
				if len(errs) > 0 {
					t.Fatalf("%s/%s: %v", project, set.Service, errs)
				}
			}
		}
	}
	// Label values: the project and service must themselves be label values, which the plan
	// checks (k8s_name) before any frame is sent.
	for _, v := range render.Labels(request("shop", svc("my_api")), "my_api") {
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			t.Fatalf("%q: %v", v, errs)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/runtime/kubernetes/render/`
Expected: FAIL: `no non-test Go files in .../render` (the package does not exist yet).

- [ ] **Step 3: Implement**

Create `internal/runtime/kubernetes/render/render.go`:

```go
// Package render turns one deployment request into the Kubernetes objects a cluster agent
// applies: per service a Deployment, a Service, a ConfigMap and a Secret, labelled as KyYard's.
// It runs in the agent only; the server never imports it and links no Kubernetes library.
package render

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Labels and annotations on every object KyYard applies. LabelInstance is ownership: an object
// with the planned name that lacks it, or carries another instance's, is not KyYard's to touch.
const (
	LabelName            = "app.kubernetes.io/name"
	LabelInstanceName    = "app.kubernetes.io/instance"
	LabelManagedBy       = "app.kubernetes.io/managed-by"
	LabelApplication     = "kyyard.busnes.app/application"
	LabelInstance        = "kyyard.busnes.app/instance"
	LabelService         = "kyyard.busnes.app/service"
	AnnotationRevision   = "kyyard.busnes.app/revision"
	AnnotationDeployment = "kyyard.busnes.app/deployment"
	AnnotationSpecDigest = "kyyard.busnes.app/spec-digest"
	ManagedBy            = "kyyard"
)

// Set is one service's objects. Secret is nil when the service has no secret-backed value, and
// Service is nil when it publishes no port.
type Set struct {
	Service    string
	Name       string
	ConfigMap  *corev1.ConfigMap
	Secret     *corev1.Secret
	Deployment *appsv1.Deployment
	Endpoint   *corev1.Service
}

// Request renders every service of req, in order. req must have passed
// ValidateFor(protocol.RuntimeKubernetes, ...).
func Request(req protocol.DeploymentRequest) []Set {
	services := make([]string, 0, len(req.Services))
	for _, s := range req.Services {
		services = append(services, s.Name)
	}
	names := protocol.KubernetesNames(req.Project, services)
	out := make([]Set, 0, len(req.Services))
	for _, s := range req.Services {
		out = append(out, service(req, s, names[s.Name]))
	}
	return out
}

// Labels are the labels of every object of service s of the instance req targets.
func Labels(req protocol.DeploymentRequest, s string) map[string]string {
	k := req.Kubernetes
	return map[string]string{LabelName: s, LabelInstanceName: req.Project, LabelManagedBy: ManagedBy, LabelApplication: k.ApplicationID, LabelInstance: k.InstanceID, LabelService: s}
}

// Selector is the part of Labels a Deployment and its Service select pods by; it never changes
// for a service, so a later apply can update the Deployment.
func Selector(instance, s string) map[string]string {
	return map[string]string{LabelInstance: instance, LabelService: s}
}

func service(req protocol.DeploymentRequest, s protocol.DeploymentService, name string) Set {
	k := req.Kubernetes
	meta := func(n string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: n, Namespace: k.Namespace, Labels: Labels(req, s.Name), Annotations: map[string]string{
			AnnotationRevision: strconv.Itoa(req.Revision), AnnotationDeployment: req.Deployment, AnnotationSpecDigest: k.SpecDigest,
		}}
	}
	set := Set{Service: s.Name, Name: name}
	config := map[string]string{}
	secret := map[string][]byte{}
	for key, value := range s.Env {
		if slices.Contains(s.SecretKeys, key) {
			secret[key] = []byte(value)
		} else {
			config[key] = value
		}
	}
	set.ConfigMap = &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta(name + "-env"), Data: config}
	envFrom := []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name + "-env"}}}}
	if len(s.SecretKeys) > 0 {
		set.Secret = &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: meta(name + "-secret"), Type: corev1.SecretTypeOpaque, Data: secret}
		envFrom = append(envFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name + "-secret"}}})
	}
	var containerPorts []corev1.ContainerPort
	var servicePorts []corev1.ServicePort
	for _, p := range s.Ports {
		proto := corev1.Protocol(strings.ToUpper(p.Protocol))
		if !slices.ContainsFunc(containerPorts, func(c corev1.ContainerPort) bool { return c.ContainerPort == int32(p.Container) && c.Protocol == proto }) {
			containerPorts = append(containerPorts, corev1.ContainerPort{ContainerPort: int32(p.Container), Protocol: proto})
		}
		servicePorts = append(servicePorts, corev1.ServicePort{Name: fmt.Sprintf("%s-%d", p.Protocol, p.Host), Port: int32(p.Host), TargetPort: intstr.FromInt32(int32(p.Container)), Protocol: proto})
	}
	one, automount := int32(1), false
	// Each apply rolls the pods, as a Docker apply recreates the container: a changed value in
	// the ConfigMap or Secret reaches the process.
	pod := metav1.ObjectMeta{Labels: Labels(req, s.Name), Annotations: map[string]string{AnnotationDeployment: req.Deployment}}
	set.Deployment = &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta(name), Spec: appsv1.DeploymentSpec{
		Replicas: &one,
		Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
		Selector: &metav1.LabelSelector{MatchLabels: Selector(k.InstanceID, s.Name)},
		Template: corev1.PodTemplateSpec{ObjectMeta: pod, Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: &automount,
			Containers: []corev1.Container{{
				Name:    strings.ReplaceAll(s.Name, "_", "-"),
				Image:   s.Pull.Reference,
				Ports:   containerPorts,
				EnvFrom: envFrom,
			}},
		}},
	}}
	if len(servicePorts) > 0 {
		set.Endpoint = &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta(name), Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, Selector: Selector(k.InstanceID, s.Name), Ports: servicePorts,
		}}
	}
	return set
}
```

- [ ] **Step 4: Run the test to verify it passes, and that the server links no client-go**

Run: `gofmt -w internal/runtime && go vet ./internal/runtime/... && go test -race -count=1 ./internal/runtime/kubernetes/render/ && go list -deps ./cmd/server | grep -c k8s.io`
Expected: `ok`, then `0`.

- [ ] **Step 5: DOX and commit**

In `internal/runtime/kubernetes/AGENTS.md`: in `## Ownership`, after the first sentence, add `The \`render/\` subpackage turns a cluster deployment frame into client-go objects; only this package imports it.`; in `## Local Contracts`, append:

```markdown
- `render.Request` renders each service of a validated cluster frame as a `Set`: ConfigMap `<name>-env` (the env keys not in `SecretKeys`), Secret `<name>-secret` (`Opaque`, the secret keys; nil without any), Deployment `<name>` (one replica, `Recreate`, selector `kyyard.busnes.app/instance` + `/service`, one container named after the service with `_` as `-`, image `Pull.Reference`, container ports deduplicated, `envFrom` the ConfigMap then the Secret, `restartPolicy: Always`, `automountServiceAccountToken: false`, no probes, resources or security context) and Service `<name>` (`ClusterIP`, same selector, one port per published port named `<protocol>-<port>`, `targetPort` the target; nil without ports), names from `protocol.KubernetesNames`. Every object carries the six labels (`app.kubernetes.io/name`, `/instance`, `/managed-by: kyyard`, `kyyard.busnes.app/application`, `/instance`, `/service`) and the annotations `kyyard.busnes.app/revision`, `/deployment`, `/spec-digest`; the pod template carries the labels and the deployment annotation, so every apply rolls the pods.
```

and in `## Verification`, append `- \`go test ./internal/runtime/kubernetes/render/\` compares a two-service render with the expected objects field by field, decodes each strictly through the client-go scheme, and checks every name and label against apimachinery's validation for awkward projects and services.`

```bash
git add internal/runtime/kubernetes && make tidy-check lint && git commit -m "feat(k8s): render a service as KyYard-labelled objects" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: The cluster agent applies and removes an application

**Files:**
- Create: `internal/runtime/kubernetes/deploy.go` (`Client.Deploy`), `internal/runtime/kubernetes/remove.go` (`Client.Remove`)
- Test: `internal/runtime/kubernetes/deploy_test.go` (new)
- Modify: `internal/runtime/kubernetes/kubernetes.go` (`Client.poll`, workload labels)
- Modify: `internal/agent/client/connect.go` (`helloCapabilities`), `internal/agent/client/deployments.go` (frames held to the agent's runtime)
- Test: `internal/agent/client/capabilities_test.go` (new), `internal/agent/client/kubernetes_test.go` (the end-to-end cluster agent advertises deploy and remove)
- Modify: `internal/runtime/docker/deploy.go`, `internal/runtime/docker/remove.go` (`ValidateFor(RuntimeDocker, …)`), `cmd/agent/main.go` (wiring)
- Docs: `internal/runtime/kubernetes/AGENTS.md`, `internal/runtime/AGENTS.md`, `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: Task 1 (`ValidateFor`, `KubernetesNames`, `ValidServiceName`, identity fields, step codes), Task 4 (`render.Request`, `render.Selector`, `render.LabelInstance`, `render.LabelService`, `render.LabelApplication`); existing test helper `cluster(t, objects...)` in `kubernetes_test.go`.
- Produces:
  ```go
  // package kubernetes
  func (c *Client) Deploy(ctx context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult
  func (c *Client) Remove(ctx context.Context, req protocol.RemovalRequest, started func()) protocol.DeploymentResult
  const rolloutPoll = 2 * time.Second // Client.poll; tests shorten it
  // package client (unexported)
  func helloCapabilities(opts *Options) []string
  ```
  `cmd/agent --kubernetes` sets `Options.Deploy`/`Options.Remove` to the cluster's; the agent then advertises `kubernetes.deploy` and `kubernetes.remove`.

- [ ] **Step 1: Write the failing tests**

Create `internal/runtime/kubernetes/deploy_test.go`:

```go
package kubernetes

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testSpec     = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testApp      = "11111111-2222-4333-8444-555555555555"
	testInstance = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	testUID      = "0f1e2d3c-4b5a-4968-8776-655443322110"
)

var deploymentsResource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

// deployRequest is a two-service frame for namespace shop: web (a secret, a published port) and
// api (neither).
func deployRequest(deadline time.Duration) protocol.DeploymentRequest {
	now := time.Now()
	return protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", Revision: 1, IssuedAt: now, Deadline: now.Add(deadline),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec},
		Services: []protocol.DeploymentService{
			{Name: "web", Restart: "always", Pull: &protocol.ImagePull{Reference: "ghcr.io/org/web@" + testDigest, Digest: testDigest}, Ports: []protocol.Port{{Container: 80, Host: 8080, Protocol: "tcp"}}, Env: map[string]string{"MODE": "prod", "TOKEN": "s3cret"}, SecretKeys: []string{"TOKEN"}, Mounts: []protocol.Mount{}},
			{Name: "api", Pull: &protocol.ImagePull{Reference: "ghcr.io/org/api@" + testDigest, Digest: testDigest}, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: []protocol.Mount{}},
		}}
}

// deployCluster is a fake API server that grants the agent's access review (unless denied),
// gives each Deployment a UID and a rising generation as the API server would, and, when
// rollouts is true, reports it rolled out.
func deployCluster(t *testing.T, rollouts, denied bool, objects ...runtime.Object) (*Client, *fake.Clientset) {
	t.Helper()
	c, cs := cluster(t, objects...)
	c.poll = 10 * time.Millisecond
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		a := review.Spec.ResourceAttributes
		review.Status.Allowed = !denied && a.Namespace == "shop" && a.Verb == "create" && a.Group == "apps" && a.Resource == "deployments"
		return true, review, nil
	})
	cs.PrependReactor("*", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// CreateAction and UpdateAction have the same methods: tell them apart by verb.
		var d *appsv1.Deployment
		switch action.GetVerb() {
		case "create":
			d = action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment)
			d.UID, d.Generation = testUID, 1
		case "update":
			d = action.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment)
			old, err := cs.Tracker().Get(deploymentsResource, d.Namespace, d.Name)
			if err != nil {
				return true, nil, err
			}
			d.UID, d.Generation = old.(*appsv1.Deployment).UID, old.(*appsv1.Deployment).Generation+1
		default:
			return false, nil, nil
		}
		if rollouts {
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1}
		} else {
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"},
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable", Message: "secret-canary message text"},
			}}
		}
		return false, nil, nil
	})
	return c, cs
}

// writes lists the verbs that changed the cluster, the access review aside.
func writes(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" && a.GetResource().Resource != "selfsubjectaccessreviews" {
			out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	return out
}

func owned(service string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: "shop", Labels: map[string]string{render.LabelInstance: testInstance, render.LabelService: service}}
}

// A first apply creates every object labelled as the instance's and waits for each rollout; the
// run reports Deployment identities. A second apply updates the same objects in place, keeping
// the Service's cluster IP. started is called once, after every precondition and before the
// first write.
func TestDeployCreatesThenUpdatesOwnedObjects(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	var startedAt []int
	res := c.Deploy(context.Background(), deployRequest(time.Minute), func() { startedAt = append(startedAt, len(writes(cs))) })
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.Service+" "+s.Step+" "+s.Outcome)
	}
	want := []string{"web precondition succeeded", "api precondition succeeded", "web create succeeded", "web start succeeded", "api create succeeded", "api start succeeded"}
	if !slices.Equal(steps, want) {
		t.Fatalf("steps %v", steps)
	}
	if !slices.Equal(startedAt, []int{0}) {
		t.Fatalf("started at %v", startedAt)
	}
	if !slices.Equal(writes(cs), []string{"create configmaps", "create secrets", "create deployments", "create services", "create configmaps", "create deployments"}) {
		t.Fatalf("writes %v", writes(cs))
	}
	if len(res.Services) != 2 || res.Services[0] != (protocol.DeploymentIdentity{Service: "web", Kind: protocol.KindDeployment, Namespace: "shop", Name: "shop-web", UID: testUID, Generation: 1, ImageDigest: testDigest}) {
		t.Fatalf("identities %+v", res.Services)
	}
	secret, err := cs.CoreV1().Secrets("shop").Get(context.Background(), "shop-web-secret", metav1.GetOptions{})
	if err != nil || string(secret.Data["TOKEN"]) != "s3cret" || secret.Labels[render.LabelInstance] != testInstance {
		t.Fatalf("secret %+v %v", secret, err)
	}
	svc, _ := cs.CoreV1().Services("shop").Get(context.Background(), "shop-web", metav1.GetOptions{})
	svc.Spec.ClusterIP, svc.Spec.ClusterIPs = "10.96.0.7", []string{"10.96.0.7"}
	if _, err := cs.CoreV1().Services("shop").Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	again := deployRequest(time.Minute)
	again.Deployment, again.Revision = "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b", 2
	again.Services[0].Env["MODE"] = "staging"
	cs.ClearActions()
	res = c.Deploy(context.Background(), again, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Services[0].Generation != 2 {
		t.Fatalf("update %+v", res)
	}
	if !slices.Equal(writes(cs), []string{"update configmaps", "update secrets", "update deployments", "update services", "update configmaps", "update deployments"}) {
		t.Fatalf("writes %v", writes(cs))
	}
	cm, _ := cs.CoreV1().ConfigMaps("shop").Get(context.Background(), "shop-web-env", metav1.GetOptions{})
	svc, _ = cs.CoreV1().Services("shop").Get(context.Background(), "shop-web", metav1.GetOptions{})
	if cm.Data["MODE"] != "staging" || cm.Annotations[render.AnnotationRevision] != "2" || svc.Spec.ClusterIP != "10.96.0.7" {
		t.Fatalf("updated %+v %+v", cm, svc.Spec)
	}
}

// An object under a planned name that is not this instance's stops the run before any write,
// naming its kind and name; so does a grant the agent does not hold.
func TestDeployRefusesBeforeWriting(t *testing.T) {
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-api", Labels: map[string]string{"app": "api"}}}
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-web-secret", Labels: map[string]string{render.LabelInstance: "99999999-7777-4888-9999-aaaaaaaaaaaa"}}}
	for name, tc := range map[string]struct {
		objects      []runtime.Object
		denied       bool
		service      string
		code, detail string
	}{
		"unlabelled Deployment":     {[]runtime.Object{foreign}, false, "api", "name_taken", "Deployment/shop-api"},
		"another instance's Secret": {[]runtime.Object{other}, false, "web", "name_taken", "Secret/shop-web-secret"},
		"no grant":                  {nil, true, "web", "forbidden", ""},
	} {
		t.Run(name, func(t *testing.T) {
			c, cs := deployCluster(t, true, tc.denied, tc.objects...)
			started := false
			res := c.Deploy(context.Background(), deployRequest(time.Minute), func() { started = true })
			failing := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Outcome == protocol.OutcomeDenied })
			if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultStepFailed || failing < 0 || res.Validate() != nil {
				t.Fatalf("result %+v", res)
			}
			s := res.Steps[failing]
			if s.Service != tc.service || s.Step != protocol.StepPrecondition || s.Code != tc.code || s.Detail != tc.detail {
				t.Fatalf("step %+v", s)
			}
			if started || len(writes(cs)) != 0 || len(res.Services) != 0 {
				t.Fatalf("wrote %v, started %v", writes(cs), started)
			}
		})
	}
}

// One conflict is re-read and retried; a second is a failed create naming the object.
func TestDeployRetriesOneConflict(t *testing.T) {
	for _, conflicts := range []int{1, 2} {
		existing := &corev1.ConfigMap{ObjectMeta: owned("web")}
		existing.Name = "shop-web-env"
		c, cs := deployCluster(t, true, false, existing)
		left := conflicts
		cs.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			if left == 0 {
				return false, nil, nil
			}
			left--
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "shop-web-env", nil)
		})
		res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
		if conflicts == 1 && res.Outcome != protocol.OutcomeSucceeded {
			t.Fatalf("one conflict: %+v", res)
		}
		if conflicts == 2 {
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			if res.Outcome != protocol.OutcomeFailed || i < 0 || res.Steps[i].Step != protocol.StepCreate || res.Steps[i].Code != "conflict" || res.Steps[i].Detail != "ConfigMap/shop-web-env" || res.Validate() != nil {
				t.Fatalf("two conflicts: %+v", res)
			}
		}
	}
}

// A rollout that does not finish by the deadline times out with the reasons the cluster gives,
// in the closed shape only; the objects stay applied and the identity is still reported.
func TestDeployRolloutTimeout(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: owned("web"), Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "web", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "secret-canary"}}}}}}
	pod.Name = "shop-web-abc"
	c, cs := deployCluster(t, false, false, pod)
	res := c.Deploy(context.Background(), deployRequest(300*time.Millisecond), func() {})
	i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
	if res.Outcome != protocol.OutcomeTimedOut || i < 0 || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	s := res.Steps[i]
	if s.Service != "web" || s.Step != protocol.StepStart || s.Code != "rollout_timeout" || s.Detail != "progressing=ProgressDeadlineExceeded,available=MinimumReplicasUnavailable,pod=ImagePullBackOff" {
		t.Fatalf("step %+v", s)
	}
	if len(res.Services) != 1 || res.Services[0].Name != "shop-web" {
		t.Fatalf("identities %+v", res.Services)
	}
	if _, err := cs.AppsV1().Deployments("shop").Get(context.Background(), "shop-web", metav1.GetOptions{}); err != nil {
		t.Fatalf("the applied Deployment was rolled back: %v", err)
	}
	for _, step := range res.Steps[i+1:] {
		if step.Outcome != protocol.OutcomeSkipped {
			t.Fatalf("after the timeout: %+v", step)
		}
	}
}

// A Docker frame, or a cluster frame that fails validation, is refused with nothing read.
func TestDeployRefusesAnInvalidFrame(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	docker := deployRequest(time.Minute)
	docker.Kubernetes = nil
	res := c.Deploy(context.Background(), docker, func() { t.Fatal("started") })
	if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || len(cs.Actions()) != 0 {
		t.Fatalf("docker frame: %+v %v", res, cs.Actions())
	}
}

// Removal deletes, in the foreground, what carries the instance's label, including a service
// no longer in the definition, and each named Secret that is the instance's; a Secret under a
// planned name without the label, and anything unlabelled, stays. A service with nothing left
// is skipped.
func TestRemoveDeletesOnlyTheInstancesObjects(t *testing.T) {
	obj := func(name, service string) metav1.ObjectMeta {
		m := owned(service)
		m.Name = name
		return m
	}
	objects := []runtime.Object{
		&appsv1.Deployment{ObjectMeta: obj("shop-web", "web")},
		&corev1.Service{ObjectMeta: obj("shop-web", "web")},
		&corev1.ConfigMap{ObjectMeta: obj("shop-web-env", "web")},
		&corev1.Secret{ObjectMeta: obj("shop-web-secret", "web")},
		&appsv1.Deployment{ObjectMeta: obj("shop-old", "old")},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-api-env"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-api-secret"}},
	}
	c, cs := deployCluster(t, true, false, objects...)
	now := time.Now()
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: []string{"api", "web"}}
	started := 0
	res := c.Remove(context.Background(), req, func() { started++ })
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil || started != 1 {
		t.Fatalf("result %+v", res)
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.Service+" "+s.Step+" "+s.Outcome)
	}
	if !slices.Equal(steps, []string{"api precondition succeeded", "api remove skipped", "old precondition succeeded", "old remove succeeded", "web precondition succeeded", "web remove succeeded"}) {
		t.Fatalf("steps %v", steps)
	}
	var deleted []string
	for _, a := range cs.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok {
			deleted = append(deleted, a.GetResource().Resource+"/"+d.GetName())
			if p := d.GetDeleteOptions().PropagationPolicy; p == nil || *p != metav1.DeletePropagationForeground {
				t.Fatalf("%s deleted without foreground propagation", d.GetName())
			}
		}
	}
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"configmaps/shop-web-env", "deployments/shop-old", "deployments/shop-web", "secrets/shop-web-secret", "services/shop-web"}) {
		t.Fatalf("deleted %v", deleted)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "secrets" {
			t.Fatal("secrets were listed")
		}
	}
}

// A removal whose objects are already gone succeeds without writing anything.
func TestRemoveOfNothingSkips(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	now := time.Now()
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: []string{"web"}}
	res := c.Remove(context.Background(), req, func() { t.Fatal("started with nothing to delete") })
	if res.Outcome != protocol.OutcomeSucceeded || len(res.Steps) != 2 || res.Steps[1].Outcome != protocol.OutcomeSkipped || len(writes(cs)) != 0 {
		t.Fatalf("result %+v writes %v", res, writes(cs))
	}
}

// The inventory carries KyYard's labels on the Deployments it applied, and nothing for others.
func TestSnapshotReadsKyYardLabels(t *testing.T) {
	ours := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-web", Labels: map[string]string{render.LabelApplication: testApp, render.LabelInstance: testInstance}}}
	theirs := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "other"}}
	c, _ := cluster(t, ours, theirs)
	snap, _ := c.Snapshot(context.Background())
	byName := map[string]protocol.Workload{}
	for _, w := range snap.Kubernetes.Workloads {
		byName[w.Name] = w
	}
	if byName["shop-web"].Application != testApp || byName["shop-web"].Instance != testInstance || byName["other"].Instance != "" {
		t.Fatalf("workloads %+v", snap.Kubernetes.Workloads)
	}
}
```

Create `internal/agent/client/capabilities_test.go`:

```go
package client

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// A cluster agent advertises cluster capabilities only, deploy and remove included when it can;
// a Docker agent keeps deployment.apply, deployment.pull and deployment.remove.
func TestHelloCapabilitiesPerRuntime(t *testing.T) {
	deploy := func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{}
	}
	remove := func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{}
	}
	logs := func(context.Context, protocol.LogRequest, func([]byte) error) error { return nil }
	cluster := helloCapabilities(&Options{Kubernetes: true, Logs: logs, Deploy: deploy, Remove: remove})
	if !slices.Equal(cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs, protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesRemove}) || !protocol.CapabilitiesFit(protocol.RuntimeKubernetes, cluster) {
		t.Fatalf("cluster %v", cluster)
	}
	if readOnly := helloCapabilities(&Options{Kubernetes: true, Logs: logs}); !slices.Equal(readOnly, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs}) {
		t.Fatalf("read-only cluster %v", readOnly)
	}
	docker := helloCapabilities(&Options{Deploy: deploy, Remove: remove})
	if !slices.Equal(docker, []string{protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull, protocol.CapabilityDeploymentRemove}) || !protocol.CapabilitiesFit(protocol.RuntimeDocker, docker) {
		t.Fatalf("docker %v", docker)
	}
}

// A frame for the other runtime is refused invalid_request and never reaches the runtime.
func TestDeployerRefusesTheOtherRuntimesFrame(t *testing.T) {
	ran := false
	deploy := func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		ran = true
		return protocol.DeploymentResult{}
	}
	remove := func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult {
		ran = true
		return protocol.DeploymentResult{}
	}
	cluster := newDeployer(context.Background(), t.TempDir(), &Options{Kubernetes: true, Deploy: deploy, Remove: remove})
	out := make(chan outFrame, 4)
	defer cluster.attach(context.Background(), out)()
	raw, _ := json.Marshal(testRequest("ep_1"))
	cluster.handleApply(context.Background(), "ep_1", raw, out)
	raw, _ = json.Marshal(testRemoval("ep_1"))
	cluster.handleRemoval(context.Background(), "ep_1", raw, out)
	for range 2 {
		var res protocol.DeploymentResult
		if err := decodeResult(nextFrame(t, out), &res); err != nil || res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest {
			t.Fatalf("docker frame to a cluster agent: %+v %v", res, err)
		}
	}
	host := newDeployer(context.Background(), t.TempDir(), &Options{Deploy: deploy})
	defer host.attach(context.Background(), out)()
	req := testRequest("ep_1")
	req.Kubernetes = &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: "11111111-2222-4333-8444-555555555555", InstanceID: "66666666-7777-4888-9999-aaaaaaaaaaaa", SpecDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	raw, _ = json.Marshal(req)
	host.handleApply(context.Background(), "ep_1", raw, out)
	var res protocol.DeploymentResult
	if err := decodeResult(nextFrame(t, out), &res); err != nil || res.Code != protocol.ResultInvalidRequest {
		t.Fatalf("cluster frame to a Docker agent: %+v %v", res, err)
	}
	if ran {
		t.Fatal("a frame for the other runtime ran")
	}
}
```

In `internal/agent/client/kubernetes_test.go`:

Replace

```go
	}
	logs := func(context.Context, protocol.LogRequest, func([]byte) error) error { return nil }
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx, id, client.Options{HTTPClient: httpClient, Identities: identities, Kubernetes: true, Snapshot: snapshot, Logs: logs, InventoryEvery: time.Second})
	}()
	waitState(t, ctx, ts, id.EndpointID, "active")
```

with

```go
	}
	logs := func(context.Context, protocol.LogRequest, func([]byte) error) error { return nil }
	deploy := func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{}
	}
	remove := func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{}
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx, id, client.Options{HTTPClient: httpClient, Identities: identities, Kubernetes: true, Snapshot: snapshot, Logs: logs, Deploy: deploy, Remove: remove, InventoryEvery: time.Second})
	}()
	waitState(t, ctx, ts, id.EndpointID, "active")
```

Replace

```go
	for {
		ep, err := ts.ReadEndpoint(ctx, access, id.EndpointID)
		if err == nil && slices.Equal(ep.Capabilities, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs}) {
			break
		}
```

with

```go
	for {
		ep, err := ts.ReadEndpoint(ctx, access, id.EndpointID)
		// The server stores them sorted; a hello it refused would store none.
		if err == nil && slices.Equal(ep.Capabilities, []string{protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesRemove, protocol.CapabilityPodLogs}) {
			break
		}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runtime/kubernetes/ ./internal/agent/client/`
Expected: FAIL to compile: `c.poll undefined`, `c.Deploy undefined`, `c.Remove undefined`, `undefined: helloCapabilities`.

- [ ] **Step 3: Implement**

Create `internal/runtime/kubernetes/deploy.go`:

```go
package kubernetes

import (
	"cmp"
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// rolloutPoll is how often a start step reads its Deployment while waiting for the rollout.
const rolloutPoll = 2 * time.Second

// Deploy applies each service's objects and waits for its rollout. First every service's
// precondition: the agent's own grant (a SelfSubjectAccessReview for create deployments in the
// namespace, forbidden when denied), then each planned object read by name, refused name_taken
// when one exists without this instance's label. Then per service create (ConfigMap, Secret,
// Deployment, Service, each updated when it exists and is owned, created otherwise; one conflict
// is re-read and retried) and start (the rollout, polled until ready or the deadline:
// rollout_timeout). The first step that is not a success ends the run and every later step is
// skipped. Nothing is rolled back: a timed-out rollout leaves the objects as applied. started is
// called once, before the first write.
func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		return refused(res, err)
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	r := &run{c: c, parent: parent, res: res, started: started, namespace: req.Kubernetes.Namespace, instance: req.Kubernetes.InstanceID}
	sets := render.Request(req)
	have := make([]existing, len(sets))
	for i, set := range sets {
		r.step(set.Service, protocol.StepPrecondition, func() (string, string, string) {
			if i == 0 {
				if o, code, detail := r.allowed(ctx); o != protocol.OutcomeSucceeded {
					return o, code, detail
				}
			}
			var o, code, detail string
			have[i], o, code, detail = r.read(ctx, set)
			return o, code, detail
		})
	}
	for i, set := range sets {
		r.step(set.Service, protocol.StepCreate, func() (string, string, string) {
			r.begin()
			d, o, code, detail := r.apply(ctx, set, have[i])
			if o == protocol.OutcomeSucceeded {
				r.res.Services = append(r.res.Services, protocol.DeploymentIdentity{Service: set.Service, Kind: protocol.KindDeployment, Namespace: d.Namespace, Name: d.Name, UID: string(d.UID), Generation: d.Generation, ImageDigest: req.Services[i].Pull.Digest})
			}
			return o, code, detail
		})
		r.step(set.Service, protocol.StepStart, func() (string, string, string) { return r.rollout(ctx, set) })
	}
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

type run struct {
	c         *Client
	parent    context.Context
	res       protocol.DeploymentResult
	started   func()
	begun     bool
	namespace string
	instance  string
}

// begin tells the caller, once, that the run is about to write to the cluster.
func (r *run) begin() {
	if !r.begun {
		r.begun = true
		r.started()
	}
}

// step records one outcome; the first that is not a success fixes the run's outcome, coded
// step_failed, and every later step is skipped.
func (r *run) step(service, step string, do func() (outcome, code, detail string)) {
	if r.res.Outcome != "" {
		r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: step, Outcome: protocol.OutcomeSkipped})
		return
	}
	outcome, code, detail := do()
	s := protocol.DeploymentStep{Service: service, Step: step, Outcome: outcome}
	if outcome != protocol.OutcomeSucceeded && outcome != protocol.OutcomeSkipped {
		s.Code, s.Detail = code, detail
		r.res.Outcome, r.res.Code = outcome, protocol.ResultStepFailed
	}
	r.res.Steps = append(r.res.Steps, s)
}

// refused answers a frame ValidateFor refused, with no step run.
func refused(res protocol.DeploymentResult, err error) protocol.DeploymentResult {
	if !protocol.ValidRequestID(res.RequestID) {
		res.RequestID = ""
	}
	res.Outcome, res.Code = protocol.OutcomeDenied, protocol.ResultInvalidRequest
	if errors.Is(err, protocol.ErrClockSkew) {
		res.Outcome, res.Code = protocol.OutcomeFailed, protocol.ResultClockSkew
	}
	return res
}

func succeeded() (string, string, string) { return protocol.OutcomeSucceeded, "", "" }

// failure classifies an API call that did not succeed. A status is an answer; with none, a
// cancelled parent means the agent is stopping and the API server may have acted.
func (r *run) failure(ctx context.Context, err error) (string, string, string) {
	var status apierrors.APIStatus
	switch {
	case apierrors.IsForbidden(err):
		return protocol.OutcomeDenied, "forbidden", ""
	case errors.As(err, &status) && status.Status().Code >= 100 && status.Status().Code <= 599:
		return protocol.OutcomeFailed, "runtime_status", strconv.Itoa(int(status.Status().Code))
	case errors.Is(r.parent.Err(), context.Canceled):
		return protocol.OutcomeUnknown, "cancelled", ""
	case ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "runtime_timeout", ""
	}
	return protocol.OutcomeFailed, "runtime_error", ""
}

// allowed asks the API server whether the agent may create Deployments in the namespace: the
// grant, not the namespace list the server stores, decides.
func (r *run) allowed(ctx context.Context) (string, string, string) {
	review, err := r.c.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: r.namespace, Verb: "create", Group: "apps", Resource: "deployments"},
	}}, metav1.CreateOptions{})
	if err != nil {
		return r.failure(ctx, err)
	}
	if !review.Status.Allowed {
		return protocol.OutcomeDenied, "forbidden", ""
	}
	return succeeded()
}

// existing is what a precondition found under a service's planned names: nil where nothing is.
type existing struct {
	configMap  *corev1.ConfigMap
	secret     *corev1.Secret
	deployment *appsv1.Deployment
	service    *corev1.Service
}

func (r *run) owned(o metav1.Object) bool { return o.GetLabels()[render.LabelInstance] == r.instance }

func nameTaken(kind, name string) (string, string, string) {
	return protocol.OutcomeDenied, "name_taken", kind + "/" + name
}

// objectAPI is the part of a typed client a service's objects are applied through.
type objectAPI[T any] interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (T, error)
	Create(ctx context.Context, obj T, opts metav1.CreateOptions) (T, error)
	Update(ctx context.Context, obj T, opts metav1.UpdateOptions) (T, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// get reads one object by name: found false when it does not exist.
func get[T metav1.Object](ctx context.Context, api objectAPI[T], name string) (T, bool, error) {
	o, err := api.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		var zero T
		return zero, false, nil
	}
	return o, err == nil, err
}

// read is a service's precondition: each planned object, owned or absent.
func (r *run) read(ctx context.Context, set render.Set) (existing, string, string, string) {
	var e existing
	core, apps := r.c.cs.CoreV1(), r.c.cs.AppsV1()
	check := func(kind, name string, o metav1.Object, found bool, err error) (string, string, string) {
		switch {
		case err != nil:
			return r.failure(ctx, err)
		case found && !r.owned(o):
			return nameTaken(kind, name)
		}
		return succeeded()
	}
	cm, found, err := get(ctx, objectAPI[*corev1.ConfigMap](core.ConfigMaps(r.namespace)), set.ConfigMap.Name)
	if o, code, detail := check("ConfigMap", set.ConfigMap.Name, cm, found, err); o != protocol.OutcomeSucceeded {
		return e, o, code, detail
	} else if found {
		e.configMap = cm
	}
	if set.Secret != nil {
		s, found, err := get(ctx, objectAPI[*corev1.Secret](core.Secrets(r.namespace)), set.Secret.Name)
		if o, code, detail := check("Secret", set.Secret.Name, s, found, err); o != protocol.OutcomeSucceeded {
			return e, o, code, detail
		} else if found {
			e.secret = s
		}
	}
	d, found, err := get(ctx, objectAPI[*appsv1.Deployment](apps.Deployments(r.namespace)), set.Deployment.Name)
	if o, code, detail := check("Deployment", set.Deployment.Name, d, found, err); o != protocol.OutcomeSucceeded {
		return e, o, code, detail
	} else if found {
		e.deployment = d
	}
	if set.Endpoint != nil {
		s, found, err := get(ctx, objectAPI[*corev1.Service](core.Services(r.namespace)), set.Endpoint.Name)
		if o, code, detail := check("Service", set.Endpoint.Name, s, found, err); o != protocol.OutcomeSucceeded {
			return e, o, code, detail
		} else if found {
			e.service = s
		}
	}
	o, code, detail := succeeded()
	return e, o, code, detail
}

// apply writes a service's objects in order and returns the Deployment as written.
func (r *run) apply(ctx context.Context, set render.Set, e existing) (*appsv1.Deployment, string, string, string) {
	core, apps := r.c.cs.CoreV1(), r.c.cs.AppsV1()
	if _, o, code, detail := upsert(ctx, r, "ConfigMap", core.ConfigMaps(r.namespace), set.ConfigMap, e.configMap, nil); o != protocol.OutcomeSucceeded {
		return nil, o, code, detail
	}
	if set.Secret != nil {
		if _, o, code, detail := upsert(ctx, r, "Secret", core.Secrets(r.namespace), set.Secret, e.secret, nil); o != protocol.OutcomeSucceeded {
			return nil, o, code, detail
		}
	}
	d, o, code, detail := upsert(ctx, r, "Deployment", apps.Deployments(r.namespace), set.Deployment, e.deployment, nil)
	if o != protocol.OutcomeSucceeded {
		return nil, o, code, detail
	}
	if set.Endpoint != nil {
		// A Service's cluster IP is immutable: an update keeps the one it was given.
		keep := func(want, have *corev1.Service) {
			want.Spec.ClusterIP, want.Spec.ClusterIPs, want.Spec.IPFamilies, want.Spec.IPFamilyPolicy = have.Spec.ClusterIP, have.Spec.ClusterIPs, have.Spec.IPFamilies, have.Spec.IPFamilyPolicy
		}
		if _, o, code, detail := upsert(ctx, r, "Service", core.Services(r.namespace), set.Endpoint, e.service, keep); o != protocol.OutcomeSucceeded {
			return nil, o, code, detail
		}
	}
	return d, protocol.OutcomeSucceeded, "", ""
}

// upsert updates want over have, an owned object read at the precondition (nil: create it),
// keeping have's resourceVersion. A conflict, or an object that appeared since, is re-read once:
// still owned, it is written again; a second conflict fails with conflict.
func upsert[T interface {
	metav1.Object
	comparable
}](ctx context.Context, r *run, kind string, api objectAPI[T], want, have T, keep func(want, have T)) (T, string, string, string) {
	var zero T
	for attempt := 0; ; attempt++ {
		var got T
		var err error
		if have == zero {
			want.SetResourceVersion("")
			got, err = api.Create(ctx, want, metav1.CreateOptions{})
		} else {
			want.SetResourceVersion(have.GetResourceVersion())
			if keep != nil {
				keep(want, have)
			}
			got, err = api.Update(ctx, want, metav1.UpdateOptions{})
		}
		if err == nil {
			return got, protocol.OutcomeSucceeded, "", ""
		}
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			o, code, detail := r.failure(ctx, err)
			return zero, o, code, detail
		}
		if attempt == 1 {
			return zero, protocol.OutcomeFailed, "conflict", kind + "/" + want.GetName()
		}
		fresh, found, err := get(ctx, api, want.GetName())
		switch {
		case err != nil:
			o, code, detail := r.failure(ctx, err)
			return zero, o, code, detail
		case !found:
			have = zero
		case !r.owned(fresh):
			o, code, detail := nameTaken(kind, want.GetName())
			return zero, o, code, detail
		default:
			have = fresh
		}
	}
}

// rollout waits until the Deployment's controller has seen the latest generation and its one
// replica is updated and ready, reading it every poll within the request's deadline.
func (r *run) rollout(ctx context.Context, set render.Set) (string, string, string) {
	api := r.c.cs.AppsV1().Deployments(r.namespace)
	var last *appsv1.Deployment
	for {
		d, err := api.Get(ctx, set.Deployment.Name, metav1.GetOptions{})
		switch {
		case err == nil:
			last = d
			if ready(d) {
				return succeeded()
			}
		case ctx.Err() == nil:
			return r.failure(ctx, err)
		}
		select {
		case <-ctx.Done():
			if errors.Is(r.parent.Err(), context.Canceled) {
				return protocol.OutcomeUnknown, "cancelled", ""
			}
			return protocol.OutcomeTimedOut, "rollout_timeout", r.stalled(set, last)
		case <-time.After(r.c.poll):
		}
	}
}

func ready(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation && s.Replicas == want && s.UpdatedReplicas == want && s.ReadyReplicas == want
}

// reasonWord is the only shape of reason a rollout_timeout detail carries: a Kubernetes reason
// is a CamelCase word, and anything else is dropped rather than passed on.
var reasonWord = regexp.MustCompile(`^[A-Za-z]{1,64}$`)

// stalled says why a rollout did not finish: the Deployment's Progressing reason, its Available
// reason when unavailable, and the newest pod's waiting reason, as far as each can be read in
// a few seconds past the deadline.
func (r *run) stalled(set render.Set, d *appsv1.Deployment) string {
	var parts []string
	if d != nil {
		for _, c := range d.Status.Conditions {
			switch {
			case c.Type == appsv1.DeploymentProgressing && reasonWord.MatchString(c.Reason):
				parts = append(parts, "progressing="+c.Reason)
			case c.Type == appsv1.DeploymentAvailable && c.Status != corev1.ConditionTrue && reasonWord.MatchString(c.Reason):
				parts = append(parts, "available="+c.Reason)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.parent), 5*time.Second)
	defer cancel()
	pods, err := r.c.cs.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(render.Selector(r.instance, set.Service)).String()})
	if err == nil && len(pods.Items) > 0 {
		newest := slices.MaxFunc(pods.Items, func(a, b corev1.Pod) int {
			return cmp.Compare(a.CreationTimestamp.UnixNano(), b.CreationTimestamp.UnixNano())
		})
		for _, s := range newest.Status.ContainerStatuses {
			if s.State.Waiting != nil && reasonWord.MatchString(s.State.Waiting.Reason) {
				parts = append(parts, "pod="+s.State.Waiting.Reason)
				break
			}
		}
	}
	return strings.Join(parts[:min(len(parts), 3)], ",")
}
```

Create `internal/runtime/kubernetes/remove.go`:

```go
package kubernetes

import (
	"context"
	"slices"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// doomed is one object a removal deletes.
type doomed struct {
	kind, name string
	remove     func(context.Context, string, metav1.DeleteOptions) error
}

// Remove deletes the instance's objects: the Deployments, Services and ConfigMaps in the
// namespace carrying its instance label, and each named service's Secret when it carries the
// label too. An object without it is never touched. The first service's precondition does the
// reads; each service then has a remove step, skipped when nothing of it is left. Deletes are
// foreground, so a Deployment's pods go with it.
func (c *Client) Remove(parent context.Context, req protocol.RemovalRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		return refused(res, err)
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	r := &run{c: c, parent: parent, res: res, started: started, namespace: req.Kubernetes.Namespace, instance: req.Kubernetes.InstanceID}
	found := map[string][]doomed{}
	services := slices.Clone(req.Services)
	r.step(services[0], protocol.StepPrecondition, func() (string, string, string) {
		o, code, detail := r.find(ctx, req, found)
		for s := range found {
			if !slices.Contains(services, s) {
				services = append(services, s)
			}
		}
		slices.Sort(services[1:])
		return o, code, detail
	})
	for i, s := range services {
		if i > 0 {
			r.step(s, protocol.StepPrecondition, succeeded)
		}
		if len(found[s]) == 0 {
			r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: s, Step: protocol.StepRemove, Outcome: protocol.OutcomeSkipped})
			continue
		}
		r.step(s, protocol.StepRemove, func() (string, string, string) {
			r.begin()
			foreground := metav1.DeletePropagationForeground
			for _, d := range found[s] {
				if err := d.remove(ctx, d.name, metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
					return r.failure(ctx, err)
				}
			}
			return succeeded()
		})
	}
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

// find collects, by service label, every object of the instance, and each named service's
// Secret by its name when it is the instance's.
func (r *run) find(ctx context.Context, req protocol.RemovalRequest, found map[string][]doomed) (string, string, string) {
	core, apps := r.c.cs.CoreV1(), r.c.cs.AppsV1()
	selector := metav1.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{render.LabelInstance: r.instance}).String()}
	add := func(kind string, o metav1.Object, remove func(context.Context, string, metav1.DeleteOptions) error) {
		if s := o.GetLabels()[render.LabelService]; protocol.ValidServiceName(s) {
			found[s] = append(found[s], doomed{kind, o.GetName(), remove})
		}
	}
	deployments, err := apps.Deployments(r.namespace).List(ctx, selector)
	if err != nil {
		return r.failure(ctx, err)
	}
	for i := range deployments.Items {
		add("Deployment", &deployments.Items[i], apps.Deployments(r.namespace).Delete)
	}
	svcs, err := core.Services(r.namespace).List(ctx, selector)
	if err != nil {
		return r.failure(ctx, err)
	}
	for i := range svcs.Items {
		add("Service", &svcs.Items[i], core.Services(r.namespace).Delete)
	}
	configs, err := core.ConfigMaps(r.namespace).List(ctx, selector)
	if err != nil {
		return r.failure(ctx, err)
	}
	for i := range configs.Items {
		add("ConfigMap", &configs.Items[i], core.ConfigMaps(r.namespace).Delete)
	}
	// The role grants no list on Secrets: each is read by the name its service gives it.
	names := protocol.KubernetesNames(req.Project, req.Services)
	for _, s := range req.Services {
		secret, ok, err := get(ctx, objectAPI[*corev1.Secret](core.Secrets(r.namespace)), names[s]+"-secret")
		if err != nil {
			return r.failure(ctx, err)
		}
		if ok && r.owned(secret) {
			add("Secret", secret, core.Secrets(r.namespace).Delete)
		}
	}
	return succeeded()
}
```

In `internal/runtime/kubernetes/kubernetes.go`:

Replace

```go
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
```

with

```go
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
```

Replace

```go
type Client struct {
	cs k8s.Interface
	// openLog streams one container's log. Tests replace it: the fake clientset answers every
	// log request with the same text and ignores its options.
```

with

```go
type Client struct {
	cs k8s.Interface
	// poll is how often a rollout is read; tests shorten it.
	poll time.Duration
	// openLog streams one container's log. Tests replace it: the fake clientset answers every
	// log request with the same text and ignores its options.
```

Replace

```go
// NewFromClientset is for tests: cs is usually k8s.io/client-go/kubernetes/fake.
func NewFromClientset(cs k8s.Interface) *Client {
	return &Client{cs: cs, log: log.Default(), openLog: func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		return cs.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
	}}
```

with

```go
// NewFromClientset is for tests: cs is usually k8s.io/client-go/kubernetes/fake.
func NewFromClientset(cs k8s.Interface) *Client {
	return &Client{cs: cs, log: log.Default(), poll: rolloutPoll, openLog: func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		return cs.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
	}}
```

Replace

```go
func namespace(ns corev1.Namespace) string { return ns.Name }

func deployment(d appsv1.Deployment) protocol.Workload {
	return protocol.Workload{Kind: "Deployment", Namespace: d.Namespace, Name: d.Name, Desired: replicas(d.Spec.Replicas), Ready: d.Status.ReadyReplicas, Updated: d.Status.UpdatedReplicas, Images: images(d.Spec.Template.Spec), Paused: d.Spec.Paused}
}
```

with

```go
func namespace(ns corev1.Namespace) string { return ns.Name }

// deployment carries KyYard's application and instance labels, so the server can tell the
// Deployments it applied from everything else in the cluster.
func deployment(d appsv1.Deployment) protocol.Workload {
	return protocol.Workload{Kind: "Deployment", Namespace: d.Namespace, Name: d.Name, Desired: replicas(d.Spec.Replicas), Ready: d.Status.ReadyReplicas, Updated: d.Status.UpdatedReplicas, Images: images(d.Spec.Template.Spec), Paused: d.Spec.Paused,
		Application: d.Labels[render.LabelApplication], Instance: d.Labels[render.LabelInstance]}
}
```

In `internal/agent/client/connect.go`:

Replace

```go
}

// checkServerOrigin admits https anywhere and http only to loopback: enrollment carries the
// single-use token and every connection carries the identity, so neither may cross a network
```

with

```go
}

// helloCapabilities is what the agent advertises for its runtime. A cluster agent names only
// cluster capabilities: kubernetes.deploy and kubernetes.remove in place of deployment.apply and
// deployment.remove, and never deployment.pull, because the kubelet pulls.
func helloCapabilities(opts *Options) []string {
	capabilities := []string{}
	if opts.Kubernetes {
		capabilities = append(capabilities, protocol.CapabilityKubernetesInventory)
		if opts.Logs != nil {
			capabilities = append(capabilities, protocol.CapabilityPodLogs)
		}
		if opts.Deploy != nil {
			capabilities = append(capabilities, protocol.CapabilityKubernetesDeploy)
		}
		if opts.Remove != nil {
			capabilities = append(capabilities, protocol.CapabilityKubernetesRemove)
		}
		return capabilities
	}
	if opts.Inspect != nil {
		capabilities = append(capabilities, protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict, protocol.CapabilityContainerInspectHealth)
	}
	if opts.Exec != nil {
		capabilities = append(capabilities, "container.exec")
	}
	if opts.Deploy != nil {
		capabilities = append(capabilities, protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull)
	}
	if opts.Remove != nil {
		capabilities = append(capabilities, protocol.CapabilityDeploymentRemove)
	}
	return capabilities
}

// checkServerOrigin admits https anywhere and http only to loopback: enrollment carries the
// single-use token and every connection carries the identity, so neither may cross a network
```

Replace

```go
		heartbeat = 30 * time.Second
	}
	capabilities := []string{}
	if opts.Kubernetes {
		capabilities = append(capabilities, protocol.CapabilityKubernetesInventory)
		if opts.Logs != nil {
			capabilities = append(capabilities, protocol.CapabilityPodLogs)
		}
	}
	if opts.Inspect != nil {
		capabilities = append(capabilities, protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict, protocol.CapabilityContainerInspectHealth)
	}
	if opts.Exec != nil {
		capabilities = append(capabilities, "container.exec")
	}
	if opts.Deploy != nil {
		capabilities = append(capabilities, protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull)
	}
	if opts.Remove != nil {
		capabilities = append(capabilities, protocol.CapabilityDeploymentRemove)
	}
	if err := write(ctx, conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities, AgentVersion: opts.Version}); err != nil {
		return err
	}
```

with

```go
		heartbeat = 30 * time.Second
	}
	if err := write(ctx, conn, protocol.TypeHello, protocol.Hello{Capabilities: helloCapabilities(opts), AgentVersion: opts.Version}); err != nil {
		return err
	}
```

In `internal/agent/client/deployments.go`:

Replace

```go
			return res
		}
	}
	d.run(sessionCtx, out, req.Deployment, req.RequestID, req.Endpoint, endpointID, req.Validate, exec)
}

// handleRemoval answers one deployment.remove payload; see run. See handleApply for the
```

with

```go
			return res
		}
	}
	d.run(sessionCtx, out, req.Deployment, req.RequestID, req.Endpoint, endpointID, func(now time.Time) error { return req.ValidateFor(d.runtime(), now) }, exec)
}

// runtime is the agent's: a frame for the other one is invalid_request, never run.
func (d *deployer) runtime() string {
	if d.opts.Kubernetes {
		return protocol.RuntimeKubernetes
	}
	return protocol.RuntimeDocker
}

// handleRemoval answers one deployment.remove payload; see run. See handleApply for the
```

Replace

```go
			return remove(ctx, req, func() { d.begin(req.Deployment, req.RequestID) })
		}
	}
	d.run(sessionCtx, out, req.Deployment, req.RequestID, req.Endpoint, endpointID, req.Validate, exec)
}

// run gates and runs one decoded request in the agent's single deployment slot. Refusals are
```

with

```go
			return remove(ctx, req, func() { d.begin(req.Deployment, req.RequestID) })
		}
	}
	d.run(sessionCtx, out, req.Deployment, req.RequestID, req.Endpoint, endpointID, func(now time.Time) error { return req.ValidateFor(d.runtime(), now) }, exec)
}

// run gates and runs one decoded request in the agent's single deployment slot. Refusals are
```

In `internal/runtime/docker/deploy.go`:

Replace

```go
func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.Validate(time.Now()); err != nil {
		return refused(res, err)
	}
```

with

```go
func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.ValidateFor(protocol.RuntimeDocker, time.Now()); err != nil {
		return refused(res, err)
	}
```

In `internal/runtime/docker/remove.go`:

Replace

```go
func (c *Client) Remove(parent context.Context, req protocol.RemovalRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.Validate(time.Now()); err != nil {
		return refused(res, err)
	}
```

with

```go
func (c *Client) Remove(parent context.Context, req protocol.RemovalRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.ValidateFor(protocol.RuntimeDocker, time.Now()); err != nil {
		return refused(res, err)
	}
```

In `cmd/agent/main.go`:

Replace

```go
		}
		identities, where = fatalOnConflict{secret}, "Secret "+namespace+"/"+*identitySecret
		snapshot, logs = cluster.Snapshot, cluster.Logs
		facts = cluster.Facts(ctx)
	} else if *socket != "" {
```

with

```go
		}
		identities, where = fatalOnConflict{secret}, "Secret "+namespace+"/"+*identitySecret
		snapshot, logs, deploy, remove = cluster.Snapshot, cluster.Logs, cluster.Deploy, cluster.Remove
		facts = cluster.Facts(ctx)
	} else if *socket != "" {
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -w cmd internal && go vet ./... && go test -race -count=1 ./internal/runtime/... ./internal/agent/... ./cmd/agent/ && go list -deps ./cmd/server | grep -c k8s.io`
Expected: every package `ok` (the Docker adapter's deploy and removal tests unchanged), then `0`.

- [ ] **Step 5: DOX and commit**

In `internal/runtime/kubernetes/AGENTS.md`: replace `## Purpose`'s second sentence (`Read-only in M8 PR 20: inventory, health input and pod logs.`) with `It reads inventory, health input and pod logs (M8 PR 20) and, in the namespaces its manifest grants, applies and removes an application's objects (M8 PR 21).`; in `## Ownership`, replace `No client-go type leaves this package; \`cmd/agent\` sees only \`Client\`,` with `No client-go type leaves this package; \`cmd/agent\` sees only \`Client\` (with \`Deploy\` and \`Remove\`),`; in `## Local Contracts`, append:

```markdown
- `Deploy` validates `ValidateFor(kubernetes)`, then runs every service's precondition before any write: the first also asks the API server (`SelfSubjectAccessReview`, `create` `apps/deployments` in the namespace) and stops `denied forbidden` without the grant; each reads its planned objects by name and stops `denied name_taken` with `Kind/name` when one exists without this instance's `kyyard.busnes.app/instance` label. `started` is called once, before the first write. Per service, `create` writes ConfigMap, Secret, Deployment, Service in that order, updating an owned object at its `resourceVersion` (a Service keeps its cluster IPs and IP families) and creating an absent one; a conflict or a lost create race re-reads once (still owned: written again; now foreign: `name_taken`), and a second conflict fails `conflict` with `Kind/name`. The Deployment as written is the service's identity (UID, generation, the pulled digest). `start` reads the Deployment every `rolloutPoll` (2 s) until the observed generation is current and the one replica is updated and ready, within the request deadline: past it the step is `timed_out rollout_timeout` with the Progressing reason, the Available reason when unavailable and the newest labelled pod's waiting reason, words only; nothing is rolled back and every later step is skipped. API errors map to `forbidden` (403), `runtime_status` with the code, `runtime_timeout`, `cancelled` (unknown) or `runtime_error`.
- `Remove` validates `ValidateFor(kubernetes)`; the first service's precondition lists Deployments, Services and ConfigMaps in the namespace by the instance label and gets each named service's Secret by name (the role grants no Secret `list`), keeping only labelled ones, and services found by label but not named are added. Each service's `remove` deletes its objects with foreground propagation (already gone counts as removed) and is skipped when nothing of it is left. `Snapshot` reports a Deployment's `kyyard.busnes.app/application` and `/instance` labels in `Workload.Application`/`Instance`.
```

and in `## Verification`, append `- \`go test -race ./internal/runtime/kubernetes/\` also drives \`Deploy\` and \`Remove\` against the fake clientset with reactors standing in for the API server (access review, UIDs and generations, rollout status, conflicts): create then update in place, refusals before any write, one conflict retried, a timed-out rollout's reasons, a Docker frame refused, and removal by label that leaves foreign objects and never lists Secrets.`

In `internal/runtime/AGENTS.md`, `## Purpose`, replace `(M8, read-only in PR 20)` with `(M8: inventory and pod logs in PR 20, stateless deployment in PR 21)`, and in `## Child DOX Index` replace `client-go adapter for a cluster agent: inventory, pod logs, the identity Secret and the install manifest.` with `client-go adapter for a cluster agent: inventory, pod logs, the identity Secret, the install manifest, and rendering, applying and removing an application's objects.`

In `internal/agent/AGENTS.md`, replace the bullet beginning `- \`Options.Kubernetes\` marks a cluster agent` with:

```markdown
- `Options.Kubernetes` marks a cluster agent: `helloCapabilities` advertises `kubernetes.inventory`, plus `pod.logs` with `Options.Logs`, `kubernetes.deploy` with `Options.Deploy` and `kubernetes.remove` with `Options.Remove`, and no Docker capability (a Docker agent keeps `deployment.apply`, `deployment.pull`, `deployment.remove`). The deployer validates every frame with `ValidateFor` against the agent's runtime, so a frame for the other runtime is `invalid_request` and never runs; the Docker adapter checks the same. When its snapshot fails, the facts-only report carries an empty `KubernetesInventory` so it keeps the Kubernetes shape. `cmd/agent --kubernetes` wires the cluster's `Deploy` and `Remove`.
```

```bash
git add cmd internal && make tidy-check lint && git commit -m "feat(k8s): the cluster agent applies and removes an application" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Namespaced deploy RBAC, token namespaces and the manifest route

**Files:**
- Modify: `internal/runtime/kubernetes/manifest/manifest.go` (`Input.Namespaces`, `RenderRBAC`, deploy Roles, the access-review rule)
- Test: `internal/runtime/kubernetes/manifest/manifest_test.go`
- Modify: `internal/api/endpoint_handlers.go` (token `namespaces`, `handleEndpointManifest`), `internal/api/server.go` (route)
- Test: `internal/api/endpoint_manifest_test.go` (new)
- Test: `internal/runtime/kubernetes/cluster_test.go` (real cluster: one namespace, the deploy Role's verbs, a deploy and a removal as the agent)
- Docs: `internal/runtime/kubernetes/AGENTS.md`, `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: Task 2 (`store.NormalizeNamespaces`, `SetEndpointDeployNamespaces`, `CreateEnrollmentToken(…, namespaces...)`, `Endpoint.DeployNamespaces`), Task 5 (`Client.Deploy`, `Client.Remove` for the real-cluster test), `protocol.ValidDNSLabel`.
- Produces:
  ```go
  // package manifest (still no k8s.io import)
  const MaxNamespaces = 32
  type Input struct{ Image, Link, Name string; Namespaces []string } // Namespaces sorted, distinct DNS labels
  func Render(in Input) (string, error)                               // full manifest, a Role and RoleBinding kyyard-agent-deploy per namespace
  func RenderRBAC(name string, namespaces []string) (string, error)   // the same without the enrollment Secret and the Deployment
  // HTTP
  // POST /api/organizations/{organization}/endpoints/{endpoint}/manifest  {namespaces}
  //   200 {manifest, manifest_file, command, namespaces, note}; 409 runtime_unsupported (Docker); 400 bad list; 403
  // POST .../enrollment-tokens accepts namespaces (Kubernetes only) and answers namespaces
  ```

- [ ] **Step 1: Write the failing tests**

In `internal/runtime/kubernetes/manifest/manifest_test.go`:

Replace

```go
package manifest_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/config"
```

with

```go
package manifest_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/config"
```

Replace

```go
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
```

with

```go
	if !slices.Equal(kinds, []string{"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "Secret", "Deployment"}) {
		t.Fatalf("kinds %v", kinds)
	}
	var granted []string
	for _, rule := range clusterRole.Rules {
		// The one write: asking the API server what the agent itself may do.
		if slices.Equal(rule.APIGroups, []string{"authorization.k8s.io"}) {
			if !slices.Equal(rule.Resources, []string{"selfsubjectaccessreviews"}) || !slices.Equal(rule.Verbs, []string{"create"}) || len(rule.ResourceNames) != 0 {
				t.Fatalf("access review rule %+v", rule)
			}
			continue
		}
		if !slices.Equal(rule.Verbs, []string{"get", "list"}) || len(rule.ResourceNames) != 0 || len(rule.NonResourceURLs) != 0 {
			t.Fatalf("cluster rule %+v", rule)
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
```

Replace

```go
		if got := manifest.FileName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
```

with

```go
		if got := manifest.FileName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// Each listed namespace gets the deploy Role, exactly: Deployments, Services and ConfigMaps
// read and written, Secrets written and read by name but never listed, bound to the agent.
func TestManifestGrantsDeployInListedNamespaces(t *testing.T) {
	doc, err := manifest.Render(manifest.Input{Image: image, Link: link, Name: name, Namespaces: []string{"billing", "shop"}})
	if err != nil {
		t.Fatal(err)
	}
	var kinds, namespaces []string
	for _, obj := range decode(t, doc) {
		kinds = append(kinds, obj.GetObjectKind().GroupVersionKind().Kind)
		switch o := obj.(type) {
		case *rbacv1.Role:
			if o.Name != "kyyard-agent-deploy" {
				continue
			}
			namespaces = append(namespaces, o.Namespace)
			want := []rbacv1.PolicyRule{
				{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}},
				{APIGroups: []string{""}, Resources: []string{"services", "configmaps"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}},
				{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "create", "update", "patch", "delete"}},
			}
			if !reflect.DeepEqual(o.Rules, want) {
				t.Fatalf("deploy role in %s: %+v", o.Namespace, o.Rules)
			}
		case *rbacv1.RoleBinding:
			if o.Name == "kyyard-agent-deploy" && (o.RoleRef.Name != "kyyard-agent-deploy" || len(o.Subjects) != 1 || o.Subjects[0].Name != "kyyard-agent" || o.Subjects[0].Namespace != "kyyard-agent") {
				t.Fatalf("deploy binding %+v", o)
			}
		}
	}
	if !slices.Equal(namespaces, []string{"billing", "shop"}) {
		t.Fatalf("deploy roles in %v", namespaces)
	}
	if !slices.Equal(kinds, []string{"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "Role", "RoleBinding", "Role", "RoleBinding", "Secret", "Deployment"}) {
		t.Fatalf("kinds %v", kinds)
	}
}

// The regenerated manifest carries the RBAC and nothing that enrolls: no Secret, no link, no
// Deployment.
func TestManifestRBACOnly(t *testing.T) {
	doc, err := manifest.RenderRBAC(name, []string{"shop"})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, obj := range decode(t, doc) {
		kinds = append(kinds, obj.GetObjectKind().GroupVersionKind().Kind)
	}
	if !slices.Equal(kinds, []string{"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "Role", "RoleBinding"}) {
		t.Fatalf("kinds %v", kinds)
	}
	if strings.Contains(doc, "kyyard=") || strings.Contains(doc, "kyyard-agent-enrollment") {
		t.Fatalf("an enrollment in the RBAC manifest:\n%s", doc)
	}
}

func TestManifestRefusesBadNamespaces(t *testing.T) {
	for _, list := range [][]string{{"shop", "billing"}, {"shop", "shop"}, {"Shop"}, {"shop."}, strings.Split(strings.Repeat("n,", 32)+"x", ",")} {
		if _, err := manifest.Render(manifest.Input{Image: image, Link: link, Name: name, Namespaces: list}); err == nil {
			t.Errorf("rendered %v", list)
		}
		if _, err := manifest.RenderRBAC(name, list); err == nil {
			t.Errorf("rendered RBAC %v", list)
		}
	}
	if _, err := manifest.RenderRBAC("", nil); err == nil {
		t.Error("rendered without a name")
	}
}
```

Create `internal/api/endpoint_manifest_test.go`:

```go
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// An administrator records the namespaces a cluster's agent may write in and gets the RBAC-only
// manifest granting exactly those; the call is audited with the list. A Docker endpoint has no
// manifest, a bad list is refused, and a member who may not enroll learns nothing.
func TestEndpointManifestRoute(t *testing.T) {
	f := newRuntimeFleet(t)
	route := func(id string) string { return "/api/organizations/a/endpoints/" + id + "/manifest" }
	w := tenantRequest(f.s, f.admin, "POST", route(f.cluster.id), `{"namespaces":["shop","billing"]}`, true)
	var out struct {
		Manifest, Command, Note string
		File                    string   `json:"manifest_file"`
		Namespaces              []string `json:"namespaces"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("manifest: %d %s", w.Code, w.Body.String())
	}
	if out.File != "kyyard-agent-cluster-1.yaml" || out.Command != "kubectl apply -f kyyard-agent-cluster-1.yaml" || !slices.Equal(out.Namespaces, []string{"billing", "shop"}) {
		t.Fatalf("response %+v", out)
	}
	if strings.Count(out.Manifest, "name: kyyard-agent-deploy\n  namespace: \"billing\"") != 2 || strings.Contains(out.Manifest, "kind: Deployment") || strings.Contains(out.Manifest, "kyyard-agent-enrollment") || !strings.Contains(out.Note, "delete role,rolebinding kyyard-agent-deploy") {
		t.Fatalf("manifest:\n%s", out.Manifest)
	}
	var stored []string
	for _, ns := range f.endpoint(t, f.cluster.id)["deploy_namespaces"].([]any) {
		stored = append(stored, ns.(string))
	}
	if !slices.Equal(stored, []string{"billing", "shop"}) {
		t.Fatalf("stored %v", stored)
	}
	rows, _, err := f.st.Audit().ListAuditRecords(context.Background(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rows, func(r *store.AuditRecord) bool {
		return r.Action == "endpoint.enroll" && r.Resource == f.cluster.id+"/manifest" && r.Details == "namespaces=billing,shop" && r.Result == "success"
	}) {
		t.Fatal("no audit row names the manifest's namespaces")
	}
	for _, tc := range []struct {
		cookie *http.Cookie
		id     string
		body   string
		status int
		code   string
	}{
		{f.admin, f.host.id, `{"namespaces":["shop"]}`, 409, "runtime_unsupported"},
		{f.admin, f.cluster.id, `{"namespaces":["Shop"]}`, 400, ""},
		{f.admin, f.cluster.id, `{"namespaces":["kyyard-agent"]}`, 400, ""},
		{f.admin, f.cluster.id, `{"namespaces":["shop"],"extra":1}`, 400, ""},
	} {
		w := tenantRequest(f.s, tc.cookie, "POST", route(tc.id), tc.body, true)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
			t.Errorf("%s %s: %d %s", tc.id, tc.body, w.Code, w.Body.String())
		}
	}
	viewer := loginAs(t, f.s, f.st, "viewer", "user")
	if err := f.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if w := tenantRequest(f.s, viewer, "POST", route(f.cluster.id), `{"namespaces":["shop"]}`, true); w.Code != 403 || strings.Contains(w.Body.String(), "manifest") {
		t.Fatalf("viewer: %d %s", w.Code, w.Body.String())
	}
}

// Namespaces named at enrollment go into the first manifest and onto the endpoint; a Docker
// token takes none.
func TestKubernetesEnrollmentWithNamespaces(t *testing.T) {
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
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	tokens := "/api/organizations/a/environments/env-a/enrollment-tokens"
	for _, body := range []string{`{"runtime":"docker","namespaces":["shop"]}`, `{"runtime":"kubernetes","name":"prod","namespaces":["kube-system"]}`, `{"runtime":"kubernetes","name":"prod","namespaces":["shop","shop"]}`} {
		if w := tenantRequest(s, admin, "POST", tokens, body, true); w.Code != 400 {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	w := tenantRequest(s, admin, "POST", tokens, `{"runtime":"kubernetes","name":"prod","namespaces":["shop"]}`, true)
	var out struct {
		Token, Manifest, Disclosure string
		Namespaces                  []string `json:"namespaces"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(out.Manifest, "name: kyyard-agent-deploy\n  namespace: \"shop\"") || strings.Count(out.Manifest, out.Token) != 1 || !strings.Contains(out.Disclosure, "In each namespace you listed") || !slices.Equal(out.Namespaces, []string{"shop"}) {
		t.Fatalf("mint %+v", out)
	}
	ag := redeemToken(t, s, out.Token, "prod")
	e, err := ts.ReadEndpointRaw(ctx, ag.id)
	if err != nil || !slices.Equal(e.DeployNamespaces, []string{"shop"}) {
		t.Fatalf("enrolled %+v %v", e, err)
	}
}
```

In `internal/runtime/kubernetes/cluster_test.go`:

Replace

```go
	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/manifest"
```

with

```go
	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/manifest"
```

Replace

```go
)

// TestManifestOnARealCluster applies the rendered manifest to the cluster KY_TEST_KUBECONFIG
// names (a disposable kind cluster: it creates and deletes cluster-scoped RBAC), then acts as
// the agent's ServiceAccount: Secrets are denied cluster-wide, pods are listed, the identity
// Secret round-trips, and a snapshot names the cluster's nodes with nothing forbidden.
func TestManifestOnARealCluster(t *testing.T) {
	path := os.Getenv("KY_TEST_KUBECONFIG")
```

with

```go
)

// deployNamespace is the namespace the real-cluster test grants and deploys into.
const deployNamespace = "kyyard-test-deploy"

// TestManifestOnARealCluster applies the rendered manifest to the cluster KY_TEST_KUBECONFIG
// names (a disposable kind cluster: it creates and deletes cluster-scoped RBAC), then acts as
// the agent's ServiceAccount: Secrets are denied cluster-wide, pods are listed, the identity
// Secret round-trips, and a snapshot names the cluster's nodes with nothing forbidden. In the
// one granted namespace the agent may write Deployments and read Secrets by name but not list
// them; it deploys KY_TEST_DEPLOY_IMAGE (a digest-pinned image that keeps running, such as
// registry.k8s.io/pause@sha256:...) as one service, sees it ready, and removes it.
func TestManifestOnARealCluster(t *testing.T) {
	path := os.Getenv("KY_TEST_KUBECONFIG")
```

Replace

```go
		t.Skip("KY_TEST_KUBECONFIG is not set")
	}
	admin, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
```

with

```go
		t.Skip("KY_TEST_KUBECONFIG is not set")
	}
	image := os.Getenv("KY_TEST_DEPLOY_IMAGE")
	if image == "" {
		t.Fatal("KY_TEST_DEPLOY_IMAGE must name a digest-pinned image that keeps running")
	}
	admin, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
```

Replace

```go
	removeAgent(t, ctx, cs)
	t.Cleanup(func() { removeAgent(t, context.Background(), cs) })

	doc, err := manifest.Render(manifest.Input{Image: "ghcr.io/busnes-app/kyyard@sha256:" + strings.Repeat("0", 64), Link: "https://kyyard.invalid/#kyyard=" + strings.Repeat("A", 43), Name: "kind"})
	if err != nil {
		t.Fatal(err)
```

with

```go
	removeAgent(t, ctx, cs)
	t.Cleanup(func() { removeAgent(t, context.Background(), cs) })
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: deployNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	doc, err := manifest.Render(manifest.Input{Image: "ghcr.io/busnes-app/kyyard@sha256:" + strings.Repeat("0", 64), Link: "https://kyyard.invalid/#kyyard=" + strings.Repeat("A", 43), Name: "kind", Namespaces: []string{deployNamespace}})
	if err != nil {
		t.Fatal(err)
```

Replace

```go
		}
	}

	c, err := kubernetes.New(agent)
```

with

```go
		}
	}
	for _, tc := range []struct {
		verb, group, resource, namespace string
		allowed                          bool
	}{
		{"create", "apps", "deployments", deployNamespace, true},
		{"delete", "", "services", deployNamespace, true},
		{"update", "", "configmaps", deployNamespace, true},
		{"get", "", "secrets", deployNamespace, true},
		{"list", "", "secrets", deployNamespace, false},
		{"create", "apps", "deployments", "default", false},
		{"create", "", "secrets", "default", false},
	} {
		review, err := as.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: tc.verb, Group: tc.group, Resource: tc.resource, Namespace: tc.namespace}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if review.Status.Allowed != tc.allowed {
			t.Errorf("%s %s/%s in %q: allowed=%v", tc.verb, tc.group, tc.resource, tc.namespace, review.Status.Allowed)
		}
	}

	c, err := kubernetes.New(agent)
```

Replace

```go
		t.Fatalf("snapshot as the agent: %v truncated %v nodes %+v", err, snap.Truncated, snap.Kubernetes.Nodes)
	}
}
```

with

```go
		t.Fatalf("snapshot as the agent: %v truncated %v nodes %+v", err, snap.Truncated, snap.Kubernetes.Nodes)
	}

	_, digest, _ := strings.Cut(image, "@")
	now := time.Now()
	target := &protocol.KubernetesTarget{Namespace: deployNamespace, ApplicationID: "11111111-2222-4333-8444-555555555555", InstanceID: "66666666-7777-4888-9999-aaaaaaaaaaaa", SpecDigest: "sha256:" + strings.Repeat("f", 64)}
	req := protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_kind", Project: "kind", Revision: 1, IssuedAt: now, Deadline: now.Add(2 * time.Minute), Kubernetes: target,
		Services: []protocol.DeploymentService{{Name: "idle", Pull: &protocol.ImagePull{Reference: image, Digest: digest}, Ports: []protocol.Port{{Container: 8080, Host: 80, Protocol: "tcp"}}, Env: map[string]string{"TOKEN": "x"}, SecretKeys: []string{"TOKEN"}, Mounts: []protocol.Mount{}}}}
	res := c.Deploy(ctx, req, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("deploy as the agent: %+v", res)
	}
	d, err := cs.AppsV1().Deployments(deployNamespace).Get(ctx, "kind-idle", metav1.GetOptions{})
	if err != nil || d.Status.ReadyReplicas != 1 || string(d.UID) != res.Services[0].UID {
		t.Fatalf("deployed %+v %v", d, err)
	}
	removal := protocol.RemovalRequest{Deployment: "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_kind", Project: "kind", IssuedAt: time.Now(), Deadline: time.Now().Add(time.Minute), Kubernetes: target, Services: []string{"idle"}}
	if res := c.Remove(ctx, removal, func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("remove as the agent: %+v", res)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		_, err := cs.AppsV1().Deployments(deployNamespace).Get(ctx, "kind-idle", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Deployment is still there: %v", err)
		}
	}
	for _, name := range []string{"kind-idle-env", "kind-idle-secret"} {
		_, errCM := cs.CoreV1().ConfigMaps(deployNamespace).Get(ctx, name, metav1.GetOptions{})
		_, errS := cs.CoreV1().Secrets(deployNamespace).Get(ctx, name, metav1.GetOptions{})
		if !apierrors.IsNotFound(errCM) || !apierrors.IsNotFound(errS) {
			t.Fatalf("%s left behind: %v %v", name, errCM, errS)
		}
	}
}
```

Replace

```go
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

with

```go
	ignore(cs.RbacV1().ClusterRoleBindings().Delete(ctx, "kyyard-agent-read", metav1.DeleteOptions{}))
	ignore(cs.RbacV1().ClusterRoles().Delete(ctx, "kyyard-agent-read", metav1.DeleteOptions{}))
	for _, ns := range []string{"kyyard-agent", deployNamespace} {
		ignore(cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}))
	}
	deadline := time.Now().Add(2 * time.Minute)
	for _, ns := range []string{"kyyard-agent", deployNamespace} {
		for {
			_, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("namespace %s still present: %v", ns, err)
			}
			time.Sleep(time.Second)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runtime/kubernetes/... ./internal/api/`
Expected: FAIL to compile: `unknown field Namespaces in struct literal of type manifest.Input`, `undefined: manifest.RenderRBAC`; the API test's manifest route answers 404 once the package compiles.

- [ ] **Step 3: Implement**

In `internal/runtime/kubernetes/manifest/manifest.go`:

Replace

```go
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
```

with

```go
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"text/template"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/config"
)

// MaxNamespaces bounds the namespaces one manifest grants writes in.
const MaxNamespaces = 32

// Input is everything the manifest varies on. Link carries the single-use token; it appears
// once, in the enrollment Secret. Namespaces, sorted and distinct, each get the deploy Role.
type Input struct {
	Image      string
	Link       string
	Name       string
	Namespaces []string
}
```

Replace

```go
		return "", errors.New("the agent image is not pinned to a digest")
	}
	if in.Name == "" || !strings.HasPrefix(in.Link, "https://") {
		return "", errors.New("a manifest needs an endpoint name and an HTTPS enrollment link")
	}
	var b strings.Builder
	err := manifestTemplate.Execute(&b, map[string]string{"Image": in.Image, "Link": in.Link, "Name": in.Name, "File": FileName(in.Name)})
	return b.String(), err
}
```

with

```go
		return "", errors.New("the agent image is not pinned to a digest")
	}
	if !strings.HasPrefix(in.Link, "https://") {
		return "", errors.New("a manifest needs an HTTPS enrollment link")
	}
	return execute(in)
}

// RenderRBAC returns the manifest without the enrollment Secret and the agent Deployment: what
// an administrator applies to change the namespaces an enrolled agent may write in.
func RenderRBAC(name string, namespaces []string) (string, error) {
	return execute(Input{Name: name, Namespaces: namespaces})
}

func execute(in Input) (string, error) {
	if in.Name == "" {
		return "", errors.New("a manifest needs an endpoint name")
	}
	if len(in.Namespaces) > MaxNamespaces || !slices.IsSorted(in.Namespaces) || len(slices.Compact(slices.Clone(in.Namespaces))) != len(in.Namespaces) || slices.ContainsFunc(in.Namespaces, func(ns string) bool { return !protocol.ValidDNSLabel(ns) }) {
		return "", errors.New("namespaces are distinct DNS labels, sorted, at most 32")
	}
	var b strings.Builder
	err := manifestTemplate.Execute(&b, struct {
		Input
		File string
	}{in, FileName(in.Name)})
	return b.String(), err
}
```

Replace

```go
# persistentvolumeclaims, deployments, statefulsets and daemonsets in every namespace. It cannot
# read Secrets or ConfigMaps; in its own namespace it may read and write its identity Secret.
# The Secret kyyard-agent-enrollment holds a single-use enrollment link. Once the endpoint is
# approved, delete it: kubectl -n kyyard-agent delete secret kyyard-agent-enrollment
# Uninstall: kubectl delete -f {{.File}}
apiVersion: v1
```

with

```go
# persistentvolumeclaims, deployments, statefulsets and daemonsets in every namespace. It cannot
# read Secrets or ConfigMaps; in its own namespace it may read and write its identity Secret.
{{- if .Namespaces}}
# In each namespace listed below (Role kyyard-agent-deploy) it may create, update and delete
# Deployments, Services, ConfigMaps and Secrets; Secrets are read by name, never listed. Create
# the namespaces first. A namespace dropped from a later manifest keeps its Role until you run
# kubectl -n <namespace> delete role,rolebinding kyyard-agent-deploy
{{- end}}
{{- if .Link}}
# The Secret kyyard-agent-enrollment holds a single-use enrollment link. Once the endpoint is
# approved, delete it: kubectl -n kyyard-agent delete secret kyyard-agent-enrollment
{{- end}}
# Uninstall: kubectl delete -f {{.File}}
apiVersion: v1
```

Replace

```go
    resources: [deployments, statefulsets, daemonsets]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
```

with

```go
    resources: [deployments, statefulsets, daemonsets]
    verbs: [get, list]
  # The agent asks the API server what it may do before every apply.
  - apiGroups: [authorization.k8s.io]
    resources: [selfsubjectaccessreviews]
    verbs: [create]
---
apiVersion: rbac.authorization.k8s.io/v1
```

Replace

```go
    name: kyyard-agent
    namespace: kyyard-agent
---
apiVersion: v1
```

with

```go
    name: kyyard-agent
    namespace: kyyard-agent
{{- range .Namespaces}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kyyard-agent-deploy
  namespace: {{q .}}
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
rules:
  - apiGroups: [apps]
    resources: [deployments]
    verbs: [get, list, create, update, patch, delete]
  - apiGroups: [""]
    resources: [services, configmaps]
    verbs: [get, list, create, update, patch, delete]
  # No list: the agent reads each Secret it owns by name.
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, create, update, patch, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kyyard-agent-deploy
  namespace: {{q .}}
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kyyard-agent-deploy
subjects:
  - kind: ServiceAccount
    name: kyyard-agent
    namespace: kyyard-agent
{{- end}}
{{- if .Link}}
---
apiVersion: v1
```

Replace

```go
            optional: true
            defaultMode: 0440
`))
```

with

```go
            optional: true
            defaultMode: 0440
{{- end}}
`))
```

In `internal/api/endpoint_handlers.go`:

Replace

```go
const clusterDisclosure = "The KyYard agent's ServiceAccount can get and list namespaces, nodes, pods, pod logs, events, services, persistent volume claims, deployments, statefulsets and daemonsets in every namespace. It cannot read Secrets or ConfigMaps; in its own namespace kyyard-agent it reads and writes only its identity Secret. Applying the manifest needs cluster-admin, because it creates a ClusterRole and a ClusterRoleBinding."

const clusterNote = "Save the manifest and apply it with a cluster-admin kubeconfig. Run kubectl -n kyyard-agent logs deploy/kyyard-agent and compare the agent key fingerprint before approving. Once approved, delete the spent enrollment Secret: kubectl -n kyyard-agent delete secret kyyard-agent-enrollment. Uninstall with kubectl delete -f on the same file."
```

with

```go
const clusterDisclosure = "The KyYard agent's ServiceAccount can get and list namespaces, nodes, pods, pod logs, events, services, persistent volume claims, deployments, statefulsets and daemonsets in every namespace. It cannot read Secrets or ConfigMaps; in its own namespace kyyard-agent it reads and writes only its identity Secret. Applying the manifest needs cluster-admin, because it creates a ClusterRole and a ClusterRoleBinding."

// namespaceDisclosure is added when the manifest grants writes in namespaces.
const namespaceDisclosure = " In each namespace you listed it may create, update and delete Deployments, Services, ConfigMaps and Secrets (Secrets by name only, never listed), and nothing elsewhere."

// manifestNote goes with a regenerated manifest.
const manifestNote = "Apply it with a cluster-admin kubeconfig: kubectl apply -f on the saved file. Create the namespaces first. A namespace you removed keeps its Role until you run kubectl -n <namespace> delete role,rolebinding kyyard-agent-deploy."

const clusterNote = "Save the manifest and apply it with a cluster-admin kubeconfig. Run kubectl -n kyyard-agent logs deploy/kyyard-agent and compare the agent key fingerprint before approving. Once approved, delete the spent enrollment Secret: kubectl -n kyyard-agent delete secret kyyard-agent-enrollment. Uninstall with kubectl delete -f on the same file."
```

Replace

```go
func (s *Server) handleCreateEnrollmentToken(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		Runtime string `json:"runtime"`
		Name    string `json:"name"`
	}
	if err := strictJSON(r, &input); err != nil {
```

with

```go
func (s *Server) handleCreateEnrollmentToken(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		Runtime    string   `json:"runtime"`
		Name       string   `json:"name"`
		Namespaces []string `json:"namespaces"`
	}
	if err := strictJSON(r, &input); err != nil {
```

Replace

```go
	// A pod has no useful hostname, so a cluster is named here; a Docker host names itself.
	kube := input.Runtime == protocol.RuntimeKubernetes
	if kube != (input.Name != "") || (kube && !store.ValidEndpointName(input.Name)) {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	https := strings.HasPrefix(s.config.Server.AppURL, "https://")
	image := s.config.Server.AgentImage
```

with

```go
	// A pod has no useful hostname, so a cluster is named here; a Docker host names itself.
	kube := input.Runtime == protocol.RuntimeKubernetes
	if kube != (input.Name != "") || (kube && !store.ValidEndpointName(input.Name)) || (!kube && len(input.Namespaces) > 0) {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	namespaces, err := store.NormalizeNamespaces(input.Namespaces)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	https := strings.HasPrefix(s.config.Server.AppURL, "https://")
	image := s.config.Server.AgentImage
```

Replace

```go
		return
	}
	tok, err := s.store.Tenancy().CreateEnrollmentToken(r.Context(), a, input.Runtime, image)
	if err != nil {
		s.tenantError(w, err)
```

with

```go
		return
	}
	tok, err := s.store.Tenancy().CreateEnrollmentToken(r.Context(), a, input.Runtime, image, namespaces...)
	if err != nil {
		s.tenantError(w, err)
```

Replace

```go
	out["image"] = image
	if kube {
		doc, err := manifest.Render(manifest.Input{Image: image, Link: strings.TrimRight(s.config.Server.AppURL, "/") + "/#kyyard=" + secret, Name: input.Name})
		if err != nil {
			s.tenantError(w, err)
```

with

```go
	out["image"] = image
	if kube {
		doc, err := manifest.Render(manifest.Input{Image: image, Link: strings.TrimRight(s.config.Server.AppURL, "/") + "/#kyyard=" + secret, Name: input.Name, Namespaces: namespaces})
		if err != nil {
			s.tenantError(w, err)
```

Replace

```go
		file := manifest.FileName(input.Name)
		out["manifest"], out["manifest_file"], out["command"] = doc, file, "kubectl apply -f "+file
		out["disclosure"], out["note"] = clusterDisclosure, clusterNote
		s.writeJSON(w, http.StatusCreated, out)
		return
```

with

```go
		file := manifest.FileName(input.Name)
		out["manifest"], out["manifest_file"], out["command"] = doc, file, "kubectl apply -f "+file
		out["disclosure"], out["note"], out["namespaces"] = clusterDisclosure, clusterNote, namespaces
		if len(namespaces) > 0 {
			out["disclosure"] = clusterDisclosure + namespaceDisclosure
		}
		s.writeJSON(w, http.StatusCreated, out)
		return
```

Replace

```go
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
```

with

```go
}

// handleEndpointManifest records the namespaces a cluster's agent may write in and returns the
// manifest that grants exactly those, RBAC only, for a cluster-admin to apply. The list is what
// plans and mappings check; the agent asks the API server for its real grant before each apply.
func (s *Server) handleEndpointManifest(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	var input struct {
		Namespaces []string `json:"namespaces"`
	}
	if err := strictJSON(r, &input); err != nil {
		s.tenantError(w, err)
		return
	}
	e, err := s.store.Tenancy().SetEndpointDeployNamespaces(r.Context(), a, id, input.Namespaces)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	doc, err := manifest.RenderRBAC(e.Name, e.DeployNamespaces)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	file := manifest.FileName(e.Name)
	s.writeJSON(w, http.StatusOK, map[string]any{"manifest": doc, "manifest_file": file, "command": "kubectl apply -f " + file, "namespaces": e.DeployNamespaces, "note": manifestNote})
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
```

In `internal/api/server.go`:

Replace

```go
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/containers/{container}/exec", s.tracked(s.tenantRoute(s.handleContainerExec)))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/approve", s.tenantRoute(s.handleApproveEndpoint))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/reject", s.tenantRoute(s.endpointTransition(s.store.Tenancy().RejectEndpoint)))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/revoke", s.tenantRoute(s.endpointTransition(s.store.Tenancy().RevokeEndpoint)))
```

with

```go
	s.mux.HandleFunc("GET /api/organizations/{organization}/endpoints/{endpoint}/containers/{container}/exec", s.tracked(s.tenantRoute(s.handleContainerExec)))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/approve", s.tenantRoute(s.handleApproveEndpoint))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/manifest", s.tenantRoute(s.handleEndpointManifest))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/reject", s.tenantRoute(s.endpointTransition(s.store.Tenancy().RejectEndpoint)))
	s.mux.HandleFunc("POST /api/organizations/{organization}/endpoints/{endpoint}/revoke", s.tenantRoute(s.endpointTransition(s.store.Tenancy().RevokeEndpoint)))
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -w internal && go vet ./... && go test -race -count=1 ./internal/runtime/kubernetes/... && go test -count=1 -run 'TestEndpointManifestRoute|TestKubernetesEnrollment' ./internal/api/ && go list -deps ./cmd/server | grep -c k8s.io`
Expected: every package `ok` (`TestManifestOnARealCluster` skips without `KY_TEST_KUBECONFIG`), then `0`.

- [ ] **Step 5: DOX and commit**

In `internal/runtime/kubernetes/AGENTS.md`, in the bullet beginning `- \`manifest/\` renders the install manifest`, append: ` \`Input.Namespaces\` (sorted, distinct DNS labels, at most 32) adds, per namespace, a Role and RoleBinding \`kyyard-agent-deploy\` to the agent's ServiceAccount: \`apps deployments\` and core \`services\`, \`configmaps\` get/list/create/update/patch/delete, core \`secrets\` get/create/update/patch/delete and no list. The ClusterRole's read rules are unchanged and it gains one \`authorization.k8s.io selfsubjectaccessreviews create\` rule. \`RenderRBAC\` renders the same file without the enrollment Secret and the Deployment (no link, no image), for an administrator changing an enrolled cluster's namespaces; a namespace dropped later keeps its Role until deleted by hand, as the header says.` In `## Verification`, replace the bullet beginning `- \`KY_TEST_KUBECONFIG=<kubeconfig of a disposable cluster> go test -run TestManifestOnARealCluster` with:

```markdown
- `KY_TEST_KUBECONFIG=<kubeconfig of a disposable cluster> KY_TEST_DEPLOY_IMAGE=<digest-pinned image that keeps running, e.g. registry.k8s.io/pause@sha256:...> go test -run TestManifestOnARealCluster ./internal/runtime/kubernetes/` creates the namespace `kyyard-test-deploy`, applies the manifest granting it, impersonates the ServiceAccount (SelfSubjectAccessReview: `get secrets` denied cluster-wide, `list pods` allowed, identity Secret update allowed only by name; in the granted namespace `create deployments`, `delete services`, `update configmaps` and `get secrets` allowed, `list secrets` denied; `create deployments` in `default` denied), round-trips the identity Secret, takes a snapshot naming the nodes, deploys one service as the agent and sees it ready with the reported UID, removes it and sees its Deployment, ConfigMap and Secret gone. It creates and deletes cluster-scoped RBAC and both namespaces before and after; it skips without `KY_TEST_KUBECONFIG`, fails without `KY_TEST_DEPLOY_IMAGE`, and is not in CI.
```

In `internal/api/AGENTS.md`, in the bullet beginning `- \`POST .../enrollment-tokens\` with \`{runtime: "kubernetes", name}\``, replace `with \`{runtime: "kubernetes", name}\`` with `with \`{runtime: "kubernetes", name, namespaces?}\`` and append: ` \`namespaces\` (Kubernetes only; \`store.NormalizeNamespaces\`, 400 before minting) are rendered into the manifest's deploy Roles, stored on the token for the endpoint, answered as \`namespaces\`, and add the namespaced-write sentence to the disclosure.` After that bullet, add:

```markdown
- `POST /api/organizations/{organization}/endpoints/{endpoint}/manifest` with `{namespaces}` (`handleEndpointManifest`) stores the list through `SetEndpointDeployNamespaces` (`endpoint.enroll`, audited `<endpoint>/manifest` with the list; a Docker endpoint is 409 `runtime_unsupported`, a bad list 400) and answers 200 `{manifest, manifest_file, command, namespaces, note}` with `manifest.RenderRBAC`: the RBAC only, no enrollment link. The list is what mappings and plans check; the agent asks the API server for its real grant before each apply.
```

```bash
git add internal && make tidy-check lint && git commit -m "feat(k8s): namespaced deploy RBAC in the manifest and its regeneration route" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: API — `runtimeGate`, and mapping, plan, apply and removal on a cluster

**Files:**
- Modify: `internal/api/runtime_gate.go` (`dockerOnly` becomes `runtimeGate(kind)`)
- Modify: `internal/api/log_handlers.go`, `inspection.go`, `exec_handlers.go`, `endpoint_handlers.go` (call sites, `dockerRoute`)
- Modify: `internal/api/application_handlers.go` (mapping, plan, apply, removal on both runtimes; `maxFrameBytes`)
- Modify: `internal/api/tenant_handlers.go` (`namespace_unknown`)
- Test: `internal/api/kubernetes_deploy_test.go` (new: the fake cluster agent end to end, plan blockers, the capability, the gate matrix); `internal/api/runtime_rules_test.go` (hello rows; the Docker-routes test's comment)
- Docs: `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: Tasks 1–3 and 6 (the store's Kubernetes paths, `ErrNamespaceUnknown`, the manifest route), existing test helpers `setupTestServer`, `loginAs`, `enrollClusterAgent`, `connect`, `readEnvelope`, `writeEnvelope`, `waitFor`, `tenantRequest`, `newRuntimeFleet`, `fakeDigests`, `api.SetDigestResolverForTest`, `api.ValidationTickForTest`.
- Produces (package `api`, unexported):
  ```go
  type routeKind int
  const (dockerRoute routeKind = iota; applicationRoute)
  func (s *Server) runtimeGate(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpointID string, kind routeKind) bool
  ```
  HTTP: mapping PUT takes `{endpoint_id, namespace}` (400 `namespace_unknown`); a Kubernetes plan runs under the image-update guard and a registry slot; apply needs `kubernetes.deploy` and removal `kubernetes.remove` on a cluster (501 otherwise); a plan or instance of the other runtime's shape is 409 `runtime_unsupported`.

- [ ] **Step 1: Write the failing tests**

Create `internal/api/kubernetes_deploy_test.go`:

```go
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// clusterHost is an approved Kubernetes endpoint granted namespace shop, connected through a
// fake cluster agent that advertises the deploy and remove capabilities, with anonymous pulls
// on and a registry that answers remote for every tag.
type clusterHost struct {
	s      *api.Server
	st     store.Store
	admin  *http.Cookie
	ag     enrolledAgent
	sock   *agentSocket
	ctx    context.Context
	base   string
	remote string
}

func newClusterHost(t *testing.T, capabilities ...string) clusterHost {
	t.Helper()
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	ts := st.Tenancy()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}))
	must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}))
	h := clusterHost{s: s, st: st, admin: loginAs(t, s, st, "deployer", "user"), ctx: ctx, base: "/api/organizations/a/environments/env-a/applications", remote: "sha256:" + strings.Repeat("d", 64)}
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_deployer", Role: store.RoleOrganizationAdmin, Status: "active"}))
	must(ts.SetAnonymousPull(ctx, store.TenantAccess{ActorID: "usr_deployer", OrganizationID: "a"}, true))
	api.SetDigestResolverForTest(s, &fakeDigests{digest: h.remote})
	httpSrv := httptest.NewServer(s)
	t.Cleanup(httpSrv.Close)
	h.ag = enrollClusterAgent(t, s, st, "usr_deployer", "cluster-1")
	h.do(t, "POST", "/api/organizations/a/endpoints/"+h.ag.id+"/approve", `{"fingerprint":"`+h.ag.fp+`"}`, 204)
	h.do(t, "POST", "/api/organizations/a/endpoints/"+h.ag.id+"/manifest", `{"namespaces":["shop"]}`, 200)
	sock, reason := connect(t, ctx, httpSrv.URL, h.ag, h.ag.priv, protocol.Version)
	if sock == nil {
		t.Fatalf("connect: %s", reason)
	}
	t.Cleanup(func() { sock.conn.CloseNow() })
	h.sock = sock
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()), Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.36.0"},
		Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "n1", Ready: true}}, Namespaces: []string{"shop"}, Workloads: []protocol.Workload{}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}}})
	h.sync(t)
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, h.ag.id); return e != nil && e.State == "active" })
	return h
}

func (h clusterHost) do(t *testing.T, method, path, body string, status int) string {
	t.Helper()
	w := tenantRequest(h.s, h.admin, method, path, body, true)
	if w.Code != status {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// sync proves every frame written before it has been handled.
func (h clusterHost) sync(t *testing.T) {
	t.Helper()
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeHeartbeat, nil)
	if f := readEnvelope(t, h.ctx, h.sock.conn); f.Type != protocol.TypeHeartbeat {
		t.Fatalf("expected a heartbeat, got %s", f.Type)
	}
}

// importApp saves compose as application name and returns its path.
func (h clusterHost) importApp(t *testing.T, name, compose string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "compose": compose})
	var app store.Application
	if err := json.Unmarshal([]byte(h.do(t, "POST", h.base, string(body), 201)), &app); err != nil {
		t.Fatal(err)
	}
	return h.base + "/" + app.ID
}

var clusterCapabilities = []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesRemove}

// The stateless definition maps to a namespace of the cluster, plans with every image pinned at
// the registry, applies through the cluster agent with its target and secret keys, settles with
// Deployment identities, is validated as unverifiable, and is removed by its instance label.
func TestKubernetesApplicationOverTheClusterAgent(t *testing.T) {
	h := newClusterHost(t, clusterCapabilities...)
	const canary = "cluster-secret-canary"
	app := h.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1.2, restart: always, ports: [{target: 80, published: 8080}], environment: {TOKEN: "+canary+"}}}")
	if body := h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"prod"}`, 400); !strings.Contains(body, "namespace_unknown") {
		t.Fatalf("unknown namespace: %s", body)
	}
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped); err != nil || mapped.Runtime != protocol.RuntimeKubernetes || mapped.Namespace != "shop" || mapped.Preview.Project != "shop" {
		t.Fatalf("mapping %+v %v", mapped, err)
	}
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop"})
	var planned store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/deployments", string(planBody), 201)), &planned); err != nil {
		t.Fatal(err)
	}
	ps := planned.Plan.Services[0]
	if planned.Plan.Namespace != "shop" || ps.Object == nil || ps.Object.Name != "shop-web" || ps.PullDigest != h.remote || ps.PullReference != "ghcr.io/org/web@"+h.remote {
		t.Fatalf("plan %+v", planned.Plan)
	}
	var applying store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/deployments/"+planned.ID+"/apply", `{"confirm":"shop"}`, 202)), &applying); err != nil || applying.State != "applying" {
		t.Fatalf("apply %+v %v", applying, err)
	}
	frame := readEnvelope(t, h.ctx, h.sock.conn)
	var sent protocol.DeploymentRequest
	if frame.Type != protocol.TypeDeploymentApply || json.Unmarshal(frame.Payload, &sent) != nil {
		t.Fatalf("frame %s", frame.Type)
	}
	svc := sent.Services[0]
	if sent.Kubernetes == nil || sent.Kubernetes.Namespace != "shop" || sent.Kubernetes.InstanceID != mapped.InstanceID || svc.Env["TOKEN"] != canary || !slices.Equal(svc.SecretKeys, []string{"TOKEN"}) || svc.Pull.Digest != h.remote || svc.ContainerName != "" || len(sent.Registries) != 0 {
		t.Fatalf("frame %+v", sent)
	}
	if err := sent.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: planned.ID, RequestID: planned.CorrelationID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded}, {Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}, {Service: "web", Step: protocol.StepStart, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{{Service: "web", Kind: protocol.KindDeployment, Namespace: "shop", Name: "shop-web", UID: "0f1e2d3c-4b5a-4968-8776-655443322110", Generation: 1, ImageDigest: h.remote}}})
	h.sync(t)
	var settled store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/deployments/"+planned.ID, "", 200)), &settled); err != nil || settled.State != protocol.OutcomeSucceeded || settled.Result.Services[0].Name != "shop-web" || strings.Contains(h.do(t, "GET", app+"/deployments/"+planned.ID, "", 200), canary) {
		t.Fatalf("settled %+v %v", settled, err)
	}
	// A cluster agent cannot report container health, so the apply is not validated.
	api.ValidationTickForTest(h.s, time.Now().Add(store.ValidationGrace+time.Minute))
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/deployments/"+planned.ID, "", 200)), &settled); err != nil || settled.Validation == nil || settled.Validation.Verdict != store.VerdictUnverifiable {
		t.Fatalf("validation %+v %v", settled.Validation, err)
	}

	var removing store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/removal", `{"instance_id":"`+mapped.InstanceID+`","confirm":"shop"}`, 202)), &removing); err != nil {
		t.Fatal(err)
	}
	frame = readEnvelope(t, h.ctx, h.sock.conn)
	var removal protocol.RemovalRequest
	if frame.Type != protocol.TypeDeploymentRemove || json.Unmarshal(frame.Payload, &removal) != nil || removal.Kubernetes == nil || !slices.Equal(removal.Services, []string{"web"}) || len(removal.Containers) != 0 {
		t.Fatalf("removal frame %s %+v", frame.Type, removal)
	}
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: removing.ID, RequestID: removing.CorrelationID, Outcome: protocol.OutcomeSucceeded, Services: []protocol.DeploymentIdentity{},
		Steps: []protocol.DeploymentStep{{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded}, {Service: "web", Step: protocol.StepRemove, Outcome: protocol.OutcomeSucceeded}}})
	h.sync(t)
	var instances []store.ApplicationInstance
	if err := json.Unmarshal([]byte(h.do(t, "GET", h.base+"/instances", "", 200)), &instances); err != nil || len(instances) != 0 {
		t.Fatalf("instances after removal %+v %v", instances, err)
	}
}

// A stateful definition stops at the plan, naming each service and why; nothing is sent.
func TestKubernetesPlanBlockersOverTheAPI(t *testing.T) {
	h := newClusterHost(t, clusterCapabilities...)
	app := h.importApp(t, "db", "services: {db: {image: ghcr.io/org/db:1, volumes: ['data:/var/lib/db']}, web: {image: ghcr.io/org/web:1, restart: on-failure}}\nvolumes: {data: {}}")
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	_ = json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped)
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "db"})
	var refused struct {
		Code     string
		Blockers []string
		Services []store.BlockedService
	}
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/deployments", string(planBody), 409)), &refused); err != nil {
		t.Fatal(err)
	}
	if refused.Code != "preflight_blocked" || !slices.Equal(refused.Blockers, []string{"kubernetes_unsupported"}) || len(refused.Services) != 2 || !slices.Equal(refused.Services[0].Unsupported, []string{"k8s_volume"}) || !slices.Equal(refused.Services[1].Unsupported, []string{"k8s_restart"}) {
		t.Fatalf("refused %+v", refused)
	}
}

// A cluster agent without kubernetes.deploy is asked to upgrade, not sent a frame.
func TestKubernetesApplyNeedsTheCapability(t *testing.T) {
	h := newClusterHost(t, protocol.CapabilityKubernetesInventory)
	app := h.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1}}")
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	_ = json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped)
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop"})
	if body := h.do(t, "POST", app+"/deployments", string(planBody), 409); !strings.Contains(body, "agent_deploy_unsupported") {
		t.Fatalf("plan: %s", body)
	}
	h.do(t, "POST", app+"/removal", `{"instance_id":"`+mapped.InstanceID+`","confirm":"shop"}`, 501)
}

// Docker routes refuse the cluster; application routes take it, so none of them answers
// runtime_unsupported for an application mapped to the cluster.
func TestRuntimeGateMatrix(t *testing.T) {
	h := newClusterHost(t, clusterCapabilities...)
	app := h.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1}}")
	ep := "/api/organizations/a/endpoints/" + h.ag.id
	for _, route := range []struct{ method, path, body string }{
		{"POST", ep + "/commands", `{"action":"container.restart","container":"shop-web"}`},
		{"GET", ep + "/containers/shop-web/removal", ""},
		{"GET", ep + "/containers/shop-web/inspection", ""},
		{"GET", ep + "/containers/shop-web/logs", ""},
		{"GET", app + "/adoption?endpoint=" + h.ag.id + "&project=shop", ""},
		{"POST", app + "/adoption", `{"endpoint_id":"` + h.ag.id + `","project":"shop","digest":"x","confirm":"shop"}`},
	} {
		w := tenantRequest(h.s, h.admin, route.method, route.path, route.body, true)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
			t.Errorf("docker route %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	_ = json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped)
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop"})
	var planned store.Deployment
	_ = json.Unmarshal([]byte(h.do(t, "POST", app+"/deployments", string(planBody), 201)), &planned)
	for _, route := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", app + "/mapping", "", 200},
		{"GET", app + "/preflight", "", 200},
		{"PUT", app + "/mapping", `{"endpoint_id":"` + h.ag.id + `","namespace":"shop"}`, 204},
		{"POST", app + "/deployments/" + planned.ID + "/apply", `{"confirm":"shop"}`, 409}, // the remap outdated the plan
	} {
		w := tenantRequest(h.s, h.admin, route.method, route.path, route.body, true)
		if w.Code != route.status || strings.Contains(w.Body.String(), "runtime_unsupported") {
			t.Errorf("application route %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
}
```

In `internal/api/runtime_rules_test.go`:

Replace

```go
		{"cluster capabilities from a cluster", f.cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs}, true},
		{"docker capabilities from a host", f.host, []string{protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
```

with

```go
		{"cluster capabilities from a cluster", f.cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs}, true},
		{"docker capabilities from a host", f.host, []string{protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply}, true},
		{"deployment.pull from a cluster", f.cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesDeploy, protocol.CapabilityDeploymentPull}, false},
		{"cluster deployment from a host", f.host, []string{protocol.CapabilityDeploymentApply, protocol.CapabilityKubernetesDeploy}, false},
		{"cluster deployment capabilities from a cluster", f.cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs, protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesRemove}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
```

Replace

```go
}

// Every route that acts on containers, images, exec, inspection, deployments, adoption or the
// service mapping refuses a Kubernetes endpoint with runtime_unsupported, before any
// capability or state check.
func TestDockerRoutesRefuseAKubernetesEndpoint(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
```

with

```go
}

// A Docker-adopted application whose endpoint reads as a cluster is refused on every route with
// runtime_unsupported, before any capability or state check: the Docker routes refuse the
// cluster, and the application routes refuse an instance of the other runtime's shape.
func TestDockerRoutesRefuseAKubernetesEndpoint(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestKubernetes|TestRuntimeGateMatrix|TestHelloCapabilities' ./internal/api/`
Expected: FAIL: an unlisted namespace answers 500 (`TestKubernetesApplicationOverTheClusterAgent`), and planning a mapped cluster application answers 409 `runtime_unsupported` from `dockerOnly` (`TestKubernetesPlanBlockersOverTheAPI`, `TestKubernetesApplyNeedsTheCapability`, `TestRuntimeGateMatrix`). The new hello rows already pass: they pin Task 1's `CapabilitiesFit` at the socket.

- [ ] **Step 3: Implement**

In `internal/api/runtime_gate.go`:

Replace

```go
)

// dockerOnly refuses, with 409 runtime_unsupported, an action on containers, images,
// networks, volumes, exec, inspection or deployments aimed at an endpoint that is not Docker.
// It runs before any capability check, so the answer names the runtime rather than asking for
// an agent upgrade that would not help. It writes the response and reports false on refusal.
func (s *Server) dockerOnly(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpointID string) bool {
	e, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpointID)
	if err != nil {
```

with

```go
)

// routeKind is which runtimes a route serves.
type routeKind int

const (
	// dockerRoute acts on containers, images, exec, inspection, commands or adoption: Docker only.
	dockerRoute routeKind = iota
	// applicationRoute maps, plans, applies or removes an application on either runtime; the
	// store holds each instance to its endpoint's runtime.
	applicationRoute
)

// runtimeGate refuses, with 409 runtime_unsupported, a Docker route aimed at an endpoint that is
// not Docker. It runs before any capability check, so the answer names the runtime rather than
// asking for an agent upgrade that would not help. It writes the response and reports false on
// refusal.
func (s *Server) runtimeGate(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpointID string, kind routeKind) bool {
	e, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpointID)
	if err != nil {
```

Replace

```go
		return false
	}
	if e.Runtime != protocol.RuntimeDocker {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return false
```

with

```go
		return false
	}
	if kind == dockerRoute && e.Runtime != protocol.RuntimeDocker {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return false
```

In `internal/api/log_handlers.go`:

Replace

```go
		return
	}
	if !s.dockerOnly(w, r, a, id) {
		return
	}
```

with

```go
		return
	}
	if !s.runtimeGate(w, r, a, id, dockerRoute) {
		return
	}
```

In `internal/api/inspection.go`:

Replace

```go
		return
	}
	// Inspection's permission is endpoint.read, which dockerOnly's endpoint read checks.
	if !s.dockerOnly(w, r, a, endpoint) {
		return
	}
```

with

```go
		return
	}
	// Inspection's permission is endpoint.read, which runtimeGate's endpoint read checks.
	if !s.runtimeGate(w, r, a, endpoint, dockerRoute) {
		return
	}
```

In `internal/api/exec_handlers.go`:

Replace

```go
		return
	}
	if !s.dockerOnly(w, r, a, endpoint) {
		return
	}
```

with

```go
		return
	}
	if !s.runtimeGate(w, r, a, endpoint, dockerRoute) {
		return
	}
```

In `internal/api/endpoint_handlers.go`:

Replace

```go
		s.tenantError(w, err)
		return
	}
	if !s.dockerOnly(w, r, a, id) {
		return
	}
	var body struct {
```

with

```go
		s.tenantError(w, err)
		return
	}
	if !s.runtimeGate(w, r, a, id, dockerRoute) {
		return
	}
	var body struct {
```

Replace

```go
		s.tenantError(w, err)
		return
	}
	if !s.dockerOnly(w, r, a, id) {
		return
	}
	container := r.PathValue("container")
```

with

```go
		s.tenantError(w, err)
		return
	}
	if !s.runtimeGate(w, r, a, id, dockerRoute) {
		return
	}
	container := r.PathValue("container")
```

In `internal/api/application_handlers.go`:

Replace

```go
func (s *Server) handleAdoptionPreview(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if endpoint := r.URL.Query().Get("endpoint"); endpoint != "" && !s.dockerOnly(w, r, a, endpoint) {
		return
	}
```

with

```go
func (s *Server) handleAdoptionPreview(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if endpoint := r.URL.Query().Get("endpoint"); endpoint != "" && !s.runtimeGate(w, r, a, endpoint, dockerRoute) {
		return
	}
```

Replace

```go
		return
	}
	if input.EndpointID != "" && !s.dockerOnly(w, r, a, input.EndpointID) {
		return
	}
```

with

```go
		return
	}
	if input.EndpointID != "" && !s.runtimeGate(w, r, a, input.EndpointID, dockerRoute) {
		return
	}
```

Replace

```go
		return
	}
	// An instance that cannot be read is the store's to refuse, with its own answer.
	if instance, err := s.store.Tenancy().ReadApplicationInstance(r.Context(), a, r.PathValue("application"), input.InstanceID); err == nil && !s.dockerOnly(w, r, a, instance.EndpointID) {
		return
	}
```

with

```go
		return
	}
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

Replace

```go
// maxFrameBytes is the largest deployment frame an agent with capabilities accepts.
func maxFrameBytes(capabilities []string) int {
	if slices.Contains(capabilities, protocol.CapabilityDeploymentPull) {
		return protocol.MaxDeploymentRequestBytes
	}
```

with

```go
// maxFrameBytes is the largest deployment frame an agent with capabilities accepts.
func maxFrameBytes(capabilities []string) int {
	if slices.Contains(capabilities, protocol.CapabilityDeploymentPull) || slices.Contains(capabilities, protocol.CapabilityKubernetesDeploy) {
		return protocol.MaxDeploymentRequestBytes
	}
```

Replace

```go
		return
	}
	if len(input.Update) > 0 {
		id, err := uuid.Parse(r.PathValue("application"))
		if err != nil {
```

with

```go
		return
	}
	// A Kubernetes plan resolves every image at the registry, so it runs as an update does.
	instance, err := s.store.Tenancy().ReadApplicationInstance(r.Context(), a, r.PathValue("application"), input.InstanceID)
	kube := err == nil && instance.Namespace != ""
	if err == nil && !s.runtimeGate(w, r, a, instance.EndpointID, applicationRoute) {
		return
	}
	if len(input.Update) > 0 || kube {
		id, err := uuid.Parse(r.PathValue("application"))
		if err != nil {
```

Replace

```go
		defer done()
	}
	// Before the preflight, which refuses a non-Docker inventory as a changed adoption.
	if instance, err := s.store.Tenancy().ReadApplicationInstance(r.Context(), a, r.PathValue("application"), input.InstanceID); err == nil && !s.dockerOnly(w, r, a, instance.EndpointID) {
		return
	}
	// The plan measures its frame against what this endpoint's agent accepts.
	pre, err := s.store.Tenancy().PreflightApplication(r.Context(), a, r.PathValue("application"))
```

with

```go
		defer done()
	}
	// The plan measures its frame against what this endpoint's agent accepts.
	pre, err := s.store.Tenancy().PreflightApplication(r.Context(), a, r.PathValue("application"))
```

Replace

```go
	// Inspect before taking a registry slot, so slow agents cannot hold the organization's slots.
	input.Inspections = s.planInspections(w, r, a, ep, pre)
	if len(input.Update) > 0 {
		release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
		if !ok {
```

with

```go
	// Inspect before taking a registry slot, so slow agents cannot hold the organization's slots.
	input.Inspections = s.planInspections(w, r, a, ep, pre)
	if len(input.Update) > 0 || kube {
		release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
		if !ok {
```

Replace

```go
		return
	}
	if ep.Runtime != protocol.RuntimeDocker {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return
	}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityDeploymentApply) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable deployments")
		return
	}
	pulls := slices.ContainsFunc(plan.Plan.Services, func(ps store.PlannedService) bool { return ps.PullDigest != "" })
	if pulls && !slices.Contains(ep.Capabilities, protocol.CapabilityDeploymentPull) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable deployments that pull images")
		return
```

with

```go
		return
	}
	// A plan is for its endpoint's runtime: a Kubernetes plan names a namespace, a Docker plan none.
	kube := ep.Runtime == protocol.RuntimeKubernetes
	if kube != (plan.Plan.Namespace != "") {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return
	}
	deploys := protocol.CapabilityDeploymentApply
	if kube {
		deploys = protocol.CapabilityKubernetesDeploy
	}
	if !slices.Contains(ep.Capabilities, deploys) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable deployments")
		return
	}
	pulls := slices.ContainsFunc(plan.Plan.Services, func(ps store.PlannedService) bool { return ps.PullDigest != "" })
	if pulls && !kube && !slices.Contains(ep.Capabilities, protocol.CapabilityDeploymentPull) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable deployments that pull images")
		return
```

Replace

```go
		return
	}
	if ep.Runtime != protocol.RuntimeDocker {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return
	}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityDeploymentRemove) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable application removal")
		return
```

with

```go
		return
	}
	kube := ep.Runtime == protocol.RuntimeKubernetes
	if kube != (instance.Namespace != "") {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return
	}
	removes := protocol.CapabilityDeploymentRemove
	if kube {
		removes = protocol.CapabilityKubernetesRemove
	}
	if !slices.Contains(ep.Capabilities, removes) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable application removal")
		return
```

In `internal/api/tenant_handlers.go`:

Replace

```go
	case errors.Is(err, store.ErrRuntimeUnsupported):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This endpoint's runtime does not support this action", "code": "runtime_unsupported"})
	case errors.Is(err, store.ErrMappingRequired):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Map the application's services to adopted containers first", "code": "mapping_required"})
```

with

```go
	case errors.Is(err, store.ErrRuntimeUnsupported):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This endpoint's runtime does not support this action", "code": "runtime_unsupported"})
	case errors.Is(err, store.ErrNamespaceUnknown):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The cluster's manifest does not grant that namespace", "code": "namespace_unknown"})
	case errors.Is(err, store.ErrMappingRequired):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Map the application's services to adopted containers first", "code": "mapping_required"})
```

- [ ] **Step 4: Run the tests to verify they pass, on both databases**

Run: `gofmt -w internal/api && go vet ./... && go test -race -count=1 ./internal/api/ && PG=… go test -count=1 ./internal/api/`
Expected: `ok` twice; `TestDockerRoutesRefuseAKubernetesEndpoint` still passes (a Docker-adopted application on a cluster is refused by the store and the handlers).

- [ ] **Step 5: DOX and commit**

In `internal/api/AGENTS.md`, replace the bullet beginning `- \`dockerOnly\` (\`runtime_gate.go\`)` with:

```markdown
- `runtimeGate(kind)` (`runtime_gate.go`) reads the endpoint under `endpoint.read`. `dockerRoute` answers 409 `runtime_unsupported` for a non-Docker endpoint before any capability check: commands, removal preview, inspection, container logs, exec (before the upgrade), adoption preview and adoption by the named endpoint. `applicationRoute` (service mapping by the body's `endpoint_id` or the instance's endpoint, plans by the instance's endpoint) accepts both runtimes; the store holds each instance to its endpoint's runtime (`ErrRuntimeUnsupported`), and apply and application removal refuse a plan or instance of the other runtime's shape (a namespace on Docker, none on a cluster) with the same 409. Exec and inspection run the gate after their per-actor rate limit and permission check (`CheckExecAccess`; inspection's `endpoint.read` is the gate's own endpoint read), so a member without the permission gets 403 and a `denied` audit row, not the runtime; the other routes run it before their action's own permission check. Endpoint, inventory, samples, rollups and the command list stay readable.
- On a Kubernetes instance, a plan runs as an image update does (`CheckImageUpdateAccess`, the application guard, a registry slot), since the store resolves every image at the registry; apply requires `kubernetes.deploy` and removal `kubernetes.remove` (501 without), `maxFrameBytes` is the full cap for `kubernetes.deploy`, and the frames are the store's (`deployment.apply` with `kubernetes`, `deployment.remove` with `kubernetes` and `services`). `ErrNamespaceUnknown` is 400 `namespace_unknown`.
```

and in the bullet beginning `- The agent socket knows the endpoint's runtime`, replace `a Kubernetes endpoint names only \`kubernetes.inventory\`/\`pod.logs\`, a Docker endpoint neither` with `a Kubernetes endpoint names only \`kubernetes.inventory\`, \`pod.logs\`, \`kubernetes.deploy\` and \`kubernetes.remove\` (so \`deployment.pull\` from a cluster is refused), a Docker endpoint none of them`.

```bash
git add internal/api && make tidy-check lint && git commit -m "feat(api): application routes on Kubernetes endpoints (runtimeGate)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Web — mapping card, manifest regeneration, code tables and the cluster's applications

**Files:**
- Create: `web/src/components/KubernetesManifest.tsx` (`ManifestRegeneration`, `downloadManifest`), `web/src/components/KubernetesMapping.tsx` (`KubernetesMapping`, `KubernetesWorkloads`, `KUBERNETES_UNVALIDATED`)
- Test: `web/src/components/KubernetesManifest.test.tsx`, `web/src/components/KubernetesMapping.test.tsx` (new)
- Modify: `web/src/tenant.ts` (types, `canEnroll`, `parseNamespaces`)
- Modify: `web/src/components/ApplicationAdoption.tsx` (a cluster instance, the mapping card, Docker hosts only in the host list, removal text), `Applications.tsx` (Deployments in place of the Docker mapping and comparison for a cluster instance)
- Modify: `web/src/components/ApplicationPreflight.tsx`, `ApplicationInspection.tsx` (blocker and `k8s_` sentences), `ApplicationDeploymentPlan.tsx` (step codes, objects, identities, apply text), `ApplicationValidation.tsx` (the Kubernetes sentence)
- Modify: `web/src/components/KubernetesCluster.tsx` (Applications section), `web/src/pages/EndpointPage.tsx` (props), `web/src/components/Endpoints.tsx` (namespaces at enrollment)
- Test: `web/src/components/ApplicationDeploymentPlan.test.tsx`, `ApplicationValidation.test.tsx`, `Endpoints.test.tsx`, `web/src/pages/EndpointPage.test.tsx`
- Build: `web/dist`, `web/tsconfig.tsbuildinfo`
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes: the API of Tasks 6 and 7 (`deploy_namespaces` on endpoints, `namespace` on instances, `PUT .../mapping {endpoint_id, namespace}`, `POST .../endpoints/{endpoint}/manifest`, `object` and `namespace` on plans, Deployment identities, `Workload.instance`), existing `secureFetch`, `useTenantResource`, `refusal`, `StateNotice`, `displayName`, `knownBlockers`, `serviceFindings`.
- Produces (TypeScript):
  ```ts
  export const canEnroll: (role: string | undefined) => boolean      // organization or environment admin
  export const parseNamespaces: (text: string) => string[]
  export function ManifestRegeneration(props: { org: string; endpoint: Endpoint; onSaved: () => void }): JSX.Element
  export function downloadManifest(file: string, text: string): void
  export function KubernetesMapping(props: { base: string; org: string; env: string; instance?: ApplicationInstance; admin: boolean; onChanged: (status?: string) => void }): JSX.Element | null
  export function KubernetesWorkloads(props: { org: string; instance: ApplicationInstance }): JSX.Element
  export const KUBERNETES_UNVALIDATED: string; export const KUBERNETES_UNVERIFIED: string
  // ValidationLine and verdictText take kubernetes?: boolean; KubernetesCluster takes org, instances, admin, onChanged
  // ApplicationInstance.namespace?: string; Endpoint.deploy_namespaces?: string[]; Workload.application?/instance?
  ```

- [ ] **Step 1: Write the failing tests**

Create `web/src/components/KubernetesMapping.test.tsx`:

```tsx
import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { KubernetesMapping, KubernetesWorkloads, KUBERNETES_UNVALIDATED } from './KubernetesMapping';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
const cluster = { id: 'ep_k', name: 'prod', runtime: 'kubernetes', state: 'active', deploy_namespaces: ['billing', 'shop'] };
const host = { id: 'ep_d', name: 'docker', runtime: 'docker', state: 'active' };

it('maps to a namespace the cluster grants, and only to a cluster', async () => {
  document.cookie = 'ky_csrf=csrf';
  const changed = vi.fn();
  const fetcher = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => init?.method === 'PUT' ? new Response(null, { status: 204 }) : json([host, cluster]));
  vi.stubGlobal('fetch', fetcher);
  render(<KubernetesMapping base="/app" org="a" env="e" admin={false} onChanged={changed} />);
  const select = await screen.findByLabelText('Cluster');
  expect([...select.querySelectorAll('option')].map((o) => o.textContent)).toEqual(['Choose a cluster', 'prod · active']);
  fireEvent.change(select, { target: { value: 'ep_k' } });
  const save = screen.getByRole('button', { name: 'Save namespace mapping' });
  expect(save.hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Namespace'), { target: { value: 'shop' } });
  fireEvent.click(save);
  await vi.waitFor(() => expect(changed).toHaveBeenCalledWith('Mapped to namespace shop. Nothing was deployed.'));
  const put = fetcher.mock.calls.find((c) => c[1]?.method === 'PUT');
  expect(String(put?.[0])).toBe('/app/mapping');
  expect(JSON.parse(String(put?.[1]?.body))).toEqual({ endpoint_id: 'ep_k', namespace: 'shop' });
  expect(new Headers(put?.[1]?.headers).get('X-CSRF-Token')).toBe('csrf');
  expect(screen.queryByRole('button', { name: 'Regenerate manifest' })).toBeNull();
});

it('explains a namespace the manifest does not grant, and offers administrators the manifest', async () => {
  vi.stubGlobal('fetch', vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => init?.method === 'PUT' ? json({ code: 'namespace_unknown', error: 'secret-canary' }, 400) : json([cluster])));
  render(<KubernetesMapping base="/app" org="a" env="e" admin onChanged={vi.fn()} />);
  fireEvent.change(await screen.findByLabelText('Cluster'), { target: { value: 'ep_k' } });
  fireEvent.change(screen.getByLabelText('Namespace'), { target: { value: 'billing' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save namespace mapping' }));
  expect((await screen.findByRole('alert')).textContent).toContain("The cluster's manifest does not grant that namespace.");
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(screen.getByRole('button', { name: 'Regenerate manifest' })).toBeTruthy();
});

it('renders nothing without a cluster', async () => {
  const fetcher = vi.fn(async () => json([host]));
  vi.stubGlobal('fetch', fetcher);
  const { container } = render(<KubernetesMapping base="/app" org="a" env="e" admin onChanged={vi.fn()} />);
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalled());
  await vi.waitFor(() => expect(container.textContent).toBe(''));
});

it("lists the instance's Deployments from the cluster inventory and says health is not validated", async () => {
  const instance = { id: 'i1', application_id: 'app', endpoint_id: 'ep_k', endpoint_name: 'prod', project: 'shop', namespace: 'shop', revision: 1, current_revision: 1, previous_revision: 0, mapping_version: 1, container_count: 0, containers: [] };
  const workloads = [
    { kind: 'Deployment', namespace: 'shop', name: 'shop-web', desired: 1, ready: 1, updated: 1, images: ['ghcr.io/org/web@sha256:abc'], paused: false, instance: 'i1' },
    { kind: 'Deployment', namespace: 'shop', name: 'foreign', desired: 1, ready: 0, updated: 1, images: ['x'], paused: false },
  ];
  vi.stubGlobal('fetch', vi.fn(async () => json({ snapshot: { kubernetes: { workloads } } })));
  render(<KubernetesWorkloads org="a" instance={instance} />);
  expect(await screen.findByText('shop-web')).toBeTruthy();
  expect(screen.getByText('1/1')).toBeTruthy();
  expect(screen.queryByText('foreign')).toBeNull();
  expect(screen.getByText(KUBERNETES_UNVALIDATED)).toBeTruthy();
});
```

Create `web/src/components/KubernetesManifest.test.tsx`:

```tsx
import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ManifestRegeneration } from './KubernetesManifest';
import type { Endpoint } from '../tenant';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const endpoint = { id: 'ep_k', name: 'prod', runtime: 'kubernetes', state: 'active', deploy_namespaces: ['shop'] } as Endpoint;

it('posts the typed namespaces and shows the manifest to apply', async () => {
  const saved = vi.fn();
  const fetcher = vi.fn(async () => json({ manifest: 'kind: Role', manifest_file: 'kyyard-agent-prod.yaml', command: 'kubectl apply -f kyyard-agent-prod.yaml', namespaces: ['billing', 'shop'], note: 'Create the namespaces first.' }));
  vi.stubGlobal('fetch', fetcher);
  render(<ManifestRegeneration org="a" endpoint={endpoint} onSaved={saved} />);
  fireEvent.click(screen.getByRole('button', { name: 'Regenerate manifest' }));
  const input = screen.getByLabelText('Namespaces to deploy to');
  expect(input).toHaveProperty('value', 'shop');
  fireEvent.change(input, { target: { value: 'shop, billing  ' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save and show manifest' }));
  const region = await screen.findByRole('region', { name: 'Namespace manifest' });
  expect(region.textContent).toContain('kubectl apply -f kyyard-agent-prod.yaml');
  expect(region.textContent).toContain('Create the namespaces first.');
  const [url, init] = fetcher.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe('/api/organizations/a/endpoints/ep_k/manifest');
  expect(JSON.parse(String(init.body))).toEqual({ namespaces: ['shop', 'billing'] });
  expect(saved).toHaveBeenCalledOnce();
});

it('names a refused list in fixed text', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => json({ error: 'secret-canary' }, 400)));
  render(<ManifestRegeneration org="a" endpoint={endpoint} onSaved={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Regenerate manifest' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save and show manifest' }));
  expect((await screen.findByRole('alert')).textContent).toContain('List at most 32 namespaces');
  expect(document.body.textContent).not.toContain('secret-canary');
});
```

In `web/src/components/ApplicationDeploymentPlan.test.tsx`:

Replace

```tsx
import { ApplicationDeploymentPlan } from './ApplicationDeploymentPlan';
import { messages } from './ApplicationPreflight';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.useRealTimers(); });
const mapping = { instance_id: 'i', version: 3, mapped_revision: 2, services: [], bindings: {}, preview: { revision: 2, digest: 'd', project: 'shop', endpoint_name: 'Docker', containers: [] } };
```

with

```tsx
import { ApplicationDeploymentPlan } from './ApplicationDeploymentPlan';
import { messages } from './ApplicationPreflight';
import { KUBERNETES_UNVERIFIED } from './ApplicationValidation';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.useRealTimers(); });
const mapping = { instance_id: 'i', version: 3, mapped_revision: 2, services: [], bindings: {}, preview: { revision: 2, digest: 'd', project: 'shop', endpoint_name: 'Docker', containers: [] } };
```

Replace

```tsx
  expect(screen.getAllByText(/Rollback sent to revision 1\./).length).toBeGreaterThan(0);
});
```

with

```tsx
  expect(screen.getAllByText(/Rollback sent to revision 1\./).length).toBeGreaterThan(0);
});

it('renders a Kubernetes plan, its step codes and Deployment identities', async () => {
  const uid = '0f1e2d3c-4b5a-4968-8776-655443322110';
  const kube = { ...plan, state: 'timed_out', plan: { project: 'shop', namespace: 'shop', services: [{ ...plan.plan.services[0], image_id: '', container_id: '', pull_digest: `sha256:${'d'.repeat(64)}`, object: { namespace: 'shop', name: 'shop-web' } }] },
    validation: { deployment_id: 'd1', policy_run_id: '', automated: false, is_rollback: false, phase: 'done', started_at: '', observe_until: '', verdict: 'unverifiable', detail: 'the agent cannot report container health', rollback: null, correlation_id: 'c', finished_at: null },
    result: { code: 'step_failed', steps: [
      { service: 'web', step: 'precondition', outcome: 'denied', code: 'name_taken', detail: 'Deployment/shop-web' },
      { service: 'web', step: 'start', outcome: 'timed_out', code: 'rollout_timeout', detail: 'progressing=ProgressDeadlineExceeded,pod=ImagePullBackOff' },
      { service: 'api', step: 'start', outcome: 'timed_out', code: 'rollout_timeout', detail: 'pod=back-off <b>secret-canary</b>' },
      { service: 'api', step: 'create', outcome: 'denied', code: 'forbidden', detail: '' },
    ], services: [{ service: 'web', container_id: '', image_id: '', created_unix: 0, kind: 'Deployment', namespace: 'shop', name: 'shop-web', uid }] } };
  vi.stubGlobal('fetch', stubFetch([kube]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect(await screen.findByText('Something else already holds that name: Deployment/shop-web.')).toBeTruthy();
  expect(screen.getByText('The Deployment did not become ready before the deadline; it stays as applied (progressing ProgressDeadlineExceeded, pod ImagePullBackOff).')).toBeTruthy();
  expect(screen.getByText('The Deployment did not become ready before the deadline; it stays as applied.')).toBeTruthy();
  expect(screen.getByText("The agent's access in the namespace does not allow this; apply the cluster's regenerated manifest.")).toBeTruthy();
  expect(screen.getAllByText('Deployment shop/shop-web').length).toBeGreaterThan(0);
  expect(screen.getByText(uid)).toBeTruthy();
  expect(screen.getByText(KUBERNETES_UNVERIFIED)).toBeTruthy();
  expect(document.body.textContent).not.toContain('secret-canary');
});
it('names each service a Kubernetes plan refuses, with the fix', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    if (init?.method === 'POST') return new Response(JSON.stringify({ code: 'preflight_blocked', blockers: ['kubernetes_unsupported', 'k8s_namespace'], services: [{ name: 'db', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_volume'] }, { name: 'web', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_host_ip', 'k8s_restart'] }] }), { status: 409 });
    return new Response('[]');
  }));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  fireEvent.change(screen.getByLabelText('Confirm plan project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan deployment' }));
  const alert = await screen.findByRole('alert');
  expect(alert.textContent).toContain(messages.kubernetes_unsupported);
  expect(alert.textContent).toContain(messages.k8s_namespace);
  expect(alert.textContent).toContain('db: mounts a volume; Kubernetes deployment of stateful services arrives with the migration analyzer');
  expect(alert.textContent).toContain('web: publishes a port on a host address');
});
```

In `web/src/components/ApplicationValidation.test.tsx`:

Replace

```tsx
import { afterEach, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { ROLLBACK_REASONS, VALIDATION_VERDICTS, ValidationLine, pauseText, reasonText } from './ApplicationValidation';
import type { Validation } from '../tenant';
afterEach(cleanup);
```

with

```tsx
import { afterEach, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { KUBERNETES_UNVERIFIED, ROLLBACK_REASONS, VALIDATION_VERDICTS, ValidationLine, pauseText, reasonText } from './ApplicationValidation';
import type { Validation } from '../tenant';
afterEach(cleanup);
```

Replace

```tsx
  for (const code of emitted) expect(Object.hasOwn(ROLLBACK_REASONS, code)).toBe(true);
});
```

with

```tsx
  for (const code of emitted) expect(Object.hasOwn(ROLLBACK_REASONS, code)).toBe(true);
});

it('gives a Kubernetes apply the one unverifiable sentence instead of the upgrade advice', () => {
  cleanup();
  const text = render(<ValidationLine v={validation({ verdict: 'unverifiable', detail: 'the agent cannot report container health' })} kubernetes />).container.textContent ?? '';
  expect(text).toBe(KUBERNETES_UNVERIFIED);
  expect(text).not.toContain('upgrade');
  expect(line({ verdict: 'unverifiable', detail: 'the agent cannot report container health' })).toContain('upgrade it');
});
```

In `web/src/components/Endpoints.test.tsx`:

Replace

```tsx
  expect(screen.getByText('v1.36.0')).toBeTruthy();
});
```

with

```tsx
  expect(screen.getByText('v1.36.0')).toBeTruthy();
});

it('sends the namespaces a cluster may deploy to with its enrollment', async () => {
  let posted = '';
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'POST') { posted = String(init.body); return json({ id: 't1', runtime: 'kubernetes', expires_at: '2026-09-16T00:15:00Z', token: 'tok', command: 'kubectl apply -f x.yaml', manifest: 'kind: Role', manifest_file: 'x.yaml', disclosure: 'd', namespaces: ['billing', 'shop'] }, 201); }
    return json([]);
  }));
  render(<Endpoints org="a" env="env-a" />);
  fireEvent.change(screen.getByRole('combobox', { name: 'Runtime' }), { target: { value: 'kubernetes' } });
  fireEvent.change(screen.getByRole('textbox', { name: 'Cluster name' }), { target: { value: 'prod' } });
  fireEvent.change(screen.getByRole('textbox', { name: 'Namespaces to deploy to' }), { target: { value: 'shop billing' } });
  fireEvent.click(screen.getByRole('button', { name: 'Enroll a cluster' }));
  await screen.findByRole('region', { name: 'Enrollment manifest' });
  expect(JSON.parse(posted)).toEqual({ runtime: 'kubernetes', name: 'prod', namespaces: ['shop', 'billing'] });
});
```

In `web/src/pages/EndpointPage.test.tsx`:

Replace

```tsx
});

it('renders a Kubernetes cluster read-only, filters by namespace and opens pod logs on the pod route', async () => {
  const now = new Date().toISOString();
  const cluster = { ...endpoint, runtime: 'kubernetes', capabilities: ['kubernetes.inventory', 'pod.logs'], cluster_health: 'degraded' };
  const kubernetes = {
    nodes: [{ name: 'control-1', kubelet_version: 'v1.36.0', os: 'linux', arch: 'amd64', ready: true, roles: ['control-plane'], unschedulable: false }, { name: 'worker-1', kubelet_version: 'v1.36.0', os: 'linux', arch: 'arm64', ready: false, roles: [], unschedulable: true }],
    namespaces: ['kube-system', 'shop'],
    workloads: [{ kind: 'Deployment', namespace: 'shop', name: 'web', desired: 3, ready: 2, updated: 3, images: ['nginx:1.29'], paused: false }, { kind: 'DaemonSet', namespace: 'kube-system', name: 'proxy', desired: 2, ready: 2, updated: 2, images: ['kube-proxy:1'], paused: false }],
    pods: [{ namespace: 'shop', name: 'web-7c9', phase: 'Running', node: 'worker-1', owner_kind: 'Deployment', owner_name: 'web', started_at: now, containers: [{ name: 'web', image: 'nginx:1.29', image_id: '', state: 'running', reason: '', ready: true, restart_count: 2 }, { name: 'log', image: 'busybox:1', image_id: '', state: 'waiting', reason: 'CrashLoopBackOff', ready: false, restart_count: 5 }] },
      { namespace: 'kube-system', name: 'proxy-x', phase: 'Running', node: 'control-1', owner_kind: 'DaemonSet', owner_name: 'proxy', started_at: now, containers: [{ name: 'proxy', image: 'kube-proxy:1', image_id: '', state: 'running', reason: '', ready: true, restart_count: 0 }] }],
```

with

```tsx
});

it('renders a Kubernetes cluster, its mapped applications, filters by namespace and opens pod logs on the pod route', async () => {
  const now = new Date().toISOString();
  const cluster = { ...endpoint, runtime: 'kubernetes', capabilities: ['kubernetes.inventory', 'pod.logs', 'kubernetes.deploy'], cluster_health: 'degraded', deploy_namespaces: ['shop'] };
  const mapped = { id: 'i1', application_id: 'app', endpoint_id: 'ep_1', endpoint_name: 'host-1', project: 'storefront', namespace: 'shop', revision: 1, current_revision: 1, previous_revision: 0, mapping_version: 1, container_count: 0, containers: [] };
  const kubernetes = {
    nodes: [{ name: 'control-1', kubelet_version: 'v1.36.0', os: 'linux', arch: 'amd64', ready: true, roles: ['control-plane'], unschedulable: false }, { name: 'worker-1', kubelet_version: 'v1.36.0', os: 'linux', arch: 'arm64', ready: false, roles: [], unschedulable: true }],
    namespaces: ['kube-system', 'shop'],
    workloads: [{ kind: 'Deployment', namespace: 'shop', name: 'web', desired: 3, ready: 2, updated: 3, images: ['nginx:1.29'], paused: false, application: 'app', instance: 'i1' }, { kind: 'DaemonSet', namespace: 'kube-system', name: 'proxy', desired: 2, ready: 2, updated: 2, images: ['kube-proxy:1'], paused: false }],
    pods: [{ namespace: 'shop', name: 'web-7c9', phase: 'Running', node: 'worker-1', owner_kind: 'Deployment', owner_name: 'web', started_at: now, containers: [{ name: 'web', image: 'nginx:1.29', image_id: '', state: 'running', reason: '', ready: true, restart_count: 2 }, { name: 'log', image: 'busybox:1', image_id: '', state: 'waiting', reason: 'CrashLoopBackOff', ready: false, restart_count: 5 }] },
      { namespace: 'kube-system', name: 'proxy-x', phase: 'Running', node: 'control-1', owner_kind: 'DaemonSet', owner_name: 'proxy', started_at: now, containers: [{ name: 'proxy', image: 'kube-proxy:1', image_id: '', state: 'running', reason: '', ready: true, restart_count: 0 }] }],
```

Replace

```tsx
    if (url.includes('/pods/')) return new Response('ready\n', { status: 200 });
    if (url.endsWith('/inventory')) return json({ endpoint_id: 'ep_1', state: 'active', generation: 3, observed_at: now, received_at: now, snapshot });
    if (url.endsWith('/samples') || url.includes('/commands') || url.endsWith('/applications') || url === '/api/organizations') return json([]);
    return json(cluster);
  }));
```

with

```tsx
    if (url.includes('/pods/')) return new Response('ready\n', { status: 200 });
    if (url.endsWith('/inventory')) return json({ endpoint_id: 'ep_1', state: 'active', generation: 3, observed_at: now, received_at: now, snapshot });
    if (url.endsWith('/applications')) return json([mapped]);
    if (url.endsWith('/samples') || url.includes('/commands') || url === '/api/organizations') return json([]);
    return json(cluster);
  }));
```

Replace

```tsx
  expect(screen.queryByText('Actions')).toBeNull();
  expect(screen.queryByText(/Pull an image/)).toBeNull();
  expect(screen.getByRole('region', { name: 'Applications' }).textContent).toContain('Kubernetes deployment arrives in a later release');
  expect(screen.getByText('2/3')).toBeTruthy();
  expect(screen.getByText('proxy-x', { exact: false })).toBeTruthy();
```

with

```tsx
  expect(screen.queryByText('Actions')).toBeNull();
  expect(screen.queryByText(/Pull an image/)).toBeNull();
  const applications = screen.getByRole('region', { name: 'Applications' }).textContent;
  expect(applications).toContain('KyYard deploys only to shop');
  expect(applications).toContain('storefront · shop · 0 of 1 Deployments ready');
  // Only an administrator is offered the manifest.
  expect(screen.queryByRole('button', { name: 'Regenerate manifest' })).toBeNull();
  expect(screen.getByText('2/3')).toBeTruthy();
  expect(screen.getByText('proxy-x', { exact: false })).toBeTruthy();
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm ci && npm test; cd ..`
Expected: FAIL: `KubernetesMapping.test.tsx` and `KubernetesManifest.test.tsx` cannot resolve their components, `KUBERNETES_UNVERIFIED` is not exported, and the cluster page still says deployment arrives later.

- [ ] **Step 3: Implement**

In `web/src/tenant.ts`:

Replace

```ts
export interface Member { user_id: string; username: string; role: string; status: string }
export interface EndpointAlert { id: number; kind: string; details: string; created_at: string }
export interface Endpoint { id: string; environment_id: string; name: string; runtime: string; state: string; facts: Record<string, string>; fingerprint: string; pending_fingerprint?: string; capabilities: string[]; alerts: EndpointAlert[]; created_at: string; approved_by?: string; cluster_health?: 'healthy' | 'degraded' | 'unknown' }
export interface EnrollmentToken { id: string; runtime: string; expires_at: string; token: string; command?: string; image?: string; note?: string; disclosure: string; manifest?: string; manifest_file?: string }
export interface Port { host_ip?: string; host?: number; container: number; protocol: string }
export interface Container { id: string; name: string; image: string; image_id: string; state: string; status: string; created_at: string; ports: Port[]; labels: Record<string, string>; networks: string[]; compose_project?: string }
```

with

```ts
export interface Member { user_id: string; username: string; role: string; status: string }
export interface EndpointAlert { id: number; kind: string; details: string; created_at: string }
export interface Endpoint { id: string; environment_id: string; name: string; runtime: string; state: string; facts: Record<string, string>; fingerprint: string; pending_fingerprint?: string; capabilities: string[]; alerts: EndpointAlert[]; created_at: string; approved_by?: string; cluster_health?: 'healthy' | 'degraded' | 'unknown'; deploy_namespaces?: string[] }
export interface EnrollmentToken { id: string; runtime: string; expires_at: string; token: string; command?: string; image?: string; note?: string; disclosure: string; manifest?: string; manifest_file?: string; namespaces?: string[] }
export interface Port { host_ip?: string; host?: number; container: number; protocol: string }
export interface Container { id: string; name: string; image: string; image_id: string; state: string; status: string; created_at: string; ports: Port[]; labels: Record<string, string>; networks: string[]; compose_project?: string }
```

Replace

```ts
// A cluster agent's inventory (protocol.KubernetesInventory); started_at is year 1 when unknown.
export interface KubeNode { name: string; kubelet_version: string; os: string; arch: string; ready: boolean; roles: string[]; unschedulable: boolean }
export interface Workload { kind: string; namespace: string; name: string; desired: number; ready: number; updated: number; images: string[]; paused: boolean }
export interface PodContainer { name: string; image: string; image_id: string; state: string; reason: string; ready: boolean; restart_count: number }
export interface Pod { namespace: string; name: string; phase: string; node: string; owner_kind: string; owner_name: string; started_at: string; containers: PodContainer[] }
```

with

```ts
// A cluster agent's inventory (protocol.KubernetesInventory); started_at is year 1 when unknown.
export interface KubeNode { name: string; kubelet_version: string; os: string; arch: string; ready: boolean; roles: string[]; unschedulable: boolean }
// application and instance are KyYard's labels on a Deployment it applied.
export interface Workload { kind: string; namespace: string; name: string; desired: number; ready: number; updated: number; images: string[]; paused: boolean; application?: string; instance?: string }
export interface PodContainer { name: string; image: string; image_id: string; state: string; reason: string; ready: boolean; restart_count: number }
export interface Pod { namespace: string; name: string; phase: string; node: string; owner_kind: string; owner_name: string; started_at: string; containers: PodContainer[] }
```

Replace

```ts
export const tenantRoles = ['organization_admin', 'environment_admin', 'operator', 'developer', 'read_only'] as const;
// Mirrors permissions.Allows(role, ContainerExec): only organization admins may exec.
export const canExec = (role: string | undefined) => role === 'organization_admin';
```

with

```ts
export const tenantRoles = ['organization_admin', 'environment_admin', 'operator', 'developer', 'read_only'] as const;
// Mirrors permissions.Allows(role, EndpointEnroll): organization and environment admins enroll
// endpoints and regenerate a cluster's manifest.
export const canEnroll = (role: string | undefined) => role === 'organization_admin' || role === 'environment_admin';

// parseNamespaces reads a typed namespace list: names split on commas and whitespace, empties
// dropped. The server sorts, deduplicates and validates.
export const parseNamespaces = (text: string) => text.split(/[\s,]+/).filter(Boolean);

// Mirrors permissions.Allows(role, ContainerExec): only organization admins may exec.
export const canExec = (role: string | undefined) => role === 'organization_admin';
```

Create `web/src/components/KubernetesManifest.tsx`:

```tsx
import { useState } from 'react';
import { secureFetch } from '../api';
import { offlineWrite, parseNamespaces, refusal, type Endpoint } from '../tenant';

type Manifest = { manifest: string; manifest_file: string; command: string; namespaces: string[]; note: string };

// downloadManifest saves text as file through a temporary object URL.
export function downloadManifest(file: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'application/yaml' }));
  const link = document.createElement('a');
  link.href = url;
  link.download = file;
  link.click();
  URL.revokeObjectURL(url);
}

// ManifestRegeneration records the namespaces a cluster's agent may deploy to and shows the
// RBAC-only manifest that grants them, for a cluster-admin to apply. The server audits it.
export function ManifestRegeneration({ org, endpoint, onSaved }: { org: string; endpoint: Endpoint; onSaved: () => void }) {
  const [open, setOpen] = useState(false);
  const [text, setText] = useState((endpoint.deploy_namespaces ?? []).join(' '));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState<Manifest | null>(null);
  const save = async () => {
    setBusy(true); setError('');
    try {
      const r = await secureFetch(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint.id)}/manifest`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ namespaces: parseNamespaces(text) }) });
      if (r.ok) { setResult(await r.json() as Manifest); onSaved(); return; }
      setError(await refusal(r, { forbidden: 'Only an administrator can change the namespaces a cluster deploys to.', invalid: 'List at most 32 namespaces by name: lower-case letters, digits and hyphens; not kyyard-agent or a kube- namespace.' }));
    } catch { setError(offlineWrite); } finally { setBusy(false); }
  };
  return <section className="dr-stack" aria-label="Deploy namespaces">
    <button type="button" className="btn-secondary" onClick={() => setOpen(!open)}>{open ? 'Close manifest' : 'Regenerate manifest'}</button>
    {open && <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void save(); }}>
      <label>Namespaces to deploy to<input value={text} onChange={(e) => setText(e.target.value)} disabled={busy} autoComplete="off" placeholder="shop billing" /></label>
      <p>Saving replaces the list. KyYard maps applications only to listed namespaces, and the agent checks its own access in the cluster before every apply, so nothing is deployed until the manifest is applied.</p>
      <button disabled={busy}>Save and show manifest</button>
      {error && <p role="alert">{error}</p>}
    </form>}
    {result && <div className="dr-alert dr-alert-warn" role="region" aria-label="Namespace manifest">
      <pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}>{result.command}</pre>
      <details><summary>Manifest</summary><pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}>{result.manifest}</pre></details>
      <p>{result.note}</p>
      <button onClick={() => downloadManifest(result.manifest_file, result.manifest)}>Download manifest</button>{' '}
      <button className="btn-secondary" onClick={() => setResult(null)}>Dismiss</button>
    </div>}
  </section>;
}
```

Create `web/src/components/KubernetesMapping.tsx`:

```tsx
import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource, type Endpoint, type Inventory } from '../tenant';
import { StateNotice } from './StateNotice';
import { ManifestRegeneration } from './KubernetesManifest';
import { displayName } from './Endpoints';
import type { ApplicationInstance } from './ApplicationAdoption';

const MAPPING_REFUSALS: Record<string, string> = {
  namespace_unknown: "The cluster's manifest does not grant that namespace. Regenerate the manifest, apply it, then map again.",
  application_adopted: 'The application is mapped to another endpoint, or was deployed in its current namespace. Remove it there before moving it.',
  deployment_in_progress: 'A deployment is being applied; wait for its result.',
  runtime_unsupported: 'That endpoint is not a Kubernetes cluster.',
};

// KubernetesMapping maps the application to a namespace of a Kubernetes cluster: the namespace
// is chosen from the ones the cluster's manifest grants, and an administrator can regenerate the
// manifest beside it. Nothing is deployed until a plan is applied.
export function KubernetesMapping({ base, org, env, instance, admin, onChanged }: { base: string; org: string; env: string; instance?: ApplicationInstance; admin: boolean; onChanged: (status?: string) => void }) {
  const endpoints = useTenantResource<Endpoint[]>(`/api/organizations/${encodeURIComponent(org)}/environments/${encodeURIComponent(env)}/endpoints?limit=200`);
  const clusters = (Array.isArray(endpoints.data) ? endpoints.data : []).filter((e) => e.runtime === 'kubernetes');
  const [chosen, setChosen] = useState(instance?.endpoint_id ?? '');
  const [namespace, setNamespace] = useState(instance?.namespace ?? '');
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const cluster = clusters.find((c) => c.id === chosen);
  if (endpoints.state === 'ready' && clusters.length === 0) return null;
  const save = async () => {
    setBusy(true); setMessage('');
    try {
      const r = await secureFetch(`${base}/mapping`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ endpoint_id: chosen, namespace }) });
      if (r.ok) { onChanged(`Mapped to namespace ${namespace}. Nothing was deployed.`); return; }
      const payload = await r.json().catch(() => ({})) as { code?: unknown };
      const code = typeof payload.code === 'string' ? payload.code : '';
      setMessage(r.status === 403 ? 'Only an administrator can map applications.' : Object.hasOwn(MAPPING_REFUSALS, code) ? MAPPING_REFUSALS[code] : r.status === 409 ? 'Another application on this cluster has the same name, or the mapping changed. Refresh applications.' : 'The mapping was refused. Refresh applications and try again.');
    } catch { setMessage('Offline: the server could not be reached.'); } finally { setBusy(false); }
  };
  return <section className="dr-stack" aria-label="Kubernetes mapping">
    <h3>{instance ? `Kubernetes namespace ${instance.namespace}` : 'Deploy to a Kubernetes cluster'}</h3>
    <p>Each service becomes a Deployment, a Service, a ConfigMap and a Secret in the namespace, labelled as this application's. A service with a volume, a host address or a restart policy other than always stops at the plan with the reason.</p>
    <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
    {endpoints.state === 'ready' && <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void save(); }}>
      <label>Cluster<select value={chosen} disabled={busy || Boolean(instance)} onChange={(e) => { setChosen(e.target.value); setNamespace(''); }}>
        <option value="">Choose a cluster</option>
        {clusters.map((c) => <option key={c.id} value={c.id}>{displayName(c.name)} · {c.state}</option>)}
      </select></label>
      {cluster && ((cluster.deploy_namespaces ?? []).length > 0
        ? <label>Namespace<select value={namespace} disabled={busy} onChange={(e) => setNamespace(e.target.value)}>
          <option value="">Choose a namespace</option>
          {(cluster.deploy_namespaces ?? []).map((ns) => <option key={ns} value={ns}>{ns}</option>)}
        </select></label>
        : <p>This cluster's manifest grants no namespace yet{admin ? '. Regenerate it below and apply it.' : '; ask an administrator to regenerate it.'}</p>)}
      <button disabled={busy || !cluster || !namespace || namespace === instance?.namespace}>Save namespace mapping</button>
      {message && <p role="alert">{message}</p>}
    </form>}
    {cluster && admin && <ManifestRegeneration key={cluster.id} org={org} endpoint={cluster} onSaved={endpoints.reload} />}
  </section>;
}

// KUBERNETES_UNVALIDATED is the one sentence the application page gives a Kubernetes instance.
export const KUBERNETES_UNVALIDATED = 'KyYard does not read health from a Kubernetes Deployment yet: the rollout wait is the apply\'s health check, and nothing is rolled back automatically. To go back, plan and apply the earlier revision.';

// KubernetesWorkloads lists the instance's Deployments as the cluster last reported them.
export function KubernetesWorkloads({ org, instance }: { org: string; instance: ApplicationInstance }) {
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(instance.endpoint_id)}/inventory`);
  const rows = (inventory.data?.snapshot.kubernetes?.workloads ?? []).filter((w) => w.kind === 'Deployment' && w.instance === instance.id);
  return <section className="dr-stack" aria-label="Kubernetes Deployments">
    <h3>Deployments in {instance.namespace} on {instance.endpoint_name}</h3>
    <p>{KUBERNETES_UNVALIDATED}</p>
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {inventory.state === 'ready' && (rows.length === 0 ? <p>No Deployment of this application is reported yet.</p> : <table className="ky-table ky-responsive-table"><thead><tr><th>Deployment</th><th>Ready</th><th>Images</th></tr></thead><tbody>{rows.map((w) => <tr key={w.name}>
      <td data-label="Deployment">{displayName(w.name)}</td>
      <td data-label="Ready">{w.ready}/{w.desired}{w.paused ? ' (paused)' : ''}</td>
      <td data-label="Images" style={{ overflowWrap: 'anywhere' }}>{w.images.map(displayName).join(', ')}</td>
    </tr>)}</tbody></table>)}
  </section>;
}
```

In `web/src/components/ApplicationAdoption.tsx`:

Replace

```tsx
import { usePagination } from './Pagination';
import { knownBlockers } from './ApplicationPreflight';

export type ApplicationInstance = { id: string; application_id: string; endpoint_id: string; endpoint_name: string; project: string; revision: number; current_revision: number; previous_revision: number; mapping_version: number; container_count: number; containers: AdoptedContainer[] };
type AdoptedContainer = { id: string; name: string; image_id: string; created_at: string };
const REMOVAL_CODES: Record<string, string> = {
```

with

```tsx
import { usePagination } from './Pagination';
import { knownBlockers } from './ApplicationPreflight';
import { KubernetesMapping } from './KubernetesMapping';

// namespace is set exactly for an instance mapped to a Kubernetes cluster.
export type ApplicationInstance = { id: string; application_id: string; endpoint_id: string; endpoint_name: string; project: string; revision: number; current_revision: number; previous_revision: number; mapping_version: number; container_count: number; containers: AdoptedContainer[]; namespace?: string };
type AdoptedContainer = { id: string; name: string; image_id: string; created_at: string };
const REMOVAL_CODES: Record<string, string> = {
```

Replace

```tsx
type Preview = { application_name: string; endpoint_name: string; endpoint_id: string; project: string; revision: number; digest: string; containers: AdoptedContainer[] };

export function ApplicationAdoption({ base, org, env, applicationName, instance, onChanged }: { base: string; org: string; env: string; applicationName: string; instance?: ApplicationInstance; onChanged: (status?: string) => void }) {
  const [endpoint, setEndpoint] = useState('');
  const [offset, setOffset] = useState(0);
```

with

```tsx
type Preview = { application_name: string; endpoint_name: string; endpoint_id: string; project: string; revision: number; digest: string; containers: AdoptedContainer[] };

export function ApplicationAdoption({ base, org, env, applicationName, instance, admin = false, onChanged }: { base: string; org: string; env: string; applicationName: string; instance?: ApplicationInstance; admin?: boolean; onChanged: (status?: string) => void }) {
  const [endpoint, setEndpoint] = useState('');
  const [offset, setOffset] = useState(0);
```

Replace

```tsx
    finally { setBusy(false); }
  };
  if (instance) return <div>
    <p>Adopted project <bdi>{instance.project}</bdi> · host {instance.endpoint_name} · {instance.container_count} recorded containers. Adoption does not mean the running configuration matches revision {instance.revision}.</p>
```

with

```tsx
    finally { setBusy(false); }
  };
  if (instance?.namespace) return <div>
    <p>Mapped project <bdi>{instance.project}</bdi> · namespace {instance.namespace} on cluster {instance.endpoint_name}.</p>
    <KubernetesMapping base={base} org={org} env={env} instance={instance} admin={admin} onChanged={onChanged} />
    <RemoveApplication base={base} instance={instance} onChanged={onChanged} />
  </div>;
  if (instance) return <div>
    <p>Adopted project <bdi>{instance.project}</bdi> · host {instance.endpoint_name} · {instance.container_count} recorded containers. Adoption does not mean the running configuration matches revision {instance.revision}.</p>
```

Replace

```tsx
    <p>Associate this application with an exact container snapshot. No restart, relabeling or deployment. Networks and volumes remain unowned.</p>
    <StateNotice state={hosts.state} onRetry={hosts.reload} />
    {hosts.state === 'ready' && <label>Docker host<select value={endpoint} onChange={(e) => setEndpoint(e.target.value)}><option value="">Choose a host</option>{hosts.data?.map((h) => <option key={h.id} value={h.id}>{h.name} · {h.state}</option>)}</select></label>}
    {(offset > 0 || (hosts.data?.length ?? 0) === 20) && <div className="ky-pagination"><button className="btn-secondary" disabled={offset === 0} onClick={() => { setEndpoint(''); setOffset(offset - 20); }}>Previous hosts</button><button className="btn-secondary" disabled={hosts.state !== 'ready' || (hosts.data?.length ?? 0) < 20} onClick={() => { setEndpoint(''); setOffset(offset + 20); }}>Next hosts</button></div>}
    {endpoint && <ProjectChoice key={endpoint} base={base} org={org} endpoint={endpoint} onChanged={onChanged} />}
  </div>;
}
```

with

```tsx
    <p>Associate this application with an exact container snapshot. No restart, relabeling or deployment. Networks and volumes remain unowned.</p>
    <StateNotice state={hosts.state} onRetry={hosts.reload} />
    {hosts.state === 'ready' && <label>Docker host<select value={endpoint} onChange={(e) => setEndpoint(e.target.value)}><option value="">Choose a host</option>{hosts.data?.filter((h) => h.runtime !== 'kubernetes').map((h) => <option key={h.id} value={h.id}>{h.name} · {h.state}</option>)}</select></label>}
    {(offset > 0 || (hosts.data?.length ?? 0) === 20) && <div className="ky-pagination"><button className="btn-secondary" disabled={offset === 0} onClick={() => { setEndpoint(''); setOffset(offset - 20); }}>Previous hosts</button><button className="btn-secondary" disabled={hosts.state !== 'ready' || (hosts.data?.length ?? 0) < 20} onClick={() => { setEndpoint(''); setOffset(offset + 20); }}>Next hosts</button></div>}
    {endpoint && <ProjectChoice key={endpoint} base={base} org={org} endpoint={endpoint} onChanged={onChanged} />}
    <KubernetesMapping base={base} org={org} env={env} admin={admin} onChanged={onChanged} />
  </div>;
}
```

Replace

```tsx
  return <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void remove(); }}>
    <h3>Remove application</h3>
    <p>Stops and removes the {instance.container_count} adopted containers of <bdi>{instance.project}</bdi> on {instance.endpoint_name}. Named volumes, images and the saved revisions are kept; the application is marked removed and can be discarded later. Nothing rolls back.</p>
    <label>Confirm removal project<input value={confirm} onChange={(e) => setConfirm(e.target.value)} disabled={busy || uncertain} autoComplete="off" /></label>
    <button className="btn-danger" disabled={busy || uncertain || confirm !== instance.project}>Remove application</button>
```

with

```tsx
  return <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void remove(); }}>
    <h3>Remove application</h3>
    {instance.namespace
      ? <p>Deletes the Deployments, Services, ConfigMaps and Secrets labelled as <bdi>{instance.project}</bdi> in namespace {instance.namespace} on {instance.endpoint_name}; objects KyYard did not label are left alone. The saved revisions are kept; the application is marked removed and can be discarded later. Nothing rolls back.</p>
      : <p>Stops and removes the {instance.container_count} adopted containers of <bdi>{instance.project}</bdi> on {instance.endpoint_name}. Named volumes, images and the saved revisions are kept; the application is marked removed and can be discarded later. Nothing rolls back.</p>}
    <label>Confirm removal project<input value={confirm} onChange={(e) => setConfirm(e.target.value)} disabled={busy || uncertain} autoComplete="off" /></label>
    <button className="btn-danger" disabled={busy || uncertain || confirm !== instance.project}>Remove application</button>
```

In `web/src/components/Applications.tsx`:

Replace

```tsx
import { ApplicationComparison } from './ApplicationComparison';
import { ApplicationAdoption, type ApplicationInstance } from './ApplicationAdoption';
import { useState } from 'react';
import { usePagination } from './Pagination';
import { secureFetch } from '../api';
import { canManagePolicies, useTenantResource, type MemberOrganization } from '../tenant';
import { StateNotice } from './StateNotice';
```

with

```tsx
import { ApplicationComparison } from './ApplicationComparison';
import { ApplicationAdoption, type ApplicationInstance } from './ApplicationAdoption';
import { KubernetesWorkloads } from './KubernetesMapping';
import { useState } from 'react';
import { usePagination } from './Pagination';
import { secureFetch } from '../api';
import { canEnroll, canManagePolicies, useTenantResource, type MemberOrganization } from '../tenant';
import { StateNotice } from './StateNotice';
```

Replace

```tsx
  const instances = useTenantResource<ApplicationInstance[]>(`${base}/instances`);
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const policyAdmin = canManagePolicies((Array.isArray(organizations.data) ? organizations.data : []).find((o) => o.id === org)?.role);
  const refresh = () => { drafts.reload(); instances.reload(); setSelected(''); };
  const pagination = usePagination(drafts.data ?? [], base);
```

with

```tsx
  const instances = useTenantResource<ApplicationInstance[]>(`${base}/instances`);
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const role = (Array.isArray(organizations.data) ? organizations.data : []).find((o) => o.id === org)?.role;
  const policyAdmin = canManagePolicies(role);
  const refresh = () => { drafts.reload(); instances.reload(); setSelected(''); };
  const pagination = usePagination(drafts.data ?? [], base);
```

Replace

```tsx
          }}>Discard {draft.name}</button>
        </div>
        {selected === draft.id && <><RevisionView key={`${draft.id}/${draft.latest_revision}`} base={base} draft={draft} /><ApplicationRevisionEditor key={`edit/${draft.id}/${draft.latest_revision}`} base={`${base}/${encodeURIComponent(draft.id)}`} name={draft.name} expected={draft.latest_revision} onSaved={() => { refresh(); setMessage('New revision saved. Running containers were not changed.'); }} />{instances.state === 'ready' && <ApplicationAdoption key={instances.data?.find((i) => i.application_id === draft.id)?.id ?? draft.id} applicationName={draft.name} base={`${base}/${encodeURIComponent(draft.id)}`} org={org} env={env} instance={instances.data?.find((i) => i.application_id === draft.id)} onChanged={(status) => { refresh(); setMessage(status ?? ''); }} />}{instances.state === 'ready' && !instances.data?.some((i) => i.application_id === draft.id) && <ApplicationHistory base={`${base}/${encodeURIComponent(draft.id)}`} />}{instances.state === 'ready' && instances.data?.filter((i) => i.application_id === draft.id).map((i) => <div key={i.id}><ApplicationMapping base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} /><ApplicationComparison base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} /><ApplicationPreflight org={org} base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} /><ApplicationUpdates base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} mappingVersion={i.mapping_version} project={i.project} latestRevision={draft.latest_revision} onPlanned={() => setPlanKey((k) => k + 1)} /><ApplicationDeploymentPlan base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} latestRevision={draft.latest_revision} instance={i} refreshKey={planKey} /></div>)}
        {instances.state === 'ready' && <ApplicationPolicy key={`policy/${draft.id}`} base={`${base}/${encodeURIComponent(draft.id)}`} admin={policyAdmin} org={org} endpointID={instances.data?.find((i) => i.application_id === draft.id)?.endpoint_id} />}</>}
      </li>)}
```

with

```tsx
          }}>Discard {draft.name}</button>
        </div>
        {selected === draft.id && <><RevisionView key={`${draft.id}/${draft.latest_revision}`} base={base} draft={draft} /><ApplicationRevisionEditor key={`edit/${draft.id}/${draft.latest_revision}`} base={`${base}/${encodeURIComponent(draft.id)}`} name={draft.name} expected={draft.latest_revision} onSaved={() => { refresh(); setMessage('New revision saved. Running containers were not changed.'); }} />{instances.state === 'ready' && <ApplicationAdoption key={instances.data?.find((i) => i.application_id === draft.id)?.id ?? draft.id} applicationName={draft.name} base={`${base}/${encodeURIComponent(draft.id)}`} org={org} env={env} instance={instances.data?.find((i) => i.application_id === draft.id)} admin={canEnroll(role)} onChanged={(status) => { refresh(); setMessage(status ?? ''); }} />}{instances.state === 'ready' && !instances.data?.some((i) => i.application_id === draft.id) && <ApplicationHistory base={`${base}/${encodeURIComponent(draft.id)}`} />}{instances.state === 'ready' && instances.data?.filter((i) => i.application_id === draft.id).map((i) => <div key={i.id}>{i.namespace ? <KubernetesWorkloads org={org} instance={i} /> : <><ApplicationMapping base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} /><ApplicationComparison base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} /></>}<ApplicationPreflight org={org} base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} /><ApplicationUpdates base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} mappingVersion={i.mapping_version} project={i.project} latestRevision={draft.latest_revision} onPlanned={() => setPlanKey((k) => k + 1)} /><ApplicationDeploymentPlan base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} latestRevision={draft.latest_revision} instance={i} refreshKey={planKey} /></div>)}
        {instances.state === 'ready' && <ApplicationPolicy key={`policy/${draft.id}`} base={`${base}/${encodeURIComponent(draft.id)}`} admin={policyAdmin} org={org} endpointID={instances.data?.find((i) => i.application_id === draft.id)?.endpoint_id} />}</>}
      </li>)}
```

In `web/src/components/ApplicationPreflight.tsx`:

Replace

```tsx
type Blocker = 'mapping_requires_review' | 'unassigned_adopted_containers' | 'image_inventory_incomplete' | 'service_unmapped' | 'explicit_image_reference_required' | 'image_not_reported' | 'image_reference_ambiguous' | 'image_identity_invalid' | 'reported_port_overlap' | 'desired_port_overlap' | 'replacement_identity_invalid' | 'revision_services_differ' | 'bind_mount_new' | 'mounts_unreported' | 'mount_unsupported' | 'volume_missing'
  | 'clock_skew' | 'inspection_unavailable' | 'replacement_identity_changed' | 'configuration_unsupported' | 'frame_too_large' | 'too_many_registry_hosts' | 'frame_invalid' | 'agent_deploy_unsupported' | 'agent_pull_unsupported' | 'agent_inspect_unsupported';
export const messages: Record<Blocker, string> = {
  mapping_requires_review: 'Review and save service mapping for the latest definition.',
```

with

```tsx
type Blocker = 'mapping_requires_review' | 'unassigned_adopted_containers' | 'image_inventory_incomplete' | 'service_unmapped' | 'explicit_image_reference_required' | 'image_not_reported' | 'image_reference_ambiguous' | 'image_identity_invalid' | 'reported_port_overlap' | 'desired_port_overlap' | 'replacement_identity_invalid' | 'revision_services_differ' | 'bind_mount_new' | 'mounts_unreported' | 'mount_unsupported' | 'volume_missing'
  | 'clock_skew' | 'inspection_unavailable' | 'replacement_identity_changed' | 'configuration_unsupported' | 'frame_too_large' | 'too_many_registry_hosts' | 'frame_invalid' | 'agent_deploy_unsupported' | 'agent_pull_unsupported' | 'agent_inspect_unsupported' | 'kubernetes_unsupported' | 'k8s_namespace';
export const messages: Record<Blocker, string> = {
  mapping_requires_review: 'Review and save service mapping for the latest definition.',
```

Replace

```tsx
  agent_pull_unsupported: 'Upgrade the host agent to enable deployments that pull images.',
  agent_inspect_unsupported: 'Upgrade the host agent to enable live inspection, which planning requires.',
};
// Definition volumes (named|bind) and runtime mounts (volume|bind); kinds render from this table only.
```

with

```tsx
  agent_pull_unsupported: 'Upgrade the host agent to enable deployments that pull images.',
  agent_inspect_unsupported: 'Upgrade the host agent to enable live inspection, which planning requires.',
  kubernetes_unsupported: 'Some services cannot run as a Kubernetes Deployment yet; each is named below with the reason.',
  k8s_namespace: "The cluster's manifest no longer grants this namespace. Regenerate the manifest and apply it, or map the application to a granted namespace.",
};
// Definition volumes (named|bind) and runtime mounts (volume|bind); kinds render from this table only.
```

In `web/src/components/ApplicationInspection.tsx`:

Replace

```tsx
  network: 'is on a network other than its project network',
  image_config: "overrides its image's command, entrypoint, healthcheck, working directory or stop signal",
};
type Inspection = {
```

with

```tsx
  network: 'is on a network other than its project network',
  image_config: "overrides its image's command, entrypoint, healthcheck, working directory or stop signal",
  k8s_volume: 'mounts a volume; Kubernetes deployment of stateful services arrives with the migration analyzer',
  k8s_host_ip: 'publishes a port on a host address; a Kubernetes Service has none, so drop the address',
  k8s_restart: 'sets a restart policy other than always or unless-stopped; a Deployment always restarts',
  k8s_name: 'has a name Kubernetes cannot use as a label; end it with a letter or digit',
  k8s_namespace: "is mapped to a namespace the cluster's manifest no longer grants",
};
type Inspection = {
```

In `web/src/components/ApplicationDeploymentPlan.tsx`:

Replace

```tsx
import type { ApplicationInstance } from './ApplicationAdoption';

type PlannedService = { name: string; reference: string; image_id: string; image_digest: string; container_id: string; replaces: { container_id: string; image_id: string; created_unix: number }; restart: string; ports: { target: number; published: number; protocol: string; host_ip: string }[]; secret_refs: string[]; pull_reference?: string; pull_digest?: string; mounts?: Mount[]; dropped_mounts?: Mount[] };
type DeployStep = { service: string; step: string; outcome: string; code?: string; detail: string };
type DeployedService = { service: string; container_id: string; image_id: string; created_unix: number };
type RemovalTarget = { service: string; container_id: string; image_id: string; created_unix: number; name: string };
type Deployment = { id: string; instance_id: string; endpoint_id: string; endpoint_name?: string; kind?: string; applied_by?: string; state: string; revision: number; mapping_version: number; created_at: string; expires_at: string; expired: boolean; detail: string; correlation_id?: string; applied_at?: string | null; deadline?: string | null; settled_at?: string | null; result: { code?: string; steps: DeployStep[]; services: DeployedService[] } | null; plan: { project: string; services?: PlannedService[]; containers?: RemovalTarget[]; volumes?: string[] }; validation?: Validation };
type Mapping = { instance_id: string; version: number; preview: { revision: number; project: string } };
type Props = { base: string; instanceID: string; latestRevision: number; instance: ApplicationInstance; refreshKey?: number };
```

with

```tsx
import type { ApplicationInstance } from './ApplicationAdoption';

type PlannedService = { name: string; reference: string; image_id: string; image_digest: string; container_id: string; replaces: { container_id: string; image_id: string; created_unix: number }; restart: string; ports: { target: number; published: number; protocol: string; host_ip: string }[]; secret_refs: string[]; pull_reference?: string; pull_digest?: string; mounts?: Mount[]; dropped_mounts?: Mount[]; object?: { namespace: string; name: string } };
type DeployStep = { service: string; step: string; outcome: string; code?: string; detail: string };
// A Kubernetes identity names a Deployment (kind, namespace, name, uid) in place of a container.
type DeployedService = { service: string; container_id: string; image_id: string; created_unix: number; kind?: string; namespace?: string; name?: string; uid?: string };
type RemovalTarget = { service: string; container_id: string; image_id: string; created_unix: number; name: string };
type Deployment = { id: string; instance_id: string; endpoint_id: string; endpoint_name?: string; kind?: string; applied_by?: string; state: string; revision: number; mapping_version: number; created_at: string; expires_at: string; expired: boolean; detail: string; correlation_id?: string; applied_at?: string | null; deadline?: string | null; settled_at?: string | null; result: { code?: string; steps: DeployStep[]; services: DeployedService[] } | null; plan: { project: string; services?: PlannedService[]; containers?: RemovalTarget[]; volumes?: string[]; namespace?: string }; validation?: Validation };
type Mapping = { instance_id: string; version: number; preview: { revision: number; project: string } };
type Props = { base: string; instanceID: string; latestRevision: number; instance: ApplicationInstance; refreshKey?: number };
```

Replace

```tsx
  configuration_drift: 'The container changed after the precondition.',
  name_reserved: 'A container already holds the name reserved for the previous one.',
  name_taken: 'A container with that name already exists.',
  identity_unusable: 'The runtime returned an unusable container identity.',
  identity_unreadable: 'The container started but its identity could not be read',
```

with

```tsx
  configuration_drift: 'The container changed after the precondition.',
  name_reserved: 'A container already holds the name reserved for the previous one.',
  name_taken: 'Something else already holds that name',
  identity_unusable: 'The runtime returned an unusable container identity.',
  identity_unreadable: 'The container started but its identity could not be read',
```

Replace

```tsx
  runtime_error: 'The runtime call failed.',
  runtime_status: 'The runtime refused',
  legacy: LEGACY_OUTCOME,
};
```

with

```tsx
  runtime_error: 'The runtime call failed.',
  runtime_status: 'The runtime refused',
  forbidden: "The agent's access in the namespace does not allow this; apply the cluster's regenerated manifest.",
  rollout_timeout: 'The Deployment did not become ready before the deadline; it stays as applied',
  conflict: 'The object kept changing under the agent',
  legacy: LEGACY_OUTCOME,
};
```

Replace

```tsx
    case 'runtime_status':
      return /^[1-5][0-9]{2}$/.test(detail) ? `${text} with status ${detail}.` : `${text}.`;
  }
  return text;
}
function isDeployment(x: unknown): x is Deployment {
  if (!x || typeof x !== 'object') return false;
```

with

```tsx
    case 'runtime_status':
      return /^[1-5][0-9]{2}$/.test(detail) ? `${text} with status ${detail}.` : `${text}.`;
    case 'name_taken':
    case 'conflict':
      return OBJECT.test(detail) ? `${text}: ${detail}.` : `${text}.`;
    case 'rollout_timeout': {
      const reasons = detail.split(',').filter((r) => ROLLOUT.test(r)).map((r) => r.replace('=', ' '));
      return reasons.length ? `${text} (${reasons.join(', ')}).` : `${text}.`;
    }
  }
  return text;
}
// The closed detail shapes of the Kubernetes codes: Kind/name, and condition=Reason words.
const OBJECT = /^(Deployment|Service|ConfigMap|Secret)\/[a-z0-9][-a-z0-9.]{0,252}$/;
const ROLLOUT = /^(progressing|available|pod)=[A-Za-z]{1,64}$/;
function isDeployment(x: unknown): x is Deployment {
  if (!x || typeof x !== 'object') return false;
```

Replace

```tsx
    {outcome && <p>{outcome}</p>}
    {explanation && <p role="alert">{explanation}</p>}
    {current.validation && <p><ValidationLine v={current.validation} /></p>}
    {current.result && <>
      {steps.controls}
```

with

```tsx
    {outcome && <p>{outcome}</p>}
    {explanation && <p role="alert">{explanation}</p>}
    {current.validation && <p><ValidationLine v={current.validation} kubernetes={Boolean(current.plan.namespace)} /></p>}
    {current.result && <>
      {steps.controls}
```

Replace

```tsx
        <td data-label="Detail">{stepText(s)}</td>
      </tr>)}</tbody></table>
      {current.result.services.length > 0 && <ul className="ky-list">{current.result.services.map(s => <li key={s.container_id} style={{ overflowWrap: 'anywhere' }}><strong>{s.service}</strong><br /><span>{s.container_id}</span><br /><span>{s.image_id}</span></li>)}</ul>}
    </>}
  </>;
```

with

```tsx
        <td data-label="Detail">{stepText(s)}</td>
      </tr>)}</tbody></table>
      {current.result.services.length > 0 && <ul className="ky-list">{current.result.services.map(s => <li key={s.service} style={{ overflowWrap: 'anywhere' }}><strong>{s.service}</strong><br />{s.kind === 'Deployment' ? <><span>Deployment {s.namespace}/{s.name}</span><br /><span>{s.uid}</span></> : <><span>{s.container_id}</span><br /><span>{s.image_id}</span></>}</li>)}</ul>}
    </>}
  </>;
```

Replace

```tsx
  const services = usePagination(d.plan.services ?? [], `${d.id}-services`);
  const containers = usePagination(d.plan.containers ?? [], `${d.id}-containers`);
  if (d.kind === 'remove') return <>
    {containers.controls}
```

with

```tsx
  const services = usePagination(d.plan.services ?? [], `${d.id}-services`);
  const containers = usePagination(d.plan.containers ?? [], `${d.id}-containers`);
  if (d.kind === 'remove' && d.plan.namespace) return <p>Deletes the objects labelled as this instance's in namespace {d.plan.namespace}.</p>;
  if (d.kind === 'remove') return <>
    {containers.controls}
```

Replace

```tsx
      <td data-label="Service"><div className="ky-resource-name"><strong>{s.name}</strong><small>{s.reference} · restart {s.restart || 'default'}</small></div></td>
      <td data-label="Pinned image"><div className="ky-resource-name">{/^sha256:[0-9a-f]{64}$/.test(s.pull_digest ?? '') ? <span>pulls {s.pull_digest?.slice(7, 19)}</span> : <><span>{s.image_id}</span><small>{s.image_digest || 'No repository digest reported'}</small></>}</div></td>
      <td data-label="Replaces container"><div className="ky-resource-name"><span>{s.container_id}</span><small>image {s.replaces.image_id}</small></div></td>
      <td data-label="Mounts">{s.mounts?.length ? <MountList mounts={s.mounts} /> : 'None'}{s.dropped_mounts?.length ? <><p>Will be dropped by the recreate:</p><MountList mounts={s.dropped_mounts} /></> : null}</td>
      <td data-label="Secrets">{s.secret_refs.length ? `${s.secret_refs.length} reference(s), values not shown` : 'None'}</td>
```

with

```tsx
      <td data-label="Service"><div className="ky-resource-name"><strong>{s.name}</strong><small>{s.reference} · restart {s.restart || 'default'}</small></div></td>
      <td data-label="Pinned image"><div className="ky-resource-name">{/^sha256:[0-9a-f]{64}$/.test(s.pull_digest ?? '') ? <span>pulls {s.pull_digest?.slice(7, 19)}</span> : <><span>{s.image_id}</span><small>{s.image_digest || 'No repository digest reported'}</small></>}</div></td>
      <td data-label="Replaces container">{s.object ? <div className="ky-resource-name"><span>Deployment {s.object.namespace}/{s.object.name}</span><small>updated in place</small></div> : <div className="ky-resource-name"><span>{s.container_id}</span><small>image {s.replaces.image_id}</small></div>}</td>
      <td data-label="Mounts">{s.mounts?.length ? <MountList mounts={s.mounts} /> : 'None'}{s.dropped_mounts?.length ? <><p>Will be dropped by the recreate:</p><MountList mounts={s.dropped_mounts} /></> : null}</td>
      <td data-label="Secrets">{s.secret_refs.length ? `${s.secret_refs.length} reference(s), values not shown` : 'None'}</td>
```

Replace

```tsx
      </> : <p>No plan for this instance.</p>)}
      {!mismatched && current && current.state === 'planned' && !isExpired(current) && <form className="dr-stack" onSubmit={e => { e.preventDefault(); void apply(); }}>
        <p>Applying replaces the mapped containers on {current.endpoint_id} with revision {current.revision} of {current.plan.project}. Nothing rolls back on failure; a failed run leaves the previous container renamed on the host.</p>
        <label>Confirm apply project<input value={applyConfirm} onChange={e => setApplyConfirm(e.target.value)} disabled={applyBusy || applyBlocked} autoComplete="off" /></label>
        <button disabled={applyBusy || applyBlocked || applyConfirm !== current.plan.project}>Apply deployment</button>
```

with

```tsx
      </> : <p>No plan for this instance.</p>)}
      {!mismatched && current && current.state === 'planned' && !isExpired(current) && <form className="dr-stack" onSubmit={e => { e.preventDefault(); void apply(); }}>
        {current.plan.namespace
          ? <p>Applying updates the Deployments, Services, ConfigMaps and Secrets of {current.plan.project} in namespace {current.plan.namespace} to revision {current.revision} and waits for each rollout. Nothing rolls back on failure; a failed run leaves the objects as applied.</p>
          : <p>Applying replaces the mapped containers on {current.endpoint_id} with revision {current.revision} of {current.plan.project}. Nothing rolls back on failure; a failed run leaves the previous container renamed on the host.</p>}
        <label>Confirm apply project<input value={applyConfirm} onChange={e => setApplyConfirm(e.target.value)} disabled={applyBusy || applyBlocked} autoComplete="off" /></label>
        <button disabled={applyBusy || applyBlocked || applyConfirm !== current.plan.project}>Apply deployment</button>
```

In `web/src/components/ApplicationValidation.tsx`:

Replace

```tsx
const fixed = (table: Record<string, string>, key: string) => Object.hasOwn(table, key) ? table[key] : '';

// verdictText is the verdict and why, through the fixed tables and the service-name shape only.
export function verdictText(v: Validation): string {
  const head = fixed(VALIDATION_VERDICTS, v.verdict) || 'Unrecognised verdict.';
  const why = fixed(VALIDATION_DETAILS, v.detail) || (SERVICE.test(v.detail) ? `Service ${v.detail}.` : '');
```

with

```tsx
const fixed = (table: Record<string, string>, key: string) => Object.hasOwn(table, key) ? table[key] : '';

// KUBERNETES_UNVERIFIED replaces the upgrade advice for a cluster: no agent can report health yet.
export const KUBERNETES_UNVERIFIED = 'Not validated: KyYard does not read health from a Kubernetes Deployment yet, so the rollout wait was this apply\'s health check.';

// verdictText is the verdict and why, through the fixed tables and the service-name shape only.
export function verdictText(v: Validation, kubernetes = false): string {
  if (kubernetes && v.verdict === 'unverifiable') return KUBERNETES_UNVERIFIED;
  const head = fixed(VALIDATION_VERDICTS, v.verdict) || 'Unrecognised verdict.';
  const why = fixed(VALIDATION_DETAILS, v.detail) || (SERVICE.test(v.detail) ? `Service ${v.detail}.` : '');
```

Replace

```tsx
}
// ValidationLine is one validation: verdict, rollback and the rollback deployment's ID prefix.
export function ValidationLine({ v }: { v: Validation }) {
  const id = v.rollback?.outcome === 'applied' ? v.rollback.deployment_id : '';
  return <span>{verdictText(v)}{v.rollback && <> {rollbackText(v)}</>}{DEPLOYMENT.test(id) && <> Deployment <code title={id}>{id.slice(0, 8)}</code>.</>}</span>;
}
```

with

```tsx
}
// ValidationLine is one validation: verdict, rollback and the rollback deployment's ID prefix.
export function ValidationLine({ v, kubernetes = false }: { v: Validation; kubernetes?: boolean }) {
  const id = v.rollback?.outcome === 'applied' ? v.rollback.deployment_id : '';
  return <span>{verdictText(v, kubernetes)}{v.rollback && <> {rollbackText(v)}</>}{DEPLOYMENT.test(id) && <> Deployment <code title={id}>{id.slice(0, 8)}</code>.</>}</span>;
}
```

In `web/src/components/KubernetesCluster.tsx`:

Replace

```tsx
import { displayName } from './Endpoints';
import { ResourceTable } from './ResourceTable';
import type { Endpoint, KubernetesInventory, Pod, PodContainer } from '../tenant';
```

with

```tsx
import { displayName } from './Endpoints';
import { ResourceTable } from './ResourceTable';
import { ManifestRegeneration } from './KubernetesManifest';
import type { ApplicationInstance } from './ApplicationAdoption';
import type { Endpoint, KubernetesInventory, Pod, PodContainer } from '../tenant';
```

Replace

```tsx
const stateBadge: Record<string, string> = { running: 'badge-success', terminated: 'badge-danger' };

// A Kubernetes endpoint is read-only in this release: health, nodes, workloads, pods with their
// logs, services and claims. No Docker control is rendered, and none would be accepted.
export function KubernetesCluster({ base, endpoint, inventory }: { base: string; endpoint: Endpoint; inventory: KubernetesInventory }) {
  const [namespace, setNamespace] = useState('');
  const [logs, setLogs] = useState<{ pod: Pod; container: PodContainer } | null>(null);
```

with

```tsx
const stateBadge: Record<string, string> = { running: 'badge-success', terminated: 'badge-danger' };

// A Kubernetes endpoint shows health, nodes, workloads, pods with their logs, services, claims
// and the applications mapped to it. No Docker control is rendered, and none would be accepted.
// instances is null while the applications cannot be read.
export function KubernetesCluster({ org, base, endpoint, inventory, instances, admin, onChanged }: { org: string; base: string; endpoint: Endpoint; inventory: KubernetesInventory; instances: ApplicationInstance[] | null; admin: boolean; onChanged: () => void }) {
  const [namespace, setNamespace] = useState('');
  const [logs, setLogs] = useState<{ pod: Pod; container: PodContainer } | null>(null);
```

Replace

```tsx
    ]} />
    <section className="panel" aria-label="Applications"><h2 style={{ fontSize: 16 }}>Applications</h2>
      <p>Kubernetes deployment arrives in a later release. KyYard reads this cluster; it does not change it.</p>
    </section>
    {logs && <dialog ref={dialog} className="modal-window ky-log-dialog" aria-label={`Logs for ${logs.pod.namespace}/${logs.pod.name}/${logs.container.name}`} onCancel={(event) => { event.preventDefault(); setLogs(null); }} onClose={() => setLogs(null)}>
```

with

```tsx
    ]} />
    <section className="panel" aria-label="Applications"><h2 style={{ fontSize: 16 }}>Applications</h2>
      <p>{(endpoint.deploy_namespaces ?? []).length ? `KyYard deploys only to ${(endpoint.deploy_namespaces ?? []).join(', ')}, as the applied manifest grants.` : 'The manifest grants no namespace, so nothing deploys to this cluster.'}</p>
      {instances === null ? <p>Applications could not be read.</p> : instances.length === 0 ? <p>No application is mapped to this cluster.</p> : <ul className="ky-list">{instances.map((i) => {
        const deployments = inventory.workloads.filter((w) => w.kind === 'Deployment' && w.instance === i.id);
        const ready = deployments.filter((w) => w.desired > 0 && w.ready === w.desired).length;
        return <li key={i.id}><strong>{displayName(i.project)}</strong> · {displayName(i.namespace ?? '')} · {deployments.length ? `${ready} of ${deployments.length} Deployments ready` : 'not deployed'}</li>;
      })}</ul>}
      {admin && <ManifestRegeneration org={org} endpoint={endpoint} onSaved={onChanged} />}
    </section>
    {logs && <dialog ref={dialog} className="modal-window ky-log-dialog" aria-label={`Logs for ${logs.pod.namespace}/${logs.pod.name}/${logs.container.name}`} onCancel={(event) => { event.preventDefault(); setLogs(null); }} onClose={() => setLogs(null)}>
```

In `web/src/pages/EndpointPage.tsx`:

Replace

```tsx
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { envPath } from '../router';
import { canExec, useTenantResource, type Endpoint, type Inventory, type MemberOrganization, type Sample } from '../tenant';
import { displayName } from '../components/Endpoints';
import { KubernetesCluster } from '../components/KubernetesCluster';
```

with

```tsx
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { envPath } from '../router';
import { canEnroll, canExec, useTenantResource, type Endpoint, type Inventory, type MemberOrganization, type Sample } from '../tenant';
import { displayName } from '../components/Endpoints';
import { KubernetesCluster } from '../components/KubernetesCluster';
```

Replace

```tsx
  const samples = useTenantResource<Sample[]>(`${base}/samples`);
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const exec = canExec((Array.isArray(organizations.data) ? organizations.data : []).find((o) => o.id === org)?.role);
  const latest = new Map((Array.isArray(samples.data) ? samples.data : []).map((s) => [s.container_id, s]));
  // -1 is "no interval yet" and a missing row is "no data"; neither is zero usage.
```

with

```tsx
  const samples = useTenantResource<Sample[]>(`${base}/samples`);
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const role = (Array.isArray(organizations.data) ? organizations.data : []).find((o) => o.id === org)?.role;
  const exec = canExec(role);
  const latest = new Map((Array.isArray(samples.data) ? samples.data : []).map((s) => [s.container_id, s]));
  // -1 is "no interval yet" and a missing row is "no data"; neither is zero usage.
```

Replace

```tsx
            {inv.snapshot.truncated?.length ? ` Lists truncated: ${inv.snapshot.truncated.join(', ')}.` : ''}
          </p>
          {cluster && shown === 'cluster' && e && (inv.snapshot.kubernetes ? <KubernetesCluster key={base} base={base} endpoint={e} inventory={inv.snapshot.kubernetes} /> : <EmptyNotice>The agent has not reported the cluster yet.</EmptyNotice>)}
          {shown === 'projects' && <><StateNotice state={ownership.state} onRetry={ownership.reload} /><ComposeProjects ownership={ownership.state === 'ready' && Array.isArray(ownership.data) ? ownership.data : null} containers={inv.snapshot.containers} truncated={inv.snapshot.truncated?.includes('containers') ?? false} onSelect={(name) => {
            setProjectFilter({ base, name }); setView('containers');
```

with

```tsx
            {inv.snapshot.truncated?.length ? ` Lists truncated: ${inv.snapshot.truncated.join(', ')}.` : ''}
          </p>
          {cluster && shown === 'cluster' && e && (inv.snapshot.kubernetes ? <KubernetesCluster key={base} org={org} base={base} endpoint={e} inventory={inv.snapshot.kubernetes} instances={ownership.state === 'ready' && Array.isArray(ownership.data) ? ownership.data : null} admin={canEnroll(role)} onChanged={details.reload} /> : <EmptyNotice>The agent has not reported the cluster yet.</EmptyNotice>)}
          {shown === 'projects' && <><StateNotice state={ownership.state} onRetry={ownership.reload} /><ComposeProjects ownership={ownership.state === 'ready' && Array.isArray(ownership.data) ? ownership.data : null} containers={inv.snapshot.containers} truncated={inv.snapshot.truncated?.includes('containers') ?? false} onSelect={(name) => {
            setProjectFilter({ base, name }); setView('containers');
```

In `web/src/components/Endpoints.tsx`:

Replace

```tsx
import React, { useState } from 'react';
import { secureFetch } from '../api';
import { tenantWrite, useTenantResource, type Endpoint, type EnrollmentToken } from '../tenant';
import { EmptyNotice, StateNotice } from './StateNotice';
import { Link } from './Link';
```

with

```tsx
import React, { useState } from 'react';
import { secureFetch } from '../api';
import { parseNamespaces, tenantWrite, useTenantResource, type Endpoint, type EnrollmentToken } from '../tenant';
import { EmptyNotice, StateNotice } from './StateNotice';
import { Link } from './Link';
```

Replace

```tsx
  const [runtime, setRuntime] = useState<'docker' | 'kubernetes'>('docker');
  const [clusterName, setClusterName] = useState('');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);
```

with

```tsx
  const [runtime, setRuntime] = useState<'docker' | 'kubernetes'>('docker');
  const [clusterName, setClusterName] = useState('');
  const [namespaces, setNamespaces] = useState('');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);
```

Replace

```tsx
    setMessage('');
    try {
      const body = runtime === 'kubernetes' ? { runtime, name: clusterName.trim() } : { runtime };
      const resp = await secureFetch(`${envBase}/enrollment-tokens`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (!resp.ok) {
```

with

```tsx
    setMessage('');
    try {
      const listed = parseNamespaces(namespaces);
      const body = runtime === 'kubernetes' ? { runtime, name: clusterName.trim(), ...(listed.length ? { namespaces: listed } : {}) } : { runtime };
      const resp = await secureFetch(`${envBase}/enrollment-tokens`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (!resp.ok) {
```

Replace

```tsx
        </select></label>
        {runtime === 'kubernetes' && <input aria-label="Cluster name" placeholder="Cluster name" required maxLength={255} value={clusterName} onChange={(e) => setClusterName(e.target.value)} />}
        <button disabled={busy || (runtime === 'kubernetes' && clusterName.trim() === '')} onClick={() => void mint()}>{runtime === 'kubernetes' ? 'Enroll a cluster' : 'Enroll a host'}</button>
      </div>
```

with

```tsx
        </select></label>
        {runtime === 'kubernetes' && <input aria-label="Cluster name" placeholder="Cluster name" required maxLength={255} value={clusterName} onChange={(e) => setClusterName(e.target.value)} />}
        {runtime === 'kubernetes' && <input aria-label="Namespaces to deploy to" placeholder="Namespaces to deploy to (optional)" value={namespaces} onChange={(e) => setNamespaces(e.target.value)} />}
        <button disabled={busy || (runtime === 'kubernetes' && clusterName.trim() === '')} onClick={() => void mint()}>{runtime === 'kubernetes' ? 'Enroll a cluster' : 'Enroll a host'}</button>
      </div>
```

- [ ] **Step 4: Run the tests, then rebuild the embedded bundle**

Run: `cd web && npm test && cd .. && make build-web`
Expected: every web test passes, and `make build-web` type-checks (`tsc -b`) and rewrites `web/dist` (never while `make ci` runs).

- [ ] **Step 5: DOX and commit**

In `web/AGENTS.md`, in the bullet beginning `- The environment's enrollment has a runtime selector`, replace `it posts \`{runtime: "kubernetes", name}\`` with `it posts \`{runtime: "kubernetes", name}\`, plus \`namespaces\` when the optional "Namespaces to deploy to" field lists any (split on commas and spaces)`; in the bullet beginning `- \`/organizations/{org}/endpoints/{endpoint}\` for a \`kubernetes\` endpoint`, replace `and an Applications panel saying Kubernetes deployment arrives in a later release. No container, image, network, volume, exec, inspect, deploy or update control is rendered;` with `and an Applications panel naming the namespaces the manifest grants and each mapped application's project, namespace and ready Deployments (from \`Workload.instance\`), with Regenerate manifest for organization and environment admins (\`canEnroll\`). No container, image, network, volume, exec, inspect or update control is rendered;`; and append:

```markdown
- An application without an instance offers, below the Docker adoption (whose host list leaves clusters out), `KubernetesMapping`: a cluster select (Kubernetes endpoints only; the card is absent without one), a namespace select from the cluster's `deploy_namespaces`, "Save namespace mapping" (`PUT .../mapping {endpoint_id, namespace}`, fixed sentences for `namespace_unknown`, `application_adopted`, `deployment_in_progress`, `runtime_unsupported`, 403 and other 409s), and for admins `ManifestRegeneration` beside it (`POST .../endpoints/{endpoint}/manifest` with the typed list, then the command, the collapsed RBAC manifest, the note and Download manifest). A Kubernetes instance shows its namespace, the same card (cluster fixed, namespace changeable), `KubernetesWorkloads` (its labelled Deployments from the cluster inventory, ready/desired, images, and the one sentence that health is not validated and nothing rolls back) in place of the Docker service mapping and comparison, and removal worded as deleting the labelled objects. Plans show `Deployment <namespace>/<name>` for each service and the apply text for a namespace; step codes `forbidden`, `rollout_timeout` (reasons as words), `conflict` and `name_taken` (`Kind/name`) render from the table in their closed shapes only; blockers `kubernetes_unsupported` and `k8s_namespace` and the `k8s_` codes read as sentences naming the fix; history lists Deployment identities; a Kubernetes apply's unverifiable validation reads `KUBERNETES_UNVERIFIED`, not the upgrade advice.
```

```bash
git add web && make tidy-check lint && git commit -m "feat(web): Kubernetes mapping, plan codes and cluster applications" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Documents, the implementation status and the full gate

**Files:**
- Modify: `README.md` (`### Kubernetes endpoints`)
- Modify: `docs/application-schema.md` (`## Kubernetes (M8)`)
- Modify: `docs/agent-protocol.md` (`## Kubernetes runtime`)
- Modify: `docs/authorization-matrix.md` (`endpoint.enroll` row)
- Modify: `docs/threat-model.md` (the Kubernetes agent row)
- Modify: `KyYard-Implementation-Plan.md` (§8)

**Interfaces:**
- Consumes: every contract of Tasks 1–8, stated as built.
- Produces: operator and protocol documents; no code.

- [ ] **Step 1: README**

In `README.md`, replace everything from the line `### Kubernetes endpoints` up to (not including) the line `## Persistent keys` with:

```markdown
### Kubernetes endpoints

A Kubernetes cluster enrolls as an endpoint the way a Docker host does. KyYard reads the whole
cluster (inventory, cluster health, pod logs) and deploys stateless applications into the
namespaces you grant it. Prerequisites: `KY_APP_URL` on HTTPS, a digest-pinned agent image
(discovered from the installed server image, or `KY_AGENT_IMAGE`), and a cluster-admin
kubeconfig for `kubectl apply`.

1. On the environment screen choose **Kubernetes cluster**, name the cluster, optionally list
   the namespaces KyYard may deploy to, and select **Enroll a cluster**. Create those
   namespaces first; KyYard never creates one. The manifest is shown once: download it (or copy
   it) and run the `kubectl apply -f kyyard-agent-<name>.yaml` command shown beside it.
2. Read the key fingerprint with `kubectl -n kyyard-agent logs deploy/kyyard-agent` and
   approve the matching endpoint.
3. Delete the spent enrollment Secret: `kubectl -n kyyard-agent delete secret kyyard-agent-enrollment`.
   The link inside it is single use, and the agent restarts from its identity Secret.

What the manifest creates, all labelled `app.kubernetes.io/name: kyyard-agent`,
`app.kubernetes.io/managed-by: kyyard`: the namespace `kyyard-agent`; the ServiceAccount
`kyyard-agent`; the ClusterRole and ClusterRoleBinding `kyyard-agent-read` (only `get` and
`list` on namespaces, nodes, pods, pod logs, events, services, persistentvolumeclaims,
deployments, statefulsets and daemonsets, plus `create` on `selfsubjectaccessreviews` so the
agent can ask what it may do: no Secrets, no ConfigMaps, no `watch`, no wildcard); the Role and
RoleBinding `kyyard-agent-identity` (`get`/`create` Secrets in `kyyard-agent` and `update` only
on `kyyard-agent-identity`, where the agent keeps its identity); in each listed namespace the
Role and RoleBinding `kyyard-agent-deploy` (`get`, `list`, `create`, `update`, `patch`,
`delete` on Deployments, Services and ConfigMaps; the same on Secrets except `list`, so the
agent reads only the Secrets it named); the Secret `kyyard-agent-enrollment`; and a one-replica
`Recreate` Deployment running `/app/kyyard-agent --kubernetes` as UID 65532 with a read-only
root filesystem, no privilege escalation, every capability dropped, seccomp `RuntimeDefault`,
and 50m/64Mi requested, 500m/256Mi limited. Applying it needs cluster-admin because it creates
cluster RBAC; the agent itself holds only the roles above.

To change the namespaces of an enrolled cluster, an organization or environment administrator
opens the cluster (or an application's Kubernetes card), selects **Regenerate manifest**, types
the list and applies the file shown with `kubectl apply -f`. It carries the RBAC only: no
enrollment link, no agent Deployment. A namespace you drop keeps its Role until you run
`kubectl -n <namespace> delete role,rolebinding kyyard-agent-deploy`. The list only decides
where KyYard lets you map applications; before every apply the agent asks the API server
whether it may create Deployments there and stops (`forbidden`) if the manifest was not
applied.

Deploying: open an application, choose the cluster and a namespace under **Deploy to a
Kubernetes cluster**, then plan and apply as on a Docker host. Each service becomes a
Deployment (one replica, `Recreate`), a `ClusterIP` Service for its published ports, a
ConfigMap and a Secret for its environment, all named `<application>-<service>` and labelled
with the application and instance; every image is pinned by digest at plan time. KyYard never
touches an object with those names that it did not label (`name_taken`), and the apply waits
for each rollout; one that does not finish before the apply's ten-minute deadline stops with
the reason the cluster gives (`ImagePullBackOff`, `CrashLoopBackOff`, ...) and leaves the objects as applied.
Stateless only in this release: a service with a volume, a port bound to a host address, or a
restart policy other than `always`/`unless-stopped` stops at the plan with the reason. The
kubelet pulls the images: for a private registry, give the namespace's `default` ServiceAccount
an imagePullSecret. A Kubernetes apply is not health-validated and never rolls back
automatically; plan and apply the earlier revision to go back. Removing the application
deletes the objects labelled as its own and nothing else.

Container, image, network, volume, terminal, inspection and adoption actions are refused for a
cluster (`409 runtime_unsupported`) and not shown. Uninstall with
`kubectl delete -f kyyard-agent-<name>.yaml`, then revoke the endpoint. To enroll the same
cluster again after a revocation, delete the Secret `kyyard-agent-identity` (or uninstall)
before applying a new manifest: an identity from an old enrollment refuses a new link.

Upgrade a cluster agent in place with
`kubectl -n kyyard-agent set image deploy/kyyard-agent agent=ghcr.io/busnes-app/kyyard@sha256:<digest>`.
The identity Secret survives the new pod, so no re-enrollment is needed. A new enrollment token
mints a new enrollment link, which an already-enrolled identity refuses: to change namespaces,
use **Regenerate manifest**, which carries no link.

```

- [ ] **Step 2: Application schema and agent protocol**

In `docs/application-schema.md`, replace the paragraph under `## Kubernetes (M8)` (the one beginning `The common model maps to Deployments/StatefulSets`) with:

```markdown
**Implemented (M8 PR 21), stateless.** An application maps to a Kubernetes endpoint with `PUT .../mapping {endpoint_id, namespace}` (`application.adopt`): the namespace must be in the endpoint's `deploy_namespaces`, the list the cluster's manifest grants (400 `namespace_unknown`). The first mapping creates the instance: project `KubernetesProject(<application name>)` (lower-case letters and digits, other runs `-`, at most 63), mapping version 1, `previous_revision` 0, no adopted identity; each later mapping is a review and may move the namespace only before the first apply. Adoption, service-to-container mapping and comparison stay Docker-only.

Each service renders as a Deployment, a Service, a ConfigMap and a Secret named from `<project>-<service>` (lower-case, `_` and `.` as `-`, `ky-` prefixed unless it starts with a letter, cut to 63 with a six-hex digest suffix when too long or shared): Deployment `<name>` (one replica, `Recreate`, image `host/repo@sha256:...`, `envFrom` the ConfigMap `<name>-env` and the `Opaque` Secret `<name>-secret`, `restartPolicy: Always`), Service `<name>` (`ClusterIP`, `port` = published, `targetPort` = target). Every object is labelled `app.kubernetes.io/name: <service>`, `app.kubernetes.io/instance: <project>`, `app.kubernetes.io/managed-by: kyyard`, `kyyard.busnes.app/application`, `kyyard.busnes.app/instance`, `kyyard.busnes.app/service` and annotated `kyyard.busnes.app/revision`, `/deployment`, `/spec-digest`; the pod template carries the deployment annotation, so every apply rolls the pods. Every environment value is secret-backed today, so values go to the Secret and the ConfigMap stays empty until plain configuration exists.

A plan resolves every image at the registry (a digest reference pins itself) and refuses, all at once, `k8s_namespace` (the namespace is no longer granted) and per service `kubernetes_unsupported` with `k8s_volume` (any volume), `k8s_host_ip` (a port with a host address), `k8s_restart` (neither `always`, `unless-stopped` nor default) and `k8s_name` (a project or service that is not a label value); nothing reaches the agent. The frame carries `KubernetesTarget` and each service's `SecretKeys`, and no registry credential. The agent refuses an object with a planned name that is not the instance's (`name_taken`) and a missing grant (`forbidden`) before any write, applies ConfigMap, Secret, Deployment, Service, waits for each rollout (`rollout_timeout` with the cluster's reasons), and reports Deployment identities (namespace, name, UID, generation, digest), which settle into the result. A cluster agent cannot report container health, so every apply is `unverifiable` and an automated policy pauses; rollback is `ineligible` (`no_prior_identity`). Removal deletes, by the instance label (Secrets by name), only KyYard's objects. PR 22 adds StatefulSets, persistent volume claims and the migration analyzer that classifies each service as `supported`, `operator_choice_required` or `blocked`.
```

In `docs/agent-protocol.md`, `## Kubernetes runtime`, replace `**Implemented (M8 PR 20), read-only.**` with `**Implemented: inventory, health and pod logs (M8 PR 20); stateless deployment into granted namespaces (M8 PR 21).**`; replace the bullet beginning `- Capabilities: a cluster agent advertises` with:

```markdown
- Capabilities: a cluster agent advertises `kubernetes.inventory`, `pod.logs` when it can read logs, `kubernetes.deploy` and `kubernetes.remove` when it can apply and remove, and nothing else (never `deployment.pull`: the kubelet pulls); a Docker agent advertises none of these. A `hello` that breaks this is answered `error {code: capability_mismatch, runtime}` and closed with `protocol_error`. Stored capabilities gate every handler, and every Docker-only route answers 409 `runtime_unsupported` before any capability check.
```

and append at the end of the section:

```markdown
- Deployment on a cluster: `deployment.apply` and `deployment.remove` carry `kubernetes: {namespace, application_id, instance_id, spec_digest}` exactly for a Kubernetes endpoint (`ValidateFor`); a frame for the other runtime is `invalid_request`. A cluster service has no `container_name`, `image_id`, `replaces` or mounts; it has `pull {reference: host/repo@sha256:..., digest}` with no tag, `restart` `always`, `unless-stopped` or empty, ports without `host_ip` (unique within the service), and `secret_keys`, the sorted env keys backed by a secret reference. A cluster frame has no `volumes` and no `registries`. A cluster removal names `services` (1..100) and no `containers`.
- The agent runs, per service in plan order, `precondition` (the first also runs a `SelfSubjectAccessReview` for `create apps/deployments` in the namespace: `forbidden`; each reads its planned objects: `name_taken` with detail `Kind/name` for one not labelled `kyyard.busnes.app/instance: <instance>`), then after every precondition `create` (ConfigMap, Secret, Deployment, Service; update when owned, create otherwise; a second conflict is `conflict` with `Kind/name`) and `start` (the rollout, read every 2 s until the observed generation is current and the replica updated and ready; past the deadline `rollout_timeout` with detail at most three `progressing|available|pod=<Reason>`). Docker-only steps are not recorded. The `started` marker is written after the last precondition and before the first write. A result's `services` are `{service, kind: Deployment, namespace, name, uid, generation, image_digest}`.
- Removal: the first service's `precondition` lists Deployments, Services and ConfigMaps by `kyyard.busnes.app/instance` and gets each named service's Secret by name, keeping only labelled ones; each service's `remove` deletes with foreground propagation and is skipped when nothing is left.
- Unsupported codes add `k8s_volume`, `k8s_host_ip`, `k8s_restart`, `k8s_name`, `k8s_namespace` (a plan's refusal; never an agent's). A cluster snapshot's Deployments carry `application` and `instance` from KyYard's labels.
```

- [ ] **Step 3: Authorization matrix and threat model**

In `docs/authorization-matrix.md`, in the `endpoint.enroll` row, replace `one-time enrollment command, or for a Kubernetes cluster the one-time manifest (the token only inside its enrollment Secret, returned only in this authenticated response)` with `one-time enrollment command, or for a Kubernetes cluster the one-time manifest (the token only inside its enrollment Secret, returned only in this authenticated response); also the cluster's deploy namespaces (\`POST .../endpoints/{endpoint}/manifest\`, answering an RBAC-only manifest with no link)`, and replace its audit cell `success/failure, target = endpoint` with `success/failure, target = endpoint; the manifest route \`<endpoint>/manifest\` with the namespace list`.

In `docs/threat-model.md`, `## Threats and mitigations`, replace the row beginning `| Compromised Kubernetes agent or its ServiceAccount token |` with:

```markdown
| Compromised Kubernetes agent or its ServiceAccount token | The ClusterRole grants only `get`/`list` on namespaces, nodes, pods, pod logs, events, services, persistentvolumeclaims, deployments, statefulsets and daemonsets, and `create` on `selfsubjectaccessreviews`: no Secrets, no ConfigMaps, no `watch`, no wildcard. Writes exist only in the namespaces the administrator listed, through a Role per namespace: Deployments, Services, ConfigMaps and Secrets, Secrets without `list`, so the agent reads only Secrets it names; its own namespace grants only its identity Secret, and `kyyard-agent` and `kube-*` cannot be listed. The agent writes only objects it labels with the instance and stops with `name_taken` before touching a same-named object it did not label, so it cannot take over another tool's objects; it asks the API server for its grant before every apply rather than trusting the server's list. Residual: a compromised agent can alter or delete workloads and read or replace the KyYard Secrets in the listed namespaces, and nothing else; pod logs and the listed metadata are readable cluster-wide by design; applying a manifest needs cluster-admin, so the person applying it can grant anything, and the manifest they apply is the one KyYard returned in an authenticated response; a namespace dropped from the list keeps its Role until deleted by hand. Two agents sharing one identity: the server refuses a second live connection (`duplicate_connection`), and the identity Secret is written against the `resourceVersion` each agent last saw, so a stale writer gets a conflict and exits | `TestManifestRBACIsReadOnlyWithoutSecrets`, `TestManifestGrantsDeployInListedNamespaces`, `TestManifestOnARealCluster` (`KY_TEST_KUBECONFIG`, local only), `TestDeployRefusesBeforeWriting`, `TestRemoveDeletesOnlyTheInstancesObjects`, `TestSecretIdentityStoreConflicts`, `TestSecretIdentityStoreStaleWriterStops`, `TestHelloCapabilitiesMustFitTheRuntime`, `TestDockerRoutesRefuseAKubernetesEndpoint`, `TestRuntimeGateMatrix` |
```

- [ ] **Step 4: Implementation status**

In `KyYard-Implementation-Plan.md` §8, insert directly above the line `Next: M8 (Kubernetes and migration).`:

```markdown
Implemented M8 PR 21 (`feat/k8s-reconcile`): the stateless reference definition deploys to a Kubernetes endpoint. The manifest grants a `kyyard-agent-deploy` Role in each namespace the administrator lists (at enrollment, or later through an audited RBAC-only regeneration; migration 34 stores the list and each instance's namespace). An application maps to a listed namespace, plans with every image pinned at the registry and stops with named `k8s_` reasons for volumes, host addresses, restart policies and names; the cluster agent (`kubernetes.deploy`, `kubernetes.remove`) checks its own grant, refuses objects it did not label, applies a Deployment, Service, ConfigMap and Secret per service, waits for the rollout, and removes by label. Applies are unverifiable and never roll back; update checks read the running digest from the labelled Deployment. The real-cluster test deploys and removes one service locally with `KY_TEST_KUBECONFIG` and `KY_TEST_DEPLOY_IMAGE`, not in CI.
```

and replace that line itself with `Next: M8 PR 22 (StatefulSets, persistent volume claims and the migration analyzer).`

- [ ] **Step 5: The full gate**

Run: `make ci`
Expected: `==> Local CI checks passed` (tidy, lint, race suite with coverage, web tests, smoke).

Run: `PG=… go test -count=1 ./...`
Expected: every package `ok`.

Run: `go list -deps ./cmd/server | grep -c k8s.io`
Expected: `0`.

If a disposable cluster is at hand: `KY_TEST_KUBECONFIG=$HOME/.kube/config KY_TEST_DEPLOY_IMAGE=registry.k8s.io/pause@sha256:<digest> go test -count=1 -run TestManifestOnARealCluster -v ./internal/runtime/kubernetes/`
Expected: PASS. Otherwise the real-cluster proof stays unproven and the PR description says so.

- [ ] **Step 6: Commit**

DOX pass: `README.md` and `docs/` are the root's operator documents (the root `AGENTS.md` names them); no `AGENTS.md` contract changes in this task. Re-read the chain root → `internal/agent`, `internal/store`, `internal/runtime` → `internal/runtime/kubernetes`, `internal/api`, `web` and confirm each names the contracts Tasks 1–8 landed.

```bash
git add README.md docs KyYard-Implementation-Plan.md && make tidy-check lint && git commit -m "docs: Kubernetes reconciliation, protocol, authorization and threat model" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```
