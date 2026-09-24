package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// planHost is an application adopted and mapped on an approved endpoint whose inventory the
// store accepted directly: one container per service, every one on the same local image.
type planHost struct {
	s           *api.Server
	st          store.Store
	admin       *http.Cookie
	ag          enrolledAgent
	deployments string
	planBody    string
	targets     []protocol.InspectionTarget // per service, in the order given
}

func newPlanHost(t *testing.T, capabilities []string, services ...string) planHost {
	t.Helper()
	s, st, _ := setupTestServer(t)
	ctx := context.Background()
	ts := st.Tenancy()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}))
	must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}))
	h := planHost{s: s, st: st, admin: loginAs(t, s, st, "planner", "user")}
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_planner", Role: store.RoleOrganizationAdmin, Status: "active"}))
	h.ag = enrollAgent(t, s, st, h.admin, "plan-host")
	h.do(t, "POST", "/api/organizations/a/endpoints/"+h.ag.id+"/approve", `{"fingerprint":"`+h.ag.fp+`"}`, 204)
	must(ts.SetEndpointCapabilities(ctx, h.ag.id, capabilities))
	image, created := "sha256:"+strings.Repeat("b", 64), time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	snapshot := protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Images: []protocol.Image{{ID: image, Tags: []string{"nginx:1"}}}}
	compose, bindings := []string{}, map[string]string{}
	for i, name := range services {
		id := strings.Repeat(strconv.Itoa(i+1), 64)
		snapshot.Containers = append(snapshot.Containers, protocol.Container{ID: id, Name: "shop-" + name, ImageID: image, ComposeProject: "shop", CreatedAt: created, Mounts: []protocol.Mount{}})
		compose = append(compose, name+": {image: nginx:1}")
		bindings[name] = id
		h.targets = append(h.targets, protocol.InspectionTarget{ContainerID: id, ImageID: image, CreatedUnix: created.Unix()})
	}
	raw, _ := json.Marshal(snapshot)
	_, err := ts.AcceptInventory(ctx, h.ag.id, uint64(time.Now().Unix()), time.Now(), raw)
	must(err)
	base := "/api/organizations/a/environments/env-a/applications"
	importBody, _ := json.Marshal(map[string]string{"name": "shop", "compose": "services: {" + strings.Join(compose, ", ") + "}"})
	var app store.Application
	must(json.Unmarshal([]byte(h.do(t, "POST", base, string(importBody), 201)), &app))
	adoption := base + "/" + app.ID + "/adoption"
	var preview store.AdoptionPreview
	must(json.Unmarshal([]byte(h.do(t, "GET", adoption+"?endpoint="+h.ag.id+"&project=shop", "", 200)), &preview))
	adoptionBody, _ := json.Marshal(store.AdoptionRequest{EndpointID: h.ag.id, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	var instance store.ApplicationInstance
	must(json.Unmarshal([]byte(h.do(t, "POST", adoption, string(adoptionBody), 201)), &instance))
	mapping := base + "/" + app.ID + "/mapping"
	var mapped store.ApplicationMapping
	must(json.Unmarshal([]byte(h.do(t, "GET", mapping, "", 200)), &mapped))
	mappingBody, _ := json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: bindings})
	h.do(t, "PUT", mapping, string(mappingBody), 204)
	h.deployments = base + "/" + app.ID + "/deployments"
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: instance.ID, MappingVersion: mapped.Version + 1, Revision: 1, Confirm: "shop"})
	h.planBody = string(planBody)
	return h
}

