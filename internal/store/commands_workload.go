package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// ActionWorkloadApply is a direct command whose frame is a workload.apply, settled from the
// agent's deployment.result with service protocol.WorkloadApplyService.
const ActionWorkloadApply = "workload.apply"

var (
	// ErrNamespaceNotGranted is a cluster write in a namespace the endpoint's manifest does not
	// list: refused before any frame, whatever the agent's grant says.
	ErrNamespaceNotGranted = errors.New("the cluster's manifest does not grant writes in that namespace")
	// ErrWorkloadManaged is a configuration read, apply or delete of a workload a KyYard
	// application deployed; it changes through its application.
	ErrWorkloadManaged = errors.New("the workload belongs to a KyYard application")
)

// workloadActions name a workload or a pod by "<namespace>/<kind>/<name>" in the reference.
var workloadActions = map[string]bool{protocol.ActionWorkloadRestart: true, protocol.ActionWorkloadScale: true, protocol.ActionWorkloadDelete: true, protocol.ActionPodDelete: true}

// WorkloadAction reports whether action targets a Kubernetes workload or pod.
func WorkloadAction(action string) bool { return workloadActions[action] }

// workloadCommand checks a workload command's shape before any row is read: the reference, the
// kind the action takes, the replica count (scale only, 0..MaxWorkloadReplicas) and a delete's
// confirmation of the name. A workload carries no state or image expectation.
func workloadCommand(action, reference, confirm string, expects protocol.Expectation) (protocol.WorkloadRef, error) {
	ref, err := protocol.ParseWorkloadRef(reference)
	if err != nil {
		return ref, fmt.Errorf("%w: reference", ErrInvalid)
	}
	pod := ref.Kind == protocol.WorkloadPod
	switch {
	case pod != (action == protocol.ActionPodDelete):
		return ref, fmt.Errorf("%w: %s does not take a %s", ErrInvalid, action, ref.Kind)
	case action == protocol.ActionWorkloadScale && ref.Kind == protocol.WorkloadDaemonSet:
		return ref, fmt.Errorf("%w: a DaemonSet has no replica count", ErrInvalid)
	case (action == protocol.ActionWorkloadScale) != (expects.Replicas != nil):
		return ref, fmt.Errorf("%w: replicas are a scale's, and a scale's only", ErrInvalid)
	case expects.Replicas != nil && (*expects.Replicas < 0 || *expects.Replicas > protocol.MaxWorkloadReplicas):
		return ref, fmt.Errorf("%w: replicas are 0 to %d", ErrInvalid, protocol.MaxWorkloadReplicas)
	case expects.State != "" || expects.ImageDigest != "":
		return ref, fmt.Errorf("%w: a workload command carries no state or image expectation", ErrInvalid)
	case destructivePermissions[commandActions[action]] && confirm != ref.Name:
		return ref, fmt.Errorf("%w: confirm must be %q", ErrInvalid, ref.Name)
	}
	return ref, nil
}

