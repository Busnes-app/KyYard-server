package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// validationCaps is an agent that inspects with verdicts and health, applies and pulls.
var validationCaps = []string{protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull, protocol.CapabilityContainerInspect, protocol.CapabilityContainerInspectVerdict, protocol.CapabilityContainerInspectHealth}

const (
	afterGrace  = store.ValidationGrace + time.Second
	afterWindow = store.ValidationGrace + store.ValidationWindow + time.Second
	// afterLate is observe_until plus a grace: a window with no complete observation gives up.
	afterLate = 2*store.ValidationGrace + store.ValidationWindow
)

var (
	hostImage      = "sha256:" + strings.Repeat("b", 64) // newPlanHost's image, tagged nginx:1
	hostDigest     = "nginx@sha256:" + strings.Repeat("c", 64)
	priorContainer = strings.Repeat("e", 64)
	newContainer   = strings.Repeat("f", 64)
	newImage       = "sha256:" + strings.Repeat("9", 64)
	newDigest      = "sha256:" + strings.Repeat("d", 64)
	generationSeq  atomic.Uint64
)

// nextGeneration is an inventory generation above every one sent before, inside the store's skew.
func nextGeneration() uint64 { return uint64(time.Now().Unix()) + 10 + generationSeq.Add(1) }

// observations answers the plan-time inspector, which validation shares: every container running,
// verified and healthy with no restarts unless on changes it.
type observations struct {
	mu    sync.Mutex
	by    map[string]func(*protocol.ContainerInspection) error
	stall *stall // answers stall.container, when set
}

func (o *observations) on(container string, f func(*protocol.ContainerInspection) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.by == nil {
		o.by = map[string]func(*protocol.ContainerInspection) error{}
	}
	o.by[container] = f
}

func (o *observations) inspect(ctx context.Context, target protocol.InspectionTarget) (protocol.ContainerInspection, error) {
	in := verifiedObservation(target)
	in.Health = "healthy"
	o.mu.Lock()
	f, st := o.by[target.ContainerID], o.stall
	o.mu.Unlock()
	if st != nil && target.ContainerID == st.container {
		return protocol.ContainerInspection{}, st.wait(ctx)
	}
	if f != nil {
		if err := f(&in); err != nil {
			return protocol.ContainerInspection{}, err
		}
	}
	return in, nil
}

func health(status string) func(*protocol.ContainerInspection) error {
	return func(in *protocol.ContainerInspection) error { in.Health = status; return nil }
}

// validationHost is a planHost whose web service already ran one succeeded manual apply of
// revision 1 on the host image (prior): the deployment a rollback returns to. Its agent is online
// with health, every inspection goes through obs, and every store call through faults, installed
// before the agent connects so no handler reads the store while it is swapped.
type validationHost struct {
	planHost
	faults  *faultyTenancy
	sock    *agentSocket
	ctx     context.Context
	obs     *observations
	prior   string
	running protocol.Container // the web container the last settle reported
}

func newValidationHost(t *testing.T) *validationHost {
	t.Helper()
	v := &validationHost{planHost: newPlanHost(t, validationCaps, "web"), obs: &observations{}}
	v.faults = &faultyTenancy{TenancyStore: v.st.Tenancy()}
	api.SetStoreForTest(v.s, faultyStore{Store: v.st, t: v.faults})
	api.SetPlanInspectorForTest(v.s, v.obs.inspect)
	v.sock, v.ctx = v.online(t, validationCaps)
	v.prior = v.applyByHand(t)
	v.settle(t, v.prior, priorContainer, hostImage, "", []protocol.Image{{ID: hostImage, Tags: []string{"nginx:1"}, Digests: []string{hostDigest}}})
	return v
}

// applyByHand plans and applies the latest revision as the planner and reads the frame.
func (v *validationHost) applyByHand(t *testing.T) string {
	t.Helper()
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "POST", v.deployments, v.planBody, 201)), &d); err != nil {
		t.Fatal(err)
	}
	v.do(t, "POST", v.deployments+"/"+d.ID+"/apply", `{"confirm":"shop"}`, 202)
	if req := v.frame(t); req.Deployment != d.ID {
		t.Fatalf("frame for %s, want %s", req.Deployment, d.ID)
	}
	return d.ID
}

// frame reads the next frame as a deployment request.
func (v *validationHost) frame(t *testing.T) protocol.DeploymentRequest {
	t.Helper()
	f := readEnvelope(t, v.ctx, v.sock.conn)
	var req protocol.DeploymentRequest
	if f.Type != protocol.TypeDeploymentApply || json.Unmarshal(f.Payload, &req) != nil {
		t.Fatalf("expected a deployment frame, got %s", f.Type)
	}
	return req
}

// settle answers deployment id as the agent would: web replaced by container on image (digest for
// a pulled service), then an inventory showing it beside images.
func (v *validationHost) settle(t *testing.T, id, container, image, digest string, images []protocol.Image) {
	t.Helper()
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments+"/"+id, "", 200)), &d); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Truncate(time.Second)
	res := protocol.DeploymentResult{Deployment: id, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{{Service: "web", ContainerID: container, ImageID: image, CreatedUnix: created.Unix(), ImageDigest: digest}}}
	if err := v.st.Tenancy().SettleDeployment(context.Background(), v.ag.id, res); err != nil {
		t.Fatal(err)
	}
	v.running = protocol.Container{ID: container, Name: "shop-web", ImageID: image, State: "running", ComposeProject: "shop", CreatedAt: created, Mounts: []protocol.Mount{}}
	v.inventory(t, images)
}

