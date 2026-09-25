package kubernetes

import (
	"context"
	"slices"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// doomed is one object a removal deletes.
type doomed struct {
	kind, name string
	remove     func(context.Context, string, metav1.DeleteOptions) error
}

// Remove deletes the instance's objects: the Deployments, Services and ConfigMaps in the
// namespace carrying its instance label, and each named service's Secret when it carries the
// label too. An object without it is never touched. The first service's precondition does the
// reads; each service then has a remove step, skipped when nothing of it is left. Deletes are
// foreground, so a Deployment's pods go with it.
func (c *Client) Remove(parent context.Context, req protocol.RemovalRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		return refused(res, err)
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	r := &run{c: c, parent: parent, res: res, started: started, namespace: req.Kubernetes.Namespace, instance: req.Kubernetes.InstanceID}
	found := map[string][]doomed{}
	services := slices.Clone(req.Services)
	r.step(services[0], protocol.StepPrecondition, func() (string, string, string) {
		o, code, detail := r.find(ctx, req, found)
		for s := range found {
			if !slices.Contains(services, s) {
				services = append(services, s)
			}
		}
		slices.Sort(services[1:])
		return o, code, detail
	})
	for i, s := range services {
		if i > 0 {
			r.step(s, protocol.StepPrecondition, succeeded)
		}
		if len(found[s]) == 0 {
			r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: s, Step: protocol.StepRemove, Outcome: protocol.OutcomeSkipped})
			continue
		}
		r.step(s, protocol.StepRemove, func() (string, string, string) {
			r.begin()
			foreground := metav1.DeletePropagationForeground
			for _, d := range found[s] {
				if err := d.remove(ctx, d.name, metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
					return r.failure(ctx, err)
				}
			}
			return succeeded()
		})
	}
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

// find collects, by service label, every object of the instance, and each named service's
// Secret by its name when it is the instance's.
func (r *run) find(ctx context.Context, req protocol.RemovalRequest, found map[string][]doomed) (string, string, string) {
	core, apps := r.c.cs.CoreV1(), r.c.cs.AppsV1()
	selector := metav1.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{render.LabelInstance: r.instance}).String()}
	add := func(kind string, o metav1.Object, remove func(context.Context, string, metav1.DeleteOptions) error) {
		if s := o.GetLabels()[render.LabelService]; protocol.ValidServiceName(s) {
			found[s] = append(found[s], doomed{kind, o.GetName(), remove})
		}
	}
	deployments, err := apps.Deployments(r.namespace).List(ctx, selector)
	if err != nil {
		return r.failure(ctx, err)
	}
	for i := range deployments.Items {
		add("Deployment", &deployments.Items[i], apps.Deployments(r.namespace).Delete)
	}
	svcs, err := core.Services(r.namespace).List(ctx, selector)
	if err != nil {
		return r.failure(ctx, err)
	}
	for i := range svcs.Items {
		add("Service", &svcs.Items[i], core.Services(r.namespace).Delete)
	}
	configs, err := core.ConfigMaps(r.namespace).List(ctx, selector)
	if err != nil {
		return r.failure(ctx, err)
	}
	for i := range configs.Items {
		add("ConfigMap", &configs.Items[i], core.ConfigMaps(r.namespace).Delete)
	}
	// The role grants no list on Secrets: each is read by the name its service gives it.
	names := protocol.KubernetesNames(req.Project, req.Services)
	for _, s := range req.Services {
		secret, ok, err := get(ctx, objectAPI[*corev1.Secret](core.Secrets(r.namespace)), names[s]+"-secret")
		if err != nil {
			return r.failure(ctx, err)
		}
		if ok && r.owned(secret) {
			add("Secret", secret, core.Secrets(r.namespace).Delete)
		}
	}
	return succeeded()
}
