package api

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// SetRecoveryClientForTest replaces the KyRecovery pairing client. Test-only: this file is not
// part of the package build.
func SetRecoveryClientForTest(s *Server, p recoveryClient) { s.recovery = p }

// SetStoreForTest replaces the handlers' store, so a test can break one write. Test-only.
func SetStoreForTest(s *Server, st store.Store) { s.store = st }

// AttemptsCapForTest is the limiter's hard bound on distinct keys.
const AttemptsCapForTest = attemptsCap

// AttemptKeysForTest returns the limiter's live keys. Test-only: this file is not part of the
// package build.
func AttemptKeysForTest(s *Server) []string {
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()
	keys := make([]string, 0, len(s.attempts)+len(s.accounts))
	for k := range s.attempts {
		keys = append(keys, k)
	}
	for k := range s.accounts {
		keys = append(keys, k)
	}
	return keys
}

// AccountKeysForTest returns only the account-keyed windows. Test-only.
func AccountKeysForTest(s *Server) []string {
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()
	return slices.Collect(maps.Keys(s.accounts))
}

// LoginDelayForTest reports username's backoff delay from now, and ends it early while
// keeping its failure count, so a test can watch the delay grow without waiting. Test-only.
func LoginDelayForTest(s *Server, username string) time.Duration {
	key := crypto.SHA256Hex([]byte(strings.ToLower(username)))
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()
	e := s.loginBackoff[key]
	delay := time.Until(e.until)
	e.until = time.Now().Add(-time.Second)
	s.loginBackoff[key] = e
	return delay
}

// AllowAttemptForTest drives the limiter directly so a test can fill it without paying for
// 10 000 HTTP requests. Test-only.
func AllowAttemptForTest(s *Server, key string, limit int, window time.Duration) bool {
	return s.allowAttempt(key, limit, window)
}

// RegisterDetachedForTest registers one detached handler and returns its unregister func, so a
// test can drive the counter without an HTTP request. Test-only.
func RegisterDetachedForTest(s *Server) func() {
	s.detached.add()
	return s.detached.done
}

// SetDigestResolverForTest replaces the registry resolver behind update checks. Test-only.
func SetDigestResolverForTest(s *Server, r store.DigestResolver) { s.digestResolver = r }

// SetAllowPrivilegedForTest sets KY_CONTAINER_ALLOW_PRIVILEGED. Test-only.
func SetAllowPrivilegedForTest(s *Server, on bool) { s.config.Container.AllowPrivileged = on }

// SetPlanInspectorForTest replaces the agent round trip of each plan-time inspection. Test-only.
func SetPlanInspectorForTest(s *Server, f func(context.Context, protocol.InspectionTarget) (protocol.ContainerInspection, error)) {
	s.planInspector = f
}

// PlanInspectionBudgetForTest is the plan's inspection budget.
const PlanInspectionBudgetForTest = planInspectionBudget

// RegistrySlotsHeldForTest counts the registry slots in use server-wide. Test-only.
func RegistrySlotsHeldForTest(s *Server) int { return len(s.registrySlots) }

// PolicyTickForTest runs one scheduler tick at now; the runs it starts are not waited for.
func PolicyTickForTest(s *Server, now time.Time) { s.policyTick(context.Background(), now) }

// WaitPolicyRunsForTest blocks until every run a tick started has finished.
func WaitPolicyRunsForTest(s *Server) { s.policies.runs.Wait() }

// SetPolicyClockForTest makes RunPolicies tick every interval and read the time from clock.
func SetPolicyClockForTest(s *Server, interval time.Duration, clock func() time.Time) {
	s.policies.interval, s.policies.now = interval, clock
}

// SetPolicyPanicHookForTest fires f once, the moment the next run's row is open, then clears
// itself. Test-only: exercises the scheduler's panic recover.
func SetPolicyPanicHookForTest(s *Server, f func()) { s.policies.panicHook = f }

// ValidationTickForTest runs one validation tick with the clock at now; it returns when the tick,
// rollback included, is done.
func ValidationTickForTest(s *Server, now time.Time) {
	s.validationTick(context.Background(), func() time.Time { return now })
}

// SetValidationClockForTest makes RunValidations tick every interval and read the time from clock.
func SetValidationClockForTest(s *Server, interval time.Duration, clock func() time.Time) {
	s.validations.interval, s.validations.now = interval, clock
}

// ErrInspectionInvalidForTest is what an inspection that failed validation returns.
var ErrInspectionInvalidForTest = errInspectionInvalid

// StillAllowedForTest runs a log stream's periodic re-check. Test-only.
func StillAllowedForTest(s *Server, r *http.Request, a store.TenantAccess, endpointID string) bool {
	return s.stillAllowed(r, a, endpointID)
}

var TrackedReferenceForTest = trackedReference
var RunningDigestForTest = runningDigest

// ResetAttemptsForTest clears the rate limiter so one test can make many checks. Test-only.
func ResetAttemptsForTest(s *Server) {
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()
	clear(s.attempts)
	clear(s.accounts)
	clear(s.loginBackoff)
}

// AllowAccountAttemptForTest drives the account-keyed limiter directly. Test-only.
func AllowAccountAttemptForTest(s *Server, key string, limit int, window time.Duration) bool {
	return s.allowAccountAttempt(key, limit, window)
}

// AppVersionForTest is the version the agent skill's frontmatter must carry.
const AppVersionForTest = appVersion

// MCPToolMessagesForTest are the tool errors the agent skill's error table must name.
var MCPToolMessagesForTest = []string{msgReadOnly, msgNotAnUpdate, msgDropsMounts, msgTooManyOperations}

// UpdateRefusalForTest is apply_update's verdict on plan d, for plans a fixture cannot reach.
func UpdateRefusalForTest(ctx context.Context, s *Server, a store.TenantAccess, d *store.Deployment) (string, error) {
	return mcpTools{s: s}.updateRefusal(ctx, a, d)
}

// SetInventoryFreshForTest sets the inventory age a workload update check accepts without asking
// the agent for a fresh report.
func SetInventoryFreshForTest(s *Server, d time.Duration) { s.inventoryFreshFor = d }
