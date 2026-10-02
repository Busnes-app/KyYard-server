package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
)

// RunDeployment decodes an exact new Deployment, retaining native selectors, ports, probes,
// volume references and security settings. Namespace Pod Security remains the admission gate.
// Identity/service-account fields are constrained because this is a namespace-scoped run.
func RunDeployment(raw json.RawMessage, target WorkloadRef) (*appsv1.Deployment, error) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return nil, errors.New("invalid run manifest")
	}
	var d appsv1.Deployment
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	bad := errors.New("invalid run manifest")
	if decoder.Decode(&d) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, bad
	}
	if d.APIVersion != "apps/v1" || d.Kind != "Deployment" || target.Kind != WorkloadDeployment || d.Name != target.Name || d.Namespace != target.Namespace || !ValidDNSLabel(d.Name) || !ValidDNSLabel(d.Namespace) {
		return nil, bad
	}
	if d.UID != "" || d.ResourceVersion != "" || d.GenerateName != "" || len(d.OwnerReferences) > 0 || len(d.Finalizers) > 0 || d.DeletionTimestamp != nil || d.Generation != 0 || !d.CreationTimestamp.IsZero() {
		return nil, bad
	}
	if d.Spec.Selector == nil || len(d.Spec.Selector.MatchLabels) == 0 || len(d.Spec.Selector.MatchExpressions) > 0 || d.Spec.Paused {
		return nil, bad
	}
	for key, value := range d.Spec.Selector.MatchLabels {
		if d.Spec.Template.Labels[key] != value {
			return nil, bad
		}
	}
	for _, labels := range []map[string]string{d.Labels, d.Spec.Template.Labels} {
		for key, value := range labels {
			if strings.HasPrefix(key, "kyyard.busnes.app/") || key == "app.kubernetes.io/managed-by" && value == "kyyard" {
				return nil, bad
			}
		}
	}
	pod := &d.Spec.Template.Spec
	if pod.ServiceAccountName != "" && pod.ServiceAccountName != "default" || pod.DeprecatedServiceAccount != "" || pod.AutomountServiceAccountToken != nil && *pod.AutomountServiceAccountToken {
		return nil, bad
	}
	// Keep token automount off on the image run path, including manifests which omit the field.
	no := false
	pod.AutomountServiceAccountToken = &no
	if len(pod.Containers) == 0 || len(pod.Containers)+len(pod.InitContainers) > 16 || len(pod.EphemeralContainers) > 0 || len(pod.Volumes) > 32 || pod.RestartPolicy != "" && pod.RestartPolicy != "Always" {
		return nil, bad
	}
	for _, volume := range pod.Volumes {
		if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.ServiceAccountToken != nil {
					return nil, bad
				}
			}
		}
	}
	for _, c := range pod.Containers {
		if !ValidDNSLabel(c.Name) || !validWorkloadImage(c.Image) {
			return nil, bad
		}
	}
	for _, c := range pod.InitContainers {
		if !ValidDNSLabel(c.Name) || !validWorkloadImage(c.Image) {
			return nil, bad
		}
	}
	if d.Spec.Replicas == nil {
		n := int32(1)
		d.Spec.Replicas = &n
	}
	if *d.Spec.Replicas < 0 || *d.Spec.Replicas > MaxWorkloadReplicas {
		return nil, bad
	}
	return &d, nil
}
