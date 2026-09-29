package store

import (
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
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/google/uuid"
)

// Direct commands recreate or run one container from a configuration the operator saw in full.
// Each is an endpoint_commands row whose frame is an explicit deployment.apply, settled from the
// agent's deployment.result.
const (
	ActionRecreate = "container.recreate"
	ActionRun      = "container.run"
)

var directActions = map[string]bool{ActionRecreate: true, ActionRun: true}

var (
	ErrCommandInProgress = errors.New("a container recreate or run is in flight on this endpoint")
	ErrContainerManaged  = errors.New("the container belongs to an adopted application")
)

// InvalidSpecError names what stops a direct command's configuration.
type InvalidSpecError struct{ Blockers []string }

func (e *InvalidSpecError) Error() string { return "invalid spec: " + strings.Join(e.Blockers, ",") }

func invalidSpec(blockers ...string) error {
	slices.Sort(blockers)
	return &InvalidSpecError{Blockers: slices.Compact(blockers)}
}

// DirectCommand is a recreate or run as the API built it.
type DirectCommand struct {
	Action string
	// Confirm repeats the container's current name (recreate) or the new one (run).
	Confirm string
	// State is the replaced container's state the operator saw; recreate only.
	State         string
	MaxFrameBytes int
	// Frame has one explicit service; identity, issue time and deadline are set here.
	Frame protocol.DeploymentRequest
}

// CreateDirectCommand records a recreate or run under container.configure and returns the frame
// to send. It refuses while another direct command on the endpoint is in flight, a container an
// adopted application owns, and a spec the inventory contradicts. Audit details carry the image,
// the acknowledged bind count and the names of the settings the frame sets, never a value.
func (t *tenancyStore) CreateDirectCommand(ctx context.Context, a TenantAccess, endpointID string, dc DirectCommand) (*Command, *protocol.DeploymentRequest, error) {
	req := dc.Frame
	if !directActions[dc.Action] || len(req.Services) != 1 || req.Services[0].Explicit == nil {
		return nil, nil, ErrInvalid
	}
	svc := &req.Services[0]
	recreate := dc.Action == ActionRecreate
	if recreate && (svc.Replaces.Validate() != nil || dc.State == "") || !recreate && (svc.Replaces != protocol.InspectionTarget{} || dc.State != "") {
		return nil, nil, ErrInvalid
	}
	if a.CorrelationID == "" {
		a.CorrelationID = uuid.NewString()
	}
	now := time.Now().UTC()
	cmd := &Command{ID: uuid.NewString(), EndpointID: endpointID, ActorID: a.ActorID, RequestID: a.CorrelationID, Action: dc.Action, ContainerID: svc.Replaces.ContainerID, CreatedAt: now, Deadline: now.Add(DeploymentApplyDeadline)}
	target := endpointID + "/" + svc.ContainerName
	if recreate {
		cmd.Expects = protocol.Expectation{ImageDigest: svc.Replaces.ImageID, State: dc.State}
		target = endpointID + "/" + svc.Replaces.ContainerID
	}
	req.Deployment, req.RequestID, req.Endpoint = cmd.ID, cmd.RequestID, endpointID
	req.Project, req.Revision, req.Explicit = protocol.ExplicitProject, protocol.ExplicitRevision, true
	req.IssuedAt, req.Deadline = now, cmd.Deadline
	image := svc.ImageID
	if svc.Pull != nil {
		image = svc.Pull.Digest
	}
	// Only acknowledgements a bind uses are audited and counted.
	var acknowledged []string
	for _, path := range svc.Explicit.AcknowledgedBinds {
		if slices.ContainsFunc(svc.Mounts, func(m protocol.Mount) bool { return m.Kind == protocol.MountBind && m.Source == path }) {
			acknowledged = append(acknowledged, path)
		}
	}
	details := fmt.Sprintf("image=%s binds=%d fields=%s", image, len(acknowledged), strings.Join(directFields(svc), ","))
	err := t.withTenantTargetDetails(ctx, a, permissions.ContainerConfigure, target, &details, func(tx *sql.Tx) error {
		lock := ""
		if t.store.driver == "postgres" {
			lock = " FOR UPDATE" // serializes direct commands per endpoint; SQLite has one writer
		}
		var state string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT organization_id,environment_id,state FROM endpoints WHERE id=? AND organization_id=? AND (?='' OR environment_id=?)`+lock),
			endpointID, a.OrganizationID, a.EnvironmentID, a.EnvironmentID).Scan(&cmd.OrganizationID, &cmd.EnvironmentID, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if state != "active" {
			return fmt.Errorf("%w: it is %s", ErrEndpointOffline, state)
		}
		// Past its deadline the agent has given up; a late answer may still settle it.
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_commands SET outcome=?, detail=?, settled_at=? WHERE endpoint_id=? AND action IN (?,?) AND outcome='' AND deadline<?`),
			protocol.OutcomeUnknown, "no result arrived before the deadline", now, endpointID, ActionRecreate, ActionRun, now); err != nil {
			return err
		}
		snap, old, err := t.directTarget(ctx, tx, endpointID, dc, svc, now)
		if err != nil {
			return err
		}
		if err := directBlockers(snap, old, svc); err != nil {
			return err
		}
		if b := frameBlocker(req, now, dc.MaxFrameBytes); b != "" {
			return invalidSpec("spec_invalid:" + b)
		}
		expects, err := json.Marshal(cmd.Expects)
		if err != nil {
			return err
		}
		// One row per acknowledged host path, committed with the command or not at all.
		for _, path := range acknowledged {
			if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`),
				a.actor(), "container.bind.acknowledged", protocol.CleanText(target, 255), protocol.CleanText(path, 200), a.IPAddress, now, "organization", a.OrganizationID, a.EnvironmentID, a.CorrelationID, "success"); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO endpoint_commands (id,endpoint_id,organization_id,environment_id,actor_id,request_id,action,container_id,reference,expects,deadline,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`),
			cmd.ID, cmd.EndpointID, cmd.OrganizationID, cmd.EnvironmentID, cmd.ActorID, cmd.RequestID, cmd.Action, cmd.ContainerID, "", string(expects), cmd.Deadline, cmd.CreatedAt)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return cmd, &req, nil
}

