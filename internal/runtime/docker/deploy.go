// internal/runtime/docker/deploy.go
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// stopGrace is the seconds Docker waits before killing a container being stopped; ten is
// Docker's own default and Compose's.
const stopGrace = 10

// Deploy replaces each service's mapped container with one created from the pinned image ID,
// in plan order: precondition, image, stop, rename, create, start, remove. The first step that
// is not a success ends the run and every later step is recorded as skipped. Nothing is rolled
// back: a renamed, stopped old container stays where the result says it is. No image is
// pulled and no volume is touched. See docs/superpowers/specs/2026-09-22-deployment-runtime-design.md.
func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.Validate(time.Now()); err != nil {
		res.Outcome, res.Detail = protocol.OutcomeDenied, "the deployment request is invalid"
		return res
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	r := &deployRun{c: c, parent: parent, req: req, res: res}
	for _, s := range req.Services {
		r.service(ctx, s)
	}
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

type deployRun struct {
	c      *Client
	parent context.Context
	req    protocol.DeploymentRequest
	res    protocol.DeploymentResult
}

func (r *deployRun) stopped() bool { return r.res.Outcome != "" }

// step records one outcome. The first non-success fixes the run's outcome and detail.
func (r *deployRun) step(service, step string, run func() (string, string)) bool {
	if r.stopped() {
		r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: step, Outcome: protocol.OutcomeSkipped})
		return false
	}
	outcome, detail := run()
	r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: step, Outcome: outcome, Detail: bound(detail, protocol.MaxDeploymentStepDetailBytes)})
	if outcome != protocol.OutcomeSucceeded {
		r.res.Outcome, r.res.Detail = outcome, bound(fmt.Sprintf("service %s, step %s: %s", service, step, detail), protocol.MaxResultDetailBytes)
		return false
	}
	return true
}

// outcomeFor classifies a call that returned an error. A parent cancelled before its deadline
// means the session dropped and the Engine may have acted: unknown, never retried by itself.
func (r *deployRun) outcomeFor(ctx context.Context, err error, status int) (string, string) {
	switch {
	case r.parent.Err() == context.Canceled:
		return protocol.OutcomeUnknown, "the connection ended before the runtime answered"
	case err != nil && ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "the runtime did not answer in time"
	case err != nil:
		return protocol.OutcomeFailed, "the runtime call failed"
	}
	return protocol.OutcomeFailed, fmt.Sprintf("the runtime refused with status %d", status)
}

type inspectedForDeploy struct {
	ID         string `json:"Id"`
	Image      string
	Name       string
	Created    time.Time
	State      struct{ Status string }
	Mounts     []struct{ Type string }
	HostConfig struct {
		NetworkMode string
		Privileged  bool
	}
}

