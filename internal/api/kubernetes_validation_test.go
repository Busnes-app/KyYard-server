package api_test

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const (
	clusterUID = "0f1e2d3c-4b5a-4968-8776-655443322110"
	clusterPod = "aaaaaaaa-1111-4111-8111-111111111111"
)

var (
	priorDigest  = "sha256:" + strings.Repeat("b", 64)
	updateDigest = "sha256:" + strings.Repeat("c", 64)
)

// clusterValidation is a clusterHost whose application shop (web: ghcr.io/org/web:1) already ran
// one succeeded manual apply at priorDigest: the deployment a rollback returns to. Every status
// read goes through inspect, which reports the web Deployment at the last settled generation, its
// one pod running with restarts.
type clusterValidation struct {
	clusterHost
	app, appID, instance string
	prior                string
	mu                   sync.Mutex
	generation           int64
	restarts             int32
}

func newClusterValidation(t *testing.T, capabilities ...string) *clusterValidation {
	t.Helper()
	v := &clusterValidation{clusterHost: newClusterHost(t, capabilities...)}
	v.app = v.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1}}")
	v.appID = strings.TrimPrefix(v.app, v.base+"/")
	v.do(t, "PUT", v.app+"/mapping", `{"endpoint_id":"`+v.ag.id+`","namespace":"shop"}`, 204)
	var mapped store.ApplicationMapping
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.app+"/mapping", "", 200)), &mapped); err != nil {
		t.Fatal(err)
	}
	v.instance = mapped.InstanceID
	api.SetPlanInspectorForTest(v.s, v.inspect)
	api.SetDigestResolverForTest(v.s, &fakeDigests{digest: priorDigest})
	body, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop"})
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "POST", v.app+"/deployments", string(body), 201)), &d); err != nil {
		t.Fatal(err)
	}
	v.do(t, "POST", v.app+"/deployments/"+d.ID+"/apply", `{"confirm":"shop"}`, 202)
	v.prior = v.settle(t, v.frame(t))
	return v
}

// inspect is the fake cluster's status read of the web Deployment.
func (v *clusterValidation) inspect(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return protocol.ContainerInspection{Target: target, ObservedAt: time.Now().UTC(), Workload: &protocol.WorkloadStatus{UID: clusterUID, Generation: v.generation, ObservedGeneration: v.generation, Desired: 1, Updated: 1, Ready: 1, Available: 1, Conditions: []protocol.WorkloadCondition{},
		Pods: []protocol.PodStatus{{Name: "shop-web-7d9f8b6c5-x2x4z", UID: clusterPod, Phase: "Running", Containers: []protocol.PodContainer{{Name: "web", State: "running", Ready: true, RestartCount: v.restarts}}}}}}, nil
}

func (v *clusterValidation) restart(n int32) {
	v.mu.Lock()
	v.restarts = n
	v.mu.Unlock()
}

// frame reads the next frame as a cluster deployment request.
func (v *clusterValidation) frame(t *testing.T) protocol.DeploymentRequest {
	t.Helper()
	f := readEnvelope(t, v.ctx, v.sock.conn)
	var req protocol.DeploymentRequest
	if f.Type != protocol.TypeDeploymentApply || json.Unmarshal(f.Payload, &req) != nil || req.Kubernetes == nil {
		t.Fatalf("expected a cluster deployment frame, got %s", f.Type)
	}
	return req
}

