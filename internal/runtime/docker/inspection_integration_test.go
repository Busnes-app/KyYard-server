package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// Opt-in, already-present image only. No production container is inspected or
// modified: setup and cleanup touch only the ID returned by this fixture.
func TestInspectionRealDocker(t *testing.T) {
	image := os.Getenv("KY_TEST_DOCKER_INSPECTION_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_INSPECTION_IMAGE to an existing shell image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "docker", "run", "-d", "--pull", "never", "--no-healthcheck", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--tmpfs", "/scratch:ro", "--env", "TOKEN=inspection-secret-canary", "--label", "private=inspection-secret-canary", image, "sh", "-c", "sleep 120").CombinedOutput()
	if err != nil {
		t.Fatalf("fixture: %v: %s", err, raw)
	}
	id := strings.TrimSpace(string(raw))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", "rm", "-fv", id).CombinedOutput(); err != nil {
			t.Errorf("fixture cleanup: %v: %s", err, out)
		}
	})
	raw, err = exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .}}", id).Output()
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Image   string
		Created time.Time
	}
	if err = json.Unmarshal(raw, &identity); err != nil {
		t.Fatal("fixture identity unreadable")
	}
	target := protocol.InspectionTarget{ContainerID: id, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()}
	c := New("/var/run/docker.sock")
	out, err := c.InspectContainer(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if out.Target != target || out.State != "running" || out.NetworkMode != "none" || out.Mounts.Tmpfs != 1 || out.Mounts.ReadOnly != 1 || !out.ReadOnlyRootFS || out.Privileged || out.ConfigurationVerified || out.ImagePlatform.OS != "linux" {
		t.Fatalf("unexpected inspection: %+v", out)
	}
	for _, code := range []string{"tmpfs", "read_only_rootfs", "capabilities", "security_opt", "network"} {
		if !slices.Contains(out.Unsupported, code) {
			t.Fatalf("unsupported %v lacks %s", out.Unsupported, code)
		}
	}
	if out.Health != "none" || out.RestartCount != 0 {
		t.Fatalf("a container without a healthcheck: health %q, restarts %d", out.Health, out.RestartCount)
	}
	if out.Validate(target, time.Now(), true) != nil {
		t.Fatalf("real inspection invalid: %+v", out)
	}
	serialized, _ := json.Marshal(out)
	if strings.Contains(string(serialized), "inspection-secret-canary") || strings.Contains(string(serialized), "/scratch") || strings.Contains(string(serialized), "sleep 120") {
		t.Fatal("configuration escaped redaction")
	}
	target.CreatedUnix++
	if out, err = c.InspectContainer(ctx, target); out != nil || !errors.Is(err, ErrInspectionChanged) {
		t.Fatalf("stale target accepted: %v %v", out, err)
	}

	// A healthcheck Docker has run reports its status; its command and log never leave the adapter.
	raw, err = exec.CommandContext(ctx, "docker", "run", "-d", "--pull", "never", "--network", "none", "--health-cmd", "true", "--health-interval", "1s", "--health-retries", "1", "--health-start-period", "0s", image, "sh", "-c", "sleep 120").CombinedOutput()
	if err != nil {
		t.Fatalf("healthcheck fixture: %v: %s", err, raw)
	}
	checked := strings.TrimSpace(string(raw))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", "rm", "-fv", checked).CombinedOutput(); err != nil {
			t.Errorf("healthcheck fixture cleanup: %v: %s", err, out)
		}
	})
	for {
		status, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Health.Status}}", checked).Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(status)) == "healthy" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the healthcheck never passed")
		case <-time.After(200 * time.Millisecond):
		}
	}
	raw, err = exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .}}", checked).Output()
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &identity); err != nil {
		t.Fatal("healthcheck fixture identity unreadable")
	}
	target = protocol.InspectionTarget{ContainerID: checked, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()}
	if out, err = c.InspectContainer(ctx, target); err != nil || out.Health != "healthy" || out.RestartCount != 0 || out.Validate(target, time.Now(), true) != nil {
		t.Fatalf("healthcheck inspection: %+v %v", out, err)
	}

	// The inventory reports what the daemon says about a running container on the default bridge.
	raw, err = exec.CommandContext(ctx, "docker", "run", "-d", "--pull", "never", "--no-healthcheck", "--restart", "unless-stopped", image, "sh", "-c", "sleep 120").CombinedOutput()
	if err != nil {
		t.Fatalf("snapshot fixture: %v: %s", err, raw)
	}
	bridged := strings.TrimSpace(string(raw))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", "rm", "-fv", bridged).CombinedOutput(); err != nil {
			t.Errorf("snapshot fixture cleanup: %v: %s", err, out)
		}
	})
	snap, err := c.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(snap.Containers, func(x protocol.Container) bool { return x.ID == bridged })
	if i < 0 {
		t.Fatal("fixture missing from the snapshot")
	}
	got := snap.Containers[i]
	if got.StartedAt.IsZero() || time.Since(got.StartedAt) > time.Hour || got.Health != "none" || got.RestartPolicy != "unless-stopped" || len(got.Networks) == 0 || len(got.NetworkAttachments) != len(got.Networks) {
		t.Fatalf("snapshot facts: %+v", got)
	}
	if addr, err := netip.ParseAddr(got.NetworkAttachments[0].IP); err != nil || !addr.Is4() || got.NetworkAttachments[0].Name != got.Networks[0] {
		t.Fatalf("network attachment %+v: %v", got.NetworkAttachments[0], err)
	}

	// A configuration read returns the values the fixture was created with.
	raw, err = exec.CommandContext(ctx, "docker", "run", "-d", "--pull", "never", "--no-healthcheck", "-e", "A=1", "-e", "B=x=y", "--memory", "64m", "--cap-add", "NET_ADMIN", "-p", "127.0.0.1::80", "--label", "t=1", "--restart", "on-failure:2", "--stop-timeout", "7", image, "sh", "-c", "sleep 120").CombinedOutput()
	if err != nil {
		t.Fatalf("configuration fixture: %v: %s", err, raw)
	}
	configured := strings.TrimSpace(string(raw))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", "rm", "-fv", configured).CombinedOutput(); err != nil {
			t.Errorf("configuration fixture cleanup: %v: %s", err, out)
		}
	})
	raw, err = exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .}}", configured).Output()
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &identity); err != nil {
		t.Fatal("configuration fixture identity unreadable")
	}
	target = protocol.InspectionTarget{ContainerID: configured, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()}
	cfg, err := c.ReadConfiguration(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	hostPort, err := exec.CommandContext(ctx, "docker", "port", configured, "80/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.Env, protocol.EnvEntry{Name: "A", Value: "1"}) || !slices.Contains(cfg.Env, protocol.EnvEntry{Name: "B", Value: "x=y"}) || cfg.Labels["t"] != "1" ||
		cfg.Resources.MemoryBytes != 67108864 || !slices.ContainsFunc(cfg.CapAdd, func(c string) bool { return strings.TrimPrefix(c, "CAP_") == "NET_ADMIN" }) || cfg.Restart != "on-failure" || cfg.RestartRetries != 2 || cfg.StopTimeout == nil || *cfg.StopTimeout != 7 || len(cfg.Unsupported) != 0 {
		t.Fatalf("configuration: %+v", cfg)
	}
	i80 := slices.IndexFunc(cfg.Ports, func(p protocol.Port) bool { return p.Container == 80 })
	if i80 < 0 || cfg.Ports[i80].HostIP != "127.0.0.1" || !strings.HasSuffix(strings.TrimSpace(string(hostPort)), ":"+strconv.Itoa(cfg.Ports[i80].Host)) || cfg.Ports[i80].Host == 0 {
		t.Fatalf("ports %+v, docker says %s", cfg.Ports, hostPort)
	}
	if cfg.Validate(target, time.Now()) != nil {
		t.Fatalf("real configuration invalid: %+v", cfg)
	}
}
