# Migration analysis and preview (M8 PR 22)

An administrator asks KyYard how an application running on a Docker host would fare on a
Kubernetes cluster. KyYard reads the stored definition and what actually runs, classifies every
service on every axis as `supported`, `operator_choice_required` or `blocked` with a fixed code
and a sentence, records the choices the operator makes (a StorageClass and size per named
volume), and then creates a destination application on the cluster that deploys through the
same plan and apply path PR 21 built. Data copy and traffic switching are the operator's steps,
listed as a checklist; KyYard never stops or removes the source. This closes the M8 gate: the
stateless reference definition previews and deploys on both runtimes, and unsupported stateful
cases stop with actionable explanations.

Decisions recorded 2026-09-25 (Yoshi): the analyzer's source is the mapped Docker instance,
its definition plus the endpoint's live inventory; named volumes become a PVC per volume with an
operator-chosen StorageClass while bind mounts, host networking and privileged flags stay
blocked; cutover is explicit manual steps with a checklist and the source is preserved until
the operator confirms; the destination deploy reuses PR 21's plan and apply path with the
migration recorded on the deployment.

Ruling (controller, 2026-09-25): `application_instances` carries `UNIQUE (application_id)` and
every plan, apply, removal, update-check and policy path resolves the instance by application.
A second mapping of the same application would relax that invariant across all of them. The
destination is therefore a **destination application**: a new application record whose first
revision is the source's current revision plus the migration's storage choices, mapped to the
cluster and namespace, linked from the migration row. PR 21's routes and code run unchanged on
it, one code path and one audit trail, which is what the decision was for. The migration row
carries both application IDs and every deployment of the destination carries `migration_id`.

## Model

Migration 35.

```sql
CREATE TABLE application_migrations (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL, environment_id TEXT NOT NULL,
  application_id TEXT NOT NULL,            -- the source
  destination_application_id TEXT,         -- set at destination creation, ON DELETE SET NULL
  destination_endpoint_id TEXT NOT NULL,
  namespace TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('analyzed','destination_created','validated','cutover_confirmed','abandoned')),
  report TEXT NOT NULL,                    -- JSON, MaxMigrationReportBytes 65536
  choices TEXT NOT NULL DEFAULT '{}',      -- JSON
  created_by TEXT NOT NULL, created_at, updated_at, confirmed_by, confirmed_at
);
CREATE UNIQUE INDEX application_migrations_open ON application_migrations (application_id) WHERE status NOT IN ('cutover_confirmed','abandoned');
ALTER TABLE deployments ADD COLUMN migration_id TEXT REFERENCES application_migrations(id) ON DELETE SET NULL;
```

One open migration per source application. Composite tenant FKs like the neighbouring tables;
every query carries organization and environment predicates.

`ApplicationSpec` gains `Kubernetes *KubernetesExtension` (`json:"kubernetes,omitempty"`):

```go
type KubernetesExtension struct {
    Volumes map[string]KubernetesVolume `json:"volumes"` // keyed by declared volume name
}
type KubernetesVolume struct {
    StorageClass string `json:"storage_class"`  // DNS-1123 subdomain, "" = cluster default
    Size         string `json:"size"`           // Kubernetes quantity, 1Mi..16Ti
    AccessMode   string `json:"access_mode"`    // ReadWriteOnce only in this release
}
```

The Compose importer never sets it; the spec digest covers it; `encodeApplicationSpec` validates
it. It lives on the destination application's revision, so the source definition is untouched.

## Analyzer

`internal/migration` (pure, no store or SDK imports): `Analyze(in Input) Report`.

```go
type Input struct {
    Spec       store.ApplicationSpec       // the source's current revision; the package imports store types, never the store's SQL
    Containers []protocol.Container        // the mapped containers from the endpoint's inventory
    Volumes    []protocol.Volume           // the endpoint's volumes
    Inspections map[string]protocol.ContainerInspection // by service, when the plan-time inspection ran
    Destination Destination                // endpoint runtime, deploy_namespaces, storage classes from inventory
    Choices    KubernetesExtension
}
type Report struct {
    Version    int                 // 1
    Services   []ServiceReport     // sorted by name
    Checklist  []Step              // fixed steps, see below
    Assumptions []string           // fixed sentences from a closed table
}
type ServiceReport struct{ Name string; Findings []Finding }
type Finding struct {
    Axis  string // storage|networking|ports|secrets|probes|resources|scheduling|flags
    Class string // supported|operator_choice_required|blocked
    Code  string // closed vocabulary below
    Detail string // validated per code: a volume name, a port, a code from UnsupportedCodes
}
```

