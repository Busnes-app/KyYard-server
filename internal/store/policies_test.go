package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func dailyPolicy() PolicyInput {
	return PolicyInput{Mode: PolicyModeApply, Timezone: "UTC", Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, StartMinute: 600, EndMinute: 660}
}

func instant(s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return v.UTC()
}

// policyFixture is planFixture's adopted, mapped application with a policy the actor saved.
func policyFixture(t *testing.T, in PolicyInput) (*SQLStore, TenantAccess, *Application, string, *ApplicationMapping, *UpdatePolicy) {
	t.Helper()
	st, a, app, endpoint, _, m := planFixture(t)
	p, created, err := st.Tenancy().PutUpdatePolicy(context.Background(), a, app.ID, in)
	if err != nil || !created {
		t.Fatalf("policy: created=%v %v", created, err)
	}
	return st, a, app, endpoint, m, p
}

// policyDraft imports an application nobody adopted, which DiscardApplication can delete.
func policyDraft(t *testing.T, st *SQLStore, a TenantAccess, name string) *Application {
	t.Helper()
	app, err := st.Tenancy().ImportApplication(context.Background(), a, name, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}, map[string]string{}, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// policyMember adds an active member of a's organization with role and returns their access.
func policyMember(t *testing.T, st *SQLStore, a TenantAccess, id string, role TenantRole) TenantAccess {
	t.Helper()
	ctx := context.Background()
	if err := st.Users().CreateUser(ctx, &User{ID: id, Username: id, Role: "user", Status: "active", SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: id, Role: role, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	a.ActorID = id
	return a
}

// rawPolicyRun writes a finished run row directly, for tests that precede the scheduler's writers.
func rawPolicyRun(t *testing.T, st *SQLStore, p *UpdatePolicy, occurrence time.Time, outcome string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`INSERT INTO policy_runs(id,policy_id,organization_id,environment_id,application_id,occurrence,started_at,finished_at,outcome,correlation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`), id, p.ID, p.OrganizationID, p.EnvironmentID, p.ApplicationID, occurrence, occurrence, occurrence, outcome, "raw"); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUpdatePolicyTables(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO update_policies(id,organization_id,environment_id,application_id,created_by,mode,timezone,weekdays,start_minute,end_minute,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`), uuid.NewString(), a.OrganizationID, a.EnvironmentID, app.ID, "actor", "apply", "UTC", "1", 600, 660, "active", now, now); err == nil {
		t.Fatal("a second policy for one application")
	}
	for name, stmt := range map[string]string{
		"mode":   `UPDATE update_policies SET mode='yolo' WHERE id=?`,
		"status": `UPDATE update_policies SET status='gone' WHERE id=?`,
		"window": `UPDATE update_policies SET start_minute=600,end_minute=610 WHERE id=?`,
		"reason": `UPDATE update_policies SET paused_reason=` + "'" + strings.Repeat("r", 256) + "'" + ` WHERE id=?`,
	} {
		if _, err := st.db.ExecContext(ctx, st.rebind(stmt), p.ID); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	run := rawPolicyRun(t, st, p, instant("2026-09-24T10:00:00Z"), RunApplied)
	for name, stmt := range map[string]string{
		"outcome": `UPDATE policy_runs SET outcome='exploded' WHERE id=?`,
		"detail":  `UPDATE policy_runs SET detail=` + "'" + strings.Repeat("d", 256) + "'" + ` WHERE id=?`,
	} {
		if _, err := st.db.ExecContext(ctx, st.rebind(stmt), run); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// UNIQUE (policy_id, occurrence) is the idempotency key: one attempt per window.
	if _, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO policy_runs(id,policy_id,organization_id,environment_id,application_id,occurrence,started_at,correlation_id) VALUES(?,?,?,?,?,?,?,?)`), uuid.NewString(), p.ID, a.OrganizationID, a.EnvironmentID, app.ID, instant("2026-09-24T10:00:00Z"), now, "dup"); err == nil {
		t.Fatal("a second run for one occurrence")
	}
}

// A policy and its runs go with their application; deployments are another table's business.
func TestUpdatePolicyGoesWithItsApplication(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app := policyDraft(t, st, a, "draft")
	p, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	rawPolicyRun(t, st, p, instant("2026-09-24T10:00:00Z"), RunNoUpdate)
	if err := ts.DiscardApplication(ctx, a, app.ID, 1); err != nil {
		t.Fatal(err)
	}
	var policies, runs int
	if err := st.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM update_policies),(SELECT COUNT(*) FROM policy_runs)`).Scan(&policies, &runs); err != nil || policies != 0 || runs != 0 {
		t.Fatalf("left behind: %d policies, %d runs, %v", policies, runs, err)
	}
}

func TestPutUpdatePolicyValidatesEveryField(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app := policyDraft(t, st, a, "draft")
	for _, tc := range []struct {
		name string
		edit func(*PolicyInput)
		want error
	}{
		{"no mode", func(in *PolicyInput) { in.Mode = "" }, ErrInvalidMode},
		{"unknown mode", func(in *PolicyInput) { in.Mode = "auto" }, ErrInvalidMode},
		{"no zone", func(in *PolicyInput) { in.Timezone = "" }, ErrInvalidTimezone},
		{"the host's zone", func(in *PolicyInput) { in.Timezone = "Local" }, ErrInvalidTimezone},
		{"unknown zone", func(in *PolicyInput) { in.Timezone = "Mars/Olympus_Mons" }, ErrInvalidTimezone},
		{"a path", func(in *PolicyInput) { in.Timezone = "../../etc/passwd" }, ErrInvalidTimezone},
		{"long zone", func(in *PolicyInput) { in.Timezone = "Europe/" + strings.Repeat("a", 60) }, ErrInvalidTimezone},
		{"control character", func(in *PolicyInput) { in.Timezone = "UTC\n" }, ErrInvalidTimezone},
		{"no weekdays", func(in *PolicyInput) { in.Weekdays = nil }, ErrInvalidWeekdays},
		{"empty weekdays", func(in *PolicyInput) { in.Weekdays = []int{} }, ErrInvalidWeekdays},
		{"weekday 7", func(in *PolicyInput) { in.Weekdays = []int{7} }, ErrInvalidWeekdays},
		{"negative weekday", func(in *PolicyInput) { in.Weekdays = []int{-1} }, ErrInvalidWeekdays},
		{"repeated weekday", func(in *PolicyInput) { in.Weekdays = []int{1, 1} }, ErrInvalidWeekdays},
		{"eight weekdays", func(in *PolicyInput) { in.Weekdays = []int{0, 1, 2, 3, 4, 5, 6, 0} }, ErrInvalidWeekdays},
		{"negative start", func(in *PolicyInput) { in.StartMinute = -1 }, ErrInvalidWindow},
		{"past midnight", func(in *PolicyInput) { in.StartMinute, in.EndMinute = 1430, 1441 }, ErrInvalidWindow},
		{"crosses midnight", func(in *PolicyInput) { in.StartMinute, in.EndMinute = 1380, 60 }, ErrInvalidWindow},
		{"empty window", func(in *PolicyInput) { in.EndMinute = in.StartMinute }, ErrInvalidWindow},
		{"fourteen minutes", func(in *PolicyInput) { in.EndMinute = in.StartMinute + 14 }, ErrInvalidWindow},
	} {
		in := dailyPolicy()
		tc.edit(&in)
		if _, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, in); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if p, _, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || p != nil {
		t.Fatalf("a refused policy was stored: %+v %v", p, err)
	}
	// The edges: an alias the zone database knows, fifteen minutes, both ends of the day.
	for _, in := range []PolicyInput{
		{Mode: PolicyModePlanOnly, Timezone: "US/Eastern", Weekdays: []int{6}, StartMinute: 0, EndMinute: 15},
		{Mode: PolicyModeApply, Timezone: "Pacific/Kiritimati", Weekdays: []int{0}, StartMinute: 1425, EndMinute: 1440},
	} {
		if _, _, err := ts.PutUpdatePolicy(ctx, a, app.ID, in); err != nil {
			t.Fatalf("%+v: %v", in, err)
		}
	}
	if _, _, err := ts.PutUpdatePolicy(ctx, a, uuid.NewString(), dailyPolicy()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a policy for no application: %v", err)
	}
}

func TestPutUpdatePolicyActsAsTheLastEditor(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, PolicyInput{Mode: PolicyModePlanOnly, Timezone: "Europe/Paris", Weekdays: []int{5, 1, 3}, StartMinute: 120, EndMinute: 240})
	ctx := context.Background()
	if p.Mode != PolicyModePlanOnly || p.Timezone != "Europe/Paris" || !slices.Equal(p.Weekdays, []int{1, 3, 5}) || p.StartMinute != 120 || p.EndMinute != 240 || p.Status != PolicyActive || p.CreatedBy != "actor" || p.ConsecutiveFailures != 0 || p.PausedReason != "" || p.ApplicationID != app.ID {
		t.Fatalf("created: %+v", p)
	}
	other := policyMember(t, st, a, "other", RoleOrganizationAdmin)
	edited, created, err := st.Tenancy().PutUpdatePolicy(ctx, other, app.ID, dailyPolicy())
	if err != nil || created || edited.ID != p.ID || edited.CreatedBy != "other" || edited.Timezone != "UTC" || edited.Mode != PolicyModeApply || !edited.CreatedAt.Equal(p.CreatedAt) || edited.UpdatedAt.Before(p.UpdatedAt) {
		t.Fatalf("edited: created=%v %+v %v", created, edited, err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM audit_records WHERE action='application.policy' AND resource=? AND result='success'`), app.ID+"/policies/"+p.ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("save audit rows: %d %v", n, err)
	}
}

