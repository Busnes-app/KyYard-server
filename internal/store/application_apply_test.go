package store

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func applyFixture(t *testing.T) (*SQLStore, TenantAccess, *Application, string, protocol.Snapshot, *ApplicationMapping, *Deployment, []byte) {
	t.Helper()
	st, a, app, endpoint, snapshot, _ := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	key := make([]byte, 32)
	// A revision with a secret so apply has something to resolve.
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Restart: "always", Ports: []ApplicationPort{{Target: 80, Published: 8080, Protocol: "tcp"}}, Environment: map[string]ApplicationSecretRef{"TOKEN": {SecretRef: "web-token"}}}}}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, map[string]string{"web-token": "apply-secret-canary"}, key); err != nil {
		t.Fatal(err)
	}
	m2, _ := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m2)); err != nil {
		t.Fatal(err)
	}
	m2, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m2))
	if err != nil {
		t.Fatal(err)
	}
	return st, a, app, endpoint, snapshot, m2, d, key
}

func TestApplyDeploymentBuildsTheRequestAndMovesToApplying(t *testing.T) {
	st, a, app, endpoint, snapshot, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	applied, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key)
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != "applying" || applied.AppliedBy != a.ActorID || applied.AppliedAt == nil || applied.Deadline == nil || time.Until(*applied.Deadline) > DeploymentApplyDeadline {
		t.Fatalf("row: %+v", applied)
	}
	c := snapshot.Containers[0]
	if req.Deployment != d.ID || req.Endpoint != endpoint || req.Project != "shop" || req.Revision != 2 || len(req.Services) != 1 {
		t.Fatalf("request: %+v", req)
	}
	s := req.Services[0]
	if s.Name != "web" || s.ContainerName != c.Name || s.ImageID != d.Plan.Services[0].ImageID || s.Replaces.ContainerID != c.ID || s.Restart != "always" || len(s.Ports) != 1 || s.Ports[0].Host != 8080 || s.Ports[0].Container != 80 || s.Env["TOKEN"] != "apply-secret-canary" {
		t.Fatalf("service: %+v", s)
	}
	if err := req.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	// The row and the audit trail carry no value.
	var stored string
	if err := st.db.QueryRow(st.rebind(`SELECT plan||detail||result FROM deployments WHERE id=?`), d.ID).Scan(&stored); err != nil || strings.Contains(stored, "canary") {
		t.Fatal("secret persisted on the row")
	}
	var leaked int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_records WHERE resource LIKE '%canary%' OR details LIKE '%canary%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("secret in audit: %d %v", leaked, err)
	}
	// Apply twice: the CAS lets one through.
	if _, _, err = ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatalf("second apply: %v", err)
	}
}

func TestApplyDeploymentPreconditions(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		mutate func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, endpoint string, d *Deployment)
		want   error
	}{
		"wrong confirm": {func(*testing.T, *SQLStore, TenantAccess, *Application, string, *Deployment) {}, ErrAdoptionChanged},
		"expired": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, d *Deployment) {
			if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=? WHERE id=?`), time.Now().Add(-time.Minute), d.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"head moved": {func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, _ string, _ *Deployment) {
			if _, err := st.Tenancy().AppendApplicationRevision(ctx, a, app.ID, 2, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}); err != nil {
				t.Fatal(err)
			}
		}, ErrAdoptionChanged},
		"endpoint offline": {func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, endpoint string, _ *Deployment) {
			if _, err := st.db.Exec(st.rebind(`UPDATE endpoints SET state='offline' WHERE id=?`), endpoint); err != nil {
				t.Fatal(err)
			}
		}, ErrEndpointOffline},
	} {
		t.Run(name, func(t *testing.T) {
			st, a, app, endpoint, _, _, d, key := applyFixture(t)
			tc.mutate(t, st, a, app, endpoint, d)
			confirm := "shop"
			if name == "wrong confirm" {
				confirm = "nope"
			}
			if _, _, err := st.Tenancy().ApplyDeployment(ctx, a, app.ID, d.ID, confirm, key); !errors.Is(err, tc.want) {
				t.Fatalf("%s: %v", name, err)
			}
			var state string
			if err := st.db.QueryRow(st.rebind(`SELECT state FROM deployments WHERE id=?`), d.ID).Scan(&state); err != nil || state != "planned" {
				t.Fatalf("%s left state %s", name, state)
			}
		})
	}
	// Roles: operator is refused, developer allowed.
	st, a, app, _, _, _, d, key := applyFixture(t)
	if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleOperator, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator: %v", err)
	}
	if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleDeveloper, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatalf("developer: %v", err)
	}
}

func settledResult(d *Deployment, outcome string, newID string) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: d.ID, Outcome: outcome, Steps: []protocol.DeploymentStep{{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}}, Services: []protocol.DeploymentIdentity{}}
	if newID != "" {
		res.Services = append(res.Services, protocol.DeploymentIdentity{Service: "web", ContainerID: newID, ImageID: d.Plan.Services[0].ImageID, CreatedUnix: 1800000000})
	}
	if outcome != protocol.OutcomeSucceeded {
		res.Steps[0].Outcome = outcome
		res.Steps[0].Detail = "fixed text"
	}
	return res
}

func TestSettleDeploymentRebindsAndAdvances(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	newID := strings.Repeat("e", 64)
	// A result from another endpoint changes nothing.
	if err := ts.SettleDeployment(ctx, "other-endpoint", settledResult(d, protocol.OutcomeSucceeded, newID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign endpoint: %v", err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, newID)); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.State != "succeeded" || got.SettledAt == nil || got.Result == nil || len(got.Result.Services) != 1 {
		t.Fatalf("settled: %+v %v", got, err)
	}
	instances, err := ts.ListApplicationInstances(ctx, a, endpoint)
	if err != nil || len(instances) != 1 || instances[0].CurrentRevision != 2 || instances[0].PreviousRevision != 0 {
		t.Fatalf("revision: %+v %v", instances, err)
	}
	var container, service, name string
	if err := st.db.QueryRow(st.rebind(`SELECT container_id,service_name,name FROM application_resources WHERE instance_id=?`), m.InstanceID).Scan(&container, &service, &name); err != nil || container != newID || service != "web" || name != "shop-web" {
		t.Fatalf("rebind: %s %s %s %v", container, service, name, err)
	}
	// First answer wins on a settled row.
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second settle: %v", err)
	}
	var audits int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE action=? AND resource=? AND user_id=? AND result='success' AND details LIKE '%succeeded%'`), "application.deploy", app.ID+"/deployments/"+d.ID, "agent:"+endpoint).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("settle audit: %d %v", audits, err)
	}
}

