package protocol

import (
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Deployment frames carry one plan to the agent and one result back. The request holds
// resolved environment values: it exists only in memory on both sides and is never logged.
// See docs/agent-protocol.md, Deployment apply.
const (
	TypeDeploymentApply          = "deployment.apply"
	TypeDeploymentResult         = "deployment.result"
	CapabilityDeploymentApply    = "deployment.apply"
	MaxDeploymentRequestBytes    = 320 << 10
	MaxDeploymentResultBytes     = 160 << 10
	DeploymentLifetime           = 15 * time.Minute
	MaxDeploymentServices        = 100
	MaxDeploymentEnvEntries      = 128
	MaxDeploymentEnvValueBytes   = 16 << 10
	MaxDeploymentEnvBytes        = 64 << 10
	MaxDeploymentPorts           = 64
	MaxDeploymentStepDetailBytes = 256
	MaxRegistryAuthHosts         = 16
	MaxRegistryAuthSecretBytes   = 4096
	StepPrecondition             = "precondition"
	StepImage                    = "image"
	StepPull                     = "pull"
	StepStop                     = "stop"
	StepRename                   = "rename"
	StepCreate                   = "create"
	StepStart                    = "start"
	StepRemove                   = "remove"
	OutcomeSkipped               = "skipped"
	TypeDeploymentRemove         = "deployment.remove"
	CapabilityDeploymentRemove   = "deployment.remove"
	MaxRemovalTargets            = 100 // three steps each fit a result's 8*MaxDeploymentServices
)

var (
	deploymentUUID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	deploymentProject = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	deploymentService = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	deploymentEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	deploymentSteps   = map[string]bool{StepPrecondition: true, StepImage: true, StepPull: true, StepStop: true, StepRename: true, StepCreate: true, StepStart: true, StepRemove: true}
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
}

// ImagePull names an image by host, repository and the digest it must resolve to.
type ImagePull struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}
type RegistryAuth struct {
	Username string `json:"username"`
	Secret   string `json:"secret"`
}

// Host is the registry host Reference names (internal/registry.ParseReference's rule), or ""
// when it names none or is invalid.
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
	_, digest := SplitImageReference(p.Reference)
	return p.Host() != "" && imageID.MatchString(digest) && digest == p.Digest
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
	names, containers, replaces, bindings, pulled := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[binding]bool{}, map[string]bool{}
	for _, s := range r.Services {
		image := (s.Pull == nil && fullImageID(s.ImageID)) || (s.Pull != nil && s.ImageID == "" && s.Pull.valid())
		if !deploymentService.MatchString(s.Name) || names[s.Name] || !ValidContainerID(s.ContainerName) || containers[s.ContainerName] || !image || s.Replaces.Validate() != nil || replaces[s.Replaces.ContainerID] || !deploymentRestart[s.Restart] {
			return errors.New("invalid deployment service")
		}
		names[s.Name], containers[s.ContainerName], replaces[s.Replaces.ContainerID] = true, true, true
		if s.Pull != nil {
			pulled[s.Pull.Host()] = true
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
