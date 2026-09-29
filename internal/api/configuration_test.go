package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const configurationSentinel = "SENTINEL_SECRET_VALUE"

func configurationFixture(t *testing.T) terminalFixture {
	return inspectionFixtureWith(t, []string{protocol.CapabilityContainerInspect, protocol.CapabilityContainerConfigure})
}
func configurationPath(f terminalFixture) string {
	return strings.TrimSuffix(f.path(), "exec") + "configuration"
}
func beginConfiguration(f terminalFixture) <-chan *httptest.ResponseRecorder {
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() { ch <- tenantRequest(f.s, f.admin, "GET", configurationPath(f), "", false) }()
	return ch
}
func configurationGrant(t *testing.T, f terminalFixture) protocol.ConfigurationOpen {
	t.Helper()
	frame := readEnvelope(t, f.ctx, f.ag.conn)
	var req protocol.ConfigurationOpen
	if frame.Type != protocol.TypeConfigurationOpen || json.Unmarshal(frame.Payload, &req) != nil || req.ValidateWithin(time.Now(), protocol.ConfigurationLifetime, protocol.RuntimeDocker) != nil {
		t.Fatalf("invalid configuration request: %+v", frame)
	}
	return req
}
func configurationReply(req protocol.ConfigurationOpen, unsupported ...string) protocol.ConfigurationResult {
	if unsupported == nil {
		unsupported = []string{}
	}
	return protocol.ConfigurationResult{Request: req.Request, Status: "ok", Result: &protocol.ContainerConfiguration{
		Target: req.Target, ObservedAt: time.Now(), Name: "web", ImageID: req.Target.ImageID,
		Image:       protocol.ImagePull{Reference: "ghcr.io/acme/web:1", Digest: "sha256:" + strings.Repeat("c", 64), Tag: "ghcr.io/acme/web:1"},
		NetworkMode: "bridge", Restart: "no", Env: []protocol.EnvEntry{{Name: "API_KEY", Value: configurationSentinel}}, Unsupported: unsupported,
	}}
}

