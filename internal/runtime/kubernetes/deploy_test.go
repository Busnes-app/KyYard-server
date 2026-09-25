package kubernetes

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testSpec     = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testApp      = "11111111-2222-4333-8444-555555555555"
	testInstance = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	testUID      = "0f1e2d3c-4b5a-4968-8776-655443322110"
)

var deploymentsResource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

// deployRequest is a two-service frame for namespace shop: web (a secret, a published port) and
// api (neither).
func deployRequest(deadline time.Duration) protocol.DeploymentRequest {
	now := time.Now()
	return protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", Revision: 1, IssuedAt: now, Deadline: now.Add(deadline),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec},
		Services: []protocol.DeploymentService{
			{Name: "web", Restart: "always", Pull: &protocol.ImagePull{Reference: "ghcr.io/org/web@" + testDigest, Digest: testDigest}, Ports: []protocol.Port{{Container: 80, Host: 8080, Protocol: "tcp"}}, Env: map[string]string{"MODE": "prod", "TOKEN": "s3cret"}, SecretKeys: []string{"TOKEN"}, Mounts: []protocol.Mount{}},
			{Name: "api", Pull: &protocol.ImagePull{Reference: "ghcr.io/org/api@" + testDigest, Digest: testDigest}, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: []protocol.Mount{}},
		}}
}

// deployCluster is a fake API server that grants the agent's access review (unless denied),
// gives each Deployment a UID and a rising generation as the API server would, and, when
// rollouts is true, reports it rolled out.
func deployCluster(t *testing.T, rollouts, denied bool, objects ...runtime.Object) (*Client, *fake.Clientset) {
	t.Helper()
	c, cs := cluster(t, objects...)
	c.poll = 10 * time.Millisecond
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		a := review.Spec.ResourceAttributes
		review.Status.Allowed = !denied && a.Namespace == "shop" && a.Verb == "create" && a.Group == "apps" && a.Resource == "deployments"
		return true, review, nil
	})
	cs.PrependReactor("*", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// CreateAction and UpdateAction have the same methods: tell them apart by verb.
		var d *appsv1.Deployment
		switch action.GetVerb() {
		case "create":
			d = action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment)
			d.UID, d.Generation = testUID, 1
		case "update":
			d = action.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment)
			old, err := cs.Tracker().Get(deploymentsResource, d.Namespace, d.Name)
			if err != nil {
				return true, nil, err
			}
			d.UID, d.Generation = old.(*appsv1.Deployment).UID, old.(*appsv1.Deployment).Generation+1
		default:
			return false, nil, nil
		}
		if rollouts {
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1}
		} else {
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"},
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable", Message: "secret-canary message text"},
			}}
		}
		return false, nil, nil
	})
	return c, cs
}

// writes lists the verbs that changed the cluster, the access review aside.
func writes(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" && a.GetResource().Resource != "selfsubjectaccessreviews" {
			out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	return out
}

func owned(service string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: "shop", Labels: map[string]string{render.LabelInstance: testInstance, render.LabelService: service}}
}

// A first apply creates every object labelled as the instance's and waits for each rollout; the
// run reports Deployment identities. A second apply updates the same objects in place, keeping
// the Service's cluster IP. started is called once, after every precondition and before the
// first write.
func TestDeployCreatesThenUpdatesOwnedObjects(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	var startedAt []int
	res := c.Deploy(context.Background(), deployRequest(time.Minute), func() { startedAt = append(startedAt, len(writes(cs))) })
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.Service+" "+s.Step+" "+s.Outcome)
	}
	want := []string{"web precondition succeeded", "api precondition succeeded", "web create succeeded", "web start succeeded", "api create succeeded", "api start succeeded"}
	if !slices.Equal(steps, want) {
		t.Fatalf("steps %v", steps)
	}
	if !slices.Equal(startedAt, []int{0}) {
		t.Fatalf("started at %v", startedAt)
	}
	if !slices.Equal(writes(cs), []string{"create configmaps", "create secrets", "create deployments", "create services", "create configmaps", "create deployments"}) {
		t.Fatalf("writes %v", writes(cs))
	}
	if len(res.Services) != 2 || res.Services[0] != (protocol.DeploymentIdentity{Service: "web", Kind: protocol.KindDeployment, Namespace: "shop", Name: "shop-web", UID: testUID, Generation: 1, ImageDigest: testDigest}) {
		t.Fatalf("identities %+v", res.Services)
	}
	secret, err := cs.CoreV1().Secrets("shop").Get(context.Background(), "shop-web-secret", metav1.GetOptions{})
	if err != nil || string(secret.Data["TOKEN"]) != "s3cret" || secret.Labels[render.LabelInstance] != testInstance {
		t.Fatalf("secret %+v %v", secret, err)
	}
	svc, _ := cs.CoreV1().Services("shop").Get(context.Background(), "shop-web", metav1.GetOptions{})
	svc.Spec.ClusterIP, svc.Spec.ClusterIPs = "10.96.0.7", []string{"10.96.0.7"}
	if _, err := cs.CoreV1().Services("shop").Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	again := deployRequest(time.Minute)
	again.Deployment, again.Revision = "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b", 2
	again.Services[0].Env["MODE"] = "staging"
	cs.ClearActions()
	res = c.Deploy(context.Background(), again, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Services[0].Generation != 2 {
		t.Fatalf("update %+v", res)
	}
	if !slices.Equal(writes(cs), []string{"update configmaps", "update secrets", "update deployments", "update services", "update configmaps", "update deployments"}) {
		t.Fatalf("writes %v", writes(cs))
	}
	cm, _ := cs.CoreV1().ConfigMaps("shop").Get(context.Background(), "shop-web-env", metav1.GetOptions{})
	svc, _ = cs.CoreV1().Services("shop").Get(context.Background(), "shop-web", metav1.GetOptions{})
	if cm.Data["MODE"] != "staging" || cm.Annotations[render.AnnotationRevision] != "2" || svc.Spec.ClusterIP != "10.96.0.7" {
		t.Fatalf("updated %+v %+v", cm, svc.Spec)
	}
}

