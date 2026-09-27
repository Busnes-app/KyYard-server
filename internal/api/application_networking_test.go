package api_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestStaticServiceIPOverAPIAndAgent(t *testing.T) {
	h := newClusterHost(t, append(slices.Clone(clusterCapabilities), protocol.CapabilityKubernetesServiceIPs)...)
	app := h.importApp(t, "shop", "services: {web: {image: ghcr.io/org/web:1, ports: [{target: 80, published: 8080}], environment: {TOKEN: network-secret-canary}}}")
	h.do(t, "PUT", app+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	body := `{"expected_revision":1,"service_ips":{"web":"10.96.0.40"}}`
	if w := tenantRequest(h.s, h.admin, "PUT", app+"/networking", body, false); w.Code != 403 {
		t.Fatalf("missing CSRF: %d", w.Code)
	}
	h.do(t, "PUT", app+"/networking", `{"expected_revision":1,"service_ips":{"web":"None"}}`, 400)
	h.do(t, "PUT", app+"/networking", body, 201)
	h.do(t, "PUT", app+"/networking", body, 409)
	var revision store.ApplicationRevision
	raw := h.do(t, "GET", app+"/revisions/2", "", 200)
	if err := json.Unmarshal([]byte(raw), &revision); err != nil || revision.Spec.Kubernetes.ServiceIPs["web"] != "10.96.0.40" || strings.Contains(raw, "network-secret-canary") {
		t.Fatalf("revision: %s %v", raw, err)
	}
	var m store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &m); err != nil {
		t.Fatal(err)
	}
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: m.InstanceID, MappingVersion: m.Version, Revision: 2, Confirm: "shop"})
	var d store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/deployments", string(planBody), 201)), &d); err != nil || d.Plan.Services[0].ClusterIP != "10.96.0.40" {
		t.Fatalf("plan: %+v %v", d.Plan, err)
	}
	h.do(t, "POST", app+"/deployments/"+d.ID+"/apply", `{"confirm":"shop"}`, 202)
	frame := readEnvelope(t, h.ctx, h.sock.conn)
	var sent protocol.DeploymentRequest
	if frame.Type != protocol.TypeDeploymentApply || json.Unmarshal(frame.Payload, &sent) != nil || sent.Services[0].ClusterIP != "10.96.0.40" || sent.Services[0].Env["TOKEN"] != "network-secret-canary" {
		t.Fatal("static IP or preserved secret missing from frame")
	}
	if err := h.st.Tenancy().SetMembership(h.ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_deployer", Role: store.RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	h.do(t, "PUT", app+"/networking", `{"expected_revision":2,"service_ips":{}}`, 403)
}
