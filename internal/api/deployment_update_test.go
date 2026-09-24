package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// A plan with update pins the registry digest, shares the server-wide registry cap with update
// checks, outlives WriteTimeout, and applies only to an agent that advertises deployment.pull.
// The stored credential reaches the agent's frame and no HTTP body.
func TestPlanUpdateThroughTheRegistry(t *testing.T) {
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
	admin := loginAs(t, s, st, "updater", "user")
	viewer := loginAs(t, s, st, "viewer", "user")
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_updater", Role: store.RoleOrganizationAdmin, Status: "active"}))
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}))
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	const secret = "plan-update-registry-canary"
	send := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := tenantRequest(s, admin, method, path, body, true)
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("%s %s leaked the credential: %s", method, path, w.Body.String())
		}
		return w
	}
	request := func(method, path, body string, status int) string {
		t.Helper()
		w := send(method, path, body)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), status)
		}
		return w.Body.String()
	}
	code := func(body, want string) {
		t.Helper()
		var e struct{ Code string }
		if json.Unmarshal([]byte(body), &e) != nil || e.Code != want {
			t.Fatalf("want code %s: %s", want, body)
		}
	}
	request("PUT", "/api/organizations/a/registries", `{"host":"ghcr.io","name":"GitHub","username":"bot","credential":"`+secret+`","allow_private":false}`, 200)

	ag := enrollAgent(t, s, st, admin, "host-update")
	request("POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, 204)
	containerID, imageID := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	local, remote := "sha256:"+strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64)
	created, gen := time.Now().UTC().Truncate(time.Second).Add(-time.Hour), uint64(time.Now().Unix())
	online := func(capabilities ...string) *agentSocket {
		t.Helper()
		waitFor(t, func() bool { return !s.Connected(ag.id) })
		sock, reason := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
		if sock == nil {
			t.Fatalf("connect refused: %s", reason)
		}
		writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities})
		gen++
		writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{
			Generation: gen, Engine: protocol.Engine{Version: "1"},
			Containers: []protocol.Container{{ID: containerID, Name: "shop-web", ImageID: imageID, State: "running", ComposeProject: "shop", CreatedAt: created, Mounts: []protocol.Mount{}, Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}}},
			Images:     []protocol.Image{{ID: imageID, Tags: []string{"ghcr.io/org/web:1.2"}, Digests: []string{"ghcr.io/org/web@" + local}}},
			Networks:   []protocol.Network{}, Volumes: []protocol.Volume{},
		})
		writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
		if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeHeartbeat {
			t.Fatalf("expected a heartbeat, got %s", f.Type)
		}
		return sock
	}
	sock := online(protocol.CapabilityDeploymentApply)

	base := "/api/organizations/a/environments/env-a/applications"
	importBody, _ := json.Marshal(map[string]string{"name": "shop", "compose": "services: {web: {image: ghcr.io/org/web:1.2}}"})
	var app store.Application
	must(json.Unmarshal([]byte(request("POST", base, string(importBody), 201)), &app))
	adoption := base + "/" + app.ID + "/adoption"
	var preview store.AdoptionPreview
	must(json.Unmarshal([]byte(request("GET", adoption+"?endpoint="+ag.id+"&project=shop", "", 200)), &preview))
	adoptionBody, _ := json.Marshal(store.AdoptionRequest{EndpointID: ag.id, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	var instance store.ApplicationInstance
	must(json.Unmarshal([]byte(request("POST", adoption, string(adoptionBody), 201)), &instance))
	mapping := base + "/" + app.ID + "/mapping"
	var mapped store.ApplicationMapping
	must(json.Unmarshal([]byte(request("GET", mapping, "", 200)), &mapped))
	mappingBody, _ := json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: map[string]string{"web": containerID}})
	request("PUT", mapping, string(mappingBody), 204)
	deployments := base + "/" + app.ID + "/deployments"
	check := base + "/" + app.ID + "/updates/check"
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: instance.ID, MappingVersion: mapped.Version + 1, Revision: 1, Confirm: "shop", Update: []string{"web"}})
	plainBody, _ := json.Marshal(store.PlanRequest{InstanceID: instance.ID, MappingVersion: mapped.Version + 1, Revision: 1, Confirm: "shop"})

	// A registry refusal is a named blocker, not a plan.
	api.SetDigestResolverForTest(s, &fakeDigests{err: registry.ErrUnauthorized})
	body := request("POST", deployments, string(planBody), 409)
	var refused struct {
		Code     string
		Blockers []string
	}
	must(json.Unmarshal([]byte(body), &refused))
	if refused.Code != "preflight_blocked" || !slices.Contains(refused.Blockers, "registry_unauthorized") {
		t.Fatalf("unauthorized registry: %s", body)
	}

	fake := &fakeDigests{digest: remote}
	api.SetDigestResolverForTest(s, fake)
	plan := func() store.Deployment {
		t.Helper()
		var d store.Deployment
		must(json.Unmarshal([]byte(request("POST", deployments, string(planBody), 201)), &d))
		return d
	}
	planned := plan()
	if ps := planned.Plan.Services; len(ps) != 1 || ps[0].PullDigest != remote || ps[0].PullReference != "ghcr.io/org/web@"+remote {
		t.Fatalf("plan: %+v", planned.Plan)
	}
	fake.mu.Lock()
	passed := len(fake.secrets) == 1 && fake.secrets[0] == secret
	fake.mu.Unlock()
	if !passed {
		t.Fatal("the stored credential did not reach the resolver")
	}

	// Organization b: an adopted, mapped application whose registry answers at once.
	must(ts.CreateOrganization(ctx, &store.Organization{ID: "b", Name: "B"}))
	must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-b", OrganizationID: "b", Name: "Prod"}))
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "usr_updater", Role: store.RoleOrganizationAdmin, Status: "active"}))
	request("PUT", "/api/organizations/b/registries", `{"host":"ghcr.io","name":"GitHub","username":"bot","credential":"`+secret+`","allow_private":false}`, 200)
	accessB := store.TenantAccess{ActorID: "usr_updater", OrganizationID: "b", EnvironmentID: "env-b"}
	tok, err := ts.CreateEnrollmentToken(ctx, accessB, "docker", "")
	must(err)
	pubB, privB, _ := ed25519.GenerateKey(rand.Reader)
	epB, err := ts.Enroll(ctx, store.EnrollmentRequest{Token: tok.Secret, PublicKey: pubB, Proof: ed25519.Sign(privB, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "host-b"})
	must(err)
	must(ts.ApproveEndpoint(ctx, accessB, epB.ID, epB.Fingerprint))
	containerB, imageB := strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	snapshotB, _ := json.Marshal(protocol.Snapshot{
		Engine:     protocol.Engine{Version: "1"},
		Images:     []protocol.Image{{ID: imageB, Tags: []string{"ghcr.io/orgb/web:1"}, Digests: []string{"ghcr.io/orgb/web@" + local}}},
		Containers: []protocol.Container{{ID: containerB, Name: "shop-web", ImageID: imageB, ComposeProject: "shop", CreatedAt: created, Mounts: []protocol.Mount{}}},
	})
	_, err = ts.AcceptInventory(ctx, epB.ID, uint64(time.Now().Unix()), time.Now(), snapshotB)
	must(err)
	baseB := "/api/organizations/b/environments/env-b/applications"
	importB, _ := json.Marshal(map[string]string{"name": "shop", "compose": "services: {web: {image: ghcr.io/orgb/web:1}}"})
	var appB store.Application
	must(json.Unmarshal([]byte(request("POST", baseB, string(importB), 201)), &appB))
	must(json.Unmarshal([]byte(request("GET", baseB+"/"+appB.ID+"/adoption?endpoint="+epB.ID+"&project=shop", "", 200)), &preview))
	adoptB, _ := json.Marshal(store.AdoptionRequest{EndpointID: epB.ID, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	var instanceB store.ApplicationInstance
	must(json.Unmarshal([]byte(request("POST", baseB+"/"+appB.ID+"/adoption", string(adoptB), 201)), &instanceB))
	var mappedB store.ApplicationMapping
	must(json.Unmarshal([]byte(request("GET", baseB+"/"+appB.ID+"/mapping", "", 200)), &mappedB))
	mapB, _ := json.Marshal(store.MappingRequest{InstanceID: instanceB.ID, Version: mappedB.Version, Digest: mappedB.Preview.Digest, Confirm: "shop", Bindings: map[string]string{"web": containerB}})
	request("PUT", baseB+"/"+appB.ID+"/mapping", string(mapB), 204)

	// Organization a's quota is two registry calls in flight, a check and a plan; a third is 429
	// and a plan with no update is not counted. Organization b still gets through.
	gated := &fakeDigests{digest: remote, gate: make(chan struct{}), entered: make(chan struct{}, 2)}
	api.SetDigestResolverForTest(s, routeDigests{"orgb/web": fake, "": gated})
	results := make(chan *httptest.ResponseRecorder, 2)
	var release sync.Once
	open := func() { release.Do(func() { close(gated.gate) }) }
	t.Cleanup(open)
	// The goroutines only request; every check runs here, where t.Fatal is allowed.
	go func() { results <- tenantRequest(s, admin, "POST", check, "", true) }()
	<-gated.entered
	go func() { results <- tenantRequest(s, admin, "POST", deployments, string(planBody), true) }()
	<-gated.entered
	w := send("POST", deployments, string(planBody))
	if w.Code != 429 || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("over the organization's quota: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	code(w.Body.String(), "too_many_checks")
	var checkedB store.UpdateCheck
	must(json.Unmarshal([]byte(request("POST", baseB+"/"+appB.ID+"/updates/check", "", 200)), &checkedB))
	if len(checkedB.Services) != 1 || checkedB.Services[0].RemoteDigest != remote {
		t.Fatalf("organization b's check: %+v", checkedB)
	}
	// Authorization answers before the cap: a member who may not deploy is 403, audited, not 429.
	w = tenantRequest(s, viewer, "POST", deployments, string(planBody), true)
	if w.Code != 403 {
		t.Fatalf("read-only update plan with the cap full: %d %s", w.Code, w.Body.String())
	}
	code(w.Body.String(), "tenant_access_denied")
	request("POST", deployments, string(plainBody), 201)
	open()
	for range 2 {
		w := <-results
		if (w.Code != 200 && w.Code != 201) || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("a request under the cap: %d %s", w.Code, w.Body.String())
		}
	}
	records, err := ts.ReadAudit(ctx, store.TenantAccess{ActorID: "usr_updater", OrganizationID: "a"}, 0, 200)
	must(err)
	denied := 0
	for _, rec := range records {
		if rec.UserID == "usr_viewer" && rec.Action == "application.deploy" && rec.Result == "denied" && rec.Resource == app.ID+"/updates" {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("read-only denial audit rows: %d", denied)
	}
	api.SetDigestResolverForTest(s, fake)
	plan() // every slot came back

	// A plan outliving the listener's WriteTimeout still delivers its answer.
	slow := &fakeDigests{digest: remote, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	api.SetDigestResolverForTest(s, slow)
	live := httptest.NewUnstartedServer(s)
	live.Config.WriteTimeout = 300 * time.Millisecond
	live.Start()
	t.Cleanup(live.Close)
	go func() {
		<-slow.entered
		time.Sleep(3 * live.Config.WriteTimeout)
		close(slow.gate)
	}()
	req, _ := http.NewRequest("POST", live.URL+deployments, strings.NewReader(string(planBody)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "token"})
	req.Header.Set(auth.HeaderCSRF, "token")
	resp, err := live.Client().Do(req)
	must(err)
	slowBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	must(err)
	if resp.StatusCode != 201 || !strings.Contains(string(slowBody), `"pull_digest":"`+remote+`"`) {
		t.Fatalf("slow plan: %d %s", resp.StatusCode, slowBody)
	}
	api.SetDigestResolverForTest(s, fake)

	// An agent without deployment.pull is never sent a pull.
	planned = plan()
	apply := deployments + "/" + planned.ID + "/apply"
	if body := request("POST", apply, `{"confirm":"shop"}`, 501); !strings.Contains(body, "Upgrade the host agent to enable deployments that pull images") {
		t.Fatalf("pull without the capability: %s", body)
	}
	var d store.Deployment
	must(json.Unmarshal([]byte(request("GET", deployments+"/"+planned.ID, "", 200)), &d))
	if d.State != "planned" {
		t.Fatalf("a refused apply moved the row: %+v", d)
	}
	// A plan with no pull still applies to that agent.
	var plain store.Deployment
	must(json.Unmarshal([]byte(request("POST", deployments, string(plainBody), 201)), &plain))
	request("POST", deployments+"/"+plain.ID+"/apply", `{"confirm":"shop"}`, 202)
	if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeDeploymentApply {
		t.Fatalf("expected the plain apply frame, got %s", f.Type)
	}
	sock.conn.CloseNow()
	waitFor(t, func() bool {
		var d store.Deployment
		return json.Unmarshal([]byte(request("GET", deployments+"/"+plain.ID, "", 200)), &d) == nil && d.State == protocol.OutcomeUnknown
	})

	sock = online(protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull)
	defer sock.conn.CloseNow()
	planned = plan()
	request("POST", deployments+"/"+planned.ID+"/apply", `{"confirm":"shop"}`, 202)
	frame := readEnvelope(t, ctx, sock.conn)
	var sent protocol.DeploymentRequest
	must(json.Unmarshal(frame.Payload, &sent))
	if frame.Type != protocol.TypeDeploymentApply || len(sent.Services) != 1 || sent.Services[0].Pull == nil || sent.Services[0].Pull.Digest != remote || sent.Registries["ghcr.io"].Secret != secret {
		t.Fatalf("the pull frame: %s", frame.Type)
	}
}

// routeDigests sends a Head to the resolver named by its repository, "" for any other.
type routeDigests map[string]store.DigestResolver

func (r routeDigests) Head(ctx context.Context, ref registry.Reference, cred *registry.Credential, allowPrivate bool) (string, error) {
	if next, ok := r[ref.Repository]; ok {
		return next.Head(ctx, ref, cred, allowPrivate)
	}
	return r[""].Head(ctx, ref, cred, allowPrivate)
}
