package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Migration represents an incremental schema change step.
type Migration struct {
	Version  int
	Name     string
	SQLite   string
	Postgres string
}

var registry = []Migration{
	{
		Version: 1,
		Name:    "initial_schema",
		SQLite: `
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    email TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL DEFAULT 'user',
    status TEXT NOT NULL DEFAULT 'active',
    sso_provider TEXT NOT NULL DEFAULT 'local',
    sso_subject TEXT NOT NULL DEFAULT '',
    totp_secret_enc TEXT NOT NULL DEFAULT '',
    totp_enabled INTEGER NOT NULL DEFAULT 0,
    recovery_codes_hash TEXT NOT NULL DEFAULT '[]',
    push_device_id TEXT NOT NULL DEFAULT '',
    must_change_password INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    last_login_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_users_username ON users(username);
CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
CREATE INDEX IF NOT EXISTS idx_users_sso ON users(sso_provider, sso_subject);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_agent TEXT NOT NULL DEFAULT '',
    ip_address TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    expires_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS device_pairings (
    secret TEXT PRIMARY KEY,
    code TEXT NOT NULL UNIQUE,
    user_id TEXT NOT NULL DEFAULT '',
    device_name TEXT NOT NULL DEFAULT '',
    platform TEXT NOT NULL DEFAULT '',
    push_token TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    created_at DATETIME NOT NULL,
    expires_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pairings_code ON device_pairings(code);
CREATE INDEX IF NOT EXISTS idx_pairings_expires ON device_pairings(expires_at);

CREATE TABLE IF NOT EXISTS groups (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL UNIQUE,
    external_id TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS group_members (
    group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

CREATE TABLE IF NOT EXISTS audit_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    resource TEXT NOT NULL DEFAULT '',
    details TEXT NOT NULL DEFAULT '',
    ip_address TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_user_created ON audit_records(user_id, created_at);

CREATE TABLE IF NOT EXISTS server_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL
);
`,
		Postgres: `
CREATE TABLE IF NOT EXISTS users (
    id VARCHAR(64) PRIMARY KEY,
    username VARCHAR(128) NOT NULL UNIQUE,
    email VARCHAR(255) NOT NULL DEFAULT '',
    display_name VARCHAR(255) NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    role VARCHAR(32) NOT NULL DEFAULT 'user',
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    sso_provider VARCHAR(32) NOT NULL DEFAULT 'local',
    sso_subject VARCHAR(255) NOT NULL DEFAULT '',
    totp_secret_enc TEXT NOT NULL DEFAULT '',
    totp_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    recovery_codes_hash TEXT NOT NULL DEFAULT '[]',
    push_device_id VARCHAR(255) NOT NULL DEFAULT '',
    must_change_password BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    last_login_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_users_username ON users(username);
CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
CREATE INDEX IF NOT EXISTS idx_users_sso ON users(sso_provider, sso_subject);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash VARCHAR(64) PRIMARY KEY,
    user_id VARCHAR(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_agent TEXT NOT NULL DEFAULT '',
    ip_address VARCHAR(64) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS device_pairings (
    secret VARCHAR(64) PRIMARY KEY,
    code VARCHAR(16) NOT NULL UNIQUE,
    user_id VARCHAR(64) NOT NULL DEFAULT '',
    device_name VARCHAR(255) NOT NULL DEFAULT '',
    platform VARCHAR(32) NOT NULL DEFAULT '',
    push_token TEXT NOT NULL DEFAULT '',
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pairings_code ON device_pairings(code);
CREATE INDEX IF NOT EXISTS idx_pairings_expires ON device_pairings(expires_at);

CREATE TABLE IF NOT EXISTS groups (
    id VARCHAR(64) PRIMARY KEY,
    display_name VARCHAR(255) NOT NULL UNIQUE,
    external_id VARCHAR(255) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS group_members (
    group_id VARCHAR(64) NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id VARCHAR(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

CREATE TABLE IF NOT EXISTS audit_records (
    id BIGSERIAL PRIMARY KEY,
    user_id VARCHAR(64) NOT NULL DEFAULT '',
    action VARCHAR(64) NOT NULL,
    resource VARCHAR(255) NOT NULL DEFAULT '',
    details TEXT NOT NULL DEFAULT '',
    ip_address VARCHAR(64) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_user_created ON audit_records(user_id, created_at);

CREATE TABLE IF NOT EXISTS server_settings (
    key VARCHAR(128) PRIMARY KEY,
    value TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL
);
`,
	},
	{
		Version: 2,
		Name:    "one_time_auth_challenges",
		SQLite: `
CREATE TABLE IF NOT EXISTS mfa_challenges (
    token_hash TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mfa_challenges_expires ON mfa_challenges(expires_at);
`,
		Postgres: `
CREATE TABLE IF NOT EXISTS mfa_challenges (
    token_hash VARCHAR(64) PRIMARY KEY,
    user_id VARCHAR(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mfa_challenges_expires ON mfa_challenges(expires_at);
`,
	},
	{
		Version:  3,
		Name:     "totp_last_counter",
		SQLite:   `ALTER TABLE users ADD COLUMN totp_last_counter INTEGER NOT NULL DEFAULT 0;`,
		Postgres: `ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_last_counter BIGINT NOT NULL DEFAULT 0;`,
	},
	{
		Version: 4,
		Name:    "mfa_credential_snapshot",
		SQLite: `DELETE FROM mfa_challenges;
ALTER TABLE mfa_challenges ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';`,
		Postgres: `DELETE FROM mfa_challenges;
ALTER TABLE mfa_challenges ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';`,
	},
	{Version: 5, Name: "tenant_schema", SQLite: `CREATE TABLE organizations (
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL CHECK (length(trim(name)) > 0),
 created_at DATETIME NOT NULL
);
CREATE TABLE organization_memberships (
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 role TEXT NOT NULL CHECK (role IN ('organization_admin','environment_admin','operator','developer','read_only')),
 status TEXT NOT NULL CHECK (status IN ('active','disabled')),
 PRIMARY KEY (organization_id, user_id)
);
CREATE INDEX idx_memberships_user ON organization_memberships(user_id);
CREATE TABLE environments (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 name TEXT NOT NULL CHECK (length(trim(name)) > 0),
 UNIQUE (organization_id, id),
 UNIQUE (organization_id, name)
);
CREATE TABLE organization_groups (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 name TEXT NOT NULL CHECK (length(trim(name)) > 0),
 UNIQUE (organization_id, id),
 UNIQUE (organization_id, name)
);
CREATE TABLE organization_group_members (
 organization_id TEXT NOT NULL,
 group_id TEXT NOT NULL,
 user_id TEXT NOT NULL,
 PRIMARY KEY (organization_id, group_id, user_id),
 FOREIGN KEY (organization_id, group_id) REFERENCES organization_groups(organization_id, id) ON DELETE CASCADE,
 FOREIGN KEY (organization_id, user_id) REFERENCES organization_memberships(organization_id, user_id) ON DELETE CASCADE
);
CREATE TABLE tenancy_bootstrap (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 completed_at DATETIME
);
INSERT INTO organizations (id, name, created_at) VALUES ('org_initial', 'Default organization', CURRENT_TIMESTAMP);
INSERT INTO tenancy_bootstrap (id) VALUES (1);
`, Postgres: `CREATE TABLE organizations (
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL CHECK (length(trim(name)) > 0),
 created_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE organization_memberships (
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 role TEXT NOT NULL CHECK (role IN ('organization_admin','environment_admin','operator','developer','read_only')),
 status TEXT NOT NULL CHECK (status IN ('active','disabled')),
 PRIMARY KEY (organization_id, user_id)
);
CREATE INDEX idx_memberships_user ON organization_memberships(user_id);
CREATE TABLE environments (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 name TEXT NOT NULL CHECK (length(trim(name)) > 0),
 UNIQUE (organization_id, id),
 UNIQUE (organization_id, name)
);
CREATE TABLE organization_groups (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 name TEXT NOT NULL CHECK (length(trim(name)) > 0),
 UNIQUE (organization_id, id),
 UNIQUE (organization_id, name)
);
CREATE TABLE organization_group_members (
 organization_id TEXT NOT NULL,
 group_id TEXT NOT NULL,
 user_id TEXT NOT NULL,
 PRIMARY KEY (organization_id, group_id, user_id),
 FOREIGN KEY (organization_id, group_id) REFERENCES organization_groups(organization_id, id) ON DELETE CASCADE,
 FOREIGN KEY (organization_id, user_id) REFERENCES organization_memberships(organization_id, user_id) ON DELETE CASCADE
);
CREATE TABLE tenancy_bootstrap (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 completed_at TIMESTAMPTZ
);
INSERT INTO organizations (id, name, created_at) VALUES ('org_initial', 'Default organization', CURRENT_TIMESTAMP);
INSERT INTO tenancy_bootstrap (id) VALUES (1);
`},
	{Version: 6, Name: "scoped_audit", SQLite: `ALTER TABLE audit_records ADD COLUMN scope TEXT NOT NULL DEFAULT 'platform' CHECK (scope IN ('platform','organization'));
ALTER TABLE audit_records ADD COLUMN organization_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_records ADD COLUMN environment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_records ADD COLUMN correlation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_records ADD COLUMN result TEXT NOT NULL DEFAULT 'unknown' CHECK (result IN ('unknown','success','denied','failure'));
CREATE INDEX idx_audit_scope_created ON audit_records(organization_id, created_at, id);
`, Postgres: `ALTER TABLE audit_records ADD COLUMN scope TEXT NOT NULL DEFAULT 'platform' CHECK (scope IN ('platform','organization'));
ALTER TABLE audit_records ADD COLUMN organization_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_records ADD COLUMN environment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_records ADD COLUMN correlation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_records ADD COLUMN result TEXT NOT NULL DEFAULT 'unknown' CHECK (result IN ('unknown','success','denied','failure'));
CREATE INDEX idx_audit_scope_created ON audit_records(organization_id, created_at, id);
`},
	{Version: 7, Name: "endpoints", SQLite: `CREATE TABLE endpoints (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 environment_id TEXT NOT NULL,
 name TEXT NOT NULL CHECK (length(trim(name)) > 0),
 runtime TEXT NOT NULL CHECK (runtime IN ('docker','kubernetes')),
 state TEXT NOT NULL CHECK (state IN ('pending','approved','active','offline','revoked')),
 facts TEXT NOT NULL DEFAULT '{}',
 created_at DATETIME NOT NULL,
 approved_at DATETIME,
 approved_by TEXT NOT NULL DEFAULT '',
 revoked_at DATETIME,
 last_seen_at DATETIME,
 UNIQUE (organization_id, id),
 UNIQUE (organization_id, name),
 FOREIGN KEY (organization_id, environment_id) REFERENCES environments(organization_id, id) ON DELETE RESTRICT
);
CREATE TABLE endpoint_keys (
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 fingerprint TEXT NOT NULL,
 public_key TEXT NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('approved','pending_review','retired')),
 created_at DATETIME NOT NULL,
 acknowledged_at DATETIME,
 retired_at DATETIME,
 PRIMARY KEY (endpoint_id, fingerprint)
);
CREATE TABLE agent_enrollment_tokens (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 runtime TEXT NOT NULL CHECK (runtime IN ('docker','kubernetes')),
 token_hash TEXT NOT NULL UNIQUE,
 created_by TEXT NOT NULL,
 created_at DATETIME NOT NULL,
 expires_at DATETIME NOT NULL,
 consumed_at DATETIME,
 endpoint_id TEXT NOT NULL DEFAULT '',
 agent_image TEXT NOT NULL DEFAULT '',
 FOREIGN KEY (organization_id, environment_id) REFERENCES environments(organization_id, id) ON DELETE CASCADE
);
`, Postgres: `CREATE TABLE endpoints (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 environment_id TEXT NOT NULL,
 name TEXT NOT NULL CHECK (length(trim(name)) > 0),
 runtime TEXT NOT NULL CHECK (runtime IN ('docker','kubernetes')),
 state TEXT NOT NULL CHECK (state IN ('pending','approved','active','offline','revoked')),
 facts TEXT NOT NULL DEFAULT '{}',
 created_at TIMESTAMPTZ NOT NULL,
 approved_at TIMESTAMPTZ,
 approved_by TEXT NOT NULL DEFAULT '',
 revoked_at TIMESTAMPTZ,
 last_seen_at TIMESTAMPTZ,
 UNIQUE (organization_id, id),
 UNIQUE (organization_id, name),
 FOREIGN KEY (organization_id, environment_id) REFERENCES environments(organization_id, id) ON DELETE RESTRICT
);
CREATE TABLE endpoint_keys (
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 fingerprint TEXT NOT NULL,
 public_key TEXT NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('approved','pending_review','retired')),
 created_at TIMESTAMPTZ NOT NULL,
 acknowledged_at TIMESTAMPTZ,
 retired_at TIMESTAMPTZ,
 PRIMARY KEY (endpoint_id, fingerprint)
);
CREATE TABLE agent_enrollment_tokens (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 runtime TEXT NOT NULL CHECK (runtime IN ('docker','kubernetes')),
 token_hash TEXT NOT NULL UNIQUE,
 created_by TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 consumed_at TIMESTAMPTZ,
 endpoint_id TEXT NOT NULL DEFAULT '',
 agent_image TEXT NOT NULL DEFAULT '',
 FOREIGN KEY (organization_id, environment_id) REFERENCES environments(organization_id, id) ON DELETE CASCADE
);
`},
	{Version: 8, Name: "endpoint_inventory", SQLite: `ALTER TABLE endpoints ADD COLUMN inventory_generation INTEGER NOT NULL DEFAULT 0;
`, Postgres: `ALTER TABLE endpoints ADD COLUMN inventory_generation BIGINT NOT NULL DEFAULT 0;
`},
	{Version: 9, Name: "endpoint_events", SQLite: `CREATE TABLE endpoint_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 severity TEXT NOT NULL CHECK (severity IN ('info','high')),
 kind TEXT NOT NULL,
 details TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL,
 acknowledged_at DATETIME
);
CREATE INDEX idx_endpoint_events_endpoint ON endpoint_events(endpoint_id, created_at);
CREATE TABLE endpoint_capabilities (
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 capability TEXT NOT NULL,
 PRIMARY KEY (endpoint_id, capability)
);
`, Postgres: `CREATE TABLE endpoint_events (
 id BIGSERIAL PRIMARY KEY,
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 severity TEXT NOT NULL CHECK (severity IN ('info','high')),
 kind TEXT NOT NULL,
 details TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL,
 acknowledged_at TIMESTAMPTZ
);
CREATE INDEX idx_endpoint_events_endpoint ON endpoint_events(endpoint_id, created_at);
CREATE TABLE endpoint_capabilities (
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 capability TEXT NOT NULL,
 PRIMARY KEY (endpoint_id, capability)
);
`},
	{Version: 10, Name: "endpoint_inventory", SQLite: `CREATE TABLE endpoint_inventory (
 endpoint_id TEXT PRIMARY KEY REFERENCES endpoints(id) ON DELETE CASCADE,
 generation INTEGER NOT NULL,
 observed_at DATETIME NOT NULL,
 received_at DATETIME NOT NULL,
 snapshot TEXT NOT NULL
);
`, Postgres: `CREATE TABLE endpoint_inventory (
 endpoint_id TEXT PRIMARY KEY REFERENCES endpoints(id) ON DELETE CASCADE,
 generation BIGINT NOT NULL,
 observed_at TIMESTAMPTZ NOT NULL,
 received_at TIMESTAMPTZ NOT NULL,
 snapshot TEXT NOT NULL
);
`},
	{Version: 11, Name: "endpoint_events_open_unique", SQLite: `CREATE UNIQUE INDEX idx_endpoint_events_open ON endpoint_events(endpoint_id, kind) WHERE acknowledged_at IS NULL;`, Postgres: `CREATE UNIQUE INDEX idx_endpoint_events_open ON endpoint_events(endpoint_id, kind) WHERE acknowledged_at IS NULL;`},
	{Version: 12, Name: "container_samples", SQLite: `CREATE TABLE container_samples (
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 container_id TEXT NOT NULL,
 observed_at DATETIME NOT NULL,
 cpu_percent REAL NOT NULL,
 memory_bytes INTEGER NOT NULL,
 memory_limit INTEGER NOT NULL,
 rx_bytes INTEGER NOT NULL,
 tx_bytes INTEGER NOT NULL,
 pids INTEGER NOT NULL,
 PRIMARY KEY (endpoint_id, container_id, observed_at)
);
CREATE INDEX idx_container_samples_observed ON container_samples(observed_at);
`, Postgres: `CREATE TABLE container_samples (
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 container_id TEXT NOT NULL,
 observed_at TIMESTAMPTZ NOT NULL,
 cpu_percent DOUBLE PRECISION NOT NULL,
 memory_bytes BIGINT NOT NULL,
 memory_limit BIGINT NOT NULL,
 rx_bytes BIGINT NOT NULL,
 tx_bytes BIGINT NOT NULL,
 pids BIGINT NOT NULL,
 PRIMARY KEY (endpoint_id, container_id, observed_at)
);
CREATE INDEX idx_container_samples_observed ON container_samples(observed_at);
`},
	{Version: 13, Name: "container_samples_endpoint_time", SQLite: `CREATE INDEX idx_container_samples_endpoint_time ON container_samples(endpoint_id, observed_at, container_id);`, Postgres: `CREATE INDEX idx_container_samples_endpoint_time ON container_samples(endpoint_id, observed_at, container_id);`},
	// -1 is "the runtime did not say", which existing rows cannot distinguish from zero either.
	{Version: 14, Name: "container_samples_restarts", SQLite: `ALTER TABLE container_samples ADD COLUMN restart_count INTEGER NOT NULL DEFAULT -1;`, Postgres: `ALTER TABLE container_samples ADD COLUMN restart_count BIGINT NOT NULL DEFAULT -1;`},
}

