package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/google/uuid"
)

func insertImageCheck(t *testing.T, st *SQLStore, instance string, at time.Time) {
	t.Helper()
	if _, err := st.db.Exec(st.rebind(`INSERT INTO image_checks(instance_id,service_name,reference,local_digest,remote_digest,verdict,detail,checked_at) VALUES(?,?,?,?,?,?,?,?)`), instance, "web", "nginx:1", "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64), "update_available", "", at); err != nil {
		t.Fatal(err)
	}
}

func imageCheckCount(t *testing.T, st *SQLStore, instance string) (n int) {
	t.Helper()
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM image_checks WHERE instance_id=?`), instance).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReadImageChecksEmptyWithoutInstance(t *testing.T) {
	st, a, app, _, _ := adoptionFixture(t)
	ctx := context.Background()
	out, err := st.Tenancy().ReadImageChecks(ctx, a, app.ID)
	if err != nil || out.InstanceID != "" || out.MappingVersion != 0 || out.Services == nil || len(out.Services) != 0 {
		t.Fatalf("empty read: %+v %v", out, err)
	}
	if _, err := st.Tenancy().ReadImageChecks(ctx, a, "not-a-uuid"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	noEnv := a
	noEnv.EnvironmentID = ""
	if _, err := st.Tenancy().ReadImageChecks(ctx, noEnv, app.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no environment: %v", err)
	}
}

func TestImageChecksClearedBySuccessfulApply(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	at := time.Now().UTC().Truncate(time.Second)
	insertImageCheck(t, st, m.InstanceID, at)
	out, err := ts.ReadImageChecks(ctx, a, app.ID)
	if err != nil || out.InstanceID != m.InstanceID || out.MappingVersion != m.Version || len(out.Services) != 1 {
		t.Fatalf("read: %+v %v", out, err)
	}
	if c := out.Services[0]; c.Service != "web" || c.Reference != "nginx:1" || c.Verdict != "update_available" || c.LocalDigest == "" || c.RemoteDigest == "" || !c.CheckedAt.Equal(at) {
		t.Fatalf("row: %+v", c)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); err != nil {
		t.Fatal(err)
	}
	if n := imageCheckCount(t, st, m.InstanceID); n != 0 {
		t.Fatalf("rows outlived the apply: %d", n)
	}
}

func TestImageChecksSurviveAFailedApply(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	insertImageCheck(t, st, m.InstanceID, time.Now().UTC())
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); err != nil {
		t.Fatal(err)
	}
	if n := imageCheckCount(t, st, m.InstanceID); n != 1 {
		t.Fatalf("failed apply cleared the check: %d", n)
	}
}

func TestImageChecksCascadeOnRelease(t *testing.T) {
	st, a, app, endpoint, _ := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	insertImageCheck(t, st, instance.ID, time.Now().UTC())
	if err := ts.ReleaseApplication(ctx, a, app.ID, instance.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	if n := imageCheckCount(t, st, instance.ID); n != 0 {
		t.Fatalf("rows outlived the release: %d", n)
	}
}

type fakeCall struct {
	Ref          registry.Reference
	Cred         *registry.Credential
	AllowPrivate bool
}

type fakeReply struct {
	digest string
	err    error
}

type fakeResolver struct {
	mu     sync.Mutex
	calls  []fakeCall
	reply  map[string]fakeReply // keyed by host/repository:tag
	block  chan struct{}        // when non-nil, Head waits on it or ctx
	during func()               // when set, runs inside Head
}

func (f *fakeResolver) Head(ctx context.Context, ref registry.Reference, cred *registry.Credential, allowPrivate bool) (string, error) {
	f.mu.Lock()
	var copied *registry.Credential
	if cred != nil {
		c := *cred
		copied = &c
	}
	f.calls = append(f.calls, fakeCall{Ref: ref, Cred: copied, AllowPrivate: allowPrivate})
	r, ok := f.reply[ref.Host+"/"+ref.Repository+":"+ref.Tag]
	f.mu.Unlock()
	if f.during != nil {
		f.during()
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if !ok {
		return "", errors.New("unexpected reference")
	}
	return r.digest, r.err
}

func (f *fakeResolver) called() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func digestOf(c string) string { return "sha256:" + strings.Repeat(c, 64) }

var imageCheckKey = []byte(strings.Repeat("k", 32))

// imageCheckFixture adopts the services' containers and maps each service to its own.
func imageCheckFixture(t *testing.T, services []ApplicationService, digests map[string][]string) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot) {
	t.Helper()
	st, a, app, endpoint, snapshot := adoptionFixtureWith(t, services, digests)
	ctx := context.Background()
	ts := st.Tenancy()
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
		t.Fatal(err)
	}
	remapImageChecks(t, st, a, app, services, snapshot)
	return st, a, app, endpoint, snapshot
}

// remapImageChecks binds each service to the container adoptionFixtureWith gave it by name.
func remapImageChecks(t *testing.T, st *SQLStore, a TenantAccess, app *Application, services []ApplicationService, snapshot protocol.Snapshot) {
	t.Helper()
	ctx := context.Background()
	m, err := st.Tenancy().ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := MappingRequest{InstanceID: m.InstanceID, Version: m.Version, Digest: m.Preview.Digest, Confirm: m.Preview.Project, Bindings: map[string]string{}}
	for _, s := range services {
		for _, c := range snapshot.Containers {
			if c.Name == "shop-"+s.Name {
				r.Bindings[s.Name] = c.ID
			}
		}
	}
	if err := st.Tenancy().SetApplicationMapping(ctx, a, app.ID, r); err != nil {
		t.Fatal(err)
	}
}

// nginxCheckFixture is one mapped nginx:1 service with a known local digest, anonymous pull on.
func nginxCheckFixture(t *testing.T) (*SQLStore, TenantAccess, *Application) {
	t.Helper()
	st, a, app, _, _ := imageCheckFixture(t, []ApplicationService{{Name: "web", Image: "nginx:1"}}, map[string][]string{"web": {"nginx@" + digestOf("e")}})
	org := a
	org.EnvironmentID = ""
	if err := st.Tenancy().SetAnonymousPull(context.Background(), org, true); err != nil {
		t.Fatal(err)
	}
	return st, a, app
}

func byService(rows []ImageCheck) map[string]ImageCheck {
	out := map[string]ImageCheck{}
	for _, r := range rows {
		out[r.Service] = r
	}
	return out
}

func TestCheckImageUpdatesVerdicts(t *testing.T) {
	services := []ApplicationService{
		{Name: "web", Image: "ghcr.io/org/web:1.2"},
		{Name: "db", Image: "postgres:16"},
		{Name: "pinned", Image: "ghcr.io/org/x@" + digestOf("c")},
		{Name: "built", Image: "local/thing:1"},
		{Name: "dup", Image: "quay.io/a/b:1"},
	}
	digests := map[string][]string{
		"web":    {"ghcr.io/org/web@" + digestOf("a")},
		"db":     {"postgres@" + digestOf("b")},
		"pinned": {"ghcr.io/org/x@" + digestOf("c")},
		"built":  {},
		"dup":    {"quay.io/a/b@" + digestOf("1"), "quay.io/a/b@" + digestOf("2")},
	}
	st, a, app, _, _ := imageCheckFixture(t, services, digests)
	ctx := context.Background()
	ts := st.Tenancy()
	org := a
	org.EnvironmentID = ""
	if _, err := ts.PutRegistry(ctx, org, RegistryInput{Host: "ghcr.io", Name: "GitHub", Username: "bot", Credential: ptr("secret")}, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
	f := &fakeResolver{reply: map[string]fakeReply{
		"ghcr.io/org/web:1.2":           {digest: digestOf("f")},
		"docker.io/library/postgres:16": {digest: digestOf("b")},
	}}
	out, err := ts.CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil || out.InstanceID != m.InstanceID || out.MappingVersion != m.Version || len(out.Services) != 5 {
		t.Fatalf("check: %+v %v", out, err)
	}
	want := map[string][4]string{ // verdict, detail, local, remote
		"web":    {"update_available", "", digestOf("a"), digestOf("f")},
		"db":     {"registry_error", "not_configured", digestOf("b"), ""},
		"pinned": {"pinned", "", "", ""},
		"built":  {"unknown_local", "", "", ""},
		"dup":    {"unknown_local", "", "", ""},
	}
	stored, err := ts.ReadImageChecks(ctx, a, app.ID)
	if err != nil || len(stored.Services) != 5 {
		t.Fatalf("stored: %+v %v", stored, err)
	}
	returned, persisted := byService(out.Services), byService(stored.Services)
	for name, w := range want {
		for _, got := range []ImageCheck{returned[name], persisted[name]} {
			if got.Verdict != w[0] || got.Detail != w[1] || got.LocalDigest != w[2] || got.RemoteDigest != w[3] || got.Reference == "" || got.CheckedAt.IsZero() {
				t.Fatalf("%s: %+v", name, got)
			}
		}
	}
	calls := f.called()
	if len(calls) != 1 || calls[0].Ref.Host != "ghcr.io" || calls[0].Cred == nil || calls[0].Cred.Secret != "secret" || calls[0].Cred.Username != "bot" || calls[0].AllowPrivate {
		t.Fatalf("first run calls: %+v", calls)
	}

	if err := ts.SetAnonymousPull(ctx, org, true); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	out, err = ts.CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if db := byService(out.Services)["db"]; db.Verdict != "current" || db.Detail != "" || db.RemoteDigest != digestOf("b") {
		t.Fatalf("db with anonymous pull: %+v", db)
	}
	calls = f.called()
	if len(calls) != 2 {
		t.Fatalf("second run calls: %+v", calls)
	}
	for _, c := range calls {
		switch c.Ref.Host {
		case "docker.io":
			if c.Cred != nil || c.Ref.Repository != "library/postgres" {
				t.Fatalf("anonymous call: %+v", c)
			}
		case "ghcr.io":
		default:
			t.Fatalf("unexpected call: %+v", c)
		}
	}

	details := registryAudit(t, st, "application.deploy", app.ID+"/updates")
	if len(details) != 2 || details[0] != "services=5 updates=1 errors=1" || details[1] != "services=5 updates=1 errors=0" {
		t.Fatalf("audit: %q", details)
	}
	var leaked int
	if err := st.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM audit_records WHERE details LIKE '%secret%' OR resource LIKE '%secret%')+(SELECT COUNT(*) FROM image_checks WHERE service_name LIKE '%secret%' OR reference LIKE '%secret%' OR local_digest LIKE '%secret%' OR remote_digest LIKE '%secret%' OR verdict LIKE '%secret%' OR detail LIKE '%secret%')`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("secret stored: %d %v", leaked, err)
	}
	if raw, err := json.Marshal(out); err != nil || strings.Contains(string(raw), "secret") {
		t.Fatalf("secret returned: %s %v", raw, err)
	}
}

