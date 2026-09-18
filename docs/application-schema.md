# KyYard application schema

**Status:** M6 Compose discovery and internal application/revision persistence are implemented. Bounded public draft import and encrypted environment resolution are implemented. Explicit container-snapshot adoption and release are implemented. Deployment remains for following M6–M7 slices.

## Vocabulary

- **Application** — a named unit of desired state inside one environment. Names are unique per environment and enforced by the database.
- **Revision** — an immutable, numbered snapshot of an application's desired configuration. A deploy always targets one revision.
- **Instance** — the placement of an application on one endpoint. One application may have several instances in later milestones; 0.1 uses one.
- **Deployment** — one attempt to make an instance match a revision, with per-step outcomes.
- **Resource** — a runtime object (container, image, volume, network, later Kubernetes objects) observed on an endpoint, owned by an instance or unmanaged.
- **Registry** — a named image source with optional credentials. A credential is sent only when the image reference's registry host exactly equals the registry's configured host (no suffix or wildcard matching), is never attached to a request that follows a redirect to another host, and a reference whose registry host has no configured `registries` row fails the preview and the pull with `registry_not_configured` by default. Anonymous pulls from unconfigured hosts are an explicit per-organization opt-in that only `registry.manage` (organization administrators) may enable, recorded as an audited setting change (`registry.anonymous_pull.enabled`).
- **Update policy** (M7b) — a rule for detecting and applying image updates.

Desired configuration, observed runtime, drift and last deployment are stored and shown separately; the UI never blends them.

## Tables

`applications`, `application_revisions`, `application_instances`, `deployments`, `deployment_events`, `registries`, `registry_credentials`, `update_policies`, `maintenance_windows`. Every row carries `organization_id`; rows that belong to an environment carry `environment_id`; composite foreign keys keep every reference inside one organization. Every table joins backup coverage in the slice that creates it.

## Implemented persistence foundation

Migration 18 stores applications and immutable revision history. Application names are unique inside their environment; composite foreign keys keep revisions in the same organization and environment as their application. All store operations require explicit environment scope and live named permissions. Create commits revision 1 with the application and audit; edit appends using an expected head number, rejecting a stale concurrent edit. Edits never update an existing revision. Explicit draft discard is the sole deletion path in this foundation. Audit contains scope, actor, application/revision target and outcome, never configuration. The SHA-256 digest covers the exact deterministic JSON encoding stored for that revision; reads recompute it and return `ErrRevisionCorrupt` on mismatch. It is a consistency check, not a signature or semantic equivalence check.

The typed `ApplicationSpec` holds `kind: compose.v1`, 1–100 uniquely named services, image references, optional restart/long-form ports and environment references. Raw YAML and literal values are never saved in the spec.

## Implemented draft import

