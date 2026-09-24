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
	"reflect"
	"regexp"
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

// Deploy replaces each service's mapped container with one created from the pinned image ID.
// First every service's precondition and image (pull, for a service naming a digest), in plan
// order, with each frame volume ensured after the precondition of the first service mounting
// it; then per service rename, create, stop, start, remove. Renaming and creating while the
// old container still runs means a name conflict or a refused create costs no downtime. The
// first step that is not a success ends the run and every later step is recorded as skipped.
// Nothing is rolled back: the steps say where the old container was left. No volume is ever
// removed. See docs/superpowers/specs/2026-09-22-deployment-runtime-design.md,
// 2026-09-23-pull-step-design.md and 2026-09-24-volumes-design.md.
func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.Validate(time.Now()); err != nil {
		res.Outcome, res.Detail = protocol.OutcomeDenied, "the deployment request is invalid"
		return res
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	r := &deployRun{c: c, parent: parent, req: req, res: res, ensured: map[string]bool{}, keepOnly: map[string]bool{}}
	// The daemon default runtime is what a container created without one gets; read once per run.
	ictx, icancel := context.WithTimeout(ctx, callBudget)
	var info struct{ DefaultRuntime string }
	if r.c.get(ictx, "/info", &info) == nil && info.DefaultRuntime != "" {
		r.defaultRuntime = info.DefaultRuntime
	}
	icancel()
	r.pullDeadline = time.Now().Add(pullPhase(time.Until(req.Deadline)))
	// Every service is checked and its image made present before any container is touched, so
	// a refused precondition or a failed pull on any service leaves the host unchanged.
	ready := make([]prepared, 0, len(req.Services))
	for _, s := range req.Services {
		ready = append(ready, r.prepare(ctx, s))
	}
	for _, p := range ready {
		r.replace(ctx, p)
	}
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

type deployRun struct {
	c              *Client
	parent         context.Context
	req            protocol.DeploymentRequest
	res            protocol.DeploymentResult
	defaultRuntime string          // "" when it could not be read; the first precondition then fails
	pullDeadline   time.Time       // shared by every pull; see pullPhase
	ensured        map[string]bool // volumes whose step is recorded
	keepOnly       map[string]bool // existing volumes not the project's own: each service may only keep its mounts of them
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
// no answer, a cancelled parent means the run was cancelled (agent shutdown under the
// detached-context contract) before the runtime answered, and the Engine may have acted:
// unknown, never retried by itself.
func (r *deployRun) outcomeFor(ctx context.Context, err error, status int) (string, string) {
	if statusOf(err) != 0 {
		err = nil
	}
	switch {
	case err != nil && r.parent.Err() == context.Canceled:
		return protocol.OutcomeUnknown, "the run was cancelled before the runtime answered"
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
	Mounts  *[]inspectedMount
	Config  *struct {
		imageDefaults
		User string
	}
	HostConfig *struct {
		NetworkMode                                                     string
		Privileged, AutoRemove, ReadonlyRootfs                          *bool
		Tmpfs, Sysctls                                                  map[string]string
		CapAdd, CapDrop, SecurityOpt, GroupAdd, ExtraHosts, Links       []string
		Dns, DnsOptions, DnsSearch                                      []string
		Devices, Ulimits, DeviceRequests                                []json.RawMessage
		PidMode, IpcMode, Runtime, UsernsMode, CgroupParent, CpusetCpus string
		Memory, MemorySwap, MemoryReservation, NanoCpus                 int64
		CpuShares, CpuQuota                                             int64
		PidsLimit                                                       *int64
		Init                                                            *bool
		VolumesFrom                                                     []string
		VolumeDriver                                                    string
		Mounts                                                          []struct {
			BindOptions *struct {
				Propagation                                                string
				NonRecursive, ReadOnlyNonRecursive, ReadOnlyForceRecursive bool
			}
			VolumeOptions *struct {
				NoCopy       bool
				Subpath      string
				DriverConfig *struct{ Name string }
			}
		}
	}
	NetworkSettings *struct {
		Networks map[string]json.RawMessage
	}
}

type inspectedMount struct {
	Type, Name, Source, Destination, Mode, Propagation string
	RW                                                 bool
}

// anonymousVolume is the name Docker generates for a volume nobody named.
var anonymousVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

// mountOptions reports an option on the old container's mounts the recreate would drop: a
// propagation other than the default, nocopy, a volume subpath or driver, recursion settings.
func mountOptions(in inspectedForDeploy) bool {
	for _, m := range *in.Mounts {
		if (m.Propagation != "" && m.Propagation != "rprivate") || slices.Contains(strings.Split(m.Mode, ","), "nocopy") {
			return true
		}
	}
	for _, m := range in.HostConfig.Mounts {
		if b := m.BindOptions; b != nil && ((b.Propagation != "" && b.Propagation != "rprivate") || b.NonRecursive || b.ReadOnlyNonRecursive || b.ReadOnlyForceRecursive) {
			return true
		}
		if v := m.VolumeOptions; v != nil && (v.NoCopy || v.Subpath != "" || (v.DriverConfig != nil && v.DriverConfig.Name != "" && v.DriverConfig.Name != "local")) {
			return true
		}
	}
	return false
}

// imageDefaults are the Config fields a container inherits from its image. A container whose
// values differ was given them at run time, and recreation from the image would drop them.
type imageDefaults struct {
	Cmd, Entrypoint        []string
	Healthcheck            *healthcheck
	WorkingDir, StopSignal string
}

type healthcheck struct {
	Test                           []string
	Interval, Timeout, StartPeriod int64
	Retries                        int
}

// none reports no healthcheck: absent, no test, or the image's explicit ["NONE"].
func (h *healthcheck) none() bool {
	return h == nil || len(h.Test) == 0 || (len(h.Test) == 1 && h.Test[0] == "NONE")
}

func (a imageDefaults) differs(b imageDefaults) string {
	switch {
	case !slices.Equal(a.Cmd, b.Cmd) || !slices.Equal(a.Entrypoint, b.Entrypoint):
		return "command"
	case a.Healthcheck.none() != b.Healthcheck.none() || (!a.Healthcheck.none() && !reflect.DeepEqual(a.Healthcheck, b.Healthcheck)):
		return "healthcheck"
	case a.WorkingDir != b.WorkingDir:
		return "working directory"
	case a.StopSignal != b.StopSignal:
		return "stop signal"
	}
	return ""
}

const cannotExpress = "the container has configuration the definition cannot express: "

// undescribed refuses the listed configuration recreation would drop, returning the denial
// detail or "" when there is none. The definition expresses image, env, ports, restart, the
// project network and volume and bind mounts only; log configuration and settings outside this
// list and imageDefaults are not compared.
func undescribed(in inspectedForDeploy, projectNetwork, defaultRuntime string) string {
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
	case slices.ContainsFunc(*in.Mounts, func(m inspectedMount) bool { return m.Type != "volume" && m.Type != "bind" }):
		return cannotExpress + "mounts"
	case slices.ContainsFunc(*in.Mounts, func(m inspectedMount) bool { return m.Type == "volume" && anonymousVolume.MatchString(m.Name) }):
		return cannotExpress + "anonymous volumes"
	case len(h.VolumesFrom) > 0:
		return cannotExpress + "volumes-from"
	case h.VolumeDriver != "" && h.VolumeDriver != "local":
		return cannotExpress + "volume driver"
	case mountOptions(in):
		return cannotExpress + "mount options"
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
	case h.IpcMode != "" && h.IpcMode != "private" && h.IpcMode != "shareable": // daemon defaults recreation reproduces
		return cannotExpress + "IPC mode"
	case in.Config.User != "":
		return cannotExpress + "user"
	case h.Runtime != "" && h.Runtime != defaultRuntime:
		return cannotExpress + "runtime"
	case h.Memory > 0 || h.MemorySwap > 0 || h.MemoryReservation > 0:
		return cannotExpress + "memory limits"
	case h.NanoCpus > 0 || h.CpuShares > 0 || h.CpuQuota > 0 || h.CpusetCpus != "":
		return cannotExpress + "CPU limits"
	case h.PidsLimit != nil && *h.PidsLimit != 0:
		return cannotExpress + "PID limit"
	case len(h.Ulimits) > 0:
		return cannotExpress + "ulimits"
	case len(h.Sysctls) > 0:
		return cannotExpress + "sysctls"
	case len(h.DeviceRequests) > 0:
		return cannotExpress + "device requests"
	case h.Init != nil && *h.Init:
		return cannotExpress + "init"
	case h.UsernsMode != "":
		return cannotExpress + "user namespace"
	case h.CgroupParent != "":
		return cannotExpress + "cgroup parent"
	case len(h.GroupAdd) > 0:
		return cannotExpress + "supplementary groups"
	case len(h.ExtraHosts) > 0:
		return cannotExpress + "extra hosts"
	case len(h.Dns) > 0 || len(h.DnsOptions) > 0 || len(h.DnsSearch) > 0:
		return cannotExpress + "DNS"
	case len(h.Links) > 0:
		return cannotExpress + "links"
	case h.NetworkMode != "default" && h.NetworkMode != "bridge" && h.NetworkMode != projectNetwork:
		return cannotExpress + "network mode"
	case len(n.Networks) != 1 || !onNetwork:
		return cannotExpress + "networks"
	}
	return ""
}

// prepared is what a service's precondition and image (or pull) steps settled for its replacement.
type prepared struct {
	s           protocol.DeploymentService // ImageID is the pulled ID for a pulled service
	name        string                     // the old container's name, without the leading slash
	networkMode string
}

func (r *deployRun) prepare(ctx context.Context, s protocol.DeploymentService) prepared {
	old := url.PathEscape(s.Replaces.ContainerID)
	var before inspectedForDeploy
	var networkMode string
	var oldMounts []inspectedMount
	r.step(s.Name, protocol.StepPrecondition, func() (string, string) {
		if r.defaultRuntime == "" {
			return protocol.OutcomeFailed, "the daemon's default runtime could not be read"
		}
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
		if detail := undescribed(before, r.req.Project+"_default", r.defaultRuntime); detail != "" {
			return protocol.OutcomeDenied, detail
		}
		// No mounts key is a server older than mounts: it relied on the agent refusing them.
		if s.Mounts == nil && len(*before.Mounts) > 0 {
			return protocol.OutcomeDenied, cannotExpress + "mounts"
		}
		// Binds are preserve-only: a deploy never introduces a host path.
		for _, m := range s.Mounts {
			if m.Kind == protocol.MountBind && !slices.ContainsFunc(*before.Mounts, func(o inspectedMount) bool {
				return o.Type == "bind" && o.Source == m.Source && o.Destination == m.Target && o.RW == !m.ReadOnly
			}) {
				return protocol.OutcomeDenied, "bind mount not present on the container"
			}
			if m.Kind == protocol.MountVolume && r.keepOnly[m.Source] && !keeps(*before.Mounts, m) {
				return protocol.OutcomeDenied, notPresent
			}
		}
		ictx, icancel := context.WithTimeout(ctx, callBudget)
		defer icancel()
		var im struct{ Config imageDefaults }
		if err := r.c.get(ictx, "/images/"+url.PathEscape(s.Replaces.ImageID)+"/json", &im); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return protocol.OutcomeDenied, "the container's image is no longer present"
			}
			return r.outcomeFor(ictx, err, statusOf(err))
		}
		if field := before.Config.differs(im.Config); field != "" {
			return protocol.OutcomeDenied, cannotExpress + field
		}
		networkMode, oldMounts = before.HostConfig.NetworkMode, *before.Mounts
		return protocol.OutcomeSucceeded, ""
	})
	r.ensureVolumes(ctx, s, oldMounts)
	if s.Pull != nil {
		r.step(s.Name, protocol.StepPull, func() (string, string) {
			outcome, detail, id := r.pull(ctx, s)
			s.ImageID = id // the replacement is created from, and verified against, the pulled ID
			return outcome, detail
		})
	} else {
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
	}
	return prepared{s: s, name: strings.TrimPrefix(before.Name, "/"), networkMode: networkMode}
}

