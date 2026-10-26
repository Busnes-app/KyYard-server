package registry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/fips140"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryTLSChecksTrustedRootKeyFloor(t *testing.T) {
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
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"pulse-fixture.invalid", "*.example.com"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, rootKey)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); serve(w, r, singleType, singleBody) }))
			peer.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: leafKey}}}
			peer.StartTLS()
			defer peer.Close()
			control, err := peer.Client().Get(peer.URL)
			if err != nil {
				t.Fatalf("strong-leaf control: %v", err)
			}
			control.Body.Close()
			pool := x509.NewCertPool()
			pool.AddCert(root)
			client := testClient(t, peer, Options{})
			client.opts.RootCAs = pool
			before := calls.Load()
			_, err = client.Resolve(context.Background(), ref(peer), testCred)
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
