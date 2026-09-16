package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
)

// Expiry is a property of stored timestamps, so the test moves them rather than the clock.
func TestEnrollmentExpiryIsEnforcedFromStoredTimestamps(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	req := EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "late"}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE agent_enrollment_tokens SET expires_at=? WHERE id=?`), time.Now().Add(-time.Second), tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Enroll(ctx, req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expired token accepted: %v", err)
	}
	tok, err = ts.CreateEnrollmentToken(ctx, a, "docker")
	if err != nil {
		t.Fatal(err)
	}
	req.Token = tok.Secret
	req.Proof = ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret))
	e, err := ts.Enroll(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoints SET created_at=? WHERE id=?`), time.Now().Add(-25*time.Hour), e.ID); err != nil {
		t.Fatal(err)
	}
	org := TenantAccess{ActorID: a.ActorID, OrganizationID: a.OrganizationID}
	got, err := ts.ReadEndpoint(ctx, org, e.ID)
	if err != nil || got.State != "expired" {
		t.Fatalf("stale pending endpoint not shown expired: %v %+v", err, got)
	}
	if err := ts.ApproveEndpoint(ctx, org, e.ID, e.Fingerprint); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale pending endpoint approved: %v", err)
	}
}
