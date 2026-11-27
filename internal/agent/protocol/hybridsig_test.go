package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"net/url"
	"strings"
	"testing"
)

func TestHybridKeyRoundTripAndValidation(t *testing.T) {
	pub, priv, err := GenerateHybridKey()
	if err != nil {
		t.Fatalf("GenerateHybridKey: %v", err)
	}
	if len(pub) != HybridPublicKeySize {
		t.Fatalf("pub len = %d, want %d", len(pub), HybridPublicKeySize)
	}
	if len(priv) != HybridPrivateKeySize {
		t.Fatalf("priv len = %d, want %d", len(priv), HybridPrivateKeySize)
	}
	if !ValidPublicKey(pub) {
		t.Fatal("ValidPublicKey rejected generated hybrid public key")
	}
	if !ValidPrivateKey(priv) {
		t.Fatal("ValidPrivateKey rejected generated hybrid private key")
	}
	derivedPub, err := PublicKeyFromPrivate(priv)
	if err != nil {
		t.Fatalf("PublicKeyFromPrivate: %v", err)
	}
	if !bytes.Equal(pub, derivedPub) {
		t.Fatal("PublicKeyFromPrivate did not match generated public key")
	}

	// Corrupt hybrid prefix must be rejected.
	badPriv := bytes.Clone(priv)
	badPriv[0] ^= 0xff
	if ValidPrivateKey(badPriv) {
		t.Fatal("ValidPrivateKey accepted corrupted magic prefix")
	}
	if _, err := PublicKeyFromPrivate(badPriv); err == nil {
		t.Fatal("PublicKeyFromPrivate accepted corrupted magic prefix")
	}

	// Ambiguous or truncated key sizes must be rejected.
	if ValidPublicKey(make([]byte, 48)) {
		t.Fatal("ValidPublicKey accepted 48-byte garbage")
	}
	if ValidPrivateKey(make([]byte, 48)) {
		t.Fatal("ValidPrivateKey accepted 48-byte garbage")
	}
}

