package auth

import (
	"time"

	"github.com/Busness-app/ky-primitives/totp"
)

// GenerateTOTPSecret returns a fresh base32 RFC 6238 secret.
func GenerateTOTPSecret() (string, error) { return totp.GenerateSecret() }

// GenerateTOTPURL returns the otpauth:// URI an authenticator app enrols from.
func GenerateTOTPURL(issuer, accountName, secret string) string {
	return totp.ProvisioningURI(issuer, accountName, secret)
}

// ValidateTOTP reports whether code is valid for secret now, and the counter it matched.
// The caller must spend the counter with UserStore.SpendTOTPCounter before trusting it.
func ValidateTOTP(secret, code string) (int64, bool) {
	return totp.Validate(secret, code, time.Now())
}
