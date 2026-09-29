package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/google/uuid"
)

var (
	directImage   = terminalSpec.ImageID
	directDigest  = "sha256:" + strings.Repeat("c", 64)
	directDB      = strings.Repeat("d", 64)
	directCreated = time.Unix(1700000000, 0).UTC()
	directCaps    = []string{protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull, protocol.CapabilityContainerInspect, protocol.CapabilityContainerConfigure}
)

// directFixture is a connected Docker agent whose inventory holds "web" (the edit target, with a
// bind and a published port) and "db", both on a pulled image the inventory lists.
func directFixture(t *testing.T, capabilities []string) terminalFixture {
	t.Helper()
	f := newTerminalFixture(t)
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities})
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInventory, protocol.Snapshot{
		Generation: uint64(time.Now().Unix()) + 2, ObservedAt: time.Now(), Engine: protocol.Engine{Version: "1"},
		Containers: []protocol.Container{
			{ID: terminalSpec.Container, Name: "web", ImageID: directImage, CreatedAt: directCreated, State: "running", Labels: map[string]string{}, Networks: []string{"bridge"},
				Ports: []protocol.Port{{HostIP: "0.0.0.0", Host: 8080, Container: 80, Protocol: "tcp"}}, Mounts: []protocol.Mount{{Kind: protocol.MountBind, Source: "/srv/web", Target: "/data"}}},
			{ID: directDB, Name: "db", ImageID: directImage, CreatedAt: directCreated, State: "running", Labels: map[string]string{}, Networks: []string{"bridge"},
				Ports: []protocol.Port{{HostIP: "0.0.0.0", Host: 5432, Container: 5432, Protocol: "tcp"}}, Mounts: []protocol.Mount{}},
		},
		Images:   []protocol.Image{{ID: directImage, Tags: []string{"ghcr.io/acme/web:1"}, Digests: []string{"ghcr.io/acme/web@" + directDigest}}},
		Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
	})
	syncAgent(t, f)
	return f
}

// syncAgent proves every frame written before it has been handled: the loop is sequential.
func syncAgent(t *testing.T, f terminalFixture) {
	t.Helper()
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeHeartbeat, nil)
	if e := readEnvelope(t, f.ctx, f.ag.conn); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("expected a heartbeat, got %s", e.Type)
	}
}

func directSpec(name string) protocol.ContainerConfiguration {
	return protocol.ContainerConfiguration{
		Name: name, Image: protocol.ImagePull{Reference: "ghcr.io/acme/web:1", Digest: directDigest, Tag: "ghcr.io/acme/web:1"}, ImageID: directImage,
		Command: []string{"serve"}, Env: []protocol.EnvEntry{{Name: "API_KEY", Value: configurationSentinel}}, Labels: map[string]string{"app": "web"},
		Restart: "unless-stopped", NetworkMode: "bridge", Networks: []protocol.NetworkAttachmentSpec{{Name: "bridge"}},
		Ports:  []protocol.Port{{HostIP: "0.0.0.0", Host: 8080, Container: 80, Protocol: "tcp"}},
		Mounts: []protocol.Mount{{Kind: protocol.MountBind, Source: "/srv/web", Target: "/data"}}, Unsupported: []string{},
	}
}

func recreatePath(f terminalFixture) string {
	return "/api/organizations/a/endpoints/" + f.ag.id + "/containers/" + terminalSpec.Container + "/recreate"
}
func runPath(f terminalFixture) string {
	return "/api/organizations/a/endpoints/" + f.ag.id + "/containers"
}
func recreateBody(spec protocol.ContainerConfiguration, acknowledge ...string) string {
	raw, _ := json.Marshal(map[string]any{"expects": map[string]any{"image_id": directImage, "created_unix": directCreated.Unix(), "state": "running"}, "spec": spec, "acknowledge_binds": acknowledge, "confirm": "web"})
	return string(raw)
}
func runBody(spec protocol.ContainerConfiguration, acknowledge ...string) string {
	raw, _ := json.Marshal(map[string]any{"spec": spec, "acknowledge_binds": acknowledge, "confirm": spec.Name})
	return string(raw)
}

// applyFrame reads the next frame the agent receives, which must be a deployment.apply.
func applyFrame(t *testing.T, f terminalFixture) protocol.DeploymentRequest {
	t.Helper()
	e := readEnvelope(t, f.ctx, f.ag.conn)
	var req protocol.DeploymentRequest
	if e.Type != protocol.TypeDeploymentApply || json.Unmarshal(e.Payload, &req) != nil || req.Validate(time.Now()) != nil {
		t.Fatalf("not a valid deployment.apply: %s %s", e.Type, e.Payload)
	}
	return req
}

