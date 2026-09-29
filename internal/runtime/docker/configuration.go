package docker

import (
	"context"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// inspectedConfiguration decodes the container fields an edit form shows. It is the one place
// Config.Env is read, on purpose (docs/container-edit spec section 2.1): the operator edits
// environment values, so they travel, to the authorized reader only. inspectedContainer and
// inspectedForDeploy still decode none of it.
type inspectedConfiguration struct {
	ID      string `json:"Id"`
	Image   string
	Name    string
	Created time.Time
	Config  *struct {
		Image, User, WorkingDir, Hostname, StopSignal string
		Env, Cmd, Entrypoint                          []string
		Labels                                        map[string]string
		ExposedPorts                                  map[string]struct{}
		Tty, OpenStdin                                bool
		StopTimeout                                   *int
		Healthcheck                                   *struct {
			Test                                    []string
			Interval, Timeout, StartPeriod, Retries int64
		}
	}
	HostConfig *struct {
		NetworkMode                string
		Privileged, ReadonlyRootfs bool
		Init                       *bool
		RestartPolicy              struct {
			Name              string
			MaximumRetryCount int
		}
		PortBindings                                  map[string][]struct{ HostIp, HostPort string }
		Memory, MemorySwap, NanoCpus                  int64
		PidsLimit                                     *int64
		CapAdd, CapDrop, SecurityOpt, ExtraHosts, Dns []string
		Devices                                       []struct{ PathOnHost, PathInContainer, CgroupPermissions string }
		LogConfig                                     struct {
			Type   string
			Config map[string]string
		}
		Tmpfs map[string]string
	}
	Mounts          mountList
	NetworkSettings *struct {
		Networks map[string]struct {
			Aliases    []string
			IPAMConfig *struct{ IPv4Address string }
		}
		Ports map[string][]struct{ HostIp, HostPort string }
	}
}

// configurationOnly lists the codes of undescribed that name settings ExplicitService cannot
// express; the rest of its checks are settings the configuration carries.
var configurationOnly = []string{"volumes_from", "volume_driver", "mount_options", "ulimits", "sysctls", "device_requests", "pid_mode", "ipc_mode", "userns_mode", "cgroup_parent", "group_add", "links", "runtime", "anonymous_volume"}

// ReadConfiguration reads one container's editable configuration through Engine v1.41, like
// InspectContainer: GETs only, the pinned image, and a second container read that must match
// the first. Errors are fixed values; daemon text never leaves.
func (c *Client) ReadConfiguration(parent context.Context, target protocol.InspectionTarget) (*protocol.ContainerConfiguration, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, callBudget)
	defer cancel()
	daemonRuntime, err := c.inspectionRuntime(ctx)
	if err != nil {
		return nil, err
	}
	var before, after inspectedConfiguration
	var full, fullAfter inspectedForDeploy
	path := "/containers/" + target.ContainerID + "/json"
	if err := c.inspectionGet(ctx, path, &before, &full); err != nil {
		return nil, err
	}
	if before.ID != target.ContainerID || before.Image != target.ImageID || before.Created.Unix() != target.CreatedUnix {
		return nil, ErrInspectionChanged
	}
	if before.Config == nil || before.HostConfig == nil || before.NetworkSettings == nil || !reported(full) {
		return nil, ErrInspectionInvalid
	}
	var im struct {
		ID          string `json:"Id"`
		RepoDigests []string
	}
	if err = c.inspectionGet(ctx, "/images/"+target.ImageID+"/json", &im); err != nil {
		return nil, err
	}
	if im.ID != target.ImageID {
		return nil, ErrInspectionChanged
	}
	if err = c.inspectionGet(ctx, path, &after, &fullAfter); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(before, after) || !reported(fullAfter) || !sameConfiguration(full, fullAfter) {
		return nil, ErrInspectionChanged
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	out := configurationFacts(before, im.RepoDigests)
	for _, code := range undescribed(full, "", daemonRuntime) {
		if slices.Contains(configurationOnly, code) {
			out.Unsupported = append(out.Unsupported, code)
		}
	}
	out.Unsupported = append(out.Unsupported, out.truncated...)
	out.Target, out.ImageID, out.ObservedAt = target, target.ImageID, time.Now().UTC()
	if out.Validate(target, time.Now()) != nil {
		return nil, ErrInspectionInvalid
	}
	return &out.ContainerConfiguration, nil
}

type configurationRead struct {
	protocol.ContainerConfiguration
	truncated []string // codes for what was cut to fit the protocol's bounds
}

func (o *configurationRead) cut(code string) {
	if !slices.Contains(o.truncated, code) {
		o.truncated = append(o.truncated, code)
	}
}

// list caps l at protocol.MaxListEntries, naming field when it cuts. It never returns nil.
func list[T any](o *configurationRead, field string, l []T) []T {
	if len(l) > protocol.MaxListEntries {
		o.cut("list_truncated:" + field)
		l = l[:protocol.MaxListEntries]
	}
	return append([]T{}, l...)
}

// argv caps l at protocol.MaxArgv entries of protocol.MaxArgvEntryBytes, cutting at the first
// entry that does not fit.
func argv(o *configurationRead, l []string) []string {
	out := []string{}
	for _, s := range l {
		if len(out) == protocol.MaxArgv || len(s) > protocol.MaxArgvEntryBytes || !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			o.cut("argv_truncated")
			break
		}
		out = append(out, s)
	}
	return out
}

