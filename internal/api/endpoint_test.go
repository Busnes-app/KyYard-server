package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/store"
)

const agentImage = "ghcr.io/example/kyyard-agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestEnrollmentRoutes(t *testing.T) {
	t.Setenv("KY_AGENT_IMAGE", agentImage)
	s, st, _ := setupTestServer(t)
	ctx := context.Background()
	ts := st.Tenancy()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}))
	must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}))
	admin := loginAs(t, s, st, "envadmin", "user")
	viewer := loginAs(t, s, st, "viewer", "user")
	platform := loginAs(t, s, st, "platform", "admin")
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}))
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}))
	check := func(c *http.Cookie, method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := tenantRequest(s, c, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), status)
		}
		return w
	}
	tokens := "/api/organizations/a/environments/env-a/enrollment-tokens"
	check(nil, "POST", tokens, `{"runtime":"docker"}`, 401)
	check(platform, "POST", tokens, `{"runtime":"docker"}`, 403)
	check(viewer, "POST", tokens, `{"runtime":"docker"}`, 403)
	check(admin, "POST", tokens, `{"runtime":"docker","organization_id":"b"}`, 400)
	check(admin, "POST", "/api/organizations/a/environments/env-missing/enrollment-tokens", `{"runtime":"docker"}`, 404)
	if w := tenantRequest(s, admin, "POST", tokens, `{"runtime":"docker"}`, false); w.Code != 403 {
		t.Fatal("token minting bypassed CSRF")
	}
	w := check(admin, "POST", tokens, `{"runtime":"docker"}`, 201)
	var minted struct {
		Token, Command, Disclosure, Image, Note string
		ExpiresAt                               string `json:"expires_at"`
	}
	must(json.Unmarshal(w.Body.Bytes(), &minted))
	if !strings.Contains(minted.Command, minted.Token) || !strings.Contains(minted.Command, "docker.sock") || !strings.Contains(minted.Disclosure, "root-equivalent") || minted.ExpiresAt == "" {
		t.Fatalf("enrollment command incomplete: %+v", minted)
	}
	if !strings.Contains(minted.Command, " "+agentImage+" ") || minted.Image != agentImage || strings.Contains(minted.Command, ":latest") {
		t.Fatalf("command does not name exactly the configured digest-pinned image: %s", minted.Command)
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(minted.Token)
	must(err)

	// The agent route needs no session and no CSRF; refusals are uniform.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	enroll := func(token []byte, proof []byte, name string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"token": base64.RawURLEncoding.EncodeToString(token), "public_key": base64.RawURLEncoding.EncodeToString(pub), "proof": base64.RawURLEncoding.EncodeToString(proof), "name": name, "facts": map[string]string{"hostname": "h1"}})
		r := httptest.NewRequest("POST", "/api/agent/v1/enroll", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	proof := ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tokenBytes))
	forged := make([]byte, protocol.TokenSize)
	if w := enroll(forged, ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, forged)), "host"); w.Code != 401 || strings.Contains(w.Body.String(), "token") {
		t.Fatalf("forged token: %d %s", w.Code, w.Body.String())
	}
	if w := enroll(tokenBytes, ed25519.Sign(priv, protocol.Preimage(protocol.ContextAuth, tokenBytes)), "host"); w.Code != 401 {
		t.Fatalf("proof under the wrong context accepted: %d", w.Code)
	}
	w = enroll(tokenBytes, proof, "host")
	if w.Code != 201 {
		t.Fatalf("enroll: %d %s", w.Code, w.Body.String())
	}
	var enrolled struct {
		EndpointID, State, Fingerprint, InstanceFingerprint string `json:"-"`
		ID                                                  string `json:"endpoint_id"`
		St                                                  string `json:"state"`
		FP                                                  string `json:"fingerprint"`
		Inst                                                string `json:"instance_fingerprint"`
	}
	must(json.Unmarshal(w.Body.Bytes(), &enrolled))
	if enrolled.St != "pending" || enrolled.FP != protocol.Fingerprint(pub) || len(enrolled.Inst) != 64 {
		t.Fatalf("enrollment response: %+v", enrolled)
	}
	if w := enroll(tokenBytes, proof, "host"); w.Code != 401 {
		t.Fatalf("replayed token: %d", w.Code)
	}

	// Rate limit on the agent route.
	limited := false
	for i := 0; i < 12; i++ {
		if enroll(forged, proof, "x").Code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("enrollment route is not rate limited")
	}

	endpoints := "/api/organizations/a/endpoints"
	check(nil, "GET", endpoints, "", 401)
	check(platform, "GET", endpoints, "", 403)
	w = check(viewer, "GET", endpoints, "", 200)
	var list []store.Endpoint
	must(json.Unmarshal(w.Body.Bytes(), &list))
	if len(list) != 1 || list[0].ID != enrolled.ID || list[0].State != "pending" {
		t.Fatalf("endpoint list: %+v", list)
	}
	check(viewer, "GET", "/api/organizations/a/environments/env-a/endpoints", "", 200)
	one := endpoints + "/" + enrolled.ID
	check(viewer, "POST", one+"/approve", `{"fingerprint":"`+enrolled.FP+`"}`, 403)
	check(admin, "POST", one+"/approve", `{"fingerprint":"not-hex"}`, 400)
	check(admin, "POST", one+"/approve", `{"fingerprint":"`+strings.Repeat("0", 64)+`"}`, 400)
	if w := tenantRequest(s, admin, "POST", one+"/approve", `{"fingerprint":"`+enrolled.FP+`"}`, false); w.Code != 403 {
		t.Fatal("approval bypassed CSRF")
	}
	check(admin, "POST", one+"/approve", `{"fingerprint":"`+enrolled.FP+`"}`, 204)
	check(admin, "PATCH", one, `{"name":"renamed"}`, 204)
	check(viewer, "POST", one+"/revoke", "", 403)
	check(admin, "DELETE", "/api/organizations/a/environments/env-a", "", 409)
	check(admin, "POST", one+"/revoke", "", 204)
	check(admin, "POST", one+"/revoke", "", 404)
	w = check(admin, "GET", one, "", 200)
	var got store.Endpoint
	must(json.Unmarshal(w.Body.Bytes(), &got))
	if got.State != "revoked" || got.Name != "renamed" {
		t.Fatalf("final state: %+v", got)
	}
	check(admin, "GET", "/api/agent/v1/anything", "", 404)
}

// Without a configured digest-pinned image the token still issues, but no command does.
func TestEnrollmentTokenWithoutImageHasNoCommand(t *testing.T) {
	s, st, _ := setupTestServer(t)
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
	w := tenantRequest(s, admin, "POST", "/api/organizations/a/environments/env-a/enrollment-tokens", `{"runtime":"docker"}`, true)
	if w.Code != 201 {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, has := out["command"]; has || out["token"] == "" || out["note"] == nil || out["disclosure"] == nil {
		t.Fatalf("token without image: %v", out)
	}
}
