package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// An administrator who is demoted, disabled or removed in one organization loses the agent
// tokens, status feeds and unclaimed pairing codes they created there, in the same change,
// with one audit row each; another administrator's and another organization's survive.
func TestLosingOrganizationAdminRevokesWhatTheyCreated(t *testing.T) {
	for _, change := range []string{"demote", "disable", "remove", "delete account"} {
		t.Run(change, func(t *testing.T) {
			st, a := orgAdmin(t)
			ctx := context.Background()
			if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_b", Username: "bea", Role: "user", Status: "active", PasswordHash: "x"}); err != nil {
				t.Fatal(err)
			}
			if err := st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "org_a", UserID: "usr_b", Role: store.RoleOrganizationAdmin, Status: "active"}); err != nil {
				t.Fatal(err)
			}
			b := store.TenantAccess{ActorID: "usr_b", OrganizationID: "org_a"}
			kept := mustAgentToken(t, st, a, store.ScopeMCPRead)
			token := mustAgentToken(t, st, b, store.ScopeMCPOperate)
			feed, err := st.Tenancy().CreateStatusFeed(ctx, b, "pager")
			if err != nil {
				t.Fatal(err)
			}
			pairing := mustPairing(t, st, b)
			// bea also administers org_b, which this change does not touch.
			if err := st.Tenancy().CreateOrganizationWithAdmin(ctx, &store.Organization{ID: "org_b", Name: "B"}, "usr_b"); err != nil {
				t.Fatal(err)
			}
			other := mustAgentToken(t, st, store.TenantAccess{ActorID: "usr_b", OrganizationID: "org_b"}, store.ScopeMCPRead)

			if list, err := st.Tenancy().ListAgentTokens(ctx, a); err != nil || len(list) != 2 || list[0].CreatedByName != "bea" {
				t.Fatalf("the list names no creator: %+v %v", list, err)
			}
			switch change {
			case "demote":
				err = st.Tenancy().PutMembership(ctx, a, "usr_b", store.RoleOperator, "active")
			case "disable":
				err = st.Tenancy().PutMembership(ctx, a, "usr_b", store.RoleOrganizationAdmin, "disabled")
			case "remove":
				err = st.Tenancy().RemoveMembership(ctx, a, "usr_b")
			case "delete account":
				// org_b would lose its only administrator; give it another first.
				if err := st.Users().CreateUser(ctx, &store.User{ID: "usr_c", Username: "cal", Role: "user", Status: "active", PasswordHash: "x"}); err != nil {
					t.Fatal(err)
				}
				if err := st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "org_b", UserID: "usr_c", Role: store.RoleOrganizationAdmin, Status: "active"}); err != nil {
					t.Fatal(err)
				}
				err = st.Users().DeleteUser(ctx, "usr_b")
			}
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if _, err := st.Tenancy().LookupAgentToken(ctx, token.Token, now); !errors.Is(err, store.ErrForbidden) {
				t.Fatalf("the ex-administrator's agent token still works: %v", err)
			}
			if _, err := st.Tenancy().ReadStatusFeed(ctx, feed.Token, "10.0.0.9", nil); !errors.Is(err, store.ErrForbidden) {
				t.Fatalf("the ex-administrator's feed still reads: %v", err)
			}
			if _, err := st.Tenancy().ClaimServiceToken(ctx, pairing.Code, "pulse", "10.0.0.9"); err == nil {
				t.Fatal("the ex-administrator's pairing code still claims a token")
			}
			if _, err := st.Tenancy().LookupAgentToken(ctx, kept.Token, now); err != nil {
				t.Fatalf("another administrator's token was revoked: %v", err)
			}
			if _, err := st.Tenancy().LookupAgentToken(ctx, other.Token, now); change != "delete account" && err != nil {
				t.Fatalf("another organization's token was revoked: %v", err)
			}
			for action, id := range map[string]string{"agent_token.revoke": token.ID, store.AuditStatusFeedRevoke: feed.ID, "service_token.pairing_revoke": pairing.ID} {
				var rows []store.AuditRecord
				for _, r := range auditRows(t, st, action) {
					if r.OrganizationID == "org_a" {
						rows = append(rows, r)
					}
				}
				if len(rows) != 1 || rows[0].Resource != id {
					t.Fatalf("%s audit: %+v", action, rows)
				}
			}
		})
	}
}
