package protocol

import (
	"strings"
	"testing"
	"time"
)

const (
	testDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testApp      = "11111111-2222-4333-8444-555555555555"
	testInstance = "66666666-7777-4888-9999-aaaaaaaaaaaa"
)

func goodKubernetesDeployment(now time.Time) DeploymentRequest {
	return DeploymentRequest{
		Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", Revision: 2, IssuedAt: now, Deadline: now.Add(5 * time.Minute),
		Kubernetes: &KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testDigest},
		Services: []DeploymentService{
			{Name: "web", Restart: "always", Pull: &ImagePull{Reference: "ghcr.io/org/web@" + testDigest, Digest: testDigest}, Ports: []Port{{Container: 80, Host: 8080, Protocol: "tcp"}}, Env: map[string]string{"MODE": "prod", "TOKEN": "x"}, SecretKeys: []string{"TOKEN"}, Mounts: []Mount{}},
			{Name: "api", Pull: &ImagePull{Reference: "ghcr.io/org/api@" + testDigest, Digest: testDigest}, Ports: []Port{{Container: 80, Host: 8080, Protocol: "tcp"}}, Env: map[string]string{}, Mounts: []Mount{}},
		},
	}
}

// A cluster frame carries its target and every service pulled by digest, with nothing a Docker
// host needs; a published port may repeat across services, each of which gets its own Service.
func TestKubernetesDeploymentRequestValidation(t *testing.T) {
	now := time.Now()
	if err := goodKubernetesDeployment(now).Validate(now); err != nil {
		t.Fatal(err)
	}
	if err := goodKubernetesDeployment(now).ValidateFor(RuntimeKubernetes, now); err != nil {
		t.Fatal(err)
	}
	if goodKubernetesDeployment(now).ValidateFor(RuntimeDocker, now) == nil || goodDeployment(now).ValidateFor(RuntimeKubernetes, now) == nil {
		t.Fatal("a frame crossed runtimes")
	}
	if err := goodDeployment(now).ValidateFor(RuntimeDocker, now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DeploymentRequest){
		"bad namespace":        func(r *DeploymentRequest) { r.Kubernetes.Namespace = "Shop" },
		"bad application":      func(r *DeploymentRequest) { r.Kubernetes.ApplicationID = "app" },
		"bad instance":         func(r *DeploymentRequest) { r.Kubernetes.InstanceID = "" },
		"bad spec digest":      func(r *DeploymentRequest) { r.Kubernetes.SpecDigest = "sha256:abc" },
		"container name":       func(r *DeploymentRequest) { r.Services[0].ContainerName = "shop-web" },
		"local image":          func(r *DeploymentRequest) { r.Services[0].ImageID = testDigest },
		"no pull":              func(r *DeploymentRequest) { r.Services[0].Pull = nil },
		"tag moved":            func(r *DeploymentRequest) { r.Services[0].Pull.Tag = "ghcr.io/org/web:1" },
		"replaces a container": func(r *DeploymentRequest) { r.Services[0].Replaces.ContainerID = strings.Repeat("b", 64) },
		"mounts": func(r *DeploymentRequest) {
			r.Services[0].Mounts = []Mount{{Kind: MountVolume, Source: "shop_data", Target: "/data"}}
		},
		"volumes":              func(r *DeploymentRequest) { r.Volumes = []string{"shop_data"} },
		"registry credential":  func(r *DeploymentRequest) { r.Registries = map[string]RegistryAuth{"ghcr.io": {Secret: "s"}} },
		"on-failure restart":   func(r *DeploymentRequest) { r.Services[0].Restart = "on-failure" },
		"no restart":           func(r *DeploymentRequest) { r.Services[0].Restart = "no" },
		"host address":         func(r *DeploymentRequest) { r.Services[0].Ports[0].HostIP = "127.0.0.1" },
		"port twice":           func(r *DeploymentRequest) { r.Services[0].Ports = append(r.Services[0].Ports, r.Services[0].Ports[0]) },
		"unknown secret key":   func(r *DeploymentRequest) { r.Services[0].SecretKeys = []string{"PASSWORD"} },
		"unsorted secret keys": func(r *DeploymentRequest) { r.Services[0].SecretKeys = []string{"TOKEN", "MODE"} },
		"repeated secret key":  func(r *DeploymentRequest) { r.Services[0].SecretKeys = []string{"TOKEN", "TOKEN"} },
		"duplicate service":    func(r *DeploymentRequest) { r.Services[1].Name = "web" },
		"env nul":              func(r *DeploymentRequest) { r.Services[0].Env["MODE"] = "a\x00b" },
	} {
		r := goodKubernetesDeployment(now)
		mutate(&r)
		if r.Validate(now) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	docker := goodDeployment(now)
	docker.Services[0].SecretKeys = []string{"TOKEN"}
	if docker.Validate(now) == nil {
		t.Error("a Docker frame carried secret keys")
	}
}

func TestKubernetesRemovalRequestValidation(t *testing.T) {
	now := time.Now()
	good := func() RemovalRequest {
		return RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(5 * time.Minute),
			Kubernetes: &KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testDigest}, Services: []string{"web", "api"}}
	}
	if err := good().ValidateFor(RuntimeKubernetes, now); err != nil {
		t.Fatal(err)
	}
	if good().ValidateFor(RuntimeDocker, now) == nil || goodRemoval(now).ValidateFor(RuntimeKubernetes, now) == nil {
		t.Fatal("a removal crossed runtimes")
	}
	for name, mutate := range map[string]func(*RemovalRequest){
		"no services":       func(r *RemovalRequest) { r.Services = nil },
		"bad service":       func(r *RemovalRequest) { r.Services[0] = "Web" },
		"repeated service":  func(r *RemovalRequest) { r.Services[1] = "web" },
		"containers":        func(r *RemovalRequest) { r.Containers = goodRemoval(now).Containers },
		"bad target":        func(r *RemovalRequest) { r.Kubernetes.Namespace = "" },
		"too many services": func(r *RemovalRequest) { r.Services = make([]string, MaxRemovalTargets+1) },
	} {
		r := good()
		mutate(&r)
		if r.Validate(now) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	docker := goodRemoval(now)
	docker.Services = []string{"web"}
	if docker.Validate(now) == nil {
		t.Error("a Docker removal named services")
	}
}

