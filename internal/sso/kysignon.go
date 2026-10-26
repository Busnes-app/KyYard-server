package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// ErrUsernameTaken refuses a directory user whose username matches an existing account ignoring
// case: sign-in matches LOWER(username), and a directory's claim is never a link to that account.
var ErrUsernameTaken = errors.New("username is taken by another account")

// KySignOnClient manages interactions with the central KySignOn identity provider.
type KySignOnClient struct {
	config config.SSOConfig
	store  store.Store
	flow   *oauthFlow
}

func NewKySignOnClient(cfg config.SSOConfig, st store.Store) *KySignOnClient {
	return &KySignOnClient{
		config: cfg,
		store:  st,
		flow:   newOAuthFlow(cfg.KySignOnIssuer, cfg.KySignOnClientID, cfg.KySignOnSecret),
	}
}

// BuildAuthURL generates the authorization code URL with PKCE for KySignOn.
func (k *KySignOnClient) BuildAuthURL(ctx context.Context, redirectURI, state, verifier, nonce string) (string, error) {
	return k.flow.authCodeURL(ctx, redirectURI, state, verifier, nonce)
}

// ExchangeCode exchanges the authorization code and verifier for identity claims.
func (k *KySignOnClient) ExchangeCode(ctx context.Context, code, verifier, redirectURI, expectedNonce string) (*IdentityClaims, error) {
	claims, err := k.flow.exchange(ctx, code, verifier, redirectURI, expectedNonce)
	if err != nil {
		return nil, err
	}
	claims.Provider = "kysignon"
	return claims, nil
}

// refusal is a directory user refused for a taken username.
type refusal struct{ username string }

func (r refusal) Error() string { return ErrUsernameTaken.Error() + ": " + r.username }
func (r refusal) Unwrap() error { return ErrUsernameTaken }

// KySignOnSyncPayload defines the schema received during automatic directory replication webhooks.
type KySignOnSyncPayload struct {
	Event       string `json:"event"`    // "user.created", "user.updated", "user.deactivated", "user.deleted"
	EventID     string `json:"event_id"` // unique per event; an event applies once
	ID          string `json:"id"`       // the user's stable subject
	Username    string `json:"username"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	Timestamp   int64  `json:"timestamp"`
}

// HandleSyncWebhook processes inbound HMAC-SHA256 signed user updates from KySignOn server.
// Each signed event applies at most once and never after a newer event for the same user.
func (k *KySignOnClient) HandleSyncWebhook(ctx context.Context, body []byte, signature string) error {
	if len(k.config.KySignOnHMACSecret) < crypto.MinHMACKeyBytes {
		return fmt.Errorf("webhook HMAC secret must contain at least %d bytes", crypto.MinHMACKeyBytes)
	}

	if !crypto.VerifyHMACSHA256(body, k.config.KySignOnHMACSecret, signature) {
		return errors.New("invalid webhook signature")
	}

	var payload KySignOnSyncPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("invalid json payload: %w", err)
	}
	at := time.Unix(payload.Timestamp, 0)
	if payload.Timestamp == 0 || time.Since(at).Abs() > store.DirectoryEventWindow {
		return errors.New("webhook timestamp is missing or expired")
	}
	if payload.EventID == "" || len(payload.EventID) > 128 || payload.ID == "" || len(payload.ID) > 255 {
		return errors.New("webhook event_id or id is missing or too long")
	}
	ev := store.DirectoryEvent{Provider: "kysignon", ID: payload.EventID, Subject: payload.ID, At: at}
	err := k.store.Users().ApplyDirectoryEvent(ctx, ev, func(tx store.DirectoryTx) error { return apply(tx, payload) })
	// Audited once the event's transaction is over: a refused event is rolled back, so the
	// provider's retry is judged afresh.
	var r refusal
	if errors.As(err, &r) {
		if err := k.store.Audit().LogAudit(ctx, &store.AuditRecord{Action: "auth.sso.refused", Result: "denied", Resource: "kysignon", Details: "username=" + protocol.CleanText(r.username, 64)}); err != nil {
			log.Printf("[SSO] audit auth.sso.refused not recorded: %v", err)
		}
	}
	return err
}

func apply(tx store.DirectoryTx, payload KySignOnSyncPayload) error {
	switch payload.Event {
	case "user.created", "user.updated":
		existing, err := tx.GetUserBySSO("kysignon", payload.ID)
		if err == nil && existing.SSOProvider != "kysignon" {
			return refusal{existing.Username}
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}

		role := payload.Role
		if role == "" {
			role = "user"
		}
		status := payload.Status
		if status == "" {
			status = "active"
		}

		if existing != nil {
			privilegesChanged := existing.Role != role || existing.Status != status
			// An invalid name is a refused rename; the rest of the event still applies.
			if auth.ValidateUsername(payload.Username) == nil {
				existing.Username = payload.Username
			}
			existing.Email = payload.Email
			existing.DisplayName = payload.DisplayName
			existing.Role = role
			existing.Status = status
			if err := tx.UpdateUserIdentity(existing); errors.Is(err, store.ErrAlreadyExists) {
				return refusal{existing.Username}
			} else if err != nil {
				return err
			}
			if privilegesChanged {
				return tx.DeleteUserSessions(existing.ID)
			}
			return nil
		}

		username := LocalUsername("kysignon", payload.ID, payload.Username)
		if _, err := tx.GetUserByUsername(username); err == nil {
			return refusal{username}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		newUser := &store.User{
			ID:          fmt.Sprintf("usr_%s", crypto.RandomHex(12)),
			Username:    username,
			Email:       payload.Email,
			DisplayName: payload.DisplayName,
			Role:        role,
			Status:      status,
			SSOProvider: "kysignon",
			SSOSubject:  payload.ID,
		}
		err = tx.CreateUser(newUser)
		if errors.Is(err, store.ErrAlreadyExists) {
			return refusal{username}
		}
		return err

	case "user.deactivated":
		existing, err := tx.GetUserBySSO("kysignon", payload.ID)
		if err == nil && existing.SSOProvider != "kysignon" {
			return refusal{existing.Username}
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		existing.Status = "inactive"
		if err := tx.UpdateUserIdentity(existing); err != nil {
			return err
		}
		return tx.DeleteUserSessions(existing.ID)

	case "user.deleted":
		existing, err := tx.GetUserBySSO("kysignon", payload.ID)
		if err == nil && existing.SSOProvider != "kysignon" {
			return refusal{existing.Username}
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		return tx.DeleteUser(existing.ID)
	}

	return nil
}
