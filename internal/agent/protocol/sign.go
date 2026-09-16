// Package protocol holds the wire-level helpers shared by the control plane and the agent:
// signed-message preimages (docs/agent-protocol.md section 2) and key fingerprints.
package protocol

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

const (
	ContextEnroll = "kyyard-agent-enroll-v1"
	ContextAuth   = "kyyard-agent-auth-v1"
	ContextRotate = "kyyard-agent-rotate-v1"
	TokenSize     = 32
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

// VerifyEnrollment checks the agent's proof of possession over the raw token.
func VerifyEnrollment(publicKey, token, proof []byte) bool {
	if len(publicKey) != ed25519.PublicKeySize || len(token) != TokenSize || len(proof) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(publicKey), Preimage(ContextEnroll, token), proof)
}
