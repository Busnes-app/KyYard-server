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
	oldID        = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
	newID        = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	oldImage     = "sha256:d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
	newImage     = "sha256:e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4"
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
			NetworkMode   string
			PortBindings  map[string][]struct{ HostIp, HostPort string }
			RestartPolicy struct {
				Name              string
				MaximumRetryCount int
			}
		}
		NetworkingConfig *struct {
			EndpointsConfig map[string]struct{ Aliases []string }
		}
	}
	if err := json.Unmarshal([]byte(f.calls[4].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.HostConfig.NetworkMode != "" || body.NetworkingConfig != nil {
		t.Fatalf("networking on default mode: %+v", body)
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

func TestDeployKeepsTheProjectNetwork(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.oldContainer["HostConfig"].(map[string]any)["NetworkMode"] = "shop_default"
	res := f.client().Deploy(context.Background(), request(webService()))
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("outcome: %+v", res)
	}
	var body struct {
		HostConfig struct {
			NetworkMode string
		}
		NetworkingConfig struct {
			EndpointsConfig map[string]struct{ Aliases []string }
		}
	}
	if err := json.Unmarshal([]byte(f.calls[4].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.HostConfig.NetworkMode != "shop_default" {
		t.Fatalf("network mode: %+v", body)
	}
	if aliases := body.NetworkingConfig.EndpointsConfig["shop_default"].Aliases; len(aliases) != 1 || aliases[0] != "web" {
		t.Fatalf("aliases: %+v", body)
	}
}

func TestDeployPreconditionsRefuseBeforeTouchingAnything(t *testing.T) {
	for name, mutate := range map[string]func(*fakeDeployEngine){
		"image":   func(f *fakeDeployEngine) { f.oldContainer["Image"] = newImage },
		"created": func(f *fakeDeployEngine) { f.oldContainer["Created"] = "2023-11-14T22:13:21Z" },
		"mounts":  func(f *fakeDeployEngine) { f.oldContainer["Mounts"] = []any{map[string]any{"Type": "volume"}} },
		"network": func(f *fakeDeployEngine) { f.oldContainer["HostConfig"] = map[string]any{"NetworkMode": "host"} },
		"privileged": func(f *fakeDeployEngine) {
			f.oldContainer["HostConfig"] = map[string]any{"NetworkMode": "bridge", "Privileged": true}
		},
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
		"image missing":   {func(f *fakeDeployEngine) { f.imageStatus = 404 }, protocol.OutcomeFailed, protocol.StepImage, 2},
		"stop refused":    {func(f *fakeDeployEngine) { f.stopStatus = 500 }, protocol.OutcomeFailed, protocol.StepStop, 3},
		"rename conflict": {func(f *fakeDeployEngine) { f.renameStatus = 409 }, protocol.OutcomeFailed, protocol.StepRename, 4},
		"create conflict": {func(f *fakeDeployEngine) { f.createStatus = 409 }, protocol.OutcomeFailed, protocol.StepCreate, 5},
		"start fails":     {func(f *fakeDeployEngine) { f.startStatus = 500 }, protocol.OutcomeFailed, protocol.StepStart, 6},
		"remove fails":    {func(f *fakeDeployEngine) { f.removeStatus = 409 }, protocol.OutcomeFailed, protocol.StepRemove, 8},
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
