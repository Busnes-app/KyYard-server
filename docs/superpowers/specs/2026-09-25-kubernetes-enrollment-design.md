# Kubernetes enrollment and inventory (M8 PR 20)

A Kubernetes cluster becomes a KyYard endpoint the way a Docker host does: an administrator
mints an enrollment token, applies one generated manifest, approves the fingerprint, and the
endpoint page fills with the cluster's inventory and health. The agent is the same binary
with a second runtime adapter, its RBAC is read-only on a named resource subset with no
access to Secrets, and every Docker-only action is refused for the endpoint at the capability
level and absent from the UI. Writes to the cluster are PR 21.

Decisions recorded 2026-09-25 (Yoshi): enrollment by a generated manifest and one `kubectl
apply`; cluster-wide read-only RBAC on a bounded subset, Secrets excluded; PR 20 exposes
inventory, health and pod logs only; the adapter uses the client-go typed clientset with
polled inventory, no informers.

## Agent

`cmd/agent` gains `--kubernetes` (bool). With it set the agent loads the in-cluster
configuration (`rest.InClusterConfig`), refuses `--docker-socket` other than empty
(`kubernetes and docker are exclusive`), and fills the same `client.Options` seams the Docker
adapter fills: `Snapshot`, `Logs`. `Metrics`, `Operate`, `Inspect`, `Exec`, `Deploy` and
`Remove` stay nil, so the capability list built in `connect.go` never includes a Docker-only
capability. The adapter lives in `internal/runtime/kubernetes` (`Client`, `New(cfg
*rest.Config) (*Client, error)`, `NewFromClientset` for tests); client-go types never leave
the package.

**Identity.** A pod has no durable disk by default and the agent must not re-enroll on every
restart, so the identity is kept in a Secret in the agent's own namespace. `client.IdentityStore`
(`Load() (*Identity, error)`, `Save(*Identity) error`) gets two implementations: the existing
directory (`--identity-dir`) and `kubernetes.SecretIdentityStore` (namespace and Secret name
from `--identity-secret`, default `kyyard-agent-identity`). The Secret is written with
`Update` after `Get`, and created only if absent; a conflict on write is retried once and then
fatal, because two agents sharing one identity is the failure this store prevents. The
generation counter in the identity keeps the snapshot monotonic across restarts exactly as on
disk.

**Enrollment link.** `--link-file` reads the link from a file (the manifest mounts the
enrollment Secret there) so the token never appears in a command line or pod spec.
`--link` still works for Docker.

**Facts.** `runtime=kubernetes`, `server_version` (from the discovery client), `node_count`,
`platform`.

## RBAC and manifest

The server renders the manifest from a Go template with the same pinned image digest the
Docker command uses (`config.IsPinnedAgentImage`; unpinned refuses with 409
`agent_image_unpinned` as today). Objects, all in namespace `kyyard-agent`, labelled
`app.kubernetes.io/name: kyyard-agent`, `app.kubernetes.io/managed-by: kyyard`:

- `Namespace kyyard-agent`.
- `ServiceAccount kyyard-agent`.
- `ClusterRole kyyard-agent-read`, verbs `get`, `list` only, on: core `namespaces`, `nodes`,
  `pods`, `pods/log`, `events`, `services`, `persistentvolumeclaims`; `apps` `deployments`,
  `statefulsets`, `daemonsets`. No `secrets`, no `configmaps`, no `watch`, no wildcard.
