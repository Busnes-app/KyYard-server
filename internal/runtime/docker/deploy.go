// internal/runtime/docker/deploy.go
package docker

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
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

// replaceBudget is the time one service's phase-two steps may need: stop and start at
// operationBudget, recheck, rename, create and the identity read at callBudget.
const replaceBudget = 2*operationBudget + 4*callBudget

// rollbackBudget is an explicit recreate's reserve for undoing a failed start: remove and
// rename at callBudget, restart at operationBudget.
const rollbackBudget = operationBudget + 2*callBudget

// startWatch is how long an explicit recreate's new container must stay running (or leave the
// starting health state) before its start counts, read every startPoll. watchBudget is the
// watch with its last read.
const (
	startWatch  = 5 * time.Second
	startPoll   = 500 * time.Millisecond
	watchBudget = startWatch + callBudget
)

// Deploy replaces each service's mapped container with one created from the pinned image ID.
// First every service's precondition and image (pull, for a service naming a digest), in plan
// order, with each frame volume ensured after the precondition of the first service mounting
// it; then per service recheck, rename, create, stop, start, remove. Renaming and creating while the
// old container still runs means a name conflict or a refused create costs no downtime. The
// first step that is not a success ends the run and every later step is recorded as skipped.
// Only an explicit recreate puts the host back: a failed create or stop is undone, and a failed
// start, which includes a new container that stops running or restarts within startWatch when
// the old one was running, is rolled back (a rollback step); otherwise the steps say where the
// old container was left. An explicit run (no Replaces) is image, create, start. No volume is
// ever removed. See docs/superpowers/specs/2026-09-22-deployment-runtime-design.md,
// 2026-09-23-pull-step-design.md and 2026-09-24-volumes-design.md.
// started is called once, immediately before the first phase-two call (after the first recheck's
// deadline guard, or an explicit run's create guard); a run that ends in phase one never calls it.
func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.ValidateFor(protocol.RuntimeDocker, time.Now()); err != nil {
		return refused(res, err)
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	r := &deployRun{c: c, parent: parent, req: req, res: res, ensured: map[string]bool{}, keepOnly: map[string]bool{}, started: started}
	// The daemon's defaults are what a container created without a setting gets; read once per run.
	ictx, icancel := context.WithTimeout(ctx, callBudget)
	var info daemon
	if r.c.get(ictx, "/info", &info) == nil && info.DefaultRuntime != "" {
		r.defaultRuntime, r.cgroupVersion = info.DefaultRuntime, info.CgroupVersion
	}
	icancel()
	remaining := time.Until(req.Deadline)
	if req.Explicit {
		remaining -= rollbackBudget + watchBudget // the recheck guard reserves them after the pulls
	}
	r.pullDeadline = time.Now().Add(pullPhase(remaining))
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
	cgroupVersion  string          // /info CgroupVersion; "" reads as 2
	pullDeadline   time.Time       // shared by every pull; see pullPhase
	ensured        map[string]bool // volumes whose step is recorded
	keepOnly       map[string]bool // existing volumes not the project's own: each service may only keep its mounts of them
	started        func()          // called once before the run first reads or changes a container in phase two
	begun          bool
}

// begin tells the caller, once, that the run is about to change the host.
func (r *deployRun) begin() {
	if !r.begun {
		r.begun = true
		r.started()
	}
}

// refused answers a frame Validate refused, with no step run: invalid_request, or clock_skew
// failed. A request ID that is not valid is not echoed.
func refused(res protocol.DeploymentResult, err error) protocol.DeploymentResult {
	if !protocol.ValidRequestID(res.RequestID) {
		res.RequestID = ""
	}
	res.Outcome, res.Code = protocol.OutcomeDenied, protocol.ResultInvalidRequest
	if errors.Is(err, protocol.ErrClockSkew) {
		res.Outcome, res.Code = protocol.OutcomeFailed, protocol.ResultClockSkew
	}
	return res
}

// step records one outcome with its code and the code's parameter. The first non-success fixes
// the run's outcome, coded step_failed: the steps say which.
func (r *deployRun) step(service, step string, run func() (outcome, code, detail string)) {
	if r.res.Outcome != "" {
		r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: step, Outcome: protocol.OutcomeSkipped})
		return
	}
	outcome, code, detail := run()
	s := protocol.DeploymentStep{Service: service, Step: step, Outcome: outcome}
	if outcome != protocol.OutcomeSucceeded {
		s.Code, s.Detail = code, detail
		r.res.Outcome, r.res.Code = outcome, protocol.ResultStepFailed
	}
	r.res.Steps = append(r.res.Steps, s)
}

// succeeded, deny and fail are a step's plain answers.
func succeeded() (string, string, string)       { return protocol.OutcomeSucceeded, "", "" }
func deny(code string) (string, string, string) { return protocol.OutcomeDenied, code, "" }
func fail(code string) (string, string, string) { return protocol.OutcomeFailed, code, "" }

// outcomeFor classifies a call that did not succeed. A status is an answer: failed,
// runtime_status with the status. With no answer, a cancelled parent means the run was cancelled
// (agent shutdown under the detached-context contract) before the runtime answered, and the
// Engine may have acted: unknown, never retried by itself.
func (r *deployRun) outcomeFor(ctx context.Context, err error, status int) (string, string, string) {
	if statusOf(err) != 0 {
		err = nil
	}
	switch {
	case err != nil && r.parent.Err() == context.Canceled:
		return protocol.OutcomeUnknown, "cancelled", ""
	case err != nil && ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "runtime_timeout", ""
	case err != nil, status < 100, status > 599:
		return fail("runtime_error")
	}
	return protocol.OutcomeFailed, "runtime_status", strconv.Itoa(status)
}

