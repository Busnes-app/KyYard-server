package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

const (
	workloadSentinel = "workload-secret-canary"
	podUID           = "11111111-2222-4333-8444-555555555555"
)

var workloadCaps = []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesWorkloads, protocol.CapabilityPodExec}

// workloadFleet is a cluster granted namespace shop and a Docker host, with a member per role.
type workloadFleet struct {
	s                                  *api.Server
	st                                 store.Store
	ctx                                context.Context
	url                                string
	cluster, host                      enrolledAgent
	org, envAdmin, operator, viewer    *http.Cookie
	conn                               *websocket.Conn
	clusterPath, hostPath, webWorkload string
}

func newWorkloadFleet(t *testing.T, caps ...string) *workloadFleet {
	t.Helper()
	s, st, _ := setupTestServerWith(t, func(cfg *config.Config) { cfg.Server.AppURL = terminalOrigin })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}); err != nil {
		t.Fatal(err)
	}
	f := &workloadFleet{s: s, st: st, ctx: ctx}
	for _, m := range []struct {
		cookie **http.Cookie
		name   string
		role   store.TenantRole
	}{{&f.org, "orgadmin", store.RoleOrganizationAdmin}, {&f.envAdmin, "envadmin", store.RoleEnvironmentAdmin}, {&f.operator, "operator", store.RoleOperator}, {&f.viewer, "viewer", store.RoleReadOnly}} {
		*m.cookie = loginAs(t, s, st, m.name, "user")
		if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_" + m.name, Role: m.role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	httpSrv := httptest.NewServer(s)
	t.Cleanup(func() { s.BeginShutdown(); httpSrv.Close(); s.WaitDetached() })
	f.url = httpSrv.URL
	f.cluster = enrollClusterAgent(t, s, st, "usr_orgadmin", "cluster-1")
	f.host = enrollAgent(t, s, st, f.org, "host-1")
	for _, ag := range []enrolledAgent{f.cluster, f.host} {
		if w := tenantRequest(s, f.org, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
			t.Fatalf("approve: %d %s", w.Code, w.Body.String())
		}
	}
	if _, err := ts.SetEndpointDeployNamespaces(ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: "a"}, f.cluster.id, []string{"shop"}); err != nil {
		t.Fatal(err)
	}
	f.clusterPath = "/api/organizations/a/endpoints/" + f.cluster.id
	f.hostPath = "/api/organizations/a/endpoints/" + f.host.id
	f.webWorkload = f.clusterPath + "/workloads/shop/deployment/web"
	f.connect(t, caps...)
	return f
}

// connect (re)connects the cluster agent with caps and a known inventory.
func (f *workloadFleet) connect(t *testing.T, caps ...string) {
	t.Helper()
	if f.conn != nil {
		f.conn.Close(websocket.StatusNormalClosure, "reconnect")
		waitFor(t, func() bool { return !f.s.Connected(f.cluster.id) })
	}
	sock, reason := connect(t, f.ctx, f.url, f.cluster, f.cluster.priv, protocol.Version)
	if reason != "" {
		t.Fatalf("connect: %s", reason)
	}
	t.Cleanup(func() { sock.conn.CloseNow() })
	f.conn = sock.conn
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeHello, protocol.Hello{Capabilities: caps})
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()) - 1000 + reports.Add(1), Kubernetes: &protocol.KubernetesInventory{
		Namespaces: []string{"shop", "other"},
		Workloads: []protocol.Workload{
			{Kind: "Deployment", Namespace: "shop", Name: "web"},
			{Kind: "Deployment", Namespace: "shop", Name: "api", Application: "shop", Instance: "shop"},
			{Kind: "Deployment", Namespace: "other", Name: "web"},
		},
		Pods: []protocol.Pod{{Namespace: "shop", Name: "web-7c9", UID: podUID, Phase: "Running", OwnerKind: "Deployment", OwnerName: "web", Containers: []protocol.PodContainer{{Name: "web"}}}},
	}})
	f.sync(t)
	waitFor(t, func() bool {
		e, _ := f.st.Tenancy().ReadEndpointRaw(f.ctx, f.cluster.id)
		return e != nil && e.State == "active"
	})
}

