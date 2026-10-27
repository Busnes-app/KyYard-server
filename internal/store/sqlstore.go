package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/store/migrations"
)

// SQLStore implements Store on top of database/sql.
type SQLStore struct {
	db       *sql.DB
	driver   string
	budget   int64        // bytes of telemetry allowed; zero disables the check
	lastUsed atomic.Int64 // bytes at the previous evaluation, to tell growth from a plateau
	ceiling  int          // stored sample rows allowed per endpoint
	pressure atomic.Int32 // current Pressure, read on every telemetry write
	users    *userStore
	sessions *sessionStore
	devices  *deviceStore
	audit    *auditStore
	settings *settingsStore

	allowPrivileged atomic.Bool // KY_CONTAINER_ALLOW_PRIVILEGED for application deploys
}

// newSQLStore creates and initializes a SQLStore, running migrations automatically.
func newSQLStore(ctx context.Context, db *sql.DB, driver string, lim limits) (*SQLStore, error) {
	driver = strings.ToLower(driver)
	if driver == "postgresql" {
		driver = "postgres"
	}

	if err := migrations.Run(ctx, db, driver); err != nil {
		return nil, fmt.Errorf("migration failure on driver %s: %w", driver, err)
	}

	if lim.ceiling <= 0 {
		lim.ceiling = MaxSampleRowsPerEndpoint
	}
	s := &SQLStore{
		db:      db,
		driver:  driver,
		budget:  lim.budget,
		ceiling: lim.ceiling,
	}

	s.users = &userStore{store: s}
	s.sessions = &sessionStore{store: s}
	s.devices = &deviceStore{store: s}
	s.audit = &auditStore{store: s}
	s.settings = &settingsStore{store: s}

	return s, nil
}

func (s *SQLStore) Tenancy() TenancyStore { return &tenancyStore{store: s} }

func (s *SQLStore) Users() UserStore        { return s.users }
func (s *SQLStore) Sessions() SessionStore  { return s.sessions }
func (s *SQLStore) Devices() DeviceStore    { return s.devices }
func (s *SQLStore) Audit() AuditStore       { return s.audit }
func (s *SQLStore) Settings() SettingsStore { return s.settings }

func (s *SQLStore) Driver() string                 { return s.driver }
func (s *SQLStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *SQLStore) Close() error                   { return s.db.Close() }

// rebind converts '?' placeholders to '$1, $2, ...' for Postgres
func (s *SQLStore) rebind(query string) string {
	if s.driver != "postgres" {
		return query
	}

	var b strings.Builder
	b.Grow(len(query) + 16)
	paramIdx := 1

	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(paramIdx))
			paramIdx++
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------
// User Store
// ---------------------------------------------------------------------

type userStore struct {
	store *SQLStore
}

func (u *userStore) CreateUser(ctx context.Context, user *User) error {
	tx, err := u.store.beginTx(ctx, false)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := u.createUser(ctx, tx, user); err != nil {
		return err
	}
	return tx.Commit()
}

func (u *userStore) createUser(ctx context.Context, tx *sql.Tx, user *User) error {
	if user.SSOProvider == InstallerProvider {
		return ErrInstallerOwned
	}
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	if user.UpdatedAt.IsZero() {
		user.UpdatedAt = now
	}
	if user.RecoveryCodesHash == "" {
		user.RecoveryCodesHash = "[]"
	}

	q := u.store.rebind(`
INSERT INTO users (
    id, username, email, display_name, password_hash, role, status,
    sso_provider, sso_subject, totp_secret_enc, totp_enabled,
    recovery_codes_hash, push_device_id, must_change_password,
    created_at, updated_at, last_login_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`)

	var lastLogin sql.NullTime
	if user.LastLoginAt != nil {
		lastLogin = sql.NullTime{Time: *user.LastLoginAt, Valid: true}
	}

	_, err := tx.ExecContext(ctx, q,
		user.ID, user.Username, user.Email, user.DisplayName, user.PasswordHash,
		user.Role, user.Status, user.SSOProvider, user.SSOSubject,
		user.TOTPSecretEnc, user.TOTPEnabled, user.RecoveryCodesHash,
		user.PushDeviceID, user.MustChangePassword,
		user.CreatedAt, user.UpdatedAt, lastLogin,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return ErrAlreadyExists
		}
		return err
	}
	return u.registerSSOIdentity(ctx, tx, user)
}