func TestCheckImageUpdatesRegistryErrors(t *testing.T) {
	st, a, app := nginxCheckFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		err    error
		detail string
	}{
		{registry.ErrUnauthorized, "unauthorized"},
		{registry.ErrNotFound, "not_found"},
		{registry.ErrRateLimited, "rate_limited"},
		{registry.ErrPrivateDestination, "private_destination"},
		{errors.New("boom"), "unavailable"},
	} {
		f := &fakeResolver{reply: map[string]fakeReply{"docker.io/library/nginx:1": {err: tc.err}}}
		out, err := st.Tenancy().CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false)
		if err != nil {
			t.Fatal(err)
		}
		c := out.Services[0]
		if len(out.Services) != 1 || c.Verdict != "registry_error" || c.Detail != tc.detail || c.RemoteDigest != "" || c.LocalDigest != digestOf("e") || strings.Contains(c.Detail, "boom") {
			t.Fatalf("%v: %+v", tc.err, out.Services)
		}
	}
}

// Anything the rows describe that changes while the registry is asked refuses the write.
func TestCheckImageUpdatesRefusesAChangeMidCheck(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, ts TenancyStore, a TenantAccess, app *Application){
		"mapping": func(t *testing.T, ts TenancyStore, a TenantAccess, app *Application) {
			m, err := ts.ReadApplicationMapping(context.Background(), a, app.ID)
			if err == nil {
				err = ts.SetApplicationMapping(context.Background(), a, app.ID, mappingRequest(m))
			}
			if err != nil {
				t.Error(err)
			}
		},
		"revision": func(t *testing.T, ts TenancyStore, a TenantAccess, app *Application) {
			if _, err := ts.AppendApplicationRevision(context.Background(), a, app.ID, 1, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:2"}}}); err != nil {
				t.Error(err)
			}
		},
		"release": func(t *testing.T, ts TenancyStore, a TenantAccess, app *Application) {
			m, err := ts.ReadApplicationMapping(context.Background(), a, app.ID)
			if err == nil {
				err = ts.ReleaseApplication(context.Background(), a, app.ID, m.InstanceID, "shop")
			}
			if err != nil {
				t.Error(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, a, app := nginxCheckFixture(t)
			ctx := context.Background()
			ts := st.Tenancy()
			m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
			if err != nil {
				t.Fatal(err)
			}
			f := &fakeResolver{reply: map[string]fakeReply{"docker.io/library/nginx:1": {digest: digestOf("e")}}}
			f.during = func() { change(t, ts, a, app) }
			if _, err := ts.CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false); !errors.Is(err, ErrAdoptionChanged) {
				t.Fatalf("check: %v", err)
			}
			if len(f.called()) != 1 {
				t.Fatalf("the change did not run mid-check: %+v", f.called())
			}
			if n := imageCheckCount(t, st, m.InstanceID); n != 0 {
				t.Fatalf("rows written: %d", n)
			}
		})
	}
}

