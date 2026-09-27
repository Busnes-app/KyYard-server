package kubernetes

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	k8stesting "k8s.io/client-go/testing"
)

func refusedServiceIP(name string) error {
	return apierrors.NewInvalid(schema.GroupKind{Kind: "Service"}, name, field.ErrorList{field.Invalid(field.NewPath("spec", "clusterIPs").Index(0), "10.96.0.40", "secret-canary allocator message")})
}

func TestStaticServiceIPCreateAndPreserve(t *testing.T) {
	ctx := context.Background()
	for _, ip := range []string{"10.96.0.40", "fd00::40"} {
		t.Run(ip, func(t *testing.T) {
			c, cs := deployCluster(t, true, false)
			req := deployRequest(time.Minute)
			req.Services[0].ClusterIP = ip
			for range 2 {
				if res := c.Deploy(ctx, req, func() {}); res.Outcome != protocol.OutcomeSucceeded {
					t.Fatalf("apply: %+v", res)
				}
			}
			// Automatic allocation on a later revision must also keep the same IP.
			req.Services[0].ClusterIP = ""
			if res := c.Deploy(ctx, req, func() {}); res.Outcome != protocol.OutcomeSucceeded {
				t.Fatalf("automatic reapply: %+v", res)
			}
			s, err := cs.CoreV1().Services("shop").Get(ctx, "shop-web", metav1.GetOptions{})
			if err != nil || s.Spec.ClusterIP != ip {
				t.Fatalf("Service: %+v %v", s, err)
			}
			observed := service(*s)
			if observed.Instance != testInstance || observed.Service != "web" || observed.ClusterIP != ip {
				t.Fatalf("inventory: %+v", observed)
			}
		})
	}
}

func TestStaticServiceIPRefusalsBeforeWorkloadWrites(t *testing.T) {
	for _, phase := range []string{"allocation", "immutable", "conflict-retry"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			req := deployRequest(time.Minute)
			req.Services[0].ClusterIP = "10.96.0.40"
			var objects []runtime.Object
			if phase == "immutable" || phase == "conflict-retry" {
				svc := render.Request(req)[0].Endpoint
				if phase == "immutable" {
					svc.Spec.ClusterIP = "10.96.0.41"
				}
				objects = append(objects, svc)
			}
			c, cs := deployCluster(t, true, false, objects...)
			cs.PrependReactor("create", "services", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if phase == "allocation" {
					return true, nil, refusedServiceIP("shop-web")
				}
				return false, nil, nil
			})
			cs.PrependReactor("update", "services", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if phase != "conflict-retry" {
					return false, nil, nil
				}
				svc := a.(k8stesting.UpdateAction).GetObject().(*corev1.Service).DeepCopy()
				svc.Spec.ClusterIP = "10.96.0.41"
				if err := cs.Tracker().Update(schema.GroupVersionResource{Version: "v1", Resource: "services"}, svc, "shop"); err != nil {
					t.Fatal(err)
				}
				return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "services"}, svc.Name, nil)
			})
			res := c.Deploy(ctx, req, func() {})
			want := "service_ip_unavailable"
			if phase == "immutable" || phase == "conflict-retry" {
				want = "service_ip_immutable"
			}
			if res.Outcome != protocol.OutcomeDenied || res.Validate() != nil || !slices.ContainsFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code == want && s.Detail == "Service/shop-web" }) {
				t.Fatalf("refusal: %+v", res)
			}
			for _, a := range cs.Actions() {
				if (a.GetVerb() == "create" || a.GetVerb() == "update" || a.GetVerb() == "delete") && slices.Contains([]string{"deployments", "secrets", "configmaps", "persistentvolumeclaims"}, a.GetResource().Resource) {
					t.Fatalf("workload write after refused IP: %+v", a)
				}
			}
		})
	}
}
