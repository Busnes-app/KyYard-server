package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// A Kubernetes token comes with the manifest and its kubectl command, the token inside the
// manifest once, and the RBAC disclosure; a cluster must be named, a Docker host must not be.
func TestKubernetesEnrollmentReturnsTheManifest(t *testing.T) {
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
	viewer := loginAs(t, s, st, "viewer", "user")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	tokens := "/api/organizations/a/environments/env-a/enrollment-tokens"
	mint := func(c *http.Cookie, body string, status int) string {
		t.Helper()
		w := tenantRequest(s, c, "POST", tokens, body, true)
		if w.Code != status {
			t.Fatalf("%s: %d %s, want %d", body, w.Code, w.Body.String(), status)
		}
		return w.Body.String()
	}
	mint(admin, `{"runtime":"kubernetes"}`, 400)
	mint(admin, `{"runtime":"kubernetes","name":"bad\nname"}`, 400)
	mint(admin, `{"runtime":"docker","name":"host"}`, 400)
	mint(viewer, `{"runtime":"kubernetes","name":"prod"}`, 403)

	var out struct {
		Token, Command, Disclosure, Image, Note, Manifest string
		File                                              string `json:"manifest_file"`
	}
	if err := json.Unmarshal([]byte(mint(admin, `{"runtime":"kubernetes","name":"prod \"east\""}`, 201)), &out); err != nil {
		t.Fatal(err)
	}
	if out.File != "kyyard-agent-prod-east.yaml" || out.Command != "kubectl apply -f kyyard-agent-prod-east.yaml" || out.Image != agentImage {
		t.Fatalf("command %q file %q image %q", out.Command, out.File, out.Image)
	}
	if strings.Count(out.Manifest, out.Token) != 1 || !strings.Contains(out.Manifest, "image: \""+agentImage+"\"") || !strings.Contains(out.Manifest, `"--name", "prod \"east\""`) {
		t.Fatalf("manifest:\n%s", out.Manifest)
	}
	if !strings.Contains(out.Disclosure, "cannot read Secrets") || !strings.Contains(out.Disclosure, "cluster-admin") || !strings.Contains(out.Note, "delete secret kyyard-agent-enrollment") {
		t.Fatalf("disclosure %q note %q", out.Disclosure, out.Note)
	}

	// No manifest without a pinned image or an HTTPS address; a member who may not enroll
	// learns neither.
	cfg.Server.AgentImage, cfg.Server.DockerSocket = "", ""
	if body := mint(admin, `{"runtime":"kubernetes","name":"prod"}`, 409); !strings.Contains(body, "agent_image_unpinned") {
		t.Fatalf("unpinned: %s", body)
	}
	mint(viewer, `{"runtime":"kubernetes","name":"prod"}`, 403)
	cfg.Server.AgentImage, cfg.Server.AppURL = agentImage, "http://yard.example"
	if body := mint(admin, `{"runtime":"kubernetes","name":"prod"}`, 409); !strings.Contains(body, "https_required") {
		t.Fatalf("http: %s", body)
	}
}
