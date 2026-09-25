package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// runServer's store setup must settle what a previous process left in flight; deleting the
// reconcile step from startStore fails here.
func TestStartStoreSettlesInFlightCommands(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "ky.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ts := st.Tenancy()
	must(st.Users().CreateUser(ctx, &store.User{ID: "actor", Username: "actor", Role: "user", Status: "active", SSOProvider: "local"}))
	must(ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: store.InitialOrganizationID, UserID: "actor", Role: store.RoleOrganizationAdmin, Status: "active"}))
	must(ts.CreateEnvironment(ctx, &store.Environment{ID: "env", OrganizationID: store.InitialOrganizationID, Name: "Env"}))
	a := store.TenantAccess{ActorID: "actor", OrganizationID: store.InitialOrganizationID, EnvironmentID: "env", CorrelationID: "test"}

	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	must(err)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	enrolled, err := ts.Enroll(ctx, store.EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "host"})
	must(err)
	must(ts.ApproveEndpoint(ctx, a, enrolled.ID, enrolled.Fingerprint))
	snap := protocol.Snapshot{Generation: 1, Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}
	raw, err := json.Marshal(snap)
	must(err)
	if ok, err := ts.AcceptInventory(ctx, enrolled.ID, snap.Generation, time.Now().UTC(), raw); err != nil || !ok {
		t.Fatalf("inventory: %v %v", ok, err)
	}
	cmd, err := ts.CreateCommand(ctx, a, enrolled.ID, protocol.ActionRestart, "web", "", protocol.Expectation{})
	must(err)
	must(ts.MarkCommandDispatched(ctx, cmd.ID))

	must(startStore(ctx, st))
	got, err := ts.ReadCommand(ctx, a, enrolled.ID, cmd.ID)
	must(err)
	if got.Outcome != protocol.OutcomeUnknown || got.SettledAt == nil {
		t.Fatalf("in-flight command after startStore: %+v", got)
	}
}
