package suite

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serve runs h on a Unix socket in a fresh private directory, short enough for sun_path.
func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "suite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "runner.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return socket
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func TestCheckSocketPath(t *testing.T) {
	for _, bad := range []string{"run/runner.sock", "/run/kyquickstart/../runner.sock", "/run//runner.sock", "/" + strings.Repeat("d", maxSocketPath)} {
		if CheckSocketPath(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if err := CheckSocketPath("/run/kyquickstart/runner.sock"); err != nil {
		t.Fatal(err)
	}
}

func TestDialRefusesADirectoryOthersCanReach(t *testing.T) {
	socket := serve(t, reply(200, `{"revision":1,"stack":""}`))
	c := New(socket)
	if _, err := c.Plan(context.Background()); err != nil {
		t.Fatalf("private directory: %v", err)
	}
	if err := os.Chmod(filepath.Dir(socket), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Plan(context.Background()); !errors.Is(err, ErrUnsafeSocket) {
		t.Fatalf("shared directory: %v, want ErrUnsafeSocket", err)
	}
}

func TestDecodingIsStrict(t *testing.T) {
	for _, body := range []string{`{"revision":1,"stack":"","extra":true}`, `{"revision":1,"stack":""}{}`, `{"revision":"1"}`} {
		if _, err := New(serve(t, reply(200, body))).Plan(context.Background()); !errors.Is(err, ErrInvalidResponse) {
			t.Errorf("%s: %v, want ErrInvalidResponse", body, err)
		}
	}
	big := `{"revision":1,"stack":"` + strings.Repeat("a", maxResponseBytes) + `"}`
	if _, err := New(serve(t, reply(200, big))).Plan(context.Background()); !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("oversized answer: %v", err)
	}
}

func TestRefusalsAndMissingOperations(t *testing.T) {
	c := New(serve(t, reply(409, `{"error":{"code":"busy","message":"operation 1 is running"}}`)))
	var refusal *Error
	if err := c.Start(context.Background(), Request{ID: "x", Kind: "upgrade", Apps: []string{}}); !errors.As(err, &refusal) || refusal.Status != 409 || refusal.Code != "busy" || refusal.Message != "operation 1 is running" {
		t.Fatalf("refusal: %v", err)
	}
	if _, err := New(serve(t, reply(409, `busy`))).Status(context.Background()); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("unstructured refusal: %v", err)
	}
	if _, err := New(serve(t, reply(404, `404 page not found`))).Operation(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing operation: %v", err)
	}
	gone := serve(t, reply(200, `{}`))
	os.Remove(gone)
	if _, err := New(gone).Status(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("no listener: %v", err)
	}
}

// The request is the spec's: apps always a list, target only for install, and the started ID
// echoed back.
func TestStartSendsTheProtocolRequest(t *testing.T) {
	var got []map[string]any
	c := New(serve(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{"id": body["id"]})
	}))
	if err := c.Start(context.Background(), Request{ID: "a", Kind: "upgrade", Apps: []string{}, AcknowledgeNoBackup: true, Actor: "usr_1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background(), Request{ID: "b", Kind: "install", Apps: []string{"hello"}, Target: "nas", Actor: "usr_1"}); err != nil {
		t.Fatal(err)
	}
	upgrade, _ := json.Marshal(got[0])
	install, _ := json.Marshal(got[1])
	if string(upgrade) != `{"acknowledge_no_backup":true,"actor":"usr_1","apps":[],"id":"a","kind":"upgrade"}` || string(install) != `{"acknowledge_no_backup":false,"actor":"usr_1","apps":["hello"],"id":"b","kind":"install","target":"nas"}` {
		t.Fatalf("requests: %s %s", upgrade, install)
	}
	echo := New(serve(t, reply(202, `{"id":"other"}`)))
	if err := echo.Start(context.Background(), Request{ID: "a", Kind: "upgrade", Apps: []string{}}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("mismatched id: %v", err)
	}
}

func TestOperationRefusesUnknownStates(t *testing.T) {
	op := `{"id":"x","kind":"upgrade","apps":[],"actor":"u","state":"%s","started":"2026-10-16T10:00:00Z","finished":null,"steps":[],"error":null}`
	if got, err := New(serve(t, reply(200, strings.Replace(op, "%s", "running", 1)))).Operation(context.Background(), "x"); err != nil || got.State != "running" {
		t.Fatalf("running: %v %+v", err, got)
	}
	if _, err := New(serve(t, reply(200, strings.Replace(op, "%s", "paused", 1)))).Operation(context.Background(), "x"); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("unknown state: %v", err)
	}
}