// sync proves every frame written before it has been handled, and that the server sent the
// agent nothing in between.
func (f *workloadFleet) sync(t *testing.T) {
	t.Helper()
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeHeartbeat, nil)
	if e := readEnvelope(t, f.ctx, f.conn); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("expected a heartbeat, got %s %s", e.Type, e.Payload)
	}
}

func (f *workloadFleet) command(t *testing.T) protocol.Command {
	t.Helper()
	e := readEnvelope(t, f.ctx, f.conn)
	var cmd protocol.Command
	if e.Type != protocol.TypeCommand || json.Unmarshal(e.Payload, &cmd) != nil {
		t.Fatalf("not a command: %s %s", e.Type, e.Payload)
	}
	return cmd
}

func workloadConfiguration(ref protocol.WorkloadRef) *protocol.WorkloadConfiguration {
	replicas := int32(2)
	return &protocol.WorkloadConfiguration{Target: ref, ObservedAt: time.Now(), ResourceVersion: "42", Replicas: &replicas, Strategy: "RollingUpdate",
		Containers:     []protocol.WorkloadContainer{{Name: "web", Image: "ghcr.io/acme/web:1", Command: []string{}, Args: []string{}, Env: []protocol.WorkloadEnv{{Name: "TOKEN", Value: workloadSentinel}, {Name: "DB", SecretRef: "db/password"}}}},
		InitContainers: []protocol.WorkloadContainer{}, EnvFrom: []string{}, Unsupported: []string{}}
}

// answerConfiguration reads the next configuration grant and answers it with cfg.
func (f *workloadFleet) answerConfiguration(t *testing.T, cfg func(protocol.WorkloadRef) *protocol.WorkloadConfiguration) {
	t.Helper()
	e := readEnvelope(t, f.ctx, f.conn)
	var open protocol.InspectionOpen
	if e.Type != protocol.TypeConfigurationOpen || json.Unmarshal(e.Payload, &open) != nil || open.Target.Workload.Kind == "" {
		t.Errorf("not a workload configuration grant: %s %s", e.Type, e.Payload)
		return
	}
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeConfigurationResult, protocol.ConfigurationResult{Request: open.Request, Status: "ok", Workload: cfg(open.Target.Workload)})
}

