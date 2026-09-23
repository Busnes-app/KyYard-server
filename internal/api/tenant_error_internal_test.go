package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestTenantErrorRemovalTooLarge(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).tenantError(w, store.ErrRemovalTooLarge)
	var body struct{ Code, Error string }
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Code != "removal_too_large" || body.Error != "This instance has more containers than KyYard removes in one operation; release it and remove the containers by hand" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
