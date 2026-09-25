package kubernetes

import (
	"context"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// cluster is a fake API server holding objects, with a fixed server version and a quiet log.
func cluster(t *testing.T, objects ...runtime.Object) (*Client, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(objects...)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: "36", GitVersion: "v1.36.0", Platform: "linux/amd64"}
	c := NewFromClientset(cs)
	c.log = log.New(io.Discard, "", 0)
	return c, cs
}

func meta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name}
}

// Every field the inventory carries is read from the object that holds it, and a Deployment's
// pod names the Deployment through its ReplicaSet's name.
func TestSnapshotMapsTheCluster(t *testing.T) {
	started := metav1.NewTime(time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC))
	controller := true
	three, storage := int32(3), "fast"
	objects := []runtime.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Labels: map[string]string{"node-role.kubernetes.io/worker": "", "kubernetes.io/os": "linux"}},
			Spec:   corev1.NodeSpec{Unschedulable: true},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.36.0", OperatingSystem: "linux", Architecture: "arm64"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "control-1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.36.0", OperatingSystem: "linux", Architecture: "amd64"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&appsv1.Deployment{ObjectMeta: meta("shop", "web"), Spec: appsv1.DeploymentSpec{Replicas: &three, Paused: true, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx:1.29"}, {Name: "log", Image: "busybox:1"}}}}},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 2, UpdatedReplicas: 1}},
		&appsv1.StatefulSet{ObjectMeta: meta("shop", "db"), Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "postgres:17"}}}}},
			Status: appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1}},
		&appsv1.DaemonSet{ObjectMeta: meta("kube-system", "proxy"), Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "proxy", Image: "kube-proxy:1"}}}}},
			Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 1, UpdatedNumberScheduled: 2}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-7c9d-x2", Labels: map[string]string{"pod-template-hash": "7c9d"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-7c9d", Controller: &controller}}},
			Spec: corev1.PodSpec{NodeName: "worker-1", Containers: []corev1.Container{{Name: "web", Image: "nginx:1.29"}, {Name: "log", Image: "busybox:1"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &started, ContainerStatuses: []corev1.ContainerStatus{
				{Name: "web", ImageID: "docker.io/library/nginx@sha256:aa", Ready: true, RestartCount: 4, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "log", ImageID: "docker.io/library/busybox@sha256:bb", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
			}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-0", OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: &controller}}},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "postgres:17"}}},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: "db", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}}},
		&corev1.Pod{ObjectMeta: meta("default", "bare"), Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sh", Image: "alpine:3"}}}, Status: corev1.PodStatus{Phase: corev1.PodPending}},
		&corev1.Service{ObjectMeta: meta("shop", "web"), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, ClusterIP: "10.96.0.10", Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP}, {Port: 53, Protocol: corev1.ProtocolUDP}}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: meta("shop", "data"), Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &storage},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}},
	}
	c, _ := cluster(t, objects...)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.CheckRuntimeShape(protocol.RuntimeKubernetes, snap); err != nil || len(snap.Truncated) != 0 {
		t.Fatalf("shape %v truncated %v", err, snap.Truncated)
	}
	if e := snap.Engine; e.Runtime != "kubernetes" || e.Version != "v1.36.0" || e.APIVersion != "1.36" || e.OS != "linux" || e.Arch != "amd64" {
		t.Fatalf("engine %+v", e)
	}
	k := snap.Kubernetes
	want := []protocol.Node{
		{Name: "control-1", KubeletVersion: "v1.36.0", OS: "linux", Arch: "amd64", Ready: true, Roles: []string{"control-plane"}},
		{Name: "worker-1", KubeletVersion: "v1.36.0", OS: "linux", Arch: "arm64", Ready: false, Roles: []string{"worker"}, Unschedulable: true},
	}
	if fmt.Sprint(k.Nodes) != fmt.Sprint(want) {
		t.Fatalf("nodes %+v", k.Nodes)
	}
	if !slices.Equal(k.Namespaces, []string{"default", "shop"}) {
		t.Fatalf("namespaces %v", k.Namespaces)
	}
	wantWorkloads := []protocol.Workload{
		{Kind: "DaemonSet", Namespace: "kube-system", Name: "proxy", Desired: 2, Ready: 1, Updated: 2, Images: []string{"kube-proxy:1"}},
		{Kind: "StatefulSet", Namespace: "shop", Name: "db", Desired: 1, Ready: 1, Updated: 1, Images: []string{"postgres:17"}},
		{Kind: "Deployment", Namespace: "shop", Name: "web", Desired: 3, Ready: 2, Updated: 1, Images: []string{"nginx:1.29", "busybox:1"}, Paused: true},
	}
	if fmt.Sprint(k.Workloads) != fmt.Sprint(wantWorkloads) {
		t.Fatalf("workloads %+v", k.Workloads)
	}
	if len(k.Pods) != 3 {
		t.Fatalf("pods %+v", k.Pods)
	}
	bare, db, web := k.Pods[0], k.Pods[1], k.Pods[2]
	if bare.Name != "bare" || bare.Phase != "Pending" || bare.OwnerKind != "" || bare.Containers[0].State != "waiting" || !bare.StartedAt.IsZero() {
		t.Fatalf("bare pod %+v", bare)
	}
	if db.OwnerKind != "StatefulSet" || db.OwnerName != "db" || db.Containers[0].State != "terminated" || db.Containers[0].Reason != "Completed" {
		t.Fatalf("stateful pod %+v", db)
	}
	wantWeb := protocol.Pod{Namespace: "shop", Name: "web-7c9d-x2", Phase: "Running", Node: "worker-1", OwnerKind: "Deployment", OwnerName: "web", StartedAt: started.UTC(), Containers: []protocol.PodContainer{
		{Name: "web", Image: "nginx:1.29", ImageID: "docker.io/library/nginx@sha256:aa", State: "running", Ready: true, RestartCount: 4},
		{Name: "log", Image: "busybox:1", ImageID: "docker.io/library/busybox@sha256:bb", State: "waiting", Reason: "CrashLoopBackOff"},
	}}
	if fmt.Sprint(web) != fmt.Sprint(wantWeb) {
		t.Fatalf("deployment pod %+v", web)
	}
	if s := k.Services; len(s) != 1 || s[0].Type != "NodePort" || s[0].ClusterIP != "10.96.0.10" || !slices.Equal(s[0].Ports, []string{"80:30080/TCP", "53/UDP"}) {
		t.Fatalf("services %+v", s)
	}
	if cl := k.Claims; len(cl) != 1 || cl[0] != (protocol.Claim{Namespace: "shop", Name: "data", Phase: "Bound", StorageClass: "fast", Capacity: "10Gi"}) {
		t.Fatalf("claims %+v", cl)
	}
}

