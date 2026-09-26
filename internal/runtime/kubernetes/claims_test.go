package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// claimRequest is deployRequest with web mounting claim shop-data of class (none for "").
func claimRequest(class string) protocol.DeploymentRequest {
	req := deployRequest(time.Minute)
	req.Kubernetes.Claims = []protocol.KubernetesClaim{{Name: "shop-data", StorageClass: class, Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}}
	req.Services[0].Volumes = []protocol.KubernetesMount{{Claim: "shop-data", MountPath: "/data"}}
	return req
}

// ownedClaim is a claim of this instance under web, of class and size.
func ownedClaim(class *string, size string) *corev1.PersistentVolumeClaim {
	m := owned("web")
	m.Name = "shop-data"
	return &corev1.PersistentVolumeClaim{ObjectMeta: m, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: class, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}}}
}

func ptr[T any](v T) *T { return &v }

// removalRequest removes the named services of the instance in namespace shop.
func removalRequest(services ...string) protocol.RemovalRequest {
	now := time.Now()
	return protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: services}
}

// A first apply creates the claim before the service's other objects and mounts it; a second
// apply finds it owned and unchanged and writes nothing to it.
func TestDeployCreatesAClaimOnce(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	res := c.Deploy(context.Background(), claimRequest("fast"), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("result %+v", res)
	}
	if !slices.Equal(writes(cs), []string{"create persistentvolumeclaims", "create configmaps", "create secrets", "create deployments", "create services", "create configmaps", "create deployments"}) {
		t.Fatalf("writes %v", writes(cs))
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims("shop").Get(context.Background(), "shop-data", metav1.GetOptions{})
	if err != nil || *pvc.Spec.StorageClassName != "fast" || pvc.Labels[render.LabelInstance] != testInstance || pvc.Labels[render.LabelService] != "web" || pvc.Spec.Resources.Requests.Storage().String() != "10Gi" {
		t.Fatalf("claim %+v %v", pvc, err)
	}
	d, _ := cs.AppsV1().Deployments("shop").Get(context.Background(), "shop-web", metav1.GetOptions{})
	if v := d.Spec.Template.Spec.Volumes; len(v) != 1 || v[0].PersistentVolumeClaim.ClaimName != "shop-data" || d.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath != "/data" {
		t.Fatalf("pod %+v", d.Spec.Template.Spec)
	}
	cs.ClearActions()
	again := claimRequest("fast")
	again.Deployment = "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b"
	if res := c.Deploy(context.Background(), again, func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("again %+v", res)
	}
	if slices.ContainsFunc(writes(cs), func(w string) bool {
		return w == "create persistentvolumeclaims" || w == "update persistentvolumeclaims"
	}) {
		t.Fatalf("a second apply wrote the claim: %v", writes(cs))
	}
}

// A claim planned with the cluster default keeps whatever class the cluster assigned: the next
// apply is not claim_immutable.
func TestDeployClaimDefaultClassIsStable(t *testing.T) {
	c, cs := deployCluster(t, true, false, ownedClaim(ptr("standard"), "10Gi"))
	res := c.Deploy(context.Background(), claimRequest(""), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || slices.Contains(writes(cs), "create persistentvolumeclaims") {
		t.Fatalf("result %+v writes %v", res, writes(cs))
	}
}

// A claim someone else holds under the planned name, or this instance's claim of another class,
// size or access mode, stops the run at the precondition with nothing written.
func TestDeployClaimPreconditions(t *testing.T) {
	foreign := ownedClaim(ptr("fast"), "10Gi")
	foreign.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	many := ownedClaim(ptr("fast"), "10Gi")
	many.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	for name, tc := range map[string]struct {
		have *corev1.PersistentVolumeClaim
		code string
	}{
		"another owner":  {foreign, "name_taken"},
		"another size":   {ownedClaim(ptr("fast"), "20Gi"), "claim_immutable"},
		"another class":  {ownedClaim(ptr("slow"), "10Gi"), "claim_immutable"},
		"no class":       {ownedClaim(nil, "10Gi"), "claim_immutable"},
		"another access": {many, "claim_immutable"},
	} {
		t.Run(name, func(t *testing.T) {
			c, cs := deployCluster(t, true, false, tc.have)
			started := false
			res := c.Deploy(context.Background(), claimRequest("fast"), func() { started = true })
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			if res.Outcome != protocol.OutcomeDenied || i < 0 || res.Validate() != nil {
				t.Fatalf("result %+v", res)
			}
			if s := res.Steps[i]; s.Service != "web" || s.Step != protocol.StepPrecondition || s.Code != tc.code || s.Detail != "PersistentVolumeClaim/shop-data" {
				t.Fatalf("step %+v", s)
			}
			if started || len(writes(cs)) != 0 {
				t.Fatalf("wrote %v", writes(cs))
			}
		})
	}
}

// A claim that appears between the precondition and the create is taken when it is this
// instance's and as planned, and refused otherwise.
func TestDeployClaimRace(t *testing.T) {
	for name, tc := range map[string]struct {
		appears *corev1.PersistentVolumeClaim
		code    string
	}{
		"as planned":   {ownedClaim(ptr("fast"), "10Gi"), ""},
		"another size": {ownedClaim(ptr("fast"), "1Gi"), "claim_immutable"},
	} {
		t.Run(name, func(t *testing.T) {
			c, cs := deployCluster(t, true, false)
			cs.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				if err := cs.Tracker().Add(tc.appears); err != nil {
					t.Fatal(err)
				}
				return false, nil, nil
			})
			res := c.Deploy(context.Background(), claimRequest("fast"), func() {})
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			switch {
			case tc.code == "" && res.Outcome != protocol.OutcomeSucceeded:
				t.Fatalf("result %+v", res)
			case tc.code != "" && (i < 0 || res.Steps[i].Step != protocol.StepCreate || res.Steps[i].Code != tc.code || res.Validate() != nil):
				t.Fatalf("result %+v", res)
			}
		})
	}
}

