package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

func TestAgentHybridEnrollConnectAndRotate(t *testing.T) {
	s, st, cfg := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}); err != nil {
		t.Fatal(err)
	}
	admin := loginAs(t, s, st, "envadmin", "user")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()

	// Mint enrollment token and enroll via the real agent client (which mints a hybridsig key).
	w := tenantRequest(s, admin, "POST", "/api/organizations/a/environments/env-a/enrollment-tokens", `{"runtime":"docker"}`, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint token: %d %s", w.Code, w.Body.String())
	}
	var minted struct{ Token string }
	_ = json.Unmarshal(w.Body.Bytes(), &minted)

	id, err := client.Enroll(ctx, httpSrv.Client(), httpSrv.URL, client.DirStore(t.TempDir()), "hybrid-host", minted.Token, map[string]string{"hostname": "hybrid-host"})
	if err != nil {
		t.Fatalf("client.Enroll: %v", err)
	}
	if len(id.PrivateKey) != protocol.HybridPrivateKeySize {
		t.Fatalf("enrolled private key len = %d, want %d (hybridsig)", len(id.PrivateKey), protocol.HybridPrivateKeySize)
	}
	wantHybridInstPub, _, err := protocol.DeriveInstanceHybridKey(cfg.Security.InstanceKey)
	if err != nil {
		t.Fatal(err)
	}
	if id.InstanceFingerprint != protocol.Fingerprint(wantHybridInstPub) {
		t.Fatalf("instance fingerprint = %q, want hybrid %q", id.InstanceFingerprint, protocol.Fingerprint(wantHybridInstPub))
	}

	pub, err := protocol.PublicKeyFromPrivate(id.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	// Approve the enrolled endpoint.
	w = tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+id.EndpointID+"/approve", `{"fingerprint":"`+protocol.Fingerprint(pub)+`"}`, true)
	if w.Code != http.StatusNoContent {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}

	// Handshake with hybridsig instance challenge and hybridsig agent auth frame.
	u, _ := url.Parse(httpSrv.URL)
	agentNonce := make([]byte, 32)
	_, _ = rand.Read(agentNonce)
	q := url.Values{
		protocol.AgentNonceParam: {base64.RawURLEncoding.EncodeToString(agentNonce)},
		protocol.InstanceFPParam: {id.InstanceFingerprint},
	}
	conn, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/agent/v1/connect?"+q.Encode(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)

	chEnv := readEnvelope(t, ctx, conn)
	var ch protocol.Challenge
	if err := json.Unmarshal(chEnv.Payload, &ch); err != nil {
		t.Fatal(err)
	}
	if len(ch.InstanceKey) != protocol.HybridPublicKeySize || len(ch.Signature) != protocol.HybridSignatureSize {
		t.Fatalf("server challenge not hybridsig: key=%d sig=%d", len(ch.InstanceKey), len(ch.Signature))
	}
	if !ch.Proven(agentNonce, u.Host, id.InstanceFingerprint) {
		t.Fatal("server hybrid challenge failed Proven")
	}

	authSig, err := protocol.Sign(id.PrivateKey, protocol.ContextAuth, protocol.AuthPreimage(id.EndpointID, ch.Nonce, u.Host, protocol.Version))
	if err != nil {
		t.Fatal(err)
	}
	writeEnvelope(t, ctx, conn, protocol.TypeAuth, protocol.Auth{
		EndpointID:  id.EndpointID,
		Fingerprint: protocol.Fingerprint(pub),
		Version:     protocol.Version,
		Signature:   authSig,
	})
	helloEnv := readEnvelope(t, ctx, conn)
	if helloEnv.Type != protocol.TypeHello {
		t.Fatalf("after auth got %q, want %q", helloEnv.Type, protocol.TypeHello)
	}

	// Offer a hybridsig -> hybridsig key rotation over the live socket.
	newPub, _, err := protocol.GenerateHybridKey()
	if err != nil {
		t.Fatal(err)
	}
	rotSig, err := protocol.Sign(id.PrivateKey, protocol.ContextRotate, protocol.Preimage(protocol.ContextRotate, protocol.RawFingerprint(pub), newPub))
	if err != nil {
		t.Fatal(err)
	}
	writeEnvelope(t, ctx, conn, protocol.TypeRotate, protocol.Rotate{PublicKey: newPub, Signature: rotSig})
	rotEnv := readEnvelope(t, ctx, conn)
	var rotAck protocol.Rotated
	if err := json.Unmarshal(rotEnv.Payload, &rotAck); err != nil {
		t.Fatal(err)
	}
	if rotAck.Code != "" || rotAck.Fingerprint != protocol.Fingerprint(newPub) {
		t.Fatalf("rotation ack = %+v, want empty code and fp %s", rotAck, protocol.Fingerprint(newPub))
	}
}

