package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// Update policies: one per application, saved by an organization administrator and run by the
// scheduler in internal/api as whoever saved it last. See docs/application-schema.md, Update
// policies.
const (
	PolicyModeApply    = "apply"
	PolicyModePlanOnly = "plan_only"
	PolicyActive       = "active"
	PolicyPaused       = "paused"
	// MinPolicyWindowMinutes is the shortest window. A window never crosses midnight.
	MinPolicyWindowMinutes = 15
	// PolicyFailureLimit consecutive failed windows pause a policy.
	PolicyFailureLimit = 3
	// MaxPolicyRuns bounds one page of runs; the policy read carries the latest policyRunsShown.
	MaxPolicyRuns   = 100
	policyRunsShown = 20
)

// A run's outcome. It is empty only while the run is in flight.
const (
	RunSkippedMissed = "skipped_missed"
	RunSkippedBusy   = "skipped_busy"
	RunNoUpdate      = "no_update"
	RunPlanned       = "planned"
	RunApplied       = "applied"
	RunBlocked       = "blocked"
	RunFailed        = "failed"
	RunPaused        = "paused"
)

// The scheduler's own sentences: the only prose a run's detail or a pause reason holds.
const (
	PolicyDetailMissed      = "the server was not running during this window"
	PolicyDetailBusy        = "another deployment occupied the window"
	PolicyDetailCreatorLost = "the policy's creator no longer holds application.deploy"
	PolicyDetailRestarted   = "the server restarted during the run"
	PolicyReasonFailures    = "three consecutive windows failed"
)

var (
	ErrInvalidTimezone = errors.New("invalid update policy time zone")
	ErrInvalidWindow   = errors.New("invalid update policy window")
	ErrInvalidWeekdays = errors.New("invalid update policy weekdays")
	ErrInvalidMode     = errors.New("invalid update policy mode")
	ErrPolicyNotPaused = errors.New("update policy is not paused")
)

// PolicyInput is what an administrator saves: a mode and a weekly window in an IANA zone.
type PolicyInput struct {
	Mode        string `json:"mode"`
	Timezone    string `json:"timezone"`
	Weekdays    []int  `json:"weekdays"`
	StartMinute int    `json:"start_minute"`
	EndMinute   int    `json:"end_minute"`
}

type UpdatePolicy struct {
	ID                  string    `json:"id"`
	ApplicationID       string    `json:"application_id"`
	CreatedBy           string    `json:"created_by"`
	Mode                string    `json:"mode"`
	Timezone            string    `json:"timezone"`
	Weekdays            []int     `json:"weekdays"`
	StartMinute         int       `json:"start_minute"`
	EndMinute           int       `json:"end_minute"`
	Status              string    `json:"status"`
	PausedReason        string    `json:"paused_reason"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	// NextOccurrence is computed on read, UTC; nil while paused.
	NextOccurrence *time.Time `json:"next_occurrence"`
	OrganizationID string     `json:"-"`
	EnvironmentID  string     `json:"-"`
}

type PolicyRun struct {
	ID       string `json:"id"`
	PolicyID string `json:"policy_id"`
	// Occurrence is the window's start, UTC: the run's identity with PolicyID.
	Occurrence    time.Time  `json:"occurrence"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	Outcome       string     `json:"outcome"`
	DeploymentID  string     `json:"deployment_id"`
	Detail        string     `json:"detail"`
	CorrelationID string     `json:"correlation_id"`
}

