package protocol

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testWorkloadUID = "0f1e2d3c-4b5a-4968-8776-655443322110"
	testPodUID      = "11111111-2222-4333-8444-555555555555"
)

func workloadTarget() InspectionTarget {
	return InspectionTarget{Workload: WorkloadRef{Namespace: "shop", Name: "shop-web", UID: testWorkloadUID}}
}

func workloadAnswer() ContainerInspection {
	return ContainerInspection{Target: workloadTarget(), ObservedAt: time.Now(), Workload: &WorkloadStatus{UID: testWorkloadUID, Generation: 2, ObservedGeneration: 2, Desired: 1, Updated: 1, Ready: 1, Available: 1,
		Conditions: []WorkloadCondition{{Type: "Available", Status: "True", Reason: "MinimumReplicasAvailable"}},
		Pods:       []PodStatus{{Name: "shop-web-7d9f8b6c5-x2x4z", UID: testPodUID, Phase: "Running", Containers: []PodContainer{{Name: "web", State: "running", Ready: true, RestartCount: 1}}}}}}
}

// A target has exactly one shape per runtime, and a grant is checked against the agent's.
func TestInspectionTargetShapePerRuntime(t *testing.T) {
	docker := InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	both := docker
	both.Workload = workloadTarget().Workload
	for _, tc := range []struct {
		name    string
		target  InspectionTarget
		runtime string
		ok      bool
	}{
		{"docker target on docker", docker, RuntimeDocker, true},
		{"workload target on kubernetes", workloadTarget(), RuntimeKubernetes, true},
		{"docker target on kubernetes", docker, RuntimeKubernetes, false},
		{"workload target on docker", workloadTarget(), RuntimeDocker, false},
		{"both on kubernetes", both, RuntimeKubernetes, false},
		{"both on docker", both, RuntimeDocker, false},
		{"namespace not a label", InspectionTarget{Workload: WorkloadRef{Namespace: "Shop", Name: "shop-web", UID: testWorkloadUID}}, RuntimeKubernetes, false},
		{"name not a label", InspectionTarget{Workload: WorkloadRef{Namespace: "shop", Name: "shop.web", UID: testWorkloadUID}}, RuntimeKubernetes, false},
		{"uid not a uuid", InspectionTarget{Workload: WorkloadRef{Namespace: "shop", Name: "shop-web", UID: "secret-canary"}}, RuntimeKubernetes, false},
		{"empty", InspectionTarget{}, RuntimeKubernetes, false},
	} {
		if err := tc.target.ValidateFor(tc.runtime); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
		grant := InspectionOpen{Request: "r", Endpoint: "e", Actor: "a", Connection: make([]byte, 32), Expires: time.Now().Add(10 * time.Second), Target: tc.target}
		if err := grant.ValidateFor(time.Now(), tc.runtime); (err == nil) != tc.ok {
			t.Errorf("%s grant: %v", tc.name, err)
		}
	}
	// A Docker target's wire form is unchanged: no workload key.
	raw, _ := json.Marshal(docker)
	if strings.Contains(string(raw), "workload") {
		t.Fatalf("docker target grew a workload key: %s", raw)
	}
	raw, _ = json.Marshal(ContainerInspection{Target: docker})
	if strings.Contains(string(raw), "workload") {
		t.Fatalf("docker answer grew a workload key: %s", raw)
	}
}

