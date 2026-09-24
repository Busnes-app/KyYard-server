# Volumes in the application definition

The application definition expresses image, environment, ports, restart and the project
network. Everything else on a container is refused at deploy so a recreate never drops it,
and `mounts` is the refusal that stops every stateful application: the acceptance run's
"Compose app with persistent data" cannot pass steps 5 and 6. This slice adds volumes to the
definition end to end: import, revision, preflight, plan, the wire, the adapter's recreate,
and the panels. Data survives because it lives in volumes the recreate mounts again.

Decisions recorded 2026-09-24 (Yoshi): bind mounts are preserve-only (a deploy may keep a
bind the container already has, never introduce one); named volumes the plan needs are
created by the agent at apply.

## Model

```go
type ApplicationVolume struct {
	Kind     string `json:"kind"`               // "named" | "bind"
	Source   string `json:"source"`             // declared volume name (named) or absolute host path (bind)
	Target   string `json:"target"`             // absolute path in the container
	ReadOnly bool   `json:"read_only,omitempty"`
}
// ApplicationService gains: Volumes []ApplicationVolume `json:"volumes,omitempty"`
// ApplicationSpec gains:    Volumes []DeclaredVolume    `json:"volumes,omitempty"`
type DeclaredVolume struct {
	Name     string `json:"name"`
	External bool   `json:"external,omitempty"` // host name is Name; otherwise "<project>_<Name>"
}
```

- Bounds (enforced by `ValidateApplicationSpec`): at most 32 volumes per service and 64
  declared per spec; a service volume's `Source` for kind `named` must be a declared name;
  for kind `bind` it must be an absolute, cleaned path (`path.Clean(source) == source`, no
  `..`); `Target` absolute and cleaned, unique per service; names `[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}`
  (Docker's volume name grammar); `tmpfs` is not part of the model.
- The host name of a named volume is resolved at plan time: `External` → `Name`; otherwise
  `<project>_<Name>` where project is the adopted instance's Compose project. This is what
  `docker compose` does, so a KyYard recreate mounts the same volume Compose created.

## Import (`internal/applications`)

`volumes:` becomes a supported service field and a supported top-level key.

- Short syntax `SOURCE:TARGET[:ro|rw]`: a source starting with `/` is a bind (also `./` and
  `~` → refused: relative and home-relative binds need a working directory KyYard does not
  have; the diagnostic says to write the absolute path); any other source is a named volume
  and must be declared at top level. A bare `TARGET` (anonymous volume) is refused with a
  diagnostic ("Anonymous volumes are unsupported; declare a named volume").
- Long syntax: `type: volume|bind`, `source`, `target`, `read_only`; any other key (`tmpfs`,
  `consistency`, `bind.propagation`, `volume.nocopy`) is refused as unsupported.
- Top-level `volumes:` accepts `name: {}` / `name:` and `name: {external: true}`; other keys
  (`driver`, `driver_opts`, `labels`, `name`) are refused as unsupported.
- Every refusal names the line and column like the existing diagnostics and carries no
  source text.

## Preflight and plan

`buildDeploymentPreflight` gains, per service, a comparison of the definition's volumes with
the mapped container's mounts. Preflight runs from the endpoint's stored inventory, so the
inventory `Container` gains `mounts: [{kind: volume|bind|other, source, target, read_only}]`
(the agent reads it from the container list's `Mounts`; at most 32 per container, further
entries truncate the list and set `mounts_truncated: true`; `source` is the volume name for
volumes and the host path for binds). The adoption preview stores the mapped containers'
mounts with their identity, and the on-demand inspection keeps its counts.

- `bind_mount_new`: a `bind` in the definition with no identical mount (source, target,
  read-only) on the container. Text in the UI: "This revision adds a host path the running
  container does not have; KyYard never introduces bind mounts. Mount it by hand first, or
  drop it from the definition."
- Named volumes never block: the recreate mounts what the definition says, creating the
  volume if needed. A bind the container has that the definition dropped is allowed (an
  operator's deliberate change); it is listed in the preview as "will be dropped".
- `mounts_unreported`: the inventory carries no mount list for the container (an agent older
  than this slice) or the list was truncated; blocks like `image_not_reported`.

The `PlannedService` gains `mounts` (the resolved list: kind `volume`|`bind`, resolved host
source, target, read-only) and the plan gains `volumes` (the named volumes to ensure, resolved
host names, deduplicated).

## Wire (`internal/agent/protocol`)

```go
type Mount struct {
	Kind     string `json:"kind"`     // "volume" | "bind"
	Source   string `json:"source"`   // volume name or absolute host path
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}
// DeploymentService gains: Mounts  []Mount  `json:"mounts,omitempty"` (≤ 32)
// DeploymentRequest gains: Volumes []string `json:"volumes,omitempty"` (≤ 64 names to ensure)
const StepVolume = "volume"
```

Validate: kind in the set; volume names by Docker's grammar; bind sources absolute and clean;
targets absolute, clean and unique per service; every `Volumes` entry is a name some
service's `volume` mount uses. `MaxDeploymentRequestBytes` is unchanged (a mount is ~100
bytes; 100 services × 32 mounts fits).

## Adapter (`internal/runtime/docker`)

- `undescribed`: mounts of type `volume` and `bind` are no longer refused; `tmpfs` and any
  other type still are. The precondition additionally requires every `bind` in the frame's
  `Mounts` to be present on the old container with the same source, target and read-only,
  else `denied` with detail `bind mount not present on the container` — the server's
  blocker made this unlikely, the agent enforces it.
- Phase one gains step `volume`, once per distinct name in `Volumes`, before any
  replacement: `POST /volumes/create` with `{Name, Labels: {com.docker.compose.project: <project>, com.docker.compose.volume: <name minus project prefix, or the name>}}`.
  201 and "already exists" (Docker returns 201 for an existing name) both succeed; other
  statuses fail the step with `volume create failed`. A failure leaves every container
  untouched (phase-one rule).
- `create` sets `HostConfig.Mounts` from the frame's `Mounts` (`Type`, `Source`, `Target`,
  `ReadOnly`) and no `Binds`.
