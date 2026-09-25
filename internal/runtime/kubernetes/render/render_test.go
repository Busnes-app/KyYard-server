package render_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
)

const (
	digest     = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	specDigest = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	app        = "11111111-2222-4333-8444-555555555555"
	instance   = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	deployment = "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b"
)

func request(project string, services ...protocol.DeploymentService) protocol.DeploymentRequest {
	now := time.Now()
	return protocol.DeploymentRequest{Deployment: deployment, RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: project, Revision: 3, IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: app, InstanceID: instance, SpecDigest: specDigest}, Services: services}
}

func web() protocol.DeploymentService {
	return protocol.DeploymentService{Name: "web", Restart: "always", Pull: &protocol.ImagePull{Reference: "ghcr.io/org/web@" + digest, Digest: digest},
		Ports: []protocol.Port{{Container: 80, Host: 8080, Protocol: "tcp"}, {Container: 80, Host: 8081, Protocol: "tcp"}, {Container: 53, Host: 5353, Protocol: "udp"}},
		Env:   map[string]string{"MODE": "prod", "TOKEN": "s3cret"}, SecretKeys: []string{"TOKEN"}, Mounts: []protocol.Mount{}}
}

func api() protocol.DeploymentService {
	return protocol.DeploymentService{Name: "api", Pull: &protocol.ImagePull{Reference: "ghcr.io/org/api@" + digest, Digest: digest}, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: []protocol.Mount{}}
}

// The two-service definition renders exactly these objects: every label and annotation, the
// ConfigMap and Secret split, the Service's port mapping, and no Secret or Service where there
// is nothing to put in one.
func TestRenderTwoServices(t *testing.T) {
	req := request("shop", web(), api())
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
	sets := render.Request(req)
	labels := func(s string) map[string]string {
		return map[string]string{"app.kubernetes.io/name": s, "app.kubernetes.io/instance": "shop", "app.kubernetes.io/managed-by": "kyyard", "kyyard.busnes.app/application": app, "kyyard.busnes.app/instance": instance, "kyyard.busnes.app/service": s}
	}
	meta := func(name, s string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: labels(s), Annotations: map[string]string{"kyyard.busnes.app/revision": "3", "kyyard.busnes.app/deployment": deployment, "kyyard.busnes.app/spec-digest": specDigest}}
	}
	one, automount := int32(1), false
	deploymentOf := func(name, s, image string, ports []corev1.ContainerPort, envFrom []corev1.EnvFromSource) *appsv1.Deployment {
		return &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta(name, s), Spec: appsv1.DeploymentSpec{
			Replicas: &one, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"kyyard.busnes.app/instance": instance, "kyyard.busnes.app/service": s}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels(s), Annotations: map[string]string{"kyyard.busnes.app/deployment": deployment}}, Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyAlways, AutomountServiceAccountToken: &automount,
				Containers: []corev1.Container{{Name: s, Image: image, Ports: ports, EnvFrom: envFrom}},
			}},
		}}
	}
	want := []render.Set{
		{
			Service: "web", Name: "shop-web",
			ConfigMap: &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta("shop-web-env", "web"), Data: map[string]string{"MODE": "prod"}},
			Secret:    &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: meta("shop-web-secret", "web"), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"TOKEN": []byte("s3cret")}},
			Deployment: deploymentOf("shop-web", "web", "ghcr.io/org/web@"+digest,
				[]corev1.ContainerPort{{ContainerPort: 80, Protocol: corev1.ProtocolTCP}, {ContainerPort: 53, Protocol: corev1.ProtocolUDP}},
				[]corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "shop-web-env"}}}, {SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "shop-web-secret"}}}}),
			Endpoint: &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta("shop-web", "web"), Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"kyyard.busnes.app/instance": instance, "kyyard.busnes.app/service": "web"},
				Ports: []corev1.ServicePort{
					{Name: "tcp-8080", Port: 8080, TargetPort: intstr.FromInt32(80), Protocol: corev1.ProtocolTCP},
					{Name: "tcp-8081", Port: 8081, TargetPort: intstr.FromInt32(80), Protocol: corev1.ProtocolTCP},
					{Name: "udp-5353", Port: 5353, TargetPort: intstr.FromInt32(53), Protocol: corev1.ProtocolUDP},
				},
			}},
		},
		{
			Service: "api", Name: "shop-api",
			ConfigMap:  &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta("shop-api-env", "api"), Data: map[string]string{}},
			Deployment: deploymentOf("shop-api", "api", "ghcr.io/org/api@"+digest, nil, []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "shop-api-env"}}}}),
		},
	}
	if !reflect.DeepEqual(sets, want) {
		got, _ := json.MarshalIndent(sets, "", "  ")
		t.Fatalf("rendered:\n%s", got)
	}
	// Each object survives the API's strict decoding: no field is misspelt or dropped.
	decoder := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer()
	for _, set := range sets {
		for _, obj := range []any{set.ConfigMap, set.Secret, set.Deployment, set.Endpoint} {
			if reflect.ValueOf(obj).IsNil() {
				continue
			}
			raw, _ := json.Marshal(obj)
			if _, _, err := decoder.Decode(raw, nil, nil); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
		}
	}
}

// Whatever the project and service names the definition allows, every rendered name and label
// is one Kubernetes accepts, and colliding slugs get distinct names.
func TestRenderNamesAreValid(t *testing.T) {
	svc := func(name string) protocol.DeploymentService {
		s := api()
		s.Name = name
		s.Ports = []protocol.Port{{Container: 80, Host: 80, Protocol: "tcp"}}
		return s
	}
	for _, project := range []string{"shop", "1shop", "Shop.Front_2", strings.Repeat("p", 64)} {
		sets := render.Request(request(project, svc("my_api"), svc("my-api"), svc(strings.Repeat("s", 63))))
		seen := map[string]bool{}
		for _, set := range sets {
			if seen[set.Name] {
				t.Fatalf("%s: %s rendered twice", project, set.Name)
			}
			seen[set.Name] = true
			for _, errs := range [][]string{
				validation.IsDNS1035Label(set.Endpoint.Name),
				validation.IsDNS1123Label(set.Deployment.Name),
				validation.IsDNS1123Subdomain(set.ConfigMap.Name),
				validation.IsDNS1123Label(set.Deployment.Spec.Template.Spec.Containers[0].Name),
				validation.IsValidPortName(set.Endpoint.Spec.Ports[0].Name),
			} {
				if len(errs) > 0 {
					t.Fatalf("%s/%s: %v", project, set.Service, errs)
				}
			}
		}
	}
	// Label values: the project and service must themselves be label values, which the plan
	// checks (k8s_name) before any frame is sent.
	for _, v := range render.Labels(request("shop", svc("my_api")), "my_api") {
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			t.Fatalf("%q: %v", v, errs)
		}
	}
}
