package docker_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	// A busy pull emits megabytes of progress before it fails. Reading a prefix of the stream
	// would discard the line that says so and report the failure as a success, which is the
	// one direction this must never fail in: the operator would believe a patched image is on
	// the host.
	var chatty strings.Builder
	for chatty.Len() < 4<<20 {
		chatty.WriteString(`{"status":"Downloading","progressDetail":{"current":1,"total":2}}` + "\n")
	}
	chatty.WriteString(`{"error":"toomanyrequests: rate limit exceeded"}` + "\n")
	status, body = 0, chatty.String()
	if outcome, detail := c.Operate(context.Background(), pull); outcome != protocol.OutcomeFailed || !strings.Contains(detail, "rate limit") {
		t.Fatalf("a failure after %d bytes of progress was reported as %s %q", chatty.Len(), outcome, detail)
	}

	// A line mentioning the word is not a failure; only an error field is.
	body = `{"status":"Pulling fs layer error-handling:latest"}`
	if outcome, detail := c.Operate(context.Background(), pull); outcome != protocol.OutcomeSucceeded {
		t.Fatalf("a progress line naming the word was read as %s %q", outcome, detail)
	}
}

// A tag is a label the host reassigns. A removal decided against one image must not destroy
// whatever the tag points at when the agent gets round to it.
func TestRemovingAnImageChecksItIsStillTheSameImage(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	var present string
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.URL.EscapedPath())
			return
		}
		if present == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"Id":"` + present + `"}`))
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	rm := protocol.Command{
		Action:    protocol.ActionImageRemove,
		Reference: "ghcr.io/busnes-app/kyyard:1.2.3",
		Expects:   protocol.Expectation{ImageDigest: digest},
	}

	present = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	if outcome, detail := c.Operate(context.Background(), rm); outcome != protocol.OutcomeDenied || !strings.Contains(detail, "different image") {
		t.Fatalf("a tag that had moved: %s %q", outcome, detail)
	}
	if len(deleted) != 0 {
		t.Fatalf("the delete was sent anyway: %v", deleted)
	}

	present = ""
	if outcome, detail := c.Operate(context.Background(), rm); outcome != protocol.OutcomeDenied || !strings.Contains(detail, "does not have that image") {
		t.Fatalf("an image the host no longer has: %s %q", outcome, detail)
	}
	if len(deleted) != 0 {
		t.Fatalf("the delete was sent anyway: %v", deleted)
	}

	present = digest
	if outcome, detail := c.Operate(context.Background(), rm); outcome != protocol.OutcomeSucceeded {
		t.Fatalf("the image it was decided about: %s %q", outcome, detail)
	}
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], url.PathEscape("ghcr.io/busnes-app/kyyard:1.2.3")) {
		t.Fatalf("what was deleted: %v", deleted)
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
