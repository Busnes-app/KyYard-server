package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Busness-app/ky_server_base/internal/config"
	"github.com/Busness-app/ky_server_base/internal/crypto"
	"github.com/Busness-app/ky_server_base/internal/store"
)

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

// KySignOnSyncPayload defines the schema received during automatic directory replication webhooks.
type KySignOnSyncPayload struct {
	Event       string `json:"event"` // "user.created", "user.updated", "user.deactivated", "user.deleted"
	ID          string `json:"id"`
	Username    string `json:"username"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	Timestamp   int64  `json:"timestamp"`
}

// HandleSyncWebhook processes inbound HMAC-SHA256 signed user updates from KySignOn server.
func (k *KySignOnClient) HandleSyncWebhook(ctx context.Context, body []byte, signature string) error {
	if k.config.KySignOnHMACSecret == "" {
		return errors.New("webhook HMAC secret is not configured")
	}

	if !crypto.VerifyHMACSHA256(body, k.config.KySignOnHMACSecret, signature) {
		return errors.New("invalid webhook signature")
	}

	var payload KySignOnSyncPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("invalid json payload: %w", err)
	}
	if payload.Timestamp == 0 || time.Since(time.Unix(payload.Timestamp, 0)).Abs() > 5*time.Minute {
		return errors.New("webhook timestamp is missing or expired")
	}

	switch payload.Event {
	case "user.created", "user.updated":
		existing, err := k.store.Users().GetUserBySSO(ctx, "kysignon", payload.ID)
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
			existing.Username = payload.Username
			existing.Email = payload.Email
			existing.DisplayName = payload.DisplayName
			existing.Role = role
			existing.Status = status
			if err := k.store.Users().UpdateUser(ctx, existing); err != nil {
				return err
			}
			if privilegesChanged {
				return k.store.Sessions().DeleteUserSessions(ctx, existing.ID)
			}
			return nil
		}

		newUser := &store.User{
			ID:          fmt.Sprintf("usr_%s", crypto.RandomHex(12)),
			Username:    payload.Username,
			Email:       payload.Email,
			DisplayName: payload.DisplayName,
			Role:        role,
			Status:      status,
			SSOProvider: "kysignon",
			SSOSubject:  payload.ID,
		}
		return k.store.Users().CreateUser(ctx, newUser)

	case "user.deactivated":
		existing, err := k.store.Users().GetUserBySSO(ctx, "kysignon", payload.ID)
		if err != nil {
			return nil // User might not exist locally
		}
		existing.Status = "inactive"
		if err := k.store.Users().UpdateUser(ctx, existing); err != nil {
			return err
		}
		return k.store.Sessions().DeleteUserSessions(ctx, existing.ID)

	case "user.deleted":
		existing, err := k.store.Users().GetUserBySSO(ctx, "kysignon", payload.ID)
		if err != nil {
			return nil
		}
		return k.store.Users().DeleteUser(ctx, existing.ID)
	}

	return nil
}
