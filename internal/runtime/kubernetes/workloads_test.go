package kubernetes

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testApplyID = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	secretValue = "never-leaves-the-cluster"
)

// webDeployment is an unmanaged Deployment whose template carries what an apply must leave alone:
// a volume, probes, security contexts, an init container, a Secret env with optional set, envFrom
// and an extended resource.
func webDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web", UID: testUID, ResourceVersion: "41", Generation: 3,
			Labels: map[string]string{"team": "shop"}, Annotations: map[string]string{"deployment.kubernetes.io/revision": "3"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(2)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}, Annotations: map[string]string{"prometheus.io/scrape": "true"}},
				Spec: corev1.PodSpec{
					Volumes:         []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr(true)},
					InitContainers:  []corev1.Container{{Name: "migrate", Image: "ghcr.io/org/web:1", Command: []string{"/migrate"}}},
					Containers: []corev1.Container{{
						Name:  "web",
						Image: "ghcr.io/org/web:1",
						Args:  []string{"--port", "80"},
						Env: []corev1.EnvVar{
							{Name: "MODE", Value: "prod"},
							{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "web-secret"}, Key: "token", Optional: ptr(true)}}},
							{Name: "LEVEL", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "web-env"}, Key: "level"}}},
						},
						EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "web-secret"}}}, {ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "web-env"}}}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("64Mi"), "nvidia.com/gpu": resource.MustParse("1")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi"), "nvidia.com/gpu": resource.MustParse("1")},
						},
						LivenessProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz"}}, PeriodSeconds: 7},
						SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr(true)},
						VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
					}},
				},
			},
		},
	}
}

func dbStatefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db", UID: testUID, ResourceVersion: "7"},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr(int32(1)), UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "postgres:17"}}}}},
	}
}

func agentDaemonSet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "logs", UID: testUID, ResourceVersion: "9"},
		Spec:       appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fluent", Image: "fluent/fluent-bit:3"}}}}},
	}
}

func shopPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-0", UID: "11111111-2222-4333-8444-000000000001"}}
}

// workloadCluster is a fake API server whose access review grants the workload Role in shop
// (unless denied) and whose Deployments report every written generation rolled out when rolls
// is true.
func workloadCluster(t *testing.T, denied, rolls bool, objects ...runtime.Object) (*Client, *fake.Clientset) {
	t.Helper()
	c, cs := cluster(t, append([]runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop", Labels: map[string]string{podSecurityEnforce: "baseline"}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-secret"}, StringData: map[string]string{"token": secretValue}},
	}, objects...)...)
	c.poll = 10 * time.Millisecond
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = !denied && review.Spec.ResourceAttributes.Namespace == "shop"
		return true, review, nil
	})
	// The API server's strategic merge patch: a stale metadata.resourceVersion is a 409, and a
	// Deployment's written generation rolls out when rolls is true.
	cs.PrependReactor("patch", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		p := action.(k8stesting.PatchAction)
		if p.GetPatchType() != types.StrategicMergePatchType {
			return false, nil, nil
		}
		gvr := appsv1.SchemeGroupVersion.WithResource("deployments")
		stored, err := cs.Tracker().Get(gvr, p.GetNamespace(), p.GetName())
		if err != nil {
			return true, nil, err
		}
		var sent struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(p.GetPatch(), &sent); err != nil {
			return true, nil, err
		}
		if sent.Metadata.ResourceVersion != "" && sent.Metadata.ResourceVersion != stored.(*appsv1.Deployment).ResourceVersion {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, p.GetName(), nil)
		}
		old, _ := json.Marshal(stored)
		merged, err := strategicpatch.StrategicMergePatch(old, p.GetPatch(), &appsv1.Deployment{})
		if err != nil {
			return true, nil, err
		}
		d := &appsv1.Deployment{}
		if err := json.Unmarshal(merged, d); err != nil {
			return true, nil, err
		}
		d.Generation++
		if rolls {
			n := replicas(d.Spec.Replicas)
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: n, UpdatedReplicas: n, ReadyReplicas: n, AvailableReplicas: n}
		}
		return true, d, cs.Tracker().Update(gvr, d, p.GetNamespace())
	})
	return c, cs
}

func command(action, reference string) protocol.Command {
	return protocol.Command{ID: "01J00000000000000000000000", Action: action, Reference: reference, Deadline: time.Now().Add(time.Minute)}
}

// mutations lists the verbs that changed the cluster, the access review aside.
func mutations(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" && a.GetResource().Resource != "selfsubjectaccessreviews" {
			out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	return out
}

// reviews lists each access review as verb resource.
func reviews(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "selfsubjectaccessreviews" {
			attrs := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).Spec.ResourceAttributes
			out = append(out, attrs.Verb+" "+attrs.Group+"/"+attrs.Resource)
		}
	}
	return out
}

