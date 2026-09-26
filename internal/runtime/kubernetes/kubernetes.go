// Package kubernetes is the Kubernetes adapter: it reads the cluster the agent runs in through
// the typed clientset, with the agent's ServiceAccount, and returns product types only. No
// client-go type leaves this package.
package kubernetes

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// pageSize is the Limit of every List call; Continue pages the rest.
	pageSize = 500
	// callBudget bounds one API call whose caller named no deadline.
	callBudget = 20 * time.Second
	// serviceAccountNamespace is where the kubelet writes the pod's own namespace.
	serviceAccountNamespace = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	// nodeRolePrefix labels a node with a role; the role is the rest of the key.
	nodeRolePrefix = "node-role.kubernetes.io/"
)

type Client struct {
	cs k8s.Interface
	// poll is how often a rollout is read; tests shorten it.
	poll time.Duration
	// openLog streams one container's log. Tests replace it: the fake clientset answers every
	// log request with the same text and ignores its options.
	openLog func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)
	log     *log.Logger
}

// New reads the cluster cfg names.
func New(cfg *rest.Config) (*Client, error) {
	cs, err := k8s.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return NewFromClientset(cs), nil
}

// InCluster reads the cluster the agent's pod runs in, as its ServiceAccount.
func InCluster() (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

// NewFromClientset is for tests: cs is usually k8s.io/client-go/kubernetes/fake.
func NewFromClientset(cs k8s.Interface) *Client {
	return &Client{cs: cs, log: log.Default(), poll: rolloutPoll, openLog: func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		return cs.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
	}}
}

// OwnNamespace is the namespace the agent's pod runs in.
func OwnNamespace() (string, error) {
	raw, err := os.ReadFile(serviceAccountNamespace)
	if err != nil {
		return "", err
	}
	ns := strings.TrimSpace(string(raw))
	if !protocol.ValidDNSLabel(ns) {
		return "", errors.New("the service account namespace file does not hold a namespace name")
	}
	return ns, nil
}

// serverVersion reads the API server's version within callBudget: the one read Facts and every
// snapshot's Engine share.
func (c *Client) serverVersion(ctx context.Context) (*version.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	return c.cs.Discovery().ServerVersionWithContext(ctx)
}

// engine is the API server's version as a snapshot reports it.
func (c *Client) engine(ctx context.Context) (protocol.Engine, error) {
	v, err := c.serverVersion(ctx)
	if err != nil {
		return protocol.Engine{Runtime: protocol.RuntimeKubernetes}, err
	}
	os, arch, _ := strings.Cut(v.Platform, "/")
	return protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: v.GitVersion, APIVersion: v.Major + "." + v.Minor, OS: os, Arch: arch}, nil
}

// Facts is the enrollment report for a cluster: what an approver compares with the cluster
// they meant to enroll. A fact the agent cannot read is "unknown", never left out.
func (c *Client) Facts(ctx context.Context) map[string]string {
	facts := map[string]string{"runtime": protocol.RuntimeKubernetes, "server_version": "unknown", "node_count": "unknown", "platform": "unknown"}
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	if v, err := c.serverVersion(ctx); err == nil {
		facts["server_version"], facts["platform"] = v.GitVersion, v.Platform
	}
	if nodes, err := listAll(ctx, protocol.MaxNodes, c.nodes); err == nil {
		facts["node_count"] = strconv.Itoa(len(nodes))
	}
	return facts
}

