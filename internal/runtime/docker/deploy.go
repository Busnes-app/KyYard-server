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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// stopGrace is the seconds Docker waits before killing a container being stopped; ten is
// Docker's own default and Compose's.
const stopGrace = 10

// replaceBudget is the time one service's mutating steps may need: stop and start at
// operationBudget, rename, create and the identity read at callBudget.
const replaceBudget = 2*operationBudget + 3*callBudget

// Deploy replaces each service's mapped container with one created from the pinned image ID,
// in plan order: precondition, image, rename, create, stop, start, remove. Renaming and creating
// while the old container still runs means a name conflict or a refused create costs no
// downtime. The first step that is not a success ends the run and every later step is recorded
// as skipped. Nothing is rolled back: the steps say where the old container was left. No image
// is pulled and no volume is touched. See docs/superpowers/specs/2026-09-22-deployment-runtime-design.md.
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

// step records one outcome. The first non-success fixes the run's outcome and detail.
func (r *deployRun) step(service, step string, run func() (string, string)) {
	if r.res.Outcome != "" {
		r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: step, Outcome: protocol.OutcomeSkipped})
		return
	}
	outcome, detail := run()
	r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: step, Outcome: outcome, Detail: bound(detail, protocol.MaxDeploymentStepDetailBytes)})
	if outcome != protocol.OutcomeSucceeded {
		r.res.Outcome, r.res.Detail = outcome, bound(fmt.Sprintf("service %s, step %s: %s", service, step, detail), protocol.MaxResultDetailBytes)
	}
}

// outcomeFor classifies a call that did not succeed. A status is an answer and is failed. With
// no answer, a cancelled parent means the session dropped and the Engine may have acted:
// unknown, never retried by itself.
func (r *deployRun) outcomeFor(ctx context.Context, err error, status int) (string, string) {
	if statusOf(err) != 0 {
		err = nil
	}
	switch {
	case err != nil && r.parent.Err() == context.Canceled:
		return protocol.OutcomeUnknown, "the connection ended before the runtime answered"
	case err != nil && ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "the runtime did not answer in time"
	case err != nil:
		return protocol.OutcomeFailed, "the runtime call failed"
	}
	return protocol.OutcomeFailed, fmt.Sprintf("the runtime refused with status %d", status)
}

// inspectedForDeploy is decoded with pointers so a field the Engine did not report is refused
// rather than read as its zero value.
type inspectedForDeploy struct {
	ID      string `json:"Id"`
	Image   string
	Name    string
	Created time.Time
	Mounts  *[]json.RawMessage
	Config  *struct {
		Cmd, Entrypoint []string
		User            string
	}
	HostConfig *struct {
		NetworkMode                            string
		Privileged, AutoRemove, ReadonlyRootfs *bool
		Tmpfs                                  map[string]string
		CapAdd, CapDrop, SecurityOpt           []string
		Devices                                []json.RawMessage
		PidMode, IpcMode                       string
	}
	NetworkSettings *struct {
		Networks map[string]json.RawMessage
	}
}

const cannotExpress = "the container has configuration the definition cannot express: "

// undescribed refuses configuration recreation would drop, returning the denial detail or ""
// when there is none. The definition expresses image, env, ports, restart and the project
// network only; the image-default command is checked separately against the old image.
func undescribed(in inspectedForDeploy, projectNetwork string) string {
	h, n := in.HostConfig, in.NetworkSettings
	if h == nil || in.Config == nil || n == nil || in.Mounts == nil || h.Privileged == nil || h.AutoRemove == nil || h.ReadonlyRootfs == nil {
		return "the runtime did not report the container's full configuration"
	}
	network := projectNetwork
	if h.NetworkMode == "default" || h.NetworkMode == "bridge" {
		network = "bridge"
	}
	_, onNetwork := n.Networks[network]
	switch {
	case len(*in.Mounts) > 0:
		return cannotExpress + "mounts"
	case len(h.Tmpfs) > 0:
		return cannotExpress + "tmpfs"
	case *h.AutoRemove:
		return cannotExpress + "auto-remove"
	case *h.ReadonlyRootfs:
		return cannotExpress + "read-only root filesystem"
	case *h.Privileged:
		return cannotExpress + "privileged"
	case len(h.CapAdd) > 0 || len(h.CapDrop) > 0:
		return cannotExpress + "capabilities"
	case len(h.SecurityOpt) > 0:
		return cannotExpress + "security options"
	case len(h.Devices) > 0:
		return cannotExpress + "devices"
	case h.PidMode != "" && h.PidMode != "private":
		return cannotExpress + "PID mode"
	case h.IpcMode != "" && h.IpcMode != "private":
		return cannotExpress + "IPC mode"
	case in.Config.User != "":
		return cannotExpress + "user"
	case h.NetworkMode != "default" && h.NetworkMode != "bridge" && h.NetworkMode != projectNetwork:
		return cannotExpress + "network mode"
	case len(n.Networks) != 1 || !onNetwork:
		return cannotExpress + "networks"
	}
	return ""
}

