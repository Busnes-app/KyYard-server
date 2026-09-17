package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// An unacknowledged rotated key expires: it stops being "pending", cannot be acknowledged, and a
// new rotation may replace it.
func TestPendingRotationExpires(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	e, err := ts.Enroll(ctx, EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "host"})
	if err != nil {
		t.Fatal(err)
	}
	org := TenantAccess{ActorID: a.ActorID, OrganizationID: a.OrganizationID}
	if err := ts.ApproveEndpoint(ctx, org, e.ID, e.Fingerprint); err != nil {
		t.Fatal(err)
	}
	newPub, _, _ := ed25519.GenerateKey(rand.Reader)
	sig := ed25519.Sign(priv, protocol.Preimage(protocol.ContextRotate, protocol.RawFingerprint(pub), newPub))
	fp, err := ts.RotateEndpointKey(ctx, e.ID, newPub, sig, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_keys SET created_at=? WHERE fingerprint=?`), time.Now().Add(-8*24*time.Hour), fp); err != nil {
		t.Fatal(err)
	}
	if err := ts.AcknowledgeEndpointKey(ctx, org, e.ID, fp); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired pending key acknowledged: %v", err)
	}
	if view, err := ts.ReadEndpoint(ctx, org, e.ID); err != nil || view.PendingFingerprint != "" {
		t.Fatalf("expired pending key still offered for acknowledgement: %v %+v", err, view)
	}
	if _, err := ts.AgentIdentity(ctx, e.ID, fp); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("expired pending key state: %v", err)
	}
	another, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := ts.RotateEndpointKey(ctx, e.ID, another, ed25519.Sign(priv, protocol.Preimage(protocol.ContextRotate, protocol.RawFingerprint(pub), another)), ""); err != nil {
		t.Fatalf("rotation after expiry refused: %v", err)
	}
}