func TestDeriveInstanceHybridKeyDeterministic(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	pub1, priv1, err := DeriveInstanceHybridKey(seed)
	if err != nil {
		t.Fatalf("DeriveInstanceHybridKey #1: %v", err)
	}
	pub2, priv2, err := DeriveInstanceHybridKey(seed)
	if err != nil {
		t.Fatalf("DeriveInstanceHybridKey #2: %v", err)
	}
	if !bytes.Equal(pub1, pub2) || !bytes.Equal(priv1, priv2) {
		t.Fatal("DeriveInstanceHybridKey is not deterministic")
	}
	if _, _, err := DeriveInstanceHybridKey(seed[:16]); err == nil {
		t.Fatal("DeriveInstanceHybridKey accepted short seed")
	}

	// Hybrid instance fingerprint must differ from legacy Ed25519 instance fingerprint.
	legacyFP := Fingerprint(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	hybridFP := Fingerprint(pub1)
	if legacyFP == hybridFP {
		t.Fatal("hybrid instance fingerprint collided with legacy Ed25519 fingerprint")
	}
}

func TestHybridChallengeProofAndNoDowngrade(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	_, _ = rand.Read(seed)
	_, hybridPriv, err := DeriveInstanceHybridKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	legacyPriv := ed25519.NewKeyFromSeed(seed)

	nonce := make([]byte, 32)
	agentNonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	_, _ = rand.Read(agentNonce)

	hch := SignChallenge(hybridPriv, nonce, agentNonce, "yard.example")
	if len(hch.InstanceKey) != HybridPublicKeySize {
		t.Fatalf("challenge instance key len = %d, want %d", len(hch.InstanceKey), HybridPublicKeySize)
	}
	if len(hch.Signature) != HybridSignatureSize {
		t.Fatalf("challenge signature len = %d, want %d", len(hch.Signature), HybridSignatureSize)
	}
	if !hch.Proven(agentNonce, "yard.example", hch.InstanceFingerprint) {
		t.Fatal("hybrid challenge failed Proven")
	}
	if hch.Proven(agentNonce, "evil.example", hch.InstanceFingerprint) {
		t.Fatal("hybrid challenge verified for wrong host")
	}

	// Downgrade attack: an agent pinning the hybrid fingerprint must refuse a legacy
	// Ed25519 challenge even when signed by the same underlying instance seed.
	lch := SignChallenge(legacyPriv, nonce, agentNonce, "yard.example")
	if !lch.Proven(agentNonce, "yard.example", lch.InstanceFingerprint) {
		t.Fatal("legacy Ed25519 challenge failed Proven for legacy pinned fingerprint")
	}
	if lch.Proven(agentNonce, "yard.example", hch.InstanceFingerprint) {
		t.Fatal("legacy Ed25519 challenge bypassed hybrid pinned fingerprint")
	}
	lchSpoof := lch
	lchSpoof.InstanceFingerprint = hch.InstanceFingerprint
	if lchSpoof.Proven(agentNonce, "yard.example", hch.InstanceFingerprint) {
		t.Fatal("spoofed fingerprint on legacy challenge bypassed hybrid pinned fingerprint")
	}
}

func TestHybridEnrollAuthAndCrossAlgorithmRotation(t *testing.T) {
	hPub, hPriv, err := GenerateHybridKey()
	if err != nil {
		t.Fatal(err)
	}
	token := make([]byte, 32)
	nonce := make([]byte, 32)
	_, _ = rand.Read(token)
	_, _ = rand.Read(nonce)

	// Enrollment proof.
	enrollSig, err := Sign(hPriv, ContextEnroll, Preimage(ContextEnroll, token))
	if err != nil {
		t.Fatalf("Sign enroll: %v", err)
	}
	if !VerifyEnrollment(hPub, token, enrollSig) {
		t.Fatal("VerifyEnrollment failed for hybridsig")
	}
	if VerifyEnrollment(hPub, nonce, enrollSig) {
		t.Fatal("VerifyEnrollment accepted wrong token")
	}

	// Auth proof (and cross-context rejection).
	authSig, err := Sign(hPriv, ContextAuth, AuthPreimage("ep_1", nonce, "yard.example", Version))
	if err != nil {
		t.Fatalf("Sign auth: %v", err)
	}
	if !VerifyAuth(hPub, "ep_1", nonce, "yard.example", Version, authSig) {
		t.Fatal("VerifyAuth failed for hybridsig")
	}
	if VerifyAuth(hPub, "ep_2", nonce, "yard.example", Version, authSig) {
		t.Fatal("VerifyAuth accepted wrong endpoint ID")
	}
	// Context separation: signing AuthPreimage under ContextEnroll must not verify as ContextAuth.
	wrongCtxSig, err := Sign(hPriv, ContextEnroll, AuthPreimage("ep_1", nonce, "yard.example", Version))
	if err != nil {
		t.Fatal(err)
	}
	if VerifyAuth(hPub, "ep_1", nonce, "yard.example", Version, wrongCtxSig) {
		t.Fatal("VerifyAuth accepted signature made under ContextEnroll")
	}

	// Legacy Ed25519 key rotating forward to a hybridsig key.
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rotSig, err := Sign(edPriv, ContextRotate, Preimage(ContextRotate, RawFingerprint(edPub), hPub))
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyRotation(edPub, RawFingerprint(edPub), hPub, rotSig) {
		t.Fatal("VerifyRotation failed for legacy Ed25519 -> hybridsig upgrade")
	}

	// Hybridsig key rotating to a second hybridsig key.
	hPub2, _, err := GenerateHybridKey()
	if err != nil {
		t.Fatal(err)
	}
	rotSig2, err := Sign(hPriv, ContextRotate, Preimage(ContextRotate, RawFingerprint(hPub), hPub2))
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyRotation(hPub, RawFingerprint(hPub), hPub2, rotSig2) {
		t.Fatal("VerifyRotation failed for hybridsig -> hybridsig rotation")
	}
}

func TestInstanceFPParamParsing(t *testing.T) {
	if fp, err := InstanceFP(url.Values{}); fp != "" || err != nil {
		t.Fatalf("empty query = (%q, %v), want (\"\", nil)", fp, err)
	}
	good := strings.Repeat("a", 64)
	if fp, err := InstanceFP(url.Values{InstanceFPParam: {good}}); fp != good || err != nil {
		t.Fatalf("valid query = (%q, %v), want (%q, nil)", fp, err, good)
	}
	for _, bad := range [][]string{
		{""},
		{"short"},
		{strings.Repeat("A", 64)},
		{strings.Repeat("g", 64)},
		{good, good},
	} {
		if fp, err := InstanceFP(url.Values{InstanceFPParam: bad}); fp != "" || err == nil {
			t.Fatalf("InstanceFP(%q) = (%q, %v), want error", bad, fp, err)
		}
	}
}
