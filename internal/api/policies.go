package api

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/google/uuid"
)

const (
	// policyRunsServerWide bounds automated runs in flight; each also holds its endpoint alone.
	policyRunsServerWide = 2
	// policySlotWait is how long a run waits for a registry slot before skipping its window.
	policySlotWait = 30 * time.Second
)

// policyScheduler is the scheduler's in-memory half: which runs are in flight and which open
// windows a tick passed over as busy. Everything that must survive a restart is in the database.
type policyScheduler struct {
	tick     sync.Mutex            // policyTick: one tick at a time
	busy     map[policyWindow]bool // open occurrences passed over as busy; under tick
	runs     sync.WaitGroup
	mu       sync.Mutex
	inFlight map[string]string // policy ID -> endpoint ID; under mu
	// Tests only: the tick interval (zero is one minute), the clock (nil is time.Now), and a
	// hook fired once a run's row is open (nil in production; exercises the panic recover below).
	interval  time.Duration
	now       func() time.Time
	panicHook func()
}

// policyWindow is one occurrence of one policy.
type policyWindow struct {
	policy     string
	occurrence time.Time
}

// admit reserves a run for policy on endpoint, or refuses: the policy already running, another
// run on the endpoint, or policyRunsServerWide in flight. "" (not adopted) holds no endpoint.
func (p *policyScheduler) admit(policy, endpoint string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight == nil {
		p.inFlight = map[string]string{}
	}
	if _, running := p.inFlight[policy]; running || len(p.inFlight) >= policyRunsServerWide {
		return false
	}
	for _, e := range p.inFlight {
		if endpoint != "" && e == endpoint {
			return false
		}
	}
	p.inFlight[policy] = endpoint
	return true
}

func (p *policyScheduler) release(policy string) {
	p.mu.Lock()
	delete(p.inFlight, policy)
	p.mu.Unlock()
}

func (p *policyScheduler) running(policy string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.inFlight[policy]
	return ok
}

// RunPolicies runs the update-policy scheduler until ctx ends: a tick a minute, each run in its
// own goroutine. done closes only once no run is in flight, so runServer can wait on it before
// the store closes.
func (s *Server) RunPolicies(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	defer s.policies.runs.Wait()
	interval := s.policies.interval
	if interval == 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			if s.policies.now != nil {
				now = s.policies.now()
			}
			s.policyTick(ctx, now)
		}
	}
}

// policyTick records the windows that closed without a run, then starts the runs that are due,
// in created_at order, without waiting for them. Ticks never overlap.
func (s *Server) policyTick(ctx context.Context, now time.Time) {
	p := &s.policies
	p.tick.Lock()
	defer p.tick.Unlock()
	if s.stopping.Load() {
		return
	}
	ts := s.store.Tenancy()
	due, err := ts.SchedulePolicies(ctx, now)
	if err != nil {
		log.Printf("[POLICY] schedule unreadable: %v", err)
		return
	}
	open := map[policyWindow]bool{}
	for _, sp := range due {
		if sp.Open {
			open[policyWindow{sp.Policy.ID, sp.Occurrence}] = true
		}
	}
	// A window passed over as busy until it ended is skipped_busy, whatever the schedule now
	// lists; recorded first, so the missed step below finds it has its row.
	for w := range p.busy {
		if open[w] {
			continue
		}
		if s.skipWindow(ctx, w, store.RunSkippedBusy, store.PolicyDetailBusy) || now.Sub(w.occurrence) > 24*time.Hour {
			delete(p.busy, w)
		}
	}
	for _, sp := range due {
		if sp.Missed && !p.running(sp.Policy.ID) {
			s.skipWindow(ctx, policyWindow{sp.Policy.ID, sp.Previous}, store.RunSkippedMissed, store.PolicyDetailMissed)
		}
	}
	for _, sp := range due {
		if !sp.Open || p.running(sp.Policy.ID) {
			continue
		}
		if sp.EndpointBusy || !p.admit(sp.Policy.ID, sp.EndpointID) {
			if p.busy == nil {
				p.busy = map[policyWindow]bool{}
			}
			p.busy[policyWindow{sp.Policy.ID, sp.Occurrence}] = true
			continue
		}
		delete(p.busy, policyWindow{sp.Policy.ID, sp.Occurrence})
		p.runs.Add(1)
		go func(sp store.ScheduledPolicy) {
			defer p.runs.Done()
			defer p.release(sp.Policy.ID)
			// A panic here would otherwise end RunPolicies mid-tick, taking every other policy's
			// schedule down with it. openedRun is set the moment the run's row exists, so a panic
			// after that point still finishes the row instead of leaving it open forever.
			var openedRun string
			runCtx := context.WithoutCancel(ctx)
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[POLICY] policy run %s panicked: %v", sp.Policy.ID, r)
					if openedRun == "" {
						return
					}
					if err := s.store.Tenancy().FinishPolicyRun(runCtx, openedRun, store.RunFailed, "", "panic"); err != nil {
						log.Printf("[POLICY] policy %s run %s: recording panic: %v", sp.Policy.ID, openedRun, err)
					}
				}
			}()
			// A run in flight finishes: shutdown waits for it rather than cut an apply in half.
			s.runPolicy(runCtx, sp, &openedRun)
		}(sp)
	}
}

