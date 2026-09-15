package crypto_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Busness-app/ky_server_base/internal/crypto"
)

func TestAESGCMEncryption(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	plaintext := []byte("totp secret")

	enc, err := crypto.EncryptAESGCM(plaintext, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	dec, err := crypto.DecryptAESGCM(enc, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(dec, plaintext) {
		t.Fatalf("round trip: got %q", dec)
	}

	other := bytes.Repeat([]byte{0x43}, 32)
	if _, err := crypto.DecryptAESGCM(enc, other); err == nil {
		t.Fatal("wrong key decrypted")
	}
}

func TestAESGCMRefusesShortKey(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := crypto.EncryptAESGCM([]byte("x"), make([]byte, n))
		if !errors.Is(err, crypto.ErrKeyLength) {
			t.Errorf("len %d: got %v, want ErrKeyLength", n, err)
		}
	}
}

func TestHMACSHA256(t *testing.T) {
	secret := "shared-sync-webhook-secret"
	payload := []byte(`{"event":"user.updated","username":"alice"}`)

	sig := crypto.ComputeHMACSHA256(payload, secret)
	if !crypto.VerifyHMACSHA256(payload, secret, sig) {
		t.Errorf("expected HMAC verification to succeed")
	}

	if crypto.VerifyHMACSHA256(payload, "wrong-secret", sig) {
		t.Errorf("expected HMAC verification to fail with wrong secret")
	}
}

func TestPKCEGeneration(t *testing.T) {
	verifier, challenge, err := crypto.GeneratePKCE()
	if err != nil {
		t.Fatalf("failed to generate PKCE: %v", err)
	}
	if len(verifier) == 0 || len(challenge) == 0 {
		t.Errorf("expected non-empty verifier and challenge")
	}
	if verifier == challenge {
		t.Errorf("verifier and challenge should not be identical")
	}
}

func TestDeriveKey(t *testing.T) {
	master := []byte("0123456789abcdef0123456789abcdef")
	a := crypto.DeriveKey(master, "app:setting:one")
	b := crypto.DeriveKey(master, "app:setting:two")
	if len(a) != 32 {
		t.Fatalf("derived key is %d bytes, want 32", len(a))
	}
	if string(a) == string(b) {
		t.Error("two different labels derived the same key")
	}
	if string(a) != string(crypto.DeriveKey(master, "app:setting:one")) {
		t.Error("the same label derived a different key on a second call")
	}
}
