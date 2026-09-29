# Container management: detail pages, one-click actions, direct edit

**Status:** design for review, 2026-09-29. Decision taken with Yoshi: unmanaged containers are edited directly, not adopted first. This resolves the open question at `KyYard-Engineering-Handoff.md` line 434 and replaces the "configuration editing requires adoption" default in `docs/authorization-matrix.md` and `docs/application-schema.md`.

## Problem

Against a live install with a local Docker host, a remote Docker host and a Kubernetes cluster:

- Nothing lets an operator create a container or change an existing one. The only mutation path is Compose import, adoption by Compose project label, revision, plan, apply. A container not started by Compose cannot be changed at all. Kubernetes workloads are read-only.
- The container list has no uptime, no IP address, no health. The agent does not report them.
- Every action is behind an inline `<details>` disclosure inside the table cell, so opening it reflows the row. Logs and the terminal open as full-screen modals.
- There is no per-container page. Live inspection exists only behind a deployment preflight button.

## Goals

1. A container row shows state, health, uptime, IP addresses and ports at a glance, with actions as icon buttons that never reflow the table.
2. Clicking a container opens a page with tabs: Overview, Configuration, Logs, Terminal, Activity.
3. An organization administrator edits any Docker container's configuration on that page and saves it. Save recreates the container in place with the existing preconditioned rename/create/start/remove path. The same form runs a new container.
4. Kubernetes workloads get the equivalent: a workload page, rollout restart, scale, delete, edit (image, env, replicas, resources), and a terminal into a pod.

## Non-goals

- Image building. Never added silently (`application-schema.md`).
- Editing Compose applications through this path. An adopted container's page shows its application and links there; its Edit tab is disabled with that explanation, so desired state has one owner.
- Docker Swarm, Compose project creation from the form, volume or network editing.
- Changing the deploy engine's semantics for applications. It gains fields; existing plans render byte-identical.

## Delivery order

Three sub-projects, each its own plan and PR series. Later ones depend on earlier ones.

1. **Observe**: protocol additions, the detail page, the action toolbar. No new permission, no threat-model change.
2. **Edit Docker**: configuration read, edit form, recreate and run. New permission, threat-model and matrix changes.
3. **Kubernetes parity**: workload actions, workload page, pod terminal, workload edit.

## 1. Observe

### 1.1 Protocol: richer container report

`protocol.Container` (`internal/agent/protocol/inventory.go`) gains:

| Field | JSON | Source | Notes |
|---|---|---|---|
| `StartedAt time.Time` | `started_at` | `ContainerInspect.State.StartedAt` | zero when not running or unknown; the client shows uptime as now minus `started_at` and never trusts `status` text |
| `Health string` | `health` | `State.Health.Status` | `none`, `starting`, `healthy`, `unhealthy`, same vocabulary the inspection already uses |
| `Networks []NetworkAttachment` | `networks` | `NetworkSettings.Networks` | replaces `[]string`; `{name, ip, ip6, gateway?}`; bounded to 16 |
| `RestartPolicy string` | `restart_policy` | `HostConfig.RestartPolicy.Name` | the inspection already exposes it; the list needs it too |

`Networks` changes shape. This is protocol version 1 with an additive field set on the wire, so the server accepts both: a `[]string` decodes to attachments with empty IPs (older agent), a `[]NetworkAttachment` decodes as-is. The frontend `Container.networks` type becomes the attachment shape and `web/src/tenant.ts` normalises strings. The inventory's `/containers/json` list does not carry `StartedAt` or health, so the Docker adapter does one `ContainerInspect` per running container per report, under the report's existing deadline, and reports `started_at` zero for any it could not read within budget. The inventory is capped (`truncated`), so the extra calls are bounded by the same cap.

Kubernetes already reports pod `started_at`; the pod list shows uptime the same way.

### 1.2 Container page