// inventory sends the running web container beside images.
func (v *validationHost) inventory(t *testing.T, images []protocol.Image) {
	t.Helper()
	raw, _ := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{v.running}, Images: images})
	if _, err := v.st.Tenancy().AcceptInventory(context.Background(), v.ag.id, nextGeneration(), time.Now(), raw); err != nil {
		t.Fatal(err)
	}
}

// automate saves an apply-mode policy, offers a newer registry digest and runs its window: the
// policy applies the update and the agent settles it on newContainer running newImage. The host
// keeps the old image, untagged.
func (v *validationHost) automate(t *testing.T) (string, *store.UpdatePolicy) {
	t.Helper()
	v.do(t, "PUT", "/api/organizations/a/registry-policy", `{"anonymous_pull_enabled":true}`, 204)
	api.SetDigestResolverForTest(v.s, &fakeDigests{digest: newDigest})
	p := v.policy(t, store.PolicyModeApply)
	v.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := v.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunApplied {
		t.Fatalf("runs: %+v", runs)
	}
	if req := v.frame(t); req.Deployment != runs[0].DeploymentID {
		t.Fatalf("frame for %s, want %s", req.Deployment, runs[0].DeploymentID)
	}
	v.settle(t, runs[0].DeploymentID, newContainer, newImage, newDigest, []protocol.Image{{ID: hostImage, Digests: []string{hostDigest}}, {ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{"nginx@" + newDigest}}})
	return runs[0].DeploymentID, p
}

func (v *validationHost) validation(t *testing.T, deployment string) *store.Validation {
	t.Helper()
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments+"/"+deployment, "", 200)), &d); err != nil {
		t.Fatal(err)
	}
	if d.Validation == nil {
		t.Fatalf("deployment %s has no validation", deployment)
	}
	return d.Validation
}

// at runs one validation tick at the validation's start plus offset.
func (v *validationHost) at(t *testing.T, deployment string, offset time.Duration) {
	t.Helper()
	api.ValidationTickForTest(v.s, v.validation(t, deployment).StartedAt.Add(offset))
}

func (v *validationHost) policyNow(t *testing.T) *store.UpdatePolicy {
	t.Helper()
	p, _, err := v.st.Tenancy().ReadUpdatePolicy(context.Background(), v.as("usr_planner"), v.appID())
	if err != nil || p == nil {
		t.Fatalf("policy: %+v %v", p, err)
	}
	return p
}

func (v *validationHost) deploymentCount(t *testing.T) int {
	t.Helper()
	var list []store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments, "", 200)), &list); err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func (v *validationHost) pauseRows(t *testing.T) int {
	t.Helper()
	n := 0
	for _, r := range policyAuditRows(t, v.planHost, v.admin) {
		if r.Action == store.AuditPolicyPaused {
			n++
		}
	}
	return n
}

func TestValidationRecordsAHealthyUpdate(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	got := v.validation(t, id)
	if !got.Automated || got.IsRollback || got.PolicyRunID == "" || got.Phase != store.PhaseGrace || !got.ObserveUntil.Equal(got.StartedAt.Add(store.ValidationGrace+store.ValidationWindow)) {
		t.Fatalf("opened: %+v", got)
	}
	api.ValidationTickForTest(v.s, got.StartedAt.Add(store.ValidationGrace-time.Second))
	if got := v.validation(t, id); got.Phase != store.PhaseGrace {
		t.Fatalf("observed during grace: %+v", got)
	}
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Phase != store.PhaseObserving || got.Verdict != "" {
		t.Fatalf("after grace: %+v", got)
	}
	v.at(t, id, afterWindow)
	got = v.validation(t, id)
	if got.Phase != store.PhaseDone || got.Verdict != store.VerdictHealthy || got.Rollback != nil || got.FinishedAt == nil {
		t.Fatalf("finished: %+v", got)
	}
	if p := v.policyNow(t); p.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", p)
	}
	audited := false
	for _, r := range policyAuditRows(t, v.planHost, v.admin) {
		if r.Action == store.AuditValidation && r.Resource == v.appID()+"/deployments/"+id && r.UserID == "system" && r.Result == "success" && r.CorrelationID == got.CorrelationID {
			audited = true
		}
	}
	if !audited {
		t.Fatal("no application.validation row")
	}
}

