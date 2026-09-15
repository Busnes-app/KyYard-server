package scim_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busness-app/ky_server_base/internal/config"
	"github.com/Busness-app/ky_server_base/internal/scim"
	"github.com/Busness-app/ky_server_base/internal/store"
	"github.com/Busness-app/ky_server_base/internal/testdb"
)

func setupSCIMServer(t *testing.T) (*scim.Server, *http.ServeMux, string) {
	t.Helper()
	st, err := store.Open(context.Background(), testdb.Config(t))
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	token := "scim-secret-bearer-token"
	srv := scim.NewServer(st, config.SCIMConfig{
		Enabled:     true,
		BearerToken: token,
	}, "http://localhost:8080")

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return srv, mux, token
}

func TestSCIMUserLifecycle(t *testing.T) {
	srv, mux, token := setupSCIMServer(t)
	handler := srv.AuthMiddleware(mux)

	// 1. Create SCIM User
	userPayload := map[string]any{
		"schemas":     []string{scim.SchemaUser},
		"userName":    "scim_alice",
		"displayName": "Alice SCIM",
		"active":      true,
		"emails": []map[string]any{
			{"value": "scim_alice@busnes.app", "primary": true},
		},
	}
	body, _ := json.Marshal(userPayload)

	req := httptest.NewRequest("POST", "/scim/v2/Users", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	var created struct {
		ID       string `json:"id"`
		UserName string `json:"userName"`
		Active   bool   `json:"active"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.UserName != "scim_alice" || !created.Active {
		t.Errorf("unexpected created SCIM user: %+v", created)
	}

	// 2. Query SCIM Users
	req = httptest.NewRequest("GET", "/scim/v2/Users?filter=userName%20eq%20%22scim_alice%22", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}

	var listResp struct {
		TotalResults int `json:"totalResults"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if listResp.TotalResults != 1 {
		t.Errorf("expected 1 user result, got %d", listResp.TotalResults)
	}

	// 3. Patch SCIM User (Deactivate)
	patchPayload := map[string]any{
		"schemas": []string{scim.SchemaPatchOp},
		"Operations": []map[string]any{
			{"op": "replace", "path": "active", "value": false},
		},
	}
	patchBody, _ := json.Marshal(patchPayload)
	req = httptest.NewRequest("PATCH", "/scim/v2/Users/"+created.ID, bytes.NewReader(patchBody))
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after patch, got %d: %s", w.Code, w.Body.String())
	}

	var patched struct {
		Active bool `json:"active"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &patched)
	if patched.Active {
		t.Errorf("expected user to be deactivated after patch")
	}

	// 4. Delete SCIM User
	req = httptest.NewRequest("DELETE", "/scim/v2/Users/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 No Content, got %d", w.Code)
	}
}

func TestSCIMAuthEnforcement(t *testing.T) {
	srv, mux, _ := setupSCIMServer(t)
	handler := srv.AuthMiddleware(mux)

	// Missing token -> 401
	req := httptest.NewRequest("GET", "/scim/v2/Users", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for missing token, got %d", w.Code)
	}
}