The environment Applications view imports a single Compose document up to 64 KiB. The parser uses `go.yaml.in/yaml/v3` v3.0.4 with node/depth limits (8,192/16). Supported service fields are `image`, `environment` (explicit string map or `KEY=value` list), `restart` (`no`, `always`, `unless-stopped`, `on-failure`) and `ports` (at most 64 long-syntax entries with decimal `target`/`published`, optional IP `host_ip`, and `tcp`/`udp` protocol). Both ports must be 1–65535. No implicit environment/file lookup occurs. Interpolation is refused; literal dollars use `$$`, following [Compose escaping](https://docs.docker.com/reference/compose-file/interpolation/). Unsupported fields, multiple documents, duplicate keys, aliases, anchors and explicit tags are errors. Parser diagnostics contain fixed reasons and positions only.

Every environment value becomes a deterministic service/key reference. Migration 20 stores its encrypted bundle in `application_revisions.secrets_enc`, omitted from ordinary DTOs. The bundle is limited to 64 KiB serialized and 16 KiB per value; exact reference coverage is required. AES-GCM uses a derived key bound to organization, environment, application, revision number and spec digest. Internal `ResolveApplicationSecrets` checks live `secret.reveal` authority (organization administrators only) and commits audit before returning plaintext; no HTTP reveal endpoint exists. SQLite recovery proves decryption using the payload's recovered encryption key. Import/discard have no runtime side effects. Broader Compose parsing, public revision editing and deployment are still future work.

Admission limits are 100 applications per organization, 100 revisions per application and 64 KiB per encoded revision; an organization-row lock protects creation capacity across administrators. An environment containing an application cannot be removed. History is retained rather than automatically pruned. An administrator may explicitly discard an undeployed draft and all its saved revisions at an expected head using `application.destroy`; discard commits with audit, frees quota and permits deleting an emptied environment. Instance foreign keys restrict draft discard until explicit release. Managed application removal is a separate future workflow with retained deployment history. Both tables travel in the SQLite snapshot, verified by `TestApplicationRevisionsSurviveBackup`; PostgreSQL capsule limitations are unchanged.

## Target common revision model

```
revision {
  number, created_by, created_at, source (compose|import|kubernetes), note,
  spec: {
    kind: "compose.v1" | "kubernetes.v1",
    services: [ { name, image: {reference, digest}, ports, env: [{name, value|secret_ref}],
                  volumes: [{kind: named|bind|tmpfs, source, target, read_only}],
                  networks, command, healthcheck, restart, labels, depends_on } ],
    volumes, networks,
    extensions: { docker: {...}, kubernetes: {...} }
  },
  secret_refs: [ names ],
  image_digests: { reference -> digest }
}
```

- The common model holds what both runtimes can express honestly. Runtime-specific fields live under `extensions` and are preserved verbatim through edits; the model never pretends unlike constructs are the same.
- Image references are resolved to digests at preview time and recorded; the deploy uses the recorded digest, never the mutable tag.
- Plaintext secret values are never in a revision spec. `secret_ref` names a secret in the organization's secret store; values are encrypted at rest and resolved only inside the deploy command to the agent (see `agent-protocol.md` section 8).

## Target Compose subset (*proposed; not yet supported*)

Supported in 0.1: `services` with `image`, `container_name`, `command`, `entrypoint`, `environment` (list or map), `env_file` (resolved at import; every value becomes a secret reference unless the operator explicitly marks the key as plain configuration, and inline `environment` keys matching `PASSWORD|SECRET|TOKEN|KEY|PRIVATE` are offered the same conversion before the revision is saved), `ports`, `expose`, `volumes` (named, bind, tmpfs), `networks`, `depends_on`, `restart`, `healthcheck`, `labels`, `user`, `working_dir`, `cap_add`/`cap_drop`, `privileged` (shown with a warning), `logging`; top-level `volumes` and `networks`. Rejected with a named reason: `build`, `extends`, `profiles`, `secrets`/`configs` file mounts from the host, `deploy` (swarm), `x-` extensions other than KyYard's own. Image building is never added silently.

The implemented subset above is intentionally narrower than this target; unsupported fields are refused.

## Adoption and import

- Discovery lists Compose projects on an endpoint and distinguishes recorded adoption from unmanaged observations. The endpoint UI groups the existing authorized snapshot by exact project name, shows observed container/running counts and lets the operator filter the existing container controls. Empty and truncated reports describe only the reported inventory; freshness remains visible. Inspection exposes container name, image and state, not arbitrary labels, environment values or host configuration files. Discovery itself creates no application records and changes no runtime ownership. Inspect, import and adopt are distinct actions.
- **Import** reads the project into a new application and revision without changing the runtime. **Adopt** additionally marks the running resources as owned by the instance. Neither happens implicitly; unmanaged lifecycle actions (restart, logs) remain allowed by permission.
- Adopted resources keep their names; the deploy preview shows what a first deploy would recreate.

## Implemented adoption

Migration 21 records one instance per application and one owner per endpoint/project and endpoint/container ID. The imported desired revision and observed container snapshot remain separate: adoption does not certify matching configuration, infer a service mapping, relabel containers, deploy, or claim shared networks/volumes. The revision number is an association anchor, not a deployed revision.

An administrator selects a host/project, reviews all immutable container IDs, image IDs and creation times, and types the exact project name. The preview digest binds application ID/name/head, endpoint ID/name, project and sorted resource identities. POST recomputes it under application-then-endpoint locks. Identical later reports remain valid; changed/recreated resources or a changed application head invalidate confirmation. Require active Docker, nonempty complete container inventory received within three minutes and observed within five; truncated reports fail. At most 1,000 container ownership records per endpoint, at most one instance per application (100 applications per organization). SQL constraints serialize conflicting ownership and prevent cross-environment references; audit commits atomically.

Explicit release removes only the reviewed instance ID and its resource associations. An old release cannot affect a replacement adoption. No runtime commands are sent by adoption or release. Once released, a draft may be discarded. A future deployment slice must prevent releasing in-flight/deployed history and recheck every touched identity; names/project labels are never authority to claim a replacement container. Both new tables are proven in SQLite snapshot recovery. Endpoint discovery labels registered identities as adopted and leaves newly observed IDs unclaimed.

## Observed comparison

The adopted application configuration provides an on-demand read-only comparison against its latest saved revision. It reads the scoped instance, head/digest, endpoint and inventory together, then the immutable resource records. A concurrent release with no remaining resources refuses the result. The returned instance ID lets the UI reject a comparison for a replacement adoption.

Only active Docker with a complete container list received within three minutes and observed within five is compared. Unavailable, stale, malformed, duplicate-ID or truncated reports return an explicit availability state and no container observations. The result is bounded by 1,000 recorded resources plus 1,000 observed containers; UI tables page at 25 rows.

An adopted ID can be present, missing, changed in image/creation identity, or moved to a different reported project. Newly observed project IDs remain unowned. Only unchanged adopted identities with the current `com.docker.compose.service` label contribute to advisory service counts. Labels can be absent or trimmed, so zero never proves that a service is absent. Image comparisons use exact reference strings, not registry resolution or content identity; equal mutable tags are not proof of equal content. Environment values, ports, restart policies, mounts and networks are not compared. No secrets, arbitrary labels, runtime commands, persisted mapping, deployment preview or approval are produced. These observations are a diagnostic prerequisite; executable reconciliation still needs explicit mapping, pinned images and agent-side precondition enforcement.

## Deploy

1. **Preview** resolves the revision against the endpoint: image digests, secret references (names only), volume and network existence, port conflicts, and unsupported inputs. The preview is valid for *proposed* 10 minutes and records the observed identity (ID, digest, creation time) of every resource it plans to touch; at deploy time the agent re-checks those identities as preconditions, so unrelated changes on the host do not invalidate a preview but a change to a touched resource fails the step with `precondition_failed`.
2. **Deploy** takes an approved preview. Deployments serialize per instance (a per-instance lock row); a second request while one runs returns `409 deployment_in_progress`.
3. Each step (pull, create network, create volume, create container, start, health wait) records `succeeded`, `failed`, `skipped` or `unknown`. A partial failure leaves the recorded per-step outcome and the prior revision's references; nothing is auto-rolled back.
4. On success the instance's `current_revision` advances; the previous revision, its digests and the runtime result stay recorded for manual recovery.

## Delete semantics

- Discarding an undeployed draft explicitly deletes its saved configuration/history and changes no runtime resources.
- Removing a managed application (future deployment slice) stops and removes its containers and networks, keeps named volumes and images, and marks the application `removed` with its revisions retained for *proposed* 90 days.
- Volume deletion is a separate action (`volume.destroy`) with its own confirmation naming the data.
- Deleting an environment requires that it holds no applications and no unrevoked endpoints; the store enforces both conditions.

## Rollback honesty

- Rollback means deploying a previous recorded revision with its recorded digests. It never reverses writes to volumes or external data.
- The UI states, before confirming, when the target revision's images are no longer available, when its secrets have been rotated since, and that data changes are not reversed.
- M7b automated rollback (health-based) only targets a recorded available revision and reports the same limits.

## Kubernetes (M8)

The common model maps to Deployments/StatefulSets, Services, ConfigMaps, Secrets and PVCs. Migration classifies each service as `supported`, `operator_choice_required` (for example bind mounts, host networking) or `blocked` (privileged, host paths without a chosen storage class) and shows the classification before any change.

## Decisions

| Decision | Proposed | Status |
|---|---|---|
| Secret representation | reference-only specs, encrypted per-revision bundles in sealed backups; internal audited resolution | implemented for draft import; deploy resolution pending |
| Compose subset | list above | proposed |
| Unmanaged edits | require adoption | proposed (plan default) |
| Preview validity | 10 minutes, preconditions on touched resources only | proposed |
| `env_file` values | secret references by default, plain only by explicit choice | proposed |
| Unconfigured registry | refuse by default; anonymous pull is an audited per-organization opt-in gated by `registry.manage` (see `authorization-matrix.md`) | proposed |
| Application removal | keeps volumes and images | required by plan |
| Revision retention after removal | 90 days | proposed, see retention-policy.md |
