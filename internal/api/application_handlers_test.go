package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestApplicationImportRoutes(t *testing.T) {
	s, st, _ := setupTestServer(t)
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
	snapshot, _ := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{{ID: strings.Repeat("a", 64), Name: "shop-web", ImageID: "sha256:" + strings.Repeat("b", 64), ComposeProject: "shop", CreatedAt: time.Now().UTC()}}})
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
	comparison := base + "/" + app.ID + "/comparison"
	request(nil, "GET", comparison, "", 401)
	request(admin, "GET", comparison, "", 200)
	for _, role := range []store.TenantRole{store.RoleReadOnly, store.RoleDeveloper, store.RoleOperator} {
		must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_importer", Role: role, Status: "active"}))
		request(admin, "GET", comparison, "", 200)
	}
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_importer", Role: store.RoleOrganizationAdmin, Status: "active"}))
	request(admin, "GET", "/api/organizations/b/environments/env-b/applications/"+app.ID+"/comparison", "", 403)
	request(admin, "GET", base+"/instances", "", 200)
	request(admin, "GET", "/api/organizations/a/endpoints/"+ep.ID+"/applications", "", 200)
	request(admin, "DELETE", base+"/"+app.ID, `{"expected_revision":1}`, 409)
	releaseBody, _ := json.Marshal(map[string]string{"instance_id": instance.ID, "confirm": "shop"})
	request(admin, "DELETE", adoption, string(releaseBody), 204)
	request(admin, "DELETE", adoption, string(releaseBody), 409)
	request(admin, "GET", comparison, "", 404)
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
