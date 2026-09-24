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
	pullStatus       int            // POST /images/create; 200 default
	pullStatusFor    map[string]int // per fromImage, overriding pullStatus
	pullBody         string         // its progress stream
	pullAuth         []string       // X-Registry-Auth of each pull, "" when absent
	pulledStatus     int            // GET /images/{host%2Frepo@digest}/json; 200 default
	pulled           map[string]any // its body
	tagStatus        int            // POST /images/{pulled}/tag; 201 default
	srv              *httptest.Server
}

func newFakeDeployEngine(t *testing.T) *fakeDeployEngine {
	t.Helper()
	f := &fakeDeployEngine{oldStatus: 200, oldImageStatus: 200, imageStatus: 200, inspectNewStatus: 200, defaultRuntime: "runc", stopStatus: 204, renameStatus: 204, createStatus: 201, startStatus: 204, removeStatus: 204, pullStatus: 200, pulledStatus: 200, tagStatus: 201,
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
			w.WriteHeader(f.oldStatus)
			_ = json.NewEncoder(w).Encode(f.oldContainer)
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+otherOldID+"/json"):
			other := map[string]any{}
			for k, v := range f.oldContainer {
				other[k] = v
			}
			other["Id"], other["Name"] = otherOldID, "/shop-db-1"
			_ = json.NewEncoder(w).Encode(other)
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
		case r.Method == "POST" && strings.HasSuffix(p, "/images/"+newImage+"/tag"):
			w.WriteHeader(f.tagStatus)
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
			w.WriteHeader(f.inspectNewStatus)
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
	want := []string{"GET /info", "GET /containers/" + oldID + "/json", "GET /images/" + oldImage + "/json", "GET /images/" + newImage + "/json", "POST /containers/" + oldID + "/rename", "POST /containers/create", "POST /containers/" + oldID + "/stop", "POST /containers/" + newID + "/start", "GET /containers/" + newID + "/json", "DELETE /containers/" + oldID}
	if got := f.steps(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("call order:\n got %v\nwant %v", got, want)
	}
	if f.calls[4].Query != "name=shop-web-1.kyyard-prev-3f2b1c9e" || f.calls[5].Query != "name=shop-web-1" || f.calls[6].Query != "t=10" || f.calls[9].Query != "" {
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
	if err := json.Unmarshal([]byte(f.calls[5].Body), &body); err != nil {
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
	f.oldContainer["NetworkSettings"] = map[string]any{"Networks": map[string]any{"shop_default": map[string]any{}}}
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
	if err := json.Unmarshal([]byte(f.calls[5].Body), &body); err != nil {
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
	if res := f.client().Deploy(context.Background(), request(webService())); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("inherited settings: %+v", res)
	}
}

// "NONE", no test and no healthcheck all mean the same: none.
func TestDeployAcceptsDisabledHealthcheckOnImageWithout(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.oldContainer["Config"].(map[string]any)["Healthcheck"] = map[string]any{"Test": []string{"NONE"}}
	if res := f.client().Deploy(context.Background(), request(webService())); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("NONE healthcheck: %+v", res)
	}
}

// The runtime is compared with the daemon's default, not a fixed name.
func TestDeployRuntimeFollowsTheDaemonDefault(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.defaultRuntime = "nvidia"
	f.oldContainer["HostConfig"].(map[string]any)["Runtime"] = "nvidia"
	if res := f.client().Deploy(context.Background(), request(webService())); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("daemon-default runtime: %+v", res)
	}
	f = newFakeDeployEngine(t)
	f.defaultRuntime = ""
	db := webService()
	db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", strings.Repeat("f", 64), nil
	res := f.client().Deploy(context.Background(), request(webService(), db))
	if res.Outcome != protocol.OutcomeFailed || res.Steps[0].Step != protocol.StepPrecondition || res.Steps[0].Detail != "the daemon's default runtime could not be read" || res.Steps[len(res.Steps)-1].Outcome != protocol.OutcomeSkipped || len(f.calls) != 1 {
		t.Fatalf("unreadable default runtime: %+v calls=%v", res, f.steps())
	}
}

