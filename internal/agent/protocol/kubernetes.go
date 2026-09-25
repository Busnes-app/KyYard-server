package protocol

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"
)

// Runtimes an endpoint can have. The endpoint row fixes one at enrollment; nothing changes it.
const (
	RuntimeDocker     = "docker"
	RuntimeKubernetes = "kubernetes"
)

// Kubernetes capabilities. A cluster agent advertises these and nothing else; a Docker agent
// never advertises them (CapabilitiesFit).
const (
	CapabilityKubernetesInventory = "kubernetes.inventory"
	CapabilityPodLogs             = "pod.logs"
)

// Cluster inventory bounds. Lists are cut at their cap and named in Truncated; nested lists are
// cut silently by Clamp, except a pod's containers, which also mark "pods".
const (
	MaxNodes          = 500
	MaxNamespaces     = 500
	MaxWorkloads      = 2000
	MaxPods           = 2000
	MaxServices       = 2000
	MaxClaims         = 1000
	MaxPodContainers  = 32
	MaxWorkloadImages = 32
	MaxNodeRoles      = 16
	MaxServicePorts   = 32
	MaxKubeNameBytes  = 253
	MaxKubeImageBytes = 1 << 10
	MaxKubeShortBytes = 64
)

// KubernetesInventory is what a cluster agent reports in place of the Docker lists.
type KubernetesInventory struct {
	Nodes      []Node     `json:"nodes"`
	Namespaces []string   `json:"namespaces"`
	Workloads  []Workload `json:"workloads"`
	Pods       []Pod      `json:"pods"`
	Services   []Service  `json:"services"`
	Claims     []Claim    `json:"claims"`
}

type Node struct {
	Name           string   `json:"name"`
	KubeletVersion string   `json:"kubelet_version"`
	OS             string   `json:"os"`
	Arch           string   `json:"arch"`
	Ready          bool     `json:"ready"`
	Roles          []string `json:"roles"`
	Unschedulable  bool     `json:"unschedulable"`
}

// Workload is a Deployment, StatefulSet or DaemonSet.
type Workload struct {
	Kind      string   `json:"kind"`
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Desired   int32    `json:"desired"`
	Ready     int32    `json:"ready"`
	Updated   int32    `json:"updated"`
	Images    []string `json:"images"`
	Paused    bool     `json:"paused"`
}

// Pod carries its Kubernetes phase and its controller: a Deployment's pod names the
// Deployment, not the ReplicaSet between them.
type Pod struct {
	Namespace  string         `json:"namespace"`
	Name       string         `json:"name"`
	Phase      string         `json:"phase"`
	Node       string         `json:"node"`
	OwnerKind  string         `json:"owner_kind"`
	OwnerName  string         `json:"owner_name"`
	StartedAt  time.Time      `json:"started_at"`
	Containers []PodContainer `json:"containers"`
	// containersCut is set by UnmarshalJSON when it dropped containers past MaxPodContainers.
	containersCut bool
}

// PodContainer State is running, waiting or terminated; Reason is the runtime's word for why.
type PodContainer struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	ImageID      string `json:"image_id"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restart_count"`
}

type Service struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	ClusterIP string   `json:"cluster_ip"`
	Ports     []string `json:"ports"`
}

// Claim is a PersistentVolumeClaim.
type Claim struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Phase        string `json:"phase"`
	StorageClass string `json:"storage_class"`
	Capacity     string `json:"capacity"`
}