type directCommand struct {
	store.Command
	Result *struct {
		Code  string                    `json:"code"`
		Steps []protocol.DeploymentStep `json:"steps"`
	} `json:"result"`
}

func blockers(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		Code     string   `json:"code"`
		Blockers []string `json:"blockers"`
	}
	if w.Code != 422 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Code != "invalid_spec" || !slices.Contains(body.Blockers, want) {
		t.Fatalf("want 422 invalid_spec listing %s, got %d %s", want, w.Code, w.Body.String())
	}
}

func TestRecreateSendsAnExplicitFrameAndSettlesFromTheResult(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	f := directFixture(t, directCaps)
	w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(directSpec("web")), true)
	if w.Code != 202 {
		t.Fatalf("recreate: %d %s", w.Code, w.Body.String())
	}
	var cmd store.Command
	if err := json.Unmarshal(w.Body.Bytes(), &cmd); err != nil || cmd.Action != "container.recreate" || cmd.ContainerID != terminalSpec.Container || cmd.Outcome != "" {
		t.Fatalf("command: %v %s", err, w.Body.String())
	}
	req := applyFrame(t, f)
	svc := req.Services[0]
	want := protocol.InspectionTarget{ContainerID: terminalSpec.Container, ImageID: directImage, CreatedUnix: directCreated.Unix()}
	if !req.Explicit || req.Project != protocol.ExplicitProject || req.Revision != protocol.ExplicitRevision || len(req.Services) != 1 || req.Deployment != cmd.ID || req.RequestID != cmd.RequestID ||
		svc.Explicit == nil || svc.Replaces != want || svc.ContainerName != "web" || svc.ImageID != directImage || svc.Pull != nil || svc.Env["API_KEY"] != configurationSentinel || svc.Restart != "unless-stopped" {
		t.Fatalf("frame: %+v %+v", req, svc)
	}
	for k := range svc.Explicit.Labels {
		if strings.HasPrefix(k, "com.docker.compose.") {
			t.Fatalf("a Compose label was added: %s", k)
		}
	}
	// A second edit on the endpoint waits for the first.
	if w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(directSpec("web")), true); w.Code != 409 || !strings.Contains(w.Body.String(), "command_in_progress") {
		t.Fatalf("second recreate: %d %s", w.Code, w.Body.String())
	}
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{
		Deployment: req.Deployment, RequestID: req.RequestID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: "direct", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded}, {Service: "direct", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}, {Service: "direct", Step: protocol.StepStart, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{{Service: "direct", ContainerID: strings.Repeat("e", 64), ImageID: directImage, CreatedUnix: time.Now().Unix()}},
	})
	syncAgent(t, f)
	var got directCommand
	r := tenantRequest(f.s, f.admin, "GET", "/api/organizations/a/endpoints/"+f.ag.id+"/commands/"+cmd.ID, "", false)
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &got) != nil || got.Outcome != protocol.OutcomeSucceeded || got.SettledAt == nil || got.Result == nil || len(got.Result.Steps) != 3 {
		t.Fatalf("settled command: %d %s", r.Code, r.Body.String())
	}
	// The Activity list shows it under the container it edited.
	r = tenantRequest(f.s, f.admin, "GET", "/api/organizations/a/endpoints/"+f.ag.id+"/commands?container="+terminalSpec.Container, "", false)
	var list []directCommand
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].ID != cmd.ID || list[0].Result == nil {
		t.Fatalf("activity: %d %s", r.Code, r.Body.String())
	}
	rows, _, err := f.st.Audit().ListAuditRecords(f.ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if strings.Contains(row.Details, configurationSentinel) || strings.Contains(row.Resource, configurationSentinel) {
			t.Fatalf("audit row carries an env value: %+v", row)
		}
		if row.Action == "container.configure" && row.Result == "success" {
			found = true
			if row.Resource != f.ag.id+"/"+terminalSpec.Container || !strings.HasPrefix(row.Details, "image="+directImage+" binds=0 fields=") || !strings.Contains(row.Details, "env") {
				t.Fatalf("success row: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("no container.configure success row")
	}
	log.SetOutput(io.Discard)
	if strings.Contains(logs.String(), configurationSentinel) {
		t.Fatal("an env value reached the log")
	}
	// The edit is over: another may start.
	if w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(directSpec("web")), true); w.Code != 202 {
		t.Fatalf("after settle: %d %s", w.Code, w.Body.String())
	}
}

func TestRecreateDeniedBelowOrganizationAdmin(t *testing.T) {
	f := directFixture(t, directCaps)
	token, _ := serviceAccess(t, f)
	if w := bearer(f.s, "POST", recreatePath(f), token); w.Code != 403 {
		t.Fatalf("service token: %d %s", w.Code, w.Body.String())
	}
	if err := f.st.Tenancy().SetMembership(f.ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_execadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, body string }{{recreatePath(f), recreateBody(directSpec("web"))}, {runPath(f), runBody(directSpec("fresh"))}} {
		if w := tenantRequest(f.s, f.admin, "POST", tc.path, tc.body, true); w.Code != 403 {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	rows, _, _ := f.st.Audit().ListAuditRecords(f.ctx, 0, 100)
	denied := 0
	for _, r := range rows {
		if r.UserID == "usr_execadmin" && r.Action == "container.configure" && r.Result == "denied" {
			denied++
		}
	}
	if denied != 2 {
		t.Fatalf("%d denied rows, want 2", denied)
	}
	assertNoFrame(t, f)
}

// assertNoFrame ends the agent's socket: a Read cancelled by its context closes it.
func assertNoFrame(t *testing.T, f terminalFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 200*time.Millisecond)
	defer cancel()
	if _, _, err := f.ag.conn.Read(ctx); err == nil {
		t.Fatal("a refused request sent a frame to the agent")
	}
}

func TestRecreateRefusesManagedContainer(t *testing.T) {
	h := newPlanHost(t, append(inspecting, protocol.CapabilityContainerConfigure), "web")
	spec := directSpec("shop-web")
	spec.ImageID, spec.Image.Digest = h.targets[0].ImageID, ""
	body, _ := json.Marshal(map[string]any{"expects": map[string]any{"image_id": h.targets[0].ImageID, "created_unix": h.targets[0].CreatedUnix, "state": "running"}, "spec": spec, "confirm": "shop-web"})
	w := tenantRequest(h.s, h.admin, "POST", "/api/organizations/a/endpoints/"+h.ag.id+"/containers/"+h.targets[0].ContainerID+"/recreate", string(body), true)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "application_managed") {
		t.Fatalf("managed container: %d %s", w.Code, w.Body.String())
	}
}

func TestRecreateRefusesAnIncompleteConfiguration(t *testing.T) {
	f := directFixture(t, directCaps)
	spec := directSpec("web")
	spec.Unsupported = []string{"env_truncated"}
	blockers(t, tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(spec), true), "configuration_incomplete")
	// The form dropped the list, but this actor's last read of the container reported it.
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- tenantRequest(f.s, f.admin, "GET", strings.TrimSuffix(recreatePath(f), "recreate")+"configuration", "", false)
	}()
	grant := configurationGrant(t, f)
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeConfigurationResult, configurationReply(grant, "env_truncated"))
	if w := <-response; w.Code != 200 {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
	blockers(t, tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(directSpec("web")), true), "configuration_incomplete")
	assertNoFrame(t, f)
}

