package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/google/uuid"
)

// ApplyDeployment turns a planned row into an applying one and builds the frame the agent
// executes. The values it resolves exist only in the returned request. maxFrameBytes is the
// largest frame the endpoint's agent accepts. See docs/application-schema.md, Deploy.
func (t *tenancyStore) ApplyDeployment(ctx context.Context, a TenantAccess, app, id, confirm string, key []byte, maxFrameBytes int) (*Deployment, *protocol.DeploymentRequest, error) {
	return t.applyDeployment(ctx, a, app, id, confirm, key, maxFrameBytes, nil, nil)
}

// ApplyPolicyDeployment is ApplyDeployment for a policy run: inside the same transaction, after
// the application lock, policy must still exist, be active, in apply mode and act as a.ActorID,
// or the row stays planned and the call is ErrPolicyChanged. The row records run as the policy run
// that applied it, which makes its validation automated; a rollback passes no run.
func (t *tenancyStore) ApplyPolicyDeployment(ctx context.Context, a TenantAccess, policy, run, app, id, confirm string, key []byte, maxFrameBytes int) (*Deployment, *protocol.DeploymentRequest, error) {
	var runID any
	if run != "" {
		runID = run
	}
	return t.applyDeployment(ctx, a, app, id, confirm, key, maxFrameBytes, runID, func(tx *sql.Tx) error {
		lock := ""
		if t.store.driver == "postgres" {
			lock = " FOR UPDATE"
		}
		var status, mode, createdBy string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT status,mode,created_by FROM update_policies WHERE id=? AND organization_id=? AND environment_id=? AND application_id=?`+lock), policy, a.OrganizationID, a.EnvironmentID, app).Scan(&status, &mode, &createdBy)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (status != PolicyActive || mode != PolicyModeApply || createdBy != a.ActorID)) {
			return ErrPolicyChanged
		}
		return err
	})
}

// applyDeployment is ApplyDeployment with guard, when non-nil, run after the application lock, and
// run (a policy run ID or nil) written with the flip to applying.
func (t *tenancyStore) applyDeployment(ctx context.Context, a TenantAccess, app, id, confirm string, key []byte, maxFrameBytes int, run any, guard func(*sql.Tx) error) (*Deployment, *protocol.DeploymentRequest, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" || len(key) != 32 {
		return nil, nil, ErrInvalid
	}
	planID, err := uuid.Parse(id)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	// Every audit row of a deployment carries its plan's correlation ID, the apply's included.
	// A row's ID never changes, so the transaction need not re-read it.
	var correlation string
	if t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT correlation_id FROM deployments WHERE organization_id=? AND environment_id=? AND application_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), planID.String()).Scan(&correlation) == nil && correlation != "" {
		a.CorrelationID = correlation
	}
	var out *Deployment
	var req *protocol.DeploymentRequest
	err = t.withTenantTarget(ctx, a, permissions.ApplicationDeploy, appID.String()+"/deployments/"+planID.String()+"/apply", func(tx *sql.Tx) error {
		// Application row first, the lock order PlanDeployment and ReleaseApplication take.
		lock := ""
		if t.store.driver == "postgres" {
			lock = " FOR UPDATE"
		}
		var head int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`+lock), a.OrganizationID, a.EnvironmentID, appID.String()).Scan(&head); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if guard != nil {
			if err := guard(tx); err != nil {
				return err
			}
		}
		d, err := scanDeployment(tx.QueryRowContext(ctx, t.store.rebind(selectDeployments+`WHERE d.organization_id=? AND d.environment_id=? AND d.application_id=? AND d.id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), planID.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if d.State == "applying" {
			return ErrDeploymentInProgress
		}
		if confirm != d.Plan.Project {
			return ErrInvalid
		}
		if d.Kind != "apply" || d.State != "planned" || d.Expired || d.Revision < 1 || d.Revision > head {
			return ErrAdoptionChanged
		}
		var mappingVersion int
		var endpointState string
		err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.mapping_version,e.state FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.id=? AND i.endpoint_id=? AND i.organization_id=? AND i.environment_id=?`), d.InstanceID, d.EndpointID, a.OrganizationID, a.EnvironmentID).Scan(&mappingVersion, &endpointState)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && mappingVersion != d.MappingVersion) {
			return ErrAdoptionChanged
		}
		if err != nil {
			return err
		}
		if endpointState != "active" {
			return ErrEndpointOffline
		}
		now := time.Now().UTC()
		frame, err := t.buildDeploymentFrame(ctx, tx, a, d, key, now)
		if err != nil {
			return err
		}
		// The agent closes the session on a frame past its bound, so refuse it while the row
		// is still planned rather than send one that can only end unknown.
		if frameBlocker(frame, now, maxFrameBytes) != "" {
			return ErrInvalid
		}
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='applying',applied_by=?,applied_at=?,deadline=?,policy_run_id=? WHERE id=? AND state='planned'`), a.ActorID, now, frame.Deadline, run, d.ID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrAdoptionChanged
		}
		d.State, d.AppliedBy, d.AppliedAt, d.Deadline = "applying", a.ActorID, &now, &frame.Deadline
		out, req = d, &frame
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, req, nil
}

// buildDeploymentFrame resolves plan d into the frame an agent executes, issued at now: the
// revision's environment values and each pulled host's registry credential, decrypted with key
// and held only in the returned request. PlanDeployment measures it and drops it;
// ApplyDeployment sends it. A plan that no longer matches its revision, its adopted resources or
// the registry rows is ErrAdoptionChanged.
func (t *tenancyStore) buildDeploymentFrame(ctx context.Context, tx *sql.Tx, a TenantAccess, d *Deployment, key []byte, now time.Time) (protocol.DeploymentRequest, error) {
	spec, values, digest, err := t.resolveApplicationValues(ctx, tx, a, d.ApplicationID, d.Revision, key)
	if err != nil {
		return protocol.DeploymentRequest{}, err
	}
	if digest != d.SpecDigest || len(spec.Services) != len(d.Plan.Services) {
		return protocol.DeploymentRequest{}, ErrAdoptionChanged
	}
	// A plan saved before correlation IDs has no request ID to send: plan again.
	if d.CorrelationID == "" {
		return protocol.DeploymentRequest{}, ErrAdoptionChanged
	}
	names := map[string]string{}
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name FROM application_resources WHERE instance_id=? AND endpoint_id=?`), d.InstanceID, d.EndpointID)
	if err != nil {
		return protocol.DeploymentRequest{}, err
	}
	for rows.Next() {
		var cid, name string
		if err := rows.Scan(&cid, &name); err != nil {
			rows.Close()
			return protocol.DeploymentRequest{}, err
		}
		names[cid] = name
	}
	if err := rows.Close(); err != nil {
		return protocol.DeploymentRequest{}, err
	}
	if err := rows.Err(); err != nil {
		return protocol.DeploymentRequest{}, err
	}
	req := protocol.DeploymentRequest{Deployment: d.ID, RequestID: d.CorrelationID, Endpoint: d.EndpointID, Project: d.Plan.Project, Revision: d.Revision, IssuedAt: now, Deadline: now.Add(DeploymentApplyDeadline), Services: []protocol.DeploymentService{}, Volumes: d.Plan.Volumes}
	hosts := map[string]bool{}
	for i, ps := range d.Plan.Services {
		name, ok := names[ps.ContainerID]
		if !ok || spec.Services[i].Name != ps.Name {
			return protocol.DeploymentRequest{}, ErrAdoptionChanged
		}
		svc := protocol.DeploymentService{Name: ps.Name, ContainerName: name, ImageID: ps.ImageID, Replaces: ps.Replaces, Restart: ps.Restart, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: append([]protocol.Mount{}, ps.Mounts...)}
		if ps.PullDigest != "" {
			svc.ImageID, svc.Pull = "", &protocol.ImagePull{Reference: ps.PullReference, Digest: ps.PullDigest}
			// The agent moves the service's tag to the pulled image, so the next plan (and
			// Compose on the host) resolves the tag to the update rather than reverting it.
			ref, err := registry.ParseReference(ps.Reference)
			if err != nil {
				return protocol.DeploymentRequest{}, ErrAdoptionChanged
			}
			if ref.Digest == "" {
				svc.Pull.Tag = ref.Host + "/" + ref.Repository + ":" + ref.Tag
			}
			hosts[svc.Pull.Host()] = true
		}
		for _, p := range ps.Ports {
			svc.Ports = append(svc.Ports, protocol.Port{Container: p.Target, Host: p.Published, Protocol: p.Protocol, HostIP: p.HostIP})
		}
		for envName, ref := range spec.Services[i].Environment {
			svc.Env[envName] = values[ref.SecretRef]
		}
		req.Services = append(req.Services, svc)
	}
	// A credential travels once per host, decrypted here and never stored. A host whose row is
	// gone pulls anonymously only while the organization still allows it.
	for host := range hosts {
		_, cred, err := t.registryFor(ctx, tx, a.OrganizationID, host, key)
		if errors.Is(err, ErrNotFound) {
			anonymous, err := t.anonymousPull(ctx, tx, a.OrganizationID)
			if err != nil {
				return protocol.DeploymentRequest{}, err
			}
			if !anonymous {
				return protocol.DeploymentRequest{}, ErrAdoptionChanged
			}
			continue
		}
		if err != nil {
			return protocol.DeploymentRequest{}, err
		}
		if cred != nil {
			if req.Registries == nil {
				req.Registries = map[string]protocol.RegistryAuth{}
			}
			req.Registries[host] = protocol.RegistryAuth{Username: cred.Username, Secret: cred.Secret}
		}
	}
	return req, nil
}

