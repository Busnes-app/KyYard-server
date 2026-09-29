package docker

import (
	"context"
	"encoding/json"
	"maps"
	"net/netip"
	"reflect"
	"regexp"
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

// rawInspection is the same container read as maps, so a setting no typed field names is seen.
type rawInspection struct {
	Config, HostConfig map[string]json.RawMessage
	NetworkSettings    *struct {
		Networks map[string]map[string]json.RawMessage
	}
}

// configKeys, hostConfigKeys and endpointKeys list every key the Engine reports that the read
// knows: "" when a configuration field carries it, another check (undescribed, mountOptions,
// the tmpfs options) reads it, or it is the client's or the daemon's own; otherwise the code it is
// flagged under when set. A key not listed is flagged host_config:<Key> when set, so a setting
// this adapter has never heard of fails closed rather than being dropped by a recreate.
var (
	configKeys = keyCodes(map[string][]string{
		"": {"Image", "User", "WorkingDir", "Hostname", "StopSignal", "Env", "Cmd", "Entrypoint", "Labels", "ExposedPorts", "Tty", "OpenStdin", "StopTimeout", "Healthcheck",
			// attach settings of the client that ran it, and what the image declares (its volumes are anonymous_volume's)
			"AttachStdin", "AttachStdout", "AttachStderr", "StdinOnce", "Volumes", "OnBuild", "ArgsEscaped", "Shell"},
	})
	hostConfigKeys = keyCodes(map[string][]string{
		"": {"NetworkMode", "Privileged", "ReadonlyRootfs", "Init", "RestartPolicy", "PortBindings", "Memory", "MemorySwap", "NanoCpus", "PidsLimit",
			"CapAdd", "CapDrop", "SecurityOpt", "ExtraHosts", "Dns", "Devices", "LogConfig", "Tmpfs", "Binds", "Mounts",
			"VolumesFrom", "VolumeDriver", "Ulimits", "Sysctls", "DeviceRequests", "PidMode", "IpcMode", "UsernsMode", "CgroupParent", "GroupAdd", "Links", "Runtime",
			// the client's console and ID file; the default masked paths, which privileged and security_opt decide
			"ConsoleSize", "ContainerIDFile", "MaskedPaths", "ReadonlyPaths"},
		"resource_limits": {"CpuShares", "CpuPeriod", "CpuQuota", "CpuRealtimePeriod", "CpuRealtimeRuntime", "CpusetCpus", "CpusetMems", "CpuCount", "CpuPercent",
			"MemoryReservation", "MemorySwappiness", "KernelMemory", "KernelMemoryTCP", "BlkioWeight", "BlkioWeightDevice", "BlkioDeviceReadBps",
			"BlkioDeviceWriteBps", "BlkioDeviceReadIOps", "BlkioDeviceWriteIOps", "IOMaximumBandwidth", "IOMaximumIOps"},
		"dns":         {"DnsOptions", "DnsSearch"},
		"auto_remove": {"AutoRemove"},
	})
	endpointKeys = keyCodes(map[string][]string{
		"":      {"Aliases", "IPAMConfig", "NetworkID", "EndpointID", "Gateway", "IPAddress", "IPPrefixLen", "IPv6Gateway", "GlobalIPv6Address", "GlobalIPv6PrefixLen", "MacAddress", "DNSNames"},
		"links": {"Links"},
	})
	// daemonDefaults are values the daemon gives a container that sets nothing.
	daemonDefaults = map[string]string{"ShmSize": "67108864", "CgroupnsMode": `"private"`, "MemorySwappiness": "-1"}
	engineKey      = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
)

func keyCodes(byCode map[string][]string) map[string]string {
	out := map[string]string{}
	for code, keys := range byCode {
		for _, k := range keys {
			out[k] = code
		}
	}
	return out
}

// unset reports a value that sets nothing: null, false, 0, "", an empty list or map, or the
// daemon's own default for key.
func unset(key string, raw json.RawMessage) bool {
	if d, ok := daemonDefaults[key]; ok && string(raw) == d {
		return true
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case float64:
		return x == 0
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// uncarried adds to codes each set key of section the configuration does not carry. It reports
// false for a key outside the Engine's grammar.
func uncarried(section map[string]json.RawMessage, known map[string]string, codes map[string]bool) bool {
	for k, raw := range section {
		code, listed := known[k]
		switch {
		case !engineKey.MatchString(k):
			return false
		case k == "IPAMConfig": // only its IPv4Address is carried
			var ipam map[string]json.RawMessage
			_ = json.Unmarshal(raw, &ipam)
			for sub, v := range ipam {
				if sub != "IPv4Address" && !unset(sub, v) {
					codes["host_config:IPAMConfig"] = true
				}
			}
		case unset(k, raw) || (listed && code == ""):
		case listed:
			codes[code] = true
		default:
			codes["host_config:"+k] = true
		}
	}
	return true
}

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
	var raw, rawAfter rawInspection
	path := "/containers/" + target.ContainerID + "/json"
	if err := c.inspectionGet(ctx, path, &before, &full, &raw); err != nil {
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
	if err = c.inspectionGet(ctx, path, &after, &fullAfter, &rawAfter); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(before, after) || !reported(fullAfter) || !sameConfiguration(full, fullAfter) || !reflect.DeepEqual(raw, rawAfter) {
		return nil, ErrInspectionChanged
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	out := configurationFacts(before, im.RepoDigests)
	codes := map[string]bool{}
	for _, code := range undescribed(full, "", daemonRuntime) {
		codes[code] = codes[code] || slices.Contains(protocol.ConfigurationOnlyCodes, code)
	}
	ok := raw.NetworkSettings != nil && uncarried(raw.Config, configKeys, codes) && uncarried(raw.HostConfig, hostConfigKeys, codes)
	for _, n := range raw.NetworkSettings.Networks {
		ok = ok && uncarried(n, endpointKeys, codes)
	}
	if !ok {
		return nil, ErrInspectionInvalid
	}
	// Known codes in their vocabulary's order, then Engine keys in theirs.
	for _, code := range protocol.UnsupportedCodes {
		if codes[code] {
			out.Unsupported = append(out.Unsupported, code)
		}
	}
	var keys []string
	for code, set := range codes {
		if set && strings.HasPrefix(code, "host_config:") {
			keys = append(keys, code)
		}
	}
	slices.Sort(keys)
	out.Unsupported = append(out.Unsupported, keys[:min(len(keys), protocol.MaxListEntries)]...)
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
	if o.Hostname == in.ID[:min(12, len(in.ID))] {
		o.Hostname = "" // Docker's default; a recreate gets its own
	}
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

// env splits KEY=VALUE on the first "="; a later duplicate wins, as in Docker, and an entry
// the protocol would refuse is cut.
func env(o *configurationRead, l []string) []protocol.EnvEntry {
	var names []string
	values := map[string]string{}
	for _, kv := range l {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := values[k]; !dup {
			names = append(names, k)
		}
		values[k] = v
	}
	out, total := []protocol.EnvEntry{}, 0
	for _, k := range names {
		v := values[k]
		if !protocol.ValidConfigurationEnvName(k) || len(v) > protocol.MaxDeploymentEnvValueBytes || !utf8.ValidString(v) || strings.ContainsRune(v, 0) || total+len(k)+len(v) > protocol.MaxDeploymentEnvBytes || len(out) == protocol.MaxDeploymentEnvEntries {
			o.cut("env_truncated")
			continue
		}
		total += len(k) + len(v)
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
				o.cut("list_truncated:ports") // a dynamic binding is pinned or dropped
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
			out = append(out, protocol.Mount{Kind: protocol.MountTmpfs, Target: m.Destination, ReadOnly: !m.RW})
		default:
			o.cut("list_truncated:mounts")
			continue
		}
		targets[m.Destination] = true
	}
	// The Engine may report --tmpfs only in HostConfig.Tmpfs; of its options only ro is carried
	// (mountOptions names the rest).
	for target, options := range in.HostConfig.Tmpfs {
		if !targets[target] {
			out = append(out, protocol.Mount{Kind: protocol.MountTmpfs, Target: target, ReadOnly: slices.Contains(strings.Split(options, ","), "ro")})
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
		spec := protocol.NetworkAttachmentSpec{Name: name, Aliases: list(o, "aliases", aliases)}
		if n.IPAMConfig != nil {
			spec.IP = n.IPAMConfig.IPv4Address
		}
		out = append(out, spec)
	}
	return out
}
