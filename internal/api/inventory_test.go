package api_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

// Inventory arrives over the socket, is stored bounded and generation-ordered, and is read back
// only inside the endpoint's organization with freshness facts. Reads write no audit row.
func TestInventoryIsStoredAndReadWithFreshness(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "b", Name: "B"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a2", OrganizationID: "a", Name: "Other"})
	admin := loginAs(t, s, st, "envadmin", "user")
	viewer := loginAs(t, s, st, "viewer", "user")
	stranger := loginAs(t, s, st, "stranger", "user")
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "usr_stranger", Role: store.RoleOrganizationAdmin, Status: "active"})
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-i")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	inventoryPath := "/api/organizations/a/endpoints/" + ag.id + "/inventory"
	if w := tenantRequest(s, viewer, "GET", inventoryPath, "", true); w.Code != 404 {
		t.Fatalf("inventory before any snapshot: %d", w.Code)
	}
	sock, _ := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	observed := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	snap := protocol.Snapshot{Generation: 5, ObservedAt: observed, Engine: protocol.Engine{Runtime: "docker", Version: "29.7.2"}, Containers: []protocol.Container{{ID: "c1", Name: "web", Image: "nginx:1", State: "running", Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}}}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, snap)
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })
	// A stale generation with different contents must not replace the stored snapshot.
	stale := snap
	stale.Generation = 4
	stale.Containers = nil
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, stale)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeError || !strings.Contains(string(e.Payload), "generation_rejected") {
		t.Fatalf("stale generation was not named: %+v", e)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	readEnvelope(t, ctx, sock.conn)

	w := tenantRequest(s, viewer, "GET", inventoryPath, "", true)
	if w.Code != 200 {
		t.Fatalf("inventory read: %d %s", w.Code, w.Body.String())
	}
	var inv store.Inventory
	if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	var stored protocol.Snapshot
	if err := json.Unmarshal(inv.Snapshot, &stored); err != nil {
		t.Fatal(err)
	}
	if inv.Generation != 5 || inv.State != "active" || !inv.ObservedAt.Equal(observed) || inv.ReceivedAt.IsZero() || len(stored.Containers) != 1 || stored.Containers[0].Name != "web" || stored.Engine.Version != "29.7.2" {
		t.Fatalf("stored inventory: %+v %+v", inv, stored)
	}
	// Scope: another organization and the wrong environment see nothing.
	if w := tenantRequest(s, stranger, "GET", inventoryPath, "", true); w.Code != 403 {
		t.Fatalf("cross-tenant inventory: %d", w.Code)
	}
	if w := tenantRequest(s, viewer, "GET", "/api/organizations/a/environments/env-a2/endpoints", "", true); w.Code != 200 || strings.Contains(w.Body.String(), ag.id) {
		t.Fatalf("endpoint listed under the wrong environment: %d %s", w.Code, w.Body.String())
	}
	// A dirty snapshot is stored as its clamped re-encoding: no undeclared key, no bidi
	// control, no oversized label map.
	bigLabels := map[string]string{}
	for i := 0; i < protocol.MaxLabels*2; i++ {
		bigLabels[strings.Repeat("k", i+1)] = "v"
	}
	dirty, _ := json.Marshal(map[string]any{"generation": 6, "observed_at": observed, "undeclared": "env=SECRET", "engine": map[string]any{"runtime": "docker"}, "containers": []any{map[string]any{"id": "c9", "name": "web\u202eevil", "env": []string{"SECRET=1"}, "labels": bigLabels, "ports": []any{}, "networks": []any{}}}, "images": []any{}, "networks": []any{}, "volumes": []any{}})
	frame, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: protocol.TypeInventory, Payload: dirty})
	if err := sock.conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	readEnvelope(t, ctx, sock.conn)
	w = tenantRequest(s, viewer, "GET", inventoryPath, "", true)
	_ = json.Unmarshal(w.Body.Bytes(), &inv)
	rawStored := string(inv.Snapshot)
	if inv.Generation != 6 || strings.Contains(rawStored, "undeclared") || strings.Contains(rawStored, "SECRET") || strings.Contains(rawStored, "\u202e") || strings.Contains(rawStored, "‮") {
		t.Fatalf("dirty snapshot stored verbatim: %s", rawStored[:200])
	}
	_ = json.Unmarshal(inv.Snapshot, &stored)
	if len(stored.Containers) != 1 || stored.Containers[0].Name != "webevil" || len(stored.Containers[0].Labels) != protocol.MaxLabels {
		t.Fatalf("clamp not applied: %+v", stored.Containers)
	}
	// An oversized frame gets an error frame and the session continues.
	huge := snap
	huge.Generation = 7
	huge.Containers = nil
	for i := 0; i < 6000; i++ {
		huge.Containers = append(huge.Containers, protocol.Container{ID: strings.Repeat("x", 64), Name: strings.Repeat("n", 100), Status: strings.Repeat("s", 40), Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}})
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, huge)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeError || !strings.Contains(string(e.Payload), "snapshot_too_large") {
		t.Fatalf("oversized snapshot: %+v", e)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
		t.Fatal("session did not continue after an oversized snapshot")
	}
	w = tenantRequest(s, viewer, "GET", inventoryPath, "", true)
	_ = json.Unmarshal(w.Body.Bytes(), &inv)
	if inv.Generation != 6 {
		t.Fatalf("oversized snapshot changed the stored generation: %d", inv.Generation)
	}
	// Successful inventory reads are not audited, but the sensitive low-volume reads are:
	// one audit-trail read and one member enumeration each leave exactly one row.
	orgAdmin := loginAs(t, s, st, "orgadmin", "user")
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_orgadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	if w := tenantRequest(s, orgAdmin, "GET", "/api/organizations/a/audit", "", true); w.Code != 200 {
		t.Fatalf("audit read: %d", w.Code)
	}
	if w := tenantRequest(s, orgAdmin, "GET", "/api/organizations/a/members", "", true); w.Code != 200 {
		t.Fatalf("members read: %d", w.Code)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, r := range records {
		if r.Result == "success" {
			counts[r.Action]++
		}
	}
	if counts["endpoint.read"] != 0 || counts["organization.audit.read"] != 1 || counts["organization.members.manage"] != 1 || counts["endpoint.enroll"] == 0 {
		t.Fatalf("read audit: %v", counts)
	}
	_ = websocket.StatusNormalClosure
}
