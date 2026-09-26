package kubernetes

import (
	"context"
	"errors"
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

// deployCluster is a fake API server whose namespace shop enforces Pod Security baseline, that
// grants the agent's access review (unless denied), gives each Deployment a UID and a rising
// generation as the API server would, and, when rollouts is true, reports it rolled out;
// otherwise it reports a rollout still progressing.
func deployCluster(t *testing.T, rollouts, denied bool, objects ...runtime.Object) (*Client, *fake.Clientset) {
	t.Helper()
	status := func(d *appsv1.Deployment) {
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "ReplicaSetUpdated"},
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable", Message: "secret-canary message text"},
		}}
	}
	if rollouts {
		status = func(d *appsv1.Deployment) {
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
		}
	}
	return deployClusterWith(t, status, denied, objects...)
}

// deployClusterWith is deployCluster with the status each written Deployment reports.
func deployClusterWith(t *testing.T, status func(*appsv1.Deployment), denied bool, objects ...runtime.Object) (*Client, *fake.Clientset) {
	t.Helper()
	shop := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop", Labels: map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}}}
	c, cs := cluster(t, append([]runtime.Object{shop}, objects...)...)
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
		status(d)
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
	if s.Service != "web" || s.Step != protocol.StepStart || s.Code != "rollout_timeout" || s.Detail != "progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable,pod=ImagePullBackOff" {
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
// no longer in the definition, and each service's Secret found by its Deployment's own name
// (label-only "old" included); a Secret without the label stays, whatever its name. A service
// with nothing left is skipped.
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
		&corev1.Secret{ObjectMeta: obj("shop-old-secret", "old")},
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
	if !slices.Equal(deleted, []string{"configmaps/shop-web-env", "deployments/shop-old", "deployments/shop-web", "secrets/shop-old-secret", "secrets/shop-web-secret", "services/shop-web"}) {
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

// A removal of one of two colliding service names still finds its Secret by its Deployment's
// name, not by recomputing KubernetesNames from this request's services alone: "we-b" and "we_b"
// collide to the same slug ('-' and '_' both sanitise to '-'), so each carries a hash suffix that
// depends on which of the two is in the plan, and a removal naming only one of them must not
// miss its hashed Secret.
func TestRemoveFindsSecretByDeploymentNameAcrossCollisions(t *testing.T) {
	names := protocol.KubernetesNames("shop", []string{"we-b", "we_b"})
	name := names["we-b"]
	if name == "shop-we-b" {
		t.Fatalf("test setup: expected we-b's name to carry a collision hash, got %q", name)
	}
	deployment := owned("we-b")
	deployment.Name = name
	secret := owned("we-b")
	secret.Name = name + "-secret"
	c, cs := deployCluster(t, true, false, &appsv1.Deployment{ObjectMeta: deployment}, &corev1.Secret{ObjectMeta: secret})
	now := time.Now()
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: []string{"we-b"}}
	res := c.Remove(context.Background(), req, func() {})
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("result %+v", res)
	}
	if _, err := cs.CoreV1().Secrets("shop").Get(context.Background(), name+"-secret", metav1.GetOptions{}); err == nil {
		t.Fatal("the hashed Secret survived a removal naming only one of the colliding services")
	}
}

// A same-name object recreated after the precondition read it is not the object this removal
// found: the delete carries that object's UID as a precondition, and a resulting conflict is
// treated as the object already being gone from this run's perspective rather than a failure.
func TestRemoveGuardsDeletesByTheUIDItRead(t *testing.T) {
	d := owned("web")
	d.Name = "shop-web"
	d.UID = "11111111-1111-4111-8111-111111111111"
	c, cs := deployCluster(t, true, false, &appsv1.Deployment{ObjectMeta: d})
	cs.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del := action.(k8stesting.DeleteActionImpl)
		if del.DeleteOptions.Preconditions == nil || del.DeleteOptions.Preconditions.UID == nil || *del.DeleteOptions.Preconditions.UID != d.UID {
			t.Fatalf("delete without the UID read at precondition: %+v", del.DeleteOptions)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "deployments"}, "shop-web", errors.New("uid precondition failed"))
	})
	now := time.Now()
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: []string{"web"}}
	res := c.Remove(context.Background(), req, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
}

