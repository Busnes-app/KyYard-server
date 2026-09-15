package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busness-app/ky-primitives/capsule"
	"github.com/Busness-app/ky-primitives/recoveryclient"
	"github.com/Busness-app/ky-primitives/recoverykey"
)

func sealFixture(t *testing.T, service string) (string, []string) {
	t.Helper()
	priv, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	shares, err := recoverykey.Split(priv, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	key := recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}
	payload := recoveryclient.Payload{ServiceName: service, AppVersion: "1.0.0",
		Files: []recoveryclient.File{{Path: "data/x.db", Data: []byte("payload"), Mode: 0600}}}
	raw, _, err := recoveryclient.Seal(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "x.kycap")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Non-consecutive indices: {1,2} would let an XOR-shaped bug pass.
	return path, []string{shares[0].String(), shares[2].String()}
}

func TestRestoreExtractsWithTwoShares(t *testing.T) {
	path, shares := sealFixture(t, "busnes_app")
	target := t.TempDir()
	var out bytes.Buffer
	if err := restore(path, target, "busnes_app", shares, &out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "data", "x.db"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("restored file: %q %v", got, err)
	}
	if !strings.Contains(out.String(), "busnes_app") {
		t.Fatalf("manifest not printed: %s", out.String())
	}
}

func TestRestoreRefusesAnotherService(t *testing.T) {
	path, shares := sealFixture(t, "someone_else")
	err := restore(path, t.TempDir(), "busnes_app", shares, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "someone_else") {
		t.Fatalf("got %v, want a service-name refusal naming the capsule's service", err)
	}
}

func TestRestoreRefusesTheWrongKit(t *testing.T) {
	path, _ := sealFixture(t, "busnes_app")
	_, otherShares := sealFixture(t, "busnes_app")
	err := restore(path, t.TempDir(), "busnes_app", otherShares, &bytes.Buffer{})
	if !errors.Is(err, capsule.ErrWrongRecoveryKey) {
		t.Fatalf("got %v, want ErrWrongRecoveryKey", err)
	}
}

func TestRestoreRefusesOneShare(t *testing.T) {
	path, shares := sealFixture(t, "busnes_app")
	if err := restore(path, t.TempDir(), "busnes_app", shares[:1], &bytes.Buffer{}); err == nil {
		t.Fatal("one share of a 2-of-3 kit was accepted")
	}
}
