package api_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/migration"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const migrationCanary = "migration-secret-canary"

// migrationFleet is a clusterHost whose cluster reports the default StorageClass standard, plus
// a Docker endpoint "docker-1" (no socket: its inventory is accepted directly and inspections
// answer through the plan-time hook) where "shop" is adopted and mapped: db mounts named volume
// data and holds a secret, web publishes 8080.
func migrationFleet(t *testing.T) (clusterHost, string, string) {
	t.Helper()
	h := newClusterHost(t, clusterCapabilities...)
	ctx := context.Background()
	ts := h.st.Tenancy()
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()) + 1, Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.36.0"},
		Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "n1", Ready: true}}, Namespaces: []string{"shop"}, Workloads: []protocol.Workload{}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}, StorageClasses: []protocol.StorageClass{{Name: "standard", Default: true}}}})
	h.sync(t)
	host := enrollAgent(t, h.s, h.st, h.admin, "docker-1")
	h.do(t, "POST", "/api/organizations/a/endpoints/"+host.id+"/approve", `{"fingerprint":"`+host.fp+`"}`, 204)
	if err := ts.SetEndpointCapabilities(ctx, host.id, inspecting); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	snapshot := protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Volumes: []protocol.Volume{{Name: "shop_data"}}}
	bindings := map[string]string{}
	for i, name := range []string{"db", "web"} {
		id, image := strings.Repeat(string("12"[i]), 64), "sha256:"+strings.Repeat(string("ab"[i]), 64)
		snapshot.Containers = append(snapshot.Containers, protocol.Container{ID: id, Name: "shop-" + name, ImageID: image, ComposeProject: "shop", CreatedAt: created, Mounts: []protocol.Mount{}, Networks: []string{"shop_default"}})
		snapshot.Images = append(snapshot.Images, protocol.Image{ID: image, Tags: []string{"ghcr.io/org/" + name + ":1"}})
		bindings[name] = id
	}
	raw, _ := json.Marshal(snapshot)
	if _, err := ts.AcceptInventory(ctx, host.id, uint64(time.Now().Unix()), time.Now(), raw); err != nil {
		t.Fatal(err)
	}
	app := h.importApp(t, "shop", "services: {db: {image: ghcr.io/org/db:1, restart: always, volumes: ['data:/var/lib/db'], environment: {PASSWORD: "+migrationCanary+"}}, web: {image: ghcr.io/org/web:1, restart: always, ports: [{target: 80, published: 8080}]}}\nvolumes: {data: {}}")
	var preview store.AdoptionPreview
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/adoption?endpoint="+host.id+"&project=shop", "", 200)), &preview); err != nil {
		t.Fatal(err)
	}
	adoption, _ := json.Marshal(store.AdoptionRequest{EndpointID: host.id, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	var instance store.ApplicationInstance
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/adoption", string(adoption), 201)), &instance); err != nil {
		t.Fatal(err)
	}
	var mapped store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped); err != nil {
		t.Fatal(err)
	}
	mapping, _ := json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: bindings})
	h.do(t, "PUT", app+"/mapping", string(mapping), 204)
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	return h, app, host.id
}

