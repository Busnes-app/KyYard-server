package protocol

import (
	"errors"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"
)

// InspectionTarget pins the identity already authorized by the caller. Docker
// inventory exposes creation time in whole seconds. See application-schema.md,
// Runtime inspection foundation. This is not a wire grant or deployment approval.
type InspectionTarget struct {
	ContainerID string `json:"container_id"`
	ImageID     string `json:"image_id"`
	CreatedUnix int64  `json:"created_unix"`
}

func (t InspectionTarget) Validate() error {
	if !fullDockerID.MatchString(t.ContainerID) || !strings.HasPrefix(t.ImageID, "sha256:") || !fullDockerID.MatchString(strings.TrimPrefix(t.ImageID, "sha256:")) || t.CreatedUnix <= 0 {
		return errors.New("inspection requires full container/image IDs and creation time")
	}
	return nil
}

const MaxInspectionEntries = 64

// ContainerInspection is an allowlisted observation, not a recreation spec.
// Counts omit mount paths/network names. Environment, labels, argv, healthcheck
// commands, raw configuration and hashes of those values never enter this type.
// Unsupported names, as codes from UnsupportedCodes, configuration a recreate from the
// definition would drop; ConfigurationVerified is true exactly when it is empty.
type ContainerInspection struct {
	Target     InspectionTarget `json:"target"`
	ObservedAt time.Time        `json:"observed_at"`
	State      string           `json:"state"`
	// Health is Docker's State.Health.Status, "none" without a healthcheck; RestartCount is the
	// runtime's restart count. Both are set exactly when the agent advertises
	// CapabilityContainerInspectHealth. The healthcheck's command and log never enter this type.
	Health                string        `json:"health"`
	RestartCount          int           `json:"restart_count"`
	ImagePlatform         ImagePlatform `json:"image_platform"`
	RestartPolicy         string        `json:"restart_policy"`
	RestartRetries        int           `json:"restart_retries"`
	Ports                 []Port        `json:"ports"`
	Mounts                MountCounts   `json:"mounts"`
	NetworkMode           string        `json:"network_mode"`
	NetworkCount          int           `json:"network_count"`
	Privileged            bool          `json:"privileged"`
	ReadOnlyRootFS        bool          `json:"read_only_rootfs"`
	AutoRemove            bool          `json:"auto_remove"`
	Unsupported           []string      `json:"unsupported"`
	ConfigurationVerified bool          `json:"configuration_verified"`
}
type ImagePlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}
type MountCounts struct {
	Bind     int `json:"bind"`
	Volume   int `json:"volume"`
	Tmpfs    int `json:"tmpfs"`
	Other    int `json:"other"`
	ReadOnly int `json:"read_only"`
}

// UnsupportedCodes is the closed vocabulary of ContainerInspection.Unsupported, in the order the
// Docker adapter reports them, then the k8s_ codes a plan for a Kubernetes endpoint refuses. Each
// is configuration the target runtime cannot take from the definition.
var UnsupportedCodes = []string{"mount_type", "anonymous_volume", "volumes_from", "volume_driver", "mount_options", "tmpfs", "auto_remove", "read_only_rootfs", "privileged", "capabilities", "security_opt", "devices", "pid_mode", "ipc_mode", "user", "runtime", "resource_limits", "ulimits", "sysctls", "device_requests", "init", "userns_mode", "cgroup_parent", "group_add", "extra_hosts", "dns", "links", "network", "image_config", "k8s_volume", "k8s_host_ip", "k8s_restart", "k8s_name", "k8s_namespace"}

// knownCodes accepts at most MaxUnsupported distinct codes from UnsupportedCodes.
func knownCodes(codes []string) bool {
	if len(codes) > MaxUnsupported {
		return false
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] || !slices.Contains(UnsupportedCodes, c) {
			return false
		}
		seen[c] = true
	}
	return true
}

