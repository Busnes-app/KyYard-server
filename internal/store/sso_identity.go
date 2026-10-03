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
