package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
)

// Retention is enforced from stored timestamps in bounded batches.
func TestPruneRemovesExpiredSamplesInBatches(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	enrolled, err := ts.Enroll(ctx, EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "host"})
	if err != nil {
		t.Fatal(err)
	}
	e := enrolled.ID
	old := time.Now().UTC().Add(-SampleRetention - time.Hour)
	for i := 0; i < 12; i++ {
		if _, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids) VALUES (?,?,?,?,?,?,?,?,?)`), e, "c", old.Add(time.Duration(i)*time.Second), 1.0, 1, 1, 1, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := ts.RecordSamples(ctx, e, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c", CPUPercent: 1}}}); err != nil {
		t.Fatal(err)
	}
	n, err := ts.Prune(ctx)
	if err != nil || n != 12 {
		t.Fatalf("prune: %d %v", n, err)
	}
	var left int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM container_samples WHERE endpoint_id=?`), e).Scan(&left); err != nil || left != 1 {
		t.Fatalf("rows after prune: %d %v", left, err)
	}
}
