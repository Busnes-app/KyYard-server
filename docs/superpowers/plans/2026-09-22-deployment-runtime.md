# Deployment Runtime Primitives Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the Docker adapter a `Deploy` that executes one deployment request (stop, rename, create, start, remove per service, with preconditions and per-step outcomes) and define the wire types PR B will carry, without wiring any of it to the server, the agent loop or the UI.

**Architecture:** `internal/agent/protocol/deployment.go` holds the request/result types, constants and `Validate`. `internal/runtime/docker/deploy.go` holds `Client.Deploy`, a small `deployRun` state machine that records every step and stops at the first non-success, plus one JSON-POST helper. Fake-Engine tests prove call order, bodies and outcome classification; a gated real-Docker test proves it against an Engine and runs in CI.

**Tech Stack:** Go 1.25, Docker Engine API v1.41 over the existing `Client` (`get`, `post`, `del`, `bounded`, `statusError`), `httptest` fake Engines, real Docker in CI.

**Spec:** `docs/superpowers/specs/2026-09-22-deployment-runtime-design.md`

## Global Constraints

- Worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/deployment-apply`, branch `feat/deployment-apply`. Never edit the root checkout.
- Nothing in this plan sends a frame, advertises a capability, adds a route, or touches the store or UI. `Deploy` is callable only from tests until PR B.
- Constants exactly as the spec: `TypeDeploymentApply = "deployment.apply"`, `TypeDeploymentResult = "deployment.result"`, `CapabilityDeploymentApply = "deployment.apply"`, `MaxDeploymentRequestBytes = 192 << 10`, `MaxDeploymentResultBytes = 64 << 10`, `DeploymentLifetime = 15 * time.Minute`, `MaxDeploymentServices = 100`, `MaxDeploymentStepDetailBytes = 256`.
- Bounds: services 1..100; env ≤128 entries, names `^[A-Za-z_][A-Za-z0-9_]{0,127}$`, values ≤16 KiB valid UTF-8 without NUL, total ≤64 KiB; ports ≤64 with container 1..65535, host 1..65535, protocol tcp|udp; restart in `""|no|always|unless-stopped|on-failure`; container name `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$` (`protocol.ValidContainerID`); service `^[a-z0-9][a-z0-9_-]{0,62}$`; project `^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`; image ID `sha256:` + 64 hex; deployment ID a lowercase UUID; endpoint matches `execStreamID`.
- Step sequence per service: precondition, image, stop (`?t=10`), rename (`{oldName}.kyyard-prev-{deployment[:8]}`), create (201), start then identity read, remove (no `v=1`). First non-success ends the run; remaining steps and services are `skipped`.
- Outcome classification: a deadline already past fails `Validate` → `denied` with no call; parent context cancelled → `unknown`; any deadline reached during a call (per-call or overall) → `timed_out`; Engine 404 on the old container → `denied`; identity/mount/network/privileged mismatch → `denied`; missing image → `failed`; create 409 → `failed`; other ≥400 → `failed` with the status code in fixed text.
- Engine error text never enters a result; env values never enter a result (canary tests).
- Every Engine call gets `callBudget` (20 s) except stop, which gets `operationBudget` (30 s); all nested under the request deadline.
- `gofmt -w` every touched file; `go vet ./internal/runtime/docker/ ./internal/agent/protocol/`; commits end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. A rename target that already exists (a previous failed apply left `web.kyyard-prev-xxxx`): the rename step gets 409 and must be `failed` with the old container stopped but untouched otherwise. Test in Task 3 ("rename conflict").
2. The old container's inspected `Name` has a leading slash (`/shop-web-1`); the rename must strip it or Docker refuses. Test in Task 2 (happy path asserts the rename query).
3. A service whose create succeeds but start fails: the new container exists stopped, the old is renamed and stopped, nothing removed, identity NOT recorded. Test in Task 3 ("start fails").
4. Parent cancel while the Engine is mid-stop: the step must be `unknown`, not `timed_out`, because the Engine may have acted. Test in Task 3 ("cancel mid-run").
5. Env value containing `=` or newline must survive as one `K=V` entry and never appear in the result. Test in Task 2 (happy path uses `TOKEN=a=b\ncanary`).

---

### Task 1: Protocol types and validation

**Files:**
- Create: `internal/agent/protocol/deployment.go`
- Test: `internal/agent/protocol/deployment_test.go`

**Interfaces:**
- Consumes: `InspectionTarget` and its `Validate`, `Port`, `fullDockerID`, `execStreamID`, `ValidContainerID`, `MaxResultDetailBytes`, outcome constants, `CleanText`.
- Produces (verbatim):

```go
const (
	TypeDeploymentApply          = "deployment.apply"
	TypeDeploymentResult         = "deployment.result"
	CapabilityDeploymentApply    = "deployment.apply"
	MaxDeploymentRequestBytes    = 192 << 10
	MaxDeploymentResultBytes     = 64 << 10
	DeploymentLifetime           = 15 * time.Minute
	MaxDeploymentServices        = 100
	MaxDeploymentEnvEntries      = 128
	MaxDeploymentEnvValueBytes   = 16 << 10
	MaxDeploymentEnvBytes        = 64 << 10
	MaxDeploymentPorts           = 64
	MaxDeploymentStepDetailBytes = 256
	StepPrecondition             = "precondition"
	StepImage                    = "image"
	StepStop                     = "stop"
	StepRename                   = "rename"
	StepCreate                   = "create"
	StepStart                    = "start"
	StepRemove                   = "remove"
	OutcomeSkipped               = "skipped"
)
type DeploymentRequest struct {
	Deployment string              `json:"deployment"`
	Endpoint   string              `json:"endpoint"`
	Project    string              `json:"project"`
	Revision   int                 `json:"revision"`
	Deadline   time.Time           `json:"deadline"`
	Services   []DeploymentService `json:"services"`
}
type DeploymentService struct {
	Name          string            `json:"name"`
	ContainerName string            `json:"container_name"`
	ImageID       string            `json:"image_id"`
	Replaces      InspectionTarget  `json:"replaces"`
	Restart       string            `json:"restart"`
	Ports         []Port            `json:"ports"`
	Env           map[string]string `json:"env"`
}
func (r DeploymentRequest) Validate(now time.Time) error
type DeploymentResult struct {
	Deployment string               `json:"deployment"`
	Outcome    string               `json:"outcome"`
	Detail     string               `json:"detail"`
	Steps      []DeploymentStep     `json:"steps"`
	Services   []DeploymentIdentity `json:"services"`
}
type DeploymentStep struct {
	Service string `json:"service"`
	Step    string `json:"step"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}
type DeploymentIdentity struct {
	Service     string `json:"service"`
	ContainerID string `json:"container_id"`
	ImageID     string `json:"image_id"`
	CreatedUnix int64  `json:"created_unix"`
}
func (r DeploymentResult) Validate() error
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/agent/protocol/deployment_test.go
package protocol

import (
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
			Restart: "always", Ports: []Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}}, Env: map[string]string{"TOKEN": "x"},
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
		"bad restart":       func(r *DeploymentRequest) { r.Services[0].Restart = "forever" },
		"port zero":         func(r *DeploymentRequest) { r.Services[0].Ports[0].Host = 0 },
		"port proto":        func(r *DeploymentRequest) { r.Services[0].Ports[0].Protocol = "sctp" },
		"port ip":           func(r *DeploymentRequest) { r.Services[0].Ports[0].HostIP = "lo" },
		"env name":          func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"1X": "v"} },
		"env nul":           func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"X": "a\x00b"} },
		"env value size":    func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"X": strings.Repeat("v", MaxDeploymentEnvValueBytes+1)} },
		"env total size":    func(r *DeploymentRequest) { r.Services[0].Env = map[string]string{"A": strings.Repeat("v", MaxDeploymentEnvValueBytes), "B": strings.Repeat("v", MaxDeploymentEnvValueBytes), "C": strings.Repeat("v", MaxDeploymentEnvValueBytes), "D": strings.Repeat("v", MaxDeploymentEnvValueBytes), "E": "v"} },
		"duplicate service": func(r *DeploymentRequest) { r.Services = append(r.Services, r.Services[0]) },
		"duplicate replace": func(r *DeploymentRequest) { s := r.Services[0]; s.Name = "db"; s.ContainerName = "shop-db-1"; r.Services = append(r.Services, s) },
		"duplicate name":    func(r *DeploymentRequest) { s := r.Services[0]; s.Name = "db"; s.Replaces.ContainerID = strings.Repeat("d", 64); r.Services = append(r.Services, s) },
	} {
		r := goodDeployment(now)
		mutate(&r)
		if r.Validate(now) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	r := goodDeployment(now)
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
		"outcome":       func(r *DeploymentResult) { r.Outcome = "done" },
		"step name":     func(r *DeploymentResult) { r.Steps[0].Step = "pull" },
		"step outcome":  func(r *DeploymentResult) { r.Steps[0].Outcome = "ok" },
		"step detail":   func(r *DeploymentResult) { r.Steps[0].Detail = strings.Repeat("d", MaxDeploymentStepDetailBytes+1) },
		"step service":  func(r *DeploymentResult) { r.Steps[0].Service = "Web" },
		"identity":      func(r *DeploymentResult) { r.Services[0].ImageID = "latest" },
		"detail":        func(r *DeploymentResult) { r.Detail = strings.Repeat("d", MaxResultDetailBytes+1) },
		"too many steps": func(r *DeploymentResult) { r.Steps = make([]DeploymentStep, 8*MaxDeploymentServices+1) },
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/protocol/ -run 'TestDeployment' -count=1`
Expected: compile error, `DeploymentRequest` undefined.

- [ ] **Step 3: Implement**

```go
// internal/agent/protocol/deployment.go
package protocol

import (
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Deployment frames carry one plan to the agent and one result back. The request holds
// resolved environment values: it exists only in memory on both sides and is never logged.
// See docs/agent-protocol.md, Deployment apply.
const (
	TypeDeploymentApply          = "deployment.apply"
	TypeDeploymentResult         = "deployment.result"
	CapabilityDeploymentApply    = "deployment.apply"
	MaxDeploymentRequestBytes    = 192 << 10
	MaxDeploymentResultBytes     = 64 << 10
	DeploymentLifetime           = 15 * time.Minute
	MaxDeploymentServices        = 100
	MaxDeploymentEnvEntries      = 128
	MaxDeploymentEnvValueBytes   = 16 << 10
	MaxDeploymentEnvBytes        = 64 << 10
	MaxDeploymentPorts           = 64
	MaxDeploymentStepDetailBytes = 256
	StepPrecondition             = "precondition"
	StepImage                    = "image"
	StepStop                     = "stop"
	StepRename                   = "rename"
	StepCreate                   = "create"
	StepStart                    = "start"
	StepRemove                   = "remove"
	OutcomeSkipped               = "skipped"
)

var (
	deploymentUUID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	deploymentProject = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	deploymentService = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	deploymentEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	deploymentSteps   = map[string]bool{StepPrecondition: true, StepImage: true, StepStop: true, StepRename: true, StepCreate: true, StepStart: true, StepRemove: true}
	deploymentRestart = map[string]bool{"": true, "no": true, "always": true, "unless-stopped": true, "on-failure": true}
	resultOutcomes    = map[string]bool{OutcomeSucceeded: true, OutcomeFailed: true, OutcomeDenied: true, OutcomeTimedOut: true, OutcomeUnknown: true}
)

type DeploymentRequest struct {
	Deployment string              `json:"deployment"`
	Endpoint   string              `json:"endpoint"`
	Project    string              `json:"project"`
	Revision   int                 `json:"revision"`
	Deadline   time.Time           `json:"deadline"`
	Services   []DeploymentService `json:"services"`
}
type DeploymentService struct {
	Name          string            `json:"name"`
	ContainerName string            `json:"container_name"`
	ImageID       string            `json:"image_id"`
	Replaces      InspectionTarget  `json:"replaces"`
	Restart       string            `json:"restart"`
	Ports         []Port            `json:"ports"`
	Env           map[string]string `json:"env"`
}

func fullImageID(id string) bool {
	return strings.HasPrefix(id, "sha256:") && fullDockerID.MatchString(strings.TrimPrefix(id, "sha256:"))
}

// Validate refuses anything the adapter would have to guess about. Every bound here is a
// wire bound as well: PR B rejects a frame that fails it before touching the runtime.
func (r DeploymentRequest) Validate(now time.Time) error {
	if !deploymentUUID.MatchString(r.Deployment) || !execStreamID.MatchString(r.Endpoint) || !deploymentProject.MatchString(r.Project) || r.Revision < 1 || r.Revision > 100 {
		return errors.New("invalid deployment identity")
	}
	if !r.Deadline.After(now) || r.Deadline.After(now.Add(DeploymentLifetime)) {
		return errors.New("invalid deployment deadline")
	}
	if len(r.Services) == 0 || len(r.Services) > MaxDeploymentServices {
		return errors.New("invalid service count")
	}
	names, containers, replaces := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range r.Services {
		if !deploymentService.MatchString(s.Name) || names[s.Name] || !ValidContainerID(s.ContainerName) || containers[s.ContainerName] || !fullImageID(s.ImageID) || s.Replaces.Validate() != nil || replaces[s.Replaces.ContainerID] || !deploymentRestart[s.Restart] {
			return errors.New("invalid deployment service")
		}
		names[s.Name], containers[s.ContainerName], replaces[s.Replaces.ContainerID] = true, true, true
		if len(s.Ports) > MaxDeploymentPorts {
			return errors.New("too many ports")
		}
		for _, p := range s.Ports {
			if p.Container < 1 || p.Container > 65535 || p.Host < 1 || p.Host > 65535 || (p.Protocol != "tcp" && p.Protocol != "udp") {
				return errors.New("invalid port")
			}
			if p.HostIP != "" {
				if ip, err := netip.ParseAddr(p.HostIP); err != nil || ip.Zone() != "" {
					return errors.New("invalid host address")
				}
			}
		}
		if len(s.Env) > MaxDeploymentEnvEntries {
			return errors.New("too many environment entries")
		}
		total := 0
		for k, v := range s.Env {
			total += len(k) + len(v)
			if !deploymentEnvName.MatchString(k) || len(v) > MaxDeploymentEnvValueBytes || !utf8.ValidString(v) || strings.ContainsRune(v, 0) || total > MaxDeploymentEnvBytes {
				return errors.New("invalid environment value")
			}
		}
	}
	return nil
}

type DeploymentResult struct {
	Deployment string               `json:"deployment"`
	Outcome    string               `json:"outcome"`
	Detail     string               `json:"detail"`
	Steps      []DeploymentStep     `json:"steps"`
	Services   []DeploymentIdentity `json:"services"`
}
type DeploymentStep struct {
	Service string `json:"service"`
	Step    string `json:"step"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}
