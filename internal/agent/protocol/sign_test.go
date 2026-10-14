package protocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"
)

// A signature made for one context must not verify under another, and shifting bytes
// between fields must change the preimage (docs/agent-protocol.md test 12).
func TestPreimagesAreDomainSeparated(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	token := make([]byte, TokenSize)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	proof := ed25519.Sign(priv, Preimage(ContextEnroll, token))
	if !VerifyEnrollment(pub, token, proof) {
		t.Fatal("valid enrollment proof refused")
	}
	if VerifyEnrollment(pub, token, ed25519.Sign(priv, Preimage(ContextAuth, token))) {
		t.Fatal("connection-context signature accepted as an enrollment proof")
	}
	if VerifyEnrollment(pub, token, ed25519.Sign(priv, Preimage(ContextRotate, token))) {
		t.Fatal("rotation-context signature accepted as an enrollment proof")
	}
	if VerifyEnrollment(pub, token[:31], proof) || VerifyEnrollment(pub[:31], token, proof) || VerifyEnrollment(pub, token, proof[:63]) {
		t.Fatal("wrong-length input accepted")
	}
	a := Preimage(ContextAuth, []byte("ab"), []byte("c"))
	b := Preimage(ContextAuth, []byte("a"), []byte("bc"))
	if string(a) == string(b) {
		t.Fatal("length prefixes do not separate fields")
	}
	// A field past 65535 bytes keeps a faithful length prefix, and the context is prefixed too.
	big := make([]byte, 70000)
	p := Preimage(ContextAuth, big)
	if len(p) != 4+len(ContextAuth)+4+len(big) || binary.BigEndian.Uint32(p[4+len(ContextAuth):]) != 70000 {
		t.Fatal("length prefix does not describe a large field")
	}
	if string(Preimage("ab", []byte("c"))) == string(Preimage("a", []byte("bc"))) {
		t.Fatal("context is not length-prefixed")
	}
	if Fingerprint(pub) == Fingerprint(append([]byte{}, pub[1:]...)) || len(Fingerprint(pub)) != 64 {
		t.Fatal("fingerprint shape")
	}
}

// The server's challenge proves the pinned instance key holds it: an echoed fingerprint, another
// key, another host, another nonce or an agent-context signature does not.
func TestChallengeProvesTheInstanceKey(t *testing.T) {
	_, instance, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	nonce := make([]byte, 32)
	ch := SignChallenge(instance, nonce, "yard.example")
	pinned := ch.InstanceFingerprint
	if !ch.Proven("yard.example", pinned) {
		t.Fatal("a genuine challenge was refused")
	}
	echoed := Challenge{Nonce: nonce, InstanceFingerprint: pinned, Versions: []int{Version}}
	forged := SignChallenge(other, nonce, "yard.example")
	forged.InstanceFingerprint = pinned
	moved := ch
	moved.Nonce = make([]byte, 32)
	moved.Nonce[0] = 1
	agentSigned := ch
	agentSigned.Signature = ed25519.Sign(instance, AuthPreimage("endpoint", nonce, "yard.example", Version))
	for name, c := range map[string]Challenge{"echoed fingerprint": echoed, "other key": forged, "other nonce": moved, "agent context": agentSigned} {
		if c.Proven("yard.example", pinned) {
			t.Fatalf("%s accepted", name)
		}
	}
	if ch.Proven("evil.example", pinned) {
		t.Fatal("a challenge for another host accepted")
	}
}