func TestConfigurationReadForOrganizationAdmin(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	f := configurationFixture(t)
	response := beginConfiguration(f)
	req := configurationGrant(t, f)
	if req.Target.ContainerID != terminalSpec.Container || req.Target.ImageID != terminalSpec.ImageID || req.Actor != "usr_execadmin" {
		t.Fatalf("grant not pinned to inventory: %+v", req)
	}
	// The agent refuses a grant longer than its lifetime; the server leaves clock-skew margin.
	if req.Expires.After(time.Now().Add(protocol.ConfigurationLifetime - time.Second)) {
		t.Fatalf("grant expires too late: %v", time.Until(req.Expires))
	}
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeConfigurationResult, configurationReply(req, "env_truncated"))
	w := <-response
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), configurationSentinel) {
		t.Fatalf("response: %d %q %s", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
	}
	var got protocol.ContainerConfiguration
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Target != req.Target || len(got.Env) != 1 || got.Env[0].Value != configurationSentinel {
		t.Fatalf("bad body: %s", w.Body.String())
	}
	rows, _, err := f.st.Audit().ListAuditRecords(f.ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if strings.Contains(r.Details, configurationSentinel) || strings.Contains(r.Resource, configurationSentinel) {
			t.Fatalf("audit row carries an env value: %+v", r)
		}
		if r.Action == "container.configuration.read" && r.Result == "success" && r.UserID == "usr_execadmin" {
			found = true
			if r.Resource != f.ag.id+"/"+terminalSpec.Container || r.Details != "image="+terminalSpec.ImageID+" unsupported=1" {
				t.Fatalf("success row: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("no container.configuration.read success row")
	}
	// SetOutput takes the logger's lock, so every earlier write has landed in logs.
	log.SetOutput(io.Discard)
	if strings.Contains(logs.String(), configurationSentinel) {
		t.Fatal("an env value reached the log")
	}
}

func TestConfigurationReadCompleteAndUntrusted(t *testing.T) {
	f := configurationFixture(t)
	response := beginConfiguration(f)
	req := configurationGrant(t, f)
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeConfigurationResult, configurationReply(req))
	if w := <-response; w.Code != 200 {
		t.Fatalf("complete read: %d %s", w.Code, w.Body.String())
	}
	// A result for another target is never published.
	response = beginConfiguration(f)
	req = configurationGrant(t, f)
	bad := configurationReply(req)
	bad.Result.Target.CreatedUnix++
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeConfigurationResult, bad)
	if w := <-response; w.Code != 502 || strings.Contains(w.Body.String(), configurationSentinel) {
		t.Fatalf("untrusted result: %d %s", w.Code, w.Body.String())
	}
}

func TestConfigurationReadDeniedBelowOrganizationAdmin(t *testing.T) {
	f := configurationFixture(t)
	roles := []store.TenantRole{store.RoleEnvironmentAdmin, store.RoleOperator, store.RoleDeveloper, store.RoleReadOnly}
	for _, role := range roles {
		if err := f.st.Tenancy().SetMembership(f.ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_execadmin", Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if w := tenantRequest(f.s, f.admin, "GET", configurationPath(f), "", false); w.Code != 403 {
			t.Fatalf("%s: %d %s", role, w.Code, w.Body.String())
		}
	}
	rows, _, _ := f.st.Audit().ListAuditRecords(f.ctx, 0, 100)
	denied := 0
	for _, r := range rows {
		if r.UserID == "usr_execadmin" && r.Action == "container.configure" && r.Result == "denied" {
			denied++
		}
	}
	if denied != len(roles) {
		t.Fatalf("%d denied audit rows, want %d", denied, len(roles))
	}
	// No grant reached the agent.
	ctx, cancel := context.WithTimeout(f.ctx, 200*time.Millisecond)
	defer cancel()
	if _, _, err := f.ag.conn.Read(ctx); err == nil {
		t.Fatal("a denied read sent a frame to the agent")
	}
}

func TestConfigurationReadRefusesServiceToken(t *testing.T) {
	f := configurationFixture(t)
	token, _ := serviceAccess(t, f)
	if w := bearer(f.s, "GET", configurationPath(f), token); w.Code != 403 {
		t.Fatalf("service token: %d %s", w.Code, w.Body.String())
	}
	rows, _, _ := f.st.Audit().ListAuditRecords(f.ctx, 0, 100)
	denied := false
	for _, r := range rows {
		denied = denied || (strings.HasPrefix(r.UserID, "service:") && r.Action == "container.configure" && r.Result == "denied")
	}
	if !denied {
		t.Fatal("the service refusal left no denied audit row")
	}
}

func TestConfigurationReadRefusesManagedContainer(t *testing.T) {
	h := newPlanHost(t, append(inspecting, protocol.CapabilityContainerConfigure), "web")
	w := tenantRequest(h.s, h.admin, "GET", "/api/organizations/a/endpoints/"+h.ag.id+"/containers/"+h.targets[0].ContainerID+"/configuration", "", false)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "application_managed") {
		t.Fatalf("managed container: %d %s", w.Code, w.Body.String())
	}
}

func TestConfigurationReadNeedsCapabilityAndDocker(t *testing.T) {
	f := inspectionFixtureWith(t, []string{protocol.CapabilityContainerInspect})
	if w := tenantRequest(f.s, f.admin, "GET", configurationPath(f), "", false); w.Code != 501 {
		t.Fatalf("no capability: %d %s", w.Code, w.Body.String())
	}
	fleet := newRuntimeFleet(t)
	if err := fleet.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	w := tenantRequest(fleet.s, fleet.admin, "GET", "/api/organizations/a/endpoints/"+fleet.cluster.id+"/containers/"+terminalSpec.Container+"/configuration", "", false)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
		t.Fatalf("kubernetes endpoint: %d %s", w.Code, w.Body.String())
	}
}

func TestConfigurationReadRateLimit(t *testing.T) {
	f := inspectionFixtureWith(t, []string{protocol.CapabilityContainerInspect})
	for i := 1; i <= 13; i++ {
		w := tenantRequest(f.s, f.admin, "GET", configurationPath(f), "", false)
		if want := map[bool]int{true: 501, false: 429}[i <= 12]; w.Code != want {
			t.Fatalf("read %d: %d, want %d", i, w.Code, want)
		}
	}
}
