package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Busness-app/kyyard-server/internal/permissions"
)

func validTenantRole(role TenantRole) bool {
	switch role {
	case RoleOrganizationAdmin, RoleEnvironmentAdmin, RoleOperator, RoleDeveloper, RoleReadOnly:
		return true
	}
	return false
}

func (t *tenancyStore) ListMembers(ctx context.Context, a TenantAccess, offset, limit int) ([]OrganizationMember, error) {
	result := []OrganizationMember{}
	err := t.readTenant(ctx, a, permissions.MembersManage, func(tx *sql.Tx) error {
		if offset < 0 || limit < 1 || limit > 200 {
			return ErrInvalid
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT m.user_id,u.username,m.role,m.status FROM organization_memberships m JOIN users u ON u.id=m.user_id WHERE m.organization_id=? ORDER BY u.username,m.user_id LIMIT ? OFFSET ?`), a.OrganizationID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m OrganizationMember
			if err := rows.Scan(&m.UserID, &m.Username, &m.Role, &m.Status); err != nil {
				return err
			}
			result = append(result, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// PutMembership creates or replaces a membership. The organization must keep at least one
// active administrator, so an administrator cannot lock everyone out, including themself.
func (t *tenancyStore) PutMembership(ctx context.Context, a TenantAccess, userID string, role TenantRole, status string) error {
	return t.withTenantTarget(ctx, a, permissions.MembersManage, userID, func(tx *sql.Tx) error {
		if userID == "" || len(userID) > 64 || !validTenantRole(role) || (status != "active" && status != "disabled") {
			return ErrInvalid
		}
		if err := t.lockOrganization(ctx, tx, a.OrganizationID); err != nil {
			return err
		}
		var found int
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT 1 FROM users WHERE id=?`), userID).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO organization_memberships (organization_id,user_id,role,status) VALUES (?,?,?,?) ON CONFLICT (organization_id,user_id) DO UPDATE SET role=excluded.role,status=excluded.status`), a.OrganizationID, userID, role, status); err != nil {
			return err
		}
		return t.requireActiveAdmin(ctx, tx, a.OrganizationID)
	})
}

func (t *tenancyStore) RemoveMembership(ctx context.Context, a TenantAccess, userID string) error {
	return t.withTenantTarget(ctx, a, permissions.MembersManage, userID, func(tx *sql.Tx) error {
		if err := t.lockOrganization(ctx, tx, a.OrganizationID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM organization_memberships WHERE organization_id=? AND user_id=?`), a.OrganizationID, userID)
		if err := tenantChangeResult(result, err); err != nil {
			return err
		}
		return t.requireActiveAdmin(ctx, tx, a.OrganizationID)
	})
}

// lockOrganization serializes membership changes within one organization so the
// administrator count below cannot be read by two transactions before either commits.
// SQLite already holds its single writer lock from withTenant.
func (t *tenancyStore) lockOrganization(ctx context.Context, tx *sql.Tx, org string) error {
	if t.store.driver != "postgres" {
		return nil
	}
	var found int
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT 1 FROM organizations WHERE id=? FOR UPDATE`), org).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	return err
}

func (t *tenancyStore) requireActiveAdmin(ctx context.Context, tx *sql.Tx, org string) error {
	var n int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM organization_memberships m JOIN users u ON u.id=m.user_id WHERE m.organization_id=? AND m.role='organization_admin' AND m.status='active' AND u.status='active'`), org).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

func (t *tenancyStore) ListMemberOrganizations(ctx context.Context, userID string) ([]MemberOrganization, error) {
	rows, err := t.store.db.QueryContext(ctx, t.store.rebind(`SELECT o.id,o.name,m.role FROM organization_memberships m JOIN organizations o ON o.id=m.organization_id WHERE m.user_id=? AND m.status='active' ORDER BY o.name,o.id`), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []MemberOrganization{}
	for rows.Next() {
		var o MemberOrganization
		if err := rows.Scan(&o.ID, &o.Name, &o.Role); err != nil {
			return nil, err
		}
		result = append(result, o)
	}
	return result, rows.Err()
}
