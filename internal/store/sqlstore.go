package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

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
	groups   *groupStore
	audit    *auditStore
	settings *settingsStore
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
	s.groups = &groupStore{store: s}
	s.audit = &auditStore{store: s}
	s.settings = &settingsStore{store: s}

	return s, nil
}

func (s *SQLStore) Tenancy() TenancyStore { return &tenancyStore{store: s} }

func (s *SQLStore) Users() UserStore        { return s.users }
func (s *SQLStore) Sessions() SessionStore  { return s.sessions }
func (s *SQLStore) Devices() DeviceStore    { return s.devices }
func (s *SQLStore) Groups() GroupStore      { return s.groups }
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

	_, err := u.store.db.ExecContext(ctx, q,
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
	return nil
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
	q := u.store.rebind(`
SELECT id, username, email, display_name, password_hash, role, status,
       sso_provider, sso_subject, totp_secret_enc, totp_enabled,
       recovery_codes_hash, push_device_id, must_change_password,
       totp_last_counter, created_at, updated_at, last_login_at
FROM users WHERE LOWER(username) = LOWER(?)
`)
	return u.scanUser(u.store.db.QueryRowContext(ctx, q, username))
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
	q := u.store.rebind(`
SELECT id, username, email, display_name, password_hash, role, status,
       sso_provider, sso_subject, totp_secret_enc, totp_enabled,
       recovery_codes_hash, push_device_id, must_change_password,
       totp_last_counter, created_at, updated_at, last_login_at
FROM users WHERE sso_provider = ? AND sso_subject = ?
`)
	return u.scanUser(u.store.db.QueryRowContext(ctx, q, provider, subject))
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
WHERE id = ?
`)

	res, err := u.store.db.ExecContext(ctx, q,
		user.Username, user.Email, user.DisplayName, user.PasswordHash,
		user.Role, user.Status, user.SSOProvider, user.SSOSubject,
		user.TOTPSecretEnc, user.TOTPEnabled, user.RecoveryCodesHash,
		user.PushDeviceID, user.MustChangePassword, user.UpdatedAt,
		lastLogin, user.ID,
	)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
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
	q := u.store.rebind("DELETE FROM users WHERE id = ?")
	res, err := u.store.db.ExecContext(ctx, q, id)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (u *userStore) ListUsers(ctx context.Context, offset, limit int, search string) ([]*User, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var countQuery, listQuery string
	var countArgs, listArgs []any

	if strings.TrimSpace(search) != "" {
		like := "%" + strings.ToLower(strings.TrimSpace(search)) + "%"
		countQuery = "SELECT COUNT(1) FROM users WHERE LOWER(username) LIKE ? OR LOWER(display_name) LIKE ? OR LOWER(email) LIKE ?"
		countArgs = []any{like, like, like}

		listQuery = `
SELECT id, username, email, display_name, password_hash, role, status,
       sso_provider, sso_subject, totp_secret_enc, totp_enabled,
       recovery_codes_hash, push_device_id, must_change_password,
       totp_last_counter, created_at, updated_at, last_login_at
FROM users
WHERE LOWER(username) LIKE ? OR LOWER(display_name) LIKE ? OR LOWER(email) LIKE ?
ORDER BY created_at DESC LIMIT ? OFFSET ?`
		listArgs = []any{like, like, like, limit, offset}
	} else {
		countQuery = "SELECT COUNT(1) FROM users"
		listQuery = `
SELECT id, username, email, display_name, password_hash, role, status,
       sso_provider, sso_subject, totp_secret_enc, totp_enabled,
       recovery_codes_hash, push_device_id, must_change_password,
       totp_last_counter, created_at, updated_at, last_login_at
FROM users
ORDER BY created_at DESC LIMIT ? OFFSET ?`
		listArgs = []any{limit, offset}
	}

	var total int
	err := u.store.db.QueryRowContext(ctx, u.store.rebind(countQuery), countArgs...).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	rows, err := u.store.db.QueryContext(ctx, u.store.rebind(listQuery), listArgs...)
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
// Session Store
// ---------------------------------------------------------------------

type sessionStore struct {
	store *SQLStore
}

// withPassword serializes credential-derived grants with password replacement.
// Updating the same user row takes a write lock on both supported databases.
func (s *SQLStore) withPassword(ctx context.Context, userID, expectedHash string, apply func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
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
	return s.store.withPassword(ctx, sess.UserID, expectedPasswordHash, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.store.rebind(`INSERT INTO sessions (token_hash, user_id, user_agent, ip_address, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`), sess.TokenHash, sess.UserID, sess.UserAgent, sess.IPAddress, sess.CreatedAt, sess.ExpiresAt)
		return err
	})
}

func (u *userStore) CompletePasswordChange(ctx context.Context, userID, oldHash, newHash, ip string) error {
	return u.store.withPassword(ctx, userID, oldHash, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx, u.store.rebind(`UPDATE users SET password_hash = ?, must_change_password = ?, updated_at = ? WHERE id = ? AND must_change_password = ? AND sso_provider = 'local'`), newHash, false, now, userID, true)
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
		return u.revokePasswordGrants(ctx, tx, userID, "forced replacement; sessions revoked", ip, now)
	})
}

// ResetAdminPassword is the operator recovery path, including disabled local accounts.
func (u *userStore) ResetAdminPassword(ctx context.Context, userID, newHash string) error {
	tx, err := u.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, u.store.rebind(`UPDATE users SET password_hash = ?, must_change_password = ?, status = 'active', role = 'admin', updated_at = ? WHERE id = ? AND sso_provider = 'local'`), newHash, true, now, userID)
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
	if err := u.revokePasswordGrants(ctx, tx, userID, "operator reset; sessions revoked", "", now); err != nil {
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
	return s.store.withPassword(ctx, challenge.UserID, expectedPasswordHash, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.store.rebind("INSERT INTO mfa_challenges (token_hash, user_id, expires_at, password_hash) VALUES (?, ?, ?, ?)"), challenge.TokenHash, challenge.UserID, challenge.ExpiresAt, expectedPasswordHash)
		return err
	})
}

func (s *sessionStore) ConsumeMFAChallenge(ctx context.Context, tokenHash string) (string, string, error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	q := s.store.rebind("SELECT user_id, expires_at, password_hash FROM mfa_challenges WHERE token_hash = ?")
	var userID, passwordHash string
	var expiresAt time.Time
	if err := tx.QueryRowContext(ctx, q, tokenHash).Scan(&userID, &expiresAt, &passwordHash); err != nil {
		if errorsIs(err, sql.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", err
	}
	if !time.Now().UTC().Before(expiresAt) {
		return "", "", ErrSessionExpired
	}
	deleteQ := s.store.rebind("DELETE FROM mfa_challenges WHERE token_hash = ?")
	res, err := tx.ExecContext(ctx, deleteQ, tokenHash)
	if err != nil {
		return "", "", err
	}
	rows, err := res.RowsAffected()
	if err != nil || rows != 1 {
		return "", "", ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return userID, passwordHash, nil
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
INSERT INTO device_pairings (secret, code, user_id, device_name, platform, push_token, status, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
`)
	_, err := d.store.db.ExecContext(ctx, q,
		p.Secret, p.Code, p.UserID, p.DeviceName, p.Platform, p.PushToken,
		p.Status, p.CreatedAt, p.ExpiresAt,
	)
	return err
}

