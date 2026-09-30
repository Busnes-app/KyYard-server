package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// restartedAt is the pod-template annotation a restart stamps, which rolls the pods.
const restartedAt = "kyyard.busnes.app/restarted-at"

// workloadResources is the API group and resource of each kind a WorkloadRef names.
var workloadResources = map[string][2]string{
	protocol.WorkloadDeployment:  {"apps", "deployments"},
	protocol.WorkloadStatefulSet: {"apps", "statefulsets"},
	protocol.WorkloadDaemonSet:   {"apps", "daemonsets"},
	protocol.WorkloadPod:         {"", "pods"},
}

// workloadAPI is the part of a typed client a workload or pod is acted on through.
type workloadAPI[T any] interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (T, error)
	Update(ctx context.Context, obj T, opts metav1.UpdateOptions) (T, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (T, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// handle is one object as read, with pointers into it: a change through them is what update
// writes. template is nil for a pod, replicas nil for a DaemonSet or a pod, paused a Deployment's.
type handle struct {
	meta     *metav1.ObjectMeta
	template *corev1.PodTemplateSpec
	replicas **int32
	paused   *bool
	strategy string
	update   func(context.Context) error
	patch    func(context.Context, []byte) error
	remove   func(context.Context, metav1.DeleteOptions) error
}

func bind[T any](ctx context.Context, api workloadAPI[T], name string, view func(T) handle) (*handle, error) {
	o, err := api.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	h := view(o)
	h.update = func(ctx context.Context) error { _, err := api.Update(ctx, o, metav1.UpdateOptions{}); return err }
	h.patch = func(ctx context.Context, body []byte) error {
		_, err := api.Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
		return err
	}
	h.remove = func(ctx context.Context, opts metav1.DeleteOptions) error { return api.Delete(ctx, name, opts) }
	return &h, nil
}

// workload reads the object ref names.
func (c *Client) workload(ctx context.Context, ref protocol.WorkloadRef) (*handle, error) {
	apps, ns := c.cs.AppsV1(), ref.Namespace
	switch ref.Kind {
	case protocol.WorkloadDeployment:
		return bind(ctx, apps.Deployments(ns), ref.Name, func(d *appsv1.Deployment) handle {
			return handle{meta: &d.ObjectMeta, template: &d.Spec.Template, replicas: &d.Spec.Replicas, paused: &d.Spec.Paused, strategy: string(d.Spec.Strategy.Type)}
		})
	case protocol.WorkloadStatefulSet:
		return bind(ctx, apps.StatefulSets(ns), ref.Name, func(s *appsv1.StatefulSet) handle {
			return handle{meta: &s.ObjectMeta, template: &s.Spec.Template, replicas: &s.Spec.Replicas, strategy: string(s.Spec.UpdateStrategy.Type)}
		})
	case protocol.WorkloadDaemonSet:
		return bind(ctx, apps.DaemonSets(ns), ref.Name, func(s *appsv1.DaemonSet) handle {
			return handle{meta: &s.ObjectMeta, template: &s.Spec.Template, strategy: string(s.Spec.UpdateStrategy.Type)}
		})
	}
	return bind(ctx, c.cs.CoreV1().Pods(ns), ref.Name, func(p *corev1.Pod) handle { return handle{meta: &p.ObjectMeta} })
}

// Operate runs one workload or pod action named by Reference ("<namespace>/<kind>/<name>"), after
// a SelfSubjectAccessReview for its verb: an agent whose manifest lacks the grant answers
// denied forbidden. Restart stamps the pod template's restartedAt annotation, scale sets
// spec.replicas to Expects.Replicas (0..MaxWorkloadReplicas; a DaemonSet has none), delete
// removes the object read just before, by its UID, with Background propagation. The granted
// namespaces are the server's check; here the access review is the boundary.
func (c *Client) Operate(ctx context.Context, cmd protocol.Command) (outcome, detail string) {
	verb := "delete"
	switch cmd.Action {
	case protocol.ActionWorkloadRestart, protocol.ActionWorkloadScale:
		verb = "patch"
	case protocol.ActionWorkloadDelete, protocol.ActionPodDelete:
	default:
		return protocol.OutcomeDenied, "unsupported action"
	}
	ref, err := protocol.ParseWorkloadRef(cmd.Reference)
	if err != nil {
		return protocol.OutcomeDenied, "invalid_reference"
	}
	scale := cmd.Action == protocol.ActionWorkloadScale
	if (ref.Kind == protocol.WorkloadPod) != (cmd.Action == protocol.ActionPodDelete) || (scale && ref.Kind == protocol.WorkloadDaemonSet) {
		return protocol.OutcomeDenied, "unsupported"
	}
	n := cmd.Expects.Replicas
	if scale && (n == nil || *n < 0 || *n > protocol.MaxWorkloadReplicas) {
		return protocol.OutcomeDenied, "replicas_out_of_range"
	}
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	if !cmd.Deadline.IsZero() {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, cmd.Deadline)
		defer stop()
	}

	r := &run{c: c, parent: ctx, namespace: ref.Namespace}
	gr := workloadResources[ref.Kind]
	ok, err := r.review(ctx, authorizationv1.ResourceAttributes{Namespace: ref.Namespace, Verb: verb, Group: gr[0], Resource: gr[1], Name: ref.Name})
	if err != nil {
		return operateFailure(ctx, err)
	}
	if !ok {
		return protocol.OutcomeDenied, "forbidden"
	}
	h, err := c.workload(ctx, ref)
	if err != nil {
		return operateFailure(ctx, err)
	}
	switch {
	case cmd.Action == protocol.ActionWorkloadRestart:
		err = h.patch(ctx, mergePatch(map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]string{restartedAt: time.Now().UTC().Format(time.RFC3339)}}}}))
	case scale:
		err = h.patch(ctx, mergePatch(map[string]any{"replicas": *n}))
	default:
		uid := h.meta.UID
		background := metav1.DeletePropagationBackground
		if err = h.remove(ctx, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &background}); apierrors.IsNotFound(err) {
			err = nil // gone since the read, as asked
		}
	}
	if apierrors.IsForbidden(err) {
		// The access review allowed the verb, so the cluster's admission refused the change.
		return protocol.OutcomeDenied, "admission_denied"
	}
	if err != nil {
		return operateFailure(ctx, err)
	}
	return protocol.OutcomeSucceeded, ""
}