func (h planHost) do(t *testing.T, method, path, body string, status int) string {
	t.Helper()
	w := tenantRequest(h.s, h.admin, method, path, body, true)
	if w.Code != status {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// preflightTargets are the mapped containers in plan order, as the preflight lists them.
func (h planHost) preflightTargets(t *testing.T) []protocol.InspectionTarget {
	t.Helper()
	var pre store.DeploymentPreflight
	if err := json.Unmarshal([]byte(h.do(t, "GET", strings.TrimSuffix(h.deployments, "deployments")+"preflight", "", 200)), &pre); err != nil {
		t.Fatal(err)
	}
	out := []protocol.InspectionTarget{}
	for _, svc := range pre.Services {
		out = append(out, *svc.InspectionTarget)
	}
	return out
}

// verifiedObservation is what an agent reports for a container a recreate fully expresses.
func verifiedObservation(target protocol.InspectionTarget) protocol.ContainerInspection {
	return protocol.ContainerInspection{Target: target, ObservedAt: time.Now().UTC(), State: "running", RestartPolicy: "no", NetworkMode: "bridge", ImagePlatform: protocol.ImagePlatform{OS: "linux", Architecture: "amd64"}, Ports: []protocol.Port{}, Unsupported: []string{}, ConfigurationVerified: true}
}

// verifiedInspector answers every plan-time inspection with a verified observation.
func verifiedInspector(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
	return verifiedObservation(target), nil
}

var inspecting = []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict}

// The plan inspects each mapped service's container one at a time, in plan order, all under
// the plan's budget.
func TestPlanInspectsEachMappedServiceInTurn(t *testing.T) {
	h := newPlanHost(t, inspecting, "web", "db")
	var mu sync.Mutex
	var seen []protocol.InspectionTarget
	inFlight, most := 0, 0
	start := time.Now()
	api.SetPlanInspectorForTest(h.s, func(ctx context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
		mu.Lock()
		inFlight++
		most = max(most, inFlight)
		seen = append(seen, target)
		mu.Unlock()
		defer func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()
		if deadline, ok := ctx.Deadline(); !ok || deadline.After(start.Add(api.PlanInspectionBudgetForTest+time.Second)) {
			t.Errorf("inspection deadline %v (set %v) outlives the plan budget", deadline, ok)
		}
		time.Sleep(20 * time.Millisecond)
		return verifiedObservation(target), nil
	})
	want := h.preflightTargets(t)
	h.do(t, "POST", h.deployments, h.planBody, 201)
	mu.Lock()
	defer mu.Unlock()
	if most != 1 || !slices.Equal(seen, want) {
		t.Fatalf("at most %d at once, inspected %v, want %v", most, seen, want)
	}
}

// A plan's inspections share one inspection attempt, however many services it maps.
func TestPlanCountsOneInspectionAttempt(t *testing.T) {
	h := newPlanHost(t, inspecting, "web", "db")
	var mu sync.Mutex
	calls := 0
	api.SetPlanInspectorForTest(h.s, func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return verifiedObservation(target), nil
	})
	for range 29 {
		api.AllowAttemptForTest(h.s, "inspection:usr_planner", 30, time.Minute)
	}
	h.do(t, "POST", h.deployments, h.planBody, 201)
	mu.Lock()
	first := calls
	mu.Unlock()
	if first != 2 {
		t.Fatalf("the 30th attempt inspected %d services", first)
	}
	if b := h.refused(t); !slices.Equal(b.Blockers, []string{"inspection_unavailable"}) || len(b.Services) != 2 {
		t.Fatalf("over the inspection limit: %+v", b)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != first {
		t.Fatal("a plan over the inspection limit inspected")
	}
}

// Without container.inspect nothing is inspected, no attempt is spent, and the store names why.
func TestPlanSkipsInspectionWithoutTheCapability(t *testing.T) {
	h := newPlanHost(t, []string{protocol.CapabilityDeploymentApply}, "web")
	api.SetPlanInspectorForTest(h.s, func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
		t.Error("inspected without container.inspect")
		return verifiedObservation(target), nil
	})
	body := h.do(t, "POST", h.deployments, h.planBody, 409)
	if !strings.Contains(body, "agent_inspect_unsupported") || strings.Contains(body, "inspection_unavailable") {
		t.Fatalf("blockers: %s", body)
	}
	if slices.Contains(api.AttemptKeysForTest(h.s), "inspection:usr_planner") {
		t.Fatal("an inspection attempt was spent")
	}
}

// Inspections are the server's to gather: a client cannot hand one in (Review Focus 1).
func TestPlanRefusesClientSuppliedInspections(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	forged := `,"inspections":{"` + h.targets[0].ContainerID + `":{"configuration_verified":true,"unsupported":[]}}}`
	h.do(t, "POST", h.deployments, strings.TrimSuffix(h.planBody, "}")+forged, 400)
}

