// Package migration analyzes how an application adopted on a Docker host would run on a
// Kubernetes cluster: every service on every axis, as supported, operator_choice_required or
// blocked, with a code from a closed vocabulary and a parameter. It is pure: it reads store and
// protocol types, never the store's SQL, a runtime or a Kubernetes library.
package migration

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// Version is the report format.
const Version = 1

// Classes, in increasing severity.
const (
	Supported      = "supported"
	ChoiceRequired = "operator_choice_required"
	Blocked        = "blocked"
)

// Axes, in report order.
const (
	AxisStorage    = "storage"
	AxisNetworking = "networking"
	AxisPorts      = "ports"
	AxisSecrets    = "secrets"
	AxisProbes     = "probes"
	AxisResources  = "resources"
	AxisScheduling = "scheduling"
	AxisFlags      = "flags"
)

// Codes is the closed finding vocabulary; the web has a sentence for each
// (web/src/migration-codes.json).
var Codes = []string{
	"volume_named", "volume_named_shared", "volume_bind", "volume_external", "storage_supported",
	"network_host", "networks_multiple", "network_references", "networking_supported",
	"port_published", "port_host_ip", "port_unpublished",
	"secrets_supported",
	"healthcheck_dropped", "probes_supported",
	"resource_limits_dropped", "resources_supported",
	"scheduling_blocked", "scheduling_supported",
	"flag_blocked", "restart_policy", "read_only_rootfs", "flags_supported",
	"inspection_unavailable",
}

// AssumptionCodes are the report's fixed assumptions.
var AssumptionCodes = []string{"volume_size_unknown"}

// Input is one source analyzed against one destination.
type Input struct {
	// Spec is the source's latest revision, Project its Compose project.
	Spec    store.ApplicationSpec
	Project string
	// Containers and Inspections are keyed by service: the mapped container from the endpoint's
	// inventory and, when the plan-time inspection ran, what it observed.
	Containers  map[string]protocol.Container
	Inspections map[string]protocol.ContainerInspection
	// Volumes are the source endpoint's volumes.
	Volumes     []protocol.Volume
	Destination Destination
	Choices     store.MigrationChoices
}

// Destination is the cluster side: the namespace, the destination application's project and the
// StorageClasses the cluster reports.
type Destination struct {
	Namespace      string
	Project        string
	StorageClasses []protocol.StorageClass
}

type Report struct {
	Version int `json:"version"`
	// Ready is true when no finding is blocked or operator_choice_required.
	Ready       bool            `json:"ready"`
	Services    []ServiceReport `json:"services"`
	Checklist   []Step          `json:"checklist"`
	Assumptions []string        `json:"assumptions"`
}

// ServiceReport's Class is its most severe finding's.
type ServiceReport struct {
	Name     string    `json:"name"`
	Class    string    `json:"class"`
	Findings []Finding `json:"findings"`
}

