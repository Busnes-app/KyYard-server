// Package render turns one deployment request into the Kubernetes objects a cluster agent
// applies: per service a Deployment, a Service, a ConfigMap and a Secret, labelled as KyYard's.
// It runs in the agent only; the server never imports it and links no Kubernetes library.
package render

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Labels and annotations on every object KyYard applies. LabelInstance is ownership: an object
// with the planned name that lacks it, or carries another instance's, is not KyYard's to touch.
const (
	LabelName            = "app.kubernetes.io/name"
	LabelInstanceName    = "app.kubernetes.io/instance"
	LabelManagedBy       = "app.kubernetes.io/managed-by"
	LabelApplication     = "kyyard.busnes.app/application"
	LabelInstance        = "kyyard.busnes.app/instance"
	LabelService         = "kyyard.busnes.app/service"
	AnnotationRevision   = "kyyard.busnes.app/revision"
	AnnotationDeployment = "kyyard.busnes.app/deployment"
	AnnotationSpecDigest = "kyyard.busnes.app/spec-digest"
	ManagedBy            = "kyyard"
)

// ProgressDeadlineSeconds is below the server's ten-minute apply deadline, so a stuck rollout is
// reported by the Deployment itself (ProgressDeadlineExceeded) while the agent still waits.
// MinReadySeconds keeps a process that exits seconds after it starts from counting as available.
const (
	ProgressDeadlineSeconds = 540
	MinReadySeconds         = 10
)

// Set is one service's objects. Secret is nil when the service has no secret-backed value, and
// Service is nil when it publishes no port. Claims are the PersistentVolumeClaims it mounts,
// each mounted by this service alone.
type Set struct {
	Service    string
	Name       string
	ConfigMap  *corev1.ConfigMap
	Secret     *corev1.Secret
	Deployment *appsv1.Deployment
	Endpoint   *corev1.Service
	Claims     []*corev1.PersistentVolumeClaim
}

// Request renders every service of req, in order. req must have passed
// ValidateFor(protocol.RuntimeKubernetes, ...).
func Request(req protocol.DeploymentRequest) []Set {
	services := make([]string, 0, len(req.Services))
	for _, s := range req.Services {
		services = append(services, s.Name)
	}
	names := protocol.KubernetesNames(req.Project, services)
	out := make([]Set, 0, len(req.Services))
	for _, s := range req.Services {
		out = append(out, service(req, s, names[s.Name]))
	}
	return out
}

// Labels are the labels of every object of service s of the instance req targets.
func Labels(req protocol.DeploymentRequest, s string) map[string]string {
	k := req.Kubernetes
	return map[string]string{LabelName: s, LabelInstanceName: req.Project, LabelManagedBy: ManagedBy, LabelApplication: k.ApplicationID, LabelInstance: k.InstanceID, LabelService: s}
}

// Selector is the part of Labels a Deployment and its Service select pods by; it never changes
// for a service, so a later apply can update the Deployment.
func Selector(instance, s string) map[string]string {
	return map[string]string{LabelInstance: instance, LabelService: s}
}

func service(req protocol.DeploymentRequest, s protocol.DeploymentService, name string) Set {
	k := req.Kubernetes
	meta := func(n string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: n, Namespace: k.Namespace, Labels: Labels(req, s.Name), Annotations: map[string]string{
			AnnotationRevision: strconv.Itoa(req.Revision), AnnotationDeployment: req.Deployment, AnnotationSpecDigest: k.SpecDigest,
		}}
	}
	set := Set{Service: s.Name, Name: name}
	config := map[string]string{}
	secret := map[string][]byte{}
	for key, value := range s.Env {
		if slices.Contains(s.SecretKeys, key) {
			secret[key] = []byte(value)
		} else {
			config[key] = value
		}
	}
	set.ConfigMap = &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta(name + "-env"), Data: config}
	envFrom := []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name + "-env"}}}}
	if len(s.SecretKeys) > 0 {
		set.Secret = &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: meta(name + "-secret"), Type: corev1.SecretTypeOpaque, Data: secret}
		envFrom = append(envFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name + "-secret"}}})
	}
	var containerPorts []corev1.ContainerPort
	var servicePorts []corev1.ServicePort
	for _, p := range s.Ports {
		proto := corev1.Protocol(strings.ToUpper(p.Protocol))
		if !slices.ContainsFunc(containerPorts, func(c corev1.ContainerPort) bool { return c.ContainerPort == int32(p.Container) && c.Protocol == proto }) {
			containerPorts = append(containerPorts, corev1.ContainerPort{ContainerPort: int32(p.Container), Protocol: proto})
		}
		servicePorts = append(servicePorts, corev1.ServicePort{Name: fmt.Sprintf("%s-%d", p.Protocol, p.Host), Port: int32(p.Host), TargetPort: intstr.FromInt32(int32(p.Container)), Protocol: proto})
	}
	one, automount, progress := int32(1), false, int32(ProgressDeadlineSeconds)
	// Each apply rolls the pods, as a Docker apply recreates the container: a changed value in
	// the ConfigMap or Secret reaches the process.
	pod := metav1.ObjectMeta{Labels: Labels(req, s.Name), Annotations: map[string]string{AnnotationDeployment: req.Deployment}}
	set.Deployment = &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta(name), Spec: appsv1.DeploymentSpec{
		Replicas:                &one,
		Strategy:                appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
		MinReadySeconds:         MinReadySeconds,
		ProgressDeadlineSeconds: &progress,
		Selector:                &metav1.LabelSelector{MatchLabels: Selector(k.InstanceID, s.Name)},
		Template: corev1.PodTemplateSpec{ObjectMeta: pod, Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: &automount,
			Containers: []corev1.Container{{
				Name:    strings.ReplaceAll(s.Name, "_", "-"),
				Image:   s.Pull.Reference,
				Ports:   containerPorts,
				EnvFrom: envFrom,
			}},
		}},
	}}
	for _, m := range s.Volumes {
		spec := &set.Deployment.Spec.Template.Spec
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: m.Claim, MountPath: m.MountPath, ReadOnly: m.ReadOnly})
		if slices.ContainsFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == m.Claim }) {
			continue
		}
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: m.Claim, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.Claim}}})
		i := slices.IndexFunc(k.Claims, func(c protocol.KubernetesClaim) bool { return c.Name == m.Claim })
		set.Claims = append(set.Claims, claim(k.Claims[i], meta(m.Claim)))
	}
	if len(servicePorts) > 0 {
		set.Endpoint = &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta(name), Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, Selector: Selector(k.InstanceID, s.Name), Ports: servicePorts,
		}}
	}
	return set
}

// claim is a ReadWriteOnce PersistentVolumeClaim of the requested size, in the named StorageClass
// or, with none, the cluster's default.
func claim(c protocol.KubernetesClaim, meta metav1.ObjectMeta) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}, ObjectMeta: meta, Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(c.Size)}},
	}}
	if c.StorageClass != "" {
		class := c.StorageClass
		pvc.Spec.StorageClassName = &class
	}
	return pvc
}
