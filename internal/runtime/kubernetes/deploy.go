package kubernetes

import (
	"cmp"
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// rolloutPoll is how often a start step reads its Deployment while waiting for the rollout.
const rolloutPoll = 2 * time.Second

// Deploy applies each service's objects and waits for its rollout. First every service's
// precondition: the agent's own grant (a SelfSubjectAccessReview for create deployments in the
// namespace, forbidden when denied), then each planned object read by name, refused name_taken
// when one exists without this instance's label. Then per service create (ConfigMap, Secret,
// Deployment, Service, each updated when it exists and is owned, created otherwise; one conflict
// is re-read and retried) and start (the rollout, polled until ready or the deadline:
// rollout_timeout). The first step that is not a success ends the run and every later step is
// skipped. Nothing is rolled back: a timed-out rollout leaves the objects as applied. started is
// called once, before the first write.
func (c *Client) Deploy(parent context.Context, req protocol.DeploymentRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		return refused(res, err)
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	r := &run{c: c, parent: parent, res: res, started: started, namespace: req.Kubernetes.Namespace, instance: req.Kubernetes.InstanceID}
	sets := render.Request(req)
	have := make([]existing, len(sets))
	for i, set := range sets {
		r.step(set.Service, protocol.StepPrecondition, func() (string, string, string) {
			if i == 0 {
				if o, code, detail := r.allowed(ctx); o != protocol.OutcomeSucceeded {
					return o, code, detail
				}
			}
			var o, code, detail string
			have[i], o, code, detail = r.read(ctx, set)
			return o, code, detail
		})
	}
	for i, set := range sets {
		r.step(set.Service, protocol.StepCreate, func() (string, string, string) {
			r.begin()
			d, o, code, detail := r.apply(ctx, set, have[i])
			if o == protocol.OutcomeSucceeded {
				r.res.Services = append(r.res.Services, protocol.DeploymentIdentity{Service: set.Service, Kind: protocol.KindDeployment, Namespace: d.Namespace, Name: d.Name, UID: string(d.UID), Generation: d.Generation, ImageDigest: req.Services[i].Pull.Digest})
			}
			return o, code, detail
		})
		r.step(set.Service, protocol.StepStart, func() (string, string, string) { return r.rollout(ctx, set) })
	}
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

type run struct {
	c         *Client
	parent    context.Context
	res       protocol.DeploymentResult
	started   func()
	begun     bool
	namespace string
	instance  string
}

// begin tells the caller, once, that the run is about to write to the cluster.
func (r *run) begin() {
	if !r.begun {
		r.begun = true
		r.started()
	}
}

// step records one outcome; the first that is not a success fixes the run's outcome, coded
// step_failed, and every later step is skipped.
func (r *run) step(service, step string, do func() (outcome, code, detail string)) {
	if r.res.Outcome != "" {
		r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: service, Step: step, Outcome: protocol.OutcomeSkipped})
		return
	}
	outcome, code, detail := do()
	s := protocol.DeploymentStep{Service: service, Step: step, Outcome: outcome}
	if outcome != protocol.OutcomeSucceeded && outcome != protocol.OutcomeSkipped {
		s.Code, s.Detail = code, detail
		r.res.Outcome, r.res.Code = outcome, protocol.ResultStepFailed
	}
	r.res.Steps = append(r.res.Steps, s)
}

// refused answers a frame ValidateFor refused, with no step run.
func refused(res protocol.DeploymentResult, err error) protocol.DeploymentResult {
	if !protocol.ValidRequestID(res.RequestID) {
		res.RequestID = ""
	}
	res.Outcome, res.Code = protocol.OutcomeDenied, protocol.ResultInvalidRequest
	if errors.Is(err, protocol.ErrClockSkew) {
		res.Outcome, res.Code = protocol.OutcomeFailed, protocol.ResultClockSkew
	}
	return res
}

func succeeded() (string, string, string) { return protocol.OutcomeSucceeded, "", "" }

// failure classifies an API call that did not succeed. A status is an answer; with none, a
// cancelled parent means the agent is stopping and the API server may have acted.
func (r *run) failure(ctx context.Context, err error) (string, string, string) {
	var status apierrors.APIStatus
	switch {
	case apierrors.IsForbidden(err):
		return protocol.OutcomeDenied, "forbidden", ""
	case errors.As(err, &status) && status.Status().Code >= 100 && status.Status().Code <= 599:
		return protocol.OutcomeFailed, "runtime_status", strconv.Itoa(int(status.Status().Code))
	case errors.Is(r.parent.Err(), context.Canceled):
		return protocol.OutcomeUnknown, "cancelled", ""
	case ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "runtime_timeout", ""
	}
	return protocol.OutcomeFailed, "runtime_error", ""
}