func (d *deviceStore) GetPairingByCode(ctx context.Context, code string) (*DevicePairing, error) {
	q := d.store.rebind(`
SELECT secret, code, user_id, device_name, platform, push_token, status, created_at, expires_at
FROM device_pairings WHERE code = ?
`)
	return d.scanPairing(d.store.db.QueryRowContext(ctx, q, code))
}

func (d *deviceStore) GetPairingBySecret(ctx context.Context, secret string) (*DevicePairing, error) {
	q := d.store.rebind(`
SELECT secret, code, user_id, device_name, platform, push_token, status, created_at, expires_at
FROM device_pairings WHERE secret = ?
`)
	return d.scanPairing(d.store.db.QueryRowContext(ctx, q, secret))
}

func (d *deviceStore) scanPairing(row interface{ Scan(...any) error }) (*DevicePairing, error) {
	var p DevicePairing
	err := row.Scan(
		&p.Secret, &p.Code, &p.UserID, &p.DeviceName, &p.Platform,
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
WHERE secret = ? AND status = 'pending' AND expires_at > ?
`)
	res, err := d.store.db.ExecContext(ctx, q, deviceName, platform, pushToken, secret, time.Now().UTC())
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
// Group Store
// ---------------------------------------------------------------------

type groupStore struct {
	store *SQLStore
}

func (g *groupStore) CreateGroup(ctx context.Context, group *Group) error {
	now := time.Now().UTC()
	if group.CreatedAt.IsZero() {
		group.CreatedAt = now
	}
	if group.UpdatedAt.IsZero() {
		group.UpdatedAt = now
	}

	q := g.store.rebind(`
INSERT INTO groups (id, display_name, external_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
`)
	_, err := g.store.db.ExecContext(ctx, q, group.ID, group.DisplayName, group.ExternalID, group.CreatedAt, group.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (g *groupStore) GetGroupByID(ctx context.Context, id string) (*Group, error) {
	q := g.store.rebind("SELECT id, display_name, external_id, created_at, updated_at FROM groups WHERE id = ?")
	var grp Group
	err := g.store.db.QueryRowContext(ctx, q, id).Scan(&grp.ID, &grp.DisplayName, &grp.ExternalID, &grp.CreatedAt, &grp.UpdatedAt)
	if err != nil {
		if errorsIs(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	members, err := g.getMembers(ctx, grp.ID)
	if err != nil {
		return nil, err
	}
	grp.Members = members
	return &grp, nil
}

func (g *groupStore) GetGroupByName(ctx context.Context, name string) (*Group, error) {
	q := g.store.rebind("SELECT id, display_name, external_id, created_at, updated_at FROM groups WHERE LOWER(display_name) = LOWER(?)")
	var grp Group
	err := g.store.db.QueryRowContext(ctx, q, name).Scan(&grp.ID, &grp.DisplayName, &grp.ExternalID, &grp.CreatedAt, &grp.UpdatedAt)
	if err != nil {
		if errorsIs(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	members, err := g.getMembers(ctx, grp.ID)
	if err != nil {
		return nil, err
	}
	grp.Members = members
	return &grp, nil
}

func (g *groupStore) getMembers(ctx context.Context, groupID string) ([]string, error) {
	q := g.store.rebind("SELECT user_id FROM group_members WHERE group_id = ?")
	rows, err := g.store.db.QueryContext(ctx, q, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err == nil {
			members = append(members, uid)
		}
	}
	return members, rows.Err()
}

func (g *groupStore) UpdateGroup(ctx context.Context, group *Group) error {
	group.UpdatedAt = time.Now().UTC()
	q := g.store.rebind("UPDATE groups SET display_name = ?, external_id = ?, updated_at = ? WHERE id = ?")
	res, err := g.store.db.ExecContext(ctx, q, group.DisplayName, group.ExternalID, group.UpdatedAt, group.ID)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (g *groupStore) DeleteGroup(ctx context.Context, id string) error {
	q := g.store.rebind("DELETE FROM groups WHERE id = ?")
	res, err := g.store.db.ExecContext(ctx, q, id)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (g *groupStore) ListGroups(ctx context.Context, offset, limit int) ([]*Group, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var count int
	err := g.store.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM groups").Scan(&count)
	if err != nil {
		return nil, 0, err
	}

	q := g.store.rebind("SELECT id, display_name, external_id, created_at, updated_at FROM groups ORDER BY display_name ASC LIMIT ? OFFSET ?")
	rows, err := g.store.db.QueryContext(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var groups []*Group
	for rows.Next() {
		var grp Group
		if err := rows.Scan(&grp.ID, &grp.DisplayName, &grp.ExternalID, &grp.CreatedAt, &grp.UpdatedAt); err != nil {
			return nil, 0, err
		}
		groups = append(groups, &grp)
	}

	for _, grp := range groups {
		members, _ := g.getMembers(ctx, grp.ID)
		grp.Members = members
	}

	return groups, count, rows.Err()
}

func (g *groupStore) AddGroupMember(ctx context.Context, groupID, userID string) error {
	q := g.store.rebind("INSERT INTO group_members (group_id, user_id) VALUES (?, ?) ON CONFLICT DO NOTHING")
	_, err := g.store.db.ExecContext(ctx, q, groupID, userID)
	return err
}

func (g *groupStore) RemoveGroupMember(ctx context.Context, groupID, userID string) error {
	q := g.store.rebind("DELETE FROM group_members WHERE group_id = ? AND user_id = ?")
	_, err := g.store.db.ExecContext(ctx, q, groupID, userID)
	return err
}

func (g *groupStore) GetUserGroups(ctx context.Context, userID string) ([]*Group, error) {
	q := g.store.rebind(`
SELECT g.id, g.display_name, g.external_id, g.created_at, g.updated_at
FROM groups g
JOIN group_members gm ON g.id = gm.group_id
WHERE gm.user_id = ?
ORDER BY g.display_name ASC
`)
	rows, err := g.store.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []*Group
	for rows.Next() {
		var grp Group
		if err := rows.Scan(&grp.ID, &grp.DisplayName, &grp.ExternalID, &grp.CreatedAt, &grp.UpdatedAt); err != nil {
			return nil, err
		}
		groups = append(groups, &grp)
	}
	return groups, rows.Err()
}

// ---------------------------------------------------------------------
// Audit Store
// ---------------------------------------------------------------------

type auditStore struct {
	store *SQLStore
}

func (a *auditStore) LogAudit(ctx context.Context, r *AuditRecord) error {
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
	_, err := a.store.db.ExecContext(ctx, q, r.UserID, r.Action, r.Resource, r.Details, r.IPAddress, r.CreatedAt, r.Scope, r.OrganizationID, r.EnvironmentID, r.CorrelationID, r.Result)
	return err
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
		if err := rows.Scan(&k, &v); err == nil {
			settings[k] = v
		}
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

// SampleCeiling is the stored-rows-per-endpoint limit this store enforces.
func (s *SQLStore) SampleCeiling() int { return s.ceiling }
