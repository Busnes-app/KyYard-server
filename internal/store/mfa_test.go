package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestUpdateMFAComparesAndAudits(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	u := &store.User{ID: "mfa", Username: "mfa", PasswordHash: "hash", Role: "user", Status: "active", SSOProvider: "local"}
	if err := st.Users().CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	off := store.MFAState{RecoveryCodesHash: "[]"}
	pending := store.MFAState{SecretEnc: "sealed", RecoveryCodesHash: "[]"}
	on := store.MFAState{SecretEnc: "sealed", Enabled: true, RecoveryCodesHash: `["a"]`}

	if err := st.Users().UpdateMFA(ctx, u.ID, "stale", off, pending, "auth.mfa.enrol_started", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("stale password hash accepted", err)
	}
	if err := st.Users().UpdateMFA(ctx, u.ID, "hash", off, pending, "auth.mfa.enrol_started", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	// A second writer holding the old state loses.
	if err := st.Users().UpdateMFA(ctx, u.ID, "hash", off, on, "auth.mfa.enabled", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("stale state accepted", err)
	}
	if err := st.Users().UpdateMFA(ctx, u.ID, "hash", pending, on, "auth.mfa.enabled", ""); err != nil {
		t.Fatal(err)
	}
	got, err := st.Users().GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TOTPEnabled || got.TOTPSecretEnc != "sealed" || got.RecoveryCodesHash != `["a"]` {
		t.Fatalf("state not written: %+v", got)
	}
	audits, _, err := st.Audit().ListAuditRecords(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range audits {
		seen[a.Action] = true
	}
	if !seen["auth.mfa.enrol_started"] || !seen["auth.mfa.enabled"] {
		t.Fatal("missing audit rows", seen)
	}
}

func TestUpdateMFARefusesFederatedAndFlaggedAccounts(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	for _, u := range []*store.User{
		{ID: "fed", Username: "fed", PasswordHash: "hash", Role: "user", Status: "active", SSOProvider: "oidc", SSOSubject: "sub"},
		{ID: "flag", Username: "flag", PasswordHash: "hash", Role: "user", Status: "active", SSOProvider: "local", MustChangePassword: true},
	} {
		if err := st.Users().CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		err := st.Users().UpdateMFA(ctx, u.ID, "hash", store.MFAState{RecoveryCodesHash: "[]"}, store.MFAState{SecretEnc: "x", RecoveryCodesHash: "[]"}, "auth.mfa.enrol_started", "")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatal(u.ID, err)
		}
	}
}

func TestChangePasswordRevokesAndRefusesFlagged(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	u := &store.User{ID: "self", Username: "self", PasswordHash: "old", Role: "user", Status: "active", SSOProvider: "local"}
	if err := st.Users().CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{TokenHash: "session", UserID: u.ID, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.Sessions().CreateSession(ctx, sess, "old"); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().ChangePassword(ctx, u.ID, "wrong", "new", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("wrong hash accepted", err)
	}
	if err := st.Users().ChangePassword(ctx, u.ID, "old", "new", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Sessions().GetSession(ctx, sess.TokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("session survived", err)
	}
	flagged := &store.User{ID: "flagged", Username: "flagged", PasswordHash: "old", Role: "user", Status: "active", SSOProvider: "local", MustChangePassword: true}
	if err := st.Users().CreateUser(ctx, flagged); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().ChangePassword(ctx, flagged.ID, "old", "new", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("flagged account used the self-service path", err)
	}
}
