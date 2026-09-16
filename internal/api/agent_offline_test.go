package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/config"
	"github.com/Busness-app/kyyard-server/internal/store"
	"github.com/Busness-app/kyyard-server/internal/testdb"
)

// A predecessor's offline write must not demote the successor that now holds the endpoint.
func TestMarkOfflineSkipsWhenASuccessorHoldsTheEndpoint(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dbCfg := testdb.Config(t)
	dbCfg.DataDir = cfg.Database.DataDir
	cfg.Database = dbCfg
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer(cfg, st)
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().CreateUser(ctx, &store.User{ID: "admin", Username: "admin", Role: "user", SSOProvider: "local", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "admin", Role: store.RoleOrganizationAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	access := store.TenantAccess{ActorID: "admin", OrganizationID: "a", EnvironmentID: "env-a"}
	tok, err := ts.CreateEnrollmentToken(ctx, access, "docker", "")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	e, err := ts.Enroll(ctx, store.EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ApproveEndpoint(ctx, store.TenantAccess{ActorID: "admin", OrganizationID: "a"}, e.ID, e.Fingerprint); err != nil {
		t.Fatal(err)
	}

	first := &agentConn{endpointID: e.ID, closed: make(chan struct{})}
	if s.agents.add(first) != nil {
		t.Fatal("first add")
	}
	// The successor takes the slot while the predecessor is still finishing up.
	s.agents.remove(first)
	second := &agentConn{endpointID: e.ID, closed: make(chan struct{})}
	if s.agents.add(second) != nil {
		t.Fatal("second add")
	}
	if ok, err := ts.AcceptInventory(ctx, e.ID, 5); err != nil || !ok {
		t.Fatalf("inventory: %v %v", ok, err)
	}
	s.markOffline(ctx, first) // the predecessor's late write
	if state, _ := ts.EndpointState(ctx, e.ID); state != "active" {
		t.Fatalf("predecessor demoted the live successor: %s", state)
	}
	s.markOffline(ctx, second)
	if state, _ := ts.EndpointState(ctx, e.ID); state != "offline" {
		t.Fatalf("owner could not mark offline: %s", state)
	}
}