// mergePatch is a JSON merge patch of spec.
func mergePatch(spec map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"spec": spec})
	return b
}

// operateFailure classifies a call that did not succeed, in fixed words: a conflict is the UID
// precondition, an object recreated under the name since the read.
func operateFailure(ctx context.Context, err error) (string, string) {
	var status apierrors.APIStatus
	switch {
	case apierrors.IsForbidden(err):
		return protocol.OutcomeDenied, "forbidden"
	case apierrors.IsNotFound(err):
		return protocol.OutcomeDenied, "not_found"
	case apierrors.IsConflict(err):
		return protocol.OutcomeDenied, "conflict"
	case errors.As(err, &status) && status.Status().Code >= 100 && status.Status().Code <= 599:
		return protocol.OutcomeFailed, "runtime_status " + strconv.Itoa(int(status.Status().Code))
	case ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "runtime_timeout"
	}
	return protocol.OutcomeFailed, "runtime_error"
}

// ReadWorkload reads a Deployment's, StatefulSet's or DaemonSet's editable pod template within
// callBudget. Secret and ConfigMap env are named by reference ("<name>/<key>") and never read.
// Unsupported names what an apply would drop: env from another source (env_field_ref), env or
// argv past the protocol's bounds (env_truncated, argv_truncated). Containers past
// MaxWorkloadContainers, envFrom past MaxWorkloadEnvFrom and resources other than cpu and memory
// are left out of the read but never touched by an apply, so they are not unsupported.
func (c *Client) ReadWorkload(ctx context.Context, target protocol.InspectionTarget) (*protocol.WorkloadConfiguration, error) {
	ref := target.Workload
	if err := target.ValidateFor(protocol.RuntimeKubernetes); err != nil || ref.Kind == "" || ref.Kind == protocol.WorkloadPod {
		return nil, errors.New("a workload configuration read names a Deployment, StatefulSet or DaemonSet by kind")
	}
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	h, err := c.workload(ctx, ref)
	if err != nil {
		return nil, err
	}
	cfg := configuration(ref, h)
	cfg.ObservedAt = time.Now().UTC()
	if err := cfg.Validate(ref, cfg.ObservedAt); err != nil {
		return nil, err
	}
	return cfg, nil
}

