package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

type Application struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	EnvironmentID  string    `json:"environment_id"`
	Name           string    `json:"name"`
	LatestRevision int       `json:"latest_revision"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	// RemovedAt is set once a removal took its containers off the host; revisions and history
	// stay until discard or ApplicationRemovedRetention.
	RemovedAt *time.Time `json:"removed_at"`
}
type ApplicationRevision struct {
	ID             string          `json:"id"`
	ApplicationID  string          `json:"application_id"`
	OrganizationID string          `json:"organization_id"`
	EnvironmentID  string          `json:"environment_id"`
	Number         int             `json:"number"`
	Spec           ApplicationSpec `json:"spec"`
	Digest         string          `json:"digest"`
	CreatedBy      string          `json:"created_by"`
	CreatedAt      time.Time       `json:"created_at"`
}

// CreateApplication commits the application, first revision and permission audit
// atomically. It creates desired state only, not an adopted or deployed instance.
func (t *tenancyStore) CreateApplication(ctx context.Context, a TenantAccess, name string, spec ApplicationSpec) (*Application, error) {
	return t.createApplication(ctx, a, name, spec, nil, nil)
}

func (t *tenancyStore) createApplication(ctx context.Context, a TenantAccess, name string, spec ApplicationSpec, values map[string]string, key []byte) (*Application, error) {
	app := Application{ID: uuid.NewString(), OrganizationID: a.OrganizationID, EnvironmentID: a.EnvironmentID, Name: strings.TrimSpace(name), LatestRevision: 1, CreatedBy: a.ActorID, CreatedAt: time.Now().UTC()}
	err := t.withTenantTarget(ctx, a, permissions.ApplicationImport, app.ID, func(tx *sql.Tx) error {
		return t.insertApplication(ctx, tx, a, app, spec, values, key)
	})
	if err != nil {
		return nil, err
	}
	return &app, nil
}

// insertApplication writes app with revision 1 = spec and, when values is non-nil, its sealed
// values, inside the caller's authorized transaction, under the organization's quota.
func (t *tenancyStore) insertApplication(ctx context.Context, tx *sql.Tx, a TenantAccess, app Application, spec ApplicationSpec, values map[string]string, key []byte) error {
	if a.EnvironmentID == "" || !validTenantName(app.Name) {
		return ErrInvalid
	}
	raw, digest, err := encodeApplicationSpec(spec)
	if err != nil {
		return err
	}
	// Serialize the organization-wide quota across distinct administrators.
	if _, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE organizations SET name=name WHERE id=?`), a.OrganizationID); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM applications WHERE organization_id=?`), a.OrganizationID).Scan(&count); err != nil {
		return err
	}
	if count >= MaxApplicationsPerOrganization {
		return ErrApplicationLimit
	}
	_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO applications(id,organization_id,environment_id,name,latest_revision,created_by,created_at) VALUES(?,?,?,?,?,?,?)`), app.ID, app.OrganizationID, app.EnvironmentID, app.Name, app.LatestRevision, app.CreatedBy, app.CreatedAt)
	if err != nil {
		return err
	}
	if err := t.insertApplicationRevision(ctx, tx, a, app.ID, 1, raw, digest, app.CreatedAt); err != nil {
		return err
	}
	if values != nil {
		return t.sealApplicationValues(ctx, tx, a, app.ID, 1, spec, digest, values, key)
	}
	return nil
}
func (t *tenancyStore) insertApplicationRevision(ctx context.Context, tx *sql.Tx, a TenantAccess, id string, number int, raw []byte, digest string, at time.Time) error {
	_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_revisions(id,application_id,organization_id,environment_id,number,spec,digest,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`), uuid.NewString(), id, a.OrganizationID, a.EnvironmentID, number, string(raw), digest, a.ActorID, at)
	return err
}

// AppendApplicationRevision rejects edits based on an old head. The conditional
// update serializes writers even when their membership locks are different rows.
func (t *tenancyStore) AppendApplicationRevision(ctx context.Context, a TenantAccess, id string, expected int, spec ApplicationSpec) (int, error) {
	return t.appendApplicationRevision(ctx, a, id, expected, spec, nil, nil)
}

func (t *tenancyStore) appendApplicationRevision(ctx context.Context, a TenantAccess, id string, expected int, spec ApplicationSpec, values map[string]string, key []byte) (int, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return 0, ErrInvalid
	}
	id = parsed.String()
	next := expected + 1
	err = t.withTenantTarget(ctx, a, permissions.ApplicationEdit, id+"/revisions/"+strconv.Itoa(next), func(tx *sql.Tx) error {
		if a.EnvironmentID == "" || expected < 1 || expected > MaxApplicationRevisions {
			return ErrInvalid
		}
		raw, digest, err := encodeApplicationSpec(spec)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE applications SET latest_revision=latest_revision+1 WHERE organization_id=? AND environment_id=? AND id=? AND latest_revision=? AND latest_revision<?`), a.OrganizationID, a.EnvironmentID, id, expected, MaxApplicationRevisions)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			var current int
			err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, id).Scan(&current)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if current >= MaxApplicationRevisions {
				return ErrApplicationLimit
			}
			return ErrRevisionConflict
		}
		if err := t.insertApplicationRevision(ctx, tx, a, id, next, raw, digest, time.Now().UTC()); err != nil {
			return err
		}
		if values != nil {
			return t.sealApplicationValues(ctx, tx, a, id, next, spec, digest, values, key)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return next, nil
}
func (t *tenancyStore) ListApplications(ctx context.Context, a TenantAccess, offset, limit int) ([]Application, error) {
	out := []Application{}
	err := t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		if a.EnvironmentID == "" || offset < 0 || limit < 1 || limit > 200 {
			return ErrInvalid
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,organization_id,environment_id,name,latest_revision,created_by,created_at,removed_at FROM applications WHERE organization_id=? AND environment_id=? ORDER BY name,id LIMIT ? OFFSET ?`), a.OrganizationID, a.EnvironmentID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var app Application
			var removed sql.NullTime
			if err = rows.Scan(&app.ID, &app.OrganizationID, &app.EnvironmentID, &app.Name, &app.LatestRevision, &app.CreatedBy, &app.CreatedAt, &removed); err != nil {
				return err
			}
			if removed.Valid {
				app.RemovedAt = &removed.Time
			}
			out = append(out, app)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
func (t *tenancyStore) ReadApplicationRevision(ctx context.Context, a TenantAccess, id string, number int) (*ApplicationRevision, error) {
	var revision ApplicationRevision
	err := t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		if a.EnvironmentID == "" || number < 1 {
			return ErrInvalid
		}
		var raw string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,application_id,organization_id,environment_id,number,spec,digest,created_by,created_at FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), a.OrganizationID, a.EnvironmentID, id, number).Scan(&revision.ID, &revision.ApplicationID, &revision.OrganizationID, &revision.EnvironmentID, &revision.Number, &raw, &revision.Digest, &revision.CreatedBy, &revision.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(applicationSpecDigest([]byte(raw))), []byte(revision.Digest)) != 1 {
			return ErrRevisionCorrupt
		}
		return json.Unmarshal([]byte(raw), &revision.Spec)
	})
	if err != nil {
		return nil, err
	}
	return &revision, nil
}

// DiscardApplication explicitly deletes an undeployed draft and its revision history.
// Adopted instances restrict deletion until ownership is explicitly released.
func (t *tenancyStore) DiscardApplication(ctx context.Context, a TenantAccess, id string, expected int) error {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return ErrInvalid
	}
	id = parsed.String()
	return t.withTenantTarget(ctx, a, permissions.ApplicationDestroy, id, func(tx *sql.Tx) error {
		if a.EnvironmentID == "" || expected < 1 || expected > MaxApplicationRevisions {
			return ErrInvalid
		}
		query := `SELECT latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`
		if t.store.driver == "postgres" {
			query += " FOR UPDATE"
		}
		var head int
		err := tx.QueryRowContext(ctx, t.store.rebind(query), a.OrganizationID, a.EnvironmentID, id).Scan(&head)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if head != expected {
			return ErrRevisionConflict
		}
		var adopted int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM application_instances WHERE application_id=?`), id).Scan(&adopted); err != nil {
			return err
		}
		if adopted != 0 {
			return ErrApplicationAdopted
		}
		if _, err = tx.ExecContext(ctx, t.store.rebind(`DELETE FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`DELETE FROM applications WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, id)
		return err
	})
}
