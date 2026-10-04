package docker_test

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
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
	otherOldID   = "a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6"
	pullDigest   = "sha256:a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5"
	requestID    = "0123456789abcdef0123456789abcdef"
)

type engineCall struct{ Method, Path, Query, Body string }

// fakeDeployEngine answers the nine calls one service needs. Knobs make single steps fail so
// each classification is proven by one test.
type fakeDeployEngine struct {
	mu               sync.Mutex
	calls            []engineCall
	oldContainer     map[string]any // returned by GET /containers/{old}/json
	oldStatus        int            // 200 default; 404 = the container is gone
	oldImageStatus   int            // GET /images/{old}/json; 200 default
	oldImageConfig   map[string]any // its Config; Cmd and Entrypoint equal the old container's by default
	imageStatus      int            // GET /images/{new}/json; 200 default
	inspectNewStatus int            // GET /containers/{new}/json; 200 default
	imageReportedID  string         // the Id GET /images/{new}/json reports; newImage default
	createdID        string         // the Id POST /containers/create answers; newID default
	startedImage     string         // the Image GET /containers/{new}/json reports; newImage default
	defaultRuntime   string         // GET /info DefaultRuntime; "runc" default
	cgroupVersion    string         // GET /info CgroupVersion; "2" default
	stopStatus       int            // 204 default; 304 allowed
	stopDelay        time.Duration
	renameDelay      time.Duration // before the parking rename answers; a rename back is not delayed
	renameStatus     int
	renameBackStatus int           // a rename giving the old container its name back; renameStatus when 0
	pauseStatus      int           // POST /containers/{old}/pause, the rollback's re-pause; 204 when 0
	waitStatus       int           // POST /containers/{old}/wait; 200 when 0
	waitDelay        time.Duration // after the headers, before the body
	waitHeaderDelay  time.Duration // before the headers
	waitBody         string        // the body once the wait ends; a stopped container by default
	createStatus     int           // 201 default
	createDelay      time.Duration // before POST /containers/create answers
	discardDelay     time.Duration // before the new container's DELETE answers
	restoreDelay     time.Duration // before a rename back or the old container's start answers
	nameStatus       int           // GET /containers/adhoc/json, a run's container read by name; 404 when 0
	nameState        string        // its State
	nameImage        string        // its Image; newImage when ""
	nameID           string        // its Id; newID when ""
	nameName         string        // its Name; "/adhoc" when ""
	startStatus      int
	removeStatus     int
	oldStartStatus   int      // POST /containers/{old}/start, the rollback's restart; 204 default
	removeNewStatus  int      // DELETE /containers/{new}, the rollback's removal; 204 default
	connectStatus    int      // POST /networks/{name}/connect; 200 default
	onStart          func()   // runs on the handler before POST /containers/{new}/start answers
	newStates        []string // State of each GET /containers/{new}/json in turn, the last repeating; running default
	newReads         int
	newRestarts      int                                            // RestartCount of GET /containers/{new}/json
	pullStatus       int                                            // POST /images/create; 200 default
	pullStatusFor    map[string]int                                 // per fromImage, overriding pullStatus
	pullBody         string                                         // its progress stream
	pullAuth         []string                                       // X-Registry-Auth of each pull, "" when absent
	pulledStatus     int                                            // GET /images/{host%2Frepo@digest}/json; 200 default
	pulled           map[string]any                                 // its body
	tagStatus        int                                            // POST /images/{pulled}/tag; 201 default
	volumeStatus     int                                            // POST /volumes/create; 201 default, as Docker answers for an existing name too
	volumes          map[string]any                                 // existing volumes by name: GET /volumes/{name} answers 200 with it, else 404
	createdInstead   map[string]any                                 // what POST /volumes/create answers with, as if another client created the name first
	otherMounts      []any                                          // the second fixture container's Mounts; the first's when nil
	reads            map[string]int                                 // GETs of each old container so far
	drift            func(id string, read int, body map[string]any) // edits a copy of what the read-th GET (from 1) of an old container answers
	srv              *httptest.Server
}

func newFakeDeployEngine(t *testing.T) *fakeDeployEngine {
	t.Helper()
	f := &fakeDeployEngine{oldStatus: 200, oldImageStatus: 200, imageStatus: 200, inspectNewStatus: 200, defaultRuntime: "runc", stopStatus: 204, renameStatus: 204, createStatus: 201, startStatus: 204, removeStatus: 204, oldStartStatus: 204, removeNewStatus: 204, connectStatus: 200, pullStatus: 200, pulledStatus: 200, tagStatus: 201, volumeStatus: 201, reads: map[string]int{},
		imageReportedID: newImage, createdID: newID, startedImage: newImage, cgroupVersion: "2",
		pullBody: `{"status":"Pulling from org/app"}` + "\n" + `{"status":"Digest: ` + pullDigest + `"}` + "\n",
		pulled:   map[string]any{"Id": newImage, "RepoDigests": []string{"ghcr.io/org/app@" + pullDigest}}}
	f.oldContainer = map[string]any{"Id": oldID, "Image": oldImage, "Name": "/shop-web-1", "Created": "2023-11-14T22:13:20Z", "Mounts": []any{},
		"Config": map[string]any{"Cmd": []string{"nginx", "-g", "daemon off;"}, "Entrypoint": nil, "User": "", "Healthcheck": nil, "WorkingDir": "", "StopSignal": ""},
		"HostConfig": map[string]any{"NetworkMode": "default", "Privileged": false, "AutoRemove": false, "ReadonlyRootfs": false, "Tmpfs": nil, "CapAdd": nil, "CapDrop": nil, "SecurityOpt": nil, "Devices": []any{}, "PidMode": "", "IpcMode": "private",
			"Runtime": "runc", "Memory": 0, "MemorySwap": 0, "MemoryReservation": 0, "NanoCpus": 0, "CpuShares": 0, "CpuQuota": 0, "CpusetCpus": "", "PidsLimit": nil, "Ulimits": nil, "DeviceRequests": nil,
			"UsernsMode": "", "CgroupParent": "", "GroupAdd": nil, "ExtraHosts": nil, "Dns": nil, "DnsOptions": []string{}, "DnsSearch": []string{}, "Links": nil, "LogConfig": map[string]any{"Type": "json-file", "Config": map[string]any{}}},
		"NetworkSettings": map[string]any{"Networks": map[string]any{"bridge": map[string]any{}}}}
	f.oldImageConfig = map[string]any{"Cmd": []string{"nginx", "-g", "daemon off;"}, "Entrypoint": nil, "Healthcheck": nil, "WorkingDir": "", "StopSignal": ""}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, engineCall{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(body)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.EscapedPath()
		switch {
		case r.Method == "GET" && strings.HasSuffix(p, "/info"):
			_ = json.NewEncoder(w).Encode(map[string]any{"DefaultRuntime": f.defaultRuntime, "CgroupVersion": f.cgroupVersion})
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+oldID+"/json"):
			body := f.oldRead(oldID, f.oldContainer)
			w.WriteHeader(f.oldStatus)
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+otherOldID+"/json"):
			other := cloneJSON(f.oldContainer)
			other["Id"], other["Name"] = otherOldID, "/shop-db-1"
			if f.otherMounts != nil {
				other["Mounts"] = f.otherMounts
			}
			_ = json.NewEncoder(w).Encode(f.oldRead(otherOldID, other))
		case r.Method == "GET" && strings.HasSuffix(p, "/images/"+oldImage+"/json"):
			w.WriteHeader(f.oldImageStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": oldImage, "Config": f.oldImageConfig})
		case r.Method == "GET" && strings.HasSuffix(p, "/images/"+newImage+"/json"):
			w.WriteHeader(f.imageStatus)
			_, _ = w.Write([]byte(`{"Id":"` + f.imageReportedID + `"}`))
		case r.Method == "POST" && strings.HasSuffix(p, "/images/create"):
			f.mu.Lock()
			f.pullAuth = append(f.pullAuth, r.Header.Get("X-Registry-Auth"))
			f.mu.Unlock()
			if status, ok := f.pullStatusFor[r.URL.Query().Get("fromImage")]; ok {
				w.WriteHeader(status)
				return
			}
			w.WriteHeader(f.pullStatus)
			_, _ = w.Write([]byte(f.pullBody))
		case r.Method == "GET" && strings.Contains(p, "%2F") && strings.HasSuffix(p, "@"+pullDigest+"/json"):
			w.WriteHeader(f.pulledStatus)
			_ = json.NewEncoder(w).Encode(f.pulled)
		case r.Method == "GET" && strings.Contains(p, "/volumes/"):
			v, ok := f.volumes[p[strings.LastIndex(p, "/")+1:]]
			if !ok {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(v)
		case r.Method == "POST" && strings.HasSuffix(p, "/volumes/create"):
			// Docker answers 201 with the existing volume when the name is taken.
			var in struct {
				Name   string
				Labels map[string]string
			}
			_ = json.Unmarshal(body, &in)
			v, ok := f.volumes[in.Name]
			if !ok {
				v = map[string]any{"Name": in.Name, "Driver": "local", "Labels": in.Labels, "Options": nil}
			}
			if f.createdInstead != nil {
				v = f.createdInstead
			}
			w.WriteHeader(f.volumeStatus)
			_ = json.NewEncoder(w).Encode(v)
		case r.Method == "POST" && strings.HasSuffix(p, "/images/"+newImage+"/tag"):
			w.WriteHeader(f.tagStatus)
		case r.Method == "POST" && (strings.HasSuffix(p, "/containers/"+oldID+"/stop") || strings.HasSuffix(p, "/containers/"+otherOldID+"/stop")):
			time.Sleep(f.stopDelay)
			w.WriteHeader(f.stopStatus)
		case r.Method == "POST" && (strings.HasSuffix(p, "/containers/"+oldID+"/rename") || strings.HasSuffix(p, "/containers/"+otherOldID+"/rename")):
			if strings.Contains(r.URL.RawQuery, "kyyard-prev") {
				time.Sleep(f.renameDelay)
			} else {
				time.Sleep(f.restoreDelay)
			}
			if f.renameBackStatus != 0 && r.URL.RawQuery == "name=shop-web-1" {
				w.WriteHeader(f.renameBackStatus)
				return
			}
			w.WriteHeader(f.renameStatus)
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/"+oldID+"/wait"):
			// As v1.41 does: headers at once, the body only when the container has stopped.
			time.Sleep(f.waitHeaderDelay)
			w.WriteHeader(cmp.Or(f.waitStatus, 200))
			w.(http.Flusher).Flush()
			time.Sleep(f.waitDelay)
			_, _ = w.Write([]byte(cmp.Or(f.waitBody, `{"StatusCode":0,"Error":null}`)))
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/"+oldID+"/pause"):
			w.WriteHeader(cmp.Or(f.pauseStatus, 204))
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/create"):
			time.Sleep(f.createDelay)
			w.WriteHeader(f.createStatus)
			_, _ = w.Write([]byte(`{"Id":"` + f.createdID + `","Warnings":[]}`))
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/"+newID+"/start"):
			if f.onStart != nil {
				f.onStart()
			}
			w.WriteHeader(f.startStatus)
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/"+oldID+"/start"):
			time.Sleep(f.restoreDelay)
			w.WriteHeader(f.oldStartStatus)
		case r.Method == "DELETE" && strings.HasSuffix(p, "/containers/"+newID):
			time.Sleep(f.discardDelay)
			w.WriteHeader(f.removeNewStatus)
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/adhoc/json"):
			w.WriteHeader(cmp.Or(f.nameStatus, 404))
			_, _ = w.Write([]byte(`{"Id":"` + cmp.Or(f.nameID, newID) + `","Image":"` + cmp.Or(f.nameImage, newImage) + `","Name":"` + cmp.Or(f.nameName, "/adhoc") + `","State":` + cmp.Or(f.nameState, created) + `}`))
		case r.Method == "POST" && strings.Contains(p, "/networks/") && strings.HasSuffix(p, "/connect"):
			w.WriteHeader(f.connectStatus)
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+newID+"/json"):
			f.mu.Lock()
			state := `{"Status":"running","Running":true}`
			if n := len(f.newStates); n > 0 {
				state = f.newStates[min(f.newReads, n-1)]
			}
			f.newReads++
			f.mu.Unlock()
			w.WriteHeader(f.inspectNewStatus)
			_, _ = w.Write([]byte(`{"Id":"` + newID + `","Image":"` + f.startedImage + `","Created":"2024-01-01T00:00:01Z","Name":"/shop-web-1","RestartCount":` + strconv.Itoa(f.newRestarts) + `,"State":` + state + `}`))
		case r.Method == "DELETE" && (strings.HasSuffix(p, "/containers/"+oldID) || strings.HasSuffix(p, "/containers/"+otherOldID)):
			w.WriteHeader(f.removeStatus)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// client watches a started container for 50ms, not the production five seconds, and waits 200ms
// for a stop that went unanswered to finish.
func (f *fakeDeployEngine) client() *docker.Client {
	return docker.NewHTTP(f.srv.Client(), f.srv.URL).WatchStartFor(50*time.Millisecond, 5*time.Millisecond).WaitStopFor(200 * time.Millisecond)
}

// oldRead counts a GET of an old container and lets drift edit a copy of its body. drift runs on
// the handler goroutine before the status is written, so it may also set oldStatus.
func (f *fakeDeployEngine) oldRead(id string, body map[string]any) map[string]any {
	f.mu.Lock()
	f.reads[id]++
	n, drift := f.reads[id], f.drift
	f.mu.Unlock()
	body = cloneJSON(body)
	if drift != nil {
		drift(id, n, body)
	}
	return body
}

func cloneJSON(m map[string]any) map[string]any {
	raw, _ := json.Marshal(m)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// dbService is a second service on otherOldID, so both services can succeed against the fake.
func dbService() protocol.DeploymentService {
	db := webService()
	db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", otherOldID, nil
	return db
}
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
	return protocol.DeploymentRequest{Deployment: deploymentID, RequestID: requestID, Endpoint: "ep_1", Project: "shop", Revision: 2, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Services: services}
}
func webService() protocol.DeploymentService {
	return protocol.DeploymentService{Name: "web", ContainerName: "shop-web-1", ImageID: newImage, Replaces: protocol.InspectionTarget{ContainerID: oldID, ImageID: oldImage, CreatedUnix: 1700000000}, Restart: "on-failure", Ports: []protocol.Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}}, Env: map[string]string{"TOKEN": "a=b\ncanary-secret", "A": "1"}, Mounts: []protocol.Mount{}}
}

