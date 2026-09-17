package docker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// A pull is a long stream under a 200, so the client must carry no http.Client.Timeout: that
// timeout covers reading the body, and any image taking longer than it to fetch would be cut
// mid-stream and settle as unknown -- toil an operator has to reconcile by hand, on the normal
// path, for the feature this package exists to provide. Every bound is a context deadline.
func TestAPullIsBoundedByItsContextAndNotByTheClient(t *testing.T) {
	stream := func(w http.ResponseWriter, lines int, gap time.Duration) {
		flusher, _ := w.(http.Flusher)
		for i := 0; i < lines; i++ {
			fmt.Fprintf(w, "{\"status\":\"Downloading layer %d\"}\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(gap)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream(w, 20, 50*time.Millisecond)
	}))
	defer srv.Close()

	c := newClient(srv.Client().Transport, srv.URL+"/"+apiVersion)
	if c.http.Timeout != 0 {
		t.Fatalf("the client carries a %s timeout, which would cut a pull's progress stream", c.http.Timeout)
	}
	cmd := protocol.Command{Action: protocol.ActionImagePull, Reference: "ghcr.io/busnes-app/kyyard:1.2.3"}
	if outcome, detail := c.Operate(context.Background(), cmd); outcome != protocol.OutcomeSucceeded {
		t.Fatalf("a pull that streamed for a second: %s %q", outcome, detail)
	}

	// And if a stream is cut anyway, the answer says so rather than claiming the image landed.
	cut := newClient(srv.Client().Transport, srv.URL+"/"+apiVersion)
	cut.http.Timeout = 200 * time.Millisecond
	if outcome, detail := cut.Operate(context.Background(), cmd); outcome != protocol.OutcomeUnknown {
		t.Fatalf("a stream cut short: %s %q", outcome, detail)
	}
}

// The agent is the last thing between a request body and a root-equivalent socket, so it
// checks the reference grammar itself rather than trusting the control plane to have done it.
func TestTheAgentRefusesAReferenceThatIsNotOne(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
	}))
	defer srv.Close()
	c := NewHTTP(srv.Client(), srv.URL)
	for _, bad := range []string{"a/../../etc", "a/./b", "../images/json", "a//b", ""} {
		for _, action := range []string{protocol.ActionImagePull, protocol.ActionImageRemove} {
			cmd := protocol.Command{Action: action, Reference: bad, Expects: protocol.Expectation{ImageDigest: "sha256:" + "a1"}}
			if outcome, detail := c.Operate(context.Background(), cmd); outcome != protocol.OutcomeDenied {
				t.Fatalf("%s %q: %s %q", action, bad, outcome, detail)
			}
		}
	}
	if asked != 0 {
		t.Fatalf("%d requests reached the daemon for references that are not references", asked)
	}
}
