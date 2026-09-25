package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// policyCaps is an agent that inspects with verdicts, applies and pulls.
var policyCaps = []string{protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull, protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict}

// tomorrow is the next UTC midnight: the fixture policy's windows (10:00-11:00 UTC) from there on
// lie after the policy was saved, so a window the test clock has passed counts as missed.
func tomorrow() time.Time { return time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour) }

func (h planHost) appID() string {
	return strings.TrimSuffix(strings.TrimPrefix(h.deployments, "/api/organizations/a/environments/env-a/applications/"), "/deployments")
}

func (h planHost) as(user string) store.TenantAccess {
	return store.TenantAccess{ActorID: user, OrganizationID: "a", EnvironmentID: "env-a", CorrelationID: "policy-test"}
}

// policy saves a 10:00-11:00 UTC policy as the planner for every day but today (UTC): the
// windows from tomorrow on lie after the save, and today's, which may not have ended yet, can
// never be recorded as missed whatever the hour the test runs.
func (h planHost) policy(t *testing.T, mode string) *store.UpdatePolicy {
	t.Helper()
	var days []int
	for d := range 7 {
		if d != int(time.Now().UTC().Weekday()) {
			days = append(days, d)
		}
	}
	p, _, err := h.st.Tenancy().PutUpdatePolicy(context.Background(), h.as("usr_planner"), h.appID(), store.PolicyInput{Mode: mode, Timezone: "UTC", Weekdays: days, StartMinute: 600, EndMinute: 660})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// updatable gives the host's image a registry digest, turns anonymous pulls on and answers every
// registry lookup with a newer digest, so a check finds each service update_available.
func (h planHost) updatable(t *testing.T) *fakeDigests {
	t.Helper()
	snap := h.snapshot
	snap.Images = []protocol.Image{{ID: h.snapshot.Images[0].ID, Tags: []string{"nginx:1"}, Digests: []string{"nginx@sha256:" + strings.Repeat("c", 64)}}}
	raw, _ := json.Marshal(snap)
	if _, err := h.st.Tenancy().AcceptInventory(context.Background(), h.ag.id, uint64(time.Now().Unix())+1, time.Now(), raw); err != nil {
		t.Fatal(err)
	}
	h.do(t, "PUT", "/api/organizations/a/registry-policy", `{"anonymous_pull_enabled":true}`, 204)
	fake := &fakeDigests{digest: "sha256:" + strings.Repeat("d", 64)}
	api.SetDigestResolverForTest(h.s, fake)
	return fake
}

// tick runs one scheduler tick at now and waits for the runs it started.
func (h planHost) tick(now time.Time) {
	api.PolicyTickForTest(h.s, now)
	api.WaitPolicyRunsForTest(h.s)
}

func (h planHost) runs(t *testing.T, user string) []store.PolicyRun {
	t.Helper()
	runs, err := h.st.Tenancy().ListPolicyRuns(context.Background(), h.as(user), h.appID(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func policyAuditRows(t *testing.T, h planHost, cookie *http.Cookie) []store.AuditRecord {
	t.Helper()
	w := tenantRequest(h.s, cookie, "GET", "/api/organizations/a/audit?limit=200", "", false)
	var rows []store.AuditRecord
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
		t.Fatalf("audit: %d %s", w.Code, w.Body.String())
	}
	return rows
}

// secondAdmin is another organization administrator, who can still read once the planner cannot.
func (h planHost) secondAdmin(t *testing.T) *http.Cookie {
	t.Helper()
	cookie := loginAs(t, h.s, h.st, "other", "user")
	if err := h.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_other", Role: store.RoleOrganizationAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	return cookie
}

// Inside the window with an update available, the run checks, plans and applies as the policy's
// creator, sends the frame, and every row of it carries one correlation ID.
func TestPolicyRunPlansAndAppliesAsItsCreator(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.updatable(t)
	sock, ctx := h.online(t, policyCaps)
	p := h.policy(t, store.PolicyModeApply)
	day := tomorrow()
	h.tick(day.Add(10*time.Hour + 30*time.Minute))

	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunApplied || runs[0].DeploymentID == "" || runs[0].Detail != "" || !runs[0].Occurrence.Equal(day.Add(10*time.Hour)) || runs[0].FinishedAt == nil {
		t.Fatalf("runs: %+v", runs)
	}
	run := runs[0]
	frame := readEnvelope(t, ctx, sock.conn)
	var req protocol.DeploymentRequest
	if frame.Type != protocol.TypeDeploymentApply || json.Unmarshal(frame.Payload, &req) != nil || req.Deployment != run.DeploymentID || req.RequestID != run.CorrelationID || len(req.Services) != 1 || req.Services[0].Pull == nil || req.Services[0].Pull.Digest != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("frame: %s %+v", frame.Type, req)
	}
	var d store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", h.deployments+"/"+run.DeploymentID, "", 200)), &d); err != nil {
		t.Fatal(err)
	}
	if d.State != "applying" || d.CreatedBy != "usr_planner" || d.AppliedBy != "usr_planner" || d.CorrelationID != run.CorrelationID {
		t.Fatalf("deployment: %+v", d)
	}
	want := map[string]string{
		"application.deploy " + h.appID() + "/updates":                                  "usr_planner",
		"application.deploy " + h.appID() + "/deployments/" + d.ID:                      "usr_planner",
		"application.deploy " + h.appID() + "/deployments/" + d.ID + "/apply":           "usr_planner",
		"application.policy.run " + h.appID() + "/policies/" + p.ID + "/runs/" + run.ID: "system",
	}
	for _, r := range policyAuditRows(t, h, h.admin) {
		if r.CorrelationID != run.CorrelationID {
			continue
		}
		key := r.Action + " " + r.Resource
		if want[key] != r.UserID || r.Result != "success" {
			t.Fatalf("unexpected row under the run's correlation: %+v", r)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing rows: %v", want)
	}
	// The window has its run: another tick inside it starts nothing.
	h.tick(day.Add(10*time.Hour + 45*time.Minute))
	if runs := h.runs(t, "usr_planner"); len(runs) != 1 {
		t.Fatalf("a second run in one window: %+v", runs)
	}
}

// plan_only stops at a plan that waits for a click like any manual plan.
func TestPolicyPlanOnlyStopsAtPlanned(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.updatable(t)
	h.policy(t, store.PolicyModePlanOnly)
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunPlanned || runs[0].DeploymentID == "" {
		t.Fatalf("runs: %+v", runs)
	}
	var d store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", h.deployments+"/"+runs[0].DeploymentID, "", 200)), &d); err != nil {
		t.Fatal(err)
	}
	if d.State != "planned" || d.CreatedBy != "usr_planner" || d.CorrelationID != runs[0].CorrelationID || d.Plan.Services[0].PullDigest != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("plan: %+v", d)
	}
}

