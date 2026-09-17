package docker_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		if strings.HasSuffix(r.URL.Path, "/json") {
			// Container inspect, where the restart counter lives; the stats endpoint has none.
			_, _ = w.Write([]byte(`{"RestartCount":4,"State":{"RestartCount":4}}`))
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
	if first.Samples[0].RestartCount != 4 {
		t.Fatalf("restart count: %+v", first.Samples)
	}
	second := c.Stats(context.Background(), running)
	// delta total 1e9 over delta system 2e10 on 4 cpus = 20 %
	if len(second.Samples) != 1 || second.Samples[0].CPUPercent < 19.9 || second.Samples[0].CPUPercent > 20.1 {
		t.Fatalf("cpu delta: %+v", second.Samples)
	}
}

// A runtime that will not answer the inspect must read as "no data", never as a container that
// has never restarted: the difference is what tells an operator a host is crash-looping.
func TestRestartCountIsMinusOneWhenTheRuntimeWillNotSay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"c1"}]`))
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.WriteHeader(500)
		default:
			_, _ = w.Write([]byte(`{"read":"2026-09-16T12:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":1},"system_cpu_usage":2,"online_cpus":1},"memory_stats":{"usage":8,"limit":16},"pids_stats":{"current":1}}`))
		}
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	m := c.Stats(context.Background(), []string{"c1"})
	if len(m.Samples) != 1 || m.Samples[0].RestartCount != -1 {
		t.Fatalf("expected an unknown restart count, got %+v", m.Samples)
	}
}

// A daemon that accepts connections and then answers slowly must cost a fixed slice per
// container, not one slice per request: the two calls share the budget, or the tail of the
// list is starved the same way every cycle.
func TestOneSlowContainerSpendsOneBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			_, _ = w.Write([]byte(`[{"Id":"slow"}]`))
			return
		}
		select {
		case <-time.After(4 * time.Second):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"read":"2026-09-16T12:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":1},"system_cpu_usage":2,"online_cpus":1},"memory_stats":{"usage":8,"limit":16},"pids_stats":{"current":1},"RestartCount":2}`))
	}))
	defer srv.Close()
	c := docker.NewHTTP(srv.Client(), srv.URL)
	started := time.Now()
	m := c.Stats(context.Background(), []string{"slow"})
	if took := time.Since(started); took > 6*time.Second {
		t.Fatalf("two calls took %s, so each opened its own budget", took)
	}
	// The stats call answered inside the budget; the inspect that followed did not, and an
	// unanswered counter is unknown rather than zero.
	if len(m.Samples) != 1 || m.Samples[0].RestartCount != -1 {
		t.Fatalf("expected one sample with an unknown restart count, got %+v", m.Samples)
	}
}
