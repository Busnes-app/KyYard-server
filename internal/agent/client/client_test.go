package client_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
	"github.com/Busness-app/ky-primitives/password"
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
	id, err := client.Enroll(ctx, httpClient, httpSrv.URL, dir, "host-x", tok.Token)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := client.Enroll(ctx, httpClient, httpSrv.URL, filepath.Join(t.TempDir(), "again"), "host-y", tok.Token); err == nil {
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
		if _, err := client.Enroll(ctx, stub.Client(), bad, t.TempDir(), "h", strings.Repeat("A", 43)); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if requests != 0 {
		t.Fatalf("%d requests left the agent for refused origins", requests)
	}
	if _, err := client.Enroll(ctx, stub.Client(), stub.URL, t.TempDir(), "h", strings.Repeat("A", 43)); err == nil || requests != 1 {
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
