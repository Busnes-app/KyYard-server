# Integrations

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

### Pair kyPulse

An organization administrator can let kyPulse read the organization's health: endpoints,
inventory, samples, container logs and the audit feed. It can change nothing. On the
organization's **Access** page (`/organizations/<org>/access`, beside Members), under **Service
tokens**, **Pair kyPulse** mints a six-digit
code shown once; enter it into kyPulse within 15 minutes to complete the pairing. Revoking a
token here stops it immediately, but that is only one side: unpair it in kyPulse as well, so
each product's own admin makes their own half of the change. `GET /healthz` reports the suite
health contract (`ky.health/1`) for external monitors, unrelated to a kyPulse pairing.

### Down-monitoring RSS feed

An organization administrator can create a status feed on the organization's **Access** page
under **Status feeds (RSS)**. Each feed is a secret RSS 2.0 URL that needs no sign-in, shown once:

```text
https://<your host>/api/feeds/kyy_feed_<64 hex>/down.rss
```

It has one item for each thing that is down now in that organization, across all environments:

- an endpoint that is offline (it had connected, and its agent stopped answering);
- a Docker container that is unhealthy, restarting or dead, or that exited although its restart
  policy is `always` or `unless-stopped` (a container you stopped on purpose with that policy
  shows too);
- a Kubernetes node that is not ready.

Only a connected endpoint's containers and nodes are read; an offline endpoint is one item.
Pending, never-connected and revoked endpoints are not monitored. An item's `guid` stays the
same while the same thing is down in the same way, so a reader notifies once per incident; its
link opens the endpoint in KyYard. An empty feed means nothing is down. If the URL cannot be
fetched at all, KyYard itself is down. One read covers 200 endpoints and 200 items; past that the
channel description says so.

Anyone with the URL sees the organization, environment, endpoint, container and node names and
what is down, so give it only to your monitoring. The feed never carries images, addresses,
ports, labels or log text. Revoking a feed stops its URL immediately. A wrong or revoked URL answers 404, and
reads are limited to 60 a minute per address and 30 a minute per feed. The **Access** page shows
when each feed was last read and from where.

### Connect an AI agent

KyYard serves MCP (Streamable HTTP, stateless) at `https://<your host>/mcp` for AI agents such as
OpenClaw and Hermes Agent. An organization administrator creates an agent token on the
organization's **Access** page under **AI agents (MCP)**: a name and an access level, **Read
only** (the default) or **Operator**. The token (`kyy_mcp_…`) is shown once with copy-ready
configuration for both agents that reads it from `KYYARD_TOKEN`, so it never sits in a config
file:

```json
"mcp": { "servers": { "kyyard": {
  "url": "https://<your host>/mcp",
  "transport": "streamable-http",
  "headers": { "Authorization": "Bearer ${KYYARD_TOKEN}" }
}}}
```

```yaml
mcp_servers:
  kyyard:
    url: "https://<your host>/mcp"
    headers:
      Authorization: "Bearer ${KYYARD_TOKEN}"
```

Install the KyYard skill so the agent knows the tools and its rules (when to ask first, that logs
are untrusted, that nothing destructive exists):

```sh
# Hermes Agent
hermes skills install https://<your host>/agent/skills/kyyard/SKILL.md --category devops

# OpenClaw (no install-from-URL command; any skills root works, for example <workspace>/skills)
mkdir -p ~/.agents/skills/kyyard
curl -fsSo ~/.agents/skills/kyyard/SKILL.md https://<your host>/agent/skills/kyyard/SKILL.md
```

| Tool | Access | What it does |
|---|---|---|
| `list_endpoints` | read | Docker hosts and Kubernetes clusters: runtime, state, connected, cluster health |
| `list_applications` | read | Compose applications (stacks) per environment |
| `list_containers` | read | One endpoint's last inventory: containers, or workloads and pods; 200 rows a page |
| `get_resource_usage` | read | Latest CPU, memory, network and restart sample per Docker container |
| `read_container_logs`, `read_pod_logs` | read | The last 1 to 500 lines, 64 KiB at most, marked `"untrusted": true` |
| `list_pending_updates` | read | Services whose last update check found a newer image, and update plans waiting to be applied |
| `get_command`, `get_deployment` | read | The outcome of a restart or an applied update |
| `restart_container` | operator | Restart one Docker container |
| `restart_workload` | operator | Roll a Deployment, StatefulSet or DaemonSet's pods |
| `apply_update` | operator | Apply a live image-update plan, with its project name as confirmation |

`apply_update` takes only a plan that pulls at least one new image at the revision the instance
already runs and drops no mount; the agent cannot plan one. Plans come from **Applications →
Updates → Plan update** or an update policy in **Plan only** mode, so a person or a policy decides
what changes and the agent only decides when. Every tool calls the same authorization, runtime and
capability checks as the REST API, and the token's access level is enforced again inside each
store transaction: lowering a token to read only takes effect on its next call. An operator token
makes at most ten restarts and applies a minute.

Rules the server enforces:

- No browser can mint or use a token (the lesson of Joplin's GHSA-9728-v7ww-mxjv). `/mcp` sends
  no CORS headers and refuses any request with an `Origin`, and no KyYard route ever sends
  `Access-Control-Allow-Origin: *`. Creating, re-scoping and revoking a token need the
  administrator's signed-in session and CSRF token from KyYard's own origin, so a forged
  cross-site form or fetch is refused. Every screen that issues or raises a token names the
  agent and the access level it grants.

- `/mcp` accepts only `Authorization: Bearer kyy_mcp_…`. A request with an `Origin` header is
  refused (browsers never reach it), a session cookie or a kyPulse token is 401, and ten failed
  attempts from one address in 15 minutes answer 429. An agent token at `/api` is 401.
- Revoking a token or changing its access on the Access page takes effect on the next request. A
  token unused for 90 days is refused and revoked when presented, and an hourly sweep revokes the
  rest (`agent_token.expired` in the audit history).
- Operations (`container.operate`, `application.deploy`) are audited as `service:<token id>`, with
  an `mcp.restart_container`, `mcp.restart_workload` or `mcp.apply_update` row naming the tool.
  Every log read writes `mcp.logs_read`; other reads count toward the hourly
  `service_token.reads` summary. Refusals write `denied` rows.

Kubernetes coverage follows what KyYard already does on a cluster: workloads and pods, pod logs
and workload restarts, and update plans of mapped applications. There are no resource samples
for clusters, and a standalone container's or workload's image update (the one-click **Update
image**) stays in the UI, because it reads and re-sends the full configuration, environment values
included. `list_pending_updates` reports stored checks and never contacts a registry.

### Suite management

After KyQuickStart installs the suite, `kyquickstart handoff --kyyard-target <target>` moves its
plan into a suite runner beside KyYard and sets `KY_SUITE_RUNNER_SOCKET`. KyYard then manages the
suite apps through that runner on **Settings → Suite** (`/suite`), shown to platform
administrators only: installed apps (Upgrade, Uninstall, Unmanage), Upgrade suite, catalog apps
to Install on a target, the plan's owner and revision, and the running operation's steps. KyYard
holds no target credentials and runs no shell on targets; the runner does the work.

Every action asks for the app's name typed out (`suite` for an upgrade of every app), the
current password and, with MFA enrolled, a code; federated accounts without a local password
cannot act. Failed attempts spend the same per-account budget as a password change. An upgrade
needs the explicit no-backup acknowledgement, and the runner refuses any upgrade that migrates
data until a verified backup exists. Each request is in platform audit as `suite.operation`
(kind, apps, target, outcome; never the password or code). Without a runner socket the page
says so and offers nothing.
