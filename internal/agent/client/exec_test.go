package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func execRequest() protocol.ExecOpen {
	return protocol.ExecOpen{Stream: "stream", Endpoint: "endpoint", Actor: "actor", Connection: make([]byte, 32), Expires: time.Now().Add(50 * time.Second), Spec: protocol.ExecSpec{Container: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), User: "65534", Argv: []string{"/bin/sh"}}, Size: protocol.TerminalSize{Rows: 24, Columns: 80}}
}
func execFrame(typ string, payload any) protocol.Envelope {
	b, _ := json.Marshal(payload)
	return protocol.Envelope{V: protocol.Version, Type: typ, Payload: b}
}
func nextExecFrame(t *testing.T, out <-chan outFrame, typ string) outFrame {
	t.Helper()
	select {
	case f := <-out:
		if f.Type != typ {
			t.Fatalf("got %s, want %s: %+v", f.Type, typ, f.Payload)
		}
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for " + typ)
		return outFrame{}
	}
}
func waitExecBudget(t *testing.T, b *execBudget, n int) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		b.mu.Lock()
		got := len(b.actors)
		b.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("exec budget did not settle")
}

type pipeExec struct {
	net.Conn
	sizes  chan protocol.TerminalSize
	status protocol.ExecStatus
}

func (p *pipeExec) Resize(ctx context.Context, size protocol.TerminalSize) error {
	select {
	case p.sizes <- size:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *pipeExec) Inspect(context.Context) (protocol.ExecStatus, error) { return p.status, nil }

func TestExecTransportBytesResizeAndExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local, peer := net.Pipe()
	defer peer.Close()
	code := 7
	runtime := &pipeExec{Conn: local, sizes: make(chan protocol.TerminalSize, 2), status: protocol.ExecStatus{ExitCode: &code}}
	out := make(chan outFrame, 8)
	budget := &execBudget{}
	s := newExecStreams(ctx, "endpoint", make([]byte, 32), budget, &Options{Exec: func(context.Context, protocol.ExecSpec) (ExecSession, error) { return runtime, nil }}, out)
	req := execRequest()
	if err := s.handle(execFrame(protocol.TypeExecOpen, req), true); err != nil {
		t.Fatal(err)
	}
	nextExecFrame(t, out, protocol.TypeExecReady)
	if size := <-runtime.sizes; size != req.Size {
		t.Fatal(size)
	}
	data := []byte{0, 255, 0xe2, 0x82, 0xac, 27, '[', 'm'}
	if err := s.handle(execFrame(protocol.TypeExecInput, protocol.ExecData{Stream: req.Stream, Data: data}), true); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(data))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, got) {
		t.Fatalf("input changed: %x", got)
	}
	resize := protocol.TerminalSize{Rows: 41, Columns: 113}
	if err := s.handle(execFrame(protocol.TypeExecResize, protocol.ExecResize{Stream: req.Stream, Size: resize}), true); err != nil {
		t.Fatal(err)
	}
	if size := <-runtime.sizes; size != resize {
		t.Fatal(size)
	}
	if _, err := peer.Write(data); err != nil {
		t.Fatal(err)
	}
	output := nextExecFrame(t, out, protocol.TypeExecOutput).Payload.(protocol.ExecData)
	if !bytes.Equal(data, output.Data) {
		t.Fatalf("output changed: %x", output.Data)
	}
	_ = peer.Close()
	closed := nextExecFrame(t, out, protocol.TypeExecClose).Payload.(protocol.ExecClose)
	if closed.ExitCode == nil || *closed.ExitCode != 7 {
		t.Fatalf("wrong exit: %+v", closed)
	}
	waitExecBudget(t, budget, 0)
	if err := s.handle(execFrame(protocol.TypeExecOpen, req), true); !errors.Is(err, errExecProtocol) {
		t.Fatal("completed grant replay accepted", err)
	}
}

