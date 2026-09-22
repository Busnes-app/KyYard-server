// internal/runtime/docker/deploy_integration_test.go
package docker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// Opt-in, already-present image only. Everything created carries the fixture project label
// or name and is removed by label/name in cleanup; no other container or network is touched.
func TestDeployRealDocker(t *testing.T) {
	image := os.Getenv("KY_TEST_DOCKER_DEPLOY_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_DEPLOY_IMAGE to an existing shell image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	project := "kyyarddeployfixture"
	name := project + "-web-1"
	network := project + "_default"
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	removeFixtureContainers := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		ids, _ := exec.CommandContext(cctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).Output()
		for _, id := range strings.Fields(string(ids)) {
			_ = exec.CommandContext(cctx, "docker", "rm", "-fv", id).Run()
		}
	}
	removeFixtureNetwork := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_ = exec.CommandContext(cctx, "docker", "network", "rm", network).Run()
	}
	// Idempotent setup: a prior aborted run may have left the container or network behind.
	removeFixtureContainers()
	removeFixtureNetwork()
	t.Cleanup(func() {
		removeFixtureContainers()
		removeFixtureNetwork()
	})
	if out, err := docker("network", "create", network); err != nil {
		t.Fatalf("fixture network: %v: %s", err, out)
	}
	oldID, err := docker("run", "-d", "--pull", "never", "--name", name, "--network", network, "--network-alias", "web",
		"--label", "com.docker.compose.project="+project, "--label", "com.docker.compose.service=web", image, "sh", "-c", "sleep 300")
	if err != nil {
		t.Fatalf("fixture: %v: %s", err, oldID)
	}
	raw, err := docker("inspect", "--format", "{{json .}}", oldID)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Image   string
		Created time.Time
	}
	if err = json.Unmarshal([]byte(raw), &identity); err != nil {
		t.Fatal(err)
	}
	req := protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: "ep_1", Project: project, Revision: 3, Deadline: time.Now().Add(2 * time.Minute), Services: []protocol.DeploymentService{{
		Name: "web", ContainerName: name, ImageID: identity.Image,
		Replaces: protocol.InspectionTarget{ContainerID: oldID, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()},
		Restart:  "unless-stopped", Env: map[string]string{"TOKEN": "deploy-secret-canary"},
	}}}
	// The image has no command of its own we can rely on: alpine's default command exits
	// immediately, so this does not assert the new container is running, only its identity.
	res := New("/var/run/docker.sock").Deploy(ctx, req)
	serialized, _ := json.Marshal(res)
	if strings.Contains(string(serialized), "deploy-secret-canary") {
		t.Fatal("environment value reached the result")
	}
	if res.Outcome != protocol.OutcomeSucceeded || len(res.Services) != 1 {
		t.Fatalf("deploy: %s", serialized)
	}
	raw, err = docker("inspect", "--format", "{{json .}}", res.Services[0].ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Name   string
		Image  string
		Config struct {
			Env    []string
			Labels map[string]string
		}
		HostConfig struct {
			RestartPolicy struct{ Name string }
		}
		NetworkSettings struct {
			Networks map[string]struct{ Aliases []string }
		}
	}
	if err = json.Unmarshal([]byte(raw), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "/"+name || created.Image != identity.Image || created.HostConfig.RestartPolicy.Name != "unless-stopped" || created.Config.Labels["com.docker.compose.project"] != project || created.Config.Labels["kyyard.revision"] != "3" || !slices.Contains(created.Config.Env, "TOKEN=deploy-secret-canary") {
		t.Fatalf("new container: %s", raw)
	}
	if len(created.NetworkSettings.Networks) != 1 {
		t.Fatalf("new container networks: %+v", created.NetworkSettings.Networks)
	}
	net, ok := created.NetworkSettings.Networks[network]
	if !ok || !slices.Contains(net.Aliases, "web") {
		t.Fatalf("new container not on project network with alias: %+v", created.NetworkSettings.Networks)
	}
	if _, err = docker("inspect", oldID); err == nil {
		t.Fatal("old container survived removal")
	}
}