type DeploymentIdentity struct {
	Service     string `json:"service"`
	ContainerID string `json:"container_id"`
	ImageID     string `json:"image_id"`
	CreatedUnix int64  `json:"created_unix"`
}

func (r DeploymentResult) Validate() error {
	if !deploymentUUID.MatchString(r.Deployment) || !resultOutcomes[r.Outcome] || len(r.Detail) > MaxResultDetailBytes || len(r.Steps) > 8*MaxDeploymentServices || len(r.Services) > MaxDeploymentServices {
		return errors.New("invalid deployment result")
	}
	for _, s := range r.Steps {
		if !deploymentService.MatchString(s.Service) || !deploymentSteps[s.Step] || !(resultOutcomes[s.Outcome] || s.Outcome == OutcomeSkipped) || len(s.Detail) > MaxDeploymentStepDetailBytes {
			return errors.New("invalid deployment step")
		}
	}
	for _, id := range r.Services {
		if !deploymentService.MatchString(id.Service) || (InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() != nil {
			return errors.New("invalid deployment identity")
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `gofmt -w internal/agent/protocol/deployment*.go && go test ./internal/agent/protocol/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/protocol && git commit -m "feat: deployment apply wire types

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Adapter Deploy, happy path

**Files:**
- Create: `internal/runtime/docker/deploy.go`
- Test: `internal/runtime/docker/deploy_test.go` (fake Engine helper + happy path)

**Interfaces:**
- Consumes: Task 1 types; `Client.get`, `Client.post`, `Client.del`, `statusOf`, `callBudget`, `operationBudget`, `protocol.CleanText`.
- Produces: `func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest) protocol.DeploymentResult`; test helper `newFakeDeployEngine(t) *fakeDeployEngine` (package `docker_test`) with recorded `calls []engineCall{Method, Path, Query, Body string}` and knobs used by Task 3.

- [ ] **Step 1: Write the fake Engine and the failing happy-path test**

```go
// internal/runtime/docker/deploy_test.go
package docker_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
)

const (
	oldID = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
	newID = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	oldImage = "sha256:d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
	newImage = "sha256:e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4"
	deploymentID = "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b"
)

type engineCall struct{ Method, Path, Query, Body string }

// fakeDeployEngine answers the eight calls one service needs. Knobs make single steps fail so
// each classification is proven by one test.
type fakeDeployEngine struct {
	mu           sync.Mutex
	calls        []engineCall
	oldContainer map[string]any // returned by GET /containers/{old}/json
	oldStatus    int            // 200 default; 404 = the container is gone
	imageStatus  int            // GET /images/{new}/json; 200 default
	stopStatus   int            // 204 default; 304 allowed
	stopDelay    time.Duration
	renameStatus int
	createStatus int // 201 default
	startStatus  int
	removeStatus int
	srv          *httptest.Server
}

func newFakeDeployEngine(t *testing.T) *fakeDeployEngine {
	t.Helper()
	f := &fakeDeployEngine{oldStatus: 200, imageStatus: 200, stopStatus: 204, renameStatus: 204, createStatus: 201, startStatus: 204, removeStatus: 204}
	f.oldContainer = map[string]any{"Id": oldID, "Image": oldImage, "Name": "/shop-web-1", "Created": "2023-11-14T22:13:20Z", "State": map[string]any{"Status": "running"}, "Mounts": []any{}, "HostConfig": map[string]any{"NetworkMode": "default", "Privileged": false}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, engineCall{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(body)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.EscapedPath()
		switch {
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+oldID+"/json"):
			w.WriteHeader(f.oldStatus)
			_ = json.NewEncoder(w).Encode(f.oldContainer)
		case r.Method == "GET" && strings.HasSuffix(p, "/images/"+newImage+"/json"):
			w.WriteHeader(f.imageStatus)
			_, _ = w.Write([]byte(`{"Id":"` + newImage + `"}`))
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/"+oldID+"/stop"):
			time.Sleep(f.stopDelay)
			w.WriteHeader(f.stopStatus)
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/"+oldID+"/rename"):
			w.WriteHeader(f.renameStatus)
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/create"):
			w.WriteHeader(f.createStatus)
			_, _ = w.Write([]byte(`{"Id":"` + newID + `","Warnings":[]}`))
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/"+newID+"/start"):
			w.WriteHeader(f.startStatus)
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+newID+"/json"):
			_, _ = w.Write([]byte(`{"Id":"` + newID + `","Image":"` + newImage + `","Created":"2024-01-01T00:00:01Z","Name":"/shop-web-1","State":{"Status":"running"}}`))
		case r.Method == "DELETE" && strings.HasSuffix(p, "/containers/"+oldID):
			w.WriteHeader(f.removeStatus)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}
func (f *fakeDeployEngine) client() *docker.Client { return docker.NewHTTP(f.srv.Client(), f.srv.URL) }
func (f *fakeDeployEngine) steps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, c := range f.calls {
		out = append(out, c.Method+" "+c.Path[strings.LastIndex(c.Path, "/v1.41")+len("/v1.41"):])
	}
	return out
}
func request(services ...protocol.DeploymentService) protocol.DeploymentRequest {
	return protocol.DeploymentRequest{Deployment: deploymentID, Endpoint: "ep_1", Project: "shop", Revision: 2, Deadline: time.Now().Add(5 * time.Minute), Services: services}
}
func webService() protocol.DeploymentService {
	return protocol.DeploymentService{Name: "web", ContainerName: "shop-web-1", ImageID: newImage, Replaces: protocol.InspectionTarget{ContainerID: oldID, ImageID: oldImage, CreatedUnix: 1700000000}, Restart: "on-failure", Ports: []protocol.Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}}, Env: map[string]string{"TOKEN": "a=b\ncanary-secret", "A": "1"}}
}

