package main

import (
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Busnes-app/ky-primitives/pqtls"
)

func writeTempCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return ca
}

// The agent's enrollment and socket client offers the hybrid post-quantum key
// exchange first, so a Go TLS 1.3 edge negotiates X25519MLKEM768 with it, with
// and without fips140=on (CI runs both).
func TestAgentClientNegotiatesHybridKeyExchange(t *testing.T) {
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
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()

	client, err := newHTTPClient(writeTempCA(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.TLS == nil {
		t.Fatal("client response missing TLS connection state")
	}
	pqtls.RequireHybrid(t, *resp.TLS)

	mu.Lock()
	gotTLS, gotState := hasTLS, serverState
	mu.Unlock()
	if !gotTLS {
		t.Fatal("server request missing TLS connection state")
	}
	pqtls.RequireHybrid(t, gotState)
}

// Operator-managed HTTPS edges may not yet support ML-KEM-768, so the agent
// client keeps Go's classical TLS 1.3 fallback instead of pinning HybridOnly.
func TestAgentClientAllowsClassicalFallback(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.CurveP256},
	}
	srv.StartTLS()
	defer srv.Close()

	client, err := newHTTPClient(writeTempCA(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("classical fallback failed: %v", err)
	}
	resp.Body.Close()

	if resp.TLS == nil || resp.TLS.CurveID != tls.CurveP256 {
		t.Fatalf("curve=%v, want CurveP256", resp.TLS)
	}
}
