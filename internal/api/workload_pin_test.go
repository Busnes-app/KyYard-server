package api_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func withPull(t *testing.T, body string, pull ...string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	m["pull"] = pull
	raw, _ := json.Marshal(m)
	return string(raw)
}

func (f *workloadFleet) anonymousPulls(t *testing.T) {
	t.Helper()
	if err := f.st.Tenancy().SetAnonymousPull(f.ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: "a"}, true); err != nil {
		t.Fatal(err)
	}
}

func (f *workloadFleet) appliedImages(t *testing.T) []string {
	t.Helper()
	e := readEnvelope(t, f.ctx, f.conn)
	var frame protocol.WorkloadApply
	if e.Type != protocol.TypeWorkloadApply || json.Unmarshal(e.Payload, &frame) != nil || frame.Validate(time.Now()) != nil {
		t.Fatalf("frame %s %s", e.Type, e.Payload)
	}
	var out []string
	for _, c := range append(frame.Spec.Containers, frame.Spec.InitContainers...) {
		out = append(out, c.Image)
	}
	return out
}

// Pull pins each named container at its tag's registry digest, keeps the tag, re-resolves an
// earlier pin from its tag, and Heads a repeated reference once.
func TestWorkloadApplyPinsPulledImages(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	f.anonymousPulls(t)
	resolver := &countingResolver{}
	api.SetDigestResolverForTest(f.s, resolver)
	pinned := "sha256:" + strings.Repeat("f", 64)
	cfg := workloadConfiguration(protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "web"})
	cfg.Containers[0].Image = "ghcr.io/acme/web:2@sha256:" + strings.Repeat("1", 64)
	side := cfg.Containers[0]
	side.Name, side.Image, side.Env = "side", "ghcr.io/acme/web:2", []protocol.WorkloadEnv{}
	keep := side
	keep.Name, keep.Image = "keep", "ghcr.io/acme/other:1"
	cfg.Containers = append(cfg.Containers, side, keep)
	w := tenantRequest(f.s, f.org, "POST", f.webWorkload+"/apply", withPull(t, applyBody(t, cfg, "web"), "web", "side"), true)
	if w.Code != 202 {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
	got := f.appliedImages(t)
	want := []string{"ghcr.io/acme/web:2@" + pinned, "ghcr.io/acme/web:2@" + pinned, "ghcr.io/acme/other:1"}
	if strings.Join(got, ",") != strings.Join(want, ",") || resolver.calls.Load() != 1 {
		t.Fatalf("images %v, %d heads", got, resolver.calls.Load())
	}
}

// Refusals spend no registry call and record no command.
func TestWorkloadPinRefusals(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	resolver := &countingResolver{}
	api.SetDigestResolverForTest(f.s, resolver)
	cfg := workloadConfiguration(protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "web"})
	managed := workloadConfiguration(protocol.WorkloadRef{Namespace: "shop", Kind: "deployment", Name: "api"})
	for name, c := range map[string]struct {
		path, body string
		code       int
	}{
		"unknown container":  {f.webWorkload + "/apply", withPull(t, applyBody(t, cfg, "web"), "ghost"), 400},
		"duplicate":          {f.webWorkload + "/apply", withPull(t, applyBody(t, cfg, "web"), "web", "web"), 400},
		"managed":            {f.clusterPath + "/workloads/shop/deployment/api/apply", withPull(t, applyBody(t, managed, "api"), "web"), 409},
		"wrong confirm":      {f.webWorkload + "/apply", withPull(t, applyBody(t, cfg, "api"), "web"), 400},
		"no registry access": {f.webWorkload + "/apply", withPull(t, applyBody(t, cfg, "web"), "web"), 409},
	} {
		if w := tenantRequest(f.s, f.org, "POST", c.path, c.body, true); w.Code != c.code {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	f.anonymousPulls(t)
	digestOnly := *cfg
	digestOnly.Containers = []protocol.WorkloadContainer{cfg.Containers[0]}
	digestOnly.Containers[0].Image = "ghcr.io/acme/web@sha256:" + strings.Repeat("1", 64)
	if w := tenantRequest(f.s, f.org, "POST", f.webWorkload+"/apply", withPull(t, applyBody(t, &digestOnly, "web"), "web"), true); w.Code != 422 || !strings.Contains(w.Body.String(), "image_unresolved") {
		t.Fatalf("digest only: %d %s", w.Code, w.Body.String())
	}
	if n := resolver.calls.Load(); n != 0 {
		t.Fatalf("%d registry calls for refused pins", n)
	}
	api.SetDigestResolverForTest(f.s, &fakeDigests{err: errors.Join(errors.New("sensitive-registry-diagnostic"), registry.ErrNotFound)})
	w := tenantRequest(f.s, f.org, "POST", f.webWorkload+"/apply", withPull(t, applyBody(t, cfg, "web"), "web"), true)
	if w.Code != 422 || !strings.Contains(w.Body.String(), "image_unresolved") || strings.Contains(w.Body.String(), "sensitive") {
		t.Fatalf("unresolved: %d %s", w.Code, w.Body.String())
	}
	f.sync(t) // no frame was sent
	commands, err := f.st.Tenancy().ListCommands(f.ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: "a"}, f.cluster.id, "", "", 50)
	if err != nil || len(commands) != 0 {
		t.Fatalf("refusals recorded commands: %v %v", commands, err)
	}
}

// A run pins when asked and, unticked, still runs with no registry access configured.
func TestWorkloadRunPins(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	api.SetDigestResolverForTest(f.s, fixedResolver{"sha256:" + strings.Repeat("e", 64)})
	if w := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/workloads", workloadRunBody(t, "shop", "fresh", "fresh"), true); w.Code != 202 {
		t.Fatalf("unpinned run: %d %s", w.Code, w.Body.String())
	}
	if got := f.appliedImages(t); got[0] != "ghcr.io/acme/web:1" {
		t.Fatalf("unpinned run image %v", got)
	}
	f.settleLast(t)
	f.anonymousPulls(t)
	if w := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/workloads", withPull(t, workloadRunBody(t, "shop", "pinned", "pinned"), "web"), true); w.Code != 202 {
		t.Fatalf("pinned run: %d %s", w.Code, w.Body.String())
	}
	if got := f.appliedImages(t); got[0] != "ghcr.io/acme/web:1@sha256:"+strings.Repeat("e", 64) {
		t.Fatalf("pinned run image %v", got)
	}
}

func (f *workloadFleet) settleLast(t *testing.T) {
	t.Helper()
	commands, err := f.st.Tenancy().ListCommands(f.ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: "a"}, f.cluster.id, "", "", 1)
	if err != nil || len(commands) != 1 {
		t.Fatalf("last command: %v %v", commands, err)
	}
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: commands[0].ID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: protocol.WorkloadApplyService, Step: protocol.StepApply, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{}})
	f.sync(t)
}