func TestSettleDeploymentFromUnknownAndAbandon(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	n, err := ts.AbandonDeployments(ctx, endpoint)
	if err != nil || n != 1 {
		t.Fatalf("abandon: %d %v", n, err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "unknown" || got.Detail == "" {
		t.Fatalf("abandoned: %+v", got)
	}
	// A late result still settles an unknown row, but a failed run does not advance the revision.
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); err != nil {
		t.Fatal(err)
	}
	got, _ = ts.ReadDeployment(ctx, a, app.ID, d.ID)
	instances, _ := ts.ListApplicationInstances(ctx, a, endpoint)
	if got.State != "failed" || instances[0].CurrentRevision != 0 {
		t.Fatalf("late settle: %+v %+v", got, instances)
	}
}

func TestPruneSweepsStaleApplying(t *testing.T) {
	st, a, app, _, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET deadline=? WHERE id=?`), time.Now().Add(-3*time.Minute), d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "unknown" {
		t.Fatalf("sweep: %+v", got)
	}
}

func TestSettleRebindRefusesMissingResource(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`DELETE FROM application_resources WHERE instance_id=?`), m.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatalf("rebind without resource: %v", err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "applying" {
		t.Fatalf("half-written: %+v", got)
	}
}

func TestLateResultAfterNewerPlanIsIgnored(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m)); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late settle after a newer plan: %v", err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	instances, _ := ts.ListApplicationInstances(ctx, a, endpoint)
	var container string
	if err := st.db.QueryRow(st.rebind(`SELECT container_id FROM application_resources WHERE instance_id=?`), m.InstanceID).Scan(&container); err != nil || container != d.Plan.Services[0].ContainerID {
		t.Fatalf("resources moved: %s %v", container, err)
	}
	if got.State != "unknown" || instances[0].CurrentRevision != 0 {
		t.Fatalf("late settle applied: %+v %+v", got, instances)
	}
}

func TestLateResultAfterReleaseIsIgnored(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AbandonDeployments(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop"); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late settle after release: %v", err)
	}
	var resources int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM application_resources WHERE instance_id=?`), m.InstanceID).Scan(&resources); err != nil || resources != 0 {
		t.Fatalf("late settle resurrected resources: %d %v", resources, err)
	}
}

func TestSettleDeploymentRefusesIncompleteOrWrongIdentities(t *testing.T) {
	for name, mutate := range map[string]func(*protocol.DeploymentResult){
		"succeeded without every service": func(r *protocol.DeploymentResult) { r.Services = r.Services[:0] },
		"unpinned image":                  func(r *protocol.DeploymentResult) { r.Services[0].ImageID = "sha256:" + strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			st, a, app, endpoint, _, m, d, key := applyFixture(t)
			ctx := context.Background()
			ts := st.Tenancy()
			if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
				t.Fatal(err)
			}
			res := settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))
			mutate(&res)
			if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s: %v", name, err)
			}
			got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
			var container string
			if err := st.db.QueryRow(st.rebind(`SELECT container_id FROM application_resources WHERE instance_id=?`), m.InstanceID).Scan(&container); err != nil || container != d.Plan.Services[0].ContainerID || got.State != "applying" {
				t.Fatalf("%s wrote: %s %+v %v", name, container, got, err)
			}
		})
	}
}

