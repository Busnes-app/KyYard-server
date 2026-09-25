package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"path"
	"regexp"
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
