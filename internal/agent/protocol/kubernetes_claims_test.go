package protocol

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// claimDeployment is goodKubernetesDeployment with web mounting claim shop-data.
func claimDeployment(now time.Time) DeploymentRequest {
	r := goodKubernetesDeployment(now)
	r.Kubernetes.Claims = []KubernetesClaim{{Name: "shop-data", StorageClass: "standard", Size: "10Gi", AccessMode: AccessReadWriteOnce}}
	r.Services[0].Volumes = []KubernetesMount{{Claim: "shop-data", MountPath: "/var/lib/data"}}
	return r
}

// A cluster frame's claims are each mounted by exactly one service at a clean path, sized in
// Mi, Gi or Ti up to 16Ti, ReadWriteOnce, in a named or the default StorageClass; a Docker frame
// carries no claim mount and a removal no claim.
func TestKubernetesClaimsValidation(t *testing.T) {
	now := time.Now()
	if err := claimDeployment(now).ValidateFor(RuntimeKubernetes, now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DeploymentRequest){
		"default class": func(r *DeploymentRequest) { r.Kubernetes.Claims[0].StorageClass = "" },
		"same claim, two paths": func(r *DeploymentRequest) {
			r.Services[0].Volumes = append(r.Services[0].Volumes, KubernetesMount{Claim: "shop-data", MountPath: "/backup", ReadOnly: true})
		},
		"16Ti": func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "16Ti" },
		"1Mi":  func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "1Mi" },
	} {
		r := claimDeployment(now)
		mutate(&r)
		if err := r.Validate(now); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*DeploymentRequest){
		"unmounted claim": func(r *DeploymentRequest) { r.Services[0].Volumes = nil },
		"unknown claim":   func(r *DeploymentRequest) { r.Services[0].Volumes[0].Claim = "shop-logs" },
		"two services": func(r *DeploymentRequest) {
			r.Services[1].Volumes = []KubernetesMount{{Claim: "shop-data", MountPath: "/data"}}
		},
		"duplicate claim": func(r *DeploymentRequest) { r.Kubernetes.Claims = append(r.Kubernetes.Claims, r.Kubernetes.Claims[0]) },
		"bad claim name": func(r *DeploymentRequest) {
			r.Kubernetes.Claims[0].Name = "Shop_Data"
			r.Services[0].Volumes[0].Claim = "Shop_Data"
		},
		"bad class":       func(r *DeploymentRequest) { r.Kubernetes.Claims[0].StorageClass = "Fast SSD" },
		"read write many": func(r *DeploymentRequest) { r.Kubernetes.Claims[0].AccessMode = "ReadWriteMany" },
		"no access mode":  func(r *DeploymentRequest) { r.Kubernetes.Claims[0].AccessMode = "" },
		"decimal size":    func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "1.5Gi" },
		"bare bytes":      func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "1073741824" },
		"SI suffix":       func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "10G" },
		"zero":            func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "0Gi" },
		"over 16Ti":       func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "17Ti" },
		"relative path":   func(r *DeploymentRequest) { r.Services[0].Volumes[0].MountPath = "data" },
		"root path":       func(r *DeploymentRequest) { r.Services[0].Volumes[0].MountPath = "/" },
		"unclean path":    func(r *DeploymentRequest) { r.Services[0].Volumes[0].MountPath = "/var/../data" },
		"path twice": func(r *DeploymentRequest) {
			r.Services[0].Volumes = append(r.Services[0].Volumes, r.Services[0].Volumes[0])
		},
		"too many mounts": func(r *DeploymentRequest) { r.Services[0].Volumes = manyMounts(MaxKubernetesMounts + 1) },
		"too many claims": func(r *DeploymentRequest) { r.Kubernetes.Claims = manyClaims(MaxKubernetesClaims + 1) },
		"docker mount beside": func(r *DeploymentRequest) {
			r.Services[0].Mounts = []Mount{{Kind: MountVolume, Source: "shop_data", Target: "/x"}}
		},
	} {
		r := claimDeployment(now)
		mutate(&r)
		if r.Validate(now) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	docker := goodDeployment(now)
	docker.Services[0].Volumes = []KubernetesMount{{Claim: "shop-data", MountPath: "/data"}}
	if docker.Validate(now) == nil {
		t.Error("a Docker frame carried a claim mount")
	}
	removal := RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(5 * time.Minute),
		Kubernetes: &KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testDigest, Claims: claimDeployment(now).Kubernetes.Claims}, Services: []string{"web"}}
	if removal.Validate(now) == nil {
		t.Error("a removal carried claims")
	}
}

