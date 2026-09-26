package kubernetes_test

import (
	"context"
	"crypto/ed25519"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/manifest"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// deployNamespace is the namespace the real-cluster test grants and deploys into.
const deployNamespace = "kyyard-test-deploy"

// TestManifestOnARealCluster applies the rendered manifest to the cluster KY_TEST_KUBECONFIG
// names (a disposable kind cluster: it creates and deletes cluster-scoped RBAC), then acts as
// the agent's ServiceAccount: Secrets are denied cluster-wide, pods are listed, the identity
// Secret round-trips, and a snapshot names the cluster's nodes and a default StorageClass with
// nothing forbidden. In the one granted namespace, which enforces Pod Security baseline, the agent
// may write Deployments, read Secrets by name but not list them, and create claims but neither
// update nor delete them; it deploys KY_TEST_DEPLOY_IMAGE (a digest-pinned image that keeps
// running, such as registry.k8s.io/pause@sha256:...) as one service mounting a claim of the
// default StorageClass, sees it available and the claim bound, applies it again unchanged, and
// removes it, leaving the claim.
func TestManifestOnARealCluster(t *testing.T) {
	path := os.Getenv("KY_TEST_KUBECONFIG")
	if path == "" {
		t.Skip("KY_TEST_KUBECONFIG is not set")
	}
	image := os.Getenv("KY_TEST_DEPLOY_IMAGE")
	if image == "" {
		t.Fatal("KY_TEST_DEPLOY_IMAGE must name a digest-pinned image that keeps running")
	}
	admin, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal(err)
	}
	cs := k8s.NewForConfigOrDie(admin)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	removeAgent(t, ctx, cs)
	t.Cleanup(func() { removeAgent(t, context.Background(), cs) })
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: deployNamespace, Labels: map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	doc, err := manifest.Render(manifest.Input{Image: "ghcr.io/busnes-app/kyyard@sha256:" + strings.Repeat("0", 64), Link: "https://kyyard.invalid/#kyyard=" + strings.Repeat("A", 43), Name: "kind", Namespaces: []string{deployNamespace}})
	if err != nil {
		t.Fatal(err)
	}
	decoder := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer()
	for _, part := range strings.Split(doc, "\n---\n") {
		obj, _, err := decoder.Decode([]byte(part), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		switch o := obj.(type) {
		case *corev1.Namespace:
			_, err = cs.CoreV1().Namespaces().Create(ctx, o, metav1.CreateOptions{})
		case *corev1.ServiceAccount:
			_, err = cs.CoreV1().ServiceAccounts(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.ClusterRole:
			_, err = cs.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.ClusterRoleBinding:
			_, err = cs.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.Role:
			_, err = cs.RbacV1().Roles(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.RoleBinding:
			_, err = cs.RbacV1().RoleBindings(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
		case *corev1.Secret:
			_, err = cs.CoreV1().Secrets(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
		case *appsv1.Deployment:
			_, err = cs.AppsV1().Deployments(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
		default:
			t.Fatalf("unexpected %T", obj)
		}
		if err != nil {
			t.Fatalf("apply %T: %v", obj, err)
		}
	}

	agent := rest.CopyConfig(admin)
	agent.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:kyyard-agent:kyyard-agent"}
	as := k8s.NewForConfigOrDie(agent)
	for _, tc := range []struct {
		verb, resource, namespace, name string
		allowed                         bool
	}{
		{"get", "secrets", "", "", false},
		{"get", "secrets", "default", "", false},
		{"list", "secrets", "kube-system", "", false},
		{"list", "configmaps", "", "", false},
		{"watch", "pods", "", "", false},
		{"delete", "pods", "default", "", false},
		{"list", "pods", "", "", true},
		{"get", "pods/log", "default", "", true},
		{"update", "secrets", "kyyard-agent", "kyyard-agent-identity", true},
		{"update", "secrets", "kyyard-agent", "kyyard-agent-enrollment", false},
	} {
		attrs := &authorizationv1.ResourceAttributes{Verb: tc.verb, Namespace: tc.namespace, Name: tc.name}
		attrs.Resource, attrs.Subresource, _ = strings.Cut(tc.resource, "/")
		review, err := as.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attrs}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if review.Status.Allowed != tc.allowed {
			t.Errorf("%s %s in %q (%s): allowed=%v", tc.verb, tc.resource, tc.namespace, tc.name, review.Status.Allowed)
		}
	}
	for _, tc := range []struct {
		verb, group, resource, namespace string
		allowed                          bool
	}{
		{"create", "apps", "deployments", deployNamespace, true},
		{"delete", "", "services", deployNamespace, true},
		{"update", "", "configmaps", deployNamespace, true},
		{"get", "", "secrets", deployNamespace, true},
		{"list", "", "secrets", deployNamespace, false},
		{"create", "apps", "deployments", "default", false},
		{"create", "", "secrets", "default", false},
		{"create", "", "persistentvolumeclaims", deployNamespace, true},
		{"update", "", "persistentvolumeclaims", deployNamespace, false},
		{"delete", "", "persistentvolumeclaims", deployNamespace, false},
		{"create", "", "persistentvolumeclaims", "default", false},
		{"list", "storage.k8s.io", "storageclasses", "", true},
	} {
		review, err := as.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: tc.verb, Group: tc.group, Resource: tc.resource, Namespace: tc.namespace}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if review.Status.Allowed != tc.allowed {
			t.Errorf("%s %s/%s in %q: allowed=%v", tc.verb, tc.group, tc.resource, tc.namespace, review.Status.Allowed)
		}
	}

	c, err := kubernetes.New(agent)
	if err != nil {
		t.Fatal(err)
	}
	store, err := kubernetes.NewSecretIdentityStore(c, "kyyard-agent", "kyyard-agent-identity")
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	if err := store.Save(&client.Identity{EndpointID: "ep_kind", PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: "https://kyyard.invalid", Generation: 1}); err != nil {
		t.Fatalf("identity create: %v", err)
	}
	if err := store.Save(&client.Identity{EndpointID: "ep_kind", PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: "https://kyyard.invalid", Generation: 2}); err != nil {
		t.Fatalf("identity update: %v", err)
	}
	if got, err := store.Load(); err != nil || got.Generation != 2 {
		t.Fatalf("identity load: %+v %v", got, err)
	}
	snap, err := c.Snapshot(ctx)
	if err != nil || len(snap.Truncated) != 0 || len(snap.Kubernetes.Nodes) == 0 || snap.Kubernetes.Nodes[0].Name == "" {
		t.Fatalf("snapshot as the agent: %v truncated %v nodes %+v", err, snap.Truncated, snap.Kubernetes.Nodes)
	}
	if !slices.ContainsFunc(snap.Kubernetes.StorageClasses, func(c protocol.StorageClass) bool { return c.Default }) {
		t.Fatalf("no default StorageClass reported: %+v", snap.Kubernetes.StorageClasses)
	}

	_, digest, _ := strings.Cut(image, "@")
	now := time.Now()
	target := &protocol.KubernetesTarget{Namespace: deployNamespace, ApplicationID: "11111111-2222-4333-8444-555555555555", InstanceID: "66666666-7777-4888-9999-aaaaaaaaaaaa", SpecDigest: "sha256:" + strings.Repeat("f", 64),
		Claims: []protocol.KubernetesClaim{{Name: "kind-data", Size: "64Mi", AccessMode: protocol.AccessReadWriteOnce}}}
	req := protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_kind", Project: "kind", Revision: 1, IssuedAt: now, Deadline: now.Add(2 * time.Minute), Kubernetes: target,
		Services: []protocol.DeploymentService{{Name: "idle", Pull: &protocol.ImagePull{Reference: image, Digest: digest}, Ports: []protocol.Port{{Container: 8080, Host: 80, Protocol: "tcp"}}, Env: map[string]string{"TOKEN": "x"}, SecretKeys: []string{"TOKEN"}, Mounts: []protocol.Mount{},
			Volumes: []protocol.KubernetesMount{{Claim: "kind-data", MountPath: "/data"}}}}}
	res := c.Deploy(ctx, req, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("deploy as the agent: %+v", res)
	}
	d, err := cs.AppsV1().Deployments(deployNamespace).Get(ctx, "kind-idle", metav1.GetOptions{})
	if err != nil || d.Status.ReadyReplicas != 1 || string(d.UID) != res.Services[0].UID {
		t.Fatalf("deployed %+v %v", d, err)
	}
	// The pod mounted the claim, so the default StorageClass bound it and filled in its name; a
	// second apply takes the claim as it is.
	pvc, err := cs.CoreV1().PersistentVolumeClaims(deployNamespace).Get(ctx, "kind-data", metav1.GetOptions{})
	if err != nil || pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		t.Fatalf("claim %+v %v", pvc, err)
	}
	again := req
	again.Deployment, again.Revision, again.IssuedAt, again.Deadline = "5b4c3d2e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", 2, time.Now(), time.Now().Add(2*time.Minute)
	if res := c.Deploy(ctx, again, func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("second deploy as the agent: %+v", res)
	}
	// What a validation reads, as the agent: the settled Deployment available, its pod running.
	watched := protocol.InspectionTarget{Workload: protocol.WorkloadRef{Namespace: deployNamespace, Name: "kind-idle", UID: res.Services[0].UID}}
	in, err := c.Inspect(ctx, watched)
	if err != nil || in.Validate(watched, time.Now(), false) != nil {
		t.Fatalf("inspect as the agent: %+v %v", in, err)
	}
	if w := in.Workload; w.Missing || w.UID != res.Services[0].UID || w.ObservedGeneration < w.Generation || w.Desired != 1 || w.Available != 1 || w.Ready != 1 || len(w.Pods) != 1 || w.Pods[0].Containers[0].State != "running" {
		t.Fatalf("workload status %+v", in.Workload)
	}
	removal := protocol.RemovalRequest{Deployment: "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_kind", Project: "kind", IssuedAt: time.Now(), Deadline: time.Now().Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: target.Namespace, ApplicationID: target.ApplicationID, InstanceID: target.InstanceID, SpecDigest: target.SpecDigest}, Services: []string{"idle"}}
	removed := c.Remove(ctx, removal, func() {})
	if removed.Outcome != protocol.OutcomeSucceeded || !slices.Contains(removed.Steps, protocol.DeploymentStep{Service: "idle", Step: protocol.StepVolume, Outcome: protocol.OutcomeSkipped, Detail: protocol.DetailRetained}) {
		t.Fatalf("remove as the agent: %+v", removed)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		_, err := cs.AppsV1().Deployments(deployNamespace).Get(ctx, "kind-idle", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Deployment is still there: %v", err)
		}
	}
	for _, name := range []string{"kind-idle-env", "kind-idle-secret"} {
		_, errCM := cs.CoreV1().ConfigMaps(deployNamespace).Get(ctx, name, metav1.GetOptions{})
		_, errS := cs.CoreV1().Secrets(deployNamespace).Get(ctx, name, metav1.GetOptions{})
		if !apierrors.IsNotFound(errCM) || !apierrors.IsNotFound(errS) {
			t.Fatalf("%s left behind: %v %v", name, errCM, errS)
		}
	}
	if kept, err := cs.CoreV1().PersistentVolumeClaims(deployNamespace).Get(ctx, "kind-data", metav1.GetOptions{}); err != nil || kept.DeletionTimestamp != nil {
		t.Fatalf("the removal took the claim: %+v %v", kept, err)
	}
}

// removeAgent deletes what the manifest creates and waits for the namespace to go, so a run
// that died halfway does not break the next one.
func removeAgent(t *testing.T, ctx context.Context, cs k8s.Interface) {
	t.Helper()
	ignore := func(err error) {
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("cleanup: %v", err)
		}
	}
	ignore(cs.RbacV1().ClusterRoleBindings().Delete(ctx, "kyyard-agent-read", metav1.DeleteOptions{}))
	ignore(cs.RbacV1().ClusterRoles().Delete(ctx, "kyyard-agent-read", metav1.DeleteOptions{}))
	for _, ns := range []string{"kyyard-agent", deployNamespace} {
		ignore(cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}))
	}
	deadline := time.Now().Add(2 * time.Minute)
	for _, ns := range []string{"kyyard-agent", deployNamespace} {
		for {
			_, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("namespace %s still present: %v", ns, err)
			}
			time.Sleep(time.Second)
		}
	}
}
