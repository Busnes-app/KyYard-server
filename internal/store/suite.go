package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// SuiteOperation mirrors one suite runner operation (migration 44). Snapshot is the runner's
// last answer for it, empty until one is read.
type SuiteOperation struct {
	ID         string          `json:"id"`
	ActorID    string          `json:"actor_id"`
	Kind       string          `json:"kind"`
	Apps       []string        `json:"apps"`
	Target     string          `json:"target"`
	State      string          `json:"state"`
	StartedAt  time.Time       `json:"started"`
	FinishedAt *time.Time      `json:"finished"`
	Snapshot   json.RawMessage `json:"operation,omitempty"`
	ErrorCode  string          `json:"error_code"`
}

// SuiteStore is trusted persistence; the API enforces suite.read and suite.manage.
type SuiteStore interface {
	CreateSuiteOperation(ctx context.Context, op *SuiteOperation) error
	ReadSuiteOperation(ctx context.Context, id string) (*SuiteOperation, error)
	// UpdateSuiteOperation writes state, finish, snapshot and error code while the row is still
	// running, so a slow poll cannot reopen a settled row. A settled row is ErrNotFound.
	UpdateSuiteOperation(ctx context.Context, op *SuiteOperation) error
}

type suiteStore struct{ store *SQLStore }

func (s *SQLStore) Suite() SuiteStore { return suiteStore{s} }

func (s suiteStore) CreateSuiteOperation(ctx context.Context, op *SuiteOperation) error {
	apps, err := json.Marshal(op.Apps)
	if err != nil {
		return err
	}
	_, err = s.store.db.ExecContext(ctx, s.store.rebind(`INSERT INTO suite_operations (id, actor_id, kind, apps, target, state, started_at, finished_at, snapshot, error_code) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		op.ID, op.ActorID, op.Kind, string(apps), op.Target, op.State, op.StartedAt, op.FinishedAt, string(op.Snapshot), op.ErrorCode)
	return err
}

func (s suiteStore) ReadSuiteOperation(ctx context.Context, id string) (*SuiteOperation, error) {
	var op SuiteOperation
	var apps, snapshot string
	var finished sql.NullTime
	err := s.store.db.QueryRowContext(ctx, s.store.rebind(`SELECT id, actor_id, kind, apps, target, state, started_at, finished_at, snapshot, error_code FROM suite_operations WHERE id = ?`), id).
		Scan(&op.ID, &op.ActorID, &op.Kind, &apps, &op.Target, &op.State, &op.StartedAt, &finished, &snapshot, &op.ErrorCode)
	if errorsIs(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(apps), &op.Apps); err != nil {
		return nil, err
	}
	if finished.Valid {
		op.FinishedAt = &finished.Time
	}
	if snapshot != "" {
		op.Snapshot = json.RawMessage(snapshot)
	}
	return &op, nil
}

func (s suiteStore) UpdateSuiteOperation(ctx context.Context, op *SuiteOperation) error {
	res, err := s.store.db.ExecContext(ctx, s.store.rebind(`UPDATE suite_operations SET state = ?, finished_at = ?, snapshot = ?, error_code = ? WHERE id = ? AND state = 'running'`),
		op.State, op.FinishedAt, string(op.Snapshot), op.ErrorCode, op.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrNotFound
	}
	return nil
}