func TestOperateRestartsEachKind(t *testing.T) {
	for _, tc := range []struct {
		ref      string
		resource string
		template func(*fake.Clientset) corev1.PodTemplateSpec
	}{
		{"shop/deployment/web", "deployments", func(cs *fake.Clientset) corev1.PodTemplateSpec {
			d, _ := cs.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
			return d.Spec.Template
		}},
		{"shop/statefulset/db", "statefulsets", func(cs *fake.Clientset) corev1.PodTemplateSpec {
			s, _ := cs.AppsV1().StatefulSets("shop").Get(context.Background(), "db", metav1.GetOptions{})
			return s.Spec.Template
		}},
		{"shop/daemonset/logs", "daemonsets", func(cs *fake.Clientset) corev1.PodTemplateSpec {
			s, _ := cs.AppsV1().DaemonSets("shop").Get(context.Background(), "logs", metav1.GetOptions{})
			return s.Spec.Template
		}},
	} {
		c, cs := workloadCluster(t, false, true, webDeployment(), dbStatefulSet(), agentDaemonSet())
		outcome, detail := c.Operate(context.Background(), command(protocol.ActionWorkloadRestart, tc.ref))
		if outcome != protocol.OutcomeSucceeded || detail != "" {
			t.Fatalf("%s: %s %q", tc.ref, outcome, detail)
		}
		if got := mutations(cs); !reflect.DeepEqual(got, []string{"patch " + tc.resource}) {
			t.Fatalf("%s: writes %v", tc.ref, got)
		}
		if got := reviews(cs); !reflect.DeepEqual(got, []string{"patch apps/" + tc.resource}) {
			t.Fatalf("%s: reviews %v", tc.ref, got)
		}
		stamp := tc.template(cs).Annotations[restartedAt]
		if at, err := time.Parse(time.RFC3339, stamp); err != nil || time.Since(at) > time.Minute {
			t.Fatalf("%s: restart annotation %q", tc.ref, stamp)
		}
	}
}

func TestOperateScales(t *testing.T) {
	c, cs := workloadCluster(t, false, true, webDeployment(), dbStatefulSet())
	for ref, n := range map[string]int32{"shop/deployment/web": 0, "shop/statefulset/db": 3} {
		cmd := command(protocol.ActionWorkloadScale, ref)
		cmd.Expects.Replicas = ptr(n)
		if outcome, detail := c.Operate(context.Background(), cmd); outcome != protocol.OutcomeSucceeded {
			t.Fatalf("%s: %s %q", ref, outcome, detail)
		}
	}
	d, _ := cs.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	s, _ := cs.AppsV1().StatefulSets("shop").Get(context.Background(), "db", metav1.GetOptions{})
	if *d.Spec.Replicas != 0 || *s.Spec.Replicas != 3 {
		t.Fatalf("replicas %d %d", *d.Spec.Replicas, *s.Spec.Replicas)
	}
}

// A DaemonSet has no replica count, and a count outside 0..MaxWorkloadReplicas (or none) is
// refused before any call.
func TestOperateScaleRefusals(t *testing.T) {
	for _, tc := range []struct {
		ref      string
		replicas *int32
		detail   string
	}{
		{"shop/daemonset/logs", ptr(int32(2)), "unsupported"},
		{"shop/pod/web-0", ptr(int32(2)), "unsupported"},
		{"shop/deployment/web", nil, "replicas_out_of_range"},
		{"shop/deployment/web", ptr(int32(-1)), "replicas_out_of_range"},
		{"shop/deployment/web", ptr(int32(protocol.MaxWorkloadReplicas + 1)), "replicas_out_of_range"},
	} {
		c, cs := workloadCluster(t, false, true, webDeployment(), agentDaemonSet(), shopPod())
		cmd := command(protocol.ActionWorkloadScale, tc.ref)
		cmd.Expects.Replicas = tc.replicas
		outcome, detail := c.Operate(context.Background(), cmd)
		if outcome != protocol.OutcomeDenied || detail != tc.detail || len(cs.Actions()) != 0 {
			t.Fatalf("%s %v: %s %q, calls %v", tc.ref, tc.replicas, outcome, detail, cs.Actions())
		}
	}
	c, _ := workloadCluster(t, false, true, webDeployment())
	cmd := command(protocol.ActionWorkloadScale, "shop/deployment/web")
	cmd.Expects.Replicas = ptr(int32(protocol.MaxWorkloadReplicas))
	if outcome, _ := c.Operate(context.Background(), cmd); outcome != protocol.OutcomeSucceeded {
		t.Fatalf("the bound itself: %s", outcome)
	}
}