// inspectedForDeploy is decoded with pointers so a field the Engine did not report is refused
// rather than read as its zero value.
type inspectedForDeploy struct {
	ID      string `json:"Id"`
	Image   string
	Name    string
	Created time.Time
	Mounts  *mountList
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
			TmpfsOptions *struct {
				SizeBytes, Mode int64
				Options         [][]string
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

// mountList decodes sorted: the Engine builds Mounts from a map, so two reads of one
// container may list them in different orders.
type mountList []inspectedMount

func (l *mountList) UnmarshalJSON(b []byte) error {
	var m []inspectedMount
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	slices.SortFunc(m, func(a, b inspectedMount) int {
		return cmp.Or(strings.Compare(a.Destination, b.Destination), strings.Compare(a.Source, b.Source))
	})
	*l = m
	return nil
}

// anonymousVolume is the name Docker generates for a volume nobody named.
var anonymousVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

// mountOptions reports an option on the old container's mounts the recreate would drop: a
// propagation other than the default, nocopy, a volume subpath or driver, recursion settings,
// a tmpfs size, mode or flag.
func mountOptions(in inspectedForDeploy) bool {
	for _, options := range in.HostConfig.Tmpfs {
		if slices.ContainsFunc(strings.Split(options, ","), func(o string) bool { return o != "" && o != "rw" && o != "ro" }) {
			return true
		}
	}
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
		if o := m.TmpfsOptions; o != nil && (o.SizeBytes != 0 || o.Mode != 0 || len(o.Options) > 0) {
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

// differs reports a Cmd, Entrypoint, Healthcheck, WorkingDir or StopSignal the container was
// given at run time: recreation from the image would drop it (code image_config).
func (a imageDefaults) differs(b imageDefaults) bool {
	return !slices.Equal(a.Cmd, b.Cmd) || !slices.Equal(a.Entrypoint, b.Entrypoint) ||
		a.Healthcheck.none() != b.Healthcheck.none() || (!a.Healthcheck.none() && !reflect.DeepEqual(a.Healthcheck, b.Healthcheck)) ||
		a.WorkingDir != b.WorkingDir || a.StopSignal != b.StopSignal
}

// reported says the Engine returned every field undescribed reads through a pointer; an absent
// one is refused rather than read as its zero value.
func reported(in inspectedForDeploy) bool {
	h := in.HostConfig
	return h != nil && in.Config != nil && in.NetworkSettings != nil && in.Mounts != nil && h.Privileged != nil && h.AutoRemove != nil && h.ReadonlyRootfs != nil
}

// cappedLists decodes the capped lists of a configuration read that inspectedForDeploy does not.
type cappedLists struct {
	Config     struct{ ExposedPorts map[string]struct{} }
	HostConfig struct {
		PortBindings map[string][]struct{ HostPort string }
		LogConfig    struct{ Config map[string]string }
		Devices      []device
	}
	NetworkSettings struct {
		Networks map[string]struct{ Aliases []string }
	}
}

// overCap reports a list ReadConfiguration would cut (list_truncated:<field>): an explicit frame
// built from that read cannot carry all of it. in must be reported.
func (l cappedLists) overCap(in inspectedForDeploy) bool {
	o, h, entry := &configurationRead{}, in.HostConfig, fits(protocol.MaxListEntryBytes)
	for _, strs := range [][]string{h.CapAdd, h.CapDrop, h.SecurityOpt, h.ExtraHosts, h.Dns} {
		list(o, "", strs, entry)
	}
	list(o, "", l.HostConfig.Devices, deviceFits)
	if m := h.NetworkMode; m != "host" && m != "none" && !strings.HasPrefix(m, "container:") {
		for _, n := range l.NetworkSettings.Networks {
			list(o, "", slices.DeleteFunc(slices.Clone(n.Aliases), func(a string) bool { return a == in.ID[:min(12, len(in.ID))] }), fits(256))
		}
		if len(l.NetworkSettings.Networks) > protocol.MaxConfigurationNetworks {
			return true
		}
	}
	ports, bound := 0, map[string]bool{}
	for key, bindings := range l.HostConfig.PortBindings {
		if _, _, ok := parsePortKey(key); !ok {
			return true
		}
		for _, b := range bindings {
			if b.HostPort == "" {
				return true // pinned or dropped by the read
			}
			ports, bound[key] = ports+1, true
		}
	}
	for key := range l.Config.ExposedPorts {
		if _, _, ok := parsePortKey(key); !ok {
			return true
		}
		if !bound[key] {
			ports++
		}
	}
	mounts := len(*in.Mounts)
	for _, m := range *in.Mounts {
		if m.Type != "volume" && m.Type != "bind" && m.Type != "tmpfs" {
			return true
		}
	}
	for target := range h.Tmpfs {
		if !slices.ContainsFunc(*in.Mounts, func(m inspectedMount) bool { return m.Destination == target }) {
			mounts++
		}
	}
	return len(o.truncated) > 0 || len(l.HostConfig.LogConfig.Config) > protocol.MaxLogOptions || ports > protocol.MaxDeploymentPorts || mounts > protocol.MaxMounts
}

// sameConfiguration compares what recreation depends on: Config, HostConfig and Mounts. State
// and NetworkSettings change on their own (a restart) and are not compared.
func sameConfiguration(a, b inspectedForDeploy) bool {
	return reflect.DeepEqual(a.Config, b.Config) && reflect.DeepEqual(a.HostConfig, b.HostConfig) && reflect.DeepEqual(a.Mounts, b.Mounts)
}

// undescribed lists, as codes of protocol.UnsupportedCodes in its order, the configuration
// recreation would drop. The definition expresses image, env, ports, restart, the project
// network and volume and bind mounts only; log configuration and settings outside this list
// are not compared, and image_config is the caller's, compared against the image. in must be
// reported.
func undescribed(in inspectedForDeploy, projectNetwork, defaultRuntime string) []string {
	h, n, mounts := in.HostConfig, in.NetworkSettings, *in.Mounts
	network := projectNetwork
	if h.NetworkMode == "default" || h.NetworkMode == "bridge" {
		network = "bridge"
	}
	_, onNetwork := n.Networks[network]
	checks := []struct {
		code  string
		found bool
	}{
		{"mount_type", slices.ContainsFunc(mounts, func(m inspectedMount) bool { return m.Type != "volume" && m.Type != "bind" })},
		{"anonymous_volume", slices.ContainsFunc(mounts, func(m inspectedMount) bool { return m.Type == "volume" && anonymousVolume.MatchString(m.Name) })},
		{"volumes_from", len(h.VolumesFrom) > 0},
		{"volume_driver", h.VolumeDriver != "" && h.VolumeDriver != "local"},
		{"mount_options", mountOptions(in)},
		{"tmpfs", len(h.Tmpfs) > 0},
		{"auto_remove", *h.AutoRemove},
		{"read_only_rootfs", *h.ReadonlyRootfs},
		{"privileged", *h.Privileged},
		{"capabilities", len(h.CapAdd) > 0 || len(h.CapDrop) > 0},
		{"security_opt", len(h.SecurityOpt) > 0},
		{"devices", len(h.Devices) > 0},
		{"pid_mode", h.PidMode != "" && h.PidMode != "private"},
		{"ipc_mode", h.IpcMode != "" && h.IpcMode != "private" && h.IpcMode != "shareable"}, // daemon defaults recreation reproduces
		{"user", in.Config.User != ""},
		{"runtime", h.Runtime != "" && h.Runtime != defaultRuntime},
		{"resource_limits", h.Memory > 0 || h.MemorySwap > 0 || h.MemoryReservation > 0 || h.NanoCpus > 0 || h.CpuShares > 0 || h.CpuQuota > 0 || h.CpusetCpus != "" || (h.PidsLimit != nil && *h.PidsLimit != 0)},
		{"ulimits", len(h.Ulimits) > 0},
		{"sysctls", len(h.Sysctls) > 0},
		{"device_requests", len(h.DeviceRequests) > 0},
		{"init", h.Init != nil && *h.Init},
		{"userns_mode", h.UsernsMode != ""},
		{"cgroup_parent", h.CgroupParent != ""},
		{"group_add", len(h.GroupAdd) > 0},
		{"extra_hosts", len(h.ExtraHosts) > 0},
		{"dns", len(h.Dns) > 0 || len(h.DnsOptions) > 0 || len(h.DnsSearch) > 0},
		{"links", len(h.Links) > 0},
		{"network", (h.NetworkMode != "default" && h.NetworkMode != "bridge" && h.NetworkMode != projectNetwork) || len(n.Networks) != 1 || !onNetwork},
	}
	codes := []string{}
	for _, c := range checks {
		if c.found {
			codes = append(codes, c.code)
		}
	}
	return codes
}

// unsupported is the precondition's refusal for configuration a recreate would drop: the codes
// joined by ",", as many whole ones as the step bound holds. The plan's live inspection already
// listed every one.
func unsupported(codes []string) (string, string, string) {
	detail := codes[0]
	for _, c := range codes[1:] {
		if len(detail)+1+len(c) > protocol.MaxDeploymentStepDetailBytes {
			break
		}
		detail += "," + c
	}
	return protocol.OutcomeDenied, "unsupported", detail
}

// prepared is what a service's precondition and image (or pull) steps settled for its replacement.
type prepared struct {
	s           protocol.DeploymentService // ImageID is the pulled ID for a pulled service
	name        string                     // the old container's name, without the leading slash
	networkMode string
	before      inspectedForDeploy // the precondition's read; recheck compares against it
	raw         rawInspection      // the same read by key; an explicit recheck compares it too
}

func (r *deployRun) prepare(ctx context.Context, s protocol.DeploymentService) prepared {
	old := url.PathEscape(s.Replaces.ContainerID)
	var before inspectedForDeploy
	var raw rawInspection
	var networkMode string
	var oldMounts []inspectedMount
	if run(r.req, s) {
		// An unacknowledged bind is denied at create; nothing is pulled or created first.
		if !unacknowledgedBind(s) {
			r.ensureVolumes(ctx, s, nil)
			r.image(ctx, &s)
		}
		return prepared{s: s}
	}
	r.step(s.Name, protocol.StepPrecondition, func() (string, string, string) {
		if r.defaultRuntime == "" && !r.req.Explicit {
			return fail("runtime_unreadable")
		}
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var lists cappedLists
		if err := r.c.get(cctx, "/containers/"+old+"/json", &before, &raw, &lists); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return deny("container_missing")
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		if before.ID != s.Replaces.ContainerID || before.Image != s.Replaces.ImageID || before.Created.Unix() != s.Replaces.CreatedUnix {
			return deny("identity_mismatch")
		}
		if !reported(before) {
			return deny("configuration_unreported")
		}
		// An explicit frame carries every setting the configuration read carries, so only what
		// that read would name is refused: the same codes, whatever the frame's sender claimed.
		codes := undescribed(before, r.req.Project+"_default", r.defaultRuntime)
		if r.req.Explicit {
			set := map[string]bool{}
			for _, c := range codes {
				set[c] = slices.Contains(protocol.ConfigurationOnlyCodes, c)
			}
			if !raw.uncarried(r.cgroupVersion, set) || lists.overCap(before) {
				return deny("configuration_unreported")
			}
			codes = orderedCodes(set)
		}
		if len(codes) > 0 {
			return unsupported(codes)
		}
		// Binds are preserve-only unless the operator acknowledged the host path: an old bind
		// covers a new one at its source and target in its mode, or (an explicit edit, the
		// server's rule) read-only where the old one was writable.
		for _, m := range s.Mounts {
			if m.Kind == protocol.MountBind && (s.Explicit == nil || !slices.Contains(s.Explicit.AcknowledgedBinds, m.Source)) && !slices.ContainsFunc(*before.Mounts, func(o inspectedMount) bool {
				return o.Type == "bind" && o.Source == m.Source && o.Destination == m.Target && (o.RW == !m.ReadOnly || (s.Explicit != nil && m.ReadOnly))
			}) {
				return deny("bind_missing")
			}
			if m.Kind == protocol.MountVolume && r.keepOnly[m.Source] && !keeps(*before.Mounts, m) {
				return deny("volume_mount_missing")
			}
		}
		networkMode, oldMounts = before.HostConfig.NetworkMode, *before.Mounts
		if r.req.Explicit {
			return succeeded()
		}
		ictx, icancel := context.WithTimeout(ctx, callBudget)
		defer icancel()
		var im struct{ Config imageDefaults }
		if err := r.c.get(ictx, "/images/"+url.PathEscape(s.Replaces.ImageID)+"/json", &im); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return deny("image_missing")
			}
			return r.outcomeFor(ictx, err, statusOf(err))
		}
		if before.Config.differs(im.Config) {
			return unsupported([]string{"image_config"})
		}
		return succeeded()
	})
	r.ensureVolumes(ctx, s, oldMounts)
	r.image(ctx, &s)
	return prepared{s: s, name: strings.TrimPrefix(before.Name, "/"), networkMode: networkMode, before: before, raw: raw}
}

// run reports an explicit frame that creates a container rather than replacing one.
func run(req protocol.DeploymentRequest, s protocol.DeploymentService) bool {
	return req.Explicit && s.Replaces == (protocol.InspectionTarget{})
}

// unacknowledgedBind reports a run's bind whose host path the operator did not confirm.
func unacknowledgedBind(s protocol.DeploymentService) bool {
	return slices.ContainsFunc(s.Mounts, func(m protocol.Mount) bool {
		return m.Kind == protocol.MountBind && !slices.Contains(s.Explicit.AcknowledgedBinds, m.Source)
	})
}

// image records the pull step, or checks the pinned image is present. A pull sets s.ImageID.
func (r *deployRun) image(ctx context.Context, s *protocol.DeploymentService) {
	if s.Pull != nil {
		r.step(s.Name, protocol.StepPull, func() (string, string, string) {
			outcome, code, detail, id := r.pull(ctx, *s)
			s.ImageID = id // the replacement is created from, and verified against, the pulled ID
			return outcome, code, detail
		})
	} else {
		r.step(s.Name, protocol.StepImage, func() (string, string, string) {
			cctx, cancel := context.WithTimeout(ctx, callBudget)
			defer cancel()
			var im struct {
				ID string `json:"Id"`
			}
			if err := r.c.get(cctx, "/images/"+url.PathEscape(s.ImageID)+"/json", &im); err != nil {
				if statusOf(err) == http.StatusNotFound {
					return fail("pinned_image_missing")
				}
				return r.outcomeFor(cctx, err, statusOf(err))
			}
			if im.ID != s.ImageID {
				return fail("image_identity_mismatch")
			}
			return succeeded()
		})
	}
}

func (r *deployRun) replace(ctx context.Context, p prepared) {
	s, old := p.s, url.PathEscape(p.s.Replaces.ContainerID)
	if run(r.req, s) {
		created := r.create(ctx, p)
		if outcome, up := r.start(ctx, s, created, false); failedStep(outcome) && !up && !r.settleRun(ctx, s, created, outcome) {
			r.rollbackFailed(s.Name)
		}
		return
	}
	budget := replaceBudget
	if r.req.Explicit {
		budget += rollbackBudget + watchBudget
	}
	// The old container's state at the recheck, the last read before stop: a rollback pauses it
	// again, and an unanswered stop is checked only if it was running.
	paused, wasRunning := false, false
	// The pull window can be minutes: re-read the container right before touching it.
	r.step(s.Name, protocol.StepRecheck, func() (string, string, string) {
		if time.Until(r.req.Deadline) < budget {
			return protocol.OutcomeTimedOut, "deadline", ""
		}
		r.begin()
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var now inspectedForDeploy
		var raw rawInspection
		var state struct {
			State *struct{ Running, Paused bool }
		}
		if err := r.c.get(cctx, "/containers/"+old+"/json", &now, &raw, &state); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return deny("container_missing")
			}
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		// An explicit frame's precondition checked every key, so every key must still hold.
		if now.ID != s.Replaces.ContainerID || now.Image != s.Replaces.ImageID || now.Created.Unix() != s.Replaces.CreatedUnix || !sameConfiguration(p.before, now) ||
			(r.req.Explicit && !p.raw.sameKeys(raw)) {
			return deny("configuration_drift")
		}
		paused = state.State != nil && state.State.Paused
		wasRunning = state.State != nil && state.State.Running
		return succeeded()
	})
	unparked := true
	r.step(s.Name, protocol.StepRename, func() (outcome, code, detail string) {
		name := parked(s.ContainerName, r.req.Deployment)
		defer func() {
			if unanswered(outcome) && r.req.Explicit {
				unparked = r.unparkIfParked(ctx, p, name)
			}
		}()
		if p.name == name {
			return succeeded() // this deployment parked it already
		}
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+old+"/rename?name="+url.QueryEscape(name))
		if err != nil || status >= 400 {
			if status == http.StatusConflict {
				return fail("name_reserved")
			}
			return r.outcomeFor(cctx, err, status)
		}
		return succeeded()
	})
	if !unparked {
		r.rollbackFailed(s.Name)
	}
	created := r.create(ctx, p)
	running, restored := false, true // running: the old container was running when stopped, and a rollback starts it again
	r.step(s.Name, protocol.StepStop, func() (outcome, code, detail string) {
		defer func() {
			if outcome != protocol.OutcomeSucceeded && r.req.Explicit {
				// An unanswered stop may have stopped it: start it again if a read says so.
				restored = r.undo(ctx, p, created) && (!unanswered(outcome) || !wasRunning || r.reviveIfStopped(ctx, p, paused))
			}
		}()
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+old+"/stop?t="+strconv.Itoa(stopGrace))
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		running = status != http.StatusNotModified
		return succeeded()
	})
	if !restored {
		r.rollbackFailed(s.Name)
	}
	// A recreate's container must keep running only if the old one was: a stopped job's
	// replacement may exit as the old one did.
	if outcome, _ := r.start(ctx, s, created, r.req.Explicit && running); failedStep(outcome) && r.req.Explicit {
		r.rollback(ctx, p, created, running, paused)
	}
	r.step(s.Name, protocol.StepRemove, func() (string, string, string) {
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		status, err := r.c.del(cctx, "/containers/"+old)
		if err != nil || status >= 400 {
			return r.outcomeFor(cctx, err, status)
		}
		return succeeded()
	})
}

