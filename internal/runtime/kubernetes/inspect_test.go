package kubernetes

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

const testPodUID = "22222222-3333-4444-8555-666666666666"

// inspectTarget is shop-web, the web service's Deployment, as the settle recorded it.
func inspectTarget() protocol.InspectionTarget {
	return protocol.InspectionTarget{Workload: protocol.WorkloadRef{Namespace: "shop", Name: "shop-web", UID: testUID}}
}

// rolledOut is shop-web at generation 2, its one replica updated, ready and available.
func rolledOut() *appsv1.Deployment {
	m := owned("web")
	m.Name, m.UID, m.Generation = "shop-web", types.UID(testUID), 2
	return &appsv1.Deployment{ObjectMeta: m, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: render.Selector(testInstance, "web")}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable", Message: "secret-canary"}}}}
}

// webPod is a pod of the web service whose one container is in state.
func webPod(name, uid string, state corev1.ContainerState, restarts int32) *corev1.Pod {
	m := owned("web")
	m.Name, m.UID = name, types.UID(uid)
	return &corev1.Pod{ObjectMeta: m, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "ghcr.io/org/web@" + testDigest}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "web", State: state, Ready: state.Running != nil, RestartCount: restarts, ImageID: "secret-canary-image-id"}}}}
}

func running() corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
}

// Inspect answers a validated status for the target Deployment and its pods, with no image and
// no message; a Deployment that is gone answers Missing, one recreated under the name its own
// UID, and more pods than an answer reports is an error.
func TestInspectReadsTheDeploymentAndItsPods(t *testing.T) {
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "secret-canary back-off"}}
	other := webPod("shop-web-x", testPodUID, running(), 0)
	other.Labels = map[string]string{"app": "other"}
	c, _ := cluster(t, rolledOut(), webPod("shop-web-a", testPodUID, waiting, 3), other)
	in, err := c.Inspect(context.Background(), inspectTarget())
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Validate(inspectTarget(), time.Now(), false); err != nil {
		t.Fatalf("answer invalid: %v %+v", err, in.Workload)
	}
	w := in.Workload
	if w.Missing || w.UID != testUID || w.Generation != 2 || w.ObservedGeneration != 2 || w.Desired != 1 || w.Available != 1 || w.Ready != 1 || w.Updated != 1 || len(w.Conditions) != 1 || w.Conditions[0] != (protocol.WorkloadCondition{Type: "Available", Status: "True", Reason: "MinimumReplicasAvailable"}) {
		t.Fatalf("status %+v", w)
	}
	if len(w.Pods) != 1 || w.Pods[0].UID != testPodUID || w.Pods[0].Phase != "Running" || w.Pods[0].Containers[0] != (protocol.PodContainer{Name: "web", State: "waiting", Reason: "CrashLoopBackOff", RestartCount: 3}) {
		t.Fatalf("pods %+v", w.Pods)
	}

	gone, _ := cluster(t)
	if in, err := gone.Inspect(context.Background(), inspectTarget()); err != nil || !in.Workload.Missing || in.Validate(inspectTarget(), time.Now(), false) != nil {
		t.Fatalf("missing: %+v %v", in, err)
	}

	recreated := rolledOut()
	recreated.UID = types.UID(testPodUID)
	again, _ := cluster(t, recreated)
	if in, err := again.Inspect(context.Background(), inspectTarget()); err != nil || in.Workload.UID != testPodUID || in.Validate(inspectTarget(), time.Now(), false) != nil {
		t.Fatalf("recreated: %+v %v", in, err)
	}

	crowd := []runtime.Object{rolledOut()}
	for i := range protocol.MaxWorkloadPods + 1 {
		crowd = append(crowd, webPod("shop-web-"+strconv.Itoa(i), testPodUID, running(), 0))
	}
	many, _ := cluster(t, crowd...)
	if _, err := many.Inspect(context.Background(), inspectTarget()); err == nil {
		t.Fatal("more pods than an answer reports was answered")
	}
	if _, err := c.Inspect(context.Background(), protocol.InspectionTarget{ContainerID: "x"}); err == nil {
		t.Fatal("a container target was read")
	}
}

// A terminated container keeps its reason word; a reason that is not one word is dropped.
func TestInspectKeepsOnlyReasonWords(t *testing.T) {
	exited := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}
	pulling := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "image pull: secret-canary"}}
	c, _ := cluster(t, rolledOut(), webPod("shop-web-a", testPodUID, exited, 0), webPod("shop-web-b", testUID, pulling, 0))
	in, err := c.Inspect(context.Background(), inspectTarget())
	if err != nil || in.Validate(inspectTarget(), time.Now(), false) != nil {
		t.Fatalf("%+v %v", in, err)
	}
	reasons := map[string]string{}
	for _, p := range in.Workload.Pods {
		reasons[p.Name] = p.Containers[0].State + "/" + p.Containers[0].Reason
	}
	if reasons["shop-web-a"] != "terminated/Error" || reasons["shop-web-b"] != "waiting/" {
		t.Fatalf("reasons %v", reasons)
	}
}

// Pods are selected by the Deployment's own selector, which Kubernetes keeps immutable, not by its
// editable labels: a pod of another instance under the same service label is not the target's.
func TestInspectSelectsByTheDeploymentSelector(t *testing.T) {
	foreign := webPod("shop-web-x", testUID, running(), 7)
	foreign.Labels[render.LabelInstance] = "77777777-8888-4999-8aaa-bbbbbbbbbbbb"
	for name, edit := range map[string]func(*appsv1.Deployment){
		"as rendered":    func(*appsv1.Deployment) {},
		"labels edited":  func(d *appsv1.Deployment) { d.Labels = map[string]string{"app": "edited"} },
		"labels removed": func(d *appsv1.Deployment) { d.Labels = nil },
	} {
		d := rolledOut()
		edit(d)
		c, _ := cluster(t, d, webPod("shop-web-a", testPodUID, running(), 0), foreign)
		in, err := c.Inspect(context.Background(), inspectTarget())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if pods := in.Workload.Pods; len(pods) != 1 || pods[0].UID != testPodUID {
			t.Fatalf("%s: pods %+v", name, pods)
		}
	}
	unselected := rolledOut()
	unselected.Spec.Selector = nil
	c, _ := cluster(t, unselected, webPod("shop-web-a", testPodUID, running(), 0))
	if _, err := c.Inspect(context.Background(), inspectTarget()); err == nil {
		t.Fatal("a Deployment without a selector was answered")
	}
}

// A refused or failed read is an error the agent sends as unavailable, never Missing.
func TestInspectReadErrors(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "shop-web", nil)
	for name, fail := range map[string]struct{ verb, resource string }{
		"deployment get forbidden": {"get", "deployments"},
		"pod list failed":          {"list", "pods"},
	} {
		c, cs := cluster(t, rolledOut(), webPod("shop-web-a", testPodUID, running(), 0))
		cs.PrependReactor(fail.verb, fail.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, forbidden
		})
		if in, err := c.Inspect(context.Background(), inspectTarget()); err == nil {
			t.Fatalf("%s: answered %+v", name, in)
		}
	}
}