// Dropping a service's last secret-backed key deletes the stale Secret an earlier apply left; an
// unowned Secret under the same planned name is left alone rather than refused, since this apply
// does not write to it.
func TestDeployDropsAStaleSecretWhenKeysAreRemoved(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("first deploy: %+v", res)
	}
	again := deployRequest(time.Minute)
	again.Deployment, again.Revision = "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b", 2
	again.Services[0].SecretKeys = nil
	res = c.Deploy(context.Background(), again, func() {})
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("second deploy: %+v", res)
	}
	if _, err := cs.CoreV1().Secrets("shop").Get(context.Background(), "shop-web-secret", metav1.GetOptions{}); err == nil {
		t.Fatal("the stale Secret survived a redeploy without secret keys")
	}

	foreign := owned("api")
	foreign.Labels = map[string]string{} // unowned: no instance label
	foreign.Name = "shop-api-secret"
	c, cs = deployCluster(t, true, false, &corev1.Secret{ObjectMeta: foreign})
	res = c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("deploy beside a foreign secret: %+v", res)
	}
	kept, err := cs.CoreV1().Secrets("shop").Get(context.Background(), "shop-api-secret", metav1.GetOptions{})
	if err != nil || len(kept.Labels) != 0 {
		t.Fatalf("the foreign secret was touched: %+v %v", kept, err)
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

// Dropping a service's last published port deletes the Service an earlier apply left, guarded by
// the UID it read; an unowned Service under the planned name is left alone.
func TestDeployDropsAStaleServiceWhenPortsAreRemoved(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	if res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("first deploy: %+v", res)
	}
	svc, err := cs.CoreV1().Services("shop").Get(context.Background(), "shop-web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	svc.UID = "22222222-2222-4222-8222-222222222222"
	if _, err := cs.CoreV1().Services("shop").Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs.PrependReactor("delete", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del := action.(k8stesting.DeleteActionImpl)
		if del.DeleteOptions.Preconditions == nil || del.DeleteOptions.Preconditions.UID == nil || *del.DeleteOptions.Preconditions.UID != svc.UID {
			t.Errorf("stale Service deleted without the UID it read: %+v", del.DeleteOptions)
		}
		return false, nil, nil
	})
	again := deployRequest(time.Minute)
	again.Deployment, again.Revision = "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b", 2
	again.Services[0].Ports = []protocol.Port{}
	if res := c.Deploy(context.Background(), again, func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("second deploy: %+v", res)
	}
	if _, err := cs.CoreV1().Services("shop").Get(context.Background(), "shop-web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the stale Service survived a redeploy without ports: %v", err)
	}

	foreign := metav1.ObjectMeta{Namespace: "shop", Name: "shop-api"}
	c, cs = deployCluster(t, true, false, &corev1.Service{ObjectMeta: foreign})
	if res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("deploy beside a foreign Service: %+v", res)
	}
	if _, err := cs.CoreV1().Services("shop").Get(context.Background(), "shop-api", metav1.GetOptions{}); err != nil {
		t.Fatalf("the foreign Service was touched: %v", err)
	}
}

// Only a namespace that enforces Pod Security baseline or restricted is deployed into: a missing
// or privileged level stops the first precondition before any write, naming the level.
func TestDeployRequiresPodSecurityBaseline(t *testing.T) {
	for name, tc := range map[string]struct {
		labels map[string]string
		detail string
	}{
		"missing":    {map[string]string{"pod-security.kubernetes.io/warn": "restricted"}, "missing"},
		"privileged": {map[string]string{"pod-security.kubernetes.io/enforce": "privileged"}, "privileged"},
		"invalid":    {map[string]string{"pod-security.kubernetes.io/enforce": "strict"}, "invalid"},
		"baseline":   {map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}, ""},
		"restricted": {map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			c, cs := deployCluster(t, true, false)
			ns, _ := cs.CoreV1().Namespaces().Get(context.Background(), "shop", metav1.GetOptions{})
			ns.Labels = tc.labels
			if _, err := cs.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			cs.ClearActions()
			started := false
			res := c.Deploy(context.Background(), deployRequest(time.Minute), func() { started = true })
			if res.Validate() != nil {
				t.Fatalf("result %+v %v", res, res.Validate())
			}
			if tc.detail == "" {
				if res.Outcome != protocol.OutcomeSucceeded {
					t.Fatalf("%s refused: %+v", name, res)
				}
				return
			}
			s := res.Steps[0]
			if res.Outcome != protocol.OutcomeDenied || s.Service != "web" || s.Step != protocol.StepPrecondition || s.Code != "pod_security" || s.Detail != tc.detail {
				t.Fatalf("result %+v", res)
			}
			if started || len(writes(cs)) != 0 {
				t.Fatalf("wrote %v, started %v", writes(cs), started)
			}
		})
	}
}