// An unhealthy update is rolled back to the prior apply's image, pinned by ID, as the policy's
// creator; the policy pauses with one audit row (Review Focus 1).
func TestValidationRollsBackAnUnhealthyUpdate(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	req := v.frame(t)
	if req.Revision != 1 || len(req.Services) != 1 || req.Services[0].ImageID != hostImage || req.Services[0].Pull != nil || req.Services[0].Replaces.ContainerID != newContainer {
		t.Fatalf("rollback frame: %+v", req)
	}
	got := v.validation(t, id)
	if got.Verdict != store.VerdictUnhealthy || got.Detail != "web" || got.Rollback == nil || *got.Rollback != (store.ValidationRollback{DeploymentID: req.Deployment, Revision: 1, Outcome: store.RollbackApplied}) {
		t.Fatalf("validation: %+v %+v", got, got.Rollback)
	}
	var rb store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments+"/"+req.Deployment, "", 200)), &rb); err != nil {
		t.Fatal(err)
	}
	if rb.State != "applying" || rb.CreatedBy != "usr_planner" || rb.AppliedBy != "usr_planner" || rb.CorrelationID == got.CorrelationID || rb.Plan.Services[0].ImageID != hostImage {
		t.Fatalf("rollback deployment: %+v", rb)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyPaused || pol.PausedReason != store.ValidationReasonRolledBack {
		t.Fatalf("policy: %+v", pol)
	}
	if n := v.pauseRows(t); n != 1 {
		t.Fatalf("pause rows: %d", n)
	}
	if runs := v.runs(t, "usr_planner"); runs[0].Outcome != store.RunApplied || runs[0].DeploymentID != id {
		t.Fatalf("the run changed: %+v", runs[0])
	}
}

