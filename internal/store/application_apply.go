package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// ApplyDeployment turns a planned row into an applying one and builds the frame the agent
// executes. The values it resolves exist only in the returned request. See
// docs/application-schema.md, Deploy.
func (t *tenancyStore) ApplyDeployment(ctx context.Context, a TenantAccess, app, id, confirm string, key []byte) (*Deployment, *protocol.DeploymentRequest, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" || len(key) != 32 {
		return nil, nil, ErrInvalid
	}
	planID, err := uuid.Parse(id)
	if err != nil {
		return nil, nil, ErrNotFound
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
		spec, values, digest, err := t.resolveApplicationValues(ctx, tx, a, appID.String(), d.Revision, key)
		if err != nil {
			return err
		}
		if digest != d.SpecDigest || len(spec.Services) != len(d.Plan.Services) {
			return ErrAdoptionChanged
		}
		names := map[string]string{}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name FROM application_resources WHERE instance_id=? AND endpoint_id=?`), d.InstanceID, d.EndpointID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var cid, name string
			if err := rows.Scan(&cid, &name); err != nil {
				rows.Close()
				return err
			}
			names[cid] = name
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		now := time.Now().UTC()
		req = &protocol.DeploymentRequest{Deployment: d.ID, Endpoint: d.EndpointID, Project: d.Plan.Project, Revision: d.Revision, Deadline: now.Add(DeploymentApplyDeadline), Services: []protocol.DeploymentService{}}
		for i, ps := range d.Plan.Services {
			name, ok := names[ps.ContainerID]
			if !ok || spec.Services[i].Name != ps.Name {
				return ErrAdoptionChanged
			}
			svc := protocol.DeploymentService{Name: ps.Name, ContainerName: name, ImageID: ps.ImageID, Replaces: ps.Replaces, Restart: ps.Restart, Ports: []protocol.Port{}, Env: map[string]string{}}
			for _, p := range ps.Ports {
				svc.Ports = append(svc.Ports, protocol.Port{Container: p.Target, Host: p.Published, Protocol: p.Protocol, HostIP: p.HostIP})
			}
			for envName, ref := range spec.Services[i].Environment {
				svc.Env[envName] = values[ref.SecretRef]
			}
			req.Services = append(req.Services, svc)
		}
		if err := req.Validate(now); err != nil {
			return ErrInvalid
		}
		// The agent closes the session on a frame past its bound, so refuse it while the row
		// is still planned rather than send one that can only end unknown.
		if raw, err := json.Marshal(req); err != nil || len(raw) > protocol.MaxDeploymentRequestBytes {
			return ErrInvalid
		}
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='applying',applied_by=?,applied_at=?,deadline=? WHERE id=? AND state='planned'`), a.ActorID, now, req.Deadline, d.ID)
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
		d.State, d.AppliedBy, d.AppliedAt, d.Deadline = "applying", a.ActorID, &now, &req.Deadline
		out = d
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, req, nil
}

// FailDeployment records that the frame never left the server. An unknown row qualifies too:
// a disconnect may have abandoned it first, but the server knows nothing reached the agent.
func (t *tenancyStore) FailDeployment(ctx context.Context, id, detail string) error {
	_, err := t.systemTransition(ctx, `id=? AND state IN ('applying','unknown')`, id, `state='failed',detail=?,settled_at=?`, []any{protocol.CleanText(detail, 255), time.Now().UTC()}, "not_sent", "failure")
	return err
}