// Delete reads the object fresh and deletes that one only: its UID as a precondition, Background
// propagation. Pods the same.
func TestOperateDeletesAtTheReadUID(t *testing.T) {
	for _, tc := range []struct{ action, ref, resource, uid string }{
		{protocol.ActionWorkloadDelete, "shop/deployment/web", "deployments", testUID},
		{protocol.ActionWorkloadDelete, "shop/daemonset/logs", "daemonsets", testUID},
		{protocol.ActionPodDelete, "shop/pod/web-0", "pods", string(shopPod().UID)},
	} {
		c, cs := workloadCluster(t, false, true, webDeployment(), agentDaemonSet(), shopPod())
		outcome, detail := c.Operate(context.Background(), command(tc.action, tc.ref))
		if outcome != protocol.OutcomeSucceeded {
			t.Fatalf("%s: %s %q", tc.ref, outcome, detail)
		}
		var opts *metav1.DeleteOptions
		for _, a := range cs.Actions() {
			if a.GetVerb() == "delete" {
				o := a.(k8stesting.DeleteAction).GetDeleteOptions()
				opts = &o
			}
		}
		if opts == nil || opts.Preconditions == nil || opts.Preconditions.UID == nil || string(*opts.Preconditions.UID) != tc.uid || opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationBackground {
			t.Fatalf("%s: delete options %+v", tc.ref, opts)
		}
		if got := reviews(cs); len(got) != 1 || !strings.HasPrefix(got[0], "delete ") || !strings.HasSuffix(got[0], "/"+tc.resource) {
			t.Fatalf("%s: reviews %v", tc.ref, got)
		}
	}
}

// An object recreated under the name between the read and the delete is not removed: the UID
// precondition's conflict is a refusal.
func TestOperateDeleteOfAReplacedObject(t *testing.T) {
	c, cs := workloadCluster(t, false, true, webDeployment())
	cs.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", nil)
	})
	if outcome, detail := c.Operate(context.Background(), command(protocol.ActionWorkloadDelete, "shop/deployment/web")); outcome != protocol.OutcomeDenied || detail != "conflict" {
		t.Fatalf("%s %q", outcome, detail)
	}
	if outcome, detail := c.Operate(context.Background(), command(protocol.ActionWorkloadDelete, "shop/deployment/gone")); outcome != protocol.OutcomeDenied || detail != "not_found" {
		t.Fatalf("missing: %s %q", outcome, detail)
	}
}

// An agent on an older manifest is refused by the access review for every action and writes
// nothing.
func TestOperateForbidden(t *testing.T) {
	for _, cmd := range []protocol.Command{
		command(protocol.ActionWorkloadRestart, "shop/deployment/web"),
		command(protocol.ActionWorkloadDelete, "shop/deployment/web"),
		command(protocol.ActionPodDelete, "shop/pod/web-0"),
		func() protocol.Command {
			c := command(protocol.ActionWorkloadScale, "shop/deployment/web")
			c.Expects.Replicas = ptr(int32(1))
			return c
		}(),
	} {
		c, cs := workloadCluster(t, true, true, webDeployment(), shopPod())
		outcome, detail := c.Operate(context.Background(), cmd)
		if outcome != protocol.OutcomeDenied || detail != "forbidden" || len(mutations(cs)) != 0 {
			t.Fatalf("%s: %s %q %v", cmd.Action, outcome, detail, mutations(cs))
		}
	}
}

// A Deployment KyYard manages is removed through its application, never by a workload delete;
// restart and scale stay allowed.
func TestOperateRefusesDeletingAManagedWorkload(t *testing.T) {
	managed := webDeployment()
	managed.Labels[render.LabelManagedBy] = render.ManagedBy
	c, cs := workloadCluster(t, false, true, managed)
	if outcome, detail := c.Operate(context.Background(), command(protocol.ActionWorkloadDelete, "shop/deployment/web")); outcome != protocol.OutcomeDenied || detail != "application_managed" || len(mutations(cs)) != 0 {
		t.Fatalf("%s %q %v", outcome, detail, mutations(cs))
	}
	if outcome, _ := c.Operate(context.Background(), command(protocol.ActionWorkloadRestart, "shop/deployment/web")); outcome != protocol.OutcomeSucceeded {
		t.Fatalf("restart: %s", outcome)
	}
}

func TestOperateRefusesWhatItDoesNotRun(t *testing.T) {
	c, cs := workloadCluster(t, false, true, webDeployment(), shopPod())
	for _, tc := range []struct{ cmd, detail string }{
		{protocol.ActionStart + " shop/deployment/web", "unsupported action"},
		{protocol.ActionWorkloadRestart + " shop/pod/web-0", "unsupported"},
		{protocol.ActionPodDelete + " shop/deployment/web", "unsupported"},
		{protocol.ActionWorkloadRestart + " shop/Deployment/web", "invalid_reference"},
		{protocol.ActionWorkloadRestart + " ../deployment/web", "invalid_reference"},
	} {
		action, ref, _ := strings.Cut(tc.cmd, " ")
		if outcome, detail := c.Operate(context.Background(), command(action, ref)); outcome != protocol.OutcomeDenied || detail != tc.detail {
			t.Fatalf("%s: %s %q", tc.cmd, outcome, detail)
		}
	}
	if len(cs.Actions()) != 0 {
		t.Fatalf("calls %v", cs.Actions())
	}
}