// createWorkloadCommand is CreateCommand for a workload or pod action: the reference names it,
// the endpoint must be an active Kubernetes endpoint whose manifest grants the namespace, the
// target must be in the last inventory, and a delete of a workload a KyYard application deployed
// is refused (a restart or scale is not). The audit rows are named after the action.
func (t *tenancyStore) createWorkloadCommand(ctx context.Context, a TenantAccess, endpointID, action, reference, confirm string, expects protocol.Expectation) (*Command, error) {
	ref, err := workloadCommand(action, reference, confirm, expects)
	if err != nil {
		return nil, err
	}
	if a.CorrelationID == "" {
		a.CorrelationID = uuid.NewString()
	}
	now := time.Now().UTC()
	cmd := &Command{ID: uuid.NewString(), EndpointID: endpointID, ActorID: a.ActorID, RequestID: a.CorrelationID, Action: action, Reference: ref.String(), Expects: expects, Deadline: now.Add(CommandDeadline), CreatedAt: now}
	target, details := endpointID+"/"+cmd.Reference, ""
	if expects.Replicas != nil {
		details = fmt.Sprintf("replicas=%d", *expects.Replicas)
	}
	err = t.runAs(ctx, a, commandActions[action], action, &target, &details, true, func(tx *sql.Tx) error {
		var state, runtime, namespaces string
		var err error
		cmd.OrganizationID, cmd.EnvironmentID, state, runtime, namespaces, err = t.clusterEndpoint(ctx, tx, a, endpointID)
		if err != nil {
			return err
		}
		managed, _, err := t.clusterTarget(ctx, tx, endpointID, runtime, namespaces, ref)
		if err != nil {
			return err
		}
		if managed && action == protocol.ActionWorkloadDelete {
			return ErrWorkloadManaged
		}
		if state != "active" {
			return fmt.Errorf("%w: it is %s", ErrEndpointOffline, state)
		}
		raw, err := json.Marshal(cmd.Expects)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_commands (id,endpoint_id,organization_id,environment_id,actor_id,request_id,action,container_id,reference,expects,deadline,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`),
			cmd.ID, cmd.EndpointID, cmd.OrganizationID, cmd.EnvironmentID, cmd.ActorID, cmd.RequestID, cmd.Action, "", cmd.Reference, string(raw), cmd.Deadline, cmd.CreatedAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return cmd, nil
}

// clusterTarget checks ref against the endpoint row and its last inventory inside tx: a
// Kubernetes endpoint whose manifest grants the namespace, and the workload or pod present. It
// returns whether a KyYard application deployed the workload, and the pod for a pod target.
func (t *tenancyStore) clusterTarget(ctx context.Context, tx *sql.Tx, endpointID, runtime, namespaces string, ref protocol.WorkloadRef) (managed bool, pod *protocol.Pod, err error) {
	if runtime != protocol.RuntimeKubernetes {
		return false, nil, ErrRuntimeUnsupported
	}
	if !slices.Contains(decodeNamespaces(namespaces), ref.Namespace) {
		return false, nil, ErrNamespaceNotGranted
	}
	var raw string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT snapshot FROM endpoint_inventory WHERE endpoint_id=?`), endpointID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, ErrNotFound
	}
	if err != nil {
		return false, nil, err
	}
	var snap protocol.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return false, nil, err
	}
	if snap.Kubernetes == nil {
		return false, nil, ErrNotFound
	}
	if ref.Kind == protocol.WorkloadPod {
		i := slices.IndexFunc(snap.Kubernetes.Pods, func(p protocol.Pod) bool { return p.Namespace == ref.Namespace && p.Name == ref.Name })
		if i < 0 {
			return false, nil, ErrNotFound
		}
		return false, &snap.Kubernetes.Pods[i], nil
	}
	i := slices.IndexFunc(snap.Kubernetes.Workloads, func(w protocol.Workload) bool {
		return strings.EqualFold(w.Kind, ref.Kind) && w.Namespace == ref.Namespace && w.Name == ref.Name
	})
	if i < 0 {
		return false, nil, ErrNotFound
	}
	return snap.Kubernetes.Workloads[i].Application != "", nil, nil
}

