package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// SetApplicationServiceIPs saves a network-only revision, re-sealing the existing
// values without exposing them. See docs/application-schema.md, Static Service IPs.
// An empty map restores automatic allocation for newly created Services; it never
// changes a live Service's immutable IP. Saving does not dispatch runtime work.
func (t *tenancyStore) SetApplicationServiceIPs(ctx context.Context, a TenantAccess, id string, expected int, ips map[string]string, key []byte) (int, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return 0, ErrInvalid
	}
	id = parsed.String()
	err = t.withTenantTarget(ctx, a, permissions.ApplicationEdit, id+"/networking/revisions/"+strconv.Itoa(expected+1), func(tx *sql.Tx) error {
		if expected < 1 || expected >= MaxApplicationRevisions || len(ips) > protocol.MaxDeploymentServices {
			return ErrInvalid
		}
		if err := t.lockApplication(ctx, tx, a, id); err != nil {
			return err
		}
		var runtime string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.runtime FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.application_id=? AND i.organization_id=? AND i.environment_id=?`), id, a.OrganizationID, a.EnvironmentID).Scan(&runtime)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMappingRequired
		}
		if err != nil {
			return err
		}
		if runtime != protocol.RuntimeKubernetes {
			return ErrRuntimeUnsupported
		}
		spec, values, _, err := t.resolveApplicationValues(ctx, tx, a, id, expected, key)
		if err != nil {
			return err
		}
		defer clear(values)
		if spec.Kubernetes == nil {
			spec.Kubernetes = &KubernetesExtension{}
		}
		spec.Kubernetes.ServiceIPs = ips
		return t.appendApplicationRevisionTx(ctx, tx, a, id, expected, spec, values, key)
	})
	if err != nil {
		return 0, err
	}
	return expected + 1, nil
}
