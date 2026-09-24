package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func goodDeployment(now time.Time) DeploymentRequest {
	return DeploymentRequest{
		Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", Revision: 2, IssuedAt: now, Deadline: now.Add(5 * time.Minute),
		Services: []DeploymentService{{
			Name: "web", ContainerName: "shop-web-1", ImageID: "sha256:" + strings.Repeat("a", 64),
			Replaces: InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000},
			Restart:  "always", Ports: []Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}}, Env: map[string]string{"TOKEN": "x"}, Mounts: []Mount{},
		}},
	}
}

func TestDeploymentRequestValidation(t *testing.T) {
	now := time.Now()
	if err := goodDeployment(now).Validate(now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DeploymentRequest){
		"bad uuid":          func(r *DeploymentRequest) { r.Deployment = "nope" },
		"bad endpoint":      func(r *DeploymentRequest) { r.Endpoint = "a b" },
		"bad project":       func(r *DeploymentRequest) { r.Project = "-shop" },
		"revision zero":     func(r *DeploymentRequest) { r.Revision = 0 },
		"revision 101":      func(r *DeploymentRequest) { r.Revision = 101 },
		"deadline past":     func(r *DeploymentRequest) { r.Deadline = now.Add(-time.Second) },
		"deadline far":      func(r *DeploymentRequest) { r.Deadline = now.Add(DeploymentLifetime + time.Second) },
		"no services":       func(r *DeploymentRequest) { r.Services = nil },
		"bad service name":  func(r *DeploymentRequest) { r.Services[0].Name = "Web" },
		"bad container":     func(r *DeploymentRequest) { r.Services[0].ContainerName = "a/b" },
		"bad image":         func(r *DeploymentRequest) { r.Services[0].ImageID = strings.Repeat("a", 64) },
		"bad replaces":      func(r *DeploymentRequest) { r.Services[0].Replaces.CreatedUnix = 0 },
		"no request id":     func(r *DeploymentRequest) { r.RequestID = "" },
		"bad request id":    func(r *DeploymentRequest) { r.RequestID = "a b" },
		"long request id":   func(r *DeploymentRequest) { r.RequestID = strings.Repeat("a", 65) },
		"mounts key absent": func(r *DeploymentRequest) { r.Services[0].Mounts = nil },
		"bad restart":       func(r *DeploymentRequest) { r.Services[0].Restart = "forever" },
		"port zero":         func(r *DeploymentRequest) { r.Services[0].Ports[0].Host = 0 },
		"port proto":        func(r *DeploymentRequest) { r.Services[0].Ports[0].Protocol = "sctp" },
		"port ip":           func(r *DeploymentRequest) { r.Services[0].Ports[0].HostIP = "lo" },
		"env name":          func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"1X": "v"} },
		"env nul":           func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"X": "a\x00b"} },
		"env value size": func(r *DeploymentRequest) {
			r.Services[0].Env = map[string]string{"X": strings.Repeat("v", MaxDeploymentEnvValueBytes+1)}
		},
		"env total size": func(r *DeploymentRequest) {
			r.Services[0].Env = map[string]string{"A": strings.Repeat("v", MaxDeploymentEnvValueBytes), "B": strings.Repeat("v", MaxDeploymentEnvValueBytes), "C": strings.Repeat("v", MaxDeploymentEnvValueBytes), "D": strings.Repeat("v", MaxDeploymentEnvValueBytes), "E": "v"}
		},
		"duplicate service": func(r *DeploymentRequest) { r.Services = append(r.Services, r.Services[0]) },
		"duplicate replace": func(r *DeploymentRequest) {
			s := r.Services[0]
			s.Name = "db"
			s.ContainerName = "shop-db-1"
			r.Services = append(r.Services, s)
		},
		"duplicate port in service": func(r *DeploymentRequest) {
			p := r.Services[0].Ports[0]
			p.Container = 81
			r.Services[0].Ports = append(r.Services[0].Ports, p)
		},
		"duplicate wildcard port": func(r *DeploymentRequest) {
			r.Services[0].Ports = []Port{{Container: 80, Host: 8080, Protocol: "tcp"}, {Container: 81, Host: 8080, Protocol: "tcp", HostIP: "0.0.0.0"}}
		},
		"duplicate v6 wildcard port": func(r *DeploymentRequest) {
			r.Services[0].Ports = []Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "::"}, {Container: 81, Host: 8080, Protocol: "tcp", HostIP: "::0"}}
		},
		"duplicate port across services": func(r *DeploymentRequest) {
			s := r.Services[0]
			s.Name, s.ContainerName, s.Replaces.ContainerID = "db", "shop-db-1", strings.Repeat("d", 64)
			s.Ports = []Port{{Container: 5432, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}}
			r.Services = append(r.Services, s)
		},
		"duplicate name": func(r *DeploymentRequest) {
			s := r.Services[0]
			s.Name = "db"
			s.Replaces.ContainerID = strings.Repeat("d", 64)
			r.Services = append(r.Services, s)
		},
	} {
		r := goodDeployment(now)
		mutate(&r)
		if r.Validate(now) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	r := goodDeployment(now)
	r.Services[0].Ports = []Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "0.0.0.0"}, {Container: 80, Host: 8080, Protocol: "tcp", HostIP: "::"}, {Container: 80, Host: 8080, Protocol: "udp"}}
	if err := r.Validate(now); err != nil {
		t.Fatalf("IPv4 and IPv6 wildcard on one port refused: %v", err)
	}
	r = goodDeployment(now)
	r.Services[0].Restart, r.Services[0].Ports, r.Services[0].Env = "", nil, nil
	if err := r.Validate(now); err != nil {
		t.Fatalf("minimal service refused: %v", err)
	}
}

