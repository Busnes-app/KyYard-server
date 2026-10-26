# Containers

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

### Show containers after installation

The standard Compose installation mounts the host Docker socket and runs the local connection
inside KyYard. After signing in and replacing the initial password, open **Containers**:
**Local Docker** appears in the initial organization's **Local** environment automatically.
No separate agent container, enrollment token, shell command or fingerprint approval is needed.
Container actions retain the same organization permissions and audit trail as remote hosts,
except that a terminal, configuration edit or removal on this host, an application removal
included, also needs a platform administrator. KyYard never opens, edits, stops or removes its own container through it.
The socket grants host-level Docker authority to KyYard.

**Existing installations:** update both the image and Compose file, then recreate KyYard using
your existing Compose overlays and data volume. Merely restarting an older container does not
add the socket mount. A `docker run` installation needs `-v /var/run/docker.sock:/var/run/docker.sock`.
The built-in connection resumes after server replacement automatically. An older standalone
same-host agent is no longer needed; stop it if you want to avoid listing that host twice.

If your shell reports permission denied for `/var/run/docker.sock`, use `sudo docker compose`
for installation/update commands. Do not make the socket world-writable. The generated remote-host command uses `sudo docker run`; omit `sudo` if your account already has Docker access.

For rootless or nonstandard Docker, set `KY_DOCKER_SOCKET_PATH` to the host socket path for
Compose's bind mount. Inside KyYard, `KY_DOCKER_SOCKET` defaults to `/var/run/docker.sock`;
set it explicitly empty for a control-plane-only process. Remove the socket mount as well
if the deployment must have no host Docker authority. Without a readable socket, check the
server's `[DOCKER]` / runtime messages; KyYard's other functions remain available.

### Container user

The server runs as the unprivileged `kyyard` user (uid/gid 10001). The image entrypoint starts
as root only to chown everything under `KY_DATA_DIR` (and `KY_BACKUP_DIR`) that `kyyard` does not
already own, and to add `kyyard` to a group with the mounted socket's gid; then it drops to
`kyyard`. Existing root-owned volumes and `./data` bind mounts are converted on the first start,
with no manual step; a bind-mounted `./data` is then owned by uid 10001 on the host. This limits
what a compromised server process can do inside the container, but **Docker socket access is
still host-root-equivalent**: whoever controls the socket controls the host.

If `kyyard` still cannot write the data directory, use the socket (for example a socket with mode
0600) or read `/etc/kyyard-db-ca.crt`, the server keeps running as root, hands the files back to
root and logs `kyyard: running as root`. Fix the cause (make the PostgreSQL CA file readable, for
example mode 0644) and recreate the container to drop root. `docker run --user` skips the
entrypoint's root steps; that user then needs the data directory and the socket itself.
Run in-container CLI commands as the server user: `docker compose exec --user kyyard kyyard
/app/kyyard-server <command>`.

The local endpoint is initialized once. Revocation remains effective across restarts and
environment deletion; startup never restores revoked authority. Its identity derives from
the backed-up instance key, and its inventory generation resumes from the database.

**Additional hosts / manual agent installations:**

1. Open the organization (**Home → Environments & add endpoint**, or `/organizations/<org>`).
   A fresh install has no organization yet: Home then says so, and a platform administrator
   creates one under **Settings → Administration → Organizations**, naming the first
   administrator. Until then every other account sees "No host access yet".
2. Create or open an environment, then choose **Enroll a host**.
3. Run the displayed command on the remote host. It pulls the image and starts one persistent
   agent container with an enrollment link. No server container is required on that host.
4. Run `sudo docker logs kyyard-agent`, click **Refresh hosts**, compare
   the full **agent key fingerprint**, then **Approve**.
5. Return to **Containers**. Inventory arrives after approval and updates every minute.

The command has this shape (copy the real link from KyYard):

```sh
sudo docker run -d --name kyyard-agent --restart unless-stopped --pull always \
  --no-healthcheck --entrypoint /app/kyyard-agent \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v kyyard-agent-identity:/var/lib/kyyard-agent \
  'ghcr.io/busnes-app/kyyard@sha256:<verified-digest>' --link '<enrollment-link>' --name "$(hostname)"
```

The agent enrolls on first start, then reuses its saved identity on restart, even after
that one-use link expires. A different link refuses an existing identity volume. Keep
`kyyard-agent-identity`; to enroll into another environment, revoke the old endpoint and
choose a new volume. The link is a short-lived secret visible in shell history and Docker
container arguments; it is never logged by the agent or saved in its identity file (only
its hash is retained to recognize the original link on restart).

