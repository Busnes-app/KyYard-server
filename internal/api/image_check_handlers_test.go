package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/google/uuid"
)

// fakeDigests answers every Head with digest. With gate set, it reports entry on entered and
// holds until gate closes or ctx ends.
type fakeDigests struct {
	digest  string
	gate    chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	secrets []string
}

func (f *fakeDigests) Head(ctx context.Context, _ registry.Reference, cred *registry.Credential, _ bool) (string, error) {
	f.mu.Lock()
	if cred != nil {
		f.secrets = append(f.secrets, cred.Secret)
	}
	f.mu.Unlock()
	if f.gate != nil {
		f.entered <- struct{}{}
		select {
		case <-f.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return f.digest, nil
}

func TestImageCheckRoutes(t *testing.T) {
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
	user := loginAs(t, s, st, "checker", "user")
	outsider := loginAs(t, s, st, "outsider", "user")
	watcher := loginAs(t, s, st, "watcher", "user")
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_watcher", Role: store.RoleOperator, Status: "active"}))
	setRole := func(role store.TenantRole) {
		t.Helper()
		must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_checker", Role: role, Status: "active"}))
	}
	setRole(store.RoleOrganizationAdmin)
	const secret = "update-check-registry-canary"
	send := func(cookie *http.Cookie, method, path, body string, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		w := tenantRequest(s, cookie, method, path, body, csrf)
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("%s %s leaked the credential: %s", method, path, w.Body.String())
		}
		return w
	}
	request := func(cookie *http.Cookie, method, path, body string, status int) string {
		t.Helper()
		w := send(cookie, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), status)
		}
		return w.Body.String()
	}
	code := func(body, want string) {
		t.Helper()
		var e struct{ Code string }
		if json.Unmarshal([]byte(body), &e) != nil || e.Code != want {
			t.Fatalf("want code %s: %s", want, body)
		}
	}

	request(user, "PUT", "/api/organizations/a/registries", `{"host":"ghcr.io","name":"GitHub","username":"bot","credential":"`+secret+`","allow_private":false}`, 200)
	base := "/api/organizations/a/environments/env-a/applications"
	body, _ := json.Marshal(map[string]string{"name": "shop", "compose": "services: {web: {image: ghcr.io/org/web:1.2}}"})
	var app store.Application
	must(json.Unmarshal([]byte(request(user, "POST", base, string(body), 201)), &app))
	updates := base + "/" + app.ID + "/updates"
	check := updates + "/check"

	// Nothing adopted: the read is empty, not an error.
	request(nil, "GET", updates, "", 401)
	request(nil, "POST", check, "", 401)
	var got store.UpdateCheck
	must(json.Unmarshal([]byte(request(user, "GET", updates, "", 200)), &got))
	if got.InstanceID != "" || got.Services == nil || len(got.Services) != 0 {
		t.Fatalf("empty read: %+v", got)
	}

	a := store.TenantAccess{ActorID: "usr_checker", OrganizationID: "a", EnvironmentID: "env-a"}
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	must(err)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ep, err := ts.Enroll(ctx, store.EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "host"})
	must(err)
	must(ts.ApproveEndpoint(ctx, a, ep.ID, ep.Fingerprint))
	containerID, imageID := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	local, remote := "sha256:"+strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64)
	snapshot, _ := json.Marshal(protocol.Snapshot{
		Engine:     protocol.Engine{Version: "1"},
		Images:     []protocol.Image{{ID: imageID, Tags: []string{"ghcr.io/org/web:1.2"}, Digests: []string{"ghcr.io/org/web@" + local}}},
		Containers: []protocol.Container{{ID: containerID, Name: "shop-web", ImageID: imageID, ComposeProject: "shop", CreatedAt: time.Now().UTC()}},
	})
	_, err = ts.AcceptInventory(ctx, ep.ID, uint64(time.Now().Unix()), time.Now(), snapshot)
	must(err)
	adoption := base + "/" + app.ID + "/adoption"
	var preview store.AdoptionPreview
	must(json.Unmarshal([]byte(request(user, "GET", adoption+"?endpoint="+ep.ID+"&project=shop", "", 200)), &preview))
	adoptionBody, _ := json.Marshal(store.AdoptionRequest{EndpointID: ep.ID, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	var instance store.ApplicationInstance
	must(json.Unmarshal([]byte(request(user, "POST", adoption, string(adoptionBody), 201)), &instance))

	fake := &fakeDigests{digest: remote}
	api.SetDigestResolverForTest(s, fake)
	// Adopted but not mapped.
	code(request(user, "POST", check, "", 409), "mapping_required")

	mapping := base + "/" + app.ID + "/mapping"
	var mapped store.ApplicationMapping
	must(json.Unmarshal([]byte(request(user, "GET", mapping, "", 200)), &mapped))
	mappingBody, _ := json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: map[string]string{"web": containerID}})
	request(user, "PUT", mapping, string(mappingBody), 204)

	setRole(store.RoleReadOnly)
	must(json.Unmarshal([]byte(request(user, "GET", updates, "", 200)), &got))
	if got.InstanceID != instance.ID || got.Services == nil || len(got.Services) != 0 {
		t.Fatalf("read before any check: %+v", got)
	}
	code(request(user, "POST", check, "", 403), "tenant_access_denied")

	setRole(store.RoleOperator)
	if w := request(user, "POST", check, "", 403); !strings.Contains(w, `"error":"Tenant access denied"`) {
		t.Fatalf("operator refusal: %s", w)
	}

	setRole(store.RoleDeveloper)
	if w := send(user, "POST", check, "", false); w.Code != 403 || !strings.Contains(w.Body.String(), `"error":"Invalid CSRF token"`) {
		t.Fatalf("check without CSRF: %d %s", w.Code, w.Body.String())
	}
	must(json.Unmarshal([]byte(request(user, "POST", check, "", 200)), &got))
	if got.InstanceID != instance.ID || len(got.Services) != 1 {
		t.Fatalf("check: %+v", got)
	}
	if c := got.Services[0]; c.Service != "web" || c.Verdict != "update_available" || c.LocalDigest != local || c.RemoteDigest != remote {
		t.Fatalf("row: %+v", c)
	}
	fake.mu.Lock()
	passed := len(fake.secrets) == 1 && fake.secrets[0] == secret
	fake.mu.Unlock()
	if !passed {
		t.Fatal("the stored credential did not reach the resolver")
	}
	var read store.UpdateCheck
	must(json.Unmarshal([]byte(request(user, "GET", updates, "", 200)), &read))
	if len(read.Services) != 1 || read.Services[0].Verdict != "update_available" || read.Services[0].RemoteDigest != remote {
		t.Fatalf("read after check: %+v", read)
	}

	// A second check while one runs is refused; the first still completes.
	gated := &fakeDigests{digest: local, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	var release sync.Once
	open := func() { release.Do(func() { close(gated.gate) }) }
	t.Cleanup(open)
	api.SetDigestResolverForTest(s, gated)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- tenantRequest(s, user, "POST", check, "", true) }()
	<-gated.entered
	code(request(user, "POST", check, "", 409), "check_in_progress")
	// Any spelling uuid.Parse accepts names the same slot.
	code(request(user, "POST", base+"/"+strings.ToUpper(app.ID)+"/updates/check", "", 409), "check_in_progress")
	// Authorization answers before the slot: no 409 for callers who may not check.
	code(request(outsider, "POST", check, "", 403), "tenant_access_denied")
	code(request(watcher, "POST", check, "", 403), "tenant_access_denied")
	request(user, "POST", base+"/"+uuid.NewString()+"/updates/check", "", 404)
	request(user, "POST", base+"/not-a-uuid/updates/check", "", 400)
	open()
	if w := <-first; w.Code != 200 || !strings.Contains(w.Body.String(), `"verdict":"current"`) {
		t.Fatalf("first check: %d %s", w.Code, w.Body.String())
	}
	// The slot is released: another check runs.
	must(json.Unmarshal([]byte(request(user, "POST", check, "", 200)), &got))

	setRole(store.RoleOrganizationAdmin)
	records, err := ts.ReadAudit(ctx, store.TenantAccess{ActorID: "usr_checker", OrganizationID: "a"}, 0, 200)
	must(err)
	denied := 0
	for _, rec := range records {
		if rec.UserID == "usr_watcher" && rec.Action == "application.deploy" && rec.Result == "denied" && rec.Resource == app.ID+"/updates" {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("operator denial audit rows on %s/updates: %d", app.ID, denied)
	}

	// A check outliving the listener's WriteTimeout still delivers its answer.
	slow := &fakeDigests{digest: remote, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	api.SetDigestResolverForTest(s, slow)
	live := httptest.NewUnstartedServer(s)
	live.Config.WriteTimeout = 300 * time.Millisecond
	live.Start()
	t.Cleanup(live.Close)
	go func() {
		<-slow.entered
		time.Sleep(3 * live.Config.WriteTimeout)
		close(slow.gate)
	}()
	req, _ := http.NewRequest("POST", live.URL+check, nil)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(user)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "token"})
	req.Header.Set(auth.HeaderCSRF, "token")
	resp, err := live.Client().Do(req)
	must(err)
	slowBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	must(err)
	if resp.StatusCode != 200 || !strings.Contains(string(slowBody), `"verdict":"update_available"`) {
		t.Fatalf("slow check: %d %s", resp.StatusCode, slowBody)
	}

	other := "/api/organizations/b/environments/env-b/applications/" + app.ID + "/updates"
	code(request(user, "GET", other, "", 403), "tenant_access_denied")
	code(request(user, "POST", other+"/check", "", 403), "tenant_access_denied")
}