func TestDeployReplacesOneServiceInOrder(t *testing.T) {
	f := newFakeDeployEngine(t)
	res := f.client().Deploy(context.Background(), request(webService()))
	if res.Outcome != protocol.OutcomeSucceeded || res.Deployment != deploymentID {
		t.Fatalf("outcome: %+v", res)
	}
	want := []string{"GET /containers/" + oldID + "/json", "GET /images/" + newImage + "/json", "POST /containers/" + oldID + "/stop", "POST /containers/" + oldID + "/rename", "POST /containers/create", "POST /containers/" + newID + "/start", "GET /containers/" + newID + "/json", "DELETE /containers/" + oldID}
	if got := f.steps(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("call order:\n got %v\nwant %v", got, want)
	}
	if f.calls[2].Query != "t=10" || f.calls[3].Query != "name=shop-web-1.kyyard-prev-3f2b1c9e" || f.calls[4].Query != "name=shop-web-1" || f.calls[7].Query != "" {
		t.Fatalf("queries: %+v", f.calls)
	}
	var body struct {
		Image        string
		Env          []string
		Labels       map[string]string
		ExposedPorts map[string]struct{}
		HostConfig   struct {
			PortBindings  map[string][]struct{ HostIp, HostPort string }
			RestartPolicy struct {
				Name              string
				MaximumRetryCount int
			}
		}
	}
	if err := json.Unmarshal([]byte(f.calls[4].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Image != newImage || strings.Join(body.Env, "|") != "A=1|TOKEN=a=b\ncanary-secret" || body.HostConfig.RestartPolicy.Name != "on-failure" || body.HostConfig.RestartPolicy.MaximumRetryCount != 0 {
		t.Fatalf("create body: %+v", body)
	}
	for k, v := range map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "web", "com.docker.compose.container-number": "1", "com.docker.compose.oneoff": "False", "kyyard.deployment": deploymentID, "kyyard.revision": "2"} {
		if body.Labels[k] != v {
			t.Fatalf("label %s = %q", k, body.Labels[k])
		}
	}
	if _, ok := body.ExposedPorts["80/tcp"]; !ok || len(body.HostConfig.PortBindings["80/tcp"]) != 1 || body.HostConfig.PortBindings["80/tcp"][0].HostIp != "127.0.0.1" || body.HostConfig.PortBindings["80/tcp"][0].HostPort != "8080" {
		t.Fatalf("ports: %+v", body)
	}
	if len(res.Steps) != 7 || len(res.Services) != 1 || res.Services[0].ContainerID != newID || res.Services[0].ImageID != newImage || res.Services[0].CreatedUnix != 1704067201 {
		t.Fatalf("result: %+v", res)
	}
	for _, s := range res.Steps {
		if s.Outcome != protocol.OutcomeSucceeded || s.Service != "web" {
			t.Fatalf("step: %+v", s)
		}
	}
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), "canary-secret") {
		t.Fatal("environment value reached the result")
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDeployRefusesAnInvalidRequestWithoutCalling(t *testing.T) {
	f := newFakeDeployEngine(t)
	s := webService()
	s.Env = map[string]string{"1BAD": "x"}
	res := f.client().Deploy(context.Background(), request(s))
	if res.Outcome != protocol.OutcomeDenied || len(f.calls) != 0 || len(res.Steps) != 0 {
		t.Fatalf("invalid request: %+v calls=%d", res, len(f.calls))
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/runtime/docker/ -run 'TestDeploy' -count=1`
Expected: compile error, `Deploy` undefined.

- [ ] **Step 3: Implement deploy.go**

```go
// internal/runtime/docker/deploy.go
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// stopGrace is the seconds Docker waits before killing a container being stopped; ten is
// Docker's own default and Compose's.
const stopGrace = 10

// Deploy replaces each service's mapped container with one created from the pinned image ID,
// in plan order: precondition, image, stop, rename, create, start, remove. The first step that
// is not a success ends the run and every later step is recorded as skipped. Nothing is rolled
// back: a renamed, stopped old container stays where the result says it is. No image is
// pulled and no volume is touched. See docs/superpowers/specs/2026-09-22-deployment-runtime-design.md.
func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.Validate(time.Now()); err != nil {
		res.Outcome, res.Detail = protocol.OutcomeDenied, "the deployment request is invalid"
		return res
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	r := &deployRun{c: c, parent: parent, req: req, res: res}
	for _, s := range req.Services {
		r.service(ctx, s)
	}
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

type deployRun struct {
	c      *Client
	parent context.Context
	req    protocol.DeploymentRequest
	res    protocol.DeploymentResult
}

func (r *deployRun) stopped() bool { return r.res.Outcome != "" }

// step records one outcome. The first non-success fixes the run's outcome and detail.
func (r *deployRun) step(service, step string, run func() (string, string)) bool {
	if r.stopped() {
		r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: step, Outcome: protocol.OutcomeSkipped})
		return false
	}
	outcome, detail := run()
	r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: step, Outcome: outcome, Detail: bound(detail, protocol.MaxDeploymentStepDetailBytes)})
	if outcome != protocol.OutcomeSucceeded {
		r.res.Outcome, r.res.Detail = outcome, bound(fmt.Sprintf("service %s, step %s: %s", service, step, detail), protocol.MaxResultDetailBytes)
		return false
	}
	return true
}