func TestValidationStopsWhenThePriorImagesAreGone(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	v.inventory(t, []protocol.Image{{ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{"nginx@" + newDigest}}})
	v.at(t, id, afterGrace)
	got := v.validation(t, id)
	if got.Verdict != store.VerdictUnhealthy || got.Rollback == nil || *got.Rollback != (store.ValidationRollback{Outcome: store.RollbackIneligible, Detail: store.RollbackPriorImagesMissing}) {
		t.Fatalf("validation: %+v %+v", got, got.Rollback)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyPaused || pol.PausedReason != store.ValidationReasonNotRolledBack+store.RollbackPriorImagesMissing {
		t.Fatalf("policy: %+v", pol)
	}
	if n := v.deploymentCount(t); n != 2 {
		t.Fatalf("deployments: %d", n)
	}
}

// A manual apply gets its verdict and nothing else.
func TestValidationOfAManualApplyOnlyRecords(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(priorContainer, health("unhealthy"))
	v.at(t, v.prior, afterGrace)
	got := v.validation(t, v.prior)
	if got.Automated || got.Verdict != store.VerdictUnhealthy || got.Detail != "web" || got.Rollback != nil {
		t.Fatalf("manual: %+v", got)
	}
	if n := v.deploymentCount(t); n != 1 {
		t.Fatalf("deployments: %d", n)
	}
}

// An agent offline at the baseline is waited for: back before the window's end, the window runs.
func TestValidationWaitsForAnOfflineHost(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	v.sock.conn.CloseNow()
	waitFor(t, func() bool { return !v.s.Connected(v.ag.id) })
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Phase != store.PhaseGrace || got.Verdict != "" {
		t.Fatalf("offline at the baseline ended it: %+v", got)
	}
	v.sock, v.ctx = v.online(t, validationCaps)
	v.at(t, id, afterGrace+store.ValidationPoll)
	if got := v.validation(t, id); got.Phase != store.PhaseObserving {
		t.Fatalf("no baseline after the reconnect: %+v", got)
	}
	v.at(t, id, afterWindow)
	if got := v.validation(t, id); got.Verdict != store.VerdictHealthy {
		t.Fatalf("after the reconnect: %+v", got)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", pol)
	}
}

// Offline for the whole window, and its grace after, is unverifiable.
func TestValidationOfAnOfflineHostIsUnverifiable(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	v.sock.conn.CloseNow()
	waitFor(t, func() bool { return !v.s.Connected(v.ag.id) })
	v.at(t, id, afterGrace)
	v.at(t, id, afterLate-time.Second)
	if got := v.validation(t, id); got.Verdict != "" {
		t.Fatalf("given up before the window's grace ended: %+v", got)
	}
	v.at(t, id, afterLate)
	got := v.validation(t, id)
	if got.Verdict != store.VerdictUnverifiable || got.Detail != store.ValidationDetailUnobserved || got.Rollback != nil {
		t.Fatalf("offline: %+v", got)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyPaused || pol.PausedReason != store.ValidationReasonUnverified+store.ValidationDetailUnobserved {
		t.Fatalf("policy: %+v", pol)
	}
}

// Offline through the window and back just after it: no baseline is taken so late, since the next
// poll would judge the whole window healthy from it.
func TestValidationWithNoBaselineByTheWindowsEndIsUnverifiable(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	v.disconnect(t)
	v.at(t, id, afterGrace)
	v.sock, v.ctx = v.online(t, validationCaps)
	v.at(t, id, afterWindow)
	got := v.validation(t, id)
	if got.Verdict != store.VerdictUnverifiable || got.Detail != store.ValidationDetailUnobserved || got.Phase != store.PhaseDone || got.Rollback != nil {
		t.Fatalf("a baseline after observe_until: %+v", got)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyPaused || pol.PausedReason != store.ValidationReasonUnverified+store.ValidationDetailUnobserved {
		t.Fatalf("policy: %+v", pol)
	}
	v.noFrame(t)
}

// A policy run that fails after planning names its plan; an admin who applies that plan later
// makes a manual apply: validated, never rolled back.
func TestValidationOfAFailedRunsPlanAppliedByHandIsManual(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	v.faulty().refuseApply.Store(true)
	v.do(t, "PUT", "/api/organizations/a/registry-policy", `{"anonymous_pull_enabled":true}`, 204)
	api.SetDigestResolverForTest(v.s, &fakeDigests{digest: newDigest})
	v.policy(t, store.PolicyModeApply)
	v.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := v.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunFailed || runs[0].DeploymentID == "" {
		t.Fatalf("runs: %+v", runs)
	}
	v.faulty().refuseApply.Store(false)
	d := runs[0].DeploymentID
	v.do(t, "POST", v.deployments+"/"+d+"/apply", `{"confirm":"shop"}`, 202)
	if req := v.frame(t); req.Deployment != d {
		t.Fatalf("frame for %s, want %s", req.Deployment, d)
	}
	v.settle(t, d, newContainer, newImage, newDigest, []protocol.Image{{ID: hostImage, Digests: []string{hostDigest}}, {ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{"nginx@" + newDigest}}})
	if got := v.validation(t, d); got.Automated || got.PolicyRunID != "" {
		t.Fatalf("a hand-applied plan is automated: %+v", got)
	}
	v.at(t, d, afterGrace)
	if got := v.validation(t, d); got.Verdict != store.VerdictUnhealthy || got.Rollback != nil {
		t.Fatalf("hand-applied: %+v", got)
	}
	if n := v.deploymentCount(t); n != 2 {
		t.Fatalf("deployments: %d", n)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", pol)
	}
	v.noFrame(t)
}

func TestValidationWithoutTheHealthCapabilityIsUnverifiable(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	if err := v.st.Tenancy().SetEndpointCapabilities(context.Background(), v.ag.id, policyCaps); err != nil {
		t.Fatal(err)
	}
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Verdict != store.VerdictUnverifiable || got.Detail != store.ValidationDetailNoHealth {
		t.Fatalf("no health: %+v", got)
	}
}

func TestValidationOfAnInvalidInspectionIsUnverifiable(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, func(*protocol.ContainerInspection) error { return api.ErrInspectionInvalidForTest })
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Verdict != store.VerdictUnverifiable || got.Detail != store.ValidationDetailInvalid {
		t.Fatalf("invalid inspection: %+v", got)
	}
	if pol := v.policyNow(t); pol.PausedReason != store.ValidationReasonUnverified+store.ValidationDetailInvalid {
		t.Fatalf("policy: %+v", pol)
	}
}

// Over the real socket the loop's grant names the system actor in the wire's identifier form, and
// a health agent's answer becomes the baseline.
func TestValidationInspectsOverTheAgentSocketAsTheSystem(t *testing.T) {
	v := newValidationHost(t)
	api.SetPlanInspectorForTest(v.s, nil)
	start := v.validation(t, v.prior).StartedAt
	done := make(chan struct{})
	go func() { defer close(done); api.ValidationTickForTest(v.s, start.Add(afterGrace)) }()
	g := grant(t, v.ctx, v.sock)
	if g.Actor != "system-validation" || g.Validate(time.Now()) != nil || g.Target.ContainerID != priorContainer {
		t.Fatalf("grant: %+v", g)
	}
	in := verifiedObservation(g.Target)
	in.Health, in.RestartCount = "healthy", 2
	writeEnvelope(t, v.ctx, v.sock.conn, protocol.TypeInspectionResult, protocol.InspectionResult{Request: g.Request, Status: "ok", Result: &in})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick never finished")
	}
	pending, err := v.st.Tenancy().PendingValidations(context.Background())
	if err != nil || len(pending) != 1 || pending[0].Phase != store.PhaseObserving || pending[0].Baseline["web"] != (store.ServiceBaseline{ContainerID: priorContainer, RestartCount: 2}) {
		t.Fatalf("pending: %+v %v", pending, err)
	}
}

// A restart mid-window resumes the window; a rollback already dispatched is never sent again.
func TestValidationResumesAfterARestart(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	if _, err := v.st.Tenancy().ReconcileAfterStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := v.validation(t, id); got.Phase != store.PhaseObserving {
		t.Fatalf("the open window did not survive the restart: %+v", got)
	}
	v.at(t, id, afterWindow)
	if got := v.validation(t, id); got.Verdict != store.VerdictHealthy {
		t.Fatalf("resumed: %+v", got)
	}
}

func TestValidationNeverDispatchesARollbackTwice(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	first := v.frame(t)
	if _, err := v.st.Tenancy().ReconcileAfterStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	v.at(t, id, afterWindow)
	api.ValidationTickForTest(v.s, time.Now().Add(time.Hour))
	got := v.validation(t, id)
	if got.Rollback == nil || got.Rollback.DeploymentID != first.Deployment || got.Rollback.Outcome != store.RollbackApplied {
		t.Fatalf("rollback: %+v", got.Rollback)
	}
	if n := v.deploymentCount(t); n != 3 {
		t.Fatalf("deployments: %d", n)
	}
}

// Shutdown waits for a rollback in flight: done closes only once its frame is sent and recorded.
func TestRunValidationsWaitsForAnInFlightRollback(t *testing.T) {
	v := newValidationHost(t)
	entered, gate := make(chan struct{}, 1), make(chan struct{})
	var calls atomic.Int32
	v.obs.on(newContainer, func(in *protocol.ContainerInspection) error {
		in.Health = "unhealthy"
		if calls.Add(1) == 2 { // the first is the validation's poll, the second the rollback plan's
			entered <- struct{}{}
			<-gate
		}
		return nil
	})
	id, _ := v.automate(t)
	start := v.validation(t, id).StartedAt
	api.SetValidationClockForTest(v.s, 10*time.Millisecond, func() time.Time { return start.Add(afterGrace) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go v.s.RunValidations(ctx, done)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no rollback reached its plan")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("done closed with a rollback in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("done never closed")
	}
	req := v.frame(t)
	if got := v.validation(t, id); got.Rollback == nil || got.Rollback.Outcome != store.RollbackApplied || got.Rollback.DeploymentID != req.Deployment {
		t.Fatalf("rollback: %+v", got.Rollback)
	}
}

// Review Focus 2: a rollback's own failed validation is recorded and changes nothing else.
func TestValidationNeverRollsBackARollback(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	req := v.frame(t)
	back := strings.Repeat("7", 64)
	v.obs.on(back, health("unhealthy"))
	v.settle(t, req.Deployment, back, hostImage, "", []protocol.Image{{ID: hostImage, Digests: []string{hostDigest}}, {ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{"nginx@" + newDigest}}})
	if got := v.validation(t, req.Deployment); !got.IsRollback || got.Automated {
		t.Fatalf("rollback's validation: %+v", got)
	}
	v.at(t, req.Deployment, afterGrace)
	if got := v.validation(t, req.Deployment); got.Verdict != store.VerdictUnhealthy || got.Rollback != nil {
		t.Fatalf("rollback's verdict: %+v", got)
	}
	if n := v.deploymentCount(t); n != 3 {
		t.Fatalf("deployments: %d", n)
	}
	if pol := v.policyNow(t); pol.PausedReason != store.ValidationReasonRolledBack || v.pauseRows(t) != 1 {
		t.Fatalf("policy: %+v, pause rows %d", pol, v.pauseRows(t))
	}
}

// Review Focus 3: a manual apply inside an automated window makes the automated one changed.
func TestValidationOfAnUpdateReplacedByAManualApply(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	manual := v.applyByHand(t)
	v.settle(t, manual, strings.Repeat("6", 64), newImage, "", []protocol.Image{{ID: hostImage, Digests: []string{hostDigest}}, {ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{"nginx@" + newDigest}}})
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Verdict != store.VerdictChanged || got.Detail != "web" || got.Rollback != nil {
		t.Fatalf("automated: %+v", got)
	}
	if got := v.validation(t, manual); got.Automated || got.Verdict == store.VerdictChanged {
		t.Fatalf("manual: %+v", got)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", pol)
	}
	if n := v.deploymentCount(t); n != 3 {
		t.Fatalf("deployments: %d", n)
	}
}

// Review Focus 4: a new connection mid-window carries on the same window.
func TestValidationSurvivesAnAgentReconnect(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	v.sock.conn.CloseNow()
	waitFor(t, func() bool { return !v.s.Connected(v.ag.id) })
	v.at(t, id, afterGrace+store.ValidationPoll)
	if got := v.validation(t, id); got.Phase != store.PhaseObserving {
		t.Fatalf("offline mid-window ended it: %+v", got)
	}
	v.sock, v.ctx = v.online(t, validationCaps)
	v.at(t, id, afterWindow)
	if got := v.validation(t, id); got.Verdict != store.VerdictHealthy {
		t.Fatalf("after the reconnect: %+v", got)
	}
}

// Review Focus 5: with the policy or the adoption gone mid-window, a verdict and nothing else.
func TestValidationWhenThePolicyOrTheAdoptionGoes(t *testing.T) {
	t.Run("policy deleted", func(t *testing.T) {
		v := newValidationHost(t)
		v.obs.on(newContainer, health("unhealthy"))
		id, _ := v.automate(t)
		if err := v.st.Tenancy().DeleteUpdatePolicy(context.Background(), v.as("usr_planner"), v.appID()); err != nil {
			t.Fatal(err)
		}
		v.at(t, id, afterGrace)
		if got := v.validation(t, id); got.Verdict != store.VerdictUnhealthy || got.PolicyRunID != "" || got.Rollback != nil {
			t.Fatalf("policy deleted: %+v", got)
		}
		if n := v.deploymentCount(t); n != 2 {
			t.Fatalf("deployments: %d", n)
		}
	})
	t.Run("application released", func(t *testing.T) {
		v := newValidationHost(t)
		v.obs.on(newContainer, health("unhealthy"))
		id, _ := v.automate(t)
		var d store.Deployment
		if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments+"/"+id, "", 200)), &d); err != nil {
			t.Fatal(err)
		}
		v.do(t, "DELETE", strings.TrimSuffix(v.deployments, "/deployments")+"/adoption", `{"instance_id":"`+d.InstanceID+`","confirm":"shop"}`, 204)
		v.at(t, id, afterGrace)
		if got := v.validation(t, id); got.Verdict != store.VerdictChanged || got.Detail != store.ValidationDetailReleased || got.Rollback != nil {
			t.Fatalf("released: %+v", got)
		}
		if pol := v.policyNow(t); pol.Status != store.PolicyActive {
			t.Fatalf("policy: %+v", pol)
		}
	})
}

