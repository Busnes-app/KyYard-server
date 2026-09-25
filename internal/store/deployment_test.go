package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/registry"
)

func TestDeploymentsTableExists(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	var n int
	if err := st.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deployments table: %d %v", n, err)
	}
}

func planFixture(t *testing.T) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot, *ApplicationMapping) {
	t.Helper()
	st, a, app, endpoint, snapshot, m := mappingFixture(t)
	snapshot.Images = []protocol.Image{{ID: "sha256:" + strings.Repeat("d", 64), Tags: []string{"nginx:1"}, Digests: []string{"nginx@sha256:" + strings.Repeat("e", 64)}}}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if err := st.Tenancy().SetApplicationMapping(context.Background(), a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, err := st.Tenancy().ReadApplicationMapping(context.Background(), a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, a, app, endpoint, snapshot, m
}

// planRequest is what the API sends for m: the cap of an agent with deployment.pull and a
// verified live inspection of every adopted container.
func planRequest(m *ApplicationMapping) PlanRequest {
	r := PlanRequest{InstanceID: m.InstanceID, MappingVersion: m.Version, Revision: m.Preview.Revision, Confirm: m.Preview.Project, MaxFrameBytes: protocol.MaxDeploymentRequestBytes, Inspections: map[string]protocol.ContainerInspection{}}
	for _, c := range m.Preview.Containers {
		target := protocol.InspectionTarget{ContainerID: c.ID, ImageID: c.ImageID, CreatedUnix: c.CreatedAt.Unix()}
		r.Inspections[c.ID] = protocol.ContainerInspection{Target: target, ObservedAt: time.Now().UTC(), ConfigurationVerified: true, Unsupported: []string{}}
	}
	return r
}

func TestPlanDeploymentBindsIdentities(t *testing.T) {
	st, a, app, _, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	c := snapshot.Containers[0]
	if d.State != "planned" || d.InstanceID != m.InstanceID || d.Revision != 1 || d.MappingVersion != 1 || d.Expired || time.Until(d.ExpiresAt) > DeploymentPlanTTL || time.Until(d.ExpiresAt) < 9*time.Minute {
		t.Fatalf("plan: %+v", d)
	}
	s := d.Plan.Services[0]
	if d.Plan.Project != "shop" || s.Name != "web" || s.ImageID != snapshot.Images[0].ID || s.ImageDigest != snapshot.Images[0].Digests[0] || s.ContainerID != c.ID || s.Replaces.ImageID != c.ImageID || s.Replaces.CreatedUnix != c.CreatedAt.Unix() {
		t.Fatalf("service: %+v", s)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.ID != d.ID || got.SpecDigest != d.SpecDigest {
		t.Fatalf("read: %+v %v", got, err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 1 || list[0].ID != d.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	var commands int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM endpoint_commands`).Scan(&commands); err != nil || commands != 0 {
		t.Fatal("plan dispatched a command")
	}
	foreign := a
	foreign.EnvironmentID = "foreign"
	if _, err = ts.ReadDeployment(ctx, foreign, app.ID, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign read: %v", err)
	}
	if _, err = ts.PlanDeployment(ctx, foreign, app.ID, planRequest(m), nil, imageCheckKey, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign plan: %v", err)
	}
}

func TestPlanDeploymentRefusesStaleAndBlocked(t *testing.T) {
	st, a, app, endpoint, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for name, r := range map[string]PlanRequest{
		"instance": {InstanceID: "other", MappingVersion: 1, Revision: 1, Confirm: "shop"},
		"mapping":  {InstanceID: m.InstanceID, MappingVersion: 2, Revision: 1, Confirm: "shop"},
		"project":  {InstanceID: m.InstanceID, MappingVersion: 1, Revision: 1, Confirm: "nope"},
	} {
		if _, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, imageCheckKey, false); !errors.Is(err, ErrAdoptionChanged) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, PlanRequest{InstanceID: m.InstanceID, MappingVersion: 1, Revision: 2, Confirm: "shop"}, nil, imageCheckKey, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unsaved revision: %v", err)
	}
	// Definition advances: until the mapping is reviewed against the head, no revision plans.
	if _, err := ts.AppendApplicationRevision(ctx, a, app.ID, 1, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}); err != nil {
		t.Fatal(err)
	}
	var blocked *PreflightBlockedError
	for _, rev := range []int{1, 2} {
		r := planRequest(m)
		r.Revision = rev
		if _, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, imageCheckKey, false); !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, "mapping_requires_review") {
			t.Fatalf("revision %d: mapping stale not reported: %v", rev, err)
		}
	}
	// Re-map, then remove the image from inventory: a service blocker refuses too.
	m2, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m2)); err != nil {
		t.Fatal(err)
	}
	snapshot.Images = nil
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	m2, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m2), nil, imageCheckKey, false); !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, "image_not_reported") {
		t.Fatalf("service blocker not reported: %v", err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 0 {
		t.Fatal("refused plan left a row")
	}
}

func TestPlanDeploymentReplacesAndExpires(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	first, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil || second.ID == first.ID {
		t.Fatalf("replacement: %+v %v", second, err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 1 || list[0].ID != second.ID {
		t.Fatalf("one row per instance: %+v %v", list, err)
	}
	if _, err = ts.ReadDeployment(ctx, a, app.ID, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("replaced plan still readable")
	}
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=?`), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, second.ID)
	if err != nil || !got.Expired {
		t.Fatalf("expiry: %+v %v", got, err)
	}
}

func TestPlanDeploymentRoles(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for role, ok := range map[TenantRole]bool{RoleDeveloper: true, RoleEnvironmentAdmin: true, RoleOperator: false, RoleReadOnly: false} {
		if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		_, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
		if ok && err != nil || !ok && !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s: %v", role, err)
		}
		if _, err = ts.ListDeployments(ctx, a, app.ID); err != nil {
			t.Fatalf("%s list: %v", role, err)
		}
	}
}

// An adopted container whose image ID lacks the sha256: prefix passes adoption (which only
// requires a non-empty image ID) but cannot back a real InspectionTarget. PlanDeployment must
// refuse it rather than mint a plan naming a container with no identity to recheck at apply.
func TestPlanDeploymentRefusesInvalidReplacementIdentity(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app, err := ts.ImportApplication(ctx, a, "shop", ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}, map[string]string{}, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{{ID: strings.Repeat("a", 64), Name: "shop-web", ImageID: strings.Repeat("b", 64), CreatedAt: time.Now().UTC().Add(-time.Hour), ComposeProject: "shop"}}}
	endpoint := activeEndpointWith(t, ts, a, snapshot.Containers, nil)
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
		t.Fatal(err)
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, err = ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Images = []protocol.Image{{ID: "sha256:" + strings.Repeat("d", 64), Tags: []string{"nginx:1"}}}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	var blocked *PreflightBlockedError
	if _, err = ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, "replacement_identity_invalid") {
		t.Fatalf("invalid replacement identity not reported: %v", err)
	}
}

