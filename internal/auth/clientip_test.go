package auth_test

import (
	"net/http/httptest"
	"testing"

	"github.com/Busness-app/ky_server_base/internal/auth"
	"github.com/Busness-app/ky_server_base/internal/config"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		trusted string
		peer    string
		xff     string
		xffAdd  []string
		realIP  string
		want    string
	}{
		{
			name: "no trusted proxy ignores a spoofed header",
			peer: "203.0.113.5:41000", xff: "1.2.3.4", realIP: "5.6.7.8",
			want: "203.0.113.5",
		},
		{
			name:    "trusted peer takes the rightmost untrusted entry",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000",
			xff:  "198.51.100.9, 192.0.2.8",
			want: "198.51.100.9",
		},
		{
			name:    "an untrusted hop to the right wins over the client's own claim",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000",
			xff:  "10.9.9.9, 198.51.100.9, 192.0.2.8",
			want: "198.51.100.9",
		},
		{
			name:    "a chain of nothing but trusted proxies falls back to the peer",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000", xff: "192.0.2.8, 192.0.2.9",
			want: "192.0.2.7",
		},
		{
			name:    "garbage in the chain falls back to the peer",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000", xff: "not-an-ip",
			want: "192.0.2.7",
		},
		{
			name:    "an absent header falls back to the peer",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000",
			want: "192.0.2.7",
		},
		{
			name: "an IPv6 peer keeps its address and loses its port",
			peer: "[2001:db8::1]:41000", xff: "1.2.3.4",
			want: "2001:db8::1",
		},
		{
			name:    "a trusted IPv6 proxy forwards its IPv6 client",
			trusted: "2001:db8::/64", peer: "[2001:db8::1]:41000", xff: "2001:db8:1::5, 2001:db8::2",
			want: "2001:db8:1::5",
		},
		{
			name: "an IPv4-mapped peer keys the same bucket as the plain IPv4 one",
			peer: "[::ffff:203.0.113.5]:41000",
			want: "203.0.113.5",
		},
		{
			// HAProxy's "option forwardfor" emits its own header line rather than appending to
			// the caller's, so the real chain is the last line, not the first.
			name:    "a second X-Forwarded-For line beats an attacker's first one",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000",
			xffAdd: []string{"9.9.9.9", "198.51.100.9"},
			want:   "198.51.100.9",
		},
		{
			name:    "an IPv4-mapped CIDR entry matches an unmapped IPv4 proxy",
			trusted: "::ffff:192.0.2.0/120", peer: "192.0.2.7:41000", xff: "198.51.100.9",
			want: "198.51.100.9",
		},
		{
			name:    "a bare IP entry in the allowlist matches only itself",
			trusted: "192.0.2.7", peer: "192.0.2.8:41000", xff: "198.51.100.9",
			want: "192.0.2.8",
		},
		{
			// Azure App Service / Application Gateway append "ip:port", not a bare IP.
			name:    "an IPv4 entry with a port is still read as a client address",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000", xff: "1.2.3.4:56789",
			want: "1.2.3.4",
		},
		{
			name:    "a bracketed IPv6 entry with a port is still read as a client address",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000", xff: "[2001:db8::1]:443",
			want: "2001:db8::1",
		},
		{
			name:    "a trusted hop written with a port is still recognised as trusted",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000", xff: "198.51.100.9, 192.0.2.8:41000",
			want: "198.51.100.9",
		},
		{
			name:    "garbage with a colon still falls back to the peer",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000", xff: "not-an-ip:not-a-port",
			want: "192.0.2.7",
		},
		{
			// net.SplitHostPort accepts a non-numeric port; it only requires a host:port shape.
			name:    "a non-numeric port after a valid IP still yields the IP",
			trusted: "192.0.2.0/24", peer: "192.0.2.7:41000", xff: "1.2.3.4:abc",
			want: "1.2.3.4",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trusted, err := config.ParseTrustedProxies(tc.trusted)
			if err != nil {
				t.Fatalf("ParseTrustedProxies(%q): %v", tc.trusted, err)
			}
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.peer
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			for _, line := range tc.xffAdd {
				r.Header.Add("X-Forwarded-For", line)
			}
			if tc.realIP != "" {
				r.Header.Set("X-Real-IP", tc.realIP)
			}
			if got := auth.ClientIP(r, trusted); got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxiesRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"nonsense", "192.0.2.0/33", "192.0.2.1, oops", "192.0.2.0/", "0.0.0.0/0", "::/0", "::ffff:0.0.0.0/96"} {
		if _, err := config.ParseTrustedProxies(raw); err == nil {
			t.Errorf("ParseTrustedProxies(%q) accepted an invalid entry", raw)
		}
	}
	got, err := config.ParseTrustedProxies("  192.0.2.1 , 10.0.0.0/8 , 10.0.0.1/32")
	if err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
	if len(got) != 3 || got[0].String() != "192.0.2.1/32" || got[1].String() != "10.0.0.0/8" || got[2].String() != "10.0.0.1/32" {
		t.Errorf("parsed %v, want [192.0.2.1/32 10.0.0.0/8 10.0.0.1/32]", got)
	}
}
