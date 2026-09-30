package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Cluster workload operations: commands on workloads and pods, the configuration read of a
// workload's pod template, its apply, and pod exec.
const (
	// CapabilityKubernetesWorkloads marks a cluster agent that runs the workload and pod
	// actions, reads a workload's configuration and applies it.
	CapabilityKubernetesWorkloads = "kubernetes.workloads"
	// CapabilityPodExec marks a cluster agent that opens an exec session in a pod container.
	CapabilityPodExec = "pod.exec"

	TypeWorkloadApply     = "workload.apply"
	MaxWorkloadApplyBytes = 128 << 10
	// WorkloadApplyService is the one service a workload apply's DeploymentResult names.
	WorkloadApplyService = "workload"

	MaxWorkloadReplicas     = 1000
	MaxWorkloadContainers   = 16
	MaxWorkloadEnv          = 128 // per container
	MaxQuantityBytes        = 32
	MaxResourceVersionBytes = 64
	MaxWorkloadEnvFrom      = MaxListEntries
	MaxWorkloadUnsupported  = 16
)

// Kinds a WorkloadRef names, lower-case as they travel.
const (
	WorkloadDeployment  = "deployment"
	WorkloadStatefulSet = "statefulset"
	WorkloadDaemonSet   = "daemonset"
	WorkloadPod         = "pod"
)

var workloadKinds = map[string]bool{WorkloadDeployment: true, WorkloadStatefulSet: true, WorkloadDaemonSet: true, WorkloadPod: true}

// workloadStrategies are the update strategies each configurable kind has.
var workloadStrategies = map[string][]string{
	WorkloadDeployment:  {"", "RollingUpdate", "Recreate"},
	WorkloadStatefulSet: {"", "RollingUpdate", "OnDelete"},
	WorkloadDaemonSet:   {"", "RollingUpdate", "OnDelete"},
}

// WorkloadUnsupportedCodes names what a workload read carries that an apply would drop.
var WorkloadUnsupportedCodes = []string{"env_field_ref", "resources_extended", "containers_truncated", "env_truncated", "argv_truncated", "env_from_truncated"}

