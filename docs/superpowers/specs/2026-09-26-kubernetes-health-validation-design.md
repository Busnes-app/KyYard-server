# Kubernetes health validation and rollback (M8 follow-on)

A Kubernetes apply gets the same treatment a Docker apply has had since PR 19: after the rollout
succeeds, KyYard watches the instance for a bounded window and records a verdict; an automated
update that fails validation returns to the images the previous apply pinned, when that is
possible, or pauses the policy with the reason. The loop, the window, the verdict vocabulary, the
audit rows and the policy pause texts are the existing ones. What is new is a per-instance status
read for clusters, a cluster judge over that status, and a cluster rollback target.

Decisions recorded 2026-09-26 (Yoshi): this slice is next; design calls delegated to the
controller. Rulings (controller): the inspection frame family gains a Kubernetes target and answer
rather than a new transport; the rollback pins the previous succeeded deployment's recorded
digests; the ClusterRole is unchanged because Deployments and pods are already readable.

## Observation: the inspection frames learn Kubernetes

`protocol.InspectionTarget` gains `Workload *WorkloadRef{Namespace, Name, UID string}` (exclusive
with `ContainerID`; `ValidateFor(runtime)` requires the one the runtime needs). `ContainerInspection`
gains `Workload *WorkloadStatus` (`omitempty`), which a Kubernetes answer carries and a Docker answer
never does:

```go
type WorkloadStatus struct {
    UID                string
    Generation         int64
    ObservedGeneration int64
    Desired, Updated, Ready, Available int32
    Conditions []WorkloadCondition // Type, Status, Reason; closed reason words, <= 8
    Pods       []PodStatus         // <= MaxPodContainers*4 pods; Name, UID, Phase, Containers []PodContainer
    Missing    bool                // the Deployment is gone
}
```

Caps and grammar follow the inventory types (`PodContainer` is reused; names are DNS-1123; reason
words match `reasonWord`). The frame size cap is unchanged. The agent answers from `Get` on the
Deployment and a `List` of its pods by the instance and service labels in the target's namespace;
a target whose UID differs from the live object answers `Missing: false` with the live UID so the
judge sees `changed`. New capability `kubernetes.inspect`, advertised by a cluster agent when
`Options.Inspect` is set (the same option the Docker adapter uses), allowed only for the
`kubernetes` runtime in `CapabilitiesFit`. The existing grant, nonce, expiry, rate budget and
`inspection.cancel` rules apply unchanged; the server-side `Validate` of an answer is runtime-aware.
The RBAC already grants `get`/`list` on deployments and pods cluster-wide, so the manifest,
disclosure and threat model do not change; `TestClusterDisclosureMatchesTheManifestAndThreatModel`
stays as it is.

## Validation rows and the cluster judge

`SettleDeployment` already opens a validation row for every succeeded apply. For a cluster
instance the row's services are the plan's services and the target of each observation is
`WorkloadRef{Namespace: plan.Namespace, Name: PlannedService.Object.Name, UID:
identity.UID}` from the settled Deployment identity. `PendingValidations` reads the endpoint's
capabilities: `kubernetes.inspect` plays the role `container.inspect.health` plays for Docker;
without it the row is `unverifiable` with a new detail `ValidationDetailNoInspect` ("the cluster
agent cannot report workload status; upgrade the agent image") and the policy pauses as today.

`ServiceBaseline` gains `PodUIDs []string` and `RestartCount` is the sum over the pods'
containers. `Judge` treats a cluster observation so:

- `changed`: the Deployment is `Missing`, or its UID differs from the target, or its `Generation`
  advanced beyond the generation the apply settled (someone edited it), or the pod set has a UID
  not present at baseline while the baseline pods are gone and restarts did not grow (a recreate
  by someone else).
- `restarting`: the summed restart count grew since baseline.
- `exited`: a container is `terminated` and the pod is not being replaced (`Available` below
  `Desired` and no `waiting` container).
- `unhealthy`: at or after `observe_until`, `Available < Desired` or `Ready < Desired`, or any
  container `waiting` with reason `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`,
  `CreateContainerConfigError` or `CreateContainerError`.
- `healthy`: a complete observation at or after `observe_until` with `ObservedGeneration >=
  Generation`, `Available == Desired == Ready`, no restart growth, no waiting containers.
- `unverifiable`: as today (offline for the window, no baseline by the window's end, an invalid
  answer, the capability missing).

`verdictRank` is unchanged. Detail sentences are new fixed constants (closed table) naming the
first failing service and the reason word.

## Rollback target for clusters

`RollbackTarget` handles `plan.Namespace != ""` instead of returning `no_prior_identity`:

- The prior deployment is the instance's latest `succeeded` `apply` before the validated one
  (the row whose settle set `current_revision` to what is now `previous_revision`). None →
  `no_prior_identity`. Its plan must be readable; its `Namespace` must equal the validated plan's
  (else `service_set_changed` with detail `namespace`).
- Eligibility: the service sets of the prior plan and the validated plan are equal
  (`service_set_changed` otherwise); the prior plan's `Claims` and each service's `ClaimMounts`
  equal the validated plan's (`service_set_changed` with detail `claims`; claims are immutable and
  KyYard never deletes one, so a rollback must not try to change them); every prior service has a
  recorded `Pull.Digest` (`no_prior_identity` otherwise); the prior revision's spec still
  validates (`prior_definition_invalid`). No image-presence check: the kubelet pulls by digest.
