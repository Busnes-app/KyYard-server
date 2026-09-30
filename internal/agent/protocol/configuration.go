package protocol

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	TypeConfigurationOpen        = "configuration.open"
	TypeConfigurationResult      = "configuration.result"
	TypeConfigurationCancel      = "configuration.cancel"
	CapabilityContainerConfigure = "container.configure"
	// ConfigurationLifetime is the longest grant a configuration.open may carry. The alias below
	// shares InspectionOpen.ValidateFor, which enforces InspectionLifetime; the server issues
	// Expires = min(context deadline, now + ConfigurationLifetime) and the agent enforces this
	// bound with ValidateWithin.
	ConfigurationLifetime      = 20 * time.Second
	MaxConfigurationFrameBytes = 320 << 10

	MaxArgv, MaxArgvEntryBytes        = 64, 4096
	MaxListEntries, MaxListEntryBytes = 32, 1024
	MaxConfigurationNetworks          = 16 // the brief's MaxNetworks; that name is the inventory's 200
	MaxLogOptions                     = 16

	// MountTmpfs is a configuration-only mount kind; inventory reports tmpfs as "other".
	MountTmpfs = "tmpfs"
)

// ConfigurationOpen and ConfigurationCancel carry the inspection grant shape: request,
// endpoint, actor, connection nonce, expiry and immutable target.
type (
	ConfigurationOpen   = InspectionOpen
	ConfigurationCancel = InspectionCancel
)

// ConfigurationResult answers a ConfigurationOpen. Status is ok, unavailable or busy; only ok
// carries an answer, a Result for a container or a Workload for a cluster object, and never
// runtime error text.
type ConfigurationResult struct {
	Request  string                  `json:"request"`
	Status   string                  `json:"status"`
	Result   *ContainerConfiguration `json:"result,omitempty"`
	Workload *WorkloadConfiguration  `json:"workload,omitempty"`
}

type EnvEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// NetworkAttachmentSpec is a network to join; IP is the static address, "" when dynamic.
type NetworkAttachmentSpec struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
	IP      string   `json:"ip,omitempty"`
}

type Resources struct {
	NanoCPUs        int64 `json:"nano_cpus"`
	MemoryBytes     int64 `json:"memory_bytes"`
	MemorySwapBytes int64 `json:"memory_swap_bytes"`
	PidsLimit       int64 `json:"pids_limit"`
}

type Healthcheck struct {
	Test               []string `json:"test"`
	IntervalSeconds    float64  `json:"interval_seconds"`
	TimeoutSeconds     float64  `json:"timeout_seconds"`
	StartPeriodSeconds float64  `json:"start_period_seconds"`
	Retries            int      `json:"retries"`
}

type Device struct {
	Host        string `json:"host"`
	Container   string `json:"container"`
	Permissions string `json:"permissions"`
}

type LogConfig struct {
	Driver  string            `json:"driver"`
	Options map[string]string `json:"options,omitempty"`
}

