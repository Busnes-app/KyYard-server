package sso_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/sso"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

func TestOAuthAuthorizationURLUsesDiscoveryAndPKCE(t *testing.T) {
	var issuer string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
			"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys",
		})
	}))
	defer idp.Close()
	issuer = idp.URL

	client := sso.NewKySignOnClient(config.SSOConfig{KySignOnIssuer: issuer, KySignOnClientID: "client"}, nil)
	authURL, err := client.BuildAuthURL(context.Background(), "https://app.example/callback", "state", "verifier", "nonce")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Path != "/authorize" || query.Get("state") != "state" || query.Get("nonce") != "nonce" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		t.Fatalf("unexpected authorization URL: %s", authURL)
	}
}

func TestKySignOnWebhookSync(t *testing.T) {
	st, err := store.Open(context.Background(), testdb.Config(t))
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	hmacSecret := "webhook-secret-999"
	client := sso.NewKySignOnClient(config.SSOConfig{
		KySignOnHMACSecret: hmacSecret,
	}, st)

	payload := sso.KySignOnSyncPayload{
		Event:       "user.created",
		ID:          "ext-usr-456",
		Username:    "bob",
		Email:       "bob@busnes.app",
		DisplayName: "Bob Engineer",
		Role:        "user",
		Status:      "active",
		Timestamp:   time.Now().Unix(),
	}
	body, _ := json.Marshal(payload)
	sig := crypto.ComputeHMACSHA256(body, hmacSecret)

	// 1. Sync create user
	if err := client.HandleSyncWebhook(context.Background(), body, sig); err != nil {
		t.Fatalf("HandleSyncWebhook failed: %v", err)
	}

	created, err := st.Users().GetUserBySSO(context.Background(), "kysignon", "ext-usr-456")
	if err != nil {
		t.Fatalf("GetUserBySSO failed: %v", err)
	}
	if created.Username != "bob" || created.DisplayName != "Bob Engineer" {
		t.Errorf("unexpected created user: %+v", created)
	}

	// 2. Sync deactivation
	payload.Event = "user.deactivated"
	body, _ = json.Marshal(payload)
	sig = crypto.ComputeHMACSHA256(body, hmacSecret)

	if err := client.HandleSyncWebhook(context.Background(), body, sig); err != nil {
		t.Fatalf("HandleSyncWebhook deactivation failed: %v", err)
	}

	updated, _ := st.Users().GetUserBySSO(context.Background(), "kysignon", "ext-usr-456")
	if updated.Status != "inactive" {
		t.Errorf("expected inactive status, got %s", updated.Status)
	}
}

func TestSAMLServiceProvider(t *testing.T) {
	sp := sso.NewSAMLServiceProvider("https://app.busnes.app/saml/metadata", "https://app.busnes.app/saml/acs")

	metadata := sp.GenerateMetadata()
	if len(metadata) == 0 || !testing.Verbose() && len(metadata) < 50 {
		if len(metadata) == 0 {
			t.Errorf("expected non-empty metadata")
		}
	}

}

// A directory user whose username matches an existing account ignoring case is refused: no second
// account, and the existing one is not linked (Review Focus 4).
func TestKySignOnWebhookRefusesACaseVariantUsername(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()
	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_erin", Username: "erin", Role: "admin", Status: "active", SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
	client := sso.NewKySignOnClient(config.SSOConfig{KySignOnHMACSecret: "webhook-secret-999"}, st)
	body, _ := json.Marshal(sso.KySignOnSyncPayload{Event: "user.created", ID: "ext-erin", Username: "Erin", Email: "erin@busnes.app", Role: "user", Status: "active", Timestamp: time.Now().Unix()})
	err = client.HandleSyncWebhook(ctx, body, crypto.ComputeHMACSHA256(body, "webhook-secret-999"))
	if !errors.Is(err, sso.ErrUsernameTaken) || !strings.Contains(err.Error(), "Erin") {
		t.Fatalf("case variant: %v", err)
	}
	if _, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-erin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second account was created: %v", err)
	}
	if u, err := st.Users().GetUserByUsername(ctx, "ERIN"); err != nil || u.ID != "usr_erin" || u.SSOProvider != "local" {
		t.Fatalf("the local account changed: %+v %v", u, err)
	}
}

// A directory rename onto another account's username, ignoring case, is refused the same way,
// and the driver's text (PostgreSQL names the conflicting key) never reaches the caller.
func TestKySignOnWebhookRefusesARenameOntoATakenUsername(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()
	if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_erin", Username: "erin", Role: "admin", Status: "active", SSOProvider: "local"}); err != nil {
		t.Fatal(err)
	}
	client := sso.NewKySignOnClient(config.SSOConfig{KySignOnHMACSecret: "webhook-secret-999"}, st)
	send := func(event, username string) error {
		body, _ := json.Marshal(sso.KySignOnSyncPayload{Event: event, ID: "ext-frank", Username: username, Email: "frank@busnes.app", Role: "user", Status: "active", Timestamp: time.Now().Unix()})
		return client.HandleSyncWebhook(ctx, body, crypto.ComputeHMACSHA256(body, "webhook-secret-999"))
	}
	if err := send("user.created", "frank"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"erin", "Erin"} {
		err := send("user.updated", name)
		if !errors.Is(err, sso.ErrUsernameTaken) || err.Error() != sso.ErrUsernameTaken.Error()+": "+name {
			t.Fatalf("rename onto %q: %v", name, err)
		}
	}
	if u, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-frank"); err != nil || u.Username != "frank" {
		t.Fatalf("the directory account changed: %+v %v", u, err)
	}
}
