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

func TestDeployPreconditionRefusesHostNetworkMode(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.oldContainer["HostConfig"].(map[string]any)["NetworkMode"] = "host"
	res := f.client().Deploy(context.Background(), request(webService()))
	if res.Outcome != protocol.OutcomeDenied {
		t.Fatalf("outcome: %+v", res)
	}
	if len(res.Steps) == 0 || res.Steps[0].Step != protocol.StepPrecondition || res.Steps[0].Outcome != protocol.OutcomeDenied {
		t.Fatalf("step: %+v", res.Steps)
	}
}
