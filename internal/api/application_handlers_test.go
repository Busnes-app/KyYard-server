package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// expireDeployments backdates every live deployment plan directly in storage. No production
// hook exists to expire a plan early; this reaches the DB the same way tenancy_test.go does.
func expireDeployments(t *testing.T, cfg *config.Config) {
	t.Helper()
	driver := "pgx"
	q := "UPDATE deployments SET expires_at=$1"
	if cfg.Database.Driver == "sqlite" {
		driver = "sqlite"
		q = "UPDATE deployments SET expires_at=?"
	}
	db, err := sql.Open(driver, cfg.Database.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestApplicationImportRoutes(t *testing.T) {
	s, st, cfg := setupTestServer(t)
	api.SetPlanInspectorForTest(s, verifiedInspector)
	ctx := context.Background()
	ts := st.Tenancy()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"a", "b"} {
		must(ts.CreateOrganization(ctx, &store.Organization{ID: org, Name: org}))
		must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-" + org, OrganizationID: org, Name: "prod"}))
	}
	admin := loginAs(t, s, st, "importer", "user")
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_importer", Role: store.RoleOrganizationAdmin, Status: "active"}))
	base := "/api/organizations/a/environments/env-a/applications"
	body, _ := json.Marshal(map[string]string{"name": "shop", "compose": "services: {web: {image: nginx:1, environment: {TOKEN: secret-canary}}}"})
	request := func(cookie *http.Cookie, method, path, body string, status int) string {
		t.Helper()
		w := tenantRequest(s, cookie, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret-canary") {
			t.Fatal("response leaked secret")
		}
		return w.Body.String()
	}
	request(nil, "POST", base, string(body), 401)
	if w := tenantRequest(s, admin, "POST", base, string(body), false); w.Code != 403 {
		t.Fatal("missing csrf accepted")
	}
	saved := request(admin, "POST", base, string(body), 201)
	var app store.Application
	must(json.Unmarshal([]byte(saved), &app))
	a := store.TenantAccess{ActorID: "usr_importer", OrganizationID: "a", EnvironmentID: "env-a"}
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	must(err)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ep, err := ts.Enroll(ctx, store.EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "host"})
	must(err)
	must(ts.ApproveEndpoint(ctx, a, ep.ID, ep.Fingerprint))
	must(ts.SetEndpointCapabilities(ctx, ep.ID, []string{protocol.CapabilityDeploymentApply, protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict}))
	created := time.Now().UTC()
	snapshot, _ := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{{ID: strings.Repeat("a", 64), Name: "shop-web", ImageID: "sha256:" + strings.Repeat("b", 64), ComposeProject: "shop", CreatedAt: created, Mounts: []protocol.Mount{}}}})
	_, err = ts.AcceptInventory(ctx, ep.ID, uint64(time.Now().Unix()), time.Now(), snapshot)
	must(err)
	adoption := base + "/" + app.ID + "/adoption"
	var preview store.AdoptionPreview
	must(json.Unmarshal([]byte(request(admin, "GET", adoption+"?endpoint="+ep.ID+"&project=shop", "", 200)), &preview))
	adoptionBody, _ := json.Marshal(store.AdoptionRequest{EndpointID: ep.ID, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	if w := tenantRequest(s, admin, "POST", adoption, string(adoptionBody), false); w.Code != 403 {
		t.Fatal("adoption accepted without CSRF")
	}
	request(nil, "POST", adoption, string(adoptionBody), 401)
	var instance store.ApplicationInstance
	must(json.Unmarshal([]byte(request(admin, "POST", adoption, string(adoptionBody), 201)), &instance))
	mapping := base + "/" + app.ID + "/mapping"
	var mapped store.ApplicationMapping
	must(json.Unmarshal([]byte(request(admin, "GET", mapping, "", 200)), &mapped))
	mappingBody, _ := json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: map[string]string{"web": strings.Repeat("a", 64)}})
	request(nil, "GET", mapping, "", 401)
	if w := tenantRequest(s, admin, "PUT", mapping, string(mappingBody), false); w.Code != 403 {
		t.Fatal("mapping without CSRF")
	}
	request(admin, "PUT", mapping, string(mappingBody), 204)
	request(admin, "PUT", mapping, string(mappingBody), 409)
	preflight := base + "/" + app.ID + "/preflight"
	request(nil, "GET", preflight, "", 401)
	if got := request(admin, "GET", preflight, "", 200); !strings.Contains(got, `"executable":false`) {
		t.Fatalf("preflight without the image: %s", got)
	}
	request(admin, "GET", "/api/organizations/b/environments/env-b/applications/"+app.ID+"/preflight", "", 403)
	releaseBody, _ := json.Marshal(map[string]string{"instance_id": instance.ID, "confirm": "shop"})
	deployments := base + "/" + app.ID + "/deployments"
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: instance.ID, MappingVersion: 1, Revision: 1, Confirm: "shop"})
	request(nil, "POST", deployments, string(planBody), 401)
	if w := tenantRequest(s, admin, "POST", deployments, string(planBody), false); w.Code != 403 {
		t.Fatal("plan without CSRF")
	}
	// No image in inventory yet: blocked, and the body names the blocker only.
	blocked := request(admin, "POST", deployments, string(planBody), 409)
	if !strings.Contains(blocked, `"preflight_blocked"`) || !strings.Contains(blocked, "image_not_reported") {
		t.Fatalf("blocked body: %s", blocked)
	}
	withImage, _ := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Images: []protocol.Image{{ID: "sha256:" + strings.Repeat("c", 64), Tags: []string{"nginx:1"}}}, Containers: []protocol.Container{{ID: strings.Repeat("a", 64), Name: "shop-web", ImageID: "sha256:" + strings.Repeat("b", 64), ComposeProject: "shop", CreatedAt: created, Mounts: []protocol.Mount{}}}})
	_, err = ts.AcceptInventory(ctx, ep.ID, uint64(time.Now().Unix())+1, time.Now(), withImage)
	must(err)
	if got := request(admin, "GET", preflight, "", 200); !strings.Contains(got, `"executable":true`) || !strings.Contains(got, `"blockers":[]`) {
		t.Fatalf("ready preflight: %s", got)
	}
	var planned store.Deployment
	must(json.Unmarshal([]byte(request(admin, "POST", deployments, string(planBody), 201)), &planned))
	request(admin, "GET", deployments, "", 200)
	request(admin, "GET", deployments+"/"+planned.ID, "", 200)
	request(admin, "GET", deployments+"/not-a-uuid", "", 404)
	request(admin, "GET", "/api/organizations/b/environments/env-b/applications/"+app.ID+"/deployments", "", 403)
	request(admin, "DELETE", adoption, string(releaseBody), 409) // live plan
	comparison := base + "/" + app.ID + "/comparison"
	request(nil, "GET", comparison, "", 401)
	request(admin, "GET", comparison, "", 200)
	for _, role := range []store.TenantRole{store.RoleReadOnly, store.RoleDeveloper, store.RoleOperator} {
		must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_importer", Role: role, Status: "active"}))
		request(admin, "GET", comparison, "", 200)
		request(admin, "GET", mapping, "", 200)
		request(admin, "GET", preflight, "", 200)
		request(admin, "GET", deployments, "", 200)
		request(admin, "PUT", mapping, string(mappingBody), 403)
		if role == store.RoleDeveloper {
			request(admin, "POST", deployments, string(planBody), 201)
		} else {
			request(admin, "POST", deployments, string(planBody), 403)
		}
	}
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_importer", Role: store.RoleOrganizationAdmin, Status: "active"}))
	request(admin, "GET", "/api/organizations/b/environments/env-b/applications/"+app.ID+"/comparison", "", 403)
	request(admin, "GET", base+"/instances", "", 200)
	request(admin, "GET", "/api/organizations/a/endpoints/"+ep.ID+"/applications", "", 200)
	request(admin, "DELETE", base+"/"+app.ID, `{"expected_revision":1}`, 409)
	expireDeployments(t, cfg)
	request(admin, "DELETE", adoption, string(releaseBody), 204)
	request(admin, "DELETE", adoption, string(releaseBody), 409)
	request(admin, "GET", comparison, "", 404)
	request(admin, "GET", preflight, "", 404)
	request(admin, "GET", base, "", 200)
	request(admin, "GET", base+"/"+app.ID+"/revisions/1", "", 200)
	request(admin, "POST", base, string(body), 409)
	request(admin, "POST", base, `{"name":"bad","compose":"services: {web: {build: secret-canary}}"}`, 400)
	request(admin, "POST", base, `{"name":"bad","compose":"services: {}","actor_id":"forged"}`, 400)
	request(admin, "GET", "/api/organizations/a/environments/env-b/applications", "", 404)
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "usr_importer", Role: store.RoleOrganizationAdmin, Status: "active"}))
	request(admin, "GET", "/api/organizations/b/environments/env-b/applications/"+app.ID+"/revisions/1", "", 404)
	for _, role := range []store.TenantRole{store.RoleReadOnly, store.RoleDeveloper, store.RoleOperator} {
		must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_importer", Role: role, Status: "active"}))
		request(admin, "POST", base, string(body), 403)
		request(admin, "POST", adoption, string(adoptionBody), 403)
		request(admin, "DELETE", adoption, string(releaseBody), 403)
		request(admin, "DELETE", base+"/"+app.ID, `{"expected_revision":1}`, 403)
		request(admin, "GET", base+"/"+app.ID+"/revisions/1", "", 200)
	}
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_importer", Role: store.RoleOrganizationAdmin, Status: "active"}))
	request(admin, "DELETE", base+"/"+app.ID, `{"expected_revision":2}`, 409)
	request(admin, "DELETE", base+"/"+app.ID, `{"expected_revision":1}`, 204)
	request(admin, "GET", base+"/"+app.ID+"/revisions/1", "", 404)
	audits, err := ts.ReadAudit(ctx, store.TenantAccess{ActorID: "usr_importer", OrganizationID: "a"}, 0, 200)
	must(err)
	if strings.Contains(fmt.Sprint(audits), "secret-canary") {
		t.Fatal("audit leaked source")
	}
}

