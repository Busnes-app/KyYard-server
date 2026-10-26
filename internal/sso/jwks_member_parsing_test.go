package sso_test

import (
	"context"
	"crypto"
	"crypto/fips140"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/sso"
	"github.com/coreos/go-oidc/v3/oidc"
)

// Check the key used by the real signature verifier, including JWK member
// casing and order. Transport rejection alone cannot prove that alignment.
func TestOIDCJWKMemberParsingMatchesVerifier(t *testing.T) {
	for _, bits := range []int{1024, 2047, 2048} {
		key, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			t.Fatal(err)
		}
		for _, variant := range []struct{ name, before, after, envelope string }{
			{name: "canonical", envelope: "keys"},
			{name: "uppercase type", after: `,"KTY":"EC"`, envelope: "keys"},
			{name: "mixed-case type", after: `,"Kty":"EC"`, envelope: "keys"},
			{name: "uppercase modulus", after: `,"N":"` + base64.RawURLEncoding.EncodeToString(new(big.Int).Lsh(big.NewInt(1), 2047).Bytes()) + `"`, envelope: "keys"},
			{name: "uppercase short modulus", after: `,"N":"AQ"`, envelope: "keys"},
			{name: "aliases before canonical", before: `"KTY":"EC","N":"AQ",`, envelope: "keys"},
			{name: "type and modulus aliases", after: `,"KTY":"EC","N":"AQ"`, envelope: "keys"},
			{name: "uppercase envelope", after: `,"KTY":"EC"`, envelope: "KEYS"},
		} {
			for _, provider := range []string{"kysignon", "generic", "configured-provider"} {
				t.Run(fmt.Sprintf("%s/RSA%d/%s", provider, bits, variant.name), func(t *testing.T) {
					var issuer, token string
					var fetches atomic.Int32
					idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/.well-known/openid-configuration":
							_ = json.NewEncoder(w).Encode(map[string]any{
								"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
								"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys",
								"id_token_signing_alg_values_supported": []string{"RS256"},
							})
						case "/keys":
							fetches.Add(1)
							_, _ = fmt.Fprintf(w, `{"%s":[{%s"kty":"RSA","kid":"fixture","alg":"RS256","e":"AQAB","n":"%s"%s}]}`,
								variant.envelope, variant.before, base64.RawURLEncoding.EncodeToString(key.N.Bytes()), variant.after)
						case "/token":
							_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "fixture", "token_type": "Bearer", "id_token": token})
						default:
							http.NotFound(w, r)
						}
					}))
					defer idp.Close()
					issuer = idp.URL
					token = signedIDToken(t, key, issuer)
					ctx := oidc.ClientContext(context.Background(), idp.Client())
					cfg := config.SSOConfig{KySignOnIssuer: issuer, KySignOnClientID: "client", GenericOIDCIssuer: issuer, GenericOIDCClientID: "client"}
					exchange := sso.NewKySignOnClient(cfg, nil).ExchangeCode
					if provider == "generic" {
						exchange = sso.NewGenericOIDCClient(cfg, nil).ExchangeCode
					}
					if provider == "configured-provider" {
						exchange = (sso.Provider{Kind: "oidc", Issuer: issuer, ClientID: "client"}).Exchange
					}
					claims, err := exchange(ctx, "code", "verifier", "https://app.example/callback", "nonce")
					if bits < 2048 {
						if !errors.Is(err, sso.ErrInvalidIDToken) || claims != nil {
							t.Fatalf("accepted RSA-%d token: claims=%+v err=%v", bits, claims, err)
						}
					} else if err != nil || claims.Subject != "subject" {
						t.Fatalf("strong token: claims=%+v err=%v", claims, err)
					} else {
						if _, err := exchange(ctx, "code", "verifier", "https://app.example/callback", "nonce"); err != nil {
							t.Fatal(err)
						}
						wantFetches := int32(1)
						if provider == "configured-provider" {
							wantFetches = 2
						}
						if fetches.Load() != wantFetches {
							t.Fatalf("JWKS cache not retained: %d", fetches.Load())
						}
						if claims, err := exchange(ctx, "code", "verifier", "https://app.example/callback", "wrong"); !errors.Is(err, sso.ErrInvalidIDToken) || claims != nil {
							t.Fatal("accepted wrong nonce")
						}
					}
				})
			}
		}
	}
}

func signedIDToken(t *testing.T, key *rsa.PrivateKey, issuer string) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := json.Marshal(map[string]any{
		"iss": issuer, "aud": "client", "sub": "subject", "nonce": "nonce",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestOIDCStrongModeRequiresHTTPSMetadata(t *testing.T) {
	for _, field := range []string{"issuer", "authorization_endpoint", "token_endpoint", "jwks_uri", "discovery redirect"} {
		t.Run(field, func(t *testing.T) {
			var issuer string
			var requests atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if field == "discovery redirect" && r.URL.Path != "/discovery" {
					http.Redirect(w, r, "/discovery", http.StatusFound)
					return
				}
				metadata := map[string]any{
					"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
					"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys",
				}
				if field != "issuer" && field != "discovery redirect" {
					metadata[field] = "http://id.example.invalid/endpoint"
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(metadata)
			})
			var idp *httptest.Server
			if field == "issuer" {
				idp = httptest.NewServer(handler)
			} else {
				idp = httptest.NewTLSServer(handler)
			}
			defer idp.Close()
			issuer = idp.URL
			ctx := oidc.ClientContext(context.Background(), idp.Client())
			client := sso.NewKySignOnClient(config.SSOConfig{KySignOnIssuer: issuer, KySignOnClientID: "client"}, nil)
			_, err := client.BuildAuthURL(ctx, "https://app.example/callback", "state", "verifier", "nonce")
			refuse := fips140.Enabled() || (field != "issuer" && field != "discovery redirect")
			if (err != nil) != refuse {
				t.Fatalf("mode=%v, discovery error=%v", fips140.Enabled(), err)
			}
			if field == "issuer" && fips140.Enabled() && requests.Load() != 0 {
				t.Fatal("contacted a plaintext issuer in strong mode")
			}
		})
	}
}
