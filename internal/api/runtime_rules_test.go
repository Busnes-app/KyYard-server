package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

// runtimeFleet is organization a with one approved Kubernetes endpoint and one approved Docker
// endpoint, served over a real socket.
type runtimeFleet struct {
	s       *api.Server
	st      store.Store
	admin   *http.Cookie
	url     string
	cluster enrolledAgent
	host    enrolledAgent
}

func newRuntimeFleet(t *testing.T) runtimeFleet {
	t.Helper()
	s, st, _ := setupTestServer(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}); err != nil {
		t.Fatal(err)
	}
	admin := loginAs(t, s, st, "envadmin", "user")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(s)
	t.Cleanup(httpSrv.Close)
	f := runtimeFleet{s: s, st: st, admin: admin, url: httpSrv.URL}
	f.cluster = enrollClusterAgent(t, s, st, "usr_envadmin", "cluster-1")
	f.host = enrollAgent(t, s, st, admin, "host-1")
	for _, ag := range []enrolledAgent{f.cluster, f.host} {
		if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
			t.Fatalf("approve: %d %s", w.Code, w.Body.String())
		}
	}
	return f
}

func (f runtimeFleet) endpoint(t *testing.T, id string) map[string]any {
	t.Helper()
	w := tenantRequest(f.s, f.admin, "GET", "/api/organizations/a/endpoints/"+id, "", false)
	var out map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("endpoint: %d %s", w.Code, w.Body.String())
	}
	return out
}

// expectClose reads until the server closes the socket and returns the close reason.
func expectClose(t *testing.T, ctx context.Context, c *websocket.Conn) string {
	t.Helper()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if errorsAs(err, &ce) {
				return ce.Reason
			}
			t.Fatalf("read: %v", err)
		}
	}
}

// A hello naming a capability outside the endpoint's runtime is answered capability_mismatch
// and closed; a hello that fits is stored.
func TestHelloCapabilitiesMustFitTheRuntime(t *testing.T) {
	f := newRuntimeFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name  string
		agent enrolledAgent
		caps  []string
		fits  bool
	}{
		{"docker capability from a cluster", f.cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityContainerInspect}, false},
		{"cluster capability from a host", f.host, []string{protocol.CapabilityDeploymentApply, protocol.CapabilityPodLogs}, false},
		{"cluster capabilities from a cluster", f.cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs}, true},
		{"docker capabilities from a host", f.host, []string{protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock, reason := connect(t, ctx, f.url, tc.agent, tc.agent.priv, protocol.Version)
			if reason != "" {
				t.Fatalf("connect: %s", reason)
			}
			defer sock.conn.CloseNow()
			writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: tc.caps})
			if !tc.fits {
				if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeError || !strings.Contains(string(e.Payload), "capability_mismatch") {
					t.Fatalf("answer: %+v", e)
				}
				if reason := expectClose(t, ctx, sock.conn); reason != protocol.CloseProtocol {
					t.Fatalf("closed with %q", reason)
				}
			} else {
				writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
				if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
					t.Fatalf("session did not continue: %+v", e)
				}
				var stored []string
				for _, c := range f.endpoint(t, tc.agent.id)["capabilities"].([]any) {
					stored = append(stored, c.(string))
				}
				slices.Sort(tc.caps)
				if !slices.Equal(stored, tc.caps) {
					t.Fatalf("stored %v", stored)
				}
				sock.conn.Close(websocket.StatusNormalClosure, "done")
			}
			waitFor(t, func() bool { return !f.s.Connected(tc.agent.id) })
		})
	}
}

