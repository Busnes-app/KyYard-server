package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
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
	ts := st.Tenancy()
	flaky := &flakySettle{TenancyStore: ts}
	s := NewServer(cfg, flakyStore{Store: st, tenancy: flaky})
	defer s.stopping.Store(true)
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
	busy, _, err := dialAgent(ctx, u.Host, e, priv)
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

	// A reader attached to the incumbent. The successor knows nothing of this stream, so
	// nothing will ever arrive on it again: displacing the socket must end it rather than
	// leave the reader waiting, holding its place on the endpoint, for an hour.
	orphan, refusal := s.logs.open(e.ID, "admin", "org")
	if refusal != "" {
		t.Fatalf("opening a stream: %s", refusal)
	}
	defer s.logs.release(orphan)

	// A command the incumbent took and never answered. The successor never hears of it, so the
	// incumbent's end must settle it, even though a successor holds the endpoint by then and
	// whether or not the dispatch mark was written (it is not, here).
	if ok, err := ts.AcceptInventory(ctx, e.ID, uint64(time.Now().Unix()), time.Now(), []byte(`{"engine":{"version":"27.0"}}`)); err != nil || !ok {
		t.Fatalf("inventory: %v %v", ok, err)
	}
	carried, err := ts.CreateCommand(ctx, org, e.ID, protocol.ActionImagePull, "docker.io/library/alpine:3.24", "", protocol.Expectation{})
	if err != nil {
		t.Fatal(err)
	}
	if !s.agents.deliverCommand(e.ID, carried.ID, envelope(protocol.TypeCommand, protocol.Command{ID: carried.ID})) {
		t.Fatal("the incumbent did not take the command")
	}

	// Silent: the last frame is old and the ping goes unanswered. The newcomer is admitted,
	// the eviction is audited against the evicted address, and no alarm is raised.
	incumbent.lastFrame.Store(0)
	env, c, err := dialAgent(ctx, u.Host, e, priv)
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
	select {
	case <-orphan.done:
		if end := orphan.end.Load(); end == nil || end.Reason != "the endpoint reconnected" {
			t.Fatalf("the displaced socket's reader was told %+v", end)
		}
	default:
		t.Fatal("a reader of the displaced socket was left waiting for chunks nobody will send")
	}
	// A reader that arrives after the successor is admitted belongs to the live session and
	// survives the loser's unwind.
	fresh, refusal := s.logs.open(e.ID, "another-reader", "org")
	if refusal != "" {
		t.Fatalf("opening a stream on the successor: %s", refusal)
	}
	defer s.logs.release(fresh)
	// A command the successor carries is the live session's and survives the loser's unwind.
	live, err := ts.CreateCommand(ctx, org, e.ID, protocol.ActionImagePull, "docker.io/library/alpine:3.23", "", protocol.Expectation{})
	if err != nil {
		t.Fatal(err)
	}
	if !s.agents.deliverCommand(e.ID, live.ID, envelope(protocol.TypeCommand, protocol.Command{ID: live.ID})) {
		t.Fatal("the successor did not take the command")
	}
	s.markOffline(ctx, incumbent)
	if got, err := ts.ReadCommand(ctx, org, e.ID, carried.ID); err != nil || got.Outcome != protocol.OutcomeUnknown {
		t.Fatalf("the evicted socket's command was left in flight: %+v %v", got, err)
	}
	if got, err := ts.ReadCommand(ctx, org, e.ID, live.ID); err != nil || got.Outcome != "" {
		t.Fatalf("the successor's command was settled by the loser's unwind: %+v %v", got, err)
	}
	select {
	case <-fresh.done:
		t.Fatal("the displaced socket's unwind closed the successor's reader")
	default:
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
// auth, or the error that ended the socket. It never fails the test itself, so it is safe off
// the test goroutine; a non-nil conn is the caller's to close.
func dialAgent(ctx context.Context, host string, e *store.Endpoint, priv ed25519.PrivateKey) (protocol.Envelope, *websocket.Conn, error) {
	var env protocol.Envelope
	c, _, err := websocket.Dial(ctx, "ws://"+host+"/api/agent/v1/connect", nil)
	if err != nil {
		return env, nil, fmt.Errorf("dial: %w", err)
	}
	_, raw, err := c.Read(ctx)
	if err != nil {
		c.CloseNow()
		return env, nil, fmt.Errorf("challenge: %w", err)
	}
	_ = json.Unmarshal(raw, &env)
	var ch protocol.Challenge
	_ = json.Unmarshal(env.Payload, &ch)
	sig := ed25519.Sign(priv, protocol.AuthPreimage(e.ID, ch.Nonce, host, protocol.Version))
	authRaw, _ := json.Marshal(protocol.Auth{EndpointID: e.ID, Fingerprint: e.Fingerprint, Version: protocol.Version, Signature: sig})
	frame, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: protocol.TypeAuth, Payload: authRaw})
	if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
		c.CloseNow()
		return env, nil, fmt.Errorf("auth: %w", err)
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

