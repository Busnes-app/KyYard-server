package registry

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testUser   = "robot"
	testSecret = "s3cret-value"
	testToken  = "tok-abc123"
	indexType  = "application/vnd.oci.image.index.v1+json"
	singleType = "application/vnd.oci.image.manifest.v1+json"
)

var (
	testCred  = &Credential{Username: testUser, Secret: testSecret}
	basicAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte(testUser+":"+testSecret))
	indexBody = []byte(`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
		{"platform":{"os":"linux","architecture":"amd64"}},
		{"platform":{"os":"linux","architecture":"arm64","variant":"v8"}},
		{"platform":{"os":"linux","architecture":"amd64"}},
		{"platform":{"os":"unknown","architecture":"unknown"}},
		{"platform":{"os":"","architecture":"arm"}},
		{}]}`)
	singleBody = []byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
)

type hit struct{ Host, Path, Auth string }

// recorder logs every request any fake server receives, so a test can prove where a
// credential went.
type recorder struct {
	mu   sync.Mutex
	hits []hit
}

func (rc *recorder) wrap(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		rc.hits = append(rc.hits, hit{r.Host, r.URL.Path, r.Header.Get("Authorization")})
		rc.mu.Unlock()
		h(w, r)
	})
}

func (rc *recorder) all() []hit {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]hit(nil), rc.hits...)
}

func (rc *recorder) count(path string) int {
	n := 0
	for _, h := range rc.all() {
		if h.Path == path {
			n++
		}
	}
	return n
}

func newServer(t *testing.T, rc *recorder, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(rc.wrap(h))
	t.Cleanup(srv.Close)
	return srv
}

// hostOf names a fake server by a hostname its certificate covers (*.example.com).
func hostOf(srv *httptest.Server, name string) string {
	u, _ := url.Parse(srv.URL)
	return name + ".example.com:" + u.Port()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func serve(w http.ResponseWriter, mediaType string, body []byte) {
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", digestOf(body))
	_, _ = w.Write(body)
}

// testClient trusts the fake servers' certificate and dials every *.example.com name to
// loopback. Only a package test can set allowLoopback.
func testClient(t *testing.T, srv *httptest.Server, opts Options) *Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	opts.RootCAs = pool
	opts.AllowPrivate = true
	opts.DialAddr = func(_ context.Context, host string) (netip.Addr, error) {
		if !strings.HasSuffix(host, ".example.com") {
			return netip.Addr{}, errors.New("unexpected host " + host)
		}
		return netip.MustParseAddr("127.0.0.1"), nil
	}
	c := New(opts)
	c.allowLoopback = true
	return c
}

func ref(srv *httptest.Server) Reference {
	return Reference{Host: hostOf(srv, "registry"), Repository: "library/alpine", Tag: "3.24"}
}

// noSecrets fails when an error string carries a credential or token.
func noSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range []string{testSecret, testToken, basicAuth} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error %q leaks a secret", err)
		}
	}
}

