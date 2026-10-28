# kyyard-server

Supported cryptographic configuration and migration: [Key strength and TLS](docs/STRONG-CRYPTO.md).

[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/15935/badge)](https://www.bestpractices.dev/projects/15935)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/Busnes-app/KyYard-server/badge)](https://scorecard.dev/viewer/?uri=github.com/Busnes-app/KyYard-server)

**Start here:** [Standalone installation](INSTALL.md). Local accounts and MFA work without KyIdentity. The default Docker installation connects the local Docker host and grants KyYard host-level authority through the mounted Docker socket. Remote hosts and Kubernetes clusters need separate enrollment; suite management needs a KyQuickStart runner handoff.

The simple control plane for your container fleet.

KyYard provides a Go backend, embedded React PWA,
SQLite/PostgreSQL, local authentication with self-service TOTP MFA and recovery codes, configurable OAuth 2/OIDC (including KyIdentity) and KyRecovery.
The standard installation automatically shows the local Docker host, with container inventory, lifecycle controls, image operations and logs. [Connect additional hosts](docs/CONTAINERS.md#show-containers-after-installation) when needed. Remaining milestones are tracked in [KyYard-Implementation-Plan.md](KyYard-Implementation-Plan.md).
SAML metadata is inherited; SAML login is not yet implemented.

Fresh installations only: KyYard uses its own recovery service identity and token-sealing
label. Existing base-project databases, pairing tokens and capsules are not a supported
in-place migration. The SQLite filename `ky_server.db` remains the inherited storage format.
Container data lives under `/data`, including local capsules under `/data/backups` in Compose.
KyYard generates persistent secrets without manual configuration. Compose runs one
container with SQLite and a named `kyyard-data` volume; SSO is optional and configured in Settings.

The default local URL is `http://localhost:9273`; follow the installation guide before exposing the service.

## What it can do

- Inspect and control containers, images, logs and resource use.
- Manage local Docker and enroll additional Docker hosts or Kubernetes endpoints.
- Use organizations, roles, local MFA or optional KyIdentity sign-in.
- Back up KyYard state separately from the applications it manages.
- Let an AI agent inspect the homelab, and optionally restart containers and apply updates, over MCP.

## Connect an AI agent

AI agents such as OpenClaw and Hermes Agent reach an organization over MCP at
`https://<your host>/mcp`. On the organization's **Access** page, under **AI agents (MCP)**,
an organization administrator names the agent and picks an access level:

- **Read only** (the default): the agent lists hosts, clusters, applications and containers,
  their state, health and resource use, recent logs (500 lines at most) and pending updates.
- **Operator**: it can also restart a container or Kubernetes workload and apply an image-update
  plan someone made in KyYard.

No agent can stop, delete, run commands in or reconfigure anything, touch volumes or read
secrets. The page shows the token once, with config for both agents that reads it from
`KYYARD_TOKEN`, and the command to install the KyYard skill (served at
`/agent/skills/kyyard/SKILL.md`), which teaches the agent the tools and when to ask you first.
You can change an agent's access there later. An agent token unused for 90 days is revoked; an
agent token never works as a kyPulse service token, nor the reverse. Every operation, log read and
refusal is in the audit history. Details: [Integrations](docs/INTEGRATIONS.md#connect-an-ai-agent).

Container logs are written by the software you run, and an agent may treat them as instructions.
Give an agent operator access only if you trust everyone who can message it and every workload
whose logs it reads.

## License

kyyard-server is released under the [MIT License](LICENSE.txt).

[![OctoCounts](https://api.octocounts.com/badge/Busnes-app/KyYard-server/branch/master)](https://octocounts.com/github/Busnes-app/KyYard-server/tree/master)

## Documentation

- [Running](docs/RUNNING.md) — Start KyYard, First sign-in, Account security, Transport and advanced deployment, and related procedures. <a id="start-kyyard"></a><a id="first-sign-in"></a><a id="account-security"></a><a id="transport-and-advanced-deployment"></a><a id="server-settings"></a>
- [Tenants](docs/TENANTS.md) — Initial organization, Organizations and accounts, Tenant API. <a id="initial-organization"></a><a id="organizations-and-accounts"></a><a id="tenant-api"></a>
- [Integrations](docs/INTEGRATIONS.md) — Pair kyPulse, Connect an AI agent, Suite management. <a id="pair-kypulse"></a><a id="suite-management"></a>
- [Containers](docs/CONTAINERS.md) — Show containers after installation, Container user, Static internal IPs for proxy backends, Compose projects, and related procedures. <a id="show-containers-after-installation"></a><a id="container-user"></a><a id="static-internal-ips-for-proxy-backends"></a><a id="compose-projects"></a><a id="container-terminals"></a><a id="edit-or-run-a-container"></a><a id="import-a-compose-draft"></a><a id="update-policies"></a><a id="fleet-image-runs-and-yaml"></a>
- [Agents and Kubernetes](docs/AGENTS-AND-KUBERNETES.md) — Agent, Kubernetes endpoints, Migrating a Docker application to a cluster. <a id="agent"></a><a id="kubernetes-endpoints"></a><a id="migrating-a-docker-application-to-a-cluster"></a>
- [Recovery](docs/RECOVERY.md) — Persistent keys, Disaster recovery, Two ways to get a key, Why TLS matters here, and related procedures. <a id="persistent-keys"></a><a id="disaster-recovery"></a><a id="two-ways-to-get-a-key"></a><a id="why-tls-matters-here"></a><a id="one-run-every-destination"></a><a id="environment"></a><a id="restoring"></a>
- [Updates](docs/UPDATES.md) — Upgrading. <a id="upgrading"></a>
- [Verifying releases](docs/VERIFYING-RELEASES.md) — Check a release image's signature and source commit, and rebuild its binaries to the same bytes.
- [Assurance case](docs/ASSURANCE-CASE.md) — Threat model summary, trust boundaries, and why the security requirements are met.

- [Contributing and verification](CONTRIBUTING.md#verification) — Local checks and desktop/phone browser regressions.