func configuration(ref protocol.WorkloadRef, h *handle) *protocol.WorkloadConfiguration {
	unsupported := map[string]bool{}
	spec := h.template.Spec
	cfg := &protocol.WorkloadConfiguration{Target: ref, ResourceVersion: h.meta.ResourceVersion, Strategy: h.strategy, Managed: h.meta.Labels[render.LabelManagedBy] == render.ManagedBy,
		Containers: workloadContainers(spec.Containers, unsupported), InitContainers: workloadContainers(spec.InitContainers, unsupported), EnvFrom: []string{}}
	if h.replicas != nil {
		n := replicas(*h.replicas)
		cfg.Replicas = &n
	}
	if h.paused != nil {
		cfg.Paused = *h.paused
	}
	for _, c := range slices.Concat(spec.Containers, spec.InitContainers) {
		for _, from := range c.EnvFrom {
			var e string
			switch {
			case from.SecretRef != nil:
				e = "secret/" + from.SecretRef.Name
			case from.ConfigMapRef != nil:
				e = "configmap/" + from.ConfigMapRef.Name
			}
			if e != "" && len(cfg.EnvFrom) < protocol.MaxWorkloadEnvFrom && !slices.Contains(cfg.EnvFrom, e) {
				cfg.EnvFrom = append(cfg.EnvFrom, e)
			}
		}
	}
	cfg.Unsupported = slices.Sorted(maps.Keys(unsupported))
	if cfg.Unsupported == nil {
		cfg.Unsupported = []string{}
	}
	return cfg
}

func workloadContainers(list []corev1.Container, unsupported map[string]bool) []protocol.WorkloadContainer {
	out := []protocol.WorkloadContainer{}
	for _, c := range list[:min(len(list), protocol.MaxWorkloadContainers)] {
		out = append(out, workloadContainer(c, unsupported))
	}
	return out
}

func workloadContainer(c corev1.Container, unsupported map[string]bool) protocol.WorkloadContainer {
	out := protocol.WorkloadContainer{Name: c.Name, Image: c.Image, Command: argv(c.Command, unsupported), Args: argv(c.Args, unsupported), Env: []protocol.WorkloadEnv{}, Resources: workloadResourcesOf(c.Resources)}
	total := 0
	for _, e := range c.Env {
		w, ok := workloadEnv(e)
		if !ok {
			unsupported["env_field_ref"] = true
			continue
		}
		size := len(w.Name) + len(w.Value)
		if len(out.Env) == protocol.MaxWorkloadEnv || total+size > protocol.MaxDeploymentEnvBytes || !protocol.ValidConfigurationEnvName(w.Name) || !rawText(w.Value, protocol.MaxDeploymentEnvValueBytes) ||
			slices.ContainsFunc(out.Env, func(have protocol.WorkloadEnv) bool { return have.Name == w.Name }) {
			unsupported["env_truncated"] = true
			continue
		}
		total += size
		out.Env = append(out.Env, w)
	}
	return out
}

