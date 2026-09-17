package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/config"
	"github.com/Busness-app/kyyard-server/internal/store"
	"github.com/Busness-app/kyyard-server/internal/testdb"
	"github.com/coder/websocket"
)

// A socket the network dropped silently still holds the endpoint's slot; a reconnect must
// evict it (audited) rather than be refused as a duplicate and raise the copied-volume
// alarm. A socket that merely cannot answer a ping because its loop is busy with a frame is
// not dead and must be kept.
func TestReconnectAfterSilentDropIsNotADuplicate(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dbCfg := testdb.Config(t)
	dbCfg.DataDir = cfg.Database.DataDir
	cfg.Database = dbCfg
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	// The incumbent: a websocket whose peer vanished without a close frame, so it can never
	// answer a ping.
	incumbent := &agentConn{endpointID: e.ID, fingerprint: e.Fingerprint, ip: "10.0.0.1", conn: deadPeer(t, ctx), send: make(chan protocol.Envelope, 8), closed: make(chan struct{})}
	if s.agents.add(incumbent) != nil {
		t.Fatal("registry not empty")
	}
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	u, _ := url.Parse(httpSrv.URL)

	// Busy, not gone: a frame just landed, so the loop is inside a handler and the reader
	// cannot answer the probe. The newcomer is the duplicate.
	incumbent.lastFrame.Store(time.Now().UnixNano())
	busy, _, err := dialAgent(t, ctx, u.Host, e, priv)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Reason != protocol.CloseDuplicate {
		t.Fatalf("busy incumbent: expected %s refusal, got %+v %v", protocol.CloseDuplicate, busy, err)
	}
	select {
	case <-incumbent.closed:
		t.Fatal("busy incumbent was evicted")
	default:
	}
	if err := ts.AcknowledgeEndpointEvent(ctx, org, e.ID, latestAlert(t, ctx, ts, org, e.ID, "duplicate_connection")); err != nil {
		t.Fatal(err)
	}

	// Silent: the last frame is old and the ping goes unanswered. The newcomer is admitted,
	// the eviction is audited against the evicted address, and no alarm is raised.
	incumbent.lastFrame.Store(0)
	env, c, err := dialAgent(t, ctx, u.Host, e, priv)
	if err != nil {
		t.Fatalf("reconnect after a silent drop was refused: %v", err)
	}
	defer c.CloseNow()
	if env.Type != protocol.TypeHello {
		t.Fatalf("expected hello, got %+v", env)
	}
	select {
	case <-incumbent.closed:
	default:
		t.Fatal("dead incumbent was not evicted")
	}
	view, err := ts.ReadEndpoint(ctx, org, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Alerts) != 0 {
		t.Fatalf("silent drop reconnect raised an alarm: %+v", view.Alerts)
	}
	if !s.Connected(e.ID) {
		t.Fatal("newcomer not registered")
	}
	records, err := ts.ReadAudit(ctx, org, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	evicted := false
	for _, r := range records {
		if r.Action == "agent.connect" && r.Result == "failure" && r.IPAddress == "10.0.0.1" && strings.HasPrefix(r.Details, "evicted: ") {
			evicted = true
		}
	}
	if !evicted {
		t.Fatalf("eviction not audited: %+v", records)
	}
}

// deadPeer dials a websocket whose server side kills the TCP connection without a close
// handshake, so pings are never answered.
func deadPeer(t *testing.T, ctx context.Context) *websocket.Conn {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := websocket.Accept(w, r, nil); err != nil {
			return
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
				conn.Close()
			}
		}
	}))
	t.Cleanup(dead.Close)
	c, _, err := websocket.Dial(ctx, "ws"+dead.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// dialAgent runs the real handshake for an approved endpoint and returns the first frame after
// auth, or the error that ended the socket.
func dialAgent(t *testing.T, ctx context.Context, host string, e *store.Endpoint, priv ed25519.PrivateKey) (protocol.Envelope, *websocket.Conn, error) {
	t.Helper()
	c, _, err := websocket.Dial(ctx, "ws://"+host+"/api/agent/v1/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env protocol.Envelope
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &env)
	var ch protocol.Challenge
	_ = json.Unmarshal(env.Payload, &ch)
	sig := ed25519.Sign(priv, protocol.AuthPreimage(e.ID, ch.Nonce, host, protocol.Version))
	authRaw, _ := json.Marshal(protocol.Auth{EndpointID: e.ID, Fingerprint: e.Fingerprint, Version: protocol.Version, Signature: sig})
	frame, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: protocol.TypeAuth, Payload: authRaw})
	if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	_, raw, err = c.Read(ctx)
	if err != nil {
		c.CloseNow()
		return env, nil, err
	}
	_ = json.Unmarshal(raw, &env)
	return env, c, nil
}

func latestAlert(t *testing.T, ctx context.Context, ts store.TenancyStore, org store.TenantAccess, endpointID, kind string) int64 {
	t.Helper()
	view, err := ts.ReadEndpoint(ctx, org, endpointID)
	if err != nil {
		t.Fatal(err)
	}
	for _, al := range view.Alerts {
		if al.Kind == kind {
			return al.ID
		}
	}
	t.Fatalf("no %s alert: %+v", kind, view.Alerts)
	return 0
}
