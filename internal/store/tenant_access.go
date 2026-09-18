package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// withTenant serializes live authorization with a mutation and commits both with the audit
// record. PostgreSQL locks the user and membership rows through FOR UPDATE on the join.
// SQLite has a single writer, so a no-op write on the membership row takes the RESERVED
// lock before the read: a deferred read-then-write transaction would otherwise lose to a
// concurrent revocation and fail with BUSY_SNAPSHOT instead of waiting behind it.
func (t *tenancyStore) withTenant(ctx context.Context, a TenantAccess, action permissions.Action, op func(*sql.Tx) error) error {
	return t.run(ctx, a, action, "", true, op)
}

// withTenantTarget is withTenant auditing an explicit target (for example a member's user ID).
func (t *tenancyStore) withTenantTarget(ctx context.Context, a TenantAccess, action permissions.Action, target string, op func(*sql.Tx) error) error {
	return t.run(ctx, a, action, target, true, op)
}

// auditedReads are the low-volume, sensitive reads that keep a success row: who looked at the
// audit trail, and who enumerated the organization's members. Every other successful read
// writes nothing (docs/authorization-matrix.md, "Read audit"); denials are always recorded.
var auditedReads = map[permissions.Action]bool{
	permissions.AuditRead:     true,
	permissions.MembersManage: true,
	// A log carries whatever the application printed, so who read one is worth a row even
	// though the bodies themselves are never stored (docs/authorization-matrix.md).
	permissions.ContainerLogs: true,
}

// readTenant checks the same live authorization without locks; reads use a snapshot. A
// successful read writes no audit row unless its action is in auditedReads: inventory and
// list reads arrive every few seconds per endpoint and would be the audit-growth threat
// themselves.
func (t *tenancyStore) readTenant(ctx context.Context, a TenantAccess, action permissions.Action, op func(*sql.Tx) error) error {
	return t.run(ctx, a, action, "", false, op)
}

