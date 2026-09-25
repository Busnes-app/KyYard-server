package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

// Health validation: every succeeded apply is watched for a bounded time and given a verdict; an
// automated one that fails is rolled back when it can be (rollback.go). The loop is
// api.Server.RunValidations. See docs/application-schema.md, Health validation.
const (
	ValidationGrace  = 30 * time.Second
	ValidationWindow = 2 * time.Minute
	ValidationPoll   = 20 * time.Second
	// MaxPendingValidations bounds one tick's work; the rest wait for the next tick.
	MaxPendingValidations = 256
	// AuditValidation is the loop's audit action, written as "system".
	AuditValidation = "application.validation"
	// maxBaselineBytes matches the baseline column's CHECK.
	maxBaselineBytes = 32768
)

const (
	PhaseGrace     = "grace"
	PhaseObserving = "observing"
	PhaseDone      = "done"
)

const (
	VerdictHealthy      = "healthy"
	VerdictUnhealthy    = "unhealthy"
	VerdictExited       = "exited"
	VerdictRestarting   = "restarting"
	VerdictUnverifiable = "unverifiable"
	VerdictChanged      = "changed"
)

// A rollback's outcome, and the codes its detail holds besides plan blockers and the scheduler's
// codes (policyErrorCodes in internal/api).
const (
	RollbackApplied    = "applied"
	RollbackIneligible = "ineligible"
	RollbackFailed     = "failed"

	// A rollback returns each service to its plan's Replaces image; a service that replaced no
	// container has none.
	RollbackNoPriorIdentity        = "no_prior_identity"
	RollbackPriorDefinitionInvalid = "prior_definition_invalid"
	RollbackServiceSetChanged      = "service_set_changed"
	RollbackPriorImagesMissing     = "prior_images_missing"
	RollbackInFlight               = "rollback_in_flight"
	RollbackAlreadyRolledBack      = "already_rolled_back"
	RollbackCreatorLost            = "creator_lost"
	RollbackNotSent                = "not_sent"
	RollbackInterrupted            = "interrupted"
)

// The loop's own sentences: the only prose a validation detail or its pause reason holds. A
// failing verdict's detail is the deciding service's name.
const (
	ValidationDetailUnobserved = "the host could not be observed"
	ValidationDetailServerDown = "the server was not running during the window"
	ValidationDetailNoHealth   = "the agent cannot report container health"
	ValidationDetailInvalid    = "an inspection failed validation"
	ValidationDetailReleased   = "the application was released"

	ValidationReasonRolledBack    = "rolled back after a failed update"
	ValidationReasonNotRolledBack = "update failed and could not be rolled back: "
	ValidationReasonUnverified    = "update could not be validated: "
)

// Where a settled container stands against the instance's resources and the latest inventory.
const (
	PresencePresent  = "present"
	PresenceGone     = "gone"
	PresenceReplaced = "replaced"
	PresenceUnknown  = "unknown" // no inventory received since the settle: inspect and wait
)

// validationResults maps a verdict to its audit result; its keys are the closed vocabulary.
var validationResults = map[string]string{VerdictHealthy: "success", VerdictUnhealthy: "failure", VerdictExited: "failure", VerdictRestarting: "failure", VerdictUnverifiable: "failure", VerdictChanged: "failure"}

// rollbackVerdicts are the failures an automated update is rolled back for.
var rollbackVerdicts = map[string]bool{VerdictUnhealthy: true, VerdictExited: true, VerdictRestarting: true}

// awaitingRollback selects, as v, a finished automated validation whose rollback decision is owed.
const awaitingRollback = `(v.phase='done' AND v.automated=1 AND v.is_rollback=0 AND v.policy_run_id IS NOT NULL AND v.verdict IN ('unhealthy','exited','restarting') AND v.rollback_outcome='' AND v.rollback_deployment_id IS NULL)`

// validationColumns reads a validation aliased v and its rollback deployment aliased rd. Every
// column is NULL-safe, so a LEFT JOIN that found none scans as none.
const validationColumns = `COALESCE(v.deployment_id,''),COALESCE(v.policy_run_id,''),COALESCE(v.automated,0),COALESCE(v.is_rollback,0),COALESCE(v.phase,''),v.started_at,v.observe_until,COALESCE(v.verdict,''),COALESCE(v.detail,''),COALESCE(v.rollback_deployment_id,''),COALESCE(v.rollback_outcome,''),COALESCE(v.rollback_detail,''),COALESCE(rd.revision,0),COALESCE(v.correlation_id,''),v.finished_at`

