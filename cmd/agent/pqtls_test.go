package main

import (
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The agent's enrollment and socket client offers the hybrid post-quantum key
// exchange first, so a Go TLS 1.3 edge negotiates X25519MLKEM768 with it, with
// and without fips140=on (CI runs both).
func TestAgentClientNegotiatesHybridKeyExchange(t *testing.T) {
	var server tls.CurveID
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server = r.TLS.CurveID
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := newHTTPClient(ca)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS == nil || resp.TLS.CurveID != tls.X25519MLKEM768 || server != tls.X25519MLKEM768 {
		t.Fatalf("key exchange client=%v server=%v, want X25519MLKEM768", resp.TLS.CurveID, server)
	}
}
