package sso

import (
	"context"

	"github.com/Busness-app/ky_server_base/internal/config"
	"github.com/Busness-app/ky_server_base/internal/store"
)

type GenericOIDCClient struct {
	flow *oauthFlow
}

func NewGenericOIDCClient(cfg config.SSOConfig, _ store.Store) *GenericOIDCClient {
	return &GenericOIDCClient{flow: newOAuthFlow(cfg.GenericOIDCIssuer, cfg.GenericOIDCClientID, cfg.GenericOIDCSecret)}
}

func (g *GenericOIDCClient) BuildAuthURL(ctx context.Context, redirectURI, state, verifier, nonce string) (string, error) {
	return g.flow.authCodeURL(ctx, redirectURI, state, verifier, nonce)
}

func (g *GenericOIDCClient) ExchangeCode(ctx context.Context, code, verifier, redirectURI, expectedNonce string) (*IdentityClaims, error) {
	claims, err := g.flow.exchange(ctx, code, verifier, redirectURI, expectedNonce)
	if err != nil {
		return nil, err
	}
	claims.Provider = "oidc"
	return claims, nil
}
