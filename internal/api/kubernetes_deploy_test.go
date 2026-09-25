package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// clusterHost is an approved Kubernetes endpoint granted namespace shop, connected through a
// fake cluster agent that advertises the deploy and remove capabilities, with anonymous pulls
// on and a registry that answers remote for every tag.
type clusterHost struct {
	s      *api.Server
	st     store.Store
	admin  *http.Cookie
	ag     enrolledAgent
	sock   *agentSocket
	ctx    context.Context
	base   string
	remote string
}

func newClusterHost(t *testing.T, capabilities ...string) clusterHost {
	t.Helper()
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	ts := st.Tenancy()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}))
	must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}))
	h := clusterHost{s: s, st: st, admin: loginAs(t, s, st, "deployer", "user"), ctx: ctx, base: "/api/organizations/a/environments/env-a/applications", remote: "sha256:" + strings.Repeat("d", 64)}
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_deployer", Role: store.RoleOrganizationAdmin, Status: "active"}))
	must(ts.SetAnonymousPull(ctx, store.TenantAccess{ActorID: "usr_deployer", OrganizationID: "a"}, true))
	api.SetDigestResolverForTest(s, &fakeDigests{digest: h.remote})
	httpSrv := httptest.NewServer(s)
	t.Cleanup(httpSrv.Close)
	h.ag = enrollClusterAgent(t, s, st, "usr_deployer", "cluster-1")
	h.do(t, "POST", "/api/organizations/a/endpoints/"+h.ag.id+"/approve", `{"fingerprint":"`+h.ag.fp+`"}`, 204)
	h.do(t, "POST", "/api/organizations/a/endpoints/"+h.ag.id+"/manifest", `{"namespaces":["shop"]}`, 200)
	sock, reason := connect(t, ctx, httpSrv.URL, h.ag, h.ag.priv, protocol.Version)
	if sock == nil {
		t.Fatalf("connect: %s", reason)
	}
	t.Cleanup(func() { sock.conn.CloseNow() })
	h.sock = sock
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()), Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.36.0"},
		Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "n1", Ready: true}}, Namespaces: []string{"shop"}, Workloads: []protocol.Workload{}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}}})
	h.sync(t)
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, h.ag.id); return e != nil && e.State == "active" })
	return h
}

func (h clusterHost) do(t *testing.T, method, path, body string, status int) string {
	t.Helper()
	w := tenantRequest(h.s, h.admin, method, path, body, true)
	if w.Code != status {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// sync proves every frame written before it has been handled.
func (h clusterHost) sync(t *testing.T) {
	t.Helper()
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeHeartbeat, nil)
	if f := readEnvelope(t, h.ctx, h.sock.conn); f.Type != protocol.TypeHeartbeat {
		t.Fatalf("expected a heartbeat, got %s", f.Type)
	}
}

// importApp saves compose as application name and returns its path.
func (h clusterHost) importApp(t *testing.T, name, compose string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "compose": compose})
	var app store.Application
	if err := json.Unmarshal([]byte(h.do(t, "POST", h.base, string(body), 201)), &app); err != nil {
		t.Fatal(err)
	}
	return h.base + "/" + app.ID
}

var clusterCapabilities = []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesRemove}

