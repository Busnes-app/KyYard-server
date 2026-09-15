package devices

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Busness-app/ky_server_base/internal/crypto"
	"github.com/Busness-app/ky_server_base/internal/store"
)

var (
	ErrPairingNotFound = errors.New("pairing session not found")
	ErrPairingExpired  = errors.New("pairing code expired")
)

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
	Code      string `json:"code"`
	Secret    string `json:"secret"`
	ExpiresAt int64  `json:"expires_at"`
	QRPayload string `json:"qr_payload"`
}

// InitPairing generates a 90-second ephemeral 6-digit PIN and QR payload for pairing mobile/PWA wrappers.
func (s *PairingService) InitPairing(ctx context.Context, userID string) (*InitPairingResult, error) {
	// Generate random 6-digit code
	nBig, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return nil, err
	}
	code := fmt.Sprintf("%06d", nBig.Int64()+100000)

	secret := crypto.RandomHex(24)
	now := time.Now().UTC()
	expiresAt := now.Add(90 * time.Second)

	pairing := &store.DevicePairing{
		Code:      code,
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
		"code":     code,
		"secret":   secret,
		"expires":  expiresAt.Unix(),
	}
	qrBytes, _ := json.Marshal(qrData)

	return &InitPairingResult{
		Code:      code,
		Secret:    secret,
		ExpiresAt: expiresAt.Unix(),
		QRPayload: string(qrBytes),
	}, nil
}

// VerifyPairing processes code submission from a client device (e.g. mobile app scanning QR or entering PIN).
func (s *PairingService) VerifyPairing(ctx context.Context, codeOrSecret, deviceName, platform, pushToken string) (*store.DevicePairing, error) {
	var pairing *store.DevicePairing
	var err error

	if len(codeOrSecret) == 6 {
		pairing, err = s.store.Devices().GetPairingByCode(ctx, codeOrSecret)
	} else {
		pairing, err = s.store.Devices().GetPairingBySecret(ctx, codeOrSecret)
	}

	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrPairingNotFound
		}
		if errors.Is(err, store.ErrPairingExpired) {
			return nil, ErrPairingExpired
		}
		return nil, err
	}

	if time.Now().UTC().After(pairing.ExpiresAt) {
		return nil, ErrPairingExpired
	}

	if pairing.Status != "pending" {
		return nil, ErrPairingNotFound
	}
	if err := s.store.Devices().ConsumePairing(ctx, pairing.Secret, deviceName, platform, pushToken); err != nil {
		return nil, err
	}

	pairing.DeviceName = deviceName
	pairing.Platform = platform
	pairing.PushToken = pushToken
	pairing.Status = "consumed"

	return pairing, nil
}

// PollPairingStatus checks if a pending pairing session has been approved by the device.
func (s *PairingService) PollPairingStatus(ctx context.Context, secret string) (*store.DevicePairing, error) {
	pairing, err := s.store.Devices().GetPairingBySecret(ctx, secret)
	if err != nil {
		return nil, err
	}
	return pairing, nil
}
