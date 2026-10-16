package store

import (
	"context"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/crypto"
)

func TestPairingSecretIsStoredOnlyAsItsHash(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	secret := "raw-pairing-secret"
	if err := st.Devices().CreatePairing(ctx, &DevicePairing{Secret: secret, Status: "pending", ExpiresAt: time.Now().UTC().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := st.db.QueryRowContext(ctx, `SELECT secret_hash FROM device_pairings`).Scan(&stored); err != nil || stored != crypto.SHA256Hex([]byte(secret)) {
		t.Fatalf("stored %q %v", stored, err)
	}
	if p, err := st.Devices().GetPairingBySecret(ctx, secret); err != nil || p.Secret != secret {
		t.Fatalf("lookup %+v %v", p, err)
	}
	if _, err := st.Devices().GetPairingBySecret(ctx, stored); err == nil {
		t.Fatal("the stored hash redeemed the pairing")
	}
	if err := st.Devices().ConsumePairing(ctx, secret, "phone", "ios", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Devices().ConsumePairing(ctx, secret, "phone", "ios", ""); err == nil {
		t.Fatal("consumed twice")
	}
}
