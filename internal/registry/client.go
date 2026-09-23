package registry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	ErrUnauthorized   = errors.New("registry refused the credentials")
	ErrNotFound       = errors.New("image not found in registry")
	ErrRateLimited    = errors.New("registry rate limit reached")
	ErrUnavailable    = errors.New("registry unavailable")
	ErrDigestMismatch = errors.New("registry digest does not match the manifest")
)

const (
	maxBody      = 4 << 20
	maxChallenge = 4 << 10
	maxPlatforms = 64
	// dockerHubAPI serves docker.io's registry API.
	dockerHubAPI   = "registry-1.docker.io"
	manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"
)

var (
	digestRE    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	platformRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	challengeRE = regexp.MustCompile(`([A-Za-z_]+)="([^"]*)"`)
)

// Options configures a Client. RootCAs and DialAddr exist for tests only: production uses
// the system roots and DNS.
type Options struct {
	AllowPrivate bool
	Timeout      time.Duration // per request; 15s when zero
	RootCAs      *x509.CertPool
	DialAddr     func(ctx context.Context, host string) (netip.Addr, error)
}

// Resolved is what a tag or digest points at.
type Resolved struct {
	Digest    string // Docker-Content-Digest of the manifest or index
	MediaType string
	Platforms []string // "os/arch[/variant]" from an index; empty for a single manifest
	Checked   time.Time
}

type Client struct {
	opts          Options
	allowLoopback bool // set only by package tests
}

func New(opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	return &Client{opts: opts}
}

// session is one Resolve: its transport dials only addresses the guard pinned.
type session struct {
	c      *Client
	http   *http.Client
	mu     sync.Mutex
	pinned map[string]netip.Addr
}

// Resolve reads the manifest digest ref names: at most two manifest requests and one token
// request. cred goes only to ref's host or the token realm that host advertises.
func (c *Client) Resolve(ctx context.Context, ref Reference, cred *Credential) (Resolved, error) {
	if ref.Host == "" || ref.Repository == "" || (ref.Tag == "" && ref.Digest == "") {
		return Resolved{}, ErrInvalidReference
	}
	host := ref.Host
	if host == DockerHub {
		host = dockerHubAPI
	}
	s := &session{c: c, pinned: map[string]netip.Addr{}}
	dialer := &net.Dialer{Timeout: c.opts.Timeout}
	tr := &http.Transport{
		Proxy: nil, // a proxy would dial on our behalf, past the guard
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			s.mu.Lock()
			a, ok := s.pinned[h]
			s.mu.Unlock()
			if !ok {
				return nil, fmt.Errorf("%w: %s was not checked", ErrPrivateDestination, h)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		},
		TLSClientConfig:        &tls.Config{RootCAs: c.opts.RootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    c.opts.Timeout,
		ResponseHeaderTimeout:  c.opts.Timeout,
		MaxResponseHeaderBytes: 64 << 10,
		ForceAttemptHTTP2:      true,
	}
	defer tr.CloseIdleConnections()
	s.http = &http.Client{
		Transport:     tr,
		Timeout:       c.opts.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	pin := ref.Digest
	if pin == "" {
		pin = ref.Tag
	}
	manifestURL := "https://" + host + "/v2/" + ref.Repository + "/manifests/" + pin
	resp, body, err := s.get(ctx, manifestURL, "", true)
	if err != nil {
		return Resolved{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		auth, err := s.authorize(ctx, host, ref.Repository, resp.Header, cred)
		if err != nil {
			return Resolved{}, err
		}
		if resp, body, err = s.get(ctx, manifestURL, auth, true); err != nil {
			return Resolved{}, err
		}
	}
	if err := statusErr(host, resp.StatusCode); err != nil {
		return Resolved{}, err
	}
	return parseManifest(host, resp.Header, body, ref.Digest)
}

// get sends one GET to a pinned address and reads at most maxBody bytes.
func (s *session) get(ctx context.Context, rawURL, auth string, manifest bool) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: bad request URL", ErrUnavailable)
	}
	host := req.URL.Hostname()
	s.mu.Lock()
	_, ok := s.pinned[host]
	s.mu.Unlock()
	if !ok {
		a, err := s.c.lookup(ctx, host)
		if err != nil {
			return nil, nil, err
		}
		s.mu.Lock()
		s.pinned[host] = a
		s.mu.Unlock()
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if manifest {
		req.Header.Set("Accept", manifestAccept)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, req.URL.Host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, req.URL.Host, err)
	}
	if len(body) > maxBody {
		return nil, nil, fmt.Errorf("%w: %s response over 4 MiB", ErrUnavailable, req.URL.Host)
	}
	return resp, body, nil
}

// authorize answers a 401 challenge with an Authorization value for the retry.
func (s *session) authorize(ctx context.Context, host, repo string, h http.Header, cred *Credential) (string, error) {
	unauthorized := fmt.Errorf("%w: %s 401", ErrUnauthorized, host)
	challenge := h.Get("WWW-Authenticate")
	if challenge == "" || len(challenge) > maxChallenge {
		return "", unauthorized
	}
	scheme, params, _ := strings.Cut(challenge, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		if cred == nil {
			return "", unauthorized
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+cred.Secret)), nil
	case "bearer":
		return s.token(ctx, host, repo, params, cred)
	}
	return "", unauthorized
}

