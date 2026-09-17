// Command soak drives the storage path at the capacity targets in docs/retention-policy.md and
// checks that the bounds it promises actually hold: rows stay under the per-endpoint ceiling,
// retention keeps up with the writers, the disk budget engages and can be left again, refused
// mutations are recorded, and the list screens stay fast while all of that runs.
//
// It exercises the store rather than the agent socket. The question the M4 gate asks is about
// database and storage bounds, and a websocket per endpoint would measure the transport
// instead. What it cannot yet cover is named in the report rather than assumed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

type settings struct {
	dir        string
	endpoints  int
	containers int
	duration   time.Duration
	cadence    time.Duration
	retention  time.Duration
	report     time.Duration
	budget     int64
	quiet      bool
}

// report is what the run proves, or fails to.
type report struct {
	Ticks           int
	SamplesOffered  int64
	RowsStored      int64
	CeilingRefusals int64
	PressureDrops   int64
	RollupErrors    int64
	PruneErrors     int64
	ReadFailures    int64
	RoleDenials     int64
	CrossTenantLeak int64
	ProbeErrors     int64
	WriteErrors     int64
	// One representative message per condition. A day-long run of a failing tick would
	// otherwise print thousands of identical lines and bury the verdict underneath them.
	Examples        map[string]string
	DenialsRefused  int64
	DenialsLeaked   int64
	RollupRows      int64
	PeakUsage       int64
	FinalUsage      int64
	Budget          int64
	Duration        time.Duration
	PressureSeen    map[store.Pressure]int
	MaxRowsEndpoint int64
	Ceiling         int
	OldestSample    time.Duration
	OldestRollup    time.Duration
	ReadP95         time.Duration
	Failures        []string
}

func main() {
	var s settings
	flag.StringVar(&s.dir, "dir", "", "data directory (default: a temporary one)")
	flag.IntVar(&s.endpoints, "endpoints", 25, "endpoints per instance")
	flag.IntVar(&s.containers, "containers", 100, "containers per endpoint")
	flag.DurationVar(&s.duration, "duration", 24*time.Hour, "how long to run")
	flag.DurationVar(&s.cadence, "cadence", time.Minute, "how often each endpoint reports")
	flag.DurationVar(&s.retention, "retention-interval", time.Minute, "how often retention runs")
	flag.DurationVar(&s.report, "report", 5*time.Minute, "how often to print progress")
	flag.Int64Var(&s.budget, "budget", 2<<30, "retention disk budget in bytes")
	flag.BoolVar(&s.quiet, "quiet", false, "only print the final report")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, err := run(ctx, s)
	if err != nil {
		log.Fatalf("soak: %v", err)
	}
	fmt.Print(r.String())
	if len(r.Failures) > 0 {
		os.Exit(1)
	}
}