// An object under a planned name that is not this instance's stops the run before any write,
// naming its kind and name; so does a grant the agent does not hold.
func TestDeployRefusesBeforeWriting(t *testing.T) {
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-api", Labels: map[string]string{"app": "api"}}}
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-web-secret", Labels: map[string]string{render.LabelInstance: "99999999-7777-4888-9999-aaaaaaaaaaaa"}}}
	for name, tc := range map[string]struct {
		objects      []runtime.Object
		denied       bool
		service      string
		code, detail string
	}{
		"unlabelled Deployment":     {[]runtime.Object{foreign}, false, "api", "name_taken", "Deployment/shop-api"},
		"another instance's Secret": {[]runtime.Object{other}, false, "web", "name_taken", "Secret/shop-web-secret"},
		"no grant":                  {nil, true, "web", "forbidden", ""},
	} {
		t.Run(name, func(t *testing.T) {
			c, cs := deployCluster(t, true, tc.denied, tc.objects...)
			started := false
			res := c.Deploy(context.Background(), deployRequest(time.Minute), func() { started = true })
			failing := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Outcome == protocol.OutcomeDenied })
			if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultStepFailed || failing < 0 || res.Validate() != nil {
				t.Fatalf("result %+v", res)
			}
			s := res.Steps[failing]
			if s.Service != tc.service || s.Step != protocol.StepPrecondition || s.Code != tc.code || s.Detail != tc.detail {
				t.Fatalf("step %+v", s)
			}
			if started || len(writes(cs)) != 0 || len(res.Services) != 0 {
				t.Fatalf("wrote %v, started %v", writes(cs), started)
			}
		})
	}
}

// One conflict is re-read and retried; a second is a failed create naming the object.
func TestDeployRetriesOneConflict(t *testing.T) {
	for _, conflicts := range []int{1, 2} {
		existing := &corev1.ConfigMap{ObjectMeta: owned("web")}
		existing.Name = "shop-web-env"
		c, cs := deployCluster(t, true, false, existing)
		left := conflicts
		cs.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			if left == 0 {
				return false, nil, nil
			}
			left--
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "shop-web-env", nil)
		})
		res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
		if conflicts == 1 && res.Outcome != protocol.OutcomeSucceeded {
			t.Fatalf("one conflict: %+v", res)
		}
		if conflicts == 2 {
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			if res.Outcome != protocol.OutcomeFailed || i < 0 || res.Steps[i].Step != protocol.StepCreate || res.Steps[i].Code != "conflict" || res.Steps[i].Detail != "ConfigMap/shop-web-env" || res.Validate() != nil {
				t.Fatalf("two conflicts: %+v", res)
			}
		}
	}
}

