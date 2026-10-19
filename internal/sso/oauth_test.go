package sso

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

func TestClaimsReadKyIdentityRolesExactly(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	verifier := oidc.NewVerifier("https://identity.test", &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}, &oidc.Config{ClientID: "yard"})
	for _, tc := range []struct {
		name  string
		roles any // omitted when nil
		want  []string
		ok    bool
	}{
		{"roles", []string{"kyyard.admin", "kyyard.viewer"}, []string{"kyyard.admin", "kyyard.viewer"}, true},
		{"empty", []string{}, []string{}, true},
		{"missing", nil, nil, true},
		{"null", json.RawMessage("null"), nil, true},
		{"string", "kyyard.admin", nil, false},
		{"numbers", []int{1}, nil, false},
		{"scim objects", []map[string]string{{"value": "kyyard.admin"}}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"iss": "https://identity.test", "aud": "yard", "sub": "owner", "exp": time.Now().Add(time.Minute).Unix(), "app_roles": []string{"kyyard.admin"}}
			if tc.roles != nil {
				body["roles"] = tc.roles
			}
			token, err := verifier.Verify(context.Background(), signRS256(t, key, body))
			if err != nil {
				t.Fatal(err)
			}
			claims, err := claimsFromIDToken(token)
			if !tc.ok {
				if err == nil {
					t.Fatal("accepted roles", claims.AppRoles)
				}
				return
			}
			// app_roles is not KyIdentity's claim and never grants a role.
			if err != nil || !slices.Equal(claims.AppRoles, tc.want) || (tc.want == nil) != (claims.AppRoles == nil) {
				t.Fatal(claims, err)
			}
		})
	}
}

func signRS256(t *testing.T, key *rsa.PrivateKey, body map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signed := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." + enc(body)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}
