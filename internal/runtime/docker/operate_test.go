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

// The container identifier arrives in a request body and is concatenated into a URL addressed
// to the host's root-equivalent socket. It must name a container and nothing else: no extra
// path segment, no query, whatever the caller sent.
func TestAContainerIdentifierCannotShapeTheURL(t *testing.T) {
	var paths, queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The escaped form is what went on the wire and what a router sees; URL.Path has
		// already had %2F turned back into a separator by the time a handler reads it.
		paths = append(paths, r.URL.EscapedPath())
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Image":"sha256:abc","State":{"Status":"running"}}`))
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	c.Operate(context.Background(), protocol.Command{
		Action:    protocol.ActionRestart,
		Container: "c1/../../images/json?all=1#x",
	})
	if len(paths) == 0 {
		t.Fatal("the adapter made no request")
	}
	for i, p := range paths {
		cut := strings.Index(p, "/containers/")
		if cut < 0 {
			t.Fatalf("request %d left the containers route: %q", i, p)
		}
		rest := p[cut+len("/containers/"):]
		// One segment for the container, one for the verb, and nothing else.
		if segments := strings.Split(rest, "/"); len(segments) != 2 {
			t.Fatalf("request %d addressed %q, which is not one container and one action", i, p)
		}
		if queries[i] != "" {
			t.Fatalf("request %d carried a query the caller chose: %q", i, queries[i])
		}
	}
}