// Over a real agent socket the plan sends one grant per mapped container, expiring within the
// plan budget, and plans on the agent's answer.
func TestPlanInspectsOverTheAgentSocket(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	httpSrv := httptest.NewServer(h.s)
	defer httpSrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sock, reason := connect(t, ctx, httpSrv.URL, h.ag, h.ag.priv, protocol.Version)
	if sock == nil {
		t.Fatalf("connect refused: %s", reason)
	}
	defer sock.conn.CloseNow()
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: inspecting})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeHeartbeat {
		t.Fatalf("expected a heartbeat, got %s", f.Type)
	}
	start := time.Now()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- tenantRequest(h.s, h.admin, "POST", h.deployments, h.planBody, true) }()
	frame := readEnvelope(t, ctx, sock.conn)
	var grant protocol.InspectionOpen
	if frame.Type != protocol.TypeInspectionOpen || json.Unmarshal(frame.Payload, &grant) != nil || grant.Validate(time.Now()) != nil {
		t.Fatalf("expected an inspection grant, got %s", frame.Type)
	}
	if grant.Target != h.targets[0] || grant.Actor != "usr_planner" || grant.Expires.After(start.Add(api.PlanInspectionBudgetForTest+time.Second)) {
		t.Fatalf("grant: %+v", grant)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInspectionResult, protocol.InspectionResult{Request: grant.Request, Status: "ok", Result: ptr(verifiedObservation(grant.Target))})
	if w := <-response; w.Code != 201 {
		t.Fatalf("plan: %d %s", w.Code, w.Body.String())
	}
}

func ptr[T any](v T) *T { return &v }

// An update plan inspects before it takes a registry slot, so slow agents cannot hold the
// organization's registry slots for the inspection budget.
func TestPlanUpdateInspectsBeforeTakingARegistrySlot(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	inspected := false
	api.SetPlanInspectorForTest(h.s, func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
		inspected = true
		if held := api.RegistrySlotsHeldForTest(h.s); held != 0 {
			t.Errorf("%d registry slots held while inspecting", held)
		}
		return verifiedObservation(target), nil
	})
	var plan store.PlanRequest
	if err := json.Unmarshal([]byte(h.planBody), &plan); err != nil {
		t.Fatal(err)
	}
	plan.Update = []string{"web"}
	body, _ := json.Marshal(plan)
	tenantRequest(h.s, h.admin, "POST", h.deployments, string(body), true) // the registry outcome is not this test's
	if !inspected {
		t.Fatal("the update plan did not inspect")
	}
}