// Removal keeps every claim of the instance, deletes none, and reports each as a skipped
// volume step with detail retained under its service; another's claim is not even reported.
// A claim labelled for a service outside the removal's own list is still reported, under a
// service step of its own.
func TestRemoveRetainsClaims(t *testing.T) {
	claim := func(service, name, size string) *corev1.PersistentVolumeClaim {
		m := owned(service)
		m.Name = name
		fast := "fast"
		return &corev1.PersistentVolumeClaim{ObjectMeta: m, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &fast, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}}}
	}
	foreign := claim("web", "shop-other", "1Gi")
	foreign.Labels = map[string]string{render.LabelInstance: "99999999-7777-4888-9999-aaaaaaaaaaaa", render.LabelService: "web"}
	web := &appsv1.Deployment{ObjectMeta: owned("web")}
	web.Name = "shop-web"
	api := &appsv1.Deployment{ObjectMeta: owned("api")}
	api.Name = "shop-api"
	c, cs := deployCluster(t, true, false, web, api, foreign,
		claim("web", "shop-data", "10Gi"), claim("web", "shop-logs", "1Gi"),
		claim("api", "shop-cache", "10Gi"), claim("api", "shop-tmp", "1Gi"),
		claim("worker", "shop-worker-data", "10Gi"))
	now := time.Now()
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: []string{"web", "api"}}
	res := c.Remove(context.Background(), req, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.Service+" "+s.Step+" "+s.Outcome+" "+s.Detail)
	}
	want := []string{
		"web precondition succeeded ", "web remove succeeded ", "web volume skipped retained", "web volume skipped retained",
		"api precondition succeeded ", "api remove succeeded ", "api volume skipped retained", "api volume skipped retained",
		"worker precondition succeeded ", "worker remove skipped ", "worker volume skipped retained",
	}
	if !slices.Equal(steps, want) {
		t.Fatalf("steps %q", steps)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "persistentvolumeclaims" {
			t.Fatal("a claim was deleted")
		}
	}
	for _, name := range []string{"shop-data", "shop-logs", "shop-cache", "shop-tmp", "shop-worker-data"} {
		if _, err := cs.CoreV1().PersistentVolumeClaims("shop").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("the claim %s is gone: %v", name, err)
		}
	}
}

// The snapshot reports StorageClasses by name, the default one marked by either annotation.
func TestSnapshotReadsStorageClasses(t *testing.T) {
	c, _ := cluster(t,
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}, Provisioner: "rancher.io/local-path"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Annotations: map[string]string{"storageclass.beta.kubernetes.io/is-default-class": "true"}}, Provisioner: "x"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "false"}}, Provisioner: "x"},
	)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.StorageClass{{Name: "fast"}, {Name: "legacy", Default: true}, {Name: "standard", Default: true}}
	if !slices.Equal(snap.Kubernetes.StorageClasses, want) || len(snap.Truncated) != 0 {
		t.Fatalf("classes %+v truncated %v", snap.Kubernetes.StorageClasses, snap.Truncated)
	}
}

