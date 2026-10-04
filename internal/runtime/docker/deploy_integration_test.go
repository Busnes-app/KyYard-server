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
	req := protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: project, Revision: 3, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Services: []protocol.DeploymentService{{
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
			Mounts:   []protocol.Mount{},
		})
	}
	res := New("/var/run/docker.sock").Deploy(ctx, req, func() {})
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
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: project, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute),
		Containers: []protocol.RemovalTarget{{Service: "web", Target: protocol.InspectionTarget{ContainerID: id, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()}}}}
	res := New("/var/run/docker.sock").Remove(ctx, req, func() {})
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
		if outcome, _, _ := r.outcomeFor(parent, tc.err, tc.status); outcome != tc.outcome {
			t.Fatalf("%s: %s, want %s", name, outcome, tc.outcome)
		}
	}
}

// Opt-in, already-present image only, like TestDeployRealDocker. The started hook runs at the
// last moment before phase two; a memory limit set there must be denied at recheck and the old
// container left running under its name.
func TestRecheckRealDocker(t *testing.T) {
	image := os.Getenv("KY_TEST_DOCKER_DEPLOY_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_DEPLOY_IMAGE to an existing shell image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	project := "kyyardrecheckfixture"
	name := project + "-web-1"
	network := project + "_default"
	fixtureImage := project + ":local"
	builder := project + "-build"
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		ids, _ := exec.CommandContext(cctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).Output()
		for _, id := range strings.Fields(string(ids)) {
			_ = exec.CommandContext(cctx, "docker", "rm", "-fv", id).Run()
		}
		_ = exec.CommandContext(cctx, "docker", "rm", "-fv", builder).Run()
		_ = exec.CommandContext(cctx, "docker", "rmi", "-f", fixtureImage).Run()
		_ = exec.CommandContext(cctx, "docker", "network", "rm", network).Run()
	}
	cleanup() // a prior aborted run may have left any of it behind
	t.Cleanup(cleanup)
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
	req := protocol.DeploymentRequest{Deployment: "4a3c2d1e-9f8b-4c7d-8e6f-2d3e4f5a6b7c", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: project, Revision: 2, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute), Services: []protocol.DeploymentService{{
		Name: "web", ContainerName: name, ImageID: identity.Image, Restart: "no", Mounts: []protocol.Mount{},
		Replaces: protocol.InspectionTarget{ContainerID: oldID, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()},
	}}}
	res := New("/var/run/docker.sock").Deploy(ctx, req, func() {
		if out, err := docker("update", "--memory", "64m", "--memory-swap", "128m", oldID); err != nil {
			t.Errorf("docker update: %v: %s", err, out)
		}
	})
	serialized, _ := json.Marshal(res)
	if res.Outcome != protocol.OutcomeDenied || len(res.Steps) < 3 || res.Steps[2].Step != protocol.StepRecheck || res.Steps[2].Code != "configuration_drift" || res.Code != protocol.ResultStepFailed {
		t.Fatalf("recheck: %s", serialized)
	}
	if out, err := docker("inspect", "--format", "{{.Id}} {{.State.Running}}", name); err != nil || out != oldID+" true" {
		t.Fatalf("the old container was touched: %v %s", err, out)
	}
}

// explicitFixture runs a disposable container on the default bridge from an already-present
// image, labelled kyyard.test=<name> so cleanup removes it and anything created in its place
// (before, for an aborted run's leftover, and after). It returns the explicit frame that
// recreates it with the same command and label, and its identity.
func explicitFixture(t *testing.T, ctx context.Context, name string, options ...string) (protocol.DeploymentRequest, string) {
	t.Helper()
	image := os.Getenv("KY_TEST_DOCKER_INSPECTION_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_INSPECTION_IMAGE to an existing shell image")
	}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		ids, _ := exec.CommandContext(cctx, "docker", "ps", "-aq", "--filter", "label=kyyard.test="+name).Output()
		for _, id := range strings.Fields(string(ids)) {
			_ = exec.CommandContext(cctx, "docker", "rm", "-fv", id).Run()
		}
		_ = exec.CommandContext(cctx, "docker", "rm", "-fv", name).Run()
	}
	cleanup()
	t.Cleanup(cleanup)
	args := append([]string{"run", "-d", "--pull", "never", "--name", name, "--network", "bridge", "--env", "FOO=old", "--label", "kyyard.test=" + name}, options...)
	args = append(args, image, "sleep", "300")
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	var identity struct {
		Image   string
		Created time.Time
	}
	raw, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .}}", id).Output()
	if err != nil || json.Unmarshal(raw, &identity) != nil {
		t.Fatalf("fixture identity: %v", err)
	}
	return protocol.DeploymentRequest{
		Deployment: "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1",
		Project: protocol.ExplicitProject, Revision: protocol.ExplicitRevision, Explicit: true, IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute),
		Services: []protocol.DeploymentService{{
			Name: "web", ContainerName: name, ImageID: identity.Image, Restart: "no", Env: map[string]string{"FOO": "old"}, Mounts: []protocol.Mount{},
			Replaces: protocol.InspectionTarget{ContainerID: id, ImageID: identity.Image, CreatedUnix: identity.Created.Unix()},
			Explicit: &protocol.ExplicitService{Command: []string{"sleep", "300"}, Labels: map[string]string{"kyyard.test": name}, NetworkMode: "bridge", Networks: []protocol.NetworkAttachmentSpec{{Name: "bridge"}}},
		}},
	}, id
}

