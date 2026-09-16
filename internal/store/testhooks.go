package store

import (
	"context"
	"time"
)

// BackdatePendingKey moves a pending key's creation time; tests use it to reach the expiry
// window without waiting seven days. It is not part of any product path.
func (s *SQLStore) BackdatePendingKey(ctx context.Context, fingerprint string, createdAt time.Time) error {
	_, err := s.db.ExecContext(ctx, s.rebind(`UPDATE endpoint_keys SET created_at=? WHERE fingerprint=?`), createdAt.UTC(), fingerprint)
	return err
}
