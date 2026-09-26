package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// An administrator records the namespaces a cluster's agent may write in and gets the RBAC-only
// manifest granting exactly those; the call is audited with the list. A Docker endpoint has no
// manifest, a bad list is refused, and a member who may not enroll learns nothing.
func TestEndpointManifestRoute(t *testing.T) {
	f := newRuntimeFleet(t)
	route := func(id string) string { return "/api/organizations/a/endpoints/" + id + "/manifest" }
	w := tenantRequest(f.s, f.admin, "POST", route(f.cluster.id), `{"namespaces":["shop","billing"]}`, true)
	var out struct {
		Manifest, Command, Note string
		File                    string   `json:"manifest_file"`
		Namespaces              []string `json:"namespaces"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("manifest: %d %s", w.Code, w.Body.String())
	}
	if out.File != "kyyard-agent-cluster-1.yaml" || out.Command != "kubectl apply -f kyyard-agent-cluster-1.yaml" || !slices.Equal(out.Namespaces, []string{"billing", "shop"}) {
		t.Fatalf("response %+v", out)
	}
	if strings.Count(out.Manifest, "name: kyyard-agent-deploy\n  namespace: \"billing\"") != 2 || strings.Contains(out.Manifest, "kind: Deployment") || strings.Contains(out.Manifest, "kyyard-agent-enrollment") || !strings.Contains(out.Note, "delete role,rolebinding kyyard-agent-deploy") {
		t.Fatalf("manifest:\n%s", out.Manifest)
	}
	var stored []string
	for _, ns := range f.endpoint(t, f.cluster.id)["deploy_namespaces"].([]any) {
		stored = append(stored, ns.(string))
	}
	if !slices.Equal(stored, []string{"billing", "shop"}) {
		t.Fatalf("stored %v", stored)
	}
	rows, _, err := f.st.Audit().ListAuditRecords(context.Background(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rows, func(r *store.AuditRecord) bool {
		return r.Action == "endpoint.enroll" && r.Resource == f.cluster.id+"/manifest" && r.Details == "namespaces=billing,shop" && r.Result == "success"
	}) {
		t.Fatal("no audit row names the manifest's namespaces")
	}
	for _, tc := range []struct {
		cookie *http.Cookie
		id     string
		body   string
		status int
		code   string
	}{
		{f.admin, f.host.id, `{"namespaces":["shop"]}`, 409, "runtime_unsupported"},
		{f.admin, f.cluster.id, `{"namespaces":["Shop"]}`, 400, ""},
		{f.admin, f.cluster.id, `{"namespaces":["kyyard-agent"]}`, 400, ""},
		{f.admin, f.cluster.id, `{"namespaces":["shop"],"extra":1}`, 400, ""},
	} {
		w := tenantRequest(f.s, tc.cookie, "POST", route(tc.id), tc.body, true)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
			t.Errorf("%s %s: %d %s", tc.id, tc.body, w.Code, w.Body.String())
		}
	}
	viewer := loginAs(t, f.s, f.st, "viewer", "user")
	if err := f.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if w := tenantRequest(f.s, viewer, "POST", route(f.cluster.id), `{"namespaces":["shop"]}`, true); w.Code != 403 || strings.Contains(w.Body.String(), "manifest") {
		t.Fatalf("viewer: %d %s", w.Code, w.Body.String())
	}
}

// Namespaces named at enrollment go into the first manifest and onto the endpoint; a Docker
// token takes none.
func TestKubernetesEnrollmentWithNamespaces(t *testing.T) {
	t.Setenv("KY_AGENT_IMAGE", agentImage)
	s, st, cfg := setupTestServer(t)
	cfg.Server.AppURL = "https://yard.example"
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}); err != nil {
		t.Fatal(err)
	}
	admin := loginAs(t, s, st, "envadmin", "user")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	tokens := "/api/organizations/a/environments/env-a/enrollment-tokens"
	for _, body := range []string{`{"runtime":"docker","namespaces":["shop"]}`, `{"runtime":"kubernetes","name":"prod","namespaces":["kube-system"]}`, `{"runtime":"kubernetes","name":"prod","namespaces":["shop","shop"]}`} {
		if w := tenantRequest(s, admin, "POST", tokens, body, true); w.Code != 400 {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	w := tenantRequest(s, admin, "POST", tokens, `{"runtime":"kubernetes","name":"prod","namespaces":["shop"]}`, true)
	var out struct {
		Token, Manifest, Disclosure string
		Namespaces                  []string `json:"namespaces"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(out.Manifest, "name: kyyard-agent-deploy\n  namespace: \"shop\"") || strings.Count(out.Manifest, out.Token) != 1 || !strings.Contains(out.Disclosure, "In each namespace you listed") || !slices.Equal(out.Namespaces, []string{"shop"}) {
		t.Fatalf("mint %+v", out)
	}
	ag := redeemToken(t, s, out.Token, "prod")
	e, err := ts.ReadEndpointRaw(ctx, ag.id)
	if err != nil || !slices.Equal(e.DeployNamespaces, []string{"shop"}) {
		t.Fatalf("enrolled %+v %v", e, err)
	}
}