// An apply that succeeds while the registry is asked rebinds the containers; the check made
// before it must not land over it.
func TestCheckImageUpdatesRefusesASettledApply(t *testing.T) {
	st, a, app, endpoint, snapshot, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	snapshot.Images = append(snapshot.Images, protocol.Image{ID: snapshot.Containers[0].ImageID, Digests: []string{"nginx@" + digestOf("f")}})
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	org := a
	org.EnvironmentID = ""
	if err := ts.SetAnonymousPull(ctx, org, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	f := &fakeResolver{reply: map[string]fakeReply{"docker.io/library/nginx:1": {digest: digestOf("e")}}}
	f.during = func() {
		if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); err != nil {
			t.Error(err)
		}
	}
	if _, err := ts.CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatalf("check over a settled apply: %v", err)
	}
	if len(f.called()) != 1 {
		t.Fatalf("calls: %+v", f.called())
	}
	if n := imageCheckCount(t, st, m.InstanceID); n != 0 {
		t.Fatalf("stale rows written: %d", n)
	}
}

func TestCheckImageUpdatesRefusesMalformedLocalDigests(t *testing.T) {
	for _, digest := range []string{"nginx@sha256:" + strings.Repeat("E", 64), "nginx@sha256:" + strings.Repeat("e", 63), "nginx@latest", "nginx@"} {
		st, a, app, _, _ := imageCheckFixture(t, []ApplicationService{{Name: "web", Image: "nginx:1"}}, map[string][]string{"web": {digest}})
		org := a
		org.EnvironmentID = ""
		if err := st.Tenancy().SetAnonymousPull(context.Background(), org, true); err != nil {
			t.Fatal(err)
		}
		f := &fakeResolver{reply: map[string]fakeReply{"docker.io/library/nginx:1": {digest: digestOf("e")}}}
		out, err := st.Tenancy().CheckImageUpdates(context.Background(), a, app.ID, f, imageCheckKey, false)
		if err != nil || len(out.Services) != 1 || out.Services[0].Verdict != "unknown_local" || out.Services[0].LocalDigest != "" || len(f.called()) != 0 {
			t.Fatalf("%q: %+v %v %+v", digest, out, err, f.called())
		}
	}
}

