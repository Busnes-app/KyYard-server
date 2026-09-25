package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

// The scheduler's audit actions. They are not permissions: the scheduler writes them as
// "system", and each run's own steps are audited under the policy's creator.
const (
	AuditPolicyRun    = "application.policy.run"
	AuditPolicyPaused = "application.policy.paused"
)

// policyResults maps a run outcome to the audit result; its keys are the closed vocabulary a
// finished run may carry.
var policyResults = map[string]string{
	RunSkippedMissed: "success", RunSkippedBusy: "success", RunNoUpdate: "success", RunPlanned: "success", RunApplied: "success",
	RunBlocked: "failure", RunFailed: "failure", RunPaused: "denied",
}

// occurrenceOn is p's window on calendar day y-m-d in loc (d may overflow; time.Date normalises
// it). ok is false when the zone skips the window's start that day (a DST gap: the built wall
// clock differs from the one asked for) or the day's weekday is not in the set. An overlap
// resolves to Go's first mapping, so it is one occurrence.
func occurrenceOn(p UpdatePolicy, loc *time.Location, y int, m time.Month, d int) (start, end time.Time, ok bool) {
	start = time.Date(y, m, d, p.StartMinute/60, p.StartMinute%60, 0, 0, loc)
	if start.Hour()*60+start.Minute() != p.StartMinute || !slices.Contains(p.Weekdays, int(start.Weekday())) {
		return time.Time{}, time.Time{}, false
	}
	return start, time.Date(y, m, d, p.EndMinute/60, p.EndMinute%60, 0, 0, loc), true
}

// policyOccurrence is p's occurrence on now's calendar date in p's zone, in UTC, and whether it
// is open: start ≤ now < end. Windows never cross midnight, so no other day's can be open. A
// zone that no longer loads has no occurrences.
func policyOccurrence(p UpdatePolicy, now time.Time) (start, end time.Time, open bool) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	y, m, d := now.In(loc).Date()
	start, end, ok := occurrenceOn(p, loc, y, m, d)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	return start.UTC(), end.UTC(), !now.Before(start) && now.Before(end)
}

// policyHorizon is how many days either side of now the scheduler looks for an occurrence: two
// weeks covers a single weekday whose nearest occurrence fell in a DST gap.
const policyHorizon = 14

// previousOccurrence is p's most recent occurrence that has ended by now, in UTC.
func previousOccurrence(p UpdatePolicy, now time.Time) (start, end time.Time, ok bool) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	y, m, d := now.In(loc).Date()
	for i := 0; i <= policyHorizon; i++ {
		if s, e, ok := occurrenceOn(p, loc, y, m, d-i); ok && !e.After(now) {
			return s.UTC(), e.UTC(), true
		}
	}
	return time.Time{}, time.Time{}, false
}

// nextOccurrence is the start of p's first occurrence after now, in UTC.
func nextOccurrence(p UpdatePolicy, now time.Time) (time.Time, bool) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.Time{}, false
	}
	y, m, d := now.In(loc).Date()
	for i := 0; i <= policyHorizon; i++ {
		if s, _, ok := occurrenceOn(p, loc, y, m, d+i); ok && s.After(now) {
			return s.UTC(), true
		}
	}
	return time.Time{}, false
}

// nextOccurrenceOf is what the API shows as next_occurrence: nil while p is paused.
func nextOccurrenceOf(p UpdatePolicy, now time.Time) *time.Time {
	if p.Status != PolicyActive {
		return nil
	}
	next, ok := nextOccurrence(p, now)
	if !ok {
		return nil
	}
	return &next
}

// ScheduledPolicy is an active policy as the scheduler sees it at one instant.
type ScheduledPolicy struct {
	Policy UpdatePolicy
	// EndpointID is the adopted instance's endpoint; "" when the application is not adopted.
	EndpointID string
	// EndpointBusy: a deployment on that endpoint is applying, or planned and unexpired.
	EndpointBusy bool
	// Open: Occurrence is open now and has no run row.
	Open       bool
	Occurrence time.Time
	// Missed: Previous ended after the policy's last change and has no run row.
	Missed   bool
	Previous time.Time
}

