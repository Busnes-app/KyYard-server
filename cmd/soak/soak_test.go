package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/Busnes-app/kyyard-server/internal/store"
	_ "modernc.org/sqlite"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The 24-hour run is not something CI can hold, so a compressed one runs the same driver and
// the same checks here. It proves the harness still works and still asks real questions; it
// does not prove the day-long bounds, which is what the command exists for.
func TestSoakDriverHoldsItsBoundsInMiniature(t *testing.T) {
	if testing.Short() {
		t.Skip("soak driver runs for seconds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r, err := run(ctx, settings{
		dir:        t.TempDir(),
		endpoints:  3,
		containers: 5,
		duration:   8 * time.Second,
		cadence:    200 * time.Millisecond,
		retention:  500 * time.Millisecond,
		report:     time.Hour,
		budget:     2 << 30,
		quiet:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Failures) != 0 {
		t.Fatalf("the driver reported failures on a healthy run: %v\n%s", r.Failures, r.String())
	}
	// The run has to have actually done the things it claims to check.
	// Offered is not stored: the cadence rule drops most of what a fast run sends, and an
	// empty table satisfies every other bound here.
	if r.RowsStored == 0 {
		t.Fatal("no sample rows reached the database, so the storage bounds were never exercised")
	}
	if r.DenialsRefused == 0 {
		t.Fatal("no refusal was exercised, so the denial path proves nothing")
	}
	if r.Ticks == 0 {
		t.Fatal("the list screens were never read, so p95 means nothing")
	}
	if !strings.Contains(r.String(), "not covered here") {
		t.Fatal("the report must name what it does not cover")
	}
}

// A budget the schema alone exceeds puts the run under pressure at once; the harness must
// report that rather than pass quietly, because a soak that cannot tell is worth nothing.
func TestSoakReportsWhenTheBudgetCannotHold(t *testing.T) {
	if testing.Short() {
		t.Skip("soak driver runs for seconds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := run(ctx, settings{
		dir:        t.TempDir(),
		endpoints:  2,
		containers: 5,
		duration:   4 * time.Second,
		cadence:    200 * time.Millisecond,
		retention:  500 * time.Millisecond,
		report:     time.Hour,
		budget:     1, // anything stored is over budget
		quiet:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.PeakUsage < r.Budget {
		t.Fatalf("a one-byte budget was not exceeded: %d", r.PeakUsage)
	}
	if r.PressureSeen[0]+r.PressureSeen[1]+r.PressureSeen[2] == 0 {
		t.Fatal("pressure was never evaluated during the run")
	}
	// A budget nothing can satisfy must be reported as a failure, not passed over: a soak
	// that cannot tell whether the budget held is worth nothing.
	if len(r.Failures) == 0 {
		t.Fatalf("a run that ended over its budget reported no failure:\n%s", r.String())
	}
}

// A timestamp the harness cannot read must be reported, not skipped: skipping deletes the
// retention assertion and prints an age of zero, which reads exactly like a healthy run.
func TestAgeOfReportsWhatItCannotRead(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	hour := now.Add(-time.Hour)
	for _, tc := range []struct {
		name  string
		value any
		want  time.Duration
		bad   bool
	}{
		{name: "no rows", value: nil, want: 0},
		{name: "time", value: hour, want: time.Hour},
		{name: "driver text", value: "2026-09-17 11:00:00 +0000 UTC", want: time.Hour},
		{name: "rfc3339", value: "2026-09-17T11:00:00Z", want: time.Hour},
		{name: "zone-less", value: "2026-09-17 11:00:00", want: time.Hour},
		{name: "bytes", value: []byte("2026-09-17 11:00:00 +0000 UTC"), want: time.Hour},
		{name: "nonsense", value: "not a time at all", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ageOf(tc.value, now)
			if tc.bad {
				if err == nil {
					t.Fatalf("an unreadable timestamp passed silently as %s", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("age %s, want %s", got, tc.want)
			}
		})
	}
}

// A summary far past its window must be reported. This guards the copy-paste that had the
// summary check comparing the sample age, which made it unable to fire.
func TestCheckCatchesASummaryPastItsWindow(t *testing.T) {
	r := &report{Budget: 1 << 30, PressureSeen: map[store.Pressure]int{}}
	now := time.Now().UTC()
	r.OldestSample = within(r, "sample", now.Add(-time.Hour), now, store.SampleRetention, 2*time.Minute)
	if len(r.Failures) != 0 {
		t.Fatalf("a sample inside its window was reported: %v", r.Failures)
	}
	r.OldestRollup = within(r, "summary", now.Add(-8*24*time.Hour), now, store.RollupRetention, time.Hour)
	if len(r.Failures) != 1 || !strings.Contains(r.Failures[0], "summary") {
		t.Fatalf("a summary eight days old was not reported against the seven-day window: %v", r.Failures)
	}
}

// Retention failing every tick leaves nothing for the checks that read summaries to read, so
// silence there has to be a failure rather than an absence.
func TestSoakReportsWhenRetentionCannotRun(t *testing.T) {
	if testing.Short() {
		t.Skip("soak driver runs for seconds")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Take the table away once the schema exists, so every roll-up pass fails.
	broken := make(chan struct{})
	go func() {
		defer close(broken)
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			db, err := sql.Open("sqlite", filepath.Join(dir, "soak.db"))
			if err == nil {
				_, execErr := db.Exec(`DROP TABLE IF EXISTS container_rollups`)
				db.Close()
				if execErr == nil {
					var probe *sql.DB
					if probe, err = sql.Open("sqlite", filepath.Join(dir, "soak.db")); err == nil {
						defer probe.Close()
					}
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	r, err := run(ctx, settings{
		dir: dir, endpoints: 2, containers: 3, duration: 6 * time.Second,
		cadence: 300 * time.Millisecond, retention: 300 * time.Millisecond,
		report: time.Hour, budget: 2 << 30, quiet: true,
	})
	<-broken
	if err != nil {
		t.Fatal(err)
	}
	if r.RollupErrors == 0 {
		t.Skip("the table was not dropped in time; nothing to prove")
	}
	if len(r.Failures) == 0 {
		t.Fatalf("roll-up failed %d times and the run reported success:\n%s", r.RollupErrors, r.String())
	}
}

// A probe that could not run is neither a pass nor a leak. Under contention on one SQLite
// connection an odd error is an ordinary outcome, and calling it a leak would assert a
// boundary crossing that never happened.
func TestProbeClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want probeOutcome
	}{
		{name: "refused by role or scope", err: store.ErrForbidden, want: probeRefused},
		{name: "refused by scope as not found", err: store.ErrNotFound, want: probeRefused},
		{name: "wrapped refusal", err: fmt.Errorf("renaming: %w", store.ErrForbidden), want: probeRefused},
		{name: "it succeeded", err: nil, want: probeLeaked},
		{name: "deadline", err: context.DeadlineExceeded, want: probeUnproven},
		{name: "locked database", err: errors.New("database is locked"), want: probeUnproven},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.want {
				t.Fatalf("classified as %d, want %d", got, tc.want)
			}
		})
	}
}

// One line per condition, whatever its count: the verdict an operator reads after a day must
// not sit under thousands of identical lines.
func TestRepeatedFailuresAreSummarisedOnce(t *testing.T) {
	r := &report{Budget: 1 << 30, PressureSeen: map[store.Pressure]int{}, Examples: map[string]string{}}
	for i := 0; i < 500; i++ {
		r.RollupErrors++
		r.note("roll-up", fmt.Errorf("no such table: container_rollups"))
	}
	r.Failures = append(r.Failures, fmt.Sprintf("roll-up failed %d times, first: %s", r.RollupErrors, r.Examples["roll-up"]))
	out := r.String()
	if n := strings.Count(out, "roll-up failed"); n != 1 {
		t.Fatalf("500 failures printed %d lines:\n%s", n, out)
	}
	if !strings.Contains(out, "500 times") || !strings.Contains(out, "no such table") {
		t.Fatalf("the summary lost either the count or the example:\n%s", out)
	}
}
