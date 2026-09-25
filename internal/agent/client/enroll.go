package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// Facts is the bounded enrollment report; the server keeps only the keys it knows.
func Facts(runtimeVersion string) map[string]string {
	host, _ := os.Hostname()
	if runtimeVersion == "" {
		runtimeVersion = "unknown"
	}
	return map[string]string{"hostname": host, "os": runtime.GOOS + "/" + runtime.GOARCH, "cpus": strconv.Itoa(runtime.NumCPU()), "runtime_version": runtimeVersion}
}

// Enroll redeems a token for an identity and pins the server's instance fingerprint. The token
// is used once and dropped; only the resulting identity is persisted.
func Enroll(ctx context.Context, httpClient *http.Client, server, dir, name, tokenB64 string, runtimeVersion string) (*Identity, error) {
	if _, err := checkServerOrigin(server); err != nil {
		return nil, err
	}
	token, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(tokenB64))
	if err != nil || len(token) != protocol.TokenSize {
		return nil, errors.New("enrollment token is not a valid 32-byte token")
	}
	pub, priv, err := newKey()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{
		"token": strings.TrimSpace(tokenB64), "public_key": base64.RawURLEncoding.EncodeToString(pub),
		"proof": base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, token))),
		"name":  name, "facts": Facts(runtimeVersion),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+"/api/agent/v1/enroll", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("enrollment refused (HTTP %d)", resp.StatusCode)
	}
	var reply struct {
		EndpointID          string `json:"endpoint_id"`
		Fingerprint         string `json:"fingerprint"`
		InstanceFingerprint string `json:"instance_fingerprint"`
	}
	if err := json.Unmarshal(out, &reply); err != nil || reply.EndpointID == "" || len(reply.InstanceFingerprint) != 64 {
		return nil, errors.New("enrollment reply incomplete")
	}
	if reply.Fingerprint != protocol.Fingerprint(pub) {
		return nil, errors.New("server echoed a fingerprint that is not ours")
	}
	id := &Identity{EndpointID: reply.EndpointID, PrivateKey: priv, InstanceFingerprint: reply.InstanceFingerprint, Server: strings.TrimRight(server, "/"), EnrollmentHash: enrollmentHash(tokenB64), RotatedAt: time.Now().UTC()}
	if err := SaveIdentity(dir, id); err != nil {
		return nil, err
	}
	return id, nil
}

// ReadToken supports stdin enrollment for native clients and older installations.
func ReadToken(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 256))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", errors.New("no enrollment token on stdin and no identity on disk")
	}
	return tok, nil
}

// ParseLink accepts only the generated HTTPS enrollment link. The token fragment
// is extracted locally and never sent as part of an HTTP URL or error message.
func ParseLink(link string) (server, token string, err error) {
	invalid := errors.New("invalid enrollment link; copy a fresh link from KyYard")
	if len(link) > 4096 {
		return "", "", invalid
	}
	u, e := url.Parse(link)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") || !strings.HasPrefix(u.Fragment, "kyyard=") {
		return "", "", invalid
	}
	token = strings.TrimPrefix(u.Fragment, "kyyard=")
	raw, e := base64.RawURLEncoding.DecodeString(token)
	if e != nil || len(raw) != protocol.TokenSize || base64.RawURLEncoding.EncodeToString(raw) != token {
		return "", "", invalid
	}
	server = "https://" + u.Host
	if _, e := checkServerOrigin(server); e != nil {
		return "", "", invalid
	}
	return server, token, nil
}

func enrollmentHash(token string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// MatchesEnrollment prevents a new link from silently reusing another enrollment.
func (id *Identity) MatchesEnrollment(server, token string) bool {
	return id.Server == server && id.EnrollmentHash != "" && id.EnrollmentHash == enrollmentHash(token)
}
