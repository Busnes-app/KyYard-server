package protocol

import (
	"errors"
	"net/netip"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Deployment frames carry one plan to the agent and one result back. The request holds
// resolved environment values: it exists only in memory on both sides and is never logged.
// See docs/agent-protocol.md, Deployment apply.
const (
	TypeDeploymentApply             = "deployment.apply"
	TypeDeploymentResult            = "deployment.result"
	CapabilityDeploymentApply       = "deployment.apply"
	CapabilityDeploymentPull        = "deployment.pull" // the agent runs a service's Pull step
	MaxDeploymentRequestBytes       = 320 << 10
	MaxDeploymentRequestBytesLegacy = 192 << 10 // an agent without CapabilityDeploymentPull
	MaxDeploymentResultBytes        = 160 << 10
	DeploymentLifetime              = 15 * time.Minute
	MaxDeploymentServices           = 100
	MaxDeploymentEnvEntries         = 128
	MaxDeploymentEnvValueBytes      = 16 << 10
	MaxDeploymentEnvBytes           = 64 << 10
	MaxDeploymentPorts              = 64
	MaxDeploymentStepDetailBytes    = 256
	MaxRegistryAuthHosts            = 16
	MaxRegistryAuthSecretBytes      = 4096
	MaxDeploymentVolumes            = 64
	MaxMountPathBytes               = 4096
	StepPrecondition                = "precondition"
	StepImage                       = "image"
	StepPull                        = "pull"
	StepVolume                      = "volume"
	StepStop                        = "stop"
	StepRename                      = "rename"
	StepCreate                      = "create"
	StepStart                       = "start"
	StepRemove                      = "remove"
	OutcomeSkipped                  = "skipped"
	TypeDeploymentRemove            = "deployment.remove"
	CapabilityDeploymentRemove      = "deployment.remove"
	MaxRemovalTargets               = 100 // three steps each fit a result's 8*MaxDeploymentServices
)

var (
	deploymentUUID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	deploymentProject = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	deploymentService = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	deploymentEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// Docker's volume name characters, long enough for a resolved "<project>_<name>" (64+1+64).
	deploymentVolume  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,128}$`)
	deploymentSteps   = map[string]bool{StepVolume: true, StepPrecondition: true, StepImage: true, StepPull: true, StepStop: true, StepRename: true, StepCreate: true, StepStart: true, StepRemove: true}
	deploymentRestart = map[string]bool{"": true, "no": true, "always": true, "unless-stopped": true, "on-failure": true}
	resultOutcomes    = map[string]bool{OutcomeSucceeded: true, OutcomeFailed: true, OutcomeDenied: true, OutcomeTimedOut: true, OutcomeUnknown: true}
)

type DeploymentRequest struct {
	Deployment string              `json:"deployment"`
	Endpoint   string              `json:"endpoint"`
	Project    string              `json:"project"`
	Revision   int                 `json:"revision"`
	Deadline   time.Time           `json:"deadline"`
	Services   []DeploymentService `json:"services"`
	// Registries holds a credential per registry host some service pulls from; never logged.
	Registries map[string]RegistryAuth `json:"registries,omitempty"`
	// Volumes are the named volumes the agent ensures before any replacement, each one some
	// service mounts.
	Volumes []string `json:"volumes,omitempty"`
}
type DeploymentService struct {
	Name          string            `json:"name"`
	ContainerName string            `json:"container_name"`
	ImageID       string            `json:"image_id"`
	Replaces      InspectionTarget  `json:"replaces"`
	Restart       string            `json:"restart"`
	Ports         []Port            `json:"ports"`
	Env           map[string]string `json:"env"`
	// Pull, when set, has the agent pull the image first; ImageID is then empty.
	Pull *ImagePull `json:"pull,omitempty"`
	// Mounts are MountVolume or MountBind only; a bind must already be on the replaced container.
	Mounts []Mount `json:"mounts,omitempty"`
}

// ImagePull names an image by host, repository and the digest it must resolve to. Tag, when
// set, is the service's tag reference (same host and repository) the pulled image is tagged
// with, so the host's tag follows the update; empty for a digest-pinned spec reference.
type ImagePull struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
	Tag       string `json:"tag,omitempty"`
}
type RegistryAuth struct {
	Username string `json:"username"`
	Secret   string `json:"secret"`
}

// Host is the registry host Reference names, or "" when it names none or is invalid. It is
// read as written, not canonicalised; the server sends the canonical host.
func (p ImagePull) Host() string {
	if !ValidImageReference(p.Reference) {
		return ""
	}
	name, _ := SplitImageReference(p.Reference)
	if first, _, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return ""
}

func (p ImagePull) valid() bool {
	name, digest := SplitImageReference(p.Reference)
	if p.Host() == "" || !imageID.MatchString(digest) || digest != p.Digest {
		return false
	}
	if p.Tag == "" {
		return true
	}
	tagName, tag := SplitImageReference(p.Tag)
	return ValidImageReference(p.Tag) && !strings.Contains(p.Tag, "@") && tag != "" && tagName == name
}

// cleanAbsolute is an absolute path in canonical form, not the root, that displays as it is:
// CleanText leaves it unchanged and it has no format characters (bidi, zero-width).
func cleanAbsolute(p string) bool {
	return len(p) > 1 && len(p) <= MaxMountPathBytes && path.IsAbs(p) && path.Clean(p) == p && CleanText(p, MaxMountPathBytes) == p && !strings.ContainsFunc(p, func(r rune) bool { return unicode.Is(unicode.Cf, r) })
}

func validMounts(mounts []Mount, used map[string]bool) bool {
	if len(mounts) > MaxMounts {
		return false
	}
	targets := map[string]bool{}
	for _, m := range mounts {
		switch {
		case !cleanAbsolute(m.Target) || targets[m.Target]:
			return false
		case m.Kind == MountVolume && deploymentVolume.MatchString(m.Source):
			used[m.Source] = true
		case m.Kind == MountBind && cleanAbsolute(m.Source):
		default:
			return false
		}
		targets[m.Target] = true
	}
	return true
}

func fullImageID(id string) bool {
	return strings.HasPrefix(id, "sha256:") && fullDockerID.MatchString(strings.TrimPrefix(id, "sha256:"))
}

// Validate refuses anything the adapter would have to guess about. Every bound here is a
// wire bound as well: PR B rejects a frame that fails it before touching the runtime.
func (r DeploymentRequest) Validate(now time.Time) error {
	if !deploymentUUID.MatchString(r.Deployment) || !execStreamID.MatchString(r.Endpoint) || !deploymentProject.MatchString(r.Project) || r.Revision < 1 || r.Revision > 100 {
		return errors.New("invalid deployment identity")
	}
	if !r.Deadline.After(now) || r.Deadline.After(now.Add(DeploymentLifetime)) {
		return errors.New("invalid deployment deadline")
	}
	if len(r.Services) == 0 || len(r.Services) > MaxDeploymentServices {
		return errors.New("invalid service count")
	}
	type binding struct {
		ip       string
		port     int
		protocol string
	}
	names, containers, replaces, bindings, pulled, mounted := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[binding]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range r.Services {
		image := (s.Pull == nil && fullImageID(s.ImageID)) || (s.Pull != nil && s.ImageID == "" && s.Pull.valid())
		if !deploymentService.MatchString(s.Name) || names[s.Name] || !ValidContainerID(s.ContainerName) || containers[s.ContainerName] || !image || s.Replaces.Validate() != nil || replaces[s.Replaces.ContainerID] || !deploymentRestart[s.Restart] {
			return errors.New("invalid deployment service")
		}
		names[s.Name], containers[s.ContainerName], replaces[s.Replaces.ContainerID] = true, true, true
		if s.Pull != nil {
			pulled[s.Pull.Host()] = true
		}
		if !validMounts(s.Mounts, mounted) {
			return errors.New("invalid mount")
		}
		if len(s.Ports) > MaxDeploymentPorts {
			return errors.New("too many ports")
		}
		for _, p := range s.Ports {
			if p.Container < 1 || p.Container > 65535 || p.Host < 1 || p.Host > 65535 || (p.Protocol != "tcp" && p.Protocol != "udp") {
				return errors.New("invalid port")
			}
			// "" is Docker's IPv4 wildcard; 0.0.0.0 and :: are separate binds Docker allows together.
			b := binding{"v4-any", p.Host, p.Protocol}
			if p.HostIP != "" {
				ip, err := netip.ParseAddr(p.HostIP)
				if err != nil || ip.Zone() != "" {
					return errors.New("invalid host address")
				}
				switch {
				case ip.IsUnspecified() && ip.Is4():
				case ip.IsUnspecified():
					b.ip = "v6-any"
				default:
					b.ip = ip.String()
				}
			}
			if bindings[b] {
				return errors.New("duplicate port binding")
			}
			bindings[b] = true
		}
		if len(s.Env) > MaxDeploymentEnvEntries {
			return errors.New("too many environment entries")
		}
		total := 0
		for k, v := range s.Env {
			total += len(k) + len(v)
			if !deploymentEnvName.MatchString(k) || len(v) > MaxDeploymentEnvValueBytes || !utf8.ValidString(v) || strings.ContainsRune(v, 0) || total > MaxDeploymentEnvBytes {
				return errors.New("invalid environment value")
			}
		}
	}
	// Volumes lists exactly the volumes the services mount, once each, so every one is ensured.
	if len(r.Volumes) > MaxDeploymentVolumes {
		return errors.New("too many volumes")
	}
	ensured := map[string]bool{}
	for _, v := range r.Volumes {
		if !mounted[v] || ensured[v] {
			return errors.New("invalid volume")
		}
		ensured[v] = true
	}
	if len(ensured) != len(mounted) {
		return errors.New("invalid volume")
	}
	// No credential travels for a host nothing in this frame pulls from.
	if len(r.Registries) > MaxRegistryAuthHosts {
		return errors.New("too many registry credentials")
	}
	for host, auth := range r.Registries {
		if !pulled[host] || auth.Secret == "" || len(auth.Secret) > MaxRegistryAuthSecretBytes || !utf8.ValidString(auth.Secret) || strings.ContainsRune(auth.Secret, 0) || len(auth.Username) > 255 {
			return errors.New("invalid registry auth")
		}
	}
	return nil
}

type DeploymentResult struct {
	Deployment string               `json:"deployment"`
	Outcome    string               `json:"outcome"`
	Detail     string               `json:"detail"`
	Steps      []DeploymentStep     `json:"steps"`
	Services   []DeploymentIdentity `json:"services"`
}
type DeploymentStep struct {
	Service string `json:"service"`
	Step    string `json:"step"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}