// pulled turns service i into one the agent pulls from host, pinned to a digest.
func pulled(r *DeploymentRequest, i int, host string) {
	d := "sha256:" + strings.Repeat("e", 64)
	r.Services[i].ImageID = ""
	r.Services[i].Pull = &ImagePull{Reference: host + "/acme/web@" + d, Digest: d}
}

// withServices grows r to n services, each with its own name, container and replaced identity.
func withServices(r *DeploymentRequest, n int) {
	base := r.Services[0]
	for i := len(r.Services); i < n; i++ {
		s := base
		s.Name, s.ContainerName, s.Replaces.ContainerID, s.Ports = fmt.Sprintf("s%d", i), fmt.Sprintf("shop-s%d-1", i), fmt.Sprintf("%064x", i), nil
		r.Services = append(r.Services, s)
	}
}

func TestDeploymentRequestPull(t *testing.T) {
	now := time.Now()
	for name, mutate := range map[string]func(*DeploymentRequest){
		"pull":           func(r *DeploymentRequest) { pulled(r, 0, "ghcr.io") },
		"localhost host": func(r *DeploymentRequest) { pulled(r, 0, "localhost") },
		"host with port": func(r *DeploymentRequest) { pulled(r, 0, "registry.lan:5000") },
		"port, no dot":   func(r *DeploymentRequest) { pulled(r, 0, "registry:5000") },
		"pull and tag": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Tag = "ghcr.io/acme/web:1.2"
		},
		"pull and auth": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Registries = map[string]RegistryAuth{"ghcr.io": {Username: "bot", Secret: "token"}}
		},
		"secret at cap": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Registries = map[string]RegistryAuth{"ghcr.io": {Username: strings.Repeat("u", 255), Secret: strings.Repeat("s", MaxRegistryAuthSecretBytes)}}
		},
		"hosts at cap": func(r *DeploymentRequest) {
			withServices(r, MaxRegistryAuthHosts)
			r.Registries = map[string]RegistryAuth{}
			for i := range r.Services {
				host := fmt.Sprintf("r%d.example", i)
				pulled(r, i, host)
				r.Registries[host] = RegistryAuth{Secret: "token"}
			}
		},
	} {
		r := goodDeployment(now)
		mutate(&r)
		if err := r.Validate(now); err != nil {
			t.Fatalf("%s refused: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*DeploymentRequest){
		"pull with image id": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].ImageID = "sha256:" + strings.Repeat("a", 64)
		},
		"no pull no image id": func(r *DeploymentRequest) { r.Services[0].ImageID = "" },
		"pull by tag": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Reference = "ghcr.io/acme/web:1.2"
		},
		// Digest agrees with the pin, so only the sha256: rule refuses it.
		"pull pinned by tag": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Reference, r.Services[0].Pull.Digest = "ghcr.io/acme/web:1.2", "1.2"
		},
		"tag with digest": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Tag = r.Services[0].Pull.Reference
		},
		"tag without tag": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Tag = "ghcr.io/acme/web"
		},
		"tag invalid": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Tag = "ghcr.io/../web:1.2"
		},
		"tag other repository": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Tag = "ghcr.io/acme/api:1.2"
		},
		"tag other host": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Tag = "quay.io/acme/web:1.2"
		},
		"pull without host": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Reference = strings.TrimPrefix(r.Services[0].Pull.Reference, "ghcr.io/")
		},
		"pull single component": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Reference = "nginx@" + r.Services[0].Pull.Digest
		},
		"pull digest mismatch": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Digest = "sha256:" + strings.Repeat("f", 64)
		},
		"pull bad reference": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Services[0].Pull.Reference = "ghcr.io/../web@" + r.Services[0].Pull.Digest
		},
		"auth for unpulled host": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Registries = map[string]RegistryAuth{"quay.io": {Secret: "token"}}
		},
		"auth without pull": func(r *DeploymentRequest) {
			r.Registries = map[string]RegistryAuth{"ghcr.io": {Secret: "token"}}
		},
		"too many hosts": func(r *DeploymentRequest) {
			withServices(r, MaxRegistryAuthHosts+1)
			r.Registries = map[string]RegistryAuth{}
			for i := range r.Services {
				host := fmt.Sprintf("r%d.example", i)
				pulled(r, i, host)
				r.Registries[host] = RegistryAuth{Secret: "token"}
			}
		},
		"empty secret": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Registries = map[string]RegistryAuth{"ghcr.io": {Username: "bot"}}
		},
		"secret over cap": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Registries = map[string]RegistryAuth{"ghcr.io": {Secret: strings.Repeat("s", MaxRegistryAuthSecretBytes+1)}}
		},
		"secret nul": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Registries = map[string]RegistryAuth{"ghcr.io": {Secret: "a\x00b"}}
		},
		"secret not utf-8": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Registries = map[string]RegistryAuth{"ghcr.io": {Secret: "a\xffb"}}
		},
		"username over cap": func(r *DeploymentRequest) {
			pulled(r, 0, "ghcr.io")
			r.Registries = map[string]RegistryAuth{"ghcr.io": {Username: strings.Repeat("u", 256), Secret: "token"}}
		},
	} {
		r := goodDeployment(now)
		mutate(&r)
		if r.Validate(now) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestImagePullHost(t *testing.T) {
	d := "@sha256:" + strings.Repeat("e", 64)
	for ref, want := range map[string]string{
		"ghcr.io/acme/web" + d:              "ghcr.io",
		"registry.lan:5000/web" + d:         "registry.lan:5000",
		"registry:5000/app" + d:             "registry:5000",
		"localhost/web" + d:                 "localhost",
		"acme/web" + d:                      "",
		"nginx" + d:                         "",
		"ghcr.io" + d:                       "",
		"ghcr.io/../web" + d:                "",
		"sha256:" + strings.Repeat("e", 64): "",
	} {
		if got := (ImagePull{Reference: ref}).Host(); got != want {
			t.Errorf("Host(%q) = %q, want %q", ref, got, want)
		}
	}
}

// The caps are wire bounds agents already built against; a change must be deliberate.
func TestDeploymentCaps(t *testing.T) {
	if MaxDeploymentRequestBytes != 327680 || MaxDeploymentRequestBytesLegacy != 196608 || MaxDeploymentResultBytes != 163840 || MaxRegistryAuthHosts != 16 || MaxRegistryAuthSecretBytes != 4096 {
		t.Fatal("a deployment wire cap changed")
	}
}

func goodRemoval(now time.Time) RemovalRequest {
	return RemovalRequest{
		Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(5 * time.Minute),
		Containers: []RemovalTarget{
			{Service: "web", Target: InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}},
			{Service: "unmapped-0123456789ab", Target: InspectionTarget{ContainerID: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("d", 64), CreatedUnix: 1700000000}},
		},
	}
}

