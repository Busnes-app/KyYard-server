package api_test

import (
	"encoding/json"
	"net/http/httptest"
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

func TestInventoryRefreshRefusesAnOlderAgent(t *testing.T) {
	f := directFixture(t, directCaps)
	if w := tenantRequest(f.s, f.admin, "POST", "/api/organizations/a/endpoints/"+f.ag.id+"/inventory/refresh", "", true); w.Code != 501 {
		t.Fatalf("old agent: %d %s", w.Code, w.Body.String())
	}
}