func (u *userStore) scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var user User
	var lastLogin sql.NullTime

	err := row.Scan(
		&user.ID, &user.Username, &user.Email, &user.DisplayName, &user.PasswordHash,
		&user.Role, &user.Status, &user.SSOProvider, &user.SSOSubject,
		&user.TOTPSecretEnc, &user.TOTPEnabled, &user.RecoveryCodesHash,
		&user.PushDeviceID, &user.MustChangePassword, &user.TOTPLastCounter,
		&user.CreatedAt, &user.UpdatedAt, &lastLogin,
	)
	if err != nil {
		if errorsIs(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if lastLogin.Valid {
		user.LastLoginAt = &lastLogin.Time
	}
	return &user, nil
}

func (u *userStore) GetUserByID(ctx context.Context, id string) (*User, error) {
	q := u.store.rebind(`
SELECT id, username, email, display_name, password_hash, role, status,
       sso_provider, sso_subject, totp_secret_enc, totp_enabled,
       recovery_codes_hash, push_device_id, must_change_password,
       totp_last_counter, created_at, updated_at, last_login_at
FROM users WHERE id = ?
`)
	return u.scanUser(u.store.db.QueryRowContext(ctx, q, id))
}

func (u *userStore) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	return u.getUserByUsername(ctx, u.store.db, username)
}

func (u *userStore) getUserByUsername(ctx context.Context, db dbtx, username string) (*User, error) {
	q := u.store.rebind(`
SELECT id, username, email, display_name, password_hash, role, status,
       sso_provider, sso_subject, totp_secret_enc, totp_enabled,
       recovery_codes_hash, push_device_id, must_change_password,
       totp_last_counter, created_at, updated_at, last_login_at
FROM users WHERE LOWER(username) = LOWER(?)
`)
	return u.scanUser(db.QueryRowContext(ctx, q, username))
}

func (u *userStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	q := u.store.rebind(`
SELECT id, username, email, display_name, password_hash, role, status,
       sso_provider, sso_subject, totp_secret_enc, totp_enabled,
       recovery_codes_hash, push_device_id, must_change_password,
       totp_last_counter, created_at, updated_at, last_login_at
FROM users WHERE LOWER(email) = LOWER(?)
`)
	return u.scanUser(u.store.db.QueryRowContext(ctx, q, email))
}

func (u *userStore) GetUserBySSO(ctx context.Context, provider, subject string) (*User, error) {
	return u.getUserBySSO(ctx, u.store.db, provider, subject)
}

func (u *userStore) getUserBySSO(ctx context.Context, db dbtx, provider, subject string) (*User, error) {
	q := u.store.rebind(`
SELECT id, username, email, display_name, password_hash, role, status,
       sso_provider, sso_subject, totp_secret_enc, totp_enabled,
       recovery_codes_hash, push_device_id, must_change_password,
       totp_last_counter, created_at, updated_at, last_login_at
FROM users WHERE id = (SELECT user_id FROM user_sso_identities WHERE provider = ? AND subject = ?)
`)
	return u.scanUser(db.QueryRowContext(ctx, q, provider, subject))
}

func (u *userStore) UpdateUser(ctx context.Context, user *User) error {
	user.UpdatedAt = time.Now().UTC()
	var lastLogin sql.NullTime
	if user.LastLoginAt != nil {
		lastLogin = sql.NullTime{Time: *user.LastLoginAt, Valid: true}
	}

	q := u.store.rebind(`
UPDATE users SET
    username = ?, email = ?, display_name = ?, password_hash = ?,
    role = ?, status = ?, sso_provider = ?, sso_subject = ?,
    totp_secret_enc = ?, totp_enabled = ?, recovery_codes_hash = ?,
    push_device_id = ?, must_change_password = ?, updated_at = ?,
    last_login_at = ?
WHERE id = ? AND (
    NOT EXISTS (SELECT 1 FROM server_settings WHERE key = ?)
    OR (username = ? AND password_hash = ? AND role = ? AND status = ? AND must_change_password = ?)
)
`)

	tx, err := u.store.beginTx(ctx, false)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldProvider, oldSubject string
	if err := tx.QueryRowContext(ctx, u.store.rebind("SELECT sso_provider, sso_subject FROM users WHERE id = ?"), user.ID).Scan(&oldProvider, &oldSubject); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if oldProvider != user.SSOProvider || oldSubject != user.SSOSubject {
		return ErrInvalid
	}
	res, err := tx.ExecContext(ctx, q,
		user.Username, user.Email, user.DisplayName, user.PasswordHash,
		user.Role, user.Status, user.SSOProvider, user.SSOSubject,
		user.TOTPSecretEnc, user.TOTPEnabled, user.RecoveryCodesHash,
		user.PushDeviceID, user.MustChangePassword, user.UpdatedAt,
		lastLogin, user.ID, "installer.user."+user.ID,
		user.Username, user.PasswordHash, user.Role, user.Status, user.MustChangePassword,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return ErrAlreadyExists
		}
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		var owned int
		if err := tx.QueryRowContext(ctx, u.store.rebind("SELECT COUNT(*) FROM server_settings WHERE key=?"), "installer.user."+user.ID).Scan(&owned); err != nil {
			return err
		}
		if owned != 0 {
			return ErrInstallerOwned
		}
		return ErrNotFound
	}
	if err := u.registerSSOIdentity(ctx, tx, user); err != nil {
		return err
	}
	return tx.Commit()
}