func workloadTarget(ref string) protocol.InspectionTarget {
	r, err := protocol.ParseWorkloadRef(ref)
	if err != nil {
		panic(err)
	}
	return protocol.InspectionTarget{Workload: r}
}

// The read maps the pod template, names Secret and ConfigMap env by reference and never reads a
// Secret: its value is nowhere in the answer.
func TestReadWorkloadMapsTheTemplate(t *testing.T) {
	c, cs := workloadCluster(t, false, true, webDeployment())
	target := workloadTarget("shop/deployment/web")
	got, err := c.ReadWorkload(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(target.Workload, time.Now()); err != nil {
		t.Fatalf("the read does not validate: %v", err)
	}
	want := &protocol.WorkloadConfiguration{
		Target: target.Workload, ObservedAt: got.ObservedAt, ResourceVersion: "41", Replicas: ptr(int32(2)), Strategy: "RollingUpdate",
		Containers: []protocol.WorkloadContainer{{Name: "web", Image: "ghcr.io/org/web:1", Command: []string{}, Args: []string{"--port", "80"},
			Env:       []protocol.WorkloadEnv{{Name: "MODE", Value: "prod"}, {Name: "TOKEN", SecretRef: "web-secret/token"}, {Name: "LEVEL", ConfigMapRef: "web-env/level"}},
			Resources: protocol.WorkloadResources{CPURequest: "250m", MemoryRequest: "64Mi", MemoryLimit: "128Mi"}}},
		InitContainers: []protocol.WorkloadContainer{{Name: "migrate", Image: "ghcr.io/org/web:1", Command: []string{"/migrate"}, Args: []string{}, Env: []protocol.WorkloadEnv{}}},
		EnvFrom:        []string{"secret/web-secret", "configmap/web-env"},
		Unsupported:    []string{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read\n got %+v\nwant %+v", got, want)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), secretValue) {
		t.Fatal("the Secret's value is in the read")
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "secrets" {
			t.Fatalf("the read touched Secrets: %v", a)
		}
	}
}

func TestReadWorkloadKindsAndFlags(t *testing.T) {
	managed := webDeployment()
	managed.Labels = map[string]string{render.LabelManagedBy: render.ManagedBy}
	managed.Spec.Template.Spec.Containers[0].Env = append(managed.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}})
	managed.Spec.Template.Spec.Containers[0].Command = make([]string, protocol.MaxArgv+1)
	c, _ := workloadCluster(t, false, true, managed, dbStatefulSet(), agentDaemonSet())

	d, err := c.ReadWorkload(context.Background(), workloadTarget("shop/deployment/web"))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Managed || !reflect.DeepEqual(d.Unsupported, []string{"argv_truncated", "env_field_ref"}) || len(d.Containers[0].Env) != 3 || len(d.Containers[0].Command) != protocol.MaxArgv {
		t.Fatalf("managed %v unsupported %v env %v argv %d", d.Managed, d.Unsupported, d.Containers[0].Env, len(d.Containers[0].Command))
	}
	s, err := c.ReadWorkload(context.Background(), workloadTarget("shop/statefulset/db"))
	if err != nil || s.Replicas == nil || *s.Replicas != 1 || s.Strategy != "OnDelete" || s.ResourceVersion != "7" {
		t.Fatalf("statefulset %+v %v", s, err)
	}
	ds, err := c.ReadWorkload(context.Background(), workloadTarget("shop/daemonset/logs"))
	if err != nil || ds.Replicas != nil || ds.Containers[0].Image != "fluent/fluent-bit:3" {
		t.Fatalf("daemonset %+v %v", ds, err)
	}
	for _, target := range []protocol.InspectionTarget{workloadTarget("shop/pod/web-0"), {Workload: protocol.WorkloadRef{Namespace: "shop", Name: "web", UID: testUID}}} {
		if _, err := c.ReadWorkload(context.Background(), target); err == nil {
			t.Fatalf("read %+v", target)
		}
	}
}