func (r *deployRun) service(ctx context.Context, s protocol.DeploymentService) {
	old := url.PathEscape(s.Replaces.ContainerID)
	var before inspectedForDeploy
	var networkMode string
	r.step(s.Name, protocol.StepPrecondition, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		if err := r.c.get(cctx, "/containers/"+old+"/json", &before); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return protocol.OutcomeDenied, "the container no longer exists"
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		if before.ID != s.Replaces.ContainerID || before.Image != s.Replaces.ImageID || before.Created.Unix() != s.Replaces.CreatedUnix {
			return protocol.OutcomeDenied, "the container is not the one this plan was decided about"
		}
		if detail := undescribed(before, r.req.Project+"_default"); detail != "" {
			return protocol.OutcomeDenied, detail
		}
		// A Cmd or Entrypoint set at run time would be replaced by the image default.
		ictx, icancel := context.WithTimeout(ctx, callBudget)
		defer icancel()
		var im struct {
			Config struct{ Cmd, Entrypoint []string }
		}
		if err := r.c.get(ictx, "/images/"+url.PathEscape(s.Replaces.ImageID)+"/json", &im); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return protocol.OutcomeDenied, "the container's image is no longer present"
			}
			return r.outcomeFor(ictx, err, statusOf(err))
		}
		if !slices.Equal(before.Config.Cmd, im.Config.Cmd) || !slices.Equal(before.Config.Entrypoint, im.Config.Entrypoint) {
			return protocol.OutcomeDenied, cannotExpress + "command"
		}
		networkMode = before.HostConfig.NetworkMode
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
	r.step(s.Name, protocol.StepRename, func() (string, string) {
		if time.Until(r.req.Deadline) < replaceBudget {
			return protocol.OutcomeTimedOut, "not enough time left before the deadline to replace this service safely"
		}
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
		status, err := r.c.postJSON(cctx, "/containers/create?name="+url.QueryEscape(s.ContainerName), createBody(r.req, s, networkMode), &out)
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
	r.step(s.Name, protocol.StepStop, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+old+"/stop?t="+strconv.Itoa(stopGrace))
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		return protocol.OutcomeSucceeded, ""
	})
	r.step(s.Name, protocol.StepStart, func() (string, string) {
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+url.PathEscape(created)+"/start")
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			outcome, detail := r.outcomeFor(cctx, err, status)
			return outcome, fmt.Sprintf("%s (container %s)", detail, created)
		}
		ictx, icancel := context.WithTimeout(ctx, callBudget)
		defer icancel()
		var after inspectedForDeploy
		if err := r.c.get(ictx, "/containers/"+url.PathEscape(created)+"/json", &after); err != nil {
			outcome, detail := r.outcomeFor(ictx, err, statusOf(err))
			return outcome, fmt.Sprintf("the container started but its identity could not be read: %s (container %s)", detail, created)
		}
		id := protocol.DeploymentIdentity{Service: s.Name, ContainerID: after.ID, ImageID: after.Image, CreatedUnix: after.Created.Unix()}
		if after.ID != created || after.Image != s.ImageID || (protocol.InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() != nil {
			return protocol.OutcomeFailed, fmt.Sprintf("the container started but its identity could not be verified (container %s)", created)
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
type endpointSettings struct {
	Aliases []string `json:"Aliases"`
}
type networkingConfig struct {
	EndpointsConfig map[string]endpointSettings `json:"EndpointsConfig"`
}
type containerCreate struct {
	Image        string              `json:"Image"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   struct {
		NetworkMode   string                   `json:"NetworkMode,omitempty"`
		PortBindings  map[string][]portBinding `json:"PortBindings,omitempty"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	NetworkingConfig *networkingConfig `json:"NetworkingConfig,omitempty"`
}

// createBody is the whole configuration of the new container: the definition's subset and
// the Compose labels discovery already groups by. Nothing is copied from the old container
// except the network mode the precondition already accepted: a Compose project's containers
// run on "<project>_default", and dropping that would strand the new one off the project
// network and its service-name DNS.
func createBody(req protocol.DeploymentRequest, s protocol.DeploymentService, networkMode string) containerCreate {
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
	if projectNetwork := req.Project + "_default"; networkMode == projectNetwork {
		body.HostConfig.NetworkMode = projectNetwork
		body.NetworkingConfig = &networkingConfig{EndpointsConfig: map[string]endpointSettings{projectNetwork: {Aliases: []string{s.Name}}}}
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
