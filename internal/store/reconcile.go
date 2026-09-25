package store

import (
	"context"
	"fmt"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

const reconcileDetail = "the server restarted before a result arrived"

// ReconcileAfterStart settles every command still in flight as unknown: the process that
// dispatched it is gone, so no answer can arrive. Returns the number of commands settled.
// An empty outcome is the only condition, so a first real answer always wins and a partially
// written row still settles. Deployments are left to their own sweep. Policy runs the previous
// process left open fail with `the server restarted during the run` and count against their
// policy, in the same transaction; the count returned is commands only. Validations still in
// grace past their window become `unverifiable` (`the server was not running during the window`),
// and a named, undecided rollback is decided by the live loop's rule (decideNamedRollbacks),
// pausing its policy, in the same transaction.
func (t *tenancyStore) ReconcileAfterStart(ctx context.Context) (int64, error) {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	type endpoint struct{ id, org, env string }
	var inFlight []endpoint
	rows, err := tx.QueryContext(ctx, `SELECT endpoint_id,organization_id,environment_id FROM endpoint_commands WHERE outcome='' GROUP BY endpoint_id,organization_id,environment_id`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var e endpoint
		if err := rows.Scan(&e.id, &e.org, &e.env); err != nil {
			rows.Close()
			return 0, err
		}
		inFlight = append(inFlight, e)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var total int64
	for _, e := range inFlight {
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_commands SET outcome=?, detail=?, settled_at=? WHERE endpoint_id=? AND organization_id=? AND environment_id=? AND outcome=''`),
			protocol.OutcomeUnknown, reconcileDetail, now, e.id, e.org, e.env)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`),
			"system", "endpoint.commands.reconciled", e.id, fmt.Sprintf("commands=%d", n), now, "organization", e.org, e.env, uuid.NewString(), "unknown"); err != nil {
			return 0, err
		}
		total += n
	}
	if err := t.reconcilePolicyRuns(ctx, tx); err != nil {
		return 0, err
	}
	if err := t.reconcileValidations(ctx, tx); err != nil {
		return 0, err
	}
	return total, tx.Commit()
}
