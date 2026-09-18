package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

var (
	ErrInspectionUnavailable = errors.New("runtime inspection unavailable")
	ErrInspectionInvalid     = errors.New("runtime inspection response invalid or exceeds bounds")
	ErrInspectionChanged     = errors.New("runtime inspection target or observations changed")
	ErrInspectionNotFound    = errors.New("runtime inspection target not found")
	platformPart             = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
)

const maxInspectionBody = 1 << 20

type inspectedContainer struct {
	ID         string `json:"Id"`
	Image      string
	Created    time.Time
	State      *struct{ Status string }
	Config     *struct{} // Require presence, but never decode secret-bearing fields.
	HostConfig *struct {
		NetworkMode   string
		Tmpfs         map[string]string
		RestartPolicy struct {
			Name              string
			MaximumRetryCount int
		}
		Privileged, ReadonlyRootfs, AutoRemove *bool
	}
	Mounts []struct {
		Type        string
		Destination string
		RW          *bool
	}
	NetworkSettings *struct {
		Networks map[string]struct{}
		Ports    map[string][]struct{ HostIP, HostPort string }
	}
}

// InspectContainer reads a bounded, redacted observation through Engine v1.41.
// It performs GETs only, follows the pinned image ID rather than a tag, and
// rechecks selected container fields after the image read. Callers own scope,
// authorization/admission through the agent inspection transport.
func (c *Client) InspectContainer(parent context.Context, target protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, callBudget)
	defer cancel()
	var before, after inspectedContainer
	path := "/containers/" + target.ContainerID + "/json"
	if err := c.inspectionGet(ctx, path, &before); err != nil {
		return nil, err
	}
	if before.ID != target.ContainerID || before.Image != target.ImageID || before.Created.Unix() != target.CreatedUnix {
		return nil, ErrInspectionChanged
	}
	out, err := inspectionFacts(before)
	if err != nil {
		return nil, err
	}
	var im struct {
		ID                    string `json:"Id"`
		OS                    string `json:"Os"`
		Architecture, Variant string
	}
	if err = c.inspectionGet(ctx, "/images/"+target.ImageID+"/json", &im); err != nil {
		return nil, err
	}
	if im.ID != target.ImageID {
		return nil, ErrInspectionChanged
	}
	if !platformPart.MatchString(im.OS) || !platformPart.MatchString(im.Architecture) || (im.Variant != "" && !platformPart.MatchString(im.Variant)) {
		return nil, ErrInspectionInvalid
	}
	if err = c.inspectionGet(ctx, path, &after); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(before, after) {
		return nil, ErrInspectionChanged
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	out.Target = target
	out.ImagePlatform = protocol.ImagePlatform{OS: im.OS, Architecture: im.Architecture, Variant: im.Variant}
	out.ObservedAt = time.Now().UTC()
	return out, nil
}

// A separate small read budget avoids changing the fleet snapshot contract.
// Never wrap daemon, decode or transport errors: they can contain configuration.
func (c *Client) inspectionGet(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return ErrInspectionUnavailable
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrInspectionUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrInspectionNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return ErrInspectionUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxInspectionBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrInspectionUnavailable
	}
	if len(body) > maxInspectionBody || json.Unmarshal(body, out) != nil {
		return ErrInspectionInvalid
	}
	return nil
}

