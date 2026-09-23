package api_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// Opt-in, already-present image only: import, adopt, map, plan, apply and remove through the real
// server, agent client and Docker Engine. Everything created carries the fixture project
// label or name and is removed by label, name and tag; nothing else on the host is touched.
func TestApplyRealDocker(t *testing.T) {
	image := os.Getenv("KY_TEST_DOCKER_DEPLOY_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_DEPLOY_IMAGE to an existing shell image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const (
		project = "kyyardapplyfixture"
		name    = project + "-web-1"
		network = project + "_default"
		fixture = project + ":local"
		builder = project + "-build"
		canary  = "apply-secret-canary"
	)
	run := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		ids, _ := exec.CommandContext(cctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).Output()
		for _, id := range strings.Fields(string(ids)) {
			_ = exec.CommandContext(cctx, "docker", "rm", "-fv", id).Run()
		}
		_ = exec.CommandContext(cctx, "docker", "rm", "-fv", builder).Run()
		_ = exec.CommandContext(cctx, "docker", "rmi", "-f", fixture).Run()
		_ = exec.CommandContext(cctx, "docker", "network", "rm", network).Run()
	}
	cleanup() // a prior aborted run may have left any of it behind
	t.Cleanup(cleanup)
	if out, err := run("create", "--pull", "never", "--name", builder, image); err != nil {
		t.Fatalf("fixture image source: %v: %s", err, out)
	}
	if out, err := run("commit", "--change", `CMD ["sleep","300"]`, builder, fixture); err != nil {
		t.Fatalf("fixture image: %v: %s", err, out)
	}
	if out, err := run("rm", "-fv", builder); err != nil {
		t.Fatalf("fixture image source removal: %v: %s", err, out)
	}
	if out, err := run("network", "create", network); err != nil {
		t.Fatalf("fixture network: %v: %s", err, out)
	}
	oldID, err := run("run", "-d", "--pull", "never", "--name", name, "--network", network, "--network-alias", "web",
		"--label", "com.docker.compose.project="+project, "--label", "com.docker.compose.service=web", fixture)
	if err != nil {
		t.Fatalf("fixture: %v: %s", err, oldID)
	}

	s, st, _ := setupTestServer(t)
	ts := st.Tenancy()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}))
	must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}))
	admin := loginAs(t, s, st, "applyadmin", "user")
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_applyadmin", Role: store.RoleOrganizationAdmin, Status: "active"}))
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	defer s.BeginShutdown()
	ag := enrollAgent(t, s, st, admin, "apply-real")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal(w.Code)
	}
	engine := docker.New("/var/run/docker.sock")
	agentCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- client.Run(agentCtx, &client.Identity{EndpointID: ag.id, PrivateKey: ag.priv, InstanceFingerprint: ag.inst, Server: httpSrv.URL}, client.Options{
			IdentityDir: t.TempDir(), Snapshot: engine.Snapshot, Inspect: engine.InspectContainer, Deploy: engine.Deploy, Remove: engine.Remove, InventoryEvery: 2 * time.Second,
		})
	}()
	defer func() { stop(); <-done; s.WaitDetached() }()

	var bodies strings.Builder
	request := func(method, path, body string, status int) string {
		t.Helper()
		w := tenantRequest(s, admin, method, path, body, true)
		bodies.WriteString(w.Body.String())
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	eventually := func(what string, within time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(within)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within %s", what, within)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	base := "/api/organizations/a/environments/env-a/applications"
	importBody, _ := json.Marshal(map[string]string{"name": project, "compose": "services: {web: {image: " + fixture + ", restart: unless-stopped, environment: {TOKEN: " + canary + "}}}"})
	var app store.Application
	must(json.Unmarshal([]byte(request("POST", base, string(importBody), 201)), &app))
	adoption := base + "/" + app.ID + "/adoption"
	// Adoption needs fresh inventory, which the agent sends once it is connected and active.
	var preview store.AdoptionPreview
	eventually("adoption preview", 30*time.Second, func() bool {
		w := tenantRequest(s, admin, "GET", adoption+"?endpoint="+ag.id+"&project="+project, "", true)
		return w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &preview) == nil && slices.ContainsFunc(preview.Containers, func(c store.AdoptedContainer) bool { return c.ID == oldID })
	})
	adoptionBody, _ := json.Marshal(store.AdoptionRequest{EndpointID: ag.id, Project: project, Digest: preview.Digest, Confirm: project})
	var instance store.ApplicationInstance
	must(json.Unmarshal([]byte(request("POST", adoption, string(adoptionBody), 201)), &instance))
	mapping := base + "/" + app.ID + "/mapping"
	var mapped store.ApplicationMapping
	must(json.Unmarshal([]byte(request("GET", mapping, "", 200)), &mapped))
	mappingBody, _ := json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: project, Bindings: map[string]string{"web": oldID}})
	request("PUT", mapping, string(mappingBody), 204)
	deployments := base + "/" + app.ID + "/deployments"
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: instance.ID, MappingVersion: mapped.Version + 1, Revision: 1, Confirm: project})
	var planned store.Deployment
	must(json.Unmarshal([]byte(request("POST", deployments, string(planBody), 201)), &planned))
	request("POST", deployments+"/"+planned.ID+"/apply", `{"confirm":"`+project+`"}`, 202)

	var settled store.Deployment
	eventually("deployment settled", 90*time.Second, func() bool {
		w := tenantRequest(s, admin, "GET", deployments+"/"+planned.ID, "", true)
		return w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &settled) == nil && settled.State != "applying"
	})
	serialized, _ := json.Marshal(settled)
	bodies.Write(serialized)
	if settled.State != protocol.OutcomeSucceeded || settled.Result == nil || len(settled.Result.Services) != 1 {
		t.Fatalf("deployment: %s", serialized)
	}
	newID := settled.Result.Services[0].ContainerID
	if newID == oldID {
		t.Fatal("the container was not replaced")
	}
	// The mapping reads through inventory, so it answers once the agent has reported the new
	// container.
	eventually("mapping rebound", 30*time.Second, func() bool {
		w := tenantRequest(s, admin, "GET", mapping, "", true)
		return w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &mapped) == nil
	})
	bodies.WriteString(request("GET", mapping, "", 200))
	if len(mapped.Preview.Containers) != 1 || mapped.Preview.Containers[0].ID != newID || mapped.Bindings["web"] != newID {
		t.Fatalf("mapping still names the replaced container: %+v", mapped)
	}
	var instances []store.ApplicationInstance
	must(json.Unmarshal([]byte(request("GET", base+"/instances", "", 200)), &instances))
	if len(instances) != 1 || instances[0].CurrentRevision != 1 {
		t.Fatalf("instance revision: %+v", instances)
	}

	raw, err := run("inspect", "--format", "{{json .}}", newID)
	if err != nil {
		t.Fatalf("inspect new container: %v: %s", err, raw)
	}
	var created struct {
		Name            string
		State           struct{ Running bool }
		Config          struct{ Env []string }
		NetworkSettings struct {
			Networks map[string]struct{ Aliases []string }
		}
	}
	must(json.Unmarshal([]byte(raw), &created))
	net, onNetwork := created.NetworkSettings.Networks[network]
	if !created.State.Running || created.Name != "/"+name || !onNetwork || !slices.Contains(net.Aliases, "web") || !slices.Contains(created.Config.Env, "TOKEN="+canary) {
		t.Fatalf("new container: running=%v name=%s networks=%v env has token=%v", created.State.Running, created.Name, created.NetworkSettings.Networks, slices.Contains(created.Config.Env, "TOKEN="+canary))
	}
	if out, err := run("inspect", oldID); err == nil {
		t.Fatalf("old container survived: %s", out)
	}

	// Removal stops and deletes the adopted container and keeps the network; the application
	// stays, marked removed, until it is discarded.
	removalBody, _ := json.Marshal(store.RemovalBody{InstanceID: instance.ID, Confirm: project})
	var removing store.Deployment
	must(json.Unmarshal([]byte(request("POST", base+"/"+app.ID+"/removal", string(removalBody), 202)), &removing))
	eventually("removal settled", 90*time.Second, func() bool {
		w := tenantRequest(s, admin, "GET", deployments+"/"+removing.ID, "", true)
		return w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &settled) == nil && settled.State != "applying"
	})
	if serialized, _ = json.Marshal(settled); settled.State != protocol.OutcomeSucceeded || settled.Kind != "remove" {
		t.Fatalf("removal: %s", serialized)
	}
	if out, err := run("inspect", newID); err == nil {
		t.Fatalf("the removed container survived: %s", out)
	}
	if out, err := run("network", "inspect", network); err != nil {
		t.Fatalf("removal took the network: %v: %s", err, out)
	}
	must(json.Unmarshal([]byte(request("GET", base+"/instances", "", 200)), &instances))
	if len(instances) != 0 {
		t.Fatalf("the instance was not released: %+v", instances)
	}
	var apps []store.Application
	must(json.Unmarshal([]byte(request("GET", base, "", 200)), &apps))
	if len(apps) != 1 || apps[0].RemovedAt == nil {
		t.Fatalf("the application is not marked removed: %+v", apps)
	}
	request("DELETE", base+"/"+app.ID, `{"expected_revision":1}`, 204)
	if strings.Contains(bodies.String(), canary) {
		t.Fatal("a resolved value reached an API body")
	}
}