// settle answers req as the cluster agent would, the Deployment's generation one higher, and
// reports the Deployment running req's digest in the inventory.
func (v *clusterValidation) settle(t *testing.T, req protocol.DeploymentRequest) string {
	t.Helper()
	v.mu.Lock()
	v.generation++
	generation := v.generation
	v.mu.Unlock()
	digest := req.Services[0].Pull.Digest
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{},
		Services: []protocol.DeploymentIdentity{{Service: "web", Kind: protocol.KindDeployment, Namespace: "shop", Name: "shop-web", UID: clusterUID, Generation: generation, ImageDigest: digest}}}
	if err := v.st.Tenancy().SettleDeployment(context.Background(), v.ag.id, res); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.36.0"}, Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "n1", Ready: true}}, Namespaces: []string{"shop"}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{},
			Workloads: []protocol.Workload{{Kind: protocol.KindDeployment, Namespace: "shop", Name: "shop-web", Desired: 1, Ready: 1, Updated: 1, Images: []string{"ghcr.io/org/web@" + digest}, Application: v.appID, Instance: v.instance}}}})
	if _, err := v.st.Tenancy().AcceptInventory(context.Background(), v.ag.id, nextGeneration(), time.Now(), raw); err != nil {
		t.Fatal(err)
	}
	return req.Deployment
}

func (v *clusterValidation) access() store.TenantAccess {
	return store.TenantAccess{ActorID: "usr_deployer", OrganizationID: "a", EnvironmentID: "env-a", CorrelationID: "cluster-validation-test"}
}

// automate saves an apply-mode policy as the deployer, offers updateDigest and runs its window:
// the policy applies the update and the agent settles it.
func (v *clusterValidation) automate(t *testing.T) string {
	t.Helper()
	var days []int
	for d := range 7 {
		if d != int(time.Now().UTC().Weekday()) {
			days = append(days, d)
		}
	}
	if _, _, err := v.st.Tenancy().PutUpdatePolicy(context.Background(), v.access(), v.appID, store.PolicyInput{Mode: store.PolicyModeApply, Timezone: "UTC", Weekdays: days, StartMinute: 600, EndMinute: 660}); err != nil {
		t.Fatal(err)
	}
	api.SetDigestResolverForTest(v.s, &fakeDigests{digest: updateDigest})
	api.PolicyTickForTest(v.s, tomorrow().Add(10*time.Hour+30*time.Minute))
	api.WaitPolicyRunsForTest(v.s)
	runs, err := v.st.Tenancy().ListPolicyRuns(context.Background(), v.access(), v.appID, 10)
	if err != nil || len(runs) != 1 || runs[0].Outcome != store.RunApplied {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	req := v.frame(t)
	if req.Deployment != runs[0].DeploymentID || req.Services[0].Pull.Digest != updateDigest {
		t.Fatalf("update frame %+v", req)
	}
	return v.settle(t, req)
}

func (v *clusterValidation) validation(t *testing.T, deployment string) *store.Validation {
	t.Helper()
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.app+"/deployments/"+deployment, "", 200)), &d); err != nil || d.Validation == nil {
		t.Fatalf("deployment %s: %+v %v", deployment, d, err)
	}
	return d.Validation
}

func (v *clusterValidation) at(t *testing.T, deployment string, offset time.Duration) {
	t.Helper()
	api.ValidationTickForTest(v.s, v.validation(t, deployment).StartedAt.Add(offset))
}

func (v *clusterValidation) policy(t *testing.T) *store.UpdatePolicy {
	t.Helper()
	p, _, err := v.st.Tenancy().ReadUpdatePolicy(context.Background(), v.access(), v.appID)
	if err != nil || p == nil {
		t.Fatalf("policy: %+v %v", p, err)
	}
	return p
}

var inspectingCluster = append(slices.Clone(clusterCapabilities), protocol.CapabilityKubernetesInspect)

// An automated cluster update whose Deployment stays available with no restart is healthy, and
// its policy stays active.
func TestClusterValidationRecordsAHealthyUpdate(t *testing.T) {
	v := newClusterValidation(t, inspectingCluster...)
	id := v.automate(t)
	if got := v.validation(t, id); !got.Automated || got.Phase != store.PhaseGrace {
		t.Fatalf("opened: %+v", got)
	}
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Phase != store.PhaseObserving {
		t.Fatalf("after grace: %+v", got)
	}
	v.at(t, id, afterWindow)
	if got := v.validation(t, id); got.Verdict != store.VerdictHealthy || got.Rollback != nil {
		t.Fatalf("finished: %+v", got)
	}
	if p := v.policy(t); p.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", p)
	}
}