func applyBody(t *testing.T, cfg *protocol.WorkloadConfiguration, confirm string) string {
	t.Helper()
	spec := *cfg
	spec.ObservedAt = time.Time{}
	raw, err := json.Marshal(map[string]any{"resource_version": spec.ResourceVersion, "spec": spec, "confirm": confirm})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func dispatch(action, reference, confirm, expects string) string {
	body := `{"action":"` + action + `","reference":"` + reference + `","confirm":"` + confirm + `"`
	if expects != "" {
		body += `,"expects":` + expects
	}
	return body + "}"
}

// Each route asks its own permission: restart and scale container.operate, the deletes
// container.destroy, configuration read and apply container.configure (never a service token),
// and the pod terminal container.exec.
func TestWorkloadRoutesPermissionMatrix(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	commands := f.clusterPath + "/commands"
	restart := dispatch(protocol.ActionWorkloadRestart, "shop/deployment/web", "", "")
	scale := dispatch(protocol.ActionWorkloadScale, "shop/deployment/web", "", `{"replicas":3}`)
	deleteWeb := dispatch(protocol.ActionWorkloadDelete, "shop/deployment/web", "web", "")
	deletePod := dispatch(protocol.ActionPodDelete, "shop/pod/web-7c9", "web-7c9", "")
	for _, c := range []struct {
		who     *http.Cookie
		name    string
		body    string
		allowed bool
	}{
		{f.viewer, "viewer restart", restart, false},
		{f.operator, "operator restart", restart, true},
		{f.operator, "operator scale", scale, true},
		{f.operator, "operator delete", deleteWeb, false},
		{f.operator, "operator pod delete", deletePod, false},
		{f.envAdmin, "environment admin pod delete", deletePod, true},
		{f.envAdmin, "environment admin delete", deleteWeb, true},
	} {
		w := tenantRequest(f.s, c.who, "POST", commands, c.body, true)
		if c.allowed != (w.Code == 202) || !c.allowed && w.Code != 403 {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
		if c.allowed {
			var body map[string]any
			_ = json.Unmarshal([]byte(c.body), &body)
			if cmd := f.command(t); cmd.Action != body["action"] || cmd.Reference != body["reference"] || cmd.Container != "" {
				t.Fatalf("%s sent %+v", c.name, cmd)
			}
		}
	}
	for _, who := range []*http.Cookie{f.envAdmin, f.operator, f.viewer} {
		if w := tenantRequest(f.s, who, "GET", f.webWorkload+"/configuration", "", false); w.Code != 403 {
			t.Errorf("configuration read: %d %s", w.Code, w.Body.String())
		}
		if w := tenantRequest(f.s, who, "POST", f.webWorkload+"/apply", applyBody(t, workloadConfiguration(protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "web"}), "web"), true); w.Code != 403 {
			t.Errorf("apply: %d %s", w.Code, w.Body.String())
		}
		if res := f.dialPod(t, who); res == nil || res.StatusCode != 403 {
			t.Errorf("pod exec: %+v", res)
		}
	}
	token, _ := f.service(t)
	for _, method := range []string{"GET", "POST"} {
		path := f.webWorkload + "/configuration"
		if method == "POST" {
			path = f.webWorkload + "/apply"
		}
		if w := bearer(f.s, method, path, token); w.Code != 403 {
			t.Errorf("service token %s: %d %s", method, w.Code, w.Body.String())
		}
	}
	f.sync(t)
}

func (f *workloadFleet) service(t *testing.T) (string, store.TenantAccess) {
	t.Helper()
	p, err := f.st.Tenancy().CreateServicePairing(f.ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	issue, err := f.st.Tenancy().ClaimServiceToken(f.ctx, p.Code, "kypulse", "10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := f.st.Tenancy().LookupServiceToken(f.ctx, issue.Token)
	if err != nil {
		t.Fatal(err)
	}
	return issue.Token, store.TenantAccess{ServiceTokenID: tok.ID, OrganizationID: "a"}
}

// dialPod upgrades the pod terminal route as who and returns the refusal, or nil once upgraded.
func (f *workloadFleet) dialPod(t *testing.T, who *http.Cookie) *http.Response {
	t.Helper()
	c, res := f.dialPodPath(t, who, "/pods/shop/web-7c9/exec")
	if c != nil {
		c.CloseNow()
		return nil
	}
	return res
}

func (f *workloadFleet) dialPodPath(t *testing.T, who *http.Cookie, suffix string) (*websocket.Conn, *http.Response) {
	t.Helper()
	headers := http.Header{"Origin": {terminalOrigin}, "Cookie": {who.String() + "; ky_csrf=terminal-test-csrf"}}
	c, res, _ := websocket.Dial(f.ctx, "ws"+strings.TrimPrefix(f.url, "http")+f.clusterPath+suffix, &websocket.DialOptions{HTTPHeader: headers})
	return c, res
}

// The workload routes refuse a Docker endpoint with 409 runtime_unsupported, after the action's
// own permission; TestDockerRoutesRefuseAKubernetesEndpoint is the other direction.
func TestKubernetesRoutesRefuseADockerEndpoint(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	for _, route := range []struct{ method, path, body string }{
		{"POST", f.hostPath + "/commands", dispatch(protocol.ActionWorkloadRestart, "shop/deployment/web", "", "")},
		{"POST", f.hostPath + "/commands", dispatch(protocol.ActionPodDelete, "shop/pod/web-7c9", "web-7c9", "")},
		{"GET", f.hostPath + "/workloads/shop/deployment/web/configuration", ""},
		{"POST", f.hostPath + "/workloads/shop/deployment/web/apply", "{}"},
	} {
		w := tenantRequest(f.s, f.org, route.method, route.path, route.body, true)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
			t.Errorf("%s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	headers := http.Header{"Origin": {terminalOrigin}, "Cookie": {f.org.String() + "; ky_csrf=terminal-test-csrf"}}
	c, res, _ := websocket.Dial(f.ctx, "ws"+strings.TrimPrefix(f.url, "http")+f.hostPath+"/pods/shop/web-7c9/exec", &websocket.DialOptions{HTTPHeader: headers})
	if c != nil {
		c.CloseNow()
		t.Fatal("a pod terminal upgraded on a Docker endpoint")
	}
	if res == nil || res.StatusCode != 409 {
		t.Fatalf("pod exec on Docker: %+v", res)
	}
	// A Docker action on the cluster is still the Docker routes' refusal.
	if w := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/commands", `{"action":"container.restart","container":"web"}`, true); w.Code != 409 || !strings.Contains(w.Body.String(), "runtime_unsupported") {
		t.Fatalf("container action on the cluster: %d %s", w.Code, w.Body.String())
	}
}

// Every cluster write and the configuration read stop at the granted namespace (422), a
// capability the agent lacks (501), a managed workload for read, apply and delete (409, while
// restart and scale go through), and a malformed command (400); none of them sends a frame.
func TestWorkloadRefusals(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	commands := f.clusterPath + "/commands"
	code := func(w *httptest.ResponseRecorder, status int, want string) {
		t.Helper()
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != status || want != "" && body.Code != want {
			t.Errorf("want %d %s, got %d %s", status, want, w.Code, w.Body.String())
		}
	}
	other := f.clusterPath + "/workloads/other/deployment/web"
	code(tenantRequest(f.s, f.org, "POST", commands, dispatch(protocol.ActionWorkloadRestart, "other/deployment/web", "", ""), true), 422, "namespace_not_granted")
	code(tenantRequest(f.s, f.org, "GET", other+"/configuration", "", false), 422, "namespace_not_granted")
	code(tenantRequest(f.s, f.org, "POST", other+"/apply", applyBody(t, workloadConfiguration(protocol.WorkloadRef{Namespace: "other", Kind: "deployment", Name: "web"}), "web"), true), 422, "namespace_not_granted")
	c, res := f.dialPodPath(t, f.org, "/pods/other/web-7c9/exec")
	if c != nil || res == nil || res.StatusCode != 422 {
		t.Errorf("pod exec in another namespace: %+v", res)
	}

	managedPath := f.clusterPath + "/workloads/shop/deployment/api"
	code(tenantRequest(f.s, f.org, "GET", managedPath+"/configuration", "", false), 409, "application_managed")
	code(tenantRequest(f.s, f.org, "POST", managedPath+"/apply", applyBody(t, workloadConfiguration(protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "api"}), "api"), true), 409, "application_managed")
	code(tenantRequest(f.s, f.org, "POST", commands, dispatch(protocol.ActionWorkloadDelete, "shop/deployment/api", "api", ""), true), 409, "application_managed")
	f.sync(t)
	code(tenantRequest(f.s, f.org, "POST", commands, dispatch(protocol.ActionWorkloadRestart, "shop/deployment/api", "", ""), true), 202, "")
	f.command(t)
	code(tenantRequest(f.s, f.org, "POST", commands, dispatch(protocol.ActionWorkloadScale, "shop/deployment/api", "", `{"replicas":0}`), true), 202, "")
	if cmd := f.command(t); cmd.Expects.Replicas == nil || *cmd.Expects.Replicas != 0 {
		t.Fatalf("scale to zero sent %+v", cmd.Expects)
	}

	for name, body := range map[string]string{
		"scale without replicas": dispatch(protocol.ActionWorkloadScale, "shop/deployment/web", "", ""),
		"scale past the bound":   dispatch(protocol.ActionWorkloadScale, "shop/deployment/web", "", `{"replicas":1001}`),
		"restart with replicas":  dispatch(protocol.ActionWorkloadRestart, "shop/deployment/web", "", `{"replicas":1}`),
		"delete without a name":  dispatch(protocol.ActionWorkloadDelete, "shop/deployment/web", "", ""),
		"delete another name":    dispatch(protocol.ActionWorkloadDelete, "shop/deployment/web", "api", ""),
		"bad reference":          dispatch(protocol.ActionWorkloadRestart, "shop/web", "", ""),
	} {
		if w := tenantRequest(f.s, f.org, "POST", commands, body, true); w.Code != 400 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	code(tenantRequest(f.s, f.org, "GET", commands+"?reference=shop/web", "", false), 400, "invalid_reference")
	code(tenantRequest(f.s, f.org, "GET", f.clusterPath+"/workloads/shop/pod/web-7c9/configuration", "", false), 400, "")
	code(tenantRequest(f.s, f.org, "POST", f.webWorkload+"/apply", applyBody(t, workloadConfiguration(protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "web"}), "api"), true), 400, "")
	f.sync(t)

	// An agent without kubernetes.workloads or pod.exec is asked to upgrade.
	f.connect(t, protocol.CapabilityKubernetesInventory)
	code(tenantRequest(f.s, f.org, "POST", commands, dispatch(protocol.ActionWorkloadRestart, "shop/deployment/web", "", ""), true), 501, "")
	code(tenantRequest(f.s, f.org, "GET", f.webWorkload+"/configuration", "", false), 501, "")
	code(tenantRequest(f.s, f.org, "POST", f.webWorkload+"/apply", applyBody(t, workloadConfiguration(protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "web"}), "web"), true), 501, "")
	if res := f.dialPod(t, f.org); res == nil || res.StatusCode != 501 {
		t.Errorf("pod exec without pod.exec: %+v", res)
	}
	f.sync(t)
}

// The configuration read returns the agent's answer to an organization administrator and audits
// only the count of unsupported settings; the apply sends a workload.apply frame, settles from
// the agent's deployment.result, and ?reference= lists it. No value reaches audit or the log.
func TestWorkloadConfigurationAndApplySettle(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	f := newWorkloadFleet(t, workloadCaps...)
	ref := protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "web"}
	done := make(chan struct{})
	go func() { defer close(done); f.answerConfiguration(t, workloadConfiguration) }()
	w := tenantRequest(f.s, f.org, "GET", f.webWorkload+"/configuration", "", false)
	<-done
	var read protocol.WorkloadConfiguration
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &read) != nil || read.Target != ref || read.ResourceVersion != "42" || read.Containers[0].Env[0].Value != workloadSentinel {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
	// A read the agent reports as managed is refused like the inventory's.
	done = make(chan struct{})
	go func() {
		defer close(done)
		f.answerConfiguration(t, func(r protocol.WorkloadRef) *protocol.WorkloadConfiguration {
			c := workloadConfiguration(r)
			c.Managed = true
			return c
		})
	}()
	w = tenantRequest(f.s, f.org, "GET", f.webWorkload+"/configuration", "", false)
	<-done
	if w.Code != 409 || !strings.Contains(w.Body.String(), "application_managed") || strings.Contains(w.Body.String(), workloadSentinel) {
		t.Fatalf("managed read: %d %s", w.Code, w.Body.String())
	}

	edited := read
	edited.Containers[0].Image = "ghcr.io/acme/web:2"
	w = tenantRequest(f.s, f.org, "POST", f.webWorkload+"/apply", applyBody(t, &edited, "web"), true)
	var cmd store.Command
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &cmd) != nil || cmd.Action != store.ActionWorkloadApply || cmd.Reference != "shop/deployment/web" || cmd.Outcome != "" {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
	e := readEnvelope(t, f.ctx, f.conn)
	var frame protocol.WorkloadApply
	if e.Type != protocol.TypeWorkloadApply || json.Unmarshal(e.Payload, &frame) != nil || frame.Validate(time.Now()) != nil || frame.Request != cmd.ID || frame.Target != ref || frame.ResourceVersion != "42" || frame.Spec.Containers[0].Image != "ghcr.io/acme/web:2" {
		t.Fatalf("frame %s %s", e.Type, e.Payload)
	}
	// A second apply waits for the first.
	if w := tenantRequest(f.s, f.org, "POST", f.webWorkload+"/apply", applyBody(t, &edited, "web"), true); w.Code != 409 || !strings.Contains(w.Body.String(), "command_in_progress") {
		t.Fatalf("second apply: %d %s", w.Code, w.Body.String())
	}
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: frame.Request, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: protocol.WorkloadApplyService, Step: protocol.StepApply, Outcome: protocol.OutcomeSucceeded}, {Service: protocol.WorkloadApplyService, Step: protocol.StepRollout, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{}})
	f.sync(t)
	w = tenantRequest(f.s, f.org, "GET", f.clusterPath+"/commands?reference=shop/deployment/web", "", false)
	var listed []store.Command
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed) != 1 || listed[0].ID != cmd.ID || listed[0].Outcome != protocol.OutcomeSucceeded || listed[0].Result == nil {
		t.Fatalf("listed: %d %s", w.Code, w.Body.String())
	}
	if w := tenantRequest(f.s, f.org, "GET", f.clusterPath+"/commands?reference=shop/deployment/api", "", false); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("other reference: %d %s", w.Code, w.Body.String())
	}

	rows, _, err := f.st.Audit().ListAuditRecords(f.ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if strings.Contains(row.Details, workloadSentinel) || strings.Contains(row.Resource, workloadSentinel) {
			t.Fatalf("a value reached the audit: %+v", row)
		}
		if row.Resource == f.cluster.id+"/shop/deployment/web" && row.Result == "success" {
			seen[row.Action+" "+row.Details] = true
		}
	}
	for _, want := range []string{"workload.configuration.read unsupported=0", "workload.apply resource_version=42 containers=1", "workload.apply code=- new=-"} {
		if !seen[want] {
			t.Errorf("no audit %q in %v", want, seen)
		}
	}
	if strings.Contains(logs.String(), workloadSentinel) {
		t.Fatal("a value reached the log")
	}
}

