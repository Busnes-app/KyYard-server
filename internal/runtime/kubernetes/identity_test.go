package kubernetes

import (
	"crypto/ed25519"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testIdentity(endpoint string, generation uint64) *client.Identity {
	_, priv, _ := ed25519.GenerateKey(nil)
	return &client.Identity{EndpointID: endpoint, PrivateKey: priv, InstanceFingerprint: strings.Repeat("a", 64), Server: "https://yard.example", Generation: generation}
}

// The first save creates the Secret, later saves update it in place, and a load returns what
// was saved; an absent Secret is a never-enrolled agent.
func TestSecretIdentityStoreCreatesThenUpdates(t *testing.T) {
	c, cs := cluster(t)
	store, err := NewSecretIdentityStore(c, "kyyard-agent", "kyyard-agent-identity")
	if err != nil {
		t.Fatal(err)
	}
	if id, err := store.Load(); id != nil || err != nil {
		t.Fatalf("fresh: %+v %v", id, err)
	}
	if err := store.Save(testIdentity("ep_1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testIdentity("ep_1", 2)); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got.EndpointID != "ep_1" || got.Generation != 2 {
		t.Fatalf("load %+v %v", got, err)
	}
	sec, _ := cs.CoreV1().Secrets("kyyard-agent").Get(t.Context(), "kyyard-agent-identity", metav1.GetOptions{})
	if sec.Type != corev1.SecretTypeOpaque || sec.Labels["app.kubernetes.io/managed-by"] != "kyyard" || len(sec.Data) != 1 {
		t.Fatalf("secret %+v", sec)
	}
	var creates, updates int
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "secrets" {
			switch a.GetVerb() {
			case "create":
				creates++
			case "update":
				updates++
			}
		}
	}
	if creates != 1 || updates != 1 {
		t.Fatalf("%d creates, %d updates", creates, updates)
	}
	if _, err := NewSecretIdentityStore(c, "Kyyard", "x"); err == nil {
		t.Fatal("an invalid namespace was accepted")
	}
}

// One conflicting write is retried; a second is ErrIdentityConflict, and so is a Secret that
// already holds another endpoint's identity, which is never overwritten.
func TestSecretIdentityStoreConflicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		conflicts int
		want      error
		updates   int
	}{
		{"one conflict is retried", 1, nil, 2},
		{"a second conflict stops", 2, ErrIdentityConflict, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cs := cluster(t)
			store, _ := NewSecretIdentityStore(c, "kyyard-agent", "kyyard-agent-identity")
			if err := store.Save(testIdentity("ep_1", 1)); err != nil {
				t.Fatal(err)
			}
			updates := 0
			cs.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				if updates++; updates <= tc.conflicts {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "kyyard-agent-identity", errors.New("changed"))
				}
				return false, nil, nil
			})
			if err := store.Save(testIdentity("ep_1", 2)); !errors.Is(err, tc.want) || updates != tc.updates {
				t.Fatalf("save: %v after %d updates", err, updates)
			}
		})
	}
	c, _ := cluster(t)
	store, _ := NewSecretIdentityStore(c, "kyyard-agent", "kyyard-agent-identity")
	if err := store.Save(testIdentity("ep_other", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testIdentity("ep_1", 1)); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("overwrote another endpoint's identity: %v", err)
	}
	if got, _ := store.Load(); got.EndpointID != "ep_other" {
		t.Fatalf("stored %s", got.EndpointID)
	}
}

// resourceVersions makes the fake clientset's Secrets behave like the API server's: every
// write stamps a new resourceVersion and an update against an older one is a conflict.
func resourceVersions(cs *fake.Clientset) {
	gvr := corev1.SchemeGroupVersion.WithResource("secrets")
	next := 0
	stamp := func(sec *corev1.Secret) *corev1.Secret {
		next++
		sec = sec.DeepCopy()
		sec.ResourceVersion = strconv.Itoa(next)
		return sec
	}
	cs.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sec := stamp(a.(k8stesting.CreateAction).GetObject().(*corev1.Secret))
		return true, sec, cs.Tracker().Create(gvr, sec, a.GetNamespace())
	})
	cs.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sec := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		cur, err := cs.Tracker().Get(gvr, a.GetNamespace(), sec.Name)
		if err != nil {
			return true, nil, err
		}
		if cur.(*corev1.Secret).ResourceVersion != sec.ResourceVersion {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, sec.Name, errors.New("stale resourceVersion"))
		}
		sec = stamp(sec)
		return true, sec, cs.Tracker().Update(gvr, sec, a.GetNamespace())
	})
}

// Two agents holding one identity: the one that loaded earlier and writes an older generation
// stops instead of rolling the counter back.
func TestSecretIdentityStoreStaleWriterStops(t *testing.T) {
	c, cs := cluster(t)
	resourceVersions(cs)
	a, _ := NewSecretIdentityStore(c, "kyyard-agent", "kyyard-agent-identity")
	b, _ := NewSecretIdentityStore(c, "kyyard-agent", "kyyard-agent-identity")
	if err := a.Save(testIdentity("ep_1", 1)); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*SecretIdentityStore{a, b} {
		if _, err := s.Load(); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Save(testIdentity("ep_1", 100)); err != nil {
		t.Fatal(err)
	}
	err := b.Save(testIdentity("ep_1", 2))
	got, _ := a.Load()
	if !errors.Is(err, ErrIdentityConflict) || got.Generation != 100 {
		t.Fatalf("stale writer saved: err=%v, stored generation now %d", err, got.Generation)
	}
}
