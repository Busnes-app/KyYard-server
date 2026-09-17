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
	TypeMetrics   = "metrics"
	TypeApproved  = "enrollment.approved"
	TypeRotate    = "identity.rotate"  // agent → server: a new key signed by the current one
	TypeRotated   = "identity.rotated" // server → agent: the operator acknowledged that key
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
	CloseKeyRetired   = "key_retired"        // switch to the acknowledged key
	CloseKeyPending   = "key_pending_review" // keep using the approved key
)

// Challenge is the server's first frame: a fresh nonce and its pinned identity.
type Challenge struct {
	Nonce               []byte `json:"nonce"`
	InstanceFingerprint string `json:"instance_fingerprint"`
	Versions            []int  `json:"versions"`
}

// Auth is the agent's reply, signed under ContextAuth by its endpoint key.
type Auth struct {
	EndpointID  string `json:"endpoint_id"`
	Fingerprint string `json:"fingerprint"` // which of the endpoint's keys signed
	Version     int    `json:"version"`
	Signature   []byte `json:"signature"`
}

// Rotate carries a freshly minted public key and the current key's signature over it.
type Rotate struct {
	PublicKey []byte `json:"public_key"`
	Signature []byte `json:"signature"`
}

// Rotated names the key that now authenticates (server → agent) or was recorded (ack of Rotate).
type Rotated struct {
	Fingerprint string `json:"fingerprint"`
	Code        string `json:"code,omitempty"`
}

// Hello is exchanged after authentication. The server's copy states the endpoint state and the
// heartbeat it expects; the agent's copy states what it can do.
type Hello struct {
	State            string   `json:"state,omitempty"`
	HeartbeatSeconds int      `json:"heartbeat_seconds,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"`
	AgentVersion     string   `json:"agent_version,omitempty"`
}

// UnmarshalHelloBounded limits capability count and total decoded text before
// the caller hands it to the store.
func UnmarshalHelloBounded(data []byte, h *Hello) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var raw struct {
		State            string
		HeartbeatSeconds int
		Capabilities     []string
		AgentVersion     string
	}
	if v, ok := fields["state"]; ok {
		if err := json.Unmarshal(v, &raw.State); err != nil {
			return err
		}
	}
	if v, ok := fields["heartbeat_seconds"]; ok {
		if err := json.Unmarshal(v, &raw.HeartbeatSeconds); err != nil {
			return err
		}
	}
	if v, ok := fields["agent_version"]; ok {
		if err := json.Unmarshal(v, &raw.AgentVersion); err != nil {
			return err
		}
	}
	if v, ok := fields["capabilities"]; ok {
		var err error
		raw.Capabilities, _, err = decodeBounded[string](v, 64)
		if err != nil {
			return err
		}
	}
	total := 0
	for i, v := range raw.Capabilities {
		if len(v) > 256 {
			raw.Capabilities[i] = v[:256]
		}
		total += len(raw.Capabilities[i])
		if total > 4096 {
			raw.Capabilities = raw.Capabilities[:i+1]
			raw.Capabilities[i] = raw.Capabilities[i][:len(raw.Capabilities[i])-(total-4096)]
			break
		}
	}
	h.State, h.HeartbeatSeconds, h.Capabilities, h.AgentVersion = raw.State, raw.HeartbeatSeconds, raw.Capabilities, raw.AgentVersion
	return nil
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
