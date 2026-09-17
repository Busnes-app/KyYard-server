package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
	_ "modernc.org/sqlite"
)

type fixture struct {
	admin     store.TenantAccess
	readAdmin store.TenantAccess
	readOnly  store.TenantAccess
	endpoints []string
	// inspect is the fixture's own handle. Counting rows is the harness asking a question
	// about the database, not a thing the product should grow an API for.
	inspect *sql.DB
}

// seed builds the instance the run drives: one organization holding every endpoint, and a
// second whose only member may read. The refused mutations come from that member, so denial
// traffic and telemetry are never the same tenant's work.
func seed(ctx context.Context, st store.Store, s settings, dsn string) (*fixture, error) {
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "writers", Name: "Writers"}); err != nil {
		return nil, err
	}
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "readers", Name: "Readers"}); err != nil {
		return nil, err
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env", OrganizationID: "writers", Name: "Prod"}); err != nil {
		return nil, err
	}
	members := []struct {
		id, org string
		role    store.TenantRole
	}{
		{"usr_admin", "writers", store.RoleOrganizationAdmin},
		{"usr_readadmin", "readers", store.RoleOrganizationAdmin},
		{"usr_viewer", "readers", store.RoleReadOnly},
	}
	for _, m := range members {
		if err := st.Users().CreateUser(ctx, &store.User{ID: m.id, Username: m.id, Role: "user", Status: "active", SSOProvider: "local"}); err != nil {
			return nil, err
		}
		if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: m.org, UserID: m.id, Role: m.role, Status: "active"}); err != nil {
			return nil, err
		}
	}
	inspect, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	f := &fixture{
		admin:     store.TenantAccess{ActorID: "usr_admin", OrganizationID: "writers"},
		readAdmin: store.TenantAccess{ActorID: "usr_readadmin", OrganizationID: "readers"},
		readOnly:  store.TenantAccess{ActorID: "usr_viewer", OrganizationID: "readers"},
		inspect:   inspect,
	}
	enrol := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "writers", EnvironmentID: "env"}
	for i := 0; i < s.endpoints; i++ {
		tok, err := ts.CreateEnrollmentToken(ctx, enrol, "docker", "")
		if err != nil {
			return nil, err
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		e, err := ts.Enroll(ctx, store.EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: fmt.Sprintf("host-%03d", i)})
		if err != nil {
			return nil, err
		}
		if err := ts.ApproveEndpoint(ctx, f.admin, e.ID, e.Fingerprint); err != nil {
			return nil, err
		}
		f.endpoints = append(f.endpoints, e.ID)
	}
	return f, nil
}

// check asks the questions the retention policy answers on paper. Anything it cannot ask is
// named in the report rather than passed over.
func check(ctx context.Context, st store.Store, f *fixture, r *report) error {
	ts := st.Tenancy()

	for _, id := range f.endpoints {
		var rows int64
		if err := f.inspect.QueryRowContext(ctx, `SELECT COUNT(*) FROM container_samples WHERE endpoint_id=?`, id).Scan(&rows); err != nil {
			return err
		}
		r.RowsStored += rows
		if rows > r.MaxRowsEndpoint {
			r.MaxRowsEndpoint = rows
		}
	}
	if r.RowsStored == 0 {
		// Every other storage bound is satisfied by an empty table: no rows past the ceiling,
		// no sample too old to have been pruned. A run that stored nothing proves nothing.
		r.Failures = append(r.Failures, "not one sample row was stored, so no storage bound was exercised")
	}
	r.Ceiling = st.SampleCeiling()
	if r.MaxRowsEndpoint > int64(st.SampleCeiling()) {
		r.Failures = append(r.Failures, fmt.Sprintf("an endpoint holds %d sample rows, past the %d ceiling", r.MaxRowsEndpoint, st.SampleCeiling()))
	}

	var oldestSample, oldestRollup any
	if err := f.inspect.QueryRowContext(ctx, `SELECT MIN(observed_at) FROM container_samples`).Scan(&oldestSample); err != nil {
		return err
	}
	if err := f.inspect.QueryRowContext(ctx, `SELECT MIN(hour) FROM container_rollups`).Scan(&oldestRollup); err != nil {
		return err
	}
	now := time.Now().UTC()
	// Both windows go through one helper: checking the sample age against the summary window
	// is the kind of copy-paste that leaves an assertion unable to fire.
	r.OldestSample = within(r, "sample", oldestSample, now, store.SampleRetention, 2*time.Minute)
	r.OldestRollup = within(r, "summary", oldestRollup, now, store.RollupRetention, time.Hour)

	// The level returning to normal proves nothing on its own: by design any prune pass
	// clears it, so the flag toggles whether or not the database came back under budget.
	used, err := st.Usage(ctx)
	if err != nil {
		return err
	}
	r.FinalUsage = used
	if used > r.Budget {
		r.Failures = append(r.Failures, fmt.Sprintf("the run ended %d bytes over its %d budget", used-r.Budget, r.Budget))
	}
	// Nothing in RecordSamples consults the disk budget; shedding lives at the transport, and
	// the writers here shed the same way. So the question is whether pressure was raised and
	// whether anything was actually dropped for it.
	if r.PeakUsage >= r.Budget {
		if r.PressureSeen[store.PressureDegraded]+r.PressureSeen[store.PressureStopped] == 0 {
			r.Failures = append(r.Failures, "usage passed the budget and pressure was never raised")
		}
		if r.PressureDrops == 0 {
			r.Failures = append(r.Failures, "pressure was raised and not one write was shed for it")
		}
	}
	// One line per condition, whatever its count: a day-long run of a failing tick would
	// otherwise print thousands of identical lines and bury the verdict under them.
	if r.RollupErrors > 0 {
		r.Failures = append(r.Failures, fmt.Sprintf("roll-up failed %d times, first: %s", r.RollupErrors, r.Examples["roll-up"]))
	}
	if r.PruneErrors > 0 {
		r.Failures = append(r.Failures, fmt.Sprintf("prune failed %d times, first: %s", r.PruneErrors, r.Examples["prune"]))
	}
	if r.WriteErrors > 0 {
		r.Failures = append(r.Failures, fmt.Sprintf("%d writes failed unexpectedly, first: %s", r.WriteErrors, r.Examples["write"]))
	}
	if r.ProbeErrors > 0 {
		// Unproven is neither pass nor leak, and a run that could not evaluate its own
		// tenancy probes has not answered the question it exists to ask.
		for condition, example := range r.Examples {
			if strings.HasPrefix(condition, "probe:") {
				r.Failures = append(r.Failures, fmt.Sprintf("a %s probe could not be evaluated, first: %s", strings.TrimPrefix(condition, "probe:"), example))
			}
		}
	}
	if r.DenialsLeaked > 0 {
		r.Failures = append(r.Failures, fmt.Sprintf("%d refusals leaked: a member renamed an endpoint they may not touch", r.DenialsLeaked))
	}
	// A run long enough to end an hour must have summarised one, or the roll-up half of the
	// policy went untested while every check that reads summaries found none to read.
	if r.Duration > store.SampleRetention && r.RollupRows == 0 {
		r.Failures = append(r.Failures, "a run spanning the raw window produced no hourly summary")
	}
	if r.ReadFailures > 0 {
		r.Failures = append(r.Failures, fmt.Sprintf("%d of %d list reads failed under load", r.ReadFailures, r.Ticks))
	}
	if r.CrossTenantLeak > 0 {
		r.Failures = append(r.Failures, fmt.Sprintf("%d cross-tenant mutations were not refused", r.CrossTenantLeak))
	}
	if r.DenialsRefused == 0 {
		r.Failures = append(r.Failures, "no refusal was exercised, so the denial path proves nothing")
	}

	if r.ReadP95 > 500*time.Millisecond {
		r.Failures = append(r.Failures, fmt.Sprintf("p95 on the list screens was %s, past the 500ms target", r.ReadP95.Round(time.Millisecond)))
	}

	// The member refused for the whole run must still be removable, or a tenant could be
	// locked out of tidying up by the very traffic the refusals were protecting it from.
	if err := ts.RemoveMembership(ctx, f.readAdmin, "usr_viewer"); err != nil {
		r.Failures = append(r.Failures, fmt.Sprintf("an administrator could not remove the refused member: %v", err))
	}
	return nil
}