// Over the real inspection path, each grant waits for its answer before the next is sent, an
// answered grant is never cancelled, and one left unanswered is cancelled when the plan's
// budget runs out, after which the plan returns without asking about the rest.
func TestPlanInspectionBudgetCancelsTheUnansweredGrant(t *testing.T) {
	h := newPlanHost(t, inspecting, "web", "db", "cache")
	api.SetPlanInspectorForTest(h.s, nil)
	httpSrv := httptest.NewServer(h.s)
	defer httpSrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sock, reason := connect(t, ctx, httpSrv.URL, h.ag, h.ag.priv, protocol.Version)
	if sock == nil {
		t.Fatalf("connect refused: %s", reason)
	}
	defer sock.conn.CloseNow()
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: inspecting})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeHeartbeat {
		t.Fatalf("expected a heartbeat, got %s", f.Type)
	}
	want := h.preflightTargets(t)
	start := time.Now()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- tenantRequest(h.s, h.admin, "POST", h.deployments, h.planBody, true) }()
	grantOf := func() protocol.InspectionOpen {
		t.Helper()
		frame := readEnvelope(t, ctx, sock.conn)
		var grant protocol.InspectionOpen
		if frame.Type != protocol.TypeInspectionOpen || json.Unmarshal(frame.Payload, &grant) != nil {
			t.Fatalf("expected an inspection grant, got %s", frame.Type)
		}
		return grant
	}
	first := grantOf()
	if first.Target != want[0] {
		t.Fatalf("first grant targets %+v, want %+v", first.Target, want[0])
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInspectionResult, protocol.InspectionResult{Request: first.Request, Status: "ok", Result: ptr(verifiedObservation(first.Target))})
	second := grantOf() // an inspection.cancel here would mean the answered grant was cancelled
	if second.Target != want[1] || second.Expires.After(start.Add(api.PlanInspectionBudgetForTest+time.Second)) {
		t.Fatalf("second grant: %+v", second)
	}
	frame := readEnvelope(t, ctx, sock.conn)
	var stopped protocol.InspectionCancel
	if frame.Type != protocol.TypeInspectionCancel || json.Unmarshal(frame.Payload, &stopped) != nil || stopped.Request != second.Request {
		t.Fatalf("expected the unanswered grant's cancel, got %s", frame.Type)
	}
	select {
	case w := <-response:
		var b planBlocked
		if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &b) != nil || b.Code != "preflight_blocked" || !slices.Equal(b.Blockers, []string{"inspection_unavailable"}) || len(b.Services) != 2 {
			t.Fatalf("plan: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(start.Add(api.PlanInspectionBudgetForTest + 2*time.Second).Sub(time.Now())):
		t.Fatal("the plan outlived its inspection budget")
	}
	// Frames leave in order, so a grant for the third service would precede this heartbeat.
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeHeartbeat {
		t.Fatalf("a spent budget still sent %s", f.Type)
	}
}

// planBlocked is a 409 plan refusal as the API writes it.
type planBlocked struct {
	Code     string
	Blockers []string
	Services []struct {
		Name        string
		Blockers    []string
		Unsupported []string
	}
}

// refused posts the plan, expects 409 and checks the body names no container.
func (h planHost) refused(t *testing.T) planBlocked {
	t.Helper()
	body := h.do(t, "POST", h.deployments, h.planBody, 409)
	var b planBlocked
	if err := json.Unmarshal([]byte(body), &b); err != nil || b.Code != "preflight_blocked" {
		t.Fatalf("refusal: %s", body)
	}
	for _, target := range h.targets {
		if strings.Contains(body, target.ContainerID) || strings.Contains(body, target.ImageID) {
			t.Fatalf("the refusal named a container: %s", body)
		}
	}
	return b
}

// online connects the host's agent over a real socket advertising capabilities.
func (h planHost) online(t *testing.T, capabilities []string) (*agentSocket, context.Context) {
	t.Helper()
	srv := httptest.NewServer(h.s)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	sock, reason := connect(t, ctx, srv.URL, h.ag, h.ag.priv, protocol.Version)
	if sock == nil {
		t.Fatalf("connect refused: %s", reason)
	}
	t.Cleanup(func() { sock.conn.CloseNow() })
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if f := readEnvelope(t, ctx, sock.conn); f.Type != protocol.TypeHeartbeat {
		t.Fatalf("expected a heartbeat, got %s", f.Type)
	}
	return sock, ctx
}

// grant reads the next frame as an inspection grant.
func grant(t *testing.T, ctx context.Context, sock *agentSocket) protocol.InspectionOpen {
	t.Helper()
	frame := readEnvelope(t, ctx, sock.conn)
	var g protocol.InspectionOpen
	if frame.Type != protocol.TypeInspectionOpen || json.Unmarshal(frame.Payload, &g) != nil {
		t.Fatalf("expected an inspection grant, got %s", frame.Type)
	}
	return g
}

