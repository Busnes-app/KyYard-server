package client

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const (
	deploymentLedgerLife = 24 * time.Hour
	deploymentLedgerMax  = 20
	// restartedDetail settles a run that began changing the host and never recorded a result.
	restartedDetail = "the agent restarted after replacement began; inspect the host"
)

type deploymentEntry struct {
	Result   protocol.DeploymentResult `json:"result"`
	Finished time.Time                 `json:"finished"`
	// Started is set, with no result yet, while a run may be changing the host.
	Started time.Time `json:"started,omitzero"`
}

// pending is a run that began changing the host and has no result yet.
func (e deploymentEntry) pending() bool { return e.Result.Deployment == "" }

// deployer runs one deployment at a time on the agent's root context, remembers every result
// before sending it, delivers it to whichever session is current, and re-sends what no session
// carried. See docs/agent-protocol.md, Deployment apply.
type deployer struct {
	root    context.Context
	opts    *Options
	path    string
	mu      sync.Mutex
	done    map[string]deploymentEntry
	running string       // ID of the deployment in the single slot, "" when idle
	current *sessionLink // the connected session, nil between sessions
	runs    sync.WaitGroup
}

type sessionLink struct {
	ctx context.Context
	out chan<- outFrame
}

func newDeployer(root context.Context, dir string, opts *Options) *deployer {
	d := &deployer{root: root, opts: opts, path: filepath.Join(dir, "deployments.json"), done: map[string]deploymentEntry{}}
	if raw, err := os.ReadFile(d.path); err == nil {
		var saved map[string]deploymentEntry
		if json.Unmarshal(raw, &saved) == nil {
			d.done = saved
			if d.settleStarted() {
				if err := d.save(); err != nil && d.opts.Log != nil {
					d.opts.Log.Printf("deployment ledger: interrupted runs not recorded: %v", err)
				}
			}
			d.prune()
		}
	}
	return d
}

// settleStarted turns every run a restart interrupted after it began changing the host into an
// unknown result, re-sent like any other. It reports whether it found one.
func (d *deployer) settleStarted() bool {
	now, found := time.Now().UTC(), false
	for id, e := range d.done {
		if e.pending() {
			d.done[id] = deploymentEntry{Result: protocol.DeploymentResult{Deployment: id, Outcome: protocol.OutcomeUnknown, Detail: restartedDetail, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}, Finished: now}
			found = true
		}
	}
	return found
}

// save writes the ledger durably. The caller holds mu, or owns d alone.
func (d *deployer) save() error {
	raw, err := json.Marshal(d.done)
	if err != nil {
		return err
	}
	return writeDurable(d.path, raw)
}

// begin records durably, before it returns, that run id is about to change the host.
func (d *deployer) begin(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done[id] = deploymentEntry{Started: time.Now().UTC()}
	if err := d.save(); err != nil && d.opts.Log != nil {
		// A restart before the result would then run the frame again; the recheck still guards it.
		d.opts.Log.Printf("deployment %s: start not recorded: %v", id, err)
	}
}

func (d *deployer) prune() {
	cutoff := time.Now().UTC().Add(-deploymentLedgerLife)
	ids := make([]string, 0, len(d.done))
	for id, e := range d.done {
		switch {
		case e.pending():
		case e.Finished.Before(cutoff):
			delete(d.done, id)
		default:
			ids = append(ids, id)
		}
	}
	if len(ids) <= deploymentLedgerMax {
		return
	}
	sort.Slice(ids, func(i, j int) bool { return d.done[ids[i]].Finished.Before(d.done[ids[j]].Finished) })
	for _, id := range ids[:len(ids)-deploymentLedgerMax] {
		delete(d.done, id)
	}
}

// attach makes a session the destination for results that finish while it is connected; the
// returned func detaches it unless a newer session has already taken its place.
func (d *deployer) attach(ctx context.Context, out chan<- outFrame) (detach func()) {
	link := &sessionLink{ctx: ctx, out: out}
	d.mu.Lock()
	d.current = link
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		if d.current == link {
			d.current = nil
		}
		d.mu.Unlock()
	}
}

// finish records a run's result, frees the slot and returns the session to deliver it to.
// The ledger is written before the slot is released, so no second run can race the file.
func (d *deployer) finish(res protocol.DeploymentResult) *sessionLink {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done[res.Deployment] = deploymentEntry{Result: res, Finished: time.Now().UTC()}
	d.prune()
	if err := d.save(); err != nil && d.opts.Log != nil {
		// Still delivered and re-sent from memory; only a restart loses it.
		d.opts.Log.Printf("deployment %s: result not recorded: %v", res.Deployment, err)
	}
	d.running = ""
	return d.current
}

// wait returns once every started run has recorded its result.
func (d *deployer) wait() { d.runs.Wait() }

func resultFrame(res protocol.DeploymentResult) outFrame {
	return outFrame{Type: protocol.TypeDeploymentResult, Payload: res}
}