// A verdict recorded before a restart with its rollback decision still owed is decided on the next
// tick.
func TestValidationDecidesARollbackOwedFromBeforeARestart(t *testing.T) {
	v := newValidationHost(t)
	id, _ := v.automate(t)
	if decide, err := v.st.Tenancy().FinishValidation(context.Background(), id, store.VerdictUnhealthy, "web"); err != nil || !decide {
		t.Fatalf("finish: %v %v", decide, err)
	}
	api.ValidationTickForTest(v.s, time.Now())
	req := v.frame(t)
	if req.Revision != 1 || req.Services[0].ImageID != hostImage {
		t.Fatalf("rollback frame: %+v", req)
	}
	if got := v.validation(t, id); got.Rollback == nil || got.Rollback.Outcome != store.RollbackApplied || got.Rollback.DeploymentID != req.Deployment {
		t.Fatalf("rollback: %+v", got.Rollback)
	}
	if pol := v.policyNow(t); pol.PausedReason != store.ValidationReasonRolledBack {
		t.Fatalf("policy: %+v", pol)
	}
}

// faultyTenancy breaks or intercepts one store call at a time for the loop's failure paths.
type faultyTenancy struct {
	store.TenancyStore
	extraPending []store.PendingValidation // read before the store's rows; set before any tick
	failOutcomes atomic.Int32              // MarkRollbackOutcome calls still to fail
	failAttach   atomic.Bool
	refuseApply  atomic.Bool // ApplyPolicyDeployment answers ErrInvalid without applying
	beforeApply  func()      // runs as ApplyPolicyDeployment is entered
	afterApply   func()      // runs once it has returned
}

