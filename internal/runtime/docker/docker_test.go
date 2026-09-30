package docker_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
)

func fakeEngine(t *testing.T, containers int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = w.Write([]byte(`{"ServerVersion":"29.7.2","OSType":"linux","Architecture":"x86_64","KernelVersion":"7.2","NCPU":8,"MemTotal":1024,"Name":"host-1"}`))
		case strings.HasSuffix(r.URL.Path, "/version"):
			_, _ = w.Write([]byte(`{"ApiVersion":"1.55"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			var b strings.Builder
			b.WriteString("[")
			for i := 0; i < containers; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				mounts := `[]`
				switch i {
				case 0:
					mounts = `[{"Type":"volume","Name":"shop_data","Source":"/var/lib/docker/volumes/shop_data/_data","Destination":"/data","RW":true},{"Type":"bind","Source":"/srv/cfg","Destination":"/etc/app","RW":false},{"Type":"tmpfs","Source":"","Destination":"/run","RW":true}]`
				case 1:
					var m []string
					for j := 0; j < protocol.MaxMounts+8; j++ {
						m = append(m, `{"Type":"volume","Name":"v","Destination":"/m`+strings.Repeat("x", j)+`","RW":true}`)
					}
					mounts = "[" + strings.Join(m, ",") + "]"
				}
				b.WriteString(`{"Id":"c` + strings.Repeat("0", 3) + string(rune('a'+i%26)) + `","Names":["/web` + string(rune('a'+i%26)) + `"],"Image":"nginx:1","ImageID":"sha256:i1","State":"running","Status":"Up 2 hours","Created":1700000000,"Labels":{"com.docker.compose.project":"shop","env":"KEY=value"},"Ports":[{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"}],"NetworkSettings":{"Networks":{"shop_default":{"IPAddress":"172.18.0.3","GlobalIPv6Address":""}}},"Mounts":` + mounts + `}`)
			}
			b.WriteString("]")
			_, _ = w.Write([]byte(b.String()))
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_, _ = w.Write([]byte(`[{"Id":"sha256:i1","RepoTags":["nginx:1"],"RepoDigests":null,"Size":123,"Created":1700000000}]`))
		case strings.HasSuffix(r.URL.Path, "/networks"):
			_, _ = w.Write([]byte(`[{"Id":"n1","Name":"shop_default","Driver":"bridge","Scope":"local"}]`))
		case strings.HasSuffix(r.URL.Path, "/volumes"):
			_, _ = w.Write([]byte(`{"Volumes":[{"Name":"shop_data","Driver":"local","Mountpoint":"/var/lib/docker/volumes/shop_data/_data","CreatedAt":"2026-09-01T00:00:00Z"}]}`))
		case strings.HasPrefix(r.URL.Path, "/v1.41/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.41/containers/"), "/json")
			if id == "c000b" { // the second container vanished between list and inspect
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write([]byte(`{"Id":"` + id + `","State":{"StartedAt":"2026-09-29T09:30:00.123456789Z","Health":{"Status":"healthy","Log":[{"Output":"secret-looking output"}]}},"HostConfig":{"RestartPolicy":{"Name":"unless-stopped"}}}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestSnapshotMapsAndBoundsEngineData(t *testing.T) {
	srv := fakeEngine(t, 3)
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Engine.Version != "29.7.2" || snap.Engine.APIVersion != "1.55" || snap.Engine.CPUs != 8 {
		t.Fatalf("engine: %+v", snap.Engine)
	}
	if len(snap.Containers) != 3 || snap.Containers[0].Name != "weba" || snap.Containers[0].Ports[0].Host != 8080 || snap.Containers[0].ComposeProject != "shop" || snap.Containers[0].Networks[0] != "shop_default" {
		t.Fatalf("containers: %+v", snap.Containers)
	}
	if len(snap.Images) != 1 || snap.Images[0].Digests == nil || len(snap.Networks) != 1 || len(snap.Volumes) != 1 || snap.Volumes[0].CreatedAt.IsZero() {
		t.Fatalf("resources: %+v %+v %+v", snap.Images, snap.Networks, snap.Volumes)
	}
	if snap.Truncated != nil {
		t.Fatal("nothing should be truncated")
	}
}

// Mounts arrive as kinds with the volume name or host path as source, capped per container.
func TestSnapshotReportsMounts(t *testing.T) {
	srv := fakeEngine(t, 3)
	defer srv.Close()
	snap, err := docker.NewHTTP(srv.Client(), srv.URL).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := snap.Containers[0], snap.Containers[1], snap.Containers[2]
	want := []protocol.Mount{{Kind: "volume", Source: "shop_data", Target: "/data"}, {Kind: "bind", Source: "/srv/cfg", Target: "/etc/app", ReadOnly: true}, {Kind: "other", Target: "/run"}}
	if len(a.Mounts) != len(want) || a.MountsTruncated {
		t.Fatalf("mounts: %+v", a.Mounts)
	}
	for _, m := range want {
		if !slices.Contains(a.Mounts, m) {
			t.Fatalf("mount %+v missing from %+v", m, a.Mounts)
		}
	}
	if len(b.Mounts) != protocol.MaxMounts || !b.MountsTruncated {
		t.Fatalf("cap: %d mounts, truncated %v", len(b.Mounts), b.MountsTruncated)
	}
	if c.Mounts == nil || len(c.Mounts) != 0 {
		t.Fatalf("no mounts must be reported as an empty list: %#v", c.Mounts)
	}
}

func TestSnapshotTruncatesPastTheCap(t *testing.T) {
	srv := fakeEngine(t, protocol.MaxContainers+5)
	defer srv.Close()
	snap, err := docker.NewHTTP(srv.Client(), srv.URL).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Containers) != protocol.MaxContainers || len(snap.Truncated) != 1 || snap.Truncated[0] != "containers" {
		t.Fatalf("truncation: %d %v", len(snap.Containers), snap.Truncated)
	}
}

// Against the real daemon when the socket is reachable; proves the adapter, not the fixture.
func TestSnapshotAgainstLocalDocker(t *testing.T) {
	const sock = "/var/run/docker.sock"
	if _, err := os.Stat(sock); err != nil {
		t.Skip("no docker socket")
	}
	snap, err := docker.New(sock).Snapshot(context.Background())
	if err != nil {
		t.Skipf("docker not reachable: %v", err)
	}
	if snap.Engine.Runtime != "docker" || snap.Engine.Version == "" || snap.Engine.APIVersion == "" || snap.Engine.CPUs == 0 {
		t.Fatalf("engine facts: %+v", snap.Engine)
	}
	t.Logf("engine %s api %s: %d containers, %d images, %d networks, %d volumes", snap.Engine.Version, snap.Engine.APIVersion, len(snap.Containers), len(snap.Images), len(snap.Networks), len(snap.Volumes))
}

func TestSnapshotEnrichesRunningContainers(t *testing.T) {
	srv := fakeEngine(t, 3)
	defer srv.Close()
	snap, err := docker.NewHTTP(srv.Client(), srv.URL).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, b := snap.Containers[0], snap.Containers[1]
	if a.StartedAt.IsZero() || a.StartedAt.Nanosecond() != 0 || a.Health != "healthy" || a.RestartPolicy != "unless-stopped" {
		t.Fatalf("first container not enriched (whole seconds): %+v", a)
	}
	if !b.StartedAt.IsZero() || b.Health != "" || b.RestartPolicy != "" {
		t.Fatalf("a container gone at inspect must report zero values, not fail: %+v", b)
	}
	if a.Networks[0] != "shop_default" || a.NetworkAttachments[0] != (protocol.NetworkAttachment{Name: "shop_default", IP: "172.18.0.3"}) {
		t.Fatalf("networks: %+v %+v", a.Networks, a.NetworkAttachments)
	}
}

func TestSnapshotInspectsAtMostTheCap(t *testing.T) {
	const cap = 200 // maxSnapshotInspects
	var inspects atomic.Int32
	srv := fakeEngine(t, cap+10)
	defer srv.Close()
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") && !strings.HasSuffix(r.URL.Path, "/containers/json") && strings.Contains(r.URL.Path, "/containers/") {
			inspects.Add(1)
		}
		resp, err := http.Get(srv.URL + r.URL.RequestURI())
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer counting.Close()
	snap, err := docker.NewHTTP(counting.Client(), counting.URL).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := inspects.Load(); got != cap {
		t.Fatalf("inspected %d, want the cap %d", got, cap)
	}
	if !snap.Containers[cap+1].StartedAt.IsZero() {
		t.Fatal("a container past the cap must not be enriched")
	}
}

// A daemon that answers inspects slowly costs the snapshot at most the inspect budget (5 s):
// containers not reached in time keep zero values and the snapshot still returns.
func TestSnapshotInspectBudgetBoundsASlowDaemon(t *testing.T) {
	t.Parallel()
	const stall = 2 * time.Second
	srv := fakeEngine(t, 6)
	defer srv.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json") && !strings.HasSuffix(r.URL.Path, "/containers/json") {
			select {
			case <-time.After(stall):
			case <-r.Context().Done():
				return
			}
		}
		resp, err := http.Get(srv.URL + r.URL.RequestURI())
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer slow.Close()
	start := time.Now()
	snap, err := docker.NewHTTP(slow.Client(), slow.URL).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second+stall {
		t.Fatalf("snapshot took %s past the inspect budget", elapsed)
	}
	if snap.Containers[0].StartedAt.IsZero() {
		t.Fatal("the first container was inspected in budget")
	}
	for _, c := range snap.Containers[2:] {
		if !c.StartedAt.IsZero() || c.Health != "" {
			t.Fatalf("a container past the budget must keep zero values: %+v", c)
		}
	}
}

// Past the inspect cap, each snapshot starts where the last one stopped, so every running
// container gets its facts within a few cycles instead of the same first 200 every time.
func TestSnapshotRotatesPastTheCap(t *testing.T) {
	srv := fakeEngine(t, 210)
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	first, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 200; i < 210; i++ {
		if !first.Containers[i].StartedAt.IsZero() || second.Containers[i].StartedAt.IsZero() {
			t.Fatalf("container %d: first %v second %v", i, first.Containers[i].StartedAt, second.Containers[i].StartedAt)
		}
	}
	if !second.Containers[195].StartedAt.IsZero() {
		t.Fatal("the second snapshot went past the cap")
	}
}
