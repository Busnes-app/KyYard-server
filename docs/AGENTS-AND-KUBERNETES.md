# Agents and Kubernetes

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

### Agent

`kyyard-agent` (built by `make build`) reads the enrollment token from stdin on first start, keeps its
Ed25519 identity in `--identity-dir` (default `/var/lib/kyyard-agent`, 0700), pins the control
plane's instance fingerprint, and holds one outbound WebSocket to `/api/agent/v1/connect` with a
30-second heartbeat, reconnecting with backoff. Plaintext `http://` is refused except to loopback.
It stays `pending` until approved, becomes `active` on its first inventory report, is shown
`offline` after three missed heartbeats, and exits when revoked. It reports a bounded inventory snapshot (engine facts, containers, images, networks, volumes;
never container environment) from `--docker-socket` at connect and every `--inventory-every`
(default one minute); the endpoint screen shows it with its age, and running containers carry the latest CPU, memory
and network sample (kept six hours, pruned every minute). Every 30 days (`--rotate-every`)
it offers a new key; the old key keeps working until an administrator acknowledges the new
fingerprint on the environment screen, and a second live connection for the same endpoint is
refused, flagged, and blocks rotation until cleared. Behind nginx add the standard
upgrade block from `scripts/spikes/websocket-proxy/nginx.conf` (`proxy_http_version 1.1`,
`Upgrade $http_upgrade`, `Connection $connection_upgrade` via the `map`); Caddy needs nothing. Lists accept `offset` (default 0) and
`limit` (default 50, maximum 200). Browser writes require the existing CSRF token.
Responses include a server-generated `X-Request-ID` for audit correlation.

| Organization membership | Read organization/environments/endpoints | Manage environments and endpoints | Manage members | Read tenant audit |
|---|---|---|---|---|
| Organization administrator | Yes | Yes | Yes | Yes |
| Environment administrator | Yes | Yes | No | No |
| Operator / developer / read-only | Yes | No | No | No |

These memberships apply throughout their organization; individual environment grants and
workload permissions arrive with their corresponding APIs. Successful operations and audit
records commit together. Audit failures prevent mutations. Historical platform events keep
their original meaning and are excluded from tenant history. Existing recovery and instance
settings retain platform authorization; tenant settings and credentials are not exposed yet.

### Kubernetes endpoints

A Kubernetes cluster enrolls as an endpoint the way a Docker host does. KyYard reads the whole
cluster (inventory, cluster health, pod logs), deploys stateless applications into the
namespaces you grant it, and there restarts, scales, edits and deletes workloads, deletes pods
and opens pod terminals. An organization administrator can also run a Deployment from an image in
a granted namespace (**Run a workload** on the cluster page, once its agent is upgraded to a
release that runs workloads); it is an ordinary unmanaged workload afterwards, edited and
deleted like any other. Prerequisites: `KY_APP_URL` on HTTPS, a digest-pinned agent image
(discovered from the installed server image, or `KY_AGENT_IMAGE`), and a cluster-admin
kubeconfig for `kubectl apply`. Upgrade the KyYard server before any cluster agent, never the
reverse: an older server refuses a newer agent at hello (the full order closes this section).

1. On the environment screen choose **Kubernetes cluster**, name the cluster, optionally list
   the namespaces KyYard may deploy to, and select **Enroll a cluster**. Create those
   namespaces first, labelled `pod-security.kubernetes.io/enforce=baseline` (or `restricted`);
   KyYard never creates one and refuses to deploy into one without that label. The manifest is shown once: download it (or copy
   it) and run the `kubectl apply -f kyyard-agent-<name>.yaml` command shown beside it.
2. Read the key fingerprint with `kubectl -n kyyard-agent logs deploy/kyyard-agent` and
   approve the matching endpoint.
3. Delete the spent enrollment Secret: `kubectl -n kyyard-agent delete secret kyyard-agent-enrollment`.
   The link inside it is single use, and the agent restarts from its identity Secret.