func (f *faultyTenancy) PendingValidations(ctx context.Context) ([]store.PendingValidation, error) {
	rows, err := f.TenancyStore.PendingValidations(ctx)
	return append(append([]store.PendingValidation{}, f.extraPending...), rows...), err
}

func (f *faultyTenancy) MarkRollbackOutcome(ctx context.Context, deployment, outcome, detail string) error {
	if f.failOutcomes.Add(-1) >= 0 {
		return errors.New("injected outcome write failure")
	}
	return f.TenancyStore.MarkRollbackOutcome(ctx, deployment, outcome, detail)
}

func (f *faultyTenancy) AttachPolicyRunDeployment(ctx context.Context, run, deployment string) error {
	if f.failAttach.Load() {
		return errors.New("injected attach failure")
	}
	return f.TenancyStore.AttachPolicyRunDeployment(ctx, run, deployment)
}

func (f *faultyTenancy) ApplyPolicyDeployment(ctx context.Context, a store.TenantAccess, policy, run, app, id, confirm string, key []byte, maxFrameBytes int) (*store.Deployment, *protocol.DeploymentRequest, error) {
	if f.refuseApply.Load() {
		return nil, nil, store.ErrInvalid
	}
	if f.beforeApply != nil {
		f.beforeApply()
	}
	d, req, err := f.TenancyStore.ApplyPolicyDeployment(ctx, a, policy, run, app, id, confirm, key, maxFrameBytes)
	if f.afterApply != nil {
		f.afterApply()
	}
	return d, req, err
}

type faultyStore struct {
	store.Store
	t *faultyTenancy
}

func (f faultyStore) Tenancy() store.TenancyStore { return f.t }

// faulty returns the faultyTenancy every server store call goes through.
func (v *validationHost) faulty() *faultyTenancy { return v.faults }

// noFrame proves nothing was queued for the agent: a heartbeat's answer is the next frame.
func (v *validationHost) noFrame(t *testing.T) {
	t.Helper()
	writeEnvelope(t, v.ctx, v.sock.conn, protocol.TypeHeartbeat, nil)
	if f := readEnvelope(t, v.ctx, v.sock.conn); f.Type != protocol.TypeHeartbeat {
		t.Fatalf("a frame was sent: %s", f.Type)
	}
}

// disconnect closes the agent's socket and waits for the server to see it.
func (v *validationHost) disconnect(t *testing.T) {
	v.sock.conn.CloseNow()
	waitFor(t, func() bool { return !v.s.Connected(v.ag.id) })
}

func (v *validationHost) deploymentState(t *testing.T, id string) string {
	t.Helper()
	var d store.Deployment
	if err := json.Unmarshal([]byte(v.do(t, "GET", v.deployments+"/"+id, "", 200)), &d); err != nil {
		t.Fatal(err)
	}
	return d.State
}

