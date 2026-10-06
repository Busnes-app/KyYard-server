package api_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/registry"
)

type workloadCheck struct {
	Workload, Verdict, Detail string
	Containers                []struct{ Name, Reference, LocalDigest, RemoteDigest, Verdict, Detail string } `json:"containers"`
}

// report replaces the cluster inventory with web's pods running images and image IDs.
func (f *workloadFleet) report(t *testing.T, pods ...protocol.Pod) {
	t.Helper()
	writeEnvelope(t, f.ctx, f.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()) - 1000 + reports.Add(1), Kubernetes: &protocol.KubernetesInventory{
		Namespaces: []string{"shop", "other"},
		Workloads: []protocol.Workload{
			{Kind: "Deployment", Namespace: "shop", Name: "web"},
			{Kind: "Deployment", Namespace: "shop", Name: "api", Application: "shop", Instance: "shop"},
		},
		Pods: pods,
	}})
	f.sync(t)
}

func webPod(name string, containers ...protocol.PodContainer) protocol.Pod {
	return protocol.Pod{Namespace: "shop", Name: name, Phase: "Running", OwnerKind: "Deployment", OwnerName: "web", Containers: containers}
}

func (f *workloadFleet) check(t *testing.T, path string) workloadCheck {
	t.Helper()
	w := tenantRequest(f.s, f.org, "POST", path+"/updates/check", "", true)
	var out workloadCheck
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("check %s: %d %s", path, w.Code, w.Body.String())
	}
	return out
}

func TestWorkloadUpdateCheckVerdicts(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	f.anonymousPulls(t)
	remote := "sha256:" + strings.Repeat("b", 64)
	old := "sha256:" + strings.Repeat("a", 64)
	fake := &fakeDigests{digest: remote}
	api.SetDigestResolverForTest(f.s, fake)
	running := func(image, id string) protocol.PodContainer {
		return protocol.PodContainer{Name: "web", Image: image, ImageID: id, State: "running"}
	}
	for name, c := range map[string]struct {
		pods    []protocol.Pod
		verdict string
	}{
		"update":       {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2", "ghcr.io/acme/web@"+old))}, "update_available"},
		"current":      {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2", "ghcr.io/acme/web@"+remote))}, "up_to_date"},
		"earlier pin":  {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2@"+old, "ghcr.io/acme/web@"+old))}, "update_available"},
		"digest only":  {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web@"+old, "ghcr.io/acme/web@"+old))}, "pinned"},
		"cri-dockerd":  {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2", "docker-pullable://ghcr.io/acme/web@"+remote))}, "up_to_date"},
		"mid-rollout":  {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2", "ghcr.io/acme/web@"+old)), webPod("web-2", running("ghcr.io/acme/web:2", "ghcr.io/acme/web@"+remote))}, "unknown"},
		"no pods":      {nil, "unknown"},
		"not reported": {[]protocol.Pod{webPod("web-1", running("ghcr.io/acme/web:2", ""))}, "unknown"},
	} {
		f.report(t, c.pods...)
		api.ResetAttemptsForTest(f.s)
		if got := f.check(t, f.webWorkload); got.Verdict != c.verdict || got.Workload != "shop/deployment/web" {
			t.Errorf("%s: %+v", name, got)
		}
	}
	f.report(t, webPod("web-1", running("ghcr.io/acme/web:2", "ghcr.io/acme/web@"+old)))
	calls := fake.calls()
	api.ResetAttemptsForTest(f.s)
	if got := f.check(t, f.clusterPath+"/workloads/shop/deployment/api"); got.Verdict != "managed" || len(got.Containers) != 0 || fake.calls() != calls {
		t.Fatalf("managed: %+v, %d heads", got, fake.calls()-calls)
	}
	for _, c := range []struct {
		err    error
		detail string
	}{{registry.ErrUnauthorized, "unauthorized"}, {registry.ErrNotFound, "not_found"}, {registry.ErrRateLimited, "rate_limited"}, {registry.ErrPrivateDestination, "private_destination"}, {registry.ErrUnavailable, "unavailable"}} {
		api.SetDigestResolverForTest(f.s, &fakeDigests{err: fmt.Errorf("sensitive-registry-diagnostic: %w", c.err)})
		api.ResetAttemptsForTest(f.s)
		w := tenantRequest(f.s, f.org, "POST", f.webWorkload+"/updates/check", "", true)
		var got workloadCheck
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Verdict != "registry_error" || got.Detail != c.detail || got.Containers[0].Detail != c.detail || strings.Contains(w.Body.String(), "sensitive") {
			t.Errorf("%s: %d %s", c.detail, w.Code, w.Body.String())
		}
	}
}

func TestWorkloadUpdateCheckRefusals(t *testing.T) {
	f := newWorkloadFleet(t, workloadCaps...)
	api.SetDigestResolverForTest(f.s, &fakeDigests{digest: "sha256:" + strings.Repeat("b", 64)})
	f.report(t, webPod("web-1", protocol.PodContainer{Name: "web", Image: "ghcr.io/acme/web:2", ImageID: "ghcr.io/acme/web@sha256:" + strings.Repeat("a", 64)}))
	path := f.webWorkload + "/updates/check"
	if w := tenantRequest(f.s, f.org, "POST", path, "", false); w.Code != 403 {
		t.Fatalf("CSRF %d", w.Code)
	}
	if w := tenantRequest(f.s, f.org, "POST", path, "", true); w.Code != 409 || !strings.Contains(w.Body.String(), "anonymous_pull_disabled") && !strings.Contains(w.Body.String(), "registry_not_configured") {
		t.Fatalf("registry policy %d %s", w.Code, w.Body.String())
	}
	if w := tenantRequest(f.s, f.viewer, "POST", path, "", true); w.Code != 403 {
		t.Fatalf("viewer %d", w.Code)
	}
	if w := tenantRequest(f.s, f.org, "POST", f.hostPath+"/workloads/shop/deployment/web/updates/check", "", true); w.Code != 409 {
		t.Fatalf("docker endpoint %d %s", w.Code, w.Body.String())
	}
	if w := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/workloads/shop/pod/web-1/updates/check", "", true); w.Code != 400 {
		t.Fatalf("pod kind %d", w.Code)
	}
	if w := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/workloads/shop/deployment/ghost/updates/check", "", true); w.Code != 404 {
		t.Fatalf("absent %d", w.Code)
	}
	token, _ := f.service(t)
	if w := bearer(f.s, "POST", path, token); w.Code != 403 {
		t.Fatalf("service token %d", w.Code)
	}
	f.anonymousPulls(t)
	limited := false
	for range 13 {
		if w := tenantRequest(f.s, f.org, "POST", path, "", true); w.Code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("no rate limit after 13 checks")
	}
}
