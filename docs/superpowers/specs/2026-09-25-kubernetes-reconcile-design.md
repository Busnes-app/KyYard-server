# Kubernetes application reconciliation (M8 PR 21)

The same `compose.v1` application definition that deploys to a Docker host deploys to a
Kubernetes endpoint: an administrator maps the application to a cluster and a namespace, plans,
and applies; the cluster agent renders each service as a Deployment, a Service, a ConfigMap and
a Secret it owns by label, applies them, and waits for the rollout. Stateful definitions stop at
the plan with a named reason. Writes are bounded to namespaces the cluster-admin listed when
they applied the manifest. This is the M8 gate's first half: the stateless reference definition
previews and deploys on both runtimes, and unsupported cases stop with actionable explanations.

Decisions recorded 2026-09-25 (Yoshi): write RBAC bounded to named target namespaces via a
regenerated manifest; stateless only (Deployment, Service, ConfigMap, Secret), volumes block
with reasons; ownership by labels and annotation, a same-name object without them blocks;
environment to a ConfigMap and secret-marked values to a Secret, both KyYard-owned.

## Write RBAC and the manifest

`manifest.Input` gains `Namespaces []string` (each a DNS-1123 label, at most 32, sorted, no
duplicates). For each namespace the template adds, in that namespace, a `Role
kyyard-agent-deploy` and a `RoleBinding kyyard-agent-deploy` to the agent ServiceAccount:

- `apps` `deployments`: `get`, `list`, `create`, `update`, `patch`, `delete`.
- core `services`, `configmaps`: `get`, `list`, `create`, `update`, `patch`, `delete`.
- core `secrets`: `get`, `create`, `update`, `patch`, `delete`. No `list`: owned Secrets are
  found by their deterministic names, never by scanning the namespace.
- `authorization.k8s.io` `selfsubjectaccessreviews`: `create` (cluster-scoped, in the existing
  ClusterRole) so the agent can check its own grant before touching anything.

The manifest never creates the namespaces; the cluster-admin does. The read ClusterRole is
unchanged and its tests keep asserting no Secrets, no ConfigMaps, no `watch`, no wildcard.

The enrollment-token request for runtime `kubernetes` accepts `namespaces` (optional), and a
new route `POST .../endpoints/{endpoint}/manifest` (`endpoint.enroll`, Kubernetes endpoints
only, audited `endpoint.manifest`) takes `{namespaces}` and returns `manifest`, `manifest_file`
and `command` for the RBAC objects only (Roles and RoleBindings, plus the existing ClusterRole
and bindings so the file is complete; no enrollment Secret, no Deployment). The list is stored
write-once-per-call on the endpoint (`endpoints.deploy_namespaces`, JSON, migration 34) and
shown on the endpoint page with the note that the admin must apply the regenerated manifest.
The agent does not trust the stored list: before every apply it runs a `SelfSubjectAccessReview`
for `create deployments` in the target namespace and stops with `forbidden` if denied.

## Mapping

`PUT .../applications/{application}/mapping` on a Kubernetes endpoint takes `{endpoint_id,
namespace}`; `namespace` must be in the endpoint's `deploy_namespaces` (400 `namespace_unknown`)
and the application must not be mapped to another endpoint (existing rule). The instance record
gains `namespace` (migration 34, `application_instances.namespace`, empty for Docker). Service
to container mapping, adoption preview and adoption stay Docker-only (`runtime_unsupported`);
a Kubernetes instance starts at `previous_revision` 0 with no adopted identity.

## Rendering

`internal/runtime/kubernetes/render` turns one `protocol.DeploymentService` plus the target
into `k8s.io/api` objects. It runs in the agent; the server never imports it, and
`go list -deps ./cmd/server | grep -c k8s.io` stays 0. Names
are `<project>-<service>` slugged to a DNS-1123 label (lower-case, `_` and `.` to `-`, cut to
63 with a 6-hex digest suffix when cut or when two services slug alike). Labels on every
object: `app.kubernetes.io/name: <service>`, `app.kubernetes.io/instance: <project>`,
`app.kubernetes.io/managed-by: kyyard`, `kyyard.busnes.app/application: <application id>`,
`kyyard.busnes.app/instance: <instance id>`, `kyyard.busnes.app/service: <service>`.
Annotations: `kyyard.busnes.app/revision`, `kyyard.busnes.app/deployment` (the deployment
UUID), `kyyard.busnes.app/spec-digest`.

- **Deployment** `<name>`: one replica, `Recreate` strategy (a compose service has one
  instance; two replicas of a service with an implicit local state is not what the definition
  says), pod labels as above, one container `<service>` with the image pinned by digest
  (`image@sha256:...`, resolved server-side as today), `containerPort`s from the definition,
  `envFrom` the ConfigMap and, when present, the Secret; `restartPolicy: Always`;
  `progressDeadlineSeconds: 540` (below the apply deadline) and `minReadySeconds: 10`. No probes,
  no resources, no security context beyond what the definition can express (none) in PR 21;
  the plan says so in the `unsupported` detail only if the definition asked for something.
