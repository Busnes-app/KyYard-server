# 0.1 acceptance runbook

The human acceptance script from `KyYard-Implementation-Plan.md` section 7, expanded against
the current UI. A preparer sets the stage and observes; an operator who knows containers but
not KyYard does the work through the UI. The operator gets only the operator card below. This
runbook is for the preparer: what to set up, where each task lives, what passes, and what to
record.

## Release-defect rule

Copied from the plan, section 7:

> Record success/failure, confusing steps and recovery outcomes. Any required documentation lookup, cross-tenant exposure, unexplained unknown action, lost secret, or unusable recovery is a release defect. Fix and repeat affected paths before calling 0.1 ready for internal use.

## Known gaps

Found while writing this runbook against the current UI. Each one the operator runs into goes
in the results table.

- Direct YAML runs accept the documented Compose subset and native Deployments only; other Kubernetes resource kinds use Applications. Image hints require an explicit registry check, and an unknown digest is not proof of being current.
- Registry access (needed for update checks) is set on the Members page, not near Updates.
- Platform audit (sign-in, password change, backup, `organization.create`, `user.create`) has
  no screen; only tenant audit does.
- The audit Actor column shows user IDs (`usr_…`), not names. Keep the list of which ID is
  which account from the prerequisites for step 8.
- Container command results (restart, start, stop, remove) are not audited as results; the
  audit row is the request, under its permission. Run and recreate results are audited.
- A container edit keeps the environment, labels and command the old image gave the container:
  the read cannot tell them from the operator's, so after an image change they are pinned to
  the old image's values.
- Recreating the KyYard server container, or a host agent's own container, through KyYard is
  unsupported: the stop step stops the agent running the recreate.
- A recreate of a running container waits five seconds for the new one to keep running
  without a restart; a healthcheck that fails later is not awaited. A stopped container's
  replacement is started and not watched.
- A daemon configured with `default-cgroupns-mode` or `default-shm-size` makes every container
  read `host_config:CgroupnsMode` or `host_config:ShmSize`, which blocks the edit, until the
  adapter learns the daemon default. The read assumes Docker's own defaults: cgroup namespace
  `host` on a cgroup v1 host, `private` otherwise, and 64 MiB of shm.
- The run's behaviour on a real cluster is unproven: CI covers it with a fake clientset only.
- A source build (`docker-compose.build.yml`) shows an enrollment token and a "Source
  installation" note instead of the one-line command: the image has no published digest to
  pin. Set `KY_AGENT_IMAGE` (README) or compose the `docker run` by hand from the token.
- Approve, Restart, Revoke, Save revision, Pin key and "Allow anonymous pulls" use the
  browser's native confirm dialog; the fingerprint to compare is in that dialog's text.
- After "Adopt reviewed containers" and after "Save revision N" the configuration view
  collapses to the application list; reopen "View configuration" to continue. The status text
  shows only inside the reopened view.
- "Plan update" plans the latest saved revision, not the current one: after a put-back to an
  earlier revision, applying an update also moves the definition forward. The plan panel says
  which revision it planned.
- An apply replaces every mapped container of the project, not only the services whose image
  or definition changed.
- A second organization's Containers home says "The standard installation connects local
  Docker automatically; check Docker access if it is missing." Local Docker belongs to the
  initial organization only.
- A revoked host's page says "No inventory yet. It arrives with the agent's first report after
  approval."; a revoked host never reports again. Its agent container, if started with
  `--restart unless-stopped`, restarts every few seconds and writes an `agent.connect denied`
  audit row each time: remove the container after revoking.
- A read-only member still sees the mutating controls (Save new revision, Release adoption, Map
  services to containers, Deployment plan, the registry form); the server refuses them.
- Revisions that differ only in encrypted environment values show the same digest.
- Recreated containers show their image ID, not the tag, in `docker ps` on the host: the
  recreate pins by ID.
- The agent container itself carries an anonymous volume for the image's declared `/data`.
- Making a read-only bind writable on a container's Configuration tab shows no acknowledgement
  checkbox, and Save answers "Acknowledge every new host path.": the server counts it as a new
  bind and the form does not.