// frameBlocker names what stops req reaching an agent that accepts maxFrameBytes, or "" when
// nothing does. Plan and apply both run it: capabilities can change between them.
func frameBlocker(req protocol.DeploymentRequest, now time.Time, maxFrameBytes int) string {
	if len(req.Registries) > protocol.MaxRegistryAuthHosts {
		return "too_many_registry_hosts"
	}
	if req.Validate(now) != nil {
		return "frame_invalid"
	}
	if raw, err := json.Marshal(req); err != nil || len(raw) > min(maxFrameBytes, protocol.MaxDeploymentRequestBytes) {
		return "frame_too_large"
	}
	return ""
}

// checkFrame builds the frame apply would send for d and refuses the plan when the endpoint's
// agent could not accept it. The frame, values and credentials included, is dropped.
func (t *tenancyStore) checkFrame(ctx context.Context, tx *sql.Tx, a TenantAccess, d *Deployment, key []byte, maxFrameBytes int) error {
	now := time.Now().UTC()
	req, err := t.buildDeploymentFrame(ctx, tx, a, d, key, now)
	if err != nil {
		return err
	}
	if b := frameBlocker(req, now, maxFrameBytes); b != "" {
		return blocked([]string{b})
	}
	return nil
}

// FailDeployment records that the frame never left the server. An unknown row qualifies too:
// a disconnect may have abandoned it first, but the server knows nothing reached the agent.
func (t *tenancyStore) FailDeployment(ctx context.Context, id, detail string) error {
	_, err := t.systemTransition(ctx, `id=? AND state IN ('applying','unknown')`, id, `state='failed',detail=?,settled_at=?`, []any{protocol.CleanText(detail, 255), time.Now().UTC()}, "not_sent", "failure")
	return err
}