// stepText is a step's code with its parameter, the way the tables below spell them.
func stepText(s protocol.DeploymentStep) string {
	if s.Detail == "" {
		return s.Code
	}
	return s.Code + ": " + s.Detail
}

func TestDeployReplacesOneServiceInOrder(t *testing.T) {
	f := newFakeDeployEngine(t)
	res := f.client().Deploy(context.Background(), request(webService()), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Deployment != deploymentID || res.RequestID != requestID || res.Code != "" {
		t.Fatalf("outcome: %+v", res)
	}
	want := []string{"GET /info", "GET /containers/" + oldID + "/json", "GET /images/" + oldImage + "/json", "GET /images/" + newImage + "/json", "GET /containers/" + oldID + "/json", "POST /containers/" + oldID + "/rename", "POST /containers/create", "POST /containers/" + oldID + "/stop", "POST /containers/" + newID + "/start", "GET /containers/" + newID + "/json", "DELETE /containers/" + oldID}
	if got := f.steps(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("call order:\n got %v\nwant %v", got, want)
	}
	if f.calls[5].Query != "name=shop-web-1.kyyard-prev-3f2b1c9e" || f.calls[6].Query != "name=shop-web-1" || f.calls[7].Query != "t=10" || f.calls[10].Query != "" {
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
	if err := json.Unmarshal([]byte(f.calls[6].Body), &body); err != nil {
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
	if len(res.Steps) != 8 || len(res.Services) != 1 || res.Services[0].ContainerID != newID || res.Services[0].ImageID != newImage || res.Services[0].CreatedUnix != 1704067201 {
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

// A frame Validate refuses runs nothing and is answered invalid_request; a request ID that is not
// valid is not echoed.
func TestDeployRefusesAnInvalidRequestWithoutCalling(t *testing.T) {
	f := newFakeDeployEngine(t)
	s := webService()
	s.Env = map[string]string{"1BAD": "x"}
	res := f.client().Deploy(context.Background(), request(s), func() {})
	if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || res.RequestID != requestID || len(f.calls) != 0 || len(res.Steps) != 0 || res.Validate() != nil {
		t.Fatalf("invalid request: %+v calls=%d", res, len(f.calls))
	}
	req := request(webService())
	req.RequestID = "a b\ninjected"
	res = f.client().Deploy(context.Background(), req, func() {})
	if res.Code != protocol.ResultInvalidRequest || res.RequestID != "" || len(f.calls) != 0 || res.Validate() != nil {
		t.Fatalf("bad request id: %+v", res)
	}
}

func TestDeployKeepsTheProjectNetwork(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.oldContainer["HostConfig"].(map[string]any)["NetworkMode"] = "shop_default"
	f.oldContainer["NetworkSettings"] = map[string]any{"Networks": map[string]any{"shop_default": map[string]any{}}}
	res := f.client().Deploy(context.Background(), request(webService()), func() {})
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
	if err := json.Unmarshal([]byte(f.calls[6].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.HostConfig.NetworkMode != "shop_default" {
		t.Fatalf("network mode: %+v", body)
	}
	if aliases := body.NetworkingConfig.EndpointsConfig["shop_default"].Aliases; len(aliases) != 1 || aliases[0] != "web" {
		t.Fatalf("aliases: %+v", body)
	}
}

// Settings the container inherited from its image are reproduced by recreation from it.
func TestDeployAcceptsImageInheritedSettings(t *testing.T) {
	f := newFakeDeployEngine(t)
	inherited := map[string]any{"Healthcheck": map[string]any{"Test": []string{"CMD", "true"}, "Interval": 30000000000}, "WorkingDir": "/srv", "StopSignal": "SIGQUIT"}
	for k, v := range inherited {
		f.oldContainer["Config"].(map[string]any)[k] = v
		f.oldImageConfig[k] = v
	}
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("inherited settings: %+v", res)
	}
}

// "NONE", no test and no healthcheck all mean the same: none.
func TestDeployAcceptsDisabledHealthcheckOnImageWithout(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.oldContainer["Config"].(map[string]any)["Healthcheck"] = map[string]any{"Test": []string{"NONE"}}
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("NONE healthcheck: %+v", res)
	}
}

// The runtime is compared with the daemon's default, not a fixed name.
func TestDeployRuntimeFollowsTheDaemonDefault(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.defaultRuntime = "nvidia"
	f.oldContainer["HostConfig"].(map[string]any)["Runtime"] = "nvidia"
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("daemon-default runtime: %+v", res)
	}
	f = newFakeDeployEngine(t)
	f.defaultRuntime = ""
	db := webService()
	db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", strings.Repeat("f", 64), nil
	res := f.client().Deploy(context.Background(), request(webService(), db), func() {})
	if res.Outcome != protocol.OutcomeFailed || res.Steps[0].Step != protocol.StepPrecondition || res.Steps[0].Code != "runtime_unreadable" || res.Steps[len(res.Steps)-1].Outcome != protocol.OutcomeSkipped || len(f.calls) != 1 {
		t.Fatalf("unreadable default runtime: %+v calls=%v", res, f.steps())
	}
}

// Daemon-default IPC modes are reproduced by recreation on the same daemon.
func TestDeployAcceptsDaemonDefaultIPCModes(t *testing.T) {
	for _, mode := range []string{"", "private", "shareable"} {
		f := newFakeDeployEngine(t)
		f.oldContainer["HostConfig"].(map[string]any)["IpcMode"] = mode
		if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeSucceeded {
			t.Fatalf("IpcMode %q: %+v", mode, res)
		}
	}
}

func TestDeployPreconditionsRefuseBeforeTouchingAnything(t *testing.T) {
	host := func(k string, v any) func(*fakeDeployEngine) {
		return func(f *fakeDeployEngine) { f.oldContainer["HostConfig"].(map[string]any)[k] = v }
	}
	unset := func(k string) func(*fakeDeployEngine) {
		return func(f *fakeDeployEngine) { delete(f.oldContainer, k) }
	}
	networks := func(names ...string) func(*fakeDeployEngine) {
		return func(f *fakeDeployEngine) {
			n := map[string]any{}
			for _, name := range names {
				n[name] = map[string]any{}
			}
			f.oldContainer["NetworkSettings"] = map[string]any{"Networks": n}
		}
	}
	const (
		notTheOne  = "identity_mismatch"
		unreported = "configuration_unreported"
		imageGone  = "image_missing"
	)
	for name, tc := range map[string]struct {
		mutate func(*fakeDeployEngine)
		calls  int // after GET /info: 1 when decided from the container alone, 2 when the old image was read
		detail string
	}{
		"image":                  {func(f *fakeDeployEngine) { f.oldContainer["Image"] = newImage }, 1, notTheOne},
		"created":                {func(f *fakeDeployEngine) { f.oldContainer["Created"] = "2023-11-14T22:13:21Z" }, 1, notTheOne},
		"absent HostConfig":      {unset("HostConfig"), 1, unreported},
		"absent Config":          {unset("Config"), 1, unreported},
		"absent NetworkSettings": {unset("NetworkSettings"), 1, unreported},
		"absent Mounts":          {unset("Mounts"), 1, unreported},
		"absent Privileged": {func(f *fakeDeployEngine) {
			delete(f.oldContainer["HostConfig"].(map[string]any), "Privileged")
		}, 1, unreported},
		"tmpfs mount": {func(f *fakeDeployEngine) {
			f.oldContainer["Mounts"] = []any{map[string]any{"Type": "tmpfs", "Destination": "/run"}}
		}, 1, "unsupported: mount_type"},
		"npipe mount": {func(f *fakeDeployEngine) { f.oldContainer["Mounts"] = []any{map[string]any{"Type": "npipe"}} }, 1, "unsupported: mount_type"},
		"anonymous volume": {func(f *fakeDeployEngine) {
			f.oldContainer["Mounts"] = []any{map[string]any{"Type": "volume", "Name": strings.Repeat("9f", 32), "Destination": "/var/lib/postgresql/data", "RW": true}}
		}, 1, "unsupported: anonymous_volume"},
		"volumes from":  {host("VolumesFrom", []string{"shop-data-1"}), 1, "unsupported: volumes_from"},
		"volume driver": {host("VolumeDriver", "nfs"), 1, "unsupported: volume_driver"},
		"bind propagation": {func(f *fakeDeployEngine) {
			f.oldContainer["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/srv", "Destination": "/srv", "RW": true, "Propagation": "rshared"}}
		}, 1, "unsupported: mount_options"},
		"volume nocopy mode": {func(f *fakeDeployEngine) {
			f.oldContainer["Mounts"] = []any{map[string]any{"Type": "volume", "Name": "shop_data", "Destination": "/data", "RW": true, "Mode": "nocopy"}}
		}, 1, "unsupported: mount_options"},
		"volume subpath":       {host("Mounts", []any{map[string]any{"Type": "volume", "Source": "shop_data", "Target": "/data", "VolumeOptions": map[string]any{"Subpath": "app"}}}), 1, "unsupported: mount_options"},
		"volume nocopy":        {host("Mounts", []any{map[string]any{"Type": "volume", "Source": "shop_data", "Target": "/data", "VolumeOptions": map[string]any{"NoCopy": true}}}), 1, "unsupported: mount_options"},
		"volume driver config": {host("Mounts", []any{map[string]any{"Type": "volume", "Source": "shop_data", "Target": "/data", "VolumeOptions": map[string]any{"DriverConfig": map[string]any{"Name": "nfs"}}}}), 1, "unsupported: mount_options"},
		"bind non-recursive":   {host("Mounts", []any{map[string]any{"Type": "bind", "Source": "/srv", "Target": "/srv", "BindOptions": map[string]any{"NonRecursive": true}}}), 1, "unsupported: mount_options"},
		"bind api propagation": {host("Mounts", []any{map[string]any{"Type": "bind", "Source": "/srv", "Target": "/srv", "BindOptions": map[string]any{"Propagation": "rslave"}}}), 1, "unsupported: mount_options"},
		"tmpfs":                {host("Tmpfs", map[string]string{"/run": "rw"}), 1, "unsupported: tmpfs"},
		"auto-remove":          {host("AutoRemove", true), 1, "unsupported: auto_remove"},
		"read-only root":       {host("ReadonlyRootfs", true), 1, "unsupported: read_only_rootfs"},
		"privileged":           {host("Privileged", true), 1, "unsupported: privileged"},
		"cap add":              {host("CapAdd", []string{"NET_ADMIN"}), 1, "unsupported: capabilities"},
		"cap drop":             {host("CapDrop", []string{"ALL"}), 1, "unsupported: capabilities"},
		"security opt":         {host("SecurityOpt", []string{"no-new-privileges"}), 1, "unsupported: security_opt"},
		"devices":              {host("Devices", []any{map[string]any{"PathOnHost": "/dev/fuse"}}), 1, "unsupported: devices"},
		"pid mode":             {host("PidMode", "host"), 1, "unsupported: pid_mode"},
		"ipc mode host":        {host("IpcMode", "host"), 1, "unsupported: ipc_mode"},
		"ipc container":        {host("IpcMode", "container:"+oldID), 1, "unsupported: ipc_mode"},
		"user":                 {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["User"] = "1000" }, 1, "unsupported: user"},
		"network mode":         {host("NetworkMode", "host"), 1, "unsupported: network"},
		"two networks":         {networks("bridge", "shop_default"), 1, "unsupported: network"},
		"other network":        {networks("shop_default"), 1, "unsupported: network"},
		"cmd":                  {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["Cmd"] = []string{"sleep", "300"} }, 2, "unsupported: image_config"},
		"entrypoint": {func(f *fakeDeployEngine) {
			f.oldContainer["Config"].(map[string]any)["Entrypoint"] = []string{"/bin/sh", "-c"}
		}, 2, "unsupported: image_config"},
		"old image gone":     {func(f *fakeDeployEngine) { f.oldImageStatus = 404 }, 2, imageGone},
		"runtime":            {host("Runtime", "runsc"), 1, "unsupported: runtime"},
		"memory":             {host("Memory", 1<<30), 1, "unsupported: resource_limits"},
		"memory swap":        {host("MemorySwap", 1<<30), 1, "unsupported: resource_limits"},
		"memory reservation": {host("MemoryReservation", 1<<30), 1, "unsupported: resource_limits"},
		"nano cpus":          {host("NanoCpus", 500000000), 1, "unsupported: resource_limits"},
		"cpu shares":         {host("CpuShares", 512), 1, "unsupported: resource_limits"},
		"cpu quota":          {host("CpuQuota", 50000), 1, "unsupported: resource_limits"},
		"cpuset":             {host("CpusetCpus", "0"), 1, "unsupported: resource_limits"},
		"pids":               {host("PidsLimit", 100), 1, "unsupported: resource_limits"},
		"ulimits":            {host("Ulimits", []any{map[string]any{"Name": "nofile", "Soft": 1024, "Hard": 1024}}), 1, "unsupported: ulimits"},
		"sysctls":            {host("Sysctls", map[string]string{"net.ipv4.ip_forward": "1"}), 1, "unsupported: sysctls"},
		"device requests":    {host("DeviceRequests", []any{map[string]any{"Driver": "nvidia", "Count": -1}}), 1, "unsupported: device_requests"},
		"init":               {host("Init", true), 1, "unsupported: init"},
		"userns":             {host("UsernsMode", "host"), 1, "unsupported: userns_mode"},
		"cgroup parent":      {host("CgroupParent", "/custom"), 1, "unsupported: cgroup_parent"},
		"group add":          {host("GroupAdd", []string{"audio"}), 1, "unsupported: group_add"},
		"extra hosts":        {host("ExtraHosts", []string{"db:10.0.0.2"}), 1, "unsupported: extra_hosts"},
		"dns":                {host("Dns", []string{"1.1.1.1"}), 1, "unsupported: dns"},
		"dns options":        {host("DnsOptions", []string{"ndots:1"}), 1, "unsupported: dns"},
		"dns search":         {host("DnsSearch", []string{"lan"}), 1, "unsupported: dns"},
		"links":              {host("Links", []string{"/shop-db-1:/shop-web-1/db"}), 1, "unsupported: links"},
		"healthcheck differs": {func(f *fakeDeployEngine) {
			f.oldContainer["Config"].(map[string]any)["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}}
		}, 2, "unsupported: image_config"},
		"working dir differs": {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["WorkingDir"] = "/srv" }, 2, "unsupported: image_config"},
		"healthcheck interval differs": {func(f *fakeDeployEngine) {
			f.oldImageConfig["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}, "Interval": 30000000000}
			f.oldContainer["Config"].(map[string]any)["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}, "Interval": 5000000000}
		}, 2, "unsupported: image_config"},
		"runtime not the daemon default": {func(f *fakeDeployEngine) {
			f.defaultRuntime = "nvidia"
			f.oldContainer["HostConfig"].(map[string]any)["Runtime"] = "runsc"
		}, 1, "unsupported: runtime"},
		"stop signal differs": {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["StopSignal"] = "SIGINT" }, 2, "unsupported: image_config"},
		"privileged with devices": {func(f *fakeDeployEngine) {
			host("Privileged", true)(f)
			host("Devices", []any{map[string]any{"PathOnHost": "/dev/fuse"}})(f)
		}, 1, "unsupported: privileged,devices"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			db := webService()
			db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", strings.Repeat("f", 64), nil
			res := f.client().Deploy(context.Background(), request(webService(), db), func() {})
			if res.Outcome != protocol.OutcomeDenied || len(f.calls) != 1+tc.calls || stepText(res.Steps[0]) != tc.detail {
				t.Fatalf("%s: %+v calls=%v", name, res, f.steps())
			}
			for _, c := range f.calls {
				if c.Method != "GET" {
					t.Fatalf("%s: mutating call %+v", name, c)
				}
			}
			if res.Steps[0].Outcome != protocol.OutcomeDenied || res.Steps[1].Outcome != protocol.OutcomeSkipped || res.Steps[len(res.Steps)-1].Service != "db" || res.Steps[len(res.Steps)-1].Outcome != protocol.OutcomeSkipped || len(res.Steps) != 16 {
				t.Fatalf("%s steps: %+v", name, res.Steps)
			}
			if res.Code != protocol.ResultStepFailed {
				t.Fatalf("result code: %q", res.Code)
			}
		})
	}
	f := newFakeDeployEngine(t)
	f.oldStatus = 404
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeDenied || stepText(res.Steps[0]) != "container_missing" || len(f.calls) != 2 {
		t.Fatalf("missing container: %+v", res)
	}
	f = newFakeDeployEngine(t)
	delete(f.oldContainer, "HostConfig")
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); stepText(res.Steps[0]) != "configuration_unreported" {
		t.Fatalf("absent HostConfig: %+v", res.Steps[0])
	}
}

