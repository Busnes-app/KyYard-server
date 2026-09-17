# Runtime adapters

## Purpose
Adapters that speak to a container runtime and return product types only. `docker` is the first; `kubernetes` follows in M8.

## Ownership
Owns the Engine API client (`docker.New` over the Unix socket, `docker.NewHTTP` for tests and TCP daemons), the mapping into `protocol.Snapshot`, and the bounds on what a snapshot may carry. SDK or wire types never leave this package; the protocol package owns the product types.

## Local Contracts
- `pullImage` sends no credential: registry credentials are M7a, so a registry that wants one is reported as a refusal naming that reason rather than a generic failure an operator would have to guess at. A pull reports some failures inside a `200` as a JSON line carrying `error`, so the body is part of the answer. `removeImage` refuses an image a container still uses rather than forcing it, which would leave running containers pointing at nothing.
- Removal refuses a running, restarting or paused container rather than forcing it: Docker would oblige by killing the process first, and that is a second decision an operator should make explicitly. The delete carries no options, so named volumes and images survive; destroying data is its own action. A `409` from the runtime is reported as a refusal naming the dependency, not as a failure.
- `Operate` escapes the container identifier at every use, because it arrives in a request body and is concatenated into a URL addressed to the host's root-equivalent socket; the server constrains the grammar as well and neither check is allowed to be the only one. It runs one container action after re-reading the container: Docker has no universal resource version, so the precondition is operation-specific identity read immediately before acting (image digest, state). A precondition that no longer holds is `denied`, not `failed`, because nothing was attempted; `304 Not Modified` is `succeeded`, since the container was already where the caller wanted it.
- `Snapshot` reads `/info`, `/version`, `/containers/json?all=1`, `/images/json`, `/networks` and `/volumes` under Engine API `v1.41` (any newer daemon serves it), sorts every list so equal state serialises equally, and truncates at `protocol.MaxContainers` (1000), `MaxImages` (1000), `MaxNetworks` (200), `MaxVolumes` (500), naming truncated lists in `Truncated`. Labels are capped at 32 entries of 256 bytes. Response bodies are read to 32 MiB at most.
- Before a snapshot leaves the host it is passed through `protocol.Clamp` (schema conformance and text safety) and `protocol.Shrink` (drops labels, then list tails, until the encoding fits `protocol.MaxSnapshotBytes`, naming what it dropped), so an honest agent never sends what the server would refuse. `bound` cuts on rune boundaries.
- Container environment is never read or reported: it is where secrets live. Ports, labels, networks, image and state are.
- `Stats` samples the named running containers with one-shot reads (`/containers/{id}/stats?stream=false&one-shot=true`, no daemon-side wait), computes the CPU share of one core from the delta against the previous call kept in memory per container (the first sample reports -1, meaning no interval yet), sums network bytes across interfaces, gives each container read five seconds, and caps a frame at `protocol.MaxSamples` (1000). A container that vanished between listing and sampling is skipped, not an error.
- `Engine` supplies `runtime_version` at enrollment; a daemon that is down at start is retried on the inventory schedule, not fatal.

## Verification
- `go test ./internal/runtime/...` runs against a fake Engine (mapping, truncation) and, when `/var/run/docker.sock` is reachable, against the real daemon.

## Child DOX Index
None.