// ContainerConfiguration is the editable configuration of one container, read for the edit
// form. Unlike ContainerInspection it carries values (env, labels, argv). Unsupported names
// configuration the agent cannot express in a service spec.
type ContainerConfiguration struct {
	Target     InspectionTarget `json:"target"`
	ObservedAt time.Time        `json:"observed_at"`
	Name       string           `json:"name"`
	Image      ImagePull        `json:"image"`
	ImageID    string           `json:"image_id"`
	Command    []string         `json:"command"`
	Entrypoint []string         `json:"entrypoint"`
	User       string           `json:"user"`
	WorkingDir string           `json:"working_dir"`
	Hostname   string           `json:"hostname"`
	Env        []EnvEntry       `json:"env"`
	// Labels are cleaned (CleanText) by the agent before sending; Validate refuses any that are not.
	Labels         map[string]string       `json:"labels"`
	Restart        string                  `json:"restart"`
	RestartRetries int                     `json:"restart_retries"`
	Ports          []Port                  `json:"ports"` // Host 0 is exposed but unpublished
	Mounts         []Mount                 `json:"mounts"`
	NetworkMode    string                  `json:"network_mode"`
	Networks       []NetworkAttachmentSpec `json:"networks"`
	Resources      Resources               `json:"resources"`
	Healthcheck    *Healthcheck            `json:"healthcheck,omitempty"`
	Privileged     bool                    `json:"privileged"`
	ReadOnlyRootfs bool                    `json:"read_only_rootfs"`
	Init           bool                    `json:"init"`
	TTY            bool                    `json:"tty"`
	StdinOpen      bool                    `json:"stdin_open"`
	CapAdd         []string                `json:"cap_add"`
	CapDrop        []string                `json:"cap_drop"`
	SecurityOpt    []string                `json:"security_opt"`
	ExtraHosts     []string                `json:"extra_hosts"`
	DNS            []string                `json:"dns"`
	Devices        []Device                `json:"devices"`
	Log            LogConfig               `json:"log"`
	StopSignal     string                  `json:"stop_signal"`
	StopTimeout    *int                    `json:"stop_timeout,omitempty"`
	Unsupported    []string                `json:"unsupported"`
}

// configurationCodes are the truncation codes a configuration adds to UnsupportedCodes. They
// stay out of that list, which the web vocabulary fixture pins.
var configurationCodes = []string{"env_truncated", "labels_truncated", "argv_truncated"}

// ConfigurationOnlyCodes are the UnsupportedCodes naming settings an ExplicitService cannot
// express: a configuration read lists them, and an explicit recreate of a container that has
// any is refused.
var ConfigurationOnlyCodes = []string{"volumes_from", "volume_driver", "mount_options", "ulimits", "sysctls", "device_requests", "pid_mode", "ipc_mode", "userns_mode", "cgroup_parent", "group_add", "links", "runtime", "anonymous_volume"}

var (
	listTruncated = regexp.MustCompile(`^list_truncated:[a-z_]+$`)
	// hostConfig names an Engine setting the read does not know, by its key.
	hostConfig     = regexp.MustCompile(`^host_config:[A-Za-z0-9]{1,64}$`)
	devicePerms    = regexp.MustCompile(`^[rwm]{1,3}$`)
	configRestarts = []string{"no", "always", "unless-stopped", "on-failure"}
)

// FieldError is a refused configuration; Field names what was refused.
type FieldError struct{ Field, Message string }

func (e *FieldError) Error() string { return e.Message + ": " + e.Field }

func configErr(field string) error {
	return &FieldError{Field: field, Message: "invalid container configuration"}
}

// text is a displayable string: valid UTF-8 that CleanText leaves alone, at most max bytes.
func text(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && CleanText(s, max) == s
}

