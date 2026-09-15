package auth_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Busness-app/ky-primitives/totp"
	"github.com/Busness-app/ky_server_base/internal/auth"
)

func TestTOTPValidateReturnsCounter(t *testing.T) {
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if uri := auth.GenerateTOTPURL("BusnesApp", "alice", secret); !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("uri %q", uri)
	}
	now := time.Now()
	code, err := totp.Code(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	counter, ok := auth.ValidateTOTP(secret, code)
	if !ok {
		t.Fatal("fresh code rejected")
	}
	if want := now.Unix() / totp.Period; counter < want-1 || counter > want+1 {
		t.Fatalf("counter %d not within one step of %d", counter, want)
	}
	if _, ok := auth.ValidateTOTP(secret, "000000"); ok {
		if _, ok2 := auth.ValidateTOTP(secret, "999999"); ok2 {
			t.Fatal("two arbitrary codes both valid")
		}
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashedJSON, err := auth.GenerateRecoveryCodes(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 8 || len(codes[0]) != 14 { // xxxx-xxxx-xxxx
		t.Fatalf("codes %v", codes)
	}

	// Typed in upper case without dashes: still redeems.
	typed := strings.ToUpper(strings.ReplaceAll(codes[3], "-", ""))
	updated, ok := auth.RedeemRecoveryCode(typed, hashedJSON)
	if !ok {
		t.Fatalf("normalised form of %q rejected", codes[3])
	}

	// The slot is blanked, not removed: still 8 entries, one empty.
	var digests []string
	if err := json.Unmarshal([]byte(updated), &digests); err != nil {
		t.Fatal(err)
	}
	if len(digests) != 8 || digests[3] != "" {
		t.Fatalf("slot 3 not blanked in place: %v", digests)
	}

	if _, again := auth.RedeemRecoveryCode(codes[3], updated); again {
		t.Fatal("redeemed code accepted twice")
	}
	if _, other := auth.RedeemRecoveryCode(codes[4], updated); !other {
		t.Fatal("unrelated code rejected after a redemption")
	}
}

func TestProofOfWorkChallenge(t *testing.T) {
	const secret = "test-signing-secret"
	challenge, err := auth.GeneratePoWChallenge(1000, secret)
	if err != nil {
		t.Fatalf("failed to generate PoW challenge: %v", err)
	}

	// Solve challenge client-side
	var winningNumber int = -1
	for i := 1; i <= challenge.MaxNumber; i++ {
		sol := auth.PoWSolution{
			Algorithm: challenge.Algorithm,
			Salt:      challenge.Salt,
			Challenge: challenge.Challenge,
			Number:    i,
			MaxNumber: challenge.MaxNumber,
			ExpiresAt: challenge.ExpiresAt,
			Signature: challenge.Signature,
		}
		solBytes, _ := json.Marshal(sol)
		solB64 := base64.StdEncoding.EncodeToString(solBytes)
		if auth.VerifyPoWSolution(solB64, secret) {
			winningNumber = i
			break
		}
	}

	if winningNumber == -1 {
		t.Errorf("failed to solve PoW within max iterations")
	}
	forged := auth.PoWSolution{Algorithm: "SHA-256", Salt: "mine", Challenge: "fake", Number: 1, MaxNumber: 1, ExpiresAt: challenge.ExpiresAt, Signature: challenge.Signature}
	forgedJSON, _ := json.Marshal(forged)
	if auth.VerifyPoWSolution(base64.StdEncoding.EncodeToString(forgedJSON), secret) {
		t.Fatal("accepted forged challenge metadata")
	}
}

func TestPolicyValidation(t *testing.T) {
	if err := auth.ValidatePassword("short"); err != auth.ErrPasswordTooShort {
		t.Errorf("expected ErrPasswordTooShort, got %v", err)
	}
	if err := auth.ValidatePassword("ValidLongPassword123!"); err != nil {
		t.Errorf("expected valid password, got %v", err)
	}

	if err := auth.ValidateUsername("a"); err != auth.ErrInvalidUsername {
		t.Errorf("expected ErrInvalidUsername for short username, got %v", err)
	}
	if err := auth.ValidateUsername("alice.ops_01"); err != nil {
		t.Errorf("expected valid username, got %v", err)
	}

	if err := auth.ValidateEmail("not-an-email"); err != auth.ErrInvalidEmail {
		t.Errorf("expected ErrInvalidEmail, got %v", err)
	}
	if err := auth.ValidateEmail("alice@busnes.app"); err != nil {
		t.Errorf("expected valid email, got %v", err)
	}
}
