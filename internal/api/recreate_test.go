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
	"sync/atomic"
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
	localImage    = "sha256:" + strings.Repeat("7", 64) // built on the host: a tag, no digest
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
				Ports: []protocol.Port{{HostIP: "0.0.0.0", Host: 8080, Container: 80, Protocol: "tcp"}}, Mounts: []protocol.Mount{{Kind: protocol.MountBind, Source: "/srv/web", Target: "/data", ReadOnly: true}}},
			{ID: directDB, Name: "db", ImageID: directImage, CreatedAt: directCreated, State: "running", Labels: map[string]string{}, Networks: []string{"bridge"},
				Ports: []protocol.Port{{HostIP: "0.0.0.0", Host: 5432, Container: 5432, Protocol: "tcp"}}, Mounts: []protocol.Mount{}},
		},
		Images: []protocol.Image{{ID: directImage, Tags: []string{"ghcr.io/acme/web:1"}, Digests: []string{"ghcr.io/acme/web@" + directDigest}},
			{ID: localImage, Tags: []string{"web-local:dev"}, Digests: []string{}}},
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
		Mounts: []protocol.Mount{{Kind: protocol.MountBind, Source: "/srv/web", Target: "/data", ReadOnly: true}}, Unsupported: []string{},
	}
}

func recreatePath(f terminalFixture) string {
	return "/api/organizations/a/endpoints/" + f.ag.id + "/containers/" + terminalSpec.Container + "/recreate"
}