// Losers of the slot race all reach the event recorder in parallel; the partial unique index,
// not a prior read, keeps the operator's alert list to one duplicate_connection.
func TestDuplicateConnectionEventIsRecordedOnce(t *testing.T) {
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
	ts := st.Tenancy()
	flaky := &flakySettle{TenancyStore: ts}
	s := NewServer(cfg, flakyStore{Store: st, tenancy: flaky})
	defer s.stopping.Store(true)
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
	incumbent := &agentConn{endpointID: e.ID, fingerprint: e.Fingerprint, ip: "10.0.0.1", conn: deadPeer(t, ctx), send: make(chan protocol.Envelope, 8), closed: make(chan struct{})}
	incumbent.lastFrame.Store(time.Now().UnixNano()) // live by the proof-of-life rule
	if s.agents.add(incumbent) != nil {
		t.Fatal("registry not empty")
	}
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	u, _ := url.Parse(httpSrv.URL)
	const n = 8
	refused := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, _, err := dialAgent(ctx, u.Host, e, priv)
			refused <- err
		}()
	}
	for i := 0; i < n; i++ {
		var ce websocket.CloseError
		select {
		case err := <-refused:
			if !errors.As(err, &ce) || ce.Reason != protocol.CloseDuplicate {
				t.Fatalf("competitor %d not refused as a duplicate: %v", i, err)
			}
		case <-ctx.Done():
			t.Fatalf("only %d of %d competitors answered before the deadline", i, n)
		}
	}
	view, err := ts.ReadEndpoint(ctx, org, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, al := range view.Alerts {
		if al.Kind == "duplicate_connection" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d duplicate_connection alerts after %d parallel refusals: %+v", count, n, view.Alerts)
	}
}

// flakySettle refuses its first n SettleCommand calls, standing in for a locked database.
type flakySettle struct {
	store.TenancyStore
	n atomic.Int32
}

func (f *flakySettle) SettleCommand(ctx context.Context, endpointID, id, outcome, detail string) error {
	if f.n.Add(-1) >= 0 {
		return errors.New("database is locked")
	}
	return f.TenancyStore.SettleCommand(ctx, endpointID, id, outcome, detail)
}

type flakyStore struct {
	store.Store
	tenancy *flakySettle
}

func (f flakyStore) Tenancy() store.TenancyStore { return f.tenancy }

// A socket's end keeps settling its unanswered commands while the store refuses, so a slow
// database cannot leave them in flight beside a successor.
func TestEvictedSocketCommandsSettleAfterAStoreRefusal(t *testing.T) {
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
	ts := st.Tenancy()
	flaky := &flakySettle{TenancyStore: ts}
	s := NewServer(cfg, flakyStore{Store: st, tenancy: flaky})
	defer s.stopping.Store(true)
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
	if ok, err := ts.AcceptInventory(ctx, e.ID, uint64(time.Now().Unix()), time.Now(), []byte(`{"engine":{"version":"27.0"}}`)); err != nil || !ok {
		t.Fatalf("inventory: %v %v", ok, err)
	}
	cmd, err := ts.CreateCommand(ctx, org, e.ID, protocol.ActionImagePull, "docker.io/library/alpine:3.24", "", protocol.Expectation{})
	if err != nil {
		t.Fatal(err)
	}
	gone := &agentConn{endpointID: e.ID, send: make(chan protocol.Envelope, 8), closed: make(chan struct{})}
	s.agents.add(gone)
	if !s.agents.deliverCommand(e.ID, cmd.ID, envelope(protocol.TypeCommand, protocol.Command{ID: cmd.ID})) {
		t.Fatal("the socket did not take the command")
	}
	successor := &agentConn{endpointID: e.ID, send: make(chan protocol.Envelope, 8), closed: make(chan struct{})}
	if !s.agents.replace(gone, successor) {
		t.Fatal("replace")
	}
	old := carriedRetry
	carriedRetry = 20 * time.Millisecond
	t.Cleanup(func() { carriedRetry = old })
	flaky.n.Store(3)
	s.markOffline(ctx, gone)
	for {
		got, err := ts.ReadCommand(ctx, org, e.ID, cmd.ID)
		if err == nil && got.Outcome == protocol.OutcomeUnknown {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("the evicted socket's command stayed in flight after the store refused: %+v %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if flaky.n.Load() >= 0 {
		t.Fatal("the store never refused, so the retry was not exercised")
	}

	// A store that keeps refusing: shutdown stops the worker, and WaitDetached joins it before the
	// store would close, so no settle runs after.
	stuck, err := ts.CreateCommand(ctx, org, e.ID, protocol.ActionImagePull, "docker.io/library/alpine:3.23", "", protocol.Expectation{})
	if err != nil {
		t.Fatal(err)
	}
	second := &agentConn{endpointID: e.ID, send: make(chan protocol.Envelope, 8), closed: make(chan struct{})}
	if !s.agents.replace(successor, second) || !s.agents.deliverCommand(e.ID, stuck.ID, envelope(protocol.TypeCommand, protocol.Command{ID: stuck.ID})) {
		t.Fatal("the second socket did not take the command")
	}
	s.agents.replace(second, &agentConn{endpointID: e.ID, send: make(chan protocol.Envelope, 8), closed: make(chan struct{})})
	flaky.n.Store(1 << 30)
	s.markOffline(ctx, second)
	time.Sleep(3 * carriedRetry)
	s.stopping.Store(true)
	joined := make(chan struct{})
	go func() { s.WaitDetached(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitDetached did not join the retry worker at shutdown")
	}
	calls := flaky.n.Load()
	time.Sleep(3 * carriedRetry)
	if flaky.n.Load() != calls {
		t.Fatal("the retry worker settled after shutdown")
	}
}
