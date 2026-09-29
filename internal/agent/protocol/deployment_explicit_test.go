package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// deploymentWire is goodDeployment's exact JSON at a fixed time. Frames without Explicit must
// stay byte-identical so an older agent reads what a newer server sends.
const deploymentWire = `{"deployment":"3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b","request_id":"0123456789abcdef0123456789abcdef","endpoint":"ep_1","project":"shop","revision":2,"issued_at":"2026-01-02T03:04:05Z","deadline":"2026-01-02T03:09:05Z","services":[{"name":"web","container_name":"shop-web-1","image_id":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","replaces":{"container_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","image_id":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","created_unix":1700000000},"restart":"always","ports":[{"host_ip":"127.0.0.1","host":8080,"container":80,"protocol":"tcp"}],"env":{"TOKEN":"x"},"mounts":[]}]}`

func TestDeploymentRequestWireUnchanged(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	b, err := json.Marshal(goodDeployment(now))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != deploymentWire || strings.Contains(string(b), "explicit") {
		t.Fatalf("wire changed:\n%s", b)
	}
}

func explicitRequest(now time.Time) DeploymentRequest {
	r := goodDeployment(now)
	r.Explicit, r.Project, r.Revision = true, ExplicitProject, ExplicitRevision
	r.Services[0].Explicit = &ExplicitService{}
	return r
}

