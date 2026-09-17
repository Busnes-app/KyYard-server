package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"math"
	"testing"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
)

// An implausible generation is refused and cannot pin the inventory; approval resets it.
func TestImplausibleGenerationIsRefusedAndApprovalResets(t *testing.T) {
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
	body := []byte(`{"generation":1,"containers":[],"images":[],"networks":[],"volumes":[]}`)
	if ok, err := ts.AcceptInventory(ctx, e.ID, math.MaxInt64, time.Now(), body); err != nil || ok {
		t.Fatalf("far-future generation accepted: %v %v", ok, err)
	}
	if ok, err := ts.AcceptInventory(ctx, e.ID, uint64(time.Now().Add(2*time.Hour).Unix()), time.Now(), body); err != nil || ok {
		t.Fatalf("generation past the skew window accepted: %v %v", ok, err)
	}
	now := uint64(time.Now().Unix())
	if ok, err := ts.AcceptInventory(ctx, e.ID, now, time.Now(), body); err != nil || !ok {
		t.Fatalf("plausible generation refused after a rejected one: %v %v", ok, err)
	}
	if _, err := ts.ReadInventory(ctx, org, e.ID); err != nil {
		t.Fatalf("inventory not stored: %v", err)
	}
	// Revoke and a fresh enrollment path are operator-reachable resets.
	if err := ts.RevokeEndpoint(ctx, org, e.ID); err != nil {
		t.Fatal(err)
	}
	var gen uint64
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT inventory_generation FROM endpoints WHERE id=?`), e.ID).Scan(&gen); err != nil || gen != 0 {
		t.Fatalf("revocation did not reset the generation: %d %v", gen, err)
	}
	if _, err := ts.ReadInventory(ctx, org, e.ID); err != ErrNotFound {
		t.Fatalf("revocation did not clear the stored snapshot: %v", err)
	}
}