What the manifest creates, all labelled `app.kubernetes.io/name: kyyard-agent`,
`app.kubernetes.io/managed-by: kyyard`: the namespace `kyyard-agent`; the ServiceAccount
`kyyard-agent`; the ClusterRole and ClusterRoleBinding `kyyard-agent-read` (only `get` and
`list` on namespaces, nodes, pods, pod logs, events, services, persistentvolumeclaims,
deployments, statefulsets and daemonsets, plus `create` on `selfsubjectaccessreviews` so the
agent can ask what it may do: no Secrets, no ConfigMaps, no `watch`, no wildcard); the Role and
RoleBinding `kyyard-agent-identity` (`get`/`create` Secrets in `kyyard-agent` and `update` only
on `kyyard-agent-identity`, where the agent keeps its identity); in each listed namespace the
Role and RoleBinding `kyyard-agent-deploy` (`get`, `list`, `create`, `update`, `patch`,
`delete` on Deployments, Services and ConfigMaps; the same on Secrets except `list`; `get`,
`list`, `patch`, `update`, `delete` on StatefulSets and DaemonSets; `delete` on pods; `create` on
`pods/exec`; `get` on ReplicaSets, Jobs and CronJobs, to check a pod's controllers before exec or
delete); the Secret `kyyard-agent-enrollment`; and a one-replica
`Recreate` Deployment running `/app/kyyard-agent --kubernetes` as UID 65532 with a read-only
root filesystem (the `emptyDir` volume `scratch` at `/var/lib/kyyard-agent`, the agent's
`--scratch-dir`, holds its command ledgers), no privilege escalation, every capability dropped, seccomp `RuntimeDefault`,
and 50m/64Mi requested, 500m/256Mi limited. Applying it needs cluster-admin because it creates
cluster RBAC; the agent itself holds only the roles above.

What a listed namespace exposes: the agent can `get` any Secret in it by name (it cannot list
them, but names show through the cluster-wide pod and service reads), and creating Deployments
lets it run any pod there, under any of that namespace's ServiceAccounts and mounting any of its
Secrets; `pods/exec` lets it open a shell in any pod there. Where a namespace enforces no Pod Security level, such a pod could run privileged or on
the host network and reach the node. KyYard therefore deploys only into namespaces that enforce
Pod Security `baseline` or stricter: before every apply the agent reads the namespace's
`pod-security.kubernetes.io/enforce` label and stops (`pod_security`) when it is missing,
`privileged` or any value other than `baseline` or `restricted`. List only such namespaces. Pod logs and the metadata above are readable in every
namespace by design, and a namespace dropped from the list keeps its Role until you delete it.

To change the namespaces of an enrolled cluster, an organization or environment administrator
opens the cluster (or an application's Kubernetes card), selects **Regenerate manifest**, types
the list and applies the file shown with `kubectl apply -f`. It carries the RBAC and the agent
Deployment on this server's agent image (discovered, or `KY_AGENT_IMAGE`; without one the
request is refused `agent_image_unpinned`), but no enrollment Secret or link, so applying it
also upgrades the enrolled agent in place. A namespace you drop keeps its Role until you run
`kubectl -n <namespace> delete role,rolebinding kyyard-agent-deploy`. The list only decides
where KyYard lets you map applications; before every apply the agent asks the API server
whether it may create Deployments there and stops (`forbidden`) if the manifest was not
applied.