Route: `/organizations/{org}/endpoints/{endpoint}/containers/{container}` in `web/src/router.ts` (`{ name: 'container'; org; endpoint; container }`). Container IDs are 64 hex, which passes the segment grammar. Kubernetes: `/organizations/{org}/endpoints/{endpoint}/workloads/{namespace}/{name}` (sub-project 3).

Page: `web/src/pages/ContainerPage.tsx`. Breadcrumb Endpoints / host / container. Heading: name, state badge, health badge, image. Tabs in the URL as `?tab=` so a reload lands on the same tab. Tabs are the existing `ky-resource-tabs` bar.

- **Overview.** Facts list: state, health, uptime (live, ticking every second from `started_at`), created, image and digest, restart policy and count, IP address per network, ports, mounts, labels, Compose project or application link, container ID. Below it the existing samples chart data as text (CPU, memory, network, pids) from `/containers/{c}/rollups`. This tab is `endpoint.read`.
- **Configuration.** Sub-project 2. Until then, the redacted inspection (`/containers/{c}/inspection`) rendered as facts. `endpoint.read`.
- **Logs.** The existing `ContainerLogs` component, inline, filling the page. `container.logs`.
- **Terminal.** The existing `ContainerTerminal`, inline, filling the page. Hidden unless `canExec`. `container.exec`.
- **Activity.** `/commands?container=<id>`: the endpoint's command list filtered server-side by container (new query parameter, same handler, same limit).

The page polls inventory every 30 seconds like `EndpointPage`, and stops on denial. When the container disappears from a fresh inventory, the page shows "This container is no longer reported" with a link back, and disables every action.

### 1.3 Action toolbar

`ContainerControls` becomes a horizontal group of icon buttons, in the table cell and on the page heading, using `lucide-react` (already a dependency): Play, Square, RotateCw, ScrollText (logs), Terminal, Trash2. Each button has `aria-label` and a `title`. Buttons that do not apply to the state are not rendered (start when running, stop when stopped), same rule as today. Disabled while a command is in flight. The `<details>` element, its CSS and the `web/AGENTS.md` line requiring it go.

Logs and Terminal buttons navigate to the container page's tab. In the table, they are links, not modals. The modal `<dialog>` code and `.ky-log-dialog` go.

Confirmations stay as they are: `window.confirm` for start, stop, restart, and the typed-name `window.prompt` for remove after the removal preview. The status message moves out of the table cell into a single `role="status"` line above the table (one per table, latest command), so a result never grows a row.

Mobile: the responsive table already renders labelled rows; the icon group wraps and each button keeps a 40px hit target.

### 1.4 List columns