func (u *userStore) UpdateRecoveryCodes(ctx context.Context, userID, oldHashes, newHashes string) error {
	q := u.store.rebind("UPDATE users SET recovery_codes_hash = ?, updated_at = ? WHERE id = ? AND recovery_codes_hash = ?")
	res, err := u.store.db.ExecContext(ctx, q, newHashes, time.Now().UTC(), userID, oldHashes)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrAlreadyExists
	}
	return nil
}

func (u *userStore) SpendTOTPCounter(ctx context.Context, userID string, counter int64) error {
	q := u.store.rebind("UPDATE users SET totp_last_counter = ?, updated_at = ? WHERE id = ? AND totp_last_counter < ?")
	res, err := u.store.db.ExecContext(ctx, q, counter, time.Now().UTC(), userID, counter)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrAlreadyExists
	}
	return nil
}

func (u *userStore) DeleteUser(ctx context.Context, id string) error {
	tx, err := u.store.beginTx(ctx, false)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := u.adminQuorum(ctx, tx, id, false, u.deleteRow(ctx, id)); err != nil {
		return err
	}
	return tx.Commit()
}

func (u *userStore) deleteRow(ctx context.Context, id string) func(*sql.Tx) (sql.Result, error) {
	return func(tx *sql.Tx) (sql.Result, error) {
		return tx.ExecContext(ctx, u.store.rebind("DELETE FROM users WHERE id = ? AND NOT EXISTS (SELECT 1 FROM server_settings WHERE key = ?)"), id, "installer.user."+id)
	}
}

// DirectoryEventWindow is how far a directory event's timestamp may be from now.
const DirectoryEventWindow = 5 * time.Minute

