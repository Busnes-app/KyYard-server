package client

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// A cluster agent advertises cluster capabilities only, deploy and remove included when it can;
// a Docker agent keeps deployment.apply, deployment.pull and deployment.remove.
func TestHelloCapabilitiesPerRuntime(t *testing.T) {
	deploy := func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{}
	}
	remove := func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{}
	}
	logs := func(context.Context, protocol.LogRequest, func([]byte) error) error { return nil }
	cluster := helloCapabilities(&Options{Kubernetes: true, Logs: logs, Deploy: deploy, Remove: remove})
	if !slices.Equal(cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs, protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesRemove}) || !protocol.CapabilitiesFit(protocol.RuntimeKubernetes, cluster) {
		t.Fatalf("cluster %v", cluster)
	}
	if readOnly := helloCapabilities(&Options{Kubernetes: true, Logs: logs}); !slices.Equal(readOnly, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs}) {
		t.Fatalf("read-only cluster %v", readOnly)
	}
	docker := helloCapabilities(&Options{Deploy: deploy, Remove: remove})
	if !slices.Equal(docker, []string{protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull, protocol.CapabilityDeploymentRemove}) || !protocol.CapabilitiesFit(protocol.RuntimeDocker, docker) {
		t.Fatalf("docker %v", docker)
	}
}

// A frame for the other runtime is refused invalid_request and never reaches the runtime.
func TestDeployerRefusesTheOtherRuntimesFrame(t *testing.T) {
	ran := false
	deploy := func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		ran = true
		return protocol.DeploymentResult{}
	}
	remove := func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult {
		ran = true
		return protocol.DeploymentResult{}
	}
	cluster := newDeployer(context.Background(), t.TempDir(), &Options{Kubernetes: true, Deploy: deploy, Remove: remove})
	out := make(chan outFrame, 4)
	defer cluster.attach(context.Background(), out)()
	raw, _ := json.Marshal(testRequest("ep_1"))
	cluster.handleApply(context.Background(), "ep_1", raw, out)
	raw, _ = json.Marshal(testRemoval("ep_1"))
	cluster.handleRemoval(context.Background(), "ep_1", raw, out)
	for range 2 {
		var res protocol.DeploymentResult
		if err := decodeResult(nextFrame(t, out), &res); err != nil || res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest {
			t.Fatalf("docker frame to a cluster agent: %+v %v", res, err)
		}
	}
	host := newDeployer(context.Background(), t.TempDir(), &Options{Deploy: deploy})
	defer host.attach(context.Background(), out)()
	req := testRequest("ep_1")
	req.Kubernetes = &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: "11111111-2222-4333-8444-555555555555", InstanceID: "66666666-7777-4888-9999-aaaaaaaaaaaa", SpecDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	raw, _ = json.Marshal(req)
	host.handleApply(context.Background(), "ep_1", raw, out)
	var res protocol.DeploymentResult
	if err := decodeResult(nextFrame(t, out), &res); err != nil || res.Code != protocol.ResultInvalidRequest {
		t.Fatalf("cluster frame to a Docker agent: %+v %v", res, err)
	}
	if ran {
		t.Fatal("a frame for the other runtime ran")
	}
}
