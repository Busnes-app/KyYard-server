package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/fips140"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentTLSChecksTrustedRootKeyFloor(t *testing.T) {
	for _, bits := range []int{1024, 2047, 2048} {
		t.Run(fmt.Sprint(bits), func(t *testing.T) {
			rootKey, err := rsa.GenerateKey(rand.Reader, bits)
			if err != nil {
				t.Fatal(err)
			}
			leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
			rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
			if err != nil {
				t.Fatal(err)
			}
			root, err = x509.ParseCertificate(rootDER)
			if err != nil {
				t.Fatal(err)
			}
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"pulse-fixture.invalid"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, rootKey)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusNoContent) }))
			peer.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: leafKey}}}
			peer.StartTLS()
			defer peer.Close()
			control, err := peer.Client().Get(peer.URL)
			if err != nil {
				t.Fatalf("strong-leaf control: %v", err)
			}
			control.Body.Close()
			caFile := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := newHTTPClient(caFile)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			before := calls.Load()
			req, err := http.NewRequest(http.MethodGet, peer.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer synthetic-fixture")
			response, err := client.Do(req)
			if response != nil {
				response.Body.Close()
			}
			refuse := bits < 2048 && fips140.Enabled()
			if (err != nil) != refuse {
				t.Fatalf("RSA%d configured=%v: %v", bits, fips140.Enabled(), err)
			}

			if refuse && calls.Load() != before {
				t.Fatal("HTTP request crossed weak-root TLS")
			}
		})
	}
}