// Daemon-default IPC modes are reproduced by recreation on the same daemon.
func TestDeployAcceptsDaemonDefaultIPCModes(t *testing.T) {
	for _, mode := range []string{"", "private", "shareable"} {
		f := newFakeDeployEngine(t)
		f.oldContainer["HostConfig"].(map[string]any)["IpcMode"] = mode
		if res := f.client().Deploy(context.Background(), request(webService())); res.Outcome != protocol.OutcomeSucceeded {
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
	for name, tc := range map[string]struct {
		mutate func(*fakeDeployEngine)
		calls  int // after GET /info: 1 when decided from the container alone, 2 when the old image was read
	}{
		"image":                  {func(f *fakeDeployEngine) { f.oldContainer["Image"] = newImage }, 1},
		"created":                {func(f *fakeDeployEngine) { f.oldContainer["Created"] = "2023-11-14T22:13:21Z" }, 1},
		"absent HostConfig":      {unset("HostConfig"), 1},
		"absent Config":          {unset("Config"), 1},
		"absent NetworkSettings": {unset("NetworkSettings"), 1},
		"absent Mounts":          {unset("Mounts"), 1},
		"absent Privileged": {func(f *fakeDeployEngine) {
			delete(f.oldContainer["HostConfig"].(map[string]any), "Privileged")
		}, 1},
		"mounts":         {func(f *fakeDeployEngine) { f.oldContainer["Mounts"] = []any{map[string]any{"Type": "volume"}} }, 1},
		"tmpfs":          {host("Tmpfs", map[string]string{"/run": "rw"}), 1},
		"auto-remove":    {host("AutoRemove", true), 1},
		"read-only root": {host("ReadonlyRootfs", true), 1},
		"privileged":     {host("Privileged", true), 1},
		"cap add":        {host("CapAdd", []string{"NET_ADMIN"}), 1},
		"cap drop":       {host("CapDrop", []string{"ALL"}), 1},
		"security opt":   {host("SecurityOpt", []string{"no-new-privileges"}), 1},
		"devices":        {host("Devices", []any{map[string]any{"PathOnHost": "/dev/fuse"}}), 1},
		"pid mode":       {host("PidMode", "host"), 1},
		"ipc mode host":  {host("IpcMode", "host"), 1},
		"ipc container":  {host("IpcMode", "container:"+oldID), 1},
		"user":           {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["User"] = "1000" }, 1},
		"network mode":   {host("NetworkMode", "host"), 1},
		"two networks":   {networks("bridge", "shop_default"), 1},
		"other network":  {networks("shop_default"), 1},
		"cmd":            {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["Cmd"] = []string{"sleep", "300"} }, 2},
		"entrypoint": {func(f *fakeDeployEngine) {
			f.oldContainer["Config"].(map[string]any)["Entrypoint"] = []string{"/bin/sh", "-c"}
		}, 2},
		"old image gone":     {func(f *fakeDeployEngine) { f.oldImageStatus = 404 }, 2},
		"runtime":            {host("Runtime", "runsc"), 1},
		"memory":             {host("Memory", 1<<30), 1},
		"memory swap":        {host("MemorySwap", 1<<30), 1},
		"memory reservation": {host("MemoryReservation", 1<<30), 1},
		"nano cpus":          {host("NanoCpus", 500000000), 1},
		"cpu shares":         {host("CpuShares", 512), 1},
		"cpu quota":          {host("CpuQuota", 50000), 1},
		"cpuset":             {host("CpusetCpus", "0"), 1},
		"pids":               {host("PidsLimit", 100), 1},
		"ulimits":            {host("Ulimits", []any{map[string]any{"Name": "nofile", "Soft": 1024, "Hard": 1024}}), 1},
		"sysctls":            {host("Sysctls", map[string]string{"net.ipv4.ip_forward": "1"}), 1},
		"device requests":    {host("DeviceRequests", []any{map[string]any{"Driver": "nvidia", "Count": -1}}), 1},
		"init":               {host("Init", true), 1},
		"userns":             {host("UsernsMode", "host"), 1},
		"cgroup parent":      {host("CgroupParent", "/custom"), 1},
		"group add":          {host("GroupAdd", []string{"audio"}), 1},
		"extra hosts":        {host("ExtraHosts", []string{"db:10.0.0.2"}), 1},
		"dns":                {host("Dns", []string{"1.1.1.1"}), 1},
		"dns options":        {host("DnsOptions", []string{"ndots:1"}), 1},
		"dns search":         {host("DnsSearch", []string{"lan"}), 1},
		"links":              {host("Links", []string{"/shop-db-1:/shop-web-1/db"}), 1},
		"healthcheck differs": {func(f *fakeDeployEngine) {
			f.oldContainer["Config"].(map[string]any)["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}}
		}, 2},
		"working dir differs": {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["WorkingDir"] = "/srv" }, 2},
		"healthcheck interval differs": {func(f *fakeDeployEngine) {
			f.oldImageConfig["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}, "Interval": 30000000000}
			f.oldContainer["Config"].(map[string]any)["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}, "Interval": 5000000000}
		}, 2},
		"runtime not the daemon default": {func(f *fakeDeployEngine) {
			f.defaultRuntime = "nvidia"
			f.oldContainer["HostConfig"].(map[string]any)["Runtime"] = "runsc"
		}, 1},
		"stop signal differs": {func(f *fakeDeployEngine) { f.oldContainer["Config"].(map[string]any)["StopSignal"] = "SIGINT" }, 2},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			db := webService()
			db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", strings.Repeat("f", 64), nil
			res := f.client().Deploy(context.Background(), request(webService(), db))
			if res.Outcome != protocol.OutcomeDenied || len(f.calls) != 1+tc.calls {
				t.Fatalf("%s: %+v calls=%v", name, res, f.steps())
			}
			for _, c := range f.calls {
				if c.Method != "GET" {
					t.Fatalf("%s: mutating call %+v", name, c)
				}
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
	if res := f.client().Deploy(context.Background(), request(webService())); res.Outcome != protocol.OutcomeDenied || res.Steps[0].Detail != "the container no longer exists" || len(f.calls) != 2 {
		t.Fatalf("missing container: %+v", res)
	}
	f = newFakeDeployEngine(t)
	delete(f.oldContainer, "HostConfig")
	if res := f.client().Deploy(context.Background(), request(webService())); res.Steps[0].Detail != "the runtime did not report the container's full configuration" {
		t.Fatalf("absent HostConfig detail: %+v", res.Steps[0])
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
		"rename conflict": {func(f *fakeDeployEngine) { f.renameStatus = 409 }, protocol.OutcomeFailed, protocol.StepRename, 5},
		"create conflict": {func(f *fakeDeployEngine) { f.createStatus = 409 }, protocol.OutcomeFailed, protocol.StepCreate, 6},
		"stop refused":    {func(f *fakeDeployEngine) { f.stopStatus = 500 }, protocol.OutcomeFailed, protocol.StepStop, 7},
		"start fails":     {func(f *fakeDeployEngine) { f.startStatus = 500 }, protocol.OutcomeFailed, protocol.StepStart, 8},
		"remove fails":    {func(f *fakeDeployEngine) { f.removeStatus = 409 }, protocol.OutcomeFailed, protocol.StepRemove, 10},
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
	// A parent deadline stands in for the request deadline expiring mid-call: the guard before
	// rename reads the request deadline, which is far enough away to pass it.
	f = newFakeDeployEngine(t)
	f.stopDelay = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	res := f.client().Deploy(ctx, request(webService()))
	if res.Outcome != protocol.OutcomeTimedOut || res.Steps[4].Step != protocol.StepStop || res.Steps[4].Outcome != protocol.OutcomeTimedOut || len(res.Services) != 0 {
		t.Fatalf("deadline mid-stop: %+v", res)
	}
	f = newFakeDeployEngine(t)
	f.stopDelay = 3 * time.Second
	ctx, cancel = context.WithCancel(context.Background())
	go func() { time.Sleep(500 * time.Millisecond); cancel() }()
	res = f.client().Deploy(ctx, request(webService()))
	if res.Outcome != protocol.OutcomeUnknown || res.Steps[4].Step != protocol.StepStop || res.Steps[4].Outcome != protocol.OutcomeUnknown {
		t.Fatalf("cancel mid-stop: %+v", res)
	}
}

func TestDeployRefusesToStartWithoutTimeToFinish(t *testing.T) {
	f := newFakeDeployEngine(t)
	req := request(webService())
	req.Deadline = time.Now().Add(60 * time.Second)
	res := f.client().Deploy(context.Background(), req)
	if res.Outcome != protocol.OutcomeTimedOut || res.Steps[2].Step != protocol.StepRename || res.Steps[2].Outcome != protocol.OutcomeTimedOut || res.Steps[2].Detail != "not enough time left before the deadline to replace this service safely" {
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
	res := f.client().Deploy(context.Background(), request(webService()))
	if res.Outcome != protocol.OutcomeFailed || res.Steps[5].Step != protocol.StepStart || !strings.Contains(res.Steps[5].Detail, newID) || len(res.Services) != 0 {
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
	res := f.client().Deploy(context.Background(), request(webService(), db))
	if res.Outcome != protocol.OutcomeDenied || len(res.Services) != 0 {
		t.Fatalf("outcome: %+v", res)
	}
	got := []string{}
	for _, s := range res.Steps {
		got = append(got, s.Service+" "+s.Step+" "+s.Outcome)
	}
	want := "web precondition succeeded,web image succeeded,db precondition denied,db image skipped," +
		"web rename skipped,web create skipped,web stop skipped,web start skipped,web remove skipped," +
		"db rename skipped,db create skipped,db stop skipped,db start skipped,db remove skipped"
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