// CheckDirectCommand runs CreateDirectCommand's preconditions (the endpoint is active, no other
// direct command is in flight, the target is unmanaged and unchanged, the confirmation matches)
// without writing, so the API refuses before spending registry budget on the image.
func (t *tenancyStore) CheckDirectCommand(ctx context.Context, a TenantAccess, endpointID string, dc DirectCommand) error {
	if !directActions[dc.Action] || len(dc.Frame.Services) != 1 {
		return ErrInvalid
	}
	return t.readTenant(ctx, a, permissions.ContainerConfigure, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT state FROM endpoints WHERE id=? AND organization_id=? AND (?='' OR environment_id=?)`), endpointID, a.OrganizationID, a.EnvironmentID, a.EnvironmentID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if state != "active" {
			return fmt.Errorf("%w: it is %s", ErrEndpointOffline, state)
		}
		_, _, err = t.directTarget(ctx, tx, endpointID, dc, &dc.Frame.Services[0], time.Now().UTC())
		return err
	})
}

// directTarget refuses while another live direct command is on the endpoint, or a managed
// container, and checks against the last inventory that a recreate's target is still the
// container the operator read and that the confirmation names it (or, for a run, the new name).
// It returns the inventory and the replaced container (nil for a run).
func (t *tenancyStore) directTarget(ctx context.Context, tx *sql.Tx, endpointID string, dc DirectCommand, svc *protocol.DeploymentService, now time.Time) (protocol.Snapshot, *protocol.Container, error) {
	var snap protocol.Snapshot
	var busy int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM endpoint_commands WHERE endpoint_id=? AND action IN (?,?) AND outcome='' AND deadline>=?`), endpointID, ActionRecreate, ActionRun, now).Scan(&busy); err != nil {
		return snap, nil, err
	}
	if busy > 0 {
		return snap, nil, ErrCommandInProgress
	}
	recreate := dc.Action == ActionRecreate
	if recreate {
		var one int
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT 1 FROM application_resources WHERE endpoint_id=? AND container_id=?`), endpointID, svc.Replaces.ContainerID).Scan(&one)
		if err == nil {
			return snap, nil, ErrContainerManaged
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return snap, nil, err
		}
	}
	var raw string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT snapshot FROM endpoint_inventory WHERE endpoint_id=?`), endpointID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return snap, nil, fmt.Errorf("%w: this endpoint has reported no inventory", ErrInvalid)
	}
	if err != nil {
		return snap, nil, err
	}
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return snap, nil, err
	}
	if !recreate {
		if dc.Confirm != svc.ContainerName {
			return snap, nil, fmt.Errorf("%w: confirm must be %q", ErrInvalid, svc.ContainerName)
		}
		return snap, nil, nil
	}
	i := slices.IndexFunc(snap.Containers, func(c protocol.Container) bool { return c.ID == svc.Replaces.ContainerID })
	if i < 0 {
		return snap, nil, ErrAdoptionChanged
	}
	old := &snap.Containers[i]
	if old.ImageID != svc.Replaces.ImageID || old.CreatedAt.Unix() != svc.Replaces.CreatedUnix || old.State != dc.State {
		return snap, nil, ErrAdoptionChanged
	}
	if dc.Confirm != old.Name {
		return snap, nil, fmt.Errorf("%w: confirm must be %q", ErrInvalid, old.Name)
	}
	return snap, old, nil
}