// The pod terminal opens only for the inventory's pod: the start message names its container
// and UID, and a UID the inventory does not know is refused before any frame. Open and close
// are audited as pod.exec, without argv.
func TestPodExecOpenCloseAndUIDMismatch(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	start := func(c *websocket.Conn, uid string) {
		spec := protocol.ExecSpec{Pod: &protocol.PodTarget{Namespace: "shop", Name: "web-7c9", Container: "web", UID: uid}, Argv: []string{"/bin/sh", "secret-argv-must-not-be-audited"}}
		raw, _ := json.Marshal(map[string]any{"csrf": "terminal-test-csrf", "confirm": "web-7c9", "spec": spec, "size": protocol.TerminalSize{Rows: 24, Columns: 80}})
		if err := c.Write(f.ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
	}
	c, res := f.dialPodPath(t, f.org, "/pods/shop/web-7c9/exec")
	if c == nil {
		t.Fatalf("dial: %+v", res)
	}
	defer c.CloseNow()
	start(c, "99999999-2222-4333-8444-555555555555")
	if e := readEnvelope(t, f.ctx, c); e.Type != protocol.TypeExecClose || !strings.Contains(string(e.Payload), "refused") {
		t.Fatalf("mismatched UID: %s %s", e.Type, e.Payload)
	}
	f.sync(t) // nothing was sent to the agent

	c, res = f.dialPodPath(t, f.org, "/pods/shop/web-7c9/exec")
	if c == nil {
		t.Fatalf("dial: %+v", res)
	}
	defer c.CloseNow()
	start(c, podUID)
	e := readEnvelope(t, f.ctx, f.conn)
	var grant protocol.ExecOpen
	if e.Type != protocol.TypeExecOpen || json.Unmarshal(e.Payload, &grant) != nil || grant.Validate(time.Now()) != nil || grant.Spec.ValidateFor(protocol.RuntimeKubernetes) != nil || grant.Spec.Pod.UID != podUID {
		t.Fatalf("grant %s %s", e.Type, e.Payload)
	}
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeExecReady, protocol.ExecStream{Stream: grant.Stream})
	if e := readEnvelope(t, f.ctx, c); e.Type != protocol.TypeExecReady {
		t.Fatalf("not ready: %s", e.Type)
	}
	exit := 0
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeExecClose, protocol.ExecClose{Stream: grant.Stream, Reason: "process exited", ExitCode: &exit})
	if e := readEnvelope(t, f.ctx, c); e.Type != protocol.TypeExecClose {
		t.Fatalf("not closed: %s", e.Type)
	}
	resource := f.cluster.id + "/pods/shop/web-7c9/web"
	waitFor(t, func() bool {
		rows, _, err := f.st.Audit().ListAuditRecords(f.ctx, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		opened, closed := false, false
		for _, row := range rows {
			if strings.Contains(row.Details, "secret-argv") {
				t.Fatalf("argv audited: %+v", row)
			}
			if row.CorrelationID == grant.Stream && row.Resource == resource {
				opened = opened || row.Action == "pod.exec.open"
				closed = closed || (row.Action == "pod.exec.close" && row.Result == "success" && strings.Contains(row.Details, "exit_code=0"))
			}
		}
		return opened && closed
	})
}

