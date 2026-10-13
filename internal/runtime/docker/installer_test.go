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

func TestInstallerOwnershipRefusesLifecycleAndExecBeforeMutation(t *testing.T) {
	mutations := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": oldID, "Image": oldImage,
			"Config": map[string]any{"Labels": map[string]string{"ky.managed-by": "kyquickstart"}},
			"State":  map[string]any{"Status": "running", "Running": true}})
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	for _, action := range []string{protocol.ActionStart, protocol.ActionStop, protocol.ActionRestart, protocol.ActionRemove} {
		outcome, detail := c.Operate(context.Background(), protocol.Command{Action: action, Container: oldID})
		if outcome != protocol.OutcomeDenied || detail != "application_managed" {
			t.Fatalf("%s: %s %q", action, outcome, detail)
		}
	}
	if _, err := c.OpenExec(context.Background(), protocol.ExecSpec{Container: oldID, ImageID: oldImage, User: "1000:1000", Argv: []string{"/bin/sh"}}); err == nil {
		t.Fatal("installer-owned container accepted exec")
	}
	if mutations != 0 {
		t.Fatalf("sent %d mutations", mutations)
	}
}

func TestInstallerOwnershipRefusesReplacementAndRemoval(t *testing.T) {
	t.Run("replacement", func(t *testing.T) {
		f := newFakeDeployEngine(t)
		f.oldContainer["Config"].(map[string]any)["Labels"] = map[string]string{"ky.managed-by": "kyquickstart"}
		res := f.client().Deploy(context.Background(), request(webService()), func() {})
		if res.Outcome != protocol.OutcomeDenied {
			t.Fatalf("replacement: %+v", res)
		}
		for _, call := range f.steps() {
			if !strings.HasPrefix(call, "GET ") {
				t.Fatalf("replacement mutated: %s", call)
			}
		}
	})
	t.Run("removal", func(t *testing.T) {
		f := newFakeRemoveEngine(t)
		f.containers[oldID]["Config"] = map[string]any{"Labels": map[string]string{"ky.managed-by": "kyquickstart"}}
		res := f.client().Remove(context.Background(), removal(oldID), func() {})
		if res.Outcome != protocol.OutcomeDenied {
			t.Fatalf("removal: %+v", res)
		}
		for _, call := range f.steps() {
			if !strings.HasPrefix(call, "GET ") {
				t.Fatalf("removal mutated: %s", call)
			}
		}
	})
}
