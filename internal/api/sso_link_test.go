package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/sso"
)

func TestSSOLinkRequiresBothIdentitiesAndKeepsTheLocalAccount(t *testing.T) {
	var challenge string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"profile-token","token_type":"Bearer"}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer profile-token" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"id":"identity-admin","login":"localadmin","name":"IdP admin"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer idp.Close()
	srv, st, cfg := setupTestServer(t)
	local := loginAs(t, srv, st, "localadmin", "admin")
	other := loginAs(t, srv, st, "other", "user")
	before, err := st.Users().GetUserByUsername(context.Background(), "localadmin")
	if err != nil {
		t.Fatal(err)
	}
	p := sso.Provider{ID: "idp_test", Name: "Fixture", Kind: "oauth2", ClientID: "yard", ClientSecret: "secret", AuthorizationURL: idp.URL + "/authorize", TokenURL: idp.URL + "/token", UserInfoURL: idp.URL + "/user", SubjectField: "id", UsernameField: "login", Enabled: true, AutoProvision: true}
	plain, _ := json.Marshal([]sso.Provider{p})
	sealed, _ := crypto.EncryptAESGCM(plain, crypto.DeriveKey(cfg.Security.EncryptionKey, "kyyard/sso/providers/v1"))
	if err := st.Settings().SetSetting(context.Background(), "sso_providers_enc", sealed); err != nil {
		t.Fatal(err)
	}
	start := func(cookie *http.Cookie, pass string, csrf bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"password": pass})
		r := httptest.NewRequest("POST", "/api/sso/idp_test/link", strings.NewReader(string(body)))
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf {
			r.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "x"})
			r.Header.Set(auth.HeaderCSRF, "x")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		cookie *http.Cookie
		pass   string
		csrf   bool
	}{{nil, "SuperSecretPass123!", true}, {local, "wrong", true}, {local, "SuperSecretPass123!", false}} {
		if w := start(tc.cookie, tc.pass, tc.csrf); w.Code == 200 {
			t.Fatal("unproved link accepted")
		}
	}
	callback := func(begin *httptest.ResponseRecorder, cookie *http.Cookie) *httptest.ResponseRecorder {
		var body struct {
			URL string `json:"authorization_url"`
		}
		if err := json.Unmarshal(begin.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		dest, _ := url.Parse(body.URL)
		challenge = dest.Query().Get("code_challenge")
		r := httptest.NewRequest("GET", "/api/sso/idp_test/callback?state="+dest.Query().Get("state")+"&code=valid-code", nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		for _, c := range begin.Result().Cookies() {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	// A password replacement after initiation invalidates the linking proof.
	stale := start(local, "SuperSecretPass123!", true)
	before.PasswordHash = "replaced"
	if err := st.Users().UpdateUser(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	if w := callback(stale, local); w.Code != 403 {
		t.Fatal("rotated password accepted", w.Code, w.Body)
	}
	otherUser, err := st.Users().GetUserByUsername(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	before.PasswordHash = otherUser.PasswordHash
	// Both fixture accounts use the same local password, but hashes have different salts.
	if err := st.Users().UpdateUser(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	// Same browser proof is insufficient when the local session changes.
	begin := start(local, "SuperSecretPass123!", true)
	if begin.Code != 200 {
		t.Fatal(begin.Code, begin.Body)
	}
	if w := callback(begin, other); w.Code != 403 {
		t.Fatal("session switched", w.Code, w.Body)
	}
	begin = start(local, "SuperSecretPass123!", true)
	w := callback(begin, local)
	if w.Code != 302 || w.Header().Get("Location") != "/settings?sso=linked" {
		t.Fatal(w.Code, w.Body)
	}
	if replay := callback(begin, local); replay.Code != 400 {
		t.Fatal("replayed link", replay.Code)
	}
	linked, err := st.Users().GetUserBySSO(context.Background(), p.ID, "identity-admin")
	if err != nil || linked.ID != before.ID || linked.PasswordHash != before.PasswordHash || linked.SSOProvider != "local" || linked.Role != "admin" {
		t.Fatalf("account lost: %+v %v", linked, err)
	}
	// A normal future SSO login resolves to that same account, instead of provisioning.
	login := do(t, srv, "GET", "/api/sso/idp_test/login", nil)
	dest, _ := url.Parse(login.Header().Get("Location"))
	challenge = dest.Query().Get("code_challenge")
	r := httptest.NewRequest("GET", "/api/sso/idp_test/callback?state="+dest.Query().Get("state")+"&code=valid-code", nil)
	for _, c := range login.Result().Cookies() {
		r.AddCookie(c)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != 302 {
		t.Fatal("linked sign-in", w.Code, w.Body)
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			session = c
		}
	}
	me := do(t, srv, "GET", "/api/auth/me", session)
	if me.Code != 200 || !strings.Contains(me.Body.String(), before.ID) {
		t.Fatal("different SSO account", me.Code, me.Body)
	}
	rows := auditRows(t, st, "auth.sso.link")
	if len(rows) != 1 || rows[0].UserID != before.ID || rows[0].Result != "success" {
		t.Fatal("missing atomic link audit", rows)
	}
}