func TestRemovalRequestValidation(t *testing.T) {
	now := time.Now()
	if err := goodRemoval(now).Validate(now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RemovalRequest){
		"bad uuid":      func(r *RemovalRequest) { r.Deployment = "nope" },
		"bad endpoint":  func(r *RemovalRequest) { r.Endpoint = "a b" },
		"bad project":   func(r *RemovalRequest) { r.Project = "-shop" },
		"deadline past": func(r *RemovalRequest) { r.Deadline = now.Add(-time.Second) },
		"deadline far":  func(r *RemovalRequest) { r.Deadline = now.Add(DeploymentLifetime + time.Second) },
		"no containers": func(r *RemovalRequest) { r.Containers = nil },
		"bad service":   func(r *RemovalRequest) { r.Containers[0].Service = "Web" },
		"bad target":    func(r *RemovalRequest) { r.Containers[0].Target.CreatedUnix = 0 },
		"no request id": func(r *RemovalRequest) { r.RequestID = "" },
		"duplicate service": func(r *RemovalRequest) {
			r.Containers[1].Service = r.Containers[0].Service
		},
		"duplicate container": func(r *RemovalRequest) {
			r.Containers[1].Target.ContainerID = r.Containers[0].Target.ContainerID
		},
		"too many containers": func(r *RemovalRequest) {
			for i := len(r.Containers); i <= MaxRemovalTargets; i++ {
				r.Containers = append(r.Containers, RemovalTarget{Service: fmt.Sprintf("s%d", i), Target: InspectionTarget{ContainerID: fmt.Sprintf("%064x", i), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}})
			}
		},
	} {
		r := goodRemoval(now)
		mutate(&r)
		if r.Validate(now) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// The cap is inclusive: MaxRemovalTargets targets pass, one more is refused.
	r := goodRemoval(now)
	for i := len(r.Containers); i < MaxRemovalTargets; i++ {
		r.Containers = append(r.Containers, RemovalTarget{Service: fmt.Sprintf("s%d", i), Target: InspectionTarget{ContainerID: fmt.Sprintf("%064x", i), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}})
	}
	if err := r.Validate(now); err != nil {
		t.Fatalf("%d targets: %v", len(r.Containers), err)
	}
	r.Containers = append(r.Containers, RemovalTarget{Service: "extra", Target: InspectionTarget{ContainerID: strings.Repeat("e", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}})
	if len(r.Containers) != MaxRemovalTargets+1 || r.Validate(now) == nil {
		t.Fatalf("%d targets accepted", len(r.Containers))
	}
}

func goodResult() DeploymentResult {
	return DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Outcome: OutcomeSucceeded, Steps: []DeploymentStep{{Service: "web", Step: StepCreate, Outcome: OutcomeSucceeded}}, Services: []DeploymentIdentity{{Service: "web", ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}}}
}

func TestDeploymentResultValidation(t *testing.T) {
	good := goodResult()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	failing := func(r *DeploymentResult, outcome string) {
		r.Outcome, r.Code = outcome, ResultStepFailed
		r.Steps[0].Outcome = outcome
		r.Services = []DeploymentIdentity{}
	}
	for name, mutate := range map[string]func(*DeploymentResult){
		"outcome":          func(r *DeploymentResult) { r.Outcome = "done" },
		"step name":        func(r *DeploymentResult) { r.Steps[0].Step = "fetch" },
		"image digest":     func(r *DeploymentResult) { r.Services[0].ImageDigest = "sha256:" + strings.Repeat("B", 64) },
		"image digest tag": func(r *DeploymentResult) { r.Services[0].ImageDigest = "latest" },
		"step outcome":     func(r *DeploymentResult) { r.Steps[0].Outcome = "ok" },
		"step detail over the bound": func(r *DeploymentResult) {
			failing(r, OutcomeFailed)
			r.Steps[0].Code, r.Steps[0].Detail = "unsupported", strings.Repeat("d", MaxDeploymentStepDetailBytes+1)
		},
		"detail on succeeded step":   func(r *DeploymentResult) { r.Steps[0].Detail = "done" },
		"code on succeeded step":     func(r *DeploymentResult) { r.Steps[0].Code = "runtime_error" },
		"code on skipped step":       func(r *DeploymentResult) { r.Steps[0].Outcome, r.Steps[0].Code = OutcomeSkipped, "runtime_error" },
		"code on succeeded result":   func(r *DeploymentResult) { r.Code = ResultStepFailed },
		"failed result without code": func(r *DeploymentResult) { r.Outcome = OutcomeFailed },
		"unknown result code":        func(r *DeploymentResult) { r.Outcome, r.Code = OutcomeFailed, "exploded" },
		"denied step without code":   func(r *DeploymentResult) { failing(r, OutcomeDenied) },
		"failed step without code":   func(r *DeploymentResult) { failing(r, OutcomeFailed) },
		"sentence as a step code": func(r *DeploymentResult) {
			failing(r, OutcomeDenied)
			r.Steps[0].Code = "the container no longer exists"
		},
		"free result detail": func(r *DeploymentResult) {
			r.Outcome, r.Code, r.Detail = OutcomeFailed, ResultStepFailed, "service web, step create: failed"
		},
		"bad request id": func(r *DeploymentResult) { r.RequestID = "a b" },
		"step service":   func(r *DeploymentResult) { r.Steps[0].Service = "Web" },
		"identity":       func(r *DeploymentResult) { r.Services[0].ImageID = "latest" },
		"too many steps": func(r *DeploymentResult) { r.Steps = make([]DeploymentStep, MaxDeploymentResultSteps+1) },
	} {
		r := good
		r.Steps = append([]DeploymentStep{}, good.Steps...)
		r.Services = append([]DeploymentIdentity{}, good.Services...)
		mutate(&r)
		if r.Validate() == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	for _, step := range []string{StepImage, StepPull} {
		r := good
		r.Steps = []DeploymentStep{{Service: "web", Step: step, Outcome: OutcomeSucceeded}}
		r.Services = []DeploymentIdentity{good.Services[0]}
		r.Services[0].ImageDigest = "sha256:" + strings.Repeat("d", 64)
		if err := r.Validate(); err != nil {
			t.Fatalf("%s step with a pulled identity refused: %v", step, err)
		}
	}
	recheck := good
	recheck.Outcome, recheck.Code = OutcomeDenied, ResultStepFailed
	recheck.Steps = []DeploymentStep{{Service: "web", Step: StepRecheck, Outcome: OutcomeDenied, Code: "configuration_drift"}}
	recheck.Services = []DeploymentIdentity{}
	if err := recheck.Validate(); err != nil {
		t.Fatalf("recheck step refused: %v", err)
	}
	skipped := good
	skipped.Steps = []DeploymentStep{{Service: "web", Step: StepStop, Outcome: OutcomeSkipped}}
	if err := skipped.Validate(); err != nil {
		t.Fatalf("skipped step refused: %v", err)
	}
	// An older agent sends no request_id; a succeeded result needs nothing else.
	older := good
	older.RequestID = ""
	if err := older.Validate(); err != nil {
		t.Fatalf("a result without request_id refused: %v", err)
	}
}

// Every step code takes exactly the parameter its row names; anything else is refused and never
// stored (Review Focus 1).
func TestDeploymentStepCodeDetails(t *testing.T) {
	id := strings.Repeat("c", 64)
	for _, tc := range []struct {
		code, detail string
		ok           bool
	}{
		{"runtime_unreadable", "", true},
		{"container_missing", "", true},
		{"identity_mismatch", "", true},
		{"image_identity_mismatch", "", true},
		{"configuration_unreported", "", true},
		{"unsupported", "privileged", true},
		{"bind_missing", "", true},
		{"volume_mount_missing", "", true},
		{"volume_not_owned", "", true},
		{"volume_missing", "", true},
		{"volume_create_failed", "", true},
		{"image_missing", "", true},
		{"pinned_image_missing", "", true},
		{"configuration_drift", "", true},
		{"name_reserved", "", true},
		{"name_taken", "", true},
		{"identity_unusable", "", true},
		{"identity_unreadable", id, true},
		{"identity_unverified", id, true},
		{"dependents", "", true},
		{"deadline", "", true},
		{"pull_failed", "", true},
		{"pull_unauthorized", "", true},
		{"pull_not_found", "", true},
		{"pull_digest_mismatch", "", true},
		{"cancelled", "", true},
		{"runtime_timeout", "", true},
		{"runtime_error", "", true},
		{"runtime_status", "500", true},
		{CodeLegacy, "", true},
		{"unsupported", "privileged,devices", true},
		{"runtime_status", "404", true},
		{"container_missing", "the container no longer exists", false},
		{"runtime_error", "the runtime call failed", false},
		{"legacy", "old text", false},
		{"unsupported", "", false},
		{"unsupported", "privileged,privileged", false},
		{"unsupported", "privileged, devices", false},
		{"unsupported", "made_up", false},
		{"unsupported", "privileged,", false},
		{"unsupported", strings.Join(UnsupportedCodes, ","), false}, // 311 bytes: past the step bound
		{"identity_unreadable", "", false},
		{"identity_unverified", strings.Repeat("C", 64), false},
		{"identity_unverified", id[:63], false},
		{"runtime_status", "abc", false},
		{"runtime_status", "600", false},
		{"runtime_status", "50", false},
		{"runtime_status", "", false},
		{"", "", false},
	} {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Outcome: OutcomeDenied, Code: ResultStepFailed,
			Steps: []DeploymentStep{{Service: "web", Step: StepPrecondition, Outcome: OutcomeDenied, Code: tc.code, Detail: tc.detail}}, Services: []DeploymentIdentity{}}
		if err := r.Validate(); (err == nil) != tc.ok {
			t.Errorf("code %q detail %q: %v", tc.code, tc.detail, err)
		}
	}
	if len(stepCodes) != 30 || len(resultCodes) != 8 {
		t.Fatalf("closed sets: %d step codes, %d result codes", len(stepCodes), len(resultCodes))
	}
}

// A result that did not succeed names one closed code. legacy is in the set, but reading a result
// as an older agent's is the server's job: the protocol never fills a missing code, with or
// without a request_id (Review Focus 5).
func TestDeploymentResultCodes(t *testing.T) {
	for _, code := range []string{ResultStepFailed, ResultClockSkew, ResultInvalidRequest, ResultWrongEndpoint, ResultBusy, ResultRestarted, ResultUnreadable, CodeLegacy} {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: OutcomeDenied, Code: code, Steps: []DeploymentStep{}, Services: []DeploymentIdentity{}}
		if err := r.Validate(); err != nil {
			t.Fatalf("%s: %v", code, err)
		}
	}
	if ResultStepFailed != "step_failed" || ResultClockSkew != "clock_skew" || ResultInvalidRequest != "invalid_request" || ResultWrongEndpoint != "wrong_endpoint" || ResultBusy != "busy" || ResultRestarted != "restarted" || ResultUnreadable != "unreadable" || CodeLegacy != "legacy" {
		t.Fatal("a result code's spelling changed")
	}
	for _, requestID := range []string{"", "0123456789abcdef0123456789abcdef"} {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: requestID, Outcome: OutcomeDenied, Code: ResultStepFailed,
			Steps: []DeploymentStep{{Service: "web", Step: StepPrecondition, Outcome: OutcomeDenied}}, Services: []DeploymentIdentity{}}
		if r.Validate() == nil {
			t.Fatalf("request_id %q: a denied step without a code validated", requestID)
		}
	}
}