func TestReleaseRefusesLivePlanAndDeletesExpired(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
	if err := ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); !errors.Is(err, ErrDeploymentPlanned) {
		t.Fatalf("release with live plan: %v", err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=?`), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 0 {
		t.Fatal("release kept plan rows")
	}
}

func TestMigration24ShapesDeployments(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	ctx := context.Background()
	for _, col := range []string{"applied_by", "applied_at", "deadline", "settled_at", "detail", "result"} {
		if _, err := st.db.ExecContext(ctx, `SELECT `+col+` FROM deployments LIMIT 1`); err != nil {
			t.Fatalf("column %s: %v", col, err)
		}
	}
	for _, col := range []string{"current_revision", "previous_revision"} {
		if _, err := st.db.ExecContext(ctx, `SELECT `+col+` FROM application_instances LIMIT 1`); err != nil {
			t.Fatalf("instance column %s: %v", col, err)
		}
	}
}

func TestPlanDeploymentRespectsLiveRows(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	first, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	// A settled history row must survive a new plan; only the planned row is replaced.
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET state='succeeded',settled_at=? WHERE id=?`), time.Now().UTC(), first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	list, err := ts.ListDeployments(ctx, a, app.ID)
	if err != nil || len(list) != 2 || list[0].ID != second.ID || list[1].State != "succeeded" {
		t.Fatalf("history kept: %+v %v", list, err)
	}
	// An applying row blocks a new plan.
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET state='applying' WHERE id=?`), second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); !errors.Is(err, ErrDeploymentInProgress) {
		t.Fatalf("plan during apply: %v", err)
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); !errors.Is(err, ErrDeploymentInProgress) {
		t.Fatalf("release during apply: %v", err)
	}
	// Settled rows never block release and are kept.
	if _, err = st.db.Exec(st.rebind(`UPDATE deployments SET state='failed' WHERE id=?`), second.ID); err != nil {
		t.Fatal(err)
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("history after release: %d %v", n, err)
	}
}

const pullCanary = "pull-canary-cred"

// pullFixture adopts and maps services at a revision with a value bundle, so its plans can be
// applied, and saves ghcr.io with a credential. Anonymous pull stays off.
func pullFixture(t *testing.T, services []ApplicationService, digests map[string][]string) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot) {
	t.Helper()
	st, a, app, endpoint, snapshot := imageCheckFixture(t, services, digests)
	ctx := context.Background()
	ts := st.Tenancy()
	for i, s := range services {
		if !strings.Contains(s.Image, "@") {
			snapshot.Images[i].Tags = []string{s.Image}
		}
	}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, ApplicationSpec{Kind: "compose.v1", Services: services}, map[string]string{}, imageCheckKey); err != nil {
		t.Fatal(err)
	}
	remapImageChecks(t, st, a, app, services, snapshot)
	org := a
	org.EnvironmentID = ""
	if _, err := ts.PutRegistry(ctx, org, RegistryInput{Host: "ghcr.io", Name: "GitHub", Username: "bot", Credential: ptr(pullCanary), AllowPrivate: true}, imageCheckKey, true); err != nil {
		t.Fatal(err)
	}
	return st, a, app, endpoint, snapshot
}

func pullPlanRequest(t *testing.T, ts TenancyStore, a TenantAccess, app *Application, update ...string) PlanRequest {
	t.Helper()
	m, err := ts.ReadApplicationMapping(context.Background(), a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := planRequest(m)
	r.Update = update
	return r
}

func deploymentRows(t *testing.T, st *SQLStore, app string) (n int) {
	t.Helper()
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM deployments WHERE application_id=?`), app).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func setAnonymousPull(t *testing.T, st *SQLStore, a TenantAccess, on bool) {
	t.Helper()
	org := a
	org.EnvironmentID = ""
	if err := st.Tenancy().SetAnonymousPull(context.Background(), org, on); err != nil {
		t.Fatal(err)
	}
}

