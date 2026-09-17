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
	BudgetRefusals  int64
	ReadFailures    int64
	CrossTenantLeak int64
	DenialsRefused  int64
	DenialsLeaked   int64
	RollupRows      int64
	PeakUsage       int64
	FinalUsage      int64
	Budget          int64
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

	r := &report{Budget: s.budget, PressureSeen: map[store.Pressure]int{}}
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
					err := st.Tenancy().RecordSamples(runCtx, id, m)
					mu.Lock()
					switch {
					case runCtx.Err() != nil:
					case errors.Is(err, store.ErrSampleBudget):
						// Evidence the ceiling actually turned a write away, which a level
						// flag toggling on and off is not.
						r.BudgetRefusals++
					case err != nil:
						r.Failures = append(r.Failures, fmt.Sprintf("metrics refused for an unexpected reason: %v", err))
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
				// A target that exists nowhere: this asks whether refusals grow the audit
				// trail without bound, and may answer "not found" as fairly as "forbidden".
				fresh := st.Tenancy().RenameEndpoint(runCtx, fixture.readOnly, fmt.Sprintf("ep_absent_%d", n), "nope")
				// A real, approved endpoint belonging to the other organization. Only
				// forbidden will do: "not found" here would mean the boundary had become a
				// lookup, telling this member which identifiers exist.
				real := st.Tenancy().RenameEndpoint(runCtx, fixture.readOnly, fixture.endpoints[0], "nope")
				mu.Lock()
				switch {
				case runCtx.Err() != nil:
				case errors.Is(fresh, store.ErrForbidden), errors.Is(fresh, store.ErrNotFound):
					r.DenialsRefused++
				case fresh == nil:
					r.DenialsLeaked++
					r.Failures = append(r.Failures, "a read-only member renamed an endpoint")
				default:
					r.Failures = append(r.Failures, fmt.Sprintf("denial path failed oddly: %v", fresh))
				}
				switch {
				case runCtx.Err() != nil:
				case errors.Is(real, store.ErrForbidden):
					r.DenialsRefused++
				default:
					r.CrossTenantLeak++
					r.Failures = append(r.Failures, fmt.Sprintf("a read-only member of another organization reached a real endpoint: %v", real))
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
				if err == nil {
					window = 2 * time.Hour
					mu.Lock()
					r.RollupRows += n
					mu.Unlock()
				}
				for i := 0; i < 20; i++ {
					removed, err := st.Tenancy().Prune(runCtx)
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
					log.Printf("[SOAK] %d samples offered, %d refused for budget, %d denials refused, %d bytes used, telemetry %s", r.SamplesOffered, r.BudgetRefusals, r.DenialsRefused, used, st.Pressure())
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
