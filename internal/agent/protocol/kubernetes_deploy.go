package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Cluster deployment capabilities. A cluster agent advertises them when it can apply and remove
// an application's objects; it never advertises deployment.pull, because the kubelet pulls.
const (
	CapabilityKubernetesDeploy = "kubernetes.deploy"
	CapabilityKubernetesRemove = "kubernetes.remove"
)

// KubernetesTarget is where a cluster agent applies or removes an instance's objects, and what
// it labels and annotates them with. A request carries it exactly when the endpoint's runtime is
// kubernetes (ValidateFor).
type KubernetesTarget struct {
	Namespace     string `json:"namespace"`
	ApplicationID string `json:"application_id"`
	InstanceID    string `json:"instance_id"`
	SpecDigest    string `json:"spec_digest"`
}

func (k KubernetesTarget) Validate() error {
	if !ValidDNSLabel(k.Namespace) || !deploymentUUID.MatchString(k.ApplicationID) || !deploymentUUID.MatchString(k.InstanceID) || !imageID.MatchString(k.SpecDigest) {
		return errors.New("a Kubernetes target names a namespace, the application and instance UUIDs and the spec digest")
	}
	return nil
}

// ValidServiceName is the grammar of a service name on the wire: a cluster agent reads it back
// from its objects' labels.
func ValidServiceName(s string) bool { return deploymentService.MatchString(s) }

// KindDeployment is the one object kind a Kubernetes identity names.
const KindDeployment = "Deployment"

// MaxKubeObjectName bounds a Deployment's and a Service's name: a DNS-1123 label.
const MaxKubeObjectName = 63

var labelValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)

// ValidLabelValue is Kubernetes label value syntax: at most 63 characters, alphanumeric at
// both ends, '-', '_' and '.' between.
func ValidLabelValue(s string) bool { return labelValue.MatchString(s) }

// KubernetesNames maps each service to the name of its Deployment and Service:
// <project>-<service> lower-cased, '_' and '.' read as '-', prefixed "ky-" when it would not
// start with a letter (a Service name must). A name longer than 63 characters, or one two
// services share, is cut and suffixed with six hex characters of a digest of project and
// service, so every name is distinct.
func KubernetesNames(project string, services []string) map[string]string {
	base := make(map[string]string, len(services))
	count := map[string]int{}
	for _, s := range services {
		slug := strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				return r
			case r >= 'A' && r <= 'Z':
				return r + 'a' - 'A'
			}
			return '-'
		}, project+"-"+s)
		slug = strings.Trim(slug, "-")
		if slug == "" || slug[0] < 'a' || slug[0] > 'z' {
			slug = "ky-" + slug
		}
		base[s] = slug
		count[slug]++
	}
	out := make(map[string]string, len(services))
	for _, s := range services {
		name := base[s]
		if len(name) > MaxKubeObjectName || count[name] > 1 {
			sum := sha256.Sum256([]byte(project + "/" + s))
			name = strings.TrimRight(name[:min(len(name), MaxKubeObjectName-7)], "-") + "-" + hex.EncodeToString(sum[:])[:6]
		}
		out[s] = name
	}
	return out
}

var kubernetesRestart = map[string]bool{"": true, "always": true, "unless-stopped": true}

// validateKubernetes is Validate for a cluster frame: no Docker field, every image pulled by
// digest (the kubelet pulls, so no tag moves and no credential travels), and secret keys named
// among the environment's.
func (r DeploymentRequest) validateKubernetes() error {
	if r.Kubernetes.Validate() != nil || len(r.Volumes) > 0 || len(r.Registries) > 0 {
		return errors.New("invalid Kubernetes deployment")
	}
	names := map[string]bool{}
	for _, s := range r.Services {
		if !deploymentService.MatchString(s.Name) || names[s.Name] || s.ContainerName != "" || s.ImageID != "" || s.Pull == nil || !s.Pull.valid() || s.Pull.Tag != "" || s.Replaces != (InspectionTarget{}) || len(s.Mounts) > 0 || !kubernetesRestart[s.Restart] {
			return errors.New("invalid Kubernetes service")
		}
		names[s.Name] = true
		if err := validPorts(s.Ports, map[binding]bool{}, false); err != nil {
			return err
		}
		if err := validEnv(s.Env); err != nil {
			return err
		}
		for i, k := range s.SecretKeys {
			if _, ok := s.Env[k]; !ok || (i > 0 && s.SecretKeys[i-1] >= k) {
				return errors.New("invalid secret keys")
			}
		}
	}
	return nil
}

// ValidateFor is Validate holding the frame to the agent's runtime: a Kubernetes target exactly
// when the runtime is kubernetes.
func (r DeploymentRequest) ValidateFor(runtime string, now time.Time) error {
	if (r.Kubernetes != nil) != (runtime == RuntimeKubernetes) {
		return errors.New("the deployment does not match the agent's runtime")
	}
	return r.Validate(now)
}

// ValidateFor is RemovalRequest.Validate holding the frame to the agent's runtime.
func (r RemovalRequest) ValidateFor(runtime string, now time.Time) error {
	if (r.Kubernetes != nil) != (runtime == RuntimeKubernetes) {
		return errors.New("the removal does not match the agent's runtime")
	}
	return r.Validate(now)
}

// valid holds an identity to one of two shapes: a Docker container, or a Kubernetes Deployment
// with its namespace, name, UID, generation and the digest its pod runs.
func (id DeploymentIdentity) valid() bool {
	if id.Kind == "" {
		return id.Namespace == "" && id.Name == "" && id.UID == "" && id.Generation == 0 && (InspectionTarget{ContainerID: id.ContainerID, ImageID: id.ImageID, CreatedUnix: id.CreatedUnix}).Validate() == nil && (id.ImageDigest == "" || imageID.MatchString(id.ImageDigest))
	}
	return id.Kind == KindDeployment && id.ContainerID == "" && id.ImageID == "" && id.CreatedUnix == 0 && ValidDNSLabel(id.Namespace) && ValidDNSLabel(id.Name) && deploymentUUID.MatchString(id.UID) && id.Generation >= 1 && imageID.MatchString(id.ImageDigest)
}

var (
	// kubeObject is a name_taken, conflict or admission_denied detail: the object's kind and name.
	kubeObject = regexp.MustCompile(`^(Deployment|Service|ConfigMap|Secret)/[a-z0-9]([-a-z0-9.]{0,241}[a-z0-9])?$`)
	// rolloutDetail is a rollout_timeout detail: up to three reasons, each a Kubernetes reason
	// word under the condition or pod it came from.
	rolloutDetail = regexp.MustCompile(`^((progressing|available|replicafailure|pod)=[A-Za-z]{1,64}(,(progressing|available|replicafailure|pod)=[A-Za-z]{1,64}){0,2})?$`)
	// podSecurityDetail is a pod_security detail: the namespace's enforce label is absent,
	// privileged, or not a level at all.
	podSecurityDetail = map[string]bool{"": true, "missing": true, "privileged": true, "invalid": true}
)
