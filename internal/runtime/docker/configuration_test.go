package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func configurationFixture() (protocol.InspectionTarget, map[string]any, map[string]any) {
	target, container, image := inspectionFixture()
	container["Name"] = "/web"
	container["Config"] = map[string]any{
		"Image": "docker.io/library/nginx:1.27", "Env": []string{"A=1", "B=x=y", "C=line1\nline2", "NOEQ"},
		"Cmd": []string{"nginx", "-g", "daemon off;"}, "Entrypoint": []string{"/docker-entrypoint.sh"},
		"User": "101:101", "WorkingDir": "/srv", "Hostname": "web-host",
		"Labels":       map[string]string{"com.docker.compose.project": "shop", "tier": "front"},
		"ExposedPorts": map[string]any{"80/tcp": map[string]any{}, "9000/tcp": map[string]any{}},
		"Tty":          true, "OpenStdin": true, "StopSignal": "SIGQUIT", "StopTimeout": 7,
		"Healthcheck": map[string]any{"Test": []string{"CMD", "true"}, "Interval": 30e9, "Timeout": 5e9, "StartPeriod": 1e9, "Retries": 3},
	}
	container["HostConfig"] = map[string]any{
		"NetworkMode": "shop_default", "Privileged": true, "ReadonlyRootfs": true, "AutoRemove": false, "Init": true,
		"RestartPolicy": map[string]any{"Name": "on-failure", "MaximumRetryCount": 3},
		"PortBindings":  map[string]any{"80/tcp": []any{map[string]string{"HostIp": "127.0.0.1", "HostPort": "8080"}}},
		"Memory":        int64(64 << 20), "MemorySwap": int64(128 << 20), "NanoCpus": int64(500000000), "PidsLimit": 100,
		"CapAdd": []string{"NET_ADMIN"}, "CapDrop": []string{"MKNOD"}, "SecurityOpt": []string{"no-new-privileges"},
		"ExtraHosts": []string{"h:10.0.0.1"}, "Dns": []string{"1.1.1.1"},
		"Devices":     []any{map[string]string{"PathOnHost": "/dev/fuse", "PathInContainer": "/dev/fuse", "CgroupPermissions": "rwm"}},
		"LogConfig":   map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m"}},
		"VolumesFrom": []string{"x"}, "Sysctls": map[string]string{"net.core.somaxconn": "1"},
		"Tmpfs": map[string]string{"/run": "rw,size=1m"},
	}
	container["Mounts"] = []any{
		map[string]any{"Type": "volume", "Name": "shop_data", "Destination": "/data", "RW": true},
		map[string]any{"Type": "bind", "Source": "/srv/www", "Destination": "/www", "RW": false},
		map[string]any{"Type": "tmpfs", "Destination": "/tmp", "RW": true},
	}
	container["NetworkSettings"] = map[string]any{
		"Networks": map[string]any{"shop_default": map[string]any{"Aliases": []string{"web", target.ContainerID[:12]}, "IPAMConfig": map[string]string{"IPv4Address": "172.20.0.10"}}},
		"Ports":    map[string]any{"80/tcp": []any{map[string]string{"HostIp": "127.0.0.1", "HostPort": "8080"}}},
	}
	image["RepoTags"] = []string{"nginx:1.27"}
	image["RepoDigests"] = []string{"other/repo@sha256:" + strings.Repeat("d", 64), "nginx@sha256:" + strings.Repeat("c", 64)}
	return target, container, image
}