- Removal is unchanged: volumes are never deleted.

## Panels

- Application revision view and comparison: per service, the volumes (kind, source, target,
  ro) beside ports.
- Preflight and plan: per service, the mounts the recreate will use; `bind_mount_new` and
  `mounts_unreported` texts; a "will be dropped" note for a bind the definition no longer
  lists; the plan lists the volumes it will ensure.

## Documents

`docs/application-schema.md` (Volumes: implemented; the bind policy; tmpfs and anonymous
volumes unsupported), `docs/threat-model.md` (host-path exposure: binds are preserve-only and
never introduced by a deploy; a revision cannot mount the Docker socket or `/` into a
container the operator did not already set up that way), `docs/agent-protocol.md` §8 (mounts,
volumes, the `volume` step), `docs/ACCEPTANCE.md` (the mount-refusal known gap goes; the
sample app uses a named volume), `KyYard-Implementation-Plan.md` §8, AGENTS.md for
applications, protocol, runtime, store, api, web.

## Tests

Importer: short and long syntax, ro/rw, external volumes, every refusal (relative bind, home
bind, anonymous, undeclared name, unsupported long keys, top-level driver), caps. Store
(SQLite + PostgreSQL): spec validation bounds; preflight `bind_mount_new`, dropped bind
allowed, named differences allowed, `mounts_unreported`; plan resolves host names (project
prefix vs external) and deduplicates `volumes`; existing plan/apply tests unchanged. Protocol:
validation of mounts and volumes. Adapter (fake Engine): volume step idempotent and labelled,
runs before any replacement, failure leaves containers untouched; precondition refuses a
missing bind; create body carries `HostConfig.Mounts`; tmpfs still refused. Real Docker (CI):
create a container with a named volume, write a file, deploy a recreate through the adapter,
read the file back from the new container. Web: revision view, preflight texts, plan volumes.

## Out of scope

tmpfs, anonymous volumes, volume drivers and options, bind propagation, introducing bind
mounts by policy (a later per-endpoint allow list), volume deletion (`volume.destroy` stays a
separate later action), Kubernetes PVC mapping.
