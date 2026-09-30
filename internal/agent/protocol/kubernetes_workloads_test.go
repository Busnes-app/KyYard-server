package protocol

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseWorkloadRef(t *testing.T) {
	r, err := ParseWorkloadRef("shop/deployment/web")
	if err != nil || r != (WorkloadRef{Namespace: "shop", Kind: WorkloadDeployment, Name: "web"}) || r.String() != "shop/deployment/web" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, kind := range []string{"statefulset", "daemonset", "pod"} {
		if _, err := ParseWorkloadRef("shop/" + kind + "/web-0"); err != nil {
			t.Error(kind, err)
		}
	}
	for _, bad := range []string{"Shop/Deployment/web", "shop/Deployment/web", "shop/web", "../x", "shop/deployment/../x", "shop/deployment/web/x", "shop/replicaset/web", "/deployment/web", "shop/deployment/", "shop/deployment/web.x", "shop/deployment/" + strings.Repeat("a", 64)} {
		if _, err := ParseWorkloadRef(bad); err == nil {
			t.Error("accepted", bad)
		}
	}
}

// The inspection shape (UID, no kind) keeps its wire form; the object shape carries a kind.
func TestWorkloadRefShapes(t *testing.T) {
	raw, _ := json.Marshal(workloadTarget().Workload)
	if string(raw) != `{"namespace":"shop","name":"shop-web","uid":"`+testWorkloadUID+`"}` {
		t.Fatalf("inspection ref wire changed: %s", raw)
	}
	object := InspectionTarget{Workload: WorkloadRef{Namespace: "shop", Kind: WorkloadStatefulSet, Name: "db"}}
	if err := object.ValidateFor(RuntimeKubernetes); err != nil {
		t.Fatal(err)
	}
	grant := ConfigurationOpen{Request: "r", Endpoint: "e", Actor: "a", Connection: make([]byte, 32), Expires: time.Now().Add(10 * time.Second), Target: object}
	if err := grant.ValidateWithin(time.Now(), ConfigurationLifetime, RuntimeKubernetes); err != nil {
		t.Fatal(err)
	}
	if grant.ValidateWithin(time.Now(), ConfigurationLifetime, RuntimeDocker) == nil {
		t.Fatal("workload grant accepted on docker")
	}
	for name, ref := range map[string]WorkloadRef{
		"kind and uid":   {Namespace: "shop", Kind: WorkloadDeployment, Name: "db", UID: testWorkloadUID},
		"unknown kind":   {Namespace: "shop", Kind: "job", Name: "db"},
		"no kind no uid": {Namespace: "shop", Name: "db"},
	} {
		if (InspectionTarget{Workload: ref}).ValidateFor(RuntimeKubernetes) == nil {
			t.Error("accepted", name)
		}
	}
	// An inspection answer is only for the inspection shape.
	a := workloadAnswer()
	a.Target = object
	if a.Validate(object, time.Now(), false) == nil {
		t.Fatal("inspection answer accepted for an object ref")
	}
}

func validWorkloadConfiguration() WorkloadConfiguration {
	two := int32(2)
	return WorkloadConfiguration{
		Target: WorkloadRef{Namespace: "shop", Kind: WorkloadDeployment, Name: "web"}, ObservedAt: time.Now(), ResourceVersion: "12345",
		Replicas: &two, Strategy: "RollingUpdate",
		Containers: []WorkloadContainer{{
			Name: "web", Image: "nginx:1.27@sha256:" + strings.Repeat("a", 64), ImageID: "docker.io/library/nginx@sha256:" + strings.Repeat("a", 64),
			Command: []string{"/bin/sh"}, Args: []string{"-c", "exec nginx\n"},
			Env:       []WorkloadEnv{{Name: "MODE", Value: "a=b\nc"}, {Name: "TOKEN", SecretRef: "shop-secrets/token"}, {Name: "LEVEL", ConfigMapRef: "shop-config/log.level"}},
			Resources: WorkloadResources{CPURequest: "250m", CPULimit: "1", MemoryRequest: "1.5Gi", MemoryLimit: "2Gi"},
		}},
		InitContainers: []WorkloadContainer{{Name: "migrate", Image: "shop/migrate:1"}},
		EnvFrom:        []string{"secret/shop-secrets", "configmap/shop-config"},
		Unsupported:    []string{},
	}
}

