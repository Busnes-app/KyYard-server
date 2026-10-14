package docker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
)

// The agent's own container is refused every change and every way in before anything is sent:
// lifecycle actions, exec, configuration read, replacement and removal.
func TestProtectedContainerIsRefusedBeforeMutation(t *testing.T) {
	mutations := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": oldID, "Image": oldImage, "Name": "/kyyard",
			"State": map[string]any{"Status": "running", "Running": true}})
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	c.Protect(oldID)
	for _, action := range []string{protocol.ActionStart, protocol.ActionStop, protocol.ActionRestart, protocol.ActionRemove} {
		// By name too: the inspection resolves it to the protected ID.
		for _, target := range []string{oldID, "kyyard"} {
			if outcome, detail := c.Operate(context.Background(), protocol.Command{Action: action, Container: target}); outcome != protocol.OutcomeDenied || detail != "self_protected" {
				t.Errorf("%s %s: %s %q", action, target, outcome, detail)
			}
		}
	}
	if _, err := c.OpenExec(context.Background(), protocol.ExecSpec{Container: oldID, ImageID: oldImage, User: "1000:1000", Argv: []string{"/bin/sh"}}); err != docker.ErrSelfProtected {
		t.Errorf("exec: %v", err)
	}
	if _, err := c.ReadConfiguration(context.Background(), protocol.InspectionTarget{ContainerID: oldID, ImageID: oldImage, CreatedUnix: 1700000000}); err != docker.ErrSelfProtected {
		t.Errorf("configuration: %v", err)
	}
	if mutations != 0 {
		t.Fatalf("sent %d mutations", mutations)
	}
	t.Run("replacement", func(t *testing.T) {
		for name, req := range map[string]protocol.DeploymentRequest{"deploy": request(webService()), "explicit": explicitRequest(explicitService())} {
			f := newFakeDeployEngine(t)
			c := f.client()
			c.Protect(oldID)
			res := c.Deploy(context.Background(), req, func() {})
			if res.Outcome != protocol.OutcomeDenied || res.Steps[0].Code != "self_protected" || res.Validate() != nil {
				t.Fatalf("%s: %s", name, explicitSteps(res))
			}
			for _, call := range f.steps() {
				if !strings.HasPrefix(call, "GET ") {
					t.Fatalf("%s mutated: %s", name, call)
				}
			}
		}
	})
	t.Run("removal", func(t *testing.T) {
		f := newFakeRemoveEngine(t)
		c := f.client()
		c.Protect(oldID)
		res := c.Remove(context.Background(), removal(oldID), func() {})
		if res.Outcome != protocol.OutcomeDenied || res.Steps[0].Code != "self_protected" {
			t.Fatalf("removal: %+v", res)
		}
		for _, call := range f.steps() {
			if !strings.HasPrefix(call, "GET ") {
				t.Fatalf("removal mutated: %s", call)
			}
		}
	})
	t.Run("another container", func(t *testing.T) {
		f := newFakeDeployEngine(t)
		c := f.client()
		c.Protect(strings.Repeat("e", 64))
		if res := c.Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeSucceeded {
			t.Fatalf("unprotected replacement: %s", explicitSteps(res))
		}
	})
}
