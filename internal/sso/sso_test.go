package sso_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
		EventID:     "evt-1",
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
	payload.EventID = "evt-2"
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
	body, _ := json.Marshal(sso.KySignOnSyncPayload{EventID: crypto.RandomHex(8), Event: "user.created", ID: "ext-erin", Username: "Erin", Email: "erin@busnes.app", Role: "user", Status: "active", Timestamp: time.Now().Unix()})
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
	assertRefusals(t, st, "username=Erin")
}

// assertRefusals checks the auth.sso.refused rows carry exactly these details, in any order.
func assertRefusals(t *testing.T, st store.Store, details ...string) {
	t.Helper()
	records, _, err := st.Audit().ListAuditRecords(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range records {
		if r.Action != "auth.sso.refused" {
			continue
		}
		if r.Result != "denied" || r.Resource != "kysignon" || r.UserID != "" {
			t.Fatalf("refusal row %+v", r)
		}
		got = append(got, r.Details)
	}
	slices.Sort(got)
	slices.Sort(details)
	if !slices.Equal(got, details) {
		t.Fatalf("refusal details %q, want %q", got, details)
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
		body, _ := json.Marshal(sso.KySignOnSyncPayload{EventID: crypto.RandomHex(8), Event: event, ID: "ext-frank", Username: username, Email: "frank@busnes.app", Role: "user", Status: "active", Timestamp: time.Now().Unix()})
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
	assertRefusals(t, st, "username=erin", "username=Erin")
}

func TestDirectoryCannotManageAnExplicitlyLinkedLocalAccount(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The audited name is cleaned of control characters and capped at 64 bytes.
	user := &store.User{ID: "local-admin", Username: "\x07" + strings.Repeat("x", 70), PasswordHash: "hash", Role: "admin", Status: "active", SSOProvider: "local"}
	if err := st.Users().CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	session := &store.Session{TokenHash: "session", UserID: user.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := st.Sessions().CreateSession(ctx, session, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().LinkSSO(ctx, user.ID, "session", "hash", "kysignon", "external-id", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	client := sso.NewKySignOnClient(config.SSOConfig{KySignOnHMACSecret: "secret"}, st)
	for _, event := range []string{"user.created", "user.updated", "user.deactivated", "user.deleted"} {
		body, _ := json.Marshal(sso.KySignOnSyncPayload{EventID: crypto.RandomHex(8), Event: event, ID: "external-id", Username: "external-admin", Role: "user", Status: "inactive", Timestamp: time.Now().Unix()})
		if err := client.HandleSyncWebhook(ctx, body, crypto.ComputeHMACSHA256(body, "secret")); !errors.Is(err, sso.ErrUsernameTaken) {
			t.Fatalf("%s did not refuse: %v", event, err)
		}
		after, err := st.Users().GetUserByID(ctx, user.ID)
		if err != nil || after.Status != "active" || after.Role != "admin" || after.Username != user.Username {
			t.Fatalf("%s changed local account", event)
		}
	}
	capped := "username=" + strings.Repeat("x", 64)
	assertRefusals(t, st, capped, capped, capped, capped)
}

func signedEvent(t *testing.T, p sso.KySignOnSyncPayload) ([]byte, string) {
	t.Helper()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return body, crypto.ComputeHMACSHA256(body, "webhook-secret-999")
}

func TestKySignOnWebhookAppliesEachEventOnceAndInOrder(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.SSOConfig{KySignOnHMACSecret: "webhook-secret-999"}
	now := time.Now().Unix()
	grant := sso.KySignOnSyncPayload{Event: "user.created", EventID: "evt-grant", ID: "ext-ann", Username: "ann", Role: "admin", Status: "active", Timestamp: now - 60}
	body, sig := signedEvent(t, grant)
	if err := sso.NewKySignOnClient(cfg, st).HandleSyncWebhook(ctx, body, sig); err != nil {
		t.Fatal(err)
	}
	revoke := grant
	revoke.Event, revoke.EventID, revoke.Role, revoke.Timestamp = "user.updated", "evt-revoke", "user", now
	rb, rs := signedEvent(t, revoke)
	if err := sso.NewKySignOnClient(cfg, st).HandleSyncWebhook(ctx, rb, rs); err != nil {
		t.Fatal(err)
	}
	// A captured grant replayed, even by a fresh client after a restart, and an older event
	// delivered late, are both refused: the account stays a plain user.
	if err := sso.NewKySignOnClient(cfg, st).HandleSyncWebhook(ctx, body, sig); !errors.Is(err, store.ErrStaleEvent) {
		t.Fatalf("replay: %v", err)
	}
	late := grant
	late.Event, late.EventID, late.Timestamp = "user.updated", "evt-late", now-30
	lb, ls := signedEvent(t, late)
	if err := sso.NewKySignOnClient(cfg, st).HandleSyncWebhook(ctx, lb, ls); !errors.Is(err, store.ErrStaleEvent) {
		t.Fatalf("out of order: %v", err)
	}
	if u, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-ann"); err != nil || u.Role != "user" {
		t.Fatalf("an old grant was re-applied: %+v %v", u, err)
	}
	unnamed := revoke
	unnamed.EventID = ""
	ub, us := signedEvent(t, unnamed)
	if err := sso.NewKySignOnClient(cfg, st).HandleSyncWebhook(ctx, ub, us); err == nil {
		t.Fatal("an event without an ID was applied")
	}
}

// A fired sole administrator must lose access: the identity provider's deactivation and
// deletion always apply, and each organization left without an administrator gets an audit row.
func TestKySignOnWebhookRemovesAnOrganizationsLastAdministratorAndAudits(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	client := sso.NewKySignOnClient(config.SSOConfig{KySignOnHMACSecret: "webhook-secret-999"}, st)
	now := time.Now().Unix()
	ts := st.Tenancy()
	lost := func() []string {
		records, _, err := st.Audit().ListAuditRecords(ctx, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var orgs []string
		for _, r := range records {
			if r.Action == "organization.last_admin_lost" {
				orgs = append(orgs, r.OrganizationID+"/"+r.Resource)
			}
		}
		return orgs
	}
	for i, event := range []string{"user.deactivated", "user.deleted"} {
		subject := "ext-sole-" + event
		body, sig := signedEvent(t, sso.KySignOnSyncPayload{Event: "user.created", EventID: "create-" + event, ID: subject, Username: fmt.Sprintf("sole%d", i), Status: "active", Timestamp: now})
		if err := client.HandleSyncWebhook(ctx, body, sig); err != nil {
			t.Fatal(err)
		}
		sole, err := st.Users().GetUserBySSO(ctx, "kysignon", subject)
		if err != nil {
			t.Fatal(err)
		}
		org := fmt.Sprintf("org-%d", i)
		if err := ts.CreateOrganization(ctx, &store.Organization{ID: org, Name: org}); err != nil {
			t.Fatal(err)
		}
		if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: org, UserID: sole.ID, Role: store.RoleOrganizationAdmin, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		body, sig = signedEvent(t, sso.KySignOnSyncPayload{Event: event, EventID: event, ID: subject, Timestamp: now + 1})
		if err := client.HandleSyncWebhook(ctx, body, sig); err != nil {
			t.Fatalf("%s refused: %v", event, err)
		}
		u, err := st.Users().GetUserByID(ctx, sole.ID)
		if event == "user.deactivated" && (err != nil || u.Status != "inactive") {
			t.Fatalf("not deactivated: %+v %v", u, err)
		}
		if event == "user.deleted" && !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("not deleted: %v", err)
		}
		if got := lost(); len(got) != i+1 || !slices.Contains(got, org+"/"+sole.ID) {
			t.Fatalf("%s: admin loss audit %v", event, got)
		}
	}
}

func TestKySignOnWebhookNeverUsesAnInvalidUsername(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	client := sso.NewKySignOnClient(config.SSOConfig{KySignOnHMACSecret: "webhook-secret-999"}, st)
	bad := "ad min‮" + strings.Repeat("x", 70)
	now := time.Now().Unix()
	body, sig := signedEvent(t, sso.KySignOnSyncPayload{Event: "user.created", EventID: "evt-1", ID: "ext-odd", Username: bad, Status: "active", Timestamp: now})
	if err := client.HandleSyncWebhook(ctx, body, sig); err != nil {
		t.Fatal(err)
	}
	u, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-odd")
	if err != nil || u.Username != sso.LocalUsername("kysignon", "ext-odd", bad) || !strings.HasPrefix(u.Username, "sso_") || len(u.Username) > 64 {
		t.Fatalf("created %+v %v", u, err)
	}
	// A rename to an invalid name is refused; the rest of the event applies.
	body, sig = signedEvent(t, sso.KySignOnSyncPayload{Event: "user.updated", EventID: "evt-2", ID: "ext-odd", Username: "../root", Email: "odd@busnes.app", Status: "active", Timestamp: now + 1})
	if err := client.HandleSyncWebhook(ctx, body, sig); err != nil {
		t.Fatal(err)
	}
	after, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-odd")
	if err != nil || after.Username != u.Username || after.Email != "odd@busnes.app" {
		t.Fatalf("updated %+v %v", after, err)
	}
	if sso.LocalUsername("p", "s", "valid.name") != "valid.name" {
		t.Fatal("a valid name was replaced")
	}
}

// cancelAfterClaim cancels processing the moment a directory event is claimed, as a dropped
// webhook connection does.
type cancelAfterClaim struct {
	store.Store
	cancel context.CancelFunc
}

func (s cancelAfterClaim) Users() store.UserStore { return cancelUsers{s.Store.Users(), s.cancel} }

type cancelUsers struct {
	store.UserStore
	cancel context.CancelFunc
}

func (u cancelUsers) ApplyDirectoryEvent(ctx context.Context, ev store.DirectoryEvent, apply func(store.DirectoryTx) error) error {
	return u.UserStore.ApplyDirectoryEvent(ctx, ev, func(tx store.DirectoryTx) error {
		u.cancel()
		return apply(tx)
	})
}

// An interrupted deactivation leaves nothing behind, so the provider's retry still removes access.
func TestKySignOnWebhookRetryAppliesAnInterruptedDeactivation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.SSOConfig{KySignOnHMACSecret: "webhook-secret-999"}
	now := time.Now().Unix()
	body, sig := signedEvent(t, sso.KySignOnSyncPayload{Event: "user.created", EventID: "evt-create", ID: "ext-bob", Username: "bob", Status: "active", Timestamp: now - 1})
	if err := sso.NewKySignOnClient(cfg, st).HandleSyncWebhook(ctx, body, sig); err != nil {
		t.Fatal(err)
	}
	bob, err := st.Users().GetUserBySSO(ctx, "kysignon", "ext-bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Sessions().CreateSession(ctx, &store.Session{TokenHash: "bob-session", UserID: bob.ID, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, bob.PasswordHash); err != nil {
		t.Fatal(err)
	}

	body, sig = signedEvent(t, sso.KySignOnSyncPayload{Event: "user.deactivated", EventID: "evt-fire", ID: "ext-bob", Timestamp: now})
	cut, cancel := context.WithCancel(ctx)
	interrupted := sso.NewKySignOnClient(cfg, cancelAfterClaim{st, cancel}).HandleSyncWebhook(cut, body, sig)
	if err := sso.NewKySignOnClient(cfg, st).HandleSyncWebhook(ctx, body, sig); err != nil {
		t.Fatalf("retry refused: %v", err)
	}
	if u, err := st.Users().GetUserByID(ctx, bob.ID); err != nil || u.Status != "inactive" {
		t.Fatalf("still active after retry: %+v %v", u, err)
	}
	if _, err := st.Sessions().GetSession(ctx, "bob-session"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session survived: %v", err)
	}
	if interrupted == nil {
		t.Fatal("an interrupted event reported success")
	}
}
