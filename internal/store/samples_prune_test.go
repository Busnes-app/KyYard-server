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

	if p, err := st.EvaluatePressure(ctx); err != nil || p != PressureStopped {
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
		if _, err := st.EvaluatePressure(ctx); err != nil {
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
	if p, _ := st.EvaluatePressure(ctx); p != PressureStopped {
		t.Fatalf("a database over its budget read as %s", p)
	}
	// A pass with nothing to reclaim must not leave telemetry refused for good.
	if p, err := st.EvaluatePressure(ctx); err != nil || p == PressureStopped {
		t.Fatalf("pressure stuck at %s though retention has nothing left to take (%v)", p, err)
	}
	var samples int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM container_samples`)).Scan(&samples); err != nil || samples != 0 {
		t.Fatalf("the test did not isolate unreclaimable growth: %d samples, %v", samples, err)
	}
}

// An hour's summary must say what the hour held, keep "no data" distinct from zero, and be
// safe to compute twice: the loop runs it every minute over hours that still have raw rows.
func TestRollUpSummarisesEndedHours(t *testing.T) {
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
	hour := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	tx, _ := st.db.BeginTx(ctx, nil)
	stmt, err := tx.PrepareContext(ctx, st.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids,restart_count) VALUES (?,?,?,?,?,?,?,?,?,?)`))
	if err != nil {
		t.Fatal(err)
	}
	// Two answered samples and one the runtime would not answer for.
	rows := []struct {
		at            time.Time
		cpu           float64
		mem, rx, pids int64
		restarts      int64
	}{
		{hour.Add(1 * time.Minute), 10, 100, 5, 3, 2},
		{hour.Add(2 * time.Minute), 30, 300, 9, 7, 4},
		{hour.Add(3 * time.Minute), -1, 200, 9, 1, -1},
	}
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, e, "c1", r.at, r.cpu, r.mem, 0, r.rx, 0, r.pids, r.restarts); err != nil {
			t.Fatal(err)
		}
	}
	// A container whose every sample is unanswered must summarise as unknown, never as zero.
	if _, err := stmt.ExecContext(ctx, e, "quiet", hour.Add(4*time.Minute), -1.0, 0, 0, 0, 0, 0, -1); err != nil {
		t.Fatal(err)
	}
	// The hour still running is not summarised: it is incomplete by definition.
	if _, err := stmt.ExecContext(ctx, e, "c1", time.Now().UTC(), 99.0, 1, 0, 0, 0, 0, 9); err != nil {
		t.Fatal(err)
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := ts.RollUp(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.RollUp(ctx, time.Time{}); err != nil { // idempotent: the loop runs it every minute
		t.Fatal(err)
	}
	got, err := ts.ReadRollups(ctx, a, e, "c1", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one ended hour, got %d: %+v", len(got), got)
	}
	h := got[0]
	if h.Samples != 3 {
		t.Fatalf("samples counted: %d", h.Samples)
	}
	if h.CPUAverage < 19.9 || h.CPUAverage > 20.1 || h.CPUPeak != 30 {
		t.Fatalf("unanswered cpu was averaged in: avg %v peak %v", h.CPUAverage, h.CPUPeak)
	}
	if h.MemoryAvg != 200 || h.MemoryPeak != 300 || h.RxBytes != 9 || h.PidsPeak != 7 || h.RestartCount != 4 {
		t.Fatalf("hour summarised wrong: %+v", h)
	}
	quiet, err := ts.ReadRollups(ctx, a, e, "quiet", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(quiet) != 1 || quiet[0].CPUAverage != -1 || quiet[0].CPUPeak != -1 || quiet[0].RestartCount != -1 {
		t.Fatalf("an hour of unanswered samples reported a number: %+v", quiet)
	}

	// Summaries outlive the samples they came from, and go at their own retention.
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE container_rollups SET hour=? WHERE container_id='quiet'`), time.Now().UTC().Add(-RollupRetention-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if left, err := ts.ReadRollups(ctx, a, e, "quiet", 0); err != nil || len(left) != 0 {
		t.Fatalf("a summary past its retention survived: %+v %v", left, err)
	}
	if kept, err := ts.ReadRollups(ctx, a, e, "c1", 0); err != nil || len(kept) != 1 {
		t.Fatalf("a summary inside its retention was pruned: %+v %v", kept, err)
	}
}

// Pruning erodes the oldest in-window hour from below while the loop still summarises it every
// minute. A recompute from the survivors must not replace the summary, or what outlives the
// raw rows describes the last minute of the hour rather than the hour.
func TestErodedHoursDoNotOverwriteTheirSummary(t *testing.T) {
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

	// An hour comfortably inside the raw window, so what erodes it is this test rather than
	// where the clock happens to sit: a skip here would prove nothing about the guard.
	hour := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	tx, _ := st.db.BeginTx(ctx, nil)
	stmt, err := tx.PrepareContext(ctx, st.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids,restart_count) VALUES (?,?,?,?,?,0,0,0,0,-1)`))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		// Descending, so the hour's peak sits at minute 0: inside the part erosion takes. A
		// summary rebuilt from the survivors would report their smaller peak instead.
		cpu := float64(59 - i)
		if _, err := stmt.ExecContext(ctx, e, "c1", hour.Add(time.Duration(i)*time.Minute), cpu, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := ts.RollUp(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	before, err := ts.ReadRollups(ctx, a, e, "c1", 0)
	if err != nil || len(before) != 1 {
		t.Fatalf("first summary: %+v %v", before, err)
	}
	if before[0].Samples != 60 || before[0].CPUPeak != 59 {
		t.Fatalf("the setup did not put the peak where erosion will take it: %+v", before[0])
	}

	// Erode the hour from below exactly as retention does at the six-hour boundary, without
	// depending on where wall-clock time sits inside the current hour.
	if _, err := st.db.ExecContext(ctx, st.rebind(`DELETE FROM container_samples WHERE endpoint_id=? AND observed_at<?`), e, hour.Add(45*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM container_samples WHERE endpoint_id=?`), e).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left == 0 || left >= 60 {
		t.Fatalf("the hour was not partially eroded: %d of 60 rows left", left)
	}
	if _, err := ts.RollUp(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	after, err := ts.ReadRollups(ctx, a, e, "c1", 0)
	if err != nil || len(after) != 1 {
		t.Fatalf("second summary: %+v %v", after, err)
	}
	if after[0].Samples != before[0].Samples || after[0].CPUPeak != before[0].CPUPeak {
		t.Fatalf("an eroded hour overwrote its summary: %d samples peak %v became %d samples peak %v",
			before[0].Samples, before[0].CPUPeak, after[0].Samples, after[0].CPUPeak)
	}

	// Nothing may widen an eroded hour, because a recompute from survivors cannot reconstruct
	// it. Growth is the case the guard must not block, so it is checked on an hour retention
	// has not touched.
	fresh := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	if _, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids,restart_count) VALUES (?,?,?,?,0,0,0,0,0,-1)`),
		e, "c2", fresh.Add(time.Minute), 10.0); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.RollUp(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids,restart_count) VALUES (?,?,?,?,0,0,0,0,0,-1)`),
		e, "c2", fresh.Add(2*time.Minute), 500.0); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.RollUp(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	late, err := ts.ReadRollups(ctx, a, e, "c2", 0)
	if err != nil || len(late) != 1 {
		t.Fatal(err)
	}
	if late[0].Samples != 2 || late[0].CPUPeak != 500 {
		t.Fatalf("a late sample did not widen an untouched hour: %+v", late[0])
	}
}

// Summaries must give space back under pressure like the raw window does, or they become the
// one telemetry retention cannot reclaim.
func TestPressureShortensTheRollupWindow(t *testing.T) {
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
	// Two days old: inside the week, outside the day the degraded window keeps.
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	if _, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO container_rollups (endpoint_id,container_id,hour,samples,cpu_avg,cpu_max,memory_avg,memory_max,rx_bytes,tx_bytes,pids_max,restart_count) VALUES (?,?,?,1,1,1,1,1,0,0,0,-1)`), e, "c1", old); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if kept, err := ts.ReadRollups(ctx, a, e, "c1", 0); err != nil || len(kept) != 1 {
		t.Fatalf("a summary inside the week was pruned at normal pressure: %+v %v", kept, err)
	}
	st.SetPressure(PressureDegraded)
	if _, err := ts.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if left, err := ts.ReadRollups(ctx, a, e, "c1", 0); err != nil || len(left) != 0 {
		t.Fatalf("pressure could not reclaim summary rows: %+v %v", left, err)
	}
}
