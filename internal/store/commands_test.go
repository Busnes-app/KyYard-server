package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// The container identifier becomes part of a URL addressed to the host's root-equivalent
// socket. displaySafe is the wrong gate for that: it permits a slash, a query and a fragment,
// any of which chooses a different Engine API route rather than a different container.
func TestCommandsRefuseAnIdentifierThatIsNotAContainerName(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	for _, name := range []string{
		"c1/../../images/json?all=1",
		"c1/json",
		"../secrets",
		"c1?all=1",
		"c1#fragment",
		"c1%2fjson",
		"",
		"-leading-dash",
	} {
		if _, err := ts.CreateCommand(ctx, a, "ep_any", protocol.ActionRestart, name, "", protocol.Expectation{}); err == nil {
			t.Fatalf("%q was accepted as a container name", name)
		}
	}
	// A real name and a real ID both still work; the check must not be so tight it refuses
	// what Docker itself hands out.
	for _, name := range []string{"web", "my_app-1.2", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		_, err := ts.CreateCommand(ctx, a, "ep_missing", protocol.ActionRestart, name, "", protocol.Expectation{})
		if err == nil {
			t.Fatalf("%q reached an endpoint that does not exist", name)
		}
		if err.Error() == "invalid tenant input: container" {
			t.Fatalf("%q was rejected as a bad container name", name)
		}
	}
}

// A name is a label the runtime reassigns. A destructive command must travel as the container
// the confirmation was checked against, or a recreate between inventory and execution would
// mean confirming one container and destroying another.
func TestADestructiveCommandCarriesTheConfirmedIdentity(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	enrolled, err := ts.Enroll(ctx, EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ApproveEndpoint(ctx, a, enrolled.ID, enrolled.Fingerprint); err != nil {
		t.Fatal(err)
	}
	snap := protocol.Snapshot{
		Generation: uint64(time.Now().Unix()),
		Containers: []protocol.Container{{ID: "c1", Name: "web", State: "exited", Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}}},
		Images:     []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ts.AcceptInventory(ctx, enrolled.ID, snap.Generation, time.Now().UTC(), raw); err != nil || !ok {
		t.Fatalf("inventory: %v %v", ok, err)
	}

	cmd, err := ts.CreateCommand(ctx, a, enrolled.ID, protocol.ActionRemove, "web", "web", protocol.Expectation{State: "exited"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.ContainerID != "c1" {
		t.Fatalf("a removal confirmed against c1 travels as %q, so a recreate could redirect it", cmd.ContainerID)
	}
	// Addressing it by ID is the same command.
	byID, err := ts.CreateCommand(ctx, a, enrolled.ID, protocol.ActionRemove, "c1", "web", protocol.Expectation{State: "exited"})
	if err != nil || byID.ContainerID != "c1" {
		t.Fatalf("removal by ID: %+v %v", byID, err)
	}
	// A container the server has never seen cannot be confirmed at all.
	if _, err := ts.CreateCommand(ctx, a, enrolled.ID, protocol.ActionRemove, "ghost", "ghost", protocol.Expectation{State: "exited"}); err == nil {
		t.Fatal("a container absent from inventory was accepted for removal")
	}
}