// What the live inspection reports refuses the plan, and the refusal names the service.
func TestPlanRefusesOnTheLiveInspection(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	for name, tc := range map[string]struct {
		inspect     func(context.Context, protocol.InspectionTarget) (protocol.ContainerInspection, error)
		want        string
		unsupported []string
	}{
		"no answer": {func(context.Context, protocol.InspectionTarget) (protocol.ContainerInspection, error) {
			return protocol.ContainerInspection{}, errors.New("unavailable")
		}, "inspection_unavailable", nil},
		"unsupported": {func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
			in := verifiedObservation(target)
			in.ConfigurationVerified, in.Unsupported = false, []string{"privileged", "devices"}
			return in, nil
		}, "configuration_unsupported", []string{"privileged", "devices"}},
		"another container": {func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
			target.CreatedUnix++
			return verifiedObservation(target), nil
		}, "replacement_identity_changed", nil},
	} {
		api.SetPlanInspectorForTest(h.s, tc.inspect)
		b := h.refused(t)
		if !slices.Equal(b.Blockers, []string{tc.want}) || len(b.Services) != 1 || b.Services[0].Name != "web" || !slices.Equal(b.Services[0].Unsupported, tc.unsupported) {
			t.Fatalf("%s: %+v", name, b)
		}
	}
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.do(t, "POST", h.deployments, h.planBody, 201)
}

// With no agent connected the plan is refused as uninspected, never planned blind.
func TestPlanRefusesWhenTheAgentIsOffline(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	if b := h.refused(t); !slices.Equal(b.Blockers, []string{"inspection_unavailable"}) || len(b.Services) != 1 || b.Services[0].Name != "web" {
		t.Fatalf("offline: %+v", b)
	}
}

// An agent that never answers holds the plan no longer than the budget: the plan is refused and
// the abandoned grant is cancelled on the agent (Review Focus 2).
func TestPlanRefusesWhenTheAgentNeverAnswers(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	sock, ctx := h.online(t, inspecting)
	start := time.Now()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- tenantRequest(h.s, h.admin, "POST", h.deployments, h.planBody, true) }()
	g := grant(t, ctx, sock)
	w := <-response
	if elapsed := time.Since(start); w.Code != 409 || !strings.Contains(w.Body.String(), "inspection_unavailable") || elapsed > api.PlanInspectionBudgetForTest+3*time.Second {
		t.Fatalf("after %v: %d %s", elapsed, w.Code, w.Body.String())
	}
	frame := readEnvelope(t, ctx, sock.conn)
	var stopped protocol.InspectionCancel
	if frame.Type != protocol.TypeInspectionCancel || json.Unmarshal(frame.Payload, &stopped) != nil || stopped.Request != g.Request {
		t.Fatalf("expected the grant's cancel, got %s", frame.Type)
	}
}

// An agent built before verdicts advertises container.inspect without
// container.inspect.verdict and answers with configuration_verified false and no unsupported
// list. The plan asks it nothing and names the upgrade; an agent advertising the verdict but
// answering the old way is refused as unavailable, never verified (Review Focus 5).
func TestPlanRefusesAnOlderAgentsInspection(t *testing.T) {
	t.Run("no verdict capability", func(t *testing.T) {
		h := newPlanHost(t, []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect}, "web")
		api.SetPlanInspectorForTest(h.s, func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
			t.Error("inspected an agent without container.inspect.verdict")
			return verifiedObservation(target), nil
		})
		body := h.do(t, "POST", h.deployments, h.planBody, 409)
		if !strings.Contains(body, "agent_inspect_unsupported") || strings.Contains(body, "inspection_unavailable") {
			t.Fatalf("blockers: %s", body)
		}
	})
	t.Run("verdict advertised, old answer", func(t *testing.T) {
		h := newPlanHost(t, inspecting, "web")
		sock, ctx := h.online(t, inspecting)
		response := make(chan *httptest.ResponseRecorder, 1)
		go func() { response <- tenantRequest(h.s, h.admin, "POST", h.deployments, h.planBody, true) }()
		g := grant(t, ctx, sock)
		raw, _ := json.Marshal(verifiedObservation(g.Target))
		var older map[string]any
		if err := json.Unmarshal(raw, &older); err != nil {
			t.Fatal(err)
		}
		delete(older, "unsupported")
		older["configuration_verified"] = false
		writeEnvelope(t, ctx, sock.conn, protocol.TypeInspectionResult, map[string]any{"request": g.Request, "status": "ok", "result": older})
		if w := <-response; w.Code != 409 || !strings.Contains(w.Body.String(), "inspection_unavailable") {
			t.Fatalf("an older agent's answer: %d %s", w.Code, w.Body.String())
		}
	})
}