// outcomeFor classifies a call that returned an error. A parent cancelled before its deadline
// means the session dropped and the Engine may have acted: unknown, never retried by itself.
func (r *deployRun) outcomeFor(ctx context.Context, err error, status int) (string, string) {
	switch {
	case r.parent.Err() == context.Canceled:
		return protocol.OutcomeUnknown, "the connection ended before the runtime answered"
	case err != nil && ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "the runtime did not answer in time"
	case err != nil:
		return protocol.OutcomeFailed, "the runtime call failed"
	}
	return protocol.OutcomeFailed, fmt.Sprintf("the runtime refused with status %d", status)
}

type inspectedForDeploy struct {
	ID      string `json:"Id"`
	Image   string
	Name    string
	Created time.Time
	State   struct{ Status string }
	Mounts  []struct{ Type string }
	HostConfig struct {
		NetworkMode string
		Privileged  bool
	}
}

func (r *deployRun) service(ctx context.Context, s protocol.DeploymentService) {
	old := url.PathEscape(s.Replaces.ContainerID)
	var before inspectedForDeploy
	r.step(s.Name, protocol.StepPrecondition, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		if err := r.c.get(cctx, "/containers/"+old+"/json", &before); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return protocol.OutcomeDenied, "the container no longer exists"
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		switch {
		case before.ID != s.Replaces.ContainerID || before.Image != s.Replaces.ImageID || before.Created.Unix() != s.Replaces.CreatedUnix:
			return protocol.OutcomeDenied, "the container is not the one this plan was decided about"
		case len(before.Mounts) > 0:
			return protocol.OutcomeDenied, "the container has mounts the definition does not describe; recreating it would drop them"
		case before.HostConfig.NetworkMode != "default" && before.HostConfig.NetworkMode != "bridge":
			return protocol.OutcomeDenied, "the container uses a network mode the definition does not describe"
		case before.HostConfig.Privileged:
			return protocol.OutcomeDenied, "the container is privileged; the definition cannot express that"
		}
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepImage, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var im struct {
			ID string `json:"Id"`
		}
		if err := r.c.get(cctx, "/images/"+url.PathEscape(s.ImageID)+"/json", &im); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return protocol.OutcomeFailed, "the pinned image is not present on this host"
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		if im.ID != s.ImageID {
			return protocol.OutcomeFailed, "the host reported a different image identity"
		}
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepStop, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+old+"/stop?t="+strconv.Itoa(stopGrace))
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepRename, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		name := strings.TrimPrefix(before.Name, "/") + ".kyyard-prev-" + r.req.Deployment[:8]
		status, err := r.c.post(cctx, "/containers/"+old+"/rename?name="+url.QueryEscape(name))
		if err != nil || status >= 400 {
			if status == http.StatusConflict {
				return protocol.OutcomeFailed, "a container already holds the name reserved for the previous one"
			}
			return r.outcomeFor(cctx, err, status)
		}
		return protocol.OutcomeSucceeded, ""
	})
	var created string
	r.step(s.Name, protocol.StepCreate, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var out struct {
			ID string `json:"Id"`
		}
		status, err := r.c.postJSON(cctx, "/containers/create?name="+url.QueryEscape(s.ContainerName), createBody(r.req, s), &out)
		if err != nil || status != http.StatusCreated {
			if status == http.StatusConflict {
				return protocol.OutcomeFailed, "a container with that name already exists"
			}
			return r.outcomeFor(cctx, err, status)
		}
		if !protocol.ValidExecID(out.ID) {
			return protocol.OutcomeFailed, "the runtime returned an unusable container identity"
		}
		created = out.ID
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepStart, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+created+"/start")
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		var after inspectedForDeploy
		if err := r.c.get(cctx, "/containers/"+created+"/json", &after); err != nil {
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		id := protocol.DeploymentIdentity{Service: s.Name, ContainerID: after.ID, ImageID: after.Image, CreatedUnix: after.Created.Unix()}
		if (protocol.InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() != nil || after.Image != s.ImageID {
			return protocol.OutcomeFailed, "the new container's identity could not be verified"
		}
		r.res.Services = append(r.res.Services, id)
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepRemove, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		status, err := r.c.del(cctx, "/containers/"+old)
		if err != nil || status >= 400 {
			return r.outcomeFor(cctx, err, status)
		}
		return protocol.OutcomeSucceeded, ""
	})
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}
type containerCreate struct {
	Image        string              `json:"Image"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   struct {
		PortBindings  map[string][]portBinding `json:"PortBindings,omitempty"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

// createBody is the whole configuration of the new container: the definition's subset and
// the Compose labels discovery already groups by. Nothing is copied from the old container.
func createBody(req protocol.DeploymentRequest, s protocol.DeploymentService) containerCreate {
	body := containerCreate{Image: s.ImageID, Env: []string{}, Labels: map[string]string{
		"com.docker.compose.project": req.Project, "com.docker.compose.service": s.Name, "com.docker.compose.container-number": "1", "com.docker.compose.oneoff": "False",
		"kyyard.deployment": req.Deployment, "kyyard.revision": strconv.Itoa(req.Revision),
	}}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		body.Env = append(body.Env, k+"="+s.Env[k])
	}
	if len(s.Ports) > 0 {
		body.ExposedPorts = map[string]struct{}{}
		body.HostConfig.PortBindings = map[string][]portBinding{}
	}
	for _, p := range s.Ports {
		key := strconv.Itoa(p.Container) + "/" + p.Protocol
		body.ExposedPorts[key] = struct{}{}
		body.HostConfig.PortBindings[key] = append(body.HostConfig.PortBindings[key], portBinding{HostIP: p.HostIP, HostPort: strconv.Itoa(p.Host)})
	}
	body.HostConfig.RestartPolicy.Name = s.Restart
	if body.HostConfig.RestartPolicy.Name == "" {
		body.HostConfig.RestartPolicy.Name = "no"
	}
	return body
}