The agent uses Docker's default bridge and connects outbound to the server's HTTPS address;
no ports or dedicated network are needed. Mounting the Docker socket gives it root-equivalent
host access. Replacing the server does not require recreating remote agent containers.

**Legacy/manual same-host agent replacement:** the standalone same-host agent shares the old server container's network
namespace. Stop/remove the agent with `docker rm -fv kyyard-agent`, replace the server, then run
this command on that host to reconnect the existing identity using the new installed image:

```sh
server=kyyard
server_id=$(docker inspect --type container --format '{{.Id}}' "$server") &&
image=$(docker inspect --type container --format '{{.Image}}' "$server_id") &&
docker run -d --name kyyard-agent --restart unless-stopped --pull never --no-healthcheck \
  --network "container:$server_id" --entrypoint /app/kyyard-agent \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v kyyard-agent-identity:/var/lib/kyyard-agent "$image"
```

A link for another enrollment refuses an existing identity; use the
restart command above to reconnect legacy agents. To enroll into another environment, revoke the old
endpoint and explicitly choose a new identity volume for the new enrollment.

Keep the named identity volume; `docker rm -fv` removes anonymous volumes, not named ones.
No new token or approval is needed for that existing identity. A revoked endpoint cannot be
revived this way. Never share an identity volume between hosts or control planes. Use distinct
agent container/volume names if running more than one installation on a host.

**Remote server address:** configure `KY_APP_URL` as reachable HTTPS with a trusted reverse
proxy before generating the command. An HTTP-only installation shows setup guidance rather
than a misleading same-host command. The command automatically pins the installed server image’s official repository digest,
read through the local Docker socket using the container hostname and immutable image ID.
It never resolves `:latest` when the command runs. A source build, unavailable socket,
custom container hostname or image without an official repository digest requires
`KY_AGENT_IMAGE=ghcr.io/busnes-app/kyyard@sha256:<verified-digest>` in the Compose service
environment. Without a pin, the screen shows setup guidance rather than a runnable command.
Restarting an existing agent does not upgrade its image. Every generated image is digest-pinned; see [RESTORE.md](RESTORE.md) for verification.

**Source installation:** `make build` produces `./kyyard-agent`. Use `--link '<enrollment-link>'`
or expand **Source installation** for the token and pipe it to
`./kyyard-agent --server <server-origin> --identity-dir <private-directory>`.
The process needs Docker socket access and a service manager. The legacy stdin path accepts
HTTP only on loopback; enrollment links and remote connections require HTTPS.

### Static internal IPs for proxy backends

After mapping an application to a Kubernetes namespace, open its **Internal networking** panel.
Enter an unused address from your cluster's **Service IP range** for each service you want to
pin (IPv4 or canonical IPv6). Each service needs a published port in its definition. Save the
networking revision, then plan and apply it. Saving preserves the encrypted environment values
and makes no runtime changes. Upgrade the cluster agent first: static IPs require its
`kubernetes.service_ips` capability.

Blank means automatic allocation for a newly created Service. Existing Services always keep
their assigned IP; to pin one, enter that existing address. KyYard refuses a different requested
IP rather than replacing the Service. Kubernetes rejects occupied or out-of-range addresses
when the Service is created, before that service's workload is changed. A plan does not reserve
an address. Removing the Service (including by removing its last published port or the
application) releases the address.

The panel shows the last-reported backend address and ports for Nginx or cloudflared running
**inside the same cluster**, for example `http://10.96.10.10:8080`; use `https://` when the backend
itself serves TLS. Refresh backend addresses after deployment. The proxy continues to use the
same Service IP across pod restarts and application updates. Proxy configuration and access
restrictions remain the operator's responsibility; this does not publish a LAN or public IP.

### Compose projects

Open a host from **Home** or **Endpoints**, then expand a project under **Compose projects** to inspect its reported containers and running count. **Show containers** filters the existing controls; **Show all containers** clears the filter. Projects are currently unmanaged: discovery does not import configuration, take ownership, or deploy anything. Counts reflect the displayed inventory age and may be incomplete when the container list is truncated.

### Container terminals

Open an endpoint, choose **Terminal** on a running container, enter the container user
(for example `1000` or `app`), choose the shell executable, and confirm the container
name. Only organization administrators can open terminals. Local and remote Docker
hosts use the same authorization; remote agents must run a version with exec support.
The browser must use the configured `KY_APP_URL` origin and the reverse proxy must
forward WebSocket upgrades.

Resize with the row/column controls. Closing the tab or withdrawing access disconnects
the attachment; it does **not** guarantee termination of a process inside the container.
There is no automatic reconnect or execution retry. Sessions expire after 15 minutes
without input or eight hours total. Slow-client buffer limits also disconnect rather
than silently lose terminal data. Audit records who connected, the target, selected
user, duration and known exit status, without recording commands or terminal contents.
An interrupted session without an inspected exit code remains unknown.

