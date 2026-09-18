package protocol

import (
	"errors"
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
type ContainerInspection struct {
	Target                InspectionTarget `json:"target"`
	ObservedAt            time.Time        `json:"observed_at"`
	State                 string           `json:"state"`
	ImagePlatform         ImagePlatform    `json:"image_platform"`
	RestartPolicy         string           `json:"restart_policy"`
	RestartRetries        int              `json:"restart_retries"`
	Ports                 []Port           `json:"ports"`
	Mounts                MountCounts      `json:"mounts"`
	NetworkMode           string           `json:"network_mode"`
	NetworkCount          int              `json:"network_count"`
	Privileged            bool             `json:"privileged"`
	ReadOnlyRootFS        bool             `json:"read_only_rootfs"`
	AutoRemove            bool             `json:"auto_remove"`
	ConfigurationVerified bool             `json:"configuration_verified"` // Always false in this foundation.
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
