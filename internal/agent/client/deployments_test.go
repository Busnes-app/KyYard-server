package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	return protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: endpoint, Project: "shop", Revision: 1, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Services: []protocol.DeploymentService{{Name: "web", ContainerName: "shop-web-1", ImageID: "sha256:" + strings.Repeat("a", 64), Replaces: protocol.InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000}, Env: map[string]string{"TOKEN": "agent-secret-canary"}, Mounts: []protocol.Mount{}}}}
}

func testRemoval(endpoint string) protocol.RemovalRequest {
	return protocol.RemovalRequest{Deployment: "6c5e4f3a-1b0d-4e9f-8a7b-4f5a6b7c8d9e", RequestID: "fedcba9876543210fedcba9876543210", Endpoint: endpoint, Project: "shop", IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Containers: []protocol.RemovalTarget{{Service: "web", Target: protocol.InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000}}}}
}

func TestDeployerRunsOffTheSessionAndDeliversToTheCurrentOne(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	finish := make(chan struct{})
	var seen protocol.DeploymentRequest
	var mu sync.Mutex
	opts := &Options{Deploy: func(ctx context.Context, req protocol.DeploymentRequest, _ func()) protocol.DeploymentResult {
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
	session1, cancel1 := context.WithCancel(context.Background())
	out1 := make(chan outFrame, 4)
	detach1 := d.attach(session1, out1)
	req := testRequest("ep_1")
	raw, _ := json.Marshal(req)
	d.handleApply(session1, "ep_1", raw, out1)
	<-started
	cancel1() // the socket dropped; the run must continue
	detach1()
	session2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	out2 := make(chan outFrame, 4)
	defer d.attach(session2, out2)()
	close(finish)
	// No resend: the run itself delivers to the session connected when it finishes.
	var f outFrame
	select {
	case f = <-out2:
	case <-time.After(5 * time.Second):
		t.Fatal("result not delivered to the current session")
	}
	var res protocol.DeploymentResult
	if f.Type != protocol.TypeDeploymentResult || decodeResult(f, &res) != nil || res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("result: %+v", f)
	}
	if len(out1) != 0 {
		t.Fatal("result sent to the dead session")
	}
	stored, err := os.ReadFile(filepath.Join(dir, "deployments.json"))
	if err != nil || !strings.Contains(string(stored), req.Deployment) || strings.Contains(string(stored), "agent-secret-canary") {
		t.Fatalf("ledger: %v %s", err, stored)
	}
	if info, err := os.Stat(filepath.Join(dir, "deployments.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode: %v %v", info, err)
	}
	mu.Lock()
	if len(seen.Services[0].Env) != 0 {
		t.Fatal("env values survived the run")
	}
	mu.Unlock()
	// Replay: the same ID answers from the ledger without running again.
	ran := false
	opts.Deploy = func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		ran = true
		return protocol.DeploymentResult{}
	}
	d.handleApply(session2, "ep_1", raw, out2)
	f = <-out2
	if ran || f.Type != protocol.TypeDeploymentResult {
		t.Fatal("replay ran the deployment again")
	}
}