// applyFrom is an apply of the read, changed by edit.
func applyFrom(t *testing.T, c *Client, ref string, deadline time.Duration, edit func(*protocol.WorkloadConfiguration)) protocol.WorkloadApply {
	t.Helper()
	target := workloadTarget(ref)
	cfg, err := c.ReadWorkload(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ObservedAt, cfg.Managed, cfg.Unsupported = time.Time{}, false, nil
	edit(cfg)
	now := time.Now()
	return protocol.WorkloadApply{Request: testApplyID, Endpoint: "ep_1", IssuedAt: now, Deadline: now.Add(deadline), Target: target.Workload, ResourceVersion: cfg.ResourceVersion, Spec: *cfg}
}

func steps(res protocol.DeploymentResult) string {
	var out []string
	for _, s := range res.Steps {
		out = append(out, strings.TrimRight(s.Step+" "+s.Outcome+" "+s.Code+" "+s.Detail, " "))
	}
	return strings.Join(out, "; ")
}

// An apply changes only what the form carries and leaves the rest of the object as it was:
// volumes, probes, security contexts, the Secret env's optional flag, envFrom, extended resources.
func TestApplyWorkloadChangesOnlyTheFormsFields(t *testing.T) {
	c, cs := workloadCluster(t, false, true, webDeployment())
	req := applyFrom(t, c, "shop/deployment/web", time.Minute, func(w *protocol.WorkloadConfiguration) {
		w.Replicas = ptr(int32(3))
		w.Containers[0].Image = "ghcr.io/org/web:2"
		w.Containers[0].Env[0].Value = "dev"
		w.Containers[0].Env = append(w.Containers[0].Env, protocol.WorkloadEnv{Name: "API_KEY", SecretRef: "api/key"})
		w.Containers[0].Resources.CPURequest = "500m"
		w.Containers[0].Resources.MemoryLimit = ""
	})
	started := 0
	res := c.ApplyWorkload(context.Background(), req, func() { started++ })
	if res.Outcome != protocol.OutcomeSucceeded || steps(res) != "precondition succeeded; apply succeeded; rollout succeeded" || started != 1 {
		t.Fatalf("%s %s: %s, started %d", res.Outcome, res.Code, steps(res), started)
	}
	if err := res.Validate(); err != nil || res.Deployment != testApplyID || res.Steps[0].Service != protocol.WorkloadApplyService {
		t.Fatalf("result %+v: %v", res, err)
	}
	after, _ := cs.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	before := webDeployment()
	b, a := before.Spec.Template.Spec, after.Spec.Template.Spec
	for name, pair := range map[string][2]any{
		"volumes": {b.Volumes, a.Volumes}, "pod security": {b.SecurityContext, a.SecurityContext}, "init": {b.InitContainers, a.InitContainers},
		"probe": {b.Containers[0].LivenessProbe, a.Containers[0].LivenessProbe}, "security": {b.Containers[0].SecurityContext, a.Containers[0].SecurityContext},
		"mounts": {b.Containers[0].VolumeMounts, a.Containers[0].VolumeMounts}, "envFrom": {b.Containers[0].EnvFrom, a.Containers[0].EnvFrom},
		"args": {b.Containers[0].Args, a.Containers[0].Args}, "command": {b.Containers[0].Command, a.Containers[0].Command},
		"selector": {before.Spec.Selector, after.Spec.Selector}, "strategy": {before.Spec.Strategy, after.Spec.Strategy},
		"labels": {before.Labels, after.Labels}, "annotations": {before.Annotations, after.Annotations},
		"template labels": {before.Spec.Template.Labels, after.Spec.Template.Labels}, "template annotations": {before.Spec.Template.Annotations, after.Spec.Template.Annotations},
		"secret env": {b.Containers[0].Env[1], a.Containers[0].Env[1]}, "configmap env": {b.Containers[0].Env[2], a.Containers[0].Env[2]},
	} {
		x, _ := json.Marshal(pair[0])
		y, _ := json.Marshal(pair[1])
		if string(x) != string(y) {
			t.Fatalf("%s changed:\n%s\n%s", name, x, y)
		}
	}
	w := a.Containers[0]
	if *after.Spec.Replicas != 3 || w.Image != "ghcr.io/org/web:2" || w.Env[0].Value != "dev" || w.Env[3].ValueFrom.SecretKeyRef.Name != "api" || w.Env[3].ValueFrom.SecretKeyRef.Key != "key" {
		t.Fatalf("not applied: replicas %d %+v", *after.Spec.Replicas, w)
	}
	cpu, gpu := w.Resources.Requests[corev1.ResourceCPU], w.Resources.Limits["nvidia.com/gpu"]
	if _, ok := w.Resources.Limits[corev1.ResourceMemory]; ok || cpu.String() != "500m" || gpu.String() != "1" {
		t.Fatalf("resources %+v", w.Resources)
	}
	if got := mutations(cs); !reflect.DeepEqual(got, []string{"patch deployments"}) {
		t.Fatalf("writes %v", got)
	}
}

// patches returns the strategic merge patches sent, decoded.
func patches(t *testing.T, cs *fake.Clientset) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, a := range cs.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok && p.GetPatchType() == types.StrategicMergePatchType {
			var m map[string]any
			if err := json.Unmarshal(p.GetPatch(), &m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
	}
	return out
}

// The write is a strategic merge patch holding only the edited paths and the read's
// resourceVersion; applied to the stored object's raw JSON it keeps fields the bundled client
// types do not know, and the object's labels and annotations.
func TestApplyWorkloadPatchesOnlyTheEdit(t *testing.T) {
	c, cs := workloadCluster(t, false, true, webDeployment())
	req := applyFrom(t, c, "shop/deployment/web", time.Minute, func(w *protocol.WorkloadConfiguration) { w.Containers[0].Image = "ghcr.io/org/web:2" })
	if res := c.ApplyWorkload(context.Background(), req, func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("%s", steps(res))
	}
	sent := patches(t, cs)
	want := map[string]any{
		"metadata": map[string]any{"resourceVersion": "41"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"$setElementOrder/containers": []any{map[string]any{"name": "web"}},
			"containers":                  []any{map[string]any{"name": "web", "image": "ghcr.io/org/web:2"}},
		}}},
	}
	if len(sent) != 1 || !reflect.DeepEqual(sent[0], want) {
		t.Fatalf("patch %v", sent)
	}

	live := webDeployment()
	live.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
	raw, _ := json.Marshal(live)
	stored := &unstructured.Unstructured{}
	if err := stored.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(unstructured.SetNestedField(stored.Object, "kept", "spec", "futureField"))
	containers, _, _ := unstructured.NestedSlice(stored.Object, "spec", "template", "spec", "containers")
	containers[0].(map[string]any)["futureField"] = "kept too"
	must(unstructured.SetNestedSlice(stored.Object, containers, "spec", "template", "spec", "containers"))
	before, _ := stored.MarshalJSON()
	body, _ := json.Marshal(sent[0])
	merged, err := strategicpatch.StrategicMergePatch(before, body, &appsv1.Deployment{})
	must(err)
	after := &unstructured.Unstructured{}
	must(after.UnmarshalJSON(merged))
	containers, _, _ = unstructured.NestedSlice(after.Object, "spec", "template", "spec", "containers")
	future, _, _ := unstructured.NestedString(after.Object, "spec", "futureField")
	if future != "kept" || containers[0].(map[string]any)["futureField"] != "kept too" || containers[0].(map[string]any)["image"] != "ghcr.io/org/web:2" {
		t.Fatalf("after the patch: %s", merged)
	}
	if !reflect.DeepEqual(after.GetLabels(), webDeployment().Labels) || !reflect.DeepEqual(after.GetAnnotations(), webDeployment().Annotations) {
		t.Fatalf("metadata after the patch: %v %v", after.GetLabels(), after.GetAnnotations())
	}
}