// systemTransition moves the deployments matching filter with set, one audited row each under
// the system actor (a removal's under application.destroy). It selects first and updates each
// row under the same filter, so a row a concurrent settle took is neither changed nor audited.
func (t *tenancyStore) systemTransition(ctx context.Context, filter string, arg any, set string, setArgs []any, outcome, result string) (int64, error) {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	type row struct{ id, org, env, app, kind, correlation string }
	var affected []row
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,organization_id,environment_id,application_id,kind,correlation_id FROM deployments WHERE `+filter), arg)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.org, &r.env, &r.app, &r.kind, &r.correlation); err != nil {
			rows.Close()
			return 0, err
		}
		affected = append(affected, r)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var total int64
	for _, r := range affected {
		args := append(append([]any{}, setArgs...), r.id, arg)
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET `+set+` WHERE id=? AND `+filter), args...)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n != 1 {
			continue
		}
		action := permissions.ApplicationDeploy
		if r.kind == "remove" {
			action = permissions.ApplicationDestroy
		}
		if err := t.auditDeployment(ctx, tx, "system", action, r.org, r.env, r.app, r.id, r.correlation, outcome, result, now); err != nil {
			return 0, err
		}
		total++
	}
	return total, tx.Commit()
}

// auditResults maps a deployment outcome onto the audit table's result vocabulary; the
// outcome itself goes in the details.
var auditResults = map[string]string{protocol.OutcomeSucceeded: "success", protocol.OutcomeFailed: "failure", protocol.OutcomeTimedOut: "failure", protocol.OutcomeDenied: "denied", protocol.OutcomeUnknown: "unknown"}

