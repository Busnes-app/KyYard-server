package sso

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	josejson "github.com/go-jose/go-jose/v4/json"
)

// This client belongs only to the upstream OIDC verifier's JWKS fetches. Keep
// discovery, caching, rotation, signatures and claims in coreos/go-oidc.
//
// go-oidc refetches the JWKS for every token whose signature does not verify, and the
// back-channel logout route is anonymous, so a refetch within jwksRefresh of the last
// one is answered from the last accepted set instead of the network.
type strongJWKSTransport struct {
	base    http.RoundTripper
	mu      sync.Mutex
	fetched time.Time
	last    []byte
}

const jwksRefresh = time.Minute

func (t *strongJWKSTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.fetched) >= jwksRefresh {
		t.fetched = time.Now()
		raw, err := t.fetch(req)
		if err != nil {
			return nil, err
		}
		t.last = raw
	}
	if t.last == nil {
		return nil, errors.New("OIDC JWKS refresh throttled")
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(t.last)), Request: req}, nil
}

func (t *strongJWKSTransport) fetch(req *http.Request) ([]byte, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, errors.New("OIDC JWKS redirects are refused")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC JWKS fetch: %s", resp.Status)
	}
	const maxJWKSBytes = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxJWKSBytes {
		return nil, errors.New("OIDC JWKS exceeds 1 MiB")
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, errors.New("invalid OIDC JWKS")
	}
	for _, rawKey := range set.Keys {
		// Match go-oidc's envelope and go-jose's case-sensitive JWK members
		// so the floor checks the modulus the signature verifier uses.
		var key struct {
			Type string `json:"kty"`
			N    string `json:"n"`
		}
		if err := josejson.Unmarshal(rawKey, &key); err != nil {
			return nil, errors.New("invalid OIDC JWK")
		}
		if key.Type != "RSA" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(key.N)
		if err != nil || new(big.Int).SetBytes(n).BitLen() < 2048 {
			return nil, errors.New("OIDC JWKS RSA keys must be at least 2048 bits")
		}
	}
	return raw, nil
}
