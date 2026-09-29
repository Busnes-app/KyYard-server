// Package docker is the Docker Engine adapter: it speaks the Engine REST API over the socket
// and returns product types only. No SDK type leaves this package.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// apiVersion is the oldest Engine API this adapter needs; the daemon serves any newer one.
const apiVersion = "v1.41"

const maxBody = 32 << 20

type Client struct {
	http    *http.Client
	base    string
	cpuMu   sync.Mutex
	cpuPrev map[string]cpuPoint
	// The daemon's default runtime for inspections, read at most once a minute.
	runtimeMu   sync.Mutex
	runtimeName string
	runtimeRead time.Time
	// startWatch and startPoll override the package's for tests; zero is the default.
	startWatch, startPoll time.Duration
}

// callBudget bounds a call whose caller set no deadline of its own.
//
// It is a context deadline rather than an http.Client.Timeout because that timeout also covers
// reading the body: it would cut a pull's progress stream at twenty seconds however generous
// pullBudget was, and every pull of a real image would settle as unknown. Keeping every bound
// in a context means the stated budget and the enforced budget cannot drift apart.
const callBudget = 20 * time.Second

// New returns an adapter for the Engine at socketPath (a Unix socket) or a TCP host.
func New(socketPath string) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	return newClient(transport, "http://docker/"+apiVersion)
}

// newClient is what both New and the tests build, so a test exercises the settings production
// runs with rather than settings a test invented.
func newClient(transport http.RoundTripper, base string) *Client {
	return &Client{http: &http.Client{Transport: transport}, base: base}
}

// bounded gives a call the default budget when its caller named none.
func (c *Client) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, callBudget)
}

// NewHTTP is for tests and TCP daemons: base is the daemon origin.
func NewHTTP(c *http.Client, base string) *Client {
	return &Client{http: c, base: strings.TrimRight(base, "/") + "/" + apiVersion}
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &statusError{path: path, status: resp.StatusCode}
	}
	return json.Unmarshal(body, out)
}

// statusError carries the daemon's status so callers branch on the code rather than on a
// substring of the message. The path in that message holds a caller-supplied identifier -- a
// repository ending in -404 is a legal reference -- so matching "404" in it reads the name as
// readily as the status, and answers "this host does not have that image" about a host that
// was never asked.
type statusError struct {
	path   string
	status int
}

func (e *statusError) Error() string { return fmt.Sprintf("docker %s: HTTP %d", e.path, e.status) }

// statusOf reports the daemon status an error carries, or zero if it carries none.
func statusOf(err error) int {
	var se *statusError
	if errors.As(err, &se) {
		return se.status
	}
	return 0
}

// Engine facts; used at enrollment for runtime_version too.
func (c *Client) Engine(ctx context.Context) (protocol.Engine, error) {
	var info struct {
		ServerVersion string `json:"ServerVersion"`
		OSType        string `json:"OSType"`
		Architecture  string `json:"Architecture"`
		KernelVersion string `json:"KernelVersion"`
		NCPU          int    `json:"NCPU"`
		MemTotal      int64  `json:"MemTotal"`
		Name          string `json:"Name"`
	}
	if err := c.get(ctx, "/info", &info); err != nil {
		return protocol.Engine{}, err
	}
	var ver struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := c.get(ctx, "/version", &ver); err != nil {
		return protocol.Engine{}, err
	}
	return protocol.Engine{Runtime: "docker", Version: info.ServerVersion, APIVersion: ver.APIVersion, OS: info.OSType, Arch: info.Architecture, Kernel: info.KernelVersion, CPUs: info.NCPU, MemoryBytes: info.MemTotal, Hostname: info.Name}, nil
}

