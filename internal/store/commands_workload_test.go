package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const workloadSentinel = "workload-secret-canary"

// activeClusterWith enrolls, approves and gives a Kubernetes endpoint one inventory; its
// manifest grants namespaces.
func activeClusterWith(t *testing.T, ts TenancyStore, a TenantAccess, inv protocol.KubernetesInventory, namespaces ...string) string {
	t.Helper()
	ctx := context.Background()
	tok, err := ts.CreateEnrollmentToken(ctx, a, protocol.RuntimeKubernetes, "", namespaces...)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	enrolled, err := ts.Enroll(ctx, EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "cluster"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ApproveEndpoint(ctx, a, enrolled.ID, enrolled.Fingerprint); err != nil {
		t.Fatal(err)
	}
	snap := protocol.Snapshot{Generation: uint64(time.Now().Unix()), Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}, Kubernetes: &inv}
	raw, _ := json.Marshal(snap)
	if ok, err := ts.AcceptInventory(ctx, enrolled.ID, snap.Generation, time.Now().UTC(), raw); err != nil || !ok {
		t.Fatalf("inventory: %v %v", ok, err)
	}
	return enrolled.ID
}

func testCluster() protocol.KubernetesInventory {
	return protocol.KubernetesInventory{
		Namespaces: []string{"shop", "other"},
		Workloads: []protocol.Workload{
			{Kind: "Deployment", Namespace: "shop", Name: "web"},
			{Kind: "Deployment", Namespace: "shop", Name: "api", Application: "shop", Instance: "shop"},
			{Kind: "DaemonSet", Namespace: "shop", Name: "agent"},
			{Kind: "Deployment", Namespace: "other", Name: "web"},
		},
		Pods: []protocol.Pod{{Namespace: "shop", Name: "web-7c9", UID: "11111111-2222-4333-8444-555555555555", Phase: "Running", Containers: []protocol.PodContainer{{Name: "web"}}}},
	}
}

func replicas(n int32) *int32 { return &n }

