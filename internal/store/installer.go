package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/applysetup"
)

const InstallerConfigKey = "installer.config"
const InstallerProvider = "kyidentity"

var ErrInstallerOwned = errors.New("installer-owned authority")
var installerUsername = regexp.MustCompile(`^[a-z0-9._-]{3,64}$`)

type installerIdentity struct {
	SetupID string `json:"setupId"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	Role    string `json:"role"`
}

func InstallerUserID(issuer, subject string) string {
	hash := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "usr_" + hex.EncodeToString(hash[:12])
}

func ValidateInstallerBundle(b applysetup.Common) error {
	if err := b.Validate("kyyard", "/api/sso/kyidentity/callback"); err != nil {
		return err
	}
	if b.SSO == nil || len(b.Admins) == 0 || b.SSO.DirectorySecretFile == "" || b.SSO.SCIMTokenFile != "" || b.Backup != nil || strings.TrimRight(b.SSO.IssuerURL, "/") != b.SSO.IssuerURL {
		return errors.New("KyYard requires canonical KyIdentity SSO, explicit admins and a protected directory credential; installer SCIM and backup enrollment unsupported")
	}
	for _, list := range [][]applysetup.Identity{b.Admins, b.Everyday} {
		for _, id := range list {
			if !installerUsername.MatchString(id.Username) || len(id.Subject) > 255 {
				return errors.New("invalid KyYard identity name or subject")
			}
		}
	}
	return nil
}

func (s *SQLStore) InstallerConfiguration(ctx context.Context) (applysetup.Common, bool, error) {
	raw, err := s.Settings().GetSetting(ctx, InstallerConfigKey)
	if errors.Is(err, ErrNotFound) {
		return applysetup.Common{}, false, nil
	}
	var b applysetup.Common
	if err != nil {
		return b, true, err
	}
	if err = applysetup.DecodeRaw([]byte(raw), &b); err != nil {
		return b, true, err
	}
	return b, true, ValidateInstallerBundle(b)
}

func (s *SQLStore) ApplyInstaller(ctx context.Context, b applysetup.Common, check bool) (applysetup.Report, error) {
	r := applysetup.NewReport(b)
	if err := ValidateInstallerBundle(b); err != nil {
		return r, err
	}
	var tx *sql.Tx
	var err error
	if check {
		tx, err = s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	} else {
		tx, err = s.beginTx(ctx, true)
	}
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, s.rebind("SELECT value FROM server_settings WHERE key=?"), InstallerConfigKey).Scan(&raw)
	present := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	if present {
		var old applysetup.Common
		if applysetup.DecodeRaw([]byte(raw), &old) != nil || !reflect.DeepEqual(old, b) {
			return r, errors.New("installer configuration conflicts")
		}
	} else {
		if check {
			return r, errors.New("installer setup absent")
		}
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM server_settings WHERE key='sso_providers_enc'").Scan(&n); err != nil {
			return r, err
		}
		if n != 0 {
			return r, errors.New("installer refuses existing provider configuration")
		}
	}
	now := time.Now().UTC()
	for i, list := range [][]applysetup.Identity{b.Admins, b.Everyday} {
		role := "user"
		if i == 0 {
			role = "admin"
		}
		for _, identity := range list {
			id := InstallerUserID(identity.Issuer, identity.Subject)
			owned := installerIdentity{b.SetupID, identity.Issuer, identity.Subject, role}
			key := "installer.user." + id
			var metadata string
			e := tx.QueryRowContext(ctx, s.rebind("SELECT value FROM server_settings WHERE key=?"), key).Scan(&metadata)
			if present {
				var got installerIdentity
				if e != nil || applysetup.DecodeRaw([]byte(metadata), &got) != nil || got != owned {
					return r, errors.New("installer ownership drift")
				}
				var name, gotRole, status, provider, subject, password string
				e = tx.QueryRowContext(ctx, s.rebind("SELECT username,role,status,sso_provider,sso_subject,password_hash FROM users WHERE id=?"), id).Scan(&name, &gotRole, &status, &provider, &subject, &password)
				if e != nil || name != identity.Username || gotRole != role || status != "active" || provider != InstallerProvider || subject != identity.Subject || password != "" {
					return r, errors.New("installer identity drift requires explicit resolution")
				}
				var owner string
				e = tx.QueryRowContext(ctx, s.rebind("SELECT user_id FROM user_sso_identities WHERE provider=? AND subject=?"), InstallerProvider, identity.Subject).Scan(&owner)
				if e != nil || owner != id {
					return r, errors.New("installer provider binding drift")
				}
				r.Present = append(r.Present, applysetup.Object{Kind: "identity", Name: identity.Username, ID: id})
				continue
			}
			if e == nil || !errors.Is(e, sql.ErrNoRows) {
				return r, errors.New("installer refuses existing ownership")
			}
			var n int
			e = tx.QueryRowContext(ctx, s.rebind("SELECT COUNT(*) FROM users WHERE id=? OR LOWER(username)=LOWER(?)"), id, identity.Username).Scan(&n)
			if e != nil {
				return r, e
			}
			if n != 0 {
				return r, errors.New("installer refuses existing identity")
			}
			_, e = tx.ExecContext(ctx, s.rebind(`INSERT INTO users (id,username,email,display_name,password_hash,role,status,sso_provider,sso_subject,recovery_codes_hash,created_at,updated_at) VALUES (?,?,?,?,'',?,'active',?,?,'[]',?,?)`), id, identity.Username, identity.Email, identity.Username, role, InstallerProvider, identity.Subject, now, now)
			if e != nil {
				return r, e
			}
			if e = s.users.insertSSOIdentity(ctx, tx, id, InstallerProvider, identity.Subject); e != nil {
				return r, e
			}
			bytes, e := json.Marshal(owned)
			if e != nil {
				return r, e
			}
			if e = s.installerSetting(ctx, tx, key, string(bytes), now); e != nil {
				return r, e
			}
			if e = s.installerAudit(ctx, tx, "installer.identity_created", id, b.SetupID, now); e != nil {
				return r, e
			}
			r.Created = append(r.Created, applysetup.Object{Kind: "identity", Name: identity.Username, ID: id})
		}
	}
	if present {
		r.Present = append(r.Present, applysetup.Object{Kind: "configuration", Name: "runtime"})
		return r, nil
	}
	bytes, err := json.Marshal(b)
	if err != nil {
		return r, err
	}
	if err = s.installerSetting(ctx, tx, InstallerConfigKey, string(bytes), now); err != nil {
		return r, err
	}
	if err = s.installerAudit(ctx, tx, "installer.configuration_created", b.Service, b.SetupID, now); err != nil {
		return r, err
	}
	if err = tx.Commit(); err != nil {
		return r, err
	}
	r.Created = append(r.Created, applysetup.Object{Kind: "configuration", Name: "runtime"})
	return r, nil
}

func (s *SQLStore) installerSetting(ctx context.Context, tx *sql.Tx, key, value string, now time.Time) error {
	_, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO server_settings (key,value,updated_at) VALUES (?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`), key, value, now)
	return err
}
func (s *SQLStore) installerAudit(ctx context.Context, tx *sql.Tx, action, target, details string, now time.Time) error {
	_, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO audit_records (scope,user_id,action,resource,result,details,ip_address,created_at) VALUES ('platform','installer',?,?,'success',?,'installer-local',?)`), action, target, details, now)
	return err
}

// Provider changes share the native audit lock with first setup. Both operations
// create authority in one database, so neither can adopt the other's state.
func (s *SQLStore) setUnmanagedProviderSetting(ctx context.Context, value string, remove bool) error {
	tx, err := s.beginTx(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owned int
	if err = tx.QueryRowContext(ctx, s.rebind("SELECT COUNT(*) FROM server_settings WHERE key=?"), InstallerConfigKey).Scan(&owned); err != nil {
		return err
	}
	if owned != 0 {
		return ErrInstallerOwned
	}
	if remove {
		_, err = tx.ExecContext(ctx, "DELETE FROM server_settings WHERE key='sso_providers_enc'")
	} else {
		err = s.installerSetting(ctx, tx, "sso_providers_enc", value, time.Now().UTC())
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) InstallerUserAllowed(ctx context.Context, u *User) (bool, error) {
	raw, err := s.Settings().GetSetting(ctx, "installer.user."+u.ID)
	if errors.Is(err, ErrNotFound) {
		return u.SSOProvider != InstallerProvider, nil
	}
	if err != nil {
		return false, err
	}
	var owned installerIdentity
	if err = applysetup.DecodeRaw([]byte(raw), &owned); err != nil {
		return false, err
	}
	b, managed, err := s.InstallerConfiguration(ctx)
	if err != nil || !managed {
		return false, err
	}
	if owned.SetupID != b.SetupID || owned.Issuer != b.SSO.IssuerURL || owned.Subject != u.SSOSubject || u.ID != InstallerUserID(owned.Issuer, owned.Subject) || u.Role != owned.Role || u.SSOProvider != InstallerProvider || u.PasswordHash != "" {
		return false, nil
	}
	for i, list := range [][]applysetup.Identity{b.Admins, b.Everyday} {
		for _, id := range list {
			role := "user"
			if i == 0 {
				role = "admin"
			}
			if id.Issuer == owned.Issuer && id.Subject == owned.Subject && owned.Role == role {
				return true, nil
			}
		}
	}
	return false, nil
}

type installerDirectoryFence struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

// Directory messages change availability, never account binding or role. Audit,
// replay fence and grant revocation commit together in the native database.
func (s *SQLStore) ApplyInstallerDirectory(ctx context.Context, issuer, subject, event, revision, digest string, assigned, admin bool) error {
	n, err := strconv.ParseUint(revision, 10, 64)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != revision || len(digest) != 64 {
		return errors.New("invalid directory revision or digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return errors.New("invalid directory digest")
	}
	if event != "user.created" && event != "user.updated" && event != "user.deactivated" && event != "user.deleted" {
		return errors.New("unsupported directory event")
	}
	tx, err := s.beginTx(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	owned, id, err := s.installerIdentityTx(ctx, tx, issuer, subject)
	if err != nil {
		return err
	}
	key := "installer.directory." + id
	var raw string
	err = tx.QueryRowContext(ctx, s.rebind("SELECT value FROM server_settings WHERE key=?"), key).Scan(&raw)
	if err == nil {
		var last installerDirectoryFence
		if applysetup.DecodeRaw([]byte(raw), &last) != nil || last.Revision == 0 {
			return errors.New("corrupt directory fence")
		}
		if n < last.Revision || n == last.Revision && digest != last.Digest {
			return errors.New("stale or conflicting directory revision")
		}
		if n == last.Revision {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	active := assigned && admin == (owned.Role == "admin") && event != "user.deleted" && event != "user.deactivated"
	now := time.Now().UTC()
	if err := s.installerAvailability(ctx, tx, id, active, now); err != nil {
		return err
	}
	bytes, err := json.Marshal(installerDirectoryFence{n, digest})
	if err != nil {
		return err
	}
	if err = s.installerSetting(ctx, tx, key, string(bytes), now); err != nil {
		return err
	}
	if err = s.installerAudit(ctx, tx, "installer.directory_applied", id, revision+":"+digest+":"+event, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) installerIdentityTx(ctx context.Context, tx *sql.Tx, issuer, subject string) (installerIdentity, string, error) {
	id := InstallerUserID(issuer, subject)
	var raw string
	if err := tx.QueryRowContext(ctx, s.rebind("SELECT value FROM server_settings WHERE key=?"), InstallerConfigKey).Scan(&raw); err != nil {
		return installerIdentity{}, id, err
	}
	var b applysetup.Common
	if err := applysetup.DecodeRaw([]byte(raw), &b); err != nil {
		return installerIdentity{}, id, err
	}
	if err := ValidateInstallerBundle(b); err != nil {
		return installerIdentity{}, id, err
	}
	if b.SSO.IssuerURL != issuer {
		return installerIdentity{}, id, ErrForbidden
	}
	if err := tx.QueryRowContext(ctx, s.rebind("SELECT value FROM server_settings WHERE key=?"), "installer.user."+id).Scan(&raw); err != nil {
		return installerIdentity{}, id, err
	}
	var owned installerIdentity
	if err := applysetup.DecodeRaw([]byte(raw), &owned); err != nil {
		return owned, id, err
	}
	if owned.SetupID != b.SetupID || owned.Issuer != issuer || owned.Subject != subject {
		return owned, id, ErrForbidden
	}
	for i, list := range [][]applysetup.Identity{b.Admins, b.Everyday} {
		role := "user"
		if i == 0 {
			role = "admin"
		}
		for _, identity := range list {
			if identity.Issuer == issuer && identity.Subject == subject && owned.Role == role {
				return owned, id, nil
			}
		}
	}
	return owned, id, ErrForbidden
}

func (s *SQLStore) installerAvailability(ctx context.Context, tx *sql.Tx, id string, active bool, now time.Time) error {
	status := "inactive"
	if active {
		status = "active"
	}
	result, err := tx.ExecContext(ctx, s.rebind("UPDATE users SET status=?,updated_at=? WHERE id=? AND sso_provider=? AND password_hash=''"), status, now, id, InstallerProvider)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrForbidden
	}
	if !active {
		for _, table := range []string{"sessions", "mfa_challenges", "device_pairings"} {
			if _, err := tx.ExecContext(ctx, s.rebind("DELETE FROM "+table+" WHERE user_id=?"), id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Loss of an app-specific claim disables the account until a newer signed
// directory assignment explicitly restores it; a login cannot reactivate it.
func (s *SQLStore) RevokeInstallerIdentity(ctx context.Context, issuer, subject string) error {
	tx, err := s.beginTx(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, id, err := s.installerIdentityTx(ctx, tx, issuer, subject)
	if err != nil {
		return err
	}
	var status string
	if err = tx.QueryRowContext(ctx, s.rebind("SELECT status FROM users WHERE id=?"), id).Scan(&status); err != nil {
		return err
	}
	if status == "inactive" {
		return nil
	}
	now := time.Now().UTC()
	if err = s.installerAvailability(ctx, tx, id, false, now); err != nil {
		return err
	}
	if err = s.installerAudit(ctx, tx, "installer.app_role_lost", id, "", now); err != nil {
		return err
	}
	return tx.Commit()
}