// skipWindow records w as a finished skip. It reports whether w is settled: recorded now, already
// run or recorded, or its policy paused or deleted.
func (s *Server) skipWindow(ctx context.Context, w policyWindow, outcome, detail string) bool {
	err := s.store.Tenancy().SkipPolicyWindow(ctx, w.policy, w.occurrence, outcome, detail)
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) && !errors.Is(err, store.ErrNotFound) {
		log.Printf("[POLICY] policy %s: recording %s: %v", w.policy, outcome, err)
		return false
	}
	return true
}

// runPolicy opens the window's run row, does the work as the policy's last editor under one
// correlation ID, and records the outcome. openedRun is set to the run's ID the moment it opens,
// so the caller's panic recover can still finish the row if performPolicyRun panics.
func (s *Server) runPolicy(ctx context.Context, sp store.ScheduledPolicy, openedRun *string) {
	pol := sp.Policy
	a := store.TenantAccess{ActorID: pol.CreatedBy, OrganizationID: pol.OrganizationID, EnvironmentID: pol.EnvironmentID, CorrelationID: uuid.NewString()}
	ts := s.store.Tenancy()
	run, err := ts.BeginPolicyRun(ctx, pol.ID, sp.Occurrence, a.CorrelationID)
	if errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrNotFound) {
		return // the window has its run, or the policy was paused or deleted since the tick
	}
	if err != nil {
		log.Printf("[POLICY] policy %s: opening a run: %v", pol.ID, err)
		return
	}
	*openedRun = run
	if s.policies.panicHook != nil {
		hook := s.policies.panicHook
		s.policies.panicHook = nil
		hook()
	}
	outcome, deployment, detail := s.performPolicyRun(ctx, a, pol, run)
	err = ts.FinishPolicyRun(ctx, run, outcome, deployment, detail)
	if errors.Is(err, store.ErrNotFound) {
		log.Printf("[POLICY] policy %s run %s: the policy was deleted during the run", pol.ID, run)
		return
	}
	if err != nil {
		log.Printf("[POLICY] policy %s run %s: recording %s: %v", pol.ID, run, outcome, err)
		return
	}
	log.Printf("[POLICY] policy %s window %s: %s", pol.ID, sp.Occurrence.Format(time.RFC3339), outcome)
}