func TestPlanDeploymentPinsARegistryDigest(t *testing.T) {
	services := []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}, {Name: "db", Image: "postgres:16"}}
	st, a, app, _, snapshot := pullFixture(t, services, map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}, "db": {"postgres@" + digestOf("b")}})
	ctx := context.Background()
	ts := st.Tenancy()
	f := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1.2": {digest: digestOf("f")}, "docker.io/library/postgres:16": {digest: digestOf("9")}}}
	for _, private := range []bool{false, true} {
		f.calls = nil
		d, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "web"), f, imageCheckKey, private)
		if err != nil {
			t.Fatal(err)
		}
		calls := f.called()
		if len(calls) != 1 || calls[0].Ref.Host != "ghcr.io" || calls[0].Ref.Repository != "org/web" || calls[0].Ref.Tag != "1.2" || calls[0].Cred == nil || calls[0].Cred.Username != "bot" || calls[0].Cred.Secret != pullCanary || calls[0].AllowPrivate != private {
			t.Fatalf("private=%t calls: %+v", private, calls)
		}
		web, db := d.Plan.Services[0], d.Plan.Services[1]
		if web.PullReference != "ghcr.io/org/web@"+digestOf("f") || web.PullDigest != digestOf("f") || web.ImageID != snapshot.Containers[0].ImageID || db.PullReference != "" || db.PullDigest != "" {
			t.Fatalf("plan: %+v", d.Plan.Services)
		}
		var raw string
		if err := st.db.QueryRow(st.rebind(`SELECT plan FROM deployments WHERE id=?`), d.ID).Scan(&raw); err != nil || !strings.Contains(raw, `"pull_reference":"ghcr.io/org/web@`+digestOf("f")+`"`) || !strings.Contains(raw, `"pull_digest":"`+digestOf("f")+`"`) || strings.Contains(raw, pullCanary) {
			t.Fatalf("stored plan: %s %v", raw, err)
		}
		if got := registryAudit(t, st, "application.deploy", app.ID+"/deployments/"+d.ID); len(got) != 1 || got[0] != "pulls=1" {
			t.Fatalf("audit: %q", got)
		}
	}
	// An unconfigured host needs the anonymous opt-in, and then goes without a credential.
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "db"), f, imageCheckKey, false); !isBlocked(err, "registry_not_configured") {
		t.Fatalf("db without anonymous pull: %v", err)
	}
	setAnonymousPull(t, st, a, true)
	f.calls = nil
	d, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "db", "web"), f, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if db := d.Plan.Services[1]; db.PullReference != "docker.io/library/postgres@"+digestOf("9") || db.PullDigest != digestOf("9") {
		t.Fatalf("db: %+v", db)
	}
	for _, c := range f.called() {
		if c.Ref.Host == "docker.io" && c.Cred != nil {
			t.Fatalf("anonymous call carried a credential: %+v", c)
		}
	}
	if len(f.called()) != 2 {
		t.Fatalf("calls: %+v", f.called())
	}
	if got := registryAudit(t, st, "application.deploy", app.ID+"/deployments/"+d.ID); len(got) != 1 || got[0] != "pulls=2" {
		t.Fatalf("audit: %q", got)
	}
	var leaked int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_records WHERE resource LIKE '%canary%' OR details LIKE '%canary%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("credential in audit: %d %v", leaked, err)
	}
	// Without update the plan pins nothing and its audit row carries no details.
	plain, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app), nil, imageCheckKey, false)
	if err != nil || plain.Plan.Services[0].PullDigest != "" {
		t.Fatalf("plain plan: %+v %v", plain, err)
	}
	if got := registryAudit(t, st, "application.deploy", app.ID+"/deployments/"+plain.ID); len(got) != 1 || got[0] != "" {
		t.Fatalf("plain audit: %q", got)
	}
}

