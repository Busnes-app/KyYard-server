package docker

import (
	"context"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

type cpuPoint struct {
	total, system uint64
	at            time.Time
}

// Stats samples running containers with one-shot reads (no server-side wait) and computes the
// CPU share from the delta against the previous call, kept in memory per container.
func (c *Client) Stats(ctx context.Context, running []string) protocol.Metrics {
	c.cpuMu.Lock()
	defer c.cpuMu.Unlock()
	if c.cpuPrev == nil {
		c.cpuPrev = map[string]cpuPoint{}
	}
	m := protocol.Metrics{ObservedAt: time.Now().UTC(), Samples: []protocol.Sample{}}
	seen := map[string]bool{}
	for _, id := range running {
		if len(m.Samples) >= protocol.MaxSamples {
			break
		}
		var raw struct {
			Read     time.Time `json:"read"`
			CPUStats struct {
				CPUUsage struct {
					Total uint64 `json:"total_usage"`
				} `json:"cpu_usage"`
				System     uint64 `json:"system_cpu_usage"`
				OnlineCPUs uint64 `json:"online_cpus"`
			} `json:"cpu_stats"`
			MemoryStats struct {
				Usage int64 `json:"usage"`
				Limit int64 `json:"limit"`
			} `json:"memory_stats"`
			Networks map[string]struct {
				Rx int64 `json:"rx_bytes"`
				Tx int64 `json:"tx_bytes"`
			} `json:"networks"`
			Pids struct {
				Current int64 `json:"current"`
			} `json:"pids_stats"`
		}
		// One slow container must not spend the whole budget.
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.get(sctx, "/containers/"+id+"/stats?stream=false&one-shot=true", &raw)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			continue // a container that vanished between listing and sampling is not an error
		}
		seen[id] = true
		s := protocol.Sample{ContainerID: id, CPUPercent: -1, MemoryBytes: raw.MemoryStats.Usage, MemoryLimit: raw.MemoryStats.Limit, Pids: raw.Pids.Current}
		for _, n := range raw.Networks {
			s.RxBytes += n.Rx
			s.TxBytes += n.Tx
		}
		now := cpuPoint{total: raw.CPUStats.CPUUsage.Total, system: raw.CPUStats.System, at: raw.Read}
		if prev, ok := c.cpuPrev[id]; ok && now.system > prev.system && now.total >= prev.total {
			cpus := float64(raw.CPUStats.OnlineCPUs)
			if cpus == 0 {
				cpus = 1
			}
			s.CPUPercent = float64(now.total-prev.total) / float64(now.system-prev.system) * cpus * 100
		}
		c.cpuPrev[id] = now
		m.Samples = append(m.Samples, s)
	}
	for id := range c.cpuPrev {
		if !seen[id] {
			delete(c.cpuPrev, id)
		}
	}
	return m
}

// Running lists the IDs of running containers for Stats; it is cheaper than a full snapshot.
func (c *Client) Running(ctx context.Context) ([]string, error) {
	var containers []struct {
		ID string `json:"Id"`
	}
	if err := c.get(ctx, "/containers/json", &containers); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(containers))
	for _, ct := range containers {
		ids = append(ids, ct.ID)
	}
	return ids, nil
}
