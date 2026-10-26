package sso

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestJWKSInputBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		redirect   bool
		accepted   bool
	}{
		{name: "keyset preserved for upstream verification", body: `{"keys":[]}`, accepted: true},
		{name: "malformed", body: `{"keys":`},
		{name: "oversized", body: strings.Repeat(" ", 1<<20) + `{"keys":[]}`},
		{name: "redirect cannot bypass key validation", redirect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var redirected atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/elsewhere" {
					redirected.Add(1)
					_, _ = io.WriteString(w, `{"keys":[]}`)
					return
				}
				if tc.redirect {
					http.Redirect(w, r, "/elsewhere", http.StatusFound)
					return
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := &http.Client{Transport: strongJWKSTransport{base: http.DefaultTransport}}
			resp, err := client.Get(server.URL)
			if (err == nil) != tc.accepted {
				t.Fatalf("accepted=%v, error=%v", tc.accepted, err)
			}
			if resp != nil {
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil || string(body) != tc.body {
					t.Fatal("rewrote JWKS input")
				}
			}
			if redirected.Load() != 0 {
				t.Fatal("followed JWKS redirect")
			}
		})
	}
}