// The agent's close reason reaches the browser only when it is a fixed exec refusal; any other
// text is replaced by the fixed notice.
func TestPodExecCloseReasonPassesOnlyFixedRefusals(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	for reason, want := range map[string]string{"forbidden": "forbidden", "pod_security": "pod_security", "token=secret": "terminal attachment ended; process state may be unknown"} {
		c, res := f.dialPodPath(t, f.org, "/pods/shop/web-7c9/exec")
		if c == nil {
			t.Fatalf("dial: %+v", res)
		}
		spec := protocol.ExecSpec{Pod: &protocol.PodTarget{Namespace: "shop", Name: "web-7c9", Container: "web", UID: podUID}, Argv: []string{"/bin/sh"}}
		raw, _ := json.Marshal(map[string]any{"csrf": "terminal-test-csrf", "confirm": "web-7c9", "spec": spec, "size": protocol.TerminalSize{Rows: 24, Columns: 80}})
		if err := c.Write(f.ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		var grant protocol.ExecOpen
		if e := readEnvelope(t, f.ctx, f.conn); e.Type != protocol.TypeExecOpen || json.Unmarshal(e.Payload, &grant) != nil {
			t.Fatalf("grant %s %s", e.Type, e.Payload)
		}
		writeEnvelope(t, f.ctx, f.conn, protocol.TypeExecClose, protocol.ExecClose{Stream: grant.Stream, Reason: reason})
		e := readEnvelope(t, f.ctx, c)
		var closed protocol.ExecClose
		if e.Type != protocol.TypeExecClose || json.Unmarshal(e.Payload, &closed) != nil || closed.Reason != want {
			t.Fatalf("%s: %s %s", reason, e.Type, e.Payload)
		}
		c.CloseNow()
	}
}