func dockerState(ctx context.Context, ref string) (id, state string) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Id}} {{.State.Status}}", ref).Output()
	if err != nil {
		return "", "absent"
	}
	id, state, _ = strings.Cut(strings.TrimSpace(string(out)), " ")
	return id, state
}

// An explicit recreate changes an env value and the memory limit; the new container runs with
// them under the old name and the old container is gone.
func TestRecreateRealDocker(t *testing.T) {
	t.Run("preserves requested MAC", testRecreateMACAddressRealDocker)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	name := "kyyard-recreate-fixture"
	req, oldID := explicitFixture(t, ctx, name)
	req.Services[0].Env["FOO"] = "new"
	req.Services[0].Explicit.Resources.MemoryBytes = 64 << 20
	c := New("/var/run/docker.sock")
	res := c.Deploy(ctx, req, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil || len(res.Services) != 1 {
		t.Fatalf("deploy: %+v", res)
	}
	id := res.Services[0]
	conf, err := c.ReadConfiguration(ctx, protocol.InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix})
	if err != nil {
		t.Fatal(err)
	}
	if conf.Name != name || conf.Resources.MemoryBytes != 64<<20 || !slices.Contains(conf.Env, protocol.EnvEntry{Name: "FOO", Value: "new"}) || conf.Labels["kyyard.test"] != name {
		t.Fatalf("configuration: name=%q memory=%d env=%v labels=%v", conf.Name, conf.Resources.MemoryBytes, conf.Env, conf.Labels)
	}
	if _, state := dockerState(ctx, id.ContainerID); state != "running" {
		t.Fatalf("new container %s", state)
	}
	if _, state := dockerState(ctx, oldID); state != "absent" {
		t.Fatalf("old container %s", state)
	}
}

func testRecreateMACAddressRealDocker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const mac = "02:42:ac:11:00:05"
	req, oldID := explicitFixture(t, ctx, "kyyard-recreate-mac-fixture", "--mac-address", mac, "--label", "CI_DOCKER_VERSION="+strings.Repeat("v", 981))
	c := New("/var/run/docker.sock")
	conf, err := c.ReadConfiguration(ctx, req.Services[0].Replaces)
	if err != nil || len(conf.Unsupported) != 0 {
		t.Fatalf("configuration blocked: %+v, %v", conf, err)
	}
	req.Services[0].Explicit.Labels = conf.Labels
	req.Services[0].Env["FOO"] = "new"
	res := c.Deploy(ctx, req, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil || len(res.Services) != 1 {
		t.Fatalf("recreate: %+v", res)
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.NetworkSettings.Networks.bridge.MacAddress}}", res.Services[0].ContainerID).Output()
	if err != nil || strings.TrimSpace(string(out)) != mac {
		t.Fatalf("MAC not preserved: %q, %v", out, err)
	}
	out, err = exec.CommandContext(ctx, "docker", "inspect", "--format", "{{index .Config.Labels \"CI_DOCKER_VERSION\"}}", res.Services[0].ContainerID).Output()
	if err != nil || strings.TrimSpace(string(out)) != strings.Repeat("v", 981) {
		t.Fatalf("build label not preserved: bytes=%d, %v", len(out), err)
	}
	if _, state := dockerState(ctx, oldID); state != "absent" {
		t.Fatalf("old container %s", state)
	}
}

