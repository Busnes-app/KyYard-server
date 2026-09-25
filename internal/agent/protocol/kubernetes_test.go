package protocol

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Cluster lists decode one element at a time up to their caps, a pod's containers too, and a
// cut list is named; an absent or null inventory stays nil.
func TestKubernetesSnapshotDecodesBounded(t *testing.T) {
	pods := make([]string, 0, MaxPods+3)
	many := strings.TrimSuffix(strings.Repeat(`{"name":"c"},`, MaxPodContainers+5), ",")
	pods = append(pods, `{"namespace":"shop","name":"web","containers":[`+many+`]}`)
	for i := 1; i < MaxPods+3; i++ {
		pods = append(pods, fmt.Sprintf(`{"namespace":"shop","name":"p%d"}`, i))
	}
	raw := []byte(`{"generation":1,"kubernetes":{"nodes":[{"name":"n1","ready":true}],"pods":[` + strings.Join(pods, ",") + `]}}`)
	var s Snapshot
	if err := UnmarshalSnapshotBounded(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Kubernetes == nil || len(s.Kubernetes.Pods) != MaxPods || len(s.Kubernetes.Pods[0].Containers) != MaxPodContainers || !slices.Contains(s.Truncated, "pods") {
		t.Fatalf("decoded %+v truncated %v", s.Kubernetes != nil, s.Truncated)
	}
	if len(s.Kubernetes.Nodes) != 1 || !s.Kubernetes.Nodes[0].Ready || s.Kubernetes.Nodes[0].Name != "n1" {
		t.Fatalf("nodes %+v", s.Kubernetes.Nodes)
	}
	for _, doc := range []string{`{"generation":1}`, `{"generation":1,"kubernetes":null}`} {
		var docker Snapshot
		if err := UnmarshalSnapshotBounded([]byte(doc), &docker); err != nil || docker.Kubernetes != nil {
			t.Fatalf("%s: %+v %v", doc, docker.Kubernetes, err)
		}
	}
	var bad Snapshot
	if err := UnmarshalSnapshotBounded([]byte(`{"kubernetes":{"pods":{}}}`), &bad); err == nil {
		t.Fatal("a pods object where a list belongs decoded")
	}
}

// A pod whose containers were cut while decoding marks "pods" even when the agent named no
// cut and the pod list itself fits, and marks it once when the agent did.
func TestPodContainerCutMarksPods(t *testing.T) {
	many := strings.TrimSuffix(strings.Repeat(`{"name":"c"},`, 40), ",")
	pod := `{"namespace":"shop","name":"web","containers":[` + many + `]}`
	for doc, want := range map[string][]string{
		`{"generation":1,"kubernetes":{"pods":[` + pod + `]}}`:                      {"pods"},
		`{"generation":1,"truncated":["pods"],"kubernetes":{"pods":[` + pod + `]}}`: {"pods"},
	} {
		var s Snapshot
		if err := UnmarshalSnapshotBounded([]byte(doc), &s); err != nil {
			t.Fatal(err)
		}
		if len(s.Kubernetes.Pods[0].Containers) != MaxPodContainers || !slices.Equal(s.Truncated, want) {
			t.Fatalf("containers %d truncated %v", len(s.Kubernetes.Pods[0].Containers), s.Truncated)
		}
	}
}

// A cluster inventory many times the shared byte limit fits it after Clamp and Shrink, text is
// cleaned and cut, the adapter's own truncation names survive, and no list is left nil.
func TestClampAndShrinkKubernetes(t *testing.T) {
	image := strings.Repeat("i", MaxKubeImageBytes+100)
	k := &KubernetesInventory{}
	for i := 0; i < MaxPods; i++ {
		p := Pod{Namespace: "shop", Name: fmt.Sprintf("web-%d", i), Phase: "Running"}
		for range 4 {
			p.Containers = append(p.Containers, PodContainer{Name: "c", Image: image, ImageID: image, State: "running"})
		}
		k.Pods = append(k.Pods, p)
	}
	k.Workloads = []Workload{{Kind: "Deployment", Namespace: "shop", Name: "web‮evil\nline", Images: []string{image}}}
	s := &Snapshot{Kubernetes: k, Truncated: []string{"services"}}
	Clamp(s)
	if k.Workloads[0].Name != "webevilline" || len(k.Workloads[0].Images[0]) != MaxKubeImageBytes || k.Nodes == nil || k.Services == nil || k.Claims == nil || k.Namespaces == nil {
		t.Fatalf("clamp: %+v", k.Workloads[0])
	}
	if !slices.Contains(s.Truncated, "services") {
		t.Fatalf("clamp dropped the adapter's truncation: %v", s.Truncated)
	}
	raw := Shrink(s)
	if len(raw) > MaxSnapshotBytes || !slices.Contains(s.Truncated, "pods") || !slices.Contains(s.Truncated, "services") {
		t.Fatalf("shrunk to %d bytes, truncated %v", len(raw), s.Truncated)
	}
	var back Snapshot
	if err := UnmarshalSnapshotBounded(raw, &back); err != nil || back.Kubernetes == nil || len(back.Kubernetes.Pods) == 0 {
		t.Fatalf("round trip: %v", err)
	}
	// A pod with more containers than the cap is cut and marks the pod list incomplete.
	wide := &Snapshot{Kubernetes: &KubernetesInventory{Pods: []Pod{{Name: "p", Containers: make([]PodContainer, MaxPodContainers+1)}}}}
	Clamp(wide)
	if len(wide.Kubernetes.Pods[0].Containers) != MaxPodContainers || !slices.Contains(wide.Truncated, "pods") {
		t.Fatalf("wide pod: %d %v", len(wide.Kubernetes.Pods[0].Containers), wide.Truncated)
	}
}

func TestPodTargetValidation(t *testing.T) {
	for target, ok := range map[PodTarget]bool{
		{Namespace: "shop", Name: "web-7c9d-x2"}:                        true,
		{Namespace: "shop", Name: "web.v2", Container: "nginx"}:         true,
		{Namespace: "shop", Name: strings.Repeat("a", 253)}:             true,
		{Namespace: "shop", Name: strings.Repeat("a", 254)}:             false,
		{Namespace: strings.Repeat("a", 64), Name: "web"}:               false,
		{Namespace: "Shop", Name: "web"}:                                false,
		{Namespace: "shop", Name: "../secrets"}:                         false,
		{Namespace: "shop", Name: "web", Container: "a.b"}:              false,
		{Namespace: "", Name: "web"}:                                    false,
		{Namespace: "shop", Name: "web", Container: "-x"}:               false,
		{Namespace: "kube-system", Name: "coredns-1", Container: "dns"}: true,
	} {
		if (target.Validate() == nil) != ok {
			t.Errorf("%+v: want ok=%v", target, ok)
		}
	}
}

func TestLogRequestValidateFor(t *testing.T) {
	pod := &PodTarget{Namespace: "shop", Name: "web"}
	id := strings.Repeat("a", 64)
	for _, tc := range []struct {
		runtime string
		req     LogRequest
		ok      bool
	}{
		{RuntimeKubernetes, LogRequest{Pod: pod}, true},
		{RuntimeKubernetes, LogRequest{Pod: pod, Container: id}, false},
		{RuntimeKubernetes, LogRequest{Container: id}, false},
		{RuntimeKubernetes, LogRequest{Pod: &PodTarget{Namespace: "shop", Name: "Web"}}, false},
		{RuntimeDocker, LogRequest{Container: id}, true},
		{RuntimeDocker, LogRequest{Container: id, Pod: pod}, false},
		{RuntimeDocker, LogRequest{Pod: pod}, false},
	} {
		if (tc.req.ValidateFor(tc.runtime) == nil) != tc.ok {
			t.Errorf("%s %+v: want ok=%v", tc.runtime, tc.req, tc.ok)
		}
	}
	// A Docker request's wire form is unchanged: no pod key.
	raw, _ := json.Marshal(LogRequest{Stream: "s", Container: id})
	if strings.Contains(string(raw), "pod") {
		t.Fatalf("docker request grew a pod key: %s", raw)
	}
}

func TestCapabilitiesFitTheRuntime(t *testing.T) {
	for _, tc := range []struct {
		runtime string
		caps    []string
		ok      bool
	}{
		{RuntimeKubernetes, []string{CapabilityKubernetesInventory, CapabilityPodLogs}, true},
		{RuntimeKubernetes, []string{}, true},
		{RuntimeKubernetes, []string{CapabilityKubernetesInventory, CapabilityContainerInspect}, false},
		{RuntimeKubernetes, []string{"container.exec"}, false},
		{RuntimeKubernetes, []string{"kubernetes.write"}, false},
		{RuntimeDocker, []string{CapabilityContainerInspect, CapabilityDeploymentApply, "container.exec"}, true},
		{RuntimeDocker, []string{CapabilityPodLogs}, false},
		{RuntimeDocker, []string{CapabilityDeploymentApply, CapabilityKubernetesInventory}, false},
	} {
		if CapabilitiesFit(tc.runtime, tc.caps) != tc.ok {
			t.Errorf("%s %v: want %v", tc.runtime, tc.caps, tc.ok)
		}
	}
	if CapabilityKubernetesInventory != "kubernetes.inventory" || CapabilityPodLogs != "pod.logs" {
		t.Fatal("the wire vocabulary changed")
	}
}

func TestCheckRuntimeShape(t *testing.T) {
	cluster := &KubernetesInventory{}
	for _, tc := range []struct {
		runtime string
		s       Snapshot
		ok      bool
	}{
		{RuntimeKubernetes, Snapshot{Kubernetes: cluster, Containers: []Container{}, Images: []Image{}}, true},
		{RuntimeKubernetes, Snapshot{}, false},
		{RuntimeKubernetes, Snapshot{Kubernetes: cluster, Containers: []Container{{ID: "c"}}}, false},
		{RuntimeKubernetes, Snapshot{Kubernetes: cluster, Volumes: []Volume{{Name: "v"}}}, false},
		{RuntimeDocker, Snapshot{Containers: []Container{{ID: "c"}}}, true},
		{RuntimeDocker, Snapshot{Kubernetes: cluster}, false},
	} {
		if (CheckRuntimeShape(tc.runtime, &tc.s) == nil) != tc.ok {
			t.Errorf("%s %+v: want ok=%v", tc.runtime, tc.s, tc.ok)
		}
	}
}

func TestClusterHealth(t *testing.T) {
	ready, notReady := Node{Name: "a", Ready: true}, Node{Name: "b"}
	snap := func(truncated []string, nodes ...Node) *Snapshot {
		return &Snapshot{ObservedAt: time.Now(), Kubernetes: &KubernetesInventory{Nodes: nodes}, Truncated: truncated}
	}
	for _, tc := range []struct {
		name   string
		active bool
		s      *Snapshot
		want   string
	}{
		{"every node ready", true, snap(nil, ready, ready), HealthHealthy},
		{"one node not ready", true, snap(nil, ready, notReady), HealthDegraded},
		{"a not-ready node in a partial list", true, snap([]string{"nodes"}, notReady), HealthDegraded},
		{"a partial list of ready nodes", true, snap([]string{"nodes"}, ready), HealthUnknown},
		{"no node reported", true, snap([]string{"nodes"}), HealthUnknown},
		{"offline", false, snap(nil, ready), HealthUnknown},
		{"no inventory", true, nil, HealthUnknown},
		{"docker snapshot", true, &Snapshot{}, HealthUnknown},
	} {
		if got := ClusterHealth(tc.active, tc.s); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}
