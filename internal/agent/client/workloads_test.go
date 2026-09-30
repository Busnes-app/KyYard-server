package client

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

var testWorkload = protocol.WorkloadRef{Namespace: "shop", Kind: protocol.WorkloadDeployment, Name: "web"}

func testWorkloadConfiguration() *protocol.WorkloadConfiguration {
	replicas := int32(2)
	return &protocol.WorkloadConfiguration{Target: testWorkload, ObservedAt: time.Now(), ResourceVersion: "42", Replicas: &replicas, Strategy: "RollingUpdate",
		Containers:     []protocol.WorkloadContainer{{Name: "web", Image: "ghcr.io/acme/web:1", Command: []string{}, Args: []string{}, Env: []protocol.WorkloadEnv{{Name: "TOKEN", Value: "agent-secret-canary"}}}},
		InitContainers: []protocol.WorkloadContainer{}, EnvFrom: []string{}, Unsupported: []string{}}
}

func testWorkloadApply(endpoint string) protocol.WorkloadApply {
	spec := *testWorkloadConfiguration()
	spec.ObservedAt = time.Time{}
	return protocol.WorkloadApply{Request: "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a", Endpoint: endpoint, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Target: testWorkload, ResourceVersion: "42", Spec: spec}
}

func workloadOptions() *Options {
	return &Options{
		Kubernetes: true,
		Operate:    func(context.Context, protocol.Command) (string, string) { return protocol.OutcomeSucceeded, "" },
		ReadWorkload: func(context.Context, protocol.InspectionTarget) (*protocol.WorkloadConfiguration, error) {
			return testWorkloadConfiguration(), nil
		},
		ApplyWorkload: func(context.Context, protocol.WorkloadApply, func()) protocol.DeploymentResult {
			return protocol.DeploymentResult{}
		},
		Exec: func(context.Context, protocol.ExecSpec) (ExecSession, error) { return nil, nil },
	}
}

// A cluster agent that operates, reads and applies workloads says kubernetes.workloads and
// kubernetes.workloads.run (its apply creates), and one
// that can open a pod terminal says pod.exec; neither appears without its runtime call, and a
// Docker agent never names them.
func TestHelloCapabilitiesForWorkloads(t *testing.T) {
	caps := helloCapabilities(workloadOptions())
	if !slices.Contains(caps, protocol.CapabilityKubernetesWorkloads) || !slices.Contains(caps, protocol.CapabilityKubernetesWorkloadsRun) || !slices.Contains(caps, protocol.CapabilityPodExec) || !protocol.CapabilitiesFit(protocol.RuntimeKubernetes, caps) {
		t.Fatalf("cluster %v", caps)
	}
	for name, drop := range map[string]func(*Options){
		"operate": func(o *Options) { o.Operate = nil },
		"read":    func(o *Options) { o.ReadWorkload = nil },
		"apply":   func(o *Options) { o.ApplyWorkload = nil },
	} {
		o := workloadOptions()
		drop(o)
		if caps := helloCapabilities(o); slices.Contains(caps, protocol.CapabilityKubernetesWorkloads) || slices.Contains(caps, protocol.CapabilityKubernetesWorkloadsRun) {
			t.Errorf("without %s the agent still says %v", name, caps)
		}
	}
	o := workloadOptions()
	o.Exec = nil
	if slices.Contains(helloCapabilities(o), protocol.CapabilityPodExec) {
		t.Error("pod.exec without an exec runtime")
	}
	o = workloadOptions()
	o.Kubernetes = false
	if docker := helloCapabilities(o); slices.Contains(docker, protocol.CapabilityKubernetesWorkloads) || slices.Contains(docker, protocol.CapabilityKubernetesWorkloadsRun) || slices.Contains(docker, protocol.CapabilityPodExec) {
		t.Fatalf("docker %v", docker)
	}
}

