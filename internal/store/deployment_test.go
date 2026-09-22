package store

import (
	"context"
	"testing"
)

func TestDeploymentsTableExists(t *testing.T) {
	st, _ := tenantAtomicStore(t)
	var n int
	if err := st.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deployments table: %d %v", n, err)
	}
}
