package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"time"
)

type tenancyStore struct{ store *SQLStore }

// Initialize runs once after account bootstrap, including on upgraded installations.
// Claiming the marker before reading users serializes concurrent initializers. The claim,
// membership and audit commit together; a crash rolls them all back.
func (t *tenancyStore) Initialize(ctx context.Context) error {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE tenancy_bootstrap SET completed_at=? WHERE id=1 AND completed_at IS NULL`), now)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return tx.Commit()
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("create the bootstrap account before initializing tenancy")
	}
	var userID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE role='admin' AND status='active' AND sso_provider='local' ORDER BY CASE WHEN username='admin' THEN 0 ELSE 1 END, created_at, id LIMIT 1`).Scan(&userID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO organization_memberships (organization_id,user_id,role,status) VALUES (?,?, 'organization_admin','active')`), InitialOrganizationID, userID)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,correlation_id,result) VALUES (?,'organization.bootstrap',?,'One-time initial organization membership migration',?,'organization',?,?,'success')`), userID, InitialOrganizationID, now, InitialOrganizationID, uuid.NewString())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (t *tenancyStore) CreateOrganization(ctx context.Context, o *Organization) error {
	o.CreatedAt = time.Now().UTC()
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`INSERT INTO organizations (id,name,created_at) VALUES (?,?,?)`), o.ID, o.Name, o.CreatedAt)
	return err
}

// CreateOrganizationWithAdmin creates o and its first organization_admin in one transaction,
// so no organization exists without an administrator.
func (t *tenancyStore) CreateOrganizationWithAdmin(ctx context.Context, o *Organization, adminUserID string) error {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if t.store.driver == "postgres" {
		// Self-conflicting mode serializes creators so the name check below holds until commit.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE organizations IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
	}
	var status string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT status FROM users WHERE id=?`), adminUserID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "active" {
		return ErrInvalid
	}
	var taken int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM organizations WHERE name=?`), o.Name).Scan(&taken); err != nil {
		return err
	}
	if taken > 0 {
		return ErrAlreadyExists
	}
	o.CreatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO organizations (id,name,created_at) VALUES (?,?,?)`), o.ID, o.Name, o.CreatedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO organization_memberships (organization_id,user_id,role,status) VALUES (?,?,'organization_admin','active')`), o.ID, adminUserID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListOrganizations returns every organization with its active member count, ordered by name.
func (t *tenancyStore) ListOrganizations(ctx context.Context) ([]OrganizationSummary, error) {
	rows, err := t.store.db.QueryContext(ctx, `SELECT o.id,o.name,o.created_at,(SELECT COUNT(*) FROM organization_memberships m WHERE m.organization_id=o.id AND m.status='active') FROM organizations o ORDER BY o.name,o.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []OrganizationSummary{}
	for rows.Next() {
		var o OrganizationSummary
		if err := rows.Scan(&o.ID, &o.Name, &o.CreatedAt, &o.Members); err != nil {
			return nil, err
		}
		result = append(result, o)
	}
	return result, rows.Err()
}
func (t *tenancyStore) GetOrganization(ctx context.Context, id string) (*Organization, error) {
	var o Organization
	err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT id,name,created_at FROM organizations WHERE id=?`), id).Scan(&o.ID, &o.Name, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &o, err
}
func (t *tenancyStore) SetMembership(ctx context.Context, m *OrganizationMembership) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`INSERT INTO organization_memberships (organization_id,user_id,role,status) VALUES (?,?,?,?) ON CONFLICT (organization_id,user_id) DO UPDATE SET role=excluded.role,status=excluded.status`), m.OrganizationID, m.UserID, m.Role, m.Status)
	return err
}
func (t *tenancyStore) GetMembership(ctx context.Context, org, user string) (*OrganizationMembership, error) {
	var m OrganizationMembership
	err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT organization_id,user_id,role,status FROM organization_memberships WHERE organization_id=? AND user_id=?`), org, user).Scan(&m.OrganizationID, &m.UserID, &m.Role, &m.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &m, err
}
func (t *tenancyStore) DeleteMembership(ctx context.Context, org, user string) error {
	return t.change(ctx, `DELETE FROM organization_memberships WHERE organization_id=? AND user_id=?`, org, user)
}
func (t *tenancyStore) CreateEnvironment(ctx context.Context, e *Environment) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`INSERT INTO environments (id,organization_id,name) VALUES (?,?,?)`), e.ID, e.OrganizationID, e.Name)
	return err
}
func (t *tenancyStore) GetEnvironment(ctx context.Context, org, id string) (*Environment, error) {
	var e Environment
	err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT id,organization_id,name FROM environments WHERE organization_id=? AND id=?`), org, id).Scan(&e.ID, &e.OrganizationID, &e.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &e, err
}
func (t *tenancyStore) RenameEnvironment(ctx context.Context, org, id, name string) error {
	return t.change(ctx, `UPDATE environments SET name=? WHERE organization_id=? AND id=?`, name, org, id)
}
func (t *tenancyStore) DeleteEnvironment(ctx context.Context, org, id string) error {
	return t.change(ctx, `DELETE FROM environments WHERE organization_id=? AND id=?`, org, id)
}
func (t *tenancyStore) CreateOrganizationGroup(ctx context.Context, g *OrganizationGroup) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`INSERT INTO organization_groups (id,organization_id,name) VALUES (?,?,?)`), g.ID, g.OrganizationID, g.Name)
	return err
}
func (t *tenancyStore) AddOrganizationGroupMember(ctx context.Context, org, group, user string) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`INSERT INTO organization_group_members (organization_id,group_id,user_id) VALUES (?,?,?)`), org, group, user)
	return err
}
func (t *tenancyStore) ListOrganizationGroupMembers(ctx context.Context, org, group string) ([]string, error) {
	rows, err := t.store.db.QueryContext(ctx, t.store.rebind(`SELECT user_id FROM organization_group_members WHERE organization_id=? AND group_id=? ORDER BY user_id`), org, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		users = append(users, id)
	}
	return users, rows.Err()
}
func (t *tenancyStore) RemoveOrganizationGroupMember(ctx context.Context, org, group, user string) error {
	return t.change(ctx, `DELETE FROM organization_group_members WHERE organization_id=? AND group_id=? AND user_id=?`, org, group, user)
}
func (t *tenancyStore) change(ctx context.Context, q string, args ...any) error {
	result, err := t.store.db.ExecContext(ctx, t.store.rebind(q), args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
