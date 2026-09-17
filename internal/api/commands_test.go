package api_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
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