// A rollback whose outcome write failed is decided on a later tick from its deployment's state:
// waiting while it applies, then applied, and the policy pauses once.
func TestValidationDecidesARollbackWhoseOutcomeWriteFailed(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	f := v.faulty()
	f.failOutcomes.Store(1)
	v.at(t, id, afterGrace)
	req := v.frame(t)
	if got := v.validation(t, id); got.Rollback == nil || got.Rollback.DeploymentID != req.Deployment || got.Rollback.Outcome != "" {
		t.Fatalf("after the failed write: %+v", got.Rollback)
	}
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Rollback.Outcome != "" || v.policyNow(t).Status != store.PolicyActive {
		t.Fatalf("decided while applying: %+v", got.Rollback)
	}
	v.settle(t, req.Deployment, strings.Repeat("7", 64), hostImage, "", []protocol.Image{{ID: hostImage, Digests: []string{hostDigest}}, {ID: newImage, Tags: []string{"nginx:1"}, Digests: []string{"nginx@" + newDigest}}})
	v.at(t, id, afterGrace)
	if got := v.validation(t, id); got.Rollback == nil || *got.Rollback != (store.ValidationRollback{DeploymentID: req.Deployment, Revision: 1, Outcome: store.RollbackApplied}) {
		t.Fatalf("after the settle: %+v", got.Rollback)
	}
	if pol := v.policyNow(t); pol.Status != store.PolicyPaused || pol.PausedReason != store.ValidationReasonRolledBack || v.pauseRows(t) != 1 {
		t.Fatalf("policy: %+v, pause rows %d", pol, v.pauseRows(t))
	}
	if n := v.deploymentCount(t); n != 3 {
		t.Fatalf("deployments: %d", n)
	}
}

// A policy run that cannot name its deployment on the run fails it unsent: the run's record never
// hides what it sent.
func TestPolicyRunThatCannotNameItsDeploymentSendsNothing(t *testing.T) {
	v := newValidationHost(t)
	v.faulty().failAttach.Store(true)
	v.do(t, "PUT", "/api/organizations/a/registry-policy", `{"anonymous_pull_enabled":true}`, 204)
	api.SetDigestResolverForTest(v.s, &fakeDigests{digest: newDigest})
	v.policy(t, store.PolicyModeApply)
	v.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	runs := v.runs(t, "usr_planner")
	if len(runs) != 1 || runs[0].Outcome != store.RunFailed || runs[0].Detail != "error" || runs[0].DeploymentID == "" {
		t.Fatalf("runs: %+v", runs)
	}
	if state := v.deploymentState(t, runs[0].DeploymentID); state != "failed" {
		t.Fatalf("deployment: %s", state)
	}
	v.noFrame(t)
}

// Every way a due rollback can fail is recorded as failed with its code, and the policy pauses
// with the matching sentence.
func TestValidationRollbackFailures(t *testing.T) {
	check := func(t *testing.T, v *validationHost, id, detail string, named bool, deployments int) {
		t.Helper()
		got := v.validation(t, id)
		if got.Verdict != store.VerdictUnhealthy || got.Rollback == nil || got.Rollback.Outcome != store.RollbackFailed || got.Rollback.Detail != detail || (got.Rollback.DeploymentID != "") != named {
			t.Fatalf("validation: %+v %+v", got, got.Rollback)
		}
		if pol := v.policyNow(t); pol.Status != store.PolicyPaused || pol.PausedReason != store.ValidationReasonNotRolledBack+detail {
			t.Fatalf("policy: %+v", pol)
		}
		if n := v.deploymentCount(t); n != deployments {
			t.Fatalf("deployments: %d", n)
		}
	}
	t.Run("creator lost", func(t *testing.T) {
		v := newValidationHost(t)
		v.obs.on(newContainer, health("unhealthy"))
		id, _ := v.automate(t)
		if err := v.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_planner", Role: store.RoleOperator, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		v.at(t, id, afterGrace)
		check(t, v, id, store.RollbackCreatorLost, false, 2)
		v.noFrame(t)
	})
	t.Run("policy changed", func(t *testing.T) {
		v := newValidationHost(t)
		v.obs.on(newContainer, health("unhealthy"))
		id, _ := v.automate(t)
		v.faulty().beforeApply = func() { v.policy(t, store.PolicyModePlanOnly) }
		v.at(t, id, afterGrace)
		check(t, v, id, "policy_changed", true, 3)
		v.noFrame(t)
	})
	t.Run("endpoint offline", func(t *testing.T) {
		v := newValidationHost(t)
		v.obs.on(newContainer, func(in *protocol.ContainerInspection) error {
			in.Health = "unhealthy"
			v.disconnect(t)
			return nil
		})
		id, _ := v.automate(t)
		v.at(t, id, afterGrace)
		check(t, v, id, "endpoint_offline", false, 2)
	})
	t.Run("not sent", func(t *testing.T) {
		v := newValidationHost(t)
		v.obs.on(newContainer, health("unhealthy"))
		id, _ := v.automate(t)
		v.faulty().afterApply = func() { v.disconnect(t) }
		v.at(t, id, afterGrace)
		check(t, v, id, store.RollbackNotSent, true, 3)
		if state := v.deploymentState(t, v.validation(t, id).Rollback.DeploymentID); state != "failed" {
			t.Fatalf("rollback deployment: %s", state)
		}
	})
}

