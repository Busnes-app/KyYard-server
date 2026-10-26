# Security Policy

Supported cryptographic configuration and migration: [Key strength and TLS](docs/STRONG-CRYPTO.md).

KyYard is a self-hosted control plane for a container fleet. It connects to hosts and performs container operations on behalf of authenticated operators.

## Reporting vulnerabilities

Report security vulnerabilities privately through GitHub Security Advisories:
<https://github.com/Busnes-app/KyYard-server/security/advisories>. Do not open a public issue.
Include the affected area, reproduction steps, impact, and affected versions.
We aim to acknowledge reports within two business days and will coordinate a fix
and disclosure.

## Trust boundaries and limitations

- KyYard holds credentials for the hosts it controls; compromise of the control plane can reach every host it manages. Keep its data directory and backups private.
- Control-plane actions are powerful. Every action is authorized against the authenticated identity; there is no unauthenticated control path.
- KyIdentity owns people and groups; a deactivated identity must lose control access.

## Deployment requirements

- Run behind HTTPS; never add a default that downgrades to cleartext.
- Store host credentials encrypted at rest; keep the data directory and backups private.
- Configure rate limits and lockout on authentication and control endpoints.

## Security-sensitive changes

Changes to authentication, fleet-action authorization, host credential storage, remote execution, or logging require tests for both the expected path and the relevant
failure or attack path, plus a description of the trust boundary in the pull
request.

## Related documents

- [CONTRIBUTING.md](CONTRIBUTING.md) — contribution and review requirements
- [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) — community expectations
- [README.md](README.md) — what this project is and how to run it

