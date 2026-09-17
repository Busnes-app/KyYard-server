package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
)

// Retention limits from docs/retention-policy.md (proposed, soak-gated).
const (
	SampleRetention    = 6 * time.Hour
	MaxSamplesPerFrame = protocol.MaxSamples
	// SampleCadence is the documented 60 s reporting interval; a container gains at most one
	// row per cadence (with a little slack for jitter), so the six-hour window holds about
	// 360 rows per container whatever an agent sends.
	SampleCadence  = 50 * time.Second
	EventRetention = 7 * 24 * time.Hour
	PruneBatch     = 5000
)

// MaxSampleRowsPerEndpoint is the backstop against container-ID cardinality: 100 containers ×
// 360 rows at the capacity targets, with headroom.
const MaxSampleRowsPerEndpoint = 100000

var ErrSampleBudget = errors.New("endpoint sample budget exhausted")

func ValidateSamples(m protocol.Metrics) error {
	if len(m.Samples) > MaxSamplesPerFrame {
		return ErrInvalid
	}
	for _, s := range m.Samples {
		if s.ContainerID == "" || len(s.ContainerID) > 128 || !displaySafe(s.ContainerID) {
			return ErrInvalid
		}
	}
	return nil
}

// RecordSamples stores one metrics frame. An observation more than five minutes from the
// server clock is stamped with the server's time, so a skewed agent cannot write into the
// future or the past. Samples inside a container's cadence are dropped, and a frame that
// would push the endpoint past its row ceiling, or carries a bad ID, is refused with
// ErrInvalid, which the connection treats as a protocol violation.
func (t *tenancyStore) RecordSamples(ctx context.Context, endpointID string, m protocol.Metrics) error {
	if err := ValidateSamples(m); err != nil {
		return err
	}
	if len(m.Samples) > MaxSamplesPerFrame {
		return ErrInvalid
	}
	now := time.Now().UTC()
	observed := m.ObservedAt.UTC()
	if observed.IsZero() || observed.After(now.Add(5*time.Minute)) || observed.Before(now.Add(-5*time.Minute)) {
		observed = now
	}
	for _, s := range m.Samples {
		if s.ContainerID == "" || len(s.ContainerID) > 128 || !displaySafe(s.ContainerID) {
			return ErrInvalid
		}
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	newest := map[string]time.Time{}
	for _, s := range m.Samples {
		if _, seen := newest[s.ContainerID]; seen {
			continue
		}
		var raw any
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT MAX(observed_at) FROM container_samples WHERE endpoint_id=? AND container_id=?`), endpointID, s.ContainerID).Scan(&raw); err != nil {
			return err
		}
		if raw != nil {
			at, err := scanTime(raw)
			if err != nil {
				return err
			}
			newest[s.ContainerID] = at
		}
	}
	writes := make([]protocol.Sample, 0, len(m.Samples))
	for _, s := range m.Samples {
		if last, ok := newest[s.ContainerID]; ok && observed.Sub(last) < SampleCadence {
			continue
		}
		writes = append(writes, s)
		newest[s.ContainerID] = observed
	}
	if len(writes) == 0 {
		return tx.Commit()
	}
	offset := t.store.ceiling - len(writes)
	if offset < 0 {
		return ErrSampleBudget
	}
	var present int
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT 1 FROM container_samples WHERE endpoint_id=? LIMIT 1 OFFSET ?`), endpointID, offset).Scan(&present)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && present == 1 {
		return ErrSampleBudget
	}
	for _, s := range writes {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO container_samples (endpoint_id,container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids,restart_count) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT (endpoint_id,container_id,observed_at) DO NOTHING`), endpointID, s.ContainerID, observed, s.CPUPercent, s.MemoryBytes, s.MemoryLimit, s.RxBytes, s.TxBytes, s.Pids, s.RestartCount); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SampleRow is a stored sample with its time; a missing row is "no data", never zero.
type SampleRow struct {
	ContainerID  string    `json:"container_id"`
	ObservedAt   time.Time `json:"observed_at"`
	CPUPercent   float64   `json:"cpu_percent"`
	MemoryBytes  int64     `json:"memory_bytes"`
	MemoryLimit  int64     `json:"memory_limit"`
	RxBytes      int64     `json:"rx_bytes"`
	TxBytes      int64     `json:"tx_bytes"`
	Pids         int64     `json:"pids"`
	RestartCount int64     `json:"restart_count"`
}

func scanSamples(rows *sql.Rows) ([]SampleRow, error) {
	out := []SampleRow{}
	for rows.Next() {
		var r SampleRow
		if err := rows.Scan(&r.ContainerID, &r.ObservedAt, &r.CPUPercent, &r.MemoryBytes, &r.MemoryLimit, &r.RxBytes, &r.TxBytes, &r.Pids, &r.RestartCount); err != nil {
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
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT s.container_id,s.observed_at,s.cpu_percent,s.memory_bytes,s.memory_limit,s.rx_bytes,s.tx_bytes,s.pids,s.restart_count FROM container_samples s WHERE s.endpoint_id=? AND s.observed_at>? AND NOT EXISTS (SELECT 1 FROM container_samples newer WHERE newer.endpoint_id=s.endpoint_id AND newer.container_id=s.container_id AND newer.observed_at>s.observed_at) ORDER BY s.container_id LIMIT ?`), endpointID, time.Now().UTC().Add(-SampleRetention), MaxSamplesPerFrame)
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
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,observed_at,cpu_percent,memory_bytes,memory_limit,rx_bytes,tx_bytes,pids,restart_count FROM container_samples WHERE endpoint_id=? AND container_id=? AND observed_at>? ORDER BY observed_at LIMIT 400`), endpointID, containerID, time.Now().UTC().Add(-window))
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

// scanTime reads a timestamp that came through an aggregate: PostgreSQL keeps the type,
// SQLite hands back the stored text.
func scanTime(v any) (time.Time, error) {
	switch x := v.(type) {
	case time.Time:
		return x.UTC(), nil
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, x); err == nil {
				return t.UTC(), nil
			}
		}
		return time.Time{}, fmt.Errorf("unparsable timestamp %q", x)
	case []byte:
		return scanTime(string(x))
	default:
		return time.Time{}, fmt.Errorf("unexpected timestamp type %T", v)
	}
}
