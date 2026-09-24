# Pull step and controlled recreate (M7a, PR C)

PR C of the M7a split (PR A: registries and the registry client, #58; PR B: update detection,
#59). It lets an operator take an update that PR B detected: a plan pins the registry's current
digest for chosen services, the agent pulls that digest with the registry credential carried in
the frame, verifies what it pulled, and only then runs the existing replacement. Nothing is
automatic: a detected update changes nothing until a plan is minted and applied.

Decisions recorded 2026-09-23 (Yoshi): PR C carries the pull step, the UI action, the
`application.update` matrix reconciliation and the server-wide cap on concurrent update checks;
the hardening items (closed step-detail vocabulary, agent started-marker, clock-skew refusal,
plan-time host-configuration inspection, audit correlation IDs) move to PR D. The pinned digest
comes from a fresh `Head` at plan time, never from the cached check.

## Plan

`PlanRequest` gains `update []string`: service names whose image is to be pulled from the
registry. `PlanDeployment`:

- Every name must be a service of the planned revision with a mapping binding, else blocker
  `update_not_mapped`.
- For each named service, after the preflight and inside the same authorized operation
  (`application.deploy`), the store resolves the registry access for the reference
  (`registryFor` + anonymous-pull setting, `allow_private` folded with the operator switch) and
  the API-supplied `DigestResolver` does one `Head`. The result pins on the `PlannedService`:
  `pull_reference` (`<host>/<repository>@<digest>`, host as the reference names it, `docker.io`
  for Docker Hub) and `pull_digest`. `image_id` stays the host's current image ID for the
  preview; the frame carries no image ID for a pulled service.
- Registry failures are blockers, not errors: `registry_not_configured`, `registry_unauthorized`,
  `registry_not_found`, `registry_rate_limited`, `registry_private_destination`,
  `registry_unavailable`. A blocked plan is not persisted (existing behaviour).
- The `Head` calls run outside the transaction like PR B's check (phase split: read and
  authorize, resolve, then the audited write that also re-checks mapping version, latest
  revision and container/image pairs; `ErrAdoptionChanged` on any change). At most 4
  concurrent, 60 s overall, credential in memory only for the resolve phase.
- A plan with an empty `update` list behaves exactly as today.

The plan preview and history show `pull_digest` per service.

## Wire (`internal/agent/protocol`)

```go
type ImagePull struct {
	Reference string `json:"reference"` // host/repository@sha256:…
	Digest    string `json:"digest"`
}
type RegistryAuth struct {
	Username string `json:"username"`
	Secret   string `json:"secret"` // ≤ 4096 bytes, UTF-8, no NUL
}
// DeploymentService gains:
	Pull *ImagePull `json:"pull,omitempty"` // when set, ImageID is empty and the agent fills it after the pull
// DeploymentRequest gains:
	Registries map[string]RegistryAuth `json:"registries,omitempty"` // host → auth for hosts pulled with a credential; ≤ 16 entries
// DeploymentIdentity gains:
	ImageDigest string `json:"image_digest,omitempty"` // the pulled repository digest, required for a pulled service
```

- New step `StepPull = "pull"`, valid only in apply results; step order per service:
  `precondition`, `pull`, `rename`, `create`, `stop`, `start`, `remove` (`image` stays for
  services without a pull).
- `MaxDeploymentRequestBytes` becomes 320 KiB; `MaxRegistryAuthHosts = 16`,
  `MaxRegistryAuthSecretBytes = 4096`.
- Validate: `Pull.Reference` must be a valid image reference with a digest and a host;
  `Pull.Digest` must equal the reference's digest; `ImageID` must be empty when `Pull` is set and
  full otherwise; every `Registries` key must be a host some service's `Pull.Reference` names
  (no unused credentials in a frame); a `Registries` entry needs a non-empty secret. A result
  identity for a pulled service must carry a full `ImageID` and `ImageDigest == Pull.Digest`.
- `replaceBudget` gains one call budget for the pull step's inspect; the pull itself runs under
  the frame deadline (unchanged `DeploymentApplyDeadline`, 10 min). A large image can exhaust
  it: the result is `timed_out` before any container change, which the UI already explains.

## Agent (`internal/agent/client`)

- The frame is held in memory for the run and dropped when the run ends. It is never written
  to `deployments.json` (the ledger keeps results only, as today), never logged, never included
  in a result's detail. A test proves the secret is absent from the ledger file after a run
  with a credentialed pull and that the runner holds no reference to the frame afterwards.