// postJSON sends a JSON body and decodes a JSON answer. The Engine's error body is read and
// discarded: its text can echo configuration, so only the status reaches a result.
func (c *Client) postJSON(ctx context.Context, path string, body, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.Unmarshal(answer, out)
}
```

Note: `protocol.ValidExecID` matches 64 lowercase hex, which is what the Engine returns for a container ID; reuse it rather than adding a new validator.

- [ ] **Step 4: Run the tests**

Run: `gofmt -w internal/runtime/docker/deploy*.go && go vet ./internal/runtime/docker/ && go test ./internal/runtime/docker/ -run 'TestDeploy' -count=1`
Expected: PASS for both tests.

- [ ] **Step 5: Run the whole package**

Run: `go test ./internal/runtime/docker/ -count=1`
Expected: PASS (real-Docker tests skip without their env vars).

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/docker && git commit -m "feat: Docker adapter deploys a plan by replacing containers in order

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Adapter failure semantics

**Files:**
- Test: `internal/runtime/docker/deploy_test.go` (append)
- Modify: `internal/runtime/docker/deploy.go` only if a test exposes a defect.

**Interfaces:**
- Consumes: `fakeDeployEngine` knobs from Task 2.

- [ ] **Step 1: Write the failing tests**

```go
func TestDeployPreconditionsRefuseBeforeTouchingAnything(t *testing.T) {
	for name, mutate := range map[string]func(*fakeDeployEngine){
		"image":      func(f *fakeDeployEngine) { f.oldContainer["Image"] = newImage },
		"created":    func(f *fakeDeployEngine) { f.oldContainer["Created"] = "2023-11-14T22:13:21Z" },
		"mounts":     func(f *fakeDeployEngine) { f.oldContainer["Mounts"] = []any{map[string]any{"Type": "volume"}} },
		"network":    func(f *fakeDeployEngine) { f.oldContainer["HostConfig"] = map[string]any{"NetworkMode": "host"} },
		"privileged": func(f *fakeDeployEngine) { f.oldContainer["HostConfig"] = map[string]any{"NetworkMode": "bridge", "Privileged": true} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			mutate(f)
			db := webService()
			db.Name, db.ContainerName, db.Replaces.ContainerID = "db", "shop-db-1", strings.Repeat("f", 64)
			res := f.client().Deploy(context.Background(), request(webService(), db))
			if res.Outcome != protocol.OutcomeDenied || len(f.calls) != 1 {
				t.Fatalf("%s: %+v calls=%v", name, res, f.steps())
			}
			if res.Steps[0].Outcome != protocol.OutcomeDenied || res.Steps[1].Outcome != protocol.OutcomeSkipped || res.Steps[len(res.Steps)-1].Service != "db" || res.Steps[len(res.Steps)-1].Outcome != protocol.OutcomeSkipped || len(res.Steps) != 14 {
				t.Fatalf("%s steps: %+v", name, res.Steps)
			}
			if !strings.Contains(res.Detail, "service web, step precondition") {
				t.Fatalf("detail: %q", res.Detail)
			}
		})
	}
	f := newFakeDeployEngine(t)
	f.oldStatus = 404
	if res := f.client().Deploy(context.Background(), request(webService())); res.Outcome != protocol.OutcomeDenied || res.Steps[0].Detail != "the container no longer exists" || len(f.calls) != 1 {
		t.Fatalf("missing container: %+v", res)
	}
}

