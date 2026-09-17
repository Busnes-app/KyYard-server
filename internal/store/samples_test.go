package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/store"
)

func TestSamplesAreBoundedScopedAndPruned(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	ts := st.Tenancy()
	a.EnvironmentID = "env-a"
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	mustTenant(t, err)
	key := newAgentKey(t)
	e, err := ts.Enroll(ctx, key.request(tok.Secret, "host"))
	mustTenant(t, err)
	org := store.TenantAccess{ActorID: a.ActorID, OrganizationID: "a"}

	// Frame bounds and unsafe IDs are refused before any row lands.
	too := protocol.Metrics{ObservedAt: time.Now(), Samples: make([]protocol.Sample, protocol.MaxSamples+1)}
	if err := ts.RecordSamples(ctx, e.ID, too); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("oversized frame accepted: %v", err)
	}
	if err := ts.RecordSamples(ctx, e.ID, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "bad\nid"}}}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("unsafe container id accepted: %v", err)
	}
	// A skewed observation is stamped with the server's time.
	skewed := time.Now().Add(-2 * time.Hour)
	mustTenant(t, ts.RecordSamples(ctx, e.ID, protocol.Metrics{ObservedAt: skewed, Samples: []protocol.Sample{{ContainerID: "c1", CPUPercent: 12.5, MemoryBytes: 100, MemoryLimit: 1000, Pids: 2}}}))
	latest, err := ts.LatestSamples(ctx, org, e.ID)
	mustTenant(t, err)
	if len(latest) != 1 || latest[0].CPUPercent != 12.5 || time.Since(latest[0].ObservedAt) > time.Minute {
		t.Fatalf("skewed sample not re-stamped: %+v", latest)
	}
	// Newest per container wins; other containers are independent.
	// Inside the cadence a repeat is dropped; a minute later it lands.
	mustTenant(t, ts.RecordSamples(ctx, e.ID, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c1", CPUPercent: 99}}}))
	mustTenant(t, ts.RecordSamples(ctx, e.ID, protocol.Metrics{ObservedAt: time.Now().Add(60 * time.Second), Samples: []protocol.Sample{{ContainerID: "c1", CPUPercent: 30}, {ContainerID: "c2", CPUPercent: -1}}}))
	latest, err = ts.LatestSamples(ctx, org, e.ID)
	mustTenant(t, err)
	if len(latest) != 2 || latest[0].ContainerID != "c1" || latest[0].CPUPercent != 30 || latest[1].CPUPercent != -1 {
		t.Fatalf("latest per container: %+v", latest)
	}
	series, err := ts.ReadSamples(ctx, org, e.ID, "c1", time.Hour)
	mustTenant(t, err)
	if len(series) != 2 || series[0].CPUPercent != 12.5 {
		t.Fatalf("series: %+v", series)
	}
	// Scope: another organization, or the wrong environment, sees nothing.
	if _, err := ts.LatestSamples(ctx, store.TenantAccess{ActorID: a.ActorID, OrganizationID: "b"}, e.ID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("cross-tenant samples: %v", err)
	}
	if _, err := ts.ReadSamples(ctx, store.TenantAccess{ActorID: a.ActorID, OrganizationID: "a", EnvironmentID: "env-b"}, e.ID, "c1", time.Hour); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong-environment samples: %v", err)
	}
	// Nothing to prune yet; then nothing older than retention survives a prune.
	n, err := ts.Prune(ctx)
	mustTenant(t, err)
	if n != 0 {
		t.Fatalf("pruned fresh rows: %d", n)
	}
}
