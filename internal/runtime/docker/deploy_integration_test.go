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
// With KY_TEST_DOCKER_PULL_DIGEST (alpine's repository digest) a second service is replaced
// from docker.io/library/alpine pulled anonymously by that digest. The local image is not
// removed first: on the containerd image store removing the digest reference also drops every
// tag on it (alpine:3.24, which the other regressions run with --pull never). The pull tags the
// pulled image alpine:3.24: the same image, so the tag does not move, but the test proves the
// call succeeded and the tag names the pulled ID. The web service carries a named volume
// (kyyard_it_data, written by a one-off container first) and a read-only bind of a temporary
// directory: the recreate must keep both, and the file must read back from the new container.
func TestDeployRealDocker(t *testing.T) {
	image := os.Getenv("KY_TEST_DOCKER_DEPLOY_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_DEPLOY_IMAGE to an existing shell image")
	}
	pullDigest := os.Getenv("KY_TEST_DOCKER_PULL_DIGEST")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	project := "kyyarddeployfixture"
	name := project + "-web-1"
	network := project + "_default"
	fixtureImage := project + ":local"
	builder := project + "-build"
	volume := "kyyard_it_data"
	bindDir := t.TempDir()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	removeFixtureVolume := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_ = exec.CommandContext(cctx, "docker", "volume", "rm", volume).Run()
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
	// Idempotent setup: a prior aborted run may have left the container, network or volume behind.
	removeFixtureContainers()
	removeFixtureImage()
	removeFixtureNetwork()
	removeFixtureVolume()
	t.Cleanup(func() {
		removeFixtureContainers()
		removeFixtureImage()
		removeFixtureNetwork()
		removeFixtureVolume()
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
	// The one-off carries the project label so an aborted run's leftover is cleaned up with the rest.
	if out, err := docker("run", "--rm", "--pull", "never", "--label", "com.docker.compose.project="+project, "-v", volume+":/data", image, "sh", "-c", "echo persisted > /data/marker"); err != nil {
		t.Fatalf("volume fixture: %v: %s", err, out)
	}
	oldID, err := docker("run", "-d", "--pull", "never", "--name", name, "--network", network, "--network-alias", "web",
		"-v", volume+":/data", "-v", bindDir+":/cfg:ro",
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
		Mounts: []protocol.Mount{{Kind: protocol.MountVolume, Source: volume, Target: "/data"}, {Kind: protocol.MountBind, Source: bindDir, Target: "/cfg", ReadOnly: true}},
	}}, Volumes: []string{volume}}
	pulledRef := "docker.io/library/alpine@" + pullDigest
	if pullDigest != "" {
		workerName := project + "-worker-1"
		workerID, err := docker("run", "-d", "--pull", "never", "--name", workerName, "--network", network,
			"--label", "com.docker.compose.project="+project, "--label", "com.docker.compose.service=worker", fixtureImage)
		if err != nil {
			t.Fatalf("worker fixture: %v: %s", err, workerID)
		}
		var worker struct{ Created time.Time }
		if raw, err = docker("inspect", "--format", "{{json .}}", workerID); err != nil || json.Unmarshal([]byte(raw), &worker) != nil {
			t.Fatalf("worker identity: %v: %s", err, raw)
		}
		req.Services = append(req.Services, protocol.DeploymentService{
			Name: "worker", ContainerName: workerName, Pull: &protocol.ImagePull{Reference: pulledRef, Digest: pullDigest, Tag: "docker.io/library/alpine:3.24"},
			Replaces: protocol.InspectionTarget{ContainerID: workerID, ImageID: identity.Image, CreatedUnix: worker.Created.Unix()},
		})
	}
	res := New("/var/run/docker.sock").Deploy(ctx, req)
	serialized, _ := json.Marshal(res)
	if strings.Contains(string(serialized), "deploy-secret-canary") {
		t.Fatal("environment value reached the result")
	}
	if res.Outcome != protocol.OutcomeSucceeded || len(res.Services) != len(req.Services) {
		t.Fatalf("deploy: %s", serialized)
	}
	if pullDigest != "" {
		pulled := res.Services[1]
		if pulled.ImageDigest != pullDigest {
			t.Fatalf("pulled identity: %+v", pulled)
		}
		out, err := docker("image", "inspect", "--format", "{{json .RepoDigests}}", pulled.ImageID)
		var digests []string
		if err != nil || json.Unmarshal([]byte(out), &digests) != nil || !slices.ContainsFunc(digests, func(d string) bool { return strings.HasSuffix(d, "@"+pullDigest) }) {
			t.Fatalf("pulled image %s repo digests: %v %s", pulled.ImageID, err, out)
		}
		if out, err = docker("inspect", "--format", "{{.Image}}", pulled.ContainerID); err != nil || out != pulled.ImageID {
			t.Fatalf("worker container image: %v %s", err, out)
		}
		if out, err = docker("image", "inspect", "--format", "{{.Id}}", "alpine:3.24"); err != nil || out != pulled.ImageID {
			t.Fatalf("alpine:3.24 names %s, want the pulled %s: %v", out, pulled.ImageID, err)
		}
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
	if out, err := docker("exec", res.Services[0].ContainerID, "cat", "/data/marker"); err != nil || out != "persisted" {
		t.Fatalf("volume data after recreate: %v %q", err, out)
	}
	type mount struct {
		Type, Name, Source, Destination string
		RW                              bool
	}
	var mounts []mount
	out, err := docker("inspect", "--format", "{{json .Mounts}}", res.Services[0].ContainerID)
	if err != nil || json.Unmarshal([]byte(out), &mounts) != nil || len(mounts) != 2 ||
		!slices.ContainsFunc(mounts, func(m mount) bool { return m.Type == "bind" && m.Source == bindDir && m.Destination == "/cfg" && !m.RW }) ||
		!slices.ContainsFunc(mounts, func(m mount) bool { return m.Type == "volume" && m.Name == volume && m.Destination == "/data" && m.RW }) {
		t.Fatalf("mounts after recreate: %v %s", err, out)
	}
}