type ValidationRollback struct {
	DeploymentID string `json:"deployment_id"`
	// Revision is the rollback deployment's; 0 once that row is gone.
	Revision int    `json:"revision"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail"`
}

// Validation is a deployment's health validation as the API shows it: the row without its
// baseline and tenant columns. Rollback is nil until a rollback is named or decided.
type Validation struct {
	DeploymentID  string              `json:"deployment_id"`
	PolicyRunID   string              `json:"policy_run_id"`
	Automated     bool                `json:"automated"`
	IsRollback    bool                `json:"is_rollback"`
	Phase         string              `json:"phase"`
	StartedAt     time.Time           `json:"started_at"`
	ObserveUntil  time.Time           `json:"observe_until"`
	Verdict       string              `json:"verdict"`
	Detail        string              `json:"detail"`
	Rollback      *ValidationRollback `json:"rollback"`
	CorrelationID string              `json:"correlation_id"`
	FinishedAt    *time.Time          `json:"finished_at"`
}

// ServiceBaseline is one service at the first observation after grace.
type ServiceBaseline struct {
	ContainerID  string `json:"container_id"`
	RestartCount int    `json:"restart_count"`
}

// ObservedService is a settled identity and where it stands now.
type ObservedService struct {
	protocol.DeploymentIdentity
	Presence string
}

// PendingValidation is a validation the loop still has work on, with what it needs to do it.
type PendingValidation struct {
	Validation
	OrganizationID, EnvironmentID, ApplicationID, InstanceID, EndpointID string
	Baseline                                                             map[string]ServiceBaseline
	Services                                                             []ObservedService
	// Health: the endpoint's agent advertises container.inspect.health.
	Health bool
	// Released: the instance is gone.
	Released bool
	// PolicyID and CreatedBy name the automated run's policy; empty once it is deleted.
	PolicyID, CreatedBy string
}

// Observation is one service at one poll: its presence and, when one succeeded, its inspection.
type Observation struct {
	Service    string
	Presence   string
	Inspection *protocol.ContainerInspection
}

// unobserved marks a service one poll could not see.
const unobserved = "unobserved"

// verdictRank orders the failing verdicts: a poll with several reports the strongest.
var verdictRank = map[string]int{VerdictUnhealthy: 1, VerdictRestarting: 2, VerdictExited: 3, VerdictChanged: 4}

// Judge applies the verdict rules to one poll against baseline. It returns the terminal verdict and
// the service that decided it, or "" while the window goes on. final: observe_until has passed, so
// a service still starting is unhealthy and a complete, passing poll is healthy.
func Judge(obs []Observation, baseline map[string]ServiceBaseline, final bool) (verdict, detail string) {
	complete := true
	for _, o := range obs {
		v := judgeService(o, baseline[o.Service], final)
		if v == unobserved {
			complete = false
			continue
		}
		if verdictRank[v] > verdictRank[verdict] {
			verdict, detail = v, o.Service
		}
	}
	if verdict == "" && final && complete {
		return VerdictHealthy, ""
	}
	return verdict, detail
}

func judgeService(o Observation, b ServiceBaseline, final bool) string {
	switch o.Presence {
	case PresenceReplaced:
		return VerdictChanged
	case PresenceGone:
		return VerdictExited
	}
	in := o.Inspection
	switch {
	case in == nil:
		return unobserved
	case in.State != "running" && in.State != "restarting":
		return VerdictExited
	case in.State == "restarting" || in.RestartCount > b.RestartCount:
		return VerdictRestarting
	case in.Health == "unhealthy", in.Health == "starting" && final:
		return VerdictUnhealthy
	}
	return ""
}

