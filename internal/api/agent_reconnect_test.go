package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/config"
	"github.com/Busness-app/kyyard-server/internal/store"
	"github.com/Busness-app/kyyard-server/internal/testdb"
	"github.com/coder/websocket"
)

// A socket the network dropped silently still holds the endpoint's slot; a reconnect must
// evict it, not be refused as a duplicate and raise the copied-volume alarm.
func TestReconnectAfterSilentDropIsNotADuplicate(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dbCfg := testdb.Config(t)
	dbCfg.DataDir = cfg.Database.DataDir
	cfg.Database = dbCfg
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer(cfg, st)
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	_ = st.Users().CreateUser(ctx, &store.User{ID: "admin", Username: "admin", Role: "user", SSOProvider: "local", Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "admin", Role: store.RoleOrganizationAdmin, Status: "active"})
	tok, err := ts.CreateEnrollmentToken(ctx, store.TenantAccess{ActorID: "admin", OrganizationID: "a", EnvironmentID: "env-a"}, "docker", "")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	e, err := ts.Enroll(ctx, store.EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "host"})
	if err != nil {
		t.Fatal(err)
	}
	org := store.TenantAccess{ActorID: "admin", OrganizationID: "a"}
	if err := ts.ApproveEndpoint(ctx, org, e.ID, e.Fingerprint); err != nil {
		t.Fatal(err)
	}

	// The incumbent: a websocket whose peer vanished without a close frame.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Kill the TCP connection underneath without a close handshake.
		hj, ok := w.(http.Hijacker)
		if ok {
			if conn, _, err := hj.Hijack(); err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
				conn.Close()
			}
		}
		_ = c
	}))
	defer dead.Close()
	incumbentConn, _, err := websocket.Dial(ctx, "ws"+dead.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	incumbent := &agentConn{endpointID: e.ID, fingerprint: e.Fingerprint, ip: "10.0.0.1", conn: incumbentConn, send: make(chan protocol.Envelope, 8), closed: make(chan struct{})}
	if s.agents.add(incumbent) != nil {
		t.Fatal("registry not empty")
	}

	// The newcomer performs the real handshake against the server.
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	u, _ := url.Parse(httpSrv.URL)
	c, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/agent/v1/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var env protocol.Envelope
	_ = json.Unmarshal(raw, &env)
	var ch protocol.Challenge
	_ = json.Unmarshal(env.Payload, &ch)
	sig := ed25519.Sign(priv, protocol.AuthPreimage(e.ID, ch.Nonce, u.Host, protocol.Version))
	authRaw, _ := json.Marshal(protocol.Auth{EndpointID: e.ID, Fingerprint: e.Fingerprint, Version: protocol.Version, Signature: sig})
	frame, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: protocol.TypeAuth, Payload: authRaw})
	if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	_, raw, err = c.Read(ctx)
	if err != nil {
		t.Fatalf("reconnect after a silent drop was refused: %v", err)
	}
	_ = json.Unmarshal(raw, &env)
	if env.Type != protocol.TypeHello {
		t.Fatalf("expected hello, got %+v", env)
	}
	view, err := ts.ReadEndpoint(ctx, org, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, al := range view.Alerts {
		if al.Kind == "duplicate_connection" {
			t.Fatalf("silent drop reconnect raised the duplicate alarm: %+v", view.Alerts)
		}
	}
	if !s.Connected(e.ID) {
		t.Fatal("newcomer not registered")
	}
}