// allowed asks the API server whether the agent may create Deployments in the namespace: the
// grant, not the namespace list the server stores, decides.
func (r *run) allowed(ctx context.Context) (string, string, string) {
	review, err := r.c.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: r.namespace, Verb: "create", Group: "apps", Resource: "deployments"},
	}}, metav1.CreateOptions{})
	if err != nil {
		return r.failure(ctx, err)
	}
	if !review.Status.Allowed {
		return protocol.OutcomeDenied, "forbidden", ""
	}
	return succeeded()
}

// existing is what a precondition found under a service's planned names: nil where nothing is.
type existing struct {
	configMap  *corev1.ConfigMap
	secret     *corev1.Secret
	deployment *appsv1.Deployment
	service    *corev1.Service
}

func (r *run) owned(o metav1.Object) bool { return o.GetLabels()[render.LabelInstance] == r.instance }

func nameTaken(kind, name string) (string, string, string) {
	return protocol.OutcomeDenied, "name_taken", kind + "/" + name
}

// objectAPI is the part of a typed client a service's objects are applied through.
type objectAPI[T any] interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (T, error)
	Create(ctx context.Context, obj T, opts metav1.CreateOptions) (T, error)
	Update(ctx context.Context, obj T, opts metav1.UpdateOptions) (T, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// get reads one object by name: found false when it does not exist.
func get[T metav1.Object](ctx context.Context, api objectAPI[T], name string) (T, bool, error) {
	o, err := api.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		var zero T
		return zero, false, nil
	}
	return o, err == nil, err
}

// read is a service's precondition: each planned object, owned or absent.
func (r *run) read(ctx context.Context, set render.Set) (existing, string, string, string) {
	var e existing
	core, apps := r.c.cs.CoreV1(), r.c.cs.AppsV1()
	check := func(kind, name string, o metav1.Object, found bool, err error) (string, string, string) {
		switch {
		case err != nil:
			return r.failure(ctx, err)
		case found && !r.owned(o):
			return nameTaken(kind, name)
		}
		return succeeded()
	}
	cm, found, err := get(ctx, objectAPI[*corev1.ConfigMap](core.ConfigMaps(r.namespace)), set.ConfigMap.Name)
	if o, code, detail := check("ConfigMap", set.ConfigMap.Name, cm, found, err); o != protocol.OutcomeSucceeded {
		return e, o, code, detail
	} else if found {
		e.configMap = cm
	}
	if set.Secret != nil {
		s, found, err := get(ctx, objectAPI[*corev1.Secret](core.Secrets(r.namespace)), set.Secret.Name)
		if o, code, detail := check("Secret", set.Secret.Name, s, found, err); o != protocol.OutcomeSucceeded {
			return e, o, code, detail
		} else if found {
			e.secret = s
		}
	}
	d, found, err := get(ctx, objectAPI[*appsv1.Deployment](apps.Deployments(r.namespace)), set.Deployment.Name)
	if o, code, detail := check("Deployment", set.Deployment.Name, d, found, err); o != protocol.OutcomeSucceeded {
		return e, o, code, detail
	} else if found {
		e.deployment = d
	}
	if set.Endpoint != nil {
		s, found, err := get(ctx, objectAPI[*corev1.Service](core.Services(r.namespace)), set.Endpoint.Name)
		if o, code, detail := check("Service", set.Endpoint.Name, s, found, err); o != protocol.OutcomeSucceeded {
			return e, o, code, detail
		} else if found {
			e.service = s
		}
	}
	o, code, detail := succeeded()
	return e, o, code, detail
}

// apply writes a service's objects in order and returns the Deployment as written.
func (r *run) apply(ctx context.Context, set render.Set, e existing) (*appsv1.Deployment, string, string, string) {
	core, apps := r.c.cs.CoreV1(), r.c.cs.AppsV1()
	if _, o, code, detail := upsert(ctx, r, "ConfigMap", core.ConfigMaps(r.namespace), set.ConfigMap, e.configMap, nil); o != protocol.OutcomeSucceeded {
		return nil, o, code, detail
	}
	if set.Secret != nil {
		if _, o, code, detail := upsert(ctx, r, "Secret", core.Secrets(r.namespace), set.Secret, e.secret, nil); o != protocol.OutcomeSucceeded {
			return nil, o, code, detail
		}
	}
	d, o, code, detail := upsert(ctx, r, "Deployment", apps.Deployments(r.namespace), set.Deployment, e.deployment, nil)
	if o != protocol.OutcomeSucceeded {
		return nil, o, code, detail
	}
	if set.Endpoint != nil {
		// A Service's cluster IP is immutable: an update keeps the one it was given.
		keep := func(want, have *corev1.Service) {
			want.Spec.ClusterIP, want.Spec.ClusterIPs, want.Spec.IPFamilies, want.Spec.IPFamilyPolicy = have.Spec.ClusterIP, have.Spec.ClusterIPs, have.Spec.IPFamilies, have.Spec.IPFamilyPolicy
		}
		if _, o, code, detail := upsert(ctx, r, "Service", core.Services(r.namespace), set.Endpoint, e.service, keep); o != protocol.OutcomeSucceeded {
			return nil, o, code, detail
		}
	}
	return d, protocol.OutcomeSucceeded, "", ""
}

