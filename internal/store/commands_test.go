package store

import (
	"context"
	"testing"

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
