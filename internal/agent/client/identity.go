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
	// acknowledge it, and key_retired must then be able to promote it. LapsedRecorded carries
	// the same provenance PendingRecorded did, because an offer that lapsed unconfirmed is no
	// more trustworthy for having waited.
	LapsedPrivateKey []byte `json:"lapsed_private_key,omitempty"`
	LapsedRecorded   bool   `json:"lapsed_recorded,omitempty"`
	// RecoveryAttempt is how far along the candidate list a refused key has walked. It is an
	// index, not a promotion: no key is moved or overwritten until the server accepts one, so
	// a server fault that refuses every candidate leaves the identity exactly as it was.
	RecoveryAttempt int       `json:"recovery_attempt,omitempty"`
	RotatedAt       time.Time `json:"rotated_at"`
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
	id.LapsedPrivateKey, id.LapsedRecorded = nil, false
	id.RecoveryAttempt = 0
}

// candidate is a key the agent may try when the current one is refused, and the slot holding
// it, so a key the server accepts can be committed and the rest discarded.
type candidate struct {
	key    []byte
	lapsed bool
}

// candidates lists the keys to try, in order. Every key the server confirmed it holds comes
// before any key it never answered for, because a session killed between saving an offer and
// reading the answer leaves a key the server may never have recorded, and an unknown key draws
// exactly the refusal a revoked endpoint draws. An unconfirmed key is still worth a try, last.
func (id *Identity) candidates() []candidate {
	pending := len(id.PendingPrivateKey) == ed25519.PrivateKeySize
	lapsed := len(id.LapsedPrivateKey) == ed25519.PrivateKeySize
	var out []candidate
	for _, want := range []bool{true, false} {
		if pending && id.PendingRecorded == want {
			out = append(out, candidate{key: id.PendingPrivateKey})
		}
		if lapsed && id.LapsedRecorded == want {
			out = append(out, candidate{key: id.LapsedPrivateKey, lapsed: true})
		}
	}
	return out
}

// tryNext advances to the next candidate. It moves no key: the walk is an index, so a server
// answering every attempt with a refusal costs nothing but attempts, and the key that works is
// still on disk when the fault clears. It returns false when the list is exhausted.
func (id *Identity) tryNext() bool {
	if id.RecoveryAttempt >= len(id.candidates()) {
		return false
	}
	id.RecoveryAttempt++
	return true
}

// recovering reports whether the key in hand is a candidate rather than the identity's own key.
func (id *Identity) recovering() bool { return id.RecoveryAttempt > 0 }

// signingKey is the key this session authenticates with: the candidate under trial, or the
// identity's own key when not recovering.
func (id *Identity) signingKey() ed25519.PrivateKey {
	if c := id.currentCandidate(); c != nil {
		return ed25519.PrivateKey(c.key)
	}
	return ed25519.PrivateKey(id.PrivateKey)
}

func (id *Identity) currentCandidate() *candidate {
	all := id.candidates()
	if id.RecoveryAttempt < 1 || id.RecoveryAttempt > len(all) {
		return nil
	}
	return &all[id.RecoveryAttempt-1]
}

// commitCandidate is called only once the server has accepted the candidate: that answer is the
// proof the old key is retired, so the winner becomes the identity and the rest go.
func (id *Identity) commitCandidate() {
	c := id.currentCandidate()
	id.RecoveryAttempt = 0
	if c == nil {
		return
	}
	id.PrivateKey = c.key
	id.PendingPrivateKey, id.PendingFingerprint, id.PendingSince, id.PendingRecorded = nil, "", time.Time{}, false
	id.LapsedPrivateKey, id.LapsedRecorded = nil, false
	id.RotatedAt = time.Now().UTC()
}

// promotable reports whether a refusal of the current key has somewhere to go.
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
	return protocol.Fingerprint(id.signingKey().Public().(ed25519.PublicKey))
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