func TestRequestIDs(t *testing.T) {
	for id, ok := range map[string]bool{
		"0123456789abcdef0123456789abcdef":     true,
		"3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b": true,
		"test-request":                         true,
		strings.Repeat("a", 64):                true,
		strings.Repeat("a", 65):                false,
		"":                                     false,
		"a b":                                  false,
		"a\nb":                                 false,
	} {
		if ValidRequestID(id) != ok {
			t.Errorf("%q: want %v", id, ok)
		}
	}
	now := time.Now()
	for _, v := range []any{goodDeployment(now), goodRemoval(now), goodResult()} {
		raw, _ := json.Marshal(v)
		if !strings.Contains(string(raw), `"request_id":"0123456789abcdef0123456789abcdef"`) {
			t.Fatalf("request_id missing on the wire: %s", raw)
		}
	}
}

// The largest result Deploy can produce must fit the frame, or an honest agent could not report.
// Only the step that ended the run carries a parameter, and the longest is as many whole
// unsupported codes as the step bound holds; the frame's every volume adds a step.
func TestDeploymentResultWorstCaseFitsTheFrame(t *testing.T) {
	steps := []string{StepPrecondition, StepPull, StepRecheck, StepRename, StepCreate, StepStop, StepStart, StepRemove}
	detail := UnsupportedCodes[0]
	for _, c := range UnsupportedCodes[1:] {
		if len(detail)+1+len(c) > MaxDeploymentStepDetailBytes {
			break
		}
		detail += "," + c
	}
	build := func(outcome string, stepOutcome func(service, step int) string, identities int) DeploymentResult {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: strings.Repeat("r", 64), Outcome: outcome}
		if outcome != OutcomeSucceeded {
			r.Code = ResultStepFailed
		}
		step := func(name, kind, outcome string) DeploymentStep {
			s := DeploymentStep{Service: name, Step: kind, Outcome: outcome}
			if outcome != OutcomeSucceeded && outcome != OutcomeSkipped {
				s.Code, s.Detail = "unsupported", detail
			}
			return s
		}
		for i := 0; i < MaxDeploymentServices; i++ {
			name := fmt.Sprintf("s%02d", i) + strings.Repeat("x", 60)
			for j, kind := range steps {
				r.Steps = append(r.Steps, step(name, kind, stepOutcome(i, j)))
			}
			if i == 0 {
				for v := 0; v < MaxDeploymentVolumes; v++ {
					r.Steps = append(r.Steps, step(name, StepVolume, stepOutcome(0, 1)))
				}
			}
			if i < identities {
				r.Services = append(r.Services, DeploymentIdentity{Service: name, ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), ImageDigest: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000})
			}
		}
		return r
	}
	for name, r := range map[string]DeploymentResult{
		"all succeeded": build(OutcomeSucceeded, func(int, int) string { return OutcomeSucceeded }, MaxDeploymentServices),
		"failed at last step": build(OutcomeFailed, func(i, j int) string {
			if i == MaxDeploymentServices-1 && j == len(steps)-1 {
				return OutcomeFailed
			}
			return OutcomeSucceeded
		}, MaxDeploymentServices),
		"failed at first step": build(OutcomeTimedOut, func(i, j int) string {
			if i == 0 && j == 0 {
				return OutcomeTimedOut
			}
			return OutcomeSkipped
		}, 0),
	} {
		if err := r.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > MaxDeploymentResultBytes {
			t.Fatalf("%s: %d bytes exceeds %d", name, len(raw), MaxDeploymentResultBytes)
		}
		t.Logf("%s: %d of %d bytes", name, len(raw), MaxDeploymentResultBytes)
	}
}

