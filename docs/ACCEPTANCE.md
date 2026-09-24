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

- Registry access (needed for update checks) is set on the Members page, not near Updates.
- Live container configuration is shown only through Deployment preflight's "Inspect live
  container", so only for adopted, mapped containers.
- Platform audit (sign-in, password change, backup, `organization.create`, `user.create`) has
  no screen; only tenant audit does.
- The audit Actor column shows user IDs (`usr_…`), not names. Keep the list of which ID is
  which account from the prerequisites for step 8.
- Container command results (restart, start, stop, remove) are not audited as results; the
  audit row is the request, under its permission.

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
the host agent to this release, then check again." until it is upgraded.

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
  date" or "Unknown on host", the setup did not take; fix it and rerun step 6.
- Write a marker into the `acc-app_data` volume (a row, a file) and note it.

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
   logs, restart it, and open a terminal in it.
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
  or images". The Usage column shows CPU, memory and restarts. Configuration: see Known gaps;
  the operator may find Deployment preflight's Inspect live container after step 5.
- Actions, Logs: Search log text, Load logs, Follow / Stop following, Download.
- Actions, Restart: confirm; the row reports `container.restart: succeeded`.
- Actions, Terminal (offered only to organization administrators, so admin sees it): Container
  user, Shell executable, Confirm container name (type it), then "Open terminal as …". Expect
  "Connected. Terminal contents are not recorded." Type `exit`.
- Pass: logs load, follow and download; the restart succeeds; the terminal opens and closes
  with "Process exited with code 0." Activity tab lists the restart.
- Record: anything the operator expected and did not find.

### 4. Read-only and cross-tenant

- As reader: Restart answers "You do not have permission for this action."; Logs answers "You
  do not have permission to read logs."; Actions has no Terminal button, because Terminal is
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
  actor `agent:<endpoint id>`; reconciled commands carry `system`. Container command results
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
| 4 Read-only and cross-tenant | | | | | – | |
| 5 Import and deploy | | | | | | |
| 6 Update | | | | | – | |
| 7 Disconnect and revoke | | | | | | |
| 8 Audit | | | | | – | |
| 9 Backup and restore | | | | | | |

0.1 is ready for internal use only when every row passes and no row names a release defect
that is still open.