- A cluster agent whose Role predates workload actions shows Restart, Scale, Delete and Save
  as usual; the command then answers "The agent's role does not allow this. Regenerate and
  apply the cluster manifest, then retry." Nothing warns before the attempt.
- A DaemonSet has no Scale button.
- After a workload edit, a StatefulSet's, DaemonSet's or paused Deployment's rollout is not
  awaited: the step table shows the rollout step skipped.
- A pod terminal's 15-minute idle timeout counts only typing and resizing: a session that
  only prints output (`top`) closes after 15 minutes.
- A pod terminal the agent refuses before attaching ends with the `exec.close` reason
  `forbidden` (its Role lacks `create pods/exec`: regenerate and apply the manifest) or
  `pod_security` (the namespace does not enforce Pod Security baseline or restricted, or the
  pod itself does not meet baseline; the text does not say which). Other
  failures after "Connected. Terminal contents are not recorded." (the pod replaced under
  its name, a lost connection) end with the generic "Terminal ended or was refused. …".
- A cluster agent enrolled before this release has no scratch volume until the regenerated
  manifest is applied: until then it logs that its scratch directory is not writable and
  keeps its command ledgers in memory.
- The real-cluster tests are unproven on this branch: `TestManifestOnARealCluster` and
  `TestPodExecOnARealCluster` run only with `KY_TEST_KUBECONFIG` against a disposable cluster,
  and none was available. Every other Kubernetes test runs against the fake clientset.

## Prerequisites

Set these up before the operator arrives. Note every command you ran in the run record.

**Control plane.** A clean installation from the shipped `docker-compose.yml` on a machine the
hosts can reach over HTTPS: remote enrollment refuses plain HTTP. Follow README "Transport and
advanced deployment" for the proxy overlay and `KY_APP_URL`. Set `KY_BACKUP_DIR` so capsules
have a local destination. Record the commit and image digest.

**Recovery.** A scratch suite ceremony (2 of 3 is enough) whose public key you can paste, or a
paired KyRecovery, plus custodians with their cards for step 9. Use a scratch ceremony, never
the production suite key.

**Two disposable Docker hosts**, A and B, with outbound HTTPS to the control plane. Nothing on
them you would miss. Their agents must run the same release as the control plane: an older
agent reports no container mounts, and every deployment plan on its host is blocked with
"The agent has not reported this container's mounts, or reported only part of them. Upgrade
the host agent to this release, then check again." until it is upgraded. Planning inspects each
mapped container live through its host's agent, so the agent must be online when you plan: an
offline agent gives "The live container could not be inspected before planning…", and an agent
without live inspection, or built before this release's inspection verdict, "Upgrade the host
agent to enable live inspection, which planning requires." In the other direction, an agent
newer than the control plane makes the inspection dialog show 502 for a container with nothing
a recreate would drop until the control plane is upgraded. Keep each host's clock within five minutes of the control plane's (NTP); otherwise
preflight shows the clock blocker and no plan is made.

**Sample application on host A**, one Compose project, `acc-app`:

- A `data` service that keeps its data in a named volume `data`, declared under top-level
  `volumes` (on the host it is `acc-app_data`), and a stateless `web` service with a
  published port that reaches `data` by service name.
- Use only what the import accepts: `image`, `environment` (quoted string values), `restart`,
  long-syntax `ports` (`target`, `published`, optional `host_ip`, `protocol`) and `volumes`
  (named volumes, or absolute bind paths the container already has). No `command`,
  `entrypoint`, `user`, `networks`, `tmpfs` or other extra configuration. If an image declares
  a `VOLUME`, mount the named volume at exactly that path: otherwise Docker adds an anonymous
  volume, which KyYard refuses to recreate. At least one environment value stands in for a
  secret. Start it with `docker compose up -d` from the file you will hand the operator to
  import.
- For step 6, one `acc-app` image must run at an older digest of a tag that has since moved.
  Pull the old digest and tag it before `up`: `docker pull <repo>@<old digest>` then
  `docker tag <repo>@<old digest> <repo>:<tag>` (unproven). If Check for updates later says "Up to
  date" or "Unknown on host", the setup did not take; fix it and rerun step 6. To find a moved
  tag, compare the tag's current digest from `docker buildx imagetools inspect <tag>` with the
  local `RepoDigests`; the local copy is older when they differ.