func TestCheckImageUpdatesReplacesRows(t *testing.T) {
	services := []ApplicationService{{Name: "web", Image: "nginx:1"}, {Name: "api", Image: "nginx:2"}}
	st, a, app, _, snapshot := imageCheckFixture(t, services, map[string][]string{"web": {"nginx@" + digestOf("e")}, "api": {"nginx@" + digestOf("d")}})
	ctx := context.Background()
	ts := st.Tenancy()
	org := a
	org.EnvironmentID = ""
	if err := ts.SetAnonymousPull(ctx, org, true); err != nil {
		t.Fatal(err)
	}
	f := &fakeResolver{reply: map[string]fakeReply{"docker.io/library/nginx:1": {digest: digestOf("e")}, "docker.io/library/nginx:2": {digest: digestOf("d")}}}
	if _, err := ts.CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
	stored, err := ts.ReadImageChecks(ctx, a, app.ID)
	if err != nil || len(stored.Services) != 2 {
		t.Fatalf("first check: %+v %v", stored, err)
	}
	kept := services[:1]
	if _, err := ts.AppendApplicationRevision(ctx, a, app.ID, 1, ApplicationSpec{Kind: "compose.v1", Services: kept}); err != nil {
		t.Fatal(err)
	}
	remapImageChecks(t, st, a, app, kept, snapshot)
	if _, err := ts.CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
	stored, err = ts.ReadImageChecks(ctx, a, app.ID)
	if err != nil || len(stored.Services) != 1 || stored.Services[0].Service != "web" || stored.Services[0].Verdict != "current" {
		t.Fatalf("second check: %+v %v", stored, err)
	}
	// A service the revision defines but the mapping does not bind gets no row.
	if _, err := ts.AppendApplicationRevision(ctx, a, app.ID, 2, ApplicationSpec{Kind: "compose.v1", Services: services}); err != nil {
		t.Fatal(err)
	}
	remapImageChecks(t, st, a, app, kept, snapshot)
	if _, err := ts.CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
	stored, err = ts.ReadImageChecks(ctx, a, app.ID)
	if err != nil || len(stored.Services) != 1 || stored.Services[0].Service != "web" {
		t.Fatalf("unbound service: %+v %v", stored, err)
	}
}

