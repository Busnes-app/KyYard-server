package kubernetes_test

import (
	"context"
	"crypto/ed25519"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
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

// TestManifestOnARealCluster applies the rendered manifest to the cluster KY_TEST_KUBECONFIG
// names (a disposable kind cluster: it creates and deletes cluster-scoped RBAC), then acts as
// the agent's ServiceAccount: Secrets are denied cluster-wide, pods are listed, the identity
// Secret round-trips, and a snapshot names the cluster's nodes with nothing forbidden.
func TestManifestOnARealCluster(t *testing.T) {
	path := os.Getenv("KY_TEST_KUBECONFIG")
	if path == "" {
		t.Skip("KY_TEST_KUBECONFIG is not set")
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

	doc, err := manifest.Render(manifest.Input{Image: "ghcr.io/busnes-app/kyyard@sha256:" + strings.Repeat("0", 64), Link: "https://kyyard.invalid/#kyyard=" + strings.Repeat("A", 43), Name: "kind"})
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
	ignore(cs.CoreV1().Namespaces().Delete(ctx, "kyyard-agent", metav1.DeleteOptions{}))
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := cs.CoreV1().Namespaces().Get(ctx, "kyyard-agent", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("namespace kyyard-agent still present: %v", err)
		}
		time.Sleep(time.Second)
	}
}