- Write a marker into the `acc-app_data` volume (a row, a file) and note it.

**Optional: a disposable Kubernetes cluster** for step 3c, enrolled and approved in the same
environment with namespace `acc` granted and labelled
`pod-security.kubernetes.io/enforce=baseline`. In `acc`, create with `kubectl` (not through
KyYard) a one-replica Deployment `acc-web` of a digest-pinned image that has `/bin/sh` and keeps
running, with one environment variable `ACCEPT_KEY=one`. Also create a two-replica StatefulSet
`acc-db` of the same image with a `volumeClaimTemplates` entry and
`persistentVolumeClaimRetentionPolicy: {whenScaled: Delete, whenDeleted: Delete}`, and a one-replica Deployment
`acc-priv` of the same image with `securityContext.privileged: true`: create `acc-priv` while
`acc` is labelled `enforce=privileged`, then relabel it `enforce=baseline` (`kubectl label
--overwrite`; the running pod keeps running). Workload edits and pod terminals are refused (`pod_security`) in a namespace
without that label. If the cluster was enrolled
before this release, upgrade the server first, then **Regenerate manifest** and apply it: it
carries the new Role rules and the agent Deployment (with its scratch volume) on the server's
agent image, so the enrolled agent is upgraded in place.

**Accounts and two organizations**, all through the UI, signed in as the bootstrap `admin`
(a platform administrator and the administrator of the initial organization, `org_initial`):

1. Settings, Administration, Users: Create user `reader`, then `outsider`, both with role
   User. Each shows its temporary password once; copy it before creating the next.
2. Organizations: Create organization "Acceptance B" with First administrator `outsider`.
3. Note each account's ID and Acceptance B's ID from the ID columns in Settings →
   Administration.
4. In `org_initial`, Environments, Members: Add member by user ID, reader's ID, role "read
   only", Add.
5. Sign in as reader and as outsider in turn and replace each temporary password. Outsider
   lands in Acceptance B, its only organization, and its Environments page loads.

| Account | Platform role | Membership | Used in |
|---|---|---|---|
| admin | administrator | organization administrator of `org_initial` | every step |
| reader | user | read only in `org_initial` | step 4 |
| outsider | user | organization administrator of Acceptance B only | step 4 |

## Operator card

Hand this over as is:

1. Start KyYard from the shipped Compose file, retrieve the one-time credential and replace it.
2. Create an environment and enroll and approve both hosts; inspect their identity and status.
3. Find a container, inspect its configuration and statistics, search, follow and download its
   logs, restart it, and open a terminal in it. With a cluster: do the same for a workload,
   scale it, change one environment value and delete one of its pods.
4. Show that a read-only account cannot change anything, open a terminal or see secrets, and
   that another organization cannot reach these resources through direct URLs or API requests.
5. Import the `acc-app` Compose project, preview a change, deploy it, read the deployment
   result, and put back the earlier definition without losing data.
6. Find an image update, confirm nothing changed on its own, then approve and apply it.
7. Disconnect and reconnect host A and revoke host B; explain what the screens show and why
   actions are refused.
8. Find the audit records for what you did, including failures.
9. Make a sealed backup and run the restore drill; then, with the recovery operator, restore
   onto a clean installation and check that it works.

## Steps

Each step: where it lives, what the operator does, what passes, what to record. Screen names
are as the UI shows them. Header navigation is Containers, Endpoints, Settings.

### 1. First start and credential

- `docker compose up -d`; the one-time password is in `docker compose logs kyyard` on the
  line `[SECURITY] Initial bootstrap: Created admin account. Username: admin | Password: …`.
- Sign in as `admin`. The next screen is "Change your password": Current password, New
  password, Confirm new password, Change password (the 12-character minimum is stated in the
  paragraph above the fields). Sign in again.
- Pass: after the second sign-in, Containers shows Local Docker; the old password is refused.
- Record: time taken, whether the operator found the log line unaided.

### 2. Environment and enrollment