// BaselineOf is the baseline a first observation gives, false unless every service was either
// inspected or is known gone or replaced.
func BaselineOf(obs []Observation) (map[string]ServiceBaseline, bool) {
	out := map[string]ServiceBaseline{}
	for _, o := range obs {
		switch {
		case o.Inspection != nil:
			out[o.Service] = ServiceBaseline{ContainerID: o.Inspection.Target.ContainerID, RestartCount: o.Inspection.RestartCount}
		case o.Presence != PresenceGone && o.Presence != PresenceReplaced:
			return nil, false
		}
	}
	return out, true
}

type boundResource struct{ containerID, name string }

// presence places a settled identity: rebound or unbound means a later deployment or a release
// took the service; otherwise the inventory decides, when one arrived since the settle (snap
// non-nil). A container holding the resource's name under another ID was recreated.
func presence(id protocol.DeploymentIdentity, bound map[string]boundResource, snap *protocol.Snapshot) string {
	r, ok := bound[id.Service]
	if !ok || r.containerID != id.ContainerID {
		return PresenceReplaced
	}
	if snap == nil {
		return PresenceUnknown
	}
	if slices.ContainsFunc(snap.Containers, func(c protocol.Container) bool { return c.ID == id.ContainerID }) {
		return PresencePresent
	}
	if slices.ContainsFunc(snap.Containers, func(c protocol.Container) bool { return c.Name == r.name }) {
		return PresenceReplaced
	}
	return PresenceGone
}

// validationScan receives validationColumns.
type validationScan struct {
	id, run, phase, verdict, detail, rbID, rbOutcome, rbDetail, correlation string
	automated, isRollback, rbRevision                                       int
	started, until, finished                                                sql.NullTime
}

func (s *validationScan) dest() []any {
	return []any{&s.id, &s.run, &s.automated, &s.isRollback, &s.phase, &s.started, &s.until, &s.verdict, &s.detail, &s.rbID, &s.rbOutcome, &s.rbDetail, &s.rbRevision, &s.correlation, &s.finished}
}

// validation is the scanned row, nil when the LEFT JOIN found none.
func (s *validationScan) validation() *Validation {
	if s.id == "" {
		return nil
	}
	v := &Validation{DeploymentID: s.id, PolicyRunID: s.run, Automated: s.automated == 1, IsRollback: s.isRollback == 1, Phase: s.phase, StartedAt: s.started.Time.UTC(), ObserveUntil: s.until.Time.UTC(), Verdict: s.verdict, Detail: s.detail, CorrelationID: s.correlation}
	if s.finished.Valid {
		f := s.finished.Time.UTC()
		v.FinishedAt = &f
	}
	if s.rbID != "" || s.rbOutcome != "" {
		v.Rollback = &ValidationRollback{DeploymentID: s.rbID, Revision: s.rbRevision, Outcome: s.rbOutcome, Detail: s.rbDetail}
	}
	return v
}

// insertValidation opens the validation of a succeeded apply inside the settling transaction. It is
// automated when a policy run applied the deployment (deployments.policy_run_id, written by
// ApplyPolicyDeployment), whatever became of the run; a run's plan applied by hand is manual. It is
// a rollback when a validation named it as its rollback.
func (t *tenancyStore) insertValidation(ctx context.Context, tx *sql.Tx, org, env, app, instance, endpoint, deployment, correlation string, settled time.Time) error {
	var run sql.NullString
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT policy_run_id FROM deployments WHERE id=?`), deployment).Scan(&run); err != nil {
		return err
	}
	var dispatched int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM deployment_validations WHERE rollback_deployment_id=?`), deployment).Scan(&dispatched); err != nil {
		return err
	}
	automated, rollback := 0, 0
	if run.Valid {
		automated = 1
	}
	if dispatched > 0 {
		rollback = 1
	}
	if correlation == "" {
		correlation = uuid.NewString()
	}
	_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployment_validations(deployment_id,organization_id,environment_id,application_id,instance_id,endpoint_id,policy_run_id,automated,is_rollback,phase,started_at,observe_until,correlation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`), deployment, org, env, app, instance, endpoint, run, automated, rollback, PhaseGrace, settled, settled.Add(ValidationGrace+ValidationWindow), correlation)
	return err
}

// rowQuerier is *sql.DB or *sql.Tx.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// boundServices is what the instance's resources bind each service to now.
func (t *tenancyStore) boundServices(ctx context.Context, q rowQuerier, instance string) (map[string]boundResource, error) {
	rows, err := q.QueryContext(ctx, t.store.rebind(`SELECT service_name,container_id,name FROM application_resources WHERE instance_id=? AND service_name<>''`), instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]boundResource{}
	for rows.Next() {
		var service string
		var r boundResource
		if err := rows.Scan(&service, &r.containerID, &r.name); err != nil {
			return nil, err
		}
		out[service] = r
	}
	return out, rows.Err()
}

type inventoryView struct {
	snapshot protocol.Snapshot
	received time.Time
}

// latestInventory is the endpoint's stored snapshot; nil when there is none or its container list
// is truncated, since a container missing from it would prove nothing.
func (t *tenancyStore) latestInventory(ctx context.Context, endpoint string) (*inventoryView, error) {
	var raw string
	var v inventoryView
	err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT snapshot,received_at FROM endpoint_inventory WHERE endpoint_id=?`), endpoint).Scan(&raw, &v.received)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal([]byte(raw), &v.snapshot) != nil || len(v.snapshot.Containers) > protocol.MaxContainers || slices.Contains(v.snapshot.Truncated, "containers") {
		return nil, nil
	}
	return &v, nil
}