func isBlocked(err error, blocker string) bool {
	var blocked *PreflightBlockedError
	return errors.As(err, &blocked) && slices.Contains(blocked.Blockers, blocker)
}

func TestPlanDeploymentRegistryErrorsBlock(t *testing.T) {
	st, a, app, _, _ := pullFixture(t, []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}}, map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}})
	ctx := context.Background()
	ts := st.Tenancy()
	for _, tc := range []struct {
		reply   fakeReply
		blocker string
	}{
		{fakeReply{err: registry.ErrUnauthorized}, "registry_unauthorized"},
		{fakeReply{err: registry.ErrNotFound}, "registry_not_found"},
		{fakeReply{err: registry.ErrRateLimited}, "registry_rate_limited"},
		{fakeReply{err: registry.ErrPrivateDestination}, "registry_private_destination"},
		{fakeReply{err: errors.New("boom")}, "registry_unavailable"},
		{fakeReply{digest: "not-a-digest"}, "registry_unavailable"},
	} {
		f := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1.2": tc.reply}}
		_, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "web"), f, imageCheckKey, false)
		var blocked *PreflightBlockedError
		if !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{tc.blocker}) || len(f.called()) != 1 {
			t.Fatalf("%s: %v %+v", tc.blocker, err, f.called())
		}
		if n := deploymentRows(t, st, app.ID); n != 0 {
			t.Fatalf("%s: rows written: %d", tc.blocker, n)
		}
	}
}

func TestPlanDeploymentUpdateNames(t *testing.T) {
	services := []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}, {Name: "db", Image: "ghcr.io/org/db:1"}}
	st, a, app, _, snapshot := pullFixture(t, services, map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}, "db": {"ghcr.io/org/db@" + digestOf("b")}})
	ctx := context.Background()
	ts := st.Tenancy()
	f := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1.2": {digest: digestOf("f")}, "ghcr.io/org/db:1": {digest: digestOf("9")}}}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "nope"), f, imageCheckKey, false); !isBlocked(err, "update_not_mapped") {
		t.Fatalf("unknown service: %v", err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "web", "web"), f, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "web"), nil, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no resolver: %v", err)
	}
	many := make([]string, protocol.MaxDeploymentServices+1)
	for i := range many {
		many[i] = fmt.Sprintf("s%d", i)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, many...), f, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many names: %v", err)
	}
	// db loses its binding: naming it is update_not_mapped.
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := MappingRequest{InstanceID: m.InstanceID, Version: m.Version, Digest: m.Preview.Digest, Confirm: m.Preview.Project, Bindings: map[string]string{"web": snapshot.Containers[0].ID}}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, r); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "db"), f, imageCheckKey, false); !isBlocked(err, "update_not_mapped") {
		t.Fatalf("unbound service: %v", err)
	}
	if len(f.called()) != 0 || deploymentRows(t, st, app.ID) != 0 {
		t.Fatalf("a refused plan asked the registry or wrote: %+v", f.called())
	}
}

