package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Probe without loading configuration: a healthcheck must never create keys or open a DB.
func healthcheck() error {
	host := strings.TrimSpace(os.Getenv("KY_HOST"))
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	port := strings.TrimSpace(os.Getenv("KY_PORT"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("PORT"))
	}
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/health/ready")
	if err != nil {
		return fmt.Errorf("readiness probe failed")
	}
	defer resp.Body.Close()
	var result struct {
		Status string `json:"status"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1024)).Decode(&result) != nil || result.Status != "ok" {
		return fmt.Errorf("server is not ready")
	}
	return nil
}