func TestCheckImageUpdatesDeadline(t *testing.T) {
	services := []ApplicationService{}
	digests := map[string][]string{}
	for _, n := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		services = append(services, ApplicationService{Name: n, Image: "nginx:" + n})
		digests[n] = []string{"nginx@" + digestOf("e")}
	}
	st, a, app, _, _ := imageCheckFixture(t, services, digests)
	ctx := context.Background()
	org := a
	org.EnvironmentID = ""
	if err := st.Tenancy().SetAnonymousPull(ctx, org, true); err != nil {
		t.Fatal(err)
	}
	old := ImageCheckDeadline
	ImageCheckDeadline = 200 * time.Millisecond
	t.Cleanup(func() { ImageCheckDeadline = old })
	f := &fakeResolver{block: make(chan struct{})}
	t.Cleanup(func() { close(f.block) })
	start := time.Now()
	out, err := st.Tenancy().CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("check took %v", d)
	}
	if len(out.Services) != 6 {
		t.Fatalf("services: %+v", out.Services)
	}
	for _, c := range out.Services {
		if c.Verdict != "registry_error" || c.Detail != "unavailable" {
			t.Fatalf("unresolved: %+v", c)
		}
	}
	// At most the concurrency limit reached the registry; the rest gave up waiting for a slot.
	if n := len(f.called()); n < 1 || n > imageCheckConcurrency {
		t.Fatalf("calls: %d", n)
	}
}