func TestRecreateAcceptsANewNameWithTheOldNameConfirmed(t *testing.T) {
	f := directFixture(t, directCaps)
	w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(directSpec("renamed-web")), true)
	if w.Code != 202 {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if req := applyFrame(t, f); req.Services[0].ContainerName != "renamed-web" || req.Services[0].Replaces.ContainerID != terminalSpec.Container {
		t.Fatalf("rename lost its target: %+v", req.Services[0])
	}
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
	spec := directSpec("web")
	spec.Labels["CI_DOCKER_VERSION"] = strings.Repeat("v", 981)
	w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(spec), true)
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
	if svc.Explicit.Labels["CI_DOCKER_VERSION"] != spec.Labels["CI_DOCKER_VERSION"] {
		t.Fatal("long build label lost in explicit frame")
	}
	for k := range svc.Explicit.Labels {
		if strings.HasPrefix(k, "com.docker.compose.") {
			t.Fatalf("a Compose label was added: %s", k)
		}
	}
	// A plain command.result is not how a direct command settles.
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeResult, protocol.Result{ID: cmd.ID, Outcome: protocol.OutcomeSucceeded})
	syncAgent(t, f)
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
	// The Activity list shows it under the container it edited and the one that replaced it.
	for _, id := range []string{terminalSpec.Container, strings.Repeat("e", 64)} {
		activityShows(t, f, id, cmd.ID)
	}
	if got.ContainerID != terminalSpec.Container || got.ResultContainerID != strings.Repeat("e", 64) {
		t.Fatalf("container IDs: %+v", got.Command)
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

func activityShows(t *testing.T, f terminalFixture, container, id string) {
	t.Helper()
	r := tenantRequest(f.s, f.admin, "GET", "/api/organizations/a/endpoints/"+f.ag.id+"/commands?container="+container, "", false)
	var list []directCommand
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].ID != id || list[0].Result == nil {
		t.Fatalf("activity for %s: %d %s", container, r.Code, r.Body.String())
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
	bad = directSpec("fresh")
	bad.User = strings.Repeat("u", 257)
	blockers(t, tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(bad), true), "spec_invalid:user_working_dir_or_hostname")
	spec.Mounts = append(spec.Mounts, protocol.Mount{Kind: protocol.MountBind, Source: "/srv/logs", Target: "/logs"})
	taken := spec
	taken.Name = "db"
	blockers(t, tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(taken, "/srv/web", "/srv/logs"), true), "name_taken")
	acknowledged := func() []string {
		rows, _, _ := f.st.Audit().ListAuditRecords(f.ctx, 0, 200)
		var paths []string
		for _, r := range rows {
			if r.Action == "container.bind.acknowledged" && r.Resource == f.ag.id+"/fresh" && r.Result == "success" {
				paths = append(paths, r.Details)
			}
		}
		slices.Sort(paths)
		return paths
	}
	if got := acknowledged(); len(got) != 0 {
		t.Fatalf("a refused run audited acknowledgements: %v", got)
	}
	// The refusals sent nothing: the next frame the agent reads is this run's.
	// "/srv/unused" matches no mount: acknowledged, but nothing to audit.
	w := tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(spec, "/srv/web", "/srv/logs", "/srv/unused"), true)
	if w.Code != 202 {
		t.Fatalf("acknowledged run: %d %s", w.Code, w.Body.String())
	}
	var cmd store.Command
	_ = json.Unmarshal(w.Body.Bytes(), &cmd)
	req := applyFrame(t, f)
	svc := req.Services[0]
	if svc.Replaces != (protocol.InspectionTarget{}) || svc.ContainerName != "fresh" || !slices.Equal(svc.Explicit.AcknowledgedBinds, []string{"/srv/web", "/srv/logs", "/srv/unused"}) {
		t.Fatalf("run frame: %+v", svc)
	}
	if got := acknowledged(); !slices.Equal(got, []string{"/srv/logs", "/srv/web"}) {
		t.Fatalf("acknowledgement rows: %v", got)
	}
	rows, _, _ := f.st.Audit().ListAuditRecords(f.ctx, 0, 200)
	found := false
	for _, r := range rows {
		found = found || (r.Action == "container.configure" && r.Result == "success" && r.Resource == f.ag.id+"/fresh" && strings.Contains(r.Details, "binds=2"))
	}
	if !found {
		t.Fatal("the run's audit row does not carry binds=2")
	}
	// Once settled, the run is in the new container's Activity.
	created := strings.Repeat("f", 64)
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Outcome: protocol.OutcomeSucceeded,
		Steps: []protocol.DeploymentStep{{Service: "direct", Step: protocol.StepStart, Outcome: protocol.OutcomeSucceeded}}, Services: []protocol.DeploymentIdentity{{Service: "direct", ContainerID: created, ImageID: directImage, CreatedUnix: time.Now().Unix()}}})
	syncAgent(t, f)
	activityShows(t, f, created, cmd.ID)
}

// The host's image is kept only when it is the image the spec names: its digest when the spec
// keeps one, otherwise a tag equal to the reference. Anything else pulls.
func TestDirectImageKeepsOnlyTheImageTheSpecNames(t *testing.T) {
	pulled := "sha256:" + strings.Repeat("f", 64)
	for _, tc := range []struct {
		name              string
		reference, digest string
		imageID           string
		wantLocal         string
		wantPullDigest    string
	}{
		{"digest-less image tagged as the reference", "web-local:dev", "", localImage, localImage, ""},
		{"digest-less image, reference changed", "web-local:prod", "", localImage, "", pulled},
		// A container KyYard created names its image by ID, so a re-read reports that as the reference.
		{"reference is the image's own ID", localImage, "", localImage, localImage, ""},
		{"kept digest listed by the image", "ghcr.io/acme/web:1", directDigest, directImage, directImage, ""},
		{"kept digest the image does not list", "ghcr.io/acme/web:1", "sha256:" + strings.Repeat("9", 64), directImage, "", "sha256:" + strings.Repeat("9", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := directFixture(t, directCaps)
			api.SetDigestResolverForTest(f.s, fixedResolver{pulled})
			if err := f.st.Tenancy().SetAnonymousPull(f.ctx, store.TenantAccess{ActorID: "usr_execadmin", OrganizationID: "a"}, true); err != nil {
				t.Fatal(err)
			}
			spec := directSpec("fresh")
			spec.Image, spec.ImageID, spec.Ports, spec.Mounts = protocol.ImagePull{Reference: tc.reference, Digest: tc.digest}, tc.imageID, []protocol.Port{}, []protocol.Mount{}
			if w := tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(spec), true); w.Code != 202 {
				t.Fatalf("run: %d %s", w.Code, w.Body.String())
			}
			svc := applyFrame(t, f).Services[0]
			if svc.ImageID != tc.wantLocal || (tc.wantPullDigest == "") != (svc.Pull == nil) || (svc.Pull != nil && svc.Pull.Digest != tc.wantPullDigest) {
				t.Fatalf("image %q pull %+v", svc.ImageID, svc.Pull)
			}
		})
	}
}