// Every setting at once still yields a parameter within the wire bound, made of whole codes in
// vocabulary order, and a valid result: an oversized detail would make the agent replace the
// result with unreadable.
func TestDeployUnsupportedDetailStaysWithinTheStepBound(t *testing.T) {
	f := newFakeDeployEngine(t)
	h := f.oldContainer["HostConfig"].(map[string]any)
	for k, v := range map[string]any{"Privileged": true, "AutoRemove": true, "ReadonlyRootfs": true, "Tmpfs": map[string]string{"/run": "rw"}, "CapAdd": []string{"ALL"}, "SecurityOpt": []string{"x"}, "Devices": []any{map[string]any{}}, "PidMode": "host", "IpcMode": "host", "Runtime": "runsc", "Memory": 1, "Ulimits": []any{map[string]any{}}, "Sysctls": map[string]string{"a": "b"}, "DeviceRequests": []any{map[string]any{}}, "Init": true, "UsernsMode": "host", "CgroupParent": "/x", "GroupAdd": []string{"a"}, "ExtraHosts": []string{"a:1.1.1.1"}, "Dns": []string{"1.1.1.1"}, "Links": []string{"a:b"}, "NetworkMode": "host", "VolumesFrom": []string{"x"}, "VolumeDriver": "nfs"} {
		h[k] = v
	}
	f.oldContainer["Config"].(map[string]any)["User"] = "1000"
	f.oldContainer["Mounts"] = []any{map[string]any{"Type": "tmpfs", "Destination": "/run"}, map[string]any{"Type": "volume", "Name": strings.Repeat("ab", 32), "Destination": "/d", "Mode": "nocopy"}}
	res := f.client().Deploy(context.Background(), request(webService()), func() {})
	s := res.Steps[0]
	if res.Outcome != protocol.OutcomeDenied || s.Code != "unsupported" || !strings.HasPrefix(s.Detail, "mount_type,anonymous_volume,") || len(s.Detail) > protocol.MaxDeploymentStepDetailBytes {
		t.Fatalf("detail %d bytes: %q", len(s.Detail), s.Detail)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDeployStepFailuresStopTheRun(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*fakeDeployEngine)
		outcome string
		step    string
		want    string // stepText of the failing step
		calls   int
	}{
		"image missing":   {func(f *fakeDeployEngine) { f.imageStatus = 404 }, protocol.OutcomeFailed, protocol.StepImage, "pinned_image_missing", 4},
		"rename conflict": {func(f *fakeDeployEngine) { f.renameStatus = 409 }, protocol.OutcomeFailed, protocol.StepRename, "name_reserved", 6},
		"create conflict": {func(f *fakeDeployEngine) { f.createStatus = 409 }, protocol.OutcomeFailed, protocol.StepCreate, "name_taken", 7},
		"stop refused":    {func(f *fakeDeployEngine) { f.stopStatus = 500 }, protocol.OutcomeFailed, protocol.StepStop, "runtime_status: 500", 8},
		"start fails":     {func(f *fakeDeployEngine) { f.startStatus = 500 }, protocol.OutcomeFailed, protocol.StepStart, "runtime_status: 500", 9},
		"remove fails":    {func(f *fakeDeployEngine) { f.removeStatus = 409 }, protocol.OutcomeFailed, protocol.StepRemove, "runtime_status: 409", 11},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			res := f.client().Deploy(context.Background(), request(webService()), func() {})
			if res.Outcome != tc.outcome || res.Code != protocol.ResultStepFailed || len(f.calls) != tc.calls {
				t.Fatalf("%s: %+v calls=%v", name, res, f.steps())
			}
			var failing protocol.DeploymentStep
			for _, s := range res.Steps {
				if s.Outcome != protocol.OutcomeSucceeded && s.Outcome != protocol.OutcomeSkipped {
					failing = s
					break
				}
			}
			if failing.Step != tc.step || stepText(failing) != tc.want {
				t.Fatalf("%s failed at %+v", name, failing)
			}
			if tc.step == protocol.StepRemove {
				if len(res.Services) != 1 {
					t.Fatal("started service must keep its identity when only removal failed")
				}
			} else if len(res.Services) != 0 {
				t.Fatalf("%s recorded an identity it did not start: %+v", name, res.Services)
			}
			if raw, _ := json.Marshal(res); strings.Contains(string(raw), "canary") {
				t.Fatal("an environment value reached the result")
			}
			if err := res.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	f := newFakeDeployEngine(t)
	f.stopStatus = 304
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("already stopped must count: %+v", res)
	}
}

// The identity refusals the fixture can reach name their code, and the two that concern a
// started container name it by its full ID.
func TestDeployIdentityRefusalCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*fakeDeployEngine)
		step   string
		want   string
	}{
		"image identity":      {func(f *fakeDeployEngine) { f.imageReportedID = oldImage }, protocol.StepImage, "image_identity_mismatch"},
		"unusable identity":   {func(f *fakeDeployEngine) { f.createdID = "not-an-id" }, protocol.StepCreate, "identity_unusable"},
		"unverified identity": {func(f *fakeDeployEngine) { f.startedImage = oldImage }, protocol.StepStart, "identity_unverified: " + newID},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			res := f.client().Deploy(context.Background(), request(webService()), func() {})
			var failing protocol.DeploymentStep
			for _, s := range res.Steps {
				if s.Outcome != protocol.OutcomeSucceeded && s.Outcome != protocol.OutcomeSkipped {
					failing = s
					break
				}
			}
			if res.Outcome != protocol.OutcomeFailed || failing.Step != tc.step || stepText(failing) != tc.want || len(res.Services) != 0 || res.Validate() != nil {
				t.Fatalf("%+v", res)
			}
		})
	}
}

func TestDeployTimeAndCancellation(t *testing.T) {
	f := newFakeDeployEngine(t)
	req := request(webService())
	req.Deadline = time.Now().Add(-time.Second)
	if res := f.client().Deploy(context.Background(), req, func() {}); res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || len(f.calls) != 0 {
		t.Fatalf("past deadline: %+v", res)
	}
	// A parent deadline stands in for the request deadline expiring mid-call: the guard before
	// rename reads the request deadline, which is far enough away to pass it.
	f = newFakeDeployEngine(t)
	f.stopDelay = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	res := f.client().Deploy(ctx, request(webService()), func() {})
	if res.Outcome != protocol.OutcomeTimedOut || res.Steps[5].Step != protocol.StepStop || res.Steps[5].Outcome != protocol.OutcomeTimedOut || res.Steps[5].Code != "runtime_timeout" || len(res.Services) != 0 {
		t.Fatalf("deadline mid-stop: %+v", res)
	}
	f = newFakeDeployEngine(t)
	f.stopDelay = 3 * time.Second
	ctx, cancel = context.WithCancel(context.Background())
	go func() { time.Sleep(500 * time.Millisecond); cancel() }()
	res = f.client().Deploy(ctx, request(webService()), func() {})
	if res.Outcome != protocol.OutcomeUnknown || res.Steps[5].Step != protocol.StepStop || res.Steps[5].Outcome != protocol.OutcomeUnknown || res.Steps[5].Code != "cancelled" {
		t.Fatalf("cancel mid-stop: %+v", res)
	}
}

func TestDeployRefusesToStartWithoutTimeToFinish(t *testing.T) {
	f := newFakeDeployEngine(t)
	req := request(webService())
	req.Deadline = time.Now().Add(60 * time.Second)
	res := f.client().Deploy(context.Background(), req, func() {})
	if res.Outcome != protocol.OutcomeTimedOut || res.Steps[2].Step != protocol.StepRecheck || res.Steps[2].Outcome != protocol.OutcomeTimedOut || res.Steps[2].Code != "deadline" {
		t.Fatalf("guard: %+v", res)
	}
	want := []string{"GET /info", "GET /containers/" + oldID + "/json", "GET /images/" + oldImage + "/json", "GET /images/" + newImage + "/json"}
	if got := f.steps(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls: %v", got)
	}
	for _, s := range res.Steps[3:] {
		if s.Outcome != protocol.OutcomeSkipped {
			t.Fatalf("later step ran: %+v", s)
		}
	}
}

func TestDeployIdentityReadFailureNamesTheContainer(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.inspectNewStatus = 500
	res := f.client().Deploy(context.Background(), request(webService()), func() {})
	if res.Outcome != protocol.OutcomeFailed || res.Steps[6].Step != protocol.StepStart || stepText(res.Steps[6]) != "identity_unreadable: "+newID || len(res.Services) != 0 || res.Validate() != nil {
		t.Fatalf("identity read: %+v", res)
	}
	for _, c := range f.calls {
		if c.Method == "DELETE" {
			t.Fatal("old container removed although the new identity was not read")
		}
	}
}

// Every precondition runs before any container is touched: a refused second service leaves the
// first unreplaced.
func TestDeployTwoServicesSecondRefusedTouchesNothing(t *testing.T) {
	f := newFakeDeployEngine(t)
	db := webService()
	db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", strings.Repeat("f", 64), nil
	res := f.client().Deploy(context.Background(), request(webService(), db), func() {})
	if res.Outcome != protocol.OutcomeDenied || len(res.Services) != 0 {
		t.Fatalf("outcome: %+v", res)
	}
	got := []string{}
	for _, s := range res.Steps {
		got = append(got, s.Service+" "+s.Step+" "+s.Outcome)
	}
	want := "web precondition succeeded,web image succeeded,db precondition denied,db image skipped," +
		"web recheck skipped,web rename skipped,web create skipped,web stop skipped,web start skipped,web remove skipped," +
		"db recheck skipped,db rename skipped,db create skipped,db stop skipped,db start skipped,db remove skipped"
	if strings.Join(got, ",") != want {
		t.Fatalf("steps:\n got %v\nwant %v", got, want)
	}
	for _, c := range f.steps() {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("a container was touched: %v", f.steps())
		}
	}
	infos := 0
	for _, c := range f.steps() {
		if c == "GET /info" {
			infos++
		}
	}
	if infos != 1 || f.steps()[0] != "GET /info" {
		t.Fatalf("the default runtime is read once per run, first: %v", f.steps())
	}
}