// Finding's Detail is its code's parameter: a volume name, a mount target, a port as
// <published>/<protocol>, a restart policy, a code from protocol.UnsupportedCodes, a service's
// destination name, "acknowledged" on an acknowledged drop that has no parameter, or "agent" on
// an inspection_unavailable caused by an agent that reports no health.
type Finding struct {
	Axis   string `json:"axis"`
	Class  string `json:"class"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

var severity = map[string]int{Supported: 0, ChoiceRequired: 1, Blocked: 2}

// Inspection codes by the axis and finding they become; any other code is flag_blocked.
var (
	probeCodes      = map[string]bool{"image_config": true}
	resourceCodes   = map[string]bool{"resource_limits": true, "ulimits": true}
	schedulingCodes = map[string]bool{"pid_mode": true, "ipc_mode": true, "cgroup_parent": true, "userns_mode": true, "runtime": true}
	networkCodes    = map[string]bool{"network": true}
)

// Analyze classifies every service of in.Spec, sorted by name, and lists the checklist.
func Analyze(in Input) Report {
	declared := map[string]store.DeclaredVolume{}
	for _, v := range in.Spec.Volumes {
		declared[v.Name] = v
	}
	users := map[string]int{}
	for _, s := range in.Spec.Services {
		seen := map[string]bool{}
		for _, v := range s.Volumes {
			if v.Kind == "named" && !seen[v.Source] {
				seen[v.Source], users[v.Source] = true, users[v.Source]+1
			}
		}
	}
	r := Report{Version: Version, Ready: true, Services: []ServiceReport{}, Assumptions: []string{}}
	names := protocol.KubernetesNames(in.Destination.Project, serviceNames(in.Spec))
	// In an application of several services the others address each one by name; a finding
	// that asks only for the operator's attention is supported once acknowledged.
	shared := len(in.Spec.Services) > 1
	acknowledged := func(code string) string {
		if slices.Contains(in.Choices.Acknowledged, code) {
			return Supported
		}
		return ChoiceRequired
	}
	// drop is a setting the destination does not carry: a choice until acknowledged, then
	// supported, its detail recording the acknowledgement when it has no parameter of its own.
	drop := func(axis, code, detail string) Finding {
		class := acknowledged(code)
		if class == Supported && detail == "" {
			detail = "acknowledged"
		}
		return Finding{axis, class, code, detail}
	}
	for _, s := range in.Spec.Services {
		inspection, inspected := in.Inspections[s.Name]
		var f []Finding
		f = append(f, storage(s, declared, users, in.Choices, in.Destination.StorageClasses)...)
		network := networking(in.Containers[s.Name], inspection, inspected)
		if shared {
			network = append(network, Finding{AxisNetworking, acknowledged("network_references"), "network_references", names[s.Name]})
		} else if len(network) == 0 {
			network = []Finding{{AxisNetworking, Supported, "networking_supported", ""}}
		}
		f = append(f, network...)
		f = append(f, ports(s, shared, acknowledged)...)
		f = append(f, Finding{AxisSecrets, Supported, "secrets_supported", ""})
		f = append(f, inspectedAxes(s, inspection, inspected, drop)...)
		sr := ServiceReport{Name: s.Name, Class: Supported, Findings: f}
		for _, x := range f {
			if severity[x.Class] > severity[sr.Class] {
				sr.Class = x.Class
			}
		}
		r.Ready = r.Ready && sr.Class == Supported
		r.Services = append(r.Services, sr)
	}
	slices.SortFunc(r.Services, func(a, b ServiceReport) int { return strings.Compare(a.Name, b.Name) })
	if len(users) > 0 {
		r.Assumptions = append(r.Assumptions, "volume_size_unknown")
	}
	r.Checklist = checklist(in, users, names)
	return r
}

func serviceNames(spec store.ApplicationSpec) []string {
	out := make([]string, 0, len(spec.Services))
	for _, s := range spec.Services {
		out = append(out, s.Name)
	}
	return out
}

func storage(s store.ApplicationService, declared map[string]store.DeclaredVolume, users map[string]int, choices store.MigrationChoices, classes []protocol.StorageClass) []Finding {
	var out []Finding
	for _, v := range s.Volumes {
		switch {
		case v.Kind == "bind":
			out = append(out, Finding{AxisStorage, Blocked, "volume_bind", v.Target})
		case users[v.Source] > 1:
			out = append(out, Finding{AxisStorage, Blocked, "volume_named_shared", v.Source})
		default:
			code := "volume_named"
			if declared[v.Source].External {
				code = "volume_external"
			}
			class := ChoiceRequired
			if chosen(choices, classes, v.Source) {
				class = Supported
			}
			out = append(out, Finding{AxisStorage, class, code, v.Source})
		}
	}
	if len(out) == 0 {
		out = append(out, Finding{AxisStorage, Supported, "storage_supported", ""})
	}
	return out
}

// chosen reports a valid choice for volume whose StorageClass the destination still reports ("":
// while it reports a default).
func chosen(choices store.MigrationChoices, classes []protocol.StorageClass, volume string) bool {
	c, ok := choices.Volumes[volume]
	return ok && c.Valid() && slices.ContainsFunc(classes, func(sc protocol.StorageClass) bool {
		return sc.Name == c.StorageClass || (c.StorageClass == "" && sc.Default)
	})
}

// networking is what the container's own networks say: host networking, or more than one
// network. Analyze adds how other services address it.
func networking(c protocol.Container, in protocol.ContainerInspection, inspected bool) []Finding {
	switch {
	case slices.Contains(c.Networks, "host") || (inspected && in.NetworkMode == "host"):
		return []Finding{{AxisNetworking, Blocked, "network_host", ""}}
	case len(c.Networks) > 1 || (inspected && (in.NetworkCount > 1 || slices.ContainsFunc(in.Unsupported, func(code string) bool { return networkCodes[code] }))):
		return []Finding{{AxisNetworking, Supported, "networks_multiple", ""}}
	}
	return nil
}

// ports: a service that publishes none gets no Service, so in an application of several no
// other service can reach it on the cluster.
func ports(s store.ApplicationService, shared bool, acknowledged func(string) string) []Finding {
	var out []Finding
	for _, p := range s.Ports {
		detail := fmt.Sprintf("%d/%s", p.Published, p.Protocol)
		if p.HostIP != "" {
			out = append(out, Finding{AxisPorts, Blocked, "port_host_ip", detail})
		} else {
			out = append(out, Finding{AxisPorts, Supported, "port_published", detail})
		}
	}
	if len(out) == 0 {
		class := Supported
		if shared {
			class = acknowledged("port_unpublished")
		}
		out = append(out, Finding{AxisPorts, class, "port_unpublished", ""})
	}
	return out
}

// inspectedAxes are probes, resources, scheduling and flags: each needs the inspection, so
// without one each is inspection_unavailable rather than read as support. The restart policy is
// the definition's and is judged either way.
func inspectedAxes(s store.ApplicationService, in protocol.ContainerInspection, inspected bool, drop func(axis, code, detail string) Finding) []Finding {
	var out []Finding
	restart := func() {
		if s.Restart == "no" || s.Restart == "on-failure" {
			out = append(out, Finding{AxisFlags, Blocked, "restart_policy", s.Restart})
		}
	}
	if !inspected {
		for _, axis := range []string{AxisProbes, AxisResources, AxisScheduling, AxisFlags} {
			out = append(out, Finding{axis, ChoiceRequired, "inspection_unavailable", ""})
		}
		restart()
		return out
	}
	// An agent without container.inspect.health answers no health: unknown, not "none", and the
	// fix is upgrading that agent (detail "agent"), not waiting for the host to come online.
	switch {
	case slices.ContainsFunc(in.Unsupported, func(c string) bool { return probeCodes[c] }) || (in.Health != "" && in.Health != "none"):
		out = append(out, drop(AxisProbes, "healthcheck_dropped", ""))
	case in.Health == "":
		out = append(out, Finding{AxisProbes, ChoiceRequired, "inspection_unavailable", "agent"})
	default:
		out = append(out, Finding{AxisProbes, Supported, "probes_supported", ""})
	}
	var resources, scheduling, flags []Finding
	for _, c := range in.Unsupported {
		switch {
		case probeCodes[c], networkCodes[c]:
		case resourceCodes[c]:
			resources = append(resources, drop(AxisResources, "resource_limits_dropped", c))
		case schedulingCodes[c]:
			scheduling = append(scheduling, Finding{AxisScheduling, Blocked, "scheduling_blocked", c})
		case c == "read_only_rootfs":
			flags = append(flags, drop(AxisFlags, "read_only_rootfs", ""))
		default:
			flags = append(flags, Finding{AxisFlags, Blocked, "flag_blocked", c})
		}
	}
	if len(resources) == 0 {
		resources = []Finding{{AxisResources, Supported, "resources_supported", ""}}
	}
	if len(scheduling) == 0 {
		scheduling = []Finding{{AxisScheduling, Supported, "scheduling_supported", ""}}
	}
	out = append(append(out, resources...), scheduling...)
	out = append(out, flags...)
	restart()
	if !slices.ContainsFunc(out, func(f Finding) bool { return f.Axis == AxisFlags }) {
		out = append(out, Finding{AxisFlags, Supported, "flags_supported", ""})
	}
	return out
}
