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
	"path"
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
	maxToken     = 8 << 10
	maxPlatforms = 64
	// dockerHubAPI serves docker.io's registry API.
	dockerHubAPI = "registry-1.docker.io"
)

var (
	manifestTypes = []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}
	manifestAccept = strings.Join(manifestTypes, ", ")
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
	Platforms []string // "os/arch[/variant]" from an index; empty for a single manifest, nil from Head
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

// session is one Resolve or Head: its transport dials only addresses the guard pinned.
type session struct {
	c      *Client
	http   *http.Client
	mu     sync.Mutex
	pinned map[string]netip.Addr
}

func newSession(c *Client) *session {
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
	s.http = &http.Client{
		Transport:     tr,
		Timeout:       c.opts.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return s
}

// Resolve GETs the manifest ref names and verifies its digest against the body: at most two
// manifest requests and one token request. cred goes only to ref's host or the token realm
// that host advertises. Docker Hub counts every manifest GET as a pull.
func (c *Client) Resolve(ctx context.Context, ref Reference, cred *Credential) (Resolved, error) {
	return c.manifest(ctx, http.MethodGet, ref, cred)
}

// Head reads the digest ref names from Docker-Content-Digest without the body, so Docker Hub
// does not count a pull. Platforms are nil; the same request budget and guard apply.
func (c *Client) Head(ctx context.Context, ref Reference, cred *Credential) (Resolved, error) {
	return c.manifest(ctx, http.MethodHead, ref, cred)
}

// manifest reports the caller's context error whenever it is done and the call failed: a
// registry answering as the deadline passes must not turn a cancellation into ErrUnavailable.
func (c *Client) manifest(ctx context.Context, method string, ref Reference, cred *Credential) (Resolved, error) {
	r, err := c.fetch(ctx, method, ref, cred)
	if err != nil && ctx.Err() != nil {
		return Resolved{}, fmt.Errorf("registry %s: %w", ref.Host, ctx.Err())
	}
	return r, err
}

func (c *Client) fetch(ctx context.Context, method string, ref Reference, cred *Credential) (Resolved, error) {
	if ref.Host == "" || ref.Repository == "" || (ref.Tag == "" && ref.Digest == "") {
		return Resolved{}, ErrInvalidReference
	}
	host := ref.Host
	if host == DockerHub {
		host = dockerHubAPI
	}
	s := newSession(c)
	defer s.http.CloseIdleConnections()

	pin := ref.Digest
	if pin == "" {
		pin = ref.Tag
	}
	manifestPath := "/v2/" + ref.Repository + "/manifests/" + pin
	manifestURL := "https://" + host + manifestPath
	// Reference fields are public; the URL must name exactly host and path, so a hand-built
	// Reference cannot send the credential elsewhere.
	u, err := url.Parse(manifestURL)
	if err != nil || u.Host != host || u.Path != manifestPath || path.Clean(u.Path) != u.Path ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Resolved{}, ErrInvalidReference
	}
	resp, body, err := s.do(ctx, method, manifestURL, "", true)
	if err != nil {
		return Resolved{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		auth, err := s.authorize(ctx, host, ref.Repository, resp.Header, cred)
		if err != nil {
			return Resolved{}, err
		}
		if resp, body, err = s.do(ctx, method, manifestURL, auth, true); err != nil {
			return Resolved{}, err
		}
	}
	if err := statusErr(host, resp.StatusCode); err != nil {
		return Resolved{}, err
	}
	if method == http.MethodHead {
		return parseHead(host, resp.Header, ref.Digest)
	}
	return parseManifest(host, resp.Header, body, ref.Digest)
}

// do sends one request to a pinned address; a GET reads at most maxBody bytes, a HEAD none.
func (s *session) do(ctx context.Context, method, rawURL, auth string, manifest bool) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
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
	if method == http.MethodHead {
		return resp, nil, nil
	}
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
	resp, body, err := s.do(ctx, http.MethodGet, req.URL.String(), auth, false)
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
	// It becomes a request header; a hostile realm must not make that header megabytes.
	if len(t.Token) > maxToken {
		return "", fmt.Errorf("%w: %s token over 8 KiB", ErrUnavailable, realm.Host)
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

// headerDigest is the response's Docker-Content-Digest, which must equal a requested digest.
func headerDigest(host string, h http.Header, want string) (string, error) {
	d := h.Get("Docker-Content-Digest")
	if !digestRE.MatchString(d) {
		return "", fmt.Errorf("%w: %s sent no valid Docker-Content-Digest", ErrUnavailable, host)
	}
	if want != "" && d != want {
		return "", fmt.Errorf("%w: %s", ErrDigestMismatch, host)
	}
	return d, nil
}

func mediaType(h http.Header) string {
	mt, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	if len(mt) > 255 {
		return ""
	}
	return mt
}

// parseHead trusts the TLS-authenticated header; a pull by digest verifies the content.
func parseHead(host string, h http.Header, want string) (Resolved, error) {
	d, err := headerDigest(host, h, want)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Digest: d, MediaType: mediaType(h), Checked: time.Now().UTC()}, nil
}

func parseManifest(host string, h http.Header, body []byte, want string) (Resolved, error) {
	d, err := headerDigest(host, h, want)
	if err != nil {
		return Resolved{}, err
	}
	if len(body) == 0 {
		return Resolved{}, fmt.Errorf("%w: %s sent an empty manifest", ErrUnavailable, host)
	}
	if sum := sha256.Sum256(body); "sha256:"+hex.EncodeToString(sum[:]) != d {
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
	if json.Unmarshal(body, &doc) != nil {
		return Resolved{}, fmt.Errorf("%w: %s manifest is not JSON", ErrUnavailable, host)
	}
	mt := mediaType(h)
	if mt == "" && slices.Contains(manifestTypes, doc.MediaType) {
		mt = doc.MediaType
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