func TestPlanDeploymentPinnedReferenceSkipsTheRegistry(t *testing.T) {
	pinned, db := "ghcr.io/org/x@"+digestOf("c"), "postgres@"+digestOf("d")
	st, a, app, _, _ := pullFixture(t, []ApplicationService{{Name: "pinned", Image: pinned}, {Name: "db", Image: db}}, map[string][]string{"pinned": {pinned}, "db": {db}})
	ctx := context.Background()
	ts := st.Tenancy()
	f := &fakeResolver{}
	d, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "pinned"), f, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := d.Plan.Services[0]; s.PullReference != pinned || s.PullDigest != digestOf("c") {
		t.Fatalf("pinned: %+v", s)
	}
	// Pinning is no way around the policy: an unconfigured host still needs anonymous pull.
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "db"), f, imageCheckKey, false); !isBlocked(err, "registry_not_configured") {
		t.Fatalf("pinned on an unconfigured host: %v", err)
	}
	setAnonymousPull(t, st, a, true)
	d, err = ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "db"), f, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := d.Plan.Services[1]; s.PullReference != "docker.io/library/postgres@"+digestOf("d") || s.PullDigest != digestOf("d") {
		t.Fatalf("db: %+v", s)
	}
	if len(f.called()) != 0 {
		t.Fatalf("a pinned reference asked the registry: %+v", f.called())
	}
}

func TestPlanDeploymentRefusesAChangeMidResolve(t *testing.T) {
	services := []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}}
	st, a, app, _, _ := pullFixture(t, services, map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}})
	ctx := context.Background()
	ts := st.Tenancy()
	f := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1.2": {digest: digestOf("f")}}}
	f.during = func() {
		if _, err := ts.AppendApplicationRevision(ctx, a, app.ID, 2, ApplicationSpec{Kind: "compose.v1", Services: services}); err != nil {
			t.Error(err)
		}
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "web"), f, imageCheckKey, false); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatalf("plan over a new revision: %v", err)
	}
	if len(f.called()) != 1 || deploymentRows(t, st, app.ID) != 0 {
		t.Fatalf("calls %+v, rows %d", f.called(), deploymentRows(t, st, app.ID))
	}
}

// The plan builds the real frame, secret values included, measures it against the endpoint's
// cap and drops it: at the legacy cap it is refused, at 320 KiB the stored plan holds no value.
func TestPlanDeploymentMeasuresTheFrame(t *testing.T) {
	st, a, app, _, _, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	env := map[string]ApplicationSecretRef{}
	for _, n := range []string{"A", "B", "C", "D"} {
		env["V"+n] = ApplicationSecretRef{SecretRef: "web-a"}
	}
	value := "frame-canary" + strings.Repeat("<", 9988)
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Environment: env}}}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, map[string]string{"web-a": value}, imageCheckKey); err != nil {
		t.Fatal(err)
	}
	m, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	legacy := planRequest(m)
	legacy.MaxFrameBytes = protocol.MaxDeploymentRequestBytesLegacy
	if _, err := ts.PlanDeployment(ctx, a, app.ID, legacy, nil, imageCheckKey, false); !isBlocked(err, "frame_too_large") {
		t.Fatalf("legacy cap: %v", err)
	}
	if n := deploymentRows(t, st, app.ID); n != 0 {
		t.Fatalf("a refused plan left %d rows", n)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, nil, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a plan without a key: %v", err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, make([]byte, 32), false); !errors.Is(err, ErrRevisionCorrupt) {
		t.Fatalf("a plan under the wrong key: %v", err)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := st.db.QueryRow(st.rebind(`SELECT plan FROM deployments WHERE id=?`), d.ID).Scan(&stored); err != nil || strings.Contains(stored, "frame-canary") {
		t.Fatalf("stored plan: %v", err)
	}
}