// Names are DNS-1123 labels that start with a letter, cut with a digest suffix when too long
// or shared, and distinct for every service of a definition.
func TestKubernetesNames(t *testing.T) {
	long := strings.Repeat("a", 64)
	got := KubernetesNames("Shop.Front", []string{"web", "my_api", "db-", "a-b", "a_b"})
	if got["web"] != "shop-front-web" || got["my_api"] != "shop-front-my-api" || got["db-"] != "shop-front-db" {
		t.Fatalf("names %v", got)
	}
	if got["a-b"] == got["a_b"] || !strings.HasPrefix(got["a-b"], "shop-front-a-b-") || len(got["a-b"]) != len("shop-front-a-b-")+6 {
		t.Fatalf("colliding slugs %q %q", got["a-b"], got["a_b"])
	}
	if n := KubernetesNames("1shop", []string{"web"})["web"]; n != "ky-1shop-web" {
		t.Fatalf("digit start %q", n)
	}
	cut := KubernetesNames(long, []string{"web", "api"})
	for _, n := range cut {
		if len(n) > MaxKubeObjectName || !ValidDNSLabel(n) || n[0] < 'a' || n[0] > 'z' {
			t.Fatalf("cut name %q", n)
		}
	}
	if cut["web"] == cut["api"] {
		t.Fatal("cut names collide")
	}
	if KubernetesNames(long, []string{"web"})["web"] != cut["web"] {
		t.Fatal("a name depends on its siblings when it does not collide")
	}
	if !ValidServiceName("my_api") || ValidServiceName("Web") || ValidServiceName("") {
		t.Fatal("service name grammar")
	}
	for _, v := range []string{"shop", "a.b_c-d", ""} {
		if !ValidLabelValue(v) {
			t.Errorf("%q refused", v)
		}
	}
	for _, v := range []string{"db-", "_db", long, "a b"} {
		if ValidLabelValue(v) {
			t.Errorf("%q accepted", v)
		}
	}
}