// directBlockers checks svc against the last inventory: the name and published ports are free,
// a local image is present, and every bind the replaced container (old; nil for a run) did not
// already grant is acknowledged. A writable bind where the old one was read-only is new.
func directBlockers(snap protocol.Snapshot, old *protocol.Container, svc *protocol.DeploymentService) error {
	var blockers []string
	if svc.ImageID != "" && !slices.ContainsFunc(snap.Images, func(im protocol.Image) bool { return im.ID == svc.ImageID }) {
		blockers = append(blockers, "image_unresolved")
	}
	for _, c := range snap.Containers {
		if old != nil && c.ID == old.ID {
			continue
		}
		if c.Name == svc.ContainerName {
			blockers = append(blockers, "name_taken")
		}
		for _, p := range svc.Ports {
			if p.Host > 0 && slices.ContainsFunc(c.Ports, func(q protocol.Port) bool {
				return q.Host == p.Host && q.Protocol == p.Protocol && hostIPsOverlap(p.HostIP, q.HostIP)
			}) {
				blockers = append(blockers, "port_conflict")
			}
		}
	}
	for _, m := range svc.Mounts {
		if m.Kind != protocol.MountBind || slices.Contains(svc.Explicit.AcknowledgedBinds, m.Source) {
			continue
		}
		if old == nil || !slices.ContainsFunc(old.Mounts, func(o protocol.Mount) bool {
			return o.Kind == protocol.MountBind && o.Source == m.Source && (!o.ReadOnly || m.ReadOnly)
		}) {
			blockers = append(blockers, "bind_unacknowledged")
		}
	}
	if len(blockers) > 0 {
		return invalidSpec(blockers...)
	}
	return nil
}

func hostIPsOverlap(a, b string) bool {
	wild := func(ip string) bool { return ip == "" || ip == "0.0.0.0" || ip == "::" }
	return a == b || wild(a) || wild(b)
}

