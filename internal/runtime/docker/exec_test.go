package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

var execFixtureSpec = protocol.ExecSpec{Container: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), User: "1000:1000", Argv: []string{"/bin/sh"}}

const execFixtureID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func execFixture(t *testing.T, running bool, stall ...bool) (*Client, <-chan string, *atomic.Int32) {
	t.Helper()
	resizes := make(chan string, 4)
	stop := make(chan struct{})
	starts := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/"+execFixtureSpec.Container+"/json"):
			json.NewEncoder(w).Encode(map[string]any{"Id": execFixtureSpec.Container, "Image": execFixtureSpec.ImageID, "State": map[string]bool{"Running": running}})
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body struct {
				AttachStdin, AttachStdout, AttachStderr, Tty, Privileged bool
				User                                                     string
				Cmd, Env                                                 []string
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !body.AttachStdin || !body.AttachStdout || !body.AttachStderr || !body.Tty || body.Privileged || body.User != "1000:1000" || len(body.Cmd) != 1 || body.Cmd[0] != "/bin/sh" || len(body.Env) != 1 || body.Env[0] != "TERM=xterm-256color" {
				t.Errorf("unexpected exec config: %+v", body)
			}
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"Id":%q}`, execFixtureID)
		case strings.HasSuffix(r.URL.Path, "/start"):
			starts.Add(1)
			io.Copy(io.Discard, r.Body)
			if r.Header.Get("Upgrade") != "tcp" || r.Header.Get("Connection") != "Upgrade" {
				t.Error("missing upgrade headers")
			}
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nready\n")
			buf.Flush()
			if len(stall) > 0 && stall[0] {
				<-stop
				return
			}
			io.Copy(conn, buf)
		case strings.HasSuffix(r.URL.Path, "/resize"):
			resizes <- r.URL.RawQuery
			w.WriteHeader(201)
		case strings.HasSuffix(r.URL.Path, "/exec/"+execFixtureID+"/json"):
			fmt.Fprintf(w, `{"ID":%q,"ContainerID":%q,"Running":false,"ExitCode":7}`, execFixtureID, execFixtureSpec.Container)
		default:
			t.Error("unexpected path", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })
	return NewHTTP(srv.Client(), srv.URL), resizes, starts
}
func TestExecPTYInputResizeAndStatus(t *testing.T) {
	c, resizes, starts := execFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := c.OpenExec(ctx, execFixtureSpec)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ready := make([]byte, 6)
	if _, err := io.ReadFull(s, ready); err != nil || string(ready) != "ready\n" {
		t.Fatal(string(ready), err)
	}
	if _, err := s.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(s, ready); err != nil || string(ready) != "hello\n" {
		t.Fatal(string(ready), err)
	}
	if err := s.Resize(ctx, protocol.TerminalSize{Rows: 40, Columns: 120}); err != nil {
		t.Fatal(err)
	}
	if got := <-resizes; got != "h=40&w=120" {
		t.Fatal(got)
	}
	if err := s.Resize(ctx, protocol.TerminalSize{}); err == nil {
		t.Fatal("accepted zero-size resize")
	}
	if _, err := s.Write(make([]byte, protocol.MaxExecChunkBytes+1)); err == nil {
		t.Fatal("accepted oversized input")
	}
	if starts.Load() != 1 {
		t.Fatal("start was retried")
	}
	s.Close()
	status, err := s.Inspect(ctx)
	if err != nil || status.Running || status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatal(status, err)
	}
}
func TestExecRejectsBeforeStart(t *testing.T) {
	c, _, starts := execFixture(t, false)
	if _, err := c.OpenExec(context.Background(), execFixtureSpec); err == nil {
		t.Fatal("executed in stopped container")
	}
	spec := execFixtureSpec
	spec.Container = "web"
	if _, err := c.OpenExec(context.Background(), spec); err == nil {
		t.Fatal("executed by mutable name")
	}
	if starts.Load() != 0 {
		t.Fatal("started a refused command")
	}
}
func TestExecCancellationAndIdleUnblockReads(t *testing.T) {
	for _, kind := range []string{"cancel", "idle", "absolute", "close"} {
		t.Run(kind, func(t *testing.T) {
			c, _, _ := execFixture(t, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			idle, lifetime := time.Second, time.Second
			if kind == "idle" {
				idle = 40 * time.Millisecond
			}
			if kind == "absolute" {
				lifetime = 40 * time.Millisecond
			}
			s, err := c.openExec(ctx, execFixtureSpec, idle, lifetime)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			buf := make([]byte, 6)
			if _, err := io.ReadFull(s, buf); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := s.Read(buf); done <- err }()
			if kind == "cancel" {
				cancel()
			}
			if kind == "close" {
				s.Close()
			}
			select {
			case err := <-done:
				want := ErrExecClosed
				if kind == "cancel" {
					want = context.Canceled
				}
				if kind == "idle" {
					want = ErrExecIdle
				}
				if kind == "absolute" {
					want = ErrExecLifetime
				}
				if !errors.Is(err, want) {
					t.Fatalf("got %v, want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("read leaked past cancellation")
			}
		})
	}
}
func TestExecStartFailureIsUnknownAndNeverRetried(t *testing.T) {
	var starts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			starts.Add(1)
			w.WriteHeader(500)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"Id":%q}`, execFixtureID)
		default:
			fmt.Fprintf(w, `{"Id":%q,"Image":%q,"State":{"Running":true}}`, execFixtureSpec.Container, execFixtureSpec.ImageID)
		}
	}))
	defer srv.Close()
	_, err := NewHTTP(srv.Client(), srv.URL).OpenExec(context.Background(), execFixtureSpec)
	if !errors.Is(err, ErrExecStartUnknown) || starts.Load() != 1 {
		t.Fatal(err, starts.Load())
	}
}