- Endpoints, Environments & add host. Under Environments, New environment: type a name,
  Create. Open it; the Hosts tab holds the Endpoints panel.
- Enroll a host shows a command once. Run it on host A; it prints a fingerprint. Refresh
  hosts; the host appears `pending`. Approve opens a dialog with the fingerprint: it must match
  the host's. Repeat for host B.
- Open each host (its name links to the endpoint page), Details tab, "Host details &
  identity": Runtime, Host, Capacity, Key, Capabilities.
- Pass: both hosts `active`, each approved fingerprint equals what its host printed,
  Capabilities list `deployment.apply` and `deployment.pull`.
- Record: whether the operator compared fingerprints unprompted.

### 3. Containers, logs, terminal

- Endpoint page, Containers tab, or Containers in the header. Search box: "Search containers
  or images". The Usage column shows CPU, memory and restarts; Status, Uptime and IP columns
  are beside it.
- Open a container from the list; confirm uptime ticks, the IP matches `docker inspect`, and
  Logs and Terminal tabs work.
- Row icon Logs (a link to the Logs tab): Search log text, Load logs, Follow / Stop following,
  Download.
- Row icon Restart: confirm; the status line above the table reports `container.restart: succeeded`.
- Terminal (offered only to organization administrators, so admin sees it): Container
  user, Shell executable, Confirm container name (type it), then "Open terminal as …". Expect
  "Connected. Terminal contents are not recorded." Type `exit`.
- Pass: logs load, follow and download; the restart succeeds; the terminal opens and closes
  with "Process exited with code 0." Activity tab lists the restart.
- Record: anything the operator expected and did not find.

### 3b. Edit and run a container

- Run: on the endpoint page, "Run a container". Give it a name (`accept-run`), an image
  already on the host, command `sleep 3600`, and one environment variable `ACCEPT_KEY=one`.
  Type the name in "Type the new container name to confirm", then "Run container". Expect the
  step table and "Open the new container".
- Edit: on that container's Configuration tab (admin), change `ACCEPT_KEY` to `two`. The
  save area lists "Changes: env". Type the container name, "Save and recreate". Expect
  the step table and a container with a new ID; its Configuration tab shows `two`.
- Pass: both commands succeed, the old ID is gone from `docker ps -a`, and Activity lists
  `container.run` and `container.recreate`.
- Read-only: as reader, the same Configuration tab shows the redacted view (no environment
  values, no edit form), and the endpoint page has no "Run a container" button. Removing the
  container from step 3 cleans up.
- Record: anything in the save area or step table the operator could not interpret.

### 3c. Cluster workloads (with the optional cluster)

- Endpoint page of the cluster, Cluster tab. Workload names link to their page; `acc-web`
  opens on Overview (kind, desired/ready/updated, images, pods).
- Icon group: Restart (confirm), then Scale to `2` (prompt). The status line reads
  `acc/acc-web · Restart done.` and then `acc/acc-web · Scale done.`; the Overview lists two
  pods after the next refresh.
- Configuration tab (admin): `ACCEPT_KEY` shows masked; change it to `two`, type `acc-web` in
  "Type the workload name acc-web to confirm", "Save and apply". The Last change panel shows
  "Applied." and the steps precondition, apply and rollout.
- Run a workload (admin): the namespace toolbar's "Run a container". Pick `acc`, name `acc-run`,
  replicas `1`, one container `web` with a pinned image, type `acc-run`, and run. The Last change
  panel shows "Running." and the steps precondition, create and rollout; "Open the new workload" opens `acc-run`.
  Configuration tab: change the replicas to `2` and apply; then Delete it (type `acc-run`).
  Running the name of an existing Deployment stops with "A workload with this name already
  exists in the namespace. Choose another name."
- Terminal tab (admin): pick a pod and its container, shell `/bin/sh`, type the pod name in
  "Confirm pod name", open. Expect "Connected. Terminal contents are not recorded."; `echo
  $ACCEPT_KEY` prints `two`; `exit` ends with "Process exited with code 0."
