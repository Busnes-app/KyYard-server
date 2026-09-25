package docker

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// Remove stops and deletes each target container in order: precondition, stop, remove. A
// container already gone counts as removed. Volumes (no v=1) and networks are never touched.
// The first step that is not a success ends the run and every later step is skipped.
func (c *Client) Remove(parent context.Context, req protocol.RemovalRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.ValidateFor(protocol.RuntimeDocker, time.Now()); err != nil {
		return refused(res, err)
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	r := &deployRun{c: c, parent: parent, res: res, started: started}
	for _, t := range req.Containers {
		r.removeTarget(ctx, t, req.Deadline)
	}
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

func (r *deployRun) removeTarget(ctx context.Context, t protocol.RemovalTarget, deadline time.Time) {
	id := url.PathEscape(t.Target.ContainerID)
	gone := false
	r.step(t.Service, protocol.StepPrecondition, func() (string, string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var in struct {
			ID      string `json:"Id"`
			Image   string
			Created time.Time
		}
		if err := r.c.get(cctx, "/containers/"+id+"/json", &in); err != nil {
			if statusOf(err) == http.StatusNotFound {
				gone = true
				return succeeded()
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		if in.ID != t.Target.ContainerID || in.Image != t.Target.ImageID || in.Created.Unix() != t.Target.CreatedUnix {
			return deny("identity_mismatch")
		}
		return succeeded()
	})
	if gone {
		for _, step := range []string{protocol.StepStop, protocol.StepRemove} {
			r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: t.Service, Step: step, Outcome: protocol.OutcomeSkipped})
		}
		return
	}
	r.step(t.Service, protocol.StepStop, func() (string, string, string) {
		if time.Until(deadline) < operationBudget+2*callBudget {
			return protocol.OutcomeTimedOut, "deadline", ""
		}
		r.begin()
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+id+"/stop?t="+strconv.Itoa(stopGrace))
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		return succeeded()
	})
	r.step(t.Service, protocol.StepRemove, func() (string, string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		status, err := r.c.del(cctx, "/containers/"+id)
		switch {
		case status == http.StatusConflict:
			return fail("dependents")
		case err != nil || (status >= 400 && status != http.StatusNotFound):
			return r.outcomeFor(cctx, err, status)
		}
		return succeeded()
	})
}
