package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthcheckUsesReadinessWithoutBootstrapping(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		pass       bool
	}{
		{"ready", `{"status":"ok"}`, 200, true},
		{"SPA", `<html>ok</html>`, 200, false},
		{"unavailable", `{"status":"unavailable"}`, 503, false},
		{"redirect", ``, 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health/ready" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.Header().Set("Location", "/health/ready")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
			t.Setenv("KY_HOST", host)
			t.Setenv("KY_PORT", port)
			data := filepath.Join(t.TempDir(), "must-not-exist")
			t.Setenv("KY_DATA_DIR", data)
			if err := healthcheck(); (err == nil) != tc.pass {
				t.Fatalf("probe error: %v", err)
			}
			if _, err := os.Stat(data); !os.IsNotExist(err) {
				t.Fatal("healthcheck initialized data")
			}
		})
	}
}
