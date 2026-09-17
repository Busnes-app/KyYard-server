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
	TypeCommand   = "command"          // server → agent: do one thing to one named resource
	TypeResult    = "result"           // agent → server: what became of it
	TypeError     = "error"
)

// Command outcomes. Every dispatched command ends in exactly one of these, and the set is
// closed: an outcome the server has not heard is Unknown, never a missing row.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeDenied    = "denied"    // the agent refused: wrong tenant, unmet precondition
	OutcomeTimedOut  = "timed_out" // the deadline passed before the runtime answered
	OutcomeUnknown   = "unknown"   // the socket went before a result came back
)

// Container actions an operator may take on a container that already exists. Destroying one is
// deliberately absent: it is a different permission and needs its own confirmation.
const (
	ActionStart   = "container.start"
	ActionStop    = "container.stop"
	ActionRestart = "container.restart"
	// ActionRemove destroys a container. It is listed with the others because it travels the
	// same path, but it carries a different permission and needs a confirmation the server
	// checks before the frame is built.
	ActionRemove = "container.remove"
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

// Command is one instruction for one endpoint. ID is a ULID minted by the server and is the
// dedupe key: an agent that has already run this ID returns its stored result rather than
// acting twice. RequestID is the audit correlation ID, so an agent's logs join the audit trail.
// The actor is never sent; who asked is recorded server-side only.
type Command struct {
	ID         string    `json:"id"`
	RequestID  string    `json:"request_id"`
	Org        string    `json:"org"`
	Env        string    `json:"env"`
	Endpoint   string    `json:"endpoint"`
	Deadline   time.Time `json:"deadline"`
	Action     string    `json:"action"`
	Container  string    `json:"container"`
	Capability string    `json:"capability,omitempty"`
	// Expects is what the actor saw when they asked. Docker has no universal resource
	// version, so this is operation-specific identity the agent re-checks immediately before
	// acting: the same container, in the state the decision was made about.
	Expects Expectation `json:"expects"`
}

// Expectation is the precondition a command is contingent on. An empty field is not checked,
// so a caller says only what its decision actually depended on.
type Expectation struct {
	ImageDigest string `json:"image_digest,omitempty"`
	State       string `json:"state,omitempty"`
}

// Result is what became of a command. Detail is bounded: an agent cannot make the control
// plane store an arbitrary amount of text by failing loudly.
type Result struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// MaxResultDetailBytes bounds the text an agent may attach to a result.
const MaxResultDetailBytes = 4 << 10
