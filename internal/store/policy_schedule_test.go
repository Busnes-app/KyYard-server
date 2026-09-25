package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

var everyDay = []int{0, 1, 2, 3, 4, 5, 6}

func windowPolicy(zone string, days []int, start, end int) UpdatePolicy {
	return UpdatePolicy{Timezone: zone, Weekdays: days, StartMinute: start, EndMinute: end, Status: PolicyActive}
}

// backdatePolicy moves the policy's last change, which bounds what the scheduler calls missed.
func backdatePolicy(t *testing.T, st *SQLStore, id string, when time.Time) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE update_policies SET updated_at=? WHERE id=?`), when, id); err != nil {
		t.Fatal(err)
	}
}

type policyAuditRow struct{ Action, Resource, Details, Result, User, Correlation string }

func policyAudits(t *testing.T, st *SQLStore) []policyAuditRow {
	t.Helper()
	rows, err := st.db.QueryContext(context.Background(), `SELECT action,resource,details,result,user_id,correlation_id FROM audit_records WHERE action IN ('application.policy.run','application.policy.paused') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []policyAuditRow
	for rows.Next() {
		var r policyAuditRow
		if err := rows.Scan(&r.Action, &r.Resource, &r.Details, &r.Result, &r.User, &r.Correlation); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// 2026-03-08 02:30 does not exist in New York (02:00 EST jumps to 03:00 EDT): that day has no
// occurrence, and the days either side do.
func TestPolicyOccurrenceAcrossADSTGap(t *testing.T) {
	p := windowPolicy("America/New_York", everyDay, 150, 240)
	if _, _, open := policyOccurrence(p, instant("2026-03-08T07:45:00Z")); open { // 03:45 EDT
		t.Fatal("an occurrence opened on a day its start does not exist")
	}
	start, end, ok := previousOccurrence(p, instant("2026-03-08T09:00:00Z"))
	if !ok || !start.Equal(instant("2026-03-07T07:30:00Z")) || !end.Equal(instant("2026-03-07T09:00:00Z")) {
		t.Fatalf("previous across the gap: %s %s %v", start, end, ok)
	}
	next, ok := nextOccurrence(p, instant("2026-03-08T05:00:00Z"))
	if !ok || !next.Equal(instant("2026-03-09T06:30:00Z")) {
		t.Fatalf("next across the gap: %s %v", next, ok)
	}
	start, end, open := policyOccurrence(p, instant("2026-03-09T07:00:00Z")) // 03:00 EDT
	if !open || !start.Equal(instant("2026-03-09T06:30:00Z")) || !end.Equal(instant("2026-03-09T08:00:00Z")) || start.Location() != time.UTC {
		t.Fatalf("the day after: %s %s %v", start, end, open)
	}
}

// 2026-11-01 01:30 happens twice in New York: the window opens once, at the first (EDT), and
// stays the same occurrence through the repeated hour.
func TestPolicyOccurrenceInADSTOverlap(t *testing.T) {
	p := windowPolicy("America/New_York", everyDay, 90, 120)
	for _, now := range []string{"2026-11-01T05:45:00Z", "2026-11-01T06:45:00Z"} { // 01:45 EDT, then 01:45 EST
		start, end, open := policyOccurrence(p, instant(now))
		if !open || !start.Equal(instant("2026-11-01T05:30:00Z")) || !end.Equal(instant("2026-11-01T07:00:00Z")) {
			t.Fatalf("%s: %s %s %v", now, start, end, open)
		}
	}
}

// Kiritimati is UTC+14: Thursday 12:00 UTC is Friday 02:00 there, so the weekday is the zone's.
func TestPolicyOccurrenceEastOfUTC(t *testing.T) {
	now := instant("2026-09-24T12:00:00Z")
	start, _, open := policyOccurrence(windowPolicy("Pacific/Kiritimati", []int{5}, 60, 180), now)
	if !open || !start.Equal(instant("2026-09-24T11:00:00Z")) {
		t.Fatalf("Friday in Kiritimati: %s %v", start, open)
	}
	if _, _, open := policyOccurrence(windowPolicy("Pacific/Kiritimati", []int{4}, 60, 180), now); open {
		t.Fatal("the UTC weekday opened the window")
	}
}

// A run starting at 23:59 in a window ending at midnight belongs to that day's occurrence; at
// 00:00 nothing is open and, with its run recorded, nothing is missed (Review Focus 4).
func TestPolicyWindowEndingAtMidnight(t *testing.T) {
	p := windowPolicy("UTC", []int{4}, 1380, 1440)
	start, end, open := policyOccurrence(p, instant("2026-09-24T23:59:00Z"))
	if !open || !start.Equal(instant("2026-09-24T23:00:00Z")) || !end.Equal(instant("2026-09-25T00:00:00Z")) {
		t.Fatalf("23:59: %s %s %v", start, end, open)
	}
	if _, _, open := policyOccurrence(p, instant("2026-09-25T00:00:00Z")); open {
		t.Fatal("open at midnight")
	}
	st, _, _, _, _, stored := policyFixture(t, PolicyInput{Mode: PolicyModeApply, Timezone: "UTC", Weekdays: []int{4}, StartMinute: 1380, EndMinute: 1440})
	backdatePolicy(t, st, stored.ID, instant("2026-09-01T00:00:00Z"))
	ctx := context.Background()
	ts := st.Tenancy()
	due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T23:59:00Z"))
	if err != nil || len(due) != 1 || !due[0].Open || !due[0].Occurrence.Equal(instant("2026-09-24T23:00:00Z")) {
		t.Fatalf("at 23:59: %+v %v", due, err)
	}
	run, err := ts.BeginPolicyRun(ctx, stored.ID, due[0].Occurrence, "late")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.FinishPolicyRun(ctx, run, RunApplied, "", ""); err != nil {
		t.Fatal(err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-25T00:00:30Z")); err != nil || len(due) != 0 {
		t.Fatalf("after midnight: %+v %v", due, err)
	}
}

func TestNextOccurrenceOnRead(t *testing.T) {
	st, a, app, _, _, _ := policyFixture(t, PolicyInput{Mode: PolicyModeApply, Timezone: "Europe/Paris", Weekdays: []int{1}, StartMinute: 120, EndMinute: 240})
	ctx := context.Background()
	p, _, err := st.Tenancy().ReadUpdatePolicy(ctx, a, app.ID)
	if err != nil || p.NextOccurrence == nil || !p.NextOccurrence.After(time.Now()) || p.NextOccurrence.Location() != time.UTC || p.NextOccurrence.In(mustZone(t, "Europe/Paris")).Weekday() != time.Monday {
		t.Fatalf("next: %+v %v", p, err)
	}
	next, ok := nextOccurrence(windowPolicy("UTC", []int{1}, 600, 660), instant("2026-09-21T10:30:00Z")) // Monday, open
	if !ok || !next.Equal(instant("2026-09-28T10:00:00Z")) {
		t.Fatalf("an open window is not next: %s", next)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE update_policies SET status='paused' WHERE id=?`), p.ID); err != nil {
		t.Fatal(err)
	}
	if p, _, err := st.Tenancy().ReadUpdatePolicy(ctx, a, app.ID); err != nil || p.NextOccurrence != nil {
		t.Fatalf("a paused policy has a next window: %+v %v", p, err)
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestSchedulePolicies(t *testing.T) {
	st, a, app, endpoint, m, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	backdatePolicy(t, st, p.ID, instant("2026-09-01T00:00:00Z"))
	// A second application nobody adopted, saved later: it comes second and has no endpoint.
	draft := policyDraft(t, st, a, "draft")
	second, _, err := ts.PutUpdatePolicy(ctx, a, draft.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	backdatePolicy(t, st, second.ID, instant("2026-09-02T00:00:00Z"))
	due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T10:30:00Z"))
	if err != nil || len(due) != 2 || due[0].Policy.ID != p.ID || due[1].Policy.ID != second.ID || due[1].EndpointID != "" {
		t.Fatalf("order: %+v %v", due, err)
	}
	first := due[0]
	if !first.Open || !first.Occurrence.Equal(instant("2026-09-24T10:00:00Z")) || first.EndpointID != endpoint || first.EndpointBusy || !first.Missed || !first.Previous.Equal(instant("2026-09-23T10:00:00Z")) || first.Policy.ApplicationID != app.ID || first.Policy.CreatedBy != "actor" {
		t.Fatalf("first: %+v", first)
	}
	if err := ts.DeleteUpdatePolicy(ctx, a, draft.ID); err != nil {
		t.Fatal(err)
	}
	// A live plan makes the endpoint busy.
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); err != nil {
		t.Fatal(err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T10:30:00Z")); err != nil || len(due) != 1 || !due[0].EndpointBusy {
		t.Fatalf("with a live plan: %+v %v", due, err)
	}
	// An expired plan does not: nothing ever moves it out of planned.
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE deployments SET expires_at=?`), time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T10:30:00Z")); err != nil || len(due) != 1 || due[0].EndpointBusy {
		t.Fatalf("with an expired plan: %+v %v", due, err)
	}
	// A run row closes the occurrence; a second one for it is refused.
	if _, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), "second"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("a second run for one window: %v", err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T10:30:00Z")); err != nil || len(due) != 1 || due[0].Open || !due[0].Missed {
		t.Fatalf("after the run started: %+v %v", due, err)
	}
	if err := ts.SkipPolicyWindow(ctx, p.ID, instant("2026-09-23T10:00:00Z"), RunSkippedMissed, PolicyDetailMissed); err != nil {
		t.Fatal(err)
	}
	if err := ts.SkipPolicyWindow(ctx, p.ID, instant("2026-09-23T10:00:00Z"), RunSkippedMissed, PolicyDetailMissed); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("a window skipped twice: %v", err)
	}
	if err := ts.SkipPolicyWindow(ctx, p.ID, instant("2026-09-22T10:00:00Z"), RunFailed, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("skip with a run outcome: %v", err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-24T10:30:00Z")); err != nil || len(due) != 0 {
		t.Fatalf("nothing left: %+v %v", due, err)
	}
	// A window that ended before the policy's last change is not missed.
	backdatePolicy(t, st, p.ID, instant("2026-09-25T12:00:00Z"))
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-26T09:00:00Z")); err != nil || len(due) != 0 {
		t.Fatalf("a window before the last change: %+v %v", due, err)
	}
	// A paused policy is not scheduled at all.
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE update_policies SET status='paused' WHERE id=?`), p.ID); err != nil {
		t.Fatal(err)
	}
	if due, err := ts.SchedulePolicies(ctx, instant("2026-09-27T10:30:00Z")); err != nil || len(due) != 0 {
		t.Fatalf("a paused policy: %+v %v", due, err)
	}
}

func TestPolicyRunsCountFailuresAndPause(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	var runs []string
	finish := func(day int, outcome, detail string) {
		t.Helper()
		id, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-10T10:00:00Z").AddDate(0, 0, day), fmt.Sprintf("corr-%d", day))
		if err != nil {
			t.Fatal(err)
		}
		if err := ts.FinishPolicyRun(ctx, id, outcome, "", detail); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, id)
	}
	state := func() *UpdatePolicy {
		t.Helper()
		got, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	finish(0, RunFailed, "unauthorized")
	finish(1, RunBlocked, "configuration_unsupported")
	if got := state(); got.ConsecutiveFailures != 2 || got.Status != PolicyActive {
		t.Fatalf("two failures: %+v", got)
	}
	finish(2, RunSkippedBusy, PolicyDetailBusy) // a skip leaves the count
	finish(3, RunSkippedMissed, PolicyDetailMissed)
	if got := state(); got.ConsecutiveFailures != 2 {
		t.Fatalf("after skips: %+v", got)
	}
	finish(4, RunNoUpdate, "") // a success resets it
	if got := state(); got.ConsecutiveFailures != 0 {
		t.Fatalf("after no_update: %+v", got)
	}
	for day := 5; day < 8; day++ {
		finish(day, RunFailed, "unavailable")
	}
	if got := state(); got.ConsecutiveFailures != 3 || got.Status != PolicyPaused || got.PausedReason != PolicyReasonFailures {
		t.Fatalf("three failures: %+v", got)
	}
	if _, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-18T10:00:00Z"), "corr-8"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a paused policy opened a run: %v", err)
	}
	rows := policyAudits(t, st)
	if len(rows) != 9 {
		t.Fatalf("audit rows: %+v", rows)
	}
	for i, r := range rows[:8] {
		if r.Action != AuditPolicyRun || r.User != "system" || r.Resource != app.ID+"/policies/"+p.ID+"/runs/"+runs[i] || r.Correlation != fmt.Sprintf("corr-%d", i) {
			t.Fatalf("run row %d: %+v", i, r)
		}
	}
	if rows[0].Result != "failure" || rows[0].Details != RunFailed || rows[1].Result != "failure" || rows[2].Result != "success" || rows[2].Details != RunSkippedBusy || rows[4].Result != "success" || rows[4].Details != RunNoUpdate {
		t.Fatalf("run results: %+v", rows)
	}
	if last := rows[8]; last.Action != AuditPolicyPaused || last.Result != "failure" || last.User != "system" || last.Details != PolicyReasonFailures || last.Correlation != "corr-7" || last.Resource != app.ID+"/policies/"+p.ID {
		t.Fatalf("pause row: %+v", last)
	}
	resumed, err := ts.ResumeUpdatePolicy(ctx, a, app.ID)
	if err != nil || resumed.Status != PolicyActive || resumed.ConsecutiveFailures != 0 || resumed.PausedReason != "" {
		t.Fatalf("resumed: %+v %v", resumed, err)
	}
	// A run is finished once; the outcome vocabulary is closed.
	if err := ts.FinishPolicyRun(ctx, runs[0], RunApplied, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished twice: %v", err)
	}
	open, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-19T10:00:00Z"), "corr-9")
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"", "exploded"} {
		if err := ts.FinishPolicyRun(ctx, open, outcome, "", ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("outcome %q: %v", outcome, err)
		}
	}
}

func TestPolicyRunPausesWhenItsCreatorLostAccess(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	run, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), "lost")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.FinishPolicyRun(ctx, run, RunPaused, "", PolicyDetailCreatorLost); err != nil {
		t.Fatal(err)
	}
	got, runs, err := ts.ReadUpdatePolicy(ctx, a, app.ID)
	if err != nil || got.Status != PolicyPaused || got.PausedReason != PolicyDetailCreatorLost || got.ConsecutiveFailures != 0 || len(runs) != 1 || runs[0].Outcome != RunPaused || runs[0].Detail != PolicyDetailCreatorLost {
		t.Fatalf("paused: %+v %+v %v", got, runs, err)
	}
	rows := policyAudits(t, st)
	if len(rows) != 2 || rows[0].Result != "denied" || rows[0].Details != RunPaused || rows[1].Action != AuditPolicyPaused || rows[1].Result != "denied" || rows[1].Details != PolicyDetailCreatorLost || rows[1].Correlation != "lost" {
		t.Fatalf("audit: %+v", rows)
	}
}

// A run a crash left open fails at the next start and counts; a second start changes nothing.
func TestReconcileAfterStartFailsInterruptedPolicyRuns(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	done, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-23T10:00:00Z"), "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.FinishPolicyRun(ctx, done, RunNoUpdate, "", ""); err != nil {
		t.Fatal(err)
	}
	open, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), "interrupted")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if n, err := ts.ReconcileAfterStart(ctx); err != nil || n != 0 {
			t.Fatalf("reconcile: %d %v", n, err)
		}
	}
	got, runs, err := ts.ReadUpdatePolicy(ctx, a, app.ID)
	if err != nil || got.ConsecutiveFailures != 1 || len(runs) != 2 {
		t.Fatalf("after reconcile: %+v %+v %v", got, runs, err)
	}
	if r := runs[0]; r.ID != open || r.Outcome != RunFailed || r.Detail != PolicyDetailRestarted || r.FinishedAt == nil {
		t.Fatalf("interrupted run: %+v", r)
	}
	if r := runs[1]; r.ID != done || r.Outcome != RunNoUpdate {
		t.Fatalf("finished run changed: %+v", r)
	}
	rows := policyAudits(t, st)
	if len(rows) != 2 || rows[1].Correlation != "interrupted" || rows[1].Result != "failure" || !strings.HasSuffix(rows[1].Resource, "/runs/"+open) {
		t.Fatalf("audit: %+v", rows)
	}
}

// A policy deleted (here with its application) between the tick and the run opens nothing and
// leaves nothing behind (Review Focus 1).
func TestBeginPolicyRunRefusesAGonePolicy(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app := policyDraft(t, st, a, "draft")
	p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.DiscardApplication(ctx, a, app.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.BeginPolicyRun(ctx, p.ID, instant("2026-09-24T10:00:00Z"), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("begin: %v", err)
	}
	if err := ts.SkipPolicyWindow(ctx, p.ID, instant("2026-09-23T10:00:00Z"), RunSkippedMissed, PolicyDetailMissed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("skip: %v", err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM policy_runs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("runs: %d %v", n, err)
	}
}

// A policy run's apply re-reads its policy in the apply transaction: deleted, paused, plan_only
// or saved by someone else refuses with ErrPolicyChanged and leaves the plan planned.
func TestApplyPolicyDeploymentRequiresThePolicyUnchanged(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *SQLStore, TenantAccess, string, string){
		"deleted": func(t *testing.T, st *SQLStore, a TenantAccess, app, _ string) {
			if err := st.Tenancy().DeleteUpdatePolicy(context.Background(), a, app); err != nil {
				t.Fatal(err)
			}
		},
		"paused": func(t *testing.T, st *SQLStore, _ TenantAccess, _, policy string) {
			if _, err := st.db.Exec(st.rebind(`UPDATE update_policies SET status=? WHERE id=?`), PolicyPaused, policy); err != nil {
				t.Fatal(err)
			}
		},
		"plan_only": func(t *testing.T, st *SQLStore, a TenantAccess, app, _ string) {
			in := dailyPolicy()
			in.Mode = PolicyModePlanOnly
			if _, _, err := st.Tenancy().PutUpdatePolicy(context.Background(), a, app, in); err != nil {
				t.Fatal(err)
			}
		},
		"another editor": func(t *testing.T, st *SQLStore, a TenantAccess, app, _ string) {
			if _, _, err := st.Tenancy().PutUpdatePolicy(context.Background(), policyMember(t, st, a, "other", RoleOrganizationAdmin), app, dailyPolicy()); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, a, app, _, _, _, d, key := applyFixture(t)
			ctx := context.Background()
			ts := st.Tenancy()
			p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, dailyPolicy())
			if err != nil {
				t.Fatal(err)
			}
			change(t, st, a, app.ID, p.ID)
			if _, _, err := ts.ApplyPolicyDeployment(ctx, a, p.ID, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); !errors.Is(err, ErrPolicyChanged) {
				t.Fatalf("apply: %v", err)
			}
			if got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID); err != nil || got.State != "planned" {
				t.Fatalf("deployment: %+v %v", got, err)
			}
		})
	}
	st, a, app, _, _, _, d, key := applyFixture(t)
	p, _, err := st.Tenancy().PutUpdatePolicy(context.Background(), a, app.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if applied, _, err := st.Tenancy().ApplyPolicyDeployment(context.Background(), a, p.ID, app.ID, d.ID, "shop", key, protocol.MaxDeploymentRequestBytes); err != nil || applied.State != "applying" {
		t.Fatalf("unchanged policy: %+v %v", applied, err)
	}
}