// ageOf reads a timestamp the store wrote. An empty table is nothing to check and is not an
// error; a value that cannot be read is, because silently skipping it would delete the
// assertion and print an age of zero, which reads exactly like a healthy run.
func ageOf(v any, now time.Time) (time.Duration, error) {
	if v == nil {
		return 0, nil
	}
	parsed, err := store.ParseStoredTime(v)
	if err != nil {
		return 0, err
	}
	return now.Sub(parsed), nil
}

func (r *report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nsoak report\n")
	fmt.Fprintf(&b, "  rows stored          %d (from %d samples offered)\n", r.RowsStored, r.SamplesOffered)
	fmt.Fprintf(&b, "  writes refused       %d at the row ceiling, %d shed under pressure\n", r.CeilingRefusals, r.PressureDrops)
	fmt.Fprintf(&b, "  summary rows written %d\n", r.RollupRows)
	fmt.Fprintf(&b, "  refusals exercised   %d tenancy, %d role (leaked %d, cross-tenant %d)\n", r.DenialsRefused, r.RoleDenials, r.DenialsLeaked, r.CrossTenantLeak)
	fmt.Fprintf(&b, "  rows, worst endpoint %d of %d allowed\n", r.MaxRowsEndpoint, r.Ceiling)
	fmt.Fprintf(&b, "  oldest sample        %s\n", r.OldestSample.Round(time.Second))
	fmt.Fprintf(&b, "  oldest summary       %s\n", r.OldestRollup.Round(time.Second))
	fmt.Fprintf(&b, "  usage                %d peak, %d final, of %d bytes\n", r.PeakUsage, r.FinalUsage, r.Budget)
	fmt.Fprintf(&b, "  p95 list read        %s over %d reads (%d failed)\n", r.ReadP95.Round(time.Millisecond), r.Ticks, r.ReadFailures)
	fmt.Fprintf(&b, "  probes unevaluated   %d\n", r.ProbeErrors)
	fmt.Fprintf(&b, "  not covered here     log-client memory (M5), the per-actor denial budget and per-organization audit ceiling (both still proposed)\n")
	if len(r.Failures) == 0 {
		fmt.Fprintf(&b, "  result               every bound held\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  result               %d failed\n", len(r.Failures))
	for _, f := range r.Failures {
		fmt.Fprintf(&b, "    - %s\n", f)
	}
	return b.String()
}

// within reports the age of the oldest row of a kind and fails the run when it sits outside the
// window that kind is kept for. An empty table is nothing to check; a value that cannot be read
// is a failure, because skipping it would delete the assertion and print an age of zero.
func within(r *report, kind string, oldest any, now time.Time, window, slack time.Duration) time.Duration {
	age, err := ageOf(oldest, now)
	if err != nil {
		r.Failures = append(r.Failures, fmt.Sprintf("the %s retention bound went unchecked: %v", kind, err))
		return 0
	}
	if age > window+slack {
		r.Failures = append(r.Failures, fmt.Sprintf("retention fell behind: a %s is %s old against a %s window", kind, age.Round(time.Second), window))
	}
	return age
}
