package store_test

import (
	"context"
	"errors"
	"github.com/Busness-app/kyyard-server/internal/store"
	"sync"
	"testing"
	"time"
)

func TestPasswordReplacementIsAtomicAndSingleUse(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	u := &store.User{ID: "reset", Username: "reset", PasswordHash: "old", Role: "admin", Status: "active", SSOProvider: "local", MustChangePassword: true}
	if err := st.Users().CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{TokenHash: "session", UserID: u.ID, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.Sessions().CreateSession(ctx, sess, "old"); err != nil {
		t.Fatal(err)
	}
	challenge := &store.MFAChallenge{TokenHash: "challenge", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.Sessions().CreateMFAChallenge(ctx, challenge, "old"); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, hash := range []string{"new-one", "new-two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- st.Users().CompletePasswordChange(ctx, u.ID, "old", hash, "127.0.0.1")
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("%d successful changes", success)
	}
	if _, err := st.Sessions().GetSession(ctx, sess.TokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("session survived", err)
	}
	if _, _, err := st.Sessions().ConsumeMFAChallenge(ctx, challenge.TokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("challenge survived", err)
	}
	if err := st.Sessions().CreateSession(ctx, sess, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("stale session creation", err)
	}
	if err := st.Sessions().CreateMFAChallenge(ctx, challenge, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("stale MFA creation", err)
	}
	audits, n, err := st.Audit().ListAuditRecords(ctx, 0, 10)
	if err != nil || n != 1 || audits[0].Action != "auth.password_changed" {
		t.Fatal("audit", n, err)
	}
}
