package devices_test

import (
	"context"
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
	if len(initRes.Code) != 6 || len(initRes.Secret) == 0 {
		t.Errorf("unexpected code or secret: %+v", initRes)
	}

	// 2. Poll initial status -> pending
	p, err := svc.PollPairingStatus(ctx, initRes.Secret)
	if err != nil || p.Status != "pending" {
		t.Fatalf("expected pending status, got %v (err: %v)", p, err)
	}

	// 3. Verify pairing by code
	verified, _, err := svc.VerifyPairing(ctx, initRes.Code, "Alice iPhone", "ios", "apns-token-12345")
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
	if _, _, err := svc.VerifyPairing(ctx, initRes.Code, "Attacker", "web", "other"); err == nil {
		t.Fatal("consumed pairing was replayed")
	}
}
