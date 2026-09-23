// internal/agent/client/deployments_test.go
package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/coder/websocket"
)

// outFrame.Payload is any; the session loop marshals it, so the tests do the same.
func payloadText(f outFrame) string {
	raw, _ := json.Marshal(f.Payload)
	return string(raw)
}

func decodeResult(f outFrame, res *protocol.DeploymentResult) error {
	return json.Unmarshal([]byte(payloadText(f)), res)
}

func testRequest(endpoint string) protocol.DeploymentRequest {
	return protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: endpoint, Project: "shop", Revision: 1, Deadline: time.Now().Add(5 * time.Minute), Services: []protocol.DeploymentService{{Name: "web", ContainerName: "shop-web-1", ImageID: "sha256:" + strings.Repeat("a", 64), Replaces: protocol.InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000}, Env: map[string]string{"TOKEN": "agent-secret-canary"}}}}
}

func TestDeployerRunsOffTheSessionAndPersistsBeforeSending(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	finish := make(chan struct{})
	var seen protocol.DeploymentRequest
	var mu sync.Mutex
	opts := &Options{Deploy: func(ctx context.Context, req protocol.DeploymentRequest) protocol.DeploymentResult {
		mu.Lock()
		seen = req
		mu.Unlock()
		close(started)
		<-finish
		if ctx.Err() != nil {
			return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeUnknown, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
		}
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}}
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	d := newDeployer(root, dir, opts)
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	out := make(chan outFrame, 4)
	req := testRequest("ep_1")
	raw, _ := json.Marshal(req)
	d.handle(sessionCtx, "ep_1", raw, out)
	<-started
	cancelSession() // the socket dropped; the run must continue
	close(finish)
	// The run releases its slot after recording; its own send may be dropped with the session,
	// so the next session's re-send is what carries it.
	d.busy <- struct{}{}
	<-d.busy
	d.resend(context.Background(), out)
	f := <-out
	var res protocol.DeploymentResult
	if f.Type != protocol.TypeDeploymentResult || decodeResult(f, &res) != nil || res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("result: %+v", f)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "deployments.json"))
	if err != nil || !strings.Contains(string(stored), req.Deployment) || strings.Contains(string(stored), "agent-secret-canary") {
		t.Fatalf("ledger: %v %s", err, stored)
	}
	mu.Lock()
	if len(seen.Services[0].Env) != 0 {
		t.Fatal("env values survived the run")
	}
	mu.Unlock()
	for len(out) > 0 {
		<-out
	}
	// Replay: the same ID answers from the ledger without running again.
	ran := false
	opts.Deploy = func(context.Context, protocol.DeploymentRequest) protocol.DeploymentResult {
		ran = true
		return protocol.DeploymentResult{}
	}
	d.handle(context.Background(), "ep_1", raw, out)
	f = <-out
	if ran || f.Type != protocol.TypeDeploymentResult {
		t.Fatal("replay ran the deployment again")
	}
}

func TestDeployerRefusals(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	opts := &Options{Deploy: func(context.Context, protocol.DeploymentRequest) protocol.DeploymentResult {
		calls++
		time.Sleep(200 * time.Millisecond)
		return protocol.DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}}
	d := newDeployer(context.Background(), dir, opts)
	out := make(chan outFrame, 8)
	read := func() protocol.DeploymentResult {
		f := <-out
		var res protocol.DeploymentResult
		_ = decodeResult(f, &res)
		return res
	}
	// Foreign endpoint.
	raw, _ := json.Marshal(testRequest("someone-else"))
	d.handle(context.Background(), "ep_1", raw, out)
	if res := read(); res.Outcome != protocol.OutcomeDenied || calls != 0 {
		t.Fatalf("foreign: %+v", res)
	}
	// Invalid request.
	bad := testRequest("ep_1")
	bad.Services = nil
	raw, _ = json.Marshal(bad)
	d.handle(context.Background(), "ep_1", raw, out)
	if res := read(); res.Outcome != protocol.OutcomeDenied || calls != 0 {
		t.Fatalf("invalid: %+v", res)
	}
	// Busy: a second request while one runs is denied.
	req := testRequest("ep_1")
	raw, _ = json.Marshal(req)
	d.handle(context.Background(), "ep_1", raw, out)
	second := testRequest("ep_1")
	second.Deployment = "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c"
	raw2, _ := json.Marshal(second)
	d.handle(context.Background(), "ep_1", raw2, out)
	first, next := read(), read()
	if first.Outcome != protocol.OutcomeDenied && next.Outcome != protocol.OutcomeDenied {
		t.Fatalf("one of two concurrent applies must be denied: %+v %+v", first, next)
	}
	if calls != 1 {
		t.Fatalf("deploy ran %d times", calls)
	}
	// No runtime.
	d2 := newDeployer(context.Background(), t.TempDir(), &Options{})
	d2.handle(context.Background(), "ep_1", raw, out)
	if res := read(); res.Outcome != protocol.OutcomeDenied {
		t.Fatalf("no runtime: %+v", res)
	}
}

