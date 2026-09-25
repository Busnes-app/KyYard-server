package sso

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"
)

func TestHTTPSDiscoveryCannotDowngradeEndpoints(t *testing.T) {
	for _, bad := range []string{"authorization_endpoint", "token_endpoint", "jwks_uri"} {
		t.Run(bad, func(t *testing.T) {
			var issuer string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				values := map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys"}
				values[bad] = "http://cleartext.example.test/unsafe"
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(values)
			}))
			defer server.Close()
			issuer = server.URL
			ctx := context.WithValue(context.Background(), oauth2.HTTPClient, server.Client())
			p := Provider{Kind: "oidc", Issuer: issuer, ClientID: "yard"}
			if _, err := p.AuthURL(ctx, "https://yard.test/callback", "state", "verifier", "nonce"); err == nil {
				t.Fatal("accepted cleartext", bad)
			}
		})
	}
}
func TestProviderRequiresHTTPSAndStableProfileID(t *testing.T) {
	valid := Provider{Name: "Identity", Kind: "oauth2", ClientID: "yard", AuthorizationURL: "https://identity.test/auth", TokenURL: "https://identity.test/token", UserInfoURL: "https://identity.test/me", SubjectField: "id"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http://identity.test/me", "https://user:pass@identity.test/me", "javascript:alert(1)"} {
		p := valid
		p.UserInfoURL = bad
		if p.Validate() == nil {
			t.Fatal("accepted", bad)
		}
	}
	valid.SubjectField = ""
	if valid.Validate() == nil {
		t.Fatal("accepted missing stable ID mapping")
	}
	if got := profileField(map[string]any{"data": map[string]any{"id": json.Number("987654321012345678")}}, "data.id"); got != "987654321012345678" {
		t.Fatal("lost numeric identity precision", got)
	}
}