func TestResolveAnonymousIndex(t *testing.T) {
	rc := &recorder{}
	srv := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/library/alpine/manifests/3.24" || !strings.Contains(r.Header.Get("Accept"), indexType) {
			http.NotFound(w, r)
			return
		}
		serve(w, indexType, indexBody)
	})
	got, err := testClient(t, srv, Options{}).Resolve(context.Background(), ref(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != digestOf(indexBody) || got.MediaType != indexType || got.Checked.IsZero() {
		t.Fatalf("got %+v", got)
	}
	if want := []string{"linux/amd64", "linux/arm64/v8"}; !reflect.DeepEqual(got.Platforms, want) {
		t.Fatalf("platforms %v, want %v", got.Platforms, want)
	}
	if n := len(rc.all()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

func TestResolveSingleManifest(t *testing.T) {
	srv := newServer(t, &recorder{}, func(w http.ResponseWriter, r *http.Request) { serve(w, singleType, singleBody) })
	got, err := testClient(t, srv, Options{}).Resolve(context.Background(), ref(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != digestOf(singleBody) || got.MediaType != singleType || len(got.Platforms) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveByDigestMustMatch(t *testing.T) {
	srv := newServer(t, &recorder{}, func(w http.ResponseWriter, r *http.Request) { serve(w, singleType, singleBody) })
	c := testClient(t, srv, Options{})
	r := ref(srv)
	r.Tag, r.Digest = "", digestOf(singleBody)
	if _, err := c.Resolve(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	r.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := c.Resolve(context.Background(), r, nil); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err %v, want ErrDigestMismatch", err)
	}
}

// bearerPair is a registry that challenges for a token from a separate auth server.
func bearerPair(t *testing.T, rc *recorder, wantBasic string) *httptest.Server {
	t.Helper()
	var reg *httptest.Server
	auth := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/token" || q.Get("service") != "fake" || q.Get("scope") != "repository:library/alpine:pull" || r.Header.Get("Authorization") != wantBasic {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"token":"` + testToken + `"}`))
	})
	reg = newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+hostOf(auth, "auth")+`/token",service="fake",scope="repository:library/alpine:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		serve(w, indexType, indexBody)
	})
	return reg
}

func TestResolveBearerChallenge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cred      *Credential
		wantBasic string
	}{
		{"anonymous", nil, ""},
		{"credential", testCred, basicAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := &recorder{}
			reg := bearerPair(t, rc, tc.wantBasic)
			c := testClient(t, reg, Options{})
			var mu sync.Mutex
			lookups := map[string]int{}
			inner := c.opts.DialAddr
			c.opts.DialAddr = func(ctx context.Context, host string) (netip.Addr, error) {
				mu.Lock()
				lookups[host]++
				mu.Unlock()
				return inner(ctx, host)
			}
			got, err := c.Resolve(context.Background(), ref(reg), tc.cred)
			if err != nil {
				t.Fatal(err)
			}
			// Each host is resolved once; the retry reuses the pinned address.
			if want := map[string]int{"registry.example.com": 1, "auth.example.com": 1}; !reflect.DeepEqual(lookups, want) {
				t.Fatalf("lookups %v, want %v", lookups, want)
			}
			if got.Digest != digestOf(indexBody) {
				t.Fatalf("got %+v", got)
			}
			hits := rc.all()
			want := []string{"", tc.wantBasic, "Bearer " + testToken}
			if len(hits) != 3 {
				t.Fatalf("hits %+v", hits)
			}
			for i, h := range hits {
				if h.Auth != want[i] {
					t.Fatalf("request %d (%s%s) carried %q, want %q", i, h.Host, h.Path, h.Auth, want[i])
				}
			}
			if hits[1].Path != "/token" || !strings.HasPrefix(hits[1].Host, "auth.") || !strings.HasPrefix(hits[2].Host, "registry.") {
				t.Fatalf("hits %+v", hits)
			}
		})
	}
}

func TestResolveBasicChallenge(t *testing.T) {
	rc := &recorder{}
	srv := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != basicAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		serve(w, singleType, singleBody)
	})
	c := testClient(t, srv, Options{})
	if _, err := c.Resolve(context.Background(), ref(srv), testCred); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resolve(context.Background(), ref(srv), nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous against Basic: %v, want ErrUnauthorized", err)
	}
	if _, err := c.Resolve(context.Background(), ref(srv), &Credential{Username: testUser, Secret: "wrong"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong secret: %v, want ErrUnauthorized", err)
	}
}

func TestResolveStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrNotFound},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusForbidden, ErrUnauthorized},
		{http.StatusUnauthorized, ErrUnauthorized}, // no challenge
		{http.StatusInternalServerError, ErrUnavailable},
		{http.StatusFound, ErrUnavailable},
		{http.StatusTemporaryRedirect, ErrUnavailable},
	} {
		rc := &recorder{}
		srv := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://"+r.Host+"/elsewhere")
			w.WriteHeader(tc.status)
		})
		_, err := testClient(t, srv, Options{}).Resolve(context.Background(), ref(srv), testCred)
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err %v, want %v", tc.status, err, tc.want)
		}
		noSecrets(t, err)
		if n := len(rc.all()); n != 1 {
			t.Errorf("status %d: %d requests, want 1 (no redirect followed)", tc.status, n)
		}
	}
}

func TestResolveRefusesBadManifestResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		want error
	}{
		{"digest mismatch", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Docker-Content-Digest", digestOf([]byte("other")))
			_, _ = w.Write(singleBody)
		}, ErrDigestMismatch},
		{"empty body", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Docker-Content-Digest", digestOf(nil))
		}, ErrUnavailable},
		{"missing digest", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(singleBody) }, ErrUnavailable},
		{"malformed digest", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Docker-Content-Digest", "sha256:ABC")
			_, _ = w.Write(singleBody)
		}, ErrUnavailable},
		{"body over 4 MiB", func(w http.ResponseWriter, r *http.Request) {
			big := []byte(`{"layers":[],"pad":"` + strings.Repeat("a", maxBody) + `"}`)
			w.Header().Set("Docker-Content-Digest", digestOf(big))
			_, _ = w.Write(big)
		}, ErrUnavailable},
		{"challenge over 4 KiB", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+strings.Repeat("a", maxChallenge)+`"`)
			w.WriteHeader(http.StatusUnauthorized)
		}, ErrUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, &recorder{}, tc.h)
			_, err := testClient(t, srv, Options{}).Resolve(context.Background(), ref(srv), nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCheckAddr(t *testing.T) {
	for _, tc := range []struct {
		addr                   string
		public, privateAllowed bool
	}{
		{"8.8.8.8", true, true},
		{"2606:4700::1111", true, true},
		{"10.0.0.1", false, true},
		{"192.168.1.1", false, true},
		{"172.16.0.1", false, true},
		{"100.64.0.1", false, true},
		{"fd00::1", false, true},
		{"::ffff:10.0.0.1", false, true},
		{"64:ff9b::a00:1", false, true},
		{"64:ff9b::808:808", true, true},
		{"64:ff9b:1::a00:1", false, false},
		{"64:ff9b:1::808:808", false, false},
		{"127.0.0.1", false, false},
		{"::1", false, false},
		{"::ffff:127.0.0.1", false, false},
		{"0.0.0.0", false, false},
		{"::", false, false},
		{"169.254.169.254", false, false},
		{"fe80::1", false, false},
		{"224.0.0.1", false, false},
		{"ff02::1", false, false},
	} {
		a := netip.MustParseAddr(tc.addr)
		if got := checkAddr(a, false) == nil; got != tc.public {
			t.Errorf("checkAddr(%s, false) allowed=%v, want %v", tc.addr, got, tc.public)
		}
		if got := checkAddr(a, true) == nil; got != tc.privateAllowed {
			t.Errorf("checkAddr(%s, true) allowed=%v, want %v", tc.addr, got, tc.privateAllowed)
		}
	}
}

func TestResolveRefusesPrivateDestination(t *testing.T) {
	for _, tc := range []struct {
		addr         string
		allowPrivate bool
	}{
		{"10.0.0.1", false},
		{"100.64.1.1", false},
		{"127.0.0.1", true}, // loopback stays refused with AllowPrivate
		{"169.254.169.254", true},
	} {
		c := New(Options{AllowPrivate: tc.allowPrivate, DialAddr: func(context.Context, string) (netip.Addr, error) {
			return netip.MustParseAddr(tc.addr), nil
		}})
		_, err := c.Resolve(context.Background(), Reference{Host: "registry.example.com", Repository: "app", Tag: "v1"}, testCred)
		if !errors.Is(err, ErrPrivateDestination) {
			t.Errorf("%s allowPrivate=%v: err %v, want ErrPrivateDestination", tc.addr, tc.allowPrivate, err)
		}
	}
}

// A credential reaches only the configured host and the realm that host advertised; a
// redirect from either never carries it anywhere.
func TestCredentialNeverLeavesTheConfiguredHosts(t *testing.T) {
	rc := &recorder{}
	other := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) { serve(w, singleType, singleBody) })
	otherURL := "https://" + hostOf(other, "other") + "/v2/library/alpine/manifests/3.24"

	t.Run("redirect after basic", func(t *testing.T) {
		reg := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, otherURL, http.StatusTemporaryRedirect)
		})
		_, err := testClient(t, reg, Options{}).Resolve(context.Background(), ref(reg), testCred)
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err %v, want ErrUnavailable", err)
		}
		noSecrets(t, err)
	})

	t.Run("redirect from realm", func(t *testing.T) {
		auth := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://"+hostOf(other, "other")+"/token", http.StatusFound)
		})
		reg := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+hostOf(auth, "auth")+`/token",service="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
		})
		_, err := testClient(t, reg, Options{}).Resolve(context.Background(), ref(reg), testCred)
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err %v, want ErrUnavailable", err)
		}
		noSecrets(t, err)
	})

	for _, h := range rc.all() {
		switch {
		case strings.HasPrefix(h.Host, "other."):
			t.Errorf("request reached the other host: %+v", h)
		case h.Auth == basicAuth && strings.HasPrefix(h.Host, "auth.") && h.Path == "/token":
		case h.Auth == basicAuth && strings.HasPrefix(h.Host, "registry.") && h.Path == "/v2/library/alpine/manifests/3.24":
		case h.Auth != "":
			t.Errorf("unexpected Authorization on %+v", h)
		}
	}
}

// The realm must be HTTPS without userinfo; live recording servers prove the credential
// never reaches a refused realm.
func TestResolveRefusesRealmThatIsNotHTTPS(t *testing.T) {
	authRC := &recorder{}
	token := func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"token":"` + testToken + `"}`)) }
	plain := httptest.NewServer(authRC.wrap(token))
	t.Cleanup(plain.Close)
	tlsAuth := newServer(t, authRC, token)
	for _, realm := range []string{
		"http://" + hostOf(plain, "auth") + "/token",
		"https://robot:pw@" + hostOf(tlsAuth, "auth") + "/token",
		"/token",
		"",
	} {
		rc := &recorder{}
		srv := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`",service="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
		})
		_, err := testClient(t, srv, Options{}).Resolve(context.Background(), ref(srv), testCred)
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("realm %q: err %v, want ErrUnavailable", realm, err)
		}
		if n := len(rc.all()); n != 1 {
			t.Errorf("realm %q: %d registry requests, want 1", realm, n)
		}
	}
	if hits := authRC.all(); len(hits) != 0 {
		t.Fatalf("a refused realm was contacted: %+v", hits)
	}
}

// The realm host passes the same egress guard as the registry host.
func TestResolveGuardsTheRealmHost(t *testing.T) {
	rc := &recorder{}
	auth := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token":"` + testToken + `"}`))
	})
	reg := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+hostOf(auth, "auth")+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	pool := x509.NewCertPool()
	pool.AddCert(reg.Certificate())
	c := New(Options{RootCAs: pool, DialAddr: func(_ context.Context, host string) (netip.Addr, error) {
		if host == "auth.example.com" {
			return netip.MustParseAddr("10.0.0.1"), nil
		}
		return netip.MustParseAddr("127.0.0.1"), nil
	}})
	c.allowLoopback = true
	_, err := c.Resolve(context.Background(), ref(reg), testCred)
	if !errors.Is(err, ErrPrivateDestination) {
		t.Fatalf("err %v, want ErrPrivateDestination", err)
	}
	for _, h := range rc.all() {
		if strings.HasPrefix(h.Host, "auth.") {
			t.Fatalf("the private realm was contacted: %+v", h)
		}
	}
}