// A frame whose issue time is far from the host clock is failed with the skew detail and no call.
func TestDeployReportsClockSkewWithoutCalling(t *testing.T) {
	f := newFakeDeployEngine(t)
	req := request(webService())
	req.IssuedAt = time.Now().Add(-protocol.MaxClockSkew - time.Minute)
	res := f.client().Deploy(context.Background(), req, func() {})
	if res.Outcome != protocol.OutcomeFailed || res.Code != protocol.ResultClockSkew || res.RequestID != requestID || len(res.Steps) != 0 || len(f.calls) != 0 {
		t.Fatalf("skewed frame: %+v calls=%v", res, f.steps())
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The recheck re-reads each old container right before its rename. A changed identity or a
// changed Config, HostConfig or Mounts is denied there: that service is untouched, later
// services are skipped, and a service already replaced stays replaced.
func TestRecheckDeniesDriftBetweenThePhases(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"identity":    func(b map[string]any) { b["Created"] = "2023-11-14T22:13:21Z" },
		"image":       func(b map[string]any) { b["Image"] = newImage },
		"host config": func(b map[string]any) { b["HostConfig"].(map[string]any)["Memory"] = 1 << 30 },
		"config":      func(b map[string]any) { b["Config"].(map[string]any)["User"] = "1000" },
		"mounts": func(b map[string]any) {
			b["Mounts"] = []any{map[string]any{"Type": "volume", "Name": "shop_data", "Destination": "/data", "RW": true}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			f.drift = func(id string, read int, body map[string]any) {
				if id == otherOldID && read > 1 {
					change(body)
				}
			}
			res := f.client().Deploy(context.Background(), request(webService(), dbService()), func() {})
			if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultStepFailed || res.Steps[10].Step != protocol.StepRecheck || res.Steps[10].Service != "db" || res.Steps[10].Code != "configuration_drift" {
				t.Fatalf("outcome: %+v", res)
			}
			got := []string{}
			for _, s := range res.Steps {
				if s.Step != protocol.StepPrecondition && s.Step != protocol.StepImage {
					got = append(got, s.Service+" "+s.Step+" "+s.Outcome)
				}
			}
			want := "web recheck succeeded,web rename succeeded,web create succeeded,web stop succeeded,web start succeeded,web remove succeeded," +
				"db recheck denied,db rename skipped,db create skipped,db stop skipped,db start skipped,db remove skipped"
			if strings.Join(got, ",") != want {
				t.Fatalf("steps:\n got %v\nwant %v", got, want)
			}
			if len(res.Services) != 1 || res.Services[0].Service != "web" {
				t.Fatalf("the replaced service must keep its identity: %+v", res.Services)
			}
			for _, c := range f.calls {
				if c.Method != "GET" && strings.Contains(c.Path, otherOldID) {
					t.Fatalf("db was touched: %v", f.steps())
				}
			}
			if err := res.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A container removed between the phases is denied at recheck with nothing touched.
func TestRecheckDeniesAContainerGoneBetweenThePhases(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.drift = func(id string, read int, _ map[string]any) {
		if id == oldID && read > 1 {
			f.oldStatus = 404
		}
	}
	res := f.client().Deploy(context.Background(), request(webService()), func() {})
	if res.Outcome != protocol.OutcomeDenied || res.Steps[2].Step != protocol.StepRecheck || res.Steps[2].Code != "container_missing" {
		t.Fatalf("gone at recheck: %+v", res)
	}
	for _, c := range f.calls {
		if c.Method != "GET" {
			t.Fatalf("mutating call: %v", f.steps())
		}
	}
}

// A restart between the phases changes State and NetworkSettings only: that is not drift.
func TestRecheckIgnoresStateAndNetworkSettings(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.drift = func(_ string, read int, body map[string]any) {
		if read > 1 {
			body["State"] = map[string]any{"Status": "running", "StartedAt": "2026-09-24T12:00:00Z", "RestartCount": 3}
			body["NetworkSettings"] = map[string]any{"Networks": map[string]any{"bridge": map[string]any{"IPAddress": "172.17.0.9"}}}
		}
	}
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("a restart denied the replacement: %+v", res)
	}
}

// started is called once, after phase one and before the first phase-two call, and never when
// phase one fails or the deadline guard refuses.
func TestDeployCallsStartedOnceBeforePhaseTwo(t *testing.T) {
	f := newFakeDeployEngine(t)
	marks := []int{}
	res := f.client().Deploy(context.Background(), request(webService(), dbService()), func() {
		f.mu.Lock()
		marks = append(marks, len(f.calls))
		f.mu.Unlock()
	})
	if res.Outcome != protocol.OutcomeSucceeded || len(marks) != 1 {
		t.Fatalf("started %d times: %+v", len(marks), res)
	}
	f.mu.Lock()
	before, next := f.calls[:marks[0]], f.calls[marks[0]]
	f.mu.Unlock()
	for _, c := range before {
		if c.Method != "GET" {
			t.Fatalf("mutation before started: %+v", c)
		}
	}
	if next.Method != "GET" || !strings.HasSuffix(next.Path, "/containers/"+oldID+"/json") {
		t.Fatalf("the first call after started is not web's recheck: %+v", next)
	}
	for name, mutate := range map[string]func(*fakeDeployEngine, *protocol.DeploymentRequest){
		"precondition denied": func(f *fakeDeployEngine, _ *protocol.DeploymentRequest) {
			f.oldContainer["HostConfig"].(map[string]any)["Privileged"] = true
		},
		"image missing": func(f *fakeDeployEngine, _ *protocol.DeploymentRequest) { f.imageStatus = 404 },
		"deadline guard": func(_ *fakeDeployEngine, r *protocol.DeploymentRequest) {
			r.Deadline = time.Now().Add(60 * time.Second)
		},
	} {
		f := newFakeDeployEngine(t)
		req := request(webService())
		mutate(f, &req)
		called := false
		if res := f.client().Deploy(context.Background(), req, func() { called = true }); res.Outcome == protocol.OutcomeSucceeded || called {
			t.Fatalf("%s: started=%v %+v", name, called, res)
		}
	}
}

// explicitRequest is a direct edit or run frame: one service, project "direct", revision 1.
func explicitRequest(s protocol.DeploymentService) protocol.DeploymentRequest {
	req := request(s)
	req.Project, req.Revision, req.Explicit = protocol.ExplicitProject, protocol.ExplicitRevision, true
	return req
}

// explicitService sets every explicit field to a value distinct from its zero.
func explicitService() protocol.DeploymentService {
	timeout := 7
	s := webService()
	s.Ports = []protocol.Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}, {Container: 9000, Protocol: "udp"}}
	s.Mounts = []protocol.Mount{{Kind: protocol.MountTmpfs, Target: "/scratch"}}
	s.Explicit = &protocol.ExplicitService{
		Command: []string{"serve", "--port", "80"}, Entrypoint: []string{"/entry"}, User: "1000:1000", WorkingDir: "/app", Hostname: "web1",
		Labels: map[string]string{"team": "a"}, NetworkMode: "front",
		Networks:    []protocol.NetworkAttachmentSpec{{Name: "back", Aliases: []string{"b"}}, {Name: "front", Aliases: []string{"web"}, IP: "10.0.0.5"}},
		Resources:   protocol.Resources{NanoCPUs: 500000000, MemoryBytes: 64 << 20, MemorySwapBytes: 128 << 20, PidsLimit: 100},
		Healthcheck: &protocol.Healthcheck{Test: []string{"CMD", "true"}, IntervalSeconds: 5, TimeoutSeconds: 2, StartPeriodSeconds: 1.5, Retries: 3},
		Privileged:  true, ReadOnlyRootfs: true, Init: true, TTY: true, StdinOpen: true,
		CapAdd: []string{"CAP_NET_ADMIN"}, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, ExtraHosts: []string{"db:10.0.0.9"}, DNS: []string{"1.1.1.1"},
		Devices:    []protocol.Device{{Host: "/dev/fuse", Container: "/dev/fuse", Permissions: "rwm"}},
		Log:        protocol.LogConfig{Driver: "json-file", Options: map[string]string{"max-size": "10m"}},
		StopSignal: "SIGINT", StopTimeout: &timeout, RestartRetries: 4, AcknowledgedBinds: []string{},
	}
	return s
}

func (f *fakeDeployEngine) call(method, suffix string) (engineCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Method == method && strings.HasSuffix(c.Path, suffix) {
			return c, true
		}
	}
	return engineCall{}, false
}

func explicitSteps(res protocol.DeploymentResult) string {
	out := []string{}
	for _, s := range res.Steps {
		out = append(out, s.Step+"="+s.Outcome+strings.TrimSuffix(":"+stepText(s), ":"))
	}
	return strings.Join(out, ",")
}

func TestDeployExplicitPreservesMACAddress(t *testing.T) {
	for _, mac := range []string{"", "02:42:ac:11:00:05"} {
		t.Run(mac, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			f.oldContainer["Config"].(map[string]any)["MacAddress"] = mac
			res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
			if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
				t.Fatalf("recreate/upgrade: %s", explicitSteps(res))
			}
			call, ok := f.call("POST", "/containers/create")
			var body map[string]any
			if !ok || json.Unmarshal([]byte(call.Body), &body) != nil {
				t.Fatal("missing create body")
			}
			if mac == "" {
				if _, set := body["MacAddress"]; set {
					t.Fatal("pinned a MAC for a container without one")
				}
			} else if body["MacAddress"] != mac {
				t.Fatalf("MAC address not preserved: %v", body["MacAddress"])
			}
		})
	}
}

// (a), (b): the create body carries every explicit setting and nothing the adapter adds.
func TestDeployExplicitCreateBody(t *testing.T) {
	f := newFakeDeployEngine(t)
	res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("outcome: %+v", res)
	}
	c, ok := f.call("POST", "/containers/create")
	if !ok {
		t.Fatal("no create")
	}
	var body struct {
		Image, User, WorkingDir, Hostname, StopSignal string
		Env, Cmd, Entrypoint                          []string
		Labels                                        map[string]string
		ExposedPorts                                  map[string]struct{}
		Tty, OpenStdin                                bool
		StopTimeout                                   *int
		Healthcheck                                   *struct {
			Test                                    []string
			Interval, Timeout, StartPeriod, Retries int64
		}
		HostConfig struct {
			NetworkMode   string
			PortBindings  map[string][]struct{ HostIp, HostPort string }
			Mounts        []struct{ Type, Source, Target string }
			RestartPolicy struct {
				Name              string
				MaximumRetryCount int
			}
			Memory, MemorySwap, NanoCpus      int64
			PidsLimit                         int64
			Privileged, ReadonlyRootfs        bool
			Init                              *bool
			CapAdd, CapDrop, SecurityOpt, Dns []string
			ExtraHosts                        []string
			Devices                           []struct{ PathOnHost, PathInContainer, CgroupPermissions string }
			LogConfig                         struct {
				Type   string
				Config map[string]string
			}
		}
		NetworkingConfig struct {
			EndpointsConfig map[string]struct {
				Aliases    []string
				IPAMConfig *struct{ IPv4Address string }
			}
		}
	}
	if err := json.Unmarshal([]byte(c.Body), &body); err != nil {
		t.Fatal(err)
	}
	h := body.HostConfig
	j := func(v any) string { raw, _ := json.Marshal(v); return string(raw) }
	checks := map[string][2]string{
		"image":       {body.Image, newImage},
		"env":         {j(body.Env), `["A=1","TOKEN=a=b\ncanary-secret"]`},
		"cmd":         {j(body.Cmd), `["serve","--port","80"]`},
		"entrypoint":  {j(body.Entrypoint), `["/entry"]`},
		"user":        {body.User, "1000:1000"},
		"workdir":     {body.WorkingDir, "/app"},
		"hostname":    {body.Hostname, "web1"},
		"labels":      {j(body.Labels), `{"team":"a"}`},
		"exposed":     {j(body.ExposedPorts), `{"80/tcp":{},"9000/udp":{}}`},
		"bindings":    {j(h.PortBindings), `{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}`},
		"mounts":      {j(h.Mounts), `[{"Type":"tmpfs","Source":"","Target":"/scratch"}]`},
		"restart":     {j(h.RestartPolicy), `{"Name":"on-failure","MaximumRetryCount":4}`},
		"networkMode": {h.NetworkMode, "front"},
		"endpoints":   {j(body.NetworkingConfig.EndpointsConfig), `{"front":{"Aliases":["web"],"IPAMConfig":{"IPv4Address":"10.0.0.5"}}}`},
		"resources":   {j([]int64{h.Memory, h.MemorySwap, h.NanoCpus, h.PidsLimit}), `[67108864,134217728,500000000,100]`},
		"healthcheck": {j(body.Healthcheck), `{"Test":["CMD","true"],"Interval":5000000000,"Timeout":2000000000,"StartPeriod":1500000000,"Retries":3}`},
		"flags":       {j([]any{h.Privileged, h.ReadonlyRootfs, h.Init, body.Tty, body.OpenStdin}), `[true,true,true,true,true]`},
		"caps":        {j([][]string{h.CapAdd, h.CapDrop, h.SecurityOpt, h.ExtraHosts, h.Dns}), `[["CAP_NET_ADMIN"],["ALL"],["no-new-privileges"],["db:10.0.0.9"],["1.1.1.1"]]`},
		"devices":     {j(h.Devices), `[{"PathOnHost":"/dev/fuse","PathInContainer":"/dev/fuse","CgroupPermissions":"rwm"}]`},
		"log":         {j(h.LogConfig), `{"Type":"json-file","Config":{"max-size":"10m"}}`},
		"stop":        {j([]any{body.StopSignal, body.StopTimeout}), `["SIGINT",7]`},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s:\n got %s\nwant %s", name, c[0], c[1])
		}
	}
	// The second attachment is connected after create and before start.
	got := strings.Join(f.steps(), ",")
	want := "POST /containers/create,POST /networks/back/connect,POST /containers/" + oldID + "/stop,POST /containers/" + newID + "/start"
	if !strings.Contains(got, want) {
		t.Fatalf("calls:\n got %s\nwant substring %s", got, want)
	}
	connect, _ := f.call("POST", "/networks/back/connect")
	if connect.Body != `{"Container":"`+newID+`","EndpointConfig":{"Aliases":["b"]}}` {
		t.Fatalf("connect body: %s", connect.Body)
	}
}