func TestDeployStepFailuresStopTheRun(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*fakeDeployEngine)
		outcome string
		step    string
		calls   int
	}{
		"image missing":  {func(f *fakeDeployEngine) { f.imageStatus = 404 }, protocol.OutcomeFailed, protocol.StepImage, 2},
		"stop refused":   {func(f *fakeDeployEngine) { f.stopStatus = 500 }, protocol.OutcomeFailed, protocol.StepStop, 3},
		"rename conflict": {func(f *fakeDeployEngine) { f.renameStatus = 409 }, protocol.OutcomeFailed, protocol.StepRename, 4},
		"create conflict": {func(f *fakeDeployEngine) { f.createStatus = 409 }, protocol.OutcomeFailed, protocol.StepCreate, 5},
		"start fails":    {func(f *fakeDeployEngine) { f.startStatus = 500 }, protocol.OutcomeFailed, protocol.StepStart, 6},
		"remove fails":   {func(f *fakeDeployEngine) { f.removeStatus = 409 }, protocol.OutcomeFailed, protocol.StepRemove, 8},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			res := f.client().Deploy(context.Background(), request(webService()))
			if res.Outcome != tc.outcome || len(f.calls) != tc.calls {
				t.Fatalf("%s: %+v calls=%v", name, res, f.steps())
			}
			var failing protocol.DeploymentStep
			for _, s := range res.Steps {
				if s.Outcome != protocol.OutcomeSucceeded && s.Outcome != protocol.OutcomeSkipped {
					failing = s
					break
				}
			}
			if failing.Step != tc.step {
				t.Fatalf("%s failed at %+v", name, failing)
			}
			if tc.step == protocol.StepRemove {
				if len(res.Services) != 1 {
					t.Fatal("started service must keep its identity when only removal failed")
				}
			} else if len(res.Services) != 0 {
				t.Fatalf("%s recorded an identity it did not start: %+v", name, res.Services)
			}
			if strings.Contains(res.Detail, "canary") {
				t.Fatal("detail leaked")
			}
		})
	}
	f := newFakeDeployEngine(t)
	f.stopStatus = 304
	if res := f.client().Deploy(context.Background(), request(webService())); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("already stopped must count: %+v", res)
	}
}