// A Deployment identity carries its namespace, name, UID, generation and pulled digest and no
// container field; a Docker identity carries none of the cluster fields.
func TestKubernetesIdentities(t *testing.T) {
	uid := "0f1e2d3c-4b5a-4968-8776-655443322110"
	good := DeploymentIdentity{Service: "web", Kind: KindDeployment, Namespace: "shop", Name: "shop-web", UID: uid, Generation: 3, ImageDigest: testDigest}
	result := func(ids ...DeploymentIdentity) DeploymentResult {
		return DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: OutcomeSucceeded, Steps: []DeploymentStep{}, Services: ids}
	}
	if err := result(good).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DeploymentIdentity){
		"other kind":     func(i *DeploymentIdentity) { i.Kind = "StatefulSet" },
		"bad namespace":  func(i *DeploymentIdentity) { i.Namespace = "Shop" },
		"bad name":       func(i *DeploymentIdentity) { i.Name = "shop_web" },
		"bad uid":        func(i *DeploymentIdentity) { i.UID = "x" },
		"no generation":  func(i *DeploymentIdentity) { i.Generation = 0 },
		"no digest":      func(i *DeploymentIdentity) { i.ImageDigest = "" },
		"container id":   func(i *DeploymentIdentity) { i.ContainerID = strings.Repeat("a", 64) },
		"docker created": func(i *DeploymentIdentity) { i.CreatedUnix = 1 },
	} {
		id := good
		mutate(&id)
		if result(id).Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	docker := DeploymentIdentity{Service: "web", ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	if err := result(docker).Validate(); err != nil {
		t.Fatal(err)
	}
	docker.Namespace = "shop"
	if result(docker).Validate() == nil {
		t.Error("a Docker identity carried a namespace")
	}
}

// The cluster step codes carry only their closed details.
func TestKubernetesStepCodes(t *testing.T) {
	for _, tc := range []struct {
		code, detail string
		ok           bool
	}{
		{"forbidden", "", true},
		{"forbidden", "create deployments", false},
		{"name_taken", "", true},
		{"name_taken", "Deployment/shop-web", true},
		{"name_taken", "Secret/shop-web-secret", true},
		{"name_taken", "Pod/shop-web", false},
		{"name_taken", "Deployment/Shop Web", false},
		{"conflict", "ConfigMap/shop-web-env", true},
		{"rollout_timeout", "", true},
		{"rollout_timeout", "progressing=ProgressDeadlineExceeded,available=MinimumReplicasUnavailable,pod=ImagePullBackOff", true},
		{"rollout_timeout", "pod=CrashLoopBackOff", true},
		{"rollout_timeout", "pod=back-off pulling image", false},
		{"rollout_timeout", "node=NotReady", false},
		{"unsupported", "k8s_volume,k8s_restart", true},
	} {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: OutcomeFailed, Code: ResultStepFailed, Services: []DeploymentIdentity{},
			Steps: []DeploymentStep{{Service: "web", Step: StepStart, Outcome: OutcomeFailed, Code: tc.code, Detail: tc.detail}}}
		if err := r.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s %q: %v", tc.code, tc.detail, err)
		}
	}
}

// A cluster agent may advertise deploy and remove; deployment.pull and deployment.apply stay
// Docker capabilities, and a Docker agent cannot claim the cluster ones.
func TestCapabilitiesFitClusterDeployment(t *testing.T) {
	if !CapabilitiesFit(RuntimeKubernetes, []string{CapabilityKubernetesInventory, CapabilityPodLogs, CapabilityKubernetesDeploy, CapabilityKubernetesRemove}) {
		t.Fatal("cluster deployment capabilities refused")
	}
	for _, c := range []string{CapabilityDeploymentPull, CapabilityDeploymentApply, CapabilityDeploymentRemove} {
		if CapabilitiesFit(RuntimeKubernetes, []string{CapabilityKubernetesInventory, c}) {
			t.Errorf("%s fits a cluster", c)
		}
	}
	for _, c := range []string{CapabilityKubernetesDeploy, CapabilityKubernetesRemove} {
		if CapabilitiesFit(RuntimeDocker, []string{c}) {
			t.Errorf("%s fits a Docker host", c)
		}
	}
}

// A workload's KyYard labels are cut like every short field.
func TestWorkloadLabelsClamp(t *testing.T) {
	s := &Snapshot{Kubernetes: &KubernetesInventory{Workloads: []Workload{{Kind: "Deployment", Namespace: "shop", Name: "shop-web", Application: strings.Repeat("a", 100), Instance: "i\nx"}}}}
	Clamp(s)
	w := s.Kubernetes.Workloads[0]
	if len(w.Application) != MaxKubeShortBytes || w.Instance != "ix" {
		t.Fatalf("labels %q %q", w.Application, w.Instance)
	}
}