// mounted gives service 0 a named volume and a read-only bind, with the volume to ensure.
func mounted(r *DeploymentRequest) {
	r.Services[0].Mounts = []Mount{{Kind: MountVolume, Source: "shop_data", Target: "/var/lib/data"}, {Kind: MountBind, Source: "/srv/shop/config", Target: "/etc/shop", ReadOnly: true}}
	r.Volumes = []string{"shop_data"}
}

func TestDeploymentRequestMounts(t *testing.T) {
	now := time.Now()
	for name, mutate := range map[string]func(*DeploymentRequest){
		"volume and bind": mounted,
		"mounts at cap": func(r *DeploymentRequest) {
			for i := 0; i < MaxMounts; i++ {
				r.Services[0].Mounts = append(r.Services[0].Mounts, Mount{Kind: MountVolume, Source: "shop_data", Target: fmt.Sprintf("/m%d", i)})
			}
			r.Volumes = []string{"shop_data"}
		},
		"volumes at cap": func(r *DeploymentRequest) {
			for i := 0; i < MaxDeploymentVolumes; i++ {
				v := fmt.Sprintf("shop_v%d", i)
				r.Services[0].Mounts = append(r.Services[0].Mounts, Mount{Kind: MountVolume, Source: v, Target: "/" + v})
				r.Volumes = append(r.Volumes, v)
			}
			r.Services[0].Mounts = r.Services[0].Mounts[:MaxMounts]
			withServices(r, 2)
			r.Services[1].Mounts = nil
			for _, v := range r.Volumes[MaxMounts:] {
				r.Services[1].Mounts = append(r.Services[1].Mounts, Mount{Kind: MountVolume, Source: v, Target: "/" + v})
			}
		},
		// Resolved host names are "<project>_<name>", each part up to 64 bytes.
		"longest resolved name": func(r *DeploymentRequest) {
			v := strings.Repeat("p", 64) + "_" + strings.Repeat("n", 64)
			r.Services[0].Mounts, r.Volumes = []Mount{{Kind: MountVolume, Source: v, Target: "/data"}}, []string{v}
		},
		"same volume twice across services": func(r *DeploymentRequest) {
			mounted(r)
			withServices(r, 2)
		},
	} {
		r := goodDeployment(now)
		mutate(&r)
		if err := r.Validate(now); err != nil {
			t.Fatalf("%s refused: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*DeploymentRequest){
		"kind unset":        func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[0].Kind = "" },
		"kind tmpfs":        func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[0].Kind = "tmpfs" },
		"kind other":        func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[0].Kind = "other" },
		"volume name":       func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[0].Source = "-data" },
		"volume name slash": func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[0].Source = "a/b" },
		"volume name long": func(r *DeploymentRequest) {
			mounted(r)
			r.Services[0].Mounts[0].Source = strings.Repeat("v", 130)
		},
		"bind relative":     func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[1].Source = "srv/config" },
		"bind not clean":    func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[1].Source = "/data/../etc" },
		"bind trailing":     func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[1].Source = "/srv/config/" },
		"bind control":      func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[1].Source = "/srv/con\nfig" },
		"bind bidi":         func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[1].Source = "/srv/\u202efig" },
		"bind zero-width":   func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[1].Source = "/srv/con\u200bfig" },
		"bind not utf-8":    func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[1].Source = "/srv/\xff" },
		"target zero-width": func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[0].Target = "/data\u2060" },
		"bind empty":        func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[1].Source = "" },
		"target relative":   func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[0].Target = "data" },
		"target not clean":  func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[0].Target = "/a/./b" },
		"target root":       func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[0].Target = "/" },
		"target duplicate":  func(r *DeploymentRequest) { mounted(r); r.Services[0].Mounts[1].Target = "/var/lib/data" },
		"volume unused":     func(r *DeploymentRequest) { mounted(r); r.Volumes = append(r.Volumes, "shop_other") },
		"volume only bound": func(r *DeploymentRequest) { mounted(r); r.Volumes = []string{"shop_data", "srv"} },
		"volume duplicate":  func(r *DeploymentRequest) { mounted(r); r.Volumes = []string{"shop_data", "shop_data"} },
		"volume not listed": func(r *DeploymentRequest) { mounted(r); r.Volumes = nil },
		"second volume not listed": func(r *DeploymentRequest) {
			mounted(r)
			r.Services[0].Mounts = append(r.Services[0].Mounts, Mount{Kind: MountVolume, Source: "ext", Target: "/ext"})
		},
		"volume bad name": func(r *DeploymentRequest) { mounted(r); r.Volumes = []string{"shop data"} },
		"33 mounts": func(r *DeploymentRequest) {
			for i := 0; i <= MaxMounts; i++ {
				r.Services[0].Mounts = append(r.Services[0].Mounts, Mount{Kind: MountVolume, Source: "shop_data", Target: fmt.Sprintf("/m%d", i)})
			}
			r.Volumes = []string{"shop_data"}
		},
		"65 volumes": func(r *DeploymentRequest) {
			withServices(r, 3)
			for i := 0; i <= MaxDeploymentVolumes; i++ {
				v := fmt.Sprintf("shop_v%d", i)
				s := &r.Services[i/MaxMounts]
				s.Mounts = append(s.Mounts, Mount{Kind: MountVolume, Source: v, Target: "/" + v})
				r.Volumes = append(r.Volumes, v)
			}
		},
	} {
		r := goodDeployment(now)
		mutate(&r)
		if r.Validate(now) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestDeploymentResultVolumeStep(t *testing.T) {
	r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: OutcomeFailed, Code: ResultStepFailed, Steps: []DeploymentStep{{Service: "web", Step: StepVolume, Outcome: OutcomeFailed, Code: "volume_create_failed"}}}
	if err := r.Validate(); err != nil {
		t.Fatalf("volume step refused: %v", err)
	}
}

