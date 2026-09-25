package api

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/google/uuid"
)

// validationActor is who a validation's inspections act as on the wire. A grant's actor must be an
// identifier ([A-Za-z0-9_-]), so it is not the audit rows' "system".
const validationActor = "system-validation"

// validationLoop is the validation loop's in-memory half; every row is durable.
type validationLoop struct {
	tick sync.Mutex // one tick at a time
	// Tests only: the tick interval (zero is store.ValidationPoll) and the clock (nil is time.Now).
	interval time.Duration
	now      func() time.Time
}

// RunValidations watches every settled apply until ctx ends (docs/application-schema.md, Health
// validation). A tick runs to its end, a rollback included, so done closes only between ticks and
// runServer can wait on it before the store closes.
func (s *Server) RunValidations(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	interval := s.validations.interval
	if interval == 0 {
		interval = store.ValidationPoll
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			now := time.Now()
			if s.validations.now != nil {
				now = s.validations.now()
			}
			s.validationTick(ctx, now)
		}
	}
}

// validationTick works through the pending validations oldest first. Shutdown stops it between
// rows; the rows resume on the next start.
func (s *Server) validationTick(ctx context.Context, now time.Time) {
	s.validations.tick.Lock()
	defer s.validations.tick.Unlock()
	if s.stopping.Load() {
		return
	}
	// A rollback whose outcome write failed is decided from its deployment, pausing its policy.
	if err := s.store.Tenancy().DecideNamedRollbacks(ctx); err != nil {
		log.Printf("[VALIDATION] deciding named rollbacks: %v", err)
	}
	pending, err := s.store.Tenancy().PendingValidations(ctx)
	if err != nil {
		log.Printf("[VALIDATION] pending validations unreadable: %v", err)
		return
	}
	for _, p := range pending {
		if ctx.Err() != nil || s.stopping.Load() {
			return
		}
		func() {
			// One row's panic must not end the loop for every other deployment.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[VALIDATION] deployment %s panicked: %v", p.DeploymentID, r)
				}
			}()
			s.validate(ctx, p, now)
		}()
	}
}

// validate advances one validation at now: take the baseline once grace has passed, judge each
// poll, finish on a terminal verdict, and decide a rollback when one is due.
func (s *Server) validate(ctx context.Context, p store.PendingValidation, now time.Time) {
	if p.Phase == store.PhaseDone { // a rollback decision still owed, from before a restart
		s.rollback(context.WithoutCancel(ctx), p)
		return
	}
	if p.Released {
		s.finishValidation(ctx, p, store.VerdictChanged, store.ValidationDetailReleased)
		return
	}
	if p.Phase == store.PhaseGrace && now.Before(p.StartedAt.Add(store.ValidationGrace)) {
		return
	}
	// Past the window by a further grace with no complete observation: give up.
	late := !now.Before(p.ObserveUntil.Add(store.ValidationGrace))
	switch {
	case !p.Health:
		s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailNoHealth)
		return
	case !s.Connected(p.EndpointID):
		// Offline waits, at the baseline too: only a host unseen for the whole window is unverifiable.
		if late {
			s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailUnobserved)
		}
		return
	case p.Phase == store.PhaseGrace && !now.Before(p.ObserveUntil):
		// No baseline by the window's end: one taken now would judge the window from a single poll.
		s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailUnobserved)
		return
	}
	obs, invalid := s.observeServices(ctx, p)
	if ctx.Err() != nil || s.stopping.Load() {
		return // a poll cut short by shutdown says nothing about the host
	}
	if invalid {
		s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailInvalid)
		return
	}
	baseline := p.Baseline
	if p.Phase == store.PhaseGrace {
		var ok bool
		if baseline, ok = store.BaselineOf(obs); !ok {
			if late {
				s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailUnobserved)
			}
			return
		}
		if err := s.store.Tenancy().BeginObservation(ctx, p.DeploymentID, baseline); err != nil {
			log.Printf("[VALIDATION] deployment %s: recording the baseline: %v", p.DeploymentID, err)
			return
		}
	}
	verdict, detail := store.Judge(obs, baseline, !now.Before(p.ObserveUntil))
	switch {
	case verdict != "":
		s.finishValidation(ctx, p, verdict, detail)
	case late:
		s.finishValidation(ctx, p, store.VerdictUnverifiable, store.ValidationDetailUnobserved)
	}
}

// observeServices inspects every settled container the store has not already placed as gone or
// replaced, one at a time under the plan's inspection budget. invalid: an answer failed validation.
func (s *Server) observeServices(ctx context.Context, p store.PendingValidation) (obs []store.Observation, invalid bool) {
	ctx, cancel := context.WithTimeout(ctx, planInspectionBudget)
	defer cancel()
	for _, svc := range p.Services {
		o := store.Observation{Service: svc.Service, Presence: svc.Presence}
		// A spent budget sends no expired grant: the service stays unobserved this poll.
		if (svc.Presence == store.PresencePresent || svc.Presence == store.PresenceUnknown) && ctx.Err() == nil {
			target := protocol.InspectionTarget{ContainerID: svc.ContainerID, ImageID: svc.ImageID, CreatedUnix: svc.CreatedUnix}
			in, err := s.observe(ctx, p.EndpointID, validationActor, p.OrganizationID, target, true, func() bool { return true })
			if errors.Is(err, errInspectionInvalid) {
				return nil, true
			}
			if err == nil {
				o.Inspection = &in
			}
		}
		obs = append(obs, o)
	}
	return obs, false
}

