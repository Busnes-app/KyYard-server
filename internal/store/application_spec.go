package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// ApplicationSpec is the initial secret-reference-only desired-state contract.
// It is not a Compose document or a reconstruction of runtime inventory. Import
// must reject unsupported fields before constructing it (docs/application-schema.md).
type ApplicationSpec struct {
	Kind     string               `json:"kind"`
	Services []ApplicationService `json:"services"`
	Volumes  []DeclaredVolume     `json:"volumes,omitempty"`
	// Kubernetes holds what only a cluster needs: a migration's destination revision carries
	// its storage choices. The Compose importer never sets it.
	Kubernetes *KubernetesExtension `json:"kubernetes,omitempty"`
}

// KubernetesExtension carries a StorageClass and size per named volume, keyed by its declared
// name. A chosen volume becomes a PersistentVolumeClaim on a cluster.
type KubernetesExtension struct {
	Volumes map[string]KubernetesVolume `json:"volumes"`
}

// KubernetesVolume is one volume's claim: StorageClass "" is the cluster default, Size a whole
// number of Mi, Gi or Ti from 1Mi to 16Ti, AccessMode ReadWriteOnce.
type KubernetesVolume struct {
	StorageClass string `json:"storage_class"`
	Size         string `json:"size"`
	AccessMode   string `json:"access_mode"`
}

// volume is the choice for a declared volume, on a nil extension too.
func (k *KubernetesExtension) volume(name string) (KubernetesVolume, bool) {
	if k == nil {
		return KubernetesVolume{}, false
	}
	v, ok := k.Volumes[name]
	return v, ok
}

// kubernetesClaims turns spec's chosen named volumes into the claims <project>-<volume> (the
// shared naming rule over the declared volumes) and each service's mounts of them, in mount
// order. A volume with no choice, or one two services mount, gets no claim: the preflight
// refused it already.
func kubernetesClaims(project string, spec ApplicationSpec) ([]protocol.KubernetesClaim, map[string][]protocol.KubernetesMount) {
	declared := make([]string, 0, len(spec.Volumes))
	for _, v := range spec.Volumes {
		declared = append(declared, v.Name)
	}
	names := protocol.KubernetesNames(project, declared)
	users := volumeUsers(spec)
	claims, mounts := []protocol.KubernetesClaim{}, map[string][]protocol.KubernetesMount{}
	for _, s := range spec.Services {
		for _, v := range s.Volumes {
			choice, ok := spec.Kubernetes.volume(v.Source)
			if v.Kind != "named" || !ok || users[v.Source] != 1 {
				continue
			}
			claim := names[v.Source]
			if !slices.ContainsFunc(claims, func(c protocol.KubernetesClaim) bool { return c.Name == claim }) {
				claims = append(claims, protocol.KubernetesClaim{Name: claim, StorageClass: choice.StorageClass, Size: choice.Size, AccessMode: choice.AccessMode})
			}
			mounts[s.Name] = append(mounts[s.Name], protocol.KubernetesMount{Claim: claim, MountPath: v.Target, ReadOnly: v.ReadOnly})
		}
	}
	return claims, mounts
}