// validPolicyZone admits an IANA zone the embedded database loads. "" and "Local" load too, but
// they name the server host's zone, not a place.
func validPolicyZone(name string) bool {
	if name == "" || name == "Local" || len(name) > 64 || !displaySafe(name) {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// weekdays validates in and returns its weekday set as stored: ascending, comma-separated.
func (in PolicyInput) weekdays() (string, error) {
	if in.Mode != PolicyModeApply && in.Mode != PolicyModePlanOnly {
		return "", ErrInvalidMode
	}
	if !validPolicyZone(in.Timezone) {
		return "", ErrInvalidTimezone
	}
	if len(in.Weekdays) == 0 || len(in.Weekdays) > 7 {
		return "", ErrInvalidWeekdays
	}
	days := slices.Sorted(slices.Values(in.Weekdays))
	parts := make([]string, len(days))
	for i, d := range days {
		if d < 0 || d > 6 || (i > 0 && days[i-1] == d) {
			return "", ErrInvalidWeekdays
		}
		parts[i] = strconv.Itoa(d)
	}
	// end-start >= 15 also puts start before end: no window crosses midnight.
	if in.StartMinute < 0 || in.EndMinute > 1440 || in.EndMinute-in.StartMinute < MinPolicyWindowMinutes {
		return "", ErrInvalidWindow
	}
	return strings.Join(parts, ","), nil
}

const policyColumns = `p.id,p.organization_id,p.environment_id,p.application_id,p.created_by,p.mode,p.timezone,p.weekdays,p.start_minute,p.end_minute,p.status,p.paused_reason,p.consecutive_failures,p.created_at,p.updated_at`

// scanPolicy reads policyColumns, then extra.
func scanPolicy(row interface{ Scan(...any) error }, extra ...any) (*UpdatePolicy, error) {
	var p UpdatePolicy
	var days string
	if err := row.Scan(append([]any{&p.ID, &p.OrganizationID, &p.EnvironmentID, &p.ApplicationID, &p.CreatedBy, &p.Mode, &p.Timezone, &days, &p.StartMinute, &p.EndMinute, &p.Status, &p.PausedReason, &p.ConsecutiveFailures, &p.CreatedAt, &p.UpdatedAt}, extra...)...); err != nil {
		return nil, err
	}
	p.Weekdays = []int{}
	for _, d := range strings.Split(days, ",") {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 || n > 6 {
			return nil, ErrRevisionCorrupt
		}
		p.Weekdays = append(p.Weekdays, n)
	}
	return &p, nil
}

func (t *tenancyStore) readPolicy(ctx context.Context, tx *sql.Tx, id string) (*UpdatePolicy, error) {
	return scanPolicy(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+policyColumns+` FROM update_policies p WHERE p.id=?`), id))
}