func TestDeployTimeAndCancellation(t *testing.T) {
	f := newFakeDeployEngine(t)
	req := request(webService())
	req.Deadline = time.Now().Add(-time.Second)
	if res := f.client().Deploy(context.Background(), req); res.Outcome != protocol.OutcomeDenied || len(f.calls) != 0 {
		t.Fatalf("past deadline: %+v", res)
	}
	f = newFakeDeployEngine(t)
	f.stopDelay = 3 * time.Second
	req = request(webService())
	req.Deadline = time.Now().Add(1500 * time.Millisecond)
	res := f.client().Deploy(context.Background(), req)
	if res.Outcome != protocol.OutcomeTimedOut || res.Steps[2].Outcome != protocol.OutcomeTimedOut || len(res.Services) != 0 {
		t.Fatalf("deadline mid-stop: %+v", res)
	}
	f = newFakeDeployEngine(t)
	f.stopDelay = 3 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(500 * time.Millisecond); cancel() }()
	res = f.client().Deploy(ctx, request(webService()))
	if res.Outcome != protocol.OutcomeUnknown || res.Steps[2].Outcome != protocol.OutcomeUnknown {
		t.Fatalf("cancel mid-stop: %+v", res)
	}
}

func TestDeployTwoServicesSecondFails(t *testing.T) {
	f := newFakeDeployEngine(t)
	db := webService()
	db.Name, db.ContainerName, db.Replaces.ContainerID = "db", "shop-db-1", strings.Repeat("f", 64)
	res := f.client().Deploy(context.Background(), request(webService(), db))
	if res.Outcome != protocol.OutcomeDenied || len(res.Services) != 1 || res.Services[0].Service != "web" {
		t.Fatalf("partial: %+v", res)
	}
	for _, s := range res.Steps[:7] {
		if s.Outcome != protocol.OutcomeSucceeded {
			t.Fatalf("web step: %+v", s)
		}
	}
	if res.Steps[7].Service != "db" || res.Steps[7].Outcome != protocol.OutcomeDenied {
		t.Fatalf("db precondition: %+v", res.Steps[7])
	}
}
```

The second service's container (`f`×64) is unknown to the fake, so its precondition GET is a 404 and is `denied`; that is the intended second-fails path.

- [ ] **Step 2: Run to verify which fail**

Run: `go test ./internal/runtime/docker/ -run 'TestDeploy' -count=1 -v 2>&1 | tail -40`
Expected: any failures point at a classification defect in `deploy.go`; fix the code, not the assertion, unless the assertion contradicts the spec.

- [ ] **Step 3: Run with the race detector**

Run: `go test -race ./internal/runtime/docker/ -run 'TestDeploy' -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/runtime/docker && git commit -m "test: deployment failure semantics against a fake Engine

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Real Docker regression and CI

**Files:**
- Create: `internal/runtime/docker/deploy_integration_test.go`
- Modify: `.github/workflows/ci.yml:90-97` (env var and `-run` pattern)

- [ ] **Step 1: Write the test**

```go
// internal/runtime/docker/deploy_integration_test.go
package docker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// Opt-in, already-present image only. Everything created carries the fixture project label
// and is removed by name in cleanup; no other container is touched.
func TestDeployRealDocker(t *testing.T) {
	image := os.Getenv("KY_TEST_DOCKER_DEPLOY_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_DEPLOY_IMAGE to an existing shell image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	project := "kyyarddeployfixture"
	name := project + "-web-1"
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		ids, _ := exec.CommandContext(cctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).Output()
		for _, id := range strings.Fields(string(ids)) {
			_ = exec.CommandContext(cctx, "docker", "rm", "-fv", id).Run()
		}
	})
	oldID, err := docker("run", "-d", "--pull", "never", "--name", name, "--label", "com.docker.compose.project="+project, "--label", "com.docker.compose.service=web", image, "sh", "-c", "sleep 300")
	if err != nil {
		t.Fatalf("fixture: %v: %s", err, oldID)
	}
	raw, err := docker("inspect", "--format", "{{json .}}", oldID)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Image   string
		Created time.Time
	}
	if err = json.Unmarshal([]byte(raw), &identity); err != nil {
		t.Fatal(err)
	}
	req := protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: "ep_1", Project: project, Revision: 3, Deadline: time.Now().Add(2 * time.Minute), Services: []protocol.DeploymentService{{
		Name: "web", ContainerName: name, ImageID: identity.Image,
		Replaces: protocol.InspectionTarget{ContainerID: oldID, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()},
		Restart: "unless-stopped", Env: map[string]string{"TOKEN": "deploy-secret-canary"},
	}}}
	// The image has no command of its own we can rely on; give the new container one the
	// way the old one had, through env-free means: sleep is what the fixture image runs.
	res := New("/var/run/docker.sock").Deploy(ctx, req)
	serialized, _ := json.Marshal(res)
	if strings.Contains(string(serialized), "deploy-secret-canary") {
		t.Fatal("environment value reached the result")
	}
	if res.Outcome != protocol.OutcomeSucceeded || len(res.Services) != 1 {
		t.Fatalf("deploy: %s", serialized)
	}
	raw, err = docker("inspect", "--format", "{{json .}}", res.Services[0].ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Name   string
		Image  string
		Config struct {
			Env    []string
			Labels map[string]string
		}
		HostConfig struct {
			RestartPolicy struct{ Name string }
		}
	}
	if err = json.Unmarshal([]byte(raw), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "/"+name || created.Image != identity.Image || created.HostConfig.RestartPolicy.Name != "unless-stopped" || created.Config.Labels["com.docker.compose.project"] != project || created.Config.Labels["kyyard.revision"] != "3" || !slices.Contains(created.Config.Env, "TOKEN=deploy-secret-canary") {
		t.Fatalf("new container: %s", raw)
	}
	if _, err = docker("inspect", oldID); err == nil {
		t.Fatal("old container survived removal")
	}
}
```

