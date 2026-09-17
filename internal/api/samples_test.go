package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// Metrics frames land as bounded samples readable only inside the organization.
func TestMetricsFramesAreStoredAndScoped(t *testing.T) {
	s, st, cfg := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	oversized := protocol.Metrics{ObservedAt: time.Now(), Samples: make([]protocol.Sample, protocol.MaxSamples)}
	for i := range oversized.Samples {
		oversized.Samples[i].ContainerID = strings.Repeat("a", 64)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeMetrics, oversized)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeError || !strings.Contains(string(e.Payload), "metrics_too_large") {
		t.Fatalf("oversized metrics not named: %+v", e)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("oversized metrics closed the session: %+v", e)
	}

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
	// At the row ceiling a frame is answered with an error frame and the session continues;
	// a second frame inside the cadence is dropped before the store sees it.
	fillSamples(t, cfg.Database, ag.id, store.MaxSampleRowsPerEndpoint-2)
	waitFor(t, func() bool { return !s.Connected(ag.id) })
	sock, _ = connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeMetrics, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c3"}, {ContainerID: "c4"}, {ContainerID: "c5"}}})
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeError || !strings.Contains(string(e.Payload), "samples_budget_exhausted") {
		t.Fatalf("ceiling not named: %+v", e)
	}
	writeEnvelope(t, ctx, sock.conn, protocol.TypeMetrics, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c6"}}})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("session did not continue at the ceiling: %+v", e)
	}
	w = tenantRequest(s, viewer, "GET", latestPath, "", true)
	_ = json.Unmarshal(w.Body.Bytes(), &latest)
	for _, r := range latest {
		if strings.HasPrefix(r.ContainerID, "c3") || r.ContainerID == "c6" {
			t.Fatalf("frame past the ceiling stored a row: %+v", r)
		}
	}
}

// fillSamples writes rows straight into container_samples so a test can reach the endpoint's
// row ceiling in seconds rather than through a hundred thousand frames.
func fillSamples(t *testing.T, dbCfg config.DatabaseConfig, endpointID string, n int) {
	t.Helper()
	driver, ph := dbCfg.Driver, func(int) string { return "?" }
	if driver == "postgres" {
		driver, ph = "pgx", func(i int) string { return fmt.Sprintf("$%d", i) }
	}
	db, err := sql.Open(driver, dbCfg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-3 * time.Hour)
	const batch = 500
	for done := 0; done < n; done += batch {
		rows := min(batch, n-done)
		var q strings.Builder
		q.WriteString("INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids) VALUES ")
		args := make([]any, 0, 3*rows)
		for i := 0; i < rows; i++ {
			if i > 0 {
				q.WriteString(",")
			}
			fmt.Fprintf(&q, "(%s,%s,%s,0,0,0,0,0,0)", ph(len(args)+1), ph(len(args)+2), ph(len(args)+3))
			args = append(args, endpointID, "fill", old.Add(time.Duration(done+i)*time.Millisecond))
		}
		if _, err := tx.Exec(q.String(), args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
