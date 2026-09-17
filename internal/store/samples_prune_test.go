package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
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

// Ingest cost follows the frame, not the stored set: at the row ceiling, a thousand frames
// naming a container that already has a fresh row cost one primary-key seek each.
func TestSamplesIngestCostIsBounded(t *testing.T) {
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
	if err := ts.RecordSamples(ctx, e, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c"}}}); err != nil {
		t.Fatal(err)
	}
	tx, _ := st.db.BeginTx(ctx, nil)
	stmt, err := tx.PrepareContext(ctx, st.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids) VALUES (?,?,?,0,0,0,0,0,0)`))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-3 * time.Hour)
	for i := 1; i < MaxSampleRowsPerEndpoint; i++ {
		if _, err := stmt.ExecContext(ctx, e, "fill", old.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	for i := 0; i < 1000; i++ {
		if err := ts.RecordSamples(ctx, e, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("1000 cadence-dropped frames at the ceiling took %s", took)
	}
	// A frame that must write still meets the ceiling, and is refused without a close.
	if err := ts.RecordSamples(ctx, e, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "new"}}}); !errors.Is(err, ErrSampleBudget) {
		t.Fatalf("expected ErrSampleBudget, got %v", err)
	}
}

// Under pressure the raw window closes as well as the door: refusing new writes alone never
// gives space back, and the point of the budget is to recover, not merely to stop growing.
func TestPressureShortensTheRawWindow(t *testing.T) {
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
	tx, _ := st.db.BeginTx(ctx, nil)
	stmt, err := tx.PrepareContext(ctx, st.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids) VALUES (?,?,?,0,0,0,0,0,0)`))
	if err != nil {
		t.Fatal(err)
	}
	// Two hours old: inside the six-hour window, outside the degraded one.
	old := time.Now().UTC().Add(-2 * time.Hour)
	for i := 0; i < 10; i++ {
		if _, err := stmt.ExecContext(ctx, e, fmt.Sprintf("c%d", i), old.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM container_samples WHERE endpoint_id=?`), e).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n, err := ts.Prune(ctx); err != nil || n != 0 || count() != 10 {
		t.Fatalf("normal pressure pruned inside the window: %d removed, %d left, %v", n, count(), err)
	}
	st.SetPressure(PressureDegraded)
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if left := count(); left != 0 {
		t.Fatalf("degraded pressure left %d rows outside the shortened window", left)
	}
}

// The budget is measured, not assumed: a real database reports real bytes.
func TestUsageReportsBytesAndBudget(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	used, err := st.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if used <= 0 {
		t.Fatalf("usage reported %d bytes for a database with a schema in it", used)
	}
	if st.Pressure() != PressureNormal {
		t.Fatalf("a fresh store started under pressure: %s", st.Pressure())
	}
}

// Pressure has to be a state the system can leave. If it could only ever rise, one busy day
// would refuse telemetry for every tenant until an operator intervened, with no way back.
func TestPressureClearsAfterPruning(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	// A budget the schema alone exceeds could never clear, and should not: the budget is set
	// just above what an empty database holds, so only the samples put it over.
	baseline, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := baseline.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.DiskBudget = empty + 64<<10
	opened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	st := opened.(*SQLStore)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.Users().CreateUser(ctx, &User{ID: "actor", Username: "actor", Role: "user", Status: "active", SSOProvider: "local"}))
	must(st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: InitialOrganizationID, UserID: "actor", Role: RoleOrganizationAdmin, Status: "active"}))
	must(st.Tenancy().CreateEnvironment(ctx, &Environment{ID: "environment", OrganizationID: InitialOrganizationID, Name: "Before"}))
	a := TenantAccess{ActorID: "actor", OrganizationID: InitialOrganizationID, EnvironmentID: "environment"}
	ts := st.Tenancy()
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	must(err)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	enrolled, err := ts.Enroll(ctx, EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "host"})
	must(err)

	tx, _ := st.db.BeginTx(ctx, nil)
	stmt, err := tx.PrepareContext(ctx, st.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids) VALUES (?,?,?,0,0,0,0,0,0)`))
	must(err)
	old := time.Now().UTC().Add(-8 * time.Hour) // past every retention window
	for i := 0; i < 2000; i++ {
		_, err := stmt.ExecContext(ctx, enrolled.ID, fmt.Sprintf("c%d", i%50), old.Add(time.Duration(i)*time.Second))
		must(err)
	}
	stmt.Close()
	must(tx.Commit())

	if p, err := st.EvaluatePressure(ctx, 0); err != nil || p != PressureStopped {
		t.Fatalf("a database over its budget read as %s (%v)", p, err)
	}
	// Prune until it has nothing left to take, feeding each pass back in as the loop does.
	for i := 0; i < 30; i++ {
		freed, err := ts.Prune(ctx)
		must(err)
		if st.Driver() == "postgres" && freed > 0 {
			// Stand in for the engine's own vacuum, which is what returns deleted pages.
			if _, err := st.db.ExecContext(ctx, "VACUUM container_samples"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.EvaluatePressure(ctx, freed); err != nil {
			t.Fatal(err)
		}
		if freed == 0 && st.Pressure() == PressureNormal {
			break
		}
	}
	var left int
	must(st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM container_samples`)).Scan(&left))
	if left != 0 {
		t.Fatalf("%d rows survived a prune of samples eight hours old", left)
	}
	if st.Pressure() != PressureNormal {
		t.Fatalf("pressure stuck at %s after everything it measured was deleted", st.Pressure())
	}
}

// Growth retention cannot reclaim must not hold telemetry down. Audit is never pruned and
// never refused, so if it alone could pin the level, dropping every metric on the fleet would
// never lift it again.
func TestPressureClearsWhenRetentionCannotReclaim(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	baseline, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := baseline.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.DiskBudget = empty + 32<<10
	opened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	st := opened.(*SQLStore)

	// Put the excess where Prune cannot go, and leave the samples table empty.
	for i := 0; i < 4000; i++ {
		if _, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO audit_records (user_id,action,resource,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`),
			"actor", "audit.fill", fmt.Sprintf("r%d", i), "127.0.0.1", time.Now().UTC(), "platform", "", "", fmt.Sprintf("c%d", i), "success"); err != nil {
			t.Fatal(err)
		}
	}
	used, err := st.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if used < cfg.DiskBudget {
		t.Skipf("audit rows did not exceed the budget (%d of %d); nothing to prove here", used, cfg.DiskBudget)
	}
	if p, _ := st.EvaluatePressure(ctx, 0); p != PressureStopped {
		t.Fatalf("a database over its budget read as %s", p)
	}
	// A pass with nothing to reclaim must not leave telemetry refused for good.
	if p, err := st.EvaluatePressure(ctx, 0); err != nil || p == PressureStopped {
		t.Fatalf("pressure stuck at %s though retention has nothing left to take (%v)", p, err)
	}
	var samples int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM container_samples`)).Scan(&samples); err != nil || samples != 0 {
		t.Fatalf("the test did not isolate unreclaimable growth: %d samples, %v", samples, err)
	}
}