Codes, one sentence each in the UI (closed, tested):

- storage: `volume_named` (choice: StorageClass and size; becomes `supported` once chosen),
  `volume_named_shared` (blocked: mounted by more than one service, ReadWriteOnce cannot serve
  two pods), `volume_bind` (blocked), `volume_external` (choice: an existing PVC name is out of
  scope, so it is treated as `volume_named`), `volume_size_unknown` (assumption: Docker reports
  no volume size; the operator sizes the claim).
- networking: `network_host` (blocked, from `NetworkMode`), `networks_multiple` (supported,
  note: services reach each other as `<project>-<service>` on published ports only).
- ports: `port_published` (supported: a ClusterIP Service; external exposure is the operator's
  Ingress), `port_host_ip` (blocked), `port_unpublished` (supported, note: unreachable from
  other services).
- secrets: `secrets_supported`.
- probes: `healthcheck_dropped` (choice required: no probe is rendered; the operator adds one
  after cutover or accepts the rollout wait) when the inspection's `Unsupported` names
  `healthcheck` or `image_config`.
- resources: `resource_limits_dropped` (choice required) when `Unsupported` names
  `resource_limits` or `ulimits`.
- scheduling: `scheduling_blocked` with the `Unsupported` code as detail (`pid_mode`,
  `ipc_mode`, `cgroup_parent`, `userns_mode`, `runtime`).
- flags: `flag_blocked` with the code as detail (`privileged`, `capabilities`, `security_opt`,
  `devices`, `user`); `restart_policy` blocked for `no`/`on-failure`; `read_only_rootfs`
  supported (rendered as `readOnlyRootFilesystem`).

A service is `blocked` if any finding is; the report's `Ready` is true when no finding is
`blocked` or `operator_choice_required`. When no inspection is available the probes, resources,
scheduling and flags axes carry `inspection_unavailable` (choice required: run again when the
host is online) so absence is never read as support.

Checklist (fixed steps, each with a sentence and, where it applies, a command template with
the real names filled in): label and grant the namespace (`kubectl label ns … enforce=baseline`,
regenerate the manifest); create the destination and apply it in KyYard; copy each volume's
data (`docker run --rm -v <volume>:/from … | kubectl exec … tar` recipe per PVC); validate the
destination; switch traffic (operator's DNS or Ingress); confirm cutover, then remove the source
with KyYard's removal. The checklist is display only; status moves by the two confirmations.

## Flow and API

All under `.../applications/{application}/migration`, new action `application.migrate` for the
roles that hold `application.adopt` (organization administrators), audited on
`<application>/migration/<id>`; reads under `application.read`.

- `POST …/migration` `{destination_endpoint_id, namespace}`: the source must be a Docker
  instance (else 409 `runtime_unsupported`), the destination a Kubernetes endpoint with the
  namespace in `deploy_namespaces` (400 `namespace_unknown`), no open migration (409
  `migration_open`). Runs the plan-time inspection primitive for the mapped containers
  (`system-migration`, same rate budget as a plan), analyzes, stores `analyzed`, returns the
  migration.
- `GET …/migration`: the open migration (404 when none), report, choices, status, links.
- `PUT …/migration/choices` `{volumes: {name: {storage_class, size, access_mode}}}`: validated
  against the report's named volumes and the destination inventory's StorageClasses (400
  `storage_class_unknown`, `size_invalid`, `volume_unknown`); re-analyzes with the choices.
  Allowed only in `analyzed`.
- `POST …/migration/analyze`: re-runs analysis (inventory or choices changed).
- `POST …/migration/destination`: requires `Ready`; creates the destination application
  `<name> on <endpoint>` with revision 1 = source current spec + `kubernetes` extension, its
  secret values copied from the source's current bundle into a fresh bundle, mapped to the
  endpoint and namespace; status `destination_created`; audit rows on both applications. 409
  `application_name_taken` if the name exists. The response carries the destination ID; the UI
  links to it, where plan and apply are PR 21's.
- `POST …/migration/validated` and `POST …/migration/cutover`: operator confirmations with a
  required `note` (1..500 printable), recorded with the actor. `cutover` requires `validated`.
- `DELETE …/migration`: `abandoned`. The destination application, if created, stays; the
  response says so.

Deployments: `ApplyDeployment` sets `deployments.migration_id` inside the planned→applying
transaction when the application is an open migration's destination (the `policy_run_id`
pattern); list and detail responses include it; the destination application's page shows the
migration link.