// finishValidation records the verdict and, when the store says a decision is due, decides the
// rollback, uncancelled, so a shutdown waits for it.
func (s *Server) finishValidation(ctx context.Context, p store.PendingValidation, verdict, detail string) {
	decide, err := s.store.Tenancy().FinishValidation(ctx, p.DeploymentID, verdict, detail)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("[VALIDATION] deployment %s: recording %s: %v", p.DeploymentID, verdict, err)
		}
		return
	}
	log.Printf("[VALIDATION] deployment %s: %s", p.DeploymentID, verdict)
	if decide {
		s.rollback(context.WithoutCancel(ctx), p)
	}
}

// rollback decides and, when eligible, performs the rollback of a failed automated update, then
// records the outcome, which pauses the policy.
func (s *Server) rollback(ctx context.Context, p store.PendingValidation) {
	outcome, detail := s.performRollback(ctx, p)
	if err := s.store.Tenancy().MarkRollbackOutcome(ctx, p.DeploymentID, outcome, detail); err != nil {
		log.Printf("[VALIDATION] deployment %s: recording rollback %s: %v", p.DeploymentID, outcome, err)
		return
	}
	log.Printf("[VALIDATION] deployment %s: rollback %s %s", p.DeploymentID, outcome, detail)
}

// performRollback is the rollback as the policy's creator under a fresh correlation ID: eligibility
// in the store, then a plan pinned to the prior images, the apply and the frame, exactly as a
// policy run sends one. The rollback is named on the validation before it is applied.
func (s *Server) performRollback(ctx context.Context, p store.PendingValidation) (outcome, detail string) {
	ts := s.store.Tenancy()
	a := store.TenantAccess{ActorID: p.CreatedBy, OrganizationID: p.OrganizationID, EnvironmentID: p.EnvironmentID, CorrelationID: uuid.NewString()}
	app := p.ApplicationID
	rb, reason, err := ts.RollbackTarget(ctx, a, app, p.DeploymentID)
	if err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	if reason != "" {
		return store.RollbackIneligible, reason
	}
	// Offline leaves no rollback plan behind to occupy the endpoint.
	if !s.Connected(p.EndpointID) {
		return store.RollbackFailed, "endpoint_offline"
	}
	ep, err := ts.ReadEndpoint(ctx, a, p.EndpointID)
	if err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	pre, err := ts.PreflightApplication(ctx, a, app)
	if err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	key, private := s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate
	maxFrame := maxFrameBytes(ep.Capabilities)
	req := store.PlanRequest{InstanceID: rb.InstanceID, MappingVersion: rb.MappingVersion, Revision: rb.Revision, Confirm: rb.Project, PinImages: rb.Images, MaxFrameBytes: maxFrame,
		Inspections: s.inspectForPlan(ctx, a, ep, pre, func() {}, s.policyInspectionAllowed(a, ep.ID))}
	d, err := ts.PlanDeployment(ctx, a, app, req, nil, key, private)
	var blocked *store.PreflightBlockedError
	if errors.As(err, &blocked) {
		return store.RollbackFailed, joinCodes(blocked.Blockers, 255)
	}
	if err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	if err := ts.MarkRollbackPlanned(ctx, p.DeploymentID, d.ID); err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	if !s.Connected(d.EndpointID) {
		return store.RollbackFailed, "endpoint_offline"
	}
	applied, frame, err := ts.ApplyPolicyDeployment(ctx, a, p.PolicyID, "", app, d.ID, d.Plan.Project, key, maxFrame)
	if errors.Is(err, store.ErrPolicyChanged) {
		return store.RollbackFailed, "policy_changed"
	}
	if err != nil {
		return store.RollbackFailed, rollbackCode(err)
	}
	if !s.agents.deliver(applied.EndpointID, envelope(protocol.TypeDeploymentApply, frame)) {
		if err := ts.FailDeployment(ctx, applied.ID, "the endpoint disconnected before the deployment was sent"); err != nil {
			log.Printf("[VALIDATION] deployment %s: recording an unsent rollback: %v", applied.ID, err)
		}
		return store.RollbackFailed, store.RollbackNotSent
	}
	return store.RollbackApplied, ""
}

// rollbackCode names a refusal in a rollback's detail: lost authority is creator_lost, anything
// else the policy scheduler's code.
func rollbackCode(err error) string {
	if errors.Is(err, store.ErrForbidden) {
		return store.RollbackCreatorLost
	}
	_, _, code := policyFailure(err)
	return code
}
