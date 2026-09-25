package client_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

func TestConnectURLRefusesPlaintextOffLoopback(t *testing.T) {
	for in, want := range map[string]string{"https://ky.example": "wss://ky.example/api/agent/v1/connect", "http://127.0.0.1:8080": "ws://127.0.0.1:8080/api/agent/v1/connect", "http://localhost:8080/": "ws://localhost:8080/api/agent/v1/connect"} {
		got, err := client.ConnectURL(in)
		if err != nil || got != want {
			t.Fatalf("%s: %s %v", in, got, err)
		}
	}
	for _, bad := range []string{"http://ky.example", "http://10.0.0.5", "ftp://x"} {
		if _, err := client.ConnectURL(bad); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

func TestIdentityIsPrivateAndAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if id, err := client.LoadIdentity(dir); id != nil || err != nil {
		t.Fatalf("fresh dir: %v %v", id, err)
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	if err := client.SaveIdentity(dir, &client.Identity{EndpointID: "ep_1", PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: "https://ky"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "identity.json"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode %v want %v (%v)", path, info.Mode().Perm(), want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp file left behind")
	}
	id, err := client.LoadIdentity(dir)
	if err != nil || id.EndpointID != "ep_1" {
		t.Fatalf("reload: %+v %v", id, err)
	}
}

// The whole agent path against a real server: enroll from a token, wait as pending, get
// approved, become active through inventory, and stop on revocation.
func TestAgentEnrollsConnectsAndStopsOnRevocation(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dbCfg := testdb.Config(t)
	dbCfg.DataDir = cfg.Database.DataDir
	cfg.Database = dbCfg
	cfg.Captcha.Provider = "none"
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := api.NewServer(cfg, st)
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	hash, _ := password.Hash("SuperSecretPass123!")
	_ = st.Users().CreateUser(ctx, &store.User{ID: "usr_admin", Username: "admin", PasswordHash: hash, Role: "user", Status: "active", SSOProvider: "local"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_admin", Role: store.RoleOrganizationAdmin, Status: "active"})
	jar := login(t, httpSrv.URL, "admin", "SuperSecretPass123!")
	minted := post(t, httpSrv.URL+"/api/organizations/a/environments/env-a/enrollment-tokens", jar, `{"runtime":"docker"}`)
	var tok struct{ Token string }
	_ = json.Unmarshal(minted, &tok)

	dir := filepath.Join(t.TempDir(), "id")
	httpClient := &http.Client{Timeout: 10 * time.Second}
	id, err := client.Enroll(ctx, httpClient, httpSrv.URL, client.DirStore(dir), "host-x", tok.Token, client.Facts(""))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := client.Enroll(ctx, httpClient, httpSrv.URL, client.DirStore(filepath.Join(t.TempDir(), "again")), "host-y", tok.Token, client.Facts("")); err == nil {
		t.Fatal("token reused")
	}
	states := make(chan string, 16)
	runErr := make(chan error, 1)
	runCtx, stopAgent := context.WithCancel(ctx)
	defer stopAgent()
	go func() {
		runErr <- client.Run(runCtx, id, client.Options{HTTPClient: httpClient, IdentityDir: dir, OnState: func(s string) { states <- s }})
	}()
	if s := <-states; s != "pending" {
		t.Fatalf("first state %q", s)
	}
	e, _ := ts.ReadEndpointRaw(ctx, id.EndpointID)
	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/approve", jar, `{"fingerprint":"`+e.Fingerprint+`"}`)
	if s := <-states; s != "approved" {
		t.Fatalf("state after approval %q", s)
	}
	waitState(t, ctx, ts, id.EndpointID, "active")
	// A restarted process must come back active: the generation persists and rises.
	stopAgent()
	if err := <-runErr; err != nil {
		t.Fatalf("first run: %v", err)
	}
	waitState(t, ctx, ts, id.EndpointID, "offline")
	reloaded, err := client.LoadIdentity(dir)
	if err != nil || reloaded.Generation == 0 {
		t.Fatalf("generation not persisted: %+v %v", reloaded, err)
	}
	if !reloaded.MatchesEnrollment(httpSrv.URL, tok.Token) || reloaded.MatchesEnrollment("https://another.example", tok.Token) || reloaded.MatchesEnrollment(httpSrv.URL, strings.Repeat("B", 43)) {
		t.Fatal("enrollment binding lost or accepts different link")
	}
	runCtx, stopAgent = context.WithCancel(ctx)
	defer stopAgent()
	go func() {
		runErr <- client.Run(runCtx, reloaded, client.Options{HTTPClient: httpClient, IdentityDir: dir})
	}()
	waitState(t, ctx, ts, id.EndpointID, "active")

	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/revoke", jar, "")
	select {
	case err := <-runErr:
		if !errors.Is(err, client.ErrRevoked) {
			t.Fatalf("run ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent kept running after revocation")
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		t.Fatal("identity file missing")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
	if strings.Contains(string(raw), tok.Token) {
		t.Fatal("enrollment token persisted in the identity file")
	}
}

type cookies struct{ session, csrf string }

func login(t *testing.T, base, user, pass string) cookies {
	t.Helper()
	resp, err := http.Post(base+"/api/auth/login", "application/json", strings.NewReader(`{"username":"`+user+`","password":"`+pass+`"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login: %v %v", err, resp)
	}
	var c cookies
	for _, ck := range resp.Cookies() {
		switch ck.Name {
		case auth.SessionCookieName:
			c.session = ck.Value
		case auth.CSRFCookieName:
			c.csrf = ck.Value
		}
	}
	return c
}

func post(t *testing.T, url string, c cookies, body string) []byte {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.HeaderCSRF, c.csrf)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: c.session})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: c.csrf})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if resp.StatusCode >= 300 {
		t.Fatalf("POST %s: %d %s", url, resp.StatusCode, out.String())
	}
	return []byte(out.String())
}

func waitState(t *testing.T, ctx context.Context, ts store.TenancyStore, id, want string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		e, _ := ts.ReadEndpointRaw(ctx, id)
		if e != nil && e.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("endpoint never reached %s: %+v", want, e)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Enrollment never sends the token in the clear off loopback.
func TestEnrollRefusesPlaintextOffLoopback(t *testing.T) {
	requests := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(401) }))
	defer stub.Close()
	ctx := context.Background()
	for _, bad := range []string{"http://ky.example", "http://10.0.0.5:8080", "ftp://127.0.0.1", "http://user:pw@127.0.0.1"} {
		if _, err := client.Enroll(ctx, stub.Client(), bad, client.DirStore(t.TempDir()), "h", strings.Repeat("A", 43), client.Facts("")); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if requests != 0 {
		t.Fatalf("%d requests left the agent for refused origins", requests)
	}
	if _, err := client.Enroll(ctx, stub.Client(), stub.URL, client.DirStore(t.TempDir()), "h", strings.Repeat("A", 43), client.Facts("")); err == nil || requests != 1 {
		t.Fatalf("loopback enrollment did not reach the server: %v %d", err, requests)
	}
}

// A control plane that accepts and then goes silent must not wedge the agent.
func TestAgentReconnectsWhenTheControlPlaneGoesSilent(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	dials := make(chan int, 8)
	n := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		n++
		dials <- n
		ctx := r.Context()
		nonce := make([]byte, 32)
		frame := func(typ string, payload any) []byte {
			raw, _ := json.Marshal(payload)
			b, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: typ, Payload: raw})
			return b
		}
		_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeChallenge, protocol.Challenge{Nonce: nonce, InstanceFingerprint: strings.Repeat("a", 64), Versions: []int{1}}))
		_, _, _ = c.Read(ctx)
		_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeHello, protocol.Hello{State: "active", HeartbeatSeconds: 1}))
		<-ctx.Done()
	}))
	defer stub.Close()
	id := &client.Identity{EndpointID: "ep_silent", PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: stub.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() { _ = client.Run(ctx, id, client.Options{HTTPClient: stub.Client()}) }()
	if <-dials != 1 {
		t.Fatal("first dial")
	}
	select {
	case d := <-dials:
		if d != 2 {
			t.Fatalf("dial %d", d)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("agent did not reconnect after the control plane went silent")
	}
}

// Rotation end to end: the agent offers a key, keeps authenticating with the old one until the
// operator acknowledges, then switches and the old key is dead.
func TestAgentRotatesKeyOnlyAfterAcknowledgement(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dbCfg := testdb.Config(t)
	dbCfg.DataDir = cfg.Database.DataDir
	cfg.Database = dbCfg
	cfg.Captcha.Provider = "none"
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := api.NewServer(cfg, st)
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	hash, _ := password.Hash("SuperSecretPass123!")
	_ = st.Users().CreateUser(ctx, &store.User{ID: "usr_admin", Username: "admin", PasswordHash: hash, Role: "user", Status: "active", SSOProvider: "local"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_admin", Role: store.RoleOrganizationAdmin, Status: "active"})
	jar := login(t, httpSrv.URL, "admin", "SuperSecretPass123!")
	minted := post(t, httpSrv.URL+"/api/organizations/a/environments/env-a/enrollment-tokens", jar, `{"runtime":"docker"}`)
	var tok struct{ Token string }
	_ = json.Unmarshal(minted, &tok)
	dir := filepath.Join(t.TempDir(), "id")
	httpClient := &http.Client{Timeout: 10 * time.Second}
	id, err := client.Enroll(ctx, httpClient, httpSrv.URL, client.DirStore(dir), "host-r", tok.Token, client.Facts(""))
	if err != nil {
		t.Fatal(err)
	}
	e, _ := ts.ReadEndpointRaw(ctx, id.EndpointID)
	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/approve", jar, `{"fingerprint":"`+e.Fingerprint+`"}`)
	oldFP := e.Fingerprint

	id.RotatedAt = time.Time{} // enrollment set it to now; make the first rotation due
	runCtx, stopAgent := context.WithCancel(ctx)
	defer stopAgent()
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(runCtx, id, client.Options{HTTPClient: httpClient, IdentityDir: dir, RotateEvery: 30 * time.Second})
	}()
	waitState(t, ctx, ts, id.EndpointID, "active")
	// The rotated key is recorded as pending and the old key still authenticates.
	view := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a"}
	deadline := time.Now().Add(8 * time.Second)
	for {
		e, _ = ts.ReadEndpoint(ctx, view, id.EndpointID)
		if e != nil && e.PendingFingerprint != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pending key recorded: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e.Fingerprint != oldFP || len(e.Alerts) != 1 || e.Alerts[0].Kind != "rotation_pending" {
		t.Fatalf("pending rotation state: %+v", e)
	}
	onDisk, _ := client.LoadIdentity(dir)
	if onDisk.PendingFingerprint != e.PendingFingerprint || len(onDisk.PendingPrivateKey) == 0 {
		t.Fatalf("pending key not persisted: %+v", onDisk)
	}
	// Operator acknowledges: the agent switches, reconnects with the new key, stays active.
	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/keys/"+e.PendingFingerprint+"/acknowledge", jar, "")
	deadline = time.Now().Add(8 * time.Second)
	for {
		onDisk, _ = client.LoadIdentity(dir)
		e, _ = ts.ReadEndpoint(ctx, view, id.EndpointID)
		if e != nil && onDisk.PendingFingerprint == "" && e.Fingerprint != oldFP && e.PendingFingerprint == "" && e.State == "active" && len(e.Alerts) == 0 && e.LastSeenAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent did not switch keys: disk=%+v endpoint=%+v", onDisk, e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopAgent()
	if err := <-runErr; err != nil {
		t.Fatalf("run: %v", err)
	}
	// The old key is retired for good.
	if _, err := ts.AgentIdentity(ctx, id.EndpointID, oldFP); !errors.Is(err, store.ErrKeyRetired) {
		t.Fatalf("old key still usable: %v", err)
	}
}

// Setup shared by the rotation-hardening tests: a real server, an approved agent identity.
// testDB is the database the approved agent's server uses; tests reach it directly to move
// timestamps, which is not a product path.
var testDB config.DatabaseConfig

func backdatePendingKey(t *testing.T, fingerprint string, createdAt time.Time) {
	t.Helper()
	driver := testDB.Driver
	if driver == "postgres" {
		driver = "pgx"
	}
	db, err := sql.Open(driver, testDB.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := "UPDATE endpoint_keys SET created_at=? WHERE fingerprint=?"
	if driver == "pgx" {
		q = "UPDATE endpoint_keys SET created_at=$1 WHERE fingerprint=$2"
	}
	if _, err := db.ExecContext(context.Background(), q, createdAt.UTC(), fingerprint); err != nil {
		t.Fatal(err)
	}
}

func approvedAgent(t *testing.T) (*httptest.Server, store.Store, cookies, *client.Identity, string) {
	t.Helper()
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dbCfg := testdb.Config(t)
	testDB = dbCfg
	dbCfg.DataDir = cfg.Database.DataDir
	cfg.Database = dbCfg
	cfg.Captcha.Provider = "none"
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	httpSrv := httptest.NewServer(api.NewServer(cfg, st))
	t.Cleanup(httpSrv.Close)
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	hash, _ := password.Hash("SuperSecretPass123!")
	_ = st.Users().CreateUser(ctx, &store.User{ID: "usr_admin", Username: "admin", PasswordHash: hash, Role: "user", Status: "active", SSOProvider: "local"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_admin", Role: store.RoleOrganizationAdmin, Status: "active"})
	jar := login(t, httpSrv.URL, "admin", "SuperSecretPass123!")
	minted := post(t, httpSrv.URL+"/api/organizations/a/environments/env-a/enrollment-tokens", jar, `{"runtime":"docker"}`)
	var tok struct{ Token string }
	_ = json.Unmarshal(minted, &tok)
	dir := filepath.Join(t.TempDir(), "id")
	id, err := client.Enroll(ctx, &http.Client{Timeout: 10 * time.Second}, httpSrv.URL, client.DirStore(dir), "host-h", tok.Token, client.Facts(""))
	if err != nil {
		t.Fatal(err)
	}
	e, _ := ts.ReadEndpointRaw(ctx, id.EndpointID)
	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/approve", jar, `{"fingerprint":"`+e.Fingerprint+`"}`)
	return httpSrv, st, jar, id, dir
}

// A key that cannot be persisted is never announced.
func TestRotationIsNotOfferedWhenTheIdentityCannotBePersisted(t *testing.T) {
	httpSrv, st, _, id, _ := approvedAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Persistence is broken: the identity path runs through a regular file, so no write there
	// can succeed (SaveIdentity tightens modes, so a chmod alone cannot model it).
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "id")
	id.RotatedAt = time.Time{}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx, id, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir, RotateEvery: time.Nanosecond})
	}()
	// The session ends as soon as the offer cannot be persisted, so the endpoint is active only
	// briefly; wait for the server to have applied a frame at all, then give a retry time to happen.
	deadline := time.Now().Add(8 * time.Second)
	for {
		e, _ := st.Tenancy().ReadEndpointRaw(ctx, id.EndpointID)
		if e != nil && e.LastSeenAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent never reached the server")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
	stop()
	<-done
	view, err := st.Tenancy().ReadEndpoint(ctx, store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a"}, id.EndpointID)
	if err != nil || view.PendingFingerprint != "" || len(view.Alerts) != 0 {
		t.Fatalf("a key the agent could not persist was announced: %v %+v", err, view)
	}
	if id.PendingFingerprint != "" {
		t.Fatal("pending key kept in memory after a failed save")
	}
}

// An offer nobody acknowledged expires on the agent too, and a fresh key is offered next.
func TestExpiredPendingOfferIsReplaced(t *testing.T) {
	httpSrv, st, _, id, dir := approvedAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ts := st.Tenancy()
	view := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a"}
	run := func() {
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- client.Run(runCtx, id, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir, RotateEvery: 30 * time.Second})
		}()
		deadline := time.Now().Add(8 * time.Second)
		for {
			e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
			if e != nil && e.PendingFingerprint != "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no rotation offered")
			}
			time.Sleep(20 * time.Millisecond)
		}
		stop()
		<-done
	}
	id.RotatedAt = time.Time{} // due now
	run()
	first := id.PendingFingerprint
	e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
	if first == "" || e.PendingFingerprint != first {
		t.Fatalf("first offer: %q %+v", first, e)
	}
	// Eight days pass without acknowledgement, on both sides.
	backdate := time.Now().Add(-8 * 24 * time.Hour)
	backdatePendingKey(t, first, backdate)
	id.PendingSince = backdate
	id.RotatedAt = time.Time{}
	if err := client.SaveIdentity(dir, id); err != nil {
		t.Fatal(err)
	}
	e, _ = ts.ReadEndpoint(ctx, view, id.EndpointID)
	if e.PendingFingerprint != "" {
		t.Fatalf("expired key still offered by the server view: %+v", e)
	}
	run()
	if id.PendingFingerprint == "" || id.PendingFingerprint == first {
		t.Fatalf("agent did not offer a fresh key after expiry: %q vs %q", id.PendingFingerprint, first)
	}
	e, _ = ts.ReadEndpoint(ctx, view, id.EndpointID)
	if e.PendingFingerprint != id.PendingFingerprint {
		t.Fatalf("server did not record the fresh key: %+v", e)
	}
}

// The agent may forget an offer before the server does (skewed clock, resumed VM); a late
// acknowledgement must still promote instead of locking the host out.
func TestLateAcknowledgementAfterTheAgentForgotTheOffer(t *testing.T) {
	httpSrv, st, jar, id, dir := approvedAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ts := st.Tenancy()
	view := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a"}
	run := func(untilPending bool) {
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- client.Run(runCtx, id, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir, RotateEvery: 30 * time.Second})
		}()
		deadline := time.Now().Add(8 * time.Second)
		for {
			e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
			if e != nil && e.State == "active" && (!untilPending || e.PendingFingerprint != "") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("session did not reach the expected state: %+v", e)
			}
			time.Sleep(20 * time.Millisecond)
		}
		stop()
		if err := <-done; err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	id.RotatedAt = time.Time{}
	run(true)
	offered := id.PendingFingerprint
	// Only the agent's clock says the offer lapsed; the server still accepts it.
	id.PendingSince = time.Now().Add(-client.PendingKeyLife - time.Minute)
	if err := client.SaveIdentity(dir, id); err != nil {
		t.Fatal(err)
	}
	run(false)
	// Rotation was due again, the server refused the fresh offer (its copy of the old one is
	// still live), and the agent restored the old offer as pending. Its material survived.
	if len(id.PendingPrivateKey) == 0 && len(id.LapsedPrivateKey) == 0 {
		t.Fatalf("lapsed key material dropped: %+v", id)
	}
	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/keys/"+offered+"/acknowledge", jar, "")
	// The next session meets key_retired and must promote the retained key rather than exit.
	reloaded, err := client.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx, reloaded, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir, RotateEvery: 30 * time.Second})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
		if e != nil && e.State == "active" && e.Fingerprint == offered {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("agent stopped instead of promoting: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never came back with the acknowledged key: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Metrics ride along with each inventory report and land as samples.
func TestAgentReportsMetricsWithInventory(t *testing.T) {
	httpSrv, st, _, id, dir := approvedAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	snapshot := func(context.Context) (*protocol.Snapshot, error) {
		return &protocol.Snapshot{ObservedAt: time.Now(), Containers: []protocol.Container{{ID: "c1", Name: "web", State: "running", Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}}, {ID: "c2", Name: "old", State: "exited", Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}}}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}, nil
	}
	var asked []string
	metrics := func(_ context.Context, running []string) protocol.Metrics {
		asked = append(asked, running...)
		return protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c1", CPUPercent: 7.5, MemoryBytes: 42}}}
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = client.Run(runCtx, id, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir, Snapshot: snapshot, Metrics: metrics})
	}()
	view := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a"}
	deadline := time.Now().Add(8 * time.Second)
	for {
		latest, _ := st.Tenancy().LatestSamples(ctx, view, id.EndpointID)
		if len(latest) == 1 && latest[0].ContainerID == "c1" && latest[0].CPUPercent == 7.5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("samples never arrived: %+v", latest)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(asked) != 1 || asked[0] != "c1" {
		t.Fatalf("metrics asked for %v, want only the running container", asked)
	}
}

// A slow runtime must not cost heartbeats: metrics are collected off the loop.
func TestHeartbeatsContinueWhileMetricsAreSlow(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	beats := make(chan struct{}, 64)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		frame := func(typ string, payload any) []byte {
			raw, _ := json.Marshal(payload)
			b, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: typ, Payload: raw})
			return b
		}
		_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeChallenge, protocol.Challenge{Nonce: make([]byte, 32), InstanceFingerprint: strings.Repeat("a", 64), Versions: []int{1}}))
		_, _, _ = c.Read(ctx)
		_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeHello, protocol.Hello{State: "active", HeartbeatSeconds: 1}))
		for {
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			var e protocol.Envelope
			_ = json.Unmarshal(raw, &e)
			if e.Type == protocol.TypeHeartbeat {
				beats <- struct{}{}
				_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeHeartbeat, nil))
			}
		}
	}))
	defer stub.Close()
	id := &client.Identity{EndpointID: "ep_slow", PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: stub.URL, RotatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	snapshot := func(context.Context) (*protocol.Snapshot, error) {
		return &protocol.Snapshot{Containers: []protocol.Container{{ID: "c1", State: "running", Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}}}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}, nil
	}
	metrics := func(mctx context.Context, _ []string) protocol.Metrics {
		select {
		case <-time.After(4 * time.Second):
		case <-mctx.Done():
		}
		return protocol.Metrics{}
	}
	go func() {
		_ = client.Run(ctx, id, client.Options{HTTPClient: stub.Client(), Snapshot: snapshot, Metrics: metrics, InventoryEvery: time.Hour})
	}()
	deadline := time.After(4500 * time.Millisecond)
	n := 0
	for n < 3 {
		select {
		case <-beats:
			n++
		case <-deadline:
			t.Fatalf("only %d heartbeats in 4.5 s while metrics blocked; the loop is starved", n)
		}
	}
}

// identity_revoked is terminal: the server also uses it for a bad signature or a store error,
// so an agent inside a rotation window must not answer it by discarding its current key.
func TestRevocationDoesNotSwitchKeys(t *testing.T) {
	httpSrv, st, jar, id, dir := approvedAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ts := st.Tenancy()
	view := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a"}
	id.RotatedAt = time.Time{}
	current := append([]byte(nil), id.PrivateKey...)
	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx, id, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir, RotateEvery: 30 * time.Second})
	}()
	deadline := time.Now().Add(8 * time.Second)
	for {
		e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
		if e != nil && e.State == "active" && e.PendingFingerprint != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no rotation offer: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/revoke", jar, "")
	select {
	case err := <-done:
		if !errors.Is(err, client.ErrRevoked) {
			t.Fatalf("expected ErrRevoked, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent kept running after revocation")
	}
	saved, err := client.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved.PrivateKey, current) || len(saved.PendingPrivateKey) == 0 {
		t.Fatalf("revocation rewrote the identity: current changed=%v pending kept=%v", !bytes.Equal(saved.PrivateKey, current), len(saved.PendingPrivateKey) != 0)
	}
}

// A session killed between saving a rotation offer and hearing the answer leaves a key the
// server may never have recorded. When a late acknowledgement then retires the current key,
// the agent must reach the acknowledged key rather than strand itself on the unrecorded one:
// the server answers an unknown key exactly as it answers a revoked endpoint, and that is
// terminal.
func TestAnUnrecordedOfferDoesNotStrandTheAgent(t *testing.T) {
	httpSrv, st, jar, id, dir := approvedAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	view := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a"}

	// One offer, recorded by the server.
	id.RotatedAt = time.Time{}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx, id, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir, RotateEvery: 30 * time.Second})
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
		if e != nil && e.PendingFingerprint != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no rotation offer was recorded: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	recorded := id.PendingFingerprint
	recordedKey := append([]byte(nil), id.PendingPrivateKey...)

	// The state a crash mid-offer leaves on disk: the recorded offer has lapsed out of the
	// pending slot, and a newer offer sits there that the session died before hearing an
	// answer for. Written as the file itself, the way a killed agent leaves it.
	unrecordedPub, unrecordedPriv, _ := ed25519.GenerateKey(rand.Reader)
	raw, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	saved["lapsed_private_key"] = recordedKey
	saved["pending_private_key"] = []byte(unrecordedPriv)
	saved["pending_fingerprint"] = protocol.Fingerprint(unrecordedPub)
	saved["pending_since"] = time.Now().UTC()
	delete(saved, "pending_recorded")
	raw, err = json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// The operator acknowledges the offer the server actually holds.
	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/keys/"+recorded+"/acknowledge", jar, "")

	reloaded, err := client.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	runCtx2, stop2 := context.WithCancel(ctx)
	defer stop2()
	done2 := make(chan error, 1)
	go func() {
		done2 <- client.Run(runCtx2, reloaded, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir})
	}()
	deadline = time.Now().Add(20 * time.Second)
	for {
		e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
		if e != nil && e.State == "active" && e.Fingerprint == recorded {
			return
		}
		select {
		case err := <-done2:
			t.Fatalf("agent gave up instead of trying the recorded key: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never authenticated with the acknowledged key: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The lapsed slot is not a mark of trust: an offer that lapsed without ever being confirmed
// belongs behind one the server did record, whichever slot each sits in.
func TestAnUnconfirmedLapsedKeyDoesNotOutrankARecordedOffer(t *testing.T) {
	httpSrv, st, jar, id, dir := approvedAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	view := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a"}

	// One offer the server records, left in the pending slot.
	id.RotatedAt = time.Time{}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx, id, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir, RotateEvery: 30 * time.Second})
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
		if e != nil && e.PendingFingerprint != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no rotation offer was recorded: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	recorded := id.PendingFingerprint

	// On disk: the recorded offer still pending but with its provenance lost (an older agent
	// wrote this file), and a key in the lapsed slot the server has never seen.
	_, strayPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	saved["lapsed_private_key"] = []byte(strayPriv)
	delete(saved, "lapsed_recorded")
	delete(saved, "pending_recorded")
	raw, err = json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/keys/"+recorded+"/acknowledge", jar, "")

	reloaded, err := client.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	runCtx2, stop2 := context.WithCancel(ctx)
	defer stop2()
	done2 := make(chan error, 1)
	go func() {
		done2 <- client.Run(runCtx2, reloaded, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir})
	}()
	deadline = time.Now().Add(20 * time.Second)
	for {
		e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
		if e != nil && e.State == "active" && e.Fingerprint == recorded {
			return
		}
		select {
		case err := <-done2:
			t.Fatalf("agent gave up instead of trying the acknowledged key: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never authenticated with the acknowledged key: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A server fault answers every attempt the way a revocation does. Walking the candidates must
// cost attempts and nothing else: when the fault clears, the key that works is still on disk.
func TestAFaultyServerDoesNotConsumeKeyMaterial(t *testing.T) {
	httpSrv, st, jar, id, dir := approvedAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	view := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a"}

	// An offer the server records and the operator acknowledges, so the agent's own key is
	// retired and the acknowledged key is the one that works.
	id.RotatedAt = time.Time{}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx, id, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir, RotateEvery: 30 * time.Second})
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
		if e != nil && e.PendingFingerprint != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no rotation offer was recorded: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	recorded := id.PendingFingerprint
	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/keys/"+recorded+"/acknowledge", jar, "")

	// A server that retires the key in hand and then refuses every candidate the way a
	// revocation reads: a store outage or a Host-rewriting proxy behind the same answers.
	instance := id.InstanceFingerprint
	var attempts atomic.Int64
	faulty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nonce := make([]byte, 32)
		_, _ = rand.Read(nonce)
		raw, _ := json.Marshal(protocol.Challenge{Nonce: nonce, InstanceFingerprint: instance, Versions: []int{protocol.Version}})
		frame, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: protocol.TypeChallenge, Payload: raw})
		if err := c.Write(r.Context(), websocket.MessageText, frame); err != nil {
			return
		}
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		if attempts.Add(1) == 1 {
			c.Close(websocket.StatusPolicyViolation, protocol.CloseKeyRetired)
			return
		}
		c.Close(websocket.StatusPolicyViolation, protocol.CloseRevoked)
	}))
	defer faulty.Close()

	before, err := client.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	broken := *before
	broken.Server = faulty.URL
	brokenDir := t.TempDir()
	if err := client.SaveIdentity(brokenDir, &broken); err != nil {
		t.Fatal(err)
	}
	if err := client.Run(ctx, &broken, client.Options{HTTPClient: faulty.Client(), IdentityDir: brokenDir}); !errors.Is(err, client.ErrRevoked) {
		t.Fatalf("expected the walk to end in ErrRevoked, got %v", err)
	}
	if n := attempts.Load(); n < 2 {
		t.Fatalf("the walk never left the first key: %d attempts", n)
	}
	after, err := client.LoadIdentity(brokenDir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.PrivateKey, before.PrivateKey) || !bytes.Equal(after.PendingPrivateKey, before.PendingPrivateKey) || !bytes.Equal(after.LapsedPrivateKey, before.LapsedPrivateKey) {
		t.Fatal("the walk against a faulty server overwrote key material")
	}

	// The fault clears: the same identity reaches the acknowledged key.
	healed, err := client.LoadIdentity(brokenDir)
	if err != nil {
		t.Fatal(err)
	}
	healed.Server = httpSrv.URL
	runCtx2, stop2 := context.WithCancel(ctx)
	defer stop2()
	done2 := make(chan error, 1)
	go func() {
		done2 <- client.Run(runCtx2, healed, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: brokenDir})
	}()
	deadline = time.Now().Add(20 * time.Second)
	for {
		e, _ := ts.ReadEndpoint(ctx, view, id.EndpointID)
		if e != nil && e.State == "active" && e.Fingerprint == recorded {
			return
		}
		select {
		case err := <-done2:
			t.Fatalf("agent gave up after the fault cleared: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never authenticated after the fault cleared: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A walk that runs out of candidates must leave the agent on its own key. If the index
// survived, every later cycle would sign with the last key the server refused, and the key
// that works would sit unreachable on disk.
func TestAnExhaustedWalkReturnsToTheIdentityKey(t *testing.T) {
	httpSrv, st, _, id, dir := approvedAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	approved, err := ts.ReadEndpointRaw(ctx, id.EndpointID)
	if err != nil {
		t.Fatal(err)
	}

	// Two candidates the server has never seen, both marked as offers it confirmed, either
	// side of the identity's own key, which is the approved one.
	_, stray1, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, stray2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	saved["pending_private_key"] = []byte(stray1)
	saved["pending_fingerprint"] = protocol.Fingerprint(stray1.Public().(ed25519.PublicKey))
	saved["pending_since"] = time.Now().UTC()
	saved["pending_recorded"] = true
	saved["lapsed_private_key"] = []byte(stray2)
	saved["lapsed_recorded"] = true
	raw, err = json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// A server that retires the key in hand once, then refuses everything.
	instance := id.InstanceFingerprint
	var attempts atomic.Int64
	faulty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nonce := make([]byte, 32)
		_, _ = rand.Read(nonce)
		payload, _ := json.Marshal(protocol.Challenge{Nonce: nonce, InstanceFingerprint: instance, Versions: []int{protocol.Version}})
		frame, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: protocol.TypeChallenge, Payload: payload})
		if err := c.Write(r.Context(), websocket.MessageText, frame); err != nil {
			return
		}
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		if attempts.Add(1) == 1 {
			c.Close(websocket.StatusPolicyViolation, protocol.CloseKeyRetired)
			return
		}
		c.Close(websocket.StatusPolicyViolation, protocol.CloseRevoked)
	}))
	defer faulty.Close()

	stuck, err := client.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	stuck.Server = faulty.URL
	if err := client.Run(ctx, stuck, client.Options{HTTPClient: faulty.Client(), IdentityDir: dir}); !errors.Is(err, client.ErrRevoked) {
		t.Fatalf("expected the exhausted walk to end in ErrRevoked, got %v", err)
	}
	if n := attempts.Load(); n < 3 {
		t.Fatalf("the walk did not try both candidates: %d attempts", n)
	}
	if stuck.RecoveryAttempt != 0 {
		t.Fatalf("the walk left the index at %d, so the next cycle would sign with a refused key", stuck.RecoveryAttempt)
	}
	if reloaded, err := client.LoadIdentity(dir); err != nil || reloaded.RecoveryAttempt != 0 {
		t.Fatalf("the index survived on disk: %+v %v", reloaded, err)
	}

	// The fault clears. The agent must be back on its own key, which is the approved one.
	stuck.Server = httpSrv.URL
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx, stuck, client.Options{HTTPClient: httpSrv.Client(), IdentityDir: dir})
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		e, _ := ts.ReadEndpointRaw(ctx, id.EndpointID)
		if e != nil && e.State == "active" && e.Fingerprint == approved.Fingerprint {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("agent gave up on its own key after the fault cleared: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never authenticated with its own key: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A command can take minutes -- a pull is bounded by the network, not by the daemon -- so it
// runs off the session loop. If it did not, any principal allowed to pull could make an
// endpoint miss its heartbeats, go offline, and stay unmanageable for as long as the pull ran.
func TestCommandsDoNotStallTheSession(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	beats := make(chan struct{}, 64)
	results := make(chan protocol.Result, 16)
	release := make(chan struct{})
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		frame := func(typ string, payload any) []byte {
			raw, _ := json.Marshal(payload)
			b, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: typ, Payload: raw})
			return b
		}
		_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeChallenge, protocol.Challenge{Nonce: make([]byte, 32), InstanceFingerprint: strings.Repeat("a", 64), Versions: []int{1}}))
		_, _, _ = c.Read(ctx)
		_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeHello, protocol.Hello{State: "active", HeartbeatSeconds: 1}))
		// More pulls than the image reserve holds, so the surplus must be refused rather than
		// queued behind the others -- and then a container action, which must still get
		// through: an operator's emergency stop cannot wait on a registry.
		for i := 0; i < 4; i++ {
			_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeCommand, protocol.Command{
				ID: fmt.Sprintf("pull_%d", i), Endpoint: "ep_busy", Action: protocol.ActionImagePull,
				Reference: "ghcr.io/busnes-app/kyyard:1.2.3", Deadline: time.Now().UTC().Add(time.Minute),
			}))
		}
		_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeCommand, protocol.Command{
			ID: "stop_0", Endpoint: "ep_busy", Action: protocol.ActionStop,
			Container: "c1", Deadline: time.Now().UTC().Add(time.Minute),
		}))
		for {
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			var e protocol.Envelope
			_ = json.Unmarshal(raw, &e)
			switch e.Type {
			case protocol.TypeHeartbeat:
				beats <- struct{}{}
				_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeHeartbeat, nil))
			case protocol.TypeResult:
				var res protocol.Result
				_ = json.Unmarshal(e.Payload, &res)
				results <- res
			}
		}
	}))
	defer stub.Close()
	id := &client.Identity{EndpointID: "ep_busy", PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: stub.URL, RotatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snapshot := func(context.Context) (*protocol.Snapshot, error) {
		return &protocol.Snapshot{Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}, nil
	}
	started := make(chan string, 8)
	operate := func(_ context.Context, cmd protocol.Command) (string, string) {
		started <- cmd.ID
		<-release
		return protocol.OutcomeSucceeded, "done"
	}
	go func() {
		_ = client.Run(ctx, id, client.Options{
			HTTPClient: stub.Client(), Snapshot: snapshot, InventoryEvery: time.Hour,
			IdentityDir: t.TempDir(), Operate: operate,
		})
	}()

	// maxInFlightImageCommands, which this package cannot see from outside.
	const imageReserve = 2

	// While the blocked commands sit in Operate, the loop still answers the heartbeat, refuses
	// the pulls past the image reserve immediately, and never refuses the container action.
	denied := 0
	seen := 0
	deadline := time.After(15 * time.Second)
	for denied < 4-imageReserve || seen < 3 {
		select {
		case <-beats:
			seen++
		case res := <-results:
			if res.Outcome != protocol.OutcomeDenied {
				t.Fatalf("a blocked command answered early: %+v", res)
			}
			if res.ID == "stop_0" {
				t.Fatal("pulls waiting on a registry refused an operator's stop")
			}
			denied++
		case <-deadline:
			t.Fatalf("%d heartbeats and %d refusals while commands blocked; the loop is starved", seen, denied)
		}
	}
	// The stop is running rather than refused, so it is one of the commands still blocked.
	running := map[string]bool{}
	for len(running) < imageReserve+1 {
		select {
		case id := <-started:
			running[id] = true
		case <-time.After(15 * time.Second):
			t.Fatalf("only %v started", running)
		}
	}
	if !running["stop_0"] {
		t.Fatalf("the container action never ran: %v", running)
	}

	close(release)
	settled := 0
	done := time.After(15 * time.Second)
	for settled < imageReserve+1 {
		select {
		case res := <-results:
			if res.Outcome != protocol.OutcomeSucceeded {
				t.Fatalf("a released command: %+v", res)
			}
			settled++
		case <-beats:
		case <-done:
			t.Fatalf("only %d of %d commands reported after they finished", settled, imageReserve+1)
		}
	}
}

// The in-flight limit and the dedupe ledger belong to the agent, not to one connection. A
// command outlives the socket it arrived on, so a per-session limit would grant four more with
// every reconnect -- and sessions end routinely, at the server's choosing -- while a
// per-session ledger would serialise its stale view over the file and erase what the new
// session recorded, making the agent forget commands it really ran.
func TestTheInFlightLimitAndLedgerSurviveAReconnect(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	started := make(chan string, 8)
	release := make(chan struct{})
	results := make(chan protocol.Result, 16)
	dropFirst := make(chan struct{})
	sendMore := make(chan string, 4)
	var sessions atomic.Int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		frame := func(typ string, payload any) []byte {
			raw, _ := json.Marshal(payload)
			b, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: typ, Payload: raw})
			return b
		}
		command := func(id string) []byte {
			return frame(protocol.TypeCommand, protocol.Command{
				ID: id, Endpoint: "ep_limit", Action: protocol.ActionStop, Container: "c1",
				Deadline: time.Now().UTC().Add(5 * time.Minute),
			})
		}
		_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeChallenge, protocol.Challenge{Nonce: make([]byte, 32), InstanceFingerprint: strings.Repeat("a", 64), Versions: []int{1}}))
		_, _, _ = c.Read(ctx)
		_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeHello, protocol.Hello{State: "active", HeartbeatSeconds: 1}))
		if sessions.Add(1) == 1 {
			for i := 0; i < 4; i++ {
				_ = c.Write(ctx, websocket.MessageText, command(fmt.Sprintf("cmd_a%d", i)))
			}
			<-dropFirst
			c.CloseNow()
			return
		}
		go func() {
			for id := range sendMore {
				if c.Write(ctx, websocket.MessageText, command(id)) != nil {
					return
				}
			}
		}()
		for {
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			var e protocol.Envelope
			_ = json.Unmarshal(raw, &e)
			switch e.Type {
			case protocol.TypeHeartbeat:
				_ = c.Write(ctx, websocket.MessageText, frame(protocol.TypeHeartbeat, nil))
			case protocol.TypeResult:
				var res protocol.Result
				_ = json.Unmarshal(e.Payload, &res)
				results <- res
			}
		}
	}))
	defer stub.Close()
	dir := t.TempDir()
	id := &client.Identity{EndpointID: "ep_limit", PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: stub.URL, RotatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	snapshot := func(context.Context) (*protocol.Snapshot, error) {
		return &protocol.Snapshot{Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}, nil
	}
	// This runtime call ignores cancellation, as a runtime call that has already reached the
	// daemon would: the slot stays held until the work really ends.
	operate := func(_ context.Context, cmd protocol.Command) (string, string) {
		started <- cmd.ID
		<-release
		return protocol.OutcomeSucceeded, "stopped " + cmd.ID
	}
	go func() {
		_ = client.Run(ctx, id, client.Options{
			HTTPClient: stub.Client(), Snapshot: snapshot, InventoryEvery: time.Hour,
			IdentityDir: dir, Operate: operate,
		})
	}()

	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of 4 commands started", i)
		}
	}
	close(dropFirst)

	// The redialled session inherits the limit: the four commands from the session that just
	// ended are still running, so it has none of its own to give.
	sendMore <- "cmd_b0"
	select {
	case res := <-results:
		if res.ID != "cmd_b0" || res.Outcome != protocol.OutcomeDenied {
			t.Fatalf("a reconnect granted more in-flight commands: %+v", res)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the new session never answered")
	}

	// Once they finish, the ledger that recorded them is the one the new session reads and
	// writes, so nothing it recorded is lost and a re-dispatch replays rather than re-runs.
	close(release)
	for done := false; !done; {
		sendMore <- "cmd_b1"
		select {
		case res := <-results:
			switch {
			case res.ID != "cmd_b1":
				t.Fatalf("unexpected result: %+v", res)
			case res.Outcome == protocol.OutcomeSucceeded:
				done = true
			case res.Outcome != protocol.OutcomeDenied:
				t.Fatalf("cmd_b1: %+v", res)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("cmd_b1 never answered")
		}
	}
	// Polled rather than read once: the released commands record as they finish, and only one
	// of them had to finish for cmd_b1 to get its slot. What is being asserted is that the
	// ledger never loses an entry, not how soon each arrives.
	var recorded map[string]struct {
		Outcome string `json:"outcome"`
	}
	want := []string{"cmd_a0", "cmd_a1", "cmd_a2", "cmd_a3", "cmd_b1"}
	deadline := time.Now().Add(15 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "commands.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &recorded); err != nil {
			t.Fatal(err)
		}
		missing := ""
		for _, id := range want {
			if recorded[id].Outcome == "" {
				missing = id
			}
		}
		if missing == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ledger forgot %s after a reconnect: %v", missing, recorded)
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(sendMore)
}
