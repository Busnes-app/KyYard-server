package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
)

// ErrCiphertextTooShort reports a payload shorter than the AES-GCM nonce it must start with.
var ErrCiphertextTooShort = errors.New("crypto: ciphertext too short")

// ErrKeyLength reports an AES-256-GCM key that is not exactly 32 bytes.
var ErrKeyLength = errors.New("crypto: AES-256-GCM key must be exactly 32 bytes")

// EncryptAESGCM encrypts plaintext with AES-256-GCM under a 32-byte key and a random
// 12-byte nonce, returning nonce||ciphertext as raw base64url.
func EncryptAESGCM(plaintext, key []byte) (string, error) {
	aesGCM, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(aesGCM.Seal(nonce, nonce, plaintext, nil)), nil
}

// DecryptAESGCM reverses EncryptAESGCM.
func DecryptAESGCM(encoded string, key []byte) ([]byte, error) {
	aesGCM, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(data) < aesGCM.NonceSize() {
		return nil, ErrCiphertextTooShort
	}
	nonce, ciphertext := data[:aesGCM.NonceSize()], data[aesGCM.NonceSize():]
	return aesGCM.Open(nil, nonce, ciphertext, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrKeyLength
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// DeriveKey derives a 32-byte key from master for one specific use, so a value encrypted
// under one derived key never decrypts under another even if master is shared.
func DeriveKey(master []byte, label string) []byte {
	h := hmac.New(sha256.New, master)
	h.Write([]byte(label))
	return h.Sum(nil)
}

// ComputeHMACSHA256 computes HMAC-SHA256 hex string.
func ComputeHMACSHA256(data []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyHMACSHA256 verifies HMAC-SHA256 signature in constant time.
func VerifyHMACSHA256(data []byte, secret, expectedHex string) bool {
	actualHex := ComputeHMACSHA256(data, secret)
	return subtle.ConstantTimeCompare([]byte(actualHex), []byte(expectedHex)) == 1
}

// SHA256Hex returns the hex-encoded SHA-256 digest of input.
func SHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// RandomHex generates a cryptographically secure random hex string of 2*n characters.
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RandomBase64URL generates a cryptographically secure URL-safe base64 string.
func RandomBase64URL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// GeneratePKCE creates a PKCE code_verifier and code_challenge (S256).
func GeneratePKCE() (verifier string, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}