func TestDeployerResendsPersistedResults(t *testing.T) {
	dir := t.TempDir()
	ledger := map[string]deploymentEntry{"3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b": {Result: protocol.DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: protocol.OutcomeFailed, Detail: "x", Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: time.Now().UTC()}, "old": {Result: protocol.DeploymentResult{Deployment: "old"}, Finished: time.Now().Add(-25 * time.Hour)}}
	raw, _ := json.Marshal(ledger)
	if err := os.WriteFile(filepath.Join(dir, "deployments.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDeployer(context.Background(), dir, &Options{})
	out := make(chan outFrame, 4)
	d.resend(context.Background(), out)
	select {
	case f := <-out:
		if f.Type != protocol.TypeDeploymentResult || !strings.Contains(payloadText(f), "3f2b1c9e") {
			t.Fatalf("resend: %+v", f)
		}
	default:
		t.Fatal("nothing re-sent")
	}
	select {
	case f := <-out:
		t.Fatalf("expired entry re-sent: %+v", f)
	default:
	}
}

// The session advertises the capability, re-sends the ledger after hello, answers an apply, and
// closes on an oversized one.
func TestSessionCarriesDeployments(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	dir := t.TempDir()
	const saved = "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c"
	ledger, _ := json.Marshal(map[string]deploymentEntry{saved: {Result: protocol.DeploymentResult{Deployment: saved, Outcome: protocol.OutcomeFailed, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: time.Now().UTC()}})
	if err := os.WriteFile(filepath.Join(dir, "deployments.json"), ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	checked := make(chan error, 1)
	var once sync.Once
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		first := false
		once.Do(func() { first = true })
		if !first {
			return
		}
		checked <- func() error {
			if err := write(ctx, conn, protocol.TypeChallenge, protocol.Challenge{Nonce: make([]byte, 32), InstanceFingerprint: "instance", Versions: []int{1}}); err != nil {
				return err
			}
			if _, err := read(ctx, conn); err != nil {
				return err
			}
			if err := write(ctx, conn, protocol.TypeHello, protocol.Hello{State: "approved", HeartbeatSeconds: 5}); err != nil {
				return err
			}
			req := testRequest("endpoint")
			advertised, resent, applied := false, false, false
			for !applied {
				f, err := read(ctx, conn)
				if err != nil {
					return err
				}
				switch f.Type {
				case protocol.TypeHello:
					var hello protocol.Hello
					_ = json.Unmarshal(f.Payload, &hello)
					advertised = slices.Contains(hello.Capabilities, protocol.CapabilityDeploymentApply)
					if err := write(ctx, conn, protocol.TypeDeploymentApply, req); err != nil {
						return err
					}
				case protocol.TypeDeploymentResult:
					var res protocol.DeploymentResult
					_ = json.Unmarshal(f.Payload, &res)
					switch {
					case res.Deployment == saved:
						resent = true
					case res.Deployment == req.Deployment && res.Outcome == protocol.OutcomeSucceeded:
						applied = true
					default:
						return errors.New("unexpected result " + string(f.Payload))
					}
				}
			}
			if !advertised || !resent {
				return errors.New("capability not advertised or ledger not re-sent")
			}
			if err := write(ctx, conn, protocol.TypeDeploymentApply, strings.Repeat("a", protocol.MaxDeploymentRequestBytes)); err != nil {
				return err
			}
			for {
				if _, err := read(ctx, conn); err != nil {
					var ce websocket.CloseError
					if errors.As(err, &ce) && ce.Reason == protocol.CloseProtocol {
						return nil
					}
					return err
				}
			}
		}()
	}))
	defer stub.Close()
	_, key, _ := ed25519.GenerateKey(nil)
	id := &Identity{EndpointID: "endpoint", PrivateKey: key, InstanceFingerprint: "instance", Server: stub.URL}
	opts := Options{HTTPClient: stub.Client(), IdentityDir: dir, Log: log.New(io.Discard, "", 0), Deploy: func(_ context.Context, req protocol.DeploymentRequest) protocol.DeploymentResult {
		return protocol.DeploymentResult{Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, id, opts) }()
	select {
	case err := <-checked:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	<-done
}

// handle runs on the session loop, the only reader of out, so an immediate answer must not wait
// on a full outbound queue.
func TestDeployerNeverBlocksTheSessionLoop(t *testing.T) {
	d := newDeployer(context.Background(), t.TempDir(), &Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	full := make(chan outFrame)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		raw, _ := json.Marshal(testRequest("ep_1"))
		d.handle(ctx, "ep_1", raw, full)
		d.handle(ctx, "ep_1", []byte("{"), full)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("handle blocked on a full outbound queue")
	}
	if f := <-full; f.Type != protocol.TypeDeploymentResult {
		t.Fatalf("answer: %+v", f)
	}
}