Note on the new container's command: `POST /containers/create` without `Cmd` uses the image's default command. `alpine` has `/bin/sh` as its default, which exits immediately, so a started container may exit before inspection and `State` is not asserted here; the identity read still succeeds. If the adapter's start step fails for an exited container in practice, that is a real finding to report, not to paper over. The CI image is `alpine:3.24`.

- [ ] **Step 2: Run it locally**

Run: `KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24 go test -race ./internal/runtime/docker -run '^TestDeployRealDocker$' -count=1 -v` (pull the image first with `docker pull alpine:3.24` if absent).
Expected: PASS, and `docker ps -a --filter label=com.docker.compose.project=kyyarddeploy*` shows nothing afterwards.

- [ ] **Step 3: Add the CI step**

In `.github/workflows/ci.yml` "Real Docker runtime regressions": add `KY_TEST_DOCKER_DEPLOY_IMAGE: alpine:3.24` to `env` and change the first `go test` line to `-run '^Test(Exec|Inspection|Deploy)RealDocker$'`.

- [ ] **Step 4: Commit**

```bash
git add internal/runtime/docker/deploy_integration_test.go .github/workflows/ci.yml && git commit -m "test: deployment replacement against real Docker in CI

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Docs, DOX pass, full CI

**Files:**
- Modify: `internal/runtime/AGENTS.md` (add a `Deploy` bullet beside the `InspectContainer` bullet)
- Modify: `docs/agent-protocol.md` (new `## Deployment apply` section after `## Container inspection`)
- Modify: `docs/application-schema.md:103-108` (Deploy section step 3 lists the implemented sequence)
- Modify: `KyYard-Implementation-Plan.md:236-238` (add "Implemented M6 deployment runtime" paragraph; "Next M6 slice" becomes PR B)

- [ ] **Step 1: Runtime AGENTS bullet**

> - `Client.Deploy` replaces each service's mapped container in plan order: precondition (identity, no mounts, default/bridge network, not privileged; otherwise `denied`), image present by pinned ID (`failed` when absent; never pulls), stop `t=10` (304 counts), rename the old container to `<name>.kyyard-prev-<deployment[:8]>`, create by image ID with the definition's env, ports, restart policy and Compose labels, start and read the new identity, remove the old container without `v=1`. The first non-success ends the run; later steps are `skipped`; nothing is rolled back. A cancelled session is `unknown`, a deadline is `timed_out`, Engine text never enters a result. Only tests call it until the apply transport lands.

- [ ] **Step 2: Protocol section**

> ## Deployment apply
>
> `deployment.apply` (server → agent, at most 192 KiB) carries one `DeploymentRequest`: deployment ID, endpoint, project, revision, deadline (at most 15 minutes) and the services to replace, each with the pinned image ID, the container identity it replaces, restart policy, ports and resolved environment values. The values exist only in that frame and in the agent's memory; the agent never writes them to its ledger, results, events or logs. `deployment.result` (agent → server, at most 64 KiB) carries the outcome, a fixed-text detail, every step attempted with its outcome, and the identity of every new container that started. Capability `deployment.apply` gates dispatch. Status: types and the Docker adapter are implemented; the transport, capability advertisement and server route land with the apply slice.

- [ ] **Step 3: Schema and plan documents**

Replace `docs/application-schema.md` Deploy step 3 with:

> 3. Each service is replaced in order: precondition (identity, mounts, network, privilege), image present, stop, rename, create, start, remove. Every step records `succeeded`, `failed`, `denied`, `timed_out`, `unknown` or `skipped`. A partial failure leaves the recorded per-step outcome, the renamed stopped previous container, and the prior revision's references; nothing is auto-rolled back. Network and volume creation, pulls and health waits are not part of the supported subset.

In `KyYard-Implementation-Plan.md` after the deployment plans paragraph:

> Implemented M6 deployment runtime: `deployment.apply`/`deployment.result` wire types with bounds, and `docker.Client.Deploy` replacing mapped containers natively through the Engine API with preconditions, per-step outcomes, no pull, no volume access and no rollback, proven against a fake Engine and real Docker in CI. Not reachable from the server yet.
>
> Next M6 slice: the apply transport (agent frame handling and ledger, capability, migration for deployment states and events, apply route with secret resolution, resource rebinding and `current_revision`, UI apply and status), then history, reapply and remove.

- [ ] **Step 4: Full CI**

Run: `gofmt -l internal cmd` (no output) then `make ci`.
Expected: `==> Local CI checks passed`.

- [ ] **Step 5: Commit and push**

```bash
git add internal/runtime/AGENTS.md docs KyYard-Implementation-Plan.md && git commit -m "docs: deployment runtime contract

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>" && git push -u origin feat/deployment-apply
```