// systemTransition moves the deployments matching filter with set, one audited row each under
// the system actor. It selects first and updates each row under the same filter, so a row a
// concurrent settle took is neither changed nor audited.
func (t *tenancyStore) systemTransition(ctx context.Context, filter string, arg any, set string, setArgs []any, outcome, result string) (int64, error) {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	type row struct{ id, org, env, app string }
	var affected []row
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,organization_id,environment_id,application_id FROM deployments WHERE `+filter), arg)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.org, &r.env, &r.app); err != nil {
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
		if err := t.auditDeployment(ctx, tx, "system", permissions.ApplicationDeploy, r.org, r.env, r.app, r.id, outcome, result, now); err != nil {
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
// late answer replaces, is expired instead. Anything else is ErrNotFound.
func (t *tenancyStore) SettleDeployment(ctx context.Context, endpointID string, res protocol.DeploymentResult) error {
	if res.Validate() != nil {
		return ErrInvalid
	}
	raw, err := json.Marshal(storedDeploymentResult{Steps: res.Steps, Services: res.Services})
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
	var org, env, appID, instance, kind, planRaw string
	var revision int
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT d.organization_id,d.environment_id,d.application_id,d.instance_id,d.kind,d.revision,d.plan FROM deployments d WHERE d.id=? AND d.endpoint_id=? AND `+settleable+lock), res.Deployment, endpointID).Scan(&org, &env, &appID, &instance, &kind, &revision, &planRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
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
	updated, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state=?,detail=?,result=?,settled_at=? WHERE id=? AND state IN ('applying','unknown')`), res.Outcome, protocol.CleanText(res.Detail, 255), string(raw), now, res.Deployment)
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
	}
	// Only an unknown row can have a newer plan (a live one refuses planning).
	if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET expires_at=? WHERE instance_id=? AND state='planned' AND created_at>(SELECT created_at FROM deployments WHERE id=?)`), now, instance, res.Deployment); err != nil {
		return err
	}
	// In the same transaction: a settled row without its audit row cannot exist.
	if err := t.auditDeployment(ctx, tx, "agent:"+endpointID, action, org, env, appID, res.Deployment, res.Outcome, auditResults[res.Outcome], now); err != nil {
		return err
	}
	return tx.Commit()
}

// settleApply rebinds each replaced service to the container the agent reports. Each identity
// names a distinct planned service and runs its pinned image; a success accounts for every
// planned service. Checked in full before any write.
func (t *tenancyStore) settleApply(ctx context.Context, tx *sql.Tx, endpointID, instance string, plan DeploymentPlan, res protocol.DeploymentResult) error {
	replaced := map[string]PlannedService{}
	for _, ps := range plan.Services {
		replaced[ps.Name] = ps
	}
	seen := map[string]bool{}
	for _, idn := range res.Services {
		ps, ok := replaced[idn.Service]
		if !ok || seen[idn.Service] || idn.ImageID != ps.ImageID {
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
	return nil
}

// settleRemoval forgets each target the steps show is off the host: its remove step
// succeeded, or its precondition found it already gone (stop and remove skipped). Only a
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
		if o[protocol.StepRemove] == protocol.OutcomeSucceeded || (o[protocol.StepPrecondition] == protocol.OutcomeSucceeded && o[protocol.StepStop] == protocol.OutcomeSkipped && o[protocol.StepRemove] == protocol.OutcomeSkipped) {
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

// auditDeployment writes the audit row for a deployment transition inside its transaction.
func (t *tenancyStore) auditDeployment(ctx context.Context, tx *sql.Tx, user string, action permissions.Action, org, env, appID, id, outcome, result string, at time.Time) error {
	_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), user, string(action), protocol.CleanText(appID+"/deployments/"+id, 255), "outcome="+outcome, at, "organization", org, env, uuid.NewString(), result)
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
	var org, env, appID string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT d.organization_id,d.environment_id,d.application_id FROM deployments d WHERE d.id=? AND d.endpoint_id=? AND `+settleable+lock), id, endpointID).Scan(&org, &env, &appID)
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
	if err := t.auditDeployment(ctx, tx, "agent:"+endpointID, permissions.ApplicationDeploy, org, env, appID, id, "refused", auditResults[protocol.OutcomeUnknown], now); err != nil {
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
	id := uuid.NewString()
	var out *Deployment
	var req *protocol.RemovalRequest
	err = t.withTenantTarget(ctx, a, permissions.ApplicationDestroy, appID.String()+"/removal/"+id, func(tx *sql.Tx) error {
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
		req = &protocol.RemovalRequest{Deployment: id, Endpoint: endpoint, Project: project, Deadline: now.Add(DeploymentApplyDeadline), Containers: []protocol.RemovalTarget{}}
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
		out = &Deployment{ID: id, ApplicationID: appID.String(), InstanceID: instanceID.String(), EndpointID: endpoint, EndpointName: endpointName, Kind: "remove", State: "applying", Revision: head, SpecDigest: digest, MappingVersion: mappingVersion, Plan: plan, CreatedBy: a.ActorID, CreatedAt: now, ExpiresAt: req.Deadline, AppliedBy: a.ActorID, AppliedAt: &now, Deadline: &req.Deadline}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,kind,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at,applied_by,applied_at,deadline) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), out.ID, a.OrganizationID, a.EnvironmentID, out.ApplicationID, out.InstanceID, out.EndpointID, project, out.Kind, out.State, out.Revision, out.SpecDigest, out.MappingVersion, string(raw), out.CreatedBy, now, out.ExpiresAt, out.AppliedBy, now, req.Deadline)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return out, req, nil
}