// Snapshot reads everything the read-only fleet shows, bounded and sorted so equal state
// serialises equally. Generation is assigned by the caller.
func (c *Client) Snapshot(ctx context.Context) (*protocol.Snapshot, error) {
	engine, err := c.Engine(ctx)
	if err != nil {
		return nil, err
	}
	snap := &protocol.Snapshot{ObservedAt: time.Now().UTC(), Engine: engine, Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}

	var containers []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		Image   string            `json:"Image"`
		ImageID string            `json:"ImageID"`
		State   string            `json:"State"`
		Status  string            `json:"Status"`
		Created int64             `json:"Created"`
		Labels  map[string]string `json:"Labels"`
		Ports   []struct {
			IP          string `json:"IP"`
			PrivatePort int    `json:"PrivatePort"`
			PublicPort  int    `json:"PublicPort"`
			Type        string `json:"Type"`
		} `json:"Ports"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress         string `json:"IPAddress"`
				GlobalIPv6Address string `json:"GlobalIPv6Address"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
		Mounts []struct {
			Type, Name, Source, Destination string
			RW                              bool
		} `json:"Mounts"`
	}
	if err := c.get(ctx, "/containers/json?all=1", &containers); err != nil {
		return nil, err
	}
	for _, ct := range containers {
		name := ""
		if len(ct.Names) > 0 {
			name = strings.TrimPrefix(ct.Names[0], "/")
		}
		pc := protocol.Container{ID: ct.ID, Name: bound(name, 255), Image: bound(ct.Image, 512), ImageID: ct.ImageID, State: ct.State, Status: bound(ct.Status, 128), CreatedAt: time.Unix(ct.Created, 0).UTC(), Ports: []protocol.Port{}, Labels: boundLabels(ct.Labels), Networks: []string{}, NetworkAttachments: []protocol.NetworkAttachment{}}
		for _, p := range ct.Ports {
			pc.Ports = append(pc.Ports, protocol.Port{HostIP: p.IP, Host: p.PublicPort, Container: p.PrivatePort, Protocol: p.Type})
		}
		for n := range ct.NetworkSettings.Networks {
			pc.Networks = append(pc.Networks, n)
		}
		sort.Strings(pc.Networks)
		for _, n := range pc.Networks {
			settings := ct.NetworkSettings.Networks[n]
			pc.NetworkAttachments = append(pc.NetworkAttachments, protocol.NetworkAttachment{Name: n, IP: settings.IPAddress, IP6: settings.GlobalIPv6Address})
		}
		pc.Mounts = []protocol.Mount{}
		for _, m := range ct.Mounts {
			mount := protocol.Mount{Kind: protocol.MountOther, Source: m.Source, Target: m.Destination, ReadOnly: !m.RW}
			switch m.Type {
			case "volume":
				mount.Kind, mount.Source = protocol.MountVolume, m.Name
			case "bind":
				mount.Kind = protocol.MountBind
			}
			pc.Mounts = append(pc.Mounts, mount)
		}
		sort.Slice(pc.Mounts, func(i, j int) bool { return pc.Mounts[i].Target < pc.Mounts[j].Target })
		pc.ComposeProject = ct.Labels["com.docker.compose.project"]
		snap.Containers = append(snap.Containers, pc)
	}
	sort.Slice(snap.Containers, func(i, j int) bool { return snap.Containers[i].Name < snap.Containers[j].Name })
	if len(snap.Containers) > protocol.MaxContainers {
		snap.Containers = snap.Containers[:protocol.MaxContainers]
		snap.Truncated = append(snap.Truncated, "containers")
	}
	c.enrichRunning(ctx, snap.Containers)

	var images []struct {
		ID          string   `json:"Id"`
		RepoTags    []string `json:"RepoTags"`
		RepoDigests []string `json:"RepoDigests"`
		Size        int64    `json:"Size"`
		Created     int64    `json:"Created"`
	}
	if err := c.get(ctx, "/images/json", &images); err != nil {
		return nil, err
	}
	for _, im := range images {
		snap.Images = append(snap.Images, protocol.Image{ID: im.ID, Tags: nonNil(im.RepoTags), Digests: nonNil(im.RepoDigests), SizeBytes: im.Size, CreatedAt: time.Unix(im.Created, 0).UTC()})
	}
	sort.Slice(snap.Images, func(i, j int) bool { return snap.Images[i].ID < snap.Images[j].ID })
	if len(snap.Images) > protocol.MaxImages {
		snap.Images = snap.Images[:protocol.MaxImages]
		snap.Truncated = append(snap.Truncated, "images")
	}

	var networks []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Driver string `json:"Driver"`
		Scope  string `json:"Scope"`
	}
	if err := c.get(ctx, "/networks", &networks); err != nil {
		return nil, err
	}
	for _, n := range networks {
		snap.Networks = append(snap.Networks, protocol.Network{ID: n.ID, Name: bound(n.Name, 255), Driver: n.Driver, Scope: n.Scope})
	}
	sort.Slice(snap.Networks, func(i, j int) bool { return snap.Networks[i].Name < snap.Networks[j].Name })
	if len(snap.Networks) > protocol.MaxNetworks {
		snap.Networks = snap.Networks[:protocol.MaxNetworks]
		snap.Truncated = append(snap.Truncated, "networks")
	}

	var volumes struct {
		Volumes []struct {
			Name       string `json:"Name"`
			Driver     string `json:"Driver"`
			Mountpoint string `json:"Mountpoint"`
			CreatedAt  string `json:"CreatedAt"`
		} `json:"Volumes"`
	}
	if err := c.get(ctx, "/volumes", &volumes); err != nil {
		return nil, err
	}
	for _, v := range volumes.Volumes {
		created, _ := time.Parse(time.RFC3339, v.CreatedAt)
		snap.Volumes = append(snap.Volumes, protocol.Volume{Name: bound(v.Name, 255), Driver: v.Driver, Mountpoint: bound(v.Mountpoint, 512), CreatedAt: created.UTC()})
	}
	sort.Slice(snap.Volumes, func(i, j int) bool { return snap.Volumes[i].Name < snap.Volumes[j].Name })
	if len(snap.Volumes) > protocol.MaxVolumes {
		snap.Volumes = snap.Volumes[:protocol.MaxVolumes]
		snap.Truncated = append(snap.Truncated, "volumes")
	}
	// Conform to the declared schema and fit the shared byte limit before it leaves the host.
	protocol.Clamp(snap)
	_ = protocol.Shrink(snap)
	return snap, nil
}

