package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/crypto"
)

// PoWChallenge represents a client-side SHA256 puzzle matching the KySecurity standard.
type PoWChallenge struct {
	Algorithm string `json:"algorithm"`
	Salt      string `json:"salt"`
	Challenge string `json:"challenge"`
	MaxNumber int    `json:"maxnumber"`
	ExpiresAt int64  `json:"expires_at"`
	Signature string `json:"signature"`
}

// PoWSolution represents the client's submitted answer.
type PoWSolution struct {
	Algorithm string `json:"algorithm"`
	Salt      string `json:"salt"`
	Challenge string `json:"challenge"`
	Number    int    `json:"number"`
	MaxNumber int    `json:"maxnumber"`
	ExpiresAt int64  `json:"expires_at"`
	Signature string `json:"signature"`
}

// GeneratePoWChallenge creates a puzzle of given difficulty (e.g. 5000 to 50000 max iterations).
func GeneratePoWChallenge(difficulty int, signingSecret string) (*PoWChallenge, error) {
	if difficulty <= 0 {
		difficulty = 20000
	}

	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return nil, err
	}
	salt := hex.EncodeToString(saltBytes)

	// Pick a random winning number in range [1, difficulty]
	nBig, err := rand.Int(rand.Reader, big.NewInt(int64(difficulty)))
	if err != nil {
		return nil, err
	}
	targetNumber := int(nBig.Int64()) + 1

	targetStr := fmt.Sprintf("%s%d", salt, targetNumber)
	h := sha256.Sum256([]byte(targetStr))
	challenge := hex.EncodeToString(h[:])

	challengeExpires := time.Now().Add(5 * time.Minute).Unix()
	signaturePayload := fmt.Sprintf("%s:%s:%d:%d", salt, challenge, difficulty, challengeExpires)
	return &PoWChallenge{
		Algorithm: "SHA-256",
		Salt:      salt,
		Challenge: challenge,
		MaxNumber: difficulty,
		ExpiresAt: challengeExpires,
		Signature: crypto.ComputeHMACSHA256([]byte(signaturePayload), signingSecret),
	}, nil
}

// powSpentCap bounds remembered solutions. Each entry cost its sender a solved puzzle, so
// eviction at the cap only hands back work that was already paid for.
const powSpentCap = 10000

// PoWVerifier accepts each signed challenge once, remembering it until it expires.
type PoWVerifier struct {
	mu    sync.Mutex
	spent map[string]int64 // signature -> expires_at
}

// Verify reports whether the solution is valid and its challenge not yet spent.
func (v *PoWVerifier) Verify(solutionBase64, signingSecret string) bool {
	sol, ok := verifyPoWSolution(solutionBase64, signingSecret)
	if !ok {
		return false
	}
	now := time.Now().Unix()
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, used := v.spent[sol.Signature]; used {
		return false
	}
	if v.spent == nil {
		v.spent = make(map[string]int64)
	}
	if len(v.spent) >= powSpentCap {
		for sig, exp := range v.spent {
			if exp < now {
				delete(v.spent, sig)
			}
		}
	}
	if len(v.spent) >= powSpentCap {
		for sig := range v.spent {
			delete(v.spent, sig)
			break
		}
	}
	v.spent[sol.Signature] = sol.ExpiresAt
	return true
}

// verifyPoWSolution decodes and validates the client's base64-encoded JSON solution.
func verifyPoWSolution(solutionBase64, signingSecret string) (sol PoWSolution, ok bool) {
	data, err := base64.StdEncoding.DecodeString(solutionBase64)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(solutionBase64)
		if err != nil {
			return sol, false
		}
	}

	if err := json.Unmarshal(data, &sol); err != nil {
		return sol, false
	}

	if sol.Algorithm != "SHA-256" || sol.Salt == "" || sol.Challenge == "" || sol.Signature == "" ||
		sol.Number < 1 || sol.MaxNumber < 1 || sol.Number > sol.MaxNumber || sol.ExpiresAt < time.Now().Unix() {
		return sol, false
	}
	signaturePayload := sol.Salt + ":" + sol.Challenge + ":" + strconv.Itoa(sol.MaxNumber) + ":" + strconv.FormatInt(sol.ExpiresAt, 10)
	if !crypto.VerifyHMACSHA256([]byte(signaturePayload), signingSecret, sol.Signature) {
		return sol, false
	}

	targetStr := fmt.Sprintf("%s%d", sol.Salt, sol.Number)
	h := sha256.Sum256([]byte(targetStr))
	actualChallenge := hex.EncodeToString(h[:])

	return sol, actualChallenge == sol.Challenge
}