func TestRunRefusesTakenNamesPortsAndUnacknowledgedBinds(t *testing.T) {
	f := directFixture(t, directCaps)
	blockers(t, tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(directSpec("db")), true), "name_taken")
	spec := directSpec("fresh")
	spec.Ports = []protocol.Port{{Host: 5432, Container: 5432, Protocol: "tcp"}}
	spec.Mounts = []protocol.Mount{}
	blockers(t, tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(spec), true), "port_conflict")
	spec = directSpec("fresh")
	spec.Ports = []protocol.Port{{Container: 80, Protocol: "tcp"}}
	blockers(t, tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(spec), true), "bind_unacknowledged")
	bad := directSpec("fresh")
	bad.Restart = "sometimes"
	blockers(t, tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(bad), true), "spec_invalid:restart")
	// The refusals sent nothing: the next frame the agent reads is this run's.
	w := tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(spec, "/srv/web"), true)
	if w.Code != 202 {
		t.Fatalf("acknowledged run: %d %s", w.Code, w.Body.String())
	}
	req := applyFrame(t, f)
	svc := req.Services[0]
	if svc.Replaces != (protocol.InspectionTarget{}) || svc.ContainerName != "fresh" || len(svc.Explicit.AcknowledgedBinds) != 1 || svc.Explicit.AcknowledgedBinds[0] != "/srv/web" {
		t.Fatalf("run frame: %+v", svc)
	}
	rows, _, _ := f.st.Audit().ListAuditRecords(f.ctx, 0, 100)
	found := false
	for _, r := range rows {
		found = found || (r.Action == "container.configure" && r.Result == "success" && r.Resource == f.ag.id+"/fresh" && strings.Contains(r.Details, "binds=1"))
	}
	if !found {
		t.Fatal("the run's audit row does not carry binds=1")
	}
}

