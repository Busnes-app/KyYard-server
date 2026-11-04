package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/sso"
)

// A local account sees the identity linked to it and removes it with its password; the
// provider's sessions end with the link and the password session stays.
func TestAccountListsAndUnlinksSSOIdentity(t *testing.T) {
	ctx := context.Background()
	f := newFakeIdP(t)
	srv, st, cfg := setupTestServer(t)
	plain, _ := json.Marshal([]sso.Provider{{ID: "idp_a", Name: "KyIdentity", Kind: "oidc", Issuer: f.issuer, ClientID: "yard-a", Enabled: true}})
	sealed, _ := crypto.EncryptAESGCM(plain, crypto.DeriveKey(cfg.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err := st.Settings().SetSetting(ctx, "sso_providers_enc", sealed); err != nil {
		t.Fatal(err)
	}
	local := loginAs(t, srv, st, "carol", "user")
	carol, err := st.Users().GetUserByUsername(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Users().LinkSSO(ctx, carol.ID, crypto.SHA256Hex([]byte(local.Value)), carol.PasswordHash, "idp_a", "intruder-sub", "10.0.0.7"); err != nil {
		t.Fatal(err)
	}
	viaProvider := sessionCookie(ssoLogin(t, srv, f, "idp_a", "yard-a", "intruder-sub", "sid-1"))
	if !signedIn(t, srv, viaProvider) {
		t.Fatal("the linked identity did not sign in")
	}

	w := do(t, srv, "GET", "/api/account/sso", local)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"provider":"idp_a"`) || !strings.Contains(w.Body.String(), `"name":"KyIdentity"`) || !strings.Contains(w.Body.String(), `"subject":"intruder-sub"`) || !strings.Contains(w.Body.String(), `"linked_at"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	unlink := func(provider, pw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/account/sso/"+provider+"/unlink", strings.NewReader(`{"password":"`+pw+`"}`))
		req.AddCookie(local)
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
		req.Header.Set(auth.HeaderCSRF, "test-csrf")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}
	if w := unlink("idp_a", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("unlink with a wrong password: %d", w.Code)
	}
	if w := unlink("idp_other", "SuperSecretPass123!"); w.Code != http.StatusNotFound {
		t.Fatalf("unlink of an unlinked provider: %d", w.Code)
	}
	if w := unlink("idp_a", "SuperSecretPass123!"); w.Code != http.StatusOK {
		t.Fatalf("unlink: %d %s", w.Code, w.Body)
	}
	if signedIn(t, srv, viaProvider) {
		t.Fatal("the provider's session survived the unlink")
	}
	if !signedIn(t, srv, local) {
		t.Fatal("the unlink ended the password session")
	}
	if w := do(t, srv, "GET", "/api/account/sso", local); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("list after unlink: %d %s", w.Code, w.Body)
	}
	if u, err := st.Users().GetUserBySSO(ctx, "idp_a", "intruder-sub"); err == nil && u.ID == carol.ID {
		t.Fatal("the identity still resolves to the account")
	}
	rows, _, _ := st.Audit().ListAuditRecords(ctx, 0, 200)
	found := false
	for _, r := range rows {
		found = found || (r.Action == "auth.sso.unlink" && r.UserID == carol.ID && r.Resource == "idp_a")
	}
	if !found {
		t.Fatal("no auth.sso.unlink audit row")
	}
}
