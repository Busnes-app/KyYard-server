package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/permissions"
)

// Retention limits from docs/retention-policy.md (proposed, soak-gated).
const (
	SampleRetention    = 6 * time.Hour
	MaxSamplesPerFrame = protocol.MaxSamples
	EventRetention     = 7 * 24 * time.Hour
	PruneBatch         = 5000
)

// RecordSamples stores one metrics frame. An observation more than five minutes from the
// server clock is stamped with the server's time, so a skewed agent cannot write into the
// future or the past.
func (t *tenancyStore) RecordSamples(ctx context.Context, endpointID string, m protocol.Metrics) error {
	if len(m.Samples) > MaxSamplesPerFrame {
		return ErrInvalid
	}
	now := time.Now().UTC()
	observed := m.ObservedAt.UTC()
	if observed.IsZero() || observed.After(now.Add(5*time.Minute)) || observed.Before(now.Add(-5*time.Minute)) {
		observed = now
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range m.Samples {
		if s.ContainerID == "" || len(s.ContainerID) > 128 || !displaySafe(s.ContainerID) {
			return ErrInvalid
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids) VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT (endpoint_id,container_id,observed_at) DO NOTHING`), endpointID, s.ContainerID, observed, s.CPUPercent, s.MemoryBytes, s.MemoryLimit, s.RxBytes, s.TxBytes, s.Pids); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SampleRow is a stored sample with its time; a missing row is "no data", never zero.
type SampleRow struct {
	ContainerID string    `json:"container_id"`
	ObservedAt  time.Time `json:"observed_at"`
	CPUPercent  float64   `json:"cpu_percent"`
	MemoryBytes int64     `json:"memory_bytes"`
	MemoryLimit int64     `json:"memory_limit"`
	RxBytes     int64     `json:"rx_bytes"`
	TxBytes     int64     `json:"tx_bytes"`
	Pids        int64     `json:"pids"`
}

func scanSamples(rows *sql.Rows) ([]SampleRow, error) {
	out := []SampleRow{}
	for rows.Next() {
		var r SampleRow
		if err := rows.Scan(&r.ContainerID, &r.ObservedAt, &r.CPUPercent, &r.MemoryBytes, &r.MemoryLimit, &r.RxBytes, &r.TxBytes, &r.Pids); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (t *tenancyStore) endpointInScope(ctx context.Context, tx *sql.Tx, a TenantAccess, endpointID string) error {
	var n int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM endpoints WHERE organization_id=? AND id=? AND (?='' OR environment_id=?)`), a.OrganizationID, endpointID, a.EnvironmentID, a.EnvironmentID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// LatestSamples returns the newest sample per container within retention.
func (t *tenancyStore) LatestSamples(ctx context.Context, a TenantAccess, endpointID string) ([]SampleRow, error) {
	var out []SampleRow
	err := t.readTenant(ctx, a, permissions.EndpointRead, func(tx *sql.Tx) error {
		if err := t.endpointInScope(ctx, tx, a, endpointID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT s.container_id,s.observed_at,s.cpu_percent,s.memory_bytes,s.memory_limit,s.rx_bytes,s.tx_bytes,s.pids FROM container_samples s JOIN (SELECT container_id, MAX(observed_at) AS latest FROM container_samples WHERE endpoint_id=? AND observed_at>? GROUP BY container_id) l ON l.container_id=s.container_id AND l.latest=s.observed_at WHERE s.endpoint_id=? ORDER BY s.container_id LIMIT ?`), endpointID, time.Now().UTC().Add(-SampleRetention), endpointID, MaxSamplesPerFrame)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanSamples(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReadSamples returns one container's samples within the window (capped at retention), oldest first.
func (t *tenancyStore) ReadSamples(ctx context.Context, a TenantAccess, endpointID, containerID string, window time.Duration) ([]SampleRow, error) {
	if window <= 0 || window > SampleRetention {
		window = SampleRetention
	}
	var out []SampleRow
	err := t.readTenant(ctx, a, permissions.EndpointRead, func(tx *sql.Tx) error {
		if err := t.endpointInScope(ctx, tx, a, endpointID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids FROM container_samples WHERE endpoint_id=? AND container_id=? AND observed_at>? ORDER BY observed_at LIMIT 400`), endpointID, containerID, time.Now().UTC().Add(-window))
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanSamples(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Prune deletes expired samples and acknowledged events in bounded batches and reports how
// many rows went, so the caller loops until a pass removes nothing.
func (t *tenancyStore) Prune(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	var total int64
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`DELETE FROM container_samples WHERE (endpoint_id,container_id,observed_at) IN (SELECT endpoint_id,container_id,observed_at FROM container_samples WHERE observed_at<? LIMIT ?)`, now.Add(-SampleRetention)},
		{`DELETE FROM endpoint_events WHERE id IN (SELECT id FROM endpoint_events WHERE created_at<? AND acknowledged_at IS NOT NULL LIMIT ?)`, now.Add(-EventRetention)},
	} {
		result, err := t.store.db.ExecContext(ctx, t.store.rebind(q.sql), q.arg, PruneBatch)
		if err != nil {
			return total, err
		}
		n, _ := result.RowsAffected()
		total += n
	}
	return total, nil
}