// A Kubernetes endpoint's snapshot must carry the cluster inventory and no Docker lists, a
// Docker endpoint's no cluster inventory; either violation is snapshot_rejected and the
// session goes on. The stored snapshot drives cluster_health, present only for Kubernetes.
func TestSnapshotShapeAndClusterHealth(t *testing.T) {
	f := newRuntimeFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cluster, _ := connect(t, ctx, f.url, f.cluster, f.cluster.priv, protocol.Version)
	defer cluster.conn.CloseNow()
	host, _ := connect(t, ctx, f.url, f.host, f.host.priv, protocol.Version)
	defer host.conn.CloseNow()
	gen := uint64(time.Now().Unix()) - 100
	send := func(sock *agentSocket, s protocol.Snapshot) protocol.Envelope {
		t.Helper()
		gen++
		s.Generation = gen
		writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, s)
		writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
		return readEnvelope(t, ctx, sock.conn)
	}
	rejected := func(e protocol.Envelope) bool {
		return e.Type == protocol.TypeError && strings.Contains(string(e.Payload), "snapshot_rejected")
	}
	nodes := func(ready ...bool) *protocol.KubernetesInventory {
		k := &protocol.KubernetesInventory{}
		for i, r := range ready {
			k.Nodes = append(k.Nodes, protocol.Node{Name: "node-" + string(rune('a'+i)), Ready: r})
		}
		return k
	}
	for name, s := range map[string]protocol.Snapshot{
		"docker lists from a cluster":   {Containers: []protocol.Container{{ID: "c1", Name: "web"}}},
		"both shapes from a cluster":    {Kubernetes: nodes(true), Containers: []protocol.Container{{ID: "c1", Name: "web"}}},
		"no inventory from a cluster":   {},
		"cluster inventory from a host": {Kubernetes: nodes(true)},
	} {
		sock := cluster
		if strings.HasSuffix(name, "host") {
			sock = host
		}
		if e := send(sock, s); !rejected(e) {
			t.Fatalf("%s: %+v", name, e)
		}
		// The session continues: the heartbeat sent after the snapshot is answered.
		if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
			t.Fatalf("%s: session did not continue: %+v", name, e)
		}
	}
	if _, ok := f.endpoint(t, f.cluster.id)["cluster_health"]; !ok {
		t.Fatal("an approved cluster without inventory has no cluster_health")
	}
	if got := f.endpoint(t, f.cluster.id)["cluster_health"]; got != protocol.HealthUnknown {
		t.Fatalf("no inventory yet: %v", got)
	}
	if e := send(cluster, protocol.Snapshot{Kubernetes: nodes(true, false)}); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("a valid cluster snapshot was refused: %+v", e)
	}
	waitFor(t, func() bool { return f.endpoint(t, f.cluster.id)["cluster_health"] == protocol.HealthDegraded })
	w := tenantRequest(f.s, f.admin, "GET", "/api/organizations/a/endpoints", "", false)
	var list []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	for _, e := range list {
		if _, ok := e["cluster_health"]; ok != (e["id"] == f.cluster.id) {
			t.Fatalf("list entry %v", e)
		}
	}
	if e := send(cluster, protocol.Snapshot{Kubernetes: nodes(true, true)}); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("refused: %+v", e)
	}
	waitFor(t, func() bool { return f.endpoint(t, f.cluster.id)["cluster_health"] == protocol.HealthHealthy })
	inv := tenantRequest(f.s, f.admin, "GET", "/api/organizations/a/endpoints/"+f.cluster.id+"/inventory", "", false)
	if inv.Code != 200 || !strings.Contains(inv.Body.String(), `"node-b"`) {
		t.Fatalf("inventory: %d %s", inv.Code, inv.Body.String())
	}
	if e := send(host, protocol.Snapshot{Containers: []protocol.Container{{ID: "c1", Name: "web"}}}); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("a valid docker snapshot was refused: %+v", e)
	}
	if _, ok := f.endpoint(t, f.host.id)["cluster_health"]; ok {
		t.Fatal("a Docker endpoint reports cluster_health")
	}
	cluster.conn.Close(websocket.StatusNormalClosure, "bye")
	waitFor(t, func() bool { return f.endpoint(t, f.cluster.id)["cluster_health"] == protocol.HealthUnknown })
}

// setEndpointRuntime rewrites an endpoint's runtime in storage. No route changes a runtime;
// this reaches the DB the same way expireDeployments does, to put a Docker-adopted
// application on a Kubernetes endpoint.
func setEndpointRuntime(t *testing.T, cfg *config.Config, id, runtime string) {
	t.Helper()
	driver, q := "pgx", "UPDATE endpoints SET runtime=$1 WHERE id=$2"
	if cfg.Database.Driver == "sqlite" {
		driver, q = "sqlite", "UPDATE endpoints SET runtime=? WHERE id=?"
	}
	db, err := sql.Open(driver, cfg.Database.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, runtime, id); err != nil {
		t.Fatal(err)
	}
}

// Every route that acts on containers, images, exec, inspection, deployments, adoption or the
// service mapping refuses a Kubernetes endpoint with runtime_unsupported, before any
// capability or state check.
func TestDockerRoutesRefuseAKubernetesEndpoint(t *testing.T) {
	h := newPlanHost(t, inspecting, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	var planned store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", h.deployments, h.planBody, 201)), &planned); err != nil {
		t.Fatal(err)
	}
	var plan store.PlanRequest
	_ = json.Unmarshal([]byte(h.planBody), &plan)
	app := strings.TrimSuffix(h.deployments, "/deployments")
	var mapped store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped); err != nil {
		t.Fatal(err)
	}
	mappingBody, _ := json.Marshal(store.MappingRequest{InstanceID: plan.InstanceID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: map[string]string{"web": h.targets[0].ContainerID}})
	setEndpointRuntime(t, h.cfg, h.ag.id, protocol.RuntimeKubernetes)

	ep := "/api/organizations/a/endpoints/" + h.ag.id
	for _, route := range []struct{ method, path, body string }{
		{"POST", ep + "/commands", `{"action":"container.restart","container":"shop-web"}`},
		{"GET", ep + "/containers/shop-web/removal", ""},
		{"GET", ep + "/containers/shop-web/inspection", ""},
		{"GET", ep + "/containers/shop-web/logs", ""},
		{"POST", h.deployments, h.planBody},
		{"POST", h.deployments + "/" + planned.ID + "/apply", `{"confirm":"shop"}`},
		{"POST", app + "/removal", `{"instance_id":"` + plan.InstanceID + `","confirm":"shop"}`},
		{"PUT", app + "/mapping", string(mappingBody)},
		{"GET", app + "/adoption?endpoint=" + h.ag.id + "&project=shop", ""},
		{"POST", app + "/adoption", `{"endpoint_id":"` + h.ag.id + `","project":"shop","digest":"x","confirm":"shop"}`},
	} {
		w := tenantRequest(h.s, h.admin, route.method, route.path, route.body, true)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
			t.Errorf("%s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	// The terminal is a WebSocket upgrade; the refusal comes before it.
	r := httptest.NewRequest("GET", ep+"/containers/shop-web/exec", nil)
	r.Header.Set("Origin", h.cfg.Server.AppURL)
	r.AddCookie(h.admin)
	w := httptest.NewRecorder()
	h.s.ServeHTTP(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
		t.Errorf("exec: %d %s", w.Code, w.Body.String())
	}
	// Reads stay open: the endpoint and its inventory are still visible.
	h.do(t, "GET", ep, "", 200)
	h.do(t, "GET", ep+"/inventory", "", 200)
}