func TestReadConfigurationMapsEverything(t *testing.T) {
	target, container, image := configurationFixture()
	c := fakeInspection(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("configuration read mutated runtime")
		}
		serve(container, image)(w, r)
	})
	got, err := c.ReadConfiguration(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(target, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantEnv := []protocol.EnvEntry{{Name: "A", Value: "1"}, {Name: "B", Value: "x=y"}, {Name: "C", Value: "line1\nline2"}, {Name: "NOEQ"}}
	if !slices.Equal(got.Env, wantEnv) {
		t.Fatalf("env %+v", got.Env)
	}
	if !slices.Equal(got.Unsupported, []string{"volumes_from", "sysctls"}) {
		t.Fatalf("unsupported %v", got.Unsupported)
	}
	wantPorts := []protocol.Port{{Container: 80, Host: 8080, Protocol: "tcp", HostIP: "127.0.0.1"}, {Container: 9000, Protocol: "tcp"}}
	if !slices.Equal(got.Ports, wantPorts) {
		t.Fatalf("ports %+v", got.Ports)
	}
	wantMounts := []protocol.Mount{{Kind: "volume", Source: "shop_data", Target: "/data"}, {Kind: protocol.MountTmpfs, Target: "/run"}, {Kind: protocol.MountTmpfs, Target: "/tmp"}, {Kind: "bind", Source: "/srv/www", Target: "/www", ReadOnly: true}}
	if !slices.Equal(got.Mounts, wantMounts) {
		t.Fatalf("mounts %+v", got.Mounts)
	}
	if got.Name != "web" || got.Image.Reference != "docker.io/library/nginx:1.27" || got.Image.Digest != "sha256:"+strings.Repeat("c", 64) || got.ImageID != target.ImageID {
		t.Fatalf("identity %+v %q", got.Image, got.Name)
	}
	if !slices.Equal(got.Command, []string{"nginx", "-g", "daemon off;"}) || !slices.Equal(got.Entrypoint, []string{"/docker-entrypoint.sh"}) || got.User != "101:101" || got.WorkingDir != "/srv" || got.Hostname != "web-host" {
		t.Fatalf("process %+v", got)
	}
	if got.Labels["com.docker.compose.project"] != "shop" || got.Labels["tier"] != "front" || got.Restart != "on-failure" || got.RestartRetries != 3 || got.NetworkMode != "shop_default" {
		t.Fatalf("labels/restart %+v", got)
	}
	if want := []protocol.NetworkAttachmentSpec{{Name: "shop_default", Aliases: []string{"web"}, IP: "172.20.0.10"}}; !equalJSON(got.Networks, want) {
		t.Fatalf("networks %+v", got.Networks)
	}
	if got.Resources != (protocol.Resources{NanoCPUs: 500000000, MemoryBytes: 64 << 20, MemorySwapBytes: 128 << 20, PidsLimit: 100}) {
		t.Fatalf("resources %+v", got.Resources)
	}
	if !equalJSON(got.Healthcheck, &protocol.Healthcheck{Test: []string{"CMD", "true"}, IntervalSeconds: 30, TimeoutSeconds: 5, StartPeriodSeconds: 1, Retries: 3}) {
		t.Fatalf("healthcheck %+v", got.Healthcheck)
	}
	if !got.Privileged || !got.ReadOnlyRootfs || !got.Init || !got.TTY || !got.StdinOpen || got.StopSignal != "SIGQUIT" || got.StopTimeout == nil || *got.StopTimeout != 7 {
		t.Fatalf("flags %+v", got)
	}
	if !slices.Equal(got.CapAdd, []string{"NET_ADMIN"}) || !slices.Equal(got.CapDrop, []string{"MKNOD"}) || !slices.Equal(got.SecurityOpt, []string{"no-new-privileges"}) || !slices.Equal(got.ExtraHosts, []string{"h:10.0.0.1"}) || !slices.Equal(got.DNS, []string{"1.1.1.1"}) {
		t.Fatalf("lists %+v", got)
	}
	if !equalJSON(got.Devices, []protocol.Device{{Host: "/dev/fuse", Container: "/dev/fuse", Permissions: "rwm"}}) || !equalJSON(got.Log, protocol.LogConfig{Driver: "json-file", Options: map[string]string{"max-size": "10m"}}) {
		t.Fatalf("devices/log %+v %+v", got.Devices, got.Log)
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestReadConfigurationTruncatesAndSaysSo(t *testing.T) {
	target, container, image := configurationFixture()
	cfg := container["Config"].(map[string]any)
	env := make([]string, 129)
	for i := range env {
		env[i] = fmt.Sprintf("V%d=1", i)
	}
	cmd := make([]string, 65)
	for i := range cmd {
		cmd[i] = "x"
	}
	cfg["Env"], cfg["Cmd"] = env, cmd
	container["HostConfig"].(map[string]any)["Dns"] = slices.Repeat([]string{"1.1.1.1"}, 33)
	c := fakeInspection(t, serve(container, image))
	got, err := c.ReadConfiguration(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Env) != 128 || len(got.Command) != 64 {
		t.Fatalf("env %d argv %d", len(got.Env), len(got.Command))
	}
	for _, code := range []string{"env_truncated", "argv_truncated", "list_truncated:dns"} {
		if !slices.Contains(got.Unsupported, code) {
			t.Fatalf("%v lacks %s", got.Unsupported, code)
		}
	}
}

func TestReadConfigurationRefusesADriftingContainer(t *testing.T) {
	target, container, image := configurationFixture()
	var reads atomic.Int32
	c := fakeInspection(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/images/") && reads.Add(1) == 2 {
			container["Config"].(map[string]any)["Env"] = []string{"A=2"}
		}
		serve(container, image)(w, r)
	})
	if got, err := c.ReadConfiguration(context.Background(), target); got != nil || !errors.Is(err, ErrInspectionChanged) {
		t.Fatalf("drift accepted: %v %v", got, err)
	}
	target.CreatedUnix++
	c = fakeInspection(t, serve(container, image))
	if _, err := c.ReadConfiguration(context.Background(), target); !errors.Is(err, ErrInspectionChanged) {
		t.Fatalf("wrong identity: %v", err)
	}
}
