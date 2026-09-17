package main

import (
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// The wide catch-up pass happens once, after a restart. If a failed pass narrowed the window
// anyway, the hours between two and six hours old would be deleted by retention without ever
// being summarised, and nothing would go back for them.
func TestAFailedRollUpKeepsTheWideWindow(t *testing.T) {
	window := store.SampleRetention
	// The deadline this budget exists for: a restart into a loaded database.
	window = nextRollUpWindow(window, errors.New("context deadline exceeded"))
	if window != store.SampleRetention {
		t.Fatalf("a failed pass narrowed the window to %s, losing the hours it never covered", window)
	}
	window = nextRollUpWindow(window, nil)
	if window != recentHours {
		t.Fatalf("a successful pass did not narrow the window: %s", window)
	}
	window = nextRollUpWindow(window, errors.New("later failure"))
	if window != recentHours {
		t.Fatalf("a later failure widened the window again: %s", window)
	}
}

// The budget has to leave room for the loop to run every minute.
func TestRollUpBudgetFitsTheLoop(t *testing.T) {
	if rollUpBudget >= time.Minute {
		t.Fatalf("a roll-up may run for %s, which is the whole interval between passes", rollUpBudget)
	}
}