const (
	TypeInspectionOpen         = "inspection.open"
	TypeInspectionResult       = "inspection.result"
	TypeInspectionCancel       = "inspection.cancel"
	InspectionLifetime         = 25 * time.Second
	MaxInspectionFrameBytes    = 32 << 10
	MaxInspectionsPerEndpoint  = 2
	CapabilityContainerInspect = "container.inspect"
	// CapabilityContainerInspectVerdict marks an agent whose inspections carry the unsupported
	// codes and configuration_verified; plans need it, the inspection dialog does not.
	CapabilityContainerInspectVerdict = "container.inspect.verdict"
	// CapabilityContainerInspectHealth marks an agent whose inspections carry health and
	// restart_count; health validation needs it.
	CapabilityContainerInspectHealth = "container.inspect.health"
	// MaxRestartCount bounds a reported restart count.
	MaxRestartCount = 1_000_000
	MaxUnsupported  = 40
)

type InspectionOpen struct {
	Request    string           `json:"request"`
	Endpoint   string           `json:"endpoint"`
	Actor      string           `json:"actor"`
	Connection []byte           `json:"connection"`
	Expires    time.Time        `json:"expires"`
	Target     InspectionTarget `json:"target"`
}

func (r InspectionOpen) Validate(now time.Time) error {
	if !execStreamID.MatchString(r.Request) || !execStreamID.MatchString(r.Actor) || !execStreamID.MatchString(r.Endpoint) || len(r.Connection) != 32 || !r.Expires.After(now) || r.Expires.After(now.Add(InspectionLifetime)) {
		return errors.New("invalid inspection grant")
	}
	return r.Target.Validate()
}

type InspectionCancel struct {
	Request string `json:"request"`
}
type InspectionResult struct {
	Request string               `json:"request"`
	Status  string               `json:"status"` // ok, unavailable or busy; never runtime error text.
	Result  *ContainerInspection `json:"result,omitempty"`
}

var inspectionPlatform = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// Validate bounds an untrusted agent result before it reaches an HTTP response. health says the
// answering agent advertised CapabilityContainerInspectHealth: health and restart_count are then
// required and bounded, and otherwise absent.
func (r ContainerInspection) Validate(target InspectionTarget, now time.Time, health bool) error {
	invalid := errors.New("invalid inspection result")
	if r.Target != target || target.Validate() != nil || r.ConfigurationVerified != (len(r.Unsupported) == 0) || !knownCodes(r.Unsupported) || r.ObservedAt.Before(now.Add(-InspectionLifetime)) || r.ObservedAt.After(now.Add(5*time.Second)) {
		return invalid
	}
	switch r.State {
	case "created", "running", "paused", "restarting", "removing", "exited", "dead":
	default:
		return invalid
	}
	switch r.RestartPolicy {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return invalid
	}
	switch r.NetworkMode {
	case "default", "bridge", "host", "none", "container", "custom":
	default:
		return invalid
	}
	if health {
		switch r.Health {
		case "none", "starting", "healthy", "unhealthy":
		default:
			return invalid
		}
		if r.RestartCount < 0 || r.RestartCount > MaxRestartCount {
			return invalid
		}
	} else if r.Health != "" || r.RestartCount != 0 {
		return invalid
	}
	if r.RestartRetries < 0 || r.RestartRetries > 2147483647 || r.NetworkCount < 0 || r.NetworkCount > MaxInspectionEntries || len(r.Ports) > MaxInspectionEntries {
		return invalid
	}
	total := 0
	for _, n := range []int{r.Mounts.Bind, r.Mounts.Volume, r.Mounts.Tmpfs, r.Mounts.Other} {
		if n < 0 || n > MaxInspectionEntries {
			return invalid
		}
		total += n
	}
	if total > MaxInspectionEntries || r.Mounts.ReadOnly < 0 || r.Mounts.ReadOnly > total {
		return invalid
	}
	p := r.ImagePlatform
	if !inspectionPlatform.MatchString(p.OS) || !inspectionPlatform.MatchString(p.Architecture) || (p.Variant != "" && !inspectionPlatform.MatchString(p.Variant)) {
		return invalid
	}
	for _, p := range r.Ports {
		if p.Container < 1 || p.Container > 65535 || p.Host < 0 || p.Host > 65535 || (p.Protocol != "tcp" && p.Protocol != "udp" && p.Protocol != "sctp") {
			return invalid
		}
		if p.Host == 0 {
			if p.HostIP != "" {
				return invalid
			}
		} else {
			a, err := netip.ParseAddr(p.HostIP)
			if err != nil || a.Zone() != "" {
				return invalid
			}
		}
	}
	return nil
}
