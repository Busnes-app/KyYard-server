package kubernetes

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