func readReport(t *testing.T, m store.ApplicationMigration) migration.Report {
	t.Helper()
	var r migration.Report
	if err := json.Unmarshal(m.Report, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The whole flow over the API: analysis from the Docker source against the cluster, storage
// choices validated and re-analyzed, the destination created and linked, then planned and
// applied through PR 21's path with the claim in the frame and the migration on the
// deployment, confirmed and closed.
func TestMigrationOverTheAPI(t *testing.T) {
	h, app, docker := migrationFleet(t)
	start := func(endpoint, namespace string, status int) string {
		t.Helper()
		body, _ := json.Marshal(store.MigrationStart{DestinationEndpointID: endpoint, Namespace: namespace})
		return h.do(t, "POST", app+"/migration", string(body), status)
	}
	if body := start(h.ag.id, "billing", 400); !strings.Contains(body, "namespace_unknown") {
		t.Fatalf("an ungranted namespace: %s", body)
	}
	if body := start(docker, "shop", 409); !strings.Contains(body, "runtime_unsupported") {
		t.Fatalf("a Docker destination: %s", body)
	}
	h.do(t, "GET", app+"/migration", "", 404)
	var m store.ApplicationMigration
	if err := json.Unmarshal([]byte(start(h.ag.id, "shop", 201)), &m); err != nil {
		t.Fatal(err)
	}
	report := readReport(t, m)
	db := report.Services[0]
	if m.Status != store.MigrationAnalyzed || m.Ready || report.Ready || db.Name != "db" || db.Findings[0] != (migration.Finding{Axis: migration.AxisStorage, Class: migration.ChoiceRequired, Code: "volume_named", Detail: "data"}) || db.Class != migration.ChoiceRequired {
		t.Fatalf("analysis %+v %+v", m, report)
	}
	if slices.ContainsFunc(db.Findings, func(f migration.Finding) bool { return f.Code == "inspection_unavailable" }) {
		t.Fatal("the plan-time inspection did not reach the analyzer")
	}
	if body := start(h.ag.id, "shop", 409); !strings.Contains(body, "migration_open") {
		t.Fatalf("a second migration: %s", body)
	}
	if body := h.do(t, "POST", app+"/migration/destination", "", 409); !strings.Contains(body, "migration_not_ready") {
		t.Fatalf("destination before ready: %s", body)
	}
	for choice, code := range map[string]string{
		`{"volumes":{"data":{"storage_class":"gold","size":"1Gi","access_mode":"ReadWriteOnce"}}}`:     "storage_class_unknown",
		`{"volumes":{"data":{"storage_class":"standard","size":"1G","access_mode":"ReadWriteOnce"}}}`:  "size_invalid",
		`{"volumes":{"logs":{"storage_class":"standard","size":"1Gi","access_mode":"ReadWriteOnce"}}}`: "volume_unknown",
	} {
		if body := h.do(t, "PUT", app+"/migration/choices", choice, 400); !strings.Contains(body, code) {
			t.Errorf("%s: %s", code, body)
		}
	}
	if err := json.Unmarshal([]byte(h.do(t, "PUT", app+"/migration/choices", `{"volumes":{"data":{"storage_class":"","size":"1Gi","access_mode":"ReadWriteOnce"}}}`, 200)), &m); err != nil || !m.Ready || !readReport(t, m).Ready {
		t.Fatalf("chosen %+v %v", m, err)
	}
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/migration/analyze", "", 200)), &m); err != nil || !m.Ready || m.Choices.Volumes["data"].Size != "1Gi" {
		t.Fatalf("re-analyzed %+v %v", m, err)
	}
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/migration/destination", "", 201)), &m); err != nil || m.Status != store.MigrationDestinationCreated || m.DestinationApplicationName != "shop on cluster-1" {
		t.Fatalf("destination %+v %v", m, err)
	}
	dest := h.base + "/" + m.DestinationApplicationID
	var linked store.ApplicationMigration
	if err := json.Unmarshal([]byte(h.do(t, "GET", dest+"/migration", "", 200)), &linked); err != nil || linked.Role != "destination" || linked.ApplicationID != strings.TrimPrefix(app, h.base+"/") {
		t.Fatalf("destination link %+v %v", linked, err)
	}
	h.do(t, "PUT", dest+"/migration/choices", `{"volumes":{}}`, 404)

	var mapped store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", dest+"/mapping", "", 200)), &mapped); err != nil || mapped.Namespace != "shop" || mapped.Preview.Project != "shop-on-cluster-1" {
		t.Fatalf("destination mapping %+v %v", mapped, err)
	}
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop-on-cluster-1"})
	var planned store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", dest+"/deployments", string(planBody), 201)), &planned); err != nil {
		t.Fatal(err)
	}
	claim := protocol.KubernetesClaim{Name: "shop-on-cluster-1-data", Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce}
	if !reflect.DeepEqual(planned.Plan.Claims, []protocol.KubernetesClaim{claim}) || planned.Plan.Services[0].ClaimMounts[0] != (protocol.KubernetesMount{Claim: claim.Name, MountPath: "/var/lib/db"}) {
		t.Fatalf("plan %+v", planned.Plan)
	}
	var applying store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", dest+"/deployments/"+planned.ID+"/apply", `{"confirm":"shop-on-cluster-1"}`, 202)), &applying); err != nil || applying.MigrationID != m.ID {
		t.Fatalf("apply %+v %v", applying, err)
	}
	frame := readEnvelope(t, h.ctx, h.sock.conn)
	var sent protocol.DeploymentRequest
	if frame.Type != protocol.TypeDeploymentApply || json.Unmarshal(frame.Payload, &sent) != nil {
		t.Fatalf("frame %s", frame.Type)
	}
	if !reflect.DeepEqual(sent.Kubernetes.Claims, []protocol.KubernetesClaim{claim}) || sent.Services[0].Volumes[0].Claim != claim.Name || sent.Services[0].Env["PASSWORD"] != migrationCanary || sent.ValidateFor(protocol.RuntimeKubernetes, time.Now()) != nil {
		t.Fatalf("frame %+v", sent)
	}
	var ids []protocol.DeploymentIdentity
	for _, ps := range planned.Plan.Services {
		ids = append(ids, protocol.DeploymentIdentity{Service: ps.Name, Kind: protocol.KindDeployment, Namespace: "shop", Name: ps.Object.Name, UID: "0f1e2d3c-4b5a-4968-8776-655443322110", Generation: 1, ImageDigest: ps.PullDigest})
	}
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: planned.ID, RequestID: planned.CorrelationID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: ids})
	h.sync(t)
	var settled store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", dest+"/deployments/"+planned.ID, "", 200)), &settled); err != nil || settled.State != protocol.OutcomeSucceeded || settled.MigrationID != m.ID {
		t.Fatalf("settled %+v %v", settled, err)
	}
	if strings.Contains(h.do(t, "GET", app+"/migration", "", 200), migrationCanary) || strings.Contains(h.do(t, "GET", dest+"/deployments", "", 200), migrationCanary) {
		t.Fatal("a secret value reached a response")
	}

	if body := h.do(t, "POST", app+"/migration/cutover", `{"note":"early"}`, 409); !strings.Contains(body, "migration_state") {
		t.Fatalf("cutover before validation: %s", body)
	}
	h.do(t, "POST", app+"/migration/validated", `{"note":""}`, 400)
	h.do(t, "POST", app+"/migration/validated", `{"note":"orders page answers on the cluster"}`, 200)
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/migration/cutover", `{"note":"DNS points at the ingress"}`, 200)), &m); err != nil || m.Status != store.MigrationCutoverConfirmed || m.CutoverNote != "DNS points at the ingress" {
		t.Fatalf("cutover %+v %v", m, err)
	}
	h.do(t, "GET", app+"/migration", "", 404)
	// Abandoning a migration already closed by cutover is migration_state (store's
	// latestMigrationOf/changeMigration contract, tested in Task 2-6), not 404.
	if body := h.do(t, "DELETE", app+"/migration", "", 409); !strings.Contains(body, "migration_state") {
		t.Fatalf("abandon after cutover: %s", body)
	}
}