func TestDeployExplicitRenamesAndRestoresTheOriginalNameOnFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		f := newFakeDeployEngine(t)
		if failed {
			f.startStatus = 500
		}
		s := explicitService()
		s.ContainerName = "renamed-web"
		res := f.client().Deploy(context.Background(), explicitRequest(s), func() {})
		if (!failed && res.Outcome != protocol.OutcomeSucceeded) || (failed && res.Outcome != protocol.OutcomeFailed) {
			t.Fatalf("rename failed=%v: %+v", failed, res)
		}
		call, ok := f.call("POST", "/containers/create")
		q, _ := url.ParseQuery(call.Query)
		if !ok || q.Get("name") != "renamed-web" {
			t.Fatalf("new name: %+v", call)
		}
		if failed {
			f.mu.Lock()
			last := ""
			for _, call := range f.calls {
				if strings.HasSuffix(call.Path, "/containers/"+oldID+"/rename") {
					last = call.Query
				}
			}
			f.mu.Unlock()
			q, _ := url.ParseQuery(last)
			if q.Get("name") != "shop-web-1" {
				t.Fatalf("rollback lost old name: %s", last)
			}
		}
	}
}

// A healthcheck of ["NONE"] is Docker's own "disabled" and is sent as-is; nil is the image's.
func TestDeployExplicitHealthcheckNoneAndNil(t *testing.T) {
	for name, hc := range map[string]*protocol.Healthcheck{"none": {Test: []string{"NONE"}}, "nil": nil} {
		f := newFakeDeployEngine(t)
		s := explicitService()
		s.Explicit.Healthcheck = hc
		if res := f.client().Deploy(context.Background(), explicitRequest(s), func() {}); res.Outcome != protocol.OutcomeSucceeded {
			t.Fatalf("%s: %+v", name, res)
		}
		c, _ := f.call("POST", "/containers/create")
		var body struct{ Healthcheck json.RawMessage }
		_ = json.Unmarshal([]byte(c.Body), &body)
		want := map[string]string{"none": `{"Test":["NONE"],"Interval":0,"Timeout":0,"StartPeriod":0,"Retries":0}`, "nil": ""}[name]
		if string(body.Healthcheck) != want {
			t.Fatalf("%s healthcheck: %s", name, body.Healthcheck)
		}
	}
}

// A failed connect fails create with the status; the run does not start the container.
func TestDeployExplicitConnectFailureFailsCreate(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.connectStatus = 404
	res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
	if got := explicitSteps(res); got != "precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,create=failed:runtime_status: 404,stop=skipped,start=skipped,remove=skipped" || res.Validate() != nil {
		t.Fatalf("steps: %s", got)
	}
}

// (c) A run (no Replaces) is image, create, start and touches no other container.
func TestDeployExplicitRun(t *testing.T) {
	f := newFakeDeployEngine(t)
	s := explicitService()
	s.Replaces, s.ContainerName = protocol.InspectionTarget{}, "adhoc"
	started := 0
	res := f.client().Deploy(context.Background(), explicitRequest(s), func() { started++ })
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil || started != 1 {
		t.Fatalf("outcome: %+v started=%d", res, started)
	}
	if got := explicitSteps(res); got != "image=succeeded,create=succeeded,start=succeeded" {
		t.Fatalf("steps: %s", got)
	}
	want := "GET /info,GET /images/" + newImage + "/json,POST /containers/create,POST /networks/back/connect,POST /containers/" + newID + "/start,GET /containers/" + newID + "/json"
	if got := strings.Join(f.steps(), ","); got != want {
		t.Fatalf("calls:\n got %s\nwant %s", got, want)
	}
	if c, _ := f.call("POST", "/containers/create"); c.Query != "name=adhoc" {
		t.Fatalf("create query: %s", c.Query)
	}
	if len(res.Services) != 1 || res.Services[0].ContainerID != newID {
		t.Fatalf("identity: %+v", res.Services)
	}
}

// (d) A recreate whose new container does not start is rolled back: the new one removed by
// force, the old renamed back and started again when it was running.
func TestDeployExplicitRollbackOnStartFailure(t *testing.T) {
	for _, wasRunning := range []bool{true, false} {
		f := newFakeDeployEngine(t)
		f.startStatus = 500
		if !wasRunning {
			f.stopStatus = 304
		}
		res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
		if res.Outcome != protocol.OutcomeFailed || res.Code != protocol.ResultStepFailed || res.Validate() != nil {
			t.Fatalf("outcome: %+v", res)
		}
		if got := explicitSteps(res); got != "precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,create=succeeded,stop=succeeded,start=failed:start_failed_rolled_back,rollback=succeeded,remove=skipped" {
			t.Fatalf("steps: %s", got)
		}
		tail := "POST /containers/" + oldID + "/rename,POST /containers/create,POST /networks/back/connect,POST /containers/" + oldID + "/stop,POST /containers/" + newID + "/start,DELETE /containers/" + newID + ",POST /containers/" + oldID + "/rename"
		if wasRunning {
			tail += ",POST /containers/" + oldID + "/start"
		}
		if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, tail) {
			t.Fatalf("running=%v calls:\n got %s\nwant suffix %s", wasRunning, got, tail)
		}
		del, _ := f.call("DELETE", "/containers/"+newID)
		f.mu.Lock()
		renames := []string{}
		for _, c := range f.calls {
			if strings.HasSuffix(c.Path, "/rename") {
				renames = append(renames, c.Query)
			}
		}
		f.mu.Unlock()
		if del.Query != "force=1" || strings.Join(renames, "|") != "name=shop-web-1.kyyard-prev-3f2b1c9e|name=shop-web-1" {
			t.Fatalf("delete %q renames %v", del.Query, renames)
		}
	}
}

// A rollback that cannot finish says so; the start keeps its own code.
func TestDeployExplicitRollbackFailure(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.startStatus, f.removeNewStatus = 500, 500
	res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
	if got := explicitSteps(res); got != "precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,create=succeeded,stop=succeeded,start=failed:runtime_status: 500,rollback=failed:rollback_failed,remove=skipped" || res.Validate() != nil {
		t.Fatalf("steps: %s", got)
	}
	if _, renamedBack := f.call("POST", "/containers/"+oldID+"/start"); renamedBack {
		t.Fatal("old container started after a failed removal")
	}
}

// A non-explicit deploy never rolls back.
func TestDeployWithoutExplicitDoesNotRollBack(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.startStatus = 500
	res := f.client().Deploy(context.Background(), request(webService()), func() {})
	if got := explicitSteps(res); got != "precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,create=succeeded,stop=succeeded,start=failed:runtime_status: 500,remove=skipped" {
		t.Fatalf("steps: %s", got)
	}
}

// (e), (f): an explicit edit carries what undescribed refuses for a definition (privileged, a
// changed command) and the image_config comparison is not made, but what it cannot express
// (volumes_from) is still refused; a non-explicit deploy refuses both.
func TestDeployExplicitSkipsUndescribed(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.oldContainer["HostConfig"].(map[string]any)["Privileged"] = true
	f.oldImageConfig["Cmd"] = []string{"something", "else"}
	if res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("explicit: %s", explicitSteps(res))
	}
	if _, read := f.call("GET", "/images/"+oldImage+"/json"); read {
		t.Fatal("explicit precondition read the old image")
	}
	f = newFakeDeployEngine(t)
	f.oldContainer["HostConfig"].(map[string]any)["VolumesFrom"] = []string{"other"}
	f.oldContainer["HostConfig"].(map[string]any)["Privileged"] = true
	res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
	if res.Steps[0].Outcome != protocol.OutcomeDenied || stepText(res.Steps[0]) != "unsupported: volumes_from" || len(f.steps()) != 2 {
		t.Fatalf("explicit, configuration-only: %s calls %v", explicitSteps(res), f.steps())
	}
	f = newFakeDeployEngine(t)
	f.oldContainer["HostConfig"].(map[string]any)["VolumesFrom"] = []string{"other"}
	f.oldContainer["HostConfig"].(map[string]any)["Privileged"] = true
	res = f.client().Deploy(context.Background(), request(webService()), func() {})
	if res.Steps[0].Outcome != protocol.OutcomeDenied || stepText(res.Steps[0]) != "unsupported: volumes_from,privileged" {
		t.Fatalf("non-explicit: %s", explicitSteps(res))
	}
}

// (g) A new bind is refused unless the operator acknowledged its host path.
func TestDeployExplicitBindNeedsAcknowledgement(t *testing.T) {
	s := explicitService()
	s.Mounts = append(s.Mounts, protocol.Mount{Kind: protocol.MountBind, Source: "/srv/data", Target: "/data"})
	f := newFakeDeployEngine(t)
	res := f.client().Deploy(context.Background(), explicitRequest(s), func() {})
	if res.Steps[0].Outcome != protocol.OutcomeDenied || res.Steps[0].Code != "bind_missing" || len(f.steps()) != 2 {
		t.Fatalf("unacknowledged: %s calls %v", explicitSteps(res), f.steps())
	}
	s.Explicit.AcknowledgedBinds = []string{"/srv/data"}
	f = newFakeDeployEngine(t)
	if res = f.client().Deploy(context.Background(), explicitRequest(s), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("acknowledged: %s", explicitSteps(res))
	}
}

// 1a: a failed connect removes the new container and renames the old one back, so a retry
// finds the host as it was.
func TestDeployExplicitConnectFailureRestores(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.connectStatus = 500
	res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
	if got := explicitSteps(res); got != "precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,create=failed:runtime_status: 500,stop=skipped,start=skipped,remove=skipped" {
		t.Fatalf("steps: %s", got)
	}
	tail := "POST /containers/create,POST /networks/back/connect,DELETE /containers/" + newID + ",POST /containers/" + oldID + "/rename"
	if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, tail) {
		t.Fatalf("calls:\n got %s\nwant suffix %s", got, tail)
	}
	if del, _ := f.call("DELETE", "/containers/"+newID); del.Query != "force=1" {
		t.Fatalf("delete query %q", del.Query)
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	if last.Query != "name=shop-web-1" {
		t.Fatalf("rename back: %q", last.Query)
	}
}

// A failed connect on a run removes the new container; there is no old one to restore.
func TestDeployExplicitRunConnectFailureRemovesTheNewContainer(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.connectStatus = 500
	s := explicitService()
	s.Replaces, s.ContainerName = protocol.InspectionTarget{}, "adhoc"
	f.client().Deploy(context.Background(), explicitRequest(s), func() {})
	if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, "POST /networks/back/connect,DELETE /containers/"+newID) {
		t.Fatalf("calls: %s", got)
	}
}

// 1b: a run whose start fails removes the container it created, within the start step.
func TestDeployExplicitRunStartFailureRemovesTheNewContainer(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.startStatus = 500
	s := explicitService()
	s.Replaces, s.ContainerName = protocol.InspectionTarget{}, "adhoc"
	res := f.client().Deploy(context.Background(), explicitRequest(s), func() {})
	if got := explicitSteps(res); got != "image=succeeded,create=succeeded,start=failed:runtime_status: 500" || res.Validate() != nil {
		t.Fatalf("steps: %s", got)
	}
	if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, "POST /containers/"+newID+"/start,DELETE /containers/"+newID) {
		t.Fatalf("calls: %s", got)
	}
	if del, _ := f.call("DELETE", "/containers/"+newID); del.Query != "force=1" {
		t.Fatalf("delete query %q", del.Query)
	}
}

// 1c: the parked name derives from ContainerName and never stacks: an old container already
// parked by this deployment is not renamed again; one parked by another is renamed from the base.
func TestDeployRenameDoesNotStackSuffixes(t *testing.T) {
	for name, want := range map[string]string{
		"/shop-web-1.kyyard-prev-3f2b1c9e": "",
		"/shop-web-1.kyyard-prev-0badbeef": "name=shop-web-1.kyyard-prev-3f2b1c9e",
		"/shop-web-1":                      "name=shop-web-1.kyyard-prev-3f2b1c9e",
	} {
		f := newFakeDeployEngine(t)
		f.oldContainer["Name"] = name
		res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
		if res.Outcome != protocol.OutcomeSucceeded {
			t.Fatalf("%s: %s", name, explicitSteps(res))
		}
		rename, renamed := f.call("POST", "/containers/"+oldID+"/rename")
		if rename.Query != want || renamed != (want != "") {
			t.Fatalf("%s: rename %q", name, rename.Query)
		}
	}
}

// 2: the restore runs even when the run's context is cancelled during start.
func TestDeployExplicitRollbackSurvivesCancellation(t *testing.T) {
	f := newFakeDeployEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onStart = func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	}
	res := f.client().Deploy(ctx, explicitRequest(explicitService()), func() {})
	if got := explicitSteps(res); got != "precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,create=succeeded,stop=succeeded,start=unknown:start_failed_rolled_back,rollback=succeeded,remove=skipped" {
		t.Fatalf("steps: %s", got)
	}
	tail := "DELETE /containers/" + newID + ",POST /containers/" + oldID + "/rename,POST /containers/" + oldID + "/start"
	if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, tail) {
		t.Fatalf("calls: %s", got)
	}
}

// 3: a run's binds must each be acknowledged; otherwise create is denied with no daemon call.
func TestDeployExplicitRunBindNeedsAcknowledgement(t *testing.T) {
	s := explicitService()
	s.Replaces, s.ContainerName = protocol.InspectionTarget{}, "adhoc"
	s.Mounts = append(s.Mounts, protocol.Mount{Kind: protocol.MountBind, Source: "/srv/data", Target: "/data"})
	f := newFakeDeployEngine(t)
	started := false
	res := f.client().Deploy(context.Background(), explicitRequest(s), func() { started = true })
	if got := explicitSteps(res); got != "create=denied:bind_missing,start=skipped" || started || res.Validate() != nil {
		t.Fatalf("unacknowledged: %s started=%v", got, started)
	}
	if got := strings.Join(f.steps(), ","); got != "GET /info" {
		t.Fatalf("daemon called with an unacknowledged bind: %s", got)
	}
	// Nothing is pulled and no volume created for a run that is refused.
	pulled := s
	pulled.ImageID, pulled.Pull = "", &protocol.ImagePull{Reference: "ghcr.io/org/app@" + pullDigest, Digest: pullDigest}
	pulled.Mounts = append(pulled.Mounts, protocol.Mount{Kind: protocol.MountVolume, Source: "data", Target: "/v"})
	req := explicitRequest(pulled)
	req.Volumes = []string{"data"}
	f = newFakeDeployEngine(t)
	if got := explicitSteps(f.client().Deploy(context.Background(), req, func() {})); got != "create=denied:bind_missing,start=skipped" {
		t.Fatalf("pulled: %s", got)
	}
	if got := strings.Join(f.steps(), ","); got != "GET /info" {
		t.Fatalf("pulled with an unacknowledged bind: %s", got)
	}
	s.Explicit.AcknowledgedBinds = []string{"/srv/data"}
	f = newFakeDeployEngine(t)
	if res = f.client().Deploy(context.Background(), explicitRequest(s), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("acknowledged: %s", explicitSteps(res))
	}
}