// settleable selects a deployment row d that may still take a result: applying, or unknown
// while its instance exists and no newer row for it has been applied or settled.
const settleable = `(d.state='applying' OR (d.state='unknown' AND EXISTS (SELECT 1 FROM application_instances i WHERE i.id=d.instance_id) AND NOT EXISTS (SELECT 1 FROM deployments n WHERE n.instance_id=d.instance_id AND n.created_at>d.created_at AND n.state<>'planned')))`

// lockDeploymentApplication takes the application row lock before the deployment row, the
// order ApplyDeployment and PlanDeployment use, so a late result cannot interleave with an
// apply of a newer plan. It returns the row-lock suffix for the deployment select. PostgreSQL
// only: SQLite already serializes writers.
func (t *tenancyStore) lockDeploymentApplication(ctx context.Context, tx *sql.Tx, endpointID, id string) (string, error) {
	if t.store.driver != "postgres" {
		return "", nil
	}
	var app string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT a.id FROM applications a JOIN deployments d ON d.application_id=a.id WHERE d.id=? AND d.endpoint_id=? FOR UPDATE OF a`), id, endpointID).Scan(&app)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return " FOR UPDATE", nil
}

// SettleDeployment is the one writer for outcomes, scoped to the endpoint that ran it. An
// applying row always settles. An unknown row (abandoned or swept) settles only while its
// instance exists and no newer row for it has been applied or settled, so a late answer never
// rewrites state someone has since acted on; a newer plan, built against the containers the
// late answer replaces, is expired instead. Anything else is ErrNotFound. A result from a binary
// built before codes is read through legacyResult; one that still fails validation is
// ErrUnreadableResult with nothing written, and one echoing another deployment's request ID is
// ErrInvalid.
func (t *tenancyStore) SettleDeployment(ctx context.Context, endpointID string, res protocol.DeploymentResult) error {
	res = legacyResult(res)
	if res.Validate() != nil {
		return ErrUnreadableResult
	}
	raw, err := json.Marshal(storedDeploymentResult{Code: res.Code, Steps: res.Steps, Services: res.Services})
	if err != nil || len(raw) > MaxDeploymentResultStoredBytes {
		return ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lock, err := t.lockDeploymentApplication(ctx, tx, endpointID, res.Deployment)
	if err != nil {
		return err
	}
	var org, env, appID, instance, kind, planRaw, correlation string
	var revision int
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT d.organization_id,d.environment_id,d.application_id,d.instance_id,d.kind,d.revision,d.plan,d.correlation_id FROM deployments d WHERE d.id=? AND d.endpoint_id=? AND `+settleable+lock), res.Deployment, endpointID).Scan(&org, &env, &appID, &instance, &kind, &revision, &planRaw, &correlation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// A current agent echoes the frame's request ID; another deployment's is a replay or a bug.
	if res.RequestID != "" && res.RequestID != correlation {
		return ErrInvalid
	}
	var plan DeploymentPlan
	if json.Unmarshal([]byte(planRaw), &plan) != nil {
		return ErrRevisionCorrupt
	}
	action := permissions.ApplicationDeploy
	if kind == "remove" {
		action = permissions.ApplicationDestroy
		if err := t.settleRemoval(ctx, tx, endpointID, appID, instance, plan, res); err != nil {
			return err
		}
	} else if err := t.settleApply(ctx, tx, endpointID, instance, plan, res); err != nil {
		return err
	}
	now := time.Now().UTC()
	updated, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state=?,detail='',result=?,settled_at=? WHERE id=? AND state IN ('applying','unknown')`), res.Outcome, string(raw), now, res.Deployment)
	if err != nil {
		return err
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if kind == "apply" && res.Outcome == protocol.OutcomeSucceeded {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_instances SET previous_revision=current_revision,current_revision=? WHERE id=?`), revision, instance); err != nil {
			return err
		}
		// Every succeeded apply is validated, opened in the transaction that settles it.
		if err := t.insertValidation(ctx, tx, org, env, appID, instance, endpointID, res.Deployment, correlation, now); err != nil {
			return err
		}
	}
	// Only an unknown row can have a newer plan (a live one refuses planning).
	if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET expires_at=? WHERE instance_id=? AND state='planned' AND created_at>(SELECT created_at FROM deployments WHERE id=?)`), now, instance, res.Deployment); err != nil {
		return err
	}
	// In the same transaction: a settled row without its audit row cannot exist.
	if err := t.auditDeployment(ctx, tx, "agent:"+endpointID, action, org, env, appID, res.Deployment, correlation, res.Outcome, auditResults[res.Outcome], now); err != nil {
		return err
	}
	return tx.Commit()
}

// settleApply rebinds each replaced service to the container the agent reports. Each identity
// names a distinct planned service and runs its pinned image, or for a pulled service a full
// image ID pulled at the planned digest; a success accounts for every planned service. Checked
// in full before any write.
func (t *tenancyStore) settleApply(ctx context.Context, tx *sql.Tx, endpointID, instance string, plan DeploymentPlan, res protocol.DeploymentResult) error {
	replaced := map[string]PlannedService{}
	for _, ps := range plan.Services {
		replaced[ps.Name] = ps
	}
	seen := map[string]bool{}
	for _, idn := range res.Services {
		ps, ok := replaced[idn.Service]
		if !ok || seen[idn.Service] {
			return ErrInvalid
		}
		if ps.PullDigest != "" {
			// res.Validate already required a full ImageID.
			if idn.ImageDigest != ps.PullDigest {
				return ErrInvalid
			}
		} else if idn.ImageID != ps.ImageID {
			return ErrInvalid
		}
		seen[idn.Service] = true
	}
	if res.Outcome == protocol.OutcomeSucceeded && len(seen) != len(plan.Services) {
		return ErrInvalid
	}
	for _, idn := range res.Services {
		ps := replaced[idn.Service]
		var name string
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT name FROM application_resources WHERE instance_id=? AND endpoint_id=? AND container_id=?`), instance, endpointID, ps.ContainerID).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAdoptionChanged
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM application_resources WHERE instance_id=? AND endpoint_id=? AND container_id=?`), instance, endpointID, ps.ContainerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_resources(instance_id,endpoint_id,container_id,name,image_id,created_at,service_name) VALUES(?,?,?,?,?,?,?)`), instance, endpointID, idn.ContainerID, name, idn.ImageID, time.Unix(idn.CreatedUnix, 0).UTC(), idn.Service); err != nil {
			return err
		}
	}
	// The check described the images this apply replaced.
	if res.Outcome == protocol.OutcomeSucceeded {
		return t.clearImageChecks(ctx, tx, instance)
	}
	return nil
}

