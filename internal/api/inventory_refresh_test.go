package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestInventoryRefreshIsScopedAndWaitsForANewerReport(t *testing.T) {
	f := directFixture(t, []string{protocol.CapabilityInventoryRefresh})
	path := "/api/organizations/a/endpoints/" + f.ag.id + "/inventory/refresh"
	if w := tenantRequest(f.s, f.admin, "POST", path, "", false); w.Code != 403 {
		t.Fatalf("missing CSRF: %d", w.Code)
	}
	if w := tenantRequest(f.s, f.admin, "POST", "/api/organizations/b/endpoints/"+f.ag.id+"/inventory/refresh", "", true); w.Code != 403 {
		t.Fatalf("wrong scope: %d", w.Code)
	}
	before, err := f.st.Tenancy().ReadInventory(f.ctx, store.TenantAccess{ActorID: "usr_execadmin", OrganizationID: "a"}, f.ag.id)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- tenantRequest(f.s, f.admin, "POST", path, "", true) }()
	frame := readEnvelope(t, f.ctx, f.ag.conn)
	if frame.Type != protocol.TypeInventoryRefresh {
		t.Fatalf("refresh frame: %+v", frame)
	}
	select {
	case <-done:
		t.Fatal("refresh returned cached inventory")
	case <-time.After(300 * time.Millisecond):
	}
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInventory, protocol.Snapshot{Generation: before.Generation + 1, ObservedAt: time.Now(), Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}})
	select {
	case w := <-done:
		var inv store.Inventory
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &inv) != nil || inv.Generation != before.Generation+1 {
			t.Fatalf("fresh inventory: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new report did not complete refresh")
	}
}

// Refusals spend no refresh budget: the seventh request to an older agent is still told why.
func TestInventoryRefreshRefusesAnOlderAgent(t *testing.T) {
	f := directFixture(t, directCaps)
	for i := range 8 {
		if w := tenantRequest(f.s, f.admin, "POST", "/api/organizations/a/endpoints/"+f.ag.id+"/inventory/refresh", "", true); w.Code != 501 {
			t.Fatalf("old agent, request %d: %d %s", i+1, w.Code, w.Body.String())
		}
	}
}

func TestServiceTokenCannotRefreshInventory(t *testing.T) {
	srv, st, cookie, csrf, org := tenantAdminFixture(t)
	token := pairAndClaim(t, srv, cookie, csrf, org)
	if w := bearer(srv, "POST", "/api/organizations/"+org+"/endpoints/ep_none/inventory/refresh", token); w.Code != http.StatusForbidden {
		t.Fatalf("service refresh: %d %s", w.Code, w.Body)
	}
	rows, _, _ := st.Audit().ListAuditRecords(context.Background(), 0, 50)
	for _, r := range rows {
		if r.Details == "inventory.refresh" && r.Result == "denied" && strings.HasPrefix(r.UserID, "service:") {
			return
		}
	}
	t.Fatal("service refresh refusal must be audited")
}