var (
	// envRef is "<name>/<key>": a Secret or ConfigMap name and one of its keys.
	envRef = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/[-._A-Za-z0-9]{1,253}$`)
	// envFrom is a whole Secret or ConfigMap imported as env.
	envFrom = regexp.MustCompile(`^(secret|configmap)/[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// validObject is the object shape: a known kind, a namespace and name as DNS-1123 labels, no UID.
func (w WorkloadRef) validObject() bool {
	return workloadKinds[w.Kind] && w.UID == "" && ValidDNSLabel(w.Namespace) && ValidDNSLabel(w.Name)
}

// ParseWorkloadRef reads "<namespace>/<kind>/<name>", the form a command's Reference carries.
func ParseWorkloadRef(reference string) (WorkloadRef, error) {
	parts := strings.Split(reference, "/")
	if len(parts) != 3 {
		return WorkloadRef{}, errors.New("a workload reference is <namespace>/<kind>/<name>")
	}
	r := WorkloadRef{Namespace: parts[0], Kind: parts[1], Name: parts[2]}
	if !r.validObject() {
		return WorkloadRef{}, errors.New("a workload reference names a namespace, a known lower-case kind and a name, in Kubernetes label syntax")
	}
	return r, nil
}

func (r WorkloadRef) String() string { return r.Namespace + "/" + r.Kind + "/" + r.Name }

// WorkloadContainer is one container of a pod template. ImageID is what a running pod reports,
// "" when none does.
type WorkloadContainer struct {
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	ImageID   string            `json:"image_id"`
	Command   []string          `json:"command"`
	Args      []string          `json:"args"`
	Env       []WorkloadEnv     `json:"env"`
	Resources WorkloadResources `json:"resources"`
}

// WorkloadEnv is a literal Value or a reference ("<name>/<key>") to a Secret or ConfigMap key,
// never both: a referenced value is never read.
type WorkloadEnv struct {
	Name         string `json:"name"`
	Value        string `json:"value"`
	SecretRef    string `json:"secret_ref,omitempty"`
	ConfigMapRef string `json:"config_map_ref,omitempty"`
}

// WorkloadResources are Kubernetes quantity strings, "" when unset.
type WorkloadResources struct {
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

// WorkloadConfiguration is the editable pod template of a Deployment, StatefulSet or DaemonSet.
// Replicas is nil exactly for a DaemonSet; Paused is a Deployment's only. EnvFrom entries are
// "secret/<name>" or "configmap/<name>". Managed is a workload KyYard deployed, edited through
// its application. Unsupported names, from WorkloadUnsupportedCodes, what an apply would drop.
type WorkloadConfiguration struct {
	Target          WorkloadRef         `json:"target"`
	ObservedAt      time.Time           `json:"observed_at"`
	ResourceVersion string              `json:"resource_version"`
	Replicas        *int32              `json:"replicas,omitempty"`
	Paused          bool                `json:"paused"`
	Strategy        string              `json:"strategy"`
	Containers      []WorkloadContainer `json:"containers"`
	InitContainers  []WorkloadContainer `json:"init_containers"`
	EnvFrom         []string            `json:"env_from"`
	Managed         bool                `json:"managed"`
	Unsupported     []string            `json:"unsupported"`
}

var errWorkload = errors.New("invalid workload configuration")

func workloadErr(field string) error { return fmt.Errorf("%w: %s", errWorkload, field) }

// Validate bounds an untrusted agent's read before it reaches an HTTP response.
func (c *WorkloadConfiguration) Validate(target WorkloadRef, now time.Time) error {
	if c.Target != target {
		return workloadErr("target")
	}
	if d := now.Sub(c.ObservedAt); d > MaxClockSkew || d < -MaxClockSkew {
		return workloadErr("observed_at")
	}
	if err := c.validSpec(); err != nil {
		return err
	}
	if b, err := json.Marshal(c); err != nil || len(b) >= MaxConfigurationFrameBytes {
		return workloadErr("size")
	}
	return nil
}

// validSpec is everything but the target match and the observation time.
func (c *WorkloadConfiguration) validSpec() error {
	kind := c.Target.Kind
	switch {
	case !c.Target.validObject() || kind == WorkloadPod:
		return workloadErr("target")
	case !validResourceVersion(c.ResourceVersion):
		return workloadErr("resource_version")
	case (c.Replicas == nil) != (kind == WorkloadDaemonSet) || (c.Replicas != nil && (*c.Replicas < 0 || *c.Replicas > MaxWorkloadReplicas)):
		return workloadErr("replicas")
	case c.Paused && kind != WorkloadDeployment:
		return workloadErr("paused")
	case !slices.Contains(workloadStrategies[kind], c.Strategy):
		return workloadErr("strategy")
	case len(c.Containers) == 0 || len(c.Containers) > MaxWorkloadContainers || len(c.InitContainers) > MaxWorkloadContainers:
		return workloadErr("containers")
	case len(c.EnvFrom) > MaxWorkloadEnvFrom:
		return workloadErr("env_from")
	case len(c.Unsupported) > MaxWorkloadUnsupported:
		return workloadErr("unsupported")
	}
	names := map[string]bool{}
	for _, list := range [][]WorkloadContainer{c.Containers, c.InitContainers} {
		for _, w := range list {
			if names[w.Name] {
				return workloadErr("containers")
			}
			names[w.Name] = true
			if err := w.validate(); err != nil {
				return err
			}
		}
	}
	seen := map[string]bool{}
	for _, e := range c.EnvFrom {
		if seen[e] || !envFrom.MatchString(e) {
			return workloadErr("env_from")
		}
		seen[e] = true
	}
	clear(seen)
	for _, code := range c.Unsupported {
		if seen[code] || !slices.Contains(WorkloadUnsupportedCodes, code) {
			return workloadErr("unsupported")
		}
		seen[code] = true
	}
	return nil
}

func (w WorkloadContainer) validate() error {
	switch {
	case !ValidDNSLabel(w.Name):
		return workloadErr("container name")
	case !validWorkloadImage(w.Image) || !text(w.ImageID, MaxKubeImageBytes):
		return workloadErr("image")
	case !rawArgv(w.Command) || !rawArgv(w.Args):
		return workloadErr("argv")
	case !validQuantity(w.Resources.CPURequest) || !validQuantity(w.Resources.CPULimit) || !validQuantity(w.Resources.MemoryRequest) || !validQuantity(w.Resources.MemoryLimit):
		return workloadErr("resources")
	case len(w.Env) > MaxWorkloadEnv:
		return workloadErr("env")
	}
	total := 0
	seen := map[string]bool{}
	for _, e := range w.Env {
		total += len(e.Name) + len(e.Value)
		refs := 0
		for _, r := range []string{e.SecretRef, e.ConfigMapRef} {
			if r != "" {
				refs++
				if !envRef.MatchString(r) {
					return workloadErr("env")
				}
			}
		}
		if !configurationEnvName(e.Name) || seen[e.Name] || refs > 1 || (refs == 1 && e.Value != "") || !raw(e.Value, MaxDeploymentEnvValueBytes) || total > MaxDeploymentEnvBytes {
			return workloadErr("env")
		}
		seen[e.Name] = true
	}
	return nil
}

// validWorkloadImage is an image reference as a pod spec may write it, tag and digest together
// included.
func validWorkloadImage(s string) bool {
	if name, digest, ok := strings.Cut(s, "@"); ok && strings.Contains(name, ":") && strings.LastIndex(name, ":") > strings.LastIndex(name, "/") {
		return imageID.MatchString(digest) && ValidImageReference(name)
	}
	return ValidImageReference(s)
}

// validQuantity is "" or a Kubernetes quantity of at most MaxQuantityBytes starting with its
// number (ParseQuantity reads a bare suffix such as "Gi" as zero).
func validQuantity(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > MaxQuantityBytes || !(s[0] >= '0' && s[0] <= '9' || s[0] == '.') {
		return false
	}
	q, err := resource.ParseQuantity(s)
	return err == nil && q.Sign() >= 0
}

var resourceVersion = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validResourceVersion(s string) bool { return resourceVersion.MatchString(s) }

// WorkloadApply sets a workload's pod template (image, command, args, env, resources) and
// replicas and paused from Spec, on the object at ResourceVersion only. It is answered by a
// DeploymentResult whose Deployment is Request and whose one service is WorkloadApplyService.
type WorkloadApply struct {
	Request         string                `json:"request"`
	Endpoint        string                `json:"endpoint"`
	IssuedAt        time.Time             `json:"issued_at"`
	Deadline        time.Time             `json:"deadline"`
	Target          WorkloadRef           `json:"target"`
	ResourceVersion string                `json:"resource_version"`
	Spec            WorkloadConfiguration `json:"spec"`
}

// Validate holds the spec to the target and version it was read at: desired state, so no
// observation time, not managed and nothing unsupported.
func (a WorkloadApply) Validate(now time.Time) error {
	if !deploymentUUID.MatchString(a.Request) || !execStreamID.MatchString(a.Endpoint) {
		return errors.New("invalid workload apply identity")
	}
	if err := issued(a.IssuedAt, a.Deadline, now); err != nil {
		return err
	}
	if !a.Deadline.After(now) || a.Deadline.After(now.Add(DeploymentLifetime)) {
		return errors.New("invalid workload apply deadline")
	}
	if a.Spec.Target != a.Target || a.Spec.ResourceVersion != a.ResourceVersion || !a.Spec.ObservedAt.IsZero() || a.Spec.Managed || len(a.Spec.Unsupported) > 0 {
		return errors.New("the workload apply spec does not match its target")
	}
	if err := a.Spec.validSpec(); err != nil {
		return err
	}
	if b, err := json.Marshal(a); err != nil || len(b) > MaxWorkloadApplyBytes {
		return errors.New("the workload apply exceeds its frame")
	}
	return nil
}
