// Package protocol holds the wire-level helpers shared by the control plane and the agent:
// signed-message preimages (docs/agent-protocol.md section 2) and key fingerprints.
package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"

	"github.com/Busnes-app/ky-primitives/hybridsig"
)

const (
	ContextEnroll = "kyyard-agent-enroll-v1"
	ContextAuth   = "kyyard-agent-auth-v1"
	ContextRotate = "kyyard-agent-rotate-v1"
	// ContextServer is signed by the control plane's instance key, never by an agent key.
	ContextServer = "kyyard-server-challenge-v2"
	TokenSize     = 32

	hybridKeyPrefix = hybridsig.Alg + "\x00"
	// HybridPublicKeySize is the wire length of a hybridsig (ML-DSA-65 + Ed25519) public key.
	HybridPublicKeySize = hybridsig.PublicKeyBytes
	// HybridPrivateKeySize is the persisted length of a tagged hybridsig private key:
	// "mldsa65+ed25519\x00" || mldsaSeed(32) || edSeed(32) = 80 bytes.
	// Distinct from ed25519.PrivateKeySize (64 bytes) so persisted identities are unambiguous.
	HybridPrivateKeySize = len(hybridKeyPrefix) + 2*hybridsig.SeedBytes
	// HybridSignatureSize is the wire length of a hybridsig signature.
	HybridSignatureSize = hybridsig.SignatureBytes
)

var (
	// ErrInvalidKey reports malformed or unsupported key bytes.
	ErrInvalidKey = errors.New("protocol: invalid signing key")
)

// Preimage is the context string and every field, each as a 4-byte big-endian length followed
// by the bytes, so a signature under one context can never verify under another or split
// differently. The prefix is total for any field a message can carry.
func Preimage(context string, fields ...[]byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(context)))
	out = append(out, context...)
	for _, f := range fields {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f)))
		out = append(out, f...)
	}
	return out
}

// Fingerprint identifies a public key: hex SHA-256 of its raw bytes.
func Fingerprint(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:])
}

// RawFingerprint is the 32-byte digest the rotation preimage carries; Fingerprint is its hex.
func RawFingerprint(publicKey []byte) []byte {
	sum := sha256.Sum256(publicKey)
	return sum[:]
}

func encodeHybridPrivateKey(k *hybridsig.PrivateKey) []byte {
	mSeed, edSeed := k.Seeds()
	out := make([]byte, 0, HybridPrivateKeySize)
	out = append(out, hybridKeyPrefix...)
	out = append(out, mSeed...)
	out = append(out, edSeed...)
	return out
}

func parseHybridPrivateKey(raw []byte) (*hybridsig.PrivateKey, error) {
	if len(raw) != HybridPrivateKeySize || !bytes.HasPrefix(raw, []byte(hybridKeyPrefix)) {
		return nil, ErrInvalidKey
	}
	seeds := raw[len(hybridKeyPrefix):]
	return hybridsig.FromSeeds(seeds[:hybridsig.SeedBytes], seeds[hybridsig.SeedBytes:])
}

// GenerateHybridKey creates a fresh ML-DSA-65 + Ed25519 hybrid signing keypair.
func GenerateHybridKey() (pub, priv []byte, err error) {
	k, err := hybridsig.Generate()
	if err != nil {
		return nil, nil, err
	}
	return k.Public().Bytes(), encodeHybridPrivateKey(k), nil
}

// DeriveHybridKey deterministically derives a domain-separated hybrid keypair from a
// 32-byte seed (such as the persisted server InstanceKey).
func DeriveHybridKey(seed []byte, domain string) (pub, priv []byte, err error) {
	if len(seed) != ed25519.SeedSize || domain == "" {
		return nil, nil, ErrInvalidKey
	}
	mMac := hmac.New(sha256.New, seed)
	_, _ = mMac.Write([]byte(domain + "/mldsa65"))
	eMac := hmac.New(sha256.New, seed)
	_, _ = eMac.Write([]byte(domain + "/ed25519"))
	k, err := hybridsig.FromSeeds(mMac.Sum(nil), eMac.Sum(nil))
	if err != nil {
		return nil, nil, err
	}
	return k.Public().Bytes(), encodeHybridPrivateKey(k), nil
}