// Abandoning keeps a created destination and says so.
func TestMigrationAbandonKeepsTheDestination(t *testing.T) {
	h, app, _ := migrationFleet(t)
	body, _ := json.Marshal(store.MigrationStart{DestinationEndpointID: h.ag.id, Namespace: "shop"})
	h.do(t, "POST", app+"/migration", string(body), 201)
	h.do(t, "PUT", app+"/migration/choices", `{"volumes":{"data":{"storage_class":"standard","size":"1Gi","access_mode":"ReadWriteOnce"}}}`, 200)
	var m store.ApplicationMigration
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/migration/destination", "", 201)), &m); err != nil {
		t.Fatal(err)
	}
	var abandoned struct {
		Status                   string `json:"status"`
		DestinationApplicationID string `json:"destination_application_id"`
		DestinationKept          bool   `json:"destination_kept"`
	}
	if err := json.Unmarshal([]byte(h.do(t, "DELETE", app+"/migration", "", 200)), &abandoned); err != nil || abandoned.Status != store.MigrationAbandoned || !abandoned.DestinationKept || abandoned.DestinationApplicationID != m.DestinationApplicationID {
		t.Fatalf("abandoned %+v %v", abandoned, err)
	}
	h.do(t, "GET", h.base+"/"+m.DestinationApplicationID+"/mapping", "", 200)
}

// Only an organization administrator migrates; other members read. A source that is not
// adopted on Docker, and a destination that is not a cluster, are runtime_unsupported.
func TestMigrationRolesAndRuntimes(t *testing.T) {
	h, app, _ := migrationFleet(t)
	body, _ := json.Marshal(store.MigrationStart{DestinationEndpointID: h.ag.id, Namespace: "shop"})
	envAdmin := loginAs(t, h.s, h.st, "envadmin", "user")
	if err := h.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	h.do(t, "POST", app+"/migration", string(body), 201)
	for _, route := range []struct{ method, path, body string }{
		{"POST", app + "/migration", string(body)},
		{"PUT", app + "/migration/choices", `{"volumes":{}}`},
		{"POST", app + "/migration/analyze", ""},
		{"POST", app + "/migration/destination", ""},
		{"POST", app + "/migration/validated", `{"note":"x"}`},
		{"POST", app + "/migration/cutover", `{"note":"x"}`},
		{"DELETE", app + "/migration", ""},
	} {
		if w := tenantRequest(h.s, envAdmin, route.method, route.path, route.body, true); w.Code != 403 || !strings.Contains(w.Body.String(), "tenant_access_denied") {
			t.Errorf("an environment administrator %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	if w := tenantRequest(h.s, envAdmin, "GET", app+"/migration", "", false); w.Code != 200 {
		t.Fatalf("an environment administrator reads: %d", w.Code)
	}
	h.do(t, "DELETE", app+"/migration", "", 200)
	cluster := h.importApp(t, "cart", "services: {web: {image: ghcr.io/org/web:1}}")
	h.do(t, "PUT", cluster+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	if body := h.do(t, "POST", cluster+"/migration", string(body), 409); !strings.Contains(body, "runtime_unsupported") {
		t.Fatalf("a cluster source: %s", body)
	}
	draft := h.importApp(t, "draft", "services: {web: {image: ghcr.io/org/web:1}}")
	if body := h.do(t, "POST", draft+"/migration", string(body), 409); !strings.Contains(body, "mapping_required") {
		t.Fatalf("an unadopted source: %s", body)
	}
}
