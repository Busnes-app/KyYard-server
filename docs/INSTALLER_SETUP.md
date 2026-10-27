# Installer setup

The server exposes a container-local `apply-setup` interface. It requires the shared
`ky.product-setup/1` schema, service `kyyard`, a stable setup UUID, canonical HTTPS
KyIdentity issuer, client ID, protected absolute client/directory credential paths,
and explicit administrator and everyday issuer/subject assignments. Usernames match
`[a-z0-9._-]{3,64}`. The callback is `/api/sso/kyidentity/callback` under `KY_APP_URL`.
Register `/api/sso/kyidentity/backchannel-logout` under `KY_APP_URL` as the client's
back-channel logout URI in KyIdentity, so signing out there ends the KyYard session too.

Run `/app/kyyard-server apply-setup --capabilities`, then
`/app/kyyard-server apply-setup --file /data/installer/setup.json` inside the running
container as the server user (`docker exec --user kyyard`, uid 10001). Add `--check` to
inspect existing setup without changing it. Credential files and the bundle must be bounded
private regular files owned by that user; the image entrypoint chowns everything under
`/data` to it on each start. The CLI talks to `/data/.installer/setup.sock` and never opens the database.
Configuration activates immediately; `--restart` is unsupported.

Existing account names and provider configuration are never adopted. The native local
recovery account and its tenant grants stay intact. New platform administrators receive
no organization membership automatically. Assign tenant authority through an explicit,
audited organization grant; platform administration alone cannot access a tenant.
Ordinary settings and user APIs cannot change the installer-owned account authority.

Managed sign-in uses verified OIDC ID tokens, exact issuer/subject bindings and
`kyyard.admin` for administrator assignment. Unassigned identities cannot sign in.
There is no everyday role: everyday access comes from the everyday subject assignment, and
other role values are ignored. The `kyyard.admin` claim must match the assignment; an
everyday identity carrying it, or an administrator without it, has its access revoked at sign-in.
The installer provider refuses account linking and automatic provisioning. Signed
directory updates use `POST /api/installer/directory`, the shared `syncauth` headers,
an explicit `active` value, `roles` values and `meta.version` such as `W/"3"`.
Revocation retains accounts and tenant grants while atomically disabling access,
purging sessions/MFA/pairings, advancing the durable revision fence and auditing.

`GET /api/installer/network` is the public forwarded-header self-check. It uses
the same client-address and trusted-peer rules as native authentication. Pin
`KY_TRUSTED_PROXIES` to the actual edge source; the default trusts no proxy.

Managed SQLite backups include both protected setup credentials and their restore-path
mapping. Seal/open drills reject missing credentials or mappings inconsistent with the
retained setup record. Restore the extracted credentials to the original container paths
as described in `docs/RESTORE.md`; the extraction never writes arbitrary absolute paths.

Docker and Kubernetes runtime adapters refuse mutations and terminal exec against
`ky.managed-by=kyquickstart` workloads. Read-only inspection remains available.
On Kubernetes, pod exec and pod delete also follow the pod's UID-verified controller chain
(of any kind, up to three controllers) and refuse when any link carries the label or cannot be
verified. That reads ReplicaSets, Jobs and CronJobs, so the agent's per-namespace Role grants
`get` on them; reapply the regenerated manifest after upgrading. It grants no `get` on custom
resources, so pods an operator's custom resource controls stay refused.

This interface is under qualification. Remaining
gates include restored-process verification, explicit installer tenant
and endpoint composition, shared upgrade orchestration and
release provenance. Backup enrollment is refused by this first setup envelope; use
the native product recovery UI until the installer enrollment adapter is complete.

Installer-managed provider configuration is read-only in Settings → Sign-in, with a
public callback-copy control; credentials are never shown. The installer registry runs
`integrate-product --product kyyard` against an explicit target/container and protected
bundle. Its disposable `KYQ_TEST_YARD_BINARY` fixture proves actual SSH create/check/resume
and live activation without restart.

Focused store/API checks run against SQLite and PostgreSQL through the existing
`internal/testdb` fixture. `scripts/installer-oidc-test.py` exercises an actual server
binary and a disposable certificate-verified TLS identity provider. Set
`KY_INSTALLER_TEST_BINARY` to the absolute binary path and run it with Python 3;
OpenSSL is required for its temporary certificate and signed ID tokens. The fixture
disables the local Docker agent and never contacts existing suite services. CI's smoke
job runs it after `make build`; the same job gates main publication and release images.