- Overview, a pod's Delete icon: type the pod name. The Deployment replaces it.
- `acc-db`: Scale to `1`. The status line reads `acc/acc-db · Scale refused. Scaling down would
  delete this StatefulSet's volume claims (whenScaled: Delete).` and `kubectl get pvc -n
  acc` still lists both claims. Scale to `3` succeeds. Configuration tab: set Replicas to `1`,
  "Save and apply": the precondition step is refused with the same text and nothing changes.
  Delete (type `acc-db`): the status line reads `acc/acc-db · Delete refused. Deleting this
  StatefulSet would delete its volume claims (whenDeleted: Delete). Set whenDeleted: Retain
  first.` After `kubectl patch statefulset acc-db -n acc -p
  '{"spec":{"persistentVolumeClaimRetentionPolicy":{"whenDeleted":"Retain"}}}'`, Delete succeeds
  and `kubectl get pvc -n acc` still lists both claims.
- `acc-priv` (its workload page), Terminal tab: pick its pod and open a terminal. It ends with "This pod or its namespace does not
  meet Pod Security baseline, so terminals are refused." and never shows "Connected.".
- Pass: every command other than those refusals succeeds; the workload's Activity tab lists restart, scale and apply; the
  cluster's Activity tab also lists the pod delete. As reader, the workload page shows each
  pod's Logs link but no Restart, Scale, Delete or Terminal controls and no Configuration form.
- Record: whether the operator found the workload page and the typed-name confirmations
  unaided.

### 4. Read-only and cross-tenant

- As reader: Restart answers "You do not have permission for this action."; Logs answers "You
  do not have permission to read logs."; the row has no Terminal icon, because Terminal is
  offered only to organization administrators. Pass for Terminal: no Terminal button on any
  container. No exec request is made, so step 8 has no `container.exec` row for reader; that
  absence is expected, not a gap.
  Applications shows environment variable names only ("Encrypted environment keys");
  Registries shows "credential set", never the value.
- As outsider: open `/organizations/org_initial/endpoints/<host A id>` and
  `/api/organizations/org_initial/endpoints` directly. Neither returns data about
  `org_initial`, and no screen outsider can open names it or its hosts.
- Pass: every attempt refused, nothing of `org_initial` visible to outsider.
- Record: each URL tried and its answer. Any data shown is a cross-tenant exposure.

### 5. Import, deploy, put back

- Environment, Applications tab. "Import Compose draft": Application name, Compose YAML, Import
  draft ("Draft imported. No containers were changed.").
- View configuration for the application. Adopt: choose Docker host A and Compose project
  `acc-app`, review the container list, type the project name, Adopt reviewed containers.
- Map services to containers: pick one container per service, type the project name, Save
  service mapping. Compare with host is a read-only check.
- Preview a change: Save new revision, paste the whole definition with one environment value
  changed (every value must be supplied again), Save revision N. The mapping panel then asks
  for review: save the mapping again.
- Deployment preflight: the Mounts column shows `volume acc-app_data → <target>` for `data`
  and "No mounts." for `web`; a mount the running container has and the definition does not
  appears under "Will be dropped by the recreate:".
- Deployment plan: Revision to plan (latest), type the project name, Plan deployment. The plan
  table shows Service, Pinned image, Replaces container, Mounts, Secrets, and "Volumes to
  ensure: acc-app_data" above it. Show steps lists a `volume` step for `data` before its image
  step. Type the project name under
  "Confirm apply project", Apply deployment. The state polls to `succeeded`; Deployment
  history, Show steps lists each step.
- Put back: Deployment plan, Revision to plan = the earlier revision (the note says its own
  saved values are used and data written since is not reversed), plan, apply.
- Pass: both applies `succeeded`; current revision returns to the earlier one; the marker
  in `acc-app_data` is intact after each apply; `docker volume ls` on host A shows no new
  volume for the project.
- Record: every blocker message the operator saw and whether they understood it.

### 6. Update detection and approval

- Registry access first: Members page (Environments, Members), Registries panel. Either add a
  registry entry for the image's host or switch on "Allow anonymous pulls" (confirm; audited).
