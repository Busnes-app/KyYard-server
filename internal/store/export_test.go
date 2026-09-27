package store

import "time"

func SetPairingCodeForTest(code string) (restore func()) {
	old := pairingCode
	pairingCode = func() (string, error) { return code, nil }
	return func() { pairingCode = old }
}

func SetPairingLifeForTest(d time.Duration) (restore func()) {
	old := pairingLife
	pairingLife = d
	return func() { pairingLife = old }
}