// Same fixture pattern as TestDeployRealDocker, with a named and an anonymous volume attached:
// Remove deletes the container and leaves the project network and both volumes in place. Only
// the anonymous one proves no v=1 was sent; Docker never deletes a named volume with a container.
func TestRemoveRealDocker(t *testing.T) {
	image := os.Getenv("KY_TEST_DOCKER_DEPLOY_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_DEPLOY_IMAGE to an existing shell image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	project := "kyyardremovefixture"
	name := project + "-web-1"
	network := project + "_default"
	volume := project + "_data"
	fixtureImage := project + ":local"
	builder := project + "-build"
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	var anonymous string // the fixture's anonymous volume, once known
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		ids, _ := exec.CommandContext(cctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).Output()
		for _, id := range strings.Fields(string(ids)) {
			_ = exec.CommandContext(cctx, "docker", "rm", "-f", id).Run()
		}
		_ = exec.CommandContext(cctx, "docker", "rm", "-f", builder).Run()
		_ = exec.CommandContext(cctx, "docker", "rmi", "-f", fixtureImage).Run()
		_ = exec.CommandContext(cctx, "docker", "network", "rm", network).Run()
		_ = exec.CommandContext(cctx, "docker", "volume", "rm", volume).Run()
		if anonymous != "" {
			_ = exec.CommandContext(cctx, "docker", "volume", "rm", anonymous).Run()
		}
	}
	// Idempotent setup: a prior aborted run may have left any of these behind.
	cleanup()
	t.Cleanup(cleanup)
	if out, err := docker("create", "--pull", "never", "--name", builder, image); err != nil {
		t.Fatalf("fixture image source: %v: %s", err, out)
	}
	if out, err := docker("commit", "--change", `CMD ["sleep","300"]`, builder, fixtureImage); err != nil {
		t.Fatalf("fixture image: %v: %s", err, out)
	}
	if out, err := docker("rm", "-f", builder); err != nil {
		t.Fatalf("fixture image source removal: %v: %s", err, out)
	}
	if out, err := docker("network", "create", network); err != nil {
		t.Fatalf("fixture network: %v: %s", err, out)
	}
	id, err := docker("run", "-d", "--pull", "never", "--name", name, "--network", network, "-v", volume+":/data", "-v", "/scratch",
		"--label", "com.docker.compose.project="+project, "--label", "com.docker.compose.service=web", fixtureImage)
	if err != nil {
		t.Fatalf("fixture: %v: %s", err, id)
	}
	raw, err := docker("inspect", "--format", "{{json .}}", id)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Image   string
		Created time.Time
		Mounts  []struct{ Name, Destination string }
	}
	if err = json.Unmarshal([]byte(raw), &identity); err != nil {
		t.Fatal(err)
	}
	for _, m := range identity.Mounts {
		if m.Destination == "/scratch" {
			anonymous = m.Name
		}
	}
	if anonymous == "" {
		t.Fatalf("fixture has no anonymous volume: %s", raw)
	}
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Endpoint: "ep_1", Project: project, Deadline: time.Now().Add(5 * time.Minute),
		Containers: []protocol.RemovalTarget{{Service: "web", Target: protocol.InspectionTarget{ContainerID: id, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()}}}}
	res := New("/var/run/docker.sock").Remove(ctx, req)
	if serialized, _ := json.Marshal(res); res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("remove: %s", serialized)
	}
	if _, err = docker("inspect", id); err == nil {
		t.Fatal("container survived removal")
	}
	if out, err := docker("network", "inspect", network); err != nil {
		t.Fatalf("project network removed: %s", out)
	}
	if out, err := docker("volume", "inspect", volume); err != nil {
		t.Fatalf("named volume removed: %s", out)
	}
	if out, err := docker("volume", "inspect", anonymous); err != nil {
		t.Fatalf("anonymous volume removed: %s", out)
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
