package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// A settled operation stays settled: a slower read cannot write an older answer over it.
func TestSettledSuiteOperationIsNotReopened(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	op := &store.SuiteOperation{ID: "5a1e5b8e-4b7a-4c55-9a37-2f1c4f0e8b11", ActorID: "usr_a", Kind: "upgrade", Apps: []string{}, State: "running", StartedAt: time.Now().UTC()}
	mustTenant(t, st.Suite().CreateSuiteOperation(ctx, op))
	got, err := st.Suite().ReadSuiteOperation(ctx, op.ID)
	mustTenant(t, err)
	if got.Apps == nil || len(got.Apps) != 0 || got.FinishedAt != nil || got.Snapshot != nil {
		t.Fatalf("read back: %+v", got)
	}
	op.State = "failed"
	mustTenant(t, st.Suite().UpdateSuiteOperation(ctx, op))
	op.State = "running"
	if err := st.Suite().UpdateSuiteOperation(ctx, op); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reopened a settled operation: %v", err)
	}
	if _, err := st.Suite().ReadSuiteOperation(ctx, "5a1e5b8e-0000-4c55-9a37-2f1c4f0e8b11"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing operation: %v", err)
	}
}
