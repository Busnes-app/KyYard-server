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
		if rows > r.MaxRowsEndpoint {
			r.MaxRowsEndpoint = rows
		}
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
	if age, ok := ageOf(oldestSample, now); ok {
		r.OldestSample = age
		// One retention interval of slack: a row may age out between prune and this check.
		if age > store.SampleRetention+2*time.Minute {
			r.Failures = append(r.Failures, fmt.Sprintf("retention fell behind: a sample is %s old against a %s window", age.Round(time.Second), store.SampleRetention))
		}
	}
	if age, ok := ageOf(oldestRollup, now); ok {
		r.OldestRollup = age
		if age > store.RollupRetention+time.Hour {
			r.Failures = append(r.Failures, fmt.Sprintf("a summary is %s old against a %s window", age.Round(time.Second), store.RollupRetention))
		}
	}

	if r.PeakUsage >= r.Budget && r.PressureSeen[store.PressureNormal] == 0 {
		r.Failures = append(r.Failures, "the budget was exceeded and telemetry never returned to normal")
	}
	if r.DenialsRefused == 0 {
		r.Failures = append(r.Failures, "no refusal was exercised, so the denial path proves nothing")
	}
	if r.DenialsLeaked > 0 {
		r.Failures = append(r.Failures, fmt.Sprintf("%d refusals leaked through", r.DenialsLeaked))
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

func ageOf(v any, now time.Time) (time.Duration, bool) {
	if v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case time.Time:
		return now.Sub(t.UTC()), true
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05.999999999Z07:00"} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return now.Sub(parsed.UTC()), true
			}
		}
	case []byte:
		return ageOf(string(t), now)
	}
	return 0, false
}

func (r *report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nsoak report\n")
	fmt.Fprintf(&b, "  samples written      %d\n", r.SamplesWritten)
	fmt.Fprintf(&b, "  summary rows written %d\n", r.RollupRows)
	fmt.Fprintf(&b, "  refusals exercised   %d (leaked %d)\n", r.DenialsRefused, r.DenialsLeaked)
	fmt.Fprintf(&b, "  rows, worst endpoint %d of %d allowed\n", r.MaxRowsEndpoint, r.Ceiling)
	fmt.Fprintf(&b, "  oldest sample        %s\n", r.OldestSample.Round(time.Second))
	fmt.Fprintf(&b, "  oldest summary       %s\n", r.OldestRollup.Round(time.Second))
	fmt.Fprintf(&b, "  peak usage           %d of %d bytes\n", r.PeakUsage, r.Budget)
	fmt.Fprintf(&b, "  p95 list read        %s over %d reads\n", r.ReadP95.Round(time.Millisecond), r.Ticks)
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
