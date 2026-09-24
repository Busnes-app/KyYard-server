package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// ImageCheck is the cached update verdict for one mapped service.
type ImageCheck struct {
	Service      string    `json:"service"`
	Reference    string    `json:"reference"`
	LocalDigest  string    `json:"local_digest"`
	RemoteDigest string    `json:"remote_digest"`
	Verdict      string    `json:"verdict"`
	Detail       string    `json:"detail"`
	CheckedAt    time.Time `json:"checked_at"`
}

// UpdateCheck is an instance's cached checks; InstanceID is empty when nothing is adopted.
type UpdateCheck struct {
	InstanceID     string       `json:"instance_id"`
	MappingVersion int          `json:"mapping_version"`
	Services       []ImageCheck `json:"services"`
}

func (t *tenancyStore) ReadImageChecks(ctx context.Context, a TenantAccess, app string) (*UpdateCheck, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	out := &UpdateCheck{Services: []ImageCheck{}}
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,mapping_version FROM application_instances WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, id.String()).Scan(&out.InstanceID, &out.MappingVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT service_name,reference,local_digest,remote_digest,verdict,detail,checked_at FROM image_checks WHERE instance_id=? ORDER BY service_name`), out.InstanceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c ImageCheck
			if err := rows.Scan(&c.Service, &c.Reference, &c.LocalDigest, &c.RemoteDigest, &c.Verdict, &c.Detail, &c.CheckedAt); err != nil {
				return err
			}
			out.Services = append(out.Services, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (t *tenancyStore) clearImageChecks(ctx context.Context, tx *sql.Tx, instance string) error {
	_, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM image_checks WHERE instance_id=?`), instance)
	return err
}
