package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func inspectionFixture() (protocol.InspectionTarget, map[string]any, map[string]any) {
	target := protocol.InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	container := map[string]any{
		"Id": target.ContainerID, "Image": target.ImageID, "Created": time.Unix(target.CreatedUnix, 123456789).UTC().Format(time.RFC3339Nano),
		"State":           map[string]any{"Status": "running", "Error": "secret-canary"},
		"Config":          map[string]any{"Env": []string{"TOKEN=secret-canary"}, "Cmd": []string{"secret-canary"}, "Labels": map[string]string{"token": "secret-canary"}, "Healthcheck": map[string]any{"Test": []string{"secret-canary"}}},
		"HostConfig":      map[string]any{"NetworkMode": "secret-canary", "Privileged": false, "ReadonlyRootfs": true, "AutoRemove": false, "RestartPolicy": map[string]any{"Name": "on-failure", "MaximumRetryCount": 3}, "Binds": []string{"/secret-canary:/secret-canary"}, "LogConfig": map[string]any{"Config": map[string]string{"token": "secret-canary"}}},
		"Mounts":          []any{map[string]any{"Type": "bind", "RW": false, "Source": "/secret-canary", "Destination": "/secret-canary"}, map[string]any{"Type": "volume", "RW": true, "Name": "secret-canary"}},
		"NetworkSettings": map[string]any{"Networks": map[string]any{"secret-canary": map[string]string{"EndpointID": "secret-canary"}}, "Ports": map[string]any{"80/tcp": []any{map[string]string{"HostIp": "::", "HostPort": "8080"}, map[string]string{"HostIp": "0.0.0.0", "HostPort": "8080"}}, "53/udp": nil}},
	}
	image := map[string]any{"Id": target.ImageID, "Os": "linux", "Architecture": "arm64", "Variant": "v8", "Config": map[string]any{"Env": []string{"TOKEN=secret-canary"}}, "Comment": "secret-canary"}
	return target, container, image
}
func fakeInspection(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.41/info" {
			json.NewEncoder(w).Encode(map[string]string{"DefaultRuntime": "runc"})
			return
		}
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return NewHTTP(s.Client(), s.URL)
}

// expressibleFixture is inspectionFixture on its Compose project network, with a writable root
// and the image's own command, so nothing a recreate would drop remains.
func expressibleFixture() (protocol.InspectionTarget, map[string]any, map[string]any) {
	target, container, image := inspectionFixture()
	container["Config"] = map[string]any{"Env": []string{"TOKEN=secret-canary"}, "Cmd": []string{"nginx"}, "Labels": map[string]string{"com.docker.compose.project": "shop", "token": "secret-canary"}}
	h := container["HostConfig"].(map[string]any)
	h["NetworkMode"], h["ReadonlyRootfs"] = "shop_default", false
	container["NetworkSettings"].(map[string]any)["Networks"] = map[string]any{"shop_default": map[string]string{"EndpointID": "secret-canary"}}
	image["Config"] = map[string]any{"Env": []string{"TOKEN=secret-canary"}, "Cmd": []string{"nginx"}}
	return target, container, image
}

