package api_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestRunYAMLPreviewIsScopedAndNeverDispatches(t *testing.T) {
	f := newWorkloadFleet(t)
	source := `apiVersion: apps/v1
kind: Deployment
metadata: {name: new-web, namespace: shop}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers: [{name: web, image: nginx:stable}]
`
	body, _ := json.Marshal(map[string]string{"yaml": source})
	path := f.clusterPath + "/run-yaml/preview"
	if w := tenantRequest(f.s, f.org, "POST", path, string(body), false); w.Code != 403 {
		t.Fatalf("CSRF %d", w.Code)
	}
	if w := tenantRequest(f.s, f.viewer, "POST", path, string(body), true); w.Code != 403 {
		t.Fatalf("permission %d", w.Code)
	}
	if w := tenantRequest(f.s, f.org, "POST", path, string(body), true); w.Code != 200 || !strings.Contains(w.Body.String(), "run_manifest") {
		t.Fatalf("preview %d %s", w.Code, w.Body.String())
	}
	body, _ = json.Marshal(map[string]string{"yaml": strings.Replace(source, "namespace: shop", "namespace: foreign", 1)})
	if w := tenantRequest(f.s, f.org, "POST", path, string(body), true); w.Code != 422 {
		t.Fatalf("namespace %d %s", w.Code, w.Body.String())
	}
	commands, err := f.st.Tenancy().ListCommands(f.ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: "a"}, f.cluster.id, "", "", 50)
	if err != nil || len(commands) != 0 {
		t.Fatalf("preview recorded commands: %v %v", commands, err)
	}
}
func TestContainerUpdateCheckUsesImmutableLocalImageAndRegistryPolicy(t *testing.T) {
	f := inspectionFixture(t)
	old := "sha256:" + strings.Repeat("a", 64)
	remote := "sha256:" + strings.Repeat("b", 64)
	snap := protocol.Snapshot{Generation: uint64(time.Now().Unix()) + 10, ObservedAt: time.Now(), Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{{ID: terminalSpec.Container, Name: "web", Image: "ghcr.io/acme/web:latest", ImageID: terminalSpec.ImageID, CreatedAt: time.Unix(1700000000, 0), State: "running"}}, Images: []protocol.Image{{ID: terminalSpec.ImageID, Tags: []string{"ghcr.io/acme/web:latest"}, Digests: []string{"ghcr.io/acme/web@" + old}}}}
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInventory, snap)
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeHeartbeat, nil)
	readEnvelope(t, f.ctx, f.ag.conn)
	fake := &fakeDigests{digest: remote}
	api.SetDigestResolverForTest(f.s, fake)
	path := strings.TrimSuffix(f.path(), "exec") + "updates/check"
	if w := tenantRequest(f.s, f.admin, "POST", path, "", false); w.Code != 403 {
		t.Fatalf("CSRF %d", w.Code)
	}
	if w := tenantRequest(f.s, f.admin, "POST", path, "", true); w.Code != 409 {
		t.Fatalf("registry policy %d %s", w.Code, w.Body.String())
	}
	if w := tenantRequest(f.s, f.admin, "PUT", "/api/organizations/a/registry-policy", `{"anonymous_pull_enabled":true}`, true); w.Code != 204 {
		t.Fatalf("policy %d %s", w.Code, w.Body.String())
	}
	w := tenantRequest(f.s, f.admin, "POST", path, "", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"verdict":"update_available"`) || fake.calls() != 1 {
		t.Fatalf("check %d %s calls %d", w.Code, w.Body.String(), fake.calls())
	}
}

func TestNativeYAMLRunRequiresCapabilityAndRetainsManifest(t *testing.T) {
	for _, capable := range []bool{false, true} {
		t.Run(map[bool]string{false: "old-agent", true: "native-agent"}[capable], func(t *testing.T) {
			caps := append([]string{}, workloadCaps...)
			if capable {
				caps = append(caps, protocol.CapabilityKubernetesManifestsRun)
			}
			f := newWorkloadFleet(t, caps...)
			source := `apiVersion: apps/v1
kind: Deployment
metadata: {name: native, namespace: shop}
spec:
  selector: {matchLabels: {app: native}}
  template:
    metadata: {labels: {app: native}}
    spec:
      containers:
      - name: native
        image: nginx:stable
        ports: [{containerPort: 80}]
`
			body, _ := json.Marshal(map[string]string{"yaml": source})
			preview := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/run-yaml/preview", string(body), true)
			if preview.Code != 200 {
				t.Fatalf("preview %d %s", preview.Code, preview.Body.String())
			}
			var parsed struct {
				Workloads []protocol.WorkloadConfiguration `json:"workloads"`
			}
			if json.Unmarshal(preview.Body.Bytes(), &parsed) != nil || len(parsed.Workloads) != 1 {
				t.Fatal("invalid preview")
			}
			body, _ = json.Marshal(map[string]any{"spec": parsed.Workloads[0], "confirm": "native"})
			response := tenantRequest(f.s, f.org, "POST", f.clusterPath+"/workloads", string(body), true)
			if !capable {
				if response.Code != 501 {
					t.Fatalf("old agent %d %s", response.Code, response.Body.String())
				}
				return
			}
			if response.Code != 202 {
				t.Fatalf("run %d %s", response.Code, response.Body.String())
			}
			envelope := readEnvelope(t, f.ctx, f.conn)
			var frame protocol.WorkloadApply
			if envelope.Type != protocol.TypeWorkloadApply || json.Unmarshal(envelope.Payload, &frame) != nil || frame.Validate(time.Now()) != nil {
				t.Fatal("invalid native frame")
			}
			d, err := protocol.RunDeployment(frame.Spec.RunManifest, frame.Target)
			if err != nil || d.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort != 80 {
				t.Fatal("manifest lost before dispatch")
			}
		})
	}
}