// A recreate whose spec names no image ID falls back to the target's image only when the spec
// keeps a digest or names that ID; a cleared digest with a tag reference pulls.
func TestRecreateImageFallback(t *testing.T) {
	pulled := "sha256:" + strings.Repeat("f", 64)
	for _, tc := range []struct {
		name, reference, digest string
		wantLocal, wantPull     string
	}{
		{"cleared digest, tag reference", "ghcr.io/acme/web:1", "", "", pulled},
		{"kept digest", "ghcr.io/acme/web:1", directDigest, directImage, ""},
		{"reference is the target's image ID", directImage, "", directImage, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := directFixture(t, directCaps)
			api.SetDigestResolverForTest(f.s, fixedResolver{pulled})
			if err := f.st.Tenancy().SetAnonymousPull(f.ctx, store.TenantAccess{ActorID: "usr_execadmin", OrganizationID: "a"}, true); err != nil {
				t.Fatal(err)
			}
			spec := directSpec("web")
			spec.Image, spec.ImageID = protocol.ImagePull{Reference: tc.reference, Digest: tc.digest}, ""
			if w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(spec), true); w.Code != 202 {
				t.Fatalf("recreate: %d %s", w.Code, w.Body.String())
			}
			svc := applyFrame(t, f).Services[0]
			if svc.ImageID != tc.wantLocal || (tc.wantPull == "") != (svc.Pull == nil) || (svc.Pull != nil && svc.Pull.Digest != tc.wantPull) {
				t.Fatalf("image %q pull %+v", svc.ImageID, svc.Pull)
			}
		})
	}
}

// Making a read-only bind writable grants the container something new.
func TestRecreateNeedsAcknowledgementToMakeABindWritable(t *testing.T) {
	f := directFixture(t, directCaps)
	spec := directSpec("web")
	spec.Mounts[0].ReadOnly = false
	blockers(t, tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(spec), true), "bind_unacknowledged")
	if w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(spec, "/srv/web"), true); w.Code != 202 {
		t.Fatalf("acknowledged: %d %s", w.Code, w.Body.String())
	}
}

// An old bind covers a new one only at the same target: the agent's rule.
func TestRecreateNeedsAcknowledgementToMoveABind(t *testing.T) {
	f := directFixture(t, directCaps)
	spec := directSpec("web")
	spec.Mounts[0].Target = "/elsewhere"
	blockers(t, tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(spec), true), "bind_unacknowledged")
	assertNoFrame(t, f)
}

