package api_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/store"
)

// Metrics frames land as bounded samples readable only inside the organization.
func TestMetricsFramesAreStoredAndScoped(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "b", Name: "B"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	admin := loginAs(t, s, st, "envadmin", "user")
	viewer := loginAs(t, s, st, "viewer", "user")
	stranger := loginAs(t, s, st, "stranger", "user")
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "b", UserID: "usr_stranger", Role: store.RoleOrganizationAdmin, Status: "active"})
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-m")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	sock, _ := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{Generation: 1, Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeMetrics, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c1", CPUPercent: 42, MemoryBytes: 512, MemoryLimit: 1024, RxBytes: 1, TxBytes: 2, Pids: 3}, {ContainerID: "c2", CPUPercent: -1}}})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	readEnvelope(t, ctx, sock.conn)

	latestPath := "/api/organizations/a/endpoints/" + ag.id + "/samples"
	w := tenantRequest(s, viewer, "GET", latestPath, "", true)
	if w.Code != 200 {
		t.Fatalf("latest: %d %s", w.Code, w.Body.String())
	}
	var latest []store.SampleRow
	if err := json.Unmarshal(w.Body.Bytes(), &latest); err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 || latest[0].ContainerID != "c1" || latest[0].CPUPercent != 42 || latest[1].CPUPercent != -1 {
		t.Fatalf("latest samples: %+v", latest)
	}
	w = tenantRequest(s, viewer, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/c1/samples?minutes=30", "", true)
	if w.Code != 200 {
		t.Fatalf("series: %d", w.Code)
	}
	var series []store.SampleRow
	_ = json.Unmarshal(w.Body.Bytes(), &series)
	if len(series) != 1 || series[0].MemoryBytes != 512 {
		t.Fatalf("series: %+v", series)
	}
	if w := tenantRequest(s, stranger, "GET", latestPath, "", true); w.Code != 403 {
		t.Fatalf("cross-tenant samples: %d", w.Code)
	}
	if w := tenantRequest(s, viewer, "GET", "/api/organizations/a/endpoints/ep_missing/samples", "", true); w.Code != 404 {
		t.Fatalf("unknown endpoint samples: %d", w.Code)
	}
	// A frame the store refuses (here: an unsafe container ID) closes the socket, and nothing
	// of it is stored.
	writeEnvelope(t, ctx, sock.conn, protocol.TypeMetrics, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "bad\nid"}}})
	if _, _, err := sock.conn.Read(ctx); err == nil {
		t.Fatal("refused metrics frame left the socket open")
	}
	w = tenantRequest(s, viewer, "GET", latestPath, "", true)
	_ = json.Unmarshal(w.Body.Bytes(), &latest)
	if len(latest) != 2 {
		t.Fatalf("refused frame was stored: %d", len(latest))
	}
}
