package manifest_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/manifest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
)

const (
	image = "ghcr.io/busnes-app/kyyard@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	token = "AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKK"
	link  = "https://yard.example/#kyyard=" + token
	name  = `prod "east": #1`
)

// decode reads every document strictly into its typed object, so a misspelt field fails here
// rather than being dropped by the API server.
func decode(t *testing.T, doc string) []runtime.Object {
	t.Helper()
	d := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer()
	var out []runtime.Object
	for _, part := range strings.Split(doc, "\n---\n") {
		obj, _, err := d.Decode([]byte(part), nil, nil)
		if err != nil {
			t.Fatalf("document does not decode: %v\n%s", err, part)
		}
		out = append(out, obj)
	}
	return out
}

func render(t *testing.T) (string, []runtime.Object) {
	t.Helper()
	doc, err := manifest.Render(manifest.Input{Image: image, Link: link, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return doc, decode(t, doc)
}

// The RBAC is exactly the read subset: get and list only, no Secrets or ConfigMaps, no watch
// and no wildcard; the only Secret access is the namespaced identity Role.
func TestManifestRBACIsReadOnlyWithoutSecrets(t *testing.T) {
	_, objects := render(t)
	var kinds []string
	var clusterRole *rbacv1.ClusterRole
	var role *rbacv1.Role
	for _, obj := range objects {
		kinds = append(kinds, obj.GetObjectKind().GroupVersionKind().Kind)
		switch o := obj.(type) {
		case *rbacv1.ClusterRole:
			clusterRole = o
		case *rbacv1.Role:
			role = o
		case *rbacv1.ClusterRoleBinding:
			if o.RoleRef.Name != "kyyard-agent-read" || len(o.Subjects) != 1 || o.Subjects[0].Kind != "ServiceAccount" || o.Subjects[0].Name != "kyyard-agent" || o.Subjects[0].Namespace != "kyyard-agent" {
				t.Fatalf("cluster role binding %+v", o)
			}
		case *rbacv1.RoleBinding:
			if o.Namespace != "kyyard-agent" || o.RoleRef.Name != "kyyard-agent-identity" || len(o.Subjects) != 1 || o.Subjects[0].Name != "kyyard-agent" {
				t.Fatalf("role binding %+v", o)
			}
		}
	}
	if !slices.Equal(kinds, []string{"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "Secret", "Deployment"}) {
		t.Fatalf("kinds %v", kinds)
	}
	var granted []string
	for _, rule := range clusterRole.Rules {
		if !slices.Equal(rule.Verbs, []string{"get", "list"}) || len(rule.ResourceNames) != 0 || len(rule.NonResourceURLs) != 0 {
			t.Fatalf("cluster rule %+v", rule)
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				granted = append(granted, group+"/"+resource)
			}
		}
	}
	slices.Sort(granted)
	want := []string{"/events", "/namespaces", "/nodes", "/persistentvolumeclaims", "/pods", "/pods/log", "/services", "apps/daemonsets", "apps/deployments", "apps/statefulsets"}
	if !slices.Equal(granted, want) {
		t.Fatalf("cluster role grants %v", granted)
	}
	for _, g := range granted {
		if strings.Contains(g, "*") || strings.HasSuffix(g, "/secrets") || strings.HasSuffix(g, "/configmaps") {
			t.Fatalf("cluster role grants %s", g)
		}
	}
	if role.Namespace != "kyyard-agent" || len(role.Rules) != 2 {
		t.Fatalf("role %+v", role)
	}
	open, named := role.Rules[0], role.Rules[1]
	if !slices.Equal(open.Resources, []string{"secrets"}) || !slices.Equal(open.Verbs, []string{"get", "create"}) || len(open.ResourceNames) != 0 {
		t.Fatalf("identity rule %+v", open)
	}
	if !slices.Equal(named.Resources, []string{"secrets"}) || !slices.Equal(named.Verbs, []string{"update"}) || !slices.Equal(named.ResourceNames, []string{"kyyard-agent-identity"}) {
		t.Fatalf("identity update rule %+v", named)
	}
}

// The token appears exactly once, inside the enrollment Secret; the image is the pinned digest;
// the Deployment runs one locked-down replica with the Secret mounted read-only.
func TestManifestCarriesTheTokenOnceAndALockedDownAgent(t *testing.T) {
	doc, objects := render(t)
	if n := strings.Count(doc, token); n != 1 {
		t.Fatalf("the token appears %d times", n)
	}
	for _, obj := range objects {
		switch o := obj.(type) {
		case *corev1.Secret:
			if o.Name != "kyyard-agent-enrollment" || o.Namespace != "kyyard-agent" || o.StringData["link"] != link {
				t.Fatalf("secret %+v", o)
			}
		case *appsv1.Deployment:
			spec := o.Spec.Template.Spec
			c := spec.Containers[0]
			if *o.Spec.Replicas != 1 || o.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || spec.ServiceAccountName != "kyyard-agent" || !*spec.AutomountServiceAccountToken {
				t.Fatalf("deployment %+v", o.Spec)
			}
			if c.Image != image || !config.IsPinnedAgentImage(c.Image) || !slices.Equal(c.Command, []string{"/app/kyyard-agent"}) {
				t.Fatalf("container %+v", c)
			}
			if !slices.Equal(c.Args, []string{"--kubernetes", "--link-file", "/etc/kyyard/link", "--identity-secret", "kyyard-agent-identity", "--name", name, "--docker-socket="}) {
				t.Fatalf("args %q", c.Args)
			}
			sc, psc := c.SecurityContext, spec.SecurityContext
			if !*psc.RunAsNonRoot || *psc.RunAsUser != 65532 || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
				t.Fatalf("security context %+v %+v", psc, sc)
			}
			if c.Resources.Requests.Cpu().String() != "50m" || c.Resources.Requests.Memory().String() != "64Mi" || c.Resources.Limits.Cpu().String() != "500m" || c.Resources.Limits.Memory().String() != "256Mi" {
				t.Fatalf("resources %+v", c.Resources)
			}
			if m := c.VolumeMounts[0]; m.MountPath != "/etc/kyyard" || !m.ReadOnly || spec.Volumes[0].Secret.SecretName != "kyyard-agent-enrollment" || !*spec.Volumes[0].Secret.Optional {
				t.Fatalf("mount %+v %+v", m, spec.Volumes[0])
			}
		}
		labels := obj.(interface{ GetLabels() map[string]string }).GetLabels()
		if labels["app.kubernetes.io/name"] != "kyyard-agent" || labels["app.kubernetes.io/managed-by"] != "kyyard" {
			t.Fatalf("%T labels %v", obj, labels)
		}
	}
}

func TestManifestRefusesAnUnpinnedImage(t *testing.T) {
	for _, in := range []manifest.Input{
		{Image: "ghcr.io/busnes-app/kyyard:latest", Link: link, Name: "c"},
		{Image: image, Link: "http://yard.example/#kyyard=" + token, Name: "c"},
		{Image: image, Link: link},
	} {
		if _, err := manifest.Render(in); err == nil {
			t.Fatalf("rendered %+v", in)
		}
	}
}

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"prod-cluster":  "kyyard-agent-prod-cluster.yaml",
		`prod "east" 1`: "kyyard-agent-prod-east-1.yaml",
		"Ünïcode/../x":  "kyyard-agent-n-code-x.yaml",
		"$(rm -rf /)":   "kyyard-agent-rm-rf.yaml",
		"***":           "kyyard-agent-endpoint.yaml",
	} {
		if got := manifest.FileName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