// 4: an explicit frame mounts any existing volume as it is and creates a missing one without
// labels; a non-explicit frame still refuses another project's volume.
func TestDeployExplicitVolumes(t *testing.T) {
	foreign := map[string]any{"Name": "shop_data", "Driver": "local", "Labels": map[string]string{"com.docker.compose.project": "shop"}, "Options": nil}
	s := explicitService()
	s.Replaces, s.ContainerName = protocol.InspectionTarget{}, "adhoc"
	s.Mounts = []protocol.Mount{{Kind: protocol.MountVolume, Source: "shop_data", Target: "/a"}, {Kind: protocol.MountVolume, Source: "data", Target: "/b"}}
	req := explicitRequest(s)
	req.Volumes = []string{"shop_data", "data"}
	f := newFakeDeployEngine(t)
	f.volumes = map[string]any{"shop_data": foreign}
	res := f.client().Deploy(context.Background(), req, func() {})
	if got := explicitSteps(res); got != "volume=succeeded,volume=succeeded,image=succeeded,create=succeeded,start=succeeded" {
		t.Fatalf("steps: %s", got)
	}
	c, ok := f.call("POST", "/volumes/create")
	if !ok || c.Body != `{"Name":"data"}` {
		t.Fatalf("volume create: %+v", c)
	}
	w := webService()
	w.Mounts = []protocol.Mount{{Kind: protocol.MountVolume, Source: "other_data", Target: "/a"}}
	plain := request(w)
	plain.Volumes = []string{"other_data"}
	f = newFakeDeployEngine(t)
	f.volumes = map[string]any{"other_data": map[string]any{"Name": "other_data", "Driver": "local", "Labels": map[string]string{"com.docker.compose.project": "other"}, "Options": nil}}
	if res = f.client().Deploy(context.Background(), plain, func() {}); res.Steps[1].Code != "volume_not_owned" {
		t.Fatalf("non-explicit: %s", explicitSteps(res))
	}
}

// 5: Init is sent only when true; false leaves the daemon default.
func TestDeployExplicitInitOnlyWhenSet(t *testing.T) {
	f := newFakeDeployEngine(t)
	s := explicitService()
	s.Explicit.Init = false
	f.client().Deploy(context.Background(), explicitRequest(s), func() {})
	c, _ := f.call("POST", "/containers/create")
	if strings.Contains(c.Body, `"Init"`) {
		t.Fatalf("Init sent: %s", c.Body)
	}
}

// One bind rule with the server: an old bind covers a new one at the same source and target
// unless the new one would write where the old one could only read.
func TestDeployExplicitBindCoverage(t *testing.T) {
	for name, c := range map[string]struct {
		oldRW, newRO bool
		target       string
		ok           bool
	}{
		"same":           {true, false, "/data", true},
		"tightened":      {true, true, "/data", true},
		"read-only kept": {false, true, "/data", true},
		"loosened":       {false, false, "/data", false},
		"other target":   {true, false, "/elsewhere", false},
	} {
		f := newFakeDeployEngine(t)
		f.oldContainer["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/srv/data", "Destination": "/data", "RW": c.oldRW}}
		s := explicitService()
		s.Mounts = append(s.Mounts, protocol.Mount{Kind: protocol.MountBind, Source: "/srv/data", Target: c.target, ReadOnly: c.newRO})
		res := f.client().Deploy(context.Background(), explicitRequest(s), func() {})
		if got := res.Outcome == protocol.OutcomeSucceeded; got != c.ok || (!c.ok && stepText(res.Steps[0]) != "bind_missing") {
			t.Errorf("%s: %s", name, explicitSteps(res))
		}
	}
}

// I5: an explicit recreate whose create fails after the rename gives the old container its name
// back; one whose stop fails also removes the new container, so a retry finds the host as it was.
func TestDeployExplicitCreateOrStopFailureRestores(t *testing.T) {
	for name, c := range map[string]struct {
		set   func(*fakeDeployEngine)
		steps string
		tail  string
	}{
		"create": {func(f *fakeDeployEngine) { f.createStatus = 500 },
			"precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,create=failed:runtime_status: 500,stop=skipped,start=skipped,remove=skipped",
			"POST /containers/create,POST /containers/" + oldID + "/rename"},
		"stop": {func(f *fakeDeployEngine) { f.stopStatus = 500 },
			"precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,create=succeeded,stop=failed:runtime_status: 500,start=skipped,remove=skipped",
			"POST /containers/" + oldID + "/stop,DELETE /containers/" + newID + ",POST /containers/" + oldID + "/rename"},
	} {
		f := newFakeDeployEngine(t)
		c.set(f)
		res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
		if got := explicitSteps(res); got != c.steps || res.Validate() != nil {
			t.Fatalf("%s steps: %s", name, got)
		}
		if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, c.tail) {
			t.Fatalf("%s calls:\n got %s\nwant suffix %s", name, got, c.tail)
		}
		f.mu.Lock()
		last := f.calls[len(f.calls)-1]
		f.mu.Unlock()
		if last.Query != "name=shop-web-1" {
			t.Fatalf("%s rename back: %q", name, last.Query)
		}
	}
	// A definition deploy leaves the steps to say where the old container is.
	f := newFakeDeployEngine(t)
	f.createStatus = 500
	f.client().Deploy(context.Background(), request(webService()), func() {})
	if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, "POST /containers/create") {
		t.Fatalf("non-explicit calls: %s", got)
	}
}

// C2: a recreated container that stops running within the start watch is a failed start, and
// the recreate rolls back; one whose healthcheck is still starting when the watch ends stands.
func TestDeployExplicitStartWatch(t *testing.T) {
	const (
		running  = `{"Status":"running","Running":true}`
		starting = `{"Status":"running","Running":true,"Health":{"Status":"starting"}}`
		healthy  = `{"Status":"running","Running":true,"Health":{"Status":"healthy"}}`
	)
	for name, c := range map[string]struct {
		states []string
		steps  string
	}{
		"exited":     {[]string{running, `{"Status":"exited","Running":false}`}, "start=failed:start_failed_rolled_back,rollback=succeeded,remove=skipped"},
		"restarting": {[]string{`{"Status":"restarting","Running":true}`}, "start=failed:start_failed_rolled_back,rollback=succeeded,remove=skipped"},
		"dead":       {[]string{`{"Status":"dead","Running":false}`}, "start=failed:start_failed_rolled_back,rollback=succeeded,remove=skipped"},
		"unhealthy":  {[]string{starting, `{"Status":"running","Running":true,"Health":{"Status":"unhealthy"}}`}, "start=failed:start_failed_rolled_back,rollback=succeeded,remove=skipped"},
		"stays":      {[]string{running}, "start=succeeded,remove=succeeded"},
		"starting":   {[]string{starting}, "start=succeeded,remove=succeeded"},
		"healthy":    {[]string{starting, healthy}, "start=succeeded,remove=succeeded"},
	} {
		f := newFakeDeployEngine(t)
		f.newStates = c.states
		res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
		if got := explicitSteps(res); !strings.HasSuffix(got, ",stop=succeeded,"+c.steps) || res.Validate() != nil {
			t.Errorf("%s: %s", name, got)
		}
	}
	// A healthcheck leaving starting ends the watch early.
	f := newFakeDeployEngine(t)
	f.newStates = []string{healthy}
	c := docker.NewHTTP(f.srv.Client(), f.srv.URL).WatchStartFor(time.Minute, 5*time.Millisecond)
	begun := time.Now()
	if res := c.Deploy(context.Background(), explicitRequest(explicitService()), func() {}); res.Outcome != protocol.OutcomeSucceeded || time.Since(begun) > 10*time.Second {
		t.Fatalf("healthy: %s after %v", explicitSteps(res), time.Since(begun))
	}
	// When the rollback cannot finish the start says why it failed.
	f = newFakeDeployEngine(t)
	f.newStates, f.removeNewStatus = []string{`{"Status":"exited","Running":false}`}, 500
	if got := explicitSteps(f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})); !strings.HasSuffix(got, "start=failed:exited_early,rollback=failed:rollback_failed,remove=skipped") {
		t.Fatalf("rollback failed: %s", got)
	}
	// A run is not watched: a one-shot container may exit, and its logs are the operator's.
	f = newFakeDeployEngine(t)
	f.newStates = []string{`{"Status":"exited","Running":false}`}
	s := explicitService()
	s.Replaces, s.ContainerName = protocol.InspectionTarget{}, "adhoc"
	if res := f.client().Deploy(context.Background(), explicitRequest(s), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("run: %s", explicitSteps(res))
	}
}

// The watch fails a container its restart policy restarted, runs only when the old container was
// running, and never sleeps past its window.
func TestDeployExplicitWatchRules(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.newRestarts = 1
	if got := explicitSteps(f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})); !strings.HasSuffix(got, "start=failed:start_failed_rolled_back,rollback=succeeded,remove=skipped") {
		t.Fatalf("restarted: %s", got)
	}
	// A stopped old container's replacement may exit as it did: only the identity read.
	f = newFakeDeployEngine(t)
	f.stopStatus, f.newStates = 304, []string{`{"Status":"exited","Running":false}`}
	if res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {}); res.Outcome != protocol.OutcomeSucceeded || f.newReads != 1 {
		t.Fatalf("stopped old: %s after %d reads", explicitSteps(res), f.newReads)
	}
	// A poll longer than the window is cut to the window.
	f = newFakeDeployEngine(t)
	c := docker.NewHTTP(f.srv.Client(), f.srv.URL).WatchStartFor(100*time.Millisecond, time.Minute)
	begun := time.Now()
	if res := c.Deploy(context.Background(), explicitRequest(explicitService()), func() {}); res.Outcome != protocol.OutcomeSucceeded || time.Since(begun) > 10*time.Second || f.newReads != 3 {
		t.Fatalf("bounded: %s after %v and %d reads", explicitSteps(res), time.Since(begun), f.newReads)
	}
}

// The explicit precondition refuses what the configuration read would have named, so a caller
// that leaves spec.unsupported empty cannot drop a setting the frame does not carry.
func TestDeployExplicitRefusesUncarriedSettings(t *testing.T) {
	for name, c := range map[string]struct {
		set    map[string]any
		cgroup string
		step   string
	}{
		"cpu shares":        {map[string]any{"CpuShares": 512}, "2", "unsupported: resource_limits"},
		"unknown key":       {map[string]any{"UTSMode": "host"}, "2", "unsupported: host_config:UTSMode"},
		"several":           {map[string]any{"UTSMode": "host", "DnsSearch": []string{"lan"}, "CpuPeriod": 100000}, "2", "unsupported: resource_limits,dns,host_config:UTSMode"},
		"with volumes_from": {map[string]any{"VolumesFrom": []string{"x"}, "OomKillDisable": true}, "2", "unsupported: volumes_from,host_config:OomKillDisable"},
		"daemon defaults":   {map[string]any{"CgroupnsMode": "host", "ShmSize": 67108864, "MemorySwappiness": -1}, "1", ""},
	} {
		f := newFakeDeployEngine(t)
		f.cgroupVersion = c.cgroup
		for k, v := range c.set {
			f.oldContainer["HostConfig"].(map[string]any)[k] = v
		}
		res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
		if c.step == "" {
			if res.Outcome != protocol.OutcomeSucceeded {
				t.Errorf("%s: %s", name, explicitSteps(res))
			}
			continue
		}
		if res.Steps[0].Outcome != protocol.OutcomeDenied || stepText(res.Steps[0]) != c.step || len(f.steps()) != 2 || res.Validate() != nil {
			t.Errorf("%s: %s calls %v", name, explicitSteps(res), f.steps())
		}
	}
}

// A docker update during the pull window changes a setting the typed read does not decode; the
// explicit recheck compares every key.
func TestDeployExplicitRecheckComparesEveryKey(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.drift = func(id string, read int, body map[string]any) {
		if read > 1 {
			body["HostConfig"].(map[string]any)["BlkioWeight"] = 300
		}
	}
	res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
	if got := explicitSteps(res); !strings.Contains(got, "recheck=denied:configuration_drift") {
		t.Fatalf("steps: %s", got)
	}
}

// A run's container that started stays when only its identity read fails: the start succeeded,
// and removing it would discard a running workload. A recreate still rolls back.
func TestDeployExplicitRunKeepsAStartedContainer(t *testing.T) {
	s := explicitService()
	s.Replaces, s.ContainerName = protocol.InspectionTarget{}, "adhoc"
	for name, c := range map[string]struct {
		set  func(*fakeDeployEngine)
		step string
	}{
		"unreadable": {func(f *fakeDeployEngine) { f.inspectNewStatus = 500 }, "start=failed:identity_unreadable: " + newID},
		"unverified": {func(f *fakeDeployEngine) { f.startedImage = oldImage }, "start=failed:identity_unverified: " + newID},
	} {
		f := newFakeDeployEngine(t)
		c.set(f)
		res := f.client().Deploy(context.Background(), explicitRequest(s), func() {})
		if got := explicitSteps(res); got != "image=succeeded,create=succeeded,"+c.step || res.Validate() != nil {
			t.Fatalf("%s: %s", name, got)
		}
		if _, removed := f.call("DELETE", "/containers/"+newID); removed {
			t.Fatalf("%s: a started run container was removed", name)
		}
	}
	f := newFakeDeployEngine(t)
	f.inspectNewStatus, f.stopStatus = 500, 304
	if got := explicitSteps(f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})); !strings.HasSuffix(got, "start=failed:start_failed_rolled_back,rollback=succeeded,remove=skipped") {
		t.Fatalf("recreate: %s", got)
	}
}

