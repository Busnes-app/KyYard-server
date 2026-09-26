package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const testWorkloadUID = "0f1e2d3c-4b5a-4968-8776-655443322110"

func workloadRequest() protocol.InspectionOpen {
	req := inspectionRequest()
	req.Target = protocol.InspectionTarget{Workload: protocol.WorkloadRef{Namespace: "shop", Name: "shop-web", UID: testWorkloadUID}}
	return req
}

// A cluster agent takes a Deployment target and refuses a container one; a Docker agent the
// reverse. An answer too large for the frame is sent unavailable, never oversized.
func TestClusterInspectionTargetsAndFrameBound(t *testing.T) {
	var pods []protocol.PodStatus
	answer := func(ctx context.Context, target protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
		return &protocol.ContainerInspection{Target: target, ObservedAt: time.Now(), Workload: &protocol.WorkloadStatus{UID: testWorkloadUID, Generation: 1, ObservedGeneration: 1, Desired: 1, Updated: 1, Ready: 1, Available: 1, Conditions: []protocol.WorkloadCondition{}, Pods: pods}}, nil
	}
	out := make(chan outFrame, 8)
	cluster := &Options{Kubernetes: true, inspectionSlots: make(chan struct{}, 2), Inspect: answer}
	req := workloadRequest()
	s := newInspections(context.Background(), req.Endpoint, req.Connection, cluster, out)
	if err := s.handle(execFrame(protocol.TypeInspectionOpen, req), true); err != nil {
		t.Fatal(err)
	}
	if reply := nextExecFrame(t, out, protocol.TypeInspectionResult).Payload.(protocol.InspectionResult); reply.Status != "ok" || reply.Result.Workload == nil {
		t.Fatalf("workload answer: %+v", reply)
	}
	docker := inspectionRequest()
	docker.Request = "docker"
	if err := s.handle(execFrame(protocol.TypeInspectionOpen, docker), true); err == nil {
		t.Fatal("a cluster agent took a container target")
	}
	host := newInspections(context.Background(), req.Endpoint, req.Connection, &Options{inspectionSlots: make(chan struct{}, 2), Inspect: answer}, out)
	if err := host.handle(execFrame(protocol.TypeInspectionOpen, workloadRequest()), true); err == nil {
		t.Fatal("a Docker agent took a Deployment target")
	}
	// MaxWorkloadPods pods with long names pass validation but do not fit the frame.
	for range protocol.MaxWorkloadPods {
		pods = append(pods, protocol.PodStatus{Name: strings.Repeat("p", 250), UID: testWorkloadUID, Phase: "Running", Containers: []protocol.PodContainer{{Name: "web", State: "running"}}})
	}
	big := workloadRequest()
	big.Request = "big"
	if in, _ := answer(context.Background(), big.Target); in.Validate(big.Target, time.Now(), true) != nil {
		t.Fatal("the oversized answer must be valid, or the bound is not what refuses it")
	}
	if err := s.handle(execFrame(protocol.TypeInspectionOpen, big), true); err != nil {
		t.Fatal(err)
	}
	if reply := nextExecFrame(t, out, protocol.TypeInspectionResult).Payload.(protocol.InspectionResult); reply.Status != "unavailable" || reply.Result != nil {
		t.Fatalf("oversized answer: %s", reply.Status)
	}
}
