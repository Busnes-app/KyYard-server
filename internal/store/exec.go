package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
)

// OpenExecTarget implements docs/agent-protocol.md section 7. The confirmed
// inventory identity, live permission and durable intent precede any runtime work.
// Argv and terminal contents are deliberately absent from the audit trail.
// A pod target (spec.Pod) must be on a Kubernetes endpoint whose manifest grants its namespace,
// and match the last inventory's pod by namespace and name: confirm repeats the pod name, the
// UID is the inventory's, the pod is Running and runs the container. It audits pod.exec.open.
func (t *tenancyStore) OpenExecTarget(ctx context.Context, a TenantAccess, endpointID, streamID, confirm string, spec protocol.ExecSpec) (string, error) {
	if spec.Validate() != nil || streamID == "" || len(streamID) > 128 {
		return "", ErrInvalid
	}
	target := endpointID + "/" + spec.Container
	if spec.Pod != nil {
		target = ExecPodResource(endpointID, *spec.Pod)
	}
	var environment string
	err := t.withTenantTarget(ctx, a, permissions.ContainerExec, target, func(tx *sql.Tx) error {
		var state, raw, runtime, namespaces string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.environment_id,e.state,e.runtime,e.deploy_namespaces,i.snapshot FROM endpoints e JOIN endpoint_inventory i ON i.endpoint_id=e.id WHERE e.id=? AND e.organization_id=? AND (?='' OR e.environment_id=?)`), endpointID, a.OrganizationID, a.EnvironmentID, a.EnvironmentID).Scan(&environment, &state, &runtime, &namespaces, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if spec.ValidateFor(runtime) != nil {
			return ErrRuntimeUnsupported
		}
		if state != "active" {
			return ErrEndpointOffline
		}
		if spec.Pod != nil {
			return t.openPodExec(ctx, tx, a, environment, namespaces, raw, target, streamID, confirm, *spec.Pod)
		}
		var snapshot protocol.Snapshot
		if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
			return err
		}
		found := false
		for _, c := range snapshot.Containers {
			if c.ID == spec.Container {
				if c.Name != confirm || c.ImageID != spec.ImageID || c.State != "running" {
					return ErrInvalid
				}
				found = true
				break
			}
		}
		if !found {
			return ErrNotFound
		}
		// Scope comes from the endpoint, including when the route has no environment.
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), a.ActorID, "container.exec.open", endpointID+"/"+spec.Container, fmt.Sprintf("stream=%q user=%q image=%q", streamID, spec.User, spec.ImageID), a.IPAddress, time.Now().UTC(), "organization", a.OrganizationID, environment, a.CorrelationID, "unknown")
		return err
	})
	return environment, err
}

// ExecPodResource is the audit resource of a pod terminal: endpoint/pods/namespace/pod/container.
func ExecPodResource(endpointID string, pod protocol.PodTarget) string {
	return endpointID + "/pods/" + pod.Namespace + "/" + pod.Name + "/" + pod.Container
}

func (t *tenancyStore) openPodExec(ctx context.Context, tx *sql.Tx, a TenantAccess, environment, namespaces, raw, resource, streamID, confirm string, target protocol.PodTarget) error {
	if !slices.Contains(decodeNamespaces(namespaces), target.Namespace) {
		return ErrNamespaceNotGranted
	}
	var snapshot protocol.Snapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return err
	}
	if snapshot.Kubernetes == nil {
		return ErrNotFound
	}
	i := slices.IndexFunc(snapshot.Kubernetes.Pods, func(p protocol.Pod) bool { return p.Namespace == target.Namespace && p.Name == target.Name })
	if i < 0 {
		return ErrNotFound
	}
	pod := snapshot.Kubernetes.Pods[i]
	if confirm != pod.Name || pod.UID == "" || pod.UID != target.UID || pod.Phase != "Running" ||
		!slices.ContainsFunc(pod.Containers, func(c protocol.PodContainer) bool { return c.Name == target.Container }) {
		return ErrInvalid
	}
	_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), a.ActorID, "pod.exec.open", resource, fmt.Sprintf("stream=%q uid=%q", streamID, target.UID), a.IPAddress, time.Now().UTC(), "organization", a.OrganizationID, environment, a.CorrelationID, "unknown")
	return err
}

// CheckEndpointAccess authorizes action on an endpoint in scope, with no success row and a
// denied one on refusal. The API runs it before its runtime gate, so a member without the
// permission is told so rather than told the endpoint's runtime; it grants no runtime authority.
func (t *tenancyStore) CheckEndpointAccess(ctx context.Context, a TenantAccess, action permissions.Action, endpointID string) error {
	return t.readTenant(ctx, a, action, func(tx *sql.Tx) error { return t.endpointInScope(ctx, tx, a, endpointID) })
}
