package main

import (
	"context"
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
