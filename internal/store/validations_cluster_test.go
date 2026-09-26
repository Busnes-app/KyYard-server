package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const (
	podA = "aaaaaaaa-1111-4111-8111-111111111111"
	podB = "bbbbbbbb-2222-4222-8222-222222222222"
)

// clusterApply plans, applies and settles the latest revision of a kubernetesPlanFixture
// application with web resolved to digest, the identities at generation 1.
func clusterApply(t *testing.T, st *SQLStore, a TenantAccess, app *Application, cluster, digest string) *Deployment {
	t.Helper()
	ctx := context.Background()
	ts := st.Tenancy()
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	putClusterInventory(t, ts, cluster, nil)
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digest}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, m.Preview.Project, imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}}
	for _, ps := range d.Plan.Services {
		res.Services = append(res.Services, kubeIdentity(ps))
	}
	if err := ts.SettleDeployment(ctx, cluster, res); err != nil {
		t.Fatal(err)
	}
	return d
}

// workload is a status read of the web Deployment at generation 1: desired 1, with pods.
func workload(available int32, pods ...protocol.PodStatus) *protocol.ContainerInspection {
	return &protocol.ContainerInspection{Target: protocol.InspectionTarget{Workload: protocol.WorkloadRef{Namespace: "shop", Name: "shop-front-web", UID: kubeUID}},
		Workload: &protocol.WorkloadStatus{UID: kubeUID, Generation: 1, ObservedGeneration: 1, Desired: 1, Updated: 1, Ready: available, Available: available, Pods: pods}}
}

func pod(uid, state, reason string, restarts int32) protocol.PodStatus {
	return protocol.PodStatus{Name: "shop-front-web-" + uid[:4], UID: uid, Phase: "Running", Containers: []protocol.PodContainer{{Name: "web", State: state, Reason: reason, RestartCount: restarts}}}
}