// The transport dials only hosts the guard has pinned.
func TestTransportRefusesAnUncheckedHost(t *testing.T) {
	s := newSession(New(Options{}))
	conn, err := s.http.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "other.example.com:443")
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, ErrPrivateDestination) {
		t.Fatalf("err %v, want ErrPrivateDestination", err)
	}
}

// A hand-built Reference cannot move the request or its credential off the named host.
func TestResolveRefusesReferencesThatEscapeTheURL(t *testing.T) {
	for _, r := range []Reference{
		{Host: "registry.example.com@evil.com", Repository: "app", Tag: "v1"},
		{Host: "registry.example.com/evil", Repository: "app", Tag: "v1"},
		{Host: "registry.example.com", Repository: "app?x=1", Tag: "v1"},
		{Host: "registry.example.com", Repository: "app#frag", Tag: "v1"},
		{Host: "registry.example.com", Repository: "../app", Tag: "v1"},
		{Host: "registry.example.com", Repository: "app%2Fother", Tag: "v1"},
		{Host: "registry.example.com", Repository: "app", Tag: "v1/../../x"},
	} {
		c := New(Options{DialAddr: func(_ context.Context, host string) (netip.Addr, error) {
			t.Errorf("%+v: looked up %q", r, host)
			return netip.Addr{}, errors.New("no")
		}})
		if _, err := c.Resolve(context.Background(), r, testCred); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("%+v: err %v, want ErrInvalidReference", r, err)
		}
	}
}