func run(ctx context.Context, s settings) (*report, error) {
	if s.dir == "" {
		dir, err := os.MkdirTemp("", "kyyard-soak-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		s.dir = dir
	}
	cfg := config.DatabaseConfig{
		Driver:     "sqlite",
		DSN:        filepath.Join(s.dir, "soak.db"),
		DataDir:    s.dir,
		DiskBudget: s.budget,
	}
	st, err := store.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	fixture, err := seed(ctx, st, s, cfg.DSN)
	if err != nil {
		return nil, err
	}

	r := &report{Budget: s.budget, Duration: s.duration, PressureSeen: map[store.Pressure]int{}, Examples: map[string]string{}}
	var mu sync.Mutex
	var reads []time.Duration
	runCtx, cancel := context.WithTimeout(ctx, s.duration)
	defer cancel()
	var wg sync.WaitGroup

	// The writers: every endpoint reports its containers at the cadence.
	for _, e := range fixture.endpoints {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			t := time.NewTicker(s.cadence)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					m := protocol.Metrics{ObservedAt: time.Now().UTC(), Samples: make([]protocol.Sample, s.containers)}
					for i := range m.Samples {
						m.Samples[i] = protocol.Sample{ContainerID: fmt.Sprintf("%s-c%03d", id, i), CPUPercent: float64(i % 100), MemoryBytes: int64(i) << 20, RestartCount: int64(i % 3)}
					}
					// Shed by pressure before writing, as internal/api does for a real agent.
					// Without this the soak would drive a path no deployment uses and could
					// not say whether the disk budget sheds anything at all.
					if st.Pressure() != store.PressureNormal {
						mu.Lock()
						r.PressureDrops++
						mu.Unlock()
						continue
					}
					err := st.Tenancy().RecordSamples(runCtx, id, m)
					mu.Lock()
					switch {
					case runCtx.Err() != nil:
					case errors.Is(err, store.ErrSampleBudget):
						// The per-endpoint row ceiling, which is not the disk budget: nothing
						// in RecordSamples consults that.
						r.CeilingRefusals++
					case err != nil:
						r.WriteErrors++
						r.note("write", err)
					default:
						// Offered, not stored: the cadence rule drops most of these. What
						// landed is counted from the database in check.
						r.SamplesOffered += int64(len(m.Samples))
					}
					mu.Unlock()
				}
			}
		}(e)
	}

	// A read-only member of another organization, refused for the whole run against target
	// identifiers it has never used before, while the writers above keep going.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(s.cadence)
		defer t.Stop()
		for n := 0; ; n++ {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				// Audit growth: a target that exists nowhere, refused for the whole run.
				fresh := st.Tenancy().RenameEndpoint(runCtx, fixture.readOnly, fmt.Sprintf("ep_absent_%d", n), "nope")
				// The role gate: a read-only member is refused before the operation runs at
				// all, so this says nothing about tenancy and is not counted as if it did.
				role := st.Tenancy().RenameEndpoint(runCtx, fixture.readOnly, fixture.endpoints[0], "nope")
				// The organization boundary itself: an administrator of the other tenant, who
				// passes every role check, so only the scoping predicate in the statement can
				// refuse. This is the probe that notices if that predicate is ever dropped.
				boundary := st.Tenancy().RenameEndpoint(runCtx, fixture.readAdmin, fixture.endpoints[0], "nope")
				if runCtx.Err() != nil {
					continue
				}
				mu.Lock()
				for _, p := range []struct {
					name string
					err  error
					leak *int64
				}{
					{"absent-target", fresh, &r.DenialsLeaked},
					{"role gate", role, &r.DenialsLeaked},
					{"organization boundary", boundary, &r.CrossTenantLeak},
				} {
					switch classify(p.err) {
					case probeRefused:
						if p.name == "role gate" {
							r.RoleDenials++
						} else {
							r.DenialsRefused++
						}
					case probeLeaked:
						*p.leak++
					default:
						// The probe could not be evaluated. Under contention on one SQLite
						// connection that is an ordinary outcome, and calling it a leak would
						// assert a boundary crossing that never happened.
						r.ProbeErrors++
						r.note("probe:"+p.name, p.err)
					}
				}
				mu.Unlock()
			}
		}
	}()

	// Retention, as cmd/server runs it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(s.retention)
		defer t.Stop()
		window := store.SampleRetention
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				rollCtx, cancelRoll := context.WithTimeout(runCtx, 30*time.Second)
				n, err := st.Tenancy().RollUp(rollCtx, time.Now().UTC().Add(-window))
				cancelRoll()
				mu.Lock()
				switch {
				case runCtx.Err() != nil:
				case err != nil:
					// A roll-up that fails every tick leaves no summaries at all, and a check
					// that only looks at the summaries it finds would call that healthy.
					r.RollupErrors++
					r.note("roll-up", err)
				default:
					window = 2 * time.Hour
					r.RollupRows += n
				}
				mu.Unlock()
				for i := 0; i < 20; i++ {
					removed, err := st.Tenancy().Prune(runCtx)
					if err != nil && runCtx.Err() == nil {
						mu.Lock()
						r.PruneErrors++
						r.note("prune", err)
						mu.Unlock()
						break
					}
					if err != nil || removed == 0 {
						break
					}
				}
				level, err := st.(*store.SQLStore).EvaluatePressure(runCtx)
				if err != nil {
					continue
				}
				used, _ := st.Usage(runCtx)
				mu.Lock()
				r.PressureSeen[level]++
				if used > r.PeakUsage {
					r.PeakUsage = used
				}
				mu.Unlock()
			}
		}
	}()

	// The list screens an operator actually waits on, timed while everything else runs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				started := time.Now()
				_, listErr := st.Tenancy().ListEndpoints(runCtx, fixture.admin, 0, 50)
				_, sampleErr := st.Tenancy().LatestSamples(runCtx, fixture.admin, fixture.endpoints[0])
				elapsed := time.Since(started)
				if runCtx.Err() != nil {
					continue
				}
				mu.Lock()
				// Timed whether it worked or not: a read that fails under load is exactly what
				// the latency gate is for, and dropping it would flatter the result twice.
				reads = append(reads, elapsed)
				r.Ticks++
				if listErr != nil || sampleErr != nil {
					r.ReadFailures++
				}
				mu.Unlock()
			}
		}
	}()

	if !s.quiet {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(s.report)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					used, _ := st.Usage(runCtx)
					mu.Lock()
					log.Printf("[SOAK] %d samples offered, %d shed under pressure, %d denials refused, %d bytes used, telemetry %s", r.SamplesOffered, r.PressureDrops, r.DenialsRefused, used, st.Pressure())
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()
	mu.Lock()
	sort.Slice(reads, func(i, j int) bool { return reads[i] < reads[j] })
	if len(reads) > 0 {
		r.ReadP95 = reads[(len(reads)*95)/100]
	}
	mu.Unlock()
	if err := check(context.WithoutCancel(ctx), st, fixture, r); err != nil {
		return r, err
	}
	return r, nil
}

// probeOutcome is what a refusal probe proved, if anything.
type probeOutcome int

const (
	// probeRefused: the operation was turned away, which is the expected result.
	probeRefused probeOutcome = iota
	// probeLeaked: it succeeded, so the boundary it tests has moved.
	probeLeaked
	// probeUnproven: it failed for some other reason, so the probe says nothing either way.
	// Treating this as a leak would assert a crossing that never happened.
	probeUnproven
)

func classify(err error) probeOutcome {
	switch {
	case err == nil:
		return probeLeaked
	case errors.Is(err, store.ErrForbidden), errors.Is(err, store.ErrNotFound):
		return probeRefused
	default:
		return probeUnproven
	}
}

// note keeps the first message seen for a condition. The count says how often; one example
// says what it looked like.
func (r *report) note(condition string, err error) {
	if _, seen := r.Examples[condition]; !seen {
		r.Examples[condition] = err.Error()
	}
}
