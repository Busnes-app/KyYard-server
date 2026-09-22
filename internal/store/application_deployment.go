package store

import "strings"

// PreflightBlockedError names the findings that stopped a plan. A plan never guesses past them.
type PreflightBlockedError struct{ Blockers []string }

func (e *PreflightBlockedError) Error() string {
	return "deployment preflight blocked: " + strings.Join(e.Blockers, ",")
}
