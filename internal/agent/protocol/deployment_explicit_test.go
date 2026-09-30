package protocol

import (
	"encoding/json"
	"reflect"
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
		want   string // the refusal's text; "" accepts
	}{
		"port host 0 explicit": {func(r *DeploymentRequest) { r.Services[0].Ports = []Port{{Container: 80, Protocol: "tcp"}} }, ""},
		"port host 0 non-explicit": {func(r *DeploymentRequest) {
			*r = goodDeployment(now)
			r.Services[0].Ports = []Port{{Container: 80, Protocol: "tcp"}}
		}, "invalid port"},
		"exposed twice": {func(r *DeploymentRequest) {
			r.Services[0].Ports = []Port{{Container: 80, Protocol: "tcp"}, {Container: 80, Protocol: "tcp"}}
		}, "invalid port"},
		"exposed with host ip": {func(r *DeploymentRequest) {
			r.Services[0].Ports = []Port{{Container: 80, Protocol: "tcp", HostIP: "127.0.0.1"}}
		}, "invalid port"},
		"tmpfs explicit": {func(r *DeploymentRequest) { r.Services[0].Mounts = []Mount{{Kind: MountTmpfs, Target: "/run"}} }, ""},
		"tmpfs read-only": {func(r *DeploymentRequest) {
			r.Services[0].Mounts = []Mount{{Kind: MountTmpfs, Target: "/run", ReadOnly: true}}
		}, ""},
		"tmpfs with source": {func(r *DeploymentRequest) {
			r.Services[0].Mounts = []Mount{{Kind: MountTmpfs, Source: "/x", Target: "/run"}}
		}, "invalid mount"},
		"tmpfs non-explicit": {func(r *DeploymentRequest) {
			*r = goodDeployment(now)
			r.Services[0].Mounts = []Mount{{Kind: MountTmpfs, Target: "/run"}}
		}, "invalid mount"},
		"explicit data, flag off": {func(r *DeploymentRequest) { r.Explicit = false }, "invalid deployment service"},
		"flag on, no data":        {func(r *DeploymentRequest) { r.Services[0].Explicit = nil }, "invalid deployment service"},
		"two services":            {func(r *DeploymentRequest) { withServices(r, 2); r.Services[1].Explicit = &ExplicitService{} }, "invalid explicit request"},
		"wrong project":           {func(r *DeploymentRequest) { r.Project = "shop" }, "invalid explicit request"},
		"wrong revision":          {func(r *DeploymentRequest) { r.Revision = 2 }, "invalid explicit request"},
		"run without container name": {func(r *DeploymentRequest) {
			r.Services[0].Replaces = InspectionTarget{}
			r.Services[0].ContainerName = ""
		}, "invalid deployment service"},
		"partial replaces":     {func(r *DeploymentRequest) { r.Services[0].Replaces.ImageID = "" }, "invalid deployment service"},
		"cluster":              {func(r *DeploymentRequest) { r.Kubernetes = &KubernetesTarget{} }, "explicit request for a cluster"},
		"env name docker rule": {func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"a b": "1"} }, ""},
		"env name with equals": {func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"a=b": "1"} }, "invalid environment value"},
		"argv too long":        {func(r *DeploymentRequest) { r.Services[0].Explicit.Command = make([]string, MaxArgv+1) }, "invalid deployment service"},
		"too many networks": {func(r *DeploymentRequest) {
			r.Services[0].Explicit.Networks = make([]NetworkAttachmentSpec, MaxConfigurationNetworks+1)
		}, "invalid deployment service"},
		"host network as an attachment": {func(r *DeploymentRequest) {
			r.Services[0].Explicit.Networks = []NetworkAttachmentSpec{{Name: "host"}}
		}, "invalid deployment service"},
		"container network as an attachment": {func(r *DeploymentRequest) {
			r.Services[0].Explicit.Networks = []NetworkAttachmentSpec{{Name: "container:db"}}
		}, "invalid deployment service"},
		"bad network ip": {func(r *DeploymentRequest) {
			r.Services[0].Explicit.Networks = []NetworkAttachmentSpec{{Name: "n", IP: "x"}}
		}, "invalid deployment service"},
		"relative acknowledged bind":  {func(r *DeploymentRequest) { r.Services[0].Explicit.AcknowledgedBinds = []string{"srv/data"} }, "invalid deployment service"},
		"unclean acknowledged bind":   {func(r *DeploymentRequest) { r.Services[0].Explicit.AcknowledgedBinds = []string{"/srv/../data"} }, "invalid deployment service"},
		"too many acknowledged binds": {func(r *DeploymentRequest) { r.Services[0].Explicit.AcknowledgedBinds = manyBinds(MaxMounts + 1) }, "invalid deployment service"},
		"retries with always":         {func(r *DeploymentRequest) { r.Services[0].Restart, r.Services[0].Explicit.RestartRetries = "always", 2 }, "invalid deployment service"},
		"retries with on-failure": {func(r *DeploymentRequest) {
			r.Services[0].Restart, r.Services[0].Explicit.RestartRetries = "on-failure", 2
		}, ""},
		"negative retries": {func(r *DeploymentRequest) { r.Services[0].Explicit.RestartRetries = -1 }, "invalid deployment service"},
		"bad device permissions": {func(r *DeploymentRequest) {
			r.Services[0].Explicit.Devices = []Device{{Host: "/d", Container: "/d", Permissions: "x"}}
		}, "invalid deployment service"},
		"too many log options":         {func(r *DeploymentRequest) { r.Services[0].Explicit.Log.Options = manyOptions(MaxLogOptions + 1) }, "invalid deployment service"},
		"negative memory":              {func(r *DeploymentRequest) { r.Services[0].Explicit.Resources.MemoryBytes = -1 }, "invalid deployment service"},
		"label with control character": {func(r *DeploymentRequest) { r.Services[0].Explicit.Labels = map[string]string{"a": "b\x07"} }, "invalid deployment service"},
		"stop timeout out of range":    {func(r *DeploymentRequest) { n := 4000; r.Services[0].Explicit.StopTimeout = &n }, "invalid deployment service"},
	} {
		r := explicitRequest(now)
		tc.mutate(&r)
		if err := r.Validate(now); (err == nil) != (tc.want == "") || err != nil && err.Error() != tc.want {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// A populated explicit service survives the wire unchanged.
func TestExplicitServiceRoundTrip(t *testing.T) {
	timeout := 30
	in := ExplicitService{Command: []string{"sh", "-c", "a"}, Entrypoint: []string{"/init"}, User: "1000:1000", WorkingDir: "/srv", Hostname: "web",
		Labels: map[string]string{"a": "b"}, NetworkMode: "bridge", Networks: []NetworkAttachmentSpec{{Name: "n", Aliases: []string{"web"}, IP: "10.0.0.2"}},
		Resources:   Resources{NanoCPUs: 5e8, MemoryBytes: 1 << 28, MemorySwapBytes: 1 << 29, PidsLimit: 100},
		Healthcheck: &Healthcheck{Test: []string{"CMD", "true"}, IntervalSeconds: 5, TimeoutSeconds: 1, StartPeriodSeconds: 2, Retries: 3},
		Privileged:  true, ReadOnlyRootfs: true, Init: true, TTY: true, StdinOpen: true,
		CapAdd: []string{"NET_ADMIN"}, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, ExtraHosts: []string{"db:10.0.0.3"}, DNS: []string{"1.1.1.1"},
		Devices: []Device{{Host: "/dev/x", Container: "/dev/x", Permissions: "rw"}}, Log: LogConfig{Driver: "json-file", Options: map[string]string{"max-size": "1m"}},
		StopSignal: "SIGTERM", StopTimeout: &timeout, RestartRetries: 2, AcknowledgedBinds: []string{"/srv/data"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ExplicitService
	if err := json.Unmarshal(b, &out); err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip: %v\n%+v\n%+v", err, in, out)
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

// An explicit precondition names an Engine key it cannot carry the way the configuration read
// does, host_config:<Key>, beside the known codes.
func TestUnsupportedStepNamesEngineKeys(t *testing.T) {
	for detail, ok := range map[string]bool{
		"resource_limits,dns,host_config:UTSMode": true,
		"host_config:OomKillDisable":              true,
		"host_config:Oom-Kill":                    false,
		"host_config:UTSMode,host_config:UTSMode": false,
		"list_truncated:ports":                    false,
		"bogus":                                   false,
	} {
		r := goodResult()
		r.Outcome, r.Code = OutcomeDenied, ResultStepFailed
		r.Steps = []DeploymentStep{{Service: "web", Step: StepPrecondition, Outcome: OutcomeDenied, Code: "unsupported", Detail: detail}}
		if err := r.Validate(); (err == nil) != ok {
			t.Errorf("%s: %v", detail, err)
		}
	}
}