func targetDenials(t *testing.T, st *SQLStore, resource string) (n int) {
	t.Helper()
	if err := st.db.QueryRowContext(context.Background(), st.rebind(`SELECT COUNT(*) FROM audit_records WHERE action='application.deploy' AND result='denied' AND resource=?`), resource).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCheckImageUpdatesPermissions(t *testing.T) {
	st, a, app := nginxCheckFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	f := &fakeResolver{reply: map[string]fakeReply{"docker.io/library/nginx:1": {digest: digestOf("e")}}}
	if _, err := ts.CheckImageUpdates(ctx, a, uuid.NewString(), f, imageCheckKey, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown application: %v", err)
	}
	if _, err := ts.CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey[:31], false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short key: %v", err)
	}
	if _, err := ts.CheckImageUpdates(ctx, a, app.ID, nil, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no resolver: %v", err)
	}
	for _, role := range []TenantRole{RoleDeveloper, RoleOperator, RoleReadOnly} {
		if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		denied := auditCount(t, st, "application.deploy", "denied")
		_, err := ts.CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false)
		if role == RoleDeveloper {
			if err != nil {
				t.Fatalf("developer: %v", err)
			}
			continue
		}
		if !errors.Is(err, ErrForbidden) || auditCount(t, st, "application.deploy", "denied") != denied+1 || targetDenials(t, st, app.ID+"/updates") != denied+1 {
			t.Fatalf("%s: %v", role, err)
		}
	}

	st, a, app, _, _, _ = mappingFixture(t)
	if _, err := st.Tenancy().CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false); !errors.Is(err, ErrMappingRequired) {
		t.Fatalf("unmapped instance: %v", err)
	}
}

func TestCheckImageUpdatesPrivateFlag(t *testing.T) {
	st, a, app, _, _ := imageCheckFixture(t, []ApplicationService{{Name: "web", Image: "nginx:1"}}, map[string][]string{"web": {"nginx@" + digestOf("e")}})
	ctx := context.Background()
	org := a
	org.EnvironmentID = ""
	if _, err := st.Tenancy().PutRegistry(ctx, org, RegistryInput{Host: "docker.io", Name: "Hub", AllowPrivate: true}, imageCheckKey, true); err != nil {
		t.Fatal(err)
	}
	for _, allowed := range []bool{false, true} {
		f := &fakeResolver{reply: map[string]fakeReply{"docker.io/library/nginx:1": {digest: digestOf("e")}}}
		if _, err := st.Tenancy().CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, allowed); err != nil {
			t.Fatal(err)
		}
		if calls := f.called(); len(calls) != 1 || calls[0].AllowPrivate != allowed || calls[0].Cred != nil {
			t.Fatalf("allowed=%t: %+v", allowed, calls)
		}
	}
}

func TestCheckImageUpdateAccess(t *testing.T) {
	st, a, app := nginxCheckFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	noEnv := a
	noEnv.EnvironmentID = ""
	for _, tc := range []struct {
		a   TenantAccess
		app string
		err error
	}{
		{a, "not-a-uuid", ErrInvalid},
		{noEnv, app.ID, ErrInvalid},
		{a, uuid.NewString(), ErrNotFound},
		{TenantAccess{ActorID: "usr_nobody", OrganizationID: a.OrganizationID, EnvironmentID: a.EnvironmentID}, app.ID, ErrForbidden},
	} {
		if err := ts.CheckImageUpdateAccess(ctx, tc.a, tc.app); !errors.Is(err, tc.err) {
			t.Fatalf("%+v %s: %v, want %v", tc.a, tc.app, err, tc.err)
		}
	}
	for _, role := range []TenantRole{RoleDeveloper, RoleOperator, RoleReadOnly} {
		if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		denied, allowed := targetDenials(t, st, app.ID+"/updates"), auditCount(t, st, "application.deploy", "success")
		err := ts.CheckImageUpdateAccess(ctx, a, strings.ToUpper(app.ID))
		if role == RoleDeveloper {
			// Admission only: the check itself writes the success row.
			if err != nil || auditCount(t, st, "application.deploy", "success") != allowed {
				t.Fatalf("developer: %v", err)
			}
			continue
		}
		// The target is the canonical ID whatever spelling was asked for.
		if !errors.Is(err, ErrForbidden) || targetDenials(t, st, app.ID+"/updates") != denied+1 {
			t.Fatalf("%s: %v", role, err)
		}
	}
}

