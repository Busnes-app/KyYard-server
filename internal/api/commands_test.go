package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// A command is durable intent: the record exists before the frame is sent, the agent's answer
// settles it once, and an answer that never arrives is unknown rather than a missing row.
func TestCommandDispatchSettlesAndIsScoped(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "b", Name: "B"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	admin := loginAs(t, s, st, "envadmin", "user")
	viewer := loginAs(t, s, st, "viewer", "user")
	stranger := loginAs(t, s, st, "stranger", "user")
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "usr_stranger", Role: store.RoleOrganizationAdmin, Status: "active"})
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-cmd")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	path := "/api/organizations/a/endpoints/" + ag.id + "/commands"
	body := `{"action":"container.restart","container":"c1","expects":{"state":"running"}}`

	// Not connected: the command is refused rather than queued, because an operator's
	// decision must not execute at an unknown later time against a host they cannot see.
	w := tenantRequest(s, admin, "POST", path, body, true)
	if w.Code != 409 {
		t.Fatalf("dispatch to a disconnected endpoint: %d %s", w.Code, w.Body.String())
	}
	before, err := ts.ListCommands(ctx, store.TenantAccess{ActorID: "usr_envadmin", OrganizationID: "a"}, ag.id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("a refusal left a command record behind: %+v", before)
	}

	sock, _ := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()), Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}})
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })

	// Connected: the record is written, the frame carries the tenant binding and the
	// expectation, and the actor is never sent to the agent.
	w = tenantRequest(s, admin, "POST", path, body, true)
	if w.Code != 202 {
		t.Fatalf("dispatch: %d %s", w.Code, w.Body.String())
	}
	var cmd store.Command
	if err := json.Unmarshal(w.Body.Bytes(), &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Outcome != "" || cmd.ActorID != "usr_envadmin" {
		t.Fatalf("a fresh command should be in flight and attributed: %+v", cmd)
	}
	frame := readEnvelope(t, ctx, sock.conn)
	if frame.Type != protocol.TypeCommand {
		t.Fatalf("expected a command frame, got %+v", frame)
	}
	var sent protocol.Command
	_ = json.Unmarshal(frame.Payload, &sent)
	if sent.ID != cmd.ID || sent.Endpoint != ag.id || sent.Org != "a" || sent.Expects.State != "running" {
		t.Fatalf("the frame lost its binding or its precondition: %+v", sent)
	}
	if strings.Contains(string(frame.Payload), "usr_envadmin") {
		t.Fatal("the actor was sent to the agent; who asked is recorded server-side only")
	}

	// The agent answers, and the record settles once.
	writeEnvelope(t, ctx, sock.conn, protocol.TypeResult, protocol.Result{ID: cmd.ID, Outcome: protocol.OutcomeSucceeded})
	waitFor(t, func() bool {
		got, err := ts.ReadCommand(ctx, store.TenantAccess{ActorID: "usr_envadmin", OrganizationID: "a"}, ag.id, cmd.ID)
		return err == nil && got.Outcome == protocol.OutcomeSucceeded
	})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeResult, protocol.Result{ID: cmd.ID, Outcome: protocol.OutcomeFailed, Detail: "a late duplicate"})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	readEnvelope(t, ctx, sock.conn)
	settled, err := ts.ReadCommand(ctx, store.TenantAccess{ActorID: "usr_envadmin", OrganizationID: "a"}, ag.id, cmd.ID)
	if err != nil || settled.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("a late answer overwrote the first: %+v %v", settled, err)
	}

	// Scope: a reader may look, a read-only member may not operate, another tenant sees
	// neither. The matrix puts container.operate with the administrators and the operator.
	if w := tenantRequest(s, viewer, "GET", path, "", true); w.Code != 200 {
		t.Fatalf("a reader could not list commands: %d", w.Code)
	}
	if w := tenantRequest(s, viewer, "POST", path, body, true); w.Code != 403 {
		t.Fatalf("a read-only member dispatched a command: %d", w.Code)
	}
	if w := tenantRequest(s, stranger, "POST", path, body, true); w.Code != 403 {
		t.Fatalf("another organization dispatched a command: %d", w.Code)
	}
	if w := tenantRequest(s, admin, "POST", path, `{"action":"container.remove","container":"c1"}`, true); w.Code != 400 {
		t.Fatalf("destroying a container is not an operate action and must be refused here: %d", w.Code)
	}
}