func denied(id, detail string) protocol.DeploymentResult {
	return protocol.DeploymentResult{Deployment: id, Outcome: protocol.OutcomeDenied, Detail: detail, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
}

// handleApply answers one deployment.apply payload; see run.
func (d *deployer) handleApply(sessionCtx context.Context, endpointID string, payload []byte, out chan<- outFrame) {
	var req protocol.DeploymentRequest
	if json.Unmarshal(payload, &req) != nil {
		go send(sessionCtx, out, resultFrame(denied(req.Deployment, "invalid deployment request")))
		return
	}
	var exec func(context.Context) protocol.DeploymentResult
	if deploy := d.opts.Deploy; deploy != nil {
		exec = func(ctx context.Context) protocol.DeploymentResult {
			res := deploy(ctx, req, func() { d.begin(req.Deployment) })
			for i := range req.Services {
				clear(req.Services[i].Env)
			}
			clear(req.Registries)
			return res
		}
	}
	d.run(sessionCtx, out, req.Deployment, req.Endpoint, endpointID, req.Validate, exec, "this agent has no runtime to deploy")
}

// handleRemoval answers one deployment.remove payload; see run.
func (d *deployer) handleRemoval(sessionCtx context.Context, endpointID string, payload []byte, out chan<- outFrame) {
	var req protocol.RemovalRequest
	if json.Unmarshal(payload, &req) != nil {
		go send(sessionCtx, out, resultFrame(denied(req.Deployment, "invalid deployment request")))
		return
	}
	var exec func(context.Context) protocol.DeploymentResult
	if remove := d.opts.Remove; remove != nil {
		exec = func(ctx context.Context) protocol.DeploymentResult {
			return remove(ctx, req, func() { d.begin(req.Deployment) })
		}
	}
	d.run(sessionCtx, out, req.Deployment, req.Endpoint, endpointID, req.Validate, exec, "this agent has no runtime to remove")
}

// run gates and runs one decoded request in the agent's single deployment slot. Refusals are
// sent and not remembered; a run's result is remembered and then sent to the current session.
// run is called on the session loop, which is out's only reader, so every send happens off it.
func (d *deployer) run(sessionCtx context.Context, out chan<- outFrame, id, endpoint, endpointID string, validate func(time.Time) error, exec func(context.Context) protocol.DeploymentResult, noRuntime string) {
	// A re-sent frame for the live run: a refusal would settle the row it is still applying,
	// so say nothing and let the real result answer, even once the frame's deadline has passed.
	d.mu.Lock()
	live := id != "" && d.running == id
	d.mu.Unlock()
	if live {
		return
	}
	if err := validate(time.Now()); err != nil {
		res := denied(id, "invalid deployment request")
		if errors.Is(err, protocol.ErrClockSkew) {
			res.Outcome, res.Detail = protocol.OutcomeFailed, err.Error()
		}
		go send(sessionCtx, out, resultFrame(res))
		return
	}
	if endpoint != endpointID {
		go send(sessionCtx, out, resultFrame(denied(id, "this deployment is addressed to another endpoint")))
		return
	}
	d.mu.Lock()
	prior, replay := d.done[id]
	running := d.running
	if !replay && exec != nil && running == "" {
		d.running = id
	}
	d.mu.Unlock()
	switch {
	case replay:
		go send(sessionCtx, out, resultFrame(prior.Result))
		return
	case exec == nil:
		go send(sessionCtx, out, resultFrame(denied(id, noRuntime)))
		return
	case running == id:
		return
	case running != "":
		go send(sessionCtx, out, resultFrame(denied(id, "this agent is already applying a deployment")))
		return
	}
	d.runs.Add(1)
	go func() {
		res := exec(d.root)
		if res.Deployment == "" {
			res.Deployment = id
		}
		// Never record or send what the server would refuse to read: the host may have acted.
		if res.Deployment != id || res.Validate() != nil {
			res = protocol.DeploymentResult{Deployment: id, Outcome: protocol.OutcomeUnknown, Detail: "the runtime returned an unreadable result", Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
		}
		link := d.finish(res)
		d.runs.Done()
		if link != nil {
			send(link.ctx, link.out, resultFrame(res))
		}
	}()
}

// resend queues every remembered result until the session ends. The ledger can outnumber the
// outbound buffer, so it blocks rather than drops. The server settles once; the rest are ignored.
func (d *deployer) resend(ctx context.Context, out chan<- outFrame) {
	d.mu.Lock()
	d.prune()
	results := make([]protocol.DeploymentResult, 0, len(d.done))
	for _, e := range d.done {
		if !e.pending() {
			results = append(results, e.Result)
		}
	}
	d.mu.Unlock()
	for _, res := range results {
		send(ctx, out, resultFrame(res))
	}
}

func send(ctx context.Context, out chan<- outFrame, f outFrame) {
	select {
	case out <- f:
	case <-ctx.Done():
	}
}