- Application, Updates, Check for updates. The table shows Service, Reference, Status ("Update
  available"), On host, In registry, Checked.
- Nothing changed: the endpoint's container still runs the old image ID, and Activity shows no
  new command.
- Type the project name under "Confirm update project", Plan update ("Plan created; review it
  in the deployment plan panel."). Deployment plan shows "pulls …" in Pinned image. Type the
  project name, Apply deployment.
- Pass: the apply `succeeded`, its steps include the pull; a new Check for updates says "Up to
  date"; the marker in `acc-app_data` is intact.
- Record: the digests before and after, and whether anything moved before the apply.

### 7. Disconnect, reconnect, revoke

- On host A: `docker stop kyyard-agent`. Within about 90 seconds the host shows `offline`;
  after three minutes its inventory reads "stale". Container actions, Logs and Terminal are
  disabled. `docker start kyyard-agent`; it returns to `active` with fresh inventory.
- Optional, for an `unknown`: click Restart on a host A container and stop the agent before
  the result shows. Activity then lists the command as `unknown`.
- Host B: environment, Hosts, Revoke, confirm ("Its identity stops working immediately and
  cannot be restored."). It shows `revoked`; no action works on it.
- Pass: the operator explains offline, stale, revoked and any unknown without help.
- Record: each state seen and the operator's explanation. An unknown they cannot explain is a
  release defect.

### 8. Audit

- Organization Environments page, Audit (or View audit history on the environment). Columns:
  Time, Actor, Action, Target, Result, Request.
- Expect rows such as `environment.create`, `endpoint.enroll`, `endpoint.revoke`,
  `container.operate` (restart, start, stop), `container.destroy` (remove), `container.logs`,
  `container.exec.open`, `application.deploy`, `registry.manage`, and reader's Restart and
  Logs refusals with result `denied`. There is no `container.restart` row: that name appears
  only in the container row's status. Enrollment, connection, key rotation and deployment results carry
  actor `agent:<endpoint id>`; reconciled commands carry `system`. Container restart, start, stop and remove results
  are not audited.
- Pass: every privileged action from steps 2 to 7 and every refusal from step 4 is there, and
  its Actor (a user ID) is the ID of the account that did it.
- Record: any action missing or misattributed.

### 9. Backup, drill, clean-install restore

- Settings, Recovery, Open backup & recovery. If no key is pinned, "Recovery key by hand":
  paste the scratch ceremony's public key, Needed / Of, Pin key.
- Back up now: a success message, Local copies counts one more. Run restore drill: "Restore
  drill" with a green "passed" badge and its checks, including `Schema Version:
  data/ky_server.db`.
- Clean-install restore: follow `docs/RESTORE.md` Steps 1 to 5 with the recovery operator and
  the custodians on a fresh machine, keeping `KY_APP_URL` so the agents find it. Stop the
  original first; two servers with one identity must never run together.
- Verify on the restored server: sign-in works; Backup & recovery shows the same recovery key
  ID; host A reconnects on its own and turns `active` (the instance key came back); host B is
  still `revoked`; the application's revisions and deployment history are there; Check for
  updates works (the registry credential decrypts); any command in flight at the capsule
  moment shows `unknown` with `the server restarted before a result arrived`.
- Pass: all of the above, with no secret lost and no step the custodians could not follow.
- Record: `capsule_id`, `created_at` and digest; the drill's checks; custodians used; time from
  start to a working server; each RESTORE.md step that needed explaining.

## Results

Copy this table into the run record.

| Run | |
|---|---|
| Date | |
| Commit and image digest | |
| Preparer | |
| Operator | |
| Recovery operator and custodians | |

| Step | Pass / fail | Time | Confusing moments | Documentation needed | Recovery outcome | Defects filed |
|---|---|---|---|---|---|---|
| 1 First start | | | | | – | |
| 2 Enrollment | | | | | – | |
| 3 Containers | | | | | – | |
| 3b Edit and run | | | | | – | |
| 4 Read-only and cross-tenant | | | | | – | |
| 5 Import and deploy | | | | | | |
| 6 Update | | | | | – | |
| 7 Disconnect and revoke | | | | | | |
| 8 Audit | | | | | – | |
| 9 Backup and restore | | | | | | |

0.1 is ready for internal use only when every row passes and no row names a release defect
that is still open.