// An undo that cannot give the old container its name back says so in a rollback step.
func TestDeployExplicitUndoFailureIsReported(t *testing.T) {
	for name, c := range map[string]struct {
		set   func(*fakeDeployEngine)
		steps string
	}{
		"create, rename back": {func(f *fakeDeployEngine) { f.createStatus, f.renameBackStatus = 500, 500 },
			"create=failed:runtime_status: 500,rollback=failed:rollback_failed,stop=skipped,start=skipped,remove=skipped"},
		"stop, rename back": {func(f *fakeDeployEngine) { f.stopStatus, f.renameBackStatus = 500, 500 },
			"create=succeeded,stop=failed:runtime_status: 500,rollback=failed:rollback_failed,start=skipped,remove=skipped"},
		"stop, remove new": {func(f *fakeDeployEngine) { f.stopStatus, f.removeNewStatus = 500, 500 },
			"create=succeeded,stop=failed:runtime_status: 500,rollback=failed:rollback_failed,start=skipped,remove=skipped"},
		"start, rename back": {func(f *fakeDeployEngine) { f.startStatus, f.renameBackStatus = 500, 500 },
			"create=succeeded,stop=succeeded,start=failed:runtime_status: 500,rollback=failed:rollback_failed,remove=skipped"},
	} {
		f := newFakeDeployEngine(t)
		c.set(f)
		res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
		if got := explicitSteps(res); got != "precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,"+c.steps || res.Validate() != nil {
			t.Errorf("%s: %s", name, got)
		}
	}
	// A successful undo adds no step.
	f := newFakeDeployEngine(t)
	f.createStatus = 500
	if got := explicitSteps(f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})); strings.Contains(got, "rollback") {
		t.Fatalf("undone: %s", got)
	}
}

// A paused old container comes back paused after a rollback; a pause that fails is reported.
func TestDeployExplicitRollbackRestoresPaused(t *testing.T) {
	for _, pauseStatus := range []int{204, 500} {
		f := newFakeDeployEngine(t)
		f.oldContainer["State"] = map[string]any{"Status": "paused", "Running": true, "Paused": true}
		f.startStatus, f.pauseStatus = 500, pauseStatus
		res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
		want := "start=failed:start_failed_rolled_back,rollback=succeeded,remove=skipped"
		if pauseStatus != 204 {
			want = "start=failed:runtime_status: 500,rollback=failed:rollback_failed,remove=skipped"
		}
		if got := explicitSteps(res); !strings.HasSuffix(got, want) || res.Validate() != nil {
			t.Fatalf("pause %d: %s", pauseStatus, got)
		}
		if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, "POST /containers/"+oldID+"/start,POST /containers/"+oldID+"/pause") {
			t.Fatalf("pause %d calls: %s", pauseStatus, got)
		}
	}
	// A running old container is not paused.
	f := newFakeDeployEngine(t)
	f.oldContainer["State"] = map[string]any{"Status": "running", "Running": true, "Paused": false}
	f.startStatus = 500
	f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
	if _, paused := f.call("POST", "/containers/"+oldID+"/pause"); paused {
		t.Fatal("a running container was paused")
	}
}

// A list longer than the frame can carry would be cut by the configuration read, so the explicit
// precondition refuses it rather than recreating without the tail.
func TestDeployExplicitRefusesTruncatedLists(t *testing.T) {
	long := strings.Repeat("a", protocol.MaxListEntryBytes+1)
	for name, edit := range map[string]func(map[string]any){
		"cap_add": func(c map[string]any) {
			c["HostConfig"].(map[string]any)["CapAdd"] = slices.Repeat([]string{"CAP_NET_ADMIN"}, protocol.MaxListEntries+1)
		},
		"dns entry": func(c map[string]any) { c["HostConfig"].(map[string]any)["Dns"] = []string{long} },
		"devices": func(c map[string]any) {
			c["HostConfig"].(map[string]any)["Devices"] = slices.Repeat([]any{map[string]any{"PathOnHost": "/dev/fuse", "PathInContainer": "/dev/fuse", "CgroupPermissions": "rwm"}}, protocol.MaxListEntries+1)
		},
		"log options": func(c map[string]any) {
			opts := map[string]any{}
			for i := range protocol.MaxLogOptions + 1 {
				opts[strconv.Itoa(i)] = "x"
			}
			c["HostConfig"].(map[string]any)["LogConfig"] = map[string]any{"Type": "json-file", "Config": opts}
		},
		"aliases": func(c map[string]any) {
			c["NetworkSettings"] = map[string]any{"Networks": map[string]any{"bridge": map[string]any{"Aliases": slices.Repeat([]string{"a"}, protocol.MaxListEntries+1)}}}
		},
		"ports": func(c map[string]any) {
			c["HostConfig"].(map[string]any)["PortBindings"] = map[string]any{"80/tcp": []any{map[string]string{"HostIp": "127.0.0.1", "HostPort": ""}}}
		},
	} {
		f := newFakeDeployEngine(t)
		edit(f.oldContainer)
		res := f.client().Deploy(context.Background(), explicitRequest(explicitService()), func() {})
		if res.Steps[0].Outcome != protocol.OutcomeDenied || stepText(res.Steps[0]) != "configuration_unreported" || len(f.steps()) != 2 || res.Validate() != nil {
			t.Errorf("%s: %s calls %v", name, explicitSteps(res), f.steps())
		}
	}
}

// unanswered makes one call of a deploy go unanswered: the parent context is cancelled, or its
// deadline passes, while the fake holds the call.
func unanswered(cancelled bool) (context.Context, context.CancelFunc) {
	if cancelled {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(300 * time.Millisecond); cancel() }()
		return ctx, cancel
	}
	return context.WithTimeout(context.Background(), 300*time.Millisecond)
}

const (
	holdCall       = time.Second
	running        = `{"Status":"running","Running":true}`
	exited         = `{"Status":"exited","Running":false}`
	created        = `{"Status":"created","Running":false}`
	parkedWeb      = "/shop-web-1.kyyard-prev-3f2b1c9e"
	recreateCalls  = "GET /info,GET /containers/" + oldID + "/json,GET /images/" + newImage + "/json,GET /containers/" + oldID + "/json,"
	runPrefixCalls = "GET /info,GET /images/" + newImage + "/json,POST /containers/create,POST /networks/back/connect,"
)

// A run whose start went unanswered may have started the container: it is removed only once a
// read shows it was never started (created); a started one, even exited, is left, and one that
// cannot be read is reported.
func TestDeployExplicitRunStartUnanswered(t *testing.T) {
	s := explicitService()
	s.Replaces, s.ContainerName = protocol.InspectionTarget{}, "adhoc"
	for name, c := range map[string]struct {
		cancelled   bool
		state       string
		inspect     int
		steps, tail string
	}{
		"timed out, running": {false, running, 200, "start=timed_out:runtime_timeout", "GET /containers/" + newID + "/json"},
		"timed out, exited":  {false, exited, 200, "start=timed_out:runtime_timeout", "GET /containers/" + newID + "/json"},
		"timed out, created": {false, created, 200, "start=timed_out:runtime_timeout", "GET /containers/" + newID + "/json,DELETE /containers/" + newID},
		"cancelled, created": {true, created, 200, "start=unknown:cancelled", "GET /containers/" + newID + "/json,DELETE /containers/" + newID},
		"timed out, unread":  {false, running, 500, "start=timed_out:runtime_timeout,rollback=failed:rollback_failed", "GET /containers/" + newID + "/json"},
		"cancelled, running": {true, running, 200, "start=unknown:cancelled", "GET /containers/" + newID + "/json"},
		"cancelled, exited":  {true, exited, 200, "start=unknown:cancelled", "GET /containers/" + newID + "/json"},
		"timed out, gone":    {false, running, 404, "start=timed_out:runtime_timeout", "GET /containers/" + newID + "/json"},
	} {
		f := newFakeDeployEngine(t)
		f.onStart = func() { time.Sleep(holdCall) }
		f.newStates, f.inspectNewStatus = []string{c.state}, c.inspect
		ctx, cancel := unanswered(c.cancelled)
		res := f.client().Deploy(ctx, explicitRequest(s), func() {})
		cancel()
		if got := explicitSteps(res); got != "image=succeeded,create=succeeded,"+c.steps || res.Validate() != nil {
			t.Errorf("%s steps: %s", name, got)
		}
		if got, want := strings.Join(f.steps(), ","), runPrefixCalls+"POST /containers/"+newID+"/start,"+c.tail; got != want {
			t.Errorf("%s calls:\n got %s\nwant %s", name, got, want)
		}
		if del, deleted := f.call("DELETE", "/containers/"+newID); deleted && del.Query != "" {
			t.Errorf("%s delete query %q: a container read as created is removed unforced", name, del.Query)
		}
	}
}

// A run's container that cannot be removed after a failed create or start is reported.
func TestDeployExplicitRunDiscardFailureIsReported(t *testing.T) {
	s := explicitService()
	s.Replaces, s.ContainerName = protocol.InspectionTarget{}, "adhoc"
	for name, c := range map[string]struct {
		set   func(*fakeDeployEngine)
		steps string
	}{
		"create": {func(f *fakeDeployEngine) { f.connectStatus = 500 }, "create=failed:runtime_status: 500,rollback=failed:rollback_failed,start=skipped"},
		"start":  {func(f *fakeDeployEngine) { f.startStatus = 500 }, "create=succeeded,start=failed:runtime_status: 500,rollback=failed:rollback_failed"},
	} {
		f := newFakeDeployEngine(t)
		f.removeNewStatus = 500
		c.set(f)
		res := f.client().Deploy(context.Background(), explicitRequest(s), func() {})
		if got := explicitSteps(res); got != "image=succeeded,"+c.steps || res.Validate() != nil {
			t.Errorf("%s: %s", name, got)
		}
	}
}