func TestResolveCapsPlatforms(t *testing.T) {
	var entries []string
	for i := range maxPlatforms + 10 {
		entries = append(entries, fmt.Sprintf(`{"platform":{"os":"linux","architecture":"arch%d"}}`, i))
	}
	body := []byte(`{"manifests":[` + strings.Join(entries, ",") + `]}`)
	srv := newServer(t, &recorder{}, func(w http.ResponseWriter, r *http.Request) { serve(w, indexType, body) })
	got, err := testClient(t, srv, Options{}).Resolve(context.Background(), ref(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Platforms) != maxPlatforms {
		t.Fatalf("%d platforms, want %d", len(got.Platforms), maxPlatforms)
	}
}

func TestResolveMediaTypeFallback(t *testing.T) {
	for body, want := range map[string]string{
		`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`: indexType,
		`{"mediaType":"text/html\u0007<script>"}`:                                "",
	} {
		srv := newServer(t, &recorder{}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Docker-Content-Digest", digestOf([]byte(body)))
			w.Header()["Content-Type"] = nil
			_, _ = w.Write([]byte(body))
		})
		got, err := testClient(t, srv, Options{}).Resolve(context.Background(), ref(srv), nil)
		if err != nil || got.MediaType != want {
			t.Errorf("body %s: media type %q, %v; want %q", body, got.MediaType, err, want)
		}
	}
}

func TestResolveRequestBudget(t *testing.T) {
	rc := &recorder{}
	auth := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"` + testToken + `"}`))
	})
	reg := newServer(t, rc, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+hostOf(auth, "auth")+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := testClient(t, reg, Options{}).Resolve(context.Background(), ref(reg), testCred)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err %v, want ErrUnauthorized", err)
	}
	noSecrets(t, err)
	if m, tok := rc.count("/v2/library/alpine/manifests/3.24"), rc.count("/token"); m != 2 || tok != 1 {
		t.Fatalf("%d manifest and %d token requests, want 2 and 1", m, tok)
	}
}

func TestResolveHonoursTimeouts(t *testing.T) {
	srv := newServer(t, &recorder{}, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	start := time.Now()
	_, err := testClient(t, srv, Options{Timeout: 100 * time.Millisecond}).Resolve(context.Background(), ref(srv), nil)
	if !errors.Is(err, ErrUnavailable) || time.Since(start) > 2*time.Second {
		t.Fatalf("Options.Timeout: err %v after %v", err, time.Since(start))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = testClient(t, srv, Options{}).Resolve(ctx, ref(srv), nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("context deadline: err %v after %v", err, time.Since(start))
	}
}
