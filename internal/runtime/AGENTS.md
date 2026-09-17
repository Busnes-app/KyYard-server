# Runtime adapters

## Purpose
Adapters that speak to a container runtime and return product types only. `docker` is the first; `kubernetes` follows in M8.

## Ownership
Owns the Engine API client (`docker.New` over the Unix socket, `docker.NewHTTP` for tests and TCP daemons), the mapping into `protocol.Snapshot`, and the bounds on what a snapshot may carry. SDK or wire types never leave this package; the protocol package owns the product types.

## Local Contracts
- `Snapshot` reads `/info`, `/version`, `/containers/json?all=1`, `/images/json`, `/networks` and `/volumes` under Engine API `v1.41` (any newer daemon serves it), sorts every list so equal state serialises equally, and truncates at `protocol.MaxContainers` (1000), `MaxImages` (1000), `MaxNetworks` (200), `MaxVolumes` (500), naming truncated lists in `Truncated`. Labels are capped at 32 entries of 256 bytes. Response bodies are read to 32 MiB at most.
- Before a snapshot leaves the host it is passed through `protocol.Clamp` (schema conformance and text safety) and `protocol.Shrink` (drops labels, then list tails, until the encoding fits `protocol.MaxSnapshotBytes`, naming what it dropped), so an honest agent never sends what the server would refuse. `bound` cuts on rune boundaries.
- Container environment is never read or reported: it is where secrets live. Ports, labels, networks, image and state are.
- `Engine` supplies `runtime_version` at enrollment; a daemon that is down at start is retried on the inventory schedule, not fatal.

## Verification
- `go test ./internal/runtime/...` runs against a fake Engine (mapping, truncation) and, when `/var/run/docker.sock` is reachable, against the real daemon.

## Child DOX Index
None.
