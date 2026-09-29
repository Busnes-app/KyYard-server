package docker

import (
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// A namespace mode reaches the create body only as the network mode the frame names; an
// attachment called host or container:<id> never becomes one.
func TestExplicitBodyNeverTakesTheModeFromANamespaceAttachment(t *testing.T) {
	for _, name := range []string{"host", "container:db"} {
		var body containerCreate
		explicitBody(&body, &protocol.ExplicitService{Networks: []protocol.NetworkAttachmentSpec{{Name: name}}})
		if body.HostConfig.NetworkMode != "" {
			t.Errorf("%s: network mode %q", name, body.HostConfig.NetworkMode)
		}
	}
	var body containerCreate
	explicitBody(&body, &protocol.ExplicitService{Networks: []protocol.NetworkAttachmentSpec{{Name: "shop_default"}}})
	if body.HostConfig.NetworkMode != "shop_default" {
		t.Errorf("a user network: %q", body.HostConfig.NetworkMode)
	}
}