// A rollout that times out names each planned claim the cluster has not bound, by its phase: a
// Pending claim (no provisioner, no default class) is why the pod never scheduled. A Bound claim
// adds nothing.
func TestDeployRolloutTimeoutNamesAnUnboundClaim(t *testing.T) {
	for phase, want := range map[corev1.PersistentVolumeClaimPhase]string{
		corev1.ClaimPending: "progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable,claim=Pending",
		corev1.ClaimLost:    "progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable,claim=Lost",
		corev1.ClaimBound:   "progressing=ReplicaSetUpdated,available=MinimumReplicasUnavailable",
	} {
		t.Run(string(phase), func(t *testing.T) {
			c, cs := deployCluster(t, false, false)
			cs.PrependReactor("create", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
				action.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim).Status.Phase = phase
				return false, nil, nil
			})
			res := c.Deploy(context.Background(), deployClaimRequest(300*time.Millisecond), func() {})
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			if res.Outcome != protocol.OutcomeTimedOut || i < 0 || res.Validate() != nil {
				t.Fatalf("result %+v %v", res, res.Validate())
			}
			if s := res.Steps[i]; s.Service != "web" || s.Step != protocol.StepStart || s.Code != "rollout_timeout" || s.Detail != want {
				t.Fatalf("step %+v", s)
			}
		})
	}
}

// A claim read the API server answers with an error other than NotFound fails the precondition
// with that status; it is never mistaken for another owner's claim, and nothing is written.
func TestDeployClaimReadErrorIsNotNameTaken(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	cs.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd leader changed"))
	})
	res := c.Deploy(context.Background(), deployClaimRequest(time.Minute), func() {})
	i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
	if res.Outcome != protocol.OutcomeFailed || i < 0 || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	if s := res.Steps[i]; s.Service != "web" || s.Step != protocol.StepPrecondition || s.Code != "runtime_status" || s.Detail != "500" {
		t.Fatalf("step %+v", s)
	}
	if len(writes(cs)) != 0 {
		t.Fatalf("wrote %v", writes(cs))
	}
}

// Past MaxDeploymentVolumes labelled claims a removal reports exactly the cap, logs that the rest
// were left off, and still succeeds: every claim is still in the cluster.
func TestRemoveReportsAtMostTheClaimCap(t *testing.T) {
	objects := []runtime.Object{}
	for i := range protocol.MaxDeploymentVolumes + 1 {
		m := owned("web")
		m.Name = fmt.Sprintf("shop-data-%02d", i)
		objects = append(objects, &corev1.PersistentVolumeClaim{ObjectMeta: m})
	}
	c, _ := deployCluster(t, true, false, objects...)
	var logged bytes.Buffer
	c.log = log.New(&logged, "", 0)
	res := c.Remove(context.Background(), removalRequest("web"), func() {})
	kept := 0
	for _, s := range res.Steps {
		if s.Step == protocol.StepVolume && s.Detail == protocol.DetailRetained {
			kept++
		}
	}
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil || kept != protocol.MaxDeploymentVolumes {
		t.Fatalf("result %+v kept %d %v", res.Outcome, kept, res.Validate())
	}
	if !strings.Contains(logged.String(), "the rest are not reported") {
		t.Fatalf("log %q", logged.String())
	}
}

// A claim list the API server refuses fails the removal's precondition with its status, before
// anything is deleted.
func TestRemoveClaimListFailureDeletesNothing(t *testing.T) {
	web := &appsv1.Deployment{ObjectMeta: owned("web")}
	web.Name = "shop-web"
	c, cs := deployCluster(t, true, false, web)
	cs.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd leader changed"))
	})
	res := c.Remove(context.Background(), removalRequest("web"), func() {})
	i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
	if res.Outcome != protocol.OutcomeFailed || i < 0 || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	if s := res.Steps[i]; s.Step != protocol.StepPrecondition || s.Code != "runtime_status" || s.Detail != "500" {
		t.Fatalf("step %+v", s)
	}
	if len(writes(cs)) != 0 {
		t.Fatalf("deleted %v", writes(cs))
	}
}

// A StorageClass list the ServiceAccount may not read (a manifest older than migrations) is
// reported empty and named storage_classes in Truncated; the rest of the snapshot is whole.
func TestSnapshotSurvivesAForbiddenStorageClassList(t *testing.T) {
	c, cs := cluster(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}})
	cs.PrependReactor("list", "storageclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, "", nil)
	})
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	k := snap.Kubernetes
	if k.StorageClasses == nil || len(k.StorageClasses) != 0 || !slices.Equal(snap.Truncated, []string{"storage_classes"}) {
		t.Fatalf("classes %v truncated %v", k.StorageClasses, snap.Truncated)
	}
	if !slices.Equal(k.Namespaces, []string{"shop"}) || snap.Engine.Version != "v1.36.0" {
		t.Fatalf("the rest of the snapshot went with the forbidden list: %v %+v", k.Namespaces, snap.Engine)
	}
}