// settleRemoval forgets each target the steps show is off the host: its precondition matched
// the pinned identity (or found it gone) and then its remove step succeeded, or stop and remove
// were skipped because it was already gone. Only a
// success that accounts for every target releases the instance and marks the application
// removed; anything less keeps the rest adopted. Checked in full before any write.
func (t *tenancyStore) settleRemoval(ctx context.Context, tx *sql.Tx, endpointID, appID, instance string, plan DeploymentPlan, res protocol.DeploymentResult) error {
	if len(res.Services) > 0 {
		return ErrInvalid
	}
	targets := map[string]string{}
	for _, c := range plan.Containers {
		targets[c.Service] = c.ContainerID
	}
	outcomes := map[string]map[string]string{}
	for _, s := range res.Steps {
		if _, ok := targets[s.Service]; !ok || (s.Step != protocol.StepPrecondition && s.Step != protocol.StepStop && s.Step != protocol.StepRemove) {
			return ErrInvalid
		}
		if outcomes[s.Service] == nil {
			outcomes[s.Service] = map[string]string{}
		}
		if _, dup := outcomes[s.Service][s.Step]; dup {
			return ErrInvalid
		}
		outcomes[s.Service][s.Step] = s.Outcome
	}
	gone := []string{}
	for service, container := range targets {
		o := outcomes[service]
		if o[protocol.StepPrecondition] == protocol.OutcomeSucceeded && (o[protocol.StepRemove] == protocol.OutcomeSucceeded || (o[protocol.StepStop] == protocol.OutcomeSkipped && o[protocol.StepRemove] == protocol.OutcomeSkipped)) {
			gone = append(gone, container)
		}
	}
	complete := len(gone) == len(targets)
	if res.Outcome == protocol.OutcomeSucceeded && !complete {
		return ErrInvalid
	}
	for _, container := range gone {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM application_resources WHERE instance_id=? AND endpoint_id=? AND container_id=?`), instance, endpointID, container); err != nil {
			return err
		}
	}
	if res.Outcome != protocol.OutcomeSucceeded {
		return nil
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM application_resources WHERE instance_id=?`), instance); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM application_instances WHERE id=?`), instance); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE applications SET removed_at=? WHERE id=?`), time.Now().UTC(), appID)
	return err
}