// A cluster agent answers a configuration grant for a workload with the workload's read; a
// UID-shaped (inspection) target is not a configuration read and comes back unavailable.
func TestClusterConfigurationReadsTheWorkload(t *testing.T) {
	out := make(chan outFrame, 8)
	var asked protocol.InspectionTarget
	opts := workloadOptions()
	opts.ReadWorkload = func(_ context.Context, target protocol.InspectionTarget) (*protocol.WorkloadConfiguration, error) {
		asked = target
		return testWorkloadConfiguration(), nil
	}
	req := configurationRequest(15 * time.Second)
	req.Target = protocol.InspectionTarget{Workload: testWorkload}
	if err := newConfigurationsFor(req, opts, out).handle(execFrame(protocol.TypeConfigurationOpen, req), true); err != nil {
		t.Fatal(err)
	}
	reply := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult)
	if reply.Status != "ok" || reply.Workload == nil || reply.Result != nil || reply.Validate() != nil || asked != req.Target {
		t.Fatalf("reply %+v asked %+v", reply, asked)
	}

	// Another workload's read is not an answer.
	opts.ReadWorkload = func(context.Context, protocol.InspectionTarget) (*protocol.WorkloadConfiguration, error) {
		c := testWorkloadConfiguration()
		c.Target.Name = "other"
		return c, nil
	}
	req.Request = "request-2"
	if err := newConfigurationsFor(req, opts, out).handle(execFrame(protocol.TypeConfigurationOpen, req), true); err != nil {
		t.Fatal(err)
	}
	if reply := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult); reply.Status != "unavailable" || reply.Workload != nil {
		t.Fatalf("mismatched read %+v", reply)
	}

	// The UID shape is inspection's.
	req.Request = "request-3"
	req.Target = protocol.InspectionTarget{Workload: protocol.WorkloadRef{Namespace: "shop", Name: "web", UID: "11111111-2222-4333-8444-555555555555"}}
	if err := newConfigurationsFor(req, opts, out).handle(execFrame(protocol.TypeConfigurationOpen, req), true); err != nil {
		t.Fatal(err)
	}
	if reply := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult); reply.Status != "unavailable" {
		t.Fatalf("inspection-shaped target %+v", reply)
	}

	// Without a read, the cluster agent is unavailable rather than reaching for Configure.
	opts = workloadOptions()
	opts.ReadWorkload = nil
	req.Request, req.Target = "request-4", protocol.InspectionTarget{Workload: testWorkload}
	if err := newConfigurationsFor(req, opts, out).handle(execFrame(protocol.TypeConfigurationOpen, req), true); err != nil {
		t.Fatal(err)
	}
	if reply := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult); reply.Status != "unavailable" {
		t.Fatalf("no read %+v", reply)
	}
}

// workload.apply runs through the deployment slot like deployment.apply and answers
// deployment.result; the values it carried are cleared after the run.
func TestWorkloadApplyAnswersADeploymentResult(t *testing.T) {
	var seen protocol.WorkloadApply
	started := false
	opts := workloadOptions()
	opts.ApplyWorkload = func(_ context.Context, req protocol.WorkloadApply, begin func()) protocol.DeploymentResult {
		seen = req
		begin()
		started = true
		return protocol.DeploymentResult{Deployment: req.Request, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{{Service: protocol.WorkloadApplyService, Step: protocol.StepApply, Outcome: protocol.OutcomeSucceeded}}, Services: []protocol.DeploymentIdentity{}}
	}
	d := newDeployer(context.Background(), t.TempDir(), opts)
	out := make(chan outFrame, 4)
	defer d.attach(context.Background(), out)()
	req := testWorkloadApply("ep_1")
	raw, _ := json.Marshal(req)
	d.handleWorkloadApply(context.Background(), "ep_1", raw, out)
	var res protocol.DeploymentResult
	if err := decodeResult(nextFrame(t, out), &res); err != nil || res.Deployment != req.Request || res.Outcome != protocol.OutcomeSucceeded || res.RequestID != "" {
		t.Fatalf("result %+v %v", res, err)
	}
	if !started || seen.Target != testWorkload || seen.ResourceVersion != "42" {
		t.Fatalf("ran %v with %+v", started, seen)
	}
	d.wait()
	// A re-sent frame replays the remembered result without running again.
	started = false
	d.handleWorkloadApply(context.Background(), "ep_1", raw, out)
	if err := decodeResult(nextFrame(t, out), &res); err != nil || res.Outcome != protocol.OutcomeSucceeded || started {
		t.Fatalf("replay %+v %v ran=%v", res, err, started)
	}
}