func TestUpdatePolicyAuthorization(t *testing.T) {
	st, a, app, _, _, _ := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	for id, role := range map[string]TenantRole{"envadmin": RoleEnvironmentAdmin, "operator": RoleOperator, "developer": RoleDeveloper, "viewer": RoleReadOnly} {
		m := policyMember(t, st, a, id, role)
		if _, _, err := ts.PutUpdatePolicy(ctx, m, app.ID, dailyPolicy()); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s put: %v", role, err)
		}
		if err := ts.DeleteUpdatePolicy(ctx, m, app.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s delete: %v", role, err)
		}
		if _, err := ts.ResumeUpdatePolicy(ctx, m, app.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s resume: %v", role, err)
		}
		if p, _, err := ts.ReadUpdatePolicy(ctx, m, app.ID); err != nil || p == nil {
			t.Errorf("%s read: %v", role, err)
		}
		if _, err := ts.ListPolicyRuns(ctx, m, app.ID, 20); err != nil {
			t.Errorf("%s runs: %v", role, err)
		}
	}
	outsider := a
	outsider.ActorID = "nobody"
	if _, _, err := ts.ReadUpdatePolicy(ctx, outsider, app.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-member read the policy: %v", err)
	}
}

func TestDeleteUpdatePolicyTakesItsRuns(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	rawPolicyRun(t, st, p, instant("2026-09-24T10:00:00Z"), RunNoUpdate)
	if err := ts.DeleteUpdatePolicy(ctx, a, app.ID); err != nil {
		t.Fatal(err)
	}
	if got, runs, err := ts.ReadUpdatePolicy(ctx, a, app.ID); err != nil || got != nil || len(runs) != 0 {
		t.Fatalf("after delete: %+v %v %v", got, runs, err)
	}
	var runs int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM policy_runs`).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("runs left: %d %v", runs, err)
	}
	if err := ts.DeleteUpdatePolicy(ctx, a, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestResumeUpdatePolicy(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.ResumeUpdatePolicy(ctx, a, app.ID); !errors.Is(err, ErrPolicyNotPaused) {
		t.Fatalf("resume an active policy: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE update_policies SET status='paused',paused_reason=?,consecutive_failures=3 WHERE id=?`), PolicyReasonFailures, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ResumeUpdatePolicy(ctx, a, app.ID)
	if err != nil || got.Status != PolicyActive || got.ConsecutiveFailures != 0 || got.PausedReason != "" || got.UpdatedAt.Before(p.UpdatedAt) {
		t.Fatalf("resumed: %+v %v", got, err)
	}
}