// Nothing newer (the host image has no registry digest: unknown_local) is no_update, no plan.
func TestPolicyRunWithNoUpdate(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	h.policy(t, store.PolicyModeApply)
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunNoUpdate || runs[0].DeploymentID != "" || runs[0].Detail != "" {
		t.Fatalf("runs: %+v", runs)
	}
	if body := h.do(t, "GET", h.deployments, "", 200); !strings.HasPrefix(body, "[]") {
		t.Fatalf("deployments: %s", body)
	}
}

// A plan-time blocker is the run's outcome, the blocker codes its detail.
func TestPolicyRunBlocked(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, func(_ context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
		in := verifiedObservation(target)
		in.ConfigurationVerified, in.Unsupported = false, []string{"privileged"}
		return in, nil
	})
	h.updatable(t)
	h.policy(t, store.PolicyModeApply)
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunBlocked || runs[0].Detail != "configuration_unsupported" || runs[0].DeploymentID != "" {
		t.Fatalf("runs: %+v", runs)
	}
}

// A creator who lost application.deploy pauses the policy without counting, with its audit rows.
func TestPolicyPausesWhenTheCreatorLosesTheRole(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	other := h.secondAdmin(t)
	p := h.policy(t, store.PolicyModeApply)
	if err := h.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_planner", Role: store.RoleOperator, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	day := tomorrow()
	h.tick(day.Add(10*time.Hour + 30*time.Minute))
	assertPausedByCreator(t, h, other, p)
	// Paused: the next window runs nothing.
	h.tick(day.Add(34*time.Hour + 30*time.Minute))
	if runs := h.runs(t, "usr_other"); len(runs) != 1 {
		t.Fatalf("a paused policy ran: %+v", runs)
	}
}