// PendingValidations lists, oldest first, every validation not done and every automated one whose
// rollback decision is owed, each with its settled services placed against the instance's
// resources and the endpoint's latest inventory.
func (t *tenancyStore) PendingValidations(ctx context.Context) ([]PendingValidation, error) {
	rows, err := t.store.db.QueryContext(ctx, t.store.rebind(`SELECT `+validationColumns+`,v.organization_id,v.environment_id,v.application_id,v.instance_id,v.endpoint_id,v.baseline,d.result,COALESCE(p.id,''),COALESCE(p.created_by,''),(SELECT COUNT(*) FROM application_instances i WHERE i.id=v.instance_id),(SELECT COUNT(*) FROM endpoint_capabilities c WHERE c.endpoint_id=v.endpoint_id AND c.capability=?) FROM deployment_validations v JOIN deployments d ON d.id=v.deployment_id LEFT JOIN deployments rd ON rd.id=v.rollback_deployment_id LEFT JOIN policy_runs r ON r.id=v.policy_run_id LEFT JOIN update_policies p ON p.id=r.policy_id WHERE v.phase<>'done' OR `+awaitingRollback+` ORDER BY v.started_at,v.deployment_id LIMIT ?`), protocol.CapabilityContainerInspectHealth, MaxPendingValidations)
	if err != nil {
		return nil, err
	}
	out := []PendingValidation{}
	for rows.Next() {
		var s validationScan
		var p PendingValidation
		var baseline, result string
		var instances, health int
		if err := rows.Scan(append(s.dest(), &p.OrganizationID, &p.EnvironmentID, &p.ApplicationID, &p.InstanceID, &p.EndpointID, &baseline, &result, &p.PolicyID, &p.CreatedBy, &instances, &health)...); err != nil {
			rows.Close()
			return nil, err
		}
		p.Validation = *s.validation()
		p.Released, p.Health = instances == 0, health > 0
		var stored storedDeploymentResult
		if json.Unmarshal([]byte(result), &stored) != nil || (baseline != "" && json.Unmarshal([]byte(baseline), &p.Baseline) != nil) {
			rows.Close()
			return nil, ErrRevisionCorrupt
		}
		for _, id := range stored.Services {
			p.Services = append(p.Services, ObservedService{DeploymentIdentity: id})
		}
		out = append(out, p)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	inventories := map[string]*inventoryView{}
	for i := range out {
		p := &out[i]
		bound, err := t.boundServices(ctx, t.store.db, p.InstanceID)
		if err != nil {
			return nil, err
		}
		inv, seen := inventories[p.EndpointID]
		if !seen {
			if inv, err = t.latestInventory(ctx, p.EndpointID); err != nil {
				return nil, err
			}
			inventories[p.EndpointID] = inv
		}
		var snap *protocol.Snapshot
		if inv != nil && !inv.received.Before(p.StartedAt) {
			snap = &inv.snapshot
		}
		for j := range p.Services {
			p.Services[j].Presence = presence(p.Services[j].DeploymentIdentity, bound, snap)
		}
	}
	return out, nil
}

// BeginObservation stores the baseline taken at the first observation after grace and moves the
// validation to observing. A row no longer in grace is ErrNotFound.
func (t *tenancyStore) BeginObservation(ctx context.Context, deployment string, baseline map[string]ServiceBaseline) error {
	raw, err := json.Marshal(baseline)
	if err != nil || len(raw) > maxBaselineBytes {
		return ErrInvalid
	}
	res, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE deployment_validations SET phase=?,baseline=? WHERE deployment_id=? AND phase=?`), PhaseObserving, string(raw), deployment, PhaseGrace)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// FinishValidation records a verdict and its audit row. For an automated update that is not itself
// a rollback, unverifiable pauses the policy, and a failing verdict reports that a rollback
// decision is due (true). A validation already done is ErrNotFound.
func (t *tenancyStore) FinishValidation(ctx context.Context, deployment, verdict, detail string) (bool, error) {
	if _, ok := validationResults[verdict]; !ok {
		return false, ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	decide, err := t.finishValidation(ctx, tx, deployment, verdict, detail, time.Now().UTC())
	if err != nil {
		return false, err
	}
	return decide, tx.Commit()
}

func (t *tenancyStore) finishValidation(ctx context.Context, tx *sql.Tx, deployment, verdict, detail string, now time.Time) (bool, error) {
	var org, env, app, correlation string
	var run sql.NullString
	var automated, rollback int
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT organization_id,environment_id,application_id,correlation_id,policy_run_id,automated,is_rollback FROM deployment_validations WHERE deployment_id=? AND phase<>'done'`), deployment).Scan(&org, &env, &app, &correlation, &run, &automated, &rollback)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	detail = protocol.CleanText(detail, 255)
	if err := updateOne(ctx, tx, t.store.rebind(`UPDATE deployment_validations SET phase=?,verdict=?,detail=?,finished_at=? WHERE deployment_id=? AND phase<>'done'`), PhaseDone, verdict, detail, now, deployment); err != nil {
		return false, err
	}
	if err := t.auditPolicy(ctx, tx, org, env, app+"/deployments/"+deployment, AuditValidation, verdict, validationResults[verdict], correlation, now); err != nil {
		return false, err
	}
	if automated == 0 || rollback == 1 || !run.Valid {
		return false, nil
	}
	if verdict == VerdictUnverifiable {
		return false, t.pauseForValidation(ctx, tx, run.String, ValidationReasonUnverified+detail, correlation, now)
	}
	return rollbackVerdicts[verdict], nil
}