func TestListPolicyRuns(t *testing.T) {
	st, a, app, _, _, p := policyFixture(t, dailyPolicy())
	ctx := context.Background()
	ts := st.Tenancy()
	for _, limit := range []int{0, -1, MaxPolicyRuns + 1} {
		if _, err := ts.ListPolicyRuns(ctx, a, app.ID, limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	older := rawPolicyRun(t, st, p, instant("2026-09-23T10:00:00Z"), RunNoUpdate)
	newer := rawPolicyRun(t, st, p, instant("2026-09-24T10:00:00Z"), RunSkippedMissed)
	runs, err := ts.ListPolicyRuns(ctx, a, app.ID, MaxPolicyRuns)
	if err != nil || len(runs) != 2 || runs[0].ID != newer || runs[1].ID != older || runs[0].Outcome != RunSkippedMissed || !runs[0].Occurrence.Equal(instant("2026-09-24T10:00:00Z")) || runs[0].FinishedAt == nil || runs[0].DeploymentID != "" {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	if runs, err := ts.ListPolicyRuns(ctx, a, app.ID, 1); err != nil || len(runs) != 1 || runs[0].ID != newer {
		t.Fatalf("limit 1: %+v %v", runs, err)
	}
	draft := policyDraft(t, st, a, "no-policy")
	if runs, err := ts.ListPolicyRuns(ctx, a, draft.ID, 20); err != nil || runs == nil || len(runs) != 0 {
		t.Fatalf("no policy: %+v %v", runs, err)
	}
}