func manyMounts(n int) []KubernetesMount {
	out := []KubernetesMount{}
	for i := range n {
		out = append(out, KubernetesMount{Claim: "shop-data", MountPath: "/m" + strings.Repeat("x", i+1)})
	}
	return out
}

func manyClaims(n int) []KubernetesClaim {
	out := []KubernetesClaim{}
	for i := range n {
		out = append(out, KubernetesClaim{Name: "c" + strings.Repeat("x", i+1), Size: "1Gi", AccessMode: AccessReadWriteOnce})
	}
	return out
}

func TestStorageSizeBytes(t *testing.T) {
	for in, want := range map[string]int64{"1Mi": 1 << 20, "512Mi": 512 << 20, "10Gi": 10 << 30, "16Ti": 16 << 40, "16384Gi": 16 << 40} {
		if got, ok := StorageSizeBytes(in); !ok || got != want {
			t.Errorf("%s: %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0Mi", "01Gi", "1Ki", "1G", "1gi", "16385Gi", "1000000Mi", "-1Gi", " 1Gi", "1Gi "} {
		if _, ok := StorageSizeBytes(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

// claim_immutable names the claim; a removal's kept claim is a skipped volume step with detail
// retained and nothing else carries that detail.
func TestClaimStepCodes(t *testing.T) {
	for _, tc := range []struct {
		step DeploymentStep
		ok   bool
	}{
		{DeploymentStep{Service: "db", Step: StepPrecondition, Outcome: OutcomeDenied, Code: "claim_immutable", Detail: "PersistentVolumeClaim/shop-data"}, true},
		{DeploymentStep{Service: "db", Step: StepPrecondition, Outcome: OutcomeDenied, Code: "claim_immutable", Detail: ""}, true},
		{DeploymentStep{Service: "db", Step: StepPrecondition, Outcome: OutcomeDenied, Code: "claim_immutable", Detail: "10Gi"}, false},
		{DeploymentStep{Service: "db", Step: StepPrecondition, Outcome: OutcomeDenied, Code: "name_taken", Detail: "PersistentVolumeClaim/shop-data"}, true},
		{DeploymentStep{Service: "db", Step: StepVolume, Outcome: OutcomeSkipped, Detail: DetailRetained}, true},
		{DeploymentStep{Service: "db", Step: StepRemove, Outcome: OutcomeSkipped, Detail: DetailRetained}, false},
		{DeploymentStep{Service: "db", Step: StepVolume, Outcome: OutcomeSucceeded, Detail: DetailRetained}, false},
		{DeploymentStep{Service: "db", Step: StepVolume, Outcome: OutcomeSkipped, Detail: "kept"}, false},
		{DeploymentStep{Service: "db", Step: StepVolume, Outcome: OutcomeSkipped, Code: "claim_immutable", Detail: DetailRetained}, false},
	} {
		outcome, code := OutcomeSucceeded, ""
		if tc.step.Outcome == OutcomeDenied {
			outcome, code = OutcomeDenied, ResultStepFailed
		}
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: outcome, Code: code, Services: []DeploymentIdentity{}, Steps: []DeploymentStep{tc.step}}
		if err := r.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.step, err)
		}
	}
}

// Storage classes decode bounded, are cleaned, never nil, and are cut first among nothing: a
// long list is named storage_classes.
func TestStorageClassesInventory(t *testing.T) {
	classes := make([]string, 0, MaxStorageClasses+5)
	for i := range MaxStorageClasses + 5 {
		classes = append(classes, `{"name":"c`+strings.Repeat("x", i%3)+`","default":`+map[bool]string{true: "true", false: "false"}[i == 0]+`}`)
	}
	var s Snapshot
	if err := UnmarshalSnapshotBounded([]byte(`{"generation":1,"kubernetes":{"storage_classes":[`+strings.Join(classes, ",")+`]}}`), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Kubernetes.StorageClasses) != MaxStorageClasses || !s.Kubernetes.StorageClasses[0].Default || !slices.Contains(s.Truncated, "storage_classes") {
		t.Fatalf("decoded %d %v", len(s.Kubernetes.StorageClasses), s.Truncated)
	}
	dirty := &Snapshot{Kubernetes: &KubernetesInventory{StorageClasses: []StorageClass{{Name: "fast\n‮ssd"}}}}
	Clamp(dirty)
	if dirty.Kubernetes.StorageClasses[0].Name != "fastssd" {
		t.Fatalf("clamp %q", dirty.Kubernetes.StorageClasses[0].Name)
	}
	empty := &Snapshot{Kubernetes: &KubernetesInventory{}}
	Clamp(empty)
	if empty.Kubernetes.StorageClasses == nil {
		t.Fatal("a nil storage class list")
	}
}