func (u *userStore) ApplyDirectoryEvent(ctx context.Context, ev DirectoryEvent, apply func(DirectoryTx) error) error {
	tx, err := u.store.beginTx(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// An event older than the window is refused by its timestamp, so its ID need not be kept.
	if _, err := tx.ExecContext(ctx, u.store.rebind("DELETE FROM directory_events WHERE event_at < ?"), time.Now().Add(-DirectoryEventWindow).Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, u.store.rebind("INSERT INTO directory_events (provider, event_id, event_at) VALUES (?, ?, ?)"), ev.Provider, ev.ID, ev.At.Unix()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return ErrStaleEvent
		}
		return err
	}
	// Events within one second keep arrival order; the ID check above refuses an exact replay.
	res, err := tx.ExecContext(ctx, u.store.rebind(`INSERT INTO directory_subjects (provider, subject, event_at) VALUES (?, ?, ?)
ON CONFLICT (provider, subject) DO UPDATE SET event_at = excluded.event_at WHERE directory_subjects.event_at <= excluded.event_at`), ev.Provider, ev.Subject, ev.At.Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrStaleEvent
	}
	if err := apply(directoryTx{u, ctx, tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// directoryTx binds the identity provider's reads and writes to one event's transaction.
type directoryTx struct {
	u   *userStore
	ctx context.Context
	tx  *sql.Tx
}

func (d directoryTx) GetUserBySSO(provider, subject string) (*User, error) {
	return d.u.getUserBySSO(d.ctx, d.tx, provider, subject)
}

func (d directoryTx) GetUserByUsername(username string) (*User, error) {
	return d.u.getUserByUsername(d.ctx, d.tx, username)
}

func (d directoryTx) CreateUser(user *User) error { return d.u.createUser(d.ctx, d.tx, user) }

func (d directoryTx) UpdateUserIdentity(user *User) error {
	user.UpdatedAt = time.Now().UTC()
	return d.u.adminQuorum(d.ctx, d.tx, user.ID, true, func(tx *sql.Tx) (sql.Result, error) {
		res, err := tx.ExecContext(d.ctx, d.u.store.rebind(`
UPDATE users SET username = ?, email = ?, display_name = ?, role = ?, status = ?, updated_at = ?
WHERE id = ? AND (
    NOT EXISTS (SELECT 1 FROM server_settings WHERE key = ?)
    OR (username = ? AND role = ? AND status = ?)
)`), user.Username, user.Email, user.DisplayName, user.Role, user.Status, user.UpdatedAt,
			user.ID, "installer.user."+user.ID, user.Username, user.Role, user.Status)
		if err != nil && (strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key")) {
			return nil, ErrAlreadyExists
		}
		return res, err
	})
}

func (d directoryTx) DeleteUser(id string) error {
	return d.u.adminQuorum(d.ctx, d.tx, id, true, d.u.deleteRow(d.ctx, id))
}

func (d directoryTx) DeleteUserSessions(userID string) error {
	_, err := d.tx.ExecContext(d.ctx, d.u.store.rebind("DELETE FROM sessions WHERE user_id = ?"), userID)
	return err
}

// adminQuorum runs write on one user row, then checks each organization the active user
// administered still has an active administrator. A local change is refused with ErrLastAdmin;
// an external (identity provider) one always applies, because a removed person must lose access,
// and records organization.last_admin_lost there instead. It locks the user row, then those
// organizations in ID order, as membership writes do. A write touching no row is
// ErrInstallerOwned or ErrNotFound. An external write's tx must hold the audit lock.
func (u *userStore) adminQuorum(ctx context.Context, tx *sql.Tx, userID string, external bool, write func(*sql.Tx) (sql.Result, error)) error {
	if _, err := tx.ExecContext(ctx, u.store.rebind("UPDATE users SET id = id WHERE id = ?"), userID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, u.store.rebind(`SELECT m.organization_id FROM organization_memberships m JOIN users u ON u.id=m.user_id WHERE m.user_id=? AND m.role='organization_admin' AND m.status='active' AND u.status='active' ORDER BY m.organization_id`), userID)
	if err != nil {
		return err
	}
	var orgs []string
	for rows.Next() {
		var org string
		if err := rows.Scan(&org); err != nil {
			rows.Close()
			return err
		}
		orgs = append(orgs, org)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tenancy := &tenancyStore{store: u.store}
	for _, org := range orgs {
		if err := tenancy.lockOrganization(ctx, tx, org); err != nil {
			return err
		}
	}
	res, err := write(tx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var owned int
		if err := tx.QueryRowContext(ctx, u.store.rebind("SELECT COUNT(*) FROM server_settings WHERE key=?"), "installer.user."+userID).Scan(&owned); err != nil {
			return err
		}
		if owned != 0 {
			return ErrInstallerOwned
		}
		return ErrNotFound
	}
	for _, org := range orgs {
		err := tenancy.requireActiveAdmin(ctx, tx, org)
		if errorsIs(err, ErrLastAdmin) && external {
			_, err = tx.ExecContext(ctx, u.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), "directory", "organization.last_admin_lost", userID, "repair: a platform administrator must grant a new organization administrator", time.Now().UTC(), "organization", org, "", "", "success")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (u *userStore) ListUsers(ctx context.Context, offset, limit int) ([]*User, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var total int
	if err := u.store.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM users").Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := u.store.db.QueryContext(ctx, u.store.rebind(`
SELECT id, username, email, display_name, password_hash, role, status,
       sso_provider, sso_subject, totp_secret_enc, totp_enabled,
       recovery_codes_hash, push_device_id, must_change_password,
       totp_last_counter, created_at, updated_at, last_login_at
FROM users
ORDER BY created_at DESC LIMIT ? OFFSET ?`), limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		user, err := u.scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		users = append(users, user)
	}

	return users, total, rows.Err()
}

func (u *userStore) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := u.store.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM users").Scan(&count)
	return count, err
}

// ---------------------------------------------------------------------
// beginTx takes the audit lock before any domain lock in a transaction that may
// append audit records. PostgreSQL sequences alone do not order commits: a cursor
// could otherwise advance past an allocated but uncommitted ID. SQLite retains
// its existing single-writer discipline.
// ponytail: one writer per audit table; replace the ID cursor protocol if
// audit throughput requires concurrent writers with independent commit order.
func (s *SQLStore) beginTx(ctx context.Context, audited bool) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if audited && s.driver == "postgres" {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7345103, 'audit_records'::regclass::oid::int)`); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	return tx, nil
}

// Session Store
// ---------------------------------------------------------------------

type sessionStore struct {
	store *SQLStore
}

// withPassword serializes credential-derived grants with password replacement.
// Updating the same user row takes a write lock on both supported databases.
func (s *SQLStore) withPassword(ctx context.Context, userID, expectedHash string, audited bool, apply func(*sql.Tx) error) error {
	tx, err := s.beginTx(ctx, audited)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, s.rebind("UPDATE users SET id = id WHERE id = ? AND password_hash = ? AND status = 'active'"), userID, expectedHash)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if err := apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sessionStore) CreateSession(ctx context.Context, sess *Session, expectedPasswordHash string) error {
	err := s.store.withPassword(ctx, sess.UserID, expectedPasswordHash, false, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.store.rebind(`INSERT INTO sessions (token_hash, user_id, user_agent, ip_address, created_at, expires_at, sso_provider, sso_sid) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), sess.TokenHash, sess.UserID, sess.UserAgent, sess.IPAddress, sess.CreatedAt, sess.ExpiresAt, sess.SSOProvider, sess.SSOSID)
		return err
	})
	if err != nil || sess.SSOProvider == "" {
		return err
	}
	// Check the fences only after the session committed: EndSSOSession and EndSSOSubject
	// commit their fence before deleting, so under read committed one of the two always
	// sees the other.
	now := time.Now().UTC()
	var fenced int
	if err := s.store.db.QueryRowContext(ctx, s.store.rebind(`SELECT
 (SELECT COUNT(1) FROM sso_logouts WHERE provider = ? AND sid = ? AND sid <> '' AND expires_at > ?) +
 (SELECT COUNT(1) FROM sso_subject_logouts f JOIN user_sso_identities i ON i.provider = f.provider AND i.subject = f.subject
  WHERE f.provider = ? AND i.user_id = ? AND f.cutoff > ? AND f.expires_at > ?)`), sess.SSOProvider, sess.SSOSID, now, sess.SSOProvider, sess.UserID, sess.CreatedAt.UTC(), now).Scan(&fenced); err != nil || fenced > 0 {
		_ = s.DeleteSession(ctx, sess.TokenHash)
		if err != nil {
			return err
		}
		return ErrSSOSessionEnded
	}
	return nil
}

func (s *sessionStore) EndSSOSession(ctx context.Context, provider, sid string, fenceUntil time.Time) error {
	if _, err := s.store.db.ExecContext(ctx, s.store.rebind(`DELETE FROM sso_logouts WHERE expires_at <= ?`), time.Now().UTC()); err != nil {
		return err
	}
	if _, err := s.store.db.ExecContext(ctx, s.store.rebind(`INSERT INTO sso_logouts (provider, sid, expires_at) VALUES (?, ?, ?) ON CONFLICT (provider, sid) DO UPDATE SET expires_at = excluded.expires_at`), provider, sid, fenceUntil.UTC()); err != nil {
		return err
	}
	_, err := s.store.db.ExecContext(ctx, s.store.rebind(`DELETE FROM sessions WHERE sso_provider = ? AND sso_sid = ?`), provider, sid)
	return err
}

func (s *sessionStore) EndSSOSubject(ctx context.Context, provider, subject string, cutoff, fenceUntil time.Time) error {
	if _, err := s.store.db.ExecContext(ctx, s.store.rebind(`DELETE FROM sso_subject_logouts WHERE expires_at <= ?`), time.Now().UTC()); err != nil {
		return err
	}
	// Keyed by subject, not user, so it also fences a first login that is still provisioning.
	if _, err := s.store.db.ExecContext(ctx, s.store.rebind(`INSERT INTO sso_subject_logouts (provider, subject, cutoff, expires_at) VALUES (?, ?, ?, ?)
ON CONFLICT (provider, subject) DO UPDATE SET cutoff = excluded.cutoff, expires_at = excluded.expires_at WHERE sso_subject_logouts.cutoff < excluded.cutoff`), provider, subject, cutoff.UTC(), fenceUntil.UTC()); err != nil {
		return err
	}
	_, err := s.store.db.ExecContext(ctx, s.store.rebind(`DELETE FROM sessions WHERE sso_provider = ? AND created_at < ? AND user_id IN (SELECT user_id FROM user_sso_identities WHERE provider = ? AND subject = ?)`), provider, cutoff.UTC(), provider, subject)
	return err
}

func (u *userStore) CompletePasswordChange(ctx context.Context, userID, oldHash, newHash, ip string) error {
	return u.replacePassword(ctx, userID, oldHash, newHash, ip, true, "forced replacement; sessions revoked")
}

// ChangePassword is the self-service change of an unflagged local password.
func (u *userStore) ChangePassword(ctx context.Context, userID, oldHash, newHash, ip string) error {
	return u.replacePassword(ctx, userID, oldHash, newHash, ip, false, "self-service change; sessions revoked")
}

func (u *userStore) replacePassword(ctx context.Context, userID, oldHash, newHash, ip string, forced bool, details string) error {
	return u.store.withPassword(ctx, userID, oldHash, true, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx, u.store.rebind(`UPDATE users SET password_hash = ?, must_change_password = ?, updated_at = ? WHERE id = ? AND must_change_password = ? AND sso_provider = 'local'`), newHash, false, now, userID, forced)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrNotFound
		}
		return u.revokePasswordGrants(ctx, tx, userID, details, ip, now)
	})
}

// ResetAdminPassword is the operator recovery path, including disabled local accounts. It
// clears MFA too: the operator has the host, and a lost authenticator otherwise locks them out.
func (u *userStore) ResetAdminPassword(ctx context.Context, userID, newHash string) error {
	tx, err := u.store.beginTx(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, u.store.rebind(`UPDATE users SET password_hash = ?, must_change_password = ?, status = 'active', role = 'admin', totp_secret_enc = '', totp_enabled = ?, recovery_codes_hash = '', updated_at = ? WHERE id = ? AND sso_provider = 'local'`), newHash, true, false, now, userID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if err := u.revokePasswordGrants(ctx, tx, userID, "operator reset; sessions and MFA cleared", "", now); err != nil {
		return err
	}
	return tx.Commit()
}

func (u *userStore) revokePasswordGrants(ctx context.Context, tx *sql.Tx, userID, details, ip string, now time.Time) error {
	for _, table := range []string{"sessions", "mfa_challenges", "device_pairings"} {
		if _, err := tx.ExecContext(ctx, u.store.rebind("DELETE FROM "+table+" WHERE user_id = ?"), userID); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, u.store.rebind(`INSERT INTO audit_records (user_id, action, resource, details, ip_address, created_at) VALUES (?, ?, ?, ?, ?, ?)`), userID, "auth.password_changed", "user", details, ip, now)
	return err
}

func (s *sessionStore) GetSession(ctx context.Context, tokenHash string) (*Session, error) {
	q := s.store.rebind(`
SELECT token_hash, user_id, user_agent, ip_address, created_at, expires_at
FROM sessions WHERE token_hash = ?
`)
	var sess Session
	err := s.store.db.QueryRowContext(ctx, q, tokenHash).Scan(
		&sess.TokenHash, &sess.UserID, &sess.UserAgent, &sess.IPAddress,
		&sess.CreatedAt, &sess.ExpiresAt,
	)
	if err != nil {
		if errorsIs(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		_ = s.DeleteSession(ctx, tokenHash)
		return nil, ErrSessionExpired
	}
	return &sess, nil
}

func (s *sessionStore) DeleteSession(ctx context.Context, tokenHash string) error {
	q := s.store.rebind("DELETE FROM sessions WHERE token_hash = ?")
	_, err := s.store.db.ExecContext(ctx, q, tokenHash)
	return err
}

func (s *sessionStore) DeleteUserSessions(ctx context.Context, userID string) error {
	q := s.store.rebind("DELETE FROM sessions WHERE user_id = ?")
	_, err := s.store.db.ExecContext(ctx, q, userID)
	return err
}

func (s *sessionStore) CleanExpiredSessions(ctx context.Context) error {
	q := s.store.rebind("DELETE FROM sessions WHERE expires_at < ?")
	_, err := s.store.db.ExecContext(ctx, q, time.Now().UTC())
	return err
}

func (s *sessionStore) CreateMFAChallenge(ctx context.Context, challenge *MFAChallenge, expectedPasswordHash string) error {
	return s.store.withPassword(ctx, challenge.UserID, expectedPasswordHash, false, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.store.rebind("INSERT INTO mfa_challenges (token_hash, user_id, expires_at, password_hash, sso_provider, sso_sid, sso_issued_at) VALUES (?, ?, ?, ?, ?, ?, ?)"), challenge.TokenHash, challenge.UserID, challenge.ExpiresAt, expectedPasswordHash, challenge.SSOProvider, challenge.SSOSID, sql.NullTime{Time: challenge.SSOIssuedAt.UTC(), Valid: !challenge.SSOIssuedAt.IsZero()})
		return err
	})
}

func (s *sessionStore) ConsumeMFAChallenge(ctx context.Context, tokenHash string) (*MFAChallenge, error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := s.store.rebind("SELECT user_id, expires_at, password_hash, sso_provider, sso_sid, sso_issued_at FROM mfa_challenges WHERE token_hash = ?")
	c := MFAChallenge{TokenHash: tokenHash}
	var issuedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, q, tokenHash).Scan(&c.UserID, &c.ExpiresAt, &c.PasswordHash, &c.SSOProvider, &c.SSOSID, &issuedAt); err != nil {
		if errorsIs(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.SSOIssuedAt = issuedAt.Time
	if !time.Now().UTC().Before(c.ExpiresAt) {
		return nil, ErrSessionExpired
	}
	deleteQ := s.store.rebind("DELETE FROM mfa_challenges WHERE token_hash = ?")
	res, err := tx.ExecContext(ctx, deleteQ, tokenHash)
	if err != nil {
		return nil, err
	}
	rows, err := res.RowsAffected()
	if err != nil || rows != 1 {
		return nil, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ---------------------------------------------------------------------
// Device Pairing Store
// ---------------------------------------------------------------------

type deviceStore struct {
	store *SQLStore
}

func (d *deviceStore) CreatePairing(ctx context.Context, p *DevicePairing) error {
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.ExpiresAt.IsZero() {
		p.ExpiresAt = now.Add(90 * time.Second) // 90-second ephemeral pairing standard
	}

	q := d.store.rebind(`
INSERT INTO device_pairings (secret_hash, user_id, device_name, platform, push_token, status, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`)
	_, err := d.store.db.ExecContext(ctx, q,
		crypto.SHA256Hex([]byte(p.Secret)), p.UserID, p.DeviceName, p.Platform, p.PushToken,
		p.Status, p.CreatedAt, p.ExpiresAt,
	)
	return err
}

func (d *deviceStore) GetPairingBySecret(ctx context.Context, secret string) (*DevicePairing, error) {
	q := d.store.rebind(`
SELECT user_id, device_name, platform, push_token, status, created_at, expires_at
FROM device_pairings WHERE secret_hash = ?
`)
	p, err := d.scanPairing(d.store.db.QueryRowContext(ctx, q, crypto.SHA256Hex([]byte(secret))))
	if err != nil {
		return nil, err
	}
	p.Secret = secret
	return p, nil
}

func (d *deviceStore) scanPairing(row interface{ Scan(...any) error }) (*DevicePairing, error) {
	var p DevicePairing
	err := row.Scan(
		&p.UserID, &p.DeviceName, &p.Platform,
		&p.PushToken, &p.Status, &p.CreatedAt, &p.ExpiresAt,
	)
	if err != nil {
		if errorsIs(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if time.Now().UTC().After(p.ExpiresAt) {
		return nil, ErrPairingExpired
	}
	return &p, nil
}

func (d *deviceStore) ConsumePairing(ctx context.Context, secret, deviceName, platform, pushToken string) error {
	q := d.store.rebind(`
UPDATE device_pairings SET status = 'consumed', device_name = ?, platform = ?, push_token = ?
WHERE secret_hash = ? AND status = 'pending' AND expires_at > ?
`)
	res, err := d.store.db.ExecContext(ctx, q, deviceName, platform, pushToken, crypto.SHA256Hex([]byte(secret)), time.Now().UTC())
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *deviceStore) CleanExpiredPairings(ctx context.Context) error {
	q := d.store.rebind("DELETE FROM device_pairings WHERE expires_at < ?")
	_, err := d.store.db.ExecContext(ctx, q, time.Now().UTC())
	return err
}

// ---------------------------------------------------------------------
// Audit Store
// ---------------------------------------------------------------------

type auditStore struct {
	store *SQLStore
}

func (a *auditStore) LogAudit(ctx context.Context, r *AuditRecord) error {
	r.Resource = protocol.CleanText(r.Resource, 255)
	if r.Scope == "" {
		r.Scope = "platform"
	}
	if r.Result == "" {
		r.Result = "unknown"
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	q := a.store.rebind(`
INSERT INTO audit_records (user_id, action, resource, details, ip_address, created_at, scope, organization_id, environment_id, correlation_id, result)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`)
	tx, err := a.store.beginTx(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, q, r.UserID, r.Action, r.Resource, r.Details, r.IPAddress, r.CreatedAt, r.Scope, r.OrganizationID, r.EnvironmentID, r.CorrelationID, r.Result)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (a *auditStore) ListAuditRecords(ctx context.Context, offset, limit int) ([]*AuditRecord, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var count int
	err := a.store.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM audit_records").Scan(&count)
	if err != nil {
		return nil, 0, err
	}

	q := a.store.rebind(`
SELECT id, user_id, action, resource, details, ip_address, created_at, scope, organization_id, environment_id, correlation_id, result
FROM audit_records
ORDER BY created_at DESC LIMIT ? OFFSET ?
`)
	rows, err := a.store.db.QueryContext(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var records []*AuditRecord
	for rows.Next() {
		var r AuditRecord
		if err := rows.Scan(&r.ID, &r.UserID, &r.Action, &r.Resource, &r.Details, &r.IPAddress, &r.CreatedAt, &r.Scope, &r.OrganizationID, &r.EnvironmentID, &r.CorrelationID, &r.Result); err != nil {
			return nil, 0, err
		}
		records = append(records, &r)
	}
	return records, count, rows.Err()
}

// ---------------------------------------------------------------------
// Settings Store
// ---------------------------------------------------------------------

type settingsStore struct {
	store *SQLStore
}

func (s *settingsStore) GetSetting(ctx context.Context, key string) (string, error) {
	q := s.store.rebind("SELECT value FROM server_settings WHERE key = ?")
	var val string
	err := s.store.db.QueryRowContext(ctx, q, key).Scan(&val)
	if err != nil {
		if errorsIs(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return val, nil
}

func (s *settingsStore) SetSetting(ctx context.Context, key, val string) error {
	if strings.HasPrefix(key, "installer.") {
		return ErrInstallerOwned
	}
	if key == "sso_providers_enc" {
		return s.store.setUnmanagedProviderSetting(ctx, val, false)
	}
	now := time.Now().UTC()
	q := s.store.rebind(`
INSERT INTO server_settings (key, value, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
`)
	_, err := s.store.db.ExecContext(ctx, q, key, val, now)
	return err
}

func (s *settingsStore) DeleteSetting(ctx context.Context, key string) error {
	if strings.HasPrefix(key, "installer.") {
		return ErrInstallerOwned
	}
	if key == "sso_providers_enc" {
		return s.store.setUnmanagedProviderSetting(ctx, "", true)
	}
	_, err := s.store.db.ExecContext(ctx, s.store.rebind(`DELETE FROM server_settings WHERE key = ?`), key)
	return err
}

func (s *settingsStore) GetAllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.store.db.QueryContext(ctx, "SELECT key, value FROM server_settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		settings[k] = v
	}
	return settings, rows.Err()
}

func errorsIs(err, target error) bool {
	if err == nil || target == nil {
		return err == target
	}
	return err == target || strings.Contains(err.Error(), target.Error())
}

// Usage reports what the database holds, cheaply enough to read every minute: SQLite counts
// its pages minus the freelist, PostgreSQL reports the database size. Neither number is a
// measure of reclaimable data alone, which is why the level's release does not depend on it.
func (s *SQLStore) Usage(ctx context.Context) (int64, error) {
	if s.driver == "postgres" {
		var n int64
		err := s.db.QueryRowContext(ctx, "SELECT pg_database_size(current_database())").Scan(&n)
		return n, err
	}
	var pages, free, size int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return 0, err
	}
	if free > pages {
		free = pages
	}
	return (pages - free) * size, nil
}

// EvaluatePressure re-reads usage and settles the level, returning it. Degrading at 95 % leaves
// room to prune before anything must stop; the threshold divides before multiplying so a very
// large budget cannot overflow into a negative one.
//
// What raises the level is being over budget *and still growing*. Any prune pass under
// pressure releases it, whether or not it freed rows.
//
// Release cannot depend on the reading falling. A delete returns pages to the table rather than
// the file, a plain PostgreSQL vacuum never shrinks an index, and the reading counts data
// retention cannot touch at all: audit, which is never refused and not yet pruned, and
// inventory, where a report replaces a row rather than adding one. So once a pass has taken
// everything retention is owed, holding the level achieves nothing that dropping more metrics
// could fix — it would only refuse telemetry for the rest of the server's life.
//
// The result at the ceiling is a throttle rather than a stop: telemetry resumes, usage grows,
// the level returns. That is escapable by construction, and a condition that persists is
// reported at intervals rather than once, because it needs an operator, not a log line.
func (s *SQLStore) EvaluatePressure(ctx context.Context) (Pressure, error) {
	current := s.Pressure()
	if s.budget <= 0 {
		return current, nil
	}
	used, err := s.Usage(ctx)
	if err != nil {
		return current, err
	}
	implied := PressureNormal
	switch {
	case used >= s.budget:
		implied = PressureStopped
	case used >= s.budget/100*95:
		implied = PressureDegraded
	}
	next := current
	switch {
	case implied > current && used > s.lastUsed.Load():
		next = implied
	case implied < current:
		next = implied
	case current > PressureNormal:
		next = PressureNormal
	}
	s.lastUsed.Store(used)
	s.SetPressure(next)
	return next, nil
}

// Budget is the configured ceiling, zero when the check is disabled.
func (s *SQLStore) Budget() int64 { return s.budget }

func (s *SQLStore) Pressure() Pressure { return Pressure(s.pressure.Load()) }

func (s *SQLStore) SetPressure(p Pressure) { s.pressure.Store(int32(p)) }

func (s *SQLStore) SetAllowPrivileged(on bool) { s.allowPrivileged.Store(on) }

// SampleCeiling is the stored-rows-per-endpoint limit this store enforces.
func (s *SQLStore) SampleCeiling() int { return s.ceiling }
