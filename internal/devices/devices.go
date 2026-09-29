package devices

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// ErrPairingNotFound covers unknown, expired and consumed pairings alike, so a verifier
// learns nothing about which secrets were ever issued.
var ErrPairingNotFound = errors.New("pairing not found or expired")

type PairingService struct {
	store   store.Store
	appName string
	appURL  string
}

func NewPairingService(st store.Store, appName, appURL string) *PairingService {
	return &PairingService{
		store:   st,
		appName: appName,
		appURL:  appURL,
	}
}

type InitPairingResult struct {
	Secret    string `json:"secret"`
	ExpiresAt int64  `json:"expires_at"`
	QRPayload string `json:"qr_payload"`
}

// InitPairing creates a 90-second pairing carried by a QR code. Only the 24-byte secret
// redeems it: a short typed code could be guessed within the window.
func (s *PairingService) InitPairing(ctx context.Context, userID string) (*InitPairingResult, error) {
	secret := crypto.RandomHex(24)
	now := time.Now().UTC()
	expiresAt := now.Add(90 * time.Second)

	pairing := &store.DevicePairing{
		Secret:    secret,
		UserID:    userID,
		Status:    "pending",
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}

	if err := s.store.Devices().CreatePairing(ctx, pairing); err != nil {
		return nil, err
	}

	qrData := map[string]any{
		"action":   "ky_pair",
		"app_name": s.appName,
		"app_url":  s.appURL,
		"secret":   secret,
		"expires":  expiresAt.Unix(),
	}
	qrBytes, _ := json.Marshal(qrData)

	return &InitPairingResult{
		Secret:    secret,
		ExpiresAt: expiresAt.Unix(),
		QRPayload: string(qrBytes),
	}, nil
}

// VerifyPairing redeems the QR secret from a client device.
func (s *PairingService) VerifyPairing(ctx context.Context, secret, deviceName, platform, pushToken string) (*store.DevicePairing, *store.User, error) {
	pairing, err := s.store.Devices().GetPairingBySecret(ctx, secret)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrPairingExpired) {
			return nil, nil, ErrPairingNotFound
		}
		return nil, nil, err
	}
	if time.Now().UTC().After(pairing.ExpiresAt) || pairing.Status != "pending" {
		return nil, nil, ErrPairingNotFound
	}
	// Carry the pre-consumption credential snapshot through session issuance.
	user, err := s.store.Users().GetUserByID(ctx, pairing.UserID)
	if err != nil || user.Status != "active" || user.MustChangePassword {
		return nil, nil, ErrPairingNotFound
	}
	if err := s.store.Devices().ConsumePairing(ctx, pairing.Secret, deviceName, platform, pushToken); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, ErrPairingNotFound
		}
		return nil, nil, err
	}

	pairing.DeviceName = deviceName
	pairing.Platform = platform
	pairing.PushToken = pushToken
	pairing.Status = "consumed"

	return pairing, user, nil
}

// PollPairingStatus checks if a pending pairing session has been approved by the device.
func (s *PairingService) PollPairingStatus(ctx context.Context, secret string) (*store.DevicePairing, error) {
	pairing, err := s.store.Devices().GetPairingBySecret(ctx, secret)
	if err != nil {
		return nil, err
	}
	return pairing, nil
}