func TestAgentLegacyEd25519CompatAndUpgradeRotation(t *testing.T) {
	s, st, cfg := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}); err != nil {
		t.Fatal(err)
	}
	admin := loginAs(t, s, st, "envadmin", "user")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()

	// Enroll a legacy Ed25519 agent (simulating a pre-upgrade host).
	ag := enrollAgent(t, s, st, admin, "legacy-host")
	w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true)
	if w.Code != http.StatusNoContent {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}

	// Malformed instance_fp must be rejected with HTTP 400 before WebSocket upgrade.
	u, _ := url.Parse(httpSrv.URL)
	agentNonce := make([]byte, 32)
	_, _ = rand.Read(agentNonce)
	badQ := url.Values{
		protocol.AgentNonceParam: {base64.RawURLEncoding.EncodeToString(agentNonce)},
		protocol.InstanceFPParam: {"not-a-hex-fingerprint"},
	}
	if _, resp, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/agent/v1/connect?"+badQ.Encode(), nil); err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed instance_fp: err=%v resp=%v, want 400", err, resp)
	}

	// Legacy agent pinning the pre-upgrade Ed25519 instance fingerprint dials with instance_fp.
	legacyInstFP := protocol.Fingerprint(ed25519.NewKeyFromSeed(cfg.Security.InstanceKey).Public().(ed25519.PublicKey))
	goodQ := url.Values{
		protocol.AgentNonceParam: {base64.RawURLEncoding.EncodeToString(agentNonce)},
		protocol.InstanceFPParam: {legacyInstFP},
	}
	conn, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/agent/v1/connect?"+goodQ.Encode(), nil)
	if err != nil {
		t.Fatalf("legacy dial: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)

	chEnv := readEnvelope(t, ctx, conn)
	var ch protocol.Challenge
	if err := json.Unmarshal(chEnv.Payload, &ch); err != nil {
		t.Fatal(err)
	}
	if len(ch.InstanceKey) != ed25519.PublicKeySize || ch.InstanceFingerprint != legacyInstFP {
		t.Fatalf("expected legacy Ed25519 challenge for legacy instance_fp, got key=%d fp=%q", len(ch.InstanceKey), ch.InstanceFingerprint)
	}
	if !ch.Proven(agentNonce, u.Host, legacyInstFP) {
		t.Fatal("legacy Ed25519 server challenge failed Proven")
	}

	// Authenticate with legacy Ed25519 agent key.
	sig := ed25519.Sign(ag.priv, protocol.AuthPreimage(ag.id, ch.Nonce, u.Host, protocol.Version))
	writeEnvelope(t, ctx, conn, protocol.TypeAuth, protocol.Auth{
		EndpointID:  ag.id,
		Fingerprint: ag.fp,
		Version:     protocol.Version,
		Signature:   sig,
	})
	helloEnv := readEnvelope(t, ctx, conn)
	if helloEnv.Type != protocol.TypeHello {
		t.Fatalf("after legacy auth got %q, want %q", helloEnv.Type, protocol.TypeHello)
	}

	// Rotate forward from legacy Ed25519 agent key to a hybridsig agent key.
	newHybridPub, _, err := protocol.GenerateHybridKey()
	if err != nil {
		t.Fatal(err)
	}
	rotSig := ed25519.Sign(ag.priv, protocol.Preimage(protocol.ContextRotate, protocol.RawFingerprint(ag.priv.Public().(ed25519.PublicKey)), newHybridPub))
	writeEnvelope(t, ctx, conn, protocol.TypeRotate, protocol.Rotate{PublicKey: newHybridPub, Signature: rotSig})
	rotEnv := readEnvelope(t, ctx, conn)
	var rotAck protocol.Rotated
	if err := json.Unmarshal(rotEnv.Payload, &rotAck); err != nil {
		t.Fatal(err)
	}
	if rotAck.Code != "" || rotAck.Fingerprint != protocol.Fingerprint(newHybridPub) {
		t.Fatalf("legacy->hybrid rotation ack = %+v, want empty code and fp %s", rotAck, protocol.Fingerprint(newHybridPub))
	}
}

func TestLocalDockerHybridAndLegacyCompat(t *testing.T) {
	// Fresh DB initializes ep_local_docker with a hybridsig public key.
	_, stFresh, cfgFresh := setupTestServer(t)
	ctx := context.Background()
	hybridPub, _, err := protocol.DeriveHybridKey(cfgFresh.Security.InstanceKey, "kyyard/local-docker-agent/v2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stFresh.Tenancy().InitializeLocalDocker(ctx, hybridPub); err != nil {
		t.Fatalf("fresh InitializeLocalDocker(hybrid): %v", err)
	}
	if _, err := stFresh.Tenancy().InitializeLocalDocker(ctx, hybridPub); err != nil {
		t.Fatalf("repeat InitializeLocalDocker(hybrid): %v", err)
	}

	// Pre-upgrade DB already initialized with legacy Ed25519 key refuses hybrid key
	// (no silent key swap) and continues accepting the approved legacy Ed25519 key.
	_, stLegacy, cfgLegacy := setupTestServer(t)
	mac := hmac.New(sha256.New, cfgLegacy.Security.InstanceKey)
	_, _ = mac.Write([]byte("kyyard/local-docker-agent/v1"))
	legacyKey := ed25519.NewKeyFromSeed(mac.Sum(nil))
	legacyPub := legacyKey.Public().(ed25519.PublicKey)
	if _, err := stLegacy.Tenancy().InitializeLocalDocker(ctx, legacyPub); err != nil {
		t.Fatalf("pre-upgrade InitializeLocalDocker(legacy): %v", err)
	}
	legacyHybridPub, _, err := protocol.DeriveHybridKey(cfgLegacy.Security.InstanceKey, "kyyard/local-docker-agent/v2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stLegacy.Tenancy().InitializeLocalDocker(ctx, legacyHybridPub); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("InitializeLocalDocker(hybrid) on legacy DB = %v, want ErrForbidden", err)
	}
	if _, err := stLegacy.Tenancy().InitializeLocalDocker(ctx, legacyPub); err != nil {
		t.Fatalf("fallback InitializeLocalDocker(legacy) = %v, want nil", err)
	}
}
