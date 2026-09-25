package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// identityKey is the Secret's one data key.
const identityKey = "identity.json"

// ErrIdentityConflict is a Secret that changed under this agent twice running, or that holds
// another endpoint's identity: the sign of two agents sharing one identity, which this store
// exists to prevent. The caller stops.
var ErrIdentityConflict = errors.New("the identity Secret changed under this agent; another agent may share this identity")

// SecretIdentityStore keeps the identity in a Secret in the agent's own namespace, so a
// restarted pod comes back as the same endpoint instead of enrolling again. It writes against
// the Secret it last read or wrote, so a second agent's write since then is a conflict.
type SecretIdentityStore struct {
	secrets typedcorev1.SecretInterface
	name    string
	mu      sync.Mutex
	seen    *corev1.Secret // as of the last Load or Save; nil when absent
}

func NewSecretIdentityStore(c *Client, namespace, name string) (*SecretIdentityStore, error) {
	if !protocol.ValidDNSLabel(namespace) || !protocol.ValidDNSSubdomain(name) {
		return nil, errors.New("the identity Secret needs a valid namespace and name")
	}
	return &SecretIdentityStore{secrets: c.cs.CoreV1().Secrets(namespace), name: name}, nil
}

func (s *SecretIdentityStore) Load() (*client.Identity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callBudget)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.read(ctx); err != nil || s.seen == nil {
		return nil, err
	}
	return client.DecodeIdentity(s.seen.Data[identityKey])
}

// Save updates the Secret as last seen, creating it only when it was absent. On a conflict it
// re-reads once: a Secret holding another endpoint or a later generation is
// ErrIdentityConflict, anything else is retried once, and a second conflict is
// ErrIdentityConflict. A Secret holding another endpoint's identity is never overwritten.
func (s *SecretIdentityStore) Save(id *client.Identity) error {
	raw, err := json.Marshal(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), callBudget)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overtakes(id) {
		return ErrIdentityConflict
	}
	err = s.write(ctx, raw)
	if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
		return err
	}
	if err := s.read(ctx); err != nil {
		return err
	}
	if s.overtakes(id) {
		return ErrIdentityConflict
	}
	err = s.write(ctx, raw)
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return ErrIdentityConflict
	}
	return err
}

// overtakes reports whether the Secret as last seen holds another endpoint or a later
// generation than id. An undecodable Secret is overwritten.
func (s *SecretIdentityStore) overtakes(id *client.Identity) bool {
	if s.seen == nil {
		return false
	}
	prior, err := client.DecodeIdentity(s.seen.Data[identityKey])
	return err == nil && (prior.EndpointID != id.EndpointID || prior.Generation > id.Generation)
}

func (s *SecretIdentityStore) read(ctx context.Context) error {
	sec, err := s.secrets.Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		s.seen = nil
		return nil
	}
	if err != nil {
		return err
	}
	s.seen = sec
	return nil
}

func (s *SecretIdentityStore) write(ctx context.Context, raw []byte) error {
	var (
		sec *corev1.Secret
		err error
	)
	if s.seen == nil {
		sec, err = s.secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: s.name, Labels: map[string]string{"app.kubernetes.io/name": "kyyard-agent", "app.kubernetes.io/managed-by": "kyyard"}},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{identityKey: raw},
		}, metav1.CreateOptions{})
	} else {
		sec = s.seen.DeepCopy()
		sec.Data = map[string][]byte{identityKey: raw}
		sec, err = s.secrets.Update(ctx, sec, metav1.UpdateOptions{})
	}
	if err == nil {
		s.seen = sec
	}
	return err
}
