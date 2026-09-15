package backup_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Busness-app/ky-primitives/recoveryclient"
)

// The scaffold builds the lib client from KY_BACKUP_ALLOW_PRIVATE_RECOVERY alone. Pin the
// contract that switch buys: RFC1918 and CGNAT are admitted only with it, loopback is never
// admitted, and HTTPS stays mandatory either way.
func TestClientOptionsAdmitOnlyPrivateAndCGNAT(t *testing.T) {
	for _, tc := range []struct {
		url     string
		private bool
		refused bool
	}{
		{"https://10.1.2.3/", false, true},
		{"https://10.1.2.3/", true, false},
		{"https://100.64.0.1/", false, true},
		{"https://100.64.0.1/", true, false},
		{"https://127.0.0.1/", false, true},
		{"https://127.0.0.1/", true, true},
		{"https://169.254.1.1/", true, true},
		{"http://recovery.example/", true, true},
		{"https://user:pw@recovery.example/", true, true},
	} {
		err := recoveryclient.ValidateURL(tc.url, tc.private)
		if (err != nil) != tc.refused {
			t.Errorf("%s private=%v: refused=%v (%v)", tc.url, tc.private, err != nil, err)
		}
	}
	// The constructed client applies the same rule before a byte leaves.
	c := recoveryclient.NewClient(recoveryclient.Options{AllowPrivate: true})
	_, err := c.ClaimPairing(context.Background(), "https://127.0.0.1:1/", "123456", "app", "app")
	if err == nil || !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("loopback with the switch on: %v", err)
	}
}