// A client that leaves mid-registry still gets its check audited, as a failure, and the cached
// rows stay as they were.
func TestCheckImageUpdatesCancelledAuditsAFailure(t *testing.T) {
	st, a, app := nginxCheckFixture(t)
	ts := st.Tenancy()
	m, err := ts.ReadApplicationMapping(context.Background(), a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	insertImageCheck(t, st, m.InstanceID, at)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeResolver{reply: map[string]fakeReply{"docker.io/library/nginx:1": {digest: digestOf("f")}}, during: cancel}
	if _, err := ts.CheckImageUpdates(ctx, a, app.ID, f, imageCheckKey, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled check: %v", err)
	}
	if len(f.called()) != 1 {
		t.Fatalf("calls: %+v", f.called())
	}
	var failures, successes int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(CASE WHEN result='failure' THEN 1 END),COUNT(CASE WHEN result='success' THEN 1 END) FROM audit_records WHERE action='application.deploy' AND resource=?`), app.ID+"/updates").Scan(&failures, &successes); err != nil {
		t.Fatal(err)
	}
	if failures != 1 || successes != 0 {
		t.Fatalf("audit: failures=%d successes=%d", failures, successes)
	}
	stored, err := ts.ReadImageChecks(context.Background(), a, app.ID)
	if err != nil || len(stored.Services) != 1 {
		t.Fatalf("stored: %+v %v", stored, err)
	}
	if c := stored.Services[0]; c.Verdict != "update_available" || c.RemoteDigest != digestOf("2") || !c.CheckedAt.Equal(at) {
		t.Fatalf("cached row changed: %+v", c)
	}
}

// Only the reference's own repository counts, however the host image spells it.
func TestLocalRepoDigestRepositoryFilter(t *testing.T) {
	nginx, err := registry.ParseReference("nginx:1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		digests []string
		want    string
	}{
		{"foreign beside own", []string{"ghcr.io/other/nginx@" + digestOf("a"), "nginx@" + digestOf("b")}, digestOf("b")},
		{"own beside foreign", []string{"nginx@" + digestOf("b"), "docker.io/other/nginx@" + digestOf("a")}, digestOf("b")},
		{"foreign alone", []string{"ghcr.io/library/nginx@" + digestOf("a")}, ""},
		{"docker.io spelling", []string{"docker.io/library/nginx@" + digestOf("c")}, digestOf("c")},
		{"index.docker.io spelling", []string{"index.docker.io/library/nginx@" + digestOf("c")}, digestOf("c")},
		{"bare spelling", []string{"nginx@" + digestOf("c")}, digestOf("c")},
	} {
		if got := localRepoDigest(protocol.Image{Digests: tc.digests}, nginx); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A host image carrying only another repository's digest is unknown_local, with no call.
func TestCheckImageUpdatesForeignLocalDigest(t *testing.T) {
	st, a, app, _, _ := imageCheckFixture(t, []ApplicationService{{Name: "web", Image: "nginx:1"}}, map[string][]string{"web": {"ghcr.io/org/nginx@" + digestOf("e")}})
	org := a
	org.EnvironmentID = ""
	if err := st.Tenancy().SetAnonymousPull(context.Background(), org, true); err != nil {
		t.Fatal(err)
	}
	f := &fakeResolver{reply: map[string]fakeReply{"docker.io/library/nginx:1": {digest: digestOf("e")}}}
	out, err := st.Tenancy().CheckImageUpdates(context.Background(), a, app.ID, f, imageCheckKey, false)
	if err != nil || len(out.Services) != 1 || out.Services[0].Verdict != "unknown_local" || len(f.called()) != 0 {
		t.Fatalf("%+v %v %+v", out, err, f.called())
	}
}
