package kubernetes

import (
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const installerOwnerLabel = "ky.managed-by"

func installerManaged(labels map[string]string) bool {
	return labels[installerOwnerLabel] == "kyquickstart"
}

func installerManagedObject(o metav1.Object) bool {
	if installerManaged(o.GetLabels()) {
		return true
	}
	switch o := o.(type) {
	case *appsv1.Deployment:
		return installerManaged(o.Spec.Template.Labels)
	case *appsv1.StatefulSet:
		return installerManaged(o.Spec.Template.Labels)
	case *appsv1.DaemonSet:
		return installerManaged(o.Spec.Template.Labels)
	}
	return false
}
func (h *handle) installerManaged() bool {
	return installerManaged(h.meta.Labels) || h.template != nil && installerManaged(h.template.Labels)
}
