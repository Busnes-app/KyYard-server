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