func serve(container, image map[string]any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/images/") {
			json.NewEncoder(w).Encode(image)
		} else {
			json.NewEncoder(w).Encode(container)
		}
	}
}
func TestInspectionRedactsAndPins(t *testing.T) {
	target, container, image := inspectionFixture()
	calls := []string{}
	c := fakeInspection(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method != "GET" {
			t.Error("inspection mutated runtime")
		}
		if r.URL.Path == "/v1.41/images/"+target.ImageID+"/json" {
			json.NewEncoder(w).Encode(image)
		} else {
			json.NewEncoder(w).Encode(container)
		}
	})
	out, err := c.InspectContainer(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /v1.41/containers/" + target.ContainerID + "/json", "GET /v1.41/images/" + target.ImageID + "/json", "GET /v1.41/containers/" + target.ContainerID + "/json"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("wrong requests: %v", calls)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret-canary") {
		t.Fatal("inspection leaked configuration")
	}
	if out.ConfigurationVerified || out.Target != target || out.ObservedAt.IsZero() || out.RestartPolicy != "on-failure" || out.RestartRetries != 3 || out.NetworkMode != "custom" || out.NetworkCount != 1 || !out.ReadOnlyRootFS || out.Privileged || out.AutoRemove {
		t.Fatalf("facts: %+v", out)
	}
	if out.Mounts.Bind != 1 || out.Mounts.Volume != 1 || out.Mounts.ReadOnly != 1 || len(out.Ports) != 3 || out.Ports[0].Container != 53 || out.Ports[0].Host != 0 || out.Ports[1].HostIP != "0.0.0.0" || out.ImagePlatform.Architecture != "arm64" || out.ImagePlatform.Variant != "v8" {
		t.Fatalf("mapping: %+v", out)
	}
}
func TestInspectionRejectsInvalidTargetsBeforeIO(t *testing.T) {
	target, _, _ := inspectionFixture()
	c := fakeInspection(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid target reached Docker") })
	for _, mutate := range []func(*protocol.InspectionTarget){func(t *protocol.InspectionTarget) { t.ContainerID = "named" }, func(t *protocol.InspectionTarget) { t.ContainerID = "../../info" }, func(t *protocol.InspectionTarget) { t.ImageID = "nginx:1" }, func(t *protocol.InspectionTarget) { t.ImageID = "sha256:" + strings.Repeat("B", 64) }, func(t *protocol.InspectionTarget) { t.CreatedUnix = 0 }} {
		bad := target
		mutate(&bad)
		if out, err := c.InspectContainer(context.Background(), bad); err == nil || out != nil {
			t.Fatal("invalid target accepted")
		}
	}
}
func TestInspectionRefusesChangesAndInvalidFacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
		want   error
	}{
		{"container identity", func(c, i map[string]any) { c["Id"] = strings.Repeat("c", 64) }, ErrInspectionChanged},
		{"container image", func(c, i map[string]any) { c["Image"] = "sha256:" + strings.Repeat("c", 64) }, ErrInspectionChanged},
		{"creation", func(c, i map[string]any) { c["Created"] = "2025-01-01T00:00:00Z" }, ErrInspectionChanged},
		{"image identity", func(c, i map[string]any) { i["Id"] = "sha256:" + strings.Repeat("c", 64) }, ErrInspectionChanged},
		{"null config", func(c, i map[string]any) { c["Config"] = nil }, ErrInspectionInvalid},
		{"null host config", func(c, i map[string]any) { c["HostConfig"] = nil }, ErrInspectionInvalid},
		{"missing flag", func(c, i map[string]any) { delete(c["HostConfig"].(map[string]any), "Privileged") }, ErrInspectionInvalid},
		{"unknown state", func(c, i map[string]any) { c["State"] = map[string]string{"Status": "secret-canary"} }, ErrInspectionInvalid},
		{"invalid retries", func(c, i map[string]any) {
			c["HostConfig"].(map[string]any)["RestartPolicy"] = map[string]any{"Name": "on-failure", "MaximumRetryCount": -1}
		}, ErrInspectionInvalid},
		{"invalid platform", func(c, i map[string]any) { i["Architecture"] = "secret-canary\n" }, ErrInspectionInvalid},
		{"port key", func(c, i map[string]any) {
			c["NetworkSettings"].(map[string]any)["Ports"] = map[string]any{"secret-canary": nil}
		}, ErrInspectionInvalid},
		{"port address", func(c, i map[string]any) {
			c["NetworkSettings"].(map[string]any)["Ports"] = map[string]any{"80/tcp": []any{map[string]string{"HostIp": "secret-canary", "HostPort": "80"}}}
		}, ErrInspectionInvalid},
		{"too many mounts", func(c, i map[string]any) { c["Mounts"] = make([]any, 65) }, ErrInspectionInvalid},
		{"too many bindings", func(c, i map[string]any) {
			bs := []any{}
			for range 65 {
				bs = append(bs, map[string]string{"HostIp": "0.0.0.0", "HostPort": "80"})
			}
			c["NetworkSettings"].(map[string]any)["Ports"] = map[string]any{"80/tcp": bs}
		}, ErrInspectionInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, container, image := inspectionFixture()
			tc.mutate(container, image)
			c := fakeInspection(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/images/") {
					json.NewEncoder(w).Encode(image)
				} else {
					json.NewEncoder(w).Encode(container)
				}
			})
			out, err := c.InspectContainer(context.Background(), target)
			if !errors.Is(err, tc.want) || out != nil {
				t.Fatalf("got %v, %v; want %v", out, err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-canary") {
				t.Fatal("error leaked response")
			}
		})
	}
	for _, field := range []string{"identity", "ports", "restart", "mounts", "state", "config"} {
		t.Run("changed during read/"+field, func(t *testing.T) {
			target, container, image := inspectionFixture()
			c := fakeInspection(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/images/") {
					switch field {
					case "identity":
						container["Id"] = strings.Repeat("c", 64)
					case "ports":
						container["NetworkSettings"].(map[string]any)["Ports"] = map[string]any{}
					case "restart":
						container["HostConfig"].(map[string]any)["RestartPolicy"] = map[string]any{"Name": "always"}
					case "mounts":
						container["Mounts"] = []any{}
					case "state":
						container["State"] = map[string]any{"Status": "exited"}
					case "config":
						container["HostConfig"].(map[string]any)["Memory"] = 1 << 30
					}
					json.NewEncoder(w).Encode(image)
				} else {
					json.NewEncoder(w).Encode(container)
				}
			})
			if out, err := c.InspectContainer(context.Background(), target); out != nil || !errors.Is(err, ErrInspectionChanged) {
				t.Fatalf("changed observations: %v %v", out, err)
			}
		})
	}
}
func TestInspectionBoundsAndFailureRedaction(t *testing.T) {
	target, _, _ := inspectionFixture()
	for _, tc := range []struct {
		name, body string
		status     int
		want       error
	}{
		{"daemon error", "secret-canary", 500, ErrInspectionUnavailable},
		{"missing", "secret-canary", 404, ErrInspectionNotFound},
		{"malformed", `{"Created":"secret-canary"}`, 200, ErrInspectionInvalid},
		{"trailing document", `{} {"secret":"secret-canary"}`, 200, ErrInspectionInvalid},
		{"valid prefix beyond bound", "{}" + strings.Repeat(" ", maxInspectionBody), 200, ErrInspectionInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeInspection(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) })
			if out, err := c.InspectContainer(context.Background(), target); out != nil || !errors.Is(err, tc.want) {
				t.Fatalf("got %v %v", out, err)
			}
		})
	}
	c := fakeInspection(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if out, err := c.InspectContainer(ctx, target); out != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline ignored: %v %v", out, err)
	}
}