// auditDeployment writes the audit row for a deployment transition inside its transaction, under
// the deployment's correlation ID (a fresh one for a row planned before correlation IDs).
func (t *tenancyStore) auditDeployment(ctx context.Context, tx *sql.Tx, user string, action permissions.Action, org, env, appID, id, correlation, outcome, result string, at time.Time) error {
	if correlation == "" {
		correlation = uuid.NewString()
	}
	_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), user, string(action), protocol.CleanText(appID+"/deployments/"+id, 255), "outcome="+outcome, at, "organization", org, env, correlation, result)
	return err
}

// RefuseDeploymentResult records that the endpoint answered with a result that does not fit
// the plan: the host may have acted, so the row becomes unknown with the caller's fixed detail.
// It selects rows as SettleDeployment does, and a repeat (already unknown with this detail)
// changes nothing and writes no audit row, so an agent re-sending the result cannot grow the log.
func (t *tenancyStore) RefuseDeploymentResult(ctx context.Context, endpointID, id, detail string) error {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lock, err := t.lockDeploymentApplication(ctx, tx, endpointID, id)
	if err != nil {
		return err
	}
	var org, env, appID, kind, correlation string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT d.organization_id,d.environment_id,d.application_id,d.kind,d.correlation_id FROM deployments d WHERE d.id=? AND d.endpoint_id=? AND `+settleable+lock), id, endpointID).Scan(&org, &env, &appID, &kind, &correlation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	detail = protocol.CleanText(detail, 255)
	updated, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='unknown',detail=? WHERE id=? AND (state='applying' OR (state='unknown' AND detail<>?))`), detail, id, detail)
	if err != nil {
		return err
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return nil
	}
	now := time.Now().UTC()
	action := permissions.ApplicationDeploy
	if kind == "remove" {
		action = permissions.ApplicationDestroy
	}
	if err := t.auditDeployment(ctx, tx, "agent:"+endpointID, action, org, env, appID, id, correlation, "refused", auditResults[protocol.OutcomeUnknown], now); err != nil {
		return err
	}
	return tx.Commit()
}

