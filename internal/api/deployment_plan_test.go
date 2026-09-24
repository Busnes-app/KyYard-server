package api_test

import (
	"context"
	"encoding/json"
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

var inspecting = []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect}

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
	h.do(t, "POST", h.deployments, h.planBody, 201)
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