// MarkRollbackPlanned names the rollback deployment on a validation awaiting its decision, before
// that deployment is applied: its settle then knows it is a rollback, and nothing dispatches
// another. A validation not awaiting a decision is ErrNotFound.
func (t *tenancyStore) MarkRollbackPlanned(ctx context.Context, deployment, rollback string) error {
	res, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE deployment_validations SET rollback_deployment_id=? WHERE deployment_id IN (SELECT v.deployment_id FROM deployment_validations v WHERE v.deployment_id=? AND `+awaitingRollback+`)`), rollback, deployment)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// MarkRollbackOutcome records the rollback decision on an automated validation and pauses its
// policy with the matching reason. applied needs the rollback already named.
func (t *tenancyStore) MarkRollbackOutcome(ctx context.Context, deployment, outcome, detail string) error {
	switch outcome {
	case RollbackApplied, RollbackIneligible, RollbackFailed:
	default:
		return ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := t.markRollbackOutcome(ctx, tx, deployment, outcome, detail, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (t *tenancyStore) markRollbackOutcome(ctx context.Context, tx *sql.Tx, deployment, outcome, detail string, now time.Time) error {
	var run sql.NullString
	var correlation, rollback string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT policy_run_id,correlation_id,COALESCE(rollback_deployment_id,'') FROM deployment_validations WHERE deployment_id=? AND phase='done' AND automated=1 AND is_rollback=0 AND rollback_outcome=''`), deployment).Scan(&run, &correlation, &rollback)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if outcome == RollbackApplied && rollback == "" {
		return ErrInvalid
	}
	detail = protocol.CleanText(detail, 255)
	if err := updateOne(ctx, tx, t.store.rebind(`UPDATE deployment_validations SET rollback_outcome=?,rollback_detail=? WHERE deployment_id=? AND rollback_outcome='' AND verdict IN ('unhealthy','exited','restarting')`), outcome, detail, deployment); err != nil {
		return err
	}
	if !run.Valid {
		return nil
	}
	reason := ValidationReasonRolledBack
	if outcome != RollbackApplied {
		reason = ValidationReasonNotRolledBack + detail
	}
	return t.pauseForValidation(ctx, tx, run.String, reason, correlation, now)
}