// A restarting cluster update is rolled back to the prior apply's digest although the registry
// now answers another one; the rollback applies, settles and is validated as a rollback, and the
// policy pauses (Review Focus 3).
func TestClusterValidationRollsBackToThePriorDigests(t *testing.T) {
	v := newClusterValidation(t, inspectingCluster...)
	id := v.automate(t)
	v.at(t, id, afterGrace)
	v.restart(2)
	v.at(t, id, afterGrace+store.ValidationPoll)
	req := v.frame(t)
	if req.Revision != 1 || len(req.Services) != 1 || req.Services[0].Pull.Digest != priorDigest || req.Services[0].Pull.Reference != "ghcr.io/org/web@"+priorDigest || req.Kubernetes.Namespace != "shop" {
		t.Fatalf("rollback frame %+v", req)
	}
	got := v.validation(t, id)
	if got.Verdict != store.VerdictRestarting || got.Detail != "web" || got.Rollback == nil || *got.Rollback != (store.ValidationRollback{DeploymentID: req.Deployment, Revision: 1, Outcome: store.RollbackApplied}) {
		t.Fatalf("validation %+v %+v", got, got.Rollback)
	}
	if p := v.policy(t); p.Status != store.PolicyPaused || p.PausedReason != store.ValidationReasonRolledBack {
		t.Fatalf("policy %+v", p)
	}
	v.restart(0)
	v.settle(t, req)
	if rb := v.validation(t, req.Deployment); !rb.IsRollback || rb.Phase != store.PhaseGrace {
		t.Fatalf("the rollback's validation %+v", rb)
	}
}

// A cluster agent without kubernetes.inspect cannot validate an automated update: it is
// unverifiable with the upgrade detail, and the policy pauses with that reason.
func TestClusterValidationWithoutTheInspectCapability(t *testing.T) {
	v := newClusterValidation(t, clusterCapabilities...)
	id := v.automate(t)
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Verdict != store.VerdictUnverifiable || got.Detail != store.ValidationDetailNoInspect {
		t.Fatalf("validation %+v", got)
	}
	if p := v.policy(t); p.Status != store.PolicyPaused || p.PausedReason != store.ValidationReasonUnverified+store.ValidationDetailNoInspect {
		t.Fatalf("policy %+v", p)
	}
}

// Over the real socket the loop's grant names the settled Deployment and no container, and the
// agent's WorkloadStatus answer, validated, becomes the baseline.
func TestClusterValidationInspectsOverTheAgentSocket(t *testing.T) {
	v := newClusterValidation(t, inspectingCluster...)
	api.SetPlanInspectorForTest(v.s, nil)
	start := v.validation(t, v.prior).StartedAt
	done := make(chan struct{})
	go func() { defer close(done); api.ValidationTickForTest(v.s, start.Add(afterGrace)) }()
	g := grant(t, v.ctx, v.sock)
	if g.Actor != "system-validation" || g.ValidateFor(time.Now(), protocol.RuntimeKubernetes) != nil || g.Target.Workload != (protocol.WorkloadRef{Namespace: "shop", Name: "shop-web", UID: clusterUID}) || g.Target.ContainerID != "" {
		t.Fatalf("grant %+v", g)
	}
	v.restart(1)
	in, _ := v.inspect(context.Background(), g.Target)
	writeEnvelope(t, v.ctx, v.sock.conn, protocol.TypeInspectionResult, protocol.InspectionResult{Request: g.Request, Status: "ok", Result: &in})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick never finished")
	}
	pending, err := v.st.Tenancy().PendingValidations(context.Background())
	if err != nil || len(pending) != 1 || pending[0].Phase != store.PhaseObserving || !maps.Equal(pending[0].Baseline["web"].PodRestarts, map[string]int{clusterPod: 1}) {
		t.Fatalf("pending %+v %v", pending, err)
	}
}
