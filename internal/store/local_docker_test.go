package store

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLocalDockerBootstrapScopeRestartAndRevocation(t *testing.T) {
	st, a := tenantAtomicStore(t)
	a.EnvironmentID = ""
	ctx := context.Background()
	pub, _, _ := ed25519.GenerateKey(nil)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.Tenancy().InitializeLocalDocker(ctx, pub); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	e, err := st.Tenancy().ReadEndpoint(ctx, a, LocalDockerEndpointID)
	if err != nil {
		t.Fatal(err)
	}
	if e.OrganizationID != InitialOrganizationID || e.State != "approved" || e.Facts["connection"] != "local" {
		t.Fatal(e)
	}
	other := a
	other.OrganizationID = "another-org"
	if _, err := st.Tenancy().ReadEndpoint(ctx, other, LocalDockerEndpointID); !errors.Is(err, ErrForbidden) {
		t.Fatal("cross-tenant access", err)
	}
	var audits int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_records WHERE action='endpoint.local_connected'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("bootstrap audit", audits, err)
	}
	if _, err := st.Tenancy().AcceptInventory(ctx, LocalDockerEndpointID, 50, time.Now(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if generation, err := st.Tenancy().InitializeLocalDocker(ctx, pub); err != nil || generation != 50 {
		t.Fatal("restart generation", generation, err)
	}
	changed, _, _ := ed25519.GenerateKey(nil)
	if _, err := st.Tenancy().InitializeLocalDocker(ctx, changed); !errors.Is(err, ErrForbidden) {
		t.Fatal("identity replaced", err)
	}
	if err := st.Tenancy().RevokeEndpoint(ctx, a, LocalDockerEndpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Tenancy().InitializeLocalDocker(ctx, pub); !errors.Is(err, ErrForbidden) {
		t.Fatal("revoked authority restored", err)
	}
	a.EnvironmentID = e.EnvironmentID
	if err := st.Tenancy().RemoveEnvironment(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Tenancy().InitializeLocalDocker(ctx, pub); !errors.Is(err, ErrForbidden) {
		t.Fatal("deleted authority recreated", err)
	}
}