func TestApplicationRevisionReplacementRoutes(t *testing.T) {
	s, st, _ := setupTestServer(t)
	api.SetPlanInspectorForTest(s, verifiedInspector)
	ctx := context.Background()
	ts := st.Tenancy()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ts.CreateOrganization(ctx, &store.Organization{ID: "edit", Name: "edit"}))
	must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-edit", OrganizationID: "edit", Name: "prod"}))
	cookie := loginAs(t, s, st, "editor", "user")
	setRole := func(role store.TenantRole) {
		must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "edit", UserID: "usr_editor", Role: role, Status: "active"}))
	}
	setRole(store.RoleOrganizationAdmin)
	a := store.TenantAccess{ActorID: "usr_editor", OrganizationID: "edit", EnvironmentID: "env-edit"}
	app, err := ts.CreateApplication(ctx, a, "shop", store.ApplicationSpec{Kind: "compose.v1", Services: []store.ApplicationService{{Name: "web", Image: "nginx:1"}}})
	must(err)
	base := "/api/organizations/edit/environments/env-edit/applications/" + app.ID
	body := `{"expected_revision":1,"compose":"services: {web: {image: nginx:2, environment: {TOKEN: revision-secret-canary}}}"}`
	request := func(c *http.Cookie, method, path, body string, csrf bool, status int) string {
		t.Helper()
		w := tenantRequest(s, c, method, path, body, csrf)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "revision-secret-canary") {
			t.Fatal("secret response")
		}
		return w.Body.String()
	}
	request(nil, "POST", base+"/revisions", body, true, 401)
	request(cookie, "POST", base+"/revisions", body, false, 403)
	for _, role := range []store.TenantRole{store.RoleReadOnly, store.RoleOperator} {
		setRole(role)
		request(cookie, "POST", base+"/revisions", body, true, 403)
	}
	setRole(store.RoleDeveloper)
	request(cookie, "POST", strings.Replace(base, "env-edit", "foreign", 1)+"/revisions", body, true, 404)
	request(cookie, "POST", base+"/revisions", `{"expected_revision":1,"compose":"services: {web: {build: revision-secret-canary}}"}`, true, 400)
	if got := request(cookie, "POST", base+"/revisions", body, true, 201); !strings.Contains(got, `"revision":2`) {
		t.Fatal(got)
	}
	request(cookie, "POST", base+"/revisions", body, true, 409)
	for _, number := range []string{"1", "2"} {
		request(cookie, "GET", base+"/revisions/"+number, "", false, 200)
	}
	request(cookie, "POST", base+"/revisions", `{"expected_revision":2,"compose":"services: {web: {image: nginx:3}}","secret_ref":"forged"}`, true, 400)
}
