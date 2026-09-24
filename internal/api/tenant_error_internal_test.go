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
		status    int
		code, msg string
	}{
		{store.ErrRemovalTooLarge, 409, "removal_too_large", "This instance has more containers than KyYard removes in one operation; release it and remove the containers by hand"},
		{store.ErrRegistryNotConfigured, 409, "registry_not_configured", "No registry is configured for this image's host and anonymous pulls are off"},
		{store.ErrMappingRequired, 409, "mapping_required", "Map the application's services to adopted containers first"},
		{errCheckInProgress, 409, "check_in_progress", "An update check for this application is already running"},
		{store.ErrPrivateRegistriesDisabled, 403, "private_registries_disabled", "Private-address registries are disabled by the operator (KY_REGISTRY_ALLOW_PRIVATE)"},
	} {
		w := httptest.NewRecorder()
		(&Server{}).tenantError(w, tc.err)
		var body struct{ Code, Error string }
		if w.Code != tc.status || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Code != tc.code || body.Error != tc.msg {
			t.Fatalf("%v: %d %s", tc.err, w.Code, w.Body.String())
		}
	}
}
