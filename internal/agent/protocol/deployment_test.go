package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func goodDeployment(now time.Time) DeploymentRequest {
	return DeploymentRequest{
		Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: "ep_1", Project: "shop", Revision: 2, Deadline: now.Add(5 * time.Minute),
		Services: []DeploymentService{{
			Name: "web", ContainerName: "shop-web-1", ImageID: "sha256:" + strings.Repeat("a", 64),
			Replaces: InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000},
			Restart:  "always", Ports: []Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}}, Env: map[string]string{"TOKEN": "x"},
		}},
	}
}

func TestDeploymentRequestValidation(t *testing.T) {
	now := time.Now()
	if err := goodDeployment(now).Validate(now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DeploymentRequest){
		"bad uuid":         func(r *DeploymentRequest) { r.Deployment = "nope" },
		"bad endpoint":     func(r *DeploymentRequest) { r.Endpoint = "a b" },
		"bad project":      func(r *DeploymentRequest) { r.Project = "-shop" },
		"revision zero":    func(r *DeploymentRequest) { r.Revision = 0 },
		"revision 101":     func(r *DeploymentRequest) { r.Revision = 101 },
		"deadline past":    func(r *DeploymentRequest) { r.Deadline = now.Add(-time.Second) },
		"deadline far":     func(r *DeploymentRequest) { r.Deadline = now.Add(DeploymentLifetime + time.Second) },
		"no services":      func(r *DeploymentRequest) { r.Services = nil },
		"bad service name": func(r *DeploymentRequest) { r.Services[0].Name = "Web" },
		"bad container":    func(r *DeploymentRequest) { r.Services[0].ContainerName = "a/b" },
		"bad image":        func(r *DeploymentRequest) { r.Services[0].ImageID = strings.Repeat("a", 64) },
		"bad replaces":     func(r *DeploymentRequest) { r.Services[0].Replaces.CreatedUnix = 0 },
		"bad restart":      func(r *DeploymentRequest) { r.Services[0].Restart = "forever" },
		"port zero":        func(r *DeploymentRequest) { r.Services[0].Ports[0].Host = 0 },
		"port proto":       func(r *DeploymentRequest) { r.Services[0].Ports[0].Protocol = "sctp" },
		"port ip":          func(r *DeploymentRequest) { r.Services[0].Ports[0].HostIP = "lo" },
		"env name":         func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"1X": "v"} },
		"env nul":          func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"X": "a\x00b"} },
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

func TestDeploymentResultValidation(t *testing.T) {
	good := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: OutcomeSucceeded, Steps: []DeploymentStep{{Service: "web", Step: StepCreate, Outcome: OutcomeSucceeded}}, Services: []DeploymentIdentity{{Service: "web", ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DeploymentResult){
		"outcome":      func(r *DeploymentResult) { r.Outcome = "done" },
		"step name":    func(r *DeploymentResult) { r.Steps[0].Step = "pull" },
		"step outcome": func(r *DeploymentResult) { r.Steps[0].Outcome = "ok" },
		"step detail": func(r *DeploymentResult) {
			r.Steps[0].Outcome, r.Steps[0].Detail = OutcomeFailed, strings.Repeat("d", MaxDeploymentStepDetailBytes+1)
		},
		"detail on succeeded step": func(r *DeploymentResult) { r.Steps[0].Detail = "done" },
		"step service":             func(r *DeploymentResult) { r.Steps[0].Service = "Web" },
		"identity":                 func(r *DeploymentResult) { r.Services[0].ImageID = "latest" },
		"detail":                   func(r *DeploymentResult) { r.Detail = strings.Repeat("d", MaxResultDetailBytes+1) },
		"too many steps":           func(r *DeploymentResult) { r.Steps = make([]DeploymentStep, 8*MaxDeploymentServices+1) },
	} {
		r := good
		r.Steps = append([]DeploymentStep{}, good.Steps...)
		r.Services = append([]DeploymentIdentity{}, good.Services...)
		mutate(&r)
		if r.Validate() == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	skipped := good
	skipped.Steps = []DeploymentStep{{Service: "web", Step: StepStop, Outcome: OutcomeSkipped}}
	if err := skipped.Validate(); err != nil {
		t.Fatalf("skipped step refused: %v", err)
	}
}

// The largest result Deploy can produce must fit the frame, or an honest agent could not report.
// Validate allows a detail only on the step that ended the run; succeeded and skipped steps carry
// none. The run's own detail is at its maximum in every case.
func TestDeploymentResultWorstCaseFitsTheFrame(t *testing.T) {
	steps := []string{StepPrecondition, StepImage, StepRename, StepCreate, StepStop, StepStart, StepRemove}
	detail := strings.Repeat("d", MaxDeploymentStepDetailBytes)
	build := func(outcome string, stepOutcome func(service, step int) string, identities int) DeploymentResult {
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: outcome, Detail: strings.Repeat("r", MaxResultDetailBytes)}
		for i := 0; i < MaxDeploymentServices; i++ {
			name := fmt.Sprintf("s%02d", i) + strings.Repeat("x", 60)
			for j, step := range steps {
				s := DeploymentStep{Service: name, Step: step, Outcome: stepOutcome(i, j)}
				if s.Outcome != OutcomeSucceeded && s.Outcome != OutcomeSkipped {
					s.Detail = detail
				}
				r.Steps = append(r.Steps, s)
			}
			if i < identities {
				r.Services = append(r.Services, DeploymentIdentity{Service: name, ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000})
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