type DeploymentIdentity struct {
	Service     string `json:"service"`
	ContainerID string `json:"container_id"`
	ImageID     string `json:"image_id"`
	CreatedUnix int64  `json:"created_unix"`
	ImageDigest string `json:"image_digest,omitempty"` // the pulled repository digest
}

func (r DeploymentResult) Validate() error {
	if !deploymentUUID.MatchString(r.Deployment) || !resultOutcomes[r.Outcome] || len(r.Detail) > MaxResultDetailBytes || len(r.Steps) > 8*MaxDeploymentServices || len(r.Services) > MaxDeploymentServices {
		return errors.New("invalid deployment result")
	}
	for _, s := range r.Steps {
		quiet := s.Outcome == OutcomeSucceeded || s.Outcome == OutcomeSkipped
		if !deploymentService.MatchString(s.Service) || !deploymentSteps[s.Step] || !(resultOutcomes[s.Outcome] || s.Outcome == OutcomeSkipped) || len(s.Detail) > MaxDeploymentStepDetailBytes || (quiet && s.Detail != "") {
			return errors.New("invalid deployment step")
		}
	}
	for _, id := range r.Services {
		if !deploymentService.MatchString(id.Service) || (InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() != nil || (id.ImageDigest != "" && !imageID.MatchString(id.ImageDigest)) {
			return errors.New("invalid deployment identity")
		}
	}
	return nil
}

// RemovalRequest stops and deletes every adopted container of an application being removed,
// each identified by the same pinned target an inspection or replacement would use rather
// than a name a daemon restart could reassign.
type RemovalRequest struct {
	Deployment string          `json:"deployment"`
	Endpoint   string          `json:"endpoint"`
	Project    string          `json:"project"`
	Deadline   time.Time       `json:"deadline"`
	Containers []RemovalTarget `json:"containers"`
}
type RemovalTarget struct {
	Service string           `json:"service"`
	Target  InspectionTarget `json:"target"`
}

func (r RemovalRequest) Validate(now time.Time) error {
	if !deploymentUUID.MatchString(r.Deployment) || !execStreamID.MatchString(r.Endpoint) || !deploymentProject.MatchString(r.Project) {
		return errors.New("invalid removal identity")
	}
	if !r.Deadline.After(now) || r.Deadline.After(now.Add(DeploymentLifetime)) {
		return errors.New("invalid removal deadline")
	}
	if len(r.Containers) == 0 || len(r.Containers) > MaxRemovalTargets {
		return errors.New("invalid container count")
	}
	names, containers := map[string]bool{}, map[string]bool{}
	for _, c := range r.Containers {
		if !deploymentService.MatchString(c.Service) || names[c.Service] || c.Target.Validate() != nil || containers[c.Target.ContainerID] {
			return errors.New("invalid removal target")
		}
		names[c.Service], containers[c.Target.ContainerID] = true, true
	}
	return nil
}
