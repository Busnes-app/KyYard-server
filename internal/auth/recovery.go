package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/Busness-app/ky-primitives/recoverycode"
)

// digest is the product's recovery-code hash: SHA-256 of the normalised code, hex.
// recoverycode leaves the hash to the product; what it owns is the normalisation, which is
// why both GenerateRecoveryCodes and RedeemRecoveryCode go through it.
func digest(normalised string) string {
	sum := sha256.Sum256([]byte(normalised))
	return hex.EncodeToString(sum[:])
}

// GenerateRecoveryCodes returns count one-time codes and the JSON array of digests to store
// in users.recovery_codes_hash.
func GenerateRecoveryCodes(count int) (plainCodes []string, hashedJSON string, err error) {
	if count <= 0 {
		count = 8
	}
	codes, err := recoverycode.Generate(count)
	if err != nil {
		return nil, "", err
	}
	digests := make([]string, len(codes))
	for i, c := range codes {
		digests[i] = digest(recoverycode.Normalize(c))
	}
	out, err := json.Marshal(digests)
	if err != nil {
		return nil, "", err
	}
	return codes, string(out), nil
}

// RedeemRecoveryCode matches what the user typed against the stored digests and blanks the
// matching slot in place. Removing the entry instead renumbers the list, which is how two
// concurrent redemptions lose one another's write under the store's compare-and-swap.
func RedeemRecoveryCode(candidateCode, hashedJSON string) (string, bool) {
	var digests []string
	if err := json.Unmarshal([]byte(hashedJSON), &digests); err != nil {
		return "", false
	}
	i, ok := recoverycode.MatchCode(candidateCode, digests, digest)
	if !ok {
		return "", false
	}
	digests[i] = ""
	out, err := json.Marshal(digests)
	if err != nil {
		return "", false
	}
	return string(out), true
}