// The socket ending is not evidence that the work did not happen. An in-flight command becomes
// unknown, and nothing retries it on its own.
func TestCommandsLeftUnansweredBecomeUnknown(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-lost")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	sock, _ := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()), Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}})
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })

	w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/commands", `{"action":"container.stop","container":"c1"}`, true)
	if w.Code != 202 {
		t.Fatalf("dispatch: %d %s", w.Code, w.Body.String())
	}
	var cmd store.Command
	_ = json.Unmarshal(w.Body.Bytes(), &cmd)
	readEnvelope(t, ctx, sock.conn) // the command reaches the agent
	sock.conn.CloseNow()            // and then the connection goes, before any answer

	view := store.TenantAccess{ActorID: "usr_envadmin", OrganizationID: "a"}
	waitFor(t, func() bool {
		got, err := ts.ReadCommand(ctx, view, ag.id, cmd.ID)
		return err == nil && got.Outcome == protocol.OutcomeUnknown
	})
	got, err := ts.ReadCommand(ctx, view, ag.id, cmd.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != protocol.OutcomeUnknown || got.DispatchedAt == nil {
		t.Fatalf("a dispatched command lost with the socket must read as unknown: %+v", got)
	}
	if !strings.Contains(got.Detail, "connection ended") {
		t.Fatalf("the unknown outcome should say why: %q", got.Detail)
	}
}

// An agent is not a trusted author of operator-facing logs. A result frame carrying an
// unrecognised outcome reaches a log line, so the identifier in it must not be able to forge
// or reorder what an operator reads.
func TestAnAgentCannotWriteExtraLinesIntoTheLog(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-log")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	sock, _ := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)

	var captured bytes.Buffer
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// An outcome outside the closed set is refused by the store, which is the branch that
	// logs; the ID carries newlines and a forged line.
	writeEnvelope(t, ctx, sock.conn, protocol.TypeResult, protocol.Result{
		ID:      "cmd\n2026/09/17 12:00:00 [SECURITY] all endpoints revoked by operator\nx",
		Outcome: "not-an-outcome",
	})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	readEnvelope(t, ctx, sock.conn) // the session continues, so the log line has been written

	out := captured.String()
	t.Logf("captured log (%d settling lines):\n%s", strings.Count(out, "settling command"), out)
	// Other goroutines log legitimately, so the question is not how many lines there are but
	// whether any of them is one the agent wrote.
	if strings.Contains(out, "[SECURITY]") || strings.Contains(out, "revoked by operator") {
		t.Fatalf("an agent forged a line in the operator's log:\n%s", out)
	}
	if !strings.Contains(out, "settling command") {
		t.Fatalf("the refusal was not reported at all: %q", out)
	}
	// One frame, one line: neither split by the agent's text nor repeated.
	if n := strings.Count(out, "settling command"); n != 1 {
		t.Fatalf("one result frame produced %d log lines:\n%s", n, out)
	}
}