func TestExecIdleUnblocksBackpressuredWrite(t *testing.T) {
	c, _, _ := execFixture(t, true, true)
	s, err := c.openExec(context.Background(), execFixtureSpec, 50*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan error, 1)
	go func() {
		p := make([]byte, protocol.MaxExecChunkBytes)
		for {
			if _, err := s.Write(p); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrExecIdle) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		s.Close()
		t.Fatal("backpressured writer leaked past idle timeout")
	}
}
func TestExecRefusesChangedImageIdentity(t *testing.T) {
	c, _, starts := execFixture(t, true)
	spec := execFixtureSpec
	spec.ImageID = "sha256:" + strings.Repeat("d", 64)
	if _, err := c.OpenExec(context.Background(), spec); err == nil {
		t.Fatal("accepted changed image")
	}
	if starts.Load() != 0 {
		t.Fatal("started exec against changed image")
	}
}
func TestExecInputRenewsIdleButNotAbsoluteLimit(t *testing.T) {
	c, _, _ := execFixture(t, true)
	s, err := c.openExec(context.Background(), execFixtureSpec, 200*time.Millisecond, 450*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Keep input active for longer than the idle budget. The absolute limit still wins.
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(time.Second)
	for {
		select {
		case <-ticker.C:
			if _, err := s.Write([]byte("x")); err != nil {
				if !errors.Is(err, ErrExecLifetime) {
					t.Fatal(err)
				}
				return
			}
		case <-deadline:
			t.Fatal("active input bypassed absolute timeout")
		}
	}
}

func TestExecInspectNeverInventsAnExitCode(t *testing.T) {
	for _, tc := range []struct {
		name, extra, container string
		wantError              bool
	}{
		{"running", `"Running":true,"ExitCode":0`, execFixtureSpec.Container, false},
		{"missing exit", `"Running":false`, execFixtureSpec.Container, true},
		{"missing state", `"ExitCode":0`, execFixtureSpec.Container, true},
		{"wrong container", `"Running":false,"ExitCode":0`, strings.Repeat("d", 64), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"ID":%q,"ContainerID":%q,%s}`, execFixtureID, tc.container, tc.extra)
			}))
			defer srv.Close()
			s := &ExecSession{client: NewHTTP(srv.Client(), srv.URL), id: execFixtureID, container: execFixtureSpec.Container}
			status, err := s.Inspect(context.Background())
			if (err != nil) != tc.wantError {
				t.Fatal(status, err)
			}
			if !tc.wantError && (!status.Running || status.ExitCode != nil) {
				t.Fatal("invented an exit", status)
			}
		})
	}
}