type fixedResolver struct{ digest string }

func (r fixedResolver) Head(context.Context, registry.Reference, *registry.Credential, bool) (string, error) {
	return r.digest, nil
}

// A spec without a digest is pulled at the registry's current digest, under the plan's rules.
func TestRunPullsTheReferenceAtTheRegistryDigest(t *testing.T) {
	f := directFixture(t, directCaps)
	spec := directSpec("fresh")
	spec.Image, spec.ImageID, spec.Ports, spec.Mounts = protocol.ImagePull{Reference: "ghcr.io/acme/web:2"}, "", []protocol.Port{}, []protocol.Mount{}
	pulled := "sha256:" + strings.Repeat("f", 64)
	api.SetDigestResolverForTest(f.s, fixedResolver{pulled})
	// Anonymous pulls are off by default and no registry is configured.
	blockers(t, tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(spec), true), "image_unresolved")
	if err := f.st.Tenancy().SetAnonymousPull(f.ctx, store.TenantAccess{ActorID: "usr_execadmin", OrganizationID: "a"}, true); err != nil {
		t.Fatal(err)
	}
	if w := tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(spec), true); w.Code != 202 {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	req := applyFrame(t, f)
	svc := req.Services[0]
	if svc.ImageID != "" || svc.Pull == nil || *svc.Pull != (protocol.ImagePull{Reference: "ghcr.io/acme/web@" + pulled, Digest: pulled, Tag: "ghcr.io/acme/web:2"}) {
		t.Fatalf("pull: %+v", svc.Pull)
	}
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Outcome: protocol.OutcomeFailed, Code: protocol.ResultStepFailed,
		Steps: []protocol.DeploymentStep{{Service: "direct", Step: protocol.StepPull, Outcome: protocol.OutcomeFailed, Code: "pull_failed"}}, Services: []protocol.DeploymentIdentity{}})
	syncAgent(t, f)
	// A kept digest the host no longer has is pulled as seen, not re-resolved.
	kept := "sha256:" + strings.Repeat("9", 64)
	spec.Image.Digest, spec.ImageID = kept, "sha256:"+strings.Repeat("8", 64)
	if w := tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(spec), true); w.Code != 202 {
		t.Fatalf("kept digest: %d %s", w.Code, w.Body.String())
	}
	if pull := applyFrame(t, f).Services[0].Pull; pull == nil || *pull != (protocol.ImagePull{Reference: "ghcr.io/acme/web@" + kept, Digest: kept}) {
		t.Fatalf("kept digest pull: %+v", pull)
	}
}

func TestRecreateOnAnOfflineEndpoint(t *testing.T) {
	f := directFixture(t, directCaps)
	f.ag.conn.CloseNow()
	waitFor(t, func() bool { return !f.s.Connected(f.ag.id) })
	if w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(directSpec("web")), true); w.Code != 409 || !strings.Contains(w.Body.String(), "endpoint_offline") {
		t.Fatalf("offline: %d %s", w.Code, w.Body.String())
	}
}

func TestDirectCommandsNeedCapabilities(t *testing.T) {
	f := directFixture(t, []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect})
	if w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(directSpec("web")), true); w.Code != 501 {
		t.Fatalf("no configure capability: %d %s", w.Code, w.Body.String())
	}
	// Reads and writes share one budget of 12 a minute.
	for i := 0; i < 11; i++ {
		tenantRequest(f.s, f.admin, "GET", strings.TrimSuffix(recreatePath(f), "recreate")+"configuration", "", false)
	}
	if w := tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(directSpec("fresh")), true); w.Code != 429 {
		t.Fatalf("13th configuration request: %d %s", w.Code, w.Body.String())
	}
}

// A result nobody is waiting for is logged and changes nothing; the socket stays up.
func TestUnknownDeploymentResultIsLogged(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	f := directFixture(t, directCaps)
	id := uuid.NewString()
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: id, RequestID: "unknown-request", Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}})
	syncAgent(t, f)
	log.SetOutput(io.Discard)
	if !strings.Contains(logs.String(), "late deployment result ignored: "+id) {
		t.Fatalf("log: %s", logs.String())
	}
}