// A write the agent's grant allows but the cluster refuses (a quota, an admission policy) is
// admission_denied naming the object, not forbidden: re-applying the manifest would not help.
func TestDeployAdmissionDenied(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	cs.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "shop-web-env", errors.New("exceeded quota: secret-canary"))
	})
	res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
	i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
	if res.Outcome != protocol.OutcomeDenied || i < 0 || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	if s := res.Steps[i]; s.Service != "web" || s.Step != protocol.StepCreate || s.Code != "admission_denied" || s.Detail != "ConfigMap/shop-web-env" {
		t.Fatalf("step %+v", s)
	}
}

// A rollout the cluster reports as failed ends the wait at once, well before the deadline, with
// the reason: a pod admission refused (ReplicaFailure FailedCreate), or a progress deadline the
// Deployment itself gave up on.
func TestDeployRolloutFailsFast(t *testing.T) {
	for name, tc := range map[string]struct {
		conditions []appsv1.DeploymentCondition
		detail     string
	}{
		"replica failure": {[]appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetCreated"},
			{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate", Message: "secret-canary"},
		}, "progressing=NewReplicaSetCreated,replicafailure=FailedCreate"},
		"progress deadline": {[]appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"},
		}, "progressing=ProgressDeadlineExceeded"},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := deployClusterWith(t, func(d *appsv1.Deployment) {
				d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 0, Conditions: tc.conditions}
			}, false)
			c.poll = time.Second
			began := time.Now()
			res := c.Deploy(context.Background(), deployRequest(time.Minute), func() {})
			if took := time.Since(began); took > 2*c.poll {
				t.Fatalf("the wait ran %v past a final failure", took)
			}
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			if res.Outcome != protocol.OutcomeFailed || i < 0 || res.Validate() != nil {
				t.Fatalf("result %+v %v", res, res.Validate())
			}
			if s := res.Steps[i]; s.Step != protocol.StepStart || s.Code != "rollout_timeout" || s.Detail != tc.detail {
				t.Fatalf("step %+v", s)
			}
		})
	}
}

// A pod that is Ready but not yet Available (inside minReadySeconds) does not end the wait: a
// process that exits seconds after it starts must not read as a successful rollout.
func TestDeployReadyIsNotAvailable(t *testing.T) {
	c, _ := deployClusterWith(t, func(d *appsv1.Deployment) {
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 0}
	}, false)
	res := c.Deploy(context.Background(), deployRequest(300*time.Millisecond), func() {})
	if res.Outcome != protocol.OutcomeTimedOut {
		t.Fatalf("a Ready but unavailable pod ended the wait: %+v", res)
	}
}

// A service whose Deployment was never created (a failed revision wrote its ConfigMap and
// Secret, then stopped) is still removed whole: its Secret is found by its ConfigMap's name.
func TestRemoveFindsSecretByConfigMapName(t *testing.T) {
	cm, secret := owned("cache"), owned("cache")
	cm.Name, secret.Name = "shop-cache-env", "shop-cache-secret"
	c, cs := deployCluster(t, true, false, &corev1.ConfigMap{ObjectMeta: cm}, &corev1.Secret{ObjectMeta: secret})
	now := time.Now()
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: []string{"web"}}
	if res := c.Remove(context.Background(), req, func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("result %+v", res)
	}
	_, errCM := cs.CoreV1().ConfigMaps("shop").Get(context.Background(), "shop-cache-env", metav1.GetOptions{})
	_, errS := cs.CoreV1().Secrets("shop").Get(context.Background(), "shop-cache-secret", metav1.GetOptions{})
	if !apierrors.IsNotFound(errCM) || !apierrors.IsNotFound(errS) {
		t.Fatalf("left behind: %v %v", errCM, errS)
	}
}
