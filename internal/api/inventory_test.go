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
	// An oversized snapshot closes the socket rather than being stored.
	huge := snap
	huge.Generation = 6
	huge.Containers = nil
	for i := 0; i < 6000; i++ {
		huge.Containers = append(huge.Containers, protocol.Container{ID: strings.Repeat("x", 64), Name: strings.Repeat("n", 100), Status: strings.Repeat("s", 40), Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}})
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, huge)
	if _, _, err := sock.conn.Read(ctx); err == nil {
		t.Fatal("oversized snapshot was accepted")
	}
	w = tenantRequest(s, viewer, "GET", inventoryPath, "", true)
	_ = json.Unmarshal(w.Body.Bytes(), &inv)
	if inv.Generation != 5 {
		t.Fatalf("oversized snapshot changed the stored generation: %d", inv.Generation)
	}
	// Successful reads are not audited (non-members never produce rows, so the cross-tenant
	// probe above leaves nothing either); the mutation trail is untouched.
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var readOK, approvals int
	for _, r := range records {
		if r.Action == "endpoint.read" && r.Result == "success" {
			readOK++
		}
		if r.Action == "endpoint.enroll" && r.Result == "success" {
			approvals++
		}
	}
	if readOK != 0 || approvals == 0 {
		t.Fatalf("read audit: reads=%d approvals=%d", readOK, approvals)
	}
	_ = websocket.StatusNormalClosure
}