Deploying: open an application, choose the cluster and a namespace under **Deploy to a
Kubernetes cluster**, then plan and apply as on a Docker host. Each service becomes a
Deployment (one replica, rolling with no downtime, or `Recreate` when it mounts a volume), a
`ClusterIP` Service for its published ports, a ConfigMap and a Secret for its environment
(written per apply and pruned after the rollout), all named `<application>-<service>` and labelled
with the application and instance; every image is pinned by digest at plan time. KyYard never
touches an object with those names that it did not label (`name_taken`); the plan already stops
when the cluster's inventory shows another application's or tool's Deployment under a planned
name (`k8s_name_taken`), or when a service's name collides with a running one's once `_` and
`.` read as `-` (`a-b` beside or in place of `a_b`), which would move its objects
(`k8s_service_renamed`). The apply waits
for each rollout until the pod has been available for 10 seconds; one the cluster reports as
failed (a pod it refused to create, or the Deployment's own 9-minute progress deadline), or that
does not finish before the apply's ten-minute deadline, stops with the reason the cluster gives
(`FailedCreate`, `ImagePullBackOff`, `CrashLoopBackOff`, ..., or a volume claim still `Pending`
because no StorageClass has provisioned it, or, with a `WaitForFirstConsumer` class, its pod has
not been scheduled) and leaves the objects as applied.
A write the cluster refuses after the agent's grant allowed it (a quota, an admission policy) stops
with `admission_denied` and the object's name; a write the agent's Role does not grant (a manifest
older than the agent) stops with `forbidden`: apply the regenerated manifest. The rendered pod has no resource requests or
limits and no security context: the namespace must admit it, which Pod Security `baseline` does
and `restricted` does not, and a ResourceQuota or policy that requires limits refuses it.
A service with a host path, a port bound to a host address, or a restart policy other than
`always`/`unless-stopped` stops at the plan with the reason; a named volume needs a storage choice,
which a migration (below) records. The
kubelet pulls the images: for a private registry, give the namespace's `default` ServiceAccount
an imagePullSecret. A Kubernetes apply is health-validated like a Docker one (Update policies,
below): for two and a half minutes the server reads the Deployment and its pods, and an update a
policy applied that fails is returned to the digests the previous apply pulled, unless an apply
was sent since (whatever its outcome, running or unknown included) or the namespace, services or
volume claims changed. It sees what Kubernetes reports (replica counts, whether the rollout reached the latest change, container states and
waiting reasons, restarts); it cannot see whether the application answers requests, since KyYard
adds no readiness probe. A Deployment someone else edits, scales or recreates during the window is
`changed` and left alone — an autoscaled Deployment, whose generation changes on every scale, is
always `changed` and so is never validated. This needs a current agent image: an older one cannot
report workload status, every automated update pauses the policy, and the policy card says so.
Removing the application deletes the objects labelled as its own and nothing else.

Services of one application reach each other through their Kubernetes Service, by its object
name (`<application>-<service>`, for example `shop-web`, or `shop-web.shop.svc.cluster.local`
from another namespace) on the service's published port, which the Service forwards to the
target port. The Compose service name (`web`) does not resolve on a cluster, and a service with
no published port gets no Service, so nothing can reach it; publish every port another service
calls. A name longer than 63 characters, or two services whose names collide, carry a six-hex
suffix: the plan shows each service's object name.

Workloads: each Deployment, StatefulSet and DaemonSet in the cluster view links to its own page
(Overview, Configuration, Logs, Terminal, Activity). In a granted namespace, operators and
administrators restart and scale a workload (0 to 1000 replicas; a DaemonSet has no scale);
organization and environment administrators delete a workload or a pod by typing its name
(a StatefulSet whose `persistentVolumeClaimRetentionPolicy` would delete its claims is refused:
`whenDeleted: Delete` for a delete, `whenScaled: Delete` for a scale below its replicas; set
`Retain` first);
organization administrators edit a workload's images, commands, arguments, environment, CPU and
memory and replicas on its Configuration tab, and open a terminal in a running pod's container
(the shell you name; stderr is merged into the output). Edits and terminals need the namespace to
enforce Pod Security `baseline` or `restricted`, as deploying does: otherwise they stop with
`pod_security`, because editing or entering a privileged workload is host access. An edit is sent
as a strategic merge patch of only the fields it changed, at the version it read: a change
someone made in between is refused as a conflict, and every field the form does not show is
left as it was; a workload with a setting the form cannot carry (an environment entry from a
`fieldRef`, say) cannot be saved. Before a terminal attaches, the agent asks the API server
whether it may `create pods/exec` on that pod; a refusal ends the terminal with the reason
`forbidden`. A Deployment's
rollout after an edit is awaited unless it is paused; a StatefulSet's or DaemonSet's is not. Environment entries
from a Secret or ConfigMap show as references and are kept, but an apply request can reference
any Secret key in a granted namespace: whoever may edit workloads there can read its Secrets. A
workload KyYard deployed says which application manages it and changes through that
application (the server and the agent both refuse to edit or delete it:
`application_managed`); restart and scale still work. The workload's and the cluster's Activity tabs list every command. An agent
whose Role predates these actions answers `forbidden`: **Regenerate manifest** and apply it.

Docker container, image, network, volume, terminal, inspection, configuration and adoption routes
are refused for a cluster (`409 runtime_unsupported`). Uninstall with
`kubectl delete -f kyyard-agent-<name>.yaml`, then revoke the endpoint. To enroll the same
cluster again after a revocation, delete the Secret `kyyard-agent-identity` (or uninstall)
before applying a new manifest: an identity from an old enrollment refuses a new link.

Upgrade in this order: the KyYard server first (a newer agent is refused by an older server at
hello and loses even its inventory, and workload actions, edits and pod terminals need a server
that knows them); then **Regenerate manifest** and `kubectl apply -f` the file. It carries the
Roles a newer agent needs (without them every apply and workload action stops `forbidden`) and
the agent Deployment on the server's agent image with its `scratch` volume, so the apply upgrades
the agent in place. The identity Secret survives the new pod, so no re-enrollment is needed.
Until it is applied, an agent from an enrollment before the scratch volume existed logs that its
scratch directory is not writable and keeps its command ledgers in memory (a restart forgets
which commands finished). A new enrollment token mints a new enrollment link, which an
already-enrolled identity refuses: to change namespaces, use **Regenerate manifest**, which
carries no link.

### Migrating a Docker application to a cluster

An organization administrator opens an application adopted on a Docker host and, under
**Migrate to a Kubernetes cluster**, picks a cluster and one of its namespaces and selects
**Analyze**. KyYard reads the definition and a live inspection of each container and shows every
service on every axis (storage, networking, ports, secrets, probes, resources, scheduling, flags)
as supported, a choice to make, or blocked, each with its reason. Host paths, a volume two
services share, a volume the running container does not mount (its data cannot be verified as
the application's), host networking, host-bound ports, privileged and host-namespace settings, and a
restart policy of `no` or `on-failure` are blocked: change the definition or the container, then
**Analyze again**. For each named volume choose a StorageClass the cluster reports and a size
(`Mi`, `Gi` or `Ti`; Docker reports no volume size); it becomes a `ReadWriteOnce`
PersistentVolumeClaim named `<destination project>-<volume>` (for `shop on cluster-a`,
`shop-on-cluster-a-data`; a long name is cut and suffixed with a hash). In an application of
several services, each one is reached on the cluster only by its destination name
(`<destination project>-<service>`, shown on the report) and only on the ports it publishes:
acknowledge that you will update the references, and that a service publishing no port is
unreachable from the others, before the report is ready. The destination keeps its storage
choices when you edit its definition later; a volume it newly declares has none, so remove it or
migrate again. Analysis and choices read the cluster's inventory only while the destination is
connected and its inventory is fresh (received in the last three minutes, observed in the last
five); otherwise they answer `inventory_stale`.

When the report is ready, **Create destination** makes a new application, `<name> on <cluster>`,
mapped to the namespace, with a copy of the source's environment values. Then follow the
checklist; KyYard never stops, changes or removes the source for you:

1. Label the namespace `pod-security.kubernetes.io/enforce=baseline`. The command has no
   `--overwrite`, so it refuses to change an existing label; a namespace at `restricted` refuses
   the destination's pods (above). Then apply the regenerated manifest, which adds
   `persistentvolumeclaims` `get, list, create` to each namespace's Role and `storageclasses`
   `get, list` to the ClusterRole, and upgrade the agent image
   (`kubectl -n kyyard-agent set image deploy/kyyard-agent agent=ghcr.io/busnes-app/kyyard@sha256:<digest>`):
   an agent from before this release reports no StorageClasses and cannot apply claims, and a
   plan with claims is refused (`agent_claims_unsupported`) until it advertises
   `kubernetes.claims`.
2. Open the destination application, plan and apply it. The plan is refused
   (`storage_class_unknown`) while a claim's StorageClass, or a default class for a claim on the
   cluster default, is missing from the cluster's inventory.
3. With more than one service, change every reference one service makes to another by its
   Compose name (environment values, configuration) to the destination name the checklist lists,
   save the destination's definition and apply it.
4. Stop writes to the source, then copy each volume into its claim with the recipe the checklist
   prints. First set `HELPER_IMAGE` to a digest-pinned image that has `sh` and `tar`
   (`busybox@sha256:<digest>`); the source host and the cluster both run it. Per service the
   recipe scales the Deployment to 0 and waits for its pod to go, runs a helper pod
   (`<name>-copy`, sleeping until the recipe deletes it) that mounts the claim at `/to`, empties
   `/to` (`rm -rf /to/* /to/..?* /to/.[!.]*`), streams
   `docker run --rm -v <volume>:/from:ro "$HELPER_IMAGE" tar -C /from -cf - .` into
   `kubectl exec -i <name>-copy -- tar -C /to -xf -`, deletes the helper and scales back to 1.
   Emptying the claim removes what the destination wrote on its first start at step 2 (a
   database initializes a directory), so the two datasets never mix.
5. Validate the destination and select **Confirm validation** with a note.
6. Point your DNS or Ingress at the destination.
7. Select **Confirm cutover** with a note, then remove the source with KyYard's removal, which
   is refused while the migration is open.

**Abandon migration** closes it and keeps a created destination. Claims survive the removal of
their application; delete one deliberately with `kubectl -n <ns> delete pvc <name>` when its data
is no longer needed; the removal's result lists them under **Kept on the cluster**. Health probes, resource limits and a read-only root filesystem are not
carried over: acknowledge each drop on the report, then add them to the Deployment after cutover.

An abandoned migration's destination stays under its name; a new migration names its destination
`<name> on <cluster> (2)`, then `(3)`, up to `(9)`, and answers `application_name_taken` past that
(remove or discard an old destination). A destination name longer than 255 bytes is refused
(`destination_name_too_long`): rename the cluster.
