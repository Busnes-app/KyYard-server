package sso

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const backchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"

// LogoutClaims names the login an OpenID Connect Back-Channel Logout token ended.
// SessionID is empty when the provider ended every login of Subject issued by IssuedAt.
type LogoutClaims struct {
	Subject   string
	SessionID string
	IssuedAt  time.Time
}

// VerifyLogout verifies a back-channel logout token signed by this OIDC provider for its client.
func (p Provider) VerifyLogout(ctx context.Context, raw string) (*LogoutClaims, error) {
	if p.Kind != "oidc" {
		return nil, ErrInvalidLogoutToken
	}
	return newOAuthFlow(p.Issuer, p.ClientID, p.ClientSecret).verifyLogout(ctx, raw)
}

// verifyLogout checks a logout token per OpenID Connect Back-Channel Logout 1.0 §2.6:
// provider signature, issuer, audience and expiry, then the claims that keep an ID
// token from being replayed as one.
func (f *oauthFlow) verifyLogout(ctx context.Context, raw string) (*LogoutClaims, error) {
	ctx = strongOIDCContext(ctx)
	if _, _, err := f.config(ctx, ""); err != nil {
		return nil, err
	}
	token, err := f.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, ErrInvalidLogoutToken
	}
	if typ, err := jwtType(raw); err != nil || (typ != "" && typ != "logout+jwt" && typ != "application/logout+jwt") {
		return nil, ErrInvalidLogoutToken
	}
	var claims struct {
		Sub    string                     `json:"sub"`
		SID    string                     `json:"sid"`
		IAT    int64                      `json:"iat"`
		Nonce  *json.RawMessage           `json:"nonce"`
		Events map[string]json.RawMessage `json:"events"`
	}
	if err := token.Claims(&claims); err != nil || claims.Nonce != nil || claims.IAT == 0 || (claims.Sub == "" && claims.SID == "") {
		return nil, ErrInvalidLogoutToken
	}
	var event map[string]any
	if err := json.Unmarshal(claims.Events[backchannelLogoutEvent], &event); err != nil || event == nil {
		return nil, ErrInvalidLogoutToken
	}
	return &LogoutClaims{Subject: claims.Sub, SessionID: claims.SID, IssuedAt: time.Unix(claims.IAT, 0).UTC()}, nil
}

// jwtType reads the protected header's typ; call it only after the signature verified.
func jwtType(raw string) (string, error) {
	header, _, _ := strings.Cut(raw, ".")
	decoded, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return "", err
	}
	var h struct {
		Typ string `json:"typ"`
	}
	err = json.Unmarshal(decoded, &h)
	return strings.ToLower(h.Typ), err
}