// SchedulePolicies lists the active policies with an open window or a missed one at now, in
// created_at order. Occurrences use now; plan expiry uses the wall clock, since a deployment's
// expiry is a fact of this server's time, not the tick's.
func (t *tenancyStore) SchedulePolicies(ctx context.Context, now time.Time) ([]ScheduledPolicy, error) {
	rows, err := t.store.db.QueryContext(ctx, `SELECT `+policyColumns+`,COALESCE(i.endpoint_id,'') FROM update_policies p LEFT JOIN application_instances i ON i.application_id=p.application_id WHERE p.status='active' ORDER BY p.created_at,p.id`)
	if err != nil {
		return nil, err
	}
	var active []ScheduledPolicy
	for rows.Next() {
		var endpoint string
		p, err := scanPolicy(rows, &endpoint)
		if err != nil {
			rows.Close()
			return nil, err
		}
		active = append(active, ScheduledPolicy{Policy: *p, EndpointID: endpoint})
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	wall := time.Now().UTC()
	out := []ScheduledPolicy{}
	for _, sp := range active {
		if start, _, open := policyOccurrence(sp.Policy, now); open {
			ran, err := t.policyRan(ctx, sp.Policy.ID, start)
			if err != nil {
				return nil, err
			}
			if !ran {
				sp.Open, sp.Occurrence = true, start
			}
		}
		if start, end, ok := previousOccurrence(sp.Policy, now); ok && end.After(sp.Policy.UpdatedAt) {
			ran, err := t.policyRan(ctx, sp.Policy.ID, start)
			if err != nil {
				return nil, err
			}
			if !ran {
				sp.Missed, sp.Previous = true, start
			}
		}
		if sp.Open && sp.EndpointID != "" {
			var live int
			if err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM deployments WHERE endpoint_id=? AND (state='applying' OR (state='planned' AND expires_at>?))`), sp.EndpointID, wall).Scan(&live); err != nil {
				return nil, err
			}
			sp.EndpointBusy = live > 0
		}
		if sp.Open || sp.Missed {
			out = append(out, sp)
		}
	}
	return out, nil
}

func (t *tenancyStore) policyRan(ctx context.Context, policy string, occurrence time.Time) (bool, error) {
	var n int
	err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM policy_runs WHERE policy_id=? AND occurrence=?`), policy, occurrence.UTC()).Scan(&n)
	return n > 0, err
}

// insertPolicyRun opens the run row for policy's occurrence. The policy must still exist and be
// active (ErrNotFound); a second row for one occurrence is ErrAlreadyExists.
func (t *tenancyStore) insertPolicyRun(ctx context.Context, tx *sql.Tx, policy string, occurrence time.Time, correlation string, now time.Time) (string, error) {
	lock := ""
	if t.store.driver == "postgres" {
		lock = " FOR UPDATE"
	}
	var org, env, app string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT organization_id,environment_id,application_id FROM update_policies WHERE id=? AND status='active'`+lock), policy).Scan(&org, &env, &app)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO policy_runs(id,policy_id,organization_id,environment_id,application_id,occurrence,started_at,correlation_id) VALUES(?,?,?,?,?,?,?,?)`), id, policy, org, env, app, occurrence.UTC(), now, correlation)
	if err != nil && (strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key")) {
		return "", ErrAlreadyExists
	}
	return id, err
}

