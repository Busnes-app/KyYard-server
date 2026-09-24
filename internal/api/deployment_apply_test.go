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
	"github.com/google/uuid"
)

// Apply sends the resolved plan to the agent once, the agent's answer settles it and rebinds
// the resources it replaced, and a socket that ends first leaves the row unknown. Resolved
// values reach the agent and nobody else.
func TestApplyDeploymentOverTheAgentSocket(t *testing.T) {
	s, st, cfg := setupTestServer(t)
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
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"}))
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}))
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-apply")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	const canary = "apply-secret-canary"
	request := func(cookie *http.Cookie, method, path, body string, status int) string {
		t.Helper()
		w := tenantRequest(s, cookie, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), canary) {
			t.Fatalf("%s %s leaked a resolved value", method, path)
		}
		return w.Body.String()
	}
	oldID, newID := strings.Repeat("a", 64), strings.Repeat("e", 64)
	oldImage, newImage := "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64)
	created := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	gen := uint64(time.Now().Unix())
	inventory := func(sock *agentSocket, id, image string, at time.Time) {
		t.Helper()
		gen++
		writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{
			Generation: gen, Engine: protocol.Engine{Version: "1"},
			Containers: []protocol.Container{{ID: id, Name: "shop-web", ImageID: image, State: "running", ComposeProject: "shop", CreatedAt: at, Mounts: []protocol.Mount{}, Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}}},
			Images:     []protocol.Image{{ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{}}},
			Networks:   []protocol.Network{}, Volumes: []protocol.Volume{},
		})
	}
	// sync proves every frame written before it has been handled: the loop is sequential.
	sync := func(sock *agentSocket) {
		t.Helper()
		writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
		if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeHeartbeat {
			t.Fatalf("expected a heartbeat, got %s", f.Type)
		}
	}
	online := func(capabilities []string) *agentSocket {
		t.Helper()
		waitFor(t, func() bool { return !s.Connected(ag.id) })
		sock, reason := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
		if sock == nil {
			t.Fatalf("connect refused: %s", reason)
		}
		writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities})
		return sock
	}
	legacy := []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect}
	pulling := []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentPull}
	reconnect := func(capabilities []string) *agentSocket {
		t.Helper()
		sock := online(capabilities)
		inventory(sock, newID, newImage, created.Add(30*time.Minute))
		sync(sock)
		waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })
		return sock
	}

	sock := online([]string{protocol.CapabilityDeploymentApply})
	inventory(sock, oldID, oldImage, created)
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })
	sync(sock)

	base := "/api/organizations/a/environments/env-a/applications"
	importBody, _ := json.Marshal(map[string]string{"name": "shop", "compose": "services: {web: {image: nginx:1, environment: {TOKEN: " + canary + "}}}"})
	var app store.Application
	must(json.Unmarshal([]byte(request(admin, "POST", base, string(importBody), 201)), &app))
	adoption := base + "/" + app.ID + "/adoption"
	var preview store.AdoptionPreview
	must(json.Unmarshal([]byte(request(admin, "GET", adoption+"?endpoint="+ag.id+"&project=shop", "", 200)), &preview))
	adoptionBody, _ := json.Marshal(store.AdoptionRequest{EndpointID: ag.id, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	var instance store.ApplicationInstance
	must(json.Unmarshal([]byte(request(admin, "POST", adoption, string(adoptionBody), 201)), &instance))
	mapping := base + "/" + app.ID + "/mapping"
	var mapped store.ApplicationMapping
	must(json.Unmarshal([]byte(request(admin, "GET", mapping, "", 200)), &mapped))
	mappingBody, _ := json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: map[string]string{"web": oldID}})
	request(admin, "PUT", mapping, string(mappingBody), 204)
	deployments := base + "/" + app.ID + "/deployments"
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: instance.ID, MappingVersion: mapped.Version + 1, Revision: 1, Confirm: "shop"})
	// The frame cap is the server's: a client cannot name one.
	request(admin, "POST", deployments, strings.TrimSuffix(string(planBody), "}")+`,"max_frame_bytes":1048576}`, 400)
	plan := func() store.Deployment {
		t.Helper()
		var d store.Deployment
		must(json.Unmarshal([]byte(request(admin, "POST", deployments, string(planBody), 201)), &d))
		return d
	}
	state := func(id string) store.Deployment {
		t.Helper()
		var d store.Deployment
		must(json.Unmarshal([]byte(request(admin, "GET", deployments+"/"+id, "", 200)), &d))
		return d
	}

	planned := plan()
	apply := deployments + "/" + planned.ID + "/apply"
	request(nil, "POST", apply, `{"confirm":"shop"}`, 401)
	if w := tenantRequest(s, admin, "POST", apply, `{"confirm":"shop"}`, false); w.Code != 403 {
		t.Fatalf("apply without CSRF: %d", w.Code)
	}
	request(viewer, "POST", apply, `{"confirm":"shop"}`, 403)
	var applying store.Deployment
	must(json.Unmarshal([]byte(request(admin, "POST", apply, `{"confirm":"shop"}`, 202)), &applying))
	if applying.State != "applying" || applying.ID != planned.ID {
		t.Fatalf("the 202 body is not the applying row: %+v", applying)
	}
	frame := readEnvelope(t, ctx, sock.conn)
	if frame.Type != protocol.TypeDeploymentApply {
		t.Fatalf("expected %s, got %s", protocol.TypeDeploymentApply, frame.Type)
	}
	var sent protocol.DeploymentRequest
	must(json.Unmarshal(frame.Payload, &sent))
	if sent.Deployment != planned.ID || len(sent.Services) != 1 || sent.Services[0].Env["TOKEN"] != canary || sent.Services[0].ContainerName != "shop-web" {
		t.Fatalf("the frame did not carry the resolved plan: %+v", sent)
	}
	if got := state(planned.ID); got.State != "applying" {
		t.Fatalf("GET after apply: %+v", got)
	}
	request(admin, "POST", apply, `{"confirm":"shop"}`, 409)
	if body := request(admin, "POST", deployments, string(planBody), 409); !strings.Contains(body, `"deployment_in_progress"`) {
		t.Fatalf("planning over an applying row: %s", body)
	}

	// An answer for a deployment this endpoint was not given changes nothing. It is larger than
	// a control frame, which a result may be.
	steps := make([]protocol.DeploymentStep, 400)
	for i := range steps {
		steps[i] = protocol.DeploymentStep{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeFailed, Detail: strings.Repeat("d", 200)}
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: uuid.NewString(), Outcome: protocol.OutcomeFailed, Steps: steps, Services: []protocol.DeploymentIdentity{}})
	sync(sock)
	if got := state(planned.ID); got.State != "applying" {
		t.Fatalf("a foreign result moved the row: %+v", got)
	}
	// A small result that fails validation is dropped: the session stays and nothing changes.
	writeEnvelope(t, ctx, sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: planned.ID, Outcome: "bogus", Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}})
	sync(sock)
	if got := state(planned.ID); got.State != "applying" {
		t.Fatalf("an invalid result moved the row: %+v", got)
	}
	sock.conn.CloseNow()
	waitFor(t, func() bool { return state(planned.ID).State == protocol.OutcomeUnknown })
	// A result that validates but is past its byte bound ends the session too: the size check
	// is what refuses it.
	big := protocol.DeploymentResult{Deployment: planned.ID, Outcome: protocol.OutcomeFailed, Steps: make([]protocol.DeploymentStep, 700), Services: []protocol.DeploymentIdentity{}}
	for i := range big.Steps {
		big.Steps[i] = protocol.DeploymentStep{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeFailed, Detail: strings.Repeat("d", protocol.MaxDeploymentStepDetailBytes)}
	}
	if raw, _ := json.Marshal(big); big.Validate() != nil || len(raw) <= protocol.MaxDeploymentResultBytes {
		t.Fatalf("the oversized fixture must validate and exceed the bound: %d bytes", len(raw))
	}
	sock = online(legacy)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeDeploymentResult, big)
	if _, _, err := sock.conn.Read(ctx); err == nil {
		t.Fatal("an oversized result frame was accepted")
	}
	if got := state(planned.ID); got.State != protocol.OutcomeUnknown || got.SettledAt != nil {
		t.Fatalf("an oversized result moved the row: %+v", got)
	}

	// A result that validates but names an image the plan did not pin leaves the row unknown
	// with a fixed detail and an audit row; the session stays.
	sock = online(legacy)
	replacedAt := created.Add(30 * time.Minute)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{
		Deployment: planned.ID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{{Service: "web", ContainerID: newID, ImageID: oldImage, CreatedUnix: replacedAt.Unix()}},
	})
	sync(sock)
	if got := state(planned.ID); got.State != protocol.OutcomeUnknown || got.Detail != "the host's result did not match the plan; inspect the host" {
		t.Fatalf("a mismatched result: %+v", got)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
	must(err)
	refused := 0
	for _, rec := range records {
		if rec.UserID == "agent:"+ag.id && rec.Resource == app.ID+"/deployments/"+planned.ID && rec.Details == "outcome=refused" {
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("refusal audit rows: %d", refused)
	}

	// The real answer still settles the unknown row and rebinds the replaced container.
	inventory(sock, newID, newImage, replacedAt)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{
		Deployment: planned.ID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{{Service: "web", ContainerID: newID, ImageID: newImage, CreatedUnix: replacedAt.Unix()}},
	})
	sync(sock)
	waitFor(t, func() bool { return state(planned.ID).State == protocol.OutcomeSucceeded })
	must(json.Unmarshal([]byte(request(admin, "GET", mapping, "", 200)), &mapped))
	if len(mapped.Preview.Containers) != 1 || mapped.Preview.Containers[0].ID != newID || mapped.Bindings["web"] != newID {
		t.Fatalf("the mapping still names the replaced container: %+v", mapped)
	}
	var instances []store.ApplicationInstance
	must(json.Unmarshal([]byte(request(admin, "GET", base+"/instances", "", 200)), &instances))
	if len(instances) != 1 || instances[0].CurrentRevision != 1 {
		t.Fatalf("the instance did not record the applied revision: %+v", instances)
	}

	// The socket ending mid-apply leaves the row unknown.
	second := plan()
	request(admin, "POST", deployments+"/"+second.ID+"/apply", `{"confirm":"shop"}`, 202)
	if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeDeploymentApply {
		t.Fatalf("expected the second apply frame, got %s", f.Type)
	}
	sock.conn.CloseNow()
	waitFor(t, func() bool { return state(second.ID).State == protocol.OutcomeUnknown })

	// An agent that does not advertise the capability is never sent a plan.
	sock = online(nil)
	inventory(sock, newID, newImage, replacedAt)
	sync(sock)
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })
	third := plan()
	request(admin, "POST", deployments+"/"+third.ID+"/apply", `{"confirm":"shop"}`, 501)
	if got := state(third.ID); got.State != "planned" {
		t.Fatalf("a refused apply moved the row: %+v", got)
	}
	sock.conn.CloseNow()

	// A frame past 192 KiB goes only to an agent with deployment.pull: an older one would close
	// its session on it. One stored value feeds four variables and JSON escapes '<' as six
	// bytes, so the frame is ~240 KiB while the stored values stay inside their bound.
	env := map[string]store.ApplicationSecretRef{}
	for _, n := range []string{"A", "B", "C", "D"} {
		env["V"+n] = store.ApplicationSecretRef{SecretRef: "web-a"}
	}
	spec := store.ApplicationSpec{Kind: "compose.v1", Services: []store.ApplicationService{{Name: "web", Image: "nginx:1", Environment: env}}}
	_, err = ts.ReplaceApplicationRevision(ctx, store.TenantAccess{ActorID: "usr_envadmin", OrganizationID: "a", EnvironmentID: "env-a"}, app.ID, 1, spec, map[string]string{"web-a": strings.Repeat("<", 10000)}, cfg.Security.EncryptionKey)
	must(err)
	// A frame past 192 KiB goes only to an agent with deployment.pull: an older one would close
	// its session on it. The plan measures against the connected agent's cap and apply measures
	// again, since the agent can change between them.
	sock.conn.CloseNow()
	sock = reconnect(legacy)
	must(json.Unmarshal([]byte(request(admin, "GET", mapping, "", 200)), &mapped))
	mappingBody, _ = json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: map[string]string{"web": newID}})
	request(admin, "PUT", mapping, string(mappingBody), 204)
	planBody, _ = json.Marshal(store.PlanRequest{InstanceID: instance.ID, MappingVersion: mapped.Version + 1, Revision: 2, Confirm: "shop"})
	if body := request(admin, "POST", deployments, string(planBody), 409); !strings.Contains(body, "frame_too_large") {
		t.Fatalf("a legacy agent's plan: %s", body)
	}
	sock.conn.CloseNow()
	sock = reconnect(pulling)
	wide := plan()
	sock.conn.CloseNow()
	sock = reconnect(legacy)
	request(admin, "POST", deployments+"/"+wide.ID+"/apply", `{"confirm":"shop"}`, 400)
	if got := state(wide.ID); got.State != "planned" {
		t.Fatalf("an oversized frame for a legacy agent moved the row: %+v", got)
	}
	sock.conn.CloseNow()
	sock = reconnect(pulling)
	request(admin, "POST", deployments+"/"+wide.ID+"/apply", `{"confirm":"shop"}`, 202)
	sock.conn.CloseNow()
}