func TestConcurrentSettlesCommitOnce(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	if st.driver != "postgres" {
		t.Skip("concurrent settles need real row locks")
	}
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	// A success must name every planned service, so it carries the identity; the failure none.
	results := []protocol.DeploymentResult{settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64)), settledResult(d, protocol.OutcomeFailed, "")}
	// Hold the row so both settles are in flight and queued on it before either can commit;
	// otherwise the first usually finishes before the second starts and the race never runs.
	holder, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	var held string
	if err := holder.QueryRowContext(ctx, st.rebind(`SELECT id FROM deployments WHERE id=? FOR UPDATE`), d.ID).Scan(&held); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, res := range results {
		go func() { errs <- ts.SettleDeployment(ctx, endpoint, res) }()
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM deployments%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settles never queued on the row: %d waiting", waiting)
		}
		runtime.Gosched()
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	var ok, notFound int
	for range results {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrNotFound):
			notFound++
		default:
			t.Fatalf("settle: %v", err)
		}
	}
	if ok != 1 || notFound != 1 {
		t.Fatalf("settles: %d committed, %d refused", ok, notFound)
	}
	var audits int
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM audit_records WHERE user_id=? AND resource=?`), "agent:"+endpoint, app.ID+"/deployments/"+d.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit rows: %d %v", audits, err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	instances, _ := ts.ListApplicationInstances(ctx, a, endpoint)
	want := 0
	if got.State == "succeeded" {
		want = 2
	}
	if instances[0].CurrentRevision != want || instances[0].PreviousRevision != 0 {
		t.Fatalf("state %s moved revisions: %+v", got.State, instances[0])
	}
}

func TestSettleDeploymentRefusesOversizedResult(t *testing.T) {
	st, a, app, endpoint, _, _, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	res := settledResult(d, protocol.OutcomeFailed, "")
	// Each detail is 64 three-byte runes: 192 bytes, under the per-step cap, so the result
	// validates and only the stored byte cap refuses it.
	for len(res.Steps) < 8*protocol.MaxDeploymentServices {
		res.Steps = append(res.Steps, protocol.DeploymentStep{Service: "web", Step: protocol.StepStart, Outcome: protocol.OutcomeFailed, Detail: strings.Repeat("\u20ac", 64)})
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, res); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized result: %v", err)
	}
	got, _ := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if got.State != "applying" {
		t.Fatalf("oversized result settled: %+v", got)
	}
}

func TestPlanAndReleaseNeverLeaveALivePlanForAReleasedInstance(t *testing.T) {
	st, a, app, _, _, m := planFixture(t)
	if st.driver != "postgres" {
		t.Skip("interleaving needs real row locks")
	}
	ctx := context.Background()
	ts := st.Tenancy()
	var planFirst, releaseFirst int
	for round := 0; round < 40; round++ {
		// Re-adopt if the previous round released.
		if _, err := ts.ReadApplicationMapping(ctx, a, app.ID); errors.Is(err, ErrNotFound) {
			p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, m.Preview.EndpointID, "shop")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: m.Preview.EndpointID, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
				t.Fatal(err)
			}
			m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
			if err = ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
				t.Fatal(err)
			}
			m, _ = ts.ReadApplicationMapping(ctx, a, app.ID)
		}
		// Expire the previous round's plan so release can win this one.
		if _, err := st.db.Exec(st.rebind(`UPDATE deployments SET expires_at=? WHERE instance_id=? AND state='planned'`), time.Now().UTC().Add(-time.Minute), m.InstanceID); err != nil {
			t.Fatal(err)
		}
		var planErr, releaseErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(rand.IntN(2000)) * time.Microsecond)
			_, planErr = ts.PlanDeployment(ctx, a, app.ID, planRequest(m))
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(rand.IntN(2000)) * time.Microsecond)
			releaseErr = ts.ReleaseApplication(ctx, a, app.ID, m.InstanceID, "shop")
		}()
		wg.Wait()
		for _, err := range []error{planErr, releaseErr} {
			if err != nil && !errors.Is(err, ErrDeploymentPlanned) && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrAdoptionChanged) {
				t.Fatalf("round %d: unexpected error: %v", round, err)
			}
		}
		switch {
		case planErr == nil && releaseErr != nil:
			planFirst++
		case releaseErr == nil && planErr != nil:
			releaseFirst++
		default:
			t.Fatalf("round %d: plan %v, release %v", round, planErr, releaseErr)
		}
		var live int
		if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM deployments d WHERE d.state='planned' AND d.expires_at>? AND NOT EXISTS (SELECT 1 FROM application_instances i WHERE i.id=d.instance_id)`), time.Now().UTC()).Scan(&live); err != nil || live != 0 {
			t.Fatalf("round %d: live plan for a released instance: %d %v", round, live, err)
		}
	}
	t.Logf("plan first %d, release first %d", planFirst, releaseFirst)
	if planFirst == 0 || releaseFirst == 0 {
		t.Fatalf("one ordering never ran: plan first %d, release first %d", planFirst, releaseFirst)
	}
}