func TestExecGrantRefusalBeforeRuntime(t *testing.T) {
	for name, change := range map[string]func(*protocol.ExecOpen){
		"wrong endpoint":      func(r *protocol.ExecOpen) { r.Endpoint = "other" },
		"previous connection": func(r *protocol.ExecOpen) { r.Connection = bytes.Repeat([]byte{1}, 32) },
		"expired":             func(r *protocol.ExecOpen) { r.Expires = time.Now().Add(-time.Second) },
		"long lived":          func(r *protocol.ExecOpen) { r.Expires = time.Now().Add(time.Hour) },
		"no actor":            func(r *protocol.ExecOpen) { r.Actor = "" },
		"no user":             func(r *protocol.ExecOpen) { r.Spec.User = "" },
		"no image":            func(r *protocol.ExecOpen) { r.Spec.ImageID = "" },
		"bad dimensions":      func(r *protocol.ExecOpen) { r.Size.Rows = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			opts := &Options{Exec: func(context.Context, protocol.ExecSpec) (ExecSession, error) {
				calls.Add(1)
				return nil, errors.New("must not execute")
			}}
			s := newExecStreams(context.Background(), "endpoint", make([]byte, 32), &execBudget{}, opts, make(chan outFrame, 8))
			req := execRequest()
			change(&req)
			if err := s.handle(execFrame(protocol.TypeExecOpen, req), true); !errors.Is(err, errExecProtocol) {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("runtime called")
			}
		})
	}
	s := newExecStreams(context.Background(), "endpoint", make([]byte, 32), &execBudget{}, &Options{}, make(chan outFrame, 8))
	if err := s.handle(execFrame(protocol.TypeExecOpen, execRequest()), true); !errors.Is(err, errExecUnavailable) {
		t.Fatal("nil runtime did not return graceful refusal", err)
	}
	if !errors.Is(s.handle(execFrame(protocol.TypeExecOpen, execRequest()), true), errExecProtocol) {
		t.Fatal("unsupported grant not consumed")
	}
	s.opts.Exec = func(context.Context, protocol.ExecSpec) (ExecSession, error) { panic("pending agent executed") }
	pending := execRequest()
	pending.Stream = "pending-stream"
	if !errors.Is(s.handle(execFrame(protocol.TypeExecOpen, pending), false), errExecProtocol) {
		t.Fatal("pending agent accepted")
	}
	for _, f := range []protocol.Envelope{
		execFrame(protocol.TypeExecInput, protocol.ExecData{Stream: "stream", Data: make([]byte, protocol.MaxExecChunkBytes+1)}),
		execFrame(protocol.TypeExecResize, protocol.ExecResize{Stream: "stream"}),
		{Type: protocol.TypeExecInput, Payload: bytes.Repeat([]byte{' '}, protocol.MaxExecFrameBytes+1)},
	} {
		if s.handle(f, true) == nil {
			t.Fatal("invalid frame accepted")
		}
	}
}

func TestExecAdmissionSurvivesReconnectUntilWorkersFinish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := make(chan struct{})
	defer close(blocked)
	started := make(chan struct{}, 4)
	opts := &Options{Exec: func(context.Context, protocol.ExecSpec) (ExecSession, error) {
		started <- struct{}{}
		<-blocked
		return nil, errors.New("argv secret must not escape")
	}}
	b := &execBudget{}
	out := make(chan outFrame, 8)
	s := newExecStreams(ctx, "endpoint", make([]byte, 32), b, opts, out)
	for i := 0; i < 4; i++ {
		req := execRequest()
		req.Stream += string(rune('a' + i))
		req.Actor += string(rune('a' + i))
		if err := s.handle(execFrame(protocol.TypeExecOpen, req), true); err != nil {
			t.Fatal(err)
		}
		<-started
	}
	// A full endpoint refuses without creating another worker.
	if err := s.handle(execFrame(protocol.TypeExecOpen, execRequest()), true); !errors.Is(err, errExecCapacity) {
		t.Fatal(err)
	}
	cancel()
	replacement := newExecStreams(context.Background(), "endpoint", bytes.Repeat([]byte{1}, 32), b, opts, out)
	req := execRequest()
	req.Connection = bytes.Repeat([]byte{1}, 32)
	if err := replacement.handle(execFrame(protocol.TypeExecOpen, req), true); !errors.Is(err, errExecCapacity) {
		t.Fatal("reconnect reset budget", err)
	}
	waitExecBudget(t, b, 4)
}

