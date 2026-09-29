package devices_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/devices"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

func TestPairingLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_alice", Username: "alice", Status: "active", SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
	svc := devices.NewPairingService(st, "KyYard", "http://localhost:8080")

	// 1. Init
	initRes, err := svc.InitPairing(ctx, "usr_alice")
	if err != nil {
		t.Fatalf("InitPairing failed: %v", err)
	}
	if len(initRes.Secret) != 48 || strings.Contains(initRes.QRPayload, `"code"`) {
		t.Errorf("want a 24-byte secret and no typed code: %+v", initRes)
	}

	// 2. Poll initial status -> pending
	p, err := svc.PollPairingStatus(ctx, initRes.Secret)
	if err != nil || p.Status != "pending" {
		t.Fatalf("expected pending status, got %v (err: %v)", p, err)
	}

	// 3. Verify pairing by the QR secret
	verified, _, err := svc.VerifyPairing(ctx, initRes.Secret, "Alice iPhone", "ios", "apns-token-12345")
	if err != nil {
		t.Fatalf("VerifyPairing failed: %v", err)
	}
	if verified.Status != "consumed" || verified.DeviceName != "Alice iPhone" {
		t.Errorf("unexpected verified status: %+v", verified)
	}

	// 4. Poll again -> consumed
	p2, err := svc.PollPairingStatus(ctx, initRes.Secret)
	if err != nil || p2.Status != "consumed" {
		t.Fatalf("expected consumed status on second poll, got %v", p2)
	}
	if _, _, err := svc.VerifyPairing(ctx, initRes.Secret, "Attacker", "web", "other"); !errors.Is(err, devices.ErrPairingNotFound) {
		t.Fatalf("consumed pairing was replayed: %v", err)
	}
}

// A six-digit code is guessable inside the 90-second window, so nothing short of the full
// secret may redeem a pairing, and every miss reads the same.
func TestPairingRejectsAnythingButTheSecret(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_alice", Username: "alice", Status: "active", SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
	svc := devices.NewPairingService(st, "KyYard", "http://localhost:8080")
	initRes, err := svc.InitPairing(ctx, "usr_alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, guess := range []string{"123456", "", initRes.Secret[:6], initRes.Secret[:47], strings.ToUpper(initRes.Secret)} {
		if _, _, err := svc.VerifyPairing(ctx, guess, "x", "pwa", ""); !errors.Is(err, devices.ErrPairingNotFound) {
			t.Errorf("guess %q: got %v, want ErrPairingNotFound", guess, err)
		}
	}
	if p, err := svc.PollPairingStatus(ctx, initRes.Secret); err != nil || p.Status != "pending" {
		t.Fatalf("a wrong guess consumed the pairing: %v %v", p, err)
	}
	if _, _, err := svc.VerifyPairing(ctx, initRes.Secret, "Alice iPhone", "ios", ""); err != nil {
		t.Fatalf("the real secret was refused: %v", err)
	}
}
