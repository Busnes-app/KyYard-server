package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestDeleteEndpointOnlyRemovesRevokedAndFreesItsName(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	ts := st.Tenancy()
	a.EnvironmentID = "env-a"
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	mustTenant(t, err)
	e, err := ts.Enroll(ctx, newAgentKey(t).request(tok.Secret, "host"))
	mustTenant(t, err)
	org := store.TenantAccess{ActorID: a.ActorID, OrganizationID: "a"}
	mustTenant(t, ts.ApproveEndpoint(ctx, org, e.ID, e.Fingerprint))

	if err := ts.DeleteEndpoint(ctx, org, e.ID); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("deleted a live endpoint: %v", err)
	}
	mustTenant(t, ts.RevokeEndpoint(ctx, org, e.ID))
	tenantUser(t, st, "viewer", "user", "local", "active")
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "viewer", Role: store.RoleReadOnly, Status: "active"}))
	if err := ts.DeleteEndpoint(ctx, store.TenantAccess{ActorID: "viewer", OrganizationID: "a"}, e.ID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read-only deleted an endpoint: %v", err)
	}
	if err := ts.DeleteEndpoint(ctx, store.TenantAccess{ActorID: a.ActorID, OrganizationID: "b"}, e.ID); !errors.Is(err, store.ErrForbidden) && !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another organization deleted the endpoint: %v", err)
	}

	mustTenant(t, ts.DeleteEndpoint(ctx, org, e.ID))
	if _, err := ts.ReadEndpoint(ctx, org, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted endpoint still readable: %v", err)
	}
	if err := ts.DeleteEndpoint(ctx, org, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	// The name is free for the host's next enrollment.
	tok2, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	mustTenant(t, err)
	if _, err := ts.Enroll(ctx, newAgentKey(t).request(tok2.Secret, "host")); err != nil {
		t.Fatalf("re-enrollment under the freed name: %v", err)
	}
	records, err := ts.ReadAudit(ctx, org, 0, 200)
	mustTenant(t, err)
	var audited bool
	for _, r := range records {
		if r.Action == "endpoint.delete" && r.Resource == e.ID && r.UserID == a.ActorID && r.Result == "success" {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("deletion not audited: %+v", records)
	}
}