// An apply for another endpoint, an invalid one, or one sent to a Docker agent is refused and
// never runs.
func TestWorkloadApplyRefusals(t *testing.T) {
	ran := false
	opts := workloadOptions()
	opts.ApplyWorkload = func(context.Context, protocol.WorkloadApply, func()) protocol.DeploymentResult {
		ran = true
		return protocol.DeploymentResult{}
	}
	out := make(chan outFrame, 8)
	d := newDeployer(context.Background(), t.TempDir(), opts)
	defer d.attach(context.Background(), out)()
	send := func(d *deployer, req protocol.WorkloadApply) protocol.DeploymentResult {
		t.Helper()
		raw, _ := json.Marshal(req)
		d.handleWorkloadApply(context.Background(), "ep_1", raw, out)
		var res protocol.DeploymentResult
		if err := decodeResult(nextFrame(t, out), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := send(d, testWorkloadApply("ep_2")); res.Code != protocol.ResultWrongEndpoint {
		t.Fatalf("other endpoint %+v", res)
	}
	bad := testWorkloadApply("ep_1")
	bad.Request = "4d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a"
	bad.ResourceVersion = "43"
	if res := send(d, bad); res.Code != protocol.ResultInvalidRequest {
		t.Fatalf("mismatched version %+v", res)
	}
	docker := workloadOptions()
	docker.Kubernetes = false
	host := newDeployer(context.Background(), t.TempDir(), docker)
	defer host.attach(context.Background(), out)()
	other := testWorkloadApply("ep_1")
	other.Request = "5d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a"
	if res := send(host, other); res.Code != protocol.ResultInvalidRequest {
		t.Fatalf("docker agent %+v", res)
	}
	if ran {
		t.Fatal("a refused apply ran")
	}
}

// A pod exec grant reaches only a cluster agent, and a container grant only a Docker agent: the
// other shape is a protocol error before any runtime call.
func TestExecTargetMatchesTheRuntime(t *testing.T) {
	pod := execRequest()
	pod.Spec = protocol.ExecSpec{Pod: &protocol.PodTarget{Namespace: "shop", Name: "web-7c9", Container: "web", UID: "11111111-2222-4333-8444-555555555555"}, Argv: []string{"/bin/sh"}}
	for _, c := range []struct {
		kube bool
		req  protocol.ExecOpen
	}{{false, pod}, {true, execRequest()}} {
		called := false
		opts := &Options{Kubernetes: c.kube, Exec: func(context.Context, protocol.ExecSpec) (ExecSession, error) { called = true; return nil, nil }}
		s := newExecStreams(context.Background(), "endpoint", make([]byte, 32), &execBudget{}, opts, make(chan outFrame, 4))
		if err := s.handle(execFrame(protocol.TypeExecOpen, c.req), true); err != errExecProtocol || called {
			t.Errorf("kubernetes=%v pod=%v: %v called=%v", c.kube, c.req.Spec.Pod != nil, err, called)
		}
	}
	// The matching shape is admitted.
	opened := make(chan protocol.ExecSpec, 1)
	opts := &Options{Kubernetes: true, Exec: func(_ context.Context, spec protocol.ExecSpec) (ExecSession, error) {
		opened <- spec
		return nil, context.Canceled
	}}
	s := newExecStreams(context.Background(), "endpoint", make([]byte, 32), &execBudget{}, opts, make(chan outFrame, 4))
	if err := s.handle(execFrame(protocol.TypeExecOpen, pod), true); err != nil {
		t.Fatal(err)
	}
	select {
	case spec := <-opened:
		if spec.Pod == nil || spec.Pod.UID != pod.Spec.Pod.UID || !strings.HasPrefix(spec.Argv[0], "/bin") {
			t.Fatalf("spec %+v", spec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pod exec never reached the runtime")
	}
}