// token fetches a bearer token from the realm host advertised; the credential, if any, is
// sent there as basic auth.
func (s *session) token(ctx context.Context, host, repo, params string, cred *Credential) (string, error) {
	p := map[string]string{}
	for _, m := range challengeRE.FindAllStringSubmatch(params, -1) {
		p[strings.ToLower(m[1])] = m[2]
	}
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" || realm.User != nil {
		return "", fmt.Errorf("%w: %s token realm is not an https URL", ErrUnavailable, host)
	}
	q := realm.Query()
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	scope := p["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", fmt.Errorf("%w: %s token realm", ErrUnavailable, host)
	}
	auth := ""
	if cred != nil {
		req.SetBasicAuth(cred.Username, cred.Secret)
		auth = req.Header.Get("Authorization")
	}
	resp, body, err := s.get(ctx, req.URL.String(), auth, false)
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("%w: %s token %d", ErrUnauthorized, realm.Host, resp.StatusCode)
	case http.StatusTooManyRequests:
		return "", fmt.Errorf("%w: %s token %d", ErrRateLimited, realm.Host, resp.StatusCode)
	default:
		return "", fmt.Errorf("%w: %s token %d", ErrUnavailable, realm.Host, resp.StatusCode)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &t) != nil || (t.Token == "" && t.AccessToken == "") {
		return "", fmt.Errorf("%w: %s token response unreadable", ErrUnavailable, realm.Host)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	return "Bearer " + t.Token, nil
}

func statusErr(host string, code int) error {
	var err error
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		err = ErrUnauthorized
	case http.StatusNotFound:
		err = ErrNotFound
	case http.StatusTooManyRequests:
		err = ErrRateLimited
	default:
		err = ErrUnavailable
	}
	return fmt.Errorf("%w: %s %d", err, host, code)
}

func parseManifest(host string, h http.Header, body []byte, want string) (Resolved, error) {
	d := h.Get("Docker-Content-Digest")
	if !digestRE.MatchString(d) {
		return Resolved{}, fmt.Errorf("%w: %s sent no valid Docker-Content-Digest", ErrUnavailable, host)
	}
	if len(body) > 0 {
		if sum := sha256.Sum256(body); "sha256:"+hex.EncodeToString(sum[:]) != d {
			return Resolved{}, fmt.Errorf("%w: %s", ErrDigestMismatch, host)
		}
	}
	if want != "" && d != want {
		return Resolved{}, fmt.Errorf("%w: %s", ErrDigestMismatch, host)
	}
	var doc struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if len(body) > 0 && json.Unmarshal(body, &doc) != nil {
		return Resolved{}, fmt.Errorf("%w: %s manifest is not JSON", ErrUnavailable, host)
	}
	mt, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	if mt == "" {
		mt = doc.MediaType
	}
	if len(mt) > 255 {
		mt = ""
	}
	var platforms []string
	for _, m := range doc.Manifests {
		p := m.Platform
		// Attestation entries carry unknown/unknown; they are not runnable platforms.
		if !platformRE.MatchString(p.OS) || !platformRE.MatchString(p.Architecture) || p.OS == "unknown" {
			continue
		}
		name := p.OS + "/" + p.Architecture
		if platformRE.MatchString(p.Variant) {
			name += "/" + p.Variant
		}
		if !slices.Contains(platforms, name) {
			platforms = append(platforms, name)
		}
		if len(platforms) == maxPlatforms {
			break
		}
	}
	return Resolved{Digest: d, MediaType: mt, Platforms: platforms, Checked: time.Now().UTC()}, nil
}
