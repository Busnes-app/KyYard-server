package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
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

// Whatever an agent sends, a container gains at most one row per cadence and an endpoint
// never exceeds its row ceiling; a frame that would breach it is refused.
func TestSamplesBoundedPerContainer(t *testing.T) {
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
	base := time.Now().UTC().Add(-4 * time.Minute) // inside the skew window
	for i := 0; i < 500; i++ {
		if err := ts.RecordSamples(ctx, e, protocol.Metrics{ObservedAt: base.Add(time.Duration(i) * time.Second), Samples: []protocol.Sample{{ContainerID: "c", CPUPercent: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM container_samples WHERE endpoint_id=? AND container_id='c'`), e).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count > 500/50+1 {
		t.Fatalf("cadence cap not enforced: %d rows for 500 one-second samples", count)
	}
	many := make([]protocol.Sample, 1000)
	for i := range many {
		many[i] = protocol.Sample{ContainerID: fmt.Sprintf("id-%04d", i)}
	}
	if err := ts.RecordSamples(ctx, e, protocol.Metrics{ObservedAt: time.Now(), Samples: many}); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM container_samples WHERE endpoint_id=?`), e).Scan(&count); err != nil {
		t.Fatal(err)
	}
	// Fill to the ceiling directly, then one more frame is refused and nothing of it lands.
	tx, _ := st.db.BeginTx(ctx, nil)
	stmt, err := tx.PrepareContext(ctx, st.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids) VALUES (?,?,?,0,0,0,0,0,0)`))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-3 * time.Hour)
	for i := count; i < MaxSampleRowsPerEndpoint; i++ {
		if _, err := stmt.ExecContext(ctx, e, "fill", old.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	err = ts.RecordSamples(ctx, e, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "over"}}})
	if !errors.Is(err, ErrSampleBudget) {
		t.Fatalf("frame past the ceiling accepted: %v", err)
	}
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM container_samples WHERE endpoint_id=?`), e).Scan(&count); err != nil || count != MaxSampleRowsPerEndpoint {
		t.Fatalf("rows after refused frame: %d %v", count, err)
	}
}
