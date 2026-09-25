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