// create records the create step and returns the new container's ID. An explicit run has no
// recheck, so its deadline guard and the started call are here.
func (r *deployRun) create(ctx context.Context, p prepared) string {
	s := p.s
	var created string
	restored := true
	r.step(s.Name, protocol.StepCreate, func() (outcome, code, detail string) {
		defer func() {
			switch {
			case outcome == protocol.OutcomeSucceeded || !r.req.Explicit:
			case run(r.req, s) && created == "":
				// An unanswered create may have made the container anyway.
				restored = !unanswered(outcome) || r.settleRun(ctx, s, "", outcome)
			default:
				restored = r.undo(ctx, p, created)
			}
		}()
		if run(r.req, s) {
			// A run has no precondition: its binds are checked here, before any daemon call.
			if unacknowledgedBind(s) {
				return deny("bind_missing")
			}
			if time.Until(r.req.Deadline) < replaceBudget {
				return protocol.OutcomeTimedOut, "deadline", ""
			}
			r.begin()
		}
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		defer cancel()
		var out struct {
			ID string `json:"Id"`
		}
		status, err := r.c.postJSON(cctx, "/containers/create?name="+url.QueryEscape(s.ContainerName), createBody(r.req, s, p.networkMode), &out)
		if err != nil || status != http.StatusCreated {
			if status == http.StatusConflict {
				return fail("name_taken")
			}
			return r.outcomeFor(cctx, err, status)
		}
		if !protocol.ValidExecID(out.ID) {
			return fail("identity_unusable")
		}
		created = out.ID
		// Create accepts one network; the rest are connected before start.
		if s.Explicit != nil {
			_, rest := attachments(s.Explicit)
			for _, n := range rest {
				body := struct {
					Container      string           `json:"Container"`
					EndpointConfig endpointSettings `json:"EndpointConfig"`
				}{created, endpoint(n)}
				nctx, ncancel := context.WithTimeout(ctx, callBudget)
				status, err := r.c.postJSON(nctx, "/networks/"+url.PathEscape(n.Name)+"/connect", body, nil)
				if err != nil || status != http.StatusOK {
					defer ncancel()
					return r.outcomeFor(nctx, err, status)
				}
				ncancel()
			}
		}
		return succeeded()
	})
	if !restored {
		r.rollbackFailed(s.Name)
	}
	return created
}

