package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"net/url"
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
// key, another host, another nonce, another agent nonce (a replay) or an agent-context signature
// does not.
func TestChallengeProvesTheInstanceKey(t *testing.T) {
	_, instance, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	nonce, agentNonce := make([]byte, 32), make([]byte, 32)
	agentNonce[0] = 7
	ch := SignChallenge(instance, nonce, agentNonce, "yard.example")
	pinned := ch.InstanceFingerprint
	if !ch.Proven(agentNonce, "yard.example", pinned) {
		t.Fatal("a genuine challenge was refused")
	}
	echoed := Challenge{Nonce: nonce, InstanceFingerprint: pinned, Versions: []int{Version}}
	forged := SignChallenge(other, nonce, agentNonce, "yard.example")
	forged.InstanceFingerprint = pinned
	moved := ch
	moved.Nonce = make([]byte, 32)
	moved.Nonce[0] = 1
	agentSigned := ch
	agentSigned.Signature = ed25519.Sign(instance, AuthPreimage("endpoint", nonce, "yard.example", Version))
	for name, c := range map[string]Challenge{"echoed fingerprint": echoed, "other key": forged, "other nonce": moved, "agent context": agentSigned} {
		if c.Proven(agentNonce, "yard.example", pinned) {
			t.Fatalf("%s accepted", name)
		}
	}
	if ch.Proven(agentNonce, "evil.example", pinned) {
		t.Fatal("a challenge for another host accepted")
	}
	if ch.Proven(make([]byte, 32), "yard.example", pinned) || ch.Proven(nil, "yard.example", pinned) {
		t.Fatal("a challenge for another agent nonce accepted")
	}
}

func TestAgentNonceIsExactlyOne32ByteValue(t *testing.T) {
	good := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if n, err := AgentNonce(url.Values{}); n != nil || err != nil {
		t.Fatalf("absent: %v %v", n, err)
	}
	if n, err := AgentNonce(url.Values{AgentNonceParam: {good}}); len(n) != 32 || err != nil {
		t.Fatalf("good: %v %v", n, err)
	}
	for name, v := range map[string][]string{
		"empty":    {""},
		"short":    {base64.RawURLEncoding.EncodeToString(make([]byte, 31))},
		"long":     {base64.RawURLEncoding.EncodeToString(make([]byte, 33))},
		"padded":   {base64.URLEncoding.EncodeToString(make([]byte, 32))},
		"std":      {base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 32))},
		"repeated": {good, good},
	} {
		if n, err := AgentNonce(url.Values{AgentNonceParam: v}); n != nil || err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
