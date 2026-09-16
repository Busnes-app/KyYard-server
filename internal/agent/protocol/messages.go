package protocol

import (
	"crypto/ed25519"
	"encoding/json"
	"strconv"
	"time"
)

// Version is the protocol major version both sides negotiate in hello.
const Version = 1

// Envelope is one frame (docs/agent-protocol.md section 4). Payload is decoded by Type.
type Envelope struct {
	V         int             `json:"v"`
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Org       string          `json:"org,omitempty"`
	Env       string          `json:"env,omitempty"`
	Endpoint  string          `json:"endpoint,omitempty"`
	Deadline  *time.Time      `json:"deadline,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Frame types used by the connection lifecycle.
const (
	TypeChallenge = "challenge"
	TypeAuth      = "auth"
	TypeHello     = "hello"
	TypeHeartbeat = "heartbeat"
	TypeInventory = "inventory"
	TypeApproved  = "enrollment.approved"
	TypeError     = "error"
)

// Close reasons the server sends in the WebSocket close frame.
const (
	CloseRevoked      = "identity_revoked"
	CloseDuplicate    = "duplicate_connection"
	CloseIncompatible = "incompatible_version"
	CloseRejected     = "enrollment_rejected"
	CloseShutdown     = "server_shutdown"
	CloseTimeout      = "heartbeat_timeout"
	CloseProtocol     = "protocol_error"
)

// Challenge is the server's first frame: a fresh nonce and its pinned identity.
type Challenge struct {
	Nonce               []byte `json:"nonce"`
	InstanceFingerprint string `json:"instance_fingerprint"`
	Versions            []int  `json:"versions"`
}

// Auth is the agent's reply, signed under ContextAuth by its endpoint key.
type Auth struct {
	EndpointID string `json:"endpoint_id"`
	Version    int    `json:"version"`
	Signature  []byte `json:"signature"`
}

// Hello is exchanged after authentication. The server's copy states the endpoint state and the
// heartbeat it expects; the agent's copy states what it can do.
type Hello struct {
	State            string   `json:"state,omitempty"`
	HeartbeatSeconds int      `json:"heartbeat_seconds,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"`
	AgentVersion     string   `json:"agent_version,omitempty"`
}

// Inventory carries a monotonically increasing generation; contents arrive with M4.
type Inventory struct {
	Generation uint64            `json:"generation"`
	Facts      map[string]string `json:"facts,omitempty"`
}

// AuthPreimage binds the signature to this endpoint, this nonce, the host the agent dialed and
// the version it speaks, so a capture cannot be replayed elsewhere or later.
func AuthPreimage(endpointID string, nonce []byte, serverHost string, version int) []byte {
	return Preimage(ContextAuth, []byte(endpointID), nonce, []byte(serverHost), []byte(strconv.Itoa(version)))
}

func VerifyAuth(publicKey []byte, endpointID string, nonce []byte, serverHost string, version int, signature []byte) bool {
	if len(publicKey) != ed25519.PublicKeySize || len(nonce) != 32 || len(signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(publicKey), AuthPreimage(endpointID, nonce, serverHost, version), signature)
}