// start records the start step and returns its outcome (skipped when not run), and whether the
// container was up (started, through any watch) so only its identity read failed. A watched
// start must keep running through the watch.
func (r *deployRun) start(ctx context.Context, s protocol.DeploymentService, created string, watched bool) (result string, up bool) {
	result = protocol.OutcomeSkipped
	r.step(s.Name, protocol.StepStart, func() (outcome, code, detail string) {
		defer func() { result = outcome }()
		cctx, cancel := context.WithTimeout(ctx, operationBudget)
		defer cancel()
		status, err := r.c.post(cctx, "/containers/"+url.PathEscape(created)+"/start")
		if err != nil || (status >= 400 && status != http.StatusNotModified) {
			return r.outcomeFor(cctx, err, status)
		}
		if watched {
			if outcome, code, detail := r.watch(ctx, created); outcome != protocol.OutcomeSucceeded {
				return outcome, code, detail
			}
		}
		up = true
		ictx, icancel := context.WithTimeout(ctx, callBudget)
		defer icancel()
		var after inspectedForDeploy
		if err := r.c.get(ictx, "/containers/"+url.PathEscape(created)+"/json", &after); err != nil {
			outcome, _, _ := r.outcomeFor(ictx, err, statusOf(err))
			return outcome, "identity_unreadable", created
		}
		id := protocol.DeploymentIdentity{Service: s.Name, ContainerID: after.ID, ImageID: after.Image, CreatedUnix: after.Created.Unix()}
		if s.Pull != nil {
			id.ImageDigest = s.Pull.Digest
		}
		if after.ID != created || after.Image != s.ImageID || (protocol.InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() != nil {
			return protocol.OutcomeFailed, "identity_unverified", created
		}
		r.res.Services = append(r.res.Services, id)
		return succeeded()
	})
	return result, up
}

// watch reads a started container every startPoll for startWatch and fails the start
// (exited_early) once it is no longer running (exited, dead or restarting), has been restarted
// by its restart policy or is unhealthy. A healthcheck turning healthy ends the watch early; one
// still starting at its end does not fail it. No poll sleeps past the window's end and the last read ends within
// callBudget of it, so the watch fits watchBudget.
func (r *deployRun) watch(ctx context.Context, id string) (string, string, string) {
	end := time.Now().Add(cmp.Or(r.c.startWatch, startWatch))
	wctx, wcancel := context.WithDeadline(ctx, end.Add(callBudget))
	defer wcancel()
	for {
		var in struct {
			RestartCount *int
			State        *struct {
				Status  string
				Running bool
				Health  *struct{ Status string }
			}
		}
		cctx, cancel := context.WithTimeout(wctx, callBudget)
		err := r.c.get(cctx, "/containers/"+url.PathEscape(id)+"/json", &in)
		if err != nil {
			defer cancel()
			return r.outcomeFor(cctx, err, statusOf(err))
		}
		cancel()
		switch st := in.State; {
		case st == nil || in.RestartCount == nil || *in.RestartCount > 0 || !st.Running || st.Status == "exited" || st.Status == "dead" || st.Status == "restarting" ||
			(st.Health != nil && st.Health.Status == "unhealthy"):
			return fail("exited_early")
		case st.Health != nil && st.Health.Status != "" && st.Health.Status != "starting" && st.Health.Status != "none":
			return succeeded()
		case !time.Now().Before(end):
			return succeeded()
		}
		select {
		case <-ctx.Done():
			return r.outcomeFor(ctx, ctx.Err(), 0)
		case <-time.After(min(cmp.Or(r.c.startPoll, startPoll), time.Until(end))):
		}
	}
}

// rollback undoes an explicit recreate whose start failed: the new container is removed by
// force, the old one renamed back and, if it was running, started, and paused again if it was
// paused. It is recorded after the failed start, which the skip rule in step would otherwise
// swallow. Success recodes the start start_failed_rolled_back; failure is rollback_failed and
// the start keeps its code.
func (r *deployRun) rollback(ctx context.Context, p prepared, created string, running, paused bool) {
	rctx, cancel := r.restoring(ctx)
	defer cancel()
	if !r.discard(rctx, created) || !r.unpark(rctx, p) || (running && !r.revive(rctx, p, paused)) {
		r.rollbackFailed(p.s.Name)
		return
	}
	failed := &r.res.Steps[len(r.res.Steps)-1]
	failed.Code, failed.Detail = "start_failed_rolled_back", ""
	r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: p.s.Name, Step: protocol.StepRollback, Outcome: protocol.OutcomeSucceeded})
}

