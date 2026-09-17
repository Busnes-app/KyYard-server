package docker_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
)

// A pull reports some failures inside a 200, and a registry that wants a credential must say
// so plainly: registry credentials are not built yet, and an operator should learn that from
// the outcome rather than guess at a generic failure.
func TestPullReportsWhatActuallyHappened(t *testing.T) {
	var status int
	var body string
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("fromImage")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	pull := protocol.Command{Action: protocol.ActionImagePull, Reference: "ghcr.io/busnes-app/kyyard:1.2.3"}

	body = `{"status":"Pulling"}` + "\n" + `{"status":"Downloaded"}`
	if outcome, detail := c.Operate(context.Background(), pull); outcome != protocol.OutcomeSucceeded {
		t.Fatalf("a clean pull: %s %q", outcome, detail)
	}
	if gotQuery != "ghcr.io/busnes-app/kyyard:1.2.3" {
		t.Fatalf("the reference reached the daemon as %q", gotQuery)
	}

	// A late failure inside a 200 is still a failure.
	body = `{"status":"Pulling"}` + "\n" + `{"error":"manifest unknown","errorDetail":{"message":"manifest unknown"}}`
	if outcome, detail := c.Operate(context.Background(), pull); outcome != protocol.OutcomeFailed || !strings.Contains(detail, "manifest unknown") {
		t.Fatalf("a late failure was reported as %s %q", outcome, detail)
	}

	// Authorization, whether it arrives as a status or in the stream, names the real reason.
	body = `{"error":"unauthorized: authentication required"}`
	if outcome, detail := c.Operate(context.Background(), pull); outcome != protocol.OutcomeDenied || !strings.Contains(detail, "credential") {
		t.Fatalf("an unauthorized stream was reported as %s %q", outcome, detail)
	}
	status, body = http.StatusUnauthorized, ""
	if outcome, detail := c.Operate(context.Background(), pull); outcome != protocol.OutcomeDenied || !strings.Contains(detail, "credential") {
		t.Fatalf("a 401 was reported as %s %q", outcome, detail)
	}
	status, body = http.StatusNotFound, ""
	if outcome, detail := c.Operate(context.Background(), pull); outcome != protocol.OutcomeDenied || !strings.Contains(detail, "no such image") {
		t.Fatalf("a missing tag was reported as %s %q", outcome, detail)
	}
}

// An image a container still uses is refused rather than forced: forcing would leave running
// containers pointing at something that is no longer there.
func TestRemovingAnImageInUseIsRefused(t *testing.T) {
	var status int
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	rm := protocol.Command{Action: protocol.ActionImageRemove, Reference: "ghcr.io/busnes-app/kyyard:1.2.3"}

	status = http.StatusConflict
	if outcome, detail := c.Operate(context.Background(), rm); outcome != protocol.OutcomeDenied || !strings.Contains(detail, "still using") {
		t.Fatalf("an image in use: %s %q", outcome, detail)
	}
	status = http.StatusOK
	if outcome, detail := c.Operate(context.Background(), rm); outcome != protocol.OutcomeSucceeded {
		t.Fatalf("an unused image: %s %q", outcome, detail)
	}
	// The reference is one path segment, whatever slashes it contains.
	if cut := strings.Index(path, "/images/"); cut < 0 || strings.Contains(path[cut+len("/images/"):], "/") {
		t.Fatalf("the reference shaped the path: %q", path)
	}
}