// UnmarshalJSON caps a pod's containers while decoding: of the nested lists it is the one a
// small document can expand into a large value.
func (p *Pod) UnmarshalJSON(data []byte) error {
	type plain Pod
	var aux struct {
		plain
		Containers json.RawMessage `json:"containers"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*p = Pod(aux.plain)
	if len(aux.Containers) == 0 {
		return nil
	}
	containers, over, err := decodeBounded[PodContainer](aux.Containers, MaxPodContainers)
	p.Containers, p.containersCut = containers, over
	return err
}

func decodeKubernetes(raw []byte) (*KubernetesInventory, []string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	k := &KubernetesInventory{}
	var cut []string
	err := errors.Join(
		decodeField(fields, "nodes", MaxNodes, &k.Nodes, &cut),
		decodeField(fields, "namespaces", MaxNamespaces, &k.Namespaces, &cut),
		decodeField(fields, "workloads", MaxWorkloads, &k.Workloads, &cut),
		decodeField(fields, "pods", MaxPods, &k.Pods, &cut),
		decodeField(fields, "services", MaxServices, &k.Services, &cut),
		decodeField(fields, "claims", MaxClaims, &k.Claims, &cut),
	)
	if err != nil {
		return nil, nil, err
	}
	if !slices.Contains(cut, "pods") && slices.ContainsFunc(k.Pods, func(p Pod) bool { return p.containersCut }) {
		cut = append(cut, "pods")
	}
	return k, cut, nil
}

func decodeField[T any](fields map[string]json.RawMessage, name string, max int, dst *[]T, cut *[]string) error {
	raw, ok := fields[name]
	if !ok {
		return nil
	}
	items, over, err := decodeBounded[T](raw, max)
	if err != nil {
		return err
	}
	*dst = items
	if over {
		*cut = append(*cut, name)
	}
	return nil
}

// clampKubernetes is Clamp for the cluster lists: caps, text safety, no nil lists.
func clampKubernetes(k *KubernetesInventory, truncated map[string]bool) {
	short := func(s string) string { return CleanText(s, MaxKubeShortBytes) }
	name := func(s string) string { return CleanText(s, MaxKubeNameBytes) }
	if len(k.Nodes) > MaxNodes {
		k.Nodes, truncated["nodes"] = k.Nodes[:MaxNodes], true
	}
	for i := range k.Nodes {
		n := &k.Nodes[i]
		n.Name, n.KubeletVersion, n.OS, n.Arch = name(n.Name), short(n.KubeletVersion), short(n.OS), short(n.Arch)
		n.Roles = cleanList(n.Roles, MaxNodeRoles, MaxKubeShortBytes)
	}
	if len(k.Namespaces) > MaxNamespaces {
		k.Namespaces, truncated["namespaces"] = k.Namespaces[:MaxNamespaces], true
	}
	k.Namespaces = cleanList(k.Namespaces, MaxNamespaces, MaxKubeNameBytes)
	if len(k.Workloads) > MaxWorkloads {
		k.Workloads, truncated["workloads"] = k.Workloads[:MaxWorkloads], true
	}
	for i := range k.Workloads {
		w := &k.Workloads[i]
		w.Kind, w.Namespace, w.Name = short(w.Kind), name(w.Namespace), name(w.Name)
		w.Images = cleanList(w.Images, MaxWorkloadImages, MaxKubeImageBytes)
	}
	if len(k.Pods) > MaxPods {
		k.Pods, truncated["pods"] = k.Pods[:MaxPods], true
	}
	for i := range k.Pods {
		p := &k.Pods[i]
		p.Namespace, p.Name, p.Phase, p.Node = name(p.Namespace), name(p.Name), short(p.Phase), name(p.Node)
		p.OwnerKind, p.OwnerName = short(p.OwnerKind), name(p.OwnerName)
		if len(p.Containers) > MaxPodContainers {
			p.Containers, truncated["pods"] = p.Containers[:MaxPodContainers], true
		}
		for j := range p.Containers {
			c := &p.Containers[j]
			c.Name, c.Image, c.ImageID = name(c.Name), CleanText(c.Image, MaxKubeImageBytes), CleanText(c.ImageID, MaxKubeImageBytes)
			c.State, c.Reason = short(c.State), short(c.Reason)
		}
		if p.Containers == nil {
			p.Containers = []PodContainer{}
		}
	}
	if len(k.Services) > MaxServices {
		k.Services, truncated["services"] = k.Services[:MaxServices], true
	}
	for i := range k.Services {
		s := &k.Services[i]
		s.Namespace, s.Name, s.Type, s.ClusterIP = name(s.Namespace), name(s.Name), short(s.Type), short(s.ClusterIP)
		s.Ports = cleanList(s.Ports, MaxServicePorts, MaxKubeShortBytes)
	}
	if len(k.Claims) > MaxClaims {
		k.Claims, truncated["claims"] = k.Claims[:MaxClaims], true
	}
	for i := range k.Claims {
		c := &k.Claims[i]
		c.Namespace, c.Name, c.Phase, c.StorageClass, c.Capacity = name(c.Namespace), name(c.Name), short(c.Phase), name(c.StorageClass), short(c.Capacity)
	}
	if k.Nodes == nil {
		k.Nodes = []Node{}
	}
	if k.Workloads == nil {
		k.Workloads = []Workload{}
	}
	if k.Pods == nil {
		k.Pods = []Pod{}
	}
	if k.Services == nil {
		k.Services = []Service{}
	}
	if k.Claims == nil {
		k.Claims = []Claim{}
	}
}

// shrinkKubernetes drops the tail of the longest cluster list and names it; false when every
// list is already empty.
func shrinkKubernetes(k *KubernetesInventory, truncated map[string]bool) bool {
	lists := []struct {
		name string
		n    int
		cut  func()
	}{
		{"pods", len(k.Pods), func() { k.Pods = k.Pods[:len(k.Pods)*3/4] }},
		{"workloads", len(k.Workloads), func() { k.Workloads = k.Workloads[:len(k.Workloads)*3/4] }},
		{"services", len(k.Services), func() { k.Services = k.Services[:len(k.Services)*3/4] }},
		{"claims", len(k.Claims), func() { k.Claims = k.Claims[:len(k.Claims)*3/4] }},
		{"namespaces", len(k.Namespaces), func() { k.Namespaces = k.Namespaces[:len(k.Namespaces)*3/4] }},
		{"nodes", len(k.Nodes), func() { k.Nodes = k.Nodes[:len(k.Nodes)*3/4] }},
	}
	best := -1
	for i, l := range lists {
		if l.n > 0 && (best < 0 || l.n > lists[best].n) {
			best = i
		}
	}
	if best < 0 {
		return false
	}
	lists[best].cut()
	truncated[lists[best].name] = true
	return true
}

// ErrSnapshotShape is a snapshot whose lists do not match the endpoint's runtime.
var ErrSnapshotShape = errors.New("the snapshot does not match the endpoint's runtime")

// CheckRuntimeShape holds a Kubernetes endpoint to a cluster inventory and no Docker lists,
// and a Docker endpoint to no cluster inventory.
func CheckRuntimeShape(runtime string, s *Snapshot) error {
	if runtime == RuntimeKubernetes {
		if s.Kubernetes == nil || len(s.Containers)+len(s.Images)+len(s.Networks)+len(s.Volumes) > 0 {
			return ErrSnapshotShape
		}
		return nil
	}
	if s.Kubernetes != nil {
		return ErrSnapshotShape
	}
	return nil
}

// kubernetesCapabilities is everything a cluster agent may advertise.
var kubernetesCapabilities = map[string]bool{CapabilityKubernetesInventory: true, CapabilityPodLogs: true}

// CapabilitiesFit reports whether a hello's capabilities belong to the endpoint's runtime:
// a cluster agent names only cluster capabilities, a Docker agent names none of them.
func CapabilitiesFit(runtime string, capabilities []string) bool {
	for _, c := range capabilities {
		if (runtime == RuntimeKubernetes) != kubernetesCapabilities[c] {
			return false
		}
	}
	return true
}

// Cluster health, derived on read from the stored inventory.
const (
	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
	HealthUnknown  = "unknown"
)

// ClusterHealth is degraded when a reported node is not Ready, healthy when every node is
// reported and Ready, and unknown when the endpoint is not active, has no cluster inventory,
// reports no node, or reports only part of its nodes.
func ClusterHealth(active bool, s *Snapshot) string {
	if !active || s == nil || s.Kubernetes == nil || len(s.Kubernetes.Nodes) == 0 {
		return HealthUnknown
	}
	for _, n := range s.Kubernetes.Nodes {
		if !n.Ready {
			return HealthDegraded
		}
	}
	if slices.Contains(s.Truncated, "nodes") {
		return HealthUnknown
	}
	return HealthHealthy
}

var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// ValidDNSLabel is RFC 1123 label syntax as Kubernetes applies it: namespaces, containers.
func ValidDNSLabel(s string) bool { return dnsLabel.MatchString(s) }

// ValidDNSSubdomain is RFC 1123 subdomain syntax as Kubernetes applies it: pod names.
func ValidDNSSubdomain(s string) bool { return len(s) <= 253 && dnsSubdomain.MatchString(s) }

// PodTarget names one pod and, optionally, one of its containers. An empty Container asks the
// agent to take the pod's only container; it refuses when there are several.
type PodTarget struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Container string `json:"container,omitempty"`
}

func (p PodTarget) Validate() error {
	if !ValidDNSLabel(p.Namespace) || !ValidDNSSubdomain(p.Name) || (p.Container != "" && !ValidDNSLabel(p.Container)) {
		return errors.New("a pod target names a namespace, a pod and optionally a container, in Kubernetes name syntax")
	}
	return nil
}

// ValidateFor holds a log request to the one target the runtime reads: a container ID for
// Docker, a pod for Kubernetes, never both.
func (r LogRequest) ValidateFor(runtime string) error {
	if runtime == RuntimeKubernetes {
		if r.Container != "" || r.Pod == nil {
			return errors.New("a Kubernetes log request names a pod and no container ID")
		}
		return r.Pod.Validate()
	}
	if r.Pod != nil || !ValidContainerID(r.Container) {
		return errors.New("a Docker log request names a container ID and no pod")
	}
	return nil
}
