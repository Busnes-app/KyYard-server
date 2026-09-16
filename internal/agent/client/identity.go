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
)

// Identity is everything the agent keeps between runs. The enrollment token is never in it.
type Identity struct {
	EndpointID          string `json:"endpoint_id"`
	PrivateKey          []byte `json:"private_key"`
	InstanceFingerprint string `json:"instance_fingerprint"`
	Server              string `json:"server"`
	// Generation is the last inventory generation sent; it only rises, across restarts too.
	Generation uint64 `json:"generation"`
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
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, identityPath(dir))
}

func newKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}