// raw is a value that may hold newlines (argv, env): valid UTF-8, no NUL, at most max bytes.
func raw(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func textList(l []string, max int) bool {
	if len(l) > MaxListEntries {
		return false
	}
	for _, s := range l {
		if !text(s, max) {
			return false
		}
	}
	return true
}

func rawArgv(l []string) bool {
	if len(l) > MaxArgv {
		return false
	}
	for _, s := range l {
		if !raw(s, MaxArgvEntryBytes) {
			return false
		}
	}
	return true
}

func nonNegative(v ...float64) bool {
	for _, f := range v {
		if f < 0 || f > 1e12 {
			return false
		}
	}
	return true
}

func validIPOrEmpty(s string) bool {
	if s == "" {
		return true
	}
	ip, err := netip.ParseAddr(s)
	return err == nil && ip.Zone() == ""
}

// Validate bounds an untrusted agent's configuration before it reaches an HTTP response.
func (c *ContainerConfiguration) Validate(target InspectionTarget, now time.Time) error {
	if err := c.Target.Validate(); err != nil || c.Target != target {
		return configErr("target")
	}
	if d := now.Sub(c.ObservedAt); d > MaxClockSkew || d < -MaxClockSkew {
		return configErr("observed_at")
	}
	switch {
	case !text(c.Name, 255) || c.Name == "":
		return configErr("name")
	case !ValidImageReference(c.Image.Reference) || (c.Image.Digest != "" && !imageID.MatchString(c.Image.Digest)) || !text(c.Image.Tag, 512):
		return configErr("image")
	case !fullImageID(c.ImageID):
		return configErr("image_id")
	case !rawArgv(c.Command) || !rawArgv(c.Entrypoint):
		return configErr("argv")
	case !text(c.User, 256) || !text(c.WorkingDir, MaxMountPathBytes) || !text(c.Hostname, 256):
		return configErr("user, working_dir or hostname")
	case !slices.Contains(configRestarts, c.Restart) || c.RestartRetries < 0 || c.RestartRetries > MaxRestartCount || (c.RestartRetries > 0 && c.Restart != "on-failure"):
		return configErr("restart")
	case !text(c.NetworkMode, 256):
		return configErr("network_mode")
	case !text(c.StopSignal, 32) || (c.StopTimeout != nil && (*c.StopTimeout < 0 || *c.StopTimeout > 3600)):
		return configErr("stop")
	case !textList(c.CapAdd, MaxListEntryBytes) || !textList(c.CapDrop, MaxListEntryBytes) || !textList(c.SecurityOpt, MaxListEntryBytes) || !textList(c.ExtraHosts, MaxListEntryBytes) || !textList(c.DNS, MaxListEntryBytes):
		return configErr("list")
	case c.Resources.NanoCPUs < 0 || c.Resources.MemoryBytes < 0 || c.Resources.MemorySwapBytes < -1 || c.Resources.PidsLimit < -1:
		return configErr("resources")
	}
	if err := c.validEnv(); err != nil {
		return err
	}
	if len(c.Labels) > MaxLabels {
		return configErr("labels")
	}
	for k, v := range c.Labels {
		if k == "" || !text(k, MaxLabelBytes) || !text(v, MaxLabelBytes) {
			return configErr("labels")
		}
	}
	if err := c.validPorts(); err != nil {
		return err
	}
	if err := c.validMounts(); err != nil {
		return err
	}
	if len(c.Networks) > MaxConfigurationNetworks {
		return configErr("networks")
	}
	seen := map[string]bool{}
	for _, n := range c.Networks {
		if n.Name == "" || !text(n.Name, 256) || NamespaceNetwork(n.Name) || seen[n.Name] || !textList(n.Aliases, 256) || !validIPOrEmpty(n.IP) {
			return configErr("networks")
		}
		seen[n.Name] = true
	}
	if h := c.Healthcheck; h != nil {
		if !rawArgv(h.Test) || !nonNegative(h.IntervalSeconds, h.TimeoutSeconds, h.StartPeriodSeconds) || h.Retries < 0 || h.Retries > 1000 {
			return configErr("healthcheck")
		}
	}
	if len(c.Devices) > MaxListEntries {
		return configErr("devices")
	}
	for _, d := range c.Devices {
		if !text(d.Host, MaxListEntryBytes) || d.Host == "" || !text(d.Container, MaxListEntryBytes) || d.Container == "" || !devicePerms.MatchString(d.Permissions) {
			return configErr("devices")
		}
	}
	if !text(c.Log.Driver, 64) || len(c.Log.Options) > MaxLogOptions {
		return configErr("log")
	}
	for k, v := range c.Log.Options {
		if k == "" || !text(k, 256) || !text(v, 256) {
			return configErr("log")
		}
	}
	if err := c.validUnsupported(); err != nil {
		return err
	}
	if b, err := json.Marshal(c); err != nil || len(b) >= MaxConfigurationFrameBytes {
		return configErr("size")
	}
	return nil
}

// configurationEnvName follows Docker, not the deployment grammar: an existing container may
// carry any name Docker accepted.
func configurationEnvName(n string) bool {
	return n != "" && len(n) <= 128 && utf8.ValidString(n) && !strings.ContainsAny(n, "=\x00")
}

// ValidConfigurationEnvName is the name rule Validate applies to a container's environment.
func ValidConfigurationEnvName(name string) bool { return configurationEnvName(name) }

func (c *ContainerConfiguration) validEnv() error {
	if len(c.Env) > MaxDeploymentEnvEntries {
		return configErr("env")
	}
	total := 0
	seen := map[string]bool{}
	for _, e := range c.Env {
		total += len(e.Name) + len(e.Value)
		if !configurationEnvName(e.Name) || seen[e.Name] || !raw(e.Value, MaxDeploymentEnvValueBytes) || total > MaxDeploymentEnvBytes {
			return configErr("env")
		}
		seen[e.Name] = true
	}
	return nil
}

// validPorts allows Host 0 (exposed, unpublished); published bindings are unique.
func (c *ContainerConfiguration) validPorts() error {
	if len(c.Ports) > MaxDeploymentPorts {
		return configErr("ports")
	}
	published := make([]Port, 0, len(c.Ports))
	exposed := map[string]bool{}
	for _, p := range c.Ports {
		if p.Container < 1 || p.Container > 65535 || p.Host < 0 || p.Host > 65535 || (p.Protocol != "tcp" && p.Protocol != "udp") {
			return configErr("ports")
		}
		if p.Host > 0 {
			published = append(published, p)
			continue
		}
		key := fmt.Sprintf("%d/%s", p.Container, p.Protocol)
		if p.HostIP != "" || exposed[key] {
			return configErr("ports")
		}
		exposed[key] = true
	}
	if validPorts(published, map[binding]bool{}, true, false) != nil {
		return configErr("ports")
	}
	return nil
}

// validMounts accepts volume and bind mounts as a deployment does, plus tmpfs (no source).
func (c *ContainerConfiguration) validMounts() error {
	if len(c.Mounts) > MaxMounts {
		return configErr("mounts")
	}
	var plain []Mount
	targets := map[string]bool{}
	for _, m := range c.Mounts {
		if m.Kind == MountTmpfs {
			if m.Source != "" || !cleanAbsolute(m.Target) || targets[m.Target] {
				return configErr("mounts")
			}
			targets[m.Target] = true
			continue
		}
		plain = append(plain, m)
	}
	if !validMounts(plain, map[string]bool{}, false) {
		return configErr("mounts")
	}
	for _, m := range plain {
		if targets[m.Target] {
			return configErr("mounts")
		}
	}
	return nil
}

func (c *ContainerConfiguration) validUnsupported() error {
	if len(c.Unsupported) > MaxUnsupported+len(configurationCodes)+2*MaxListEntries {
		return configErr("unsupported")
	}
	seen := map[string]bool{}
	for _, code := range c.Unsupported {
		if seen[code] || !(slices.Contains(UnsupportedCodes, code) || slices.Contains(configurationCodes, code) || listTruncated.MatchString(code) || hostConfig.MatchString(code)) {
			return configErr("unsupported")
		}
		seen[code] = true
	}
	return nil
}

// Validate checks the frame's shape; a present Result or Workload is validated against the
// grant's target by the caller with its own Validate.
func (r ConfigurationResult) Validate() error {
	if !execStreamID.MatchString(r.Request) {
		return configErr("request")
	}
	switch r.Status {
	case "ok":
		if (r.Result == nil) == (r.Workload == nil) {
			return configErr("ok without exactly one result")
		}
	case "unavailable", "busy":
		if r.Result != nil || r.Workload != nil {
			return configErr("result on a refusal")
		}
	default:
		return configErr("status")
	}
	return nil
}