// An update plan decrypts the registry credential into the frame it measures and stores none of
// it. The proof it measured the credential: a cap the frame fits without it refuses the plan.
func TestPlanDeploymentDropsTheCredentialItMeasured(t *testing.T) {
	st, a, app, _, _ := pullFixture(t, []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}}, map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}})
	ctx := context.Background()
	ts := st.Tenancy()
	f := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1.2": {digest: digestOf("f")}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "web"), f, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := (&tenancyStore{store: st}).buildDeploymentFrame(ctx, tx, a, d, imageCheckKey, time.Now().UTC())
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	with, _ := json.Marshal(req)
	req.Registries = nil
	without, _ := json.Marshal(req)
	if !strings.Contains(string(with), pullCanary) || strings.Contains(string(without), pullCanary) || len(with)-len(without) < 60 {
		t.Fatalf("the built frame does not carry the credential: %d vs %d bytes", len(with), len(without))
	}
	// Timestamps vary by a few bytes between builds; the credential is well past that margin.
	r := pullPlanRequest(t, ts, a, app, "web")
	r.MaxFrameBytes = len(without) + 30
	if _, err := ts.PlanDeployment(ctx, a, app.ID, r, f, imageCheckKey, false); !isBlocked(err, "frame_too_large") {
		t.Fatalf("a cap that fits only the frame without its credential: %v", err)
	}
	var stored string
	if err := st.db.QueryRow(st.rebind(`SELECT plan FROM deployments WHERE id=?`), d.ID).Scan(&stored); err != nil || strings.Contains(stored, pullCanary) {
		t.Fatalf("stored plan: %v", err)
	}
	var leaked int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE resource LIKE ? OR details LIKE ?`), "%"+pullCanary+"%", "%"+pullCanary+"%").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("credential in audit: %d %v", leaked, err)
	}
}

// frameBlocker names the first thing that stops a frame: credentials, validity, then size.
func TestFrameBlocker(t *testing.T) {
	now := time.Now()
	env := map[string]string{}
	for _, n := range []string{"A", "B", "C", "D"} {
		env["V"+n] = strings.Repeat("<", 10000)
	}
	good := protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", Revision: 1, IssuedAt: now, Deadline: now.Add(DeploymentApplyDeadline), Services: []protocol.DeploymentService{{
		Name: "web", ContainerName: "shop-web", ImageID: "sha256:" + strings.Repeat("a", 64), Mounts: []protocol.Mount{}, Env: env,
		Replaces: protocol.InspectionTarget{ContainerID: strings.Repeat("b", 64), ImageID: "sha256:" + strings.Repeat("c", 64), CreatedUnix: 1700000000},
	}}}
	if b := frameBlocker(good, now, protocol.MaxDeploymentRequestBytes); b != "" {
		t.Fatalf("a 240 KiB frame at 320 KiB: %s", b)
	}
	if b := frameBlocker(good, now, protocol.MaxDeploymentRequestBytesLegacy); b != "frame_too_large" {
		t.Fatalf("a 240 KiB frame at 192 KiB: %s", b)
	}
	if b := frameBlocker(good, now, 1<<30); b != "" {
		t.Fatalf("a cap above the wire bound: %s", b)
	}
	invalid := good
	invalid.Deadline = now.Add(-time.Second)
	if b := frameBlocker(invalid, now, protocol.MaxDeploymentRequestBytes); b != "frame_invalid" {
		t.Fatalf("invalid: %s", b)
	}
	crowded := good
	crowded.Registries = map[string]protocol.RegistryAuth{}
	for i := range protocol.MaxRegistryAuthHosts + 1 {
		crowded.Registries[fmt.Sprintf("r%d.example.com", i)] = protocol.RegistryAuth{Username: "u", Secret: "s"}
	}
	if b := frameBlocker(crowded, now, protocol.MaxDeploymentRequestBytes); b != "too_many_registry_hosts" {
		t.Fatalf("17 hosts: %s", b)
	}
}

// A plan the endpoint's agent could not run is refused at plan time, not at apply.
func TestPlanDeploymentRequiresAgentCapabilities(t *testing.T) {
	st, a, app, endpoint, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for name, tc := range map[string]struct {
		capabilities []string
		want         []string
	}{
		"none":       {nil, []string{"agent_deploy_unsupported", "agent_inspect_unsupported"}},
		"no deploy":  {[]string{protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict}, []string{"agent_deploy_unsupported"}},
		"no inspect": {[]string{protocol.CapabilityDeploymentApply}, []string{"agent_inspect_unsupported"}},
		"no verdict": {[]string{protocol.CapabilityContainerInspect, protocol.CapabilityDeploymentApply}, []string{"agent_inspect_unsupported"}},
	} {
		if err := ts.SetEndpointCapabilities(ctx, endpoint, tc.capabilities); err != nil {
			t.Fatal(err)
		}
		var blocked *PreflightBlockedError
		if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, tc.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := ts.SetEndpointCapabilities(ctx, endpoint, []string{protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict, protocol.CapabilityDeploymentApply}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
}

// Each mapped service needs a live inspection of the container the preflight names, and that
// inspection must find nothing a recreate would drop. The refusal names the service.
func TestPlanDeploymentChecksTheLiveInspection(t *testing.T) {
	st, a, app, endpoint, snapshot, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	c := snapshot.Containers[0]
	target := protocol.InspectionTarget{ContainerID: c.ID, ImageID: c.ImageID, CreatedUnix: c.CreatedAt.Unix()}
	moved := target
	moved.CreatedUnix++
	for name, tc := range map[string]struct {
		inspections map[string]protocol.ContainerInspection
		want        string
		unsupported []string
	}{
		"none":             {map[string]protocol.ContainerInspection{}, "inspection_unavailable", nil},
		"identity changed": {map[string]protocol.ContainerInspection{c.ID: {Target: moved, ConfigurationVerified: true, Unsupported: []string{}}}, "replacement_identity_changed", nil},
		"unsupported":      {map[string]protocol.ContainerInspection{c.ID: {Target: target, Unsupported: []string{"privileged", "devices"}}}, "configuration_unsupported", []string{"privileged", "devices"}},
	} {
		r := planRequest(m)
		r.Inspections = tc.inspections
		_, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, imageCheckKey, false)
		var blocked *PreflightBlockedError
		if !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{tc.want}) || len(blocked.Services) != 1 {
			t.Fatalf("%s: %v", name, err)
		}
		if s := blocked.Services[0]; s.Name != "web" || !slices.Equal(s.Blockers, []string{tc.want}) || !slices.Equal(s.Unsupported, tc.unsupported) {
			t.Fatalf("%s: service %+v", name, s)
		}
	}
	if n := deploymentRows(t, st, app.ID); n != 0 {
		t.Fatalf("a refused plan left %d rows", n)
	}
	// Without container.inspect there was nothing to ask: only the capability is named.
	if err := ts.SetEndpointCapabilities(ctx, endpoint, []string{protocol.CapabilityDeploymentApply}); err != nil {
		t.Fatal(err)
	}
	r := planRequest(m)
	r.Inspections = nil
	var blocked *PreflightBlockedError
	if _, err := ts.PlanDeployment(ctx, a, app.ID, r, nil, imageCheckKey, false); !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{"agent_inspect_unsupported"}) || len(blocked.Services) != 0 {
		t.Fatalf("no capability: %v", err)
	}
	if err := ts.SetEndpointCapabilities(ctx, endpoint, []string{protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict, protocol.CapabilityDeploymentApply}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
}

// An update that pulls needs deployment.pull; a plan with no pull does not.
func TestPlanDeploymentRefusesAPullTheAgentCannotRun(t *testing.T) {
	st, a, app, endpoint, _ := pullFixture(t, []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1.2"}}, map[string][]string{"web": {"ghcr.io/org/web@" + digestOf("a")}})
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.SetEndpointCapabilities(ctx, endpoint, []string{protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict, protocol.CapabilityDeploymentApply}); err != nil {
		t.Fatal(err)
	}
	f := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1.2": {digest: digestOf("f")}}}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app, "web"), f, imageCheckKey, false); !isBlocked(err, "agent_pull_unsupported") {
		t.Fatalf("pull without deployment.pull: %v", err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pullPlanRequest(t, ts, a, app), nil, imageCheckKey, false); err != nil {
		t.Fatalf("a plan without a pull: %v", err)
	}
}