// A frame for the deployment already running gets no answer (a refusal would settle the live
// row); a different deployment is refused.
func TestDeployerStaysSilentForTheRunningDeployment(t *testing.T) {
	finish := make(chan struct{})
	started := make(chan struct{})
	d := newDeployer(context.Background(), t.TempDir(), &Options{Deploy: func(_ context.Context, req protocol.DeploymentRequest, _ func()) protocol.DeploymentResult {
		close(started)
		<-finish
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan outFrame, 8)
	defer d.attach(ctx, out)()
	req := testRequest("ep_1")
	raw, _ := json.Marshal(req)
	d.handleApply(ctx, "ep_1", raw, out)
	<-started
	d.handleApply(ctx, "ep_1", raw, out)
	// Still silent once the frame's deadline has passed: the running ID is checked first.
	late := req
	late.Deadline = time.Now().Add(-time.Minute)
	rawLate, _ := json.Marshal(late)
	d.handleApply(ctx, "ep_1", rawLate, out)
	other := testRequest("ep_1")
	other.Deployment = "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c"
	raw2, _ := json.Marshal(other)
	d.handleApply(ctx, "ep_1", raw2, out)
	var res protocol.DeploymentResult
	if decodeResult(<-out, &res) != nil || res.Deployment != other.Deployment || res.Outcome != protocol.OutcomeDenied {
		t.Fatalf("different deployment: %+v", res)
	}
	close(finish)
	if decodeResult(<-out, &res) != nil || res.Deployment != req.Deployment || res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("running deployment answered with %+v", res)
	}
	select {
	case f := <-out:
		t.Fatalf("extra answer: %s", payloadText(f))
	case <-time.After(50 * time.Millisecond):
	}
}

// Cancelling the root lets the run finish; wait returns only once its result is on disk.
func TestDeployerWaitsForTheRunToRecord(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	root, cancel := context.WithCancel(context.Background())
	d := newDeployer(root, dir, &Options{Deploy: func(ctx context.Context, req protocol.DeploymentRequest, _ func()) protocol.DeploymentResult {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // a runtime winding down after the cancel
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeTimedOut, Code: protocol.ResultStepFailed, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}})
	req := testRequest("ep_1")
	raw, _ := json.Marshal(req)
	d.handleApply(context.Background(), "ep_1", raw, make(chan outFrame, 1))
	<-started
	cancel()
	d.wait()
	stored, err := os.ReadFile(filepath.Join(dir, "deployments.json"))
	if err != nil || !strings.Contains(string(stored), req.Deployment) || !strings.Contains(string(stored), protocol.OutcomeTimedOut) {
		t.Fatalf("ledger after wait: %v %s", err, stored)
	}
}

// A result the server could not read is recorded and sent as unknown with a fixed detail.
func TestDeployerReplacesAnUnreadableResult(t *testing.T) {
	dir := t.TempDir()
	d := newDeployer(context.Background(), dir, &Options{Deploy: func(_ context.Context, req protocol.DeploymentRequest, _ func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: "exploded", Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}})
	out := make(chan outFrame, 1)
	defer d.attach(context.Background(), out)()
	req := testRequest("ep_1")
	raw, _ := json.Marshal(req)
	d.handleApply(context.Background(), "ep_1", raw, out)
	var res protocol.DeploymentResult
	select {
	case f := <-out:
		if decodeResult(f, &res) != nil {
			t.Fatal(payloadText(f))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
	if res.Deployment != req.Deployment || res.Outcome != protocol.OutcomeUnknown || res.Code != protocol.ResultUnreadable || res.RequestID != req.RequestID || res.Validate() != nil {
		t.Fatalf("result: %+v", res)
	}
	d.wait()
	stored, err := os.ReadFile(filepath.Join(dir, "deployments.json"))
	if err != nil || strings.Contains(string(stored), "exploded") || !strings.Contains(string(stored), "\"code\":\"unreadable\"") {
		t.Fatalf("ledger: %v %s", err, stored)
	}
}

func TestDeployerRefusals(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	opts := &Options{Deploy: func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		calls++
		time.Sleep(200 * time.Millisecond)
		return protocol.DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}}
	d := newDeployer(context.Background(), dir, opts)
	out := make(chan outFrame, 8)
	defer d.attach(context.Background(), out)()
	read := func() protocol.DeploymentResult {
		f := <-out
		var res protocol.DeploymentResult
		_ = decodeResult(f, &res)
		return res
	}
	// Foreign endpoint.
	raw, _ := json.Marshal(testRequest("someone-else"))
	d.handleApply(context.Background(), "ep_1", raw, out)
	if res := read(); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultWrongEndpoint || res.RequestID != "0123456789abcdef0123456789abcdef" || calls != 0 {
		t.Fatalf("foreign: %+v", res)
	}
	// Invalid request.
	bad := testRequest("ep_1")
	bad.Services = nil
	raw, _ = json.Marshal(bad)
	d.handleApply(context.Background(), "ep_1", raw, out)
	if res := read(); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || calls != 0 {
		t.Fatalf("invalid: %+v", res)
	}
	// Busy: a second request while one runs is denied.
	req := testRequest("ep_1")
	raw, _ = json.Marshal(req)
	d.handleApply(context.Background(), "ep_1", raw, out)
	second := testRequest("ep_1")
	second.Deployment = "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c"
	raw2, _ := json.Marshal(second)
	d.handleApply(context.Background(), "ep_1", raw2, out)
	first, next := read(), read()
	busy := first
	if next.Outcome == protocol.OutcomeDenied {
		busy = next
	}
	if busy.Outcome != protocol.OutcomeDenied || busy.Code != protocol.ResultBusy {
		t.Fatalf("one of two concurrent applies must be denied busy: %+v %+v", first, next)
	}
	if calls != 1 {
		t.Fatalf("deploy ran %d times", calls)
	}
	// No runtime.
	d2 := newDeployer(context.Background(), t.TempDir(), &Options{})
	d2.handleApply(context.Background(), "ep_1", raw, out)
	if res := read(); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest {
		t.Fatalf("no runtime: %+v", res)
	}
}

func TestDeployerResendsPersistedResults(t *testing.T) {
	dir := t.TempDir()
	ledger := map[string]deploymentEntry{"3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b": {Result: protocol.DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: protocol.OutcomeFailed, Code: protocol.ResultStepFailed, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: time.Now().UTC()}, "old": {Result: protocol.DeploymentResult{Deployment: "old"}, Finished: time.Now().Add(-25 * time.Hour)}}
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

// The session advertises each capability only with its runtime, re-sends the ledger after hello,
// answers an apply (and a removal when it can), and closes on an oversized frame.
func TestSessionCarriesDeployments(t *testing.T) {
	remove := func(_ context.Context, req protocol.RemovalRequest, _ func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}
	t.Run("apply only", func(t *testing.T) { sessionCarriesDeployments(t, nil) })
	t.Run("apply and remove", func(t *testing.T) { sessionCarriesDeployments(t, remove) })
}

func sessionCarriesDeployments(t *testing.T, remove func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	dir := t.TempDir()
	const saved = "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c"
	ledger, _ := json.Marshal(map[string]deploymentEntry{saved: {Result: protocol.DeploymentResult{Deployment: saved, Outcome: protocol.OutcomeFailed, Code: protocol.ResultStepFailed, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: time.Now().UTC()}})
	if err := os.WriteFile(filepath.Join(dir, "deployments.json"), ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	withRemove := remove != nil
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
			removal := testRemoval("endpoint")
			removal.Deployment = "5b4d3e2f-0a9c-4d8e-9f7a-3e4f5a6b7c8d"
			advertised, advertisedRemove, resent, applied, removed := false, false, false, false, !withRemove
			for !applied || !removed {
				f, err := read(ctx, conn)
				if err != nil {
					return err
				}
				switch f.Type {
				case protocol.TypeHello:
					var hello protocol.Hello
					_ = json.Unmarshal(f.Payload, &hello)
					advertised = slices.Contains(hello.Capabilities, protocol.CapabilityDeploymentApply) && slices.Contains(hello.Capabilities, protocol.CapabilityDeploymentPull)
					advertisedRemove = slices.Contains(hello.Capabilities, protocol.CapabilityDeploymentRemove)
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
						if withRemove {
							if err := write(ctx, conn, protocol.TypeDeploymentRemove, removal); err != nil {
								return err
							}
						}
					case withRemove && res.Deployment == removal.Deployment && res.Outcome == protocol.OutcomeSucceeded:
						removed = true
					default:
						return errors.New("unexpected result " + string(f.Payload))
					}
				}
			}
			if !advertised || !resent || advertisedRemove != withRemove {
				return errors.New("capabilities not advertised as configured or ledger not re-sent")
			}
			oversized := protocol.TypeDeploymentApply
			if withRemove {
				oversized = protocol.TypeDeploymentRemove
			}
			// A payload of exactly the cap is read and answered; one byte more closes the session.
			if err := write(ctx, conn, oversized, strings.Repeat("a", protocol.MaxDeploymentRequestBytes-2)); err != nil {
				return err
			}
			for {
				f, err := read(ctx, conn)
				if err != nil {
					return err
				}
				var res protocol.DeploymentResult
				if f.Type == protocol.TypeDeploymentResult && json.Unmarshal(f.Payload, &res) == nil && res.Outcome == protocol.OutcomeDenied {
					break
				}
			}
			if err := write(ctx, conn, oversized, strings.Repeat("a", protocol.MaxDeploymentRequestBytes-1)); err != nil {
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
	opts := Options{HTTPClient: stub.Client(), IdentityDir: dir, Log: log.New(io.Discard, "", 0), Deploy: func(_ context.Context, req protocol.DeploymentRequest, _ func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}, Remove: remove}
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
		d.handleApply(ctx, "ep_1", raw, full)
		d.handleApply(ctx, "ep_1", []byte("{"), full)
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

func removed(req protocol.RemovalRequest) protocol.DeploymentResult {
	return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
}

func readResult(t *testing.T, out <-chan outFrame) protocol.DeploymentResult {
	t.Helper()
	select {
	case f := <-out:
		var res protocol.DeploymentResult
		if f.Type != protocol.TypeDeploymentResult || decodeResult(f, &res) != nil {
			t.Fatalf("frame: %+v", f)
		}
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
	return protocol.DeploymentResult{}
}

// A removal runs Options.Remove on the root context, is recorded before it is delivered to the
// session current when it finishes, and replays from the ledger without running again.
func TestDeployerRunsARemoval(t *testing.T) {
	dir := t.TempDir()
	started, finish := make(chan struct{}), make(chan struct{})
	var ranCtx context.Context
	opts := &Options{Remove: func(ctx context.Context, req protocol.RemovalRequest, _ func()) protocol.DeploymentResult {
		ranCtx = ctx
		close(started)
		<-finish
		return removed(req)
	}}
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	d := newDeployer(root, dir, opts)
	session1, cancel1 := context.WithCancel(context.Background())
	out1 := make(chan outFrame, 4)
	detach1 := d.attach(session1, out1)
	req := testRemoval("ep_1")
	raw, _ := json.Marshal(req)
	d.handleRemoval(session1, "ep_1", raw, out1)
	<-started
	cancel1()
	detach1()
	session2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	out2 := make(chan outFrame, 4)
	defer d.attach(session2, out2)()
	close(finish)
	res := readResult(t, out2)
	if res.Deployment != req.Deployment || res.Outcome != protocol.OutcomeSucceeded || len(out1) != 0 {
		t.Fatalf("result: %+v, dead session got %d", res, len(out1))
	}
	if ranCtx.Err() != nil {
		t.Fatal("removal ran on the session's context")
	}
	// The ledger is written before the frame is sent.
	stored, err := os.ReadFile(filepath.Join(dir, "deployments.json"))
	if err != nil || !strings.Contains(string(stored), req.Deployment) {
		t.Fatalf("ledger: %v %s", err, stored)
	}
	opts.Remove = func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult {
		t.Error("replay ran the removal again")
		return protocol.DeploymentResult{}
	}
	d.handleRemoval(session2, "ep_1", raw, out2)
	if res := readResult(t, out2); res.Deployment != req.Deployment || res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("replay: %+v", res)
	}
}

// A removal result persisted by an earlier process replays without touching the runtime.
func TestDeployerReplaysAPersistedRemoval(t *testing.T) {
	dir := t.TempDir()
	req := testRemoval("ep_1")
	ledger, _ := json.Marshal(map[string]deploymentEntry{req.Deployment: {Result: protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeFailed, Detail: "x", Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: time.Now().UTC()}})
	if err := os.WriteFile(filepath.Join(dir, "deployments.json"), ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDeployer(context.Background(), dir, &Options{Remove: func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult {
		t.Error("replay ran the removal")
		return protocol.DeploymentResult{}
	}})
	out := make(chan outFrame, 1)
	raw, _ := json.Marshal(req)
	d.handleRemoval(context.Background(), "ep_1", raw, out)
	if res := readResult(t, out); res.Deployment != req.Deployment || res.Outcome != protocol.OutcomeFailed {
		t.Fatalf("replay: %+v", res)
	}
}

// Apply and removal share one slot: a removal while an apply runs is refused, the apply still
// answers, and a repeat of the running removal is silent.
func TestDeployerSharesTheSlotWithRemoval(t *testing.T) {
	applyStarted, applyFinish := make(chan struct{}), make(chan struct{})
	removeStarted, removeFinish := make(chan struct{}), make(chan struct{})
	d := newDeployer(context.Background(), t.TempDir(), &Options{
		Deploy: func(_ context.Context, req protocol.DeploymentRequest, _ func()) protocol.DeploymentResult {
			close(applyStarted)
			<-applyFinish
			return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
		},
		Remove: func(_ context.Context, req protocol.RemovalRequest, _ func()) protocol.DeploymentResult {
			close(removeStarted)
			<-removeFinish
			return removed(req)
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan outFrame, 8)
	defer d.attach(ctx, out)()
	apply := testRequest("ep_1")
	rawApply, _ := json.Marshal(apply)
	removal := testRemoval("ep_1")
	rawRemoval, _ := json.Marshal(removal)
	d.handleApply(ctx, "ep_1", rawApply, out)
	<-applyStarted
	d.handleRemoval(ctx, "ep_1", rawRemoval, out)
	if res := readResult(t, out); res.Deployment != removal.Deployment || res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultBusy || res.RequestID != removal.RequestID {
		t.Fatalf("removal during apply: %+v", res)
	}
	close(applyFinish)
	if res := readResult(t, out); res.Deployment != apply.Deployment || res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("apply: %+v", res)
	}
	d.wait()
	d.handleRemoval(ctx, "ep_1", rawRemoval, out)
	<-removeStarted
	d.handleRemoval(ctx, "ep_1", rawRemoval, out)
	late := removal
	late.Deadline = time.Now().Add(-time.Minute)
	rawLate, _ := json.Marshal(late)
	d.handleRemoval(ctx, "ep_1", rawLate, out)
	// An apply while the removal runs is refused too.
	other := testRequest("ep_1")
	other.Deployment = "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c"
	rawOther, _ := json.Marshal(other)
	d.handleApply(ctx, "ep_1", rawOther, out)
	if res := readResult(t, out); res.Deployment != other.Deployment || res.Outcome != protocol.OutcomeDenied {
		t.Fatalf("apply during removal: %+v", res)
	}
	close(removeFinish)
	if res := readResult(t, out); res.Deployment != removal.Deployment || res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("removal: %+v", res)
	}
	select {
	case f := <-out:
		t.Fatalf("extra answer: %s", payloadText(f))
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDeployerRemovalRefusals(t *testing.T) {
	out := make(chan outFrame, 4)
	raw, _ := json.Marshal(testRemoval("ep_1"))
	d := newDeployer(context.Background(), t.TempDir(), &Options{})
	d.handleRemoval(context.Background(), "ep_1", raw, out)
	if res := readResult(t, out); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest {
		t.Fatalf("no runtime: %+v", res)
	}
	d = newDeployer(context.Background(), t.TempDir(), &Options{Remove: func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult {
		t.Error("refused removal ran")
		return protocol.DeploymentResult{}
	}})
	foreign, _ := json.Marshal(testRemoval("someone-else"))
	d.handleRemoval(context.Background(), "ep_1", foreign, out)
	if res := readResult(t, out); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultWrongEndpoint {
		t.Fatalf("foreign: %+v", res)
	}
	bad := testRemoval("ep_1")
	bad.Containers = nil
	rawBad, _ := json.Marshal(bad)
	d.handleRemoval(context.Background(), "ep_1", rawBad, out)
	if res := readResult(t, out); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest {
		t.Fatalf("invalid: %+v", res)
	}
	d.handleRemoval(context.Background(), "ep_1", []byte("{"), out)
	if res := readResult(t, out); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || res.RequestID != "" {
		t.Fatalf("unreadable: %+v", res)
	}
}

// A credentialed pull leaves the secret nowhere after the run: not in the ledger file, not in
// the in-memory ledger, and the runner has cleared the frame's credential map.
func TestDeployerNeverKeepsTheRegistryCredential(t *testing.T) {
	const secret = "topsecret"
	dir := t.TempDir()
	var held map[string]protocol.RegistryAuth
	digest := "sha256:" + strings.Repeat("d", 64)
	d := newDeployer(context.Background(), dir, &Options{Deploy: func(_ context.Context, req protocol.DeploymentRequest, _ func()) protocol.DeploymentResult {
		held = req.Registries
		s := req.Services[0]
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded,
			Steps:    []protocol.DeploymentStep{{Service: s.Name, Step: protocol.StepPull, Outcome: protocol.OutcomeSucceeded}},
			Services: []protocol.DeploymentIdentity{{Service: s.Name, ContainerID: strings.Repeat("e", 64), ImageID: "sha256:" + strings.Repeat("f", 64), CreatedUnix: 1700000100, ImageDigest: digest}}}
	}})
	out := make(chan outFrame, 1)
	defer d.attach(context.Background(), out)()
	req := testRequest("ep_1")
	req.Services[0].ImageID = ""
	req.Services[0].Pull = &protocol.ImagePull{Reference: "ghcr.io/org/web@" + digest, Digest: digest}
	req.Registries = map[string]protocol.RegistryAuth{"ghcr.io": {Username: "u", Secret: secret}}
	if err := req.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(req)
	d.handleApply(context.Background(), "ep_1", raw, out)
	res := readResult(t, out)
	d.wait()
	if res.Outcome != protocol.OutcomeSucceeded || strings.Contains(payloadText(resultFrame(res)), secret) {
		t.Fatalf("result: %+v", res)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "deployments.json"))
	if err != nil || strings.Contains(string(stored), secret) || strings.Contains(string(stored), "registries") {
		t.Fatalf("ledger: %v %s", err, stored)
	}
	// Results only: an entry holding the request would pass the scans above once cleared.
	var entries map[string]map[string]json.RawMessage
	if err := json.Unmarshal(stored, &entries); err != nil || len(entries) != 1 {
		t.Fatalf("ledger shape: %v %s", err, stored)
	}
	for id, e := range entries {
		keys := slices.Sorted(maps.Keys(e))
		if !slices.Equal(keys, []string{"finished", "result"}) {
			t.Fatalf("ledger entry %s has keys %v", id, keys)
		}
	}
	d.mu.Lock()
	memory, _ := json.Marshal(d.done)
	d.mu.Unlock()
	if strings.Contains(string(memory), secret) || strings.Contains(string(memory), "registries") {
		t.Fatalf("in-memory ledger: %s", memory)
	}
	// held is the frame's own map, not a copy: empty proves the runner let go of the credential.
	if held == nil || len(held) != 0 {
		t.Fatalf("the runner kept the credential map: %v", held)
	}
}

// A skewed frame is answered failed with the fixed detail, runs nothing and is not recorded.
func TestDeployerReportsClockSkewAsFailed(t *testing.T) {
	ran := false
	d := newDeployer(context.Background(), t.TempDir(), &Options{Deploy: func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		ran = true
		return protocol.DeploymentResult{}
	}})
	out := make(chan outFrame, 1)
	req := testRequest("ep_1")
	req.IssuedAt = time.Now().Add(protocol.MaxClockSkew + time.Minute)
	req.Deadline = req.IssuedAt.Add(time.Minute)
	raw, _ := json.Marshal(req)
	d.handleApply(context.Background(), "ep_1", raw, out)
	var res protocol.DeploymentResult
	if decodeResult(<-out, &res) != nil || res.Deployment != req.Deployment || res.Outcome != protocol.OutcomeFailed || res.Code != protocol.ResultClockSkew || res.RequestID != req.RequestID || res.Validate() != nil {
		t.Fatalf("skewed frame: %+v", res)
	}
	d.mu.Lock()
	_, recorded := d.done[req.Deployment]
	d.mu.Unlock()
	if ran || recorded {
		t.Fatalf("ran=%v recorded=%v", ran, recorded)
	}
}

// A run that began changing the host and never recorded a result (the agent restarted) is
// reported unknown after the restart, re-sent, and answered from the ledger, never run again.
func TestDeployerStartedMarkerSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	marked := make(chan struct{})
	release := make(chan struct{})
	first := newDeployer(context.Background(), dir, &Options{Deploy: func(_ context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult {
		started()
		close(marked)
		<-release
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}})
	req := testRequest("ep_1")
	raw, _ := json.Marshal(req)
	first.handleApply(context.Background(), "ep_1", raw, make(chan outFrame, 1))
	<-marked
	stored, err := os.ReadFile(filepath.Join(dir, "deployments.json"))
	if err != nil || !strings.Contains(string(stored), req.Deployment) || !strings.Contains(string(stored), `"started"`) || !strings.Contains(string(stored), "\"request_id\":\"0123456789abcdef0123456789abcdef\"") || strings.Contains(string(stored), "agent-secret-canary") {
		t.Fatalf("started entry: %v %s", err, stored)
	}
	// The agent restarts here: a new deployer reads the ledger the first one left.
	var ran atomic.Bool
	second := newDeployer(context.Background(), dir, &Options{Deploy: func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		ran.Store(true)
		return protocol.DeploymentResult{}
	}})
	out := make(chan outFrame, 4)
	second.resend(context.Background(), out)
	var res protocol.DeploymentResult
	if decodeResult(<-out, &res) != nil || res.Deployment != req.Deployment || res.Outcome != protocol.OutcomeUnknown || res.Code != protocol.ResultRestarted || res.RequestID != req.RequestID || res.Validate() != nil {
		t.Fatalf("re-sent: %+v", res)
	}
	second.handleApply(context.Background(), "ep_1", raw, out)
	if decodeResult(<-out, &res) != nil || res.Outcome != protocol.OutcomeUnknown {
		t.Fatalf("replayed: %+v", res)
	}
	if ran.Load() {
		t.Fatal("a re-sent frame ran again after a restart")
	}
	close(release)
	first.wait()
}

// Pruning never drops a run that began and has no result, however old or full the ledger, and
// re-sending skips it: it has nothing to send yet.
func TestDeployerPruneKeepsAStartedRun(t *testing.T) {
	d := newDeployer(context.Background(), t.TempDir(), &Options{})
	for i := range deploymentLedgerMax + 5 {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		d.done[id] = deploymentEntry{Result: protocol.DeploymentResult{Deployment: id, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: time.Now().UTC()}
	}
	running := "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b"
	d.begin(running, "")
	e := d.done[running]
	e.Started = time.Now().UTC().Add(-2 * deploymentLedgerLife)
	d.done[running] = e
	d.prune()
	if e, ok := d.done[running]; !ok || !e.pending() {
		t.Fatal("pruning dropped a started run")
	}
	if len(d.done) != deploymentLedgerMax+1 {
		t.Fatalf("ledger holds %d entries", len(d.done))
	}
	out := make(chan outFrame, 2*deploymentLedgerMax)
	d.resend(context.Background(), out)
	if len(out) != deploymentLedgerMax {
		t.Fatalf("re-sent %d results", len(out))
	}
	for range deploymentLedgerMax {
		var res protocol.DeploymentResult
		if decodeResult(<-out, &res) != nil || res.Deployment == running {
			t.Fatalf("re-sent the started run: %+v", res)
		}
	}
}

// A remembered result is replayed whatever the frame carrying its ID: a re-sent frame for a
// run settled unknown after a restart is answered from the ledger even past its deadline.
func TestDeployerReplaysTheLedgerPastTheDeadline(t *testing.T) {
	dir := t.TempDir()
	req := testRequest("ep_1")
	newDeployer(context.Background(), dir, &Options{}).begin(req.Deployment, req.RequestID)
	var ran atomic.Bool
	d := newDeployer(context.Background(), dir, &Options{Deploy: func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		ran.Store(true)
		return protocol.DeploymentResult{}
	}})
	req.IssuedAt, req.Deadline = time.Now().Add(-4*time.Minute), time.Now().Add(-time.Minute)
	if req.Validate(time.Now()) == nil {
		t.Fatal("the frame must be past its deadline")
	}
	raw, _ := json.Marshal(req)
	out := make(chan outFrame, 1)
	d.handleApply(context.Background(), "ep_1", raw, out)
	var res protocol.DeploymentResult
	if decodeResult(<-out, &res) != nil || res.Deployment != req.Deployment || res.Outcome != protocol.OutcomeUnknown || res.Code != protocol.ResultRestarted || res.RequestID != req.RequestID {
		t.Fatalf("expired re-sent frame: %+v", res)
	}
	if ran.Load() {
		t.Fatal("a remembered deployment ran again")
	}
}

// The request ID is echoed on every answer the deployer sends, run or refused, and logged beside
// the deployment ID at receipt, start and finish; a malformed one is neither echoed nor logged.
func TestDeployerEchoesAndLogsTheRequestID(t *testing.T) {
	var logs strings.Builder
	d := newDeployer(context.Background(), t.TempDir(), &Options{Log: log.New(&logs, "", 0), Deploy: func(_ context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult {
		started()
		// The runtime's result need not carry it: the deployer echoes the frame's.
		return protocol.DeploymentResult{Deployment: req.Deployment, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	}})
	out := make(chan outFrame, 4)
	defer d.attach(context.Background(), out)()
	req := testRequest("ep_1")
	raw, _ := json.Marshal(req)
	d.handleApply(context.Background(), "ep_1", raw, out)
	if res := readResult(t, out); res.Outcome != protocol.OutcomeSucceeded || res.RequestID != req.RequestID || res.Validate() != nil {
		t.Fatalf("result: %+v", res)
	}
	d.wait()
	for _, event := range []string{"received", "started", "finished succeeded"} {
		if want := "deployment " + req.Deployment + " (request " + req.RequestID + "): " + event; !strings.Contains(logs.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, logs.String())
		}
	}
	bad := testRequest("ep_1")
	bad.Deployment, bad.RequestID = "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c", "a b\ninjected"
	raw, _ = json.Marshal(bad)
	d.handleApply(context.Background(), "ep_1", raw, out)
	if res := readResult(t, out); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || res.RequestID != "" {
		t.Fatalf("malformed request id: %+v", res)
	}
	if strings.Contains(logs.String(), "injected") || strings.Contains(logs.String(), bad.Deployment) {
		t.Fatalf("a refused frame was logged:\n%s", logs.String())
	}
}