## Volumes on the wire and in the cluster

- `protocol.DeploymentService` gains `Volumes []KubernetesMount{Claim, MountPath string;
  ReadOnly bool}` and `KubernetesTarget` gains `Claims []KubernetesClaim{Name, StorageClass,
  Size, AccessMode}` (caps: 16 claims per request, 8 mounts per service, quantity syntax
  validated). `validateKubernetes` allows them; Docker requests refuse them.
- The plan for a Kubernetes instance reads the spec's `kubernetes.volumes`: a named volume with
  a choice renders a claim `<project>-<volume>` (via `KubernetesNames`) and the mount; a named
  volume without a choice is `k8s_volume` with detail `choice_required`; a bind stays
  `k8s_volume`; a named volume mounted by two services is `k8s_volume_shared`.
- `render` adds the PVC (labelled like the others, `ReadWriteOnce`, the storage class or none,
  the size), the pod volume and the container mount.
- `Deploy`: in `precondition`, `Get` each claim and check ownership (`name_taken`); in `create`,
  create a missing claim; an existing owned claim whose class or size differs is `claim_immutable`
  (new step code, detail `Kind/name`) and the run stops before the Deployment. Claims are applied
  before the Deployment. `Remove` never deletes a claim: each owned claim is reported `skipped`
  with detail `retained`, and the docs say the operator deletes data deliberately.
- Manifest: the namespaced Role gains `persistentvolumeclaims` `get, list, create` (no update,
  patch or delete); the ClusterRole gains `storage.k8s.io` `storageclasses` `get, list`. The
  regeneration route and docs tell the operator to re-apply.
- Inventory: `KubernetesInventory.StorageClasses []StorageClass{Name string; Default bool}`
  (cap 100), read by the agent, shown in the choices form.

## Web

A Migration card on a Docker-mapped instance (admins): destination cluster and namespace
selects, Analyze; the report as a table (service × axis with a badge and the sentence); the
volume choices form (StorageClass from the destination's inventory, size, access mode fixed);
Create destination (disabled until Ready) linking to the destination application; the
checklist with the two confirmations and their notes; Abandon. The destination application
page shows "Migration destination of <source>" with a link. Plan sentences for `k8s_volume`
`choice_required`, `k8s_volume_shared`, `claim_immutable`, and the retained-claim removal line.

## Documents

`docs/application-schema.md` (the `kubernetes` extension, PVC naming, the analyzer codes, what
stays blocked), `docs/agent-protocol.md` (mounts and claims, step code, StorageClasses),
`docs/authorization-matrix.md` (`application.migrate`), `docs/threat-model.md` (analysis reads
inspections under the plan budget; destination secrets are a copy of the source bundle under the
same encryption; claims retained on removal), README (the migration walkthrough and checklist),
AGENTS.md chain (`internal/migration/AGENTS.md` new), Implementation Plan §8 and the M8 gate,
Handoff §9 out-of-scope line reconciled.

## Tests

- `internal/migration`: golden reports for a definition with a named volume, a bind, a shared
  volume, a host-IP port, an inspection with `privileged` and `resource_limits`, and no
  inspection; choices turning `volume_named` into `supported`; every code has a sentence (the
  web table is checked against the Go vocabulary by a generated JSON fixture).
- store (both databases): lifecycle, one open migration, choices validation, destination
  creation copying the bundle, `migration_id` set on the destination's applies and never on the
  source's, abandon leaving the destination.
- protocol and render: claims and mounts validation and rendering.
- runtime/kubernetes: claim precondition, create, `claim_immutable`, removal retains.
- api: the whole flow over the runtime fleet with a Docker source seeded with mounts and a
  cluster destination, then plan and apply of the destination through the fake cluster agent
  with the claim in the frame; the gate matrix rows; permission rows.
- Real cluster: extend the real-cluster test with one claim on kind's default StorageClass and a
  pod mounting it, then removal leaving the claim.
- web: card states and the report table.

## Out of scope

Copying volume data, switching traffic, StatefulSets, `ReadWriteMany`, rendering probes or
resources, deleting claims, migrating from Kubernetes to Docker, more than one open migration
per application, migrating an application that is not adopted on a Docker host.
