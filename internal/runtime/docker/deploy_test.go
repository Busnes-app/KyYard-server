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
	otherOldID   = "a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6"
	pullDigest   = "sha256:a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5"
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
	defaultRuntime   string         // GET /info DefaultRuntime; "runc" default
	stopStatus       int            // 204 default; 304 allowed
	stopDelay        time.Duration
	renameStatus     int
	createStatus     int // 201 default
	startStatus      int
	removeStatus     int
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
	f := &fakeDeployEngine{oldStatus: 200, oldImageStatus: 200, imageStatus: 200, inspectNewStatus: 200, defaultRuntime: "runc", stopStatus: 204, renameStatus: 204, createStatus: 201, startStatus: 204, removeStatus: 204, pullStatus: 200, pulledStatus: 200, tagStatus: 201, volumeStatus: 201, reads: map[string]int{},
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
			_ = json.NewEncoder(w).Encode(map[string]any{"DefaultRuntime": f.defaultRuntime})
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
			_, _ = w.Write([]byte(`{"Id":"` + newImage + `"}`))
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
			w.WriteHeader(f.renameStatus)
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/create"):
			w.WriteHeader(f.createStatus)
			_, _ = w.Write([]byte(`{"Id":"` + newID + `","Warnings":[]}`))
		case r.Method == "POST" && strings.HasSuffix(p, "/containers/"+newID+"/start"):
			w.WriteHeader(f.startStatus)
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+newID+"/json"):
			w.WriteHeader(f.inspectNewStatus)
			_, _ = w.Write([]byte(`{"Id":"` + newID + `","Image":"` + newImage + `","Created":"2024-01-01T00:00:01Z","Name":"/shop-web-1","State":{"Status":"running"}}`))
		case r.Method == "DELETE" && (strings.HasSuffix(p, "/containers/"+oldID) || strings.HasSuffix(p, "/containers/"+otherOldID)):
			w.WriteHeader(f.removeStatus)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}
func (f *fakeDeployEngine) client() *docker.Client { return docker.NewHTTP(f.srv.Client(), f.srv.URL) }

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
	return protocol.DeploymentRequest{Deployment: deploymentID, Endpoint: "ep_1", Project: "shop", Revision: 2, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Services: services}
}
func webService() protocol.DeploymentService {
	return protocol.DeploymentService{Name: "web", ContainerName: "shop-web-1", ImageID: newImage, Replaces: protocol.InspectionTarget{ContainerID: oldID, ImageID: oldImage, CreatedUnix: 1700000000}, Restart: "on-failure", Ports: []protocol.Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}}, Env: map[string]string{"TOKEN": "a=b\ncanary-secret", "A": "1"}}
}

