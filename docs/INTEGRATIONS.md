# Integrations

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

### Pair kyPulse

An organization administrator can let kyPulse read the organization's health: endpoints,
inventory, samples, container logs and the audit feed. It can change nothing. On the
organization's **Members** page, under **Service tokens**, **Pair kyPulse** mints a six-digit
code shown once; enter it into kyPulse within 15 minutes to complete the pairing. Revoking a
token here stops it immediately, but that is only one side: unpair it in kyPulse as well, so
each product's own admin makes their own half of the change. `GET /healthz` reports the suite
health contract (`ky.health/1`) for external monitors, unrelated to a kyPulse pairing.

### Connect an AI agent

KyYard serves MCP (Streamable HTTP, stateless) at `https://<your host>/mcp` for AI agents such as
OpenClaw and Hermes Agent. An organization administrator creates an agent token on the
organization's **Members** page under **AI agents (MCP)**: a name and an access level, **Read
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

- `/mcp` accepts only `Authorization: Bearer kyy_mcp_…`. A request with an `Origin` header is
  refused (browsers never reach it), a session cookie or a kyPulse token is 401, and ten failed
  attempts from one address in 15 minutes answer 429. An agent token at `/api` is 401.
- Revoking a token or changing its access on the Members page takes effect on the next request. A
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
