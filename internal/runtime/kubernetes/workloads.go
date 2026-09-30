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
	"k8s.io/apimachinery/pkg/util/strategicpatch"
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
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (T, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// handle is one object as read, with pointers into it: a change through them is what write
// sends. template is nil for a pod, replicas nil for a DaemonSet or a pod, paused a Deployment's.
// retention is a StatefulSet's claim retention policy, nil for other kinds or when unset.
type handle struct {
	meta      *metav1.ObjectMeta
	template  *corev1.PodTemplateSpec
	replicas  **int32
	paused    *bool
	strategy  string
	retention *appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy
	write     func(context.Context) error
	patch     func(context.Context, []byte) error
	remove    func(context.Context, metav1.DeleteOptions) error
}

// scaleDeletesClaims reports whether setting replicas to n deletes volume claims: a StatefulSet
// with whenScaled Delete taken below its current replicas.
func (h *handle) scaleDeletesClaims(n *int32) bool {
	return h.retention != nil && h.retention.WhenScaled == appsv1.DeletePersistentVolumeClaimRetentionPolicyType && n != nil && *n < replicas(*h.replicas)
}

// deleteDeletesClaims reports whether deleting the object deletes its volume claims: a
// StatefulSet with whenDeleted Delete.
func (h *handle) deleteDeletesClaims() bool {
	return h.retention != nil && h.retention.WhenDeleted == appsv1.DeletePersistentVolumeClaimRetentionPolicyType
}

func bind[T any](ctx context.Context, api workloadAPI[T], name string, view func(T) handle) (*handle, error) {
	o, err := api.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	read, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	h := view(o)
	version := h.meta.ResourceVersion
	// write sends the changes made through the handle since the read as a strategic merge patch
	// against the typed kind, carrying the read's resourceVersion: fields this client's types do
	// not know are never sent, so they survive, and a concurrent write is the API server's 409.
	h.write = func(ctx context.Context) error {
		edited, err := json.Marshal(o)
		if err != nil {
			return err
		}
		body, err := strategicpatch.CreateTwoWayMergePatch(read, edited, o)
		if err != nil {
			return err
		}
		var patch map[string]any
		if err := json.Unmarshal(body, &patch); err != nil {
			return err
		}
		metadata, _ := patch["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["resourceVersion"] = version
		patch["metadata"] = metadata
		if body, err = json.Marshal(patch); err != nil {
			return err
		}
		_, err = api.Patch(ctx, name, types.StrategicMergePatchType, body, metav1.PatchOptions{})
		return err
	}
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
			return handle{meta: &s.ObjectMeta, template: &s.Spec.Template, replicas: &s.Spec.Replicas, strategy: string(s.Spec.UpdateStrategy.Type),
				retention: s.Spec.PersistentVolumeClaimRetentionPolicy}
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
// spec.replicas to Expects.Replicas (0..MaxWorkloadReplicas; a DaemonSet has none) at the read's
// resourceVersion and refuses to scale a StatefulSet below its replicas when that deletes the
// excess replicas' claims (whenScaled: Delete, pvc_retention), delete
// removes the object read just before, by its UID (a workload also by its resourceVersion), with
// Background propagation, unless it is labelled managed-by kyyard (application_managed) or is a
// StatefulSet whose claims go with it (whenDeleted: Delete, pvc_retention). The granted
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
	case scale && h.scaleDeletesClaims(n):
		return protocol.OutcomeDenied, "pvc_retention"
	case scale:
		// The read's resourceVersion makes a change since the check above a 409.
		body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": h.meta.ResourceVersion}, "spec": map[string]any{"replicas": *n}})
		err = h.patch(ctx, body)
	case cmd.Action == protocol.ActionWorkloadDelete && h.meta.Labels[render.LabelManagedBy] == render.ManagedBy:
		// A KyYard-managed workload is removed through its application.
		return protocol.OutcomeDenied, "application_managed"
	case cmd.Action == protocol.ActionWorkloadDelete && h.deleteDeletesClaims():
		return protocol.OutcomeDenied, "pvc_retention"
	default:
		pre := metav1.Preconditions{UID: &h.meta.UID}
		if cmd.Action == protocol.ActionWorkloadDelete {
			// A change since the checks above, a retention policy included, is a 409.
			pre.ResourceVersion = &h.meta.ResourceVersion
		}
		background := metav1.DeletePropagationBackground
		if err = h.remove(ctx, metav1.DeleteOptions{Preconditions: &pre, PropagationPolicy: &background}); apierrors.IsNotFound(err) {
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

// operateFailure classifies a call that did not succeed, in fixed words: a conflict is a
// precondition, an object changed or recreated under the name since the read.
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
// rollout within req.Deadline. The precondition, after an access review for patch (forbidden)
// and the namespace's Pod Security level (pod_security, as a deploy), refuses a managed object
// (application_managed), one at another resourceVersion than the read or without a named
// container (conflict), one whose read is not whole (configuration_unreported: an apply
// would drop what it did not show) and replicas that delete claims, as a scale (pvc_retention). The write is a strategic
// merge patch of only the changed paths (see bind), so fields the form does not carry, those it
// carries unchanged and those this client's types do not know keep their exact bytes. It carries
// the read's resourceVersion, so a concurrent write is the API server's 409, reported conflict. A
// StatefulSet's or DaemonSet's rollout, and a paused Deployment's, is not waited for: rollout is
// skipped. started is called once, before the write. A create frame runs createWorkload instead.
func (c *Client) ApplyWorkload(parent context.Context, req protocol.WorkloadApply, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Request, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.Validate(time.Now()); err != nil {
		return refused(res, err)
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	ref := req.Target
	r := &run{c: c, parent: parent, res: res, started: started, namespace: ref.Namespace}
	if req.Create {
		return r.createWorkload(ctx, req)
	}
	var h *handle
	service := protocol.WorkloadApplyService
	r.step(service, protocol.StepPrecondition, func() (string, string, string) {
		gr := workloadResources[ref.Kind]
		ok, err := r.review(ctx, authorizationv1.ResourceAttributes{Namespace: ref.Namespace, Verb: "patch", Group: gr[0], Resource: gr[1], Name: ref.Name})
		if err != nil {
			return r.failure(ctx, err)
		}
		if !ok {
			return protocol.OutcomeDenied, "forbidden", ""
		}
		if o, code, detail := r.podSecurity(ctx); o != protocol.OutcomeSucceeded {
			return o, code, detail
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
		case h.scaleDeletesClaims(req.Spec.Replicas):
			return protocol.OutcomeDenied, "pvc_retention", ""
		case !edit(h, req.Spec):
			return protocol.OutcomeDenied, "conflict", ""
		}
		return succeeded()
	})
	r.step(service, protocol.StepApply, func() (string, string, string) {
		r.begin()
		err := h.write(ctx)
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

// createWorkload runs a create frame: precondition (the create grant, Pod Security, the name
// free), create of renderWorkload's Deployment, and its rollout. A name that exists, or is taken
// before the create lands, is name_taken: nothing is overwritten.
func (r *run) createWorkload(ctx context.Context, req protocol.WorkloadApply) protocol.DeploymentResult {
	ref, service := req.Target, protocol.WorkloadApplyService
	api := r.c.cs.AppsV1().Deployments(ref.Namespace)
	r.step(service, protocol.StepPrecondition, func() (string, string, string) {
		if o, code, detail := r.allowed(ctx); o != protocol.OutcomeSucceeded {
			return o, code, detail
		}
		if o, code, detail := r.podSecurity(ctx); o != protocol.OutcomeSucceeded {
			return o, code, detail
		}
		if _, err := api.Get(ctx, ref.Name, metav1.GetOptions{}); err == nil {
			return nameTaken("Deployment", ref.Name)
		} else if !apierrors.IsNotFound(err) {
			return r.failure(ctx, err)
		}
		return succeeded()
	})
	r.step(service, protocol.StepCreate, func() (string, string, string) {
		r.begin()
		_, err := api.Create(ctx, renderWorkload(ref, req.Spec), metav1.CreateOptions{})
		switch {
		case err == nil:
			return succeeded()
		case apierrors.IsAlreadyExists(err):
			return nameTaken("Deployment", ref.Name)
		case apierrors.IsForbidden(err):
			return protocol.OutcomeDenied, "admission_denied", ""
		}
		return r.failure(ctx, err)
	})
	r.step(service, protocol.StepRollout, func() (string, string, string) {
		return r.rollout(ctx, ref.Name, func(d *appsv1.Deployment) string { return reasons(conditionReasons(d)) })
	})
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

// renderWorkload is the Deployment a run creates: the spec's replicas, strategy (RollingUpdate
// when unset) and containers under the name's own app.kubernetes.io labels, not KyYard's
// managed-by, so the workload page edits it afterwards. Nothing else is set.
func renderWorkload(ref protocol.WorkloadRef, spec protocol.WorkloadConfiguration) *appsv1.Deployment {
	labels := func() map[string]string {
		return map[string]string{render.LabelName: ref.Name, render.LabelInstanceName: ref.Name}
	}
	strategy := appsv1.RollingUpdateDeploymentStrategyType
	if spec.Strategy != "" {
		strategy = appsv1.DeploymentStrategyType(spec.Strategy)
	}
	containers := make([]corev1.Container, 0, len(spec.Containers))
	for _, w := range spec.Containers {
		c := corev1.Container{Name: w.Name, Image: w.Image, Command: w.Command, Args: w.Args, Env: envVars(nil, w.Env)}
		setQuantity(&c.Resources.Requests, corev1.ResourceCPU, "", w.Resources.CPURequest)
		setQuantity(&c.Resources.Limits, corev1.ResourceCPU, "", w.Resources.CPULimit)
		setQuantity(&c.Resources.Requests, corev1.ResourceMemory, "", w.Resources.MemoryRequest)
		setQuantity(&c.Resources.Limits, corev1.ResourceMemory, "", w.Resources.MemoryLimit)
		containers = append(containers, c)
	}
	n, automount := *spec.Replicas, false
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name, Labels: labels()},
		Spec: appsv1.DeploymentSpec{
			Replicas: &n,
			Selector: &metav1.LabelSelector{MatchLabels: labels()},
			Strategy: appsv1.DeploymentStrategy{Type: strategy},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels()},
				Spec:       corev1.PodSpec{Containers: containers, RestartPolicy: corev1.RestartPolicyAlways, AutomountServiceAccountToken: &automount},
			},
		},
	}
}