func (t *tenancyStore) run(ctx context.Context, a TenantAccess, action permissions.Action, target string, lock bool, op func(*sql.Tx) error) error {
	if a.ActorID == "" || a.OrganizationID == "" {
		return ErrForbidden
	}
	if a.CorrelationID == "" {
		a.CorrelationID = uuid.NewString()
	}
	record := &AuditRecord{UserID: a.ActorID, Action: string(action), Resource: a.OrganizationID, IPAddress: a.IPAddress, Scope: "organization", OrganizationID: a.OrganizationID, EnvironmentID: a.EnvironmentID, CorrelationID: a.CorrelationID, CreatedAt: time.Now().UTC()}
	if a.EnvironmentID != "" {
		record.Resource = a.EnvironmentID
	}
	if target != "" {
		record.Resource = target
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `SELECT u.status,u.must_change_password,m.status,m.role FROM users u JOIN organization_memberships m ON m.user_id=u.id WHERE u.id=? AND m.organization_id=?`
	if lock && t.store.driver == "postgres" {
		query += " FOR UPDATE"
	} else if lock {
		if _, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE organization_memberships SET status=status WHERE organization_id=? AND user_id=?`), a.OrganizationID, a.ActorID); err != nil {
			return err
		}
	}
	var status, memberStatus, role string
	var restricted bool
	err = tx.QueryRowContext(ctx, t.store.rebind(query), a.ActorID, a.OrganizationID).Scan(&status, &restricted, &memberStatus, &role)
	if errors.Is(err, sql.ErrNoRows) {
		// The URL scope is unverified for a non-member: no tenant audit row, or any caller
		// could write into another organization's history without bound.
		return ErrForbidden
	}
	if err == nil && (status != "active" || restricted || memberStatus != "active" || !permissions.Allows(role, action)) {
		err = ErrForbidden
	}
	if err == nil && a.EnvironmentID != "" && action != permissions.EnvironmentCreate {
		var found int
		err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT 1 FROM environments WHERE organization_id=? AND id=?`), a.OrganizationID, a.EnvironmentID).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
	}
	if err == nil {
		err = op(tx)
	}
	if err != nil {
		_ = tx.Rollback()
		record.Result = "failure"
		if errors.Is(err, ErrForbidden) {
			record.Result = "denied"
		}
		// Never put SQL errors, names, request bodies or credentials into audit details.
		if auditErr := t.store.Audit().LogAudit(ctx, record); auditErr != nil {
			return auditErr
		}
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key") {
			return ErrAlreadyExists
		}
		return err
	}
	if !lock && !auditedReads[action] {
		return tx.Commit()
	}
	record.Result = "success"
	_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), record.UserID, record.Action, record.Resource, record.IPAddress, record.CreatedAt, record.Scope, record.OrganizationID, record.EnvironmentID, record.CorrelationID, record.Result)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (t *tenancyStore) ReadOrganization(ctx context.Context, a TenantAccess) (*Organization, error) {
	var o Organization
	err := t.readTenant(ctx, a, permissions.OrganizationRead, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,name,created_at FROM organizations WHERE id=?`), a.OrganizationID).Scan(&o.ID, &o.Name, &o.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &o, nil
}
func (t *tenancyStore) ReadEnvironment(ctx context.Context, a TenantAccess) (*Environment, error) {
	var e Environment
	err := t.readTenant(ctx, a, permissions.EnvironmentRead, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,organization_id,name FROM environments WHERE organization_id=? AND id=?`), a.OrganizationID, a.EnvironmentID).Scan(&e.ID, &e.OrganizationID, &e.Name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &e, nil
}
func (t *tenancyStore) ListEnvironments(ctx context.Context, a TenantAccess, offset, limit int) ([]Environment, error) {
	result := []Environment{}
	err := t.readTenant(ctx, a, permissions.EnvironmentRead, func(tx *sql.Tx) error {
		if offset < 0 || limit < 1 || limit > 200 {
			return ErrInvalid
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,organization_id,name FROM environments WHERE organization_id=? ORDER BY id LIMIT ? OFFSET ?`), a.OrganizationID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Environment
			if err := rows.Scan(&e.ID, &e.OrganizationID, &e.Name); err != nil {
				return err
			}
			result = append(result, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// validTenantName refuses text that could forge or reorder a line in the confirmation dialogs
// and logs an operator relies on: control characters (C0, DEL, C1), Unicode line and paragraph
// separators, and bidi embedding/override/isolate controls.
func validTenantName(name string) bool {
	return strings.TrimSpace(name) != "" && displaySafe(name)
}

func displaySafe(s string) bool {
	if len(s) > 255 {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return false
		}
	}
	return true
}
func (t *tenancyStore) AddEnvironment(ctx context.Context, a TenantAccess, name string) (*Environment, error) {
	a.EnvironmentID = uuid.NewString()
	e := Environment{ID: a.EnvironmentID, OrganizationID: a.OrganizationID, Name: strings.TrimSpace(name)}
	err := t.withTenant(ctx, a, permissions.EnvironmentCreate, func(tx *sql.Tx) error {
		if !validTenantName(name) {
			return ErrInvalid
		}
		_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO environments (id,organization_id,name) VALUES (?,?,?)`), e.ID, e.OrganizationID, e.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &e, nil
}
func (t *tenancyStore) UpdateEnvironment(ctx context.Context, a TenantAccess, name string) error {
	return t.withTenant(ctx, a, permissions.EnvironmentUpdate, func(tx *sql.Tx) error {
		if !validTenantName(name) {
			return ErrInvalid
		}
		result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE environments SET name=? WHERE organization_id=? AND id=?`), strings.TrimSpace(name), a.OrganizationID, a.EnvironmentID)
		return tenantChangeResult(result, err)
	})
}
func (t *tenancyStore) RemoveEnvironment(ctx context.Context, a TenantAccess) error {
	return t.withTenant(ctx, a, permissions.EnvironmentDelete, func(tx *sql.Tx) error {
		// Endpoints must be revoked first; revoked ones are history and go with the environment.
		var live int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM endpoints WHERE organization_id=? AND environment_id=? AND state<>'revoked'`), a.OrganizationID, a.EnvironmentID).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrInUse
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM endpoints WHERE organization_id=? AND environment_id=?`), a.OrganizationID, a.EnvironmentID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM environments WHERE organization_id=? AND id=?`), a.OrganizationID, a.EnvironmentID)
		return tenantChangeResult(result, err)
	})
}
func tenantChangeResult(result sql.Result, err error) error {
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
func (t *tenancyStore) ReadAudit(ctx context.Context, a TenantAccess, offset, limit int) ([]AuditRecord, error) {
	records := []AuditRecord{}
	err := t.readTenant(ctx, a, permissions.AuditRead, func(tx *sql.Tx) error {
		if offset < 0 || limit < 1 || limit > 200 {
			return ErrInvalid
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result FROM audit_records WHERE scope='organization' AND organization_id=? AND (?='' OR environment_id=?) ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`), a.OrganizationID, a.EnvironmentID, a.EnvironmentID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r AuditRecord
			if err := rows.Scan(&r.ID, &r.UserID, &r.Action, &r.Resource, &r.Details, &r.IPAddress, &r.CreatedAt, &r.Scope, &r.OrganizationID, &r.EnvironmentID, &r.CorrelationID, &r.Result); err != nil {
				return err
			}
			records = append(records, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}