// Deployment detail, the deployment list and the policy's runs carry the validation without its
// baseline; a deployment still applying carries none.
func TestValidationJSON(t *testing.T) {
	v := newValidationHost(t)
	v.obs.on(newContainer, health("unhealthy"))
	id, _ := v.automate(t)
	v.at(t, id, afterGrace)
	rollback := v.frame(t)
	policyURL := strings.TrimSuffix(v.deployments, "deployments") + "update-policy"
	for name, body := range map[string]string{
		"detail": v.do(t, "GET", v.deployments+"/"+id, "", 200),
		"list":   v.do(t, "GET", v.deployments, "", 200),
		"policy": v.do(t, "GET", policyURL, "", 200),
		"runs":   v.do(t, "GET", policyURL+"/runs?limit=20", "", 200),
	} {
		if !strings.Contains(body, `"validation":{`) || strings.Contains(body, "baseline") {
			t.Fatalf("%s: %s", name, body)
		}
	}
	var pol struct {
		PausedReason string            `json:"paused_reason"`
		Runs         []store.PolicyRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(v.do(t, "GET", policyURL, "", 200)), &pol); err != nil || len(pol.Runs) != 1 {
		t.Fatalf("policy: %+v %v", pol, err)
	}
	r := pol.Runs[0].Validation
	if r == nil || r.Verdict != store.VerdictUnhealthy || r.Rollback == nil || *r.Rollback != (store.ValidationRollback{DeploymentID: rollback.Deployment, Revision: 1, Outcome: store.RollbackApplied}) || pol.PausedReason != store.ValidationReasonRolledBack {
		t.Fatalf("run validation: %+v, paused %q", r, pol.PausedReason)
	}
	if body := v.do(t, "GET", v.deployments+"/"+rollback.Deployment, "", 200); strings.Contains(body, `"validation"`) {
		t.Fatalf("an applying deployment carries a validation: %s", body)
	}
}

// stall is an agent that never answers for container: each inspection blocks until release closes
// or its budget ends, counting how many are in flight at once.
type stall struct {
	container string
	mu        sync.Mutex
	release   chan struct{}
	inFlight  int
	most      int
	calls     int
}

func (s *stall) wait(ctx context.Context) error {
	s.mu.Lock()
	s.inFlight++
	s.calls++
	s.most = max(s.most, s.inFlight)
	release := s.release
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.inFlight--; s.mu.Unlock() }()
	select {
	case <-release:
	case <-ctx.Done():
	}
	return errors.New("stalled")
}

// Reviewer's regression: another organization's stalled agent, with several validations pending
// ahead of this one, neither delays this organization's polls nor runs more than one at a time.
func TestValidationIsNotHeldUpByAnotherOrganizationsStalledAgent(t *testing.T) {
	v := newValidationHost(t)
	served := make(chan struct{}, 8)
	v.obs.on(newContainer, func(*protocol.ContainerInspection) error { served <- struct{}{}; return nil })
	id, _ := v.automate(t)
	healthy := v.validation(t, id)
	st := &stall{container: strings.Repeat("5", 64)}
	v.obs.mu.Lock()
	v.obs.stall = st
	v.obs.mu.Unlock()
	for i := range 3 {
		v.faults.extraPending = append(v.faults.extraPending, store.PendingValidation{
			Validation:     store.Validation{DeploymentID: strings.Repeat(string(rune('1'+i)), 8), Automated: true, Phase: store.PhaseObserving, StartedAt: healthy.StartedAt, ObserveUntil: healthy.ObserveUntil},
			OrganizationID: "b", EndpointID: v.ag.id, Health: true, Baseline: map[string]store.ServiceBaseline{"web": {ContainerID: st.container}},
			Services: []store.ObservedService{{DeploymentIdentity: protocol.DeploymentIdentity{Service: "web", ContainerID: st.container, ImageID: newImage}, Presence: store.PresencePresent}},
		})
	}
	for _, offset := range []time.Duration{afterGrace, afterWindow} {
		st.mu.Lock()
		st.release, st.calls = make(chan struct{}), 0
		release := st.release
		st.mu.Unlock()
		done := make(chan struct{})
		go func() { defer close(done); v.at(t, id, offset) }()
		select {
		case <-served: // the stalled rows are still blocked: release is open
		case <-time.After(api.PlanInspectionBudgetForTest / 2):
			t.Fatalf("at %v the healthy organization waited on the stalled one", offset)
		}
		close(release)
		<-done
		st.mu.Lock()
		calls, most := st.calls, st.most
		st.mu.Unlock()
		if calls != 3 || most != 1 {
			t.Fatalf("at %v: %d stalled polls, %d at once; want 3, 1", offset, calls, most)
		}
	}
	if got := v.validation(t, id); got.Verdict != store.VerdictHealthy {
		t.Fatalf("healthy organization: %+v", got)
	}
	if p := v.policyNow(t); p.Status != store.PolicyActive {
		t.Fatalf("policy: %+v", p)
	}
}