// AbandonDeployments marks what was applying when the connection ended as unknown. The
// agent keeps the real answer and re-sends it on reconnect.
func (t *tenancyStore) AbandonDeployments(ctx context.Context, endpointID string) (int64, error) {
	return t.systemTransition(ctx, `endpoint_id=? AND state='applying'`, endpointID, `state='unknown',detail=?`, []any{"the connection ended before a result arrived"}, "abandoned", auditResults[protocol.OutcomeUnknown])
}

type RemovalBody struct {
	InstanceID string `json:"instance_id"`
	Confirm    string `json:"confirm"`
}

// RemoveApplication starts stopping and deleting every adopted container of the instance. The
// row goes straight to applying: there is nothing to review beyond the typed project name, and
// the frame names each container by its pinned identity. Volumes, networks and images stay.
// It refuses while a fresh inventory shows an unadopted container of the project, or the last
// apply's outcome is unknown: releasing the instance then would orphan a running container.
// See docs/application-schema.md, Delete semantics.
func (t *tenancyStore) RemoveApplication(ctx context.Context, a TenantAccess, app string, r RemovalBody) (*Deployment, *protocol.RemovalRequest, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, nil, ErrInvalid
	}
	instanceID, err := uuid.Parse(r.InstanceID)
	if err != nil {
		return nil, nil, ErrAdoptionChanged
	}
	// The removal's request ID is its correlation ID, as a plan's is.
	if a.CorrelationID == "" {
		a.CorrelationID = uuid.NewString()
	}
	id := uuid.NewString()
	var out *Deployment
	var req *protocol.RemovalRequest
	var details string
	err = t.withTenantTargetDetails(ctx, a, permissions.ApplicationDestroy, appID.String()+"/deployments/"+id, &details, func(tx *sql.Tx) error {
		// Application row first, the lock order every deployment writer takes.
		lock := ""
		if t.store.driver == "postgres" {
			lock = " FOR UPDATE"
		}
		var head int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`+lock), a.OrganizationID, a.EnvironmentID, appID.String()).Scan(&head); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var project, endpoint, endpointName, endpointState string
		var mappingVersion int
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.project,i.endpoint_id,i.mapping_version,e.name,e.state FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND i.environment_id=? AND i.application_id=? AND i.id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), instanceID.String()).Scan(&project, &endpoint, &mappingVersion, &endpointName, &endpointState)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAdoptionChanged
		}
		if err != nil {
			return err
		}
		if r.Confirm != project {
			return ErrInvalid
		}
		if endpointState != "active" {
			return ErrEndpointOffline
		}
		var liveState string
		var expiresAt time.Time
		err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT state,expires_at FROM deployments WHERE organization_id=? AND environment_id=? AND instance_id=? AND state IN ('planned','applying')`), a.OrganizationID, a.EnvironmentID, instanceID.String()).Scan(&liveState, &expiresAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if liveState == "applying" {
			return ErrDeploymentInProgress
		}
		if liveState == "planned" && expiresAt.After(time.Now().UTC()) {
			return ErrDeploymentPlanned
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM deployments WHERE organization_id=? AND environment_id=? AND instance_id=? AND state='planned'`), a.OrganizationID, a.EnvironmentID, instanceID.String()); err != nil {
			return err
		}
		var digest string
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT digest FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), a.OrganizationID, a.EnvironmentID, appID.String(), head).Scan(&digest); err != nil {
			return err
		}
		now := time.Now().UTC()
		plan := DeploymentPlan{Project: project, Services: []PlannedService{}, Containers: []RemovalPlanTarget{}}
		req = &protocol.RemovalRequest{Deployment: id, RequestID: a.CorrelationID, Endpoint: endpoint, Project: project, IssuedAt: now, Deadline: now.Add(DeploymentApplyDeadline), Containers: []protocol.RemovalTarget{}}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name,image_id,created_at,service_name FROM application_resources WHERE instance_id=? AND endpoint_id=? ORDER BY container_id`), instanceID.String(), endpoint)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c RemovalPlanTarget
			var created time.Time
			if err := rows.Scan(&c.ContainerID, &c.Name, &c.ImageID, &created, &c.Service); err != nil {
				rows.Close()
				return err
			}
			// Whole seconds: the precision the runtime reports, as InspectionTarget uses.
			c.CreatedUnix = created.Unix()
			if c.Service == "" {
				c.Service = "unmapped-" + c.ContainerID[:min(12, len(c.ContainerID))]
			}
			plan.Containers = append(plan.Containers, c)
			req.Containers = append(req.Containers, protocol.RemovalTarget{Service: c.Service, Target: protocol.InspectionTarget{ContainerID: c.ContainerID, ImageID: c.ImageID, CreatedUnix: c.CreatedUnix}})
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(plan.Containers) == 0 {
			return ErrAdoptionChanged
		}
		if len(plan.Containers) > protocol.MaxRemovalTargets {
			return ErrRemovalTooLarge
		}
		if err := t.removalBlockers(ctx, tx, endpoint, instanceID.String(), project, plan.Containers); err != nil {
			return err
		}
		if req.Validate(now) != nil {
			return ErrInvalid
		}
		if raw, err := json.Marshal(req); err != nil || len(raw) > protocol.MaxDeploymentRequestBytes {
			return ErrInvalid
		}
		raw, err := json.Marshal(plan)
		if err != nil || len(raw) > MaxDeploymentPlanBytes {
			return ErrInvalid
		}
		out = &Deployment{ID: id, ApplicationID: appID.String(), InstanceID: instanceID.String(), EndpointID: endpoint, EndpointName: endpointName, Kind: "remove", State: "applying", Revision: head, SpecDigest: digest, MappingVersion: mappingVersion, Plan: plan, CreatedBy: a.ActorID, CreatedAt: now, ExpiresAt: req.Deadline, AppliedBy: a.ActorID, AppliedAt: &now, Deadline: &req.Deadline, CorrelationID: a.CorrelationID}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,kind,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at,applied_by,applied_at,deadline,correlation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), out.ID, a.OrganizationID, a.EnvironmentID, out.ApplicationID, out.InstanceID, out.EndpointID, project, out.Kind, out.State, out.Revision, out.SpecDigest, out.MappingVersion, string(raw), out.CreatedBy, now, out.ExpiresAt, out.AppliedBy, now, req.Deadline, out.CorrelationID)
		details = fmt.Sprintf("project=%s endpoint=%s containers=%d", project, endpoint, len(plan.Containers))
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return out, req, nil
}

