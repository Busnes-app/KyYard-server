package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func adminRequest(srv *api.Server, cookie *http.Cookie, csrf, method, path string, body any) *httptest.ResponseRecorder {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// An administrator creates a feed and gets its URL once; the URL alone reads RSS 2.0 with no
// session; a revoked or guessed URL is a plain 404.
func TestStatusFeedServesRSSAndRevokes(t *testing.T) {
	srv, _, cookie, csrf, org := tenantAdminFixture(t)
	base := "/api/organizations/" + org + "/status-feeds"
	w := adminRequest(srv, cookie, csrf, "POST", base, map[string]string{"name": "uptime monitor"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created struct {
		URL        string `json:"url"`
		StatusFeed struct {
			ID string `json:"id"`
		} `json:"status_feed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(created.URL)
	if err != nil || !strings.HasPrefix(u.Path, "/api/feeds/kyy_feed_") || !strings.HasSuffix(u.Path, "/down.rss") {
		t.Fatalf("feed URL: %q", created.URL)
	}
	if list := adminRequest(srv, cookie, csrf, "GET", base, nil); list.Code != http.StatusOK || strings.Contains(list.Body.String(), "kyy_feed_") {
		t.Fatalf("list leaks the token or failed: %d %s", list.Code, list.Body)
	}

	read := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}
	got := read(u.Path)
	if got.Code != http.StatusOK || !strings.HasPrefix(got.Header().Get("Content-Type"), "application/rss+xml") {
		t.Fatalf("feed: %d %v %s", got.Code, got.Header(), got.Body)
	}
	feedHeaders(t, got)
	var doc struct {
		Version string `xml:"version,attr"`
		Channel struct {
			Title       string     `xml:"title"`
			Description string     `xml:"description"`
			Items       []struct{} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(got.Body.Bytes(), &doc); err != nil || doc.Version != "2.0" || doc.Channel.Title != "KyYard down: Fixture" || len(doc.Channel.Items) != 0 || doc.Channel.Description != "Nothing is down." {
		t.Fatalf("rss: %+v %v\n%s", doc, err, got.Body)
	}

	guess := read("/api/feeds/kyy_feed_" + strings.Repeat("0", 64) + "/down.rss")
	if guess.Code != http.StatusNotFound {
		t.Fatalf("guessed feed: %d", guess.Code)
	}
	feedHeaders(t, guess)
	for _, bad := range []string{"/api/feeds/" + strings.Repeat("a", 200) + "/down.rss", "/api/feeds/kyy_feed_zz/down.rss", "/api/feeds/" + strings.TrimPrefix(strings.TrimSuffix(u.Path, "/down.rss"), "/api/feeds/kyy_feed_") + "/down.rss"} {
		if w := read(bad); w.Code != http.StatusNotFound || w.Body.String() != guess.Body.String() {
			t.Errorf("%s: %d %q, want the same 404 as a guess", bad, w.Code, w.Body)
		}
	}
	if del := adminRequest(srv, cookie, csrf, "DELETE", base+"/"+created.StatusFeed.ID, nil); del.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", del.Code, del.Body)
	}
	if after := read(u.Path); after.Code != http.StatusNotFound || after.Body.String() != guess.Body.String() {
		t.Fatalf("revoked feed: %d %q", after.Code, after.Body)
	}
}

// feedHeaders: a feed answer is never cached or sniffed, never indexed, and sends no Referer
// carrying the URL when a reader follows a link.
func feedHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for k, v := range map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Robots-Tag": "noindex"} {
		if got := w.Header().Values(k); len(got) != 1 || got[0] != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// Only the organization administrator lists, creates or revokes feeds; every other role and a
// member of another organization is refused, and nothing is created.
func TestStatusFeedManagementByRole(t *testing.T) {
	srv, st, cookie, csrf, org := tenantAdminFixture(t)
	base := "/api/organizations/" + org + "/status-feeds"
	w := adminRequest(srv, cookie, csrf, "POST", base, map[string]string{"name": "pager"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created struct {
		StatusFeed struct{ ID string } `json:"status_feed"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	ctx := context.Background()
	if err := st.Tenancy().CreateOrganizationWithAdmin(ctx, &store.Organization{ID: "org_other", Name: "Other"}, "usr_orgadmin"); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"environment_admin", "operator", "developer", "read_only", "outsider"} {
		member := loginAs(t, srv, st, "m_"+role, "user")
		if role != "outsider" {
			if err := st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: org, UserID: "usr_m_" + role, Role: store.TenantRole(role), Status: "active"}); err != nil {
				t.Fatal(err)
			}
		}
		for _, c := range []struct{ method, path string }{{"GET", base}, {"POST", base}, {"DELETE", base + "/" + created.StatusFeed.ID}} {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{"name":"mine"}`))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(member)
			req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
			req.Header.Set(auth.HeaderCSRF, "test-csrf")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
				t.Errorf("%s %s %s: %d %s", role, c.method, c.path, rec.Code, rec.Body)
			}
		}
	}
	// The administrator of both organizations names this feed under the other one.
	if w := adminRequest(srv, cookie, csrf, "DELETE", "/api/organizations/org_other/status-feeds/"+created.StatusFeed.ID, nil); w.Code != http.StatusNotFound {
		t.Fatalf("revoked a feed through another organization: %d %s", w.Code, w.Body)
	}
	list, err := st.Tenancy().ListStatusFeeds(ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: org})
	if err != nil || len(list) != 1 || list[0].ID != created.StatusFeed.ID {
		t.Fatalf("feeds after refused attempts: %+v %v", list, err)
	}
}

// The feed carries organization, environment, endpoint and container names and conditions,
// nothing else from the inventory: no image, address, port, label, status text or mount.
// Hostile names arrive escaped and parse back intact. The URL never reaches the server log.
func TestStatusFeedShowsOnlyEscapedNames(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	srv, st, cookie, csrf, org := tenantAdminFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env_f", OrganizationID: org, Name: "prod & <test>"}); err != nil {
		t.Fatal(err)
	}
	tok, err := ts.CreateEnrollmentToken(ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: org, EnvironmentID: "env_f"}, protocol.RuntimeDocker, "")
	if err != nil {
		t.Fatal(err)
	}
	ag := redeemToken(t, srv, base64.RawURLEncoding.EncodeToString(tok.Secret), "nas")
	if err := ts.ApproveEndpoint(ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: org, EnvironmentID: "env_f"}, ag.id, ag.fp); err != nil {
		t.Fatal(err)
	}
	hostile := []string{`]]><evil/>`, `a&b"c'd<e>`, "<![CDATA[x]]>"}
	var containers []protocol.Container
	for i, name := range hostile {
		containers = append(containers, protocol.Container{ID: fmt.Sprintf("%063x%d", 0xc0ffee, i), CreatedAt: time.Date(2026, 9, 14, 7, 30, 0, 0, time.UTC), Name: "/" + name, State: "dead",
			Image: "10.9.8.7:5000/private/app:SECRET-TAG", Status: "Exited STATUS-TEXT", Labels: map[string]string{"password": "LABEL-SECRET"},
			Ports: []protocol.Port{{HostIP: "192.168.77.5", Host: 8443, Container: 443, Protocol: "tcp"}}, Networks: []string{"backnet"},
			NetworkAttachments: []protocol.NetworkAttachment{{Name: "backnet", IP: "172.31.4.4"}}, Mounts: []protocol.Mount{{Kind: protocol.MountBind, Source: "/srv/MOUNT-SECRET", Target: "/data"}}})
	}
	raw, _ := json.Marshal(protocol.Snapshot{Generation: 1, Engine: protocol.Engine{Version: "27.0"}, Containers: containers, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}})
	if ok, err := ts.AcceptInventory(ctx, ag.id, 1, time.Now().UTC(), raw); err != nil || !ok {
		t.Fatalf("inventory: %v %v", ok, err)
	}
	w := adminRequest(srv, cookie, csrf, "POST", "/api/organizations/"+org+"/status-feeds", map[string]string{"name": "pager"})
	var created struct{ URL string }
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	u, _ := url.Parse(created.URL)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", u.Path, nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("feed: %d %s", rec.Code, body)
	}
	for _, leak := range []string{fmt.Sprintf("%x", 0xc0ffee), "2026-09-14", "07:30:00", "10.9.8.7", "SECRET-TAG", "private/app", "STATUS-TEXT", "LABEL-SECRET", "password", "192.168.77.5", "8443", "172.31.4.4", "backnet", "MOUNT-SECRET", "<evil", "<![CDATA[x"} {
		if strings.Contains(body, leak) {
			t.Errorf("feed carries %q:\n%s", leak, body)
		}
	}
	var doc struct {
		Channel struct {
			Items []struct {
				Title       string `xml:"title"`
				Description string `xml:"description"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil || len(doc.Channel.Items) != len(hostile) {
		t.Fatalf("rss: %v %+v\n%s", err, doc, body)
	}
	titles := map[string]bool{}
	for _, it := range doc.Channel.Items {
		titles[it.Title] = true
		if !strings.Contains(it.Description, "on nas (prod & <test>) is dead") {
			t.Errorf("description: %q", it.Description)
		}
	}
	for _, name := range hostile {
		if !titles["Dead: "+name+" on nas"] {
			t.Errorf("no item for %q in %v", name, titles)
		}
	}
	if secret := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/api/feeds/kyy_feed_"), "/down.rss"); strings.Contains(logs.String(), secret) {
		t.Fatalf("the feed token reached the log:\n%s", logs.String())
	}
}

// A kyPulse service token reads the organization but never manages its feeds.
func TestServiceTokenCannotManageStatusFeeds(t *testing.T) {
	srv, _, cookie, csrf, org := tenantAdminFixture(t)
	token := pairAndClaim(t, srv, cookie, csrf, org)
	for _, m := range []string{"GET", "POST", "DELETE"} {
		path := "/api/organizations/" + org + "/status-feeds"
		if m == "DELETE" {
			path += "/feed_x"
		}
		req := httptest.NewRequest(m, path, strings.NewReader(`{"name":"pulse"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("service token %s feeds: %d %s", m, w.Code, w.Body)
		}
	}
}

// One address is limited across guesses, so the endpoint is no token oracle; one feed is
// limited across addresses, so a leaked URL bounds what it costs the server. The per-feed
// window is keyed on the feed found, never on caller input: a case variant of the token is
// the same 404 as a guess, not a second budget, and guesses add no per-token window.
func TestStatusFeedReadsAreRateLimited(t *testing.T) {
	srv, _, cookie, csrf, org := tenantAdminFixture(t)
	w := adminRequest(srv, cookie, csrf, "POST", "/api/organizations/"+org+"/status-feeds", map[string]string{"name": "monitor"})
	var created struct{ URL string }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	u, _ := url.Parse(created.URL)
	secret := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/api/feeds/kyy_feed_"), "/down.rss")
	read := func(path, addr string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code
	}
	limitedAfter := func(path func(int) string, addr func(int) string) int {
		for i := 0; i < 100; i++ {
			if read(path(i), addr(i)) == http.StatusTooManyRequests {
				return i
			}
		}
		return -1
	}
	guess := func(i int) string { return fmt.Sprintf("/api/feeds/kyy_feed_%064x/down.rss", i) }
	if n := limitedAfter(guess, func(int) string { return "198.51.100.7:1000" }); n != 60 {
		t.Fatalf("guesses from one address limited after %d, want 60", n)
	}
	// Guesses from many addresses are 404s and leave no per-token window behind.
	for i := 0; i < 50; i++ {
		if code := read(guess(1<<20+i), fmt.Sprintf("192.0.2.%d:1000", i)); code != http.StatusNotFound {
			t.Fatalf("guess %d: %d, want 404", i, code)
		}
	}
	for _, k := range api.AttemptKeysForTest(srv) {
		if strings.HasPrefix(k, "status-feed-token:") {
			t.Fatalf("a guess created limiter key %q", k)
		}
	}
	upper := "/api/feeds/kyy_feed_" + strings.ToUpper(secret) + "/down.rss"
	if code := read(upper, "203.0.113.200:1000"); code != http.StatusNotFound {
		t.Fatalf("upper-case variant of the token: %d, want 404", code)
	}
	if n := limitedAfter(func(int) string { return u.Path }, func(i int) string { return fmt.Sprintf("203.0.113.%d:1000", i) }); n != 30 {
		t.Fatalf("one feed from many addresses limited after %d, want 30", n)
	}
	if code := read(upper, "203.0.113.201:1000"); code != http.StatusNotFound {
		t.Fatalf("upper-case variant once the feed is limited: %d, want 404", code)
	}
	if feedKeys := slices.DeleteFunc(api.AccountKeysForTest(srv), func(k string) bool { return !strings.HasPrefix(k, "status-feed-id:feed_") }); len(feedKeys) != 1 {
		t.Fatalf("per-feed windows: %q, want one keyed on the feed ID", feedKeys)
	}
}
