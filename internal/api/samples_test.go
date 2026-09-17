package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// The subject is the behaviour at the ceiling, not its height: a hundred thousand rows
	// costs minutes against PostgreSQL and proves nothing a hundred do not.
	s, st, cfg := setupTestServerWith(t, func(c *config.Config) { c.Database.SampleCeiling = 100 })
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
	writeEnvelope(t, ctx, sock.conn, protocol.TypeMetrics, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c1", CPUPercent: 42, MemoryBytes: 512, MemoryLimit: 1024, RxBytes: 1, TxBytes: 2, Pids: 3, RestartCount: 7}, {ContainerID: "c2", CPUPercent: -1, RestartCount: -1}}})
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
	if len(latest) != 2 || latest[0].ContainerID != "c1" || latest[0].CPUPercent != 42 || latest[1].CPUPercent != -1 || latest[0].RestartCount != 7 || latest[1].RestartCount != -1 {
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
	fillSamples(t, cfg.Database, ag.id, st.SampleCeiling()-2)
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
// row ceiling in seconds rather than through a hundred thousand frames. One prepared statement
// inside one transaction: batching into multi-row statements is an order of magnitude slower
// against PostgreSQL, which re-plans each distinct statement shape.
func fillSamples(t *testing.T, dbCfg config.DatabaseConfig, endpointID string, n int) {
	t.Helper()
	driver, q := dbCfg.Driver, `INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids) VALUES (?,?,?,0,0,0,0,0,0)`
	if driver == "postgres" {
		driver, q = "pgx", `INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids) VALUES ($1,$2,$3,0,0,0,0,0,0)`
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
	stmt, err := tx.Prepare(q)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-3 * time.Hour)
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(endpointID, "fill", old.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// When storage runs short the fleet must stay visible: metrics go first, inventory only if
// that was not enough, and heartbeats, endpoint state and the audit trail never.
func TestTelemetryBacksOffUnderRetentionPressure(t *testing.T) {
	s, st, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()
	ag := enrollAgent(t, s, st, admin, "host-p")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	sock, _ := connect(t, ctx, httpSrv.URL, ag, ag.priv, protocol.Version)
	empty := protocol.Snapshot{Engine: protocol.Engine{Runtime: "docker"}, Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}

	// Only the prune loop moves this level in production, so the test reaches past the
	// interface rather than the interface offering a way to disarm the budget.
	// Degraded: metrics are refused by name, inventory still lands.
	st.(*store.SQLStore).SetPressure(store.PressureDegraded)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeMetrics, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c1"}}})
	e := readEnvelope(t, ctx, sock.conn)
	if e.Type != protocol.TypeError || !strings.Contains(string(e.Payload), "retention_pressure") || !strings.Contains(string(e.Payload), "degraded") {
		t.Fatalf("metrics under degraded pressure: %+v", e)
	}
	first := empty
	first.Generation = uint64(time.Now().Unix())
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, first)
	waitFor(t, func() bool { e, _ := ts.ReadEndpointRaw(ctx, ag.id); return e != nil && e.State == "active" })

	// Stopped: inventory is refused too, and the stored snapshot does not move.
	st.(*store.SQLStore).SetPressure(store.PressureStopped)
	later := empty
	later.Generation = first.Generation + 1
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, later)
	e = readEnvelope(t, ctx, sock.conn)
	if e.Type != protocol.TypeError || !strings.Contains(string(e.Payload), "stopped") {
		t.Fatalf("inventory under stopped pressure: %+v", e)
	}
	w := tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/inventory", "", true)
	var inv struct{ Generation uint64 }
	_ = json.Unmarshal(w.Body.Bytes(), &inv)
	if inv.Generation != first.Generation {
		t.Fatalf("stopped pressure stored a snapshot: %d", inv.Generation)
	}

	// The session and its heartbeats outlive both, and the audit trail is untouched.
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("pressure ended the session: %+v", e)
	}
	records, err := ts.ReadAudit(ctx, store.TenantAccess{ActorID: "usr_envadmin", OrganizationID: "a"}, 0, 50)
	if err != nil || len(records) == 0 {
		t.Fatalf("audit refused under pressure: %d rows, %v", len(records), err)
	}
	st.(*store.SQLStore).SetPressure(store.PressureNormal)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeMetrics, protocol.Metrics{ObservedAt: time.Now(), Samples: []protocol.Sample{{ContainerID: "c1", RestartCount: 1}}})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("metrics were refused after the pressure cleared: %+v", e)
	}
	w = tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/samples", "", true)
	var latest []store.SampleRow
	_ = json.Unmarshal(w.Body.Bytes(), &latest)
	if len(latest) != 1 || latest[0].ContainerID != "c1" {
		t.Fatalf("samples did not resume: %+v", latest)
	}
}

// The hourly summary is a separate route from the raw samples, so a caller knows which
// resolution it asked for, and it is scoped exactly as the raw one is.
func TestContainerRollupsAreServedAndScoped(t *testing.T) {
	s, st, cfg := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	ag := enrollAgent(t, s, st, admin, "host-r")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatal("approve")
	}
	// The agent path re-stamps an observation this old, by design, so the sample for an hour
	// that has already ended is seeded directly.
	past := time.Now().UTC().Add(-2 * time.Hour)
	seedSample(t, cfg.Database, ag.id, "c1", past, 40, 900, 6)
	if _, err := ts.RollUp(ctx); err != nil {
		t.Fatal(err)
	}

	path := "/api/organizations/a/endpoints/" + ag.id + "/containers/c1/rollups?hours=48"
	w := tenantRequest(s, viewer, "GET", path, "", true)
	if w.Code != 200 {
		t.Fatalf("rollups: %d %s", w.Code, w.Body.String())
	}
	var hours []store.RollupRow
	if err := json.Unmarshal(w.Body.Bytes(), &hours); err != nil {
		t.Fatal(err)
	}
	if len(hours) != 1 || hours[0].CPUPeak != 40 || hours[0].RestartCount != 6 || hours[0].Samples != 1 {
		t.Fatalf("summary served: %+v", hours)
	}
	if !hours[0].Hour.Equal(past.Truncate(time.Hour)) {
		t.Fatalf("hour bucket %v does not match the sample at %v", hours[0].Hour, past)
	}
	if w := tenantRequest(s, stranger, "GET", path, "", true); w.Code != 403 {
		t.Fatalf("cross-tenant rollups: %d", w.Code)
	}
	if w := tenantRequest(s, viewer, "GET", "/api/organizations/a/endpoints/ep_missing/containers/c1/rollups", "", true); w.Code != 404 {
		t.Fatalf("unknown endpoint rollups: %d", w.Code)
	}
	// A window past retention is clamped rather than refused, like the raw route.
	if w := tenantRequest(s, viewer, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/c1/rollups?hours=100000", "", true); w.Code != 200 {
		t.Fatalf("oversized window: %d", w.Code)
	}
}

// seedSample writes one sample at a chosen time, which the agent path will not do: it
// re-stamps observations far from the server clock.
func seedSample(t *testing.T, dbCfg config.DatabaseConfig, endpointID, container string, at time.Time, cpu float64, mem, restarts int64) {
	t.Helper()
	driver, q := dbCfg.Driver, `INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids,restart_count) VALUES (?,?,?,?,?,0,0,0,0,?)`
	if driver == "postgres" {
		driver, q = "pgx", `INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids,restart_count) VALUES ($1,$2,$3,$4,$5,0,0,0,0,$6)`
	}
	db, err := sql.Open(driver, dbCfg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, endpointID, container, at, cpu, mem, restarts); err != nil {
		t.Fatal(err)
	}
}
