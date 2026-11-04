package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Identity ownership is shared by provisioned accounts and explicit local-account links.
func (u *userStore) registerSSOIdentity(ctx context.Context, tx *sql.Tx, user *User) error {
	if user.SSOProvider == "local" || user.SSOSubject == "" {
		return nil
	}
	return u.insertSSOIdentity(ctx, tx, user.ID, user.SSOProvider, user.SSOSubject)
}

func (u *userStore) insertSSOIdentity(ctx context.Context, tx *sql.Tx, userID, provider, subject string) error {
	_, err := tx.ExecContext(ctx, u.store.rebind(`INSERT INTO user_sso_identities (provider, subject, user_id) VALUES (?, ?, ?) ON CONFLICT(provider, subject) DO NOTHING`), provider, subject, userID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return ErrAlreadyExists
		}
		return err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, u.store.rebind(`SELECT user_id FROM user_sso_identities WHERE provider = ? AND subject = ?`), provider, subject).Scan(&owner); err != nil {
		return err
	}
	if owner != userID {
		return ErrAlreadyExists
	}
	return nil
}

// LinkSSO proves the original session and password again at the atomic write. Local
// credentials, account ID and tenant memberships remain owned by the existing account.
func (u *userStore) LinkSSO(ctx context.Context, userID, sessionHash, passwordHash, provider, subject, ip string) error {
	if provider == InstallerProvider {
		return ErrInstallerOwned
	}
	if provider == "" || provider == "local" || len(provider) > 64 || subject == "" || len(subject) > 255 || passwordHash == "" {
		return ErrInvalid
	}
	return u.store.withPassword(ctx, userID, passwordHash, true, func(tx *sql.Tx) error {
		var local string
		var forced bool
		if err := tx.QueryRowContext(ctx, u.store.rebind(`SELECT sso_provider, must_change_password FROM users WHERE id = ?`), userID).Scan(&local, &forced); err != nil {
			return err
		}
		if local != "local" || forced {
			return ErrInvalid
		}
		query := `SELECT user_id FROM sessions WHERE token_hash = ? AND user_id = ? AND expires_at > ?`
		if u.store.driver == "postgres" {
			query += " FOR UPDATE"
		}
		var owner string
		if err := tx.QueryRowContext(ctx, u.store.rebind(query), sessionHash, userID, time.Now().UTC()).Scan(&owner); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if err := u.insertSSOIdentity(ctx, tx, userID, provider, subject); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, u.store.rebind(`INSERT INTO audit_records (user_id, action, resource, result, ip_address, created_at) VALUES (?, ?, ?, ?, ?, ?)`), userID, "auth.sso.link", provider, "success", ip, time.Now().UTC())
		return err
	})
}

// SSOIdentity is one provider identity that signs in to an account. LinkedAt is the newest
// auth.sso.link audit row for it, nil for a provisioned identity or a pruned row.
type SSOIdentity struct {
	Provider string     `json:"provider"`
	Subject  string     `json:"subject"`
	LinkedAt *time.Time `json:"linked_at,omitempty"`
}

// ListSSOIdentities returns the identities that sign in to userID, by provider.
func (u *userStore) ListSSOIdentities(ctx context.Context, userID string) ([]SSOIdentity, error) {
	rows, err := u.store.db.QueryContext(ctx, u.store.rebind(`SELECT provider, subject FROM user_sso_identities WHERE user_id = ? ORDER BY provider`), userID)
	if err != nil {
		return nil, err
	}
	out := []SSOIdentity{}
	for rows.Next() {
		var id SSOIdentity
		if err := rows.Scan(&id.Provider, &id.Subject); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		var at time.Time
		err := u.store.db.QueryRowContext(ctx, u.store.rebind(`SELECT created_at FROM audit_records WHERE user_id = ? AND action = 'auth.sso.link' AND resource = ? ORDER BY id DESC LIMIT 1`), userID, out[i].Provider).Scan(&at)
		if err == nil {
			out[i].LinkedAt = &at
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}
	return out, nil
}

// UnlinkSSO removes a local account's identity at provider under the password hash the caller
// proved, ends the sessions and pending MFA challenges that provider signed in, and audits
// auth.sso.unlink in the same transaction. No such identity is ErrNotFound.
func (u *userStore) UnlinkSSO(ctx context.Context, userID, passwordHash, provider, ip string) error {
	if passwordHash == "" {
		return ErrInvalid
	}
	return u.store.withPassword(ctx, userID, passwordHash, true, func(tx *sql.Tx) error {
		var local string
		if err := tx.QueryRowContext(ctx, u.store.rebind(`SELECT sso_provider FROM users WHERE id = ?`), userID).Scan(&local); err != nil {
			return err
		}
		if local != "local" {
			return ErrInvalid
		}
		res, err := tx.ExecContext(ctx, u.store.rebind(`DELETE FROM user_sso_identities WHERE user_id = ? AND provider = ?`), userID, provider)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		for _, table := range []string{"sessions", "mfa_challenges"} {
			if _, err := tx.ExecContext(ctx, u.store.rebind(`DELETE FROM `+table+` WHERE user_id = ? AND sso_provider = ?`), userID, provider); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, u.store.rebind(`INSERT INTO audit_records (user_id, action, resource, result, ip_address, created_at) VALUES (?, ?, ?, ?, ?, ?)`), userID, "auth.sso.unlink", provider, "success", ip, time.Now().UTC())
		return err
	})
}