// carried is k for a new revision spec that brings none: the choices of the volumes spec still
// declares, nil when none remain. A Compose import never sets the extension, and a claim is
// immutable, so the previous choice is what the cluster holds.
func (k *KubernetesExtension) carried(spec ApplicationSpec) *KubernetesExtension {
	if k == nil {
		return nil
	}
	kept := map[string]KubernetesVolume{}
	for _, v := range spec.Volumes {
		if c, ok := k.Volumes[v.Name]; ok {
			kept[v.Name] = c
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return &KubernetesExtension{Volumes: kept}
}

// Valid checks one choice's grammar; the destination inventory decides whether its class exists.
func (v KubernetesVolume) Valid() bool {
	_, size := protocol.StorageSizeBytes(v.Size)
	return protocol.ValidStorageClass(v.StorageClass) && size && v.AccessMode == protocol.AccessReadWriteOnce
}

type ApplicationService struct {
	Ports       []ApplicationPort               `json:"ports,omitempty"`
	Restart     string                          `json:"restart,omitempty"`
	Name        string                          `json:"name"`
	Image       string                          `json:"image"`
	Environment map[string]ApplicationSecretRef `json:"environment,omitempty"`
	Volumes     []ApplicationVolume             `json:"volumes,omitempty"`
}
type ApplicationPort struct {
	Target    int    `json:"target"`
	Published int    `json:"published"`
	HostIP    string `json:"host_ip,omitempty"`
	Protocol  string `json:"protocol"`
}

// ApplicationVolume mounts a declared volume (Kind "named", Source its name) or an
// absolute host path (Kind "bind") at Target.
type ApplicationVolume struct {
	Kind     string `json:"kind"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}
type DeclaredVolume struct {
	Name     string `json:"name"`
	External bool   `json:"external,omitempty"`
}

// VolumeHostName is the runtime volume name, as docker compose derives it.
func VolumeHostName(project string, v DeclaredVolume) string {
	if v.External {
		return v.Name
	}
	return project + "_" + v.Name
}

// namedVolumes lists the declared volumes some service mounts by name, in declared order: the
// volumes a migration asks a storage choice for.
func (spec ApplicationSpec) namedVolumes() []string {
	out := []string{}
	for _, v := range spec.Volumes {
		if slices.ContainsFunc(spec.Services, func(s ApplicationService) bool {
			return slices.ContainsFunc(s.Volumes, func(m ApplicationVolume) bool { return m.Kind == "named" && m.Source == v.Name })
		}) {
			out = append(out, v.Name)
		}
	}
	return out
}

// serviceNames lists the spec's services in order.
func (spec ApplicationSpec) serviceNames() []string {
	out := make([]string, 0, len(spec.Services))
	for _, s := range spec.Services {
		out = append(out, s.Name)
	}
	return out
}

func ValidateApplicationSpec(spec ApplicationSpec) error {
	_, _, err := encodeApplicationSpec(spec)
	return err
}

type ApplicationSecretRef struct {
	SecretRef string `json:"secret_ref"`
}

const (
	MaxApplicationsPerOrganization = 100
	MaxApplicationRevisions        = 100
	MaxApplicationSpecBytes        = 64 * 1024
)

var applicationServiceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var applicationEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
var applicationVolumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
var applicationSecretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// declaredVolumeName is the grammar a newly declared volume name must match: two characters at
// least. It is stricter than ValidVolumeName, which also validates volumes in stored revisions
// saved before this bound existed; loosening later would require both to move together.
var declaredVolumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}$`)

func encodeApplicationSpec(spec ApplicationSpec) ([]byte, string, error) {
	if spec.Kind != "compose.v1" || len(spec.Services) == 0 || len(spec.Services) > 100 {
		return nil, "", ErrInvalid
	}
	if len(spec.Volumes) > 64 {
		return nil, "", ErrInvalid
	}
	declared := make(map[string]bool, len(spec.Volumes))
	for _, v := range spec.Volumes {
		if !ValidVolumeName(v.Name) || declared[v.Name] {
			return nil, "", ErrInvalid
		}
		declared[v.Name] = true
	}
	names := make(map[string]bool, len(spec.Services))
	for _, service := range spec.Services {
		if !applicationServiceName.MatchString(service.Name) || names[service.Name] || len(service.Image) > 512 || !protocol.ValidImageReference(service.Image) || len(service.Environment) > 128 {
			return nil, "", ErrInvalid
		}
		names[service.Name] = true
		switch service.Restart {
		case "", "no", "always", "unless-stopped", "on-failure":
		default:
			return nil, "", ErrInvalid
		}
		if len(service.Ports) > 64 {
			return nil, "", ErrInvalid
		}
		for _, port := range service.Ports {
			if port.Target < 1 || port.Target > 65535 || port.Published < 1 || port.Published > 65535 || (port.Protocol != "tcp" && port.Protocol != "udp") {
				return nil, "", ErrInvalid
			}
			if port.HostIP != "" {
				addr, err := netip.ParseAddr(port.HostIP)
				if err != nil || addr.Zone() != "" {
					return nil, "", ErrInvalid
				}
			}
		}
		if len(service.Volumes) > 32 {
			return nil, "", ErrInvalid
		}
		targets := make(map[string]bool, len(service.Volumes))
		for _, v := range service.Volumes {
			source := (v.Kind == "named" && declared[v.Source]) || (v.Kind == "bind" && CleanAbsolutePath(v.Source))
			if !source || !CleanAbsolutePath(v.Target) || v.Target == "/" || targets[v.Target] {
				return nil, "", ErrInvalid
			}
			targets[v.Target] = true
		}
		for name, ref := range service.Environment {
			if !applicationEnvName.MatchString(name) || !applicationSecretName.MatchString(ref.SecretRef) {
				return nil, "", ErrInvalid
			}
		}
	}
	if k := spec.Kubernetes; k != nil {
		if len(k.Volumes) == 0 || len(k.Volumes) > protocol.MaxKubernetesClaims {
			return nil, "", ErrInvalid
		}
		for name, v := range k.Volumes {
			if !declared[name] || !v.Valid() {
				return nil, "", ErrInvalid
			}
		}
	}
	raw, err := json.Marshal(spec)
	if err != nil || len(raw) > MaxApplicationSpecBytes {
		return nil, "", ErrInvalid
	}
	return raw, applicationSpecDigest(raw), nil
}

// ValidVolumeName is Docker's volume name grammar, as stored: it also validates volumes in
// revisions saved before ValidDeclaredVolumeName's two-character minimum took effect.
func ValidVolumeName(name string) bool { return applicationVolumeName.MatchString(name) }

// ValidDeclaredVolumeName is the grammar for a newly declared volume name, at least two
// characters. Importers use this, not ValidVolumeName, so a one-character name can no longer
// be declared even though one already stored still validates.
func ValidDeclaredVolumeName(name string) bool { return declaredVolumeName.MatchString(name) }

// CleanAbsolutePath holds mount paths to absolute, normalized, display-safe (so at most
// 255 bytes) text with no surrounding whitespace.
func CleanAbsolutePath(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p && strings.TrimSpace(p) == p && displaySafe(p)
}

func applicationSpecDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}