// The issue time bounds skew between the server's clock and the agent's, and the deadline is
// measured from it as well as from the agent's now.
func TestDeploymentRequestIssuedAt(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		mutate func(*DeploymentRequest)
		skew   bool
	}{
		"missing": {func(r *DeploymentRequest) { r.IssuedAt = time.Time{} }, false},
		"issued behind": {func(r *DeploymentRequest) {
			r.IssuedAt = now.Add(-MaxClockSkew - time.Second)
			r.Deadline = now.Add(time.Minute)
		}, true},
		"issued ahead": {func(r *DeploymentRequest) {
			r.IssuedAt = now.Add(MaxClockSkew + time.Second)
			r.Deadline = r.IssuedAt.Add(time.Minute)
		}, true},
		"deadline at issue": {func(r *DeploymentRequest) { r.Deadline = r.IssuedAt }, false},
		"deadline past lifetime from issue": {func(r *DeploymentRequest) {
			r.IssuedAt = now.Add(-time.Minute)
			r.Deadline = r.IssuedAt.Add(DeploymentLifetime + time.Second)
		}, false},
	} {
		r := goodDeployment(now)
		tc.mutate(&r)
		err := r.Validate(now)
		if err == nil || errors.Is(err, ErrClockSkew) != tc.skew {
			t.Fatalf("%s: %v", name, err)
		}
	}
	r := goodDeployment(now)
	r.IssuedAt = now.Add(-MaxClockSkew + time.Second)
	if err := r.Validate(now); err != nil {
		t.Fatalf("skew inside the bound refused: %v", err)
	}
	if ErrClockSkew.Error() != "clock skew exceeds 5 minutes" {
		t.Fatalf("detail: %q", ErrClockSkew.Error())
	}
}

func TestRemovalRequestIssuedAt(t *testing.T) {
	now := time.Now()
	r := goodRemoval(now)
	r.IssuedAt = time.Time{}
	if r.Validate(now) == nil {
		t.Fatal("missing issue time accepted")
	}
	r = goodRemoval(now)
	r.IssuedAt = now.Add(-MaxClockSkew - time.Second)
	r.Deadline = now.Add(time.Minute)
	if err := r.Validate(now); !errors.Is(err, ErrClockSkew) {
		t.Fatalf("skewed removal: %v", err)
	}
}
