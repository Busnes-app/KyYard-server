package store_test

import (
	"context"
	"errors"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"testing"
	"time"
)

func TestSSOLinkKeepsLocalAccountAndRefusesStaleProof(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	u := &store.User{ID: "local", Username: "admin", PasswordHash: "old", Role: "admin", Status: "active", SSOProvider: "local"}
	if err := st.Users().CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	session := &store.Session{TokenHash: "session", UserID: u.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := st.Sessions().CreateSession(ctx, session, "old"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ session, hash string }{{"missing", "old"}, {"session", "stale"}} {
		if err := st.Users().LinkSSO(ctx, u.ID, tc.session, tc.hash, "idp_test", "subject", "127.0.0.1"); err == nil {
			t.Fatal("stale proof accepted")
		}
	}
	if err := st.Users().LinkSSO(ctx, u.ID, "session", "old", "idp_test", "subject", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	linked, err := st.Users().GetUserBySSO(ctx, "idp_test", "subject")
	if err != nil || linked.ID != u.ID || linked.SSOProvider != "local" || linked.PasswordHash != "old" || linked.Role != "admin" {
		t.Fatalf("account changed: %+v %v", linked, err)
	}
	if err := st.Users().CreateUser(ctx, &store.User{ID: "other", Username: "other", Role: "user", Status: "active", SSOProvider: "idp_test", SSOSubject: "subject"}); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate identity: %v", err)
	}
	if _, err := st.Users().GetUserByID(ctx, "other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("failed provision left an account", err)
	}
	if err := st.Users().LinkSSO(ctx, u.ID, "session", "old", "idp_test", "replacement", "127.0.0.1"); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatal("reassigned provider identity", err)
	}
	if err := st.Users().ResetAdminPassword(ctx, u.ID, "new"); err != nil {
		t.Fatal(err)
	}
	// The operator reset is account recovery: a linked identity must not sign in past it.
	if _, err := st.Users().GetUserBySSO(ctx, "idp_test", "subject"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("identity still linked after the operator reset", err)
	}
	if err := st.Users().LinkSSO(ctx, u.ID, "session", "old", "idp_other", "new", "127.0.0.1"); err == nil {
		t.Fatal("reset proof accepted")
	}
}

func TestSSOIdentityHasOneOwnerAcrossConcurrentLinks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	for _, id := range []string{"one", "two"} {
		if err := st.Users().CreateUser(ctx, &store.User{ID: id, Username: id, PasswordHash: "hash", Status: "active", Role: "user", SSOProvider: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := st.Sessions().CreateSession(ctx, &store.Session{TokenHash: id, UserID: id, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, 2)
	for _, id := range []string{"one", "two"} {
		go func(id string) { results <- st.Users().LinkSSO(ctx, id, id, "hash", "idp_test", "shared", "127.0.0.1") }(id)
	}
	successes := 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("%d links won", successes)
	}
}

// A provider sign-in that resolved the account before an unlink cannot get a session after it:
// the session write rechecks the link under the same user-row lock as UnlinkSSO.
func TestNoProviderSessionAfterUnlink(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	u := &store.User{ID: "local", Username: "carol", PasswordHash: "hash", Role: "user", Status: "active", SSOProvider: "local"}
	if err := st.Users().CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.Sessions().CreateSession(ctx, &store.Session{TokenHash: "local", UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().LinkSSO(ctx, u.ID, "local", "hash", "idp_test", "intruder", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	via := func(token string) error {
		return st.Sessions().CreateSession(ctx, &store.Session{TokenHash: token, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), SSOProvider: "idp_test", SSOSID: token}, "hash")
	}
	if err := via("before"); err != nil {
		t.Fatalf("a linked provider's session: %v", err)
	}
	if err := st.Users().UnlinkSSO(ctx, u.ID, "hash", "idp_test", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := via("after"); err == nil {
		t.Fatal("an unlinked provider issued a session")
	}
	// The account's own provider needs no link row: a provisioned account still signs in.
	p := &store.User{ID: "fed", Username: "dave", Role: "user", Status: "active", SSOProvider: "idp_test", SSOSubject: "dave"}
	if err := st.Users().CreateUser(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.Sessions().CreateSession(ctx, &store.Session{TokenHash: "fed", UserID: p.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), SSOProvider: "idp_test"}, ""); err != nil {
		t.Fatalf("a provisioned account's provider session: %v", err)
	}
}
