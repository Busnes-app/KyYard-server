package store

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
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

// On the built-in endpoint, exec, configuration and removal need a platform administrator too:
// the host runs KyYard itself. Other actions and other endpoints keep the tenant rule.
func TestLocalDockerProtectedActionsNeedPlatformAdmin(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	pub, _, _ := ed25519.GenerateKey(nil)
	if _, err := ts.InitializeLocalDocker(ctx, pub); err != nil {
		t.Fatal(err)
	}
	a.EnvironmentID = ""
	platform := policyMember(t, st, a, "platform", RoleOrganizationAdmin)
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE users SET role='admin' WHERE id=?`), "platform"); err != nil {
		t.Fatal(err)
	}
	remote := activeEndpointWith(t, ts, TenantAccess{ActorID: a.ActorID, OrganizationID: a.OrganizationID, EnvironmentID: "environment"}, nil, nil)
	for _, action := range []permissions.Action{permissions.ContainerExec, permissions.ContainerConfigure, permissions.ContainerDestroy} {
		if err := ts.CheckEndpointAccess(ctx, a, action, LocalDockerEndpointID); !errors.Is(err, ErrForbidden) {
			t.Errorf("organization admin %s on local: %v", action, err)
		}
		if err := ts.CheckEndpointAccess(ctx, platform, action, LocalDockerEndpointID); err != nil {
			t.Errorf("platform admin %s on local: %v", action, err)
		}
		if err := ts.CheckEndpointAccess(ctx, a, action, remote); err != nil {
			t.Errorf("organization admin %s on another endpoint: %v", action, err)
		}
	}
	if err := ts.CheckEndpointAccess(ctx, a, permissions.ContainerOperate, LocalDockerEndpointID); err != nil {
		t.Errorf("organization admin operate on local: %v", err)
	}
}