func inspectionFacts(raw inspectedContainer) (*protocol.ContainerInspection, error) {
	if raw.State == nil || raw.Config == nil || raw.HostConfig == nil || raw.NetworkSettings == nil || raw.HostConfig.Privileged == nil || raw.HostConfig.ReadonlyRootfs == nil || raw.HostConfig.AutoRemove == nil {
		return nil, ErrInspectionInvalid
	}
	h, n := raw.HostConfig, raw.NetworkSettings
	out := &protocol.ContainerInspection{State: raw.State.Status, RestartPolicy: h.RestartPolicy.Name, RestartRetries: h.RestartPolicy.MaximumRetryCount, Ports: []protocol.Port{}, NetworkCount: len(n.Networks), Privileged: *h.Privileged, ReadOnlyRootFS: *h.ReadonlyRootfs, AutoRemove: *h.AutoRemove}
	switch out.State {
	case "created", "running", "paused", "restarting", "removing", "exited", "dead":
	default:
		return nil, ErrInspectionInvalid
	}
	switch out.RestartPolicy {
	case "", "no":
		out.RestartPolicy = "no"
	case "always", "unless-stopped", "on-failure":
	default:
		return nil, ErrInspectionInvalid
	}
	if out.RestartRetries < 0 || out.RestartRetries > 2147483647 || len(raw.Mounts) > protocol.MaxInspectionEntries || len(h.Tmpfs) > protocol.MaxInspectionEntries || len(n.Networks) > protocol.MaxInspectionEntries || len(n.Ports) > protocol.MaxInspectionEntries {
		return nil, ErrInspectionInvalid
	}
	switch h.NetworkMode {
	case "default", "bridge", "host", "none":
		out.NetworkMode = h.NetworkMode
	case "":
		return nil, ErrInspectionInvalid
	default:
		out.NetworkMode = "custom"
		if strings.HasPrefix(h.NetworkMode, "container:") {
			out.NetworkMode = "container"
		}
	}
	tmpfs := map[string]bool{}
	for _, m := range raw.Mounts {
		if m.RW == nil {
			return nil, ErrInspectionInvalid
		}
		switch m.Type {
		case "bind":
			out.Mounts.Bind++
		case "volume":
			out.Mounts.Volume++
		case "tmpfs":
			out.Mounts.Tmpfs++
			tmpfs[m.Destination] = true
		default:
			out.Mounts.Other++
		}
		if !*m.RW {
			out.Mounts.ReadOnly++
		}
	}
	// Engine may report --tmpfs only in HostConfig.Tmpfs. Count each target
	// once without returning its path or options.
	for target, options := range h.Tmpfs {
		if tmpfs[target] {
			continue
		}
		out.Mounts.Tmpfs++
		readOnly := false
		for _, option := range strings.Split(options, ",") {
			if option == "ro" {
				readOnly = true
			}
			if option == "rw" {
				readOnly = false
			}
		}
		if readOnly {
			out.Mounts.ReadOnly++
		}
	}
	if out.Mounts.Bind+out.Mounts.Volume+out.Mounts.Tmpfs+out.Mounts.Other > protocol.MaxInspectionEntries {
		return nil, ErrInspectionInvalid
	}
	for key, bindings := range n.Ports {
		target, proto, ok := strings.Cut(key, "/")
		port, err := strconv.Atoi(target)
		if !ok || err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != target || (proto != "tcp" && proto != "udp" && proto != "sctp") {
			return nil, ErrInspectionInvalid
		}
		if len(bindings) == 0 {
			out.Ports = append(out.Ports, protocol.Port{Container: port, Protocol: proto})
		}
		if len(bindings) > protocol.MaxInspectionEntries {
			return nil, ErrInspectionInvalid
		}
		for _, b := range bindings {
			host, err := strconv.Atoi(b.HostPort)
			addr, iperr := netip.ParseAddr(b.HostIP)
			if err != nil || host < 1 || host > 65535 || strconv.Itoa(host) != b.HostPort || iperr != nil || addr.Zone() != "" {
				return nil, ErrInspectionInvalid
			}
			out.Ports = append(out.Ports, protocol.Port{Container: port, Host: host, Protocol: proto, HostIP: addr.String()})
		}
		if len(out.Ports) > protocol.MaxInspectionEntries {
			return nil, ErrInspectionInvalid
		}
	}
	slices.SortFunc(out.Ports, func(a, b protocol.Port) int {
		if a.Container != b.Container {
			return a.Container - b.Container
		}
		if a.Protocol != b.Protocol {
			return strings.Compare(a.Protocol, b.Protocol)
		}
		if a.Host != b.Host {
			return a.Host - b.Host
		}
		return strings.Compare(a.HostIP, b.HostIP)
	})
	return out, nil
}
