package docker

import (
	"testing"
	"time"
)

// The pull phase is the smaller of 80% of what remains and what remains less one replacement,
// whatever the service count: a 5-service frame with the full 600 s deadline pulls.
func TestPullPhase(t *testing.T) {
	for _, tc := range []struct {
		remaining, phase time.Duration
	}{
		{15 * time.Minute, 720 * time.Second},  // 80% binds
		{700 * time.Second, 560 * time.Second}, // both rules agree
		{500 * time.Second, 360 * time.Second}, // replaceBudget binds
		{replaceBudget + callBudget, callBudget},
		{replaceBudget, 0},
	} {
		if got := pullPhase(tc.remaining); got != tc.phase {
			t.Fatalf("pullPhase(%s) = %s, want %s", tc.remaining, got, tc.phase)
		}
	}
}