// directFields names the settings a direct frame sets, for the audit row.
func directFields(s *protocol.DeploymentService) []string {
	e := s.Explicit
	set := map[string]bool{
		"command": len(e.Command) > 0, "entrypoint": len(e.Entrypoint) > 0, "user": e.User != "", "working_dir": e.WorkingDir != "", "hostname": e.Hostname != "",
		"env": len(s.Env) > 0, "labels": len(e.Labels) > 0, "restart": s.Restart != "" && s.Restart != "no", "ports": len(s.Ports) > 0, "mounts": len(s.Mounts) > 0,
		"networks": len(e.Networks) > 0, "resources": e.Resources != protocol.Resources{}, "healthcheck": e.Healthcheck != nil, "privileged": e.Privileged,
		"read_only_rootfs": e.ReadOnlyRootfs, "init": e.Init, "tty": e.TTY, "stdin_open": e.StdinOpen, "cap_add": len(e.CapAdd) > 0, "cap_drop": len(e.CapDrop) > 0,
		"security_opt": len(e.SecurityOpt) > 0, "extra_hosts": len(e.ExtraHosts) > 0, "dns": len(e.DNS) > 0, "devices": len(e.Devices) > 0, "log": e.Log.Driver != "",
		"stop_signal": e.StopSignal != "", "stop_timeout": e.StopTimeout != nil,
	}
	var out []string
	for name, on := range set {
		if on {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// ResolveDirectImage pins reference to the registry's current digest under image.pull, with the
// plan's registry rules: the organization's credential for the host, or an anonymous pull only
// while the organization allows it. The credential is returned for the frame and never stored.
func (t *tenancyStore) ResolveDirectImage(ctx context.Context, a TenantAccess, endpointID, reference string, resolver DigestResolver, key []byte, privateAllowed bool) (*protocol.ImagePull, map[string]protocol.RegistryAuth, error) {
	ref, err := registry.ParseReference(reference)
	if err != nil || resolver == nil {
		return nil, nil, invalidSpec("image_unresolved")
	}
	var cred *registry.Credential
	var allowPrivate bool
	target := endpointID + "/images"
	err = t.run(ctx, a, permissions.ImagePull, &target, nil, false, func(tx *sql.Tx) error {
		if err := t.endpointInScope(ctx, tx, a, endpointID); err != nil {
			return err
		}
		reg, c, err := t.registryFor(ctx, tx, a.OrganizationID, ref.Host, key)
		if errors.Is(err, ErrNotFound) {
			anonymous, err := t.anonymousPull(ctx, tx, a.OrganizationID)
			if err == nil && !anonymous {
				err = invalidSpec("image_unresolved")
			}
			return err
		}
		if err != nil {
			return err
		}
		cred, allowPrivate = c, reg.AllowPrivate && privateAllowed
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	digest := ref.Digest
	if digest == "" {
		rctx, cancel := context.WithTimeout(ctx, ImageCheckDeadline)
		digest, err = resolver.Head(rctx, ref, cred, allowPrivate)
		cancel()
		if err != nil || !validSHA256(digest) {
			return nil, nil, invalidSpec("image_unresolved")
		}
	}
	pull := &protocol.ImagePull{Reference: ref.Host + "/" + ref.Repository + "@" + digest, Digest: digest}
	if ref.Digest == "" {
		pull.Tag = ref.Host + "/" + ref.Repository + ":" + ref.Tag
	}
	var auth map[string]protocol.RegistryAuth
	if cred != nil {
		auth = map[string]protocol.RegistryAuth{ref.Host: {Username: cred.Username, Secret: cred.Secret}}
	}
	return pull, auth, nil
}

// SettleDirectCommand records a direct command's deployment.result, matched by the frame's
// deployment and request IDs on the endpoint that ran it. The first real answer wins; it may
// replace the unknown a dropped socket or a passed deadline left. Anything else is ErrNotFound.
func (t *tenancyStore) SettleDirectCommand(ctx context.Context, endpointID string, res protocol.DeploymentResult) error {
	if res.Validate() != nil {
		return ErrUnreadableResult
	}
	if res.RequestID == "" {
		return ErrNotFound // a binary without request IDs never ran a direct command
	}
	// A direct frame has one service, named direct; a result about anything else is not its.
	if len(res.Services) > 1 || slices.ContainsFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Service != "direct" }) ||
		slices.ContainsFunc(res.Services, func(id protocol.DeploymentIdentity) bool { return id.Service != "direct" }) {
		return ErrUnreadableResult
	}
	created := ""
	if len(res.Services) == 1 {
		created = res.Services[0].ContainerID
	}
	raw, err := json.Marshal(storedDeploymentResult{Code: res.Code, Steps: res.Steps, Services: res.Services})
	if err != nil || len(raw) > MaxDeploymentResultStoredBytes {
		return ErrInvalid
	}
	updated, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_commands SET outcome=?, detail=?, result=?, result_container_id=?, settled_at=? WHERE id=? AND endpoint_id=? AND request_id=? AND action IN (?,?) AND (outcome='' OR (outcome=? AND result=''))`),
		res.Outcome, res.Code, string(raw), created, time.Now().UTC(), res.Deployment, endpointID, res.RequestID, ActionRecreate, ActionRun, protocol.OutcomeUnknown)
	if err != nil {
		return err
	}
	if n, err := updated.RowsAffected(); err != nil || n != 1 {
		if err == nil {
			err = ErrNotFound
		}
		return err
	}
	return nil
}

// FailDirectCommand records that a direct command's frame never left the server.
func (t *tenancyStore) FailDirectCommand(ctx context.Context, endpointID, id, detail string) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_commands SET outcome=?, detail=?, settled_at=? WHERE id=? AND endpoint_id=? AND action IN (?,?) AND outcome=''`),
		protocol.OutcomeFailed, protocol.CleanText(detail, protocol.MaxResultDetailBytes), time.Now().UTC(), id, endpointID, ActionRecreate, ActionRun)
	return err
}