func configurationFacts(in inspectedConfiguration, repoDigests []string) *configurationRead {
	cfg, h := in.Config, in.HostConfig
	o := &configurationRead{}
	o.Name = strings.TrimPrefix(in.Name, "/")
	o.Image.Reference = cfg.Image
	name, _ := protocol.SplitImageReference(cfg.Image)
	for _, d := range repoDigests {
		if repo, digest, ok := strings.Cut(d, "@"); ok && canonicalRepository(repo) == canonicalRepository(name) {
			o.Image.Digest = digest
			break
		}
	}
	o.Command, o.Entrypoint = argv(o, cfg.Cmd), argv(o, cfg.Entrypoint)
	o.User, o.WorkingDir, o.Hostname = cfg.User, cfg.WorkingDir, cfg.Hostname
	o.Env = env(o, cfg.Env)
	o.Labels = labels(o, cfg.Labels)
	o.Restart, o.RestartRetries = h.RestartPolicy.Name, h.RestartPolicy.MaximumRetryCount
	if o.Restart == "" {
		o.Restart = "no"
	}
	o.Ports = ports(o, in)
	o.Mounts = mounts(o, in)
	o.NetworkMode = h.NetworkMode
	o.Networks = networks(o, in)
	o.Resources = protocol.Resources{NanoCPUs: h.NanoCpus, MemoryBytes: h.Memory, MemorySwapBytes: h.MemorySwap}
	if h.PidsLimit != nil {
		o.Resources.PidsLimit = *h.PidsLimit
	}
	if hc := cfg.Healthcheck; hc != nil && len(hc.Test) > 0 {
		s := func(ns int64) float64 { return float64(ns) / float64(time.Second) }
		o.Healthcheck = &protocol.Healthcheck{Test: argv(o, hc.Test), IntervalSeconds: s(hc.Interval), TimeoutSeconds: s(hc.Timeout), StartPeriodSeconds: s(hc.StartPeriod), Retries: int(hc.Retries)}
	}
	o.Privileged, o.ReadOnlyRootfs, o.Init = h.Privileged, h.ReadonlyRootfs, h.Init != nil && *h.Init
	o.TTY, o.StdinOpen = cfg.Tty, cfg.OpenStdin
	o.CapAdd, o.CapDrop = list(o, "cap_add", h.CapAdd), list(o, "cap_drop", h.CapDrop)
	o.SecurityOpt, o.ExtraHosts, o.DNS = list(o, "security_opt", h.SecurityOpt), list(o, "extra_hosts", h.ExtraHosts), list(o, "dns", h.Dns)
	o.Devices = []protocol.Device{}
	for _, d := range list(o, "devices", h.Devices) {
		o.Devices = append(o.Devices, protocol.Device{Host: d.PathOnHost, Container: d.PathInContainer, Permissions: cmpOr(d.CgroupPermissions, "rwm")})
	}
	o.Log.Driver = h.LogConfig.Type
	if len(h.LogConfig.Config) > protocol.MaxLogOptions {
		o.cut("list_truncated:log_options")
		keys := slices.Sorted(maps.Keys(h.LogConfig.Config))[:protocol.MaxLogOptions]
		o.Log.Options = map[string]string{}
		for _, k := range keys {
			o.Log.Options[k] = h.LogConfig.Config[k]
		}
	} else {
		o.Log.Options = h.LogConfig.Config
	}
	o.StopSignal, o.StopTimeout = cfg.StopSignal, cfg.StopTimeout
	o.Unsupported = []string{}
	return o
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// env splits KEY=VALUE on the first "="; an entry the protocol would refuse is cut.
func env(o *configurationRead, l []string) []protocol.EnvEntry {
	out, total := []protocol.EnvEntry{}, 0
	seen := map[string]bool{}
	for _, kv := range l {
		k, v, _ := strings.Cut(kv, "=")
		total += len(k) + len(v)
		if k == "" || len(k) > 128 || !utf8.ValidString(k) || strings.ContainsRune(k, 0) || seen[k] || len(v) > protocol.MaxDeploymentEnvValueBytes || !utf8.ValidString(v) || strings.ContainsRune(v, 0) || total > protocol.MaxDeploymentEnvBytes || len(out) == protocol.MaxDeploymentEnvEntries {
			o.cut("env_truncated")
			continue
		}
		seen[k] = true
		out = append(out, protocol.EnvEntry{Name: k, Value: v})
	}
	return out
}

// labels keeps what fits the protocol's bounds, in key order.
func labels(o *configurationRead, in map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(in)) {
		if v := in[k]; len(out) == protocol.MaxLabels || k == "" || protocol.CleanText(k, protocol.MaxLabelBytes) != k || protocol.CleanText(v, protocol.MaxLabelBytes) != v {
			o.cut("labels_truncated")
		} else {
			out[k] = v
		}
	}
	return out
}