// A cluster answer carries a bounded WorkloadStatus and no Docker field; a Docker answer never
// carries a WorkloadStatus.
func TestWorkloadAnswerValidation(t *testing.T) {
	target := workloadTarget()
	if err := workloadAnswer().Validate(target, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	missing := ContainerInspection{Target: target, ObservedAt: time.Now(), Workload: &WorkloadStatus{Missing: true}}
	if err := missing.Validate(target, time.Now(), false); err != nil {
		t.Fatalf("missing: %v", err)
	}
	recreated := workloadAnswer()
	recreated.Workload.UID = testPodUID
	if err := recreated.Validate(target, time.Now(), false); err != nil {
		t.Fatalf("a live UID other than the target's is a valid answer: %v", err)
	}
	pods := func(n, containers int) []PodStatus {
		out := make([]PodStatus, n)
		for i := range out {
			out[i] = PodStatus{Name: "p", UID: testPodUID, Phase: "Running", Containers: make([]PodContainer, containers)}
			for j := range out[i].Containers {
				out[i].Containers[j] = PodContainer{Name: "c", State: "running"}
			}
		}
		return out
	}
	full := workloadAnswer()
	full.Workload.Pods = pods(MaxWorkloadPods, 1)
	full.Workload.Conditions = make([]WorkloadCondition, MaxWorkloadConditions)
	for i := range full.Workload.Conditions {
		full.Workload.Conditions[i] = WorkloadCondition{Type: "Progressing", Status: "Unknown"}
	}
	if err := full.Validate(target, time.Now(), false); err != nil {
		t.Fatalf("at the caps: %v", err)
	}
	for name, mutate := range map[string]func(*ContainerInspection){
		"no status":          func(r *ContainerInspection) { r.Workload = nil },
		"docker state":       func(r *ContainerInspection) { r.State = "running" },
		"docker health":      func(r *ContainerInspection) { r.Health = "healthy" },
		"verified":           func(r *ContainerInspection) { r.ConfigurationVerified = true },
		"other target":       func(r *ContainerInspection) { r.Target.Workload.Name = "shop-api" },
		"missing with a uid": func(r *ContainerInspection) { r.Workload.Missing = true },
		"uid not a uuid":     func(r *ContainerInspection) { r.Workload.UID = "secret-canary" },
		"generation zero":    func(r *ContainerInspection) { r.Workload.Generation = 0 },
		"observed ahead":     func(r *ContainerInspection) { r.Workload.ObservedGeneration = r.Workload.Generation + 1 },
		"negative count":     func(r *ContainerInspection) { r.Workload.Available = -1 },
		"condition status":   func(r *ContainerInspection) { r.Workload.Conditions[0].Status = "Maybe" },
		"condition reason":   func(r *ContainerInspection) { r.Workload.Conditions[0].Reason = "secret canary" },
		"too many conditions": func(r *ContainerInspection) {
			r.Workload.Conditions = slices.Repeat([]WorkloadCondition{{Type: "Progressing", Status: "Unknown"}}, MaxWorkloadConditions+1)
		},
		"too many pods":           func(r *ContainerInspection) { r.Workload.Pods = pods(MaxWorkloadPods+1, 1) },
		"too many containers":     func(r *ContainerInspection) { r.Workload.Pods = pods(1, MaxPodContainers+1) },
		"pod phase":               func(r *ContainerInspection) { r.Workload.Pods[0].Phase = "Exploded" },
		"pod name":                func(r *ContainerInspection) { r.Workload.Pods[0].Name = "Shop_Web" },
		"container image":         func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].Image = "nginx:1" },
		"container image id":      func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].ImageID = "sha256:x" },
		"container state":         func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].State = "paused" },
		"container reason":        func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].Reason = "back-off 5m0s" },
		"negative restarts":       func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].RestartCount = -1 },
		"restarts over the bound": func(r *ContainerInspection) { r.Workload.Pods[0].Containers[0].RestartCount = MaxRestartCount + 1 },
	} {
		bad := workloadAnswer()
		mutate(&bad)
		if bad.Validate(target, time.Now(), false) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	docker := InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	answer := ContainerInspection{Target: docker, ObservedAt: time.Now(), State: "running", RestartPolicy: "no", NetworkMode: "none", ImagePlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}, ConfigurationVerified: true, Workload: workloadAnswer().Workload}
	if answer.Validate(docker, time.Now(), false) == nil {
		t.Fatal("a Docker answer carried a workload status")
	}
}

// kubernetes.inspect is a cluster capability; a Docker hello naming it is refused.
func TestCapabilitiesFitKubernetesInspect(t *testing.T) {
	if CapabilityKubernetesInspect != "kubernetes.inspect" {
		t.Fatal("the wire vocabulary changed")
	}
	if !CapabilitiesFit(RuntimeKubernetes, []string{CapabilityKubernetesInventory, CapabilityKubernetesInspect}) {
		t.Fatal("refused on a cluster")
	}
	if CapabilitiesFit(RuntimeDocker, []string{CapabilityContainerInspect, CapabilityKubernetesInspect}) {
		t.Fatal("fits a Docker host")
	}
	if CapabilitiesFit(RuntimeKubernetes, []string{CapabilityKubernetesInspect, CapabilityContainerInspectHealth}) {
		t.Fatal("a cluster named the Docker health capability")
	}
}
