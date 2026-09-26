package protocol

import (
	"errors"
	"regexp"
)

// Kubernetes inspection: the inspection frames carry a Deployment target and its status. See
// agent-protocol.md, Container inspection.
const (
	// CapabilityKubernetesInspect marks a cluster agent that answers a WorkloadRef inspection;
	// health validation of a cluster needs it.
	CapabilityKubernetesInspect = "kubernetes.inspect"
	// MaxWorkloadConditions bounds a status's conditions; MaxWorkloadPods its pods.
	MaxWorkloadConditions = 8
	MaxWorkloadPods       = MaxPodContainers * 4
)

// WorkloadRef names the Deployment a cluster inspection reads.
type WorkloadRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// WorkloadStatus is a Deployment's rollout state and its pods. Missing: the Deployment is gone,
// and nothing else is set. A UID other than the target's is a Deployment recreated under the name.
type WorkloadStatus struct {
	UID                string              `json:"uid"`
	Generation         int64               `json:"generation"`
	ObservedGeneration int64               `json:"observed_generation"`
	Desired            int32               `json:"desired"`
	Updated            int32               `json:"updated"`
	Ready              int32               `json:"ready"`
	Available          int32               `json:"available"`
	Conditions         []WorkloadCondition `json:"conditions"`
	Pods               []PodStatus         `json:"pods"`
	Missing            bool                `json:"missing"`
}

type WorkloadCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// PodStatus is one pod of the Deployment. Its containers carry no image: the judge reads state,
// reason, readiness and restarts only, and the answer must fit MaxInspectionFrameBytes.
type PodStatus struct {
	Name       string         `json:"name"`
	UID        string         `json:"uid"`
	Phase      string         `json:"phase"`
	Containers []PodContainer `json:"containers"`
}

// reasonWord is a Kubernetes reason: one CamelCase word, as a rollout_timeout detail carries it.
var reasonWord = regexp.MustCompile(`^[A-Za-z]{1,64}$`)

var (
	conditionStatus = map[string]bool{"True": true, "False": true, "Unknown": true}
	podPhase        = map[string]bool{"Pending": true, "Running": true, "Succeeded": true, "Failed": true, "Unknown": true}
	containerState  = map[string]bool{"running": true, "waiting": true, "terminated": true}
)

func (w WorkloadRef) valid() bool {
	return ValidDNSLabel(w.Namespace) && ValidDNSLabel(w.Name) && deploymentUUID.MatchString(w.UID)
}

// validate bounds an untrusted status: counts non-negative, closed vocabularies, reason words,
// Kubernetes names and the caps.
func (s WorkloadStatus) validate() error {
	invalid := errors.New("invalid workload status")
	if s.Missing {
		if s.UID != "" || s.Generation != 0 || s.ObservedGeneration != 0 || s.Desired != 0 || s.Updated != 0 || s.Ready != 0 || s.Available != 0 || len(s.Conditions) > 0 || len(s.Pods) > 0 {
			return invalid
		}
		return nil
	}
	if !deploymentUUID.MatchString(s.UID) || s.Generation < 1 || s.ObservedGeneration < 0 || s.ObservedGeneration > s.Generation || s.Desired < 0 || s.Updated < 0 || s.Ready < 0 || s.Available < 0 || len(s.Conditions) > MaxWorkloadConditions || len(s.Pods) > MaxWorkloadPods {
		return invalid
	}
	for _, c := range s.Conditions {
		if !reasonWord.MatchString(c.Type) || !conditionStatus[c.Status] || (c.Reason != "" && !reasonWord.MatchString(c.Reason)) {
			return invalid
		}
	}
	for _, p := range s.Pods {
		if !ValidDNSSubdomain(p.Name) || !deploymentUUID.MatchString(p.UID) || !podPhase[p.Phase] || len(p.Containers) > MaxPodContainers {
			return invalid
		}
		for _, c := range p.Containers {
			if !ValidDNSLabel(c.Name) || c.Image != "" || c.ImageID != "" || !containerState[c.State] || (c.Reason != "" && !reasonWord.MatchString(c.Reason)) || c.RestartCount < 0 || c.RestartCount > MaxRestartCount {
				return invalid
			}
		}
	}
	return nil
}

// dockerEmpty is an answer carrying none of the Docker fields: what a cluster answer must be.
func (r ContainerInspection) dockerEmpty() bool {
	return r.State == "" && r.Health == "" && r.RestartCount == 0 && r.ImagePlatform == (ImagePlatform{}) && r.RestartPolicy == "" && r.RestartRetries == 0 && len(r.Ports) == 0 && r.Mounts == (MountCounts{}) && r.NetworkMode == "" && r.NetworkCount == 0 && !r.Privileged && !r.ReadOnlyRootFS && !r.AutoRemove && len(r.Unsupported) == 0 && !r.ConfigurationVerified
}
