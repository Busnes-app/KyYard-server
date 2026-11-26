package registry

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Busnes-app/ky-primitives/pqtls"
)

// Outbound registry metadata TLS offers X25519MLKEM768 first against a TLS 1.3
// registry peer, both normally and under GODEBUG=fips140=on.
func TestRegistryClientNegotiatesHybridKeyExchange(t *testing.T) {
	var (
		mu          sync.Mutex
		serverState tls.ConnectionState
		hasTLS      bool
	)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.TLS != nil {
			serverState = *r.TLS
			hasTLS = true
		}
		mu.Unlock()
		serve(w, r, singleType, singleBody)
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()

	if _, err := testClient(t, srv, Options{}).Resolve(context.Background(), ref(srv), testCred); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	mu.Lock()
	gotTLS, gotState := hasTLS, serverState
	mu.Unlock()
	if !gotTLS {
		t.Fatal("server request missing TLS connection state")
	}
	pqtls.RequireHybrid(t, gotState)
}

// External container registries and operator-managed proxies may not yet
// support ML-KEM-768, so the registry client retains classical TLS 1.3 fallback.
func TestRegistryClientAllowsClassicalFallback(t *testing.T) {
	var (
		mu          sync.Mutex
		serverState tls.ConnectionState
		hasTLS      bool
	)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.TLS != nil {
			serverState = *r.TLS
			hasTLS = true
		}
		mu.Unlock()
		serve(w, r, singleType, singleBody)
	}))
	srv.TLS = &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.CurveP256},
	}
	srv.StartTLS()
	defer srv.Close()

	if _, err := testClient(t, srv, Options{}).Resolve(context.Background(), ref(srv), testCred); err != nil {
		t.Fatalf("classical fallback Resolve: %v", err)
	}

	mu.Lock()
	gotTLS, gotState := hasTLS, serverState
	mu.Unlock()
	if !gotTLS || gotState.CurveID != tls.CurveP256 {
		t.Fatalf("hasTLS=%v curve=%v, want CurveP256", gotTLS, gotState.CurveID)
	}
}