func bound(s string, n int) string { return protocol.CleanText(s, n) }

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// boundLabels keeps a bounded, sorted subset; labels are configuration, not secrets, but an
// unbounded map is still a storage lever.
func boundLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(out) >= protocol.MaxLabels {
			break
		}
		out[bound(k, protocol.MaxLabelBytes)] = bound(in[k], protocol.MaxLabelBytes)
	}
	return out
}

// The container list carries no start time or health, so each running container is inspected
// once, bounded in count and time so a large host still reports. A container that cannot be
// read (gone, or past the budget) keeps zero values: the UI shows "—", never a stale guess.
const (
	maxSnapshotInspects   = 200
	snapshotInspectBudget = 5 * time.Second
)

func (c *Client) enrichRunning(parent context.Context, containers []protocol.Container) {
	ctx, cancel := context.WithTimeout(parent, snapshotInspectBudget)
	defer cancel()
	inspected := 0
	for i := range containers {
		if containers[i].State != "running" || inspected >= maxSnapshotInspects || ctx.Err() != nil {
			continue
		}
		inspected++
		var raw struct {
			State struct {
				StartedAt string `json:"StartedAt"`
				// Only the status is decoded: the health log carries the healthcheck's output.
				Health *struct{ Status string } `json:"Health"`
			} `json:"State"`
			HostConfig struct {
				RestartPolicy struct{ Name string } `json:"RestartPolicy"`
			} `json:"HostConfig"`
		}
		if err := c.get(ctx, "/containers/"+url.PathEscape(containers[i].ID)+"/json", &raw); err != nil {
			continue
		}
		if started, err := time.Parse(time.RFC3339Nano, raw.State.StartedAt); err == nil && started.Year() > 1 {
			containers[i].StartedAt = started.UTC().Truncate(time.Second)
		}
		containers[i].Health = "none"
		if raw.State.Health != nil && protocol.HealthStates[raw.State.Health.Status] {
			containers[i].Health = raw.State.Health.Status
		}
		containers[i].RestartPolicy = raw.HostConfig.RestartPolicy.Name
	}
}