func (r *deployRun) service(ctx context.Context, s protocol.DeploymentService) {
	old := url.PathEscape(s.Replaces.ContainerID)
	var before inspectedForDeploy
	r.step(s.Name, protocol.StepPrecondition, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		if err := r.c.get(cctx, "/containers/"+old+"/json", &before); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return protocol.OutcomeDenied, "the container no longer exists"
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		switch {
		case before.ID != s.Replaces.ContainerID || before.Image != s.Replaces.ImageID || before.Created.Unix() != s.Replaces.CreatedUnix:
			return protocol.OutcomeDenied, "the container is not the one this plan was decided about"
		case len(before.Mounts) > 0:
			return protocol.OutcomeDenied, "the container has mounts the definition does not describe; recreating it would drop them"
		case before.HostConfig.NetworkMode != "default" && before.HostConfig.NetworkMode != "bridge":
			return protocol.OutcomeDenied, "the container uses a network mode the definition does not describe"
		case before.HostConfig.Privileged:
			return protocol.OutcomeDenied, "the container is privileged; the definition cannot express that"
		}
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepImage, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var im struct {
			ID string `json:"Id"`
		}
		if err := r.c.get(cctx, "/images/"+url.PathEscape(s.ImageID)+"/json", &im); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return protocol.OutcomeFailed, "the pinned image is not present on this host"
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		if im.ID != s.ImageID {
			return protocol.OutcomeFailed, "the host reported a different image identity"
		}
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepStop, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+old+"/stop?t="+strconv.Itoa(stopGrace))
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepRename, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		name := strings.TrimPrefix(before.Name, "/") + ".kyyard-prev-" + r.req.Deployment[:8]
		status, err := r.c.post(cctx, "/containers/"+old+"/rename?name="+url.QueryEscape(name))
		if err != nil || status >= 400 {
			if status == http.StatusConflict {
				return protocol.OutcomeFailed, "a container already holds the name reserved for the previous one"
			}
			return r.outcomeFor(cctx, err, status)
		}
		return protocol.OutcomeSucceeded, ""
	})
	var created string
	r.step(s.Name, protocol.StepCreate, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var out struct {
			ID string `json:"Id"`
		}
		status, err := r.c.postJSON(cctx, "/containers/create?name="+url.QueryEscape(s.ContainerName), createBody(r.req, s), &out)
		if err != nil || status != http.StatusCreated {
			if status == http.StatusConflict {
				return protocol.OutcomeFailed, "a container with that name already exists"
			}
			return r.outcomeFor(cctx, err, status)
		}
		if !protocol.ValidExecID(out.ID) {
			return protocol.OutcomeFailed, "the runtime returned an unusable container identity"
		}
		created = out.ID
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepStart, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+created+"/start")
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		var after inspectedForDeploy
		if err := r.c.get(cctx, "/containers/"+created+"/json", &after); err != nil {
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		id := protocol.DeploymentIdentity{Service: s.Name, ContainerID: after.ID, ImageID: after.Image, CreatedUnix: after.Created.Unix()}
		if (protocol.InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() != nil || after.Image != s.ImageID {
			return protocol.OutcomeFailed, "the new container's identity could not be verified"
		}
		r.res.Services = append(r.res.Services, id)
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepRemove, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		status, err := r.c.del(cctx, "/containers/"+old)
		if err != nil || status >= 400 {
			return r.outcomeFor(cctx, err, status)
		}
		return protocol.OutcomeSucceeded, ""
	})
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}
type containerCreate struct {
	Image        string              `json:"Image"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   struct {
		PortBindings  map[string][]portBinding `json:"PortBindings,omitempty"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

// createBody is the whole configuration of the new container: the definition's subset and
// the Compose labels discovery already groups by. Nothing is copied from the old container.
func createBody(req protocol.DeploymentRequest, s protocol.DeploymentService) containerCreate {
	body := containerCreate{Image: s.ImageID, Env: []string{}, Labels: map[string]string{
		"com.docker.compose.project": req.Project, "com.docker.compose.service": s.Name, "com.docker.compose.container-number": "1", "com.docker.compose.oneoff": "False",
		"kyyard.deployment": req.Deployment, "kyyard.revision": strconv.Itoa(req.Revision),
	}}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		body.Env = append(body.Env, k+"="+s.Env[k])
	}
	if len(s.Ports) > 0 {
		body.ExposedPorts = map[string]struct{}{}
		body.HostConfig.PortBindings = map[string][]portBinding{}
	}
	for _, p := range s.Ports {
		key := strconv.Itoa(p.Container) + "/" + p.Protocol
		body.ExposedPorts[key] = struct{}{}
		body.HostConfig.PortBindings[key] = append(body.HostConfig.PortBindings[key], portBinding{HostIP: p.HostIP, HostPort: strconv.Itoa(p.Host)})
	}
	body.HostConfig.RestartPolicy.Name = s.Restart
	if body.HostConfig.RestartPolicy.Name == "" {
		body.HostConfig.RestartPolicy.Name = "no"
	}
	return body
}

// postJSON sends a JSON body and decodes a JSON answer. The Engine's error body is read and
// discarded: its text can echo configuration, so only the status reaches a result.
func (c *Client) postJSON(ctx context.Context, path string, body, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.Unmarshal(answer, out)
}