// pauseForValidation pauses the policy whose run made the deployment, with its audit row under the
// deployment's correlation ID. A deleted or already paused policy is left as it is.
func (t *tenancyStore) pauseForValidation(ctx context.Context, tx *sql.Tx, run, reason, correlation string, now time.Time) error {
	var policy, org, env, app string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT p.id,p.organization_id,p.environment_id,p.application_id FROM policy_runs r JOIN update_policies p ON p.id=r.policy_id WHERE r.id=?`), run).Scan(&policy, &org, &env, &app)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	reason = protocol.CleanText(reason, 255)
	res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE update_policies SET status=?,paused_reason=?,updated_at=? WHERE id=? AND status=?`), PolicyPaused, reason, now, policy, PolicyActive)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	return t.auditPolicy(ctx, tx, org, env, app+"/policies/"+policy, AuditPolicyPaused, reason, "failure", correlation, now)
}

// reconcileValidations settles what the previous process left: a validation still in grace whose
// window has ended never had its baseline taken, and a named rollback is decided by the live rule
// (decideNamedRollbacks). Neither is dispatched again.
func (t *tenancyStore) reconcileValidations(ctx context.Context, tx *sql.Tx) error {
	now := time.Now().UTC()
	missed, err := t.validationIDs(ctx, tx, `SELECT deployment_id FROM deployment_validations WHERE phase='grace' AND observe_until<? ORDER BY started_at,deployment_id`, now)
	if err != nil {
		return err
	}
	for _, id := range missed {
		if _, err := t.finishValidation(ctx, tx, id, VerdictUnverifiable, ValidationDetailServerDown, now); err != nil {
			return err
		}
	}
	return t.decideNamedRollbacks(ctx, tx, now)
}

// DecideNamedRollbacks decides every named, undecided rollback from its deployment's state, for
// the live loop: one whose outcome write failed would otherwise leave its policy active.
func (t *tenancyStore) DecideNamedRollbacks(ctx context.Context) error {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := t.decideNamedRollbacks(ctx, tx, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// decideNamedRollbacks marks named, undecided rollbacks, at startup and on every live tick alike:
// applied when the deployment settled succeeded; one still applying or an unexpired plan waits (at
// startup the deployment sweep settles an applying row); failed interrupted otherwise (unknown,
// failed, denied, timed_out, an expired plan, or gone).
func (t *tenancyStore) decideNamedRollbacks(ctx context.Context, tx *sql.Tx, now time.Time) error {
	undecided := `SELECT v.deployment_id FROM deployment_validations v LEFT JOIN deployments rd ON rd.id=v.rollback_deployment_id WHERE v.rollback_deployment_id IS NOT NULL AND v.rollback_outcome='' AND `
	interrupted := `COALESCE(rd.state,'')<>'succeeded' AND NOT (COALESCE(rd.state,'')='applying' OR (COALESCE(rd.state,'')='planned' AND rd.expires_at>?))`
	for _, c := range []struct {
		where, outcome, detail string
		args                   []any
	}{
		{`rd.state='succeeded'`, RollbackApplied, "", nil},
		{interrupted, RollbackFailed, RollbackInterrupted, []any{now}},
	} {
		ids, err := t.validationIDs(ctx, tx, undecided+c.where+` ORDER BY v.started_at,v.deployment_id`, c.args...)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := t.markRollbackOutcome(ctx, tx, id, c.outcome, c.detail, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateOne runs an UPDATE that must change exactly one row; none is ErrNotFound.
func updateOne(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

func (t *tenancyStore) validationIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, t.store.rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