// A list over its cap is cut, in namespace/name order, and named; a list the ServiceAccount
// may not read is reported empty and named, and the rest of the cluster is still reported.
func TestSnapshotCutsAndSurvivesAForbiddenList(t *testing.T) {
	var objects []runtime.Object
	for i := range protocol.MaxClaims + 1 {
		objects = append(objects, &corev1.PersistentVolumeClaim{ObjectMeta: meta("shop", fmt.Sprintf("data-%04d", i))})
	}
	objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}, &corev1.Service{ObjectMeta: meta("shop", "web")})
	c, cs := cluster(t, objects...)
	cs.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", nil)
	})
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	k := snap.Kubernetes
	if len(k.Claims) != protocol.MaxClaims || k.Claims[0].Name != "data-0000" || k.Claims[protocol.MaxClaims-1].Name != fmt.Sprintf("data-%04d", protocol.MaxClaims-1) {
		t.Fatalf("claims %d, first %q", len(k.Claims), k.Claims[0].Name)
	}
	if len(k.Services) != 0 || !slices.Equal(snap.Truncated, []string{"services", "claims"}) {
		t.Fatalf("services %v truncated %v", k.Services, snap.Truncated)
	}
	if !slices.Equal(k.Namespaces, []string{"shop"}) {
		t.Fatalf("the rest of the cluster went with the forbidden list: %v", k.Namespaces)
	}
}

// Lists are read a page of 500 at a time and every page is joined.
func TestSnapshotPagesWithLimitAndContinue(t *testing.T) {
	c, cs := cluster(t)
	var asked []metav1.ListOptions
	cs.PrependReactor("list", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		opts := a.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		asked = append(asked, opts)
		if opts.Continue == "" {
			return true, &corev1.NamespaceList{ListMeta: metav1.ListMeta{Continue: "page-2"}, Items: []corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}}}, nil
		}
		return true, &corev1.NamespaceList{Items: []corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "b"}}}}, nil
	})
	snap, _ := c.Snapshot(context.Background())
	if !slices.Equal(snap.Kubernetes.Namespaces, []string{"a", "b"}) || len(asked) != 2 || asked[0].Limit != 500 || asked[1].Continue != "page-2" {
		t.Fatalf("namespaces %v after %+v", snap.Kubernetes.Namespaces, asked)
	}
}

