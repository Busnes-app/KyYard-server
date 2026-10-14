package store

import (
	"context"
	"database/sql"
	"time"
)

// MFAState is the second-factor slice of a users row that self-service enrolment changes.
type MFAState struct {
	SecretEnc         string
	Enabled           bool
	RecoveryCodesHash string
}

// UpdateMFA swaps a local account's MFA state from exactly from to to, under the
// verified password hash, and records action in the same transaction. Any concurrent
// change (password, enrolment, a redeemed recovery code) makes it ErrNotFound.
func (u *userStore) UpdateMFA(ctx context.Context, userID, passwordHash string, from, to MFAState, action, ip string) error {
	return u.store.withPassword(ctx, userID, passwordHash, true, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx, u.store.rebind(`UPDATE users SET totp_secret_enc = ?, totp_enabled = ?, recovery_codes_hash = ?, updated_at = ?
WHERE id = ? AND sso_provider = 'local' AND must_change_password = ? AND totp_secret_enc = ? AND totp_enabled = ? AND recovery_codes_hash = ?`),
			to.SecretEnc, to.Enabled, to.RecoveryCodesHash, now,
			userID, false, from.SecretEnc, from.Enabled, from.RecoveryCodesHash)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, u.store.rebind(`INSERT INTO audit_records (user_id, action, resource, result, ip_address, created_at) VALUES (?, ?, ?, ?, ?, ?)`), userID, action, "user", "success", ip, now)
		return err
	})
}