// I8: host-level settings need KY_CONTAINER_ALLOW_PRIVILEGED; a plain spec never does.
func TestDirectCommandsGateHostLevelSettings(t *testing.T) {
	for name, set := range map[string]func(*protocol.ContainerConfiguration){
		"privileged": func(s *protocol.ContainerConfiguration) { s.Privileged = true },
		"devices": func(s *protocol.ContainerConfiguration) {
			s.Devices = []protocol.Device{{Host: "/dev/fuse", Container: "/dev/fuse", Permissions: "rwm"}}
		},
		"security_opt": func(s *protocol.ContainerConfiguration) { s.SecurityOpt = []string{"apparmor=unconfined"} },
		"cap_add":      func(s *protocol.ContainerConfiguration) { s.CapAdd = []string{"CAP_SYS_ADMIN"} },
		"host network": func(s *protocol.ContainerConfiguration) { s.NetworkMode, s.Networks = "host", nil },
		"docker socket": func(s *protocol.ContainerConfiguration) {
			s.Mounts = append(s.Mounts, protocol.Mount{Kind: protocol.MountBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := directFixture(t, directCaps)
			spec := directSpec("web")
			set(&spec)
			blockers(t, tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(spec, "/var/run/docker.sock"), true), "privileged_disabled")
			api.SetAllowPrivilegedForTest(f.s, true)
			if w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(spec, "/var/run/docker.sock"), true); w.Code != 202 {
				t.Fatalf("allowed: %d %s", w.Code, w.Body.String())
			}
		})
	}
	f := directFixture(t, directCaps)
	spec := directSpec("web")
	spec.CapAdd = []string{"NET_BIND_SERVICE", "CAP_CHOWN"} // within Docker's defaults
	spec.SecurityOpt = []string{"no-new-privileges"}        // hardening
	if w := tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(spec), true); w.Code != 202 {
		t.Fatalf("plain spec: %d %s", w.Code, w.Body.String())
	}
}

// The host's or another container's namespace is a network mode, never an attachment: an
// attachment named so is refused before the host-level gate, with the opt-in or without.
func TestDirectCommandsRefuseANamespaceAttachment(t *testing.T) {
	for _, name := range []string{"host", "container:" + strings.Repeat("a", 64)} {
		for _, allow := range []bool{false, true} {
			f := directFixture(t, directCaps)
			api.SetAllowPrivilegedForTest(f.s, allow)
			spec := directSpec("web")
			spec.NetworkMode, spec.Networks = "", []protocol.NetworkAttachmentSpec{{Name: name}}
			blockers(t, tenantRequest(f.s, f.admin, "POST", recreatePath(f), recreateBody(spec), true), "spec_invalid:networks")
			assertNoFrame(t, f)
		}
	}
}

type countingResolver struct{ calls atomic.Int32 }

func (r *countingResolver) Head(context.Context, registry.Reference, *registry.Credential, bool) (string, error) {
	r.calls.Add(1)
	return "sha256:" + strings.Repeat("f", 64), nil
}

// A request the store would refuse spends no registry call.
func TestRefusedRunSpendsNoRegistryCall(t *testing.T) {
	f := directFixture(t, directCaps)
	resolver := &countingResolver{}
	api.SetDigestResolverForTest(f.s, resolver)
	if err := f.st.Tenancy().SetAnonymousPull(f.ctx, store.TenantAccess{ActorID: "usr_execadmin", OrganizationID: "a"}, true); err != nil {
		t.Fatal(err)
	}
	spec := directSpec("fresh")
	spec.Image, spec.ImageID, spec.Mounts = protocol.ImagePull{Reference: "ghcr.io/acme/web:2"}, "", []protocol.Mount{}
	spec.Ports = []protocol.Port{}
	raw, _ := json.Marshal(map[string]any{"spec": spec, "confirm": "not-fresh"})
	if w := tenantRequest(f.s, f.admin, "POST", runPath(f), string(raw), true); w.Code != 400 {
		t.Fatalf("wrong confirm: %d %s", w.Code, w.Body.String())
	}
	if n := resolver.calls.Load(); n != 0 {
		t.Fatalf("%d registry calls for a refused run", n)
	}
	if w := tenantRequest(f.s, f.admin, "POST", runPath(f), runBody(spec), true); w.Code != 202 || resolver.calls.Load() != 1 {
		t.Fatalf("run: %d %s, %d calls", w.Code, w.Body.String(), resolver.calls.Load())
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
