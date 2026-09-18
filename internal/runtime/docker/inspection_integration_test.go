package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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
	raw, err := exec.CommandContext(ctx, "docker", "run", "-d", "--pull", "never", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--tmpfs", "/scratch:ro", "--env", "TOKEN=inspection-secret-canary", "--label", "private=inspection-secret-canary", image, "sh", "-c", "sleep 120").CombinedOutput()
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
	serialized, _ := json.Marshal(out)
	if strings.Contains(string(serialized), "inspection-secret-canary") || strings.Contains(string(serialized), "/scratch") || strings.Contains(string(serialized), "sleep 120") {
		t.Fatal("configuration escaped redaction")
	}
	target.CreatedUnix++
	if out, err = c.InspectContainer(ctx, target); out != nil || !errors.Is(err, ErrInspectionChanged) {
		t.Fatalf("stale target accepted: %v %v", out, err)
	}
}