- **Service** `<name>`, `ClusterIP`, one port per published port: `port` = published,
  `targetPort` = target, protocol from the definition. Unpublished ports get no Service entry;
  with none left, an owned Service from an earlier revision is deleted (UID-guarded).
- **ConfigMap** `<name>-env`: every environment value that is not secret-backed.
- **Secret** `<name>-secret`, `Opaque`: every secret-backed value. Omitted when there is none.

`protocol.DeploymentService` gains `SecretKeys []string` (env keys backed by an
`ApplicationSecretRef`), set by the server from the revision's secret bundle, so the agent
never guesses which values are secret.

### Blockers

Planned server-side against the definition, in the `precondition` step with code `unsupported`
and detail codes added to `protocol.UnsupportedCodes`:

- `k8s_volume`: any service volume (named or bind). Detail names the service and the target.
- `k8s_host_ip`: a port with `HostIP` set.
- `k8s_restart`: `restart` other than `always`, `unless-stopped` or empty.
- `k8s_name`: two services whose slugs collide even after the suffix (cannot happen with the
  digest suffix; kept as the closed code for the invariant).
- `k8s_namespace`: the instance's namespace is no longer in `deploy_namespaces`.

The plan is refused (400 `plan_blocked`, existing shape) listing every blocker; nothing reaches
the agent. Ownership conflicts are agent-side: an object with the planned name that lacks the
`kyyard.busnes.app/instance` label, or carries another instance's, fails the `precondition`
step with code `name_taken` (added to `stepCodes`) naming kind and name.

## Plan and apply

`PlanDeployment` on a Kubernetes instance skips live inspection and image-ID identity: the
plan's `PlannedService` carries `Object{Namespace, Name}` and `Replaces` empty; `Volumes` and
`Containers` are empty. Image update checks (`update`, `pull_reference`, `pull_digest`) work as
today. `PinImages` works as today (a rollback pins the image). `capability` checks require
`kubernetes.deploy` (apply) and `kubernetes.remove` (removal); a cluster agent advertises them
when `Options.Deploy`/`Options.Remove` are set, and `CapabilitiesFit` allows exactly
`kubernetes.inventory`, `pod.logs`, `kubernetes.deploy`, `kubernetes.remove` for the runtime.
`deployment.pull` is never advertised by a cluster agent: the kubelet pulls.

`dockerOnly` becomes `runtimeGate(kind)`: mapping PUT, plan, apply and application removal
accept both runtimes; adoption preview, adoption, commands, container logs, exec, inspection
and removal preview stay Docker-only. The Kubernetes apply path reads the instance's namespace
and puts it on the request: `protocol.DeploymentRequest.Kubernetes *KubernetesTarget{Namespace,
ApplicationID, InstanceID}` (`omitempty`; `Validate` requires it exactly when the endpoint's
runtime is `kubernetes`, and the agent refuses a request without it with `invalid_request`).