// rollbackFailed records that the old container was not put back, after the step that failed.
func (r *deployRun) rollbackFailed(service string) {
	r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: protocol.StepRollback, Outcome: protocol.OutcomeFailed, Code: "rollback_failed"})
}

// parkedSuffix marks an old container renamed aside for a deployment's replacement.
const parkedSuffix = ".kyyard-prev-"

// parked is the name a deployment parks the old container under.
func parked(containerName, deployment string) string {
	return containerName + parkedSuffix + deployment[:8]
}

// restoring bounds one undo by rollbackBudget, detached from the run's cancellation and
// deadline: an agent shutting down mid-start still puts the old container back. Each undo opens
// one and every call it makes shares it.
func (r *deployRun) restoring(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cmp.Or(r.c.restoreBudget, rollbackBudget))
}

// revive starts the old container again and, if it was paused, pauses it.
func (r *deployRun) revive(ctx context.Context, p prepared, paused bool) bool {
	old := "/containers/" + url.PathEscape(p.s.Replaces.ContainerID)
	if status, err := r.c.post(ctx, old+"/start"); err != nil || status >= 400 {
		return false
	}
	if !paused {
		return true
	}
	status, err := r.c.post(ctx, old+"/pause")
	return err == nil && status < 400
}

// failedStep is a step outcome that is neither success nor a skip.
func failedStep(outcome string) bool {
	return outcome != protocol.OutcomeSucceeded && outcome != protocol.OutcomeSkipped
}