// Every cluster verdict, from a status read against the baseline and the settled generation.
func TestJudgeCluster(t *testing.T) {
	base := map[string]ServiceBaseline{"web": {PodRestarts: map[string]int{podA: 2}}}
	web := func(in *protocol.ContainerInspection) []Observation {
		return []Observation{{Service: "web", Presence: PresenceUnknown, Inspection: in, Generation: 1}}
	}
	with := func(in *protocol.ContainerInspection, f func(*protocol.WorkloadStatus)) *protocol.ContainerInspection {
		f(in.Workload)
		return in
	}
	up := func() *protocol.ContainerInspection { return workload(1, pod(podA, "running", "", 2)) }
	evicted := pod(podA, "terminated", "Evicted", 2)
	evicted.Phase = "Failed"
	for _, tc := range []struct {
		name            string
		obs             []Observation
		final           bool
		verdict, detail string
	}{
		{"available before the end goes on", web(up()), false, "", ""},
		{"available at the end", web(up()), true, VerdictHealthy, ""},
		{"missing", web(&protocol.ContainerInspection{Target: up().Target, Workload: &protocol.WorkloadStatus{Missing: true}}), false, VerdictChanged, "web"},
		{"recreated under the name", web(with(up(), func(w *protocol.WorkloadStatus) { w.UID = podB })), false, VerdictChanged, "web"},
		{"edited past the settled generation", web(with(up(), func(w *protocol.WorkloadStatus) { w.Generation, w.ObservedGeneration = 2, 2 })), false, VerdictChanged, "web"},
		{"recreated by someone else", web(workload(1, pod(podB, "running", "", 0))), false, VerdictChanged, "web"},
		{"a replacement pod that restarts is restarting", web(workload(1, pod(podB, "running", "", 3))), false, VerdictRestarting, "web"},
		{"restarted since the baseline", web(workload(1, pod(podA, "running", "", 3))), false, VerdictRestarting, "web"},
		{"exited and not replaced", web(workload(0, pod(podA, "terminated", "Error", 2))), false, VerdictExited, "web"},
		{"terminated while a container waits goes on", web(workload(0, pod(podA, "terminated", "Error", 2), pod(podB, "waiting", "ContainerCreating", 0))), false, "", ""},
		{"crash loop at once", web(workload(0, pod(podA, "waiting", "CrashLoopBackOff", 2))), false, VerdictUnhealthy, "web:CrashLoopBackOff"},
		{"image pull back-off at once", web(workload(0, pod(podA, "waiting", "ImagePullBackOff", 2))), false, VerdictUnhealthy, "web:ImagePullBackOff"},
		{"image pull error at once", web(workload(0, pod(podA, "waiting", "ErrImagePull", 2))), false, VerdictUnhealthy, "web:ErrImagePull"},
		{"create error at once", web(workload(0, pod(podA, "waiting", "CreateContainerError", 2))), false, VerdictUnhealthy, "web:CreateContainerError"},
		{"config error at once", web(workload(0, pod(podA, "waiting", "CreateContainerConfigError", 2))), false, VerdictUnhealthy, "web:CreateContainerConfigError"},
		{"creating goes on", web(workload(0, pod(podA, "waiting", "ContainerCreating", 2))), false, "", ""},
		{"creating at the end", web(workload(0, pod(podA, "waiting", "ContainerCreating", 2))), true, VerdictUnhealthy, "web"},
		{"unavailable at the end", web(workload(0, pod(podA, "running", "", 2))), true, VerdictUnhealthy, "web"},
		{"not ready at the end", web(with(up(), func(w *protocol.WorkloadStatus) { w.Ready = 0 })), true, VerdictUnhealthy, "web"},
		{"generation not observed at the end", web(with(up(), func(w *protocol.WorkloadStatus) { w.ObservedGeneration = 0 })), true, VerdictUnhealthy, "web"},
		{"an evicted pod beside a replacement not yet available goes on", web(workload(0, evicted, pod(podB, "running", "", 0))), false, "", ""},
		{"an evicted pod beside a replacement still short at the end", web(workload(0, evicted, pod(podB, "running", "", 0))), true, VerdictUnhealthy, "web"},
		{"an evicted pod beside its available replacement", web(workload(1, evicted, pod(podB, "running", "", 0))), true, VerdictHealthy, ""},
		{"unobserved holds the end open", []Observation{{Service: "web", Presence: PresenceUnknown, Generation: 1}}, true, "", ""},
	} {
		if v, d := Judge(tc.obs, base, tc.final); v != tc.verdict || d != tc.detail {
			t.Errorf("%s: %q %q, want %q %q", tc.name, v, d, tc.verdict, tc.detail)
		}
	}
	// Restarts count per pod against its own baseline, so a pod leaving the list hides none.
	api := func(in *protocol.ContainerInspection) Observation {
		return Observation{Service: "api", Presence: PresenceUnknown, Inspection: in, Generation: 1}
	}
	for _, tc := range []struct {
		name            string
		base            map[string]ServiceBaseline
		obs             []Observation
		verdict, detail string
	}{
		{"a new pod restarting after an old one left", map[string]ServiceBaseline{"web": {PodRestarts: map[string]int{podA: 4, podB: 0}}}, web(workload(1, pod(podB, "running", "", 3))), VerdictRestarting, "web"},
		{"a replacement restarting after an evicted pod went", base, web(workload(1, pod(podB, "running", "", 1))), VerdictRestarting, "web"},
		{"a restart outranks a crash loop, without its reason", map[string]ServiceBaseline{"web": base["web"], "api": {PodRestarts: map[string]int{podB: 0}}},
			append(web(workload(1, pod(podA, "running", "", 3))), api(workload(0, pod(podB, "waiting", "CrashLoopBackOff", 0)))), VerdictRestarting, "web"},
		{"a crash loop decides with its reason", map[string]ServiceBaseline{"web": base["web"], "api": {PodRestarts: map[string]int{podB: 0}}},
			append(web(up()), api(workload(0, pod(podB, "waiting", "CrashLoopBackOff", 0)))), VerdictUnhealthy, "api:CrashLoopBackOff"},
	} {
		if v, d := Judge(tc.obs, tc.base, false); v != tc.verdict || d != tc.detail {
			t.Errorf("%s: %q %q, want %q %q", tc.name, v, d, tc.verdict, tc.detail)
		}
	}
}

