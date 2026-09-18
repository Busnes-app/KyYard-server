package sso

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Provider supports OIDC discovery and OAuth2 providers with a JSON profile API.
// The subject mapping must name a stable provider-issued account ID, never an email.
type Provider struct {
	CallbackURL      string `json:"callback_url,omitempty"`
	ID               string `json:"id"`
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	Issuer           string `json:"issuer"`
	ClientID         string `json:"client_id"`
	ClientSecret     string `json:"client_secret,omitempty"`
	AuthorizationURL string `json:"authorization_url"`
	TokenURL         string `json:"token_url"`
	UserInfoURL      string `json:"userinfo_url"`
	Scopes           string `json:"scopes"`
	SubjectField     string `json:"subject_field"`
	UsernameField    string `json:"username_field"`
	NameField        string `json:"name_field"`
	EmailField       string `json:"email_field"`
	AuthMethod       string `json:"auth_method"`
	Enabled          bool   `json:"enabled"`
	AutoProvision    bool   `json:"auto_provision"`
	SecretSet        bool   `json:"secret_set,omitempty"`
}

func (p Provider) Validate() error {
	if p.Name == "" || len(p.Name) > 80 || p.ClientID == "" || len(p.ClientID) > 512 || len(p.ClientSecret) > 4096 || len(p.Scopes) > 1024 {
		return errors.New("provider name and client ID are required (within field limits)")
	}
	urls := []string{}
	switch p.Kind {
	case "oidc":
		urls = append(urls, p.Issuer)
	case "oauth2":
		urls = append(urls, p.AuthorizationURL, p.TokenURL, p.UserInfoURL)
		if p.SubjectField == "" {
			return errors.New("OAuth2 requires a stable subject field")
		}
	default:
		return errors.New("choose OIDC or OAuth2")
	}
	for _, v := range urls {
		u, e := url.Parse(v)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(v) > 2048 {
			return errors.New("provider URLs must be absolute HTTPS URLs without credentials or fragments")
		}
	}
	for _, v := range []string{p.SubjectField, p.UsernameField, p.NameField, p.EmailField} {
		if len(v) > 128 {
			return errors.New("profile field path too long")
		}
	}
	if p.AuthMethod != "" && p.AuthMethod != "basic" && p.AuthMethod != "post" {
		return errors.New("invalid token authentication method")
	}
	return nil
}

func ProviderContext(ctx context.Context) context.Context {
	// Administrators configure trusted identity infrastructure, including private KyIdentity
	// hosts. TLS stays verified and redirects are refused, especially on token/profile calls.
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
}
func (p Provider) oauthConfig(redirect string) *oauth2.Config {
	style := oauth2.AuthStyleInHeader
	if p.AuthMethod == "post" {
		style = oauth2.AuthStyleInParams
	}
	return &oauth2.Config{ClientID: p.ClientID, ClientSecret: p.ClientSecret, RedirectURL: redirect, Scopes: strings.Fields(p.Scopes), Endpoint: oauth2.Endpoint{AuthURL: p.AuthorizationURL, TokenURL: p.TokenURL, AuthStyle: style}}
}
func (p Provider) AuthURL(ctx context.Context, redirect, state, verifier, nonce string) (string, error) {
	if p.Kind == "oidc" {
		return newOAuthFlow(p.Issuer, p.ClientID, p.ClientSecret).authCodeURL(ctx, redirect, state, verifier, nonce)
	}
	return p.oauthConfig(redirect).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), nil
}
func (p Provider) Exchange(ctx context.Context, code, verifier, redirect, nonce string) (*IdentityClaims, error) {
	if p.Kind == "oidc" {
		return newOAuthFlow(p.Issuer, p.ClientID, p.ClientSecret).exchange(ctx, code, verifier, redirect, nonce)
	}
	token, err := p.oauthConfig(redirect).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	response, err := p.oauthConfig(redirect).Client(ctx, token).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, errors.New("profile request refused")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("profile response too large")
	}
	var profile map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err = decoder.Decode(&profile); err != nil {
		return nil, err
	}
	subject := profileField(profile, p.SubjectField)
	if subject == "" || len(subject) > 255 {
		return nil, errors.New("profile has no valid subject")
	}
	username := profileField(profile, p.UsernameField)
	if username == "" {
		username = subject
	}
	return &IdentityClaims{Subject: subject, PreferredUsername: username, Name: profileField(profile, p.NameField), Email: profileField(profile, p.EmailField)}, nil
}
func profileField(profile map[string]any, path string) string {
	var value any = profile
	for _, part := range strings.Split(path, ".") {
		m, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = m[part]
	}
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return ""
	}
}