// A recreate's stop that went unanswered may have stopped the old container, or be stopping it:
// after the undo it is read and, while running, waited on to stop; one that stopped is started
// (and paused again), one still running when the wait ends is left, and a read, wait or restart
// that fails is rollback_failed. One found stopped at the recheck is not read.
func TestDeployExplicitStopUnanswered(t *testing.T) {
	const undone = "POST /containers/" + oldID + "/stop,DELETE /containers/" + newID + ",POST /containers/" + oldID + "/rename,"
	for name, c := range map[string]struct {
		cancelled, stops, paused bool
		readStatus, startStatus  int
		waitStatus               int
		waitDelay                time.Duration
		steps, tail              string
	}{
		"timed out, stopped":        {false, true, false, 200, 204, 200, 0, "stop=timed_out:runtime_timeout", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/start"},
		"timed out, stopped paused": {false, true, true, 200, 204, 200, 0, "stop=timed_out:runtime_timeout", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/start,POST /containers/" + oldID + "/pause"},
		"cancelled, stopped":        {true, true, false, 200, 204, 200, 0, "stop=unknown:cancelled", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/start"},
		"unread":                    {false, true, false, 500, 204, 200, 0, "stop=timed_out:runtime_timeout,rollback=failed:rollback_failed", "GET /containers/" + oldID + "/json"},
		"restart fails":             {false, true, false, 200, 500, 200, 0, "stop=timed_out:runtime_timeout,rollback=failed:rollback_failed", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/start"},
		"timed out, stops in grace": {false, false, false, 200, 204, 200, 0, "stop=timed_out:runtime_timeout", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/wait,POST /containers/" + oldID + "/start"},
		"cancelled, stops in grace": {true, false, false, 200, 204, 200, 0, "stop=unknown:cancelled", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/wait,POST /containers/" + oldID + "/start"},
		"paused, stops in grace":    {false, false, true, 200, 204, 200, 0, "stop=timed_out:runtime_timeout", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/wait,POST /containers/" + oldID + "/start,POST /containers/" + oldID + "/pause"},
		"keeps running":             {false, false, false, 200, 204, 200, holdCall, "stop=timed_out:runtime_timeout", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/wait"},
		"wait fails":                {false, false, false, 200, 204, 500, 0, "stop=timed_out:runtime_timeout,rollback=failed:rollback_failed", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/wait"},
	} {
		f := newFakeDeployEngine(t)
		f.stopDelay, f.oldStartStatus, f.waitStatus, f.waitDelay = holdCall, c.startStatus, c.waitStatus, c.waitDelay
		f.oldContainer["State"] = map[string]any{"Status": "running", "Running": true, "Paused": c.paused}
		f.drift = func(_ string, _ int, body map[string]any) {
			if _, stopped := f.call("POST", "/containers/"+oldID+"/stop"); stopped {
				f.oldStatus = c.readStatus
				if c.stops {
					body["State"] = map[string]any{"Status": "exited", "Running": false, "Paused": false}
				}
			}
		}
		ctx, cancel := unanswered(c.cancelled)
		res := f.client().Deploy(ctx, explicitRequest(explicitService()), func() {})
		cancel()
		if got := explicitSteps(res); got != "precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,create=succeeded,"+c.steps+",start=skipped,remove=skipped" || res.Validate() != nil {
			t.Errorf("%s steps: %s", name, got)
		}
		if got, want := strings.Join(f.steps(), ","), recreateCalls+"POST /containers/"+oldID+"/rename,POST /containers/create,POST /networks/back/connect,"+undone+c.tail; got != want {
			t.Errorf("%s calls:\n got %s\nwant %s", name, got, want)
		}
		if wait, waited := f.call("POST", "/containers/"+oldID+"/wait"); waited && wait.Query != "condition=not-running" {
			t.Errorf("%s wait query %q", name, wait.Query)
		}
	}
	// A wait whose body reports an error, or cannot be decoded, did not prove the container stopped.
	for name, body := range map[string]string{"wait error": `{"StatusCode":0,"Error":{"Message":"x"}}`, "wait garbage": `{"StatusCode":`} {
		f := newFakeDeployEngine(t)
		f.stopDelay, f.waitBody = holdCall, body
		f.oldContainer["State"] = map[string]any{"Status": "running", "Running": true, "Paused": false}
		ctx, cancel := unanswered(false)
		res := f.client().Deploy(ctx, explicitRequest(explicitService()), func() {})
		cancel()
		if got := explicitSteps(res); !strings.HasSuffix(got, ",stop=timed_out:runtime_timeout,rollback=failed:rollback_failed,start=skipped,remove=skipped") {
			t.Errorf("%s steps: %s", name, got)
		}
		if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, undone+"GET /containers/"+oldID+"/json,POST /containers/"+oldID+"/wait") {
			t.Errorf("%s calls: %s", name, got)
		}
	}
	// A stop that failed with an answer keeps the plain undo, and an old container that was not
	// running at the recheck is not read.
	for name, set := range map[string]func(*fakeDeployEngine){
		"answered":    func(f *fakeDeployEngine) { f.stopStatus = 500 },
		"not running": func(f *fakeDeployEngine) { f.stopDelay = holdCall },
	} {
		f := newFakeDeployEngine(t)
		state := map[string]any{"Status": "exited", "Running": false, "Paused": false}
		if name == "answered" {
			state = map[string]any{"Status": "running", "Running": true, "Paused": false}
		}
		f.oldContainer["State"] = state
		set(f)
		ctx, cancel := unanswered(false)
		f.client().Deploy(ctx, explicitRequest(explicitService()), func() {})
		cancel()
		if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, undone[:len(undone)-1]) {
			t.Errorf("%s calls: %s", name, got)
		}
	}
}

// A bound that ends before the wait's headers arrive means still running, not a failed wait: no
// step, no start.
func TestDeployExplicitStopWaitHeadersLate(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.stopDelay, f.waitHeaderDelay = holdCall, 200*time.Millisecond
	f.oldContainer["State"] = map[string]any{"Status": "running", "Running": true, "Paused": false}
	ctx, cancel := unanswered(false)
	res := f.client().WaitStopFor(5*time.Millisecond).Deploy(ctx, explicitRequest(explicitService()), func() {})
	cancel()
	if got := explicitSteps(res); !strings.HasSuffix(got, ",stop=timed_out:runtime_timeout,start=skipped,remove=skipped") {
		t.Errorf("steps: %s", got)
	}
	if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, "GET /containers/"+oldID+"/json,POST /containers/"+oldID+"/wait") {
		t.Errorf("calls: %s", got)
	}
}

// The wait is stopWait at most and always leaves the restart its reserve.
func TestWaitBound(t *testing.T) {
	for name, c := range map[string]struct{ stopWait, remaining, reserve, want time.Duration }{
		"stopWait-limited":   {30 * time.Second, 70 * time.Second, 30 * time.Second, 30 * time.Second},
		"reserve-limited":    {30 * time.Second, 50 * time.Second, 30 * time.Second, 20 * time.Second},
		"exactly zero":       {30 * time.Second, 30 * time.Second, 30 * time.Second, 0},
		"negative remaining": {30 * time.Second, -time.Second, 30 * time.Second, 0},
	} {
		if got := docker.WaitBound(c.stopWait, c.remaining, c.reserve); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// A recreate's rename that went unanswered may have parked the old container: it is read by ID
// and given its name back when it carries the parked name; a read that fails is rollback_failed.
func TestDeployExplicitRenameUnanswered(t *testing.T) {
	for name, c := range map[string]struct {
		cancelled, renames     bool
		readStatus, backStatus int
		steps, tail            string
	}{
		"timed out, renamed":     {false, true, 200, 0, "rename=timed_out:runtime_timeout", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/rename"},
		"timed out, not renamed": {false, false, 200, 0, "rename=timed_out:runtime_timeout", "GET /containers/" + oldID + "/json"},
		"cancelled, renamed":     {true, true, 200, 0, "rename=unknown:cancelled", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/rename"},
		"unread":                 {false, true, 500, 0, "rename=timed_out:runtime_timeout,rollback=failed:rollback_failed", "GET /containers/" + oldID + "/json"},
		"rename back fails":      {false, true, 200, 500, "rename=timed_out:runtime_timeout,rollback=failed:rollback_failed", "GET /containers/" + oldID + "/json,POST /containers/" + oldID + "/rename"},
	} {
		f := newFakeDeployEngine(t)
		f.renameDelay, f.renameBackStatus = holdCall, c.backStatus
		f.oldContainer["State"] = map[string]any{"Status": "running", "Running": true, "Paused": false}
		f.drift = func(_ string, _ int, body map[string]any) {
			if _, renamed := f.call("POST", "/containers/"+oldID+"/rename"); renamed {
				f.oldStatus = c.readStatus
				if c.renames {
					body["Name"] = parkedWeb
				}
			}
		}
		ctx, cancel := unanswered(c.cancelled)
		res := f.client().Deploy(ctx, explicitRequest(explicitService()), func() {})
		cancel()
		if got := explicitSteps(res); got != "precondition=succeeded,image=succeeded,recheck=succeeded,"+c.steps+",create=skipped,stop=skipped,start=skipped,remove=skipped" || res.Validate() != nil {
			t.Errorf("%s steps: %s", name, got)
		}
		if got, want := strings.Join(f.steps(), ","), recreateCalls+"POST /containers/"+oldID+"/rename,"+c.tail; got != want {
			t.Errorf("%s calls:\n got %s\nwant %s", name, got, want)
		}
		if back, _ := f.lastCall(); c.renames && c.readStatus == 200 && back.Query != "name=shop-web-1" {
			t.Errorf("%s rename back: %q", name, back.Query)
		}
	}
}

func (f *fakeDeployEngine) lastCall() (engineCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return engineCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// Each undo runs within its budget, not one per call: calls that answer within the budget one
// by one but not together, or a hung call, end the undo rollback_failed, and the calls stop where
// it ran out. After an unanswered stop the wait is cut so the restart keeps its reserve, however
// slow the read or long the wait's own bound: a wait that answers in time leaves the restart made,
// one that holds, or no time left to wait, leaves the container running with no step. That case
// chains two contexts, the undo's and the revive's, so it may take two budgets.
func TestDeployExplicitEachUndoWithinItsBudget(t *testing.T) {
	const budget, reserve = 600 * time.Millisecond, 200 * time.Millisecond
	const rolledBack = "DELETE /containers/" + newID + ",POST /containers/" + oldID + "/rename"
	const revive, left = "stop=timed_out:runtime_timeout,start=skipped,remove=skipped", rolledBack + ",GET /containers/" + oldID + "/json"
	for name, c := range map[string]struct {
		set         func(*fakeDeployEngine)
		readDelay   time.Duration // the read after the unanswered stop
		steps, tail string
	}{
		"slow calls share one undo's budget": {func(f *fakeDeployEngine) {
			f.startStatus, f.discardDelay, f.restoreDelay = 500, budget*3/10, budget*9/10
		}, 0,
			"stop=succeeded,start=failed:runtime_status: 500,rollback=failed:rollback_failed,remove=skipped", "POST /containers/" + newID + "/start," + rolledBack},
		"hung discard": {func(f *fakeDeployEngine) { f.startStatus, f.discardDelay = 500, holdCall }, 0,
			"stop=succeeded,start=failed:runtime_status: 500,rollback=failed:rollback_failed,remove=skipped", "POST /containers/" + newID + "/start,DELETE /containers/" + newID},
		"wait cut for the restart": {func(f *fakeDeployEngine) { f.stopDelay, f.waitDelay = holdCall, 2*holdCall }, 0,
			revive, left + ",POST /containers/" + oldID + "/wait"},
		// Guards against over-cutting: a wait and restart well inside the cut still happen.
		// TestWaitBound proves the reserve.
		"slow read, restart keeps its reserve": {func(f *fakeDeployEngine) {
			f.stopDelay, f.waitDelay, f.restoreDelay = holdCall, 10*time.Millisecond, 30*time.Millisecond
		}, budget * 4 / 10,
			revive, left + ",POST /containers/" + oldID + "/wait,POST /containers/" + oldID + "/start"},
		"no time left to wait": {func(f *fakeDeployEngine) { f.stopDelay = holdCall }, budget * 3 / 4,
			revive, left},
	} {
		f := newFakeDeployEngine(t)
		f.oldContainer["State"] = map[string]any{"Status": "running", "Running": true, "Paused": false}
		f.drift = func(string, int, map[string]any) {
			if _, stopped := f.call("POST", "/containers/"+oldID+"/stop"); stopped {
				time.Sleep(c.readDelay)
			}
		}
		c.set(f)
		ctx, cancel := unanswered(false)
		began := time.Now()
		res := f.client().WaitStopFor(holdCall).RestoreFor(budget).ReserveRestartFor(reserve).Deploy(ctx, explicitRequest(explicitService()), func() {})
		elapsed := time.Since(began)
		cancel()
		if got := explicitSteps(res); got != "precondition=succeeded,image=succeeded,recheck=succeeded,rename=succeeded,create=succeeded,"+c.steps || res.Validate() != nil {
			t.Errorf("%s steps: %s", name, got)
		}
		if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, c.tail) {
			t.Errorf("%s calls:\n got %s\nwant suffix %s", name, got, c.tail)
		}
		if elapsed > 2*budget {
			t.Errorf("%s took %s, more than each undo within its budget", name, elapsed)
		}
	}
}

// A run whose create went unanswered may have made the container: it is read by name and removed
// by ID, unforced, only when it was never started (created) and carries the name and the frame's
// image; absent, started or another's (another image, or another name the daemon resolved by ID
// prefix) is left, and a read that fails or answers an invalid ID, or a removal that fails, is
// rollback_failed. A create the
// deadline guard never sent is not looked up, nor is a recreate's unanswered create: its rename
// back reports a container holding the name.
func TestDeployExplicitRunCreateUnanswered(t *testing.T) {
	s := explicitService()
	s.Replaces, s.ContainerName = protocol.InspectionTarget{}, "adhoc"
	const read, removed = "GET /containers/adhoc/json", "GET /containers/adhoc/json,DELETE /containers/" + newID
	for name, c := range map[string]struct {
		cancelled            bool
		status, removeStatus int
		state, image         string
		id, readName         string
		steps, tail          string
	}{
		"timed out, absent":  {false, 404, 204, created, newImage, "", "", "create=timed_out:runtime_timeout", read},
		"timed out, created": {false, 200, 204, created, newImage, "", "", "create=timed_out:runtime_timeout", removed},
		"cancelled, created": {true, 200, 204, created, newImage, "", "", "create=unknown:cancelled", removed},
		"timed out, running": {false, 200, 204, running, newImage, "", "", "create=timed_out:runtime_timeout", read},
		"another's":          {false, 200, 204, created, oldImage, "", "", "create=timed_out:runtime_timeout", read},
		"another name":       {false, 200, 204, created, newImage, "", "/adhoc-2", "create=timed_out:runtime_timeout", read},
		"invalid ID":         {false, 200, 204, created, newImage, "adhoc", "", "create=timed_out:runtime_timeout,rollback=failed:rollback_failed", read},
		"lookup fails":       {false, 500, 204, created, newImage, "", "", "create=timed_out:runtime_timeout,rollback=failed:rollback_failed", read},
		"delete fails":       {false, 200, 500, created, newImage, "", "", "create=timed_out:runtime_timeout,rollback=failed:rollback_failed", removed},
	} {
		f := newFakeDeployEngine(t)
		f.createDelay, f.nameStatus, f.nameState, f.nameImage, f.removeNewStatus = holdCall, c.status, c.state, c.image, c.removeStatus
		f.nameID, f.nameName = c.id, c.readName
		ctx, cancel := unanswered(c.cancelled)
		res := f.client().Deploy(ctx, explicitRequest(s), func() {})
		cancel()
		if got := explicitSteps(res); got != "image=succeeded,"+c.steps+",start=skipped" || res.Validate() != nil {
			t.Errorf("%s steps: %s", name, got)
		}
		if got, want := strings.Join(f.steps(), ","), "GET /info,GET /images/"+newImage+"/json,POST /containers/create,"+c.tail; got != want {
			t.Errorf("%s calls:\n got %s\nwant %s", name, got, want)
		}
		if del, deleted := f.call("DELETE", "/containers/"+newID); deleted && del.Query != "" {
			t.Errorf("%s delete query %q", name, del.Query)
		}
	}
	// The deadline guard sends no create: nothing is looked up or removed, and started is not called.
	f := newFakeDeployEngine(t)
	f.nameStatus = 200
	req := explicitRequest(s)
	req.Deadline = time.Now().Add(60 * time.Second)
	started := false
	res := f.client().Deploy(context.Background(), req, func() { started = true })
	if got := explicitSteps(res); got != "image=succeeded,create=timed_out:deadline,start=skipped" || started {
		t.Errorf("deadline guard steps: %s started=%v", got, started)
	}
	if got := strings.Join(f.steps(), ","); got != "GET /info,GET /images/"+newImage+"/json" {
		t.Errorf("deadline guard calls: %s", got)
	}
	f = newFakeDeployEngine(t)
	f.createDelay = holdCall
	ctx, cancel := unanswered(false)
	f.client().Deploy(ctx, explicitRequest(explicitService()), func() {})
	cancel()
	if got := strings.Join(f.steps(), ","); !strings.HasSuffix(got, "POST /containers/create,POST /containers/"+oldID+"/rename") {
		t.Errorf("recreate calls: %s", got)
	}
}

func TestDeployExplicitKeepsPinnedUpdateTagDuringRename(t *testing.T) {
	f := newFakeDeployEngine(t)
	reference := "ghcr.io/org/app:1.2@" + pullDigest
	f.oldContainer["Config"].(map[string]any)["Image"] = reference
	f.startedImage = oldImage
	s := explicitService()
	s.ImageID, s.ContainerName = oldImage, "renamed-web"
	res := f.client().Deploy(context.Background(), explicitRequest(s), func() {})
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("rename: %+v", res)
	}
	call, ok := f.call("POST", "/containers/create")
	var body struct{ Image string }
	if !ok || json.Unmarshal([]byte(call.Body), &body) != nil || body.Image != reference {
		t.Fatalf("kept image lost its update tag: %s", call.Body)
	}
}