// clusterEndpoint reads the scoped endpoint's state, runtime and granted namespaces inside tx.
func (t *tenancyStore) clusterEndpoint(ctx context.Context, tx *sql.Tx, a TenantAccess, endpointID string) (org, env, state, runtime, namespaces string, err error) {
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT organization_id,environment_id,state,runtime,deploy_namespaces FROM endpoints WHERE id=? AND organization_id=? AND (?='' OR environment_id=?)`),
		endpointID, a.OrganizationID, a.EnvironmentID, a.EnvironmentID).Scan(&org, &env, &state, &runtime, &namespaces)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// CheckWorkloadTarget authorizes action and checks ref on an active Kubernetes endpoint: a
// granted namespace, present in the last inventory, and not deployed by a KyYard application.
// It writes no success row; the API runs it before a configuration read is asked for.
func (t *tenancyStore) CheckWorkloadTarget(ctx context.Context, a TenantAccess, action permissions.Action, endpointID string, ref protocol.WorkloadRef) error {
	return t.readTenant(ctx, a, action, func(tx *sql.Tx) error {
		_, _, state, runtime, namespaces, err := t.clusterEndpoint(ctx, tx, a, endpointID)
		if err != nil {
			return err
		}
		managed, _, err := t.clusterTarget(ctx, tx, endpointID, runtime, namespaces, ref)
		if err != nil {
			return err
		}
		if managed {
			return ErrWorkloadManaged
		}
		if state != "active" {
			return fmt.Errorf("%w: it is %s", ErrEndpointOffline, state)
		}
		return nil
	})
}

// RecordWorkloadConfigurationRead re-authorizes container.configure and writes one success row
// workload.configuration.read naming the workload and how many settings an apply would drop,
// never a value.
func (t *tenancyStore) RecordWorkloadConfigurationRead(ctx context.Context, a TenantAccess, endpointID string, ref protocol.WorkloadRef, unsupported int) error {
	resource, details := endpointID+"/"+ref.String(), fmt.Sprintf("unsupported=%d", unsupported)
	return t.runAs(ctx, a, permissions.ContainerConfigure, "workload.configuration.read", &resource, &details, true, func(tx *sql.Tx) error {
		return t.endpointInScope(ctx, tx, a, endpointID)
	})
}

// WorkloadApply is an edited pod template as the API received it: Spec.ResourceVersion is the
// read's, and Confirm repeats the workload's name.
type WorkloadApply struct {
	Target  protocol.WorkloadRef
	Confirm string
	Spec    protocol.WorkloadConfiguration
}

// CreateWorkloadApply records a workload.apply direct command under container.configure and
// returns the frame to send. It refuses while another direct command on the endpoint is live,
// a namespace the manifest does not grant, a workload absent from the inventory or deployed by a
// KyYard application, and a spec the protocol refuses. The intent row (workload.apply) names the
// resource version and the container count, never a value; the frame is not stored.
func (t *tenancyStore) CreateWorkloadApply(ctx context.Context, a TenantAccess, endpointID string, w WorkloadApply) (*Command, *protocol.WorkloadApply, error) {
	if parsed, err := protocol.ParseWorkloadRef(w.Target.String()); err != nil || parsed != w.Target || w.Target.Kind == protocol.WorkloadPod {
		return nil, nil, ErrInvalid
	}
	if w.Confirm != w.Target.Name {
		return nil, nil, fmt.Errorf("%w: confirm must be %q", ErrInvalid, w.Target.Name)
	}
	if a.CorrelationID == "" {
		a.CorrelationID = uuid.NewString()
	}
	now := time.Now().UTC()
	cmd := &Command{ID: uuid.NewString(), EndpointID: endpointID, ActorID: a.ActorID, RequestID: a.CorrelationID, Action: ActionWorkloadApply, Reference: w.Target.String(), Expects: protocol.Expectation{}, CreatedAt: now, Deadline: now.Add(DeploymentApplyDeadline)}
	spec := w.Spec
	spec.Target, spec.ObservedAt = w.Target, time.Time{}
	frame := &protocol.WorkloadApply{Request: cmd.ID, Endpoint: endpointID, IssuedAt: now, Deadline: cmd.Deadline, Target: w.Target, ResourceVersion: spec.ResourceVersion, Spec: spec}
	target := endpointID + "/" + cmd.Reference
	details := fmt.Sprintf("resource_version=%s containers=%d", protocol.CleanText(spec.ResourceVersion, protocol.MaxResourceVersionBytes), len(spec.Containers))
	err := t.runAs(ctx, a, permissions.ContainerConfigure, ActionWorkloadApply, &target, &details, true, func(tx *sql.Tx) error {
		var state, runtime, namespaces string
		var err error
		cmd.OrganizationID, cmd.EnvironmentID, state, runtime, namespaces, err = t.clusterEndpoint(ctx, tx, a, endpointID)
		if err != nil {
			return err
		}
		if state != "active" {
			return fmt.Errorf("%w: it is %s", ErrEndpointOffline, state)
		}
		if err := t.sweepDirect(ctx, tx, endpointID, now); err != nil {
			return err
		}
		if err := t.directBusy(ctx, tx, endpointID, now); err != nil {
			return err
		}
		managed, _, err := t.clusterTarget(ctx, tx, endpointID, runtime, namespaces, w.Target)
		if err != nil {
			return err
		}
		if managed || spec.Managed {
			return ErrWorkloadManaged
		}
		if len(spec.Unsupported) > 0 {
			return invalidSpec("configuration_incomplete")
		}
		if err := frame.Validate(now); err != nil {
			_, field, _ := strings.Cut(err.Error(), ": ")
			return invalidSpec("spec_invalid:" + strings.NewReplacer(", ", "_", " ", "_").Replace(cmp.Or(field, "frame")))
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_commands (id,endpoint_id,organization_id,environment_id,actor_id,request_id,action,container_id,reference,expects,deadline,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`),
			cmd.ID, cmd.EndpointID, cmd.OrganizationID, cmd.EnvironmentID, cmd.ActorID, cmd.RequestID, cmd.Action, "", cmd.Reference, "{}", cmd.Deadline, cmd.CreatedAt)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return cmd, frame, nil
}