// performPolicyRun is one run's work as a: authorize, check, plan and, in apply mode, apply and
// send, each step re-authorized by the store. It returns the outcome, the deployment it made and
// a fixed detail.
func (s *Server) performPolicyRun(ctx context.Context, a store.TenantAccess, pol store.UpdatePolicy, run string) (outcome, deployment, detail string) {
	ts := s.store.Tenancy()
	app := pol.ApplicationID
	if err := ts.CheckImageUpdateAccess(ctx, a, app); err != nil {
		return policyFailure(err)
	}
	done, ok := s.holdApplication(a, app)
	if !ok {
		return store.RunSkippedBusy, "", "check_in_progress"
	}
	defer done()
	release, ok := s.waitRegistrySlot(a.OrganizationID, policySlotWait)
	if !ok {
		return store.RunSkippedBusy, "", "too_many_checks"
	}
	defer release()
	key, private := s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate
	check, err := ts.CheckImageUpdates(ctx, a, app, s.resolver(), key, private)
	if err != nil {
		return policyFailure(err)
	}
	var update []string
	registryDetail := ""
	for _, c := range check.Services {
		switch {
		case c.Verdict == "update_available":
			update = append(update, c.Service)
		case c.Verdict == "registry_error" && registryDetail == "":
			registryDetail = c.Detail
		}
	}
	switch {
	case len(update) == 0 && registryDetail != "":
		return store.RunFailed, "", registryDetail
	case len(update) == 0:
		return store.RunNoUpdate, "", ""
	}
	pre, err := ts.PreflightApplication(ctx, a, app)
	if err != nil {
		return policyFailure(err)
	}
	// An apply run that cannot send leaves no plan behind to occupy the endpoint.
	if pol.Mode == store.PolicyModeApply && !s.Connected(pre.EndpointID) {
		return store.RunFailed, "", "endpoint_offline"
	}
	ep, err := ts.ReadEndpoint(ctx, a, pre.EndpointID)
	if err != nil {
		return policyFailure(err)
	}
	instance, err := ts.ReadApplicationInstance(ctx, a, app, pre.InstanceID)
	if err != nil {
		return policyFailure(err)
	}
	maxFrame := maxFrameBytes(ep.Capabilities)
	req := store.PlanRequest{InstanceID: pre.InstanceID, MappingVersion: pre.MappingVersion, Revision: pre.Revision, Confirm: instance.Project, Update: update, MaxFrameBytes: maxFrame,
		Inspections: s.inspectForPlan(ctx, a, ep, pre, func() {}, s.policyInspectionAllowed(a, ep.ID))}
	d, err := ts.PlanDeployment(ctx, a, app, req, s.resolver(), key, private)
	var blocked *store.PreflightBlockedError
	if errors.As(err, &blocked) {
		return store.RunBlocked, "", joinCodes(blocked.Blockers, 255)
	}
	if err != nil {
		return policyFailure(err)
	}
	if pol.Mode == store.PolicyModePlanOnly {
		return store.RunPlanned, d.ID, ""
	}
	// From here as handleApplyDeployment: the row is applying before the frame leaves, and a
	// frame that cannot be queued fails the row.
	if !s.Connected(d.EndpointID) {
		return store.RunFailed, d.ID, "endpoint_offline"
	}
	// The policy is re-read inside the apply's transaction: one deleted, paused, switched to
	// plan_only or saved by someone else since the tick leaves its plan for a click.
	applied, frame, err := ts.ApplyPolicyDeployment(ctx, a, pol.ID, app, d.ID, d.Plan.Project, key, maxFrame)
	if errors.Is(err, store.ErrPolicyChanged) {
		return store.RunPlanned, d.ID, "policy_changed"
	}
	if err != nil {
		failed, _, why := policyFailure(err)
		return failed, d.ID, why
	}
	// Named before the frame leaves: a settle that beats FinishPolicyRun still finds this run and
	// validates the deployment as automated. Unnamed, it would be validated as a manual apply and
	// never rolled back, so it is not sent.
	if err := ts.AttachPolicyRunDeployment(ctx, run, applied.ID); err != nil {
		log.Printf("[POLICY] run %s: naming deployment %s: %v", run, applied.ID, err)
		if err := ts.FailDeployment(ctx, applied.ID, "the deployment could not be recorded on its policy run"); err != nil {
			log.Printf("[POLICY] deployment %s: recording an unsent frame: %v", applied.ID, err)
		}
		return store.RunFailed, applied.ID, "error"
	}
	if !s.agents.deliver(applied.EndpointID, envelope(protocol.TypeDeploymentApply, frame)) {
		if err := ts.FailDeployment(ctx, applied.ID, "the endpoint disconnected before the deployment was sent"); err != nil {
			log.Printf("[POLICY] deployment %s: recording an unsent frame: %v", applied.ID, err)
		}
		return store.RunFailed, applied.ID, "not_sent"
	}
	return store.RunApplied, applied.ID, ""
}

// policyInspectionAllowed re-checks during each inspection that the policy's actor may still read
// the endpoint: inspectionAllowed without a session to re-authenticate.
func (s *Server) policyInspectionAllowed(a store.TenantAccess, endpoint string) func(context.Context) bool {
	return func(ctx context.Context) bool {
		ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		return s.store.Tenancy().StillAllowed(ctx, a, permissions.EndpointRead, endpoint) == nil
	}
}

// policyErrorCodes name a store refusal in a run's detail; anything else is "error".
var policyErrorCodes = []struct {
	err  error
	code string
}{
	{store.ErrNotFound, "not_adopted"},
	{store.ErrMappingRequired, "mapping_required"},
	{store.ErrAdoptionChanged, "adoption_changed"},
	{store.ErrDeploymentInProgress, "deployment_in_progress"},
	{store.ErrEndpointOffline, "endpoint_offline"},
	{store.ErrInvalid, "invalid"},
}

// policyFailure maps a store refusal to the run's outcome: lost authority pauses the policy,
// anything else fails the window with a fixed code.
func policyFailure(err error) (outcome, deployment, detail string) {
	if errors.Is(err, store.ErrForbidden) {
		return store.RunPaused, "", store.PolicyDetailCreatorLost
	}
	for _, c := range policyErrorCodes {
		if errors.Is(err, c.err) {
			return store.RunFailed, "", c.code
		}
	}
	return store.RunFailed, "", "error"
}

// joinCodes joins whole codes with "," within limit bytes, dropping those that do not fit.
func joinCodes(codes []string, limit int) string {
	out := ""
	for _, c := range codes {
		next := c
		if out != "" {
			next = out + "," + c
		}
		if len(next) > limit {
			break
		}
		out = next
	}
	return out
}