// Run executes all pending migrations for the specified database driver.
func Run(ctx context.Context, db *sql.DB, driver string) error {
	driver = strings.ToLower(driver)
	if driver == "postgresql" {
		driver = "postgres"
	}

	// Create schema_migrations table
	initTableQuery := `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at DATETIME NOT NULL
);`
	if driver == "postgres" {
		initTableQuery = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL
);`
	}

	if _, err := db.ExecContext(ctx, initTableQuery); err != nil {
		return fmt.Errorf("failed to init schema_migrations: %w", err)
	}

	for _, m := range registry {
		var exists int
		err := db.QueryRowContext(ctx, "SELECT COUNT(1) FROM schema_migrations WHERE version = $1", m.Version).Scan(&exists)
		if err != nil {
			// Try SQLite positional ? parameter if $1 failed
			err = db.QueryRowContext(ctx, "SELECT COUNT(1) FROM schema_migrations WHERE version = ?", m.Version).Scan(&exists)
			if err != nil {
				return fmt.Errorf("failed to check migration version %d: %w", m.Version, err)
			}
		}

		if exists > 0 {
			continue
		}

		ddl := m.SQLite
		if driver == "postgres" {
			ddl = m.Postgres
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin migration tx for v%d: %w", m.Version, err)
		}

		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed executing migration v%d (%s): %w", m.Version, m.Name, err)
		}

		recordQuery := "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)"
		if driver == "postgres" {
			recordQuery = "INSERT INTO schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)"
		}

		if _, err := tx.ExecContext(ctx, recordQuery, m.Version, m.Name, time.Now().UTC()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed to record migration v%d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration v%d: %w", m.Version, err)
		}
	}

	return nil
}
