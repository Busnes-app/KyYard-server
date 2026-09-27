package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// SetPairingCodeSourceForTest pins the six-digit draw to src. Calls are serialised, so src
// may keep a plain counter while concurrent mints draw.
func SetPairingCodeSourceForTest(src func() string) (restore func()) {
	old := pairingCode
	var mu sync.Mutex
	pairingCode = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return src(), nil
	}
	return func() { pairingCode = old }
}

func SetPairingLifeForTest(d time.Duration) (restore func()) {
	old := pairingLife
	pairingLife = d
	return func() { pairingLife = old }
}

// LivePairingCodeHashes returns the code_hash of every currently live (unconsumed, unexpired)
// pairing, for a concurrency test to prove no two collide.
func LivePairingCodeHashes(ctx context.Context, st Store) ([]string, error) {
	s := st.(*SQLStore)
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT code_hash FROM service_token_pairings WHERE consumed_at IS NULL AND expires_at>?`), time.Now().UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		out = append(out, hash)
	}
	return out, rows.Err()
}

// PairingRowExists reports whether a service_token_pairings row with this id still exists,
// expired or not, for a test to prove the sweep in CreateServicePairing physically deletes
// expired rows rather than merely excluding them from a live query.
func PairingRowExists(ctx context.Context, st Store, id string) (bool, error) {
	s := st.(*SQLStore)
	var exists int
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT 1 FROM service_token_pairings WHERE id=?`), id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