// upsert updates want over have, an owned object read at the precondition (nil: create it),
// keeping have's resourceVersion. A conflict, or an object that appeared since, is re-read once:
// still owned, it is written again; a second conflict fails with conflict.
func upsert[T interface {
	metav1.Object
	comparable
}](ctx context.Context, r *run, kind string, api objectAPI[T], want, have T, keep func(want, have T)) (T, string, string, string) {
	var zero T
	for attempt := 0; ; attempt++ {
		var got T
		var err error
		if have == zero {
			want.SetResourceVersion("")
			got, err = api.Create(ctx, want, metav1.CreateOptions{})
		} else {
			want.SetResourceVersion(have.GetResourceVersion())
			if keep != nil {
				keep(want, have)
			}
			got, err = api.Update(ctx, want, metav1.UpdateOptions{})
		}
		if err == nil {
			return got, protocol.OutcomeSucceeded, "", ""
		}
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			o, code, detail := r.failure(ctx, err)
			return zero, o, code, detail
		}
		if attempt == 1 {
			return zero, protocol.OutcomeFailed, "conflict", kind + "/" + want.GetName()
		}
		fresh, found, err := get(ctx, api, want.GetName())
		switch {
		case err != nil:
			o, code, detail := r.failure(ctx, err)
			return zero, o, code, detail
		case !found:
			have = zero
		case !r.owned(fresh):
			o, code, detail := nameTaken(kind, want.GetName())
			return zero, o, code, detail
		default:
			have = fresh
		}
	}
}

// rollout waits until the Deployment's controller has seen the latest generation and its one
// replica is updated and ready, reading it every poll within the request's deadline.
func (r *run) rollout(ctx context.Context, set render.Set) (string, string, string) {
	api := r.c.cs.AppsV1().Deployments(r.namespace)
	var last *appsv1.Deployment
	for {
		d, err := api.Get(ctx, set.Deployment.Name, metav1.GetOptions{})
		switch {
		case err == nil:
			last = d
			if ready(d) {
				return succeeded()
			}
		case ctx.Err() == nil:
			return r.failure(ctx, err)
		}
		select {
		case <-ctx.Done():
			if errors.Is(r.parent.Err(), context.Canceled) {
				return protocol.OutcomeUnknown, "cancelled", ""
			}
			return protocol.OutcomeTimedOut, "rollout_timeout", r.stalled(set, last)
		case <-time.After(r.c.poll):
		}
	}
}

func ready(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation && s.Replicas == want && s.UpdatedReplicas == want && s.ReadyReplicas == want
}

// reasonWord is the only shape of reason a rollout_timeout detail carries: a Kubernetes reason
// is a CamelCase word, and anything else is dropped rather than passed on.
var reasonWord = regexp.MustCompile(`^[A-Za-z]{1,64}$`)

// stalled says why a rollout did not finish: the Deployment's Progressing reason, its Available
// reason when unavailable, and the newest pod's waiting reason, as far as each can be read in
// a few seconds past the deadline.
func (r *run) stalled(set render.Set, d *appsv1.Deployment) string {
	var parts []string
	if d != nil {
		for _, c := range d.Status.Conditions {
			switch {
			case c.Type == appsv1.DeploymentProgressing && reasonWord.MatchString(c.Reason):
				parts = append(parts, "progressing="+c.Reason)
			case c.Type == appsv1.DeploymentAvailable && c.Status != corev1.ConditionTrue && reasonWord.MatchString(c.Reason):
				parts = append(parts, "available="+c.Reason)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.parent), 5*time.Second)
	defer cancel()
	pods, err := r.c.cs.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(render.Selector(r.instance, set.Service)).String()})
	if err == nil && len(pods.Items) > 0 {
		newest := slices.MaxFunc(pods.Items, func(a, b corev1.Pod) int {
			return cmp.Compare(a.CreationTimestamp.UnixNano(), b.CreationTimestamp.UnixNano())
		})
		for _, s := range newest.Status.ContainerStatuses {
			if s.State.Waiting != nil && reasonWord.MatchString(s.State.Waiting.Reason) {
				parts = append(parts, "pod="+s.State.Waiting.Reason)
				break
			}
		}
	}
	return strings.Join(parts[:min(len(parts), 3)], ",")
}
