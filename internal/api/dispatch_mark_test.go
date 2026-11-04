package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

// hangingMark is a store whose dispatch mark waits for its context, as on a PostgreSQL row lock.
type hangingMark struct {
	store.Store
	tenancy hangingTenancy
}

func (h hangingMark) Tenancy() store.TenancyStore { return h.tenancy }

type hangingTenancy struct{ store.TenancyStore }

func (hangingTenancy) MarkCommandDispatched(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// A dispatch mark that cannot be written gives up within its budget, releases the endpoint's
// dispatch lock (an eviction must not wait on the database), and settles the command unknown,
// since no sweep finds an unmarked command.
func TestAStuckDispatchMarkReleasesTheEndpointAndSettlesUnknown(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dbCfg := testdb.Config(t)
	dbCfg.DataDir = cfg.Database.DataDir
	cfg.Database = dbCfg
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer(cfg, st)
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	_ = st.Users().CreateUser(ctx, &store.User{ID: "admin", Username: "admin", Role: "user", SSOProvider: "local", Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "admin", Role: store.RoleOrganizationAdmin, Status: "active"})
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
	org := store.TenantAccess{ActorID: "admin", OrganizationID: "a"}
	if err := ts.ApproveEndpoint(ctx, org, e.ID, e.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if ok, err := ts.AcceptInventory(ctx, e.ID, uint64(time.Now().Unix()), time.Now(), []byte(`{"containers":[]}`)); err != nil || !ok {
		t.Fatalf("inventory: %t %v", ok, err)
	}
	cmd, err := ts.CreateCommand(ctx, access, e.ID, protocol.ActionRestart, "web", "", protocol.Expectation{})
	if err != nil {
		t.Fatal(err)
	}
	conn := &agentConn{endpointID: e.ID, fingerprint: e.Fingerprint, ip: "10.0.0.1", conn: deadPeer(t, ctx), send: make(chan protocol.Envelope, 8), closed: make(chan struct{})}
	if s.agents.add(conn) != nil {
		t.Fatal("registry not empty")
	}
	defer func(old time.Duration) { markBudget = old }(markBudget)
	markBudget = 200 * time.Millisecond
	s.store = hangingMark{Store: st, tenancy: hangingTenancy{st.Tenancy()}}

	start := time.Now()
	if !s.sendCommand(ctx, e.ID, cmd.ID, protocol.Envelope{Type: protocol.TypeCommand}) {
		t.Fatal("the connection did not take the command")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the mark held the endpoint for %s", took)
	}
	lock := s.dispatchLock(e.ID)
	if !lock.TryLock() {
		t.Fatal("the dispatch lock is still held")
	}
	lock.Unlock()
	got, err := ts.ReadCommand(ctx, org, e.ID, cmd.ID)
	if err != nil || got.Outcome != protocol.OutcomeUnknown {
		t.Fatalf("an unmarked command: %+v %v", got, errors.Unwrap(err))
	}
}