func TestDeployReplacesOneServiceInOrder(t *testing.T) {
	f := newFakeDeployEngine(t)
	res := f.client().Deploy(context.Background(), request(webService()), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Deployment != deploymentID {
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

func TestDeployRefusesAnInvalidRequestWithoutCalling(t *testing.T) {
	f := newFakeDeployEngine(t)
	s := webService()
	s.Env = map[string]string{"1BAD": "x"}
	res := f.client().Deploy(context.Background(), request(s), func() {})
	if res.Outcome != protocol.OutcomeDenied || len(f.calls) != 0 || len(res.Steps) != 0 {
		t.Fatalf("invalid request: %+v calls=%d", res, len(f.calls))
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
	if res.Outcome != protocol.OutcomeFailed || res.Steps[0].Step != protocol.StepPrecondition || res.Steps[0].Detail != "the daemon's default runtime could not be read" || res.Steps[len(res.Steps)-1].Outcome != protocol.OutcomeSkipped || len(f.calls) != 1 {
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
		notTheOne  = "the container is not the one this plan was decided about"
		unreported = "the runtime did not report the container's full configuration"
		imageGone  = "the container's image is no longer present"
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
		}, 1, "unsupported: privileged, devices"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			db := webService()
			db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", strings.Repeat("f", 64), nil
			res := f.client().Deploy(context.Background(), request(webService(), db), func() {})
			if res.Outcome != protocol.OutcomeDenied || len(f.calls) != 1+tc.calls || res.Steps[0].Detail != tc.detail {
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
			if !strings.Contains(res.Detail, "service web, step precondition") {
				t.Fatalf("detail: %q", res.Detail)
			}
		})
	}
	f := newFakeDeployEngine(t)
	f.oldStatus = 404
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeDenied || res.Steps[0].Detail != "the container no longer exists" || len(f.calls) != 2 {
		t.Fatalf("missing container: %+v", res)
	}
	f = newFakeDeployEngine(t)
	delete(f.oldContainer, "HostConfig")
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Steps[0].Detail != "the runtime did not report the container's full configuration" {
		t.Fatalf("absent HostConfig detail: %+v", res.Steps[0])
	}
}

// Every setting at once still yields a step detail within the wire bound and a valid result:
// an oversized detail would make the agent replace the result with "unreadable".
func TestDeployUnsupportedDetailStaysWithinTheStepBound(t *testing.T) {
	f := newFakeDeployEngine(t)
	h := f.oldContainer["HostConfig"].(map[string]any)
	for k, v := range map[string]any{"Privileged": true, "AutoRemove": true, "ReadonlyRootfs": true, "Tmpfs": map[string]string{"/run": "rw"}, "CapAdd": []string{"ALL"}, "SecurityOpt": []string{"x"}, "Devices": []any{map[string]any{}}, "PidMode": "host", "IpcMode": "host", "Runtime": "runsc", "Memory": 1, "Ulimits": []any{map[string]any{}}, "Sysctls": map[string]string{"a": "b"}, "DeviceRequests": []any{map[string]any{}}, "Init": true, "UsernsMode": "host", "CgroupParent": "/x", "GroupAdd": []string{"a"}, "ExtraHosts": []string{"a:1.1.1.1"}, "Dns": []string{"1.1.1.1"}, "Links": []string{"a:b"}, "NetworkMode": "host", "VolumesFrom": []string{"x"}, "VolumeDriver": "nfs"} {
		h[k] = v
	}
	f.oldContainer["Config"].(map[string]any)["User"] = "1000"
	f.oldContainer["Mounts"] = []any{map[string]any{"Type": "tmpfs", "Destination": "/run"}, map[string]any{"Type": "volume", "Name": strings.Repeat("ab", 32), "Destination": "/d", "Mode": "nocopy"}}
	res := f.client().Deploy(context.Background(), request(webService()), func() {})
	if res.Outcome != protocol.OutcomeDenied || !strings.HasPrefix(res.Steps[0].Detail, "unsupported: mount_type, anonymous_volume") || len(res.Steps[0].Detail) > protocol.MaxDeploymentStepDetailBytes {
		t.Fatalf("detail %d bytes: %q", len(res.Steps[0].Detail), res.Steps[0].Detail)
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
		calls   int
	}{
		"image missing":   {func(f *fakeDeployEngine) { f.imageStatus = 404 }, protocol.OutcomeFailed, protocol.StepImage, 4},
		"rename conflict": {func(f *fakeDeployEngine) { f.renameStatus = 409 }, protocol.OutcomeFailed, protocol.StepRename, 6},
		"create conflict": {func(f *fakeDeployEngine) { f.createStatus = 409 }, protocol.OutcomeFailed, protocol.StepCreate, 7},
		"stop refused":    {func(f *fakeDeployEngine) { f.stopStatus = 500 }, protocol.OutcomeFailed, protocol.StepStop, 8},
		"start fails":     {func(f *fakeDeployEngine) { f.startStatus = 500 }, protocol.OutcomeFailed, protocol.StepStart, 9},
		"remove fails":    {func(f *fakeDeployEngine) { f.removeStatus = 409 }, protocol.OutcomeFailed, protocol.StepRemove, 11},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			res := f.client().Deploy(context.Background(), request(webService()), func() {})
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
			if (tc.step == protocol.StepStop || tc.step == protocol.StepStart) && !strings.Contains(failing.Detail, "(container "+newID+")") {
				t.Fatalf("%s failure must name the created container: %q", tc.step, failing.Detail)
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
	if res := f.client().Deploy(context.Background(), request(webService()), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("already stopped must count: %+v", res)
	}
}

func TestDeployTimeAndCancellation(t *testing.T) {
	f := newFakeDeployEngine(t)
	req := request(webService())
	req.Deadline = time.Now().Add(-time.Second)
	if res := f.client().Deploy(context.Background(), req, func() {}); res.Outcome != protocol.OutcomeDenied || len(f.calls) != 0 {
		t.Fatalf("past deadline: %+v", res)
	}
	// A parent deadline stands in for the request deadline expiring mid-call: the guard before
	// rename reads the request deadline, which is far enough away to pass it.
	f = newFakeDeployEngine(t)
	f.stopDelay = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	res := f.client().Deploy(ctx, request(webService()), func() {})
	if res.Outcome != protocol.OutcomeTimedOut || res.Steps[5].Step != protocol.StepStop || res.Steps[5].Outcome != protocol.OutcomeTimedOut || len(res.Services) != 0 {
		t.Fatalf("deadline mid-stop: %+v", res)
	}
	f = newFakeDeployEngine(t)
	f.stopDelay = 3 * time.Second
	ctx, cancel = context.WithCancel(context.Background())
	go func() { time.Sleep(500 * time.Millisecond); cancel() }()
	res = f.client().Deploy(ctx, request(webService()), func() {})
	if res.Outcome != protocol.OutcomeUnknown || res.Steps[5].Step != protocol.StepStop || res.Steps[5].Outcome != protocol.OutcomeUnknown {
		t.Fatalf("cancel mid-stop: %+v", res)
	}
}

func TestDeployRefusesToStartWithoutTimeToFinish(t *testing.T) {
	f := newFakeDeployEngine(t)
	req := request(webService())
	req.Deadline = time.Now().Add(60 * time.Second)
	res := f.client().Deploy(context.Background(), req, func() {})
	if res.Outcome != protocol.OutcomeTimedOut || res.Steps[2].Step != protocol.StepRecheck || res.Steps[2].Outcome != protocol.OutcomeTimedOut || res.Steps[2].Detail != "not enough time left before the deadline to replace this service safely" {
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
	if res.Outcome != protocol.OutcomeFailed || res.Steps[6].Step != protocol.StepStart || !strings.Contains(res.Steps[6].Detail, newID) || len(res.Services) != 0 {
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
	if res.Outcome != protocol.OutcomeFailed || res.Detail != "clock skew exceeds 5 minutes" || len(res.Steps) != 0 || len(f.calls) != 0 {
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
			if res.Outcome != protocol.OutcomeDenied || res.Detail != "service db, step recheck: the container changed after the precondition" {
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
	if res.Outcome != protocol.OutcomeDenied || res.Steps[2].Step != protocol.StepRecheck || res.Steps[2].Detail != "the container no longer exists" {
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