// removalBlockers refuses a removal the host may not survive: a fresh inventory must show no
// container of the project outside the adopted ones, and the instance's last acted-on row must
// not be an apply of unknown outcome, which may have left a replacement KyYard never recorded.
func (t *tenancyStore) removalBlockers(ctx context.Context, tx *sql.Tx, endpoint, instance, project string, adopted []RemovalPlanTarget) error {
	var state, raw string
	var received, observed time.Time
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.state,v.snapshot,v.received_at,v.observed_at FROM endpoints e JOIN endpoint_inventory v ON v.endpoint_id=e.id WHERE e.id=?`), endpoint).Scan(&state, &raw, &received, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAdoptionChanged
	}
	if err != nil {
		return err
	}
	snapshot, _, err := freshInventory(state, raw, received, observed)
	if err != nil {
		return err
	}
	owned := map[string]bool{}
	for _, c := range adopted {
		owned[c.ContainerID] = true
	}
	blockers := []string{}
	for _, c := range snapshot.Containers {
		if c.ComposeProject == project && !owned[c.ID] {
			blockers = append(blockers, "unadopted_project_containers")
			break
		}
	}
	var kind, last string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT kind,state FROM deployments WHERE instance_id=? AND state<>'planned' ORDER BY created_at DESC,id DESC LIMIT 1`), instance).Scan(&kind, &last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if kind == "apply" && last == "unknown" {
		blockers = append(blockers, "apply_outcome_unknown")
	}
	if len(blockers) > 0 {
		return &PreflightBlockedError{Blockers: blockers}
	}
	return nil
}