// unanswered is an outcome where the Engine may have acted without saying so.
func unanswered(outcome string) bool {
	return outcome == protocol.OutcomeUnknown || outcome == protocol.OutcomeTimedOut
}

var errStateUnreported = errors.New("docker: container state unreported")

// containerNow is a container's identity and state as a restore reads them.
type containerNow struct {
	ID          string `json:"Id"`
	Name, Image string
	State       *struct {
		Status  string // created, running, paused, restarting, removing, exited or dead
		Running bool
	}
}

// current reads a container's identity and state, for a restore to act on; an error means the
// state is unknown.
func (r *deployRun) current(ctx context.Context, id string) (containerNow, error) {
	var in containerNow
	if err := r.c.get(ctx, "/containers/"+url.PathEscape(id)+"/json", &in); err != nil {
		return in, err
	}
	if in.State == nil {
		return in, errStateUnreported
	}
	return in, nil
}

// settleRun removes a run's container that did not come up, so a retry finds the name free; it
// reports false when that failed. After an unanswered start or create the container may have
// started, or, with no ID, exist under the name: it is removed, by the ID read, only once a read
// shows it was never started (created) and, found by name, has the name and the frame's image.
// Gone already counts; one that cannot be read is left and reported.
func (r *deployRun) settleRun(ctx context.Context, s protocol.DeploymentService, created, outcome string) bool {
	rctx, cancel := r.restoring(ctx)
	defer cancel()
	if unanswered(outcome) {
		now, err := r.current(rctx, cmp.Or(created, s.ContainerName))
		switch {
		case statusOf(err) == http.StatusNotFound:
			return true
		case err != nil || !protocol.ValidExecID(now.ID):
			return false
		case now.State.Status != "created", created == "" && (now.Name != "/"+s.ContainerName || now.Image != s.ImageID):
			return true
		}
		created = now.ID
	}
	return r.discard(rctx, created)
}