- `ClusterRoleBinding kyyard-agent-read` to the ServiceAccount.
- `Role kyyard-agent-identity` in `kyyard-agent`: `get`, `create` on `secrets`, and `update`
  restricted with `resourceNames: [kyyard-agent-identity]` (create cannot be name-restricted;
  the Role is namespaced to the agent's own namespace, which holds nothing else).
- `RoleBinding kyyard-agent-identity`.
- `Secret kyyard-agent-enrollment` with key `link` holding the enrollment link. Single use:
  the token is consumed at enrollment; the Secret may be deleted afterwards and the manifest
  says so.
- `Deployment kyyard-agent`, one replica, `strategy: Recreate` (two replicas would fight over
  the identity), `automountServiceAccountToken: true`, container `agent` with `command:
  ["/app/kyyard-agent"]`, args `--kubernetes --link-file /etc/kyyard/link --identity-secret
  kyyard-agent-identity --name <endpoint name> --docker-socket=`, the enrollment Secret
  mounted read-only at `/etc/kyyard`, `runAsNonRoot: true`, `readOnlyRootFilesystem: true`,
  `allowPrivilegeEscalation: false`, all capabilities dropped, seccomp `RuntimeDefault`,
  requests 50m/64Mi, limits 500m/256Mi.

The endpoint name is required when the runtime is `kubernetes` (a pod has no useful hostname)
and follows the existing endpoint-name rule. A test renders the manifest, decodes every
document, and asserts: the ClusterRole grants no verb on `secrets` or `configmaps`, no
`watch`, no `*`; the only Secret write is the namespaced Role above; the image is a digest; the
token appears exactly once, inside the Secret. A second test applies the rendered manifest to
a real cluster when `KY_TEST_KUBECONFIG` is set and confirms `kubectl auth can-i` semantics
via `SelfSubjectAccessReview`: `get secrets` denied cluster-wide, `list pods` allowed.

## Enrollment flow

`POST .../enrollment-tokens` accepts `{runtime: "kubernetes", name}` and returns, beside the
existing fields, `manifest` (the YAML) and `command: kubectl apply -f kyyard-agent-<name>.yaml`.
No unauthenticated manifest route exists: the token is a bearer, and the only copy leaves the
server inside the authenticated response. The UI offers Download and Copy for the manifest and
shows the command; the Docker one-liner is unchanged. The disclosure text for Kubernetes states
what the ServiceAccount can read, that it cannot read Secrets, and that applying the manifest
requires cluster-admin.

Redemption, approval, rejection, revocation, key rotation and the heartbeat/offline rules are
the existing ones. The `endpoints.runtime` column and `validRuntime` already accept
`kubernetes`.

## Capabilities

Two new capabilities: `kubernetes.inventory` and `pod.logs`. A `hello` from an endpoint whose
runtime is `kubernetes` that names any capability outside `{kubernetes.inventory, pod.logs}`,
or a `hello` from a `docker` endpoint that names either of them, is answered with `error
capability_mismatch` and the socket closed with `protocol.CloseProtocol`. Stored capabilities are what gate handlers, so a Kubernetes endpoint can
never satisfy `container.*` or `deployment.*` checks.

Every existing endpoint handler that acts on containers, images, networks, volumes, exec,
inspection or deployments refuses a Kubernetes endpoint before the capability check with 409
`runtime_unsupported` and a fixed detail. Mapping an application to a Kubernetes endpoint is
refused the same way (PR 21 lifts this). The UI never renders those actions for a Kubernetes
endpoint.

## Inventory

`protocol.Snapshot` gains `Kubernetes *KubernetesInventory` (`json:"kubernetes,omitempty"`).
For a `kubernetes` endpoint the server requires it non-nil and requires `Containers`,
`Images`, `Networks` and `Volumes` empty; for a `docker` endpoint it requires it nil. Either
violation is `error snapshot_rejected`, and the session continues as with `snapshot_too_large`.
`Engine` carries `name: "kubernetes"` and the API server version.

```go
type KubernetesInventory struct {
    Nodes      []Node      // <= MaxNodes 500
    Namespaces []string    // <= MaxNamespaces 500
    Workloads  []Workload  // <= MaxWorkloads 2000
    Pods       []Pod       // <= MaxPods 2000
    Services   []Service   // <= MaxServices 2000
    Claims     []Claim     // <= MaxClaims 1000
}
type Node struct{ Name, KubeletVersion, OS, Arch string; Ready bool; Roles []string; Unschedulable bool }
type Workload struct{ Kind, Namespace, Name string; Desired, Ready, Updated int32; Images []string; Paused bool }
type Pod struct{ Namespace, Name, Phase, Node, OwnerKind, OwnerName string; StartedAt time.Time; Containers []PodContainer }
type PodContainer struct{ Name, Image, ImageID, State, Reason string; Ready bool; RestartCount int32 }
type Service struct{ Namespace, Name, Type, ClusterIP string; Ports []string }
type Claim struct{ Namespace, Name, Phase, StorageClass, Capacity string }
```

`Kind` is `Deployment|StatefulSet|DaemonSet`; `State` is `running|waiting|terminated`;
`Phase` is the Kubernetes pod phase. Decoding is bounded per list like the Docker lists
(`decodeBounded`, `Truncated` names cut lists, string fields capped at 253 bytes for names and
1 KiB for images), and `MaxSnapshotBytes` stays 1 MiB: the adapter sorts each list by
namespace then name and cuts at the cap before serializing, recording the cut in `Truncated`.
Lists come from `List` calls on the typed clientset with `Limit` paging, one page size 500, on
the existing `--inventory-every` cadence; a list that fails is reported empty with its kind in
`Truncated` and a warning log, so one forbidden verb does not blank the whole endpoint.

Storage is the existing `endpoint_inventory.snapshot` blob and `GET .../endpoints/{endpoint}/inventory`
returns the same document; no migration. Samples and rollups are not collected for Kubernetes
endpoints in PR 20 (`Metrics` is nil).

**Cluster health** is derived on read, not stored: `healthy` when every node is Ready,
`degraded` when at least one is not, `unknown` when the endpoint is offline or has no
inventory. It is a field on the endpoint detail response (`cluster_health`) and on the
endpoint list, present only for `kubernetes`.

## Pod logs

`GET .../endpoints/{endpoint}/pods/{namespace}/{pod}/logs?container=&tail=&search=&follow=&download=&timestamps=`
requires `container.logs` on the organization (the action means "read workload logs"; the
matrix note says so) and the `pod.logs` capability, and reuses `log_streams.go` unchanged:
same caps (tail 200 default and 10,000 max, 4 MiB per request, 1 MiB buffered per stream, 2
streams per endpoint), same notice tokens, same SSE and download modes, same `log.cancel`.
`protocol.LogRequest` gains `Pod *PodTarget{Namespace, Name, Container}`; `Container` (the
Docker ID) and `Pod` are exclusive and the server validates the one the endpoint's runtime
needs. Names are validated as DNS-1123 labels/subdomains at the API boundary. `container` is
required when the pod has more than one container and defaults to the only one otherwise; the
agent answers `unknown_container` from the pod spec rather than guessing. The adapter streams
`GetLogs(...).Stream` with `TailLines`, `SinceTime`, `Timestamps`, `Follow`, and applies the
same 4 MiB/10,000-line ceiling before the sink.

## UI

`EndpointPage` renders a Kubernetes view when `runtime === "kubernetes"`: cluster health and
node table; a namespace filter; workloads (kind, name, ready/desired, images); pods (phase,
node, restarts, containers with state) with a Logs action per container opening the existing
log viewer against the pod route; services and claims tables. Truncated lists show the existing
gap notice. No container, image, network, volume, exec, inspect, deploy or update controls are
rendered, and the applications panel says Kubernetes deployment arrives in a later release.
The enrollment dialog gets a runtime selector; choosing Kubernetes requires a name and swaps
the one-liner for the manifest Download/Copy and `kubectl apply` command.

## Documents

`README.md` (Kubernetes endpoint section: prerequisites, the manifest, RBAC summary, deleting
the enrollment Secret, uninstall by `kubectl delete -f`), `docs/agent-protocol.md`
(capabilities, `KubernetesInventory`, `PodTarget`, runtime and snapshot rules),
`docs/authorization-matrix.md` (`container.logs` covers pod logs; `endpoint.enroll` covers
the manifest), `docs/threat-model.md` (Kubernetes agent row: read-only ClusterRole without
Secrets, identity Secret in its own namespace, what cluster-admin applying the manifest
implies), `internal/runtime/kubernetes/AGENTS.md`, `internal/runtime/AGENTS.md` index,
`internal/agent`, `internal/api` and `web` AGENTS.md, Implementation Plan §8 status.

## Tests

- `internal/runtime/kubernetes`: fake clientset (`k8s.io/client-go/kubernetes/fake`) drives
  `Snapshot` (mapping of every field, owner resolution through ReplicaSets, cap and
  `Truncated`, a forbidden list) and `Logs` (tail, follow cancel, multi-container refusal).
  `SecretIdentityStore` create, update, conflict retry.
- Real cluster, gated on `KY_TEST_KUBECONFIG` (kind in CI is out of scope for this PR; run
  locally): manifest applies, RBAC denies `get secrets`, agent snapshot names the kind node.
- `internal/api`: fake agent enrolled with `runtime: kubernetes` (existing harness); hello
  with a Docker capability closed with `capability_mismatch`; Docker capability on a Docker
  endpoint unaffected; snapshot shape rules; `runtime_unsupported` on every container,
  deployment and mapping route; pod log route end to end through the existing stream harness;
  manifest rendering assertions; `cluster_health` derivation.
- `web`: enrollment dialog runtime switch; Kubernetes endpoint page renders inventory and no
  Docker actions; pod log action opens the viewer with the pod route.

## Out of scope

Any write to the cluster (PR 21), application mapping and reconciliation (PR 21), migration
analysis (PR 22), pod exec, metrics samples for pods, Helm packaging, informers, custom
resources, multi-cluster kubeconfig from outside the cluster.
