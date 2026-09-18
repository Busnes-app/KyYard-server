package docker_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
)

// frame is one of Docker's multiplexed log frames: an 8-byte header, then the payload.
func frame(stream byte, payload string) []byte {
	head := make([]byte, 8)
	head[0] = stream
	binary.BigEndian.PutUint32(head[4:], uint32(len(payload)))
	return append(head, payload...)
}

// A container without a TTY has its output multiplexed by the daemon. The header is framing,
// not log text: an operator must never see it, and stdout and stderr are one log to them.
func TestLogsDemultiplexTheRuntimeStream(t *testing.T) {
	var gotQuery url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.EscapedPath(), "/json") {
			_, _ = w.Write([]byte(`{"Config":{"Tty":false}}`))
			return
		}
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.Query()
		_, _ = w.Write(frame(1, "out one\n"))
		_, _ = w.Write(frame(2, "err two\n"))
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)

	var got strings.Builder
	err := c.Logs(context.Background(), protocol.LogRequest{Stream: "s1", Container: "c1", Tail: 50, Timestamps: true}, func(b []byte) error {
		got.WriteString(string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if got.String() != "out one\nerr two\n" {
		t.Fatalf("the log arrived as %q", got.String())
	}
	if gotQuery.Get("tail") != "50" || gotQuery.Get("timestamps") != "1" || gotQuery.Get("stdout") != "1" || gotQuery.Get("stderr") != "1" {
		t.Fatalf("the request asked for %v", gotQuery)
	}
	if gotQuery.Get("follow") != "" {
		t.Fatalf("a request that did not ask to follow followed: %v", gotQuery)
	}
	if !strings.HasSuffix(gotPath, "/containers/c1/logs") {
		t.Fatalf("the log path was %q", gotPath)
	}
}

// A container with a TTY writes its log raw, with no framing to strip.
func TestLogsFromATTYContainerArriveUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.EscapedPath(), "/json") {
			_, _ = w.Write([]byte(`{"Config":{"Tty":true}}`))
			return
		}
		_, _ = w.Write([]byte("plain output\n"))
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	var got strings.Builder
	if err := c.Logs(context.Background(), protocol.LogRequest{Container: "c1", Tail: 10}, func(b []byte) error {
		got.WriteString(string(b))
		return nil
	}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if got.String() != "plain output\n" {
		t.Fatalf("a TTY log arrived as %q", got.String())
	}
}

// The sink decides when enough is enough, and its refusal ends the stream rather than being
// swallowed: the bounds live with the reader that is counting.
func TestLogsStopWhenTheSinkRefuses(t *testing.T) {
	stop := errors.New("that is enough")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.EscapedPath(), "/json") {
			_, _ = w.Write([]byte(`{"Config":{"Tty":false}}`))
			return
		}
		for i := 0; i < 100; i++ {
			_, _ = w.Write(frame(1, "a line\n"))
		}
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	calls := 0
	err := c.Logs(context.Background(), protocol.LogRequest{Container: "c1", Tail: 10}, func([]byte) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("the sink's refusal was answered with %v after %d calls", err, calls)
	}
}

// The agent is the last thing between a request and a root-equivalent socket, so an identifier
// that is not one reaches no request at all.
func TestLogsRefuseAnIdentifierThatIsNotAContainer(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked++ }))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	for _, bad := range []string{"c1/json", "../secrets", "c1?all=1", ""} {
		if err := c.Logs(context.Background(), protocol.LogRequest{Container: bad, Tail: 10}, func([]byte) error { return nil }); err == nil {
			t.Fatalf("%q was accepted as a container", bad)
		}
	}
	if asked != 0 {
		t.Fatalf("%d requests reached the daemon", asked)
	}
}

// A log that ends is not a failure: a stopped container's log ends immediately, and reporting
// that as an error would make every finished container look broken.
func TestAFinishedLogIsNotAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.EscapedPath(), "/json") {
			_, _ = w.Write([]byte(`{"Config":{"Tty":false}}`))
		}
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	if err := c.Logs(context.Background(), protocol.LogRequest{Container: "c1", Tail: 10}, func([]byte) error { return nil }); err != nil {
		t.Fatalf("an empty log: %v", err)
	}
}
