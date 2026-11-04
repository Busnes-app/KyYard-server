package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
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
		// Only a user the actor can already see is added: one sharing an organization with them,
		// or any user for a platform administrator. Every other ID is "not found", so an
		// organization administrator cannot probe which user IDs exist.
		var found int
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT 1 FROM users u WHERE u.id=? AND (EXISTS (SELECT 1 FROM users p WHERE p.id=? AND p.role='admin') OR EXISTS (SELECT 1 FROM organization_memberships o JOIN organization_memberships m ON m.organization_id=o.organization_id WHERE o.user_id=u.id AND m.user_id=? AND m.status='active'))`), userID, a.ActorID, a.ActorID).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO organization_memberships (organization_id,user_id,role,status) VALUES (?,?,?,?) ON CONFLICT (organization_id,user_id) DO UPDATE SET role=excluded.role,status=excluded.status`), a.OrganizationID, userID, role, status); err != nil {
			return err
		}
		if err := t.revokeLostAdminCredentials(ctx, tx, a.OrganizationID, userID, a.Principal()); err != nil {
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
		if err := t.revokeLostAdminCredentials(ctx, tx, a.OrganizationID, userID, a.Principal()); err != nil {
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

// revokeLostAdminCredentials runs inside a membership or account change, holding the audit
// lock. When userID no longer holds an active organization_admin membership on an active
// account in org, it revokes the AI agent tokens and status feeds they created there and
// deletes their unclaimed kyPulse pairing codes, one audit row each (actor is who made the
// change), as update policies already pause when their creator loses authority.
func (t *tenancyStore) revokeLostAdminCredentials(ctx context.Context, tx *sql.Tx, org, userID, actor string) error {
	var n int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM organization_memberships m JOIN users u ON u.id=m.user_id WHERE m.organization_id=? AND m.user_id=? AND m.role='organization_admin' AND m.status='active' AND u.status='active'`), org, userID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	now := time.Now().UTC()
	details := "creator " + userID + " is no longer an organization administrator"
	revoke := func(table string) string {
		return `UPDATE ` + table + ` SET revoked_at=?, revoked_by=? WHERE id=? AND revoked_at IS NULL`
	}
	for _, c := range []struct{ query, write, action string }{
		{`SELECT id FROM service_tokens WHERE organization_id=? AND created_by=? AND scope<>'` + ScopePulse + `' AND revoked_at IS NULL`, revoke("service_tokens"), "agent_token.revoke"},
		{`SELECT id FROM status_feeds WHERE organization_id=? AND created_by=? AND revoked_at IS NULL`, revoke("status_feeds"), AuditStatusFeedRevoke},
		{`SELECT id FROM service_token_pairings WHERE organization_id=? AND created_by=? AND consumed_at IS NULL`, `DELETE FROM service_token_pairings WHERE id=? AND consumed_at IS NULL`, "service_token.pairing_revoke"},
	} {
		ids, err := t.collectIDs(ctx, tx, c.query, org, userID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			args := []any{now, actor, id}
			if c.action == "service_token.pairing_revoke" {
				args = []any{id}
			}
			if _, err := tx.ExecContext(ctx, t.store.rebind(c.write), args...); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), actor, c.action, id, details, "", now, "organization", org, "", uuid.NewString(), "success"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *tenancyStore) collectIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, t.store.rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