// Workload commands name their target in the reference, check its shape before any row, and
// check the endpoint, the granted namespace and the inventory inside the transaction.
func TestWorkloadCommands(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeClusterWith(t, ts, a, testCluster(), "shop")
	host := activeEndpointWith(t, ts, a, nil, nil)
	for _, c := range []struct {
		name, endpoint, action, reference, confirm string
		expects                                    protocol.Expectation
		want                                       error
	}{
		{"short reference", cluster, protocol.ActionWorkloadRestart, "shop/web", "", protocol.Expectation{}, ErrInvalid},
		{"upper-case kind", cluster, protocol.ActionWorkloadRestart, "shop/Deployment/web", "", protocol.Expectation{}, ErrInvalid},
		{"pod for a workload action", cluster, protocol.ActionWorkloadRestart, "shop/pod/web-7c9", "", protocol.Expectation{}, ErrInvalid},
		{"workload for pod.delete", cluster, protocol.ActionPodDelete, "shop/deployment/web", "web", protocol.Expectation{}, ErrInvalid},
		{"scale without replicas", cluster, protocol.ActionWorkloadScale, "shop/deployment/web", "", protocol.Expectation{}, ErrInvalid},
		{"scale below zero", cluster, protocol.ActionWorkloadScale, "shop/deployment/web", "", protocol.Expectation{Replicas: replicas(-1)}, ErrInvalid},
		{"scale past the bound", cluster, protocol.ActionWorkloadScale, "shop/deployment/web", "", protocol.Expectation{Replicas: replicas(1001)}, ErrInvalid},
		{"scale a DaemonSet", cluster, protocol.ActionWorkloadScale, "shop/daemonset/agent", "", protocol.Expectation{Replicas: replicas(1)}, ErrInvalid},
		{"restart with replicas", cluster, protocol.ActionWorkloadRestart, "shop/deployment/web", "", protocol.Expectation{Replicas: replicas(1)}, ErrInvalid},
		{"docker restart with replicas", host, protocol.ActionRestart, "web", "", protocol.Expectation{Replicas: replicas(1)}, ErrInvalid},
		{"a state expectation", cluster, protocol.ActionWorkloadRestart, "shop/deployment/web", "", protocol.Expectation{State: "running"}, ErrInvalid},
		{"delete without the name", cluster, protocol.ActionWorkloadDelete, "shop/deployment/web", "", protocol.Expectation{}, ErrInvalid},
		{"delete with another name", cluster, protocol.ActionWorkloadDelete, "shop/deployment/web", "api", protocol.Expectation{}, ErrInvalid},
		{"namespace not granted", cluster, protocol.ActionWorkloadRestart, "other/deployment/web", "", protocol.Expectation{}, ErrNamespaceNotGranted},
		{"not in the inventory", cluster, protocol.ActionWorkloadRestart, "shop/deployment/ghost", "", protocol.Expectation{}, ErrNotFound},
		{"delete a managed workload", cluster, protocol.ActionWorkloadDelete, "shop/deployment/api", "api", protocol.Expectation{}, ErrWorkloadManaged},
		{"a Docker endpoint", host, protocol.ActionWorkloadRestart, "shop/deployment/web", "", protocol.Expectation{}, ErrRuntimeUnsupported},
	} {
		if _, err := ts.CreateCommand(ctx, a, c.endpoint, c.action, c.reference, c.confirm, c.expects); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	for _, c := range []struct {
		action, reference, confirm string
		expects                    protocol.Expectation
	}{
		{protocol.ActionWorkloadRestart, "shop/deployment/api", "", protocol.Expectation{}}, // managed: restart is allowed
		{protocol.ActionWorkloadScale, "shop/deployment/api", "", protocol.Expectation{Replicas: replicas(0)}},
		{protocol.ActionWorkloadScale, "shop/deployment/web", "", protocol.Expectation{Replicas: replicas(1000)}},
		{protocol.ActionWorkloadDelete, "shop/daemonset/agent", "agent", protocol.Expectation{}},
		{protocol.ActionPodDelete, "shop/pod/web-7c9", "web-7c9", protocol.Expectation{}},
	} {
		cmd, err := ts.CreateCommand(ctx, a, cluster, c.action, c.reference, c.confirm, c.expects)
		if err != nil || cmd.Reference != c.reference || cmd.ContainerID != "" || cmd.Action != c.action {
			t.Fatalf("%s %s: %+v %v", c.action, c.reference, cmd, err)
		}
	}
	web, err := ts.ListCommands(ctx, a, cluster, "", "shop/deployment/web", 0)
	if err != nil || len(web) != 1 || web[0].Expects.Replicas == nil || *web[0].Expects.Replicas != 1000 {
		t.Fatalf("listed %+v %v", web, err)
	}
	rows, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, row := range rows {
		if row.Result == "success" {
			found[row.Action+" "+row.Resource] = row.Details
		}
	}
	if d, ok := found["workload.scale "+cluster+"/shop/deployment/web"]; !ok || d != "replicas=1000" {
		t.Errorf("scale audit %q %v in %v", d, ok, found)
	}
	if _, ok := found["pod.delete "+cluster+"/shop/pod/web-7c9"]; !ok {
		t.Errorf("no pod.delete audit in %v", found)
	}
}

func testApply() WorkloadApply {
	return WorkloadApply{Target: protocol.WorkloadRef{Namespace: "shop", Kind: protocol.WorkloadDeployment, Name: "web"}, Confirm: "web", Spec: protocol.WorkloadConfiguration{
		ResourceVersion: "42", Replicas: replicas(2), Strategy: "RollingUpdate",
		Containers:     []protocol.WorkloadContainer{{Name: "web", Image: "ghcr.io/acme/web:2", Command: []string{}, Args: []string{}, Env: []protocol.WorkloadEnv{{Name: "TOKEN", Value: workloadSentinel}}}},
		InitContainers: []protocol.WorkloadContainer{}, EnvFrom: []string{},
	}}
}

// A workload apply is a direct command: one live per endpoint, refused for a managed workload,
// an ungranted namespace or a spec the protocol refuses, and settled from a deployment.result
// with no request ID and the workload service. Nothing it audits carries a value.
func TestWorkloadApplyCommand(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeClusterWith(t, ts, a, testCluster(), "shop")
	for name, c := range map[string]struct {
		mutate func(*WorkloadApply)
		want   error
	}{
		"confirm": {func(w *WorkloadApply) { w.Confirm = "api" }, ErrInvalid},
		"pod": {func(w *WorkloadApply) {
			w.Target.Kind, w.Target.Name, w.Confirm = protocol.WorkloadPod, "web-7c9", "web-7c9"
		}, ErrInvalid},
		"managed":      {func(w *WorkloadApply) { w.Target.Name, w.Confirm = "api", "api" }, ErrWorkloadManaged},
		"namespace":    {func(w *WorkloadApply) { w.Target.Namespace = "other" }, ErrNamespaceNotGranted},
		"absent":       {func(w *WorkloadApply) { w.Target.Name, w.Confirm = "ghost", "ghost" }, ErrNotFound},
		"read managed": {func(w *WorkloadApply) { w.Spec.Managed = true }, ErrWorkloadManaged},
	} {
		w := testApply()
		c.mutate(&w)
		if _, _, err := ts.CreateWorkloadApply(ctx, a, cluster, w); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	incomplete := testApply()
	incomplete.Spec.Unsupported = []string{"env_field_ref"}
	var spec *InvalidSpecError
	if _, _, err := ts.CreateWorkloadApply(ctx, a, cluster, incomplete); !errors.As(err, &spec) || spec.Blockers[0] != "configuration_incomplete" {
		t.Fatalf("unsupported: %v", err)
	}
	bad := testApply()
	bad.Spec.Replicas = replicas(1001)
	if _, _, err := ts.CreateWorkloadApply(ctx, a, cluster, bad); !errors.As(err, &spec) || spec.Blockers[0] != "spec_invalid:replicas" {
		t.Fatalf("invalid spec: %v", err)
	}

	cmd, frame, err := ts.CreateWorkloadApply(ctx, a, cluster, testApply())
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Action != ActionWorkloadApply || cmd.Reference != "shop/deployment/web" || frame.Request != cmd.ID || frame.Endpoint != cluster || frame.ResourceVersion != "42" || frame.Validate(time.Now()) != nil {
		t.Fatalf("command %+v frame %+v", cmd, frame)
	}
	if _, _, err := ts.CreateWorkloadApply(ctx, a, cluster, testApply()); !errors.Is(err, ErrCommandInProgress) {
		t.Fatalf("second live apply: %v", err)
	}
	// A result naming the direct service, or carrying a request ID, is not this command's.
	res := protocol.DeploymentResult{Deployment: cmd.ID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{{Service: protocol.WorkloadApplyService, Step: protocol.StepApply, Outcome: protocol.OutcomeSucceeded}}, Services: []protocol.DeploymentIdentity{}}
	direct := res
	direct.Steps = []protocol.DeploymentStep{{Service: "direct", Step: protocol.StepApply, Outcome: protocol.OutcomeSucceeded}}
	if err := ts.SettleDirectCommand(ctx, cluster, direct); !errors.Is(err, ErrUnreadableResult) {
		t.Fatalf("direct-service result: %v", err)
	}
	echoed := res
	echoed.RequestID = cmd.RequestID
	if err := ts.SettleDirectCommand(ctx, cluster, echoed); !errors.Is(err, ErrUnreadableResult) {
		t.Fatalf("request-ID result: %v", err)
	}
	if err := ts.SettleCommand(ctx, cluster, cmd.ID, protocol.OutcomeFailed, "x"); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDirectCommand(ctx, cluster, res); err != nil {
		t.Fatalf("settle: %v", err)
	}
	listed, err := ts.ListCommands(ctx, a, cluster, "", "shop/deployment/web", 0)
	if err != nil || len(listed) != 1 || listed[0].Outcome != protocol.OutcomeSucceeded || listed[0].Result == nil {
		t.Fatalf("listed %+v %v", listed, err)
	}
	rows, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	intent, outcome := false, false
	for _, row := range rows {
		if strings.Contains(row.Details, workloadSentinel) || strings.Contains(row.Resource, workloadSentinel) {
			t.Fatalf("a value reached the audit: %+v", row)
		}
		if row.Action == ActionWorkloadApply && row.Resource == cluster+"/shop/deployment/web" {
			intent = intent || row.Details == "resource_version=42 containers=1"
			outcome = outcome || (row.Result == "success" && strings.HasPrefix(row.Details, "code=-"))
		}
	}
	if !intent || !outcome {
		t.Fatalf("intent %v outcome %v", intent, outcome)
	}
	var stored string
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT expects||result FROM endpoint_commands WHERE id=?`), cmd.ID).Scan(&stored); err != nil || strings.Contains(stored, workloadSentinel) {
		t.Fatalf("stored %q %v", stored, err)
	}
}

func testRun() WorkloadApply {
	w := testApply()
	w.Target.Name, w.Confirm, w.Spec.ResourceVersion = "shop-new", "shop-new", ""
	return w
}

// A workload run is a direct command beside apply: one live per endpoint, refused for a name the
// inventory already lists, an ungranted namespace, a kind other than Deployment and a spec the
// create frame refuses, and settled from the same deployment.result. Nothing it audits carries a
// value.
func TestWorkloadRunCommand(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeClusterWith(t, ts, a, testCluster(), "shop")
	host := activeEndpointWith(t, ts, a, nil, nil)
	if _, _, err := ts.CreateWorkloadRun(ctx, a, host, testRun()); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("docker host: %v", err)
	}
	for name, c := range map[string]struct {
		mutate func(*WorkloadApply)
		want   error
	}{
		"confirm":   {func(w *WorkloadApply) { w.Confirm = "web" }, ErrInvalid},
		"pod":       {func(w *WorkloadApply) { w.Target.Kind = protocol.WorkloadPod }, ErrInvalid},
		"namespace": {func(w *WorkloadApply) { w.Target.Namespace = "other" }, ErrNamespaceNotGranted},
	} {
		w := testRun()
		c.mutate(&w)
		if _, _, err := ts.CreateWorkloadRun(ctx, a, cluster, w); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	for name, c := range map[string]struct {
		mutate func(*WorkloadApply)
		want   string
	}{
		"taken":       {func(w *WorkloadApply) { w.Target.Name, w.Confirm = "web", "web" }, "name_taken"},
		"statefulset": {func(w *WorkloadApply) { w.Target.Kind = protocol.WorkloadStatefulSet }, "spec_invalid:target.kind"},
		"unsupported": {func(w *WorkloadApply) { w.Spec.Unsupported = []string{"env_field_ref"} }, "configuration_incomplete"},
		"version":     {func(w *WorkloadApply) { w.Spec.ResourceVersion = "42" }, "spec_invalid:resource_version"},
		"paused":      {func(w *WorkloadApply) { w.Spec.Paused = true }, "spec_invalid:frame"},
		"no replicas": {func(w *WorkloadApply) { w.Spec.Replicas = nil }, "spec_invalid:replicas"},
	} {
		w := testRun()
		c.mutate(&w)
		var spec *InvalidSpecError
		if _, _, err := ts.CreateWorkloadRun(ctx, a, cluster, w); !errors.As(err, &spec) || !slices.Equal(spec.Blockers, []string{c.want}) {
			t.Errorf("%s: %v %+v, want %s", name, err, spec, c.want)
		}
	}

	cmd, frame, err := ts.CreateWorkloadRun(ctx, a, cluster, testRun())
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Action != ActionWorkloadRun || cmd.Reference != "shop/deployment/shop-new" || !frame.Create || frame.ResourceVersion != "" || frame.Request != cmd.ID || frame.Validate(time.Now()) != nil {
		t.Fatalf("command %+v frame %+v", cmd, frame)
	}
	if p, ok := CommandPermission(ActionWorkloadRun); !ok || p != "container.configure" {
		t.Fatalf("permission %s %v", p, ok)
	}
	if _, _, err := ts.CreateWorkloadApply(ctx, a, cluster, testApply()); !errors.Is(err, ErrCommandInProgress) {
		t.Fatalf("apply beside a live run: %v", err)
	}
	// The kind is an input check: a StatefulSet run is 422 even while the slot is taken.
	sts := testRun()
	sts.Target.Kind = protocol.WorkloadStatefulSet
	if _, _, err := ts.CreateWorkloadRun(ctx, a, cluster, sts); !errors.As(err, new(*InvalidSpecError)) {
		t.Fatalf("statefulset beside a live run: %v", err)
	}
	if err := ts.SettleCommand(ctx, cluster, cmd.ID, protocol.OutcomeFailed, "x"); err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: cmd.ID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{{Service: protocol.WorkloadApplyService, Step: protocol.StepApply, Outcome: protocol.OutcomeSucceeded}}, Services: []protocol.DeploymentIdentity{}}
	if err := ts.SettleDirectCommand(ctx, cluster, res); err != nil {
		t.Fatalf("settle: %v", err)
	}
	listed, err := ts.ListCommands(ctx, a, cluster, "", "shop/deployment/shop-new", 0)
	if err != nil || len(listed) != 1 || listed[0].Outcome != protocol.OutcomeSucceeded || listed[0].Result == nil {
		t.Fatalf("listed %+v %v", listed, err)
	}
	rows, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	intent, outcome := false, false
	for _, row := range rows {
		if strings.Contains(row.Details, workloadSentinel) || strings.Contains(row.Resource, workloadSentinel) {
			t.Fatalf("a value reached the audit: %+v", row)
		}
		if row.Action == ActionWorkloadRun && row.Resource == cluster+"/shop/deployment/shop-new" && row.Result == "success" {
			intent = intent || row.Details == "containers=1 replicas=2"
			outcome = outcome || strings.HasPrefix(row.Details, "code=-")
		}
	}
	if !intent || !outcome {
		t.Fatalf("intent %v outcome %v", intent, outcome)
	}
}

// A pod terminal matches the inventory's pod: namespace granted, name confirmed, the UID the
// inventory knows, Running, and the container present. A pod spec never opens on a Docker host.
func TestOpenPodExecTarget(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeClusterWith(t, ts, a, testCluster(), "shop")
	host := activeEndpointWith(t, ts, a, nil, nil)
	good := protocol.ExecSpec{Pod: &protocol.PodTarget{Namespace: "shop", Name: "web-7c9", Container: "web", UID: "11111111-2222-4333-8444-555555555555"}, Argv: []string{"/bin/sh"}}
	for name, c := range map[string]struct {
		endpoint, confirm string
		mutate            func(*protocol.PodTarget)
		want              error
	}{
		"uid":       {cluster, "web-7c9", func(p *protocol.PodTarget) { p.UID = "99999999-2222-4333-8444-555555555555" }, ErrInvalid},
		"confirm":   {cluster, "web", func(*protocol.PodTarget) {}, ErrInvalid},
		"container": {cluster, "web-7c9", func(p *protocol.PodTarget) { p.Container = "sidecar" }, ErrInvalid},
		"absent":    {cluster, "ghost", func(p *protocol.PodTarget) { p.Name = "ghost" }, ErrNotFound},
		"namespace": {cluster, "web-7c9", func(p *protocol.PodTarget) { p.Namespace = "other" }, ErrNamespaceNotGranted},
		"docker":    {host, "web-7c9", func(*protocol.PodTarget) {}, ErrRuntimeUnsupported},
	} {
		spec := good
		pod := *good.Pod
		c.mutate(&pod)
		spec.Pod = &pod
		if _, err := ts.OpenExecTarget(ctx, a, c.endpoint, "stream-"+name, c.confirm, spec); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, err := ts.OpenExecTarget(ctx, a, cluster, "stream-ok", "web-7c9", good); err != nil {
		t.Fatal(err)
	}
	rows, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	opened := false
	for _, row := range rows {
		if strings.Contains(row.Details, "/bin/sh") {
			t.Fatalf("argv audited: %+v", row)
		}
		opened = opened || (row.Action == "pod.exec.open" && row.Resource == cluster+"/pods/shop/web-7c9/web" && strings.Contains(row.Details, `stream="stream-ok"`))
	}
	if !opened {
		t.Fatal("no pod.exec.open row")
	}
}
