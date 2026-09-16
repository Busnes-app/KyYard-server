package protocol

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

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
	// MaxSnapshotBytes is the one limit both sides enforce on the serialized snapshot: the
	// agent shrinks to it before sending, the server refuses past it.
	MaxSnapshotBytes = 1 << 20
	MaxNameBytes     = 255
	MaxImageRefBytes = 512
	MaxStatusBytes   = 128
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

// CleanText makes agent-supplied text safe to show next to other text: control characters,
// line and paragraph separators and bidi controls are removed, and the result is cut on a rune
// boundary at max bytes. The server applies it to every string it stores; the adapter applies
// it before sending so honest agents never trip the limits.
func CleanText(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == utf8.RuneError {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Clamp rewrites a snapshot so it conforms to the declared schema by construction: list caps,
// string bounds, label caps and text safety, whatever the sender did. Truncated lists are named.
func Clamp(s *Snapshot) {
	s.Engine = Engine{Runtime: CleanText(s.Engine.Runtime, 32), Version: CleanText(s.Engine.Version, 64), APIVersion: CleanText(s.Engine.APIVersion, 32), OS: CleanText(s.Engine.OS, 64), Arch: CleanText(s.Engine.Arch, 32), Kernel: CleanText(s.Engine.Kernel, 128), CPUs: s.Engine.CPUs, MemoryBytes: s.Engine.MemoryBytes, Hostname: CleanText(s.Engine.Hostname, MaxNameBytes)}
	truncated := map[string]bool{}
	for _, t := range s.Truncated {
		truncated[CleanText(t, 16)] = true
	}
	if len(s.Containers) > MaxContainers {
		s.Containers, truncated["containers"] = s.Containers[:MaxContainers], true
	}
	for i := range s.Containers {
		c := &s.Containers[i]
		c.ID, c.Name, c.Image, c.ImageID = CleanText(c.ID, 128), CleanText(c.Name, MaxNameBytes), CleanText(c.Image, MaxImageRefBytes), CleanText(c.ImageID, 128)
		c.State, c.Status, c.ComposeProject = CleanText(c.State, 32), CleanText(c.Status, MaxStatusBytes), CleanText(c.ComposeProject, MaxNameBytes)
		c.Labels = cleanLabels(c.Labels)
		if len(c.Ports) > 64 {
			c.Ports = c.Ports[:64]
		}
		for j := range c.Ports {
			c.Ports[j].HostIP, c.Ports[j].Protocol = CleanText(c.Ports[j].HostIP, 64), CleanText(c.Ports[j].Protocol, 8)
		}
		if len(c.Networks) > 32 {
			c.Networks = c.Networks[:32]
		}
		for j := range c.Networks {
			c.Networks[j] = CleanText(c.Networks[j], MaxNameBytes)
		}
		if c.Ports == nil {
			c.Ports = []Port{}
		}
		if c.Networks == nil {
			c.Networks = []string{}
		}
	}
	if len(s.Images) > MaxImages {
		s.Images, truncated["images"] = s.Images[:MaxImages], true
	}
	for i := range s.Images {
		im := &s.Images[i]
		im.ID = CleanText(im.ID, 128)
		im.Tags, im.Digests = cleanList(im.Tags, 32, MaxImageRefBytes), cleanList(im.Digests, 32, MaxImageRefBytes)
	}
	if len(s.Networks) > MaxNetworks {
		s.Networks, truncated["networks"] = s.Networks[:MaxNetworks], true
	}
	for i := range s.Networks {
		n := &s.Networks[i]
		n.ID, n.Name, n.Driver, n.Scope = CleanText(n.ID, 128), CleanText(n.Name, MaxNameBytes), CleanText(n.Driver, 64), CleanText(n.Scope, 32)
	}
	if len(s.Volumes) > MaxVolumes {
		s.Volumes, truncated["volumes"] = s.Volumes[:MaxVolumes], true
	}
	for i := range s.Volumes {
		v := &s.Volumes[i]
		v.Name, v.Driver, v.Mountpoint = CleanText(v.Name, MaxNameBytes), CleanText(v.Driver, 64), CleanText(v.Mountpoint, MaxImageRefBytes)
	}
	if s.Containers == nil {
		s.Containers = []Container{}
	}
	if s.Images == nil {
		s.Images = []Image{}
	}
	if s.Networks == nil {
		s.Networks = []Network{}
	}
	if s.Volumes == nil {
		s.Volumes = []Volume{}
	}
	s.Truncated = nil
	for _, name := range []string{"containers", "images", "networks", "volumes", "labels"} {
		if truncated[name] {
			s.Truncated = append(s.Truncated, name)
		}
	}
}

func cleanLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(out) >= MaxLabels {
			break
		}
		out[CleanText(k, MaxLabelBytes)] = CleanText(in[k], MaxLabelBytes)
	}
	return out
}

func cleanList(in []string, max, each int) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if len(out) >= max {
			break
		}
		out = append(out, CleanText(s, each))
	}
	return out
}

// Shrink drops the tails of the largest lists, then labels, until the serialized snapshot
// fits MaxSnapshotBytes, naming what it dropped. It returns the final encoding.
func Shrink(s *Snapshot) []byte {
	for {
		raw, err := json.Marshal(s)
		if err == nil && len(raw) <= MaxSnapshotBytes {
			return raw
		}
		truncated := map[string]bool{}
		for _, t := range s.Truncated {
			truncated[t] = true
		}
		hasLabels := false
		for i := range s.Containers {
			if len(s.Containers[i].Labels) > 0 {
				hasLabels = true
				break
			}
		}
		switch {
		case hasLabels:
			for i := range s.Containers {
				s.Containers[i].Labels = map[string]string{}
			}
			truncated["labels"] = true
		case len(s.Containers) >= len(s.Images) && len(s.Containers) >= len(s.Volumes) && len(s.Containers) > 0:
			s.Containers, truncated["containers"] = s.Containers[:len(s.Containers)*3/4], true
		case len(s.Images) >= len(s.Volumes) && len(s.Images) > 0:
			s.Images, truncated["images"] = s.Images[:len(s.Images)*3/4], true
		case len(s.Volumes) > 0:
			s.Volumes, truncated["volumes"] = s.Volumes[:len(s.Volumes)*3/4], true
		case len(s.Networks) > 0:
			s.Networks, truncated["networks"] = s.Networks[:len(s.Networks)*3/4], true
		default:
			raw, _ := json.Marshal(s)
			return raw
		}
		s.Truncated = nil
		for _, name := range []string{"containers", "images", "networks", "volumes", "labels"} {
			if truncated[name] {
				s.Truncated = append(s.Truncated, name)
			}
		}
	}
}

// MaxSamples bounds one metrics frame; the adapter reports running containers only.
const MaxSamples = 1000

// Metrics is a bounded observation of running containers taken at one instant.
type Metrics struct {
	ObservedAt time.Time `json:"observed_at"`
	Samples    []Sample  `json:"samples"`
}

// Sample is one container's usage at ObservedAt. CPUPercent is the share of one core over the
// interval since the previous sample (100 = one core busy); the first sample after a restart
// has no interval and reports -1, which the UI shows as "no data" rather than zero.
type Sample struct {
	ContainerID string  `json:"container_id"`
	CPUPercent  float64 `json:"cpu_percent"`
	MemoryBytes int64   `json:"memory_bytes"`
	MemoryLimit int64   `json:"memory_limit"`
	RxBytes     int64   `json:"rx_bytes"`
	TxBytes     int64   `json:"tx_bytes"`
	Pids        int64   `json:"pids"`
}
