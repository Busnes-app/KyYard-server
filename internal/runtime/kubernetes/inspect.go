package kubernetes

import (
	"context"
	"errors"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Inspect reads the Deployment a validation watches and the pods its selector (immutable, unlike
// its labels) selects, within callBudget: a Get and one List, both verbs the manifest already
// grants. A Deployment that is gone answers Missing; one recreated under the name answers with its
// own UID. More than MaxWorkloadPods pods is an error (unavailable), never a cut list: one leaving
// out every baseline pod would read as a recreate, and a restarted pod left out would hide it.
func (c *Client) Inspect(ctx context.Context, target protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
	if err := target.ValidateFor(protocol.RuntimeKubernetes); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	ref := target.Workload
	out := &protocol.ContainerInspection{Target: target}
	d, err := c.cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		out.ObservedAt, out.Workload = time.Now().UTC(), &protocol.WorkloadStatus{Missing: true}
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	status := workloadStatus(d)
	if d.Spec.Selector == nil {
		return nil, errors.New("the Deployment has no pod selector")
	}
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil || selector.Empty() {
		return nil, errors.New("the Deployment has no usable pod selector")
	}
	pods, err := c.cs.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String(), Limit: protocol.MaxWorkloadPods + 1})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) > protocol.MaxWorkloadPods || pods.Continue != "" {
		return nil, errors.New("the Deployment has more pods than an inspection reports")
	}
	for _, p := range pods.Items {
		ps := protocol.PodStatus{Name: p.Name, UID: string(p.UID), Phase: string(p.Status.Phase), Containers: []protocol.PodContainer{}}
		for _, pc := range pod(p).Containers {
			pc.Image, pc.ImageID = "", ""
			if !reasonWord.MatchString(pc.Reason) {
				pc.Reason = ""
			}
			ps.Containers = append(ps.Containers, pc)
		}
		status.Pods = append(status.Pods, ps)
	}
	out.ObservedAt, out.Workload = time.Now().UTC(), status
	return out, nil
}

// workloadStatus is the Deployment's rollout state: its counts and its conditions' reason words.
func workloadStatus(d *appsv1.Deployment) *protocol.WorkloadStatus {
	s := &protocol.WorkloadStatus{UID: string(d.UID), Generation: d.Generation, ObservedGeneration: d.Status.ObservedGeneration, Desired: replicas(d.Spec.Replicas),
		Updated: d.Status.UpdatedReplicas, Ready: d.Status.ReadyReplicas, Available: d.Status.AvailableReplicas, Conditions: []protocol.WorkloadCondition{}, Pods: []protocol.PodStatus{}}
	for _, c := range d.Status.Conditions {
		if len(s.Conditions) == protocol.MaxWorkloadConditions || !reasonWord.MatchString(string(c.Type)) {
			continue
		}
		wc := protocol.WorkloadCondition{Type: string(c.Type), Status: string(c.Status)}
		if reasonWord.MatchString(c.Reason) {
			wc.Reason = c.Reason
		}
		s.Conditions = append(s.Conditions, wc)
	}
	return s
}
