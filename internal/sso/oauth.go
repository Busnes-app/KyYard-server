package sso

import (
	"context"
	"crypto/fips140"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type oauthFlow struct {
	issuer       string
	clientID     string
	clientSecret string
	mu           sync.Mutex
	provider     *oidc.Provider
	verifier     *oidc.IDTokenVerifier
}

func newOAuthFlow(issuer, clientID, clientSecret string) *oauthFlow {
	return &oauthFlow{issuer: strings.TrimRight(issuer, "/"), clientID: clientID, clientSecret: clientSecret}
}

func (f *oauthFlow) getProvider(ctx context.Context) (*oidc.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provider != nil {
		return f.provider, nil
	}
	if fips140.Enabled() {
		if err := requireOIDCHTTPS(f.issuer); err != nil {
			return nil, err
		}
	}
	ctx = strongOIDCContext(ctx)
	provider, err := oidc.NewProvider(ctx, f.issuer)
	if err != nil {
		return nil, err
	}
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, err
	}
	// Preserve the existing no-downgrade rule for HTTPS issuers in every mode.
	if fips140.Enabled() || strings.HasPrefix(f.issuer, "https://") {
		if err := requireOIDCHTTPS(provider.Endpoint().AuthURL, provider.Endpoint().TokenURL, metadata.JWKS); err != nil {
			return nil, err
		}
	}
	client := http.DefaultClient
	if configured, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		client = configured
	}
	jwksClient := *client
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	jwksClient.Transport = strongJWKSTransport{base: transport}
	if jwksClient.Timeout == 0 {
		jwksClient.Timeout = 10 * time.Second
	}
	// Keep maintained signature/claim verification, key caching and rotation.
	f.verifier = provider.VerifierContext(oidc.ClientContext(ctx, &jwksClient), &oidc.Config{ClientID: f.clientID})
	f.provider = provider
	return provider, nil
}

func (f *oauthFlow) config(ctx context.Context, redirectURI string) (*oauth2.Config, *oidc.Provider, error) {
	if f.issuer == "" || f.clientID == "" {
		return nil, nil, ErrProviderDisabled
	}
	provider, err := f.getProvider(ctx)
	if err != nil {
		return nil, nil, err
	}
	return &oauth2.Config{ClientID: f.clientID, ClientSecret: f.clientSecret, Endpoint: provider.Endpoint(), RedirectURL: redirectURI, Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}, provider, nil
}

func (f *oauthFlow) authCodeURL(ctx context.Context, redirectURI, state, verifier, nonce string) (string, error) {
	config, _, err := f.config(ctx, redirectURI)
	if err != nil {
		return "", err
	}
	return config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce)), nil
}

func (f *oauthFlow) exchange(ctx context.Context, code, verifier, redirectURI, expectedNonce string) (*IdentityClaims, error) {
	ctx = strongOIDCContext(ctx)
	config, _, err := f.config(ctx, redirectURI)
	if err != nil {
		return nil, err
	}
	token, err := config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, err
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, errors.New("missing id_token in token response")
	}
	idToken, err := f.verifier.Verify(ctx, rawIDToken)
	if err != nil || expectedNonce == "" || idToken.Nonce != expectedNonce {
		return nil, ErrInvalidIDToken
	}
	return claimsFromIDToken(idToken)
}

func requireOIDCHTTPS(endpoints ...string) error {
	for _, raw := range endpoints {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return errors.New("strong-crypto mode requires HTTPS OIDC issuer and endpoints without URL credentials")
		}
	}
	return nil
}

// Keep the caller's trust roots/transport; the selected profile refuses redirects
// for discovery, token exchange and JWKS rather than allowing an HTTP downgrade.
func strongOIDCContext(ctx context.Context) context.Context {
	if !fips140.Enabled() {
		return ctx
	}
	client := http.DefaultClient
	if configured, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		client = configured
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("OIDC redirects are refused in strong-crypto mode")
	}
	if copy.Timeout == 0 {
		copy.Timeout = 10 * time.Second
	}
	return oidc.ClientContext(ctx, &copy)
}

func claimsFromIDToken(idToken *oidc.IDToken) (*IdentityClaims, error) {
	// roles is KyIdentity's per-app role claim, a string array; any other non-null type rejects the token.
	var raw struct {
		Sub               string   `json:"sub"`
		Email             string   `json:"email"`
		Name              string   `json:"name"`
		PreferredUsername string   `json:"preferred_username"`
		Role              string   `json:"role"`
		AppRoles          []string `json:"roles"`
	}
	if err := idToken.Claims(&raw); err != nil || raw.Sub == "" {
		return nil, ErrInvalidIDToken
	}
	username := raw.PreferredUsername
	if username == "" {
		username = raw.Email
	}
	if username == "" {
		username = raw.Sub
	}
	return &IdentityClaims{Issuer: idToken.Issuer, AppRoles: raw.AppRoles, Subject: raw.Sub, Email: raw.Email, Name: raw.Name, PreferredUsername: username, Role: raw.Role}, nil
}