### Edit or run a container

An organization administrator edits a container that no application owns on its
**Configuration** tab: KyYard reads the full configuration from the host, environment values
included (masked until **Reveal**), and **Save and recreate** replaces the container with the
edited copy after you type its name. The host agent must advertise `container.configure`.
Settings KyYard cannot read (listed on the tab) block saving, so a recreate never drops them
silently. A new host path bind needs its own acknowledgement. Host-level settings (privileged,
devices, extra capabilities, host networking, system path binds) are refused unless
`KY_CONTAINER_ALLOW_PRIVILEGED` is on; that guards against mistakes, not against an
administrator (see the variable). A container whose start fails, or that stops or restarts
within five seconds of replacing a running one, is rolled back to the old one. Environment, labels and command a container inherited
from its image are kept as they were read, even when you change the image. KyYard refuses to
recreate the container its own agent runs in (the server's, or a host agent's): stopping it
would stop the agent doing the recreate. **Run a container** on a Docker host's page creates a new one the
same way. A container an adopted application owns is edited through that application.
Environment values travel only inside the agent's TLS session and the HTTPS response and are
never stored, audited or logged; audit records who read or changed which container and the
names of the settings set. Reads, recreates and runs share a budget of 12 per administrator
per minute.

## Import a Compose draft

Open **Endpoints → Environments & add host → your environment → Applications**.
Choose **Import Compose draft**, enter a name and paste YAML. Import creates saved
configuration only; it does not deploy, adopt or change existing containers.

This initial importer accepts service `image`, explicit string `environment`,
`restart`, long-form `ports` (`target`, `published`, optional `host_ip` and
`protocol`) and `volumes`: named volumes declared under top-level `volumes`
(optionally `external: true`) and absolute bind paths, short or long syntax, `ro` or
`rw`. Quote environment numbers and booleans. Supply resolved values;
interpolation and file lookups are unsupported, and literal dollars must use `$$`.
Documents are limited to 64 KiB. Other fields, including networks, builds, commands,
tmpfs, anonymous volumes, relative bind paths, volume drivers, anchors and aliases, are
rejected rather than silently dropped.

All environment values are encrypted. Saved configuration shows keys and references,
not values. Administrators can import and discard drafts; discard deletes their
saved history and changes no running containers. Editing, adoption and deployment
follow below. See [the application contract](application-schema.md)
for the implemented limits and the separate target Compose feature set.

To adopt an existing project, open an application's configuration, choose its host
and Compose project, review the exact container identities, and type the project
name. Adoption records an association only; it does not prove the imported revision
matches the host or start a deployment. The host must be active with recent,
complete inventory. Networks and volumes remain unowned. **Release adoption**
removes that association without stopping containers; release it before discarding
the draft. Replacement containers are not automatically adopted.

Use **Saved revision** to inspect earlier definitions. **Save new revision** accepts
a complete replacement Compose definition, including all environment values.
Omitted services/values are not copied forward. Saving preserves previous revisions
and changes no containers. A conflicting or uncertain save requires refreshing
Applications before trying again. History is limited to 100 revisions per application.

Use **Map services to containers** to explicitly associate saved service names
with already-adopted container IDs. Leave unused entries unmapped. Review the
host, project and full IDs, then type the project name to save. This changes no
containers. If the definition changes, review and save the mapping again; missing
or replaced adopted containers require resolving adoption first.

Use **Compare with host** in an adopted application's configuration to inspect
missing/replaced identities, unowned project containers and observed service/image
reference differences. The view is read-only and paginated. Stale or partial
inventory produces an unknown result. Service labels and matching image tags do
not prove configuration parity; environment values and other runtime settings
are not compared. This does not deploy or authorize changes.

Use **Deployment preflight** to check the saved mapping, resolve exact image
references from the host's image inventory and find reported published-port
overlaps. Missing images/references require pulling or correcting the definition,
then refreshing inventory. Preflight is read-only: runtime configuration, host
processes and unreported port bindings are not checked, and reported image IDs are
not saved deployment pins. Each service lists the mounts a recreate will use. A deploy
creates a missing project volume (`<project>_<name>`, as Compose names it) and never adds a
host path: a bind the running container does not already have blocks the plan, so mount it
by hand first or drop it. Mounts the definition drops are listed and do not block. With no
findings left it says **Ready to plan**; otherwise
it lists what to fix, or says **Not ready to plan.** when only per-service findings
remain in the table. **Deployment plan** then pins the images and replaced containers,
and **Apply deployment** runs it on the host.

