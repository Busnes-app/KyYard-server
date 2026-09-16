package protocol

import "time"

// Product inventory types. Runtime SDK shapes never leave their adapter; these are the bounded
// summaries the control plane stores (docs/application-schema.md, retention-policy.md).
// Container environment is deliberately absent: it is where secrets live.
const (
	MaxContainers = 1000
	MaxImages     = 1000
	MaxNetworks   = 200
	MaxVolumes    = 500
	MaxLabels     = 32
	MaxLabelBytes = 256
)

type Snapshot struct {
	Generation uint64      `json:"generation"`
	ObservedAt time.Time   `json:"observed_at"`
	Engine     Engine      `json:"engine"`
	Containers []Container `json:"containers"`
	Images     []Image     `json:"images"`
	Networks   []Network   `json:"networks"`
	Volumes    []Volume    `json:"volumes"`
	// Truncated names the lists that hit their cap; the UI shows the gap.
	Truncated []string `json:"truncated,omitempty"`
}

type Engine struct {
	Runtime     string `json:"runtime"`
	Version     string `json:"version"`
	APIVersion  string `json:"api_version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Kernel      string `json:"kernel"`
	CPUs        int    `json:"cpus"`
	MemoryBytes int64  `json:"memory_bytes"`
	Hostname    string `json:"hostname"`
}

type Port struct {
	HostIP    string `json:"host_ip,omitempty"`
	Host      int    `json:"host,omitempty"`
	Container int    `json:"container"`
	Protocol  string `json:"protocol"`
}

type Container struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	ImageID   string            `json:"image_id"`
	State     string            `json:"state"`  // created, running, paused, restarting, exited, dead
	Status    string            `json:"status"` // human text from the runtime, bounded
	CreatedAt time.Time         `json:"created_at"`
	Ports     []Port            `json:"ports"`
	Labels    map[string]string `json:"labels"`
	Networks  []string          `json:"networks"`
	// Managed is the Compose project label when present; ownership arrives with M6.
	ComposeProject string `json:"compose_project,omitempty"`
}

type Image struct {
	ID        string    `json:"id"`
	Tags      []string  `json:"tags"`
	Digests   []string  `json:"digests"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

type Network struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Driver string `json:"driver"`
	Scope  string `json:"scope"`
}

type Volume struct {
	Name       string    `json:"name"`
	Driver     string    `json:"driver"`
	Mountpoint string    `json:"mountpoint"`
	CreatedAt  time.Time `json:"created_at"`
}