// The creator's account deleted outright pauses the same way (Review Focus 5).
func TestPolicyPausesWhenTheCreatorIsDeleted(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	other := h.secondAdmin(t)
	p := h.policy(t, store.PolicyModeApply)
	if err := h.st.Users().DeleteUser(context.Background(), "usr_planner"); err != nil {
		t.Fatal(err)
	}
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	assertPausedByCreator(t, h, other, p)
}

func assertPausedByCreator(t *testing.T, h planHost, other *http.Cookie, p *store.UpdatePolicy) {
	t.Helper()
	runs := h.runs(t, "usr_other")
	if len(runs) != 1 || runs[0].Outcome != store.RunPaused || runs[0].Detail != store.PolicyDetailCreatorLost || runs[0].DeploymentID != "" {
		t.Fatalf("runs: %+v", runs)
	}
	got, _, err := h.st.Tenancy().ReadUpdatePolicy(context.Background(), h.as("usr_other"), h.appID())
	if err != nil || got.Status != store.PolicyPaused || got.PausedReason != store.PolicyDetailCreatorLost || got.ConsecutiveFailures != 0 {
		t.Fatalf("policy: %+v %v", got, err)
	}
	var run, paused bool
	for _, r := range policyAuditRows(t, h, other) {
		if r.CorrelationID != runs[0].CorrelationID || r.UserID != "system" {
			continue
		}
		switch {
		case r.Action == store.AuditPolicyRun && r.Result == "denied" && r.Details == store.RunPaused:
			run = true
		case r.Action == store.AuditPolicyPaused && r.Result == "denied" && r.Resource == h.appID()+"/policies/"+p.ID:
			paused = true
		}
	}
	if !run || !paused {
		t.Fatalf("audit rows: run=%v paused=%v", run, paused)
	}
	if body := tenantRequest(h.s, other, "GET", h.deployments, "", false).Body.String(); !strings.HasPrefix(body, "[]") {
		t.Fatalf("a deployment was made: %s", body)
	}
}

// A window that ended while nothing ran is recorded once as missed; it is not a failure.
func TestPolicyRecordsAMissedWindow(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	h.policy(t, store.PolicyModeApply)
	day := tomorrow()
	for range 2 {
		h.tick(day.Add(11*time.Hour + 5*time.Minute))
	}
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunSkippedMissed || runs[0].Detail != store.PolicyDetailMissed || !runs[0].Occurrence.Equal(day.Add(10*time.Hour)) || runs[0].FinishedAt == nil {
		t.Fatalf("runs: %+v", runs)
	}
	if got, _, _ := h.st.Tenancy().ReadUpdatePolicy(context.Background(), h.as("usr_planner"), h.appID()); got.ConsecutiveFailures != 0 {
		t.Fatalf("a missed window counted: %+v", got)
	}
}

// A live plan holds the endpoint: no run while the window is open, skipped_busy once it closes.
func TestPolicySkipsABusyEndpoint(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.updatable(t)
	h.policy(t, store.PolicyModeApply)
	h.do(t, "POST", h.deployments, h.planBody, 201)
	day := tomorrow()
	h.tick(day.Add(10*time.Hour + 30*time.Minute))
	h.tick(day.Add(10*time.Hour + 40*time.Minute))
	if runs := h.runs(t, "usr_planner"); len(runs) != 0 {
		t.Fatalf("ran beside a live plan: %+v", runs)
	}
	h.tick(day.Add(11*time.Hour + time.Minute))
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunSkippedBusy || runs[0].Detail != store.PolicyDetailBusy {
		t.Fatalf("runs: %+v", runs)
	}
}

