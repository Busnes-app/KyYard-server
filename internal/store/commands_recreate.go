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
	// AllowPrivileged is KY_CONTAINER_ALLOW_PRIVILEGED: without it a host-level setting is refused.
	AllowPrivileged bool
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
		if err := t.sweepDirect(ctx, tx, endpointID, now); err != nil {
			return err
		}
		snap, old, err := t.directTarget(ctx, tx, endpointID, dc, svc, now)
		if err != nil {
			return err
		}
		if err := directBlockers(snap, old, svc, dc.AllowPrivileged); err != nil {
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
	if !directActions[dc.Action] || len(dc.Frame.Services) != 1 || dc.Frame.Services[0].Explicit == nil {
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
		if _, _, err = t.directTarget(ctx, tx, endpointID, dc, &dc.Frame.Services[0], time.Now().UTC()); err != nil {
			return err
		}
		if !dc.AllowPrivileged && hostLevel(&dc.Frame.Services[0]) {
			return invalidSpec("privileged_disabled")
		}
		return nil
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
// already grant is acknowledged. An old bind grants a new one at the same source and target,
// read-only unless the old one was writable: the agent's rule.
func directBlockers(snap protocol.Snapshot, old *protocol.Container, svc *protocol.DeploymentService, allowPrivileged bool) error {
	var blockers []string
	if !allowPrivileged && hostLevel(svc) {
		blockers = append(blockers, "privileged_disabled")
	}
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
			return o.Kind == protocol.MountBind && o.Source == m.Source && o.Target == m.Target && (!o.ReadOnly || m.ReadOnly)
		}) {
			blockers = append(blockers, "bind_unacknowledged")
		}
	}
	if len(blockers) > 0 {
		return invalidSpec(blockers...)
	}
	return nil
}

// dockerDefaultCaps is the capability set Docker grants a container that adds none.
var dockerDefaultCaps = []string{"CHOWN", "DAC_OVERRIDE", "FSETID", "FOWNER", "MKNOD", "NET_RAW", "SETGID", "SETUID", "SETFCAP", "SETPCAP", "NET_BIND_SERVICE", "SYS_CHROOT", "KILL", "AUDIT_WRITE"}

// hostPaths are host paths a bind reaches only with the opt-in: the path and every parent, and
// for all but / and /etc (whose files, like /etc/localtime, containers commonly read) anything
// beneath it too. /var/run is /run on every current distribution.
var hostPaths = []string{"/", "/etc", "/run", "/var/run", "/var/spool", "/var/lib", "/usr", "/lib", "/lib64", "/bin", "/sbin", "/proc", "/sys", "/dev", "/boot", "/root"}

// hardening are the security_opt entries that only take privilege away.
var hardening = []string{"no-new-privileges", "no-new-privileges:true"}

// hostLevel reports a setting that reaches past the container into the host, which
// KY_CONTAINER_ALLOW_PRIVILEGED gates: privileged, devices, a security option other than
// hardening, capabilities beyond Docker's defaults, the host's or another container's network
// (as the mode or an attachment), or a bind of hostPaths.
func hostLevel(svc *protocol.DeploymentService) bool {
	e := svc.Explicit
	if e.Privileged || len(e.Devices) > 0 || protocol.NamespaceNetwork(e.NetworkMode) ||
		slices.ContainsFunc(e.Networks, func(n protocol.NetworkAttachmentSpec) bool { return protocol.NamespaceNetwork(n.Name) }) ||
		slices.ContainsFunc(e.SecurityOpt, func(o string) bool { return !slices.Contains(hardening, o) }) {
		return true
	}
	if slices.ContainsFunc(e.CapAdd, func(c string) bool {
		return !slices.Contains(dockerDefaultCaps, strings.TrimPrefix(strings.ToUpper(c), "CAP_"))
	}) {
		return true
	}
	under := func(p, dir string) bool { return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/") }
	return slices.ContainsFunc(svc.Mounts, func(m protocol.Mount) bool {
		return m.Kind == protocol.MountBind && slices.ContainsFunc(hostPaths, func(h string) bool {
			return under(h, m.Source) || (h != "/" && h != "/etc" && under(m.Source, h))
		})
	})
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
	return t.settleDirect(ctx, endpointID, res.Deployment, res.Outcome, res.Code, created, `, result=?, result_container_id=?`, []any{string(raw), created},
		`request_id=? AND (outcome='' OR (outcome=? AND result=''))`, []any{res.RequestID, protocol.OutcomeUnknown})
}

// FailDirectCommand records that a direct command's frame never left the server. A command
// already settled is left alone.
func (t *tenancyStore) FailDirectCommand(ctx context.Context, endpointID, id, detail string) error {
	err := t.settleDirect(ctx, endpointID, id, protocol.OutcomeFailed, protocol.CleanText(detail, protocol.MaxResultDetailBytes), "", "", nil, `outcome=''`, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// settleDirect records a direct command's outcome and, in the same transaction, its audit row
// (auditDirect). set adds assignments (", col=?") and where conditions to the update; it returns
// ErrNotFound when no row matched.
func (t *tenancyStore) settleDirect(ctx context.Context, endpointID, id, outcome, code, created, set string, setArgs []any, where string, whereArgs []any) error {
	tx, err := t.store.beginTx(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	args := append([]any{outcome, code, now}, setArgs...)
	args = append(append(args, id, endpointID, ActionRecreate, ActionRun), whereArgs...)
	updated, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_commands SET outcome=?, detail=?, settled_at=?`+set+` WHERE id=? AND endpoint_id=? AND action IN (?,?) AND `+where), args...)
	if err != nil {
		return err
	}
	if n, err := updated.RowsAffected(); err != nil || n != 1 {
		if err == nil {
			err = ErrNotFound
		}
		return err
	}
	if err := t.auditDirect(ctx, tx, endpointID, id, outcome, code, created, now); err != nil {
		return err
	}
	return tx.Commit()
}

// sweepDirect settles the endpoint's direct commands past their deadline unknown, each with its
// outcome audit row (code deadline).
func (t *tenancyStore) sweepDirect(ctx context.Context, tx *sql.Tx, endpointID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id FROM endpoint_commands WHERE endpoint_id=? AND action IN (?,?) AND outcome='' AND deadline<?`), endpointID, ActionRecreate, ActionRun, now)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoint_commands SET outcome=?, detail=?, settled_at=? WHERE id=?`),
			protocol.OutcomeUnknown, "no result arrived before the deadline", now, id); err != nil {
			return err
		}
		if err := t.auditDirect(ctx, tx, endpointID, id, protocol.OutcomeUnknown, "deadline", "", now); err != nil {
			return err
		}
	}
	return nil
}

// auditDirect writes a direct command's outcome row under its correlation ID and actor: action
// container.recreate or container.run, resource endpoint/container (the replaced one, else the
// created one), details the code and the new container's ID.
func (t *tenancyStore) auditDirect(ctx context.Context, tx *sql.Tx, endpointID, id, outcome, code, created string, now time.Time) error {
	var actor, org, env, correlation, action, container string
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT actor_id,organization_id,environment_id,request_id,action,container_id FROM endpoint_commands WHERE id=?`), id).Scan(&actor, &org, &env, &correlation, &action, &container); err != nil {
		return err
	}
	resource := endpointID + "/" + cmp.Or(container, created, "-")
	details := "code=" + cmp.Or(code, "-") + " new=" + cmp.Or(created, "-")
	_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`),
		actor, action, protocol.CleanText(resource, 255), protocol.CleanText(details, 255), now, "organization", org, env, correlation, auditResults[outcome])
	return err
}