// The stateless definition maps to a namespace of the cluster, plans with every image pinned at
// the registry, applies through the cluster agent with its target and secret keys, settles with
// Deployment identities, is validated as unverifiable, and is removed by its instance label.
func TestKubernetesApplicationOverTheClusterAgent(t *testing.T) {
	h := newClusterHost(t, clusterCapabilities...)
	const canary = "cluster-secret-canary"
	app := h.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1.2, restart: always, ports: [{target: 80, published: 8080}], environment: {TOKEN: "+canary+"}}}")
	if body := h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"prod"}`, 400); !strings.Contains(body, "namespace_unknown") {
		t.Fatalf("unknown namespace: %s", body)
	}
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped); err != nil || mapped.Runtime != protocol.RuntimeKubernetes || mapped.Namespace != "shop" || mapped.Preview.Project != "shop" {
		t.Fatalf("mapping %+v %v", mapped, err)
	}
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop"})
	var planned store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/deployments", string(planBody), 201)), &planned); err != nil {
		t.Fatal(err)
	}
	ps := planned.Plan.Services[0]
	if planned.Plan.Namespace != "shop" || ps.Object == nil || ps.Object.Name != "shop-web" || ps.PullDigest != h.remote || ps.PullReference != "ghcr.io/org/web@"+h.remote {
		t.Fatalf("plan %+v", planned.Plan)
	}
	var applying store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/deployments/"+planned.ID+"/apply", `{"confirm":"shop"}`, 202)), &applying); err != nil || applying.State != "applying" {
		t.Fatalf("apply %+v %v", applying, err)
	}
	frame := readEnvelope(t, h.ctx, h.sock.conn)
	var sent protocol.DeploymentRequest
	if frame.Type != protocol.TypeDeploymentApply || json.Unmarshal(frame.Payload, &sent) != nil {
		t.Fatalf("frame %s", frame.Type)
	}
	svc := sent.Services[0]
	if sent.Kubernetes == nil || sent.Kubernetes.Namespace != "shop" || sent.Kubernetes.InstanceID != mapped.InstanceID || svc.Env["TOKEN"] != canary || !slices.Equal(svc.SecretKeys, []string{"TOKEN"}) || svc.Pull.Digest != h.remote || svc.ContainerName != "" || len(sent.Registries) != 0 {
		t.Fatalf("frame %+v", sent)
	}
	if err := sent.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: planned.ID, RequestID: planned.CorrelationID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded}, {Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}, {Service: "web", Step: protocol.StepStart, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{{Service: "web", Kind: protocol.KindDeployment, Namespace: "shop", Name: "shop-web", UID: "0f1e2d3c-4b5a-4968-8776-655443322110", Generation: 1, ImageDigest: h.remote}}})
	h.sync(t)
	var settled store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/deployments/"+planned.ID, "", 200)), &settled); err != nil || settled.State != protocol.OutcomeSucceeded || settled.Result.Services[0].Name != "shop-web" || strings.Contains(h.do(t, "GET", app+"/deployments/"+planned.ID, "", 200), canary) {
		t.Fatalf("settled %+v %v", settled, err)
	}
	// A cluster agent cannot report container health, so the apply is not validated.
	api.ValidationTickForTest(h.s, time.Now().Add(store.ValidationGrace+time.Minute))
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/deployments/"+planned.ID, "", 200)), &settled); err != nil || settled.Validation == nil || settled.Validation.Verdict != store.VerdictUnverifiable {
		t.Fatalf("validation %+v %v", settled.Validation, err)
	}

	var removing store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/removal", `{"instance_id":"`+mapped.InstanceID+`","confirm":"shop"}`, 202)), &removing); err != nil {
		t.Fatal(err)
	}
	frame = readEnvelope(t, h.ctx, h.sock.conn)
	var removal protocol.RemovalRequest
	if frame.Type != protocol.TypeDeploymentRemove || json.Unmarshal(frame.Payload, &removal) != nil || removal.Kubernetes == nil || !slices.Equal(removal.Services, []string{"web"}) || len(removal.Containers) != 0 {
		t.Fatalf("removal frame %s %+v", frame.Type, removal)
	}
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: removing.ID, RequestID: removing.CorrelationID, Outcome: protocol.OutcomeSucceeded, Services: []protocol.DeploymentIdentity{},
		Steps: []protocol.DeploymentStep{{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded}, {Service: "web", Step: protocol.StepRemove, Outcome: protocol.OutcomeSucceeded}}})
	h.sync(t)
	var instances []store.ApplicationInstance
	if err := json.Unmarshal([]byte(h.do(t, "GET", h.base+"/instances", "", 200)), &instances); err != nil || len(instances) != 0 {
		t.Fatalf("instances after removal %+v %v", instances, err)
	}
}

// A stateful definition stops at the plan, naming each service and why; nothing is sent.
func TestKubernetesPlanBlockersOverTheAPI(t *testing.T) {
	h := newClusterHost(t, clusterCapabilities...)
	app := h.importApp(t, "db", "services: {db: {image: ghcr.io/org/db:1, volumes: ['data:/var/lib/db']}, web: {image: ghcr.io/org/web:1, restart: on-failure}}\nvolumes: {data: {}}")
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	_ = json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped)
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "db"})
	var refused struct {
		Code     string
		Blockers []string
		Services []store.BlockedService
	}
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/deployments", string(planBody), 409)), &refused); err != nil {
		t.Fatal(err)
	}
	if refused.Code != "preflight_blocked" || !slices.Equal(refused.Blockers, []string{"kubernetes_unsupported"}) || len(refused.Services) != 2 || !slices.Equal(refused.Services[0].Unsupported, []string{"k8s_volume"}) || !slices.Equal(refused.Services[1].Unsupported, []string{"k8s_restart"}) {
		t.Fatalf("refused %+v", refused)
	}
}

// A cluster agent without kubernetes.deploy is asked to upgrade, not sent a frame.
func TestKubernetesApplyNeedsTheCapability(t *testing.T) {
	h := newClusterHost(t, protocol.CapabilityKubernetesInventory)
	app := h.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1}}")
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	_ = json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped)
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop"})
	if body := h.do(t, "POST", app+"/deployments", string(planBody), 409); !strings.Contains(body, "agent_deploy_unsupported") {
		t.Fatalf("plan: %s", body)
	}
	h.do(t, "POST", app+"/removal", `{"instance_id":"`+mapped.InstanceID+`","confirm":"shop"}`, 501)
}

// Docker routes refuse the cluster; application routes take it, so none of them answers
// runtime_unsupported for an application mapped to the cluster.
func TestRuntimeGateMatrix(t *testing.T) {
	h := newClusterHost(t, clusterCapabilities...)
	app := h.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1}}")
	ep := "/api/organizations/a/endpoints/" + h.ag.id
	for _, route := range []struct{ method, path, body string }{
		{"POST", ep + "/commands", `{"action":"container.restart","container":"shop-web"}`},
		{"GET", ep + "/containers/shop-web/removal", ""},
		{"GET", ep + "/containers/shop-web/inspection", ""},
		{"GET", ep + "/containers/shop-web/logs", ""},
		{"GET", app + "/adoption?endpoint=" + h.ag.id + "&project=shop", ""},
		{"POST", app + "/adoption", `{"endpoint_id":"` + h.ag.id + `","project":"shop","digest":"x","confirm":"shop"}`},
	} {
		w := tenantRequest(h.s, h.admin, route.method, route.path, route.body, true)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
			t.Errorf("docker route %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	_ = json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped)
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop"})
	var planned store.Deployment
	_ = json.Unmarshal([]byte(h.do(t, "POST", app+"/deployments", string(planBody), 201)), &planned)
	for _, route := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", app + "/mapping", "", 200},
		{"GET", app + "/preflight", "", 200},
		{"PUT", app + "/mapping", `{"endpoint_id":"` + h.ag.id + `","namespace":"shop"}`, 204},
		{"POST", app + "/deployments/" + planned.ID + "/apply", `{"confirm":"shop"}`, 409}, // the remap outdated the plan
	} {
		w := tenantRequest(h.s, h.admin, route.method, route.path, route.body, true)
		if w.Code != route.status || strings.Contains(w.Body.String(), "runtime_unsupported") {
			t.Errorf("application route %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
}