func TestExecCancelUnblocksIOAndKeepsExitUnknown(t *testing.T) {
	for _, mode := range []string{"cancel", "input overflow", "disconnect", "unconfirmed EOF"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			local, peer := net.Pipe()
			defer peer.Close()
			rt := &pipeExec{Conn: local, sizes: make(chan protocol.TerminalSize, 1), status: protocol.ExecStatus{Running: true}}
			b := &execBudget{}
			out := make(chan outFrame, 8)
			s := newExecStreams(ctx, "endpoint", make([]byte, 32), b, &Options{Exec: func(context.Context, protocol.ExecSpec) (ExecSession, error) { return rt, nil }}, out)
			req := execRequest()
			if err := s.handle(execFrame(protocol.TypeExecOpen, req), true); err != nil {
				t.Fatal(err)
			}
			nextExecFrame(t, out, protocol.TypeExecReady)
			for i := 0; i < 20; i++ {
				if mode == "unconfirmed EOF" {
					break
				}
				if err := s.handle(execFrame(protocol.TypeExecInput, protocol.ExecData{Stream: req.Stream, Data: []byte("blocked write")}), true); err != nil {
					t.Fatal(err)
				}
				if mode != "input overflow" {
					break
				}
			}
			switch mode {
			case "cancel":
				if err := s.handle(execFrame(protocol.TypeExecCancel, protocol.ExecStream{Stream: req.Stream}), true); err != nil {
					t.Fatal(err)
				}
			case "disconnect":
				cancel()
			case "unconfirmed EOF":
				_ = peer.Close()
			}
			if mode != "disconnect" {
				closed := nextExecFrame(t, out, protocol.TypeExecClose).Payload.(protocol.ExecClose)
				if closed.ExitCode != nil || !strings.Contains(closed.Reason, "unknown") {
					t.Fatal(closed)
				}
			}
			waitExecBudget(t, b, 0)
		})
	}
}

func TestExecSlowOutputIsBoundedAndDisconnectReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local, peer := net.Pipe()
	defer peer.Close()
	rt := &pipeExec{Conn: local, sizes: make(chan protocol.TerminalSize, 1)}
	out := make(chan outFrame, 1)
	b := &execBudget{}
	s := newExecStreams(ctx, "endpoint", make([]byte, 32), b, &Options{Exec: func(context.Context, protocol.ExecSpec) (ExecSession, error) { return rt, nil }}, out)
	if err := s.handle(execFrame(protocol.TypeExecOpen, execRequest()), true); err != nil {
		t.Fatal(err)
	}
	nextExecFrame(t, out, protocol.TypeExecReady)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if _, err := peer.Write(make([]byte, protocol.MaxExecChunkBytes)); err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
		t.Fatal("unbounded output accepted")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("output writer survived disconnect")
	}
	waitExecBudget(t, b, 0)
}

// Exercise the real socket loop: a stalled runtime cannot suppress heartbeats,
// capacity refusal leaves the connection alive, and cancel reaches the worker.
func TestExecSocketRemainsResponsive(t *testing.T) {
	for _, state := range []string{"active", "approved", "offline"} {
		t.Run(state, func(t *testing.T) { testExecSocketRemainsResponsive(t, state) })
	}
}