The agent (`kubernetes.Client.Deploy`, same signature as Docker's, wired to `Options.Deploy`)
runs, per service in plan order:

1. `precondition`: `SelfSubjectAccessReview` for `create deployments` in the namespace
   (`forbidden`), then `Get` on the namespace: its `pod-security.kubernetes.io/enforce` label
   must be `baseline` or `restricted` (`pod_security`, detail `missing`, `privileged` or
   `invalid`), then `Get` on each planned object and the ownership check (`name_taken`).
   The `started` marker is recorded after the last precondition and before the first write.
2. `create`: apply ConfigMap, Secret, Deployment, Service in that order: `Update` when the
   object exists and is owned (preserving `resourceVersion` semantics), `Create` otherwise. A
   conflict is retried once after a re-read; a second conflict is `step_failed` with detail
   `conflict`. A 403 on a write (the grant was allowed, so a quota or admission policy refused
   it) is `admission_denied` with detail `Kind/name`.
3. `start`: wait until the Deployment's `observedGeneration` is current and `readyReplicas` and
   `availableReplicas` equal `replicas`, polling every 2 s within the request deadline. For the
   current generation, `ReplicaFailure=True` or `Progressing` with reason
   `ProgressDeadlineExceeded` ends the wait at once as a failed `rollout_timeout`. Timeout is `step_failed`
   with code `rollout_timeout` (added to `stepCodes`) and a detail from the Deployment's
   `Progressing`/`Available`/`ReplicaFailure` conditions and the newest pod's waiting reason
   (`ImagePullBackOff`, `CrashLoopBackOff`, `CreateContainerConfigError`), all from the closed
   `CleanText` path. A timed-out rollout is not rolled back by the agent: the objects stay as
   applied and the result says so; the operator reapplies the previous revision.
4. `recheck` and Docker-only steps (`image`, `pull`, `volume`, `stop`, `rename`, `remove`)
   are `skipped` with detail `kubernetes`.

The result carries, per service, `Identity{Kind: "Deployment", Namespace, Name, UID,
Generation}` in place of the container identity (`DeploymentIdentity` gains those fields,
`omitempty`; Docker fields stay). `SettleDeployment` stores it. Frame caps are unchanged; a
rendered object set is small.

`Remove` (`kubernetes.remove`): `List` Deployments, Services and ConfigMaps in the namespace by
the instance label, `Get` each service's Secret by name, verify ownership, delete with
`propagationPolicy: Foreground`. Objects that are gone are `skipped`.

## Validation, rollback and policies

A cluster agent never advertises `container.inspect.health`, so every Kubernetes apply is
`unverifiable` by the existing rule, and an automated policy run pauses with the existing text.
Rollback is `ineligible` with `no_prior_identity` because `Replaces` is empty. The application
page says so in one sentence for Kubernetes instances. Update policies work (image checks and
planned applies); the rollout wait is the apply's own health signal in this release.

## Inventory and status

`protocol.Workload` gains `Application, Instance string` (from the labels, empty when not
KyYard's; 64 bytes each). The application page shows the instance's Deployments from the
endpoint's inventory (desired, ready, images, paused); the cluster page's Applications section
lists the instance names mapped to the endpoint with their ready state, replacing the "arrives
in a later release" sentence.

## Authorization and audit

No new action: mapping, plan, apply and removal keep the actions their Docker routes use today;
the manifest route is `endpoint.enroll`. Audit rows are the existing
plan/apply/settle rows; the manifest route audits `endpoint.manifest` with the namespace list.
Threat model: the Kubernetes agent row is rewritten (it can now write, only in listed
namespaces, only objects it labels; `name_taken` stops it hijacking another tool's objects;
`secrets` without `list`). Blast radius: within a granted namespace a compromised agent can
create any pod, under any of the namespace's ServiceAccounts and mounting any Secret, and `get`
any Secret by name. KyYard therefore deploys only into namespaces that enforce PodSecurity
`baseline` or stricter (`pod_security` in the `precondition` step otherwise), and the operator
should grant only such namespaces. Pod logs and listed metadata are readable cluster-wide by
design; a namespace dropped from the list keeps its Role until deleted by hand.

## UI

Mapping card on a Kubernetes endpoint: namespace select from `deploy_namespaces`, with the
"regenerate manifest" action beside it for admins. Plan view: the Kubernetes step codes and
`unsupported` details in the code tables (`k8s_volume` and friends read as sentences naming the
fix: "Service db mounts a volume; Kubernetes deployment of stateful services arrives with the
migration analyzer"). History shows Deployment identities. Validation panel: the single
`unverifiable` sentence for Kubernetes.

## Documents

`docs/application-schema.md` (rewrite `## Kubernetes (M8)`: object mapping, names, labels,
blockers, what PR 22 adds), `docs/agent-protocol.md` (capabilities, `KubernetesTarget`,
`SecretKeys`, identity fields, step and unsupported codes), `docs/authorization-matrix.md`
(manifest route row), `docs/threat-model.md`, `README.md` (namespaces at enrollment, regenerating
the manifest, what deploys and what blocks), AGENTS.md chain, Implementation Plan §8.

## Tests

- `render`: golden objects for a two-service definition (one with a secret), slugging and
  collision suffix, label and annotation sets, Service port mapping; every blocker.
- `internal/runtime/kubernetes`: fake clientset `Deploy` (create path, update path preserving
  ownership, `name_taken`, `forbidden` via a reactor on SSAR, conflict retry, rollout wait and
  timeout with condition detail, `started` ordering) and `Remove` (label selection, foreign
  object left alone, gone objects skipped).
- `manifest`: namespaced Roles exactly as listed, no `list` on secrets, ClusterRole unchanged;
  `Namespaces` validation.
- `internal/api`: mapping on a cluster endpoint (namespace rules, `namespace_unknown`), plan
  blockers, plan and apply through the fake cluster agent (request carries `KubernetesTarget`
  and `SecretKeys`; result identities settle), removal, `runtimeGate` matrix (every route, both
  runtimes), manifest route and audit, capability fit for the two new capabilities, hello
  refusal of `deployment.pull` from a cluster agent.
- Real cluster, `KY_TEST_KUBECONFIG`: extend `TestManifestOnARealCluster` with one namespace,
  assert the Role verbs via `SelfSubjectAccessReview` (no `list secrets`), then run the agent's
  `Deploy` for a one-service `busybox`-class definition and see the Deployment ready, then
  `Remove` and see it gone.
- web: mapping card, plan code tables, cluster Applications section.

## Out of scope

StatefulSets, PVCs and any volume (PR 22), networks and `depends_on` ordering, probes,
resources, replicas other than one, Ingress, adoption of existing Kubernetes objects, health
validation from Deployment status, rollback for Kubernetes instances, Helm.
