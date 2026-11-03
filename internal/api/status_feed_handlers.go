package api

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

const (
	// feedPerIP is checked before the token is looked up, so guessing costs the guesser;
	// feedPerToken bounds what one leaked URL can make the server decode. It is checked after
	// a successful lookup and keyed on the feed ID, so caller input never mints a window.
	feedPerIP     = 60
	feedPerToken  = 30
	feedWindow    = time.Minute
	feedTTLMinute = 1
)

func (s *Server) handleStatusFeeds(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	list, err := s.store.Tenancy().ListStatusFeeds(r.Context(), a)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

// handleCreateStatusFeed mints a feed and shows its URL once.
func (s *Server) handleCreateStatusFeed(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	issue, err := s.store.Tenancy().CreateStatusFeed(r.Context(), a, req.Name)
	if errors.Is(err, store.ErrStatusFeedLimit) {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Revoke a status feed before creating another", "code": "status_feed_limit"})
		return
	}
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"url": s.statusFeedURL(issue.Token), "status_feed": issue.StatusFeed,
		"disclosure": "Shown once. Anyone with this URL sees this organization's name, its environment names and the names of its down endpoints, containers and nodes, so give it only to your monitoring."})
}

func (s *Server) handleRevokeStatusFeed(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id := r.PathValue("feed")
	if id == "" || len(id) > 64 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().RevokeStatusFeed(r.Context(), a, id); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) statusFeedURL(token string) string {
	return strings.TrimRight(s.config.Server.AppURL, "/") + "/api/feeds/" + token + "/down.rss"
}

type rssDocument struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	LastBuildDate string    `xml:"lastBuildDate"`
	TTL           int       `xml:"ttl"`
	Items         []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link,omitempty"`
	Description string  `xml:"description"`
	Category    string  `xml:"category"`
	GUID        rssGUID `xml:"guid"`
	PubDate     string  `xml:"pubDate,omitempty"`
}

type rssGUID struct {
	IsPermaLink bool   `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

// handleStatusFeed is unauthenticated: the token in the path is the whole proof, and it reads
// one organization's down list, nothing else. Every refusal is the same 404, so a guess learns
// nothing. An empty feed means nothing is down; a monitor that cannot fetch it at all should
// treat KyYard itself as down.
func (s *Server) handleStatusFeed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !s.allowAttempt("status-feed:"+s.limitIP(r), feedPerIP, feedWindow) {
		s.writeError(w, http.StatusTooManyRequests, "Too many feed reads; wait a minute")
		return
	}
	token := r.PathValue("token")
	if len(token) > 128 {
		http.NotFound(w, r)
		return
	}
	// The per-feed window lives with the account-keyed ones: feeds are bounded per
	// organization, so it cannot crowd out other routes' per-address windows.
	allow := func(feedID string) bool {
		return s.allowAccountAttempt("status-feed-id:"+feedID, feedPerToken, feedWindow)
	}
	report, err := s.store.Tenancy().ReadStatusFeed(r.Context(), token, s.requestIP(r), allow)
	if errors.Is(err, store.ErrForbidden) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, store.ErrStatusFeedThrottled) {
		s.writeError(w, http.StatusTooManyRequests, "Too many feed reads; wait a minute")
		return
	}
	if err != nil {
		log.Printf("[FEED] status feed read failed: %v", err)
		s.writeError(w, http.StatusServiceUnavailable, "Status unavailable")
		return
	}
	body, err := xml.MarshalIndent(s.rssOf(report, time.Now().UTC()), "", "  ")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Status unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

func (s *Server) rssOf(report *store.StatusReport, now time.Time) rssDocument {
	link := strings.TrimRight(s.config.Server.AppURL, "/")
	summary := "Nothing is down."
	if n := len(report.Items); n > 0 {
		summary = fmt.Sprintf("%d down.", n)
	}
	if report.Truncated {
		summary += " The organization has more endpoints or down items than one feed shows; open KyYard for the rest."
	}
	ch := rssChannel{Title: "KyYard down: " + report.Organization, Link: link, Description: summary, LastBuildDate: now.Format(time.RFC1123Z), TTL: feedTTLMinute, Items: make([]rssItem, 0, len(report.Items))}
	for _, d := range report.Items {
		item := rssItem{Category: d.Kind, GUID: rssGUID{Value: d.Key}}
		where := d.Endpoint
		if d.Environment != "" {
			where += " (" + d.Environment + ")"
		}
		switch d.Kind {
		case store.DownEndpointOffline:
			item.Title = "Offline: " + d.Endpoint
			item.Description = "Endpoint " + where + " is offline"
		case store.DownNodeNotReady:
			item.Title = "Node not ready: " + d.Subject + " on " + d.Endpoint
			item.Description = "Node " + d.Subject + " of cluster " + where + " is not ready"
		default:
			item.Title = strings.ToUpper(d.Condition[:1]) + d.Condition[1:] + ": " + d.Subject + " on " + d.Endpoint
			item.Description = "Container " + d.Subject + " on " + where + " is " + d.Condition
		}
		if link != "" {
			item.Link = link + "/organizations/" + url.PathEscape(report.OrganizationID) + "/endpoints/" + url.PathEscape(d.EndpointID)
		}
		if !d.At.IsZero() {
			item.PubDate = d.At.UTC().Format(time.RFC1123Z)
		}
		ch.Items = append(ch.Items, item)
	}
	return rssDocument{Version: "2.0", Channel: ch}
}