func TestExplicitDeploymentRequest(t *testing.T) {
	now := time.Now()
	if err := explicitRequest(now).Validate(now); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	run := explicitRequest(now)
	run.Services[0].Replaces = InspectionTarget{}
	if err := run.Validate(now); err != nil {
		t.Fatalf("run: %v", err)
	}
	full := explicitRequest(now)
	timeout := 30
	full.Services[0].Explicit = &ExplicitService{Command: []string{"sh", "-c", "a\nb"}, User: "1000:1000", Labels: map[string]string{"a": "b"}, NetworkMode: "bridge",
		Networks: []NetworkAttachmentSpec{{Name: "n", IP: "10.0.0.2"}}, Healthcheck: &Healthcheck{Test: []string{"CMD", "true"}}, Devices: []Device{{Host: "/dev/x", Container: "/dev/x", Permissions: "rw"}},
		Log: LogConfig{Driver: "json-file", Options: map[string]string{"max-size": "1m"}}, StopTimeout: &timeout, AcknowledgedBinds: []string{"/srv/data"}}
	full.Services[0].Env = map[string]string{"my-var.x": "1"}
	if err := full.Validate(now); err != nil {
		t.Fatalf("full: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*DeploymentRequest)
		ok     bool
	}{
		"port host 0 explicit": {func(r *DeploymentRequest) { r.Services[0].Ports = []Port{{Container: 80, Protocol: "tcp"}} }, true},
		"port host 0 non-explicit": {func(r *DeploymentRequest) {
			*r = goodDeployment(now)
			r.Services[0].Ports = []Port{{Container: 80, Protocol: "tcp"}}
		}, false},
		"exposed twice": {func(r *DeploymentRequest) {
			r.Services[0].Ports = []Port{{Container: 80, Protocol: "tcp"}, {Container: 80, Protocol: "tcp"}}
		}, false},
		"exposed with host ip": {func(r *DeploymentRequest) {
			r.Services[0].Ports = []Port{{Container: 80, Protocol: "tcp", HostIP: "127.0.0.1"}}
		}, false},
		"tmpfs explicit": {func(r *DeploymentRequest) { r.Services[0].Mounts = []Mount{{Kind: MountTmpfs, Target: "/run"}} }, true},
		"tmpfs read-only": {func(r *DeploymentRequest) {
			r.Services[0].Mounts = []Mount{{Kind: MountTmpfs, Target: "/run", ReadOnly: true}}
		}, true},
		"tmpfs with source": {func(r *DeploymentRequest) {
			r.Services[0].Mounts = []Mount{{Kind: MountTmpfs, Source: "/x", Target: "/run"}}
		}, false},
		"tmpfs non-explicit": {func(r *DeploymentRequest) {
			*r = goodDeployment(now)
			r.Services[0].Mounts = []Mount{{Kind: MountTmpfs, Target: "/run"}}
		}, false},
		"explicit data, flag off": {func(r *DeploymentRequest) { r.Explicit = false }, false},
		"flag on, no data":        {func(r *DeploymentRequest) { r.Services[0].Explicit = nil }, false},
		"two services":            {func(r *DeploymentRequest) { withServices(r, 2); r.Services[1].Explicit = &ExplicitService{} }, false},
		"wrong project":           {func(r *DeploymentRequest) { r.Project = "shop" }, false},
		"wrong revision":          {func(r *DeploymentRequest) { r.Revision = 2 }, false},
		"run without container name": {func(r *DeploymentRequest) {
			r.Services[0].Replaces = InspectionTarget{}
			r.Services[0].ContainerName = ""
		}, false},
		"partial replaces":     {func(r *DeploymentRequest) { r.Services[0].Replaces.ImageID = "" }, false},
		"cluster":              {func(r *DeploymentRequest) { r.Kubernetes = &KubernetesTarget{} }, false},
		"env name docker rule": {func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"a b": "1"} }, true},
		"env name with equals": {func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"a=b": "1"} }, false},
		"argv too long":        {func(r *DeploymentRequest) { r.Services[0].Explicit.Command = make([]string, MaxArgv+1) }, false},
		"too many networks": {func(r *DeploymentRequest) {
			r.Services[0].Explicit.Networks = make([]NetworkAttachmentSpec, MaxConfigurationNetworks+1)
		}, false},
		"host network as an attachment": {func(r *DeploymentRequest) {
			r.Services[0].Explicit.Networks = []NetworkAttachmentSpec{{Name: "host"}}
		}, false},
		"container network as an attachment": {func(r *DeploymentRequest) {
			r.Services[0].Explicit.Networks = []NetworkAttachmentSpec{{Name: "container:db"}}
		}, false},
		"bad network ip": {func(r *DeploymentRequest) {
			r.Services[0].Explicit.Networks = []NetworkAttachmentSpec{{Name: "n", IP: "x"}}
		}, false},
		"relative acknowledged bind":  {func(r *DeploymentRequest) { r.Services[0].Explicit.AcknowledgedBinds = []string{"srv/data"} }, false},
		"unclean acknowledged bind":   {func(r *DeploymentRequest) { r.Services[0].Explicit.AcknowledgedBinds = []string{"/srv/../data"} }, false},
		"too many acknowledged binds": {func(r *DeploymentRequest) { r.Services[0].Explicit.AcknowledgedBinds = manyBinds(MaxMounts + 1) }, false},
		"retries with always":         {func(r *DeploymentRequest) { r.Services[0].Restart, r.Services[0].Explicit.RestartRetries = "always", 2 }, false},
		"retries with on-failure": {func(r *DeploymentRequest) {
			r.Services[0].Restart, r.Services[0].Explicit.RestartRetries = "on-failure", 2
		}, true},
		"negative retries": {func(r *DeploymentRequest) { r.Services[0].Explicit.RestartRetries = -1 }, false},
		"bad device permissions": {func(r *DeploymentRequest) {
			r.Services[0].Explicit.Devices = []Device{{Host: "/d", Container: "/d", Permissions: "x"}}
		}, false},
		"too many log options":         {func(r *DeploymentRequest) { r.Services[0].Explicit.Log.Options = manyOptions(MaxLogOptions + 1) }, false},
		"negative memory":              {func(r *DeploymentRequest) { r.Services[0].Explicit.Resources.MemoryBytes = -1 }, false},
		"label with control character": {func(r *DeploymentRequest) { r.Services[0].Explicit.Labels = map[string]string{"a": "b\x07"} }, false},
		"stop timeout out of range":    {func(r *DeploymentRequest) { n := 4000; r.Services[0].Explicit.StopTimeout = &n }, false},
	} {
		r := explicitRequest(now)
		tc.mutate(&r)
		if err := r.Validate(now); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func manyBinds(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "/srv/" + strings.Repeat("a", i%5+1) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	return out
}

func manyOptions(n int) map[string]string {
	out := map[string]string{}
	for i := 0; i < n; i++ {
		out[strings.Repeat("k", i+1)] = "v"
	}
	return out
}

func TestRollbackStepCodes(t *testing.T) {
	r := goodResult()
	r.Outcome, r.Code = OutcomeFailed, ResultStepFailed
	r.Steps = []DeploymentStep{
		{Service: "web", Step: StepStart, Outcome: OutcomeFailed, Code: "start_failed_rolled_back"},
		{Service: "web", Step: StepRollback, Outcome: OutcomeSucceeded},
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Steps = []DeploymentStep{{Service: "web", Step: StepRollback, Outcome: OutcomeFailed, Code: "rollback_failed"}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Steps[0].Detail = "x"
	if r.Validate() == nil {
		t.Fatal("rollback_failed takes no detail")
	}
}
