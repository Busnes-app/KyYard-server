package sso_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/sso"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
	"strings"
	"testing"
	"time"
)

func TestWebhookKeyFloorBeforeAccountMutation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, size := range []int{15, 16, 32} {
		secret := strings.Repeat("s", size)
		subject := fmt.Sprintf("key-floor-%d", size)
		body, err := json.Marshal(sso.KySignOnSyncPayload{EventID: subject, Event: "user.created", ID: subject, Username: subject, Role: "user", Status: "active", Timestamp: time.Now().Unix()})
		if err != nil {
			t.Fatal(err)
		}
		client := sso.NewKySignOnClient(config.SSOConfig{KySignOnHMACSecret: secret}, st)
		err = client.HandleSyncWebhook(ctx, body, crypto.ComputeHMACSHA256(body, secret))
		user, lookup := st.Users().GetUserBySSO(ctx, "kysignon", subject)
		if size < 16 {
			if err == nil || !strings.Contains(err.Error(), "at least 16") || !errors.Is(lookup, store.ErrNotFound) {
				t.Fatalf("short signed key mutated an account: user=%+v err=%v lookup=%v", user, err, lookup)
			}
		} else if err != nil || lookup != nil || user.Username != subject {
			t.Fatalf("strong signed key refused: user=%+v err=%v lookup=%v", user, err, lookup)
		}
	}
}