// stopWait bounds the wait for an old container whose stop went unanswered: the daemon may still
// be inside the stop's grace period, whatever became of the request.
const stopWait = stopGrace*time.Second + callBudget

// reviveIfStopped starts the old container again after an unanswered stop, once it is not
// running. One still running when the wait ends is left running.
func (r *deployRun) reviveIfStopped(ctx context.Context, p prepared, paused bool) bool {
	rctx, cancel := r.restoring(ctx)
	defer cancel()
	now, err := r.current(rctx, p.s.Replaces.ContainerID)
	if err != nil {
		return false
	}
	if now.State.Running {
		if stopped, err := r.c.waitStopped(rctx, p.s.Replaces.ContainerID, cmp.Or(r.c.stopWait, stopWait)); err != nil || !stopped {
			return err == nil // still running when the wait ended: left running
		}
	}
	return r.revive(rctx, p, paused)
}

// waitResponse is the body of POST /containers/{id}/wait.
type waitResponse struct {
	StatusCode int
	Error      *struct{ Message string }
}

// waitStopped waits up to bound for a container to stop running. The Engine sends the wait's
// 200 headers at once and its body only once the container has stopped, so the body is the
// answer: a bound that ends while it is awaited means the container still runs (false, nil). A
// ctx that ends first is an error.
func (c *Client) waitStopped(ctx context.Context, id string, bound time.Duration) (bool, error) {
	wctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	var out waitResponse
	path := "/containers/" + url.PathEscape(id) + "/wait?condition=not-running"
	status, err := c.postJSON(wctx, path, nil, &out)
	switch {
	case status == http.StatusOK && err != nil && wctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
		return false, nil
	case err != nil:
		return false, err
	case status != http.StatusOK:
		return false, &statusError{path, status}
	case out.Error != nil:
		return false, errors.New("docker: the wait reported an error")
	}
	return true, nil
}

// unparkIfParked gives the old container its name back after an unanswered rename, if a read
// shows the daemon parked it.
func (r *deployRun) unparkIfParked(ctx context.Context, p prepared, name string) bool {
	rctx, cancel := r.restoring(ctx)
	defer cancel()
	now, err := r.current(rctx, p.s.Replaces.ContainerID)
	return err == nil && (now.Name != "/"+name || r.unpark(rctx, p))
}

// undo puts the host back after an explicit step failed before the new container started, so a
// retry is not wedged on the name: the new container, if any, is removed and the old one given
// its name back. It reports false when the new container was not removed or a recreate's old
// container did not get its name back.
func (r *deployRun) undo(ctx context.Context, p prepared, created string) bool {
	rctx, cancel := r.restoring(ctx)
	defer cancel()
	discarded := created == "" || r.discard(rctx, created)
	return discarded && (run(r.req, p.s) || r.unpark(rctx, p))
}

// discard force-removes a container this run created; gone already counts.
func (r *deployRun) discard(ctx context.Context, created string) bool {
	status, err := r.c.del(ctx, "/containers/"+url.PathEscape(created)+"?force=1")
	return err == nil && (status < 400 || status == http.StatusNotFound)
}

// unpark gives the old container its name back, without any parked suffix.
func (r *deployRun) unpark(ctx context.Context, p prepared) bool {
	name, _, _ := strings.Cut(p.name, parkedSuffix)
	status, err := r.c.post(ctx, "/containers/"+url.PathEscape(p.s.Replaces.ContainerID)+"/rename?name="+url.QueryEscape(name))
	return err == nil && status < 400
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}
type endpointSettings struct {
	Aliases    []string `json:"Aliases"`
	IPAMConfig *struct {
		IPv4Address string `json:"IPv4Address"`
	} `json:"IPAMConfig,omitempty"`
}
type device struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"`
}
type logConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
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

// containerCreate is the create body. Fields after Labels are set only for an explicit frame;
// Cmd and Entrypoint are sent even when nil, which Docker reads as the image's, so an explicit
// [] still clears an image entrypoint.
type containerCreate struct {
	Image        string              `json:"Image"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
	User         string              `json:"User,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Hostname     string              `json:"Hostname,omitempty"`
	Tty          bool                `json:"Tty,omitempty"`
	OpenStdin    bool                `json:"OpenStdin,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	StopTimeout  *int                `json:"StopTimeout,omitempty"`
	Healthcheck  *healthcheck        `json:"Healthcheck,omitempty"`
	HostConfig   struct {
		NetworkMode   string                   `json:"NetworkMode,omitempty"`
		PortBindings  map[string][]portBinding `json:"PortBindings,omitempty"`
		Mounts        []mountSpec              `json:"Mounts,omitempty"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		Memory         int64      `json:"Memory,omitempty"`
		MemorySwap     int64      `json:"MemorySwap,omitempty"`
		NanoCpus       int64      `json:"NanoCpus,omitempty"`
		PidsLimit      int64      `json:"PidsLimit,omitempty"`
		Privileged     bool       `json:"Privileged,omitempty"`
		ReadonlyRootfs bool       `json:"ReadonlyRootfs,omitempty"`
		Init           bool       `json:"Init,omitempty"` // false leaves the daemon default
		CapAdd         []string   `json:"CapAdd,omitempty"`
		CapDrop        []string   `json:"CapDrop,omitempty"`
		SecurityOpt    []string   `json:"SecurityOpt,omitempty"`
		ExtraHosts     []string   `json:"ExtraHosts,omitempty"`
		Dns            []string   `json:"Dns,omitempty"`
		Devices        []device   `json:"Devices,omitempty"`
		LogConfig      *logConfig `json:"LogConfig,omitempty"`
	} `json:"HostConfig"`
	NetworkingConfig *networkingConfig `json:"NetworkingConfig,omitempty"`
}