func testExecSocketRemainsResponsive(t *testing.T, state string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := make(chan struct{})
	stopped := make(chan struct{})
	checked := make(chan error, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			checked <- err
			return
		}
		defer conn.CloseNow()
		if err = write(ctx, conn, protocol.TypeChallenge, protocol.Challenge{Nonce: make([]byte, 32), InstanceFingerprint: "instance", Versions: []int{1}}); err != nil {
			checked <- err
			return
		}
		if _, err = read(ctx, conn); err != nil {
			checked <- err
			return
		}
		if err = write(ctx, conn, protocol.TypeHello, protocol.Hello{State: state, HeartbeatSeconds: 1}); err != nil {
			checked <- err
			return
		}
		req := execRequest()
		if err = write(ctx, conn, protocol.TypeExecOpen, req); err != nil {
			checked <- err
			return
		}
		select {
		case <-started:
		case <-ctx.Done():
			checked <- ctx.Err()
			return
		}
		req.Stream = "second-stream"
		if err = write(ctx, conn, protocol.TypeExecOpen, req); err != nil {
			checked <- err
			return
		}
		beats, refused := 0, false
		for beats < 2 || !refused {
			f, err := read(ctx, conn)
			if err != nil {
				checked <- err
				return
			}
			switch f.Type {
			case protocol.TypeHello:
				var hello protocol.Hello
				_ = json.Unmarshal(f.Payload, &hello)
				if len(hello.Capabilities) != 1 || hello.Capabilities[0] != "container.exec" {
					checked <- errors.New("configured exec runtime not advertised")
					return
				}
			case protocol.TypeHeartbeat:
				beats++
				if err = write(ctx, conn, protocol.TypeHeartbeat, nil); err != nil {
					checked <- err
					return
				}
			case protocol.TypeExecClose:
				var closed protocol.ExecClose
				_ = json.Unmarshal(f.Payload, &closed)
				if closed.Stream != "second-stream" || closed.ExitCode != nil {
					checked <- errors.New("unexpected stream close")
					return
				}
				refused = true
			}
		}
		if err = write(ctx, conn, protocol.TypeExecCancel, protocol.ExecStream{Stream: "stream"}); err != nil {
			checked <- err
			return
		}
		select {
		case <-stopped:
			checked <- nil
		case <-ctx.Done():
			checked <- ctx.Err()
		}
	}))
	defer stub.Close()
	_, key, _ := ed25519.GenerateKey(nil)
	id := &Identity{EndpointID: "endpoint", PrivateKey: key, InstanceFingerprint: "instance", Server: stub.URL}
	target, _ := ConnectURL(stub.URL)
	b := &execBudget{}
	opts := &Options{HTTPClient: stub.Client(), Log: log.New(io.Discard, "", 0), Exec: func(ctx context.Context, _ protocol.ExecSpec) (ExecSession, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}}
	done := make(chan struct{})
	go func() { defer close(done); _ = session(ctx, id, target, opts, openLedger(t.TempDir()), newBudget(), b) }()
	select {
	case err := <-checked:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session survived cancellation")
	}
	waitExecBudget(t, b, 0)
}

func TestExecConsumedGrantCacheIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan outFrame, 8)
	s := newExecStreams(ctx, "endpoint", make([]byte, 32), &execBudget{}, &Options{Exec: func(context.Context, protocol.ExecSpec) (ExecSession, error) { return nil, errors.New("secret argv") }}, out)
	for i := 0; i < 1024; i++ {
		s.seen[fmt.Sprint(i)] = time.Now().Add(time.Minute)
	}
	req := execRequest()
	if err := s.handle(execFrame(protocol.TypeExecOpen, req), true); !errors.Is(err, errExecCapacity) {
		t.Fatal("replay cache grew past ceiling", err)
	}
	// Only expired records may be evicted. A runtime failure still consumes its grant.
	s.seen["0"] = time.Now().Add(-time.Second)
	if err := s.handle(execFrame(protocol.TypeExecOpen, req), true); err != nil {
		t.Fatal(err)
	}
	closed := nextExecFrame(t, out, protocol.TypeExecClose).Payload.(protocol.ExecClose)
	if strings.Contains(closed.Reason, "secret") || closed.ExitCode != nil {
		t.Fatal(closed)
	}
	waitExecBudget(t, s.budget, 0)
	if err := s.handle(execFrame(protocol.TypeExecOpen, req), true); !errors.Is(err, errExecProtocol) {
		t.Fatal("failed start replay accepted", err)
	}
}

