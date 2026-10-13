package kubernetes

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

func TestInstallerOwnershipRefusesWorkloadMutations(t *testing.T) {
	for _, templateOnly := range []bool{false, true} {
		for _, action := range []string{protocol.ActionWorkloadScale, protocol.ActionWorkloadRestart, protocol.ActionWorkloadDelete} {
			d := webDeployment()
			if templateOnly {
				d.Spec.Template.Labels[installerOwnerLabel] = "kyquickstart"
			} else {
				d.Labels[installerOwnerLabel] = "kyquickstart"
			}
			c, cs := workloadCluster(t, false, true, d)
			cmd := command(action, "shop/deployment/web")
			if action == protocol.ActionWorkloadScale {
				cmd.Expects.Replicas = ptr(int32(0))
			}
			outcome, detail := c.Operate(context.Background(), cmd)
			if outcome != protocol.OutcomeDenied || detail != "application_managed" {
				t.Fatalf("template=%t %s: %s %q", templateOnly, action, outcome, detail)
			}
			if got := mutations(cs); len(got) != 0 {
				t.Fatalf("template=%t %s mutated: %v", templateOnly, action, got)
			}
		}
	}
}

func TestInstallerOwnershipRefusesPodExec(t *testing.T) {
	pod := execPod(nil)
	pod.Labels = map[string]string{installerOwnerLabel: "kyquickstart"}
	c, got := execClient(t, newFake(nil), pod)
	if _, err := c.OpenExec(context.Background(), execSpec()); err == nil {
		t.Fatal("installer-owned pod accepted exec")
	}
	if len(got.Command) != 0 {
		t.Fatal("installer-owned pod reached executor")
	}
}

// controlledBy is a controller reference to kind name with uid.
func controlledBy(apiVersion, kind, name string, uid types.UID) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: ptr(true)}}
}

// A running pod without the label, whose controller chain carries it only on a controller's
// metadata, is installer-owned: exec and pod delete are refused before the executor or any
// delete, with every other check passing. An owner that is unreadable, gone, of another UID or
// of an unknown kind refuses too; an unlabelled chain is allowed.
func TestInstallerOwnershipFollowsPodControllers(t *testing.T) {
	managed := map[string]string{installerOwnerLabel: "kyquickstart"}
	deployment := func(labels map[string]string) runtime.Object {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web", UID: "dep-uid", Labels: labels}}
	}
	replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-7c9d", UID: "rs-uid",
		OwnerReferences: controlledBy("apps/v1", "Deployment", "web", "dep-uid")}}
	toReplicaSet := controlledBy("apps/v1", "ReplicaSet", "web-7c9d", "rs-uid")
	for _, tc := range []struct {
		name    string
		owners  []metav1.OwnerReference
		objects []runtime.Object
		forbid  string // a resource whose get the API server refuses
		allowed bool
	}{
		{name: "deployment labelled through its replicaset", owners: toReplicaSet, objects: []runtime.Object{replicaSet, deployment(managed)}},
		{name: "cronjob labelled through its job", owners: controlledBy("batch/v1", "Job", "nightly-1", "job-uid"), objects: []runtime.Object{
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly-1", UID: "job-uid", OwnerReferences: controlledBy("batch/v1", "CronJob", "nightly", "cj-uid")}},
			&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly", UID: "cj-uid", Labels: managed}}}},
		{name: "statefulset labelled", owners: controlledBy("apps/v1", "StatefulSet", "db", "sts-uid"), objects: []runtime.Object{
			&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db", UID: "sts-uid", Labels: managed}}}},
		{name: "owner gone", owners: toReplicaSet, objects: []runtime.Object{deployment(nil)}},
		{name: "owner replaced under its name", owners: controlledBy("apps/v1", "ReplicaSet", "web-7c9d", "old-uid"), objects: []runtime.Object{replicaSet, deployment(nil)}},
		{name: "owner unreadable", owners: toReplicaSet, objects: []runtime.Object{replicaSet, deployment(nil)}, forbid: "replicasets"},
		{name: "owner of an unknown kind", owners: controlledBy("example.com/v1", "Database", "db", "db-uid")},
		{name: "unlabelled chain", owners: toReplicaSet, objects: []runtime.Object{replicaSet, deployment(nil)}, allowed: true},
		{name: "no controller", allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := execPod(func(p *corev1.Pod) { p.OwnerReferences = tc.owners })
			c, cs := workloadCluster(t, false, true, append([]runtime.Object{pod}, tc.objects...)...)
			if tc.forbid != "" {
				cs.PrependReactor("get", tc.forbid, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: tc.forbid}, "", nil)
				})
			}
			executed := false
			c.executor = func(string, string, *corev1.PodExecOptions) (streamer, error) {
				executed = true
				return newFake(nil), nil
			}
			s, err := c.OpenExec(context.Background(), execSpec())
			if err == nil {
				s.Close()
			}
			if (err == nil) != tc.allowed || executed != tc.allowed || err != nil && !strings.Contains(err.Error(), "KyQuickStart-owned") {
				t.Errorf("exec: err %v, executor reached %t", err, executed)
			}
			outcome, detail := c.Operate(context.Background(), command(protocol.ActionPodDelete, "shop/pod/web-0"))
			deleted := slices.Contains(mutations(cs), "delete pods")
			if tc.allowed {
				if outcome != protocol.OutcomeSucceeded || !deleted {
					t.Errorf("delete: %s %q, deleted %t", outcome, detail, deleted)
				}
			} else if outcome != protocol.OutcomeDenied || detail != "application_managed" || deleted {
				t.Errorf("delete: %s %q, deleted %t", outcome, detail, deleted)
			}
		})
	}
}