func (r *deployRun) replace(ctx context.Context, p prepared) {
	s, old := p.s, url.PathEscape(p.s.Replaces.ContainerID)
	r.step(s.Name, protocol.StepRename, func() (string, string) {
		if time.Until(r.req.Deadline) < replaceBudget {
			return protocol.OutcomeTimedOut, "not enough time left before the deadline to replace this service safely"
		}
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		name := p.name + ".kyyard-prev-" + r.req.Deployment[:8]
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
		status, err := r.c.postJSON(cctx, "/containers/create?name="+url.QueryEscape(s.ContainerName), createBody(r.req, s, p.networkMode), &out)
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
			outcome, detail := r.outcomeFor(cctx, err, status)
			return outcome, fmt.Sprintf("%s (container %s)", detail, created)
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
		if s.Pull != nil {
			id.ImageDigest = s.Pull.Digest
		}
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
type mountSpec struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}
type containerCreate struct {
	Image        string              `json:"Image"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   struct {
		NetworkMode   string                   `json:"NetworkMode,omitempty"`
		PortBindings  map[string][]portBinding `json:"PortBindings,omitempty"`
		Mounts        []mountSpec              `json:"Mounts,omitempty"`
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
	for _, m := range s.Mounts {
		body.HostConfig.Mounts = append(body.HostConfig.Mounts, mountSpec{Type: m.Kind, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
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