// Destroying a container is not a stronger form of stopping one. It needs its own permission,
// a statement of what the actor saw, and the container's name typed back, and the preview an
// operator confirms from must say what survives as well as what does not.
func TestRemovingAContainerNeedsConfirmationAndItsOwnPermission(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	admin := loginAs(t, s, st, "envadmin", "user")
	operator := loginAs(t, s, st, "operator", "user")
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_operator", Role: "operator", Status: "active"})
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-rm")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	sock, _ := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	snap := protocol.Snapshot{
		Generation: uint64(time.Now().Unix()),
		Containers: []protocol.Container{{ID: "c1", Name: "web", Image: "nginx:1", State: "exited", Status: "Exited", ComposeProject: "shop", Ports: []protocol.Port{{Container: 80, Protocol: "tcp"}}, Labels: map[string]string{}, Networks: []string{}}},
		Images:     []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, snap)
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })

	// The preview names the consequences, including what survives.
	w := tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/web/removal", "", true)
	if w.Code != 200 {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	var preview struct {
		Container    string   `json:"container"`
		ContainerID  string   `json:"container_id"`
		Consequences []string `json:"consequences"`
		ConfirmWith  string   `json:"confirm_with"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &preview)
	joined := strings.Join(preview.Consequences, " | ")
	if !strings.Contains(joined, "writable layer") || !strings.Contains(joined, "left alone") || !strings.Contains(joined, "shop") {
		t.Fatalf("the preview did not say what goes and what stays: %q", joined)
	}
	if preview.ConfirmWith != "web" {
		t.Fatalf("the preview did not say what to type back: %q", preview.ConfirmWith)
	}

	path := "/api/organizations/a/endpoints/" + ag.id + "/commands"
	// An operator may restart but not destroy: the matrix gives destroy to administrators.
	if w := tenantRequest(s, operator, "POST", path, `{"action":"container.restart","container":"web"}`, true); w.Code != 202 {
		t.Fatalf("an operator could not restart: %d %s", w.Code, w.Body.String())
	}
	readEnvelope(t, ctx, sock.conn)
	if w := tenantRequest(s, operator, "POST", path, `{"action":"container.remove","container":"web","confirm":"web","expects":{"state":"exited"}}`, true); w.Code != 403 {
		t.Fatalf("an operator destroyed a container: %d", w.Code)
	}
	// An administrator still has to say what they saw and name it back.
	if w := tenantRequest(s, admin, "POST", path, `{"action":"container.remove","container":"web","confirm":"web"}`, true); w.Code != 400 {
		t.Fatalf("removal without a stated expectation: %d", w.Code)
	}
	if w := tenantRequest(s, admin, "POST", path, `{"action":"container.remove","container":"web","expects":{"state":"exited"}}`, true); w.Code != 400 {
		t.Fatalf("removal without confirmation: %d", w.Code)
	}
	if w := tenantRequest(s, admin, "POST", path, `{"action":"container.remove","container":"web","confirm":"webb","expects":{"state":"exited"}}`, true); w.Code != 400 {
		t.Fatalf("removal confirmed with the wrong name: %d", w.Code)
	}
	// The preview's own output must work as dispatch input: that is the flow it exists for.
	fromPreview, err := json.Marshal(map[string]any{
		"action": "container.remove", "container": preview.Container, "confirm": preview.ConfirmWith,
		"expects": map[string]string{"state": "exited"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := tenantRequest(s, admin, "POST", path, string(fromPreview), true); w.Code != 202 {
		t.Fatalf("the preview's own values were refused: %d %s", w.Code, w.Body.String())
	}
	readEnvelope(t, ctx, sock.conn)
	// And so must the container ID, confirmed with the name the server knows.
	byID, _ := json.Marshal(map[string]any{
		"action": "container.remove", "container": preview.ContainerID, "confirm": preview.ConfirmWith,
		"expects": map[string]string{"state": "exited"},
	})
	w = tenantRequest(s, admin, "POST", path, string(byID), true)
	if w.Code != 202 {
		t.Fatalf("removal addressed by container ID: %d %s", w.Code, w.Body.String())
	}
	frame := readEnvelope(t, ctx, sock.conn)
	var sent protocol.Command
	_ = json.Unmarshal(frame.Payload, &sent)
	if sent.Action != protocol.ActionRemove || sent.Expects.State != "exited" {
		t.Fatalf("the removal frame lost its action or its precondition: %+v", sent)
	}
	// Addressed by name, dispatched as the container the confirmation resolved to: a name is
	// a label the runtime reassigns, and a recreate between inventory and execution would
	// otherwise redirect the removal to whatever holds it.
	if sent.Container != preview.ContainerID {
		t.Fatalf("the frame carried %q rather than the confirmed container %q", sent.Container, preview.ContainerID)
	}
	if strings.Contains(string(frame.Payload), "confirm") {
		t.Fatal("the confirmation was sent to the agent; it is the server's check, not the agent's")
	}
}

// Image actions name a reference, not a container, and carry their own permissions. Pulling
// spends the host's disk and bandwidth, so the matrix stops at the operator; removing an image
// is an administrator's action.
func TestImageCommandsAreScopedAndValidated(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	admin := loginAs(t, s, st, "envadmin", "user")
	operator := loginAs(t, s, st, "operator", "user")
	viewer := loginAs(t, s, st, "viewer", "user")
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_operator", Role: "operator", Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"})
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-img")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	sock, _ := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()), Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}})
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })
	path := "/api/organizations/a/endpoints/" + ag.id + "/commands"

	// An operator may pull.
	w := tenantRequest(s, operator, "POST", path, `{"action":"image.pull","reference":"ghcr.io/busnes-app/kyyard:1.2.3"}`, true)
	if w.Code != 202 {
		t.Fatalf("an operator could not pull: %d %s", w.Code, w.Body.String())
	}
	var cmd store.Command
	_ = json.Unmarshal(w.Body.Bytes(), &cmd)
	if cmd.Reference != "ghcr.io/busnes-app/kyyard:1.2.3" || cmd.ContainerID != "" {
		t.Fatalf("an image command recorded a container: %+v", cmd)
	}
	frame := readEnvelope(t, ctx, sock.conn)
	var sent protocol.Command
	_ = json.Unmarshal(frame.Payload, &sent)
	if sent.Reference != cmd.Reference || sent.Container != "" {
		t.Fatalf("the frame confused a reference with a container: %+v", sent)
	}

	// A read-only member may not, and removing an image is an administrator's action.
	if w := tenantRequest(s, viewer, "POST", path, `{"action":"image.pull","reference":"nginx:1"}`, true); w.Code != 403 {
		t.Fatalf("a read-only member pulled an image: %d", w.Code)
	}
	if w := tenantRequest(s, operator, "POST", path, `{"action":"image.remove","reference":"nginx:1"}`, true); w.Code != 403 {
		t.Fatalf("an operator removed an image: %d", w.Code)
	}
	if w := tenantRequest(s, admin, "POST", path, `{"action":"image.remove","reference":"nginx:1"}`, true); w.Code != 202 {
		t.Fatalf("an administrator could not remove an image: %d %s", w.Code, w.Body.String())
	}
	readEnvelope(t, ctx, sock.conn)

	// A reference is not a container name, and neither grammar accepts the other's abuses.
	for _, bad := range []string{
		`{"action":"image.pull","reference":"nginx:1?all=1"}`,
		`{"action":"image.pull","reference":"../../etc/passwd"}`,
		`{"action":"image.pull","reference":""}`,
		`{"action":"container.restart","container":"ghcr.io/busnes-app/kyyard:1.2.3"}`,
	} {
		if w := tenantRequest(s, admin, "POST", path, bad, true); w.Code != 400 {
			t.Fatalf("%s was accepted: %d", bad, w.Code)
		}
	}
}