func TestExecOutputStallClosesAttachment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local, peer := net.Pipe()
	defer peer.Close()
	rt := &pipeExec{Conn: local, sizes: make(chan protocol.TerminalSize, 1)}
	out := make(chan outFrame) // ready is drained, then the peer stops reading output
	s := newExecStreams(ctx, "endpoint", make([]byte, 32), &execBudget{}, &Options{Exec: func(context.Context, protocol.ExecSpec) (ExecSession, error) { return rt, nil }}, out)
	if err := s.handle(execFrame(protocol.TypeExecOpen, execRequest()), true); err != nil {
		t.Fatal(err)
	}
	nextExecFrame(t, out, protocol.TypeExecReady)
	if _, err := peer.Write([]byte("output")); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(7 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatal("stalled attachment was not closed", err)
	}
	closed := nextExecFrame(t, out, protocol.TypeExecClose).Payload.(protocol.ExecClose)
	if closed.ExitCode != nil || !strings.Contains(closed.Reason, "unknown") {
		t.Fatal(closed)
	}
	waitExecBudget(t, s.budget, 0)
}

func TestExecSocketWithoutRuntimeRemainsResponsive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checked := make(chan error, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			checked <- err
			return
		}
		defer conn.CloseNow()
		if err = write(ctx, conn, protocol.TypeChallenge, protocol.Challenge{Nonce: make([]byte, 32), InstanceFingerprint: "instance", Versions: []int{1}}); err != nil {
			checked <- err
			return
		}
		if _, err = read(ctx, conn); err != nil {
			checked <- err
			return
		}
		if err = write(ctx, conn, protocol.TypeHello, protocol.Hello{State: "active", HeartbeatSeconds: 1}); err != nil {
			checked <- err
			return
		}
		if err = write(ctx, conn, protocol.TypeExecOpen, execRequest()); err != nil {
			checked <- err
			return
		}
		refused, beats := false, 0
		for beats < 2 {
			f, err := read(ctx, conn)
			if err != nil {
				checked <- err
				return
			}
			switch f.Type {
			case protocol.TypeExecClose:
				var closed protocol.ExecClose
				if json.Unmarshal(f.Payload, &closed) != nil || closed.Stream != "stream" || closed.ExitCode != nil || closed.Reason != "this agent has no exec runtime; not started" {
					checked <- fmt.Errorf("incorrect refusal: %+v", closed)
					return
				}
				refused = true
			case protocol.TypeHeartbeat:
				if refused {
					beats++
				}
				if err = write(ctx, conn, protocol.TypeHeartbeat, nil); err != nil {
					checked <- err
					return
				}
			}
		}
		checked <- nil
	}))
	defer stub.Close()
	_, key, _ := ed25519.GenerateKey(nil)
	id := &Identity{EndpointID: "endpoint", PrivateKey: key, InstanceFingerprint: "instance", Server: stub.URL}
	target, _ := ConnectURL(stub.URL)
	opts := &Options{HTTPClient: stub.Client(), Log: log.New(io.Discard, "", 0)}
	ledger := openLedger(t.TempDir())
	done := make(chan struct{})
	go func() { defer close(done); _ = session(ctx, id, target, opts, ledger, newBudget(), &execBudget{}) }()
	select {
	case err := <-checked:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session survived cancellation")
	}
}