// An unchanged apply writes the object as it was read.
func TestApplyWorkloadUnchangedIsIdentical(t *testing.T) {
	c, cs := workloadCluster(t, false, true, webDeployment())
	res := c.ApplyWorkload(context.Background(), applyFrom(t, c, "shop/deployment/web", time.Minute, func(*protocol.WorkloadConfiguration) {}), func() {})
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("%s", steps(res))
	}
	after, _ := cs.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	x, _ := json.Marshal(webDeployment().Spec)
	y, _ := json.Marshal(after.Spec)
	if string(x) != string(y) {
		t.Fatalf("spec changed:\n%s\n%s", x, y)
	}
}

// A concurrent edit is refused, never overwritten: a resourceVersion other than the read's, or the
// API server's own 409 on the patch.
func TestApplyWorkloadConflict(t *testing.T) {
	c, cs := workloadCluster(t, false, true, webDeployment())
	req := applyFrom(t, c, "shop/deployment/web", time.Minute, func(w *protocol.WorkloadConfiguration) { w.Containers[0].Image = "ghcr.io/org/web:2" })
	req.ResourceVersion, req.Spec.ResourceVersion = "40", "40"
	res := c.ApplyWorkload(context.Background(), req, func() { t.Fatal("started before a refusal") })
	if res.Outcome != protocol.OutcomeDenied || steps(res) != "precondition denied conflict; apply skipped; rollout skipped" || len(mutations(cs)) != 0 {
		t.Fatalf("version mismatch: %s %v", steps(res), mutations(cs))
	}

	c, cs = workloadCluster(t, false, true, webDeployment())
	cs.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", nil)
	})
	res = c.ApplyWorkload(context.Background(), applyFrom(t, c, "shop/deployment/web", time.Minute, func(w *protocol.WorkloadConfiguration) { w.Containers[0].Image = "ghcr.io/org/web:2" }), func() {})
	if res.Outcome != protocol.OutcomeDenied || steps(res) != "precondition succeeded; apply denied conflict; rollout skipped" {
		t.Fatalf("API 409: %s", steps(res))
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyWorkloadRefusals(t *testing.T) {
	managed := webDeployment()
	managed.Labels = map[string]string{render.LabelManagedBy: render.ManagedBy}
	fieldRef := webDeployment()
	fieldRef.Spec.Template.Spec.Containers[0].Env = append(fieldRef.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}})
	for _, tc := range []struct {
		name   string
		denied bool
		object *appsv1.Deployment
		edit   func(*protocol.WorkloadConfiguration)
		want   string
	}{
		{"managed", false, managed, func(*protocol.WorkloadConfiguration) {}, "precondition denied application_managed"},
		{"forbidden", true, webDeployment(), func(*protocol.WorkloadConfiguration) {}, "precondition denied forbidden"},
		{"field ref", false, fieldRef, func(*protocol.WorkloadConfiguration) {}, "precondition denied configuration_unreported"},
		{"unknown container", false, webDeployment(), func(w *protocol.WorkloadConfiguration) { w.Containers[0].Name = "other" }, "precondition denied conflict"},
	} {
		c, cs := workloadCluster(t, false, true, tc.object)
		req := applyFrom(t, c, "shop/deployment/web", time.Minute, tc.edit)
		if tc.denied {
			c, cs = workloadCluster(t, true, true, tc.object)
		}
		res := c.ApplyWorkload(context.Background(), req, func() { t.Fatalf("%s: started", tc.name) })
		if res.Outcome != protocol.OutcomeDenied || steps(res) != tc.want+"; apply skipped; rollout skipped" || len(mutations(cs)) != 0 {
			t.Fatalf("%s: %s %v", tc.name, steps(res), mutations(cs))
		}
		if err := res.Validate(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

// An apply needs the namespace to enforce Pod Security baseline or restricted, as a deploy does:
// editing a privileged workload is host access.
func TestApplyWorkloadRefusesWithoutPodSecurity(t *testing.T) {
	for level, detail := range map[string]string{"": "missing", "privileged": "privileged", "loose": "invalid"} {
		c, cs := workloadCluster(t, false, true, webDeployment())
		req := applyFrom(t, c, "shop/deployment/web", time.Minute, func(w *protocol.WorkloadConfiguration) { w.Containers[0].Image = "ghcr.io/org/web:2" })
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
		if level != "" {
			ns.Labels = map[string]string{podSecurityEnforce: level}
		}
		if _, err := cs.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		res := c.ApplyWorkload(context.Background(), req, func() { t.Fatalf("%q: started", level) })
		if res.Outcome != protocol.OutcomeDenied || steps(res) != "precondition denied pod_security "+detail+"; apply skipped; rollout skipped" || len(mutations(cs)) != 1 {
			t.Fatalf("%q: %s %v", level, steps(res), mutations(cs))
		}
	}
	c, _ := workloadCluster(t, false, true, webDeployment())
	res := c.ApplyWorkload(context.Background(), applyFrom(t, c, "shop/deployment/web", time.Minute, func(*protocol.WorkloadConfiguration) {}), func() {})
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("baseline: %s", steps(res))
	}
}

// Replicas are bounded 0..MaxWorkloadReplicas: past it the frame is refused before any call.
func TestApplyWorkloadReplicasBound(t *testing.T) {
	c, cs := workloadCluster(t, false, true, webDeployment())
	req := applyFrom(t, c, "shop/deployment/web", time.Minute, func(w *protocol.WorkloadConfiguration) { w.Replicas = ptr(int32(protocol.MaxWorkloadReplicas + 1)) })
	calls := len(cs.Actions())
	res := c.ApplyWorkload(context.Background(), req, func() { t.Fatal("started") })
	if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || len(cs.Actions()) != calls {
		t.Fatalf("%s %s", res.Outcome, res.Code)
	}
}

func TestApplyWorkloadRollout(t *testing.T) {
	// A Deployment that never becomes available times out at the frame's deadline.
	c, _ := workloadCluster(t, false, false, webDeployment())
	res := c.ApplyWorkload(context.Background(), applyFrom(t, c, "shop/deployment/web", 300*time.Millisecond, func(w *protocol.WorkloadConfiguration) { w.Containers[0].Image = "ghcr.io/org/web:2" }), func() {})
	if res.Outcome != protocol.OutcomeTimedOut || !strings.HasPrefix(steps(res), "precondition succeeded; apply succeeded; rollout timed_out rollout_timeout") {
		t.Fatalf("%s", steps(res))
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
	// A StatefulSet's rollout is not waited for; a paused Deployment does not roll.
	c, cs := workloadCluster(t, false, false, dbStatefulSet(), webDeployment())
	res = c.ApplyWorkload(context.Background(), applyFrom(t, c, "shop/statefulset/db", time.Minute, func(w *protocol.WorkloadConfiguration) { w.Containers[0].Image = "postgres:18" }), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || steps(res) != "precondition succeeded; apply succeeded; rollout skipped" {
		t.Fatalf("statefulset: %s", steps(res))
	}
	if s, _ := cs.AppsV1().StatefulSets("shop").Get(context.Background(), "db", metav1.GetOptions{}); s.Spec.Template.Spec.Containers[0].Image != "postgres:18" {
		t.Fatal("statefulset not updated")
	}
	res = c.ApplyWorkload(context.Background(), applyFrom(t, c, "shop/deployment/web", time.Minute, func(w *protocol.WorkloadConfiguration) { w.Paused = true }), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || steps(res) != "precondition succeeded; apply succeeded; rollout skipped" {
		t.Fatalf("paused: %s", steps(res))
	}
}

func TestPodCarriesItsUID(t *testing.T) {
	if got := pod(*shopPod()).UID; got != string(shopPod().UID) {
		t.Fatalf("uid %q", got)
	}
}

// scaleCluster is workloadCluster whose JSON merge patches answer 409 when the patch carries a
// metadata.resourceVersion other than the stored one, as the API server does.
func scaleCluster(t *testing.T, objects ...runtime.Object) (*Client, *fake.Clientset) {
	t.Helper()
	c, cs := workloadCluster(t, false, true, objects...)
	for _, res := range []string{"deployments", "statefulsets"} {
		cs.PrependReactor("patch", res, func(action k8stesting.Action) (bool, runtime.Object, error) {
			p := action.(k8stesting.PatchAction)
			if p.GetPatchType() != types.MergePatchType {
				return false, nil, nil
			}
			stored, err := cs.Tracker().Get(p.GetResource(), p.GetNamespace(), p.GetName())
			if err != nil {
				return true, nil, err
			}
			var sent struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(p.GetPatch(), &sent); err != nil {
				return true, nil, err
			}
			if sent.Metadata.ResourceVersion != stored.(metav1.Object).GetResourceVersion() {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: res}, p.GetName(), nil)
			}
			return false, nil, nil
		})
	}
	return c, cs
}

func claimRetention(whenScaled appsv1.PersistentVolumeClaimRetentionPolicyType, n int32) *appsv1.StatefulSet {
	s := dbStatefulSet()
	s.Spec.Replicas = ptr(n)
	s.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType, WhenScaled: whenScaled}
	return s
}

// Scaling a StatefulSet whose claims are deleted when scaled down below its current replicas
// would delete the excess replicas' volume claims: refused before any write. Scaling up, or
// down with Retain, goes ahead.
func TestOperateScaleRefusesDeletingClaims(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    *appsv1.StatefulSet
		to     int32
		detail string
	}{
		{"delete, down", claimRetention(appsv1.DeletePersistentVolumeClaimRetentionPolicyType, 3), 1, "pvc_retention"},
		{"delete, to zero", claimRetention(appsv1.DeletePersistentVolumeClaimRetentionPolicyType, 3), 0, "pvc_retention"},
		{"delete, unset replicas is 1", func() *appsv1.StatefulSet {
			s := claimRetention(appsv1.DeletePersistentVolumeClaimRetentionPolicyType, 1)
			s.Spec.Replicas = nil
			return s
		}(), 0, "pvc_retention"},
		{"delete, up", claimRetention(appsv1.DeletePersistentVolumeClaimRetentionPolicyType, 1), 3, ""},
		{"delete, same", claimRetention(appsv1.DeletePersistentVolumeClaimRetentionPolicyType, 2), 2, ""},
		{"retain, down", claimRetention(appsv1.RetainPersistentVolumeClaimRetentionPolicyType, 3), 1, ""},
		{"no policy, down", dbStatefulSet(), 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cs := scaleCluster(t, tc.set)
			cmd := command(protocol.ActionWorkloadScale, "shop/statefulset/db")
			cmd.Expects.Replicas = ptr(tc.to)
			outcome, detail := c.Operate(context.Background(), cmd)
			if tc.detail != "" {
				if outcome != protocol.OutcomeDenied || detail != tc.detail || len(mutations(cs)) != 0 {
					t.Fatalf("%s %q, writes %v", outcome, detail, mutations(cs))
				}
				return
			}
			if outcome != protocol.OutcomeSucceeded {
				t.Fatalf("%s %q", outcome, detail)
			}
			s, _ := cs.AppsV1().StatefulSets("shop").Get(context.Background(), "db", metav1.GetOptions{})
			if *s.Spec.Replicas != tc.to {
				t.Fatalf("replicas %d", *s.Spec.Replicas)
			}
		})
	}
}

