package store

import (
	"errors"
	"time"
)

func SetPairingCodeForTest(code string) (restore func()) {
	old := pairingCode
	pairingCode = func() (string, error) { return code, nil }
	return func() { pairingCode = old }
}

// SetPairingCodesForTest replays codes in order, one per draw; a draw past the end of codes
// errors, matching a real generator never repeating deterministically.
func SetPairingCodesForTest(codes ...string) (restore func()) {
	old := pairingCode
	i := 0
	pairingCode = func() (string, error) {
		if i >= len(codes) {
			return "", errors.New("test: pairing codes exhausted")
		}
		c := codes[i]
		i++
		return c, nil
	}
	return func() { pairingCode = old }
}

func SetPairingLifeForTest(d time.Duration) (restore func()) {
	old := pairingLife
	pairingLife = d
	return func() { pairingLife = old }
}
