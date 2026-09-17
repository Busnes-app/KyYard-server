// Package client is the KyYard agent: durable identity, enrollment and the connection loop from
// docs/agent-protocol.md sections 2, 3, 6 and 9.
package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
)

// Identity is everything the agent keeps between runs. The enrollment token is never in it.
type Identity struct {
	EndpointID          string `json:"endpoint_id"`
	PrivateKey          []byte `json:"private_key"`
	InstanceFingerprint string `json:"instance_fingerprint"`
	Server              string `json:"server"`
	// Generation is the last inventory generation sent; it only rises, across restarts too.
	Generation uint64 `json:"generation"`
	// A rotated key waits here until the operator acknowledges it; the current key keeps
	// authenticating meanwhile.
	PendingPrivateKey  []byte    `json:"pending_private_key,omitempty"`
	PendingFingerprint string    `json:"pending_fingerprint,omitempty"`
	PendingSince       time.Time `json:"pending_since,omitempty"`
	// PendingRecorded says the server answered that it holds this offer. A key saved but never
	// confirmed (the session died between the write and the answer) may not exist server-side,
	// so it is the last key to try, never the first.
	PendingRecorded bool `json:"pending_recorded,omitempty"`
	// LapsedPrivateKey is an offer the agent gave up on. It is kept until the server proves it
	// gone (by recording a new offer), because with skewed clocks the server may still
	// acknowledge it, and key_retired must then be able to promote it.
	LapsedPrivateKey []byte    `json:"lapsed_private_key,omitempty"`
	RotatedAt        time.Time `json:"rotated_at"`
}

// Promote makes the acknowledged pending key the current one.
func (id *Identity) Promote() {
	if len(id.PendingPrivateKey) == ed25519.PrivateKeySize {
		id.PrivateKey = id.PendingPrivateKey
		id.RotatedAt = time.Now().UTC()
	}
	id.PendingPrivateKey = nil
	id.PendingFingerprint = ""
	id.PendingSince = time.Time{}
	id.PendingRecorded = false
	id.LapsedPrivateKey = nil
}

// switchKey moves to the next candidate when the current key is refused: the live offer
// first, then a lapsed one. The other candidate is kept so a wrong guess can still fall back.
// It returns false when nothing is left to try.
// switchKey moves to the next key to try. A confirmed offer comes first, then a lapsed one the
// server once recorded, and only then an offer the server never confirmed: promoting an
// unrecorded key ahead of a recorded one strands the agent, because the server cannot tell an
// unknown key from a revoked endpoint and answers both the same, which is terminal.
func (id *Identity) switchKey() bool {
	pending := len(id.PendingPrivateKey) == ed25519.PrivateKeySize
	switch {
	case pending && id.PendingRecorded:
		id.takePending()
	case len(id.LapsedPrivateKey) == ed25519.PrivateKeySize:
		id.PrivateKey = id.LapsedPrivateKey
		id.LapsedPrivateKey = nil
	case pending:
		id.takePending()
	default:
		return false
	}
	id.RotatedAt = time.Now().UTC()
	return true
}

// promotable reports whether a refusal of the current key has somewhere to go.
func (id *Identity) takePending() {
	id.PrivateKey = id.PendingPrivateKey
	id.PendingPrivateKey, id.PendingFingerprint, id.PendingSince = nil, "", time.Time{}
	id.PendingRecorded = false
}

func (id *Identity) promotable() bool {
	return len(id.PendingPrivateKey) == ed25519.PrivateKeySize || len(id.LapsedPrivateKey) == ed25519.PrivateKeySize
}

// pendingFingerprint derives the fingerprint from the retained pending key, so an offer whose
// bookkeeping lapsed can still be matched against the server's acknowledgement.
func (id *Identity) pendingFingerprint() string {
	if len(id.PendingPrivateKey) != ed25519.PrivateKeySize {
		return ""
	}
	return protocol.Fingerprint(ed25519.PrivateKey(id.PendingPrivateKey).Public().(ed25519.PublicKey))
}

func (id *Identity) fingerprint() string {
	return protocol.Fingerprint(ed25519.PrivateKey(id.PrivateKey).Public().(ed25519.PublicKey))
}

func identityPath(dir string) string { return filepath.Join(dir, "identity.json") }

// LoadIdentity returns nil, nil when the agent has never enrolled.
func LoadIdentity(dir string) (*Identity, error) {
	raw, err := os.ReadFile(identityPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("identity file: %w", err)
	}
	if len(id.PrivateKey) != ed25519.PrivateKeySize || id.EndpointID == "" {
		return nil, errors.New("identity file: incomplete")
	}
	return &id, nil
}

// SaveIdentity writes atomically with owner-only permissions on the directory and file.
func SaveIdentity(dir string, id *Identity) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(id)
	if err != nil {
		return err
	}
	tmp := identityPath(dir) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	// A rotation offer is announced only after this returns, so the bytes must be on disk,
	// not in the writeback window a power loss would erase.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, identityPath(dir)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func newKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}