// workloadEnv maps a literal or a Secret or ConfigMap key; false for any other source.
func workloadEnv(e corev1.EnvVar) (protocol.WorkloadEnv, bool) {
	w := protocol.WorkloadEnv{Name: e.Name, Value: e.Value}
	switch f := e.ValueFrom; {
	case f == nil:
	case f.SecretKeyRef != nil:
		w.SecretRef = f.SecretKeyRef.Name + "/" + f.SecretKeyRef.Key
	case f.ConfigMapKeyRef != nil:
		w.ConfigMapRef = f.ConfigMapKeyRef.Name + "/" + f.ConfigMapKeyRef.Key
	default:
		return w, false
	}
	return w, true
}

// argv is l up to the first entry past the protocol's bounds.
func argv(l []string, unsupported map[string]bool) []string {
	out := []string{}
	for i, s := range l {
		if i == protocol.MaxArgv || !rawText(s, protocol.MaxArgvEntryBytes) {
			unsupported["argv_truncated"] = true
			break
		}
		out = append(out, s)
	}
	return out
}

func rawText(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func workloadResourcesOf(r corev1.ResourceRequirements) protocol.WorkloadResources {
	q := func(l corev1.ResourceList, name corev1.ResourceName) string {
		if v, ok := l[name]; ok {
			return v.String()
		}
		return ""
	}
	return protocol.WorkloadResources{CPURequest: q(r.Requests, corev1.ResourceCPU), CPULimit: q(r.Limits, corev1.ResourceCPU), MemoryRequest: q(r.Requests, corev1.ResourceMemory), MemoryLimit: q(r.Limits, corev1.ResourceMemory)}
}

// ApplyWorkload writes req.Spec's image, command, args, env and cpu and memory of each named
// container, and replicas and paused, onto the object read fresh, then waits for a Deployment's
// rollout within req.Deadline. The precondition, after an access review for update (forbidden),
// refuses a managed object (application_managed), one at another resourceVersion than the read
// or without a named container (conflict) and one whose read is not whole
// (configuration_unreported: an apply would drop what it did not show). Fields the form does not
// carry, and those it carries unchanged, keep their exact bytes. The Update carries the read's
// resourceVersion, so a concurrent write is the API server's 409, reported conflict. A
// StatefulSet's or DaemonSet's rollout, and a paused Deployment's, is not waited for: rollout is
// skipped. started is called once, before the write.
func (c *Client) ApplyWorkload(parent context.Context, req protocol.WorkloadApply, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Request, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.Validate(time.Now()); err != nil {
		return refused(res, err)
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	ref := req.Target
	r := &run{c: c, parent: parent, res: res, started: started, namespace: ref.Namespace}
	var h *handle
	service := protocol.WorkloadApplyService
	r.step(service, protocol.StepPrecondition, func() (string, string, string) {
		gr := workloadResources[ref.Kind]
		ok, err := r.review(ctx, authorizationv1.ResourceAttributes{Namespace: ref.Namespace, Verb: "update", Group: gr[0], Resource: gr[1], Name: ref.Name})
		if err != nil {
			return r.failure(ctx, err)
		}
		if !ok {
			return protocol.OutcomeDenied, "forbidden", ""
		}
		if h, err = c.workload(ctx, ref); apierrors.IsNotFound(err) {
			return protocol.OutcomeDenied, "conflict", ""
		} else if err != nil {
			return r.failure(ctx, err)
		}
		have := configuration(ref, h)
		switch {
		case have.Managed:
			return protocol.OutcomeDenied, "application_managed", ""
		case h.meta.ResourceVersion != req.ResourceVersion:
			return protocol.OutcomeDenied, "conflict", ""
		case len(have.Unsupported) > 0:
			return protocol.OutcomeDenied, "configuration_unreported", ""
		case !edit(h, req.Spec):
			return protocol.OutcomeDenied, "conflict", ""
		}
		return succeeded()
	})
	r.step(service, protocol.StepApply, func() (string, string, string) {
		r.begin()
		err := h.update(ctx)
		switch {
		case err == nil:
			return succeeded()
		case apierrors.IsConflict(err):
			return protocol.OutcomeDenied, "conflict", ""
		case apierrors.IsForbidden(err):
			return protocol.OutcomeDenied, "admission_denied", ""
		}
		return r.failure(ctx, err)
	})
	r.step(service, protocol.StepRollout, func() (string, string, string) {
		if ref.Kind != protocol.WorkloadDeployment || req.Spec.Paused {
			return protocol.OutcomeSkipped, "", ""
		}
		return r.rollout(ctx, ref.Name, func(d *appsv1.Deployment) string { return reasons(conditionReasons(d)) })
	})
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

// edit sets spec's fields on the object where they differ from what it reads as, so an unchanged
// field keeps its bytes. false: spec names a container the object does not have.
func edit(h *handle, spec protocol.WorkloadConfiguration) bool {
	for _, pair := range []struct {
		live []corev1.Container
		want []protocol.WorkloadContainer
	}{{h.template.Spec.Containers, spec.Containers}, {h.template.Spec.InitContainers, spec.InitContainers}} {
		for _, w := range pair.want {
			i := slices.IndexFunc(pair.live, func(c corev1.Container) bool { return c.Name == w.Name })
			if i < 0 {
				return false
			}
			editContainer(&pair.live[i], w)
		}
	}
	if h.replicas != nil && replicas(*h.replicas) != *spec.Replicas {
		n := *spec.Replicas
		*h.replicas = &n
	}
	if h.paused != nil {
		*h.paused = spec.Paused
	}
	return true
}

func editContainer(c *corev1.Container, w protocol.WorkloadContainer) {
	have := workloadContainer(*c, map[string]bool{})
	c.Image = w.Image
	if !slices.Equal(have.Command, w.Command) {
		c.Command = w.Command
	}
	if !slices.Equal(have.Args, w.Args) {
		c.Args = w.Args
	}
	if !slices.Equal(have.Env, w.Env) {
		c.Env = envVars(c.Env, w.Env)
	}
	setQuantity(&c.Resources.Requests, corev1.ResourceCPU, have.Resources.CPURequest, w.Resources.CPURequest)
	setQuantity(&c.Resources.Limits, corev1.ResourceCPU, have.Resources.CPULimit, w.Resources.CPULimit)
	setQuantity(&c.Resources.Requests, corev1.ResourceMemory, have.Resources.MemoryRequest, w.Resources.MemoryRequest)
	setQuantity(&c.Resources.Limits, corev1.ResourceMemory, have.Resources.MemoryLimit, w.Resources.MemoryLimit)
}

// envVars is want as env, reusing a live entry that maps to the same thing verbatim (a Secret
// reference keeps its optional flag).
func envVars(live []corev1.EnvVar, want []protocol.WorkloadEnv) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(want))
	for _, w := range want {
		if i := slices.IndexFunc(live, func(e corev1.EnvVar) bool { m, ok := workloadEnv(e); return ok && m == w }); i >= 0 {
			out = append(out, live[i])
			continue
		}
		e := corev1.EnvVar{Name: w.Name, Value: w.Value}
		if name, key, ok := strings.Cut(w.SecretRef, "/"); ok {
			e.ValueFrom = &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}
		}
		if name, key, ok := strings.Cut(w.ConfigMapRef, "/"); ok {
			e.ValueFrom = &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}
		}
		out = append(out, e)
	}
	return out
}

// setQuantity sets or, for "", removes one resource when want differs from have; the frame's
// Validate has parsed want.
func setQuantity(list *corev1.ResourceList, name corev1.ResourceName, have, want string) {
	switch {
	case have == want:
	case want == "":
		delete(*list, name)
	default:
		if *list == nil {
			*list = corev1.ResourceList{}
		}
		(*list)[name] = resource.MustParse(want)
	}
}
