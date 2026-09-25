package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// Removal goes to an agent that advertises it, names the adopted container by its pinned
// identity, and its result releases the instance and marks the application removed. A result
// that reports identities is not a removal and leaves the row unknown.
func TestRemovalOverTheAgentSocket(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}))
	must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}))
	admin := loginAs(t, s, st, "envadmin", "user")
	viewer := loginAs(t, s, st, "viewer", "user")
	developer := loginAs(t, s, st, "dev", "user")
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"}))
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}))
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_dev", Role: store.RoleDeveloper, Status: "active"}))
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-remove")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	request := func(cookie *http.Cookie, method, path, body string, status int) string {
		t.Helper()
		w := tenantRequest(s, cookie, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	containerID, image := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	created := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	gen := uint64(time.Now().Unix())
	sync := func(sock *agentSocket) {
		t.Helper()
		writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
		if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeHeartbeat {
			t.Fatalf("expected a heartbeat, got %s", f.Type)
		}
	}
	inventory := func(sock *agentSocket, extra ...protocol.Container) {
		t.Helper()
		gen++
		writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{
			Generation: gen, Engine: protocol.Engine{Version: "1"},
			Containers: append([]protocol.Container{{ID: containerID, Name: "shop-web", ImageID: image, State: "running", ComposeProject: "shop", CreatedAt: created, Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}}}, extra...),
			Images:     []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		})
		sync(sock)
	}
	online := func(capabilities ...string) *agentSocket {
		t.Helper()
		waitFor(t, func() bool { return !s.Connected(ag.id) })
		sock, reason := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
		if sock == nil {
			t.Fatalf("connect refused: %s", reason)
		}
		writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities})
		inventory(sock)
		waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })
		return sock
	}

	sock := online(protocol.CapabilityDeploymentApply)
	base := "/api/organizations/a/environments/env-a/applications"
	var app store.Application
	must(json.Unmarshal([]byte(request(admin, "POST", base, `{"name":"shop","compose":"services: {web: {image: nginx:1}}"}`, 201)), &app))
	adoption := base + "/" + app.ID + "/adoption"
	var preview store.AdoptionPreview
	must(json.Unmarshal([]byte(request(admin, "GET", adoption+"?endpoint="+ag.id+"&project=shop", "", 200)), &preview))
	adoptionBody, _ := json.Marshal(store.AdoptionRequest{EndpointID: ag.id, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	var instance store.ApplicationInstance
	must(json.Unmarshal([]byte(request(admin, "POST", adoption, string(adoptionBody), 201)), &instance))
	mapping := base + "/" + app.ID + "/mapping"
	var mapped store.ApplicationMapping
	must(json.Unmarshal([]byte(request(admin, "GET", mapping, "", 200)), &mapped))
	mappingBody, _ := json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: map[string]string{"web": containerID}})
	request(admin, "PUT", mapping, string(mappingBody), 204)

	removal := base + "/" + app.ID + "/removal"
	deployments := base + "/" + app.ID + "/deployments"
	body := `{"instance_id":"` + instance.ID + `","confirm":"shop"}`
	history := func() []store.Deployment {
		t.Helper()
		var rows []store.Deployment
		must(json.Unmarshal([]byte(request(admin, "GET", deployments, "", 200)), &rows))
		return rows
	}
	request(nil, "POST", removal, body, 401)
	if w := tenantRequest(s, admin, "POST", removal, body, false); w.Code != 403 {
		t.Fatalf("removal without CSRF: %d", w.Code)
	}
	request(admin, "POST", removal, `{"instance_id":"`+instance.ID+`","confirm":"shop","extra":1}`, 400)
	request(admin, "POST", removal, `{"instance_id":"00000000-0000-0000-0000-000000000000","confirm":"shop"}`, 404)
	// An agent that cannot remove is never sent a removal and no row is recorded.
	request(admin, "POST", removal, body, 501)
	if rows := history(); len(rows) != 0 {
		t.Fatalf("a refused removal recorded a row: %+v", rows)
	}
	sock.conn.CloseNow()

	sock = online(protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentRemove)
	request(viewer, "POST", removal, body, 403)
	request(developer, "POST", removal, body, 403)
	request(admin, "POST", removal, `{"instance_id":"`+instance.ID+`","confirm":"other"}`, 400)
	// A container of the project KyYard never adopted would be orphaned by the release.
	inventory(sock, protocol.Container{ID: strings.Repeat("f", 64), Name: "shop-web-old", ImageID: image, State: "exited", ComposeProject: "shop", CreatedAt: created, Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}})
	var blocked struct {
		Code     string   `json:"code"`
		Blockers []string `json:"blockers"`
	}
	must(json.Unmarshal([]byte(request(admin, "POST", removal, body, 409)), &blocked))
	if blocked.Code != "preflight_blocked" || len(blocked.Blockers) != 1 || blocked.Blockers[0] != "unadopted_project_containers" {
		t.Fatalf("unadopted project container: %+v", blocked)
	}
	if rows := history(); len(rows) != 0 {
		t.Fatalf("a blocked removal recorded a row: %+v", rows)
	}
	inventory(sock)
	remove := func() (store.Deployment, protocol.RemovalRequest) {
		t.Helper()
		var d store.Deployment
		must(json.Unmarshal([]byte(request(admin, "POST", removal, body, 202)), &d))
		frame := readEnvelope(t, ctx, sock.conn)
		if frame.Type != protocol.TypeDeploymentRemove {
			t.Fatalf("expected %s, got %s", protocol.TypeDeploymentRemove, frame.Type)
		}
		var sent protocol.RemovalRequest
		must(json.Unmarshal(frame.Payload, &sent))
		if sent.Deployment != d.ID || sent.Project != "shop" || len(sent.Containers) != 1 || sent.Containers[0].Service != "web" || sent.Containers[0].Target.ContainerID != containerID {
			t.Fatalf("the frame did not name the adopted container: %+v", sent)
		}
		return d, sent
	}
	steps := []protocol.DeploymentStep{
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded},
		{Service: "web", Step: protocol.StepStop, Outcome: protocol.OutcomeSucceeded},
		{Service: "web", Step: protocol.StepRemove, Outcome: protocol.OutcomeSucceeded},
	}

	// A removal result that reports a container identity did not do what was asked.
	refused, _ := remove()
	rows := history()
	if len(rows) != 1 || rows[0].ID != refused.ID || rows[0].Kind != "remove" || rows[0].State != "applying" || rows[0].EndpointName != "host-remove" {
		t.Fatalf("the history does not show the applying removal: %+v", rows)
	}
	request(admin, "POST", removal, body, 409)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{
		Deployment: refused.ID, Outcome: protocol.OutcomeSucceeded, Steps: steps,
		Services: []protocol.DeploymentIdentity{{Service: "web", ContainerID: strings.Repeat("e", 64), ImageID: image, CreatedUnix: created.Unix()}},
	})
	sync(sock)
	if rows := history(); rows[0].State != protocol.OutcomeUnknown {
		t.Fatalf("a removal result with identities was accepted: %+v", rows[0])
	}

	removing, removingFrame := remove()
	if removingFrame.RequestID == "" || removingFrame.RequestID != removing.CorrelationID {
		t.Fatalf("the removal frame's request id %q, row %q", removingFrame.RequestID, removing.CorrelationID)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{
		Deployment: removing.ID, RequestID: removingFrame.RequestID, Outcome: protocol.OutcomeSucceeded, Steps: steps, Services: []protocol.DeploymentIdentity{},
	})
	sync(sock)
	var settled store.Deployment
	must(json.Unmarshal([]byte(request(admin, "GET", deployments+"/"+removing.ID, "", 200)), &settled))
	if settled.State != protocol.OutcomeSucceeded || settled.Kind != "remove" {
		t.Fatalf("the removal did not settle: %+v", settled)
	}
	var instances []store.ApplicationInstance
	must(json.Unmarshal([]byte(request(admin, "GET", base+"/instances", "", 200)), &instances))
	if len(instances) != 0 {
		t.Fatalf("the instance was not released: %+v", instances)
	}
	var apps []store.Application
	must(json.Unmarshal([]byte(request(admin, "GET", base, "", 200)), &apps))
	if len(apps) != 1 || apps[0].RemovedAt == nil {
		t.Fatalf("the application is not marked removed: %+v", apps)
	}
	request(admin, "DELETE", base+"/"+app.ID, `{"expected_revision":1}`, 204)
	sock.conn.CloseNow()
}