// A scale patch carries the resourceVersion of the object it inspected, so a change between the
// read and the patch is the API server's 409, reported conflict.
func TestOperateScaleIsBoundToTheRead(t *testing.T) {
	c, cs := scaleCluster(t, webDeployment())
	cmd := command(protocol.ActionWorkloadScale, "shop/deployment/web")
	cmd.Expects.Replicas = ptr(int32(4))
	if outcome, detail := c.Operate(context.Background(), cmd); outcome != protocol.OutcomeSucceeded {
		t.Fatalf("%s %q", outcome, detail)
	}
	var sent []string
	for _, a := range cs.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok {
			sent = append(sent, string(p.GetPatch()))
		}
	}
	if want := `{"metadata":{"resourceVersion":"41"},"spec":{"replicas":4}}`; len(sent) != 1 || sent[0] != want {
		t.Fatalf("patches %v", sent)
	}

	// Another writer moves the object between the read and the patch.
	c, cs = scaleCluster(t, webDeployment())
	cs.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		d := webDeployment()
		d.ResourceVersion = "40"
		return true, d, nil
	})
	if outcome, detail := c.Operate(context.Background(), cmd); outcome != protocol.OutcomeDenied || detail != "conflict" {
		t.Fatalf("stale read: %s %q", outcome, detail)
	}
}