func TestFactsNameTheCluster(t *testing.T) {
	c, _ := cluster(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}})
	facts := c.Facts(context.Background())
	if facts["runtime"] != "kubernetes" || facts["server_version"] != "v1.36.0" || facts["node_count"] != "2" || facts["platform"] != "linux/amd64" || len(facts) != 4 {
		t.Fatalf("facts %v", facts)
	}
}

// logPod is a pod with the given containers in namespace shop.
func logPod(name string, containers ...string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: meta("shop", name)}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	return p
}

// History asks the kubelet for the tail, the timestamps, the window and the byte ceiling, of
// the pod's only container when none is named.
func TestLogsAskForTheRequestedWindow(t *testing.T) {
	c, _ := cluster(t, logPod("web", "nginx"))
	var got *corev1.PodLogOptions
	c.openLog = func(_ context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		if namespace != "shop" || pod != "web" {
			t.Errorf("opened %s/%s", namespace, pod)
		}
		got = opts
		return io.NopCloser(strings.NewReader("one\ntwo\n")), nil
	}
	since := time.Date(2026, 9, 25, 7, 0, 0, 0, time.UTC)
	var out strings.Builder
	err := c.Logs(context.Background(), protocol.LogRequest{Stream: "s", Pod: &protocol.PodTarget{Namespace: "shop", Name: "web"}, Tail: 5, Since: since, Timestamps: true}, func(b []byte) error { out.Write(b); return nil })
	if err != nil || out.String() != "one\ntwo\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
	if got.Container != "nginx" || *got.TailLines != 5 || !got.Timestamps || got.Follow || !got.SinceTime.Time.Equal(since) || *got.LimitBytes != protocol.MaxLogBytes {
		t.Fatalf("options %+v", got)
	}
}

// A pod with several containers is refused unless one is named, and a name the pod does not
// run is refused, both before any log is opened; so is a request that is not for a pod.
func TestLogsRefuseAnUnnamedOrUnknownContainer(t *testing.T) {
	c, _ := cluster(t, logPod("multi", "web", "sidecar"))
	c.openLog = func(context.Context, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
		t.Error("a log was opened")
		return io.NopCloser(strings.NewReader("")), nil
	}
	sink := func([]byte) error { return nil }
	for _, target := range []protocol.PodTarget{{Namespace: "shop", Name: "multi"}, {Namespace: "shop", Name: "multi", Container: "db"}} {
		err := c.Logs(context.Background(), protocol.LogRequest{Pod: &target}, sink)
		if err == nil || !strings.HasPrefix(err.Error(), "unknown_container") {
			t.Fatalf("%+v: %v", target, err)
		}
	}
	if err := c.Logs(context.Background(), protocol.LogRequest{Pod: &protocol.PodTarget{Namespace: "shop", Name: "gone"}}, sink); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("missing pod: %v", err)
	}
	if err := c.Logs(context.Background(), protocol.LogRequest{Container: strings.Repeat("a", 64)}, sink); err == nil {
		t.Fatal("a container ID reached the cluster adapter")
	}
}

// A follow ends when the reader goes, even with the kubelet silent.
func TestLogsFollowEndsWhenCancelled(t *testing.T) {
	c, _ := cluster(t, logPod("web", "nginx"))
	reader, writer := io.Pipe()
	c.openLog = func(_ context.Context, _, _ string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		if !opts.Follow {
			t.Error("not following")
		}
		return reader, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Logs(ctx, protocol.LogRequest{Pod: &protocol.PodTarget{Namespace: "shop", Name: "web"}, Follow: true}, func(b []byte) error { got <- string(b); return nil })
	}()
	go func() { _, _ = writer.Write([]byte("line\n")) }()
	if s := <-got; s != "line\n" {
		t.Fatalf("got %q", s)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled follow: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follow outlived its reader")
	}
}

// The adapter stops at 10,000 lines, however much more the kubelet sends.
func TestLogsStopAtTheLineCeiling(t *testing.T) {
	c, _ := cluster(t, logPod("web", "nginx"))
	c.openLog = func(context.Context, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(strings.Repeat("x\n", protocol.MaxLogLines*2))), nil
	}
	lines := 0
	err := c.Logs(context.Background(), protocol.LogRequest{Pod: &protocol.PodTarget{Namespace: "shop", Name: "web"}}, func(b []byte) error {
		lines += strings.Count(string(b), "\n")
		return nil
	})
	if err != nil || lines != protocol.MaxLogLines {
		t.Fatalf("%d lines, %v", lines, err)
	}
}
