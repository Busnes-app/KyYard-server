# Updates

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Upgrading

`docker-compose.yml` now reads an optional `.env.server` (see [Server settings](RUNNING.md#server-settings)),
which needs Docker Compose 2.24 or later. Nothing changes until you create that file. A setting
you added to `.env` hoping it reached the server still does nothing; move it to `.env.server`.
An `env_file` in your own override keeps working and still wins.

From this release the server runs as uid 10001, not root (see [Container user](CONTAINERS.md#container-user)).
The first start chowns the data volume or `./data` to 10001 by itself. Host jobs that read
`./data` or `KY_BACKUP_DIR` as root keep working; jobs running as another non-root user need
access as uid 10001. Run `apply-setup` and other in-container commands with `--user kyyard`.

This release deletes the retired SCIM group tables (`groups`, `group_members`) on first start.
Nothing has read them since SCIM was retired; back up the database first if you want to keep
that data.

Upgrade the server first, then every agent container (remote, or a legacy same-host one); the
built-in local connection upgrades with the server. The server must know about mounts before an
agent recreates containers that have them: an older server sends no mount list and cannot show
what a recreate drops, so an agent upgraded ahead of it refuses every container with a mount.
Until an agent is upgraded it reports no container mounts, and every deployment plan on its host
is blocked (`mounts_unreported`).

Deployment frames now carry a `request_id`. An agent upgraded ahead of its server refuses every
deployment (`invalid_request`) until the server is upgraded; an older agent still deploys, and
its results show "The agent did not classify this outcome; upgrade the agent."

Usernames are unique ignoring case from this release (migration 30). If two accounts differ only
by case the server refuses to start and names them, for example
`usernames differ only by case: "erin", "Erin"; rename or delete one of each pair before upgrading`;
nothing is changed. Stop the server (`docker compose stop kyyard`), copy the database, then
rename or delete one account of each pair in it and start again. For the default SQLite
volume (find its name with `docker volume ls | grep kyyard-data`):

    docker run --rm -it -v <volume>:/data alpine:3.24 sh -c 'apk add --no-cache sqlite >/dev/null && sqlite3 /data/ky_server.db'
    sqlite> SELECT id, username, sso_provider, created_at FROM users ORDER BY LOWER(username);
    sqlite> UPDATE users SET username = 'erin-old' WHERE id = '<id of the account to rename>';

On PostgreSQL run the same statements with `psql`. A single sign-on login whose username
matches an existing account ignoring case is refused with "The username … is taken by another
account": use the explicit linking flow from the existing local account when both identities
belong to you. A matching username or email alone never links accounts.
