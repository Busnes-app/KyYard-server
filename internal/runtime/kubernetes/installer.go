package kubernetes

import (
	"context"
	"errors"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const installerOwnerLabel = "ky.managed-by"

// maxControllerHops bounds the owner walk at a common operator chain, pod → ReplicaSet →
// Deployment → custom resource; a deeper chain is refused.
const maxControllerHops = 3

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

// installerOwned reports whether o, or a controller up its chain of controller references, is
// installer-managed. It fails closed: an owner that cannot be read, is gone, has another UID, is
// a kind the API server does not map or lies past maxControllerHops counts as installer-owned.
func (c *Client) installerOwned(ctx context.Context, o metav1.Object) bool {
	for hop := 0; ; hop++ {
		if installerManagedObject(o) {
			return true
		}
		ref := metav1.GetControllerOf(o)
		if ref == nil {
			return false
		}
		if hop == maxControllerHops {
			return true
		}
		next, err := c.controller(ctx, o.GetNamespace(), ref)
		if err != nil || next.GetUID() != ref.UID {
			return true
		}
		o = next
	}
}

// controller reads the object a controller reference in namespace ns names: built-in workloads
// typed, so installerManagedObject sees pod-template labels, any other kind as metadata only.
func (c *Client) controller(ctx context.Context, ns string, ref *metav1.OwnerReference) (metav1.Object, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, err
	}
	apps, batch, get := c.cs.AppsV1(), c.cs.BatchV1(), metav1.GetOptions{}
	switch gv.Group + "/" + ref.Kind {
	case "apps/ReplicaSet":
		return object(apps.ReplicaSets(ns).Get(ctx, ref.Name, get))
	case "apps/Deployment":
		return object(apps.Deployments(ns).Get(ctx, ref.Name, get))
	case "apps/StatefulSet":
		return object(apps.StatefulSets(ns).Get(ctx, ref.Name, get))
	case "apps/DaemonSet":
		return object(apps.DaemonSets(ns).Get(ctx, ref.Name, get))
	case "batch/Job":
		return object(batch.Jobs(ns).Get(ctx, ref.Name, get))
	case "batch/CronJob":
		return object(batch.CronJobs(ns).Get(ctx, ref.Name, get))
	}
	if c.md == nil {
		return nil, errors.New("controller kind not followed")
	}
	m, err := c.mapper.RESTMappingWithContext(ctx, schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, gv.Version)
	if err != nil {
		return nil, err
	}
	return object(c.md.Resource(m.Resource).Namespace(ns).Get(ctx, ref.Name, get))
}

func object[T metav1.Object](o T, err error) (metav1.Object, error) {
	if err != nil {
		return nil, err
	}
	return o, nil
}
