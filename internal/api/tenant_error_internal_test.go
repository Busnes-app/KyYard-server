package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestTenantErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err       error
		code, msg string
	}{
		{store.ErrRemovalTooLarge, "removal_too_large", "This instance has more containers than KyYard removes in one operation; release it and remove the containers by hand"},
		{store.ErrRegistryNotConfigured, "registry_not_configured", "No registry is configured for this image's host and anonymous pulls are off"},
	} {
		w := httptest.NewRecorder()
		(&Server{}).tenantError(w, tc.err)
		var body struct{ Code, Error string }
		if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Code != tc.code || body.Error != tc.msg {
			t.Fatalf("%v: %d %s", tc.err, w.Code, w.Body.String())
		}
	}
}
