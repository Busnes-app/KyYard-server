package docker_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
)

// CPU share is a delta against the previous call; the first call has no interval and says so.
func TestStatsComputesCPUFromDeltas(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			_, _ = w.Write([]byte(`[{"Id":"c1"},{"Id":"gone"}]`))
			return
		}
		if strings.Contains(r.URL.Path, "/containers/gone/") {
			w.WriteHeader(404)
			return
		}
		calls++
		total := 1000000000 * calls // one full core-second more per call
		system := 20000000000 * calls
		fmt.Fprintf(w, `{"read":"2026-09-16T12:00:0%dZ","cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":4},"memory_stats":{"usage":1024,"limit":4096},"networks":{"eth0":{"rx_bytes":10,"tx_bytes":20},"eth1":{"rx_bytes":1,"tx_bytes":2}},"pids_stats":{"current":3}}`, calls, total, system)
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	running, err := c.Running(context.Background())
	if err != nil || len(running) != 2 {
		t.Fatalf("running: %v %v", running, err)
	}
	first := c.Stats(context.Background(), running)
	if len(first.Samples) != 1 || first.Samples[0].CPUPercent != -1 || first.Samples[0].MemoryBytes != 1024 || first.Samples[0].RxBytes != 11 || first.Samples[0].TxBytes != 22 || first.Samples[0].Pids != 3 {
		t.Fatalf("first sample: %+v", first.Samples)
	}
	second := c.Stats(context.Background(), running)
	// delta total 1e9 over delta system 2e10 on 4 cpus = 20 %
	if len(second.Samples) != 1 || second.Samples[0].CPUPercent < 19.9 || second.Samples[0].CPUPercent > 20.1 {
		t.Fatalf("cpu delta: %+v", second.Samples)
	}
}