// createBody is the whole configuration of the new container: the definition's subset and
// the Compose labels discovery already groups by. Nothing is copied from the old container
// except the network mode the precondition already accepted: a Compose project's containers
// run on "<project>_default", and dropping that would strand the new one off the project
// network and its service-name DNS.
func createBody(req protocol.DeploymentRequest, s protocol.DeploymentService, networkMode string) containerCreate {
	body := containerCreate{Image: s.ImageID, Env: []string{}}
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
		if p.Host > 0 { // Host 0 is exposed only, explicit frames alone
			body.HostConfig.PortBindings[key] = append(body.HostConfig.PortBindings[key], portBinding{HostIP: p.HostIP, HostPort: strconv.Itoa(p.Host)})
		}
	}
	for _, m := range s.Mounts {
		body.HostConfig.Mounts = append(body.HostConfig.Mounts, mountSpec{Type: m.Kind, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	body.HostConfig.RestartPolicy.Name = s.Restart
	if body.HostConfig.RestartPolicy.Name == "" {
		body.HostConfig.RestartPolicy.Name = "no"
	}
	if s.Explicit != nil {
		explicitBody(&body, s.Explicit)
		return body
	}
	body.Labels = map[string]string{
		"com.docker.compose.project": req.Project, "com.docker.compose.service": s.Name, "com.docker.compose.container-number": "1", "com.docker.compose.oneoff": "False",
		"kyyard.deployment": req.Deployment, "kyyard.revision": strconv.Itoa(req.Revision),
	}
	if projectNetwork := req.Project + "_default"; networkMode == projectNetwork {
		body.HostConfig.NetworkMode = projectNetwork
		body.NetworkingConfig = &networkingConfig{EndpointsConfig: map[string]endpointSettings{projectNetwork: {Aliases: []string{s.Name}}}}
	}
	return body
}

// explicitBody sets every explicit setting as given, with no label or network added.
func explicitBody(body *containerCreate, e *protocol.ExplicitService) {
	h := &body.HostConfig
	body.Labels, body.Cmd, body.Entrypoint = e.Labels, e.Command, e.Entrypoint
	body.User, body.WorkingDir, body.Hostname, body.Tty, body.OpenStdin = e.User, e.WorkingDir, e.Hostname, e.TTY, e.StdinOpen
	body.StopSignal, body.StopTimeout = e.StopSignal, e.StopTimeout
	if hc := e.Healthcheck; hc != nil {
		ns := func(s float64) int64 { return int64(math.Round(s * float64(time.Second))) }
		body.Healthcheck = &healthcheck{Test: hc.Test, Interval: ns(hc.IntervalSeconds), Timeout: ns(hc.TimeoutSeconds), StartPeriod: ns(hc.StartPeriodSeconds), Retries: hc.Retries}
	}
	h.RestartPolicy.MaximumRetryCount = e.RestartRetries
	h.Memory, h.MemorySwap, h.NanoCpus, h.PidsLimit = e.Resources.MemoryBytes, e.Resources.MemorySwapBytes, e.Resources.NanoCPUs, e.Resources.PidsLimit
	h.Privileged, h.ReadonlyRootfs, h.Init = e.Privileged, e.ReadOnlyRootfs, e.Init
	h.CapAdd, h.CapDrop, h.SecurityOpt, h.ExtraHosts, h.Dns = e.CapAdd, e.CapDrop, e.SecurityOpt, e.ExtraHosts, e.DNS
	for _, d := range e.Devices {
		h.Devices = append(h.Devices, device{d.Host, d.Container, d.Permissions})
	}
	if e.Log.Driver != "" {
		h.LogConfig = &logConfig{e.Log.Driver, e.Log.Options}
	}
	h.NetworkMode = e.NetworkMode
	if first, _ := attachments(e); first != nil {
		if h.NetworkMode == "" && !protocol.NamespaceNetwork(first.Name) {
			h.NetworkMode = first.Name
		}
		body.NetworkingConfig = &networkingConfig{EndpointsConfig: map[string]endpointSettings{first.Name: endpoint(*first)}}
	}
}

// attachments splits the networks into the one create joins (the network mode's, else the
// first) and the rest, connected after create.
func attachments(e *protocol.ExplicitService) (*protocol.NetworkAttachmentSpec, []protocol.NetworkAttachmentSpec) {
	if len(e.Networks) == 0 {
		return nil, nil
	}
	mode := e.NetworkMode
	if mode == "" || mode == "default" {
		mode = "bridge"
	}
	i := max(0, slices.IndexFunc(e.Networks, func(n protocol.NetworkAttachmentSpec) bool { return n.Name == mode }))
	return &e.Networks[i], slices.Delete(slices.Clone(e.Networks), i, i+1)
}

func endpoint(n protocol.NetworkAttachmentSpec) endpointSettings {
	out := endpointSettings{Aliases: n.Aliases}
	if n.IP != "" {
		out.IPAMConfig = &struct {
			IPv4Address string `json:"IPv4Address"`
		}{n.IP}
	}
	return out
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
	if resp.StatusCode >= 400 || out == nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.Unmarshal(answer, out)
}