// Three failed windows in a row pause the policy, with its audit row; the fourth window runs nothing.
func TestPolicyPausesAfterThreeFailedWindows(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	fake := h.updatable(t)
	fake.err = registry.ErrUnauthorized
	p := h.policy(t, store.PolicyModeApply)
	day := tomorrow()
	for i := range 4 {
		h.tick(day.AddDate(0, 0, i).Add(10*time.Hour + 30*time.Minute))
	}
	runs := h.runs(t, "usr_planner")
	if len(runs) != 3 {
		t.Fatalf("runs: %+v", runs)
	}
	for _, r := range runs {
		if r.Outcome != store.RunFailed || r.Detail != "unauthorized" {
			t.Fatalf("run: %+v", r)
		}
	}
	got, _, err := h.st.Tenancy().ReadUpdatePolicy(context.Background(), h.as("usr_planner"), h.appID())
	if err != nil || got.Status != store.PolicyPaused || got.PausedReason != store.PolicyReasonFailures || got.ConsecutiveFailures != 3 {
		t.Fatalf("policy: %+v %v", got, err)
	}
	var paused bool
	for _, r := range policyAuditRows(t, h, h.admin) {
		if r.Action == store.AuditPolicyPaused && r.Result == "failure" && r.UserID == "system" && r.Resource == h.appID()+"/policies/"+p.ID && r.CorrelationID == runs[0].CorrelationID {
			paused = true
		}
	}
	if !paused {
		t.Fatal("no application.policy.paused row under the third run's correlation")
	}
}

// An application released between the tick and the run fails the window and counts; it does
// not plan against nothing (Review Focus 1).
func TestPolicyRunOnAReleasedApplicationFails(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	h.policy(t, store.PolicyModeApply)
	base := strings.TrimSuffix(h.deployments, "/deployments")
	var m store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", base+"/mapping", "", 200)), &m); err != nil {
		t.Fatal(err)
	}
	h.do(t, "DELETE", base+"/adoption", `{"instance_id":"`+m.InstanceID+`","confirm":"shop"}`, 204)
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunFailed || runs[0].Detail != "not_adopted" || runs[0].DeploymentID != "" {
		t.Fatalf("runs: %+v", runs)
	}
	if got, _, _ := h.st.Tenancy().ReadUpdatePolicy(context.Background(), h.as("usr_planner"), h.appID()); got.ConsecutiveFailures != 1 || got.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", got)
	}
}

// Ticks never overlap: four at once on one open window make one run and one plan (Review Focus 3).
func TestPolicyTicksNeverOverlap(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.updatable(t)
	h.policy(t, store.PolicyModePlanOnly)
	now := tomorrow().Add(10*time.Hour + 30*time.Minute)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); api.PolicyTickForTest(h.s, now) }()
	}
	wg.Wait()
	api.WaitPolicyRunsForTest(h.s)
	runs := h.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunPlanned {
		t.Fatalf("runs: %+v", runs)
	}
	var list []store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", h.deployments, "", 200)), &list); err != nil || len(list) != 1 {
		t.Fatalf("deployments: %+v %v", list, err)
	}
}

// done closes only once the run in flight has finished and been recorded.
func TestRunPoliciesClosesDoneOnlyAfterTheRunInFlight(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	fake := h.updatable(t)
	fake.gate, fake.entered = make(chan struct{}), make(chan struct{}, 4)
	h.policy(t, store.PolicyModePlanOnly)
	now := tomorrow().Add(10*time.Hour + 30*time.Minute)
	api.SetPolicyClockForTest(h.s, 10*time.Millisecond, func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go h.s.RunPolicies(ctx, done)
	select {
	case <-fake.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no run reached the registry")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("done closed with a run in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(fake.gate)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("done never closed")
	}
	if runs := h.runs(t, "usr_planner"); len(runs) != 1 || runs[0].Outcome != store.RunPlanned {
		t.Fatalf("the run was not recorded before done: %+v", runs)
	}
}

// A window passed over as busy until it closed is skipped_busy even when the next tick comes
// after a later window has ended too: that one is the missed window, not the busy one.
func TestPolicyRemembersABusyWindowAcrossAGap(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	h.updatable(t)
	h.policy(t, store.PolicyModeApply)
	h.do(t, "POST", h.deployments, h.planBody, 201)
	day := tomorrow()
	h.tick(day.Add(10*time.Hour + 30*time.Minute))
	h.tick(day.Add(35*time.Hour + 5*time.Minute))
	got := map[time.Time]string{}
	for _, r := range h.runs(t, "usr_planner") {
		got[r.Occurrence.UTC()] = r.Outcome
	}
	want := map[time.Time]string{day.Add(10 * time.Hour): store.RunSkippedBusy, day.Add(34 * time.Hour): store.RunSkippedMissed}
	if len(got) != len(want) || got[day.Add(10*time.Hour)] != want[day.Add(10*time.Hour)] || got[day.Add(34*time.Hour)] != want[day.Add(34*time.Hour)] {
		t.Fatalf("runs: %v", got)
	}
}
