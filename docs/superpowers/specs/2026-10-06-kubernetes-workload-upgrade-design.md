# Kubernetes workload image updates (parity piece 1 of 4)

A Docker container can be checked against its registry and updated in one click (PRs 97, 99): an
update check returns a closed verdict, and *Update image* recreates the container from the
reference's current digest. A Kubernetes workload that is not part of a KyYard application has
neither; its configuration form takes a free-text image and never resolves it. This slice gives
standalone Deployments, StatefulSets and DaemonSets the same check, the same one-click update and
a digest pin on apply and run.

Decisions recorded 2026-10-06 (Yoshi): Kubernetes parity is split into four pieces, in order:
workload image updates, a richer workload Overview, a runtime-aware home page with the stale
rollback text removed, and cluster usage metrics. Each gets its own spec and PR. This piece
matches Docker on `master` at `c22dc73`: pin on apply, per-workload update check, one-click
update. No background update policy for standalone workloads; applications keep theirs.

## Constraints carried over

- Images are pinned by digest, never written as a bare tag (`docs/application-schema.md`,
  `store/application_deployment.go`).
- An update is a rollout of the existing object through the guarded `workload.apply` patch: read
  `resourceVersion`, changed fields only, `application_managed` workloads refused, Pod Security
  baseline/restricted required. Nothing deletes or recreates a Service, so static ClusterIPs hold.
- Registry credentials never leave the server. A private image still needs the namespace's
  `imagePullSecret`, as an application deploy does.
- The agent is unchanged: it already applies whatever image string the frame names.

## Digest pin on apply and run

`POST …/workloads/{namespace}/{kind}/{name}/apply` and `POST …/workloads` (run) accept an
optional `pull: [<container name>…]` naming entries of `spec.containers` or
`spec.init_containers`. An unknown or duplicate name is `ErrInvalid`. For each named container,
before the command is recorded, the server:

1. splits the image with `trackedReference` (see Update check); a reference that already
   carries a digest is re-resolved from its tag (the tag is what the operator is asking about),
   and one with a digest but no tag is refused as `image_unresolved`;
2. resolves credentials with `ResolveRegistryAccess(…, permissions.ImagePull, …)`, under the same
   organization registry, anonymous-pull and private-destination rules as Docker;
3. takes a registry slot (`acquireRegistrySlot`), extends the write deadline, and `Head`s the
   reference under `ImageCheckDeadline`;
4. rewrites the container image to `host/repository:tag@sha256:…`.

Any failure answers 422 `image_unresolved` (or the registry policy code Docker returns) and sends
nothing. The store's existing checks run first, so a managed workload or a missing permission is
refused before any registry call: a new read-only `CheckWorkloadFrame` runs
`CreateWorkloadApply`'s preconditions without writing, as `CheckDirectCommand` does for Docker.
Docker's `ResolveDirectImage` keeps its signature. The credential-and-Head step that the
container check, the workload check and the pin share moves into one `internal/api` helper,
`headDigest`, so the rules cannot drift.

The kubelet pulls `repo:tag@digest` by digest; the tag stays readable in the spec, so the next
update knows what to resolve again.

## Update check

`POST …/workloads/{namespace}/{kind}/{name}/updates/check` with the Docker check's gates: service
tokens refused (audited as `bearer`), 12 per minute per principal (shared `image-check:` key),
`CheckEndpointAccess(ContainerConfigure)`, the Kubernetes runtime gate, kind limited to
deployment, statefulset and daemonset.

The answer is read from the stored inventory snapshot and registry `Head`s only:

```json
{ "workload": "ns/deployment/name", "verdict": "update_available", "detail": "", "checked_at": "…",
  "containers": [ { "name": "web", "reference": "nginx:1.27", "local_digest": "sha256:…",
                    "remote_digest": "sha256:…", "verdict": "update_available", "detail": "" } ] }
```

- The containers and references come from the workload's pods in the snapshot (`OwnerKind` and
  `OwnerName`). `local_digest` is the digest in each pod's `image_id` whose repository matches
  the reference. Pods that disagree, as they do mid-rollout, give `unknown`.
- An image is split by `trackedReference`: the part before `@` is the tracked tag (parsed by
  `registry.ParseReference`, which refuses a tag and digest together), the part after is the
  pin. `repo:tag@sha256:…` tracks `repo:tag`, so a pinned workload is still offered later
  updates. Only `repo@sha256:…` with no tag is `pinned`.
- Per-container verdicts use Docker's words: `up_to_date`, `update_available`, `pinned`,
  `unknown`, and `registry_error` with a closed `detail` (`unauthorized`, `not_found`,
  `rate_limited`, `private_destination`, `unavailable`). Registry text is never returned.
- The workload verdict is `update_available` if any container's is, else `registry_error` if any
  container's is, else `up_to_date` if all are, else `pinned` if all are pinned or up to date,
  else `unknown`. The workload `detail` is the first `registry_error` container's.
- A workload with the KyYard application labels answers `managed`, with no registry call. The UI
  points to the application's update flow.
- Containers are checked one at a time inside one registry slot, at most 32 (`MaxWorkloadImages`).
  Repeated references are resolved once.
- Before answering, the snapshot is read again; if the workload's pods changed, it is
  `ErrAdoptionChanged`, as with Docker.

## UI

- `useWorkloadUpdateChecker` mirrors `useContainerUpdateChecker`: queued checks for visible
  rows, a five-minute cache keyed by workload and pod image IDs, a manual re-check. `WorkloadUpdate`
  renders the same badge vocabulary as `ContainerUpdate`, plus *Managed by an application*.
- *Update image* (`useWorkloadImageUpdate`, page-owned like `useContainerImageUpdate`): it reads
  `…/configuration`, refuses when `unsupported` is non-empty, and submits `apply` with that
  configuration's `resource_version`, the configuration unchanged, `pull` set to every
  container whose image tracks a tag (a digest-only image is skipped) and `confirm` the workload
  name. Like Docker's button, it pulls whatever the tags name now; containers already current
  are re-pinned to the same digest. A lost response locks retries
  and says to check recent activity.
- It appears on the cluster page's Workloads table and on the workload page, for roles that can
  configure and only when the agent advertises `kubernetes.workloads`.
- `WorkloadConfigurationForm`: each container shows the running digest from its pods and a *Pin
  the reference's current digest* checkbox. When editing, the box is ticked when the reference changes and
  starts unticked otherwise; in run mode it starts unticked, so a run still works without
  registry access configured. A ticked container goes into `pull`. The "Nothing rolls back" note
  stays, because it is true outside applications.

## Tests

- **API** (fake resolver):
  - apply and run with `pull` pin the right containers;
  - unknown container names are refused;
  - an unresolvable reference is 422 and records no command;
  - a managed workload and a missing permission are refused with zero `Head` calls;
  - a reference that already has a digest is re-resolved from its tag.
- **Check:** each verdict, including pods that disagree, `managed`, every registry error code, a
  service token, the rate limit, a Docker endpoint (409), and a changed snapshot.
- **Web** (vitest):
  - checker caching and the badges;
  - *Update image* submits `pull` only for `update_available`, refuses when settings are
    unsupported, and locks after a lost response;
  - the form checkbox defaults for a changed and an unchanged reference.
- **Real cluster** (`KY_TEST_KUBECONFIG`, local only): applying with a pinned image rolls out and
  the pods report that digest.

## Out of scope

- Update policies for standalone workloads.
- Bare pods.
- Pushing `imagePullSecret`s.
- Pieces 2 to 4.
