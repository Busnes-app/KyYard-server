package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

type enrolledAgent struct {
	id   string
	priv ed25519.PrivateKey
	fp   string
	inst string
}

// enrollAgent mints a token as the environment admin and redeems it through the real route.
func enrollAgent(t *testing.T, s *api.Server, st store.Store, admin *http.Cookie, name string) enrolledAgent {
	t.Helper()
	w := tenantRequest(s, admin, "POST", "/api/organizations/a/environments/env-a/enrollment-tokens", `{"runtime":"docker"}`, true)
	if w.Code != 201 {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	var minted struct{ Token string }
	_ = json.Unmarshal(w.Body.Bytes(), &minted)
	token, _ := base64.RawURLEncoding.DecodeString(minted.Token)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body, _ := json.Marshal(map[string]any{"token": minted.Token, "public_key": base64.RawURLEncoding.EncodeToString(pub), "proof": base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, token))), "name": name, "facts": map[string]string{"hostname": name}})
	r := httptest.NewRequest("POST", "/api/agent/v1/enroll", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 201 {
		t.Fatalf("enroll: %d %s", rec.Code, rec.Body.String())
	}
	var reply struct {
		ID   string `json:"endpoint_id"`
		FP   string `json:"fingerprint"`
		Inst string `json:"instance_fingerprint"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	return enrolledAgent{id: reply.ID, priv: priv, fp: reply.FP, inst: reply.Inst}
}

type agentSocket struct {
	conn  *websocket.Conn
	hello protocol.Hello
}

func readEnvelope(t *testing.T, ctx context.Context, c *websocket.Conn) protocol.Envelope {
	t.Helper()
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var e protocol.Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func writeEnvelope(t *testing.T, ctx context.Context, c *websocket.Conn, typ string, payload any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	frame, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: typ, Payload: raw})
	if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write %s: %v", typ, err)
	}
}

// connect performs the handshake; on refusal it returns the close reason and a nil socket.
func connect(t *testing.T, ctx context.Context, base string, ag enrolledAgent, priv ed25519.PrivateKey, version int) (*agentSocket, string) {
	t.Helper()
	u, _ := url.Parse(base)
	c, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/agent/v1/connect", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.SetReadLimit(4 << 20)
	ch := readEnvelope(t, ctx, c)
	var challenge protocol.Challenge
	_ = json.Unmarshal(ch.Payload, &challenge)
	if ch.Type != protocol.TypeChallenge || challenge.InstanceFingerprint != ag.inst || len(challenge.Nonce) != 32 {
		t.Fatalf("challenge: %+v", ch)
	}
	sig := ed25519.Sign(priv, protocol.AuthPreimage(ag.id, challenge.Nonce, u.Host, version))
	writeEnvelope(t, ctx, c, protocol.TypeAuth, protocol.Auth{EndpointID: ag.id, Version: version, Signature: sig})
	_, raw, err := c.Read(ctx)
	if err != nil {
		var ce websocket.CloseError
		if errorsAs(err, &ce) {
			return nil, ce.Reason
		}
		t.Fatalf("after auth: %v", err)
	}
	var hello protocol.Envelope
	_ = json.Unmarshal(raw, &hello)
	var h protocol.Hello
	_ = json.Unmarshal(hello.Payload, &h)
	return &agentSocket{conn: c, hello: h}, ""
}

func errorsAs(err error, target *websocket.CloseError) bool {
	for err != nil {
		if ce, ok := err.(websocket.CloseError); ok {
			*target = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestAgentConnectionLifecycle(t *testing.T) {
	s, st, _ := setupTestServer(t)
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
	ag := enrollAgent(t, s, st, admin, "host-1")

	// Wrong key: refused without detail. Wrong version: named. Unknown endpoint: same as revoked.
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	if _, reason := connect(t, ctx, httpSrv.URL, ag, wrongPriv, protocol.Version); reason != protocol.CloseRevoked {
		t.Fatalf("wrong key: %q", reason)
	}
	if _, reason := connect(t, ctx, httpSrv.URL, ag, ag.priv, 99); reason != protocol.CloseIncompatible {
		t.Fatalf("wrong version: %q", reason)
	}
	ghost := ag
	ghost.id = "ep_doesnotexist"
	if _, reason := connect(t, ctx, httpSrv.URL, ghost, ag.priv, protocol.Version); reason != protocol.CloseRevoked {
		t.Fatalf("unknown endpoint: %q", reason)
	}

	// Pending: restricted channel, receives the approval notice, inventory is ignored.
	sock, reason := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	if reason != "" || sock.hello.State != "pending" || sock.hello.HeartbeatSeconds != 30 {
		t.Fatalf("pending hello: %q %+v", reason, sock)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Inventory{Generation: 5})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("heartbeat ack: %+v", e)
	}
	if e, err := ts.ReadEndpointRaw(ctx, ag.id); err != nil || e.State != "pending" {
		t.Fatalf("pending endpoint moved by inventory: %+v", e)
	}
	// A second socket for the same endpoint is refused while the first is live.
	if _, reason := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version); reason != protocol.CloseDuplicate {
		t.Fatalf("duplicate: %q", reason)
	}
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeApproved {
		t.Fatalf("approval notice: %+v", e)
	}
	sock.conn.Close(websocket.StatusNormalClosure, "approved")
	waitFor(t, func() bool { return !s.Connected(ag.id) })

	// Approved: first accepted inventory makes it active; stale generations are ignored.
	sock, reason = connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	if reason != "" || sock.hello.State != "approved" {
		t.Fatalf("approved hello: %q %+v", reason, sock.hello)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Inventory{Generation: 10})
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Inventory{Generation: 9})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	readEnvelope(t, ctx, sock.conn)
	if e, _ := ts.ReadEndpointRaw(ctx, ag.id); e.LastSeenAt == nil {
		t.Fatal("heartbeat did not record last_seen_at")
	}
	writeEnvelope(t, ctx, sock.conn, "nonsense", nil)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeError || !strings.Contains(string(e.Payload), "unsupported_type") {
		t.Fatalf("unknown type: %+v", e)
	}
	// Disconnect marks it offline; reconnect with a newer generation returns it to active.
	sock.conn.Close(websocket.StatusNormalClosure, "bye")
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "offline" })
	sock, _ = connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	if sock.hello.State != "offline" {
		t.Fatalf("offline hello: %+v", sock.hello)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Inventory{Generation: 11})
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })

	// Revocation closes the live socket in the same request and the next connect is refused.
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/revoke", "", true); w.Code != 204 {
		t.Fatalf("revoke: %d", w.Code)
	}
	_, _, err := sock.conn.Read(ctx)
	var ce websocket.CloseError
	if !errorsAs(err, &ce) || ce.Reason != protocol.CloseRevoked {
		t.Fatalf("revocation did not close the socket: %v", err)
	}
	if _, reason := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version); reason != protocol.CloseRevoked {
		t.Fatalf("revoked reconnect: %q", reason)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var ok, denied int
	for _, r := range records {
		if r.Action == "agent.connect" && r.UserID == "agent:"+ag.id {
			if r.Result == "success" {
				ok++
			} else if r.Result == "denied" {
				denied++
			}
		}
	}
	if ok < 3 || denied < 3 {
		t.Fatalf("connect audit: success=%d denied=%d", ok, denied)
	}
}

func TestAgentShutdownClosesSockets(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"})
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-2")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	sock, _ := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Inventory{Generation: 1})
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })
	s.BeginShutdown()
	_, _, err := sock.conn.Read(ctx)
	var ce websocket.CloseError
	if !errorsAs(err, &ce) || ce.Reason != protocol.CloseShutdown {
		t.Fatalf("shutdown did not close the socket: %v", err)
	}
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "offline" })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// An unauthenticated client may not make the server buffer a large frame.
func TestAgentConnectLimitsFramesBeforeAuth(t *testing.T) {
	s, _, _ := setupTestServer(t)
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, _ := url.Parse(httpSrv.URL)
	c, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/agent/v1/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	readEnvelope(t, ctx, c)
	big := make([]byte, 64<<10)
	_ = c.Write(ctx, websocket.MessageText, big)
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("server answered an oversized pre-auth frame instead of closing")
	}
}