Endpoint page container table: Container (name, image, ports, project), Status (state badge, health badge), Uptime, IP (first attachment's IP, `title` lists all), Usage, Actions. The Dashboard table gets the same columns minus Usage. Container names are `Link`s to the page.

### 1.5 Tests

- Protocol: decode `networks` as strings and as attachments; bounded attachments; `started_at` zero for a stopped container.
- Docker adapter: `TestInspectionRealDocker` gains `started_at` and IP assertions against the fixture container; a unit test with a fake daemon proves the per-container inspect budget drops to zero-value fields rather than failing the report.
- Router: the new route parses, rejects `..` segments, and round-trips `?tab=`.
- Page: vitest renders each tab, the disappeared state, the disabled actions, and asserts every icon button has an accessible name.
- Toolbar: existing `ContainerControls.test.tsx` cases re-targeted at buttons; a test proves opening the row's actions does not change the row's height (no expanded content rendered).
- Smoke: `scripts/smoke-test.sh` fetches the container page URL and asserts the SPA serves it.

## 2. Edit Docker

### 2.1 What changes in the trust model

Today environment values flow one way: from the encrypted revision bundle, through `secret.reveal`, into a deployment frame, to the agent, in memory only. Inspection is an allowlist that never carries env, labels, argv, mount paths or network names.

Direct edit adds the reverse flow: the agent reads a live container's full configuration and returns it to the server, which returns it to an administrator's browser. The values are held in memory on the server for the request only, never persisted, never logged, never in audit detail. This is the same class of exposure as a terminal, so it takes the same permission class. `docs/threat-model.md` gets a row; `docs/authorization-matrix.md` gets the permission and drops the "editing requires adoption" sentence; `docs/application-schema.md` line 85-86 and the Decisions row change to "unmanaged edits: direct, `container.configure`".

### 2.2 Permission

`container.configure` in `internal/permissions`: organization administrators only, like `container.exec`. Covers the configuration read, recreate and run. Audit rows: `container.configuration.read` (target container, no values), `container.recreate` (target, old and new image digest, the list of changed field names such as `env,ports`, never values), `container.run` (name, image digest).

### 2.3 Protocol: configuration read

New request/response frame pair `container.configuration` / `container.configuration.result`, capability `container.configure`, Docker agents only. Request: target identity `{container_id, image_id, created_unix}`, the same triple the inspection uses, so a swapped container is refused with `identity_mismatch`. Response `protocol.ContainerConfiguration`:

```
image {reference, id}          command []string   entrypoint []string
user, working_dir, hostname    env []{name, value}    labels map
restart {policy, retries}      ports []Port (with host_ip)
mounts []Mount (+ volume driver options omitted, tmpfs size)
networks []{name, aliases, ip (static, if set)}   network_mode
resources {cpus (nano), memory_bytes, memory_swap_bytes, pids_limit}
healthcheck {test, interval, timeout, retries, start_period}
privileged, read_only_rootfs, cap_add, cap_drop, security_opt, devices, extra_hosts, dns
log_driver, log_options, stop_signal, stop_timeout, tty, stdin_open, init
unsupported []string           observed_at
```

Every string and list is bounded by the deployment constants (`MaxDeploymentEnvEntries`, `MaxDeploymentEnvValueBytes`, `MaxDeploymentPorts`, `MaxMounts`, `MaxMountPathBytes`) plus new bounds for command, labels, devices and options; the agent truncates nothing silently, it lists what it dropped in `unsupported` with a code (`env_truncated`, `labels_truncated`), and the form refuses to save while `unsupported` is non-empty so a save never drops something the operator did not see. Frame size cap equals `MaxDeploymentRequestBytes`. The response is delivered on the agent's existing command channel with a 20-second deadline, and the server holds it only inside the HTTP handler that requested it.

The existing redacted `ContainerInspection` stays as it is for plans and validations.

### 2.4 Protocol: recreate and run

`protocol.DeploymentRequest`'s service gains the same fields the configuration carries (command, entrypoint, user, working_dir, hostname, labels, networks with aliases and static IP, network_mode, resources, healthcheck, privileged, capabilities, security_opt, devices, extra_hosts, dns, logging, stop signal and timeout, tty, stdin, init). Every one is optional and omitted for application deploys, so `Validate` and the rendered create body for an existing plan do not change (golden test). The agent's `create` step maps them onto the Docker create body. The `unsupported` refusals in the precondition step apply only when the request does not set the field: a request that carries `privileged: true` because the operator saw it and kept it is not "dropping" it. That is the one semantic change to the precondition, and it is gated on a new request field `explicit: true` that only the recreate handler sets.

A recreate is a `DeploymentRequest` with one service, `explicit: true`, the old container as the precondition target, `instance` empty, `owned_volumes` empty, and no Compose labels added (the operator's labels are sent as they are). Volumes named in mounts must already exist or be bind paths the old container already had, same rule as applications (`bind_mount_new`). A new bind path is allowed only in a run or recreate, with a `window.confirm`-class typed acknowledgement in the form ("this container will see host path X"), audited with the path.

Run is the same frame with no precondition target; the name must be free (`name_taken`). Image resolution reuses the plan path: reference to digest through `internal/registry`, with the pull step and registry credentials under `image.pull` rules.

Rollback: the deploy engine (`internal/runtime/docker/deploy.go`) renames the old container, creates the new one, stops the old, starts the new, then removes the old. On a failed `start` it stops there: the old container is left stopped under its renamed name and the new one exists but is not running, which is acceptable for an application (the operator reapplies a revision) but not for a one-off edit. Recreate adds a `rollback` step, taken only when `explicit` is set and `start` fails: remove the new container, rename the old one back, start it. Its outcome is recorded like any step, so a rollback that itself fails is visible and audited, never silent. The page shows the step list from the result like `ApplicationDeploymentPlan` does.

Env values in the recreate request travel exactly as they do for applications: in the frame, in memory, never logged. Because there is no revision bundle, there is nothing at rest. A later "save as application" is out of scope.

### 2.5 HTTP

| Method | Path | Permission | Body / result |
|---|---|---|---|
| GET | `/endpoints/{e}/containers/{c}/configuration` | `container.configure` | the configuration; `Cache-Control: no-store` |
| POST | `/endpoints/{e}/containers/{c}/recreate` | `container.configure` | `{expects: {image_id, created_unix, state}, spec, acknowledge_binds: [...]}` returns a command ID; polled through the existing `/commands/{id}` |
| POST | `/endpoints/{e}/containers` | `container.configure` | `{spec, acknowledge_binds}` returns a command ID |

Both writes run the plan-time checks the deployment path runs (image digest resolution, port conflict against the inventory, name grammar) synchronously and return 422 with the code list before dispatching. Dispatch is one deployment frame; the result is stored as a command row like every other command, so the Activity tab and audit see it.

Rate: one configuration read per container per 5 seconds per actor, one recreate in flight per endpoint (the agent's `busy` code already enforces the second).

### 2.6 UI

Configuration tab, administrators: a form generated from the configuration, grouped: Image (reference with digest shown, "pull latest for this tag" checkbox), Command (entrypoint, command, working dir, user), Environment (name/value rows, values masked with a reveal toggle per row and a "reveal all" that is itself audited as `container.configuration.reveal`), Ports, Volumes and binds, Network (mode, attachments with aliases and static IP), Restart policy, Resources (CPUs, memory, pids), Health check, Security (privileged, read-only root, capabilities, security options, devices) with the existing warning style, Logging, Misc (hostname, dns, extra hosts, stop signal and timeout, tty, stdin, init).

Save: a diff summary (field names, old and new for non-secret fields, "changed" for env values) and the typed container name, then submit. The page follows the command and reloads inventory on completion. Non-administrators see the same groups read-only from the redacted inspection with a note that editing is administrator-only, so the tab is useful to everyone.

Run: "Run a container" button on the endpoint page's Containers toolbar, opening the same form empty at `/containers/new` with `image` required.

Compose-adopted containers: the form is read-only with "Managed by application X. Edit it there." and a link.

### 2.7 Tests

- Protocol: `ContainerConfiguration` bounds, `unsupported` codes, `DeploymentRequest` golden render unchanged for existing plans, `explicit` gating of the unsupported refusal.
- Docker adapter: `TestConfigurationRealDocker` reads the fixture container and matches env, command, ports, mounts, resources; `TestRecreateRealDocker` changes an env value and a memory limit on a fixture, asserts the new container runs with them and the old one is gone; a run-then-remove test; a failed-start rollback test with an image whose entrypoint exits 1.
- API: permission matrix test (only OA), audit rows carry names not values, `no-store`, 422 codes, the in-flight limit, and that a managed container's recreate is refused with `application_managed`.
- Web: form round-trips a configuration to a spec byte-for-byte when nothing changed (the diff is empty and Save is disabled), masked env, reveal audit call, bind acknowledgement, managed read-only state.
- Smoke: `scripts/smoke-test.sh` runs a container through the API against the local Docker, edits it, removes it.

## 3. Kubernetes parity

### 3.1 RBAC

The generated manifest (`internal/runtime/kubernetes/manifest`) currently grants `get, list` cluster-wide and full verbs only in granted namespaces. Workload actions on arbitrary namespaces need `patch, update, delete` on deployments, statefulsets and daemonsets, `create` on `pods/exec`, and `delete` on pods, in the namespaces the operator grants. The manifest regenerates with a new revision; an agent whose ClusterRole predates it advertises no `kubernetes.workloads` capability and the UI says "regenerate and apply the manifest to enable workload actions" like `KubernetesManifest` does today for other capabilities. Granted namespaces remain the boundary: no action outside them.

### 3.2 Actions

Commands on the existing command channel, capability `kubernetes.workloads`, permission `container.operate` for restart and scale (they are reversible), `container.destroy` for delete, `container.configure` for edit:

| Action | Effect |
|---|---|
| `workload.restart` | patch the pod template annotation `kyyard.busnes.app/restarted-at` |
| `workload.scale` | patch `spec.replicas`; refused on a DaemonSet |
| `workload.delete` | delete the workload with propagation `Background`; typed name confirmation like container remove |
| `pod.delete` | delete one pod; typed name |
| `workload.configure` | read the workload's pod template (image, env from literal values and `envFrom` names, resources, replicas, args, command). Secret-backed env is shown by reference, never resolved: the ClusterRole never gets `secrets` |
| `workload.recreate` | patch the pod template: image, literal env, resources, replicas, command, args. Uses a strategic merge patch with `resourceVersion` as precondition so a concurrent edit is `conflict` |

KyYard-applied Deployments (labelled `managed-by: kyyard`) are refused for edit with `application_managed`, same as Docker, and restart and scale are allowed.

### 3.3 Pod terminal

`pod.exec` through the existing exec WebSocket relay: the agent opens `pods/{name}/exec` with SPDY through the clientset's `remotecommand` and bridges to the same stream framing the Docker exec uses. User selection does not exist in Kubernetes exec, so the form asks for container name and shell only; the typed-name confirmation stays. Permission `container.exec`, capability `pod.exec`, same session limits.

### 3.4 UI

Workload page at `/organizations/{org}/endpoints/{endpoint}/workloads/{ns}/{name}`: Overview (kind, replicas desired/ready/updated, images, conditions, pods with uptime, node, restarts, pod IPs), Configuration (edit form for the pod template), Logs (pod and container picker over the existing pod log component), Terminal (pod and container picker), Activity. The cluster view's workload and pod tables get the icon toolbar (restart, scale, delete, logs, terminal) and names become links.

### 3.5 Tests

Fake clientset tests for every action and the RBAC gate; `TestManifestOnARealCluster` extended with a restart, scale and edit against a disposable cluster; UI tests as in section 1.5.

## Docs to update on landing

- `docs/agent-protocol.md`: new frames, capabilities, inventory fields.
- `docs/authorization-matrix.md`: `container.configure`, workload rows, remove the adoption sentence, fix `container.read` "redacted env/labels" to say labels are reported.
- `docs/application-schema.md`: decisions row for unmanaged edits; line 85-86.
- `docs/threat-model.md`: row "Configuration read and recreate of any container" with the mitigations above and the tests that prove them.
- `web/AGENTS.md`: toolbar, container page, tabs in URL; remove the disclosure and modal rules.
- `internal/runtime/AGENTS.md`, `internal/agent/AGENTS.md`, `internal/api/AGENTS.md`, `internal/permissions/AGENTS.md`: their contracts.
- `KyYard-Engineering-Handoff.md` line 434: answered.
- `docs/ACCEPTANCE.md`: steps for edit and run.

## Decisions

| Question | Decision | Status |
|---|---|---|
| Edit unmanaged containers directly or adopt first | direct, `container.configure`, OA only | decided 2026-09-29 |
| Env values on the read path | in memory for the request, masked in UI, reveal audited, never persisted | decided |
| Uptime source | `started_at` from inspect, computed client-side | decided |
| Actions presentation | icon buttons, confirmations unchanged | decided |
| Logs and terminal | page tabs, not modals | decided |
| Compose-adopted containers | read-only here, edit in the application | decided |
| Kubernetes secret-backed env | reference only, ClusterRole never reads Secrets | decided |
| Save-as-application from a container | out of scope | deferred |