// An explicit run creates a container from an image already present; Remove takes it away.
func TestRunRealDocker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	name := "kyyard-run-fixture"
	req, fixtureID := explicitFixture(t, ctx, name+"-source")
	s := &req.Services[0]
	s.Name, s.ContainerName, s.Replaces, s.Explicit.Labels["kyyard.test"] = "runner", name, protocol.InspectionTarget{}, name+"-source"
	c := New("/var/run/docker.sock")
	res := c.Deploy(ctx, req, func() {})
	steps := []string{}
	for _, st := range res.Steps {
		steps = append(steps, st.Step+"="+st.Outcome)
	}
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil || strings.Join(steps, ",") != "image=succeeded,create=succeeded,start=succeeded" {
		t.Fatalf("run: %+v", res)
	}
	id := res.Services[0]
	if got, state := dockerState(ctx, name); got != id.ContainerID || state != "running" {
		t.Fatalf("run container %s %s", got, state)
	}
	if _, state := dockerState(ctx, fixtureID); state != "running" {
		t.Fatalf("an unrelated container was touched: %s", state)
	}
	removed := c.Remove(ctx, protocol.RemovalRequest{Deployment: req.Deployment, RequestID: req.RequestID, Endpoint: req.Endpoint, Project: req.Project, IssuedAt: time.Now(), Deadline: time.Now().Add(2 * time.Minute),
		Containers: []protocol.RemovalTarget{{Service: "runner", Target: protocol.InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}}}}, func() {})
	if removed.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("remove: %+v", removed)
	}
	if _, state := dockerState(ctx, name); state != "absent" {
		t.Fatalf("run container after removal: %s", state)
	}
}

// A recreate whose new container cannot start is rolled back: the old container runs again
// under its name and the new one is gone. The command names no executable, so start itself
// fails; a command that starts and then exits would not.
func TestRecreateRollbackRealDocker(t *testing.T) {
	// A command that cannot start, and one that starts and exits at once (C2).
	for _, command := range [][]string{{"/kyyard-no-such-executable"}, {"sh", "-c", "exit 1"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		name := "kyyard-rollback-fixture"
		req, oldID := explicitFixture(t, ctx, name)
		req.Services[0].Explicit.Command = command
		res := New("/var/run/docker.sock").Deploy(ctx, req, func() {})
		if res.Outcome != protocol.OutcomeFailed || res.Validate() != nil {
			t.Fatalf("%v deploy: %+v", command, res)
		}
		var start, rollback protocol.DeploymentStep
		for _, s := range res.Steps {
			switch s.Step {
			case protocol.StepStart:
				start = s
			case protocol.StepRollback:
				rollback = s
			}
		}
		if start.Code != "start_failed_rolled_back" || rollback.Outcome != protocol.OutcomeSucceeded {
			t.Fatalf("%v steps: %+v", command, res.Steps)
		}
		if id, state := dockerState(ctx, name); id != oldID || state != "running" {
			t.Fatalf("%v old container under its name: %s %s", command, id, state)
		}
		out, _ := exec.CommandContext(ctx, "docker", "ps", "-aq", "--no-trunc", "--filter", "label=kyyard.test="+name).Output()
		if ids := strings.Fields(string(out)); len(ids) != 1 || ids[0] != oldID {
			t.Fatalf("%v containers left: %v", command, ids)
		}
	}
}

// waitStopped against a real daemon, whose wait answers 200 at once and writes its body only when
// the container stops: a running container outlasts the bound, and a stop ends the wait.
func TestStopWaitRealDocker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, id := explicitFixture(t, ctx, "kyyard-stopwait-fixture")
	c := New("/var/run/docker.sock")
	began := time.Now()
	if stopped, err := c.waitStopped(ctx, id, 2*time.Second); stopped || err != nil {
		t.Fatalf("running container: stopped=%v err=%v", stopped, err)
	}
	t.Logf("running: wait ended at its 2s bound after %v", time.Since(began).Round(time.Millisecond))
	stop := make(chan error, 1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		stop <- exec.CommandContext(ctx, "docker", "stop", "-t", "1", id).Run()
	}()
	began = time.Now()
	if stopped, err := c.waitStopped(ctx, id, 20*time.Second); !stopped || err != nil {
		t.Fatalf("stopping container: stopped=%v err=%v", stopped, err)
	}
	t.Logf("stopping: wait returned the body after %v (docker stop -t 1 sent at 500ms)", time.Since(began).Round(time.Millisecond))
	if err := <-stop; err != nil {
		t.Fatalf("docker stop: %v", err)
	}
	if _, state := dockerState(ctx, id); state != "exited" {
		t.Fatalf("state after the wait: %s", state)
	}
}