// BeginPolicyRun records that a run for policy's occurrence started, before any work: the row is
// what makes a window run once across ticks and restarts.
func (t *tenancyStore) BeginPolicyRun(ctx context.Context, policy string, occurrence time.Time, correlation string) (string, error) {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := t.insertPolicyRun(ctx, tx, policy, occurrence, correlation, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// FinishPolicyRun records a run's outcome, counts it against the policy and audits it.
func (t *tenancyStore) FinishPolicyRun(ctx context.Context, run, outcome, deployment, detail string) error {
	if _, ok := policyResults[outcome]; !ok {
		return ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := t.finishPolicyRun(ctx, tx, run, outcome, deployment, detail, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// SkipPolicyWindow records a window the scheduler did not run, as one finished row.
func (t *tenancyStore) SkipPolicyWindow(ctx context.Context, policy string, occurrence time.Time, outcome, detail string) error {
	if outcome != RunSkippedMissed && outcome != RunSkippedBusy {
		return ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	id, err := t.insertPolicyRun(ctx, tx, policy, occurrence, uuid.NewString(), now)
	if err != nil {
		return err
	}
	if err := t.finishPolicyRun(ctx, tx, id, outcome, "", detail, now); err != nil {
		return err
	}
	return tx.Commit()
}

// finishPolicyRun settles an open run: blocked and failed count, applied, planned and no_update
// reset the count, skips leave it; PolicyFailureLimit failures or a paused outcome pause the
// policy. One audit row for the run and one for a pause, under the run's correlation ID.
func (t *tenancyStore) finishPolicyRun(ctx context.Context, tx *sql.Tx, run, outcome, deployment, detail string, now time.Time) error {
	var policy, org, env, app, correlation string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT policy_id,organization_id,environment_id,application_id,correlation_id FROM policy_runs WHERE id=? AND outcome=''`), run).Scan(&policy, &org, &env, &app, &correlation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var dep any
	if deployment != "" {
		dep = deployment
	}
	res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE policy_runs SET outcome=?,deployment_id=?,detail=?,finished_at=? WHERE id=? AND outcome=''`), outcome, dep, protocol.CleanText(detail, 255), now, run)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	pause := ""
	switch outcome {
	case RunBlocked, RunFailed:
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET consecutive_failures=consecutive_failures+1 WHERE id=?`), policy); err != nil {
			return err
		}
		var failures int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT consecutive_failures FROM update_policies WHERE id=?`), policy).Scan(&failures); err != nil {
			return err
		}
		if failures >= PolicyFailureLimit {
			pause = PolicyReasonFailures
		}
	case RunApplied, RunPlanned, RunNoUpdate:
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET consecutive_failures=0 WHERE id=?`), policy); err != nil {
			return err
		}
	case RunPaused:
		pause = PolicyDetailCreatorLost
	}
	resource := app + "/policies/" + policy
	if err := t.auditPolicy(ctx, tx, org, env, resource+"/runs/"+run, AuditPolicyRun, outcome, policyResults[outcome], correlation, now); err != nil {
		return err
	}
	if pause == "" {
		return nil
	}
	res, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET status=?,paused_reason=?,updated_at=? WHERE id=? AND status=?`), PolicyPaused, pause, now, policy, PolicyActive)
	if err != nil {
		return err
	}
	if n, err = res.RowsAffected(); err != nil {
		return err
	}
	if n == 0 {
		return nil // already paused
	}
	result := "failure"
	if outcome == RunPaused {
		result = "denied"
	}
	return t.auditPolicy(ctx, tx, org, env, resource, AuditPolicyPaused, pause, result, correlation, now)
}

// auditPolicy writes one scheduler row as "system" inside tx.
func (t *tenancyStore) auditPolicy(ctx context.Context, tx *sql.Tx, org, env, resource, action, details, result, correlation string, at time.Time) error {
	_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), "system", action, protocol.CleanText(resource, 255), protocol.CleanText(details, 255), at, "organization", org, env, correlation, result)
	return err
}

// reconcilePolicyRuns fails every run the previous process left open, counting each.
func (t *tenancyStore) reconcilePolicyRuns(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM policy_runs WHERE outcome='' ORDER BY started_at,id`)
	if err != nil {
		return err
	}
	var open []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		open = append(open, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, id := range open {
		if err := t.finishPolicyRun(ctx, tx, id, RunFailed, "", PolicyDetailRestarted, now); err != nil {
			return err
		}
	}
	return nil
}