// policyOf returns the ID and status of app's policy in a's scope, locked on PostgreSQL;
// ErrNotFound when it has none.
func (t *tenancyStore) policyOf(ctx context.Context, tx *sql.Tx, a TenantAccess, app string) (string, string, error) {
	lock := ""
	if t.store.driver == "postgres" {
		lock = " FOR UPDATE"
	}
	var id, status string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,status FROM update_policies WHERE organization_id=? AND environment_id=? AND application_id=?`+lock), a.OrganizationID, a.EnvironmentID, app).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return id, status, err
}

const policyRunColumns = `id,policy_id,occurrence,started_at,finished_at,outcome,deployment_id,detail,correlation_id`

// policyRuns lists policy's runs newest window first.
func (t *tenancyStore) policyRuns(ctx context.Context, tx *sql.Tx, policy string, limit int) ([]PolicyRun, error) {
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT `+policyRunColumns+` FROM policy_runs WHERE policy_id=? ORDER BY occurrence DESC,id DESC LIMIT ?`), policy, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PolicyRun{}
	for rows.Next() {
		var r PolicyRun
		var finished sql.NullTime
		var deployment sql.NullString
		if err := rows.Scan(&r.ID, &r.PolicyID, &r.Occurrence, &r.StartedAt, &finished, &r.Outcome, &deployment, &r.Detail, &r.CorrelationID); err != nil {
			return nil, err
		}
		if finished.Valid {
			r.FinishedAt = &finished.Time
		}
		r.DeploymentID = deployment.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// PutUpdatePolicy creates app's policy or replaces its settings, and reports whether it created
// one. The saver becomes created_by: a policy always acts as its last editor. Status and the
// failure count are left alone; ResumeUpdatePolicy reopens a paused policy.
func (t *tenancyStore) PutUpdatePolicy(ctx context.Context, a TenantAccess, app string, in PolicyInput) (*UpdatePolicy, bool, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, false, ErrInvalid
	}
	target := appID.String() + "/policies"
	var out *UpdatePolicy
	created := false
	err = t.run(ctx, a, permissions.ApplicationPolicy, &target, nil, true, func(tx *sql.Tx) error {
		days, err := in.weekdays()
		if err != nil {
			return err
		}
		// The application row first, the lock order plans and releases take.
		if err := t.lockApplication(ctx, tx, a, appID.String()); err != nil {
			if errors.Is(err, ErrAdoptionChanged) {
				return ErrNotFound
			}
			return err
		}
		now := time.Now().UTC()
		id, _, err := t.policyOf(ctx, tx, a, appID.String())
		switch {
		case errors.Is(err, ErrNotFound):
			id, created = uuid.NewString(), true
			_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO update_policies(id,organization_id,environment_id,application_id,created_by,mode,timezone,weekdays,start_minute,end_minute,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`), id, a.OrganizationID, a.EnvironmentID, appID.String(), a.ActorID, in.Mode, in.Timezone, days, in.StartMinute, in.EndMinute, PolicyActive, now, now)
		case err == nil:
			_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET created_by=?,mode=?,timezone=?,weekdays=?,start_minute=?,end_minute=?,updated_at=? WHERE id=?`), a.ActorID, in.Mode, in.Timezone, days, in.StartMinute, in.EndMinute, now, id)
		}
		if err != nil {
			return err
		}
		target = appID.String() + "/policies/" + id
		out, err = t.readPolicy(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// ReadUpdatePolicy returns app's policy and its latest runs, or a nil policy when it has none:
// "no policy" is an answer, not a failure worth an audit row.
func (t *tenancyStore) ReadUpdatePolicy(ctx context.Context, a TenantAccess, app string) (*UpdatePolicy, []PolicyRun, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, nil, ErrInvalid
	}
	var out *UpdatePolicy
	runs := []PolicyRun{}
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		p, err := scanPolicy(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+policyColumns+` FROM update_policies p WHERE p.organization_id=? AND p.environment_id=? AND p.application_id=?`), a.OrganizationID, a.EnvironmentID, appID.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = p
		runs, err = t.policyRuns(ctx, tx, p.ID, policyRunsShown)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return out, runs, nil
}

// DeleteUpdatePolicy deletes app's policy and, by cascade, its runs. Deployments it made stay.
func (t *tenancyStore) DeleteUpdatePolicy(ctx context.Context, a TenantAccess, app string) error {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return ErrInvalid
	}
	target := appID.String() + "/policies"
	return t.run(ctx, a, permissions.ApplicationPolicy, &target, nil, true, func(tx *sql.Tx) error {
		id, _, err := t.policyOf(ctx, tx, a, appID.String())
		if err != nil {
			return err
		}
		target = appID.String() + "/policies/" + id
		_, err = tx.ExecContext(ctx, t.store.rebind(`DELETE FROM update_policies WHERE id=?`), id)
		return err
	})
}

// ResumeUpdatePolicy reopens a paused policy with its failure count at zero. It does not change
// who the policy acts as: a policy paused because its creator lost authority pauses again until
// someone who may deploy saves it.
func (t *tenancyStore) ResumeUpdatePolicy(ctx context.Context, a TenantAccess, app string) (*UpdatePolicy, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	target := appID.String() + "/policies"
	var out *UpdatePolicy
	err = t.run(ctx, a, permissions.ApplicationPolicy, &target, nil, true, func(tx *sql.Tx) error {
		id, status, err := t.policyOf(ctx, tx, a, appID.String())
		if err != nil {
			return err
		}
		target = appID.String() + "/policies/" + id + "/resume"
		if status != PolicyPaused {
			return ErrPolicyNotPaused
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET status=?,paused_reason='',consecutive_failures=0,updated_at=? WHERE id=?`), PolicyActive, time.Now().UTC(), id); err != nil {
			return err
		}
		out, err = t.readPolicy(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPolicyRuns returns up to limit (1..MaxPolicyRuns) of app's runs, newest window first; none
// when app has no policy.
func (t *tenancyStore) ListPolicyRuns(ctx context.Context, a TenantAccess, app string, limit int) ([]PolicyRun, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	out := []PolicyRun{}
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		if limit < 1 || limit > MaxPolicyRuns {
			return ErrInvalid
		}
		var id string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id FROM update_policies WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, appID.String()).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out, err = t.policyRuns(ctx, tx, id, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