// DeriveInstanceHybridKey deterministically derives the server's hybrid instance keypair
// from the persisted 32-byte InstanceKey seed.
func DeriveInstanceHybridKey(seed []byte) (pub, priv []byte, err error) {
	if len(seed) != ed25519.SeedSize {
		return nil, nil, ErrInvalidKey
	}
	mMac := hmac.New(sha256.New, seed)
	_, _ = mMac.Write([]byte("kyyard/instance-key/mldsa65/v1"))
	k, err := hybridsig.FromSeeds(mMac.Sum(nil), seed)
	if err != nil {
		return nil, nil, err
	}
	return k.Public().Bytes(), encodeHybridPrivateKey(k), nil
}

// ValidPublicKey reports whether pub is a valid hybridsig or legacy Ed25519 public key.
func ValidPublicKey(pub []byte) bool {
	if len(pub) == ed25519.PublicKeySize {
		return true
	}
	if len(pub) == HybridPublicKeySize {
		_, err := hybridsig.ParsePublicKey(pub)
		return err == nil
	}
	return false
}

// ValidPrivateKey reports whether priv is a valid hybridsig or legacy Ed25519 private key.
func ValidPrivateKey(priv []byte) bool {
	if len(priv) == ed25519.PrivateKeySize {
		return true
	}
	_, err := parseHybridPrivateKey(priv)
	return err == nil
}

// PublicKeyFromPrivate derives the encoded public key bytes for a hybridsig or legacy Ed25519 private key.
func PublicKeyFromPrivate(priv []byte) ([]byte, error) {
	if len(priv) == ed25519.PrivateKeySize {
		return bytes.Clone(ed25519.PrivateKey(priv).Public().(ed25519.PublicKey)), nil
	}
	k, err := parseHybridPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return k.Public().Bytes(), nil
}

// Sign signs preimage under label using priv (hybridsig or legacy Ed25519).
func Sign(priv []byte, label string, preimage []byte) ([]byte, error) {
	if len(priv) == ed25519.PrivateKeySize {
		return ed25519.Sign(ed25519.PrivateKey(priv), preimage), nil
	}
	k, err := parseHybridPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return k.Sign(label, preimage)
}

// Verify checks signature over preimage under label, dispatching strictly on pub's algorithm
// with no cross-algorithm fallback.
func Verify(pub []byte, label string, preimage, signature []byte) bool {
	if len(pub) == HybridPublicKeySize {
		k, err := hybridsig.ParsePublicKey(pub)
		if err != nil {
			return false
		}
		return k.Verify(label, preimage, signature) == nil
	}
	if len(pub) == ed25519.PublicKeySize {
		if len(signature) != ed25519.SignatureSize {
			return false
		}
		return ed25519.Verify(ed25519.PublicKey(pub), preimage, signature)
	}
	return false
}

// VerifyRotation checks that the current key signed the new one under the rotation context.
// Both legacy Ed25519 and hybridsig keys are accepted, allowing legacy agents to rotate up
// to hybridsig seamlessly.
func VerifyRotation(currentKey, currentRawFingerprint, newKey, signature []byte) bool {
	if !ValidPublicKey(currentKey) || !ValidPublicKey(newKey) {
		return false
	}
	return Verify(currentKey, ContextRotate, Preimage(ContextRotate, currentRawFingerprint, newKey), signature)
}

// VerifyEnrollment checks the agent's proof of possession over the raw token.
func VerifyEnrollment(publicKey, token, proof []byte) bool {
	if !ValidPublicKey(publicKey) || len(token) != TokenSize {
		return false
	}
	return Verify(publicKey, ContextEnroll, Preimage(ContextEnroll, token), proof)
}
