package sso

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejson "github.com/go-jose/go-jose/v4/json"
)

// This client belongs only to the OIDC verifier's JWKS fetches.
//
// Back-channel logout is anonymous, so a refetch within jwksRefresh of the last
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
		// so the floor checks the exact key material the signature verifier uses.
		var key struct {
			Type string `json:"kty"`
			Alg  string `json:"alg"`
			Use  string `json:"use"`
			N    string `json:"n"`
			E    string `json:"e"`
			Pub  string `json:"pub"`
		}
		if err := josejson.Unmarshal(rawKey, &key); err != nil {
			return nil, errors.New("invalid OIDC JWK")
		}
		switch key.Type {
		case "RSA":
			if key.Pub != "" || strings.HasPrefix(key.Alg, "ML-DSA-") {
				return nil, errors.New("invalid OIDC RSA JWK")
			}
			n, err := base64.RawURLEncoding.DecodeString(key.N)
			if err != nil || new(big.Int).SetBytes(n).BitLen() < 2048 {
				return nil, errors.New("OIDC JWKS RSA keys must be at least 2048 bits")
			}
		case "AKP":
			if key.N != "" || key.E != "" {
				return nil, errors.New("OIDC JWKS AKP key must not contain RSA parameters")
			}
			if key.Alg != "ML-DSA-65" {
				return nil, errors.New("OIDC JWKS AKP keys must use ML-DSA-65")
			}
			if key.Use != "" && key.Use != "sig" {
				return nil, errors.New("OIDC JWKS AKP keys must have use sig")
			}
			pub, err := base64.RawURLEncoding.DecodeString(key.Pub)
			if err != nil {
				return nil, errors.New("invalid OIDC AKP public key encoding")
			}
			if _, err := mldsa.NewPublicKey(mldsa.MLDSA65(), pub); err != nil {
				return nil, errors.New("invalid OIDC ML-DSA-65 public key")
			}
		}
	}
	return raw, nil
}

// remoteJWKSKeySet implements oidc.KeySet on top of strongJWKSTransport while
// retaining RFC 9964 ML-DSA-65 AKP keys that coreos/go-oidc v3.21.0 skips.
type remoteJWKSKeySet struct {
	jwksURL string
	client  *http.Client
	algs    []jose.SignatureAlgorithm
	ttl     time.Duration

	mu       sync.RWMutex
	keys     []jose.JSONWebKey
	expiry   time.Time
	inflight *jwksRefreshCall
}

type jwksRefreshCall struct {
	done chan struct{}
	keys []jose.JSONWebKey
	err  error
}

const defaultJWKSCacheTTL = 5 * time.Minute

func newRemoteJWKSKeySet(jwksURL string, client *http.Client, supportedAlgs []string) *remoteJWKSKeySet {
	algs := make([]jose.SignatureAlgorithm, 0, len(supportedAlgs))
	for _, a := range supportedAlgs {
		algs = append(algs, jose.SignatureAlgorithm(a))
	}
	return &remoteJWKSKeySet{
		jwksURL: jwksURL,
		client:  client,
		algs:    algs,
		ttl:     defaultJWKSCacheTTL,
	}
}

func (r *remoteJWKSKeySet) cachedKeys() []jose.JSONWebKey {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.keys
}

func (r *remoteJWKSKeySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	jws, err := jose.ParseSigned(jwt, r.algs)
	if err != nil {
		return nil, fmt.Errorf("malformed jwt: %w", err)
	}
	if len(jws.Signatures) == 0 {
		return nil, errors.New("jwt has no signatures")
	}
	sig := jws.Signatures[0]
	kid := sig.Header.KeyID
	alg := sig.Header.Algorithm

	keys, err := r.getKeys(ctx, false)
	if err != nil {
		// If the cache expired during a temporary provider outage, fall back to
		// verifying against the last accepted keys before failing sign-in.
		if cached := r.cachedKeys(); len(cached) > 0 {
			if payload, ok := verifyWithKeys(jws, cached, kid, alg); ok {
				return payload, nil
			}
		}
		return nil, err
	}
	if payload, ok := verifyWithKeys(jws, keys, kid, alg); ok {
		return payload, nil
	}

	keys, err = r.getKeys(ctx, true)
	if err != nil {
		return nil, err
	}
	if payload, ok := verifyWithKeys(jws, keys, kid, alg); ok {
		return payload, nil
	}
	return nil, errors.New("failed to verify id token signature")
}

func verifyWithKeys(jws *jose.JSONWebSignature, keys []jose.JSONWebKey, kid, alg string) ([]byte, bool) {
	for _, key := range keys {
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if kid != "" && key.KeyID != "" && key.KeyID != kid {
			continue
		}
		if key.Algorithm != "" && alg != "" && key.Algorithm != alg {
			continue
		}
		if payload, err := jws.Verify(&key); err == nil {
			return payload, true
		}
	}
	return nil, false
}

func (r *remoteJWKSKeySet) getKeys(ctx context.Context, forceRefresh bool) ([]jose.JSONWebKey, error) {
	now := time.Now()
	if !forceRefresh {
		r.mu.RLock()
		if len(r.keys) > 0 && now.Before(r.expiry) {
			keys := r.keys
			r.mu.RUnlock()
			return keys, nil
		}
		r.mu.RUnlock()
	}

	r.mu.Lock()
	if !forceRefresh && len(r.keys) > 0 && time.Now().Before(r.expiry) {
		keys := r.keys
		r.mu.Unlock()
		return keys, nil
	}
	if call := r.inflight; call != nil {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.keys, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &jwksRefreshCall{done: make(chan struct{})}
	r.inflight = call
	r.mu.Unlock()

	timeout := r.client.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	go func() {
		defer cancel()
		keys, err := r.fetchKeys(fetchCtx)
		r.mu.Lock()
		if err == nil {
			r.keys = keys
			r.expiry = time.Now().Add(r.ttl)
		}
		call.keys = keys
		call.err = err
		r.inflight = nil
		close(call.done)
		r.mu.Unlock()
	}()

	select {
	case <-call.done:
		return call.keys, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *remoteJWKSKeySet) fetchKeys(ctx context.Context) ([]jose.JSONWebKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC JWKS fetch failed: HTTP %d", resp.StatusCode)
	}
	const maxJWKSBytes = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxJWKSBytes {
		return nil, errors.New("OIDC JWKS exceeds 1 MiB")
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("invalid OIDC JWKS: %w", err)
	}
	return set.Keys, nil
}
