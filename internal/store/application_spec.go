package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"regexp"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// ApplicationSpec is the initial secret-reference-only desired-state contract.
// It is not a Compose document or a reconstruction of runtime inventory. Import
// must reject unsupported fields before constructing it (docs/application-schema.md).
type ApplicationSpec struct {
	Kind     string               `json:"kind"`
	Services []ApplicationService `json:"services"`
}
type ApplicationService struct {
	Ports       []ApplicationPort               `json:"ports,omitempty"`
	Restart     string                          `json:"restart,omitempty"`
	Name        string                          `json:"name"`
	Image       string                          `json:"image"`
	Environment map[string]ApplicationSecretRef `json:"environment,omitempty"`
}
type ApplicationPort struct {
	Target    int    `json:"target"`
	Published int    `json:"published"`
	HostIP    string `json:"host_ip,omitempty"`
	Protocol  string `json:"protocol"`
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
var applicationSecretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func encodeApplicationSpec(spec ApplicationSpec) ([]byte, string, error) {
	if spec.Kind != "compose.v1" || len(spec.Services) == 0 || len(spec.Services) > 100 {
		return nil, "", ErrInvalid
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

func applicationSpecDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}