// A rollout that does not finish by the deadline times out with the reasons the cluster gives,
// in the closed shape only; the objects stay applied and the identity is still reported.
func TestDeployRolloutTimeout(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: owned("web"), Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "web", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "secret-canary"}}}}}}
	pod.Name = "shop-web-abc"
	c, cs := deployCluster(t, false, false, pod)
	res := c.Deploy(context.Background(), deployRequest(300*time.Millisecond), func() {})
	i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
	if res.Outcome != protocol.OutcomeTimedOut || i < 0 || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	s := res.Steps[i]
	if s.Service != "web" || s.Step != protocol.StepStart || s.Code != "rollout_timeout" || s.Detail != "progressing=ProgressDeadlineExceeded,available=MinimumReplicasUnavailable,pod=ImagePullBackOff" {
		t.Fatalf("step %+v", s)
	}
	if len(res.Services) != 1 || res.Services[0].Name != "shop-web" {
		t.Fatalf("identities %+v", res.Services)
	}
	if _, err := cs.AppsV1().Deployments("shop").Get(context.Background(), "shop-web", metav1.GetOptions{}); err != nil {
		t.Fatalf("the applied Deployment was rolled back: %v", err)
	}
	for _, step := range res.Steps[i+1:] {
		if step.Outcome != protocol.OutcomeSkipped {
			t.Fatalf("after the timeout: %+v", step)
		}
	}
}

// A Docker frame, or a cluster frame that fails validation, is refused with nothing read.
func TestDeployRefusesAnInvalidFrame(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	docker := deployRequest(time.Minute)
	docker.Kubernetes = nil
	res := c.Deploy(context.Background(), docker, func() { t.Fatal("started") })
	if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || len(cs.Actions()) != 0 {
		t.Fatalf("docker frame: %+v %v", res, cs.Actions())
	}
}

// Removal deletes, in the foreground, what carries the instance's label, including a service
// no longer in the definition, and each named Secret that is the instance's; a Secret under a
// planned name without the label, and anything unlabelled, stays. A service with nothing left
// is skipped.
func TestRemoveDeletesOnlyTheInstancesObjects(t *testing.T) {
	obj := func(name, service string) metav1.ObjectMeta {
		m := owned(service)
		m.Name = name
		return m
	}
	objects := []runtime.Object{
		&appsv1.Deployment{ObjectMeta: obj("shop-web", "web")},
		&corev1.Service{ObjectMeta: obj("shop-web", "web")},
		&corev1.ConfigMap{ObjectMeta: obj("shop-web-env", "web")},
		&corev1.Secret{ObjectMeta: obj("shop-web-secret", "web")},
		&appsv1.Deployment{ObjectMeta: obj("shop-old", "old")},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-api-env"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-api-secret"}},
	}
	c, cs := deployCluster(t, true, false, objects...)
	now := time.Now()
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: []string{"api", "web"}}
	started := 0
	res := c.Remove(context.Background(), req, func() { started++ })
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil || started != 1 {
		t.Fatalf("result %+v", res)
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.Service+" "+s.Step+" "+s.Outcome)
	}
	if !slices.Equal(steps, []string{"api precondition succeeded", "api remove skipped", "old precondition succeeded", "old remove succeeded", "web precondition succeeded", "web remove succeeded"}) {
		t.Fatalf("steps %v", steps)
	}
	var deleted []string
	for _, a := range cs.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok {
			deleted = append(deleted, a.GetResource().Resource+"/"+d.GetName())
			if p := d.GetDeleteOptions().PropagationPolicy; p == nil || *p != metav1.DeletePropagationForeground {
				t.Fatalf("%s deleted without foreground propagation", d.GetName())
			}
		}
	}
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"configmaps/shop-web-env", "deployments/shop-old", "deployments/shop-web", "secrets/shop-web-secret", "services/shop-web"}) {
		t.Fatalf("deleted %v", deleted)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "secrets" {
			t.Fatal("secrets were listed")
		}
	}
}

// A removal whose objects are already gone succeeds without writing anything.
func TestRemoveOfNothingSkips(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	now := time.Now()
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: []string{"web"}}
	res := c.Remove(context.Background(), req, func() { t.Fatal("started with nothing to delete") })
	if res.Outcome != protocol.OutcomeSucceeded || len(res.Steps) != 2 || res.Steps[1].Outcome != protocol.OutcomeSkipped || len(writes(cs)) != 0 {
		t.Fatalf("result %+v writes %v", res, writes(cs))
	}
}

// The inventory carries KyYard's labels on the Deployments it applied, and nothing for others.
func TestSnapshotReadsKyYardLabels(t *testing.T) {
	ours := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-web", Labels: map[string]string{render.LabelApplication: testApp, render.LabelInstance: testInstance}}}
	theirs := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "other"}}
	c, _ := cluster(t, ours, theirs)
	snap, _ := c.Snapshot(context.Background())
	byName := map[string]protocol.Workload{}
	for _, w := range snap.Kubernetes.Workloads {
		byName[w.Name] = w
	}
	if byName["shop-web"].Application != testApp || byName["shop-web"].Instance != testInstance || byName["other"].Instance != "" {
		t.Fatalf("workloads %+v", snap.Kubernetes.Workloads)
	}
}
