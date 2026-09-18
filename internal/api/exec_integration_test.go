package api_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

// Opt-in like the runtime regression: this touches only its own disposable,
// network-isolated workload, using an image the operator has already pulled.
func TestBrowserExecRealDocker(t *testing.T) {
	image := os.Getenv("KY_TEST_DOCKER_EXEC_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_EXEC_IMAGE to an existing shell image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "docker", "run", "-d", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pull", "never", image, "sh", "-c", "sleep 120").CombinedOutput()
	if err != nil {
		t.Fatalf("fixture: %v %s", err, raw)
	}
	id := strings.TrimSpace(string(raw))
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if raw, err := exec.CommandContext(c, "docker", "rm", "-fv", id).CombinedOutput(); err != nil {
			t.Errorf("cleanup: %v %s", err, raw)
		}
	})
	raw, err = exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Image}} {{.Name}}", id).Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		t.Fatal("inspect format")
	}
	spec := protocol.ExecSpec{Container: id, ImageID: fields[0], User: "65534:65534", Argv: []string{"/bin/sh"}}
	name := strings.TrimPrefix(fields[1], "/")
	s, st, _ := setupTestServerWith(t, func(cfg *config.Config) { cfg.Server.AppURL = terminalOrigin })
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}); err != nil {
		t.Fatal(err)
	}
	admin := loginAs(t, s, st, "execadmin", "user")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_execadmin", Role: store.RoleOrganizationAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	defer s.BeginShutdown()
	ag := enrollAgent(t, s, st, admin, "exec-real")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal(w.Code)
	}
	engine := docker.New("/var/run/docker.sock")
	agentCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- client.Run(agentCtx, &client.Identity{EndpointID: ag.id, PrivateKey: ag.priv, InstanceFingerprint: ag.inst, Server: httpSrv.URL}, client.Options{IdentityDir: t.TempDir(), Snapshot: func(context.Context) (*protocol.Snapshot, error) {
			return &protocol.Snapshot{Containers: []protocol.Container{{ID: id, ImageID: spec.ImageID, Name: name, State: "running"}}}, nil
		}, Exec: func(ctx context.Context, spec protocol.ExecSpec) (client.ExecSession, error) {
			return engine.OpenExec(ctx, spec)
		}})
	}()
	defer func() { stop(); <-done; s.WaitDetached() }()
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })
	// Use the browser protocol against the real server, agent client and Docker PTY.
	f := terminalFixture{s: s, st: st, url: httpSrv.URL, admin: admin, ag: logAgent{id: ag.id}, ctx: ctx}
	// The shared dial helper's URL uses terminalSpec, so dial this actual ID directly.
	headers := map[string][]string{"Origin": {terminalOrigin}, "Cookie": {admin.String() + "; ky_csrf=terminal-test-csrf"}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpSrv.URL, "http")+"/api/organizations/a/endpoints/"+ag.id+"/containers/"+id+"/exec", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	f.start(t, c, "terminal-test-csrf", name, spec)
	ready := readEnvelope(t, ctx, c)
	if ready.Type != protocol.TypeExecReady {
		t.Fatalf("not ready: %s %s", ready.Type, ready.Payload)
	}
	var stream protocol.ExecStream
	if json.Unmarshal(ready.Payload, &stream) != nil {
		t.Fatal("bad ready")
	}
	writeEnvelope(t, ctx, c, protocol.TypeExecResize, protocol.ExecResize{Stream: stream.Stream, Size: protocol.TerminalSize{Rows: 41, Columns: 113}})
	writeEnvelope(t, ctx, c, protocol.TypeExecInput, protocol.ExecData{Stream: stream.Stream, Data: []byte("stty size; id -u; printf 'browser-proof-ok\\n'; exit 7\n")})
	var output strings.Builder
	for {
		frame := readEnvelope(t, ctx, c)
		if frame.Type == protocol.TypeExecOutput {
			var data protocol.ExecData
			_ = json.Unmarshal(frame.Payload, &data)
			output.Write(data.Data)
			if output.Len() > 32768 {
				t.Fatal("unexpected output size")
			}
		}
		if frame.Type == protocol.TypeExecClose {
			var end protocol.ExecClose
			_ = json.Unmarshal(frame.Payload, &end)
			if end.ExitCode == nil || *end.ExitCode != 7 {
				t.Fatalf("unknown exit: %+v", end)
			}
			break
		}
	}
	for _, want := range []string{"41 113", "65534", "browser-proof-ok"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q from %q", want, output.String())
		}
	}
}