// ports lists every published binding and each exposed port with none. A binding without a
// host port (-p 127.0.0.1::80) reports the port the daemon assigned, when it has one.
func ports(o *configurationRead, in inspectedConfiguration) []protocol.Port {
	out, bound := []protocol.Port{}, map[string]bool{}
	add := func(p protocol.Port) {
		if len(out) == protocol.MaxDeploymentPorts {
			o.cut("list_truncated:ports")
			return
		}
		out = append(out, p)
	}
	for key, bindings := range in.HostConfig.PortBindings {
		port, proto, ok := parsePortKey(key)
		if !ok {
			o.cut("list_truncated:ports")
			continue
		}
		for _, b := range bindings {
			hostPort := b.HostPort
			if hostPort == "" {
				for _, a := range in.NetworkSettings.Ports[key] {
					if a.HostIp == b.HostIp {
						hostPort = a.HostPort
					}
				}
			}
			host, err := strconv.Atoi(hostPort)
			if err != nil || host < 1 {
				continue // no assigned port: exposed only
			}
			ip := b.HostIp
			if a, err := netip.ParseAddr(ip); ip != "" && err == nil {
				ip = a.String()
			}
			bound[key] = true
			add(protocol.Port{HostIP: ip, Host: host, Container: port, Protocol: proto})
		}
	}
	for key := range in.Config.ExposedPorts {
		if port, proto, ok := parsePortKey(key); !ok {
			o.cut("list_truncated:ports")
		} else if !bound[key] {
			add(protocol.Port{Container: port, Protocol: proto})
		}
	}
	slices.SortFunc(out, func(a, b protocol.Port) int {
		if a.Container != b.Container {
			return a.Container - b.Container
		}
		if a.Protocol != b.Protocol {
			return strings.Compare(a.Protocol, b.Protocol)
		}
		if a.Host != b.Host {
			return a.Host - b.Host
		}
		return strings.Compare(a.HostIP, b.HostIP)
	})
	return out
}

func parsePortKey(key string) (int, string, bool) {
	target, proto, ok := strings.Cut(key, "/")
	port, err := strconv.Atoi(target)
	return port, proto, ok && err == nil && port >= 1 && port <= 65535 && strconv.Itoa(port) == target && (proto == "tcp" || proto == "udp")
}

// mounts lists volume, bind and tmpfs mounts by target. An anonymous volume is left to the
// anonymous_volume code, and a mount of another type is cut.
func mounts(o *configurationRead, in inspectedConfiguration) []protocol.Mount {
	out, targets := []protocol.Mount{}, map[string]bool{}
	for _, m := range in.Mounts {
		switch {
		case m.Type == "volume" && anonymousVolume.MatchString(m.Name):
			continue
		case m.Type == "volume":
			out = append(out, protocol.Mount{Kind: protocol.MountVolume, Source: m.Name, Target: m.Destination, ReadOnly: !m.RW})
		case m.Type == "bind":
			out = append(out, protocol.Mount{Kind: protocol.MountBind, Source: m.Source, Target: m.Destination, ReadOnly: !m.RW})
		case m.Type == "tmpfs":
			out = append(out, protocol.Mount{Kind: protocol.MountTmpfs, Target: m.Destination})
		default:
			o.cut("list_truncated:mounts")
			continue
		}
		targets[m.Destination] = true
	}
	// The Engine may report --tmpfs only in HostConfig.Tmpfs.
	for target := range in.HostConfig.Tmpfs {
		if !targets[target] {
			out = append(out, protocol.Mount{Kind: protocol.MountTmpfs, Target: target})
		}
	}
	slices.SortFunc(out, func(a, b protocol.Mount) int { return strings.Compare(a.Target, b.Target) })
	if len(out) > protocol.MaxMounts {
		o.cut("list_truncated:mounts")
		out = out[:protocol.MaxMounts]
	}
	return out
}

// networks lists the attachments to join, none for host, none or container modes. The
// container's short ID alias is Docker's own and is dropped.
func networks(o *configurationRead, in inspectedConfiguration) []protocol.NetworkAttachmentSpec {
	out := []protocol.NetworkAttachmentSpec{}
	if m := in.HostConfig.NetworkMode; m == "host" || m == "none" || strings.HasPrefix(m, "container:") {
		return out
	}
	for _, name := range slices.Sorted(maps.Keys(in.NetworkSettings.Networks)) {
		if len(out) == protocol.MaxConfigurationNetworks {
			o.cut("list_truncated:networks")
			break
		}
		n := in.NetworkSettings.Networks[name]
		aliases := slices.DeleteFunc(slices.Clone(n.Aliases), func(a string) bool { return a == in.ID[:min(12, len(in.ID))] })
		spec := protocol.NetworkAttachmentSpec{Name: name, Aliases: list(o, "networks", aliases)}
		if n.IPAMConfig != nil {
			spec.IP = n.IPAMConfig.IPv4Address
		}
		out = append(out, spec)
	}
	return out
}
