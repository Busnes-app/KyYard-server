package store

import (
	"context"
	"errors"
	"testing"
)

func TestDeleteEndpointRefusesWhileAnApplicationUsesIt(t *testing.T) {
	ctx := context.Background()
	st, a, app, endpoint, _ := adoptionFixture(t)
	ts := st.Tenancy()
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
		t.Fatal(err)
	}
	org := TenantAccess{ActorID: a.ActorID, OrganizationID: a.OrganizationID}
	if err := ts.RevokeEndpoint(ctx, org, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := ts.DeleteEndpoint(ctx, org, endpoint); !errors.Is(err, ErrInUse) {
		t.Fatalf("deleted an endpoint an application instance names: %v", err)
	}
	if _, err := ts.ReadEndpoint(ctx, org, endpoint); err != nil {
		t.Fatalf("refused deletion still removed the endpoint: %v", err)
	}
}