func TestInspectionTmpfsProjection(t *testing.T) {
	target, container, image := inspectionFixture()
	container["HostConfig"].(map[string]any)["Tmpfs"] = map[string]string{"/secret-canary": "ro,size=1m", "/duplicate": "rw"}
	container["Mounts"] = []any{map[string]any{"Type": "tmpfs", "Destination": "/duplicate", "RW": true}}
	c := fakeInspection(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/images/") {
			json.NewEncoder(w).Encode(image)
		} else {
			json.NewEncoder(w).Encode(container)
		}
	})
	out, err := c.InspectContainer(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if out.Mounts.Tmpfs != 2 || out.Mounts.ReadOnly != 1 {
		t.Fatalf("tmpfs missing or counted twice: %+v", out.Mounts)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret-canary") || strings.Contains(string(raw), "duplicate") {
		t.Fatal("tmpfs path leaked")
	}
}

type inspectionTransport func(*http.Request) (*http.Response, error)

func (f inspectionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestInspectionCapsLongDeadlinesAndRedactsTransportErrors(t *testing.T) {
	target, _, _ := inspectionFixture()
	client := &http.Client{Transport: inspectionTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > callBudget {
			t.Error("caller extended inspection budget")
		}
		return nil, errors.New("secret-canary")
	})}
	c := NewHTTP(client, "http://docker")
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if out, err := c.InspectContainer(ctx, target); out != nil || err != ErrInspectionUnavailable {
		t.Fatalf("transport error escaped: %v %v", out, err)
	}
}

