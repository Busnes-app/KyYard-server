// internal/agent/client/deployments.go
package client

import (
	"context"
	"encoding/json"
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
)

type deploymentEntry struct {
	Result   protocol.DeploymentResult `json:"result"`
	Finished time.Time                 `json:"finished"`
}

// deployer runs one deployment at a time on the agent's root context, remembers every result
// before sending it, and re-sends what a dropped session never carried. See
// docs/agent-protocol.md, Deployment apply.
type deployer struct {
	root context.Context
	opts *Options
	path string
	mu   sync.Mutex
	done map[string]deploymentEntry
	busy chan struct{}
}

func newDeployer(root context.Context, dir string, opts *Options) *deployer {
	d := &deployer{root: root, opts: opts, path: filepath.Join(dir, "deployments.json"), done: map[string]deploymentEntry{}, busy: make(chan struct{}, 1)}
	if raw, err := os.ReadFile(d.path); err == nil {
		var saved map[string]deploymentEntry
		if json.Unmarshal(raw, &saved) == nil {
			d.done = saved
			d.prune()
		}
	}
	return d
}

func (d *deployer) prune() {
	cutoff := time.Now().UTC().Add(-deploymentLedgerLife)
	for id, e := range d.done {
		if e.Finished.Before(cutoff) {
			delete(d.done, id)
		}
	}
	if len(d.done) <= deploymentLedgerMax {
		return
	}
	ids := make([]string, 0, len(d.done))
	for id := range d.done {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return d.done[ids[i]].Finished.Before(d.done[ids[j]].Finished) })
	for _, id := range ids[:len(d.done)-deploymentLedgerMax] {
		delete(d.done, id)
	}
}

func (d *deployer) record(res protocol.DeploymentResult) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done[res.Deployment] = deploymentEntry{Result: res, Finished: time.Now().UTC()}
	d.prune()
	raw, err := json.Marshal(d.done)
	if err != nil {
		return err
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.path)
}

func resultFrame(res protocol.DeploymentResult) outFrame {
	return outFrame{Type: protocol.TypeDeploymentResult, Payload: res}
}

func denied(id, detail string) protocol.DeploymentResult {
	return protocol.DeploymentResult{Deployment: id, Outcome: protocol.OutcomeDenied, Detail: detail, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
}

// handle answers one deployment.apply payload. Refusals are sent and not remembered; a run's
// result is remembered and then sent. handle runs on the session loop, which is out's only
// reader, so every send happens off it; sessionCtx bounds only the send.
func (d *deployer) handle(sessionCtx context.Context, endpointID string, payload []byte, out chan<- outFrame) {
	var req protocol.DeploymentRequest
	if json.Unmarshal(payload, &req) != nil || req.Validate(time.Now()) != nil {
		go send(sessionCtx, out, resultFrame(denied(req.Deployment, "invalid deployment request")))
		return
	}
	if req.Endpoint != endpointID {
		go send(sessionCtx, out, resultFrame(denied(req.Deployment, "this deployment is addressed to another endpoint")))
		return
	}
	d.mu.Lock()
	prior, ok := d.done[req.Deployment]
	d.mu.Unlock()
	if ok {
		go send(sessionCtx, out, resultFrame(prior.Result))
		return
	}
	deploy := d.opts.Deploy
	if deploy == nil {
		go send(sessionCtx, out, resultFrame(denied(req.Deployment, "this agent has no runtime to deploy")))
		return
	}
	select {
	case d.busy <- struct{}{}:
	default:
		go send(sessionCtx, out, resultFrame(denied(req.Deployment, "this agent is already applying a deployment")))
		return
	}
	go func() {
		defer func() { <-d.busy }()
		res := deploy(d.root, req)
		for i := range req.Services {
			clear(req.Services[i].Env)
		}
		if res.Deployment == "" {
			res.Deployment = req.Deployment
		}
		// Unrecorded, the result still goes out; it just cannot be re-sent after a drop.
		if err := d.record(res); err != nil && d.opts.Log != nil {
			d.opts.Log.Printf("deployment %s: result not recorded: %v", req.Deployment, err)
		}
		send(sessionCtx, out, resultFrame(res))
	}()
}

// resend queues every remembered result until the session ends. The ledger can outnumber the
// outbound buffer, so it blocks rather than drops. The server settles once; the rest are ignored.
func (d *deployer) resend(ctx context.Context, out chan<- outFrame) {
	d.mu.Lock()
	d.prune()
	results := make([]protocol.DeploymentResult, 0, len(d.done))
	for _, e := range d.done {
		results = append(results, e.Result)
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