// A cluster baseline keeps each pod's restarts by UID; a Deployment already missing, recreated
// or edited gives none, so its first poll judges it changed; one wanting pods but listing none
// gives no baseline yet.
func TestBaselineOfACluster(t *testing.T) {
	got, ok := BaselineOf([]Observation{{Service: "web", Presence: PresenceUnknown, Generation: 1, Inspection: workload(1, pod(podA, "running", "", 2), pod(podB, "running", "", 3))}})
	if !ok || !reflect.DeepEqual(got["web"], ServiceBaseline{PodRestarts: map[string]int{podA: 2, podB: 3}}) {
		t.Fatalf("baseline: %+v %v", got, ok)
	}
	if got, ok := BaselineOf([]Observation{{Service: "web", Presence: PresenceUnknown, Generation: 1, Inspection: workload(0)}}); ok {
		t.Fatalf("no pods listed yet gave a baseline: %+v", got)
	}
	scaledDown := workload(0)
	scaledDown.Workload.Desired, scaledDown.Workload.Updated = 0, 0
	if got, ok := BaselineOf([]Observation{{Service: "web", Presence: PresenceUnknown, Generation: 1, Inspection: scaledDown}}); !ok || len(got) != 1 {
		t.Fatalf("a Deployment wanting no pods: %+v %v", got, ok)
	}
	recreated := workload(1, pod(podB, "running", "", 0))
	recreated.Workload.UID = podB
	for name, in := range map[string]*protocol.ContainerInspection{
		"missing":   {Target: recreated.Target, Workload: &protocol.WorkloadStatus{Missing: true}},
		"recreated": recreated,
		"edited":    func() *protocol.ContainerInspection { in := workload(1); in.Workload.Generation = 2; return in }(),
	} {
		obs := []Observation{{Service: "web", Presence: PresenceUnknown, Generation: 1, Inspection: in}}
		got, ok := BaselineOf(obs)
		if !ok || len(got) != 0 {
			t.Errorf("%s: %+v %v", name, got, ok)
		}
		if v, _ := Judge(obs, got, false); v != VerdictChanged {
			t.Errorf("%s judged %q", name, v)
		}
	}
}

// A settled cluster apply is pending with its Deployment identities placed unknown, marked
// Kubernetes, and Health only once the agent advertises kubernetes.inspect.
func TestPendingValidationsOfACluster(t *testing.T) {
	st, a, app, cluster, _ := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	d := clusterApply(t, st, a, app, cluster, digestOf("b"))
	find := func() PendingValidation {
		t.Helper()
		pending, err := ts.PendingValidations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pending {
			if p.DeploymentID == d.ID {
				return p
			}
		}
		t.Fatal("the cluster validation is not pending")
		return PendingValidation{}
	}
	p := find()
	if !p.Kubernetes || p.Health || len(p.Services) != 2 {
		t.Fatalf("pending: %+v", p)
	}
	for _, s := range p.Services {
		if s.Presence != PresenceUnknown || s.Kind != protocol.KindDeployment || s.UID != kubeUID || s.Generation != 1 {
			t.Fatalf("service %+v", s)
		}
	}
	if err := ts.SetEndpointCapabilities(ctx, cluster, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesInspect}); err != nil {
		t.Fatal(err)
	}
	if p := find(); !p.Kubernetes || !p.Health {
		t.Fatalf("with kubernetes.inspect: %+v", p)
	}
	// The baseline round-trips its per-pod restarts through the column.
	baseline := map[string]ServiceBaseline{"web": {PodRestarts: map[string]int{podA: 1}}, "api": {PodRestarts: map[string]int{podB: 0}}}
	if err := ts.BeginObservation(ctx, d.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if p := find(); !reflect.DeepEqual(p.Baseline, baseline) {
		t.Fatalf("baseline read back: %+v", p.Baseline)
	}
}