func TestWorkloadConfigurationValid(t *testing.T) {
	c := validWorkloadConfiguration()
	if err := c.Validate(c.Target, time.Now()); err != nil {
		t.Fatal(err)
	}
	ds := validWorkloadConfiguration()
	ds.Target.Kind, ds.Replicas, ds.Strategy = WorkloadDaemonSet, nil, "OnDelete"
	if err := ds.Validate(ds.Target, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkloadConfigurationRefusals(t *testing.T) {
	over := int32(MaxWorkloadReplicas + 1)
	negative := int32(-1)
	for name, change := range map[string]func(*WorkloadConfiguration){
		"pod target":                  func(c *WorkloadConfiguration) { c.Target.Kind = WorkloadPod },
		"stale":                       func(c *WorkloadConfiguration) { c.ObservedAt = time.Now().Add(-time.Hour) },
		"no resource version":         func(c *WorkloadConfiguration) { c.ResourceVersion = "" },
		"replicas over bound":         func(c *WorkloadConfiguration) { c.Replicas = &over },
		"negative replicas":           func(c *WorkloadConfiguration) { c.Replicas = &negative },
		"deployment without replicas": func(c *WorkloadConfiguration) { c.Replicas = nil },
		"daemonset with replicas":     func(c *WorkloadConfiguration) { c.Target.Kind = WorkloadDaemonSet; c.Strategy = "" },
		"paused statefulset":          func(c *WorkloadConfiguration) { c.Target.Kind = WorkloadStatefulSet; c.Paused = true },
		"bad strategy":                func(c *WorkloadConfiguration) { c.Strategy = "Blue" },
		"no containers":               func(c *WorkloadConfiguration) { c.Containers = nil },
		"too many containers": func(c *WorkloadConfiguration) {
			for i := range MaxWorkloadContainers {
				c.Containers = append(c.Containers, WorkloadContainer{Name: "c" + string(rune('a'+i)), Image: "nginx"})
			}
		},
		"duplicate container":  func(c *WorkloadConfiguration) { c.Containers = append(c.Containers, c.Containers[0]) },
		"container name":       func(c *WorkloadConfiguration) { c.Containers[0].Name = "Web" },
		"image":                func(c *WorkloadConfiguration) { c.Containers[0].Image = "../etc" },
		"empty image":          func(c *WorkloadConfiguration) { c.Containers[0].Image = "" },
		"long argv":            func(c *WorkloadConfiguration) { c.Containers[0].Args = make([]string, MaxArgv+1) },
		"NUL arg":              func(c *WorkloadConfiguration) { c.Containers[0].Command = []string{"a\x00"} },
		"value and secret":     func(c *WorkloadConfiguration) { c.Containers[0].Env[1].Value = "leak" },
		"secret and configmap": func(c *WorkloadConfiguration) { c.Containers[0].Env[1].ConfigMapRef = "shop-config/x" },
		"ref without key":      func(c *WorkloadConfiguration) { c.Containers[0].Env[1].SecretRef = "shop-secrets" },
		"ref traversal":        func(c *WorkloadConfiguration) { c.Containers[0].Env[1].SecretRef = "../x/y" },
		"env name":             func(c *WorkloadConfiguration) { c.Containers[0].Env[0].Name = "A=B" },
		"duplicate env":        func(c *WorkloadConfiguration) { c.Containers[0].Env[1].Name = "MODE" },
		"too much env": func(c *WorkloadConfiguration) {
			for i := range MaxWorkloadEnv {
				c.Containers[0].Env = append(c.Containers[0].Env, WorkloadEnv{Name: "E" + strconv.Itoa(i)})
			}
		},
		"quantity garbage": func(c *WorkloadConfiguration) { c.Containers[0].Resources.MemoryLimit = "banana" },
		"quantity too long": func(c *WorkloadConfiguration) {
			c.Containers[0].Resources.CPULimit = strings.Repeat("1", MaxQuantityBytes+1)
		},
		"init container env": func(c *WorkloadConfiguration) {
			c.InitContainers[0].Env = []WorkloadEnv{{Name: "X", Value: "v", SecretRef: "a/b"}}
		},
		"env from kind":       func(c *WorkloadConfiguration) { c.EnvFrom = []string{"volume/x"} },
		"env from duplicate":  func(c *WorkloadConfiguration) { c.EnvFrom = []string{"secret/a", "secret/a"} },
		"unknown unsupported": func(c *WorkloadConfiguration) { c.Unsupported = []string{"privileged"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := validWorkloadConfiguration()
			change(&c)
			if c.Validate(c.Target, time.Now()) == nil {
				t.Fatal("accepted")
			}
		})
	}
	c := validWorkloadConfiguration()
	if c.Validate(WorkloadRef{Namespace: "shop", Kind: WorkloadDeployment, Name: "api"}, time.Now()) == nil {
		t.Fatal("accepted another target")
	}
}

func TestWorkloadQuantities(t *testing.T) {
	for _, q := range []string{"", "1.5Gi", "250m", "1", "128974848", "129e6", "123Mi", ".5"} {
		if !validQuantity(q) {
			t.Error("refused", q)
		}
	}
	for _, q := range []string{"banana", "1.5 Gi", "-1", "1Gb", "Gi"} {
		if validQuantity(q) {
			t.Error("accepted", q)
		}
	}
}

func TestWorkloadUnsupportedCodes(t *testing.T) {
	c := validWorkloadConfiguration()
	c.Unsupported = []string{"env_field_ref", "resources_extended"}
	if err := c.Validate(c.Target, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func validWorkloadApply(now time.Time) WorkloadApply {
	spec := validWorkloadConfiguration()
	spec.ObservedAt = time.Time{}
	return WorkloadApply{Request: "0f1e2d3c-4b5a-4968-8776-655443322111", Endpoint: "ep1", IssuedAt: now, Deadline: now.Add(5 * time.Minute), Target: spec.Target, ResourceVersion: spec.ResourceVersion, Spec: spec}
}

func TestWorkloadApplyBounds(t *testing.T) {
	now := time.Now()
	a := validWorkloadApply(now)
	if err := a.Validate(now); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*WorkloadApply){
		"request not a uuid": func(a *WorkloadApply) { a.Request = "r1" },
		"endpoint":           func(a *WorkloadApply) { a.Endpoint = "../e" },
		"no issue time":      func(a *WorkloadApply) { a.IssuedAt = time.Time{} },
		"clock skew": func(a *WorkloadApply) {
			a.IssuedAt = now.Add(-time.Hour)
			a.Deadline = now.Add(-time.Hour + time.Minute)
		},
		"past deadline":          func(a *WorkloadApply) { a.IssuedAt = now.Add(-2 * time.Minute); a.Deadline = now.Add(-time.Minute) },
		"overlong deadline":      func(a *WorkloadApply) { a.Deadline = now.Add(DeploymentLifetime + time.Minute) },
		"pod target":             func(a *WorkloadApply) { a.Target.Kind = WorkloadPod; a.Spec.Target.Kind = WorkloadPod },
		"target mismatch":        func(a *WorkloadApply) { a.Spec.Target.Name = "api" },
		"resource version":       func(a *WorkloadApply) { a.ResourceVersion = "" },
		"resource version split": func(a *WorkloadApply) { a.Spec.ResourceVersion = "1" },
		"observed spec":          func(a *WorkloadApply) { a.Spec.ObservedAt = now },
		"managed spec":           func(a *WorkloadApply) { a.Spec.Managed = true },
		"unsupported spec":       func(a *WorkloadApply) { a.Spec.Unsupported = []string{"env_field_ref"} },
		"invalid spec":           func(a *WorkloadApply) { a.Spec.Containers[0].Resources.CPULimit = "banana" },
		"oversize": func(a *WorkloadApply) {
			for i := range 12 {
				a.Spec.Containers = append(a.Spec.Containers, WorkloadContainer{Name: "c" + string(rune('a'+i)), Image: "nginx", Env: []WorkloadEnv{{Name: "BIG", Value: strings.Repeat("x", MaxDeploymentEnvValueBytes)}}})
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := validWorkloadApply(now)
			change(&a)
			if a.Validate(now) == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// An apply is answered by a DeploymentResult for the one service "workload".
func TestWorkloadApplyResultSteps(t *testing.T) {
	r := DeploymentResult{Deployment: validWorkloadApply(time.Now()).Request, Outcome: OutcomeFailed, Code: ResultStepFailed, Steps: []DeploymentStep{
		{Service: WorkloadApplyService, Step: StepPrecondition, Outcome: OutcomeSucceeded},
		{Service: WorkloadApplyService, Step: StepApply, Outcome: OutcomeSucceeded},
		{Service: WorkloadApplyService, Step: StepRollout, Outcome: OutcomeTimedOut, Code: "rollout_timeout", Detail: "progressing=ProgressDeadlineExceeded"},
	}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"forbidden", "conflict", "application_managed", "namespace_not_granted", "pvc_retention"} {
		r.Steps = []DeploymentStep{{Service: WorkloadApplyService, Step: StepPrecondition, Outcome: OutcomeDenied, Code: code}}
		if err := r.Validate(); err != nil {
			t.Error(code, err)
		}
	}
}

func TestWorkloadConfigurationResult(t *testing.T) {
	c, w := validConfiguration(), validWorkloadConfiguration()
	if err := (ConfigurationResult{Request: "r1", Status: "ok", Workload: &w}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (ConfigurationResult{Request: "r1", Status: "ok", Result: &c, Workload: &w}).Validate() == nil {
		t.Fatal("both results accepted")
	}
	if (ConfigurationResult{Request: "r1", Status: "busy", Workload: &w}).Validate() == nil {
		t.Fatal("workload on a refusal accepted")
	}
	raw, _ := json.Marshal(ConfigurationResult{Request: "r1", Status: "busy"})
	if strings.Contains(string(raw), "workload") {
		t.Fatalf("docker result grew a workload key: %s", raw)
	}
}

func TestExpectationReplicasIsAdditive(t *testing.T) {
	raw, _ := json.Marshal(Expectation{ImageDigest: "sha256:" + strings.Repeat("a", 64), State: "running"})
	if string(raw) != `{"image_digest":"sha256:`+strings.Repeat("a", 64)+`","state":"running"}` {
		t.Fatalf("expectation wire changed: %s", raw)
	}
	if raw, _ := json.Marshal(Expectation{}); string(raw) != `{}` {
		t.Fatalf("empty expectation wire changed: %s", raw)
	}
	three := int32(3)
	raw, _ = json.Marshal(Expectation{Replicas: &three})
	var back Expectation
	if string(raw) != `{"replicas":3}` || json.Unmarshal(raw, &back) != nil || back.Replicas == nil || *back.Replicas != 3 {
		t.Fatalf("replicas round trip: %s", raw)
	}
	zero := int32(0)
	if raw, _ := json.Marshal(Expectation{Replicas: &zero}); string(raw) != `{"replicas":0}` {
		t.Fatalf("scale to zero lost: %s", raw)
	}
}

func TestWorkloadVocabulary(t *testing.T) {
	if ActionWorkloadRestart != "workload.restart" || ActionWorkloadScale != "workload.scale" || ActionWorkloadDelete != "workload.delete" || ActionPodDelete != "pod.delete" ||
		CapabilityKubernetesWorkloads != "kubernetes.workloads" || CapabilityPodExec != "pod.exec" || TypeWorkloadApply != "workload.apply" || MaxWorkloadApplyBytes != 128<<10 {
		t.Fatal("the wire vocabulary changed")
	}
	if !CapabilitiesFit(RuntimeKubernetes, []string{CapabilityKubernetesWorkloads, CapabilityPodExec}) || CapabilitiesFit(RuntimeDocker, []string{CapabilityPodExec}) {
		t.Fatal("workload capabilities are cluster capabilities")
	}
}

func TestPodUIDIsAdditive(t *testing.T) {
	raw, _ := json.Marshal(Pod{Namespace: "shop", Name: "web"})
	if strings.Contains(string(raw), "uid") {
		t.Fatalf("pod without a uid grew a key: %s", raw)
	}
	var p Pod
	if json.Unmarshal([]byte(`{"namespace":"shop","name":"web","uid":"`+testPodUID+`"}`), &p) != nil || p.UID != testPodUID {
		t.Fatalf("uid not decoded: %+v", p)
	}
}