// Snapshot reads the cluster's inventory, every list at once so a slow API server costs the
// slowest list rather than the sum. A list the ServiceAccount cannot read is reported empty
// and named in Truncated, so one forbidden verb does not blank the whole endpoint; the error
// return is only ever nil.
func (c *Client) Snapshot(ctx context.Context) (*protocol.Snapshot, error) {
	k := &protocol.KubernetesInventory{Nodes: []protocol.Node{}, Namespaces: []string{}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}, StorageClasses: []protocol.StorageClass{}}
	// Each read owns its slot; the three workload kinds are joined after the wait.
	var deployments, statefulSets, daemonSets []protocol.Workload
	lists := []struct {
		name string
		read func() error
	}{
		{"nodes", reader(ctx, protocol.MaxNodes, c.nodes, node, &k.Nodes)},
		{"namespaces", reader(ctx, protocol.MaxNamespaces, c.namespaces, namespace, &k.Namespaces)},
		{"workloads", reader(ctx, protocol.MaxWorkloads, c.deployments, deployment, &deployments)},
		{"workloads", reader(ctx, protocol.MaxWorkloads, c.statefulSets, statefulSet, &statefulSets)},
		{"workloads", reader(ctx, protocol.MaxWorkloads, c.daemonSets, daemonSet, &daemonSets)},
		{"pods", reader(ctx, protocol.MaxPods, c.pods, pod, &k.Pods)},
		{"services", reader(ctx, protocol.MaxServices, c.services, service, &k.Services)},
		{"claims", reader(ctx, protocol.MaxClaims, c.claims, claim, &k.Claims)},
		{"storage_classes", reader(ctx, protocol.MaxStorageClasses, c.storageClasses, storageClass, &k.StorageClasses)},
	}
	var (
		engine    protocol.Engine
		engineErr error
		errs      = make([]error, len(lists))
		wg        sync.WaitGroup
	)
	wg.Go(func() { engine, engineErr = c.engine(ctx) })
	for i, l := range lists {
		wg.Go(func() { errs[i] = l.read() })
	}
	wg.Wait()

	if engineErr != nil {
		c.log.Printf("kubernetes: reading the server version: %v", engineErr)
	}
	truncated := map[string]bool{}
	for i, l := range lists {
		if errs[i] != nil {
			c.log.Printf("kubernetes: listing %s: %v; reported empty", l.name, errs[i])
			truncated[l.name] = true
		}
	}
	k.Workloads = slices.Concat([]protocol.Workload{}, deployments, statefulSets, daemonSets)
	snap := &protocol.Snapshot{ObservedAt: time.Now().UTC(), Engine: engine, Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}, Kubernetes: k}
	slices.SortFunc(k.Nodes, func(a, b protocol.Node) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(k.Namespaces)
	slices.SortFunc(k.Workloads, func(a, b protocol.Workload) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name), strings.Compare(a.Kind, b.Kind))
	})
	slices.SortFunc(k.Pods, func(a, b protocol.Pod) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	slices.SortFunc(k.Services, func(a, b protocol.Service) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	slices.SortFunc(k.Claims, func(a, b protocol.Claim) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	slices.SortFunc(k.StorageClasses, func(a, b protocol.StorageClass) int { return strings.Compare(a.Name, b.Name) })
	for list := range truncated {
		snap.Truncated = append(snap.Truncated, list)
	}
	// Clamp cuts every list at its cap after the sort and names the cut; Shrink fits the
	// shared byte limit.
	protocol.Clamp(snap)
	_ = protocol.Shrink(snap)
	return snap, nil
}

// reader lists one kind into dst, mapped into product types; a failed list leaves dst as it
// was.
func reader[T, P any](ctx context.Context, max int, page func(context.Context, metav1.ListOptions) ([]T, string, error), conv func(T) P, dst *[]P) func() error {
	return func() error {
		items, err := listAll(ctx, max, page)
		if err != nil {
			return err
		}
		for _, item := range items {
			*dst = append(*dst, conv(item))
		}
		return nil
	}
}

// listAll pages through one kind with Limit and Continue. It stops once it holds more than
// max, which is enough to know the list is cut without reading the rest of a large cluster;
// the API server returns items in namespace/name order, so the pages read are the head of it.
func listAll[T any](ctx context.Context, max int, page func(context.Context, metav1.ListOptions) ([]T, string, error)) ([]T, error) {
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	var out []T
	opts := metav1.ListOptions{Limit: pageSize}
	for {
		items, next, err := page(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" || len(out) > max {
			return out, nil
		}
		opts.Continue = next
	}
}

func (c *Client) nodes(ctx context.Context, o metav1.ListOptions) ([]corev1.Node, string, error) {
	l, err := c.cs.CoreV1().Nodes().List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) namespaces(ctx context.Context, o metav1.ListOptions) ([]corev1.Namespace, string, error) {
	l, err := c.cs.CoreV1().Namespaces().List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) deployments(ctx context.Context, o metav1.ListOptions) ([]appsv1.Deployment, string, error) {
	l, err := c.cs.AppsV1().Deployments("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) statefulSets(ctx context.Context, o metav1.ListOptions) ([]appsv1.StatefulSet, string, error) {
	l, err := c.cs.AppsV1().StatefulSets("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) daemonSets(ctx context.Context, o metav1.ListOptions) ([]appsv1.DaemonSet, string, error) {
	l, err := c.cs.AppsV1().DaemonSets("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) pods(ctx context.Context, o metav1.ListOptions) ([]corev1.Pod, string, error) {
	l, err := c.cs.CoreV1().Pods("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) services(ctx context.Context, o metav1.ListOptions) ([]corev1.Service, string, error) {
	l, err := c.cs.CoreV1().Services("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) claims(ctx context.Context, o metav1.ListOptions) ([]corev1.PersistentVolumeClaim, string, error) {
	l, err := c.cs.CoreV1().PersistentVolumeClaims("").List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

func (c *Client) storageClasses(ctx context.Context, o metav1.ListOptions) ([]storagev1.StorageClass, string, error) {
	l, err := c.cs.StorageV1().StorageClasses().List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

// storageClass is default when either the current or the beta is-default-class annotation is
// "true", as the DefaultStorageClass admission plugin reads it.
func storageClass(sc storagev1.StorageClass) protocol.StorageClass {
	return protocol.StorageClass{Name: sc.Name, Default: sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" || sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"}
}

func namespace(ns corev1.Namespace) string { return ns.Name }

// deployment carries KyYard's application and instance labels, so the server can tell the
// Deployments it applied from everything else in the cluster.
func deployment(d appsv1.Deployment) protocol.Workload {
	return protocol.Workload{Kind: "Deployment", Namespace: d.Namespace, Name: d.Name, Desired: replicas(d.Spec.Replicas), Ready: d.Status.ReadyReplicas, Updated: d.Status.UpdatedReplicas, Images: images(d.Spec.Template.Spec), Paused: d.Spec.Paused,
		Application: d.Labels[render.LabelApplication], Instance: d.Labels[render.LabelInstance]}
}

func statefulSet(s appsv1.StatefulSet) protocol.Workload {
	return protocol.Workload{Kind: "StatefulSet", Namespace: s.Namespace, Name: s.Name, Desired: replicas(s.Spec.Replicas), Ready: s.Status.ReadyReplicas, Updated: s.Status.UpdatedReplicas, Images: images(s.Spec.Template.Spec)}
}

func daemonSet(s appsv1.DaemonSet) protocol.Workload {
	return protocol.Workload{Kind: "DaemonSet", Namespace: s.Namespace, Name: s.Name, Desired: s.Status.DesiredNumberScheduled, Ready: s.Status.NumberReady, Updated: s.Status.UpdatedNumberScheduled, Images: images(s.Spec.Template.Spec)}
}

func node(n corev1.Node) protocol.Node {
	out := protocol.Node{Name: n.Name, KubeletVersion: n.Status.NodeInfo.KubeletVersion, OS: n.Status.NodeInfo.OperatingSystem, Arch: n.Status.NodeInfo.Architecture, Unschedulable: n.Spec.Unschedulable, Roles: []string{}}
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			out.Ready = cond.Status == corev1.ConditionTrue
		}
	}
	for key := range n.Labels {
		if role, ok := strings.CutPrefix(key, nodeRolePrefix); ok && role != "" {
			out.Roles = append(out.Roles, role)
		}
	}
	slices.Sort(out.Roles)
	return out
}

// replicas is a workload's desired count; Kubernetes defaults an unset one to 1.
func replicas(n *int32) int32 {
	if n == nil {
		return 1
	}
	return *n
}

func images(spec corev1.PodSpec) []string {
	out := []string{}
	for _, c := range spec.Containers {
		out = append(out, c.Image)
	}
	return out
}

func pod(p corev1.Pod) protocol.Pod {
	out := protocol.Pod{Namespace: p.Namespace, Name: p.Name, Phase: string(p.Status.Phase), Node: p.Spec.NodeName, Containers: []protocol.PodContainer{}}
	if p.Status.StartTime != nil {
		out.StartedAt = p.Status.StartTime.UTC()
	}
	out.OwnerKind, out.OwnerName = owner(p)
	statuses := map[string]corev1.ContainerStatus{}
	for _, s := range p.Status.ContainerStatuses {
		statuses[s.Name] = s
	}
	for _, c := range p.Spec.Containers {
		pc := protocol.PodContainer{Name: c.Name, Image: c.Image, State: "waiting"}
		if s, ok := statuses[c.Name]; ok {
			pc.ImageID, pc.Ready, pc.RestartCount = s.ImageID, s.Ready, s.RestartCount
			switch {
			case s.State.Running != nil:
				pc.State = "running"
			case s.State.Terminated != nil:
				pc.State, pc.Reason = "terminated", s.State.Terminated.Reason
			case s.State.Waiting != nil:
				pc.Reason = s.State.Waiting.Reason
			}
		}
		out.Containers = append(out.Containers, pc)
	}
	return out
}

// owner is the pod's controller. A ReplicaSet a Deployment made is named after the Deployment
// plus the pod-template-hash the pod also carries, so the Deployment is read off the name
// rather than from the ReplicaSet, which the agent's role cannot list.
func owner(p corev1.Pod) (kind, name string) {
	ref := metav1.GetControllerOf(&p)
	if ref == nil {
		return "", ""
	}
	if hash := p.Labels[appsv1.DefaultDeploymentUniqueLabelKey]; ref.Kind == "ReplicaSet" && hash != "" {
		if deployment, ok := strings.CutSuffix(ref.Name, "-"+hash); ok && deployment != "" {
			return "Deployment", deployment
		}
	}
	return ref.Kind, ref.Name
}

func service(s corev1.Service) protocol.Service {
	out := protocol.Service{Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type), ClusterIP: s.Spec.ClusterIP, Ports: []string{}}
	for _, p := range s.Spec.Ports {
		port := strconv.Itoa(int(p.Port))
		if p.NodePort != 0 {
			port += ":" + strconv.Itoa(int(p.NodePort))
		}
		out.Ports = append(out.Ports, port+"/"+string(p.Protocol))
	}
	return out
}

func claim(pvc corev1.PersistentVolumeClaim) protocol.Claim {
	out := protocol.Claim{Namespace: pvc.Namespace, Name: pvc.Name, Phase: string(pvc.Status.Phase)}
	if pvc.Spec.StorageClassName != nil {
		out.StorageClass = *pvc.Spec.StorageClassName
	}
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		out.Capacity = q.String()
	}
	return out
}
