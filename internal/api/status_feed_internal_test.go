package api

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// Each down item becomes one RSS item whose guid is its stable key and whose link opens the
// endpoint; names a host chose are escaped, never markup.
func TestRSSOfDownItems(t *testing.T) {
	s := &Server{config: &config.Config{}}
	s.config.Server.AppURL = "https://yard.example/"
	at := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	doc := s.rssOf(&store.StatusReport{OrganizationID: "org_1", Organization: "Home", Items: []store.DownItem{
		{Kind: store.DownEndpointOffline, Key: "endpoint_offline:ep1:t", Endpoint: "nas", EndpointID: "ep1", Environment: "prod", Condition: "offline", At: at},
		{Kind: store.DownContainer, Key: "container:unhealthy:ep2:c", Endpoint: "pi", EndpointID: "ep2", Subject: "<script>", Condition: "unhealthy", At: at},
		{Kind: store.DownNodeNotReady, Key: "node_not_ready:ep3:w2", Endpoint: "k3s", EndpointID: "ep3", Subject: "w2", Condition: "not_ready", At: at},
	}}, at)
	if doc.Channel.Description != "3 down." || len(doc.Channel.Items) != 3 {
		t.Fatalf("channel: %+v", doc.Channel)
	}
	want := []string{"Offline: nas", "Unhealthy: <script> on pi", "Node not ready: w2 on k3s"}
	for i, it := range doc.Channel.Items {
		if it.Title != want[i] || it.GUID.IsPermaLink || it.PubDate != "Sat, 31 Oct 2026 08:00:00 +0000" {
			t.Errorf("item %d: %+v", i, it)
		}
	}
	if got := doc.Channel.Items[0]; got.Link != "https://yard.example/organizations/org_1/endpoints/ep1" || got.Description != "Endpoint nas (prod) is offline" {
		t.Errorf("offline item: %+v", got)
	}
	raw, err := xml.Marshal(doc)
	if err != nil || strings.Contains(string(raw), "<script>") {
		t.Fatalf("marshal: %v %s", err, raw)
	}
}
