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

// Removal refuses a running container rather than forcing it, refuses when something still
// depends on it, and never takes the data with it.
func TestRemovalRefusesRatherThanForces(t *testing.T) {
	var methods, paths, queries []string
	state := "running"
	conflict := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.EscapedPath())
		queries = append(queries, r.URL.RawQuery)
		if r.Method == http.MethodDelete {
			if conflict {
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Image":"sha256:abc","State":{"Status":"` + state + `"},"Mounts":[{"Name":"data","Type":"volume"}]}`))
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	cmd := protocol.Command{Action: protocol.ActionRemove, Container: "web", Expects: protocol.Expectation{State: "running"}}

	outcome, detail := c.Operate(context.Background(), cmd)
	if outcome != protocol.OutcomeDenied || !strings.Contains(detail, "stop it first") {
		t.Fatalf("a running container was not refused: %s %q", outcome, detail)
	}
	for _, m := range methods {
		if m == http.MethodDelete {
			t.Fatal("a running container was deleted")
		}
	}

	state = "exited"
	cmd.Expects.State = "exited"
	conflict = true
	if outcome, detail := c.Operate(context.Background(), cmd); outcome != protocol.OutcomeDenied || !strings.Contains(detail, "depends on") {
		t.Fatalf("a dependency conflict was not reported as a refusal: %s %q", outcome, detail)
	}

	conflict = false
	methods, queries = nil, nil
	if outcome, detail := c.Operate(context.Background(), cmd); outcome != protocol.OutcomeSucceeded {
		t.Fatalf("a stopped container was not removed: %s %q", outcome, detail)
	}
	for i, m := range methods {
		if m == http.MethodDelete && queries[i] != "" {
			// v=1 would take named volumes with it. Destroying data is a separate action
			// with its own confirmation, so this request carries no options at all.
			t.Fatalf("the delete carried options the caller never asked for: %q", queries[i])
		}
	}
}