The authenticated container inspection API is
`GET /api/organizations/{organization}/endpoints/{endpoint}/containers/{container}/inspection`.
Use the full reported container ID. It returns redacted live runtime facts when
the agent supports `container.inspect`; older agents return an upgrade response.
The request requires fresh inventory and ends on access loss or disconnect.
This API does not expose secrets or enable application deployment.

## Update policies

An organization administrator can give an adopted, mapped application an **Update policy**:
a weekly maintenance window (days, start and end in a time zone you choose) and a mode. In
each window the server checks the application's images and, when a registry has a newer
image, plans the update (**Plan only**, which waits for someone to apply it) or plans and
applies it (**Plan and apply**). The policy acts as the administrator who last saved it: if
that person can no longer deploy, the policy pauses itself instead. Three failed windows in a
row also pause it; **Resume** starts it again. Windows are wall-clock times: a start that a
daylight-saving jump skips does not run that day, and a window cannot cross midnight. A window
missed while the server was down is recorded, not run late. Every run, and every step it takes,
is in the audit log under one correlation ID.

After every deployment the server watches the containers for two and a half minutes: each must
keep running, pass its own healthcheck if the image has one, and not restart. The verdict is
shown on the deployment and on the policy's run list. When an update the policy applied fails,
the server returns the application to the previous deployment's exact images if they are still
on the host, or says why it cannot; either way the policy pauses until you resume it. Updates you
apply by hand are judged the same way but never rolled back automatically, including a plan a
policy run left behind. This needs the host's agent to report container health: the policy card
warns when it cannot. Upgrade the server before its agents: a server older than health reporting
refuses a health agent's inspection answers and closes its connection.

Automated updates need an agent with live inspection support (`container.inspect.verdict`);
with an older agent every window is `blocked` until the agent is upgraded, and three such
windows pause the policy. A run inspects as the administrator it acts for and shares that
person's inspection budget (30 a minute): a run that cannot inspect is `blocked` with
`inspection_unavailable`.

The scheduler runs inside the server; there is nothing extra to deploy. The binary embeds the
time-zone database (`time/tzdata`), so a bare binary on a host without `/usr/share/zoneinfo`
evaluates zones the same way as the container.

### Fleet, image runs and YAML

**Home** shows Docker and Kubernetes endpoint summaries, connected endpoints first, with a direct **Run a container** action. Kubernetes runs a container image as a Deployment in a granted namespace. If setup is missing, the run page offers **Regenerate manifest** to upgrade the agent and grant namespaces; apply that manifest with cluster-admin credentials. Native YAML requires the agent capability `kubernetes.manifests.run`. Upgrade the server before the agent, and keep `KY_AGENT_IMAGE` pinned to the published release digest.

The run forms show name and image first; expand Environment, Ports, Volumes or Advanced settings when needed. **Docker Compose YAML** previews the supported import subset (image, explicit string environment, restart, quoted short or long-form ports and binds/existing external named volumes). **Kubernetes YAML** previews native apps/v1 Deployment documents, preserving selectors, ports, probes and existing storage/Secret references. A YAML run may name any Secret in a granted namespace, so running Kubernetes YAML is equivalent to reading every Secret there; grant only namespaces whose Secrets every organization administrator may read. At most 16 resources and 64 KiB; unsupported kinds and fields refuse the entire preview. Service and PVC creation uses Applications. Each reviewed resource needs its own typed name and runs independently; a failure leaves prior successes in place. Custom service accounts and service-account token mounts are refused.

Visible Docker rows automatically check for image updates for administrators on active hosts. Checks run one at a time within the request budget; results stay cached for five minutes while the endpoint page is open. The refresh icon requests a fresh check. Checks observe registry metadata and do not pull or replace containers. A fresh digest comparison can show **Update available**, **Up to date**, **Digest pinned**, **Image version unknown** or a specific registry failure. **Registry access not configured** means the image host needs a registry entry or the administrator must enable anonymous pulls in the organization's **Access → Registries** (`/organizations/<org>/access`; the badge's **Configure registry access** link goes there); the check never changes that policy. **Update image** opens Configuration; **Pull latest image and recreate** resolves/pulls the reference again and replaces the container while carrying its reviewed settings. Review and type its name to submit. Data volumes are preserved; a managed application must be updated through Applications. Digest-pinned references stay pinned unless the operator changes the reference.

Deployments, StatefulSets and DaemonSets on a cluster, outside applications, get the same check on the cluster page and the workload page, and the same one-click **Update image**, which resolves each tag at the registry and pins the pods to its digest. Private images still need an `imagePullSecret` in the workload's namespace. In the workload form, tick **Pin the reference's current digest** to store a tag as `tag@digest`.