- `Options.Deploy` signature is unchanged (the request already travels whole).

## Adapter (`internal/runtime/docker`)

`pull` step for a service with `Pull`:

1. `POST /images/create?fromImage=<host/repository>&tag=<digest>` with header `X-Registry-Auth`
   (base64 of `{"username","password","serveraddress"}`) when `Registries[host]` exists;
   anonymous otherwise. The body is streamed and discarded; a non-200 or an `error` line in
   the stream fails the step with the fixed detail `pull failed` (`unauthorized` for 401/403,
   `not found` for 404) and never the daemon's text.
2. `GET /images/<host/repository@digest>/json`; the response `RepoDigests` must contain
   `<host/repository>@<digest>` (with Docker Hub's `docker.io/` prefix normalised the way the
   daemon reports it) else the step fails with `pulled image does not match`.
3. The inspected `Id` becomes the service's `ImageID` for `create`; the result identity carries
   `ImageDigest`.

Pull runs after `precondition` and before any container is touched; a pull failure on any
service fails the deployment with nothing changed. `pullImage` (image controls) keeps sending
no credential; `credentialsMissing` text changes to say pulls with credentials go through a
deployment.

## Store: settle

`settleApply`: for a pulled service, accept `idn.ImageID != ps.ImageID` only when
`idn.ImageDigest == ps.PullDigest` and `idn.ImageID` is full; otherwise `ErrInvalid` (refused
result). `application_resources.image_id` records the new ID. History rows already carry the
plan JSON, so the pinned digest is visible per deployment.

## API

- `POST .../deployments` body gains `update`; the handler passes the resolver, the encryption
  key and the operator switch as the check does.
- Server-wide cap: at most 4 update checks or plan resolves in flight (`chan struct{}` of 4 on
  the server); over it, 429 `{"error": "Too many registry checks are running; try again in a moment", "code": "too_many_checks"}`.
  The per-application guard stays.
- Frame assembly (`handleApplyDeployment` → `ApplyDeployment`): the store builds `Registries`
  from the plan's pulled hosts at apply time (credential decrypted inside the apply
  transaction under `application.deploy`, sent once per host, never stored in the deployment
  row or result). The `deployments` row stores only `plan` and `result`, never the frame; the
  plan JSON carries `pull_reference` and `pull_digest` and no credential.

## Authorization

`docs/authorization-matrix.md`: retire the `application.update` row. A manual update is a plan
with `update` plus an apply, both under `application.deploy` (organization admin, environment
admin, developer). Operators keep `image.pull` for host-level pulls without credentials. The
permissions code has no `application.update` action to remove; only the document changes.

## UI

Updates panel: when any service is `update_available`, a **Plan update** button posts
`POST .../deployments` with `update` = those services and the current instance, mapping
version and confirm; the plan panel then shows the plan with the pinned digest per service for
review and apply. Fixed texts for `too_many_checks` and every `registry_*` blocker.

## Documents

`docs/agent-protocol.md` section 8 (pull step, registries map, result identity digest),
`docs/application-schema.md` (Deploy: pull; Registries: PR C delivered), `docs/threat-model.md`
(credential lifetime: server memory during plan `Head` and frame assembly, in transit inside
the authenticated agent session, agent memory during the run; never on disk on either side),
`docs/authorization-matrix.md`, `KyYard-Implementation-Plan.md` section 8 (M7a complete),
`internal/agent/protocol/AGENTS.md`, `internal/agent/client/AGENTS.md`,
`internal/runtime/docker/AGENTS.md`, `internal/store/AGENTS.md`, `internal/api/AGENTS.md`,
`web/AGENTS.md`.

## Tests

Store (SQLite + PostgreSQL) with a fake resolver: plan pins the digest; every registry blocker;
`update_not_mapped`; stale-state refusal; frame assembly carries the credential once per host
and never persists it; settle accepts a matching pulled identity and refuses a mismatched digest
or an empty image ID. Protocol: validation of `Pull`, `Registries`, size caps, result identity.
Agent: ledger scrub. Adapter: fake Engine for pull success, 401, 404, stream error, digest
mismatch, credential header present only for hosts in `Registries`; real-Docker CI step pulls
`alpine` by digest and replaces a container. API: `update` plan route, 429 cap, no credential
in any response. Web: Plan update button posts the right body; blocker texts.

## Out of scope (PR D)

Closed step-detail vocabulary, agent started-marker, clock-skew refusal, plan-time
host-configuration inspection, audit correlation IDs, a per-pull budget beyond the frame
deadline, automatic policies (M7b).
