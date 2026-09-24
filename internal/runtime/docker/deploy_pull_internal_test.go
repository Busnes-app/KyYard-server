package docker

import (
	"testing"
	"time"
)

// The pull window reserves every service's replacement and never goes below callBudget.
func TestPullWindowReservesEveryReplacement(t *testing.T) {
	for _, tc := range []struct {
		remaining time.Duration
		services  int
		window    time.Duration
		ok        bool
	}{
		{replaceBudget + callBudget, 1, callBudget, true},
		{replaceBudget + callBudget - time.Nanosecond, 1, callBudget - time.Nanosecond, false},
		{3*replaceBudget + time.Minute, 3, time.Minute, true},
		{3*replaceBudget + time.Minute, 4, time.Minute - replaceBudget, false},
	} {
		if window, ok := pullWindow(tc.remaining, tc.services); window != tc.window || ok != tc.ok {
			t.Fatalf("pullWindow(%s, %d) = %s %v, want %s %v", tc.remaining, tc.services, window, ok, tc.window, tc.ok)
		}
	}
}