- `Rollback.Images` carries `service -> <repository>@sha256:<digest>` from the prior plan's
  `Pull.Reference` and `Pull.Digest`. `Revision` is `previous_revision`.

`PlanDeployment` accepts `PinImages` for a Kubernetes instance: a pinned service skips registry
resolution and plans with `Pull.Reference` set to the pinned digest reference and no tag; the
frame's `Pull` carries that digest. Everything else in the cluster plan is unchanged (blockers,
claims from the extension, `KubernetesTarget`). `performRollback` needs no change beyond the
inspections it already skips for a cluster; the rollback deployment settles, opens its own
validation row with `is_rollback`, and never triggers another rollback, as today.

## Policies and UI

Automated cluster applies now pause with the same three texts Docker uses. The policy card's
"agent cannot report health" warning becomes runtime-aware: for a cluster it triggers on a missing
`kubernetes.inspect` and names the agent image upgrade. `KUBERNETES_UNVERIFIED` is retired; the
verdict sentences apply to both runtimes; the new detail constants get sentences; the rollback
line for a cluster reads "Rollback sent to revision N" as for Docker.

## Compatibility

Server first, then agents (documented order). A cluster agent without `kubernetes.inspect` makes
every automated cluster update `unverifiable` with the upgrade detail until it is upgraded; a
manual apply records `unverifiable` only. A Docker endpoint that names `kubernetes.inspect` is
refused at hello like the other cluster capabilities.

## Documents

`docs/application-schema.md` (the Kubernetes section: validation and rollback now apply; the
retired sentence removed), `docs/agent-protocol.md` (the Kubernetes target and answer on the
inspection frames, the capability, caps), `docs/authorization-matrix.md` (unchanged actions; note
that `system-validation` reads clusters too), README (what a cluster validation checks and what it
cannot see), AGENTS.md chain, Implementation Plan §8.

## Tests

- protocol: `WorkloadRef`/`WorkloadStatus` validation and caps; `CapabilitiesFit` with the new
  capability; runtime-aware answer validation.
- runtime/kubernetes: the inspect handler against the fake clientset (ready, restarting, waiting
  reasons, missing, UID mismatch, pod list capped).
- store: the cluster judge table (every verdict), `RollbackTarget` for clusters (each refusal and
  the eligible case), `PinImages` planning a pinned digest with no registry call.
- api: the validation loop over a fake cluster agent answering inspections (healthy; restarting →
  rollback planned with the prior digests, applied, settled, `is_rollback` row; an agent without
  the capability → `unverifiable` with the upgrade detail and a paused policy); a Docker hello
  naming `kubernetes.inspect` refused.
- Real cluster (`KY_TEST_KUBECONFIG`, `KY_TEST_DEPLOY_IMAGE`): after the existing deploy, one
  `WorkloadStatus` read shows `Available == Desired` and the pod's container running.
- web: verdict and detail sentences, the policy warning per runtime, fixtures.

## Out of scope

Probes and readiness beyond Kubernetes' own, rollback of claims or namespaces, validating
Deployments KyYard did not create, StatefulSets, a `watch`-based observer, changing the window or
poll cadence.
