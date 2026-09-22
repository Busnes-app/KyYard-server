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
// or name and is removed by label/name/tag in cleanup; no other container, network or image is
// touched. The fixture image is the given image committed with a long-lived default command, so
// the old container runs without a command override and passes the command precondition.
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
	fixtureImage := project + ":local"
	builder := project + "-build"
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
	removeFixtureImage := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_ = exec.CommandContext(cctx, "docker", "rm", "-fv", builder).Run()
		_ = exec.CommandContext(cctx, "docker", "rmi", "-f", fixtureImage).Run()
	}
	// Idempotent setup: a prior aborted run may have left the container or network behind.
	removeFixtureContainers()
	removeFixtureImage()
	removeFixtureNetwork()
	t.Cleanup(func() {
		removeFixtureContainers()
		removeFixtureImage()
		removeFixtureNetwork()
	})
	if out, err := docker("create", "--pull", "never", "--name", builder, image); err != nil {
		t.Fatalf("fixture image source: %v: %s", err, out)
	}
	if out, err := docker("commit", "--change", `CMD ["sleep","300"]`, builder, fixtureImage); err != nil {
		t.Fatalf("fixture image: %v: %s", err, out)
	}
	if out, err := docker("rm", "-fv", builder); err != nil {
		t.Fatalf("fixture image source removal: %v: %s", err, out)
	}
	if out, err := docker("network", "create", network); err != nil {
		t.Fatalf("fixture network: %v: %s", err, out)
	}
	oldID, err := docker("run", "-d", "--pull", "never", "--name", name, "--network", network, "--network-alias", "web",
		"--label", "com.docker.compose.project="+project, "--label", "com.docker.compose.service=web", fixtureImage)
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
	req := protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: "ep_1", Project: project, Revision: 3, Deadline: time.Now().Add(5 * time.Minute), Services: []protocol.DeploymentService{{
		Name: "web", ContainerName: name, ImageID: identity.Image,
		Replaces: protocol.InspectionTarget{ContainerID: oldID, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()},
		Restart:  "unless-stopped", Env: map[string]string{"TOKEN": "deploy-secret-canary"},
	}}}
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
		State  struct{ Running bool }
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
	if !created.State.Running || created.Name != "/"+name || created.Image != identity.Image || created.HostConfig.RestartPolicy.Name != "unless-stopped" || created.Config.Labels["com.docker.compose.project"] != project || created.Config.Labels["kyyard.revision"] != "3" || !slices.Contains(created.Config.Env, "TOKEN=deploy-secret-canary") {
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

// A status the Engine sent is an answer even when the session has since dropped: only a call
// with no answer is unknown.
func TestDeployAnswerAfterCancelIsFailed(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	r := &deployRun{parent: parent}
	for name, tc := range map[string]struct {
		err     error
		status  int
		outcome string
	}{
		"status error": {&statusError{status: 500}, 500, protocol.OutcomeFailed},
		"status":       {nil, 409, protocol.OutcomeFailed},
		"no answer":    {context.Canceled, 0, protocol.OutcomeUnknown},
	} {
		if outcome, _ := r.outcomeFor(parent, tc.err, tc.status); outcome != tc.outcome {
			t.Fatalf("%s: %s, want %s", name, outcome, tc.outcome)
		}
	}
}