// The verdict is codes only: the canary fixture has a read-only root, a network off the project
// network (no Compose label) and a command its image does not set; no value leaks.
func TestInspectionReportsWhatARecreateWouldDrop(t *testing.T) {
	target, container, image := inspectionFixture()
	out, err := fakeInspection(t, serve(container, image)).InspectContainer(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if out.ConfigurationVerified || fmt.Sprint(out.Unsupported) != "[read_only_rootfs network image_config]" || out.Validate(target, time.Now()) != nil {
		t.Fatalf("verdict: %v %v", out.ConfigurationVerified, out.Unsupported)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret-canary") {
		t.Fatal("inspection leaked configuration")
	}
	target, container, image = expressibleFixture()
	out, err = fakeInspection(t, serve(container, image)).InspectContainer(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !out.ConfigurationVerified || out.Unsupported == nil || len(out.Unsupported) != 0 || out.Validate(target, time.Now()) != nil {
		t.Fatalf("expressible: %v %v", out.ConfigurationVerified, out.Unsupported)
	}
	raw, _ = json.Marshal(out)
	if strings.Contains(string(raw), "secret-canary") || strings.Contains(string(raw), "shop") {
		t.Fatal("inspection leaked a label or network name")
	}
}

// The daemon's default runtime is read once a minute per client, and a container on it is fine.
func TestInspectionCachesTheDefaultRuntime(t *testing.T) {
	target, container, image := expressibleFixture()
	container["HostConfig"].(map[string]any)["Runtime"] = "nvidia"
	var infos atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.41/info" {
			infos.Add(1)
			json.NewEncoder(w).Encode(map[string]string{"DefaultRuntime": "nvidia"})
			return
		}
		serve(container, image)(w, r)
	}))
	t.Cleanup(s.Close)
	c := NewHTTP(s.Client(), s.URL)
	for range 2 {
		out, err := c.InspectContainer(context.Background(), target)
		if err != nil || !out.ConfigurationVerified {
			t.Fatalf("%+v %v", out, err)
		}
	}
	if n := infos.Load(); n != 1 {
		t.Fatalf("GET /info %d times", n)
	}
}

// An unreadable default runtime makes the inspection unavailable rather than reporting every
// container's runtime as unsupported.
func TestInspectionRefusesAnUnreadableRuntime(t *testing.T) {
	target, container, image := expressibleFixture()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.41/info" {
			w.WriteHeader(500)
			return
		}
		serve(container, image)(w, r)
	}))
	t.Cleanup(s.Close)
	if out, err := NewHTTP(s.Client(), s.URL).InspectContainer(context.Background(), target); out != nil || !errors.Is(err, ErrInspectionUnavailable) {
		t.Fatalf("got %v %v", out, err)
	}
}

// The Engine lists Mounts from a map, unsorted: the same mounts in another order on the second
// read are not a change.
func TestInspectionIgnoresMountOrder(t *testing.T) {
	target, container, image := expressibleFixture()
	container["Mounts"] = []any{
		map[string]any{"Type": "volume", "RW": true, "Name": "shop_data", "Source": "/var/lib/docker/volumes/shop_data/_data", "Destination": "/data"},
		map[string]any{"Type": "bind", "RW": false, "Source": "/srv/cfg", "Destination": "/cfg"},
	}
	reads := 0
	c := fakeInspection(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/images/") {
			json.NewEncoder(w).Encode(image)
			return
		}
		if reads++; reads > 1 {
			m := container["Mounts"].([]any)
			container["Mounts"] = []any{m[1], m[0]}
		}
		json.NewEncoder(w).Encode(container)
	})
	out, err := c.InspectContainer(context.Background(), target)
	if err != nil || out.Mounts.Bind != 1 || out.Mounts.Volume != 1 {
		t.Fatalf("reordered mounts: %+v %v", out, err)
	}
}
